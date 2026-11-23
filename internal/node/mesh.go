package node

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	mrand "math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// Per-envelope outcomes of a delivery attempt.
const (
	ResStored   = "stored"   // committed by the receiver (also for duplicates)
	ResRejected = "rejected" // permanent: never retry this envelope
	ResRetry    = "retry"    // not now: stop this batch and try again later
)

// Tunables. Variables so tests can shorten them.
var (
	RetryBase       = time.Second
	RetryMax        = 5 * time.Minute
	RetentionExpiry = 7 * 24 * time.Hour  // give up on a queued delivery after this
	RetentionKeep   = 30 * 24 * time.Hour // delete messages older than this (must exceed RetentionExpiry)
	RefreshEvery    = time.Minute         // how often non-owners re-read member lists
	RefreshMinGap   = 2 * time.Second     // at most one refresh of a room per this interval
	Tick            = time.Second         // outbox scan interval when nothing wakes it
	InviteTTL       = 15 * time.Minute
)

// Result is the receiver's verdict on one envelope.
type Result struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// DeliverReq is a batch from one node to another. Versions carries the
// sender's member-list version per room so a stale receiver knows to refresh.
type DeliverReq struct {
	Envelopes []core.Envelope  `json:"envelopes"`
	Versions  map[string]int64 `json:"versions,omitempty"`
}

// JoinReq redeems an invite. The joining node's identity is the TLS identity
// of the connection, never a field in this request.
type JoinReq struct {
	Secret string   `json:"secret"`
	Name   string   `json:"name"`
	Addrs  []string `json:"addrs"`
}

// RoomReq asks a room's owner for the current member list and reports the
// asker's current addresses.
type RoomReq struct {
	Room  string   `json:"room"`
	Addrs []string `json:"addrs"`
}

// ErrForbidden is what a transport returns when the remote node refused us
// as a non-member. It is how a node learns it was removed from a room.
var ErrForbidden = errors.New("forbidden")

// Transport carries requests to another node. Implementations must
// authenticate the remote node as `to.Node` before sending anything.
type Transport interface {
	Deliver(ctx context.Context, to store.Member, req DeliverReq) ([]Result, error)
	Join(ctx context.Context, owner store.Member, req JoinReq) (store.Room, error)
	Room(ctx context.Context, owner store.Member, req RoomReq) (store.Room, error)
}

// ---- inbound (called by the peer listener with the authenticated node id) ----

// Receive stores a batch from fromNode and returns one result per envelope,
// stopping at the first that must be retried so per-sender order holds.
func (n *Node) Receive(ctx context.Context, fromNode string, req DeliverReq, refresh func(room string)) []Result {
	if len(req.Envelopes) > core.MaxBatch {
		return []Result{{Status: ResRejected, Reason: "batch too large"}}
	}
	out := make([]Result, 0, len(req.Envelopes))
	stored := false
	for i := range req.Envelopes {
		e := &req.Envelopes[i]
		e.FromNode = fromNode // identity comes from the connection, not the payload
		res := n.receiveOne(ctx, e, req.Versions, refresh)
		out = append(out, res)
		if res.Status == ResStored {
			stored = true
		}
		if res.Status == ResRetry {
			break
		}
	}
	if stored {
		n.broadcast()
	}
	return out
}

