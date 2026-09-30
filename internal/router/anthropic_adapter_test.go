package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	json "github.com/goccy/go-json"
)

func mustTranslate(t *testing.T, in string) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(in), &payload); err != nil {
		t.Fatal(err)
	}
	out, err := chatCompletionsToMessages(payload)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	return out
}

func TestChatToMessagesBasicMapping(t *testing.T) {
	out := mustTranslate(t, `{
		"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hello"}],
		"temperature":0.5,"top_p":0.9,"max_tokens":77,"stop":["END","\n\n"],
		"store":true,"metadata":{"x":1},"user":"u1","service_tier":"auto","seed":7
	}`)
	if out["system"] != "be brief" {
		t.Fatalf("system=%v", out["system"])
	}
	if out["max_tokens"] != int64(77) {
		t.Fatalf("max_tokens=%v", out["max_tokens"])
	}
	stops, _ := out["stop_sequences"].([]string)
	if len(stops) != 2 || stops[0] != "END" {
		t.Fatalf("stop_sequences=%v", stops)
	}
	if out["temperature"] != 0.5 || out["top_p"] != 0.9 {
		t.Fatalf("sampling=%v %v", out["temperature"], out["top_p"])
	}
	// 记账字段不进入上游请求。
	for _, key := range []string{"store", "metadata", "user", "service_tier", "seed"} {
		if _, ok := out[key]; ok {
			t.Fatalf("%s must be dropped", key)
		}
	}
	messages, _ := out["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages=%v", messages)
	}
}

func TestChatToMessagesDefaultMaxTokens(t *testing.T) {
	out := mustTranslate(t, `{"messages":[{"role":"user","content":"hi"}]}`)
	if out["max_tokens"] != int64(defaultAnthropicMaxTokens) {
		t.Fatalf("max_tokens=%v", out["max_tokens"])
	}
	// max_completion_tokens 优先级低于 max_tokens 的出现顺序。
	out = mustTranslate(t, `{"messages":[{"role":"user","content":"hi"}],"max_completion_tokens":33}`)
	if out["max_tokens"] != int64(33) {
		t.Fatalf("max_tokens=%v", out["max_tokens"])
	}
}

func TestChatToMessagesToolsAndChoices(t *testing.T) {
	out := mustTranslate(t, `{
		"messages":[{"role":"user","content":"weather?"}],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"lookup","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
		"tool_choice":"required"
	}`)
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", tools)
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "get_weather" {
		t.Fatalf("tool=%v", tool)
	}
	schema, _ := tool["input_schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("input_schema=%v", schema)
	}
	choice, _ := out["tool_choice"].(map[string]any)
	if choice["type"] != "any" {
		t.Fatalf("tool_choice=%v", out["tool_choice"])
	}
	// named tool_choice → {type: tool, name}
	out = mustTranslate(t, `{
		"messages":[{"role":"user","content":"x"}],
		"tools":[{"type":"function","function":{"name":"a","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"a"}}
	}`)
	choice, _ = out["tool_choice"].(map[string]any)
	if choice["type"] != "tool" || choice["name"] != "a" {
		t.Fatalf("tool_choice=%v", choice)
	}
	// tool_choice none → 不携带 tools
	out = mustTranslate(t, `{
		"messages":[{"role":"user","content":"x"}],
		"tools":[{"type":"function","function":{"name":"a","parameters":{"type":"object"}}}],
		"tool_choice":"none"
	}`)
	if _, ok := out["tools"]; ok {
		t.Fatalf("tool_choice=none must drop tools: %v", out["tools"])
	}
}

