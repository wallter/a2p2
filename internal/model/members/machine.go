package members

import (
	"sort"
	"strconv"
)

// Machine is the small local layer that decides what one machine accepts
// into its set of entries. Unlike Resolve it may depend on arrival order:
//
//  1. An entry is held until every entry it says it had seen is present.
//  2. A second, different entry by the same author at the same position is
//     refused, and the two are kept as evidence that the key signed a fork.
//     Everything that depends on a refused entry is refused too. So the
//     first branch a machine sees is the one it keeps.
//  3. A fork by a key that was not an admin is settled at once: neither
//     entry had effect. A fork by an admin is settled when it is removed
//     (its fork counts as its own vote, rule 12). Once the
//     refused entries, together with the accepted ones, give a set in which
//     every forking key has been removed as an admin, the machine accepts
//     them all. From then on both branches are in every machine's set, so
//     every machine reaches the same state.
//
// A machine passes on everything it holds, including refused entries, so
// the evidence travels.
type Machine struct {
	Rules    Rules
	root     Entry
	Invalid  map[string]string // entries refused for their form, with reasons
	set      map[string]Entry
	pos      map[string]string
	held     map[string]Entry
	order    []string // held entries in arrival order
	refused  map[string]Entry
	Evidence [][2]string
	open     [][2]string // evidence not yet settled
}

// NewMachine returns a machine for the room whose create entry is root,
// resolving with rules r.
func NewMachine(r Rules, root Entry) *Machine {
	m := &Machine{Rules: r, root: root, Invalid: map[string]string{}, set: map[string]Entry{}, pos: map[string]string{}, held: map[string]Entry{}, refused: map[string]Entry{}}
	m.Receive(root)
	return m
}

// Receive offers entries to the machine.
func (m *Machine) Receive(es ...Entry) {
	for _, e := range es {
		_, a := m.set[e.ID]
		_, b := m.held[e.ID]
		_, c := m.refused[e.ID]
		if a || b || c || m.Invalid[e.ID] != "" {
			continue
		}
		switch {
		case e.ID != e.hash():
			m.Invalid[e.ID] = ReasonBadID
			continue
		case malformed(e):
			m.Invalid[e.ID] = ReasonMalformed
			continue
		case e.Kind == Create && e.ID != m.root.ID:
			m.Invalid[e.ID] = ReasonOtherCreate
			continue
		}
		m.held[e.ID] = e
		m.order = append(m.order, e.ID)
	}
	// Each pass handles the earliest held entry, in arrival order, that can
	// be decided, then starts again; so of two competing entries the one
	// that arrived first is decided first.
	for changed := true; changed; {
		changed = false
		for _, id := range m.order {
			e, ok := m.held[id]
			if !ok {
				continue
			}
			ready, refused, invalid := true, false, ""
			for _, p := range e.Parents {
				if _, ok := m.refused[p]; ok {
					refused = true
				}
				if m.Invalid[p] != "" {
					invalid = ReasonInvalidParent
				}
				if _, ok := m.set[p]; !ok {
					ready = false
				}
			}
			if ready && invalid == "" && e.Prev != "" {
				// The chain is checked before the position is reserved, so
				// a malformed entry reserves nothing and is never evidence.
				if p := m.set[e.Prev]; p.Author != e.Author || p.Seq != e.Seq-1 {
					invalid = ReasonBadChain
				}
			}
			k := e.Author + "\x00" + strconv.Itoa(e.Seq)
			switch {
			case invalid != "":
				m.Invalid[id] = invalid
			case refused:
				m.refused[id] = e
			case !ready:
				continue
			case m.pos[k] != "" && m.pos[k] != id:
				m.refused[id] = e
				m.Evidence = append(m.Evidence, [2]string{m.pos[k], id})
				m.open = append(m.open, [2]string{m.pos[k], id})
			default:
				m.set[id] = e
				m.pos[k] = id
			}
			delete(m.held, id)
			changed = true
			break
		}
	}
	var still []string
	for _, id := range m.order {
		if _, ok := m.held[id]; ok {
			still = append(still, id)
		}
	}
	m.order = still
	if m.settle() && len(m.held) > 0 {
		m.Receive()
	}
}

