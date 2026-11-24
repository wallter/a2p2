package members

import (
	"crypto/sha256"
	"encoding/hex"
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
	ReasonInvalidParent = "an entry it had seen is itself invalid"
	ReasonNoRoot        = "the set does not contain exactly one create entry"
	ReasonNotAdmin      = "its author was not an admin in what it had seen"
	ReasonAlreadyMember = "the machine was already a member in what its author had seen"
	ReasonAlreadyAdmin  = "the machine was already an admin in what its author had seen"
	ReasonNotMember     = "the machine was not a member in what its author had seen"
	ReasonNeedsSecond   = "changing the admin set needs a matching entry from another admin"
	ReasonAfterCut      = "its author was removed as an admin, and no remover had seen this grant"
	ReasonFork          = "its author signed two different entries at one position, and no removal has settled which one stands"
	ReasonAuthorVoid    = "its author's own admin role had no effect"
	ReasonRemovalWins   = "a removal of the same machine that it had not seen takes precedence"
	ReasonSelfVote      = "a machine's vote on its own removal does not count"
)

// State is what a set of entries means: who is a member, who is an admin,
// and for every entry whether it had an effect.
type State struct {
	Members []string
	Admins  []string
	Effects map[string]Effect

	members, admins map[string]bool
	// removedBy is, per machine, the ids of the removals of it that took
	// effect, joined. Two removal votes for an admin combine only if their
	// authors had seen the same removals of it, so an old vote cannot be
	// reused after the machine was added back.
	removedBy map[string]string
}

// Rules are the knobs that separate candidate rule sets. Final is the rule
// set this model ends with; Lead is the lead's starting candidate, kept so
// the tests can show where it fails.
type Rules struct {
	// SoloLimit: while the room has at most this many admins (as seen by
	// the author), one admin alone may change the admin set. Above it, two
	// distinct admins must sign matching entries.
	SoloLimit int
	// TargetJudgedInOwnPast: a removal is a member removal (one admin is
	// enough) or an admin removal (needs the threshold) judged only by what
	// its author had seen. Final rules also treat it as an admin removal
	// when the target has been made an admin concurrently.
	TargetJudgedInOwnPast bool
}

var (
	// Final: one admin alone may change the admin set only while it is the
	// room's only admin (T9 as written).
	Final = Rules{SoloLimit: 1}
	// Lead: candidate rules C1-C7 from the brief.
	Lead = Rules{SoloLimit: 2, TargetJudgedInOwnPast: true}
)

// Resolve applies the Final rules. It is a pure function of the set of
// entries: order and duplicates in the slice do not matter.
func Resolve(entries []Entry) State { return Final.Resolve(entries) }

// Resolve computes the state of a room from a set of entries.
func (r Rules) Resolve(entries []Entry) State { return r.resolveShared(entries, map[string]*State{}) }

// resolveShared is Resolve with a cache of the states of causally closed
// subsets. The state of such a subset depends only on its own entries, so
// the exhaustive tests share one cache across calls to save time.
func (r Rules) resolveShared(entries []Entry, memo map[string]*State) State {
	effects := map[string]Effect{}
	valid := validate(entries, effects)
	rs := &resolver{rules: r, e: valid, anc: map[string]map[string]bool{}, memo: memo}
	for id := range valid {
		rs.ancestors(id)
	}
	st := rs.resolve(sortedKeys(valid))
	out := *st
	out.Effects = effects
	for id, ef := range st.Effects {
		out.Effects[id] = ef
	}
	return out
}

// validate checks each entry's form and chain and keeps the valid ones.
// Every entry it drops gets a reason.
func validate(entries []Entry, eff map[string]Effect) map[string]Entry {
	byID := map[string]Entry{}
	for _, e := range entries {
		switch {
		case e.ID != e.hash():
			eff[e.ID] = Effect{Reason: ReasonBadID}
		case malformed(e):
			eff[e.ID] = Effect{Reason: ReasonMalformed}
		default:
			byID[e.ID] = e
		}
	}
	// An entry is valid if every entry it had seen is valid and its previous
	// entry is its author's previous entry.
	valid := map[string]Entry{}
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
		if e.Kind == Create {
			roots++
		}
	}
	if roots != 1 {
		for id := range valid {
			eff[id] = Effect{Reason: ReasonNoRoot}
		}
		return map[string]Entry{}
	}
	return valid
}

