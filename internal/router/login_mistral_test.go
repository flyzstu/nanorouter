package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	json "github.com/goccy/go-json"
)

// resetMistralTokenCache 清空凭据文件的进程内缓存（5s TTL 会跨测试串味）。
func resetMistralTokenCache() {
	mistralTokenCache.Lock()
	defer mistralTokenCache.Unlock()
	mistralTokenCache.at = time.Time{}
	mistralTokenCache.value = ""
}

// fakeMistralConsole 模拟 console 的 sign-in / poll / exchange 三条端点。
type fakeMistralConsole struct {
	t            *testing.T
	mu           sync.Mutex
	pollBody     string // poll 响应体；为空则一直 pending
	pollStatus   int    // poll 的 HTTP 状态码（默认 200）
	exchangeReqs chan<- *mistralExchangeRecord
	signInReqs   chan<- *http.Request
	userAgents   []string // 每个入站请求的 User-Agent，按到达顺序
}

// mistralExchangeRecord 捕获 exchange 请求的头与体。
type mistralExchangeRecord struct {
	Header http.Header
	Body   []byte
}

func (f *fakeMistralConsole) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	f.userAgents = append(f.userAgents, req.Header.Get("User-Agent"))
	f.mu.Unlock()
	switch {
	case req.Method == http.MethodPost && req.URL.Path == "/api/vibe/sign-in":
		if f.signInReqs != nil {
			f.signInReqs <- req
		}
		var payload struct {
			CodeChallenge       string `json:"code_challenge"`
			CodeChallengeMethod string `json:"code_challenge_method"`
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			f.t.Errorf("sign-in body read: %v", err)
		} else if err := json.Unmarshal(body, &payload); err != nil {
			f.t.Errorf("sign-in body: %v", err)
		}
		if payload.CodeChallenge == "" || payload.CodeChallengeMethod != "S256" {
			f.t.Errorf("sign-in payload=%+v", payload)
		}
		// sign_in_url / poll_url 都落在本服务的 origin 上，通过校验。
		_, _ = w.Write([]byte(`{"process_id":"proc-1","sign_in_url":"` + consoleBase + `/sign-in?code=abc","poll_url":"` + consoleBase + `/api/vibe/sign-in/proc-1/poll","expires_at":"2026-12-31T00:00:00Z"}`))
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/poll"):
		f.mu.Lock()
		defer f.mu.Unlock()
		status := f.pollStatus
		if status == 0 {
			status = http.StatusOK
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		body := f.pollBody
		if body == "" {
			body = `{"status":"pending"}`
		}
		_, _ = w.Write([]byte(body))
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/exchange"):
		if f.exchangeReqs != nil {
			body, _ := io.ReadAll(req.Body)
			f.exchangeReqs <- &mistralExchangeRecord{Header: req.Header.Clone(), Body: body}
		}
		_, _ = w.Write([]byte(`{"api_key":"mk-test-key-123"}`))
	default:
		http.NotFound(w, req)
	}
}

// consoleBase 由测试服务器动态指定；handler 闭包引用包级变量。
var consoleBase string

func newMistralTestConsole(t *testing.T, mutate func(*fakeMistralConsole)) (*httptest.Server, *fakeMistralConsole) {
	t.Helper()
	console := &fakeMistralConsole{t: t}
	if mutate != nil {
		mutate(console)
	}
	server := httptest.NewServer(console)
	consoleBase = server.URL
	t.Cleanup(func() {
		server.Close()
		consoleBase = ""
	})
	return server, console
}

