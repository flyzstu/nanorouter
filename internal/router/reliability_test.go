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
)

func TestAdmissionControllerBoundsInflightAndQueue(t *testing.T) {
	controller := newAdmissionController(BackpressureConfig{MaxInflight: 1, MaxQueue: 1, QueueTimeout: time.Second})
	releaseFirst, outcome := controller.acquire(context.Background())
	if outcome != admissionAcquired {
		t.Fatalf("first outcome=%s", outcome)
	}

	second := make(chan struct {
		release func()
		outcome admissionOutcome
	}, 1)
	go func() {
		release, got := controller.acquire(context.Background())
		second <- struct {
			release func()
			outcome admissionOutcome
		}{release: release, outcome: got}
	}()
	deadline := time.Now().Add(time.Second)
	for controller.queued.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if controller.queued.Load() != 1 {
		t.Fatal("second request did not enter queue")
	}
	if _, got := controller.acquire(context.Background()); got != admissionFull {
		t.Fatalf("third outcome=%s, want queue_full", got)
	}

	releaseFirst()
	got := <-second
	if got.outcome != admissionAcquired {
		t.Fatalf("queued outcome=%s", got.outcome)
	}
	got.release()
	if controller.inflight.Load() != 0 || controller.queued.Load() != 0 {
		t.Fatalf("inflight=%d queued=%d", controller.inflight.Load(), controller.queued.Load())
	}
}

func TestReliabilityConfigRejectsInvalidSettings(t *testing.T) {
	backpressure := []BackpressureConfig{
		{MaxInflight: -1},
		{MaxQueue: 1},
		{MaxInflight: 1, QueueTimeout: time.Second},
	}
	for _, cfg := range backpressure {
		if err := cfg.normalize(); err == nil {
			t.Fatalf("backpressure accepted: %+v", cfg)
		}
	}
	budgets := []RetryBudgetConfig{
		{Ratio: 0.1},
		{Enabled: true, Ratio: 1.1, Burst: 1},
		{Enabled: true, Ratio: 0.1, Burst: -1},
	}
	for _, cfg := range budgets {
		if err := cfg.normalize("free"); err == nil {
			t.Fatalf("retry budget accepted: %+v", cfg)
		}
	}
	global := RetryConfig{}
	if err := global.normalize(); err != nil {
		t.Fatal(err)
	}
	negative := -1
	for _, cfg := range []CandidateRetryConfig{
		{MaxRetries: &negative},
		{BaseDelay: 2 * time.Second, MaxDelay: time.Second},
		{BaseDelay: -time.Second},
	} {
		if _, err := cfg.resolve(global); err == nil {
			t.Fatalf("candidate retry accepted: %+v", cfg)
		}
	}
}

func TestRetryBudgetBoundsAttemptAmplification(t *testing.T) {
	budget := newRetryBudget(RetryBudgetConfig{Enabled: true, Ratio: 0.25, Burst: 2})
	if ok, _ := budget.consume(); !ok {
		t.Fatal("first burst token unavailable")
	}
	if ok, _ := budget.consume(); !ok {
		t.Fatal("second burst token unavailable")
	}
	if ok, _ := budget.consume(); ok {
		t.Fatal("empty budget allowed retry")
	}
	for i := 0; i < 4; i++ {
		budget.credit()
	}
	if ok, tokens := budget.consume(); !ok || tokens != 0 {
		t.Fatalf("credited retry ok=%v tokens=%v", ok, tokens)
	}
}

func TestGlobalBackpressureRejectsBeforeAnotherUpstreamCall(t *testing.T) {
	entered := make(chan struct{}, 1)
	unblock := make(chan struct{})
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		entered <- struct{}{}
		<-unblock
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{
		Backpressure: BackpressureConfig{MaxInflight: 1},
		Routes:       map[string]Route{"free": {Candidates: []Candidate{{ID: "u", BaseURL: upstream.URL, Model: "up"}}}},
	}, db)
	if err != nil {
		t.Fatal(err)
	}
	handler := r.Handler()
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`)))
		firstDone <- rec
	}()
	<-entered

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`)))
	if second.Code != http.StatusTooManyRequests || !strings.Contains(second.Body.String(), "gateway_overloaded") {
		t.Fatalf("status=%d body=%s", second.Code, second.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d, want 1", calls.Load())
	}
	close(unblock)
	if rec := <-firstDone; rec.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", rec.Code, rec.Body.String())
	}
	metrics := httptest.NewRecorder()
	handler.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, metric := range []string{"nanorouter_backpressure_inflight 0", "nanorouter_backpressure_queued 0", `nanorouter_backpressure_rejected_total{reason="queue_full"} 1`} {
		if !strings.Contains(metrics.Body.String(), metric) {
			t.Fatalf("missing metric %q:\n%s", metric, metrics.Body.String())
		}
	}
}