func TestChatToMessagesToolResultMerging(t *testing.T) {
	out := mustTranslate(t, `{
		"messages":[
			{"role":"user","content":"run both"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"a","arguments":"{\"x\":1}"}},
				{"id":"call_2","type":"function","function":{"name":"b","arguments":"{\"y\":2}"}}
			]},
			{"role":"tool","tool_call_id":"call_1","content":"result one"},
			{"role":"tool","tool_call_id":"call_2","content":"result two"},
			{"role":"user","content":"thanks"}
		]
	}`)
	messages, _ := out["messages"].([]any)
	// user → assistant(tool_use) → user(tool_result x2) → user(正文与 tool_result 合并)
	if len(messages) != 3 {
		t.Fatalf("messages=%d: %v", len(messages), messages)
	}
	assistant, _ := messages[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("assistant=%v", assistant)
	}
	blocks, _ := assistant["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("assistant blocks=%v", blocks)
	}
	first, _ := blocks[0].(map[string]any)
	if first["type"] != "tool_use" || first["id"] != "call_1" || first["name"] != "a" {
		t.Fatalf("tool_use=%v", first)
	}
	input, _ := first["input"].(map[string]any)
	if input["x"] != float64(1) {
		t.Fatalf("input=%v", input)
	}
	toolResults, _ := messages[2].(map[string]any)
	if toolResults["role"] != "user" {
		t.Fatalf("tool result carrier=%v", toolResults)
	}
	resultBlocks, _ := toolResults["content"].([]any)
	if len(resultBlocks) != 3 {
		t.Fatalf("merged user blocks=%v（两条 tool_result 应与后续 user 正文合并为一条消息）", resultBlocks)
	}
	for i := 0; i < 2; i++ {
		block, _ := resultBlocks[i].(map[string]any)
		if block["type"] != "tool_result" {
			t.Fatalf("block=%v", block)
		}
	}
}

func TestChatToMessagesImages(t *testing.T) {
	out := mustTranslate(t, `{
		"messages":[{"role":"user","content":[
			{"type":"text","text":"look"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},
			{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}
		]}]
	}`)
	messages, _ := out["messages"].([]any)
	user, _ := messages[0].(map[string]any)
	blocks, _ := user["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("blocks=%v", blocks)
	}
	base64Block, _ := blocks[1].(map[string]any)
	source, _ := base64Block["source"].(map[string]any)
	if source["type"] != "base64" || source["media_type"] != "image/png" || source["data"] != "AAAA" {
		t.Fatalf("base64 source=%v", source)
	}
	urlBlock, _ := blocks[2].(map[string]any)
	urlSource, _ := urlBlock["source"].(map[string]any)
	if urlSource["type"] != "url" || urlSource["url"] != "https://example.com/cat.png" {
		t.Fatalf("url source=%v", urlSource)
	}
	if _, err := chatCompletionsToMessages(mustParse(t, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/bmp;base64,AA"}}]}]}`)); err == nil {
		t.Fatal("unsupported media type must fail explicitly")
	}
}

func TestChatToMessagesUnsupportedFeaturesFail(t *testing.T) {
	cases := map[string]string{
		"n>1":               `{"messages":[{"role":"user","content":"x"}],"n":2}`,
		"response_format":   `{"messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_object"}}`,
		"logit_bias":        `{"messages":[{"role":"user","content":"x"}],"logit_bias":{"5":100}}`,
		"logprobs":          `{"messages":[{"role":"user","content":"x"}],"logprobs":true}`,
		"presence_penalty":  `{"messages":[{"role":"user","content":"x"}],"presence_penalty":0.5}`,
		"frequency_penalty": `{"messages":[{"role":"user","content":"x"}],"frequency_penalty":0.5}`,
		"bad role":          `{"messages":[{"role":"assistant","content":"prefill"},{"role":"user","content":"x"}]}`,
	}
	for name, body := range cases {
		if _, err := chatCompletionsToMessages(mustParse(t, body)); err == nil {
			t.Fatalf("%s must fail explicitly", name)
		}
	}
	// 零值视为未设置，不拒绝。
	if _, err := chatCompletionsToMessages(mustParse(t, `{"messages":[{"role":"user","content":"x"}],"presence_penalty":0,"frequency_penalty":0}`)); err != nil {
		t.Fatalf("zero penalties must pass: %v", err)
	}
}

func TestChatToMessagesReasoningEffort(t *testing.T) {
	out := mustTranslate(t, `{"messages":[{"role":"user","content":"x"}],"max_tokens":20000,"reasoning_effort":"high"}`)
	thinking, _ := out["thinking"].(map[string]any)
	if thinking == nil || thinking["budget_tokens"] != int64(16384) {
		t.Fatalf("thinking=%v", thinking)
	}
	// 预算钳制：max_tokens 2000 → budget 1999-? 需 < max_tokens。
	out = mustTranslate(t, `{"messages":[{"role":"user","content":"x"}],"max_tokens":2000,"reasoning_effort":"high"}`)
	thinking, _ = out["thinking"].(map[string]any)
	if number(thinking["budget_tokens"]) >= 2000 {
		t.Fatalf("budget=%v must be < max_tokens", thinking["budget_tokens"])
	}
	// none → 不携带 thinking。
	out = mustTranslate(t, `{"messages":[{"role":"user","content":"x"}],"reasoning_effort":"none"}`)
	if _, ok := out["thinking"]; ok {
		t.Fatalf("thinking=%v", out["thinking"])
	}
	// 未知档位显式失败。
	if _, err := chatCompletionsToMessages(mustParse(t, `{"messages":[{"role":"user","content":"x"}],"reasoning_effort":"turbo"}`)); err == nil {
		t.Fatal("unknown effort must fail")
	}
}

func TestMessagesToChatCompletionTranslation(t *testing.T) {
	body := `{
		"id":"msg_9","type":"message","role":"assistant","model":"glm-5.3",
		"content":[
			{"type":"thinking","thinking":"pondering","signature":"sig"},
			{"type":"text","text":"the answer"},
			{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"北京"}}
		],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":12,"output_tokens":34}
	}`
	out, err := messagesToChatCompletion([]byte(body), "zcode")
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatal(err)
	}
	if response["object"] != "chat.completion" || response["model"] != "zcode" {
		t.Fatalf("response=%v", response)
	}
	choices, _ := response["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason=%v", choice["finish_reason"])
	}
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "the answer" {
		t.Fatalf("content=%v", message["content"])
	}
	if message["reasoning_content"] != "pondering" {
		t.Fatalf("reasoning_content=%v", message["reasoning_content"])
	}
	toolCalls, _ := message["tool_calls"].([]any)
	call, _ := toolCalls[0].(map[string]any)
	if call["id"] != "toolu_1" {
		t.Fatalf("tool call=%v", call)
	}
	fn, _ := call["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("function=%v", fn)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatalf("arguments=%v", fn["arguments"])
	}
	if args["city"] != "北京" {
		t.Fatalf("args=%v", args)
	}
	usage, _ := response["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(12) || usage["completion_tokens"] != float64(34) || usage["total_tokens"] != float64(46) {
		t.Fatalf("usage=%v", usage)
	}
}

