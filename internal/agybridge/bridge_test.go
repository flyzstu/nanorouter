package agybridge

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const fakeStream = `{"event":"init","conversation_id":"c1","model":"fake-model"}
{"event":"step_update","idx":1,"step_type":"text","text":"Hello"}
{"event":"step_update","idx":1,"step_type":"text","text":"Hello world"}
{"event":"step_update","idx":2,"step_type":"thinking","text":"Let me think"}
{"event":"result","result":{"conversation_id":"c1","status":"DONE","response":"Hello world","usage":{"input_tokens":3,"output_tokens":2,"thinking_tokens":1}}}`

func fakeAgy(t *testing.T, body string) string {
	return fakeAgyScript(t, `if [ -n "$FAKE_ARGS_FILE" ]; then printf '%s\n' "$@" > "$FAKE_ARGS_FILE"; fi
cat <<'EOF'
`+body+`
EOF`)
}

func fakeAgyScript(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake agy uses a POSIX shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "agy")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testBridge(t *testing.T, bin, token string) *httptest.Server {
	return testBridgeWithConfig(t, Config{Bin: bin, Token: token})
}

func testBridgeWithToolMode(t *testing.T, bin, token, toolMode string) *httptest.Server {
	return testBridgeWithConfig(t, Config{Bin: bin, Token: token, ToolMode: toolMode})
}