func TestRouteRetryBudgetStopsExtraUpstreamAttempt(t *testing.T) {
	var calls atomic.Int64
	servers := make([]*httptest.Server, 0, 3)
	newUpstream := func(status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"temporary"}}`))
		}))
	}
	for i := 0; i < 3; i++ {
		server := newUpstream(http.StatusServiceUnavailable)
		servers = append(servers, server)
		defer server.Close()
	}
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"free": {
		RetryBudget: RetryBudgetConfig{Enabled: true, Ratio: 0.01, Burst: 1},
		Candidates: []Candidate{
			{ID: "one", BaseURL: servers[0].URL, Model: "one"},
			{ID: "two", BaseURL: servers[1].URL, Model: "two"},
			{ID: "three", BaseURL: servers[2].URL, Model: "three"},
		},
	}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`)))
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "retry_budget_exhausted") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d, want 2", calls.Load())
	}
	metrics := httptest.NewRecorder()
	r.Handler().ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metrics.Body.String(), `nanorouter_retry_budget_denied_total{route="free"} 1`) {
		t.Fatalf("retry budget metric missing:\n%s", metrics.Body.String())
	}
}

func TestExposedCandidateUsesIndependentRetryPolicy(t *testing.T) {
	var directCalls, otherCalls atomic.Int64
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := directCalls.Add(1)
		if call <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"temporary"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer direct.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherCalls.Add(1)
		_, _ = w.Write([]byte(`{"id":"other"}`))
	}))
	defer other.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	maxRetries := 2
	r, err := New(Config{Routes: map[string]Route{
		"pool": {Candidates: []Candidate{
			{ID: "direct", BaseURL: direct.URL, Model: "direct-up", ExposeAs: "direct-model", Retry: CandidateRetryConfig{MaxRetries: &maxRetries, BaseDelay: 5 * time.Second, MaxDelay: 5 * time.Second}},
			{ID: "other", BaseURL: other.URL, Model: "other-up"},
		}},
	}}, db)
	if err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	r.retryWait = func(_ context.Context, delay time.Duration) bool {
		waits = append(waits, delay)
		r.mu.Lock()
		r.states["direct"].until = time.Time{}
		r.mu.Unlock()
		return true
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"direct-model","messages":[]}`)))
	if rec.Code != http.StatusOK || directCalls.Load() != 3 || otherCalls.Load() != 0 {
		t.Fatalf("status=%d direct=%d other=%d body=%s", rec.Code, directCalls.Load(), otherCalls.Load(), rec.Body.String())
	}
	if len(waits) != 2 {
		t.Fatalf("waits=%v", waits)
	}
	for _, delay := range waits {
		if delay < 2400*time.Millisecond || delay > 5*time.Second {
			t.Fatalf("candidate delay=%s, want equal jitter in [2.5s, 5s]", delay)
		}
	}
}

func TestExposedCandidateRetryAfterOverridesCandidateDelay(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	maxRetries := 1
	r, err := New(Config{Routes: map[string]Route{"pool": {Candidates: []Candidate{{
		ID: "direct", BaseURL: upstream.URL, Model: "up", ExposeAs: "direct", Retry: CandidateRetryConfig{MaxRetries: &maxRetries, BaseDelay: time.Second, MaxDelay: time.Second},
	}}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	var waited time.Duration
	r.retryWait = func(_ context.Context, delay time.Duration) bool {
		waited = delay
		r.mu.Lock()
		r.states["direct"].until = time.Time{}
		r.mu.Unlock()
		return true
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"direct","messages":[]}`)))
	if rec.Code != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls.Load(), rec.Body.String())
	}
	if waited < 6900*time.Millisecond || waited > 7*time.Second {
		t.Fatalf("waited=%s, want Retry-After near 7s", waited)
	}
}

func TestExposedCandidateDoesNotRetryStatus500(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"failed"}}`))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"pool": {Candidates: []Candidate{{ID: "direct", BaseURL: upstream.URL, Model: "up", ExposeAs: "direct"}}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"direct","messages":[]}`)))
	if rec.Code != http.StatusInternalServerError || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls.Load(), rec.Body.String())
	}
}

func TestExposedCandidateInheritsGlobalRetry(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	maxRetries := 1
	r, err := New(Config{
		Retry:  RetryConfig{MaxRateLimitRetries: &maxRetries},
		Routes: map[string]Route{"pool": {Candidates: []Candidate{{ID: "direct", BaseURL: upstream.URL, Model: "up", ExposeAs: "direct"}}}},
	}, db)
	if err != nil {
		t.Fatal(err)
	}
	r.retryBackoff = func(int) time.Duration { return 0 }
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"direct","messages":[]}`)))
	if rec.Code != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls.Load(), rec.Body.String())
	}
}

func TestExposedCandidateRetryUsesParentBudget(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	maxRetries := 2
	r, err := New(Config{Routes: map[string]Route{"pool": {
		RetryBudget: RetryBudgetConfig{Enabled: true, Ratio: 0.01, Burst: 1},
		Candidates:  []Candidate{{ID: "direct", BaseURL: upstream.URL, Model: "up", ExposeAs: "direct", Retry: CandidateRetryConfig{MaxRetries: &maxRetries}}},
	}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	r.retryBackoff = func(int) time.Duration { return 0 }
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"direct","messages":[]}`)))
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "retry_budget_exhausted") || calls.Load() != 2 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls.Load(), rec.Body.String())
	}
}
