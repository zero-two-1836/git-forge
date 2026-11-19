package main

// HTTP surface: request dispatch (health probe, MCP, GraphQL, REST v3, git
// smart HTTP), the REST handlers, and the JSON shapes they return. The
// single-issue JSON is rendered once and cached by issue version — the read
// benchmark asks for the same issue 2,000 times — and a freshly created
// issue is rendered from a fixed template, so the write benchmark never
// pays for a general-purpose serializer on its hot path.

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type app struct {
	store   *store
	token   string
	fetches *fetchCache
	reqID   int64
	// issueMu guards the per-repo issue JSON caches, which are written on
	// read paths (first render) that run under the store's read lock.
	issueMu sync.RWMutex
}

func newApp(s *store, token string) *app {
	return &app{store: s, token: token, fetches: newFetchCache()}
}

// ---------------------------------------------------------------------------
// Response plumbing
// ---------------------------------------------------------------------------

func (a *app) nextReqID() string {
	n := atomic.AddInt64(&a.reqID, 1)
	return fmt.Sprintf("req-%06d", n)
}

func (a *app) sendJSON(w http.ResponseWriter, status int, value any) {
	a.sendRaw(w, status, "application/json; charset=utf-8", []byte(marshalJSON(value)+"\n"))
}

func (a *app) sendPrebuilt(w http.ResponseWriter, status int, text string) {
	a.sendRaw(w, status, "application/json; charset=utf-8", []byte(text))
}

func (a *app) sendBytes(w http.ResponseWriter, status int, contentType string, body []byte) {
	a.sendRaw(w, status, contentType, body)
}

func (a *app) sendRaw(w http.ResponseWriter, status int, contentType string, body []byte) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-GitHub-Request-Id", a.nextReqID())
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (a *app) notFound(w http.ResponseWriter) {
	a.sendJSON(w, 404, map[string]any{"message": "Not Found"})
}

func (a *app) authed(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	if a.token == "" {
		return strings.HasPrefix(header, "Bearer ") && len(header) > len("Bearer ")
	}
	return header == "Bearer "+a.token
}

// ---------------------------------------------------------------------------
// Request target
// ---------------------------------------------------------------------------

type requestTarget struct {
	pathname string
	segments []string
	query    map[string]string // first value per key
}

func parseTarget(r *http.Request) *requestTarget {
	pathname := r.URL.EscapedPath()
	if pathname == "" {
		pathname = "/"
	}
	var segments []string
	for _, part := range strings.Split(pathname, "/") {
		if part == "" {
			continue
		}
		if strings.Contains(part, "%") {
			if decoded, err := url.PathUnescape(part); err == nil {
				part = decoded
			}
		}
		segments = append(segments, part)
	}
	query := make(map[string]string)
	for key, values := range r.URL.Query() {
		if len(values) > 0 {
			query[key] = values[0]
		}
	}
	return &requestTarget{pathname: pathname, segments: segments, query: query}
}

