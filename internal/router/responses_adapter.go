package router

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	json "github.com/goccy/go-json"
)

// responsesToChatCompletions 是有意保守的降级转换。它只转换语义明确的
// 文本/图片消息和常见采样参数；不能忠实表示的 Responses 特性必须报错。
func responsesToChatCompletions(in map[string]any, maxTokensField string) (map[string]any, error) {
	for _, key := range []string{"previous_response_id", "background", "conversation", "prompt"} {
		if _, ok := in[key]; ok {
			return nil, fmt.Errorf("%s cannot be adapted to Chat Completions", key)
		}
	}
	messages := make([]any, 0)
	if rawInstructions, exists := in["instructions"]; exists && rawInstructions != nil {
		instructions, ok := rawInstructions.(string)
		if !ok {
			return nil, fmt.Errorf("instructions must be a string")
		}
		if instructions != "" {
			messages = append(messages, map[string]any{"role": "system", "content": instructions})
		}
	}
	input, ok := in["input"]
	if !ok {
		return nil, fmt.Errorf("input is required when adapting Responses API")
	}
	switch v := input.(type) {
	case string:
		messages = append(messages, map[string]any{"role": "user", "content": v})
	case []any:
		var pendingCalls []any
		flushCalls := func() {
			if len(pendingCalls) > 0 {
				messages = append(messages, map[string]any{"role": "assistant", "tool_calls": pendingCalls})
				pendingCalls = nil
			}
		}
		for _, raw := range v {
			item, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("input items must be objects")
			}
			typ, _ := item["type"].(string)
			switch typ {
			case "function_call":
				call, err := responseFunctionCallToChat(item)
				if err != nil {
					return nil, err
				}
				pendingCalls = append(pendingCalls, call)
				continue
			case "function_call_output":
				flushCalls()
				callID, _ := item["call_id"].(string)
				if callID == "" {
					return nil, fmt.Errorf("function_call_output requires call_id")
				}
				content, err := functionOutputToChat(item["output"])
				if err != nil {
					return nil, err
				}
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": content})
				continue
			case "", "message":
				flushCalls()
			default:
				return nil, fmt.Errorf("input item type %q cannot be adapted", typ)
			}
			role, _ := item["role"].(string)
			if role == "" {
				role = "user"
			}
			content, err := responseContentToChat(item["content"])
			if err != nil {
				return nil, err
			}
			messages = append(messages, map[string]any{"role": role, "content": content})
		}
		flushCalls()
	default:
		return nil, fmt.Errorf("input must be a string or message array")
	}
	out := map[string]any{"messages": messages}
	for _, key := range []string{"temperature", "top_p", "stream", "user", "parallel_tool_calls", "reasoning_effort", "service_tier", "safety_identifier", "prompt_cache_key", "frequency_penalty", "presence_penalty"} {
		if v, ok := in[key]; ok {
			out[key] = v
		}
	}
	if topLogprobs, ok := in["top_logprobs"]; ok {
		out["top_logprobs"] = topLogprobs
		out["logprobs"] = true
	}
	if rawReasoning, exists := in["reasoning"]; exists && rawReasoning != nil {
		reasoning, ok := rawReasoning.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("reasoning must be an object")
		}
		if summary, exists := reasoning["summary"]; exists && summary != nil && summary != "none" {
			return nil, fmt.Errorf("reasoning.summary cannot be adapted to Chat Completions")
		}
		if effort, ok := reasoning["effort"]; ok {
			out["reasoning_effort"] = effort
		}
	}
	if tools, ok := in["tools"]; ok {
		converted, err := responseToolsToChat(tools)
		if err != nil {
			return nil, err
		}
		out["tools"] = converted
	}
	if choice, ok := in["tool_choice"]; ok {
		converted, err := responseToolChoiceToChat(choice)
		if err != nil {
			return nil, err
		}
		out["tool_choice"] = converted
	}
	if text, ok := in["text"]; ok {
		format, err := responseTextFormatToChat(text)
		if err != nil {
			return nil, err
		}
		if format != nil {
			out["response_format"] = format
		}
		if textObject, _ := text.(map[string]any); textObject != nil {
			if verbosity, ok := textObject["verbosity"]; ok {
				out["verbosity"] = verbosity
			}
		}
	}
	if stream, _ := in["stream"].(bool); stream {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	if max, ok := in["max_output_tokens"]; ok {
		if maxTokensField == "" {
			maxTokensField = "max_tokens"
		}
		out[maxTokensField] = max
	}
	return out, nil
}

