package agybridge

import (
	"crypto/subtle"
	"io"
	"net/http"
	"strings"
	"time"

	json "github.com/goccy/go-json"
)

// Config 是 sidecar 的运行时配置。
type Config struct {
	Bin              string
	WorkDir          string
	Mode             string
	Effort           string
	Timeout          time.Duration
	Token            string
	Models           []string
	DefaultModel     string
	EmitThinking     bool
	EmitToolActivity bool
}

// Bridge 把 agy CLI 包装成 OpenAI / Anthropic 兼容的流式 HTTP 端点。
type Bridge struct {
	cfg    Config
	runner *Runner
}

func NewBridge(cfg Config) *Bridge {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	return &Bridge{
		cfg: cfg,
		runner: &Runner{
			Bin:     cfg.Bin,
			WorkDir: cfg.WorkDir,
			Mode:    cfg.Mode,
			Effort:  cfg.Effort,
			Timeout: cfg.Timeout,
		},
	}
}

func (b *Bridge) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.Handle("GET /v1/models", b.auth(http.HandlerFunc(b.handleModels)))
	mux.Handle("POST /v1/chat/completions", b.auth(http.HandlerFunc(b.handleOpenAI)))
	mux.Handle("POST /v1/messages", b.auth(http.HandlerFunc(b.handleAnthropic)))
	return mux
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

func writeAnthropicError(w http.ResponseWriter, status int, typ, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    typ,
			"message": message,
		},
	})
}
