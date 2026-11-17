package main

import (
	"strings"

	"github.com/akunzai/glov/windows/internal/harness"
)

// agentOptions are glov claude's (codex's, agy's, glov agent <harness>'s).
type agentOptions struct {
	// harness is the profile's name.
	harness string
	// fresh is --new: another session instead of the harness's own.
	fresh bool
	// allowRemotes lets a new VM reach the project remotes' repositories
	// without asking.
	allowRemotes bool
	// sync is --sync: how a new VM reaches the project, api.SyncGit or
	// api.SyncLive; "" asks in a terminal, git otherwise.
	sync string
	// herdr is --herdr: the harness in herdr-core instead of tmux.
	herdr bool
	// yes is --yes: with --herdr, add the VM as a herdr-core machine
	// without asking.
	yes bool
	// args go to the harness.
	args []string
}

// agentHarness is the harness glov <word> names: a profile's command (agy)
// or name (antigravity).
func agentHarness(word string) (harness.Profile, bool) {
	for _, p := range harness.All {
		if p.Command == word {
			return p, true
		}
	}
	p, err := harness.Named(word)
	return p, err == nil
}

const agentUse = "usage: glov <harness> [--new] [--sync git|live] [--allow-remotes] [--egress open|restricted] [--allow-plain-secrets] [--herdr [--yes]] [--] [harness arguments...]"

// parseAgent reads glov claude's arguments after the harness: glov's own
// options, wherever they are before --, and the rest for the harness, as
// on macOS (glov claude -c is glov claude -- -c).
func parseAgent(name string, args []string) (command, error) {
	p, ok := agentHarness(name)
	if !ok {
		return command{}, usageError("unknown harness " + name + ": use " + strings.Join(agentCommands(), ", "))
	}
	cmd := command{name: "agent", agent: agentOptions{harness: p.Name}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			cmd.agent.args = append(cmd.agent.args, args[i+1:]...)
			return cmd, nil
		case a == "--new":
			cmd.agent.fresh = true
		case a == "--allow-remotes":
			cmd.agent.allowRemotes = true
		case a == "--herdr":
			cmd.agent.herdr = true
		case a == "--yes":
			cmd.agent.yes = true
		case a == "--allow-plain-secrets":
			cmd.allowPlain = true
		case a == "--sync" || strings.HasPrefix(a, "--sync="):
			v, has := strings.CutPrefix(a, "--sync=")
			if !has {
				if i+1 == len(args) {
					return command{}, usageError(agentUse)
				}
				i++
				v = args[i]
			}
			if v != "git" && v != "live" {
				return command{}, usageError("--sync is git or live")
			}
			cmd.agent.sync = v
		case a == "--egress" || strings.HasPrefix(a, "--egress="):
			v, has := strings.CutPrefix(a, "--egress=")
			if !has {
				if i+1 == len(args) {
					return command{}, usageError(agentUse)
				}
				i++
				v = args[i]
			}
			if v != "open" && v != "restricted" {
				return command{}, usageError("--egress is open or restricted")
			}
			cmd.egress = v
		default:
			cmd.agent.args = append(cmd.agent.args, a)
		}
	}
	if cmd.allowPlain && cmd.egress != "open" {
		return command{}, usageError("--allow-plain-secrets needs --egress open")
	}
	return cmd, nil
}

// agentCommands are the words glov <harness> takes.
func agentCommands() []string {
	var out []string
	for _, p := range harness.All {
		out = append(out, p.Command)
	}
	return out
}