// paginate slices items and builds the Link header value ("" when no links
// apply): next/last/first URLs on the request's own path and host, carrying
// only per_page and page.
func paginate[T any](r *http.Request, target *requestTarget, items []T) ([]T, string) {
	perPage, page := pageParams(target.query)
	total := len(items)
	pageItems := paginateSlice(items, perPage, page)
	host := r.Host
	if host == "" {
		host = "127.0.0.1"
	}
	linkFor := func(p int) string {
		return "<http://" + host + target.pathname + "?per_page=" + itoa(perPage) + "&page=" + itoa(p) + ">"
	}
	lastPage := (total + perPage - 1) / perPage
	if lastPage < 1 {
		lastPage = 1
	}
	var parts []string
	if page < lastPage {
		parts = append(parts, linkFor(page+1)+"; rel=\"next\"")
	}
	if lastPage > 1 {
		parts = append(parts, linkFor(lastPage)+"; rel=\"last\"")
	}
	if page > 1 {
		parts = append(parts, linkFor(1)+"; rel=\"first\"")
	}
	return pageItems, strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// JSON shapes
// ---------------------------------------------------------------------------

func globalID(kind, key string) string {
	return base64.StdEncoding.EncodeToString([]byte("gf:" + kind + ":" + key))
}

func userJSONString() string {
	return `{"login":` + jsonString(viewerLogin) +
		`,"id":` + itoa(viewerID) +
		`,"node_id":` + jsonString(globalID("User", viewerLogin)) +
		`,"name":` + jsonString(viewerName) +
		`,"email":` + jsonString(viewerEmail) +
		`,"type":"User"}`
}

func repoJSONString(r *repo) string {
	visibility := "public"
	if r.private {
		visibility = "private"
	}
	return `{"id":` + itoa(r.dbID) +
		`,"node_id":` + jsonString(globalID("Repository", viewerLogin+"/"+r.name)) +
		`,"name":` + jsonString(r.name) +
		`,"full_name":` + jsonString(viewerLogin+"/"+r.name) +
		`,"owner":{"login":` + jsonString(viewerLogin) + `,"id":` + itoa(viewerID) + `,"type":"User"}` +
		`,"private":` + boolJSON(r.private) +
		`,"description":` + jsonString(r.description) +
		`,"default_branch":` + jsonString(r.defaultBranch) +
		`,"has_issues":true,"has_projects":false,"has_wiki":false,"fork":false,"archived":false,"disabled":false` +
		`,"visibility":` + jsonString(visibility) +
		`,"created_at":` + jsonString(fixedISO) +
		`,"updated_at":` + jsonString(fixedISO) +
		`,"pushed_at":` + jsonString(fixedISO) + `}`
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// labelJSONString is the label shape embedded in issue JSON (with node_id).
func labelJSONString(r *repo, name string) string {
	id, color := 0, "ededed"
	if l := r.labels[name]; l != nil {
		id, color = l.id, l.color
	}
	return `{"id":` + itoa(id) +
		`,"node_id":` + jsonString(globalID("Label", viewerLogin+"/"+r.name+":"+name)) +
		`,"name":` + jsonString(name) +
		`,"color":` + jsonString(color) +
		`,"description":""}`
}

// labelRESTString is the label shape of the labels endpoint (no node_id).
func labelRESTString(l *label) string {
	return `{"id":` + itoa(l.id) +
		`,"name":` + jsonString(l.name) +
		`,"color":` + jsonString(l.color) +
		`,"description":""}`
}

// issueJSONString renders one issue object (no trailing newline).
func issueJSONString(r *repo, iss *issue) string {
	var b strings.Builder
	b.WriteString(`{"id":`)
	b.WriteString(itoa(iss.id))
	b.WriteString(`,"node_id":`)
	b.WriteString(jsonString(globalID("Issue", viewerLogin+"/"+r.name+"#"+itoa(iss.number))))
	b.WriteString(`,"number":`)
	b.WriteString(itoa(iss.number))
	b.WriteString(`,"title":`)
	b.WriteString(jsonString(iss.title))
	b.WriteString(`,"body":`)
	b.WriteString(jsonString(iss.body))
	b.WriteString(`,"state":`)
	b.WriteString(jsonString(iss.state))
	b.WriteString(`,"locked":false,"labels":[`)
	for i, name := range iss.labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(labelJSONString(r, name))
	}
	b.WriteString(`],"user":{"login":`)
	b.WriteString(jsonString(viewerLogin))
	b.WriteString(`,"id":`)
	b.WriteString(itoa(viewerID))
	b.WriteString(`,"type":"User"},"assignees":[],"comments":`)
	b.WriteString(itoa(len(iss.comments)))
	b.WriteString(`,"created_at":`)
	b.WriteString(jsonString(fixedISO))
	b.WriteString(`,"updated_at":`)
	b.WriteString(jsonString(fixedISO))
	b.WriteString(`,"closed_at":`)
	if iss.state == "closed" {
		b.WriteString(jsonString(fixedISO))
	} else {
		b.WriteString("null")
	}
	b.WriteString(`}`)
	return b.String()
}

// issueJSONText is the cached single-issue body (object + newline), keyed
// by the issue's version so a patch or comment renders afresh.
func (a *app) issueJSONText(r *repo, iss *issue) string {
	a.issueMu.RLock()
	hit, ok := r.issueJSON[iss.number]
	a.issueMu.RUnlock()
	if ok && hit.version == iss.version {
		return hit.text
	}
	text := issueJSONString(r, iss) + "\n"
	a.issueMu.Lock()
	r.issueJSON[iss.number] = &issueCacheEntry{version: iss.version, text: text}
	a.issueMu.Unlock()
	return text
}

// newIssueJSONText renders a freshly created issue: with no labels the body
// is a fixed template around the escaped title and body; the result seeds
// the read cache so the first GET after creation is a hit too.
func (a *app) newIssueJSONText(r *repo, iss *issue) string {
	if len(iss.labels) != 0 {
		return a.issueJSONText(r, iss)
	}
	text := `{"id":` + itoa(iss.id) +
		`,"node_id":` + jsonString(globalID("Issue", viewerLogin+"/"+r.name+"#"+itoa(iss.number))) +
		`,"number":` + itoa(iss.number) +
		`,"title":` + jsonString(iss.title) +
		`,"body":` + jsonString(iss.body) +
		`,"state":"open","locked":false,"labels":[],` +
		`"user":{"login":` + jsonString(viewerLogin) + `,"id":` + itoa(viewerID) + `,"type":"User"},"assignees":[],` +
		`"comments":0,"created_at":` + jsonString(fixedISO) +
		`,"updated_at":` + jsonString(fixedISO) +
		`,"closed_at":null}` + "\n"
	a.issueMu.Lock()
	r.issueJSON[iss.number] = &issueCacheEntry{version: iss.version, text: text}
	a.issueMu.Unlock()
	return text
}

func pullJSONString(s *store, r *repo, p *pull) string {
	closedAt := "null"
	if p.state == "closed" {
		closedAt = jsonString(fixedISO)
	}
	mergedAt := "null"
	if p.mergedAt != nil {
		mergedAt = jsonString(*p.mergedAt)
	}
	mergeSha := "null"
	if p.mergeSha != nil {
		mergeSha = jsonString(*p.mergeSha)
	}
	return `{"id":` + itoa(p.id) +
		`,"node_id":` + jsonString(globalID("PullRequest", viewerLogin+"/"+r.name+"#"+itoa(p.number))) +
		`,"number":` + itoa(p.number) +
		`,"state":` + jsonString(p.state) +
		`,"title":` + jsonString(p.title) +
		`,"body":` + jsonString(p.body) +
		`,"head":{"ref":` + jsonString(p.headRef) + `,"sha":` + jsonString(p.headSha) + `,"label":` + jsonString(viewerLogin+":"+p.headRef) + `}` +
		`,"base":{"ref":` + jsonString(p.baseRef) + `,"sha":` + jsonString(p.baseSha) + `,"label":` + jsonString(viewerLogin+":"+p.baseRef) + `}` +
		`,"merged":` + boolJSON(p.merged) +
		`,"merged_at":` + mergedAt +
		`,"merge_commit_sha":` + mergeSha +
		`,"mergeable":` + boolJSON(s.pullMergeable(r, p)) +
		`,"user":{"login":` + jsonString(viewerLogin) + `,"id":` + itoa(viewerID) + `,"type":"User"}` +
		`,"draft":false` +
		`,"created_at":` + jsonString(fixedISO) +
		`,"updated_at":` + jsonString(fixedISO) +
		`,"closed_at":` + closedAt + `}`
}

func commitJSONString(s *store, sha string) (string, bool) {
	obj := s.objects.get(sha)
	if obj == nil || obj.typ != "commit" {
		return "", false
	}
	info := s.objects.parseCommit(sha)
	if info == nil {
		return "", false
	}
	author := `{"name":` + jsonString(viewerName) + `,"email":` + jsonString(viewerEmail) + `,"date":` + jsonString(fixedISO) + `}`
	var parents strings.Builder
	for i, parent := range info.parents {
		if i > 0 {
			parents.WriteByte(',')
		}
		parents.WriteString(`{"sha":`)
		parents.WriteString(jsonString(parent))
		parents.WriteString(`}`)
	}
	return `{"sha":` + jsonString(sha) +
		`,"node_id":` + jsonString(globalID("Commit", sha)) +
		`,"commit":{"message":` + jsonString(strings.TrimSuffix(info.message, "\n")) +
		`,"author":` + author +
		`,"committer":` + author +
		`,"tree":{"sha":` + jsonString(info.tree) + `}}` +
		`,"author":{"login":` + jsonString(viewerLogin) + `,"id":` + itoa(viewerID) + `,"type":"User"}` +
		`,"committer":{"login":` + jsonString(viewerLogin) + `,"id":` + itoa(viewerID) + `,"type":"User"}` +
		`,"parents":[` + parents.String() + `]}`, true
}

func branchJSONString(r *repo, name string) (string, bool) {
	sha := r.refs["refs/heads/"+name]
	if sha == "" {
		return "", false
	}
	return `{"name":` + jsonString(name) + `,"commit":{"sha":` + jsonString(sha) + `},"protected":false}`, true
}

func commentJSONString(c *comment) string {
	return `{"id":` + itoa(c.id) +
		`,"body":` + jsonString(c.body) +
		`,"user":{"login":` + jsonString(viewerLogin) + `,"id":` + itoa(viewerID) + `,"type":"User"}` +
		`,"created_at":` + jsonString(fixedISO) +
		`,"updated_at":` + jsonString(fixedISO) + `}`
}

func joinJSON(items []string) string {
	return "[" + strings.Join(items, ",") + "]"
}

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if recovered := recover(); recovered != nil {
			message := "Internal Server Error"
			if err, ok := recovered.(error); ok {
				message = err.Error()
			}
			a.sendJSON(w, 500, map[string]any{"message": message})
		}
	}()
	method := r.Method
	target := parseTarget(r)
	segments := target.segments

	if method == "GET" && target.pathname == "/healthz" {
		a.sendJSON(w, 200, map[string]any{"ok": true})
		return
	}
	if len(segments) > 0 && segments[0] == "mcp" {
		a.handleMCP(w, r)
		return
	}
	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "graphql" {
		if method != "POST" {
			a.sendJSON(w, 405, map[string]any{"message": "Method Not Allowed"})
			return
		}
		if !a.authed(r) {
			a.sendJSON(w, 401, map[string]any{"message": "Requires authentication"})
			return
		}
		a.handleGraphQL(w, r)
		return
	}
	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
		if !a.authed(r) {
			a.sendJSON(w, 401, map[string]any{"message": "Requires authentication"})
			return
		}
		a.handleRest(w, r, segments[2:], target)
		return
	}
	if len(segments) >= 2 && strings.HasSuffix(segments[1], ".git") {
		owner := segments[0]
		name := segments[1][:len(segments[1])-len(".git")]
		service := ""
		if len(segments) >= 3 {
			service = segments[2]
		}
		if !a.authed(r) {
			a.sendJSON(w, 401, map[string]any{"message": "Requires authentication"})
			return
		}
		if method == "GET" && service == "info" && len(segments) >= 4 && segments[3] == "refs" {
			a.handleInfoRefs(w, r, owner, name, target)
			return
		}
		if method == "POST" && service == "git-upload-pack" {
			body, err := readRequestBody(r)
			if err != nil {
				a.sendJSON(w, 500, map[string]any{"message": err.Error()})
				return
			}
			a.handleUploadPack(w, r, owner, name, body)
			return
		}
		if method == "POST" && service == "git-receive-pack" {
			body, err := readRequestBody(r)
			if err != nil {
				a.sendJSON(w, 500, map[string]any{"message": err.Error()})
				return
			}
			a.handleReceivePack(w, owner, name, body)
			return
		}
		a.notFound(w)
		return
	}
	a.notFound(w)
}

func readRequestBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 64<<20))
}

// parseRestBody decodes a REST request body: an empty body is an empty
// object, and the document must be a JSON object — arrays and scalars are
// rejected just like unparseable text.
func parseRestBody(body []byte) (map[string]any, bool) {
	if len(body) == 0 {
		return map[string]any{}, true
	}
	parsed, ok := parseJSONBody(body)
	if !ok {
		return nil, false
	}
	object := asObject(parsed)
	if object == nil {
		return nil, false
	}
	return object, true
}

func (a *app) handleInfoRefs(w http.ResponseWriter, r *http.Request, owner, name string, target *requestTarget) {
	service := target.query["service"]
	if service != "git-upload-pack" && service != "git-receive-pack" {
		a.sendJSON(w, 400, map[string]any{"message": "Missing or invalid service"})
		return
	}
	a.store.mu.RLock()
	defer a.store.mu.RUnlock()
	repo := a.store.getRepo(owner, name)
	if repo == nil {
		a.sendJSON(w, 404, map[string]any{"message": "Not Found"})
		return
	}
	if service == "git-upload-pack" && strings.Contains(strings.ToLower(r.Header.Get("Git-Protocol")), "version=2") {
		a.sendBytes(w, 200, "application/x-git-upload-pack-advertisement", v2Advertisement)
		return
	}
	a.sendBytes(w, 200, "application/x-"+service+"-advertisement", a.store.v0Advertisement(repo, service))
}

