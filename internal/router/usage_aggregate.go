package router

// 账号额度聚合端点 GET /v1/usage：把网关手上所有账号级额度归一成一张卡。
//
// 与 /v1/zcode/usage 的分工：后者是智谱看板的原样透传（sections 保留上游
// 形状），本端点只输出归一化后的窗口（windows），供 dsh-nanorouter 之类
// 的插件直接画计量条，不关心上游信封长什么样。
//
// 三条纪律：
//   - available 只表示「有凭据」，不表示上游健康；上游失败进 failures，
//     其余事实照常渲染（fail-open，与 Mistral 官方 CLI 一致）。
//   - 一个 provider 失败不影响另一个：并发拉取，逐段收集错误。
//   - 不在网关侧截断超额：used_percent 允许 >100，「已超支」必须看得见。

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	json "github.com/goccy/go-json"
)

// usageProviderZcode / usageProviderMistral 是 provider 标识。
const (
	usageProviderZcode   = "zcode"
	usageProviderMistral = "mistral-vibe"
)

// usageWindowKinds 是归一化后的窗口类型。
const (
	usageWindowFiveHour     = "five_hour"
	usageWindowWeekly       = "weekly"
	usageWindowMonthlyTools = "monthly_tools"
	usageWindowMonthlyQuota = "monthly_quota"
	usageWindowOther        = "other"
)

// usageWindow 是一条计量条。Limit/Used/Remaining 为 0 时表示上游未声明，
// 前端只应渲染 ResetAt 与 UsedPercent。
type usageWindow struct {
	Kind        string  `json:"kind"`
	Type        string  `json:"type,omitempty"`
	Unit        int     `json:"unit,omitempty"`
	Number      int     `json:"number,omitempty"`
	Limit       float64 `json:"limit"`
	Used        float64 `json:"used"`
	Remaining   float64 `json:"remaining"`
	UsedPercent float64 `json:"used_percent"`
	ResetAt     int64   `json:"reset_at,omitempty"`
	PaygEnabled *bool   `json:"payg_enabled,omitempty"`
}

