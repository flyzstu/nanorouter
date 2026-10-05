package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newAuthHeaderTestRouter 构造开了 auth_env 的最小 Router。候选指向不可达
// 地址：认证层测试不应触达真实上游，认证通过与否在进入代理前就见分晓。
func newAuthHeaderTestRouter(t *testing.T) *Router {
	t.Helper()
	t.Setenv("NANOROUTER_AUTH_HEADER_TEST_KEY", "secret-key")
	r, err := New(Config{
		AuthEnv: "NANOROUTER_AUTH_HEADER_TEST_KEY",
		Routes: map[string]Route{"zcode": {Candidates: []Candidate{{
			ID: "z", BaseURL: "http://127.0.0.1:1", Model: "glm-5.3", Anthropic: true,
		}}}},
	}, mustOpenMemoryDB(t))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAuthAcceptsBearerAndXAPIKey(t *testing.T) {
	handler := newAuthHeaderTestRouter(t).Handler()
	cases := []struct {
		name   string
		header func(*http.Request)
		status int
	}{
		{"bearer", func(q *http.Request) { q.Header.Set("Authorization", "Bearer secret-key") }, http.StatusOK},
		{"x-api-key", func(q *http.Request) { q.Header.Set("X-Api-Key", "secret-key") }, http.StatusOK},
		{"wrong bearer", func(q *http.Request) { q.Header.Set("Authorization", "Bearer nope") }, http.StatusUnauthorized},
		{"wrong x-api-key", func(q *http.Request) { q.Header.Set("X-Api-Key", "nope") }, http.StatusUnauthorized},
		{"anonymous", func(*http.Request) {}, http.StatusUnauthorized},
		{"bearer prefix only", func(q *http.Request) { q.Header.Set("Authorization", "Bearer") }, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		q := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		tc.header(q)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, q)
		if rec.Code != tc.status {
			t.Fatalf("%s: status = %d, want %d", tc.name, rec.Code, tc.status)
		}
	}
	// 并存语义：一头错一头对，对的那个生效。
	q := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	q.Header.Set("Authorization", "Bearer secret-key")
	q.Header.Set("X-Api-Key", "wrong")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, q)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct bearer should win over wrong x-api-key: %d", rec.Code)
	}
	q = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	q.Header.Set("Authorization", "Bearer wrong")
	q.Header.Set("X-Api-Key", "secret-key")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, q)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct x-api-key should pass when bearer wrong: %d", rec.Code)
	}
}

func TestAuthXAPIKeyOnStatsPaths(t *testing.T) {
	handler := newAuthHeaderTestRouter(t).Handler()
	// 统计页与数据端点同样接受 x-api-key；/stats 是内嵌页面，200 即通过。
	for _, path := range []string{"/stats", "/v1/stats/summary"} {
		q := httptest.NewRequest(http.MethodGet, path, nil)
		q.Header.Set("X-Api-Key", "secret-key")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, q)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, rec.Code)
		}
	}
	// Basic 回退不回归：key 在密码位仍通过。
	q := httptest.NewRequest(http.MethodGet, "/stats", nil)
	q.SetBasicAuth("browser", "secret-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, q)
	if rec.Code != http.StatusOK {
		t.Fatalf("basic fallback regressed: %d", rec.Code)
	}
}

func TestAuthErrorDialectByPath(t *testing.T) {
	handler := newAuthHeaderTestRouter(t).Handler()
	// Messages 端点保持 Anthropic 信封——换成 x-api-key 的 Anthropic 客户端
	// 见到的错误形状不能变。
	q := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	q.Header.Set("X-Api-Key", "wrong")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, q)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("messages status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "authentication_error") {
		t.Fatalf("messages body = %s", rec.Body.String())
	}
	// 其余生成端点仍是 OpenAI 信封。
	q = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, q)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("chat completions: %d %s", rec.Code, rec.Body.String())
	}
}
