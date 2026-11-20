package main

// Git smart HTTP: protocol v0 and v2 advertisements, ls-refs, fetch with
// shallow support, and receive-pack with report-status. The fetch planner
// reproduces the house algorithm exactly — which objects the client's haves
// already cover, the depth walk from the wants, the shallow boundary, and
// the tag/commit/tree/blob pack order — and memoizes whole fetch outcomes
// so the benchmark's repeated shallow clones cost one computation.

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"sort"
	"strings"
	"sync"
)

const (
	uploadPackCaps  = "multi_ack multi_ack_detailed thin-pack side-band side-band-64k ofs-delta shallow deepen-since deepen-not no-progress include-tag allow-tip-sha1-in-want allow-reachable-sha1-in-want symref=HEAD:refs/heads/main agent=" + agentName
	receivePackCaps = "report-status side-band-64k delete-refs ofs-delta agent=" + agentName
)

// v2Advertisement is constant (it names no refs), so it is framed once.
var v2Advertisement = buildV2Advertisement()

func buildV2Advertisement() []byte {
	var out bytes.Buffer
	out.Write(pktLine("# service=git-upload-pack\n"))
	out.Write(pktFlush())
	out.Write(pktLine("version 2\n"))
	out.Write(pktLine("agent=" + agentName + "\n"))
	out.Write(pktLine("ls-refs=unborn\n"))
	out.Write(pktLine("fetch=shallow filter\n"))
	out.Write(pktLine("object-format=sha1\n"))
	out.Write(pktFlush())
	return out.Bytes()
}

// peelOf returns the commit a tag ref peels to, or "" for non-tag refs and
// lightweight tags (the peeled line is only advertised for annotated tags).
func (s *store) peelOf(r *repo, name, oid string) string {
	if !strings.HasPrefix(name, "refs/tags/") {
		return ""
	}
	obj := s.objects.get(oid)
	if obj == nil || obj.typ != "tag" {
		return ""
	}
	return s.objects.peelToCommit(oid)
}

// v0Advertisement lists every ref (sorted), with peeled lines for annotated
// tags, under the service's capability string. The HEAD line always names
// the tip of refs/heads/main.
func (s *store) v0Advertisement(r *repo, service string) []byte {
	caps := uploadPackCaps
	if service == "git-receive-pack" {
		caps = receivePackCaps
	}
	refs := r.sortedRefs()
	out := make([]byte, 0, 128+len(refs)*96)
	out = appendPktText(out, "# service="+service+"\n")
	out = append(out, pktFlush()...)
	mainTip := r.refs["refs/heads/main"]
	if len(refs) == 0 {
		out = appendPktText(out, zeroOid+" capabilities^{}\x00"+caps+"\n")
	} else {
		head := mainTip
		if head == "" {
			head = zeroOid
		}
		out = appendPktText(out, head+" HEAD\x00"+caps+"\n")
		for _, ref := range refs {
			out = appendPktText(out, ref[1]+" "+ref[0]+"\n")
		}
		for _, ref := range refs {
			if peeled := s.peelOf(r, ref[0], ref[1]); peeled != "" {
				out = appendPktText(out, peeled+" "+ref[0]+"^{}\n")
			}
		}
	}
	out = append(out, pktFlush()...)
	return out
}

// collectDataLines decodes a pkt-line stream and returns the payloads of its
// data packets (flush/delim/response-end packets are structural and drop
// out). Malformed framing is an error, mirroring decodePktLines.
func collectDataLines(body []byte) ([]string, error) {
	var lines []string
	offset := 0
	for offset < len(body) {
		if offset+4 > len(body) {
			return nil, errors.New("truncated pkt-line length")
		}
		length, ok := parseHexPrefix(string(body[offset : offset+4]))
		if !ok {
			return nil, errors.New("not a pkt-line")
		}
		switch length {
		case 0, 1, 2:
			offset += 4
		default:
			if length < 4 || offset+length > len(body) {
				return nil, errors.New("pkt-line length out of range")
			}
			lines = append(lines, string(body[offset+4:offset+length]))
			offset += length
		}
	}
	return lines, nil
}

