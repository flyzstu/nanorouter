package router

// Chat Completions ↔ Anthropic Messages 双向适配。
//
// 设计原则与 responses_adapter.go 一致：只转换语义明确的字段；不能忠实
// 表示的下游特性必须显式 400（unsupported_openai_feature），绝不静默丢弃
// 或伪造等价行为。已知无声降级仅限两类，均在 design.md 记录：
//   - assistant 历史中的 reasoning_content（未签名的思维链 Anthropic 会
//     拒绝，无签名机制可回传，丢弃是业界通行做法）；
//   - OpenAI 侧记账字段（store/metadata/user/service_tier/seed/stream_options），
//     对生成结果无语义影响。
// 该适配的目标上游是 ZCode Coding Plan 的 Messages 网关；对原生
// api.anthropic.com 的差异（如 thinking 签名）由上游自行校验。

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	json "github.com/goccy/go-json"
)

// defaultAnthropicMaxTokens 在下游未携带任何输出上限时使用。Anthropic
// Messages 必须携带 max_tokens，网关不猜模型上限，取保守默认并写入文档。
const defaultAnthropicMaxTokens = 4096

// reasoningEffortBudgets 把 OpenAI 侧 reasoning_effort 映射到 Anthropic
// thinking 预算。档位集合覆盖 OpenAI（minimal/low/medium/high）与
// dsh/pi-ai（xhigh/max）两家的词汇。
var reasoningEffortBudgets = map[string]int64{
	"minimal": 1024,
	"low":     2048,
	"medium":  8192,
	"high":    16384,
	"xhigh":   32768,
	"max":     32768,
}

// anthropicMinThinkingBudget 是 Anthropic 协议的 budget_tokens 下限。
const anthropicMinThinkingBudget = 1024

// chatCompletionsToMessages 将 Chat Completions 请求翻译为 Anthropic
// Messages 请求。入参已过候选清洗与 token 钳制；model 字段由候选循环
// 在翻译后统一替换。
func chatCompletionsToMessages(in map[string]any) (map[string]any, error) {
	for field, check := range map[string]func(any) bool{
		"n":                 func(v any) bool { return number(v) > 1 },
		"logit_bias":        func(v any) bool { bias, _ := v.(map[string]any); return len(bias) > 0 },
		"logprobs":          func(v any) bool { b, _ := v.(bool); return b },
		"top_logprobs":      func(v any) bool { return number(v) > 0 },
		"presence_penalty":  func(v any) bool { f, _ := v.(float64); return f != 0 },
		"frequency_penalty": func(v any) bool { f, _ := v.(float64); return f != 0 },
	} {
		if raw, ok := in[field]; ok && check(raw) {
			return nil, fmt.Errorf("%s cannot be adapted to Anthropic Messages", field)
		}
	}
	if raw, ok := in["response_format"]; ok && raw != nil {
		format, ok := raw.(map[string]any)
		typ := ""
		if ok {
			typ, _ = format["type"].(string)
		}
		if !ok || (typ != "" && typ != "text") {
			return nil, fmt.Errorf("response_format cannot be adapted to Anthropic Messages; structured output requires a native Responses candidate")
		}
	}

	maxTokens := int64(defaultAnthropicMaxTokens)
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		if v := number(in[field]); v > 0 {
			maxTokens = v
			break
		}
	}

	// parallel_tool_calls 与 verbosity（Responses 链带来）无 Messages
	// 等价且在单候选语义下无观察差异，属白名单式映射的自然舍弃，
	// 已记录进 design.md 的已知降级清单。
	out := map[string]any{"max_tokens": maxTokens}
	if stream, _ := in["stream"].(bool); stream {
		out["stream"] = true
	}
	for _, key := range []string{"temperature", "top_p"} {
		if v, ok := in[key].(float64); ok {
			out[key] = v
		}
	}
	// thinking 启用时 Anthropic 协议禁止 temperature/top_p（前者仅允许 1），
	// 原样携带会让一类合法下游请求被网关整体 400，故在翻译层移除。
	dropSamplingForThinking := false
	stop, err := stopSequences(in["stop"])
	if err != nil {
		return nil, err
	}
	if len(stop) > 0 {
		out["stop_sequences"] = stop
	}

	system, messages, err := chatMessagesToAnthropic(in["messages"])
	if err != nil {
		return nil, err
	}
	if system != "" {
		out["system"] = system
	}
	out["messages"] = messages

	if raw, ok := in["tools"].([]any); ok && len(raw) > 0 {
		if choice, _ := in["tool_choice"].(string); choice != "none" {
			tools, err := chatToolsToAnthropic(raw)
			if err != nil {
				return nil, err
			}
			out["tools"] = tools
			if choice, ok := in["tool_choice"]; ok {
				anthropicChoice, err := chatToolChoiceToAnthropic(choice)
				if err != nil {
					return nil, err
				}
				if anthropicChoice != nil {
					out["tool_choice"] = anthropicChoice
				}
			}
		}
		// tool_choice: none 表示不调用工具；Anthropic 没有对应值，
		// 语义等价的表示是整体不携带 tools（结果数与内容不变）。
	}

	if effort, ok := in["reasoning_effort"].(string); ok && effort != "" {
		thinking, err := reasoningEffortToThinking(effort, maxTokens)
		if err != nil {
			return nil, err
		}
		if thinking != nil {
			// Anthropic 要求 budget_tokens 严格小于 max_tokens。
			if budget := number(thinking["budget_tokens"]); budget >= maxTokens {
				thinking["budget_tokens"] = maxTokens - 1
			}
			out["thinking"] = thinking
			dropSamplingForThinking = true
		}
	}
	if dropSamplingForThinking {
		delete(out, "temperature")
		delete(out, "top_p")
	}
	if out["thinking"] != nil {
		// 强制工具选择（any/tool）与 thinking 同样互斥：本地显式失败，
		// 让调用方二选一，而不是等网关 400 后误导为候选故障。
		if choice, ok := out["tool_choice"].(map[string]any); ok {
			if typ, _ := choice["type"].(string); typ == "any" || typ == "tool" {
				return nil, fmt.Errorf("tool_choice %q cannot be combined with reasoning_effort in Anthropic Messages", typ)
			}
		}
	}
	return out, nil
}

