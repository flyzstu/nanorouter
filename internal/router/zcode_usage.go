// ZCode Coding Plan 用量查询：把 glm-usage 看板的页面统计功能内嵌进网关。
//
// 数据来自智谱 bigmodel.cn 的用量监控端点（不是模型网关），凭据复用
// resolveCandidateAPIKey 同一条链（数据库 → 凭据文件），不引入新配置。
// 每个端点独立缓存、失败隔离：一块挂了只清自己的数据并进 failures，
// 其余块照常渲染——与 glm-usage 的容忍读取器同构。
//
// 上游合同的两个坑（来自 glm-usage 的实测，见其 client.py）：
//   - 鉴权失败以 HTTP 200 + {"code":401|1001,...} 信封返回，只看状态码
//     会把拒绝渲染成全零卡片；
//   - 时间窗是 Asia/Shanghai 的 "YYYY-MM-DD HH:mm:ss" 文本，其他格式被拒。

package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	zcodeUsageBaseURL = "https://bigmodel.cn"
	// 单端点超时。这是页面辅助数据，等待上游超过 8 秒不如尽快返回已缓存
	// 的旧值——页面还有网关自身的统计可看。
	zcodeUsageTimeout = 8 * time.Second
	// 失败后旧值还能服役的窗口。设短：宁可块报错，也别把过期的配额数字
	// 当成现状。
	zcodeUsageStaleTTL = 10 * time.Minute
	// 单飞条目的存活上限：超时后允许新请求重新进上游，避免一次卡死的
	// 上游调用把所有后续请求吊在同一个已僵死的 call 上。
	zcodeUsageSingleflightTTL = 10 * time.Second
	// 无旧值可降级时的失败退避：统计页的 10 秒自动刷新不该在冷失败期间
	// 以每 10 秒 4 个请求的节奏持续敲打故障中的上游。
	zcodeUsageFailureRetry = 30 * time.Second
	// 上游响应体上限：四个端点都是聚合 JSON，8 MiB 绰绰有余，防病态响应。
	zcodeUsageMaxBodyBytes = 8 << 20
)

// zcodeUsageEndpoint 是一个上游端点：路径与对应的缓存 TTL。windowed 标记
// 是否携带时间窗参数——quota 是即时快照，glm-usage 调它时不带任何参数。
type zcodeUsageEndpoint struct {
	key      string
	path     string
	cacheTTL time.Duration
	windowed bool
}

var zcodeUsageEndpoints = []zcodeUsageEndpoint{
	{key: "quota", path: "/api/monitor/usage/quota/limit", cacheTTL: 60 * time.Second},
	{key: "usage", path: "/api/monitor/credit-usage/usage-detail", cacheTTL: 5 * time.Minute, windowed: true},
	{key: "activity", path: "/api/monitor/credit-usage/activity", cacheTTL: 5 * time.Minute, windowed: true},
	{key: "performance", path: "/api/monitor/usage/model-performance-day", cacheTTL: 5 * time.Minute, windowed: true},
}

// zcodeUsageEntry 是一个端点的缓存条目。err 非空表示上次拉取失败：
// 数据仍在 staleTTL 内可降级返回（glm-usage 的 expired-but-readable 语义），
// 但同时要触发后台刷新，不能让旧值一直服役到天荒地老。
type zcodeUsageEntry struct {
	data      json.RawMessage
	fetchedAt time.Time
	err       error
}

type zcodeUsageCall struct {
	done    chan struct{}
	started time.Time
	data    json.RawMessage
	err     error
}

// zcodeUsageCache 按端点+时间窗缓存上游结果，单飞去重并发拉取：页面一次
// 打开就是四个请求，多人同时开页面也不该放大上游流量。
//
// fetch 的四种结局：新鲜缓存直接回；旧值降级回 + 后台刷新；无缓存时由
// 单飞 owner 阻塞进上游（并发后来者共享结果）；owner 取消/超时不落缓存，
// 下一次请求重新进上游。
type zcodeUsageCache struct {
	mu       sync.Mutex
	entries  map[string]zcodeUsageEntry
	inflight map[string]*zcodeUsageCall
	client   *http.Client
	now      func() time.Time
	// baseURL 默认 zcodeUsageBaseURL；测试指向 httptest 服务器。
	baseURL string
}

func newZcodeUsageCache() *zcodeUsageCache {
	return &zcodeUsageCache{
		entries:  map[string]zcodeUsageEntry{},
		inflight: map[string]*zcodeUsageCall{},
		client:   newUpstreamHTTPClient(),
		now:      time.Now,
		baseURL:  zcodeUsageBaseURL,
	}
}