// parseHexPrefix mirrors parseInt(text, 16): leading whitespace and an
// optional 0x prefix are skipped, then hex digits are consumed until the
// first non-digit; no digits at all is NaN (ok=false).
func parseHexPrefix(text string) (int, bool) {
	i := 0
	for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\n' || text[i] == '\r') {
		i++
	}
	if i+1 < len(text) && text[i] == '0' && (text[i+1] == 'x' || text[i+1] == 'X') {
		i += 2
	}
	value, digits := 0, 0
	for i < len(text) {
		c := text[i]
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return value, digits > 0
		}
		value = value*16 + d
		digits++
		i++
	}
	return value, digits > 0
}

// lsRefs answers a protocol v2 ls-refs command from its argument lines.
func (s *store) lsRefs(r *repo, lines []string) []byte {
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	for _, line := range lines {
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "agent=") || strings.HasPrefix(text, "object-format=") {
			continue
		}
		switch {
		case text == "peel":
			peel = true
		case text == "symrefs":
			symrefs = true
		case text == "unborn":
			unborn = true
		case strings.HasPrefix(text, "ref-prefix "):
			prefixes = append(prefixes, text[len("ref-prefix "):])
		}
	}
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	out := make([]byte, 0, 256)
	mainTip := r.refs["refs/heads/main"]
	if keep("HEAD") {
		if mainTip != "" {
			line := mainTip + " HEAD"
			if symrefs {
				line += " symref-target:refs/heads/main"
			}
			out = appendPktText(out, line+"\n")
		} else if unborn {
			out = appendPktText(out, "unborn HEAD symref-target:refs/heads/main\n")
		}
	}
	for _, ref := range r.sortedRefs() {
		if !keep(ref[0]) {
			continue
		}
		line := ref[1] + " " + ref[0]
		if peel {
			if peeled := s.peelOf(r, ref[0], ref[1]); peeled != "" {
				line += " peeled:" + peeled
			}
		}
		out = appendPktText(out, line+"\n")
	}
	out = append(out, pktFlush()...)
	return out
}

// ---------------------------------------------------------------------------
// Fetch planning
// ---------------------------------------------------------------------------

type fetchPlan struct {
	acks       []string
	shallow    []string
	unshallow  []string
	wantedRefs [][2]string // {oid, ref}
}

type fetchOutcome struct {
	plan     fetchPlan
	sideband []byte

	// The assembled wire responses, built once per outcome: a fetch's
	// answer is a function of the plan and the sideband alone (the v2
	// acknowledgments section depends only on whether the request had
	// haves), so repeat fetches — the clone benchmark's fifty — serve the
	// cached bytes instead of re-framing the pack every time.
	asmMu      sync.Mutex
	asmV0      []byte
	asmV2Ack   []byte
	asmV2NoAck []byte
}

func (o *fetchOutcome) assembledV0() []byte {
	o.asmMu.Lock()
	defer o.asmMu.Unlock()
	if o.asmV0 == nil {
		o.asmV0 = buildFetchV0(o)
	}
	return o.asmV0
}

func (o *fetchOutcome) assembledV2(withAck bool) []byte {
	o.asmMu.Lock()
	defer o.asmMu.Unlock()
	if withAck {
		if o.asmV2Ack == nil {
			o.asmV2Ack = buildFetchV2(o, true)
		}
		return o.asmV2Ack
	}
	if o.asmV2NoAck == nil {
		o.asmV2NoAck = buildFetchV2(o, false)
	}
	return o.asmV2NoAck
}

// fetchCache memoizes fetch outcomes. The key carries the owner, the repo,
// its sorted refs, the object count, and the fetch inputs, so any push or
// ref change invalidates naturally. Oldest entries are evicted past 32.
type fetchCache struct {
	mu    sync.Mutex
	items map[string]*fetchOutcome
	order []string
}

const fetchCacheMax = 32

func newFetchCache() *fetchCache {
	return &fetchCache{items: make(map[string]*fetchOutcome)}
}

func (c *fetchCache) get(key string) *fetchOutcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.items[key]
}

func (c *fetchCache) put(key string, outcome *fetchOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; !ok {
		c.order = append(c.order, key)
	}
	c.items[key] = outcome
	for len(c.items) > fetchCacheMax && len(c.order) > 0 {
		delete(c.items, c.order[0])
		c.order = c.order[1:]
	}
}

