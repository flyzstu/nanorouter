package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

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

	cfg, err := router.LoadConfig(*configPath)
	if err != nil {
		logger, _ := zap.NewProduction()
		logger.Fatal("load configuration failed", zap.Error(err))
	}
	logger, err := newLogger(cfg.Logging.Level, cfg.Logging.Format, cfg.Logging.ColorEnabled(), *debug)
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	message := func(english, chinese string) string {
		if cfg.Logging.Language == "zh-CN" {
			return chinese
		}
		return english
	}
	dsn, err := cfg.Database.ResolveDSN()
	if err != nil {
		logger.Fatal(message("resolve database DSN failed", "解析数据库 DSN 失败"), zap.Error(err))
	}
	db, err := sql.Open(cfg.Database.SQLDriver(), dsn)
	if err != nil {
		logger.Fatal(message("open database failed", "打开数据库失败"), zap.Error(err))
	}
	defer db.Close()
	if err := router.Migrate(context.Background(), db); err != nil {
		logger.Fatal(message("migrate database failed", "迁移数据库失败"), zap.Error(err))
	}

	srv, err := router.NewWithLogger(cfg, db, logger)
	if err != nil {
		logger.Fatal(message("create router failed", "创建路由器失败"), zap.Error(err))
	}
	server := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	logger.Info(message("nanorouter started", "nanorouter 已启动"), zap.String("listen", cfg.Listen), zap.String("database_driver", cfg.Database.Driver))
	runErr := make(chan error, 1)
	go func() { runErr <- server.ListenAndServe() }()
	shutdownSignal, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	select {
	case err := <-runErr:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error(message("server stopped unexpectedly", "服务器意外停止"), zap.Error(err))
		}
	case <-shutdownSignal.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error(message("HTTP shutdown failed", "HTTP 服务关闭失败"), zap.Error(err))
		}
		cancel()
	}
	ledgerCtx, cancelLedger := context.WithTimeout(context.Background(), 5*time.Second)
	if err := srv.Close(ledgerCtx); err != nil {
		logger.Error(message("ledger flush failed", "账本排空失败"), zap.Error(err))
	}
	cancelLedger()
}

func newLogger(levelName, format string, color, debug bool) (*zap.Logger, error) {
	level := zap.InfoLevel
	if err := level.UnmarshalText([]byte(levelName)); err != nil {
		return nil, err
	}
	if debug {
		level = zap.DebugLevel
		return zap.New(zapcore.NewCore(newTextEncoder(true), zapcore.Lock(os.Stderr), level)), nil
	}
	if format == "text" {
		return zap.New(zapcore.NewCore(newTextEncoder(color), zapcore.Lock(os.Stderr), level)), nil
	}
	logConfig := zap.NewProductionConfig()
	logConfig.EncoderConfig.TimeKey = "time"
	logConfig.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	logConfig.EncoderConfig.LevelKey = "level"
	logConfig.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
	logConfig.DisableCaller = true
	logConfig.DisableStacktrace = true
	logConfig.Level = zap.NewAtomicLevelAt(level)
	return logConfig.Build()
}
