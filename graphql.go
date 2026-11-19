package main

// A small GraphQL executor for exactly the documents the forge's clients
// send: viewer/repository/node/__type reads, issue and pull request
// connections, and the create/close/merge/comment mutations. It is a
// hand-written lexer/parser plus a field-whitelist executor, and it keeps
// the house quirks: unknown fields are per-field errors (HTTP stays 200),
// connection edges are cursor-numbered by position while page cursors use
// issue numbers, and a few types (__Schema, LabelConnection) resolve
// exactly as the reference executor resolves them — no more, no less.

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Lexer
// ---------------------------------------------------------------------------

type gqlToken struct {
	kind  string // name | int | float | string | punct | eof
	value string
}

func gqlLex(source string) ([]gqlToken, error) {
	runes := []rune(source)
	var tokens []gqlToken
	i := 0
	isNameStart := func(ch rune) bool {
		return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch == '_'
	}
	isNameChar := func(ch rune) bool {
		return isNameStart(ch) || ch >= '0' && ch <= '9'
	}
	isDigit := func(ch rune) bool { return ch >= '0' && ch <= '9' }
	for i < len(runes) {
		ch := runes[i]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' || ch == '\uFEFF' {
			i++
			continue
		}
		if ch == '#' {
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '.' && i+2 < len(runes) && runes[i+1] == '.' && runes[i+2] == '.' {
			tokens = append(tokens, gqlToken{"punct", "..."})
			i += 3
			continue
		}
		if strings.ContainsRune("{}():!=$@[]=", ch) {
			tokens = append(tokens, gqlToken{"punct", string(ch)})
			i++
			continue
		}
		if ch == '"' {
			if i+2 < len(runes) && runes[i+1] == '"' && runes[i+2] == '"' {
				rest := string(runes[i+3:])
				end := strings.Index(rest, `"""`)
				if end < 0 {
					tokens = append(tokens, gqlToken{"string", rest})
					i = len(runes)
				} else {
					tokens = append(tokens, gqlToken{"string", rest[:end]})
					i += 3 + len([]rune(rest[:end])) + 3
				}
				continue
			}
			j := i + 1
			var value strings.Builder
			for j < len(runes) && runes[j] != '"' {
				if runes[j] == '\\' && j+1 < len(runes) {
					esc := runes[j+1]
					switch esc {
					case 'n':
						value.WriteByte('\n')
						j += 2
					case 't':
						value.WriteByte('\t')
						j += 2
					case 'r':
						value.WriteByte('\r')
						j += 2
					case 'u':
						if j+5 >= len(runes) {
							return nil, errors.New("invalid unicode escape")
						}
						code, err := strconv.ParseUint(string(runes[j+2:j+6]), 16, 32)
						if err != nil {
							return nil, errors.New("invalid unicode escape")
						}
						value.WriteRune(rune(code))
						j += 6
					default:
						value.WriteRune(esc)
						j += 2
					}
					continue
				}
				value.WriteRune(runes[j])
				j++
			}
			tokens = append(tokens, gqlToken{"string", value.String()})
			i = j + 1
			continue
		}
		if isDigit(ch) || ch == '-' {
			j := i
			isFloat := false
			if runes[j] == '-' {
				j++
			}
			for j < len(runes) && isDigit(runes[j]) {
				j++
			}
			if j < len(runes) && runes[j] == '.' && j+1 < len(runes) && isDigit(runes[j+1]) {
				isFloat = true
				j++
				for j < len(runes) && isDigit(runes[j]) {
					j++
				}
			}
			if j < len(runes) && (runes[j] == 'e' || runes[j] == 'E') {
				isFloat = true
				j++
				if j < len(runes) && (runes[j] == '+' || runes[j] == '-') {
					j++
				}
				for j < len(runes) && isDigit(runes[j]) {
					j++
				}
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			tokens = append(tokens, gqlToken{kind, string(runes[i:j])})
			i = j
			continue
		}
		if isNameStart(ch) {
			j := i
			for j < len(runes) && isNameChar(runes[j]) {
				j++
			}
			tokens = append(tokens, gqlToken{"name", string(runes[i:j])})
			i = j
			continue
		}
		return nil, fmt.Errorf("unexpected character %s", jsonString(string(ch)))
	}
	tokens = append(tokens, gqlToken{"eof", ""})
	return tokens, nil
}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

type gqlVar struct{ name string }

type gqlSelection struct {
	alias      string
	hasAlias   bool
	name       string
	args       map[string]any
	selections []*gqlSelection
	fragment   string // fragment-spread name, "" otherwise
	inlineType string // inline-fragment type condition, "" otherwise
}

type gqlOperation struct {
	kind       string // query | mutation
	selections []*gqlSelection
}

type gqlFragment struct {
	typ        string
	selections []*gqlSelection
}

type gqlDocument struct {
	operations []*gqlOperation
	fragments  map[string]*gqlFragment
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

func (p *gqlParser) peek() gqlToken {
	if p.pos >= len(p.tokens) {
		return gqlToken{"eof", ""}
	}
	return p.tokens[p.pos]
}

func (p *gqlParser) next() gqlToken {
	token := p.peek()
	p.pos++
	return token
}

func (p *gqlParser) expectPunct(value string) error {
	token := p.next()
	if token.kind != "punct" || token.value != value {
		return fmt.Errorf("expected %s", value)
	}
	return nil
}

func (p *gqlParser) expectName() (string, error) {
	token := p.next()
	if token.kind != "name" {
		return "", errors.New("expected a name")
	}
	return token.value, nil
}

func (p *gqlParser) parseDocument() (*gqlDocument, error) {
	doc := &gqlDocument{fragments: make(map[string]*gqlFragment)}
	for p.peek().kind != "eof" {
		token := p.peek()
		if token.kind == "name" && token.value == "fragment" {
			p.next()
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if _, err := p.expectName(); err != nil { // 'on'
				return nil, err
			}
			typ, err := p.expectName()
			if err != nil {
				return nil, err
			}
			selections, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			doc.fragments[name] = &gqlFragment{typ: typ, selections: selections}
			continue
		}
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		doc.operations = append(doc.operations, op)
	}
	return doc, nil
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	kind := "query"
	token := p.peek()
	if token.kind == "name" && (token.value == "query" || token.value == "mutation") {
		kind = p.next().value
	}
	if p.peek().kind == "name" {
		p.next() // operation name, not otherwise used
	}
	if p.peek().kind == "punct" && p.peek().value == "(" {
		if err := p.skipParens(); err != nil {
			return nil, err
		}
	}
	selections, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &gqlOperation{kind: kind, selections: selections}, nil
}

func (p *gqlParser) skipParens() error {
	if err := p.expectPunct("("); err != nil {
		return err
	}
	depth := 1
	for depth > 0 {
		token := p.next()
		if token.kind == "eof" {
			return errors.New("unbalanced parens")
		}
		if token.kind == "punct" && token.value == "(" {
			depth++
		}
		if token.kind == "punct" && token.value == ")" {
			depth--
		}
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var selections []*gqlSelection
	for !(p.peek().kind == "punct" && p.peek().value == "}") {
		if p.peek().kind == "eof" {
			return nil, errors.New("unbalanced selection set")
		}
		selection, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		selections = append(selections, selection)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return selections, nil
}

func (p *gqlParser) parseSelection() (*gqlSelection, error) {
	if p.peek().kind == "punct" && p.peek().value == "..." {
		p.next()
		token := p.peek()
		if token.kind == "name" && token.value == "on" {
			p.next()
			typ, err := p.expectName()
			if err != nil {
				return nil, err
			}
			selections, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &gqlSelection{name: "", args: map[string]any{}, selections: selections, inlineType: typ}, nil
		}
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		return &gqlSelection{name: "", args: map[string]any{}, fragment: name}, nil
	}
	name, err := p.expectName()
	if err != nil {
		return nil, err
	}
	selection := &gqlSelection{name: name, args: map[string]any{}}
	if p.peek().kind == "punct" && p.peek().value == ":" {
		p.next()
		selection.alias, selection.hasAlias = name, true
		realName, err := p.expectName()
		if err != nil {
			return nil, err
		}
		selection.name = realName
	}
	if p.peek().kind == "punct" && p.peek().value == "(" {
		args, err := p.parseArguments()
		if err != nil {
			return nil, err
		}
		selection.args = args
	}
	// Directives (@include/@skip and friends): parsed and ignored.
	for p.peek().kind == "punct" && p.peek().value == "@" {
		p.next()
		if _, err := p.expectName(); err != nil {
			return nil, err
		}
		if p.peek().kind == "punct" && p.peek().value == "(" {
			if err := p.skipParens(); err != nil {
				return nil, err
			}
		}
	}
	if p.peek().kind == "punct" && p.peek().value == "{" {
		selections, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		selection.selections = selections
	}
	return selection, nil
}

func (p *gqlParser) parseArguments() (map[string]any, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	args := map[string]any{}
	for !(p.peek().kind == "punct" && p.peek().value == ")") {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[name] = value
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *gqlParser) parseValue() (any, error) {
	token := p.next()
	if token.kind == "punct" && token.value == "$" {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		return gqlVar{name}, nil
	}
	if token.kind == "punct" && token.value == "[" {
		var items []any
		for !(p.peek().kind == "punct" && p.peek().value == "]") {
			item, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		if err := p.expectPunct("]"); err != nil {
			return nil, err
		}
		return items, nil
	}
	if token.kind == "punct" && token.value == "{" {
		object := map[string]any{}
		for !(p.peek().kind == "punct" && p.peek().value == "}") {
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			value, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		if err := p.expectPunct("}"); err != nil {
			return nil, err
		}
		return object, nil
	}
	switch token.kind {
	case "string":
		return token.value, nil
	case "int":
		value, _ := parseIntJS(token.value)
		return float64(value), nil
	case "float":
		value, _ := strconv.ParseFloat(token.value, 64)
		return value, nil
	case "name":
		switch token.value {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		}
		return token.value, nil // enum value
	}
	return nil, errors.New("unexpected value")
}

// resolveGqlVariables substitutes {$var} markers from the request variables.
func resolveGqlVariables(value any, variables map[string]any) any {
	switch t := value.(type) {
	case gqlVar:
		if v, ok := variables[t.name]; ok {
			return v
		}
		return nil
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = resolveGqlVariables(item, variables)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for key, item := range t {
			out[key] = resolveGqlVariables(item, variables)
		}
		return out
	}
	return value
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

// gqlValue is the executor's object model: a field map whose "__t" key is
// the GraphQL type and whose other "__"-prefixed keys are internal handles
// (stripped before output).
type gqlValue = map[string]any

type gqlCtx struct {
	app       *app
	errors    []string
	fragments map[string]*gqlFragment
	variables map[string]any
}

var (
	gqlIssueFields   = []string{"id", "databaseId", "number", "title", "body", "state", "url", "author", "labels", "comments", "createdAt", "updatedAt", "closedAt", "repository"}
	gqlPullFields    = []string{"id", "databaseId", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "files", "url", "author", "createdAt", "updatedAt", "closedAt", "mergedAt", "mergeCommit"}
	gqlRepoFields    = []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "isPrivate", "description", "url", "issue", "issues", "pullRequest", "pullRequests", "labels", "createdAt", "updatedAt"}
	gqlUserFields    = []string{"login", "id", "name", "avatarUrl", "url", "email"}
	gqlCommentFields = []string{"id", "body", "author", "createdAt", "updatedAt"}
	gqlRefFields     = []string{"name", "prefix", "target"}
	gqlConnFields    = []string{"nodes", "edges", "totalCount", "pageInfo"}
	gqlPageInfoField = []string{"hasNextPage", "hasPreviousPage", "endCursor", "startCursor"}
	gqlTypeFields    = []string{"name", "kind", "description", "fields", "enumValues", "possibleTypes", "inputFields", "interfaces", "ofType"}
)

func gqlFieldKnown(typ, field string) bool {
	contains := func(list []string) bool {
		for _, entry := range list {
			if entry == field {
				return true
			}
		}
		return false
	}
	switch typ {
	case "Query":
		return contains([]string{"viewer", "repository", "node", "__type", "__schema"})
	case "Mutation":
		return contains([]string{"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"})
	case "Repository":
		return contains(gqlRepoFields)
	case "Issue":
		return contains(gqlIssueFields)
	case "PullRequest":
		return contains(gqlPullFields)
	case "User":
		return contains(gqlUserFields)
	case "IssueComment":
		return contains(gqlCommentFields)
	case "Ref":
		return contains(gqlRefFields)
	case "IssueConnection", "PullRequestConnection", "PullRequestChangedFileConnection", "IssueCommentConnection":
		return contains(gqlConnFields)
	case "PageInfo":
		return contains(gqlPageInfoField)
	case "__Type":
		return contains(gqlTypeFields)
	case "IssueEdge", "PullRequestEdge", "IssueCommentEdge", "PullRequestChangedFileEdge":
		return field == "node" || field == "cursor"
	case "PullRequestChangedFile":
		return contains([]string{"path", "additions", "deletions", "changeType", "status"})
	case "Label":
		return contains([]string{"id", "name", "color", "description"})
	case "Commit":
		return contains([]string{"oid", "message", "author", "committer"})
	case "AddIssueCommentPayload", "CreateIssuePayload", "CloseIssuePayload", "CreatePullRequestPayload", "MergePullRequestPayload":
		return contains([]string{"issue", "pullRequest", "commentEdge", "clientMutationId"})
	}
	return false
}

func gqlTypeOf(value gqlValue) string {
	if typ, ok := value["__t"].(string); ok {
		return typ
	}
	return "Unknown"
}

// stripGqlInternal drops the "__"-prefixed bookkeeping keys recursively.
func stripGqlInternal(value any) any {
	switch t := value.(type) {
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = stripGqlInternal(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for key, item := range t {
			if strings.HasPrefix(key, "__") {
				continue
			}
			out[key] = stripGqlInternal(item)
		}
		return out
	}
	return value
}

func gqlUserValue() gqlValue {
	return gqlValue{
		"__t": "User", "login": viewerLogin, "id": globalID("User", viewerLogin),
		"name": viewerName, "email": "", "avatarUrl": "", "url": "",
	}
}

func gqlRepoValue(r *repo) gqlValue {
	return gqlValue{
		"__t": "Repository", "__repo": r,
		"id": globalID("Repository", viewerLogin+"/"+r.name), "databaseId": r.dbID,
		"name": r.name, "nameWithOwner": viewerLogin + "/" + r.name,
		"isPrivate": r.private, "description": r.description,
		"viewerPermission": "ADMIN", "hasIssuesEnabled": true,
		"createdAt": fixedISO, "updatedAt": fixedISO,
	}
}

func gqlIssueValue(r *repo, iss *issue) gqlValue {
	state := "OPEN"
	if iss.state != "open" {
		state = "CLOSED"
	}
	var closedAt any
	if iss.state == "closed" {
		closedAt = fixedISO
	}
	return gqlValue{
		"__t": "Issue", "__repo": r, "__issue": iss,
		"id": globalID("Issue", viewerLogin+"/"+r.name+"#"+itoa(iss.number)), "databaseId": iss.id,
		"number": iss.number, "title": iss.title, "body": iss.body, "state": state,
		"createdAt": fixedISO, "updatedAt": fixedISO, "closedAt": closedAt,
	}
}

func gqlPullValue(r *repo, p *pull) gqlValue {
	state := "OPEN"
	if p.merged {
		state = "MERGED"
	} else if p.state != "open" {
		state = "CLOSED"
	}
	var mergedAt, closedAt any
	if p.mergedAt != nil {
		mergedAt = *p.mergedAt
	}
	if p.state == "closed" {
		closedAt = fixedISO
	}
	return gqlValue{
		"__t": "PullRequest", "__repo": r, "__pull": p,
		"id": globalID("PullRequest", viewerLogin+"/"+r.name+"#"+itoa(p.number)), "databaseId": p.id,
		"number": p.number, "title": p.title, "body": p.body, "state": state,
		"merged": p.merged, "headRefName": p.headRef, "baseRefName": p.baseRef,
		"headRefOid": p.headSha, "baseRefOid": p.baseSha,
		"mergedAt": mergedAt, "createdAt": fixedISO, "updatedAt": fixedISO, "closedAt": closedAt,
	}
}

func gqlCommentValue(r *repo, iss *issue, c *comment) gqlValue {
	return gqlValue{
		"__t": "IssueComment", "__repo": r, "__issue": iss, "__comment": c,
		"id":   globalID("IssueComment", viewerLogin+"/"+r.name+"#"+itoa(iss.number)+"#"+itoa(c.id)),
		"body": c.body,
	}
}

func gqlCursorFor(number int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + itoa(number)))
}

// gqlNumberAfter decodes an `after` cursor back to its number, or returns
// ok=false when it is not a well-formed cursor.
func gqlNumberAfter(after any) (int, bool) {
	text, isStr := after.(string)
	if !isStr || text == "" {
		return 0, false
	}
	decoded, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(text)
		if err != nil {
			return 0, false
		}
	}
	decodedText := string(decoded)
	if !strings.HasPrefix(decodedText, "cursor:") {
		return 0, false
	}
	value, ok := parseIntJS(decodedText[len("cursor:"):])
	if !ok {
		return 0, false
	}
	return value, true
}

func gqlPageInfo(hasNext bool, endCursor, startCursor any) gqlValue {
	return gqlValue{
		"__t": "PageInfo", "hasNextPage": hasNext, "hasPreviousPage": false,
		"endCursor": endCursor, "startCursor": startCursor,
	}
}

func (c *gqlCtx) issuesConnection(r *repo, args map[string]any) gqlValue {
	var wanted map[string]bool
	if states, isList := args["states"].([]any); isList {
		wanted = make(map[string]bool, len(states))
		for _, entry := range states {
			wanted[strings.ToUpper(jsString(entry))] = true
		}
	}
	direction := "ASC"
	if orderBy, isObj := args["orderBy"].(map[string]any); isObj {
		if d, present := orderBy["direction"]; present {
			direction = strings.ToUpper(jsString(d))
		}
	}
	after, hasAfter := gqlNumberAfter(args["after"])
	first := 100.0
	if v, isNum := args["first"].(float64); isNum {
		first = v
	}
	issues := make([]*issue, 0, len(r.issues))
	for _, iss := range r.issues {
		issues = append(issues, iss)
	}
	if wanted != nil {
		filtered := issues[:0]
		for _, iss := range issues {
			state := "OPEN"
			if iss.state != "open" {
				state = "CLOSED"
			}
			if wanted[state] {
				filtered = append(filtered, iss)
			}
		}
		issues = filtered
	}
	if direction == "DESC" {
		sort.Slice(issues, func(i, j int) bool { return issues[i].seq > issues[j].seq })
	} else {
		sort.Slice(issues, func(i, j int) bool { return issues[i].seq < issues[j].seq })
	}
	if hasAfter {
		filtered := issues[:0]
		for _, iss := range issues {
			if direction == "DESC" {
				if iss.number < after {
					filtered = append(filtered, iss)
				}
			} else if iss.number > after {
				filtered = append(filtered, iss)
			}
		}
		issues = filtered
	}
	total := len(issues)
	page := firstPage(issues, first)
	nodes := make([]any, 0, len(page))
	for _, iss := range page {
		nodes = append(nodes, gqlIssueValue(r, iss))
	}
	var endCursor, startCursor any
	if len(page) > 0 {
		endCursor = gqlCursorFor(page[len(page)-1].number)
		startCursor = gqlCursorFor(page[0].number)
	}
	return gqlValue{
		"__t": "IssueConnection", "__nodes": nodes, "__total": total,
		"__pageInfo": gqlPageInfo(total > len(page), endCursor, startCursor),
	}
}

func (c *gqlCtx) pullsConnection(r *repo, args map[string]any) gqlValue {
	after, hasAfter := gqlNumberAfter(args["after"])
	first := 100.0
	if v, isNum := args["first"].(float64); isNum {
		first = v
	}
	pulls := make([]*pull, 0, len(r.pulls))
	for _, p := range r.pulls {
		pulls = append(pulls, p)
	}
	sort.Slice(pulls, func(i, j int) bool { return pulls[i].seq < pulls[j].seq })
	if hasAfter {
		filtered := pulls[:0]
		for _, p := range pulls {
			if p.number > after {
				filtered = append(filtered, p)
			}
		}
		pulls = filtered
	}
	total := len(pulls)
	page := firstPage(pulls, first)
	nodes := make([]any, 0, len(page))
	for _, p := range page {
		nodes = append(nodes, gqlPullValue(r, p))
	}
	var endCursor, startCursor any
	if len(page) > 0 {
		endCursor = gqlCursorFor(page[len(page)-1].number)
		startCursor = gqlCursorFor(page[0].number)
	}
	return gqlValue{
		"__t": "PullRequestConnection", "__nodes": nodes, "__total": total,
		"__pageInfo": gqlPageInfo(total > len(page), endCursor, startCursor),
	}
}

// firstPage mirrors slice(0, max(0, first)) for a float page size.
func firstPage[T any](items []T, first float64) []T {
	n := int(first)
	if n < 0 {
		n = 0
	}
	if n > len(items) {
		n = len(items)
	}
	return items[:n]
}

// jsString mirrors String(value ?? ”) for the scalar cases the executor
// coerces (ids and enum-ish arguments).
func jsString(value any) string {
	switch t := value.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return jsonNumber(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return ""
}

// jsNumber mirrors Number(value) for the numeric argument lookups.
func jsNumber(value any) (float64, bool) {
	switch t := value.(type) {
	case float64:
		return t, true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// parseGqlGlobalID decodes a global id into kind and key.
func parseGqlGlobalID(id string) (string, string, bool) {
	decoded, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(id)
		if err != nil {
			return "", "", false
		}
	}
	text := string(decoded)
	if !strings.HasPrefix(text, "gf:") {
		return "", "", false
	}
	rest := text[3:]
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return "", "", false
	}
	return rest[:colon], rest[colon+1:], true
}

// gqlRepoFromKey resolves the "owner/name" half of a global id key.
func (c *gqlCtx) repoFromKey(key string) *repo {
	parts := strings.Split(key, "/")
	if len(parts) != 2 {
		return nil
	}
	return c.app.store.getRepo(parts[0], parts[1])
}

// gqlIssueFromKey resolves an Issue global id key ("owner/name#number").
func (c *gqlCtx) issueFromKey(key string) (*repo, *issue) {
	hashParts := strings.Split(key, "#")
	if len(hashParts) != 2 {
		return nil, nil
	}
	r := c.repoFromKey(hashParts[0])
	if r == nil {
		return nil, nil
	}
	number, ok := parseIntJS(hashParts[1])
	if !ok {
		return r, nil
	}
	return r, r.issues[number]
}

func (c *gqlCtx) pullFromKey(key string) (*repo, *pull) {
	hashParts := strings.Split(key, "#")
	if len(hashParts) != 2 {
		return nil, nil
	}
	r := c.repoFromKey(hashParts[0])
	if r == nil {
		return nil, nil
	}
	number, ok := parseIntJS(hashParts[1])
	if !ok {
		return r, nil
	}
	return r, r.pulls[number]
}

func (c *gqlCtx) resolveField(parent gqlValue, field *gqlSelection) any {
	typ := gqlTypeOf(parent)
	name := field.name
	resolvedArgs := resolveGqlVariables(field.args, c.variables)
	args, _ := resolvedArgs.(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	s := c.app.store

	switch typ {
	case "Query":
		switch name {
		case "viewer":
			return gqlUserValue()
		case "repository":
			r := s.getRepo(jsString(args["owner"]), jsString(args["name"]))
			if r == nil {
				return nil
			}
			return gqlRepoValue(r)
		case "node":
			kind, key, ok := parseGqlGlobalID(jsString(args["id"]))
			if !ok {
				return nil
			}
			switch kind {
			case "Repository":
				if r := c.repoFromKey(key); r != nil {
					return gqlRepoValue(r)
				}
			case "Issue":
				if r, iss := c.issueFromKey(key); iss != nil {
					return gqlIssueValue(r, iss)
				}
			case "PullRequest":
				if r, p := c.pullFromKey(key); p != nil {
					return gqlPullValue(r, p)
				}
			}
			return nil
		case "__type":
			return gqlIntrospectionType(jsString(args["name"]))
		case "__schema":
			return gqlValue{
				"__t":          "__Schema",
				"queryType":    gqlValue{"__t": "__Type", "name": "Query"},
				"mutationType": gqlValue{"__t": "__Type", "name": "Mutation"},
			}
		}
		return nil
	case "Mutation":
		return c.resolveMutation(name, args)
	case "Repository":
		r := parent["__repo"].(*repo)
		switch name {
		case "owner":
			return gqlUserValue()
		case "defaultBranchRef":
			return gqlValue{"__t": "Ref", "name": r.defaultBranch, "prefix": "refs/heads/"}
		case "issue":
			if number, ok := jsNumber(args["number"]); ok {
				if iss := r.issues[int(number)]; iss != nil {
					return gqlIssueValue(r, iss)
				}
			}
			return nil
		case "issues":
			return c.issuesConnection(r, args)
		case "pullRequest":
			if number, ok := jsNumber(args["number"]); ok {
				if p := r.pulls[int(number)]; p != nil {
					return gqlPullValue(r, p)
				}
			}
			return nil
		case "pullRequests":
			return c.pullsConnection(r, args)
		case "labels":
			nodes := make([]any, 0, len(r.labels))
			for _, labelName := range r.labelOrder {
				if l := r.labels[labelName]; l != nil {
					nodes = append(nodes, gqlValue{"__t": "Label", "id": l.id, "name": l.name, "color": l.color, "description": ""})
				}
			}
			return gqlValue{
				"__t": "LabelConnection", "__nodes": nodes, "__total": len(r.labels),
				"__pageInfo": gqlPageInfo(false, nil, nil),
			}
		}
		return parent[name]
	case "Issue":
		r := parent["__repo"].(*repo)
		iss := parent["__issue"].(*issue)
		switch name {
		case "author":
			return gqlUserValue()
		case "repository":
			return gqlRepoValue(r)
		case "labels":
			nodes := make([]any, 0, len(iss.labels))
			for _, labelName := range iss.labels {
				id, color := 0, "ededed"
				if l := r.labels[labelName]; l != nil {
					id, color = l.id, l.color
				}
				nodes = append(nodes, gqlValue{"__t": "Label", "id": id, "name": labelName, "color": color, "description": ""})
			}
			return gqlValue{
				"__t": "LabelConnection", "__nodes": nodes, "__total": len(iss.labels),
				"__pageInfo": gqlPageInfo(false, nil, nil),
			}
		case "comments":
			first := 100.0
			if v, isNum := args["first"].(float64); isNum {
				first = v
			}
			page := firstPage(iss.comments, first)
			nodes := make([]any, 0, len(page))
			for _, comment := range page {
				nodes = append(nodes, gqlCommentValue(r, iss, comment))
			}
			return gqlValue{
				"__t": "IssueCommentConnection", "__nodes": nodes, "__total": len(iss.comments),
				"__pageInfo": gqlPageInfo(len(iss.comments) > len(nodes), nil, nil),
			}
		}
		return parent[name]
	case "PullRequest":
		r := parent["__repo"].(*repo)
		p := parent["__pull"].(*pull)
		switch name {
		case "author":
			return gqlUserValue()
		case "mergeable":
			if s.pullMergeable(r, p) {
				return "MERGEABLE"
			}
			return "CONFLICTING"
		case "files":
			first := 100.0
			if v, isNum := args["first"].(float64); isNum {
				first = v
			}
			files := firstPage(s.pullFiles(r, p), first)
			nodes := make([]any, 0, len(files))
			for _, f := range files {
				changeType := "MODIFIED"
				if f.status == "added" {
					changeType = "ADDED"
				} else if f.status == "removed" {
					changeType = "DELETED"
				}
				nodes = append(nodes, gqlValue{
					"__t": "PullRequestChangedFile", "path": f.filename,
					"additions": f.additions, "deletions": f.deletions,
					"changeType": changeType, "status": f.status,
				})
			}
			return gqlValue{
				"__t": "PullRequestChangedFileConnection", "__nodes": nodes, "__total": len(files),
				"__pageInfo": gqlPageInfo(false, nil, nil),
			}
		case "mergeCommit":
			if p.mergeSha == nil {
				return nil
			}
			return gqlValue{"__t": "Commit", "oid": *p.mergeSha, "message": ""}
		}
		return parent[name]
	case "IssueComment":
		if name == "author" {
			return gqlUserValue()
		}
		return parent[name]
	}

	if strings.HasSuffix(typ, "Connection") {
		switch name {
		case "nodes":
			if nodes, ok := parent["__nodes"].([]any); ok {
				return nodes
			}
			return []any{}
		case "edges":
			nodes, _ := parent["__nodes"].([]any)
			edgeType := strings.Replace(typ, "Connection", "Edge", 1)
			edges := make([]any, 0, len(nodes))
			for i, node := range nodes {
				edges = append(edges, gqlValue{"__t": edgeType, "node": node, "cursor": gqlCursorFor(i + 1)})
			}
			return edges
		case "totalCount":
			if total, ok := parent["__total"].(int); ok {
				return total
			}
			return 0
		case "pageInfo":
			return parent["__pageInfo"]
		}
		return nil
	}
	if strings.HasSuffix(typ, "Edge") {
		if name == "node" || name == "cursor" {
			return parent[name]
		}
		return nil
	}
	return parent[name]
}

func (c *gqlCtx) resolveMutation(name string, args map[string]any) any {
	s := c.app.store
	input, _ := args["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	switch name {
	case "createIssue":
		kind, key, ok := parseGqlGlobalID(jsString(input["repositoryId"]))
		if !ok || kind != "Repository" {
			c.errors = append(c.errors, "createIssue: unknown repositoryId")
			return nil
		}
		r := c.repoFromKey(key)
		if r == nil {
			c.errors = append(c.errors, "createIssue: repository not found")
			return nil
		}
		created, err := s.createIssue(r, jsString(input["title"]), jsString(input["body"]), nil)
		if err != nil {
			c.errors = append(c.errors, "createIssue: "+err.Error())
			return nil
		}
		return gqlValue{"__t": "CreateIssuePayload", "issue": gqlIssueValue(r, created)}
	case "closeIssue":
		kind, key, ok := parseGqlGlobalID(jsString(input["issueId"]))
		if !ok || kind != "Issue" {
			c.errors = append(c.errors, "closeIssue: unknown issueId")
			return nil
		}
		r, iss := c.issueFromKey(key)
		if iss == nil {
			c.errors = append(c.errors, "closeIssue: issue not found")
			return nil
		}
		iss.state = "closed"
		s.touchIssue(r, iss)
		return gqlValue{"__t": "CloseIssuePayload", "issue": gqlIssueValue(r, iss)}
	case "createPullRequest":
		kind, key, ok := parseGqlGlobalID(jsString(input["repositoryId"]))
		if !ok || kind != "Repository" {
			c.errors = append(c.errors, "createPullRequest: unknown repositoryId")
			return nil
		}
		r := c.repoFromKey(key)
		if r == nil {
			c.errors = append(c.errors, "createPullRequest: repository not found")
			return nil
		}
		created, err := s.createPull(r, jsString(input["title"]), jsString(input["body"]), jsString(input["headRefName"]), jsString(input["baseRefName"]))
		if err != nil {
			c.errors = append(c.errors, "createPullRequest: "+err.Error())
			return nil
		}
		return gqlValue{"__t": "CreatePullRequestPayload", "pullRequest": gqlPullValue(r, created)}
	case "mergePullRequest":
		kind, key, ok := parseGqlGlobalID(jsString(input["pullRequestId"]))
		if !ok || kind != "PullRequest" {
			c.errors = append(c.errors, "mergePullRequest: unknown pullRequestId")
			return nil
		}
		r, p := c.pullFromKey(key)
		if p == nil {
			c.errors = append(c.errors, "mergePullRequest: pull request not found")
			return nil
		}
		if _, err := s.mergePull(r, p); err != nil {
			c.errors = append(c.errors, "mergePullRequest: "+err.Error())
			return nil
		}
		return gqlValue{"__t": "MergePullRequestPayload", "pullRequest": gqlPullValue(r, p)}
	case "addComment":
		kind, key, ok := parseGqlGlobalID(jsString(input["subjectId"]))
		if !ok || kind != "Issue" {
			c.errors = append(c.errors, "addComment: unknown subjectId")
			return nil
		}
		r, iss := c.issueFromKey(key)
		if iss == nil {
			c.errors = append(c.errors, "addComment: issue not found")
			return nil
		}
		comment := &comment{id: s.nextID, body: jsString(input["body"])}
		s.nextID++
		iss.comments = append(iss.comments, comment)
		s.touchIssue(r, iss)
		node := gqlCommentValue(r, iss, comment)
		return gqlValue{
			"__t":         "AddIssueCommentPayload",
			"commentEdge": gqlValue{"__t": "IssueCommentEdge", "node": node, "cursor": gqlCursorFor(comment.id)},
		}
	}
	return nil
}

var gqlEnumTypes = map[string][]string{
	"SearchType":             {"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "CODE"},
	"IssueState":             {"OPEN", "CLOSED"},
	"PullRequestState":       {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":         {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"RepositoryPermission":   {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"OrderDirection":         {"ASC", "DESC"},
	"IssueOrderField":        {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
}

func gqlIntrospectionType(name string) gqlValue {
	if enumValues, isEnum := gqlEnumTypes[name]; isEnum {
		values := make([]any, 0, len(enumValues))
		for _, entry := range enumValues {
			values = append(values, map[string]any{"name": entry, "description": ""})
		}
		return gqlValue{
			"__t": "__Type", "name": name, "kind": "ENUM", "description": "",
			"fields": nil, "enumValues": values,
		}
	}
	var fields []string
	switch name {
	case "Issue":
		fields = gqlIssueFields
	case "PullRequest":
		fields = gqlPullFields
	case "Repository":
		fields = gqlRepoFields
	case "User":
		fields = gqlUserFields
	case "IssueComment":
		fields = gqlCommentFields
	}
	fieldValues := []any{}
	if fields != nil {
		for _, entry := range fields {
			fieldValues = append(fieldValues, map[string]any{"name": entry, "description": ""})
		}
	}
	return gqlValue{
		"__t": "__Type", "name": name, "kind": "OBJECT", "description": "",
		"fields": fieldValues, "enumValues": nil,
	}
}

// expandGqlSelections expands fragment spreads and inline fragments
// (the latter only when the type condition matches the runtime type).
func (c *gqlCtx) expandGqlSelections(selections []*gqlSelection, onType string) []*gqlSelection {
	var out []*gqlSelection
	for _, selection := range selections {
		if selection.fragment != "" {
			if fragment, ok := c.fragments[selection.fragment]; ok {
				out = append(out, c.expandGqlSelections(fragment.selections, onType)...)
			}
			continue
		}
		if selection.inlineType != "" {
			if selection.inlineType == onType {
				out = append(out, c.expandGqlSelections(selection.selections, onType)...)
			}
			continue
		}
		out = append(out, selection)
	}
	return out
}

func (c *gqlCtx) executeSelections(parent gqlValue, selections []*gqlSelection) map[string]any {
	typ := gqlTypeOf(parent)
	expanded := c.expandGqlSelections(selections, typ)
	out := make(map[string]any, len(expanded))
	for _, selection := range expanded {
		responseKey := selection.name
		if selection.hasAlias {
			responseKey = selection.alias
		}
		if !gqlFieldKnown(typ, selection.name) {
			c.errors = append(c.errors, fmt.Sprintf("Cannot query field %q on type %q.", selection.name, typ))
			out[responseKey] = nil
			continue
		}
		value := c.resolveField(parent, selection)
		out[responseKey] = c.selectValue(value, selection.selections)
	}
	return out
}

func (c *gqlCtx) selectValue(value any, selections []*gqlSelection) any {
	switch t := value.(type) {
	case nil:
		return nil
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = c.selectValue(item, selections)
		}
		return out
	case map[string]any:
		if _, hasType := t["__t"]; hasType && len(selections) > 0 {
			return c.executeSelections(t, selections)
		}
		return stripGqlInternal(t)
	}
	return value
}

func (a *app) runGraphQLQuery(entry any) map[string]any {
	record := asObject(entry)
	if record == nil {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "Invalid GraphQL request"}}}
	}
	query := asString(record["query"])
	variables := asObject(record["variables"])
	if variables == nil {
		variables = map[string]any{}
	}
	tokens, err := gqlLex(query)
	if err != nil {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": err.Error()}}}
	}
	doc, err := (&gqlParser{tokens: tokens}).parseDocument()
	if err != nil {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": err.Error()}}}
	}
	if len(doc.operations) == 0 {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "No operation in document"}}}
	}
	operation := doc.operations[0]
	ctx := &gqlCtx{app: a, fragments: doc.fragments, variables: variables}
	rootType := "Query"
	if operation.kind == "mutation" {
		rootType = "Mutation"
	}
	data := ctx.executeSelections(gqlValue{"__t": rootType}, operation.selections)
	out := map[string]any{"data": data}
	if len(ctx.errors) > 0 {
		errorsList := make([]any, 0, len(ctx.errors))
		for _, message := range ctx.errors {
			errorsList = append(errorsList, map[string]any{"message": message})
		}
		out["errors"] = errorsList
	}
	return out
}

func (a *app) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	raw, err := readRequestBody(r)
	if err != nil {
		a.graphqlSend(w, 500, map[string]any{"errors": []any{map[string]any{"message": err.Error()}}})
		return
	}
	parsed, ok := parseJSONBody(raw)
	if !ok {
		a.graphqlSend(w, 400, map[string]any{"errors": []any{map[string]any{"message": "Invalid JSON body"}}})
		return
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if batch, isArray := parsed.([]any); isArray {
		results := make([]any, 0, len(batch))
		for _, entry := range batch {
			results = append(results, a.runGraphQLQuery(entry))
		}
		a.graphqlSend(w, 200, results)
		return
	}
	a.graphqlSend(w, 200, a.runGraphQLQuery(parsed))
}

// graphqlSend writes a GraphQL body without the REST request-id header.
func (a *app) graphqlSend(w http.ResponseWriter, status int, value any) {
	text := marshalJSON(value) + "\n"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", itoa(len(text)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(text))
}
