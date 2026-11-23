package router

import "testing"

func TestUsageFromRawParsesTokenDetails(t *testing.T) {
	raw := map[string]any{
		"prompt_tokens":     float64(10),
		"completion_tokens": float64(20),
		"total_tokens":      float64(30),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": float64(4),
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": float64(7),
		},
	}
	usage := usageFromRaw(raw)
	if usage.Prompt != 10 || usage.Completion != 20 || usage.Total != 30 {
		t.Fatalf("unexpected totals: %+v", usage)
	}
	if usage.Reasoning != 7 || usage.Cached != 4 {
		t.Fatalf("token details lost: reasoning=%d cached=%d", usage.Reasoning, usage.Cached)
	}
}

func TestResponseBaseReportsUsageDetails(t *testing.T) {
	response := responseBase("resp_1", "m", 1, "completed", nil, []any{},
		Usage{Prompt: 2, Completion: 5, Total: 7, Reasoning: 3, Cached: 1, Source: "upstream"})
	usage, _ := response["usage"].(map[string]any)
	output, _ := usage["output_tokens_details"].(map[string]any)
	input, _ := usage["input_tokens_details"].(map[string]any)
	if got, ok := output["reasoning_tokens"].(int64); !ok || got != 3 {
		t.Fatalf("reasoning_tokens = %v, want 3", output["reasoning_tokens"])
	}
	if got, ok := input["cached_tokens"].(int64); !ok || got != 1 {
		t.Fatalf("cached_tokens = %v, want 1", input["cached_tokens"])
	}
}

func TestFinishReasonFromPayload(t *testing.T) {
	if got := finishReasonFromPayload([]byte(`{"choices":[{"finish_reason":"length"}]}`)); got != "length" {
		t.Fatalf("finish = %q, want length", got)
	}
	if got := finishReasonFromPayload([]byte(`{"choices":[{"delta":{},"finish_reason":null}]}`)); got != "" {
		t.Fatalf("finish = %q, want empty", got)
	}
	if got := finishReasonFromPayload([]byte(`{"usage":{"total_tokens":3}}`)); got != "" {
		t.Fatalf("finish = %q, want empty", got)
	}
}

func TestMergeUsageKeepsFinishReason(t *testing.T) {
	merged := mergeUsage(Usage{Source: "unavailable"}, Usage{Source: "upstream", Total: 3, FinishReason: "stop"})
	if merged.FinishReason != "stop" {
		t.Fatalf("finish = %q, want stop", merged.FinishReason)
	}
}
