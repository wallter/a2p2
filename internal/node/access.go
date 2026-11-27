package node

// Membership and records ([RM1], [RM2] of internal/model/delivery and
// DESIGN sections 2 and 3). A record is stored only if its author is
// or was admitted (members.State.Admitted) or is this machine. Whether this
// machine may hold a record's text is its own current history permission:
// without history, text at or before a head of its admission frontier is
// not held (header only, not_admitted). A head it does not hold yet makes
// that author's later text undecided: not held, and the record waits as
// missing, so no reader's cursor passes text that may become readable when
// the head arrives (fail closed for reading, not for the cursor). A Closed
// permission (no effective grant in the current period) withholds all text
// until a usable permission exists. Its agents start reading at the
// admitter's positions. Hiding records of authors no longer admitted, and
// marking those no longer members, is done when reading (reading.go), from
// the member cache, so nothing is deleted for it.

import (
	"context"
	"slices"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/members"
	"github.com/wallter/a2p2/internal/store"
)

// ReceiveRecord places a verified record received from another machine
// (step 5 calls it for pages and offers) under the membership rules, in one
// transaction: refused with store.OutcomeAuthorNotAdmitted if its author
// was never admitted here; otherwise placed as store.PutRecord places it,
// with its text kept only within this machine's permission. Storing one of
// this machine's frontier heads re-decides the text held of that author
// and seeds cursors. gen, if not zero, must be the room's generation.
// Whether the peer it came from is a member now is the caller's check
// (step 5).
func (r *Rooms) ReceiveRecord(ctx context.Context, gen int64, rec *core.VerifiedRecord) (store.Outcome, error) {
	if rec == nil || rec.ID() == "" {
		return "", store.ErrUnverified
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f := rec.Fields()
	rl, err := r.room(f.Room)
	if err != nil {
		return "", err
	}
	if gen != 0 && gen != rl.gen {
		return "", ErrStaleGeneration
	}
	plan := r.recordPlan(rl.st, rl.lookup(nil), rec, r.m.now())
	if plan == nil {
		return store.OutcomeAuthorNotAdmitted, nil
	}
	out, err := r.m.st.PutRecordPlanned(ctx, rec, gen, plan)
	if err == nil {
		r.m.wakes.touch(f.Room)
	}
	return out, err
}

// recordPlan is ReceiveRecord's decision for rec under membership st (look
// finds its entries): nil if rec's author was never admitted; otherwise
// the plan to place it with, run inside the placing transaction.
func (r *Rooms) recordPlan(st members.State, look func(string) *core.VerifiedEntry, rec *core.VerifiedRecord,
	now time.Time) func(*store.Reader) (store.RecordPlan, error) {
	f := rec.Fields()
	author, id := rec.Author(), rec.ID()
	if author != r.m.ID && !slices.Contains(st.Admitted, author) {
		return nil
	}
	perm, member := st.Permissions[r.m.ID]
	from := readFrom(perm, look)
	var last *readFront // removed: the frontier it had (Rooms.setFrontier)
	if !member {
		if fr, ok := r.frontiers()[f.Room]; ok {
			last = &fr
		}
	}
	return func(rd *store.Reader) (store.RecordPlan, error) {
		p := store.RecordPlan{Now: now}
		switch {
		case author == r.m.ID:
			return p, nil
		case !member:
			p.Text = store.TextHidden // not a member here: headers only
			// A head of the frontier it had still decides what it held back.
			if last != nil && slices.Contains(last.From[author], id) {
				a := r.access(rd, f.Room, last.perm, last.From)
				p.Access = &a
			}
			return p, nil
		}
		v := r.m.newView(rd, f.Room, now) // loaded only if perm needs it (textAccess)
		var err error
		if p.Text, err = textAccess(v, perm, author, f, id); err != nil {
			return p, err
		}
		if slices.Contains(from[author], id) || slices.Contains(perm.From[author], id) {
			a := r.access(rd, f.Room, perm, from)
			p.Access = &a
		}
		return p, nil
	}
}

// readFrom is where this machine's agents start reading: the union per
// author of the from heads of the grants of its current membership period,
// whether or not they give history [R8]. History decides only whether
// earlier text is readable (perm.From).
func readFrom(perm members.Permission, look func(string) *core.VerifiedEntry) map[string][]string {
	out := map[string][]string{}
	for _, g := range perm.Grants {
		v := look(g)
		if v == nil {
			continue
		}
		for a, ids := range v.Fields().From {
			for _, id := range ids {
				if !slices.Contains(out[a], id) {
					out[a] = append(out[a], id)
				}
			}
		}
	}
	return out
}

// access is what this machine's permission perm and reading frontier from
// mean for the room's records: which text it may hold, and where its
// agents start. The view is loaded when first used, so it sees what the
// transaction wrote before.
func (r *Rooms) access(rd *store.Reader, room string, perm members.Permission, from map[string][]string) store.Access {
	var v *roomView
	view := func() (*roomView, error) {
		if v == nil {
			x := r.m.newView(rd, room, time.Time{})
			if err := x.load(); err != nil {
				return nil, err
			}
			v = x
		}
		return v, nil
	}
	return store.Access{
		Readable: func(author string, f core.RecordFields, id string) (store.TextAccess, error) {
			if author == r.m.ID {
				return store.TextReadable, nil
			}
			x, err := view()
			if err != nil {
				return store.TextHidden, err
			}
			return textAccess(x, perm, author, f, id)
		},
		Frontier: func(s store.Stream) (int64, error) {
			x, err := view()
			if err != nil {
				return 0, err
			}
			return frontier(x, from, s)
		},
	}
}

// textAccess decides whether perm lets this machine hold the text of the
// record (author, f, id): hidden if it is at or before a head of the
// frontier that is held, undecided if some head of the author is not held
// (or not the author's), readable otherwise.
func textAccess(v *roomView, perm members.Permission, author string, f core.RecordFields, id string) (store.TextAccess, error) {
	switch {
	case perm.All:
		return store.TextReadable, nil
	case perm.Closed:
		return store.TextHidden, nil
	}
	if err := v.ensure(); err != nil {
		return store.TextHidden, err
	}
	x := &rec{f: f, id: id, author: author}
	undecided := false
	for _, head := range perm.From[author] {
		h, err := v.get(head)
		if err != nil {
			return store.TextHidden, err
		}
		if h == nil || h.author != author {
			undecided = true
			continue
		}
		if h.id == id || v.descends(h, x) {
			return store.TextHidden, nil
		}
	}
	if undecided {
		return store.TextUndecided, nil
	}
	return store.TextReadable, nil
}

// frontier is where an agent of this machine starts reading stream s: the
// last position at or before a head of its reading frontier, or 0 (no
// seeding) when there is none for the author or not every head of the
// author is held yet: a cursor seeded from some of them could not be
// seeded again when the rest arrive (reading holds the streams back until
// then, roomView.unseeded).
func frontier(v *roomView, from map[string][]string, s store.Stream) (int64, error) {
	var n int64
	for _, head := range from[s.Author] {
		h, err := v.get(head)
		if err != nil {
			return 0, err
		}
		if h == nil || h.author != s.Author {
			return 0, nil
		}
		if p := reach(v, h)[s.Epoch]; p > n {
			n = p
		}
	}
	return n, nil
}

// reach is, per epoch of h's author, the last position at or before h:
// h's own stream up to h, and the streams its first record links to,
// transitively (as descends, but including h).
func reach(v *roomView, h *rec) map[string]int64 {
	out := map[string]int64{h.f.Epoch: h.f.Seq}
	todo := []string{h.f.Epoch}
	for len(todo) > 0 {
		s := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		first := v.first[streamKey{h.author, s}]
		if first == nil || out[s] < 1 {
			continue
		}
		for _, l := range first.f.Links {
			if l.Seq > out[l.Epoch] {
				out[l.Epoch] = l.Seq
				todo = append(todo, l.Epoch)
			}
		}
	}
	return out
}
