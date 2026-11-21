package router

import (
	"bufio"
	"bytes"
	"io"
	"strings"

	json "github.com/goccy/go-json"
)

// Mistral 的 chat completions 与 OpenAI 规范存在三类偏差：
//  1. assistant content 可能是 parts 数组，thinking 部分混在其中；
//  2. 流式 chunk 带私有字段 p，usage 内联在最后一个内容帧且不会补
//     choices:[] 独立帧，即使请求了 stream_options.include_usage；
//  3. 错误体是扁平对象，而不是 {"error": {...}} 信封。
//
// 本文件只在候选显式配置 mistral_compat 时介入，且只改写响应方向：
// 请求体、凭据与故障切换语义保持原样。

var mistralCompletionFields = map[string]bool{
	"id": true, "object": true, "created": true, "model": true,
	"choices": true, "usage": true, "system_fingerprint": true,
	"service_tier": true, "error": true,
}

var mistralChoiceFields = map[string]bool{
	"index": true, "message": true, "delta": true,
	"finish_reason": true, "logprobs": true,
}

var mistralMessageFields = map[string]bool{
	"role": true, "content": true, "name": true, "tool_calls": true,
	"function_call": true, "refusal": true, "audio": true,
	"annotations": true, "reasoning_content": true,
}

var mistralDeltaFields = map[string]bool{
	"role": true, "content": true, "tool_calls": true, "function_call": true,
	"refusal": true, "audio": true, "reasoning_content": true,
}

// normalizeMistralCompletion 把非流式 Mistral chat completion 规整为
// OpenAI 形状；非 chat completion（错误对象等）原样返回。
func normalizeMistralCompletion(body []byte) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return body, nil
	}
	if stringValue(raw["object"]) != "chat.completion" {
		return body, nil
	}
	out := make(map[string]any, len(raw))
	for key, value := range raw {
		if mistralCompletionFields[key] {
			out[key] = value
		}
	}
	if choices, ok := raw["choices"].([]any); ok {
		normalized := make([]any, len(choices))
		for i, rawChoice := range choices {
			choice, _ := rawChoice.(map[string]any)
			normalized[i] = normalizeMistralChoice(choice)
		}
		out["choices"] = normalized
	}
	return json.Marshal(out)
}

func normalizeMistralChoice(choice map[string]any) map[string]any {
	out := make(map[string]any, len(choice))
	for key, value := range choice {
		if mistralChoiceFields[key] {
			out[key] = value
		}
	}
	if message, ok := choice["message"].(map[string]any); ok {
		out["message"] = normalizeMistralMessage(message)
	}
	return out
}

func normalizeMistralMessage(message map[string]any) map[string]any {
	out := make(map[string]any, len(message)+1)
	for key, value := range message {
		if mistralMessageFields[key] {
			out[key] = value
		}
	}
	if _, ok := message["content"]; ok {
		text, reasoning := mistralSplitContent(message["content"])
		if text == "" {
			// OpenAI 对没有可见文本的 assistant 消息使用 null，
			// 只有 thinking 内容时不应伪造空字符串回答。
			out["content"] = nil
		} else {
			out["content"] = text
		}
		if reasoning != "" {
			out["reasoning_content"] = reasoning
		}
	}
	if calls, ok := message["tool_calls"].([]any); ok {
		// 非流式 message 的 tool_calls 没有 index 字段。
		out["tool_calls"] = normalizeMistralToolCalls(calls, false)
	}
	return out
}

func normalizeMistralDelta(delta map[string]any) map[string]any {
	out := make(map[string]any, len(delta)+1)
	for key, value := range delta {
		if mistralDeltaFields[key] {
			out[key] = value
		}
	}
	text, reasoning := mistralSplitContent(delta["content"])
	if _, ok := delta["content"]; ok {
		out["content"] = text
	}
	if reasoning != "" {
		out["reasoning_content"] = reasoning
	}
	if calls, ok := delta["tool_calls"].([]any); ok {
		// 流式 delta 的 tool_calls 依赖 index 拼装，必须保留。
		out["tool_calls"] = normalizeMistralToolCalls(calls, true)
	}
	return out
}

