// Package secret keeps the secrets the owner names on this host (glov
// secret): each one's hosts and source in secrets.json, its value
// encrypted beside it. glovd alone reads values, for the egress proxy and
// a harness's tmpfs; no RPC method returns one (docs/networking.md
// "Secrets").
package secret

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/akunzai/glov/windows/internal/egress"
)

// ValidName reports a secret name: letters, digits, '.', '_' and '-', at
// most 64.
func ValidName(name string) error {
	if name == "" || len(name) > 64 || name == "." || name == ".." || strings.ContainsFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-')
	}) {
		return fmt.Errorf("secret name %s: use letters, digits, '.', '_' or '-', at most 64", name)
	}
	return nil
}

// Sources of a value.
const (
	// Stored is typed or piped into glov secret set.
	Stored = "stored"
	// Command is the stdout of a command glov runs as the user (glov
	// secret source), at source, grant and refresh.
	Command = "command"
)

// Definition is one secret, without its value.
type Definition struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	// Command is a Command secret's argv, its program an absolute path.
	Command []string `json:"command,omitempty"`
	// Hosts are where the value may be sent: egress entries in canonical
	// form.
	Hosts []string `json:"hosts"`
	// Length and Ending (its last 4 characters, with at least 16) are what
	// may be shown of a stored value, kept so glov secret list reads none.
	Length int    `json:"length,omitzero"`
	Ending string `json:"ending,omitempty"`
	// Forge makes the secret a git sign-in (glov secret set --forge): the
	// guest's git, gh and glab sign in with it, and each VM granted it
	// reaches only its repositories on its hosts (glov secret repos).
	Forge egress.Forge `json:"forge,omitempty"`
}

// Ending is the last 4 characters of a value of at least 16, else "".
func Ending(value string) string {
	r := []rune(value)
	if len(r) < 16 {
		return ""
	}
	return string(r[len(r)-4:])
}

// Describe is what may be shown of a value: its length and ending.
func Describe(length int, ending string) string {
	s := fmt.Sprintf("%d character", length)
	if length != 1 {
		s += "s"
	}
	if ending != "" {
		s += ", ending …" + ending
	}
	return s
}

// OddCharacters counts the whitespace and control characters in value,
// which tokens never have.
func OddCharacters(value string) int {
	n := 0
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			n++
		}
	}
	return n
}

// Values is secret values by name.
type Values interface {
	Set(name, value string) error
	// Get is false if there is none.
	Get(name string) (string, bool, error)
	Remove(name string) (bool, error)
}

// Store is the definitions in Dir's secrets.json and their Values.
type Store struct {
	Dir    string
	Values Values
	mu     sync.Mutex
}

// ErrNotFound names a secret nobody defined.
var ErrNotFound = errors.New("no such secret")

func (s *Store) path() string { return filepath.Join(s.Dir, "secrets.json") }

