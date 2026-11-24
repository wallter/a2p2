package members

import (
	"container/heap"
	"sort"
	"strconv"
)

// Machine is the small local layer that decides what one machine accepts
// into its set of entries. Unlike the resolver it may depend on arrival
// order:
//
//  1. An entry is checked for form, room and chain (its previous entry is
//     its author's, one position back) before anything else; one that
//     fails is invalid, reserves nothing and is not passed on.
//  2. An entry is held until every entry it says it had seen is present.
//     Of several entries waiting, the one that arrived first is decided
//     first.
//  3. A second, different entry by the same author at the same position is
//     refused, and the two are kept as evidence that the key signed a fork.
//     Everything that depends on a refused entry is refused too. So the
//     first branch a machine sees is the one it keeps.
//  4. A fork by a key that was not an admin at either entry is settled at
//     once: neither had effect. A fork by an admin is settled when it is
//     removed as an admin (its fork counts as its own vote, rule 12). The
//     entries refused for it, and their dependants, then go through steps
//     1 to 3 again, with both entries at the forked position allowed. An
//     open fork by another key holds back only what depends on it.
//
// A machine passes on everything it accepted, held or refused, so the
// evidence travels. Bounds: at most maxHeld entries and maxHeldBytes bytes
// wait for missing parents, the oldest dropped first; at most maxBranches
// entries are kept for one position.
type Machine struct {
	rules    ruleSet
	root     entry
	arrivals int
	arrival  map[string]int
	set      map[string]entry
	pos      map[string]string   // author and position: the accepted entry
	branches map[string][]string // author and position: every entry kept there
	settled  map[string]bool     // positions of settled forks: both allowed
	held     map[string]entry
	bytes    int
	waiting  map[string][]string // an entry not yet accepted: what waits on it
	refused  map[string]entry
	invalid  map[string]string
	evidence [][2]string
	open     [][2]string // evidence not yet settled
	results  []Result
	queue    arrivalQueue
	// dirty: since the last settlement check, something was accepted or
	// refused that can change whether an open fork is settled.
	dirty bool
}

const (
	maxHeld      = 1024
	maxHeldBytes = 4 << 20
	maxBranches  = 8
)

// Disposition is what a machine did with an entry.
type Disposition string

const (
	Accepted Disposition = "accepted"
	Held     Disposition = "held"    // waiting for an entry it had seen
	Refused  Disposition = "refused" // fork evidence, or depends on it
	Invalid  Disposition = "invalid" // form, room, signature or chain
	Dropped  Disposition = "dropped" // over a bound; may be received again
)

// Result is one entry's disposition, with a reason unless accepted.
type Result struct {
	ID          string
	Disposition Disposition
	Reason      string
}

// newMachine returns a machine for the room whose create entry is root,
// resolving with rules r.
func newMachine(r ruleSet, root entry) *Machine {
	m := &Machine{rules: r, root: root, arrival: map[string]int{}, set: map[string]entry{}, pos: map[string]string{},
		branches: map[string][]string{}, settled: map[string]bool{}, held: map[string]entry{}, waiting: map[string][]string{},
		refused: map[string]entry{}, invalid: map[string]string{}}
	m.receive(root)
	return m
}

func position(e entry) string { return e.Author + "\x00" + strconv.FormatInt(e.Seq, 10) }

// receive offers entries to the machine and returns, in the order they
// were decided, what happened to every entry it touched: those offered
// and those released from waiting.
func (m *Machine) receive(es ...entry) []Result {
	m.results = nil
	for _, e := range es {
		_, a := m.set[e.ID]
		_, b := m.held[e.ID]
		_, c := m.refused[e.ID]
		if a || b || c || m.invalid[e.ID] != "" {
			continue
		}
		switch {
		case e.ID != m.rules.idOf(e):
			m.reject(e.ID, ReasonBadID)
			continue
		case malformed(e):
			m.reject(e.ID, ReasonMalformed)
			continue
		case e.Kind == kCreate && e.ID != m.root.ID:
			m.reject(e.ID, ReasonOtherCreate)
			continue
		case e.room != m.root.room:
			m.reject(e.ID, ReasonOtherRoom)
			continue
		}
		m.arrival[e.ID] = m.arrivals
		m.arrivals++
		heap.Push(&m.queue, queued{m.arrival[e.ID], e})
	}
	for {
		m.drain()
		if !m.settle() {
			break
		}
	}
	m.bound()
	return m.results
}

func (m *Machine) reject(id, reason string) {
	m.invalid[id] = reason
	m.results = append(m.results, Result{id, Invalid, reason})
}

// drain decides queued entries, earliest arrival first.
func (m *Machine) drain() {
	for m.queue.Len() > 0 {
		q := heap.Pop(&m.queue).(queued)
		m.decide(q.e)
	}
}

