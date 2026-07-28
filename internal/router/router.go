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
	"strconv"
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
const maxRateLimitRetries = 5

type Router struct {
	cfg          Config
	db           *Store
	client       *http.Client
	states       map[string]*state
	limiters     map[string]chan struct{}
	mu           sync.Mutex
	next         map[string]int
	requests     *prometheus.CounterVec
	tokens       *prometheus.CounterVec
	failovers    *prometheus.CounterVec
	registry     *prometheus.Registry
	logger       *zap.Logger
	retryBackoff func(int) time.Duration
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
	r := &Router{cfg: cfg, db: NewStore(db, cfg.Database.Driver), client: &http.Client{}, states: map[string]*state{}, limiters: map[string]chan struct{}{}, next: map[string]int{}, registry: prometheus.NewRegistry(), logger: logger, retryBackoff: exponentialBackoff}
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
	created := time.Now().Unix()
	for _, id := range modelIDs {
		data = append(data, map[string]any{"id": id, "object": "model", "created": created, "owned_by": "nanorouter"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}
func (r *Router) proxy(endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, q *http.Request) {
		startedAt := time.Now()
		requestID := requestID(q)
		w.Header().Set("X-Request-ID", requestID)
		r.logger.Info(r.logMessage("request_started"), eventFields("request_started", requestStartLogFields(q, requestID, endpoint)...)...)

		body, err := io.ReadAll(http.MaxBytesReader(w, q.Body, maxRequestBody))
		if err != nil {
			http.Error(w, `{"error":{"message":"invalid request body"}}`, 400)
			r.logRequestFailed(requestID, endpoint, "", false, http.StatusBadRequest, 0, time.Since(startedAt), "invalid_request_body")
			return
		}
		var payload map[string]any
		if json.Unmarshal(body, &payload) != nil {
			http.Error(w, `{"error":{"message":"expected JSON body"}}`, 400)
			r.logRequestFailed(requestID, endpoint, "", false, http.StatusBadRequest, 0, time.Since(startedAt), "invalid_json")
			return
		}
		routeName, _ := payload["model"].(string)
		route, ok := r.routeForModel(routeName)
		if !ok {
			http.Error(w, `{"error":{"message":"unknown model route"}}`, 400)
			r.logRequestFailed(requestID, endpoint, routeName, false, http.StatusBadRequest, 0, time.Since(startedAt), "unknown_model_route")
			return
		}
		streamed, _ := payload["stream"].(bool)
		ctx, cancelRequest := context.WithTimeout(q.Context(), r.cfg.RequestTimeout)
		defer cancelRequest()
		concurrencyLimited := false
		rateLimited := false
		retryDelay := time.Duration(0)
		retries := 0
		attempt := 0
		attempted := make(map[string]bool, len(route.Candidates))
		// 只修改 model 字段，其他 OpenAI/Responses 字段透明保留，
		// 因而 tools、图片、response_format 等新字段不需要网关逐一适配。
		for {
			u, ok := r.pick(routeName, route, endpoint, attempted)
			if !ok {
				if rateLimited && retries < maxRateLimitRetries {
					retries++
					delay := retryDelay
					retryFields := append(requestOutcomeLogFields(requestID, routeName, endpoint, streamed, attempt, time.Since(startedAt)),
						zap.Int("retry", retries),
						zap.Int("max_retries", maxRateLimitRetries),
						zap.Int("retry_delay_ms", int(delay.Milliseconds())),
						zap.String("retry_reason", "upstream_rate_limit"),
					)
					r.logger.Warn(r.logMessage("retry_scheduled"), eventFields("retry_scheduled", retryFields...)...)
					if !waitForRetry(ctx, delay) {
						if q.Context().Err() != nil {
							r.logRequestFailed(requestID, endpoint, routeName, streamed, 499, attempt, time.Since(startedAt), "client_canceled")
							return
						}
						writeAPIError(w, http.StatusGatewayTimeout, "request deadline exceeded while waiting to retry", "upstream_retry_timeout")
						r.logRequestFailed(requestID, endpoint, routeName, streamed, http.StatusGatewayTimeout, attempt, time.Since(startedAt), "retry_timeout")
						return
					}
					attempted = make(map[string]bool, len(route.Candidates))
					retryDelay = 0
					continue
				}
				if rateLimited {
					writeAPIError(w, http.StatusTooManyRequests, "upstreams remained rate limited after retries", "upstream_rate_limited")
					r.logRequestFailed(requestID, endpoint, routeName, streamed, http.StatusTooManyRequests, attempt, time.Since(startedAt), "upstream_rate_limited")
					return
				}
				if concurrencyLimited {
					writeAPIError(w, http.StatusTooManyRequests, "all eligible upstreams are at their concurrency limit", "upstream_concurrency_limit")
					r.logRequestFailed(requestID, endpoint, routeName, streamed, http.StatusTooManyRequests, attempt, time.Since(startedAt), "upstream_concurrency_limit")
					return
				}
				http.Error(w, `{"error":{"message":"all configured upstreams are unavailable","type":"upstream_error"}}`, http.StatusServiceUnavailable)
				r.logRequestFailed(requestID, endpoint, routeName, streamed, http.StatusServiceUnavailable, attempt, time.Since(startedAt), "upstreams_unavailable")
				return
			}
			// 同一请求中失败或拥塞的候选不再重复尝试，避免权重导致的原地重试。
			attempted[u.ID] = true
			p := clone(payload)
			upstreamEndpoint := endpoint
			adaptedResponses := endpoint == "responses" && u.ResponsesViaChatCompletions
			if adaptedResponses {
				p, err = responsesToChatCompletions(payload, u.ChatMaxTokensField)
				if err != nil {
					writeAPIError(w, http.StatusBadRequest, err.Error(), "unsupported_response_feature")
					r.logRequestFailed(requestID, endpoint, routeName, streamed, http.StatusBadRequest, attempt, time.Since(startedAt), "unsupported_response_feature")
					return
				}
				upstreamEndpoint = "chat_completions"
			}
			p, dropped := sanitizeRequest(p, u)
			if len(dropped) > 0 {
				r.logger.Debug(r.logMessage("request_sanitized"), eventFields("request_sanitized", zap.String("request_id", requestID), zap.String("route", routeName), zap.String("upstream", u.ID), zap.Strings("removed_fields", dropped))...)
			}
			p["model"] = u.Model
			encoded, _ := json.Marshal(p)
			queueStart := time.Now()
			release, acquired := r.acquire(ctx, u)
			if !acquired {
				concurrencyLimited = true
				queueLatency := int(time.Since(queueStart).Milliseconds())
				r.record(q.Context(), routeName, u, endpoint, http.StatusTooManyRequests, queueLatency, streamed, attempt, Usage{Source: "unavailable"}, "concurrency")
				r.failovers.WithLabelValues(routeName, "concurrency_limit").Inc()
				r.logger.Warn(r.logMessage("upstream_attempt_finished"), eventFields("upstream_attempt_finished", append(failureLogFields(requestID, routeName, u, endpoint, attempt, queueLatency, streamed, http.StatusTooManyRequests, "concurrency_limit", true), zap.String("attempt_phase", "queue"))...)...)
				attempt++
				continue
			}
			start := time.Now()
			r.logger.Info(r.logMessage("upstream_attempt_started"), eventFields("upstream_attempt_started", append(requestLogFields(requestID, routeName, u, endpoint, attempt, 0, streamed, 0), zap.Int("retry", retries))...)...)
			resp, err := r.do(ctx, u, upstreamEndpoint, encoded)
			latency := int(time.Since(start).Milliseconds())
			if err != nil {
				release()
				r.record(q.Context(), routeName, u, endpoint, 0, latency, streamed, attempt, Usage{Source: "unavailable"}, "network")
				r.fail(routeName, u, 30*time.Second)
				r.failovers.WithLabelValues(routeName, "network").Inc()
				fields := failureLogFields(requestID, routeName, u, endpoint, attempt, latency, streamed, 0, "network_error", true)
				fields = append(fields, zap.Error(err))
				r.logger.Warn(r.logMessage("upstream_attempt_finished"), eventFields("upstream_attempt_finished", fields...)...)
				attempt++
				continue
			}
			// 收到上游响应头后立即记录下游请求到上游返回的完整等待时间，
			// 包含请求解析、选路、排队及此前的回退耗时。
			responseFields := requestLogFields(requestID, routeName, u, endpoint, attempt, latency, streamed, resp.StatusCode)
			responseFields = append(responseFields, zap.Int("request_to_upstream_response_ms", int(time.Since(startedAt).Milliseconds())))
			r.logger.Info(r.logMessage("upstream_response_received"), eventFields("upstream_response_received", responseFields...)...)
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				// expose_as 代表明确指定某个上游；此时不能用通用 503 掩盖
				// 上游诊断，须原样交给下游。普通路由只对候选认证、模型失效或
				// 临时服务错误切换，避免 400/422 参数错误误触发模型轮转。
				directUpstream := routeName == u.ExposeAs && u.ExposeAs != ""
				if directUpstream || !failoverEligible(resp.StatusCode) {
					upstreamError := r.copyErrorResponse(w, resp)
					release()
					r.record(q.Context(), routeName, u, endpoint, resp.StatusCode, latency, streamed, attempt, Usage{Source: "unavailable"}, "client")
					fields := failureLogFields(requestID, routeName, u, endpoint, attempt, latency, streamed, resp.StatusCode, "upstream_rejected", false)
					fields = append(fields, upstreamError.logFields(true)...)
					r.logger.Warn(r.logMessage("upstream_attempt_finished"), eventFields("upstream_attempt_finished", fields...)...)
					r.logRequestFailed(requestID, endpoint, routeName, streamed, resp.StatusCode, attempt+1, time.Since(startedAt), "upstream_rejected")
					return
				}

				upstreamError := readUpstreamError(resp)
				release()
				if resp.StatusCode == http.StatusTooManyRequests {
					rateLimited = true
					backoff := r.retryBackoff(retries + 1)
					cooldown := maxDuration(backoff, retryAfter(resp))
					retryDelay = maxDuration(retryDelay, cooldown)
					r.fail(routeName, u, cooldown)
				} else {
					r.fail(routeName, u, cooldown(resp))
				}
				r.record(q.Context(), routeName, u, endpoint, resp.StatusCode, latency, streamed, attempt, Usage{Source: "unavailable"}, "upstream")
				r.failovers.WithLabelValues(routeName, http.StatusText(resp.StatusCode)).Inc()
				fields := failureLogFields(requestID, routeName, u, endpoint, attempt, latency, streamed, resp.StatusCode, "upstream_unavailable", true)
				fields = append(fields, upstreamError.logFields(true)...)
				r.logger.Warn(r.logMessage("upstream_attempt_finished"), eventFields("upstream_attempt_finished", fields...)...)
				attempt++
				continue
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
				duration := int(time.Since(start).Milliseconds())
				r.record(q.Context(), routeName, u, endpoint, resp.StatusCode, duration, true, attempt, usage, "")
				r.logger.Info(r.logMessage("upstream_attempt_finished"), eventFields("upstream_attempt_finished", successLogFields(requestID, routeName, u, endpoint, attempt, latency, duration, true, resp.StatusCode, usage)...)...)
				r.logRequestCompleted(requestID, endpoint, routeName, u, true, attempt+1, time.Since(startedAt), usage)
			} else {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if adaptedResponses {
					b, err = chatCompletionToResponse(b, routeName)
					if err != nil {
						release()
						writeAPIError(w, http.StatusBadGateway, "cannot adapt upstream chat response: "+err.Error(), "upstream_error")
						r.record(q.Context(), routeName, u, endpoint, resp.StatusCode, latency, false, attempt, Usage{Source: "unavailable"}, "upstream")
						r.logRequestFailed(requestID, endpoint, routeName, false, http.StatusBadGateway, attempt+1, time.Since(startedAt), "response_adaptation_failed")
						return
					}
					w.Header().Set("Content-Type", "application/json")
				} else {
					r.copyHeaders(w, resp.Header)
				}
				w.WriteHeader(resp.StatusCode)
				_, _ = w.Write(b)
				release()
				usage := usageFromJSON(b)
				duration := int(time.Since(start).Milliseconds())
				r.record(q.Context(), routeName, u, endpoint, resp.StatusCode, duration, false, attempt, usage, "")
				r.logger.Info(r.logMessage("upstream_attempt_finished"), eventFields("upstream_attempt_finished", successLogFields(requestID, routeName, u, endpoint, attempt, latency, duration, false, resp.StatusCode, usage)...)...)
				r.logRequestCompleted(requestID, endpoint, routeName, u, false, attempt+1, time.Since(startedAt), usage)
			}
			return
		}
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

// requestStartLogFields 仅记录可安全公开的下游元数据；请求正文与认证信息绝不写入日志。
func requestStartLogFields(q *http.Request, requestID, endpoint string) []zap.Field {
	return []zap.Field{
		zap.String("request_id", requestID),
		zap.String("endpoint", endpoint),
		zap.String("http_method", q.Method),
		zap.String("http_path", q.URL.Path),
		zap.String("remote_addr", q.RemoteAddr),
		zap.Int64("request_content_length", q.ContentLength),
		zap.Any("request_headers", sanitizeLogHeaders(q.Header)),
	}
}

func requestOutcomeLogFields(requestID, route, endpoint string, streamed bool, attempts int, duration time.Duration) []zap.Field {
	return []zap.Field{
		zap.String("request_id", requestID),
		zap.String("route", route),
		zap.String("endpoint", endpoint),
		zap.Bool("stream", streamed),
		zap.Int("attempts", attempts),
		zap.Int("duration_ms", int(duration.Milliseconds())),
	}
}

func (r *Router) logRequestCompleted(requestID, endpoint, route string, u Candidate, streamed bool, attempts int, duration time.Duration, usage Usage) {
	fields := requestOutcomeLogFields(requestID, route, endpoint, streamed, attempts, duration)
	fields = append(fields,
		zap.String("outcome", "success"),
		zap.String("upstream", u.ID),
		zap.String("upstream_model", u.Model),
		zap.Int64("prompt_tokens", usage.Prompt),
		zap.Int64("completion_tokens", usage.Completion),
		zap.Int64("total_tokens", usage.Total),
		zap.String("usage_source", usage.Source),
	)
	r.logger.Info(r.logMessage("request_completed"), eventFields("request_completed", fields...)...)
}

func (r *Router) logRequestFailed(requestID, endpoint, route string, streamed bool, status, attempts int, duration time.Duration, outcome string) {
	fields := requestOutcomeLogFields(requestID, route, endpoint, streamed, attempts, duration)
	fields = append(fields, zap.Int("status", status), zap.String("outcome", outcome))
	r.logger.Warn(r.logMessage("request_failed"), eventFields("request_failed", fields...)...)
}

func (r *Router) logMessage(event string) string {
	if r.cfg.Logging.Language != "zh-CN" {
		return event
	}
	if message := chineseLogMessages[event]; message != "" {
		return message
	}
	return event
}

var chineseLogMessages = map[string]string{
	"request_started":            "收到下游请求",
	"request_sanitized":          "请求字段已清洗",
	"upstream_attempt_started":   "开始调用上游",
	"upstream_response_received": "已收到上游响应",
	"upstream_attempt_finished":  "上游调用结束",
	"retry_scheduled":            "已安排上游重试",
	"request_completed":          "请求处理完成",
	"request_failed":             "请求处理失败",
}

func eventFields(event string, fields ...zap.Field) []zap.Field {
	return append([]zap.Field{zap.String("event", event)}, fields...)
}

func sanitizeLogHeaders(headers http.Header) map[string][]string {
	result := make(map[string][]string, len(headers))
	for name, values := range headers {
		if isSensitiveHeader(name) {
			continue
		}
		result[name] = make([]string, 0, len(values))
		for _, value := range values {
			result[name] = append(result[name], truncateLogMessage(value))
		}
	}
	return result
}

func isSensitiveHeader(name string) bool {
	name = strings.ToLower(name)
	if name == "authorization" || name == "proxy-authorization" || name == "cookie" || name == "set-cookie" {
		return true
	}
	for _, marker := range []string{"key", "token", "secret", "password"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
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
func (r *Router) pick(name string, route Route, endpoint string, excluded map[string]bool) (Candidate, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 将权重展开为候选槽位，例如 weight=3 的上游在轮转序列中占三个槽位。
	// 同时过滤正在冷却、或未声明支持 Responses API 的候选。
	eligible := []Candidate{}
	for _, u := range route.Candidates {
		if !u.IsEnabled() || excluded[u.ID] {
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

func readUpstreamError(resp *http.Response) upstreamError {
	defer resp.Body.Close()
	// 切换候选前只读取有限错误前缀：足以提取诊断信息，又不会缓存大响应。
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorBody+1))
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
func failoverEligible(s int) bool {
	return s == 401 || s == 403 || s == 404 || s == 408 || s == 425 || s == 429 || s == 500 || s == 502 || s == 503 || s == 504
}
func cooldown(resp *http.Response) time.Duration {
	if delay := retryAfter(resp); delay > 0 {
		return delay
	}
	return 30 * time.Second
}

func retryAfter(resp *http.Response) time.Duration {
	value := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if delay := time.Until(at); delay > 0 {
			return delay
		}
	}
	return 0
}

func exponentialBackoff(retry int) time.Duration {
	if retry <= 1 {
		return time.Second
	}
	return time.Second * time.Duration(1<<min(retry-1, 30))
}

func waitForRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
func clone(a map[string]any) map[string]any {
	b := map[string]any{}
	for k, v := range a {
		b[k] = v
	}
	return b
}

type upstreamError struct {
	Type         string
	Code         string
	Message      string
	ErrorType    string
	Provider     string
	ProviderCode string
	Param        string
	Detail       string
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
	// OpenRouter 会把供应商原始错误放进 error.metadata；只提取诊断字段，
	// 避免 flagged_input、prompt、content 等可能含用户输入的数据进入日志。
	if metadata, ok := data["metadata"].(map[string]any); ok {
		result.ErrorType = stringField(metadata, "error_type")
		result.Provider = stringField(metadata, "provider_name")
		result.ProviderCode = scalarField(metadata, "provider_code")
		result.Param = scalarField(metadata, "param")
		if raw, ok := metadata["raw"].(string); ok {
			if nested := parseUpstreamError([]byte(raw)); nested.Message != "" {
				result.Detail = nested.Message
				if result.ErrorType == "" {
					result.ErrorType = nested.Type
				}
				if result.ProviderCode == "" {
					result.ProviderCode = nested.Code
				}
				if result.Param == "" {
					result.Param = nested.Param
				}
			}
		}
	}
	return result
}

func stringField(data map[string]any, key string) string {
	v, _ := data[key].(string)
	return v
}

func scalarField(data map[string]any, key string) string {
	switch value := data[key].(type) {
	case string:
		return value
	case float64, float32, int, int64, int32, uint, uint64, uint32, bool:
		return fmt.Sprint(value)
	default:
		return ""
	}
}

func (e upstreamError) logFields(includeMessage bool) []zap.Field {
	fields := []zap.Field{}
	if e.Type != "" {
		fields = append(fields, zap.String("upstream_error_type", e.Type))
	} else if e.ErrorType != "" {
		fields = append(fields, zap.String("upstream_error_type", e.ErrorType))
	}
	if e.Code != "" {
		fields = append(fields, zap.String("upstream_error_code", e.Code))
	}
	if e.Type != "" && e.ErrorType != "" && e.ErrorType != e.Type {
		fields = append(fields, zap.String("upstream_error_category", e.ErrorType))
	}
	if e.Provider != "" {
		fields = append(fields, zap.String("upstream_provider", e.Provider))
	}
	if e.ProviderCode != "" {
		fields = append(fields, zap.String("upstream_provider_error_code", e.ProviderCode))
	}
	if e.Param != "" {
		fields = append(fields, zap.String("upstream_error_param", e.Param))
	}
	// 上游错误是故障切换的关键诊断信息；仍限制长度并压缩空白，避免日志膨胀。
	if includeMessage && e.Message != "" {
		fields = append(fields, zap.String("upstream_error_message", truncateLogMessage(e.Message)))
	}
	if includeMessage && e.Detail != "" && e.Detail != e.Message {
		fields = append(fields, zap.String("upstream_error_detail", truncateLogMessage(e.Detail)))
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
