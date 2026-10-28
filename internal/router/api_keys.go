package router

// 页面创建的 API Key。
//
// 根 Key 仍来自 auth_env（可选，只用于 break-glass）；这里的业务 Key 存
// 数据库，只保存 SHA-256 哈希与前缀，完整 Key 只在创建响应中出现一次。
// 认证路径用进程内缓存（5 秒）避免每个请求查库；本实例创建/吊销后立即
// 失效缓存，多副本部署最多 5 秒后感知。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	apiKeyPrefix           = "nr_"
	apiKeyRandomBytes      = 32
	apiKeyPrefixDisplayLen = 12
	apiKeyNameMaxRunes     = 64
	apiKeyCacheTTL         = 5 * time.Second
)

var errAPIKeyNameInvalid = errors.New("api key name must be 1-64 characters")

// apiKeyRecord 是数据库行的内存映射。RevokedAt 为零值表示有效。
type apiKeyRecord struct {
	ID        string
	Name      string
	KeyHash   string
	KeyPrefix string
	CreatedAt time.Time
	RevokedAt time.Time
}

func (r apiKeyRecord) active() bool { return r.RevokedAt.IsZero() }

type apiKeyStore struct {
	db *sql.DB

	mu       sync.Mutex
	active   map[string]struct{}
	loadedAt time.Time
	now      func() time.Time

	// loadMu 串行化缓存刷新；mu 只保护字段读写，不再横跨 DB 查询，
	// 避免刷新期间阻塞仍命中缓存的请求。
	loadMu sync.Mutex
}

func newAPIKeyStore(db *sql.DB) *apiKeyStore {
	return &apiKeyStore{
		db:     db,
		active: map[string]struct{}{},
		now:    time.Now,
	}
}

// ensureAPIKeyTable 建表。放在 Migrate 里与账本/凭据表同批升级。
func ensureAPIKeyTable(ctx context.Context, db *sql.DB) error {
	// key_hash 的 UNIQUE 约束自带索引，认证查询走该索引即可。
	statements := []string{
		`CREATE TABLE IF NOT EXISTS api_keys (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  key_hash TEXT NOT NULL UNIQUE,
  key_prefix TEXT NOT NULL,
  created_at TEXT NOT NULL,
  revoked_at TEXT NOT NULL DEFAULT ''
);`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create api_keys failed: %w", err)
		}
	}
	return nil
}

func (s *apiKeyStore) list(ctx context.Context) ([]apiKeyRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, key_hash, key_prefix, created_at, revoked_at FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	records := make([]apiKeyRecord, 0, 8)
	for rows.Next() {
		var record apiKeyRecord
		var createdAt, revokedAt string
		if err := rows.Scan(&record.ID, &record.Name, &record.KeyHash, &record.KeyPrefix, &createdAt, &revokedAt); err != nil {
			return nil, err
		}
		record.CreatedAt = parseAPIKeyTime(createdAt)
		record.RevokedAt = parseAPIKeyTime(revokedAt)
		records = append(records, record)
	}
	return records, rows.Err()
}

// create 生成并写入一把业务 Key。返回的记录不含完整 Key；完整 Key 只通过
// 第二个返回值交给创建响应，调用方不得落库或写日志。
func (s *apiKeyStore) create(ctx context.Context, name string) (apiKeyRecord, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > apiKeyNameMaxRunes {
		return apiKeyRecord{}, "", errAPIKeyNameInvalid
	}
	raw := make([]byte, apiKeyRandomBytes)
	if _, err := rand.Read(raw); err != nil {
		return apiKeyRecord{}, "", fmt.Errorf("generate api key failed: %w", err)
	}
	key := apiKeyPrefix + base64.RawURLEncoding.EncodeToString(raw)
	prefix := key
	if len(prefix) > apiKeyPrefixDisplayLen {
		prefix = prefix[:apiKeyPrefixDisplayLen]
	}
	record := apiKeyRecord{
		ID:        newID(),
		Name:      name,
		KeyHash:   hashAPIKey(key),
		KeyPrefix: prefix,
		CreatedAt: s.now().UTC(),
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys (id, name, key_hash, key_prefix, created_at, revoked_at) VALUES ($1, $2, $3, $4, $5, '')`,
		record.ID, record.Name, record.KeyHash, record.KeyPrefix, record.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return apiKeyRecord{}, "", fmt.Errorf("insert api key failed: %w", err)
	}
	s.invalidate()
	return record, key, nil
}

// revoke 标记吊销。返回 (found, alreadyRevoked, err)。
func (s *apiKeyStore) revoke(ctx context.Context, id string) (bool, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return false, false, nil
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = $1 WHERE id = $2 AND revoked_at = ''`, now, id)
	if err != nil {
		return false, false, err
	}
	if affected, err := result.RowsAffected(); err == nil && affected > 0 {
		s.invalidate()
		return true, false, nil
	}
	var revokedAt string
	if err := s.db.QueryRowContext(ctx, `SELECT revoked_at FROM api_keys WHERE id = $1`, id).Scan(&revokedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, false, nil
		}
		return false, false, err
	}
	s.invalidate()
	return true, true, nil
}

// authenticate 判断 presented 是否为当前有效的业务 Key。
func (s *apiKeyStore) authenticate(ctx context.Context, presented string) (bool, error) {
	presented = strings.TrimSpace(presented)
	if presented == "" {
		return false, nil
	}
	hash := hashAPIKey(presented)

	s.mu.Lock()
	if !s.loadedAt.IsZero() && s.now().Sub(s.loadedAt) < apiKeyCacheTTL {
		_, ok := s.active[hash]
		s.mu.Unlock()
		return ok, nil
	}
	s.mu.Unlock()

	// 缓存过期时只让一个请求查库，其余请求等它完成后复用结果；
	// 命中缓存的请求不会被这次刷新阻塞。
	s.loadMu.Lock()
	defer s.loadMu.Unlock()

	s.mu.Lock()
	if !s.loadedAt.IsZero() && s.now().Sub(s.loadedAt) < apiKeyCacheTTL {
		_, ok := s.active[hash]
		s.mu.Unlock()
		return ok, nil
	}
	s.mu.Unlock()

	next, err := s.queryActive(ctx)
	if err != nil {
		// 缓存过期且数据库不可用时 fail-closed：根 Key 仍可使用。
		return false, err
	}
	s.mu.Lock()
	s.active = next
	s.loadedAt = s.now()
	s.mu.Unlock()
	_, ok := next[hash]
	return ok, nil
}

func (s *apiKeyStore) queryActive(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key_hash FROM api_keys WHERE revoked_at = ''`)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	next := make(map[string]struct{}, 16)
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		next[hash] = struct{}{}
	}
	return next, rows.Err()
}

func (s *apiKeyStore) invalidate() {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = map[string]struct{}{}
	s.loadedAt = time.Time{}
}

func hashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func parseAPIKeyTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}
