package router

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	json "github.com/goccy/go-json"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

const maxRequestBody = 16 << 20
const maxUpstreamErrorBody = 8 << 10
const maxUpstreamErrorMessage = 512

type Router struct {
	cfg       Config
	db        *Store
	client    *http.Client
	states    map[string]*state
	limiters  map[string]chan struct{}
	mu        sync.Mutex
	next      map[string]int
	requests  *prometheus.CounterVec
	tokens    *prometheus.CounterVec
	failovers *prometheus.CounterVec
	registry  *prometheus.Registry
	logger    *zap.Logger
}

// state 是单个上游的熔断状态。冷却期内不再向它发送新请求，
// 让 429 配额或短暂的服务故障有机会恢复。
type state struct {
	until    time.Time
	failures int
}

func New(cfg Config, db *sql.DB) (*Router, error) {
	return NewWithLogger(cfg, db, zap.NewNop())
}

// NewWithLogger 让命令入口注入生产日志器，而单元测试保持安静。
func NewWithLogger(cfg Config, db *sql.DB, logger *zap.Logger) (*Router, error) {
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 90 * time.Second
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	r := &Router{cfg: cfg, db: &Store{db}, client: &http.Client{}, states: map[string]*state{}, limiters: map[string]chan struct{}{}, next: map[string]int{}, registry: prometheus.NewRegistry(), logger: logger}
	configuredLimits := make(map[string]int)
	for routeName, route := range cfg.Routes {
		for _, candidate := range route.Candidates {
			if candidate.MaxConcurrency < 0 || candidate.QueueTimeout < 0 || (candidate.MaxConcurrency == 0 && candidate.QueueTimeout > 0) {
				return nil, fmt.Errorf("route %q candidate %q has invalid concurrency limit", routeName, candidate.ID)
			}
			if limit, exists := configuredLimits[candidate.ID]; exists && limit != candidate.MaxConcurrency {
				return nil, fmt.Errorf("candidate %q has conflicting max_concurrency values", candidate.ID)
			}
			configuredLimits[candidate.ID] = candidate.MaxConcurrency
			if candidate.MaxConcurrency == 0 {
				continue
			}
			if _, exists := r.limiters[candidate.ID]; exists {
				continue
			}
			r.limiters[candidate.ID] = make(chan struct{}, candidate.MaxConcurrency)
		}
	}
	r.requests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "nanorouter_requests_total", Help: "Upstream attempts"}, []string{"route", "upstream", "endpoint", "status"})
	r.tokens = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "nanorouter_tokens_total", Help: "Reported tokens"}, []string{"route", "upstream", "type"})
	r.failovers = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "nanorouter_failovers_total", Help: "Retries sent to another upstream"}, []string{"route", "reason"})
	r.registry.MustRegister(r.requests, r.tokens, r.failovers)
	return r, nil
}
func (r *Router) Handler() http.Handler {
	mux := chi.NewRouter()
	mux.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("/metrics", promhttp.HandlerFor(r.registry, promhttp.HandlerOpts{}))
	mux.Get("/v1/models", r.models)
	mux.Post("/v1/chat/completions", r.proxy("chat_completions"))
	mux.Post("/v1/responses", r.proxy("responses"))
	return r.auth(mux)
}
func (r *Router) auth(next http.Handler) http.Handler {
	expected := os.Getenv(r.cfg.AuthEnv)
	if expected == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("Authorization") != "Bearer "+expected {
			http.Error(w, `{"error":{"message":"unauthorized"}}`, 401)
			return
		}
		next.ServeHTTP(w, q)
	})
}
func (r *Router) models(w http.ResponseWriter, q *http.Request) {
	modelIDs := make([]string, 0, len(r.cfg.Routes))
	for name, route := range r.cfg.Routes {
		if hasEnabledCandidate(route) {
			modelIDs = append(modelIDs, name)
		}
		for _, candidate := range route.Candidates {
			if candidate.ExposeAs != "" && candidate.IsEnabled() {
				modelIDs = append(modelIDs, candidate.ExposeAs)
			}
		}
	}
	sort.Strings(modelIDs)
	data := make([]map[string]any, 0, len(modelIDs))
	for _, id := range modelIDs {
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "nanorouter"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}
func (r *Router) proxy(endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, q *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, q.Body, maxRequestBody))
		if err != nil {
			http.Error(w, `{"error":{"message":"invalid request body"}}`, 400)
			return
		}
		var payload map[string]any
		if json.Unmarshal(body, &payload) != nil {
			http.Error(w, `{"error":{"message":"expected JSON body"}}`, 400)
			return
		}
		routeName, _ := payload["model"].(string)
		route, ok := r.routeForModel(routeName)
		if !ok {
			http.Error(w, `{"error":{"message":"unknown model route"}}`, 400)
			return
		}
		requestID := requestID(q)
		w.Header().Set("X-Request-ID", requestID)
		streamed, _ := payload["stream"].(bool)
		concurrencyLimited := false
		// 只修改 model 字段，其他 OpenAI/Responses 字段透明保留，
		// 因而 tools、图片、response_format 等新字段不需要网关逐一适配。
		for attempt := 0; attempt < len(route.Candidates); attempt++ {
			u, ok := r.pick(routeName, route, endpoint)
			if !ok {
				break
			}
			p := clone(payload)
			upstreamEndpoint := endpoint
			adaptedResponses := endpoint == "responses" && u.ResponsesViaChatCompletions
			if adaptedResponses {
				p, err = responsesToChatCompletions(payload, u.ChatMaxTokensField)
				if err != nil {
					writeAPIError(w, http.StatusBadRequest, err.Error(), "unsupported_response_feature")
					return
				}
				upstreamEndpoint = "chat_completions"
			}
			p, dropped := sanitizeRequest(p, u)
			if len(dropped) > 0 {
				r.logger.Debug("Unsupported upstream request fields removed", zap.String("request_id", requestID), zap.String("route", routeName), zap.String("upstream", u.ID), zap.Strings("fields", dropped))
			}
			p["model"] = u.Model
			encoded, _ := json.Marshal(p)
			queueStart := time.Now()
			release, acquired := r.acquire(q.Context(), u)
			if !acquired {
				concurrencyLimited = true
				queueLatency := int(time.Since(queueStart).Milliseconds())
				r.record(q.Context(), routeName, u, endpoint, http.StatusTooManyRequests, queueLatency, streamed, attempt, Usage{Source: "unavailable"}, "concurrency")
				r.failovers.WithLabelValues(routeName, "concurrency_limit").Inc()
				r.logger.Warn("Upstream concurrency limit reached; trying another candidate", failureLogFields(requestID, routeName, u, endpoint, attempt, queueLatency, streamed, http.StatusTooManyRequests, "concurrency_limit", true)...)
				continue
			}
			start := time.Now()
			ctx, cancel := context.WithTimeout(q.Context(), r.cfg.RequestTimeout)
			resp, err := r.do(ctx, u, upstreamEndpoint, encoded)
			latency := int(time.Since(start).Milliseconds())
			if err != nil {
				release()
				cancel()
				r.record(q.Context(), routeName, u, endpoint, 0, latency, streamed, attempt, Usage{Source: "unavailable"}, "network")
				r.fail(routeName, u, 30*time.Second)
				r.failovers.WithLabelValues(routeName, "network").Inc()
				fields := failureLogFields(requestID, routeName, u, endpoint, attempt, latency, streamed, 0, "network_error", true)
				fields = append(fields, zap.Error(err))
				r.logger.Warn("Upstream request failed; failing over", fields...)
				continue
			}
			if retryable(resp.StatusCode) {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				release()
				cancel()
				r.record(q.Context(), routeName, u, endpoint, resp.StatusCode, latency, streamed, attempt, Usage{Source: "unavailable"}, "upstream")
				r.fail(routeName, u, cooldown(resp))
				r.failovers.WithLabelValues(routeName, http.StatusText(resp.StatusCode)).Inc()
				r.logger.Warn("Upstream request failed; failing over", failureLogFields(requestID, routeName, u, endpoint, attempt, latency, streamed, resp.StatusCode, "upstream_unavailable", true)...)
				continue
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				upstreamError := r.copyErrorResponse(w, resp)
				release()
				cancel()
				r.record(q.Context(), routeName, u, endpoint, resp.StatusCode, latency, streamed, attempt, Usage{Source: "unavailable"}, "client")
				fields := failureLogFields(requestID, routeName, u, endpoint, attempt, latency, streamed, resp.StatusCode, "upstream_rejected", false)
				fields = append(fields, upstreamError.logFields(r.logger.Core().Enabled(zap.DebugLevel))...)
				r.logger.Warn("Upstream request rejected", fields...)
				return
			}
			r.success(u)
			w.Header().Set("X-Nanorouter-Route", routeName)
			w.Header().Set("X-Nanorouter-Upstream", u.ID)
			w.Header().Set("X-Nanorouter-Attempts", fmt.Sprint(attempt+1))
			if streamed {
				usage := Usage{Source: "unavailable"}
				if adaptedResponses {
					usage = r.streamChatAsResponses(w, resp, routeName)
				} else {
					usage = r.stream(w, resp)
				}
				release()
				cancel()
				duration := int(time.Since(start).Milliseconds())
				r.record(q.Context(), routeName, u, endpoint, resp.StatusCode, duration, true, attempt, usage, "")
				r.logger.Info("Upstream request completed", successLogFields(requestID, routeName, u, endpoint, attempt, latency, duration, true, resp.StatusCode, usage)...)
			} else {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if adaptedResponses {
					b, err = chatCompletionToResponse(b, routeName)
					if err != nil {
						release()
						cancel()
						writeAPIError(w, http.StatusBadGateway, "cannot adapt upstream chat response: "+err.Error(), "upstream_error")
						r.record(q.Context(), routeName, u, endpoint, resp.StatusCode, latency, false, attempt, Usage{Source: "unavailable"}, "upstream")
						return
					}
					w.Header().Set("Content-Type", "application/json")
				} else {
					r.copyHeaders(w, resp.Header)
				}
				w.WriteHeader(resp.StatusCode)
				_, _ = w.Write(b)
				release()
				cancel()
				usage := usageFromJSON(b)
				duration := int(time.Since(start).Milliseconds())
				r.record(q.Context(), routeName, u, endpoint, resp.StatusCode, duration, false, attempt, usage, "")
				r.logger.Info("Upstream request completed", successLogFields(requestID, routeName, u, endpoint, attempt, latency, duration, false, resp.StatusCode, usage)...)
			}
			return
		}
		if concurrencyLimited {
			writeAPIError(w, http.StatusTooManyRequests, "all eligible upstreams are at their concurrency limit", "upstream_concurrency_limit")
			return
		}
		http.Error(w, `{"error":{"message":"all configured upstreams are unavailable","type":"upstream_error"}}`, 503)
	}
}

