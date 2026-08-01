package router

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	json "github.com/goccy/go-json"
)

type parsedResponseEvent struct {
	name string
	data map[string]any
}

func parseResponseEvents(t *testing.T, stream string) []parsedResponseEvent {
	t.Helper()
	blocks := strings.Split(strings.TrimSpace(stream), "\n\n")
	events := make([]parsedResponseEvent, 0, len(blocks))
	for _, block := range blocks {
		var name, payload string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				payload = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" || payload == "" {
			t.Fatalf("invalid SSE block %q", block)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(payload), &data); err != nil {
			t.Fatalf("decode %s event: %v\npayload=%s", name, err, payload)
		}
		events = append(events, parsedResponseEvent{name: name, data: data})
	}
	return events
}

func findResponseEvent(t *testing.T, events []parsedResponseEvent, name string) parsedResponseEvent {
	t.Helper()
	for _, event := range events {
		if event.name == name {
			return event
		}
	}
	t.Fatalf("event %q not found in %#v", name, events)
	return parsedResponseEvent{}
}

func TestResponsesAdapterConvertsFunctionsAndStructuredOutput(t *testing.T) {
	var in map[string]any
	body := `{
		"input":[
			{"type":"function_call","call_id":"call_1","name":"weather","arguments":"{\"city\":\"Paris\"}"},
			{"type":"function_call_output","call_id":"call_1","output":{"temperature":21}},
			{"role":"user","content":[{"type":"input_text","text":"Summarize it"}]}
		],
		"tools":[{"type":"function","name":"weather","description":"Get weather","parameters":{"type":"object"},"strict":true}],
		"tool_choice":{"type":"function","name":"weather"},
		"parallel_tool_calls":false,
		"reasoning":{"effort":"medium","summary":"none"},
		"top_logprobs":3,
		"text":{"verbosity":"low","format":{"type":"json_schema","name":"answer","strict":true,"schema":{"type":"object"}}}
	}`
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatal(err)
	}
	out, err := responsesToChatCompletions(in, "")
	if err != nil {
		t.Fatal(err)
	}
	messages := out["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages=%#v", messages)
	}
	assistant := messages[0].(map[string]any)
	if assistant["role"] != "assistant" || len(assistant["tool_calls"].([]any)) != 1 {
		t.Fatalf("assistant=%#v", assistant)
	}
	toolResult := messages[1].(map[string]any)
	if toolResult["role"] != "tool" || toolResult["tool_call_id"] != "call_1" || toolResult["content"] != `{"temperature":21}` {
		t.Fatalf("tool result=%#v", toolResult)
	}
	tools := out["tools"].([]any)
	function := tools[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "weather" || function["strict"] != true {
		t.Fatalf("tools=%#v", tools)
	}
	format := out["response_format"].(map[string]any)
	if format["type"] != "json_schema" || format["json_schema"].(map[string]any)["name"] != "answer" {
		t.Fatalf("response_format=%#v", format)
	}
	if out["reasoning_effort"] != "medium" || out["verbosity"] != "low" || out["logprobs"] != true || out["top_logprobs"] != float64(3) {
		t.Fatalf("mapped parameters=%#v", out)
	}
}

func TestChatCompletionToResponseConvertsToolCallsAndIncompleteStatus(t *testing.T) {
	body := []byte(`{
		"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"length"}],
		"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}
	}`)
	outBody, err := chatCompletionToResponse(body, "public-model")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(outBody, &out); err != nil {
		t.Fatal(err)
	}
	if out["status"] != "incomplete" || out["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Fatalf("status=%#v details=%#v", out["status"], out["incomplete_details"])
	}
	items := out["output"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["type"] != "function_call" || items[0].(map[string]any)["call_id"] != "call_1" {
		t.Fatalf("output=%#v", items)
	}
}

func TestStreamChatAsResponsesProducesCompleteTextAndUsage(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hel"},"finish_reason":null}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(stream))}
	rec := httptest.NewRecorder()
	usage, err := (&Router{}).streamChatAsResponses(rec, resp, "public-model")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Total != 3 {
		t.Fatalf("usage=%#v", usage)
	}
	got := rec.Body.String()
	for _, want := range []string{
		"event: response.output_item.added",
		`"type":"response.output_text.done"`,
		`"text":"hello"`,
		`"total_tokens":3`,
		"event: response.completed",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in stream:\n%s", want, got)
		}
	}
}

