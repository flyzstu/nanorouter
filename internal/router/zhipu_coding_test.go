package router

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	json "github.com/goccy/go-json"
	_ "github.com/ncruces/go-sqlite3/driver"
)

const (
	zhipuCodingBaseURL = "https://open.bigmodel.cn/api/coding/paas/v4"
	zhipuCodingModel   = "glm-5.3"
	zhipuCodingKeyEnv  = "ZHIPU_API_KEY"
)

func zhipuCodingCandidate(baseURL string) Candidate {
	return Candidate{
		ID:                          "zhipuaicoding-glm-5-3",
		BaseURL:                     baseURL,
		APIKeyEnv:                   zhipuCodingKeyEnv,
		Model:                       zhipuCodingModel,
		ExposeAs:                    "glm-5.3",
		SimulateOpenCode:            true,
		ResponsesViaChatCompletions: true,
	}
}

func newZhipuCodingTestRouter(t *testing.T, candidates ...Candidate) (*Router, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{
		RequestTimeout: 2 * time.Second,
		Routes:         map[string]Route{"zhipuaicoding": {Candidates: candidates}},
	}, db)
	if err != nil {
		t.Fatal(err)
	}
	return r, db
}

func TestZhipuCodingCandidateConfiguration(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	config := `routes:
  zhipuaicoding:
    candidates:
      - id: zhipuaicoding-glm-5-3
        base_url: https://open.bigmodel.cn/api/coding/paas/v4
        api_key_env: ZHIPU_API_KEY
        model: glm-5.3
        expose_as: glm-5.3
        simulate_opencode: true
        enabled: true
        headers:
          X-Custom-Test: test-value
`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	route, ok := c.Routes["zhipuaicoding"]
	if !ok || len(route.Candidates) != 1 {
		t.Fatalf("routes=%+v", c.Routes)
	}
	cand := route.Candidates[0]
	if !cand.SimulateOpenCode {
		t.Fatalf("expected simulate_opencode=true, got %+v", cand)
	}
	if cand.Headers["X-Custom-Test"] != "test-value" {
		t.Fatalf("expected custom header, got %+v", cand.Headers)
	}
}

func TestZhipuCodingSimulatesOpenCodeHeaders(t *testing.T) {
	t.Setenv(zhipuCodingKeyEnv, "test-zhipu-key-12345")
	var capturedHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer upstream.Close()

	r, _ := newZhipuCodingTestRouter(t, zhipuCodingCandidate(upstream.URL))

	body := `{"model":"zhipuaicoding","messages":[{"role":"user","content":"hello"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Session-Affinity", "custom-session-999")
	req.Header.Set("X-Parent-Session-Id", "custom-parent-111")
	r.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 1. User-Agent 必须完全模拟 OpenCode
	if ua := capturedHeaders.Get("User-Agent"); ua != defaultOpenCodeUserAgent {
		t.Fatalf("expected User-Agent=%s, got %s", defaultOpenCodeUserAgent, ua)
	}

	// 2. Authorization 必须使用 Zhipu API Key
	if auth := capturedHeaders.Get("Authorization"); auth != "Bearer test-zhipu-key-12345" {
		t.Fatalf("expected Authorization=Bearer test-zhipu-key-12345, got %s", auth)
	}

	// 3. X-Session-Affinity 与 X-Session-Id 必须正确透传
	if aff := capturedHeaders.Get("X-Session-Affinity"); aff != "custom-session-999" {
		t.Fatalf("expected X-Session-Affinity=custom-session-999, got %s", aff)
	}
	if sid := capturedHeaders.Get("X-Session-Id"); sid != "custom-session-999" {
		t.Fatalf("expected X-Session-Id=custom-session-999, got %s", sid)
	}

	// 4. X-Parent-Session-Id 必须正确透传
	if pid := capturedHeaders.Get("X-Parent-Session-Id"); pid != "custom-parent-111" {
		t.Fatalf("expected X-Parent-Session-Id=custom-parent-111, got %s", pid)
	}
}

func TestZhipuCodingInjectsThinkingAndStreamOptions(t *testing.T) {
	t.Setenv(zhipuCodingKeyEnv, "test-zhipu-key")
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	r, _ := newZhipuCodingTestRouter(t, zhipuCodingCandidate(upstream.URL))

	// 下游未传入 thinking 与 stream_options，但带有 promptCacheKey、store、reasoning_effort 和 assistant 的 reasoning_content
	body := `{
		"model":"zhipuaicoding",
		"stream": true,
		"promptCacheKey": "should-be-dropped",
		"prompt_cache_key": "should-be-dropped",
		"store": false,
		"reasoning_effort": "high",
		"messages":[
			{"role":"user","content":"first"},
			{"role":"assistant","content":"thought","reasoning_content":"deep thought process"},
			{"role":"user","content":"second"}
		]
	}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 1. model 字段已重写为真实模型 ID
	if received["model"] != "glm-5.3" {
		t.Fatalf("expected model=glm-5.3, got %v", received["model"])
	}

	// 2. thinking 必须注入为 {"type": "enabled", "clear_thinking": false}
	thinking, ok := received["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking not injected: %+v", received)
	}
	if thinking["type"] != "enabled" || thinking["clear_thinking"] != false {
		t.Fatalf("invalid thinking payload: %+v", thinking)
	}

	// 3. stream_options.include_usage 必须注入
	streamOptions, ok := received["stream_options"].(map[string]any)
	if !ok || streamOptions["include_usage"] != true {
		t.Fatalf("stream_options not injected: %+v", received)
	}

	// 4. promptCacheKey, prompt_cache_key, store, reasoning_effort 必须被清理
	for _, f := range []string{"promptCacheKey", "prompt_cache_key", "store", "reasoning_effort"} {
		if _, ok := received[f]; ok {
			t.Fatalf("%s was not removed: %+v", f, received)
		}
	}

	// 5. assistant 历史消息的 reasoning_content 必须保留，模拟 OpenCode 多轮推理连续性
	msgs := received["messages"].([]any)
	asst := msgs[1].(map[string]any)
	if asst["reasoning_content"] != "deep thought process" {
		t.Fatalf("reasoning_content should be preserved, got %+v", asst)
	}
}

