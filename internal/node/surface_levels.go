package node

// The surfaces of reading levels (U1 to U4): an agent reads and sets its
// own level per room, reads what its level kept out (Skipped), and the page
// and status show every local agent's level. The person's own label is not
// an agent with a level: what is addressed to the person is never filtered.

import (
	"context"
	"errors"
	"strings"

	"github.com/wallter/a2p2/internal/store"
)

// RoomLevel is one agent's level in one room.
type RoomLevel struct {
	Room  string
	Label string
	Level string
}

// levelRooms resolves room ("" is every room held, local first).
func (s *Surface) levelRooms(ctx context.Context, room string) ([]store.LogRoom, error) {
	rs, err := s.rooms(ctx)
	if err != nil || room == "" {
		return rs, err
	}
	id, err := s.ResolveRoom(ctx, room)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		if r.ID == id {
			return []store.LogRoom{r}, nil
		}
	}
	return nil, codedErr(ErrorNotFound, "no room %q here", room)
}

func (s *Surface) notRegistered(label string, err error) error {
	if errors.Is(err, ErrUnknownAgent) {
		return codedErr(ErrorNotRegistered, "agent %s is not registered; call register first", s.agentID(label, s.m.ID))
	}
	return err
}

// Levels returns agent's level in room, or in every room when room is "".
// With level set, it first sets agent's level there (U2): messages held now
// that the old level kept out and the new one lets in are marked skipped,
// and nothing is marked seen.
func (s *Surface) Levels(ctx context.Context, agent, room, level string) ([]RoomLevel, error) {
	label, err := s.label(agent)
	if err != nil {
		return nil, err
	}
	if level != "" && !store.ValidLevel(level) {
		return nil, userErr("level is all, addressed or none")
	}
	if level != "" && label == HumanLabel {
		return nil, userErr("the person's reading has no level: what is addressed to the person is never filtered")
	}
	rs, err := s.levelRooms(ctx, room)
	if err != nil {
		return nil, err
	}
	shown, err := s.roomLabels(ctx)
	if err != nil {
		return nil, err
	}
	out := []RoomLevel{}
	for _, r := range rs {
		if level != "" {
			if err := s.m.SetLevel(ctx, r.ID, label, level); err != nil {
				return nil, s.notRegistered(label, err)
			}
		}
		l, err := s.m.Level(ctx, r.ID, label)
		if err != nil {
			return nil, s.notRegistered(label, err)
		}
		out = append(out, RoomLevel{Room: r.ID, Label: shown[r.ID], Level: l})
	}
	return out, nil
}

// Skipped returns what agent's level kept out of its inbox and it has not
// confirmed, in room or in every room, oldest first (U3): the newest limit
// of them, before message before if given, so that pages reach everything
// within retention. Pages run in the order of every view (sortEntries);
// before may name a message confirmed since, whose header stays. It marks
// nothing seen; the agent may then report a message whose text it showed
// handled (Machine.Showed).
func (s *Surface) Skipped(ctx context.Context, agent, room, before string, limit int) ([]Entry, error) {
	label, err := s.label(agent)
	if err != nil {
		return nil, err
	}
	if limit, err = historyLimit(limit); err != nil {
		return nil, err
	}
	rs, err := s.levelRooms(ctx, room)
	if err != nil {
		return nil, err
	}
	var at *Entry
	if before != "" {
		broom, err := s.roomOf(ctx, strings.TrimPrefix(before, "x:"))
		if err == nil {
			err = s.m.view(ctx, broom, func(v *roomView) error {
				h, err := placeOf(v, broom, before, "this machine's rooms")
				if err == nil {
					at, err = v.entry(v.keep(h), EntryMessage, h.Record.ID())
				}
				return err
			})
		}
		if errors.Is(err, ErrNotHeld) {
			err = codedErr(ErrorNotFound, "message %q is not held here", before)
		}
		if err != nil {
			return nil, err
		}
	}
	var out []Entry
	for _, r := range rs {
		es, err := s.m.Skipped(ctx, r.ID, label)
		if err != nil {
			return nil, s.notRegistered(label, err)
		}
		for _, e := range es {
			if at == nil || entryBefore(e, *at) {
				e.Skipped = true
				out = append(out, e)
			}
		}
	}
	sortEntries(out)
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	if err := s.showed(ctx, label, out, false); err != nil {
		return nil, err
	}
	return s.withLabels(ctx, out, nil)
}

// AgentLevels lists every level that is not the default, with agent ids,
// for status and the page; an agent and room not listed is at "all".
func (s *Surface) AgentLevels(ctx context.Context) ([]store.AgentLevel, error) {
	ls, err := s.m.st.Levels(ctx)
	for i := range ls {
		ls[i].Agent = s.agentID(ls[i].Agent, s.m.ID)
	}
	return ls, err
}
