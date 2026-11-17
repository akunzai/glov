package egress

import (
	"context"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

// forgeCheck is a git host: the forge, the granted git secret (whose value
// resolves GraphQL node IDs) and the VM's repositories.
type forgeCheck struct {
	forge  Forge
	secret string
	repos  []RepoPattern
}

// forgeOf is the git secret granted to the VM that host:port belongs to,
// if any: its signed-in requests are held to the VM's repositories.
func forgeOf(host string, port int, settings Settings, defined []Secret) (forgeCheck, bool) {
	if _, isIP := parseAddr(host); isIP {
		return forgeCheck{}, false
	}
	for _, d := range defined {
		if d.Forge != "" && slices.Contains(settings.Secrets, d.Name) && NewPolicy(d.Hosts).Allows(host, port) {
			var repos []RepoPattern
			for _, text := range settings.Repos {
				if p, ok := ParseRepoPattern(text); ok {
					repos = append(repos, p)
				}
			}
			return forgeCheck{d.Forge, d.Name, repos}, true
		}
	}
	return forgeCheck{}, false
}

// maxGraphQL bounds a GraphQL body the proxy reads whole.
const maxGraphQL = 256 << 10

// checkRepos refuses a signed-in request to a forge's host that reaches a
// repository outside the VM's. A GraphQL request's body is read whole and
// returned with what follows it in rest, to go upstream as it came.
func (p *Proxy) checkRepos(guest net.Conn, req request, plan Plan, check forgeCheck, rest []byte) (body, after []byte, _ *Refusal) {
	refuse := func(reason string) ([]byte, []byte, *Refusal) {
		return nil, nil, &Refusal{Status: 403, Reason: reason,
			Message: reason + "; allow a repository with glov secret repos " + p.VM + " add <host>/<owner>/<repo>"}
	}
	host := RepoHost(check.forge, req.host)
	target := RepoTargetOf(check.forge, req.host, plan.Path)
	if !target.is(TargetGraphQL) {
		if why := RepoRefusal(target, plan.Method, host, check.repos); why != "" {
			return refuse(why)
		}
		return nil, rest, nil
	}
	if plan.Method != "POST" {
		return refuse("a GraphQL request that is not a POST")
	}
	header := func(name string) (string, bool) {
		i := slices.IndexFunc(plan.Headers, func(h Header) bool { return strings.EqualFold(h.Name, name) })
		if i < 0 {
			return "", false
		}
		return plan.Headers[i].Value, true
	}
	if _, chunked := header("transfer-encoding"); chunked {
		return refuse("a GraphQL request without a Content-Length")
	}
	// One length of digits only, so the proxy reads the body the upstream
	// reads.
	length := 0
	if n := slices.IndexFunc(plan.Headers, func(h Header) bool { return strings.EqualFold(h.Name, "content-length") }); n >= 0 {
		v := plan.Headers[n].Value
		if v == "" || strings.Trim(v, "0123456789") != "" || slices.ContainsFunc(plan.Headers[n+1:], func(h Header) bool { return strings.EqualFold(h.Name, "content-length") }) {
			return refuse("a GraphQL request without one Content-Length")
		}
		var err error
		if length, err = strconv.Atoi(v); err != nil {
			length = maxGraphQL + 1
		}
	}
	if length > maxGraphQL {
		return refuse("a GraphQL request over " + strconv.Itoa(maxGraphQL>>10) + " KiB")
	}
	if len(rest) < length {
		more := make([]byte, length-len(rest))
		guest.SetReadDeadline(time.Now().Add(headTimeout))
		_, err := io.ReadFull(guest, more)
		guest.SetReadDeadline(time.Time{})
		if err != nil {
			return nil, nil, &Refusal{Status: 408, Message: "no request body within " + headTimeout.String(), Reason: "request timeout"}
		}
		rest = append(rest, more...)
	}
	body, rest = rest[:length:length], rest[length:]
	request, why := ParseGraphQL(body)
	if why != "" {
		return refuse(why)
	}
	nodes := map[string]string{}
	if len(request.NodeIDs) > 0 {
		token, err := p.Value(check.secret)
		if err == nil {
			nodes, err = p.resolveNodes(req.host, token, request.NodeIDs)
		}
		if err != nil {
			return refuse("cannot tell the repositories of the request's node IDs: " + err.Error())
		}
	}
	allowed := func(repo string) bool {
		return slices.ContainsFunc(check.repos, func(r RepoPattern) bool { return r.Matches(host, repo) })
	}
	if why := GraphQLRefusal(request, host, nodes, allowed); why != "" {
		return refuse(why)
	}
	return body, rest, nil
}

func (p *Proxy) resolveNodes(host, token string, ids []string) (map[string]string, error) {
	ctx := context.Background()
	if p.Nodes != nil {
		return p.Nodes(ctx, host, token, ids)
	}
	return githubNodes.Resolve(ctx, host, token, ids)
}
