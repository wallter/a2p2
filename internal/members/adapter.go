package members

import (
	"errors"
	"fmt"

	"github.com/wallter/a2p2/internal/core"
)

// The adapter: entries from internal/core, which can enter only as
// core.VerifiedEntry (parsed, signature checked), enter the same Machine and resolver as the model's
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

// fromCore gives the resolver what it needs from a verified core entry.
// Fields are copied once from the immutable core entry, which the entry
// keeps.
func fromCore(c *core.VerifiedEntry) entry {
	f := c.Fields()
	return entry{
		ID: c.ID(), Author: c.Author(), Seq: f.Seq, Prev: f.Prev, Parents: f.Parents,
		Kind: entryKind(f.Kind), Target: f.Target, History: f.History, From: f.From,
		src: c, room: f.Room, size: len(c.Header()) + len(c.Sig()),
	}
}

// real reports whether c holds an entry: a nil or zero VerifiedEntry, the
// only kind that exists without core having checked a signature, has an
// empty id and is refused, never treated as an entry.
func isEntry(c *core.VerifiedEntry) bool { return c != nil && c.ID() != "" }

// NewMachine returns a machine for the room whose create entry is root.
// Entries enter only as core.VerifiedEntry: callers verify once, at
// ingress, with (*core.Entry).Verified (G1).
func NewMachine(root *core.VerifiedEntry) (*Machine, error) {
	if !isEntry(root) {
		return nil, errors.New("members: " + ReasonNotVerified)
	}
	if root.Fields().Kind != core.EntryCreate {
		return nil, errors.New("members: a room's root must be its create entry")
	}
	return newMachine(daemon, fromCore(root)), nil
}

// Receive offers verified entries to the machine (PROTOCOL section 3, C3)
// and returns, in the order they were decided, what happened to every
// entry it touched, including entries released from waiting. The store
// can persist each batch and replay the batches in order to the same
// result. The room (R1), the pinned create (C1) and the chain (C2) are
// checked here. A nil or zero entry is refused with an empty id.
func (m *Machine) Receive(es ...*core.VerifiedEntry) []Result {
	in := make([]entry, 0, len(es))
	var pre []Result
	for _, c := range es {
		if !isEntry(c) {
			pre = append(pre, Result{"", Invalid, ReasonNotVerified})
			continue
		}
		in = append(in, fromCore(c))
	}
	return append(pre, m.receive(in...)...)
}

// Offer returns what the machine passes on: accepted entries in causal
// order, then held and refused ones (fork evidence) in arrival order.
func (m *Machine) Offer() []*core.VerifiedEntry {
	var out []*core.VerifiedEntry
	for _, e := range m.offer() {
		if e.src != nil {
			out = append(out, e.src)
		}
	}
	return out
}

// ResolveRoom computes the state of the room whose create entry has id
// root from verified entries. Every other create, every entry signed for
// another room, and every entry that depends on one, has no effect, with a
// reason. An empty root, or a nil or zero entry, is an error.
func ResolveRoom(root string, es []*core.VerifiedEntry) (State, error) {
	if root == "" {
		return State{}, errors.New("members: ResolveRoom needs the room's create id")
	}
	in := make([]entry, 0, len(es))
	for i, c := range es {
		if !isEntry(c) {
			return State{}, fmt.Errorf("members: entry %d: %s", i, ReasonNotVerified)
		}
		in = append(in, fromCore(c))
	}
	return daemon.resolveRoom(root, in), nil
}
