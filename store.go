package main

// In-memory git object store: SHA-1 addressed objects, packfile read/write
// (plain objects plus OFS/REF deltas), pkt-line framing, tree/commit/tag
// parsing and the reachability walks the fetch planner is built on.
//
// The store mirrors the forge's house rules: one global object map shared by
// every repository, timestamps pinned to a fixed epoch, and ids drawn from a
// single counter that starts at 10000.

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

const (
	fixedEpoch = 1735689600
	fixedISO   = "2025-01-01T00:00:00Z"
	agentName  = "git-forge/1.0.0"

	viewerLogin = "adaiversen"
	viewerID    = 1000
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"

	defaultBranch = "main"
	zeroOid       = "0000000000000000000000000000000000000000"
)

// ---------------------------------------------------------------------------
// Objects
// ---------------------------------------------------------------------------

type gitObject struct {
	typ  string // commit | tree | blob | tag
	data []byte
}

type commitInfo struct {
	tree    string
	parents []string
	message string
}

type treeEntry struct {
	mode string
	name string
	typ  string // blob | tree
	oid  string
}

// objectStore owns every git object plus the parse caches. Object map
// access is serialized by the embedding store's mutex (readers hold it
// shared, writers exclusively); the parse caches carry their own lock so
// concurrent readers can fill them without the store's write lock.
type objectStore struct {
	objects     map[string]*gitObject
	cacheMu     sync.RWMutex
	commitCache map[string]*commitInfo
	treeCache   map[string][]treeEntry
}

func newObjectStore() *objectStore {
	return &objectStore{
		objects:     make(map[string]*gitObject),
		commitCache: make(map[string]*commitInfo),
		treeCache:   make(map[string][]treeEntry),
	}
}

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

func (s *objectStore) hashObject(typ string, data []byte) string {
	header := fmt.Sprintf("%s %d\x00", typ, len(data))
	buf := make([]byte, 0, len(header)+len(data))
	buf = append(buf, header...)
	buf = append(buf, data...)
	return sha1Hex(buf)
}

func (s *objectStore) addObject(typ string, data []byte) string {
	oid := s.hashObject(typ, data)
	if _, ok := s.objects[oid]; !ok {
		stored := make([]byte, len(data))
		copy(stored, data)
		s.objects[oid] = &gitObject{typ: typ, data: stored}
	}
	return oid
}

func (s *objectStore) get(oid string) *gitObject {
	return s.objects[oid]
}

// parseCommit decodes a commit object's headers. Results are cached; commit
// objects are immutable so the cache never needs invalidation.
func (s *objectStore) parseCommit(oid string) *commitInfo {
	s.cacheMu.RLock()
	cached := s.commitCache[oid]
	s.cacheMu.RUnlock()
	if cached != nil {
		return cached
	}
	obj := s.objects[oid]
	if obj == nil || obj.typ != "commit" {
		return nil
	}
	text := string(obj.data)
	head, message := text, ""
	if idx := strings.Index(text, "\n\n"); idx >= 0 {
		head, message = text[:idx], text[idx+2:]
	}
	info := &commitInfo{message: message}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			info.tree = line[5:]
		case strings.HasPrefix(line, "parent "):
			info.parents = append(info.parents, line[7:])
		}
	}
	s.cacheMu.Lock()
	s.commitCache[oid] = info
	s.cacheMu.Unlock()
	return info
}

// parseTree decodes a tree object's binary entries, cached like commits.
func (s *objectStore) parseTree(oid string) []treeEntry {
	s.cacheMu.RLock()
	cached := s.treeCache[oid]
	s.cacheMu.RUnlock()
	if cached != nil {
		return cached
	}
	obj := s.objects[oid]
	if obj == nil || obj.typ != "tree" {
		return nil
	}
	data := obj.data
	var entries []treeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			break
		}
		mode := string(data[:sp])
		rest := data[sp+1:]
		nul := bytes.IndexByte(rest, 0)
		if nul < 0 || nul+21 > len(rest) {
			break
		}
		name := string(rest[:nul])
		oid := hex.EncodeToString(rest[nul+1 : nul+21])
		typ := "blob"
		if mode == "40000" {
			typ = "tree"
		}
		entries = append(entries, treeEntry{mode: mode, name: name, typ: typ, oid: oid})
		data = rest[nul+21:]
	}
	s.cacheMu.Lock()
	s.treeCache[oid] = entries
	s.cacheMu.Unlock()
	return entries
}

