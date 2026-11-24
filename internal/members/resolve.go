package members

import (
	"crypto/sha256"
	"encoding/hex"
	"math/bits"
	"sort"
	"strconv"
	"strings"
)

// Effect says whether an entry changed anything and, if not, why (A4).
type Effect struct {
	Applied bool
	Reason  string
}

// Reasons an entry can have no effect. They are shown to people, so they
// are plain sentences.
const (
	ReasonBadID         = "its id does not match its content"
	ReasonMalformed     = "it is malformed"
	ReasonMissingParent = "an entry it says it had seen is missing"
	ReasonBadChain      = "its previous entry is not its author's previous entry"
	ReasonOtherCreate   = "a create entry other than the room's own"
	ReasonOtherRoom     = "it is signed for another room"
	ReasonNotVerified   = "it is not a verified entry"
	// Machine only.
	ReasonForkRefused     = "another entry its author signed at this position was accepted first"
	ReasonRefusedParent   = "an entry it had seen was refused as a fork"
	ReasonDroppedHeld     = "too many entries were waiting for entries they had seen; the oldest was dropped"
	ReasonTooManyBranches = "its author signed more entries at this position than a machine keeps"
	ReasonInvalidParent   = "an entry it had seen is itself invalid"
	ReasonNoRoot          = "the set does not contain exactly one create entry"
	ReasonNotAdmin        = "its author was not an admin in what it had seen"
	ReasonAlreadyMember   = "the machine was already a member in what its author had seen"
	ReasonAlreadyAdmin    = "the machine was already an admin in what its author had seen"
	ReasonNotMember       = "the machine was not a member in what its author had seen"
	ReasonNeedsSecond     = "changing the admin set needs a matching entry from another admin"
	ReasonAfterCut        = "its author was removed as an admin, and no remover had seen this grant"
	ReasonFork            = "its author signed two different entries at one position, and no removal has settled which one stands"
	ReasonAuthorVoid      = "its author's own admin role had no effect"
	ReasonRemovalWins     = "a removal of the same machine that it had not seen takes precedence"
	ReasonSelfVote        = "a machine's vote on its own removal does not count"
)

// State is what a set of entries means: who is a member, who is an admin,
// and for every entry whether it had an effect.
type State struct {
	Members []string
	Admins  []string
	// Admitted lists every machine that has ever had an effective
	// admission in this set: the creator, and every target of an add or
	// admin grant that took effect, even if a later removal undid it.
	Admitted []string
	// Permissions gives, for each current member, its history permission.
	Permissions map[string]Permission
	Effects     map[string]Effect

	members, admins map[string]bool
	// removedBy is, per machine, the ids of the removals of it that took
	// effect, joined. Two removal votes for an admin combine only if their
	// authors had seen the same removals of it, so an old vote cannot be
	// reused after the machine was added back.
	removedBy map[string]string
}

// Permission is a member's current history permission (PROTOCOL section
// 3). It comes from the effective grants of its current membership period:
// the adds, and the admin grants that admitted it (its target was not a
// member in the signer's view), that had seen every removal of it that
// counts. Promoting an existing member changes nothing.
//
// All means it may read every earlier record: every one of those grants
// has history. Otherwise From is the union, per author, of the heads of
// the grants without history: a record by that author at or before one of
// its heads is hidden. The union is the more restrictive of the grants.
// The caller decides "at or before" over records; a head it does not hold,
// or cannot relate, hides everything by that author (fail closed). Closed
// means no grant of the current period is known (the member's only
// admission had no effect and it is a member through a later promotion):
// it may read no earlier record. The creator, never removed, has All.
type Permission struct {
	All    bool
	Closed bool
	From   map[string][]string
	Grants []string // ids of the grants, sorted
}

// Hidden reports whether a record by author, with id rec, is hidden from
// the member: it is at or before one of the member's heads for that
// author. atOrBefore(head, rec) is the caller's relation over records
// (through prev and links) and whether it could decide it; a head it does
// not hold, or cannot relate, hides everything by that author (fail
// closed). A Closed permission hides everything.
func (p Permission) Hidden(author, rec string, atOrBefore func(head, rec string) (yes, known bool)) bool {
	if p.All {
		return false
	}
	if p.Closed {
		return true
	}
	for _, h := range p.From[author] {
		if yes, known := atOrBefore(h, rec); yes || !known {
			return true
		}
	}
	return false
}

