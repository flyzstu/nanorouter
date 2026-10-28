package router

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	json "github.com/goccy/go-json"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestAPIKeyStoreLifecycle(t *testing.T) {
	store := statsTestStore(t)
	keys := newAPIKeyStore(store.db)

	// 名称前后空白应被 trim。
	record, key, err := keys.create(context.Background(), "  laptop  ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, apiKeyPrefix) || len(key) < 20 {
		t.Fatalf("key = %q", key)
	}
	if record.Name != "laptop" {
		t.Fatalf("trimmed name = %q", record.Name)
	}
	if len(record.KeyHash) != 64 {
		t.Fatalf("hash length = %d", len(record.KeyHash))
	}
	if record.KeyPrefix != key[:apiKeyPrefixDisplayLen] {
		t.Fatalf("prefix = %q, want %q", record.KeyPrefix, key[:apiKeyPrefixDisplayLen])
	}
	if record.KeyHash != hashAPIKey(key) || record.KeyHash == key {
		t.Fatalf("hash = %q", record.KeyHash)
	}

	// 数据库只存哈希，不存明文。
	var storedHash string
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT key_hash FROM api_keys WHERE id = $1`, record.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash != hashAPIKey(key) || strings.Contains(storedHash, key) {
		t.Fatalf("stored hash = %q", storedHash)
	}

	if ok, err := keys.authenticate(context.Background(), key); err != nil || !ok {
		t.Fatalf("authenticate = %v, %v", ok, err)
	}
	if ok, _ := keys.authenticate(context.Background(), key+"x"); ok {
		t.Fatal("wrong key authenticated")
	}

	found, already, err := keys.revoke(context.Background(), record.ID)
	if err != nil || !found || already {
		t.Fatalf("revoke = %v, %v, %v", found, already, err)
	}
	if ok, _ := keys.authenticate(context.Background(), key); ok {
		t.Fatal("revoked key still authenticated")
	}
	found, already, err = keys.revoke(context.Background(), record.ID)
	if err != nil || !found || !already {
		t.Fatalf("second revoke = %v, %v, %v", found, already, err)
	}
	found, _, err = keys.revoke(context.Background(), "missing")
	if err != nil || found {
		t.Fatalf("missing revoke = %v, %v", found, err)
	}

	records, err := keys.list(context.Background())
	if err != nil || len(records) != 1 {
		t.Fatalf("list = %+v, %v", records, err)
	}
	if records[0].active() || records[0].Name != "laptop" {
		t.Fatalf("record = %+v", records[0])
	}

	if _, _, err := keys.create(context.Background(), "  "); err != errAPIKeyNameInvalid {
		t.Fatalf("empty name error = %v", err)
	}
	if _, _, err := keys.create(context.Background(), strings.Repeat("名", apiKeyNameMaxRunes+1)); err != errAPIKeyNameInvalid {
		t.Fatalf("long name error = %v", err)
	}
}

func TestAPIKeyHTTPLifecycle(t *testing.T) {
	t.Setenv("NANOROUTER_API_KEY_TEST", "root-secret")
	_, handler := newAPIKeyTestRouter(t, Config{
		AuthEnv: "NANOROUTER_API_KEY_TEST",
		APIKeys: APIKeysConfig{Enabled: true},
	})

	create := doAPIKeyRequest(handler, http.MethodPost, "/v1/api-keys", `{"name":"laptop"}`, "root-secret")
	if create.Code != http.StatusCreated {
		t.Fatalf("create = %d, body = %s", create.Code, create.Body.String())
	}
	if create.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create cache-control = %q", create.Header().Get("Cache-Control"))
	}
	var created apiKeyCreateResponse
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Key == "" || created.Prefix != created.Key[:apiKeyPrefixDisplayLen] {
		t.Fatalf("created = %+v", created)
	}

	list := doAPIKeyRequest(handler, http.MethodGet, "/v1/api-keys", "", "root-secret")
	if list.Code != http.StatusOK {
		t.Fatalf("list = %d, body = %s", list.Code, list.Body.String())
	}
	if list.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("list cache-control = %q", list.Header().Get("Cache-Control"))
	}
	if strings.Contains(list.Body.String(), created.Key) {
		t.Fatal("list leaked the full key")
	}
	var listed apiKeyListResponse
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Keys) != 1 || listed.Keys[0].Prefix != created.Prefix || listed.Keys[0].Status != "active" {
		t.Fatalf("listed = %+v", listed)
	}
	if !listed.RootAuth {
		t.Fatal("root_auth should be true")
	}

	if res := doAPIKeyRequest(handler, http.MethodGet, "/v1/models", "", created.Key); res.Code != http.StatusOK {
		t.Fatalf("managed key models = %d", res.Code)
	}
	// 第二种客户端习惯头也应被接受。
	xAPIKeyReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	xAPIKeyReq.Header.Set("X-Api-Key", created.Key)
	xAPIKeyRec := httptest.NewRecorder()
	handler.ServeHTTP(xAPIKeyRec, xAPIKeyReq)
	if xAPIKeyRec.Code != http.StatusOK {
		t.Fatalf("managed key X-Api-Key models = %d", xAPIKeyRec.Code)
	}
	// 匿名访问管理端点应返回 Basic challenge，供浏览器弹登录框。
	anonymousAdmin := doAPIKeyRequest(handler, http.MethodGet, "/v1/api-keys", "", "")
	if anonymousAdmin.Code != http.StatusUnauthorized || anonymousAdmin.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("anonymous admin = %d, challenge = %q", anonymousAdmin.Code, anonymousAdmin.Header().Get("WWW-Authenticate"))
	}
	for _, path := range []string{"/stats", "/v1/stats/summary", "/v1/zcode/usage", "/v1/zcode/reset/status", "/metrics"} {
		if res := doAPIKeyRequest(handler, http.MethodGet, path, "", created.Key); res.Code != http.StatusForbidden {
			t.Fatalf("managed key %s = %d, want 403", path, res.Code)
		}
	}
	if res := doAPIKeyRequest(handler, http.MethodGet, "/v1/api-keys", "", created.Key); res.Code != http.StatusForbidden {
		t.Fatalf("managed key admin = %d, want 403", res.Code)
	}

	// Basic 回退：根 Key 可管理，业务 Key 不可管理。
	basicRoot := httptest.NewRequest(http.MethodGet, "/v1/api-keys", nil)
	basicRoot.SetBasicAuth("root-secret", "")
	basicRootRec := httptest.NewRecorder()
	handler.ServeHTTP(basicRootRec, basicRoot)
	if basicRootRec.Code != http.StatusOK {
		t.Fatalf("basic root admin = %d, want 200", basicRootRec.Code)
	}
	basicManaged := httptest.NewRequest(http.MethodGet, "/v1/api-keys", nil)
	basicManaged.SetBasicAuth("", created.Key)
	basicManagedRec := httptest.NewRecorder()
	handler.ServeHTTP(basicManagedRec, basicManaged)
	if basicManagedRec.Code != http.StatusForbidden {
		t.Fatalf("basic managed admin = %d, want 403", basicManagedRec.Code)
	}

	revoke := doAPIKeyRequest(handler, http.MethodDelete, "/v1/api-keys/"+created.ID, "", "root-secret")
	if revoke.Code != http.StatusOK {
		t.Fatalf("revoke = %d, body = %s", revoke.Code, revoke.Body.String())
	}
	if res := doAPIKeyRequest(handler, http.MethodGet, "/v1/models", "", created.Key); res.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key models = %d, want 401", res.Code)
	}
	again := doAPIKeyRequest(handler, http.MethodDelete, "/v1/api-keys/"+created.ID, "", "root-secret")
	if again.Code != http.StatusOK || !strings.Contains(again.Body.String(), `"already_revoked":true`) {
		t.Fatalf("second revoke = %d, body = %s", again.Code, again.Body.String())
	}
	if res := doAPIKeyRequest(handler, http.MethodDelete, "/v1/api-keys/missing", "", "root-secret"); res.Code != http.StatusNotFound {
		t.Fatalf("missing revoke = %d, want 404", res.Code)
	}
	// 列表保留吊销记录，状态应为 revoked。
	listAfter, listAfterBody := doAPIKeyRequest(handler, http.MethodGet, "/v1/api-keys", "", "root-secret"), apiKeyListResponse{}
	if listAfter.Code != http.StatusOK {
		t.Fatalf("list after revoke = %d", listAfter.Code)
	}
	if err := json.Unmarshal(listAfter.Body.Bytes(), &listAfterBody); err != nil {
		t.Fatal(err)
	}
	if len(listAfterBody.Keys) != 1 || listAfterBody.Keys[0].Status != "revoked" || listAfterBody.Keys[0].RevokedAt == "" {
		t.Fatalf("revoked list = %+v", listAfterBody)
	}
}

func TestAPIKeyAuthMatrix(t *testing.T) {
	for _, tc := range []struct {
		name         string
		envKey       string
		apiKeys      APIKeysConfig
		statsPublic  bool
		managedPath  int
		managedAdmin int
		anonModels   int
		anonAdmin    int
		rootAdmin    int
	}{
		{
			name: "root + managed, no public admin", envKey: "root-secret",
			apiKeys:      APIKeysConfig{Enabled: true},
			managedPath:  http.StatusOK,
			managedAdmin: http.StatusForbidden,
			anonModels:   http.StatusUnauthorized,
			anonAdmin:    http.StatusUnauthorized,
			rootAdmin:    http.StatusOK,
		},
		{
			name: "manage_public exempts admin only", envKey: "root-secret",
			apiKeys:      APIKeysConfig{Enabled: true, ManagePublic: true},
			managedPath:  http.StatusOK,
			managedAdmin: http.StatusOK,
			anonModels:   http.StatusUnauthorized,
			anonAdmin:    http.StatusOK,
			rootAdmin:    http.StatusOK,
		},
		{
			name: "no root key but managed enabled", envKey: "",
			apiKeys:      APIKeysConfig{Enabled: true},
			managedPath:  http.StatusOK,
			managedAdmin: http.StatusServiceUnavailable,
			anonModels:   http.StatusUnauthorized,
			anonAdmin:    http.StatusServiceUnavailable,
			rootAdmin:    http.StatusServiceUnavailable,
		},
		{
			name: "no root key with manage_public", envKey: "",
			apiKeys:      APIKeysConfig{Enabled: true, ManagePublic: true},
			managedPath:  http.StatusOK,
			managedAdmin: http.StatusOK,
			anonModels:   http.StatusUnauthorized,
			anonAdmin:    http.StatusOK,
			rootAdmin:    http.StatusOK,
		},
		{
			name: "disabled keeps root behavior", envKey: "root-secret",
			apiKeys:      APIKeysConfig{},
			managedPath:  http.StatusUnauthorized,
			managedAdmin: http.StatusUnauthorized,
			anonModels:   http.StatusUnauthorized,
			anonAdmin:    http.StatusUnauthorized,
			rootAdmin:    http.StatusServiceUnavailable,
		},
		{
			name: "disabled and no root key stays open", envKey: "",
			apiKeys:      APIKeysConfig{},
			managedPath:  http.StatusOK,
			managedAdmin: http.StatusServiceUnavailable,
			anonModels:   http.StatusOK,
			anonAdmin:    http.StatusServiceUnavailable,
			rootAdmin:    http.StatusServiceUnavailable,
		},
		{
			name: "manage_public ignored when disabled", envKey: "",
			apiKeys:      APIKeysConfig{ManagePublic: true},
			managedPath:  http.StatusOK,
			managedAdmin: http.StatusServiceUnavailable,
			anonModels:   http.StatusOK,
			anonAdmin:    http.StatusServiceUnavailable,
			rootAdmin:    http.StatusServiceUnavailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NANOROUTER_API_KEY_TEST", tc.envKey)
			r, handler := newAPIKeyTestRouter(t, Config{
				AuthEnv:     "NANOROUTER_API_KEY_TEST",
				StatsPublic: tc.statsPublic,
				APIKeys:     tc.apiKeys,
			})
			// 即使 Enabled=false 也先建一把 Key：验证关闭开关后数据库里
			// 已有的业务 Key 不会再被接受。
			_, managedKey, err := r.apiKeys.create(context.Background(), "matrix")
			if err != nil {
				t.Fatal(err)
			}

			if res := doAPIKeyRequest(handler, http.MethodGet, "/v1/models", "", managedKey); res.Code != tc.managedPath {
				t.Fatalf("managed /v1/models = %d, want %d", res.Code, tc.managedPath)
			}
			if res := doAPIKeyRequest(handler, http.MethodGet, "/v1/api-keys", "", managedKey); res.Code != tc.managedAdmin {
				t.Fatalf("managed /v1/api-keys = %d, want %d", res.Code, tc.managedAdmin)
			}
			if res := doAPIKeyRequest(handler, http.MethodGet, "/v1/models", "", ""); res.Code != tc.anonModels {
				t.Fatalf("anon /v1/models = %d, want %d", res.Code, tc.anonModels)
			}
			if res := doAPIKeyRequest(handler, http.MethodGet, "/v1/api-keys", "", ""); res.Code != tc.anonAdmin {
				t.Fatalf("anon /v1/api-keys = %d, want %d", res.Code, tc.anonAdmin)
			}
			if res := doAPIKeyRequest(handler, http.MethodGet, "/v1/api-keys", "", tc.envKey); res.Code != tc.rootAdmin {
				t.Fatalf("root /v1/api-keys = %d, want %d", res.Code, tc.rootAdmin)
			}

			// 统计面公开时，业务 Key 与匿名请求一样直接放行；不公开时业务
			// Key 因 scope 被拒。
			if res := doAPIKeyRequest(handler, http.MethodGet, "/stats", "", managedKey); tc.statsPublic {
				if res.Code != http.StatusOK {
					t.Fatalf("public stats = %d", res.Code)
				}
			} else if tc.apiKeys.Enabled && res.Code != http.StatusForbidden {
				t.Fatalf("managed /stats = %d, want 403", res.Code)
			}
		})
	}
}

func TestAPIKeyCreateValidation(t *testing.T) {
	t.Setenv("NANOROUTER_API_KEY_TEST", "root-secret")
	_, handler := newAPIKeyTestRouter(t, Config{
		AuthEnv: "NANOROUTER_API_KEY_TEST",
		APIKeys: APIKeysConfig{Enabled: true},
	})

	if res := doAPIKeyRequest(handler, http.MethodPost, "/v1/api-keys", `{"name":"x"}`, ""); res.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous create = %d, want 401", res.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/api-keys", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Authorization", "Bearer root-secret")
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain create = %d, want 415", rec.Code)
	}
	for _, body := range []string{`not-json`, `{"name":""}`, `{"name":"` + strings.Repeat("x", 65) + `"}`} {
		if res := doAPIKeyRequest(handler, http.MethodPost, "/v1/api-keys", body, "root-secret"); res.Code != http.StatusBadRequest {
			t.Fatalf("invalid create %q = %d, want 400", body, res.Code)
		}
	}
	// 64 个中文字符按 rune 计数应允许。
	body := `{"name":"` + strings.Repeat("名", apiKeyNameMaxRunes) + `"}`
	if res := doAPIKeyRequest(handler, http.MethodPost, "/v1/api-keys", body, "root-secret"); res.Code != http.StatusCreated {
		t.Fatalf("64-rune name = %d, body = %s", res.Code, res.Body.String())
	}
}

func TestConfigAPIKeysOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	document := "api_keys:\n  enabled: true\n  manage_public: true\nroutes:\n  r:\n    candidates:\n      - id: u\n        base_url: http://example.test\n        model: m\n"
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.APIKeys.Enabled || !c.APIKeys.ManagePublic {
		t.Fatalf("api_keys = %+v", c.APIKeys)
	}
}

func TestAPIKeyPathClassification(t *testing.T) {
	for _, path := range []string{
		"/v1/chat/completions", "/chat/completions",
		"/v1/responses", "/responses",
		"/v1/messages", "/messages",
		"/v1/models", "/models",
		"/zhipu-aicoding/models", "/zhipu-aicoding/v1/models",
		"/zhipuaicoding/v1/chat/completions",
	} {
		if !isGenerationPath(path) {
			t.Fatalf("%s should be a generation path", path)
		}
	}
	for _, path := range []string{
		"/stats", "/v1/stats/summary", "/v1/zcode/usage",
		"/v1/zcode/reset/status", "/v1/zcode/reset/use",
		"/v1/api-keys", "/v1/api-keys/abc",
		"/metrics", "/zcode/login", "/debug/pprof/",
	} {
		if isGenerationPath(path) {
			t.Fatalf("%s must not be a generation path", path)
		}
	}
	for _, path := range []string{"/v1/api-keys", "/v1/api-keys/", "/v1/api-keys/abc"} {
		if !isAPIKeyAdminPath(path) {
			t.Fatalf("%s should be an admin path", path)
		}
	}
	if isAPIKeyAdminPath("/v1/api-keys-extra") {
		t.Fatal("prefix match must not swallow /v1/api-keys-extra")
	}
}

func TestAPIKeyListOrdering(t *testing.T) {
	store := statsTestStore(t)
	keys := newAPIKeyStore(store.db)
	current := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	keys.now = func() time.Time { return current }

	first, _, err := keys.create(context.Background(), "first")
	if err != nil {
		t.Fatal(err)
	}
	current = current.Add(time.Second)
	second, _, err := keys.create(context.Background(), "second")
	if err != nil {
		t.Fatal(err)
	}
	records, err := keys.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].ID != second.ID || records[1].ID != first.ID {
		t.Fatalf("list order = %+v", records)
	}
}

func TestMigrateAPIKeysIdempotent(t *testing.T) {
	db := mustOpenMemoryDB(t)
	// mustOpenMemoryDB 已经跑过一次 Migrate；再跑一次必须幂等。
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM api_keys`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("api_keys rows = %d", count)
	}
}

