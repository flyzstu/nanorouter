package router

import (
	json "github.com/goccy/go-json"
)

func usageFromJSON(b []byte) Usage {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return Usage{Source: "unavailable"}
	}
	if u, ok := findUsage(v); ok {
		return u
	}
	return Usage{Source: "unavailable"}
}

func findUsage(v any) (Usage, bool) {
	switch x := v.(type) {
	case map[string]any:
		if raw, ok := x["usage"].(map[string]any); ok {
			return usageFromRaw(raw), true
		}
		for _, child := range x {
			if u, ok := findUsage(child); ok {
				return u, true
			}
		}
	case []any:
		for _, child := range x {
			if u, ok := findUsage(child); ok {
				return u, true
			}
		}
	}
	return Usage{}, false
}

// usageFromRaw 解析 usage 对象，兼容 OpenAI 与 Anthropic 两套字段名。
func usageFromRaw(raw map[string]any) Usage {
	u := Usage{Prompt: number(raw["prompt_tokens"]), Completion: number(raw["completion_tokens"]), Total: number(raw["total_tokens"]), Source: "upstream"}
	if u.Prompt == 0 {
		// Anthropic 形状：input_tokens 不含缓存命中/写入，与
		// 翻译路径保持同一记账口径，一并计入 prompt 侧。
		u.Prompt = number(raw["input_tokens"]) + number(raw["cache_read_input_tokens"]) + number(raw["cache_creation_input_tokens"])
	}
	if u.Completion == 0 {
		u.Completion = number(raw["output_tokens"])
	}
	if u.Total == 0 {
		u.Total = u.Prompt + u.Completion
	}
	u.Reasoning = detailNumber(raw["completion_tokens_details"], "reasoning_tokens")
	u.Cached = detailNumber(raw["prompt_tokens_details"], "cached_tokens")
	return u
}

// finishReasonFromChunk 从 OpenAI 兼容流帧提取 choices[0].finish_reason；
// 空表示该帧不是收尾帧。调用方保留最后一次非空值。
func finishReasonFromChunk(chunk map[string]any) string {
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return ""
	}
	finish, _ := choice["finish_reason"].(string)
	return finish
}

// finishReasonFromPayload 是 finishReasonFromChunk 的字节入口；解析失败即非收尾帧。
func finishReasonFromPayload(payload []byte) string {
	var chunk map[string]any
	if json.Unmarshal(payload, &chunk) != nil {
		return ""
	}
	return finishReasonFromChunk(chunk)
}

func detailNumber(v any, key string) int64 {
	if details, ok := v.(map[string]any); ok {
		return number(details[key])
	}
	return 0
}

func number(v any) int64 {
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	return 0
}
