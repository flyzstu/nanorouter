package router

// Mistral Vibe 的账号面只读：套餐、月度额度、whoami。
//
// 这三条端点与官方 Vibe CLI 的 _billing.py / _account.py 同源，路径与
// 解析规则取自 dsh-mistral-vibe 的 src/account.ts（同一套 fail-open 语义）：
// 401/403/404 表示「这个账号/平台不提供这项」，是状态而不是故障，不能
// 在卡片上挂红字；其余非 2xx 才算失败。
//
// 与 zcode 用量不同，Mistral 只有一个域，缓存键必须含凭据指纹：重新
// login mistral 换 key 后，旧账号的数据不能继续冒充新账号。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	json "github.com/goccy/go-json"
)

const (
	// mistralUsageDefaultTimeout 是单端点超时：卡片页脚不值得长等。
	mistralUsageDefaultTimeout = 3 * time.Second
	// mistralUsageMaxBody 限制响应体：三个端点都是小 JSON。
	mistralUsageMaxBody = 1 << 20
	// 缓存 TTL：billing 变化不快，whoami 的套餐类型几乎不变。
	mistralUsageBillingTTL = 5 * time.Minute
	mistralUsageWhoAmITTL  = time.Hour
	// 失败后的旧值服役窗与冷失败退避，语义对齐 zcodeUsageStaleTTL /
	// zcodeUsageFailureRetry。
	mistralUsageStaleTTL = 10 * time.Minute
	usageFailureRetry    = 30 * time.Second
	usageSingleflightTTL = 10 * time.Second
	// keyPreviewMinLen 与 account.ts 的 keyPreview 一致：短 key 不露尾四位。
	keyPreviewMinLen = 16
)

// usageAuthError 标记「上游明确拒绝凭据」。这类失败不允许降级旧值：
// 凭据失效必须暴露给操作者，而不是用过期数字盖住。
type usageAuthError struct {
	what   string
	status int
}

func (e *usageAuthError) Error() string {
	return fmt.Sprintf("%s refused the credential (HTTP %d)", e.what, e.status)
}

func isUsageAuthError(err error) bool {
	var authErr *usageAuthError
	return errors.As(err, &authErr)
}

// usageSnapshot 是一个缓存段。
type usageSnapshot struct {
	data      json.RawMessage
	fetchedAt time.Time
	err       error
}

type usageCall struct {
	done    chan struct{}
	started time.Time
	data    json.RawMessage
	err     error
}

// usageReaderCache 是带单飞与 stale-while-revalidate 的通用只读缓存。
// 语义与 zcodeUsageCache 同构，但 key 由调用方给出（含凭据指纹），
// 因此不能直接复用后者按端点列表组织的实现。
type usageReaderCache struct {
	mu       sync.Mutex
	entries  map[string]usageSnapshot
	inflight map[string]*usageCall
	now      func() time.Time
}

func newUsageReaderCache() *usageReaderCache {
	return &usageReaderCache{
		entries:  map[string]usageSnapshot{},
		inflight: map[string]*usageCall{},
		now:      time.Now,
	}
}

