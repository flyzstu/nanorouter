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
	if err := os.WriteFile(path, []byte("request_timeout: 2s\nroutes:\n  r:\n    candidates:\n      - id: u\n        base_url: http://example.test\n        model: m\n        max_concurrency: 2\n        queue_timeout: 3s\n        max_tokens_limit: 65536\n"), 0600); err != nil {
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
	if candidate.MaxConcurrency != 2 || candidate.QueueTimeout != 3*time.Second || candidate.MaxTokensLimit != 65536 {
		t.Fatalf("candidate concurrency=%d queue_timeout=%s max_tokens_limit=%d", candidate.MaxConcurrency, candidate.QueueTimeout, candidate.MaxTokensLimit)
	}
}

func TestConfigLoggingOptions(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	config := "logging:\n  language: zh\n  level: warn\nroutes:\n  r:\n    candidates:\n      - id: u\n        base_url: http://example.test\n        model: m\n"
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Logging.Language != "zh-CN" || c.Logging.Level != "WARN" || c.Logging.Format != "text" || !c.Logging.ColorEnabled() {
		t.Fatalf("logging=%+v", c.Logging)
	}
}

func TestConfigRejectsUnsupportedLoggingOptions(t *testing.T) {
	tests := []struct {
		name    string
		logging string
	}{
		{name: "language", logging: "language: de\n  level: INFO"},
		{name: "level", logging: "language: en\n  level: TRACE"},
		{name: "format", logging: "language: en\n  level: INFO\n  format: xml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := t.TempDir() + "/config.yaml"
			config := "logging:\n  " + tt.logging + "\nroutes:\n  r:\n    candidates:\n      - id: u\n        base_url: http://example.test\n        model: m\n"
			if err := os.WriteFile(path, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("expected invalid logging configuration to fail")
			}
		})
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

func TestConfigRejectsNegativeMaxTokensLimit(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	config := "routes:\n  r:\n    candidates:\n      - id: u\n        base_url: http://example.test\n        model: m\n        max_tokens_limit: -1\n"
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "max_tokens_limit") {
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
		time.Sleep(20 * time.Millisecond)
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
	entries := observed.FilterMessage("request_completed").All()
	if len(entries) != 1 {
		t.Fatalf("logs=%v", observed.All())
	}
	fields := entries[0].ContextMap()
	if fields["request_id"] != "call-123" || fields["outcome"] != "success" || fields["upstream_model"] != "up" {
		t.Fatalf("fields=%v", fields)
	}
	if fields["event"] != "request_completed" || fields["attempts"] != int64(1) {
		t.Fatalf("lifecycle fields=%v", fields)
	}
	started := observed.FilterMessage("request_started").All()
	if len(started) != 1 || started[0].ContextMap()["http_path"] != "/v1/chat/completions" {
		t.Fatalf("start logs=%v", observed.All())
	}
	responses := observed.FilterMessage("upstream_response_received").All()
	if len(responses) != 1 {
		t.Fatalf("response logs=%v", observed.All())
	}
	responseFields := responses[0].ContextMap()
	if responseFields["event"] != "upstream_response_received" || responseFields["request_id"] != "call-123" || responseFields["status"] != int64(http.StatusOK) {
		t.Fatalf("response fields=%v", responseFields)
	}
	if elapsed, ok := responseFields["request_to_upstream_response_ms"].(int64); !ok || elapsed < 15 {
		t.Fatalf("request-to-upstream timing=%v", responseFields["request_to_upstream_response_ms"])
	}
}

func TestChineseLogMessagesPreserveEventField(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	core, observed := observer.New(zap.InfoLevel)
	cfg := Config{
		Logging: LoggingConfig{Language: "zh-CN", Level: "INFO"},
		Routes:  map[string]Route{"free": {Candidates: []Candidate{{ID: "u", BaseURL: upstream.URL, Model: "up"}}}},
	}
	r, err := NewWithLogger(cfg, db, zap.New(core))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`))
	r.Handler().ServeHTTP(httptest.NewRecorder(), req)
	entries := observed.FilterMessage("已收到上游响应").All()
	if len(entries) != 1 || entries[0].ContextMap()["event"] != "upstream_response_received" {
		t.Fatalf("logs=%v", observed.All())
	}
}

func TestUpstreamRejectionLogIncludesReason(t *testing.T) {
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
	core, observed := observer.New(zap.InfoLevel)
	r, err := NewWithLogger(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{{ID: "u", BaseURL: upstream.URL, Model: "up", ExposeAs: "direct"}}}}}, db, zap.New(core))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"direct","messages":[]}`))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != `{"error":{"message":"model access denied","type":"permission_error","code":"model_not_found"}}` {
		t.Fatalf("body=%s", rec.Body.String())
	}
	entries := observed.FilterMessage("upstream_attempt_finished").All()
	if len(entries) != 1 {
		t.Fatalf("logs=%v", observed.All())
	}
	fields := entries[0].ContextMap()
	if fields["upstream_error_type"] != "permission_error" || fields["upstream_error_code"] != "model_not_found" || fields["upstream_error_message"] != "model access denied" {
		t.Fatalf("fields=%v", fields)
	}
}

