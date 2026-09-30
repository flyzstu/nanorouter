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
		{Route: "free", UpstreamID: "u1", UpstreamModel: "glm-5.3", Endpoint: "chat_completions", Status: 200, LatencyMS: 100, Usage: Usage{Prompt: 10, Completion: 20, Total: 30, Source: "upstream"}, ToolsCount: 2, ToolNames: `["read_file","bash"]`, ClientIP: "10.0.0.1", ClientAgent: "claude-code/1.0", CreatedAt: now},
		{Route: "free", UpstreamID: "u1", UpstreamModel: "glm-5.3", Endpoint: "chat_completions", Status: 200, LatencyMS: 300, Usage: Usage{Prompt: 1, Completion: 2, Total: 3, Source: "upstream"}, ClientIP: "10.0.0.1", ClientAgent: "claude-code/1.0", CreatedAt: now.Add(-time.Minute)},
		{Route: "free", UpstreamID: "u1", UpstreamModel: "glm-5.3", Endpoint: "chat_completions", Status: 502, LatencyMS: 500, ErrorKind: "upstream_error", Usage: Usage{Source: "unavailable"}, ClientIP: "10.0.0.2", ClientAgent: "curl/8.0", CreatedAt: now.Add(-2 * time.Minute)},
		{Route: "free", UpstreamID: "u1", UpstreamModel: "glm-5.3", Endpoint: "chat_completions", Status: 499, LatencyMS: 10, ErrorKind: "client_canceled", Usage: Usage{Source: "unavailable"}, ClientIP: "10.0.0.2", ClientAgent: "curl/8.0", CreatedAt: now.Add(-3 * time.Minute)},
		{Route: "free", UpstreamID: "u2", UpstreamModel: "hermes-4", Endpoint: "chat_completions", Status: 200, LatencyMS: 700, Usage: Usage{Prompt: 5, Completion: 5, Total: 10, Source: "upstream"}, ToolNames: `["read_file"]`, ClientIP: "10.0.0.3", ClientAgent: "opencode/1.2", CreatedAt: now.Add(-4 * time.Minute)},
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
	if summary.Totals.Requests != 5 || summary.Totals.Success != 3 {
		t.Fatalf("totals: requests=%d success=%d", summary.Totals.Requests, summary.Totals.Success)
	}
	if summary.Totals.TotalTokens != 43 || summary.Totals.PromptTokens != 16 || summary.Totals.CompletionTokens != 27 {
		t.Fatalf("tokens: %+v", summary.Totals)
	}
	if summary.Totals.WithTools != 1 {
		t.Fatalf("with_tools=%d", summary.Totals.WithTools)
	}
	// 延迟样本 [100,300,500,10,700] 排序后 [10,100,300,500,700]：p50=(5-1)/2=2 → 300，p95=(5-1)*95/100=3 → 500。
	if summary.Totals.LatencyP50MS != 300 || summary.Totals.LatencyP95MS != 500 {
		t.Fatalf("percentiles: p50=%d p95=%d", summary.Totals.LatencyP50MS, summary.Totals.LatencyP95MS)
	}
	if len(summary.Models) != 2 || summary.Models[0].Calls != 4 || summary.Models[0].Success != 2 || summary.Models[0].TotalTokens != 33 {
		t.Fatalf("models: %+v", summary.Models)
	}
	if len(summary.Failures) != 2 {
		t.Fatalf("failures: %+v", summary.Failures)
	}
	if summary.Failures[0].Count != 1 || summary.Failures[1].Count != 1 {
		t.Fatalf("failures: %+v", summary.Failures)
	}
	// 桶以小时前缀聚合，5 条同小时记录落在同一个桶。
	if len(summary.Series) != 1 || summary.Series[0].Calls != 5 || summary.Series[0].Failures != 2 {
		t.Fatalf("series: %+v", summary.Series)
	}
	// 上游候选：u1 四条（2 成功）、u2 一条（1 成功），按调用量降序。
	if len(summary.Upstreams) != 2 || summary.Upstreams[0].UpstreamID != "u1" || summary.Upstreams[0].Calls != 4 || summary.Upstreams[0].Success != 2 {
		t.Fatalf("upstreams: %+v", summary.Upstreams)
	}
	if summary.Upstreams[1].UpstreamID != "u2" || summary.Upstreams[1].TotalTokens != 10 {
		t.Fatalf("upstreams[1]: %+v", summary.Upstreams[1])
	}
	// 调用方：10.0.0.1 两条全成、10.0.0.2 两条全败、10.0.0.3 一条。
	if len(summary.Clients) != 3 {
		t.Fatalf("clients: %+v", summary.Clients)
	}
	clientByIP := map[string]StatsClient{}
	for _, c := range summary.Clients {
		clientByIP[c.ClientIP] = c
	}
	if c := clientByIP["10.0.0.2"]; c.Calls != 2 || c.Success != 0 || c.LastSeen == "" {
		t.Fatalf("client 10.0.0.2: %+v", c)
	}
	if c := clientByIP["10.0.0.3"]; c.TotalTokens != 10 {
		t.Fatalf("client 10.0.0.3: %+v", c)
	}
	// 客户端 UA 分组：claude-code 2、curl 2、opencode 1。
	agentCalls := map[string]int64{}
	for _, a := range summary.Agents {
		agentCalls[a.Agent] = a.Calls
	}
	if agentCalls["claude-code/1.0"] != 2 || agentCalls["curl/8.0"] != 2 || agentCalls["opencode/1.2"] != 1 {
		t.Fatalf("agents: %+v", summary.Agents)
	}
	// 工具榜：read_file 出现在两条记录共 3 次、bash 1 次，按次数降序。
	if len(summary.Tools) != 2 || summary.Tools[0].Name != "read_file" || summary.Tools[0].Count != 2 || summary.Tools[1].Name != "bash" {
		t.Fatalf("tools: %+v", summary.Tools)
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
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("User-Agent", "test-agent/9.9")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
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
	// 元数据三件套：RemoteAddr 去端口、UA 透传、工具名 JSON 数组。
	if len(summary.Clients) != 1 || summary.Clients[0].ClientIP == "" {
		t.Fatalf("clients: %+v", summary.Clients)
	}
	// httptest.NewRequest 的 RemoteAddr 是 192.0.2.1:1234，应去掉端口。
	if summary.Clients[0].ClientIP != "192.0.2.1" {
		t.Fatalf("client ip=%q", summary.Clients[0].ClientIP)
	}
	if len(summary.Agents) != 1 || summary.Agents[0].Agent != "test-agent/9.9" {
		t.Fatalf("agents: %+v", summary.Agents)
	}
	if len(summary.Tools) != 2 || summary.Tools[0].Name != "a" || summary.Tools[1].Name != "b" {
		t.Fatalf("tools: %+v", summary.Tools)
	}
}

