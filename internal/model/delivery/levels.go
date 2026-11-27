package delivery

// Reading levels (U1 to U3). A level is one agent's choice, per room, of
// how closely it follows the room. It is kept on the agent's own machine
// and changes nothing any other machine stores or sends: it only narrows
// which messages the reading rule puts in that agent's inbox.
//
//   - All: every message for the agent (to it, or room-wide), as before
//     levels existed. The default.
//   - Addressed: messages that name the agent in to, and room-wide posts
//     marked for everyone: their text begins with "@everyone" (D-reading,
//     which replaces D41's use of ask_handled; the signed formats are
//     frozen and the text is signed). ask_handled only asks for handled
//     reports. While a room-wide post's text is missing here its mark is
//     not known: it counts as let in, and the walk waits for the text as
//     for any message. Text that is gone (expired, withdrawn, not
//     admitted) marks nothing. Replies count when they name the agent, as
//     the reply tool does for the parent's author and the thread's posters.
//   - None: no message reaches the inbox; the agent reads what it skipped
//     when it asks (Skipped).
//
// Notices about messages the agent was already shown (withdrawn, replaced,
// voided) are not filtered: D9 promises the reader is told. Nor is a
// message a full inbox listing showed the agent (a receipt, List): a level
// change never takes back what the inbox listed, so it can still be
// confirmed and its later notices still reach the agent.
//
// Changing the level never makes earlier messages appear in the live
// inbox: every message held when the level changes that the old level
// kept out and the new one lets in is marked skipped, and stays out of the
// inbox. Skipped lists those, and everything the current level keeps out,
// without marking anything seen; a message whose text expired before the
// agent was shown it is listed as expired (D7).

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Level is how closely an agent follows a room.
type Level uint8

const (
	All Level = iota
	Addressed
	None
)

func (l Level) String() string { return [...]string{"all", "addressed", "none"}[l] }

// MarkedForEveryone reports whether text marks a room-wide post for
// everyone: it begins with "@everyone" (ASCII, any case), followed by
// whitespace, punctuation or the end of the text.
func MarkedForEveryone(text string) bool {
	const tag = "@everyone"
	if len(text) < len(tag) || !strings.EqualFold(text[:len(tag)], tag) {
		return false
	}
	if len(text) == len(tag) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(text[len(tag):])
	return unicode.IsSpace(r) || unicode.IsPunct(r)
}

// marked says whether message h is marked for everyone, and whether that
// is known here: not while its text is missing.
func (h *held) marked() (marked, known bool) {
	switch h.dropped {
	case Live:
		return MarkedForEveryone(h.body), true
	case Missing:
		return false, false
	}
	return false, true // the text is gone: nothing marks it
}

// admits says whether level l lets message h, already relevant to the
// agent, into its inbox. A room-wide post whose mark is not known yet is
// let in at Addressed: the walk waits for its text.
func (l Level) admits(h *held) bool {
	switch l {
	case All:
		return true
	case Addressed:
		if len(h.rec.To) > 0 {
			return true
		}
		marked, known := h.marked()
		return marked || !known
	}
	return false
}

// listed says whether a full inbox listing showed the agent message r (its
// entry, or its expired notice): it has a receipt.
func (g *agentState) listed(r *Record) bool {
	_, a := g.receipts[string(r.ID)]
	_, b := g.receipts["x:"+string(r.ID)]
	return a || b
}

// keptOut says whether message h, relevant to the agent, is one its level
// keeps out of the live inbox, now or since the level last changed. A
// message an inbox listing showed is never kept out.
func (g *agentState) keptOut(h *held) bool {
	return !g.listed(h.rec) && (!g.level.admits(h) || g.skip[h.rec.ID])
}

// unconfirmed says whether the agent has neither confirmed message r nor
// reported it handled, and r is in the agent's time (inTime).
func (m *Machine) unconfirmed(g *agentState, agent string, r *Record) bool {
	return g.inTime(r, m.ID, agent) && !g.seen[string(r.ID)] && !g.seen["x:"+string(r.ID)] &&
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
		if !g.listed(r) && !g.level.admits(h) && l.admits(h) {
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

// Skipped lists, oldest first, the messages for agent that its level kept
// out of its inbox and it has not confirmed: with their text, or as
// expired when the text expired before the agent was shown it (D7). It
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
		if !relevant(r, m.ID, agent) || !g.keptOut(h) || !m.unconfirmed(g, agent, r) {
			continue
		}
		if e := m.threadEntry(h); e != nil {
			out = append(out, *e)
		}
	}
	sortEntries(out)
	return out
}