func responseFunctionCallToChat(item map[string]any) (map[string]any, error) {
	callID, _ := item["call_id"].(string)
	name, _ := item["name"].(string)
	if callID == "" || name == "" {
		return nil, fmt.Errorf("function_call requires call_id and name")
	}
	arguments, _ := item["arguments"].(string)
	return map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": arguments}}, nil
}

func functionOutputToChat(output any) (string, error) {
	if text, ok := output.(string); ok {
		return text, nil
	}
	b, err := json.Marshal(output)
	if err != nil {
		return "", fmt.Errorf("function_call_output output cannot be encoded: %w", err)
	}
	return string(b), nil
}

func responseToolsToChat(raw any) ([]any, error) {
	tools, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("tools must be an array")
	}
	out := make([]any, 0, len(tools))
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tools must contain objects")
		}
		if typ, _ := tool["type"].(string); typ != "function" {
			return nil, fmt.Errorf("tool type %q cannot be adapted to Chat Completions", typ)
		}
		name, _ := tool["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("function tool requires name")
		}
		fn := map[string]any{"name": name}
		for _, key := range []string{"description", "parameters", "strict"} {
			if value, ok := tool[key]; ok {
				fn[key] = value
			}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out, nil
}

func responseToolChoiceToChat(raw any) (any, error) {
	if choice, ok := raw.(string); ok {
		switch choice {
		case "auto", "none", "required":
			return choice, nil
		default:
			return nil, fmt.Errorf("tool_choice %q cannot be adapted", choice)
		}
	}
	choice, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tool_choice must be a string or object")
	}
	if typ, _ := choice["type"].(string); typ != "function" {
		return nil, fmt.Errorf("tool_choice type %q cannot be adapted", typ)
	}
	name, _ := choice["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("function tool_choice requires name")
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": name}}, nil
}

func responseTextFormatToChat(raw any) (any, error) {
	text, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("text must be an object")
	}
	format, ok := text["format"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("text.format must be an object")
	}
	typ, _ := format["type"].(string)
	switch typ {
	case "", "text":
		return nil, nil
	case "json_object":
		return map[string]any{"type": "json_object"}, nil
	case "json_schema":
		name, _ := format["name"].(string)
		if name == "" || format["schema"] == nil {
			return nil, fmt.Errorf("text.format json_schema requires name and schema")
		}
		schema := map[string]any{"name": name, "schema": format["schema"]}
		for _, key := range []string{"description", "strict"} {
			if value, ok := format[key]; ok {
				schema[key] = value
			}
		}
		return map[string]any{"type": "json_schema", "json_schema": schema}, nil
	default:
		return nil, fmt.Errorf("text.format type %q cannot be adapted", typ)
	}
}

func responseContentToChat(raw any) (any, error) {
	if text, ok := raw.(string); ok {
		return text, nil
	}
	parts, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("message content must be a string or content-part array")
	}
	out := make([]any, 0, len(parts))
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("content parts must be objects")
		}
		switch part["type"] {
		case "input_text", "output_text", "text":
			text, _ := part["text"].(string)
			out = append(out, map[string]any{"type": "text", "text": text})
		case "input_image", "image_url":
			image := part["image_url"]
			if obj, ok := image.(map[string]any); ok {
				image = obj["url"]
			}
			if image == nil {
				return nil, fmt.Errorf("input_image requires image_url")
			}
			imageURL := map[string]any{"url": image}
			if detail, ok := part["detail"]; ok {
				imageURL["detail"] = detail
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": imageURL})
		default:
			return nil, fmt.Errorf("content part type %q cannot be adapted", part["type"])
		}
	}
	return out, nil
}