func TestProxyClientIPTrustForwardedFor(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	build := func(trust bool) *Router {
		t.Helper()
		db, err := sql.Open("sqlite3", ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		if err := Migrate(context.Background(), db); err != nil {
			t.Fatal(err)
		}
		r, err := New(Config{RequestTimeout: time.Second, TrustForwardedFor: trust, Routes: map[string]Route{"free": {Candidates: []Candidate{{
			ID: "u", BaseURL: upstream.URL, Model: "up",
		}}}}}, db)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	post := func(r *Router) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.9")
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("proxy: %d", rec.Code)
		}
	}

	rTrue := build(true)
	post(rTrue)
	summary, err := rTrue.db.Summary(context.Background(), time.Now().Add(-time.Hour), 13)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Clients) != 1 || summary.Clients[0].ClientIP != "203.0.113.7" {
		t.Fatalf("trusted XFF should record first hop: %+v", summary.Clients)
	}

	rFalse := build(false)
	post(rFalse)
	summary, err = rFalse.db.Summary(context.Background(), time.Now().Add(-time.Hour), 13)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Clients) != 1 || summary.Clients[0].ClientIP != "192.0.2.1" {
		t.Fatalf("untrusted XFF must fall back to RemoteAddr: %+v", summary.Clients)
	}
}
