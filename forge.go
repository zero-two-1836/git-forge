package main

// Repository behaviour on top of the object store: branch/ref resolution,
// tree path surgery for the contents API, commit listing, and the issue /
// pull-request model including the three-way mergeability check and the
// squash merge. Error strings match the forge's REST surface word for word;
// the suite matches several of them exactly.

import (
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Refs and trees
// ---------------------------------------------------------------------------

// resolveRef turns a branch name or a raw object id into the commit oid it
// points at ("" when it does not resolve): a branch name wins first, then a
// 40-hex id that names a commit directly or peels to one.
func (s *store) resolveRef(r *repo, ref string) string {
	if oid := r.refs["refs/heads/"+ref]; oid != "" {
		return oid
	}
	if isHex40(ref) {
		if obj := s.objects.get(ref); obj != nil && obj.typ == "commit" {
			return ref
		}
		return s.objects.peelToCommit(ref)

	}
	return ""
}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *store) commitTreeOf(oid string) string {
	commit := s.objects.peelToCommit(oid)
	if commit == "" {
		return ""
	}
	info := s.objects.parseCommit(commit)
	if info == nil {
		return ""
	}
	return info.tree
}

// flattenTree expands a tree into a path -> entry map (recursive).
func (s *store) flattenTree(treeOid string) map[string]flatEntry {
	out := make(map[string]flatEntry)
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		for _, e := range s.objects.parseTree(oid) {
			path := e.name
			if prefix != "" {
				path = prefix + "/" + e.name
			}
			if e.typ == "tree" {
				walk(e.oid, path)
			} else if obj := s.objects.get(e.oid); obj != nil {
				out[path] = flatEntry{oid: e.oid, data: obj.data}
			}
		}
	}
	if treeOid != "" {
		walk(treeOid, "")
	}
	return out
}

// resolvePath walks a slash path from a tree and returns the entry it names.
func (s *store) resolvePath(treeOid, path string) *treeEntry {
	parts := strings.Split(path, "/")
	current := treeOid
	for i, part := range parts {
		var found *treeEntry
		for _, e := range s.objects.parseTree(current) {
			if e.name == part {
				entry := e
				found = &entry
				break
			}
		}
		if found == nil {
			return nil
		}
		if i == len(parts)-1 {
			return found
		}
		if found.typ != "tree" {
			return nil
		}
		current = found.oid
	}
	return nil
}

// listDir returns the immediate entries of the directory at path ("" = root).
func (s *store) listDir(treeOid, path string) []treeEntry {
	if path == "" {
		return s.objects.parseTree(treeOid)
	}
	entry := s.resolvePath(treeOid, path)
	if entry == nil || entry.typ != "tree" {
		return nil
	}
	return s.objects.parseTree(entry.oid)
}

// setPath returns a new tree with the blob at path replaced by (mode, oid),
// or removed when oid is "". Intermediate trees are created as needed and
// emptied subtrees are pruned (except at the root).
func (s *store) setPath(treeOid string, parts []string, mode, oid string) string {
	entries := []treeEntry{}
	if treeOid != "" {
		entries = append(entries, s.objects.parseTree(treeOid)...)
	}
	name := parts[0]
	idx := -1
	for i, e := range entries {
		if e.name == name {
			idx = i
			break
		}
	}
	replace := func(e *treeEntry) {
		if idx >= 0 {
			if e == nil {
				entries = append(entries[:idx], entries[idx+1:]...)
			} else {
				entries[idx] = *e
			}
		} else if e != nil {
			entries = append(entries, *e)
		}
	}
	if len(parts) == 1 {
		if oid == "" {
			replace(nil)
		} else {
			replace(&treeEntry{mode: mode, name: name, typ: "blob", oid: oid})
		}
	} else {
		var subOid string
		if idx >= 0 && entries[idx].typ == "tree" {
			subOid = entries[idx].oid
		}
		newSub := s.setPath(subOid, parts[1:], mode, oid)
		empty := len(s.objects.parseTree(newSub)) == 0
		if empty {
			replace(nil)
		} else {
			replace(&treeEntry{mode: "40000", name: name, typ: "tree", oid: newSub})
		}
	}
	return s.objects.buildTree(entries)
}