// settle accepts refused branches of every fork that has been settled:
// its key was removed as an admin, or it was not an admin at either entry.
// Forks by other keys that are still open stay refused, with everything
// that depends on them, so one open fork does not hold back another's
// settlement.
func (m *Machine) settle() bool {
	if len(m.refused) == 0 {
		return false
	}
	var still [][2]string
	blocked := map[string]bool{}
	if len(m.open) > 0 {
		still, blocked = m.openForks()
	}
	m.open = still
	// Accept refused entries in causal order, except the refused entries
	// of open forks and what depends on them.
	accepted := false
	for changed := true; changed; {
		changed = false
		for _, id := range sortedKeys(m.refused) {
			e := m.refused[id]
			if blocked[id] {
				continue
			}
			ready := true
			for _, p := range e.Parents {
				if blocked[p] {
					blocked[id] = true
				}
				if _, ok := m.set[p]; !ok {
					ready = false
				}
			}
			if blocked[id] || !ready {
				continue
			}
			if p, ok := m.set[e.Prev]; e.Prev != "" && (!ok || p.Author != e.Author || p.Seq != e.Seq-1) {
				m.Invalid[id] = ReasonBadChain
				delete(m.refused, id)
				changed = true
				continue
			}
			m.set[id] = e
			delete(m.refused, id)
			changed, accepted = true, true
		}
	}
	return accepted
}

// openForks returns the forks not yet settled, and their refused entries.
func (m *Machine) openForks() ([][2]string, map[string]bool) {
	all := map[string]Entry{}
	for _, src := range []map[string]Entry{m.set, m.refused, m.held} {
		for id, e := range src {
			all[id] = e
		}
	}
	// Keep only entries whose parents are all present.
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
	cand := make([]Entry, 0, len(all))
	for _, e := range all {
		cand = append(cand, e)
	}
	st := m.Rules.ResolveRoom(m.root.ID, cand)
	var still [][2]string
	blocked := map[string]bool{}
	for _, pair := range m.open {
		if notAdminFork(st, pair) || removedAsAdmin(st, all, all[pair[0]].Author) {
			continue
		}
		still = append(still, pair)
		blocked[pair[1]] = true
	}
	return still, blocked
}

// notAdminFork reports whether neither entry of a fork had effect because
// its author was not an admin: such a fork needs no settling.
func notAdminFork(st State, pair [2]string) bool {
	return st.Effects[pair[0]].Reason == ReasonNotAdmin && st.Effects[pair[1]].Reason == ReasonNotAdmin
}

// removedAsAdmin reports whether some removal of k took effect and k is
// no longer an admin.
func removedAsAdmin(st State, es map[string]Entry, k string) bool {
	if k == "" || st.admins[k] {
		return false
	}
	for id, e := range es {
		if e.Kind == Remove && e.Target == k && st.Effects[id].Applied {
			return true
		}
	}
	return false
}

// Entries returns the entries this machine has accepted, in causal order
// (every entry after the entries it had seen; ties by id), so replaying
// them into a fresh machine accepts each one as it arrives.
func (m *Machine) Entries() []Entry {
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
	out := make([]Entry, 0, len(m.set))
	for _, id := range sortedKeys(m.set) {
		d(id)
		out = append(out, m.set[id])
	}
	sort.SliceStable(out, func(i, j int) bool { return depth[out[i].ID] < depth[out[j].ID] })
	return out
}

// Offer returns everything the machine passes on: accepted entries in
// causal order (as Entries), then held and refused entries by id. Entries
// refused for their form are not passed on.
func (m *Machine) Offer() []Entry {
	out := m.Entries()
	for _, src := range []map[string]Entry{m.held, m.refused} {
		for _, id := range sortedKeys(src) {
			out = append(out, src[id])
		}
	}
	return out
}

// State resolves the machine's accepted set.
func (m *Machine) State() State { return m.Rules.ResolveRoom(m.root.ID, m.Entries()) }
