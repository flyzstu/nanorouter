package router

// ZCode Coding Plan 额度重置卡：查询余量与手动核销。
//
// 合同对齐 ZCode 客户端 v3.15.1：
//
//	GET  /api/v1/coding-plan/reset/status
//	POST /api/v1/coding-plan/reset/use
//
// 认证不是 Coding Plan API key，而是平台凭据对：
//
//	Authorization: Bearer <zcode JWT>
//	X-Bigmodel-Authorization: <MaaS OAuth token，直传不套 Bearer>
//	Bigmodel-Target-Type: PERSONAL
//
// 两个 token 在 login zcode 时已随 api_key 一起落库/落盘（zcode_login.go 的
// storeZcodeToken），这里只是把之前“留着将来用”的字段接上，不需要新的登录
// 流程。核销是破坏性操作：只由显式 POST 触发，网关的任何自动路径（刷新、
// 登录、错误恢复）都不会调用它；且默认关闭，需要在配置里显式打开。

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	json "github.com/goccy/go-json"
	"go.uber.org/zap"
)

const (
	zcodeResetBasePath = "/api/v1/coding-plan/reset"
	// 单次上游调用超时。重置接口是人工触发的低频操作，等待一次往返是值得的。
	zcodeResetTimeout = 15 * time.Second
	// 上游响应体上限；status/use 都是小信封。
	zcodeResetMaxBodyBytes = 1 << 20
	// status 缓存窗口。统计页开启自动刷新时每 10 秒拉一次，上游对四接口有
	// 限频记录（实测 429），30 秒缓存把轮询压到可接受频率。
	zcodeResetStatusTTL = 30 * time.Second
	// idempotency_key 的长度上限（上游合同）。
	zcodeResetMaxIdempotencyKey = 64
	zcodeResetTypeFiveHour      = "FIVE_HOUR"
	zcodeResetTypeWeek          = "WEEK"
)

type zcodeResetOpportunity struct {
	ExpireAt int64 `json:"expire_at"`
}

type zcodeResetHistory struct {
	UsedAt int64 `json:"used_at"`
}

// zcodeResetStatus 是上游 status 的 data 部分，字段名保持 snake_case 以
// 原样透传给页面（与 ZCode 客户端契约一致）。
type zcodeResetStatus struct {
	AvailableFiveHourResets    []zcodeResetOpportunity `json:"available_five_hour_resets"`
	AvailableWeekResets        []zcodeResetOpportunity `json:"available_week_resets"`
	LatestFiveHourResetHistory *zcodeResetHistory      `json:"latest_five_hour_reset_history"`
	LatestWeekResetHistory     *zcodeResetHistory      `json:"latest_week_reset_history"`
	HasUnreadHistory           bool                    `json:"has_unread_history"`
}

type zcodeResetUseData struct {
	Used bool `json:"used"`
}

// zcodeResetCredential 是 reset 端点的一对凭据。
type zcodeResetCredential struct {
	Channel     string
	ZcodeJWT    string
	BusinessJWT string
}

// zcodeResetCredentialError 表示本地缺少 reset 凭据。缺失是状态而不是错误码：
// 对操作者来说需要的是“重新 login zcode”，不是重试。
type zcodeResetCredentialError struct {
	detail string
}

func (e *zcodeResetCredentialError) Error() string { return e.detail }

// zcodeResetAPIError 是上游信封或 HTTP 层的失败。Status 为 0 表示业务信封
// 在 HTTP 2xx 下返回了非 0 code。
type zcodeResetAPIError struct {
	Status    int
	Code      int
	Message   string
	RequestID string
}

func (e *zcodeResetAPIError) Error() string {
	code := fmt.Sprintf("zcode_reset_api_error:%d", e.Code)
	if e.Code == 0 {
		code = fmt.Sprintf("zcode_reset_http_error:%d", e.Status)
	}
	if e.RequestID != "" {
		return fmt.Sprintf("%s %s (x-request-id:%s)", code, e.Message, e.RequestID)
	}
	if e.Message != "" {
		return code + " " + e.Message
	}
	return code
}

