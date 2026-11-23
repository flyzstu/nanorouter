package router

// ZCode 短期项目 Token（PAT）的签发与缓存。
//
// 官方 v4 模型请求不再使用长期 `id.secret`，而是每次用 OAuth/业务 token
// 向 `POST {keyList}/{apiKeyId}/access_tokens` 换短期 PAT（见 plan §3）：
// envelope `code ∈ {200, 0, "200", "0"}`，`data.enable` 缺失视为 true，
// `expiresAt = min(data.expiresAt*1000, startedAt + data.expiresIn*1000)`，
// 提前 120s + 0~60s 抖动刷新；临时故障（network/timeout/429/5xx）且旧 PAT
// 未过期时降级复用旧值并把下次刷新推迟 5s；401/403/404/`enable:false`/畸形
// 立即失效。错误只传播固定码，不带响应体与凭据。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	json "github.com/goccy/go-json"
)

// zcodePATLead 是 PAT 到期前提前刷新的前置量；抖动上限避免多实例同时续期。
const (
	zcodePATRefreshLead   = 120 * time.Second
	zcodePATRefreshJitter = 60 * time.Second
	zcodePATRetryBackoff  = 5 * time.Second
	zcodePATMaxAttempts   = 2
	// zcodePATRetryCap 是 429 重试等待上限：官方超过 1000ms 直接交还上层。
	zcodePATRetryCap   = 1000 * time.Millisecond
	zcodePATRetryFloor = 250 * time.Millisecond
	zcodePATTimeout    = 15 * time.Second
)

// zcodePATLocation 是 PAT 的签发位置：组织 + 项目 + Coding Plan API key。
// DB/文件落盘后请求路径可直接用它续期，不必重走 customerInfo。
type zcodePATLocation struct {
	OrganizationID string
	ProjectID      string
	APIKeyID       string
}

// zcodePATMaterial 是一次签发的完整产物：短期 token 与其位置和到期时刻。
type zcodePATMaterial struct {
	Token     string
	Location  zcodePATLocation
	ExpiresAt time.Time
}

// zcodePATError 只携带官方固定错误码，不带响应体、URL 或凭据。
type zcodePATError struct{ code string }

func (e *zcodePATError) Error() string { return e.code }

func zcodePATErrorCode(err error) string {
	var perr *zcodePATError
	if errors.As(err, &perr) {
		return perr.code
	}
	var transient *zcodePATTransient
	if errors.As(err, &transient) && transient.code != "" {
		return transient.code
	}
	return "project_token_request_failed"
}

// zcodePATTransient 标记可重试的临时故障（network/timeout/429/5xx）。
// code 是官方固定映射（见 projectAccessTokenRequestFailureCode）：
// network → network_error，timeout → timeout，HTTP 状态 → http_<status>。
type zcodePATTransient struct {
	status     int
	retryAfter time.Duration
	hasRetry   bool
	timeout    bool
	network    bool
	code       string
}

func (e *zcodePATTransient) Error() string { return "project_token_transient_failure" }

// zcodePATCall 是 PAT 签发的单飞单元，与用量缓存的 call 同构。
type zcodePATCall struct {
	done      chan struct{}
	token     string
	expiresAt time.Time
	err       error
}

// zcodePATEntry 是一组 scope 下的缓存条目：有效 token、刷新时刻与指纹。
type zcodePATEntry struct {
	token       string
	expiresAt   time.Time
	refreshAt   time.Time
	fingerprint string
	pending     *zcodePATCall
}

// zcodePATCache 是 PAT 的进程内缓存。scope key 与官方一致：
// [origin, family, accountId, org, project, apiKeyId, usageScene, generation]。
type zcodePATCache struct {
	mu      sync.Mutex
	entries map[string]*zcodePATEntry
	// sessions 按 [origin, family] 记住签发用的登录 token 与代次：token
	// 变化意味着重新登录，同 scope 的旧 PAT 不得再用。
	sessions map[string]zcodePATSession
	now      func() time.Time
}

