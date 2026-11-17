package egress

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fixtures is the shared substitution cases (testdata/egress at the
// repository root), which macOS's tests read too.
func fixtures(t *testing.T) (f struct {
	Definitions []struct {
		Name  string   `json:"name"`
		Hosts []string `json:"hosts"`
	} `json:"definitions"`
	Cases []struct {
		Name    string   `json:"name"`
		Host    string   `json:"host"`
		Port    int      `json:"port"`
		VM      string   `json:"vm"`
		Granted []string `json:"granted"`
		Head    string   `json:"head"`
		Plan    *struct {
			Secrets []string
			Path    string
		} `json:"plan"`
		Refusal *struct {
			Status int
			Reason string
		} `json:"refusal"`
		Values       map[string]string `json:"values"`
		Output       *string           `json:"output"`
		ApplyRefusal *struct {
			Status int
			Reason string
		} `json:"applyRefusal"`
	} `json:"cases"`
	PlainHTTP []struct {
		Head     string `json:"head"`
		Mentions bool   `json:"mentionsPlaceholder"`
	} `json:"plainHTTP"`
}) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "egress", "substitution.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &f, json.MatchCaseInsensitiveNames(true)); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("no cases")
	}
	return f
}

func TestSharedSubstitutionCases(t *testing.T) {
	f := fixtures(t)
	var defined []Secret
	for _, d := range f.Definitions {
		defined = append(defined, Secret{Name: d.Name, Hosts: d.Hosts})
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			p, r := NewPlan([]byte(c.Head), c.Host, c.Port, c.VM, c.Granted, defined, "on the host")
			if c.Refusal != nil {
				if r == nil || r.Status != c.Refusal.Status || r.Reason != c.Refusal.Reason {
					t.Fatalf("refusal = %+v, want %+v", r, *c.Refusal)
				}
				return
			}
			if r != nil {
				t.Fatalf("refused: %+v", r)
			}
			if !slices.Equal(p.Secrets, c.Plan.Secrets) && !(len(p.Secrets) == 0 && len(c.Plan.Secrets) == 0) || p.Path != c.Plan.Path {
				t.Fatalf("plan secrets %q path %q, want %q %q", p.Secrets, p.Path, c.Plan.Secrets, c.Plan.Path)
			}
			if c.Values == nil {
				return
			}
			out, ar := p.Apply(c.Values)
			if c.ApplyRefusal != nil {
				if ar == nil || ar.Status != c.ApplyRefusal.Status || ar.Reason != c.ApplyRefusal.Reason {
					t.Fatalf("apply refusal = %+v, want %+v", ar, *c.ApplyRefusal)
				}
				return
			}
			if ar != nil || string(out) != *c.Output {
				t.Fatalf("apply = %q (%+v), want %q", out, ar, *c.Output)
			}
		})
	}
	for _, c := range f.PlainHTTP {
		if MentionsPlaceholder([]byte(c.Head)) != c.Mentions {
			t.Errorf("MentionsPlaceholder(%q) != %v", c.Head, c.Mentions)
		}
	}
}

func TestRefusalsNameTheFixAndNeverAValue(t *testing.T) {
	defined := []Secret{{Name: "gh", Hosts: []string{"api.example.com"}}, {Name: "npm", Hosts: []string{"registry.example.org"}}}
	head := "GET / HTTP/1.1\r\nHost: api.example.com\r\nA: glov-secret:%s\r\n\r\n"
	_, r := NewPlan([]byte(strings.Replace(head, "%s", "gh", 1)), "api.example.com", 443, "box", nil, defined, "on the Windows host")
	if r.Message != "VM box was not granted secret gh: run glov secret grant box gh on the Windows host" {
		t.Errorf("not granted: %q", r.Message)
	}
	_, r = NewPlan([]byte(strings.Replace(head, "%s", "nope", 1)), "api.example.com", 443, "box", nil, defined, "on the Windows host")
	if r.Message != "no secret named nope: define it on the Windows host with glov secret set or glov secret source, then glov secret grant box nope" {
		t.Errorf("undefined: %q", r.Message)
	}
	_, r = NewPlan([]byte(strings.Replace(head, "%s", "npm", 1)), "api.example.com", 443, "box", []string{"npm"}, defined, "on the Windows host")
	if r.Message != "secret npm is not bound to api.example.com: it may go to registry.example.org only" {
		t.Errorf("not bound: %q", r.Message)
	}
}