// ruleSet are the knobs that separate candidate rule sets. final is the rule
// set this model ends with; lead is the lead's starting candidate, kept so
// the tests can show where it fails.
type ruleSet struct {
	// SoloLimit: while the room has at most this many admins (as seen by
	// the author), one admin alone may change the admin set. Above it, two
	// distinct admins must sign matching entries.
	SoloLimit int
	// TargetJudgedInOwnPast: a removal is a member removal (one admin is
	// enough) or an admin removal (needs the threshold) judged only by what
	// its author had seen. final rules also treat it as an admin removal
	// when the target has been made an admin concurrently.
	TargetJudgedInOwnPast bool
	// id is the entry id function: the model hash when nil (the tests),
	// the signed-bytes id for entries from internal/core (the daemon).
	// Machine.receive and validate both check ids with it.
	id func(entry) string
}

// idOf returns e's id as r's id function computes it.
func (r ruleSet) idOf(e entry) string {
	if r.id == nil {
		return e.hash()
	}
	return r.id(e)
}

var (
	// final: one admin alone may change the admin set only while it is the
	// room's only admin (T9 as written).
	final = ruleSet{SoloLimit: 1}
	// lead: candidate rules C1-C7 from the brief.
	lead = ruleSet{SoloLimit: 2, TargetJudgedInOwnPast: true}
)

// Resolve applies the final rules. It is a pure function of the set of
// entries: order and duplicates in the slice do not matter.
func resolveSet(entries []entry) State { return final.resolve(entries) }

// Resolve computes the state of a room from a set of entries.
// The room's create is the set's only create entry; a set with several
// has no room (ReasonNoRoot): use ResolveRoom, which names it.
func (r ruleSet) resolve(entries []entry) State {
	return r.resolveShared(entries, map[string]*State{}, "")
}

// ResolveRoom computes the state of the room whose create entry is root.
// Every other create entry has no effect (ReasonOtherCreate), and neither
// has anything that depends on one. This is what a machine uses: it is
// pinned to its room's create.
func (r ruleSet) resolveRoom(root string, entries []entry) State {
	return r.resolveShared(entries, map[string]*State{}, root)
}

// resolveShared is Resolve with a cache of whole-set states, which the
// exhaustive tests share across calls: the state of a causally closed set
// depends only on its entries.
func (r ruleSet) resolveShared(entries []entry, memo map[string]*State, root string) State {
	effects := map[string]Effect{}
	valid := validate(entries, effects, root, r.idOf)
	key := strings.Join(sortedKeys(valid), ",")
	st, ok := memo[key]
	if !ok {
		st = newGraph(r, valid).top()
		memo[key] = st
	}
	out := *st
	out.Effects = effects
	for id, ef := range st.Effects {
		out.Effects[id] = ef
	}
	return out
}

// validate checks each entry's form and chain and keeps the valid ones.
// Every entry it drops gets a reason.
func validate(entries []entry, eff map[string]Effect, root string, idOf func(entry) string) map[string]entry {
	byID := map[string]entry{}
	rootRoom := roomOf(entries, root)
	for _, e := range entries {
		switch {
		case e.ID != idOf(e):
			eff[e.ID] = Effect{Reason: ReasonBadID}
		case malformed(e):
			eff[e.ID] = Effect{Reason: ReasonMalformed}
		case root != "" && e.Kind == kCreate && e.ID != root:
			eff[e.ID] = Effect{Reason: ReasonOtherCreate}
		case root != "" && e.room != rootRoom:
			eff[e.ID] = Effect{Reason: ReasonOtherRoom}
		default:
			byID[e.ID] = e
		}
	}
	// An entry is valid if every entry it had seen is valid and its previous
	// entry is its author's previous entry.
	valid := map[string]entry{}
	done := map[string]bool{}
	var check func(id string) bool
	check = func(id string) bool {
		if done[id] {
			_, ok := valid[id]
			return ok
		}
		done[id] = true // also stops a cycle, which only forged ids can make
		e, ok := byID[id]
		if !ok {
			return false
		}
		for _, p := range e.Parents {
			if !check(p) {
				reason := ReasonInvalidParent
				for _, q := range e.Parents {
					if _, ok := byID[q]; !ok {
						reason = ReasonMissingParent
					}
				}
				eff[id] = Effect{Reason: reason}
				return false
			}
		}
		if e.Prev != "" {
			if p := valid[e.Prev]; p.Author != e.Author || p.Seq != e.Seq-1 {
				eff[id] = Effect{Reason: ReasonBadChain}
				return false
			}
		}
		valid[id] = e
		return true
	}
	for _, id := range sortedKeys(byID) {
		check(id)
	}
	roots := 0
	for _, e := range valid {
		if e.Kind == kCreate {
			roots++
		}
	}
	if roots != 1 {
		for id := range valid {
			eff[id] = Effect{Reason: ReasonNoRoot}
		}
		return map[string]entry{}
	}
	return valid
}

