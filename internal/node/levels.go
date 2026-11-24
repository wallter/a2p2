package node

// Reading levels (U1 to U3): how closely one agent follows one room. The
// rule is internal/model/delivery/levels.go, and the differential test
// holds the two together. A level is local reading state, like cursors:
// it changes nothing this machine stores or sends for others.
//
//   - all: every message for the agent, as before levels existed (the
//     default).
//   - addressed: messages naming the agent in to, and room-wide posts marked
//     for everyone, which are those that ask for handled reports
//     (ask_handled; the signed formats are frozen, D27).
//   - none: no message reaches the inbox until the agent asks (Skipped).
//
// Notices about messages the agent was shown are never filtered (D9). When
// the level changes, every message held then that the old level kept out
// and the new one lets in is marked skipped, so raising a level never
// floods the live inbox with the past; Skipped lists them.

import (
	"context"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// admits says whether level lets message r, already relevant to the agent,
// into its inbox.
func admits(level string, r *rec) bool {
	switch level {
	case store.LevelAddressed:
		return len(r.f.To) > 0 || r.f.AskHandled
	case store.LevelNone:
		return false
	}
	return true
}

// keptOut says whether message r, relevant to the agent, is kept out of its
// live inbox by its level, now or since the level last changed.
func (g *agentState) keptOut(r *rec) bool {
	return !admits(g.level, r) || g.skip[r.id]
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
			if !admits(g.level, r) && admits(level, r) {
				p.Skip = append(p.Skip, r.id)
			}
			return nil
		})
		return p, err
	})
	if expiredByClock {
		m.pruneDue.Store(true)
	}
	return err
}

// Skipped returns, oldest first, the messages for agent in room whose text
// is here and that its level kept out of its inbox and it has not
// confirmed (U3). It marks nothing.
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
			if !g.keptOut(r) || r.reason != store.ReasonLive || r.f.Kind != core.KindMessage {
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