func TestRunMistralLoginHappyPath(t *testing.T) {
	exchanges := make(chan *mistralExchangeRecord, 1)
	signIns := make(chan *http.Request, 1)
	server, _ := newMistralTestConsole(t, func(c *fakeMistralConsole) {
		c.pollBody = `{"status":"completed","exchange_token":"tok-xyz"}`
		c.exchangeReqs = exchanges
		c.signInReqs = signIns
	})

	home := t.TempDir()
	t.Setenv("HOME", home)
	resetMistralTokenCache()
	t.Cleanup(resetMistralTokenCache)

	opts := MistralLoginOptions{
		BrowserBaseURL: server.URL,
		APIBaseURL:     server.URL + "/api",
		PollTimeout:    10 * time.Second,
		PollInterval:   10 * time.Millisecond,
		TokenStorePath: filepath.Join(home, ".nanorouter", mistralTokenStoreName),
		Output:         io.Discard,
	}
	path, err := RunMistralLogin(context.Background(), opts)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if path != opts.TokenStorePath {
		t.Fatalf("path=%q", path)
	}

	// sign-in 请求必须带 S256 challenge。
	select {
	case req := <-signIns:
		if req.Method != http.MethodPost || req.URL.Path != "/api/vibe/sign-in" {
			t.Fatalf("sign-in=%s %s", req.Method, req.URL.Path)
		}
	default:
		t.Fatal("sign-in request missing")
	}

	// exchange 请求必须携带 poll 返回的 exchange_token。
	select {
	case record := <-exchanges:
		var payload map[string]string
		if err := json.Unmarshal(record.Body, &payload); err != nil {
			t.Fatalf("exchange body=%s err=%v", record.Body, err)
		}
		if payload["exchange_token"] != "tok-xyz" {
			t.Fatalf("exchange_token=%q", payload["exchange_token"])
		}
		if payload["code_verifier"] == "" {
			t.Fatal("code_verifier missing")
		}
	default:
		t.Fatal("exchange request missing")
	}

	// 凭据文件内容与权限。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm=%v", perm)
	}
	var token mistralToken
	if err := json.Unmarshal(raw, &token); err != nil {
		t.Fatal(err)
	}
	if token.APIKey != "mk-test-key-123" {
		t.Fatalf("api_key=%q", token.APIKey)
	}

	// 网关回退读取：凭据文件 → key。
	if key := mistralTokenFileKey(); key != "mk-test-key-123" {
		t.Fatalf("fallback key=%q", key)
	}
}

// 官方 console 依据第一方 UA 区分 Vibe 客户端；sign-in / poll / exchange 三条请求都必须带上它。
func TestMistralLoginRequestsCarryVibeUserAgent(t *testing.T) {
	server, console := newMistralTestConsole(t, func(c *fakeMistralConsole) {
		c.pollBody = `{"status":"completed","exchange_token":"tok-xyz"}`
	})

	home := t.TempDir()
	t.Setenv("HOME", home)
	resetMistralTokenCache()
	t.Cleanup(resetMistralTokenCache)

	opts := MistralLoginOptions{
		BrowserBaseURL: server.URL,
		APIBaseURL:     server.URL + "/api",
		PollTimeout:    10 * time.Second,
		PollInterval:   10 * time.Millisecond,
		TokenStorePath: filepath.Join(home, ".nanorouter", mistralTokenStoreName),
		Output:         io.Discard,
	}
	if _, err := RunMistralLogin(context.Background(), opts); err != nil {
		t.Fatalf("login: %v", err)
	}

	console.mu.Lock()
	defer console.mu.Unlock()
	if len(console.userAgents) < 3 {
		t.Fatalf("requests=%d", len(console.userAgents))
	}
	for index, userAgent := range console.userAgents {
		if userAgent != mistralUserAgent {
			t.Fatalf("request %d user-agent=%q want %q", index, userAgent, mistralUserAgent)
		}
	}
}