// acquire 在向上游发请求前占用候选槽位。它不把本地拥塞标记为上游故障，
// 以免短暂排队导致错误冷却；槽位由调用方在完整响应（包括 SSE）结束后释放。
func (r *Router) acquire(ctx context.Context, u Candidate) (func(), bool) {
	limiter := r.limiters[u.ID]
	if limiter == nil {
		return func() {}, true
	}
	if u.QueueTimeout == 0 {
		select {
		case limiter <- struct{}{}:
			return func() { <-limiter }, true
		default:
			return nil, false
		}
	}
	queueCtx, cancel := context.WithTimeout(ctx, u.QueueTimeout)
	defer cancel()
	select {
	case limiter <- struct{}{}:
		return func() { <-limiter }, true
	case <-queueCtx.Done():
		return nil, false
	}
}

// routeForModel 优先解析顶层路由；候选的 expose_as 则创建单候选路由，
// 让下游可绕过加权/故障切换池，直接选择特定上游。
func (r *Router) routeForModel(name string) (Route, bool) {
	if route, ok := r.cfg.Routes[name]; ok {
		return route, true
	}
	for _, route := range r.cfg.Routes {
		for _, candidate := range route.Candidates {
			if candidate.ExposeAs == name {
				return Route{Candidates: []Candidate{candidate}}, true
			}
		}
	}
	return Route{}, false
}

