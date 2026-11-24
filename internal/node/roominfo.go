package node

// What a person is shown about a room's membership (room/list and alerts,
// PROTOCOL-next section 7; DESIGN-next section 3): members, admins, changes
// waiting for a second admin, entries without effect with their reasons,
// fork evidence, the stated limits that apply to the room's shape, and the
// requests waiting for this machine's person.

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/members"
	"github.com/wallter/a2p2/internal/store"
)

// MemberInfo is one current member.
type MemberInfo struct {
	Node  string
	Name  string
	Admin bool
}

// LogChange is one log entry as shown: what it would change and, unless it
// took effect, why not.
type LogChange struct {
	Entry  string
	Kind   string
	Author string
	Target string
	State  string // accepted, held or refused here
	Reason string
}

// ForkInfo is evidence that one key signed two entries at one position.
// Open means it is not settled: its key is still an admin, so an alert.
type ForkInfo struct {
	Author  string
	Seq     int64
	Entries [2]string // the one accepted here first
	Open    bool
}

// RoomInfo is one room as room/list shows it.
type RoomInfo struct {
	ID       string
	Label    string
	CreateID string
	Gen      int64
	Members  []MemberInfo
	Admins   []string
	Pending  []LogChange // waiting for a matching entry from another admin [M4]
	NoEffect []LogChange // with reasons (A4)
	Forks    []ForkInfo
	Limits   []string
	Requests []store.Request // waiting for this machine's person
}

// Limits stated for a room's shape (REQUIREMENTS section 6).
const (
	LimitOneAdmin = "While a room has had only one admin, that key is the room: a thief holding it can show a " +
		"different history to a machine that never received the real one, and a fork it signs cannot be settled."
	LimitTwoAdmins = "With two admins, losing one still leaves an admin who can add and remove machines, but the " +
		"lost key cannot be retired and no new admin can be named."
)

// Alert kinds (PROTOCOL-next section 7; "missing" comes with pull).
const (
	AlertRequest = "request"
	AlertFork    = "fork"
)

// Alert is something a person should act on.
type Alert struct {
	Kind  string
	ID    string // the request, or the second entry of the fork
	Room  string
	Text  string
	Since time.Time // zero for a fork: when evidence arrived is not kept
}

// Info returns what a person is shown about room.
func (r *Rooms) Info(ctx context.Context, room string) (RoomInfo, error) {
	r.mu.Lock()
	rl, err := r.room(room)
	var info RoomInfo
	if err == nil {
		info = r.info(rl)
	}
	r.mu.Unlock()
	if err != nil {
		return RoomInfo{}, err
	}
	qs, err := r.m.st.Requests(ctx, room, store.RequestPending)
	if err != nil {
		return RoomInfo{}, err
	}
	// A request whose invite has expired can no longer be approved: it is
	// not shown, although only Requests or Confirm record the expiry.
	now := r.m.now()
	for _, q := range qs {
		inv, err := r.m.st.InviteByRequest(ctx, q.ID)
		switch {
		case err == nil && !now.Before(inv.Expires):
			continue
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return RoomInfo{}, err
		}
		if err == nil {
			q.Deadline = inv.Expires
		}
		info.Requests = append(info.Requests, q)
	}
	return info, nil
}

// List returns Info for every room with a log, ordered by id.
func (r *Rooms) List(ctx context.Context) ([]RoomInfo, error) {
	r.mu.Lock()
	ids := sortedIDs(r.rooms)
	r.mu.Unlock()
	var out []RoomInfo
	for _, id := range ids {
		info, err := r.Info(ctx, id)
		if err == ErrNoRoom {
			continue // left meanwhile
		}
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

func (r *Rooms) info(rl *roomLog) RoomInfo {
	st := rl.st
	info := RoomInfo{ID: rl.id, Label: rl.label, CreateID: rl.createID, Gen: rl.gen, Admins: slices.Clone(st.Admins)}
	for _, n := range st.Members {
		info.Members = append(info.Members, MemberInfo{Node: n, Name: rl.cache[n].Name, Admin: slices.Contains(st.Admins, n)})
	}
	for _, id := range sortedIDs(rl.ents) {
		v, row := rl.ents[id], rl.state[id]
		f := v.Fields()
		if f.Kind == core.EntryCreate {
			continue
		}
		c := LogChange{Entry: id, Kind: string(f.Kind), Author: v.Author(), Target: f.Target, State: row.State, Reason: row.Reason}
		if row.State != store.LogAccepted {
			info.NoEffect = append(info.NoEffect, c)
			continue
		}
		eff := st.Effects[id]
		switch {
		case eff.Applied:
		case eff.Reason == members.ReasonNeedsSecond:
			c.Reason = eff.Reason
			info.Pending = append(info.Pending, c)
		default:
			c.Reason = eff.Reason
			info.NoEffect = append(info.NoEffect, c)
		}
	}
	for _, pair := range rl.mach.Evidence() {
		a := rl.ents[pair[0]]
		if a == nil {
			continue
		}
		info.Forks = append(info.Forks, ForkInfo{Author: a.Author(), Seq: a.Fields().Seq, Entries: pair,
			Open: slices.Contains(st.Admins, a.Author())})
	}
	switch len(st.Admins) {
	case 1:
		info.Limits = []string{LimitOneAdmin}
	case 2:
		info.Limits = []string{LimitTwoAdmins}
	}
	return info
}

// Alerts lists what waits for a person: requests to confirm, and forks not
// settled, in every room.
func (r *Rooms) Alerts(ctx context.Context) ([]Alert, error) {
	rooms, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []Alert
	for _, info := range rooms {
		for _, q := range info.Requests {
			out = append(out, Alert{Kind: AlertRequest, ID: q.ID, Room: info.ID, Since: q.Created,
				Text: requestText(q)})
		}
		for _, f := range info.Forks {
			if f.Open {
				out = append(out, Alert{Kind: AlertFork, ID: f.Entries[1], Room: info.ID,
					Text: "machine " + short(f.Author) + " signed two different log entries at position " +
						strconv.FormatInt(f.Seq, 10) + "; another admin's removal of it settles which stands"})
			}
		}
	}
	return out, nil
}

func requestText(q store.Request) string {
	verb := map[string]string{"add": "add", "remove": "remove", "admin": "make an admin of"}[q.Kind]
	s := verb + " machine " + short(q.Node)
	if q.Name != "" {
		s += " (" + q.Name + ")"
	}
	switch {
	case q.Action == ActionRejoin:
		s = "let machine " + short(q.Node) + " (" + q.Name + ") fetch the room's log again: already a member, " +
			"approval returns the log and changes nothing"
	case q.Kind == string(core.EntryAdd) && q.Action == ActionAdmitAdmin:
		s += " and make it an admin"
	}
	if q.Kind != string(core.EntryRemove) && q.Action != ActionPromote && q.Action != ActionRejoin {
		s += map[bool]string{true: ", with earlier history", false: ", without earlier history"}[q.History]
	}
	if q.Seconds != "" {
		s += ", seconding another admin's entry"
	}
	return s + ": waits for your confirmation"
}

func short(node string) string {
	if len(node) > 8 {
		return node[:8]
	}
	return node
}