func TestZhipuCodingDedicatedPathRouting(t *testing.T) {
	t.Setenv(zhipuCodingKeyEnv, "test-zhipu-key")
	var hitPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	r, _ := newZhipuCodingTestRouter(t, zhipuCodingCandidate(upstream.URL))

	// 1. 直接通过 /zhipuaicoding/v1/chat/completions 访问，payload 携带任意 model 也默认导流到 zhipuaicoding
	body := `{"model":"arbitrary-model","messages":[{"role":"user","content":"hi"}]}`
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/zhipuaicoding/v1/chat/completions", strings.NewReader(body))
	r.Handler().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec1.Code, rec1.Body.String())
	}
	if hitPath != "/chat/completions" {
		t.Fatalf("expected hitPath=/chat/completions, got %s", hitPath)
	}

	// 2. 通过 /zhipuaicoding/chat/completions 访问（无 /v1 前缀兼容）
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/zhipuaicoding/chat/completions", strings.NewReader(body))
	r.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec2.Code, rec2.Body.String())
	}
}

func TestZhipuCodingStreamingSSEReasoning(t *testing.T) {
	t.Setenv(zhipuCodingKeyEnv, "test-zhipu-key")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"thinking hard\"}}]}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":8,\"total_tokens\":20}}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	r, db := newZhipuCodingTestRouter(t, zhipuCodingCandidate(upstream.URL))

	body := `{"model":"zhipuaicoding","stream":true,"messages":[{"role":"user","content":"code it"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	respBody := rec.Body.String()
	if !strings.Contains(respBody, "thinking hard") {
		t.Fatalf("reasoning_content missing from stream: %s", respBody)
	}
	if !strings.Contains(respBody, "done") {
		t.Fatalf("content missing from stream: %s", respBody)
	}

	var total int
	if err := db.QueryRow(`SELECT total_tokens FROM llm_requests`).Scan(&total); err != nil || total != 20 {
		t.Fatalf("expected total_tokens=20 in store, got %d (err: %v)", total, err)
	}
}

func TestZhipuCodingReasoningEffortNoneDisablesThinking(t *testing.T) {
	t.Setenv(zhipuCodingKeyEnv, "test-zhipu-key")
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	r, _ := newZhipuCodingTestRouter(t, zhipuCodingCandidate(upstream.URL))

	body := `{
		"model":"zhipuaicoding",
		"reasoning_effort": "none",
		"messages":[{"role":"user","content":"no thinking please"}]
	}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	thinking, ok := received["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking missing: %+v", received)
	}
	if thinking["type"] != "disabled" {
		t.Fatalf("expected thinking.type=disabled, got %+v", thinking)
	}
	if _, ok := received["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort was not removed: %+v", received)
	}
}

func TestZhipuCodingGlm53FlashCandidate(t *testing.T) {
	t.Setenv(zhipuCodingKeyEnv, "test-zhipu-key")
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	cand := Candidate{
		ID:                          "zhipuaicoding-glm-5-3-flash",
		BaseURL:                     upstream.URL,
		APIKeyEnv:                   zhipuCodingKeyEnv,
		Model:                       "glm-5.3-flash",
		ExposeAs:                    "glm-5.3-flash",
		SimulateOpenCode:            true,
		ResponsesViaChatCompletions: true,
	}
	r, _ := newZhipuCodingTestRouter(t, cand)

	body := `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hello flash"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if received["model"] != "glm-5.3-flash" {
		t.Fatalf("expected model=glm-5.3-flash, got %v", received["model"])
	}
}

func TestZhipuCodingSessionAffinityFallbackToSessionID(t *testing.T) {
	t.Setenv(zhipuCodingKeyEnv, "test-zhipu-key")
	var capturedHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	r, _ := newZhipuCodingTestRouter(t, zhipuCodingCandidate(upstream.URL))

	body := `{"model":"zhipuaicoding","messages":[{"role":"user","content":"hello"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Session-Id", "session-only-777")
	r.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if aff := capturedHeaders.Get("X-Session-Affinity"); aff != "session-only-777" {
		t.Fatalf("expected X-Session-Affinity=session-only-777, got %s", aff)
	}
}
