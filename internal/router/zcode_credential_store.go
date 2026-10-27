package router

// ZCode 凭据的数据库存储。
//
// 为什么不用 Secret：Secret 卷在容器内是只读的，Pod 也没有写 Secret 的
// RBAC 权限。OAuth 刷新拿到的新 token 无处可存——下次启动读到的仍是旧的
// 过期 token，于是再触发一次 OAuth，循环往复。数据库是程序自己能写的地方，
// 也是唯一能让「登录一次、长期复用」成立的位置。
//
// 有效期不落库：智谱的设备码流程不回传 key 的有效期（见 storeZcodeToken 的
// 说明），任何本地过期时间都是猜测。last_verified 只记录上次登录写入的时刻
// 供观测，失效判定一律靠探测。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ZcodeCredential 是一条渠道的凭据。字段与登录链的产物一一对应：平台 JWT 与
// OAuth token 目前没有消费方（Start Plan 未支持），但留着它们才能在将来
// 启用时不必重新登录一次。
type ZcodeCredential struct {
	Channel      string
	APIKey       string
	PlatformJWT  string
	OAuthToken   string
	UserName     string
	DeviceMid    string
	CreatedAt    time.Time
	LastVerified time.Time
}

// zcodeCredentialStore 是凭据的读写层，带进程内缓存。
//
// 缓存存在的理由与文件源相同：请求路径每次都来读，而登录是低频动作。5 秒窗口
// 足以吸收一次登录写入后的传播延迟，同时把 DB 查询压到可忽略。负结果同样缓存
// ——刚登录完的那一瞬还没有凭据，不该让每个请求都去查一次空表。
type zcodeCredentialStore struct {
	db *sql.DB

	mu      sync.Mutex
	loaded  bool
	entries map[string]ZcodeCredential
	at      time.Time

	// loadMu 串行化缓存刷新；mu 只保护字段读写，不再横跨 DB 查询，
	// 避免刷新期间阻塞仍命中缓存的请求。
	loadMu sync.Mutex
}

const zcodeCredentialCacheTTL = 5 * time.Second

func newZcodeCredentialStore(db *sql.DB) *zcodeCredentialStore {
	return &zcodeCredentialStore{db: db, entries: map[string]ZcodeCredential{}}
}

// load 返回当前全部渠道的凭据，过期时重查。
func (s *zcodeCredentialStore) load(ctx context.Context) (map[string]ZcodeCredential, error) {
	s.mu.Lock()
	if s.loaded && time.Since(s.at) < zcodeCredentialCacheTTL {
		entries := s.entries
		s.mu.Unlock()
		return entries, nil
	}
	s.mu.Unlock()

	// 缓存过期时只让一个请求查库，其余请求等它完成后复用结果；
	// 命中缓存的请求不会被这次刷新阻塞。
	s.loadMu.Lock()
	defer s.loadMu.Unlock()

	s.mu.Lock()
	if s.loaded && time.Since(s.at) < zcodeCredentialCacheTTL {
		entries := s.entries
		s.mu.Unlock()
		return entries, nil
	}
	stale := s.entries
	s.mu.Unlock()

	entries, err := s.query(ctx)
	if err != nil {
		// 查询失败时沿用上一次的结果（含空表）：DB 抖动不该让正在服务的
		// 候选突然失去凭据。下一轮缓存过期后自然重试。
		return stale, err
	}
	s.mu.Lock()
	s.entries = entries
	s.loaded = true
	s.at = time.Now()
	s.mu.Unlock()
	return entries, nil
}

