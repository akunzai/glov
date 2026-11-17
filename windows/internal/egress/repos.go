package egress

import (
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"
)

// Forge is the code forge a git secret signs in to (glov secret source
// --forge), so the proxy can tell which repository a request to its hosts
// names and hold it to the VM's repositories (glov secret repos).
type Forge string

const (
	GitHub Forge = "github"
	GitLab Forge = "gitlab"
)

// Forges are the forges a git secret may name.
var Forges = []Forge{GitHub, GitLab}

// RepoPattern is one entry of a VM's repositories: <host>/<owner>/<repo>,
// or <host>/<group>/* for every repository under a group (GitLab subgroups
// included). Hosts and paths compare without case; a trailing .git is
// dropped.
type RepoPattern struct {
	Host string
	// Path is owner/repo or group/sub/repo, or group/*.
	Path string
}

const repoSegmentChars = "abcdefghijklmnopqrstuvwxyz0123456789._-"

// ParseRepoPattern reads text as a RepoPattern, false when it names no
// repository or group.
func ParseRepoPattern(text string) (RepoPattern, bool) {
	t := strings.ToLower(strings.Trim(text, " \t"))
	for _, scheme := range []string{"https://", "http://"} {
		t = strings.TrimPrefix(t, scheme)
	}
	t = strings.TrimRight(t, "/")
	t = strings.TrimSuffix(t, ".git")
	parts := strings.Split(t, "/")
	if len(parts) < 3 {
		return RepoPattern{}, false
	}
	host := parts[0]
	if c, ok := Canonical(host); !ok || c != host || strings.HasPrefix(host, "*") {
		return RepoPattern{}, false
	}
	parts = parts[1:]
	for i, p := range parts {
		wildcard := p == "*" && i == len(parts)-1 && i > 0
		if wildcard {
			continue
		}
		if p == "" || p == "." || p == ".." || strings.Trim(p, repoSegmentChars) != "" {
			return RepoPattern{}, false
		}
	}
	return RepoPattern{Host: host, Path: strings.Join(parts, "/")}, true
}

// String is the pattern as vm.json stores it.
func (p RepoPattern) String() string { return p.Host + "/" + p.Path }

// Matches reports whether repo (owner/repo) on host is this entry or, for
// group/*, under it.
func (p RepoPattern) Matches(host, repo string) bool {
	if strings.ToLower(host) != p.Host {
		return false
	}
	repo = strings.ToLower(repo)
	if strings.HasSuffix(p.Path, "/*") {
		return strings.HasPrefix(repo, strings.TrimSuffix(p.Path, "*"))
	}
	return repo == p.Path
}

// RepoTargetKind is what a request to a forge's host reaches.
type RepoTargetKind int

const (
	// TargetRepo is one repository, named by RepoTarget.Repo.
	TargetRepo RepoTargetKind = iota
	// TargetNone is nothing tied to one repository (/user, the API root).
	TargetNone
	// TargetGraphQL is a GraphQL request: the repositories are in its body.
	TargetGraphQL
	// TargetUnknown is a request whose repository cannot be told from it,
	// refused for RepoTarget.Reason.
	TargetUnknown
)

// RepoTarget is what one request to a forge's host reaches, as far as its
// method and path tell (RepoTargetOf).
type RepoTarget struct {
	Kind RepoTargetKind
	// Repo is the repository's path under the forge (owner/repo,
	// group/sub/repo), lower case.
	Repo string
	// Reason is why a TargetUnknown is refused.
	Reason string
}

func repoTarget(repo string) RepoTarget       { return RepoTarget{Kind: TargetRepo, Repo: repo} }
func unknownTarget(why string) RepoTarget     { return RepoTarget{Kind: TargetUnknown, Reason: why} }
func (t RepoTarget) is(k RepoTargetKind) bool { return t.Kind == k }

// RepoHost is the host repositories are named under for a request to host:
// GitHub serves its API and uploads from subdomains of github.com.
func RepoHost(forge Forge, host string) string {
	host = strings.ToLower(host)
	if forge == GitHub && (host == "api.github.com" || host == "uploads.github.com") {
		return "github.com"
	}
	return host
}

