package router

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
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

func TestConfigAdvancedReliabilityOptions(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	config := "database:\n  driver: sqlite\n  dsn: ':memory:'\n  async_write: true\n  batch_size: 8\n  queue_size: 32\n  flush_interval: 20ms\nprofiling:\n  enabled: true\nbackpressure:\n  max_inflight: 100\n  max_queue: 20\n  queue_timeout: 250ms\nroutes:\n  r:\n    retry_budget:\n      enabled: true\n      ratio: 0.2\n      burst: 5\n    candidates:\n      - id: u\n        base_url: http://example.test\n        model: m\n        max_concurrency: 10\n        min_concurrency: 2\n        adaptive_concurrency: true\n        target_ttfb: 500ms\n        attempt_timeout: 3s\n        retry:\n          enabled: true\n          max_retries: 2\n          base_delay: 500ms\n          max_delay: 3s\n"
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	u := c.Routes["r"].Candidates[0]
	if !c.Database.AsyncWrite || c.Database.BatchSize != 8 || c.Database.QueueSize != 32 || c.Database.FlushInterval != 20*time.Millisecond || !c.Profiling.Enabled {
		t.Fatalf("database/profiling=%+v %+v", c.Database, c.Profiling)
	}
	if c.Backpressure.MaxInflight != 100 || c.Backpressure.MaxQueue != 20 || c.Backpressure.QueueTimeout != 250*time.Millisecond {
		t.Fatalf("backpressure=%+v", c.Backpressure)
	}
	if budget := c.Routes["r"].RetryBudget; !budget.Enabled || budget.Ratio != 0.2 || budget.Burst != 5 {
		t.Fatalf("retry budget=%+v", budget)
	}
	if !u.AdaptiveConcurrency || u.MinConcurrency != 2 || u.TargetTTFB != 500*time.Millisecond || u.AttemptTimeout != 3*time.Second {
		t.Fatalf("candidate=%+v", u)
	}
	if u.Retry.Enabled == nil || !*u.Retry.Enabled || u.Retry.MaxRetries == nil || *u.Retry.MaxRetries != 2 || u.Retry.BaseDelay != 500*time.Millisecond || u.Retry.MaxDelay != 3*time.Second {
		t.Fatalf("candidate retry=%+v", u.Retry)
	}
}

func TestConfigRetryOptions(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	config := "retry:\n  max_rate_limit_retries: 0\n  base_delay: 250ms\n  max_delay: 2s\nroutes:\n  r:\n    candidates:\n      - id: u\n        base_url: http://example.test\n        model: m\n"
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Retry.MaxRateLimitRetries == nil || *c.Retry.MaxRateLimitRetries != 0 || c.Retry.BaseDelay != 250*time.Millisecond || c.Retry.MaxDelay != 2*time.Second {
		t.Fatalf("retry=%+v", c.Retry)
	}
}

func TestConfigRejectsInvalidRetryOptions(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	config := "retry:\n  max_rate_limit_retries: -1\n  base_delay: 2s\n  max_delay: 1s\nroutes:\n  r:\n    candidates:\n      - id: u\n        base_url: http://example.test\n        model: m\n"
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("error=%v", err)
	}
}

