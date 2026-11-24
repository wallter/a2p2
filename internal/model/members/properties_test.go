package members

import (
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The exhaustive checks generate every honest history up to a bound: four
// machines (A creates the room), each new entry signed by a machine that is
// an admin in what it had seen, having seen any causally closed subset of
// earlier entries that includes its own previous entry. That covers every
// partition and every late arrival. A2P2_MODEL_DEEP=1 raises the bound.

var machinesUnderTest = []string{"A", "B", "C", "D"}

func deep() bool { return os.Getenv("A2P2_MODEL_DEEP") == "1" }

type history struct {
	es   []Entry // in creation order
	key  string
	last map[string]Entry
}

func (h history) ids() []string {
	out := make([]string, len(h.es))
	for i, e := range h.es {
		out[i] = e.ID
	}
	return out
}

func (h history) with(e Entry) history {
	n := history{es: append(append([]Entry(nil), h.es...), e), last: map[string]Entry{}}
	for k, v := range h.last {
		n.last[k] = v
	}
	n.last[e.Author] = e
	ids := n.ids()
	sort.Strings(ids)
	n.key = strings.Join(ids, ",")
	return n
}

// downsets returns every causally closed subset of es that contains must
// (if not empty) and the create entry.
func downsets(es []Entry, must string) [][]Entry {
	idx := map[string]int{}
	for i, e := range es {
		idx[e.ID] = i
	}
	need := make([]uint32, len(es))
	for i, e := range es {
		for _, p := range e.Parents {
			need[i] |= 1 << idx[p]
		}
	}
	var out [][]Entry
	for mask := uint32(1); mask < 1<<len(es); mask++ {
		if mask&1 == 0 || (must != "" && mask&(1<<idx[must]) == 0) {
			continue
		}
		ok := true
		for i := range es {
			if mask&(1<<i) != 0 && need[i]&^mask != 0 {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		var d []Entry
		for i := range es {
			if mask&(1<<i) != 0 {
				d = append(d, es[i])
			}
		}
		out = append(out, d)
	}
	return out
}

// cache shares the states of causally closed subsets across Resolve calls
// with one rule set.
type cache map[string]*State

func (c cache) resolve(rules Rules, es []Entry) State { return rules.resolveShared(es, c) }

func idsOf(es []Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}

func latestBy(es []Entry, author string) string {
	best := Entry{}
	for _, e := range es {
		if e.Author == author && e.Seq > best.Seq {
			best = e
		}
	}
	return best.ID
}

// machinesOf returns the machines the generators use for history h: the
// four base machines plus any other machine named in h.
func machinesOf(h history) []string {
	m := map[string]bool{}
	for _, x := range machinesUnderTest {
		m[x] = true
	}
	for _, e := range h.es {
		m[e.Author] = true
		if e.Target != "" {
			m[e.Target] = true
		}
	}
	return sortedKeys(m)
}

// honestNext returns every entry an honest machine could sign next: any
// view its chain allows and any sensible change there.
func honestNext(rules Rules, h history, c cache) []Entry {
	var out []Entry
	ms := machinesOf(h)
	for _, u := range ms {
		prev := h.last[u]
		for _, d := range downsets(h.es, prev.ID) {
			p := c.resolve(rules, d)
			if !p.admins[u] {
				continue
			}
			for _, x := range ms {
				if x == u {
					continue
				}
				base := Entry{Author: u, Seq: prev.Seq + 1, Prev: prev.ID, Parents: idsOf(d), Target: x}
				if !p.members[x] {
					e := base
					e.Kind = Add
					out = append(out, New(e))
				}
				if !p.admins[x] {
					e := base
					e.Kind = MakeAdmin
					out = append(out, New(e))
				}
				if p.members[x] {
					e := base
					e.Kind = Remove
					out = append(out, New(e))
				}
			}
		}
	}
	return out
}

// extend returns every distinct honest history reached from start with up
// to n more entries, grouped by number of added entries.
func extend(rules Rules, start history, n int, c cache) [][]history {
	levels := [][]history{{start}}
	for i := 0; i < n; i++ {
		seen := map[string]bool{}
		var next []history
		for _, h := range levels[i] {
			for _, e := range honestNext(rules, h, c) {
				nh := h.with(e)
				if !seen[nh.key] {
					seen[nh.key] = true
					next = append(next, nh)
				}
			}
		}
		levels = append(levels, next)
	}
	return levels
}

// honestHistories returns every distinct honest history with up to n
// entries after the create entry, grouped by length.
func honestHistories(rules Rules, n int, c cache) [][]history {
	root := New(Entry{Author: "A", Seq: 1, Kind: Create})
	return extend(rules, history{}.with(root), n, c)
}

func bound() int {
	if deep() {
		return 5
	}
	return 4
}

var histCache = map[int][][]history{}

func histories(t *testing.T) [][]history {
	n := bound()
	if h, ok := histCache[n]; ok {
		return h
	}
	h := honestHistories(Final, n, cache{})
	total := 0
	for _, l := range h {
		total += len(l)
	}
	t.Logf("%d honest histories up to %d entries after create", total, n)
	histCache[n] = h
	return h
}

func describe(es []Entry) string {
	var b strings.Builder
	name := map[string]string{}
	for i, e := range es {
		name[e.ID] = fmt.Sprintf("e%d", i)
	}
	for _, e := range es {
		var ps []string
		for _, p := range e.Parents {
			ps = append(ps, name[p])
		}
		fmt.Fprintf(&b, "  %s: %s#%d %s %s seen=%v\n", name[e.ID], e.Author, e.Seq, e.Kind, e.Target, ps)
	}
	return b.String()
}

// ancestorsOf returns the strict ancestors of every entry.
func ancestorsOf(es []Entry) map[string]map[string]bool {
	by := map[string]Entry{}
	for _, e := range es {
		by[e.ID] = e
	}
	anc := map[string]map[string]bool{}
	var walk func(string) map[string]bool
	walk = func(id string) map[string]bool {
		if a, ok := anc[id]; ok {
			return a
		}
		a := map[string]bool{}
		for _, p := range by[id].Parents {
			a[p] = true
			for q := range walk(p) {
				a[q] = true
			}
		}
		anc[id] = a
		return a
	}
	for _, e := range es {
		walk(e.ID)
	}
	return anc
}

// checkInvariants checks P2, P5 and P6 on one resolved set.
// sawFork reports whether past holds two different entries by author at
// one position.
func sawFork(es []Entry, past map[string]bool, author string) bool {
	at := map[int]string{}
	for _, e := range es {
		if e.Author != author || !past[e.ID] {
			continue
		}
		if o, ok := at[e.Seq]; ok && o != e.ID {
			return true
		}
		at[e.Seq] = e.ID
	}
	return false
}

func checkInvariants(es []Entry, st State) error {
	anc := ancestorsOf(es)
	for _, e := range es {
		ef, ok := st.Effects[e.ID]
		if !ok || (!ef.Applied && ef.Reason == "") {
			return fmt.Errorf("P6: entry %s has neither effect nor reason", e.ID)
		}
	}
	// A removal is seen by an add once the add has seen it, or has seen
	// votes from as many distinct removers in the same removal (same
	// earlier removals of the target seen) as that removal had in all: a
	// repeated vote by one remover adds nothing.
	by := map[string]Entry{}
	for _, e := range es {
		by[e.ID] = e
	}
	basis := func(id string) string {
		var past []Entry
		for p := range anc[id] {
			past = append(past, by[p])
		}
		return Resolve(past).removedBy[by[id].Target]
	}
	for _, r := range es {
		if r.Kind != Remove || !st.Effects[r.ID].Applied || !st.members[r.Target] {
			continue
		}
		var group []Entry
		authors := map[string]bool{}
		for _, v := range es {
			if v.Kind == Remove && v.Target == r.Target && st.Effects[v.ID].Applied && basis(v.ID) == basis(r.ID) {
				group = append(group, v)
				authors[v.Author] = true
			}
		}
		readmitted := false
		for _, a := range es {
			if (a.Kind != Add && a.Kind != MakeAdmin) || a.Target != r.Target || !st.Effects[a.ID].Applied {
				continue
			}
			seen := map[string]bool{}
			for _, v := range group {
				if anc[a.ID][v.ID] {
					seen[v.Author] = true
				}
			}
			// Rule 12: having seen a fork by the target counts as having
			// seen the target's own vote.
			if len(seen) > 0 && sawFork(es, anc[a.ID], r.Target) {
				seen[r.Target] = true
			}
			if anc[a.ID][r.ID] || len(seen) >= len(authors) || len(seen) >= 2 {
				readmitted = true
			}
		}
		if !readmitted {
			return fmt.Errorf("P2: %s removed by %s is a member again without an add that saw the removal", r.Target, r.ID)
		}
	}
	for _, x := range st.Admins {
		ok := false
		for _, e := range es {
			if (e.Kind == Create && e.Author == x) || (e.Kind == MakeAdmin && e.Target == x && st.Effects[e.ID].Applied) {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("P5: %s is an admin with no grant", x)
		}
	}
	for _, m := range st.Admins {
		if !st.members[m] {
			return fmt.Errorf("an admin %s is not a member", m)
		}
	}
	return nil
}

// P1, P2, P5, P6, P7 over every honest history: same set in any order
// gives the same state, removals stick, nobody is an admin without a
// grant, every entry without effect has a reason, and a fully informed
// change affects only its target.
//
// Requirements: T9, T10, T5, A4
func TestEveryHonestHistoryResolvesSameInAnyOrderAndKeepsInvariants(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	c := cache{}
	for _, level := range histories(t) {
		for _, h := range level {
			st := Resolve(h.es)
			if err := checkInvariants(h.es, st); err != nil {
				t.Fatalf("%v\n%s", err, describe(h.es))
			}
			if err := checkLocalEffect(Final, h, c); err != nil {
				t.Fatal(err)
			}
			shuffled := append([]Entry(nil), h.es...)
			rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			shuffled = append(shuffled, shuffled[0]) // a duplicate
			if got := Resolve(shuffled); !reflect.DeepEqual(got.Members, st.Members) || !reflect.DeepEqual(got.Admins, st.Admins) || !reflect.DeepEqual(got.Effects, st.Effects) {
				t.Fatalf("P1: order changed the result (seed 1)\n%s", describe(h.es))
			}
		}
	}
}

// P1 at the machine level: machines that receive the same honest entries
// in different orders accept the same set and agree.
//
// Requirements: T9, D6
func TestMachinesReceivingSameEntriesInAnyOrderAgree(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	levels := histories(t)
	orders := 2 // random delivery orders per history
	if deep() {
		orders = 5
	}
	for _, h := range levels[len(levels)-1] {
		want := Resolve(h.es)
		for k := 0; k < orders; k++ {
			es := append([]Entry(nil), h.es...)
			rng.Shuffle(len(es), func(i, j int) { es[i], es[j] = es[j], es[i] })
			m := NewMachine(Final)
			for _, e := range es {
				m.Receive(e)
			}
			if got := m.State(); len(m.Entries()) != len(h.es) || !reflect.DeepEqual(got.Members, want.Members) || !reflect.DeepEqual(got.Admins, want.Admins) {
				t.Fatalf("P1: machine disagrees (seed 2)\n%s", describe(h.es))
			}
		}
	}
}

// attackNext returns every entry key k could sign next on its own chain,
// with any parents its chain allows, any kind, any target.
func attackNext(h history, k string) []Entry {
	prev := h.last[k]
	var out []Entry
	targets := append(machinesOf(h), "X")
	for _, d := range downsets(h.es, prev.ID) {
		for _, x := range targets {
			base := Entry{Author: k, Seq: prev.Seq + 1, Prev: prev.ID, Parents: idsOf(d), Target: x}
			for _, kind := range []Kind{Add, MakeAdmin, Remove} {
				e := base
				e.Kind = kind
				out = append(out, New(e))
			}
		}
	}
	return out
}

// forkNext returns every entry key k could sign at a position it has
// already used, continuing from its entry before that position, with any
// parents that do not include its entries from that position on: a
// restored backup or a thief re-signing the key's chain.
func forkNext(h history, k string) []Entry {
	var out []Entry
	targets := append(machinesOf(h), "X")
	for _, x := range h.es {
		if x.Author != k || x.Kind == Create {
			continue
		}
		for _, d := range downsets(h.es, x.Prev) {
			ok := true
			for _, y := range d {
				if y.Author == k && y.Seq >= x.Seq {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			for _, t := range targets {
				base := Entry{Author: k, Seq: x.Seq, Prev: x.Prev, Parents: idsOf(d), Target: t}
				for _, kind := range []Kind{Add, MakeAdmin, Remove} {
					e := base
					e.Kind = kind
					if e = New(e); e.ID != x.ID {
						out = append(out, e)
					}
				}
			}
		}
	}
	return out
}

// withForks is attackNext plus forkNext.
func withForks(h history, k string) []Entry { return append(attackNext(h, k), forkNext(h, k)...) }

// p3Violation reports how extra entries by key k changed the admin set.
func p3Violation(s0, s1 State, k string) string {
	for _, y := range s0.Admins {
		if y != k && !s1.admins[y] {
			return "removed admin " + y
		}
	}
	for _, z := range s1.Admins {
		if !s0.admins[z] {
			return "new admin " + z
		}
	}
	return ""
}

// atRest reports whether no admin-set change is waiting for a second
// admin. P3 is checked from such states: a stolen key can always complete
// a change another admin has already signed,
// and that is reported as a limit rather than a violation.
//
// A grant vote beaten by a concurrent removal is also waiting: a second
// vote that claims not to have seen the removal completes it (see
// TestStolenKeyCanCompleteAPromotionBeatenByAConcurrentRemoval). So is a
// removed admin's grant vote voided by its removal: a key that seconds it
// and then joins the removal brings it back (known gap 4, see
// TestStolenKeyCanSecondAVoidedPromotionVoteAndThenJoinTheRemoval), and so
// is a forking key's removal vote voided by rule 12 (see
// TestStolenKeyCanSecondAForkingKeysVoidedRemovalAndThenJoinTheRemoval).
func atRest(h history, s State) bool {
	for _, e := range h.es {
		r := s.Effects[e.ID].Reason
		if r == ReasonNeedsSecond || (e.Kind == MakeAdmin && (r == ReasonRemovalWins || r == ReasonAfterCut)) || (e.Kind == Remove && r == ReasonFork) {
			return false
		}
	}
	return true
}

type p3Result struct {
	checked int
	forks   int
	example string
}

// searchP3 tries every attack by one key from every history at rest with
// three or more admins. A nil shared cache means one cache per history,
// which keeps memory bounded on large searches.
func searchP3(rules Rules, levels [][]history, depth int, shared cache) p3Result {
	return searchP3With(rules, levels, depth, shared, 3, attackNext, nil)
}

// searchP3With is searchP3 from rooms of at least minAdmins admins, with
// the attacker's entries drawn from next. With a non-nil rng it also
// checks P1 on every attack set: a shuffled copy resolves the same.
func searchP3With(rules Rules, levels [][]history, depth int, shared cache, minAdmins int, gen func(history, string) []Entry, rng *rand.Rand) p3Result {
	var res p3Result
	for _, level := range levels {
		for _, h := range level {
			c := shared
			if c == nil {
				c = cache{}
			}
			s0 := c.resolve(rules, h.es)
			if len(s0.Admins) < minAdmins || !atRest(h, s0) {
				continue
			}
			keys := map[string]bool{}
			for _, e := range h.es {
				keys[e.Author] = true
			}
			for _, a := range s0.Admins {
				keys[a] = true
			}
			for _, k := range sortedKeys(keys) {
				frontier := []history{h}
				for d := 0; d < depth; d++ {
					var next []history
					for _, cur := range frontier {
						for _, e := range gen(cur, k) {
							nh := cur.with(e)
							res.checked++
							s1 := c.resolve(rules, nh.es)
							if rng != nil {
								es := append([]Entry(nil), nh.es...)
								rng.Shuffle(len(es), func(i, j int) { es[i], es[j] = es[j], es[i] })
								if got := rules.Resolve(es); !reflect.DeepEqual(got.Admins, s1.Admins) || !reflect.DeepEqual(got.Members, s1.Members) || !reflect.DeepEqual(got.Effects, s1.Effects) {
									res.example = "P1: order changed the result\n" + describe(nh.es)
									return res
								}
							}
							if v := p3Violation(s0, s1, k); v != "" {
								if res.example == "" || len(nh.es) < strings.Count(res.example, "\n") {
									res.example = fmt.Sprintf("key %s: %s\n%s", k, v, describe(nh.es))
								}
							}
							if d+1 < depth {
								next = append(next, nh)
							}
						}
					}
					frontier = next
				}
			}
		}
	}
	return res
}

// P3: once a room has three admins and no admin-set change is waiting for
// a second admin, extra entries by any one key (any claimed parents its
// chain allows, any kind and target, signed at any time,
// including after the key was removed) do not remove another admin or
// create a new one. Forks are excluded here; they are refused by the local
// layer and covered by the fork tests.
//
// Requirements: T9
func TestOneKeyCannotChangeAdminSetOnceThereAreThreeAdmins(t *testing.T) {
	depth := 1
	if deep() {
		depth = 2
	}
	res := searchP3(Final, histories(t), depth, nil)
	t.Logf("P3: %d attack sets checked at depth %d", res.checked, depth)
	if res.example != "" {
		t.Fatalf("P3 violated:\n%s", res.example)
	}
}

// The lead's candidate rules fail P3: a smallest counterexample is found
// by the same search and printed.
//
// Requirements: T9
func TestLeadRulesFailOneKeyCannotChangeAdminSet(t *testing.T) {
	levels := honestHistories(Lead, 3, cache{})
	res := searchP3(Lead, levels, 1, nil)
	if res.example == "" {
		t.Fatal("expected the lead's rules to fail P3")
	}
	t.Logf("lead rules, smallest counterexample found:\n%s", res.example)
}

// P4: from any honest state with three admins, with any one admin silent
// for ever, the other two can remove it and name a third; with any admin
// silent the others can still add and remove members.
//
// Requirements: T9, T10
func TestLossOfAnyOneAdminIsSurvivable(t *testing.T) {
	c := cache{}
	for _, level := range histories(t) {
		for _, h := range level {
			if err := checkLossSurvivable(h, c); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// checkLossSurvivable is P4 for one history with exactly three admins.
func checkLossSurvivable(h history, c cache) error {
	st := c.resolve(Final, h.es)
	if len(st.Admins) != 3 {
		return nil
	}
	for _, lost := range st.Admins {
		var live []string
		for _, a := range st.Admins {
			if a != lost {
				live = append(live, a)
			}
		}
		cur := h
		sign := func(u string, kind Kind, target string) {
			prev := cur.last[u]
			cur = cur.with(New(Entry{Author: u, Seq: prev.Seq + 1, Prev: prev.ID, Parents: cur.ids(), Kind: kind, Target: target}))
		}
		sign(live[0], Add, "N")
		sign(live[0], Remove, "N")
		if s := c.resolve(Final, cur.es); s.members["N"] {
			return fmt.Errorf("P4: one admin could not remove a member\n%s", describe(cur.es))
		}
		sign(live[0], Remove, lost)
		sign(live[1], Remove, lost)
		sign(live[0], MakeAdmin, "R")
		sign(live[1], MakeAdmin, "R")
		s := c.resolve(Final, cur.es)
		if len(s.Admins) != 3 || s.members[lost] || !s.admins["R"] {
			return fmt.Errorf("P4: could not replace %s: admins %v\n%s", lost, s.Admins, describe(cur.es))
		}
	}
	return nil
}

// P6 and P1: Resolve never panics, gives the same result in any input
// order, and gives every entry an effect or a reason, on arbitrary input:
// unknown keys, missing parents, bad chains, wrong ids, several create
// entries, forks.
//
// Requirements: A4
func TestResolveGivesEveryEntryAReasonOnArbitraryInput(t *testing.T) {
	const seed = 3
	rng := rand.New(rand.NewSource(seed))
	keys := []string{"A", "B", "C", "Z", ""}
	kinds := []Kind{Create, Add, Remove, MakeAdmin, "bogus"}
	n := 3000
	if deep() {
		n = 100000
	}
	for i := 0; i < n; i++ {
		var es []Entry
		es = append(es, New(Entry{Author: "A", Seq: 1, Kind: Create}))
		for j := 0; j < 1+rng.Intn(8); j++ {
			e := Entry{Author: keys[rng.Intn(len(keys))], Seq: rng.Intn(4), Kind: kinds[rng.Intn(len(kinds))], Target: keys[rng.Intn(len(keys))]}
			for k := 0; k < 1+rng.Intn(3); k++ {
				p := es[rng.Intn(len(es))].ID
				if rng.Intn(10) == 0 {
					p = "missing"
				}
				e.Parents = append(e.Parents, p)
			}
			if rng.Intn(2) == 0 {
				e.Prev = e.Parents[0]
			}
			e = New(e)
			if rng.Intn(20) == 0 {
				e.ID = "tampered"
			}
			es = append(es, e)
		}
		st := Resolve(es)
		for _, e := range es {
			if ef, ok := st.Effects[e.ID]; !ok || (!ef.Applied && ef.Reason == "") {
				t.Fatalf("seed %d case %d: entry without effect or reason\n%s", seed, i, describe(es))
			}
		}
		rng.Shuffle(len(es), func(i, j int) { es[i], es[j] = es[j], es[i] })
		if got := Resolve(es); !reflect.DeepEqual(got.Members, st.Members) || !reflect.DeepEqual(got.Admins, st.Admins) || !reflect.DeepEqual(got.Effects, st.Effects) {
			t.Fatalf("seed %d case %d: P1: order changed the result\n%s", seed, i, describe(es))
		}
	}
}

// historyOf turns a scenario room into a history, in causal order.
func historyOf(r *room) history {
	es := r.entries()
	anc := ancestorsOf(es)
	sort.Slice(es, func(i, j int) bool {
		if a, b := len(anc[es[i].ID]), len(anc[es[j].ID]); a != b {
			return a < b
		}
		return es[i].ID < es[j].ID
	})
	h := history{}
	for _, e := range es {
		h = h.with(e)
	}
	return h
}

// P3 from rooms larger than the exhaustive bound reaches: four admins with
// one of them asleep through the last promotion, and a room that has
// already replaced a lost admin.
//
// Requirements: T9
func TestOneKeyCannotChangeAdminSetInLargerRooms(t *testing.T) {
	var starts []history
	r, _ := threeAdmins(t)
	r.add("A", "D", r.all()...)
	r.admin("A", "D", r.all()...)
	r.admin("C", "D", r.all()...)
	starts = append(starts, historyOf(r))
	r2, h0 := threeAdmins(t)
	r2.remove("A", "C", h0...)
	r2.remove("B", "C", r2.all()...)
	r2.admin("A", "D", r2.all()...)
	r2.admin("B", "D", r2.all()...)
	starts = append(starts, historyOf(r2))
	depth := 1
	if deep() {
		depth = 2
	}
	res := searchP3(Final, [][]history{starts}, depth, nil)
	t.Logf("P3 (larger rooms): %d attack sets checked at depth %d", res.checked, depth)
	if res.example != "" {
		t.Fatalf("P3 violated:\n%s", res.example)
	}
}

// hasFork reports whether es holds two different entries by k at one
// position.
func hasFork(es []Entry, k string) bool {
	all := map[string]bool{}
	for _, e := range es {
		all[e.ID] = true
	}
	return sawFork(es, all, k)
}

// searchForks checks rule 12 against one key k that also forks its own
// chain, from every room at rest with at least minAdmins admins:
//
//   - P1: every attack set resolves the same in any order, fork evidence
//     included (the evidence is part of the set: two of its entries);
//   - without a fork, P3 as in searchP3;
//   - with a fork, P3 for a machine that holds the room and then receives
//     the attack entries (it refuses the second branch);
//   - after settlement (each other admin removing k, having seen what
//     its machine accepted), only k has left the admin set, and two machines that
//     received the attack in opposite orders agree.
func searchForks(levels [][]history, depth, minAdmins int, rng *rand.Rand) p3Result {
	var res p3Result
	fail := func(msg string, es []Entry) p3Result {
		res.example = msg + "\n" + describe(es)
		return res
	}
	for _, level := range levels {
		for _, h := range level {
			c := cache{}
			s0 := c.resolve(Final, h.es)
			if len(s0.Admins) < minAdmins || !atRest(h, s0) {
				continue
			}
			keys := map[string]bool{}
			for _, e := range h.es {
				keys[e.Author] = true
			}
			for _, k := range sortedKeys(keys) {
				frontier := []history{h}
				for d := 0; d < depth; d++ {
					var next []history
					for _, cur := range frontier {
						for _, e := range withForks(cur, k) {
							nh := cur.with(e)
							res.checked++
							s1 := c.resolve(Final, nh.es)
							es := append([]Entry(nil), nh.es...)
							rng.Shuffle(len(es), func(i, j int) { es[i], es[j] = es[j], es[i] })
							if got := Final.Resolve(es); !reflect.DeepEqual(got.Effects, s1.Effects) || !reflect.DeepEqual(got.Admins, s1.Admins) || !reflect.DeepEqual(got.Members, s1.Members) {
								return fail("P1: order changed the result", nh.es)
							}
							attack := nh.es[len(h.es):]
							if !hasFork(nh.es, k) {
								if v := p3Violation(s0, s1, k); v != "" {
									return fail("key "+k+": "+v, nh.es)
								}
							} else {
								res.forks++
								m1, m2 := machineWith(h.es), machineWith(h.es)
								m1.Receive(attack...)
								for i := len(attack) - 1; i >= 0; i-- {
									m2.Receive(attack[i])
								}
								for _, m := range []*Machine{m1, m2} {
									if v := p3Violation(s0, m.State(), k); v != "" {
										return fail("key "+k+" (machine): "+v, nh.es)
									}
								}
								for _, y := range s0.Admins {
									if y == k {
										continue
									}
									rm := settleBy(m1, nh, y, k)
									s2 := Final.Resolve(append(append([]Entry(nil), nh.es...), rm))
									for _, z := range s0.Admins {
										if z != k && !s2.admins[z] {
											return fail("key "+k+": settled by "+y+", removed admin "+z, append(nh.es, rm))
										}
									}
									for _, z := range s2.Admins {
										if !s0.admins[z] {
											return fail("key "+k+": settled by "+y+", new admin "+z, append(nh.es, rm))
										}
									}
									a, b := machineWith(h.es), machineWith(h.es)
									a.Receive(attack...)
									a.Receive(rm)
									for i := len(attack) - 1; i >= 0; i-- {
										b.Receive(attack[i])
									}
									b.Receive(rm)
									a.Receive(b.Offer()...)
									b.Receive(a.Offer()...)
									if !sameState(a.State(), b.State()) {
										return fail("key "+k+": settled by "+y+", machines disagree", append(nh.es, rm))
									}
								}
							}
							if d+1 < depth {
								next = append(next, nh)
							}
						}
					}
					frontier = next
				}
			}
		}
	}
	return res
}

// settleBy is y's removal of k signed on machine m: having seen what m
// accepted, so one branch of the fork.
func settleBy(m *Machine, h history, y, k string) Entry {
	e := Entry{Author: y, Kind: Remove, Target: k, Seq: 1, Parents: idsOf(m.Entries())}
	if p, ok := h.last[y]; ok {
		e.Seq, e.Prev = p.Seq+1, p.ID
	}
	return New(e)
}

// Rule 12 and P3: a key that forks its own chain makes only itself
// removable. From every honest room with two, three or four admins at
// rest, one key's extra entries, including entries at positions it has
// already used, do not remove another admin or create a new one, on a
// machine holding the room or after another admin settles the fork; and
// every such set resolves the same in any order (P1).
//
// Requirements: T9, D6
func TestOneKeyCannotChangeAdminSetByForkingItsOwnChain(t *testing.T) {
	depth := 1
	if deep() {
		depth = 2
	}
	rng := rand.New(rand.NewSource(12))
	// The short run attacks rooms up to two entries after create, and one
	// four-admin room; the deep run every room one entry deep, then rooms
	// up to three entries two entries deep, and four seeded rooms one entry
	// deep (two entries deep there did not finish in 50 minutes).
	levels := histories(t)
	res := searchForks(levels[:3], 1, 2, rng)
	if deep() {
		res = searchForks(levels, 1, 2, rng)
		if res.example == "" {
			r2 := searchForks(levels[:4], depth, 2, rng)
			r2.checked += res.checked
			r2.forks += res.forks
			res = r2
		}
	}
	t.Logf("forks (two and three admins): %d attack sets, %d with a fork", res.checked, res.forks)
	if res.example != "" {
		t.Fatalf("violated:\n%s", res.example)
	}
	var starts []history
	names := []string{"four admins, one stale"}
	if deep() {
		names = append(names, "admin replaced", "removal complete, more voters", "fork settled")
	}
	for _, name := range names {
		starts = append(starts, seeds(t)[name])
	}
	res = searchForks([][]history{starts}, 1, 2, rng)
	t.Logf("forks (larger rooms): %d attack sets, %d with a fork", res.checked, res.forks)
	if res.example != "" {
		t.Fatalf("violated:\n%s", res.example)
	}
}

// seeds are starting rooms where the rules are intricate, so the seeded
// search spends its depth there rather than on reaching them.
func seeds(t *testing.T) map[string]history {
	out := map[string]history{}
	r, _ := threeAdmins(t)
	out["three admins"] = historyOf(r)

	// A removed as admin, added back and made an admin again.
	r = newRoom(t, "A")
	r.admin("A", "B")
	r.add("A", "D")
	r.admin("A", "C", r.all()...)
	r.admin("B", "C", r.all()...)
	h0 := r.all()
	r.remove("B", "A", h0...)
	r.remove("C", "A", h0...)
	r.add("B", "A", r.all()...)
	r.admin("B", "A", r.all()...)
	r.admin("C", "A", r.all()...)
	out["admin removed and re-promoted"] = historyOf(r)

	// Four admins; B forked (a restored backup); A and C removed B having
	// seen different branches; both branches are in the set.
	r, _ = threeAdmins(t)
	r.admin("A", "D", r.all()...)
	r.admin("B", "D", r.all()...)
	h0 = r.all()
	lastB := r.last["B"]
	addD := r.add("B", "D", h0...)
	addE := r.fork("B", lastB.Seq+1, lastB.ID, Add, "E", h0...)
	r.remove("A", "B", append(h0, addD)...)
	r.remove("C", "B", append(h0, addE)...)
	out["fork settled"] = historyOf(r)

	// Four admins, B asleep through D's promotion.
	r, _ = threeAdmins(t)
	r.add("A", "D", r.all()...)
	r.admin("A", "D", r.all()...)
	r.admin("C", "D", r.all()...)
	out["four admins, one stale"] = historyOf(r)

	// A lost admin replaced.
	r, h0 = threeAdmins(t)
	r.remove("A", "C", h0...)
	r.remove("B", "C", r.all()...)
	r.admin("A", "D", r.all()...)
	r.admin("B", "D", r.all()...)
	out["admin replaced"] = historyOf(r)

	// Five admins; C adds X unseen by A and B, who then remove C. The
	// removal is complete and other admins can still vote on it.
	r, _ = threeAdmins(t)
	r.admin("A", "D", r.all()...)
	r.admin("B", "D", r.all()...)
	r.admin("A", "E", r.all()...)
	r.admin("B", "E", r.all()...)
	h0 = r.all()
	r.add("C", "X", h0...)
	r.remove("A", "C", h0...)
	r.remove("B", "C", h0...)
	out["removal complete, more voters"] = historyOf(r)

	// Four admins; A votes on its own removal, B on A's removal too.
	r, _ = threeAdmins(t)
	r.admin("A", "D", r.all()...)
	r.admin("B", "D", r.all()...)
	r.remove("A", "A", r.all()...)
	r.remove("B", "A", r.all()...)
	out["self-vote"] = historyOf(r)
	return out
}

// checkLocalEffect is P7: a change made by admins who have seen the whole
// history changes the status of its target and of no one else.
func checkLocalEffect(rules Rules, h history, c cache) error {
	s0 := c.resolve(rules, h.es)
	if !atRest(h, s0) {
		return nil
	}
	sign := func(cur history, u string, kind Kind, x string) history {
		prev := cur.last[u]
		return cur.with(New(Entry{Author: u, Seq: prev.Seq + 1, Prev: prev.ID, Parents: cur.ids(), Kind: kind, Target: x}))
	}
	for _, x := range append(machinesOf(h), "N") {
		var others []string
		for _, a := range s0.Admins {
			if a != x {
				others = append(others, a)
			}
		}
		if len(others) == 0 {
			continue
		}
		need := 1
		if len(s0.Admins) > 1 {
			need = 2
		}
		var trials []history
		switch {
		case !s0.members[x]:
			trials = append(trials, sign(h, others[0], Add, x))
		case !s0.admins[x]:
			trials = append(trials, sign(h, others[0], Remove, x))
		}
		if len(others) >= need && !s0.admins[x] {
			t := sign(h, others[0], MakeAdmin, x)
			if need == 2 {
				t = sign(t, others[1], MakeAdmin, x)
			}
			trials = append(trials, t)
		}
		if s0.admins[x] && len(others) >= 2 {
			t := sign(h, others[0], Remove, x)
			trials = append(trials, sign(t, others[1], Remove, x))
		}
		for _, t := range trials {
			s1 := c.resolve(rules, t.es)
			for _, y := range append(machinesOf(t), "N") {
				if y != x && (s0.members[y] != s1.members[y] || s0.admins[y] != s1.admins[y]) {
					return fmt.Errorf("P7: a change to %s also changed %s\n%s", x, y, describe(t.es))
				}
			}
		}
	}
	return nil
}

// P1-P7 from seeded rooms (an admin removed and re-promoted, a settled
// fork, four admins with one stale, a replaced admin, a completed removal
// with more admins to vote, a self-vote), exploring every honest
// continuation a few entries deep.
//
// Requirements: T9, T5, T10, A4
func TestSeededRoomsKeepEveryProperty(t *testing.T) {
	steps, depth := 1, 1
	if deep() {
		steps, depth = 2, 2
	}
	rng := rand.New(rand.NewSource(4))
	for name, start := range seeds(t) {
		c := cache{}
		levels := extend(Final, start, steps, c)
		n := 0
		for _, level := range levels {
			for _, h := range level {
				n++
				st := c.resolve(Final, h.es)
				if err := checkInvariants(h.es, st); err != nil {
					t.Fatalf("%s: %v\n%s", name, err, describe(h.es))
				}
				if err := checkLocalEffect(Final, h, c); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if err := checkLossSurvivable(h, c); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				es := append([]Entry(nil), h.es...)
				rng.Shuffle(len(es), func(i, j int) { es[i], es[j] = es[j], es[i] })
				if got := Resolve(es); !reflect.DeepEqual(got.Members, st.Members) || !reflect.DeepEqual(got.Admins, st.Admins) {
					t.Fatalf("%s: P1: order changed the result (seed 4)\n%s", name, describe(h.es))
				}
			}
		}
		// One-entry attacks: from the seed itself in the short run, from
		// every continuation in the deep run, which also tries two-entry
		// attacks from the first continuation step.
		attackFrom := levels[:1]
		if deep() {
			attackFrom = levels
		}
		res := searchP3(Final, attackFrom, 1, c)
		checked := res.checked
		if res.example == "" && depth > 1 {
			res = searchP3(Final, levels[:2], depth, nil)
			checked += res.checked
		}
		t.Logf("%s: %d histories, %d attack sets", name, n, checked)
		if res.example != "" {
			t.Fatalf("%s: P3 violated:\n%s", name, res.example)
		}
	}
}
