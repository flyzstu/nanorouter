package agybridge

import (
	"context"
	"crypto/subtle"
	"io"
	"net/http"
	"strings"
	"time"

	json "github.com/goccy/go-json"
	"go.uber.org/zap"
)

// Config 是 sidecar 的运行时配置。
type Config struct {
	Profile      string
	Bin          string
	WorkDir      string
	Mode         string
	Effort       string
	Timeout      time.Duration
	Token        string
	Models       []string
	DefaultModel string
	EmitThinking bool
	ToolMode     string
	ToolBridge   string
	SessionFile  string
	SessionTTL   time.Duration
	Logger       *zap.Logger
}

// Bridge 把 agy CLI 包装成 OpenAI / Anthropic 兼容的流式 HTTP 端点。
type Bridge struct {
	cfg         Config
	runner      *Runner
	sessions    *SessionStore
	transparent bool
	logger      *zap.Logger
}

func NewBridge(cfg Config) *Bridge {
	profile := strings.TrimSpace(cfg.Profile)
	if profile != "agent" {
		profile = "model"
	}
	cfg.Profile = profile
	transparent := profile == "model"
	if transparent {
		// 透明转发模式：下游 agent 拥有工具循环，bridge 只当无状态模型。
		cfg.Mode = "plan"
		cfg.ToolMode = "none"
		cfg.SessionFile = ""
		if cfg.ToolBridge != "off" {
			cfg.ToolBridge = "prompt"
		}
	} else {
		cfg.ToolBridge = "off"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	if cfg.ToolMode == "" {
		cfg.ToolMode = "annotate"
	}
	var sessions *SessionStore
	if !transparent {
		sessions = NewSessionStore(cfg.SessionFile, cfg.SessionTTL)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Bridge{
		cfg:         cfg,
		transparent: transparent,
		logger:      logger,
		runner: &Runner{
			Bin:     cfg.Bin,
			WorkDir: cfg.WorkDir,
			Mode:    cfg.Mode,
			Effort:  cfg.Effort,
			Timeout: cfg.Timeout,
			Logger:  logger,
		},
		sessions: sessions,
	}
}

func (b *Bridge) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.Handle("GET /v1/models", b.requestLogger(b.auth(http.HandlerFunc(b.handleModels))))
	mux.Handle("POST /v1/chat/completions", b.requestLogger(b.auth(http.HandlerFunc(b.handleOpenAI))))
	mux.Handle("POST /v1/messages", b.requestLogger(b.auth(http.HandlerFunc(b.handleAnthropic))))
	return mux
}

type requestIDKey struct{}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (b *Bridge) logPrepared(r *http.Request, protocol, model string, stream bool, messages, tools int, promptTools bool) {
	fields := []zap.Field{
		zap.String("request_id", requestIDFrom(r.Context())),
		zap.String("protocol", protocol),
		zap.String("model", model),
		zap.Bool("stream", stream),
		zap.Int("messages", messages),
		zap.Int("tools", tools),
		zap.Bool("prompt_tools", promptTools),
		zap.Bool("session", sessionKeyFromRequest(r) != ""),
	}
	if r.ContentLength > 0 {
		fields = append(fields, zap.Int64("request_bytes", r.ContentLength))
	}
	b.logger.Info("request_prepared", fields...)
}

func (b *Bridge) logResult(r *http.Request, model, outcome string, usage Usage, toolCalls int) {
	b.logger.Info("request_result",
		zap.String("request_id", requestIDFrom(r.Context())),
		zap.String("model", model),
		zap.String("outcome", outcome),
		zap.Int("tool_calls", toolCalls),
		zap.Int64("prompt_tokens", usage.InputTokens),
		zap.Int64("completion_tokens", usage.OutputTokens),
		zap.Int64("reasoning_tokens", usage.ThinkingTokens),
		zap.Int64("total_tokens", usage.TotalTokens),
	)
}

func (b *Bridge) logRequestError(r *http.Request, err error) {
	info := classifyError(err)
	b.logger.Warn("request_error",
		zap.String("request_id", requestIDFrom(r.Context())),
		zap.String("error_kind", info.Kind),
		zap.String("error_code", info.Code),
		zap.String("error", firstLine(info.Message)),
	)
}

func (b *Bridge) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := "req_" + randomID()
		w.Header().Set("X-Request-ID", requestID)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, requestID))
		rw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(rw, r)
		b.logger.Info("request_finished",
			zap.String("request_id", requestID),
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", rw.statusCode()),
			zap.Int64("response_bytes", rw.bytes),
			zap.Duration("duration", time.Since(started)),
			zap.String("remote", r.RemoteAddr),
		)
	})
}

func (b *Bridge) auth(next http.Handler) http.Handler {
	if b.cfg.Token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if presented == "" {
			presented = strings.TrimSpace(r.Header.Get("X-Api-Key"))
		}
		if subtle.ConstantTimeCompare([]byte(presented), []byte(b.cfg.Token)) != 1 {
			if strings.HasSuffix(r.URL.Path, "/messages") {
				writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid bridge token")
			} else {
				writeAPIError(w, http.StatusUnauthorized, "invalid bridge token", "authentication_error")
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (b *Bridge) handleModels(w http.ResponseWriter, _ *http.Request) {
	data := make([]map[string]any, 0, len(b.modelList()))
	for _, model := range b.modelList() {
		data = append(data, map[string]any{
			"id":       model,
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": "agy",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (b *Bridge) modelList() []string {
	if len(b.cfg.Models) > 0 {
		return b.cfg.Models
	}
	if strings.TrimSpace(b.cfg.DefaultModel) != "" {
		return []string{b.cfg.DefaultModel}
	}
	return []string{"agy-default"}
}

func (b *Bridge) modelFor(requested string) string {
	if strings.TrimSpace(requested) != "" {
		return strings.TrimSpace(requested)
	}
	if strings.TrimSpace(b.cfg.DefaultModel) != "" {
		return strings.TrimSpace(b.cfg.DefaultModel)
	}
	return b.modelList()[0]
}

func readJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}

func sessionKeyFromRequest(r *http.Request) string {
	key := strings.TrimSpace(r.Header.Get("X-Session-Affinity"))
	if key == "" {
		key = strings.TrimSpace(r.Header.Get("X-Session-Id"))
	}
	return key
}

func (b *Bridge) runWithContinuity(ctx context.Context, r *http.Request, model, fullPrompt string, continuation func() (string, bool), onDelta func(string, string) error) (*streamCollector, error) {
	if b.transparent {
		return b.run(ctx, fullPrompt, model, "", onDelta)
	}
	sessionKey := sessionKeyFromRequest(r)
	prompt := fullPrompt
	conversationID := ""
	if sessionKey != "" {
		if id, ok := b.sessions.Get(sessionKey, model); ok {
			if next, ok := continuation(); ok {
				prompt = next
				conversationID = id
			}
		}
	}
	collector, err := b.run(ctx, prompt, model, conversationID, onDelta)
	if err != nil && conversationID != "" && collector.textString() == "" && isStaleConversation(err) {
		// agy 的 conversation 可能过期/被清理：退回完整 prompt 重开一轮。
		b.sessions.Delete(sessionKey, model)
		collector, err = b.run(ctx, fullPrompt, model, "", onDelta)
	}
	if err == nil && sessionKey != "" && collector.convID != "" {
		b.sessions.Put(sessionKey, model, collector.convID)
	}
	return collector, err
}

func writeAnthropicError(w http.ResponseWriter, status int, typ, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    typ,
			"message": message,
		},
	})
}
