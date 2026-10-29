package agybridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"
)

func (b *Bridge) handleOpenAI(w http.ResponseWriter, r *http.Request) {
	var req openAIRequest
	if err := readJSONBody(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "invalid_request_error")
		return
	}
	prompt, err := formatOpenAIPrompt(req)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	model := b.modelFor(req.Model)
	if req.Stream {
		b.streamOpenAI(w, r, req, prompt, model)
		return
	}
	b.completeOpenAI(w, r, prompt, model)
}

func (b *Bridge) streamOpenAI(w http.ResponseWriter, r *http.Request, req openAIRequest, prompt, model string) {
	sse, err := newSSEWriter(w)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}
	id := "chatcmpl-" + randomID()
	created := time.Now().Unix()
	_ = writeOpenAIChunk(sse, id, created, model, map[string]any{"role": "assistant"}, nil)

	collector, runErr := b.run(r.Context(), prompt, model, func(text, thinking string) error {
		if text != "" {
			if err := writeOpenAIChunk(sse, id, created, model, map[string]any{"content": text}, nil); err != nil {
				return err
			}
		}
		if thinking != "" && b.cfg.EmitThinking {
			if err := writeOpenAIChunk(sse, id, created, model, map[string]any{"reasoning_content": thinking}, nil); err != nil {
				return err
			}
		}
		return nil
	}, b.cfg.EmitToolActivity)
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			return
		}
		_ = sse.data(map[string]any{"error": map[string]any{
			"message": runErr.Error(),
			"type":    "upstream_error",
			"code":    "agy_error",
		}})
		_ = sse.raw("data: [DONE]\n\n")
		return
	}
	stop := "stop"
	_ = writeOpenAIChunk(sse, id, created, model, map[string]any{}, &stop)
	usage := collector.usage()
	if (req.StreamOptions != nil && req.StreamOptions.IncludeUsage) || usage.InputTokens+usage.OutputTokens+usage.TotalTokens > 0 {
		_ = sse.data(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{},
			"usage":   openAIUsage(usage),
		})
	}
	_ = sse.raw("data: [DONE]\n\n")
}

func (b *Bridge) completeOpenAI(w http.ResponseWriter, r *http.Request, prompt, model string) {
	collector, err := b.run(r.Context(), prompt, model, nil, false)
	if err != nil {
		status, code := bridgeErrorStatus(err)
		writeAPIError(w, status, err.Error(), code)
		return
	}
	text := collector.textString()
	if text == "" && collector.resultErr != "" {
		writeAPIError(w, http.StatusBadGateway, collector.resultErr, "agy_error")
		return
	}
	message := map[string]any{"role": "assistant", "content": text}
	if thinking := collector.thinkingString(); thinking != "" && b.cfg.EmitThinking {
		message["reasoning_content"] = thinking
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-" + randomID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": "stop",
		}},
		"usage": openAIUsage(collector.usage()),
	})
}

func (b *Bridge) run(ctx context.Context, prompt, model string, onDelta func(text, thinking string) error, emitTools bool) (*streamCollector, error) {
	collector := newStreamCollector(emitTools)
	err := b.runner.Run(ctx, RunRequest{Prompt: prompt, Model: model}, func(ev Event) error {
		text, thinking := collector.consume(ev)
		if onDelta != nil && (text != "" || thinking != "") {
			return onDelta(text, thinking)
		}
		return nil
	})
	return collector, err
}

func writeOpenAIChunk(sse *sseWriter, id string, created int64, model string, delta map[string]any, finishReason *string) error {
	return sse.data(map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	})
}

func openAIUsage(u Usage) map[string]any {
	total := u.TotalTokens
	if total == 0 {
		total = u.InputTokens + u.OutputTokens
	}
	out := map[string]any{
		"prompt_tokens":     u.InputTokens,
		"completion_tokens": u.OutputTokens,
		"total_tokens":      total,
	}
	if u.CacheReadTokens > 0 {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": u.CacheReadTokens}
	}
	if u.ThinkingTokens > 0 {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": u.ThinkingTokens}
	}
	return out
}

func bridgeErrorStatus(err error) (int, string) {
	var auth *AuthError
	if errors.As(err, &auth) {
		return http.StatusBadGateway, "agy_auth_required"
	}
	if errors.Is(err, ErrNoResult) {
		return http.StatusBadGateway, "agy_no_result"
	}
	return http.StatusBadGateway, "agy_error"
}

func randomID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
