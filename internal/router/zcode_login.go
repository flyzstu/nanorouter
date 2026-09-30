package router

// ZCode Coding Plan 登录链：设备码轮询 → key 兑换 → 网关探测 → 凭据落盘。
//
// 1:1 移植自 dsh-zhipu-provider 的 src/login.ts 与 src/exchange.ts（后者
// 又复制自参考客户端 zcode_coding_plan_client.py 的 OAuth 流程与
// resolve_*_api_key 链）。轮询拿到的是 OAuth access token，不是可服务的
// Coding Plan 凭据——必须先兑换成 id.secret 形式的 key 并通过网关探测，
// 才允许写入凭据文件；没有探测成功的凭据宁可失败也不落盘。
//
// 凭据文件由本命令写入 ~/.nanorouter/zcode_token.json，网关侧经候选的
// zcode_token_file 开关只读回退（见 zcode.go）；参考客户端的
// ~/.zcode_coding_plan_token.json 始终只读。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	json "github.com/goccy/go-json"
)

// 登录链常量（上游合同，与 dsh login.ts/exchange.ts 保持一致）。
const (
	ZcodeLoginAPIPath     = "/api/v1"
	ZcodeLoginTimeout     = 5 * time.Minute
	ZcodeLoginMinInterval = 2 * time.Second
	ZcodeExchangeTimeout  = 15 * time.Second
	ZcodeExchangeKeyName  = "zcode-api-key"
	// zcodeProbeModel 只要求网关接受，答案本身是噪音。
	zcodeProbeModel = "GLM-5.3"
)

// 业务主机是变量而非常量：测试用 httptest 服务器替换（生产代码不写）。
var (
	zcodeBigmodelBizURL = "https://bigmodel.cn"
	zcodeZaiBizURL      = "https://api.z.ai"
)

// ZcodeLoginOptions 是一次登录的可注入缝隙，全部字段可覆盖以便测试。
type ZcodeLoginOptions struct {
	// GatewayOrigin 设备码轮询与探测使用的网关；默认 https://zcode.z.ai。
	GatewayOrigin string
	// Channel 是 Coding Plan 渠道：bigmodel（默认）或 zai。
	Channel string
	// PollTimeout 放弃轮询前的最长等待；默认 5 分钟。
	PollTimeout time.Duration
	// Client 发请求用的 HTTP 客户端；默认带 15s 单跳超时的客户端。
	Client *http.Client
	// Output 接收面向用户的提示文本；默认 os.Stderr。密钥本身只写入
	// 凭据文件，不打印到任何流。
	Output io.Writer
	// TokenStorePath 凭据文件路径；默认 ~/.nanorouter/zcode_token.json。
	// 空串且 HOME 不可用时登录成功但无法落盘，按错误处理。
	TokenStorePath string
	// OpenBrowser 尝试用系统浏览器打开授权页；nil 时跳过。
	OpenBrowser func(url string) error
}

// zcodeLoginHeaders 是 OAuth 端点的基础头（复刻参考客户端）。
func zcodeLoginHeaders(pollToken string) map[string]string {
	return map[string]string{
		"Authorization":       "Bearer " + pollToken,
		"Content-Type":        "application/json",
		"User-Agent":          "ZCode/" + ZcodeClientVersion,
		"X-Title":             zcodeTitle,
		"X-ZCode-App-Version": ZcodeClientVersion,
	}
}

type zcodeDeviceCode struct {
	flowID     string
	pollToken  string
	authURL    string
	intervalMs time.Duration
	expiresAt  time.Time
}

