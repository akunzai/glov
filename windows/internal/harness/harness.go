// Package harness is the coding-agent harness profiles an isolated VM can be
// created with (glov vm create --isolated --harness <name>): the secrets a
// harness uses and how each reaches it, the environment and home-directory
// files that turn its optional traffic off, and the hosts it must reach.
// The profiles are macOS's (Sources/GlovCore/Harnesses) byte for byte.
package harness

import (
	"bytes"
	"embed"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"maps"
	"slices"
	"strings"
)

//go:embed profiles/*.json
var files embed.FS

// File is a file in the guest user's home, as JSON (written indented) or
// text.
type File struct {
	// Path is relative to the home directory.
	Path string         `json:"path"`
	JSON jsontext.Value `json:"json,omitempty"`
	Text *string        `json:"text,omitempty"`
}

// Content is what the file holds.
func (f File) Content() string {
	if f.Text != nil {
		return *f.Text
	}
	var b bytes.Buffer
	if err := json.Indent(&b, f.JSON, "", "  "); err != nil {
		return "{}\n"
	}
	return b.String() + "\n"
}

// Profile is one harness's settings.
type Profile struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	// Command is what the harness runs as, installed to /usr/local/bin.
	Command  string   `json:"command"`
	Secrets  []Secret `json:"secrets"`
	Verified struct {
		Version string `json:"version"`
		Date    string `json:"date"`
	} `json:"verified"`
	Docs []string `json:"docs"`
	// Environment is set system-wide, in /etc/environment.
	Environment map[string]string `json:"environment"`
	Files       []File            `json:"files"`
	// Runtime is the egress allowlist it needs to run.
	Runtime []string `json:"runtime"`
	// Install is Runtime plus where the harness installs from.
	Install []string `json:"install"`
	// InstallScript runs as the guest user under the Install allowlist.
	InstallScript string `json:"installScript"`
	// UserNamespaces: its own sandbox needs unprivileged user namespaces,
	// which Ubuntu restricts; only VMs with such a harness lift that.
	UserNamespaces bool     `json:"userNamespaces"`
	Notes          []string `json:"notes"`
	// Subscription is how a VM uses the owner's subscription instead of an
	// API key (glov secret login), nil when it cannot.
	Subscription *Subscription `json:"subscription,omitempty"`
	// HostConfig is the owner's config that glov vm harness install copies
	// into the guest (StageHostConfig); Sessions is where the harness
	// writes its conversation records, below the guest home, which a VM
	// with the harness-sessions share links into it.
	HostConfig []HostCopy `json:"hostConfig,omitempty"`
	Sessions   []string   `json:"sessions,omitempty"`
}

// HostCopy is a path below the owner's profile folder and where its copy
// goes below the guest home, both '/'-separated.
type HostCopy struct {
	Host  string `json:"host"`
	Guest string `json:"guest"`
}

// Subscription is a VM signed in to the owner's subscription: its own
// credentials, kept on the host, and the guest set up for them in place of
// the API key. Each secret is the VM's own, named <vm>.<harness>.<name>
// (SecretName).
type Subscription struct {
	// SignIn runs in the VM as the guest user, in a terminal, to sign in
	// (the harness's own device sign-in); nil when the owner pastes a
	// token made elsewhere instead, as PasteHint says.
	SignIn    []string `json:"signIn,omitempty"`
	PasteHint string   `json:"pasteHint,omitempty"`
	// SignOut runs in the VM as the guest user at glov secret logout,
	// while the proxy still holds the credentials.
	SignOut []string `json:"signOut,omitempty"`
	// Secrets replace the API-key Secrets while signed in. One without Env
	// or File reaches the guest in a token endpoint's answer.
	Secrets []Secret `json:"secrets"`
	// TokenEndpoints are where the harness gets and refreshes its tokens.
	TokenEndpoints []TokenEndpoint `json:"tokenEndpoints"`
	// Environment is added to the profile's while signed in.
	Environment map[string]string `json:"environment"`
	// Settings are the single settings that differ while signed in.
	Settings []Setting `json:"settings"`
	// Runtime is added to the allowlist while signed in.
	Runtime []string `json:"runtime"`
}