func TestConfigAggregateEnabled(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	config := "routes:\n  r:\n    candidates:\n      - id: u\n        base_url: http://example.test\n        model: m\n        expose_as: direct-u\n        aggregate_enabled: false\n"
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate := c.Routes["r"].Candidates[0]
	if candidate.IsAggregateEnabled() {
		t.Fatal("aggregate_enabled=false was not loaded")
	}
	if !candidate.IsEnabled() {
		t.Fatal("candidate should remain enabled when enabled is omitted")
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

func TestSessionAffinityPinsRequestsToCandidate(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"first"}`))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"second"}`))
	}))
	defer second.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"auto": {Candidates: []Candidate{
		{ID: "first", BaseURL: first.URL, Model: "first"},
		{ID: "second", BaseURL: second.URL, Model: "second"},
	}}}}, db)
	if err != nil {
		t.Fatal(err)
	}

	var pinned string
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[]}`))
		req.Header.Set(sessionAffinityHeader, "session-123")
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s", i, rec.Code, rec.Body.String())
		}
		selected := rec.Header().Get("X-Nanorouter-Upstream")
		if i == 0 {
			pinned = selected
		} else if selected != pinned {
			t.Fatalf("request %d selected %q, want pinned upstream %q", i, selected, pinned)
		}
	}
}

func TestMissingSessionAffinityUsesWeightedRoundRobin(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"auto": {Candidates: []Candidate{
		{ID: "first", BaseURL: upstream.URL, Model: "first"},
		{ID: "second", BaseURL: upstream.URL, Model: "second"},
	}}}}, db)
	if err != nil {
		t.Fatal(err)
	}

	selected := make([]string, 0, 2)
	for _, affinity := range []string{"", "   "} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[]}`))
		if affinity != "" {
			req.Header.Set(sessionAffinityHeader, affinity)
		}
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("affinity %q status=%d body=%s", affinity, rec.Code, rec.Body.String())
		}
		selected = append(selected, rec.Header().Get("X-Nanorouter-Upstream"))
	}
	if selected[0] != "first" || selected[1] != "second" {
		t.Fatalf("selected=%v, want weighted round robin", selected)
	}
}

func TestSessionAffinityAllowsFailover(t *testing.T) {
	var failed, succeeded int
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		failed++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		succeeded++
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer good.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"auto": {Candidates: []Candidate{
		{ID: "bad", BaseURL: bad.URL, Model: "bad"},
		{ID: "good", BaseURL: good.URL, Model: "good"},
	}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	affinity := ""
	for i := 0; ; i++ {
		candidate := fmt.Sprintf("session-%d", i)
		if affinitySlot("auto", candidate, 2) == 0 {
			affinity = candidate
			break
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[]}`))
	req.Header.Set(sessionAffinityHeader, affinity)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || failed != 1 || succeeded != 1 {
		t.Fatalf("status=%d failed=%d succeeded=%d body=%s", rec.Code, failed, succeeded, rec.Body.String())
	}
	if rec.Header().Get("X-Nanorouter-Attempts") != "2" {
		t.Fatalf("attempts=%q", rec.Header().Get("X-Nanorouter-Attempts"))
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

func TestCandidateAttemptTimeoutFailsOver(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-req.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	}))
	defer slow.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ok","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer good.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{RequestTimeout: time.Second, Routes: map[string]Route{"free": {Candidates: []Candidate{
		{ID: "slow", BaseURL: slow.URL, Model: "slow", AttemptTimeout: 20 * time.Millisecond},
		{ID: "good", BaseURL: good.URL, Model: "good", AttemptTimeout: 200 * time.Millisecond},
	}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`)))
	if rec.Code != http.StatusOK || rec.Header().Get("X-Nanorouter-Upstream") != "good" || rec.Header().Get("X-Nanorouter-Attempts") != "2" {
		t.Fatalf("status=%d headers=%#v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("failover took %s", elapsed)
	}
	var kind string
	if err := db.QueryRow(`SELECT error_kind FROM llm_requests WHERE upstream_id = 'slow'`).Scan(&kind); err != nil || kind != "attempt_timeout" {
		t.Fatalf("error_kind=%q err=%v", kind, err)
	}
}