// parseZcodeDeviceCode 解析 /oauth/cli/init 的回答；不可用返回错误。
func parseZcodeDeviceCode(body []byte, origin string) (zcodeDeviceCode, error) {
	var envelope struct {
		Data *struct {
			FlowID       string  `json:"flow_id"`
			AuthorizeURL string  `json:"authorize_url"`
			PollToken    string  `json:"poll_token"`
			PollInterval float64 `json:"poll_interval_sec"`
			ExpiresAt    float64 `json:"expires_at"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Data == nil {
		return zcodeDeviceCode{}, fmt.Errorf("unreadable device-code answer")
	}
	data := envelope.Data
	if data.FlowID == "" || data.AuthorizeURL == "" {
		return zcodeDeviceCode{}, fmt.Errorf("unreadable device-code answer")
	}
	parsed, err := url.Parse(data.AuthorizeURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return zcodeDeviceCode{}, fmt.Errorf("authorize_url is not an http(s) URL")
	}
	// https 是授权页的硬要求；http 仅放行 loopback（测试与本地网关）。
	if parsed.Scheme != "https" && !isLoopbackHost(parsed.Hostname()) {
		return zcodeDeviceCode{}, fmt.Errorf("authorize_url must be https")
	}
	interval := time.Duration(data.PollInterval * float64(time.Second))
	if interval <= 0 {
		interval = ZcodeLoginMinInterval
	}
	if interval < ZcodeLoginMinInterval {
		interval = ZcodeLoginMinInterval
	}
	expiresAt := time.Now().Add(ZcodeLoginTimeout)
	if data.ExpiresAt > 0 {
		expiresAt = time.Unix(int64(data.ExpiresAt), 0)
	}
	return zcodeDeviceCode{
		flowID:     data.FlowID,
		pollToken:  data.PollToken,
		authURL:    parsed.String(),
		intervalMs: interval,
		expiresAt:  expiresAt,
	}, nil
}

type zcodePollDecision struct {
	kind    string // pending | success | expired | denied | error
	token   string // OAuth access token
	user    string
	jwt     string
	message string
}

// parseZcodeDevicePoll 解析一次 /oauth/cli/poll/{flow_id} 的回答。
// 渠道分组优先，其次顶层 accessToken——与 dsh 的 parseDevicePoll 一致。
func parseZcodeDevicePoll(body []byte, channel string) zcodePollDecision {
	var envelope struct {
		Code float64 `json:"code"`
		Msg  string  `json:"msg"`
		Data *struct {
			Status      string         `json:"status"`
			Token       string         `json:"token"`
			AccessToken string         `json:"accessToken"`
			Provider    map[string]any `json:"-"`
			User        struct {
				Name   string `json:"name"`
				Email  string `json:"email"`
				UserID string `json:"user_id"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Data == nil {
		if envelope.Code == 3004 {
			return zcodePollDecision{kind: "expired"}
		}
		message := "unreadable poll answer"
		if envelope.Msg != "" {
			message = "poll failed: " + envelope.Msg
		}
		return zcodePollDecision{kind: "error", message: message}
	}
	data := envelope.Data
	// 3004 也可能出现在 data 内层（信封形状随网关版本漂移），两处都认。
	if data.Status == "" && envelope.Code == 3004 {
		return zcodePollDecision{kind: "expired"}
	}
	switch data.Status {
	case "ready":
		var raw struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(body, &raw)
		token := zcodePollAccessToken(raw.Data, channel)
		if token == "" {
			if data.Status == "ready" {
				return zcodePollDecision{kind: "error", message: "the authorization landed without an access token"}
			}
			return zcodePollDecision{kind: "pending"}
		}
		user := data.User.Name
		if user == "" {
			user = data.User.Email
		}
		if user == "" {
			user = data.User.UserID
		}
		return zcodePollDecision{kind: "success", token: token, user: user, jwt: data.Token}
	case "failed":
		return zcodePollDecision{kind: "denied", message: "the authorization was denied on the verification page"}
	case "pending":
		return zcodePollDecision{kind: "pending"}
	default:
		return zcodePollDecision{kind: "error", message: fmt.Sprintf("device authorization failed: %s", data.Status)}
	}
}

// zcodePollAccessToken 从 data 的渠道分组或顶层提取 OAuth access token。
func zcodePollAccessToken(data map[string]any, channel string) string {
	if data == nil {
		return ""
	}
	for _, source := range []any{data[channel], data} {
		record, ok := source.(map[string]any)
		if !ok {
			continue
		}
		if v, _ := record["access_token"].(string); v != "" {
			return v
		}
		if v, _ := record["accessToken"].(string); v != "" {
			return v
		}
	}
	return ""
}

// zcodeCallJSON 执行一跳业务 API 并解析 JSON 信封。拒绝时携带服务器
// 消息但绝不包含 Authorization 头（那是活体凭据）。
func zcodeCallJSON(ctx context.Context, client *http.Client, what, url string, method string, headers map[string]string, body any) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("%s: payload cannot be encoded: %w", what, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("%s could not be reached: %w", what, err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s could not be reached: %w", what, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s response could not be read: %w", what, err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s failed: HTTP %d%s", what, resp.StatusCode, zcodeEnvelopeMessage(parsed))
	}
	if parsed != nil {
		switch code := parsed["code"].(type) {
		case float64:
			if code != 0 && code != 200 {
				return nil, fmt.Errorf("%s failed%s", what, zcodeEnvelopeMessage(parsed))
			}
		case string:
			if code != "0" && code != "200" {
				return nil, fmt.Errorf("%s failed%s", what, zcodeEnvelopeMessage(parsed))
			}
		}
	}
	if parsed == nil {
		parsed = map[string]any{}
	}
	return parsed, nil
}

func zcodeEnvelopeMessage(envelope map[string]any) string {
	if envelope == nil {
		return ""
	}
	for _, key := range []string{"msg", "message", "error_description"} {
		if v, _ := envelope[key].(string); v != "" {
			return ": " + truncateLogMessage(v)
		}
	}
	return ""
}

// zcodeDataOf 返回信封的 data 对象（无则空表）。
func zcodeDataOf(envelope map[string]any) map[string]any {
	if data, ok := envelope["data"].(map[string]any); ok {
		return data
	}
	return map[string]any{}
}

// exchangeZcodeKey 把 OAuth access token 兑换为渠道的 Coding Plan key。
// 两个渠道共享三步链：取组织/项目 → 找到或创建 zcode-api-key → 读
// secretKey 组成 id.secret；差异只在业务 token 的取得方式。
func exchangeZcodeKey(ctx context.Context, client *http.Client, oauthToken, channel string) (string, error) {
	if strings.TrimSpace(oauthToken) == "" || strings.ContainsAny(oauthToken, "\r\n ") {
		return "", fmt.Errorf("the OAuth access token cannot ride an HTTP header")
	}
	switch channel {
	case ZcodeChannelBigmodel:
		return zcodeExchangeBigmodel(ctx, client, oauthToken)
	case ZcodeChannelZai:
		return zcodeExchangeZai(ctx, client, oauthToken)
	default:
		return "", fmt.Errorf("unsupported channel %q", channel)
	}
}

func zcodeExchangeBigmodel(ctx context.Context, client *http.Client, oauthToken string) (string, error) {
	// BigModel 的业务 API 直接接受 OAuth token，且不带 Bearer 前缀——
	// 参考客户端明确标注这是 load-bearing 的差异。
	headers := map[string]string{
		"Authorization": oauthToken,
		"Content-Type":  "application/json",
		"User-Agent":    "ZCode/" + ZcodeClientVersion,
	}
	origin := zcodeBigmodelBizURL
	return zcodeExchangeChain(ctx, client, origin, headers, "默认机构", "默认项目")
}

func zcodeExchangeZai(ctx context.Context, client *http.Client, oauthToken string) (string, error) {
	origin := zcodeZaiBizURL
	login, err := zcodeCallJSON(ctx, client, "trading the Z.ai business token", origin+"/api/auth/z/login", http.MethodPost,
		map[string]string{"Content-Type": "application/json", "User-Agent": "ZCode/" + ZcodeClientVersion},
		map[string]any{"token": oauthToken})
	if err != nil {
		return "", err
	}
	businessToken, _ := zcodeDataOf(login)["access_token"].(string)
	if businessToken == "" {
		return "", fmt.Errorf("the Z.ai business token exchange returned no access token")
	}
	headers := map[string]string{
		"Authorization": "Bearer " + businessToken,
		"Content-Type":  "application/json",
		"User-Agent":    "ZCode/" + ZcodeClientVersion,
	}
	return zcodeExchangeChain(ctx, client, origin, headers, "", "")
}

// zcodeExchangeChain 执行 getCustomerInfo → api_keys 兑换链。BigModel
// 偏好名为 默认机构/默认项目 的记录（找不到取第一个），Z.ai 只有一组。
func zcodeExchangeChain(ctx context.Context, client *http.Client, origin string, headers map[string]string, preferredOrg, preferredProject string) (string, error) {
	customer, err := zcodeCallJSON(ctx, client, "reading the account", origin+"/api/biz/customer/getCustomerInfo", http.MethodGet, headers, nil)
	if err != nil {
		return "", err
	}
	organizationID, projectID, err := zcodeChooseProject(zcodeDataOf(customer), preferredOrg, preferredProject)
	if err != nil {
		return "", err
	}
	listURL := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys",
		origin, url.PathEscape(organizationID), url.PathEscape(projectID))
	listed, err := zcodeCallJSON(ctx, client, "listing the Coding Plan API keys", listURL, http.MethodGet, headers, nil)
	if err != nil {
		return "", err
	}
	apiKeyID := ""
	foundNamedKey := false
	dataList, _ := listed["data"].([]any)
	for _, raw := range dataList {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := entry["name"].(string); name == ZcodeExchangeKeyName {
			foundNamedKey = true
			apiKeyID, _ = entry["apiKey"].(string)
			break
		}
	}
	if foundNamedKey && apiKeyID == "" {
		// 参考客户端对「找到同名条目但无 id」直接报错；回退创建会产生
		// 重复 key，两份 key 中只有一份的 secret 被轮换管理。
		return "", fmt.Errorf("the existing %s entry carries no id", ZcodeExchangeKeyName)
	}
	if apiKeyID == "" {
		created, err := zcodeCallJSON(ctx, client, "creating the Coding Plan API key", listURL, http.MethodPost, headers,
			map[string]any{"name": ZcodeExchangeKeyName})
		if err != nil {
			return "", err
		}
		apiKeyID, _ = zcodeDataOf(created)["apiKey"].(string)
		if apiKeyID == "" {
			return "", fmt.Errorf("the Coding Plan API key carries no id")
		}
	}
	copied, err := zcodeCallJSON(ctx, client, "reading the Coding Plan API key secret", listURL+"/copy/"+url.PathEscape(apiKeyID), http.MethodGet, headers, nil)
	if err != nil {
		return "", err
	}
	secretKey, _ := zcodeDataOf(copied)["secretKey"].(string)
	key := apiKeyID
	if secretKey != "" {
		key = apiKeyID + "." + secretKey
	}
	// key 的全部用途是 riding 一个 Authorization 头；无法过头的值在这里
	// 就拒绝，消息里不携带 key 本身。
	if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "\r\n ") {
		return "", fmt.Errorf("the exchanged Coding Plan key cannot ride an HTTP header")
	}
	return key, nil
}

