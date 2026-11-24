package node

// Surface is the thin layer the local API, the MCP shim, the CLI, the page
// and the hooks call (PROTOCOL-next sections 6 and 7). It adds only what
// those surfaces need and the new path does not have by itself: rooms named
// by label, the default local room, agent ids (label@short node), the
// unread summary for hooks, waiting reads, history and trace. Behaviour
// stays in Machine, Rooms and Joiner; nothing here knows HTTP or JSON.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// LocalLabel is the label of the room only this machine is in.
const LocalLabel = "local"

// HumanLabel is the agent label of the person who runs this machine (as in
// v0.2): ask_human sends to it, and the page, the CLI and confirm act as it.
const HumanLabel = "human"

// Everyone, as the only element of to, sends to everyone in the room.
const Everyone = "everyone"

// Bounds of the surfaces.
const (
	DefaultInboxLimit = 50
	MaxInboxLimit     = 200
	DefaultHistory    = 50
	MaxHistory        = 500
	MaxWait           = 120 * time.Second
	firstLineRunes    = 80
	maxFirstLines     = 10
)

// NoticeV02 is shown once, by the first daemon run over a database that
// holds v0.2 messages.
const NoticeV02 = "This machine's database holds messages from a2p2 v0.2. That version's messages, rooms and " +
	"invites are not carried over and are no longer shown; pair your machines again with a2p2 room invite."

// SurfaceOpts configures a Surface.
type SurfaceOpts struct {
	Name   string          // this machine's label
	Joiner *Joiner         // nil: joining is unavailable
	Book   *AddrBook       // where peers' addresses are learned; nil: none
	Addrs  func() []string // where joiners and peers reach this machine; empty: networking is off
}

// Surface serves the surfaces of one machine.
type Surface struct {
	m      *Machine
	r      *Rooms
	j      *Joiner
	book   *AddrBook
	name   string
	addrs  func() []string
	local  string
	notice string
	// jmu serialises the joiner's accept and resume, so that one join is
	// followed by one caller at a time.
	jmu sync.Mutex
}

