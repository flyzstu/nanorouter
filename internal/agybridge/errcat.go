package agybridge

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ResultError 是 agy result 信封里的业务错误。
type ResultError struct {
	Message string
}

func (e *ResultError) Error() string { return e.Message }

// ErrorInfo 是面向 HTTP/SSE 的归一化错误。
type ErrorInfo struct {
	Kind       string
	Status     int
	Code       string
	Message    string
	Retryable  bool
	RetryAfter time.Duration
}

func classifyError(err error) ErrorInfo {
	if err == nil {
		return ErrorInfo{Kind: "none", Status: 200}
	}
	var auth *AuthError
	if errors.As(err, &auth) {
		return ErrorInfo{Kind: "auth", Status: 502, Code: "agy_auth_required", Message: err.Error()}
	}
	if errors.Is(err, ErrNoResult) {
		return ErrorInfo{Kind: "no_result", Status: 502, Code: "agy_no_result", Message: err.Error()}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorInfo{Kind: "timeout", Status: 504, Code: "agy_timeout", Message: err.Error(), Retryable: true}
	}
	if errors.Is(err, context.Canceled) {
		return ErrorInfo{Kind: "client_canceled", Status: 499, Code: "client_canceled", Message: err.Error()}
	}

	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case containsAny(lower, "429", "rate limit", "too many requests", "quota", "resource_exhausted"):
		return ErrorInfo{Kind: "rate_limit", Status: 429, Code: "agy_rate_limited", Message: msg, Retryable: true}
	case containsAny(lower, "context window", "context_length", "prompt is too long", "maximum context", "input too long"):
		return ErrorInfo{Kind: "context_window", Status: 400, Code: "context_length_exceeded", Message: msg}
	case containsAny(lower, "eligibility", "not available in your location", "location is not supported", "failed_precondition"):
		return ErrorInfo{Kind: "eligibility", Status: 403, Code: "agy_ineligible", Message: msg}
	case containsAny(lower, "sign in", "authentication", "not logged in", "not logged into", "401", "403"):
		return ErrorInfo{Kind: "auth", Status: 502, Code: "agy_auth_required", Message: msg}
	case containsAny(lower, "net/http", "timeout", "connection", "eof", "tls", "proxy", "dial", "temporary failure"):
		return ErrorInfo{Kind: "network", Status: 502, Code: "agy_network_error", Message: msg, Retryable: true}
	default:
		return ErrorInfo{Kind: "process", Status: 502, Code: "agy_error", Message: msg}
	}
}

func containsAny(s string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func isStaleConversation(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "conversation") &&
		containsAny(lower, "not found", "not exist", "invalid", "unknown", "expired")
}
