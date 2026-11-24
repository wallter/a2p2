package delivery

import "sort"

// Membership changes. The truth is a log of changes; each machine applies
// them one at a time, when told, so views differ until they catch up. How a
// change travels is modelled in internal/members; here it is one step.

// ChangeKind is what a membership change does.
type ChangeKind uint8

const (
	Add    ChangeKind = iota + 1 // a grant: the machine is admitted, with a history permission
	Remove                       // no longer a member; its past records still count
	Void                         // its admission never counted: its records are dropped
)

// Change is one entry of the membership log.
type Change struct {
	N       int
	Kind    ChangeKind
	Machine MachineID
	Adm     Admission // Add only
}

// view is one machine's idea of the membership. It is durable and goes back
// in time with a restored backup.
type view struct {
	now     map[MachineID]bool       // members now
	ever    map[MachineID]bool       // members now or before (not voided)
	adm     map[MachineID]Admission  // each machine's history permission
	out     map[MachineID]ChangeKind // removed or voided: a grant learned later does not undo it
	applied map[int]bool
}

func newView() view {
	return view{now: map[MachineID]bool{}, ever: map[MachineID]bool{}, adm: map[MachineID]Admission{},
		out: map[MachineID]ChangeKind{}, applied: map[int]bool{}}
}

func (v view) clone() view {
	c := newView()
	for k := range v.now {
		c.now[k] = true
	}
	for k := range v.ever {
		c.ever[k] = true
	}
	for k, a := range v.adm {
		c.adm[k] = a // admissions are never modified, only replaced
	}
	for k, x := range v.out {
		c.out[k] = x
	}
	for k := range v.applied {
		c.applied[k] = true
	}
	return c
}

// restrict is the more restrictive of two grants for one machine.
func restrict(a, b Admission) Admission {
	switch {
	case a.History:
		return b
	case b.History:
		return a
	}
	h := copyInts(a.Heads)
	for s, n := range b.Heads {
		if n > h[s] {
			h[s] = n
		}
	}
	return Admission{Heads: h}
}

// apply makes one change part of this machine's view.
func (m *Machine) apply(c Change, now Time) {
	v := m.st.view
	if v.applied[c.N] {
		return
	}
	v.applied[c.N] = true
	switch c.Kind {
	case Add:
		switch v.out[c.Machine] {
		case 0:
			v.now[c.Machine], v.ever[c.Machine] = true, true
		case Remove:
			v.ever[c.Machine] = true
		}
		if a, ok := v.adm[c.Machine]; ok {
			v.adm[c.Machine] = restrict(a, c.Adm)
		} else {
			v.adm[c.Machine] = c.Adm
		}
		if c.Machine == m.ID {
			m.st.adm = v.adm[m.ID]
			for _, h := range m.st.recs {
				m.setBody(h, h.body, h.dropped == Live, now)
			}
			m.dropFiles(now) // a narrower permission drops the files it hides
		}
	case Remove:
		delete(v.now, c.Machine)
		if v.out[c.Machine] == 0 {
			v.out[c.Machine] = Remove
		}
	case Void:
		v.out[c.Machine] = Void
		delete(v.now, c.Machine)
		delete(v.ever, c.Machine)
		if c.Machine != m.ID {
			m.dropAuthor(c.Machine)
			m.dropFiles(now) // with its records go the files they referred to
		} else {
			for n := range m.st.files.staged { // F11: nothing staged here can be sent now
				m.dropStaged(n)
			}
			m.st.files.flush()
		}
	}
}

