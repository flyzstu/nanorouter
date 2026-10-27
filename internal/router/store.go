package router

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var errLedgerQueueFull = errors.New("ledger async queue is full")
var errLedgerClosed = errors.New("ledger is closed")

func Migrate(ctx context.Context, db *sql.DB) error {
	statements := []string{`CREATE TABLE IF NOT EXISTS llm_requests (
 id TEXT PRIMARY KEY, created_at TEXT NOT NULL, route TEXT NOT NULL, upstream_id TEXT NOT NULL, upstream_model TEXT NOT NULL,
 endpoint TEXT NOT NULL, status_code INTEGER NOT NULL, latency_ms INTEGER NOT NULL, streamed INTEGER NOT NULL, attempt INTEGER NOT NULL,
 prompt_tokens BIGINT, completion_tokens BIGINT, total_tokens BIGINT, usage_source TEXT NOT NULL, error_kind TEXT,
 tools_count INTEGER NOT NULL DEFAULT 0, client_ip TEXT NOT NULL DEFAULT '', client_agent TEXT NOT NULL DEFAULT '',
 tool_names TEXT NOT NULL DEFAULT ''
);`,
		// 复合索引同时覆盖 created_at 范围过滤与 latency_ms 排序，
		// 统计页的 p50/p95 不再需要额外排序；旧的单列索引随之冗余。
		`CREATE INDEX IF NOT EXISTS llm_requests_created_at_latency ON llm_requests(created_at, latency_ms);`,
		`DROP INDEX IF EXISTS llm_requests_created_at;`}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	// 旧库升级：后补列逐条追加。SQLite 没有 ADD COLUMN IF NOT EXISTS，
	// 两个方言各自报"列已存在"，按错误文本幂等跳过。
	for _, column := range []string{
		`ALTER TABLE llm_requests ADD COLUMN tools_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE llm_requests ADD COLUMN client_ip TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE llm_requests ADD COLUMN client_agent TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE llm_requests ADD COLUMN tool_names TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.ExecContext(ctx, column); err != nil {
			msg := err.Error()
			if !strings.Contains(msg, "duplicate column") && !strings.Contains(msg, "already exists") {
				return err
			}
		}
	}
	// 凭据表与账本同批建立：SQLite 不支持 ADD COLUMN IF NOT EXISTS，
	// 而 zcode_credentials 的列一次到位、后续不再变，故只需 CREATE IF NOT EXISTS。
	if err := ensureZcodeCredentialTable(ctx, db); err != nil {
		return err
	}
	return nil
}

// Usage 仅保存上游明确返回的数字；避免用不同 tokenizer 的估算值混入账本。
type Usage struct {
	Prompt, Completion, Total int64
	Source                    string
}
type Record struct {
	ID, Route, UpstreamID, UpstreamModel, Endpoint, ErrorKind string
	Status, LatencyMS, Attempt                                int
	Streamed                                                  bool
	Usage                                                     Usage
	// ToolsCount 是下游请求携带的 tools 数组长度（请求侧信号，不含响应侧
	// 的 tool_calls 次数——流式透传不解析内容，记不到也不该猜）。
	ToolsCount int
	// ToolNames 是请求侧工具名 JSON 数组文本；ClientIP/ClientAgent 是调用
	// 方来源信息。三者都是请求元数据，不含 prompt 与回复内容。
	ToolNames, ClientIP, ClientAgent string
	// CreatedAt 在记录生成时确定，避免异步批量落盘时时间戳整体后移。
	CreatedAt time.Time
}

func (s *Store) Insert(ctx context.Context, r Record) error {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = s.now()
	}
	if s.queue == nil {
		_, err := s.db.ExecContext(ctx, s.insertQuery, recordArgs(r)...)
		if err != nil {
			s.writeErrors.Add(1)
		}
		return err
	}
	s.enqueueMu.RLock()
	defer s.enqueueMu.RUnlock()
	if s.closed.Load() {
		s.dropped.Add(1)
		return errLedgerClosed
	}
	select {
	case s.queue <- r:
		return nil
	default:
		s.dropped.Add(1)
		return errLedgerQueueFull
	}
}

func recordArgs(r Record) []any {
	return []any{r.ID, r.CreatedAt.UTC().Format(time.RFC3339Nano), r.Route, r.UpstreamID, r.UpstreamModel, r.Endpoint, r.Status, r.LatencyMS, boolInt(r.Streamed), r.Attempt, r.Usage.Prompt, r.Usage.Completion, r.Usage.Total, r.Usage.Source, r.ErrorKind, r.ToolsCount, r.ClientIP, r.ClientAgent, r.ToolNames}
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// Store 将同一份调用账本逻辑用于 SQLite 和 PostgreSQL，仅按方言改写占位符。
type Store struct {
	db          *sql.DB
	rebind      func(string) string
	insertQuery string
	queue       chan Record
	stop        chan struct{}
	done        chan struct{}
	closeOnce   sync.Once
	enqueueMu   sync.RWMutex
	batchSize   int
	flushEvery  time.Duration
	now         func() time.Time
	dropped     atomic.Uint64
	writeErrors atomic.Uint64
	closed      atomic.Bool

	summaryMu     sync.Mutex
	summaryLoadMu sync.Mutex
	summaryCache  map[string]summaryCacheEntry
}

// closeRows 忽略 Close 错误：迭代错误统一由 rows.Err() 上报。
func closeRows(rows *sql.Rows) { _ = rows.Close() }

func NewStore(db *sql.DB, driver string) *Store {
	return NewStoreWithConfig(db, DatabaseConfig{Driver: driver})
}

func NewStoreWithConfig(db *sql.DB, cfg DatabaseConfig) *Store {
	driver := cfg.Driver
	store := &Store{db: db, now: time.Now, rebind: func(query string) string { return query }}
	if driver == "postgres" {
		store.rebind = postgresPlaceholders
	}
	store.insertQuery = store.rebind(`INSERT INTO llm_requests VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if cfg.AsyncWrite {
		store.batchSize = cfg.BatchSize
		store.flushEvery = cfg.FlushInterval
		store.queue = make(chan Record, cfg.QueueSize)
		store.stop = make(chan struct{})
		store.done = make(chan struct{})
		go store.run()
	}
	return store
}

func (s *Store) run() {
	defer close(s.done)
	ticker := time.NewTicker(s.flushEvery)
	defer ticker.Stop()
	batch := make([]Record, 0, s.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.flush(batch)
		batch = batch[:0]
	}
	for {
		select {
		case record := <-s.queue:
			batch = append(batch, record)
			if len(batch) >= s.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-s.stop:
			for {
				select {
				case record := <-s.queue:
					batch = append(batch, record)
					if len(batch) >= s.batchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func (s *Store) flush(records []Record) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.writeErrors.Add(uint64(len(records)))
		return
	}
	stmt, err := tx.PrepareContext(ctx, s.insertQuery)
	if err != nil {
		_ = tx.Rollback()
		s.writeErrors.Add(uint64(len(records)))
		return
	}
	for _, record := range records {
		if _, err := stmt.ExecContext(ctx, recordArgs(record)...); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			s.writeErrors.Add(uint64(len(records)))
			return
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		s.writeErrors.Add(uint64(len(records)))
	}
}

func (s *Store) Close(ctx context.Context) error {
	if s.queue == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.enqueueMu.Lock()
		s.closed.Store(true)
		close(s.stop)
		s.enqueueMu.Unlock()
	})
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) Dropped() uint64     { return s.dropped.Load() }
func (s *Store) WriteErrors() uint64 { return s.writeErrors.Load() }

func postgresPlaceholders(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 16)
	index := 1
	for _, char := range query {
		if char != '?' {
			b.WriteRune(char)
			continue
		}
		_, _ = fmt.Fprintf(&b, "$%d", index)
		index++
	}
	return b.String()
}
