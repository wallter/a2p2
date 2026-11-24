package delivery

// Reading levels (U1 to U3). A level is one agent's choice, per room, of
// how closely it follows the room. It is kept on the agent's own machine
// and changes nothing any other machine stores or sends: it only narrows
// which messages the reading rule puts in that agent's inbox.
//
//   - All: every message for the agent (to it, or room-wide), as before
//     levels existed. The default.
//   - Addressed: messages that name the agent in to, and room-wide posts
//     marked for everyone. A room-wide post is marked for everyone when it
//     asks for handled reports (ask_handled): the signed formats are frozen
//     and that is the one existing field that tells such a post from an
//     ordinary one. Replies count when they name the agent, as the reply
//     tool does for the parent's author and the thread's posters.
//   - None: no message reaches the inbox; the agent reads what it skipped
//     when it asks (Skipped).
//
// Notices about messages the agent was already shown (withdrawn, replaced,
// voided) are not filtered: D9 promises the reader is told.
//
// Changing the level never makes earlier messages appear in the live
// inbox: every message held when the level changes that the old level
// kept out and the new one lets in is marked skipped, and stays out of the
// inbox. Skipped lists those, and everything the current level keeps out,
// without marking anything seen.

// Level is how closely an agent follows a room.
type Level uint8

const (
	All Level = iota
	Addressed
	None
)

func (l Level) String() string { return [...]string{"all", "addressed", "none"}[l] }

// admits says whether level l lets message r, already relevant to the
// agent, into its inbox.
func (l Level) admits(r *Record) bool {
	switch l {
	case All:
		return true
	case Addressed:
		return len(r.To) > 0 || r.AskHandled
	}
	return false
}

// keptOut says whether message r, relevant to the agent, is one its level
// keeps out of the live inbox, now or since the level last changed.
func (g *agentState) keptOut(r *Record) bool {
	return !g.level.admits(r) || g.skip[r.ID]
}

// unconfirmed says whether the agent has neither confirmed message r nor
// reported it handled, and r is after the agent's time began.
func (m *Machine) unconfirmed(g *agentState, agent string, r *Record) bool {
	return r.Seq > g.start[r.Stream()] && !g.seen[string(r.ID)] && !g.seen["x:"+string(r.ID)] &&
		!m.handledByAgent(r.ID, agent)
}

// SetLevel sets agent's level on machine id. Messages held now that the
// old level kept out and the new one lets in are marked skipped.
func (w *World) SetLevel(id MachineID, agent string, l Level) error {
	m, err := w.awake(id)
	if err != nil {
		return err
	}
	g := m.st.agents[agent]
	if g == nil {
		return ErrNoMachine
	}
	for _, h := range m.st.recs {
		r := h.rec
		if !relevant(r, m.ID, agent) || !m.unconfirmed(g, agent, r) {
			continue
		}
		if !g.level.admits(r) && l.admits(r) {
			g.skip[r.ID] = true
		}
	}
	g.level = l
	return nil
}

// LevelOf returns agent's level on machine id.
func (w *World) LevelOf(id MachineID, agent string) Level {
	if g := w.machines[id].st.agents[agent]; g != nil {
		return g.level
	}
	return All
}

// Skipped lists, oldest first, the messages for agent whose text is here
// and that its level kept out of its inbox and it has not confirmed. It
// marks nothing.
func (w *World) Skipped(id MachineID, agent string) []Entry {
	m := w.machines[id]
	m.prune(w.Now)
	g := m.st.agents[agent]
	if g == nil {
		return nil
	}
	var out []Entry
	for _, h := range m.st.recs {
		r := h.rec
		if !relevant(r, m.ID, agent) || !g.keptOut(r) || !m.unconfirmed(g, agent, r) || h.dropped != Live {
			continue
		}
		if e := m.threadEntry(h); e != nil {
			out = append(out, *e)
		}
	}
	sortEntries(out)
	return out
}