func TestAnthropicStopReasonMapping(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
		"refusal":       "content_filter",
		"weird":         "stop",
	}
	for from, want := range cases {
		if got := anthropicStopReasonToChat(from); got != want {
			t.Fatalf("%s → %s, want %s", from, got, want)
		}
	}
}

func TestAnthropicErrorToChatEnvelope(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad field"}}`)
	out := anthropicErrorToChat(body)
	var envelope map[string]any
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatal(err)
	}
	if _, hasType := envelope["type"]; hasType {
		t.Fatalf("outer type must be removed: %v", envelope)
	}
	apiError, _ := envelope["error"].(map[string]any)
	if apiError["message"] != "bad field" {
		t.Fatalf("envelope=%v", envelope)
	}
	// OpenAI 形状原样通过。
	openai := []byte(`{"error":{"message":"身份验证失败。","type":"1000"}}`)
	if string(anthropicErrorToChat(openai)) != string(openai) {
		t.Fatalf("openai envelope rewritten: %s", anthropicErrorToChat(openai))
	}
}

func mustParse(t *testing.T, body string) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestChatEndpointAdaptsToAnthropicUpstream(t *testing.T) {
	var gotSystem string
	var gotToolName string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/messages" {
			t.Fatalf("path=%s", req.URL.Path)
		}
		var in map[string]any
		_ = json.NewDecoder(req.Body).Decode(&in)
		gotSystem, _ = in["system"].(string)
		tools, _ := in["tools"].([]any)
		if len(tools) > 0 {
			tool, _ := tools[0].(map[string]any)
			gotToolName, _ = tool["name"].(string)
		}
		_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"你好"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":7}}`))
	}))
	defer upstream.Close()
	r, _ := newZcodeTestRouter(t, Candidate{ID: "z", BaseURL: upstream.URL, Model: "glm-5.3", Anthropic: true})
	rec := postJSON(t, r, "/v1/chat/completions", `{
		"model":"zcode",
		"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],
		"max_tokens":99
	}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotSystem != "be brief" {
		t.Fatalf("system=%q", gotSystem)
	}
	if gotToolName != "f" {
		t.Fatalf("tool=%q", gotToolName)
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["object"] != "chat.completion" || response["model"] != "zcode" {
		t.Fatalf("response=%v", response)
	}
	choices, _ := response["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "你好" {
		t.Fatalf("message=%v", message)
	}
	usage, _ := response["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(5) || usage["completion_tokens"] != float64(7) {
		t.Fatalf("usage=%v", usage)
	}
}

func TestChatEndpointTranslatesAnthropicSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":9}}}\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"你\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"好\"}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":15}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()
	r, db := newZcodeTestRouter(t, Candidate{ID: "z", BaseURL: upstream.URL, Model: "glm-5.3", Anthropic: true})
	rec := postJSON(t, r, "/v1/chat/completions", `{"model":"zcode","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"你"`) || !strings.Contains(body, `"content":"好"`) {
		t.Fatalf("text deltas missing: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("finish missing: %s", body)
	}
	if !strings.Contains(body, `"prompt_tokens":9`) || !strings.Contains(body, `"completion_tokens":15`) {
		t.Fatalf("usage chunk missing: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("[DONE] missing: %s", body)
	}
	var prompt, completion int64
	if err := db.QueryRow(`SELECT prompt_tokens, completion_tokens FROM llm_requests`).Scan(&prompt, &completion); err != nil {
		t.Fatal(err)
	}
	if prompt != 9 || completion != 15 {
		t.Fatalf("ledger usage prompt=%d completion=%d", prompt, completion)
	}
}

func TestChatEndpointTranslatesAnthropicToolStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":3}}}\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_7\",\"name\":\"get_weather\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"北京\\\"}\"}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":11}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()
	r, _ := newZcodeTestRouter(t, Candidate{ID: "z", BaseURL: upstream.URL, Model: "glm-5.3", Anthropic: true})
	rec := postJSON(t, r, "/v1/chat/completions", `{"model":"zcode","stream":true,"messages":[{"role":"user","content":"weather?"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"id":"toolu_7"`) || !strings.Contains(body, `"name":"get_weather"`) {
		t.Fatalf("tool call start missing: %s", body)
	}
	if !strings.Contains(body, `"{\"city\":"`) || !strings.Contains(body, `"\"北京\"}"`) {
		t.Fatalf("argument deltas missing: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason missing: %s", body)
	}
}