func TestUpstreamErrorMessageIsIncludedInFailureLogs(t *testing.T) {
	fields := upstreamError{Type: "permission_error", Message: "model access denied"}.logFields(true)
	for _, field := range fields {
		if field.Key == "upstream_error_message" && field.String == "model access denied" {
			return
		}
	}
	t.Fatalf("upstream error message missing: %v", fields)
}

func TestOpenRouterNestedErrorDetailsAreLoggedWithoutFlaggedInput(t *testing.T) {
	body := []byte(`{
		"error": {
			"code": 400,
			"message": "Provider returned error",
			"metadata": {
				"error_type": "invalid_request",
				"provider_name": "NVIDIA",
				"provider_code": "bad_parameter",
				"param": "messages",
				"flagged_input": "do not write this prompt",
				"raw": "{\"error\":{\"message\":\"assistant message is invalid\",\"type\":\"invalid_request_error\",\"code\":\"invalid_messages\"}}"
			}
		}
	}`)
	fields := parseUpstreamError(body).logFields(true)
	got := map[string]string{}
	for _, field := range fields {
		switch field.Key {
		case "upstream_error_code", "upstream_error_type", "upstream_error_category", "upstream_provider", "upstream_provider_error_code", "upstream_error_param", "upstream_error_message", "upstream_error_detail":
			got[field.Key] = field.String
		}
	}
	want := map[string]string{
		"upstream_error_code":          "400",
		"upstream_error_type":          "invalid_request",
		"upstream_provider":            "NVIDIA",
		"upstream_provider_error_code": "bad_parameter",
		"upstream_error_param":         "messages",
		"upstream_error_message":       "Provider returned error",
		"upstream_error_detail":        "assistant message is invalid",
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("%s=%q want=%q fields=%v", key, got[key], value, got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("unexpected fields=%v want=%v", got, want)
	}
	for _, field := range fields {
		if strings.Contains(field.String, "do not write this prompt") {
			t.Fatalf("flagged input leaked into logs: %v", fields)
		}
	}
}

func TestNonRetryableUpstreamRejectionDoesNotFailOver(t *testing.T) {
	var rejected, succeeded int
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rejected++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"unsupported parameter","type":"invalid_request_error","code":"bad_param"}}`))
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		succeeded++
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer good.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	core, observed := observer.New(zap.InfoLevel)
	r, err := NewWithLogger(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{
		{ID: "bad", BaseURL: bad.URL, Model: "bad"},
		{ID: "good", BaseURL: good.URL, Model: "good"},
	}}}}, db, zap.New(core))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`)))
	if rec.Code != http.StatusBadRequest || rejected != 1 || succeeded != 0 {
		t.Fatalf("status=%d rejected=%d succeeded=%d body=%s", rec.Code, rejected, succeeded, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unsupported parameter") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	entries := observed.FilterMessage("upstream_attempt_finished").All()
	if len(entries) != 1 {
		t.Fatalf("logs=%v", observed.All())
	}
	fields := entries[0].ContextMap()
	if fields["retryable"] != false || fields["outcome"] != "upstream_rejected" {
		t.Fatalf("fields=%v", fields)
	}
	if fields["upstream_error_type"] != "invalid_request_error" || fields["upstream_error_code"] != "bad_param" || fields["upstream_error_message"] != "unsupported parameter" {
		t.Fatalf("fields=%v", fields)
	}
}

func TestServerErrorFailsOverToNextCandidate(t *testing.T) {
	var failed, succeeded int
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		failed++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"temporarily unavailable"}}`))
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		succeeded++
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer good.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{
		{ID: "bad", BaseURL: bad.URL, Model: "bad"},
		{ID: "good", BaseURL: good.URL, Model: "good"},
	}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`)))
	if rec.Code != http.StatusOK || failed != 1 || succeeded != 1 {
		t.Fatalf("status=%d failed=%d succeeded=%d body=%s", rec.Code, failed, succeeded, rec.Body.String())
	}
	if attempts := rec.Header().Get("X-Nanorouter-Attempts"); attempts != "2" {
		t.Fatalf("attempts=%q", attempts)
	}
}

