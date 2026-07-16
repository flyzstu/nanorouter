package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"net/http"
	"os"
	"time"

	_ "github.com/lib/pq"
	"github.com/nanorouter/nanorouter/internal/router"
	_ "github.com/ncruces/go-sqlite3/driver"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML configuration")
	debug := flag.Bool("debug", false, "enable colored console debug logs")
	flag.Parse()
	logConfig := zap.NewProductionConfig()
	logConfig.EncoderConfig.TimeKey = "time"
	logConfig.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	logConfig.EncoderConfig.LevelKey = "level"
	logConfig.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
	logConfig.DisableCaller = true
	logConfig.DisableStacktrace = true
	var logger *zap.Logger
	var err error
	if *debug {
		logger = zap.New(zapcore.NewCore(newTextEncoder(true), zapcore.Lock(os.Stderr), zap.DebugLevel))
	} else {
		logger, err = logConfig.Build()
	}
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	cfg, err := router.LoadConfig(*configPath)
	if err != nil {
		logger.Fatal("load configuration failed", zap.Error(err))
	}
	dsn, err := cfg.Database.ResolveDSN()
	if err != nil {
		logger.Fatal("resolve database DSN failed", zap.Error(err))
	}
	db, err := sql.Open(cfg.Database.SQLDriver(), dsn)
	if err != nil {
		logger.Fatal("open sqlite failed", zap.Error(err))
	}
	defer db.Close()
	if err := router.Migrate(context.Background(), db); err != nil {
		logger.Fatal("migrate database failed", zap.Error(err))
	}

	srv, err := router.NewWithLogger(cfg, db, logger)
	if err != nil {
		logger.Fatal("create router failed", zap.Error(err))
	}
	server := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	logger.Info("nanorouter started", zap.String("listen", cfg.Listen), zap.String("database_driver", cfg.Database.Driver))
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		logger.Fatal("server stopped unexpectedly", zap.Error(err))
	}
}
