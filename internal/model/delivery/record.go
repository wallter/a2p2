// Package delivery is an executable model of store-and-forward delivery in a
// small room where every member machine keeps a copy of the whole room.
//
// It is not the product. It is plain, deterministic Go with no I/O, no real
// clock and no goroutines, written so that the delivery rules can be settled
// by tests before the real delivery core is redesigned. FINDINGS.md states the
// rules in plain words and says which tests show them.
package delivery

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Time is the model's logical clock, in hours. Tests advance it by hand.
type Time int64

const (
	Hour Time = 1
	Day  Time = 24 * Hour
	// Retention is how long a record is kept: 30 days after its creation,
	// measured by the clock of the machine holding it.
	Retention = 30 * Day
)

// MachineID names a machine by its key. A machine that loses its disk loses
// its key and comes back under a new MachineID.
type MachineID string

// Addr is one agent on one machine. Agent names are labels the machine
// assigns; only the machine part is authenticated.
type Addr struct {
	Machine MachineID
	Agent   string
}

func (a Addr) String() string { return string(a.Machine) + "/" + a.Agent }

// Kind is what a record is. Four kinds were enough for every scenario.
type Kind uint8

const (
	Message  Kind = iota + 1 // text for agents
	Handled                  // an addressee says it dealt with Ref
	Withdraw                 // the author takes back its own message Ref
	Replace                  // the author gives new text for its own message Ref
)

func (k Kind) String() string {
	switch k {
	case Message:
		return "message"
	case Handled:
		return "handled"
	case Withdraw:
		return "withdraw"
	case Replace:
		return "replace"
	}
	return "?"
}

// StreamID is one run of records from one author machine. A machine starts a
// new stream (a new Epoch, chosen at random in a real system and from a
// counter here) at its first write after every start. That is the rule that
// makes a restored backup safe to write from: positions are never reused,
// because a restored machine never continues an old stream.
type StreamID struct {
	Author MachineID
	Epoch  uint64
}

func (s StreamID) String() string { return fmt.Sprintf("%s.%d", s.Author, s.Epoch) }

func (s StreamID) less(o StreamID) bool {
	if s.Author != o.Author {
		return s.Author < o.Author
	}
	return s.Epoch < o.Epoch
}

// Pos is a record's place: its stream and its sequence number in it (from 1,
// no gaps).
type Pos struct {
	Stream StreamID
	Seq    int
}

// RecordID is the hash of a record's header, which includes the hash of its
// body. Because the body is in the id only as a hash, a machine can drop a
// body (withdrawn or expired) and still pass on a header that verifies.
type RecordID string

// Record is the signed header. Machines store and pass it on unchanged. The
// body travels beside it and is checked against BodyHash.
type Record struct {
	Author MachineID
	Epoch  uint64
	Seq    int
	// Prev is the previous record in the same stream (Seq > 1 only).
	Prev RecordID
	// Links, for Seq 1 only, are every head of the author's history the
	// machine held when the stream began: its own records that no other own
	// record it held follows. Empty for the author's first record ever. They
	// are what orders a machine's streams.
	Links []Link
	// Version, for a replace only, is one more than the version of the text
	// it replaces as its author knew it (the message itself is version 0).
	// The highest version wins, ties broken by id: a pure function of the
	// records held, whatever order they arrived in.
	Version int

	Kind       Kind
	Agent      string // the author's agent label
	To         []Addr // empty means everyone in the room
	AskHandled bool   // a room-wide post that asks for handled reports
	Ref        RecordID
	Parent     RecordID
	Thread     RecordID
	Created    Time // by the author's clock
	// Manifest, for a message or a replacement with files only, is the
	// SHA-256 of its salted manifest (attach.go; 0.2 draft): all the header
	// says about its files.
	Manifest string

	BodyHash string
	ID       RecordID
	Sig      string
}

// Stream returns the record's stream.
func (r *Record) Stream() StreamID { return StreamID{r.Author, r.Epoch} }

// Pos returns the record's position.
func (r *Record) Pos() Pos { return Pos{r.Stream(), r.Seq} }

// Link names one earlier record of the same author by id and position.
type Link struct {
	ID RecordID
	At Pos
}

// preds returns the positions this record directly follows.
func (r *Record) preds() []Link {
	if r.Seq > 1 {
		return []Link{{r.Prev, Pos{r.Stream(), r.Seq - 1}}}
	}
	return r.Links
}

func hashBody(b string) string {
	h := sha256.Sum256([]byte(b))
	return hex.EncodeToString(h[:])
}

func (r *Record) computeID() RecordID {
	var to []string
	for _, a := range r.To {
		to = append(to, fmt.Sprintf("%q", a.String()))
	}
	var links []string
	for _, l := range r.Links {
		links = append(links, fmt.Sprintf("%q@%q.%d.%d", l.ID, l.At.Stream.Author, l.At.Stream.Epoch, l.At.Seq))
	}
	s := fmt.Sprintf("v1|%q|%d|%d|%q|[%s]|%d|%d|%q|[%s]|%v|%q|%q|%q|%d|%s",
		r.Author, r.Epoch, r.Seq, r.Prev, strings.Join(links, ","), r.Version,
		r.Kind, r.Agent, strings.Join(to, ","), r.AskHandled,
		r.Ref, r.Parent, r.Thread, r.Created, r.BodyHash)
	if r.Manifest != "" { // absent for a record without files, so those ids are unchanged
		s += "|files:" + r.Manifest
	}
	h := sha256.Sum256([]byte(s))
	return RecordID(hex.EncodeToString(h[:8]))
}

// sign stands in for an Ed25519 signature: only the holder of key can make
// it, and anyone with the room's key table can check it.
func sign(key string, id RecordID) string {
	h := sha256.Sum256([]byte("sig|" + key + "|" + string(id)))
	return hex.EncodeToString(h[:8])
}

var errBadRecord = errors.New("record does not verify")

// verify checks a record as received from any machine: its id is the hash of
// its content, it is signed by the author's key, and the body (when present)
// matches. Requirement D8: authorship holds whoever passed the record on.
func (r *Record) verify(keys map[MachineID]string, body string, hasBody bool) error {
	key, ok := keys[r.Author]
	if !ok {
		return errBadRecord
	}
	if r.Seq < 1 || r.Kind < Message || r.Kind > Replace {
		return errBadRecord
	}
	if (r.Seq > 1 && (r.Prev == "" || len(r.Links) > 0)) || (r.Seq == 1 && r.Prev != "") {
		return errBadRecord
	}
	for _, l := range r.Links {
		if l.At.Stream.Author != r.Author || l.At.Seq < 1 {
			return errBadRecord
		}
	}
	if r.computeID() != r.ID || sign(key, r.ID) != r.Sig {
		return errBadRecord
	}
	if hasBody && hashBody(body) != r.BodyHash {
		return errBadRecord
	}
	return nil
}