func malformed(e Entry) bool {
	if e.Author == "" {
		return true
	}
	if e.Kind == Create {
		return e.Seq != 1 || e.Prev != "" || len(e.Parents) != 0 || e.Target != ""
	}
	if e.Kind != Add && e.Kind != Remove && e.Kind != MakeAdmin {
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

type resolver struct {
	rules Rules
	e     map[string]Entry
	anc   map[string]map[string]bool // strict ancestors of each entry
	memo  map[string]*State
}

func (rs *resolver) ancestors(id string) map[string]bool {
	if a, ok := rs.anc[id]; ok {
		return a
	}
	a := map[string]bool{}
	for _, p := range rs.e[id].Parents {
		a[p] = true
		for q := range rs.ancestors(p) {
			a[q] = true
		}
	}
	rs.anc[id] = a
	return a
}

// quorum reports whether votes come from enough distinct admins. Each vote
// carries the threshold its author saw; the lowest one applies, so a vote
// made while its author was the only admin stands alone.
func quorum(rs *resolver, votes []string, thr map[string]int) bool {
	if len(votes) == 0 {
		return false
	}
	authors := map[string]bool{}
	need := 2
	for _, v := range votes {
		authors[rs.e[v].Author] = true
		if thr[v] < need {
			need = thr[v]
		}
	}
	return len(authors) >= need
}

// evQuorum is quorum for a removal's votes, counting the target's fork
// evidence as its vote when withEvidence is set. At least one real vote is
// needed: evidence alone never removes anyone.
func evQuorum(rs *resolver, ev *removalEvent, votes []string, thr map[string]int, withEvidence bool) bool {
	if len(votes) == 0 {
		return false
	}
	authors := map[string]bool{}
	need := 2
	for _, v := range votes {
		authors[rs.e[v].Author] = true
		if thr[v] < need {
			need = thr[v]
		}
	}
	if withEvidence {
		authors[ev.target] = true
	}
	return len(authors) >= need
}

// removalEvent is one removal that took effect: a single entry for a
// machine that is not an admin, or the matching votes that removed an
// admin. An entry has seen the event once it has seen enough of its votes
// to complete it, so a late extra vote changes nothing.
type removalEvent struct {
	key    string // stable: the same in every set that contains the event
	target string
	admin  bool
	votes  []string
	kept   map[string]bool // the target's entries that stand: those a voter had seen
	// evidence is a fork by the target in the period this removal ends:
	// two different entries it signed at one position. It counts as the
	// target's own vote, from any entry that has seen both.
	evidence [2]string
}

// resolve computes the state of a causally closed set of valid entries. It
// calls itself on the causal past of each entry, which is always smaller.
func (rs *resolver) resolve(ids []string) *State {
	key := strings.Join(ids, ",")
	if s, ok := rs.memo[key]; ok {
		return s
	}
	in := map[string]bool{}
	for _, id := range ids {
		in[id] = true
	}
	order := append([]string(nil), ids...)
	sort.Slice(order, func(i, j int) bool {
		a, b := len(rs.anc[order[i]]), len(rs.anc[order[j]])
		if a != b {
			return a < b
		}
		return order[i] < order[j]
	})
	st := &State{Effects: map[string]Effect{}, members: map[string]bool{}, admins: map[string]bool{}, removedBy: map[string]string{}}
	rs.memo[key] = st
	if len(order) == 0 {
		return st
	}
	root := rs.e[order[0]]
	st.Effects[root.ID] = Effect{Applied: true}

	// Step 1. Judge each entry against what its author had seen: was the
	// author an admin, and did the change make sense there?
	prefix := map[string]*State{}
	thr := map[string]int{}
	authed := map[string]bool{}
	for _, id := range order[1:] {
		e := rs.e[id]
		p := rs.resolve(sortedKeys(rs.anc[id]))
		prefix[id] = p
		thr[id] = 2
		if len(p.admins) <= rs.rules.SoloLimit {
			thr[id] = 1
		}
		switch {
		case !p.admins[e.Author]:
			st.Effects[id] = Effect{Reason: ReasonNotAdmin}
		case e.Kind == Add && p.members[e.Target]:
			st.Effects[id] = Effect{Reason: ReasonAlreadyMember}
		case e.Kind == MakeAdmin && p.admins[e.Target]:
			st.Effects[id] = Effect{Reason: ReasonAlreadyAdmin}
		case e.Kind == Remove && !p.members[e.Target]:
			st.Effects[id] = Effect{Reason: ReasonNotMember}
		case e.Kind == Remove && e.Target == e.Author:
			st.Effects[id] = Effect{Reason: ReasonSelfVote}
		default:
			authed[id] = true
		}
	}
	// Fork evidence: two different entries by one key at one position, at
	// least one signed while it was an admin. It counts as that
	// admin's vote for its own removal in that period (rule 12).
	evidence := map[string][2]string{}
	{
		pos := map[string]string{}
		for _, id := range order[1:] {
			e := rs.e[id]
			k := e.Author + "\x00" + strconv.Itoa(e.Seq)
			other, ok := pos[k]
			if !ok {
				pos[k] = id
				continue
			}
			period := prefix[id].removedBy[e.Author]
			if other == id || !prefix[id].admins[e.Author] && !prefix[other].admins[e.Author] {
				continue
			}
			ek := e.Author + "\x00" + period
			pair := [2]string{other, id}
			if pair[1] < pair[0] {
				pair = [2]string{id, other}
			}
			if cur, ok := evidence[ek]; !ok || e.Seq < rs.e[cur[0]].Seq || e.Seq == rs.e[cur[0]].Seq && pair[0]+pair[1] < cur[0]+cur[1] {
				evidence[ek] = pair
			}
		}
	}
	byChange := map[string][]string{}
	for _, id := range order {
		if e := rs.e[id]; authed[id] {
			k := string(e.Kind) + "\x00" + e.Target
			byChange[k] = append(byChange[k], id)
		}
	}
	votesFor := func(kind Kind, target string) []string { return byChange[string(kind)+"\x00"+target] }

	// Step 2. Removals. A removal counts if its author was an admin in what
	// it had seen; removals are never cancelled by later knowledge. Removing
	// an admin needs the threshold; so does removing a machine that was
	// made an admin concurrently, otherwise a key that had not seen the
	// promotion could remove the new admin alone.
	applied := map[string]bool{}
	removals := map[string][]*removalEvent{}
	byKey := map[string]*removalEvent{}
	addEvent := func(ev *removalEvent) {
		removals[ev.target] = append(removals[ev.target], ev)
		byKey[ev.key] = ev
		for _, v := range ev.votes {
			applied[v] = true
			// A removal seen in an entry's past as a plain member's may
			// count here as an admin's, when a concurrent promotion of the
			// target is in this set: the vote finds its event either way.
			if _, ok := byKey[v]; !ok {
				byKey[v] = ev
			}
		}
	}
	// A removal signed by a forked key at or after its fork, in the period
	// of the fork, has no effect once another admin has voted to remove
	// that key, unless some such vote had seen it take effect: the fork and
	// that vote remove the key (rule 12), and, as in rule 9, what no
	// remover had seen take effect does not stand.
	againstForker := map[string][]string{}
	for _, id := range order[1:] {
		if e := rs.e[id]; authed[id] && e.Kind == Remove {
			ek := e.Target + "\x00" + prefix[id].removedBy[e.Target]
			if _, ok := evidence[ek]; ok {
				againstForker[ek] = append(againstForker[ek], id)
			}
		}
	}
	forkedOut := func(id string) bool {
		e := rs.e[id]
		ek := e.Author + "\x00" + prefix[id].removedBy[e.Author]
		pair, ok := evidence[ek]
		if !ok || e.Seq < rs.e[pair[0]].Seq {
			return false
		}
		for _, w := range againstForker[ek] {
			if rs.anc[w][id] && prefix[w].Effects[id].Applied {
				return false
			}
		}
		return len(againstForker[ek]) > 0
	}
	groups := map[string][]string{}
	var groupOrder []string
	for _, id := range order[1:] {
		e := rs.e[id]
		if !authed[id] || e.Kind != Remove {
			continue
		}
		if forkedOut(id) {
			st.Effects[id] = Effect{Reason: ReasonFork}
			continue
		}
		adminRemoval := prefix[id].admins[e.Target]
		if !adminRemoval && !rs.rules.TargetJudgedInOwnPast {
			var gs []string
			concurrent := false
			for _, g := range votesFor(MakeAdmin, e.Target) {
				if !rs.anc[g][id] {
					gs = append(gs, g)
					concurrent = concurrent || !rs.anc[id][g]
				}
			}
			adminRemoval = concurrent && quorum(rs, gs, thr)
		}
		if !adminRemoval {
			addEvent(&removalEvent{key: id, target: e.Target, votes: []string{id}})
			continue
		}
		k := e.Target + "\x00" + prefix[id].removedBy[e.Target]
		if _, ok := groups[k]; !ok {
			groupOrder = append(groupOrder, k)
		}
		groups[k] = append(groups[k], id)
	}
	for _, k := range groupOrder {
		vs := groups[k]
		target := rs.e[vs[0]].Target
		sum := sha256.Sum256([]byte(k))
		ev := &removalEvent{key: "r" + hex.EncodeToString(sum[:8]), target: target, admin: true, votes: vs, kept: map[string]bool{}, evidence: evidence[k]}
		if !evQuorum(rs, ev, vs, thr, ev.evidence[0] != "") {
			for _, v := range vs {
				st.Effects[v] = Effect{Reason: ReasonNeedsSecond}
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
		atSeq := map[int]string{}
		forkFrom := 0
		for _, v := range vs {
			for x := range rs.anc[v] {
				if rs.e[x].Author != target || !in[x] || !prefix[v].Effects[x].Applied {
					continue
				}
				ev.kept[x] = true
				q := rs.e[x].Seq
				if other, ok := atSeq[q]; ok && other != x && (forkFrom == 0 || q < forkFrom) {
					forkFrom = q
				}
				atSeq[q] = x
			}
		}
		if forkFrom > 0 {
			for x := range ev.kept {
				if rs.e[x].Seq >= forkFrom {
					delete(ev.kept, x)
				}
			}
		}
		addEvent(ev)
	}

	// Step 3. Forks: two different entries by one author at one position,
	// signed while it was an admin (a fork by a key that was not an admin
	// signs nothing with effect, so it changes nothing).
	// A fork belongs to the period of membership in which it was signed: an
	// entry that has seen a removal of its author that the fork had not
	// seen is past the fork.
	type forkMark struct {
		seq    int
		period string
	}
	forkAt := map[string]forkMark{}
	pos := map[string]string{}
	for _, id := range order[1:] {
		e := rs.e[id]
		k := e.Author + "\x00" + strconv.Itoa(e.Seq)
		if other, ok := pos[k]; ok && other != id && (prefix[id].admins[e.Author] || prefix[other].admins[e.Author]) {
			if f, ok := forkAt[e.Author]; !ok || e.Seq < f.seq {
				forkAt[e.Author] = forkMark{e.Seq, prefix[id].removedBy[e.Author]}
			}
		}
		pos[k] = id
	}

	// Step 4. Grants (add, make admin), in causal order. A grant has effect
	// only if its author's admin role itself had effect, some remover had
	// seen it in every removal of its author, and its author has not forked
	// at or before it without a removal settling the fork.
	//
	// A grant by a removed admin has no effect if, for some removal of that
	// admin, no remover had seen it and it had not seen the removal. A
	// later removal whose removers have seen it does not revive it.
	eligible := map[string]bool{}
	for _, id := range order[1:] {
		e := rs.e[id]
		if !authed[id] || e.Kind == Remove {
			continue
		}
		period := prefix[id].removedBy[e.Author]
		afterCut, keptByCut := false, false
		for _, ev := range removals[e.Author] {
			if !ev.admin {
				continue
			}
			if ev.kept[id] {
				keptByCut = true
			} else if !rs.seesEvent(id, ev, thr) {
				afterCut = true
			}
		}
		f, forked := forkAt[e.Author]
		switch {
		case afterCut:
			st.Effects[id] = Effect{Reason: ReasonAfterCut}
		case forked && f.period == period && e.Seq >= f.seq && !keptByCut:
			st.Effects[id] = Effect{Reason: ReasonFork}
		case !rs.genuine(e.Author, id, root, prefix[id], byKey, eligible, thr, votesFor):
			st.Effects[id] = Effect{Reason: ReasonAuthorVoid}
		default:
			eligible[id] = true
		}
	}

	// Step 5. The result. A machine is a member if some grant of it had
	// seen every removal of it that took effect: a removal beats a
	// concurrent add, and an add made with knowledge of the removal
	// re-admits.
	seesAll := func(id, target string) bool {
		for _, ev := range removals[target] {
			if !rs.seesEvent(id, ev, thr) {
				return false
			}
		}
		return true
	}
	adminVotes := func(target string) []string {
		var vs []string
		for _, v := range votesFor(MakeAdmin, target) {
			if eligible[v] && seesAll(v, target) {
				vs = append(vs, v)
			}
		}
		return vs
	}
	candidates := map[string]bool{root.Author: true}
	for _, id := range order[1:] {
		candidates[rs.e[id].Target] = true
	}
	for x := range candidates {
		admin := (x == root.Author && len(removals[x]) == 0) || quorum(rs, adminVotes(x), thr)
		member := admin
		for _, a := range votesFor(Add, x) {
			member = member || (eligible[a] && seesAll(a, x))
		}
		if admin {
			st.admins[x] = true
		}
		if member {
			st.members[x] = true
		}
		if len(removals[x]) > 0 {
			var keys []string
			for _, ev := range removals[x] {
				keys = append(keys, ev.key)
			}
			sort.Strings(keys)
			st.removedBy[x] = strings.Join(keys, ",")
		}
	}
	for _, id := range order[1:] {
		e := rs.e[id]
		switch {
		case applied[id]:
			st.Effects[id] = Effect{Applied: true}
		case !eligible[id]:
			// reason already recorded
		case !rs.seesEarlierRemovals(id, removals[e.Target], thr):
			st.Effects[id] = Effect{Reason: ReasonRemovalWins}
		case e.Kind == MakeAdmin && !rs.grantReached(id, votesFor(MakeAdmin, e.Target), eligible, removals[e.Target], thr):
			st.Effects[id] = Effect{Reason: ReasonNeedsSecond}
		default:
			st.Effects[id] = Effect{Applied: true}
		}
	}
	st.Members = sortedKeys(st.members)
	st.Admins = sortedKeys(st.admins)
	return st
}

// seesEvent reports whether entry id had seen enough of a removal's votes
// to complete it.
func (rs *resolver) seesEvent(id string, ev *removalEvent, thr map[string]int) bool {
	var seen []string
	for _, v := range ev.votes {
		if rs.anc[id][v] {
			seen = append(seen, v)
		}
	}
	if !ev.admin {
		return len(seen) == 1
	}
	return evQuorum(rs, ev, seen, thr, ev.evidence[0] != "" && rs.anc[id][ev.evidence[0]] && rs.anc[id][ev.evidence[1]])
}

// laterThan reports whether a removal happened after entry id: enough of
// its votes had seen id to complete it.
func (rs *resolver) laterThan(ev *removalEvent, id string, thr map[string]int) bool {
	var after []string
	for _, v := range ev.votes {
		if rs.anc[v][id] {
			after = append(after, v)
		}
	}
	if !ev.admin {
		return len(after) == 1
	}
	return evQuorum(rs, ev, after, thr, ev.evidence[0] != "" && rs.anc[ev.evidence[0]][id] && rs.anc[ev.evidence[1]][id])
}

// grantReached reports whether admin vote v, with the other eligible votes
// made after seeing the same removals of the target, reached the
// threshold. A promotion later undone by a removal still had an effect.
func (rs *resolver) grantReached(v string, votes []string, eligible map[string]bool, removals []*removalEvent, thr map[string]int) bool {
	seen := func(id string) string {
		var out []string
		for _, ev := range removals {
			if rs.seesEvent(id, ev, thr) {
				out = append(out, ev.key)
			}
		}
		return strings.Join(out, ",")
	}
	var group []string
	for _, w := range votes {
		if eligible[w] && rs.seesEarlierRemovals(w, removals, thr) && seen(w) == seen(v) {
			group = append(group, w)
		}
	}
	return quorum(rs, group, thr)
}

// seesEarlierRemovals reports whether entry id had seen every removal that
// is not itself later than id. A grant undone by a later removal still had
// an effect; one beaten by a concurrent removal did not.
func (rs *resolver) seesEarlierRemovals(id string, removals []*removalEvent, thr map[string]int) bool {
	for _, ev := range removals {
		if !rs.seesEvent(id, ev, thr) && !rs.laterThan(ev, id, thr) {
			return false
		}
	}
	return true
}

// genuine reports whether u's admin role, as entry id saw it, rests on
// grants that themselves had effect. Only removals of u that had taken
// effect in what id had seen matter here: a removal still waiting for a
// second admin when id was signed does not reach back and void id.
func (rs *resolver) genuine(u, id string, root Entry, past *State, byKey map[string]*removalEvent, eligible map[string]bool, thr map[string]int, votesFor func(Kind, string) []string) bool {
	var seen []*removalEvent
	if s := past.removedBy[u]; s != "" {
		for _, k := range strings.Split(s, ",") {
			seen = append(seen, byKey[k])
		}
	}
	if u == root.Author && len(seen) == 0 {
		return true
	}
	var gs []string
	for _, g := range votesFor(MakeAdmin, u) {
		if !eligible[g] || !rs.anc[id][g] {
			continue
		}
		ok := true
		for _, ev := range seen {
			ok = ok && ev != nil && rs.seesEvent(g, ev, thr)
		}
		if ok {
			gs = append(gs, g)
		}
	}
	return quorum(rs, gs, thr)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
