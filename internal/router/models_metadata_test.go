package router

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

// modelsMetadata 读一次 /v1/models 并返回 id → metadata。
func modelsMetadata(t *testing.T, r *Router) map[string]map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("models status=%d body=%s", rec.Code, rec.Body.String())
	}
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
	return metadata
}

// newMetadataRouter 只装路由表，不触网：这些测试断言的是发布面，不是转发。
func newMetadataRouter(t *testing.T, routes map[string]Route) *Router {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := NewWithLogger(Config{Routes: routes}, db, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r
}

func TestModelsPublishesMessagesOnlyForFullyAnthropicRoutes(t *testing.T) {
	r := newMetadataRouter(t, map[string]Route{
		// 全 anthropic：Messages 面与 Chat 面等价。
		"all-claude": {Candidates: []Candidate{
			{ID: "a1", BaseURL: "https://up.test", Model: "m", Anthropic: true},
			{ID: "a2", BaseURL: "https://up.test", Model: "m", Anthropic: true},
		}},
		// 混合：/v1/messages 会丢掉 chat 候选，不能声明。
		"mixed": {Candidates: []Candidate{
			{ID: "b1", BaseURL: "https://up.test", Model: "m", Anthropic: true},
			{ID: "b2", BaseURL: "https://up.test", Model: "m"},
		}},
		// 纯 chat：同样不能声明。
		"chat-only": {Candidates: []Candidate{
			{ID: "c1", BaseURL: "https://up.test", Model: "m"},
		}},
		// 关闭的 anthropic 候选不参与：整条路由只剩 chat 面。
		"disabled-anthropic": {Candidates: []Candidate{
			{ID: "d1", BaseURL: "https://up.test", Model: "m", Anthropic: true, Enabled: new(false)},
			{ID: "d2", BaseURL: "https://up.test", Model: "m"},
		}},
	})
	metadata := modelsMetadata(t, r)

	if metadata["all-claude"]["messages"] != true {
		t.Fatalf("all-anthropic route must publish messages: %v", metadata["all-claude"])
	}
	for _, route := range []string{"mixed", "chat-only", "disabled-anthropic"} {
		if value, ok := metadata[route]["messages"]; ok {
			t.Fatalf("%s must not publish messages, got %v", route, value)
		}
	}
}

func TestModelsKeepsContextAndReasoningAlongsideMessages(t *testing.T) {
	r := newMetadataRouter(t, map[string]Route{
		"claude": {
			ReasoningLevels: []string{"low", "high"},
			Candidates: []Candidate{
				{ID: "c1", BaseURL: "https://up.test", Model: "m", Anthropic: true, ContextLength: 200_000},
				{ID: "c2", BaseURL: "https://up.test", Model: "m", Anthropic: true},
			},
		},
	})
	meta := modelsMetadata(t, r)["claude"]
	if meta["messages"] != true {
		t.Fatalf("messages missing: %v", meta)
	}
	// 上下文窗口仍取所有声明了长度的候选的最小值。
	if meta["context_length"] != float64(200_000) {
		t.Fatalf("context_length=%v", meta["context_length"])
	}
	levels, _ := meta["supported_reasoning_levels"].([]any)
	if len(levels) != 2 {
		t.Fatalf("levels=%v", meta["supported_reasoning_levels"])
	}
}

func TestExposeAsCandidatePublishesItsOwnMessagesCapability(t *testing.T) {
	r := newMetadataRouter(t, map[string]Route{
		"mixed": {
			Candidates: []Candidate{
				{ID: "m1", BaseURL: "https://up.test", Model: "m", Anthropic: true, ExposeAs: "claude-single"},
				{ID: "m2", BaseURL: "https://up.test", Model: "m", ExposeAs: "chat-single"},
			},
		},
	})
	metadata := modelsMetadata(t, r)
	// 单候选路由按候选自身协议判定，不受同路由另一候选影响。
	if metadata["claude-single"]["messages"] != true {
		t.Fatalf("claude-single=%v", metadata["claude-single"])
	}
	if _, ok := metadata["chat-single"]["messages"]; ok {
		t.Fatalf("chat-single must not publish messages: %v", metadata["chat-single"])
	}
	if _, ok := metadata["mixed"]["messages"]; ok {
		t.Fatalf("the aggregate mixed route must not publish messages: %v", metadata["mixed"])
	}
}

func TestModelsPublishesInputModalitiesWhenACandidateAcceptsImages(t *testing.T) {
	r := newMetadataRouter(t, map[string]Route{
		// 只有一个候选收图，但发图请求会被收缩到它，所以整条路由都能收图。
		"partly-vision": {Candidates: []Candidate{
			{ID: "v1", BaseURL: "https://up.test", Model: "m", Vision: true},
			{ID: "v2", BaseURL: "https://up.test", Model: "m"},
		}},
		"text-only": {Candidates: []Candidate{
			{ID: "t1", BaseURL: "https://up.test", Model: "m"},
		}},
	})
	metadata := modelsMetadata(t, r)
	modalities, _ := metadata["partly-vision"]["input_modalities"].([]any)
	if len(modalities) != 2 || modalities[0] != "text" || modalities[1] != "image" {
		t.Fatalf("partly-vision modalities=%v", metadata["partly-vision"])
	}
	if _, ok := metadata["text-only"]["input_modalities"]; ok {
		t.Fatalf("text-only must not publish image input: %v", metadata["text-only"])
	}
}

func TestExposeAsCandidatePublishesItsOwnVisionCapability(t *testing.T) {
	r := newMetadataRouter(t, map[string]Route{
		"mixed": {Candidates: []Candidate{
			{ID: "m1", BaseURL: "https://up.test", Model: "m", Vision: true, ExposeAs: "vision-single"},
			{ID: "m2", BaseURL: "https://up.test", Model: "m", ExposeAs: "text-single"},
		}},
	})
	metadata := modelsMetadata(t, r)
	if _, ok := metadata["vision-single"]["input_modalities"]; !ok {
		t.Fatalf("vision-single=%v", metadata["vision-single"])
	}
	if _, ok := metadata["text-single"]["input_modalities"]; ok {
		t.Fatalf("text-single must not publish image input: %v", metadata["text-single"])
	}
}

func TestPickSkipsCandidatesWithoutVisionForImageRequests(t *testing.T) {
	r := &Router{states: map[string]*state{}, next: map[string]int{}}
	route := Route{Candidates: []Candidate{
		{ID: "text-up", Vision: false},
		{ID: "vision-up", Vision: true},
	}}
	// 带图请求只能落到声明 vision 的候选，即使轮转本该从第一个开始。
	for attempt := 0; attempt < 4; attempt++ {
		got, ok := r.pick("route", route, "chat_completions", nil, "", true)
		if !ok || got.ID != "vision-up" {
			t.Fatalf("attempt %d picked %q ok=%v", attempt, got.ID, ok)
		}
	}
	// 不带图的请求两个候选都可用。
	seen := map[string]bool{}
	for attempt := 0; attempt < 4; attempt++ {
		got, ok := r.pick("route", route, "chat_completions", nil, "", false)
		if !ok {
			t.Fatalf("attempt %d: no candidate", attempt)
		}
		seen[got.ID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("text requests should rotate over both candidates, saw %v", seen)
	}
}

func TestPickFailsWhenAnImageRequestHasNoVisionCandidate(t *testing.T) {
	r := &Router{states: map[string]*state{}, next: map[string]int{}}
	route := Route{Candidates: []Candidate{{ID: "text-up"}}}
	if _, ok := r.pick("route", route, "chat_completions", nil, "", true); ok {
		t.Fatal("an image request must not fall through to a text-only candidate")
	}
}

func TestRequestHasImagesAcrossAllThreeInboundShapes(t *testing.T) {
	imagePart := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA"}}
	cases := []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{"chat text only", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}, false},
		{"chat image part", map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "look"}, imagePart,
		}}}}, true},
		{"responses input_image", map[string]any{"input": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AA"},
		}}}}, true},
		{"responses nested item content", map[string]any{"input": []any{map[string]any{"type": "message", "content": []any{
			map[string]any{"type": "input_image"},
		}}}}, true},
		{"messages image block", map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AA"}},
		}}}}, true},
		{"messages text only", map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "hi"},
		}}}}, false},
		{"no messages key", map[string]any{"model": "x"}, false},
	}
	for _, tt := range cases {
		if got := requestHasImages(tt.payload); got != tt.want {
			t.Fatalf("%s: got %v want %v", tt.name, got, tt.want)
		}
	}
}