func TestStreamChatAsResponsesConvertsFunctionCall(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_7","type":"function","function":{"name":"weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		"",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":"tool_calls"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(stream))}
	rec := httptest.NewRecorder()
	if _, err := (&Router{}).streamChatAsResponses(rec, resp, "public-model"); err != nil {
		t.Fatal(err)
	}
	got := rec.Body.String()
	for _, want := range []string{
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		`"call_id":"call_7"`,
		`"name":"weather"`,
		`"arguments":"{\"city\":\"Paris\"}"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in stream:\n%s", want, got)
		}
	}
}

func TestResponsesAdapterComplexAgentTranscript(t *testing.T) {
	var in map[string]any
	body := `{
		"instructions":"Follow policy and cite tool results.",
		"input":[
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"Answer as JSON."}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Compare these places"},{"type":"input_image","image_url":"https://example.test/map.png","detail":"high"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I will collect both data points."}]},
			{"type":"function_call","call_id":"call_weather","name":"get_weather","arguments":"{\"city\":\"Paris\"}"},
			{"type":"function_call","call_id":"call_fx","name":"get_fx","arguments":"{\"base\":\"EUR\",\"quote\":\"USD\"}"},
			{"type":"function_call_output","call_id":"call_weather","output":{"temp_c":21,"conditions":["sunny","dry"]}},
			{"type":"function_call_output","call_id":"call_fx","output":"{\"rate\":1.08}"},
			{"type":"message","role":"user","content":"Return the final comparison."}
		],
		"tools":[
			{"type":"function","name":"get_weather","description":"Current weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false},"strict":true},
			{"type":"function","name":"get_fx","description":"Current FX rate","parameters":{"type":"object","properties":{"base":{"type":"string"},"quote":{"type":"string"}},"required":["base","quote"],"additionalProperties":false},"strict":true}
		],
		"tool_choice":"required",
		"parallel_tool_calls":true,
		"reasoning":{"effort":"high","summary":"none"},
		"temperature":0.2,
		"top_p":0.9,
		"max_output_tokens":900,
		"stream":true,
		"text":{"verbosity":"high","format":{"type":"json_schema","name":"comparison","description":"Final comparison","strict":true,"schema":{"type":"object","properties":{"summary":{"type":"string"},"facts":{"type":"array","items":{"type":"string"}}},"required":["summary","facts"],"additionalProperties":false}}}
	}`
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatal(err)
	}
	out, err := responsesToChatCompletions(in, "max_completion_tokens")
	if err != nil {
		t.Fatal(err)
	}
	messages := out["messages"].([]any)
	if len(messages) != 8 {
		t.Fatalf("message count=%d messages=%#v", len(messages), messages)
	}
	wantRoles := []string{"system", "developer", "user", "assistant", "assistant", "tool", "tool", "user"}
	for i, want := range wantRoles {
		if got := messages[i].(map[string]any)["role"]; got != want {
			t.Fatalf("message[%d].role=%v want=%s", i, got, want)
		}
	}
	imageParts := messages[2].(map[string]any)["content"].([]any)
	imageURL := imageParts[1].(map[string]any)["image_url"].(map[string]any)
	if imageURL["url"] != "https://example.test/map.png" || imageURL["detail"] != "high" {
		t.Fatalf("image_url=%#v", imageURL)
	}
	calls := messages[4].(map[string]any)["tool_calls"].([]any)
	if len(calls) != 2 || calls[0].(map[string]any)["id"] != "call_weather" || calls[1].(map[string]any)["id"] != "call_fx" {
		t.Fatalf("parallel calls=%#v", calls)
	}
	if got := messages[5].(map[string]any)["content"]; got != `{"conditions":["sunny","dry"],"temp_c":21}` {
		t.Fatalf("object tool output=%v", got)
	}
	if out["max_completion_tokens"] != float64(900) || out["reasoning_effort"] != "high" || out["verbosity"] != "high" {
		t.Fatalf("mapped controls=%#v", out)
	}
	if out["stream_options"].(map[string]any)["include_usage"] != true {
		t.Fatalf("stream_options=%#v", out["stream_options"])
	}
	format := out["response_format"].(map[string]any)["json_schema"].(map[string]any)
	if format["name"] != "comparison" || format["description"] != "Final comparison" || format["strict"] != true {
		t.Fatalf("json schema=%#v", format)
	}
}

func TestStreamChatAsResponsesInterleavesTextParallelCallsAndTrailingUsage(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_search","type":"function","function":{"name":"search_docs","arguments":"{\"q\":"}}]},"finish_reason":null}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"Working "},"finish_reason":null}]}`,
		"",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}},{"index":1,"function":{"arguments":"\"Responses API\"}"}}]},"finish_reason":null}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"done","tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":"tool_calls"}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":123,"completion_tokens":45,"total_tokens":168}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(stream))}
	rec := httptest.NewRecorder()
	usage, err := (&Router{}).streamChatAsResponses(rec, resp, "agent-route")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Prompt != 123 || usage.Completion != 45 || usage.Total != 168 || usage.Source != "upstream" {
		t.Fatalf("usage=%#v", usage)
	}
	events := parseResponseEvents(t, rec.Body.String())
	counts := map[string]int{}
	for _, event := range events {
		counts[event.name]++
	}
	if counts["response.created"] != 1 || counts["response.output_item.added"] != 3 || counts["response.function_call_arguments.done"] != 2 || counts["response.completed"] != 1 {
		t.Fatalf("event counts=%#v\nstream=%s", counts, rec.Body.String())
	}
	completed := findResponseEvent(t, events, "response.completed")
	response := completed.data["response"].(map[string]any)
	if response["status"] != "completed" || response["model"] != "agent-route" {
		t.Fatalf("response=%#v", response)
	}
	items := response["output"].([]any)
	if len(items) != 3 {
		t.Fatalf("output=%#v", items)
	}
	assertFunction := func(index int, callID, name, arguments string) {
		t.Helper()
		item := items[index].(map[string]any)
		if item["type"] != "function_call" || item["call_id"] != callID || item["name"] != name || item["arguments"] != arguments {
			t.Fatalf("output[%d]=%#v", index, item)
		}
	}
	assertFunction(0, "call_search", "search_docs", `{"q":"Responses API"}`)
	message := items[1].(map[string]any)
	text := message["content"].([]any)[0].(map[string]any)["text"]
	if message["type"] != "message" || text != "Working done" {
		t.Fatalf("message item=%#v", message)
	}
	assertFunction(2, "call_weather", "get_weather", `{"city":"Paris"}`)
	responseUsage := response["usage"].(map[string]any)
	if responseUsage["total_tokens"] != float64(168) {
		t.Fatalf("response usage=%#v", responseUsage)
	}
	if got := fmt.Sprint(events[len(events)-1].name); got != "response.completed" {
		t.Fatalf("last event=%s", got)
	}
}

