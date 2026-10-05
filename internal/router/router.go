package router

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
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

// ledgerWriteTimeout 限制单次同步账本写入的时长。客户端取消请求时请求 context 会被取消，
// 账本写入必须使用独立 context，否则取消会导致记录丢失并刷出无谓的 ledger_write_failed。
const ledgerWriteTimeout = 5 * time.Second
const maxRateLimitRetries = 5
const sessionAffinityHeader = "X-Session-Affinity"
const upstreamMaxIdleConns = 256
const upstreamMaxIdleConnsPerHost = 64
const defaultOpenCodeUserAgent = "opencode/1.18.30"

type Router struct {
	cfg               Config
	db                *Store
	client            *http.Client
	userAgent         string
	states            map[string]*state
	limiters          map[string]*candidateLimiter
	admission         *admissionController
	retryBudgets      map[string]*retryBudget
	mu                sync.Mutex
	next              map[string]int
	requests          *prometheus.CounterVec
	tokens            *prometheus.CounterVec
	failovers         *prometheus.CounterVec
	ttfb              *prometheus.HistogramVec
	queueWait         *prometheus.HistogramVec
	retryWaits        *prometheus.HistogramVec
	inflight          *prometheus.GaugeVec
	concurrency       *prometheus.GaugeVec
	admissionRejected *prometheus.CounterVec
	retryBudgetDenied *prometheus.CounterVec
	retryBudgetTokens *prometheus.GaugeVec
	probes            *prometheus.CounterVec
	candidates        map[string]Candidate
	allCandidates     []Candidate
	proberCancel      context.CancelFunc
	proberDone        chan struct{}
	registry          *prometheus.Registry
	logger            *zap.Logger
	retryBackoff      func(int) time.Duration
	retryWait         func(context.Context, time.Duration) bool
	// zcodeLogin 是 ZCode 登录的后台任务快照，由 r.mu 保护；nil 表示从未触发。
	// 与 states（熔断）分开：后者按候选 id 存，这是一次性人工动作。
	zcodeLogin *zcodeLoginTask
	// zcodeCredentials 是凭据的数据库存储。登录完成后写库、后续请求读库，
	// 不再依赖只读的文件或 Secret 挂载。测试里可为 nil（退回文件源）。
	zcodeCredentials *zcodeCredentialStore
	// zcodeUsage 是 ZCode 用量查询的进程内缓存，懒初始化（见 zcode_usage.go），
	// 让测试可以直接构造 Router{}。
	zcodeUsage     *zcodeUsageCache
	zcodeUsageOnce sync.Once
}

// state 是单个上游的熔断状态。冷却期内不再向它发送新请求，
// 让 429 配额或短暂的服务故障有机会恢复。
type state struct {
	until         time.Time
	failures      int
	probeInflight bool
	lastProbeAt   time.Time
	lastProbeErr  string
}

func New(cfg Config, db *sql.DB) (*Router, error) {
	return NewWithLogger(cfg, db, zap.NewNop())
}

// NewWithLogger 让命令入口注入生产日志器，而单元测试保持安静。
func NewWithLogger(cfg Config, db *sql.DB, logger *zap.Logger) (*Router, error) {
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 90 * time.Second
	}
	if err := cfg.Retry.normalize(); err != nil {
		return nil, err
	}
	if err := cfg.Database.normalize(); err != nil {
		return nil, err
	}
	if err := cfg.Backpressure.normalize(); err != nil {
		return nil, err
	}
	if err := cfg.HealthCheck.normalize(); err != nil {
		return nil, err
	}
	for routeName, route := range cfg.Routes {
		if err := route.RetryBudget.normalize(routeName); err != nil {
			return nil, err
		}
		cfg.Routes[routeName] = route
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	r := &Router{cfg: cfg, db: NewStoreWithConfig(db, cfg.Database), client: newUpstreamHTTPClient(), userAgent: cfg.UserAgent, states: map[string]*state{}, limiters: map[string]*candidateLimiter{}, admission: newAdmissionController(cfg.Backpressure), retryBudgets: map[string]*retryBudget{}, next: map[string]int{}, registry: prometheus.NewRegistry(), logger: logger, retryWait: waitForRetry, candidates: map[string]Candidate{}, zcodeCredentials: newZcodeCredentialStore(db)}
	r.retryBackoff = func(retry int) time.Duration {
		return jitteredExponentialBackoff(retry, cfg.Retry.BaseDelay, cfg.Retry.MaxDelay)
	}
	configuredLimits := make(map[string]Candidate)
	for routeName, route := range cfg.Routes {
		if budget := newRetryBudget(route.RetryBudget); budget != nil {
			r.retryBudgets[routeName] = budget
			for _, candidate := range route.Candidates {
				if candidate.ExposeAs != "" {
					r.retryBudgets[candidate.ExposeAs] = newRetryBudget(route.RetryBudget)
				}
			}
		}
		for _, candidate := range route.Candidates {
			if err := candidate.validate(cfg.Retry); err != nil {
				return nil, fmt.Errorf("route %q candidate %q: %w", routeName, candidate.ID, err)
			}
			if configured, exists := configuredLimits[candidate.ID]; exists && (configured.MaxConcurrency != candidate.MaxConcurrency || configured.AdaptiveConcurrency != candidate.AdaptiveConcurrency || configured.MinConcurrency != candidate.MinConcurrency || configured.TargetTTFB != candidate.TargetTTFB) {
				return nil, fmt.Errorf("candidate %q has conflicting concurrency settings", candidate.ID)
			}
			configuredLimits[candidate.ID] = candidate
			r.candidates[candidate.ID] = candidate
			if candidate.MaxConcurrency == 0 {
				continue
			}
			if _, exists := r.limiters[candidate.ID]; exists {
				continue
			}
			r.limiters[candidate.ID] = newCandidateLimiter(candidate)
		}
	}
	r.allCandidates = make([]Candidate, 0, len(r.candidates))
	for _, c := range r.candidates {
		r.allCandidates = append(r.allCandidates, c)
	}
	r.probes = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "nanorouter_probes_total", Help: "Background health probe attempts"}, []string{"upstream", "outcome"})
	r.requests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "nanorouter_requests_total", Help: "Upstream attempts"}, []string{"route", "upstream", "endpoint", "status"})
	r.tokens = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "nanorouter_tokens_total", Help: "Reported tokens"}, []string{"route", "upstream", "type"})
	r.failovers = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "nanorouter_failovers_total", Help: "Retries sent to another upstream"}, []string{"route", "reason"})
	r.ttfb = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "nanorouter_upstream_ttfb_seconds", Help: "Time from upstream attempt start until response headers or transport error", Buckets: prometheus.DefBuckets}, []string{"route", "upstream", "endpoint"})
	r.queueWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "nanorouter_upstream_queue_seconds", Help: "Time waiting for a candidate concurrency slot", Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}}, []string{"route", "upstream", "endpoint", "outcome"})
	r.retryWaits = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "nanorouter_retry_delay_seconds", Help: "Actual delay scheduled before an internal retry", Buckets: []float64{0, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 30, 60}}, []string{"route", "endpoint", "reason"})
	r.inflight = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "nanorouter_upstream_inflight", Help: "Requests currently occupying an upstream concurrency slot"}, []string{"route", "upstream", "endpoint"})
	r.concurrency = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "nanorouter_upstream_concurrency_limit", Help: "Current fixed or adaptive concurrency limit"}, []string{"upstream"})
	r.admissionRejected = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "nanorouter_backpressure_rejected_total", Help: "Requests rejected by global admission control"}, []string{"reason"})
	r.retryBudgetDenied = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "nanorouter_retry_budget_denied_total", Help: "Additional upstream attempts denied by the route retry budget"}, []string{"route"})
	r.retryBudgetTokens = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "nanorouter_retry_budget_tokens", Help: "Currently available route retry tokens"}, []string{"route"})
	ledgerDropped := prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "nanorouter_ledger_dropped_total", Help: "Ledger records dropped because the async queue was full or closed"}, func() float64 { return float64(r.db.Dropped()) })
	ledgerErrors := prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "nanorouter_ledger_write_errors_total", Help: "Ledger records affected by database write failures"}, func() float64 { return float64(r.db.WriteErrors()) })
	for id, limiter := range r.limiters {
		r.concurrency.WithLabelValues(id).Set(float64(limiter.currentLimit()))
	}
	for routeName, budget := range r.retryBudgets {
		r.retryBudgetTokens.WithLabelValues(routeName).Set(budget.available())
	}
	collectors := []prometheus.Collector{r.requests, r.tokens, r.failovers, r.ttfb, r.queueWait, r.retryWaits, r.inflight, r.concurrency, r.admissionRejected, r.retryBudgetDenied, r.retryBudgetTokens, r.probes, ledgerDropped, ledgerErrors}
	if r.admission != nil {
		collectors = append(collectors,
			prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "nanorouter_backpressure_inflight", Help: "Requests admitted by global backpressure control"}, func() float64 { return float64(r.admission.inflight.Load()) }),
			prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "nanorouter_backpressure_queued", Help: "Requests waiting for global admission"}, func() float64 { return float64(r.admission.queued.Load()) }),
		)
	}
	r.registry.MustRegister(collectors...)
	if r.hasProbeCandidates() {
		ctx, cancel := context.WithCancel(context.Background())
		r.proberCancel = cancel
		r.proberDone = make(chan struct{})
		go r.runProber(ctx)
	}
	r.startZcodeCredentialCheck()
	return r, nil
}
func (r *Router) Handler() http.Handler {
	mux := chi.NewRouter()
	mux.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("/metrics", promhttp.HandlerFor(r.registry, promhttp.HandlerOpts{}))
	if r.cfg.Profiling.Enabled {
		mountPprof(mux)
	}
	mux.Get("/v1/models", r.models)
	mux.Post("/v1/chat/completions", r.proxy("chat_completions"))
	mux.Post("/v1/responses", r.proxy("responses"))
	// Anthropic Messages 形状的生成端点：服务 anthropic 候选（透传），
	// 也让 Anthropic 协议客户端（Claude Code、dsh 等）直接接入网关。
	mux.Post("/v1/messages", r.proxy("messages"))
	mux.Post("/messages", r.proxy("messages"))

	// 兼容端点路径：允许无 /v1 前缀调用
	mux.Get("/models", r.models)
	mux.Post("/chat/completions", r.proxy("chat_completions"))
	mux.Post("/responses", r.proxy("responses"))

	// 内置统计页与其数据端点：同一 auth 中间件，浏览器经 Basic 回退登录。
	mux.Get("/stats", r.statsPage)
	mux.Get("/v1/stats/summary", r.statsSummary)
	// ZCode Coding Plan 用量（glm-usage 内嵌）：数据面在 statsAuthPaths 里，
	// 浏览器同样走 Basic 回退。
	mux.Get("/v1/zcode/usage", r.zcodeUsageHandler)

	// zhipu-aicoding 专属端点路径：允许将客户端 Base URL 直接配置为 /zhipu-aicoding 或 /zhipuaicoding
	mountZhipu := func(sub chi.Router) {
		sub.Get("/models", r.models)
		sub.Get("/v1/models", r.models)
		sub.Post("/chat/completions", r.proxy("chat_completions", "zhipu-aicoding", "zhipuaicoding"))
		sub.Post("/v1/chat/completions", r.proxy("chat_completions", "zhipu-aicoding", "zhipuaicoding"))
		sub.Post("/responses", r.proxy("responses", "zhipu-aicoding", "zhipuaicoding"))
		sub.Post("/v1/responses", r.proxy("responses", "zhipu-aicoding", "zhipuaicoding"))
	}
	mux.Route("/zhipu-aicoding", mountZhipu)
	mux.Route("/zhipuaicoding", mountZhipu)

	// ZCode 登录：POST 发起并返回授权链接，GET 查进度。二者都在 auth
	// 之内（不加进 statsAuthPaths），故只有 Bearer 能过——这是能让网关
	// 以自身身份登录第三方账号的特权端点，不接受浏览器的 Basic 回退。
	mux.Post("/zcode/login", r.startZcodeLogin)
	mux.Get("/zcode/login", r.zcodeLoginStatus)
	return r.auth(mux)
}

