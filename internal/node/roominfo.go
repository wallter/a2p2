package node

// What a person is shown about a room's membership (room/list and alerts,
// PROTOCOL section 7; DESIGN section 3): members, admins, changes
// waiting for a second admin, entries without effect with their reasons,
// fork evidence, the stated limits that apply to the room's shape, and the
// requests waiting for this machine's person.

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
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
	// MayBeRemoved: every other member has refused this machine as not a
	// member for MaybeRemovedAfter (a display rule; nothing is left or
	// deleted).
	MayBeRemoved bool
	ID           string
	Label        string
	CreateID     string
	Gen          int64
	Members      []MemberInfo
	Admins       []string
	Pending      []LogChange // waiting for a matching entry from another admin [M4]
	NoEffect     []LogChange // with reasons (A4)
	Forks        []ForkInfo
	Limits       []string
	Requests     []store.Request // waiting for this machine's person
}

// Limits stated for a room's shape (REQUIREMENTS section 6).
const (
	LimitOneAdmin = "While a room has had only one admin, that key is the room: a thief holding it can show a " +
		"different history to a machine that never received the real one, and a fork it signs cannot be settled."
	LimitTwoAdmins = "With two admins, losing one still leaves an admin who can add and remove machines, but the " +
		"lost key cannot be retired and no new admin can be named."
)

// Alert kinds (PROTOCOL section 7).
const (
	AlertRequest = "request"
	AlertFork    = "fork"
	AlertMissing = "missing" // a record's text not here a day after it was written
	// AlertQuestion: a message for this machine's person not yet answered
	// or dismissed (Surface.Questions).
	AlertQuestion = "question"
	// AlertNetwork: networking is on but this machine does not listen.
	AlertNetwork = "network"
	// AlertRemoved: every other member keeps refusing this machine.
	AlertRemoved = "removed"
)

// MissingAlertAfter is how long a record's text may be missing before a
// person is told.
const MissingAlertAfter = 24 * time.Hour

const maxMissingAlerts = 100

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
	labels := map[string]string{}
	for _, info := range rooms {
		labels[info.ID] = info.Label
		for _, q := range info.Requests {
			out = append(out, Alert{Kind: AlertRequest, ID: q.ID, Room: info.ID, Since: q.Created,
				Text: requestText(q, info.Label, info.Members)})
		}
		for _, f := range info.Forks {
			if f.Open {
				out = append(out, Alert{Kind: AlertFork, ID: f.Entries[1], Room: info.ID,
					Text: "machine " + short(f.Author) + " signed two different log entries at position " +
						strconv.FormatInt(f.Seq, 10) + "; another admin's removal of it settles which stands"})
			}
		}
	}
	ms, err := r.missing(ctx, labels)
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		out = append(out, Alert{Kind: AlertMissing, ID: m.ID, Room: m.Room, Since: m.Created,
			Text: "the text of record " + m.ID + " has not arrived here in the day since it was written; " +
				"no machine this one reached could hand it over"})
	}
	return out, nil
}

// missing lists up to maxMissingAlerts records of rooms held here whose
// text has been missing for MissingAlertAfter and would be shown to this
// machine's readers: not past retention by this machine's clock, readable
// under this machine's permission (not undecided), and after where its
// readers start. The rule is applied before the limit, page by page.
func (r *Rooms) missing(ctx context.Context, rooms map[string]string) ([]store.MissingRecord, error) {
	now := r.m.now()
	var out []store.MissingRecord
	var after store.MissingRecord
	for len(out) < maxMissingAlerts {
		page, err := r.m.st.MissingBetween(ctx, now.Add(-store.Retention), now.Add(-MissingAlertAfter), after, maxMissingAlerts)
		if err != nil {
			return nil, err
		}
		for _, m := range page {
			if _, ok := rooms[m.Room]; !ok {
				continue
			}
			ok, err := r.wantedText(ctx, m)
			if err != nil {
				return nil, err
			}
			if ok && len(out) < maxMissingAlerts {
				out = append(out, m)
			}
		}
		if len(page) < maxMissingAlerts {
			break
		}
		after = page[len(page)-1]
	}
	return out, nil
}

// wantedText reports whether a reader here would wait for m's text: this
// machine may hold it (readable, not undecided or hidden) and it is after
// the reading frontier of the author's stream.
func (r *Rooms) wantedText(ctx context.Context, m store.MissingRecord) (bool, error) {
	r.mu.Lock()
	rl, err := r.room(m.Room)
	if err != nil {
		r.mu.Unlock()
		return false, nil // left meanwhile
	}
	perm, member := rl.st.Permissions[r.m.ID]
	from := readFrom(perm, rl.lookup(nil))
	r.mu.Unlock()
	if !member {
		return false, nil
	}
	wanted := false
	err = r.m.view(ctx, m.Room, func(v *roomView) error {
		h, err := v.get(m.ID)
		if err != nil || h == nil {
			return err
		}
		if h.author == r.m.ID {
			wanted = true
			return nil
		}
		acc, err := textAccess(v, perm, h.author, h.f, h.id)
		if err != nil || acc != store.TextReadable {
			return err
		}
		for _, id := range from[h.author] {
			head, err := v.get(id)
			if err != nil {
				return err
			}
			if head == nil || head.author != h.author || head.id == h.id || v.descends(head, h) {
				return nil // start undecided, or at or before the start
			}
		}
		wanted = true
		return nil
	})
	return wanted, err
}

