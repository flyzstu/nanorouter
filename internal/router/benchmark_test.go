package router

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	json "github.com/goccy/go-json"
)

// BenchmarkGatewayProtocols 覆盖真实 HTTP 上游、选路、schema 转换、SSE 和账本热路径。
func BenchmarkGatewayProtocols(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		if streamed, _ := body["stream"].(bool); streamed {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\ndata: [DONE]\n\n"))
			return
		}
		if tools, _ := body["tools"].([]any); len(tools) > 0 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"id\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":8,"completion_tokens":4,"total_tokens":12}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`))
	}))
	defer upstream.Close()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		b.Fatal(err)
	}
	r, err := New(Config{
		Database: DatabaseConfig{Driver: "sqlite", AsyncWrite: true, BatchSize: 100, QueueSize: 65536, FlushInterval: 100 * time.Millisecond},
		Routes: map[string]Route{"bench": {Candidates: []Candidate{{
			ID: "upstream", BaseURL: upstream.URL, Model: "provider-model", ResponsesViaChatCompletions: true,
		}}}},
	}, db)
	if err != nil {
		b.Fatal(err)
	}
	handler := r.Handler()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.Close(ctx)
	}()

	cases := []struct {
		name string
		path string
		body string
	}{
		{name: "chat", path: "/v1/chat/completions", body: `{"model":"bench","messages":[{"role":"user","content":"hello"}]}`},
		{name: "responses", path: "/v1/responses", body: `{"model":"bench","input":"hello"}`},
		{name: "chat_sse", path: "/v1/chat/completions", body: `{"model":"bench","messages":[{"role":"user","content":"hello"}],"stream":true}`},
		{name: "responses_tools", path: "/v1/responses", body: `{"model":"bench","input":"lookup 1","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}}]}`},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					rec := httptest.NewRecorder()
					req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
					handler.ServeHTTP(rec, req)
					if rec.Code != http.StatusOK {
						b.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
					}
				}
			})
		})
	}
}
