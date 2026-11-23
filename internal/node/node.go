// Package node is the daemon's behaviour: what it means to send, receive,
// read and acknowledge. It knows nothing about HTTP, MCP or TLS; the local
// API, the MCP shim and the peer transport are thin layers over it.
package node

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// Node is one machine's participant in the mesh.
type Node struct {
	ID   string // hex Ed25519 public key
	Name string // human-friendly node name, not a credential

	st  *store.Store
	now func() time.Time

	mu      sync.Mutex
	notify  chan struct{}   // closed and replaced whenever a message is stored
	wake    chan struct{}   // nudges the outbox loop
	joining map[string]bool // rooms with a join in progress on this node

	roomMu sync.Mutex // serialises member-list changes for rooms this node owns
}

// UserError is a mistake by the caller (unknown room, bad argument). Layers
// above map it to a 4xx / tool error instead of an internal failure.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func userErr(format string, a ...any) error { return &UserError{fmt.Sprintf(format, a...)} }

// New prepares a node over an open store, creating the built-in local room
// and the human participant on first run.
func New(ctx context.Context, st *store.Store, id, name string, now func() time.Time) (*Node, error) {
	if !core.ValidNode(id) {
		return nil, fmt.Errorf("bad node id")
	}
	n := &Node{ID: id, Name: name, st: st, now: now, notify: make(chan struct{}), wake: make(chan struct{}, 1),
		joining: map[string]bool{}}
	if _, err := st.PutRoom(ctx, store.Room{ID: core.LocalRoom, Label: core.LocalRoom, Owner: id, Version: 1,
		Members: []store.Member{{Node: id, Name: name, Addrs: []string{}}}}, true, now()); err != nil {
		return nil, err
	}
	if _, err := n.Register(ctx, "human", core.HumanName, ""); err != nil {
		return nil, err
	}
	return n, nil
}

// HumanID is the agent id of this node's human.
func (n *Node) HumanID() string { return core.AgentID(core.HumanName, n.ID) }

func (n *Node) broadcast() {
	n.mu.Lock()
	close(n.notify)
	n.notify = make(chan struct{})
	n.mu.Unlock()
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

func (n *Node) changed() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.notify
}

// ---- agents ----

// Register announces a local agent and returns its identity. The label
// defaults to "<harness>-<directory name>", so the same harness in the same
// project always gets the same mailbox, across restarts and across sessions.
// Two concurrent sessions that should be distinct must pass distinct labels.
func (n *Node) Register(ctx context.Context, harness, label, cwd string) (store.Agent, error) {
	harness, cwd = core.CleanLabel(harness), canonDir(cwd)
	if label == "" {
		label = harness
		if cwd != "" {
			label += "-" + filepath.Base(cwd)
		}
	}
	label = core.CleanLabel(label)
	if label == core.HumanName && harness != "human" {
		return store.Agent{}, userErr("the label %q is reserved", core.HumanName)
	}
	a := store.Agent{ID: core.AgentID(label, n.ID), Label: label, Harness: harness, Cwd: cwd, LastSeen: n.now()}
	return a, n.st.TouchAgent(ctx, a, n.now())
}

