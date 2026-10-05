package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// zcodeUsageStub 是四个监控端点的桩上游。handlers 可按路径覆盖默认响应；
// seen 记录每次请求的路径与查询串供断言。
type zcodeUsageStub struct {
	server   *httptest.Server
	mu       chan struct{}
	requests []string
}

func newZcodeUsageStub(t *testing.T, respond map[string]string) *zcodeUsageStub {
	t.Helper()
	stub := &zcodeUsageStub{mu: make(chan struct{}, 1)}
	defaults := map[string]string{
		"/api/monitor/usage/quota/limit":           `{"code":200,"data":{"limits":[{"unit":3,"usage":100,"currentValue":20,"remaining":80}]}}`,
		"/api/monitor/credit-usage/usage-detail":   `{"code":200,"data":{"summary":{"totalCredits":{"value":12.5}},"modelSummaryList":[{"modelName":"GLM-5.3","totalCredits":9,"totalTokens":1234}]}}`,
		"/api/monitor/credit-usage/activity":       `{"code":200,"data":{"summary":{"totalTokens":9000},"series":[{"date":"2026-10-01","totalCredits":3,"totalTokens":3000}]}}`,
		"/api/monitor/usage/model-performance-day": `{"code":200,"data":{"x_time":["2026-10-01"],"liteDecodeSpeed":[55.5],"proMaxDecodeSpeed":[70.1]}}`,
	}
	mux := http.NewServeMux()
	for _, path := range []string{"/api/monitor/usage/quota/limit", "/api/monitor/credit-usage/usage-detail", "/api/monitor/credit-usage/activity", "/api/monitor/usage/model-performance-day"} {
		body, ok := respond[path]
		if !ok {
			body = defaults[path]
		}
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			stub.mu <- struct{}{}
			stub.requests = append(stub.requests, r.URL.Path+"?"+r.URL.RawQuery+"|auth="+r.Header.Get("Authorization"))
			<-stub.mu
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
	}
	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *zcodeUsageStub) hits(t *testing.T) []string {
	t.Helper()
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	return append([]string(nil), s.requests...)
}

// newZcodeUsageRouter 构造挂好桩上游与 DB 凭据的 Router。凭据写库（bigmodel
// 渠道）确保解析命中数据库而不是测试机的凭据文件。
func newZcodeUsageRouter(t *testing.T, stub *zcodeUsageStub) *Router {
	t.Helper()
	store := statsTestStore(t)
	credStore := newZcodeCredentialStore(store.db)
	if err := credStore.save(context.Background(), ZcodeCredential{
		Channel: ZcodeChannelBigmodel, APIKey: "db-test-key", UserName: "tester",
	}); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{{ID: "u", BaseURL: stub.server.URL, Model: "m"}}}}}, store.db)
	if err != nil {
		t.Fatal(err)
	}
	cache := r.zcodeUsageShared()
	cache.baseURL = stub.server.URL
	return r
}

func getZcodeUsage(t *testing.T, r *Router, query string) (*http.Response, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/zcode/usage"+query, nil)
	rec := httptest.NewRecorder()
	r.zcodeUsageHandler(rec, req)
	res := rec.Result()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return res, body
}

func TestResolveZcodeUsageWindowDefaultsAndValidation(t *testing.T) {
	// 锚在小时中段：对齐逻辑不受整点附近运行的影响。
	now := time.Date(2026, 10, 4, 15, 30, 47, 0, time.UTC)
	window, err := resolveZcodeUsageWindow(now, "")
	if err != nil {
		t.Fatal(err)
	}
	if window.days != 7 {
		t.Fatalf("days = %d, want 7", window.days)
	}
	if window.endShanghai != "2026-10-04 23:00:00" {
		t.Fatalf("end = %q, want 2026-10-04 23:00:00", window.endShanghai)
	}
	if window.startShanghai != "2026-09-27 23:00:00" {
		t.Fatalf("start = %q, want 2026-09-27 23:00:00", window.startShanghai)
	}
	if window.params()["startTime"] != window.startShanghai {
		t.Fatalf("params startTime = %q", window.params()["startTime"])
	}
	if _, err := resolveZcodeUsageWindow(now, "0"); err == nil {
		t.Fatal("days=0 should fail")
	}
	if _, err := resolveZcodeUsageWindow(now, "367"); err == nil {
		t.Fatal("days=367 should fail")
	}
	if _, err := resolveZcodeUsageWindow(now, "abc"); err == nil {
		t.Fatal("days=abc should fail")
	}
	custom, err := resolveZcodeUsageWindow(now, "30")
	if err != nil {
		t.Fatal(err)
	}
	if custom.days != 30 {
		t.Fatalf("custom days = %d, want 30", custom.days)
	}
}

