// Package node is the daemon's behaviour: signed records and how they are
// read (Machine), the room log, requests and invites (Rooms), joining
// (Joiner, JoinServer), the pull exchange and its scheduler (Exchange,
// Scheduler) and the surfaces' thin layer (Surface). It knows nothing about
// HTTP, MCP or TLS; the local API, the MCP shim and the peer transport are
// thin layers over it.
package node

// Machine is the behaviour for the rooms and agents on this machine:
// writing signed records and reading them (DESIGN-next sections 2 and 6,
// rules [R2], [R4], [R6], [R7] of internal/model/delivery).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/members"
	"github.com/wallter/a2p2/internal/store"
)

// UserError is a mistake by the caller (unknown room, bad argument). Layers
// above map it to a 4xx / tool error instead of an internal failure.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

// ackRefusal is a user error that says why something is not allowed: it is a
// *UserError to errors.As and ErrNotAllowed to errors.Is.
type ackRefusal struct{ UserError }

func (r *ackRefusal) Is(target error) bool { return target == ErrNotAllowed }

func (r *ackRefusal) As(target any) bool {
	if p, ok := target.(**UserError); ok {
		*p = &r.UserError
		return true
	}
	return false
}

func refuse(msg string) error { return &ackRefusal{UserError{msg}} }

func userErr(format string, a ...any) error { return &UserError{fmt.Sprintf(format, a...)} }

// Errors of the record operations. All are user errors.
var (
	ErrNotHeld      = &UserError{"record not held here"}
	ErrNotAllowed   = &UserError{"not allowed"}
	ErrUnknownAgent = &UserError{"agent not registered"}
)

// Why Handled refused: each is ErrNotAllowed, and says what the agent can
// do about it (D5's rule does not change; only the reason is told).
var (
	ErrAckNotMessage   = refuse("that id is a report, withdrawal or replacement, not a message")
	ErrAckOwn          = refuse("you wrote this message; only its readers report handling it")
	ErrAckNotAddressed = refuse("this message is not addressed to you and did not ask for handled reports " +
		"(ask_handled); reply to it instead")
	ErrAckNotShown = refuse("you have not been shown this message's text yet: read it with inbox first " +
		"(a thread or recent view does not count); inbox never shows a message sent before you registered, so " +
		"reply to that instead")
)

// MachineOpts injects the clock and randomness, so tests are deterministic.
type MachineOpts struct {
	Now  func() time.Time
	Rand func([]byte) // fills b with random bytes; default crypto/rand
}

// Machine writes and reads records for this machine's agents.
type Machine struct {
	ID string // node id: the hex Ed25519 public key

	st   *store.Store
	key  ed25519.PrivateKey
	now  func() time.Time
	rand func([]byte)

	// mu guards out. Every decision a write makes happens inside the
	// store's write transaction, so mu only keeps out consistent.
	mu sync.Mutex
	// out is, per room, the epoch of the stream this run of the daemon
	// writes [R2]. It is not durable: a new Machine (a daemon start) begins
	// a new stream.
	out map[string]string

	// pruneDue is set when a read found text past retention that is not yet
	// marked expired; the daemon runs Prune when it sees it (PruneDue).
	pruneDue atomic.Bool

	// fronts, set by OpenRooms, is this machine's reading frontier in each
	// room with a log (see readFront).
	fronts func() map[string]readFront
	// locked, set by OpenRooms, runs fn with no membership change and no
	// frontier published meanwhile, so that Prune reads the frontier and
	// writes expiry as one decision.
	locked func(fn func() error) error

	// wakes is touched for a room after each write that can change its
	// summary or log (wake.go).
	wakes roomWakes
}

// readFront is this machine's reading frontier in a room: per author, the
// from heads of its grants (readFrom), and whether it may read all text.
// While a head of an author is not held, the cursors of its streams are
// not seeded yet: reading holds back those streams without a cursor
// (roomView.unseeded); without all text readable their text is also
// undecided (access.go) and missing text of the author does not expire
// (undecided).
type readFront struct {
	From map[string][]string
	All  bool
	perm members.Permission // the permission it comes from
}

// undecided lists the (room, author) pairs with a head of this machine's
// reading frontier not held: whether it may read their text is undecided.
func (m *Machine) undecided(rd *store.Reader) (map[[2]string]bool, error) {
	if m.fronts == nil {
		return nil, nil
	}
	out := map[[2]string]bool{}
	for room, f := range m.fronts() {
		if f.All {
			continue
		}
		for author, heads := range f.From {
			u, err := headsMissing(rd, room, author, heads)
			if err != nil {
				return nil, err
			}
			if u {
				out[[2]string{room, author}] = true
			}
		}
	}
	return out, nil
}

// headsMissing reports whether some head of author is not held in room.
func headsMissing(rd *store.Reader, room, author string, heads []string) (bool, error) {
	for _, head := range heads {
		h, ok, err := rd.Record(room, head)
		if err != nil {
			return false, err
		}
		if !ok || h.Record.Author() != author {
			return true, nil
		}
	}
	return false, nil
}