func (c *usageReaderCache) get(
	ctx context.Context,
	key string,
	ttl time.Duration,
	read func(context.Context) (json.RawMessage, error),
) (json.RawMessage, error) {
	now := c.now()
	c.mu.Lock()
	entry, ok := c.entries[key]
	if ok && entry.err == nil && now.Sub(entry.fetchedAt) < ttl {
		c.mu.Unlock()
		return entry.data, nil
	}
	// 降级窗：有过好数据、上次刷新失败、仍在 stale 窗内，且失败不是凭据被拒。
	// 正常过期不降级——那会同步刷新，否则每次过期都谎报一次失败。
	if ok && len(entry.data) > 0 && entry.err != nil && now.Sub(entry.fetchedAt) < mistralUsageStaleTTL && !isUsageAuthError(entry.err) {
		staleErr := entry.err
		if call, busy := c.inflight[key]; busy && now.Sub(call.started) < usageSingleflightTTL {
			c.mu.Unlock()
			return entry.data, staleErr
		}
		refresh := &usageCall{done: make(chan struct{}), started: now}
		c.inflight[key] = refresh
		c.mu.Unlock()
		go func() {
			refreshCtx, cancel := context.WithTimeout(context.Background(), mistralUsageDefaultTimeout*2)
			defer cancel()
			data, err := read(refreshCtx)
			c.finish(key, refresh, data, err)
		}()
		return entry.data, staleErr
	}
	if call, busy := c.inflight[key]; busy && now.Sub(call.started) < usageSingleflightTTL {
		c.mu.Unlock()
		select {
		case <-call.done:
			return call.data, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// 冷失败节流：没有旧值可降级时，重试窗内直接回上次的失败。
	if ok && entry.err != nil && len(entry.data) == 0 && now.Sub(entry.fetchedAt) < usageFailureRetry {
		c.mu.Unlock()
		return nil, entry.err
	}
	fresh := &usageCall{done: make(chan struct{}), started: now}
	c.inflight[key] = fresh
	c.mu.Unlock()

	data, err := read(ctx)
	c.finish(key, fresh, data, err)
	// 用 finish 归一后的结果：刷新失败时它会带回 stale 窗内的旧数据。
	return fresh.data, fresh.err
}

func (c *usageReaderCache) finish(key string, call *usageCall, data json.RawMessage, err error) {
	c.mu.Lock()
	// 失败不覆盖最后一份好数据，并把旧值随错误一起交出：调用方宁可渲染
	// 过期数字并在 failures 里说明，也不要一张空卡。
	if err != nil {
		if entry, ok := c.entries[key]; ok && len(entry.data) > 0 && c.now().Sub(entry.fetchedAt) < mistralUsageStaleTTL {
			c.entries[key] = usageSnapshot{data: entry.data, fetchedAt: entry.fetchedAt, err: err}
			delete(c.inflight, key)
			c.mu.Unlock()
			call.data, call.err = entry.data, err
			close(call.done)
			return
		}
	}
	c.entries[key] = usageSnapshot{data: data, fetchedAt: c.now(), err: err}
	if current, ok := c.inflight[key]; ok && current == call {
		delete(c.inflight, key)
	}
	c.mu.Unlock()
	call.data, call.err = data, err
	close(call.done)
}

// mistralSubscription 是 billing/subscription 的关心字段。
type mistralSubscription struct {
	PlanName    string
	PaygEnabled *bool
}

// mistralBudget 是一期 Vibe Code 额度。
type mistralBudget struct {
	UsagePercentage float64
	InitialBudget   float64
	ResetAt         string
	PaygEnabled     *bool
}

// mistralWhoAmI 是 console /api/vibe/whoami 的关心字段。
type mistralWhoAmI struct {
	PlanType    string
	PlanName    string
	AccessScope string
	APIBase     string
	VibeBase    string
}

// mistralUsageRead 是一次账号读取的完整结果。每个字段都可能为空：
// Failures 只记录真正的故障，缺省能力（403/404）不写进去。
type mistralUsageRead struct {
	Subscription *mistralSubscription
	Budget       *mistralBudget
	WhoAmI       *mistralWhoAmI
	Failures     []string
	// Rejected 表示上游 401 拒绝凭据（不是故障而是需要重新登录）。
	Rejected bool
}

// mistralUsageReader 绑定了端点与缓存的一次读取器。
type mistralUsageReader struct {
	cache       *usageReaderCache
	client      *http.Client
	apiBase     string
	consoleBase string
	timeout     time.Duration
}

// apiV1Base 补齐 /v1 段，与 account.ts 的 apiV1Base 同义。
func mistralAPIV1Base(origin string) string {
	base := strings.TrimRight(strings.TrimSpace(origin), "/")
	if strings.HasSuffix(base, "/v1") {
		return base
	}
	return base + "/v1"
}

// mistralKeyPreview 只露末四位，且仅对足够长的 key（account.ts:keyPreview）。
func mistralKeyPreview(key string) string {
	suffix := ""
	if len(key) >= keyPreviewMinLen {
		suffix = key[len(key)-4:]
	}
	return "****" + suffix
}

// credentialFingerprint 用于缓存键：换 key 必须换缓存。
func credentialFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:4])
}

// getJSON 发一次 GET。401/403/404 返回 (nil, nil)：这是「不提供」而不是故障。
func (m *mistralUsageReader) getJSON(ctx context.Context, what, target, apiKey string) (json.RawMessage, error) {
	reqCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		// 401 是明确拒绝：必须暴露给操作者，否则失效的 key 只会显示成
		// 一张空卡。403/404 视为「该账号/平台没有这一项」（account.ts
		// 的 silent absence 语义）。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, &usageAuthError{what: what, status: resp.StatusCode}
	case http.StatusForbidden, http.StatusNotFound:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: HTTP %d", what, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, mistralUsageMaxBody))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, fmt.Errorf("%s: empty body", what)
	}
	return json.RawMessage(body), nil
}

// read 读三项。任一失败只进 Failures，其余照常返回。
func (m *mistralUsageReader) read(ctx context.Context, apiKey string) mistralUsageRead {
	fingerprint := credentialFingerprint(apiKey)
	apiBase := mistralAPIV1Base(m.apiBase)
	consoleBase := strings.TrimRight(m.consoleBase, "/")

	sections := []struct {
		key    string
		what   string
		target string
		ttl    time.Duration
	}{
		{"subscription", "billing/subscription", apiBase + "/billing/subscription", mistralUsageBillingTTL},
		{"budget", "billing/budget", apiBase + "/billing/budget", mistralUsageBillingTTL},
		{"whoami", "vibe/whoami", consoleBase + "/vibe/whoami", mistralUsageWhoAmITTL},
	}

	result := mistralUsageRead{Failures: []string{}}
	raw := make(map[string]json.RawMessage, len(sections))
	for _, section := range sections {
		target := section.target
		what := section.what
		data, err := m.cacheOrGet(ctx, "mistral|"+section.key+"|"+fingerprint, section.ttl,
			func(ctx context.Context) (json.RawMessage, error) {
				return m.getJSON(ctx, what, target, apiKey)
			})
		if err != nil {
			// 降级返回的旧数据仍然可用：记 failure，同时继续渲染。
			result.Failures = append(result.Failures, err.Error())
			if isUsageAuthError(err) {
				result.Rejected = true
			}
		}
		if len(data) > 0 {
			raw[section.key] = data
		}
	}

	if data, ok := raw["subscription"]; ok {
		result.Subscription = parseMistralSubscription(data)
	}
	if data, ok := raw["budget"]; ok {
		result.Budget = parseMistralBudget(data)
	}
	if data, ok := raw["whoami"]; ok {
		result.WhoAmI = parseMistralWhoAmI(data)
	}
	return result
}