// decide classifies an entry that is not accepted, refused or invalid.
func (m *Machine) decide(e entry) {
	id := e.ID
	if _, ok := m.set[id]; ok {
		return
	}
	_, wasHeld := m.held[id]
	invalidParent, refusedParent := false, false
	var missing []string
	for _, p := range e.Parents {
		switch _, inSet := m.set[p]; {
		case inSet:
		case m.invalid[p] != "":
			invalidParent = true
		default:
			if _, ok := m.refused[p]; ok {
				refusedParent = true
			}
			missing = append(missing, p)
		}
	}
	if prev, ok := m.known(e.Prev); e.Prev != "" && ok && (prev.Author != e.Author || prev.Seq != e.Seq-1) {
		m.finish(e, wasHeld, Invalid, ReasonBadChain)
		return
	}
	switch {
	case invalidParent:
		m.finish(e, wasHeld, Invalid, ReasonInvalidParent)
		return
	case refusedParent:
		m.wait(e, missing)
		m.finish(e, wasHeld, Refused, ReasonRefusedParent)
		return
	case len(missing) > 0:
		if !wasHeld {
			m.held[id] = e
			m.bytes += sizeOf(e)
			m.results = append(m.results, Result{id, Held, ReasonMissingParent})
		}
		m.wait(e, missing)
		return
	}
	k := position(e)
	switch cur := m.pos[k]; {
	case cur == "" || m.settled[k]:
		if cur == "" {
			m.pos[k] = id
			m.branches[k] = []string{id}
		} else if !contains(m.branches[k], id) {
			m.branches[k] = append(m.branches[k], id)
		}
		m.finish(e, wasHeld, Accepted, "")
	case len(m.branches[k]) >= maxBranches:
		m.drop(e, wasHeld, ReasonTooManyBranches)
	default:
		m.branches[k] = append(m.branches[k], id)
		m.evidence = append(m.evidence, [2]string{cur, id})
		m.open = append(m.open, [2]string{cur, id})
		m.dirty = true
		m.finish(e, wasHeld, Refused, ReasonForkRefused)
	}
}

// known returns an entry the machine has accepted or refused.
func (m *Machine) known(id string) (entry, bool) {
	if e, ok := m.set[id]; ok {
		return e, true
	}
	e, ok := m.refused[id]
	return e, ok
}

func (m *Machine) wait(e entry, on []string) {
	for _, p := range on {
		if !contains(m.waiting[p], e.ID) {
			m.waiting[p] = append(m.waiting[p], e.ID)
		}
	}
}

// finish records a decision and wakes what waits on the entry.
func (m *Machine) finish(e entry, wasHeld bool, d Disposition, reason string) {
	if wasHeld {
		delete(m.held, e.ID)
		m.bytes -= sizeOf(e)
	}
	switch d {
	case Accepted:
		m.set[e.ID] = e
		m.dirty = m.dirty || e.Kind == kRemove || e.Kind == kAdmin
	case Refused:
		m.refused[e.ID] = e
		m.dirty = m.dirty || e.Kind == kRemove || e.Kind == kAdmin
	case Invalid:
		m.invalid[e.ID] = reason
	}
	m.results = append(m.results, Result{e.ID, d, reason})
	if d == Refused {
		return // dependants are refused already, or will be when they come
	}
	for _, w := range m.waiting[e.ID] {
		if x, ok := m.held[w]; ok {
			heap.Push(&m.queue, queued{m.arrival[w], x})
		} else if x, ok := m.refused[w]; ok && d == Accepted {
			delete(m.refused, w)
			heap.Push(&m.queue, queued{m.arrival[w], x})
		}
	}
	delete(m.waiting, e.ID)
}

func (m *Machine) drop(e entry, wasHeld bool, reason string) {
	if wasHeld {
		delete(m.held, e.ID)
		m.bytes -= sizeOf(e)
	}
	delete(m.arrival, e.ID)
	for _, p := range e.Parents {
		ws := m.waiting[p][:0]
		for _, w := range m.waiting[p] {
			if w != e.ID {
				ws = append(ws, w)
			}
		}
		if len(ws) == 0 {
			delete(m.waiting, p)
		} else {
			m.waiting[p] = ws
		}
	}
	m.results = append(m.results, Result{e.ID, Dropped, reason})
}

// bound drops the oldest waiting entries over the bounds.
func (m *Machine) bound() {
	if len(m.held) <= maxHeld && m.bytes <= maxHeldBytes {
		return
	}
	ids := sortedKeys(m.held)
	sort.Slice(ids, func(i, j int) bool { return m.arrival[ids[i]] < m.arrival[ids[j]] })
	for _, id := range ids {
		if len(m.held) <= maxHeld && m.bytes <= maxHeldBytes {
			return
		}
		m.drop(m.held[id], true, ReasonDroppedHeld)
	}
}