func TestCandidateAuthOrModelErrorFailsOver(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var failed, succeeded int
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				failed++
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"message":"candidate unavailable"}}`))
			}))
			defer bad.Close()
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				succeeded++
				_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
			}))
			defer good.Close()
			db, _ := sql.Open("sqlite3", ":memory:")
			defer db.Close()
			if err := Migrate(context.Background(), db); err != nil {
				t.Fatal(err)
			}
			r, err := New(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{
				{ID: "bad", BaseURL: bad.URL, Model: "bad"},
				{ID: "good", BaseURL: good.URL, Model: "good"},
			}}}}, db)
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`)))
			if rec.Code != http.StatusOK || failed != 1 || succeeded != 1 {
				t.Fatalf("status=%d failed=%d succeeded=%d body=%s", rec.Code, failed, succeeded, rec.Body.String())
			}
		})
	}
}

func TestRateLimitRetriesInternallyWithExponentialSchedule(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	core, observed := observer.New(zap.InfoLevel)
	r, err := NewWithLogger(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{{ID: "limited", BaseURL: upstream.URL, Model: "up"}}}}}, db, zap.New(core))
	if err != nil {
		t.Fatal(err)
	}
	var backoffs []int
	r.retryBackoff = func(retry int) time.Duration {
		backoffs = append(backoffs, retry)
		return 0
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`)))
	if rec.Code != http.StatusOK || calls != 3 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
	if attempts := rec.Header().Get("X-Nanorouter-Attempts"); attempts != "3" {
		t.Fatalf("attempts=%q", attempts)
	}
	if got := observed.FilterMessage("retry_scheduled").All(); len(got) != 2 || got[0].ContextMap()["retry"] != int64(1) || got[1].ContextMap()["retry"] != int64(2) {
		t.Fatalf("retry logs=%v", got)
	}
	if len(backoffs) < 2 || backoffs[0] != 1 || backoffs[1] != 2 {
		t.Fatalf("backoff schedule=%v", backoffs)
	}
}

func TestExponentialBackoff(t *testing.T) {
	for retry, want := range map[int]time.Duration{
		1: time.Second,
		2: 2 * time.Second,
		3: 4 * time.Second,
		4: 8 * time.Second,
		5: 16 * time.Second,
	} {
		if got := exponentialBackoff(retry); got != want {
			t.Fatalf("retry=%d backoff=%s want=%s", retry, got, want)
		}
	}
}

func TestRateLimitStopsAfterFiveRetries(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{{ID: "limited", BaseURL: upstream.URL, Model: "up"}}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	r.retryBackoff = func(int) time.Duration { return 0 }
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`)))
	if rec.Code != http.StatusTooManyRequests || calls != maxRateLimitRetries+1 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upstream_rate_limited") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestRequestStartLogOmitsSensitiveHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
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
	req.Header.Set("Authorization", "Bearer should-not-appear")
	req.Header.Set("Cookie", "session=should-not-appear")
	req.Header.Set("X-API-Key", "should-not-appear")
	req.Header.Set("User-Agent", "nanorouter-test")
	r.Handler().ServeHTTP(httptest.NewRecorder(), req)
	entries := observed.FilterMessage("request_started").All()
	if len(entries) != 1 {
		t.Fatalf("logs=%v", observed.All())
	}
	headers, ok := entries[0].ContextMap()["request_headers"].(map[string][]string)
	if !ok {
		t.Fatalf("headers=%#v", entries[0].ContextMap()["request_headers"])
	}
	if _, found := headers["Authorization"]; found {
		t.Fatalf("authorization leaked: %v", headers)
	}
	if _, found := headers["Cookie"]; found {
		t.Fatalf("cookie leaked: %v", headers)
	}
	if _, found := headers["X-Api-Key"]; found {
		t.Fatalf("api key leaked: %v", headers)
	}
	if got := headers["User-Agent"]; len(got) != 1 || got[0] != "nanorouter-test" {
		t.Fatalf("safe header missing: %v", headers)
	}
}

func TestExposedCandidateReturnsItsUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"model access denied","type":"permission_error","code":"model_not_found"}}`))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"pool": {Candidates: []Candidate{{ID: "only", BaseURL: upstream.URL, Model: "up", ExposeAs: "direct"}}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"direct","messages":[]}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	want := `{"error":{"message":"model access denied","type":"permission_error","code":"model_not_found"}}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body=%s", got)
	}
}

func TestExposedCandidateDoesNotRetryRateLimit(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"pool": {Candidates: []Candidate{{ID: "only", BaseURL: upstream.URL, Model: "up", ExposeAs: "direct"}}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"direct","messages":[]}`)))
	if rec.Code != http.StatusTooManyRequests || calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rate limited") {
		t.Fatalf("body=%s", rec.Body.String())
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

func TestCandidateSpecificMaxTokensLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["max_tokens"] != float64(65536) {
			t.Fatalf("max_tokens=%v", in["max_tokens"])
		}
		_, _ = w.Write([]byte(`{"id":"ok","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{ID: "sense", BaseURL: upstream.URL, Model: "sensenova-6.7-flash-lite", MaxTokensLimit: 65536}
	r, err := New(Config{Routes: map[string]Route{"auto": {Candidates: []Candidate{candidate}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"auto","messages":[{"role":"user","content":"hi"}],"max_tokens":131072}`
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdaptedResponsesUsesCandidateMaxTokensLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["max_new_tokens"] != float64(65536) {
			t.Fatalf("max_new_tokens=%v", in["max_new_tokens"])
		}
		_, _ = w.Write([]byte(`{"data":{"id":"sense-1","choices":[{"message":"ok","finish_reason":"stop"}]},"status":{"code":0}}`))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{
		ID: "sense", BaseURL: upstream.URL, Model: "sensenova-6.7-flash-lite",
		ResponsesViaChatCompletions: true, ChatMaxTokensField: "max_new_tokens", MaxTokensLimit: 65536,
	}
	r, err := New(Config{Routes: map[string]Route{"auto": {Candidates: []Candidate{candidate}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"auto","input":"hi","max_output_tokens":131072}`
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
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
	var modelList struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
		} `json:"data"`
	}
	if err := json.Unmarshal(models.Body.Bytes(), &modelList); err != nil {
		t.Fatal(err)
	}
	for _, model := range modelList.Data {
		if model.Created <= 0 {
			t.Fatalf("model %q has invalid created timestamp: %d", model.ID, model.Created)
		}
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

func TestReasoningContentStrippedFromAssistantMessages(t *testing.T) {
	// 模拟 Mistral 等不支持 reasoning_content 的上游；收到该字段返回 422。
	var gotMessages []any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		gotMessages, _ = in["messages"].([]any)
		for _, raw := range gotMessages {
			msg, _ := raw.(map[string]any)
			if _, ok := msg["reasoning_content"]; ok {
				// 模拟 Mistral 422：extra_forbidden
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"detail":[{"type":"extra_forbidden","msg":"Extra inputs are not permitted"}]}`))
				return
			}
		}
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{
		ID:                "mistral",
		BaseURL:           upstream.URL,
		Model:             "mistral-medium-3-5",
		DropMessageFields: []string{"reasoning_content"},
	}
	r, err := New(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{candidate}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	// 对话历史中含有 DeepSeek 思考模式产生的 reasoning_content 字段。
	body := `{"model":"free","messages":[` +
		`{"role":"user","content":"9.11 和 9.8 哪个更大？"},` +
		`{"role":"assistant","content":"9.8 更大。","reasoning_content":"用户在比较两个小数..."}` +
		`]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 验证上游实际收到的消息中不含 reasoning_content。
	if len(gotMessages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(gotMessages))
	}
	assistant, _ := gotMessages[1].(map[string]any)
	if _, ok := assistant["reasoning_content"]; ok {
		t.Fatal("reasoning_content was not stripped from assistant message")
	}
	if assistant["content"] != "9.8 更大。" {
		t.Fatalf("content was altered: %v", assistant["content"])
	}
}
