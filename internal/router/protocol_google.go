package router

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// readGoogleToken 从文件读取 Google OAuth token
// 凭据格式统一用 login_google.go 的 GoogleToken，避免两处定义漂移。
func (r *Router) readGoogleToken() (*GoogleToken, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("get home directory: %w", err)
	}

	tokenPath := filepath.Join(home, ".nanorouter", "google_token.json")
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read google token file: %w", err)
	}

	var token GoogleToken
	if err := json.Unmarshal(data, &token); err != nil {
		return nil, fmt.Errorf("parse google token: %w", err)
	}

	if token.AccessToken == "" {
		return nil, fmt.Errorf("google token file missing access_token")
	}

	return &token, nil
}

// getGoogleAccessToken 获取有效的 access token。
// 过期（或 60s 内将过期）时用 refresh_token 刷新并写回凭据文件。
func (r *Router) getGoogleAccessToken(ctx context.Context) (string, error) {
	token, err := r.readGoogleToken()
	if err != nil {
		return "", err
	}
	if token.ExpiresAt > 0 && time.Now().Add(time.Minute).Unix() >= token.ExpiresAt {
		token, err = refreshGoogleToken(ctx, token)
		if err != nil {
			return "", fmt.Errorf("refresh google token: %w", err)
		}
	}
	return token.AccessToken, nil
}

// getGoogleProjectID 获取 Cloud Code Assist 项目 ID
func (r *Router) getGoogleProjectID() (string, error) {
	token, err := r.readGoogleToken()
	if err != nil {
		return "", err
	}
	return token.ProjectID, nil
}

// convertOpenAIToGemini 将 OpenAI Chat Completions 格式转换为 Gemini API 格式
func convertOpenAIToGemini(openaiReq map[string]any) (map[string]any, error) {
	messages, ok := openaiReq["messages"].([]any)
	if !ok {
		return nil, fmt.Errorf("missing or invalid messages field")
	}

	var contents []map[string]any
	var systemInstruction map[string]any

	for _, msg := range messages {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}

		role, _ := msgMap["role"].(string)
		content := msgMap["content"]

		// 处理 system 消息
		if role == "system" {
			if text, ok := content.(string); ok {
				systemInstruction = map[string]any{
					"parts": []map[string]any{
						{"text": text},
					},
				}
			}
			continue
		}

		// 处理 user/assistant 消息
		var parts []map[string]any

		switch c := content.(type) {
		case string:
			parts = append(parts, map[string]any{"text": c})
		case []any:
			// 处理多模态内容（文本 + 图片）
			for _, item := range c {
				itemMap, ok := item.(map[string]any)
				if !ok {
					continue
				}

				itemType, _ := itemMap["type"].(string)
				switch itemType {
				case "text":
					if text, ok := itemMap["text"].(string); ok {
						parts = append(parts, map[string]any{"text": text})
					}
				case "image_url":
					if urlMap, ok := itemMap["image_url"].(map[string]any); ok {
						if url, ok := urlMap["url"].(string); ok {
							// Gemini 使用 inlineData，需要 base64 编码
							// 这里简化处理，实际应该下载图片并编码
							parts = append(parts, map[string]any{
								"inline_data": map[string]any{
									"mime_type": "image/jpeg",
									"data":      url, // 应该是 base64 数据
								},
							})
						}
					}
				}
			}
		}

		if len(parts) > 0 {
			geminiRole := "user"
			if role == "assistant" {
				geminiRole = "model"
			}

			contents = append(contents, map[string]any{
				"role":  geminiRole,
				"parts": parts,
			})
		}
	}

	geminiReq := map[string]any{
		"contents": contents,
	}

	if systemInstruction != nil {
		geminiReq["system_instruction"] = systemInstruction
	}

	// 转换 generationConfig
	if maxTokens, ok := openaiReq["max_tokens"].(float64); ok {
		geminiReq["generationConfig"] = map[string]any{
			"maxOutputTokens": int(maxTokens),
		}
	}

	if temperature, ok := openaiReq["temperature"].(float64); ok {
		if gc, ok := geminiReq["generationConfig"].(map[string]any); ok {
			gc["temperature"] = temperature
		} else {
			geminiReq["generationConfig"] = map[string]any{
				"temperature": temperature,
			}
		}
	}

	if topP, ok := openaiReq["top_p"].(float64); ok {
		if gc, ok := geminiReq["generationConfig"].(map[string]any); ok {
			gc["topP"] = topP
		} else {
			geminiReq["generationConfig"] = map[string]any{
				"topP": topP,
			}
		}
	}

	return geminiReq, nil
}