// RepoTargetOf is the repository a request with path (no query;
// percent-encoding as sent) reaches.
func RepoTargetOf(forge Forge, host, path string) RepoTarget {
	host = strings.ToLower(host)
	switch forge {
	case GitHub:
		if host == "api.github.com" {
			return githubAPI(path)
		}
		if host == "uploads.github.com" {
			if t := githubAPI(path); !t.is(TargetGraphQL) {
				return t
			}
			return unknownTarget("GraphQL is served by api.github.com")
		}
		if path == "/api/graphql" {
			return RepoTarget{Kind: TargetGraphQL}
		}
		if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
			return githubAPI(strings.TrimPrefix(path, "/api/v3"))
		}
		return gitPath(path, false)
	case GitLab:
		if path == "/api/graphql" {
			return unknownTarget("GitLab GraphQL names projects in ways the proxy does not check; use the REST API")
		}
		if path == "/api/v4" || strings.HasPrefix(path, "/api/v4/") {
			return gitlabAPI(strings.TrimPrefix(path, "/api/v4"))
		}
		return gitPath(path, true)
	}
	return unknownTarget("forge " + string(forge) + " is not one the proxy knows")
}

// RepoRefusal is why a request is refused, or "" when the VM's
// repositories allow it. A request naming no repository may only read.
func RepoRefusal(t RepoTarget, method, host string, allowed []RepoPattern) string {
	switch t.Kind {
	case TargetRepo:
		if slices.ContainsFunc(allowed, func(p RepoPattern) bool { return p.Matches(host, t.Repo) }) {
			return ""
		}
		return "repository " + host + "/" + t.Repo + " is not allowed"
	case TargetNone:
		m := strings.ToUpper(method)
		if m == "GET" || m == "HEAD" || m == "OPTIONS" {
			return ""
		}
		return "a " + m + " that names no allowed repository"
	case TargetGraphQL:
		return ""
	}
	return t.Reason
}

// unescape is s percent-decoded, false for a bad escape or a result that is
// not UTF-8.
func unescape(s string) (string, bool) {
	d, err := url.PathUnescape(s)
	if err != nil || !utf8.ValidString(d) {
		return "", false
	}
	return d, true
}

// rawSegments is path split on /, a leading and a trailing empty segment
// dropped.
func rawSegments(path string) []string {
	raw := strings.Split(path, "/")
	if len(raw) > 0 && raw[0] == "" {
		raw = raw[1:]
	}
	if len(raw) > 0 && raw[len(raw)-1] == "" {
		raw = raw[:len(raw)-1]
	}
	return raw
}

