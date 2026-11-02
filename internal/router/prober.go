package router

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	json "github.com/goccy/go-json"
	"go.uber.org/zap"
)

// makeProbePayload 构造极简的 Ping-Pong 探测请求体。
// 限制 max_tokens: 5 并关闭思维链，同时应用候选的清洗规则（如自动剔除不支持的字段）。
func (r *Router) makeProbePayload(u Candidate) ([]byte, error) {
	if u.Anthropic {
		// Anthropic 候选的探测走 Messages 形状；不注入 thinking，让
		// 上游按默认行为回答，有效性由响应内容判定。
		p := map[string]any{
			"model": u.Model,
			"messages": []map[string]string{
				{"role": "user", "content": "ping"},
			},
			"max_tokens":  float64(5),
			"temperature": float64(0),
		}
		return json.Marshal(p)
	}
	p := map[string]any{
		"model": u.Model,
		"messages": []map[string]string{
			{"role": "user", "content": "ping"},
		},
		"max_tokens":       float64(5),
		"temperature":      float64(0),
		"reasoning_effort": "none",
	}
	sanitized, _ := sanitizeRequest(p, u)
	sanitized, _ = clampRequestMaxTokens(sanitized, u)
	sanitized["model"] = u.Model
	return json.Marshal(sanitized)
}

// isPingPongValid 校验上游响应是否为有效的 Ping-Pong 回复。
// 拦截伪 200（HTTP 200 但内嵌 error 或 choices 为空），并宽容支持思考模型（生成 reasoning_content 亦视作存活）。
func isPingPongValid(statusCode int, body []byte) (bool, string) {
	if statusCode != http.StatusOK {
		return false, fmt.Sprintf("http_%d", statusCode)
	}
	var resp struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return false, "invalid_json"
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return false, fmt.Sprintf("api_error: %s", resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return false, "empty_choices"
	}
	choice := resp.Choices[0]
	if strings.TrimSpace(choice.Message.Content) == "" && strings.TrimSpace(choice.Message.ReasoningContent) == "" {
		return false, "empty_content"
	}
	return true, ""
}

// isMessagesPingValid 校验 Anthropic Messages 形状的探测回复。与 OpenAI
// 侧同语义：拦截伪 200（内嵌 error、无内容块、零输出 token），思维链
// 上的内容（thinking 块）同样视作存活。
func isMessagesPingValid(statusCode int, body []byte) (bool, string) {
	if statusCode != http.StatusOK {
		return false, fmt.Sprintf("http_%d", statusCode)
	}
	var resp struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return false, "invalid_json"
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return false, fmt.Sprintf("api_error: %s", resp.Error.Message)
	}
	for _, block := range resp.Content {
		if strings.TrimSpace(block.Text) != "" || block.Type == "tool_use" || block.Type == "thinking" {
			return true, ""
		}
	}
	if resp.Usage.OutputTokens > 0 && resp.StopReason != "" {
		return true, ""
	}
	return false, "empty_content"
}

// isProbeEnabled 检查候选是否启用了熔断后后台探活。
func (r *Router) isProbeEnabled(u Candidate) bool {
	return u.IsProbeEnabled(r.cfg.HealthCheck)
}

// hasProbeCandidates 检查当前是否有任何候选启用了探活功能。
func (r *Router) hasProbeCandidates() bool {
	if r.cfg.HealthCheck.Enabled {
		return true
	}
	for _, c := range r.candidates {
		if c.Probe != nil && *c.Probe {
			return true
		}
	}
	return false
}

// calculateProbeBackoff 计算探测失败后的指数退避时间：30s * 2^(failures-1)，上限为 MaxBackoff。
func (r *Router) calculateProbeBackoff(failures int) time.Duration {
	base := 30 * time.Second
	if failures <= 1 {
		return base
	}
	shift := failures - 1
	if shift > 10 {
		shift = 10
	}
	delay := base * time.Duration(1<<shift)
	maxBackoff := r.cfg.HealthCheck.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 10 * time.Minute
	}
	if delay > maxBackoff {
		delay = maxBackoff
	}
	return delay
}

// runProber 是后台异步探活的工作主循环，按 interval 轮询需要探测的熔断候选。
func (r *Router) runProber(ctx context.Context) {
	defer close(r.proberDone)
	interval := r.cfg.HealthCheck.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.probeEligibleCandidates(ctx)
		}
	}
}