func TestAPIKeyManagePublicAnonymousCRUD(t *testing.T) {
	t.Setenv("NANOROUTER_API_KEY_TEST", "")
	_, handler := newAPIKeyTestRouter(t, Config{
		AuthEnv: "NANOROUTER_API_KEY_TEST",
		APIKeys: APIKeysConfig{Enabled: true, ManagePublic: true},
	})

	create := doAPIKeyRequest(handler, http.MethodPost, "/v1/api-keys", `{"name":"sso"}`, "")
	if create.Code != http.StatusCreated {
		t.Fatalf("anonymous create = %d, body = %s", create.Code, create.Body.String())
	}
	var created apiKeyCreateResponse
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if res := doAPIKeyRequest(handler, http.MethodGet, "/v1/models", "", ""); res.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous models = %d, want 401", res.Code)
	}
	if res := doAPIKeyRequest(handler, http.MethodGet, "/v1/models", "", created.Key); res.Code != http.StatusOK {
		t.Fatalf("managed models = %d, want 200", res.Code)
	}
	if res := doAPIKeyRequest(handler, http.MethodGet, "/v1/api-keys", "", ""); res.Code != http.StatusOK {
		t.Fatalf("anonymous list = %d, want 200", res.Code)
	}
	if res := doAPIKeyRequest(handler, http.MethodDelete, "/v1/api-keys/"+created.ID, "", ""); res.Code != http.StatusOK {
		t.Fatalf("anonymous revoke = %d, want 200", res.Code)
	}
	if res := doAPIKeyRequest(handler, http.MethodGet, "/v1/models", "", created.Key); res.Code != http.StatusUnauthorized {
		t.Fatalf("revoked models = %d, want 401", res.Code)
	}
}

