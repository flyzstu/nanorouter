package router

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	json "github.com/goccy/go-json"
	_ "github.com/ncruces/go-sqlite3/driver"
)

func TestConfigDuration(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte("request_timeout: 2s\nroutes:\n  r:\n    candidates:\n      - id: u\n        base_url: http://example.test\n        model: m\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.RequestTimeout.Seconds() != 2 {
		t.Fatalf("timeout=%s", c.RequestTimeout)
	}
}

func TestFailoverRewritesModelAndRecordsUsage(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }))
	defer bad.Close()
	var gotModel string
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		gotModel, _ = in["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	}))
	defer good.Close()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{{ID: "bad", BaseURL: bad.URL, Model: "bad-model"}, {ID: "good", BaseURL: good.URL, Model: "good-model"}}}}, RequestTimeout: 0}, db)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotModel != "good-model" {
		t.Fatalf("model=%q", gotModel)
	}
	var tokens int
	if err := db.QueryRow(`SELECT total_tokens FROM llm_requests WHERE upstream_id='good'`).Scan(&tokens); err != nil || tokens != 7 {
		t.Fatalf("usage: %d %v", tokens, err)
	}
}

func TestResponsesStreamCollectsNestedUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":5,\"total_tokens\":7}}}\n\n"))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{{ID: "u", BaseURL: upstream.URL, Model: "up", Responses: true}}}}, RequestTimeout: 0}, db)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"free","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "response.completed") {
		t.Fatalf("status/body %d %s", rec.Code, rec.Body.String())
	}
	var tokens int
	if err := db.QueryRow(`SELECT total_tokens FROM llm_requests`).Scan(&tokens); err != nil || tokens != 7 {
		t.Fatalf("usage: %d %v", tokens, err)
	}
}

func TestResponsesCanAdaptToChatCompletions(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["max_tokens"] != float64(42) {
			t.Fatalf("max_tokens=%v", in["max_tokens"])
		}
		messages, _ := in["messages"].([]any)
		if len(messages) != 2 {
			t.Fatalf("messages=%#v", messages)
		}
		_, _ = w.Write([]byte(`{"data":{"id":"sense-1","choices":[{"message":"你好","finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}},"status":{"code":0}}`))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{{ID: "sense", BaseURL: upstream.URL, Model: "SenseChat", ResponsesViaChatCompletions: true}}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"free","instructions":"be helpful","input":"hello","max_output_tokens":42}`))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["object"] != "response" || out["model"] != "free" {
		t.Fatalf("response=%v", out)
	}
}

func TestResponsesAdapterRejectsTools(t *testing.T) {
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{{ID: "sense", BaseURL: "http://127.0.0.1:1", Model: "SenseChat", ResponsesViaChatCompletions: true}}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"free","input":"hello","tools":[]}`))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "tools cannot be adapted") {
		t.Fatalf("status/body %d %s", rec.Code, rec.Body.String())
	}
}

func TestCandidateSpecificRequestSanitization(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		for _, key := range []string{"logprobs", "logit_bias", "top_logprobs", "metadata", "store"} {
			if _, ok := in[key]; ok {
				t.Fatalf("%s was not removed", key)
			}
		}
		messages, _ := in["messages"].([]any)
		message := messages[0].(map[string]any)
		if _, ok := message["name"]; ok {
			t.Fatal("messages[].name was not removed")
		}
		_, _ = w.Write([]byte(`{"id":"ok","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{ID: "groq", BaseURL: upstream.URL, Model: "m", DropRequestFields: []string{"logprobs", "logit_bias", "top_logprobs", "metadata", "store"}, DropMessageFields: []string{"name"}}
	r, err := New(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{candidate}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"free","messages":[{"role":"user","name":"alice","content":"hi"}],"logprobs":true,"logit_bias":{},"top_logprobs":2,"metadata":{},"store":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDisabledCandidateIsSkippedAndHiddenWhenAllDisabled(t *testing.T) {
	disabled := false
	active := true
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer good.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{
		"free":   {Candidates: []Candidate{{ID: "off", BaseURL: "http://127.0.0.1:1", Model: "off", Enabled: &disabled}, {ID: "on", BaseURL: good.URL, Model: "on", Enabled: &active}}},
		"hidden": {Candidates: []Candidate{{ID: "off2", BaseURL: "http://127.0.0.1:1", Model: "off", Enabled: &disabled}}},
	}}, db)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	models := httptest.NewRecorder()
	r.Handler().ServeHTTP(models, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if strings.Contains(models.Body.String(), "hidden") {
		t.Fatalf("disabled route leaked: %s", models.Body.String())
	}
}