// buildTree stores a tree from its entries, sorted the way git sorts them:
// by name, with directories compared as if they had a trailing slash.
func (s *objectStore) buildTree(entries []treeEntry) string {
	sorted := make([]treeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return treeSortKey(sorted[i]) < treeSortKey(sorted[j])
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.mode)
		buf.WriteByte(' ')
		buf.WriteString(e.name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.oid)
		buf.Write(raw)
	}
	return s.addObject("tree", buf.Bytes())
}

func treeSortKey(e treeEntry) string {
	if e.typ == "tree" {
		return e.name + "/"
	}
	return e.name
}

// buildCommit stores a commit with the forge's fixed author/committer line.
func (s *objectStore) buildCommit(tree string, parents []string, message string) string {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	line := fmt.Sprintf("%s <%s> %d +0000", viewerName, viewerEmail, fixedEpoch)
	b.WriteString("author " + line + "\n")
	b.WriteString("committer " + line + "\n")
	b.WriteString("\n" + message)
	return s.addObject("commit", []byte(b.String()))
}

// peelToCommit follows tag objects until it reaches a commit (at most 10
// hops). Returns "" when the chain does not end at a commit.
func (s *objectStore) peelToCommit(oid string) string {
	current := oid
	for i := 0; i < 10; i++ {
		obj := s.objects[current]
		if obj == nil {
			return ""
		}
		if obj.typ == "commit" {
			return current
		}
		if obj.typ != "tag" {
			return ""
		}
		target := tagTarget(string(obj.data))
		if target == "" {
			return ""
		}
		current = target
	}
	return ""
}

func tagTarget(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "object ") {
			return line[7:]
		}
	}
	return ""
}

// collectReachable adds oid and everything it references to acc. When stop
// is non-nil, a commit listed in stop is added but its parents are not
// followed (the shallow-boundary rule for fetch "have" computation).
func (s *objectStore) collectReachable(oid string, acc map[string]bool, stop map[string]bool) {
	obj := s.objects[oid]
	if obj == nil || acc[oid] {
		return
	}
	acc[oid] = true
	if stop != nil && stop[oid] && obj.typ == "commit" {
		return
	}
	switch obj.typ {
	case "commit":
		info := s.parseCommit(oid)
		if info == nil {
			return
		}
		s.collectReachable(info.tree, acc, stop)
		for _, p := range info.parents {
			s.collectReachable(p, acc, stop)
		}
	case "tree":
		for _, e := range s.parseTree(oid) {
			s.collectReachable(e.oid, acc, stop)
		}
	case "tag":
		if target := tagTarget(string(obj.data)); target != "" {
			s.collectReachable(target, acc, stop)
		}
	}
}

