// Package router 的 Mistral Vibe 浏览器登录流程，语义对齐 mistral-vibe 客户端
// （vibe/setup/auth/browser_sign_in.py + http_browser_sign_in_gateway.py）。
//
// 官方流程（三条 HTTP 端点，全部指向 console 的 /api 前缀）：
//  1. POST {api}/vibe/sign-in            携带 PKCE code_challenge(S256)，
//     返回 {process_id, sign_in_url, poll_url, expires_at}
//  2. GET  poll_url                      轮询状态：
//     pending（继续等）/ completed（携带 exchange_token）/
//     expired / denied / error（终态，报错退出）
//  3. POST {api}/vibe/sign-in/{process_id}/exchange
//     携带 {exchange_token, code_verifier}，返回 {api_key}
//
// 与 Google Antigravity 不同：Mistral 浏览器登录的产物是普通 API key
// （platform 发放），不是 OAuth refresh token；拿到后无需本地刷新，
// 过期/失效时重新登录即可。sign_in_url / poll_url 的 host 必须落在
// console / api 的 origin 下（官方 _validate_url_against_base_url 语义），
// 防止签名过程把用户引到第三方页面。

package router

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// Mistral 默认端点（对齐 vibe/core/config/_defaults.py）。
	mistralBrowserAuthBaseURL = "https://console.mistral.ai"
	mistralBrowserAuthAPIBase = "https://console.mistral.ai/api"

	// MistralDefaultBrowserURL / MistralDefaultAPIBaseURL 是 login 子命令
	// flag 的默认值：sign-in 页面走 console origin，sign-in/exchange 接口
	// 走 console 的 /api 前缀。
	MistralDefaultBrowserURL = mistralBrowserAuthBaseURL
	MistralDefaultAPIBaseURL = mistralBrowserAuthAPIBase

	// 凭据文件名（~/.nanorouter/ 下）。
	mistralTokenStoreName = "mistral_token.json"

	// MistralLoginTimeout 等待浏览器授权的默认时长。
	MistralLoginTimeout = 5 * time.Minute

	// mistralPollInterval 轮询 poll_url 的间隔（官方默认 3s）。
	mistralPollInterval = 3 * time.Second

	// mistralMaxPollFailures 连续轮询失败上限（官方 _max_consecutive_poll_failures）。
	mistralMaxPollFailures = 3

	// mistralChatBaseURL 是候选 base_url 的正确形态：网关按 base+"/"+path
	// 拼接（path=chat/completions），漏掉 /v1 会被上游 404
	// （"no Route matched with those values"）。
	mistralChatBaseURL = "https://api.mistral.ai/v1"
)

// mistralTokenCache 缓存凭据文件的解析结果，语义与 zcodeTokenCache 一致：
// 平台登录一次写入后长期有效，请求路径不必每次读盘。
var mistralTokenCache struct {
	sync.Mutex
	at    time.Time
	value string
}

// MistralLoginOptions 配置 Mistral Vibe 浏览器登录。
type MistralLoginOptions struct {
	// TokenStorePath 覆盖凭据存储路径（默认 ~/.nanorouter/mistral_token.json）。
	TokenStorePath string
	// BrowserBaseURL 覆盖 console 端点（sign_in_url 的 origin 校验基准）。
	BrowserBaseURL string
	// APIBaseURL 覆盖 console /api 端点（sign-in 与 exchange 请求目标）。
	APIBaseURL string
	// PollTimeout 等待授权的总时长（默认 5 分钟）。
	PollTimeout time.Duration
	// PollInterval 覆盖轮询间隔（默认 3s）。
	PollInterval time.Duration
	// OpenBrowser 打开浏览器的函数；nil 表示不自动打开。
	OpenBrowser func(rawURL string) error
	// Output 用于打印进度信息（默认 os.Stderr）。
	Output io.Writer
	// HTTPClient 覆盖登录过程中的 HTTP 客户端（测试注入 httptest 服务）。
	HTTPClient *http.Client
}

// mistralSignInProcess 对应官方 CreateProcessPayload。
type mistralSignInProcess struct {
	ProcessID  string `json:"process_id"`
	SignInURL  string `json:"sign_in_url"`
	PollURL    string `json:"poll_url"`
	ExpiresAt  string `json:"expires_at"`
	SignInPage string `json:"-"` // 校验通过后的 sign_in_url，供打开浏览器
	PollPage   string `json:"-"` // 校验通过后的 poll_url
}