func TestDecodeZcodeUsageEnvelope(t *testing.T) {
	data, err := decodeZcodeUsageEnvelope([]byte(`{"code":200,"message":"ok","data":{"k":1}}`))
	if err != nil || string(data) != `{"k":1}` {
		t.Fatalf("data=%s err=%v", data, err)
	}
	if _, err := decodeZcodeUsageEnvelope([]byte(`{"data":{"k":1}}`)); err != nil {
		t.Fatalf("missing code should pass: %v", err)
	}
	for _, code := range []string{"401", "1001"} {
		_, err := decodeZcodeUsageEnvelope([]byte(`{"code":` + code + `,"msg":"身份验证失败。"}`))
		if !isZcodeUsageAuthError(err) {
			t.Fatalf("code %s should be auth error, got %v", code, err)
		}
	}
	// 厂商对坏凭据回 200 + success:false（详见 glm-usage client.py 实测）。
	if _, err := decodeZcodeUsageEnvelope([]byte(`{"code":1000,"success":false,"msg":"身份验证失败。"}`)); err == nil || isZcodeUsageAuthError(err) {
		t.Fatalf("success=false should be plain error, got %v", err)
	}
	if _, err := decodeZcodeUsageEnvelope([]byte(`not-json`)); err == nil {
		t.Fatal("non-json should fail")
	}
	if _, err := decodeZcodeUsageEnvelope([]byte(`{"code":200}`)); err == nil {
		t.Fatal("missing data should fail")
	}
}

func TestZcodeUsageHandlerAggregatesSectionsAndCaches(t *testing.T) {
	stub := newZcodeUsageStub(t, nil)
	r := newZcodeUsageRouter(t, stub)

	res, body := getZcodeUsage(t, r, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", res.StatusCode, body)
	}
	sections, _ := body["sections"].(map[string]any)
	for _, key := range []string{"quota", "usage", "activity", "performance"} {
		if _, ok := sections[key]; !ok {
			t.Fatalf("sections missing %q: %v", key, body)
		}
	}
	if failures, _ := body["failures"].([]any); len(failures) != 0 {
		t.Fatalf("failures = %v", failures)
	}
	if body["credential"] != ZcodeChannelBigmodel {
		t.Fatalf("credential = %v", body["credential"])
	}
	quota, _ := sections["quota"].(map[string]any)
	limits, _ := quota["limits"].([]any)
	if len(limits) != 1 {
		t.Fatalf("quota limits = %v", quota)
	}

	requests := stub.hits(t)
	if len(requests) != 4 {
		t.Fatalf("upstream hits = %d, want 4: %v", len(requests), requests)
	}
	for _, hit := range requests {
		if strings.HasPrefix(hit, "/api/monitor/usage/quota/limit") && strings.Contains(hit, "startTime") {
			t.Fatalf("quota should not carry window params: %s", hit)
		}
		if strings.Contains(hit, "usage-detail") {
			// glm-usage 合同：usage-detail 带 type=1&usageType=MODEL，缺了上游回空体。
			if !strings.Contains(hit, "type=1") || !strings.Contains(hit, "usageType=MODEL") {
				t.Fatalf("usage-detail missing type params: %s", hit)
			}
		}
		if strings.Contains(hit, "credit-usage/activity") && !strings.Contains(hit, "type=1") {
			t.Fatalf("activity missing type=1: %s", hit)
		}
		if strings.Contains(hit, "model-performance-day") && strings.Contains(hit, "type=") {
			t.Fatalf("performance should not carry type param: %s", hit)
		}
		if strings.Contains(hit, "usage-detail") || strings.Contains(hit, "activity") || strings.Contains(hit, "model-performance-day") {
			if !strings.Contains(hit, "startTime=") || !strings.Contains(hit, "endTime=") {
				t.Fatalf("windowed endpoint missing time params: %s", hit)
			}
		}
		if !strings.Contains(hit, "auth=db-test-key") {
			t.Fatalf("authorization = %s, want db-test-key", hit)
		}
	}

	// 第二次请求全部命中缓存：不再打上游。
	if _, body := getZcodeUsage(t, r, ""); len(stub.hits(t)) != 4 {
		t.Fatalf("second request should be served from cache, hits = %v, body = %v", stub.hits(t), body)
	}
}

