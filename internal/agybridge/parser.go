package agybridge

import (
	"bytes"
	"math"
	"strconv"
	"strings"

	json "github.com/goccy/go-json"
)

// maxStreamLineBytes 防止 agy 输出异常长的无换行片段撑爆内存。
const maxStreamLineBytes = 8 << 20

var (
	textStepTypes = map[string]StepKind{
		"agent_text": StepText, "agenttext": StepText, "text": StepText,
		"model_response": StepText, "modelresponse": StepText, "message": StepText,
		"response_text": StepText, "agent_response": StepText, "agentresponse": StepText,
	}
	thinkingStepTypes = map[string]StepKind{
		"thinking": StepThinking, "thought": StepThinking, "reasoning": StepThinking,
	}
	toolStepTypes = map[string]StepKind{
		"tool_call": StepTool, "toolcall": StepTool, "tool": StepTool,
		"tool_use": StepTool, "tooluse": StepTool, "tool_run": StepTool,
		"toolrun": StepTool, "function_call": StepTool,
	}
	titleStepTypes     = map[string]StepKind{"title": StepTitle}
	subagentStepTypes  = map[string]StepKind{"subagent": StepSubagent, "subagent_message": StepSubagent, "subagent_result": StepSubagent}
	userInputStepTypes = map[string]StepKind{"user_input": StepUserInput, "userinput": StepUserInput, "user_message": StepUserInput}
	numericStepTypes   = map[float64]StepKind{
		14: StepThinking, 15: StepText, 23: StepTitle,
		5: StepTool, 7: StepTool, 8: StepTool, 9: StepTool, 17: StepTool,
		21: StepTool, 33: StepTool, 101: StepTool, 132: StepTool, 138: StepTool, 139: StepTool,
	}
)

// StreamParser 增量解析 agy 的 NDJSON 输出。允许撕裂行、噪声行和未知事件。
type StreamParser struct {
	buf []byte

	Lines           int
	Garbage         int
	AuthFailure     bool
	AuthFailureText string
	LastResultErr   string
	tail            []string
}

// Feed 追加一段 stdout，返回其中已经完整的事件。
func (p *StreamParser) Feed(chunk []byte) []Event {
	p.buf = append(p.buf, chunk...)
	var out []Event
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimSuffix(p.buf[:i], []byte{'\r'}))
		p.buf = p.buf[i+1:]
		if ev, ok := p.takeLine(line); ok {
			out = append(out, ev)
		}
	}
	if len(p.buf) > maxStreamLineBytes {
		p.buf = nil
		p.Garbage++
		out = append(out, Event{Kind: EventGarbage})
	}
	return out
}

// Flush 处理最后一段没有换行的输出。
func (p *StreamParser) Flush() []Event {
	rest := strings.TrimSpace(string(p.buf))
	p.buf = nil
	if rest == "" {
		return nil
	}
	if ev, ok := p.takeLine(rest); ok {
		return []Event{ev}
	}
	return nil
}

// Tail 返回最近的原始行，用于错误信息。
func (p *StreamParser) Tail() []string {
	return append([]string(nil), p.tail...)
}

func (p *StreamParser) takeLine(line string) (Event, bool) {
	if strings.TrimSpace(line) == "" {
		return Event{}, false
	}
	p.Lines++
	p.appendTail(line)

	var raw any
	if json.Unmarshal([]byte(line), &raw) != nil {
		p.Garbage++
		if looksLikeAuthFailure(line) {
			p.recordAuthFailure(line)
		}
		return Event{Kind: EventGarbage}, true
	}
	ev := classifyEvent(raw, p.Lines)
	if ev == nil {
		p.Garbage++
		return Event{Kind: EventGarbage}, true
	}
	if ev.Kind == EventResult {
		p.LastResultErr = ev.Error
		if !ev.OK && looksLikeAuthFailure(ev.Error) {
			p.recordAuthFailure(ev.Error)
		}
	}
	return *ev, true
}