type zcodePATSession struct {
	token      string
	generation string
	count      int
}

func newZcodePATCache() *zcodePATCache {
	return &zcodePATCache{
		entries:  map[string]*zcodePATEntry{},
		sessions: map[string]zcodePATSession{},
		now:      time.Now,
	}
}

// zcodePATScopeKey 拼出缓存键；usageScene 恒为 1（CODING_PLAN）。
func zcodePATScopeKey(origin, family, accountID string, loc zcodePATLocation, generation string) string {
	return strings.Join([]string{origin, family, accountID,
		loc.OrganizationID, loc.ProjectID, loc.APIKeyID, "1", generation}, "\x1f")
}

// resolve 取有效 PAT：未到期且未到刷新点直接返回；否则单飞签发。
// rejectedFingerprint 只废弃匹配的那一代 PAT（迟到的 401 不得废新凭据）。
// 返回的 expiresAt 供调用方落盘，下次重启不必重走签发。
func (c *zcodePATCache) resolve(ctx context.Context, client *http.Client, origin, family, loginToken, accountID string, loc zcodePATLocation, rejectedFingerprint string) (string, time.Time, error) {
	sessionKey := origin + "\x1f" + family
	c.mu.Lock()
	session, ok := c.sessions[sessionKey]
	if !ok || session.token != loginToken {
		session.count++
		session = zcodePATSession{token: loginToken, generation: fmt.Sprintf("g%d", session.count)}
		c.sessions[sessionKey] = session
	}
	key := zcodePATScopeKey(origin, family, accountID, loc, session.generation)
	entry, ok := c.entries[key]
	if !ok {
		entry = &zcodePATEntry{}
		c.entries[key] = entry
	}
	if rejectedFingerprint != "" && entry.fingerprint == rejectedFingerprint {
		entry.token = ""
	}
	now := c.now()
	if entry.token != "" && now.Before(minTime(entry.refreshAt, entry.expiresAt)) {
		token, expiresAt := entry.token, entry.expiresAt
		c.mu.Unlock()
		return token, expiresAt, nil
	}
	if entry.pending != nil {
		call := entry.pending
		c.mu.Unlock()
		select {
		case <-call.done:
			return call.token, call.expiresAt, call.err
		case <-ctx.Done():
			return "", time.Time{}, ctx.Err()
		}
	}
	call := &zcodePATCall{done: make(chan struct{})}
	entry.pending = call
	c.mu.Unlock()

	go func() {
		token, expiresAt, err := zcodeIssuePAT(ctx, client, origin, loginToken, loc)
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.entries[key] != entry {
			call.err = &zcodePATError{code: "project_token_scope_invalidated"}
			close(call.done)
			return
		}
		if err == nil {
			entry.token = token
			entry.expiresAt = expiresAt
			jitter := time.Duration(rand.Int64N(int64(zcodePATRefreshJitter)))
			entry.refreshAt = expiresAt.Add(-zcodePATRefreshLead - jitter)
			entry.fingerprint = zcodePATFingerprint(token)
			call.token, call.expiresAt = token, expiresAt
		} else {
			// 临时故障且旧 PAT 仍有效：降级复用，下次刷新推迟 5s。
			var transient *zcodePATTransient
			if errors.As(err, &transient) && entry.token != "" && now.Before(entry.expiresAt) {
				entry.refreshAt = minTime(now.Add(zcodePATRetryBackoff), entry.expiresAt)
				call.token, call.expiresAt = entry.token, entry.expiresAt
			} else {
				delete(c.entries, key)
				call.err = err
			}
		}
		entry.pending = nil
		close(call.done)
	}()

	select {
	case <-call.done:
		return call.token, call.expiresAt, call.err
	case <-ctx.Done():
		return "", time.Time{}, ctx.Err()
	}
}

