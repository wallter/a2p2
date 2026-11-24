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

// NoticeV02 is shown to a person once ever, after the upgrade removed the
// v0.2 messages and rooms the database held (migration 10; PROTOCOL-next
// section 8).
const NoticeV02 = "This upgrade removed the messages and rooms this machine held from the earlier version of " +
	"a2p2 (v0.2); they cannot be read any more. Make your rooms again with a2p2 room create, and pair your " +
	"machines again with a2p2 room invite."

// SurfaceOpts configures a Surface.
type SurfaceOpts struct {
	Name   string          // this machine's label
	Joiner *Joiner         // nil: joining is unavailable
	Book   *AddrBook       // where peers' addresses are learned; nil: none
	Addrs  func() []string // where joiners and peers reach this machine; empty: no listener
	// Network is the networking state this run applies (zero: off).
	Network Network
	// NetworkFault, if set, says why networking on does not listen (the
	// address could not be bound, or the listener stopped); "" when it does.
	NetworkFault func() string
	// OnNetwork, if set, is called after a networking decision is
	// recorded: the daemon applies it at once.
	OnNetwork func(Network)
	// NotMember, if set, lists the peers that keep refusing this machine
	// as not a member of a room, with since when (Exchange.NotMemberSince).
	NotMember func(room string) map[string]time.Time
}

// MaybeRemovedAfter is how long every other member of a room must have
// refused this machine as not a member before the surfaces say it may
// have been removed.
const MaybeRemovedAfter = 10 * time.Minute

// MaybeRemovedText is what the surfaces say of such a room.
const MaybeRemovedText = "this machine may have been removed from this room; ask an admin"

// Surface serves the surfaces of one machine.
type Surface struct {
	m     *Machine
	r     *Rooms
	j     *Joiner
	book  *AddrBook
	name  string
	addrs func() []string
	local string
	net   Network
	fault func() string
	onNet func(Network)
	notMb func(room string) map[string]time.Time
	// waiting, if set (tests), is called each time Inbox starts to wait:
	// a readiness barrier.
	waiting func()
}

// OpenSurface prepares the surfaces over m and r: it creates the local
// room on first start (this machine its only member and admin) and
// registers the human.
func OpenSurface(ctx context.Context, m *Machine, r *Rooms, o SurfaceOpts) (*Surface, error) {
	s := &Surface{m: m, r: r, j: o.Joiner, book: o.Book, name: o.Name, addrs: o.Addrs, local: localRoomID(m.ID),
		net: o.Network, fault: o.NetworkFault, onNet: o.OnNetwork, notMb: o.NotMember}
	if s.fault == nil {
		s.fault = func() string { return "" }
	}
	if s.net.State == "" {
		s.net.State = NetworkOff
	}
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

// Notice returns the v0.2 notice while it has not been shown, for a
// person (show: status at the CLI, the page) and never for an agent tool.
// Reading it records nothing: the surface that printed or drew it calls
// NoticeShown afterwards, so a failed request or an unrendered page does
// not use it up.
func (s *Surface) Notice(ctx context.Context, show bool) (string, error) {
	if !show {
		return "", nil
	}
	pending, err := s.m.st.V02Pending(ctx)
	if err != nil || !pending {
		return "", err
	}
	return NoticeV02, nil
}

// NoticeShown records that a person was shown the notice: it is not
// returned again.
func (s *Surface) NoticeShown(ctx context.Context) error {
	_, err := s.m.st.V02Shown(ctx)
	return err
}

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
	info, err := s.r.Info(ctx, id)
	s.named([]RoomInfo{info})
	return info, err
}

// RoomList is room/list: every room, or one.
func (s *Surface) RoomList(ctx context.Context, room string) ([]RoomInfo, error) {
	if room == "" {
		rs, err := s.r.List(ctx)
		s.named(rs)
		return rs, err
	}
	id, err := s.ResolveRoom(ctx, room)
	if err != nil {
		return nil, err
	}
	info, err := s.r.Info(ctx, id)
	if err != nil {
		return nil, err
	}
	s.named([]RoomInfo{info})
	return []RoomInfo{info}, nil
}

// Invite issues an invite to room; this machine must listen, since the
// joiner reaches it at its addresses. An invite decides nothing: the
// admin machine's person approves the accept it leads to.
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
		return Invitation{}, userErr("networking is %s here, so no machine could reach this one to join; "+
			"a person can turn the listener on at a terminal: a2p2 net on", s.net.State)
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

