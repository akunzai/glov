// Package herdr is glov claude --herdr's view of herdr-core (hc) and
// herdr-app: Windows' hc reaches a project VM as a machine through
// glov ssh <vm> -- (hc machine add --via) and runs the harness in a terminal
// there, which herdr-app shows in a space of that machine. Pure parts, so they are
// tested; the CLI runs hc and herdr-app.
package herdr

import (
	"encoding/json/v2"
	"fmt"
	"path"
	"slices"
	"strings"
)

// Warning is what the guest's hc can make Windows' hc and herdr-app do once
// the VM is a machine (herdr-core trusts a machine it adds), printed each
// time.
const Warning = `warning: with --herdr the VM is a herdr-core machine, and herdr-core trusts its machines. A hostile guest can
  - type text and Enter into any Windows terminal that started a terminal in the VM (hc run --on <vm>): glov's
    own sessions start without a parent, but do not run hc run --on <vm> from a Windows shell or agent;
  - make herdr-app open spaces, tabs and panes, show statuses, sounds and toasts;
  - write Windows' clipboard (herdr-app setting clipboard_write: false stops it);
  - claim it needs a login: never type a Windows password or code for this machine.
hc's notes = false keeps it from typing notes into agents that are parents. See docs/isolated-harness-vms.md.`

// Machine is one entry of hc machine ls --json.
type Machine struct {
	Name   string `json:"name"`
	Target string `json:"target"`
	// State is connected once hc reaches it; unreachable, with Why, until
	// then.
	State string `json:"state"`
	Why   string `json:"why"`
}

// Via is the hc machine add --via command for VM vm: glov (the absolute
// path of glov.exe) ssh vm --. Windows' hc splits it by Windows' argument
// rules, with no shell, and adds its remote command as the last argument;
// a Windows path has no '"' to escape.
func Via(glov, vm string) string {
	return `"` + glov + `" ssh ` + vm + " --"
}

// IsVM reports whether machine m is VM vm's: named after it and reached
// through glov's ssh to it, whichever glov.exe added it.
func IsVM(m Machine, vm string) bool {
	return m.Name == vm && strings.HasSuffix(m.Target, `" ssh `+vm+" --")
}

// Machines reads hc machine ls --json.
func Machines(data []byte) []Machine {
	var list struct {
		Machines []Machine `json:"machines"`
	}
	if json.Unmarshal(data, &list) != nil {
		return nil
	}
	return list.Machines
}

// LiveNames are the names of the live terminals on machine vm in
// hc ls --json.
func LiveNames(data []byte, vm string) []string {
	var list struct {
		Terminals []struct {
			Address string `json:"address"`
			Name    string `json:"name"`
			State   string `json:"state"`
		} `json:"terminals"`
	}
	if json.Unmarshal(data, &list) != nil {
		return nil
	}
	var names []string
	for _, t := range list.Terminals {
		if t.State == "live" && strings.HasPrefix(t.Address, vm+"/") && t.Name != "" && !slices.Contains(names, t.Name) {
			names = append(names, t.Name)
		}
	}
	return names
}

// Occupant is what a harness session's terminal runs now. A VM restart
// replaces the harness glov started with a shell (hc's --keep; herdr-app
// always keeps its terminals), and herdr-app's tabs also drop to a shell
// when the harness exits, so a live terminal of the harness's name is not
// proof that the harness runs there.
type Occupant struct {
	// Shell is a shell at its prompt: nothing to lose, so glov replaces it
	// with the harness.
	Shell bool
	// Other names another program someone started in the kept shell,
	// shown as it is and never ended. Neither set: the harness, or
	// something glov cannot tell apart from it.
	Other string
}

// shells are the programs a kept or exited session drops to.
var shells = []string{"sh", "bash", "zsh", "dash", "ksh", "fish"}

// OccupantOf reads hc get <terminal> --json: its foreground program, else
// the program it was started with. Anything glov cannot read counts as the
// harness (command), so a session is never ended on a guess.
func OccupantOf(data []byte, command string) Occupant {
	var t struct {
		Argv       []string `json:"argv"`
		Foreground *struct {
			Argv []string `json:"argv"`
		} `json:"foreground"`
	}
	if json.Unmarshal(data, &t) != nil {
		return Occupant{}
	}
	argv := t.Argv
	if t.Foreground != nil {
		argv = t.Foreground.Argv
	}
	if len(argv) == 0 {
		return Occupant{}
	}
	base := strings.TrimLeft(path.Base(argv[0]), "-")
	if slices.Contains(shells, base) && !slices.ContainsFunc(argv[1:], func(a string) bool { return !strings.HasPrefix(a, "-") || a == "-c" }) {
		// Options only: an interactive or login shell. herdr-app's wrapper
		// (sh -c '...' sh claude) still starts one.
		return Occupant{Shell: true}
	}
	if t.Foreground != nil && base != command && !slices.Contains(shells, base) {
		// The guest names it, and glov prints it on Windows' terminal: no
		// escape sequences.
		name := strings.Map(func(r rune) rune {
			if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._+-", r)) {
				return r
			}
			return -1
		}, base)
		if name = name[:min(len(name), 32)]; name == "" {
			name = "a program"
		}
		return Occupant{Other: name}
	}
	return Occupant{}
}

// Restoring reads hc get <terminal> --json: no program and not finished,
// as hc reports a kept terminal from the VM's restart until it has started
// its new shell (argv still the harness, pid null). Its foreground is not
// known yet.
func Restoring(data []byte) bool {
	var t struct {
		PID      *int   `json:"pid"`
		Finished *int64 `json:"finished_ms"`
	}
	return json.Unmarshal(data, &t) == nil && t.PID == nil && t.Finished == nil
}

// Restarting is the line said when session's terminal on vm is left at a
// shell and glov starts the harness (command) in its place.
func Restarting(session, vm, command string) string {
	return fmt.Sprintf("%s/%s was left at a shell (the VM restarted, or %s exited); starting %s again", vm, session, command, command)
}

// LeftAlone is the line said when someone runs program in session's kept
// shell: glov shows it as it is, since ending it could lose their work, and
// the run after they quit it starts the harness.
func LeftAlone(session, vm, program, command string) string {
	return fmt.Sprintf("%s/%s runs %s, not %s; showing it as it is (quit %s and run again to start %s)", vm, session, program, command, program, command)
}

// Place is where herdr-app shows machine vm: its space for a folder, the
// names of that space's tabs, and the terminal and id of a tab there.
type Place struct {
	Space    string
	Tabs     []string
	Terminal string
	Tab      string
}

// PlaceOf reads herdr-app ls: vm's space for folder (a guest path), and the
// terminal of its tab named tab.
func PlaceOf(data []byte, vm, folder, tab string) Place {
	var list struct {
		Spaces []struct {
			ID      string `json:"id"`
			Folder  string `json:"folder"`
			Machine string `json:"machine"`
			Tabs    []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				Terminals []struct {
					Terminal string `json:"terminal"`
				} `json:"terminals"`
			} `json:"tabs"`
		} `json:"spaces"`
	}
	if json.Unmarshal(data, &list) != nil {
		return Place{}
	}
	for _, s := range list.Spaces {
		if s.Machine != vm || s.Folder != folder {
			continue
		}
		p := Place{Space: s.ID}
		for _, t := range s.Tabs {
			if t.Name == "" {
				continue
			}
			p.Tabs = append(p.Tabs, t.Name)
			if t.Name == tab && p.Terminal == "" && len(t.Terminals) > 0 {
				p.Terminal, p.Tab = t.Terminals[0].Terminal, t.ID
			}
		}
		return p
	}
	return Place{}
}