func TestAPIKeyNotLogged(t *testing.T) {
	t.Setenv("NANOROUTER_API_KEY_TEST", "root-secret")
	core, logs := observer.New(zap.InfoLevel)
	r, err := NewWithLogger(Config{
		AuthEnv: "NANOROUTER_API_KEY_TEST",
		APIKeys: APIKeysConfig{Enabled: true},
		Routes: map[string]Route{"m": {Candidates: []Candidate{{
			ID: "u", BaseURL: "http://127.0.0.1:1", Model: "m",
		}}}},
	}, mustOpenMemoryDB(t), zap.New(core))
	if err != nil {
		t.Fatal(err)
	}
	create := doAPIKeyRequest(r.Handler(), http.MethodPost, "/v1/api-keys", `{"name":"secret-test"}`, "root-secret")
	if create.Code != http.StatusCreated {
		t.Fatalf("create = %d, body = %s", create.Code, create.Body.String())
	}
	var created apiKeyCreateResponse
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	for _, entry := range logs.All() {
		if strings.Contains(entry.Message, created.Key) {
			t.Fatalf("log message leaked key: %q", entry.Message)
		}
		for name, value := range entry.ContextMap() {
			if strings.Contains(fmt.Sprint(value), created.Key) {
				t.Fatalf("log field %q leaked key: %v", name, value)
			}
		}
	}
}