func TestZcodeUsageAuthFailureReturns401(t *testing.T) {
	stub := newZcodeUsageStub(t, map[string]string{
		"/api/monitor/usage/quota/limit":           `{"code":401,"msg":"身份验证失败。"}`,
		"/api/monitor/credit-usage/usage-detail":   `{"code":401,"msg":"身份验证失败。"}`,
		"/api/monitor/credit-usage/activity":       `{"code":401,"msg":"身份验证失败。"}`,
		"/api/monitor/usage/model-performance-day": `{"code":401,"msg":"身份验证失败。"}`,
	})
	r := newZcodeUsageRouter(t, stub)

	res, body := getZcodeUsage(t, r, "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body = %v", res.StatusCode, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "zcode_usage_unavailable" {
		t.Fatalf("error = %v", body)
	}
}

func TestZcodeUsagePartialFailureIsolated(t *testing.T) {
	stub := newZcodeUsageStub(t, map[string]string{
		"/api/monitor/usage/model-performance-day": `{"code":500,"msg":"boom"}`,
	})
	r := newZcodeUsageRouter(t, stub)

	res, body := getZcodeUsage(t, r, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200（单块失败不拖垮整体）, body = %v", res.StatusCode, body)
	}
	sections, _ := body["sections"].(map[string]any)
	if _, ok := sections["performance"]; ok {
		t.Fatalf("failed section should be absent: %v", sections)
	}
	for _, key := range []string{"quota", "usage", "activity"} {
		if _, ok := sections[key]; !ok {
			t.Fatalf("sections missing %q: %v", key, sections)
		}
	}
	failures, _ := body["failures"].([]any)
	if len(failures) != 1 {
		t.Fatalf("failures = %v", failures)
	}
	first, _ := failures[0].(map[string]any)
	if first["section"] != "performance" {
		t.Fatalf("failure section = %v", first)
	}
}

func TestZcodeUsageServesStaleValueAfterUpstreamFailure(t *testing.T) {
	stub := newZcodeUsageStub(t, nil)
	r := newZcodeUsageRouter(t, stub)
	cache := r.zcodeUsageShared()

	if res, _ := getZcodeUsage(t, r, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("warm-up failed: %d", res.StatusCode)
	}

	// 时间快进越过 fresh TTL（60s）但未过 stale TTL（10min），并让上游宕机：
	// 应降级返回旧值并附 failures。
	stub.server.Close()
	cachedNow := cache.now
	cache.now = func() time.Time { return cachedNow().Add(2 * time.Minute) }
	t.Cleanup(func() { cache.now = cachedNow })

	res, body := getZcodeUsage(t, r, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %v", res.StatusCode, body)
	}
	sections, _ := body["sections"].(map[string]any)
	if _, ok := sections["quota"]; !ok {
		t.Fatalf("stale quota should still be served: %v", body)
	}
	failures, _ := body["failures"].([]any)
	if len(failures) == 0 {
		t.Fatalf("degraded sections should carry failures: %v", body)
	}

	// 快进越过 stale TTL 后旧值不再服役：全部失败 → 502。
	cache.now = func() time.Time { return cachedNow().Add(20 * time.Minute) }
	res, _ = getZcodeUsage(t, r, "")
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 after stale expiry", res.StatusCode)
	}
}

func TestZcodeUsageWindowParamChangesCacheKey(t *testing.T) {
	stub := newZcodeUsageStub(t, nil)
	r := newZcodeUsageRouter(t, stub)

	if res, _ := getZcodeUsage(t, r, "?days=1"); res.StatusCode != http.StatusOK {
		t.Fatalf("days=1 failed: %d", res.StatusCode)
	}
	// 不同窗口是不同缓存键：必须重新进上游而不是复用 7 天窗的缓存。
	hits := len(stub.hits(t))
	if res, _ := getZcodeUsage(t, r, "?days=30"); res.StatusCode != http.StatusOK {
		t.Fatalf("days=30 failed: %d", res.StatusCode)
	}
	if after := len(stub.hits(t)); after != hits*2 {
		t.Fatalf("hits = %d, want %d（不同窗口不得共享缓存）", after, hits*2)
	}
	if _, body := getZcodeUsage(t, r, "?days=x"); body["error"] == nil {
		t.Fatalf("days=x should 400, body = %v", body)
	}
}

func TestZcodeUsageEndpointBehindAuth(t *testing.T) {
	t.Setenv("NANOROUTER_ZCODE_USAGE_TEST_KEY", "secret-key")
	stub := newZcodeUsageStub(t, nil)
	store := statsTestStore(t)
	credStore := newZcodeCredentialStore(store.db)
	if err := credStore.save(context.Background(), ZcodeCredential{Channel: ZcodeChannelBigmodel, APIKey: "db-test-key"}); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{AuthEnv: "NANOROUTER_ZCODE_USAGE_TEST_KEY", Routes: map[string]Route{"free": {Candidates: []Candidate{{ID: "u", BaseURL: stub.server.URL, Model: "m"}}}}}, store.db)
	if err != nil {
		t.Fatal(err)
	}
	r.zcodeUsageShared().baseURL = stub.server.URL
	handler := r.Handler()

	// 无凭据 → 401 + WWW-Authenticate（浏览器 Basic 回退）。
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/zcode/usage", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("missing WWW-Authenticate for browser fallback")
	}

	// Basic（key 放密码位）→ 过；数据照常返回。
	req := httptest.NewRequest(http.MethodGet, "/v1/zcode/usage", nil)
	req.SetBasicAuth("browser", "secret-key")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("basic status = %d, body = %s", rec.Code, rec.Body.String())
	}
}
