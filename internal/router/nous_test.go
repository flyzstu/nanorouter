package router

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	json "github.com/goccy/go-json"
	_ "github.com/ncruces/go-sqlite3/driver"
)

const nousTestModel = "Hermes-4.3-36B"

func nousTestCandidate(baseURL string) Candidate {
	return Candidate{
		ID:                          "nous-hermes-4-3-36b",
		BaseURL:                     baseURL,
		APIKeyEnv:                   "NOUS_API_KEY",
		Model:                       nousTestModel,
		ExposeAs:                    "hermes-4.3-36b",
		ResponsesViaChatCompletions: true,
	}
}

func newNousTestRouter(t *testing.T, routeName string, candidates ...Candidate) (*Router, *sql.DB) {
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
		Routes:         map[string]Route{routeName: {Candidates: candidates}},
	}, db)
	if err != nil {
		t.Fatal(err)
	}
	return r, db
}

func TestNousCandidateConfiguration(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	config := `routes:
  free-fast:
    candidates:
      - id: nous-hermes-4-3-36b
        base_url: https://inference-api.nousresearch.com/v1
        api_key_env: NOUS_API_KEY
        model: Hermes-4.3-36B
        expose_as: hermes-4.3-36b
        weight: 1
        enabled: true
        responses_via_chat_completions: true
`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate := c.Routes["free-fast"].Candidates[0]
	if candidate.ID != "nous-hermes-4-3-36b" || candidate.BaseURL != "https://inference-api.nousresearch.com/v1" || candidate.APIKeyEnv != "NOUS_API_KEY" {
		t.Fatalf("candidate identity=%+v", candidate)
	}
	if candidate.Model != nousTestModel || candidate.ExposeAs != "hermes-4.3-36b" || candidate.Weight != 1 {
		t.Fatalf("candidate model/routing=%+v", candidate)
	}
	if !candidate.IsEnabled() || !candidate.ResponsesViaChatCompletions {
		t.Fatalf("candidate flags=%+v", candidate)
	}
}

