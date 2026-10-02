package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	json "github.com/goccy/go-json"
)

// resetZcodeTokenCache 清空凭据文件的进程内缓存（5s TTL 会跨测试串味）。
func resetZcodeTokenCache() {
	zcodeTokenCache.Lock()
	defer zcodeTokenCache.Unlock()
	zcodeTokenCache.at = time.Time{}
	zcodeTokenCache.value = nil
}

// fakeZcodeGateway 同时模拟网关（OAuth init/poll + Messages 探测路径）。
type fakeZcodeGateway struct {
	t           *testing.T
	mu          sync.Mutex
	pollBodies  []string // 依次返回的 poll 响应；空表则一直 pending
	pollIndex   int
	probeBodies chan<- *zcodeProbeRecord // 探测请求汇报（可为 nil）
	initBodies  chan<- *http.Request
}

// zcodeProbeRecord 在 handler 内捕获探测请求的头与体：net/http 在
// handler 返回后会回收请求体，测试侧再读会拿到空。
type zcodeProbeRecord struct {
	Header http.Header
	Body   []byte
}

func (f *fakeZcodeGateway) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	f.handler(w, req)
}

func (f *fakeZcodeGateway) handler(w http.ResponseWriter, req *http.Request) {
	switch {
	case req.URL.Path == "/api/v1/oauth/cli/init":
		if f.initBodies != nil {
			f.initBodies <- req
		}
		if got := req.Header.Get("User-Agent"); got != "ZCode/"+ZcodeClientVersion {
			f.t.Errorf("init user-agent=%q", got)
		}
		_, _ = w.Write([]byte(`{"data":{"flow_id":"flow-1","authorize_url":"https://bigmodel.cn/code/abc","poll_token":"srv-poll-token","poll_interval_sec":0,"expires_at":4102444800}}`))
	case strings.HasPrefix(req.URL.Path, "/api/v1/oauth/cli/poll/"):
		f.mu.Lock()
		body := "pending"
		if f.pollIndex < len(f.pollBodies) {
			body = f.pollBodies[f.pollIndex]
			f.pollIndex++
		}
		f.mu.Unlock()
		if got := req.Header.Get("Authorization"); got != "Bearer srv-poll-token" {
			f.t.Errorf("poll authorization=%q", got)
		}
		_, _ = w.Write([]byte(body))
	case req.URL.Path == "/api/v1/ultra/anthropic/v1/messages":
		if f.probeBodies != nil {
			body, _ := io.ReadAll(req.Body)
			f.probeBodies <- &zcodeProbeRecord{Header: req.Header.Clone(), Body: body}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"p","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	default:
		f.t.Errorf("unexpected path %s", req.URL.Path)
		http.NotFound(w, req)
	}
}

// fakeBigmodelBiz 模拟 BigModel 业务链：账户 → key 列表/创建 → secret。
func fakeBigmodelBiz(t *testing.T, existingKey bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		auth := req.Header.Get("Authorization")
		// BigModel 接受裸 OAuth token，绝不能带 Bearer 前缀。
		if auth != "oauth-token-1" {
			t.Errorf("biz authorization=%q (want bare oauth token)", auth)
		}
		switch {
		case req.URL.Path == "/api/biz/customer/getCustomerInfo":
			_, _ = w.Write([]byte(`{"code":0,"data":{"organizations":[{"organizationName":"默认机构","organizationId":"org1","projects":[{"projectName":"默认项目","projectId":"prj1"}]}]}}`))
		case req.URL.Path == "/api/biz/v1/organization/org1/projects/prj1/api_keys" && req.Method == http.MethodGet:
			if existingKey {
				_, _ = w.Write([]byte(`{"code":0,"data":[{"name":"zcode-api-key","apiKey":"111"}]}`))
			} else {
				_, _ = w.Write([]byte(`{"code":0,"data":[]}`))
			}
		case req.URL.Path == "/api/biz/v1/organization/org1/projects/prj1/api_keys" && req.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"code":0,"data":{"name":"zcode-api-key","apiKey":"111"}}`))
		case req.URL.Path == "/api/biz/v1/organization/org1/projects/prj1/api_keys/copy/111":
			_, _ = w.Write([]byte(`{"code":0,"data":{"secretKey":"222"}}`))
		default:
			t.Errorf("unexpected biz path %s %s", req.Method, req.URL.Path)
			http.NotFound(w, req)
		}
	}))
}

func TestRunZcodeLoginHappyPath(t *testing.T) {
	probeRequests := make(chan *zcodeProbeRecord, 4)
	initRequests := make(chan *http.Request, 4)
	gateway := httptest.NewServer(&fakeZcodeGateway{
		t: t, pollBodies: []string{
			`{"data":{"status":"pending"}}`,
			`{"data":{"status":"ready","bigmodel":{"access_token":"oauth-token-1"},"token":"platform-jwt","user":{"name":"张三"}}}`,
		},
		probeBodies: probeRequests, initBodies: initRequests,
	})
	defer gateway.Close()
	biz := fakeBigmodelBiz(t, true)
	defer biz.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	resetZcodeTokenCache()
	t.Cleanup(resetZcodeTokenCache)

	opts := ZcodeLoginOptions{
		GatewayOrigin:  gateway.URL,
		Channel:        ZcodeChannelBigmodel,
		PollTimeout:    10 * timeSecond(),
		TokenStorePath: filepath.Join(home, ".nanorouter", zcodeTokenStoreName),
		Output:         io.Discard,
	}
	// 业务主机指向测试服务器。
	restore := replaceZcodeBizOrigins(biz.URL, biz.URL)
	defer restore()

	path, err := RunZcodeLogin(context.Background(), opts)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if path != opts.TokenStorePath {
		t.Fatalf("path=%q", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	entry := document["bigmodel"]
	if entry == nil {
		t.Fatalf("document=%v", document)
	}
	if entry["api_key"] != "111.222" {
		t.Fatalf("api_key=%v（应为 id.secret 形式）", entry["api_key"])
	}
	if entry["zcode_jwt_token"] != "platform-jwt" || entry["oauth_access_token"] != "oauth-token-1" {
		t.Fatalf("platform pair missing: %v", entry)
	}
	// 探测请求必须是 max_tokens:1 的 Messages 形状。
	select {
	case record := <-probeRequests:
		var probe map[string]any
		if err := json.Unmarshal(record.Body, &probe); err != nil {
			t.Fatalf("probe body=%s err=%v", record.Body, err)
		}
		if probe["max_tokens"] != float64(1) {
			t.Fatalf("probe=%v", probe)
		}
		if got := record.Header.Get("x-api-key"); got != "111.222" {
			t.Fatalf("probe key=%q", got)
		}
	default:
		t.Fatal("probe request missing")
	}
	// 网关回退读取：凭据文件 → key。
	if key := zcodeTokenFileKey(ZcodeChannelBigmodel); key != "111.222" {
		t.Fatalf("fallback key=%q", key)
	}
}

func TestRunZcodeLoginCreatesKeyWhenMissing(t *testing.T) {
	gateway := httptest.NewServer(&fakeZcodeGateway{
		t: t, pollBodies: []string{
			`{"data":{"status":"ready","bigmodel":{"access_token":"oauth-token-1"}}}`,
		},
	})
	defer gateway.Close()
	biz := fakeBigmodelBiz(t, false)
	defer biz.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetZcodeTokenCache()
	t.Cleanup(resetZcodeTokenCache)
	restore := replaceZcodeBizOrigins(biz.URL, biz.URL)
	defer restore()
	path, err := RunZcodeLogin(context.Background(), ZcodeLoginOptions{
		GatewayOrigin:  gateway.URL,
		Channel:        ZcodeChannelBigmodel,
		TokenStorePath: filepath.Join(home, "store.json"),
		Output:         io.Discard,
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "111.222") {
		t.Fatalf("created key missing: %s", raw)
	}
}

func TestRunZcodeLoginProbeRefusalFails(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/api/v1/oauth/cli/init":
			_, _ = w.Write([]byte(`{"data":{"flow_id":"f","authorize_url":"https://bigmodel.cn/x"}}`))
		case strings.HasPrefix(req.URL.Path, "/api/v1/oauth/cli/poll/"):
			_, _ = w.Write([]byte(`{"data":{"status":"ready","bigmodel":{"access_token":"oauth-token-1"}}}`))
		default:
			// 探测：网关拒绝该 key。
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"身份验证失败。"}}`))
		}
	}))
	defer gateway.Close()
	biz := fakeBigmodelBiz(t, true)
	defer biz.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetZcodeTokenCache()
	t.Cleanup(resetZcodeTokenCache)
	restore := replaceZcodeBizOrigins(biz.URL, biz.URL)
	defer restore()
	_, err := RunZcodeLogin(context.Background(), ZcodeLoginOptions{
		GatewayOrigin:  gateway.URL,
		Channel:        ZcodeChannelBigmodel,
		TokenStorePath: filepath.Join(home, "store.json"),
		Output:         io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "身份验证失败") {
		t.Fatalf("err=%v（探测被拒必须失败且不落盘）", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, "store.json")); !os.IsNotExist(statErr) {
		t.Fatal("refused credential must not be stored")
	}
}

