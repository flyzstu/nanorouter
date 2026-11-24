package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	json "github.com/goccy/go-json"
)

/* -------------------------------------------------------------------------- */
/* 桩上游                                                                      */
/* -------------------------------------------------------------------------- */

// usageStub 是一个按路径分发的桩上游，同时记录每个路径的命中次数。
// zcode 用量面与 reset 面、Mistral 的 billing 与 console 面共用一个 server，
// 便于断言「谁被调用了几次」。
type usageStub struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	hits     map[string]int
	headers  map[string]http.Header
	handlers map[string]http.HandlerFunc
}

func newUsageStub(t *testing.T) *usageStub {
	t.Helper()
	stub := &usageStub{
		t:        t,
		hits:     map[string]int{},
		headers:  map[string]http.Header{},
		handlers: map[string]http.HandlerFunc{},
	}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.hits[r.URL.Path]++
		stub.headers[r.URL.Path] = r.Header.Clone()
		handler := stub.handlers[r.URL.Path]
		stub.mu.Unlock()
		if handler == nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"unexpected path"}`))
			return
		}
		handler(w, r)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

// jsonHandler 返回固定 JSON。
func jsonHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// statusHandler 返回固定状态码与 JSON。
func statusHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func (s *usageStub) set(path string, handler http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[path] = handler
}

func (s *usageStub) hitCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func (s *usageStub) totalHits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, count := range s.hits {
		total += count
	}
	return total
}

func (s *usageStub) headerFor(path string) http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headers[path]
}

// 默认的 zcode 用量面四段 + 账户面四段响应。
var usageZcodeDefaults = map[string]string{
	"/api/monitor/usage/quota/limit": `{"code":200,"data":{"limits":[` +
		`{"type":"TOKENS_LIMIT","unit":3,"number":5,"usage":100,"currentValue":20,"remaining":80,"percentage":20,"nextResetTime":1762300000000},` +
		`{"type":"CREDIT_LIMIT","unit":6,"number":1,"usage":500,"currentValue":100,"percentage":20}]}}`,
	"/api/monitor/credit-usage/usage-detail":                           `{"code":200,"data":{"summary":{"totalCredits":{"value":12.5}}}}`,
	"/api/monitor/credit-usage/activity":                               `{"code":200,"data":{"summary":{"totalTokens":9000}}}`,
	"/api/monitor/usage/model-performance-day":                         `{"code":200,"data":{"x_time":["2026-10-01"]}}`,
	"/api/biz/subscription/list":                                       `{"code":200,"data":{"list":[{"productName":"GLM Coding Lite","status":"VALID","valid":"2027-09-11 10:00:00-2028-09-11 10:00:00","billingCycle":"annually","autoRenew":false}]}}`,
	"/api/biz/subscription/v1-coding-plan-auto-renew-closed-by-system": `{"code":200,"data":true}`,
	"/api/biz/customer-package-reset/list":                             `{"code":200,"data":{"fiveHourResets":[1,2,3],"weekResets":[1]}}`,
	"/api/biz/account/query-customer-account-report":                   `{"code":200,"data":{"balance":0}}`,
}

const usageResetStatusBody = `{"code":0,"data":{` +
	`"available_five_hour_resets":[{"expire_at":1},{"expire_at":2},{"expire_at":3}],` +
	`"available_week_resets":[{"expire_at":4}],` +
	`"latest_five_hour_reset_history":{"used_at":111},` +
	`"has_unread_history":true}}`

const usageResetPath = "/api/v1/coding-plan/reset/status"

func installZcodeUsageHandlers(stub *usageStub) {
	for path, body := range usageZcodeDefaults {
		stub.set(path, jsonHandler(body))
	}
	stub.set(usageResetPath, jsonHandler(usageResetStatusBody))
}

/* -------------------------------------------------------------------------- */
/* Router 夹具                                                                 */
/* -------------------------------------------------------------------------- */

const usageMistralKey = "mistral-test-key-0123456789"

type usageEnv struct {
	router *Router
	stub   *usageStub
}

type usageEnvOptions struct {
	// withZcode 是否写入 zcode 双 token 凭据并把 zcode 候选挂上。
	withZcode bool
	// withMistral 是否挂 Mistral 候选并注入 API key 环境变量。
	withMistral bool
	// usage 覆盖默认的 Usage 配置。
	usage *UsageConfig
	// configure 在构造 Router 前改 Config。
	configure func(*Config)
}

// newUsageEnv 构造一个两端都指向同一桩上游的 Router。HOME 指向临时目录，
// 避免读到开发机真实的凭据文件。
func newUsageEnv(t *testing.T, opts usageEnvOptions) *usageEnv {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	resetZcodeTokenCache()
	t.Cleanup(resetZcodeTokenCache)
	resetMistralTokenCache()
	t.Cleanup(resetMistralTokenCache)

	stub := newUsageStub(t)
	installZcodeUsageHandlers(stub)

	store := statsTestStore(t)
	candidates := []Candidate{}
	if opts.withZcode {
		credStore := newZcodeCredentialStore(store.db)
		if err := credStore.save(context.Background(), ZcodeCredential{
			Channel:     ZcodeChannelBigmodel,
			APIKey:      "db-zcode-key-0123456789",
			PlatformJWT: "db-platform-jwt",
			OAuthToken:  "db-oauth-token",
		}); err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, Candidate{
			ID: "zcode", BaseURL: stub.server.URL, Model: "glm-5.3",
			Anthropic: true, ZcodeTokenFile: true, ZcodeChannel: ZcodeChannelBigmodel,
		})
	}
	if opts.withMistral {
		t.Setenv("MISTRAL_API_KEY", usageMistralKey)
		candidates = append(candidates, Candidate{
			ID: "mistral", BaseURL: stub.server.URL + "/v1", Model: "mistral-large-latest",
			APIKeyEnv: "MISTRAL_API_KEY",
		})
	}
	if len(candidates) == 0 {
		candidates = append(candidates, Candidate{ID: "plain", BaseURL: stub.server.URL, Model: "m"})
	}

	cfg := Config{
		Routes: map[string]Route{"free": {Candidates: candidates}},
		Usage: UsageConfig{
			MistralConsoleAPIBase: stub.server.URL + "/console/api",
		},
	}
	if opts.usage != nil {
		cfg.Usage = *opts.usage
	}
	if opts.configure != nil {
		opts.configure(&cfg)
	}
	r, err := New(cfg, store.db)
	if err != nil {
		t.Fatal(err)
	}
	// 后台 goroutine（凭据体检、探测）会读库与凭据文件；测试结束前必须
	// 关掉，否则它们继续往全局 token 缓存里写空表，污染后续用例。
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.Close(ctx)
	})
	r.zcodeUsageShared().baseURL = stub.server.URL
	r.zcodeResetShared().baseURL = stub.server.URL
	return &usageEnv{router: r, stub: stub}
}

// installMistral 安装三项账号端点。apiBase 需与候选 base_url 一致，
// 否则 whoami 的租户域校验会额外产生一条 failure。
func (e *usageEnv) installMistral(subscription, budget, whoami string) {
	e.stub.set("/v1/billing/subscription", jsonHandler(subscription))
	e.stub.set("/v1/billing/budget", jsonHandler(budget))
	e.stub.set("/console/api/vibe/whoami", jsonHandler(whoami))
}

const (
	usageMistralSubscriptionBody = `{"plan_name":"INDIVIDUAL","vibe_payg_enabled":true}`
	usageMistralBudgetBody       = `{"vibe_budget":{"usage_percentage":42.5,"initial_budget":100,"reset_at":"2026-12-01T00:00:00Z","payg_enabled":false}}`
)

func (e *usageEnv) mistralWhoAmI() string {
	return `{"plan_type":"chat","plan_name":"INDIVIDUAL","primitive_access_scope":"read",` +
		`"api_base":"` + e.stub.server.URL + `/v1","vibe_base":"` + e.stub.server.URL + `"}`
}

// getUsage 直接打 handler，绕过鉴权层。
func getUsage(t *testing.T, r *Router, query string) (*http.Response, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/usage"+query, nil)
	rec := httptest.NewRecorder()
	r.usageHandler(rec, req)
	res := rec.Result()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return res, body
}

// providerByName 从响应里挑一个 provider 段。
func providerByName(t *testing.T, body map[string]any, name string) map[string]any {
	t.Helper()
	providers, _ := body["providers"].([]any)
	for _, entry := range providers {
		provider, ok := entry.(map[string]any)
		if ok && provider["provider"] == name {
			return provider
		}
	}
	t.Fatalf("provider %q not in response: %v", name, body["providers"])
	return nil
}

func windowsOf(t *testing.T, provider map[string]any) []map[string]any {
	t.Helper()
	raw, _ := provider["windows"].([]any)
	windows := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		if window, ok := entry.(map[string]any); ok {
			windows = append(windows, window)
		}
	}
	return windows
}

func failuresOf(t *testing.T, provider map[string]any) []string {
	t.Helper()
	raw, _ := provider["failures"].([]any)
	failures := make([]string, 0, len(raw))
	for _, entry := range raw {
		if text, ok := entry.(string); ok {
			failures = append(failures, text)
		}
	}
	return failures
}

/* -------------------------------------------------------------------------- */
/* 纯函数                                                                      */
/* -------------------------------------------------------------------------- */

func TestZcodeWindowKindMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		unit   int
		number int
		want   string
	}{
		{"five hour", 3, 5, usageWindowFiveHour},
		{"weekly", 6, 1, usageWindowWeekly},
		{"monthly tools", 5, 1, usageWindowMonthlyTools},
		{"unknown", 9, 9, usageWindowOther},
		// unit 6 优先于 number：周额度不看 number。
		{"weekly ignores number", 6, 5, usageWindowWeekly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := zcodeWindowKind("X", tc.unit, tc.number); got != tc.want {
				t.Fatalf("kind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestZcodeWindowsNormalizesMissingFields(t *testing.T) {
	body := json.RawMessage(`{"limits":[` +
		// remaining 缺省：用 usage-currentValue 推导。
		`{"type":"TOKENS_LIMIT","unit":3,"number":5,"usage":100,"currentValue":130,"percentage":130},` +
		// percentage 缺省：用 currentValue/usage 推导。
		`{"type":"CREDIT_LIMIT","unit":6,"number":1,"usage":50,"currentValue":25}]}`)
	windows := zcodeWindows(body)
	if len(windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(windows))
	}
	// 超额不被截断：130% 必须原样暴露。
	if windows[0].UsedPercent != 130 || windows[0].Remaining != 0 {
		t.Fatalf("over-quota window = %+v", windows[0])
	}
	if windows[0].Kind != usageWindowFiveHour {
		t.Fatalf("kind = %q", windows[0].Kind)
	}
	if windows[1].UsedPercent != 50 {
		t.Fatalf("derived percent = %v, want 50", windows[1].UsedPercent)
	}
	if windows[1].Remaining != 25 {
		t.Fatalf("derived remaining = %v, want 25", windows[1].Remaining)
	}
}

func TestZcodeWindowsEmptyOnGarbage(t *testing.T) {
	for _, body := range []string{"", "null", `{"limits":"nope"}`, `not json`} {
		if windows := zcodeWindows(json.RawMessage(body)); len(windows) != 0 {
			t.Fatalf("body %q produced %d windows", body, len(windows))
		}
	}
}

func TestEpochMillisAcceptsSecondsAndMillis(t *testing.T) {
	if got := epochMillis(float64(1762300000)); got != 1762300000000 {
		t.Fatalf("seconds → %d", got)
	}
	if got := epochMillis(float64(1762300000000)); got != 1762300000000 {
		t.Fatalf("millis → %d", got)
	}
	for _, value := range []any{nil, "x", float64(0), float64(-5)} {
		if got := epochMillis(value); got != 0 {
			t.Fatalf("%v → %d, want 0", value, got)
		}
	}
}

func TestParseUsageProviders(t *testing.T) {
	known := []string{usageProviderZcode, usageProviderMistral}
	got, err := parseUsageProviders("", known)
	if err != nil || len(got) != 2 {
		t.Fatalf("default = %v, %v", got, err)
	}
	got, err = parseUsageProviders(" mistral-vibe ", known)
	if err != nil || len(got) != 1 || got[0] != usageProviderMistral {
		t.Fatalf("single = %v, %v", got, err)
	}
	got, err = parseUsageProviders("zcode,zcode,", known)
	if err != nil || len(got) != 1 || got[0] != usageProviderZcode {
		t.Fatalf("dedupe = %v, %v", got, err)
	}
	if _, err = parseUsageProviders("zhipu", known); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

func TestUsageConfigNormalize(t *testing.T) {
	var cfg UsageConfig
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout != mistralUsageDefaultTimeout {
		t.Fatalf("timeout = %v", cfg.Timeout)
	}
	cfg = UsageConfig{Timeout: -time.Second}
	if err := cfg.normalize(); err == nil {
		t.Fatal("negative timeout accepted")
	}
	cfg = UsageConfig{Disabled: []string{"nope"}}
	if err := cfg.normalize(); err == nil {
		t.Fatal("unknown disabled provider accepted")
	}
	cfg = UsageConfig{Disabled: []string{usageProviderZcode, usageProviderMistral}}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	if !cfg.disabledSet()[usageProviderZcode] || !cfg.disabledSet()[usageProviderMistral] {
		t.Fatalf("disabledSet = %v", cfg.disabledSet())
	}
}

func TestMistralAccountParsers(t *testing.T) {
	subscription := parseMistralSubscription(json.RawMessage(`{"plan_name":"INDIVIDUAL","vibe_payg_enabled":true}`))
	if subscription == nil || subscription.PlanName != "INDIVIDUAL" {
		t.Fatalf("subscription = %+v", subscription)
	}
	if subscription.PaygEnabled == nil || !*subscription.PaygEnabled {
		t.Fatalf("payg = %v", subscription.PaygEnabled)
	}
	if parseMistralSubscription(json.RawMessage(`{}`)) != nil {
		t.Fatal("subscription without plan_name accepted")
	}

	budget := parseMistralBudget(json.RawMessage(`{"vibe_budget":{"usage_percentage":12.5,"initial_budget":80,"reset_at":"2026-12-01T00:00:00Z"}}`))
	if budget == nil || budget.UsagePercentage != 12.5 || budget.InitialBudget != 80 {
		t.Fatalf("budget = %+v", budget)
	}
	// 缺 usage_percentage 或 initial_budget 视为无额度。
	for _, body := range []string{
		`{"vibe_budget":{"initial_budget":80}}`,
		`{"vibe_budget":{"usage_percentage":10}}`,
		`{"vibe_budget":null}`,
		`{}`,
	} {
		if parseMistralBudget(json.RawMessage(body)) != nil {
			t.Fatalf("budget %q accepted", body)
		}
	}

	whoami := parseMistralWhoAmI(json.RawMessage(`{"plan_type":"chat","plan_name":"INDIVIDUAL","primitive_access_scope":"read","api_base":"https://tenant.example/v1"}`))
	if whoami == nil || whoami.PlanType != "chat" || whoami.APIBase != "https://tenant.example/v1" {
		t.Fatalf("whoami = %+v", whoami)
	}
	// plan_type 与 plan_name 都是必需的。
	for _, body := range []string{`{"plan_type":"chat"}`, `{"plan_name":"INDIVIDUAL"}`} {
		if parseMistralWhoAmI(json.RawMessage(body)) != nil {
			t.Fatalf("whoami %q accepted", body)
		}
	}
}

func TestMistralPlanKindAndTitle(t *testing.T) {
	for _, tc := range []struct {
		planType string
		kind     string
		title    string
	}{
		{"chat", "chat", "Pro"},
		{"api", "api", "[API] Scale plan"},
		{"mistral_code", "mistral-code", "Mistral Code Free"},
		{"mystery", "unknown", ""},
	} {
		kind := mistralPlanKind(tc.planType)
		if kind != tc.kind {
			t.Fatalf("%q kind = %q, want %q", tc.planType, kind, tc.kind)
		}
		name := "INDIVIDUAL"
		if tc.planType == "api" {
			name = "SCALE"
		}
		if tc.planType == "mistral_code" {
			name = "F"
		}
		if got := mistralPlanTitle(kind, name, nil); got != tc.title {
			t.Fatalf("%q title = %q, want %q", tc.planType, got, tc.title)
		}
	}
	// chat 的标题优先取 billing 的 plan_name：whoami 可能仍是 FREE。
	subscription := &mistralSubscription{PlanName: "FREE"}
	if got := mistralPlanTitle("chat", "INDIVIDUAL", subscription); got != "Free" {
		t.Fatalf("title = %q, want Free", got)
	}
}

func TestMistralKeyPreviewAndFingerprint(t *testing.T) {
	if got := mistralKeyPreview("short"); got != "****" {
		t.Fatalf("short preview = %q", got)
	}
	if got := mistralKeyPreview(usageMistralKey); got != "****6789" {
		t.Fatalf("preview = %q", got)
	}
	if credentialFingerprint("a") == credentialFingerprint("b") {
		t.Fatal("fingerprints collide for distinct keys")
	}
	if credentialFingerprint("a") != credentialFingerprint("a") {
		t.Fatal("fingerprint unstable")
	}
}

func TestMistralAPIV1Base(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://api.mistral.ai/v1", "https://api.mistral.ai/v1"},
		{"https://api.mistral.ai/v1/", "https://api.mistral.ai/v1"},
		{"https://api.mistral.ai", "https://api.mistral.ai/v1"},
		{"https://tenant.example/api", "https://tenant.example/api/v1"},
		{"  ", "/v1"},
	} {
		if got := mistralAPIV1Base(tc.in); got != tc.want {
			t.Fatalf("%q → %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRFC3339Millis(t *testing.T) {
	if got := rfc3339Millis("2026-12-01T00:00:00Z"); got != 1796083200000 {
		t.Fatalf("got %d", got)
	}
	for _, value := range []string{"", "nope", "2026-12-01"} {
		if got := rfc3339Millis(value); got != 0 {
			t.Fatalf("%q → %d", value, got)
		}
	}
}

/* -------------------------------------------------------------------------- */
/* 端到端                                                                      */
/* -------------------------------------------------------------------------- */

func TestUsageAggregatesBothProviders(t *testing.T) {
	env := newUsageEnv(t, usageEnvOptions{withZcode: true, withMistral: true})
	env.installMistral(usageMistralSubscriptionBody, usageMistralBudgetBody, func() string {
		return `{"plan_type":"chat","plan_name":"INDIVIDUAL","primitive_access_scope":"read",` +
			`"api_base":"` + env.stub.server.URL + `/v1"}`
	}())

	res, body := getUsage(t, env.router, "?include=reset")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", res.StatusCode, body)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	providers, _ := body["providers"].([]any)
	if len(providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(providers))
	}
	if body["generated_at"] == "" {
		t.Fatal("generated_at missing")
	}
	rangeBlock, _ := body["range"].(map[string]any)
	if rangeBlock["days"] != float64(7) || rangeBlock["timezone"] != "Asia/Shanghai" {
		t.Fatalf("range = %v", rangeBlock)
	}

	// zcode：quota 归一成两条窗口，其余段进 sections。
	zcode := providerByName(t, body, usageProviderZcode)
	if zcode["available"] != true || zcode["credential"] != ZcodeChannelBigmodel {
		t.Fatalf("zcode availability = %v / %v", zcode["available"], zcode["credential"])
	}
	if zcode["key_preview"] != "****6789" {
		t.Fatalf("zcode key_preview = %q", zcode["key_preview"])
	}
	windows := windowsOf(t, zcode)
	if len(windows) != 2 {
		t.Fatalf("zcode windows = %d, want 2", len(windows))
	}
	if windows[0]["kind"] != usageWindowFiveHour || windows[0]["used_percent"] != float64(20) {
		t.Fatalf("five-hour window = %v", windows[0])
	}
	if windows[0]["reset_at"] != float64(1762300000000) {
		t.Fatalf("reset_at = %v", windows[0]["reset_at"])
	}
	if windows[1]["kind"] != usageWindowWeekly || windows[1]["remaining"] != float64(400) {
		t.Fatalf("weekly window = %v", windows[1])
	}
	sections, _ := zcode["sections"].(map[string]any)
	if len(sections) != 7 {
		t.Fatalf("sections = %d, want 7 (quota excluded)", len(sections))
	}
	if _, ok := sections["quota"]; ok {
		t.Fatal("quota duplicated into sections")
	}
	plan, _ := zcode["plan"].(map[string]any)
	if plan["name"] != "GLM Coding Lite" || plan["status"] != "VALID" || plan["billing_cycle"] != "annually" {
		t.Fatalf("zcode plan = %v", plan)
	}
	if plan["auto_renew"] != false {
		t.Fatalf("auto_renew = %v, want false", plan["auto_renew"])
	}
	reset, _ := zcode["reset"].(map[string]any)
	if reset["five_hour_resets"] != float64(3) || reset["week_resets"] != float64(1) {
		t.Fatalf("reset inventory = %v", reset)
	}
	if reset["latest_five_hour_used_at"] != float64(111) || reset["has_unread_history"] != true {
		t.Fatalf("reset history = %v", reset)
	}
	if failures := failuresOf(t, zcode); len(failures) != 0 {
		t.Fatalf("zcode failures = %v", failures)
	}

	// mistral：一条月度额度窗口 + 套餐徽标。
	mistral := providerByName(t, body, usageProviderMistral)
	if mistral["available"] != true || mistral["key_preview"] != "****6789" {
		t.Fatalf("mistral availability = %v / %v", mistral["available"], mistral["key_preview"])
	}
	mistralWindows := windowsOf(t, mistral)
	if len(mistralWindows) != 1 {
		t.Fatalf("mistral windows = %d", len(mistralWindows))
	}
	window := mistralWindows[0]
	if window["kind"] != usageWindowMonthlyQuota || window["used_percent"] != 42.5 {
		t.Fatalf("mistral window = %v", window)
	}
	if window["limit"] != float64(100) || window["used"] != 42.5 || window["remaining"] != 57.5 {
		t.Fatalf("mistral window values = %v", window)
	}
	if window["reset_at"] != float64(1796083200000) {
		t.Fatalf("mistral reset_at = %v", window["reset_at"])
	}
	if window["payg_enabled"] != false {
		t.Fatalf("mistral payg = %v", window["payg_enabled"])
	}
	mistralPlan, _ := mistral["plan"].(map[string]any)
	if mistralPlan["kind"] != "chat" || mistralPlan["title"] != "Pro" || mistralPlan["access_scope"] != "read" {
		t.Fatalf("mistral plan = %v", mistralPlan)
	}
	if failures := failuresOf(t, mistral); len(failures) != 0 {
		t.Fatalf("mistral failures = %v", failures)
	}

	// 凭据传递：billing 与 console 面都带 Bearer。
	for _, path := range []string{"/v1/billing/subscription", "/console/api/vibe/whoami"} {
		header := env.stub.headerFor(path)
		if got := header.Get("Authorization"); got != "Bearer "+usageMistralKey {
			t.Fatalf("%s Authorization = %q", path, got)
		}
	}
}

func TestUsageResetInventoryOnlyOnInclude(t *testing.T) {
	env := newUsageEnv(t, usageEnvOptions{withZcode: true})
	if _, body := getUsage(t, env.router, ""); func() bool {
		zcode := providerByName(t, body, usageProviderZcode)
		_, ok := zcode["reset"]
		return ok
	}() {
		t.Fatal("reset inventory returned without ?include=reset")
	}
	if hits := env.stub.hitCount(usageResetPath); hits != 0 {
		t.Fatalf("reset endpoint hit %d times without include", hits)
	}
	_, body := getUsage(t, env.router, "?include=reset")
	zcode := providerByName(t, body, usageProviderZcode)
	if _, ok := zcode["reset"]; !ok {
		t.Fatal("reset inventory missing with ?include=reset")
	}
}

func TestUsageCachesPerProviderAndCredential(t *testing.T) {
	env := newUsageEnv(t, usageEnvOptions{withZcode: true, withMistral: true})
	env.installMistral(usageMistralSubscriptionBody, usageMistralBudgetBody, env.mistralWhoAmI())

	if _, body := getUsage(t, env.router, ""); body["error"] != nil {
		t.Fatalf("first call failed: %v", body["error"])
	}
	first := env.stub.hitCount("/v1/billing/subscription")
	if first != 1 {
		t.Fatalf("billing hits = %d, want 1", first)
	}
	// 第二次：billing 走 5 分钟缓存，quota 走 60 秒缓存，都不该再打上游。
	_, body := getUsage(t, env.router, "")
	if body["error"] != nil {
		t.Fatalf("second call failed: %v", body["error"])
	}
	if got := env.stub.hitCount("/v1/billing/subscription"); got != 1 {
		t.Fatalf("billing cached hits = %d, want 1", got)
	}
	if got := env.stub.hitCount("/api/monitor/usage/quota/limit"); got != 1 {
		t.Fatalf("quota cached hits = %d, want 1", got)
	}

	// 换 key：指纹变化必须重新拉取，旧账号的数字不能冒充新账号。
	t.Setenv("MISTRAL_API_KEY", usageMistralKey+"-rotated")
	_, body = getUsage(t, env.router, "")
	if body["error"] != nil {
		t.Fatalf("rotated call failed: %v", body["error"])
	}
	if got := env.stub.hitCount("/v1/billing/subscription"); got != 2 {
		t.Fatalf("billing hits after rotation = %d, want 2", got)
	}
}

func TestUsageMissingCredentialsIs503(t *testing.T) {
	env := newUsageEnv(t, usageEnvOptions{})
	// 没配置任何凭据时默认请求返回 200 + 空 providers，而不是 404。
	res, body := getUsage(t, env.router, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("default status = %d, body = %v", res.StatusCode, body)
	}
	if providers, _ := body["providers"].([]any); len(providers) != 0 {
		t.Fatalf("providers = %v, want empty", providers)
	}

	// 显式点名未配置凭据的 provider：503。
	res, body = getUsage(t, env.router, "?providers=zcode,mistral-vibe")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %v", res.StatusCode, body)
	}
	errBody, _ := body["error"].(map[string]any)
	if errBody["code"] != "usage_credential_missing" {
		t.Fatalf("error = %v", errBody)
	}
}

func TestUsageRejectedCredentialIs401(t *testing.T) {
	env := newUsageEnv(t, usageEnvOptions{withMistral: true})
	env.stub.set("/v1/billing/subscription", statusHandler(http.StatusUnauthorized, `{}`))
	env.stub.set("/v1/billing/budget", statusHandler(http.StatusUnauthorized, `{}`))
	env.stub.set("/console/api/vibe/whoami", statusHandler(http.StatusUnauthorized, `{}`))

	res, body := getUsage(t, env.router, "?providers=mistral-vibe")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %v", res.StatusCode, body)
	}
	errBody, _ := body["error"].(map[string]any)
	if errBody["code"] != "usage_credentials_rejected" {
		t.Fatalf("error = %v", errBody)
	}
}

func TestUsageZcodeAuthEnvelopeIs401(t *testing.T) {
	env := newUsageEnv(t, usageEnvOptions{withZcode: true})
	// glm-usage 实测：鉴权失败是 HTTP 200 + code 401 信封。
	for path := range usageZcodeDefaults {
		env.stub.set(path, jsonHandler(`{"code":401,"msg":"invalid token"}`))
	}
	// 全部被拒：整体 401，错误信封里没有 providers。
	res, body := getUsage(t, env.router, "?providers=zcode")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %v", res.StatusCode, body)
	}
	if providers, _ := body["providers"].([]any); len(providers) != 0 {
		t.Fatalf("providers = %v, want none in error envelope", providers)
	}

	// 另一路有数据时（这里用 mistral），zcode 的鉴权失败只进 failures，
	// 整体仍是 200——一块坏了不能拖垮整张卡。
	env2 := newUsageEnv(t, usageEnvOptions{withZcode: true, withMistral: true})
	for path := range usageZcodeDefaults {
		env2.stub.set(path, jsonHandler(`{"code":401,"msg":"invalid token"}`))
	}
	env2.installMistral(usageMistralSubscriptionBody, usageMistralBudgetBody, env2.mistralWhoAmI())
	res, body = getUsage(t, env2.router, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("mixed status = %d, body = %v", res.StatusCode, body)
	}
	zcode := providerByName(t, body, usageProviderZcode)
	if zcode["available"] != true {
		t.Fatalf("available = %v, want true (credential exists)", zcode["available"])
	}
	if len(failuresOf(t, zcode)) == 0 {
		t.Fatal("auth failure not reported")
	}
}

func TestUsageFailOpenOnUnexpectedCapability(t *testing.T) {
	env := newUsageEnv(t, usageEnvOptions{withMistral: true})
	// 403/404 是「这个账号没有这一项」，不是故障，不上报也不报错。
	env.stub.set("/v1/billing/subscription", statusHandler(http.StatusForbidden, `{}`))
	env.stub.set("/v1/billing/budget", statusHandler(http.StatusNotFound, `{}`))
	env.stub.set("/console/api/vibe/whoami", statusHandler(http.StatusForbidden, `{}`))

	res, body := getUsage(t, env.router, "?providers=mistral-vibe")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", res.StatusCode, body)
	}
	mistral := providerByName(t, body, usageProviderMistral)
	if failures := failuresOf(t, mistral); len(failures) != 0 {
		t.Fatalf("failures = %v, want none", failures)
	}
	if windows := windowsOf(t, mistral); len(windows) != 0 {
		t.Fatalf("windows = %v, want none", windows)
	}
}

func TestUsageIsolatesProviderFailures(t *testing.T) {
	env := newUsageEnv(t, usageEnvOptions{withZcode: true, withMistral: true})
	env.installMistral(usageMistralSubscriptionBody, usageMistralBudgetBody, env.mistralWhoAmI())
	// quota 段 500：zcode 报 failure，Mistral 段照常。
	env.stub.set("/api/monitor/usage/quota/limit", statusHandler(http.StatusInternalServerError, `{}`))

	res, body := getUsage(t, env.router, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", res.StatusCode, body)
	}
	zcode := providerByName(t, body, usageProviderZcode)
	if windows := windowsOf(t, zcode); len(windows) != 0 {
		t.Fatalf("zcode windows = %v, want none", windows)
	}
	if failures := failuresOf(t, zcode); len(failures) == 0 {
		t.Fatal("zcode quota failure not reported")
	}
	// 其余 zcode 段仍然渲染。
	if sections, _ := zcode["sections"].(map[string]any); len(sections) == 0 {
		t.Fatal("zcode sections empty despite only quota failing")
	}
	mistral := providerByName(t, body, usageProviderMistral)
	if windows := windowsOf(t, mistral); len(windows) != 1 {
		t.Fatalf("mistral windows = %v, want 1", windows)
	}
}

func TestUsageDisabledProviderReportsNothing(t *testing.T) {
	env := newUsageEnv(t, usageEnvOptions{
		withZcode:   true,
		withMistral: true,
		usage:       &UsageConfig{Disabled: []string{usageProviderMistral}},
	})
	env.installMistral(usageMistralSubscriptionBody, usageMistralBudgetBody, env.mistralWhoAmI())

	res, body := getUsage(t, env.router, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", res.StatusCode, body)
	}
	mistral := providerByName(t, body, usageProviderMistral)
	if mistral["available"] != false {
		t.Fatalf("disabled provider available = %v", mistral["available"])
	}
	if windows := windowsOf(t, mistral); len(windows) != 0 {
		t.Fatalf("disabled provider windows = %v", windows)
	}
	if got := env.stub.hitCount("/v1/billing/subscription"); got != 0 {
		t.Fatalf("disabled provider hit upstream %d times", got)
	}
}

func TestUsageRejectsBadQuery(t *testing.T) {
	env := newUsageEnv(t, usageEnvOptions{withZcode: true})
	for _, query := range []string{"?days=0", "?days=999", "?days=abc", "?providers=zhipu"} {
		res, body := getUsage(t, env.router, query)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status = %d, body = %v", query, res.StatusCode, body)
		}
	}
}

func TestUsageTenantDomainMismatchIsReported(t *testing.T) {
	env := newUsageEnv(t, usageEnvOptions{withMistral: true})
	// whoami 广告的是租户域，与请求域不一致：必须提示改配置，
	// 而不是静默显示空额度。
	env.installMistral(usageMistralSubscriptionBody, usageMistralBudgetBody,
		`{"plan_type":"chat","plan_name":"INDIVIDUAL","api_base":"https://tenant.example/v1"}`)

	_, body := getUsage(t, env.router, "?providers=mistral-vibe")
	mistral := providerByName(t, body, usageProviderMistral)
	failures := failuresOf(t, mistral)
	found := false
	for _, failure := range failures {
		if strings.Contains(failure, "tenant.example") {
			found = true
		}
	}
	if !found {
		t.Fatalf("tenant mismatch not reported: %v", failures)
	}
}

func TestUsageRequiresStatsAuth(t *testing.T) {
	t.Setenv("NANOROUTER_USAGE_TEST_KEY", "secret-key")
	env := newUsageEnv(t, usageEnvOptions{
		withZcode: true,
		configure: func(cfg *Config) {
			cfg.AuthEnv = "NANOROUTER_USAGE_TEST_KEY"
		},
	})
	handler := env.router.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/usage", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, want 200", rec.Code)
	}
}

func TestUsageStatsPublicExemptsUsage(t *testing.T) {
	t.Setenv("NANOROUTER_USAGE_TEST_KEY", "secret-key")
	env := newUsageEnv(t, usageEnvOptions{
		withZcode: true,
		configure: func(cfg *Config) {
			cfg.AuthEnv = "NANOROUTER_USAGE_TEST_KEY"
			cfg.StatsPublic = true
		},
	})
	rec := httptest.NewRecorder()
	env.router.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/usage", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("stats_public status = %d, want 200", rec.Code)
	}
	// 生成端点不受豁免影响。
	rec = httptest.NewRecorder()
	env.router.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("models anonymous status = %d, want 401", rec.Code)
	}
}

/* -------------------------------------------------------------------------- */
/* 缓存单元                                                                    */
/* -------------------------------------------------------------------------- */

func TestUsageReaderCacheStaleAndThrottle(t *testing.T) {
	cache := newUsageReaderCache()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }
	calls := 0
	read := func(context.Context) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"ok":1}`), nil
	}

	if _, err := cache.get(context.Background(), "k", time.Minute, read); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	// 新鲜期内不再读取。
	now = now.Add(30 * time.Second)
	if _, err := cache.get(context.Background(), "k", time.Minute, read); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("fresh calls = %d, want 1", calls)
	}
	// 过期后同步刷新。
	now = now.Add(40 * time.Second)
	if _, err := cache.get(context.Background(), "k", time.Minute, read); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expired calls = %d, want 2", calls)
	}
}

