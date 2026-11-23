package members

import "strconv"

// Machine is the small local layer that decides what one machine accepts
// into its set of entries. Unlike Resolve it may depend on arrival order:
//
//  1. An entry is held until every entry it says it had seen is present.
//  2. A second, different entry by the same author at the same position is
//     refused, and the two are kept as evidence that the key signed a fork.
//     Everything that depends on a refused entry is refused too. So the
//     first branch a machine sees is the one it keeps.
//  3. A fork is settled when other admins remove the forking key. Once the
//     refused entries, together with the accepted ones, give a set in which
//     every forking key has been removed as an admin, the machine accepts
//     them all. From then on both branches are in every machine's set, so
//     every machine reaches the same state.
//
// A machine passes on everything it holds, including refused entries, so
// the evidence travels.
type Machine struct {
	Rules    Rules
	set      map[string]Entry
	pos      map[string]string
	held     map[string]Entry
	order    []string // held entries in arrival order
	refused  map[string]Entry
	Evidence [][2]string
	open     [][2]string // evidence not yet settled
}

// NewMachine returns an empty machine that resolves with rules r.
func NewMachine(r Rules) *Machine {
	return &Machine{Rules: r, set: map[string]Entry{}, pos: map[string]string{}, held: map[string]Entry{}, refused: map[string]Entry{}}
}

// Receive offers entries to the machine.
func (m *Machine) Receive(es ...Entry) {
	for _, e := range es {
		_, a := m.set[e.ID]
		_, b := m.held[e.ID]
		_, c := m.refused[e.ID]
		if a || b || c || e.ID != e.hash() || malformed(e) {
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
			ready, refused := true, false
			for _, p := range e.Parents {
				if _, ok := m.refused[p]; ok {
					refused = true
				}
				if _, ok := m.set[p]; !ok {
					ready = false
				}
			}
			k := e.Author + "\x00" + strconv.Itoa(e.Seq)
			switch {
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
	m.settle()
}

// settle accepts refused branches once every forking key has been removed.
func (m *Machine) settle() {
	if len(m.refused) == 0 {
		return
	}
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
	st := m.Rules.Resolve(cand)
	for _, pair := range m.open {
		if !removedAsAdmin(st, all, all[pair[0]].Author) {
			return
		}
	}
	m.open = nil
	for id, e := range all {
		m.set[id] = e
		delete(m.refused, id)
		delete(m.held, id)
	}
	var still []string
	for _, id := range m.order {
		if _, ok := m.held[id]; ok {
			still = append(still, id)
		}
	}
	m.order = still
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

// Entries returns the entries this machine has accepted.
func (m *Machine) Entries() []Entry {
	out := make([]Entry, 0, len(m.set))
	for _, id := range sortedKeys(m.set) {
		out = append(out, m.set[id])
	}
	return out
}

// Offer returns everything the machine passes on: accepted, held and
// refused entries.
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
func (m *Machine) State() State { return m.Rules.Resolve(m.Entries()) }