// convertGeminiToOpenAI 将 Gemini API 响应转换为 OpenAI Chat Completions 格式
func convertGeminiToOpenAI(geminiResp []byte, model string) ([]byte, error) {
	var geminiData map[string]any
	if err := json.Unmarshal(geminiResp, &geminiData); err != nil {
		return nil, fmt.Errorf("parse gemini response: %w", err)
	}

	// Cloud Code Assist 的响应整体包在 response 字段里（{"response": {...}, "traceId": ...}），
	// 解包后再读顶层 candidates；标准 Gemini 直连响应没有该层，保持兼容。
	if inner, ok := geminiData["response"].(map[string]any); ok {
		geminiData = inner
	}

	candidates, ok := geminiData["candidates"].([]any)
	if !ok || len(candidates) == 0 {
		return nil, fmt.Errorf("no candidates in response")
	}

	firstCandidate, ok := candidates[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid candidate format")
	}

	content, ok := firstCandidate["content"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid content format")
	}

	parts, ok := content["parts"].([]any)
	if !ok {
		return nil, fmt.Errorf("invalid parts format")
	}

	var textContent string
	for _, part := range parts {
		partMap, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := partMap["text"].(string); ok {
			textContent += text
		}
	}

	finishReason := "stop"
	if fr, ok := firstCandidate["finishReason"].(string); ok {
		switch fr {
		case "MAX_TOKENS":
			finishReason = "length"
		case "SAFETY":
			finishReason = "content_filter"
		}
	}

	openAIData := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-google-%d", time.Now().Unix()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": textContent,
				},
				"finish_reason": finishReason,
			},
		},
	}

	// 转换 usage
	if usageMetadata, ok := geminiData["usageMetadata"].(map[string]any); ok {
		openAIData["usage"] = map[string]any{
			"prompt_tokens":     usageMetadata["promptTokenCount"],
			"completion_tokens": usageMetadata["candidatesTokenCount"],
			"total_tokens":      usageMetadata["totalTokenCount"],
		}
	}

	result, err := json.Marshal(openAIData)
	if err != nil {
		return nil, fmt.Errorf("marshal openai response: %w", err)
	}

	return result, nil
}