func chatCompletionToResponse(body []byte, model string) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	data := senseData(raw)
	if err := senseStatusError(raw); err != nil {
		return nil, err
	}
	return json.Marshal(chatDataToResponse(data, "resp_"+newID(), model))
}

// streamChatAsResponses 把 OpenAI Chat Completions（包括 data 包装变体）的 SSE
// 转成 Responses 的类型化事件；完整缓存最终对象，避免 completed 事件丢失正文或工具调用。
func (r *Router) streamChatAsResponses(w http.ResponseWriter, resp *http.Response, model string) (Usage, error) {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flush, _ := w.(http.Flusher)
	state := newResponseStreamState(w, model)
	usage := Usage{Source: "unavailable"}
	reader := acquireSSEReader(resp.Body)
	defer releaseSSEReader(reader)
	for {
		line, err := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
			if bytes.Equal(payload, []byte("[DONE]")) {
				state.complete(usage)
			} else {
				var raw map[string]any
				if json.Unmarshal(payload, &raw) == nil {
					if streamErr := senseStatusError(raw); streamErr != nil {
						state.fail(streamErr)
						return usage, streamErr
					}
					data := senseData(raw)
					if u := usageFromMap(data); u.Source == "upstream" {
						usage = u
					}
					state.consume(data)
					if flush != nil {
						flush.Flush()
					}
				}
			}
		}
		if err == io.EOF {
			state.complete(usage)
			return usage, nil
		}
		if err != nil {
			state.fail(err)
			return usage, err
		}
	}
}

type responseStreamTool struct {
	itemID      string
	callID      string
	name        string
	arguments   strings.Builder
	outputIndex int
	added       bool
}

type responseStreamState struct {
	w          http.ResponseWriter
	id         string
	model      string
	createdAt  int64
	created    bool
	completed  bool
	textAdded  bool
	textOutput int
	text       strings.Builder
	finish     string
	tools      map[int]*responseStreamTool
	nextOutput int
}

func newResponseStreamState(w http.ResponseWriter, model string) *responseStreamState {
	return &responseStreamState{w: w, id: "resp_" + newID(), model: model, createdAt: time.Now().Unix(), textOutput: -1, tools: make(map[int]*responseStreamTool)}
}

func (s *responseStreamState) ensureCreated() {
	if s.created {
		return
	}
	writeResponseEvent(s.w, "response.created", map[string]any{"type": "response.created", "response": responseBase(s.id, s.model, s.createdAt, "in_progress", nil, nil, Usage{Source: "unavailable"})})
	s.created = true
}

func (s *responseStreamState) ensureText() {
	s.ensureCreated()
	if s.textAdded {
		return
	}
	s.textAdded = true
	index := s.nextOutput
	s.textOutput = index
	s.nextOutput++
	item := responseMessageItem(s.id, "", "in_progress")
	item["content"] = []any{}
	writeResponseEvent(s.w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": index, "item": item})
	part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
	writeResponseEvent(s.w, "response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": "msg_" + s.id, "output_index": index, "content_index": 0, "part": part})
}

