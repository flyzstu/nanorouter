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
	_ "github.com/lib/pq"
	_ "github.com/ncruces/go-sqlite3/driver"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestConfigDuration(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte("request_timeout: 2s\nroutes:\n  r:\n    candidates:\n      - id: u\n        base_url: http://example.test\n        model: m\n        max_concurrency: 2\n        queue_timeout: 3s\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.RequestTimeout.Seconds() != 2 {
		t.Fatalf("timeout=%s", c.RequestTimeout)
	}
	candidate := c.Routes["r"].Candidates[0]
	if candidate.MaxConcurrency != 2 || candidate.QueueTimeout != 3*time.Second {
		t.Fatalf("candidate limit=%d queue_timeout=%s", candidate.MaxConcurrency, candidate.QueueTimeout)
	}
}

func TestConfigRejectsExposedModelCollision(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	config := "routes:\n  free-fast:\n    candidates:\n      - id: u\n        base_url: http://example.test\n        model: m\n        expose_as: free-fast\n"
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("error=%v", err)
	}
}

func TestConfigRejectsQueueTimeoutWithoutConcurrencyLimit(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	config := "routes:\n  r:\n    candidates:\n      - id: u\n        base_url: http://example.test\n        model: m\n        queue_timeout: 1s\n"
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "queue_timeout") {
		t.Fatalf("error=%v", err)
	}
}

func TestConfigSupportsPostgresDSNEnvironment(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	config := "database:\n  driver: postgresql\n  dsn_env: NANOROUTER_POSTGRES_DSN\nroutes:\n  r:\n    candidates:\n      - id: u\n        base_url: http://example.test\n        model: m\n"
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NANOROUTER_POSTGRES_DSN", "postgresql://user:password@postgres:5432/nanorouter?sslmode=disable")
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Database.Driver != "postgres" || c.Database.SQLDriver() != "postgres" {
		t.Fatalf("database=%+v sql_driver=%q", c.Database, c.Database.SQLDriver())
	}
	dsn, err := c.Database.ResolveDSN()
	if err != nil || !strings.HasPrefix(dsn, "postgresql://") {
		t.Fatalf("dsn=%q err=%v", dsn, err)
	}
}

func TestConfigSupportsLegacySQLiteDatabasePath(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	config := "database: ./usage.db\nroutes:\n  r:\n    candidates:\n      - id: u\n        base_url: http://example.test\n        model: m\n"
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Database.Driver != "sqlite" || c.Database.DSN != "./usage.db" || c.Database.SQLDriver() != "sqlite3" {
		t.Fatalf("database=%+v sql_driver=%q", c.Database, c.Database.SQLDriver())
	}
}

func TestPostgresPlaceholders(t *testing.T) {
	query := `INSERT INTO llm_requests VALUES (?, ?, ?)`
	if got, want := postgresPlaceholders(query), `INSERT INTO llm_requests VALUES ($1, $2, $3)`; got != want {
		t.Fatalf("query=%q want=%q", got, want)
	}
}

func TestPostgresStore(t *testing.T) {
	dsn := os.Getenv("NANOROUTER_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set NANOROUTER_TEST_POSTGRES_DSN to run PostgreSQL integration test")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	id := "postgres-store-" + time.Now().UTC().Format("20060102150405.000000000")
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM llm_requests WHERE id = $1`, id) })
	NewStore(db, "postgres").Insert(context.Background(), Record{
		ID: id, Route: "test", UpstreamID: "postgres", UpstreamModel: "test-model", Endpoint: "chat_completions",
		Status: http.StatusOK, LatencyMS: 1, Attempt: 0, Usage: Usage{Prompt: 2, Completion: 3, Total: 5, Source: "upstream"},
	})
	var total int64
	if err := db.QueryRow(`SELECT total_tokens FROM llm_requests WHERE id = $1`, id).Scan(&total); err != nil || total != 5 {
		t.Fatalf("total=%d err=%v", total, err)
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

func TestLlamaServerCandidateUsesV1PrefixWithoutAPIKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if authorization := r.Header.Get("Authorization"); authorization != "" {
			t.Fatalf("authorization=%q", authorization)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["model"] != "local-llama" {
			t.Fatalf("model=%v", payload["model"])
		}
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer upstream.Close()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"local": {Candidates: []Candidate{{ID: "llama", BaseURL: upstream.URL, Model: "local-llama", LlamaServer: true}}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"local","messages":[]}`))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCandidateConcurrencyLimitQueuesThenFailsOver(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-finish
		_, _ = w.Write([]byte(`{"id":"busy"}`))
	}))
	defer busy.Close()
	var spareCalls int
	spare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		spareCalls++
		_, _ = w.Write([]byte(`{"id":"spare"}`))
	}))
	defer spare.Close()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"local": {Candidates: []Candidate{
		{ID: "busy", BaseURL: busy.URL, Model: "busy", Weight: 2, MaxConcurrency: 1, QueueTimeout: 25 * time.Millisecond},
		{ID: "spare", BaseURL: spare.URL, Model: "spare"},
	}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"local","messages":[]}`)))
		firstDone <- rec
	}()
	<-started
	begin := time.Now()
	second := httptest.NewRecorder()
	r.Handler().ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"local","messages":[]}`)))
	if elapsed := time.Since(begin); elapsed < 20*time.Millisecond {
		t.Fatalf("request did not wait for queue timeout: %s", elapsed)
	}
	if second.Code != http.StatusOK || spareCalls != 1 || !strings.Contains(second.Body.String(), "spare") {
		t.Fatalf("status=%d spare_calls=%d body=%s", second.Code, spareCalls, second.Body.String())
	}
	close(finish)
	if first := <-firstDone; first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
}

