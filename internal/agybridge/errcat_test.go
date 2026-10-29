package agybridge

import (
	"context"
	"errors"
	"testing"
)

func TestClassifyErrorKinds(t *testing.T) {
	cases := []struct {
		err    error
		kind   string
		status int
	}{
		{&AuthError{Message: "not logged in"}, "auth", 502},
		{errors.New("net/http: TLS handshake timeout"), "network", 502},
		{errors.New("Eligibility check failed: not available in your location"), "eligibility", 403},
		{errors.New("429 too many requests"), "rate_limit", 429},
		{errors.New("prompt is too long: maximum context"), "context_window", 400},
		{context.DeadlineExceeded, "timeout", 504},
		{ErrNoResult, "no_result", 502},
	}
	for _, tc := range cases {
		info := classifyError(tc.err)
		if info.Kind != tc.kind || info.Status != tc.status {
			t.Fatalf("%v => %+v, want kind=%s status=%d", tc.err, info, tc.kind, tc.status)
		}
	}
}

func TestStaleConversation(t *testing.T) {
	for _, msg := range []string{
		"conversation not found",
		"invalid conversation id",
		"conversation expired",
	} {
		if !isStaleConversation(errors.New(msg)) {
			t.Fatalf("expected stale: %q", msg)
		}
	}
	if isStaleConversation(errors.New("429 rate limit")) {
		t.Fatal("rate limit must not be treated as stale conversation")
	}
}