func TestRunZcodeLoginDenied(t *testing.T) {
	gateway := httptest.NewServer(&fakeZcodeGateway{
		t: t, pollBodies: []string{`{"data":{"status":"failed"}}`},
	})
	defer gateway.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetZcodeTokenCache()
	t.Cleanup(resetZcodeTokenCache)
	_, err := RunZcodeLogin(context.Background(), ZcodeLoginOptions{
		GatewayOrigin:  gateway.URL,
		Channel:        ZcodeChannelBigmodel,
		TokenStorePath: filepath.Join(home, "store.json"),
		Output:         io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("err=%v", err)
	}
}

func TestParseZcodeDevicePoll(t *testing.T) {
	ready := parseZcodeDevicePoll([]byte(`{"data":{"status":"ready","zai":{"access_token":"t1"},"token":"jwt"}}`), ZcodeChannelZai)
	if ready.kind != "success" || ready.token != "t1" || ready.jwt != "jwt" {
		t.Fatalf("ready=%+v", ready)
	}
	// 顶层 accessToken 兼容。
	legacy := parseZcodeDevicePoll([]byte(`{"data":{"status":"ready","accessToken":"t2"}}`), ZcodeChannelBigmodel)
	if legacy.kind != "success" || legacy.token != "t2" {
		t.Fatalf("legacy=%+v", legacy)
	}
	if code := parseZcodeDevicePoll([]byte(`{"code":3004}`), ZcodeChannelBigmodel); code.kind != "expired" {
		t.Fatalf("expired=%+v", code)
	}
	if denied := parseZcodeDevicePoll([]byte(`{"data":{"status":"failed"}}`), ZcodeChannelBigmodel); denied.kind != "denied" {
		t.Fatalf("denied=%+v", denied)
	}
}

func timeSecond() time.Duration { return time.Second }

// replaceZcodeBizOrigins 把兑换链的业务主机替换为测试服务器，返回恢复函数。
func replaceZcodeBizOrigins(bigmodel, zai string) (restore func()) {
	oldBigmodel, oldZai := zcodeBigmodelBizURL, zcodeZaiBizURL
	zcodeBigmodelBizURL = bigmodel
	zcodeZaiBizURL = zai
	return func() {
		zcodeBigmodelBizURL = oldBigmodel
		zcodeZaiBizURL = oldZai
	}
}

func TestStoreZcodeTokenPermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".nanorouter", zcodeTokenStoreName)
	got, err := storeZcodeToken(path, ZcodeChannelBigmodel, "k1", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	// 凭据文件必须 0600：预存松权限文件在重写后同样被收紧。
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm=%v，凭据文件必须 0600", perm)
	}
	if err := os.Chmod(got, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := storeZcodeToken(path, ZcodeChannelZai, "k2", "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm=%v，重写后必须收紧回 0600", perm)
	}
}

func TestParseZcodeDeviceCodeRejectsInsecureURL(t *testing.T) {
	initBody := func(authURL string) []byte {
		return []byte(`{"data":{"flow_id":"f","authorize_url":"` + authURL + `"}}`)
	}
	if _, err := parseZcodeDeviceCode(initBody("file:///etc/passwd"), "https://zcode.z.ai"); err == nil {
		t.Fatal("file:// must be rejected")
	}
	if _, err := parseZcodeDeviceCode(initBody("http://evil.example.com/x"), "https://zcode.z.ai"); err == nil {
		t.Fatal("non-loopback http must be rejected")
	}
	if _, err := parseZcodeDeviceCode(initBody("https://bigmodel.cn/code/abc"), "https://zcode.z.ai"); err != nil {
		t.Fatalf("https must pass: %v", err)
	}
	if _, err := parseZcodeDeviceCode(initBody("http://127.0.0.1:8080/x"), "https://zcode.z.ai"); err != nil {
		t.Fatalf("loopback http must pass: %v", err)
	}
}