// fetch 拉一个端点。返回 (数据, 失败原因)；数据非空且 err 非空 = 降级中的旧值。
func (c *zcodeUsageCache) fetch(ctx context.Context, ep zcodeUsageEndpoint, window zcodeUsageWindow, token string) (json.RawMessage, error) {
	cacheKey := ep.key + "|" + window.cacheKey()
	serve, call, owner := c.claim(cacheKey, ep)

	if !owner {
		if call != nil {
			select {
			case <-call.done:
				return call.data, call.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return serve.data, serve.err
	}

	// owner 带着可降级旧值（过 fresh TTL 或上次失败，但仍在 stale 窗内）：
	// 先把旧值返回给本次调用者，刷新放后台，页面不必为一次刷新干等。
	if len(serve.data) > 0 {
		result := serve.err
		if result == nil {
			// 数据已过新鲜期但没有失败记录：本次刷新结果未知，如实标注降级。
			result = errors.New("zcode usage 已过缓存新鲜期，刷新未完成")
		}
		go func() {
			refreshCtx, cancel := context.WithTimeout(context.Background(), zcodeUsageTimeout)
			defer cancel()
			data, err := c.upstream(refreshCtx, ep, window, token)
			c.complete(cacheKey, call, data, err)
			c.store(cacheKey, data, err)
		}()
		return serve.data, result
	}

	data, err := c.upstream(ctx, ep, window, token)
	c.complete(cacheKey, call, data, err)
	// 请求被下游取消或自身超时不落缓存：这是本次调用的状态，不是端点的。
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	c.store(cacheKey, data, err)
	return data, err
}

// claim 三选一：缓存命中 → (serve, nil, false)；共享既有单飞 →
// (零值, call, false)；否则登记本次单飞 → (可能带降级旧值, call, true)。
// 降级窗口 = 过 fresh TTL（或上次失败）但数据仍在 stale TTL 内，且上次
// 失败不是凭据被拒（401/1001 不降级：凭据失效必须暴露给操作者）。
func (c *zcodeUsageCache) claim(cacheKey string, ep zcodeUsageEndpoint) (serve zcodeUsageEntry, call *zcodeUsageCall, owner bool) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[cacheKey]
	if ok && entry.err == nil && now.Sub(entry.fetchedAt) < ep.cacheTTL {
		return entry, nil, false
	}
	// 降级窗口：有过好数据、已过新鲜期但仍在 stale 窗内，且失败不是凭据
	// 被拒（401/1001 不降级：凭据失效必须暴露给操作者）。旧值立即返回，
	// 刷新放后台；已有刷新在跑时直接回旧值而不加入等待——glm-usage 的
	// stale-while-revalidate 语义是旧值永远立即可用，刷新结果服务下一轮。
	if ok && len(entry.data) > 0 && now.Sub(entry.fetchedAt) >= ep.cacheTTL &&
		now.Sub(entry.fetchedAt) < zcodeUsageStaleTTL && !isZcodeUsageAuthError(entry.err) {
		if inflight, busy := c.inflight[cacheKey]; busy && now.Sub(inflight.started) < zcodeUsageSingleflightTTL {
			return entry, nil, false
		}
		refresh := &zcodeUsageCall{done: make(chan struct{}), started: now}
		c.inflight[cacheKey] = refresh
		return entry, refresh, true
	}
	if inflight, busy := c.inflight[cacheKey]; busy && now.Sub(inflight.started) < zcodeUsageSingleflightTTL {
		return zcodeUsageEntry{}, inflight, false
	}
	// 冷失败节流：上次失败且无旧值可降级时，重试窗内直接回失败、不进上游
	// ——错误照常返回给每次调用方（前端能看到），只是不拿 10 秒自动刷新
	// 去轰炸故障中的上游。
	if ok && entry.err != nil && len(entry.data) == 0 && now.Sub(entry.fetchedAt) < zcodeUsageFailureRetry {
		return entry, nil, false
	}
	fresh := &zcodeUsageCall{done: make(chan struct{}), started: now}
	c.inflight[cacheKey] = fresh
	return zcodeUsageEntry{}, fresh, true
}

// complete 唤醒等单飞的请求。等价结果之后按需落缓存（store）。
func (c *zcodeUsageCache) complete(cacheKey string, call *zcodeUsageCall, data json.RawMessage, err error) {
	call.data, call.err = data, err
	close(call.done)
	c.mu.Lock()
	defer c.mu.Unlock()
	// 只有仍是自己登记的条目才删：极端情况下条目已被下一轮 owner 覆盖。
	if current, ok := c.inflight[cacheKey]; ok && current == call {
		delete(c.inflight, cacheKey)
	}
}

func (c *zcodeUsageCache) store(cacheKey string, data json.RawMessage, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 失败不覆盖最后一份好数据：glm-usage 的语义是旧值在整个 stale 窗内
	// 一直可读，直到某次成功刷新才被替换。仅当没有旧值时才落失败条目。
	if err != nil {
		if entry, ok := c.entries[cacheKey]; ok && len(entry.data) > 0 {
			c.entries[cacheKey] = zcodeUsageEntry{data: entry.data, fetchedAt: entry.fetchedAt, err: err}
			return
		}
	}
	c.entries[cacheKey] = zcodeUsageEntry{data: data, fetchedAt: c.now(), err: err}
}

// upstream 发起真正的上游请求。状态码优先于信封解析：4xx/5xx 是传输层
// 结论，信封 code 只是补充。
func (c *zcodeUsageCache) upstream(ctx context.Context, ep zcodeUsageEndpoint, window zcodeUsageWindow, token string) (json.RawMessage, error) {
	values := url.Values{}
	if ep.windowed {
		for key, value := range window.params() {
			values.Set(key, value)
		}
	}
	target := c.baseURL + ep.path
	if len(values) > 0 {
		target += "?" + values.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("zcode usage request: %w", err)
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zcode usage 上游不可达: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("zcode usage 上游 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, zcodeUsageMaxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("zcode usage 读响应失败: %w", err)
	}
	return decodeZcodeUsageEnvelope(body)
}

// zcodeUsageAuthError 标记「上游明确拒绝了凭据」（HTTP 200 信封 code
// 401/1001）。这类失败不允许降级旧值。
type zcodeUsageAuthError struct{ code int }

func (e *zcodeUsageAuthError) Error() string {
	return fmt.Sprintf("zcode usage 鉴权失败（code %d）：凭据可能已过期，请重新 login zcode", e.code)
}

func isZcodeUsageAuthError(err error) bool {
	var authErr *zcodeUsageAuthError
	return errors.As(err, &authErr)
}

func isZcodeUsageAuthErrorText(message string) bool {
	return strings.Contains(message, "zcode usage 鉴权失败")
}

// decodeZcodeUsageEnvelope 解开上游的 {"code":..,"data":..} 信封。
// glm-usage 实测：鉴权失败回 HTTP 200 + code 401/1001；成功 code 为 200 或缺省。
func decodeZcodeUsageEnvelope(body []byte) (json.RawMessage, error) {
	var envelope struct {
		Code    *int            `json:"code"`
		Success *bool           `json:"success"`
		Message string          `json:"message"`
		Msg     string          `json:"msg"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("zcode usage 返回非 JSON 响应: %w", err)
	}
	if envelope.Code != nil && (*envelope.Code == 401 || *envelope.Code == 1001) {
		return nil, &zcodeUsageAuthError{code: *envelope.Code}
	}
	if (envelope.Success != nil && !*envelope.Success) || (envelope.Code != nil && *envelope.Code != 200) {
		message := envelope.Message
		if message == "" {
			message = envelope.Msg
		}
		if message == "" {
			message = fmt.Sprintf("错误码 %d", *envelope.Code)
		}
		return nil, fmt.Errorf("zcode usage 上游错误: %s", message)
	}
	if len(envelope.Data) == 0 {
		return nil, errors.New("zcode usage 响应缺少 data 字段")
	}
	return envelope.Data, nil
}

// ---------------------------------------------------------------------------
// 时间窗：上游只认 Asia/Shanghai 的 "YYYY-MM-DD HH:mm:ss"（固定 UTC+8，
// 无夏令时）。缺省窗口对齐到整点/整天，保证同一窗口内的反复请求共享缓存。
// ---------------------------------------------------------------------------

const zcodeUsageLocationOffset = 8 * time.Hour

type zcodeUsageWindow struct {
	startShanghai string
	endShanghai   string
	days          int
}

// resolveZcodeUsageWindow 解析 ?days=（1..366）。缺省 7 天；endTime 对齐
// 当前整点（上游是日粒度聚合，秒级边界只会让缓存键每秒一换），start 取
// end 前 days 天。
func resolveZcodeUsageWindow(now time.Time, daysParam string) (zcodeUsageWindow, error) {
	days := 7
	if daysParam != "" {
		parsed, err := strconv.Atoi(daysParam)
		if err != nil || parsed < 1 || parsed > 366 {
			return zcodeUsageWindow{}, errors.New("days 必须是 1-366 的整数")
		}
		days = parsed
	}
	shanghaiNow := now.UTC().Add(zcodeUsageLocationOffset)
	end := shanghaiNow.Truncate(time.Hour)
	start := end.AddDate(0, 0, -days)
	return zcodeUsageWindow{
		startShanghai: start.Format("2006-01-02 15:04:05"),
		endShanghai:   end.Format("2006-01-02 15:04:05"),
		days:          days,
	}, nil
}

func (w zcodeUsageWindow) params() map[string]string {
	return map[string]string{"startTime": w.startShanghai, "endTime": w.endShanghai}
}

func (w zcodeUsageWindow) cacheKey() string {
	return w.startShanghai + ".." + w.endShanghai
}

// ---------------------------------------------------------------------------
// HTTP handler：GET /v1/zcode/usage
// ---------------------------------------------------------------------------

// zcodeUsageResponse 是聚合响应。sections 的值原样透传上游 data
// （json.RawMessage），不做字段猜测；usage 的 data 内含 summary /
// modelSummaryList / xTime / modelDataList，前端按 glm-usage 同款逻辑渲染。
type zcodeUsageResponse struct {
	Range      zcodeUsageWindowMeta       `json:"range"`
	Sections   map[string]json.RawMessage `json:"sections"`
	Failures   []zcodeUsageFailure        `json:"failures"`
	Credential string                     `json:"credential,omitempty"`
}

type zcodeUsageWindowMeta struct {
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
	Days      int    `json:"days"`
	Timezone  string `json:"timezone"`
}

type zcodeUsageFailure struct {
	Section string `json:"section"`
	Error   string `json:"error"`
}

// zcodeUsageShared 返回进程内用量缓存。懒初始化：测试可以直接构造
// Router{} 而不必经过 NewWithLogger。
func (r *Router) zcodeUsageShared() *zcodeUsageCache {
	r.zcodeUsageOnce.Do(func() { r.zcodeUsage = newZcodeUsageCache() })
	return r.zcodeUsage
}

// zcodeUsageHandler 服务 GET /v1/zcode/usage?days=7。四个上游端点并发拉取，
// 一块失败不影响其他块；全部失败时按失败形态选状态码（凭据问题 401，
// 其他 502）。
func (r *Router) zcodeUsageHandler(w http.ResponseWriter, q *http.Request) {
	window, err := resolveZcodeUsageWindow(time.Now(), q.URL.Query().Get("days"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error(), "invalid_days")
		return
	}
	// 页面请求不该被下游客户端的断开间接掐断刷新：脱离请求 ctx，只留超时。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(q.Context()), zcodeUsageTimeout+2*time.Second)
	defer cancel()

	// 凭据与生成路径同一条链：数据库 → 凭据文件，bigmodel 优先于 zai。
	channel := ZcodeChannelBigmodel
	token := r.resolveCandidateAPIKey(ctx, Candidate{ZcodeTokenFile: true, ZcodeChannel: channel})
	if token == "" {
		token = r.resolveCandidateAPIKey(ctx, Candidate{ZcodeTokenFile: true, ZcodeChannel: ZcodeChannelZai})
		if token != "" {
			channel = ZcodeChannelZai
		}
	}
	if token == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "没有可用的 ZCode 凭据，请先运行 login zcode", "zcode_credential_missing")
		return
	}

	type result struct {
		key  string
		data json.RawMessage
		err  error
	}
	results := make(chan result, len(zcodeUsageEndpoints))
	var wg sync.WaitGroup
	for _, ep := range zcodeUsageEndpoints {
		wg.Add(1)
		go func(ep zcodeUsageEndpoint) {
			defer wg.Done()
			data, err := r.zcodeUsageShared().fetch(ctx, ep, window, token)
			results <- result{key: ep.key, data: data, err: err}
		}(ep)
	}
	wg.Wait()
	close(results)

	resp := zcodeUsageResponse{
		Range: zcodeUsageWindowMeta{
			StartTime: window.startShanghai,
			EndTime:   window.endShanghai,
			Days:      window.days,
			Timezone:  "Asia/Shanghai",
		},
		Sections:   map[string]json.RawMessage{},
		Failures:   []zcodeUsageFailure{},
		Credential: channel,
	}
	for res := range results {
		if res.err != nil {
			resp.Failures = append(resp.Failures, zcodeUsageFailure{Section: res.key, Error: res.err.Error()})
		}
		// 降级旧值同时携带数据与错误：数据照常进 sections，错误进 failures。
		if len(res.data) > 0 {
			resp.Sections[res.key] = res.data
		}
	}
	if len(resp.Sections) == 0 {
		status := http.StatusBadGateway
		for _, failure := range resp.Failures {
			if isZcodeUsageAuthErrorText(failure.Error) {
				status = http.StatusUnauthorized
				break
			}
		}
		writeJSONError(w, status, "ZCode 用量查询全部失败", "zcode_usage_unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}
