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
	prompt, err := formatAnthropicPrompt(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	model := b.modelFor(req.Model)
	if req.Stream {
		b.streamAnthropic(w, r, req, prompt, model)
		return
	}
	b.completeAnthropic(w, r, req, prompt, model)
}

func (b *Bridge) streamAnthropic(w http.ResponseWriter, r *http.Request, req anthropicRequest, prompt, model string) {
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
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			return
		}
		info := classifyError(runErr)
		_ = sse.event("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": anthropicErrorType(info.Kind), "message": info.Message},
		})
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
	for i, tool := range tools {
		index := i + 1
		_ = sse.event("content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{
				"type": "tool_use", "id": "toolu_" + randomID(), "name": tool.Name, "input": map[string]any{},
			},
		})
		_ = sse.event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": toolArgsJSON(tool.Input)},
		})
		_ = sse.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
	}
	stopReason := "end_turn"
	if len(tools) > 0 {
		stopReason = "tool_use"
	}
	usage := collector.usage()
	_ = sse.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": usage.OutputTokens},
	})
	_ = sse.event("message_stop", map[string]any{"type": "message_stop"})
}

func (b *Bridge) completeAnthropic(w http.ResponseWriter, r *http.Request, req anthropicRequest, prompt, model string) {
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
	content := []any{map[string]any{"type": "text", "text": text}}
	stopReason := "end_turn"
	if tools := collector.toolEvents(); len(tools) > 0 {
		stopReason = "tool_use"
		for _, tool := range tools {
			content = append(content, map[string]any{
				"type": "tool_use", "id": "toolu_" + randomID(), "name": tool.Name, "input": tool.Input,
			})
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
