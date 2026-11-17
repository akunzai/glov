package egress

import (
	"encoding/json/v2"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// GraphQLRequest is one GitHub GraphQL request, parsed (ParseGraphQL), with
// the node IDs to resolve before GraphQLRefusal can decide
// (docs/networking.md "Repositories"):
//
//   - A query's top-level fields are repository(owner:, name:), node(id:)
//     and nodes(ids:) (by ID, resolved through the forge), viewer, user and
//     organization, or fields of no repository (rateLimit, meta,
//     introspection).
//   - Inside an allowed repository, fields that lead to other repositories
//     (forks, parent, cross-references) are refused, and fields that lead
//     to a person (author, assignees, owner) switch to the person rules.
//   - Under a person (viewer, a user, an organization, a non-repository
//     node), only fields without a selection set (login, name) and
//     connection plumbing (nodes, edges) are read: a user's repositories,
//     issues or pull requests span repositories the request does not name.
//   - Every top-level mutation field must name an allowed repository by node
//     ID (repositoryId, pullRequestId, ...) or nameWithOwner; its other IDs
//     must be allowed repositories or people. Subscriptions are refused.
type GraphQLRequest struct {
	operations []gqlOperation
	fragments  map[string][]gqlSelection
	// NodeIDs are the node IDs the request names, sorted.
	NodeIDs []string
}

func gqlSet(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

var (
	// gqlFree are top-level query fields that read nothing of a repository.
	gqlFree = gqlSet("__typename", "__schema", "__type", "rateLimit", "meta")
	// gqlPeople are top-level query fields about a person.
	gqlPeople = gqlSet("viewer", "user", "organization")
	// gqlElsewhere are fields inside a repository that lead to another
	// repository.
	gqlElsewhere = gqlSet("forks", "canonical", "duplicate", "closer",
		"repositories", "repository", "search", "network", "dependencyGraphManifests",
		// issues and pull requests that may sit in other repositories
		"subIssues", "trackedIssues", "trackedInIssues", "blockedBy", "blocking", "closingIssuesReferences",
		"closedByPullRequestsReferences", "subject", "fromRepository", "commitRepository", "referencedSubjects")
	// gqlToPeople are fields inside a repository that lead to a person or a
	// project, read by the person rules.
	gqlToPeople = gqlSet("owner", "author", "editor", "actor", "user", "assignees", "participants", "mentionableUsers",
		"assignableUsers", "collaborators", "stargazers", "watchers", "requestedReviewer", "mergedBy",
		"enqueuedBy", "organization", "viewer", "committer", "reactors", "suggestedReviewers", "latestReviews",
		"reviewRequests", "headRepositoryOwner", "lastEditedBy",
		// a project holds items of any repository: only its own fields are read
		"project", "projectV2", "projectsV2", "projects")
	gqlProjects = gqlSet("project", "projectV2", "projectsV2", "projects")
	// gqlPlumbing is connection plumbing a person's leaf fields may sit
	// under.
	gqlPlumbing = gqlSet("nodes", "edges", "node", "pageInfo")
	// gqlPeers are fields inside a repository that lead to another one gh
	// reads the identity of: a fork's upstream, a pull request's head and
	// base. (Issue.parent, a sub-issue's parent, is read the same way: its
	// ID and URL only.)
	gqlPeers = gqlSet("parent", "source", "templateRepository", "headRepository", "baseRepository")
	// gqlPeerFields are fields of another repository's identity that may be
	// opened under gqlPeers.
	gqlPeerFields = gqlSet("owner", "defaultBranchRef", "parent", "source")
	// gqlPeerContent are leaves of an issue or pull request (Issue.parent is
	// read here too) that hold its content.
	gqlPeerContent = gqlSet("title", "body", "bodyText", "bodyHTML", "text")
)

// ParseGraphQL reads a GraphQL request body, or is why it is refused.
func ParseGraphQL(body []byte) (GraphQLRequest, string) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return GraphQLRequest{}, "a GraphQL request that is not one JSON object with a query"
	}
	query, ok := doc["query"].(string)
	if !ok {
		return GraphQLRequest{}, "a GraphQL request that is not one JSON object with a query"
	}
	variables, _ := doc["variables"].(map[string]any)
	tokens, ok := gqlScan(query)
	var d gqlDocument
	if ok {
		p := gqlParser{tokens: tokens}
		d, ok = p.document()
	}
	if !ok {
		return GraphQLRequest{}, "a GraphQL query that does not parse"
	}
	operations := d.operations
	if name, _ := doc["operationName"].(string); name != "" {
		operations = slices.DeleteFunc(operations, func(o gqlOperation) bool { return o.name != name })
		if len(operations) == 0 {
			return GraphQLRequest{}, "no GraphQL operation named " + name
		}
	}
	// A variable the JSON leaves out takes its definition's default, as the
	// server does, else null: an absent argument names no repository, which
	// the checks read as nothing allowed.
	for i := range operations {
		values := maps.Clone(variables)
		if values == nil {
			values = map[string]any{}
		}
		for name, v := range operations[i].defaults {
			if _, ok := values[name]; !ok {
				values[name] = gqlResolve(v, nil)
			}
		}
		operations[i].variables = values
	}
	ids := map[string]bool{}
	for _, op := range operations {
		for _, f := range gqlFields(op.selections, d.fragments, nil) {
			args := gqlArgs(f, variables)
			if op.kind == gqlMutation {
				for k, v := range args {
					gqlCollectIDs(k, v, ids)
				}
			} else if f.name == "node" || f.name == "nodes" {
				for _, id := range gqlStrings(args[gqlIDArg(f.name)]) {
					ids[id] = true
				}
			}
		}
	}
	return GraphQLRequest{operations: operations, fragments: d.fragments, NodeIDs: slices.Sorted(maps.Keys(ids))}, ""
}

