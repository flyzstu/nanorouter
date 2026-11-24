package router

import (
	"net/http"
	"testing"
	"time"
)

// 降级读必须带着旧值走：quota 端点 60s 过期，第一个撞上降级窗的请求
// 拿到的是「旧数据 + 新鲜度标注」，而不是空 windows。此前 aggregate 把
// 任何 error 都当硬失败，把随行的旧数据丢了——计量条每分钟消失几秒，
// 套餐徽标同理。stale-while-revalidate 的旧值立即可用，刷新结果服务
// 下一轮；新鲜度问题只该出现在 failures 里。
func TestUsageZcodeStaleServeKeepsWindows(t *testing.T) {
	env := newUsageEnv(t, usageEnvOptions{withZcode: true})
	cache := env.router.zcodeUsageShared()
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	now := base
	cache.now = func() time.Time { return now }

	res, body := getUsage(t, env.router, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("fresh status = %d", res.StatusCode)
	}
	providers, _ := body["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(providers))
	}
	zcode, _ := providers[0].(map[string]any)
	if windows := len(zcode["windows"].([]any)); windows == 0 {
		t.Fatal("fresh response has no windows")
	}
	if failures := len(zcode["failures"].([]any)); failures != 0 {
		t.Fatalf("fresh failures = %v", zcode["failures"])
	}

	// 过了 quota 的 60s 新鲜期（仍在 10 分钟 stale 窗内）：标注降级，
	// 但窗口必须照常渲染。
	now = base.Add(90 * time.Second)
	res, body = getUsage(t, env.router, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stale status = %d, want 200 (旧值即数据)", res.StatusCode)
	}
	providers, _ = body["providers"].([]any)
	zcode, _ = providers[0].(map[string]any)
	windows, _ := zcode["windows"].([]any)
	if len(windows) == 0 {
		t.Fatal("stale serve dropped the degraded windows")
	}
	failed := false
	for _, failure := range zcode["failures"].([]any) {
		if text, _ := failure.(string); text == errZcodeUsageStaleServe.Error() {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("stale serve did not note freshness: %v", zcode["failures"])
	}
}