func malformed(e entry) bool {
	if e.Author == "" {
		return true
	}
	if e.Kind == kCreate {
		return e.Seq != 1 || e.Prev != "" || len(e.Parents) != 0 || e.Target != ""
	}
	if e.Kind != kAdd && e.Kind != kRemove && e.Kind != kAdmin {
		return true
	}
	if e.Seq < 1 || (e.Seq == 1) != (e.Prev == "") || e.Target == "" || len(e.Parents) == 0 {
		return true
	}
	seen := map[string]bool{}
	for _, p := range e.Parents {
		if seen[p] {
			return true
		}
		seen[p] = true
	}
	return e.Prev != "" && !seen[e.Prev]
}

// How the resolver is built. Every rule judges an entry against the state
// of its causal past, so the resolver needs the state of every entry's
// past, and those states are resolutions too. Entries are numbered once in
// causal order (by number of ancestors, then id); an entry's ancestors are
// a bitset over that numbering, and the state of each past is computed
// once, in that order, keeping only what later rules read from it. What an
// entry's past says (was its author an admin, the threshold it saw, the
// period of membership of its author and target) is computed once from
// that state.

type bitset []uint64

func newBitset(n int) bitset    { return make(bitset, (n+63)>>6) }
func (b bitset) has(i int) bool { return b[i>>6]>>(uint(i)&63)&1 != 0 }
func (b bitset) set(i int)      { b[i>>6] |= 1 << (uint(i) & 63) }
func (b bitset) union(c bitset) {
	for i := range c {
		b[i] |= c[i]
	}
}
func (b bitset) count() (n int) {
	for _, w := range b {
		n += bits.OnesCount64(w)
	}
	return n
}

// pstate is what the rules read from the state of an entry's past.
type pstate struct {
	members, admins bitset // by name
	nAdmins         int
	removedBy       map[int]string // by name: the keys of its removals
	applied         bitset         // by entry: had effect
}

type graph struct {
	rules ruleSet
	valid map[string]entry
	e     []entry // in causal order
	anc   []bitset
	names []string
	// by entry
	author, target []int
	dup            []bool // its author signed another entry at its position
	past           []*pstate
	thr            []int
	authed         []bool
	why            []string // its reason in the resolution under way
	aek, tek       []string // author or target, and its period in this past
	forkAdmin      []bool
	// by name
	byAuthor     [][]int
	adds, promos [][]int // authed entries
	removes      []int   // authed removals
	forks        map[string]map[int64][]forkEntry
}

// forkEntry is one entry at a position where its author signed more than
// one entry, and whether its author was an admin in its past.
type forkEntry struct {
	i     int
	admin bool
}

// removalEvent is one removal that took effect: a single entry for a
// machine that is not an admin, or the matching votes that removed an
// admin. An entry has seen the event once it has seen enough of its votes
// to complete it, so a late extra vote changes nothing.
type removalEvent struct {
	key    string // stable: the same in every set that contains the event
	target int
	admin  bool
	votes  []int
	kept   map[int]bool // the target's entries that stand: those a voter had seen
	// forkKey names the target and the period this removal ends: fork
	// evidence by the target in that period counts as its own vote, from
	// any entry whose past holds it.
	forkKey string
}