// List is every definition, sorted by name.
func (s *Store) List() ([]Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Egress is every definition as the egress proxy needs it.
func (s *Store) Egress() ([]egress.Secret, error) {
	all, err := s.List()
	var out []egress.Secret
	for _, d := range all {
		out = append(out, egress.Secret{Name: d.Name, Hosts: d.Hosts, Forge: d.Forge})
	}
	return out, err
}

// Get is the definition called name.
func (s *Store) Get(name string) (Definition, bool, error) {
	all, err := s.List()
	i := slices.IndexFunc(all, func(d Definition) bool { return d.Name == name })
	if err != nil || i < 0 {
		return Definition{}, false, err
	}
	return all[i], true, nil
}

func (s *Store) load() ([]Definition, error) {
	data, err := os.ReadFile(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return []Definition{}, nil
	}
	var all []Definition
	if err == nil {
		err = json.Unmarshal(data, &all)
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %v", s.path(), err)
	}
	return all, nil
}

func (s *Store) save(all []Definition) error {
	slices.SortFunc(all, func(a, b Definition) int { return strings.Compare(a.Name, b.Name) })
	data, err := json.Marshal(all, json.Deterministic(true))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(s.path()+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(s.path()+".tmp", s.path())
}

// canonicalHosts is hosts in canonical form, each once.
func canonicalHosts(name string, hosts []string) ([]string, error) {
	if len(hosts) == 0 {
		return nil, fmt.Errorf("secret %s: bind it to at least one host with --host", name)
	}
	out, err := egress.Allowing(nil, hosts)
	if err != nil {
		return nil, fmt.Errorf("secret %s: %v", name, err)
	}
	return out, nil
}

// Set defines name with value, replacing any secret by that name: a
// Stored secret, or with command, a Command secret whose value the
// command just printed; with forge, a git sign-in.
func (s *Store) Set(name string, hosts []string, value string, command []string, forge egress.Forge) (Definition, error) {
	if err := ValidName(name); err != nil {
		return Definition{}, err
	}
	hosts, err := canonicalHosts(name, hosts)
	if err != nil {
		return Definition{}, err
	}
	if value == "" {
		return Definition{}, fmt.Errorf("secret %s: the value is empty", name)
	}
	if forge != "" && !slices.Contains(egress.Forges, forge) {
		return Definition{}, fmt.Errorf("forge %s: use github or gitlab", forge)
	}
	d := Definition{Name: name, Source: Stored, Hosts: hosts, Length: len([]rune(value)), Ending: Ending(value)}
	if command != nil {
		if !filepath.IsAbs(command[0]) {
			return Definition{}, fmt.Errorf("secret %s: give the command with an absolute path", name)
		}
		d = Definition{Name: name, Source: Command, Command: command, Hosts: hosts}
	}
	d.Forge = forge
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.load()
	if err != nil {
		return Definition{}, err
	}
	// Values are files, and NTFS ignores case.
	if i := slices.IndexFunc(all, func(o Definition) bool { return strings.EqualFold(o.Name, name) && o.Name != name }); i >= 0 {
		return Definition{}, fmt.Errorf("secret %s: %s differs only in case; remove it first or use another name", name, all[i].Name)
	}
	if err := s.Values.Set(name, value); err != nil {
		return Definition{}, err
	}
	all = slices.DeleteFunc(all, func(o Definition) bool { return o.Name == name })
	return d, s.save(append(all, d))
}

// Refresh replaces a Command secret's value with what its command just
// printed.
func (s *Store) Refresh(name, value string) (Definition, error) {
	// Under the lock, so a Set or Remove meanwhile is not undone.
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.load()
	i := slices.IndexFunc(all, func(d Definition) bool { return d.Name == name })
	ok := i >= 0
	var d Definition
	if ok {
		d = all[i]
	}
	switch {
	case err != nil:
		return Definition{}, err
	case !ok:
		return Definition{}, fmt.Errorf("%w named %s", ErrNotFound, name)
	case d.Source != Command:
		return Definition{}, fmt.Errorf("secret %s is not taken from a command: store it again with glov secret set %s", name, name)
	case value == "":
		return Definition{}, fmt.Errorf("secret %s: %s printed nothing", name, filepath.Base(d.Command[0]))
	}
	return d, s.Values.Set(name, value)
}

// Remove deletes name and its value; false if there was none.
func (s *Store) Remove(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.load()
	if err != nil {
		return false, err
	}
	n := len(all)
	all = slices.DeleteFunc(all, func(d Definition) bool { return d.Name == name })
	if len(all) == n {
		return false, nil
	}
	if _, err := s.Values.Remove(name); err != nil {
		return false, err
	}
	return true, s.save(all)
}

// Value is name's value. Its error names the secret, never a value.
func (s *Store) Value(name string) (string, error) {
	d, ok, err := s.Get(name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w named %s", ErrNotFound, name)
	}
	v, ok, err := s.Values.Get(name)
	if err != nil {
		return "", fmt.Errorf("secret %s: %v", name, err)
	}
	if !ok {
		how := "store it again with glov secret set " + name
		if d.Source == Command {
			how = "run glov secret refresh " + name
		}
		return "", fmt.Errorf("secret %s has no stored value: %s", name, how)
	}
	return v, nil
}

// Memory keeps values in memory, for tests.
type Memory struct {
	mu     sync.Mutex
	values map[string]string
	// Fail makes every read fail with it.
	Fail error
}

func (m *Memory) Set(name, value string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.values == nil {
		m.values = map[string]string{}
	}
	m.values[name] = value
	return nil
}

func (m *Memory) Get(name string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return "", false, m.Fail
	}
	v, ok := m.values[name]
	return v, ok, nil
}

func (m *Memory) Remove(name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.values[name]
	delete(m.values, name)
	return ok, nil
}
