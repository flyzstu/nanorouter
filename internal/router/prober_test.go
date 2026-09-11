package router

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	json "github.com/goccy/go-json"
)

func TestIsPingPongValid(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       string
		wantValid  bool
		wantReason string
	}{
		{
			name:       "valid standard pong",
			statusCode: http.StatusOK,
			body:       `{"choices":[{"message":{"content":"pong","role":"assistant"}}]}`,
			wantValid:  true,
		},
		{
			name:       "valid thinking model reasoning only",
			statusCode: http.StatusOK,
			body:       `{"choices":[{"message":{"content":"","reasoning_content":"User said ping, let me reply.","role":"assistant"}}]}`,
			wantValid:  true,
		},
		{
			name:       "status 503",
			statusCode: http.StatusServiceUnavailable,
			body:       `{"error":{"message":"service unavailable"}}`,
			wantValid:  false,
			wantReason: "http_503",
		},
		{
			name:       "status 429",
			statusCode: http.StatusTooManyRequests,
			body:       `{"error":{"message":"rate limit exceeded"}}`,
			wantValid:  false,
			wantReason: "http_429",
		},
		{
			name:       "wrapped api error inside 200",
			statusCode: http.StatusOK,
			body:       `{"error":{"message":"upstream provider error","code":503}}`,
			wantValid:  false,
			wantReason: "api_error: upstream provider error",
		},
		{
			name:       "empty choices array",
			statusCode: http.StatusOK,
			body:       `{"choices":[]}`,
			wantValid:  false,
			wantReason: "empty_choices",
		},
		{
			name:       "empty content and reasoning",
			statusCode: http.StatusOK,
			body:       `{"choices":[{"message":{"content":"   ","reasoning_content":""}}]}`,
			wantValid:  false,
			wantReason: "empty_content",
		},
		{
			name:       "malformed json",
			statusCode: http.StatusOK,
			body:       `not a json`,
			wantValid:  false,
			wantReason: "invalid_json",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			valid, reason := isPingPongValid(tc.statusCode, []byte(tc.body))
			if valid != tc.wantValid {
				t.Fatalf("isPingPongValid() got valid=%v, want %v (reason=%q)", valid, tc.wantValid, reason)
			}
			if !tc.wantValid && !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("isPingPongValid() got reason=%q, want to contain %q", reason, tc.wantReason)
			}
		})
	}
}

func TestCalculateProbeBackoff(t *testing.T) {
	r := &Router{
		cfg: Config{
			HealthCheck: HealthCheckConfig{
				MaxBackoff: 5 * time.Minute,
			},
		},
	}

	if d := r.calculateProbeBackoff(0); d != 30*time.Second {
		t.Fatalf("expected 30s for 0 failures, got %v", d)
	}
	if d := r.calculateProbeBackoff(1); d != 30*time.Second {
		t.Fatalf("expected 30s for 1 failure, got %v", d)
	}
	if d := r.calculateProbeBackoff(2); d != 60*time.Second {
		t.Fatalf("expected 60s for 2 failures, got %v", d)
	}
	if d := r.calculateProbeBackoff(3); d != 120*time.Second {
		t.Fatalf("expected 120s for 3 failures, got %v", d)
	}
	if d := r.calculateProbeBackoff(4); d != 240*time.Second {
		t.Fatalf("expected 240s for 4 failures, got %v", d)
	}
	// 5 failures: 30 * 2^4 = 480s (8m) -> capped at MaxBackoff (5m)
	if d := r.calculateProbeBackoff(5); d != 5*time.Minute {
		t.Fatalf("expected 5m cap for 5 failures, got %v", d)
	}
}