type zcodeResetProtocolError struct {
	message string
}

func (e *zcodeResetProtocolError) Error() string { return e.message }

/* -------------------------------------------------------------------------- */
/* 凭据解析：数据库 → 三个凭据文件                                               */
/* -------------------------------------------------------------------------- */

// zcodeResetTokenSet 是一份渠道凭据的两个 token；空串表示该来源没有提供。
type zcodeResetTokenSet struct {
	ZcodeJWT    string
	BusinessJWT string
}

func (s zcodeResetTokenSet) complete() bool { return s.ZcodeJWT != "" && s.BusinessJWT != "" }

// zcodeResetCredential 解析指定渠道的 reset 凭据。requested 为空时按主 ZCode
// 候选渠道优先、另一渠道兜底的顺序自动选择——业务 JWT 按 family 签发，不能
// 跨渠道拼接；显式指定渠道时绝不回退。
func (r *Router) zcodeResetCredential(ctx context.Context, requested string) (zcodeResetCredential, error) {
	channels := make([]string, 0, 2)
	if requested != "" {
		channels = append(channels, requested)
	} else {
		primary := r.primaryZcodeChannel()
		if primary != ZcodeChannelZai {
			primary = ZcodeChannelBigmodel
		}
		other := ZcodeChannelZai
		if primary == ZcodeChannelZai {
			other = ZcodeChannelBigmodel
		}
		channels = append(channels, primary, other)
	}

	missing := make([]string, 0, len(channels))
	for _, channel := range channels {
		set := r.zcodeResetTokenSet(ctx, channel)
		if set.complete() {
			return zcodeResetCredential{
				Channel:     channel,
				ZcodeJWT:    normalizeZcodeBearer(set.ZcodeJWT),
				BusinessJWT: stripZcodeBearer(set.BusinessJWT),
			}, nil
		}
		missing = append(missing, fmt.Sprintf("%s 缺少 %s", channel, strings.Join(set.missingFields(), " / ")))
	}
	return zcodeResetCredential{}, &zcodeResetCredentialError{
		detail: "没有可用的 ZCode 重置卡凭据（" + strings.Join(missing, "；") + "），请先运行 login zcode",
	}
}

func (s zcodeResetTokenSet) missingFields() []string {
	fields := make([]string, 0, 2)
	if s.ZcodeJWT == "" {
		fields = append(fields, "平台 JWT")
	}
	if s.BusinessJWT == "" {
		fields = append(fields, "MaaS OAuth token")
	}
	return fields
}

// zcodeResetTokenSet 按「数据库 → login 文件 → ZCode 官方 v2 文件 → 参考
// 客户端文件」收集两个 token。数据库优先与请求路径一致：容器内文件只读，
// 刷新后的 token 只能写库。
//
// 优先返回单一来源里的完整 token 对：两个 token 必须属于同一次登录，按字段
// 跨来源拼接在切换账号后会把旧 JWT 和新业务 token 配成一对。只有所有来源都
// 不完整时才逐字段合并（例如数据库只存到 JWT、业务 token 还在文件里）。
func (r *Router) zcodeResetTokenSet(ctx context.Context, channel string) zcodeResetTokenSet {
	sources := make([]zcodeResetTokenSet, 0, 4)
	if r.zcodeCredentials != nil {
		if credential, ok := r.zcodeCredentials.tokensForChannel(ctx, channel); ok {
			sources = append(sources, zcodeResetTokenSet{
				ZcodeJWT:    strings.TrimSpace(credential.PlatformJWT),
				BusinessJWT: strings.TrimSpace(credential.OAuthToken),
			})
		}
	}
	for _, path := range []string{ZcodeTokenStorePath(), ZcodeV2CredentialsPath(), ZcodeReferenceTokenPath()} {
		if path == "" {
			continue
		}
		sources = append(sources, parseZcodeResetTokenFile(path, channel))
	}
	for _, set := range sources {
		if set.complete() {
			return set
		}
	}
	var merged zcodeResetTokenSet
	for _, set := range sources {
		if merged.ZcodeJWT == "" {
			merged.ZcodeJWT = set.ZcodeJWT
		}
		if merged.BusinessJWT == "" {
			merged.BusinessJWT = set.BusinessJWT
		}
	}
	return merged
}

