package egress

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// repoProxy is a proxy for VM box granted git secret gh (forge github) for
// api.example.com, a GitHub Enterprise host, with repos; node IDs resolve
// through nodes ("" a person, missing: no such node).
func repoProxy(t *testing.T, u *upstream, ca *CA, repos []string, nodes map[string]string) *tlsProxy {
	tp := newTLSProxy(t, u, ca)
	tp.Settings = func() (Settings, error) {
		return Settings{Allow: []string{"api.example.com"}, Secrets: []string{"gh"}, Repos: repos}, nil
	}
	tp.Secrets = func() ([]Secret, error) {
		return []Secret{{Name: "gh", Hosts: []string{"api.example.com"}, Forge: GitHub}}, nil
	}
	tp.Value = func(string) (string, error) { return "tok-REAL", nil }
	tp.Nodes = func(_ context.Context, host, token string, ids []string) (map[string]string, error) {
		if host != "api.example.com" || token != "tok-REAL" {
			t.Errorf("node lookup at %s with %s", host, token)
		}
		out := map[string]string{}
		for _, id := range ids {
			repo, ok := nodes[id]
			if !ok {
				return nil, errors.New("node " + id + " does not exist")
			}
			out[id] = repo
		}
		return out, nil
	}
	return tp
}

func get(path string, signedIn bool) string {
	auth := ""
	if signedIn {
		auth = "Authorization: token glov-secret:gh\r\n"
	}
	return "GET " + path + " HTTP/1.1\r\nHost: api.example.com\r\n" + auth + "\r\n"
}

func graphQL(t *testing.T, query string, variables map[string]any) string {
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		t.Fatal(err)
	}
	return "POST /api/graphql HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: bearer glov-secret:gh\r\nContent-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + string(body)
}

func TestASignedInRequestReachesOnlyTheVMsRepositories(t *testing.T) {
	u, ca := newUpstream(t), egressCA(t)
	tp := repoProxy(t, u, ca, []string{"api.example.com/owner/app"}, nil)
	ok, err := tp.exchange(t, get("/api/v3/repos/Owner/App/pulls", true), ca)
	if err != nil || !strings.HasPrefix(ok, "HTTP/1.1 200 OK") {
		t.Fatalf("%q, %v", ok, err)
	}
	if seen := u.seen(); len(seen) != 1 || !strings.Contains(seen[0], "Authorization: token tok-REAL") {
		t.Fatalf("upstream saw %q", seen)
	}
	other, _ := tp.exchange(t, get("/api/v3/repos/owner/secret-project/contents/README.md", true), ca)
	if !strings.HasPrefix(other, "HTTP/1.1 403 Forbidden") || !strings.Contains(other, "glov secret repos box add") {
		t.Errorf("other repository: %q", other)
	}
	write, _ := tp.exchange(t, "POST /api/v3/user/repos HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: token glov-secret:gh\r\nContent-Length: 0\r\n\r\n", ca)
	if !strings.HasPrefix(write, "HTTP/1.1 403 Forbidden") {
		t.Errorf("write of no repository: %q", write)
	}
	if len(u.seen()) != 1 {
		t.Errorf("something refused went upstream: %q", u.seen())
	}
	lines, _ := tp.snapshot()
	if len(lines) != 3 || !strings.Contains(lines[1], "deny api.example.com:443 GET /api/v3/repos/owner/secret-project/contents/README.md 403") ||
		!strings.HasSuffix(lines[1], "(repository api.example.com/owner/secret-project is not allowed)") {
		t.Errorf("log %q", lines)
	}
	if strings.Contains(strings.Join(lines, ""), "REAL") {
		t.Error("the log holds the value")
	}
}

func TestAnAnonymousRequestReachesAnyRepository(t *testing.T) {
	// A public clone or package fetch carries no sign-in: it reaches what
	// anyone on the internet can.
	u, ca := newUpstream(t), egressCA(t)
	tp := repoProxy(t, u, ca, nil, nil)
	if anonymous, _ := tp.exchange(t, get("/someone/library/archive/v1.tar.gz", false), ca); !strings.HasPrefix(anonymous, "HTTP/1.1 200 OK") {
		t.Errorf("anonymous: %q", anonymous)
	}
	if signedIn, _ := tp.exchange(t, get("/someone/library/archive/v1.tar.gz", true), ca); !strings.HasPrefix(signedIn, "HTTP/1.1 403 Forbidden") {
		t.Errorf("signed in: %q", signedIn)
	}
}

