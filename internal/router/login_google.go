// Package router 的 Google OAuth 登录流程，参考 oh-my-pi 实现。
//
// 流程：
// 1. 启动 localhost callback 服务器（默认端口 51121）
// 2. 打开浏览器引导用户授权（或手动复制 URL）
// 3. 接收 callback 中的 authorization code（或用户粘贴 redirect URL）
// 4. 用 code 换取 access token 和 refresh token
// 5. 保存到 ~/.nanorouter/google_token.json

package router

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// Google OAuth 端点
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"

	// Antigravity OAuth client credentials（来自 oh-my-pi）
	googleClientID     = "10710060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	googleClientSecret = "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"

	// 默认 scopes，与 Antigravity 一致
	googleDefaultScope = "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile https://www.googleapis.com/auth/cclog https://www.googleapis.com/auth/experimentsandconfigs https://www.googleapis.com/auth/aicode"

	// 凭据文件路径
	googleTokenStoreName = "google_token.json"

	// callback 服务器端口（固定，确保 redirect_uri 一致）
	GoogleCallbackDefaultPort = 51121

	// 默认超时
	GoogleLoginTimeout = 5 * time.Minute
)

// GoogleLoginOptions 配置 Google OAuth 登录。
type GoogleLoginOptions struct {
	// TokenStorePath 覆盖凭据存储路径（默认 ~/.nanorouter/google_token.json）。
	TokenStorePath string
	// Scope 覆盖 OAuth scope（默认使用 Antigravity 的完整 scope 列表）。
	Scope string
	// CallbackPort 指定 callback 服务器端口（默认 51121）。
	CallbackPort int
	// PollTimeout 等待用户授权的超时时间（默认 5 分钟）。
	PollTimeout time.Duration
	// OpenBrowser 打开浏览器的函数；nil 表示不自动打开。
	OpenBrowser func(rawURL string) error
	// Output 用于打印进度信息（默认 os.Stderr）。
	Output io.Writer
	// Input 用于读取用户粘贴的 redirect URL（默认 os.Stdin）。
	Input io.Reader
	// RedirectURL 直接使用提供的 redirect URL（包含 code 参数）。
	// 用于脚本化或自动化的场景。
	RedirectURL string
}

// GoogleToken 存储的凭据格式。
type GoogleToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
	Email        string `json:"email,omitempty"`
	ProjectID    string `json:"project_id,omitempty"` // Cloud Code Assist 项目 ID
	UpdatedAt    string `json:"updated_at"`
}