// OpenSurface prepares the surfaces over m and r: it creates the local
// room on first start (this machine its only member and admin), registers
// the human, and records whether the v0.2 notice is due (shown once).
func OpenSurface(ctx context.Context, m *Machine, r *Rooms, o SurfaceOpts) (*Surface, error) {
	s := &Surface{m: m, r: r, j: o.Joiner, book: o.Book, name: o.Name, addrs: o.Addrs, local: localRoomID(m.ID)}
	if s.addrs == nil {
		s.addrs = func() []string { return nil }
	}
	if _, err := r.Gen(s.local); errors.Is(err, ErrNoRoom) {
		if err := s.createLocal(ctx); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if _, err := s.Register(ctx, HumanLabel, HumanLabel, ""); err != nil {
		return nil, err
	}
	show, err := m.st.V02Notice(ctx)
	if err != nil {
		return nil, err
	}
	if show {
		s.notice = NoticeV02
	}
	if err := s.SeedBook(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// localRoomID is the local room's id: fixed per machine, so it is found
// again without being stored, and of the form of PROTOCOL-next section 0.
func localRoomID(node string) string {
	h := sha256.Sum256([]byte("a2p2 local room\x00" + node))
	return "r_" + hex.EncodeToString(h[:16])
}

func (s *Surface) createLocal(ctx context.Context) error {
	r := s.r
	e, err := core.NewEntry(r.m.key, core.EntryFields{Room: s.local, Seq: 1, Kind: core.EntryCreate, Label: LocalLabel,
		Created: r.m.now()})
	if err != nil {
		return err
	}
	v, err := e.Verified()
	if err != nil {
		return err
	}
	return r.pin(ctx, v)
}

// Node is this machine's node id; Name its label.
func (s *Surface) Node() string { return s.m.ID }
func (s *Surface) Name() string { return s.name }

// LocalRoom is the id of the local room.
func (s *Surface) LocalRoom() string { return s.local }

// HumanID is the agent id of this machine's person.
func (s *Surface) HumanID() string { return s.agentID(HumanLabel, s.m.ID) }

// Notice is the one-time notice of this run, if any.
func (s *Surface) Notice() string { return s.notice }

// Addrs are where other machines reach this one; empty when networking is off.
func (s *Surface) Addrs() []string { return s.addrs() }

func (s *Surface) agentID(label, node string) string { return label + "@" + short(node) }

// AgentID renders an addressee as an agent id.
func AgentID(a core.Addressee) string { return a.Agent + "@" + short(a.Node) }

// label checks that agent is an agent id of this machine and returns its label.
func (s *Surface) label(agent string) (string, error) {
	l, node, ok := strings.Cut(agent, "@")
	if !ok || node != short(s.m.ID) || !core.ValidLabel(l) {
		return "", userErr("%q is not an agent on this machine", agent)
	}
	return l, nil
}

// ---- rooms ----

// ResolveRoom accepts a room id or a label; empty means the local room. A
// label several rooms share is an error naming them.
func (s *Surface) ResolveRoom(ctx context.Context, name string) (string, error) {
	if name == "" {
		return s.local, nil
	}
	rooms, err := s.m.st.LogRooms(ctx)
	if err != nil {
		return "", err
	}
	var ids []string
	for _, r := range rooms {
		if r.ID == name {
			return r.ID, nil
		}
		if r.Label == name {
			ids = append(ids, r.ID)
		}
	}
	switch len(ids) {
	case 0:
		return "", userErr("no room %q here", name)
	case 1:
		return ids[0], nil
	}
	return "", userErr("several rooms are labelled %q: use one of their ids: %s", name, strings.Join(ids, ", "))
}

// roomOf finds the room holding record id (a message, or the record a
// notice's key names).
func (s *Surface) roomOf(ctx context.Context, id string) (string, error) {
	id = strings.TrimPrefix(id, "x:")
	rooms, err := s.m.st.LogRooms(ctx)
	if err != nil {
		return "", err
	}
	var found string
	err = s.m.st.View(ctx, func(rd *store.Reader) error {
		for _, r := range rooms {
			if _, ok, err := rd.Record(r.ID, id); err != nil {
				return err
			} else if ok {
				found = r.ID
				return nil
			}
		}
		return nil
	})
	if err == nil && found == "" {
		err = ErrNotHeld
	}
	return found, err
}

// CreateRoom creates a room with this machine as its only member and admin.
func (s *Surface) CreateRoom(ctx context.Context, label string) (RoomInfo, error) {
	if !core.ValidLabel(label) {
		return RoomInfo{}, userErr("a room label is 1 to 64 letters, digits, '.', '_' or '-', starting with a letter or digit")
	}
	id, err := s.r.CreateRoom(ctx, label)
	if err != nil {
		return RoomInfo{}, err
	}
	return s.r.Info(ctx, id)
}

// RoomList is room/list: every room, or one.
func (s *Surface) RoomList(ctx context.Context, room string) ([]RoomInfo, error) {
	if room == "" {
		return s.r.List(ctx)
	}
	id, err := s.ResolveRoom(ctx, room)
	if err != nil {
		return nil, err
	}
	info, err := s.r.Info(ctx, id)
	if err != nil {
		return nil, err
	}
	return []RoomInfo{info}, nil
}

// Invite issues an invite to room; networking must be on, since the joiner
// reaches this machine at its addresses.
func (s *Surface) Invite(ctx context.Context, room, role string, history bool) (Invitation, error) {
	id, err := s.ResolveRoom(ctx, room)
	if err != nil {
		return Invitation{}, err
	}
	if id == s.local {
		return Invitation{}, userErr("the local room is this machine only; create a room to share: a2p2 room create LABEL")
	}
	addrs := s.addrs()
	if len(addrs) == 0 {
		return Invitation{}, userErr("networking is off, so nobody could reach this machine to join; run: a2p2 net on")
	}
	if len(addrs) > MaxAddrs {
		addrs = addrs[:MaxAddrs]
	}
	return s.r.Invite(ctx, id, role, history, addrs)
}

// member resolves a machine named by its node id or a unique prefix of at
// least 8 hex digits among the room's current members.
func (s *Surface) member(room, node string) (string, error) {
	st, err := s.r.State(room)
	if err != nil {
		return "", err
	}
	if len(node) < 8 {
		return "", userErr("name the machine by its node id (at least its first 8 hex digits)")
	}
	var got []string
	for _, m := range st.Members {
		if strings.HasPrefix(m, node) {
			got = append(got, m)
		}
	}
	switch len(got) {
	case 0:
		return "", userErr("no member %q in the room", node)
	case 1:
		return got[0], nil
	}
	return "", userErr("%q names several members; give more of the node id", node)
}

// Admin proposes making a member an admin; a person confirms it (P3).
func (s *Surface) Admin(ctx context.Context, room, node string) (store.Request, error) {
	return s.change(ctx, room, node, core.EntryAdmin)
}

// Remove proposes removing a member; a person confirms it (P3).
func (s *Surface) Remove(ctx context.Context, room, node string) (store.Request, error) {
	return s.change(ctx, room, node, core.EntryRemove)
}

func (s *Surface) change(ctx context.Context, room, node string, k core.EntryKind) (store.Request, error) {
	id, err := s.ResolveRoom(ctx, room)
	if err != nil {
		return store.Request{}, err
	}
	target, err := s.member(id, node)
	if err != nil {
		return store.Request{}, err
	}
	return s.r.Propose(ctx, id, Change{Kind: k, Target: target})
}

// Leave stops carrying room. The local room cannot be left.
func (s *Surface) Leave(ctx context.Context, room string) error {
	id, err := s.ResolveRoom(ctx, room)
	if err != nil {
		return err
	}
	if id == s.local {
		return userErr("the local room cannot be left")
	}
	return s.r.Leave(ctx, id)
}

// Requests lists requests waiting for a person.
func (s *Surface) Requests(ctx context.Context) ([]store.Request, error) { return s.r.Requests(ctx) }

// Confirm is a person's decision on a request. No agent tool reaches it (P3).
func (s *Surface) Confirm(ctx context.Context, id string, approve bool) (Confirmation, error) {
	return s.r.Confirm(ctx, id, approve)
}

// ---- joining ----

// JoinPreview opens the invite in token. The admin's addresses in the
// token are learned at once, so that the room is pulled as soon as it is
// held.
func (s *Surface) JoinPreview(ctx context.Context, token string) (Preview, error) {
	if s.j == nil {
		return Preview{}, userErr("joining is not available")
	}
	tok, err := core.DecodeToken(token)
	if err != nil {
		return Preview{}, userErr("not an invite: %v", err)
	}
	p, err := s.j.JoinPreview(ctx, token)
	if err == nil && s.book != nil {
		s.book.Learn(p.Admin, tok.Addrs, time.Time{})
	}
	return p, err
}

// JoinAccept accepts the preview with id preview of the invite in token,
// which a person here approved, and waits up to wait for the outcome.
func (s *Surface) JoinAccept(ctx context.Context, token, preview string, wait time.Duration) (JoinOutcome, error) {
	if s.j == nil {
		return JoinOutcome{}, userErr("joining is not available")
	}
	if preview == "" {
		return JoinOutcome{}, userErr("name the preview approved (its id, from room/join)")
	}
	s.jmu.Lock()
	defer s.jmu.Unlock()
	o, err := s.j.JoinAccept(ctx, token, preview, min(wait, MaxWait))
	if err == nil {
		err = s.SeedBook(ctx)
	}
	return o, err
}

// ResumeJoins follows every join still waiting for the admin machine's
// person (the daemon calls it at start and then every few seconds).
func (s *Surface) ResumeJoins(ctx context.Context, wait time.Duration) ([]JoinOutcome, error) {
	if s.j == nil {
		return nil, nil
	}
	s.jmu.Lock()
	defer s.jmu.Unlock()
	o, err := s.j.Resume(ctx, wait)
	if err == nil {
		err = s.SeedBook(ctx)
	}
	return o, err
}

// SeedBook learns the admin machines' addresses from the joins recorded
// here (the address book is in memory; a restart relearns from them).
func (s *Surface) SeedBook(ctx context.Context) error {
	if s.book == nil {
		return nil
	}
	js, err := s.m.st.Joins(ctx, "")
	if err != nil {
		return err
	}
	for _, j := range js {
		if j.Admin != "" && len(j.Addrs) > 0 {
			s.book.Learn(j.Admin, j.Addrs, time.Time{})
		}
	}
	return nil
}

// ---- agents ----

// Register announces a local agent and returns its identity. The label
// defaults to "<harness>-<directory name>", so the same harness in the same
// project gets the same mailbox across sessions.
func (s *Surface) Register(ctx context.Context, harness, label, cwd string) (store.Agent, error) {
	harness, cwd = core.CleanLabel(harness), cleanDir(cwd)
	if label == "" {
		label = harness
		if cwd != "" {
			label += "-" + filepath.Base(cwd)
		}
	}
	label = core.CleanLabel(label)
	if label == HumanLabel && harness != HumanLabel {
		return store.Agent{}, userErr("the label %q is reserved", HumanLabel)
	}
	if !core.ValidLabel(label) {
		return store.Agent{}, userErr("bad agent label %q", label)
	}
	now := s.m.now()
	a := store.Agent{ID: s.agentID(label, s.m.ID), Label: label, Harness: harness, Cwd: cwd, LastSeen: now}
	if err := s.m.st.TouchAgent(ctx, a, now); err != nil {
		return store.Agent{}, err
	}
	if _, err := s.m.Register(ctx, label); err != nil {
		return store.Agent{}, err
	}
	return a, nil
}

func cleanDir(dir string) string {
	if dir == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return filepath.Clean(dir)
}

// Agents lists local agents, most recently seen first.
func (s *Surface) Agents(ctx context.Context) ([]store.Agent, error) { return s.m.st.Agents(ctx) }

// AgentFor finds the agent a harness hook speaks for: same harness, and the
// hook's directory is the agent's directory or inside it. Deepest wins.
func (s *Surface) AgentFor(ctx context.Context, harness, cwd string) (store.Agent, bool, error) {
	harness, cwd = core.CleanLabel(harness), cleanDir(cwd)
	agents, err := s.m.st.Agents(ctx)
	if err != nil {
		return store.Agent{}, false, err
	}
	var best store.Agent
	found := false
	for _, a := range agents {
		if a.Harness != harness || a.Cwd == "" {
			continue
		}
		if cwd == a.Cwd || strings.HasPrefix(cwd, strings.TrimSuffix(a.Cwd, "/")+"/") {
			if !found || len(a.Cwd) > len(best.Cwd) {
				best, found = a, true
			}
		}
	}
	return best, found, nil
}

// ---- writing ----

// SendInput is the send call (PROTOCOL-next section 7).
type SendInput struct {
	Agent      string
	Room       string
	Text       string
	To         []string // agent ids, or ["everyone"]
	ReplyTo    string
	AskHandled bool
	Replaces   string // with empty text: withdraw
	Choices    []string
}

// Send posts, replies, replaces or withdraws. With reply_to or replaces and
// no room, the room is the one holding that message.
func (s *Surface) Send(ctx context.Context, in SendInput) (Sent, error) {
	label, err := s.label(in.Agent)
	if err != nil {
		return Sent{}, err
	}
	var room string
	switch {
	case in.Room != "":
		room, err = s.ResolveRoom(ctx, in.Room)
	case in.Replaces != "":
		room, err = s.roomOf(ctx, in.Replaces)
	case in.ReplyTo != "":
		room, err = s.roomOf(ctx, in.ReplyTo)
	default:
		room = s.local
	}
	if err != nil {
		return Sent{}, err
	}
	if in.Replaces != "" {
		if in.ReplyTo != "" || len(in.To) > 0 || in.AskHandled || len(in.Choices) > 0 {
			return Sent{}, userErr("replaces takes only new text: the addressees, parent and choices stay as they were")
		}
		if strings.TrimSpace(in.Text) == "" {
			return s.m.Withdraw(ctx, room, label, in.Replaces)
		}
		return s.m.Replace(ctx, room, label, in.Replaces, in.Text)
	}
	if strings.TrimSpace(in.Text) == "" {
		return Sent{}, userErr("text must not be empty")
	}
	d := Draft{Room: room, Agent: label, Text: in.Text, Choices: in.Choices, ReplyTo: in.ReplyTo, AskHandled: in.AskHandled}
	if len(in.To) == 1 && in.To[0] == Everyone {
		d.Everyone = true
	} else if len(in.To) > 0 {
		if d.To, err = s.addressees(room, in.To); err != nil {
			return Sent{}, err
		}
	}
	return s.m.Send(ctx, d)
}

// addressees resolves agent ids (label@short node) against the room's
// current members.
func (s *Surface) addressees(room string, to []string) ([]core.Addressee, error) {
	st, err := s.r.State(room)
	if err != nil {
		return nil, err
	}
	var out []core.Addressee
	for _, a := range to {
		if a == Everyone {
			return nil, userErr("everyone is the only addressee when given")
		}
		l, node, ok := strings.Cut(a, "@")
		if !ok || !core.ValidLabel(l) || len(node) < 8 {
			return nil, userErr("%q is not an agent id (label@node, as in peers)", a)
		}
		var got []string
		for _, m := range st.Members {
			if strings.HasPrefix(m, node) {
				got = append(got, m)
			}
		}
		if len(got) != 1 {
			return nil, userErr("%q is not an agent of a machine in this room", a)
		}
		out = append(out, core.Addressee{Node: got[0], Agent: l})
	}
	return out, nil
}

// Ack reports that agent handled message id.
func (s *Surface) Ack(ctx context.Context, agent, id string) (Sent, error) {
	label, err := s.label(agent)
	if err != nil {
		return Sent{}, err
	}
	room, err := s.roomOf(ctx, id)
	if err != nil {
		return Sent{}, err
	}
	return s.m.Handled(ctx, room, label, id)
}

// ---- reading ----

// rooms is every room held, local first.
func (s *Surface) rooms(ctx context.Context) ([]store.LogRoom, error) {
	rs, err := s.m.st.LogRooms(ctx)
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].ID == s.local && rs[j].ID != s.local })
	return rs, err
}

