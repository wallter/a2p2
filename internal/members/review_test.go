package members

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"sort"
	"testing"
)

// salted is a model id function other than the model hash, so that the
// same history gets ids in a different order.
func salted(salt string) func(entry) string {
	return func(e entry) string {
		sum := sha256.Sum256([]byte(salt + e.hash()))
		return hex.EncodeToString(sum[:8])
	}
}

// renamed rebuilds es under id function id: the same history, other ids.
// It returns the entries and the old-to-new id map.
func renamed(es []entry, id func(entry) string) ([]entry, map[string]string) {
	anc := ancestorsOf(es)
	order := append([]entry(nil), es...)
	sort.SliceStable(order, func(i, j int) bool { return len(anc[order[i].ID]) < len(anc[order[j].ID]) })
	m := map[string]string{}
	var out []entry
	for _, e := range order {
		n := e
		n.Prev = m[e.Prev]
		n.Parents = nil
		for _, p := range e.Parents {
			n.Parents = append(n.Parents, m[p])
		}
		sort.Strings(n.Parents)
		n.ID = id(n)
		m[e.ID] = n.ID
		out = append(out, n)
	}
	return out, m
}

// sameUpToRenaming compares two states of one history under two id
// functions, mapping a's entry ids through m.
func sameUpToRenaming(a, b State, m map[string]string) bool {
	if !reflect.DeepEqual(a.Members, b.Members) || !reflect.DeepEqual(a.Admins, b.Admins) || !reflect.DeepEqual(a.Admitted, b.Admitted) {
		return false
	}
	if len(a.Effects) != len(b.Effects) || len(a.Permissions) != len(b.Permissions) {
		return false
	}
	for id, ef := range a.Effects {
		if b.Effects[m[id]] != ef {
			return false
		}
	}
	for x, p := range a.Permissions {
		q := b.Permissions[x]
		var gs []string
		for _, g := range p.Grants {
			gs = append(gs, m[g])
		}
		sort.Strings(gs)
		if p.All != q.All || p.Closed != q.Closed || !reflect.DeepEqual(p.From, q.From) || !reflect.DeepEqual(gs, q.Grants) {
			return false
		}
	}
	return true
}

// checkRenaming resolves es under the model hash and under other id
// functions (salted hashes, three unless salts are given) and reports
// whether the results agree up to renaming.
func checkRenaming(es []entry, salts ...string) bool {
	if len(salts) == 0 {
		salts = []string{"1", "2", "3"}
	}
	root := rootOf(es)
	a := final.resolveRoom(root, es)
	for _, salt := range salts {
		id := salted(salt)
		r2, m := renamed(es, id)
		rules := final
		rules.id = id
		if !sameUpToRenaming(a, rules.resolveRoom(m[root], r2), m) {
			return false
		}
	}
	return true
}

// Review blocker 1: B signs three entries at one position. A removes B
// having seen one branch, then adds B back with a frontier having seen the
// removal and two branches; C adds B back with history having seen all
// three. Whether an entry has seen fork evidence against B is a property
// of its own past (two different entries by B at one position), so both
// re-adds have seen the removal and the more restrictive applies, whatever
// the ids.
//
// Requirements: T9, D6, RM2
func TestForkEvidenceSeenDoesNotDependOnIDOrder(t *testing.T) {
	r, h0 := threeAdmins(t)
	lastB := r.last["B"]
	f1 := r.fork("B", lastB.Seq+1, lastB.ID, kAdd, "X", h0...)
	f2 := r.fork("B", lastB.Seq+1, lastB.ID, kAdd, "Y", h0...)
	f3 := r.fork("B", lastB.Seq+1, lastB.ID, kAdd, "Z", h0...)
	rm := r.remove("A", "B", append(h0, f1)...)
	gA := r.grant("A", kAdd, "B", false, map[string][]string{"A": {"0123456789abcdef0123456789abcdef"}}, append(h0, rm, f1, f2)...)
	r.grant("C", kAdd, "B", true, nil, append(h0, rm, f1, f2, f3)...)
	es := r.entries()
	st := final.resolveRoom(r.root, es)
	p := st.Permissions["B"]
	if p.All || len(p.Grants) != 2 || !reflect.DeepEqual(p.From, map[string][]string{"A": {"0123456789abcdef0123456789abcdef"}}) {
		t.Fatalf("B's permission: %+v (A's grant %s)", p, gA)
	}
	if !checkRenaming(es) {
		t.Fatal("the result depends on the id function")
	}
}

