package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// modelsNames 读一次 /v1/models 并返回 id → name，没有 name 的 id 不出现在结果里。
func modelsNames(t *testing.T, r *Router) map[string]string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("models status=%d body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]string, len(list.Data))
	for _, entry := range list.Data {
		if entry.Name != "" {
			names[entry.ID] = entry.Name
		}
	}
	return names
}

func TestModelsPublishesRouteLabelAsName(t *testing.T) {
	r := newMetadataRouter(t, map[string]Route{
		"glm-5.4":    {Label: "GLM-5.4 (Zcode)", Candidates: []Candidate{{ID: "z1", BaseURL: "https://up.test", Model: "m"}}},
		"unlabelled": {Candidates: []Candidate{{ID: "u1", BaseURL: "https://up.test", Model: "m"}}},
		// 只有空白的 label 等同于没配置。
		"blank": {Label: "   ", Candidates: []Candidate{{ID: "b1", BaseURL: "https://up.test", Model: "m"}}},
	})
	names := modelsNames(t, r)
	if names["glm-5.4"] != "GLM-5.4 (Zcode)" {
		t.Fatalf("glm-5.4 name=%q", names["glm-5.4"])
	}
	for _, id := range []string{"unlabelled", "blank"} {
		if name, ok := names[id]; ok {
			t.Fatalf("%s must not publish a name, got %q", id, name)
		}
	}
}

func TestExposeAsPublishesItsOwnLabelAndFallsBackToTheRoute(t *testing.T) {
	r := newMetadataRouter(t, map[string]Route{
		"agnes-2-5": {
			// 一条路由只放一个 expose_as 时，label 写在路由上最省事。
			Label: "Agnes 2.5 Flash",
			Candidates: []Candidate{{
				ID: "a1", BaseURL: "https://up.test", Model: "m", ExposeAs: "agnes/agnes-2.5-flash",
			}},
		},
		"pair": {Candidates: []Candidate{
			// 候选标签优先于父路由标签。
			{ID: "p1", BaseURL: "https://up.test", Model: "m", ExposeAs: "labelled", Label: "Labelled Model"},
			{ID: "p2", BaseURL: "https://up.test", Model: "m", ExposeAs: "bare"},
			// 关闭的候选整体不发布，标签也不发布。
			{ID: "p3", BaseURL: "https://up.test", Model: "m", ExposeAs: "off", Label: "Off Model", Enabled: new(false)},
		}},
	})
	names := modelsNames(t, r)
	if names["agnes/agnes-2.5-flash"] != "Agnes 2.5 Flash" {
		t.Fatalf("route-label fallback missing: %v", names)
	}
	if names["labelled"] != "Labelled Model" {
		t.Fatalf("candidate label should win: %v", names)
	}
	for _, id := range []string{"pair", "bare", "off"} {
		if name, ok := names[id]; ok {
			t.Fatalf("%s must not publish a name, got %q", id, name)
		}
	}
}

func TestCatalogLabelBecomesTheNameUnlessConfigOverridesIt(t *testing.T) {
	// 显式配置与目录同名时，配置的 label 先占位（目录只补空缺），所以目录永远
	// 不能改写运维已经声明的展示名。
	r := newMetadataRouter(t, map[string]Route{
		"mistral-large-4-0": {
			Label:      "Mistral Large 4.0 (pinned)",
			Candidates: []Candidate{{ID: "v1", BaseURL: "https://up.test", Model: "m"}},
		},
	})
	models, err := parseVibeCatalog([]byte(`{"models":[
		{"id":"mistral-large-4-0","label":"Mistral Large 4","max_context_length":524288},
		{"id":"zai-glm-5-3","label":"  GLM 5.3 (Mistral Hosted)  "},
		{"id":"no-label"}
	]}`))
	if err != nil {
		t.Fatalf("parse catalog: %v", err)
	}
	r.vibeMu.Lock()
	r.vibeModels = models
	r.vibeRoutes = map[string]vibeRouteEntry{}
	r.vibeMu.Unlock()

	names := modelsNames(t, r)
	if got := names["mistral-large-4-0"]; got != "Mistral Large 4.0 (pinned)" {
		t.Fatalf("config label must win over the catalog, got %q", got)
	}
	// 目录 label 两端的空白被裁掉。
	if got := names["zai-glm-5-3"]; got != "GLM 5.3 (Mistral Hosted)" {
		t.Fatalf("zai-glm-5-3 name=%q", got)
	}
	if got, ok := names["no-label"]; ok {
		t.Fatalf("no-label must not publish a name, got %q", got)
	}
}