// Inbox shows agent what it has not confirmed, in room or in every room,
// oldest first, at most limit entries, waiting up to wait for one. It marks
// nothing: Seen confirms.
func (s *Surface) Inbox(ctx context.Context, agent, room string, limit int, wait time.Duration) (InboxView, error) {
	label, err := s.label(agent)
	if err != nil {
		return InboxView{}, err
	}
	if room != "" {
		if room, err = s.ResolveRoom(ctx, room); err != nil {
			return InboxView{}, err
		}
	}
	switch {
	case limit < 0:
		return InboxView{}, userErr("limit must not be negative")
	case limit == 0:
		limit = DefaultInboxLimit
	case limit > MaxInboxLimit:
		limit = MaxInboxLimit
	}
	if wait < 0 || wait > MaxWait {
		return InboxView{}, userErr("wait is 0 to 120 seconds")
	}
	deadline := time.Now().Add(wait)
	for {
		wakes := s.wakes(ctx, room)
		v, err := s.inbox(ctx, label, room)
		if err != nil || len(v.Entries) > 0 || wait == 0 {
			if len(v.Entries) > limit {
				v.Entries = v.Entries[:limit]
			}
			return v, err
		}
		if !waitAny(ctx, wakes, deadline) {
			return v, ctx.Err()
		}
	}
}

