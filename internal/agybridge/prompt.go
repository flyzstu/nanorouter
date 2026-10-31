package agybridge

import (
	"fmt"
	"strings"

	json "github.com/goccy/go-json"
)

type openAIRequest struct {
	Model               string            `json:"model"`
	Messages            []openAIMessage   `json:"messages"`
	Stream              bool              `json:"stream"`
	StreamOptions       *openAIStreamOpts `json:"stream_options"`
	MaxTokens           *int              `json:"max_tokens"`
	MaxCompletionTokens *int              `json:"max_completion_tokens"`
	Temperature         *float64          `json:"temperature"`
	Tools               []any             `json:"tools"`
	ToolChoice          any               `json:"tool_choice"`
}

type openAIStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content"`
	Name       string           `json:"name"`
	ToolCalls  []openAIToolCall `json:"tool_calls"`
	ToolCallID string           `json:"tool_call_id"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	System      any                `json:"system"`
	Messages    []anthropicMessage `json:"messages"`
	Stream      bool               `json:"stream"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature *float64           `json:"temperature"`
	Tools       []any              `json:"tools"`
	ToolChoice  any                `json:"tool_choice"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// formatOpenAIPrompt 把 OpenAI 消息数组压成一段文本，交给 agy 的 -p。
// 网关本身不执行下游工具调用，工具定义与调用只作为对话上下文保留。
func formatOpenAIPrompt(req openAIRequest) (string, error) {
	if len(req.Messages) == 0 {
		return "", fmt.Errorf("messages is required")
	}
	var b strings.Builder
	b.WriteString("The following is a conversation transcript. Produce the next assistant reply.\n\n")
	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		content := formatOpenAIContent(msg.Content)
		switch role {
		case "system", "developer":
			writePromptSection(&b, "system", content)
		case "assistant":
			writePromptSection(&b, "assistant", content)
			if len(msg.ToolCalls) > 0 {
				if encoded, err := json.Marshal(msg.ToolCalls); err == nil {
					writePromptSection(&b, "assistant tool calls", string(encoded))
				}
			}
		case "tool", "function":
			label := "tool result"
			if msg.Name != "" {
				label += " " + msg.Name
			}
			if msg.ToolCallID != "" {
				label += " (" + msg.ToolCallID + ")"
			}
			writePromptSection(&b, label, content)
		default:
			writePromptSection(&b, "user", content)
		}
	}
	prompt := strings.TrimSpace(b.String())
	if prompt == "" {
		return "", fmt.Errorf("messages is empty")
	}
	return prompt, nil
}

// formatAnthropicPrompt 把 Anthropic system/messages 压成同一段文本。
func formatAnthropicPrompt(req anthropicRequest) (string, error) {
	if len(req.Messages) == 0 {
		return "", fmt.Errorf("messages is required")
	}
	var b strings.Builder
	b.WriteString("The following is a conversation transcript. Produce the next assistant reply.\n\n")
	if system := formatAnthropicContent(req.System); system != "" {
		writePromptSection(&b, "system", system)
	}
	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		content := formatAnthropicContent(msg.Content)
		if role == "assistant" {
			writePromptSection(&b, "assistant", content)
		} else {
			writePromptSection(&b, "user", content)
		}
	}
	prompt := strings.TrimSpace(b.String())
	if prompt == "" {
		return "", fmt.Errorf("messages is empty")
	}
	return prompt, nil
}

func writePromptSection(b *strings.Builder, label, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	b.WriteString("[")
	b.WriteString(label)
	b.WriteString("]\n")
	b.WriteString(content)
	b.WriteString("\n\n")
}

func formatOpenAIContent(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		parts := make([]string, 0, len(x))
		for _, raw := range x {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := part["type"].(string)
			switch typ {
			case "text", "input_text", "output_text":
				if text, _ := part["text"].(string); text != "" {
					parts = append(parts, text)
				}
			case "image_url", "input_image", "image":
				parts = append(parts, "[image omitted]")
			case "tool_result":
				parts = append(parts, formatAnthropicContent(part["content"]))
			default:
				if text, _ := part["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		if encoded, err := json.Marshal(v); err == nil {
			return string(encoded)
		}
		return fmt.Sprint(v)
	}
}

func formatAnthropicContent(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		parts := make([]string, 0, len(x))
		for _, raw := range x {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := part["type"].(string)
			switch typ {
			case "text":
				if text, _ := part["text"].(string); text != "" {
					parts = append(parts, text)
				}
			case "image":
				parts = append(parts, "[image omitted]")
			case "tool_use":
				if encoded, err := json.Marshal(part); err == nil {
					parts = append(parts, string(encoded))
				}
			case "tool_result":
				parts = append(parts, formatAnthropicContent(part["content"]))
			default:
				if text, _ := part["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		if encoded, err := json.Marshal(v); err == nil {
			return string(encoded)
		}
		return fmt.Sprint(v)
	}
}

// openAIContinuation 只保留最后一条 user 及其后的 tool 结果。
// 会话已经绑定 agy conversation 时，历史由 agy 自己维护，不能整段重发。
func openAIContinuation(req openAIRequest) (string, bool) {
	start := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		role := strings.ToLower(strings.TrimSpace(req.Messages[i].Role))
		if role == "user" {
			start = i
			break
		}
		if role == "assistant" {
			break
		}
	}
	if start < 0 {
		return "", false
	}
	var b strings.Builder
	for _, msg := range req.Messages[start:] {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		content := formatOpenAIContent(msg.Content)
		switch role {
		case "assistant":
			writePromptSection(&b, "assistant", content)
		case "tool", "function":
			label := "tool result"
			if msg.Name != "" {
				label += " " + msg.Name
			}
			if msg.ToolCallID != "" {
				label += " (" + msg.ToolCallID + ")"
			}
			writePromptSection(&b, label, content)
		default:
			writePromptSection(&b, "user", content)
		}
	}
	prompt := strings.TrimSpace(b.String())
	return prompt, prompt != ""
}

// anthropicContinuation 与 openAIContinuation 同理。
func anthropicContinuation(req anthropicRequest) (string, bool) {
	start := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		role := strings.ToLower(strings.TrimSpace(req.Messages[i].Role))
		if role == "user" {
			start = i
			break
		}
		if role == "assistant" {
			break
		}
	}
	if start < 0 {
		return "", false
	}
	var b strings.Builder
	for _, msg := range req.Messages[start:] {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		content := formatAnthropicContent(msg.Content)
		if role == "assistant" {
			writePromptSection(&b, "assistant", content)
		} else {
			writePromptSection(&b, "user", content)
		}
	}
	prompt := strings.TrimSpace(b.String())
	return prompt, prompt != ""
}