func (n *Node) receiveOne(ctx context.Context, e *core.Envelope, versions map[string]int64, refresh func(string)) Result {
	res := Result{ID: e.ID, Status: ResRejected}
	retry := func(reason string) Result { return Result{ID: e.ID, Status: ResRetry, Reason: reason} }
	if err := e.Validate(); err != nil {
		res.Reason = err.Error()
		return res
	}
	if e.Room == core.LocalRoom {
		res.Reason = "the local room does not accept remote messages"
		return res
	}
	room, err := n.st.Room(ctx, e.Room)
	if errors.Is(err, store.ErrNotFound) {
		n.mu.Lock()
		joining := n.joining[e.Room]
		n.mu.Unlock()
		if joining { // the owner admitted us and a message beat the join response home
			return retry("join in progress")
		}
		res.Reason = "not a member of this room"
		return res
	}
	if err != nil {
		return retry("storage error")
	}
	if !slices.ContainsFunc(room.Members, func(m store.Member) bool { return m.Node == e.FromNode }) {
		// We may simply not have heard that the sender joined. If the sender
		// claims a newer member list, ask the owner; otherwise it is final.
		if versions[e.Room] > room.Version && room.Owner != n.ID {
			if refresh != nil {
				refresh(e.Room)
			}
			return retry("sender not in our member list yet")
		}
		res.Reason = "sender is not a member of this room"
		return res
	}
	// The store re-checks membership, duplicates and receipt validity inside
	// the transaction that inserts, so none of them can be raced.
	switch _, err := n.st.Receive(ctx, e, n.ID, n.now()); {
	case errors.Is(err, store.ErrConflict):
		res.Reason = "id or sequence number already used by a different message"
		return res
	case errors.Is(err, store.ErrNotMember):
		res.Reason = "sender is not a member of this room"
		return res
	case errors.Is(err, store.ErrBadReceipt):
		res.Reason = "receipt does not match a message sent to that node"
		return res
	case err != nil:
		slog.Error("receive: store failed", "id", e.ID, "err", err)
		return retry("storage error")
	}
	slog.Info("stored", "id", e.ID, "room", e.Room, "from", core.ShortNode(e.FromNode), "kind", e.Kind)
	return Result{ID: e.ID, Status: ResStored}
}

// ---- outbound ----

// Mesh runs the outbox and keeps member lists fresh. Create with NewMesh.
type Mesh struct {
	n     *Node
	t     Transport
	addrs func() []string // this node's current listen addresses

	mu        sync.Mutex
	inFlight  map[string]bool      // destination nodes with a delivery in progress
	fails     map[string]int       // consecutive failed attempts per destination
	nextTry   map[string]time.Time // do not contact a destination before this
	lastOK    map[string]time.Time
	lastErr   map[string]string
	refreshed map[string]time.Time // last member-list refresh started, per room
}

func NewMesh(n *Node, t Transport, addrs func() []string) *Mesh {
	return &Mesh{n: n, t: t, addrs: addrs, inFlight: map[string]bool{}, fails: map[string]int{},
		nextTry: map[string]time.Time{}, lastOK: map[string]time.Time{}, lastErr: map[string]string{},
		refreshed: map[string]time.Time{}}
}

// Run drives deliveries until ctx ends. Safe to call once.
func (m *Mesh) Run(ctx context.Context) {
	tick := time.NewTicker(Tick)
	defer tick.Stop()
	var lastRefresh, lastPrune time.Time
	for {
		now := m.n.now()
		if _, err := m.n.st.Expire(ctx, now.Add(-RetentionExpiry), now); err != nil && ctx.Err() == nil {
			slog.Error("outbox: expire", "err", err)
		}
		nodes, err := m.n.st.QueuedNodes(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("outbox: scan", "err", err)
		}
		for _, node := range nodes {
			m.kick(ctx, node)
		}
		if now.Sub(lastRefresh) >= RefreshEvery {
			lastRefresh = now
			go m.RefreshAll(ctx)
		}
		if now.Sub(lastPrune) >= time.Hour {
			lastPrune = now
			if removed, err := m.n.st.Prune(ctx, now.Add(-RetentionKeep)); err == nil && removed > 0 {
				slog.Info("pruned old messages", "count", removed)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-m.n.wake:
		case <-tick.C:
		}
	}
}

// kick starts a delivery to node unless one is already running or the node is
// backing off. Exactly one request per destination is in flight, which is
// what preserves send order; and a fresh message never bypasses a back-off.
func (m *Mesh) kick(ctx context.Context, node string) {
	m.mu.Lock()
	if m.inFlight[node] || m.n.now().Before(m.nextTry[node]) {
		m.mu.Unlock()
		return
	}
	m.inFlight[node] = true
	m.mu.Unlock()
	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.inFlight, node)
			m.mu.Unlock()
		}()
		m.drain(ctx, node)
	}()
}

