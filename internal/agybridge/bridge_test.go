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
)

const fakeStream = `{"event":"init","conversation_id":"c1","model":"fake-model"}
{"event":"step_update","idx":1,"step_type":"text","text":"Hello"}
{"event":"step_update","idx":1,"step_type":"text","text":"Hello world"}
{"event":"step_update","idx":2,"step_type":"thinking","text":"Let me think"}
{"event":"result","result":{"conversation_id":"c1","status":"DONE","response":"Hello world","usage":{"input_tokens":3,"output_tokens":2,"thinking_tokens":1}}}`

func fakeAgy(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake agy uses a POSIX shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "agy")
	script := "#!/bin/sh\nif [ -n \"$FAKE_ARGS_FILE\" ]; then printf '%s\\n' \"$@\" > \"$FAKE_ARGS_FILE\"; fi\ncat <<'EOF'\n" + body + "\nEOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testBridge(t *testing.T, bin, token string) *httptest.Server {
	return testBridgeWithToolMode(t, bin, token, "annotate")
}

func testBridgeWithToolMode(t *testing.T, bin, token, toolMode string) *httptest.Server {
	t.Helper()
	bridge := NewBridge(Config{
		Bin:          bin,
		WorkDir:      t.TempDir(),
		Mode:         "plan",
		Timeout:      10 * time.Second,
		Token:        token,
		Models:       []string{"fake-model"},
		DefaultModel: "fake-model",
		EmitThinking: true,
		ToolMode:     toolMode,
	})
	server := httptest.NewServer(bridge.Handler())
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