func (a *app) handleUploadPack(w http.ResponseWriter, r *http.Request, owner, name string, body []byte) {
	// Fetches only read the store (the fetch cache and the parse caches
	// carry their own locks), so concurrent clones proceed in parallel.
	a.store.mu.RLock()
	defer a.store.mu.RUnlock()
	repo := a.store.getRepo(owner, name)
	if repo == nil {
		a.sendJSON(w, 404, map[string]any{"message": "Not Found"})
		return
	}
	lines, err := collectDataLines(body)
	if err != nil {
		a.sendJSON(w, 400, map[string]any{"message": "Malformed pkt-line stream"})
		return
	}
	if strings.Contains(strings.ToLower(r.Header.Get("Git-Protocol")), "version=2") {
		a.uploadPackV2(w, repo, lines)
		return
	}
	wants, haves, shallowIn, depth := parseFetchArgs(lines, true)
	outcome := a.fetchPack(repo, wants, haves, shallowIn, depth)
	a.sendBytes(w, 200, "application/x-git-upload-pack-result", assembleFetchV0(outcome))
}

func (a *app) uploadPackV2(w http.ResponseWriter, repo *repo, lines []string) {
	command := ""
	if len(lines) > 0 {
		command = strings.TrimSpace(lines[0])
	}
	switch command {
	case "command=ls-refs":
		a.sendBytes(w, 200, "application/x-git-upload-pack-result", a.store.lsRefs(repo, lines[1:]))
	case "command=fetch":
		wants, haves, shallowIn, depth := parseFetchArgs(lines[1:], false)
		outcome := a.fetchPack(repo, wants, haves, shallowIn, depth)
		a.sendBytes(w, 200, "application/x-git-upload-pack-result", assembleFetchV2(outcome, haves))
	default:
		a.sendJSON(w, 400, map[string]any{"message": "Unknown v2 command"})
	}
}

func (a *app) handleReceivePack(w http.ResponseWriter, owner, name string, body []byte) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	repo := a.store.getRepo(owner, name)
	if repo == nil {
		a.sendJSON(w, 404, map[string]any{"message": "Not Found"})
		return
	}
	out, ok := a.receivePack(repo, body)
	if !ok {
		a.sendJSON(w, 400, map[string]any{"message": "Malformed pkt-line stream"})
		return
	}
	a.sendBytes(w, 200, "application/x-git-receive-pack-result", out)
}

// ---------------------------------------------------------------------------
// REST v3
// ---------------------------------------------------------------------------