func newGraph(r ruleSet, valid map[string]entry) *graph {
	g := &graph{rules: r, valid: valid}
	ids := sortedKeys(valid)
	n := len(ids)
	// A first numbering in any causal order, to count ancestors.
	tmp := map[string]int{}
	var topo []string
	var visit func(id string)
	visit = func(id string) {
		if _, ok := tmp[id]; ok {
			return
		}
		tmp[id] = -1
		for _, p := range valid[id].Parents {
			visit(p)
		}
		tmp[id] = len(topo)
		topo = append(topo, id)
	}
	for _, id := range ids {
		visit(id)
	}
	count := map[string]int{}
	{
		anc := make([]bitset, n)
		for i, id := range topo {
			anc[i] = newBitset(n)
			for _, p := range valid[id].Parents {
				anc[i].union(anc[tmp[p]])
				anc[i].set(tmp[p])
			}
			count[id] = anc[i].count()
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		if a, b := count[ids[i]], count[ids[j]]; a != b {
			return a < b
		}
		return ids[i] < ids[j]
	})
	idx := map[string]int{}
	name := map[string]int{}
	nameOf := func(s string) int {
		if k, ok := name[s]; ok {
			return k
		}
		name[s] = len(g.names)
		g.names = append(g.names, s)
		return name[s]
	}
	positions := map[string]int{}
	g.e = make([]entry, n)
	g.anc = make([]bitset, n)
	g.author, g.target = make([]int, n), make([]int, n)
	for i, id := range ids {
		e := valid[id]
		idx[id] = i
		g.e[i] = e
		g.anc[i] = newBitset(n)
		for _, p := range e.Parents {
			g.anc[i].union(g.anc[idx[p]])
			g.anc[i].set(idx[p])
		}
		g.author[i], g.target[i] = nameOf(e.Author), nameOf(e.Target)
		positions[position(e)]++
	}
	g.dup = make([]bool, n)
	for i, e := range g.e {
		g.dup[i] = positions[position(e)] > 1
	}
	m := len(g.names)
	g.byAuthor, g.adds, g.promos = make([][]int, m), make([][]int, m), make([][]int, m)
	for i := range g.e {
		g.byAuthor[g.author[i]] = append(g.byAuthor[g.author[i]], i)
	}
	g.past, g.thr, g.authed, g.why = make([]*pstate, n), make([]int, n), make([]bool, n), make([]string, n)
	g.aek, g.tek, g.forkAdmin = make([]string, n), make([]string, n), make([]bool, n)
	byParents := map[string]*pstate{}
	for i, e := range g.e {
		ps := make([]int, len(e.Parents))
		for j, p := range e.Parents {
			ps[j] = idx[p]
		}
		sort.Ints(ps)
		var kb strings.Builder
		for _, p := range ps {
			kb.WriteString(strconv.Itoa(p))
			kb.WriteByte(',')
		}
		k := kb.String()
		p, ok := byParents[k]
		if !ok {
			p, _ = g.resolve(g.anc[i], false)
			byParents[k] = p
		}
		g.past[i] = p
		if i > 0 {
			g.judge(i)
		}
	}
	return g
}

// judge applies step 1 to entry i: was its author an admin in what it had
// seen, and did the change make sense there?
func (g *graph) judge(i int) {
	p, e, a, t := g.past[i], g.e[i], g.author[i], g.target[i]
	g.thr[i] = 2
	if p.nAdmins <= g.rules.SoloLimit {
		g.thr[i] = 1
	}
	g.aek[i] = e.Author + "\x00" + p.removedBy[a]
	g.tek[i] = e.Target + "\x00" + p.removedBy[t]
	g.forkAdmin[i] = p.admins.has(a)
	switch {
	case !p.admins.has(a):
		g.why[i] = ReasonNotAdmin
	case e.Kind == kAdd && p.members.has(t):
		g.why[i] = ReasonAlreadyMember
	case e.Kind == kAdmin && p.admins.has(t):
		g.why[i] = ReasonAlreadyAdmin
	case e.Kind == kRemove && !p.members.has(t):
		g.why[i] = ReasonNotMember
	case e.Kind == kRemove && e.Target == e.Author:
		g.why[i] = ReasonSelfVote
	default:
		g.authed[i] = true
		switch e.Kind {
		case kAdd:
			g.adds[t] = append(g.adds[t], i)
		case kAdmin:
			g.promos[t] = append(g.promos[t], i)
		case kRemove:
			g.removes = append(g.removes, i)
		}
	}
}

// top resolves the whole set.
func (g *graph) top() *State {
	all := newBitset(len(g.e))
	for i := range g.e {
		all.set(i)
	}
	_, st := g.resolve(all, true)
	return st
}

// votesFor returns the authed votes in the set for a change.
func (g *graph) votesFor(in bitset, kind entryKind, t int) []int {
	all := g.promos[t]
	if kind == kAdd {
		all = g.adds[t]
	}
	for j, x := range all {
		if !in.has(x) {
			out := append([]int(nil), all[:j]...)
			for _, y := range all[j+1:] {
				if in.has(y) {
					out = append(out, y)
				}
			}
			return out
		}
	}
	return all
}