func TestCandidateAttemptTimeoutTerminatesAdaptedSSE(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"))
		if flush, ok := w.(http.Flusher); ok {
			flush.Flush()
		}
		select {
		case <-req.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{RequestTimeout: time.Second, Routes: map[string]Route{"free": {Candidates: []Candidate{{
		ID: "slow-stream", BaseURL: upstream.URL, Model: "up", ResponsesViaChatCompletions: true, AttemptTimeout: 20 * time.Millisecond,
	}}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"free","input":"hello","stream":true}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"delta":"partial"`) || !strings.Contains(rec.Body.String(), "event: error") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		t.Fatalf("stream timeout took %s", elapsed)
	}
	if calls.Load() != 1 {
		t.Fatalf("streaming request retried after downstream bytes: calls=%d", calls.Load())
	}
}

func TestHalfOpenCircuitAllowsSingleProbe(t *testing.T) {
	r := &Router{states: map[string]*state{"recovering": {until: time.Now().Add(-time.Second), failures: 2}}, next: map[string]int{}}
	route := Route{Candidates: []Candidate{{ID: "recovering"}}}
	probe, ok := r.pick("route", route, "chat_completions", nil, "")
	if !ok || probe.ID != "recovering" {
		t.Fatalf("probe=%#v ok=%v", probe, ok)
	}
	if _, ok := r.pick("route", route, "chat_completions", nil, ""); ok {
		t.Fatal("second concurrent half-open probe should be rejected")
	}
	r.success(probe)
	if _, ok := r.pick("route", route, "chat_completions", nil, ""); !ok {
		t.Fatal("successful probe should close the circuit")
	}
}

func TestAdaptiveLimiterAIMD(t *testing.T) {
	limiter := newCandidateLimiter(Candidate{MaxConcurrency: 8, MinConcurrency: 2, AdaptiveConcurrency: true, TargetTTFB: 100 * time.Millisecond})
	if got := limiter.currentLimit(); got != 2 {
		t.Fatalf("initial limit=%d", got)
	}
	if got := limiter.observe(http.StatusOK, 50*time.Millisecond, true); got != 2 {
		t.Fatalf("limit after one success=%d", got)
	}
	if got := limiter.observe(http.StatusOK, 50*time.Millisecond, true); got != 3 {
		t.Fatalf("additive increase limit=%d", got)
	}
	if got := limiter.observe(http.StatusTooManyRequests, 10*time.Millisecond, true); got != 2 {
		t.Fatalf("multiplicative decrease limit=%d", got)
	}
	for i := 0; i < 5; i++ {
		if got := limiter.observe(http.StatusTooManyRequests, 10*time.Millisecond, true); got < 2 {
			t.Fatalf("limit fell below minimum: %d", got)
		}
	}
}

func TestAsyncSQLiteStoreFlushesOnClose(t *testing.T) {
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	store := NewStoreWithConfig(db, DatabaseConfig{Driver: "sqlite", AsyncWrite: true, BatchSize: 4, QueueSize: 32, FlushInterval: time.Hour})
	for i := 0; i < 11; i++ {
		if err := store.Insert(context.Background(), Record{ID: fmt.Sprintf("async-%d", i), Route: "r", UpstreamID: "u", UpstreamModel: "m", Endpoint: "chat_completions", Usage: Usage{Source: "unavailable"}}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := store.Close(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM llm_requests`).Scan(&count); err != nil || count != 11 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if err := store.Insert(context.Background(), Record{ID: "late"}); !errors.Is(err, errLedgerClosed) {
		t.Fatalf("insert after close error=%v", err)
	}
}

func TestPprofIsOptIn(t *testing.T) {
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	base := Config{Routes: map[string]Route{"r": {Candidates: []Candidate{{ID: "u", BaseURL: "http://example.test", Model: "m"}}}}}
	disabled, _ := New(base, db)
	rec := httptest.NewRecorder()
	disabled.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/goroutine", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled pprof status=%d", rec.Code)
	}
	base.Profiling.Enabled = true
	enabled, _ := New(base, db)
	rec = httptest.NewRecorder()
	enabled.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/goroutine?debug=1", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "goroutine profile") {
		t.Fatalf("enabled pprof status=%d body=%s", rec.Code, rec.Body.String())
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

func TestJitteredExponentialBackoffStaysWithinEqualJitterWindow(t *testing.T) {
	for retry := 1; retry <= maxRateLimitRetries; retry++ {
		upper := exponentialBackoff(retry)
		lower := upper / 2
		for sample := 0; sample < 100; sample++ {
			got := jitteredExponentialBackoff(retry, time.Second, 16*time.Second)
			if got < lower || got > upper {
				t.Fatalf("retry=%d backoff=%s outside [%s,%s]", retry, got, lower, upper)
			}
		}
	}
}

func TestRateLimitWaitsForEarliestRecoveringCandidate(t *testing.T) {
	makeLimited := func(retryAfter string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", retryAfter)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
		}))
	}
	slow := makeLimited("60")
	defer slow.Close()
	fast := makeLimited("1")
	defer fast.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{
		{ID: "slow", BaseURL: slow.URL, Model: "slow"},
		{ID: "fast", BaseURL: fast.URL, Model: "fast"},
	}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	r.retryBackoff = func(int) time.Duration { return time.Second }
	var waited time.Duration
	r.retryWait = func(_ context.Context, delay time.Duration) bool {
		waited = delay
		return false
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`)))
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if waited < 900*time.Millisecond || waited > 1100*time.Millisecond {
		t.Fatalf("waited=%s, want earliest candidate near 1s rather than 60s", waited)
	}
}

func TestUpstreamClientUsesGatewayConnectionPool(t *testing.T) {
	client := newUpstreamHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport=%T", client.Transport)
	}
	if transport.MaxIdleConns != upstreamMaxIdleConns || transport.MaxIdleConnsPerHost != upstreamMaxIdleConnsPerHost {
		t.Fatalf("connection pool total=%d per_host=%d", transport.MaxIdleConns, transport.MaxIdleConnsPerHost)
	}
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("HTTP/2 should remain enabled")
	}
}

func TestMetricsExposeQueueTTFBRetryAndInflight(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
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
	r, err := New(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{{ID: "u", BaseURL: upstream.URL, Model: "up"}}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	r.retryBackoff = func(int) time.Duration { return 0 }
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	metrics := httptest.NewRecorder()
	r.Handler().ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, name := range []string{
		"nanorouter_upstream_ttfb_seconds",
		"nanorouter_upstream_queue_seconds",
		"nanorouter_retry_delay_seconds",
		"nanorouter_upstream_inflight",
	} {
		if !strings.Contains(metrics.Body.String(), name) {
			t.Fatalf("missing metric %q:\n%s", name, metrics.Body.String())
		}
	}
	if !strings.Contains(metrics.Body.String(), `nanorouter_upstream_inflight{endpoint="chat_completions",route="free",upstream="u"} 0`) {
		t.Fatalf("inflight gauge did not return to zero:\n%s", metrics.Body.String())
	}
}

func TestLargeWeightsDoNotChangeWeightedSelection(t *testing.T) {
	r := &Router{states: map[string]*state{}, next: map[string]int{}}
	route := Route{Candidates: []Candidate{{ID: "bulk", Weight: 1_000_000}, {ID: "rare", Weight: 1}}}
	for i := 0; i < 10; i++ {
		got, ok := r.pick("large", route, "chat_completions", nil, "")
		if !ok || got.ID != "bulk" {
			t.Fatalf("pick %d=%#v ok=%v", i, got, ok)
		}
	}
	r.next["large"] = 1_000_000
	got, ok := r.pick("large", route, "chat_completions", nil, "")
	if !ok || got.ID != "rare" {
		t.Fatalf("boundary pick=%#v ok=%v", got, ok)
	}
}

func BenchmarkPickLargeWeights(b *testing.B) {
	r := &Router{states: map[string]*state{}, next: map[string]int{}}
	route := Route{Candidates: []Candidate{{ID: "bulk", Weight: 1_000_000}, {ID: "rare", Weight: 1}}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := r.pick("large", route, "chat_completions", nil, ""); !ok {
			b.Fatal("candidate not selected")
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

func TestRequestStartLogUsesReadableClientMetadata(t *testing.T) {
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
	req.Header.Set("X-Forwarded-For", "203.0.113.8, 10.0.0.2")
	req.Header.Set("X-Real-IP", "10.0.0.3")
	req.Header.Set("X-Session-Affinity", "should-not-appear")
	r.Handler().ServeHTTP(httptest.NewRecorder(), req)
	entries := observed.FilterMessage("request_started").All()
	if len(entries) != 1 {
		t.Fatalf("logs=%v", observed.All())
	}
	fields := entries[0].ContextMap()
	if _, found := fields["request_headers"]; found {
		t.Fatalf("request header map should not be logged: %v", fields)
	}
	if fields["client_ip"] != "203.0.113.8" {
		t.Fatalf("client_ip=%v", fields["client_ip"])
	}
	if fields["user_agent"] != "nanorouter-test" {
		t.Fatalf("user_agent=%v", fields["user_agent"])
	}
	if fields["session_affinity"] != true {
		t.Fatalf("session_affinity=%v", fields["session_affinity"])
	}
	encoded := fmt.Sprint(fields)
	for _, secret := range []string{"should-not-appear", "Bearer"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("sensitive value %q leaked: %v", secret, fields)
		}
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

func TestExposedCandidateCanDisableRateLimitRetry(t *testing.T) {
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
	disabled := false
	r, err := New(Config{Routes: map[string]Route{"pool": {Candidates: []Candidate{{ID: "only", BaseURL: upstream.URL, Model: "up", ExposeAs: "direct", Retry: CandidateRetryConfig{Enabled: &disabled}}}}}}, db)
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

func TestResponsesAdapterRejectsBuiltInTools(t *testing.T) {
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{{ID: "sense", BaseURL: "http://127.0.0.1:1", Model: "SenseChat", ResponsesViaChatCompletions: true}}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"free","input":"hello","tools":[{"type":"web_search"}]}`))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "tool type") {
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

func TestAggregateDisabledCandidateIsOnlyRoutableThroughExposeAs(t *testing.T) {
	aggregateEnabled := false
	var pooledRequests, directRequests int
	pooled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pooledRequests++
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer pooled.Close()
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		directRequests++
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer direct.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	exposeOnly := Candidate{
		ID:               "direct",
		BaseURL:          direct.URL,
		Model:            "direct-upstream",
		ExposeAs:         "direct-model",
		AggregateEnabled: &aggregateEnabled,
	}
	r, err := New(Config{Routes: map[string]Route{
		"free": {
			Candidates: []Candidate{
				{ID: "pooled", BaseURL: pooled.URL, Model: "pooled-upstream"},
				exposeOnly,
			},
		},
		"expose-only-parent": {Candidates: []Candidate{{
			ID:               "sole-direct",
			BaseURL:          direct.URL,
			Model:            "direct-upstream",
			ExposeAs:         "sole-direct-model",
			AggregateEnabled: &aggregateEnabled,
		}}},
	}}, db)
	if err != nil {
		t.Fatal(err)
	}

	models := httptest.NewRecorder()
	r.Handler().ServeHTTP(models, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !strings.Contains(models.Body.String(), `"id":"free"`) ||
		!strings.Contains(models.Body.String(), `"id":"direct-model"`) {
		t.Fatalf("expected aggregate and exposed models: %s", models.Body.String())
	}
	if strings.Contains(models.Body.String(), `"id":"expose-only-parent"`) {
		t.Fatalf("empty aggregate route leaked: %s", models.Body.String())
	}

	pooledRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`))
	pooledResponse := httptest.NewRecorder()
	r.Handler().ServeHTTP(pooledResponse, pooledRequest)
	if pooledResponse.Code != http.StatusOK {
		t.Fatalf("pooled status=%d body=%s", pooledResponse.Code, pooledResponse.Body.String())
	}
	if pooledRequests != 1 || directRequests != 0 {
		t.Fatalf("after pooled request: pooled=%d direct=%d", pooledRequests, directRequests)
	}

	directRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"direct-model","messages":[]}`))
	directResponse := httptest.NewRecorder()
	r.Handler().ServeHTTP(directResponse, directRequest)
	if directResponse.Code != http.StatusOK {
		t.Fatalf("direct status=%d body=%s", directResponse.Code, directResponse.Body.String())
	}
	if pooledRequests != 1 || directRequests != 1 {
		t.Fatalf("after direct request: pooled=%d direct=%d", pooledRequests, directRequests)
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
