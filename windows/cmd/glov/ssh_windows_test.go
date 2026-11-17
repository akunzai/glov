package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akunzai/glov/windows/internal/project"
)

func TestSSHArgs(t *testing.T) {
	f := sshFiles{`C:\Users\a b\AppData\Local\glov\vms\dev`}
	relay := sshRelayCommand(`C:\Program Files\glov\glov.exe`, "dev")
	if relay != `"C:\Program Files\glov\glov.exe" ssh-relay dev` {
		t.Errorf("relay = %s", relay)
	}
	got := sshArgs("dev", f, relay, false, []string{"-L", "8080:localhost:80", "-v", "uname", "-a"})
	want := []string{
		"-i", f.dir + `\id_ed25519`,
		"-o", "IdentitiesOnly=yes",
		"-o", `UserKnownHostsFile="` + f.dir + `\known_hosts"`,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "HostKeyAlias=dev",
		"-o", "ProxyCommand=" + relay,
		"-l", "glov",
		"-L", "8080:localhost:80", "-v",
		"--", "dev", "uname", "-a",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sshArgs =\n%q\nwant\n%q", got, want)
	}
	got = sshArgs("dev", f, relay, true, []string{"-A", "-L", "8080:localhost:80", "uname"})
	want = append(want[:14:14],
		"-o", "ClearAllForwardings=yes", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "Tunnel=no",
		"-o", "PermitLocalCommand=no", "-o", "EnableEscapeCommandline=no",
		"-A", "-L", "8080:localhost:80", "-a", "-x", "-S", "none", "--", "dev", "uname")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("isolated sshArgs =\n%q\nwant\n%q", got, want)
	}
	if q := sshQuote(`C:\100%\x`); q != `"C:\100%%\x"` {
		t.Errorf("sshQuote = %s", q)
	}
}

func TestSplitSSHOptions(t *testing.T) {
	for in, want := range map[string][2]string{
		"":               {"", ""},
		"uname -a":       {"", "uname -a"},
		"-t -p 22 ls -l": {"-t -p 22", "ls -l"},
		"-oFoo=1 -- -x":  {"-oFoo=1", "-x"},
		"-i":             {"-i", ""},
		"- x":            {"", "- x"},
	} {
		opts, cmd := splitSSHOptions(strings.Fields(in))
		if strings.Join(opts, " ") != want[0] || strings.Join(cmd, " ") != want[1] {
			t.Errorf("%q: options %q, command %q", in, opts, cmd)
		}
	}
}

// Runs for many VMs at once each keep their block: the edits take turns
// under ssh_config.lock.
func TestConcurrentSSHConfigEditsKeepEveryVMsBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh_config")
	var names []string
	for i := range 24 {
		names = append(names, fmt.Sprintf("vm%d", i))
	}
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Go(func() {
			err := editSSHConfig(path, func(text string) string {
				time.Sleep(2 * time.Millisecond) // widen the window a lost update needs
				return project.WithSSHHost(text, project.SSHHost{VM: name, Key: `C:\k`, KnownHosts: `C:\h`, Relay: "relay"})
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if !strings.Contains(string(text), "Host "+project.Host(name)+"\n") {
			t.Errorf("%s's block is missing", name)
		}
	}
}