// quorum reports whether votes come from enough distinct admins. Each vote
// carries the threshold its author saw; the lowest one applies, so a vote
// made while its author was the only admin stands alone.
func (g *graph) quorum(votes []int) bool { return g.evQuorum(nil, votes, false) }

// evQuorum is quorum for a removal's votes, counting the target's fork
// evidence as its vote when withEvidence is set. At least one real vote is
// needed: evidence alone never removes anyone.
func (g *graph) evQuorum(ev *removalEvent, votes []int, withEvidence bool) bool {
	if len(votes) == 0 {
		return false
	}
	var authors [2]int
	n, need := 0, 2
	add := func(a int) {
		if n < 2 && (n == 0 || authors[0] != a) {
			authors[n] = a
			n++
		}
	}
	for _, v := range votes {
		add(g.author[v])
		if g.thr[v] < need {
			need = g.thr[v]
		}
	}
	if withEvidence {
		add(ev.target)
	}
	return n >= need
}

// qualifies reports whether the picked entries of one position are fork
// evidence: two or more, at least one signed by an admin.
func qualifies(fs []forkEntry, pick func(int) bool) bool {
	n, admin := 0, false
	for _, f := range fs {
		if pick(f.i) {
			n++
			admin = admin || f.admin
		}
	}
	return n >= 2 && admin
}

// forkSeen reports whether the entries pick selects hold fork evidence for
// key ek (author and period) at some position.
func (g *graph) forkSeen(ek string, pick func(int) bool) bool {
	for _, fs := range g.forks[ek] {
		if qualifies(fs, pick) {
			return true
		}
	}
	return false
}

