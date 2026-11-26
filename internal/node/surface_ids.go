package node

// How the surfaces write who wrote a message and which room it is in, so
// that a model or a person cannot take another machine's mail for this
// machine's (decision D-NEW-G-mcp-identity):
//
//   - Here on every entry says whether its author is this machine,
//     compared by the full machine key, never by the short id.
//   - A machine whose short id (8 hex characters, 32 bits) another known
//     machine shares is written with its full key, in entries, writers,
//     status and the person's id, and marked (IDShared).
//   - Only the local room is shown as "local". Another room whose label
//     reads as "local", or as another held room's label, is shown with
//     "#" and the start of its id.

import (
	"context"
	"errors"
	"strings"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// idForms says how machine ids are written: shared holds every full key
// whose short id another key known here shares.
type idForms struct {
	shared map[string]bool
}

// node is machine key n as shown: its short id, or the full key when
// another known machine shares that short id.
func (x idForms) node(n string) string {
	if x.shared[n] {
		return n
	}
	return short(n)
}

// agent is the agent id of label on machine n as shown.
func (x idForms) agent(label, n string) string { return label + "@" + x.node(n) }

// sharedShort returns the keys among keys whose short id another,
// different key among them has.
func sharedShort(keys []string) map[string]bool {
	by := map[string]map[string]bool{}
	for _, k := range keys {
		if k == "" {
			continue
		}
		s := short(k)
		if by[s] == nil {
			by[s] = map[string]bool{}
		}
		by[s][k] = true
	}
	var out map[string]bool
	for _, ks := range by {
		if len(ks) < 2 {
			continue
		}
		if out == nil {
			out = map[string]bool{}
		}
		for k := range ks {
			out[k] = true
		}
	}
	return out
}

// ids is how machine ids are written now: this machine, every member of
// every room held, and extra (authors and addressees about to be shown,
// who may have left a room) are compared.
func (s *Surface) ids(ctx context.Context, extra []string) (idForms, error) {
	rs, err := s.m.st.LogRooms(ctx)
	if err != nil {
		return idForms{}, err
	}
	keys := append([]string{s.m.ID}, extra...)
	for _, r := range rs {
		st, err := s.r.State(r.ID)
		if errors.Is(err, ErrNoRoom) {
			continue // set aside, or left meanwhile
		}
		if err != nil {
			return idForms{}, err
		}
		keys = append(keys, st.Members...)
	}
	return idForms{shared: sharedShort(keys)}, nil
}

// fromLabel is the agent label of an entry's author.
func fromLabel(e Entry) string {
	l, _, _ := strings.Cut(e.From, "@")
	return l
}

// marked fills in, on each entry, whether this machine wrote it (Here,
// from the full key) and the ids of its author and addressees as shown:
// a short id another known machine shares is written in full and the
// entry marked IDShared.
func (s *Surface) marked(ctx context.Context, es []Entry) ([]Entry, error) {
	if len(es) == 0 {
		return es, nil
	}
	x, err := s.ids(ctx, entryNodes(es))
	if err != nil {
		return es, err
	}
	return s.markWith(x, es), nil
}

// entryNodes are the machine keys of the authors and addressees of es.
func entryNodes(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.FromNode)
		for _, a := range e.To {
			out = append(out, a.Node)
		}
	}
	return out
}

// markWith is marked with the ids as shown x, from one snapshot of the
// memberships held.
func (s *Surface) markWith(x idForms, es []Entry) []Entry {
	for i := range es {
		e := &es[i]
		e.Here = e.FromNode == s.m.ID
		if x.shared[e.FromNode] {
			e.From, e.IDShared = x.agent(fromLabel(*e), e.FromNode), true
		}
		for _, a := range e.To {
			if x.shared[a.Node] {
				e.IDShared = true
			}
		}
		if e.IDShared && len(e.To) > 0 {
			e.ToIDs = make([]string, len(e.To))
			for k, a := range e.To {
				e.ToIDs[k] = x.agent(a.Agent, a.Node)
			}
		}
	}
	return es
}

// SharedIDs is every machine key known here (this machine and the
// members of every room held) whose short id another one shares: the
// keys written in full.
func (s *Surface) SharedIDs(ctx context.Context) (map[string]bool, error) {
	x, err := s.ids(ctx, nil)
	return x.shared, err
}

// IsPerson reports whether label is the reserved label of a machine's
// person, in any case: the label a person's answer comes under. Only
// human on this machine is this machine's person (Entry.Here).
func IsPerson(label string) bool { return strings.EqualFold(label, HumanLabel) }

// ---- room labels as shown ----

// roomTag is the part of a room id shown after "#" when two rooms read
// alike: the first 8 hex characters.
func roomTag(id string) string {
	h := strings.TrimPrefix(id, "r_")
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// ShownLabels is the label each room is shown with to an agent, from
// stored labels by room id; local is the local room's id. The local room
// is "local". Any other room whose label reads as "local", as another
// room's label or as another room's id (LabelKey) gets "#" and the start
// of its id; if that still reads alike, "#" and the whole id; if even
// that does, the bare room id. Each step is taken again until no two
// rooms read alike, so the result is unique: room ids are. The page and
// the CLI draw rooms that read alike their own way.
func ShownLabels(local string, stored map[string]string) map[string]string {
	step := make(map[string]int, len(stored)) // 0 as stored, 1 tag, 2 whole id, 3 bare id
	shownAt := func(id string) string {
		switch step[id] {
		case 0:
			return stored[id]
		case 1:
			return stored[id] + "#" + roomTag(id)
		case 2:
			return stored[id] + "#" + strings.TrimPrefix(id, "r_")
		}
		return id
	}
	out := make(map[string]string, len(stored))
	idKey := make(map[string]string, len(stored))
	for id := range stored {
		idKey[LabelKey(id)] = id
	}
	lk := LabelKey(LocalLabel)
	for {
		for id := range stored {
			out[id] = shownAt(id)
		}
		if _, ok := stored[local]; ok {
			out[local] = LocalLabel
		}
		by := map[string][]string{}
		for id, l := range out {
			by[LabelKey(l)] = append(by[LabelKey(l)], id)
		}
		again := false
		up := func(id string) {
			if id != local && step[id] < 3 {
				step[id]++
				again = true
			}
		}
		for k, ids := range by {
			if len(ids) > 1 {
				for _, id := range ids {
					up(id)
				}
				continue
			}
			if id := ids[0]; (k == lk && id != local) || (idKey[k] != "" && idKey[k] != id) {
				up(id)
			}
		}
		if !again {
			return out
		}
	}
}

// shownLabels is ShownLabels of the rooms rs.
func (s *Surface) shownLabels(rs []store.LogRoom) map[string]string {
	stored := make(map[string]string, len(rs))
	for _, r := range rs {
		stored[r.ID] = r.Label
	}
	return ShownLabels(s.local, stored)
}

// reservedLocal reports whether label reads as the local room's label.
func reservedLocal(label string) bool { return LabelKey(label) == LabelKey(LocalLabel) }

// AgentIDs are the agent ids of to as shown now: a short id another
// known machine shares is written in full. It never fails: if the
// memberships cannot be read, the ids are written with short ids, so a
// send that committed is never reported as failed.
func (s *Surface) AgentIDs(ctx context.Context, to []core.Addressee) []string {
	var extra []string
	for _, a := range to {
		extra = append(extra, a.Node)
	}
	x, _ := s.ids(ctx, extra)
	out := make([]string, 0, len(to))
	for _, a := range to {
		out = append(out, x.agent(a.Agent, a.Node))
	}
	return out
}
