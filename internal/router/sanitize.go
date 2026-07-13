package router

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