func TestResponsesEndpointChainsToAnthropic(t *testing.T) {
	var gotSystem string
	var gotUserText string
	var gotMaxTokens any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/messages" {
			t.Fatalf("path=%s", req.URL.Path)
		}
		var in map[string]any
		_ = json.NewDecoder(req.Body).Decode(&in)
		gotSystem, _ = in["system"].(string)
		gotMaxTokens = in["max_tokens"]
		messages, _ := in["messages"].([]any)
		user, _ := messages[0].(map[string]any)
		blocks, _ := user["content"].([]any)
		block, _ := blocks[0].(map[string]any)
		gotUserText, _ = block["text"].(string)
		_, _ = w.Write([]byte(`{"id":"m2","type":"message","role":"assistant","content":[{"type":"text","text":"chained"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":6}}`))
	}))
	defer upstream.Close()
	r, db := newZcodeTestRouter(t, Candidate{ID: "z", BaseURL: upstream.URL, Model: "glm-5.3", Anthropic: true})
	rec := postJSON(t, r, "/v1/responses", `{"model":"zcode","instructions":"stay terse","input":"hello","max_output_tokens":55}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotSystem != "stay terse" {
		t.Fatalf("system=%q", gotSystem)
	}
	if gotUserText != "hello" {
		t.Fatalf("user=%q", gotUserText)
	}
	if gotMaxTokens != float64(55) {
		t.Fatalf("max_tokens=%v", gotMaxTokens)
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["object"] != "response" || response["status"] != "completed" {
		t.Fatalf("response=%v", response)
	}
	output, _ := response["output"].([]any)
	if len(output) == 0 {
		t.Fatalf("output=%v", output)
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "message" {
		t.Fatalf("item=%v", item)
	}
	content, _ := item["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["text"] != "chained" {
		t.Fatalf("text=%v", part["text"])
	}
	usage, _ := response["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(6) {
		t.Fatalf("usage=%v", usage)
	}
	var tokens int64
	if err := db.QueryRow(`SELECT total_tokens FROM llm_requests`).Scan(&tokens); err != nil || tokens != 10 {
		t.Fatalf("ledger=%d err=%v", tokens, err)
	}
}

func TestResponsesEndpointChainsAnthropicSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":2}}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"chain\"}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()
	r, _ := newZcodeTestRouter(t, Candidate{ID: "z", BaseURL: upstream.URL, Model: "glm-5.3", Anthropic: true})
	rec := postJSON(t, r, "/v1/responses", `{"model":"zcode","input":"hi","stream":true}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"response.created", "response.output_text.delta", "response.completed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if !strings.Contains(body, `"delta":"chain"`) {
		t.Fatalf("delta missing: %s", body)
	}
	if !strings.Contains(body, `"input_tokens":2`) || !strings.Contains(body, `"output_tokens":5`) {
		t.Fatalf("usage missing: %s", body)
	}
}