func gqlIDArg(field string) string {
	if field == "node" {
		return "id"
	}
	return "ids"
}

// GraphQLRefusal is why r is refused, or "" when it reaches only allowed
// repositories on host. nodes maps each of r.NodeIDs to its repository
// (owner/name, lower case), or to "" for a person; an ID missing from it
// is refused.
func GraphQLRefusal(r GraphQLRequest, host string, nodes map[string]string, allowed func(repo string) bool) string {
	// repoOf is id's repository ("" for a person), or why it is refused.
	repoOf := func(id string) (string, string) {
		repo, ok := nodes[id]
		if !ok {
			return "", "node " + id + " is of no repository the proxy can tell"
		}
		if repo != "" && !allowed(repo) {
			return "", "repository " + host + "/" + repo + " is not allowed"
		}
		return repo, ""
	}
	for _, op := range r.operations {
		for _, f := range gqlFields(op.selections, r.fragments, nil) {
			args := gqlArgs(f, op.variables)
			switch op.kind {
			case gqlSubscription:
				return "GraphQL subscriptions are not checked"
			case gqlMutation:
				ids := map[string]bool{}
				for k, v := range args {
					gqlCollectIDs(k, v, ids)
				}
				named := false
				for _, id := range slices.Sorted(maps.Keys(ids)) {
					repo, why := repoOf(id)
					if why != "" {
						return why
					}
					named = named || repo != ""
				}
				for _, name := range gqlNameWithOwner(args) {
					if !allowed(name) {
						return "repository " + host + "/" + name + " is not allowed"
					}
					named = true
				}
				if !named {
					return "GraphQL mutation " + f.name + " names no allowed repository"
				}
				if why := gqlWalk(f.selections, scopeRepo, r.fragments, 0); why != "" {
					return why
				}
			default:
				switch {
				case gqlFree[f.name]:
				case gqlPeople[f.name]:
					if why := gqlWalk(f.selections, scopePerson, r.fragments, 0); why != "" {
						return why
					}
				case f.name == "repository":
					owner, name := gqlStrings(args["owner"]), gqlStrings(args["name"])
					if len(owner) == 0 || len(name) == 0 {
						return "GraphQL repository(...) without an owner and name the proxy can read"
					}
					repo := strings.ToLower(owner[0] + "/" + name[0])
					if !allowed(repo) {
						return "repository " + host + "/" + repo + " is not allowed"
					}
					if why := gqlWalk(f.selections, scopeRepo, r.fragments, 0); why != "" {
						return why
					}
				case f.name == "node" || f.name == "nodes":
					ids := gqlStrings(args[gqlIDArg(f.name)])
					if len(ids) == 0 {
						return "GraphQL " + f.name + " without IDs the proxy can read"
					}
					people := false
					for _, id := range ids {
						repo, why := repoOf(id)
						if why != "" {
							return why
						}
						people = people || repo == ""
					}
					scope := scopeRepo
					if people {
						scope = scopePerson
					}
					if why := gqlWalk(f.selections, scope, r.fragments, 0); why != "" {
						return why
					}
				default:
					return "GraphQL field " + f.name + " is not one the proxy checks"
				}
			}
		}
	}
	return ""
}

