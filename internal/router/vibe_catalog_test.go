package router

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	json "github.com/goccy/go-json"
	"go.uber.org/zap"
)

const vibeCatalogFixture = `{
  "models": [
    {"id": "mistral-large-4-0", "publisher": "mistral", "aliases": ["mistral-large-4"],
     "max_context_length": 1048576, "requires": null,
     "parameters": {"reasoning": ["low", "medium", "high"]},
     "capabilities": {"function_calling": true}},
    {"id": "zai-glm-5-3-flash", "publisher": "mistral", "max_context_length": 1048576,
     "parameters": {"reasoning": ["low", "high", "max"]}},
    {"id": "zai-glm-5-3", "publisher": "zai", "max_context_length": 1048576,
     "parameters": {"reasoning": ["low", "high", "max"]}},
    {"id": "premium-model", "publisher": "mistral", "max_context_length": 65536,
     "requires": {"kind": "higher_plan"}},
    {"id": "no-tools", "publisher": "mistral", "max_context_length": 8192,
     "capabilities": {"function_calling": false}},
    {"id": "", "publisher": "mistral"},
    {"id": "mistral-large-4-0", "publisher": "mistral", "max_context_length": 1}
  ]
}`

func TestParseVibeCatalogFiltersUnpublishedModels(t *testing.T) {
	models, err := parseVibeCatalog([]byte(vibeCatalogFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("models=%+v", models)
	}
	if models[0].id != "mistral-large-4-0" || models[0].contextLength != 1048576 {
		t.Fatalf("first=%+v", models[0])
	}
	if strings.Join(models[0].reasoningLevels, ",") != "low,medium,high" {
		t.Fatalf("levels=%v", models[0].reasoningLevels)
	}
	if len(models[0].aliases) != 1 || models[0].aliases[0] != "mistral-large-4" {
		t.Fatalf("aliases=%v", models[0].aliases)
	}
	if models[1].id != "zai-glm-5-3" {
		t.Fatalf("second=%+v", models[1])
	}
	if models[2].id != "zai-glm-5-3-flash" {
		t.Fatalf("third=%+v", models[2])
	}
}

// vibeCatalogUpstream 记录转发请求的 model 字段，并同时提供目录与 chat 端点。
func vibeCatalogUpstream(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	seen := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if strings.HasSuffix(q.URL.Path, "/model-catalog") {
			if q.URL.Query().Get("surface") != "vibe-code" {
				t.Errorf("surface=%q", q.URL.Query().Get("surface"))
			}
			if auth := q.Header.Get("Authorization"); auth != "Bearer test-key" {
				t.Errorf("authorization=%q", auth)
			}
			_, _ = w.Write([]byte(vibeCatalogFixture))
			return
		}
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(q.Body).Decode(&payload)
		mu.Lock()
		seen = payload.Model
		mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	return server, func() string {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

func newVibeCatalogRouter(t *testing.T, baseURL string) *Router {
	t.Helper()
	t.Setenv("VIBE_CATALOG_KEY", "test-key")
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := NewWithLogger(Config{
		Routes: map[string]Route{"mistral": {Candidates: []Candidate{{
			ID:            "vibe",
			BaseURL:       baseURL,
			APIKeyEnv:     "VIBE_CATALOG_KEY",
			VibeCatalog:   true,
			MistralCompat: true,
		}}}},
	}, db, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r
}

func TestVibeCatalogPublishesModelsAndRoutes(t *testing.T) {
	upstream, lastModel := vibeCatalogUpstream(t)
	defer upstream.Close()
	r := newVibeCatalogRouter(t, upstream.URL)
	if ok := r.refreshVibeCatalog(context.Background()); !ok {
		t.Fatal("refresh failed")
	}

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var list struct {
		Data []struct {
			ID       string         `json:"id"`
			Metadata map[string]any `json:"metadata"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	metadata := make(map[string]map[string]any, len(list.Data))
	for _, entry := range list.Data {
		metadata[entry.ID] = entry.Metadata
	}
	if _, ok := metadata["premium-model"]; ok {
		t.Fatalf("plan-gated entry leaked: %v", metadata)
	}
	if _, ok := metadata["no-tools"]; ok {
		t.Fatalf("non-function-calling entry leaked: %v", metadata)
	}
	meta := metadata["mistral-large-4-0"]
	if meta == nil || meta["context_length"] != float64(1048576) {
		t.Fatalf("metadata=%v", metadata)
	}
	levels, _ := meta["supported_reasoning_levels"].([]any)
	if len(levels) != 3 {
		t.Fatalf("levels=%v", meta)
	}

	// 目录里的 id 与别名都必须能直接路由，并把线上 model 换成目录规范 id。
	for _, requested := range []string{"mistral-large-4-0", "mistral-large-4", "zai-glm-5-3-flash"} {
		rec = httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"`+requested+`","messages":[{"role":"user","content":"hi"}]}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("model %s status=%d body=%s", requested, rec.Code, rec.Body.String())
		}
		want := requested
		if requested == "mistral-large-4" {
			want = "mistral-large-4-0"
		}
		if got := lastModel(); got != want {
			t.Fatalf("requested %s upstream model=%q want %q", requested, got, want)
		}
	}
}

func TestVibeCatalogFailureLeavesRoutingUntouched(t *testing.T) {
	// 目录不可达（连接拒绝）：fail-open 只意味着网关照常启动，不意味着凭空放出模型。
	r := newVibeCatalogRouter(t, "http://127.0.0.1:1/v1")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mistral-large-4-0","messages":[]}`)))
	if rec.Code == http.StatusOK {
		t.Fatalf("unexpected routing before catalog load: %s", rec.Body.String())
	}
}

func TestMistralExampleConfigLoadsCatalogCandidate(t *testing.T) {
	cfg, err := LoadConfig("../../config.example.mistral.yaml")
	if err != nil {
		t.Fatalf("load example: %v", err)
	}
	route, ok := cfg.Routes["mistral-vibe"]
	if !ok || len(route.Candidates) == 0 {
		t.Fatalf("routes=%v", cfg.Routes)
	}
	candidate := route.Candidates[0]
	if !candidate.VibeCatalog || !candidate.MistralCompat || !candidate.ResponsesViaChatCompletions {
		t.Fatalf("candidate=%+v", candidate)
	}
	if candidate.Model != "" {
		t.Fatalf("catalog candidate should not need a placeholder model, got %q", candidate.Model)
	}
	if strings.Join(candidate.DropMessageFields, ",") != "reasoning_content" {
		t.Fatalf("drop_message_fields=%v", candidate.DropMessageFields)
	}
	if strings.Join(candidate.DropRequestFields, ",") != "store" {
		t.Fatalf("drop_request_fields=%v", candidate.DropRequestFields)
	}
}

func TestVibeCatalogValidation(t *testing.T) {
	cases := []struct {
		name      string
		candidate Candidate
		wantErr   string
	}{
		{"missing base", Candidate{ID: "v", VibeCatalog: true}, "requires base_url"},
		{"anthropic", Candidate{ID: "v", VibeCatalog: true, BaseURL: "https://api.mistral.ai/v1", Anthropic: true}, "conflicts with anthropic"},
		{"llama server", Candidate{ID: "v", VibeCatalog: true, BaseURL: "http://127.0.0.1:8080", LlamaServer: true}, "llama_server"},
		{"negative context", Candidate{ID: "v", BaseURL: "https://api.mistral.ai/v1", ContextLength: -1}, "negative context_length"},
		{"native responses", Candidate{ID: "v", VibeCatalog: true, BaseURL: "https://api.mistral.ai/v1", APIKeyEnv: "K", Responses: true}, "responses"},
		{"no credential", Candidate{ID: "v", VibeCatalog: true, BaseURL: "https://api.mistral.ai/v1"}, "requires a Mistral credential"},
	}
	zero := 0
	global := RetryConfig{MaxRateLimitRetries: &zero}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.candidate.validate(global)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v want substring %q", err, tc.wantErr)
			}
		})
	}
	if err := (Candidate{ID: "v", VibeCatalog: true, BaseURL: "https://api.mistral.ai/v1", APIKeyEnv: "K"}).validate(global); err != nil {
		t.Fatalf("valid candidate rejected: %v", err)
	}

}