// isAncestor reports whether ancestor is reachable from descendant by
// walking parent links.
func (s *objectStore) isAncestor(ancestor, descendant string) bool {
	seen := make(map[string]bool)
	stack := []string{descendant}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if current == ancestor {
			return true
		}
		if seen[current] {
			continue
		}
		seen[current] = true
		if info := s.parseCommit(current); info != nil {
			stack = append(stack, info.parents...)
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Packfiles
// ---------------------------------------------------------------------------

var packTypeNames = map[int]string{
	1: "commit", 2: "tree", 3: "blob", 4: "tag", 6: "ofs-delta", 7: "ref-delta",
}

var packTypeCodes = map[string]int{
	"commit": 1, "tree": 2, "blob": 3, "tag": 4,
}

type packEntry struct {
	typ     string
	data    []byte
	baseOid string // ref-delta
	baseIdx int    // ofs-delta: index of the base entry, -1 when external
	offset  int
}

// sliceReader is a byte-counting reader over a byte slice. Feeding it to
// compress/zlib (it implements io.ByteReader) makes the zlib decoder consume
// exactly the bytes of one pack entry, so the next entry's offset is known.
type sliceReader struct {
	b   []byte
	pos int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

func (r *sliceReader) ReadByte() (byte, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	c := r.b[r.pos]
	r.pos++
	return c, nil
}

func inflateEntry(pack []byte, offset int, expectedSize int) ([]byte, int, error) {
	r := &sliceReader{b: pack, pos: offset}
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, 0, err
	}
	defer zr.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, zr); err != nil {
		return nil, 0, err
	}
	if buf.Len() != expectedSize {
		return nil, 0, fmt.Errorf("inflated size %d, want %d", buf.Len(), expectedSize)
	}
	return buf.Bytes(), r.pos, nil
}

// readPack parses a packfile and returns the fully resolved objects keyed by
// oid. lookup resolves ref-delta bases that live outside the pack (thin
// packs); it may be nil when the pack must be self-contained.
func readPack(pack []byte, lookup func(oid string) *gitObject) (map[string]*gitObject, error) {
	if len(pack) < 12+20 || string(pack[:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	offset := 12
	var entries []*packEntry
	byOffset := make(map[int]int)
	for i := 0; i < count; i++ {
		if offset >= len(pack) {
			return nil, errors.New("pack truncated in entry header")
		}
		entryOffset := offset
		c := pack[offset]
		offset++
		typ := packTypeNames[int(c>>4)&7]
		if typ == "" {
			return nil, fmt.Errorf("unknown pack object type %d", (c>>4)&7)
		}
		size := int(c & 0x0f)
		shift := uint(4)
		for c&0x80 != 0 {
			if offset >= len(pack) {
				return nil, errors.New("pack truncated in entry size")
			}
			c = pack[offset]
			offset++
			size |= int(c&0x7f) << shift
			shift += 7
		}
		entry := &packEntry{typ: typ, baseIdx: -1, offset: entryOffset}
		switch typ {
		case "ofs-delta":
			if offset >= len(pack) {
				return nil, errors.New("pack truncated in ofs-delta base")
			}
			c = pack[offset]
			offset++
			baseOffset := int(c & 0x7f)
			for c&0x80 != 0 {
				if offset >= len(pack) {
					return nil, errors.New("pack truncated in ofs-delta base")
				}
				c = pack[offset]
				offset++
				baseOffset = ((baseOffset + 1) << 7) | int(c&0x7f)
			}
			basePos := entryOffset - baseOffset
			idx, ok := byOffset[basePos]
			if !ok {
				return nil, errors.New("ofs-delta base not in pack")
			}
			entry.baseIdx = idx
		case "ref-delta":
			if offset+20 > len(pack) {
				return nil, errors.New("pack truncated in ref-delta base")
			}
			entry.baseOid = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
		}
		data, end, err := inflateEntry(pack, offset, size)
		if err != nil {
			return nil, err
		}
		entry.data = data
		offset = end
		byOffset[entryOffset] = len(entries)
		entries = append(entries, entry)
	}

	// Resolve deltas in passes until every entry has a concrete type+data.
	// oidOf records each resolved entry's object id so ref-delta bases that
	// arrived in the same pack are found by map lookup, not by re-hashing.
	resolved := make([]*gitObject, len(entries))
	oidOf := make([]string, len(entries))
	byOid := make(map[string]int, len(entries))
	pending := len(entries)
	for guard := 0; pending > 0 && guard <= len(entries)+1; guard++ {
		for i, entry := range entries {
			if resolved[i] != nil {
				continue
			}
			var obj *gitObject
			switch entry.typ {
			case "commit", "tree", "blob", "tag":
				obj = &gitObject{typ: entry.typ, data: entry.data}
			case "ofs-delta":
				base := resolved[entry.baseIdx]
				if base == nil {
					continue
				}
				data, err := applyDelta(base.data, entry.data)
				if err != nil {
					return nil, err
				}
				obj = &gitObject{typ: base.typ, data: data}
			case "ref-delta":
				var base *gitObject
				if j, ok := byOid[entry.baseOid]; ok {
					base = resolved[j]
				} else if lookup != nil {
					base = lookup(entry.baseOid)
				}
				if base == nil {
					continue
				}
				data, err := applyDelta(base.data, entry.data)
				if err != nil {
					return nil, err
				}
				obj = &gitObject{typ: base.typ, data: data}
			}
			resolved[i] = obj
			oid := hashObjectRaw(obj.typ, obj.data)
			oidOf[i] = oid
			byOid[oid] = i
			pending--
		}
	}
	if pending > 0 {
		return nil, errors.New("pack has unresolved deltas")
	}
	out := make(map[string]*gitObject, len(entries))
	for i, obj := range resolved {
		out[oidOf[i]] = obj
	}
	return out, nil
}

// hashObjectRaw computes an object's id without a store instance.
func hashObjectRaw(typ string, data []byte) string {
	header := fmt.Sprintf("%s %d\x00", typ, len(data))
	h := sha1.New()
	h.Write([]byte(header))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// applyDelta applies a git delta (copy/insert program) to a base object.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if pos >= len(delta) {
				return 0, errors.New("delta truncated")
			}
			c := delta[pos]
			pos++
			value |= int(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				return value, nil
			}
		}
	}
	sourceSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	targetSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, targetSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			offset, size := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("delta truncated in copy offset")
					}
					offset |= int(delta[pos]) << uint(8*i)
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("delta truncated in copy size")
					}
					size |= int(delta[pos]) << uint(8*i)
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if offset+size > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[offset:offset+size]...)
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, errors.New("delta truncated in insert")
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, errors.New("delta opcode 0 is reserved")
		}
	}
	if len(out) != targetSize {
		return nil, errors.New("delta result size mismatch")
	}
	return out, nil
}

