package router

// API Key 管理端点。创建响应只返回一次完整 Key；列表只返回前缀和状态。

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	json "github.com/goccy/go-json"
	"go.uber.org/zap"
)

type apiKeyView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Prefix    string `json:"prefix"`
	CreatedAt string `json:"created_at"`
	RevokedAt string `json:"revoked_at,omitempty"`
	Status    string `json:"status"`
}

type apiKeyListResponse struct {
	Keys         []apiKeyView `json:"keys"`
	ManagePublic bool         `json:"manage_public"`
	RootAuth     bool         `json:"root_auth"`
}

type apiKeyCreateRequest struct {
	Name string `json:"name"`
}

type apiKeyCreateResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Prefix    string `json:"prefix"`
	CreatedAt string `json:"created_at"`
	Key       string `json:"key"`
}

type apiKeyRevokeResponse struct {
	ID             string `json:"id"`
	Revoked        bool   `json:"revoked"`
	AlreadyRevoked bool   `json:"already_revoked"`
}

func apiKeyViewFromRecord(record apiKeyRecord) apiKeyView {
	view := apiKeyView{
		ID:     record.ID,
		Name:   record.Name,
		Prefix: record.KeyPrefix,
		Status: "active",
	}
	if !record.CreatedAt.IsZero() {
		view.CreatedAt = record.CreatedAt.UTC().Format(time.RFC3339)
	}
	if !record.active() {
		view.Status = "revoked"
		view.RevokedAt = record.RevokedAt.UTC().Format(time.RFC3339)
	}
	return view
}

// listAPIKeys 服务 GET /v1/api-keys。
func (r *Router) listAPIKeys(w http.ResponseWriter, q *http.Request) {
	if !r.requireAPIKeyAdmin(w, q) {
		return
	}
	records, err := r.apiKeys.list(q.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "读取 API Key 失败", "api_keys_unavailable")
		return
	}
	keys := make([]apiKeyView, 0, len(records))
	for _, record := range records {
		keys = append(keys, apiKeyViewFromRecord(record))
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, apiKeyListResponse{
		Keys:         keys,
		ManagePublic: r.cfg.APIKeys.ManagePublic,
		RootAuth:     os.Getenv(r.cfg.AuthEnv) != "",
	})
}

// createAPIKey 服务 POST /v1/api-keys。完整 Key 只出现在本次响应。
func (r *Router) createAPIKey(w http.ResponseWriter, q *http.Request) {
	if !r.requireAPIKeyAdmin(w, q) {
		return
	}
	// 要求 JSON 请求体：Basic/SSO 凭据会随同源请求自动发送，form 编码的
	// 跨站 POST 不需要预检；application/json 会触发 CORS 预检。
	if !strings.HasPrefix(strings.ToLower(q.Header.Get("Content-Type")), "application/json") {
		writeJSONError(w, http.StatusUnsupportedMediaType, "请求体必须是 application/json", "invalid_content_type")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, q.Body, 8<<10))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "读取请求体失败", "invalid_request_body")
		return
	}
	var request apiKeyCreateRequest
	if json.Unmarshal(body, &request) != nil {
		writeJSONError(w, http.StatusBadRequest, "请求体不是合法 JSON", "invalid_json")
		return
	}
	record, key, err := r.apiKeys.create(q.Context(), request.Name)
	if errors.Is(err, errAPIKeyNameInvalid) {
		writeJSONError(w, http.StatusBadRequest, "名称必须是 1-64 个字符", "invalid_api_key_name")
		return
	}
	if err != nil {
		r.logger.Warn(r.logMessage("api_key_create_failed"),
			eventFields("api_key_create_failed", zap.String("reason", err.Error()))...)
		writeJSONError(w, http.StatusInternalServerError, "创建 API Key 失败", "api_keys_unavailable")
		return
	}
	r.logger.Info(r.logMessage("api_key_created"),
		eventFields("api_key_created",
			zap.String("id", record.ID),
			zap.String("name", record.Name),
			zap.String("prefix", record.KeyPrefix))...)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, apiKeyCreateResponse{
		ID:        record.ID,
		Name:      record.Name,
		Prefix:    record.KeyPrefix,
		CreatedAt: record.CreatedAt.UTC().Format(time.RFC3339),
		Key:       key,
	})
}

// revokeAPIKey 服务 DELETE /v1/api-keys/{id}。吊销后记录保留。
func (r *Router) revokeAPIKey(w http.ResponseWriter, q *http.Request) {
	if !r.requireAPIKeyAdmin(w, q) {
		return
	}
	id := strings.TrimSpace(chi.URLParam(q, "id"))
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "缺少 API Key id", "invalid_api_key_id")
		return
	}
	found, already, err := r.apiKeys.revoke(q.Context(), id)
	if err != nil {
		r.logger.Warn(r.logMessage("api_key_revoke_failed"),
			eventFields("api_key_revoke_failed", zap.String("id", id), zap.String("reason", err.Error()))...)
		writeJSONError(w, http.StatusInternalServerError, "吊销 API Key 失败", "api_keys_unavailable")
		return
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, "API Key 不存在", "api_key_not_found")
		return
	}
	if !already {
		r.logger.Info(r.logMessage("api_key_revoked"),
			eventFields("api_key_revoked", zap.String("id", id))...)
	}
	writeJSON(w, http.StatusOK, apiKeyRevokeResponse{
		ID:             id,
		Revoked:        !already,
		AlreadyRevoked: already,
	})
}

// requireAPIKeyAdmin 校验管理权限。manage_public 时由反代承担授权；
// 否则只认根 auth_env Key。
func (r *Router) requireAPIKeyAdmin(w http.ResponseWriter, q *http.Request) bool {
	if !r.cfg.APIKeys.Enabled {
		writeJSONError(w, http.StatusServiceUnavailable,
			"API Key 管理未启用：配置 api_keys.enabled: true", "api_keys_disabled")
		return false
	}
	if r.cfg.APIKeys.ManagePublic {
		return true
	}
	if os.Getenv(r.cfg.AuthEnv) == "" {
		writeJSONError(w, http.StatusServiceUnavailable,
			"管理 API Key 需要配置 auth_env，或设置 api_keys.manage_public 由反代鉴权",
			"api_keys_admin_unavailable")
		return false
	}
	if authRoleFromContext(q.Context()) != authRoleRoot {
		writeJSONError(w, http.StatusForbidden,
			"只有根 auth_env Key 可以管理 API Key", "api_keys_admin_required")
		return false
	}
	return true
}
