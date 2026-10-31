package agybridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	json "github.com/goccy/go-json"
)

func (b *Bridge) handleOpenAI(w http.ResponseWriter, r *http.Request) {
	var req openAIRequest
	if err := readJSONBody(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "invalid_request_error")
		return
	}
	promptTools := b.transparent && b.cfg.ToolBridge == "prompt" && len(req.Tools) > 0
	if b.transparent && (len(req.Tools) > 0 || req.ToolChoice != nil) && !promptTools {
		writeAPIError(w, http.StatusBadRequest,
			"agy-bridge model profile does not execute downstream tools; use a native model upstream, -tool-bridge prompt, or -profile agent",
			"tools_not_supported")
		return
	}
	prompt, err := formatOpenAIPrompt(req)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	if promptTools {
		if prompt, err = appendToolProtocol(prompt, req.Tools, "openai"); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
	}
	model := b.modelFor(req.Model)
	b.logPrepared(r, "openai", model, req.Stream, len(req.Messages), len(req.Tools), promptTools)
	if req.Stream {
		b.streamOpenAI(w, r, req, prompt, model, promptTools)
		return
	}
	b.completeOpenAI(w, r, req, prompt, model, promptTools)
}

func (b *Bridge) streamOpenAI(w http.ResponseWriter, r *http.Request, req openAIRequest, prompt, model string, promptTools bool) {
	sse, err := newSSEWriter(w)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}
	id := "chatcmpl-" + randomID()
	created := time.Now().Unix()
	_ = writeOpenAIChunk(sse, id, created, model, map[string]any{"role": "assistant"}, nil)

	if promptTools {
		collector, runErr := b.runWithContinuity(r.Context(), r, model, prompt, func() (string, bool) {
			return openAIContinuation(req)
		}, nil)
		if runErr != nil {
			b.logRequestError(r, runErr)
			writeOpenAIStreamError(sse, runErr)
			return
		}
		if thinking := collector.thinkingString(); thinking != "" && b.cfg.EmitThinking {
			_ = writeOpenAIChunk(sse, id, created, model, map[string]any{"reasoning_content": thinking}, nil)
		}
		text := collector.textString()
		if calls, ok := parseToolCalls(text); ok {
			writeOpenAIToolCallChunks(sse, id, created, model, calls)
			stop := "tool_calls"
			_ = writeOpenAIChunk(sse, id, created, model, map[string]any{}, &stop)
			writeOpenAIUsage(sse, id, created, model, collector.usage())
			_ = sse.raw("data: [DONE]\n\n")
			b.logResult(r, model, "tool_calls", collector.usage(), len(calls))
			return
		}
		if text != "" {
			_ = writeOpenAIChunk(sse, id, created, model, map[string]any{"content": text}, nil)
		}
		stop := "stop"
		_ = writeOpenAIChunk(sse, id, created, model, map[string]any{}, &stop)
		writeOpenAIUsage(sse, id, created, model, collector.usage())
		_ = sse.raw("data: [DONE]\n\n")
		b.logResult(r, model, "stop", collector.usage(), 0)
		return
	}

	collector, runErr := b.runWithContinuity(r.Context(), r, model, prompt, func() (string, bool) {
		return openAIContinuation(req)
	}, func(text, thinking string) error {
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
	})
	if runErr != nil {
		b.logRequestError(r, runErr)
		writeOpenAIStreamError(sse, runErr)
		return
	}
	tools := collector.toolEvents()
	calls := make([]ToolCall, 0, len(tools))
	for _, tool := range tools {
		calls = append(calls, ToolCall{Name: tool.Name, Arguments: tool.Input})
	}
	writeOpenAIToolCallChunks(sse, id, created, model, calls)
	stop := "stop"
	if len(calls) > 0 {
		stop = "tool_calls"
	}
	_ = writeOpenAIChunk(sse, id, created, model, map[string]any{}, &stop)
	writeOpenAIUsage(sse, id, created, model, collector.usage())
	_ = sse.raw("data: [DONE]\n\n")
	b.logResult(r, model, stop, collector.usage(), len(calls))
}