func (p *StreamParser) appendTail(line string) {
	const maxTail = 40
	p.tail = append(p.tail, line)
	if len(p.tail) > maxTail {
		p.tail = p.tail[len(p.tail)-maxTail:]
	}
}

func (p *StreamParser) recordAuthFailure(text string) {
	p.AuthFailure = true
	text = strings.TrimSpace(text)
	if text == "" || strings.Contains(p.AuthFailureText, text) {
		return
	}
	if p.AuthFailureText == "" {
		p.AuthFailureText = text
	} else {
		p.AuthFailureText += " " + text
	}
	if len(p.AuthFailureText) > 600 {
		p.AuthFailureText = p.AuthFailureText[:600]
	}
}

func looksLikeAuthFailure(text string) bool {
	s := strings.ToLower(text)
	for _, marker := range []string{
		"please sign in", "authentication failed", "not logged into antigravity",
		"not logged in", "token validation failed", "silent auth failed",
		"please log in", "login required",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func classifyEvent(v any, seq int) *Event {
	o, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	evt, _ := pick(o, "event", "type").(string)
	switch evt {
	case "init", "initialized":
		initObj, _ := o["init"].(map[string]any)
		cid, _ := pick(o, "conversation_id", "conversationId", "session_id", "sessionId").(string)
		if cid == "" && initObj != nil {
			cid, _ = pick(initObj, "conversation_id", "conversationId").(string)
		}
		model, _ := pick(o, "model", "model_name", "modelName").(string)
		if model == "" && initObj != nil {
			model, _ = pick(initObj, "model", "model_name", "modelName").(string)
		}
		return &Event{Kind: EventInit, ConversationID: cid, Model: model, Raw: o}

	case "step_update", "step", "stepUpdate":
		nested, _ := pick(o, "step_update", "stepUpdate", "step").(map[string]any)
		src := o
		if nested != nil {
			src = nested
		}
		stepKey := stepKeyOf(src, seq)
		kind := normalizeStepKind(pick(src, "step_type", "stepType", "type"))
		var tool *ToolInfo
		if kind == StepTool {
			tool = extractTool(src)
		}
		text, fragment := extractText(src)
		state, _ := pick(src, "state").(string)
		if state == "ERROR" && tool != nil && tool.Error == "" {
			tool.Error = "tool execution failed"
		}
		ev := &Event{
			Kind: EventStep, StepKey: stepKey, StepKind: kind,
			Text: text, Fragment: fragment, State: state, Tool: tool, Raw: o,
		}
		if rawUsage, ok := pick(src, "usage").(map[string]any); ok {
			ev.Usage = parseUsage(rawUsage)
		}
		return ev

	case "result", "done", "final":
		result, _ := pick(o, "result").(map[string]any)
		if result == nil {
			result = o
		}
		cid, _ := pick(result, "conversation_id", "conversationId").(string)
		status, _ := pick(result, "status").(string)
		response, _ := pick(result, "response", "text", "content").(string)
		errText, _ := pick(result, "error", "error_message", "errorMessage").(string)
		ok := !strings.EqualFold(status, "ERROR") && errText == ""
		ev := &Event{
			Kind: EventResult, ConversationID: cid, OK: ok,
			Response: response, Error: errText, Raw: o,
		}
		if rawUsage, ok := pick(result, "usage").(map[string]any); ok {
			ev.Usage = parseUsage(rawUsage)
		}
		return ev
	}

	// 没有 event 判别字段时按形状推断。
	if o["usage"] != nil && (o["status"] != nil || o["response"] != nil) {
		return classifyEvent(map[string]any{"event": "result", "result": o}, seq)
	}
	if pick(o, "step_type", "stepType") != nil {
		merged := map[string]any{"event": "step_update"}
		for k, v := range o {
			merged[k] = v
		}
		return classifyEvent(merged, seq)
	}
	if nested, ok := pick(o, "step_update", "stepUpdate").(map[string]any); ok && nested != nil {
		return classifyEvent(map[string]any{"event": "step_update", "step_update": nested}, seq)
	}
	return nil
}

func stepKeyOf(o map[string]any, seq int) string {
	v := pick(o, "step_index", "stepIndex", "idx", "index", "step_idx", "stepIdx", "id", "step_id", "stepId")
	switch x := v.(type) {
	case string:
		if x != "" {
			return x
		}
	case float64:
		return formatNumber(x)
	}
	return formatNumber(float64(seq))
}

func formatNumber(v float64) string {
	if v == math.Trunc(v) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func normalizeStepKind(v any) StepKind {
	switch x := v.(type) {
	case float64:
		if k, ok := numericStepTypes[x]; ok {
			return k
		}
	case string:
		s := strings.ToLower(x)
		if k, ok := textStepTypes[s]; ok {
			return k
		}
		if k, ok := thinkingStepTypes[s]; ok {
			return k
		}
		if k, ok := toolStepTypes[s]; ok {
			return k
		}
		if k, ok := titleStepTypes[s]; ok {
			return k
		}
		if k, ok := subagentStepTypes[s]; ok {
			return k
		}
		if k, ok := userInputStepTypes[s]; ok {
			return k
		}
	}
	return StepUnknown
}

func extractText(o map[string]any) (string, bool) {
	if delta, ok := pick(o, "text_delta", "textDelta").(string); ok {
		return delta, true
	}
	if text, ok := pick(o, "text", "content", "agent_text", "agentText", "output_text", "payload_text").(string); ok {
		return text, false
	}
	if payload, ok := pick(o, "payload", "step_payload", "stepPayload").(map[string]any); ok {
		if text, ok := pick(payload, "text", "content", "agent_text", "agentText").(string); ok {
			return text, false
		}
	}
	return "", false
}

func extractTool(o map[string]any) *ToolInfo {
	info, _ := pick(o, "tool_info", "toolInfo", "tool", "tool_call", "toolCall").(map[string]any)
	src := o
	if info != nil {
		src = info
	}
	name, _ := pick(src, "name", "tool_name", "toolName", "canonical_name").(string)
	if name == "" {
		return nil
	}
	tool := &ToolInfo{
		Name:   name,
		Args:   pick(src, "parameters", "params", "input", "args", "input_json", "inputJson"),
		Output: pick(src, "output", "result", "output_text"),
	}
	switch errVal := pick(src, "error").(type) {
	case string:
		tool.Error = errVal
	case map[string]any:
		tool.Error, _ = errVal["message"].(string)
	}
	return tool
}

func parseUsage(o map[string]any) *Usage {
	u := &Usage{}
	u.InputTokens = firstNumber(o, "input_tokens", "inputTokens", "prompt_tokens")
	u.OutputTokens = firstNumber(o, "output_tokens", "outputTokens", "completion_tokens")
	u.ThinkingTokens = firstNumber(o, "thinking_tokens", "thinkingTokens", "reasoning_tokens")
	u.CacheReadTokens = firstNumber(o, "cache_read_tokens", "cacheReadTokens", "cached_tokens", "cache_read_input_tokens")
	u.CacheWriteTokens = firstNumber(o, "cache_write_tokens", "cacheWriteTokens", "cache_creation_input_tokens")
	u.TotalTokens = firstNumber(o, "total_tokens", "totalTokens")
	return u
}

func firstNumber(o map[string]any, keys ...string) int64 {
	for _, key := range keys {
		if v, ok := o[key].(float64); ok {
			return int64(v)
		}
	}
	return 0
}

func pick(o map[string]any, keys ...string) any {
	for _, key := range keys {
		if v, ok := o[key]; ok && v != nil {
			return v
		}
	}
	return nil
}