func TestVibeCatalogAliasDoesNotShadowCanonicalID(t *testing.T) {
	// 排序后 aaa 先出现，其别名 bbb 不能抢走 bbb 自己的规范 id。
	r := newVibeCatalogRouter(t, "http://127.0.0.1:1/v1")
	r.applyVibeCatalog([]vibeRouteEntry{
		{model: vibeModel{id: "aaa", aliases: []string{"bbb"}}, candidate: Candidate{ID: "z"}},
		{model: vibeModel{id: "bbb"}, candidate: Candidate{ID: "a"}},
	})
	entry, ok := r.vibeRouteForModel("bbb")
	if !ok || entry.Candidates[0].ID != "a" {
		t.Fatalf("bbb routed to %+v", entry)
	}
	if entry, ok := r.vibeRouteForModel("aaa"); !ok || entry.Candidates[0].ID != "z" {
		t.Fatalf("aaa routed to %+v", entry)
	}
}

// switchableCatalog 先返回目录，随后可切换为失败，用来验证 fail-open 回退。
func switchableCatalog(t *testing.T) (*httptest.Server, func() string, func()) {
	t.Helper()
	var mu sync.Mutex
	broken := false
	seen := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if strings.HasSuffix(q.URL.Path, "/model-catalog") {
			mu.Lock()
			down := broken
			mu.Unlock()
			if down {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(vibeCatalogFixture))
			return
		}
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(q.Body).Decode(&payload)
		mu.Lock()
		seen = payload.Model
		mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	breakCatalog := func() {
		mu.Lock()
		broken = true
		mu.Unlock()
	}
	return server, func() string {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}, breakCatalog
}