// sizeOf is an entry's size for the held bound: its signed header, or an
// estimate for a model entry.
func sizeOf(e entry) int {
	if e.size > 0 {
		return e.size
	}
	n := 64 + len(e.Author) + len(e.Prev) + len(e.Target)
	for _, p := range e.Parents {
		n += len(p)
	}
	return n
}

// settle releases what was refused for every fork that has been settled:
// its key was removed as an admin, or it was not an admin at either entry.
// It reports whether anything was released.
func (m *Machine) settle() bool {
	if len(m.open) == 0 || !m.dirty {
		return false
	}
	m.dirty = false
	st := m.candidateState()
	var still [][2]string
	released := false
	for _, pair := range m.open {
		a := m.refusedOrSet(pair[0])
		if !notAdminFork(st, pair) && !removedAsAdmin(st, a.Author) {
			still = append(still, pair)
			continue
		}
		k := position(a)
		m.settled[k] = true
		if e, ok := m.refused[pair[1]]; ok {
			delete(m.refused, pair[1])
			heap.Push(&m.queue, queued{m.arrival[e.ID], e})
			released = true
		}
	}
	m.open = still
	return released
}

func (m *Machine) refusedOrSet(id string) entry {
	e, _ := m.known(id)
	return e
}

// candidateState resolves the accepted and refused entries together, with
// held entries whose parents are all present.
func (m *Machine) candidateState() State {
	all := map[string]entry{}
	for _, src := range []map[string]entry{m.set, m.refused, m.held} {
		for id, e := range src {
			all[id] = e
		}
	}
	for changed := true; changed; {
		changed = false
		for id, e := range all {
			for _, p := range e.Parents {
				if _, ok := all[p]; !ok {
					delete(all, id)
					changed = true
					break
				}
			}
		}
	}
	cand := make([]entry, 0, len(all))
	for _, e := range all {
		cand = append(cand, e)
	}
	return m.rules.resolveRoom(m.root.ID, cand)
}

// notAdminFork reports whether neither entry of a fork had effect because
// its author was not an admin: such a fork needs no settling.
func notAdminFork(st State, pair [2]string) bool {
	return st.Effects[pair[0]].Reason == ReasonNotAdmin && st.Effects[pair[1]].Reason == ReasonNotAdmin
}

// removedAsAdmin reports whether k is no longer an admin after some
// removal of it took effect.
func removedAsAdmin(st State, k string) bool {
	return k != "" && !st.admins[k] && st.removedBy[k] != ""
}

// Evidence returns the fork evidence the machine holds: pairs of entries
// one key signed at one position, the accepted one first.
func (m *Machine) Evidence() [][2]string { return append([][2]string(nil), m.evidence...) }

// entries returns the entries this machine has accepted, in causal order
// (every entry after the entries it had seen; ties by id), so replaying
// them into a fresh machine accepts each one as it arrives.
func (m *Machine) entries() []entry {
	depth := map[string]int{}
	var d func(id string) int
	d = func(id string) int {
		if v, ok := depth[id]; ok {
			return v
		}
		v := 0
		for _, p := range m.set[id].Parents {
			if x := d(p) + 1; x > v {
				v = x
			}
		}
		depth[id] = v
		return v
	}
	out := make([]entry, 0, len(m.set))
	for _, id := range sortedKeys(m.set) {
		d(id)
		out = append(out, m.set[id])
	}
	sort.SliceStable(out, func(i, j int) bool { return depth[out[i].ID] < depth[out[j].ID] })
	return out
}

// offer returns everything the machine passes on: accepted entries in
// causal order, then held and refused entries in arrival order, so a
// machine replaying it decides competing entries as this one did.
func (m *Machine) offer() []entry {
	out := m.entries()
	var rest []entry
	for _, src := range []map[string]entry{m.held, m.refused} {
		for _, e := range src {
			rest = append(rest, e)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return m.arrival[rest[i].ID] < m.arrival[rest[j].ID] })
	return append(out, rest...)
}

// State resolves the machine's accepted set.
func (m *Machine) State() State { return m.rules.resolveRoom(m.root.ID, m.entries()) }

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

type queued struct {
	at int
	e  entry
}

type arrivalQueue []queued

func (q arrivalQueue) Len() int           { return len(q) }
func (q arrivalQueue) Less(i, j int) bool { return q[i].at < q[j].at }
func (q arrivalQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *arrivalQueue) Push(x any)        { *q = append(*q, x.(queued)) }
func (q *arrivalQueue) Pop() any {
	old := *q
	x := old[len(old)-1]
	*q = old[:len(old)-1]
	return x
}
