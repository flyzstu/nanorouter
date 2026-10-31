package agybridge

import (
	"fmt"
	"strings"

	json "github.com/goccy/go-json"
)

// ToolCall 是 bridge 从 agy 文本里解析出的下游工具调用。
type ToolCall struct {
	ID        string
	Name      string
	Arguments any
}

// appendToolProtocol 把下游工具 schema 和严格的输出协议附到 prompt 末尾。
// 这是 prompt 级工具协议：agy 不执行工具，只输出一个 JSON，bridge 再转成
// 标准 tool_calls 交给下游 agent 执行。
func appendToolProtocol(prompt string, tools []any, dialect string) (string, error) {
	if len(tools) == 0 {
		return prompt, nil
	}
	specs := make([]string, 0, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, description, parameters := toolSpec(tool, dialect)
		if name == "" {
			continue
		}
		if encoded, err := json.Marshal(parameters); err == nil {
			parameters = string(encoded)
		}
		spec := "- name: " + name + "\n"
		if description != "" {
			spec += "  description: " + description + "\n"
		}
		spec += "  parameters: " + fmt.Sprint(parameters)
		specs = append(specs, spec)
	}
	if len(specs) == 0 {
		return prompt, nil
	}
	if len(specs) > 256 {
		specs = specs[:256]
	}
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\n[client tools]\n")
	b.WriteString(strings.Join(specs, "\n"))
	b.WriteString("\n\n[tool protocol]\n")
	b.WriteString("The client owns tool execution. Do not execute any tool yourself.\n")
	b.WriteString("If you need a client tool, reply with ONLY this JSON and nothing else:\n")
	b.WriteString(`{"tool_call":{"name":"<tool name>","arguments":{...}}}` + "\n")
	b.WriteString("For multiple tools use:\n")
	b.WriteString(`{"tool_calls":[{"name":"<tool name>","arguments":{...}}]}` + "\n")
	b.WriteString("Otherwise answer normally in text.\n")
	return b.String(), nil
}

func toolSpec(tool map[string]any, dialect string) (string, string, any) {
	if dialect == "anthropic" {
		name, _ := tool["name"].(string)
		description, _ := tool["description"].(string)
		parameters := tool["input_schema"]
		if parameters == nil {
			parameters = map[string]any{"type": "object"}
		}
		return name, description, parameters
	}
	fn, _ := tool["function"].(map[string]any)
	if fn == nil {
		return "", "", nil
	}
	name, _ := fn["name"].(string)
	description, _ := fn["description"].(string)
	parameters := fn["parameters"]
	if parameters == nil {
		parameters = map[string]any{"type": "object"}
	}
	return name, description, parameters
}

// parseToolCalls 只接受“整段文本就是一个 JSON 对象”的严格格式，避免把普通
// 回答里的 JSON 片段误判成工具调用。
func parseToolCalls(text string) ([]ToolCall, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, false
	}
	text = stripCodeFence(text)
	var envelope map[string]any
	if json.Unmarshal([]byte(text), &envelope) != nil {
		return nil, false
	}
	if raw, ok := envelope["tool_call"].(map[string]any); ok {
		if call, ok := parseOneToolCall(raw); ok {
			return []ToolCall{call}, true
		}
	}
	if raw, ok := envelope["tool_calls"].([]any); ok {
		calls := make([]ToolCall, 0, len(raw))
		for _, item := range raw {
			tool, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if call, ok := parseOneToolCall(tool); ok {
				calls = append(calls, call)
			}
		}
		if len(calls) > 0 {
			return calls, true
		}
	}
	return nil, false
}

func parseOneToolCall(raw map[string]any) (ToolCall, bool) {
	name, _ := raw["name"].(string)
	if name == "" {
		if fn, ok := raw["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
			if name == "" {
				return ToolCall{}, false
			}
			return ToolCall{Name: name, Arguments: fn["arguments"]}, true
		}
		return ToolCall{}, false
	}
	args := raw["arguments"]
	if args == nil {
		args = map[string]any{}
	}
	return ToolCall{Name: name, Arguments: args}, true
}

func stripCodeFence(text string) string {
	if !strings.HasPrefix(text, "```") {
		return text
	}
	lines := strings.Split(text, "\n")
	if len(lines) < 2 {
		return text
	}
	lines = lines[1:]
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "```") {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