// writePack serializes objects as a version-2 pack of plain (undeltified)
// entries, zlib level 1 — the fastest setting that stays cheap to inflate.
func writePack(objects []*gitObject) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	writeU32(&buf, 2)
	writeU32(&buf, uint32(len(objects)))
	var scratch bytes.Buffer
	for _, obj := range objects {
		code := packTypeCodes[obj.typ]
		size := len(obj.data)
		first := byte(code<<4) | byte(size&0x0f)
		size >>= 4
		for size > 0 {
			buf.WriteByte(first | 0x80)
			first = byte(size & 0x7f)
			size >>= 7
		}
		buf.WriteByte(first)
		scratch.Reset()
		zw, _ := zlib.NewWriterLevel(&scratch, 1)
		_, _ = zw.Write(obj.data)
		_ = zw.Close()
		buf.Write(scratch.Bytes())
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

func writeU32(buf *bytes.Buffer, v uint32) {
	buf.WriteByte(byte(v >> 24))
	buf.WriteByte(byte(v >> 16))
	buf.WriteByte(byte(v >> 8))
	buf.WriteByte(byte(v))
}

// ---------------------------------------------------------------------------
// Pkt-lines
// ---------------------------------------------------------------------------

func pktLine(payload string) []byte {
	length := len(payload) + 4
	return []byte(fmt.Sprintf("%04x%s", length, payload))
}

func pktFlush() []byte { return []byte("0000") }

func pktDelim() []byte { return []byte("0001") }

// readPktLines decodes pkt-line framed data, reporting the payloads and how
// the stream ended (flush, delim, end of input, or a parse error).
type pktResult struct {
	payloads [][]byte
	endKind  string // flush | delim | eof | error
	rest     []byte // unparsed remainder after the terminating packet
}

func readPktLines(data []byte) pktResult {
	var payloads [][]byte
	offset := 0
	for {
		if offset+4 > len(data) {
			return pktResult{payloads: payloads, endKind: "eof", rest: data[offset:]}
		}
		length, err := parsePktLength(data[offset : offset+4])
		if err != nil {
			return pktResult{payloads: payloads, endKind: "error", rest: data[offset:]}
		}
		switch length {
		case 0:
			return pktResult{payloads: payloads, endKind: "flush", rest: data[offset+4:]}
		case 1:
			return pktResult{payloads: payloads, endKind: "delim", rest: data[offset+4:]}
		case 2:
			return pktResult{payloads: payloads, endKind: "flush", rest: data[offset+4:]}
		}
		if length < 4 || offset+length > len(data) {
			return pktResult{payloads: payloads, endKind: "error", rest: data[offset:]}
		}
		payloads = append(payloads, data[offset+4:offset+length])
		offset += length
	}
}

func parsePktLength(b []byte) (int, error) {
	value := 0
	for _, c := range b {
		value <<= 4
		switch {
		case c >= '0' && c <= '9':
			value |= int(c - '0')
		case c >= 'a' && c <= 'f':
			value |= int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			value |= int(c-'A') + 10
		default:
			return 0, errors.New("bad pkt-line length")
		}
	}
	return value, nil
}

// sidebandPackets wraps pack bytes in band-1 pkt-lines of at most 32768
// payload bytes, the framing git-upload-pack uses for the packfile section.
func sidebandPackets(pack []byte) []byte {
	if len(pack) == 0 {
		return pktLine("\x01")
	}
	var out bytes.Buffer
	for start := 0; start < len(pack); start += 32768 {
		end := start + 32768
		if end > len(pack) {
			end = len(pack)
		}
		chunk := make([]byte, 0, end-start+1)
		chunk = append(chunk, 1)
		chunk = append(chunk, pack[start:end]...)
		out.Write(pktLine(string(chunk)))
	}
	return out.Bytes()
}

// ---------------------------------------------------------------------------
// Store: repositories, issues, pull requests
// ---------------------------------------------------------------------------

type label struct {
	id    int
	name  string
	color string
}

type comment struct {
	id   int
	body string
}

type issue struct {
	id       int
	number   int
	title    string
	body     string
	state    string // open | closed
	labels   []string
	seq      int
	version  int
	comments []*comment
}

type flatEntry struct {
	oid  string
	data []byte
}

type pull struct {
	id          int
	number      int
	title       string
	body        string
	state       string // open | closed
	headRef     string
	baseRef     string
	headSha     string
	baseSha     string
	createdFlat map[string]flatEntry
	merged      bool
	mergedAt    *string
	mergeSha    *string
	seq         int
}

type issueCacheEntry struct {
	version int
	text    string
}

type repo struct {
	name          string
	description   string
	private       bool
	defaultBranch string
	refs          map[string]string
	nextNumber    int
	dbID          int
	createdSeq    int
	issues        map[int]*issue
	pulls         map[int]*pull
	labels        map[string]*label
	labelOrder    []string // label names in creation order (REST lists them so)
	issueJSON     map[int]*issueCacheEntry
}

type store struct {
	mu          sync.RWMutex
	objects     *objectStore
	repos       map[string]*repo
	nextID      int
	nextRepoSeq int
}

func newStore() *store {
	return &store{
		objects:     newObjectStore(),
		repos:       make(map[string]*repo),
		nextID:      10000,
		nextRepoSeq: 1,
	}
}

// createRepo registers a repository for the viewer; it returns nil when
// the name is already taken.
func (s *store) createRepo(name string, private bool, description string) *repo {
	if _, exists := s.repos[name]; exists {
		return nil
	}
	r := &repo{
		name:          name,
		description:   description,
		private:       private,
		defaultBranch: defaultBranch,
		refs:          make(map[string]string),
		nextNumber:    1,
		dbID:          s.nextID,
		createdSeq:    s.nextRepoSeq,
		issues:        make(map[int]*issue),
		pulls:         make(map[int]*pull),
		labels:        make(map[string]*label),
		issueJSON:     make(map[int]*issueCacheEntry),
	}
	s.nextID++
	s.nextRepoSeq++
	s.repos[name] = r
	return r
}

// getRepo resolves a repository by owner and name. Only the scenario viewer's
// own repositories exist; any other owner is a 404, never a 403.
func (s *store) getRepo(owner, name string) *repo {
	if owner != viewerLogin {
		return nil
	}
	return s.repos[name]
}

func (s *store) listRepos() []*repo {
	out := make([]*repo, 0, len(s.repos))
	for _, r := range s.repos {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].createdSeq < out[j].createdSeq })
	return out
}

func (s *store) deleteRepo(name string) {
	delete(s.repos, name)
}

// sortedRefs returns ref names and their oids in git's refname order.
func (r *repo) sortedRefs() [][2]string {
	names := make([]string, 0, len(r.refs))
	for name := range r.refs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([][2]string, 0, len(names))
	for _, name := range names {
		out = append(out, [2]string{name, r.refs[name]})
	}
	return out
}

func (r *repo) mainTip() string {
	return r.refs["refs/heads/"+r.defaultBranch]
}
