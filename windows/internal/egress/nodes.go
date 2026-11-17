package egress

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitHubNodes resolves the GitHub node IDs a guest's GraphQL request
// passes to the repository each belongs to, by asking the forge itself with
// the VM's own git secret, so the proxy can hold a mutation by ID to the
// VM's repositories. Answers are kept for the daemon's life: a node never
// moves to another repository... except a transferred repository, whose
// new owner then has to be allowed too, which errs toward refusing.
type GitHubNodes struct {
	// Client sends the lookups; nil is one with the system's roots.
	Client *http.Client

	mu    sync.Mutex
	cache map[string]string
}

// unownedNodes are node types that belong to no repository and may be
// named freely: an assignee, a reviewer, an owner.
var unownedNodes = map[string]bool{"User": true, "Organization": true, "Bot": true, "Team": true, "Mannequin": true}

const nodesQuery = "query($ids: [ID!]!) { nodes(ids: $ids) { __typename " +
	"... on Repository { nameWithOwner } " +
	"... on RepositoryNode { repository { nameWithOwner } } " +
	"... on Ref { repository { nameWithOwner } } " +
	"... on Commit { repository { nameWithOwner } } " +
	"... on Label { repository { nameWithOwner } } " +
	"... on Milestone { repository { nameWithOwner } } } }"

// nodesEndpoint is the GraphQL endpoint on host, the host the request came
// to, so the token goes nowhere it is not bound to.
func nodesEndpoint(host string) string {
	host = strings.ToLower(host)
	if host == "api.github.com" {
		return "https://api.github.com/graphql"
	}
	return "https://" + host + "/api/graphql"
}

// Resolve is each ID's repository (owner/name, lower case), "" for a node
// of an unowned type; it fails when one is neither.
func (g *GitHubNodes) Resolve(ctx context.Context, host, token string, ids []string) (map[string]string, error) {
	key := func(id string) string { return strings.ToLower(host) + " " + id }
	answer := map[string]string{}
	var missing []string
	g.mu.Lock()
	for _, id := range ids {
		if repo, ok := g.cache[key(id)]; ok {
			answer[id] = repo
		} else {
			missing = append(missing, id)
		}
	}
	g.mu.Unlock()
	if len(missing) == 0 {
		return answer, nil
	}
	body, err := json.Marshal(map[string]any{"query": nodesQuery, "variables": map[string]any{"ids": missing}})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", nodesEndpoint(host), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := g.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("looking up node IDs: " + strings.ReplaceAll(err.Error(), token, "<token>"))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("looking up node IDs answered HTTP " + strconv.Itoa(resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, errors.New("looking up node IDs: " + err.Error())
	}
	found, err := parseNodes(data, missing)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.cache == nil {
		g.cache = map[string]string{}
	}
	for id, repo := range found {
		g.cache[key(id)] = repo
		answer[id] = repo
	}
	g.mu.Unlock()
	return answer, nil
}

// parseNodes is the nodes answer to nodesQuery, in the order of ids.
func parseNodes(data []byte, ids []string) (map[string]string, error) {
	var answer struct {
		Data struct {
			Nodes []map[string]any `json:"nodes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &answer); err != nil || len(answer.Data.Nodes) != len(ids) {
		return nil, errors.New("looking up node IDs gave no answer")
	}
	out := map[string]string{}
	for i, id := range ids {
		node := answer.Data.Nodes[i]
		typ, _ := node["__typename"].(string)
		if node == nil || typ == "" {
			return nil, errors.New("node " + id + " does not exist")
		}
		name, ok := node["nameWithOwner"].(string)
		if !ok {
			if r, isMap := node["repository"].(map[string]any); isMap {
				name, ok = r["nameWithOwner"].(string)
			}
		}
		switch {
		case ok && name != "":
			out[id] = strings.ToLower(name)
		case unownedNodes[typ]:
			out[id] = ""
		default:
			return nil, errors.New("node " + id + " is a " + typ + ", which the proxy cannot tie to a repository")
		}
	}
	return out, nil
}