// 日志字段仅包含运行元数据，禁止将 prompt、模型回复和 Authorization 写入日志。
func requestLogFields(requestID, route string, u Candidate, endpoint string, attempt, ttfb int, streamed bool, status int) []zap.Field {
	return []zap.Field{
		zap.String("request_id", requestID),
		zap.String("route", route),
		zap.String("upstream", u.ID),
		zap.String("upstream_model", u.Model),
		zap.String("endpoint", endpoint),
		zap.Int("attempt", attempt+1),
		zap.Int("status", status),
		zap.Int("ttfb_ms", ttfb),
		zap.Bool("stream", streamed),
	}
}
func failureLogFields(requestID, route string, u Candidate, endpoint string, attempt, ttfb int, streamed bool, status int, outcome string, retryable bool) []zap.Field {
	fields := requestLogFields(requestID, route, u, endpoint, attempt, ttfb, streamed, status)
	return append(fields, zap.String("outcome", outcome), zap.Bool("retryable", retryable))
}
func successLogFields(requestID, route string, u Candidate, endpoint string, attempt, ttfb, duration int, streamed bool, status int, usage Usage) []zap.Field {
	fields := requestLogFields(requestID, route, u, endpoint, attempt, ttfb, streamed, status)
	return append(fields,
		zap.String("outcome", "success"),
		zap.Int("duration_ms", duration),
		zap.Int64("prompt_tokens", usage.Prompt),
		zap.Int64("completion_tokens", usage.Completion),
		zap.Int64("total_tokens", usage.Total),
		zap.String("usage_source", usage.Source),
	)
}
func (r *Router) do(ctx context.Context, u Candidate, endpoint string, body []byte) (*http.Response, error) {
	base := strings.TrimRight(u.BaseURL, "/")
	// llama-server 的监听地址通常不带 /v1；保持其他 OpenAI 兼容上游的 base_url 语义不变。
	if u.LlamaServer && !strings.HasSuffix(strings.ToLower(base), "/v1") {
		base += "/v1"
	}
	path := strings.ReplaceAll(endpoint, "_", "/")
	if endpoint == "chat_completions" && u.ChatCompletionsPath != "" {
		path = strings.TrimLeft(u.ChatCompletionsPath, "/")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if key := os.Getenv(u.APIKeyEnv); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return r.client.Do(req)
}
func (r *Router) pick(name string, route Route, endpoint string) (Candidate, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 将权重展开为候选槽位，例如 weight=3 的上游在轮转序列中占三个槽位。
	// 同时过滤正在冷却、或未声明支持 Responses API 的候选。
	eligible := []Candidate{}
	for _, u := range route.Candidates {
		if !u.IsEnabled() {
			continue
		}
		if endpoint == "responses" && !u.Responses && !u.ResponsesViaChatCompletions {
			continue
		}
		st := r.states[u.ID]
		if st == nil || !time.Now().Before(st.until) {
			for i := 0; i < max(1, u.Weight); i++ {
				eligible = append(eligible, u)
			}
		}
	}
	if len(eligible) == 0 {
		return Candidate{}, false
	}
	u := eligible[r.next[name]%len(eligible)]
	r.next[name]++
	return u, true
}

func hasEnabledCandidate(route Route) bool {
	for _, candidate := range route.Candidates {
		if candidate.IsEnabled() {
			return true
		}
	}
	return false
}
func (r *Router) fail(_ string, u Candidate, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.states[u.ID]
	if s == nil {
		s = &state{}
		r.states[u.ID] = s
	}
	s.failures++
	s.until = time.Now().Add(d)
}
func (r *Router) success(u Candidate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.states[u.ID]; s != nil {
		s.failures = 0
		s.until = time.Time{}
	}
}
func (r *Router) record(ctx context.Context, route string, u Candidate, ep string, status, latency int, stream bool, attempt int, usage Usage, kind string) {
	r.db.Insert(ctx, Record{newID(), route, u.ID, u.Model, ep, kind, status, latency, attempt, stream, usage})
	r.requests.WithLabelValues(route, u.ID, ep, fmt.Sprint(status)).Inc()
	if usage.Source == "upstream" {
		r.tokens.WithLabelValues(route, u.ID, "prompt").Add(float64(usage.Prompt))
		r.tokens.WithLabelValues(route, u.ID, "completion").Add(float64(usage.Completion))
	}
}
func (r *Router) copyResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	r.copyHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
func (r *Router) copyErrorResponse(w http.ResponseWriter, resp *http.Response) upstreamError {
	defer resp.Body.Close()
	// 只缓存有限的错误前缀用于提取诊断字段，剩余响应继续透明转发给下游。
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorBody+1))
	r.copyHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	_, _ = io.Copy(w, resp.Body)
	return parseUpstreamError(body)
}
func (r *Router) copyHeaders(w http.ResponseWriter, h http.Header) {
	for k, v := range h {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, x := range v {
			w.Header().Add(k, x)
		}
	}
}
func (r *Router) stream(w http.ResponseWriter, resp *http.Response) Usage {
	defer resp.Body.Close()
	r.copyHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)
	f, _ := w.(http.Flusher)
	// 逐行转发 SSE，避免为了统计 usage 而把长输出完整缓存在内存中。
	reader := bufio.NewReader(resp.Body)
	usage := Usage{Source: "unavailable"}
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			_, _ = w.Write(line)
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				if u := usageFromJSON(bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))); u.Source == "upstream" {
					usage = u
				}
			}
			if f != nil {
				f.Flush()
			}
		}
		if err != nil {
			return usage
		}
	}
}
func retryable(s int) bool {
	return s == 408 || s == 425 || s == 429 || s == 500 || s == 502 || s == 503 || s == 504
}
func cooldown(resp *http.Response) time.Duration {
	if v, err := time.ParseDuration(resp.Header.Get("Retry-After") + "s"); err == nil && v > 0 {
		return v
	}
	return 30 * time.Second
}
func clone(a map[string]any) map[string]any {
	b := map[string]any{}
	for k, v := range a {
		b[k] = v
	}
	return b
}

