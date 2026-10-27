package main

import (
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestTextEncoderUsesKeyValueFields(t *testing.T) {
	encoder := newTextEncoder(false)
	buf, err := encoder.EncodeEntry(zapcore.Entry{Time: time.Date(2026, 7, 15, 21, 52, 16, 0, time.FixedZone("CST", 8*3600)), Level: zapcore.InfoLevel, Message: "Upstream request completed"}, []zapcore.Field{zap.Int("status", 200), zap.String("route", "auto")})
	if err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.HasPrefix(got, "2026-07-15 21:52:16.000 +08:00\t") {
		t.Fatalf("human-readable timestamp missing: %q", got)
	}
	if !strings.Contains(got, "\tINFO\tUpstream request completed route=\"auto\" status=200\n") {
		t.Fatalf("log=%q", got)
	}
	if strings.Contains(got, "{") {
		t.Fatalf("log should not include JSON fields: %q", got)
	}
}

func TestTextEncoderColorsLevel(t *testing.T) {
	encoder := newTextEncoder(true)
	buf, err := encoder.EncodeEntry(zapcore.Entry{Time: time.Now(), Level: zapcore.WarnLevel, Message: "warning"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "\x1b[33mWARN\x1b[0m") {
		t.Fatalf("log=%q", buf.String())
	}
}

func TestTextEncoderPrioritizesAndColorsModelFields(t *testing.T) {
	encoder := newTextEncoder(true)
	buf, err := encoder.EncodeEntry(
		zapcore.Entry{Time: time.Now(), Level: zapcore.InfoLevel, Message: "attempt"},
		[]zapcore.Field{
			zap.Int("attempt", 1),
			zap.String("upstream_model", "deepseek-chat"),
			zap.String("route", "free-fast"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	// upstream_model 与 route 需优先且带颜色打印，upstream_model 位于最前面
	expected := "upstream_model=\x1b[36;1m\"deepseek-chat\"\x1b[0m route=\x1b[36;1m\"free-fast\"\x1b[0m attempt=1"
	if !strings.Contains(got, expected) {
		t.Fatalf("expected log to contain %q, got %q", expected, got)
	}
}

func TestLoggerUsesConfiguredLevel(t *testing.T) {
	logger, err := newLogger("WARN", "text", true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Sync() }()
	if logger.Core().Enabled(zapcore.InfoLevel) {
		t.Fatal("INFO should be disabled at WARN level")
	}
	if !logger.Core().Enabled(zapcore.WarnLevel) || !logger.Core().Enabled(zapcore.ErrorLevel) {
		t.Fatal("WARN and ERROR should be enabled at WARN level")
	}
}

func TestDebugFlagOverridesConfiguredLevel(t *testing.T) {
	logger, err := newLogger("ERROR", "json", false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Sync() }()
	if !logger.Core().Enabled(zapcore.DebugLevel) {
		t.Fatal("debug flag should enable DEBUG logs")
	}
}
