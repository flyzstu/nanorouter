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
			return u, true
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
func number(v any) int64 {
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	return 0
}
