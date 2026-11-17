package egress

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// repoFixtures is the shared repository cases (testdata/egress/repos.json),
// which macOS's tests read too.
func repoFixtures(t *testing.T) (f struct {
	Patterns []struct {
		Text    string `json:"text"`
		Pattern string `json:"pattern"`
	} `json:"patterns"`
	InvalidPatterns []string `json:"invalidPatterns"`
	Matches         []struct {
		Pattern, Host, Repo string
		Matches             bool
	} `json:"matches"`
	RepoHosts []struct {
		Forge    Forge
		Host     string
		RepoHost string `json:"repoHost"`
	} `json:"repoHosts"`
	Targets []struct {
		Forge              Forge
		Host, Path, Target string
	} `json:"targets"`
	Refusals struct {
		Allowed []string
		Cases   []struct {
			Target, Method, Host string
			Refusal              *string
		}
	} `json:"refusals"`
	GraphQL struct {
		Host            string
		Allowed         []string
		Nodes           map[string]*string
		AllowedRequests []struct {
			Query     string
			Variables map[string]any
		} `json:"allowedRequests"`
		RefusedRequests []struct {
			Query     string
			Variables map[string]any
			Refusal   *string
		} `json:"refusedRequests"`
		RefusedBodies []struct{ Body, Refusal string } `json:"refusedBodies"`
		NodeIDs       []struct {
			Query     string
			Variables map[string]any
			NodeIDs   []string `json:"nodeIDs"`
		} `json:"nodeIDs"`
	} `json:"graphQL"`
}) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "egress", "repos.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &f, json.MatchCaseInsensitiveNames(true)); err != nil {
		t.Fatal(err)
	}
	return f
}

// describe is a target as the fixture writes it.
func describe(r RepoTarget) string {
	switch r.Kind {
	case TargetRepo:
		return "repo:" + r.Repo
	case TargetNone:
		return "none"
	case TargetGraphQL:
		return "graphQL"
	}
	return "unknown"
}

func TestSharedRepositoryPatterns(t *testing.T) {
	f := repoFixtures(t)
	for _, c := range f.Patterns {
		if p, ok := ParseRepoPattern(c.Text); !ok || p.String() != c.Pattern {
			t.Errorf("%q = %q, %v; want %q", c.Text, p.String(), ok, c.Pattern)
		}
	}
	for _, text := range f.InvalidPatterns {
		if p, ok := ParseRepoPattern(text); ok {
			t.Errorf("%q = %q, want invalid", text, p.String())
		}
	}
	for _, c := range f.Matches {
		p, _ := ParseRepoPattern(c.Pattern)
		if p.Matches(c.Host, c.Repo) != c.Matches {
			t.Errorf("%s matches %s %s: %v", c.Pattern, c.Host, c.Repo, !c.Matches)
		}
	}
	for _, c := range f.RepoHosts {
		if got := RepoHost(c.Forge, c.Host); got != c.RepoHost {
			t.Errorf("RepoHost(%s, %s) = %s", c.Forge, c.Host, got)
		}
	}
}

func TestSharedRepositoryTargets(t *testing.T) {
	f := repoFixtures(t)
	if len(f.Targets) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range f.Targets {
		if got := RepoTargetOf(c.Forge, c.Host, c.Path); describe(got) != c.Target {
			t.Errorf("%s %s%s = %+v, want %s", c.Forge, c.Host, c.Path, got, c.Target)
		}
	}
	var allowed []RepoPattern
	for _, text := range f.Refusals.Allowed {
		p, _ := ParseRepoPattern(text)
		allowed = append(allowed, p)
	}
	for _, c := range f.Refusals.Cases {
		var target RepoTarget
		switch kind, repo, _ := strings.Cut(c.Target, ":"); kind {
		case "repo":
			target = repoTarget(repo)
		case "none":
			target = RepoTarget{Kind: TargetNone}
		case "graphQL":
			target = RepoTarget{Kind: TargetGraphQL}
		}
		want := ""
		if c.Refusal != nil {
			want = *c.Refusal
		}
		if got := RepoRefusal(target, c.Method, c.Host, allowed); got != want {
			t.Errorf("%s %s on %s: %q, want %q", c.Method, c.Target, c.Host, got, want)
		}
	}
	if got := RepoRefusal(unknownTarget("why"), "GET", "github.com", allowed); got != "why" {
		t.Errorf("unknown: %q", got)
	}
}

func TestSharedGraphQLCases(t *testing.T) {
	g := repoFixtures(t).GraphQL
	nodes := map[string]string{}
	for id, repo := range g.Nodes {
		nodes[id] = ""
		if repo != nil {
			nodes[id] = *repo
		}
	}
	refusal := func(query string, variables map[string]any) string {
		body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
		if err != nil {
			t.Fatal(err)
		}
		r, why := ParseGraphQL(body)
		if why != "" {
			return why
		}
		return GraphQLRefusal(r, g.Host, nodes, func(repo string) bool { return slices.Contains(g.Allowed, repo) })
	}
	for _, c := range g.AllowedRequests {
		if why := refusal(c.Query, c.Variables); why != "" {
			t.Errorf("refused (%s): %s", why, c.Query)
		}
	}
	for _, c := range g.RefusedRequests {
		why := refusal(c.Query, c.Variables)
		if why == "" || c.Refusal != nil && why != *c.Refusal {
			t.Errorf("%s: refusal %q, want %v", c.Query, why, c.Refusal)
		}
	}
	for _, c := range g.RefusedBodies {
		if _, why := ParseGraphQL([]byte(c.Body)); why != c.Refusal {
			t.Errorf("%s: %q, want %q", c.Body, why, c.Refusal)
		}
	}
	for _, c := range g.NodeIDs {
		body, _ := json.Marshal(map[string]any{"query": c.Query, "variables": c.Variables})
		if r, why := ParseGraphQL(body); why != "" || !slices.Equal(r.NodeIDs, c.NodeIDs) {
			t.Errorf("%s: node IDs %q (%s), want %q", c.Query, r.NodeIDs, why, c.NodeIDs)
		}
	}
}

func TestNodeAnswersNameRepositoriesOrPeople(t *testing.T) {
	got, err := parseNodes([]byte(`{"data":{"nodes":[{"__typename":"Repository","nameWithOwner":"Owner/App"},`+
		`{"__typename":"PullRequest","repository":{"nameWithOwner":"owner/app"}},{"__typename":"User"}]}}`), []string{"R", "PR", "U"})
	if err != nil || got["R"] != "owner/app" || got["PR"] != "owner/app" || got["U"] != "" || len(got) != 3 {
		t.Fatalf("%v, %v", got, err)
	}
	for body, want := range map[string]string{
		`{"data":{"nodes":[null]}}`:                  "node X does not exist",
		`{"data":{"nodes":[{"__typename":"Gist"}]}}`: "node X is a Gist, which the proxy cannot tie to a repository",
		`{"data":{"nodes":[]}}`:                      "looking up node IDs gave no answer",
		`{"errors":[{"message":"Bad credentials"}]}`: "looking up node IDs gave no answer",
	} {
		if _, err := parseNodes([]byte(body), []string{"X"}); err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %s", body, err, want)
		}
	}
	if nodesEndpoint("API.github.com") != "https://api.github.com/graphql" || nodesEndpoint("ghe.example.com") != "https://ghe.example.com/api/graphql" {
		t.Error("endpoint")
	}
}
