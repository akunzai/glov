package secret

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func store(t *testing.T) *Store {
	return &Store{Dir: filepath.Join(t.TempDir(), "secrets"), Values: &Memory{}}
}

func TestAStoredSecretKeepsOnlyItsLengthAndEndingInTheFile(t *testing.T) {
	s := store(t)
	const value = "ghp_TESTVALUE0123456789wxyz"
	d, err := s.Set("gh", []string{"API.github.com", "api.github.com", "*.githubusercontent.com"}, value, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Source != Stored || !slices.Equal(d.Hosts, []string{"api.github.com", "*.githubusercontent.com"}) || d.Length != len(value) || d.Ending != "wxyz" {
		t.Errorf("definition %+v", d)
	}
	raw, _ := os.ReadFile(filepath.Join(s.Dir, "secrets.json"))
	if strings.Contains(string(raw), "TESTVALUE") {
		t.Error("secrets.json holds the value")
	}
	if v, err := s.Value("gh"); err != nil || v != value {
		t.Errorf("Value: matches %v, %v", v == value, err)
	}
	if Describe(d.Length, d.Ending) != "27 characters, ending …wxyz" || Describe(1, "") != "1 character" {
		t.Errorf("Describe = %q", Describe(d.Length, d.Ending))
	}
}

func TestACommandSecretIsRefreshedAndNeverStoredAsStored(t *testing.T) {
	s := store(t)
	// Absolute on every OS the portable tests run on.
	argv := []string{filepath.Join(t.TempDir(), "gh.exe"), "auth", "token"}
	d, err := s.Set("gh", []string{"github.com"}, "first", argv, "")
	if err != nil || d.Source != Command || d.Length != 0 || !slices.Equal(d.Command, argv) {
		t.Fatalf("definition %+v, %v", d, err)
	}
	if _, err := s.Refresh("gh", "second"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Value("gh"); v != "second" {
		t.Errorf("Value after refresh = %q", v)
	}
	if _, err := s.Refresh("gh", ""); err == nil || !strings.Contains(err.Error(), "gh.exe printed nothing") {
		t.Errorf("empty refresh: %v", err)
	}
	if _, err := s.Set("npm", []string{"registry.npmjs.org"}, "x", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh("npm", "y"); err == nil || !strings.Contains(err.Error(), "glov secret set npm") {
		t.Errorf("refreshing a stored secret: %v", err)
	}
	if _, err := s.Set("rel", []string{"github.com"}, "x", []string{"gh", "auth", "token"}, ""); err == nil {
		t.Error("a relative command was taken")
	}
}

func TestDefinitionsAreCheckedAndRemovalTakesTheValue(t *testing.T) {
	s := store(t)
	for _, c := range []struct {
		name  string
		hosts []string
		value string
		want  string
	}{
		{"a/b", []string{"x.example"}, "v", "use letters"},
		{"ok", nil, "v", "at least one host with --host"},
		{"ok", []string{"http://x.example"}, "v", "not a valid egress entry"},
		{"ok", []string{"x.example"}, "", "the value is empty"},
	} {
		if _, err := s.Set(c.name, c.hosts, c.value, nil, ""); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Set(%q, %q): %v, want %q", c.name, c.hosts, err, c.want)
		}
	}
	if _, err := s.Set("gh", []string{"github.com"}, "v", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set("GH", []string{"github.com"}, "v", nil, ""); err == nil || !strings.Contains(err.Error(), "differs only in case") {
		t.Errorf("a name differing in case: %v", err)
	}
	if ok, err := s.Remove("gh"); !ok || err != nil {
		t.Fatalf("Remove = %v, %v", ok, err)
	}
	if _, err := s.Value("gh"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Value after Remove: %v", err)
	}
	if ok, _ := s.Remove("gh"); ok {
		t.Error("a second Remove found it")
	}
	if all, err := s.List(); err != nil || len(all) != 0 {
		t.Errorf("List = %v, %v", all, err)
	}
}

func TestAValueThatCannotBeReadNamesTheSecretOnly(t *testing.T) {
	s := store(t)
	if _, err := s.Set("gh", []string{"github.com"}, "tok-REAL", nil, ""); err != nil {
		t.Fatal(err)
	}
	s.Values.(*Memory).Fail = errors.New("decrypt failed")
	if _, err := s.Value("gh"); err == nil || err.Error() != "secret gh: decrypt failed" {
		t.Errorf("Value: %v", err)
	}
}