// Review blocker 2: entries released when a fork settles go through the
// same chain and position checks as new ones. B forks; C signs an add
// having seen B's second branch, so a machine that kept B's first branch
// refuses it. When A and C remove B, it is released and C's position is
// reserved: C's restored backup signing another entry at that position is
// then a fork.
//
// Requirements: T9, D6
func TestEntriesReleasedBySettlementReserveTheirPositions(t *testing.T) {
	r, h0 := threeAdmins(t)
	lastB := r.last["B"]
	b1 := r.add("B", "X", h0...)
	b2 := r.fork("B", lastB.Seq+1, lastB.ID, kAdd, "Y", h0...)
	c1 := r.add("C", "Z", append(h0, b2)...)
	m := machineWith(r.entries(h0...))
	m.receive(r.byID[b1], r.byID[b2], r.byID[c1])
	if holds(m, c1) {
		t.Fatal("expected C's add to be refused with B's second branch")
	}
	rm := r.remove("A", "B", append(h0, b1)...)
	m.receive(r.byID[rm])
	if !holds(m, b2, c1) {
		t.Fatal("expected settlement to release B's branch and C's add")
	}
	c1b := r.put(entry{Author: "C", Seq: 1, Kind: kAdd, Target: "W", Parents: h0})
	m.receive(r.byID[c1b])
	if holds(m, c1b) {
		t.Fatal("a second entry at C's released position was accepted")
	}
	found := false
	for _, ev := range m.Evidence() {
		if ev[0] == c1 && ev[1] == c1b {
			found = true
		}
	}
	if !found {
		t.Fatalf("no fork evidence against C: %v", m.Evidence())
	}
}

// Review should-fix: an entry whose previous entry is a refused entry by
// another author is invalid at once, not passed on as a refused dependant.
//
// Requirements: A4
func TestPrevIsCheckedAgainstRefusedEntries(t *testing.T) {
	r, h0 := threeAdmins(t)
	lastB := r.last["B"]
	b1 := r.add("B", "X", h0...)
	b2 := r.fork("B", lastB.Seq+1, lastB.ID, kAdd, "Y", h0...)
	lastA := r.last["A"]
	bad := newEntry(entry{Author: "A", Seq: lastA.Seq + 1, Prev: b2, Kind: kAdd, Target: "Q", Parents: append(h0, b2)})
	m := machineWith(r.entries(h0...))
	m.receive(r.byID[b1], r.byID[b2])
	res := m.receive(bad)
	if len(res) != 1 || res[0].Disposition != Invalid || res[0].Reason != ReasonBadChain {
		t.Fatalf("got %+v", res)
	}
	for _, e := range m.offer() {
		if e.ID == bad.ID {
			t.Fatal("the bad entry is passed on")
		}
	}
}

// Review should-fix: two competing entries (one key, one position) wait
// for a missing parent, arriving X then Y; when the parent arrives X wins.
// A machine replaying what this one offers, or the same arrival batches,
// picks the same winner, and Receive reports every entry it touched.
//
// Requirements: D6
func TestReplayOfHeldCompetingEntriesPicksTheSameWinner(t *testing.T) {
	for _, swap := range []bool{false, true} {
		r, h0 := threeAdmins(t)
		p := r.add("A", "P", h0...)
		lastB := r.last["B"]
		x := r.add("B", "X", append(h0, p)...)
		y := r.fork("B", lastB.Seq+1, lastB.ID, kAdd, "Y", append(h0, p)...)
		first, second := x, y
		if swap {
			first, second = y, x
		}
		m := machineWith(r.entries(h0...))
		var log []Result
		log = append(log, m.receive(r.byID[first])...)
		log = append(log, m.receive(r.byID[second])...)
		// What it offers while both wait, replayed elsewhere.
		replay := machineWith(r.entries(h0...))
		replay.receive(m.offer()...)
		log = append(log, m.receive(r.byID[p])...)
		replay.receive(r.byID[p])
		if !holds(m, first) || holds(m, second) {
			t.Fatalf("swap=%v: the first to arrive should win", swap)
		}
		var touched []string
		for _, res := range log {
			touched = append(touched, res.ID+":"+string(res.Disposition))
		}
		want := []string{first + ":held", second + ":held", p + ":accepted", first + ":accepted", second + ":refused"}
		if !reflect.DeepEqual(touched, want) {
			t.Fatalf("swap=%v: dispositions %v, want %v", swap, touched, want)
		}
		if !holds(replay, first) || holds(replay, second) || !sameState(replay.State(), m.State()) {
			t.Fatalf("swap=%v: replay picked another winner", swap)
		}
	}
}
