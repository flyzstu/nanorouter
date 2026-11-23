package router

// PAT 签发与缓存的五条路径：首签成功、提前刷新降级、401 立即失效、
// enable:false、429 带 retry-after。全部走 httptest，不碰真实上游。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func patTestServer(t *testing.T, handler func(w http.ResponseWriter, req *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if auth := req.Header.Get("Authorization"); auth != "Bearer login-1" {
			t.Errorf("issue authorization=%q (want Bearer login-1)", auth)
		}
		handler(w, req)
	}))
}

func patEnvelope(token string, expiresAt int64) string {
	return fmt.Sprintf(`{"code":0,"data":{"enable":true,"accessToken":%q,"tokenType":"Bearer","expiresIn":3600,"expiresAt":%d}}`, token, expiresAt)
}

func patTestLocation() zcodePATLocation {
	return zcodePATLocation{OrganizationID: "org1", ProjectID: "prj1", APIKeyID: "111"}
}

func TestZcodePATFirstIssue(t *testing.T) {
	srv := patTestServer(t, func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/api_keys/111/access_tokens") {
			t.Errorf("unexpected path %s", req.URL.Path)
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write([]byte(patEnvelope("pat-1", time.Now().Add(time.Hour).Unix())))
	})
	defer srv.Close()
	c := newZcodePATCache()
	token, expiresAt, err := c.resolve(context.Background(), srv.Client(), srv.URL, "bigmodel", "login-1", "bigmodel", patTestLocation(), "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if token != "pat-1" {
		t.Fatalf("token=%q", token)
	}
	if time.Until(expiresAt) < 50*time.Minute {
		t.Fatalf("expiresAt=%v（应约 1h）", expiresAt)
	}
	// 同代次内第二次取应命中缓存，不再发请求。
	token2, _, err := c.resolve(context.Background(), srv.Client(), srv.URL, "bigmodel", "login-1", "bigmodel", patTestLocation(), "")
	if err != nil || token2 != "pat-1" {
		t.Fatalf("cached token=%q err=%v", token2, err)
	}
}

func TestZcodePATPrefetchDegrade(t *testing.T) {
	var calls atomic.Int32
	srv := patTestServer(t, func(w http.ResponseWriter, req *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(patEnvelope("pat-old", time.Now().Add(time.Hour).Unix())))
			return
		}
		// 刷新时 429：舊 PAT 未过期应降级复用。
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	defer srv.Close()
	c := newZcodePATCache()
	// now 可调：首签用真实时间，刷新时快进到 refresh 点之后、过期之前。
	ctx := context.Background()
	token, _, err := c.resolve(ctx, srv.Client(), srv.URL, "bigmodel", "login-1", "bigmodel", patTestLocation(), "")
	if err != nil || token != "pat-old" {
		t.Fatalf("first=%q err=%v", token, err)
	}
	// 把条目推到刷新点之后：直接改 refreshAt（同包白盒）。
	c.mu.Lock()
	for _, entry := range c.entries {
		entry.refreshAt = time.Now().Add(-time.Minute)
	}
	c.mu.Unlock()
	token2, _, err := c.resolve(ctx, srv.Client(), srv.URL, "bigmodel", "login-1", "bigmodel", patTestLocation(), "")
	if err != nil || token2 != "pat-old" {
		t.Fatalf("degraded=%q err=%v", token2, err)
	}
	if calls.Load() < 2 {
		t.Fatalf("calls=%d（刷新请求未发出）", calls.Load())
	}
}

func TestZcodePATUnauthorizedInvalidates(t *testing.T) {
	var calls atomic.Int32
	srv := patTestServer(t, func(w http.ResponseWriter, req *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			_, _ = w.Write([]byte(patEnvelope("pat-bad", time.Now().Add(time.Hour).Unix())))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":1000,"msg":"unauthorized"}`))
	})
	defer srv.Close()
	c := newZcodePATCache()
	ctx := context.Background()
	token, _, err := c.resolve(ctx, srv.Client(), srv.URL, "bigmodel", "login-1", "bigmodel", patTestLocation(), "")
	if err != nil || token != "pat-bad" {
		t.Fatalf("first=%q err=%v", token, err)
	}
	// 上游 401 回调废弃该代 PAT。
	c.noteRejected("pat-bad")
	c.mu.Lock()
	for _, entry := range c.entries {
		entry.refreshAt = time.Now().Add(-time.Minute)
	}
	c.mu.Unlock()
	_, _, err = c.resolve(ctx, srv.Client(), srv.URL, "bigmodel", "login-1", "bigmodel", patTestLocation(), "")
	if err == nil {
		t.Fatal("reissue after 401 should fail")
	}
	if code := zcodePATErrorCode(err); code != "project_token_http_401" {
		t.Fatalf("code=%q", code)
	}
}

func TestZcodePATDisabled(t *testing.T) {
	srv := patTestServer(t, func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"enable":false}}`))
	})
	defer srv.Close()
	c := newZcodePATCache()
	_, _, err := c.resolve(context.Background(), srv.Client(), srv.URL, "bigmodel", "login-1", "bigmodel", patTestLocation(), "")
	if code := zcodePATErrorCode(err); code != "project_token_disabled" {
		t.Fatalf("code=%q err=%v", code, err)
	}
}

func TestZcodePATRetryAfterHonored(t *testing.T) {
	var calls atomic.Int32
	srv := patTestServer(t, func(w http.ResponseWriter, req *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(patEnvelope("pat-retry", time.Now().Add(time.Hour).Unix())))
	})
	defer srv.Close()
	c := newZcodePATCache()
	start := time.Now()
	token, _, err := c.resolve(context.Background(), srv.Client(), srv.URL, "bigmodel", "login-1", "bigmodel", patTestLocation(), "")
	if err != nil || token != "pat-retry" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("elapsed=%v（retry-after 未等待）", elapsed)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d（应恰好重试一次）", calls.Load())
	}
}

// 超限 retry-after（>1000ms）直接交还上游，不截短等待。
func TestZcodePATRetryAfterOverflow(t *testing.T) {
	var calls atomic.Int32
	srv := patTestServer(t, func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	defer srv.Close()
	c := newZcodePATCache()
	_, _, err := c.resolve(context.Background(), srv.Client(), srv.URL, "bigmodel", "login-1", "bigmodel", patTestLocation(), "")
	if code := zcodePATErrorCode(err); code != "project_token_http_429" {
		t.Fatalf("code=%q err=%v", code, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d（超限 retry-after 不得重试）", calls.Load())
	}
}