// Setting is one setting in a home file: a key path in a JSON file, or a
// top-level key of a TOML file (Path ends in .toml). While signed in it is
// Value, or absent when Value is empty; otherwise it is what the profile's
// own file says.
type Setting struct {
	Path  string         `json:"path"`
	Key   []string       `json:"key"`
	Value jsontext.Value `json:"value,omitempty"`
}

// IsTOML reports a TOML file's setting.
func (s Setting) IsTOML() bool { return strings.HasSuffix(s.Path, ".toml") }

// TokenEndpoint is where a subscription's tokens are got and refreshed: the
// proxy puts the real values into the request body and keeps the tokens of
// the answer (Answer: JSON field to secret name) on the host.
type TokenEndpoint struct {
	Host string `json:"host"`
	// Path is matched against the request path without its query.
	Path   string            `json:"path"`
	Answer map[string]string `json:"answer,omitempty"`
}

// SecretName is the name of subscription secret name of VM vm.
func (p Profile) SecretName(vm, name string) string { return vm + "." + p.Name + "." + name }

// SignedIn is the profile as VM vm signed in to its subscription sees it:
// the subscription's secrets (named for vm) in place of the API key, and its
// environment and allowlist added (its Settings are applied by the mode
// functions). The profile itself when it has no subscription.
func (p Profile) SignedIn(vm string) Profile {
	sub := p.Subscription
	if sub == nil {
		return p
	}
	p.Secrets = nil
	for _, s := range sub.Secrets {
		s.Name = p.SecretName(vm, s.Name)
		p.Secrets = append(p.Secrets, s)
	}
	p.Environment = maps.Clone(p.Environment)
	if p.Environment == nil {
		p.Environment = map[string]string{}
	}
	maps.Copy(p.Environment, sub.Environment)
	runtime := slices.Clone(p.Runtime)
	for _, h := range sub.Runtime {
		if !slices.Contains(runtime, h) {
			runtime = append(runtime, h)
		}
	}
	p.Runtime = runtime
	return p
}

// Deliveries of a secret.
const (
	// Placeholder: the guest holds glov-secret:<name>, which the egress
	// proxy swaps for the value on the way to Hosts.
	Placeholder = "placeholder"
	// Tmpfs: the Guest Service writes the value to TmpfsDir/<name> (mode
	// 0400, the guest user's), never to the VM's disk.
	Tmpfs = "tmpfs"
)

// TmpfsDir is where a tmpfs secret's file is in the guest.
const TmpfsDir = "/run/glov/secrets"

// Secret is a secret the harness uses (glov secret), granted per VM.
type Secret struct {
	Name     string `json:"name"`
	Delivery string `json:"delivery"`
	// Env is an environment variable (in /etc/environment) set to the
	// placeholder, or for tmpfs to the file's path.
	Env string `json:"env,omitempty"`
	// File is a file in the guest user's home: for placeholder the
	// profile's JSON file of that path (or a new one) with the placeholder
	// at JSON; for tmpfs a symbolic link to the file in TmpfsDir.
	File string   `json:"file,omitempty"`
	JSON []string `json:"json,omitempty"`
	// Hosts are where the value is sent, for glov secret set --host.
	Hosts []string `json:"hosts"`
}

// PlaceholderText is glov-secret:<name>.
func (s Secret) PlaceholderText() string { return "glov-secret:" + s.Name }

// TmpfsPath is the tmpfs file's path in the guest.
func (s Secret) TmpfsPath() string { return TmpfsDir + "/" + s.Name }

// GuestEnvironment is Environment plus each secret's variable.
func (p Profile) GuestEnvironment() map[string]string {
	env := maps.Clone(p.Environment)
	if env == nil {
		env = map[string]string{}
	}
	for _, s := range p.Secrets {
		if s.Env == "" {
			continue
		}
		env[s.Env] = s.PlaceholderText()
		if s.Delivery == Tmpfs {
			env[s.Env] = s.TmpfsPath()
		}
	}
	return env
}