func (s *Surface) inbox(ctx context.Context, label, room string) (InboxView, error) {
	var ids []string
	if room != "" {
		ids = []string{room}
	} else {
		rs, err := s.rooms(ctx)
		if err != nil {
			return InboxView{}, err
		}
		for _, r := range rs {
			ids = append(ids, r.ID)
		}
	}
	var out InboxView
	for _, id := range ids {
		v, err := s.m.Inbox(ctx, id, label)
		if errors.Is(err, ErrUnknownAgent) {
			return InboxView{}, userErr("agent %s is not registered; call register first", s.agentID(label, s.m.ID))
		}
		if err != nil {
			return InboxView{}, err
		}
		out.Entries = append(out.Entries, v.Entries...)
		out.Waiting = append(out.Waiting, v.Waiting...)
	}
	sortEntries(out.Entries)
	return out, nil
}

// wakes are channels closed at the next change of room (or of any room,
// and of the set of rooms).
func (s *Surface) wakes(ctx context.Context, room string) []<-chan struct{} {
	out := []<-chan struct{}{s.m.wakes.wait(roomSet)}
	if room != "" {
		return append(out, s.m.wakes.wait(room))
	}
	rs, _ := s.m.st.LogRooms(ctx)
	for _, r := range rs {
		out = append(out, s.m.wakes.wait(r.ID))
	}
	return out
}