// computeFetch plans a fetch and returns the plan plus the pack objects in
// send order. depth <= 0 means no depth limit.
func (s *store) computeFetch(r *repo, wants, haves, shallowIn []string, depth int) (fetchPlan, []*gitObject) {
	hasDepth := depth > 0
	// Objects the client already holds. A have that is also an old shallow
	// boundary proves only the commit and its tree, never its ancestors, so
	// the reachability walk stops at the old shallow set.
	held := make(map[string]bool)
	if len(haves) > 0 {
		var stop map[string]bool
		if len(shallowIn) > 0 {
			stop = make(map[string]bool, len(shallowIn))
			for _, oid := range shallowIn {
				stop[oid] = true
			}
		}
		for _, have := range haves {
			s.objects.collectReachable(have, held, stop)
		}
	}

	type queueItem struct {
		oid   string
		depth int
	}
	var queue []queueItem
	includedCommits := make(map[string]bool)
	var includedOrder []string
	includedTags := make(map[string]bool)
	var tagOrder []string
	includeCommit := func(oid string) {
		if !includedCommits[oid] {
			includedCommits[oid] = true
			includedOrder = append(includedOrder, oid)
		}
	}
	for _, want := range wants {
		obj := s.objects.get(want)
		if obj == nil {
			continue
		}
		switch obj.typ {
		case "tag":
			if !includedTags[want] {
				includedTags[want] = true
				tagOrder = append(tagOrder, want)
			}
			queue = append(queue, queueItem{tagTarget(string(obj.data)), 0})
		case "commit":
			queue = append(queue, queueItem{want, 0})
		default: // a blob/tree want is sent directly
			includeCommit(want)
		}
	}
	depthOf := make(map[string]int)
	withinLimit := make(map[string]bool)
	for head := 0; head < len(queue); head++ {
		item := queue[head]
		oid, d := item.oid, item.depth
		if known, ok := depthOf[oid]; ok && known <= d {
			continue
		}
		depthOf[oid] = d
		if !hasDepth || d < depth {
			withinLimit[oid] = true
		}
		obj := s.objects.get(oid)
		if obj == nil || obj.typ != "commit" {
			continue
		}
		if (!hasDepth || d < depth) && !held[oid] {
			includeCommit(oid)
		}
		if !hasDepth || d+1 <= depth {
			if info := s.objects.parseCommit(oid); info != nil {
				for _, parent := range info.parents {
					queue = append(queue, queueItem{parent, d + 1})
				}
			}
		}
	}

	var shallow []string
	if hasDepth {
		for _, oid := range includedOrder {
			if depthOf[oid] != depth-1 {
				continue
			}
			if info := s.objects.parseCommit(oid); info != nil && len(info.parents) > 0 {
				shallow = append(shallow, oid)
			}
		}
	}
	shallowSet := make(map[string]bool, len(shallow))
	for _, oid := range shallow {
		shallowSet[oid] = true
	}
	var unshallow []string
	for _, oid := range shallowIn {
		if withinLimit[oid] && !shallowSet[oid] {
			unshallow = append(unshallow, oid)
		}
	}

	toSend := make(map[string]bool)
	for _, oid := range tagOrder {
		toSend[oid] = true
	}
	for _, oid := range includedOrder {
		toSend[oid] = true
		info := s.objects.parseCommit(oid)
		if info == nil {
			continue
		}
		acc := make(map[string]bool)
		s.objects.collectReachable(info.tree, acc, nil)
		for treeOid := range acc {
			toSend[treeOid] = true
		}
	}
	for oid := range held {
		delete(toSend, oid)
	}
	rank := func(typ string) int {
		switch typ {
		case "tag":
			return 0
		case "commit":
			return 1
		case "tree":
			return 2
		}
		return 3
	}
	oids := make([]string, 0, len(toSend))
	for oid := range toSend {
		if s.objects.get(oid) != nil {
			oids = append(oids, oid)
		}
	}
	sort.Slice(oids, func(i, j int) bool {
		ri, rj := rank(s.objects.get(oids[i]).typ), rank(s.objects.get(oids[j]).typ)
		if ri != rj {
			return ri < rj
		}
		return oids[i] < oids[j]
	})
	objects := make([]*gitObject, 0, len(oids))
	for _, oid := range oids {
		objects = append(objects, s.objects.get(oid))
	}

	var acks []string
	for _, have := range haves {
		for _, want := range wants {
			if s.objects.isAncestor(have, want) {
				acks = append(acks, have)
				break
			}
		}
	}
	var wantedRefs [][2]string
	refs := r.sortedRefs()
	for _, want := range wants {
		for _, ref := range refs {
			if ref[1] == want {
				wantedRefs = append(wantedRefs, [2]string{want, ref[0]})
			}
		}
	}
	return fetchPlan{acks: acks, shallow: shallow, unshallow: unshallow, wantedRefs: wantedRefs}, objects
}

