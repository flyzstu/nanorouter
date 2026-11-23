package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	json "github.com/goccy/go-json"
)

const mistralThinkingCompletion = `{"id":"cmpl-1","object":"chat.completion","created":1,"model":"mistral-large-4","p":"abcde","choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"thinking","thinking":[{"type":"text","text":"先想"}],"closed":true},{"type":"text","text":"2"}],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"},"index":0}]},"finish_reason":"tool_calls","response_error":null,"logprobs":null}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7,"completion_tokens_details":{"reasoning_tokens":2}},"system_fingerprint":null}`

func TestNormalizeMistralCompletion(t *testing.T) {
	normalized, err := normalizeMistralCompletion([]byte(mistralThinkingCompletion))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(normalized, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["p"]; ok {
		t.Fatalf("private chunk field leaked: %s", normalized)
	}
	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices=%v", out["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	if _, ok := choice["response_error"]; ok {
		t.Fatalf("choice-level response_error leaked: %s", normalized)
	}
	message, _ := choice["message"].(map[string]any)
	if got := stringValue(message["content"]); got != "2" {
		t.Fatalf("content=%q", got)
	}
	if got := stringValue(message["reasoning_content"]); got != "先想" {
		t.Fatalf("reasoning_content=%q", got)
	}
	calls, _ := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls=%v", message["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	if _, ok := call["index"]; ok {
		t.Fatalf("non-stream tool call kept index: %s", normalized)
	}
	usage, _ := out["usage"].(map[string]any)
	if number(usage["total_tokens"]) != 7 {
		t.Fatalf("usage lost: %s", normalized)
	}
	details, _ := usage["completion_tokens_details"].(map[string]any)
	if number(details["reasoning_tokens"]) != 2 {
		t.Fatalf("usage details lost: %s", normalized)
	}
}

func TestNormalizeMistralCompletionThinkingOnlyContentIsNull(t *testing.T) {
	body := `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"thinking","thinking":[{"type":"text","text":"only thinking"}],"closed":true}]},"finish_reason":"length"}]}`
	normalized, err := normalizeMistralCompletion([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(normalized, &out); err != nil {
		t.Fatal(err)
	}
	choices, _ := out["choices"].([]any)
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if content, ok := message["content"]; !ok || content != nil {
		t.Fatalf("content=%v present=%v", content, ok)
	}
	if got := stringValue(message["reasoning_content"]); got != "only thinking" {
		t.Fatalf("reasoning_content=%q", got)
	}
}

func TestNormalizeMistralCompletionIgnoresErrors(t *testing.T) {
	body := []byte(`{"object":"error","message":"bad","type":"invalid_request_invalid_args"}`)
	converted, err := normalizeMistralCompletion(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(converted) != string(body) {
		t.Fatalf("error body rewritten: %s", converted)
	}
}

func TestMistralSSEAdapterAddsUsageFrame(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"cmpl-x","object":"chat.completion.chunk","created":9,"model":"m","p":"a","choices":[{"index":0,"delta":{"role":"assistant","content":[{"type":"thinking","thinking":[{"type":"text","text":"think"}],"closed":true}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"cmpl-x","object":"chat.completion.chunk","created":9,"model":"m","p":"abcde","choices":[{"index":0,"delta":{"content":[{"type":"text","text":"hi"}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"cmpl-x","object":"chat.completion.chunk","created":9,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	adapter := newMistralSSEAdapter(io.NopCloser(strings.NewReader(input)), true)
	output, err := io.ReadAll(adapter)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), `"p":`) {
		t.Fatalf("private field leaked: %s", output)
	}
	frames := sseDataFrames(t, string(output))
	if len(frames) != 5 {
		t.Fatalf("frames=%d: %s", len(frames), output)
	}
	if frames[4] != "[DONE]" {
		t.Fatalf("last frame=%q", frames[4])
	}
	var thinking map[string]any
	if err := json.Unmarshal([]byte(frames[0]), &thinking); err != nil {
		t.Fatal(err)
	}
	choice, _ := thinking["choices"].([]any)[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	if got := stringValue(delta["reasoning_content"]); got != "think" {
		t.Fatalf("reasoning_content=%q frame=%s", got, frames[0])
	}
	if got := stringValue(delta["content"]); got != "" {
		t.Fatalf("content=%q frame=%s", got, frames[0])
	}
	var text map[string]any
	if err := json.Unmarshal([]byte(frames[1]), &text); err != nil {
		t.Fatal(err)
	}
	choice, _ = text["choices"].([]any)[0].(map[string]any)
	delta, _ = choice["delta"].(map[string]any)
	if got := stringValue(delta["content"]); got != "hi" {
		t.Fatalf("content=%q frame=%s", got, frames[1])
	}
	var tail map[string]any
	if err := json.Unmarshal([]byte(frames[3]), &tail); err != nil {
		t.Fatal(err)
	}
	var finish map[string]any
	if err := json.Unmarshal([]byte(frames[2]), &finish); err != nil {
		t.Fatal(err)
	}
	if _, ok := finish["usage"]; ok {
		t.Fatalf("inline usage kept in content frame: %s", frames[2])
	}
	choices, _ := tail["choices"].([]any)
	if choices == nil || len(choices) != 0 {
		t.Fatalf("usage frame choices=%v", tail["choices"])
	}
	usage, _ := tail["usage"].(map[string]any)
	if number(usage["prompt_tokens"]) != 3 || number(usage["completion_tokens"]) != 4 || number(usage["total_tokens"]) != 7 {
		t.Fatalf("usage=%v", tail["usage"])
	}
	if stringValue(tail["id"]) != "cmpl-x" || number(tail["created"]) != 9 || stringValue(tail["model"]) != "m" {
		t.Fatalf("usage frame metadata=%v", tail)
	}
	if got := adapter.Usage(); got.Prompt != 3 || got.Completion != 4 || got.Total != 7 || got.Source != "upstream" {
		t.Fatalf("adapter usage=%+v", got)
	}
}

func TestMistralSSEAdapterStripsUsageWithoutIncludeUsage(t *testing.T) {
	input := "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\ndata: [DONE]\n\n"
	adapter := newMistralSSEAdapter(io.NopCloser(strings.NewReader(input)), false)
	output, err := io.ReadAll(adapter)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), `"usage"`) {
		t.Fatalf("usage emitted without include_usage: %s", output)
	}
	if got := adapter.Usage(); got.Prompt != 1 || got.Completion != 2 || got.Total != 3 {
		t.Fatalf("adapter usage=%+v", got)
	}
}

func TestMistralErrorEnvelope(t *testing.T) {
	body := `{"object":"error","message":"Logprobs are not enabled for this model","type":"invalid_request_invalid_args","param":null,"code":"3051","raw_status_code":400}`
	converted := mistralErrorToOpenAI([]byte(body))
	var out map[string]any
	if err := json.Unmarshal(converted, &out); err != nil {
		t.Fatal(err)
	}
	inner, _ := out["error"].(map[string]any)
	if inner == nil {
		t.Fatalf("no error envelope: %s", converted)
	}
	if got := stringValue(inner["message"]); got != "Logprobs are not enabled for this model" {
		t.Fatalf("message=%q", got)
	}
	if got := stringValue(inner["type"]); got != "invalid_request_invalid_args" {
		t.Fatalf("type=%q", got)
	}
	if got := stringValue(inner["code"]); got != "3051" {
		t.Fatalf("code=%q", got)
	}
	if strings.Contains(string(converted), "raw_status_code") {
		t.Fatalf("raw status leaked into envelope: %s", converted)
	}

	standard := []byte(`{"error":{"message":"x","type":"invalid_request_error"}}`)
	if got := mistralErrorToOpenAI(standard); string(got) != string(standard) {
		t.Fatalf("standard envelope rewritten: %s", got)
	}
}

func TestMistralCompatNonStreamingIntegration(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mistralThinkingCompletion))
	}))
	defer upstream.Close()
	r, _ := newZcodeTestRouter(t, Candidate{
		ID: "mistral", BaseURL: upstream.URL + "/v1", Model: "mistral-large-4", MistralCompat: true,
	})
	rec := postJSON(t, r, "/v1/chat/completions", `{"model":"zcode","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["p"]; ok {
		t.Fatalf("private field leaked: %s", rec.Body.String())
	}
	choice, _ := out["choices"].([]any)[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if got := stringValue(message["content"]); got != "2" {
		t.Fatalf("content=%q body=%s", got, rec.Body.String())
	}
	if got := stringValue(message["reasoning_content"]); got != "先想" {
		t.Fatalf("reasoning_content=%q", got)
	}
	if _, ok := choice["response_error"]; ok {
		t.Fatalf("response_error leaked: %s", rec.Body.String())
	}
}

func TestMistralCompatStreamingIntegration(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, frame := range []string{
			`{"id":"cmpl-x","object":"chat.completion.chunk","created":9,"model":"m","p":"a","choices":[{"index":0,"delta":{"role":"assistant","content":[{"type":"thinking","thinking":[{"type":"text","text":"think"}],"closed":true}]},"finish_reason":null}]}`,
			`{"id":"cmpl-x","object":"chat.completion.chunk","created":9,"model":"m","p":"abcde","choices":[{"index":0,"delta":{"content":[{"type":"text","text":"hi"}]},"finish_reason":null}]}`,
			`{"id":"cmpl-x","object":"chat.completion.chunk","created":9,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`,
			`[DONE]`,
		} {
			_, _ = io.WriteString(w, "data: "+frame+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()
	r, _ := newZcodeTestRouter(t, Candidate{
		ID: "mistral", BaseURL: upstream.URL + "/v1", Model: "mistral-large-4", MistralCompat: true,
	})
	rec := postJSON(t, r, "/v1/chat/completions", `{"model":"zcode","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"p":`) {
		t.Fatalf("private field leaked: %s", rec.Body.String())
	}
	frames := sseDataFrames(t, rec.Body.String())
	if len(frames) != 5 {
		t.Fatalf("frames=%d: %s", len(frames), rec.Body.String())
	}
	if frames[len(frames)-1] != "[DONE]" {
		t.Fatalf("last frame=%q", frames[len(frames)-1])
	}
	var tail map[string]any
	if err := json.Unmarshal([]byte(frames[len(frames)-2]), &tail); err != nil {
		t.Fatal(err)
	}
	usage, _ := tail["usage"].(map[string]any)
	if number(usage["total_tokens"]) != 7 {
		t.Fatalf("usage frame=%s", frames[len(frames)-2])
	}
	choices, _ := tail["choices"].([]any)
	if len(choices) != 0 {
		t.Fatalf("usage frame choices=%v", tail["choices"])
	}
	if !strings.Contains(rec.Body.String(), `"reasoning_content":"think"`) || !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Fatalf("content not normalized: %s", rec.Body.String())
	}
}

func TestMistralCompatDisabledIsTransparent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mistralThinkingCompletion))
	}))
	defer upstream.Close()
	r, _ := newZcodeTestRouter(t, Candidate{ID: "mistral", BaseURL: upstream.URL + "/v1", Model: "mistral-large-4"})
	rec := postJSON(t, r, "/v1/chat/completions", `{"model":"zcode","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"type":"thinking"`) {
		t.Fatalf("body unexpectedly normalized: %s", rec.Body.String())
	}
}

