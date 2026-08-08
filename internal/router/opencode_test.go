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
	opencodeGoBaseURL = "https://opencode.ai/zen/go/v1"
	opencodeGoModel   = "deepseek-v4-flash"
	opencodeGoKeyEnv  = "OPENCODE_GO_API_KEY"
)

func opencodeGoChatCandidate(baseURL string) Candidate {
	return Candidate{
		ID:        "opencode-go-deepseek-v4-flash",
		BaseURL:   baseURL,
		APIKeyEnv: opencodeGoKeyEnv,
		Model:     opencodeGoModel,
		ExposeAs:  "opencode-go-deepseek-v4-flash",
	}
}

func opencodeGoResponsesCandidate(baseURL string) Candidate {
	return Candidate{
		ID:        "opencode-go-gpt-5-6-luna",
		BaseURL:   baseURL,
		APIKeyEnv: opencodeGoKeyEnv,
		Model:     "gpt-5.6-luna",
		ExposeAs:  "opencode-go-gpt-5-6-luna",
		Responses: true,
	}
}

func newOpenCodeGoTestRouter(t *testing.T, routeName string, candidates ...Candidate) (*Router, *sql.DB) {
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

func TestOpenCodeGoCandidateConfiguration(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	config := `routes:
  opencode-go:
    candidates:
      - id: opencode-go-deepseek-v4-flash
        base_url: https://opencode.ai/zen/go/v1
        api_key_env: OPENCODE_GO_API_KEY
        model: deepseek-v4-flash
        expose_as: opencode-go-deepseek-v4-flash
        weight: 3
        enabled: true
      - id: opencode-go-gpt-5-6-luna
        base_url: https://opencode.ai/zen/go/v1
        api_key_env: OPENCODE_GO_API_KEY
        model: gpt-5.6-luna
        expose_as: opencode-go-gpt-5-6-luna
        responses: true
`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	route := c.Routes["opencode-go"]
	if len(route.Candidates) != 2 {
		t.Fatalf("candidates=%d", len(route.Candidates))
	}
	flash := route.Candidates[0]
	if flash.ID != "opencode-go-deepseek-v4-flash" || flash.BaseURL != opencodeGoBaseURL || flash.APIKeyEnv != opencodeGoKeyEnv {
		t.Fatalf("flash identity=%+v", flash)
	}
	if flash.Model != opencodeGoModel || flash.ExposeAs != "opencode-go-deepseek-v4-flash" || flash.Weight != 3 || !flash.IsEnabled() {
		t.Fatalf("flash routing=%+v", flash)
	}
	luna := route.Candidates[1]
	if luna.Model != "gpt-5.6-luna" || !luna.Responses {
		t.Fatalf("luna=%+v", luna)
	}
}

func TestOpenCodeGoChatCompletionForwardsAuthAndModel(t *testing.T) {
	t.Setenv(opencodeGoKeyEnv, "opencode-go-test-key")
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.Error(w, "unexpected path", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer opencode-go-test-key" {
			http.Error(w, "unexpected authorization", http.StatusUnauthorized)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if received["model"] != opencodeGoModel {
			http.Error(w, "unexpected model", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"go-chat-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
	}))
	defer upstream.Close()

	r, db := newOpenCodeGoTestRouter(t, "opencode-go", opencodeGoChatCandidate(upstream.URL))
	body := `{"model":"opencode-go","messages":[{"role":"user","content":"hi"}],"temperature":0.1}`
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Nanorouter-Upstream") != "opencode-go-deepseek-v4-flash" {
		t.Fatalf("routing headers=%#v", rec.Header())
	}
	if received["temperature"] != float64(0.1) {
		t.Fatalf("temperature=%v payload=%#v", received["temperature"], received)
	}
	var total int
	if err := db.QueryRow(`SELECT total_tokens FROM llm_requests`).Scan(&total); err != nil || total != 7 {
		t.Fatalf("usage=%d err=%v", total, err)
	}
}

func TestOpenCodeGoNativeResponsesForwardsPayload(t *testing.T) {
	t.Setenv(opencodeGoKeyEnv, "opencode-go-responses-key")
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.Error(w, "unexpected path", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer opencode-go-responses-key" {
			http.Error(w, "unexpected authorization", http.StatusUnauthorized)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if received["model"] != "gpt-5.6-luna" {
			http.Error(w, "unexpected model", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","model":"gpt-5.6-luna","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":4,"output_tokens":3,"total_tokens":7}}`))
	}))
	defer upstream.Close()

	r, _ := newOpenCodeGoTestRouter(t, "opencode-go", opencodeGoResponsesCandidate(upstream.URL))
	body := `{"model":"opencode-go","input":"say hi","max_output_tokens":64}`
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if received["max_output_tokens"] != float64(64) {
		t.Fatalf("max_output_tokens=%v payload=%#v", received["max_output_tokens"], received)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// 原生 Responses 按设计透明透传，模型 ID 保持上游真实值。
	if out["object"] != "response" || out["model"] != "gpt-5.6-luna" {
		t.Fatalf("response=%#v", out)
	}
}