// RunGoogleLogin 执行 Google OAuth 登录流程。
func RunGoogleLogin(ctx context.Context, opts GoogleLoginOptions) (string, error) {
	if opts.PollTimeout == 0 {
		opts.PollTimeout = GoogleLoginTimeout
	}
	if opts.Scope == "" {
		opts.Scope = googleDefaultScope
	}
	if opts.CallbackPort == 0 {
		opts.CallbackPort = GoogleCallbackDefaultPort
	}
	output := opts.Output
	if output == nil {
		output = os.Stderr
	}
	input := opts.Input
	if input == nil {
		input = os.Stdin
	}

	say := func(format string, args ...any) {
		fmt.Fprintf(output, "login google: "+format+"\n", args...)
	}

	// 固定 callback URL，确保授权和兑换使用同一个 redirect_uri
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d/oauth-callback", opts.CallbackPort)

	// 如果直接提供了 redirect URL，跳过服务器和浏览器
	if opts.RedirectURL != "" {
		return exchangeRedirectURL(ctx, opts, callbackURL, say)
	}

	// 启动 callback 服务器
	codeChan := make(chan string, 1)
	errChan := make(chan error, 1)

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", opts.CallbackPort))
	if err != nil {
		return "", fmt.Errorf("start callback server failed: port %d already in use, try --port or kill existing process", opts.CallbackPort)
	}

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/oauth-callback" {
				http.NotFound(w, r)
				return
			}
			code := r.URL.Query().Get("code")
			if code == "" {
				errMsg := r.URL.Query().Get("error")
				if errMsg == "" {
					errMsg = "no code in callback"
				}
				http.Error(w, errMsg, http.StatusBadRequest)
				errChan <- fmt.Errorf("%s", errMsg)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<!DOCTYPE html>
<html>
<head><title>Login Successful</title></head>
<body style="font-family: sans-serif; text-align: center; padding: 50px;">
<h1>✓ Login Successful</h1>
<p>You can close this window and return to the terminal.</p>
</body>
</html>`)
			codeChan <- code
		}),
	}

	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- err
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	// 构建授权 URL
	authURL := buildGoogleAuthURL(callbackURL)

	say("Open this URL in a browser to authorize:")
	say("")
	say("  %s", authURL)
	say("")
	say("After authorization, the browser will redirect to a URL like:")
	say("  %s?code=...", callbackURL)
	say("If the page shows an error, that's expected on a remote server.")
	say("Copy the full URL from the browser's address bar and paste it below.")
	say("")

	// 尝试打开浏览器
	if opts.OpenBrowser != nil {
		if err := opts.OpenBrowser(authURL); err != nil {
			say("Failed to open browser automatically: %v", err)
		}
	}

	// 同时等待 callback 和用户粘贴
	code := ""
	received := make(chan string, 2)

	go func() {
		select {
		case c := <-codeChan:
			received <- c
		case err := <-errChan:
			say("Callback error: %v", err)
			received <- ""
		}
	}()

	go func() {
		reader := bufio.NewReader(input)
		for {
			fmt.Fprint(output, "Paste the redirect URL here (or wait for browser callback): ")
			line, err := reader.ReadString('\n')
			if err != nil {
				received <- ""
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			// 解析 URL 中的 code 参数
			if u, parseErr := url.Parse(line); parseErr == nil {
				if c := u.Query().Get("code"); c != "" {
					received <- c
					return
				}
			}
			// 直接就是 code
			if len(line) > 10 {
				received <- line
				return
			}
			say("Invalid input. Please paste the full redirect URL from the browser address bar.")
		}
	}()

	timeoutCtx, cancel := context.WithTimeout(ctx, opts.PollTimeout)
	defer cancel()

	select {
	case c := <-received:
		code = c
	case <-timeoutCtx.Done():
		return "", fmt.Errorf("timeout waiting for authorization")
	}

	if code == "" {
		return "", fmt.Errorf("no authorization code received")
	}

	return exchangeCodeForToken(ctx, opts, code, callbackURL, say)
}

// exchangeRedirectURL 从提供的 redirect URL 兑换 token。
func exchangeRedirectURL(ctx context.Context, opts GoogleLoginOptions, callbackURL string, say func(string, ...any)) (string, error) {
	u, err := url.Parse(opts.RedirectURL)
	if err != nil {
		return "", fmt.Errorf("parse redirect URL failed: %w", err)
	}
	code := u.Query().Get("code")
	if code == "" {
		return "", fmt.Errorf("no code found in redirect URL")
	}
	return exchangeCodeForToken(ctx, opts, code, callbackURL, say)
}

// exchangeCodeForToken 用 authorization code 换取 token 并保存。
func exchangeCodeForToken(ctx context.Context, opts GoogleLoginOptions, code, callbackURL string, say func(string, ...any)) (string, error) {
	say("Exchanging authorization code for tokens...")

	token, err := exchangeGoogleCode(ctx, code, callbackURL)
	if err != nil {
		return "", fmt.Errorf("exchange code failed: %w", err)
	}

	// 获取用户邮箱（可选，失败不阻塞登录）
	email := fetchGoogleUserInfo(ctx, token.AccessToken)
	if email != "" {
		token.Email = email
		say("Account: %s", email)
	}

	// 发现 Cloud Code Assist 项目
	say("Discovering Cloud Code Assist project...")
	projectID, err := discoverGoogleProject(ctx, token.AccessToken)
	if err != nil {
		say("Warning: project discovery failed: %v", err)
		say("You may need to complete onboarding in the official Antigravity CLI first")
	} else if projectID != "" {
		token.ProjectID = projectID
		say("Project: %s", projectID)
	}

	path, err := storeGoogleToken(opts.TokenStorePath, token)
	if err != nil {
		return "", fmt.Errorf("store token failed: %w", err)
	}

	say("Login complete. Credential stored at %s", path)
	return path, nil
}

// buildGoogleAuthURL 构建 Google OAuth 授权 URL。
func buildGoogleAuthURL(callbackURL string) string {
	params := url.Values{}
	params.Set("client_id", googleClientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", callbackURL)
	params.Set("scope", googleDefaultScope)
	params.Set("access_type", "offline")
	params.Set("prompt", "consent")

	state := make([]byte, 16)
	rand.Read(state)
	params.Set("state", fmt.Sprintf("%x", state))

	return googleAuthURL + "?" + params.Encode()
}

// exchangeGoogleCode 用 authorization code 换取 tokens。
func exchangeGoogleCode(ctx context.Context, code, callbackURL string) (*GoogleToken, error) {
	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("code", code)
	data.Set("client_id", googleClientID)
	data.Set("client_secret", googleClientSecret)
	data.Set("redirect_uri", callbackURL)

	req, err := http.NewRequestWithContext(ctx, "POST", googleTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed: %s %s", resp.Status, string(body))
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	if result.AccessToken == "" {
		return nil, fmt.Errorf("no access_token in response")
	}
	if result.RefreshToken == "" {
		return nil, fmt.Errorf("no refresh_token in response")
	}

	return &GoogleToken{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(result.ExpiresIn) * time.Second).Unix(),
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// refreshGoogleToken 用 refresh_token 换取新的 access token 并写回凭据文件。
// Google 刷新响应通常不带 refresh_token，原值保留；Email/ProjectID 不动。
func refreshGoogleToken(ctx context.Context, token *GoogleToken) (*GoogleToken, error) {
	if token.RefreshToken == "" {
		return nil, errors.New("no refresh_token available")
	}

	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("refresh_token", token.RefreshToken)
	data.Set("client_id", googleClientID)
	data.Set("client_secret", googleClientSecret)

	req, err := http.NewRequestWithContext(ctx, "POST", googleTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token refresh failed: %s %s", resp.Status, string(body))
	}

	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	if result.AccessToken == "" {
		return nil, errors.New("no access_token in refresh response")
	}

	updated := *token
	updated.AccessToken = result.AccessToken
	updated.ExpiresAt = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second).Unix()
	updated.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	if _, err := storeGoogleToken("", &updated); err != nil {
		return nil, fmt.Errorf("persist refreshed token: %w", err)
	}
	return &updated, nil
}

// fetchGoogleUserInfo 获取用户邮箱（可选，失败不阻塞登录）。
func fetchGoogleUserInfo(ctx context.Context, accessToken string) string {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.googleapis.com/oauth2/v1/userinfo?alt=json", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var result struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}
	return result.Email
}

// discoverGoogleProject 发现 Cloud Code Assist 项目 ID。
// 参考 oh-my-pi 的 google-antigravity.ts 实现。
func discoverGoogleProject(ctx context.Context, accessToken string) (string, error) {
	endpoint := "https://daily-cloudcode-pa.googleapis.com"
	url := endpoint + "/v1internal:loadCodeAssist"

	body := map[string]any{
		"metadata": map[string]string{
			"ideType": "ANTIGRAVITY",
		},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("loadCodeAssist failed: %s", resp.Status)
	}

	var result struct {
		CloudaicompanionProject string `json:"cloudaicompanionProject"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return result.CloudaicompanionProject, nil
}

// storeGoogleToken 保存凭据到文件（0600 权限）。
func storeGoogleToken(path string, token *GoogleToken) (string, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory failed: %w", err)
		}
		path = filepath.Join(home, ".nanorouter", googleTokenStoreName)
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