// Admin proposes making a member an admin; confirm decides it, and no
// agent tool reaches confirm (P3).
func (s *Surface) Admin(ctx context.Context, room, node string) (store.Request, error) {
	return s.change(ctx, room, node, core.EntryAdmin)
}

// Remove proposes removing a member; confirm decides it, and no agent
// tool reaches confirm (P3).
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

// Confirm decides a request. No agent tool or hook reaches it (P3); any
// program running as this user can still call the socket that does.
//
// An approval that would sign before this machine has checked the room's
// log with another member since it started is not decided: it answers
// state failed with reason not_caught_up, the request stays pending, and
// the surface shows the risk and may repeat with notCaughtUpOK, the
// person's second, explicit approval, which is recorded (PROTOCOL-next 7).
func (s *Surface) Confirm(ctx context.Context, id string, approve, notCaughtUpOK bool) (Confirmation, error) {
	c, err := s.r.ConfirmWith(ctx, id, approve, ConfirmOpts{NotCaughtUpOK: notCaughtUpOK})
	if errors.Is(err, ErrNotCaughtUp) {
		return Confirmation{ID: id, State: store.RequestFailed, Reason: ReasonNotCaughtUp}, nil
	}
	return c, err
}

// NotCaughtUpWarning is what a surface shows before asking for the
// second approval after a confirm answered not_caught_up.
const NotCaughtUpWarning = "This machine has not checked the room's log with another member since it started. " +
	"If it was restored from a backup, approving now can damage the room's admin record (two different entries " +
	"signed at one position, a fork). Approve again only if every other member is gone or you are sure this " +
	"machine holds the room's latest log."

// ---- joining ----