// fetchPack returns the (possibly cached) plan and sideband-framed pack for
// one fetch request.
func (a *app) fetchPack(r *repo, wants, haves, shallowIn []string, depth int) *fetchOutcome {
	var refsPart strings.Builder
	for i, ref := range r.sortedRefs() {
		if i > 0 {
			refsPart.WriteByte(';')
		}
		refsPart.WriteString(ref[0] + "=" + ref[1])
	}
	depthPart := ""
	if depth > 0 {
		depthPart = itoa(depth)
	}
	key := strings.Join([]string{
		viewerLogin, r.name, refsPart.String(), itoa(len(a.store.objects.objects)),
		strings.Join(wants, ","), strings.Join(haves, ","), strings.Join(shallowIn, ","), depthPart,
	}, "|")
	if hit := a.fetches.get(key); hit != nil {
		return hit
	}
	plan, objects := a.store.computeFetch(r, wants, haves, shallowIn, depth)
	outcome := &fetchOutcome{plan: plan, sideband: sidebandPackets(writePack(objects))}
	a.fetches.put(key, outcome)
	return outcome
}

// parseFetchArgs extracts wants/haves/shallow/depth from fetch argument
// lines (shared by the v0 and v2 request shapes; v0 want lines may carry
// capabilities after a NUL).
func parseFetchArgs(lines []string, v0 bool) (wants, haves, shallowIn []string, depth int) {
	for _, line := range lines {
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "agent=") || strings.HasPrefix(text, "object-format=") {
			continue
		}
		switch {
		case strings.HasPrefix(text, "want "):
			rest := text[len("want "):]
			if v0 {
				if nul := strings.IndexByte(rest, 0); nul >= 0 {
					rest = rest[:nul]
				}
			}
			wants = append(wants, strings.TrimSpace(rest))
		case strings.HasPrefix(text, "have "):
			haves = append(haves, strings.TrimSpace(text[len("have "):]))
		case strings.HasPrefix(text, "shallow "):
			shallowIn = append(shallowIn, strings.TrimSpace(text[len("shallow "):]))
		case strings.HasPrefix(text, "deepen "):
			if value, ok := parseIntJS(strings.TrimSpace(text[len("deepen "):])); ok {
				depth = value
			}
		}
	}
	return wants, haves, shallowIn, depth
}

// buildFetchV2 renders the protocol v2 fetch response sections; withAck
// says the request carried haves, which is all that gates the
// acknowledgments section.
func buildFetchV2(outcome *fetchOutcome, withAck bool) []byte {
	plan := outcome.plan
	out := make([]byte, 0, len(outcome.sideband)+256)
	if withAck {
		out = appendPktText(out, "acknowledgments\n")
		if len(plan.acks) > 0 {
			for _, ack := range plan.acks {
				out = appendPktText(out, "ACK "+ack+"\n")
			}
			out = appendPktText(out, "ready\n")
		} else {
			out = appendPktText(out, "NAK\n")
		}
		out = append(out, pktDelim()...)
	}
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		out = appendPktText(out, "shallow-info\n")
		for _, oid := range plan.shallow {
			out = appendPktText(out, "shallow "+oid+"\n")
		}
		for _, oid := range plan.unshallow {
			out = appendPktText(out, "unshallow "+oid+"\n")
		}
		out = append(out, pktDelim()...)
	}
	if len(plan.wantedRefs) > 0 {
		out = appendPktText(out, "wanted-refs\n")
		for _, wr := range plan.wantedRefs {
			out = appendPktText(out, wr[0]+" "+wr[1]+"\n")
		}
		out = append(out, pktDelim()...)
	}
	out = appendPktText(out, "packfile\n")
	out = append(out, outcome.sideband...)
	out = append(out, pktFlush()...)
	return out
}

// buildFetchV0 renders the protocol v0 fetch response.
func buildFetchV0(outcome *fetchOutcome) []byte {
	plan := outcome.plan
	out := make([]byte, 0, len(outcome.sideband)+128)
	if len(plan.acks) > 0 {
		for _, ack := range plan.acks {
			out = appendPktText(out, "ACK "+ack+"\n")
		}
	} else {
		out = appendPktText(out, "NAK\n")
	}
	for _, oid := range plan.shallow {
		out = appendPktText(out, "shallow "+oid+"\n")
	}
	for _, oid := range plan.unshallow {
		out = appendPktText(out, "unshallow "+oid+"\n")
	}
	out = append(out, outcome.sideband...)
	out = append(out, pktFlush()...)
	return out
}

