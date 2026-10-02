package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	json2 "github.com/goccy/go-json"
)

// zcodeLoginTestRouter 造一个带 auth_env 的网关，供登录端点测试复用。
func zcodeLoginTestRouter(t *testing.T) *Router {
	t.Helper()
	r, _ := newZcodeTestRouter(t, Candidate{ID: "z", BaseURL: "http://127.0.0.1:1", Model: "m"})
	return r
}

func postZcodeLogin(t *testing.T, r *Router, query, token string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/zcode/login" + query
	req := httptest.NewRequest(http.MethodPost, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	return rec
}

func getZcodeLogin(t *testing.T, r *Router, token string) zcodeLoginTask {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/zcode/login", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var task zcodeLoginTask
	if err := json2.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode=%v body=%s", err, rec.Body.String())
	}
	return task
}

func TestZcodeLoginStatusIsIdleBeforeTrigger(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetZcodeTokenCache()
	t.Cleanup(resetZcodeTokenCache)

	r := zcodeLoginTestRouter(t)
	if phase := getZcodeLogin(t, r, "").Phase; phase != zcodeLoginIdle {
		t.Fatalf("phase=%q（未触发时必须是 idle）", phase)
	}
}

func TestZcodeLoginEndpointRejectsBadChannel(t *testing.T) {
	r := zcodeLoginTestRouter(t)
	rec := postZcodeLogin(t, r, "?channel=nope", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestZcodeLoginEndpointReturnsAuthURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetZcodeTokenCache()
	t.Cleanup(resetZcodeTokenCache)

	// 网关下发一个"待授权"的设备码：轮询永远 pending，任务停在 awaiting。
	gateway := httptest.NewServer(&fakeZcodeGateway{t: t})
	defer gateway.Close()
	defer func(v string) { zcodeGatewayOriginOverride = v }(zcodeGatewayOriginOverride)
	zcodeGatewayOriginOverride = gateway.URL

	r := zcodeLoginTestRouter(t)
	rec := postZcodeLogin(t, r, "", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var task zcodeLoginTask
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode=%v body=%s", err, rec.Body.String())
	}
	if task.AuthURL == "" {
		t.Fatal("必须返回授权链接")
	}
	if task.Phase != zcodeLoginAwaiting {
		t.Fatalf("phase=%q", task.Phase)
	}
	if task.Channel != ZcodeChannelBigmodel {
		t.Fatalf("channel=%q", task.Channel)
	}
	// 状态端点应看到同一个链接。
	if got := getZcodeLogin(t, r, ""); got.AuthURL != task.AuthURL {
		t.Fatalf("status auth_url=%q", got.AuthURL)
	}
}

func TestZcodeLoginEndpointRejectsConcurrentTrigger(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetZcodeTokenCache()
	t.Cleanup(resetZcodeTokenCache)

	gateway := httptest.NewServer(&fakeZcodeGateway{t: t})
	defer gateway.Close()
	defer func(v string) { zcodeGatewayOriginOverride = v }(zcodeGatewayOriginOverride)
	zcodeGatewayOriginOverride = gateway.URL

	r := zcodeLoginTestRouter(t)
	if rec := postZcodeLogin(t, r, "", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("首次触发 status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec := postZcodeLogin(t, r, "", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("并发触发 status=%d body=%s（应为 409）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "auth_url") {
		t.Fatalf("409 应带回当前任务快照: %s", rec.Body.String())
	}
}

func TestZcodeLoginTaskExpiredAllowsRetrigger(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetZcodeTokenCache()
	t.Cleanup(resetZcodeTokenCache)

	gateway := httptest.NewServer(&fakeZcodeGateway{t: t})
	defer gateway.Close()
	defer func(v string) { zcodeGatewayOriginOverride = v }(zcodeGatewayOriginOverride)
	zcodeGatewayOriginOverride = gateway.URL

	r := zcodeLoginTestRouter(t)
	// 先制造一个"设备码已过期但仍在等待"的任务：active 应为 false。
	r.mu.Lock()
	r.zcodeLogin = &zcodeLoginTask{
		Phase:     zcodeLoginAwaiting,
		Channel:   ZcodeChannelBigmodel,
		AuthURL:   "https://bigmodel.cn/expired",
		ExpiresAt: time.Now().Add(-time.Minute),
	}
	r.mu.Unlock()

	if rec := postZcodeLogin(t, r, "", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("过期任务应可重新触发 status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestZcodeLoginTaskActiveOnlyWhilePending(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		task *zcodeLoginTask
		want bool
	}{
		{"nil", nil, false},
		{"awaiting 未过期", &zcodeLoginTask{Phase: zcodeLoginAwaiting, ExpiresAt: now.Add(time.Minute)}, true},
		{"awaiting 无过期时刻", &zcodeLoginTask{Phase: zcodeLoginAwaiting}, true},
		{"awaiting 已过期", &zcodeLoginTask{Phase: zcodeLoginAwaiting, ExpiresAt: now.Add(-time.Minute)}, false},
		{"exchanging", &zcodeLoginTask{Phase: zcodeLoginExchanging}, true},
		{"succeeded", &zcodeLoginTask{Phase: zcodeLoginSucceeded}, false},
		{"failed", &zcodeLoginTask{Phase: zcodeLoginFailed}, false},
		{"idle", &zcodeLoginTask{Phase: zcodeLoginIdle}, false},
	}
	for _, tc := range cases {
		if got := tc.task.active(now); got != tc.want {
			t.Fatalf("%s: active=%v want=%v", tc.name, got, tc.want)
		}
	}
}

func TestZcodeLoginBeginIsExclusive(t *testing.T) {
	r := zcodeLoginTestRouter(t)
	now := time.Now()
	if !r.beginZcodeLogin(ZcodeChannelBigmodel, now) {
		t.Fatal("首次抢占应成功")
	}
	if r.beginZcodeLogin(ZcodeChannelBigmodel, now) {
		t.Fatal("已有任务时抢占应失败")
	}
	if got := r.zcodeLoginSnapshot().Phase; got != zcodeLoginAwaiting {
		t.Fatalf("phase=%q", got)
	}
}

func TestZcodeLoginBeginConcurrentOnlyOneWins(t *testing.T) {
	r := zcodeLoginTestRouter(t)
	const n = 16
	var wg sync.WaitGroup
	results := make([]bool, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx] = r.beginZcodeLogin(ZcodeChannelBigmodel, time.Now())
		}(i)
	}
	close(start)
	wg.Wait()
	wins := 0
	for _, ok := range results {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("并发抢占成功 %d 次，必须恰好 1 次", wins)
	}
}

func TestZcodeLoginFinishClearsLinkOnSuccess(t *testing.T) {
	r := zcodeLoginTestRouter(t)
	r.beginZcodeLogin(ZcodeChannelBigmodel, time.Now())
	r.updateZcodeLogin(func(task *zcodeLoginTask) { task.AuthURL = "https://bigmodel.cn/x" })

	r.finishZcodeLogin(zcodeLoginSucceeded, nil, "/tmp/store.json")
	snapshot := r.zcodeLoginSnapshot()
	if snapshot.Phase != zcodeLoginSucceeded {
		t.Fatalf("phase=%q", snapshot.Phase)
	}
	if snapshot.AuthURL != "" {
		t.Fatalf("成功后应清掉授权链接，得到 %q", snapshot.AuthURL)
	}
	if snapshot.StorePath != "/tmp/store.json" {
		t.Fatalf("store_path=%q", snapshot.StorePath)
	}

	// 失败则保留错误信息，且链接仍可回看（便于排查点错了什么）。
	r.beginZcodeLogin(ZcodeChannelBigmodel, time.Now())
	r.updateZcodeLogin(func(task *zcodeLoginTask) { task.AuthURL = "https://bigmodel.cn/y" })
	r.finishZcodeLogin(zcodeLoginFailed, errBoom, "")
	snapshot = r.zcodeLoginSnapshot()
	if snapshot.Error == "" || snapshot.AuthURL != "https://bigmodel.cn/y" {
		t.Fatalf("failed snapshot=%+v", snapshot)
	}
}

func TestZcodeLoginRequiresAuthWhenAuthEnvSet(t *testing.T) {
	t.Setenv("NANOROUTER_TEST_AUTH", "sekret")
	r, _ := newZcodeTestRouter(t, Candidate{ID: "z", BaseURL: "http://127.0.0.1:1", Model: "m"})
	// 直接把 auth_env 指到测试变量上。
	r.cfg.AuthEnv = "NANOROUTER_TEST_AUTH"

	req := httptest.NewRequest(http.MethodPost, "/zcode/login", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权应 401，得到 %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/zcode/login", nil)
	rec = httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权 GET 应 401，得到 %d", rec.Code)
	}
}

var errBoom = &boomError{}

type boomError struct{}

func (*boomError) Error() string { return "boom" }

func TestNormalizeZcodeChannel(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantOK  bool
		comment string
	}{
		{"", ZcodeChannelBigmodel, true, "空串取默认"},
		{"bigmodel", ZcodeChannelBigmodel, true, ""},
		{"zai", ZcodeChannelZai, true, ""},
		{" zai ", ZcodeChannelZai, true, "两侧空白应被裁掉"},
		{"nope", "", false, "未知渠道应拒绝"},
	}
	for _, tc := range cases {
		got, ok := normalizeZcodeChannel(tc.in)
		if ok != tc.wantOK || got != tc.want {
			t.Fatalf("normalize(%q)=(%q,%v) want=(%q,%v) %s", tc.in, got, ok, tc.want, tc.wantOK, tc.comment)
		}
	}
}