// failed records an unsuccessful attempt and schedules the next one with
// exponential back-off and jitter.
func (m *Mesh) failed(ctx context.Context, node, reason string) {
	now := m.n.now()
	m.mu.Lock()
	m.fails[node]++
	d := RetryMax
	if f := m.fails[node]; f < 20 {
		d = min(RetryMax, RetryBase<<(f-1))
	}
	d += time.Duration(mrand.Int64N(int64(d)/4 + 1)) // up to +25% jitter
	m.nextTry[node] = now.Add(d)
	m.lastErr[node] = reason
	m.mu.Unlock()
	if err := m.n.st.Defer(ctx, node, reason, now); err != nil && ctx.Err() == nil {
		slog.Error("outbox: bookkeeping", "err", err)
	}
}

// drain sends everything queued for node, batch by batch, until the queue is
// empty or something needs to wait.
func (m *Mesh) drain(ctx context.Context, node string) {
	st := m.n.st
	for ctx.Err() == nil {
		batch, err := st.NextBatch(ctx, node, core.MaxBatch)
		if err != nil || len(batch) == 0 {
			return
		}
		to, err := st.MemberByNode(ctx, node)
		if err != nil { // no longer shares any room with us; PutRoom rejects these rows
			return
		}
		req := DeliverReq{Versions: map[string]int64{}}
		for _, msg := range batch {
			req.Envelopes = append(req.Envelopes, msg.Envelope)
			if _, ok := req.Versions[msg.Room]; !ok {
				if r, err := st.Room(ctx, msg.Room); err == nil {
					req.Versions[msg.Room] = r.Version
				}
			}
		}
		start := time.Now()
		results, err := m.t.Deliver(ctx, to, req)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("deliver failed", "to", core.ShortNode(node), "queued", len(batch), "err", err)
			m.failed(ctx, node, err.Error())
			return
		}
		now := m.n.now()
		// Apply the answered prefix. Anything unanswered, answered "retry" or
		// answered for the wrong id stops the batch and backs off.
		done, stop := 0, ""
		for i, res := range results {
			if i >= len(batch) || res.ID != batch[i].ID {
				stop = "receiver's answer did not match the batch"
				break
			}
			state := store.Stored
			switch res.Status {
			case ResStored:
			case ResRejected:
				state = store.Rejected
				slog.Warn("rejected", "id", res.ID, "by", core.ShortNode(node), "reason", res.Reason)
			default:
				stop = res.Reason
				if stop == "" {
					stop = "receiver asked to retry"
				}
				go m.Refresh(ctx, batch[i].Room) // our member list may be the stale one
			}
			if stop != "" {
				break
			}
			if err := st.SetDelivery(ctx, batch[i].N, node, state, res.Reason, now); err != nil {
				slog.Error("outbox: bookkeeping", "err", err)
				m.failed(ctx, node, "local storage error")
				return
			}
			done++
		}
		slog.Info("delivered", "to", core.ShortNode(node), "count", done, "of", len(batch), "ms", time.Since(start).Milliseconds())
		if stop == "" && done < len(batch) {
			stop = "receiver answered for only part of the batch"
		}
		if stop != "" {
			m.failed(ctx, node, stop)
			return
		}
		m.mu.Lock()
		m.fails[node], m.lastOK[node], m.lastErr[node] = 0, now, ""
		delete(m.nextTry, node)
		m.mu.Unlock()
	}
}

// PeerStatus is what we know about reaching another node.
type PeerStatus struct {
	Node      string    `json:"node"`
	Name      string    `json:"name"`
	Addrs     []string  `json:"addrs"`
	LastOK    time.Time `json:"last_ok,omitempty"`
	LastError string    `json:"last_error,omitempty"`
}

