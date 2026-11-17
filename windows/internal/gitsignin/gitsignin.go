// Package gitsignin renders what an isolated VM's guest holds so git, gh
// and glab sign in with the VM's git secrets (glov secret source --forge)
// without a token entering it: each tool gets the placeholder
// glov-secret:<name> where the token goes, and the egress proxy puts the
// value in on the way to the secret's hosts (docs/networking.md
// "Repositories"). glovd writes them through the Guest Service.
package gitsignin

import (
	"slices"
	"strings"

	"github.com/akunzai/glov/windows/internal/egress"
)

// Entry is one git secret granted to the VM.
type Entry struct {
	Name  string
	Forge egress.Forge
	// Hosts are the secret's hosts (glov secret source --host).
	Hosts []string
}

// GitHosts are the hosts git and the CLIs name repositories under: GitHub's
// API hosts fold into github.com; wildcards, IP literals and ports other
// than 443 name no git host.
func (e Entry) GitHosts() []string {
	var out []string
	for _, h := range e.Hosts {
		host := strings.ToLower(h)
		if i := strings.LastIndexByte(host, ':'); i >= 0 {
			if host[i+1:] != "443" {
				continue
			}
			host = host[:i]
		}
		if strings.Contains(host, "*") || strings.Trim(host, "0123456789.") == "" || !strings.Contains(host, ".") {
			continue
		}
		if git := egress.RepoHost(e.Forge, host); !slices.Contains(out, git) {
			out = append(out, git)
		}
	}
	return out
}

// Paths in the guest. git asks git-credential-glov (credential.helper =
// glov) for every HTTPS host; it answers for the hosts in SignInsPath only.
const (
	HelperPath    = "/usr/local/bin/git-credential-glov"
	SignInsPath   = "/etc/glov/git-sign-ins"
	GitConfigPath = "/etc/gitconfig"
	// GHHostsPath and GlabConfigPath are relative to the guest user's home.
	GHHostsPath    = ".config/gh/hosts.yml"
	GlabConfigPath = ".config/glab-cli/config.yml"
)

// Helper is git-credential-glov.
const Helper = `#!/bin/sh
# Written by glov. Answers git with the placeholder of the VM's git secret for the host; the egress proxy on the
# host puts the token in, so it never enters the VM.
[ "$1" = get ] || exit 0
host= protocol=
while IFS= read -r line && [ -n "$line" ]; do
  case $line in host=*) host=${line#host=} ;; protocol=*) protocol=${line#protocol=} ;; esac
done
[ "$protocol" = https ] || exit 0
[ -r ` + SignInsPath + ` ] || exit 0
while read -r h forge name; do
  [ "$h" = "$host" ] || continue
  case $forge in gitlab) user=oauth2 ;; *) user=x-access-token ;; esac
  printf 'username=%s\npassword=glov-secret:%s\n' "$user" "$name"
  exit 0
done < ` + SignInsPath + `
`

func sorted(entries []Entry) []Entry {
	return slices.SortedFunc(slices.Values(entries), func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
}

// SignIns is SignInsPath: one "<host> <forge> <secret>" line per git host,
// the first secret for a host winning.
func SignIns(entries []Entry) string {
	var seen []string
	var b strings.Builder
	for _, e := range sorted(entries) {
		for _, h := range e.GitHosts() {
			if !slices.Contains(seen, h) {
				seen = append(seen, h)
				b.WriteString(h + " " + string(e.Forge) + " " + e.Name + "\n")
			}
		}
	}
	return b.String()
}

const (
	begin = "# glov git sign-in: begin (written by glov; edits between these lines are replaced)"
	end   = "# glov git sign-in: end"
)

// GitConfig is /etc/gitconfig (text) with glov's block: the credential
// helper, and SSH remotes of the git hosts rewritten to HTTPS (no SSH key
// reaches the VM, and the proxy can only read HTTPS). Without entries the
// block is removed; other lines stay as they are.
func GitConfig(text string, entries []Entry) string {
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if b := slices.Index(lines, begin); b >= 0 {
		if e := slices.Index(lines[b:], end); e >= 0 {
			lines = slices.Delete(lines, b, b+e+1)
		}
	}
	var hosts []string
	for _, e := range sorted(entries) {
		for _, h := range e.GitHosts() {
			if !slices.Contains(hosts, h) {
				hosts = append(hosts, h)
			}
		}
	}
	if len(hosts) > 0 {
		lines = append(lines, begin, "[credential]", "\thelper = glov")
		for _, h := range hosts {
			lines = append(lines, `[url "https://`+h+`/"]`, "\tinsteadOf = git@"+h+":", "\tinsteadOf = ssh://git@"+h+"/")
		}
		lines = append(lines, end)
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	return b.String()
}

const header = "# Written by glov: the token is a placeholder the egress proxy on the host fills in.\n"

// pairs are the git hosts of forge's entries and the secret each signs in
// with, the first secret for a host winning.
func pairs(entries []Entry, forge egress.Forge) (hosts, names []string) {
	for _, e := range sorted(entries) {
		if e.Forge != forge {
			continue
		}
		for _, h := range e.GitHosts() {
			if !slices.Contains(hosts, h) {
				hosts, names = append(hosts, h), append(names, e.Name)
			}
		}
	}
	return hosts, names
}

// GHHosts is gh's hosts.yml for the GitHub entries, false without any.
func GHHosts(entries []Entry) (string, bool) {
	hosts, names := pairs(entries, egress.GitHub)
	if len(hosts) == 0 {
		return "", false
	}
	var b strings.Builder
	b.WriteString(header)
	for i, h := range hosts {
		b.WriteString(h + ":\n    git_protocol: https\n    oauth_token: glov-secret:" + names[i] + "\n    user: glov\n    users:\n        glov:\n" +
			"            oauth_token: glov-secret:" + names[i] + "\n")
	}
	return b.String(), true
}

// GlabConfig is glab's config.yml for the GitLab entries, false without
// any; the first host is glab's default. is_oauth2 makes glab send
// Authorization: Bearer, which GitLab takes for an OAuth token (what glab
// auth login stores) and a personal access token alike; without it an
// OAuth token goes as PRIVATE-TOKEN and gets 401. glab also parses
// oauth2_expiry_date for an OAuth entry and fails
// on an empty one, so the entry carries a fixed far-future date in RFC 822
// form (glab 1.53 reads only that; later ones also RFC 3339): the
// real token is swapped and refreshed on the host. The year is 68 because
// time.Parse reads two-digit years 69-99 as 19xx and 00-68 as 20xx.
func GlabConfig(entries []Entry) (string, bool) {
	hosts, names := pairs(entries, egress.GitLab)
	if len(hosts) == 0 {
		return "", false
	}
	var b strings.Builder
	b.WriteString(header + "git_protocol: https\ncheck_update: false\ntelemetry: false\nhost: " + hosts[0] + "\nhosts:\n")
	for i, h := range hosts {
		b.WriteString("    " + h + ":\n        api_protocol: https\n        api_host: " + h + "\n        git_protocol: https\n" +
			"        is_oauth2: \"true\"\n        oauth2_expiry_date: 01 Jan 68 00:00 UTC\n        token: glov-secret:" + names[i] + "\n")
	}
	return b.String(), true
}