// cacheOrGet 让缓存指针可被替换；nil 时退化为直接读。
func (m *mistralUsageReader) cacheOrGet(
	ctx context.Context,
	key string,
	ttl time.Duration,
	read func(context.Context) (json.RawMessage, error),
) (json.RawMessage, error) {
	if m.cache == nil {
		return read(ctx)
	}
	return m.cache.get(ctx, key, ttl, read)
}

// asRecord 把任意 JSON 值收窄成对象。
func asRecord(value any) (map[string]any, bool) {
	record, ok := value.(map[string]any)
	return record, ok
}

func parseJSONRecord(data json.RawMessage) (map[string]any, bool) {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return nil, false
	}
	return asRecord(value)
}

func jsonNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case int:
		return float64(number), true
	default:
		return 0, false
	}
}

func jsonString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func jsonBool(value any) *bool {
	flag, ok := value.(bool)
	if !ok {
		return nil
	}
	return &flag
}

// parseMistralSubscription 读 billing/subscription：只有 plan_name 是必需的。
func parseMistralSubscription(data json.RawMessage) *mistralSubscription {
	record, ok := parseJSONRecord(data)
	if !ok {
		return nil
	}
	name := jsonString(record["plan_name"])
	if name == "" {
		return nil
	}
	return &mistralSubscription{PlanName: name, PaygEnabled: jsonBool(record["vibe_payg_enabled"])}
}

// parseMistralBudget 读 billing/budget 的 vibe_budget。缺 usage_percentage 或
// initial_budget 视为无额度（account.ts:parseBudget）。
func parseMistralBudget(data json.RawMessage) *mistralBudget {
	record, ok := parseJSONRecord(data)
	if !ok {
		return nil
	}
	budget, ok := asRecord(record["vibe_budget"])
	if !ok {
		return nil
	}
	usage, okUsage := jsonNumber(budget["usage_percentage"])
	initial, okInitial := jsonNumber(budget["initial_budget"])
	if !okUsage || !okInitial {
		return nil
	}
	return &mistralBudget{
		UsagePercentage: usage,
		InitialBudget:   initial,
		ResetAt:         jsonString(budget["reset_at"]),
		PaygEnabled:     jsonBool(budget["payg_enabled"]),
	}
}

// parseMistralWhoAmI 读 console /api/vibe/whoami。
func parseMistralWhoAmI(data json.RawMessage) *mistralWhoAmI {
	record, ok := parseJSONRecord(data)
	if !ok {
		return nil
	}
	planType := jsonString(record["plan_type"])
	planName := jsonString(record["plan_name"])
	if planType == "" || planName == "" {
		return nil
	}
	return &mistralWhoAmI{
		PlanType:    planType,
		PlanName:    planName,
		AccessScope: jsonString(record["primitive_access_scope"]),
		APIBase:     jsonString(record["api_base"]),
		VibeBase:    jsonString(record["vibe_base"]),
	}
}

// mistralPlanKind 映射套餐类型，取 account.ts:planKind。
func mistralPlanKind(planType string) string {
	switch strings.ToLower(strings.TrimSpace(planType)) {
	case "chat":
		return "chat"
	case "api":
		return "api"
	case "mistral_code", "mistral-code":
		return "mistral-code"
	default:
		return "unknown"
	}
}

// mistralPlanTitle 取 account.ts:planTitle 的标题规则。
func mistralPlanTitle(kind, planName string, subscription *mistralSubscription) string {
	name := strings.ToUpper(strings.TrimSpace(planName))
	switch kind {
	case "chat":
		titles := map[string]string{"FREE": "Free", "INDIVIDUAL": "Pro", "EDU": "Student", "TEAM": "Team"}
		key := name
		if subscription != nil {
			key = strings.ToUpper(subscription.PlanName)
		}
		return titles[key]
	case "api":
		if strings.Contains(name, "FREE") {
			return "Free"
		}
		return "[API] Scale plan"
	case "mistral-code":
		switch name {
		case "F":
			return "Mistral Code Free"
		case "E":
			return "Mistral Code Enterprise"
		}
	}
	return ""
}