func TestCandidateConcurrencyLimitRejectsWhenQueueTimesOut(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-finish
		_, _ = w.Write([]byte(`{"id":"done"}`))
	}))
	defer upstream.Close()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"local": {Candidates: []Candidate{{ID: "busy", BaseURL: upstream.URL, Model: "busy", MaxConcurrency: 1, QueueTimeout: 25 * time.Millisecond}}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"local","messages":[]}`)))
		firstDone <- rec
	}()
	<-started
	second := httptest.NewRecorder()
	r.Handler().ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"local","messages":[]}`)))
	if second.Code != http.StatusTooManyRequests || !strings.Contains(second.Body.String(), "upstream_concurrency_limit") {
		t.Fatalf("status=%d body=%s", second.Code, second.Body.String())
	}
	close(finish)
	if first := <-firstDone; first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
}

func TestRequestLogsIncludeCorrelationAndTiming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	}))
	defer upstream.Close()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	core, observed := observer.New(zap.InfoLevel)
	r, err := NewWithLogger(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{{ID: "u", BaseURL: upstream.URL, Model: "up"}}}}}, db, zap.New(core))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`))
	req.Header.Set("X-Request-ID", "call-123")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-ID") != "call-123" {
		t.Fatalf("request ID header=%q", rec.Header().Get("X-Request-ID"))
	}
	entries := observed.All()
	if len(entries) != 1 || entries[0].Message != "Upstream request completed" {
		t.Fatalf("logs=%v", entries)
	}
	fields := entries[0].ContextMap()
	if fields["request_id"] != "call-123" || fields["outcome"] != "success" || fields["upstream_model"] != "up" {
		t.Fatalf("fields=%v", fields)
	}
	if _, ok := fields["ttfb_ms"]; !ok {
		t.Fatalf("ttfb missing: %v", fields)
	}
	if _, ok := fields["duration_ms"]; !ok {
		t.Fatalf("duration missing: %v", fields)
	}
}

func TestDebugLogIncludesUpstreamRejectionReason(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"model access denied","type":"permission_error","code":"model_not_found"}}`))
	}))
	defer upstream.Close()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	core, observed := observer.New(zap.DebugLevel)
	r, err := NewWithLogger(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{{ID: "u", BaseURL: upstream.URL, Model: "up"}}}}}, db, zap.New(core))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != `{"error":{"message":"model access denied","type":"permission_error","code":"model_not_found"}}` {
		t.Fatalf("body=%s", rec.Body.String())
	}
	entries := observed.All()
	if len(entries) != 1 || entries[0].Message != "Upstream request rejected" {
		t.Fatalf("logs=%v", entries)
	}
	fields := entries[0].ContextMap()
	if fields["upstream_error_type"] != "permission_error" || fields["upstream_error_code"] != "model_not_found" || fields["upstream_error_message"] != "model access denied" {
		t.Fatalf("fields=%v", fields)
	}
}

func TestUpstreamErrorMessageIsDebugOnly(t *testing.T) {
	fields := upstreamError{Type: "permission_error", Message: "model access denied"}.logFields(false)
	for _, field := range fields {
		if field.Key == "upstream_error_message" {
			t.Fatalf("message should not be logged without debug: %v", fields)
		}
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

func TestCandidateDropsOpenCodePromptCacheKeys(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		for _, key := range []string{"promptCacheKey", "prompt_cache_key"} {
			if _, ok := in[key]; ok {
				t.Fatalf("%s was not removed", key)
			}
		}
		_, _ = w.Write([]byte(`{"id":"ok","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{ID: "zhipu", BaseURL: upstream.URL, Model: "glm-5.1", DropRequestFields: []string{"promptCacheKey", "prompt_cache_key"}}
	r, err := New(Config{Routes: map[string]Route{"zhipu-agent": {Candidates: []Candidate{candidate}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"zhipu-agent","messages":[{"role":"user","content":"hi"}],"promptCacheKey":"session-a","prompt_cache_key":"session-a"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestExposedCandidateIsListedAndRoutable(t *testing.T) {
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		gotModel, _ = in["model"].(string)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{ID: "agnes", BaseURL: upstream.URL, Model: "agnes-2.0-flash", ExposeAs: "agnes-flash"}
	r, err := New(Config{Routes: map[string]Route{"free-fast": {Candidates: []Candidate{candidate}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	models := httptest.NewRecorder()
	r.Handler().ServeHTTP(models, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !strings.Contains(models.Body.String(), `"id":"agnes-flash"`) {
		t.Fatalf("exposed model missing: %s", models.Body.String())
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"agnes-flash","messages":[]}`))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotModel != "agnes-2.0-flash" {
		t.Fatalf("model=%q", gotModel)
	}
	if route := rec.Header().Get("X-Nanorouter-Route"); route != "agnes-flash" {
		t.Fatalf("route=%q", route)
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