// parseZcodeResetTokenFile 从一个凭据文件提取指定渠道的两个 token。文件缺失、
// 损坏或字段缺失都返回空值：凭据链还有其他来源，坏文件不能拖垮端点。
//
// 支持三种真实形状：
//   - login 文件 / 参考客户端：按渠道分组 {bigmodel:{zcode_jwt_token,oauth_access_token}}
//     或带 provider 字段的平铺记录；
//   - ZCode 官方 v2 文件：顶层 zcodejwttoken + oauth:<family>:access_token
//     （值可能被 AES-GCM 加密）。
//
// 业务 token 只认与渠道同 family 的字段，禁止跨 family 回退：Z.ai 与 BigModel
// 的 MaaS 身份不同，混用会被上游拒绝，也会把 A 渠道的活体凭据发给 B 渠道。
func parseZcodeResetTokenFile(path, channel string) zcodeResetTokenSet {
	raw, err := os.ReadFile(path)
	if err != nil {
		return zcodeResetTokenSet{}
	}
	var document map[string]any
	if json.Unmarshal(raw, &document) != nil || document == nil {
		return zcodeResetTokenSet{}
	}
	return parseZcodeResetTokenDocument(document, channel)
}

func parseZcodeResetTokenDocument(document map[string]any, channel string) zcodeResetTokenSet {
	if document == nil {
		return zcodeResetTokenSet{}
	}
	channel = zcodeResetFamilyChannel(channel)
	var set zcodeResetTokenSet

	readJWT := func(source map[string]any) {
		if set.ZcodeJWT != "" || source == nil {
			return
		}
		for _, key := range []string{"zcode_jwt_token", "zcodejwttoken", "zcodeJwtToken", "zcode_jwt"} {
			if token := decryptZcodeResetToken(source[key]); token != "" {
				set.ZcodeJWT = token
				return
			}
		}
	}
	readBusiness := func(source map[string]any) {
		if set.BusinessJWT != "" || source == nil {
			return
		}
		for _, key := range []string{"oauth_access_token", "oauthAccessToken"} {
			if token := decryptZcodeResetToken(source[key]); token != "" {
				set.BusinessJWT = token
				return
			}
		}
	}

	group, hasGroup := document[channel].(map[string]any)
	if hasGroup {
		readJWT(group)
		readBusiness(group)
	}

	provider, _ := document["provider"].(string)
	hasGroups := document[ZcodeChannelBigmodel] != nil || document[ZcodeChannelZai] != nil
	// 平铺记录只有在能确定 family 时才接受业务 token：要么 provider 明写，
	// 要么整份文件只有一套账户（没有渠道分组）。
	if set.BusinessJWT == "" && (provider == channel || (provider == "" && !hasGroups)) {
		readBusiness(document)
	}
	// v2 文件的 family 键是显式的，任何形状都安全。
	if set.BusinessJWT == "" {
		set.BusinessJWT = decryptZcodeResetToken(document["oauth:"+channel+":access_token"])
	}
	// 平台 JWT 属于账号而非 family：本渠道没有时允许从顶层或另一组回退。
	if set.ZcodeJWT == "" {
		readJWT(document)
	}
	if set.ZcodeJWT == "" {
		for _, other := range []string{ZcodeChannelBigmodel, ZcodeChannelZai} {
			if other == channel {
				continue
			}
			if candidate, ok := document[other].(map[string]any); ok {
				readJWT(candidate)
				if set.ZcodeJWT != "" {
					break
				}
			}
		}
	}
	return set
}

func decryptZcodeResetToken(value any) string {
	token, ok := value.(string)
	if !ok || strings.TrimSpace(token) == "" {
		return ""
	}
	decoded, ok := decryptZcodeCredentialValue(token)
	if !ok {
		return ""
	}
	return strings.TrimSpace(decoded)
}