func mountPprof(mux *chi.Mux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	for _, name := range []string{"allocs", "block", "goroutine", "heap", "mutex", "threadcreate"} {
		mux.Handle("/debug/pprof/"+name, pprof.Handler(name))
	}
}

func (r *Router) Close(ctx context.Context) error {
	if r.proberCancel != nil {
		r.proberCancel()
		select {
		case <-r.proberDone:
		case <-ctx.Done():
		}
	}
	return r.db.Close(ctx)
}
func (r *Router) auth(next http.Handler) http.Handler {
	expected := os.Getenv(r.cfg.AuthEnv)
	if expected == "" {
		return next
	}
	// 浏览器没有 Bearer 头的注入点；统计页与它的数据端点额外接受 Basic
	// 凭据（用户名或密码位放 API key 均可），401 带 WWW-Authenticate 让
	// 浏览器弹出原生登录框。生成端点不放宽，避免削弱网关认证面。
	statsAuthPaths := map[string]bool{"/stats": true, "/v1/stats/summary": true, "/v1/zcode/usage": true}
	// keyMatches 常量时间比较：逐字节短路的不等比较存在计时侧信道，
	// 网关又是长期暴露的服务，不值得省这个包。
	keyMatches := func(presented string) bool {
		return subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) == 1
	}
	return http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		// 两种传参并存：OpenAI 系客户端惯用 Bearer，Anthropic 系惯用
		// x-api-key——同一把 key 两种头都认，客户端不用为网关改认证方式。
		authorized := false
		if authHeader := q.Header.Get("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
			authorized = keyMatches(strings.TrimPrefix(authHeader, "Bearer "))
		}
		if !authorized {
			authorized = keyMatches(q.Header.Get("X-Api-Key"))
		}
		if !authorized {
			if statsAuthPaths[q.URL.Path] {
				// key 填在密码位或用户名位（浏览器登录框首格）都接受。
				if user, pass, ok := q.BasicAuth(); ok && (pass == expected || user == expected) {
					next.ServeHTTP(w, q)
					return
				}
				w.Header().Set("WWW-Authenticate", `Basic realm="nanorouter", charset="UTF-8"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			// 按请求路径选错误方言：Messages 端点的客户端期望 Anthropic 信封。
			if strings.HasSuffix(q.URL.Path, "/messages") {
				writeAnthropicAPIError(w, 401, "unauthorized", "authentication_error")
			} else {
				http.Error(w, `{"error":{"message":"unauthorized"}}`, 401)
			}
			return
		}
		next.ServeHTTP(w, q)
	})
}

// zcodeLoginStartTimeout 是 POST 等待授权链接的上限。链路要先打网关的
// init 端点才拿到链接，那一跳本身可能几百毫秒；给 15 秒是为了在网关慢
// 时不至于把一次成功的发起误报成失败，而 init 真失败时早该返回错误了。
const zcodeLoginStartTimeout = 15 * time.Second

// startZcodeLogin 发起一次设备码登录并返回授权链接。真正的轮询在后台
// 跑：浏览器里点确认可能过去几分钟，HTTP 请求不该吊在那里。
//
// 只发起不代劳：凭据由操作者自己在官方页面授权，网关只做发起与落盘。
func (r *Router) startZcodeLogin(w http.ResponseWriter, q *http.Request) {
	channel, ok := normalizeZcodeChannel(q.URL.Query().Get("channel"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"message": fmt.Sprintf("channel must be %q or %q", ZcodeChannelBigmodel, ZcodeChannelZai),
				"type":    "invalid_request_error",
			},
		})
		return
	}
	if !r.beginZcodeLogin(channel, time.Now()) {
		// 已有任务在跑。回当前快照而不是新起一条：两次设备码登录会互相
		// 覆盖凭据文件，且操作者只会拿到后一个链接。
		writeJSON(w, http.StatusConflict, r.zcodeLoginSnapshot())
		return
	}

	// 登录链需要 ctx：请求返回后 ctx 会被取消，故用进程生命周期的 context，
	// 让后台轮询活到它自己结束（设备码过期或成功）。
	ctx, cancel := context.WithCancel(context.Background())
	linkReady := make(chan struct{})
	var once sync.Once
	notify := func(phase zcodeLoginPhase, link string, expiresAt time.Time) {
		r.updateZcodeLogin(func(t *zcodeLoginTask) {
			t.Phase = phase
			if link != "" {
				t.AuthURL = link
				t.ExpiresAt = expiresAt
			}
		})
		once.Do(func() { close(linkReady) })
	}

	go func() {
		defer cancel()
		// 登录链的全部输出接进日志：授权链接、各阶段提示、落盘路径都在
		// 这里，操作者既看响应也看日志。
		// 凭据写库而非文件：容器内文件与 Secret 挂载卷都是只读的，程序没有
		// 写权限。TokenStorePath 留空，storeZcodeToken 走 Store 分支。
		storePath, err := RunZcodeLogin(ctx, ZcodeLoginOptions{
			GatewayOrigin: zcodeGatewayOrigin(),
			Channel:       channel,
			Output:        zapWriter{router: r},
			OnProgress:    notify,
			Store:         r.zcodeCredentials,
		})
		// 链接始终要放行等待者：init 失败时 RunZcodeLogin 会直接返回，
		// 没人调 notify，不放行就会白等到超时。
		once.Do(func() { close(linkReady) })
		if err != nil {
			r.finishZcodeLogin(zcodeLoginFailed, err, "")
			r.logger.Warn(r.logMessage("zcode_login_failed"), eventFields("zcode_login_failed", zap.String("channel", channel), zap.String("reason", err.Error()))...)
			return
		}
		r.finishZcodeLogin(zcodeLoginSucceeded, nil, storePath)
		r.logger.Info(r.logMessage("zcode_login_succeeded"), eventFields("zcode_login_succeeded", zap.String("channel", channel), zap.String("store_path", storePath))...)
	}()

	select {
	case <-linkReady:
	case <-time.After(zcodeLoginStartTimeout):
		// 拿不到链接就让调用方去看状态端点，而不是在这里干等。
	case <-q.Context().Done():
		// 客户端先走了：登录链继续跑（凭据照样能落盘），只是没人接链接。
		return
	}

	snapshot := r.zcodeLoginSnapshot()
	status := http.StatusAccepted
	if snapshot.Phase == zcodeLoginFailed {
		// 链路在拿到链接前就断了（init 不通、设备码不可读），这次请求
		// 确实失败了，用 502 如实反映。
		status = http.StatusBadGateway
	}
	writeJSON(w, status, snapshot)
}

// zcodeLoginStatus 返回登录进度。操作者点完授权后轮询这个端点，
// 直到 phase 变成 succeeded 或 failed。
func (r *Router) zcodeLoginStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, r.zcodeLoginSnapshot())
}

// zapWriter 把登录链的行式输出逐行写进日志，并剥掉尾部换行。
type zapWriter struct {
	router *Router
}

func (z zapWriter) Write(p []byte) (int, error) {
	if line := strings.TrimSpace(string(p)); line != "" {
		z.router.logger.Info(z.router.logMessage("zcode_login_output"), zap.String("line", line))
	}
	return len(p), nil
}

// writeJSON 写一个 JSON 响应。登录端点的响应体都是我们自己构造的
// 小对象，不值得为它引入渲染错误方言。
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (r *Router) models(w http.ResponseWriter, q *http.Request) {
	modelIDs := make([]string, 0, len(r.cfg.Routes))
	for name, route := range r.cfg.Routes {
		if hasEnabledAggregateCandidate(route) {
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
func (r *Router) proxy(endpoint string, defaultRoutes ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, q *http.Request) {
		startedAt := time.Now()
		requestID := requestID(q)
		w.Header().Set("X-Request-ID", requestID)
		ctx, cancelRequest := context.WithTimeout(q.Context(), r.cfg.RequestTimeout)
		defer cancelRequest()
		if r.admission != nil {
			releaseAdmission, outcome := r.admission.acquire(ctx)
			if outcome != admissionAcquired {
				if outcome == admissionCanceled && q.Context().Err() != nil {
					return
				}
				status := http.StatusTooManyRequests
				message := "gateway is at capacity"
				code := "gateway_overloaded"
				if outcome == admissionCanceled {
					status = http.StatusGatewayTimeout
					message = "request deadline exceeded while waiting for admission"
					code = "request_timeout"
				}
				if status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "1")
				}
				r.admissionRejected.WithLabelValues(string(outcome)).Inc()
				writeAPIErrorDialect(w, status, message, code, endpoint)
				r.logRequestFailed(requestID, endpoint, "", false, status, 0, time.Since(startedAt), string(outcome))
				return
			}
			defer releaseAdmission()
		}
		if r.logger.Core().Enabled(zap.InfoLevel) {
			r.logger.Info(r.logMessage("request_started"), eventFields("request_started", requestStartLogFields(q, requestID, endpoint)...)...)
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, q.Body, maxRequestBody))
		if err != nil {
			writeAPIErrorDialect(w, 400, "invalid request body", "invalid_request_body", endpoint)
			r.logRequestFailed(requestID, endpoint, "", false, http.StatusBadRequest, 0, time.Since(startedAt), "invalid_request_body")
			return
		}
		var payload map[string]any
		if json.Unmarshal(body, &payload) != nil {
			writeAPIErrorDialect(w, 400, "expected JSON body", "invalid_json", endpoint)
			r.logRequestFailed(requestID, endpoint, "", false, http.StatusBadRequest, 0, time.Since(startedAt), "invalid_json")
			return
		}
		meta := newRequestMeta(q, payload, r.cfg.TrustForwardedFor)
		routeName, _ := payload["model"].(string)
		route, ok := r.routeForModel(routeName)
		if !ok {
			for _, def := range defaultRoutes {
				if def == "" {
					continue
				}
				if defRoute, defOk := r.routeForModel(def); defOk {
					route = defRoute
					routeName = def
					ok = true
					break
				}
			}
		}
		if !ok {
			writeAPIErrorDialect(w, 400, "unknown model route", "unknown_model_route", endpoint)
			r.logRequestFailed(requestID, endpoint, routeName, false, http.StatusBadRequest, 0, time.Since(startedAt), "unknown_model_route")
			return
		}
		streamed, _ := payload["stream"].(bool)
		sessionAffinity := strings.TrimSpace(q.Header.Get(sessionAffinityHeader))
		if sessionAffinity == "" {
			sessionAffinity = strings.TrimSpace(q.Header.Get("X-Session-Id"))
		}
		budget := r.retryBudgets[routeName]
		if budget != nil {
			r.retryBudgetTokens.WithLabelValues(routeName).Set(budget.credit())
		}
		concurrencyLimited := false
		rateLimited := false
		retries := 0
		attempt := 0
		upstreamAttempts := 0
		attempted := make(map[string]bool, len(route.Candidates))
		var retryCounts map[string]int
		var pendingRetries map[string]pendingCandidateRetry
		// 只修改 model 字段，其他 OpenAI/Responses 字段透明保留，
		// 因而 tools、图片、response_format 等新字段不需要网关逐一适配。
		for {
			u, ok := r.pick(routeName, route, endpoint, attempted, sessionAffinity)
			if !ok {
				if pending, retryable := earliestPendingRetry(pendingRetries); retryable {
					delay := time.Until(pending.At)
					if delay < 0 {
						delay = 0
					}
					retries++
					if r.logger.Core().Enabled(zap.WarnLevel) {
						retryFields := append(requestOutcomeLogFields(requestID, routeName, endpoint, streamed, attempt, time.Since(startedAt)),
							zap.Int("retry", retries),
							zap.Int("candidate_retry", pending.Number),
							zap.Int("max_retries", pending.Policy.MaxRetries),
							zap.Int("retry_delay_ms", int(delay.Milliseconds())),
							zap.String("retry_reason", http.StatusText(pending.Status)),
							zap.String("upstream", pending.Candidate.ID),
						)
						r.logger.Warn(r.logMessage("retry_scheduled"), eventFields("retry_scheduled", retryFields...)...)
					}
					r.retryWaits.WithLabelValues(routeName, endpoint, fmt.Sprint(pending.Status)).Observe(delay.Seconds())
					if !r.retryWait(ctx, delay) {
						if q.Context().Err() != nil {
							r.logRequestFailed(requestID, endpoint, routeName, streamed, 499, attempt, time.Since(startedAt), "client_canceled")
							return
						}
						writeAPIErrorDialect(w, http.StatusGatewayTimeout, "request deadline exceeded while waiting to retry", "upstream_retry_timeout", endpoint)
						r.logRequestFailed(requestID, endpoint, routeName, streamed, http.StatusGatewayTimeout, attempt, time.Since(startedAt), "retry_timeout")
						return
					}
					delete(attempted, pending.Candidate.ID)
					delete(pendingRetries, pending.Candidate.ID)
					if retryCounts == nil {
						retryCounts = make(map[string]int, 1)
					}
					retryCounts[pending.Candidate.ID] = pending.Number
					continue
				}
				if rateLimited {
					writeAPIErrorDialect(w, http.StatusTooManyRequests, "upstreams remained rate limited after retries", "upstream_rate_limited", endpoint)
					r.logRequestFailed(requestID, endpoint, routeName, streamed, http.StatusTooManyRequests, attempt, time.Since(startedAt), "upstream_rate_limited")
					return
				}
				if concurrencyLimited {
					writeAPIErrorDialect(w, http.StatusTooManyRequests, "all eligible upstreams are at their concurrency limit", "upstream_concurrency_limit", endpoint)
					r.logRequestFailed(requestID, endpoint, routeName, streamed, http.StatusTooManyRequests, attempt, time.Since(startedAt), "upstream_concurrency_limit")
					return
				}
				writeAPIErrorDialect(w, http.StatusServiceUnavailable, "all configured upstreams are unavailable", "upstream_error", endpoint)
				r.logRequestFailed(requestID, endpoint, routeName, streamed, http.StatusServiceUnavailable, attempt, time.Since(startedAt), "upstreams_unavailable")
				return
			}
			// 同一请求中失败或拥塞的候选不再重复尝试，避免权重导致的原地重试。
			attempted[u.ID] = true
			p := clone(payload)
			upstreamEndpoint := endpoint
			adaptedResponses := false
			switch {
			case endpoint == "responses" && u.ResponsesViaChatCompletions:
				p, err = responsesToChatCompletions(payload, u.ChatMaxTokensField)
				if err != nil {
					r.releaseProbe(u)
					writeAPIErrorDialect(w, http.StatusBadRequest, err.Error(), "unsupported_response_feature", endpoint)
					r.logRequestFailed(requestID, endpoint, routeName, streamed, http.StatusBadRequest, attempt, time.Since(startedAt), "unsupported_response_feature")
					return
				}
				upstreamEndpoint = "chat_completions"
				adaptedResponses = true
			case endpoint == "responses" && u.Anthropic:
				// Responses → Chat → Messages 链式降级：请求侧两跳，
				// 响应侧按反向链路折返，复用两条各自已测的转换。
				p, err = responsesToChatCompletions(payload, "")
				if err != nil {
					r.releaseProbe(u)
					writeAPIErrorDialect(w, http.StatusBadRequest, err.Error(), "unsupported_response_feature", endpoint)
					r.logRequestFailed(requestID, endpoint, routeName, streamed, http.StatusBadRequest, attempt, time.Since(startedAt), "unsupported_response_feature")
					return
				}
				upstreamEndpoint = "chat_completions"
				adaptedResponses = true
			}
			p, dropped := sanitizeRequest(p, u)
			if len(dropped) > 0 && r.logger.Core().Enabled(zap.DebugLevel) {
				r.logger.Debug(r.logMessage("request_sanitized"), eventFields("request_sanitized", zap.String("request_id", requestID), zap.String("route", routeName), zap.String("upstream", u.ID), zap.Strings("removed_fields", dropped))...)
			}
			p, clamped := clampRequestMaxTokens(p, u)
			if len(clamped) > 0 && r.logger.Core().Enabled(zap.DebugLevel) {
				r.logger.Debug(r.logMessage("request_tokens_clamped"), eventFields("request_tokens_clamped", zap.String("request_id", requestID), zap.String("route", routeName), zap.String("upstream", u.ID), zap.Strings("fields", clamped), zap.Int("max_tokens_limit", u.MaxTokensLimit))...)
			}
			if u.Anthropic && upstreamEndpoint == "chat_completions" {
				p, err = chatCompletionsToMessages(p)
				if err != nil {
					r.releaseProbe(u)
					writeAPIErrorDialect(w, http.StatusBadRequest, err.Error(), "unsupported_openai_feature", endpoint)
					r.logRequestFailed(requestID, endpoint, routeName, streamed, http.StatusBadRequest, attempt, time.Since(startedAt), "unsupported_openai_feature")
					return
				}
				upstreamEndpoint = "messages"
			}
			// 解析一次凭据仅供日志脱敏：上游可能把 Authorization 回显进错误
			// 消息，key 不能随该字段进日志。真正的取用发生在 do() 内部。
			credential := r.resolveCandidateAPIKey(ctx, u)
			if u.SimulateZcode {
				// 官方 CLI 把设备身份写进请求体的 metadata.user_id。注入放在
				// 翻译之后：Responses→Chat 适配按白名单重建请求体，提前注入
				// 的 metadata 会被丢弃，此处才能覆盖原生透传、Chat→Messages
				// 与 Responses 链三条路径。
				if userID := r.zcodeMetadataUserID(ctx, zcodeSessionIDFor(p)); userID != "" {
					p["metadata"] = map[string]any{"user_id": userID}
				}
			}
			p["model"] = u.Model
			encoded, _ := json.Marshal(p)
			queueStart := time.Now()
			release, acquired := r.acquire(ctx, u)
			if !acquired {
				r.releaseProbe(u)
				concurrencyLimited = true
				queueLatency := int(time.Since(queueStart).Milliseconds())
				r.queueWait.WithLabelValues(routeName, u.ID, endpoint, "timeout").Observe(time.Since(queueStart).Seconds())
				r.record(requestID, routeName, u, endpoint, http.StatusTooManyRequests, queueLatency, streamed, attempt, Usage{Source: "unavailable"}, "concurrency", meta)
				r.failovers.WithLabelValues(routeName, "concurrency_limit").Inc()
				if r.logger.Core().Enabled(zap.WarnLevel) {
					r.logger.Warn(r.logMessage("upstream_attempt_finished"), eventFields("upstream_attempt_finished", append(failureLogFields(requestID, routeName, u, endpoint, attempt, queueLatency, streamed, http.StatusTooManyRequests, "concurrency_limit", true, credential), zap.String("attempt_phase", "queue"))...)...)
				}
				attempt++
				continue
			}
			queueDuration := time.Since(queueStart)
			r.queueWait.WithLabelValues(routeName, u.ID, endpoint, "acquired").Observe(queueDuration.Seconds())
			inflight := r.inflight.WithLabelValues(routeName, u.ID, endpoint)
			inflight.Inc()
			releaseSlot := release
			release = func() {
				releaseSlot()
				inflight.Dec()
			}
			if upstreamAttempts > 0 && budget != nil {
				allowed, tokens := budget.consume()
				r.retryBudgetTokens.WithLabelValues(routeName).Set(tokens)
				if !allowed {
					release()
					r.releaseProbe(u)
					r.retryBudgetDenied.WithLabelValues(routeName).Inc()
					writeAPIErrorDialect(w, http.StatusTooManyRequests, "route retry budget exhausted", "retry_budget_exhausted", endpoint)
					r.logRequestFailed(requestID, endpoint, routeName, streamed, http.StatusTooManyRequests, attempt, time.Since(startedAt), "retry_budget_exhausted")
					return
				}
			}
			start := time.Now()
			if r.logger.Core().Enabled(zap.InfoLevel) {
				r.logger.Info(r.logMessage("upstream_attempt_started"), eventFields("upstream_attempt_started", append(requestLogFields(requestID, routeName, u, endpoint, attempt, 0, streamed, 0), zap.Int("retry", retries))...)...)
			}
			attemptCtx, cancelAttempt := candidateAttemptContext(ctx, u.AttemptTimeout)
			upstreamAttempts++
			resp, err := r.do(attemptCtx, u, upstreamEndpoint, encoded, p, q)
			ttfbDuration := time.Since(start)
			latency := int(ttfbDuration.Milliseconds())
			r.ttfb.WithLabelValues(routeName, u.ID, endpoint).Observe(ttfbDuration.Seconds())
			if err != nil {
				attemptTimedOut := errors.Is(attemptCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
				cancelAttempt()
				release()
				status := 0
				kind := "network"
				outcome := "network_error"
				if attemptTimedOut {
					status = http.StatusGatewayTimeout
					kind = "attempt_timeout"
					outcome = "attempt_timeout"
				}
				r.observeConcurrency(u, status, ttfbDuration, queueDuration > time.Millisecond)
				// 客户端断开会取消请求 context；请求根本没发完，不能把它当成上游故障冷却。
				clientCanceled := q.Context().Err() != nil
				if clientCanceled {
					status, kind, outcome = 499, "client_canceled", "client_canceled"
				}
				r.record(requestID, routeName, u, endpoint, status, latency, streamed, attempt, Usage{Source: "unavailable"}, kind, meta)
				if clientCanceled {
					r.releaseProbe(u)
					if ctx.Err() != nil {
						r.logRequestFailed(requestID, endpoint, routeName, streamed, 499, attempt+1, time.Since(startedAt), "client_canceled")
						return
					}
					attempt++
					continue
				}
				r.fail(routeName, u, 30*time.Second)
				r.failovers.WithLabelValues(routeName, kind).Inc()
				if r.logger.Core().Enabled(zap.WarnLevel) {
					fields := failureLogFields(requestID, routeName, u, endpoint, attempt, latency, streamed, status, outcome, true, credential)
					fields = append(fields, zap.Error(err))
					r.logger.Warn(r.logMessage("upstream_attempt_finished"), eventFields("upstream_attempt_finished", fields...)...)
				}
				if ctx.Err() != nil {
					if q.Context().Err() != nil {
						r.logRequestFailed(requestID, endpoint, routeName, streamed, 499, attempt+1, time.Since(startedAt), "client_canceled")
						return
					}
					writeAPIErrorDialect(w, http.StatusGatewayTimeout, "request deadline exceeded", "request_timeout", endpoint)
					r.logRequestFailed(requestID, endpoint, routeName, streamed, http.StatusGatewayTimeout, attempt+1, time.Since(startedAt), "request_timeout")
					return
				}
				attempt++
				continue
			}
			r.observeConcurrency(u, resp.StatusCode, ttfbDuration, queueDuration > time.Millisecond)
			// 收到上游响应头后立即记录下游请求到上游返回的完整等待时间，
			// 包含请求解析、选路、排队及此前的回退耗时。
			if r.logger.Core().Enabled(zap.InfoLevel) {
				responseFields := requestLogFields(requestID, routeName, u, endpoint, attempt, latency, streamed, resp.StatusCode)
				responseFields = append(responseFields, zap.Int("request_to_upstream_response_ms", int(time.Since(startedAt).Milliseconds())))
				r.logger.Info(r.logMessage("upstream_response_received"), eventFields("upstream_response_received", responseFields...)...)
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				// expose_as 代表明确指定某个上游；此时不能用通用 503 掩盖
				// 上游诊断；只有明确配置允许的临时状态才原地重试该候选。
				// 普通路由仍先切换其他候选，避免重试拖慢健康上游的接管。
				directUpstream := routeName == u.ExposeAs && u.ExposeAs != ""
				policy, _ := u.Retry.resolve(r.cfg.Retry)
				candidateRetry := internalRetryEligible(resp.StatusCode) && retryCounts[u.ID] < policy.MaxRetries
				if (directUpstream && !candidateRetry) || !failoverEligible(resp.StatusCode) {
					if failoverEligible(resp.StatusCode) {
						r.fail(routeName, u, cooldown(resp))
					} else {
						r.success(u)
					}
					// 401/403 是凭据失效的信号，不是可切换的故障：冷却它没有意义，
					// 换到另一个候选也只是把同一个坏凭据再试一遍。此时改为触发
					// 一次登录，拿到新 key 后下一请求自然恢复。
					if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
						r.maybeTriggerZcodeRelogin(u, "upstream rejected the credential")
					}
					upstreamError := r.copyErrorResponse(w, resp, u, endpoint)
					cancelAttempt()
					release()
					r.record(requestID, routeName, u, endpoint, resp.StatusCode, latency, streamed, attempt, Usage{Source: "unavailable"}, "client", meta)
					if r.logger.Core().Enabled(zap.WarnLevel) {
						fields := failureLogFields(requestID, routeName, u, endpoint, attempt, latency, streamed, resp.StatusCode, "upstream_rejected", false, credential)
						fields = append(fields, upstreamError.logFields(true)...)
						r.logger.Warn(r.logMessage("upstream_attempt_finished"), eventFields("upstream_attempt_finished", fields...)...)
					}
					r.logRequestFailed(requestID, endpoint, routeName, streamed, resp.StatusCode, attempt+1, time.Since(startedAt), "upstream_rejected")
					return
				}

				upstreamError := readUpstreamError(resp)
				cancelAttempt()
				release()
				if resp.StatusCode == http.StatusTooManyRequests {
					rateLimited = true
				}
				if candidateRetry {
					number := retryCounts[u.ID] + 1
					delay := r.candidateRetryDelay(policy, number)
					delay = maxDuration(delay, retryAfter(resp))
					retryAt := time.Now().Add(delay)
					if pendingRetries == nil {
						pendingRetries = make(map[string]pendingCandidateRetry, 1)
					}
					pendingRetries[u.ID] = pendingCandidateRetry{Candidate: u, At: retryAt, Number: number, Status: resp.StatusCode, Policy: policy}
					r.failUntil(u, retryAt)
				} else {
					r.fail(routeName, u, cooldown(resp))
				}
				r.record(requestID, routeName, u, endpoint, resp.StatusCode, latency, streamed, attempt, Usage{Source: "unavailable"}, "upstream", meta)
				r.failovers.WithLabelValues(routeName, http.StatusText(resp.StatusCode)).Inc()
				if r.logger.Core().Enabled(zap.WarnLevel) {
					fields := failureLogFields(requestID, routeName, u, endpoint, attempt, latency, streamed, resp.StatusCode, "upstream_unavailable", true, credential)
					fields = append(fields, upstreamError.logFields(true)...)
					r.logger.Warn(r.logMessage("upstream_attempt_finished"), eventFields("upstream_attempt_finished", fields...)...)
				}
				attempt++
				continue
			}
			w.Header().Set("X-Nanorouter-Route", routeName)
			w.Header().Set("X-Nanorouter-Upstream", u.ID)
			w.Header().Set("X-Nanorouter-Attempts", fmt.Sprint(attempt+1))
			if streamed {
				usage := Usage{Source: "unavailable"}
				var streamErr error
				switch {
				case adaptedResponses && u.Anthropic:
					usage, streamErr = r.streamMessagesAsChat(w, resp, true, routeName)
				case u.Anthropic && endpoint == "chat_completions" && upstreamEndpoint == "messages":
					usage, streamErr = r.streamMessagesAsChat(w, resp, false, routeName)
				case adaptedResponses:
					usage, streamErr = r.streamChatAsResponses(w, resp, routeName)
				default:
					// messages 端点的同方言透传也在默认分支：SSE 原样转发，
					// usage 由 stream 内的合并逻辑采集。
					usage, streamErr = r.stream(w, resp)
				}
				// cancelAttempt 会置空 attemptCtx 的 deadline，必须先判定再取消。
				attemptTimedOut := errors.Is(attemptCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
				cancelAttempt()
				release()
				duration := int(time.Since(start).Milliseconds())
				if streamErr != nil {
					// 客户端取消或候选/全局超时都会中断对上游流的读取，但这些都不是
					// 上游故障；冷却它们会让同一路由陷入无谓的 503 窗口。
					// 只有真正的上游流错误才需要冷却并计入故障切换。
					clientCanceled := q.Context().Err() != nil
					kind, outcome, status := "stream", "stream_interrupted", 0
					switch {
					case clientCanceled:
						kind, outcome, status = "client_canceled", "client_canceled", 499
					case attemptTimedOut:
						kind, outcome, status = "attempt_timeout", "attempt_timeout", http.StatusGatewayTimeout
					case ctx.Err() != nil:
						kind, outcome, status = "request_timeout", "request_timeout", http.StatusGatewayTimeout
					default:
						r.fail(routeName, u, 30*time.Second)
					}
					r.releaseProbe(u)
					r.record(requestID, routeName, u, endpoint, status, duration, true, attempt, usage, kind, meta)
					r.logRequestFailed(requestID, endpoint, routeName, true, status, attempt+1, time.Since(startedAt), outcome)
					return
				}
				r.success(u)
				r.record(requestID, routeName, u, endpoint, resp.StatusCode, duration, true, attempt, usage, "", meta)
				if r.logger.Core().Enabled(zap.InfoLevel) {
					r.logger.Info(r.logMessage("upstream_attempt_finished"), eventFields("upstream_attempt_finished", successLogFields(requestID, routeName, u, endpoint, attempt, latency, duration, true, resp.StatusCode, usage)...)...)
				}
				r.logRequestCompleted(requestID, endpoint, routeName, u, true, attempt+1, time.Since(startedAt), usage)
			} else {
				b, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				cancelAttempt()
				if readErr != nil {
					release()
					r.fail(routeName, u, 30*time.Second)
					r.record(requestID, routeName, u, endpoint, http.StatusGatewayTimeout, int(time.Since(start).Milliseconds()), false, attempt, Usage{Source: "unavailable"}, "attempt_timeout", meta)
					attempt++
					continue
				}
				r.success(u)
				if adaptedResponses {
					if u.Anthropic {
						b, err = messagesToChatCompletion(b, routeName)
						if err != nil {
							release()
							writeAPIErrorDialect(w, http.StatusBadGateway, "cannot adapt upstream messages response: "+err.Error(), "upstream_error", endpoint)
							r.record(requestID, routeName, u, endpoint, resp.StatusCode, latency, false, attempt, Usage{Source: "unavailable"}, "upstream", meta)
							r.logRequestFailed(requestID, endpoint, routeName, false, http.StatusBadGateway, attempt+1, time.Since(startedAt), "response_adaptation_failed")
							return
						}
					}
					b, err = chatCompletionToResponse(b, routeName)
					if err != nil {
						release()
						writeAPIErrorDialect(w, http.StatusBadGateway, "cannot adapt upstream chat response: "+err.Error(), "upstream_error", endpoint)
						r.record(requestID, routeName, u, endpoint, resp.StatusCode, latency, false, attempt, Usage{Source: "unavailable"}, "upstream", meta)
						r.logRequestFailed(requestID, endpoint, routeName, false, http.StatusBadGateway, attempt+1, time.Since(startedAt), "response_adaptation_failed")
						return
					}
					w.Header().Set("Content-Type", "application/json")
				} else if u.Anthropic && endpoint == "chat_completions" {
					b, err = messagesToChatCompletion(b, routeName)
					if err != nil {
						release()
						writeAPIErrorDialect(w, http.StatusBadGateway, "cannot adapt upstream messages response: "+err.Error(), "upstream_error", endpoint)
						r.record(requestID, routeName, u, endpoint, resp.StatusCode, latency, false, attempt, Usage{Source: "unavailable"}, "upstream", meta)
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
				r.record(requestID, routeName, u, endpoint, resp.StatusCode, duration, false, attempt, usage, "", meta)
				if r.logger.Core().Enabled(zap.InfoLevel) {
					r.logger.Info(r.logMessage("upstream_attempt_finished"), eventFields("upstream_attempt_finished", successLogFields(requestID, routeName, u, endpoint, attempt, latency, duration, false, resp.StatusCode, usage)...)...)
				}
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
	return limiter.acquire(ctx, u.QueueTimeout)
}

func candidateAttemptContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(parent, timeout)
	}
	return parent, func() {}
}

func (r *Router) observeConcurrency(u Candidate, status int, ttfb time.Duration, queued bool) {
	if limiter := r.limiters[u.ID]; limiter != nil {
		limit := limiter.observe(status, ttfb, queued)
		r.concurrency.WithLabelValues(u.ID).Set(float64(limit))
	}
}

// routeForModel 优先解析顶层路由；候选的 expose_as 则创建单候选路由，
// 让下游可绕过加权/故障切换池，直接选择特定上游。
func (r *Router) routeForModel(name string) (Route, bool) {
	if route, ok := r.cfg.Routes[name]; ok {
		// 顶层模型只包含参与聚合的候选；expose_as 的单候选路由不受此开关影响。
		candidates := make([]Candidate, 0, len(route.Candidates))
		for _, candidate := range route.Candidates {
			if candidate.IsAggregateEnabled() {
				candidates = append(candidates, candidate)
			}
		}
		route.Candidates = candidates
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
	fields := []zap.Field{
		zap.String("request_id", requestID),
		zap.String("endpoint", endpoint),
		zap.String("http_method", q.Method),
		zap.String("http_path", q.URL.Path),
		zap.String("remote_addr", q.RemoteAddr),
		zap.Int64("request_content_length", q.ContentLength),
	}
	if clientIP := requestClientIP(q); clientIP != "" {
		fields = append(fields, zap.String("client_ip", clientIP))
	}
	if userAgent := strings.TrimSpace(q.UserAgent()); userAgent != "" {
		fields = append(fields, zap.String("user_agent", truncateLogMessage(userAgent)))
	}
	if strings.TrimSpace(q.Header.Get(sessionAffinityHeader)) != "" {
		// 会话键可能用于关联用户行为，只记录是否存在，不记录其实际内容。
		fields = append(fields, zap.Bool("session_affinity", true))
	}
	return fields
}

func requestClientIP(q *http.Request) string {
	if forwarded := q.Header.Get("X-Forwarded-For"); forwarded != "" {
		if clientIP := strings.TrimSpace(strings.Split(forwarded, ",")[0]); clientIP != "" {
			return truncateLogMessage(clientIP)
		}
	}
	if realIP := strings.TrimSpace(q.Header.Get("X-Real-IP")); realIP != "" {
		return truncateLogMessage(realIP)
	}
	host, _, err := net.SplitHostPort(q.RemoteAddr)
	if err == nil {
		return host
	}
	return truncateLogMessage(strings.TrimSpace(q.RemoteAddr))
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
	if !r.logger.Core().Enabled(zap.InfoLevel) {
		return
	}
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
	if !r.logger.Core().Enabled(zap.WarnLevel) {
		return
	}
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
	"request_tokens_clamped":     "请求 token 上限已调整",
	"upstream_attempt_started":   "开始调用上游",
	"upstream_response_received": "已收到上游响应",
	"upstream_attempt_finished":  "上游调用结束",
	"retry_scheduled":            "已安排上游重试",
	"request_completed":          "请求处理完成",
	"request_failed":             "请求处理失败",
	"ledger_write_failed":        "账本写入失败",
	"upstream_probe_recovered":   "上游健康探活成功并恢复",
	"upstream_probe_failed":      "上游健康探活失败",
}

func eventFields(event string, fields ...zap.Field) []zap.Field {
	return append([]zap.Field{zap.String("event", event)}, fields...)
}

func failureLogFields(requestID, route string, u Candidate, endpoint string, attempt, ttfb int, streamed bool, status int, outcome string, retryable bool, credential string) []zap.Field {
	fields := requestLogFields(requestID, route, u, endpoint, attempt, ttfb, streamed, status)
	fields = append(fields, zap.String("outcome", outcome), zap.Bool("retryable", retryable))
	// 纵深防御：上游把 Authorization/x-api-key 回显进错误消息时（被入侵
	// 或镜像上游可为之），key 不得随 upstream_error_message 进日志。
	if key := credential; key != "" {
		for i, field := range fields {
			if field.Key != "upstream_error_message" {
				continue
			}
			if value := field.String; value != "" && strings.Contains(value, key) {
				fields[i] = zap.String(field.Key, strings.ReplaceAll(value, key, "***"))
			}
		}
	}
	return fields
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
func (r *Router) do(ctx context.Context, u Candidate, endpoint string, body []byte, payload map[string]any, incomingReq *http.Request) (*http.Response, error) {
	base := strings.TrimRight(u.BaseURL, "/")
	// llama-server 的监听地址通常不带 /v1；保持其他 OpenAI 兼容上游的 base_url 语义不变。
	if u.LlamaServer && !strings.HasSuffix(strings.ToLower(base), "/v1") {
		base += "/v1"
	}
	path := strings.ReplaceAll(endpoint, "_", "/")
	if endpoint == "chat_completions" && u.ChatCompletionsPath != "" {
		path = strings.TrimLeft(u.ChatCompletionsPath, "/")
	}
	// Anthropic 候选到达此处时 endpoint 已统一为 "messages"（原生透传或
	// 翻译后），路径按 显式覆盖 > 渠道网关路径 > v1/messages 解析。
	if u.Anthropic && endpoint == "messages" {
		path = zcodeMessagesPath(u)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	ua := u.UserAgent
	if ua == "" && u.SimulateZcode {
		// 显式 user_agent 优先；未配置时复刻 ZCode 桌面客户端 UA。
		ua = "ZCode/" + ZcodeClientVersion
	} else if ua == "" && u.SimulateOpenCode {
		ua = defaultOpenCodeUserAgent
	} else if ua == "" {
		ua = r.userAgent
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if u.Anthropic {
		key := r.resolveCandidateAPIKey(ctx, u)
		if key != "" {
			// ZCode 网关要求 Bearer 与 x-api-key 并存；原生 Anthropic
			// 忽略 Authorization，两个都发与两类上游兼容。
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("x-api-key", key)
		}
		// anthropic-version 是 Messages 协议的必需头，对全部 anthropic
		// 候选注入；候选 Headers 配置可覆盖。
		req.Header.Set("anthropic-version", ZcodeAnthropicVersion)
		if u.SimulateZcode {
			messages, _ := payload["messages"].([]any)
			ids := deriveZcodeRequestIDs(messages)
			for name, value := range zcodeClientIdentityHeaders() {
				req.Header.Set(name, value)
			}
			// CLI 的归因 id：会话 id 从对话派生（同会话稳定），请求/
			// 追踪 id 每次全新；x-session-affinity 同步会话 id。
			req.Header.Set("x-request-id", ids.request)
			req.Header.Set("x-session-id", ids.session)
			req.Header.Set("x-session-affinity", ids.session)
			req.Header.Set("x-zcode-trace-id", ids.trace)
		}
	} else if key := resolveAPIKey(u.APIKeyEnv); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	// 模拟 OpenCode 客户端行为（会话头）；simulate_zcode 已在上方按
	// 对话派生并写入会话头，不进入这里的下游头透传，避免覆盖派生值。
	if u.SimulateZcode {
		// derived ids already set
	} else if u.SimulateOpenCode {
		sessionID := ""
		if incomingReq != nil {
			sessionID = strings.TrimSpace(incomingReq.Header.Get(sessionAffinityHeader))
			if sessionID == "" {
				sessionID = strings.TrimSpace(incomingReq.Header.Get("X-Session-Id"))
			}
		}
		if sessionID == "" {
			sessionID = generateSessionID()
		}
		req.Header.Set(sessionAffinityHeader, sessionID)
		req.Header.Set("X-Session-Id", sessionID)
		if incomingReq != nil {
			if pid := strings.TrimSpace(incomingReq.Header.Get("X-Parent-Session-Id")); pid != "" {
				req.Header.Set("X-Parent-Session-Id", pid)
			}
		}
	} else if incomingReq != nil {
		if aff := incomingReq.Header.Get(sessionAffinityHeader); aff != "" {
			req.Header.Set(sessionAffinityHeader, aff)
		}
		if sid := incomingReq.Header.Get("X-Session-Id"); sid != "" {
			req.Header.Set("X-Session-Id", sid)
		}
		if pid := strings.TrimSpace(incomingReq.Header.Get("X-Parent-Session-Id")); pid != "" {
			req.Header.Set("X-Parent-Session-Id", pid)
		}
	}

	// 注入候选自定义 Headers（显式配置可覆盖默认值）
	for k, v := range u.Headers {
		req.Header.Set(k, v)
	}

	return r.client.Do(req)
}

func resolveAPIKey(envName string) string {
	if envName == "" {
		return ""
	}
	if key := os.Getenv(envName); key != "" {
		return key
	}
	// 智谱 API Key 常见环境变量名互备容错
	if envName == "ZHIPU_API_KEY" {
		return os.Getenv("ZHIPUAI_API_KEY")
	}
	if envName == "ZHIPUAI_API_KEY" {
		return os.Getenv("ZHIPU_API_KEY")
	}
	return ""
}

func generateSessionID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("ses_%d", time.Now().UnixNano())
	}
	return "ses_" + hex.EncodeToString(b)
}

func newUpstreamHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// 网关的并发通常集中到少量上游；扩大空闲池可复用 TLS/HTTP2 连接，减少握手和端口抖动。
	transport.MaxIdleConns = upstreamMaxIdleConns
	transport.MaxIdleConnsPerHost = upstreamMaxIdleConnsPerHost
	return &http.Client{Transport: transport}
}

func (r *Router) pick(name string, route Route, endpoint string, excluded map[string]bool, sessionAffinity string) (Candidate, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 用累计权重定位槽位，避免为大 weight 展开重复候选切片。
	// 同时过滤正在冷却、或未声明支持 Responses API 的候选。
	totalWeight := 0
	now := time.Now()
	for _, u := range route.Candidates {
		if r.candidateEligible(u, endpoint, excluded, now) {
			totalWeight += max(1, u.Weight)
		}
	}
	if totalWeight == 0 {
		return Candidate{}, false
	}
	slot := 0
	if sessionAffinity != "" {
		// 下游提供会话键时使用稳定哈希，同一会话在候选集合不变时固定到同一上游。
		// 候选冷却或本次请求已尝试失败时，eligible 会变化，从而仍可故障切换。
		slot = affinitySlot(name, sessionAffinity, totalWeight)
	} else {
		slot = r.next[name] % totalWeight
		r.next[name]++
	}
	for _, u := range route.Candidates {
		if !r.candidateEligible(u, endpoint, excluded, now) {
			continue
		}
		weight := max(1, u.Weight)
		if slot < weight {
			r.reserveProbeLocked(u, now)
			return u, true
		}
		slot -= weight
	}
	return Candidate{}, false
}

func (r *Router) candidateEligible(u Candidate, endpoint string, excluded map[string]bool, now time.Time) bool {
	if !u.IsEnabled() || excluded[u.ID] {
		return false
	}
	if endpoint == "responses" && !u.Responses && !u.ResponsesViaChatCompletions && !u.Anthropic {
		return false
	}
	// Anthropic Messages 端点只接受 anthropic 候选：OpenAI 候选缺乏
	// 反向翻译层，静默转发必然产生协议损坏的请求。
	if endpoint == "messages" && !u.Anthropic {
		return false
	}
	st := r.states[u.ID]
	if st == nil {
		return true
	}
	if r.isProbeEnabled(u) {
		return st.failures == 0
	}
	return !now.Before(st.until)
}

func (r *Router) reserveProbeLocked(u Candidate, now time.Time) {
	if st := r.states[u.ID]; st != nil && st.failures > 0 && !now.Before(st.until) {
		lease := u.AttemptTimeout
		if lease <= 0 || lease > 15*time.Second {
			lease = 10 * time.Second
		}
		st.until = now.Add(lease)
	}
}

func (r *Router) releaseProbe(u Candidate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.states[u.ID]; st != nil && st.failures > 0 {
		st.until = time.Time{}
	}
}

func affinitySlot(routeName, sessionAffinity string, slots int) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(routeName))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(sessionAffinity))
	return int(h.Sum64() % uint64(slots))
}

func hasEnabledAggregateCandidate(route Route) bool {
	for _, candidate := range route.Candidates {
		if candidate.IsEnabled() && candidate.IsAggregateEnabled() {
			return true
		}
	}
	return false
}
func (r *Router) fail(_ string, u Candidate, d time.Duration) {
	r.failUntil(u, time.Now().Add(d))
}

// failUntil 记录候选冷却到 until。这个时间与请求内重试调度（pendingRetries）会同时维护：
// states[].until 供所有并发请求的 pick() 过滤候选；pendingRetries[].At 只服务当前请求的
// earliestPendingRetry 等待。两者的恢复时间由调用方通过 failUntil(u, retryAt) 保持同步。
func (r *Router) failUntil(u Candidate, until time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.states[u.ID]
	if s == nil {
		s = &state{}
		r.states[u.ID] = s
	}
	s.failures++
	s.until = until
}
func (r *Router) success(u Candidate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.states[u.ID]; s != nil {
		s.failures = 0
		s.until = time.Time{}
	}
}
func (r *Router) record(requestID, route string, u Candidate, ep string, status, latency int, stream bool, attempt int, usage Usage, kind string, meta requestMeta) {
	// 账本写入失败（队列满、已关闭或数据库错误）不阻塞请求本身，
	// 但必须留下带 request_id 的日志，否则只能靠聚合指标定位丢失。
	// 写入使用独立的有界 context，不受请求上下文取消（例如客户端断开）影响。
	ctx, cancel := context.WithTimeout(context.Background(), ledgerWriteTimeout)
	defer cancel()
	if err := r.db.Insert(ctx, Record{
		ID: newID(), Route: route, UpstreamID: u.ID, UpstreamModel: u.Model,
		Endpoint: ep, ErrorKind: kind, Status: status, LatencyMS: latency,
		Attempt: attempt, Streamed: stream, Usage: usage,
		ToolsCount: meta.Tools, ToolNames: meta.ToolNames, ClientIP: meta.ClientIP, ClientAgent: meta.ClientAgent,
	}); err != nil && r.logger.Core().Enabled(zap.WarnLevel) {
		r.logger.Warn(r.logMessage("ledger_write_failed"), eventFields("ledger_write_failed",
			zap.String("request_id", requestID),
			zap.String("route", route),
			zap.String("upstream", u.ID),
			zap.String("endpoint", ep),
			zap.Int("status", status),
			zap.Int("attempt", attempt+1),
			zap.String("error_kind", kind),
			zap.Error(err),
		)...)
	}
	r.requests.WithLabelValues(route, u.ID, ep, fmt.Sprint(status)).Inc()
	if usage.Source == "upstream" {
		r.tokens.WithLabelValues(route, u.ID, "prompt").Add(float64(usage.Prompt))
		r.tokens.WithLabelValues(route, u.ID, "completion").Add(float64(usage.Completion))
	}
}

// readUpstreamErrorPrefix 读取并解析上游错误体的有限前缀，供直接回传（copyErrorResponse）
// 或切换候选前诊断（readUpstreamError）复用。
func readUpstreamErrorPrefix(resp *http.Response) ([]byte, upstreamError) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorBody+1))
	return body, parseUpstreamError(body)
}

func (r *Router) copyErrorResponse(w http.ResponseWriter, resp *http.Response, u Candidate, endpoint string) upstreamError {
	defer resp.Body.Close()
	// 只缓存有限的错误前缀用于提取诊断字段，剩余响应继续透明转发给下游。
	body, ue := readUpstreamErrorPrefix(resp)
	// Anthropic 错误信封发给 OpenAI 形状下游前改写；messages 端点保持
	// 同方言透传，上游诊断原样到达。
	if u.Anthropic && endpoint != "messages" {
		body = anthropicErrorToChat(body)
	}
	r.copyHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	_, _ = io.Copy(w, resp.Body)
	return ue
}

func readUpstreamError(resp *http.Response) upstreamError {
	defer resp.Body.Close()
	// 切换候选前只读取有限错误前缀：足以提取诊断信息，又不会缓存大响应。
	_, ue := readUpstreamErrorPrefix(resp)
	return ue
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
func (r *Router) stream(w http.ResponseWriter, resp *http.Response) (Usage, error) {
	defer resp.Body.Close()
	r.copyHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)
	f, _ := w.(http.Flusher)
	// 逐行转发 SSE，避免为了统计 usage 而把长输出完整缓存在内存中。
	reader := acquireSSEReader(resp.Body)
	defer releaseSSEReader(reader)
	usage := Usage{Source: "unavailable"}
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			_, _ = w.Write(line)
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				if u := usageFromJSON(bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))); u.Source == "upstream" {
					// Anthropic 的 usage 分段到达（message_start 带输入侧、
					// message_delta 带累计输出侧）；合并而非覆盖，OpenAI 的
					// 单帧 usage 语义不受影响。
					usage = mergeUsage(usage, u)
				}
			}
			if f != nil {
				f.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return usage, nil
			}
			return usage, err
		}
	}
}

// mergeUsage 合并分段到达的 usage：各字段取已见最大值（Anthropic 的
// message_delta 输出侧是累计值），总数按两侧之和重算。
func mergeUsage(dst, src Usage) Usage {
	if src.Prompt > dst.Prompt {
		dst.Prompt = src.Prompt
	}
	if src.Completion > dst.Completion {
		dst.Completion = src.Completion
	}
	dst.Total = dst.Prompt + dst.Completion
	dst.Source = "upstream"
	return dst
}

func failoverEligible(s int) bool {
	return s == 401 || s == 403 || s == 404 || s == 408 || s == 425 || s == 429 || s == 500 || s == 502 || s == 503 || s == 504
}

func internalRetryEligible(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

type pendingCandidateRetry struct {
	Candidate Candidate
	At        time.Time
	Number    int
	Status    int
	Policy    candidateRetryPolicy
}

func earliestPendingRetry(pending map[string]pendingCandidateRetry) (pendingCandidateRetry, bool) {
	var earliest pendingCandidateRetry
	found := false
	for _, retry := range pending {
		if !found || retry.At.Before(earliest.At) {
			earliest = retry
			found = true
		}
	}
	return earliest, found
}

func (r *Router) candidateRetryDelay(policy candidateRetryPolicy, retry int) time.Duration {
	if !policy.CustomWait {
		return r.retryBackoff(retry)
	}
	return jitteredExponentialBackoff(retry, policy.BaseDelay, policy.MaxDelay)
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

// boundedExponentialBackoff 计算第 retry 次的指数退避延迟（上限 maximum）。
// 默认 1s→16s 的调度由全局 RetryConfig 的 BaseDelay/MaxDelay 提供，无需固定参数别名。
func boundedExponentialBackoff(retry int, base, maximum time.Duration) time.Duration {
	delay := base
	for step := 1; step < retry && delay < maximum; step++ {
		if delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func jitteredExponentialBackoff(retry int, base, maximum time.Duration) time.Duration {
	upper := boundedExponentialBackoff(retry, base, maximum)
	lower := upper / 2
	span := upper - lower
	if span <= 0 {
		return upper
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return upper
	}
	return lower + time.Duration(binary.LittleEndian.Uint64(random[:])%uint64(span+1))
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