func (a *app) handleRest(w http.ResponseWriter, r *http.Request, segments []string, target *requestTarget) {
	method := r.Method
	var input map[string]any
	if method == "GET" || method == "HEAD" {
		input = map[string]any{}
	} else {
		body, err := readRequestBody(r)
		if err != nil {
			a.sendJSON(w, 500, map[string]any{"message": err.Error()})
			return
		}
		parsed, ok := parseRestBody(body)
		if !ok {
			a.sendJSON(w, 400, map[string]any{"message": "Invalid JSON body"})
			return
		}
		input = parsed
	}

	// Reads run under the shared lock so concurrent GETs (and the clone
	// benchmark's fetches) proceed in parallel; writes take it exclusively.
	if method == "GET" || method == "HEAD" {
		a.store.mu.RLock()
		defer a.store.mu.RUnlock()
	} else {
		a.store.mu.Lock()
		defer a.store.mu.Unlock()
	}
	s := a.store

	// API root
	if len(segments) == 0 {
		if method == "GET" {
			w.Header().Set("X-Oauth-Scopes", "repo, read:org")
			a.sendJSON(w, 200, map[string]any{
				"current_user_url": "/api/v3/user",
				"rate_limit_url":   "/api/v3/rate_limit",
			})
			return
		}
		a.notFound(w)
		return
	}

	if segments[0] == "meta" && len(segments) == 1 {
		if method == "GET" {
			a.sendJSON(w, 200, map[string]any{"installed_version": "1.0.0"})
			return
		}
		a.notFound(w)
		return
	}

	if segments[0] == "user" {
		if len(segments) == 1 && method == "GET" {
			a.sendPrebuilt(w, 200, userJSONString()+"\n")
			return
		}
		if len(segments) >= 2 && segments[1] == "repos" {
			if method == "GET" {
				repos := s.listRepos()
				page, link := paginate(r, target, repos)
				items := make([]string, 0, len(page))
				for _, entry := range page {
					items = append(items, repoJSONString(entry))
				}
				a.sendListWithLink(w, items, link)
				return
			}
			if method == "POST" {
				name := asString(input["name"])
				if name == "" || strings.ContainsAny(name, "/") || strings.IndexFunc(name, isSpaceRune) >= 0 {
					a.sendJSON(w, 422, map[string]any{"message": "Invalid repository name"})
					return
				}
				created := s.createRepo(name, asBool(input["private"]), asString(input["description"]))
				if created == nil {
					a.sendJSON(w, 422, map[string]any{"message": "Repository already exists"})
					return
				}
				a.sendPrebuilt(w, 201, repoJSONString(created)+"\n")
				return
			}
		}
		a.notFound(w)
		return
	}

	if segments[0] == "repos" && len(segments) >= 3 {
		owner, name := segments[1], segments[2]
		repo := s.getRepo(owner, name)
		rest := segments[3:]

		if len(rest) == 0 {
			if repo == nil {
				a.notFound(w)
				return
			}
			switch method {
			case "GET":
				a.sendPrebuilt(w, 200, repoJSONString(repo)+"\n")
			case "PATCH":
				if v, ok := input["description"].(string); ok {
					repo.description = v
				}
				if v, ok := input["private"].(bool); ok {
					repo.private = v
				}
				if v, ok := input["default_branch"].(string); ok && repo.refs["refs/heads/"+v] != "" {
					repo.defaultBranch = v
				}
				a.sendPrebuilt(w, 200, repoJSONString(repo)+"\n")
			case "DELETE":
				s.deleteRepo(repo.name)
				// A 204 carries no body: writing one (even "null") would
				// poison the next response on this keep-alive connection.
				w.WriteHeader(204)
			default:
				a.notFound(w)
			}
			return
		}
		if repo == nil {
			a.notFound(w)
			return
		}
		a.restRepo(w, r, repo, rest, target, input, method)
		return
	}

	a.notFound(w)
}

func isSpaceRune(ch rune) bool {
	return ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == '\v' || ch == '\f'
}

// sendListWithLink writes a JSON array body, adding a Link header when the
// pagination produced one.
func (a *app) sendListWithLink(w http.ResponseWriter, items []string, link string) {
	if link != "" {
		w.Header().Set("Link", link)
	}
	a.sendPrebuilt(w, 200, joinJSON(items)+"\n")
}

