package provision

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/akunzai/glov/windows/internal/harness"
	"github.com/akunzai/glov/windows/internal/seed"
)

func TestWatch(t *testing.T) {
	for _, tc := range []struct {
		console string
		steps   []string
		err     string
	}{
		{"[ 1.0] Linux version\r\nGLOV-PROVISION: installing\r\nnoise\nGLOV-PROVISION: Guest Service 1.2\nGLOV-PROVISION-OK\nlater\n",
			[]string{"installing", "Guest Service 1.2"}, ""},
		{"GLOV-PROVISION: installing\nGLOV-PROVISION-FAILED: load hv_sock\n", []string{"installing"}, "provisioning failed: load hv_sock"},
		{"Kernel panic\n", nil, ErrNoReport.Error()},
	} {
		var steps, lines []string
		err := Watch(strings.NewReader(tc.console), func(s string) { steps = append(steps, s) }, func(l string) { lines = append(lines, l) })
		if (err == nil) != (tc.err == "") || (err != nil && err.Error() != tc.err) {
			t.Errorf("Watch(%q) err = %v, want %q", tc.console, err, tc.err)
		}
		if strings.Join(steps, "|") != strings.Join(tc.steps, "|") {
			t.Errorf("steps = %q, want %q", steps, tc.steps)
		}
		if len(lines) == 0 || strings.HasSuffix(lines[0], "\r") {
			t.Errorf("lines = %q", lines)
		}
	}
	if !errors.Is(Watch(strings.NewReader(""), func(string) {}, func(string) {}), ErrNoReport) {
		t.Error("empty console")
	}
}

func TestFilesCarryTheScriptAndGuest(t *testing.T) {
	files := Files("6f1c2a9e-0c5d-4b8e-9a3f-1d2e3f4a5b6c", "dev", false, nil, nil, []byte("ELF"))
	names := map[string]string{}
	for _, f := range files {
		names[f.Name] = string(f.Data)
	}
	if _, got := names["network-config"]; !got {
		t.Error("network-config missing")
	}
	if !strings.Contains(names["meta-data"], "instance-id: 6f1c2a9e") || !strings.Contains(names["meta-data"], "local-hostname: dev") {
		t.Errorf("meta-data = %q", names["meta-data"])
	}
	if !strings.HasPrefix(names["user-data"], "#!/bin/sh\n") || strings.Contains(names["user-data"], "\r") {
		t.Error("user-data is not a Unix shell script")
	}
	if !strings.Contains(names["user-data"], "\nname='dev'\n") {
		t.Error("user-data does not set the name")
	}
	if names[GuestFile] != "ELF" {
		t.Error("guest binary missing")
	}
}

func TestHostname(t *testing.T) {
	for name, want := range map[string]string{
		"dev":                          "dev",
		"web.app_1":                    "web-app-1",
		strings.Repeat("a", 64):        strings.Repeat("a", 63),
		strings.Repeat("a", 62) + "_b": strings.Repeat("a", 62),
	} {
		if got := Hostname(name); got != want {
			t.Errorf("Hostname(%q) = %q, want %q", name, got, want)
		}
	}
}

// A getty on ttyS0 can hang it up and revoke an open descriptor: the
// scripts open the console for each line instead of holding it.
func TestScriptsNeverHoldTheConsoleOpen(t *testing.T) {
	for _, isolated := range []bool{false, true} {
		script := userData(Files("id", "dev", isolated, nil, nil, nil))
		for _, line := range strings.Split(script, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "exec") && strings.Contains(line, "ttyS0") {
				t.Errorf("isolated=%v: %q holds the console open", isolated, line)
			}
		}
		for _, want := range []string{"exec >>" + LogFile + " 2>&1\n", "console \"" + Done + "\"\n"} {
			if !strings.Contains(script, want) {
				t.Errorf("isolated=%v: script lacks %q", isolated, want)
			}
		}
	}
}

func userData(files []seed.File) string {
	for _, f := range files {
		if f.Name == "user-data" {
			return string(f.Data)
		}
	}
	return ""
}

// The Guest Service serves the proxy only with the marker, which must be in
// place before it is installed (and starts on the next boot).
func TestAnIsolatedVMGetsTheEgressMarkerAndProxyEnvironment(t *testing.T) {
	script := userData(Files("id", "dev", true, nil, nil, nil))
	marker := strings.Index(script, `printf 'glov-egress-v1\n' > /etc/glov/egress-proxy`)
	install := strings.Index(script, "/tmp/"+GuestFile+" install")
	if marker < 0 || install < marker {
		t.Errorf("marker at %d, Guest Service install at %d", marker, install)
	}
	for _, want := range []string{"https_proxy=" + ProxyURL + "\n", "NO_PROXY=localhost,127.0.0.1,::1\n", `Acquire::https::Proxy "` + ProxyURL + `";`} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if !strings.HasPrefix(script, "#!/bin/sh\nname='dev'\nexec >>") {
		t.Error("script does not start with the report preamble")
	}
	if plain := userData(Files("id", "dev", false, nil, nil, nil)); strings.Contains(plain, "egress") {
		t.Error("a normal VM's script sets up egress")
	}
}

const testCA = "-----BEGIN CERTIFICATE-----\nCA\n"

func TestAHarnessVMGetsItsUserEnvironmentFilesAndConsoleSignIn(t *testing.T) {
	codex, _ := harness.Named("codex")
	claude, _ := harness.Named("claude")
	script := userData(Files("id", "dev", true, []harness.Profile{claude, codex}, []byte(testCA), nil))
	for _, want := range []string{
		`say "setting up the harnesses: claude, codex"`,
		"useradd -m -s /bin/bash -G sudo \"$u\"",
		"ANTHROPIC_API_KEY=glov-secret:anthropic\nCLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1\n",
		"CODEX_API_KEY=glov-secret:openai\n",
		`mkdir -p "$home/.codex"`,
		`chown -hR "$u:$u" "$home/.claude"`,
		`say "trusting the egress CA"`,
		"| base64 -d > " + CAPath,
		"update-ca-certificates",
		"NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt\n",
		"ExecStart=-/sbin/agetty --autologin " + User + " --noclear %I $TERM\n",
		"> " + UserNamespacesPath,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	// Each file is written from base64, which decodes to the profile's content.
	for _, f := range append(claude.GuestFiles(), codex.GuestFiles()...) {
		if !strings.Contains(script, base64.StdEncoding.EncodeToString([]byte(f.Content()))) {
			t.Errorf("script does not write %s", f.Path)
		}
	}
	if !strings.Contains(script, base64.StdEncoding.EncodeToString([]byte(testCA))) {
		t.Error("script does not write the egress CA")
	}
	if strings.Index(script, "trusting the egress CA") > strings.Index(script, "setting up the harnesses") {
		t.Error("the harnesses are set up before the CA is trusted")
	}
	setup := strings.Index(script, "setting up the harnesses")
	if setup < strings.Index(script, "egress-proxy") || setup > strings.Index(script, Done) {
		t.Error("the harness step is not between the egress setup and the end")
	}
	only, _ := harness.Named("claude")
	if s := userData(Files("id", "dev", true, []harness.Profile{only}, nil, nil)); strings.Contains(s, UserNamespacesPath) {
		t.Error("a VM without codex allows user namespaces")
	}
	if s := userData(Files("id", "dev", true, nil, nil, nil)); strings.Contains(s, "harness") || strings.Contains(s, "autologin") || strings.Contains(s, "egress CA") {
		t.Error("an isolated VM without harnesses sets them up")
	}
}
