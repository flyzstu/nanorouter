package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLiveSetDedupAndTTL(t *testing.T) {
	set := newLiveSet(100)
	now := time.Now()
	if got := set.count(now); got != 0 {
		t.Fatalf("empty set count = %d", got)
	}
	// 空键不计入：无会话亲和的请求不能虚增会话数。
	if got := set.touch("", now); got != 0 {
		t.Fatalf("empty key counted: %d", got)
	}
	set.touch("s1", now)
	set.touch("s2", now)
	set.touch("s1", now.Add(time.Second)) // 重复 touch 只算一个
	if got := set.count(now.Add(2 * time.Second)); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
	// 窗口外整体过期：5 分钟无活动后归零。
	if got := set.count(now.Add(liveSessionTTL + time.Minute)); got != 0 {
		t.Fatalf("expired count = %d, want 0", got)
	}
	// 过期后可重新积累。
	set.touch("s3", now.Add(liveSessionTTL+2*time.Minute))
	if got := set.count(now.Add(liveSessionTTL + 2*time.Minute)); got != 1 {
		t.Fatalf("rebuild count = %d, want 1", got)
	}
}

func TestLiveTrackerSnapshot(t *testing.T) {
	tracker := newLiveTracker()
	base := time.Now()
	tracker.now = func() time.Time { return base }

	tracker.enter()
	tracker.enter()
	tracker.recordRequest("sess-a", "10.0.0.1")
	tracker.recordRequest("sess-a", "10.0.0.1") // 同会话同 IP 去重
	tracker.recordRequest("sess-b", "10.0.0.2")
	tracker.recordRequest("", "10.0.0.2") // 无会话键的请求只算调用方

	snap := tracker.snapshot()
	if snap.Inflight != 2 {
		t.Fatalf("inflight = %d, want 2", snap.Inflight)
	}
	if snap.ActiveSessions != 2 {
		t.Fatalf("sessions = %d, want 2", snap.ActiveSessions)
	}
	if snap.ActiveClients != 2 {
		t.Fatalf("clients = %d, want 2", snap.ActiveClients)
	}
	tracker.leave()
	tracker.leave()
	if tracker.snapshot().Inflight != 0 {
		t.Fatal("inflight should return to 0")
	}
}

func TestStatsSummaryCarriesLiveStatsFromProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	r, err := New(Config{
		Routes: map[string]Route{"free": {Candidates: []Candidate{{ID: "u", BaseURL: upstream.URL, Model: "up"}}}},
	}, mustOpenMemoryDB(t))
	if err != nil {
		t.Fatal(err)
	}
	handler := r.Handler()

	// 三个请求：会话 s1（IP .1）、会话 s2（IP .2）、无会话键（IP .1）。
	requests := []struct{ session, remote string }{
		{"s1", "10.0.0.1:1111"},
		{"s2", "10.0.0.2:2222"},
		{"", "10.0.0.1:3333"},
	}
	for _, tc := range requests {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free","messages":[]}`))
		req.RemoteAddr = tc.remote
		if tc.session != "" {
			req.Header.Set(sessionAffinityHeader, tc.session)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("proxy status = %d body = %s", rec.Code, rec.Body.String())
		}
	}

	// 全部完成后：inflight 归零，窗口内 2 个会话（空键不计）、2 个调用方。
	summaryReq := httptest.NewRequest(http.MethodGet, "/v1/stats/summary?range=1h", nil)
	rec := httptest.NewRecorder()
	r.statsSummary(rec, summaryReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("summary status = %d", rec.Code)
	}
	var summary StatsSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Live.Inflight != 0 {
		t.Fatalf("live.inflight = %d, want 0 after completion", summary.Live.Inflight)
	}
	if summary.Live.ActiveSessions != 2 {
		t.Fatalf("live.active_sessions = %d, want 2", summary.Live.ActiveSessions)
	}
	if summary.Live.ActiveClients != 2 {
		t.Fatalf("live.active_clients = %d, want 2", summary.Live.ActiveClients)
	}
}
