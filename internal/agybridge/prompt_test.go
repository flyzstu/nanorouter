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

func TestOpenAIContinuation(t *testing.T) {
	req := openAIRequest{Messages: []openAIMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "ok"},
		{Role: "tool", ToolCallID: "call_1", Content: "result"},
		{Role: "user", Content: "second"},
		{Role: "tool", ToolCallID: "call_2", Content: "second-result"},
	}}
	prompt, ok := openAIContinuation(req)
	if !ok {
		t.Fatal("expected continuation")
	}
	if strings.Contains(prompt, "sys") || strings.Contains(prompt, "first") {
		t.Fatalf("continuation leaked history:\n%s", prompt)
	}
	for _, want := range []string{"second", "second-result", "call_2"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("continuation missing %q:\n%s", want, prompt)
		}
	}
}

func TestContinuationRejectsAssistantTail(t *testing.T) {
	req := openAIRequest{Messages: []openAIMessage{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "done"},
	}}
	if _, ok := openAIContinuation(req); ok {
		t.Fatal("assistant tail should not produce a continuation prompt")
	}
	anthropicReq := anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "done"},
	}}
	if _, ok := anthropicContinuation(anthropicReq); ok {
		t.Fatal("assistant tail should not produce an Anthropic continuation prompt")
	}
}

func TestAnthropicContinuation(t *testing.T) {
	req := anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "second"},
	}}
	prompt, ok := anthropicContinuation(req)
	if !ok || !strings.Contains(prompt, "second") || strings.Contains(prompt, "first") {
		t.Fatalf("prompt=%q ok=%v", prompt, ok)
	}
}

func TestOpenAIContentParts(t *testing.T) {
	content := []any{
		map[string]any{"type": "input_text", "text": "input"},
		map[string]any{"type": "output_text", "text": "output"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "x"}},
		map[string]any{"type": "unknown", "text": "fallback"},
	}
	got := formatOpenAIContent(content)
	for _, want := range []string{"input", "output", "[image omitted]", "fallback"} {
		if !strings.Contains(got, want) {
			t.Fatalf("content missing %q: %q", want, got)
		}
	}
}

func TestAnthropicContentParts(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "hello"},
		map[string]any{"type": "image", "source": map[string]any{"type": "base64"}},
		map[string]any{"type": "tool_use", "id": "toolu_1", "name": "lookup"},
		map[string]any{"type": "tool_result", "content": []any{map[string]any{"type": "text", "text": "result"}}},
	}
	got := formatAnthropicContent(content)
	for _, want := range []string{"hello", "[image omitted]", "tool_use", "result"} {
		if !strings.Contains(got, want) {
			t.Fatalf("content missing %q: %q", want, got)
		}
	}
}
