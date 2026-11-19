package main

// MCP over streamable HTTP: server discovery, initialize, ping, and the six
// forge tools. The tools listing is a frozen constant so it is byte-identical
// between calls; tool results carry the reduced repo/issue/pull shapes the
// MCP surface defines (smaller than the REST shapes).

import (
	"net/http"
	"sort"
	"strings"
)

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const mcpToolsJSON = `{"tools":[` +
	`{"name":"create_repo","description":"Create a new repository for the authenticated user.","inputSchema":{"type":"object","properties":{"name":{"type":"string","description":"The repository name."},"description":{"type":"string","description":"A short description of the repository."},"private":{"type":"boolean","description":"Whether the repository is private."}},"required":["name"]}},` +
	`{"name":"get_repo","description":"Get a repository by owner and name.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The repository owner login."},"repo":{"type":"string","description":"The repository name."}},"required":["owner","repo"]}},` +
	`{"name":"create_issue","description":"Create an issue in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The repository owner login."},"repo":{"type":"string","description":"The repository name."},"title":{"type":"string","description":"The issue title."},"body":{"type":"string","description":"The issue body."}},"required":["owner","repo","title"]}},` +
	`{"name":"list_issues","description":"List issues in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The repository owner login."},"repo":{"type":"string","description":"The repository name."},"state":{"type":"string","description":"Issue state filter: open, closed or all."}},"required":["owner","repo"]}},` +
	`{"name":"create_pull_request","description":"Create a pull request from a head branch into a base branch.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The repository owner login."},"repo":{"type":"string","description":"The repository name."},"title":{"type":"string","description":"The pull request title."},"body":{"type":"string","description":"The pull request body."},"head":{"type":"string","description":"The head branch name."},"base":{"type":"string","description":"The base branch name."}},"required":["owner","repo","title","head","base"]}},` +
	`{"name":"merge_pull_request","description":"Merge a pull request.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The repository owner login."},"repo":{"type":"string","description":"The repository name."},"pull_number":{"type":"integer","description":"The pull request number."}},"required":["owner","repo","pull_number"]}}` +
	`]}`

var mcpToolRequired = map[string][]string{
	"create_repo":         {"name"},
	"get_repo":            {"owner", "repo"},
	"create_issue":        {"owner", "repo", "title"},
	"list_issues":         {"owner", "repo"},
	"create_pull_request": {"owner", "repo", "title", "head", "base"},
	"merge_pull_request":  {"owner", "repo", "pull_number"},
}

// mcpSend writes a JSON-RPC body (object -> JSON + newline) without the
// REST layer's request-id header, like the MCP surface does.
func mcpSend(w http.ResponseWriter, status int, value any) {
	text := marshalJSON(value) + "\n"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", itoa(len(text)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(text))
}

