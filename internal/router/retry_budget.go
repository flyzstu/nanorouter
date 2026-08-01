package router

import (
	"math"
	"sync"
)

// retryBudget 是路由级令牌桶：正常流量补充预算，额外上游尝试消耗预算。
// 初始填满 burst，允许冷启动时进行有限故障切换，同时长期放大率受 ratio 约束。
type retryBudget struct {
	mu     sync.Mutex
	ratio  float64
	burst  float64
	tokens float64
}

func newRetryBudget(cfg RetryBudgetConfig) *retryBudget {
	if !cfg.Enabled {
		return nil
	}
	burst := float64(cfg.Burst)
	return &retryBudget{ratio: cfg.Ratio, burst: burst, tokens: burst}
}

func (b *retryBudget) credit() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = math.Min(b.burst, b.tokens+b.ratio)
	return b.tokens
}

func (b *retryBudget) consume() (bool, float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tokens < 1 {
		return false, b.tokens
	}
	b.tokens--
	return true, b.tokens
}

func (b *retryBudget) available() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokens
}