func zcodeResetFamilyChannel(channel string) string {
	if strings.ToLower(strings.TrimSpace(channel)) == ZcodeChannelZai {
		return ZcodeChannelZai
	}
	return ZcodeChannelBigmodel
}

func normalizeZcodeBearer(token string) string {
	token = strings.TrimSpace(token)
	if len(token) >= 7 && strings.EqualFold(token[:7], "bearer ") {
		return token
	}
	return "Bearer " + token
}

func stripZcodeBearer(token string) string {
	token = strings.TrimSpace(token)
	if len(token) >= 7 && strings.EqualFold(token[:7], "bearer ") {
		return strings.TrimSpace(token[7:])
	}
	return token
}

/* -------------------------------------------------------------------------- */
/* 上游调用与缓存                                                              */
/* -------------------------------------------------------------------------- */

type zcodeResetCacheEntry struct {
	status    *zcodeResetStatus
	fetchedAt time.Time
	err       error
}

// zcodeResetStatusCache 是 status 的进程内缓存。只缓存 status：use 每次都
// 必须真实上行。失败同样缓存一个窗口，避免故障中的上游被页面自动刷新轰炸。
type zcodeResetStatusCache struct {
	mu      sync.Mutex
	entries map[string]zcodeResetCacheEntry
	client  *http.Client
	now     func() time.Time
	// baseURL 默认 zcodeGatewayOrigin()；测试指向 httptest 服务器。
	baseURL string
}

func newZcodeResetStatusCache() *zcodeResetStatusCache {
	return &zcodeResetStatusCache{
		entries: map[string]zcodeResetCacheEntry{},
		client:  newUpstreamHTTPClient(),
		now:     time.Now,
	}
}

func (c *zcodeResetStatusCache) origin() string {
	if c.baseURL != "" {
		return strings.TrimRight(c.baseURL, "/")
	}
	return strings.TrimRight(zcodeGatewayOrigin(), "/")
}

func (r *Router) zcodeResetShared() *zcodeResetStatusCache {
	r.zcodeResetOnce.Do(func() { r.zcodeReset = newZcodeResetStatusCache() })
	return r.zcodeReset
}

func (c *zcodeResetStatusCache) invalidate(channel string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, channel)
}

// fetchStatus 返回 (状态, 抓取时刻, 错误)。refresh=true 跳过缓存。
func (c *zcodeResetStatusCache) fetchStatus(ctx context.Context, credential zcodeResetCredential, refresh bool) (*zcodeResetStatus, time.Time, error) {
	now := c.now()
	if !refresh {
		c.mu.Lock()
		entry, ok := c.entries[credential.Channel]
		c.mu.Unlock()
		if ok && now.Sub(entry.fetchedAt) < zcodeResetStatusTTL {
			return entry.status, entry.fetchedAt, entry.err
		}
	}

	status, err := c.requestStatus(ctx, credential)
	fetchedAt := c.now()
	c.mu.Lock()
	c.entries[credential.Channel] = zcodeResetCacheEntry{status: status, fetchedAt: fetchedAt, err: err}
	c.mu.Unlock()
	return status, fetchedAt, err
}

func (c *zcodeResetStatusCache) requestStatus(ctx context.Context, credential zcodeResetCredential) (*zcodeResetStatus, error) {
	data, err := zcodeResetCall(ctx, c.client, c.origin(), credential, http.MethodGet, "status", nil)
	if err != nil {
		return nil, err
	}
	var status zcodeResetStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return nil, &zcodeResetProtocolError{message: "zcode reset status 返回非预期结构: " + err.Error()}
	}
	if status.AvailableFiveHourResets == nil {
		status.AvailableFiveHourResets = []zcodeResetOpportunity{}
	}
	if status.AvailableWeekResets == nil {
		status.AvailableWeekResets = []zcodeResetOpportunity{}
	}
	return &status, nil
}