func TestVibeCatalogFailOpenKeepsPreviousModels(t *testing.T) {
	upstream, lastModel, breakCatalog := switchableCatalog(t)
	defer upstream.Close()
	r := newVibeCatalogRouter(t, upstream.URL)
	if ok := r.refreshVibeCatalog(context.Background()); !ok {
		t.Fatal("initial refresh failed")
	}
	before := len(r.vibeModelsSnapshot())
	if before != 3 {
		t.Fatalf("models=%d", before)
	}

	breakCatalog()
	for i := 0; i < 3; i++ {
		if ok := r.refreshVibeCatalog(context.Background()); ok {
			t.Fatal("broken refresh reported success")
		}
	}
	models := r.vibeModelsSnapshot()
	if len(models) != before {
		t.Fatalf("models after failure=%d want %d (%+v)", len(models), before, models)
	}
	if _, ok := r.vibeRouteForModel("mistral-large-4"); !ok {
		t.Fatal("alias route lost after failure")
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mistral-large-4","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := lastModel(); got != "mistral-large-4-0" {
		t.Fatalf("upstream model=%q", got)
	}
}

func TestVibeCatalogExplicitRouteBeatsAlias(t *testing.T) {
	upstream, lastModel := vibeCatalogUpstream(t)
	defer upstream.Close()
	t.Setenv("VIBE_CATALOG_KEY", "test-key")
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := NewWithLogger(Config{
		Routes: map[string]Route{
			// 显式路由名与目录别名同名：显式配置必须赢。
			"mistral-large-4": {Candidates: []Candidate{{
				ID: "manual", BaseURL: upstream.URL, APIKeyEnv: "VIBE_CATALOG_KEY", Model: "explicit-wins",
			}}},
			"mistral": {Candidates: []Candidate{{
				ID: "vibe", BaseURL: upstream.URL, APIKeyEnv: "VIBE_CATALOG_KEY",
				VibeCatalog: true, MistralCompat: true,
			}}},
		},
	}, db, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	if ok := r.refreshVibeCatalog(context.Background()); !ok {
		t.Fatal("refresh failed")
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mistral-large-4","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 显式路由的候选映射优先；若被目录别名抢走，上游会收到 mistral-large-4-0。
	if got := lastModel(); got != "explicit-wins" {
		t.Fatalf("upstream model=%q want explicit route mapping", got)
	}
}

func TestVibeCatalogFailureRedactsCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"bad key test-key"}`))
	}))
	defer upstream.Close()
	r := newVibeCatalogRouter(t, upstream.URL)
	_, err := r.fetchVibeCatalog(context.Background(), r.candidates["vibe"])
	if err == nil || strings.Contains(err.Error(), "test-key") || !strings.Contains(err.Error(), "***") {
		t.Fatalf("err=%v", err)
	}
}
