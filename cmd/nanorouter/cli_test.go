package main

import (
	"bytes"
	"strings"
	"testing"
)

// executeRoot 用注入的参数运行根命令并返回标准输出/错误。
func executeRoot(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd := newRootCommand()
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestVersionFlagPrintsInjectedVersion(t *testing.T) {
	out, _, err := executeRoot(t, "--version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.Contains(out, "nanorouter version dev") {
		t.Fatalf("version output: %q", out)
	}
	// 短旗标 -v 同样可用。
	out, _, err = executeRoot(t, "-v")
	if err != nil || !strings.Contains(out, "nanorouter version dev") {
		t.Fatalf("-v output: %q err=%v", out, err)
	}
}

func TestHelpListsAllCommandsAndFlags(t *testing.T) {
	out, _, err := executeRoot(t, "--help")
	if err != nil {
		t.Fatalf("help: %v", err)
	}
	for _, want := range []string{"login-zcode", "--config", "--debug", "Available Commands"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help output missing %q:\n%s", want, out)
		}
	}
}

func TestUnknownSubcommandIsRejected(t *testing.T) {
	_, errOut, err := executeRoot(t, "bogus-subcommand")
	if err == nil {
		t.Fatal("unknown subcommand must fail")
	}
	if !strings.Contains(errOut, "bogus-subcommand") && !strings.Contains(err.Error(), "bogus-subcommand") {
		t.Fatalf("error should name the unknown command: %v / %q", err, errOut)
	}
}

func TestLoginZcodeHelpShowsFlags(t *testing.T) {
	out, _, err := executeRoot(t, "login-zcode", "--help")
	if err != nil {
		t.Fatalf("login-zcode help: %v", err)
	}
	for _, want := range []string{"--channel", "--gateway", "--timeout", "--no-browser"} {
		if !strings.Contains(out, want) {
			t.Fatalf("login-zcode help missing %q:\n%s", want, out)
		}
	}
}