func TestResponsesAdapterRejectsComplexUnsupportedFeatures(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "hosted web search", body: `{"input":"hi","tools":[{"type":"web_search"}]}`, want: `tool type "web_search"`},
		{name: "hosted MCP", body: `{"input":"hi","tools":[{"type":"mcp","server_label":"docs","server_url":"https://example.test/mcp"}]}`, want: `tool type "mcp"`},
		{name: "response chain", body: `{"input":"next","previous_response_id":"resp_previous"}`, want: "previous_response_id"},
		{name: "background task", body: `{"input":"long job","background":true}`, want: "background"},
		{name: "reasoning summary", body: `{"input":"think","reasoning":{"effort":"high","summary":"detailed"}}`, want: "reasoning.summary"},
		{name: "reasoning item replay", body: `{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`, want: `input item type "reasoning"`},
		{name: "file input", body: `{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file_1"}]}]}`, want: `content part type "input_file"`},
		{name: "allowed tools choice", body: `{"input":"hi","tool_choice":{"type":"allowed_tools","mode":"auto","tools":[]}}`, want: `tool_choice type "allowed_tools"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var in map[string]any
			if err := json.Unmarshal([]byte(tt.body), &in); err != nil {
				t.Fatal(err)
			}
			_, err := responsesToChatCompletions(in, "")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v want substring %q", err, tt.want)
			}
		})
	}
}

func TestResponsesAdapterEndToEndParallelFunctionRoundTrip(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/chat/completions" {
			t.Errorf("upstream path=%s", req.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
			return
		}
		if body["model"] != "provider-agent-model" || body["parallel_tool_calls"] != true {
			t.Errorf("upstream controls=%#v", body)
		}
		tools, _ := body["tools"].([]any)
		if len(tools) != 2 {
			t.Errorf("upstream tools=%#v", tools)
		}
		messages, _ := body["messages"].([]any)
		if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" || messages[1].(map[string]any)["role"] != "user" {
			t.Errorf("upstream messages=%#v", messages)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-provider-1",
			"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},
				{"id":"call_fx","type":"function","function":{"name":"get_fx","arguments":"{\"base\":\"EUR\",\"quote\":\"USD\"}"}}
			]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":40,"completion_tokens":11,"total_tokens":51}
		}`))
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
	r, err := New(Config{Routes: map[string]Route{
		"public-agent": {Candidates: []Candidate{{
			ID: "provider", BaseURL: upstream.URL, Model: "provider-agent-model", ResponsesViaChatCompletions: true,
		}}},
	}}, db)
	if err != nil {
		t.Fatal(err)
	}

	body := `{
		"model":"public-agent",
		"instructions":"Use both tools before answering.",
		"input":"Compare Paris weather with EUR/USD.",
		"parallel_tool_calls":true,
		"tool_choice":"required",
		"tools":[
			{"type":"function","name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}},
			{"type":"function","name":"get_fx","parameters":{"type":"object","properties":{"base":{"type":"string"},"quote":{"type":"string"}},"required":["base","quote"]}}
		]
	}`
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Nanorouter-Route") != "public-agent" || rec.Header().Get("X-Nanorouter-Upstream") != "provider" {
		t.Fatalf("routing headers=%#v", rec.Header())
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["object"] != "response" || response["model"] != "public-agent" || response["status"] != "completed" {
		t.Fatalf("response=%#v", response)
	}
	output := response["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output=%#v", output)
	}
	for i, want := range []struct {
		callID string
		name   string
	}{{"call_weather", "get_weather"}, {"call_fx", "get_fx"}} {
		item := output[i].(map[string]any)
		if item["type"] != "function_call" || item["call_id"] != want.callID || item["name"] != want.name || item["status"] != "completed" {
			t.Fatalf("output[%d]=%#v", i, item)
		}
	}
	var tokens int
	if err := db.QueryRow(`SELECT total_tokens FROM llm_requests`).Scan(&tokens); err != nil || tokens != 51 {
		t.Fatalf("recorded total_tokens=%d err=%v", tokens, err)
	}
}