type gqlScope int

const (
	scopeRepo gqlScope = iota
	scopePerson
	// scopePeer is a repository reached from an allowed one (a fork's
	// parent, a pull request's head): its identity only (name, owner {
	// login }, defaultBranchRef { name }), so gh can tell a fork from its
	// upstream.
	scopePeer
)

// gqlWalk is the first refusal under selections read in scope.
func gqlWalk(selections []gqlSelection, scope gqlScope, fragments map[string][]gqlSelection, depth int) string {
	if depth >= 64 {
		return "a GraphQL query nested too deep"
	}
	for _, f := range gqlFields(selections, fragments, nil) {
		next := scope
		switch scope {
		case scopeRepo:
			switch {
			case gqlPeers[f.name]:
				next = scopePeer
			case gqlElsewhere[f.name]:
				return "GraphQL field " + f.name + " leads outside the allowed repository"
			case gqlToPeople[f.name]:
				next = scopePerson
			}
		case scopePeer:
			if !(len(f.selections) == 0 && !gqlPeerContent[f.name] || gqlPlumbing[f.name] || gqlPeerFields[f.name]) {
				return "GraphQL field " + f.name + " reads more of another repository than its name"
			}
			if f.name == "owner" {
				next = scopePerson
			}
		case scopePerson:
			// A person's leaves, plumbing, and other people (a review
			// request's requestedReviewer).
			if !(len(f.selections) == 0 || gqlPlumbing[f.name] || gqlToPeople[f.name] && !gqlProjects[f.name]) {
				return "GraphQL field " + f.name + " under a person reaches repositories the request does not name"
			}
		}
		if why := gqlWalk(f.selections, next, fragments, depth+1); why != "" {
			return why
		}
	}
	return ""
}

// gqlFields is the fields of selections, inline fragments and fragment
// spreads opened. A spread of an unknown fragment, or of one inside itself,
// opens to nothing: the server refuses such a document whole.
func gqlFields(selections []gqlSelection, fragments map[string][]gqlSelection, seen []string) []gqlField {
	var out []gqlField
	for _, s := range selections {
		switch {
		case s.field != nil:
			out = append(out, *s.field)
		case s.spread != "":
			if inner, ok := fragments[s.spread]; ok && !slices.Contains(seen, s.spread) {
				out = append(out, gqlFields(inner, fragments, append(slices.Clone(seen), s.spread))...)
			}
		default:
			out = append(out, gqlFields(s.inline, fragments, seen)...)
		}
	}
	return out
}

func gqlIsID(key string) bool {
	if key == "clientMutationId" {
		return false
	}
	return key == "id" || key == "ids" || strings.HasSuffix(key, "Id") || strings.HasSuffix(key, "ID") ||
		strings.HasSuffix(key, "Ids") || strings.HasSuffix(key, "IDs")
}

// gqlArgs is f's arguments with variables replaced by their JSON values;
// an argument of no value is left out.
func gqlArgs(f gqlField, variables map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range f.arguments {
		if r := gqlResolve(v, variables); r != nil {
			out[k] = r
		}
	}
	return out
}

// gqlResolve is v with variables replaced by their JSON values: a string, a
// []any or map[string]any of them, a JSON value, or nil.
func gqlResolve(v gqlValue, variables map[string]any) any {
	switch v := v.(type) {
	case string:
		return v
	case gqlVariable:
		return variables[string(v)]
	case []gqlValue:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = gqlResolve(item, variables)
		}
		return out
	case map[string]gqlValue:
		out := map[string]any{}
		for k, item := range v {
			out[k] = gqlResolve(item, variables)
		}
		return out
	}
	return nil
}

func gqlStrings(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, item := range v {
			out = append(out, gqlStrings(item)...)
		}
		return out
	}
	return nil
}

func gqlCollectIDs(key string, v any, ids map[string]bool) {
	if gqlIsID(key) {
		for _, s := range gqlStrings(v) {
			ids[s] = true
		}
	}
	switch v := v.(type) {
	case map[string]any:
		for k, inner := range v {
			gqlCollectIDs(k, inner, ids)
		}
	case []any:
		for _, inner := range v {
			gqlCollectIDs(key, inner, ids)
		}
	}
}