type upstreamError struct {
	Type    string
	Code    string
	Message string
}

func parseUpstreamError(body []byte) upstreamError {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return upstreamError{}
	}
	data := payload
	if nested, ok := payload["error"].(map[string]any); ok {
		data = nested
	}
	result := upstreamError{Type: stringField(data, "type"), Message: stringField(data, "message")}
	if result.Type == "" {
		result.Type = stringField(payload, "type")
	}
	if result.Message == "" {
		result.Message = stringField(payload, "message")
	}
	if code, ok := data["code"]; ok {
		result.Code = fmt.Sprint(code)
	} else if code, ok := payload["error_code"]; ok {
		result.Code = fmt.Sprint(code)
	}
	return result
}

func stringField(data map[string]any, key string) string {
	v, _ := data[key].(string)
	return v
}

func (e upstreamError) logFields(includeMessage bool) []zap.Field {
	fields := []zap.Field{}
	if e.Type != "" {
		fields = append(fields, zap.String("upstream_error_type", e.Type))
	}
	if e.Code != "" {
		fields = append(fields, zap.String("upstream_error_code", e.Code))
	}
	// 上游消息可能回显请求内容；仅在显式 --debug 时提供，并限制长度。
	if includeMessage && e.Message != "" {
		fields = append(fields, zap.String("upstream_error_message", truncateLogMessage(e.Message)))
	}
	return fields
}

func truncateLogMessage(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	if len(message) <= maxUpstreamErrorMessage {
		return message
	}
	return message[:maxUpstreamErrorMessage] + "…"
}

func newID() string { b := make([]byte, 12); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func requestID(q *http.Request) string {
	if id := q.Header.Get("X-Request-ID"); id != "" && len(id) <= 128 {
		return id
	}
	return newID()
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