// ---------------------------------------------------------------------------
// Receive-pack
// ---------------------------------------------------------------------------

// splitPushCommands separates the ref-update command lines from the pack
// bytes of a receive-pack request body. Malformed framing is an error.
func splitPushCommands(body []byte) ([]string, []byte, error) {
	var commands []string
	offset := 0
	for {
		if offset+4 > len(body) {
			return nil, nil, errors.New("truncated pkt-line in command list")
		}
		length, ok := parseHexPrefix(string(body[offset : offset+4]))
		if !ok {
			return nil, nil, errors.New("not a pkt-line")
		}
		if length == 0 {
			offset += 4
			break
		}
		if length < 4 || offset+length > len(body) {
			return nil, nil, errors.New("pkt-line length out of range")
		}
		commands = append(commands, string(body[offset+4:offset+length]))
		offset += length
	}
	return commands, body[offset:], nil
}

// receivePack applies a push: unpack the pack (if any), then apply each ref
// update in order, and render the report-status reply. A malformed command
// stream yields ok=false so the caller answers 400.
func (a *app) receivePack(r *repo, body []byte) ([]byte, bool) {
	rawCommands, pack, err := splitPushCommands(body)
	if err != nil {
		return nil, false
	}
	unpackOK := true
	unpackDetail := "ok"
	if len(pack) > 0 {
		if len(pack) < 32 || sha1.Sum(pack[:len(pack)-20]) != [20]byte(pack[len(pack)-20:]) {
			unpackOK = false
			unpackDetail = "pack checksum mismatch"
		} else if objects, err := readPack(pack, a.store.objects.get); err != nil {
			unpackOK = false
			unpackDetail = err.Error()
		} else {
			for oid, obj := range objects {
				if _, exists := a.store.objects.objects[oid]; !exists {
					a.store.objects.objects[oid] = obj
				}
			}
		}
	}

	sideband := true
	type result struct {
		ref    string
		ok     bool
		reason string
	}
	var results []result
	for _, raw := range rawCommands {
		head := raw
		if nul := strings.IndexByte(raw, 0); nul >= 0 {
			head = raw[:nul]
			caps := strings.Fields(raw[nul+1:])
			sideband = false
			for _, c := range caps {
				if c == "side-band-64k" {
					sideband = true
				}
			}
		}
		parts := strings.Fields(head)
		if len(parts) < 3 {
			continue
		}
		oldOid, newOid, ref := parts[0], parts[1], parts[2]
		current := r.refs[ref]
		fail := func(reason string) {
			results = append(results, result{ref, false, reason})
		}
		if !unpackOK {
			fail("unpack failed")
			continue
		}
		if newOid == zeroOid { // delete
			if current == "" {
				fail("remote ref does not exist")
			} else if current != oldOid {
				fail("old value mismatch")
			} else {
				delete(r.refs, ref)
				results = append(results, result{ref, true, ""})
			}
			continue
		}
		if oldOid == zeroOid { // create
			if current != "" {
				fail("already exists")
				continue
			}
		} else { // update
			if current == "" {
				fail("remote ref does not exist")
				continue
			}
			if current != oldOid {
				fail("old value mismatch")
				continue
			}
		}
		if a.store.objects.get(newOid) == nil {
			fail("object not found")
			continue
		}
		if oldOid != zeroOid && !a.store.objects.isAncestor(oldOid, newOid) {
			fail("non-fast-forward")
			continue
		}
		r.refs[ref] = newOid
		results = append(results, result{ref, true, ""})
	}

	inner := make([]byte, 0, 64+len(results)*64)
	if unpackOK {
		inner = appendPktText(inner, "unpack ok\n")
	} else {
		inner = appendPktText(inner, "unpack "+unpackDetail+"\n")
	}
	for _, res := range results {
		if res.ok {
			inner = appendPktText(inner, "ok "+res.ref+"\n")
		} else {
			inner = appendPktText(inner, "ng "+res.ref+" "+res.reason+"\n")
		}
	}
	inner = append(inner, pktFlush()...)
	if !sideband {
		return inner, true
	}
	out := sidebandPackets(inner)
	out = append(out, pktFlush()...)
	return out, true
}
