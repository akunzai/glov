package herdr

import (
	"reflect"
	"testing"
)

func TestTheMachineIsReachedThroughGlovSSH(t *testing.T) {
	via := Via(`C:\Program Files\glov\glov.exe`, "app-1a2b3c")
	if via != `"C:\Program Files\glov\glov.exe" ssh app-1a2b3c --` {
		t.Errorf("Via = %s", via)
	}
	if !IsVM(Machine{Name: "app-1a2b3c", Target: via}, "app-1a2b3c") {
		t.Error("the VM's machine is not recognised")
	}
	if !IsVM(Machine{Name: "app-1a2b3c", Target: Via(`D:\dev\glov.exe`, "app-1a2b3c")}, "app-1a2b3c") {
		t.Error("the VM's machine added by another glov.exe is not recognised")
	}
	for _, m := range []Machine{
		{Name: "app-1a2b3c", Target: "glov-app-1a2b3c"},
		{Name: "app-1a2b3c", Target: "app-1a2b3c"},
		{Name: "app-1a2b3c", Target: Via(`C:\glov.exe`, "app")},
		{Name: "other", Target: via},
	} {
		if IsVM(m, "app-1a2b3c") {
			t.Errorf("%+v is taken for the VM", m)
		}
	}
}

func TestHcAndHerdrAppListingsAreRead(t *testing.T) {
	machines := []byte(`{"machines":[{"name":"v","target":"glov-v","state":"connected","via":false}]}`)
	if got := Machines(machines); !reflect.DeepEqual(got, []Machine{{Name: "v", Target: "glov-v", State: "connected"}}) {
		t.Errorf("Machines = %v", got)
	}
	ls := []byte(`{"terminals":[{"address":"v/t1","name":"claude","state":"live"},{"address":"v/t2","name":"claude-2","state":"finished"},{"address":"t3","name":"codex","state":"live"},{"address":"v/t5","name":null,"state":"live"}]}`)
	if got := LiveNames(ls, "v"); !reflect.DeepEqual(got, []string{"claude"}) {
		t.Errorf("LiveNames = %v", got)
	}
	app := []byte(`{"spaces":[{"id":"sp1","folder":"/mnt/c/p","machine":null,"tabs":[{"name":"claude","terminals":[{"terminal":"t9"}]}]},
		{"id":"sp2","folder":"/mnt/c/p","machine":"v","tabs":[{"id":"v/tb4","name":"claude","terminals":[{"terminal":"v/t4"}]},{"name":null,"terminals":[]}]}]}`)
	if got := PlaceOf(app, "v", "/mnt/c/p", "claude"); !reflect.DeepEqual(got, Place{Space: "sp2", Tabs: []string{"claude"}, Terminal: "v/t4", Tab: "v/tb4"}) {
		t.Errorf("PlaceOf = %+v", got)
	}
	if got := PlaceOf(app, "v", "/mnt/c/q", "claude"); !reflect.DeepEqual(got, Place{}) {
		t.Errorf("PlaceOf another folder = %+v", got)
	}
	if got := PlaceOf([]byte("not json"), "v", "/mnt/c/p", "claude"); !reflect.DeepEqual(got, Place{}) {
		t.Errorf("PlaceOf garbage = %+v", got)
	}
}

// hc get --json as hc 0.1.19 reports each case, seen on a VZ guest across
// glov vm reboot on the Mac, which runs the same hc in the guest.
func TestASessionLeftAtAShellIsToldFromTheHarness(t *testing.T) {
	wrapper := `["sh","-c","shell=${SHELL:-/bin/sh}\n...","sh","claude"]`
	for _, c := range []struct {
		json string
		want Occupant
	}{
		// hc run --keep: running, then replaced by a shell after the restart.
		{`{"argv":["claude","-c"],"foreground":null}`, Occupant{}},
		{`{"argv":["/bin/bash"],"foreground":{"argv":["/bin/bash"],"name":"bash"}}`, Occupant{Shell: true}},
		// herdr-app's tab: its wrapper runs the harness, then a login shell
		// once it exits.
		{`{"argv":` + wrapper + `,"foreground":null}`, Occupant{}},
		{`{"argv":` + wrapper + `,"foreground":{"argv":["claude"],"name":"2.1.319"}}`, Occupant{}},
		{`{"argv":` + wrapper + `,"foreground":{"argv":["/bin/bash","-li"],"name":"bash"}}`, Occupant{Shell: true}},
		{`{"argv":["/bin/bash"],"foreground":{"argv":["-zsh"],"name":"zsh"}}`, Occupant{Shell: true}},
		// Someone typed into the kept shell.
		{`{"argv":["/bin/bash"],"foreground":{"argv":["vim","notes.md"],"name":"vim"}}`, Occupant{Other: "vim"}},
		// A hostile guest names the program: what reaches the terminal
		// carries no escape sequence.
		{`{"argv":["/bin/bash"],"foreground":{"argv":["\u001b]52;c;eA==\u0007x"],"name":"x"}}`, Occupant{Other: "52ceAx"}},
		{`{"argv":["/bin/bash"],"foreground":{"argv":["\u001b\u009b"],"name":"x"}}`, Occupant{Other: "a program"}},
		{`not json`, Occupant{}},
	} {
		if got := OccupantOf([]byte(c.json), "claude"); got != c.want {
			t.Errorf("OccupantOf(%s) = %+v, want %+v", c.json, got, c.want)
		}
	}
	// Between the machine coming back and hc starting the kept terminal's
	// shell, hc still names the harness: not yet readable.
	for _, c := range []struct {
		json string
		want bool
	}{
		{`{"argv":["claude"],"pid":null,"finished_ms":null,"reason":"stopped_by_restart","state":"live"}`, true},
		{`{"argv":["/bin/bash"],"pid":1515,"finished_ms":null,"foreground":{"argv":["/bin/bash"],"name":"bash"}}`, false},
		{`{"argv":["claude"],"pid":null,"finished_ms":1794234052785,"exit_code":0}`, false},
		{`not json`, false},
	} {
		if got := Restoring([]byte(c.json)); got != c.want {
			t.Errorf("Restoring(%s) = %v, want %v", c.json, got, c.want)
		}
	}
	if got := Restarting("claude", "v", "claude"); got != "v/claude was left at a shell (the VM restarted, or claude exited); starting claude again" {
		t.Errorf("Restarting = %q", got)
	}
	if got := LeftAlone("claude", "v", "top", "claude"); got != "v/claude runs top, not claude; showing it as it is (quit top and run again to start claude)" {
		t.Errorf("LeftAlone = %q", got)
	}
}
