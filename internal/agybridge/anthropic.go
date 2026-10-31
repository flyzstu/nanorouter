package agybridge

import (
	"context"
	"errors"
	"net/http"
)

func (b *Bridge) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	var req anthropicRequest
	if err := readJSONBody(w, r, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	promptTools := b.transparent && b.cfg.ToolBridge == "prompt" && len(req.Tools) > 0
	if b.transparent && (len(req.Tools) > 0 || req.ToolChoice != nil) && !promptTools {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"agy-bridge model profile does not execute downstream tools; use a native model upstream, -tool-bridge prompt, or -profile agent")
		return
	}
	prompt, err := formatAnthropicPrompt(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if promptTools {
		if prompt, err = appendToolProtocol(prompt, req.Tools, "anthropic"); err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
	}
	model := b.modelFor(req.Model)
	if req.Stream {
		b.streamAnthropic(w, r, req, prompt, model, promptTools)
		return
	}
	b.completeAnthropic(w, r, req, prompt, model, promptTools)
}

func (b *Bridge) streamAnthropic(w http.ResponseWriter, r *http.Request, req anthropicRequest, prompt, model string, promptTools bool) {
	sse, err := newSSEWriter(w)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	id := "msg_" + randomID()
	_ = sse.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})

	if promptTools {
		collector, runErr := b.runWithContinuity(r.Context(), r, model, prompt, func() (string, bool) {
			return anthropicContinuation(req)
		}, nil)
		if runErr != nil {
			writeAnthropicStreamError(sse, runErr)
			return
		}
		text := collector.textString()
		if calls, ok := parseToolCalls(text); ok {
			writeAnthropicToolUseBlocks(sse, calls, 0)
			writeAnthropicStop(sse, "tool_use", collector.usage())
			return
		}
		_ = sse.event("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		if text != "" {
			_ = sse.event("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": text},
			})
		}
		_ = sse.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		writeAnthropicStop(sse, "end_turn", collector.usage())
		return
	}

	_ = sse.event("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	collector, runErr := b.runWithContinuity(r.Context(), r, model, prompt, func() (string, bool) {
		return anthropicContinuation(req)
	}, func(text, _ string) error {
		if text == "" {
			return nil
		}
		return sse.event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
	})
	if runErr != nil {
		writeAnthropicStreamError(sse, runErr)
		return
	}
	if collector.textString() == "" && collector.resultErr != "" {
		_ = sse.event("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": collector.resultErr},
		})
		return
	}
	_ = sse.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	tools := collector.toolEvents()
	calls := make([]ToolCall, 0, len(tools))
	for _, tool := range tools {
		calls = append(calls, ToolCall{Name: tool.Name, Arguments: tool.Input})
	}
	writeAnthropicToolUseBlocks(sse, calls, 1)
	stopReason := "end_turn"
	if len(calls) > 0 {
		stopReason = "tool_use"
	}
	writeAnthropicStop(sse, stopReason, collector.usage())
}

func (b *Bridge) completeAnthropic(w http.ResponseWriter, r *http.Request, req anthropicRequest, prompt, model string, promptTools bool) {
	collector, err := b.runWithContinuity(r.Context(), r, model, prompt, func() (string, bool) {
		return anthropicContinuation(req)
	}, nil)
	if err != nil {
		writeClassifiedAnthropicError(w, err)
		return
	}
	text := collector.textString()
	if text == "" && collector.resultErr != "" {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", collector.resultErr)
		return
	}
	content := []any{}
	stopReason := "end_turn"
	if promptTools {
		if calls, ok := parseToolCalls(text); ok {
			stopReason = "tool_use"
			content = append(content, anthropicToolUseObjects(calls)...)
		} else {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
	} else {
		content = append(content, map[string]any{"type": "text", "text": text})
		if tools := collector.toolEvents(); len(tools) > 0 {
			stopReason = "tool_use"
			calls := make([]ToolCall, 0, len(tools))
			for _, tool := range tools {
				calls = append(calls, ToolCall{Name: tool.Name, Arguments: tool.Input})
			}
			content = append(content, anthropicToolUseObjects(calls)...)
		}
	}
	usage := collector.usage()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            "msg_" + randomID(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  usage.InputTokens,
			"output_tokens": usage.OutputTokens,
		},
	})
}

func writeAnthropicToolUseBlocks(sse *sseWriter, calls []ToolCall, startIndex int) {
	for i, call := range calls {
		if call.ID == "" {
			call.ID = "toolu_" + randomID()
		}
		index := startIndex + i
		_ = sse.event("content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{
				"type": "tool_use", "id": call.ID, "name": call.Name, "input": map[string]any{},
			},
		})
		_ = sse.event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": toolArgsJSON(call.Arguments)},
		})
		_ = sse.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
	}
}

func anthropicToolUseObjects(calls []ToolCall) []any {
	out := make([]any, 0, len(calls))
	for _, call := range calls {
		if call.ID == "" {
			call.ID = "toolu_" + randomID()
		}
		out = append(out, map[string]any{
			"type": "tool_use", "id": call.ID, "name": call.Name, "input": call.Arguments,
		})
	}
	return out
}

func writeAnthropicStop(sse *sseWriter, stopReason string, usage Usage) {
	_ = sse.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": usage.OutputTokens},
	})
	_ = sse.event("message_stop", map[string]any{"type": "message_stop"})
}

func writeAnthropicStreamError(sse *sseWriter, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	info := classifyError(err)
	_ = sse.event("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": anthropicErrorType(info.Kind), "message": info.Message},
	})
}

func writeClassifiedAnthropicError(w http.ResponseWriter, err error) {
	info := classifyError(err)
	w.Header().Set("X-Agy-Error-Kind", info.Kind)
	writeAnthropicError(w, info.Status, anthropicErrorType(info.Kind), info.Message)
}

func anthropicErrorType(kind string) string {
	switch kind {
	case "auth":
		return "authentication_error"
	case "rate_limit":
		return "rate_limit_error"
	case "context_window":
		return "invalid_request_error"
	case "eligibility":
		return "permission_error"
	default:
		return "api_error"
	}
}
