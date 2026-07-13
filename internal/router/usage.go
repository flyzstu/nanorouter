package router

import (
	"bytes"
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

// Responses 流会把 usage 放入 response.completed；Chat Completions 通常放在最后一个 data 帧。
// 递归查找让供应商扩展字段也能被采集，同时不修改下游收到的原始 SSE 内容。
func usageFromSSE(b []byte) Usage {
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		if u := usageFromJSON(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))); u.Source == "upstream" {
			return u
		}
	}
	return Usage{Source: "unavailable"}
}

func findUsage(v any) (Usage, bool) {
	switch x := v.(type) {
	case map[string]any:
		if raw, ok := x["usage"].(map[string]any); ok {
			u := Usage{Prompt: number(raw["prompt_tokens"]), Completion: number(raw["completion_tokens"]), Total: number(raw["total_tokens"]), Source: "upstream"}
			if u.Prompt == 0 {
				u.Prompt = number(raw["input_tokens"])
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
