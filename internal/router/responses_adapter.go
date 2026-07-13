package router

import (
	"bufio"
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
	for _, key := range []string{"previous_response_id", "tools", "tool_choice", "parallel_tool_calls", "background"} {
		if _, ok := in[key]; ok {
			return nil, fmt.Errorf("%s cannot be adapted to Chat Completions", key)
		}
	}
	messages := make([]any, 0)
	if instructions, ok := in["instructions"].(string); ok && instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	input, ok := in["input"]
	if !ok {
		return nil, fmt.Errorf("input is required when adapting Responses API")
	}
	switch v := input.(type) {
	case string:
		messages = append(messages, map[string]any{"role": "user", "content": v})
	case []any:
		for _, raw := range v {
			item, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("input items must be objects")
			}
			if typ, _ := item["type"].(string); typ != "" && typ != "message" {
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
	default:
		return nil, fmt.Errorf("input must be a string or message array")
	}
	out := map[string]any{"messages": messages}
	for _, key := range []string{"temperature", "top_p", "stream", "user"} {
		if v, ok := in[key]; ok {
			out[key] = v
		}
	}
	if max, ok := in["max_output_tokens"]; ok {
		if maxTokensField == "" {
			maxTokensField = "max_tokens"
		}
		out[maxTokensField] = max
	}
	return out, nil
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
		case "input_text", "text":
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
			out = append(out, map[string]any{"type": "image_url", "image_url": image})
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
	id, _ := data["id"].(string)
	if id == "" {
		id = "resp_" + newID()
	}
	text, finish := senseText(data, false)
	return json.Marshal(responseObject(id, model, text, finish, usageFromMap(data)))
}

// streamChatAsResponses 把 SenseNova 的 data-only SSE 转成 Responses SSE 事件。
func (r *Router) streamChatAsResponses(w http.ResponseWriter, resp *http.Response, model string) Usage {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flush, _ := w.(http.Flusher)
	id := "resp_" + newID()
	created := false
	completed := false
	usage := Usage{Source: "unavailable"}
	complete := func(text, finish string) {
		if completed {
			return
		}
		writeResponseEvent(w, "response.completed", map[string]any{"type": "response.completed", "response": responseObject(id, model, text, finish, usage)})
		completed = true
		if flush != nil {
			flush.Flush()
		}
	}
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
			if bytes.Equal(payload, []byte("[DONE]")) {
				complete("", "stop")
			} else {
				var raw map[string]any
				if json.Unmarshal(payload, &raw) == nil {
					data := senseData(raw)
					if upstreamID, _ := data["id"].(string); upstreamID != "" {
						id = upstreamID
					}
					if u := usageFromMap(data); u.Source == "upstream" {
						usage = u
					}
					if !created {
						writeResponseEvent(w, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(), "status": "in_progress", "model": model, "output": []any{}}})
						created = true
					}
					delta, finish := senseText(data, true)
					if delta != "" {
						writeResponseEvent(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg_" + id, "output_index": 0, "content_index": 0, "delta": delta})
					}
					if finish != "" {
						complete("", finish)
					}
					if flush != nil {
						flush.Flush()
					}
				}
			}
		}
		if err == io.EOF {
			complete("", "stop")
			return usage
		}
		if err != nil {
			complete("", "stop")
			return usage
		}
	}
}

func senseData(raw map[string]any) map[string]any {
	if data, ok := raw["data"].(map[string]any); ok {
		return data
	}
	return raw
}
func senseStatusError(raw map[string]any) error {
	if s, ok := raw["status"].(map[string]any); ok {
		if code := number(s["code"]); code != 0 {
			return fmt.Errorf("SenseNova status %d: %v", code, s["message"])
		}
	}
	return nil
}
func senseText(data map[string]any, delta bool) (string, string) {
	choices, _ := data["choices"].([]any)
	if len(choices) == 0 {
		return "", ""
	}
	choice, _ := choices[0].(map[string]any)
	key := "message"
	if delta {
		key = "delta"
	}
	text := contentText(choice[key])
	if message, ok := choice[key].(map[string]any); ok {
		text = contentText(message["content"])
	}
	finish := stringValue(choice["finish_reason"])
	return text, finish
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
func responseObject(id, model, text, finish string, usage Usage) map[string]any {
	status := "completed"
	if finish == "" {
		status = "in_progress"
	}
	return map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(), "status": status, "model": model, "output": []any{map[string]any{"id": "msg_" + id, "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}}, "usage": map[string]any{"input_tokens": usage.Prompt, "output_tokens": usage.Completion, "total_tokens": usage.Total}}
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