func (s *zcodeCredentialStore) query(ctx context.Context) (map[string]ZcodeCredential, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT channel, api_key, platform_jwt, oauth_token, user_name, device_mid, created_at, last_verified FROM zcode_credentials`)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	entries := map[string]ZcodeCredential{}
	for rows.Next() {
		var c ZcodeCredential
		var createdAt, lastVerified string
		if err := rows.Scan(&c.Channel, &c.APIKey, &c.PlatformJWT, &c.OAuthToken, &c.UserName, &c.DeviceMid, &createdAt, &lastVerified); err != nil {
			return nil, err
		}
		c.CreatedAt = parseCredentialTime(createdAt)
		c.LastVerified = parseCredentialTime(lastVerified)
		entries[c.Channel] = c
	}
	return entries, rows.Err()
}

// tokensForChannel 按渠道取平台 JWT 与 OAuth token，不要求 api_key 非空
// （reset 端点不使用 key）。这里刻意不做跨渠道回退：业务 JWT 按 family
// 签发，混用会被上游拒绝，也会把 A 渠道的活体凭据发给 B 渠道。
func (s *zcodeCredentialStore) tokensForChannel(ctx context.Context, channel string) (ZcodeCredential, bool) {
	entries, _ := s.load(ctx)
	c, ok := entries[channel]
	if !ok || (c.PlatformJWT == "" && c.OAuthToken == "") {
		return ZcodeCredential{}, false
	}
	return c, true
}

// get 按渠道取凭据。跨渠道回退的取舍与文件源一致：bigmodel 优先于 zai。
func (s *zcodeCredentialStore) get(ctx context.Context, channel string) (ZcodeCredential, bool) {
	entries, _ := s.load(ctx)
	if channel != ZcodeChannelZai {
		channel = ZcodeChannelBigmodel
	}
	if c, ok := entries[channel]; ok && c.APIKey != "" {
		return c, true
	}
	fallback := ZcodeChannelBigmodel
	if channel == fallback {
		fallback = ZcodeChannelZai
	}
	c, ok := entries[fallback]
	return c, ok && c.APIKey != ""
}

// save 写入或更新一条凭据，并立刻失效缓存让下一请求读到新值。
//
// 用参数化的 UPDATE + INSERT 而非方言相关的 UPSERT：两种方言的 upsert 语法
// 不同（PostgreSQL 是 ON CONFLICT，SQLite 有 ON CONFLICT DO UPDATE），
// 而「先改后插」在双方言下行为一致，且天然幂等——重复登录同一渠道不会插出
// 第二行。
func (s *zcodeCredentialStore) save(ctx context.Context, c ZcodeCredential) error {
	if c.Channel == "" {
		return errors.New("zcode credential requires a channel")
	}
	if c.APIKey == "" {
		return errors.New("zcode credential requires an api key")
	}
	now := time.Now().UTC()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	// 登录成功即视为已验证：登录链在落库前刚用这个 key 打过一次探测。
	c.LastVerified = now
	verified := c.LastVerified.Format(time.RFC3339Nano)

	// 先 UPDATE 再 INSERT，不用方言相关的 UPSERT：PostgreSQL 与 SQLite 的
	// upsert 语法不同，而「先改后插」在双方言下行为一致。命中 UPDATE 时
	// rowsAffected 为 1，就此收工；为 0 说明该渠道还没有记录，走 INSERT。
	res, err := s.db.ExecContext(ctx,
		`UPDATE zcode_credentials SET api_key=$1, platform_jwt=$2, oauth_token=$3,
		 user_name=$4, device_mid=$5, last_verified=$6 WHERE channel=$7`,
		c.APIKey, c.PlatformJWT, c.OAuthToken, c.UserName, c.DeviceMid, verified, c.Channel)
	if err != nil {
		return fmt.Errorf("update zcode credential failed: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected > 0 {
		s.invalidate()
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO zcode_credentials (api_key, platform_jwt, oauth_token, user_name, device_mid, created_at, last_verified, channel)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		c.APIKey, c.PlatformJWT, c.OAuthToken, c.UserName, c.DeviceMid,
		c.CreatedAt.Format(time.RFC3339Nano), verified, c.Channel); err != nil {
		// 多副本并发登录时另一个副本可能刚插入同一渠道：让出，它已写好。
		if isDuplicateKey(err) {
			s.invalidate()
			return nil
		}
		return fmt.Errorf("insert zcode credential failed: %w", err)
	}
	s.invalidate()
	return nil
}

// invalidate 清空缓存。写库后必须调用，否则新凭据要等 5 秒才生效。
// 先取 loadMu 等掉进行中的刷新，避免旧查询结果覆盖刚写入的凭据。
func (s *zcodeCredentialStore) invalidate() {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded = false
	s.entries = map[string]ZcodeCredential{}
}

// deviceMid 返回任意渠道记录上的设备身份：它属于机器而非账号，跨渠道同值。
func (s *zcodeCredentialStore) deviceMid(ctx context.Context) string {
	entries, _ := s.load(ctx)
	// 先 bigmodel 再 zai，与跨渠道回退同序。
	for _, channel := range []string{ZcodeChannelBigmodel, ZcodeChannelZai} {
		if c, ok := entries[channel]; ok && c.DeviceMid != "" {
			return c.DeviceMid
		}
	}
	return ""
}

func isDuplicateKey(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate key") || strings.Contains(msg, "unique constraint")
}

func parseCredentialTime(value string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, value); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ensureZcodeCredentialTable 建表。放在 Migrate 里以便与 llm_requests 同批升级。
func ensureZcodeCredentialTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS zcode_credentials (
  channel TEXT PRIMARY KEY,
  api_key TEXT NOT NULL,
  platform_jwt TEXT NOT NULL DEFAULT '',
  oauth_token TEXT NOT NULL DEFAULT '',
  user_name TEXT NOT NULL DEFAULT '',
  device_mid TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  last_verified TEXT NOT NULL
);`)
	if err != nil {
		return fmt.Errorf("create zcode_credentials failed: %w", err)
	}
	return nil
}