// mistralToken 存储的凭据格式。文件里只存平台 API key 与账号元数据；
// key 的生命周期归 Mistral 平台管，网关不刷新。
type mistralToken struct {
	APIKey    string `json:"api_key"`
	ExpiresAt string `json:"expires_at,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

// RunMistralLogin 执行 Mistral Vibe 浏览器登录，返回凭据文件路径。
func RunMistralLogin(ctx context.Context, opts MistralLoginOptions) (string, error) {
	if opts.PollTimeout == 0 {
		opts.PollTimeout = MistralLoginTimeout
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = mistralPollInterval
	}
	if opts.BrowserBaseURL == "" {
		opts.BrowserBaseURL = mistralBrowserAuthBaseURL
	}
	if opts.APIBaseURL == "" {
		opts.APIBaseURL = mistralBrowserAuthAPIBase
	}
	output := opts.Output
	if output == nil {
		output = os.Stderr
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	say := func(format string, args ...any) {
		fmt.Fprintf(output, "login mistral: "+format+"\n", args...)
	}

	verifier, err := mistralCodeVerifier()
	if err != nil {
		return "", fmt.Errorf("generate pkce verifier: %w", err)
	}
	challenge := mistralCodeChallenge(verifier)

	say("Starting browser sign-in at %s ...", opts.APIBaseURL)
	process, err := mistralStartSignIn(ctx, client, opts.APIBaseURL, mistralOrigin(opts.BrowserBaseURL), mistralOrigin(opts.APIBaseURL), challenge)
	if err != nil {
		return "", fmt.Errorf("start browser sign-in: %w", err)
	}
	say("Process started (id %s, expires %s).", process.ProcessID, process.ExpiresAt)

	if opts.OpenBrowser != nil {
		if err := opts.OpenBrowser(process.SignInPage); err != nil {
			say("Failed to open browser automatically: %v", err)
		}
	}
	say("Open this URL in a browser to authorize:")
	say("")
	say("  %s", process.SignInPage)
	say("")

	apiKey, err := mistralWaitForApproval(ctx, client, process, opts.APIBaseURL, verifier, opts.PollTimeout, opts.PollInterval, say)
	if err != nil {
		return "", err
	}

	path, err := storeMistralToken(opts.TokenStorePath, &mistralToken{
		APIKey:    apiKey,
		ExpiresAt: process.ExpiresAt,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return "", fmt.Errorf("store token failed: %w", err)
	}
	say("Login complete. Credential stored at %s", path)
	say("Configure the candidate with mistral_token_file: true and base_url %s", mistralChatBaseURL)
	return path, nil
}

// mistralStartSignIn 发起 sign-in：POST {api}/vibe/sign-in。
// browserOrigin / apiOrigin 是 console 与 /api 前缀的 origin，用于校验
// 服务端返回的 sign_in_url / poll_url（官方 _validate_url_against_base_url）。
func mistralStartSignIn(ctx context.Context, client *http.Client, apiBase, browserOrigin, apiOrigin, challenge string) (*mistralSignInProcess, error) {
	body, err := json.Marshal(map[string]string{
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(apiBase, "/")+"/vibe/sign-in", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sign-in start failed: %s %s", resp.Status, string(data))
	}

	var payload mistralSignInProcess
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("parse sign-in payload: %w", err)
	}
	if payload.ProcessID == "" || payload.SignInURL == "" || payload.PollURL == "" {
		return nil, fmt.Errorf("sign-in payload missing required fields")
	}
	// 官方语义：返回的 sign_in_url 必须落在 console origin，poll_url 必须
	// 落在 api origin；不符直接拒绝，防止把用户引去第三方页面。
	if origin := mistralOrigin(payload.SignInURL); origin == "" || origin != browserOrigin {
		return nil, fmt.Errorf("sign_in_url origin %q does not match console origin %q", origin, browserOrigin)
	}
	if origin := mistralOrigin(payload.PollURL); origin == "" || origin != apiOrigin {
		return nil, fmt.Errorf("poll_url origin %q does not match api origin %q", origin, apiOrigin)
	}
	payload.SignInPage = payload.SignInURL
	payload.PollPage = payload.PollURL
	return &payload, nil
}

// mistralOrigin 提取 URL 的 origin（scheme://host[:port]），失败返回空串。
func mistralOrigin(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// mistralWaitForApproval 轮询 poll_url 直到 completed / 终态。
func mistralWaitForApproval(ctx context.Context, client *http.Client, process *mistralSignInProcess, apiBase, verifier string, pollTimeout, pollInterval time.Duration, say func(string, ...any)) (string, error) {
	deadline := time.Now().Add(pollTimeout)
	failures := 0
	for time.Now().Before(deadline) {
		status, exchangeToken, message, err := mistralPollOnce(ctx, client, process.PollPage)
		if err != nil {
			failures++
			if failures >= mistralMaxPollFailures {
				return "", fmt.Errorf("poll sign-in status: %w", err)
			}
			say("Poll failed (%d/%d): %v", failures, mistralMaxPollFailures, err)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(pollInterval):
			}
			continue
		}
		failures = 0
		switch status {
		case "pending":
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(pollInterval):
			}
		case "completed":
			if exchangeToken == "" {
				return "", fmt.Errorf("sign-in completed but no exchange_token")
			}
			say("Approved. Exchanging for API key...")
			return mistralExchange(ctx, client, process, apiBase, exchangeToken, verifier)
		case "expired":
			return "", fmt.Errorf("browser sign-in expired")
		case "denied":
			return "", fmt.Errorf("browser sign-in was denied")
		case "error":
			if message == "" {
				message = "browser sign-in failed"
			}
			return "", fmt.Errorf("%s", message)
		default:
			return "", fmt.Errorf("browser sign-in returned unknown status %q", status)
		}
	}
	return "", fmt.Errorf("timeout waiting for approval")
}

// mistralPollOnce GET 一次 poll_url；410 视作 expired（官方 HTTP_GONE 语义）。
func mistralPollOnce(ctx context.Context, client *http.Client, pollURL string) (status, exchangeToken, message string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return "", "", "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", "", err
	}
	if resp.StatusCode == http.StatusGone {
		return "expired", "", "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("poll failed: %s %s", resp.Status, string(data))
	}
	var payload struct {
		Status        string `json:"status"`
		ExchangeToken string `json:"exchange_token"`
		Message       string `json:"message"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", "", "", fmt.Errorf("parse poll payload: %w", err)
	}
	return payload.Status, payload.ExchangeToken, payload.Message, nil
}

