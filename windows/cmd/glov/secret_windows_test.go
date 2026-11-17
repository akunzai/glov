package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akunzai/glov/windows/internal/api"
)

func TestASourceCommandRunsAsTheUserAndItsErrorsQuoteNoOutput(t *testing.T) {
	shell := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	if v, err := runSource(t.Context(), "s", []string{shell, "/c", "echo  tok-123 "}); err != nil || v != "tok-123" {
		t.Errorf("runSource = %q, %v", v, err)
	}
	if _, err := runSource(t.Context(), "s", []string{shell, "/c", "echo leaked & exit 3"}); err == nil || err.Error() != "secret s: cmd.exe failed (exit 3)" {
		t.Errorf("a failing command err = %v", err)
	}
	if _, err := runSource(t.Context(), "s", []string{shell, "/c", "echo leaked 1>&2"}); err == nil || err.Error() != "secret s: cmd.exe printed nothing" {
		t.Errorf("a command printing only to stderr err = %v", err)
	}
	argv, err := resolveCommand([]string{"cmd", "/c"})
	if err != nil || !strings.EqualFold(argv[0], shell) || argv[1] != "/c" {
		t.Errorf("resolveCommand(cmd) = %q, %v", argv, err)
	}
	if _, err := resolveCommand([]string{"no-such-program-glov"}); err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Errorf("resolveCommand of a missing program err = %v", err)
	}
}

func TestSecretLinesShowNoValue(t *testing.T) {
	if got := describeSecret(api.Secret{Name: "anthropic", Source: "stored", Hosts: []string{"api.anthropic.com"}, Length: 27, Ending: "wxyz"}); got != "anthropic\tstored\tapi.anthropic.com\t27 characters, ending …wxyz" {
		t.Errorf("stored = %q", got)
	}
	if got := describeSecret(api.Secret{Name: "gh", Source: "command", Command: []string{`C:\gh.exe`, "auth", "token"}, Hosts: []string{"github.com", "api.github.com"}}); got != "gh\tcommand: C:\\gh.exe auth token\tgithub.com, api.github.com" {
		t.Errorf("command = %q", got)
	}
	if got := describeSecret(api.Secret{Name: "gh", Source: "command", Command: []string{`C:\gh.exe`, "auth", "token"}, Hosts: []string{"github.com"}, Forge: "github"}); got != "gh\tcommand: C:\\gh.exe auth token (github sign-in)\tgithub.com" {
		t.Errorf("git sign-in = %q", got)
	}
	if got := describeGrants(api.SecretGrants{VM: "box", Names: []string{"gh", "old"}, Missing: []string{"old"}}); got != "gh\nold\t(no longer defined)\n" {
		t.Errorf("grants = %q", got)
	}
	if got := describeGrants(api.SecretGrants{VM: "box"}); got != "box: no secrets granted\n" {
		t.Errorf("no grants = %q", got)
	}
}

func TestASourceCommandPrintingAnotherCodePageIsRefused(t *testing.T) {
	// Big5 bytes, copied to stdout as they are by cmd.exe's type. Not
	// PowerShell: its cold start on a CI runner can outlast sourceTimeout.
	file := filepath.Join(t.TempDir(), "big5.txt")
	if err := os.WriteFile(file, []byte{0xA4, 0xA4, 0x41}, 0o600); err != nil {
		t.Fatal(err)
	}
	shell := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	_, err := runSource(t.Context(), "s", []string{shell, "/c", "type", file})
	if err == nil || err.Error() != "secret s: cmd.exe printed text that is not UTF-8" {
		t.Errorf("err = %v", err)
	}
}
