package main

import (
	"slices"
	"testing"
)

func TestHarnessCommandsTakeGlovsOptionsAndPassTheRestOn(t *testing.T) {
	cmd, err := parse([]string{"claude", "-c", "--new", "--egress", "open", "--allow-plain-secrets", "--model", "x", "--", "--new"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.name != "agent" || cmd.agent.harness != "claude" || !cmd.agent.fresh || cmd.egress != "open" || !cmd.allowPlain ||
		!slices.Equal(cmd.agent.args, []string{"-c", "--model", "x", "--new"}) {
		t.Errorf("claude = %+v", cmd)
	}
	for word, want := range map[string]string{"agy": "antigravity", "codex": "codex"} {
		if cmd, err := parse([]string{word}); err != nil || cmd.agent.harness != want {
			t.Errorf("%s = %+v, %v", word, cmd.agent, err)
		}
	}
	if cmd, err := parse([]string{"agent", "antigravity", "--allow-remotes"}); err != nil || cmd.agent.harness != "antigravity" || !cmd.agent.allowRemotes {
		t.Errorf("agent antigravity = %+v, %v", cmd.agent, err)
	}
	if cmd, err := parse([]string{"claude", "--sync", "live"}); err != nil || cmd.agent.sync != "live" {
		t.Errorf("--sync live = %+v, %v", cmd.agent, err)
	}
	if cmd, err := parse([]string{"claude", "--sync=git"}); err != nil || cmd.agent.sync != "git" {
		t.Errorf("--sync=git = %+v, %v", cmd.agent, err)
	}
	if cmd, err := parse([]string{"claude", "--herdr", "--yes", "-c"}); err != nil || !cmd.agent.herdr || !cmd.agent.yes || !slices.Equal(cmd.agent.args, []string{"-c"}) {
		t.Errorf("--herdr --yes -c = %+v, %v", cmd.agent, err)
	}
	for _, bad := range [][]string{{"claude", "--sync", "rsync"}, {"claude", "--sync"}, {"agent"}, {"agent", "aider"}, {"claude", "--egress", "wide"}, {"claude", "--allow-plain-secrets"}, {"claude", "--egress"}} {
		if _, err := parse(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if cmd, err := parse([]string{"vm", "fetch", "p-1"}); err != nil || cmd.name != "vm fetch" || cmd.vm != "p-1" {
		t.Errorf("vm fetch = %+v, %v", cmd, err)
	}
	if cmd, err := parse([]string{"vm", "land", "p-1", "work", "--yes"}); err != nil || cmd.name != "vm land" || cmd.vm != "p-1" || cmd.ref != "work" || !cmd.yes {
		t.Errorf("vm land = %+v, %v", cmd, err)
	}
	if cmd, err := parse([]string{"vm", "land", "p-1"}); err != nil || cmd.ref != "" || cmd.yes {
		t.Errorf("vm land without a branch = %+v, %v", cmd, err)
	}
	for _, bad := range [][]string{{"vm", "land"}, {"vm", "land", "p-1", "a", "b"}, {"vm", "land", "p-1", "--force"}} {
		if _, err := parse(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