func TestMakeProbePayloadSanitizesCandidate(t *testing.T) {
	r := &Router{}
	c := Candidate{
		Model:          "gpt-custom",
		MaxTokensLimit: 3,
		DropRequestFields: []string{
			"reasoning_effort",
		},
	}

	b, err := r.makeProbePayload(c)
	if err != nil {
		t.Fatalf("makeProbePayload failed: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("failed to unmarshal payload: %v", err)
	}

	if m["model"] != "gpt-custom" {
		t.Fatalf("expected model gpt-custom, got %v", m["model"])
	}
	if _, ok := m["reasoning_effort"]; ok {
		t.Fatalf("expected reasoning_effort to be dropped")
	}
	if maxTokens, ok := m["max_tokens"].(float64); !ok || maxTokens != 3 {
		t.Fatalf("expected max_tokens clamped to 3, got %v", m["max_tokens"])
	}
}

func TestProberRecoversCandidateAfterFailure(t *testing.T) {
	var callCount atomic.Int32
	var mode atomic.Int32 // 0: 503, 1: 200

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		if mode.Load() == 0 {
			http.Error(w, `{"error":{"message":"overloaded"}}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong","role":"assistant"}}]}`))
	}))
	defer ts.Close()

	probeTrue := true
	cfg := Config{
		HealthCheck: HealthCheckConfig{
			Enabled:      true,
			ProbeTimeout: 1 * time.Second,
			Interval:     20 * time.Millisecond,
			MaxBackoff:   1 * time.Minute,
		},
		Routes: map[string]Route{
			"auto": {
				Candidates: []Candidate{
					{
						ID:      "free-model",
						BaseURL: ts.URL,
						Model:   "free-model",
						Weight:  1,
						Probe:   &probeTrue,
					},
				},
			},
		},
	}

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	r, err := New(cfg, db)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	defer r.Close(context.Background())

	// 1. 发起第一次请求，上游返回 503，导致候选进入熔断
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	r.Handler().ServeHTTP(rec, req)

	// 候选进入冷却，由于 auto 只有这一个候选，整体返回 503
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503, got %d", rec.Code)
	}

	// 检查状态：当前 failures 为 1
	r.mu.Lock()
	st := r.states["free-model"]
	if st == nil || st.failures != 1 {
		r.mu.Unlock()
		t.Fatalf("expected state failures=1, got %+v", st)
	}
	// 将 until 调整到当前时间之前，以便探活循环能够立即执行探测
	st.until = time.Now().Add(-1 * time.Millisecond)
	r.mu.Unlock()

	// 2. 将模拟上游切换为健康状态（返回 200 pong）
	mode.Store(1)

	// 3. 等待后台 Prober 探活成功并恢复候选
	recovered := false
	for i := 0; i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		r.mu.Lock()
		st = r.states["free-model"]
		if st != nil && st.failures == 0 {
			recovered = true
			r.mu.Unlock()
			break
		}
		r.mu.Unlock()
	}

	if !recovered {
		t.Fatalf("prober failed to recover candidate within timeout")
	}

	// 4. 再次发起用户请求，应当成功返回 200
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	r.Handler().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected status 200 after recovery, got %d body=%s", rec2.Code, rec2.Body.String())
	}
}

func TestProberBackoffOnContinuedFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"overloaded"}}`, http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	probeTrue := true
	cfg := Config{
		HealthCheck: HealthCheckConfig{
			Enabled:      true,
			ProbeTimeout: 1 * time.Second,
			Interval:     20 * time.Millisecond,
			MaxBackoff:   10 * time.Minute,
		},
		Routes: map[string]Route{
			"auto": {
				Candidates: []Candidate{
					{
						ID:      "broken-model",
						BaseURL: ts.URL,
						Model:   "broken-model",
						Weight:  1,
						Probe:   &probeTrue,
					},
				},
			},
		},
	}

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	r, err := New(cfg, db)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	defer r.Close(context.Background())

	// 人工设置一次失败，并让 until 过期触发 probe
	r.failUntil(Candidate{ID: "broken-model", Model: "broken-model"}, time.Now().Add(-1*time.Millisecond))

	// 等待 Prober 执行并再次失败
	increased := false
	for i := 0; i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		r.mu.Lock()
		st := r.states["broken-model"]
		if st != nil && st.failures >= 2 {
			increased = true
			r.mu.Unlock()
			break
		}
		r.mu.Unlock()
	}

	if !increased {
		t.Fatalf("expected failures to increase to >= 2 after failed probe")
	}

	r.mu.Lock()
	st := r.states["broken-model"]
	until := st.until
	r.mu.Unlock()

	// 2 次失败后退避时间应为 60s（允许略小容差）
	if time.Until(until) < 50*time.Second {
		t.Fatalf("expected backoff delay around 60s, got remaining %v", time.Until(until))
	}
}

func TestProberGracefulShutdown(t *testing.T) {
	cfg := Config{
		HealthCheck: HealthCheckConfig{
			Enabled:  true,
			Interval: 10 * time.Millisecond,
		},
		Routes: map[string]Route{
			"auto": {
				Candidates: []Candidate{
					{
						ID:      "m1",
						BaseURL: "http://127.0.0.1:1",
						Model:   "m1",
					},
				},
			},
		},
	}

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	r, err := New(cfg, db)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := r.Close(ctx); err != nil {
		t.Fatalf("router.Close failed: %v", err)
	}
}

// TestProberExposeAsFastFailsWhileCoolingAndRecovers 测试单独对外暴露的模型：
// 1. 遭遇 503 后进入冷却；
// 2. 冷却期间后续请求秒级快速失败（本地返回 503，不再向上游发请求）；
// 3. 后台探活成功后，再次请求恢复正常 200。
func TestProberExposeAsFastFailsWhileCoolingAndRecovers(t *testing.T) {
	var upstreamCalls atomic.Int32
	var mode atomic.Int32 // 0: 503, 1: 200

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if mode.Load() == 0 {
			http.Error(w, `{"error":{"message":"overloaded"}}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong","role":"assistant"}}]}`))
	}))
	defer ts.Close()

	probeTrue := true
	cfg := Config{
		HealthCheck: HealthCheckConfig{
			Enabled:      true,
			ProbeTimeout: 1 * time.Second,
			Interval:     20 * time.Millisecond,
		},
		Routes: map[string]Route{
			"auto": {
				Candidates: []Candidate{
					{
						ID:       "solo-free",
						BaseURL:  ts.URL,
						Model:    "solo-free",
						ExposeAs: "my-free-model",
						Weight:   1,
						Probe:    &probeTrue,
					},
				},
			},
		},
	}

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	r, err := New(cfg, db)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	defer r.Close(context.Background())

	// 1. 首次直连访问，上游返回 503
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"my-free-model","messages":[{"role":"user","content":"hi"}]}`))
	r.Handler().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503, got %d", rec1.Code)
	}

	callsAfterFirst := upstreamCalls.Load()

	// 2. 在冷却期内，再次直连访问：应当在本地秒回 503，上游调用数不应增加
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"my-free-model","messages":[{"role":"user","content":"hi"}]}`))
	r.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected fast-fail status 503 while cooling, got %d", rec2.Code)
	}
	if upstreamCalls.Load() != callsAfterFirst {
		t.Fatalf("expected upstream not to be called during cooling fast-fail, got calls %d -> %d", callsAfterFirst, upstreamCalls.Load())
	}

	// 3. 上游恢复正常，将 until 调前触发后台探活
	mode.Store(1)
	r.mu.Lock()
	r.states["solo-free"].until = time.Now().Add(-1 * time.Millisecond)
	r.mu.Unlock()

	// 4. 等待后台 Prober 探活成功
	recovered := false
	for i := 0; i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		r.mu.Lock()
		st := r.states["solo-free"]
		if st != nil && st.failures == 0 {
			recovered = true
			r.mu.Unlock()
			break
		}
		r.mu.Unlock()
	}
	if !recovered {
		t.Fatalf("prober did not recover solo-free candidate")
	}

	// 5. 探活成功后再次直连请求，返回 200 OK
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"my-free-model","messages":[{"role":"user","content":"hi"}]}`))
	r.Handler().ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("expected status 200 after recovery, got %d body=%s", rec3.Code, rec3.Body.String())
	}
}

// TestProberAggregatedFailoverSkipsCoolingCandidate 测试在聚合路由中，
// 处于探活冷却期的模型会被 100% 旁路绕开，流量全部导向健康的候选。
func TestProberAggregatedFailoverSkipsCoolingCandidate(t *testing.T) {
	var brokenCalls atomic.Int32
	var healthyCalls atomic.Int32

	tsBroken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		brokenCalls.Add(1)
		http.Error(w, `{"error":{"message":"overloaded"}}`, http.StatusServiceUnavailable)
	}))
	defer tsBroken.Close()

	tsHealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthyCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"healthy","role":"assistant"}}]}`))
	}))
	defer tsHealthy.Close()

	probeTrue := true
	cfg := Config{
		HealthCheck: HealthCheckConfig{
			Enabled:  true,
			Interval: 1 * time.Hour, // 测试期间不让它自动恢复
		},
		Routes: map[string]Route{
			"auto": {
				Candidates: []Candidate{
					{
						ID:      "broken-candidate",
						BaseURL: tsBroken.URL,
						Model:   "broken-candidate",
						Weight:  100, // 权重极高，正常情况下会被优先选中
						Probe:   &probeTrue,
					},
					{
						ID:      "healthy-candidate",
						BaseURL: tsHealthy.URL,
						Model:   "healthy-candidate",
						Weight:  1,
						Probe:   &probeTrue,
					},
				},
			},
		},
	}

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	r, err := New(cfg, db)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	defer r.Close(context.Background())

	// 人工将 broken 候选设为冷却（failures=1）
	r.failUntil(Candidate{ID: "broken-candidate", Model: "broken-candidate"}, time.Now().Add(10*time.Minute))

	// 发起 5 次请求，因为 broken 处于探活隔离期，所有请求都必须落到 healthy
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
		r.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("request %d failed: %d", i, rec.Code)
		}
	}

	if brokenCalls.Load() != 0 {
		t.Fatalf("expected 0 calls to broken candidate, got %d", brokenCalls.Load())
	}
	if healthyCalls.Load() != 5 {
		t.Fatalf("expected 5 calls to healthy candidate, got %d", healthyCalls.Load())
	}
}