func TestNousChatCompletionForwardsAuthAndModel(t *testing.T) {
	t.Setenv("NOUS_API_KEY", "nous-test-key")
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.Error(w, "unexpected path", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer nous-test-key" {
			http.Error(w, "unexpected authorization", http.StatusUnauthorized)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if received["model"] != nousTestModel {
			http.Error(w, "unexpected model", http.StatusBadRequest)
			return
		}
		messages, _ := received["messages"].([]any)
		if len(messages) != 2 {
			http.Error(w, "unexpected messages", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"nous-chat-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Bonjour"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`))
	}))
	defer upstream.Close()

	r, db := newNousTestRouter(t, "nous", nousTestCandidate(upstream.URL))
	body := `{"model":"nous","messages":[{"role":"system","content":"You are concise."},{"role":"user","content":"Say hello."}],"temperature":0.2}`
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Nanorouter-Upstream") != "nous-hermes-4-3-36b" {
		t.Fatalf("routing headers=%#v", rec.Header())
	}
	if received["temperature"] != float64(0.2) {
		t.Fatalf("temperature=%v payload=%#v", received["temperature"], received)
	}
	if !strings.Contains(rec.Body.String(), `"content":"Bonjour"`) {
		t.Fatalf("response=%s", rec.Body.String())
	}
	var prompt, completion, total int
	if err := db.QueryRow(`SELECT prompt_tokens, completion_tokens, total_tokens FROM llm_requests`).Scan(&prompt, &completion, &total); err != nil {
		t.Fatal(err)
	}
	if prompt != 12 || completion != 3 || total != 15 {
		t.Fatalf("usage=%d/%d/%d", prompt, completion, total)
	}
}

func TestNousChatCompletionStreamingPreservesUsage(t *testing.T) {
	t.Setenv("NOUS_API_KEY", "nous-stream-key")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer nous-stream-key" {
			http.Error(w, "invalid upstream request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, chunk := range []string{
			`data: {"choices":[{"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}` + "\n\n",
			`data: {"choices":[{"delta":{"content":" Nous"},"finish_reason":"stop"}]}` + "\n\n",
			`data: {"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":2,"total_tokens":22}}` + "\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = w.Write([]byte(chunk))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	r, db := newNousTestRouter(t, "nous", nousTestCandidate(upstream.URL))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"nous","messages":[{"role":"user","content":"hello"}],"stream":true}`)))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status/content-type=%d/%q body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	for _, want := range []string{`"content":"Hello"`, `"content":" Nous"`, `"prompt_tokens":20`, "data: [DONE]"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("stream missing %q:\n%s", want, rec.Body.String())
		}
	}
	var streamed, total int
	if err := db.QueryRow(`SELECT streamed, total_tokens FROM llm_requests`).Scan(&streamed, &total); err != nil {
		t.Fatal(err)
	}
	if streamed != 1 || total != 22 {
		t.Fatalf("stream ledger=%d total=%d", streamed, total)
	}
}

func TestNousResponsesAdapterForwardsToolsAndWrapsToolCall(t *testing.T) {
	t.Setenv("NOUS_API_KEY", "nous-responses-key")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer nous-responses-key" {
			http.Error(w, "invalid upstream request", http.StatusBadRequest)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if body["model"] != nousTestModel || body["max_tokens"] != float64(128) || body["tool_choice"] != "required" {
			http.Error(w, "request was not adapted", http.StatusBadRequest)
			return
		}
		messages, _ := body["messages"].([]any)
		if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" || messages[1].(map[string]any)["role"] != "user" {
			http.Error(w, "messages were not adapted", http.StatusBadRequest)
			return
		}
		tools, _ := body["tools"].([]any)
		if len(tools) != 1 || tools[0].(map[string]any)["type"] != "function" {
			http.Error(w, "tools were not adapted", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"nous-tool-1","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30,"completion_tokens":9,"total_tokens":39}}`))
	}))
	defer upstream.Close()

	r, _ := newNousTestRouter(t, "nous", nousTestCandidate(upstream.URL))
	body := `{"model":"nous","instructions":"Use the weather function.","input":"What is the weather in Paris?","tools":[{"type":"function","name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"tool_choice":"required","max_output_tokens":128}`
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["object"] != "response" || response["model"] != "nous" || response["status"] != "completed" {
		t.Fatalf("response=%#v", response)
	}
	output, _ := response["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%#v", output)
	}
	item := output[0].(map[string]any)
	if item["type"] != "function_call" || item["call_id"] != "call_weather" || item["name"] != "get_weather" {
		t.Fatalf("tool output=%#v", item)
	}
}

func TestNousResponsesStreamingAdaptsChatSSE(t *testing.T) {
	t.Setenv("NOUS_API_KEY", "nous-responses-stream-key")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer nous-responses-stream-key" {
			http.Error(w, "invalid upstream request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"choices":[{"delta":{"role":"assistant","content":"streamed"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"content":" response"},"finish_reason":"stop"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":14,"completion_tokens":2,"total_tokens":16}}`,
			"data: [DONE]",
			"",
		}, "\n\n")))
	}))
	defer upstream.Close()

	r, db := newNousTestRouter(t, "nous", nousTestCandidate(upstream.URL))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"nous","input":"say hi","stream":true}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "response.completed") {
		t.Fatalf("status/body=%d\n%s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"streamed", "response", `"total_tokens":16`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("adapted stream missing %q:\n%s", want, rec.Body.String())
		}
	}
	var streamed, total int
	if err := db.QueryRow(`SELECT streamed, total_tokens FROM llm_requests`).Scan(&streamed, &total); err != nil {
		t.Fatal(err)
	}
	if streamed != 1 || total != 16 {
		t.Fatalf("stream ledger=%d total=%d", streamed, total)
	}
}

func TestNousRateLimitFallsBackToSecondaryCandidate(t *testing.T) {
	t.Setenv("NOUS_API_KEY", "nous-fallback-key")
	var nousCalls, backupCalls atomic.Int32
	nous := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nousCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer nous-fallback-key" {
			http.Error(w, "missing Nous authorization", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"Nous free-tier TPM limit"}}`))
	}))
	defer nous.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["model"] != "backup-model" {
			http.Error(w, "wrong backup request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"id":"backup-1","usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`))
	}))
	defer backup.Close()

	backupCandidate := Candidate{ID: "backup", BaseURL: backup.URL, Model: "backup-model"}
	nousCandidate := nousTestCandidate(nous.URL)
	r, db := newNousTestRouter(t, "free-fast", nousCandidate, backupCandidate)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free-fast","messages":[{"role":"user","content":"fallback"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if nousCalls.Load() != 1 || backupCalls.Load() != 1 {
		t.Fatalf("calls nous=%d backup=%d", nousCalls.Load(), backupCalls.Load())
	}
	if rec.Header().Get("X-Nanorouter-Upstream") != "backup" || rec.Header().Get("X-Nanorouter-Attempts") != "2" {
		t.Fatalf("routing headers=%#v", rec.Header())
	}
	var records int
	if err := db.QueryRow(`SELECT COUNT(*) FROM llm_requests`).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if records != 2 {
		t.Fatalf("ledger records=%d want 2", records)
	}
}

func TestNousUnsupportedResponsesFeatureDoesNotCallUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	r, _ := newNousTestRouter(t, "nous", nousTestCandidate(upstream.URL))
	body := `{"model":"nous","input":"continue","previous_response_id":"resp-previous"}`
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "previous_response_id") {
		t.Fatalf("status/body=%d/%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls=%d, unsupported feature should be rejected locally", calls.Load())
	}
}