func stopSequences(raw any) ([]string, error) {
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case string:
		if v == "" {
			return nil, nil
		}
		return []string{v}, nil
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("stop must be a string or string array")
			}
			if s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("stop must be a string or string array")
	}
}

// reasoningEffortToThinking 映射推理强度档位。"none" 返回 nil（不携带
// thinking 块）；预算不足 1024 时也放弃 thinking——Anthropic 拒绝更小的
// 预算，硬凑只会把请求变成 400。
func reasoningEffortToThinking(effort string, maxTokens int64) (map[string]any, error) {
	if strings.EqualFold(effort, "none") {
		return nil, nil
	}
	budget, ok := reasoningEffortBudgets[strings.ToLower(effort)]
	if !ok {
		return nil, fmt.Errorf("reasoning_effort %q cannot be adapted (supported: minimal, low, medium, high, xhigh, max, none)", effort)
	}
	if maxTokens-1 < anthropicMinThinkingBudget {
		return nil, nil
	}
	if budget > maxTokens-1 {
		budget = maxTokens - 1
	}
	return map[string]any{"type": "enabled", "budget_tokens": budget}, nil
}

func chatToolsToAnthropic(tools []any) ([]any, error) {
	out := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tools must contain objects")
		}
		if typ, _ := tool["type"].(string); typ != "" && typ != "function" {
			return nil, fmt.Errorf("tool type %q cannot be adapted to Anthropic Messages", typ)
		}
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			return nil, fmt.Errorf("function tool requires a function object")
		}
		name, _ := fn["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("function tool requires name")
		}
		converted := map[string]any{"name": name, "input_schema": map[string]any{"type": "object"}}
		if description, ok := fn["description"].(string); ok && description != "" {
			converted["description"] = description
		}
		if schema, ok := fn["parameters"].(map[string]any); ok && len(schema) > 0 {
			converted["input_schema"] = schema
		}
		out = append(out, converted)
	}
	return out, nil
}