func rpcResult(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcError(id any, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

func (a *app) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "DELETE" {
		w.WriteHeader(204)
		return
	}
	if r.Method != "POST" {
		mcpSend(w, 405, map[string]any{"message": "Method Not Allowed"})
		return
	}
	raw, err := readRequestBody(r)
	if err != nil {
		mcpSend(w, 500, map[string]any{"message": err.Error()})
		return
	}
	parsed, ok := parseJSONBody(raw)
	if !ok {
		mcpSend(w, 400, rpcError(nil, -32700, "Parse error"))
		return
	}
	record := asObject(parsed)
	if record == nil {
		mcpSend(w, 400, rpcError(nil, -32600, "Invalid Request"))
		return
	}
	id, hasID := record["id"]
	method := asString(record["method"])
	params := asObject(record["params"])
	if params == nil {
		params = map[string]any{}
	}

	// Notifications carry no id and get no JSON-RPC answer.
	if !hasID {
		if !a.authed(r) {
			mcpSend(w, 401, rpcError(nil, -32001, "Unauthorized"))
			return
		}
		w.WriteHeader(202)
		return
	}
	if !a.authed(r) {
		mcpSend(w, 401, rpcError(id, -32001, "Unauthorized"))
		return
	}

	a.store.mu.Lock()
	defer a.store.mu.Unlock()

	switch method {
	case "server/discover":
		serverInfo := map[string]any{"name": "git-forge", "version": "1.0.0"}
		versions := make([]any, len(mcpSupportedVersions))
		for i, v := range mcpSupportedVersions {
			versions[i] = v
		}
		mcpSend(w, 200, rpcResult(id, map[string]any{
			"supportedVersions": versions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        serverInfo,
			"instructions":      "A GitHub-compatible git forge: repositories, issues, pull requests, git smart HTTP.",
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": serverInfo},
		}))
	case "initialize":
		requested := asString(params["protocolVersion"])
		negotiated := "2026-07-28"
		for _, v := range mcpSupportedVersions {
			if v == requested {
				negotiated = requested
				break
			}
		}
		mcpSend(w, 200, rpcResult(id, map[string]any{
			"protocolVersion": negotiated,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "git-forge", "version": "1.0.0"},
		}))
	case "ping":
		mcpSend(w, 200, rpcResult(id, map[string]any{}))
	case "tools/list":
		tools, _ := parseJSONBody([]byte(mcpToolsJSON))
		mcpSend(w, 200, rpcResult(id, tools))
	case "tools/call":
		a.mcpToolsCall(w, id, params)
	default:
		mcpSend(w, 200, rpcError(id, -32601, "Method not found: "+method))
	}
}

// mcpErrorResult is the tools/call failure shape: the message as a text
// content block and as structured content, flagged as an error.
func mcpErrorResult(id any, message string) map[string]any {
	return rpcResult(id, map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": marshalJSON(map[string]any{"error": message})}},
		"structuredContent": map[string]any{"error": message},
		"isError":           true,
	})
}

func (a *app) mcpToolsCall(w http.ResponseWriter, id any, params map[string]any) {
	toolName := asString(params["name"])
	args := asObject(params["arguments"])
	if args == nil {
		args = map[string]any{}
	}
	required, known := mcpToolRequired[toolName]
	if !known {
		mcpSend(w, 200, rpcError(id, -32601, "Method not found: "+toolName))
		return
	}
	var missing []string
	for _, key := range required {
		if _, present := args[key]; !present {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		mcpSend(w, 200, mcpErrorResult(id, "Missing required arguments: "+strings.Join(missing, ", ")))
		return
	}
	value, errMessage := a.runMCPTool(toolName, args)
	if errMessage != "" {
		mcpSend(w, 200, mcpErrorResult(id, errMessage))
		return
	}
	mcpSend(w, 200, rpcResult(id, map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": marshalJSON(value)}},
		"structuredContent": value,
		"isError":           false,
	}))
}

// MCP value shapes (reduced compared to REST).

func mcpRepoValue(r *repo) map[string]any {
	return map[string]any{
		"id":             r.dbID,
		"name":           r.name,
		"full_name":      viewerLogin + "/" + r.name,
		"owner":          map[string]any{"login": viewerLogin},
		"private":        r.private,
		"description":    r.description,
		"default_branch": r.defaultBranch,
		"has_issues":     true,
	}
}

func mcpIssueValue(r *repo, iss *issue) map[string]any {
	labels := make([]any, 0, len(iss.labels))
	for _, name := range iss.labels {
		color := "ededed"
		if l := r.labels[name]; l != nil {
			color = l.color
		}
		labels = append(labels, map[string]any{"name": name, "color": color})
	}
	return map[string]any{
		"id":     iss.id,
		"number": iss.number,
		"title":  iss.title,
		"body":   iss.body,
		"state":  iss.state,
		"labels": labels,
	}
}

func mcpPullValue(p *pull) map[string]any {
	var mergedAt, mergeSha any
	if p.mergedAt != nil {
		mergedAt = *p.mergedAt
	}
	if p.mergeSha != nil {
		mergeSha = *p.mergeSha
	}
	return map[string]any{
		"id":               p.id,
		"number":           p.number,
		"state":            p.state,
		"title":            p.title,
		"body":             p.body,
		"head":             map[string]any{"ref": p.headRef, "sha": p.headSha},
		"base":             map[string]any{"ref": p.baseRef, "sha": p.baseSha},
		"merged":           p.merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSha,
	}
}