// resolve computes the state of a causally closed subset in of the graph.
// With top it also returns the full State.
func (g *graph) resolve(in bitset, top bool) (*pstate, *State) {
	n := len(g.e)
	ps := &pstate{members: newBitset(len(g.names)), admins: newBitset(len(g.names)), removedBy: map[int]string{}, applied: newBitset(n)}
	var order []int
	for w, word := range in {
		for word != 0 {
			order = append(order, w*64+bits.TrailingZeros64(word))
			word &= word - 1
		}
	}
	if len(order) == 0 {
		return ps, &State{Effects: map[string]Effect{}, members: map[string]bool{}, admins: map[string]bool{}, removedBy: map[string]string{}}
	}
	ps.applied.set(0)
	rootAuthor := g.author[0]
	why := g.why
	saved := make([]string, len(order))
	for j, i := range order {
		saved[j] = why[i]
	}
	defer func() {
		for j, i := range order {
			why[i] = saved[j]
		}
	}()

	// Fork evidence: two different entries by one key at one position, in
	// one period of its membership, at least one signed while it was an
	// admin. It counts as that admin's vote for its own removal in that
	// period (rule 12). Whether an entry has seen it is a property of the
	// entry's own past: does that past hold such two entries.
	forks := map[string]map[int64][]forkEntry{}
	for _, i := range order[1:] {
		if !g.dup[i] {
			continue
		}
		ek := g.aek[i]
		if forks[ek] == nil {
			forks[ek] = map[int64][]forkEntry{}
		}
		forks[ek][g.e[i].Seq] = append(forks[ek][g.e[i].Seq], forkEntry{i, g.forkAdmin[i]})
	}
	forkQ := map[string]int64{} // the first position with evidence in the whole set
	for ek, at := range forks {
		for q, fs := range at {
			if len(fs) < 2 {
				delete(at, q)
			} else if qualifies(fs, func(int) bool { return true }) && (forkQ[ek] == 0 || q < forkQ[ek]) {
				forkQ[ek] = q
			}
		}
		if len(at) == 0 {
			delete(forks, ek)
		}
	}
	g.forks = forks

	// Step 2. Removals. A removal counts if its author was an admin in what
	// it had seen; removals are never cancelled by later knowledge. Removing
	// an admin needs the threshold; so does removing a machine that was
	// made an admin concurrently, otherwise a key that had not seen the
	// promotion could remove the new admin alone.
	removals := map[int][]*removalEvent{}
	byKey := map[string]*removalEvent{}
	addEvent := func(ev *removalEvent) {
		removals[ev.target] = append(removals[ev.target], ev)
		byKey[ev.key] = ev
		for _, v := range ev.votes {
			ps.applied.set(v)
			// A removal seen in an entry's past as a plain member's may
			// count here as an admin's, when a concurrent promotion of the
			// target is in this set: the vote finds its event either way.
			if _, ok := byKey[g.e[v].ID]; !ok {
				byKey[g.e[v].ID] = ev
			}
		}
	}
	var removes []int
	for _, i := range g.removes {
		if in.has(i) {
			removes = append(removes, i)
		}
	}
	// A removal signed by a forked key at or after its fork, in the period
	// of the fork, has no effect once another admin has voted to remove
	// that key, unless some such vote had seen it take effect: the fork and
	// that vote remove the key (rule 12), and, as in rule 9, what no
	// remover had seen take effect does not stand.
	againstForker := map[string][]int{}
	for _, i := range removes {
		if forkQ[g.tek[i]] > 0 {
			againstForker[g.tek[i]] = append(againstForker[g.tek[i]], i)
		}
	}
	forkedOut := func(i int) bool {
		ek := g.aek[i]
		if q := forkQ[ek]; q == 0 || g.e[i].Seq < q {
			return false
		}
		for _, w := range againstForker[ek] {
			if g.anc[w].has(i) && g.past[w].applied.has(i) {
				return false
			}
		}
		return len(againstForker[ek]) > 0
	}
	groups := map[string][]int{}
	var groupOrder []string
	for _, i := range removes {
		if forkedOut(i) {
			why[i] = ReasonFork
			continue
		}
		t := g.target[i]
		adminRemoval := g.past[i].admins.has(t)
		if !adminRemoval && !g.rules.TargetJudgedInOwnPast {
			var gs []int
			concurrent := false
			for _, x := range g.votesFor(in, kAdmin, t) {
				if !g.anc[x].has(i) {
					gs = append(gs, x)
					concurrent = concurrent || !g.anc[i].has(x)
				}
			}
			adminRemoval = concurrent && g.quorum(gs)
		}
		if !adminRemoval {
			addEvent(&removalEvent{key: g.e[i].ID, target: t, votes: []int{i}})
			continue
		}
		k := g.tek[i]
		if _, ok := groups[k]; !ok {
			groupOrder = append(groupOrder, k)
		}
		groups[k] = append(groups[k], i)
	}
	for _, k := range groupOrder {
		vs := groups[k]
		t := g.target[vs[0]]
		sum := sha256.Sum256([]byte(k))
		ev := &removalEvent{key: "r" + hex.EncodeToString(sum[:8]), target: t, admin: true, votes: vs, kept: map[int]bool{}, forkKey: k}
		if !g.evQuorum(ev, vs, forkQ[k] > 0) {
			for _, v := range vs {
				why[v] = ReasonNeedsSecond
			}
			continue
		}
		// Which of the target's entries stand: those that some remover had
		// seen take effect. So a removal, once complete, is closed: a later
		// vote had seen the entries it voided as void, and a concurrent
		// extra vote can only add to what stands. A grant vote still
		// waiting for a second admin in a remover's view does not stand
		// through that remover. An addition revived this way is one the
		// voter could have made itself. Where the entries that stand
		// include two at one position (a fork), neither branch from that
		// position on stands.
		atSeq := map[int64]int{}
		forkFrom := int64(0)
		for _, v := range vs {
			for _, x := range g.byAuthor[t] {
				if !g.anc[v].has(x) || !in.has(x) || !g.past[v].applied.has(x) {
					continue
				}
				ev.kept[x] = true
				q := g.e[x].Seq
				if other, ok := atSeq[q]; ok && other != x && (forkFrom == 0 || q < forkFrom) {
					forkFrom = q
				}
				atSeq[q] = x
			}
		}
		if forkFrom > 0 {
			for x := range ev.kept {
				if g.e[x].Seq >= forkFrom {
					delete(ev.kept, x)
				}
			}
		}
		addEvent(ev)
	}

	// Step 3. Forks: two different entries by one author at one position,
	// signed while it was an admin (a fork by a key that was not an admin
	// signs nothing with effect, so it changes nothing). A fork belongs to
	// the period of membership in which it was signed: forkQ, by author and
	// period.

	// Step 4. Grants (add, make admin), in causal order. A grant has effect
	// only if its author's admin role itself had effect, some remover had
	// seen it in every removal of its author, and its author has not forked
	// at or before it without a removal settling the fork.
	//
	// A grant by a removed admin has no effect if, for some removal of that
	// admin, no remover had seen it and it had not seen the removal. A
	// later removal whose removers have seen it does not revive it.
	eligible := newBitset(n)
	for _, i := range order[1:] {
		if !g.authed[i] || g.e[i].Kind == kRemove {
			continue
		}
		afterCut, keptByCut := false, false
		for _, ev := range removals[g.author[i]] {
			if !ev.admin {
				continue
			}
			if ev.kept[i] {
				keptByCut = true
			} else if !g.seesEvent(i, ev) {
				afterCut = true
			}
		}
		fq := forkQ[g.aek[i]]
		switch {
		case afterCut:
			why[i] = ReasonAfterCut
		case fq > 0 && g.e[i].Seq >= fq && !keptByCut:
			why[i] = ReasonFork
		case !g.genuine(in, g.author[i], i, rootAuthor, byKey, eligible):
			why[i] = ReasonAuthorVoid
		default:
			eligible.set(i)
		}
	}

	// Step 5. The result. A machine is a member if some grant of it had
	// seen every removal of it that took effect: a removal beats a
	// concurrent add, and an add made with knowledge of the removal
	// re-admits.
	seesAll := func(i, t int) bool {
		for _, ev := range removals[t] {
			if !g.seesEvent(i, ev) {
				return false
			}
		}
		return true
	}
	candidate := newBitset(len(g.names))
	candidate.set(rootAuthor)
	for _, i := range order[1:] {
		candidate.set(g.target[i])
	}
	for x := range g.names {
		if !candidate.has(x) {
			continue
		}
		admin := x == rootAuthor && len(removals[x]) == 0
		if !admin {
			var vs []int
			for _, v := range g.votesFor(in, kAdmin, x) {
				if eligible.has(v) && seesAll(v, x) {
					vs = append(vs, v)
				}
			}
			admin = g.quorum(vs)
		}
		member := admin
		for _, a := range g.votesFor(in, kAdd, x) {
			member = member || (eligible.has(a) && seesAll(a, x))
		}
		if admin {
			ps.admins.set(x)
			ps.nAdmins++
		}
		if member {
			ps.members.set(x)
		}
		if len(removals[x]) > 0 {
			var keys []string
			for _, ev := range removals[x] {
				keys = append(keys, ev.key)
			}
			sort.Strings(keys)
			ps.removedBy[x] = strings.Join(keys, ",")
		}
	}
	for _, i := range order[1:] {
		t := g.target[i]
		switch {
		case ps.applied.has(i):
		case !eligible.has(i):
			// reason already recorded
		case !g.seesEarlierRemovals(i, removals[t]):
			why[i] = ReasonRemovalWins
		case g.e[i].Kind == kAdmin && !g.grantReached(i, g.votesFor(in, kAdmin, t), eligible, removals[t]):
			why[i] = ReasonNeedsSecond
		default:
			ps.applied.set(i)
		}
	}
	if !top {
		return ps, nil
	}

	st := &State{Effects: map[string]Effect{}, members: map[string]bool{}, admins: map[string]bool{}, removedBy: map[string]string{}}
	for _, i := range order {
		if ps.applied.has(i) {
			st.Effects[g.e[i].ID] = Effect{Applied: true}
		} else {
			st.Effects[g.e[i].ID] = Effect{Reason: why[i]}
		}
	}
	for x, s := range g.names {
		if ps.members.has(x) {
			st.members[s] = true
		}
		if ps.admins.has(x) {
			st.admins[s] = true
		}
		if k := ps.removedBy[x]; k != "" {
			st.removedBy[s] = k
		}
	}
	st.Members = sortedKeys(st.members)
	st.Admins = sortedKeys(st.admins)
	st.Permissions = map[string]Permission{}
	name := map[string]int{}
	for x, s := range g.names {
		name[s] = x
	}
	for _, s := range st.Members {
		x := name[s]
		if x == rootAuthor && len(removals[x]) == 0 {
			st.Permissions[s] = Permission{All: true}
			continue
		}
		var grants []string
		for _, gr := range g.votesFor(in, kAdd, x) {
			if ps.applied.has(gr) && seesAll(gr, x) {
				grants = append(grants, g.e[gr].ID)
			}
		}
		for _, gr := range g.votesFor(in, kAdmin, x) {
			if ps.applied.has(gr) && seesAll(gr, x) && !g.past[gr].members.has(x) {
				grants = append(grants, g.e[gr].ID)
			}
		}
		st.Permissions[s] = permissionOf(g.valid, grants)
	}
	admitted := map[string]bool{g.names[rootAuthor]: true}
	for _, i := range order[1:] {
		if k := g.e[i].Kind; (k == kAdd || k == kAdmin) && ps.applied.has(i) {
			admitted[g.e[i].Target] = true
		}
	}
	st.Admitted = sortedKeys(admitted)
	return ps, st
}