// Peers lists every other node we share a room with.
func (m *Mesh) Peers(ctx context.Context) ([]PeerStatus, error) {
	rooms, err := m.n.st.Rooms(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{m.n.ID: true}
	out := []PeerStatus{}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range rooms {
		for _, mem := range r.Members {
			if seen[mem.Node] {
				continue
			}
			seen[mem.Node] = true
			out = append(out, PeerStatus{Node: mem.Node, Name: mem.Name, Addrs: mem.Addrs,
				LastOK: m.lastOK[mem.Node], LastError: m.lastErr[mem.Node]})
		}
	}
	return out, nil
}

// ---- membership: owner side ----

// CreateRoom makes a new room owned by this node.
func (n *Node) CreateRoom(ctx context.Context, label string, addrs []string) (store.Room, error) {
	if !core.ValidLabel(label) || label == core.LocalRoom {
		return store.Room{}, userErr("room labels are 1-64 letters, digits, '.', '_' or '-', and not %q", core.LocalRoom)
	}
	r := store.Room{ID: core.NewRoomID(), Label: label, Owner: n.ID, Version: 1,
		Members: []store.Member{{Node: n.ID, Name: n.Name, Addrs: addrs}}}
	_, err := n.st.PutRoom(ctx, r, true, n.now())
	return r, err
}

// invite is the decoded form of an invite token.
type invite struct {
	Room   string   `json:"r"`
	Label  string   `json:"l"`
	Owner  string   `json:"o"`
	Name   string   `json:"n"`
	Addrs  []string `json:"a"`
	Secret string   `json:"s"`
	Exp    int64    `json:"e"`
}

const invitePrefix = "a2p2-invite-"

func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// Invite creates a one-time token that lets one node join a room this node
// owns. Whoever holds the token can join until it is used or expires, so it
// must travel over a channel the owner trusts.
func (n *Node) Invite(ctx context.Context, roomName string, addrs []string) (token string, expires time.Time, err error) {
	room, err := n.ResolveRoom(ctx, roomName)
	if err != nil {
		return "", time.Time{}, err
	}
	if room.ID == core.LocalRoom {
		return "", time.Time{}, userErr("the local room cannot be shared; create a room first")
	}
	if room.Owner != n.ID {
		return "", time.Time{}, userErr("only the room's owner (%s) can invite", core.ShortNode(room.Owner))
	}
	if len(addrs) == 0 {
		return "", time.Time{}, userErr("this node has no reachable address; is the peer listener enabled?")
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", time.Time{}, err
	}
	secret := hex.EncodeToString(b[:])
	expires = n.now().Add(InviteTTL)
	if err := n.st.AddInvite(ctx, hashSecret(secret), room.ID, expires); err != nil {
		return "", time.Time{}, err
	}
	raw, _ := json.Marshal(invite{Room: room.ID, Label: room.Label, Owner: n.ID, Name: n.Name,
		Addrs: addrs, Secret: secret, Exp: expires.Unix()})
	return invitePrefix + base64.RawURLEncoding.EncodeToString(raw), expires, nil
}

func cleanAddrs(in []string) []string {
	out := []string{}
	for _, a := range in {
		if len(a) > 0 && len(a) <= 255 && len(out) < 8 && !strings.ContainsAny(a, " /\\?#@") {
			out = append(out, a)
		}
	}
	return out
}

// putOwned writes a new version of a room this node owns. Callers hold roomMu,
// so the read-modify-write cannot interleave with another change.
func (n *Node) putOwned(ctx context.Context, room store.Room) error {
	room.Version++
	applied, err := n.st.PutRoom(ctx, room, false, n.now())
	if err == nil && !applied {
		err = errors.New("member list changed concurrently")
	}
	return err
}

// HandleJoin admits fromNode (authenticated by the transport) if it presents a
// valid invite. A node that already redeemed the invite gets the room back
// only while it is still a member: a used invite never re-admits anyone.
func (n *Node) HandleJoin(ctx context.Context, fromNode string, req JoinReq) (store.Room, error) {
	n.roomMu.Lock()
	defer n.roomMu.Unlock()
	roomID, fresh, err := n.st.RedeemInvite(ctx, hashSecret(req.Secret), fromNode, n.now())
	if err != nil {
		return store.Room{}, ErrForbidden // same answer for unknown, used and expired
	}
	room, err := n.st.Room(ctx, roomID)
	if err != nil || room.Owner != n.ID {
		return store.Room{}, ErrForbidden
	}
	if slices.ContainsFunc(room.Members, func(m store.Member) bool { return m.Node == fromNode }) {
		return room, nil
	}
	if !fresh {
		return store.Room{}, ErrForbidden
	}
	if len(room.Members) >= core.MaxRoomNodes {
		return store.Room{}, userErr("room is full")
	}
	room.Members = append(room.Members, store.Member{Node: fromNode, Name: core.CleanLabel(req.Name), Addrs: cleanAddrs(req.Addrs)})
	if err := n.putOwned(ctx, room); err != nil {
		return store.Room{}, err
	}
	room.Version++
	slog.Info("node joined", "room", room.ID, "node", core.ShortNode(fromNode), "version", room.Version)
	n.broadcast()
	return room, nil
}

// HandleRoom answers a member's request for the current member list, and
// records the member's current addresses if they changed.
func (n *Node) HandleRoom(ctx context.Context, fromNode string, req RoomReq) (store.Room, error) {
	n.roomMu.Lock()
	defer n.roomMu.Unlock()
	room, err := n.st.Room(ctx, req.Room)
	if err != nil || room.Owner != n.ID {
		return store.Room{}, ErrForbidden
	}
	i := slices.IndexFunc(room.Members, func(m store.Member) bool { return m.Node == fromNode })
	if i < 0 {
		return store.Room{}, ErrForbidden
	}
	if addrs := cleanAddrs(req.Addrs); len(addrs) > 0 && !slices.Equal(addrs, room.Members[i].Addrs) {
		room.Members[i].Addrs = addrs
		if err := n.putOwned(ctx, room); err != nil {
			return store.Room{}, err
		}
		room.Version++
	}
	return room, nil
}

// RemoveMember drops a node from a room this node owns. It takes effect on
// each other member when that member next refreshes its list.
func (n *Node) RemoveMember(ctx context.Context, roomName, node string) (store.Room, error) {
	n.roomMu.Lock()
	defer n.roomMu.Unlock()
	room, err := n.ResolveRoom(ctx, roomName)
	if err != nil {
		return room, err
	}
	if room.Owner != n.ID {
		return room, userErr("only the room's owner can remove members")
	}
	var hits []int
	for i, m := range room.Members {
		if m.Node != n.ID && (m.Node == node || strings.HasPrefix(m.Node, node) || m.Name == node) {
			hits = append(hits, i)
		}
	}
	if len(hits) != 1 {
		return room, userErr("%q matches %d members; give a node id or unique prefix", node, len(hits))
	}
	room.Members = slices.Delete(room.Members, hits[0], hits[0]+1)
	if err := n.putOwned(ctx, room); err != nil {
		return room, err
	}
	room.Version++
	slog.Info("node removed", "room", room.ID, "node", node, "version", room.Version)
	return room, nil
}

// ---- membership: member side ----

// Join redeems an invite token against its room's owner.
func (m *Mesh) Join(ctx context.Context, token string) (store.Room, error) {
	token = strings.TrimSpace(token)
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, invitePrefix))
	var inv invite
	if !strings.HasPrefix(token, invitePrefix) || err != nil || json.Unmarshal(raw, &inv) != nil ||
		!core.ValidNode(inv.Owner) || !core.ValidRoom(inv.Room) || inv.Room == core.LocalRoom {
		return store.Room{}, userErr("that is not a valid a2p2 invite")
	}
	if m.n.now().Unix() > inv.Exp {
		return store.Room{}, userErr("that invite has expired; ask for a new one")
	}
	if inv.Owner == m.n.ID {
		return store.Room{}, userErr("that invite is for a room this node already owns")
	}
	owner := store.Member{Node: inv.Owner, Name: inv.Name, Addrs: cleanAddrs(inv.Addrs)}
	// From the moment the owner admits us it may send us room traffic, possibly
	// before its answer arrives. Mark the join in progress so that traffic is
	// held with "retry" instead of being rejected as an unknown room.
	m.n.mu.Lock()
	m.n.joining[inv.Room] = true
	m.n.mu.Unlock()
	defer func() {
		m.n.mu.Lock()
		delete(m.n.joining, inv.Room)
		m.n.mu.Unlock()
	}()
	room, err := m.t.Join(ctx, owner, JoinReq{Secret: inv.Secret, Name: m.n.Name, Addrs: m.addrs()})
	if errors.Is(err, ErrForbidden) {
		return store.Room{}, userErr("the owner refused the invite (already used, expired, or revoked)")
	}
	if err != nil {
		return store.Room{}, userErr("could not reach the room owner at %s: %v", strings.Join(owner.Addrs, ", "), err)
	}
	if room.ID != inv.Room || room.Owner != inv.Owner {
		return store.Room{}, userErr("the owner answered for a different room; refusing")
	}
	if _, err := m.n.st.PutRoom(ctx, room, true, m.n.now()); err != nil {
		return store.Room{}, err
	}
	return room, nil
}