// runMCPTool executes one tool; a non-empty second return is the error
// message the tool result carries.
func (a *app) runMCPTool(name string, args map[string]any) (any, string) {
	s := a.store
	switch name {
	case "create_repo":
		repoName, isStr := args["name"].(string)
		if !isStr || repoName == "" {
			return nil, "create_repo requires a name"
		}
		created := s.createRepo(repoName, asBool(args["private"]), asString(args["description"]))
		if created == nil {
			return nil, "Repository already exists"
		}
		return mcpRepoValue(created), ""
	case "get_repo":
		owner, okOwner := args["owner"].(string)
		repoName, okRepo := args["repo"].(string)
		if !okOwner || !okRepo {
			return nil, "get_repo requires owner and repo"
		}
		r := s.getRepo(owner, repoName)
		if r == nil {
			return nil, "Repository not found"
		}
		return mcpRepoValue(r), ""
	case "create_issue":
		owner, okOwner := args["owner"].(string)
		repoName, okRepo := args["repo"].(string)
		title, okTitle := args["title"].(string)
		if !okOwner || !okRepo || !okTitle || title == "" {
			return nil, "create_issue requires owner, repo and title"
		}
		r := s.getRepo(owner, repoName)
		if r == nil {
			return nil, "Repository not found"
		}
		created, err := s.createIssue(r, title, asString(args["body"]), nil)
		if err != nil {
			return nil, err.Error()
		}
		return mcpIssueValue(r, created), ""
	case "list_issues":
		owner, okOwner := args["owner"].(string)
		repoName, okRepo := args["repo"].(string)
		if !okOwner || !okRepo {
			return nil, "list_issues requires owner and repo"
		}
		r := s.getRepo(owner, repoName)
		if r == nil {
			return nil, "Repository not found"
		}
		state := "open"
		if v, isStr := args["state"].(string); isStr {
			state = v
		}
		issues := make([]*issue, 0, len(r.issues))
		for _, iss := range r.issues {
			if state == "all" || iss.state == state {
				issues = append(issues, iss)
			}
		}
		sort.Slice(issues, func(i, j int) bool { return issues[i].seq < issues[j].seq })
		values := make([]any, 0, len(issues))
		for _, iss := range issues {
			values = append(values, mcpIssueValue(r, iss))
		}
		return map[string]any{"issues": values}, ""
	case "create_pull_request":
		owner, okOwner := args["owner"].(string)
		repoName, okRepo := args["repo"].(string)
		title, okTitle := args["title"].(string)
		head, okHead := args["head"].(string)
		base, okBase := args["base"].(string)
		if !okOwner || !okRepo || !okTitle || !okHead || !okBase {
			return nil, "create_pull_request requires owner, repo, title, head and base"
		}
		r := s.getRepo(owner, repoName)
		if r == nil {
			return nil, "Repository not found"
		}
		created, err := s.createPull(r, title, asString(args["body"]), head, base)
		if err != nil {
			return nil, err.Error()
		}
		return mcpPullValue(created), ""
	case "merge_pull_request":
		owner, okOwner := args["owner"].(string)
		repoName, okRepo := args["repo"].(string)
		numberFloat, okNumber := args["pull_number"].(float64)
		if !okOwner || !okRepo || !okNumber {
			return nil, "merge_pull_request requires owner, repo and pull_number"
		}
		r := s.getRepo(owner, repoName)
		if r == nil {
			return nil, "Repository not found"
		}
		var p *pull
		if numberFloat == float64(int(numberFloat)) {
			p = r.pulls[int(numberFloat)]
		}
		if p == nil {
			return nil, "Pull request not found"
		}
		sha, err := s.mergePull(r, p)
		if err != nil {
			return nil, err.Error()
		}
		return map[string]any{
			"sha":     sha,
			"merged":  true,
			"message": "Merge pull request #" + itoa(p.number),
		}, ""
	}
	return nil, "Unknown tool: " + name
}
