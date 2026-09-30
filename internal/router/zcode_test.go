package router

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	json "github.com/goccy/go-json"
	_ "github.com/lib/pq"
	_ "github.com/ncruces/go-sqlite3/driver"
)

// newZcodeTestRouter 构造一个带单个 anthropic 候选的网关，供 /v1/messages
// 与翻译链路测试复用。
func newZcodeTestRouter(t *testing.T, candidate Candidate) (*Router, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"zcode": {Candidates: []Candidate{candidate}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	return r, db
}

// postJSON 向网关发一个 POST 并返回 recorder。
func postJSON(t *testing.T, r *Router, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	return rec
}

func TestZcodeMessagesPassthroughPinsHeaders(t *testing.T) {
	const downstreamKey = "11.secret"
	var gotSession string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/ultra/anthropic/v1/messages" {
			t.Fatalf("path=%s", req.URL.Path)
		}
		if got := req.Header.Get("User-Agent"); got != "ZCode/"+ZcodeClientVersion {
			t.Fatalf("user-agent=%q", got)
		}
		if got := req.Header.Get("anthropic-version"); got != ZcodeAnthropicVersion {
			t.Fatalf("anthropic-version=%q", got)
		}
		if got := req.Header.Get("x-api-key"); got != downstreamKey {
			t.Fatalf("x-api-key=%q", got)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer "+downstreamKey {
			t.Fatalf("authorization=%q", got)
		}
		if got := req.Header.Get("X-ZCode-App-Version"); got != ZcodeClientVersion {
			t.Fatalf("x-zcode-app-version=%q", got)
		}
		if got := req.Header.Get("X-Client-Language"); got != "zh-CN" {
			t.Fatalf("x-client-language=%q", got)
		}
		if got := req.Header.Get("x-zcode-session-type"); got != "main" {
			t.Fatalf("x-zcode-session-type=%q", got)
		}
		if got := req.Header.Get("X-Os-Category"); got == "" {
			t.Fatal("x-os-category missing")
		}
		gotSession = req.Header.Get("x-session-id")
		if !strings.HasPrefix(gotSession, "ses_") {
			t.Fatalf("x-session-id=%q", gotSession)
		}
		if !strings.HasPrefix(req.Header.Get("x-request-id"), "req_") {
			t.Fatalf("x-request-id=%q", req.Header.Get("x-request-id"))
		}
		if !strings.HasPrefix(req.Header.Get("x-zcode-trace-id"), "trace_") {
			t.Fatalf("x-zcode-trace-id=%q", req.Header.Get("x-zcode-trace-id"))
		}
		var in map[string]any
		_ = json.NewDecoder(req.Body).Decode(&in)
		if in["model"] != "glm-5.3" {
			t.Fatalf("model=%v", in["model"])
		}
		if _, ok := in["max_tokens"]; !ok {
			t.Fatal("max_tokens must stay as the client sent it")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer upstream.Close()
	r, db := newZcodeTestRouter(t, Candidate{
		ID: "zcode-cn", BaseURL: upstream.URL, Model: "glm-5.3", APIKeyEnv: "TEST_ZCODE_KEY",
		Anthropic: true, SimulateZcode: true, ZcodeChannel: ZcodeChannelBigmodel,
	})
	t.Setenv("TEST_ZCODE_KEY", downstreamKey)
	rec := postJSON(t, r, "/v1/messages", `{"model":"zcode","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"text":"hi"`) {
		t.Fatalf("passthrough body=%s", rec.Body.String())
	}
	if gotSession == "" {
		t.Fatal("session id missing")
	}
	var prompt, completion int64
	if err := db_queryTokens(t, db, &prompt, &completion); err != nil {
		t.Fatal(err)
	}
	if prompt != 3 || completion != 2 {
		t.Fatalf("usage prompt=%d completion=%d", prompt, completion)
	}
}

// db_queryTokens 读取账本中最近一次成功尝试的用量。
func db_queryTokens(t *testing.T, db *sql.DB, prompt, completion *int64) error {
	t.Helper()
	return db.QueryRow(`SELECT prompt_tokens, completion_tokens FROM llm_requests WHERE status_code BETWEEN 200 AND 299 ORDER BY id DESC LIMIT 1`).Scan(prompt, completion)
}

func TestZcodeSessionIDStableAcrossConversation(t *testing.T) {
	sessions := make([]string, 0, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sessions = append(sessions, req.Header.Get("x-session-id"))
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()
	r, _ := newZcodeTestRouter(t, Candidate{
		ID: "z", BaseURL: upstream.URL, Model: "glm-5.3", APIKeyEnv: "TEST_ZCODE_KEY",
		Anthropic: true, SimulateZcode: true,
	})
	t.Setenv("TEST_ZCODE_KEY", "k")
	// 同一会话的第二轮：首条 user 消息不变、历史变长 → 会话 id 稳定。
	first := postJSON(t, r, "/v1/messages", `{"model":"zcode","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`)
	second := postJSON(t, r, "/v1/messages", `{"model":"zcode","max_tokens":8,"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"text","text":"hi there"}]},{"role":"user","content":"more"}]}`)
	// 不同开头 → 不同会话 id。
	third := postJSON(t, r, "/v1/messages", `{"model":"zcode","max_tokens":8,"messages":[{"role":"user","content":"different"}]}`)
	if first.Code != 200 || second.Code != 200 || third.Code != 200 {
		t.Fatalf("statuses %d %d %d", first.Code, second.Code, third.Code)
	}
	if len(sessions) != 3 {
		t.Fatalf("sessions=%v", sessions)
	}
	if sessions[0] == "" || sessions[0] != sessions[1] {
		t.Fatalf("session id not stable: %v", sessions)
	}
	if sessions[2] == sessions[0] {
		t.Fatal("different conversations must not share a session id")
	}
}

func TestZcodeChannelPathMapping(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/ultra-zai/anthropic/v1/messages" {
			t.Fatalf("path=%s", req.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()
	r, _ := newZcodeTestRouter(t, Candidate{
		ID: "zcode-zai", BaseURL: upstream.URL, Model: "glm-5.3", APIKeyEnv: "TEST_ZCODE_KEY",
		Anthropic: true, SimulateZcode: true, ZcodeChannel: ZcodeChannelZai,
	})
	t.Setenv("TEST_ZCODE_KEY", "k")
	if rec := postJSON(t, r, "/v1/messages", `{"model":"zcode","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestZcodeMessagesSSEUsageRecorded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":21}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()
	r, db := newZcodeTestRouter(t, Candidate{
		ID: "z", BaseURL: upstream.URL, Model: "glm-5.3", APIKeyEnv: "TEST_ZCODE_KEY",
		Anthropic: true, SimulateZcode: true,
	})
	t.Setenv("TEST_ZCODE_KEY", "k")
	rec := postJSON(t, r, "/v1/messages", `{"model":"zcode","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// SSE 透传：下游收到的事件与上游逐字节同形。
	for _, want := range []string{"message_start", "text_delta", "message_stop"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("missing %q in %s", want, rec.Body.String())
		}
	}
	var prompt, completion int64
	if err := db.QueryRow(`SELECT prompt_tokens, completion_tokens, total_tokens FROM llm_requests`).Scan(&prompt, &completion, new(int64)); err != nil {
		t.Fatal(err)
	}
	if prompt != 10 || completion != 21 {
		t.Fatalf("usage prompt=%d completion=%d（message_start 与 message_delta 必须合并）", prompt, completion)
	}
}

func TestZcodeMessagesFailoverBeforeSSE(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer failing.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer healthy.Close()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{
		RequestTimeout: time.Second,
		Routes: map[string]Route{"zcode": {Candidates: []Candidate{
			{ID: "down", BaseURL: failing.URL, Model: "glm-5.3", APIKeyEnv: "K", Anthropic: true, SimulateZcode: true},
			{ID: "up", BaseURL: healthy.URL, Model: "glm-5.3", APIKeyEnv: "K", Anthropic: true, SimulateZcode: true},
		}}},
	}, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("K", "k")
	rec := postJSON(t, r, "/v1/messages", `{"model":"zcode","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Nanorouter-Upstream"); got != "up" {
		t.Fatalf("upstream=%q", got)
	}
}

func TestZcodeMessagesErrorPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"身份验证失败。"}}`))
	}))
	defer upstream.Close()
	// 401 属于可切换错误：聚合路由会先试其他候选；直连（expose_as）才
	// 原样回传上游诊断。这里用直连验证信封透传。
	r, _ := newZcodeTestRouter(t, Candidate{
		ID: "z", BaseURL: upstream.URL, Model: "glm-5.3", APIKeyEnv: "TEST_ZCODE_KEY",
		Anthropic: true, SimulateZcode: true, ExposeAs: "zcode-direct",
	})
	t.Setenv("TEST_ZCODE_KEY", "bad")
	rec := postJSON(t, r, "/v1/messages", `{"model":"zcode-direct","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 401 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "身份验证失败。") {
		t.Fatalf("upstream diagnostics lost: %s", rec.Body.String())
	}
}

func TestZcodeMessagesLocalErrorsUseAnthropicEnvelope(t *testing.T) {
	r, _ := newZcodeTestRouter(t, Candidate{ID: "z", BaseURL: "http://127.0.0.1:1", Model: "glm-5.3", Anthropic: true})
	rec := postJSON(t, r, "/v1/messages", `{"model":"no-such-route","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 400 {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"type":"error"`) || !strings.Contains(rec.Body.String(), `"message":"unknown model route (unknown_model_route)"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"type":"invalid_request_error"`) {
		t.Fatalf("standard error type missing: %s", rec.Body.String())
	}
}

func TestZcodeTokenFileFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetZcodeTokenCache()
	t.Cleanup(resetZcodeTokenCache)
	store := filepath.Join(home, ".nanorouter")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	document := `{"bigmodel":{"api_key":"store-key","updated_at":"2026-01-01T00:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(store, zcodeTokenStoreName), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotKey = req.Header.Get("x-api-key")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()
	r, _ := newZcodeTestRouter(t, Candidate{
		ID: "z", BaseURL: upstream.URL, Model: "glm-5.3",
		Anthropic: true, SimulateZcode: true, ZcodeChannel: ZcodeChannelBigmodel, ZcodeTokenFile: true,
	})
	rec := postJSON(t, r, "/v1/messages", `{"model":"zcode","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotKey != "store-key" {
		t.Fatalf("key=%q", gotKey)
	}
}

func TestZcodeReferenceTokenFileFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetZcodeTokenCache()
	t.Cleanup(resetZcodeTokenCache)
	// 参考客户端的分组文件：api_key 优先，平台 JWT 不作为 key。
	reference := `{"bigmodel":{"provider":"bigmodel","api_key":"ref-key","zcode_jwt_token":"eyJhbGci.refjwt","expires_at":4102444800}}`
	if err := os.WriteFile(filepath.Join(home, zcodeReferenceTokenName), []byte(reference), 0o600); err != nil {
		t.Fatal(err)
	}
	if key := zcodeTokenFileKey(ZcodeChannelBigmodel); key != "ref-key" {
		t.Fatalf("key=%q", key)
	}
	// 过期记录降级为无凭据。
	expired := `{"bigmodel":{"provider":"bigmodel","api_key":"old","expires_at":1000}}`
	if err := os.WriteFile(filepath.Join(home, zcodeReferenceTokenName), []byte(expired), 0o600); err != nil {
		t.Fatal(err)
	}
	resetZcodeTokenCache()
	if key := zcodeTokenFileKey(ZcodeChannelBigmodel); key != "" {
		t.Fatalf("expired key=%q", key)
	}
}

func TestZcodeConfigValidation(t *testing.T) {
	cases := []struct {
		name      string
		candidate Candidate
		want      string
	}{
		{"channel", Candidate{ID: "z", BaseURL: "http://x", Model: "m", Anthropic: true, ZcodeChannel: "nope"}, "zcode_channel"},
		{"zcode without anthropic", Candidate{ID: "z", BaseURL: "http://x", Model: "m", SimulateZcode: true}, "anthropic must be enabled"},
		{"responses conflict", Candidate{ID: "z", BaseURL: "http://x", Model: "m", Anthropic: true, Responses: true}, "responses: true does not apply"},
		{"opencode conflict", Candidate{ID: "z", BaseURL: "http://x", Model: "m", Anthropic: true, SimulateOpenCode: true}, "conflicts with anthropic"},
		{"chat path conflict", Candidate{ID: "z", BaseURL: "http://x", Model: "m", Anthropic: true, ChatCompletionsPath: "chat/completions"}, "chat_completions_path"},
		{"max tokens field conflict", Candidate{ID: "z", BaseURL: "http://x", Model: "m", Anthropic: true, ChatMaxTokensField: "max_new_tokens"}, "chat_max_tokens_field"},
		{"messages path without anthropic", Candidate{ID: "z", BaseURL: "http://x", Model: "m", AnthropicMessagesPath: "v1/messages"}, "anthropic must be enabled"},
		{"token file without anthropic", Candidate{ID: "z", BaseURL: "http://x", Model: "m", ZcodeTokenFile: true}, "anthropic must be enabled"},
	}
	for _, tc := range cases {
		if err := tc.candidate.validate(RetryConfig{MaxRateLimitRetries: intPtr(0)}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err=%v", tc.name, err)
		}
	}
}