func chatToolChoiceToAnthropic(raw any) (any, error) {
	switch choice := raw.(type) {
	case string:
		switch choice {
		case "auto", "":
			return map[string]any{"type": "auto"}, nil
		case "required":
			return map[string]any{"type": "any"}, nil
		case "none":
			return nil, nil
		default:
			return nil, fmt.Errorf("tool_choice %q cannot be adapted", choice)
		}
	case map[string]any:
		typ, _ := choice["type"].(string)
		if typ != "function" {
			return nil, fmt.Errorf("tool_choice type %q cannot be adapted to Anthropic Messages", typ)
		}
		fn, _ := choice["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("function tool_choice requires name")
		}
		return map[string]any{"type": "tool", "name": name}, nil
	default:
		return nil, fmt.Errorf("tool_choice must be a string or object")
	}
}

// chatMessagesToAnthropic 翻译消息数组。连续 tool 消息合并为一条携带
// 多个 tool_result 的 user 消息；连续同角色消息合并（Anthropic 要求
// 严格交替）；system/developer 消息全部收进顶层 system。
func chatMessagesToAnthropic(raw any) (string, []any, error) {
	messages, ok := raw.([]any)
	if !ok || len(messages) == 0 {
		return "", nil, fmt.Errorf("messages is required and must be a non-empty array")
	}
	var system []string
	var out []any
	var pendingToolResults []any
	flushTools := func() {
		if len(pendingToolResults) > 0 {
			out = appendAssistantOrUser(out, "user", pendingToolResults)
			pendingToolResults = nil
		}
	}
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			return "", nil, fmt.Errorf("messages must contain objects")
		}
		role, _ := message["role"].(string)
		switch role {
		case "system", "developer":
			if text := contentText(message["content"]); text != "" {
				system = append(system, text)
			}
		case "tool":
			callID, _ := message["tool_call_id"].(string)
			if callID == "" {
				return "", nil, fmt.Errorf("tool message requires tool_call_id")
			}
			block := map[string]any{"type": "tool_result", "tool_use_id": callID, "content": toolResultContent(message["content"])}
			pendingToolResults = append(pendingToolResults, block)
		case "user", "assistant":
			flushTools()
			blocks, err := chatMessageBlocks(role, message)
			if err != nil {
				return "", nil, err
			}
			if len(blocks) == 0 {
				// 空 content 消息（纯记账角色轮次）不产生 Anthropic 块，
				// 携带空 content 会被网关以 content field required 拒绝。
				continue
			}
			out = appendAssistantOrUser(out, role, blocks)
		default:
			return "", nil, fmt.Errorf("message role %q cannot be adapted to Anthropic Messages", role)
		}
	}
	flushTools()
	if len(out) == 0 {
		return "", nil, fmt.Errorf("messages adapt to an empty conversation")
	}
	if first, ok := out[0].(map[string]any); ok {
		if role, _ := first["role"].(string); role != "user" {
			return "", nil, fmt.Errorf("adapted conversation must start with a user message")
		}
	}
	return strings.Join(system, "\n\n"), out, nil
}

// appendAssistantOrUser 追加消息并在与上一条同角色时合并内容块。
func appendAssistantOrUser(out []any, role string, blocks []any) []any {
	if len(out) > 0 {
		if last, ok := out[len(out)-1].(map[string]any); ok {
			if lastRole, _ := last["role"].(string); lastRole == role {
				existing, _ := last["content"].([]any)
				last["content"] = append(existing, blocks...)
				return out
			}
		}
	}
	return append(out, map[string]any{"role": role, "content": blocks})
}

// chatMessageBlocks 把一条 user/assistant 消息翻译为内容块数组。
func chatMessageBlocks(role string, message map[string]any) ([]any, error) {
	blocks := make([]any, 0, 2)
	rawContent := message["content"]
	switch content := rawContent.(type) {
	case nil:
	case string:
		if content != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": content})
		}
	case []any:
		for _, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("message content parts must be objects")
			}
			switch part["type"] {
			case "text":
				text, _ := part["text"].(string)
				if text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": text})
				}
			case "image_url":
				if role != "user" {
					return nil, fmt.Errorf("assistant image content cannot be adapted to Anthropic Messages")
				}
				block, err := chatImageToAnthropic(part["image_url"])
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, block)
			case "refusal":
				// 拒绝文本是上游生成结果的元数据，回放时并入正文无损。
				text, _ := part["refusal"].(string)
				if text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": text})
				}
			default:
				return nil, fmt.Errorf("content part type %q cannot be adapted to Anthropic Messages", part["type"])
			}
		}
	default:
		return nil, fmt.Errorf("message content must be a string or part array")
	}
	if role == "assistant" {
		toolCalls, _ := message["tool_calls"].([]any)
		for _, rawCall := range toolCalls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("tool_calls must contain objects")
			}
			id, _ := call["id"].(string)
			if id == "" {
				return nil, fmt.Errorf("tool_calls require id")
			}
			fn, _ := call["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("tool_calls require function.name")
			}
			input, err := parseToolArguments(fn["arguments"])
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": id, "name": name, "input": input})
		}
		// reasoning_content（未签名思维链）有意不映射：Anthropic 要求
		// 思维块带上游签名，未签名的会被整个请求拒绝。
	}
	return blocks, nil
}