// seesEvent reports whether entry i had seen enough of a removal's votes
// to complete it.
func (g *graph) seesEvent(i int, ev *removalEvent) bool {
	var seen []int
	for _, v := range ev.votes {
		if g.anc[i].has(v) {
			seen = append(seen, v)
		}
	}
	if !ev.admin {
		return len(seen) == 1
	}
	return g.evQuorum(ev, seen, g.forkSeen(ev.forkKey, func(x int) bool { return g.anc[i].has(x) }))
}

// laterThan reports whether a removal happened after entry i: enough of
// its votes had seen i to complete it.
func (g *graph) laterThan(ev *removalEvent, i int) bool {
	var after []int
	for _, v := range ev.votes {
		if g.anc[v].has(i) {
			after = append(after, v)
		}
	}
	if !ev.admin {
		return len(after) == 1
	}
	return g.evQuorum(ev, after, g.forkSeen(ev.forkKey, func(x int) bool { return g.anc[x].has(i) }))
}

// grantReached reports whether admin vote v, with the other eligible votes
// made after seeing the same removals of the target, reached the
// threshold. A promotion later undone by a removal still had an effect.
func (g *graph) grantReached(v int, votes []int, eligible bitset, removals []*removalEvent) bool {
	seen := func(i int) string {
		var out []string
		for _, ev := range removals {
			if g.seesEvent(i, ev) {
				out = append(out, ev.key)
			}
		}
		return strings.Join(out, ",")
	}
	var group []int
	for _, w := range votes {
		if eligible.has(w) && g.seesEarlierRemovals(w, removals) && seen(w) == seen(v) {
			group = append(group, w)
		}
	}
	return g.quorum(group)
}