// waitAny waits for one of chans, at most a second (so that a change no
// wake announces is seen soon) and not past deadline. It reports false if
// the deadline passed or ctx ended.
func waitAny(ctx context.Context, chans []<-chan struct{}, deadline time.Time) bool {
	d := time.Until(deadline)
	if d <= 0 {
		return false
	}
	t := time.NewTimer(min(d, time.Second))
	defer t.Stop()
	done := make(chan struct{})
	defer close(done)
	hit := make(chan struct{}, 1)
	for _, c := range chans {
		go func() {
			select {
			case <-c:
				select {
				case hit <- struct{}{}:
				default:
				}
			case <-done:
			}
		}()
	}
	select {
	case <-hit:
	case <-t.C:
	case <-ctx.Done():
		return false
	}
	return true
}

// Seen confirms what the inbox showed agent: each key with its version.
// A key is looked up in every room; results come back in the given order.
func (s *Surface) Seen(ctx context.Context, agent string, reads []store.Read) ([]SeenResult, error) {
	label, err := s.label(agent)
	if err != nil {
		return nil, err
	}
	out := make([]SeenResult, len(reads))
	byRoom := map[string][]int{}
	var order []string
	for i, rd := range reads {
		out[i] = SeenResult{Key: rd.Key, Version: rd.Version, Reason: SeenNotShown}
		room, err := s.roomOf(ctx, rd.Key)
		if errors.Is(err, ErrNotHeld) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if byRoom[room] == nil {
			order = append(order, room)
		}
		byRoom[room] = append(byRoom[room], i)
	}
	for _, room := range order {
		idx := byRoom[room]
		rs := make([]store.Read, len(idx))
		for k, i := range idx {
			rs[k] = reads[i]
		}
		res, err := s.m.Seen(ctx, room, label, rs)
		if err != nil {
			return nil, err
		}
		for k, i := range idx {
			if k < len(res) {
				out[i] = res[k]
			}
		}
	}
	return out, nil
}

