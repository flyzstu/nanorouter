package agybridge

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	json "github.com/goccy/go-json"
	"go.uber.org/zap"
)

// ErrNoResult 表示 agy 正常退出但没有给出 result 信封。
var ErrNoResult = errors.New("agy produced no result event")

// AuthError 表示 agy 明确要求重新登录。
type AuthError struct {
	Message string
}

func (e *AuthError) Error() string {
	if e.Message == "" {
		return "agy CLI is not authenticated"
	}
	return "agy CLI is not authenticated: " + e.Message
}

// Runner 按请求启动一个短生命周期的 agy 进程。
type Runner struct {
	Bin      string
	WorkDir  string
	Mode     string
	Effort   string
	Timeout  time.Duration
	ExtraEnv []string
	Logger   *zap.Logger
}

// RunRequest 是一次 agy 调用需要的输入。
type RunRequest struct {
	Prompt         string
	Model          string
	ConversationID string
}

// Run 启动 agy 并把解析出的事件逐个交给 onEvent。
func (r *Runner) Run(ctx context.Context, req RunRequest, onEvent func(Event) error) (err error) {
	if strings.TrimSpace(r.Bin) == "" {
		return errors.New("agy binary is not configured")
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	minutes := int(timeout.Minutes() + 0.999)
	if minutes < 1 {
		minutes = 1
	}
	started := time.Now()
	logger := r.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	logger.Info("agy_start",
		zap.String("request_id", requestIDFrom(ctx)),
		zap.String("model", req.Model),
		zap.String("mode", r.Mode),
		zap.Int("prompt_bytes", len(req.Prompt)),
		zap.Bool("conversation", req.ConversationID != ""),
		zap.Duration("timeout", timeout),
	)
	args := []string{
		"--output-format", "stream-json",
		"--print-timeout", fmt.Sprintf("%dm", minutes),
	}
	switch r.Mode {
	case "", "plan":
		args = append(args, "--mode", "plan")
	case "skip":
		args = append(args, "--dangerously-skip-permissions")
	default:
		args = append(args, "--mode", r.Mode)
	}
	if strings.TrimSpace(req.Model) != "" {
		args = append(args, "--model", strings.TrimSpace(req.Model))
	}
	if strings.TrimSpace(r.Effort) != "" && (req.Model == "" || strings.HasPrefix(strings.ToLower(req.Model), "gemini")) {
		args = append(args, "--effort", strings.TrimSpace(r.Effort))
	}
	if strings.TrimSpace(req.ConversationID) != "" {
		args = append(args, "--conversation", strings.TrimSpace(req.ConversationID))
	}

	var stdin string
	if argvSize(args)+len(req.Prompt) > 24_000 {
		args = append(args, "--input-format", "stream-json")
		stdin = buildStreamInputLine(req.Prompt)
	} else {
		args = append(args, "-p", req.Prompt)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, r.Bin, args...)
	if r.WorkDir != "" {
		cmd.Dir = r.WorkDir
	}
	cmd.Env = append(os.Environ(), r.ExtraEnv...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	configureProcess(cmd)
	cmd.Cancel = func() error { return killProcess(cmd) }
	cmd.WaitDelay = 2 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &tailBuffer{max: 16 << 10}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	parser := &StreamParser{}
	var result *Event
	defer func() {
		exitCode := 0
		if err != nil {
			exitCode = -1
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exitCode = exitErr.ExitCode()
			}
		}
		fields := []zap.Field{
			zap.String("request_id", requestIDFrom(ctx)),
			zap.Duration("duration", time.Since(started)),
			zap.Int("lines", parser.Lines),
			zap.Int("garbage", parser.Garbage),
			zap.Bool("auth_failure", parser.AuthFailure),
			zap.Bool("has_result", result != nil),
			zap.Int("exit_code", exitCode),
			zap.Error(err),
		}
		if result != nil {
			fields = append(fields, zap.Bool("result_ok", result.OK), zap.Int("response_bytes", len(result.Response)))
		}
		if err != nil {
			logger.Warn("agy_finish", fields...)
		} else {
			logger.Info("agy_finish", fields...)
		}
	}()
	scanErr := error(nil)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64<<10), maxStreamLineBytes)
	for scanner.Scan() {
		line := append(append([]byte(nil), scanner.Bytes()...), '\n')
		for _, ev := range parser.Feed(line) {
			if ev.Kind == EventResult {
				copied := ev
				result = &copied
			}
			if err := onEvent(ev); err != nil {
				scanErr = err
				_ = killProcess(cmd)
				break
			}
		}
		if scanErr != nil {
			break
		}
	}
	if scanErr == nil {
		scanErr = scanner.Err()
	}
	for _, ev := range parser.Flush() {
		if ev.Kind == EventResult {
			copied := ev
			result = &copied
		}
		if err := onEvent(ev); err != nil && scanErr == nil {
			scanErr = err
		}
	}
	waitErr := cmd.Wait()
	if scanErr != nil {
		return scanErr
	}
	if runCtx.Err() != nil {
		return runCtx.Err()
	}
	if parser.AuthFailure && (result == nil || !result.OK) {
		msg := parser.AuthFailureText
		if msg == "" {
			msg = firstLine(stderr.String())
		}
		return &AuthError{Message: msg}
	}
	if result != nil && !result.OK && result.Response == "" {
		msg := result.Error
		if msg == "" {
			msg = "agy returned an error result"
		}
		return &ResultError{Message: msg}
	}
	if waitErr != nil {
		if result != nil && (result.OK || result.Response != "") {
			return nil
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.Join(parser.Tail(), "\n")
		}
		if msg != "" {
			return fmt.Errorf("agy exited: %v: %s", waitErr, firstLine(msg))
		}
		return fmt.Errorf("agy exited: %w", waitErr)
	}
	if result == nil {
		return ErrNoResult
	}
	return nil
}

func argvSize(args []string) int {
	size := 0
	for _, arg := range args {
		size += len(arg) + 1
	}
	return size
}

func buildStreamInputLine(prompt string) string {
	payload := map[string]any{
		"event":   "user",
		"message": map[string]any{"role": "user", "content": prompt},
	}
	encoded, _ := json.Marshal(payload)
	return string(encoded) + "\n"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(bytes.TrimSpace(b.buf)))
}