func TestZcodeLoginStatusSnapshotIsCopy(t *testing.T) {
	r := zcodeLoginTestRouter(t)
	r.beginZcodeLogin(ZcodeChannelBigmodel, time.Now())
	first := r.zcodeLoginSnapshot()
	r.finishZcodeLogin(zcodeLoginSucceeded, nil, "/tmp/a.json")
	if first.Phase == r.zcodeLoginSnapshot().Phase {
		t.Fatal("快照必须是副本：后续状态变化不应回写已取出的快照")
	}
}

// 端点在真实登录后应当把凭据写到凭据文件——这条保证「授权完成后
// 热加载」的前提成立：文件写了，候选就能在 5 秒缓存过期后用上。
func TestZcodeLoginWritesCredentialFileForHotReload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetZcodeTokenCache()
	t.Cleanup(resetZcodeTokenCache)

	gateway := httptest.NewServer(&fakeZcodeGateway{
		t: t, pollBodies: []string{
			`{"data":{"status":"pending"}}`,
			`{"data":{"status":"ready","bigmodel":{"access_token":"oauth-token-1"},"token":"platform-jwt"}}`,
		},
	})
	defer gateway.Close()
	biz := fakeBigmodelBiz(t, true)
	defer biz.Close()
	defer replaceZcodeBizOrigins(biz.URL, biz.URL)()
	defer func(v string) { zcodeGatewayOriginOverride = v }(zcodeGatewayOriginOverride)
	zcodeGatewayOriginOverride = gateway.URL

	r := zcodeLoginTestRouter(t)
	rec := postZcodeLogin(t, r, "", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 轮询到终态：登录链自己会跑完（fake gateway 第二次 poll 即 ready）。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if task := getZcodeLogin(t, r, ""); task.Phase == zcodeLoginSucceeded || task.Phase == zcodeLoginFailed {
			if task.Phase != zcodeLoginSucceeded {
				t.Fatalf("登录失败: %+v", task)
			}
			path := filepath.Join(home, ".nanorouter", zcodeTokenStoreName)
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("凭据文件未生成: %v", err)
			}
			// 凭据应能被现有的解析链读到（无需重启即可用的前提）。
			resetZcodeTokenCache()
			if key := zcodeTokenFileKey(ZcodeChannelBigmodel); key == "" {
				t.Fatal("凭据文件已生成但解析不到 key")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("登录未在预期时间内到达终态")
}