// TestProberSendsCandidateHeadersAndAuth 测试探针执行时能正确携带候选自定义 Header、User-Agent 以及 API Key
func TestProberSendsCandidateHeadersAndAuth(t *testing.T) {
	var receivedAuth string
	var receivedCustomHeader string
	var receivedUA string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedCustomHeader = r.Header.Get("X-Probe-Test")
		receivedUA = r.Header.Get("User-Agent")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong","role":"assistant"}}]}`))
	}))
	defer ts.Close()

	t.Setenv("PROBE_TEST_KEY", "secret-token-12345")

	probeTrue := true
	cfg := Config{
		HealthCheck: HealthCheckConfig{
			Enabled:      true,
			ProbeTimeout: 1 * time.Second,
			Interval:     20 * time.Millisecond,
		},
		Routes: map[string]Route{
			"auto": {
				Candidates: []Candidate{
					{
						ID:        "test-candidate",
						BaseURL:   ts.URL,
						Model:     "test-candidate",
						APIKeyEnv: "PROBE_TEST_KEY",
						UserAgent: "custom-prober-ua/1.0",
						Headers:   map[string]string{"X-Probe-Test": "prober-value"},
						Probe:     &probeTrue,
					},
				},
			},
		},
	}

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	r, err := New(cfg, db)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	defer r.Close(context.Background())

	// 标记失败并触发探活
	r.failUntil(Candidate{ID: "test-candidate", Model: "test-candidate"}, time.Now().Add(-1*time.Millisecond))

	// 等待探测执行并成功
	for i := 0; i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		r.mu.Lock()
		st := r.states["test-candidate"]
		if st != nil && st.failures == 0 {
			r.mu.Unlock()
			break
		}
		r.mu.Unlock()
	}

	if receivedAuth != "Bearer secret-token-12345" {
		t.Fatalf("expected auth 'Bearer secret-token-12345', got %q", receivedAuth)
	}
	if receivedCustomHeader != "prober-value" {
		t.Fatalf("expected X-Probe-Test 'prober-value', got %q", receivedCustomHeader)
	}
	if receivedUA != "custom-prober-ua/1.0" {
		t.Fatalf("expected User-Agent 'custom-prober-ua/1.0', got %q", receivedUA)
	}
}