func (s *responseStreamState) consume(data map[string]any) {
	choices, _ := data["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, _ := choices[0].(map[string]any)
	if finish, _ := choice["finish_reason"].(string); finish != "" {
		s.finish = finish
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return
	}
	if text := contentText(delta["content"]); text != "" {
		s.ensureText()
		s.text.WriteString(text)
		writeResponseEvent(s.w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg_" + s.id, "output_index": s.textOutput, "content_index": 0, "delta": text})
	}
	toolCalls, _ := delta["tool_calls"].([]any)
	for _, rawCall := range toolCalls {
		call, _ := rawCall.(map[string]any)
		chatIndex := int(number(call["index"]))
		tool := s.tools[chatIndex]
		if tool == nil {
			tool = &responseStreamTool{itemID: "fc_" + newID(), outputIndex: s.nextOutput}
			s.nextOutput++
			s.tools[chatIndex] = tool
		}
		if id, _ := call["id"].(string); id != "" {
			tool.callID = id
		}
		fn, _ := call["function"].(map[string]any)
		if name, _ := fn["name"].(string); name != "" {
			tool.name += name
		}
		if !tool.added {
			s.ensureCreated()
			if tool.callID == "" {
				tool.callID = "call_" + newID()
			}
			item := responseFunctionCallItem(tool.itemID, tool.callID, tool.name, "", "in_progress")
			writeResponseEvent(s.w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": tool.outputIndex, "item": item})
			tool.added = true
		}
		if arguments, _ := fn["arguments"].(string); arguments != "" {
			tool.arguments.WriteString(arguments)
			writeResponseEvent(s.w, "response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": tool.itemID, "output_index": tool.outputIndex, "delta": arguments})
		}
	}
}

func (s *responseStreamState) complete(usage Usage) {
	if s.completed {
		return
	}
	s.ensureCreated()
	if !s.textAdded && len(s.tools) == 0 {
		s.ensureText()
	}
	if s.textAdded {
		text := s.text.String()
		writeResponseEvent(s.w, "response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": "msg_" + s.id, "output_index": s.textOutput, "content_index": 0, "text": text})
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		writeResponseEvent(s.w, "response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": "msg_" + s.id, "output_index": s.textOutput, "content_index": 0, "part": part})
		writeResponseEvent(s.w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": s.textOutput, "item": responseMessageItem(s.id, text, "completed")})
	}
	for outputIndex := 0; outputIndex < s.nextOutput; outputIndex++ {
		for _, tool := range s.tools {
			if tool.outputIndex != outputIndex {
				continue
			}
			arguments := tool.arguments.String()
			writeResponseEvent(s.w, "response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": tool.itemID, "output_index": tool.outputIndex, "arguments": arguments})
			item := responseFunctionCallItem(tool.itemID, tool.callID, tool.name, arguments, "completed")
			writeResponseEvent(s.w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": tool.outputIndex, "item": item})
		}
	}
	output := s.outputItems()
	status, incomplete := responseStatus(s.finish)
	event := "response.completed"
	if status == "incomplete" {
		event = "response.incomplete"
	}
	writeResponseEvent(s.w, event, map[string]any{"type": event, "response": responseBase(s.id, s.model, s.createdAt, status, incomplete, output, usage)})
	s.completed = true
}

func (s *responseStreamState) fail(err error) {
	if s.completed {
		return
	}
	writeResponseEvent(s.w, "error", map[string]any{"type": "error", "code": "upstream_error", "message": err.Error(), "param": nil})
	s.completed = true
}

func (s *responseStreamState) outputItems() []any {
	out := make([]any, s.nextOutput)
	if s.textAdded {
		out[s.textOutput] = responseMessageItem(s.id, s.text.String(), "completed")
	}
	for _, tool := range s.tools {
		out[tool.outputIndex] = responseFunctionCallItem(tool.itemID, tool.callID, tool.name, tool.arguments.String(), "completed")
	}
	return out
}

func chatDataToResponse(data map[string]any, id, model string) map[string]any {
	var output []any
	finish := ""
	choices, _ := data["choices"].([]any)
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		finish, _ = choice["finish_reason"].(string)
		message, _ := choice["message"].(map[string]any)
		if message == nil {
			if text, ok := choice["message"].(string); ok {
				message = map[string]any{"content": text}
			}
		}
		toolCalls, _ := message["tool_calls"].([]any)
		text := contentText(message["content"])
		refusal, _ := message["refusal"].(string)
		if text != "" || refusal != "" || len(toolCalls) == 0 {
			output = append(output, responseMessageItemFromChat(id, text, refusal))
		}
		for _, rawCall := range toolCalls {
			call, _ := rawCall.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			callID, _ := call["id"].(string)
			if callID == "" {
				callID = "call_" + newID()
			}
			output = append(output, responseFunctionCallItem("fc_"+newID(), callID, stringValue(fn["name"]), stringValue(fn["arguments"]), "completed"))
		}
		// 兼容仍返回旧式 function_call 的 Chat Completions 上游。
		if legacy, ok := message["function_call"].(map[string]any); ok {
			output = append(output, responseFunctionCallItem("fc_"+newID(), "call_"+newID(), stringValue(legacy["name"]), stringValue(legacy["arguments"]), "completed"))
		}
	}
	status, incomplete := responseStatus(finish)
	return responseBase(id, model, time.Now().Unix(), status, incomplete, output, usageFromMap(data))
}