func TestUsageReaderCacheKeepsDataOnFailure(t *testing.T) {
	cache := newUsageReaderCache()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }

	if _, err := cache.get(context.Background(), "k", time.Minute, func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":1}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	data, err := cache.get(context.Background(), "k", time.Minute, func(context.Context) (json.RawMessage, error) {
		return nil, context.DeadlineExceeded
	})
	// 降级：仍返回旧数据，同时如实标注失败。
	if err == nil {
		t.Fatal("stale degrade not reported as failure")
	}
	if string(data) != `{"ok":1}` {
		t.Fatalf("degraded data = %q", string(data))
	}

	// 过了 stale 窗，失败不再降级。
	now = now.Add(mistralUsageStaleTTL)
	data, err = cache.get(context.Background(), "k", time.Minute, func(context.Context) (json.RawMessage, error) {
		return nil, context.DeadlineExceeded
	})
	if err == nil || string(data) != "" {
		t.Fatalf("beyond stale: data=%q err=%v", string(data), err)
	}
}

func TestUsageCountsUpstreamFailures(t *testing.T) {
	env := newUsageEnv(t, usageEnvOptions{withZcode: true, withMistral: true})
	env.installMistral(usageMistralSubscriptionBody, usageMistralBudgetBody, env.mistralWhoAmI())
	env.stub.set("/api/monitor/usage/quota/limit", statusHandler(http.StatusInternalServerError, `{}`))

	if _, body := getUsage(t, env.router, ""); body["error"] != nil {
		t.Fatalf("usage failed: %v", body["error"])
	}
	// 每个 provider 一次失败最多计一次：zcode 的 quota 挂了但其他段正常，
	// 计数应为 1 而不是「按段相乘」。
	count := testCounterValue(t, env.router, "nanorouter_usage_upstream_errors_total", map[string]string{"provider": usageProviderZcode})
	if count != 1 {
		t.Fatalf("zcode failure counter = %v, want 1", count)
	}
	if count := testCounterValue(t, env.router, "nanorouter_usage_upstream_errors_total", map[string]string{"provider": usageProviderMistral}); count != 0 {
		t.Fatalf("mistral failure counter = %v, want 0", count)
	}
	// 缺凭据不是上游失败，不计入。
	missing := newUsageEnv(t, usageEnvOptions{withZcode: true})
	missing.stub.set("/api/monitor/usage/quota/limit", statusHandler(http.StatusInternalServerError, `{}`))
	if _, body := getUsage(t, missing.router, "?providers=mistral-vibe"); body["error"] == nil {
		t.Fatal("expected 503 for missing mistral credential")
	}
	if count := testCounterValue(t, missing.router, "nanorouter_usage_upstream_errors_total", map[string]string{"provider": usageProviderMistral}); count != 0 {
		t.Fatalf("missing credential counted as upstream failure: %v", count)
	}
}

// testCounterValue 从 Router 的注册表里读一条 counter 的当前值；不存在时返回 0。
func testCounterValue(t *testing.T, r *Router, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := r.registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			matched := true
			for _, label := range metric.GetLabel() {
				if want, ok := labels[label.GetName()]; ok && want != label.GetValue() {
					matched = false
				}
			}
			if matched && metric.GetCounter() != nil {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}
