package router

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// signalWriter 在首次 Flush 时通知测试：网关已开始向下游转发 SSE。
type signalWriter struct {
	*httptest.ResponseRecorder
	once    sync.Once
	started chan struct{}
}

func (w *signalWriter) Flush() {
	w.once.Do(func() { close(w.started) })
}

// TestClientCanceledStreamDoesNotCooldownUpstream 验证：客户端在流式途中取消，
// 上游不应被冷却，账本仍应记录 client_canceled（独立 context 保证写入成功）。
func TestClientCanceledStreamDoesNotCooldownUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// 第一个请求持续流式输出，直到网关因下游取消而中断。
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			for i := 0; ; i++ {
				_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"chunk-%d\"}}]}\n\n", i)
				if flusher != nil {
					flusher.Flush()
				}
				select {
				case <-r.Context().Done():
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
		}
		// 后续请求立即完成，用于验证候选未被冷却。
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok-2","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{
		RequestTimeout: 10 * time.Second,
		Routes:         map[string]Route{"free": {Candidates: []Candidate{{ID: "u", BaseURL: upstream.URL, Model: "up"}}}},
	}, db)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[],"stream":true}`)).WithContext(ctx)
	rec := &signalWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		r.Handler().ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-rec.started:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after cancel")
	}

	// 取消是客户端行为，不是上游故障：账本记录 client_canceled 且写入成功。
	var kind string
	var status int
	if err := db.QueryRow(`SELECT error_kind, status_code FROM llm_requests`).Scan(&kind, &status); err != nil {
		t.Fatalf("ledger row missing: %v", err)
	}
	if kind != "client_canceled" || status != 499 {
		t.Fatalf("ledger kind=%q status=%d", kind, status)
	}

	// 候选未被冷却：紧随其后的请求应立即成功，而不是 503 upstreams_unavailable。
	rec2 := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request status=%d body=%s", rec2.Code, rec2.Body.String())
	}
}

// TestClientCanceledUpstreamAttemptDoesNotCooldown 验证：等待上游响应头期间客户端取消，
// 同样不冷却候选，账本记录 client_canceled。
func TestClientCanceledUpstreamAttemptDoesNotCooldown(t *testing.T) {
	begin := make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(begin)
			// 客户端取消不会触发上游 handler 的 ctx.Done()（handler 不在读写连接），
			// 只能靠固定等待返回以释放连接；时长足够长供测试取消即可。
			select {
			case <-r.Context().Done():
			case <-time.After(200 * time.Millisecond):
			}
			return
		}
		_, _ = w.Write([]byte(`{"id":"ok","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Routes: map[string]Route{"free": {Candidates: []Candidate{{ID: "u", BaseURL: upstream.URL, Model: "up"}}}}}, db)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		r.Handler().ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-begin:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream attempt did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after cancel")
	}
	// 取消后上游连接处于半开状态，显式关闭以免阻塞测试结尾的 upstream.Close()。
	upstream.CloseClientConnections()

	var kind string
	var status int
	if err := db.QueryRow(`SELECT error_kind, status_code FROM llm_requests`).Scan(&kind, &status); err != nil {
		t.Fatalf("ledger row missing: %v", err)
	}
	if kind != "client_canceled" || status != 499 {
		t.Fatalf("ledger kind=%q status=%d", kind, status)
	}

	rec2 := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	// 第二个请求留下的 keep-alive 连接，显式关闭以免阻塞测试结尾的 upstream.Close()。
	upstream.CloseClientConnections()
}

// TestAttemptTimeoutStreamDoesNotCooldownUpstream 验证：候选 attempt_timeout 在流式途中
// 触发（网关自己的调用预算，不是上游故障）时，候选不应被冷却。
func TestAttemptTimeoutStreamDoesNotCooldownUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// 第一个请求持续流式输出，直到网关因 attempt_timeout 中断对上游流的读取。
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			for i := 0; ; i++ {
				_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"chunk-%d\"}}]}\n\n", i)
				if flusher != nil {
					flusher.Flush()
				}
				select {
				case <-r.Context().Done():
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok-2","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{
		RequestTimeout: 10 * time.Second,
		Routes: map[string]Route{"free": {Candidates: []Candidate{
			{ID: "u", BaseURL: upstream.URL, Model: "up", AttemptTimeout: 100 * time.Millisecond},
		}}},
	}, db)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[],"stream":true}`))
	rec := &signalWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		r.Handler().ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-rec.started:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not start")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after attempt timeout")
	}

	// 超时是候选级调用预算，不是上游故障：账本记录 attempt_timeout 且写入成功。
	var kind string
	var status int
	if err := db.QueryRow(`SELECT error_kind, status_code FROM llm_requests`).Scan(&kind, &status); err != nil {
		t.Fatalf("ledger row missing: %v", err)
	}
	if kind != "attempt_timeout" || status != http.StatusGatewayTimeout {
		t.Fatalf("ledger kind=%q status=%d", kind, status)
	}

	// 候选未被冷却：紧随其后的请求应立即成功，而不是 503 upstreams_unavailable。
	rec2 := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request status=%d body=%s", rec2.Code, rec2.Body.String())
	}
}