// requestText says exactly what approving q does, naming the machine by
// its full fingerprint and room by its label and id. The label and names
// can come from other machines, so each is escaped for one line
// (EscapeDisplay) and isolated (Isolate): the text a person approves
// cannot be rewritten or reordered by them.
// members are the room's members as this machine holds them, so that a
// seconding request names the machine as the room records it.
func requestText(q store.Request, label string, members []MemberInfo) string {
	if q.Kind == store.RequestNetwork {
		return "networking " + q.Action + ": " + NetworkText(Network{State: q.Action, Listen: q.Name})
	}
	m := "machine " + Fingerprint(q.Node) + machineNames(q, members)
	room := "room " + Isolate(EscapeDisplay(label)) + " (" + q.Room + ")"
	hist := map[bool]string{true: "with earlier history", false: "without earlier history"}[q.History]
	var s string
	switch q.Action {
	case ActionAdmit:
		s = "add " + m + " to " + room + " as a member, " + hist
	case ActionAdmitAdmin:
		s = "add " + m + " to " + room + " as an admin, " + hist
	case ActionPromote:
		s = "make " + m + ", a member of " + room + ", an admin"
	case ActionRemove:
		s = "remove " + m + " from " + room
	case ActionRejoin:
		s = "let " + m + ", already a member of " + room + " as " + an(q.Role) + ", " + hist +
			", fetch the room's log again; approving changes nothing"
	default:
		s = q.Action + " " + m + " in " + room
	}
	if q.Seconds != "" {
		s += ", seconding another admin's entry"
	}
	return s
}

// Who chose the name a request carries (RequestView.NameFrom).
const (
	// NameFromMachine: the machine asking to join chose its own name.
	NameFromMachine = "machine"
	// NameFromAdmin: another admin wrote the name into the entry this
	// request seconds.
	NameFromAdmin = "admin"
	// NameFromHere: this machine's person or agent proposed the change.
	NameFromHere = "here"
)

// nameFrom says who chose q's name, or "" when it has none.
func nameFrom(q store.Request) string {
	switch {
	case q.Name == "" || q.Kind == store.RequestNetwork || q.Kind == store.RequestSendFiles:
		return ""
	case q.Seconds != "":
		return NameFromAdmin
	case !q.Deadline.IsZero():
		return NameFromMachine
	}
	return NameFromHere
}

// memberNames are room's members with the names this machine records
// for them, or nil when room has no log here.
func (r *Rooms) memberNames(room string) []MemberInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.memberNamesLocked(room)
}

// memberNamesLocked is memberNames with r.mu held.
func (r *Rooms) memberNamesLocked(room string) []MemberInfo {
	if room == "" {
		return nil
	}
	rl, err := r.room(room)
	if err != nil {
		return nil
	}
	out := make([]MemberInfo, 0, len(rl.st.Members))
	for _, n := range rl.st.Members {
		out = append(out, MemberInfo{Node: n, Name: rl.cache[n].Name, Admin: slices.Contains(rl.st.Admins, n)})
	}
	return out
}

// recordedName is the name the room records for node, or "".
func recordedName(members []MemberInfo, node string) string {
	for _, mi := range members {
		if mi.Node == node {
			return mi.Name
		}
	}
	return ""
}

// maxSameName bounds how many other members with the target's name a
// request's text lists by fingerprint; the rest are counted.
const maxSameName = 3

// machineNames is how requestText names the target after its fingerprint.
// A name another admin wrote into the entry this request seconds is said
// to be that admin's, and the name the room records is shown beside it,
// so that the other admin cannot pass one machine off as another. Other
// members whose name is the one shown (the entry's or the room's record,
// whoever chose it, compared without case) are named too, up to
// maxSameName by fingerprint and the rest counted, so the text stays short
// in a room of any size.
func machineNames(q store.Request, members []MemberInfo) string {
	recRaw := recordedName(members, q.Node)
	name, rec := EscapeDisplay(q.Name), EscapeDisplay(recRaw)
	if name != "" {
		name = Isolate(name)
	}
	if rec != "" {
		rec = Isolate(rec)
	}
	var parts []string
	switch {
	case q.Seconds == "" && name != "":
		parts = append(parts, name)
	case q.Seconds == "":
	case name == "" && rec != "":
		parts = append(parts, "recorded in the room as \""+rec+"\"")
	case name == "":
	case rec == "" || rec == name:
		parts = append(parts, "named \""+name+"\" by the admin who signed the first entry")
	default:
		parts = append(parts, "recorded in the room as \""+rec+"\"", "named \""+name+"\" by the admin who signed the first entry")
	}
	shown := func(n string) bool {
		return n != "" && (q.Name != "" && strings.EqualFold(n, q.Name) || q.Seconds != "" && recRaw != "" && strings.EqualFold(n, recRaw))
	}
	var same []string
	count := 0
	for _, mi := range members {
		if mi.Node != q.Node && shown(mi.Name) {
			if count++; len(same) < maxSameName {
				same = append(same, Fingerprint(mi.Node))
			}
		}
	}
	switch {
	case count == 1:
		parts = append(parts, "machine "+same[0]+" in the room has this name too")
	case count > 1:
		s := "machines " + strings.Join(same, ", ")
		if count > len(same) {
			s += " and " + strconv.Itoa(count-len(same)) + " more"
		}
		parts = append(parts, s+" in the room have this name too")
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

func an(role string) string {
	if role == "admin" {
		return "an admin"
	}
	return "a " + role
}

// Fingerprint is a node id grouped for reading aloud and comparing: groups
// of four hex digits.
func Fingerprint(node string) string {
	var b strings.Builder
	for i := 0; i < len(node); i += 4 {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(node[i:min(i+4, len(node))])
	}
	return b.String()
}

func short(node string) string {
	if len(node) > 8 {
		return node[:8]
	}
	return node
}