// candidatesToProbe 在持锁状态下选出当前已到期且无在途探针的熔断候选。
func (r *Router) candidatesToProbe(now time.Time) []Candidate {
	r.mu.Lock()
	defer r.mu.Unlock()
	var toProbe []Candidate
	for _, candidate := range r.allCandidates {
		if !candidate.IsEnabled() || !r.isProbeEnabled(candidate) {
			continue
		}
		st := r.states[candidate.ID]
		if st == nil || st.failures == 0 || st.probeInflight {
			continue
		}
		if now.Before(st.until) {
			continue
		}
		st.probeInflight = true
		toProbe = append(toProbe, candidate)
	}
	return toProbe
}

// probeEligibleCandidates 触发到期候选的并发探活。
func (r *Router) probeEligibleCandidates(ctx context.Context) {
	now := time.Now()
	toProbe := r.candidatesToProbe(now)
	for _, u := range toProbe {
		go r.executeProbe(ctx, u)
	}
}

// executeProbe 对单个候选执行微型 Ping-Pong 推理探活。
func (r *Router) executeProbe(parentCtx context.Context, u Candidate) {
	timeout := r.cfg.HealthCheck.ProbeTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if u.AttemptTimeout > 0 && u.AttemptTimeout < timeout {
		timeout = u.AttemptTimeout
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	body, err := r.makeProbePayload(u)
	if err != nil {
		r.onProbeFailure(u, fmt.Sprintf("payload_error: %v", err), 0)
		return
	}

	start := time.Now()
	// Anthropic 候选探测 Messages 路径；do() 按候选协议解析路径与凭据头。
	probeEndpoint := "chat_completions"
	if u.Anthropic {
		probeEndpoint = "messages"
	}
	resp, err := r.do(ctx, u, probeEndpoint, false, body, nil, nil, "")
	duration := time.Since(start)

	if err != nil {
		r.onProbeFailure(u, fmt.Sprintf("network_error: %v", err), duration)
		return
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorBody+4096))
	if readErr != nil {
		r.onProbeFailure(u, fmt.Sprintf("read_error: %v", readErr), duration)
		return
	}

	valid, reason := isPingPongValid(resp.StatusCode, respBody)
	if u.Anthropic {
		valid, reason = isMessagesPingValid(resp.StatusCode, respBody)
	}
	if !valid {
		r.onProbeFailure(u, reason, duration)
		return
	}

	r.onProbeSuccess(u, duration)
}

// onProbeSuccess 探活成功：重置熔断计数并清空冷却时间，恢复正常可用池。
func (r *Router) onProbeSuccess(u Candidate, duration time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.states[u.ID]
	if st != nil {
		st.failures = 0
		st.until = time.Time{}
		st.probeInflight = false
		st.lastProbeAt = time.Now()
		st.lastProbeErr = ""
	}
	if r.probes != nil {
		r.probes.WithLabelValues(u.ID, "success").Inc()
	}
	if r.logger.Core().Enabled(zap.InfoLevel) {
		r.logger.Info(r.logMessage("upstream_probe_recovered"),
			eventFields("upstream_probe_recovered",
				zap.String("upstream", u.ID),
				zap.String("model", u.Model),
				zap.Int("duration_ms", int(duration.Milliseconds())),
			)...)
	}
}

// onProbeFailure 探活失败：累加 failure 计数并计算指数退避延长冷却期。
func (r *Router) onProbeFailure(u Candidate, reason string, duration time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.states[u.ID]
	if st == nil {
		st = &state{}
		r.states[u.ID] = st
	}
	st.failures++
	st.probeInflight = false
	st.lastProbeAt = time.Now()
	st.lastProbeErr = reason

	delay := r.calculateProbeBackoff(st.failures)
	st.until = time.Now().Add(delay)

	if r.probes != nil {
		r.probes.WithLabelValues(u.ID, "failure").Inc()
	}
	if r.logger.Core().Enabled(zap.WarnLevel) {
		r.logger.Warn(r.logMessage("upstream_probe_failed"),
			eventFields("upstream_probe_failed",
				zap.String("upstream", u.ID),
				zap.String("model", u.Model),
				zap.String("reason", reason),
				zap.Int("failures", st.failures),
				zap.Duration("next_delay", delay),
				zap.Int("duration_ms", int(duration.Milliseconds())),
			)...)
	}
}