// listCommits walks history from a ref (inclusive), parents in order, each
// commit once — the traversal the REST commits endpoint pages over.
func (s *store) listCommits(r *repo, ref string) []string {
	tip := s.resolveRef(r, ref)
	if tip == "" {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	stack := []string{tip}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[current] {
			continue
		}
		seen[current] = true
		info := s.objects.parseCommit(current)
		if info == nil {
			continue
		}
		out = append(out, current)
		for i := len(info.parents) - 1; i >= 0; i-- {
			stack = append(stack, info.parents[i])
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Contents API backing
// ---------------------------------------------------------------------------

type forgeError struct {
	status  int
	message string
}

func (e *forgeError) Error() string { return e.message }

func fail(status int, message string) *forgeError {
	return &forgeError{status: status, message: message}
}

// commitFile creates or updates one file on a branch (the default branch
// when branch is nil), honouring the sha preconditions of the contents API.
// The blob is stored before any validation, exactly like the house store.
// Returns whether the file was created, the new commit oid and the blob oid.
func (s *store) commitFile(r *repo, branch *string, path string, content []byte, message string, expectedSha *string) (bool, string, string, error) {
	branchName := r.defaultBranch
	if branch != nil {
		branchName = *branch
	}
	refName := "refs/heads/" + branchName
	tip := r.refs[refName]
	blobSha := s.objects.addObject("blob", content)
	var baseTree string
	var parents []string
	fileExisted := false
	isNew := false
	if tip == "" {
		if branch != nil {
			return false, "", "", fail(404, "Branch not found")
		}
		baseTree = s.objects.buildTree(nil)
		isNew = true
	} else {
		info := s.objects.parseCommit(tip)
		if info != nil {
			baseTree = info.tree
		}
		parents = []string{tip}
		existing := s.resolvePath(baseTree, path)
		if existing != nil && existing.typ == "blob" {
			fileExisted = true
			if expectedSha == nil {
				return false, "", "", fail(422, "sha is required to update a file")
			}
			if !strings.EqualFold(existing.oid, *expectedSha) {
				return false, "", "", fail(409, "sha does not match the current file")
			}
		} else if existing != nil {
			return false, "", "", fail(422, "path is a directory")
		} else if expectedSha != nil {
			return false, "", "", fail(422, "sha was supplied for a file that does not exist")
		}
	}
	newTree := s.setPath(baseTree, strings.Split(path, "/"), "100644", blobSha)
	commitSha := s.objects.buildCommit(newTree, parents, message)
	r.refs[refName] = commitSha
	return isNew || !fileExisted, commitSha, blobSha, nil
}

// deleteFile removes one file from a branch. Returns the new commit oid.
func (s *store) deleteFile(r *repo, branch *string, path, message string, expectedSha *string) (string, error) {
	branchName := r.defaultBranch
	if branch != nil {
		branchName = *branch
	}
	refName := "refs/heads/" + branchName
	tip := r.refs[refName]
	if tip == "" {
		return "", fail(404, "Not Found")
	}
	var tree string
	if info := s.objects.parseCommit(tip); info != nil {
		tree = info.tree
	}
	existing := s.resolvePath(tree, path)
	if existing == nil || existing.typ != "blob" {
		return "", fail(404, "Not Found")
	}
	if expectedSha != nil && !strings.EqualFold(existing.oid, *expectedSha) {
		return "", fail(409, "sha does not match the current file")
	}
	newTree := s.setPath(tree, strings.Split(path, "/"), "", "")
	commitSha := s.objects.buildCommit(newTree, []string{tip}, message)
	r.refs[refName] = commitSha
	return commitSha, nil
}

// ---------------------------------------------------------------------------
// Issues
// ---------------------------------------------------------------------------

func (s *store) createIssue(r *repo, title, body string, labelNames []string) (*issue, error) {
	for _, name := range labelNames {
		if r.labels[name] == nil {
			return nil, fail(422, "Label '"+name+"' does not exist")
		}
	}
	iss := &issue{
		id:      s.nextID,
		number:  r.nextNumber,
		title:   title,
		body:    body,
		state:   "open",
		labels:  append([]string{}, labelNames...),
		seq:     r.nextNumber,
		version: 0,
	}
	s.nextID++
	r.nextNumber++
	r.issues[iss.number] = iss
	return iss, nil
}

func (s *store) getIssue(r *repo, number int) *issue {
	return r.issues[number]
}

func (s *store) listIssues(r *repo, state string) []*issue {
	out := []*issue{}
	for _, iss := range r.issues {
		if iss.state == state {
			out = append(out, iss)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq > out[j].seq })
	return out
}

func (s *store) addComment(r *repo, iss *issue, body string) *comment {
	c := &comment{id: s.nextID, body: body}
	s.nextID++
	iss.comments = append(iss.comments, c)
	s.touchIssue(r, iss)
	return c
}

// touchIssue invalidates the cached REST rendering of one issue: the cache
// is keyed by version, so bumping it is the whole invalidation (stale
// entries are simply never hit again).
func (s *store) touchIssue(r *repo, iss *issue) {
	iss.version++
}

// ---------------------------------------------------------------------------
// Pull requests
// ---------------------------------------------------------------------------

func (s *store) createPull(r *repo, title, body, headRef, baseRef string) (*pull, error) {
	headSha := r.refs["refs/heads/"+headRef]
	baseSha := r.refs["refs/heads/"+baseRef]
	if headSha == "" {
		return nil, fail(422, "Head branch '"+headRef+"' does not exist")
	}
	if baseSha == "" {
		return nil, fail(422, "Base branch '"+baseRef+"' does not exist")
	}
	for _, p := range r.pulls {
		if p.state == "open" && p.headRef == headRef && p.baseRef == baseRef {
			return nil, fail(422, "A pull request is already open for this head and base")
		}
	}
	p := &pull{
		id:          s.nextID,
		number:      r.nextNumber,
		title:       title,
		body:        body,
		state:       "open",
		headRef:     headRef,
		baseRef:     baseRef,
		headSha:     headSha,
		baseSha:     baseSha,
		createdFlat: s.flattenTree(s.commitTreeOf(baseSha)),
		seq:         r.nextNumber,
	}
	s.nextID++
	r.nextNumber++
	r.pulls[p.number] = p
	return p, nil
}

func (s *store) getPull(r *repo, number int) *pull {
	return r.pulls[number]
}

func (s *store) listPulls(r *repo, state string) []*pull {
	out := []*pull{}
	for _, p := range r.pulls {
		if p.state == state {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

type pullFile struct {
	filename  string
	status    string // added | removed | modified
	additions int
	deletions int
	changes   int
}

// countLines splits file bytes into lines, dropping the single trailing
// empty element a final newline produces (and returning none for empty
// files) — the line accounting the files endpoint reports.
func countLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// pullFiles diffs the base tree as it was when the pull was created against
// the pull's head tree: added files count all their lines, removed files
// count all of theirs, modified files get a line-multiset diff (with a
// floor of one addition when only bytes changed).
func (s *store) pullFiles(r *repo, p *pull) []pullFile {
	headFlat := s.flattenTree(s.commitTreeOf(p.headSha))
	paths := make(map[string]bool)
	for path := range p.createdFlat {
		paths[path] = true
	}
	for path := range headFlat {
		paths[path] = true
	}
	sorted := make([]string, 0, len(paths))
	for path := range paths {
		sorted = append(sorted, path)
	}
	sort.Strings(sorted)
	var out []pullFile
	for _, path := range sorted {
		before, hasBefore := p.createdFlat[path]
		after, hasAfter := headFlat[path]
		if hasBefore && hasAfter && before.oid == after.oid {
			continue
		}
		switch {
		case !hasBefore:
			lines := countLines(after.data)
			out = append(out, pullFile{filename: path, status: "added", additions: len(lines), deletions: 0, changes: len(lines)})
		case !hasAfter:
			lines := countLines(before.data)
			out = append(out, pullFile{filename: path, status: "removed", additions: 0, deletions: len(lines), changes: len(lines)})
		default:
			oldLines := countLines(before.data)
			newLines := countLines(after.data)
			remaining := make(map[string]int, len(oldLines))
			for _, line := range oldLines {
				remaining[line]++
			}
			additions := 0
			for _, line := range newLines {
				if remaining[line] > 0 {
					remaining[line]--
				} else {
					additions++
				}
			}
			deletions := 0
			for _, n := range remaining {
				deletions += n
			}
			if additions == 0 && deletions == 0 {
				additions = 1
			}
			out = append(out, pullFile{filename: path, status: "modified", additions: additions, deletions: deletions, changes: additions + deletions})
		}
	}
	return out
}

// pullMergeable runs the three-way check: for every path, if both the head
// and the current base moved away from the created base in different ways,
// the pull conflicts.
func (s *store) pullMergeable(r *repo, p *pull) bool {
	if p.state != "open" {
		return false
	}
	currentBaseSha := r.refs["refs/heads/"+p.baseRef]
	if currentBaseSha == "" {
		return false
	}
	currentFlat := s.flattenTree(s.commitTreeOf(currentBaseSha))
	headFlat := s.flattenTree(s.commitTreeOf(p.headSha))
	paths := make(map[string]bool)
	for path := range p.createdFlat {
		paths[path] = true
	}
	for path := range currentFlat {
		paths[path] = true
	}
	for path := range headFlat {
		paths[path] = true
	}
	oidOf := func(m map[string]flatEntry, path string) string {
		if e, ok := m[path]; ok {
			return e.oid
		}
		return ""
	}
	for path := range paths {
		from := oidOf(p.createdFlat, path)
		head := oidOf(headFlat, path)
		current := oidOf(currentFlat, path)
		if head != from && current != from && head != current {
			return false
		}
	}
	return true
}

// mergePull squash-merges the pull: one new commit on the base tip carrying
// the head's tree. Not-open pulls are a 404, conflicted ones a 405.
func (s *store) mergePull(r *repo, p *pull) (string, error) {
	if p.state != "open" {
		return "", fail(404, "Pull request is not open")
	}
	if !s.pullMergeable(r, p) {
		return "", fail(405, "Pull request is conflicted")
	}
	baseTip := r.refs["refs/heads/"+p.baseRef]
	if baseTip == "" {
		return "", fail(404, "Base branch not found")
	}
	headTree := s.commitTreeOf(p.headSha)
	message := "Merge pull request #" + itoa(p.number) + " from " + p.headRef
	commitSha := s.objects.buildCommit(headTree, []string{baseTip}, message)
	r.refs["refs/heads/"+p.baseRef] = commitSha
	p.state = "closed"
	p.merged = true
	mergedAt := fixedISO
	p.mergedAt = &mergedAt
	p.mergeSha = &commitSha
	return commitSha, nil
}