func (b *Bridge) completeOpenAI(w http.ResponseWriter, r *http.Request, req openAIRequest, prompt, model string, promptTools bool) {
	collector, err := b.runWithContinuity(r.Context(), r, model, prompt, func() (string, bool) {
		return openAIContinuation(req)
	}, nil)
	if err != nil {
		b.logRequestError(r, err)
		writeClassifiedOpenAIError(w, err)
		return
	}
	text := collector.textString()
	if text == "" && collector.resultErr != "" {
		b.logRequestError(r, errors.New(collector.resultErr))
		writeAPIError(w, http.StatusBadGateway, collector.resultErr, "agy_error")
		return
	}
	message := map[string]any{"role": "assistant", "content": text}
	if thinking := collector.thinkingString(); thinking != "" && b.cfg.EmitThinking {
		message["reasoning_content"] = thinking
	}
	finishReason := "stop"
	toolCallCount := 0
	if promptTools {
		if calls, ok := parseToolCalls(text); ok {
			finishReason = "tool_calls"
			toolCallCount = len(calls)
			message["content"] = ""
			message["tool_calls"] = openAIToolCallObjects(calls)
		}
	} else if tools := collector.toolEvents(); len(tools) > 0 {
		finishReason = "tool_calls"
		toolCallCount = len(tools)
		calls := make([]ToolCall, 0, len(tools))
		for _, tool := range tools {
			calls = append(calls, ToolCall{Name: tool.Name, Arguments: tool.Input})
		}
		message["tool_calls"] = openAIToolCallObjects(calls)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-" + randomID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": openAIUsage(collector.usage()),
	})
	b.logResult(r, model, finishReason, collector.usage(), toolCallCount)
}

func (b *Bridge) run(ctx context.Context, prompt, model, conversationID string, onDelta func(text, thinking string) error) (*streamCollector, error) {
	collector := newStreamCollector(b.cfg.ToolMode)
	err := b.runner.Run(ctx, RunRequest{Prompt: prompt, Model: model, ConversationID: conversationID}, func(ev Event) error {
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

func writeOpenAIToolCallChunks(sse *sseWriter, id string, created int64, model string, calls []ToolCall) {
	for i, call := range calls {
		if call.ID == "" {
			call.ID = "call_" + randomID()
		}
		_ = sse.data(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": i, "id": call.ID, "type": "function",
					"function": map[string]any{"name": call.Name, "arguments": toolArgsJSON(call.Arguments)},
				}}},
				"finish_reason": nil,
			}},
		})
	}
}

func openAIToolCallObjects(calls []ToolCall) []any {
	out := make([]any, 0, len(calls))
	for _, call := range calls {
		if call.ID == "" {
			call.ID = "call_" + randomID()
		}
		out = append(out, map[string]any{
			"id": call.ID, "type": "function",
			"function": map[string]any{"name": call.Name, "arguments": toolArgsJSON(call.Arguments)},
		})
	}
	return out
}

func writeOpenAIUsage(sse *sseWriter, id string, created int64, model string, usage Usage) {
	if usage.InputTokens+usage.OutputTokens+usage.TotalTokens <= 0 {
		return
	}
	_ = sse.data(map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{},
		"usage":   openAIUsage(usage),
	})
}

func writeOpenAIStreamError(sse *sseWriter, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	info := classifyError(err)
	_ = sse.data(map[string]any{"error": map[string]any{
		"message": info.Message,
		"type":    "upstream_error",
		"code":    info.Code,
	}})
	_ = sse.raw("data: [DONE]\n\n")
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

func writeClassifiedOpenAIError(w http.ResponseWriter, err error) {
	info := classifyError(err)
	if info.RetryAfter > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(info.RetryAfter.Seconds())))
	}
	w.Header().Set("X-Agy-Error-Kind", info.Kind)
	writeAPIError(w, info.Status, info.Message, info.Code)
}

func toolArgsJSON(v any) string {
	if v == nil {
		return "{}"
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func randomID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
