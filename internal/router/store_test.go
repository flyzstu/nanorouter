package router

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
)

// TestAsyncLedgerKeepsInsertTime 验证异步批量写不会把 created_at 推迟到 flush 时刻：
// 记录在入队时确定时间戳，落库后读回的时间应接近入队时刻而非关闭（flush）时刻。
func TestAsyncLedgerKeepsInsertTime(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	// FlushInterval 足够大，保证记录入队后、Close 排空前不会因 ticker 提前落库。
	store := NewStoreWithConfig(db, DatabaseConfig{Driver: "sqlite", AsyncWrite: true, BatchSize: 100, FlushInterval: 5 * time.Second, QueueSize: 10})
	enqueuedAt := time.Date(2026, 8, 4, 12, 0, 0, 123000000, time.UTC)
	store.now = func() time.Time { return enqueuedAt }
	if err := store.Insert(context.Background(), Record{ID: "r1", Route: "free", UpstreamID: "u", UpstreamModel: "up", Endpoint: "chat_completions", Status: 200, LatencyMS: 10, Usage: Usage{Source: "unavailable"}}); err != nil {
		t.Fatal(err)
	}
	// 模拟 flush 时钟已经前进；如果 recordArgs 重新取当前时间，测试会失败。
	store.now = func() time.Time { return enqueuedAt.Add(2 * time.Second) }
	// Close 触发排空。
	if err := store.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := db.QueryRow(`SELECT created_at FROM llm_requests WHERE id = 'r1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, stored)
	if err != nil {
		t.Fatal(err)
	}
	if !createdAt.Equal(enqueuedAt) {
		t.Fatalf("created_at=%s want=%s, timestamp was changed during flush", createdAt, enqueuedAt)
	}
}