func normalizeMistralToolCalls(calls []any, keepIndex bool) []any {
	out := make([]any, 0, len(calls))
	for _, rawCall := range calls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			continue
		}
		clean := make(map[string]any, 4)
		for _, key := range []string{"id", "type", "function", "index"} {
			if key == "index" && !keepIndex {
				continue
			}
			if value, exists := call[key]; exists {
				clean[key] = value
			}
		}
		if function, ok := call["function"].(map[string]any); ok {
			cleanFunction := make(map[string]any, 2)
			for _, key := range []string{"name", "arguments"} {
				if value, exists := function[key]; exists {
					cleanFunction[key] = value
				}
			}
			clean["function"] = cleanFunction
		}
		out = append(out, clean)
	}
	return out
}

// mistralSplitContent 把 content（字符串或 parts 数组）拆成可见文本与
// thinking 文本。parts 中非 thinking 的 text 累加为回答，thinking 部分
// 单独返回，供调用方写入 reasoning_content。
func mistralSplitContent(content any) (text, reasoning string) {
	switch value := content.(type) {
	case string:
		return value, ""
	case []any:
		var textBuilder, reasoningBuilder strings.Builder
		for _, rawPart := range value {
			part, ok := rawPart.(map[string]any)
			if !ok {
				if s, ok := rawPart.(string); ok {
					textBuilder.WriteString(s)
				}
				continue
			}
			if stringValue(part["type"]) == "thinking" {
				thinking := mistralThinkingText(part["thinking"])
				if thinking == "" {
					thinking = stringValue(part["text"])
				}
				reasoningBuilder.WriteString(thinking)
				continue
			}
			textBuilder.WriteString(stringValue(part["text"]))
		}
		return textBuilder.String(), reasoningBuilder.String()
	}
	return "", ""
}

func mistralThinkingText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var builder strings.Builder
		for _, raw := range typed {
			if part, ok := raw.(map[string]any); ok {
				builder.WriteString(stringValue(part["text"]))
			}
		}
		return builder.String()
	}
	return ""
}

// mistralErrorToOpenAI 把 Mistral 的扁平错误体包装成 OpenAI 信封。
// 非 Mistral 形状（已带 error 字段或其他 JSON）原样返回。
func mistralErrorToOpenAI(body []byte) []byte {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	if _, ok := raw["error"]; ok {
		return body
	}
	if stringValue(raw["object"]) != "error" {
		return body
	}
	inner := map[string]any{
		"message": stringValue(raw["message"]),
		"type":    stringValue(raw["type"]),
	}
	if value, ok := raw["param"]; ok {
		inner["param"] = value
	}
	if value, ok := raw["code"]; ok {
		inner["code"] = value
	}
	converted, err := json.Marshal(map[string]any{"error": inner})
	if err != nil {
		return body
	}
	return converted
}

// streamWantsUsage 判断下游是否请求了 stream_options.include_usage。
func streamWantsUsage(payload map[string]any) bool {
	if streamed, _ := payload["stream"].(bool); !streamed {
		return false
	}
	options, _ := payload["stream_options"].(map[string]any)
	if options == nil {
		return false
	}
	include, _ := options["include_usage"].(bool)
	return include
}

// mistralSSEAdapter 包装上游 SSE 响应体：逐帧剥离私有字段并把 content
// parts 折成 delta.content / delta.reasoning_content；捕获内联 usage，
// 请求了 include_usage 时在 [DONE] 前补一个标准 choices:[] usage 帧。
type mistralSSEAdapter struct {
	source       io.Reader
	reader       *bufio.Reader
	out          bytes.Buffer
	includeUsage bool
	usage        Usage
	usageRaw     map[string]any
	id           string
	model        string
	created      int64
	finished     bool
	err          error
}

func newMistralSSEAdapter(source io.Reader, includeUsage bool) *mistralSSEAdapter {
	return &mistralSSEAdapter{
		source:       source,
		reader:       acquireSSEReader(source),
		includeUsage: includeUsage,
		usage:        Usage{Source: "unavailable"},
	}
}

func (a *mistralSSEAdapter) Read(p []byte) (int, error) {
	for a.out.Len() == 0 {
		if a.finished {
			return 0, io.EOF
		}
		if a.err != nil {
			return 0, a.err
		}
		a.fill()
	}
	return a.out.Read(p)
}