func TestRouteHasVisionOnlyCountsAggregateCandidates(t *testing.T) {
	if routeHasVision(Route{Candidates: []Candidate{{ID: "a", Vision: true, Enabled: new(false)}}}) {
		t.Fatal("a disabled vision candidate must not enable image routing")
	}
	if routeHasVision(Route{Candidates: []Candidate{{ID: "a", Vision: true, AggregateEnabled: new(false)}}}) {
		t.Fatal("a non-aggregate vision candidate must not enable image routing")
	}
	if !routeHasVision(Route{Candidates: []Candidate{{ID: "a", Vision: true}}}) {
		t.Fatal("an aggregate vision candidate should enable image routing")
	}
}

func TestModelsDoesNotPublishVisionForDisabledCandidates(t *testing.T) {
	r := newMetadataRouter(t, map[string]Route{
		"disabled-vision": {Candidates: []Candidate{
			{ID: "v1", BaseURL: "https://up.test", Model: "m", Vision: true, Enabled: new(false)},
			{ID: "t1", BaseURL: "https://up.test", Model: "m"},
		}},
	})
	if meta := modelsMetadata(t, r)["disabled-vision"]; meta["input_modalities"] != nil {
		t.Fatalf("a disabled vision candidate must not claim image support: %v", meta)
	}
}

func TestVibeCatalogVisionRequiresBothCatalogAndCandidate(t *testing.T) {
	cases := []struct {
		name        string
		catalog     bool
		candidate   bool
		wantImage   bool
		modelVision string
	}{
		{"both agree", true, true, true, `{"id":"m","capabilities":{"vision":true}}`},
		{"catalog says no", false, true, false, `{"id":"m","capabilities":{"vision":false}}`},
		{"catalog silent", false, true, false, `{"id":"m","capabilities":{}}`},
		{"candidate says no", true, false, false, `{"id":"m","capabilities":{"vision":true}}`},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			models, err := parseVibeCatalog([]byte(`{"models":[` + tt.modelVision + `]}`))
			if err != nil || len(models) != 1 {
				t.Fatalf("parse: models=%v err=%v", models, err)
			}
			// The catalog's per-model fact must be true for the model to carry it.
			if models[0].vision != tt.catalog {
				t.Fatalf("catalog vision=%v, want %v", models[0].vision, tt.catalog)
			}
			r := &Router{vibeRoutes: map[string]vibeRouteEntry{
				"m": {model: models[0], candidate: Candidate{ID: "c", Vision: tt.candidate}},
			}}
			route, ok := r.vibeRouteForModel("m")
			if !ok {
				t.Fatal("route not found")
			}
			if route.Candidates[0].Vision != tt.wantImage {
				t.Fatalf("route vision=%v, want %v", route.Candidates[0].Vision, tt.wantImage)
			}
		})
	}
}
