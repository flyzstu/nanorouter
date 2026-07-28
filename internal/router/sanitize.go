package router

import "math"

// sanitizeRequest 在每次选择上游后执行，因此只影响该上游的副本，
// 不会污染随后用于故障切换的原始下游请求。
func sanitizeRequest(in map[string]any, u Candidate) (map[string]any, []string) {
	out := clone(in)
	dropped := make([]string, 0)
	for _, key := range u.DropRequestFields {
		// model 是路由正确性的核心字段，禁止配置将其删除。
		if key == "model" {
			continue
		}
		if _, ok := out[key]; ok {
			delete(out, key)
			dropped = append(dropped, key)
		}
	}
	if len(u.DropMessageFields) == 0 {
		return out, dropped
	}
	messages, ok := out["messages"].([]any)
	if !ok {
		return out, dropped
	}
	clean := make([]any, 0, len(messages))
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			clean = append(clean, raw)
			continue
		}
		copyMessage := clone(message)
		for _, key := range u.DropMessageFields {
			if _, ok := copyMessage[key]; ok {
				delete(copyMessage, key)
				dropped = append(dropped, "messages[]."+key)
			}
		}
		clean = append(clean, copyMessage)
	}
	out["messages"] = clean
	return out, dropped
}

// clampRequestMaxTokens 同时覆盖 Chat Completions、Responses 和自定义适配字段。
// 只处理 JSON 整数，类型错误或小数仍交给上游按协议拒绝。
func clampRequestMaxTokens(in map[string]any, u Candidate) (map[string]any, []string) {
	if u.MaxTokensLimit == 0 {
		return in, nil
	}
	out := clone(in)
	fields := []string{"max_tokens", "max_completion_tokens", "max_output_tokens"}
	if u.ChatMaxTokensField != "" {
		fields = append(fields, u.ChatMaxTokensField)
	}
	clamped := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		if seen[field] {
			continue
		}
		seen[field] = true
		value, ok := out[field].(float64)
		if !ok || value != math.Trunc(value) {
			continue
		}
		switch {
		case value < 1:
			out[field] = 1
			clamped = append(clamped, field)
		case value > float64(u.MaxTokensLimit):
			out[field] = u.MaxTokensLimit
			clamped = append(clamped, field)
		}
	}
	return out, clamped
}