func intPtr(v int) *int { return &v }

func TestMessagesEndpointSkipsNonAnthropicCandidates(t *testing.T) {
	openaiCalls := 0
	openaiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		openaiCalls++
		_, _ = w.Write([]byte(`{"id":"x","choices":[]}`))
	}))
	defer openaiUpstream.Close()
	anthropicCalls := 0
	anthropicUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		anthropicCalls++
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer anthropicUpstream.Close()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"mixed": {Candidates: []Candidate{
		{ID: "openai-side", BaseURL: openaiUpstream.URL, Model: "gpt-x"},
		{ID: "anthropic-side", BaseURL: anthropicUpstream.URL, Model: "glm-5.3", APIKeyEnv: "K", Anthropic: true},
	}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("K", "k")
	for i := 0; i < 3; i++ {
		rec := postJSON(t, r, "/v1/messages", `{"model":"mixed","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != 200 {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("X-Nanorouter-Upstream"); got != "anthropic-side" {
			t.Fatalf("upstream=%q（messages 端点不得命中 openai 候选）", got)
		}
	}
	if openaiCalls != 0 {
		t.Fatalf("openai upstream called %d times", openaiCalls)
	}
	if anthropicCalls != 3 {
		t.Fatalf("anthropic calls=%d", anthropicCalls)
	}
	// 纯 openai 路由打 messages 端点 → 503 + anthropic 信封。
	r2, err := New(Config{Routes: map[string]Route{"plain": {Candidates: []Candidate{
		{ID: "o", BaseURL: openaiUpstream.URL, Model: "gpt-x"},
	}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	rec := postJSON(t, r2, "/v1/messages", `{"model":"plain","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 503 {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestZcodeMessagesStreamFailoverBeforeSSE(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer failing.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer healthy.Close()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{RequestTimeout: time.Second, Routes: map[string]Route{"zcode": {Candidates: []Candidate{
		{ID: "down", BaseURL: failing.URL, Model: "glm-5.3", APIKeyEnv: "K", Anthropic: true},
		{ID: "up", BaseURL: healthy.URL, Model: "glm-5.3", APIKeyEnv: "K", Anthropic: true},
	}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("K", "k")
	rec := postJSON(t, r, "/v1/messages", `{"model":"zcode","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Nanorouter-Upstream"); got != "up" {
		t.Fatalf("upstream=%q", got)
	}
	if strings.Contains(rec.Body.String(), "slow down") {
		t.Fatalf("first candidate's 429 leaked: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "message_start") {
		t.Fatalf("stream missing: %s", rec.Body.String())
	}
}

func TestMessagesUsageCountsCacheTokens(t *testing.T) {
	// 翻译路径：cache_read/creation 计入 prompt 侧。
	out, err := messagesToChatCompletion([]byte(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"cache_read_input_tokens":4,"cache_creation_input_tokens":2,"output_tokens":7}}`), "zcode")
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatal(err)
	}
	usage, _ := response["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(16) || usage["total_tokens"] != float64(23) {
		t.Fatalf("usage=%v", usage)
	}
	// 透传路径：findUsage 同一口径（仅当 prompt_tokens 缺失时启用）。
	streamed := `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":4,"cache_creation_input_tokens":2}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(streamed))
	}))
	defer upstream.Close()
	r, db := newZcodeTestRouter(t, Candidate{
		ID: "z", BaseURL: upstream.URL, Model: "glm-5.3", APIKeyEnv: "TEST_ZCODE_KEY",
		Anthropic: true, SimulateZcode: true,
	})
	t.Setenv("TEST_ZCODE_KEY", "k")
	rec := postJSON(t, r, "/v1/messages", `{"model":"zcode","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var prompt, completion int64
	if err := db.QueryRow(`SELECT prompt_tokens, completion_tokens FROM llm_requests`).Scan(&prompt, &completion); err != nil {
		t.Fatal(err)
	}
	if prompt != 16 || completion != 3 {
		t.Fatalf("stream usage prompt=%d completion=%d", prompt, completion)
	}
}

func TestMessagesLocalErrorDialects(t *testing.T) {
	r, _ := newZcodeTestRouter(t, Candidate{ID: "z", BaseURL: "http://127.0.0.1:1", Model: "glm-5.3", Anthropic: true})
	// 坏 JSON → invalid_request_error + 原始码尾注。
	rec := postJSON(t, r, "/v1/messages", `{"model":"zcode",`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"type":"invalid_request_error"`) || !strings.Contains(rec.Body.String(), "invalid_json") {
		t.Fatalf("bad json: %d %s", rec.Code, rec.Body.String())
	}
	// 未授权 → authentication_error 信封（auth 中间件按路径选方言）。
	r2, err := New(Config{
		AuthEnv: "TEST_GW_KEY",
		Routes:  map[string]Route{"zcode": {Candidates: []Candidate{{ID: "z", BaseURL: "http://127.0.0.1:1", Model: "glm-5.3", Anthropic: true}}}},
	}, mustOpenMemoryDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_GW_KEY", "sekrit")
	rec = postJSON(t, r2, "/v1/messages", `{"model":"zcode","messages":[]}`)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), `"type":"authentication_error"`) {
		t.Fatalf("auth: %d %s", rec.Code, rec.Body.String())
	}
	// 无 /v1 别名可用。
	rec = postJSON(t, r, "/messages", `{"model":"nope","messages":[]}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Fatalf("alias: %d %s", rec.Code, rec.Body.String())
	}
}

func TestChatEndpointAnthropicStreamErrorEventFails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n"))
		_, _ = w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"overloaded\"}}\n\n"))
	}))
	defer upstream.Close()
	r, db := newZcodeTestRouter(t, Candidate{ID: "z", BaseURL: upstream.URL, Model: "glm-5.3", Anthropic: true})
	rec := postJSON(t, r, "/v1/chat/completions", `{"model":"zcode","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "overloaded") {
		t.Fatalf("stream error missing: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("failed stream must not emit [DONE]: %s", rec.Body.String())
	}
	var kind string
	if err := db.QueryRow(`SELECT error_kind FROM llm_requests`).Scan(&kind); err != nil || kind != "stream" {
		t.Fatalf("error_kind=%q err=%v", kind, err)
	}
}

func TestZcodeConversationSeedAndIDs(t *testing.T) {
	// '{}' 种子与空种子都走 fallback：两次派生的会话 id 必然不同。
	first := deriveZcodeRequestIDs([]any{map[string]any{"role": "user", "content": map[string]any{}}})
	second := deriveZcodeRequestIDs([]any{map[string]any{"role": "user", "content": map[string]any{}}})
	if first.session == second.session {
		t.Fatalf("'{}' seed must fall back to per-request random, got stable %q", first.session)
	}
	// 无 user 消息 → fallback。
	orphan := deriveZcodeRequestIDs([]any{map[string]any{"role": "assistant", "content": "hi"}})
	if orphan.session == "" {
		t.Fatal("orphan conversation must still derive an id")
	}
	// seed 只取首个 user turn：其后追加历史不影响派生。
	base := []any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "hi"},
	}
	grown := append(append([]any{}, base...), map[string]any{"role": "user", "content": "more"})
	if deriveZcodeRequestIDs(base).session != deriveZcodeRequestIDs(grown).session {
		t.Fatal("session id must ignore later turns")
	}
	// content 为 null 的 user 消息被跳过。
	nullFirst := deriveZcodeRequestIDs([]any{
		map[string]any{"role": "user", "content": nil},
		map[string]any{"role": "user", "content": "real"},
	})
	if nullFirst.session != deriveZcodeRequestIDs([]any{map[string]any{"role": "user", "content": "real"}}).session {
		t.Fatal("null content must be skipped")
	}
}

func TestZcodeTokenStoreParsing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetZcodeTokenCache()
	t.Cleanup(resetZcodeTokenCache)
	store := filepath.Join(home, ".nanorouter")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store, zcodeTokenStoreName)
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		resetZcodeTokenCache()
	}
	// 毫秒制过期有效。
	write(`{"bigmodel":{"api_key":"ms-key","expires_at":4102444800000}}`)
	if key := zcodeTokenFileKey(ZcodeChannelBigmodel); key != "ms-key" {
		t.Fatalf("ms key=%q", key)
	}
	// api_key 本身是平台 JWT → 拒绝。
	write(`{"bigmodel":{"api_key":"eyJhbGci.jwt"}}`)
	if key := zcodeTokenFileKey(ZcodeChannelBigmodel); key != "" {
		t.Fatalf("jwt key=%q", key)
	}
	// 参考客户端旧平铺形状。
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	resetZcodeTokenCache()
	ref := filepath.Join(home, zcodeReferenceTokenName)
	if err := os.WriteFile(ref, []byte(`{"provider":"zai","api_key":"flat-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resetZcodeTokenCache()
	if key := zcodeTokenFileKey(ZcodeChannelZai); key != "flat-key" {
		t.Fatalf("flat key=%q", key)
	}
	// 跨分组回退：请求 zai，文件只有 bigmodel。
	resetZcodeTokenCache()
	if key := zcodeTokenFileKey(ZcodeChannelZai); key == "" {
		t.Fatalf("cross-group fallback lost the key: %q", key)
	}
	// nanorouter 自有文件优先于参考文件。
	write(`{"bigmodel":{"api_key":"own-key"}}`)
	if key := zcodeTokenFileKey(ZcodeChannelBigmodel); key != "own-key" {
		t.Fatalf("own store must win: %q", key)
	}
}

func mustOpenMemoryDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	return db
}
