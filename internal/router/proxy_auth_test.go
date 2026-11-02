package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// newProxyAuthTestRouter 构造开启 auth_env 与 proxy_auth 的最小 Router。
// 候选指向不可达地址：认证层测试不触达真实上游。
func newProxyAuthTestRouter(t *testing.T) *Router {
	t.Helper()
	t.Setenv("NANOROUTER_PROXY_AUTH_TEST_KEY", "secret-key")
	r, err := New(Config{
		AuthEnv:   "NANOROUTER_PROXY_AUTH_TEST_KEY",
		ProxyAuth: ProxyAuthConfig{Header: "Remote-User"},
		Routes: map[string]Route{"zcode": {Candidates: []Candidate{{
			ID: "z", BaseURL: "http://127.0.0.1:1", Model: "glm-5.3", Anthropic: true,
		}}}},
	}, mustOpenMemoryDB(t))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestProxyAuthHeaderExemptsBrowserSurface(t *testing.T) {
	handler := newProxyAuthTestRouter(t).Handler()
	// SSO 用户（边缘身份头命中）免 key 放行整个浏览器面。
	for _, path := range []string{"/stats", "/v1/stats/summary", "/v1/zcode/usage", "/v1/zcode/reset/status", "/v1/api-keys"} {
		q := httptest.NewRequest(http.MethodGet, path, nil)
		q.Header.Set("Remote-User", "alice")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, q)
		if rec.Code == http.StatusUnauthorized {
			t.Fatalf("%s: edge identity should bypass auth, got 401 (challenge %q)", path, rec.Header().Get("WWW-Authenticate"))
		}
		if rec.Header().Get("WWW-Authenticate") != "" {
			t.Fatalf("%s: must not send Basic challenge to authenticated SSO user", path)
		}
	}
	// 无身份头时行为不变：401 + Basic challenge。
	q := httptest.NewRequest(http.MethodGet, "/v1/zcode/reset/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, q)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("no header: status = %d, challenge = %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	// 头名大小写不敏感（Go Header.Get 按 canonical key 取值）。
	q = httptest.NewRequest(http.MethodGet, "/stats", nil)
	q.Header.Set("rEmOtE-uSeR", "alice")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, q)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("case-insensitive header: got %d", rec.Code)
	}
	// 配置头名不同则不生效：客户端可注入的头不能伪造边缘身份。
	q = httptest.NewRequest(http.MethodGet, "/stats", nil)
	q.Header.Set("X-Forwarded-User", "alice")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, q)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("wrong header name: status = %d, challenge = %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

func TestProxyAuthHeaderNeverExemptsSensitiveSurface(t *testing.T) {
	handler := newProxyAuthTestRouter(t).Handler()
	// 生成端点：边缘身份头不足以放行，仍要求 key（401，无 Basic challenge）。
	q := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	q.Header.Set("Remote-User", "alice")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, q)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("generation with edge identity = %d, want 401", rec.Code)
	}
	// 核销端点：即使 zcode_reset.allow_use 关闭，401 也不能变成放行。
	q = httptest.NewRequest(http.MethodPost, "/v1/zcode/reset/use", nil)
	q.Header.Set("Remote-User", "alice")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, q)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("reset use with edge identity = %d, want 401", rec.Code)
	}
	// /metrics 含调用量，不放行。
	q = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	q.Header.Set("Remote-User", "alice")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, q)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("metrics with edge identity = %d, want 401", rec.Code)
	}
}

func TestProxyAuthDisabledByDefault(t *testing.T) {
	// 不配置 proxy_auth 时，即使请求带上 Remote-User 也不放行。
	t.Setenv("NANOROUTER_PROXY_AUTH_TEST_KEY", "secret-key")
	r, err := New(Config{
		AuthEnv: "NANOROUTER_PROXY_AUTH_TEST_KEY",
		Routes: map[string]Route{"zcode": {Candidates: []Candidate{{
			ID: "z", BaseURL: "http://127.0.0.1:1", Model: "glm-5.3", Anthropic: true,
		}}}},
	}, mustOpenMemoryDB(t))
	if err != nil {
		t.Fatal(err)
	}
	handler := r.Handler()
	q := httptest.NewRequest(http.MethodGet, "/stats", nil)
	q.Header.Set("Remote-User", "alice")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, q)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("feature off: status = %d, challenge = %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}