// NewMachine returns the record behaviour over st for the node whose key is
// key. The daemon calls Prune once at start.
func NewMachine(st *store.Store, key ed25519.PrivateKey, o MachineOpts) *Machine {
	m := &Machine{ID: hex.EncodeToString(key.Public().(ed25519.PublicKey)), st: st, key: key,
		now: o.Now, rand: o.Rand, out: map[string]string{}}
	if m.now == nil {
		m.now = time.Now
	}
	if m.rand == nil {
		m.rand = func(b []byte) { _, _ = rand.Read(b) }
	}
	return m
}

// Register makes agent known here. The first time, in one transaction, its
// cursors start at the current end of every stream in every room, so what
// was written before it arrived is not unread for it. It reports whether
// the agent is new.
func (m *Machine) Register(ctx context.Context, agent string) (bool, error) {
	if err := checkAgent(agent); err != nil {
		return false, err
	}
	return m.st.Register(ctx, agent, m.now())
}

// Prune is the pruning pass [R6]: text 30 days old by this machine's clock
// is marked expired, durably. Reads already treat such text as expired, and
// ask for this pass (PruneDue); the daemon runs it at start, hourly, and
// whenever PruneDue says so, so that text once shown as expired stays
// expired when the clock is set back.
func (m *Machine) Prune(ctx context.Context) error {
	m.pruneDue.Store(false)
	run := func() error {
		n, err := m.st.ExpireTextWith(ctx, m.now(), m.undecided)
		if err == nil && n > 0 {
			m.wakes.touchAll()
		}
		return err
	}
	var err error
	if m.locked != nil {
		err = m.locked(run)
	} else {
		err = run()
	}
	if err != nil {
		m.pruneDue.Store(true)
		return err
	}
	return nil
}

// PruneDue reports whether a read found text past retention that only
// Prune can mark durably.
func (m *Machine) PruneDue() bool { return m.pruneDue.Load() }

// pruneIfDue runs Prune before a write or a confirmation when a read asked
// for it, so what was shown as expired is durably expired before anything
// acts on it.
func (m *Machine) pruneIfDue(ctx context.Context) error {
	if m.pruneDue.Load() {
		return m.Prune(ctx)
	}
	return nil
}

// Draft is what an agent sends.
type Draft struct {
	Room  string
	Agent string
	Text  string
	// Choices are offered answers (a question for a person).
	Choices []string
	// To names the addressees. Empty means everyone, except for a reply,
	// which goes to its parent's author unless Everyone is set (D10).
	To       []core.Addressee
	Everyone bool
	// ReplyTo is the message answered: it sets parent and thread.
	ReplyTo string
	// AskHandled asks readers of a room-wide post to report handling.
	AskHandled bool
}

// Sent is what a write returns.
type Sent struct {
	ID      string
	Room    string
	Created time.Time
	// To is a message's addressees as written; empty is everyone.
	To []core.Addressee
	// Already: a handled report that was held before this call.
	Already bool
	// Label is the room's label (filled in by Surface).
	Label string
	// ToOmitted is how many thread participants a reply to the thread
	// left out at the addressee limit (Surface, SendInput.Thread).
	ToOmitted int
}

// Send writes a message. Once it returns, the message is committed (D1).
func (m *Machine) Send(ctx context.Context, d Draft) (Sent, error) {
	if d.Everyone && len(d.To) > 0 {
		return Sent{}, userErr("a post to everyone names no addressees")
	}
	var to []core.Addressee
	s, err := m.write(ctx, d.Room, d.Agent, func(v *roomView, f *core.RecordFields) error {
		f.Kind, f.Text, f.Choices, f.AskHandled = core.KindMessage, d.Text, d.Choices, d.AskHandled
		f.To = slices.Clone(d.To)
		if d.ReplyTo != "" {
			p, err := v.get(d.ReplyTo)
			if err != nil {
				return err
			}
			if p == nil || p.f.Kind != core.KindMessage {
				return ErrNotHeld
			}
			f.Parent, f.Thread = p.id, p.f.Thread
			if f.Thread == "" {
				f.Thread = p.id
			}
			if len(d.To) == 0 && !d.Everyone {
				f.To = []core.Addressee{{Node: p.author, Agent: p.f.Agent}}
			}
		}
		slices.SortFunc(f.To, func(a, b core.Addressee) int {
			if c := strings.Compare(a.Node, b.Node); c != 0 {
				return c
			}
			return strings.Compare(a.Agent, b.Agent)
		})
		f.To = slices.Compact(f.To)
		if f.AskHandled && len(f.To) > 0 {
			return userErr("ask_handled is only for a post to everyone")
		}
		to = slices.Clone(f.To)
		return nil
	})
	s.To = to
	return s, err
}

// Replace gives new text for one of this machine's own messages (any agent
// here, T2). Its version is one above the highest version of that message
// held here, with text or not, so a version is never reused.
func (m *Machine) Replace(ctx context.Context, room, agent, ref, text string) (Sent, error) {
	return m.amend(ctx, room, agent, ref, core.KindReplace, text)
}

