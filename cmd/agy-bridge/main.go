// Command agy-bridge 把本地 agy CLI 包装成 OpenAI / Anthropic 兼容的流式 HTTP 服务。
//
// 它不嵌入 nanorouter：作为独立 sidecar 运行，nanorouter 通过普通 HTTP 候选接入。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nanorouter/nanorouter/internal/agybridge"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	version = "dev"
	logger  = newLogger()
)

func newLogger() *zap.Logger {
	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encoderCfg.EncodeLevel = zapcore.CapitalLevelEncoder
	encoderCfg.EncodeDuration = zapcore.StringDurationEncoder
	return zap.New(
		zapcore.NewCore(zapcore.NewConsoleEncoder(encoderCfg), zapcore.Lock(os.Stderr), zap.InfoLevel),
		zap.AddCaller(),
	)
}

func main() {
	var (
		showVersion   = flag.Bool("version", false, "print version and exit")
		profile       = flag.String("profile", envString("AGY_BRIDGE_PROFILE", "model"), "bridge profile: model (transparent, no tools) or agent")
		listen        = flag.String("listen", envString("AGY_BRIDGE_LISTEN", "127.0.0.1:8090"), "HTTP listen address")
		agyBin        = flag.String("agy-bin", envString("AGY_BIN", "agy"), "path to the agy binary")
		mode          = flag.String("mode", envString("AGY_BRIDGE_MODE", "plan"), "agy permission mode: plan, accept-edits or skip")
		effort        = flag.String("effort", envString("AGY_BRIDGE_EFFORT", ""), "optional agy reasoning effort")
		workDir       = flag.String("workdir", envString("AGY_BRIDGE_WORKDIR", ""), "working directory for agy (default: isolated temp dir)")
		timeout       = flag.Duration("timeout", envDuration("AGY_BRIDGE_TIMEOUT", 10*time.Minute), "per-request timeout")
		token         = flag.String("token", envString("AGY_BRIDGE_TOKEN", ""), "optional bearer token required from clients")
		models        = flag.String("models", envString("AGY_BRIDGE_MODELS", ""), "comma-separated model IDs exposed by /v1/models")
		defaultModel  = flag.String("default-model", envString("AGY_BRIDGE_DEFAULT_MODEL", ""), "model used when the request omits model")
		emitThinking  = flag.Bool("emit-thinking", envBool("AGY_BRIDGE_EMIT_THINKING", true), "stream agy thinking as reasoning_content (OpenAI only)")
		toolMode      = flag.String("tool-mode", envString("AGY_BRIDGE_TOOL_MODE", "annotate"), "agent tool activity mapping: annotate, passthrough or none")
		toolBridge    = flag.String("tool-bridge", envString("AGY_BRIDGE_TOOL_BRIDGE", "prompt"), "model profile tool handling: prompt (inject schemas + parse tool_call JSON) or off")
		sessionFile   = flag.String("session-file", envString("AGY_BRIDGE_SESSION_FILE", ""), "optional JSON file persisting session -> agy conversation bindings")
		sessionTTL    = flag.Duration("session-ttl", envDuration("AGY_BRIDGE_SESSION_TTL", 24*time.Hour), "session binding TTL")
		allowInsecure = flag.Bool("allow-insecure", envBool("AGY_BRIDGE_ALLOW_INSECURE", false), "allow non-loopback listen without a token")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("agy-bridge version %s\n", version)
		return
	}

	defer func() { _ = logger.Sync() }()

	if *profile != "model" && *profile != "agent" {
		logger.Fatal("invalid -profile", zap.String("profile", *profile), zap.String("want", "model or agent"))
	}
	switch *mode {
	case "plan", "accept-edits", "skip":
	default:
		logger.Fatal("invalid -mode", zap.String("mode", *mode), zap.String("want", "plan, accept-edits or skip"))
	}
	switch *toolMode {
	case "annotate", "passthrough", "none":
	default:
		logger.Fatal("invalid -tool-mode", zap.String("tool_mode", *toolMode), zap.String("want", "annotate, passthrough or none"))
	}
	switch *toolBridge {
	case "prompt", "off":
	default:
		logger.Fatal("invalid -tool-bridge", zap.String("tool_bridge", *toolBridge), zap.String("want", "prompt or off"))
	}
	if *profile == "model" {
		// 透明转发：下游编程 agent 拥有工具循环，bridge 只当无状态模型。
		if *mode != "plan" {
			logger.Info("profile=model forces -mode plan", zap.String("ignored", *mode))
		}
		*mode = "plan"
		if *toolMode != "none" {
			logger.Info("profile=model forces -tool-mode none", zap.String("ignored", *toolMode))
		}
		*toolMode = "none"
		if *sessionFile != "" {
			logger.Info("profile=model disables session bindings", zap.String("ignored", "-session-file"))
		}
		*sessionFile = ""
		if *workDir != "" {
			logger.Info("profile=model ignores -workdir", zap.String("ignored", *workDir))
		}
		*workDir = ""
	}
	if *token == "" && !*allowInsecure && !isLoopbackListen(*listen) {
		logger.Fatal("refusing to listen without a token", zap.String("listen", *listen))
	}
	if *workDir == "" {
		dir, err := os.MkdirTemp("", "agy-bridge-*")
		if err != nil {
			logger.Fatal("create temp workdir failed", zap.Error(err))
		}
		defer func() { _ = os.RemoveAll(dir) }()
		*workDir = dir
	} else if info, err := os.Stat(*workDir); err != nil || !info.IsDir() {
		logger.Fatal("workdir is not a directory", zap.String("workdir", *workDir))
	}

	modelList := splitModels(*models)
	cfg := agybridge.Config{
		Profile:      *profile,
		Bin:          *agyBin,
		WorkDir:      *workDir,
		Mode:         *mode,
		Effort:       *effort,
		Timeout:      *timeout,
		Token:        *token,
		Models:       modelList,
		DefaultModel: *defaultModel,
		EmitThinking: *emitThinking,
		ToolMode:     *toolMode,
		ToolBridge:   *toolBridge,
		SessionFile:  *sessionFile,
		SessionTTL:   *sessionTTL,
		Logger:       logger,
	}
	bridge := agybridge.NewBridge(cfg)
	server := &http.Server{
		Addr:              *listen,
		Handler:           bridge.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	logger.Info("agy-bridge listening",
		zap.String("listen", *listen),
		zap.String("profile", *profile),
		zap.String("bin", *agyBin),
		zap.String("mode", *mode),
		zap.String("tool_mode", *toolMode),
		zap.String("tool_bridge", *toolBridge),
		zap.String("workdir", *workDir),
		zap.Strings("models", bridgeModels(cfg)),
	)
	runErr := make(chan error, 1)
	go func() {
		runErr <- server.ListenAndServe()
	}()

	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-runErr:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal("server stopped", zap.Error(err))
		}
	case <-signals.Done():
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			logger.Error("shutdown failed", zap.Error(err))
		}
	}
}

func bridgeModels(cfg agybridge.Config) []string {
	if len(cfg.Models) > 0 {
		return cfg.Models
	}
	if cfg.DefaultModel != "" {
		return []string{cfg.DefaultModel}
	}
	return []string{"agy-default"}
}

func splitModels(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		logger.Fatal("invalid duration", zap.String("env", name), zap.Error(err))
	}
	return parsed
}

func envBool(name string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" || host == "localhost" {
		return host == "localhost"
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