func TestAPIKeyRevocationPropagatesAfterCacheTTL(t *testing.T) {
	store := statsTestStore(t)
	owner := newAPIKeyStore(store.db)
	peer := newAPIKeyStore(store.db)
	current := time.Now()
	peer.now = func() time.Time { return current }

	record, key, err := owner.create(context.Background(), "cache")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := peer.authenticate(context.Background(), key); err != nil || !ok {
		t.Fatalf("peer initial auth = %v, %v", ok, err)
	}
	if _, _, err := owner.revoke(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	// 对端缓存未过期：吊销尚未传播。
	if ok, err := peer.authenticate(context.Background(), key); err != nil || !ok {
		t.Fatalf("peer cached auth = %v, %v", ok, err)
	}
	current = current.Add(apiKeyCacheTTL + time.Second)
	// 缓存过期后重读数据库，吊销生效。
	if ok, err := peer.authenticate(context.Background(), key); err != nil || ok {
		t.Fatalf("peer auth after TTL = %v, %v", ok, err)
	}
}

func TestAPIKeyConcurrentRefresh(t *testing.T) {
	store := statsTestStore(t)
	keys := newAPIKeyStore(store.db)
	_, key, err := keys.create(context.Background(), "concurrent")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := keys.authenticate(context.Background(), key); err != nil || !ok {
		t.Fatalf("initial auth = %v, %v", ok, err)
	}

	// 让缓存立刻过期，并发的认证请求会同时触发刷新。
	keys.mu.Lock()
	keys.loadedAt = time.Time{}
	keys.mu.Unlock()

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := keys.authenticate(context.Background(), key)
			if err != nil {
				errs <- err
				return
			}
			if !ok {
				errs <- fmt.Errorf("valid key rejected during refresh")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestAPIKeyAuthFailsClosedOnDBError(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	store := newAPIKeyStore(db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ok, err := store.authenticate(context.Background(), "nr_missing")
	if err == nil || ok {
		t.Fatalf("authenticate with closed db = %v, %v; want fail-closed", ok, err)
	}
}

func TestAPIKeyPostgresStore(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("NANOROUTER_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("NANOROUTER_TEST_POSTGRES_DSN not set")
	}
	db := openTestPostgres(t, dsn)
	defer db.Close()

	ctx := context.Background()
	// 连跑两次验证建表幂等。
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `TRUNCATE TABLE api_keys`); err != nil {
		t.Fatal(err)
	}

	keys := newAPIKeyStore(db)
	record, key, err := keys.create(ctx, "pg-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(record.KeyHash) != 64 || record.KeyPrefix != key[:apiKeyPrefixDisplayLen] {
		t.Fatalf("record = %+v", record)
	}
	if ok, err := keys.authenticate(ctx, key); err != nil || !ok {
		t.Fatalf("authenticate = %v, %v", ok, err)
	}
	if ok, _ := keys.authenticate(ctx, key+"x"); ok {
		t.Fatal("wrong key authenticated")
	}
	if found, already, err := keys.revoke(ctx, record.ID); err != nil || !found || already {
		t.Fatalf("revoke = %v, %v, %v", found, already, err)
	}
	if ok, _ := keys.authenticate(ctx, key); ok {
		t.Fatal("revoked key authenticated")
	}
	records, err := keys.list(ctx)
	if err != nil || len(records) != 1 || records[0].active() {
		t.Fatalf("list = %+v, %v", records, err)
	}
}

func openTestPostgres(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	var lastErr error
	// CI 的 service container 可能在测试开始后才完成启动；这里最多等 30 秒。
	for i := 0; i < 30; i++ {
		db, err := sql.Open("postgres", dsn)
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err = db.PingContext(ctx)
			cancel()
			if err == nil {
				return db
			}
			_ = db.Close()
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	t.Fatalf("postgres not ready: %v", lastErr)
	return nil
}

func newAPIKeyTestRouter(t *testing.T, cfg Config) (*Router, http.Handler) {
	t.Helper()
	cfg.Routes = map[string]Route{"m": {Candidates: []Candidate{{
		ID: "u", BaseURL: "http://127.0.0.1:1", Model: "m",
	}}}}
	r, err := New(cfg, mustOpenMemoryDB(t))
	if err != nil {
		t.Fatal(err)
	}
	return r, r.Handler()
}

func doAPIKeyRequest(handler http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