// Thread shows the thread of message id (the whole thread when id starts
// one, else the branch under it). It marks nothing.
func (s *Surface) Thread(ctx context.Context, agent, id string) ([]Entry, error) {
	if _, err := s.label(agent); err != nil {
		return nil, err
	}
	room, err := s.roomOf(ctx, id)
	if err != nil {
		return nil, err
	}
	var thread string
	err = s.m.st.View(ctx, func(rd *store.Reader) error {
		h, _, err := rd.Record(room, id)
		if err == nil {
			thread = h.Record.Fields().Thread
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if thread == "" {
		return s.m.Thread(ctx, room, id)
	}
	return s.m.Branch(ctx, room, id)
}

// History is the room's messages held here with their text, oldest first:
// at most limit, before message before if given. It marks nothing; text
// this machine may not read is not held, so it is never shown.
func (s *Surface) History(ctx context.Context, room, before string, limit int) ([]Entry, error) {
	id, err := s.ResolveRoom(ctx, room)
	if err != nil {
		return nil, err
	}
	switch {
	case limit < 0:
		return nil, userErr("limit must not be negative")
	case limit == 0:
		limit = DefaultHistory
	case limit > MaxHistory:
		limit = MaxHistory
	}
	var out []Entry
	err = s.m.view(ctx, id, func(v *roomView) error {
		for _, st := range v.streams {
			hs, err := v.rd.RecordsFrom(id, st.Author, st.Epoch, 1)
			if err != nil {
				return err
			}
			for _, h := range hs {
				x := v.keep(h)
				if x.f.Kind != core.KindMessage || v.hidden(x.author) {
					continue
				}
				e, err := v.threadEntry(x)
				if err != nil {
					return err
				}
				if e != nil && e.Kind == EntryMessage {
					out = append(out, *e)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortEntries(out)
	if before != "" {
		i := slices.IndexFunc(out, func(e Entry) bool { return e.ID == before })
		if i < 0 {
			return nil, userErr("message %q is not in the room's history here", before)
		}
		out = out[:i]
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// TraceView is what is known here of one message (A4).
type TraceView struct {
	ID      string
	Room    string
	Saved   bool   // the message is saved on this machine
	Reason  string // its text: live, expired, withdrawn, not_admitted or missing
	Copies  []CopyView
	Handled []HandledView
}

// CopyView is a machine that reported holding the message, when.
type CopyView struct {
	Node   string
	At     time.Time
	Member bool // a member of the room now, in this machine's view
}

// HandledView is a handling report held here.
type HandledView struct {
	Agent string
	At    time.Time
}

// Trace shows where message id got to: saved here, the reason its text is
// or is not here, the copies other machines reported (dated observations)
// and the handling reports held.
func (s *Surface) Trace(ctx context.Context, id string) (TraceView, error) {
	room, err := s.roomOf(ctx, id)
	if err != nil {
		return TraceView{}, err
	}
	st, err := s.r.State(room)
	if err != nil {
		return TraceView{}, err
	}
	t := TraceView{ID: id, Room: room, Saved: true}
	var author, epoch string
	var seq int64
	err = s.m.view(ctx, room, func(v *roomView) error {
		x, err := v.get(id)
		if err != nil || x == nil {
			if err == nil {
				err = ErrNotHeld
			}
			return err
		}
		if x.f.Kind != core.KindMessage {
			return userErr("%s is not a message", id)
		}
		t.Reason, author, epoch, seq = x.reason, x.author, x.f.Epoch, x.f.Seq
		hs, err := v.refs(id)
		if err != nil {
			return err
		}
		for _, h := range hs {
			if h.f.Kind == core.KindHandled && h.h.Effect == store.EffectOK {
				t.Handled = append(t.Handled, HandledView{Agent: s.agentID(h.f.Agent, h.author), At: h.f.Created})
			}
		}
		return nil
	})
	if err != nil {
		return TraceView{}, err
	}
	cs, err := s.m.st.Copies(ctx, room, author, epoch, seq)
	if err != nil {
		return TraceView{}, err
	}
	for _, c := range cs {
		t.Copies = append(t.Copies, CopyView{Node: c.Peer, At: c.At, Member: slices.Contains(st.Members, c.Peer)})
	}
	return t, nil
}

// UnreadView is the cheap summary for hooks (U4): counts per room and,
// when asked, first lines. It never marks anything seen.
type UnreadView struct {
	Agent string
	Count int
	From  []string // agent ids of the authors, first seen first
	Rooms []RoomCount
	First []FirstLine
}

// RoomCount is the unread count of one room.
type RoomCount struct {
	Room  string
	Label string
	Count int
}

// FirstLine is an unread message's first line, clipped.
type FirstLine struct {
	ID   string
	Room string
	From string
	Line string
}

// Unread summarises agent's unread entries without marking any seen;
// lines asks for first lines too.
func (s *Surface) Unread(ctx context.Context, agent string, lines bool) (UnreadView, error) {
	label, err := s.label(agent)
	if err != nil {
		return UnreadView{}, err
	}
	rs, err := s.rooms(ctx)
	if err != nil {
		return UnreadView{}, err
	}
	u := UnreadView{Agent: agent}
	seen := map[string]bool{}
	for _, r := range rs {
		v, err := s.m.Inbox(ctx, r.ID, label)
		if err != nil {
			return UnreadView{}, err
		}
		if len(v.Entries) == 0 {
			continue
		}
		u.Count += len(v.Entries)
		u.Rooms = append(u.Rooms, RoomCount{Room: r.ID, Label: r.Label, Count: len(v.Entries)})
		for _, e := range v.Entries {
			if !seen[e.From] {
				seen[e.From] = true
				u.From = append(u.From, e.From)
			}
			if lines && len(u.First) < maxFirstLines {
				u.First = append(u.First, FirstLine{ID: e.ID, Room: r.ID, From: e.From, Line: firstLine(e)})
			}
		}
	}
	return u, nil
}

func firstLine(e Entry) string {
	if e.Kind != EntryMessage {
		return "(" + e.Kind + ")"
	}
	line, _, _ := strings.Cut(e.Text, "\n")
	if utf8.RuneCountInString(line) > firstLineRunes {
		line = string([]rune(line)[:firstLineRunes]) + "..."
	}
	return line
}

// ---- status, peers and alerts ----

// PeerView is another machine this one shares a room with.
type PeerView struct {
	Node  string
	Name  string
	Rooms []string
	Addrs []string
	// LastHeard is when it last showed what it holds (the observed
	// table); zero if never in this run of its history.
	LastHeard time.Time
}

// StatusView is the machine at a glance: what whoami and peers show.
type StatusView struct {
	Node   string
	Name   string
	Human  string
	Local  string
	Rooms  []RoomInfo
	Agents []store.Agent
	Peers  []PeerView
	Notice string
}

// Status gathers rooms, agents and peers.
func (s *Surface) Status(ctx context.Context) (StatusView, error) {
	rooms, err := s.r.List(ctx)
	if err != nil {
		return StatusView{}, err
	}
	sort.SliceStable(rooms, func(i, j int) bool { return rooms[i].ID == s.local && rooms[j].ID != s.local })
	agents, err := s.Agents(ctx)
	if err != nil {
		return StatusView{}, err
	}
	peers, err := s.peers(ctx, rooms)
	if err != nil {
		return StatusView{}, err
	}
	return StatusView{Node: s.m.ID, Name: s.name, Human: s.HumanID(), Local: s.local, Rooms: rooms, Agents: agents,
		Peers: peers, Notice: s.notice}, nil
}

func (s *Surface) peers(ctx context.Context, rooms []RoomInfo) ([]PeerView, error) {
	byNode := map[string]*PeerView{}
	var order []string
	err := s.m.st.View(ctx, func(rd *store.Reader) error {
		for _, r := range rooms {
			for _, m := range r.Members {
				if m.Node == s.m.ID {
					continue
				}
				p := byNode[m.Node]
				if p == nil {
					p = &PeerView{Node: m.Node, Name: m.Name}
					if s.book != nil {
						p.Addrs = s.book.Addrs(m.Node)
					}
					byNode[m.Node] = p
					order = append(order, m.Node)
				}
				p.Rooms = append(p.Rooms, r.ID)
				obs, err := rd.Observed(r.ID, m.Node)
				if err != nil {
					return err
				}
				for _, o := range obs {
					if o.At.After(p.LastHeard) {
						p.LastHeard = o.At
					}
				}
			}
		}
		return nil
	})
	out := make([]PeerView, 0, len(order))
	for _, n := range order {
		out = append(out, *byNode[n])
	}
	return out, err
}

// Alerts lists what waits for a person; with wait, it waits up to wait for
// something when nothing waits now.
func (s *Surface) Alerts(ctx context.Context, wait time.Duration) ([]Alert, error) {
	if wait < 0 || wait > MaxWait {
		return nil, userErr("wait is 0 to 120 seconds")
	}
	deadline := time.Now().Add(wait)
	for {
		wakes := s.wakes(ctx, "")
		as, err := s.r.Alerts(ctx)
		if err != nil || len(as) > 0 || wait == 0 {
			return as, err
		}
		if !waitAny(ctx, wakes, deadline) {
			return as, ctx.Err()
		}
	}
}

// RequestText names exactly what approving request q does (P3).
func RequestText(q store.Request) string { return requestText(q) }