// GuestFiles is Files with each placeholder secret written into its JSON
// file (a new file when the profile has none).
func (p Profile) GuestFiles() []File {
	all := slices.Clone(p.Files)
	for _, s := range p.Secrets {
		if s.Delivery != Placeholder || s.File == "" || len(s.JSON) == 0 {
			continue
		}
		i := slices.IndexFunc(all, func(f File) bool { return f.Path == s.File })
		if i < 0 {
			all = append(all, File{Path: s.File, JSON: jsontext.Value("{}")})
			i = len(all) - 1
		}
		var doc any = map[string]any{}
		if len(all[i].JSON) > 0 {
			json.Unmarshal(all[i].JSON, &doc)
		}
		doc = setting(doc, s.JSON, s.PlaceholderText())
		data, _ := json.Marshal(doc)
		all[i].JSON = data
	}
	return all
}

func setting(v any, keys []string, value string) any {
	if len(keys) == 0 {
		return value
	}
	o, _ := v.(map[string]any)
	if o == nil {
		o = map[string]any{}
	}
	o[keys[0]] = setting(o[keys[0]], keys[1:], value)
	return o
}

// SecretLink is a home-relative path that links to a tmpfs secret's file.
type SecretLink struct{ Path, Target string }

// SecretLinks is the links to the tmpfs secrets' files.
func (p Profile) SecretLinks() []SecretLink {
	var out []SecretLink
	for _, s := range p.Secrets {
		if s.Delivery == Tmpfs && s.File != "" {
			out = append(out, SecretLink{s.File, s.TmpfsPath()})
		}
	}
	return out
}

// SecretNeeds is what glov vm create says about each secret of a new VM,
// none of them granted yet; defined holds the names glov secret list
// knows, nil when glovd could not say.
func (p Profile) SecretNeeds(vm string, defined []string) []string {
	var out []string
	for _, s := range p.Secrets {
		where := s.Env
		if where == "" {
			where = "~/" + s.File
			if len(s.JSON) > 0 {
				where += " at " + strings.Join(s.JSON, ".")
			}
		}
		how := "a placeholder in " + where
		if s.Delivery == Tmpfs {
			how = "the value in " + s.TmpfsPath() + ", linked from " + where
		}
		set := "glov secret set " + s.Name + " --host " + strings.Join(s.Hosts, " --host ")
		grant := "glov secret grant " + vm + " " + s.Name
		var todo string
		switch {
		case defined == nil:
			todo = "not granted: " + grant + " (define it first if glov secret list lacks it: " + set + ")"
		case slices.Contains(defined, s.Name):
			todo = "defined, not granted: " + grant
		default:
			todo = "not defined or granted: " + set + ", then " + grant
		}
		out = append(out, fmt.Sprintf("harness %s: secret %s (%s, sent to %s) is %s", p.Name, s.Name, how, strings.Join(s.Hosts, ", "), todo))
	}
	return out
}

// All is every profile, sorted by name.
var All = func() []Profile {
	entries, err := files.ReadDir("profiles")
	if err != nil {
		panic(err)
	}
	var all []Profile
	for _, e := range entries {
		data, err := files.ReadFile("profiles/" + e.Name())
		if err != nil {
			panic(err)
		}
		var p Profile
		if err := json.Unmarshal(data, &p); err != nil {
			panic(fmt.Sprintf("harness profile %s: %v", e.Name(), err))
		}
		all = append(all, p)
	}
	slices.SortFunc(all, func(a, b Profile) int { return strings.Compare(a.Name, b.Name) })
	return all
}()

// Names is "antigravity, claude or codex".
func Names() string {
	var names []string
	for _, p := range All {
		names = append(names, p.Name)
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

// Named is the profile called name.
func Named(name string) (Profile, error) {
	for _, p := range All {
		if p.Name == name {
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("unknown harness %s: use %s", name, Names())
}

// Resolve is the profiles called names, each once, in the order given.
func Resolve(names []string) ([]Profile, error) {
	var out []Profile
	for _, n := range names {
		p, err := Named(n)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(out, func(q Profile) bool { return q.Name == n }) {
			out = append(out, p)
		}
	}
	return out, nil
}