func (c *zcodeResetStatusCache) use(ctx context.Context, credential zcodeResetCredential, resetType, idempotencyKey string) (*zcodeResetUseData, error) {
	body := map[string]any{
		"idempotency_key": idempotencyKey,
		"reset_type":      resetType,
	}
	data, err := zcodeResetCall(ctx, c.client, c.origin(), credential, http.MethodPost, "use", body)
	if err != nil {
		return nil, err
	}
	var result zcodeResetUseData
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, &zcodeResetProtocolError{message: "zcode reset use 返回非预期结构: " + err.Error()}
	}
	// 上游合同要求 used 为 true；false 或缺失不能当成成功，否则页面会谎报核销。
	if !result.Used {
		return nil, &zcodeResetProtocolError{message: "zcode reset use 未确认核销"}
	}
	return &result, nil
}

// zcodeResetCall 执行一次 reset API 调用并解开统一信封。上游在 HTTP 4xx 时
// 仍可能返回 {"code":...,"msg":...}，因此先读 code，再按 HTTP 状态兜底。
func zcodeResetCall(ctx context.Context, client *http.Client, origin string, credential zcodeResetCredential, method, action string, body any) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, &zcodeResetProtocolError{message: "zcode reset 请求体无法编码: " + err.Error()}
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, origin+zcodeResetBasePath+"/"+action, reader)
	if err != nil {
		return nil, &zcodeResetProtocolError{message: "zcode reset 请求无法构造: " + err.Error()}
	}
	req.Header.Set("Authorization", credential.ZcodeJWT)
	req.Header.Set("X-Bigmodel-Authorization", credential.BusinessJWT)
	req.Header.Set("Bigmodel-Target-Type", "PERSONAL")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ZCode/"+ZcodeClientVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, &zcodeResetProtocolError{message: "zcode reset 上游不可达: " + err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, zcodeResetMaxBodyBytes))
	requestID := strings.TrimSpace(resp.Header.Get("x-request-id"))

	var envelope struct {
		Code *int            `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	parsed := len(bytes.TrimSpace(raw)) > 0 && json.Unmarshal(raw, &envelope) == nil

	if parsed && envelope.Code != nil && *envelope.Code != 0 {
		return nil, &zcodeResetAPIError{
			Status:    resp.StatusCode,
			Code:      *envelope.Code,
			Message:   strings.TrimSpace(envelope.Msg),
			RequestID: requestID,
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		message := strings.TrimSpace(envelope.Msg)
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return nil, &zcodeResetAPIError{
			Status:    resp.StatusCode,
			Message:   message,
			RequestID: requestID,
		}
	}
	if readErr != nil {
		return nil, &zcodeResetProtocolError{message: "zcode reset 读响应失败: " + readErr.Error()}
	}
	if !parsed {
		return nil, &zcodeResetProtocolError{message: "zcode reset 返回非 JSON 响应"}
	}
	if len(envelope.Data) == 0 || string(bytes.TrimSpace(envelope.Data)) == "null" {
		return nil, &zcodeResetProtocolError{message: "zcode reset 响应缺少 data 字段"}
	}
	return envelope.Data, nil
}

/* -------------------------------------------------------------------------- */
/* HTTP handler                                                                */
/* -------------------------------------------------------------------------- */

type zcodeResetStatusResponse struct {
	Channel                    string                  `json:"channel"`
	FetchedAt                  string                  `json:"fetched_at"`
	AvailableFiveHourResets    []zcodeResetOpportunity `json:"available_five_hour_resets"`
	AvailableWeekResets        []zcodeResetOpportunity `json:"available_week_resets"`
	LatestFiveHourResetHistory *zcodeResetHistory      `json:"latest_five_hour_reset_history"`
	LatestWeekResetHistory     *zcodeResetHistory      `json:"latest_week_reset_history"`
	HasUnreadHistory           bool                    `json:"has_unread_history"`
	// UseEnabled 告诉页面核销按钮是否可用；与 endpoint 的实际闸门同源。
	UseEnabled bool `json:"use_enabled"`
}

type zcodeResetUseRequest struct {
	ResetType      string `json:"reset_type"`
	IdempotencyKey string `json:"idempotency_key"`
	Channel        string `json:"channel"`
}

type zcodeResetUseResponse struct {
	Channel        string `json:"channel"`
	ResetType      string `json:"reset_type"`
	Used           bool   `json:"used"`
	IdempotencyKey string `json:"idempotency_key"`
}

// zcodeResetStatusHandler 服务 GET /v1/zcode/reset/status?channel=&refresh=1。
func (r *Router) zcodeResetStatusHandler(w http.ResponseWriter, q *http.Request) {
	channel, err := resolveZcodeResetChannel(q.URL.Query().Get("channel"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error(), "invalid_zcode_channel")
		return
	}
	// 页面请求不该被浏览器断开掐断缓存填充：脱离请求 ctx，只留超时。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(q.Context()), zcodeResetTimeout+2*time.Second)
	defer cancel()

	credential, err := r.zcodeResetCredential(ctx, channel)
	if err != nil {
		writeZcodeResetError(w, err)
		return
	}
	refresh := q.URL.Query().Get("refresh") == "1"
	status, fetchedAt, err := r.zcodeResetShared().fetchStatus(ctx, credential, refresh)
	if err != nil {
		r.logger.Warn(r.logMessage("zcode_reset_status_failed"),
			eventFields("zcode_reset_status_failed",
				zap.String("channel", credential.Channel),
				zap.String("reason", err.Error()))...)
		writeZcodeResetError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, zcodeResetStatusResponse{
		Channel:                    credential.Channel,
		FetchedAt:                  fetchedAt.UTC().Format(time.RFC3339),
		AvailableFiveHourResets:    status.AvailableFiveHourResets,
		AvailableWeekResets:        status.AvailableWeekResets,
		LatestFiveHourResetHistory: status.LatestFiveHourResetHistory,
		LatestWeekResetHistory:     status.LatestWeekResetHistory,
		HasUnreadHistory:           status.HasUnreadHistory,
		UseEnabled:                 r.cfg.ZcodeReset.AllowUse,
	})
}

// zcodeResetUseHandler 服务 POST /v1/zcode/reset/use。
//
// 调用方应自带 idempotency_key：网络错误后重试必须复用同一个键，否则一次
// 重试可能消耗两张卡。未提供时由网关生成并在响应里返回。
func (r *Router) zcodeResetUseHandler(w http.ResponseWriter, q *http.Request) {
	if !r.cfg.ZcodeReset.AllowUse {
		writeJSONError(w, http.StatusForbidden,
			"重置卡核销未启用：在配置中设置 zcode_reset.allow_use: true 后重启网关",
			"zcode_reset_use_disabled")
		return
	}
	// 要求 JSON 请求体：Basic 认证会随同源请求自动发送，form 编码的跨站
	// POST 不需要预检；application/json 会触发 CORS 预检，反代不放行就发不出去。
	if !strings.HasPrefix(strings.ToLower(q.Header.Get("Content-Type")), "application/json") {
		writeJSONError(w, http.StatusUnsupportedMediaType, "请求体必须是 application/json", "invalid_content_type")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, q.Body, 8<<10))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "读取请求体失败", "invalid_request_body")
		return
	}
	var request zcodeResetUseRequest
	if json.Unmarshal(body, &request) != nil {
		writeJSONError(w, http.StatusBadRequest, "请求体不是合法 JSON", "invalid_json")
		return
	}
	resetType := strings.ToUpper(strings.TrimSpace(request.ResetType))
	if resetType != zcodeResetTypeFiveHour && resetType != zcodeResetTypeWeek {
		writeJSONError(w, http.StatusBadRequest, "reset_type 必须是 FIVE_HOUR 或 WEEK", "invalid_reset_type")
		return
	}
	idempotencyKey := strings.TrimSpace(request.IdempotencyKey)
	if len(idempotencyKey) > zcodeResetMaxIdempotencyKey {
		writeJSONError(w, http.StatusBadRequest, "idempotency_key 不能超过 64 个字符", "invalid_idempotency_key")
		return
	}
	generatedKey := false
	if idempotencyKey == "" {
		idempotencyKey = newZcodeResetIdempotencyKey()
		generatedKey = true
	}
	channel, err := resolveZcodeResetChannel(request.Channel)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error(), "invalid_zcode_channel")
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(q.Context()), zcodeResetTimeout+2*time.Second)
	defer cancel()
	credential, err := r.zcodeResetCredential(ctx, channel)
	if err != nil {
		writeZcodeResetError(w, err)
		return
	}
	result, err := r.zcodeResetShared().use(ctx, credential, resetType, idempotencyKey)
	if err != nil {
		r.logger.Warn(r.logMessage("zcode_reset_use_failed"),
			eventFields("zcode_reset_use_failed",
				zap.String("channel", credential.Channel),
				zap.String("reset_type", resetType),
				zap.String("idempotency_key", idempotencyKey),
				zap.Bool("generated_key", generatedKey),
				zap.String("reason", err.Error()))...)
		writeZcodeResetError(w, err)
		return
	}
	// 核销成功后立刻让缓存失效：页面紧接着刷新的必须是新余量，而不是 30 秒
	// 内的旧快照。
	r.zcodeResetShared().invalidate(credential.Channel)
	r.logger.Info(r.logMessage("zcode_reset_used"),
		eventFields("zcode_reset_used",
			zap.String("channel", credential.Channel),
			zap.String("reset_type", resetType),
			zap.String("idempotency_key", idempotencyKey),
			zap.Bool("generated_key", generatedKey))...)

	writeJSON(w, http.StatusOK, zcodeResetUseResponse{
		Channel:        credential.Channel,
		ResetType:      resetType,
		Used:           result.Used,
		IdempotencyKey: idempotencyKey,
	})
}

// resolveZcodeResetChannel 校验显式渠道；空串表示由凭据解析自动选择。
func resolveZcodeResetChannel(requested string) (string, error) {
	requested = strings.ToLower(strings.TrimSpace(requested))
	switch requested {
	case "":
		return "", nil
	case ZcodeChannelBigmodel, ZcodeChannelZai:
		return requested, nil
	default:
		return "", fmt.Errorf("channel 必须是 %q 或 %q", ZcodeChannelBigmodel, ZcodeChannelZai)
	}
}

// writeZcodeResetError 把 reset 失败映射为网关错误信封与合适的 HTTP 状态。
// 错误消息来自上游/本地解析，都不含凭据。
func writeZcodeResetError(w http.ResponseWriter, err error) {
	var credentialErr *zcodeResetCredentialError
	if errors.As(err, &credentialErr) {
		writeJSONError(w, http.StatusServiceUnavailable, credentialErr.Error(), "zcode_reset_credential_missing")
		return
	}
	var apiErr *zcodeResetAPIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Status == http.StatusUnauthorized:
			writeJSONError(w, http.StatusUnauthorized, apiErr.Error(), "zcode_reset_auth_rejected")
			return
		case apiErr.Status == http.StatusForbidden || apiErr.Code == 3101:
			writeJSONError(w, http.StatusForbidden, apiErr.Error(), "zcode_reset_forbidden")
			return
		case apiErr.Status == http.StatusTooManyRequests || apiErr.Code == 3002:
			w.Header().Set("Retry-After", "60")
			writeJSONError(w, http.StatusTooManyRequests, apiErr.Error(), "zcode_reset_throttled")
			return
		case apiErr.Code == 3001:
			writeJSONError(w, http.StatusBadRequest, apiErr.Error(), "zcode_reset_invalid_scope")
			return
		default:
			writeJSONError(w, http.StatusBadGateway, apiErr.Error(), "zcode_reset_upstream_error")
			return
		}
	}
	var protocolErr *zcodeResetProtocolError
	if errors.As(err, &protocolErr) {
		writeJSONError(w, http.StatusBadGateway, protocolErr.Error(), "zcode_reset_protocol_error")
		return
	}
	writeJSONError(w, http.StatusBadGateway, err.Error(), "zcode_reset_unavailable")
}

// newZcodeResetIdempotencyKey 生成 UUID v4（上游只要求唯一、≤64 字符）。
func newZcodeResetIdempotencyKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("fallback-%d-%d", time.Now().UnixNano(), os.Getpid())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