func (a *mistralSSEAdapter) Close() error {
	releaseSSEReader(a.reader)
	if closer, ok := a.source.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// Usage 返回观测到的上游 usage；没有 usage 时为 unavailable，
// 由调用方决定是否回退到流内旁路采集的结果。
func (a *mistralSSEAdapter) Usage() Usage {
	if a.usage.Source != "upstream" {
		return Usage{Source: "unavailable"}
	}
	return a.usage
}

func (a *mistralSSEAdapter) fill() {
	line, err := a.reader.ReadBytes('\n')
	if len(line) > 0 {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			a.handleData(bytes.TrimSpace(trimmed[len("data:"):]))
		}
	}
	if err != nil {
		if err == io.EOF {
			a.finish()
			a.finished = true
			return
		}
		a.err = err
	}
}

func (a *mistralSSEAdapter) handleData(payload []byte) {
	if bytes.Equal(payload, []byte("[DONE]")) {
		a.finish()
		a.writeRaw([]byte("[DONE]"))
		a.finished = true
		return
	}
	var chunk map[string]any
	if err := json.Unmarshal(payload, &chunk); err != nil {
		a.writeRaw(payload)
		return
	}
	object := stringValue(chunk["object"])
	if object != "chat.completion.chunk" {
		// 错误帧与未知事件不是 chat chunk，保持原样避免损坏语义。
		a.writeRaw(payload)
		return
	}
	a.capture(chunk)
	normalized := make(map[string]any, len(chunk))
	for key, value := range chunk {
		if mistralChunkFields[key] {
			normalized[key] = value
		}
	}
	if choices, ok := chunk["choices"].([]any); ok {
		streamed := make([]any, len(choices))
		for i, rawChoice := range choices {
			choice, _ := rawChoice.(map[string]any)
			clean := make(map[string]any, len(choice))
			for key, value := range choice {
				if mistralChoiceFields[key] {
					clean[key] = value
				}
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				clean["delta"] = normalizeMistralDelta(delta)
			}
			streamed[i] = clean
		}
		normalized["choices"] = streamed
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		a.writeRaw(payload)
		return
	}
	a.writeRaw(encoded)
}

var mistralChunkFields = map[string]bool{
	"id": true, "object": true, "created": true, "model": true,
	"choices": true, "system_fingerprint": true, "service_tier": true,
}

func (a *mistralSSEAdapter) capture(chunk map[string]any) {
	if id := stringValue(chunk["id"]); id != "" {
		a.id = id
	}
	if model := stringValue(chunk["model"]); model != "" {
		a.model = model
	}
	if created := number(chunk["created"]); created != 0 {
		a.created = created
	}
	if rawUsage, ok := chunk["usage"].(map[string]any); ok {
		a.usageRaw = rawUsage
		if usage := usageFromMap(map[string]any{"usage": rawUsage}); usage.Source == "upstream" {
			a.usage = usage
		}
	}
}

// finish 在流结束时补发标准 usage 尾帧；无论是否补发，都只执行一次。
func (a *mistralSSEAdapter) finish() {
	if a.finished {
		return
	}
	a.finished = true
	if !a.includeUsage || a.usage.Source != "upstream" {
		return
	}
	total := a.usage.Total
	if total == 0 {
		total = a.usage.Prompt + a.usage.Completion
	}
	usage := map[string]any{
		"prompt_tokens":     a.usage.Prompt,
		"completion_tokens": a.usage.Completion,
		"total_tokens":      total,
	}
	for _, key := range []string{"prompt_tokens_details", "completion_tokens_details"} {
		if details, ok := a.usageRaw[key]; ok {
			usage[key] = details
		}
	}
	id := a.id
	if id == "" {
		id = "chatcmpl-mistral"
	}
	chunk := map[string]any{
		"id": id, "object": "chat.completion.chunk",
		"created": a.created, "model": a.model,
		"choices": []any{}, "usage": usage,
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	a.writeRaw(encoded)
}

func (a *mistralSSEAdapter) writeRaw(payload []byte) {
	a.out.WriteString("data: ")
	a.out.Write(payload)
	a.out.WriteString("\n\n")
}
