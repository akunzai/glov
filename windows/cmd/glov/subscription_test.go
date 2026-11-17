package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/akunzai/glov/windows/internal/api"
)

func TestTheSignInCommandRunsInALoginShellAsGiven(t *testing.T) {
	if got := shellQuote([]string{"agy", "--model", "gemini-3.8-flash-low", "-p", "Reply with the single word: ok", "it's", ""}); got != `agy --model gemini-3.8-flash-low -p 'Reply with the single word: ok' 'it'\''s' ''` {
		t.Errorf("quoted %s", got)
	}
	if got := guestLoginShell([]string{"codex", "login", "--device-auth"}); got != `bash -lc 'codex login --device-auth'` {
		t.Errorf("login shell %s", got)
	}
}

func TestOnlyAHarnessWithASubscriptionSignsIn(t *testing.T) {
	if sub, err := subscriptionOf("codex"); err != nil || !slices.Equal(sub.SignIn, []string{"codex", "login", "--device-auth"}) {
		t.Errorf("codex: %+v, %v", sub, err)
	}
	if _, err := subscriptionOf("vim"); err == nil || !strings.HasSuffix(err.Error(), "those that do: antigravity, claude, codex") {
		t.Errorf("vim: %v", err)
	}
}

func TestASignInThatStoredNothingIsStillMissing(t *testing.T) {
	r := api.SecretLogin{Missing: []string{"box.codex.access", "box.codex.refresh"}}
	g := api.SecretGrants{Names: []string{"box.codex.access", "box.codex.refresh"}, Missing: []string{"box.codex.refresh"}}
	if got := stillMissing(r, g); !slices.Equal(got, []string{"box.codex.refresh"}) {
		t.Errorf("missing %q", got)
	}
}