// usagePlan 是套餐徽标需要的字段，两种 provider 各自填能填的。
type usagePlan struct {
	Name         string `json:"name,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Title        string `json:"title,omitempty"`
	AccessScope  string `json:"access_scope,omitempty"`
	Status       string `json:"status,omitempty"`
	BillingCycle string `json:"billing_cycle,omitempty"`
	Valid        string `json:"valid,omitempty"`
	AutoRenew    *bool  `json:"auto_renew,omitempty"`
}

// usageResetInventory 是 ?include=reset 时的重置卡库存。
type usageResetInventory struct {
	Channel              string `json:"channel,omitempty"`
	FetchedAt            string `json:"fetched_at,omitempty"`
	FiveHourResets       int    `json:"five_hour_resets"`
	WeekResets           int    `json:"week_resets"`
	LatestFiveHourUsedAt int64  `json:"latest_five_hour_used_at,omitempty"`
	LatestWeekUsedAt     int64  `json:"latest_week_used_at,omitempty"`
	HasUnreadHistory     bool   `json:"has_unread_history"`
}

// usageProviderResult 是一个 provider 的读取结果。
type usageProviderResult struct {
	Provider   string                     `json:"provider"`
	Available  bool                       `json:"available"`
	Credential string                     `json:"credential,omitempty"`
	KeyPreview string                     `json:"key_preview,omitempty"`
	Plan       *usagePlan                 `json:"plan,omitempty"`
	Windows    []usageWindow              `json:"windows"`
	Sections   map[string]json.RawMessage `json:"sections,omitempty"`
	Reset      *usageResetInventory       `json:"reset,omitempty"`
	Failures   []string                   `json:"failures"`
	// outcome 决定整体状态码，不对外序列化。
	outcome usageOutcome
}

// usageOutcome 是一个 provider 的读取结局。
type usageOutcome int

const (
	// usageOutcomeData：拿到至少一份可用于渲染的数据（含降级旧值）。
	usageOutcomeData usageOutcome = iota
	// usageOutcomeMissing：本地没有凭据。
	usageOutcomeMissing
	// usageOutcomeRejected：本地有凭据但上游拒绝。
	usageOutcomeRejected
	// usageOutcomeFailed：其余上游/协议故障。
	usageOutcomeFailed
)

// usageOutcomeData 之外的两个本地可判定结局由 provider 读取函数直接标注，
// 只有「缺凭据」需要文本兜底（zcode 与 mistral 各自的提示语不同）。
const (
	usageMissingZcodeText   = "no zcode credential"
	usageMissingMistralText = "no mistral credential"
)

func isUsageCredentialMissingText(message string) bool {
	return strings.Contains(message, usageMissingZcodeText) ||
		strings.Contains(message, usageMissingMistralText)
}

// classifyUsageOutcome 按 outcome 优先，其次按 failures 文本判定——
// 上游错误一旦折成字符串，就只能靠文本区分是谁的错。
func classifyUsageOutcome(result usageProviderResult) usageOutcome {
	if result.outcome != usageOutcomeData {
		return result.outcome
	}
	if len(result.Windows) > 0 || result.Plan != nil || len(result.Sections) > 0 || result.Reset != nil {
		return usageOutcomeData
	}
	if len(result.Failures) == 0 {
		return usageOutcomeData
	}
	for _, failure := range result.Failures {
		if !isUsageCredentialMissingText(failure) {
			return usageOutcomeFailed
		}
	}
	return usageOutcomeMissing
}

// usageResponse 是 /v1/usage 的完整响应。providers 为空数组而不是 404：
// 插件可以直接渲染「未配置」。
type usageResponse struct {
	GeneratedAt string                `json:"generated_at"`
	Range       zcodeUsageWindowMeta  `json:"range"`
	Providers   []usageProviderResult `json:"providers"`
}

// UsageConfig 控制 /v1/usage 的读取参数。全部可选，缺省按候选推导。
type UsageConfig struct {
	// MistralAPIBase 覆盖 billing 基址；默认取启用 Mistral 凭据的候选
	// base_url（其形态本就是 https://api.mistral.ai/v1）。
	MistralAPIBase string `yaml:"mistral_api_base"`
	// MistralConsoleAPIBase 覆盖 console /api 基址（whoami）。
	MistralConsoleAPIBase string `yaml:"mistral_console_api_base"`
	// Timeout 是单端点超时；0 时用 mistralUsageDefaultTimeout。
	Timeout time.Duration `yaml:"timeout"`
	// Disabled 关闭指定 provider 的采集（端点仍返回，available 为 false）。
	Disabled []string `yaml:"disabled"`
}

// normalize 填默认值并校验 disabled 里的名字。
func (c *UsageConfig) normalize() error {
	if c.Timeout == 0 {
		c.Timeout = mistralUsageDefaultTimeout
	}
	if c.Timeout < 0 {
		return fmt.Errorf("usage.timeout must not be negative")
	}
	for _, name := range c.Disabled {
		switch strings.TrimSpace(name) {
		case usageProviderZcode, usageProviderMistral:
		default:
			return fmt.Errorf("usage.disabled: unknown provider %q", name)
		}
	}
	return nil
}

// disabledSet 把 Disabled 折成查找表。
func (c UsageConfig) disabledSet() map[string]bool {
	set := make(map[string]bool, len(c.Disabled))
	for _, name := range c.Disabled {
		set[strings.TrimSpace(name)] = true
	}
	return set
}

// usageReaderShared 是 Mistral 账号段的进程内缓存，懒初始化。
func (r *Router) usageReaderShared() *usageReaderCache {
	r.usageReaderOnce.Do(func() { r.usageReader = newUsageReaderCache() })
	return r.usageReader
}

// mistralUsageCandidate 返回第一个可用于账号读取的候选：优先有 Mistral
// 凭据的，其次声明了 mistral_compat / vibe_catalog 的（两者都靠 Mistral
// key 说话）。
func (r *Router) mistralUsageCandidate() (Candidate, bool) {
	var fallback Candidate
	var hasFallback bool
	for _, candidate := range r.allCandidates {
		if !candidate.IsEnabled() {
			continue
		}
		if candidate.MistralTokenFile || strings.EqualFold(candidate.APIKeyEnv, "MISTRAL_API_KEY") {
			return candidate, true
		}
		if (candidate.VibeCatalog || candidate.MistralCompat) && !hasFallback {
			fallback, hasFallback = candidate, true
		}
	}
	return fallback, hasFallback
}

// hasZcodeUsageCandidate 与 primaryZcodeChannel 的判定同源。
func (r *Router) hasZcodeUsageCandidate() bool {
	for _, candidate := range r.allCandidates {
		if candidate.ZcodeTokenFile && candidate.Anthropic && candidate.IsEnabled() {
			return true
		}
	}
	return false
}

// knownUsageProviders 是已配置凭据的 provider 列表，保持稳定顺序。
func (r *Router) knownUsageProviders() []string {
	providers := make([]string, 0, 2)
	if r.hasZcodeUsageCandidate() {
		providers = append(providers, usageProviderZcode)
	}
	if _, ok := r.mistralUsageCandidate(); ok {
		providers = append(providers, usageProviderMistral)
	}
	return providers
}

// parseUsageProviders 解析 ?providers=，未知名字报 400。
func parseUsageProviders(raw string, known []string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return known, nil
	}
	seen := make(map[string]bool, 2)
	selected := make([]string, 0, 2)
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		switch name {
		case usageProviderZcode, usageProviderMistral:
		default:
			return nil, fmt.Errorf("unknown usage provider %q", name)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		// 请求了但没配置凭据：保留在结果里并以 available=false 呈现，
		// 比静默丢弃更容易排查。
		selected = append(selected, name)
	}
	return selected, nil
}

// zcodeWindows 把 quota 段的 limits 归一成计量条。
func zcodeWindows(quota json.RawMessage) []usageWindow {
	windows := []usageWindow{}
	if len(quota) == 0 {
		return windows
	}
	record, ok := parseJSONRecord(quota)
	if !ok {
		return windows
	}
	limits, _ := record["limits"].([]any)
	for _, entry := range limits {
		limit, ok := asRecord(entry)
		if !ok {
			continue
		}
		kind := zcodeWindowKind(jsonString(limit["type"]), intOf(limit["unit"]), intOf(limit["number"]))
		limitValue, _ := jsonNumber(limit["usage"])
		used, _ := jsonNumber(limit["currentValue"])
		remaining, okRemaining := jsonNumber(limit["remaining"])
		if !okRemaining {
			remaining = limitValue - used
			if remaining < 0 {
				remaining = 0
			}
		}
		percent, okPercent := jsonNumber(limit["percentage"])
		if !okPercent {
			if limitValue > 0 {
				percent = used / limitValue * 100
			}
		}
		windows = append(windows, usageWindow{
			Kind:        kind,
			Type:        jsonString(limit["type"]),
			Unit:        intOf(limit["unit"]),
			Number:      intOf(limit["number"]),
			Limit:       limitValue,
			Used:        used,
			Remaining:   remaining,
			UsedPercent: percent,
			ResetAt:     epochMillis(limit["nextResetTime"]),
		})
	}
	return windows
}

// zcodeWindowKind 取 dsh-zhipu-provider 的 windowKind 规则。
func zcodeWindowKind(windowType string, unit, number int) string {
	switch {
	case unit == 3 && number == 5:
		return usageWindowFiveHour
	case unit == 6:
		return usageWindowWeekly
	case unit == 5 && number == 1:
		return usageWindowMonthlyTools
	}
	return usageWindowOther
}

// intOf 把 JSON 数字收窄成 int（缺省 0）。
func intOf(value any) int {
	number, ok := jsonNumber(value)
	if !ok {
		return 0
	}
	return int(number)
}

// epochMillis 归一化秒/毫秒时间戳：>1e11 视为毫秒（quota.ts:epochMillis）。
func epochMillis(value any) int64 {
	raw, ok := jsonNumber(value)
	if !ok || raw <= 0 {
		return 0
	}
	if raw > 1e11 {
		return int64(raw)
	}
	return int64(raw * 1000)
}

// usageHandler 服务 GET /v1/usage?days=7&providers=…&include=reset。
func (r *Router) usageHandler(w http.ResponseWriter, q *http.Request) {
	window, err := resolveZcodeUsageWindow(time.Now(), q.URL.Query().Get("days"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error(), "invalid_days")
		return
	}
	known := r.knownUsageProviders()
	requested, err := parseUsageProviders(q.URL.Query().Get("providers"), known)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error(), "unknown_usage_provider")
		return
	}
	includeReset := q.URL.Query().Get("include") == "reset"

	// 页面请求不该被浏览器断开掐断缓存填充：脱离请求 ctx，只留超时。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(q.Context()), zcodeUsageTimeout*2)
	defer cancel()

	disabled := r.cfg.Usage.disabledSet()
	results := make([]usageProviderResult, len(requested))
	var wg sync.WaitGroup
	for index, provider := range requested {
		wg.Add(1)
		go func(index int, provider string) {
			defer wg.Done()
			result := r.readUsageProvider(ctx, provider, window, includeReset, disabled[provider])
			// 一条 provider 一次失败只计一次：同一 provider 的几个段都挂了
			// 是同一场故障，不是几场。
			if len(result.Failures) > 0 && result.outcome != usageOutcomeMissing {
				r.usageFailures.WithLabelValues(provider).Inc()
			}
			results[index] = result
		}(index, provider)
	}
	wg.Wait()

	// 状态码按「全部失败」的形态选：凭据缺失 503、上游拒绝凭据 401、其余 502。
	// 任何一个 provider 有数据（含降级旧值）都是 200。
	outcomes := make([]usageOutcome, len(results))
	for index, result := range results {
		outcomes[index] = classifyUsageOutcome(result)
	}
	anyData := false
	allMissing := len(results) > 0
	allMissingOrRejected := len(results) > 0
	for _, outcome := range outcomes {
		if outcome == usageOutcomeData {
			anyData = true
		}
		if outcome != usageOutcomeMissing {
			allMissing = false
		}
		if outcome != usageOutcomeMissing && outcome != usageOutcomeRejected {
			allMissingOrRejected = false
		}
	}
	switch {
	case anyData || len(results) == 0:
		// 没有可用 provider（未配置任何账号凭据）不是错误：返回空 providers。
	case allMissing:
		writeJSONError(w, http.StatusServiceUnavailable,
			"没有可用的账号凭据，请先运行 login zcode / login mistral", "usage_credential_missing")
		return
	case allMissingOrRejected:
		writeJSONError(w, http.StatusUnauthorized, "账号额度读取被上游拒绝，请重新登录", "usage_credentials_rejected")
		return
	default:
		writeJSONError(w, http.StatusBadGateway, "账号额度查询全部失败", "usage_unavailable")
		return
	}

	resp := usageResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Range: zcodeUsageWindowMeta{
			StartTime: window.startShanghai,
			EndTime:   window.endShanghai,
			Days:      window.days,
			Timezone:  "Asia/Shanghai",
		},
		Providers: results,
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

// readUsageProvider 读取单个 provider。任何错误都折成 failures，不再上抛。
func (r *Router) readUsageProvider(
	ctx context.Context,
	provider string,
	window zcodeUsageWindow,
	includeReset bool,
	disabled bool,
) usageProviderResult {
	if disabled {
		return usageProviderResult{Provider: provider, Windows: []usageWindow{}, Failures: []string{}}
	}
	switch provider {
	case usageProviderZcode:
		return r.readZcodeUsage(ctx, window, includeReset)
	case usageProviderMistral:
		return r.readMistralUsage(ctx)
	}
	return usageProviderResult{Provider: provider, Windows: []usageWindow{}, Failures: []string{"unknown provider"}}
}

// readZcodeUsage 复用 zcode 的单飞缓存，并把 quota 归一成 windows。
func (r *Router) readZcodeUsage(ctx context.Context, window zcodeUsageWindow, includeReset bool) usageProviderResult {
	result := usageProviderResult{
		Provider: usageProviderZcode,
		Windows:  []usageWindow{},
		Failures: []string{},
	}
	channel := ZcodeChannelBigmodel
	token := r.resolveCandidateAPIKey(ctx, Candidate{ZcodeTokenFile: true, ZcodeChannel: channel})
	if token == "" {
		token = r.resolveCandidateAPIKey(ctx, Candidate{ZcodeTokenFile: true, ZcodeChannel: ZcodeChannelZai})
		if token != "" {
			channel = ZcodeChannelZai
		}
	}
	if token == "" {
		result.Failures = append(result.Failures, "no zcode credential: run login zcode")
		result.outcome = usageOutcomeMissing
		return result
	}
	result.Available = true
	result.Credential = channel
	result.KeyPreview = mistralKeyPreview(token)

	cache := r.zcodeUsageShared()
	quota, err := cache.fetch(ctx, zcodeUsageEndpointByKey("quota"), window, token, channel)
	if err != nil {
		result.Failures = append(result.Failures, err.Error())
		if isZcodeUsageAuthError(err) {
			result.outcome = usageOutcomeRejected
		}
	} else {
		result.Windows = zcodeWindows(quota)
	}

	// sections 只保留其余段，供排障；quota 已归一进 windows，不重复。
	result.Sections = map[string]json.RawMessage{}
	type sectionResult struct {
		key  string
		data json.RawMessage
		err  error
	}
	sections := make(chan sectionResult, len(zcodeUsageEndpoints))
	var wg sync.WaitGroup
	for _, endpoint := range zcodeUsageEndpoints {
		if endpoint.key == "quota" {
			continue
		}
		wg.Add(1)
		go func(endpoint zcodeUsageEndpoint) {
			defer wg.Done()
			data, err := cache.fetch(ctx, endpoint, window, token, channel)
			sections <- sectionResult{key: endpoint.key, data: data, err: err}
		}(endpoint)
	}
	wg.Wait()
	close(sections)
	for section := range sections {
		if section.err != nil {
			result.Failures = append(result.Failures, fmt.Sprintf("%s: %s", section.key, section.err.Error()))
			continue
		}
		if len(section.data) > 0 {
			result.Sections[section.key] = section.data
		}
	}
	// subscription 段同时填套餐徽标。
	if raw, ok := result.Sections["subscription"]; ok {
		result.Plan = zcodePlanFromSubscription(raw, result.Sections["auto_renew"])
	}
	if includeReset {
		result.Reset = r.zcodeUsageReset(ctx, channel, result)
	}
	if len(result.Sections) == 0 {
		result.Sections = nil
	}
	return result
}

// zcodeUsageEndpointByKey 按 key 找端点定义。
func zcodeUsageEndpointByKey(key string) zcodeUsageEndpoint {
	for _, endpoint := range zcodeUsageEndpoints {
		if endpoint.key == key {
			return endpoint
		}
	}
	return zcodeUsageEndpoint{key: key}
}

// zcodePlanFromSubscription 从 subscription 段挑一条有效订阅填徽标。
func zcodePlanFromSubscription(subscription, autoRenew json.RawMessage) *usagePlan {
	record, ok := parseJSONRecord(subscription)
	if !ok {
		return nil
	}
	entries, _ := record["list"].([]any)
	if len(entries) == 0 {
		return nil
	}
	chosen, ok := asRecord(entries[0])
	if !ok {
		return nil
	}
	for _, entry := range entries {
		candidate, ok := asRecord(entry)
		if !ok {
			continue
		}
		if strings.EqualFold(jsonString(candidate["status"]), "VALID") {
			chosen = candidate
			break
		}
	}
	plan := &usagePlan{
		Name:         jsonString(chosen["productName"]),
		Status:       jsonString(chosen["status"]),
		BillingCycle: jsonString(chosen["billingCycle"]),
		Valid:        jsonString(chosen["valid"]),
	}
	if renew := jsonBool(chosen["autoRenew"]); renew != nil {
		plan.AutoRenew = renew
	} else if len(autoRenew) > 0 {
		var flag bool
		if json.Unmarshal(autoRenew, &flag) == nil {
			plan.AutoRenew = &flag
		}
	}
	return plan
}

// zcodeUsageReset 读重置卡库存。失败只进 failures，不改变状态码。
func (r *Router) zcodeUsageReset(ctx context.Context, channel string, result usageProviderResult) *usageResetInventory {
	credential, err := r.zcodeResetCredential(ctx, channel)
	if err != nil {
		result.Failures = append(result.Failures, "reset: "+err.Error())
		return nil
	}
	status, fetchedAt, err := r.zcodeResetShared().fetchStatus(ctx, credential, false)
	if err != nil {
		result.Failures = append(result.Failures, "reset: "+err.Error())
		return nil
	}
	inventory := &usageResetInventory{
		Channel:          credential.Channel,
		FetchedAt:        fetchedAt.UTC().Format(time.RFC3339),
		FiveHourResets:   len(status.AvailableFiveHourResets),
		WeekResets:       len(status.AvailableWeekResets),
		HasUnreadHistory: status.HasUnreadHistory,
	}
	if status.LatestFiveHourResetHistory != nil {
		inventory.LatestFiveHourUsedAt = status.LatestFiveHourResetHistory.UsedAt
	}
	if status.LatestWeekResetHistory != nil {
		inventory.LatestWeekUsedAt = status.LatestWeekResetHistory.UsedAt
	}
	return inventory
}

// readMistralUsage 读 billing + whoami，并归一出一条月度额度窗口。
func (r *Router) readMistralUsage(ctx context.Context) usageProviderResult {
	result := usageProviderResult{
		Provider: usageProviderMistral,
		Windows:  []usageWindow{},
		Failures: []string{},
	}
	candidate, ok := r.mistralUsageCandidate()
	if !ok {
		result.Failures = append(result.Failures, "no mistral credential: run login mistral")
		result.outcome = usageOutcomeMissing
		return result
	}
	token := r.resolveCandidateAPIKey(ctx, candidate)
	if token == "" {
		result.Failures = append(result.Failures, "no mistral credential: run login mistral")
		result.outcome = usageOutcomeMissing
		return result
	}
	result.Available = true
	result.KeyPreview = mistralKeyPreview(token)

	apiBase := r.cfg.Usage.MistralAPIBase
	if apiBase == "" {
		apiBase = candidate.BaseURL
	}
	consoleBase := r.cfg.Usage.MistralConsoleAPIBase
	if consoleBase == "" {
		consoleBase = mistralBrowserAuthAPIBase
	}
	reader := &mistralUsageReader{
		cache:       r.usageReaderShared(),
		client:      r.client,
		apiBase:     apiBase,
		consoleBase: consoleBase,
		timeout:     r.cfg.Usage.Timeout,
	}
	read := reader.read(ctx, token)
	result.Failures = append(result.Failures, read.Failures...)
	if read.Rejected {
		result.outcome = usageOutcomeRejected
	}

	if read.WhoAmI != nil {
		kind := mistralPlanKind(read.WhoAmI.PlanType)
		plan := &usagePlan{
			Name:        read.WhoAmI.PlanName,
			Kind:        kind,
			AccessScope: read.WhoAmI.AccessScope,
		}
		if title := mistralPlanTitle(kind, read.WhoAmI.PlanName, read.Subscription); title != "" {
			plan.Title = title
		}
		result.Plan = plan
		// 租户域与请求域不一致时明确提示：租户 key 打到公共域会 401。
		if read.WhoAmI.APIBase != "" && mistralAPIV1Base(read.WhoAmI.APIBase) != mistralAPIV1Base(apiBase) {
			result.Failures = append(result.Failures,
				fmt.Sprintf("tenant advertises api_base %s; set usage.mistral_api_base to it", read.WhoAmI.APIBase))
		}
	}
	if budget := read.Budget; budget != nil && budget.InitialBudget > 0 {
		used := budget.UsagePercentage
		if used < 0 {
			used = 0
		}
		payg := budget.PaygEnabled
		if payg == nil && read.Subscription != nil {
			payg = read.Subscription.PaygEnabled
		}
		result.Windows = append(result.Windows, usageWindow{
			Kind:        usageWindowMonthlyQuota,
			Limit:       budget.InitialBudget,
			Used:        budget.InitialBudget * used / 100,
			Remaining:   budget.InitialBudget * (100 - used) / 100,
			UsedPercent: used,
			ResetAt:     rfc3339Millis(budget.ResetAt),
			PaygEnabled: payg,
		})
	}
	return result
}

// rfc3339Millis 把 RFC3339 文本转成毫秒时间戳；解析失败返回 0。
func rfc3339Millis(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0
	}
	return parsed.UnixMilli()
}
