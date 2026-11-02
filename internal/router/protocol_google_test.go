package router

import (
	"encoding/json"
	"testing"
)

func TestConvertOpenAIToGemini(t *testing.T) {
	// 准备 OpenAI 格式的请求
	openaiReq := map[string]any{
		"model": "gemini-2.0-flash-exp",
		"messages": []any{
			map[string]any{
				"role":    "system",
				"content": "你是一个有帮助的助手",
			},
			map[string]any{
				"role":    "user",
				"content": "你好",
			},
			map[string]any{
				"role":    "assistant",
				"content": "你好！有什么可以帮助你的吗？",
			},
			map[string]any{
				"role":    "user",
				"content": "今天天气怎么样？",
			},
		},
		"max_tokens":  float64(1000),
		"temperature": float64(0.7),
		"top_p":       float64(0.9),
	}

	// 转换为 Gemini 格式
	geminiReq, err := convertOpenAIToGemini(openaiReq)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}

	// 验证 system_instruction
	if sysInst, ok := geminiReq["system_instruction"].(map[string]any); !ok {
		t.Error("缺少 system_instruction")
	} else {
		parts, ok := sysInst["parts"].([]map[string]any)
		if !ok || len(parts) == 0 {
			t.Error("system_instruction.parts 格式错误")
		} else if parts[0]["text"] != "你是一个有帮助的助手" {
			t.Errorf("system_instruction 内容错误: %v", parts[0]["text"])
		}
	}

	// 验证 contents
	contents, ok := geminiReq["contents"].([]map[string]any)
	if !ok {
		t.Fatal("缺少 contents")
	}

	// 应该有 3 条消息（2 个 user + 1 个 model）
	if len(contents) != 3 {
		t.Errorf("期望 3 条消息，实际 %d 条", len(contents))
	}

	// 验证第一条消息（user）
	if contents[0]["role"] != "user" {
		t.Errorf("第一条消息角色错误: %v", contents[0]["role"])
	}
	parts0, ok := contents[0]["parts"].([]map[string]any)
	if !ok || len(parts0) == 0 || parts0[0]["text"] != "你好" {
		t.Error("第一条消息内容错误")
	}

	// 验证第二条消息（model）
	if contents[1]["role"] != "model" {
		t.Errorf("第二条消息角色错误: %v", contents[1]["role"])
	}

	// 验证 generationConfig
	genConfig, ok := geminiReq["generationConfig"].(map[string]any)
	if !ok {
		t.Fatal("缺少 generationConfig")
	}
	if genConfig["maxOutputTokens"] != 1000 {
		t.Errorf("maxOutputTokens 错误: %v", genConfig["maxOutputTokens"])
	}
	if genConfig["temperature"] != 0.7 {
		t.Errorf("temperature 错误: %v", genConfig["temperature"])
	}
	if genConfig["topP"] != 0.9 {
		t.Errorf("topP 错误: %v", genConfig["topP"])
	}
}

func TestConvertGeminiToOpenAI(t *testing.T) {
	// 准备 Gemini 格式的响应
	geminiResp := map[string]any{
		"candidates": []map[string]any{
			{
				"content": map[string]any{
					"parts": []map[string]any{
						{
							"text": "今天天气晴朗，气温 25 度。",
						},
					},
					"role": "model",
				},
				"finishReason": "STOP",
			},
		},
		"usageMetadata": map[string]any{
			"promptTokenCount":     10,
			"candidatesTokenCount": 15,
			"totalTokenCount":      25,
		},
	}

	geminiJSON, err := json.Marshal(geminiResp)
	if err != nil {
		t.Fatalf("序列化 Gemini 响应失败: %v", err)
	}

	// 转换为 OpenAI 格式
	openaiResp, err := convertGeminiToOpenAI(geminiJSON, "gemini-2.0-flash-exp")
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}

	// 解析 OpenAI 响应
	var result map[string]any
	if err := json.Unmarshal(openaiResp, &result); err != nil {
		t.Fatalf("解析 OpenAI 响应失败: %v", err)
	}

	// 验证基本字段
	if result["object"] != "chat.completion" {
		t.Errorf("object 错误: %v", result["object"])
	}
	if result["model"] != "gemini-2.0-flash-exp" {
		t.Errorf("model 错误: %v", result["model"])
	}

	// 验证 choices
	choicesRaw, ok := result["choices"].([]any)
	if !ok || len(choicesRaw) == 0 {
		t.Fatal("缺少 choices")
	}
	choice, ok := choicesRaw[0].(map[string]any)
	if !ok {
		t.Fatal("choice 格式错误")
	}

	message, ok := choice["message"].(map[string]any)
	if !ok {
		t.Fatal("message 格式错误")
	}

	if message["role"] != "assistant" {
		t.Errorf("role 错误: %v", message["role"])
	}
	if message["content"] != "今天天气晴朗，气温 25 度。" {
		t.Errorf("content 错误: %v", message["content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason 错误: %v", choice["finish_reason"])
	}

	// 验证 usage
	usage, ok := result["usage"].(map[string]any)
	if !ok {
		t.Fatal("缺少 usage")
	}
	if usage["prompt_tokens"] != float64(10) {
		t.Errorf("prompt_tokens 错误: %v", usage["prompt_tokens"])
	}
	if usage["completion_tokens"] != float64(15) {
		t.Errorf("completion_tokens 错误: %v", usage["completion_tokens"])
	}
	if usage["total_tokens"] != float64(25) {
		t.Errorf("total_tokens 错误: %v", usage["total_tokens"])
	}
}

func TestConvertGeminiToOpenAI_EmptyResponse(t *testing.T) {
	// 测试空响应
	geminiResp := map[string]any{
		"candidates": []map[string]any{},
	}

	geminiJSON, _ := json.Marshal(geminiResp)
	_, err := convertGeminiToOpenAI(geminiJSON, "gemini-2.0-flash-exp")
	if err == nil {
		t.Error("空响应应该返回错误")
	}
}

func TestConvertGeminiToOpenAI_LengthLimit(t *testing.T) {
	// 测试 MAX_TOKENS 结束原因
	geminiResp := map[string]any{
		"candidates": []map[string]any{
			{
				"content": map[string]any{
					"parts": []map[string]any{
						{"text": "这是一个被截断的响应"},
					},
					"role": "model",
				},
				"finishReason": "MAX_TOKENS",
			},
		},
	}

	geminiJSON, _ := json.Marshal(geminiResp)
	openaiResp, err := convertGeminiToOpenAI(geminiJSON, "gemini-2.0-flash-exp")
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}

	var result map[string]any
	json.Unmarshal(openaiResp, &result)
	choicesRaw, ok := result["choices"].([]any)
	if !ok || len(choicesRaw) == 0 {
		t.Fatal("缺少 choices")
	}
	choice, ok := choicesRaw[0].(map[string]any)
	if !ok {
		t.Fatal("choice 格式错误")
	}
	if choice["finish_reason"] != "length" {
		t.Errorf("MAX_TOKENS 应该转换为 length，实际: %v", choice["finish_reason"])
	}
}