func zcodeChooseProject(data map[string]any, preferredOrg, preferredProject string) (string, string, error) {
	orgs, _ := data["organizations"].([]any)
	if len(orgs) == 0 {
		return "", "", fmt.Errorf("the account exposes no organization")
	}
	pick := func(entries []any, key, preferred string) map[string]any {
		if preferred != "" {
			for _, raw := range entries {
				entry, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if name, _ := entry[key].(string); strings.Contains(name, preferred) {
					return entry
				}
			}
		}
		entry, _ := entries[0].(map[string]any)
		return entry
	}
	org := pick(orgs, "organizationName", preferredOrg)
	orgID, _ := org["organizationId"].(string)
	if orgID == "" {
		return "", "", fmt.Errorf("the organization carries no id")
	}
	projects, _ := org["projects"].([]any)
	if len(projects) == 0 {
		return "", "", fmt.Errorf("the organization exposes no project")
	}
	project := pick(projects, "projectName", preferredProject)
	projectID, _ := project["projectId"].(string)
	if projectID == "" {
		return "", "", fmt.Errorf("the project carries no id")
	}
	return orgID, projectID, nil
}

// probeZcodeKey 在落盘前用一次 max_tokens:1 请求确认网关接受该 key。
// 只有明确的 401/403 能证伪 key；网关不可达等暂时性问题不能证伪，
// 视为通过——把瞬时故障误报成"key 无效"比多一次探测更糟。
func probeZcodeKey(ctx context.Context, client *http.Client, gatewayOrigin, channel, key string) error {
	origin := strings.TrimRight(gatewayOrigin, "/")
	body, _ := json.Marshal(map[string]any{
		"model":      zcodeProbeModel,
		"max_tokens": 1,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/"+zcodeGatewayPath(channel), bytes.NewReader(body))
	if err != nil {
		return nil // 请求构造失败不能证伪 key。
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", ZcodeAnthropicVersion)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ZCode/"+ZcodeClientVersion)
	resp, err := client.Do(req)
	if err != nil {
		return nil // 网关不可达不能证伪 key。
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("the gateway refused the exchanged Coding Plan key (身份验证失败)")
	}
	return nil
}

// RunZcodeLogin 执行完整登录链并把凭据写入 token 文件。
// 返回值是落盘路径（成功时），错误携带面向用户的消息（不含凭据）。
func RunZcodeLogin(ctx context.Context, opts ZcodeLoginOptions) (string, error) {
	origin := strings.TrimRight(strings.TrimSpace(opts.GatewayOrigin), "/")
	if origin == "" {
		origin = ZcodeDefaultOrigin
	}
	channel := opts.Channel
	if channel == "" {
		channel = ZcodeChannelBigmodel
	}
	if channel != ZcodeChannelBigmodel && channel != ZcodeChannelZai {
		return "", fmt.Errorf("unsupported channel %q (supported: bigmodel, zai)", channel)
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: ZcodeExchangeTimeout}
	}
	// 轮询不受单跳超时约束（参考实现只有整个 attempt 的 deadline）：
	// 复制 client 并清掉 Timeout，传输复用同一底层池。
	pollClient := *client
	pollClient.Timeout = 0
	out := opts.Output
	if out == nil {
		out = os.Stderr
	}
	say := func(format string, args ...any) {
		_, _ = fmt.Fprintf(out, format+"\n", args...)
	}

	pollToken := make([]byte, 32)
	if _, err := rand.Read(pollToken); err != nil {
		return "", fmt.Errorf("generate poll token failed: %w", err)
	}
	pollTokenHex := hex.EncodeToString(pollToken)

	initBody, _ := json.Marshal(map[string]any{"provider": channel})
	initReq, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+ZcodeLoginAPIPath+"/oauth/cli/init", bytes.NewReader(initBody))
	if err != nil {
		return "", fmt.Errorf("build init request failed: %w", err)
	}
	for name, value := range zcodeLoginHeaders(pollTokenHex) {
		initReq.Header.Set(name, value)
	}
	initResp, err := client.Do(initReq)
	if err != nil {
		return "", fmt.Errorf("the gateway could not be reached: %w", err)
	}
	initRaw, _ := io.ReadAll(io.LimitReader(initResp.Body, 1<<20))
	initResp.Body.Close()
	if initResp.StatusCode < 200 || initResp.StatusCode >= 300 {
		return "", fmt.Errorf("the gateway answered HTTP %d", initResp.StatusCode)
	}
	device, err := parseZcodeDeviceCode(initRaw, origin)
	if err != nil {
		return "", err
	}
	if device.pollToken == "" {
		device.pollToken = pollTokenHex
	}
	// 与 token/key 同规：poll token 也要骑一个 Authorization 头；被上游
	// 污染时立即失败，而不是当作网络错误静默退避到超时。
	if strings.ContainsAny(device.pollToken, "\r\n ") {
		return "", fmt.Errorf("unreadable device-code answer")
	}
	say("Open the authorization page and approve the login (%s channel):", channel)
	say("  %s", device.authURL)
	if authHost, err := url.Parse(device.authURL); err == nil {
		gatewayHost := ""
		if parsed, err := url.Parse(origin); err == nil {
			gatewayHost = parsed.Hostname()
		}
		// 不硬拒（上游 SSO 域名可能合法漂移），但钓鱼页必须足够显眼。
		if gatewayHost != "" && authHost.Hostname() != gatewayHost {
			say("WARNING: the authorization page lives on %q, not the gateway host %q — verify before approving.", authHost.Hostname(), gatewayHost)
		}
	}
	if opts.OpenBrowser != nil {
		if err := opts.OpenBrowser(device.authURL); err != nil {
			say("(could not open a browser automatically: %v)", err)
		}
	}

	timeout := opts.PollTimeout
	if timeout <= 0 {
		timeout = ZcodeLoginTimeout
	}
	deadline := time.Now().Add(timeout)
	if device.expiresAt.Before(deadline) {
		deadline = device.expiresAt
	}
	wait := device.intervalMs
	for {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no approval arrived before the authorization expired")
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("login canceled")
		case <-time.After(wait):
		}
		pollReq, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+ZcodeLoginAPIPath+"/oauth/cli/poll/"+url.PathEscape(device.flowID), nil)
		if err != nil {
			return "", fmt.Errorf("build poll request failed: %w", err)
		}
		pollReq.Header.Set("Authorization", "Bearer "+device.pollToken)
		pollReq.Header.Set("User-Agent", "ZCode/"+ZcodeClientVersion)
		pollResp, err := pollClient.Do(pollReq)
		if err != nil {
			// 一次丢轮询不是判决：退避后继续问。
			wait = maxDuration(wait, ZcodeLoginMinInterval)
			continue
		}
		pollRaw, _ := io.ReadAll(io.LimitReader(pollResp.Body, 1<<20))
		pollResp.Body.Close()
		decision := parseZcodeDevicePoll(pollRaw, channel)
		switch decision.kind {
		case "pending":
			continue
		case "success":
			say("Authorization received; exchanging the OAuth token for a Coding Plan key...")
			key, err := exchangeZcodeKey(ctx, client, decision.token, channel)
			if err != nil {
				return "", err
			}
			if err := probeZcodeKey(ctx, client, origin, channel, key); err != nil {
				return "", err
			}
			path, err := storeZcodeToken(opts.TokenStorePath, channel, key, decision.jwt, decision.token, decision.user)
			if err != nil {
				return "", err
			}
			say("Login complete. Credential stored at %s (channel %s).", path, channel)
			if decision.user != "" {
				say("Account: %s", decision.user)
			}
			return path, nil
		case "expired":
			return "", fmt.Errorf("the authorization expired before approval")
		case "denied":
			return "", fmt.Errorf("%s", decision.message)
		default:
			return "", fmt.Errorf("%s", decision.message)
		}
	}
}