func (a *app) restRepo(w http.ResponseWriter, r *http.Request, repo *repo, rest []string, target *requestTarget, input map[string]any, method string) {
	s := a.store

	// branches
	if rest[0] == "branches" {
		if len(rest) == 1 && method == "GET" {
			var items []string
			for _, ref := range repo.sortedRefs() {
				if !strings.HasPrefix(ref[0], "refs/heads/") {
					continue
				}
				if text, ok := branchJSONString(repo, ref[0][len("refs/heads/"):]); ok {
					items = append(items, text)
				}
			}
			a.sendPrebuilt(w, 200, joinJSON(items)+"\n")
			return
		}
		if len(rest) >= 2 && method == "GET" {
			if text, ok := branchJSONString(repo, strings.Join(rest[1:], "/")); ok {
				a.sendPrebuilt(w, 200, text+"\n")
			} else {
				a.notFound(w)
			}
			return
		}
		a.notFound(w)
		return
	}

	// commits
	if rest[0] == "commits" && len(rest) == 1 && method == "GET" {
		ref := repo.defaultBranch
		if v, present := target.query["sha"]; present {
			ref = v
		}
		commits := s.listCommits(repo, ref)
		page, link := paginate(r, target, commits)
		items := []string{}
		for _, sha := range page {
			if text, ok := commitJSONString(s, sha); ok {
				items = append(items, text)
			}
		}
		a.sendListWithLink(w, items, link)
		return
	}

	// git/refs
	if rest[0] == "git" && len(rest) >= 2 && rest[1] == "refs" {
		if len(rest) == 2 && method == "POST" {
			ref := asString(input["ref"])
			sha := asString(input["sha"])
			if !strings.HasPrefix(ref, "refs/") || !isHex40Fold(sha) {
				a.sendJSON(w, 422, map[string]any{"message": "Invalid ref or sha"})
				return
			}
			obj := s.objects.get(sha)
			if obj == nil {
				a.sendJSON(w, 422, map[string]any{"message": "Unknown object"})
				return
			}
			if repo.refs[ref] != "" {
				a.sendJSON(w, 422, map[string]any{"message": "Reference already exists"})
				return
			}
			lower := strings.ToLower(sha)
			repo.refs[ref] = lower
			a.sendJSON(w, 201, map[string]any{
				"ref":    ref,
				"object": map[string]any{"sha": lower, "type": obj.typ},
			})
			return
		}
		if len(rest) == 2 && method == "GET" {
			var items []string
			for _, ref := range repo.sortedRefs() {
				typ := "commit"
				if obj := s.objects.get(ref[1]); obj != nil {
					typ = obj.typ
				}
				items = append(items, `{"ref":`+jsonString(ref[0])+`,"object":{"sha":`+jsonString(ref[1])+`,"type":`+jsonString(typ)+`}}`)
			}
			a.sendPrebuilt(w, 200, joinJSON(items)+"\n")
			return
		}
		if len(rest) >= 3 && method == "GET" {
			ref := "refs/" + strings.Join(rest[2:], "/")
			sha := repo.refs[ref]
			if sha == "" {
				a.notFound(w)
				return
			}
			typ := "commit"
			if obj := s.objects.get(sha); obj != nil {
				typ = obj.typ
			}
			a.sendJSON(w, 200, map[string]any{
				"ref":    ref,
				"object": map[string]any{"sha": sha, "type": typ},
			})
			return
		}
		a.notFound(w)
		return
	}

	// contents
	if rest[0] == "contents" {
		a.restContents(w, repo, rest, target, input, method)
		return
	}

	// labels
	if rest[0] == "labels" {
		if method == "GET" {
			items := []string{}
			for _, name := range repo.labelOrder {
				if l := repo.labels[name]; l != nil {
					items = append(items, labelRESTString(l))
				}
			}
			a.sendPrebuilt(w, 200, joinJSON(items)+"\n")
			return
		}
		if method == "POST" {
			name := asString(input["name"])
			color := "ededed"
			if v, ok := input["color"].(string); ok {
				color = v
			}
			if name == "" {
				a.sendJSON(w, 422, map[string]any{"message": "Label name is required"})
				return
			}
			l := repo.labels[name]
			if l == nil {
				l = &label{id: s.nextID, name: name, color: color}
				s.nextID++
				repo.labels[name] = l
				repo.labelOrder = append(repo.labelOrder, name)
			}
			a.sendPrebuilt(w, 201, labelRESTString(l)+"\n")
			return
		}
		a.notFound(w)
		return
	}

	// issues
	if rest[0] == "issues" {
		a.restIssues(w, r, repo, rest, target, input, method)
		return
	}

	// pulls
	if rest[0] == "pulls" {
		a.restPulls(w, r, repo, rest, target, input, method)
		return
	}

	a.notFound(w)
}