// JoinPreview opens the invite in token. The admin's addresses in the
// token are learned at once, so that the room is pulled as soon as it is
// held.
func (s *Surface) JoinPreview(ctx context.Context, token string) (Preview, error) {
	if s.j == nil {
		return Preview{}, userErr("joining is not available")
	}
	if err := s.dials(); err != nil {
		return Preview{}, err
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
// approved at this machine (the CLI asks at a terminal; nothing proves who
// answered), and waits up to wait for the outcome.
func (s *Surface) JoinAccept(ctx context.Context, token, preview string, wait time.Duration) (JoinOutcome, error) {
	if s.j == nil {
		return JoinOutcome{}, userErr("joining is not available")
	}
	if preview == "" {
		return JoinOutcome{}, userErr("name the preview approved (its id, from room/join)")
	}
	if err := s.dials(); err != nil {
		return JoinOutcome{}, err
	}
	o, err := s.j.JoinAccept(ctx, token, preview, min(wait, MaxWait))
	if err == nil {
		err = s.SeedBook(ctx)
	}
	return o, err
}

// ResumeJoins follows every join still waiting for the admin machine's
// person (the daemon calls it at start and then every few seconds). With
// networking off it does nothing.
func (s *Surface) ResumeJoins(ctx context.Context, wait time.Duration) ([]JoinOutcome, error) {
	if s.j == nil || s.dials() != nil {
		return nil, nil
	}
	o, err := s.j.Resume(ctx, wait)
	if err == nil {
		err = s.SeedBook(ctx)
	}
	return o, err
}

// SeedBook learns the admin machines' addresses from the joins recorded
// here, through the book (which stores them when the daemon loaded it
// with LoadAddrBook): an admin the book dropped while the join waited, as
// not yet a member, is learned again once the join completes.
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
	// Thread, with ReplyTo and no To: address the parent's author and
	// every agent that has posted in the thread as this machine holds it
	// (the reply tool). Without it a reply goes to the parent's author.
	Thread bool
}

// Send posts, replies, replaces or withdraws. With reply_to or replaces and
// no room, the room is the one holding that message.
func (s *Surface) Send(ctx context.Context, in SendInput) (Sent, error) {
	label, err := s.label(in.Agent)
	if err != nil {
		return Sent{}, err
	}
	var room, ref string
	switch {
	case in.Room != "":
		room, err = s.ResolveRoom(ctx, in.Room)
	case in.Replaces != "":
		ref = in.Replaces
		room, err = s.roomOf(ctx, ref)
	case in.ReplyTo != "":
		ref = in.ReplyTo
		room, err = s.roomOf(ctx, ref)
	default:
		room = s.local
	}
	if errors.Is(err, ErrNotHeld) {
		return Sent{}, userErr("no message %s is held here: check the id (inbox and its thread view show ids)", ref)
	}
	if err != nil {
		return Sent{}, err
	}
	if in.Replaces != "" {
		if in.ReplyTo != "" || len(in.To) > 0 || in.AskHandled || len(in.Choices) > 0 {
			return Sent{}, userErr("replaces takes only new text: the addressees, parent and choices stay as they were")
		}
		if strings.TrimSpace(in.Text) == "" {
			sent, err := s.m.Withdraw(ctx, room, label, in.Replaces)
			return s.labelled(ctx, sent, err)
		}
		sent, err := s.m.Replace(ctx, room, label, in.Replaces, in.Text)
		return s.labelled(ctx, sent, err)
	}
	if strings.TrimSpace(in.Text) == "" {
		return Sent{}, userErr("text must not be empty")
	}
	d := Draft{Room: room, Agent: label, Text: in.Text, Choices: in.Choices, ReplyTo: in.ReplyTo, AskHandled: in.AskHandled}
	omitted := 0
	if len(in.To) == 1 && in.To[0] == Everyone {
		d.Everyone = true
	} else if len(in.To) > 0 {
		if d.To, err = s.addressees(room, in.To); err != nil {
			return Sent{}, err
		}
	} else if in.Thread && in.ReplyTo != "" {
		if d.To, omitted, err = s.participants(ctx, room, label, in.ReplyTo); err != nil {
			return Sent{}, err
		}
	}
	sent, err := s.m.Send(ctx, d)
	sent.ToOmitted = omitted
	return s.labelled(ctx, sent, err)
}

// labelled fills in the room label of a write's result.
func (s *Surface) labelled(ctx context.Context, sent Sent, err error) (Sent, error) {
	if err == nil {
		sent.Label, err = s.roomLabel(ctx, sent.Room)
	}
	return sent, err
}

// roomLabel is the label of room id ("" if not held).
func (s *Surface) roomLabel(ctx context.Context, id string) (string, error) {
	labels, err := s.roomLabels(ctx)
	return labels[id], err
}

func (s *Surface) roomLabels(ctx context.Context) (map[string]string, error) {
	rs, err := s.m.st.LogRooms(ctx)
	out := make(map[string]string, len(rs))
	for _, r := range rs {
		out[r.ID] = r.Label
	}
	return out, err
}

// withLabels fills in the room label of each entry.
func (s *Surface) withLabels(ctx context.Context, es []Entry, err error) ([]Entry, error) {
	if err != nil || len(es) == 0 {
		return es, err
	}
	labels, err := s.roomLabels(ctx)
	for i := range es {
		es[i].RoomLabel = labels[es[i].Room]
	}
	return es, err
}

// participants is whom a reply to parent addresses by default from the
// reply tool: the parent's author first, then every agent of a current
// member that has posted in the parent's thread as held here, newest post
// first, without the sender and without repeats, at most core.MaxTo; it
// also returns how many the limit left out. An empty list (the sender
// alone posted) leaves the record's own default, the parent's author.
func (s *Surface) participants(ctx context.Context, room, sender, parent string) ([]core.Addressee, int, error) {
	st, err := s.r.State(room)
	if err != nil {
		return nil, 0, err
	}
	member := map[string]bool{}
	for _, m := range st.Members {
		member[m] = true
	}
	var hs []store.Held
	var p store.Held
	err = s.m.st.View(ctx, func(rd *store.Reader) error {
		var ok bool
		if p, ok, err = rd.Record(room, parent); err != nil || !ok {
			return err
		}
		root := p.Record.Fields().Thread
		if root == "" {
			root = parent
		}
		hs, err = rd.ByThread(room, root)
		return err
	})
	if err != nil || p.Record == nil {
		return nil, 0, err // an unknown parent is refused by Send
	}
	sort.SliceStable(hs, func(i, j int) bool { return hs[i].Record.Fields().Created.After(hs[j].Record.Fields().Created) })
	self := core.Addressee{Node: s.m.ID, Agent: sender}
	seen := map[core.Addressee]bool{self: true}
	var out []core.Addressee
	omitted := 0
	add := func(r *core.Record, always bool) {
		a := core.Addressee{Node: r.Author(), Agent: r.Fields().Agent}
		if seen[a] || !always && !member[a.Node] {
			return
		}
		seen[a] = true
		if len(out) == core.MaxTo {
			omitted++
			return
		}
		out = append(out, a)
	}
	add(p.Record, true) // as a plain reply would address it
	for _, h := range hs {
		add(h.Record, false)
	}
	return out, omitted, nil
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
		if a == HumanLabel { // the person at this machine
			a = s.HumanID()
		}
		l, node, ok := strings.Cut(a, "@")
		if !ok || !core.ValidLabel(l) || len(node) < 8 {
			return nil, userErr("%q is not an agent id (label@machine, as peers lists them; or human, or everyone)", a)
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

// Ack reports that agent handled message id. Each refusal says why: the
// id is unknown here, the agent may not report it (not addressed, own
// message, not shown its text yet), or it reported it already.
func (s *Surface) Ack(ctx context.Context, agent, id string) (Sent, error) {
	label, err := s.label(agent)
	if err != nil {
		return Sent{}, err
	}
	room, err := s.roomOf(ctx, id)
	if err == nil {
		var h Sent
		if h, err = s.m.Handled(ctx, room, label, id); err == nil && h.Already {
			err = userErr("you already reported this message handled (report %s)", h.ID)
		} else if err == nil {
			return h, nil
		}
	}
	var ue *UserError
	switch {
	case errors.Is(err, ErrNotHeld):
		err = userErr("no message %s is held here: check the id (inbox and its thread view show ids)", id)
	case errors.As(err, &ue):
		err = ue // a refusal, as a plain user error
	}
	return Sent{}, err
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
		if s.waiting != nil {
			s.waiting()
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
	var err error
	out.Entries, err = s.withLabels(ctx, out.Entries, nil)
	return out, err
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
	var es []Entry
	if thread == "" {
		es, err = s.m.Thread(ctx, room, id)
	} else {
		es, err = s.m.Branch(ctx, room, id)
	}
	return s.withLabels(ctx, es, err)
}

// History is the room's messages held here with their text, oldest first:
// at most limit, before message before if given. Pages run by (created,
// id) through an index; before may name a message withdrawn or expired
// since, since its header stays. It marks nothing; text this machine may
// not read is not held, so it is never shown.
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
		var at time.Time
		after := ""
		if before != "" {
			h, ok, err := v.rd.Record(id, strings.TrimPrefix(before, "x:"))
			if err != nil {
				return err
			}
			if !ok || h.Record.Fields().Kind != core.KindMessage {
				return userErr("message %q is not in the room's history here", before)
			}
			at, after = h.Record.Fields().Created, h.Record.ID()
		}
		for len(out) < limit {
			hs, err := v.rd.MessagesBefore(id, at, after, limit)
			if err != nil {
				return err
			}
			for _, h := range hs {
				at, after = h.Record.Fields().Created, h.Record.ID()
				x := v.keep(h)
				if v.hidden(x.author) {
					continue
				}
				e, err := v.threadEntry(x)
				if err != nil {
					return err
				}
				if e != nil && e.Kind == EntryMessage && len(out) < limit {
					out = append(out, *e)
				}
			}
			if len(hs) < limit {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Reverse(out)
	return s.withLabels(ctx, out, nil)
}

// TraceView is what is known here of one message (A4).
type TraceView struct {
	ID   string
	Room string
	// Saved: this machine holds the message's signed header. Whether it
	// holds the text is Reason.
	Saved   bool
	Reason  string // its text here: live, expired, withdrawn, not_admitted or missing
	Copies  []CopyView
	Handled []HandledView
}

// CopyView is a machine that showed this one it holds the author's stream
// up to the message's position. That is position evidence only: it does
// not tell apart another record the author signed at the same position.
type CopyView struct {
	Node   string
	At     time.Time // when this machine observed it, not when the copy was made
	Member bool      // a member of the room now, in this machine's view (not then)
}

// HandledView is a handling report held here.
type HandledView struct {
	Agent string
	At    time.Time // when the report was written, as its author signed it
}

// Trace shows where message id got to: saved here, the reason its text is
// or is not here, the copies other machines reported (dated observations)
// and the handling reports held. An id not held here is an error.
func (s *Surface) Trace(ctx context.Context, id string) (TraceView, error) {
	room, err := s.roomOf(ctx, id)
	if errors.Is(err, ErrNotHeld) {
		return TraceView{}, userErr("no message %q is held on this machine", id)
	}
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
	// Waiting is the number of requests waiting for this machine's person.
	Waiting int
	// Network is what this run applies; Decided the latest decision, which
	// differs while it is being applied; Fault why networking on does not
	// listen.
	Network Network
	Decided Network
	Fault   string
	Notice  string
	// Writers is, per room id, the agents of current members seen writing
	// messages there, newest first: whom an agent can address.
	Writers map[string][]WriterView
}

// WriterView is an agent seen writing in a room.
type WriterView struct {
	Agent string // agent id, label@short node
	Node  string
	Last  time.Time
}

// maxWriters bounds the writers listed per room.
const maxWriters = 64

func (s *Surface) writers(ctx context.Context, rooms []RoomInfo) (map[string][]WriterView, error) {
	out := map[string][]WriterView{}
	err := s.m.st.View(ctx, func(rd *store.Reader) error {
		for _, r := range rooms {
			member := map[string]bool{}
			for _, m := range r.Members {
				member[m.Node] = true
			}
			ws, err := rd.Writers(r.ID, maxWriters)
			if err != nil {
				return err
			}
			for _, w := range ws {
				if member[w.Node] {
					out[r.ID] = append(out[r.ID], WriterView{Agent: s.agentID(w.Agent, w.Node), Node: w.Node, Last: w.Last})
				}
			}
		}
		return nil
	})
	return out, err
}

// Status gathers rooms, agents and peers. show is Notice's: true only when
// a person is shown the result.
func (s *Surface) Status(ctx context.Context, show bool) (StatusView, error) {
	rooms, err := s.r.List(ctx)
	if err != nil {
		return StatusView{}, err
	}
	s.named(rooms)
	sort.SliceStable(rooms, func(i, j int) bool { return rooms[i].ID == s.local && rooms[j].ID != s.local })
	agents, err := s.Agents(ctx)
	if err != nil {
		return StatusView{}, err
	}
	peers, err := s.peers(ctx, rooms)
	if err != nil {
		return StatusView{}, err
	}
	writers, err := s.writers(ctx, rooms)
	if err != nil {
		return StatusView{}, err
	}
	notice, err := s.Notice(ctx, show)
	if err != nil {
		return StatusView{}, err
	}
	waiting := 0
	for _, r := range rooms {
		waiting += len(r.Requests)
	}
	decided, err := LoadNetwork(ctx, s.m.st)
	if err != nil {
		return StatusView{}, err
	}
	return StatusView{Node: s.m.ID, Name: s.name, Human: s.HumanID(), Local: s.local, Rooms: rooms, Agents: agents,
		Peers: peers, Waiting: waiting, Network: s.net, Decided: decided, Fault: s.fault(), Notice: notice, Writers: writers}, nil
}

// mayBeRemoved: every other member of r has refused this machine as not a
// member, each for MaybeRemovedAfter.
func (s *Surface) mayBeRemoved(r RoomInfo) bool {
	if s.notMb == nil || r.ID == s.local {
		return false
	}
	refused, now, others := s.notMb(r.ID), s.m.now(), 0
	for _, m := range r.Members {
		if m.Node == s.m.ID {
			continue
		}
		others++
		since, ok := refused[m.Node]
		if !ok || now.Sub(since) < MaybeRemovedAfter {
			return false
		}
	}
	return others > 0
}

// named fills in this machine's own name where a room lists it without
// one (its create entry names nobody).
func (s *Surface) named(rooms []RoomInfo) {
	for i := range rooms {
		rooms[i].MayBeRemoved = s.mayBeRemoved(rooms[i])
		for k := range rooms[i].Members {
			if m := &rooms[i].Members[k]; m.Node == s.m.ID && m.Name == "" {
				m.Name = s.name
			}
		}
	}
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
		if err != nil {
			return nil, err
		}
		if rs, err := s.RoomList(ctx, ""); err == nil {
			for _, r := range rs {
				if r.MayBeRemoved {
					as = append(as, Alert{Kind: AlertRemoved, Room: r.ID, Text: "room " + r.Label + ": " + MaybeRemovedText})
				}
			}
		} else {
			return nil, err
		}
		if f := s.fault(); f != "" && s.net.State == NetworkOn {
			as = append(as, Alert{Kind: AlertNetwork, Text: "networking is on, " + f})
		}
		qs, err := s.Questions(ctx)
		if err != nil {
			return nil, err
		}
		for _, q := range qs {
			as = append(as, Alert{Kind: AlertQuestion, ID: q.ID, Room: q.Room, Since: q.Created, Text: q.From + " asks: " + q.Text})
		}
		if len(as) > 0 || wait == 0 {
			return as, err
		}
		if !waitAny(ctx, wakes, deadline) {
			return as, ctx.Err()
		}
	}
}
