package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/share"
)

func TestParse(t *testing.T) {
	for line, want := range map[string]command{
		"help":                                   {name: "help"},
		"--help":                                 {name: "help"},
		"host install":                           {name: "service install"},
		"host uninstall":                         {name: "service uninstall"},
		"host install-broker":                    {name: "service install-broker"},
		"host uninstall-broker":                  {name: "service uninstall-broker"},
		"vm list":                                {name: "vm list"},
		"vm ls":                                  {name: "vm list"},
		"vm status dev":                          {name: "vm status", vm: "dev"},
		"vm reset dev":                           {name: "vm reset", vm: "dev"},
		"host install --cache D:/c":              {name: "service install", cache: "D:/c"},
		"host install --cache=D:/c":              {name: "service install", cache: "D:/c"},
		"vm create dev":                          {name: "vm create", vm: "dev", create: api.CreateParams{VM: "dev"}},
		"vm create --cpus 4 dev":                 {name: "vm create", vm: "dev", create: api.CreateParams{VM: "dev", CPUs: 4}},
		"vm create dev --memory=2G":              {name: "vm create", vm: "dev", create: api.CreateParams{VM: "dev", MemoryMiB: 2048}},
		"vm create dev --memory 1536":            {name: "vm create", vm: "dev", create: api.CreateParams{VM: "dev", MemoryMiB: 1536}},
		"vm create dev --memory 512MiB":          {name: "vm create", vm: "dev", create: api.CreateParams{VM: "dev", MemoryMiB: 512}},
		"vm create dev --disk-size 64G":          {name: "vm create", vm: "dev", create: api.CreateParams{VM: "dev", DiskGiB: 64}},
		"vm create dev --disk-size 1t":           {name: "vm create", vm: "dev", create: api.CreateParams{VM: "dev", DiskGiB: 1024}},
		"vm start dev":                           {name: "vm start", vm: "dev"},
		"vm stop dev":                            {name: "vm stop", vm: "dev"},
		"vm stop --force dev":                    {name: "vm stop", vm: "dev", force: true},
		"vm reboot dev --timeout 30":             {name: "vm reboot", vm: "dev", timeout: 30},
		"vm wait dev":                            {name: "vm wait", vm: "dev"},
		"vm wait dev guest-ready --timeout=90":   {name: "vm wait", vm: "dev", state: "guest-ready", timeout: 90},
		"vm wait dev running":                    {name: "vm wait", vm: "dev", state: "running"},
		"vm configure dev --cpus 4":              {name: "vm configure", vm: "dev", create: api.CreateParams{VM: "dev", CPUs: 4}},
		"vm config dev --disk-size 64G":          {name: "vm configure", vm: "dev", create: api.CreateParams{VM: "dev", DiskGiB: 64}},
		"vm delete dev":                          {name: "vm delete", vm: "dev"},
		"vm rm -y dev":                           {name: "vm delete", vm: "dev", yes: true},
		"vm delete dev --yes":                    {name: "vm delete", vm: "dev", yes: true},
		"ssh dev":                                {name: "ssh", vm: "dev"},
		"ssh dev -- -t uname -a":                 {name: "ssh", vm: "dev", sshExtra: []string{"-t", "uname", "-a"}},
		"ssh-relay dev":                          {name: "ssh-relay", vm: "dev"},
		"exec dev -- uname -a":                   {name: "exec", vm: "dev", exec: api.ExecParams{VM: "dev", Argv: []string{"uname", "-a"}}},
		"exec --cwd /tmp dev --as user -- ls --": {name: "exec", vm: "dev", exec: api.ExecParams{VM: "dev", Argv: []string{"ls", "--"}, Cwd: "/tmp", As: "user"}},
		"cp ./a dev:/root/a":                     {name: "cp", src: cpPath{path: "./a"}, dst: cpPath{vm: "dev", path: "/root/a"}},
		"cp dev:/etc C:/tmp/etc":                 {name: "cp", src: cpPath{vm: "dev", path: "/etc"}, dst: cpPath{path: "C:/tmp/etc"}},
		"share add dev proj C:/src/proj":         {name: "share add", vm: "dev", share: share.Share{Name: "proj", HostPath: "C:/src/proj"}},
		"share add dev proj . --ro --exclude bin/ --exclude=/obj": {name: "share add", vm: "dev",
			share: share.Share{Name: "proj", HostPath: ".", Mode: share.RO, Exclude: []string{"bin/", "/obj"}}},
		"share rm dev proj":                                       {name: "share remove", vm: "dev", share: share.Share{Name: "proj"}},
		"share remove dev proj":                                   {name: "share remove", vm: "dev", share: share.Share{Name: "proj"}},
		"share ls dev":                                            {name: "share list", vm: "dev"},
		"share list dev":                                          {name: "share list", vm: "dev"},
		"share sync dev proj/src --verify":                        {name: "share sync", vm: "dev", ref: "proj/src", verify: true},
		"share status dev":                                        {name: "share status", vm: "dev"},
		"share status -w dev proj":                                {name: "share status", vm: "dev", share: share.Share{Name: "proj"}, watch: true},
		"vm console dev":                                          {name: "vm console", vm: "dev"},
		"vm console -f dev":                                       {name: "vm console", vm: "dev", watch: true},
		"vm create dev --distro debian":                           {name: "vm create", vm: "dev", create: api.CreateParams{VM: "dev", Distro: "debian"}},
		"vm create box --isolated --distro debian":                {name: "vm create", vm: "box", create: api.CreateParams{VM: "box", Distro: "debian", Isolated: true}},
		"egress ls box":                                           {name: "egress list", vm: "box"},
		"egress allow box api.example.com *.ubuntu.com:8443":      {name: "egress allow", vm: "box", hosts: []string{"api.example.com", "*.ubuntu.com:8443"}},
		"egress rm box api.example.com":                           {name: "egress remove", vm: "box", hosts: []string{"api.example.com"}},
		"egress log box --denied -f --lines 20":                   {name: "egress log", vm: "box", denied: true, watch: true, lines: 20},
		"egress mcp box on":                                       {name: "egress mcp", vm: "box", on: true},
		"vm harness install box":                                  {name: "harness install", vm: "box"},
		"vm harness install box --harness codex --no-host-config": {name: "harness install", vm: "box", harnesses: []string{"codex"}, noHostConfig: true},
		"vm harness add box codex":                                {name: "harness add", vm: "box", harnesses: []string{"codex"}},
		"vm harness add --no-host-config box agy":                 {name: "harness add", vm: "box", harnesses: []string{"agy"}, noHostConfig: true},
		"vm create box --isolated --harness claude --harness=codex": {name: "vm create", vm: "box",
			create: api.CreateParams{VM: "box", Isolated: true, Harnesses: []string{"claude", "codex"}}},
		"secret set gh --host github.com --host=api.github.com": {name: "secret set", names: []string{"gh"}, hosts: []string{"github.com", "api.github.com"}},
		"secret source gh --host github.com -- gh auth token --hostname github.com": {name: "secret source", names: []string{"gh"}, hosts: []string{"github.com"},
			argv: []string{"gh", "auth", "token", "--hostname", "github.com"}},
		"secret refresh gh":                              {name: "secret refresh", names: []string{"gh"}},
		"secret ls --json":                               {name: "secret list", json: true},
		"secret rm work.key-1":                           {name: "secret remove", names: []string{"work.key-1"}},
		"secret grant box gh npm":                        {name: "secret grant", vm: "box", names: []string{"gh", "npm"}},
		"secret revoke box gh":                           {name: "secret revoke", vm: "box", names: []string{"gh"}},
		"secret grants box":                              {name: "secret grants", vm: "box"},
		"secret set gh --host github.com --forge github": {name: "secret set", names: []string{"gh"}, hosts: []string{"github.com"}, forge: "github"},
		"secret source gl --forge=gitlab --host gitlab.example.com -- glab auth token": {name: "secret source", names: []string{"gl"}, hosts: []string{"gitlab.example.com"},
			forge: "gitlab", argv: []string{"glab", "auth", "token"}},
		"secret repos box":    {name: "secret repos", vm: "box", repoAction: "list"},
		"secret repos box ls": {name: "secret repos", vm: "box", repoAction: "list"},
		"secret repos box add github.com/o/a gitlab.example.com/team/*": {name: "secret repos", vm: "box", repoAction: "add",
			repos: []string{"github.com/o/a", "gitlab.example.com/team/*"}},
		"secret repos box rm github.com/o/a":                      {name: "secret repos", vm: "box", repoAction: "remove", repos: []string{"github.com/o/a"}},
		"secret login box codex":                                  {name: "secret login", vm: "box", names: []string{"codex"}},
		"secret logout box claude":                                {name: "secret logout", vm: "box", names: []string{"claude"}},
		"egress mcp box off":                                      {name: "egress mcp", vm: "box"},
		"vm create box --no-start":                                {name: "vm create", vm: "box", noStart: true, create: api.CreateParams{VM: "box"}},
		"vm create box --isolated --harness claude --no-sessions": {name: "vm create", vm: "box", noSessions: true, create: api.CreateParams{VM: "box", Isolated: true, Harnesses: []string{"claude"}}},
		"vm guest check dev":                                      {name: "guest check", vm: "dev"},
		"vm guest update dev":                                     {name: "guest update", vm: "dev"},
		"vm guest update --force dev --timeout 30":                {name: "guest update", vm: "dev", force: true, timeout: 30},
		"vm stats":               {name: "stats"},
		"vm stats dev -w --json": {name: "stats", vm: "dev", watch: true, json: true},
		"guide":                  {name: "guide"},
		"guide troubleshooting":  {name: "guide", topic: "troubleshooting"},
	} {
		got, err := parse(strings.Fields(line))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("parse(%q) = %+v, %v; want %+v", line, got, err, want)
		}
	}
	for _, line := range []string{"", "vm", "vm status", "vm status dev web", "vm status --json", "vm list extra", "vm start", "vm start dev web", "vm reset", "vm reset --force", "vm stop", "vm stop dev --hard", "vm stop --force", "vm wait dev up", "vm wait dev --timeout 0", "vm wait dev --timeout x", "vm reboot dev guest-ready", "vm configure dev", "vm configure --cpus 2", "vm configure dev --disk-size 1500M", "vm delete", "vm delete dev web", "vm delete dev --force", "vm delete --yes", "exec", "exec dev", "exec dev --", "exec -- ls", "exec dev web -- ls", "exec dev --as root -- id", "ssh", "ssh -- ls", "ssh dev ls", "ssh-relay dev -- x", "ssh-relay", "cp", "cp a b", "cp dev:/a web:/b", "cp a", "cp a dev:", "share", "share add", "share add dev proj", "share add dev proj C:/x extra", "share add dev proj C:/x --exclude", "share add dev proj C:/x --exclude=", "share add dev proj C:/x --rw", "share rm dev", "share rm dev proj --ro", "share list", "share list dev --exclude x", "share sync dev", "share sync dev proj x", "share sync dev proj --watch", "share status", "share status dev a b", "share status dev --verify", "service", "service start", "nope",
		"host install --cache", "host install --cache=", "host install extra", "host uninstall --cache D:/c", "host install -cache D:/c",
		"secret repos", "secret login box", "secret login box codex extra", "secret logout", "secret repos box add", "secret repos box list github.com/o/a", "secret repos box grant github.com/o/a", "secret grant box gh --forge github", "secret set gh --host github.com --forge", "vm create", "vm create dev web", "vm create dev --cpus", "vm create dev --cpus 0", "vm create dev --cpus two", "vm create dev --cpus 2 --cpus 3",
		"vm create dev --memory 4X", "vm create dev --memory 0", "vm create dev --memory 99999999T",
		"vm create dev --disk-size 64", "vm create dev --disk-size 1500M", "vm create dev --engine vz", "vm create dev --distro arch", "vm configure dev --distro ubuntu", "vm configure dev --isolated",
		"docker", "docker setup", "vm configure dev --no-start",
		"vm guest", "vm guest status dev", "vm guest check", "vm guest check dev --force", "vm guest update", "vm guest update dev --timeout 0", "vm guest update dev --timeout 7200", "vm guest update dev web",
		"vm harness", "vm harness add box", "vm harness add box codex extra", "vm harness install", "vm harness install box --harness", "vm harness remove box codex", "vm harness add box --harness codex", "vm stats a b", "vm stats --verbose", "guide nope", "guide docker", "guide troubleshooting extra"} {
		_, err := parse(strings.Fields(line))
		if _, ok := errors.AsType[usageError](err); !ok {
			t.Errorf("parse(%q) err = %v, want a usage error", line, err)
		}
	}
}

func TestPrintList(t *testing.T) {
	var b strings.Builder
	printList(&b, []vmRow{{"dev", "hcs", "stopped"}, {"a-very-long-vm-name-indeed", "hcs", "running"}}, "no VMs")
	want := "dev                     hcs   stopped\na-very-long-vm-name-indeedhcs   running\n"
	if b.String() != want {
		t.Fatalf("list =\n%q\nwant\n%q", b.String(), want)
	}
	b.Reset()
	printList(&b, nil, "no VMs")
	if b.String() != "no VMs\n" {
		t.Fatalf("empty list = %q", b.String())
	}
}

func ptr[T any](v T) *T { return &v }

func TestParseRejectsUngroupedNames(t *testing.T) {
	for _, line := range []string{"service install", "service uninstall", "guest check dev", "harness install box", "stats", "api-key list", "api-key set a"} {
		if _, err := parse(strings.Fields(line)); err == nil {
			t.Errorf("parse(%q) succeeded; the grouped spelling replaced it", line)
		}
	}
	for _, line := range []string{"version", "--version"} {
		if got, err := parse([]string{line}); err != nil || got.name != "version" {
			t.Errorf("parse(%q) = %+v, %v; want version", line, got, err)
		}
	}
}
