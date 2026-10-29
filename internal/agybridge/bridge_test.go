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
	script := "#!/bin/sh\ncat <<'EOF'\n" + body + "\nEOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testBridge(t *testing.T, bin, token string) *httptest.Server {
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
	})
	server := httptest.NewServer(bridge.Handler())
	t.Cleanup(server.Close)
	return server
}

func postJSON(t *testing.T, url, body, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
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