// noteRejected 供上游 401 回调：只废弃指纹命中的那一代。
func (c *zcodePATCache) noteRejected(token string) {
	if token == "" {
		return
	}
	fingerprint := zcodePATFingerprint(token)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.entries {
		if entry.fingerprint == fingerprint {
			entry.token = ""
		}
	}
}

// zcodePATFingerprint 是 SHA-256 hex，仅用于比较哪一代被拒绝，
// 不作账号身份、不传上游。
func zcodePATFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// zcodeIssuePAT 向 access_tokens 端点签发一次 PAT，临时故障最多重试一次。
// 429 带超限 retry-after（>1000ms）直接交还上层，不截短等待。
func zcodeIssuePAT(ctx context.Context, client *http.Client, origin, loginToken string, loc zcodePATLocation) (string, time.Time, error) {
	target := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys/%s/access_tokens",
		strings.TrimRight(origin, "/"), loc.OrganizationID, loc.ProjectID, loc.APIKeyID)
	body, _ := json.Marshal(map[string]any{
		"clientType":    "zcode",
		"clientVersion": ZcodeClientVersion,
	})
	headers := map[string]string{
		"Authorization": "Bearer " + loginToken,
		"Content-Type":  "application/json",
	}
	startedAt := time.Now()
	for attempt := 1; ; attempt++ {
		raw, _, retryAfter, err := zcodePATPost(ctx, client, target, headers, body)
		if err == nil {
			return zcodeParsePAT(raw, startedAt)
		}
		var transient *zcodePATTransient
		if !errors.As(err, &transient) {
			return "", time.Time{}, err
		}
		if attempt >= zcodePATMaxAttempts {
			return "", time.Time{}, err
		}
		// 提前刷新已有可用 PAT 时走同一 pending 的降级，不在这里睡。
		delay := zcodePATRetryFloor
		if transient.hasRetry {
			if retryAfter > zcodePATRetryCap {
				return "", time.Time{}, err
			}
			if retryAfter > delay {
				delay = retryAfter
			}
		}
		if headroom := zcodePATRetryCap - delay; headroom > 0 {
			delay += time.Duration(rand.Int64N(int64(headroom)))
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", time.Time{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// zcodePATPost 发一次签发请求，按官方分类错误：network/timeout/429/5xx
// 为临时故障；401/403/404 只带状态码；2xx 非 JSON 与信封码失败为畸形。
func zcodePATPost(ctx context.Context, client *http.Client, target string, headers map[string]string, body []byte) (map[string]any, int, time.Duration, error) {
	timeout := client.Timeout
	if timeout <= 0 {
		timeout = zcodePATTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, 0, 0, &zcodePATError{code: "project_token_request_failed"}
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, 0, ctx.Err()
		}
		if isTimeoutError(err) {
			return nil, 0, 0, &zcodePATTransient{timeout: true, code: "project_token_timeout"}
		}
		return nil, 0, 0, &zcodePATTransient{network: true, code: "project_token_network_error"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, 0, 0, &zcodePATTransient{network: true, code: "project_token_network_error"}
	}
	if resp.StatusCode == http.StatusTooManyRequests || (resp.StatusCode >= 500 && resp.StatusCode < 600) {
		retry := parseRetryAfter(resp.Header.Get("Retry-After"))
		return nil, resp.StatusCode, retry, &zcodePATTransient{
			status: resp.StatusCode, retryAfter: retry, hasRetry: true,
			code: fmt.Sprintf("project_token_http_%d", resp.StatusCode),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, 0, &zcodePATError{code: fmt.Sprintf("project_token_http_%d", resp.StatusCode)}
	}
	var parsed map[string]any
	if json.Unmarshal(raw, &parsed) != nil {
		return nil, resp.StatusCode, 0, &zcodePATError{code: "project_token_invalid_response"}
	}
	return parsed, resp.StatusCode, 0, nil
}

// zcodeParsePAT 解析签发信封：code 须为 200/0（含字符串形），data 缺
// enable 视为 true；enable:false 停用条目；有效期取双源最小值且须是将來。
func zcodeParsePAT(raw map[string]any, startedAt time.Time) (string, time.Time, error) {
	if raw == nil {
		return "", time.Time{}, &zcodePATError{code: "project_token_invalid_response"}
	}
	switch code := raw["code"].(type) {
	case float64:
		if code != 200 && code != 0 {
			return "", time.Time{}, &zcodePATError{code: "project_token_invalid_response"}
		}
	case string:
		if code != "200" && code != "0" {
			return "", time.Time{}, &zcodePATError{code: "project_token_invalid_response"}
		}
	default:
		return "", time.Time{}, &zcodePATError{code: "project_token_invalid_response"}
	}
	data, _ := raw["data"].(map[string]any)
	if data == nil {
		return "", time.Time{}, &zcodePATError{code: "project_token_invalid_response"}
	}
	if enable, present := data["enable"].(bool); present && !enable {
		return "", time.Time{}, &zcodePATError{code: "project_token_disabled"}
	}
	token, _ := data["accessToken"].(string)
	tokenType, _ := data["tokenType"].(string)
	expiresIn, _ := data["expiresIn"].(float64)
	expiresAt, _ := data["expiresAt"].(float64)
	if strings.TrimSpace(token) == "" || tokenType != "Bearer" || expiresIn <= 0 || expiresAt <= 0 {
		return "", time.Time{}, &zcodePATError{code: "project_token_invalid_response"}
	}
	expiry := minTime(
		time.UnixMilli(int64(expiresAt*1000)),
		startedAt.Add(time.Duration(expiresIn*float64(time.Second))),
	)
	if !expiry.After(time.Now()) {
		return "", time.Time{}, &zcodePATError{code: "project_token_invalid_response"}
	}
	return token, expiry, nil
}

// zcodeZaiBusinessLogin 用 OAuth token 换 Z.ai 业务 JWT（PAT 签发的 loginToken）。
func zcodeZaiBusinessLogin(ctx context.Context, client *http.Client, oauthToken string) (string, error) {
	body, _ := json.Marshal(map[string]any{"token": oauthToken})
	raw, _, _, err := zcodePATPost(ctx, client, zcodeZaiBizURL+"/api/auth/z/login",
		map[string]string{"Content-Type": "application/json"}, body)
	if err != nil {
		if code := zcodePATErrorCode(err); code == "project_token_request_failed" {
			return "", fmt.Errorf("the Z.ai business token exchange failed: %w", err)
		}
		return "", fmt.Errorf("the Z.ai business token exchange failed: %s", zcodePATErrorCode(err))
	}
	// 业务登录信封与 PAT 信封不同形：直接取 data.access_token。
	token, _ := zcodeDataOf(raw)["access_token"].(string)
	if token == "" {
		return "", fmt.Errorf("the Z.ai business token exchange returned no access token")
	}
	return token, nil
}

// parseRetryAfter 解析秒数或 HTTP 日期为等待时长。
func parseRetryAfter(value string) time.Duration {
	text := strings.TrimSpace(value)
	if text == "" {
		return 0
	}
	var seconds float64
	if _, err := fmt.Sscanf(text, "%f", &seconds); err == nil && seconds >= 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	if ts, err := time.Parse(time.RFC1123, text); err == nil {
		if d := time.Until(ts); d > 0 {
			return d
		}
	}
	return 0
}

// isTimeoutError 判断是否为超时类网络错误。
func isTimeoutError(err error) bool {
	type timeout interface{ Timeout() bool }
	var te timeout
	return errors.As(err, &te) && te.Timeout()
}

// ===== 请求路径续期 =====
//
// 登录落盘的是短期 PAT 而非长期 key：请求路径在到期前凭位置三元组与
// OAuth token 自助续期，不必重走 customerInfo 与设备码。续期失败但旧
// PAT 未过期时降级用旧值；旧值也过期才返回空，调用方按无凭据处理。

func (r *Router) zcodePATCache() *zcodePATCache {
	r.zcodePATOnce.Do(func() { r.zcodePAT = newZcodePATCache() })
	return r.zcodePAT
}

// cached 返回当前代次下仍新鲜的 PAT（未到刷新点）。命中则请求路径
// 无需先换业务 token，直接复用——zai 的业务登录只发生在真正续期时。
func (c *zcodePATCache) cached(origin, family, accountID string, loc zcodePATLocation) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, ok := c.sessions[origin+"\x1f"+family]
	if !ok {
		return "", false
	}
	entry, ok := c.entries[zcodePATScopeKey(origin, family, accountID, loc, session.generation)]
	if !ok || entry.token == "" {
		return "", false
	}
	if c.now().Before(minTime(entry.refreshAt, entry.expiresAt)) {
		return entry.token, true
	}
	return "", false
}

func zcodePATAccountID(c ZcodeCredential) string {
	if c.UserName != "" {
		return c.UserName
	}
	return c.Channel
}

func zcodeBizOrigin(channel string) string {
	if channel == ZcodeChannelZai {
		return zcodeZaiBizURL
	}
	return zcodeBigmodelBizURL
}

func zcodePATLocationFromCredential(c ZcodeCredential) zcodePATLocation {
	return zcodePATLocation{OrganizationID: c.OrganizationID, ProjectID: c.ProjectID, APIKeyID: c.APIKeyID}
}

// patForStoredCredential 是 DB 行的请求路径入口：PAT 新鲜直接用，
// 否则用存的 OAuth token 续期并写回。
func (r *Router) patForStoredCredential(ctx context.Context, c ZcodeCredential) string {
	if c.APIKeyID == "" || c.OAuthToken == "" {
		// 存量 v3 登录（id.secret）：无位置信息续不了，原样返回，
		// 下次重新登录后自然升级为 PAT。
		return c.APIKey
	}
	channel := c.Channel
	origin := zcodeBizOrigin(channel)
	loc := zcodePATLocationFromCredential(c)
	accountID := zcodePATAccountID(c)
	if token, ok := r.zcodePATCache().cached(origin, channel, accountID, loc); ok {
		return token
	}
	if c.APIKey != "" && time.Now().Before(c.PATExpiresAt.Add(-zcodePATRefreshLead-zcodePATRefreshJitter)) {
		return c.APIKey
	}
	token, expiresAt, err := r.issuePATForCredential(ctx, c, origin, loc, accountID)
	if err != nil {
		if c.APIKey != "" && time.Now().Before(c.PATExpiresAt) {
			return c.APIKey
		}
		return ""
	}
	if token != c.APIKey {
		c.APIKey = token
		c.PATExpiresAt = expiresAt
		_ = r.zcodeCredentials.save(ctx, c)
	}
	return token
}

// issuePATForCredential 用存的 OAuth token 换新 PAT：zai 先换业务 JWT。
func (r *Router) issuePATForCredential(ctx context.Context, c ZcodeCredential, origin string, loc zcodePATLocation, accountID string) (string, time.Time, error) {
	loginToken := c.OAuthToken
	if c.Channel == ZcodeChannelZai {
		business, err := zcodeZaiBusinessLogin(ctx, r.client, c.OAuthToken)
		if err != nil {
			return "", time.Time{}, err
		}
		loginToken = business
	}
	return r.zcodePATCache().resolve(ctx, r.client, origin, c.Channel, loginToken, accountID, loc, "")
}

// noteZcodePATRejected 供上游 401/403 回调：只废弃指纹命中的那一代 PAT，
// 下一请求用存的 OAuth token 重签。迟到的 401 不得废弃已续好的新 PAT。
func (r *Router) noteZcodePATRejected(credential string) {
	r.zcodePATCache().noteRejected(credential)
}

// zcodeFilePATRecord 是一条凭据文件记录的 PAT 相关字段。
type zcodeFilePATRecord struct {
	Key          string
	OAuthToken   string
	Location     zcodePATLocation
	PATExpiresAt time.Time
}

// parseZcodeFilePATRecord 从文件记录提取 PAT 续期所需字段；缺位置信息
// 时 Location.APIKeyID 为空，调用方按存量 key 原样返回。
func parseZcodeFilePATRecord(value any) zcodeFilePATRecord {
	record, _ := value.(map[string]any)
	if record == nil {
		return zcodeFilePATRecord{}
	}
	var rec zcodeFilePATRecord
	rec.Key, _ = record["api_key"].(string)
	if rec.Key == "" {
		rec.Key, _ = record["access_token"].(string)
	}
	rec.OAuthToken, _ = record["oauth_access_token"].(string)
	rec.Location.APIKeyID, _ = record["api_key_id"].(string)
	rec.Location.OrganizationID, _ = record["organization_id"].(string)
	rec.Location.ProjectID, _ = record["project_id"].(string)
	if text, _ := record["pat_expires_at"].(string); text != "" {
		rec.PATExpiresAt = parseCredentialTime(text)
	}
	return rec
}

// patForFileRecord 是凭据文件（login zcode 写入的分组文件）的请求路径入口。
// 找到分组记录后走与 DB 行相同的续期逻辑，续好写回文件。
func (r *Router) patForFileRecord(ctx context.Context, channel string) string {
	if channel != ZcodeChannelZai {
		channel = ZcodeChannelBigmodel
	}
	path := ZcodeTokenStorePath()
	if path == "" {
		return zcodeTokenFileKey(channel)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return zcodeTokenFileKey(channel)
	}
	var document map[string]any
	if json.Unmarshal(raw, &document) != nil || document == nil {
		return zcodeTokenFileKey(channel)
	}
	rec := parseZcodeFilePATRecord(document[channel])
	if rec.Location.APIKeyID == "" || rec.OAuthToken == "" {
		return zcodeTokenFileKey(channel)
	}
	origin := zcodeBizOrigin(channel)
	accountID := channel
	if user, _ := document[channel].(map[string]any)["user"].(string); user != "" {
		accountID = user
	}
	if token, ok := r.zcodePATCache().cached(origin, channel, accountID, rec.Location); ok {
		return token
	}
	if rec.Key != "" && time.Now().Before(rec.PATExpiresAt.Add(-zcodePATRefreshLead-zcodePATRefreshJitter)) {
		return rec.Key
	}
	loginToken := rec.OAuthToken
	if channel == ZcodeChannelZai {
		business, err := zcodeZaiBusinessLogin(ctx, r.client, rec.OAuthToken)
		if err != nil {
			if rec.Key != "" && time.Now().Before(rec.PATExpiresAt) {
				return rec.Key
			}
			return ""
		}
		loginToken = business
	}
	token, expiresAt, err := r.zcodePATCache().resolve(ctx, r.client, origin, channel, loginToken, accountID, rec.Location, "")
	if err != nil {
		if rec.Key != "" && time.Now().Before(rec.PATExpiresAt) {
			return rec.Key
		}
		return ""
	}
	if token != rec.Key {
		rewriteZcodeFilePAT(path, channel, token, expiresAt)
	}
	return token
}

// rewriteZcodeFilePAT 把续好的 PAT 写回凭据文件分组记录；写坏不影响
// 本次请求（内存里已有新 token），下次续期再写。
func rewriteZcodeFilePAT(path, channel, token string, expiresAt time.Time) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var document map[string]any
	if json.Unmarshal(raw, &document) != nil || document == nil {
		return
	}
	record, _ := document[channel].(map[string]any)
	if record == nil {
		return
	}
	record["api_key"] = token
	record["pat_expires_at"] = expiresAt.UTC().Format(time.RFC3339Nano)
	record["updated_at"] = time.Now().UTC().Format(time.RFC3339)
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
		return
	}
	_ = os.Chmod(tmp, 0o600)
	_ = os.Rename(tmp, path)
	zcodeTokenCache.Lock()
	zcodeTokenCache.value = nil
	zcodeTokenCache.Unlock()
}