// streamGeminiAsChat 将 Cloud Code Assist 的 SSE 流转换为 OpenAI Chat Completions
// 流式格式。上游是标准 SSE：逐行 `data: {json}`，且真正的响应包在 response 字段里，
// 所以必须按 data 行解析，不能用 json.Decoder 直读 body（会把整个流当一个 JSON）。
func (r *Router) streamGeminiAsChat(w http.ResponseWriter, resp *http.Response, routeName string) (Usage, error) {
	var usage Usage
	id := fmt.Sprintf("chatcmpl-google-%d", time.Now().Unix())
	created := time.Now().Unix()
	finishReason := ""
	sentRole := false

	scanner := bufio.NewScanner(resp.Body)
	// 单个 SSE 事件可能很大（长回复 + usage），放宽默认 64KB 缓冲上限。
	scanner.Buffer(make([]byte, 0, 64<<10), int(maxUpstreamResponseBody))

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}

		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return usage, fmt.Errorf("parse gemini sse data: %w", err)
		}

		// 流内 error 帧：配额/鉴权错误常以 HTTP 200 + error 事件出现，
		// 不识别会被当成空流静默吞掉。
		if errObj, ok := chunk["error"].(map[string]any); ok {
			msg, _ := errObj["message"].(string)
			if msg == "" {
				msg, _ = errObj["status"].(string)
			}
			if msg == "" {
				msg = "unknown gemini stream error"
			}
			return usage, fmt.Errorf("gemini stream error: %s", msg)
		}

		// CCA 把响应包在 response 字段；标准 Gemini 直连流没有该层。
		if inner, ok := chunk["response"].(map[string]any); ok {
			chunk = inner
		}

		candidates, ok := chunk["candidates"].([]any)
		if !ok || len(candidates) == 0 {
			// 只带 usage 的收尾帧也继续处理（下方累计）。
			if usageMetadata, ok := chunk["usageMetadata"].(map[string]any); ok {
				applyGeminiUsage(&usage, usageMetadata)
			}
			continue
		}

		firstCandidate, ok := candidates[0].(map[string]any)
		if !ok {
			continue
		}
		if fr, ok := firstCandidate["finishReason"].(string); ok && fr != "" {
			switch fr {
			case "MAX_TOKENS":
				finishReason = "length"
			case "SAFETY":
				finishReason = "content_filter"
			default:
				finishReason = "stop"
			}
		}

		content, ok := firstCandidate["content"].(map[string]any)
		if !ok {
			continue
		}
		parts, _ := content["parts"].([]any)

		var deltaContent string
		for _, part := range parts {
			partMap, ok := part.(map[string]any)
			if !ok {
				continue
			}
			// thought 文本是模型内部思考，不下发给客户端。
			if thought, _ := partMap["thought"].(bool); thought {
				continue
			}
			if text, ok := partMap["text"].(string); ok {
				deltaContent += text
			}
		}
		if deltaContent == "" {
			if usageMetadata, ok := chunk["usageMetadata"].(map[string]any); ok {
				applyGeminiUsage(&usage, usageMetadata)
			}
			continue
		}

		delta := map[string]any{"content": deltaContent}
		if !sentRole {
			delta["role"] = "assistant"
			sentRole = true
		}
		openAIChunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   routeName,
			"choices": []map[string]any{
				{"index": 0, "delta": delta, "finish_reason": nil},
			},
		}
		chunkBytes, err := json.Marshal(openAIChunk)
		if err != nil {
			return usage, fmt.Errorf("marshal openai stream chunk: %w", err)
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", chunkBytes); err != nil {
			return usage, err
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		if usageMetadata, ok := chunk["usageMetadata"].(map[string]any); ok {
			applyGeminiUsage(&usage, usageMetadata)
		}
	}
	if err := scanner.Err(); err != nil {
		return usage, fmt.Errorf("read gemini stream: %w", err)
	}

	// OpenAI 约定最后一个 chunk 带 finish_reason；上游没给就按 stop 收尾。
	if finishReason == "" {
		finishReason = "stop"
	}
	final := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   routeName,
		"choices": []map[string]any{
			{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason},
		},
	}
	finalBytes, err := json.Marshal(final)
	if err != nil {
		return usage, fmt.Errorf("marshal openai finish chunk: %w", err)
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", finalBytes); err != nil {
		return usage, err
	}
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return usage, err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	return usage, nil
}

// applyGeminiUsage 从 usageMetadata 累计/覆盖 usage（收尾帧的值最完整，直接覆盖）。
func applyGeminiUsage(usage *Usage, usageMetadata map[string]any) {
	if pt, ok := usageMetadata["promptTokenCount"].(float64); ok {
		usage.Prompt = int64(pt)
	}
	if ct, ok := usageMetadata["candidatesTokenCount"].(float64); ok {
		usage.Completion = int64(ct)
	}
	if tt, ok := usageMetadata["totalTokenCount"].(float64); ok {
		usage.Total = int64(tt)
	}
}
