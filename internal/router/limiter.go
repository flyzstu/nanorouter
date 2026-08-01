package router

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// candidateLimiter 同时承载固定并发限制与可选 AIMD 自适应窗口。
// notify 使用关闭并重建 channel 的方式唤醒所有等待者，避免 sync.Cond 无法响应 context。
type candidateLimiter struct {
	mu        sync.Mutex
	limit     int
	minimum   int
	maximum   int
	inflight  int
	adaptive  bool
	target    time.Duration
	successes int
	notify    chan struct{}
}

func newCandidateLimiter(candidate Candidate) *candidateLimiter {
	minimum := candidate.MinConcurrency
	if minimum <= 0 {
		minimum = 1
	}
	limit := candidate.MaxConcurrency
	target := candidate.TargetTTFB
	if target == 0 {
		target = 2 * time.Second
	}
	if candidate.AdaptiveConcurrency {
		limit = minimum
	}
	return &candidateLimiter{
		limit: limit, minimum: minimum, maximum: candidate.MaxConcurrency,
		adaptive: candidate.AdaptiveConcurrency, target: target, notify: make(chan struct{}),
	}
}

func (l *candidateLimiter) acquire(ctx context.Context, timeout time.Duration) (func(), bool) {
	if timeout == 0 {
		l.mu.Lock()
		if l.inflight >= l.limit {
			l.mu.Unlock()
			return nil, false
		}
		l.inflight++
		l.mu.Unlock()
		return l.releaseFunc(), true
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		l.mu.Lock()
		if l.inflight < l.limit {
			l.inflight++
			l.mu.Unlock()
			return l.releaseFunc(), true
		}
		notify := l.notify
		l.mu.Unlock()
		select {
		case <-notify:
		case <-waitCtx.Done():
			return nil, false
		}
	}
}

func (l *candidateLimiter) releaseFunc() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.inflight--
			l.signalLocked()
			l.mu.Unlock()
		})
	}
}

func (l *candidateLimiter) observe(status int, ttfb time.Duration, queued bool) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.adaptive {
		return l.limit
	}
	old := l.limit
	switch {
	case status == http.StatusTooManyRequests || (ttfb > 0 && ttfb > 2*l.target):
		l.limit = max(l.minimum, l.limit/2)
		l.successes = 0
	case status >= 200 && status < 300 && queued && ttfb <= l.target:
		l.successes++
		if l.successes >= l.limit && l.limit < l.maximum {
			l.limit++
			l.successes = 0
		}
	}
	if l.limit != old {
		l.signalLocked()
	}
	return l.limit
}

func (l *candidateLimiter) currentLimit() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}

func (l *candidateLimiter) signalLocked() {
	close(l.notify)
	l.notify = make(chan struct{})
}