// segments is the decoded segments of path, or false when one could name a
// different path once decoded or normalized upstream: . or .., an empty
// segment inside the path, a decoded / or \.
func segments(path string) ([]string, bool) {
	out := []string{}
	for _, r := range rawSegments(path) {
		d, ok := unescape(r)
		if !ok || d == "" || d == "." || d == ".." || strings.ContainsAny(d, `/\`) {
			return nil, false
		}
		out = append(out, d)
	}
	return out, true
}

var oddPath = unknownTarget("a path with an empty, dot or encoded-slash segment")

func lowerAll(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = strings.ToLower(v)
	}
	return out
}

func containsPath(set [][]string, s []string) bool {
	return slices.ContainsFunc(set, func(p []string) bool { return slices.Equal(p, s) })
}

// githubFree are GitHub API reads of no repository a signed-in request may
// make: the token's own user and the API's metadata. Everything else
// (/user/repos, /issues, /notifications, an organization's lists) spans
// repositories.
var githubFree = [][]string{{}, {"user"}, {"user", "emails"}, {"rate_limit"}, {"meta"}, {"octocat"}, {"zen"}, {"versions"}}

func githubAPI(path string) RepoTarget {
	s, ok := segments(path)
	if !ok {
		return oddPath
	}
	lower := lowerAll(s)
	if containsPath(githubFree, lower) || len(lower) == 2 && (lower[0] == "users" || lower[0] == "orgs") {
		return RepoTarget{Kind: TargetNone}
	}
	switch {
	case lower[0] == "graphql" && len(s) == 1:
		return RepoTarget{Kind: TargetGraphQL}
	case lower[0] == "repos":
		if len(s) < 3 {
			return unknownTarget("a repository request without owner and name")
		}
		return repoTarget(lower[1] + "/" + lower[2])
	case lower[0] == "repositories":
		return unknownTarget("a repository named by its number")
	case lower[0] == "search":
		return unknownTarget("search reaches every repository the token can read")
	}
	return unknownTarget("an API path of no one repository: /" + strings.Join(s, "/"))
}

// gitlabFree are GitLab API reads of no project a signed-in request may
// make.
var gitlabFree = [][]string{{}, {"user"}, {"version"}, {"metadata"}, {"personal_access_tokens", "self"}}

func gitlabAPI(path string) RepoTarget {
	// A project's path is one segment with its slashes encoded
	// (group%2Fproject), so split before decoding.
	raw := strings.Split(path, "/")
	if len(raw) > 0 && raw[0] == "" {
		raw = raw[1:]
	}
	if len(raw) >= 2 && strings.ToLower(raw[0]) == "projects" {
		id, ok := unescape(raw[1])
		var parts []string
		if ok {
			parts, ok = segments(id)
		}
		if ok && len(parts) >= 2 {
			_, ok = segments("/" + strings.Join(raw[2:], "/"))
		} else {
			ok = false
		}
		if !ok {
			if strings.Trim(raw[1], "0123456789") == "" {
				return unknownTarget("a project named by its number; name it by its path (group%2Fproject)")
			}
			return oddPath
		}
		return repoTarget(strings.ToLower(strings.Join(parts, "/")))
	}
	s, ok := segments(path)
	if !ok {
		return oddPath
	}
	lower := lowerAll(s)
	if containsPath(gitlabFree, lower) || len(lower) == 2 && lower[0] == "users" {
		return RepoTarget{Kind: TargetNone}
	}
	if slices.Contains(lower, "search") {
		return unknownTarget("search reaches every project the token can read")
	}
	return unknownTarget("an API path of no one project: /" + strings.Join(s, "/"))
}

var gitEndpoints = [][]string{{"info", "refs"}, {"git-upload-pack"}, {"git-receive-pack"}, {"info", "lfs"}}

// gitPath is the repository of a git smart HTTP or web path: owner/repo on
// GitHub; on GitLab the path up to /-/, .git or a git service endpoint,
// subgroups included. One segment names no repository.
func gitPath(path string, nested bool) RepoTarget {
	s, ok := segments(path)
	if !ok {
		return oddPath
	}
	if len(s) < 2 {
		return RepoTarget{Kind: TargetNone}
	}
	if !nested {
		return repoTarget(strings.ToLower(trimGit(s[0] + "/" + s[1])))
	}
	if dash := slices.Index(s, "-"); dash >= 0 {
		s = s[:dash]
	}
	if git := slices.IndexFunc(s, func(v string) bool { return strings.HasSuffix(strings.ToLower(v), ".git") }); git >= 0 {
		s = s[:git+1]
	}
	for _, e := range gitEndpoints {
		// The first place e occurs; only one past the first segment ends
		// the project's path.
		at := -1
		for i := 0; i+len(e) <= len(s); i++ {
			if slices.Equal(lowerAll(s[i:i+len(e)]), e) {
				at = i
				break
			}
		}
		if at > 0 {
			s = s[:at]
			break
		}
	}
	if len(s) < 2 {
		return unknownTarget("a path that names no project")
	}
	return repoTarget(strings.ToLower(trimGit(strings.Join(s, "/"))))
}

func trimGit(path string) string {
	if strings.HasSuffix(strings.ToLower(path), ".git") {
		return path[:len(path)-4]
	}
	return path
}