func TestGraphQLIsHeldToTheRepositoriesItNamesAndItsNodeIDsResolveTo(t *testing.T) {
	u, ca := newUpstream(t), egressCA(t)
	tp := repoProxy(t, u, ca, []string{"api.example.com/owner/app"}, map[string]string{"R_app": "owner/app", "R_other": "owner/other", "U_me": ""})
	create := graphQL(t, "mutation PullRequestCreate($input: CreatePullRequestInput!) { createPullRequest(input: $input) { pullRequest { url } } }",
		map[string]any{"input": map[string]any{"repositoryId": "R_app", "assigneeIds": []any{"U_me"}, "baseRefName": "main", "headRefName": "x", "title": "t"}})
	if ok, err := tp.exchange(t, create, ca); err != nil || !strings.HasPrefix(ok, "HTTP/1.1 200 OK") {
		t.Fatalf("%q, %v", ok, err)
	}
	u.mu.Lock()
	head, body := u.heads[0], u.bodies[0]
	u.mu.Unlock()
	_, sentBody, _ := strings.Cut(create, "\r\n\r\n")
	if !strings.Contains(head, "Authorization: bearer tok-REAL") || body != sentBody {
		t.Errorf("the body goes on as it came: %q %q", head, body)
	}
	for _, c := range []struct{ request, want string }{
		{graphQL(t, "mutation($input: CreatePullRequestInput!) { createPullRequest(input: $input) { pullRequest { url } } }",
			map[string]any{"input": map[string]any{"repositoryId": "R_other"}}), "api.example.com/owner/other"},
		{graphQL(t, "query($o: String!, $n: String!) { repository(owner: $o, name: $n) { id } }", map[string]any{"o": "owner", "n": "other"}), "not allowed"},
		{graphQL(t, `query { node(id: "X_9") { id } }`, nil), "node X_9"},
		{graphQL(t, `query { search(query: "secret", type: REPOSITORY, first: 1) { repositoryCount } }`, nil), "search"},
		{graphQL(t, "mutation($i: CreateRepositoryInput!) { createRepository(input: $i) { repository { url } } }",
			map[string]any{"i": map[string]any{"ownerId": "U_me", "name": "x"}}), "names no allowed repository"},
		{"GET /api/graphql HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: bearer glov-secret:gh\r\n\r\n", "not a POST"},
		{"POST /api/graphql HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: bearer glov-secret:gh\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", "without a Content-Length"},
		{"POST /api/graphql HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: bearer glov-secret:gh\r\nContent-Length: 300000\r\n\r\n", "over 256 KiB"},
		{"POST /api/graphql HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: bearer glov-secret:gh\r\nContent-Length: 99999999999999999999999\r\n\r\n", "over 256 KiB"},
		{"POST /api/graphql HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: bearer glov-secret:gh\r\nContent-Length: +2\r\n\r\n{}", "without one Content-Length"},
		{"POST /api/graphql HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: bearer glov-secret:gh\r\nContent-Length: 2\r\nContent-Length: 40\r\n\r\n{}", "without one Content-Length"},
	} {
		if got, _ := tp.exchange(t, c.request, ca); !strings.HasPrefix(got, "HTTP/1.1 403 Forbidden") || !strings.Contains(got, c.want) {
			t.Errorf("%q: %q", c.request, got)
		}
	}
	if viewer, _ := tp.exchange(t, graphQL(t, "query { viewer { login } }", nil), ca); !strings.HasPrefix(viewer, "HTTP/1.1 200 OK") {
		t.Errorf("viewer: %q", viewer)
	}
	if len(u.seen()) != 2 {
		t.Errorf("upstream saw %d requests", len(u.seen()))
	}
}