// Refresh re-reads one room's member list from its owner. If the owner says
// we are no longer a member, the room is dropped locally. Refreshes of one
// room are rate-limited, so peers cannot make us hammer an owner.
func (m *Mesh) Refresh(ctx context.Context, roomID string) {
	room, err := m.n.st.Room(ctx, roomID)
	if err != nil || room.Owner == m.n.ID || room.ID == core.LocalRoom {
		return
	}
	i := slices.IndexFunc(room.Members, func(x store.Member) bool { return x.Node == room.Owner })
	if i < 0 {
		return
	}
	m.mu.Lock()
	if last := m.refreshed[roomID]; m.n.now().Sub(last) < RefreshMinGap && !last.IsZero() {
		m.mu.Unlock()
		return
	}
	m.refreshed[roomID] = m.n.now()
	m.mu.Unlock()
	fresh, err := m.t.Room(ctx, room.Members[i], RoomReq{Room: roomID, Addrs: m.addrs()})
	switch {
	case errors.Is(err, ErrForbidden):
		// Only if nothing changed while we were asking: a refusal that arrives
		// after we rejoined must not delete the new membership.
		slog.Warn("removed from room", "room", roomID)
		_ = m.n.st.DeleteRoom(ctx, roomID, room.Version, m.n.now())
	case err != nil:
		return // owner unreachable: keep using the list we have
	case fresh.ID != roomID || fresh.Owner != room.Owner:
		slog.Warn("owner returned a different room; ignoring", "room", roomID)
	default:
		// create=false: a late answer must not resurrect a room we dropped.
		if applied, _ := m.n.st.PutRoom(ctx, fresh, false, m.n.now()); applied {
			slog.Info("member list updated", "room", roomID, "version", fresh.Version)
			m.n.broadcast()
		}
	}
}

// RefreshAll refreshes every room this node does not own.
func (m *Mesh) RefreshAll(ctx context.Context) {
	rooms, err := m.n.st.Rooms(ctx)
	if err != nil {
		return
	}
	for _, r := range rooms {
		m.Refresh(ctx, r.ID)
	}
}

// receiptID derives a stable id for the receipt an agent sends for a message,
// so acknowledging twice produces one receipt.
func receiptID(messageID, agent string) string {
	h := sha256.Sum256([]byte("receipt\x00" + messageID + "\x00" + agent))
	h[6] = h[6]&0x0f | 0x80 // version 8: custom, name-derived
	h[8] = h[8]&0x3f | 0x80
	s := hex.EncodeToString(h[:16])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

// Addrs returns the addresses this node currently tells peers to dial; empty
// when networking is off.
func (m *Mesh) Addrs() []string {
	if m.addrs == nil {
		return nil
	}
	return m.addrs()
}
