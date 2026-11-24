package members

// The resolver as it was before it was rewritten for speed (integer
// indices and bitsets), kept as a test-only oracle: the rewrite must give
// the same state, effects included, on every history the tests build.
// It is the rule text in its most direct form.

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"sort"
	"strings"
)

func (r ruleSet) refResolveRoom(root string, entries []entry) State {
	return r.refResolveShared(entries, map[string]*State{}, root)
}

// refResolveShared is Resolve with a cache of the states of causally closed
// subsets. The state of such a subset depends only on its own entries, so
// the exhaustive tests share one cache across calls to save time.
func (r ruleSet) refResolveShared(entries []entry, memo map[string]*State, root string) State {
	effects := map[string]Effect{}
	valid := validate(entries, effects, root, r.idOf)
	rs := &refResolver{rules: r, e: valid, anc: map[string]map[string]bool{}, memo: memo}
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

// refForkEntry is one entry at a position where its author signed more than
// one entry, and whether its author was an admin in its past.
type refForkEntry struct {
	id    string
	admin bool
}

// refQualifies reports whether the picked entries of one position are fork
// evidence: two or more, at least one signed by an admin.
func refQualifies(fs []refForkEntry, pick func(string) bool) bool {
	n, admin := 0, false
	for _, f := range fs {
		if pick(f.id) {
			n++
			admin = admin || f.admin
		}
	}
	return n >= 2 && admin
}

// forkSeen reports whether the entries pick selects hold fork evidence for
// key ek (author and period) at some position.
func (rs *refResolver) forkSeen(ek string, pick func(string) bool) bool {
	for _, fs := range rs.forks[ek] {
		if refQualifies(fs, pick) {
			return true
		}
	}
	return false
}

type refResolver struct {
	forks map[string]map[int64][]refForkEntry // of the set being resolved
	rules ruleSet
	e     map[string]entry
	anc   map[string]map[string]bool // strict ancestors of each entry
	memo  map[string]*State
}

func (rs *refResolver) ancestors(id string) map[string]bool {
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

// refQuorum reports whether votes come from enough distinct admins. Each vote
// carries the threshold its author saw; the lowest one applies, so a vote
// made while its author was the only admin stands alone.
func refQuorum(rs *refResolver, votes []string, thr map[string]int) bool {
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

// refEvQuorum is refQuorum for a removal's votes, counting the target's fork
// evidence as its vote when withEvidence is set. At least one real vote is
// needed: evidence alone never removes anyone.
func refEvQuorum(rs *refResolver, ev *refRemovalEvent, votes []string, thr map[string]int, withEvidence bool) bool {
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

// refRemovalEvent is one removal that took effect: a single entry for a
// machine that is not an admin, or the matching votes that removed an
// admin. An entry has seen the event once it has seen enough of its votes
// to complete it, so a late extra vote changes nothing.
type refRemovalEvent struct {
	key    string // stable: the same in every set that contains the event
	target string
	admin  bool
	votes  []string
	kept   map[string]bool // the target's entries that stand: those a voter had seen
	// forkKey names the target and the period this removal ends: fork
	// evidence by the target in that period counts as its own vote, from
	// any entry whose past holds it.
	forkKey string
}

// resolve computes the state of a causally closed set of valid entries. It
// calls itself on the causal past of each entry, which is always smaller.
func (rs *refResolver) resolve(ids []string) *State {
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
		case e.Kind == kAdd && p.members[e.Target]:
			st.Effects[id] = Effect{Reason: ReasonAlreadyMember}
		case e.Kind == kAdmin && p.admins[e.Target]:
			st.Effects[id] = Effect{Reason: ReasonAlreadyAdmin}
		case e.Kind == kRemove && !p.members[e.Target]:
			st.Effects[id] = Effect{Reason: ReasonNotMember}
		case e.Kind == kRemove && e.Target == e.Author:
			st.Effects[id] = Effect{Reason: ReasonSelfVote}
		default:
			authed[id] = true
		}
	}
	// Fork evidence: two different entries by one key at one position, in
	// one period of its membership, at least one signed while it was an
	// admin. It counts as that admin's vote for its own removal in that
	// period (rule 12). Whether an entry has seen it is a property of the
	// entry's own past: does that past hold such two entries.
	forks := map[string]map[int64][]refForkEntry{}
	for _, id := range order[1:] {
		e := rs.e[id]
		ek := e.Author + "\x00" + prefix[id].removedBy[e.Author]
		if forks[ek] == nil {
			forks[ek] = map[int64][]refForkEntry{}
		}
		forks[ek][e.Seq] = append(forks[ek][e.Seq], refForkEntry{id, prefix[id].admins[e.Author]})
	}
	for ek, at := range forks {
		for q, fs := range at {
			if len(fs) < 2 {
				delete(at, q)
			}
		}
		if len(at) == 0 {
			delete(forks, ek)
		}
	}
	saved := rs.forks
	rs.forks = forks
	defer func() { rs.forks = saved }()
	// forkFrom is the first position at which the whole set holds evidence
	// against a key in a period, 0 if none.
	forkFrom := func(ek string) int64 {
		var first int64
		for q, fs := range forks[ek] {
			if refQualifies(fs, func(string) bool { return true }) && (first == 0 || q < first) {
				first = q
			}
		}
		return first
	}
	byChange := map[string][]string{}
	for _, id := range order {
		if e := rs.e[id]; authed[id] {
			k := string(e.Kind) + "\x00" + e.Target
			byChange[k] = append(byChange[k], id)
		}
	}
	votesFor := func(kind entryKind, target string) []string { return byChange[string(kind)+"\x00"+target] }

	// Step 2. Removals. A removal counts if its author was an admin in what
	// it had seen; removals are never cancelled by later knowledge. Removing
	// an admin needs the threshold; so does removing a machine that was
	// made an admin concurrently, otherwise a key that had not seen the
	// promotion could remove the new admin alone.
	applied := map[string]bool{}
	removals := map[string][]*refRemovalEvent{}
	byKey := map[string]*refRemovalEvent{}
	addEvent := func(ev *refRemovalEvent) {
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
		if e := rs.e[id]; authed[id] && e.Kind == kRemove {
			ek := e.Target + "\x00" + prefix[id].removedBy[e.Target]
			if forkFrom(ek) > 0 {
				againstForker[ek] = append(againstForker[ek], id)
			}
		}
	}
	forkedOut := func(id string) bool {
		e := rs.e[id]
		ek := e.Author + "\x00" + prefix[id].removedBy[e.Author]
		if q := forkFrom(ek); q == 0 || e.Seq < q {
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
		if !authed[id] || e.Kind != kRemove {
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
			for _, g := range votesFor(kAdmin, e.Target) {
				if !rs.anc[g][id] {
					gs = append(gs, g)
					concurrent = concurrent || !rs.anc[id][g]
				}
			}
			adminRemoval = concurrent && refQuorum(rs, gs, thr)
		}
		if !adminRemoval {
			addEvent(&refRemovalEvent{key: id, target: e.Target, votes: []string{id}})
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
		ev := &refRemovalEvent{key: "r" + hex.EncodeToString(sum[:8]), target: target, admin: true, votes: vs, kept: map[string]bool{}, forkKey: k}
		if !refEvQuorum(rs, ev, vs, thr, forkFrom(k) > 0) {
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
		atSeq := map[int64]string{}
		forkFrom := int64(0)
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
	forkAt := map[string]int64{} // author and period: first forked position
	for ek := range forks {
		if q := forkFrom(ek); q > 0 {
			forkAt[ek] = q
		}
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
		if !authed[id] || e.Kind == kRemove {
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
		fq, forked := forkAt[e.Author+"\x00"+period]
		switch {
		case afterCut:
			st.Effects[id] = Effect{Reason: ReasonAfterCut}
		case forked && e.Seq >= fq && !keptByCut:
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
		for _, v := range votesFor(kAdmin, target) {
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
		admin := (x == root.Author && len(removals[x]) == 0) || refQuorum(rs, adminVotes(x), thr)
		member := admin
		for _, a := range votesFor(kAdd, x) {
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
		case e.Kind == kAdmin && !rs.grantReached(id, votesFor(kAdmin, e.Target), eligible, removals[e.Target], thr):
			st.Effects[id] = Effect{Reason: ReasonNeedsSecond}
		default:
			st.Effects[id] = Effect{Applied: true}
		}
	}
	st.Members = sortedKeys(st.members)
	st.Admins = sortedKeys(st.admins)
	st.Permissions = map[string]Permission{}
	for _, x := range st.Members {
		if x == root.Author && len(removals[x]) == 0 {
			st.Permissions[x] = Permission{All: true}
			continue
		}
		var grants []string
		for _, g := range votesFor(kAdd, x) {
			if st.Effects[g].Applied && seesAll(g, x) {
				grants = append(grants, g)
			}
		}
		for _, g := range votesFor(kAdmin, x) {
			if st.Effects[g].Applied && seesAll(g, x) && !prefix[g].members[x] {
				grants = append(grants, g)
			}
		}
		st.Permissions[x] = permissionOf(rs.e, grants)
	}
	admitted := map[string]bool{root.Author: true}
	for _, id := range order[1:] {
		if e := rs.e[id]; (e.Kind == kAdd || e.Kind == kAdmin) && st.Effects[id].Applied {
			admitted[e.Target] = true
		}
	}
	st.Admitted = sortedKeys(admitted)
	return st
}

// seesEvent reports whether entry id had seen enough of a removal's votes
// to complete it.
func (rs *refResolver) seesEvent(id string, ev *refRemovalEvent, thr map[string]int) bool {
	var seen []string
	for _, v := range ev.votes {
		if rs.anc[id][v] {
			seen = append(seen, v)
		}
	}
	if !ev.admin {
		return len(seen) == 1
	}
	return refEvQuorum(rs, ev, seen, thr, rs.forkSeen(ev.forkKey, func(x string) bool { return rs.anc[id][x] }))
}

// laterThan reports whether a removal happened after entry id: enough of
// its votes had seen id to complete it.
func (rs *refResolver) laterThan(ev *refRemovalEvent, id string, thr map[string]int) bool {
	var after []string
	for _, v := range ev.votes {
		if rs.anc[v][id] {
			after = append(after, v)
		}
	}
	if !ev.admin {
		return len(after) == 1
	}
	return refEvQuorum(rs, ev, after, thr, rs.forkSeen(ev.forkKey, func(x string) bool { return rs.anc[x][id] }))
}

// grantReached reports whether admin vote v, with the other eligible votes
// made after seeing the same removals of the target, reached the
// threshold. A promotion later undone by a removal still had an effect.
func (rs *refResolver) grantReached(v string, votes []string, eligible map[string]bool, removals []*refRemovalEvent, thr map[string]int) bool {
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
	return refQuorum(rs, group, thr)
}

// seesEarlierRemovals reports whether entry id had seen every removal that
// is not itself later than id. A grant undone by a later removal still had
// an effect; one beaten by a concurrent removal did not.
func (rs *refResolver) seesEarlierRemovals(id string, removals []*refRemovalEvent, thr map[string]int) bool {
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
func (rs *refResolver) genuine(u, id string, root entry, past *State, byKey map[string]*refRemovalEvent, eligible map[string]bool, thr map[string]int, votesFor func(entryKind, string) []string) bool {
	var seen []*refRemovalEvent
	if s := past.removedBy[u]; s != "" {
		for _, k := range strings.Split(s, ",") {
			seen = append(seen, byKey[k])
		}
	}
	if u == root.Author && len(seen) == 0 {
		return true
	}
	var gs []string
	for _, g := range votesFor(kAdmin, u) {
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
	return refQuorum(rs, gs, thr)
}

// matchesReference reports whether the resolver and the reference give the
// same state for es, under rules r, effects and periods included.
func matchesReference(r ruleSet, es []entry) bool {
	root := rootOf(es)
	a, b := r.resolveRoom(root, es), r.refResolveRoom(root, es)
	return sameState(a, b) && reflect.DeepEqual(a.removedBy, b.removedBy) && reflect.DeepEqual(a.admins, b.admins)
}
