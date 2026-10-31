// Command agy-bridge 把本地 agy CLI 包装成 OpenAI / Anthropic 兼容的流式 HTTP 服务。
//
// 它不嵌入 nanorouter：作为独立 sidecar 运行，nanorouter 通过普通 HTTP 候选接入。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nanorouter/nanorouter/internal/agybridge"
)

func main() {
	var (
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
		toolMode      = flag.String("tool-mode", envString("AGY_BRIDGE_TOOL_MODE", "annotate"), "tool activity mapping: annotate, passthrough or none")
		sessionFile   = flag.String("session-file", envString("AGY_BRIDGE_SESSION_FILE", ""), "optional JSON file persisting session -> agy conversation bindings")
		sessionTTL    = flag.Duration("session-ttl", envDuration("AGY_BRIDGE_SESSION_TTL", 24*time.Hour), "session binding TTL")
		allowInsecure = flag.Bool("allow-insecure", envBool("AGY_BRIDGE_ALLOW_INSECURE", false), "allow non-loopback listen without a token")
	)
	flag.Parse()

	if *profile != "model" && *profile != "agent" {
		log.Fatalf("invalid -profile %q: want model or agent", *profile)
	}
	switch *mode {
	case "plan", "accept-edits", "skip":
	default:
		log.Fatalf("invalid -mode %q: want plan, accept-edits or skip", *mode)
	}
	switch *toolMode {
	case "annotate", "passthrough", "none":
	default:
		log.Fatalf("invalid -tool-mode %q: want annotate, passthrough or none", *toolMode)
	}
	if *profile == "model" {
		// 透明转发：下游编程 agent 拥有工具循环，bridge 只当无状态模型。
		if *mode != "plan" {
			log.Printf("profile=model forces -mode plan (ignoring %q)", *mode)
		}
		*mode = "plan"
		if *toolMode != "none" {
			log.Printf("profile=model forces -tool-mode none (ignoring %q)", *toolMode)
		}
		*toolMode = "none"
		if *sessionFile != "" {
			log.Printf("profile=model disables session bindings (ignoring -session-file)")
		}
		*sessionFile = ""
		if *workDir != "" {
			log.Printf("profile=model ignores -workdir and uses an isolated temp dir")
		}
		*workDir = ""
	}
	if *token == "" && !*allowInsecure && !isLoopbackListen(*listen) {
		log.Fatalf("refusing to listen on %s without -token; bind 127.0.0.1 or pass -allow-insecure", *listen)
	}
	if *workDir == "" {
		dir, err := os.MkdirTemp("", "agy-bridge-*")
		if err != nil {
			log.Fatalf("create temp workdir: %v", err)
		}
		defer func() { _ = os.RemoveAll(dir) }()
		*workDir = dir
	} else if info, err := os.Stat(*workDir); err != nil || !info.IsDir() {
		log.Fatalf("workdir %q is not a directory", *workDir)
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
		SessionFile:  *sessionFile,
		SessionTTL:   *sessionTTL,
	}
	bridge := agybridge.NewBridge(cfg)
	server := &http.Server{
		Addr:              *listen,
		Handler:           bridge.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("agy-bridge listening on %s (profile=%s bin=%s mode=%s tool=%s workdir=%s models=%s)", *listen, *profile, *agyBin, *mode, *toolMode, *workDir, strings.Join(bridgeModels(cfg), ","))
	runErr := make(chan error, 1)
	go func() {
		runErr <- server.ListenAndServe()
	}()

	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-runErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server stopped: %v", err)
		}
	case <-signals.Done():
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
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
		log.Fatalf("invalid %s: %v", name, err)
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
