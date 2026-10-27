package router

import (
	"database/sql"

	"go.uber.org/zap"
)

// New 与 NewStore 仅供测试使用：生产入口显式注入 logger 与数据库配置。
func New(cfg Config, db *sql.DB) (*Router, error) {
	return NewWithLogger(cfg, db, zap.NewNop())
}

func NewStore(db *sql.DB, driver string) *Store {
	return NewStoreWithConfig(db, DatabaseConfig{Driver: driver})
}