func TestRunMistralLoginPollExpired(t *testing.T) {
	server, _ := newMistralTestConsole(t, func(c *fakeMistralConsole) {
		c.pollBody = `{"status":"expired"}`
	})

	home := t.TempDir()
	t.Setenv("HOME", home)
	resetMistralTokenCache()
	t.Cleanup(resetMistralTokenCache)

	opts := MistralLoginOptions{
		BrowserBaseURL: server.URL,
		APIBaseURL:     server.URL + "/api",
		PollTimeout:    10 * time.Second,
		PollInterval:   10 * time.Millisecond,
		TokenStorePath: filepath.Join(home, ".nanorouter", mistralTokenStoreName),
		Output:         io.Discard,
	}
	if _, err := RunMistralLogin(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunMistralLoginDenied(t *testing.T) {
	server, _ := newMistralTestConsole(t, func(c *fakeMistralConsole) {
		c.pollBody = `{"status":"denied"}`
	})
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetMistralTokenCache()
	t.Cleanup(resetMistralTokenCache)

	opts := MistralLoginOptions{
		BrowserBaseURL: server.URL,
		APIBaseURL:     server.URL + "/api",
		PollTimeout:    10 * time.Second,
		PollInterval:   10 * time.Millisecond,
		TokenStorePath: filepath.Join(home, ".nanorouter", mistralTokenStoreName),
		Output:         io.Discard,
	}
	if _, err := RunMistralLogin(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunMistralLoginPollGone(t *testing.T) {
	// HTTP 410 视作 expired（官方语义）。
	server, _ := newMistralTestConsole(t, func(c *fakeMistralConsole) {
		c.pollStatus = http.StatusGone
	})
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetMistralTokenCache()
	t.Cleanup(resetMistralTokenCache)

	opts := MistralLoginOptions{
		BrowserBaseURL: server.URL,
		APIBaseURL:     server.URL + "/api",
		PollTimeout:    10 * time.Second,
		PollInterval:   10 * time.Millisecond,
		TokenStorePath: filepath.Join(home, ".nanorouter", mistralTokenStoreName),
		Output:         io.Discard,
	}
	if _, err := RunMistralLogin(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunMistralLoginExchangeForbidden(t *testing.T) {
	server, _ := newMistralTestConsole(t, func(c *fakeMistralConsole) {
		c.pollBody = `{"status":"completed","exchange_token":"tok-xyz"}`
		c.pollStatus = 0
	})
	// exchange 返回 403：workspace 不符合 Vibe 条件。poll 正常、exchange 403。
	console := server.Config.Handler.(*fakeMistralConsole)
	exchangeBlocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/exchange") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		console.ServeHTTP(w, req)
	}))
	defer exchangeBlocked.Close()
	consoleBase = exchangeBlocked.URL
	defer func() { consoleBase = server.URL }()

	home := t.TempDir()
	t.Setenv("HOME", home)
	resetMistralTokenCache()
	t.Cleanup(resetMistralTokenCache)

	opts := MistralLoginOptions{
		BrowserBaseURL: exchangeBlocked.URL,
		APIBaseURL:     exchangeBlocked.URL + "/api",
		PollTimeout:    10 * time.Second,
		PollInterval:   10 * time.Millisecond,
		TokenStorePath: filepath.Join(home, ".nanorouter", mistralTokenStoreName),
		Output:         io.Discard,
	}
	_, err := RunMistralLogin(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunMistralLoginRejectsForeignOrigin(t *testing.T) {
	// 服务端返回的 sign_in_url 落在第三方 origin：必须拒绝。
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(`{"process_id":"p","sign_in_url":"https://evil.example/sign-in","poll_url":"` + req.Host + `/poll","expires_at":"2026-12-31T00:00:00Z"}`))
	}))
	defer evil.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)

	opts := MistralLoginOptions{
		BrowserBaseURL: evil.URL,
		APIBaseURL:     evil.URL + "/api",
		PollTimeout:    2 * time.Second,
		PollInterval:   10 * time.Millisecond,
		TokenStorePath: filepath.Join(home, ".nanorouter", mistralTokenStoreName),
		Output:         io.Discard,
	}
	_, err := RunMistralLogin(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err=%v", err)
	}
}

func TestMistralCandidateResolution(t *testing.T) {
	// 候选链：env 未命中 → 凭据文件命中。
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetMistralTokenCache()
	t.Cleanup(resetMistralTokenCache)

	path := filepath.Join(home, ".nanorouter", mistralTokenStoreName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	token := mistralToken{APIKey: "mk-file-key", UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	raw, err := json.Marshal(token)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	r := &Router{}
	candidate := Candidate{MistralTokenFile: true}
	if key := r.resolveCandidateAPIKey(context.Background(), candidate); key != "mk-file-key" {
		t.Fatalf("candidate key=%q", key)
	}

	// env 命中优先于凭据文件。
	t.Setenv("MISTRAL_API_KEY", "mk-env-key")
	candidate = Candidate{MistralTokenFile: true, APIKeyEnv: "MISTRAL_API_KEY"}
	if key := r.resolveCandidateAPIKey(context.Background(), candidate); key != "mk-env-key" {
		t.Fatalf("env key=%q", key)
	}
}

func TestMistralCodeChallengeIsSHA256(t *testing.T) {
	verifier := "fixed-verifier-for-test"
	challenge := mistralCodeChallenge(verifier)
	// S256：base64url(sha256(verifier))，无填充。
	if strings.Contains(challenge, "=") {
		t.Fatalf("challenge has padding: %q", challenge)
	}
	if len(challenge) != 43 {
		t.Fatalf("challenge length=%d", len(challenge))
	}
}

func TestValidateMistralTokenFileNeedsNothingSpecial(t *testing.T) {
	// mistral_token_file 无前置条件：普通 OpenAI 兼容候选即可。
	candidate := Candidate{MistralTokenFile: true, Retry: CandidateRetryConfig{}}
	if err := candidate.validate(RetryConfig{MaxRateLimitRetries: intPtr(0)}); err != nil {
		t.Fatalf("validate: %v", err)
	}
}
