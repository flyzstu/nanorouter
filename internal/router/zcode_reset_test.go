package router

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	json "github.com/goccy/go-json"
)

// zcodeResetStub 是 reset 两端点的桩上游，记录完整请求供头/体断言。
type zcodeResetStub struct {
	mu         sync.Mutex
	requests   []zcodeResetStubRequest
	status     int
	statusBody string
	use        int
	useBody    string
	server     *httptest.Server
}

type zcodeResetStubRequest struct {
	Path   string
	Header http.Header
	Body   string
}

func newZcodeResetStub(t *testing.T) *zcodeResetStub {
	t.Helper()
	stub := &zcodeResetStub{
		status:     http.StatusOK,
		statusBody: `{"code":0,"data":{"available_five_hour_resets":[{"expire_at":1893456000000},{"expire_at":1893542400000}],"available_week_resets":[{"expire_at":1896134400000}],"latest_five_hour_reset_history":{"used_at":1893000000000},"latest_week_reset_history":null,"has_unread_history":true}}`,
		use:        http.StatusOK,
		useBody:    `{"code":0,"data":{"used":true}}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc(zcodeResetBasePath+"/status", func(w http.ResponseWriter, r *http.Request) {
		stub.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stub.status)
		_, _ = w.Write([]byte(stub.statusBody))
	})
	mux.HandleFunc(zcodeResetBasePath+"/use", func(w http.ResponseWriter, r *http.Request) {
		stub.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stub.use)
		_, _ = w.Write([]byte(stub.useBody))
	})
	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *zcodeResetStub) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, zcodeResetStubRequest{Path: r.URL.Path, Header: r.Header.Clone(), Body: string(body)})
}

func (s *zcodeResetStub) hits() []zcodeResetStubRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]zcodeResetStubRequest(nil), s.requests...)
}

func (s *zcodeResetStub) setStatus(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	s.statusBody = body
}

func (s *zcodeResetStub) setUse(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.use = status
	s.useBody = body
}

// newZcodeResetRouter 构造带桩上游与 DB 双 token 的 Router。HOME 指向临时
// 目录，避免读到测试机真实的 ZCode 凭据文件。
func newZcodeResetRouter(t *testing.T, stub *zcodeResetStub, allowUse bool) *Router {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store := statsTestStore(t)
	credStore := newZcodeCredentialStore(store.db)
	if err := credStore.save(context.Background(), ZcodeCredential{
		Channel:     ZcodeChannelBigmodel,
		APIKey:      "db-test-key",
		PlatformJWT: "db-zcode-jwt",
		OAuthToken:  "db-maas-token",
		UserName:    "tester",
	}); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{
		Routes:     map[string]Route{"free": {Candidates: []Candidate{{ID: "u", BaseURL: stub.server.URL, Model: "m"}}}},
		ZcodeReset: ZcodeResetConfig{AllowUse: allowUse},
	}, store.db)
	if err != nil {
		t.Fatal(err)
	}
	r.zcodeResetShared().baseURL = stub.server.URL
	return r
}

func getZcodeResetStatus(t *testing.T, r *Router, query string) (*http.Response, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/zcode/reset/status"+query, nil)
	rec := httptest.NewRecorder()
	r.zcodeResetStatusHandler(rec, req)
	return decodeZcodeResetBody(t, rec.Result())
}

func postZcodeResetUse(t *testing.T, r *Router, body string) (*http.Response, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/zcode/reset/use", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.zcodeResetUseHandler(rec, req)
	return decodeZcodeResetBody(t, rec.Result())
}

func decodeZcodeResetBody(t *testing.T, res *http.Response) (*http.Response, map[string]any) {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return res, body
}

func TestZcodeResetStatusSendsCredentialPairAndCaches(t *testing.T) {
	stub := newZcodeResetStub(t)
	r := newZcodeResetRouter(t, stub, false)

	res, body := getZcodeResetStatus(t, r, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", res.StatusCode, body)
	}
	if body["channel"] != ZcodeChannelBigmodel {
		t.Fatalf("channel = %v", body["channel"])
	}
	if body["use_enabled"] != false {
		t.Fatalf("use_enabled = %v", body["use_enabled"])
	}
	fiveHour, _ := body["available_five_hour_resets"].([]any)
	if len(fiveHour) != 2 {
		t.Fatalf("five hour resets = %v", body["available_five_hour_resets"])
	}
	if body["has_unread_history"] != true {
		t.Fatalf("has_unread_history = %v", body["has_unread_history"])
	}

	hits := stub.hits()
	if len(hits) != 1 {
		t.Fatalf("upstream hits = %d, want 1", len(hits))
	}
	header := hits[0].Header
	if header.Get("Authorization") != "Bearer db-zcode-jwt" {
		t.Fatalf("Authorization = %q", header.Get("Authorization"))
	}
	if header.Get("X-Bigmodel-Authorization") != "db-maas-token" {
		t.Fatalf("X-Bigmodel-Authorization = %q", header.Get("X-Bigmodel-Authorization"))
	}
	if header.Get("Bigmodel-Target-Type") != "PERSONAL" {
		t.Fatalf("Bigmodel-Target-Type = %q", header.Get("Bigmodel-Target-Type"))
	}

	// 30 秒缓存窗口内重复查询不得再打上游。
	if _, again := getZcodeResetStatus(t, r, ""); again["channel"] != ZcodeChannelBigmodel {
		t.Fatalf("cached response = %v", again)
	}
	if len(stub.hits()) != 1 {
		t.Fatalf("upstream hits after cached read = %d, want 1", len(stub.hits()))
	}

	// refresh=1 绕过缓存。
	if _, refreshed := getZcodeResetStatus(t, r, "?refresh=1"); refreshed["channel"] != ZcodeChannelBigmodel {
		t.Fatalf("refreshed response = %v", refreshed)
	}
	if len(stub.hits()) != 2 {
		t.Fatalf("upstream hits after refresh = %d, want 2", len(stub.hits()))
	}
}

func TestZcodeResetStatusMissingCredential(t *testing.T) {
	stub := newZcodeResetStub(t)
	t.Setenv("HOME", t.TempDir())
	store := statsTestStore(t)
	r, err := New(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{{ID: "u", BaseURL: stub.server.URL, Model: "m"}}}}}, store.db)
	if err != nil {
		t.Fatal(err)
	}
	res, body := getZcodeResetStatus(t, r, "")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %v", res.StatusCode, body)
	}
	errBody, _ := body["error"].(map[string]any)
	if errBody["code"] != "zcode_reset_credential_missing" {
		t.Fatalf("error = %v", body)
	}
	if len(stub.hits()) != 0 {
		t.Fatalf("missing credential must not call upstream")
	}
}

func TestZcodeResetUseDisabledByDefault(t *testing.T) {
	stub := newZcodeResetStub(t)
	r := newZcodeResetRouter(t, stub, false)
	res, body := postZcodeResetUse(t, r, `{"reset_type":"FIVE_HOUR"}`)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, body = %v", res.StatusCode, body)
	}
	if len(stub.hits()) != 0 {
		t.Fatalf("disabled use must not call upstream")
	}
}

func TestZcodeResetUseForwardsKeyAndInvalidatesStatusCache(t *testing.T) {
	stub := newZcodeResetStub(t)
	r := newZcodeResetRouter(t, stub, true)

	if res, _ := getZcodeResetStatus(t, r, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	res, body := postZcodeResetUse(t, r, `{"reset_type":"week","idempotency_key":"retry-key-1"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("use status = %d, body = %v", res.StatusCode, body)
	}
	if body["used"] != true || body["reset_type"] != zcodeResetTypeWeek || body["idempotency_key"] != "retry-key-1" {
		t.Fatalf("use body = %v", body)
	}

	hits := stub.hits()
	if len(hits) != 2 {
		t.Fatalf("upstream hits = %d, want status+use", len(hits))
	}
	if hits[1].Path != zcodeResetBasePath+"/use" {
		t.Fatalf("use path = %q", hits[1].Path)
	}
	if !strings.Contains(hits[1].Body, `"idempotency_key":"retry-key-1"`) || !strings.Contains(hits[1].Body, `"reset_type":"WEEK"`) {
		t.Fatalf("use body = %s", hits[1].Body)
	}

	// 核销成功后 status 缓存必须失效：下一次查询要看到新余量。
	if res, _ := getZcodeResetStatus(t, r, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("status after use = %d", res.StatusCode)
	}
	if len(stub.hits()) != 3 {
		t.Fatalf("upstream hits after use + status = %d, want 3", len(stub.hits()))
	}
}

func TestZcodeResetUseGeneratesIdempotencyKey(t *testing.T) {
	stub := newZcodeResetStub(t)
	r := newZcodeResetRouter(t, stub, true)

	res, body := postZcodeResetUse(t, r, `{"reset_type":"FIVE_HOUR"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", res.StatusCode, body)
	}
	key, _ := body["idempotency_key"].(string)
	if len(key) != 36 || strings.Count(key, "-") != 4 {
		t.Fatalf("generated idempotency_key = %q", key)
	}
	hits := stub.hits()
	if len(hits) != 1 || !strings.Contains(hits[0].Body, key) {
		t.Fatalf("upstream body does not carry key: %v", hits)
	}
}

func TestZcodeResetUseRejectsInvalidInput(t *testing.T) {
	stub := newZcodeResetStub(t)
	r := newZcodeResetRouter(t, stub, true)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"invalid type", `{"reset_type":"MONTH"}`},
		{"long key", `{"reset_type":"WEEK","idempotency_key":"` + strings.Repeat("x", 65) + `"}`},
		{"missing type", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, body := postZcodeResetUse(t, r, tc.body)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %v", res.StatusCode, body)
			}
		})
	}
	if len(stub.hits()) != 0 {
		t.Fatalf("invalid input must not call upstream")
	}
}

func TestZcodeResetUpstreamErrorsMapToStableStatus(t *testing.T) {
	stub := newZcodeResetStub(t)
	r := newZcodeResetRouter(t, stub, false)

	stub.setStatus(http.StatusUnauthorized, "")
	res, body := getZcodeResetStatus(t, r, "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("401 mapping = %d, body = %v", res.StatusCode, body)
	}

	// 业务信封在 HTTP 403 下仍要按 code 识别，不能依赖 msg 文案。
	stub.setStatus(http.StatusForbidden, `{"code":3101,"msg":"coding plan is required"}`)
	r.zcodeResetShared().invalidate(ZcodeChannelBigmodel)
	res, body = getZcodeResetStatus(t, r, "")
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("3101 mapping = %d, body = %v", res.StatusCode, body)
	}
	message, _ := body["error"].(map[string]any)["message"].(string)
	if !strings.Contains(message, "zcode_reset_api_error:3101") {
		t.Fatalf("message = %q", message)
	}

	stub.setStatus(http.StatusTooManyRequests, `{"code":3002,"msg":"too many requests"}`)
	r.zcodeResetShared().invalidate(ZcodeChannelBigmodel)
	res, _ = getZcodeResetStatus(t, r, "")
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") != "60" {
		t.Fatalf("429 mapping status = %d retry-after = %q", res.StatusCode, res.Header.Get("Retry-After"))
	}

	// 3001（scope 不合法）映射 400，错误消息保留上游业务码。
	stub.setStatus(http.StatusBadRequest, `{"code":3001,"msg":"invalid scope"}`)
	r.zcodeResetShared().invalidate(ZcodeChannelBigmodel)
	res, body = getZcodeResetStatus(t, r, "")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("3001 mapping = %d, body = %v", res.StatusCode, body)
	}

	// data 形状异常是协议错误，不能当成空余量。
	stub.setStatus(http.StatusOK, `{"code":0,"data":"not-an-object"}`)
	r.zcodeResetShared().invalidate(ZcodeChannelBigmodel)
	res, body = getZcodeResetStatus(t, r, "")
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("protocol error mapping = %d, body = %v", res.StatusCode, body)
	}

	// 上游明确回 used=false 时不能谎报核销成功。
	stub.setUse(http.StatusOK, `{"code":0,"data":{"used":false}}`)
	useRouter := newZcodeResetRouter(t, stub, true)
	useRes, useBody := postZcodeResetUse(t, useRouter, `{"reset_type":"FIVE_HOUR"}`)
	if useRes.StatusCode != http.StatusBadGateway {
		t.Fatalf("used=false mapping = %d, body = %v", useRes.StatusCode, useBody)
	}
}

func TestParseZcodeResetTokenDocumentShapes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	grouped := map[string]any{
		"bigmodel": map[string]any{
			"zcode_jwt_token":    "jwt-grouped",
			"oauth_access_token": "maas-grouped",
		},
	}
	if set := parseZcodeResetTokenDocument(grouped, "bigmodel"); set.ZcodeJWT != "jwt-grouped" || set.BusinessJWT != "maas-grouped" {
		t.Fatalf("grouped = %+v", set)
	}

	flat := map[string]any{
		"provider":           "zai",
		"zcode_jwt_token":    "jwt-flat",
		"oauth_access_token": "maas-flat",
	}
	if set := parseZcodeResetTokenDocument(flat, "zai"); set.ZcodeJWT != "jwt-flat" || set.BusinessJWT != "maas-flat" {
		t.Fatalf("flat = %+v", set)
	}

	v2 := map[string]any{
		"zcodejwttoken":               "jwt-v2",
		"oauth:zai:access_token":      "maas-v2-zai",
		"oauth:bigmodel:access_token": "maas-v2-bigmodel",
	}
	if set := parseZcodeResetTokenDocument(v2, "zai"); set.ZcodeJWT != "jwt-v2" || set.BusinessJWT != "maas-v2-zai" {
		t.Fatalf("v2 zai = %+v", set)
	}
	if set := parseZcodeResetTokenDocument(v2, "bigmodel"); set.BusinessJWT != "maas-v2-bigmodel" {
		t.Fatalf("v2 bigmodel = %+v", set)
	}

	// 业务 token 禁止跨 family 回退：只有 zai 的 MaaS token 时，bigmodel 必须为空。
	if set := parseZcodeResetTokenDocument(v2, "bigmodel"); set.BusinessJWT == "maas-v2-zai" {
		t.Fatalf("cross-family fallback leaked: %+v", set)
	}
	zaiOnly := map[string]any{"oauth:zai:access_token": "maas-zai-only"}
	if set := parseZcodeResetTokenDocument(zaiOnly, "bigmodel"); set.BusinessJWT != "" {
		t.Fatalf("cross-family fallback = %+v", set)
	}

	// 平台 JWT 属于账号，允许从另一渠道分组回退。
	jwtElsewhere := map[string]any{
		"zai": map[string]any{"zcode_jwt_token": "shared-jwt"},
	}
	if set := parseZcodeResetTokenDocument(jwtElsewhere, "bigmodel"); set.ZcodeJWT != "shared-jwt" {
		t.Fatalf("jwt fallback = %+v", set)
	}

	encrypted := map[string]any{
		"zcodejwttoken":               encryptZcodeTestValue(t, "jwt-encrypted"),
		"oauth:bigmodel:access_token": encryptZcodeTestValue(t, "maas-encrypted"),
	}
	if set := parseZcodeResetTokenDocument(encrypted, "bigmodel"); set.ZcodeJWT != "jwt-encrypted" || set.BusinessJWT != "maas-encrypted" {
		t.Fatalf("encrypted = %+v", set)
	}
}

func TestZcodeResetCredentialFileChainAndAutoChannel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(zcodeDataBaseDirEnv, "")

	// 只有 zai 组持有完整凭据：自动选择应跳过缺少业务 token 的 bigmodel。
	document := `{
	  "bigmodel": {"zcode_jwt_token": "shared-jwt"},
	  "zai": {"zcode_jwt_token": "shared-jwt", "oauth_access_token": "zai-maas"}
	}`
	path := filepath.Join(home, ".nanorouter", zcodeTokenStoreName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}

	r := &Router{}
	credential, err := r.zcodeResetCredential(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if credential.Channel != ZcodeChannelZai {
		t.Fatalf("channel = %q, want zai", credential.Channel)
	}
	if credential.ZcodeJWT != "Bearer shared-jwt" || credential.BusinessJWT != "zai-maas" {
		t.Fatalf("credential = %+v", credential)
	}

	// 显式渠道绝不回退：bigmodel 缺业务 token 时报缺失。
	if _, err := r.zcodeResetCredential(context.Background(), ZcodeChannelBigmodel); err == nil {
		t.Fatal("explicit bigmodel must not fall back to zai business token")
	}
}

func TestZcodeResetEndpointsAuthMatrix(t *testing.T) {
	for _, tc := range []struct {
		name         string
		statsPublic  bool
		resetPublic  bool
		anonymous    int
		withBasic    int
		anonymousUse int
	}{
		// 未开启任何豁免：匿名被网关拦下，Basic 回退可过鉴权层。
		{"default keeps reset behind auth", false, false, http.StatusUnauthorized, http.StatusServiceUnavailable, http.StatusUnauthorized},
		// 关键边界：stats_public 只豁免统计面，不得顺带放开核销。
		{"stats_public does not exempt reset", true, false, http.StatusUnauthorized, http.StatusServiceUnavailable, http.StatusUnauthorized},
		// 独立开关：网关放行，交给反代；无凭据进入 handler 时回 503；
		// 核销默认闸门回 403（未触碰上游）。
		{"zcode_reset.public exempts reset surface", false, true, http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			resetZcodeTokenCache()
			t.Cleanup(resetZcodeTokenCache)
			t.Setenv("NANOROUTER_RESET_AUTH_TEST_KEY", "secret-key")

			r, err := New(Config{
				AuthEnv:     "NANOROUTER_RESET_AUTH_TEST_KEY",
				StatsPublic: tc.statsPublic,
				ZcodeReset:  ZcodeResetConfig{Public: tc.resetPublic},
				Routes: map[string]Route{"zcode": {Candidates: []Candidate{{
					ID: "z", BaseURL: "http://127.0.0.1:1", Model: "glm-5.3", Anthropic: true,
				}}}},
			}, mustOpenMemoryDB(t))
			if err != nil {
				t.Fatal(err)
			}
			handler := r.Handler()

			// 无凭据进入 handler 时回 503；401 只可能来自鉴权层。
			req := httptest.NewRequest(http.MethodGet, "/v1/zcode/reset/status", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.anonymous {
				t.Fatalf("anonymous reset status = %d, want %d", rec.Code, tc.anonymous)
			}

			req = httptest.NewRequest(http.MethodGet, "/v1/zcode/reset/status", nil)
			req.SetBasicAuth("secret-key", "")
			rec = httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.withBasic {
				t.Fatalf("basic reset status = %d, want %d", rec.Code, tc.withBasic)
			}

			req = httptest.NewRequest(http.MethodPost, "/v1/zcode/reset/use", strings.NewReader(`{"reset_type":"FIVE_HOUR"}`))
			req.Header.Set("Content-Type", "application/json")
			rec = httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.anonymousUse {
				t.Fatalf("anonymous reset use = %d, want %d", rec.Code, tc.anonymousUse)
			}
		})
	}
}

func TestConfigZcodeResetOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	document := "zcode_reset:\n  allow_use: true\n  public: true\nroutes:\n  r:\n    candidates:\n      - id: u\n        base_url: http://example.test\n        model: m\n"
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.ZcodeReset.AllowUse || !c.ZcodeReset.Public {
		t.Fatalf("zcode_reset = %+v", c.ZcodeReset)
	}
}

// TestZcodeResetPublicEndToEnd 走真实 Handler：验证 zcode_reset.public 时
// 匿名请求能完成查询与核销，同时生成端点不被顺带放开。
func TestZcodeResetPublicEndToEnd(t *testing.T) {
	stub := newZcodeResetStub(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("NANOROUTER_RESET_E2E_KEY", "secret-key")

	store := statsTestStore(t)
	credStore := newZcodeCredentialStore(store.db)
	if err := credStore.save(context.Background(), ZcodeCredential{
		Channel: ZcodeChannelBigmodel, APIKey: "db-test-key",
		PlatformJWT: "db-zcode-jwt", OAuthToken: "db-maas-token",
	}); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{
		AuthEnv: "NANOROUTER_RESET_E2E_KEY",
		ZcodeReset: ZcodeResetConfig{
			AllowUse: true,
			Public:   true,
		},
		Routes: map[string]Route{"zcode": {Candidates: []Candidate{{
			ID: "z", BaseURL: stub.server.URL, Model: "glm-5.3", Anthropic: true,
		}}}},
	}, store.db)
	if err != nil {
		t.Fatal(err)
	}
	r.zcodeResetShared().baseURL = stub.server.URL
	handler := r.Handler()

	// 匿名查询：放行并真实取到上游数据。
	req := httptest.NewRequest(http.MethodGet, "/v1/zcode/reset/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var status zcodeResetStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Channel != ZcodeChannelBigmodel || len(status.AvailableFiveHourResets) != 2 {
		t.Fatalf("status = %+v", status)
	}
	if !status.UseEnabled {
		t.Fatal("use_enabled should be true")
	}

	// 匿名核销：放行并打到桩上游。
	req = httptest.NewRequest(http.MethodPost, "/v1/zcode/reset/use",
		strings.NewReader(`{"reset_type":"FIVE_HOUR","idempotency_key":"e2e-key"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous use = %d, body = %s", rec.Code, rec.Body.String())
	}
	hits := stub.hits()
	if len(hits) != 2 || hits[1].Path != zcodeResetBasePath+"/use" || !strings.Contains(hits[1].Body, "e2e-key") {
		t.Fatalf("upstream hits = %+v", hits)
	}

	// 豁免只覆盖 reset：生成端点匿名仍然 401。
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"zcode"}`))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous generation = %d, want 401", rec.Code)
	}
}

func TestZcodeResetStatusShortCircuitsWithoutContextCancel(t *testing.T) {
	stub := newZcodeResetStub(t)
	r := newZcodeResetRouter(t, stub, false)

	req := httptest.NewRequest(http.MethodGet, "/v1/zcode/reset/status", nil)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	r.zcodeResetStatusHandler(rec, req)
	// 请求 ctx 已取消，处理器仍应完成（脱离请求 ctx 抓取并缓存）。
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// encryptZcodeTestValue 按 ZCode v2 文件的格式加密一个凭据值。
func encryptZcodeTestValue(t *testing.T, value string) string {
	t.Helper()
	block, err := aes.NewCipher(deriveZcodeCipherKey(resolveZcodeCredentialSecret()))
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}
	sealed := gcm.Seal(nil, iv, []byte(value), nil)
	tag := sealed[len(sealed)-gcm.Overhead():]
	ciphertext := sealed[:len(sealed)-gcm.Overhead()]
	return zcodeEncryptedValuePrefix +
		base64.RawURLEncoding.EncodeToString(iv) + "." +
		base64.RawURLEncoding.EncodeToString(tag) + "." +
		base64.RawURLEncoding.EncodeToString(ciphertext)
}