func TestChatEndpointRejectsUnsupportedFeatureExplicitly(t *testing.T) {
	// 上游不应被调用：unsupported 特性在本地结束。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("upstream must not be called")
	}))
	defer upstream.Close()
	r, _ := newZcodeTestRouter(t, Candidate{ID: "z", BaseURL: upstream.URL, Model: "glm-5.3", Anthropic: true})
	rec := postJSON(t, r, "/v1/chat/completions", `{"model":"zcode","n":3,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 400 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "n cannot be adapted") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestChatEndpointTranslatesAnthropicThinkingStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":2}}}\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"let me think\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig\"}}\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()
	r, _ := newZcodeTestRouter(t, Candidate{ID: "z", BaseURL: upstream.URL, Model: "glm-5.3", Anthropic: true})
	rec := postJSON(t, r, "/v1/chat/completions", `{"model":"zcode","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"reasoning_content":"let me think"`) {
		t.Fatalf("thinking delta missing: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"answer"`) {
		t.Fatalf("text delta missing: %s", rec.Body.String())
	}
	// signature_delta 是协议内部帧，不透传给下游。
	if strings.Contains(rec.Body.String(), `"signature"`) {
		t.Fatalf("signature delta leaked: %s", rec.Body.String())
	}
}

// TestResponsesChainDropsThinkingDelta 钉住现状：Responses 链的 chunk
// 消费端只读 content/tool_calls，thinking_delta 合成的 reasoning_content
// 被静默丢弃。这是 design.md 已知降级清单的一项，不是缺陷。
func TestResponsesChainDropsThinkingDelta(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"secret reasoning\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"visible\"}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()
	r, _ := newZcodeTestRouter(t, Candidate{ID: "z", BaseURL: upstream.URL, Model: "glm-5.3", Anthropic: true})
	rec := postJSON(t, r, "/v1/responses", `{"model":"zcode","input":"hi","stream":true}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"delta":"visible"`) {
		t.Fatalf("text missing: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret reasoning") {
		t.Fatalf("thinking must not leak into responses events: %s", rec.Body.String())
	}
}

func TestAnthropicErrorTypeMapping(t *testing.T) {
	cases := map[string]string{
		"authentication_error":       "authentication_error",
		"gateway_overloaded":         "rate_limit_error",
		"upstream_rate_limited":      "rate_limit_error",
		"upstream_concurrency_limit": "rate_limit_error",
		"retry_budget_exhausted":     "rate_limit_error",
		"unknown_model_route":        "invalid_request_error",
		"invalid_json":               "invalid_request_error",
		"unsupported_openai_feature": "invalid_request_error",
		"upstream_error":             "api_error",
		"mystery":                    "api_error",
	}
	for from, want := range cases {
		if got := anthropicErrorType(from); got != want {
			t.Fatalf("%s → %s, want %s", from, got, want)
		}
	}
}