func responseStatus(finish string) (string, any) {
	switch finish {
	case "length":
		return "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		return "incomplete", map[string]any{"reason": "content_filter"}
	default:
		return "completed", nil
	}
}

func responseBase(id, model string, createdAt int64, status string, incomplete, output any, usage Usage) map[string]any {
	if output == nil {
		output = []any{}
	} else if items, ok := output.([]any); ok && items == nil {
		output = []any{}
	}
	response := map[string]any{
		"id": id, "object": "response", "created_at": createdAt, "status": status,
		"error": nil, "incomplete_details": incomplete, "model": model, "output": output,
		"parallel_tool_calls": true, "previous_response_id": nil,
		"reasoning": map[string]any{"effort": nil, "summary": nil},
		"usage": map[string]any{
			"input_tokens":          usage.Prompt,
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens":         usage.Completion,
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			"total_tokens":          usage.Total,
		},
	}
	if status == "completed" {
		response["completed_at"] = time.Now().Unix()
	}
	return response
}

func responseMessageItem(id, text, status string) map[string]any {
	return map[string]any{
		"id": "msg_" + id, "type": "message", "status": status, "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}},
	}
}

func responseMessageItemFromChat(id, text, refusal string) map[string]any {
	item := responseMessageItem(id, text, "completed")
	content := make([]any, 0, 2)
	if text != "" || refusal == "" {
		content = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}})
	}
	if refusal != "" {
		content = append(content, map[string]any{"type": "refusal", "refusal": refusal})
	}
	item["content"] = content
	return item
}

func responseFunctionCallItem(itemID, callID, name, arguments, status string) map[string]any {
	return map[string]any{"id": itemID, "type": "function_call", "call_id": callID, "name": name, "arguments": arguments, "status": status}
}

func senseData(raw map[string]any) map[string]any {
	if data, ok := raw["data"].(map[string]any); ok {
		return data
	}
	return raw
}
func senseStatusError(raw map[string]any) error {
	if apiError, ok := raw["error"].(map[string]any); ok {
		message, _ := apiError["message"].(string)
		if message == "" {
			message = "upstream returned an error"
		}
		return fmt.Errorf("%s", message)
	}
	if apiError, ok := raw["error"].(string); ok && apiError != "" {
		return fmt.Errorf("%s", apiError)
	}
	if s, ok := raw["status"].(map[string]any); ok {
		if code := number(s["code"]); code != 0 {
			return fmt.Errorf("SenseNova status %d: %v", code, s["message"])
		}
	}
	return nil
}
func stringValue(v any) string { s, _ := v.(string); return s }
func contentText(v any) string {
	if text := stringValue(v); text != "" {
		return text
	}
	parts, ok := v.([]any)
	if !ok {
		return ""
	}
	var text strings.Builder
	for _, raw := range parts {
		if part, ok := raw.(map[string]any); ok {
			text.WriteString(stringValue(part["text"]))
		}
	}
	return text.String()
}
func usageFromMap(data map[string]any) Usage {
	if raw, ok := data["usage"].(map[string]any); ok {
		return Usage{Prompt: number(raw["prompt_tokens"]), Completion: number(raw["completion_tokens"]), Total: number(raw["total_tokens"]), Source: "upstream"}
	}
	return Usage{Source: "unavailable"}
}
func writeResponseEvent(w http.ResponseWriter, event string, data any) {
	b, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}
func writeAPIError(w http.ResponseWriter, status int, message, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": typ}})
}