// dropAuthor removes every record by x, whose admission was voided. Agents
// that were shown one of its messages are told.
func (m *Machine) dropAuthor(x MachineID) {
	st := m.st
	gone := map[RecordID]bool{}
	for id, h := range st.recs {
		if h.rec.Author == x {
			gone[id] = true
			delete(st.recs, id)
		}
	}
	for s := range st.contig {
		if s.Author == x {
			delete(st.contig, s)
			delete(st.at, s)
		}
	}
	for s := range st.known {
		if s.Author == x {
			delete(st.known, s)
		}
	}
	for _, idx := range []map[RecordID][]RecordID{st.withdrawn, st.replaced, st.handled, st.children} {
		for k, l := range idx {
			var keep []RecordID
			for _, id := range l {
				if !gone[id] {
					keep = append(keep, id)
				}
			}
			if keep == nil {
				delete(idx, k)
			} else {
				idx[k] = keep
			}
		}
	}
	for _, g := range st.agents {
		for id := range gone {
			if _, saw := g.seenMsg[id]; saw {
				g.told = append(g.told, id)
				delete(g.seenMsg, id)
			}
		}
		sort.Slice(g.told, func(i, j int) bool { return g.told[i] < g.told[j] })
		for s := range g.cursor {
			if s.Author == x {
				delete(g.cursor, s)
				delete(g.start, s)
			}
		}
		delete(g.heads, x)
	}
}

func (w *World) change(k ChangeKind, id MachineID, adm Admission) Change {
	c := Change{N: len(w.changes) + 1, Kind: k, Machine: id, Adm: adm}
	w.changes = append(w.changes, c)
	return c
}

// tellAll delivers a change to every machine at once.
func (w *World) tellAll(c Change) {
	for _, id := range w.order {
		if m := w.machines[id]; !m.Gone {
			m.apply(c, w.Now)
		}
	}
}

// Grant admits id, by member by, with or without history. Only by and the
// new machine know of it until others are told (Learn). Granting a machine
// already in the room records a second grant; the more restrictive applies.
func (w *World) Grant(id, by MachineID, history bool, agents ...string) (*Machine, error) {
	b, ok := w.machines[by]
	if !ok || b.Gone || !w.members[by] || by == id {
		return nil, ErrNoMachine
	}
	adm := Admission{Heads: copyInts(b.st.contig), History: history}
	m := w.machines[id]
	switch {
	case m == nil:
		m = w.newMachine(id, agents, adm)
		m.st.view = b.st.view.clone() // it learns the room from its admitter
		w.admission[id] = adm
	case w.members[id]:
		w.admission[id] = restrict(w.admission[id], adm)
	default:
		return nil, ErrNotAllow // a removed machine comes back as a new one
	}
	c := w.change(Add, id, adm)
	b.apply(c, w.Now)
	m.apply(c, w.Now)
	return m, nil
}

// Remove ends id's membership, decided on member by, which must know it as
// a member. Its records stay.
func (w *World) Remove(id, by MachineID) error {
	if !w.members[id] || !w.members[by] || id == by || !w.machines[by].st.view.now[id] {
		return ErrNotAllow
	}
	delete(w.members, id)
	w.machines[by].apply(w.change(Remove, id, Admission{}), w.Now)
	return nil
}

// VoidAdmission records, on member by, that id's admission never counted.
func (w *World) VoidAdmission(id, by MachineID) error {
	if w.machines[id] == nil || w.voided(id) || !w.members[by] || id == by || !w.machines[by].st.view.ever[id] {
		return ErrNotAllow
	}
	delete(w.members, id)
	w.machines[by].apply(w.change(Void, id, Admission{}), w.Now)
	return nil
}

func (w *World) voided(id MachineID) bool {
	for _, c := range w.changes {
		if c.Kind == Void && c.Machine == id {
			return true
		}
	}
	return false
}

// Learn tells machine id the oldest change it has not applied. It reports
// whether there was one.
func (w *World) Learn(id MachineID) (bool, error) {
	m, err := w.awake(id)
	if err != nil {
		return false, err
	}
	for _, c := range w.changes {
		if !m.st.view.applied[c.N] {
			m.apply(c, w.Now)
			return true, nil
		}
	}
	return false, nil
}

// knows reports whether machine m has applied every change about id.
func (w *World) knows(m MachineID, id MachineID) bool {
	for _, c := range w.changes {
		if c.Machine == id && !w.machines[m].st.view.applied[c.N] {
			return false
		}
	}
	return true
}
