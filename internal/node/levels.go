package node

// Reading levels (U1 to U3): how closely one agent follows one room. The
// rule is internal/model/delivery/levels.go, and the differential test
// holds the two together. A level is local reading state, like cursors:
// it changes nothing this machine stores or sends for others.
//
//   - all: every message for the agent, as before levels existed (the
//     default).
//   - addressed: messages naming the agent in to, and room-wide posts marked
//     for everyone: their text begins with "@everyone" (core.MarkedForEveryone;
//     D-reading, replacing D41's use of ask_handled, which now only asks for
//     handled reports). While a room-wide post's text is missing here its
//     mark is not known: it counts as let in, and the walk waits for the
//     text. Text that is gone (expired, withdrawn, not admitted) marks
//     nothing.
//   - none: no message reaches the inbox until the agent asks (Skipped).
//
// Notices about messages the agent was shown are never filtered (D9), nor
// is a message a full inbox listing showed (a receipt, store/shown.go): a
// level change never takes back what the inbox listed. When the level
// changes, every message held then that the old level kept out and the new
// one lets in is marked skipped, so raising a level never floods the live
// inbox with the past; Skipped lists them, and those whose text expired
// before the agent was shown it as expired (D7).

import (
	"context"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// marked says whether message r is marked for everyone, and whether that
// is known here: not while its text is missing.
func marked(r *rec) (bool, bool) {
	switch r.reason {
	case store.ReasonLive:
		return core.MarkedForEveryone(r.f.Text), true
	case store.ReasonMissing:
		return false, false
	}
	return false, true // the text is gone: nothing marks it
}

// admits says whether level lets message r, already relevant to the agent,
// into its inbox. A room-wide post whose mark is not known yet is let in
// at addressed: the walk waits for its text.
func admits(level string, r *rec) bool {
	switch level {
	case store.LevelAddressed:
		if len(r.f.To) > 0 {
			return true
		}
		m, known := marked(r)
		return m || !known
	case store.LevelNone:
		return false
	}
	return true
}

// listed says whether a full inbox listing showed the agent message r (its
// entry or its expired notice) and it has not confirmed it since.
func (g *agentState) listed(r *rec) bool {
	_, a := g.receipts[r.id]
	_, b := g.receipts["x:"+r.id]
	return a || b
}

// keptOut says whether message r, relevant to the agent, is kept out of its
// live inbox by its level, now or since the level last changed. A message
// an inbox listing showed is never kept out.
func (g *agentState) keptOut(r *rec) bool {
	return !g.listed(r) && (!admits(g.level, r) || g.skip[r.id])
}

// unconfirmed says whether the agent has neither confirmed message r nor
// reported it handled.
func (v *roomView) unconfirmed(g *agentState, agent string, r *rec) (bool, error) {
	for _, k := range []string{r.id, "x:" + r.id} {
		if seen, err := g.confirmed(k); err != nil || seen {
			return false, err
		}
	}
	done, err := v.handledByAgent(r.id, agent)
	return !done, err
}

// forAgent calls fn with every message held after where agent's time began
// in each stream it reads, that is relevant to it and that it has not
// confirmed.
func (v *roomView) forAgent(g *agentState, agent string, fn func(*rec) error) error {
	for _, s := range v.streams {
		k := streamKey{s.Author, s.Epoch}
		if u, err := v.unseeded(g, k); err != nil || u {
			if err != nil {
				return err
			}
			continue
		}
		for n := g.start[k] + 1; n <= s.Contig; n++ {
			r, err := v.at(k, n)
			if err != nil {
				return err
			}
			if r == nil || !relevant(r, v.self, agent) {
				continue
			}
			if ok, err := v.unconfirmed(g, agent, r); err != nil {
				return err
			} else if ok {
				if err := fn(r); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// skippedFor says whether message r is one Skipped lists for the agent:
// relevant to it, after its time began, kept out by its level and not
// confirmed.
func (v *roomView) skippedFor(g *agentState, agent string, r *rec) (bool, error) {
	k := r.stream()
	if r.f.Kind != core.KindMessage || !relevant(r, v.self, agent) || !g.keptOut(r) || r.f.Seq <= g.start[k] {
		return false, nil
	}
	if u, err := v.unseeded(g, k); err != nil || u {
		return false, err
	}
	return v.unconfirmed(g, agent, r)
}

// Level returns agent's level in room.
func (m *Machine) Level(ctx context.Context, room, agent string) (string, error) {
	var l string
	err := m.view(ctx, room, func(v *roomView) error {
		if err := v.requireAgent(agent); err != nil {
			return err
		}
		var err error
		l, err = v.rd.Level(agent, room)
		return err
	})
	return l, err
}

// SetLevel sets agent's level in room. In the same transaction, every
// message held now that the old level kept out and the new one lets in is
// marked skipped. It marks nothing seen.
func (m *Machine) SetLevel(ctx context.Context, room, agent, level string) error {
	if !store.ValidLevel(level) {
		return userErr("level is all, addressed or none")
	}
	if err := checkAgent(agent); err != nil {
		return err
	}
	if err := m.pruneIfDue(ctx); err != nil {
		return err
	}
	now := m.now()
	var expiredByClock bool
	err := m.st.SetLevel(ctx, agent, room, func(rd *store.Reader) (store.LevelPlan, error) {
		v := m.newView(rd, room, now)
		defer func() { expiredByClock = v.expiredByClock }()
		if err := v.load(); err != nil {
			return store.LevelPlan{}, err
		}
		if err := v.requireAgent(agent); err != nil {
			return store.LevelPlan{}, err
		}
		g, err := v.agent(agent)
		if err != nil {
			return store.LevelPlan{}, err
		}
		p := store.LevelPlan{Level: level}
		err = v.forAgent(g, agent, func(r *rec) error {
			if !g.listed(r) && !admits(g.level, r) && admits(level, r) {
				p.Skip = append(p.Skip, r.id)
			}
			return nil
		})
		return p, err
	})
	if expiredByClock {
		m.pruneDue.Store(true)
	}
	if err == nil {
		// A waiting inbox of agent here was woken by records for the
		// level its look read (touchWrite): it looks again at the new one.
		m.wakes.touchInbox(room, agent)
	}
	return err
}

// Skipped returns, oldest first, the messages for agent in room that its
// level kept out of its inbox and it has not confirmed (U3): with their
// text, or as expired when the text expired before the agent was shown it
// (D7). It marks nothing.
func (m *Machine) Skipped(ctx context.Context, room, agent string) ([]Entry, error) {
	var out []Entry
	err := m.view(ctx, room, func(v *roomView) error {
		if err := v.requireAgent(agent); err != nil {
			return err
		}
		g, err := v.agent(agent)
		if err != nil {
			return err
		}
		return v.forAgent(g, agent, func(r *rec) error {
			if !g.keptOut(r) || r.f.Kind != core.KindMessage {
				return nil
			}
			e, err := v.threadEntry(r)
			if e != nil {
				out = append(out, *e)
			}
			return err
		})
	})
	sortEntries(out)
	return out, err
}
