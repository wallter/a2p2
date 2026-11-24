package members

import (
	"errors"

	"github.com/wallter/a2p2/internal/core"
)

// The adapter: entries from internal/core, already parsed and with their
// signatures checked, enter the same Machine and resolver as the model's
// entries. Their id is the one core computed from the signed bytes (no
// hashing here), injected as the id function, and the fields are copied
// once from the immutable core entry, which the entry keeps.

// signedID is the id function for entries made from core entries.
func signedID(e entry) string {
	if e.src == nil {
		return ""
	}
	return e.src.ID()
}

// daemon are the final rules with the signed-bytes id.
var daemon = func() ruleSet {
	r := final
	r.id = signedID
	return r
}()

// fromCore gives the resolver what it needs from a core entry. Fields
// are copied once from the immutable core entry, which the entry keeps.
func fromCore(c *core.Entry) entry {
	f := c.Fields()
	return entry{
		ID: c.ID(), Author: c.Author(), Seq: f.Seq, Prev: f.Prev, Parents: f.Parents,
		Kind: entryKind(f.Kind), Target: f.Target, History: f.History, From: f.From,
		src: c, room: f.Room, size: len(c.Header()) + len(c.Sig()),
	}
}

// verified converts core entries, checking each signature (G1) here so
// that no caller can skip it: core's parsed entry type can exist before
// its signature is checked. Those that fail are returned by id.
func verified(cs []*core.Entry) ([]entry, []string) {
	in := make([]entry, 0, len(cs))
	var bad []string
	for _, c := range cs {
		switch {
		case c == nil:
		case c.VerifySignature() != nil:
			bad = append(bad, c.ID())
		default:
			in = append(in, fromCore(c))
		}
	}
	return in, bad
}

// NewMachine returns a machine for the room whose create entry is root.
func NewMachine(root *core.Entry) (*Machine, error) {
	if root == nil || root.Fields().Kind != core.EntryCreate {
		return nil, errors.New("members: a room's root must be its create entry")
	}
	if root.VerifySignature() != nil {
		return nil, errors.New("members: the room's create entry does not verify")
	}
	return newMachine(daemon, fromCore(root)), nil
}

// Receive offers core entries to the machine (PROTOCOL section 3, C3) and
// returns, in the order they were decided, what happened to every entry
// it touched, including entries released from waiting. The store can
// persist each batch and replay the batches in order to the same result.
// Signatures (G1), the room (R1), the pinned create (C1) and the chain
// (C2) are checked here too.
func (m *Machine) Receive(es ...*core.Entry) []Result {
	in, bad := verified(es)
	var pre []Result
	for _, id := range bad {
		if _, ok := m.set[id]; !ok && m.invalid[id] == "" {
			m.invalid[id] = ReasonBadSignature
			pre = append(pre, Result{id, Invalid, ReasonBadSignature})
		}
	}
	return append(pre, m.receive(in...)...)
}

// Offer returns what the machine passes on: accepted entries in causal
// order, then held and refused ones (fork evidence) in arrival order.
func (m *Machine) Offer() []*core.Entry {
	var out []*core.Entry
	for _, e := range m.offer() {
		if e.src != nil {
			out = append(out, e.src)
		}
	}
	return out
}

// ResolveRoom computes the state of the room whose create entry has id
// root from core entries. Every other create, every entry signed for
// another room or whose signature does not verify, and every entry that
// depends on one, has no effect, with a reason. An empty root is an error.
func ResolveRoom(root string, es []*core.Entry) (State, error) {
	if root == "" {
		return State{}, errors.New("members: ResolveRoom needs the room's create id")
	}
	in, bad := verified(es)
	st := daemon.resolveRoom(root, in)
	for _, id := range bad {
		if _, ok := st.Effects[id]; !ok {
			st.Effects[id] = Effect{Reason: ReasonBadSignature}
		}
	}
	return st, nil
}