// chatImageToAnthropic 映射图片内容：data URL 转 base64 source，
// http(s) URL 转 url source，其余显式拒绝。
func chatImageToAnthropic(raw any) (map[string]any, error) {
	url := ""
	switch v := raw.(type) {
	case string:
		url = v
	case map[string]any:
		url, _ = v["url"].(string)
	}
	if url == "" {
		return nil, fmt.Errorf("image_url requires url")
	}
	if strings.HasPrefix(url, "data:") {
		comma := strings.Index(url, ",")
		if comma < 0 {
			return nil, fmt.Errorf("image_url data URL carries no payload")
		}
		meta := strings.TrimPrefix(url[:comma], "data:")
		mediaType := strings.Split(meta, ";")[0]
		switch mediaType {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
		default:
			return nil, fmt.Errorf("image media type %q cannot be adapted to Anthropic Messages", mediaType)
		}
		return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": url[comma+1:]}}, nil
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": url}}, nil
	}
	return nil, fmt.Errorf("image_url must be a data or http(s) URL")
}

func parseToolArguments(raw any) (map[string]any, error) {
	switch args := raw.(type) {
	case map[string]any:
		// 个别客户端把 arguments 发成对象（非标但存在），直通保留。
		return args, nil
	case string:
		return parseToolArgumentsString(args)
	default:
		return nil, fmt.Errorf("tool_call arguments must be a JSON string or object")
	}
}

func parseToolArgumentsString(args string) (map[string]any, error) {
	if strings.TrimSpace(args) == "" {
		return map[string]any{}, nil
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return nil, fmt.Errorf("tool_call arguments are not valid JSON: %w", err)
	}
	if input == nil {
		input = map[string]any{}
	}
	return input, nil
}

// toolResultContent 把 tool 消息的 content 归一为字符串（Anthropic
// tool_result 的 content 接受字符串或块数组）。
func toolResultContent(raw any) string {
	if text, ok := raw.(string); ok {
		return text
	}
	return contentText(raw)
}

/* -------------------------------------------------------------------------- */
/* 响应翻译：Anthropic Messages → Chat Completions                             */
/* -------------------------------------------------------------------------- */

