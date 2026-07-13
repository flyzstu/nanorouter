package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"net/http"
	"time"

	"github.com/nanorouter/nanorouter/internal/router"
	_ "github.com/ncruces/go-sqlite3/driver"
	"go.uber.org/zap"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML configuration")
	flag.Parse()
	logger, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	cfg, err := router.LoadConfig(*configPath)
	if err != nil {
		logger.Fatal("load configuration failed", zap.Error(err))
	}
	db, err := sql.Open("sqlite3", cfg.Database)
	if err != nil {
		logger.Fatal("open sqlite failed", zap.Error(err))
	}
	defer db.Close()
	if err := router.Migrate(context.Background(), db); err != nil {
		logger.Fatal("migrate sqlite failed", zap.Error(err))
	}

	srv, err := router.NewWithLogger(cfg, db, logger)
	if err != nil {
		logger.Fatal("create router failed", zap.Error(err))
	}
	server := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	logger.Info("nanorouter started", zap.String("listen", cfg.Listen), zap.String("database", cfg.Database))
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		logger.Fatal("server stopped unexpectedly", zap.Error(err))
	}
}
