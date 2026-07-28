package router

import (
	"reflect"
	"testing"
)

func TestClampRequestMaxTokens(t *testing.T) {
	input := map[string]any{
		"max_tokens":            float64(100000),
		"max_completion_tokens": float64(0),
		"max_output_tokens":     float64(42),
		"max_new_tokens":        float64(70000),
		"temperature":           float64(1),
	}
	candidate := Candidate{MaxTokensLimit: 65536, ChatMaxTokensField: "max_new_tokens"}

	got, fields := clampRequestMaxTokens(input, candidate)

	if got["max_tokens"] != 65536 || got["max_completion_tokens"] != 1 ||
		got["max_output_tokens"] != float64(42) || got["max_new_tokens"] != 65536 {
		t.Fatalf("clamped request=%v", got)
	}
	if want := []string{"max_tokens", "max_completion_tokens", "max_new_tokens"}; !reflect.DeepEqual(fields, want) {
		t.Fatalf("fields=%v want=%v", fields, want)
	}
	if input["max_tokens"] != float64(100000) || input["max_completion_tokens"] != float64(0) {
		t.Fatalf("input was mutated: %v", input)
	}
}

func TestClampRequestMaxTokensLeavesDisabledOrInvalidValuesAlone(t *testing.T) {
	input := map[string]any{"max_tokens": float64(100000), "max_output_tokens": float64(1.5)}
	if got, fields := clampRequestMaxTokens(input, Candidate{}); !reflect.DeepEqual(got, input) || len(fields) != 0 {
		t.Fatalf("disabled clamp changed request: got=%v fields=%v", got, fields)
	}
	got, fields := clampRequestMaxTokens(input, Candidate{MaxTokensLimit: 10})
	if got["max_output_tokens"] != float64(1.5) || len(fields) != 1 || fields[0] != "max_tokens" {
		t.Fatalf("invalid numeric value handling: got=%v fields=%v", got, fields)
	}
}
