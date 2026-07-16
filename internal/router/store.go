package router

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

func Migrate(ctx context.Context, db *sql.DB) error {
	statements := []string{`CREATE TABLE IF NOT EXISTS llm_requests (
 id TEXT PRIMARY KEY, created_at TEXT NOT NULL, route TEXT NOT NULL, upstream_id TEXT NOT NULL, upstream_model TEXT NOT NULL,
 endpoint TEXT NOT NULL, status_code INTEGER NOT NULL, latency_ms INTEGER NOT NULL, streamed INTEGER NOT NULL, attempt INTEGER NOT NULL,
 prompt_tokens BIGINT, completion_tokens BIGINT, total_tokens BIGINT, usage_source TEXT NOT NULL, error_kind TEXT
);`, `CREATE INDEX IF NOT EXISTS llm_requests_created_at ON llm_requests(created_at);`}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return err
		}
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
}

func (s *Store) Insert(ctx context.Context, r Record) {
	_, _ = s.db.ExecContext(ctx, s.rebind(`INSERT INTO llm_requests VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`), r.ID, time.Now().UTC().Format(time.RFC3339Nano), r.Route, r.UpstreamID, r.UpstreamModel, r.Endpoint, r.Status, r.LatencyMS, boolInt(r.Streamed), r.Attempt, r.Usage.Prompt, r.Usage.Completion, r.Usage.Total, r.Usage.Source, r.ErrorKind)
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// Store 将同一份调用账本逻辑用于 SQLite 和 PostgreSQL，仅按方言改写占位符。
type Store struct {
	db     *sql.DB
	rebind func(string) string
}

func NewStore(db *sql.DB, driver string) *Store {
	store := &Store{db: db, rebind: func(query string) string { return query }}
	if driver == "postgres" {
		store.rebind = postgresPlaceholders
	}
	return store
}

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