func TestMistralCompatConfigValidation(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	config := "routes:\n  r:\n    candidates:\n      - id: u\n        base_url: http://example.test/v1\n        model: m\n        mistral_compat: true\n        anthropic: true\n"
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "mistral_compat") {
		t.Fatalf("err=%v", err)
	}
}

func sseDataFrames(t *testing.T, body string) []string {
	t.Helper()
	var frames []string
	for _, frame := range strings.Split(body, "\n\n") {
		frame = strings.TrimSpace(frame)
		if !strings.HasPrefix(frame, "data: ") {
			continue
		}
		frames = append(frames, strings.TrimPrefix(frame, "data: "))
	}
	return frames
}

func TestMistralCompatGreedySamplingAddsTopP(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantTop any // nil 表示上游不应收到 top_p
	}{
		{
			name:    "greedy sampling gets top_p",
			body:    `{"model":"zcode","temperature":0,"messages":[{"role":"user","content":"hi"}]}`,
			wantTop: float64(1),
		},
		{
			name:    "explicit top_p is preserved",
			body:    `{"model":"zcode","temperature":0,"top_p":0.5,"messages":[{"role":"user","content":"hi"}]}`,
			wantTop: 0.5,
		},
		{
			name: "nonzero temperature untouched",
			body: `{"model":"zcode","temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "absent temperature untouched",
			body: `{"model":"zcode","messages":[{"role":"user","content":"hi"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen map[string]any
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read body: %v", err)
				}
				if err := json.Unmarshal(raw, &seen); err != nil {
					t.Errorf("decode body: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(mistralThinkingCompletion))
			}))
			defer upstream.Close()
			r, _ := newZcodeTestRouter(t, Candidate{
				ID: "mistral", BaseURL: upstream.URL + "/v1", Model: "mistral-large-4", MistralCompat: true,
			})
			rec := postJSON(t, r, "/v1/chat/completions", tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			got, ok := seen["top_p"]
			if tc.wantTop == nil {
				if ok {
					t.Fatalf("upstream received unexpected top_p=%v", got)
				}
				return
			}
			if !ok {
				t.Fatalf("upstream body missing top_p: %v", seen)
			}
			if gotFloat, ok := got.(float64); !ok || gotFloat != tc.wantTop.(float64) {
				t.Fatalf("top_p=%v want %v", got, tc.wantTop)
			}
		})
	}
}
