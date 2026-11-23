// Package core defines the message envelope and the limits every other
// package enforces. It does no I/O.
package core

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Protocol limits. Changing any of these is a wire-compatibility decision.
const (
	ProtocolVersion = 1
	MaxBodyBytes    = 16 << 10 // encoded body JSON
	MaxEnvelope     = 24 << 10 // whole encoded envelope
	MaxRecipients   = 32
	MaxBatch        = 100
	MaxRoomNodes    = 64
	MaxLabel        = 64

	LocalRoom = "local" // built-in room that never leaves the machine
	HumanName = "human" // label of the human participant on every node
)

// Kind says how a message should be treated by the receiving node.
type Kind string

const (
	KindMessage  Kind = "message"  // ordinary text; replies use ReplyTo
	KindQuestion Kind = "question" // a question for a human (ask_human)
	KindAnswer   Kind = "answer"   // a human's answer to a question
	KindReceipt  Kind = "receipt"  // seen/handled notice; never shown in inboxes
)

// Receipt states carried in Body.State of a KindReceipt envelope.
const (
	ReceiptSeen    = "seen"
	ReceiptHandled = "handled"
)

// Body is the kind-specific payload.
type Body struct {
	Text    string   `json:"text,omitempty"`
	Choices []string `json:"choices,omitempty"` // question: offered answers
	Ref     string   `json:"ref,omitempty"`     // receipt: message id it refers to
	State   string   `json:"state,omitempty"`   // receipt: seen | handled
}

// Envelope is the unit of storage and transfer. FromNode is always overwritten
// by the receiver with the authenticated identity of the connection; From is a
// label chosen by the sending node and is not a credential.
type Envelope struct {
	V         int        `json:"v"`
	ID        string     `json:"id"`
	Room      string     `json:"room"`
	FromNode  string     `json:"from_node"`
	From      string     `json:"from"`
	To        []string   `json:"to,omitempty"`
	Kind      Kind       `json:"kind"`
	ReplyTo   string     `json:"reply_to,omitempty"`
	Thread    string     `json:"thread,omitempty"`
	Seq       int64      `json:"seq"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Body      Body       `json:"body"`
}

var (
	idRe    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	nodeRe  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	roomRe  = regexp.MustCompile(`^(local|r_[0-9a-f]{32})$`)
	labelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	agentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}@[0-9a-f]{8}$`)
)

// ErrInvalid wraps every validation failure so callers can map it to a
// "rejected" delivery instead of retrying.
var ErrInvalid = errors.New("invalid envelope")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Validate checks structure and limits. It does not check authorisation.
func (e *Envelope) Validate() error {
	switch {
	case e.V != ProtocolVersion:
		return invalid("unsupported version %d", e.V)
	case !idRe.MatchString(e.ID):
		return invalid("bad id")
	case !roomRe.MatchString(e.Room):
		return invalid("bad room")
	case !nodeRe.MatchString(e.FromNode):
		return invalid("bad from_node")
	case !agentRe.MatchString(e.From):
		return invalid("bad from")
	case !strings.HasSuffix(e.From, "@"+ShortNode(e.FromNode)):
		return invalid("from does not belong to from_node")
	case len(e.To) > MaxRecipients:
		return invalid("too many recipients")
	case e.Seq < 1:
		return invalid("bad seq")
	case e.CreatedAt.IsZero():
		return invalid("missing created_at")
	case e.ReplyTo != "" && !idRe.MatchString(e.ReplyTo):
		return invalid("bad reply_to")
	case e.Thread != "" && !idRe.MatchString(e.Thread):
		return invalid("bad thread")
	}
	for _, t := range e.To {
		if !agentRe.MatchString(t) {
			return invalid("bad recipient %q", t)
		}
	}
	b, err := json.Marshal(e.Body)
	if err != nil || len(b) > MaxBodyBytes {
		return invalid("body too large")
	}
	switch e.Kind {
	case KindMessage, KindAnswer:
		if strings.TrimSpace(e.Body.Text) == "" {
			return invalid("empty text")
		}
	case KindQuestion:
		if strings.TrimSpace(e.Body.Text) == "" {
			return invalid("empty question")
		}
		if len(e.Body.Choices) > 8 {
			return invalid("too many choices")
		}
	case KindReceipt:
		if !idRe.MatchString(e.Body.Ref) || (e.Body.State != ReceiptSeen && e.Body.State != ReceiptHandled) {
			return invalid("bad receipt")
		}
	default:
		return invalid("unknown kind %q", e.Kind)
	}
	if e.Kind == KindAnswer && e.ReplyTo == "" {
		return invalid("answer without reply_to")
	}
	return nil
}

// DecodeEnvelope parses and validates untrusted bytes.
func DecodeEnvelope(b []byte) (Envelope, error) {
	var e Envelope
	if len(b) > MaxEnvelope {
		return e, invalid("envelope too large")
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return e, invalid("json: %v", err)
	}
	return e, e.Validate()
}

// NewID returns a UUIDv7: 48 bits of Unix milliseconds then random bits. Ids
// sort roughly by creation time but uniqueness never depends on the clock.
func NewID(now time.Time) string {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		panic(err) // crypto/rand failing is not recoverable
	}
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(now.UnixMilli()))
	copy(u[:6], ts[2:])
	u[6] = u[6]&0x0f | 0x70
	u[8] = u[8]&0x3f | 0x80
	h := hex.EncodeToString(u[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// NewRoomID returns an opaque random room id.
func NewRoomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "r_" + hex.EncodeToString(b[:])
}

// ShortNode is the 8-character fingerprint used in agent ids and displays.
func ShortNode(node string) string {
	if len(node) < 8 {
		return node
	}
	return node[:8]
}

// AgentID joins a label and the node it lives on.
func AgentID(label, node string) string { return label + "@" + ShortNode(node) }

// ValidNode, ValidRoom and ValidLabel are used at API boundaries.
func ValidNode(s string) bool  { return nodeRe.MatchString(s) }
func ValidRoom(s string) bool  { return roomRe.MatchString(s) }
func ValidLabel(s string) bool { return labelRe.MatchString(s) }
func ValidID(s string) bool    { return idRe.MatchString(s) }

// CleanLabel turns arbitrary text (a directory name, a hostname) into a label.
func CleanLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-._")
	if len(out) > MaxLabel-8 { // leave room for a "-N" uniqueness suffix
		out = out[:MaxLabel-8]
	}
	if out == "" {
		out = "agent"
	}
	return out
}