func testBridgeWithConfig(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	if cfg.Profile == "" {
		cfg.Profile = "agent"
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = t.TempDir()
	}
	if cfg.Mode == "" {
		cfg.Mode = "plan"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if len(cfg.Models) == 0 {
		cfg.Models = []string{"fake-model"}
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = "fake-model"
	}
	if !cfg.EmitThinking {
		cfg.EmitThinking = true
	}
	if cfg.ToolMode == "" {
		cfg.ToolMode = "annotate"
	}
	server := httptest.NewServer(NewBridge(cfg).Handler())
	t.Cleanup(server.Close)
	return server
}

func postJSON(t *testing.T, url, body, token string) *http.Response {
	headers := map[string]string{}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return postJSONWithHeaders(t, url, body, headers)
}

func postJSONWithHeaders(t *testing.T, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestOpenAIStream(t *testing.T) {
	server := testBridge(t, fakeAgy(t, fakeStream), "")
	resp := postJSON(t, server.URL+"/v1/chat/completions", `{
		"model":"fake-model",
		"messages":[{"role":"user","content":"hi"}],
		"stream":true,
		"stream_options":{"include_usage":true}
	}`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{
		`"role":"assistant"`,
		`"content":"Hello"`,
		`"content":" world"`,
		`"reasoning_content":"Let me think"`,
		`"finish_reason":"stop"`,
		`"usage"`,
		`data: [DONE]`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stream missing %q:\n%s", want, text)
		}
	}
}

func TestAnthropicStream(t *testing.T) {
	server := testBridge(t, fakeAgy(t, fakeStream), "")
	resp := postJSON(t, server.URL+"/v1/messages", `{
		"model":"fake-model",
		"max_tokens":16,
		"messages":[{"role":"user","content":"hi"}],
		"stream":true
	}`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{
		"event: message_start",
		`"type":"content_block_start"`,
		`"text":"Hello"`,
		`"text":" world"`,
		"event: message_stop",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stream missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "reasoning_content") {
		t.Fatalf("anthropic stream should not expose reasoning_content:\n%s", text)
	}
}

func TestOpenAINonStreamAndUsage(t *testing.T) {
	server := testBridge(t, fakeAgy(t, fakeStream), "")
	resp := postJSON(t, server.URL+"/v1/chat/completions", `{
		"model":"fake-model",
		"messages":[{"role":"user","content":"hi"}]
	}`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{
		`"content":"Hello world"`,
		`"reasoning_content":"Let me think"`,
		`"prompt_tokens":3`,
		`"completion_tokens":2`,
		`"total_tokens":5`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("response missing %q:\n%s", want, text)
		}
	}
}

func TestBridgeAuth(t *testing.T) {
	server := testBridge(t, fakeAgy(t, fakeStream), "secret")
	resp := postJSON(t, server.URL+"/v1/chat/completions", `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without token status=%d", resp.StatusCode)
	}
	resp = postJSON(t, server.URL+"/v1/chat/completions", `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`, "secret")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("with token status=%d", resp.StatusCode)
	}
}

func TestBridgeAuthFailure(t *testing.T) {
	server := testBridge(t, fakeAgy(t, `{"event":"result","result":{"status":"ERROR","error":"Please sign in"}}`), "")
	resp := postJSON(t, server.URL+"/v1/chat/completions", `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`, "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "agy_auth_required") {
		t.Fatalf("body=%s", body)
	}
}

func TestModelsAndHealth(t *testing.T) {
	server := testBridge(t, fakeAgy(t, fakeStream), "")
	resp, err := http.Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "fake-model") {
		t.Fatalf("models status=%d body=%s", resp.StatusCode, body)
	}
	resp, err = http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status=%d", resp.StatusCode)
	}
}

func TestSessionContinuityUsesConversation(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("FAKE_ARGS_FILE", argsFile)
	server := testBridge(t, fakeAgy(t, fakeStream), "")
	headers := map[string]string{"X-Session-Affinity": "s1"}

	first := `{"model":"fake-model","messages":[{"role":"system","content":"be nice"},{"role":"user","content":"first"}]}`
	resp := postJSONWithHeaders(t, server.URL+"/v1/chat/completions", first, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first status=%d", resp.StatusCode)
	}
	args1, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args1), "--conversation") {
		t.Fatalf("first request should not resume a conversation:\n%s", args1)
	}
	if !strings.Contains(string(args1), "be nice") {
		t.Fatalf("first request should send full prompt:\n%s", args1)
	}

	second := `{"model":"fake-model","messages":[
		{"role":"system","content":"be nice"},
		{"role":"user","content":"first"},
		{"role":"assistant","content":"ok"},
		{"role":"user","content":"second"}
	]}`
	resp = postJSONWithHeaders(t, server.URL+"/v1/chat/completions", second, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second status=%d", resp.StatusCode)
	}
	args2, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(args2)
	if !strings.Contains(text, "--conversation") || !strings.Contains(text, "c1") {
		t.Fatalf("second request should resume c1:\n%s", text)
	}
	if strings.Contains(text, "be nice") {
		t.Fatalf("continuation must not resend system prompt:\n%s", text)
	}
	if !strings.Contains(text, "second") {
		t.Fatalf("continuation missing latest user turn:\n%s", text)
	}
}

const fakeToolStream = `{"event":"init","conversation_id":"c1","model":"fake-model"}
{"event":"step_update","step_index":3,"state":"DONE","step_type":"tool","tool_info":{"name":"run_command","parameters":{"CommandLine":"ls"},"output":"ok"}}
{"event":"step_update","idx":4,"step_type":"text","text":"done"}
{"event":"result","result":{"conversation_id":"c1","status":"DONE","response":"done","usage":{"input_tokens":5,"output_tokens":2}}}`

func TestOpenAIToolPassthrough(t *testing.T) {
	server := testBridgeWithToolMode(t, fakeAgy(t, fakeToolStream), "", "passthrough")
	resp := postJSON(t, server.URL+"/v1/chat/completions", `{
		"model":"fake-model",
		"messages":[{"role":"user","content":"run ls"}],
		"stream":true
	}`, "")
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{
		`"content":"done"`,
		`"tool_calls"`,
		`"name":"run_command"`,
		`CommandLine`,
		`"finish_reason":"tool_calls"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("tool stream missing %q:\n%s", want, text)
		}
	}
}

func TestAnthropicToolPassthrough(t *testing.T) {
	server := testBridgeWithToolMode(t, fakeAgy(t, fakeToolStream), "", "passthrough")
	resp := postJSON(t, server.URL+"/v1/messages", `{
		"model":"fake-model",
		"max_tokens":16,
		"messages":[{"role":"user","content":"run ls"}],
		"stream":true
	}`, "")
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{
		`"type":"tool_use"`,
		`"name":"run_command"`,
		`"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("tool stream missing %q:\n%s", want, text)
		}
	}
}

func TestErrorClassificationRateLimit(t *testing.T) {
	server := testBridge(t, fakeAgy(t, `{"event":"result","result":{"status":"ERROR","error":"429 rate limit exceeded"}}`), "")
	resp := postJSON(t, server.URL+"/v1/chat/completions", `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`, "")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Agy-Error-Kind"); got != "rate_limit" {
		t.Fatalf("kind=%q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "agy_rate_limited") {
		t.Fatalf("body=%s", body)
	}
}

func TestErrorClassificationContextWindow(t *testing.T) {
	server := testBridge(t, fakeAgy(t, `{"event":"result","result":{"status":"ERROR","error":"prompt is too long: context_length_exceeded"}}`), "")
	resp := postJSON(t, server.URL+"/v1/messages", `{"model":"fake-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Agy-Error-Kind"); got != "context_window" {
		t.Fatalf("kind=%q", got)
	}
}

func TestOpenAIStreamUsageWithoutFlag(t *testing.T) {
	server := testBridge(t, fakeAgy(t, fakeStream), "")
	resp := postJSON(t, server.URL+"/v1/chat/completions", `{
		"model":"fake-model","messages":[{"role":"user","content":"hi"}],"stream":true
	}`, "")
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"usage"`) {
		t.Fatalf("expected usage chunk:\n%s", body)
	}
}

func TestAnthropicNonStream(t *testing.T) {
	server := testBridge(t, fakeAgy(t, fakeStream), "")
	resp := postJSON(t, server.URL+"/v1/messages", `{
		"model":"fake-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]
	}`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{
		`"content":[{"text":"Hello world","type":"text"}]`,
		`"stop_reason":"end_turn"`,
		`"input_tokens":3`,
		`"output_tokens":2`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("response missing %q:\n%s", want, text)
		}
	}
}

func TestToolModeNoneIgnoresTools(t *testing.T) {
	server := testBridgeWithToolMode(t, fakeAgy(t, fakeToolStream), "", "none")
	resp := postJSON(t, server.URL+"/v1/chat/completions", `{
		"model":"fake-model","messages":[{"role":"user","content":"run ls"}],"stream":true
	}`, "")
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if strings.Contains(text, "tool_calls") {
		t.Fatalf("none mode should not emit tool_calls:\n%s", text)
	}
	if !strings.Contains(text, `"finish_reason":"stop"`) {
		t.Fatalf("expected stop finish:\n%s", text)
	}
}

const fakeTwoTools = `{"event":"init","conversation_id":"c1","model":"fake-model"}
{"event":"step_update","step_index":3,"state":"DONE","step_type":"tool","tool_info":{"name":"run_command","parameters":{"CommandLine":"ls"},"output":"ok"}}
{"event":"step_update","step_index":4,"state":"DONE","step_type":"tool","tool_info":{"name":"view_file","parameters":{"Path":"a.txt"},"output":"content"}}
{"event":"step_update","idx":5,"step_type":"text","text":"done"}
{"event":"result","result":{"conversation_id":"c1","status":"DONE","response":"done","usage":{"input_tokens":5,"output_tokens":2}}}`

func TestMultipleToolPassthrough(t *testing.T) {
	server := testBridgeWithToolMode(t, fakeAgy(t, fakeTwoTools), "", "passthrough")
	resp := postJSON(t, server.URL+"/v1/chat/completions", `{
		"model":"fake-model","messages":[{"role":"user","content":"run"}],"stream":true
	}`, "")
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{`"index":0`, `"index":1`, `"name":"run_command"`, `"name":"view_file"`, `"finish_reason":"tool_calls"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("stream missing %q:\n%s", want, text)
		}
	}

	resp = postJSON(t, server.URL+"/v1/messages", `{
		"model":"fake-model","max_tokens":16,"messages":[{"role":"user","content":"run"}],"stream":true
	}`, "")
	body, _ = io.ReadAll(resp.Body)
	if strings.Count(string(body), `"type":"tool_use"`) != 2 {
		t.Fatalf("expected two tool_use blocks:\n%s", body)
	}
}

func TestErrorClassificationNetwork(t *testing.T) {
	bin := fakeAgyScript(t, `echo 'net/http: TLS handshake timeout' >&2
exit 1`)
	server := testBridge(t, bin, "")
	resp := postJSON(t, server.URL+"/v1/chat/completions", `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`, "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Agy-Error-Kind"); got != "network" {
		t.Fatalf("kind=%q", got)
	}
}

func TestErrorClassificationNoResult(t *testing.T) {
	bin := fakeAgyScript(t, `exit 0`)
	server := testBridge(t, bin, "")
	resp := postJSON(t, server.URL+"/v1/chat/completions", `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`, "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Agy-Error-Kind"); got != "no_result" {
		t.Fatalf("kind=%q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "agy_no_result") {
		t.Fatalf("body=%s", body)
	}
}

func TestErrorClassificationTimeout(t *testing.T) {
	bin := fakeAgyScript(t, `sleep 2`)
	server := testBridgeWithConfig(t, Config{Bin: bin, Timeout: 100 * time.Millisecond})
	resp := postJSON(t, server.URL+"/v1/chat/completions", `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`, "")
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Agy-Error-Kind"); got != "timeout" {
		t.Fatalf("kind=%q", got)
	}
}

func TestErrorClassificationEligibility(t *testing.T) {
	server := testBridge(t, fakeAgy(t, `{"event":"result","result":{"status":"ERROR","error":"Eligibility check failed: not available in your location"}}`), "")
	resp := postJSON(t, server.URL+"/v1/chat/completions", `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Agy-Error-Kind"); got != "eligibility" {
		t.Fatalf("kind=%q", got)
	}
}

func TestAuthXAPIKey(t *testing.T) {
	server := testBridge(t, fakeAgy(t, fakeStream), "secret")
	resp := postJSONWithHeaders(t, server.URL+"/v1/chat/completions", `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-Api-Key": "secret"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	resp = postJSONWithHeaders(t, server.URL+"/v1/chat/completions", `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-Api-Key": "wrong"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong key status=%d", resp.StatusCode)
	}
}

func TestInvalidRequests(t *testing.T) {
	server := testBridge(t, fakeAgy(t, fakeStream), "")
	resp := postJSON(t, server.URL+"/v1/chat/completions", `{`, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid json status=%d", resp.StatusCode)
	}
	resp = postJSON(t, server.URL+"/v1/chat/completions", `{"model":"fake-model","messages":[]}`, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty messages status=%d", resp.StatusCode)
	}
	resp = postJSON(t, server.URL+"/v1/messages", `{"model":"fake-model","max_tokens":16,"messages":[]}`, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty anthropic messages status=%d", resp.StatusCode)
	}
}

func TestStaleConversationRetry(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("FAKE_ARGS_FILE", argsFile)
	bin := fakeAgyScript(t, `if [ -n "$FAKE_ARGS_FILE" ]; then printf '\n=== INVOCATION ===\n' >> "$FAKE_ARGS_FILE"; printf '%s\n' "$@" >> "$FAKE_ARGS_FILE"; fi
case " $* " in
  *" --conversation "*)
    echo '{"event":"result","result":{"status":"ERROR","error":"conversation not found"}}'
    exit 1
    ;;
esac
cat <<'INNER'
`+fakeStream+`
INNER`)
	server := testBridge(t, bin, "")
	headers := map[string]string{"X-Session-Affinity": "stale-session"}

	first := `{"model":"fake-model","messages":[{"role":"user","content":"first"}]}`
	if resp := postJSONWithHeaders(t, server.URL+"/v1/chat/completions", first, headers); resp.StatusCode != http.StatusOK {
		t.Fatalf("first status=%d", resp.StatusCode)
	}
	second := `{"model":"fake-model","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"Hello world"},{"role":"user","content":"second"}]}`
	resp := postJSONWithHeaders(t, server.URL+"/v1/chat/completions", second, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Hello world") {
		t.Fatalf("retry response=%s", body)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--conversation") {
		t.Fatalf("first stale attempt should use conversation:\n%s", args)
	}
	if strings.Count(string(args), "=== INVOCATION ===") < 3 {
		t.Fatalf("expected stale attempt + retry, got:\n%s", args)
	}
}

func TestLongPromptUsesStdin(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	stdinFile := filepath.Join(t.TempDir(), "stdin.txt")
	t.Setenv("FAKE_ARGS_FILE", argsFile)
	t.Setenv("FAKE_STDIN_FILE", stdinFile)
	bin := fakeAgyScript(t, `if [ -n "$FAKE_ARGS_FILE" ]; then printf '%s\n' "$@" > "$FAKE_ARGS_FILE"; fi
if [ -n "$FAKE_STDIN_FILE" ]; then cat > "$FAKE_STDIN_FILE"; fi
cat <<'INNER'
`+fakeStream+`
INNER`)
	server := testBridge(t, bin, "")
	long := strings.Repeat("x", 30000)
	body := `{"model":"fake-model","messages":[{"role":"system","content":"` + long + `"},{"role":"user","content":"hi"}]}`
	resp := postJSON(t, server.URL+"/v1/chat/completions", body, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	args, _ := os.ReadFile(argsFile)
	argLines := strings.Split(string(args), "\n")
	hasP, hasInput := false, false
	for _, line := range argLines {
		if line == "-p" {
			hasP = true
		}
		if line == "--input-format" {
			hasInput = true
		}
	}
	if hasP || !hasInput {
		t.Fatalf("long prompt should use --input-format, not -p:\n%s", args)
	}
	stdin, _ := os.ReadFile(stdinFile)
	if !strings.Contains(string(stdin), `"event":"user"`) || !strings.Contains(string(stdin), strings.Repeat("x", 100)) {
		t.Fatalf("stdin payload missing prompt:\n%.200s", stdin)
	}
}

func TestDefaultProfileIsModel(t *testing.T) {
	bridge := NewBridge(Config{Bin: fakeAgy(t, fakeStream)})
	if !bridge.transparent {
		t.Fatal("default profile should be transparent model mode")
	}
	if bridge.cfg.Mode != "plan" || bridge.cfg.ToolMode != "none" {
		t.Fatalf("model profile forces plan/none, got mode=%q tool=%q", bridge.cfg.Mode, bridge.cfg.ToolMode)
	}
}

func TestModelProfileRejectsTools(t *testing.T) {
	server := testBridgeWithConfig(t, Config{Bin: fakeAgy(t, fakeStream), Profile: "model", ToolBridge: "off"})

	resp := postJSON(t, server.URL+"/v1/chat/completions", `{
		"model":"fake-model",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]
	}`, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("openai tools status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "tools_not_supported") {
		t.Fatalf("openai body=%s", body)
	}

	resp = postJSON(t, server.URL+"/v1/chat/completions", `{
		"model":"fake-model",
		"messages":[{"role":"user","content":"hi"}],
		"tool_choice":"auto"
	}`, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("tool_choice status=%d", resp.StatusCode)
	}

	resp = postJSON(t, server.URL+"/v1/messages", `{
		"model":"fake-model",
		"max_tokens":16,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"lookup","input_schema":{"type":"object"}}]
	}`, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("anthropic tools status=%d", resp.StatusCode)
	}
}

func TestModelProfileIgnoresSession(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("FAKE_ARGS_FILE", argsFile)
	server := testBridgeWithConfig(t, Config{Bin: fakeAgy(t, fakeStream), Profile: "model"})
	headers := map[string]string{"X-Session-Affinity": "s1"}

	first := `{"model":"fake-model","messages":[{"role":"system","content":"be nice"},{"role":"user","content":"first"}]}`
	if resp := postJSONWithHeaders(t, server.URL+"/v1/chat/completions", first, headers); resp.StatusCode != http.StatusOK {
		t.Fatalf("first status=%d", resp.StatusCode)
	}
	second := `{"model":"fake-model","messages":[{"role":"system","content":"be nice"},{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}]}`
	if resp := postJSONWithHeaders(t, server.URL+"/v1/chat/completions", second, headers); resp.StatusCode != http.StatusOK {
		t.Fatalf("second status=%d", resp.StatusCode)
	}
	args, _ := os.ReadFile(argsFile)
	if strings.Contains(string(args), "--conversation") {
		t.Fatalf("model profile must not use conversations:\n%s", args)
	}
	if !strings.Contains(string(args), "be nice") || !strings.Contains(string(args), "second") {
		t.Fatalf("model profile should resend full transcript:\n%s", args)
	}
}

func TestModelProfileForcesPlanAndNoToolOutput(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("FAKE_ARGS_FILE", argsFile)
	server := testBridgeWithConfig(t, Config{
		Bin: fakeAgy(t, fakeToolStream), Profile: "model", Mode: "skip", ToolMode: "passthrough",
	})
	resp := postJSON(t, server.URL+"/v1/chat/completions", `{
		"model":"fake-model","messages":[{"role":"user","content":"run"}],"stream":true
	}`, "")
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "tool_calls") {
		t.Fatalf("model profile must not emit tool calls:\n%s", body)
	}
	args, _ := os.ReadFile(argsFile)
	text := string(args)
	if !strings.Contains(text, "--mode\nplan") && !strings.Contains(text, "--mode plan") {
		t.Fatalf("expected --mode plan:\n%s", text)
	}
	if strings.Contains(text, "--dangerously-skip-permissions") {
		t.Fatalf("model profile must not skip permissions:\n%s", text)
	}
}

const fakeToolCall = `{"event":"init","conversation_id":"c1","model":"fake-model"}
{"event":"step_update","idx":1,"step_type":"text","text":"{\"tool_call\":{\"name\":\"lookup\",\"arguments\":{\"id\":1}}}"}
{"event":"result","result":{"conversation_id":"c1","status":"DONE","response":"{\"tool_call\":{\"name\":\"lookup\",\"arguments\":{\"id\":1}}}","usage":{"input_tokens":5,"output_tokens":2}}}`

func TestModelProfilePromptToolCallOpenAI(t *testing.T) {
	server := testBridgeWithConfig(t, Config{Bin: fakeAgy(t, fakeToolCall), Profile: "model", ToolBridge: "prompt"})
	body := `{"model":"fake-model","messages":[{"role":"user","content":"lookup 1"}],"tools":[{"type":"function","function":{"name":"lookup","description":"look up","parameters":{"type":"object","properties":{"id":{"type":"integer"}}}}}]}`
	resp := postJSON(t, server.URL+"/v1/chat/completions", body, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	for _, want := range []string{`"tool_calls"`, `"name":"lookup"`, `"finish_reason":"tool_calls"`, `"id"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("response missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, `"content":"{\"tool_call\"`) {
		t.Fatalf("tool call JSON leaked as content:\n%s", text)
	}

	resp = postJSON(t, server.URL+"/v1/chat/completions", body[:len(body)-1]+`,"stream":true}`, "")
	raw, _ = io.ReadAll(resp.Body)
	stream := string(raw)
	for _, want := range []string{`"tool_calls"`, `"finish_reason":"tool_calls"`, `data: [DONE]`} {
		if !strings.Contains(stream, want) {
			t.Fatalf("stream missing %q:\n%s", want, stream)
		}
	}
}

func TestModelProfilePromptToolCallAnthropic(t *testing.T) {
	server := testBridgeWithConfig(t, Config{Bin: fakeAgy(t, fakeToolCall), Profile: "model", ToolBridge: "prompt"})
	body := `{"model":"fake-model","max_tokens":16,"messages":[{"role":"user","content":"lookup 1"}],"tools":[{"name":"lookup","description":"look up","input_schema":{"type":"object","properties":{"id":{"type":"integer"}}}}]}`
	resp := postJSON(t, server.URL+"/v1/messages", body, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	for _, want := range []string{`"type":"tool_use"`, `"name":"lookup"`, `"stop_reason":"tool_use"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("response missing %q:\n%s", want, text)
		}
	}

	resp = postJSON(t, server.URL+"/v1/messages", body[:len(body)-1]+`,"stream":true}`, "")
	raw, _ = io.ReadAll(resp.Body)
	stream := string(raw)
	for _, want := range []string{`"type":"tool_use"`, `"input_json_delta"`, `"stop_reason":"tool_use"`} {
		if !strings.Contains(stream, want) {
			t.Fatalf("stream missing %q:\n%s", want, stream)
		}
	}
}

func TestModelProfilePromptToolsNormalText(t *testing.T) {
	server := testBridgeWithConfig(t, Config{Bin: fakeAgy(t, fakeStream), Profile: "model", ToolBridge: "prompt"})
	resp := postJSON(t, server.URL+"/v1/chat/completions", `{
		"model":"fake-model",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]
	}`, "")
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	if !strings.Contains(text, `"content":"Hello world"`) || !strings.Contains(text, `"finish_reason":"stop"`) {
		t.Fatalf("normal text response=%s", text)
	}
	if strings.Contains(text, "tool_calls") {
		t.Fatalf("unexpected tool call:\n%s", text)
	}
}

func TestRequestLevelLogs(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	bridge := NewBridge(Config{
		Bin:          fakeAgy(t, fakeStream),
		Profile:      "model",
		WorkDir:      t.TempDir(),
		Models:       []string{"fake-model"},
		DefaultModel: "fake-model",
		Mode:         "plan",
		ToolMode:     "none",
		ToolBridge:   "prompt",
		Timeout:      10 * time.Second,
		Logger:       zap.New(core),
	})
	server := httptest.NewServer(bridge.Handler())
	t.Cleanup(server.Close)

	resp := postJSON(t, server.URL+"/v1/chat/completions", `{"model":"fake-model","messages":[{"role":"user","content":"secret prompt"}]}`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	seen := map[string]bool{}
	for _, entry := range logs.All() {
		seen[entry.Message] = true
		for _, field := range entry.Context {
			if strings.Contains(field.String, "secret prompt") {
				t.Fatalf("log leaked prompt content in %s: %s", entry.Message, field.String)
			}
		}
	}
	for _, message := range []string{"request_prepared", "agy_start", "agy_finish", "request_result", "request_finished"} {
		if !seen[message] {
			t.Fatalf("missing log %q; got %v", message, logs.All())
		}
	}
}
