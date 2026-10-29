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
		b.streamAnthropic(w, r, prompt, model)
		return
	}
	b.completeAnthropic(w, r, prompt, model)
}

func (b *Bridge) streamAnthropic(w http.ResponseWriter, r *http.Request, prompt, model string) {
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

	collector, runErr := b.run(r.Context(), prompt, model, func(text, _ string) error {
		if text == "" {
			return nil
		}
		return sse.event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
	}, b.cfg.EmitToolActivity)
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			return
		}
		_ = sse.event("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": runErr.Error()},
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
	usage := collector.usage()
	_ = sse.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": usage.OutputTokens},
	})
	_ = sse.event("message_stop", map[string]any{"type": "message_stop"})
}

func (b *Bridge) completeAnthropic(w http.ResponseWriter, r *http.Request, prompt, model string) {
	collector, err := b.run(r.Context(), prompt, model, nil, false)
	if err != nil {
		status, _ := bridgeErrorStatus(err)
		writeAnthropicError(w, status, "api_error", err.Error())
		return
	}
	text := collector.textString()
	if text == "" && collector.resultErr != "" {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", collector.resultErr)
		return
	}
	usage := collector.usage()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            "msg_" + randomID(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []any{map[string]any{"type": "text", "text": text}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  usage.InputTokens,
			"output_tokens": usage.OutputTokens,
		},
	})
}