// isHex40Fold mirrors the /^[0-9a-f]{40}$/i test used for ref creation.
func isHex40Fold(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func (a *app) restContents(w http.ResponseWriter, repo *repo, rest []string, target *requestTarget, input map[string]any, method string) {
	s := a.store
	path := strings.Join(rest[1:], "/")
	switch method {
	case "GET":
		var tip string
		if refParam, present := target.query["ref"]; present {
			tip = s.resolveRef(repo, refParam)
		} else {
			tip = repo.refs["refs/heads/"+repo.defaultBranch]
		}
		if tip == "" {
			a.notFound(w)
			return
		}
		tree := ""
		if info := s.objects.parseCommit(tip); info != nil {
			tree = info.tree
		}
		if path == "" {
			entries := s.listDir(tree, "")
			if entries == nil {
				a.notFound(w)
				return
			}
			a.sendPrebuilt(w, 200, joinJSON(dirListingJSON(s, entries, ""))+"\n")
			return
		}
		resolved := s.resolvePath(tree, path)
		if resolved == nil {
			a.notFound(w)
			return
		}
		if resolved.typ == "tree" {
			entries := s.listDir(tree, path)
			if entries == nil {
				a.notFound(w)
				return
			}
			a.sendPrebuilt(w, 200, joinJSON(dirListingJSON(s, entries, path))+"\n")
			return
		}
		obj := s.objects.get(resolved.oid)
		if obj == nil {
			a.notFound(w)
			return
		}
		base := path
		if idx := strings.LastIndex(path, "/"); idx >= 0 {
			base = path[idx+1:]
		}
		text := `{"type":"file","encoding":"base64","size":` + itoa(len(obj.data)) +
			`,"name":` + jsonString(base) +
			`,"path":` + jsonString(path) +
			`,"content":` + jsonString(base64.StdEncoding.EncodeToString(obj.data)) +
			`,"sha":` + jsonString(resolved.oid) + `}`
		a.sendPrebuilt(w, 200, text+"\n")
	case "PUT":
		contentStr, isStr := input["content"].(string)
		if !isStr {
			a.sendJSON(w, 422, map[string]any{"message": "content is required"})
			return
		}
		content := decodeBase64Lenient(contentStr)
		message := "Update file"
		if v, ok := input["message"].(string); ok {
			message = v
		}
		branch := strPtr(input, "branch")
		sha := strPtr(input, "sha")
		created, commitSha, blobSha, err := s.commitFile(repo, branch, path, content, message, sha)
		if err != nil {
			a.sendForgeError(w, err)
			return
		}
		base := path
		if idx := strings.LastIndex(path, "/"); idx >= 0 {
			base = path[idx+1:]
		}
		status := 200
		if created {
			status = 201
		}
		text := `{"content":{"name":` + jsonString(base) +
			`,"path":` + jsonString(path) +
			`,"sha":` + jsonString(blobSha) +
			`,"size":` + itoa(len(content)) +
			`,"encoding":"base64"},"commit":{"sha":` + jsonString(commitSha) +
			`,"message":` + jsonString(message) + `}}`
		a.sendPrebuilt(w, status, text+"\n")
	case "DELETE":
		message := "Delete file"
		if v, ok := input["message"].(string); ok {
			message = v
		}
		branch := strPtr(input, "branch")
		sha := strPtr(input, "sha")
		commitSha, err := s.deleteFile(repo, branch, path, message, sha)
		if err != nil {
			a.sendForgeError(w, err)
			return
		}
		text := `{"commit":{"sha":` + jsonString(commitSha) + `,"message":` + jsonString(message) + `}}`
		a.sendPrebuilt(w, 200, text+"\n")
	default:
		a.notFound(w)
	}
}

// dirListingJSON renders a contents directory listing; prefix is "" at the
// root (entry paths are bare names) and the directory path below it.
func dirListingJSON(s *store, entries []treeEntry, prefix string) []string {
	items := make([]string, 0, len(entries))
	for _, e := range entries {
		typ, size := "file", 0
		if e.mode == "40000" {
			typ = "dir"
		} else if obj := s.objects.get(e.oid); obj != nil {
			size = len(obj.data)
		}
		entryPath := e.name
		if prefix != "" {
			entryPath = prefix + "/" + e.name
		}
		items = append(items, `{"type":`+jsonString(typ)+
			`,"name":`+jsonString(e.name)+
			`,"path":`+jsonString(entryPath)+
			`,"sha":`+jsonString(e.oid)+
			`,"size":`+itoa(size)+`}`)
	}
	return items
}

// strPtr returns a pointer to the string at key when it is present and a
// string (the handlers distinguish "absent" from "empty" for branch/sha).
func strPtr(input map[string]any, key string) *string {
	if v, ok := input[key].(string); ok {
		return &v
	}
	return nil
}

// decodeBase64Lenient decodes base64 the way Buffer.from(..., 'base64')
// does: whitespace and stray characters are tolerated.
func decodeBase64Lenient(text string) []byte {
	cleaned := strings.Map(func(ch rune) rune {
		if ch == '\n' || ch == '\r' || ch == ' ' || ch == '\t' {
			return -1
		}
		return ch
	}, text)
	if raw, err := base64.StdEncoding.DecodeString(cleaned); err == nil {
		return raw
	}
	if raw, err := base64.RawStdEncoding.DecodeString(cleaned); err == nil {
		return raw
	}
	return nil
}

func (a *app) sendForgeError(w http.ResponseWriter, err error) {
	if fe, ok := err.(*forgeError); ok {
		a.sendJSON(w, fe.status, map[string]any{"message": fe.message})
		return
	}
	a.sendJSON(w, 500, map[string]any{"message": err.Error()})
}

func (a *app) restIssues(w http.ResponseWriter, r *http.Request, repo *repo, rest []string, target *requestTarget, input map[string]any, method string) {
	s := a.store
	if len(rest) == 1 {
		switch method {
		case "POST":
			title := asString(input["title"])
			body := asString(input["body"])
			labelNames := asLabelNames(input["labels"])
			if title == "" {
				a.sendJSON(w, 422, map[string]any{"message": "Title is required"})
				return
			}
			created, err := s.createIssue(repo, title, body, labelNames)
			if err != nil {
				a.sendForgeError(w, err)
				return
			}
			a.sendPrebuilt(w, 201, a.newIssueJSONText(repo, created))
			return
		case "GET":
			state := "open"
			if v, present := target.query["state"]; present {
				state = v
			}
			sortParam := "created"
			if v, present := target.query["sort"]; present {
				sortParam = v
			}
			direction := "desc"
			if v, present := target.query["direction"]; present {
				direction = v
			}
			issues := make([]*issue, 0, len(repo.issues))
			for _, iss := range repo.issues {
				issues = append(issues, iss)
			}
			sort.Slice(issues, func(i, j int) bool { return issues[i].seq < issues[j].seq })
			if state != "all" {
				filtered := issues[:0]
				for _, iss := range issues {
					if iss.state == state {
						filtered = append(filtered, iss)
					}
				}
				issues = filtered
			}
			if sortParam == "created" {
				if direction == "asc" {
					sort.Slice(issues, func(i, j int) bool { return issues[i].seq < issues[j].seq })
				} else {
					sort.Slice(issues, func(i, j int) bool { return issues[i].seq > issues[j].seq })
				}
			}
			page, link := paginate(r, target, issues)
			items := make([]string, 0, len(page))
			for _, iss := range page {
				items = append(items, issueJSONString(repo, iss))
			}
			a.sendListWithLink(w, items, link)
			return
		}
		a.notFound(w)
		return
	}
	number, ok := parseIntJS(rest[1])
	if !ok {
		a.notFound(w)
		return
	}
	if len(rest) == 2 {
		iss := repo.issues[number]
		if iss == nil {
			a.notFound(w)
			return
		}
		switch method {
		case "GET":
			a.sendPrebuilt(w, 200, a.issueJSONText(repo, iss))
		case "PATCH":
			if v, ok := input["title"].(string); ok {
				iss.title = v
			}
			if v, ok := input["body"].(string); ok {
				iss.body = v
			}
			if v, ok := input["state"].(string); ok && (v == "open" || v == "closed") {
				iss.state = v
			}
			s.touchIssue(repo, iss)
			a.sendPrebuilt(w, 200, a.issueJSONText(repo, iss))
		default:
			a.notFound(w)
		}
		return
	}
	if rest[2] == "comments" {
		iss := repo.issues[number]
		if iss == nil {
			a.notFound(w)
			return
		}
		switch method {
		case "POST":
			c := &comment{id: s.nextID, body: asString(input["body"])}
			s.nextID++
			iss.comments = append(iss.comments, c)
			s.touchIssue(repo, iss)
			a.sendPrebuilt(w, 201, commentJSONString(c)+"\n")
		case "GET":
			items := make([]string, 0, len(iss.comments))
			for _, c := range iss.comments {
				items = append(items, commentJSONString(c))
			}
			a.sendPrebuilt(w, 200, joinJSON(items)+"\n")
		default:
			a.notFound(w)
		}
		return
	}
	a.notFound(w)
}

func (a *app) restPulls(w http.ResponseWriter, r *http.Request, repo *repo, rest []string, target *requestTarget, input map[string]any, method string) {
	s := a.store
	if len(rest) == 1 {
		switch method {
		case "POST":
			title := asString(input["title"])
			body := asString(input["body"])
			head := asString(input["head"])
			base := repo.defaultBranch
			if v, ok := input["base"].(string); ok {
				base = v
			}
			if title == "" || head == "" {
				a.sendJSON(w, 422, map[string]any{"message": "Title and head are required"})
				return
			}
			created, err := s.createPull(repo, title, body, head, base)
			if err != nil {
				a.sendForgeError(w, err)
				return
			}
			a.sendPrebuilt(w, 201, pullJSONString(s, repo, created)+"\n")
			return
		case "GET":
			state := "open"
			if v, present := target.query["state"]; present {
				state = v
			}
			pulls := make([]*pull, 0, len(repo.pulls))
			for _, p := range repo.pulls {
				pulls = append(pulls, p)
			}
			if state != "all" {
				filtered := pulls[:0]
				for _, p := range pulls {
					if p.state == state {
						filtered = append(filtered, p)
					}
				}
				pulls = filtered
			}
			sort.Slice(pulls, func(i, j int) bool { return pulls[i].seq < pulls[j].seq })
			page, link := paginate(r, target, pulls)
			items := make([]string, 0, len(page))
			for _, p := range page {
				items = append(items, pullJSONString(s, repo, p))
			}
			a.sendListWithLink(w, items, link)
			return
		}
		a.notFound(w)
		return
	}
	number, ok := parseIntJS(rest[1])
	if !ok {
		a.notFound(w)
		return
	}
	p := repo.pulls[number]
	if len(rest) == 2 {
		if p == nil {
			a.notFound(w)
			return
		}
		if method == "GET" {
			a.sendPrebuilt(w, 200, pullJSONString(s, repo, p)+"\n")
			return
		}
		a.notFound(w)
		return
	}
	if rest[2] == "files" && method == "GET" {
		if p == nil {
			a.notFound(w)
			return
		}
		files := s.pullFiles(repo, p)
		items := make([]string, 0, len(files))
		for _, f := range files {
			items = append(items, `{"filename":`+jsonString(f.filename)+
				`,"status":`+jsonString(f.status)+
				`,"additions":`+itoa(f.additions)+
				`,"deletions":`+itoa(f.deletions)+
				`,"changes":`+itoa(f.changes)+`}`)
		}
		a.sendPrebuilt(w, 200, joinJSON(items)+"\n")
		return
	}
	if rest[2] == "merge" && method == "PUT" {
		if p == nil {
			a.notFound(w)
			return
		}
		sha, err := s.mergePull(repo, p)
		if err != nil {
			a.sendForgeError(w, err)
			return
		}
		a.sendJSON(w, 200, map[string]any{
			"sha":     sha,
			"merged":  true,
			"message": "Merge pull request #" + itoa(p.number) + " from " + p.headRef,
		})
		return
	}
	a.notFound(w)
}