// mistralExchange 用 exchange_token + code_verifier 换 API key：
// POST {api}/vibe/sign-in/{process_id}/exchange。
func mistralExchange(ctx context.Context, client *http.Client, process *mistralSignInProcess, apiBase, exchangeToken, verifier string) (string, error) {
	body, err := json.Marshal(map[string]string{
		"exchange_token": exchangeToken,
		"code_verifier":  verifier,
	})
	if err != nil {
		return "", err
	}
	endpoint := strings.TrimRight(apiBase, "/") + "/vibe/sign-in/" + process.ProcessID + "/exchange"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("workspace not eligible for Mistral Vibe; pick another workspace in the browser and sign in again")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("exchange failed: %s %s", resp.Status, string(data))
	}
	var payload struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("parse exchange payload: %w", err)
	}
	if payload.APIKey == "" {
		return "", fmt.Errorf("exchange did not return an API key")
	}
	return payload.APIKey, nil
}

// mistralCodeVerifier 生成 PKCE code_verifier（官方用 64 字节 url-safe 随机串）。
func mistralCodeVerifier() (string, error) {
	buf := make([]byte, 64)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// mistralCodeChallenge 计算 S256 code_challenge。
func mistralCodeChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// storeMistralToken 保存凭据到文件（0600 权限，原子替换）。
func storeMistralToken(path string, token *mistralToken) (string, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory failed: %w", err)
		}
		path = filepath.Join(home, ".nanorouter", mistralTokenStoreName)
	}
	encoded, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode token failed: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create directory failed: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
		return "", fmt.Errorf("write token file failed: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("set permissions failed: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("replace token file failed: %w", err)
	}
	return path, nil
}

// mistralTokenFileKey 从凭据文件读取 Mistral API key（带 60s 进程内缓存，
// 对齐 zcodeTokenFileKey 的读盘策略）。
func mistralTokenFileKey() string {
	mistralTokenCache.Lock()
	defer mistralTokenCache.Unlock()
	now := time.Now()
	if mistralTokenCache.value != "" && now.Sub(mistralTokenCache.at) < zcodeTokenStoreTTL {
		return mistralTokenCache.value
	}
	key := ""
	if path := mistralTokenStorePath(""); path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			var token mistralToken
			if json.Unmarshal(data, &token) == nil {
				key = strings.TrimSpace(token.APIKey)
			}
		}
	}
	mistralTokenCache.at = now
	mistralTokenCache.value = key
	return key
}

// mistralTokenStorePath 解析凭据文件路径（测试可注入 HOME 覆盖）。
func mistralTokenStorePath(home string) string {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = h
	}
	return filepath.Join(home, ".nanorouter", mistralTokenStoreName)
}
