package agybridge

import (
	"strings"
	"testing"
)

func TestFormatOpenAIPrompt(t *testing.T) {
	req := openAIRequest{
		Model: "m",
		Messages: []openAIMessage{
			{Role: "system", Content: "be nice"},
			{Role: "user", Content: []any{
				map[string]any{"type": "text", "text": "hello"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:..."}},
			}},
			{Role: "assistant", ToolCalls: []openAIToolCall{{
				ID: "call_1", Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "lookup", Arguments: `{"id":1}`},
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: "result"},
		},
	}
	prompt, err := formatOpenAIPrompt(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[system]\nbe nice",
		"[user]\nhello\n[image omitted]",
		"[assistant tool calls]",
		`"name":"lookup"`,
		"[tool result (call_1)]\nresult",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestFormatAnthropicPrompt(t *testing.T) {
	req := anthropicRequest{
		Model:  "m",
		System: []any{map[string]any{"type": "text", "text": "be strict"}},
		Messages: []anthropicMessage{
			{Role: "user", Content: []any{
				map[string]any{"type": "text", "text": "hello"},
				map[string]any{"type": "image", "source": map[string]any{"type": "base64"}},
			}},
			{Role: "assistant", Content: []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "lookup"},
			}},
			{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "content": []any{map[string]any{"type": "text", "text": "ok"}}},
			}},
		},
	}
	prompt, err := formatAnthropicPrompt(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[system]\nbe strict",
		"[user]\nhello\n[image omitted]",
		"[assistant]\n",
		`"type":"tool_use"`,
		"[user]\nok",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestFormatPromptRejectsEmptyMessages(t *testing.T) {
	if _, err := formatOpenAIPrompt(openAIRequest{}); err == nil {
		t.Fatal("expected error for empty OpenAI messages")
	}
	if _, err := formatAnthropicPrompt(anthropicRequest{}); err == nil {
		t.Fatal("expected error for empty Anthropic messages")
	}
}