// isLoopbackHost 判断主机名是否为环回地址（http 授权页仅对其放行）。
func isLoopbackHost(host string) bool {
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// storeZcodeToken 把兑换并验证过的凭据写入渠道分组文件（0600）。
// oauth token 与平台 JWT 一并保存：它们是重置卡等平台端点需要的另一半，
// 丢弃它们意味着将来只能重新登录。
func storeZcodeToken(path, channel, key, jwt, oauthToken, user string) (string, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory failed: %w", err)
		}
		path = filepath.Join(home, ".nanorouter", zcodeTokenStoreName)
	}
	document := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &document)
	}
	// 不写 expires_at：poll 不回传 key 的有效期；过期由网关 401 暴露，
	// zcodeKeyFromRecord 的 expires_at 检查只服务于参考客户端的文件。
	entry := map[string]any{
		"api_key":    key,
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	}
	if jwt != "" {
		entry["zcode_jwt_token"] = jwt
	}
	if oauthToken != "" {
		entry["oauth_access_token"] = oauthToken
	}
	if user != "" {
		entry["user"] = user
	}
	document[channel] = entry
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create credential directory failed: %w", err)
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode credential file failed: %w", err)
	}
	// 临时文件 + chmod + rename：写后显式收紧权限（WriteFile 的 mode 只
	// 在创建时生效，松权限的预存文件不会被收紧），且 rename 替换链接
	// 本身、对并发读者原子，不会读到截断 JSON。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
		return "", fmt.Errorf("write credential file failed: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("tighten credential file permissions failed: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("replace credential file failed: %w", err)
	}
	return path, nil
}