// gqlNameWithOwner is the owner/name strings a mutation passes as
// *NameWithOwner or *nameWithOwner, at any depth, lower case.
func gqlNameWithOwner(args map[string]any) []string {
	var out []string
	var visit func(key string, v any)
	visit = func(key string, v any) {
		if strings.HasSuffix(strings.ToLower(key), "namewithowner") {
			for _, s := range gqlStrings(v) {
				out = append(out, strings.ToLower(s))
			}
		}
		switch v := v.(type) {
		case map[string]any:
			for _, k := range slices.Sorted(maps.Keys(v)) {
				visit(k, v[k])
			}
		case []any:
			for _, inner := range v {
				visit(key, inner)
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(args)) {
		visit(k, args[k])
	}
	return out
}

// gqlValue is an argument as written: a string, a gqlVariable, a
// []gqlValue, a map[string]gqlValue, or nil for any other literal.
type gqlValue any

type gqlVariable string

type gqlField struct {
	name       string
	arguments  map[string]gqlValue
	selections []gqlSelection
}

// gqlSelection is a field, a fragment spread, or else an inline fragment.
type gqlSelection struct {
	field  *gqlField
	spread string
	inline []gqlSelection
}

type gqlKind int

const (
	gqlQuery gqlKind = iota
	gqlMutation
	gqlSubscription
)

type gqlOperation struct {
	kind       gqlKind
	name       string
	selections []gqlSelection
	// defaults are the default values of the variable definitions.
	defaults  map[string]gqlValue
	variables map[string]any
}

type gqlDocument struct {
	operations []gqlOperation
	fragments  map[string][]gqlSelection
}

type gqlTokenKind int

const (
	tokName gqlTokenKind = iota
	tokString
	tokNumber
	tokPunct
)

type gqlToken struct {
	kind gqlTokenKind
	text string
}

func (t gqlToken) isPunct(p string) bool { return t.kind == tokPunct && t.text == p }
func (t gqlToken) isName(n string) bool  { return t.kind == tokName && t.text == n }

// gqlScan is GraphQL's lexical tokens of text, without comments and commas;
// false for a query that does not lex.
func gqlScan(text string) ([]gqlToken, bool) {
	var tokens []gqlToken
	chars := []rune(text)
	n := len(chars)
	isQuote3 := func(j int) bool { return j+2 < n && chars[j] == '"' && chars[j+1] == '"' && chars[j+2] == '"' }
	for i := 0; i < n; {
		c := chars[i]
		switch {
		case c == '#':
			for i < n && chars[i] != '\n' && chars[i] != '\r' {
				i++
			}
		case unicode.IsSpace(c) || c == ',' || c == '\uFEFF':
			i++
		case c == '"' && isQuote3(i):
			var s strings.Builder
			j := i + 3
			for j+2 < n && !isQuote3(j) {
				if chars[j] == '\\' && j+3 < n && isQuote3(j+1) {
					s.WriteString(`"""`)
					j += 4
					continue
				}
				s.WriteRune(chars[j])
				j++
			}
			if j+2 >= n {
				return nil, false
			}
			tokens = append(tokens, gqlToken{tokString, strings.TrimSpace(s.String())})
			i = j + 3
		case c == '"':
			var s strings.Builder
			j := i + 1
			for j < n && chars[j] != '"' {
				if chars[j] == '\n' || chars[j] == '\r' {
					return nil, false
				}
				if chars[j] != '\\' {
					s.WriteRune(chars[j])
					j++
					continue
				}
				if j+1 >= n {
					return nil, false
				}
				switch e := chars[j+1]; e {
				case 'n':
					s.WriteByte('\n')
				case 't':
					s.WriteByte('\t')
				case 'r':
					s.WriteByte('\r')
				case 'b':
					s.WriteByte('\b')
				case 'f':
					s.WriteByte('\f')
				case 'u':
					if j+5 >= n {
						return nil, false
					}
					code, err := strconv.ParseUint(string(chars[j+2:j+6]), 16, 32)
					if err != nil || !utf8.ValidRune(rune(code)) {
						return nil, false
					}
					s.WriteRune(rune(code))
					j += 4
				default:
					s.WriteRune(e)
				}
				j += 2
			}
			if j >= n {
				return nil, false
			}
			tokens = append(tokens, gqlToken{tokString, s.String()})
			i = j + 1
		case unicode.IsLetter(c) || c == '_':
			j := i
			for j < n && (unicode.IsLetter(chars[j]) || unicode.IsNumber(chars[j]) || chars[j] == '_') {
				j++
			}
			tokens = append(tokens, gqlToken{tokName, string(chars[i:j])})
			i = j
		case unicode.IsNumber(c) || c == '-':
			j := i + 1
			for j < n && (unicode.IsNumber(chars[j]) || strings.ContainsRune(".eE+-", chars[j])) {
				j++
			}
			tokens = append(tokens, gqlToken{kind: tokNumber})
			i = j
		case c == '.' && i+2 < n && chars[i+1] == '.' && chars[i+2] == '.':
			tokens = append(tokens, gqlToken{tokPunct, "."})
			i += 3
		case strings.ContainsRune("!$&()[]{}:=@|", c):
			tokens = append(tokens, gqlToken{tokPunct, string(c)})
			i++
		default:
			return nil, false
		}
	}
	return tokens, true
}

// gqlParser is a recursive-descent reader of GraphQL's executable
// documents; it fails anywhere they do not follow the grammar, so the proxy
// never decides on a query it read differently from the server.
type gqlParser struct {
	tokens []gqlToken
	i      int
}

func (p *gqlParser) peek() (gqlToken, bool) {
	if p.i < len(p.tokens) {
		return p.tokens[p.i], true
	}
	return gqlToken{}, false
}

func (p *gqlParser) peekPunct(s string) bool { t, ok := p.peek(); return ok && t.isPunct(s) }

func (p *gqlParser) peekName() (string, bool) {
	t, ok := p.peek()
	if !ok || t.kind != tokName {
		return "", false
	}
	return t.text, true
}

func (p *gqlParser) document() (gqlDocument, bool) {
	doc := gqlDocument{fragments: map[string][]gqlSelection{}}
	for p.i < len(p.tokens) {
		if p.peekPunct("{") {
			sel, ok := p.selectionSet()
			if !ok {
				return doc, false
			}
			doc.operations = append(doc.operations, gqlOperation{kind: gqlQuery, selections: sel})
			continue
		}
		keyword, ok := p.peekName()
		if !ok {
			return doc, false
		}
		p.i++
		if keyword == "fragment" {
			name, ok := p.peekName()
			if !ok || name == "on" {
				return doc, false
			}
			p.i++
			if t, ok := p.peek(); !ok || !t.isName("on") {
				return doc, false
			}
			p.i++
			if _, ok := p.peekName(); !ok {
				return doc, false
			}
			p.i++
			if !p.directives() {
				return doc, false
			}
			sel, ok := p.selectionSet()
			if _, dup := doc.fragments[name]; !ok || dup {
				return doc, false
			}
			doc.fragments[name] = sel
			continue
		}
		var kind gqlKind
		switch keyword {
		case "query":
			kind = gqlQuery
		case "mutation":
			kind = gqlMutation
		case "subscription":
			kind = gqlSubscription
		default:
			return doc, false
		}
		op := gqlOperation{kind: kind, defaults: map[string]gqlValue{}}
		if n, ok := p.peekName(); ok {
			op.name = n
			p.i++
		}
		if p.peekPunct("(") {
			if op.defaults, ok = p.variableDefinitions(); !ok {
				return doc, false
			}
		}
		if !p.directives() {
			return doc, false
		}
		if op.selections, ok = p.selectionSet(); !ok {
			return doc, false
		}
		doc.operations = append(doc.operations, op)
	}
	return doc, len(doc.operations) > 0
}

// variableDefinitions reads ($name: Type = default @directive ...): the
// defaults given.
func (p *gqlParser) variableDefinitions() (map[string]gqlValue, bool) {
	p.i++
	defaults := map[string]gqlValue{}
	for {
		t, ok := p.peek()
		if !ok {
			return nil, false
		}
		if t.isPunct(")") {
			p.i++
			return defaults, true
		}
		if !t.isPunct("$") || p.i+2 >= len(p.tokens) || p.tokens[p.i+1].kind != tokName || !p.tokens[p.i+2].isPunct(":") {
			return nil, false
		}
		name := p.tokens[p.i+1].text
		p.i += 3
		if !p.typeRef(0) {
			return nil, false
		}
		if p.peekPunct("=") {
			p.i++
			v, ok := p.value()
			if !ok {
				return nil, false
			}
			defaults[name] = v
		}
		if !p.directives() {
			return nil, false
		}
	}
}

// typeRef reads Name or [Type], each optionally !.
func (p *gqlParser) typeRef(depth int) bool {
	if depth >= 16 {
		return false
	}
	if p.peekPunct("[") {
		p.i++
		if !p.typeRef(depth+1) || !p.peekPunct("]") {
			return false
		}
		p.i++
	} else {
		if _, ok := p.peekName(); !ok {
			return false
		}
		p.i++
	}
	if p.peekPunct("!") {
		p.i++
	}
	return true
}

func (p *gqlParser) selectionSet() ([]gqlSelection, bool) {
	if !p.peekPunct("{") {
		return nil, false
	}
	p.i++
	out := []gqlSelection{}
	for {
		t, ok := p.peek()
		if !ok {
			return nil, false
		}
		if t.isPunct("}") {
			p.i++
			return out, true
		}
		if t.isPunct(".") {
			p.i++
			next, _ := p.peek()
			switch {
			case next.isName("on"):
				p.i++
				if _, ok := p.peekName(); !ok {
					return nil, false
				}
				p.i++
				fallthrough
			case next.isPunct("@") || next.isPunct("{"):
				if !p.directives() {
					return nil, false
				}
				inner, ok := p.selectionSet()
				if !ok {
					return nil, false
				}
				out = append(out, gqlSelection{inline: inner})
			case next.kind == tokName && p.i < len(p.tokens):
				p.i++
				if !p.directives() {
					return nil, false
				}
				out = append(out, gqlSelection{spread: next.text})
			default:
				return nil, false
			}
			continue
		}
		if t.kind != tokName {
			return nil, false
		}
		name := t.text
		p.i++
		if p.peekPunct(":") {
			p.i++
			real, ok := p.peekName()
			if !ok {
				return nil, false
			}
			name = real
			p.i++
		}
		f := gqlField{name: name, arguments: map[string]gqlValue{}}
		if p.peekPunct("(") {
			if f.arguments, ok = p.arguments(); !ok {
				return nil, false
			}
		}
		if !p.directives() {
			return nil, false
		}
		if p.peekPunct("{") {
			if f.selections, ok = p.selectionSet(); !ok {
				return nil, false
			}
		}
		out = append(out, gqlSelection{field: &f})
	}
}

// keyed reads name: value pairs up to the punctuation end.
func (p *gqlParser) keyed(end string) (map[string]gqlValue, bool) {
	out := map[string]gqlValue{}
	for {
		t, ok := p.peek()
		if !ok {
			return nil, false
		}
		if t.isPunct(end) {
			p.i++
			return out, true
		}
		if t.kind != tokName || p.i+1 >= len(p.tokens) || !p.tokens[p.i+1].isPunct(":") {
			return nil, false
		}
		p.i += 2
		v, ok := p.value()
		if !ok {
			return nil, false
		}
		out[t.text] = v
	}
}

func (p *gqlParser) arguments() (map[string]gqlValue, bool) {
	p.i++
	return p.keyed(")")
}

func (p *gqlParser) value() (gqlValue, bool) {
	t, ok := p.peek()
	if !ok {
		return nil, false
	}
	switch {
	case t.kind == tokString:
		p.i++
		return t.text, true
	case t.isPunct("$"):
		if p.i+1 >= len(p.tokens) || p.tokens[p.i+1].kind != tokName {
			return nil, false
		}
		p.i += 2
		return gqlVariable(p.tokens[p.i-1].text), true
	case t.isPunct("["):
		p.i++
		items := []gqlValue{}
		for {
			t, ok := p.peek()
			if !ok {
				return nil, false
			}
			if t.isPunct("]") {
				p.i++
				return items, true
			}
			v, ok := p.value()
			if !ok {
				return nil, false
			}
			items = append(items, v)
		}
	case t.isPunct("{"):
		p.i++
		fields, ok := p.keyed("}")
		if !ok {
			return nil, false
		}
		return fields, true
	case t.kind == tokName || t.kind == tokNumber:
		p.i++
		return nil, true
	}
	return nil, false
}

func (p *gqlParser) directives() bool {
	for p.peekPunct("@") {
		p.i++
		if _, ok := p.peekName(); !ok {
			return false
		}
		p.i++
		if p.peekPunct("(") {
			if _, ok := p.arguments(); !ok {
				return false
			}
		}
	}
	return true
}