// anthropicStopReasonToChat 映射停止原因。refusal 是 Anthropic 的拒绝
// 信号，OpenAI 侧最接近的是 content_filter。
func anthropicStopReasonToChat(reason string) string {
	switch reason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

// messagesToChatCompletion 把非流式 Anthropic Messages 响应翻译为
// Chat Completions 响应。model 使用下游逻辑路由名。
func messagesToChatCompletion(body []byte, model string) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, fmt.Errorf("empty upstream response")
	}
	blocks, _ := raw["content"].([]any)
	var text strings.Builder
	var reasoning strings.Builder
	var toolCalls []any
	for _, rawBlock := range blocks {
		block, ok := rawBlock.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			text.WriteString(stringValue(block["text"]))
		case "thinking":
			reasoning.WriteString(stringValue(block["thinking"]))
		case "tool_use":
			input := block["input"]
			if input == nil {
				input = map[string]any{}
			}
			arguments, err := json.Marshal(input)
			if err != nil {
				return nil, fmt.Errorf("tool_use input cannot be encoded: %w", err)
			}
			callID, _ := block["id"].(string)
			if callID == "" {
				callID = "call_" + newID()
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": callID, "type": "function",
				"function": map[string]any{"name": stringValue(block["name"]), "arguments": string(arguments)},
			})
		}
	}
	message := map[string]any{"role": "assistant"}
	switch {
	case text.Len() > 0:
		message["content"] = text.String()
	case len(toolCalls) > 0:
		message["content"] = nil
	default:
		message["content"] = ""
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	stopReason, _ := raw["stop_reason"].(string)
	choice := map[string]any{"index": 0, "message": message, "finish_reason": anthropicStopReasonToChat(stopReason), "logprobs": nil}
	response := map[string]any{
		"id":      firstNonEmpty(stringValue(raw["id"]), "chatcmpl-"+newID()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{choice},
	}
	if usage, ok := raw["usage"].(map[string]any); ok {
		// 缓存 token 计入 prompt 侧：Anthropic 的 input_tokens 不含缓存
		// 命中/写入，缓存密集会话不加会系统性少记。
		prompt := number(usage["input_tokens"]) + number(usage["cache_read_input_tokens"]) + number(usage["cache_creation_input_tokens"])
		completion := number(usage["output_tokens"])
		response["usage"] = map[string]any{
			"prompt_tokens": prompt, "completion_tokens": completion,
			"total_tokens": firstNonZero(number(usage["total_tokens"]), prompt+completion),
		}
	}
	return json.Marshal(response)
}

/* -------------------------------------------------------------------------- */
/* 流式翻译：Anthropic SSE → Chat Completions chunks（可链入 Responses）        */
/* -------------------------------------------------------------------------- */

// messagesStreamTranslator 把 Anthropic Messages SSE 事件流翻译为
// Chat Completions chunk。toResponses=true 时把合成的 chunk 喂给
// responseStreamState（复用 chat→Responses 的事件合成），否则直接写出。
type messagesStreamTranslator struct {
	w           http.ResponseWriter
	flush       http.Flusher
	toResponses bool
	state       *responseStreamState
	id          string
	model       string
	blockTools  map[int64]int64
	toolCount   int64
	finish      string
	usage       Usage
	promptSeen  bool
	outputSeen  bool
}

func newMessagesStreamTranslator(w http.ResponseWriter, toResponses bool, model string) *messagesStreamTranslator {
	t := &messagesStreamTranslator{
		w: w, toResponses: toResponses, id: "chatcmpl-" + newID(), model: model,
		blockTools: map[int64]int64{},
		usage:      Usage{Source: "unavailable"},
	}
	// 两个分支都逐事件 flush：toResponses 的事件合成器自身不 flush，
	// 不在这里持住 flusher 会让下游等满缓冲才见事件。
	t.flush, _ = w.(http.Flusher)
	if toResponses {
		t.state = newResponseStreamState(w, model)
	}
	return t
}

// start 输出 SSE 响应头。调用后不能再故障切换（与 streamChatAsResponses 一致）。
func (t *messagesStreamTranslator) start() {
	t.w.Header().Set("Content-Type", "text/event-stream")
	t.w.Header().Set("Cache-Control", "no-cache")
	t.w.WriteHeader(http.StatusOK)
}

// flush 把已写出的事件推给下游。
func (t *messagesStreamTranslator) flushNow() {
	if t.flush != nil {
		t.flush.Flush()
	}
}

func (t *messagesStreamTranslator) writeChunk(delta map[string]any, finish any) {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": finish}
	t.writeData(map[string]any{"id": t.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": t.model, "choices": []any{choice}})
}

func (t *messagesStreamTranslator) writeData(payload map[string]any) {
	if t.toResponses {
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(t.w, "data: %s\n\n", b)
	if t.flush != nil {
		t.flush.Flush()
	}
}

// consume 处理一个 Anthropic SSE 事件的 data 载荷。
func (t *messagesStreamTranslator) consume(event string, payload []byte) error {
	var data map[string]any
	if json.Unmarshal(payload, &data) != nil || data == nil {
		return nil
	}
	switch event {
	case "error":
		message := "upstream stream error"
		if apiError, ok := data["error"].(map[string]any); ok {
			if m, _ := apiError["message"].(string); m != "" {
				message = m
			}
		}
		return fmt.Errorf("%s", message)
	case "message_start":
		message, _ := data["message"].(map[string]any)
		if usage, ok := message["usage"].(map[string]any); ok {
			t.mergeUsage(anthropicPromptTokens(usage), number(usage["output_tokens"]))
		}
		if t.toResponses {
			t.state.ensureCreated()
		} else {
			t.writeChunk(map[string]any{"role": "assistant", "content": ""}, nil)
		}
	case "content_block_start":
		index := number(data["index"])
		block, _ := data["content_block"].(map[string]any)
		blockType, _ := block["type"].(string)
		if blockType == "tool_use" {
			toolIndex := t.toolCount
			t.toolCount++
			t.blockTools[index] = toolIndex
			callID := stringValue(block["id"])
			if callID == "" {
				callID = "call_" + newID()
			}
			toolCall := map[string]any{"index": toolIndex, "id": callID, "type": "function", "function": map[string]any{"name": stringValue(block["name"]), "arguments": ""}}
			if t.toResponses {
				t.state.consume(t.chunkWith(map[string]any{"tool_calls": []any{toolCall}}))
			} else {
				t.writeChunk(map[string]any{"tool_calls": []any{toolCall}}, nil)
			}
		}
	case "content_block_delta":
		index := number(data["index"])
		delta, _ := data["delta"].(map[string]any)
		if delta == nil {
			return nil
		}
		switch delta["type"] {
		case "text_delta":
			text := stringValue(delta["text"])
			if text == "" {
				return nil
			}
			if t.toResponses {
				t.state.consume(t.chunkWith(map[string]any{"content": text}))
			} else {
				t.writeChunk(map[string]any{"content": text}, nil)
			}
		case "input_json_delta":
			partial := stringValue(delta["partial_json"])
			if partial == "" {
				return nil
			}
			toolIndex, ok := t.blockTools[index]
			if !ok {
				return nil
			}
			toolCall := map[string]any{"index": toolIndex, "function": map[string]any{"arguments": partial}}
			if t.toResponses {
				t.state.consume(t.chunkWith(map[string]any{"tool_calls": []any{toolCall}}))
			} else {
				t.writeChunk(map[string]any{"tool_calls": []any{toolCall}}, nil)
			}
		case "thinking_delta":
			thinking := stringValue(delta["thinking"])
			if thinking == "" {
				return nil
			}
			if t.toResponses {
				t.state.consume(t.chunkWith(map[string]any{"reasoning_content": thinking}))
			} else {
				t.writeChunk(map[string]any{"reasoning_content": thinking}, nil)
			}
		}
	case "message_delta":
		if delta, ok := data["delta"].(map[string]any); ok {
			if reason, _ := delta["stop_reason"].(string); reason != "" {
				t.finish = anthropicStopReasonToChat(reason)
			}
		}
		if usage, ok := data["usage"].(map[string]any); ok {
			t.mergeUsage(anthropicPromptTokens(usage), number(usage["output_tokens"]))
		}
	}
	return nil
}

// anthropicPromptTokens 汇总输入侧 token（含缓存命中/写入）。
func anthropicPromptTokens(usage map[string]any) int64 {
	return number(usage["input_tokens"]) + number(usage["cache_read_input_tokens"]) + number(usage["cache_creation_input_tokens"])
}

// complete 收束流：写出 finish_reason、usage 与终止标记。
func (t *messagesStreamTranslator) complete() {
	if t.finish == "" {
		t.finish = "stop"
	}
	if t.toResponses {
		t.state.complete(t.usage)
		return
	}
	t.writeChunk(map[string]any{}, t.finish)
	if t.usage.Source == "upstream" {
		usage := map[string]any{"prompt_tokens": t.usage.Prompt, "completion_tokens": t.usage.Completion, "total_tokens": t.usage.Total}
		t.writeData(map[string]any{"id": t.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": t.model, "choices": []any{}, "usage": usage})
	}
	_, _ = fmt.Fprint(t.w, "data: [DONE]\n\n")
	if t.flush != nil {
		t.flush.Flush()
	}
}

// fail 以流内错误收束（下游已收到 200 与响应头，只能流内报错）。
func (t *messagesStreamTranslator) fail(err error) {
	if t.toResponses {
		t.state.fail(err)
		return
	}
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
	_, _ = fmt.Fprintf(t.w, "data: %s\n\n", b)
	if t.flush != nil {
		t.flush.Flush()
	}
}

func (t *messagesStreamTranslator) chunkWith(delta map[string]any) map[string]any {
	return map[string]any{"id": t.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": t.model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}}
}

// mergeUsage 累积分段到达的 usage：message_start 携带输入侧，message_delta
// 携带累计输出侧，两者相加才是完整 token 量。
func (t *messagesStreamTranslator) mergeUsage(prompt, completion int64) {
	if prompt > t.usage.Prompt {
		t.usage.Prompt = prompt
		t.promptSeen = true
	}
	if completion > t.usage.Completion {
		t.usage.Completion = completion
		t.outputSeen = true
	}
	if t.promptSeen || t.outputSeen {
		t.usage.Source = "upstream"
		t.usage.Total = t.usage.Prompt + t.usage.Completion
	}
}

// streamMessagesAsChat 把 Anthropic Messages SSE 翻译为 Chat Completions
// SSE（toResponses=false），或经 responseStreamState 翻译为 Responses
// 事件流（toResponses=true）。逐事件处理，不完整缓存长回答。
func (r *Router) streamMessagesAsChat(w http.ResponseWriter, resp *http.Response, toResponses bool, model string) (Usage, error) {
	defer resp.Body.Close()
	translator := newMessagesStreamTranslator(w, toResponses, model)
	translator.start()
	reader := acquireSSEReader(resp.Body)
	defer releaseSSEReader(reader)
	event := ""
	for {
		line, err := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		switch {
		case bytes.HasPrefix(trimmed, []byte("event:")):
			event = strings.TrimSpace(string(bytes.TrimPrefix(trimmed, []byte("event:"))))
		case bytes.HasPrefix(trimmed, []byte("data:")):
			payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
			if len(payload) == 0 {
				break
			}
			// Anthropic 不使用 [DONE] 终止符；遇到时按流结束处理以容忍混合上游。
			if string(payload) == "[DONE]" {
				translator.complete()
				return translator.usage, nil
			}
			if streamErr := translator.consume(event, payload); streamErr != nil {
				translator.fail(streamErr)
				translator.flushNow()
				return translator.usage, streamErr
			}
			translator.flushNow()
		}
		if err == io.EOF {
			translator.complete()
			translator.flushNow()
			return translator.usage, nil
		}
		if err != nil {
			translator.fail(err)
			translator.flushNow()
			return translator.usage, err
		}
	}
}

/* -------------------------------------------------------------------------- */
/* 错误信封翻译                                                                */
/* -------------------------------------------------------------------------- */

// anthropicErrorToChat 把 Anthropic 错误信封 {"type":"error","error":{...}}
// 改写为 OpenAI 形状 {"error":{...}}。OpenAI 形状的响应原样通过；
// 解析失败时返回原文，绝不吞掉上游诊断。
func anthropicErrorToChat(body []byte) []byte {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil || raw == nil {
		return body
	}
	if stringValue(raw["type"]) != "error" {
		return body
	}
	apiError, ok := raw["error"].(map[string]any)
	if !ok {
		return body
	}
	converted := map[string]any{"error": apiError}
	if out, err := json.Marshal(converted); err == nil {
		return out
	}
	return body
}

// writeAnthropicAPIError 以 Anthropic 错误信封写出，供 /v1/messages 端点。
// 网关内部码映射到 Anthropic 标准 error type，原始码保留在消息尾注。
func writeAnthropicAPIError(w http.ResponseWriter, status int, message, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]any{"type": anthropicErrorType(typ), "message": message + " (" + typ + ")"}})
}

func anthropicErrorType(typ string) string {
	switch typ {
	case "authentication_error":
		return "authentication_error"
	case "gateway_overloaded", "upstream_rate_limited", "upstream_concurrency_limit", "retry_budget_exhausted":
		return "rate_limit_error"
	case "unknown_model_route", "invalid_json", "invalid_request_body", "unsupported_openai_feature", "unsupported_response_feature":
		return "invalid_request_error"
	case "upstream_error":
		return "api_error"
	default:
		return "api_error"
	}
}

// writeAPIErrorDialect 按下游端点方言写出错误：messages 端点用 Anthropic
// 信封，其余用 OpenAI 信封。
func writeAPIErrorDialect(w http.ResponseWriter, status int, message, typ, endpoint string) {
	if endpoint == "messages" {
		writeAnthropicAPIError(w, status, message, typ)
		return
	}
	writeAPIError(w, status, message, typ)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(values ...int64) int64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}
