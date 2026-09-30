package router

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

func statsTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return NewStore(db, "sqlite3")
}

func insertStatsRecord(t *testing.T, store *Store, r Record) {
	t.Helper()
	if err := store.Insert(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func TestSummaryAggregatesTotalsRateAndTools(t *testing.T) {
	store := statsTestStore(t)
	now := time.Now().UTC()
	records := []Record{
		{Route: "free", UpstreamID: "u1", UpstreamModel: "glm-5.3", Endpoint: "chat_completions", Status: 200, LatencyMS: 100, Usage: Usage{Prompt: 10, Completion: 20, Total: 30, Source: "upstream"}, ToolsCount: 2, CreatedAt: now},
		{Route: "free", UpstreamID: "u1", UpstreamModel: "glm-5.3", Endpoint: "chat_completions", Status: 200, LatencyMS: 300, Usage: Usage{Prompt: 1, Completion: 2, Total: 3, Source: "upstream"}, CreatedAt: now.Add(-time.Minute)},
		{Route: "free", UpstreamID: "u1", UpstreamModel: "glm-5.3", Endpoint: "chat_completions", Status: 502, LatencyMS: 500, ErrorKind: "upstream_error", Usage: Usage{Source: "unavailable"}, CreatedAt: now.Add(-2 * time.Minute)},
		{Route: "free", UpstreamID: "u1", UpstreamModel: "glm-5.3", Endpoint: "chat_completions", Status: 499, LatencyMS: 10, ErrorKind: "client_canceled", Usage: Usage{Source: "unavailable"}, CreatedAt: now.Add(-3 * time.Minute)},
		// 范围之外：不应计入任何聚合。
		{Route: "old", UpstreamID: "u1", UpstreamModel: "glm-5.3", Endpoint: "chat_completions", Status: 500, LatencyMS: 999, ErrorKind: "upstream_error", CreatedAt: now.Add(-25 * time.Hour)},
	}
	for i, r := range records {
		if r.ID == "" {
			r.ID = fmt.Sprintf("stat-%d", i)
		}
		insertStatsRecord(t, store, r)
	}
	summary, err := store.Summary(context.Background(), now.Add(-24*time.Hour), 13)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Totals.Requests != 4 || summary.Totals.Success != 2 {
		t.Fatalf("totals: requests=%d success=%d", summary.Totals.Requests, summary.Totals.Success)
	}
	if summary.Totals.TotalTokens != 33 || summary.Totals.PromptTokens != 11 || summary.Totals.CompletionTokens != 22 {
		t.Fatalf("tokens: %+v", summary.Totals)
	}
	if summary.Totals.WithTools != 1 {
		t.Fatalf("with_tools=%d", summary.Totals.WithTools)
	}
	// 延迟样本 [100,300,500,10] 排序后 [10,100,300,500]：p50=(4-1)/2=1 → 100，p95=(4-1)*95/100=2 → 300。
	if summary.Totals.LatencyP50MS != 100 || summary.Totals.LatencyP95MS != 300 {
		t.Fatalf("percentiles: p50=%d p95=%d", summary.Totals.LatencyP50MS, summary.Totals.LatencyP95MS)
	}
	if len(summary.Models) != 1 || summary.Models[0].Calls != 4 || summary.Models[0].Success != 2 || summary.Models[0].TotalTokens != 33 {
		t.Fatalf("models: %+v", summary.Models)
	}
	if len(summary.Failures) != 2 {
		t.Fatalf("failures: %+v", summary.Failures)
	}
	if summary.Failures[0].Count != 1 || summary.Failures[1].Count != 1 {
		t.Fatalf("failures: %+v", summary.Failures)
	}
	// 桶以小时前缀聚合，4 条同小时记录落在同一个桶。
	if len(summary.Series) != 1 || summary.Series[0].Calls != 4 || summary.Series[0].Failures != 2 {
		t.Fatalf("series: %+v", summary.Series)
	}
}

func TestMigrateAddsToolsCountToLegacyTable(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	legacy := `CREATE TABLE llm_requests (
 id TEXT PRIMARY KEY, created_at TEXT NOT NULL, route TEXT NOT NULL, upstream_id TEXT NOT NULL, upstream_model TEXT NOT NULL,
 endpoint TEXT NOT NULL, status_code INTEGER NOT NULL, latency_ms INTEGER NOT NULL, streamed INTEGER NOT NULL, attempt INTEGER NOT NULL,
 prompt_tokens BIGINT, completion_tokens BIGINT, total_tokens BIGINT, usage_source TEXT NOT NULL, error_kind TEXT
);`
	if _, err := db.Exec(legacy); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate over legacy table: %v", err)
	}
	// 二次迁移同样必须幂等。
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	store := NewStore(db, "sqlite3")
	insertStatsRecord(t, store, Record{Route: "r", UpstreamID: "u", UpstreamModel: "m", Endpoint: "chat_completions", Status: 200, ToolsCount: 3, CreatedAt: time.Now().UTC()})
	var tools int
	if err := db.QueryRow(`SELECT tools_count FROM llm_requests`).Scan(&tools); err != nil {
		t.Fatalf("tools_count column unreadable: %v", err)
	}
	if tools != 3 {
		t.Fatalf("tools=%d", tools)
	}
}

func TestStatsEndpointsAuthAndPayload(t *testing.T) {
	t.Setenv("NANOROUTER_TEST_KEY", "sekrit")
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{AuthEnv: "NANOROUTER_TEST_KEY", RequestTimeout: time.Second}, db)
	if err != nil {
		t.Fatal(err)
	}
	handler := r.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/stats/summary", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("summary without auth: %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("missing WWW-Authenticate challenge")
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/stats/summary?range=bogus", nil)
	req.SetBasicAuth("anything", "sekrit")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("summary with basic auth: %d body=%s", rec.Code, rec.Body.String())
	}
	// 非法 range 回退 24h；响应是合法 JSON 且带分桶字段。
	var payload struct {
		Range  string `json:"range"`
		Bucket string `json:"bucket"`
		Series []struct {
			Bucket string `json:"bucket"`
		} `json:"series"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("summary body: %v", err)
	}
	if payload.Range != "24h" || payload.Bucket != "1h" || len(payload.Series) != 24 {
		t.Fatalf("summary envelope: range=%s bucket=%s points=%d", payload.Range, payload.Bucket, len(payload.Series))
	}

	// key 放用户名位同样放行（浏览器登录框首格）。
	req = httptest.NewRequest(http.MethodGet, "/stats", nil)
	req.SetBasicAuth("sekrit", "ignored")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "nanorouter") {
		t.Fatalf("stats page: %d", rec.Code)
	}

	// 错误凭据拒绝；生成端点不接受 Basic（认证面不放宽）。
	req = httptest.NewRequest(http.MethodGet, "/stats", nil)
	req.SetBasicAuth("x", "wrong")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("stats with wrong creds: %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.SetBasicAuth("sekrit", "x")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("basic must not open generation endpoints: %d", rec.Code)
	}
}

func TestProxyLedgerRecordsToolsCount(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
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
	r, err := New(Config{RequestTimeout: time.Second, Routes: map[string]Route{"free": {Candidates: []Candidate{{
		ID: "u", BaseURL: upstream.URL, Model: "up",
	}}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"free","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"a"}},{"type":"function","function":{"name":"b"}}]}`
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy: %d body=%s", rec.Code, rec.Body.String())
	}
	summary, err := r.db.Summary(context.Background(), time.Now().Add(-time.Hour), 13)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Totals.WithTools != 1 {
		t.Fatalf("with_tools=%d, want 1", summary.Totals.WithTools)
	}
}