// Withdraw takes back one of this machine's own messages; its text and the
// text of its replacements are dropped here at once [R7].
func (m *Machine) Withdraw(ctx context.Context, room, agent, ref string) (Sent, error) {
	return m.amend(ctx, room, agent, ref, core.KindWithdraw, "")
}

func (m *Machine) amend(ctx context.Context, room, agent, ref string, k core.Kind, text string) (Sent, error) {
	return m.write(ctx, room, agent, func(v *roomView, f *core.RecordFields) error {
		f.Kind, f.Ref, f.Text = k, ref, text
		t, err := v.get(ref)
		if err != nil {
			return err
		}
		if t == nil {
			return ErrNotHeld
		}
		if t.author != m.ID || t.f.Kind != core.KindMessage {
			return ErrNotAllowed
		}
		if k == core.KindReplace {
			rs, err := v.refs(ref)
			if err != nil {
				return err
			}
			for _, r := range rs {
				if r.f.Kind == core.KindReplace && r.author == m.ID && r.f.Version > f.Version {
					f.Version = r.f.Version
				}
			}
			f.Version++
		}
		return nil
	})
}

// existing aborts a write whose report is already held.
type existing struct{ r *rec }

func (existing) Error() string { return "already reported" }

// Handled reports that agent dealt with message ref. Only an addressee, or
// anyone but the author on a room-wide post that asked, may report, and
// only after it confirmed the message's text [R7]. Reporting twice returns
// the first report.
func (m *Machine) Handled(ctx context.Context, room, agent, ref string) (Sent, error) {
	s, err := m.write(ctx, room, agent, func(v *roomView, f *core.RecordFields) error {
		f.Kind, f.Ref = core.KindHandled, ref
		t, err := v.get(ref)
		if err != nil {
			return err
		}
		if t == nil {
			return ErrNotHeld
		}
		if !store.CanHandle(t.h.Record, m.ID, agent) {
			switch {
			case t.f.Kind != core.KindMessage:
				return ErrAckNotMessage
			case t.author == m.ID && t.f.Agent == agent:
				return ErrAckOwn
			}
			return ErrAckNotAddressed
		}
		hs, err := v.refs(ref)
		if err != nil {
			return err
		}
		for _, h := range hs {
			if h.f.Kind == core.KindHandled && h.author == m.ID && h.f.Agent == agent && h.h.Effect == store.EffectOK {
				return existing{h}
			}
		}
		if _, shown, err := v.rd.Read(agent, room, ref); err != nil || !shown {
			if err == nil {
				err = ErrAckNotShown
			}
			return err
		}
		return nil
	})
	var ex existing
	if errors.As(err, &ex) {
		return Sent{ID: ex.r.id, Room: room, Created: ex.r.f.Created, Already: true}, nil
	}
	return s, err
}

// write builds and stores one record of this machine inside one store
// transaction: decide fills in the record's content from what is held;
// write adds the stream, position, links and time, signs, and stores. The
// first write in a room after a start begins a new stream whose first
// record links every head of this machine's own history held there [R2].
func (m *Machine) write(ctx context.Context, room, agent string, decide func(*roomView, *core.RecordFields) error) (Sent, error) {
	if err := checkAgent(agent); err != nil {
		return Sent{}, err
	}
	if err := m.pruneIfDue(ctx); err != nil {
		return Sent{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var epoch string
	r, err := m.st.Write(ctx, now, func(rd *store.Reader) (*core.VerifiedRecord, error) {
		v := m.newView(rd, room, now)
		if err := v.load(); err != nil {
			return nil, err
		}
		if err := v.requireAgent(agent); err != nil {
			return nil, err
		}
		f := core.RecordFields{Room: room, Agent: agent}
		if err := decide(v, &f); err != nil {
			return nil, err
		}
		epoch = m.out[room]
		var contig int64
		for _, s := range v.streams {
			if s.Author == m.ID && s.Epoch == epoch {
				contig = s.Contig
			}
		}
		if epoch == "" || contig == 0 {
			links, err := v.ownHeads(m.ID)
			if err != nil {
				return nil, err
			}
			epoch, f.Seq, f.Links = m.newEpoch(), 1, links
		} else {
			last, err := v.at(streamKey{m.ID, epoch}, contig)
			if err != nil || last == nil {
				return nil, errors.Join(err, errors.New("own stream lost its last record"))
			}
			f.Seq, f.Prev = contig+1, last.id
		}
		f.Epoch, f.Created = epoch, now
		rec, err := core.NewRecord(m.key, f)
		if err != nil {
			return nil, userErr("cannot write: %v", err)
		}
		return rec.Verified()
	})
	if err != nil {
		return Sent{}, err
	}
	m.out[room] = epoch
	m.wakes.touch(room)
	return Sent{ID: r.ID(), Room: room, Created: r.Fields().Created}, nil
}

// newEpoch is 16 random hex digits, not all zeros (PROTOCOL-next section 0).
func (m *Machine) newEpoch() string {
	var b [8]byte
	for b == [8]byte{} {
		m.rand(b[:])
	}
	return hex.EncodeToString(b[:])
}

func checkAgent(agent string) error {
	if !core.ValidLabel(agent) {
		return userErr("bad agent label")
	}
	return nil
}