// seesEarlierRemovals reports whether entry i had seen every removal that
// is not itself later than i. A grant undone by a later removal still had
// an effect; one beaten by a concurrent removal did not.
func (g *graph) seesEarlierRemovals(i int, removals []*removalEvent) bool {
	for _, ev := range removals {
		if !g.seesEvent(i, ev) && !g.laterThan(ev, i) {
			return false
		}
	}
	return true
}

// genuine reports whether u's admin role, as entry i saw it, rests on
// grants that themselves had effect. Only removals of u that had taken
// effect in what i had seen matter here: a removal still waiting for a
// second admin when i was signed does not reach back and void i.
func (g *graph) genuine(in bitset, u, i, rootAuthor int, byKey map[string]*removalEvent, eligible bitset) bool {
	var seen []*removalEvent
	if s := g.past[i].removedBy[u]; s != "" {
		for _, k := range strings.Split(s, ",") {
			seen = append(seen, byKey[k])
		}
	}
	if u == rootAuthor && len(seen) == 0 {
		return true
	}
	var gs []int
	for _, x := range g.votesFor(in, kAdmin, u) {
		if !eligible.has(x) || !g.anc[i].has(x) {
			continue
		}
		ok := true
		for _, ev := range seen {
			ok = ok && ev != nil && g.seesEvent(x, ev)
		}
		if ok {
			gs = append(gs, x)
		}
	}
	return g.quorum(gs)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// permissionOf merges the grants' history permissions: the more
// restrictive applies.
func permissionOf(es map[string]entry, grants []string) Permission {
	sort.Strings(grants)
	p := Permission{Grants: grants, All: len(grants) > 0, Closed: len(grants) == 0}
	heads := map[string]map[string]bool{}
	for _, g := range grants {
		if es[g].History {
			continue
		}
		p.All = false
		for a, ids := range es[g].From {
			if heads[a] == nil {
				heads[a] = map[string]bool{}
			}
			for _, id := range ids {
				heads[a][id] = true
			}
		}
	}
	if len(heads) > 0 {
		p.From = map[string][]string{}
		for a, ids := range heads {
			p.From[a] = sortedKeys(ids)
		}
	}
	return p
}

// roomOf is the signed room of the entry with id root in es.
func roomOf(es []entry, root string) string {
	for _, e := range es {
		if e.ID == root {
			return e.room
		}
	}
	return ""
}