// AgentFor finds the agent a harness hook is speaking for: same harness, and
// the hook's directory is the agent's directory or inside it. Deepest wins.
func (n *Node) AgentFor(ctx context.Context, harness, cwd string) (store.Agent, bool, error) {
	cwd = canonDir(cwd)
	agents, err := n.st.Agents(ctx)
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

// canonDir resolves symlinks so the same directory always compares equal
// (on macOS /tmp and /private/tmp are the same place under two names).
func canonDir(dir string) string {
	if dir == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return filepath.Clean(dir)
}

func (n *Node) local(agent string) error {
	if !strings.HasSuffix(agent, "@"+core.ShortNode(n.ID)) {
		return userErr("%q is not an agent on this node", agent)
	}
	return nil
}

// ---- rooms ----

// ResolveRoom accepts a room id or a label. An empty name means the local
// room. A label shared by several rooms is an error naming the candidates.
func (n *Node) ResolveRoom(ctx context.Context, name string) (store.Room, error) {
	if name == "" {
		name = core.LocalRoom
	}
	rooms, err := n.st.Rooms(ctx)
	if err != nil {
		return store.Room{}, err
	}
	var hits []store.Room
	for _, r := range rooms {
		if r.ID == name {
			return r, nil
		}
		if r.Label == name {
			hits = append(hits, r)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return store.Room{}, userErr("no room named %q; use the peers tool (or: a2p2 status) to list rooms", name)
	}
	ids := make([]string, len(hits))
	for i, r := range hits {
		ids[i] = r.ID
	}
	return store.Room{}, userErr("%d rooms are labelled %q; use an id: %s", len(hits), name, strings.Join(ids, ", "))
}

// Rooms lists the rooms this node is in.
func (n *Node) Rooms(ctx context.Context) ([]store.Room, error) { return n.st.Rooms(ctx) }

// Agents lists local agents.
func (n *Node) Agents(ctx context.Context) ([]store.Agent, error) { return n.st.Agents(ctx) }

// Senders lists agent ids recently active in a room (local and remote).
func (n *Node) Senders(ctx context.Context, room string) ([]string, error) {
	return n.st.Senders(ctx, room, 50)
}

// ---- sending ----

// SendReq is a request from a local agent to post in a room.
type SendReq struct {
	ID      string    `json:"id,omitempty"` // optional caller-chosen id; makes retries idempotent
	From    string    `json:"from"`         // local agent id
	Room    string    `json:"room,omitempty"`
	To      []string  `json:"to,omitempty"`
	Text    string    `json:"text"`
	ReplyTo string    `json:"reply_to,omitempty"`
	Kind    core.Kind `json:"kind,omitempty"`
	Choices []string  `json:"choices,omitempty"`
}

// Send validates and durably queues a message. It returns once the message is
// committed locally; it never waits for other nodes or for anyone to read it.
func (n *Node) Send(ctx context.Context, req SendReq) (store.Message, error) {
	if err := n.local(req.From); err != nil {
		return store.Message{}, err
	}
	now := n.now()
	e := core.Envelope{V: core.ProtocolVersion, ID: req.ID, FromNode: n.ID, From: req.From, To: req.To,
		Kind: req.Kind, ReplyTo: req.ReplyTo, CreatedAt: now, Seq: 1, // real seq assigned in the transaction
		Body: core.Body{Text: req.Text, Choices: req.Choices}}
	if e.ID == "" {
		e.ID = core.NewID(now)
	}
	if e.Kind == "" {
		e.Kind = core.KindMessage
	}
	if e.Kind == core.KindReceipt {
		return store.Message{}, userErr("receipts are created by ack, not send")
	}
	if e.Kind == core.KindAnswer && req.From != n.HumanID() {
		return store.Message{}, userErr("only the human can answer a question")
	}
	roomName := req.Room
	if req.ReplyTo != "" {
		orig, err := n.st.Get(ctx, req.ReplyTo)
		if errors.Is(err, store.ErrNotFound) {
			return store.Message{}, userErr("cannot reply: no message %s", req.ReplyTo)
		}
		if err != nil {
			return store.Message{}, err
		}
		if roomName == "" {
			roomName = orig.Room
		}
		e.Thread = orig.Thread
		if e.Thread == "" {
			e.Thread = orig.ID
		}
		if len(e.To) == 0 && orig.From != req.From {
			e.To = []string{orig.From}
		}
	}
	room, err := n.ResolveRoom(ctx, roomName)
	if err != nil {
		return store.Message{}, err
	}
	e.Room = room.ID
	if err := e.Validate(); err != nil {
		return store.Message{}, userErr("%v", err)
	}
	m, err := n.st.Send(ctx, &e, "", now)
	switch {
	case errors.Is(err, store.ErrConflict):
		return m, userErr("message id %s is already in use", e.ID)
	case errors.Is(err, store.ErrNotFound):
		return m, userErr("this node is no longer in room %q", room.Label)
	case errors.Is(err, core.ErrInvalid):
		return m, userErr("%v", err)
	case err != nil:
		return m, err
	}
	n.broadcast()
	return m, nil
}

// ---- reading ----

// Inbox returns unread messages for a local agent, oldest first, waiting up to
// wait for one to arrive. It does not mark them; the caller confirms with Seen
// after handing them to the agent, so a lost response is redelivered.
func (n *Node) Inbox(ctx context.Context, agent, room string, limit int, wait time.Duration) ([]store.Message, error) {
	if err := n.local(agent); err != nil {
		return nil, err
	}
	roomID := ""
	if room != "" {
		r, err := n.ResolveRoom(ctx, room)
		if err != nil {
			return nil, err
		}
		roomID = r.ID
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	if wait > 5*time.Minute {
		wait = 5 * time.Minute
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		ch := n.changed() // take the channel before querying so no arrival is missed
		msgs, err := n.st.Unread(ctx, agent, roomID, limit)
		if err != nil || len(msgs) > 0 || wait <= 0 {
			return msgs, err
		}
		select {
		case <-ch:
		case <-deadline.C:
			return msgs, nil
		case <-ctx.Done():
			return msgs, nil
		}
	}
}

// Seen records that messages were handed to the agent.
func (n *Node) Seen(ctx context.Context, agent string, ids ...string) error {
	if err := n.local(agent); err != nil {
		return err
	}
	return n.st.Mark(ctx, agent, core.ReceiptSeen, n.now(), ids...)
}

// Ack records that the agent handled a message and, if the message came from
// another node, queues a receipt addressed to that node only.
func (n *Node) Ack(ctx context.Context, agent, id string) error {
	if err := n.local(agent); err != nil {
		return err
	}
	orig, err := n.st.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return userErr("no message %s", id)
	}
	if err != nil {
		return err
	}
	now := n.now()
	if err := n.st.Mark(ctx, agent, core.ReceiptSeen, now, id); err != nil {
		return err
	}
	if orig.FromNode == n.ID {
		return n.st.Mark(ctx, agent, core.ReceiptHandled, now, id)
	}
	// Deterministic id: acknowledging twice sends one receipt. The receipt goes
	// only to the author's node; if that node has since left the room there is
	// nobody to tell, and the handled mark stays local.
	e := core.Envelope{V: core.ProtocolVersion, ID: receiptID(id, agent), Room: orig.Room, FromNode: n.ID, From: agent,
		To: []string{orig.From}, Kind: core.KindReceipt, CreatedAt: now, Seq: 1,
		Body: core.Body{Ref: id, State: core.ReceiptHandled}}
	if _, err := n.st.Send(ctx, &e, orig.FromNode, now); errors.Is(err, store.ErrNotFound) {
		return userErr("this node is no longer in that message's room")
	} else if err != nil {
		return err
	}
	n.broadcast()
	return nil
}

// History returns recent messages in a room, oldest first.
func (n *Node) History(ctx context.Context, room string, before int64, limit int) ([]store.Message, error) {
	r, err := n.ResolveRoom(ctx, room)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return n.st.History(ctx, r.ID, before, limit)
}

// Trace is everything known about one message.
type Trace struct {
	Message    store.Message    `json:"message"`
	Deliveries []store.Delivery `json:"deliveries"`
	Marks      []store.MarkRow  `json:"marks"`
}

// Trace returns a message's lifecycle: per-node delivery and per-agent marks.
func (n *Node) Trace(ctx context.Context, id string) (Trace, error) {
	m, err := n.st.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return Trace{}, userErr("no message %s", id)
	}
	if err != nil {
		return Trace{}, err
	}
	t := Trace{Message: m}
	if t.Deliveries, err = n.st.Deliveries(ctx, id); err != nil {
		return t, err
	}
	t.Marks, err = n.st.Marks(ctx, id)
	return t, err
}

// Status is the node's health at a glance.
type Status struct {
	Node   string              `json:"node"`
	Name   string              `json:"name"`
	Rooms  []store.Room        `json:"rooms"`
	Agents []store.Agent       `json:"agents"`
	Outbox []store.OutboxStats `json:"outbox"`
	Time   time.Time           `json:"time"`
}

func (n *Node) Status(ctx context.Context) (Status, error) {
	s := Status{Node: n.ID, Name: n.Name, Time: n.now()}
	var err error
	if s.Rooms, err = n.st.Rooms(ctx); err != nil {
		return s, err
	}
	if s.Agents, err = n.st.Agents(ctx); err != nil {
		return s, err
	}
	s.Outbox, err = n.st.Outbox(ctx)
	return s, err
}
