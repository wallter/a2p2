package core

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"slices"
	"time"
	"unicode/utf8"
)

// EntryKind is the kind of a room log entry (PROTOCOL section 3).
type EntryKind string

const (
	EntryCreate EntryKind = "create"
	EntryAdd    EntryKind = "add"
	EntryRemove EntryKind = "remove"
	EntryAdmin  EntryKind = "admin"
)

// EntryFields are what a signer chooses. Zero values are omitted.
type EntryFields struct {
	Room    string
	Seq     int64
	Prev    string
	Parents []string
	Kind    EntryKind
	Target  string
	Label   string // create: the room label
	Name    string // add, admin: the target's label
	History bool
	// From is the signer's heads per author: node id to record ids.
	From    map[string][]string
	Created time.Time
}

func (f EntryFields) clone() EntryFields {
	f.Parents = slices.Clone(f.Parents)
	if f.From != nil {
		from := make(map[string][]string, len(f.From))
		for k, v := range f.From {
			from[k] = slices.Clone(v)
		}
		f.From = from
	}
	return f
}

// Entry is a parsed log entry. Its fields cannot be changed after the
// checks: every accessor returns a copy. Header and signature are the bytes
// exactly as received.
type Entry struct {
	f      EntryFields
	v      int64
	author string
	id     string
	header []byte
	sig    []byte
}

func (e *Entry) Fields() EntryFields { return e.f.clone() }
func (e *Entry) ID() string          { return e.id }
func (e *Entry) Author() string      { return e.author }
func (e *Entry) Header() []byte      { return bytes.Clone(e.header) }
func (e *Entry) Sig() []byte         { return bytes.Clone(e.sig) }

// Wire returns the wire object {"h","s"}.
func (e *Entry) Wire() []byte { return encodeWire(e.header, e.sig, nil) }

// VerifySignature is G1: the Ed25519 signature by the author over the header.
func (e *Entry) VerifySignature() error { return verifySig(e.author, e.header, e.sig) }

// NewEntry encodes f once, signs it with key and returns the entry as a
// receiver would parse it. Parents and each From list are sorted for the
// caller; nothing is dropped to fit. A count over its limit is refused with
// CodeBadField naming the field, an entry over 16,384 bytes with
// CodeTooLarge ("header size").
func NewEntry(key ed25519.PrivateKey, f EntryFields) (*Entry, error) {
	if f.Created.IsZero() {
		return nil, refuseWrite(CodeBadField, "created")
	}
	if !utf8.ValidString(f.Label) {
		return nil, refuseWrite(CodeBadField, "label")
	}
	if len(f.Parents) > maxParents {
		return nil, refuseWrite(CodeBadField, "parents")
	}
	if len(f.From) > maxFromAuthors {
		return nil, refuseWrite(CodeBadField, "from")
	}
	for _, heads := range f.From {
		if len(heads) > maxFromHeads {
			return nil, refuseWrite(CodeBadField, "from")
		}
	}
	var w jwriter
	w.int("v", WireVersion)
	w.str("room", f.Room)
	w.str("author", hex.EncodeToString(key.Public().(ed25519.PublicKey)))
	w.int("seq", f.Seq)
	w.str("prev", f.Prev)
	w.strs("parents", sorted(f.Parents))
	w.str("kind", string(f.Kind))
	w.str("target", f.Target)
	w.str("label", f.Label)
	w.str("name", f.Name)
	w.bool("history", f.History)
	if len(f.From) > 0 {
		var fw jwriter
		for _, node := range sorted(mapKeys(f.From)) {
			fw.key(node)
			fw.b = appendStrings(fw.b, sorted(f.From[node]))
		}
		w.raw("from", fw.end())
	}
	w.str("created", formatTime(f.Created))
	header := w.end()
	if len(header) > maxEntry {
		return nil, refuseWrite(CodeTooLarge, "header size")
	}
	return EntryFromParts(header, ed25519.Sign(key, header))
}

// ParseEntry decodes a received wire object and runs S1 to S7 and V1 of
// PROTOCOL section 3, in that order, returning the first failure as a
// *Refusal. As for ParseRecord, the caller bounds the wire object. The
// caller then checks the room (R1) and the pinned create id (C1, using ID)
// before VerifySignature (G1), then the chain (C2).
func ParseEntry(wire []byte) (*Entry, error) {
	h, s, _, _, err := wireParts(wire, false, maxEntry)
	if err != nil {
		return nil, err
	}
	e, err := entryFromParts(h, s, false)
	if err != nil {
		return nil, err
	}
	return e, nil
}

// EntryFromParts runs the same checks (from S3) on header and signature
// bytes. It copies its inputs.
func EntryFromParts(header, sig []byte) (*Entry, error) {
	e, err := entryFromParts(bytes.Clone(header), bytes.Clone(sig), false)
	if err != nil {
		return nil, err
	}
	return e, nil
}

// StoredEntryFromParts decodes an entry this machine stored after it
// passed the checks on receipt. It runs the checks of form only: sizes,
// field types, the fields each kind needs, and V1. The rules on what a
// room label or a machine name may contain are not run, so that a rule
// added to receipt later does not stop a log stored under the older rule
// from loading (DESIGN section 5). A new content rule goes in the
// receipt checks only, never here. It copies its inputs.
func StoredEntryFromParts(header, sig []byte) (*Entry, error) {
	e, err := entryFromParts(bytes.Clone(header), bytes.Clone(sig), true)
	if err != nil {
		return nil, err
	}
	return e, nil
}

// entryFromParts decodes h and s; stored skips the content rules
// (StoredEntryFromParts).
func entryFromParts(h, s []byte, stored bool) (*Entry, *Refusal) {
	switch {
	case len(s) != sigSize:
		return nil, refuse("S3", "signature")
	case len(h) > maxEntry:
		return nil, refuse("S3", "header")
	}
	top, err := headerObject(h)
	if err != nil {
		return nil, err
	}
	e, err := entryFields(top, stored)
	if err != nil {
		return nil, err
	}
	if e.v != WireVersion {
		return nil, refuse("V1", "")
	}
	e.id = EntryID(h)
	e.header, e.sig = h, s
	return e, nil
}

func entryFields(top jvalue, stored bool) (*Entry, *Refusal) {
	// Content rules on text fields: receipt only (StoredEntryFromParts).
	labelOK := func(s string) bool { return len(s) <= maxRoomLabel }
	nameOK := ValidLabel
	if stored {
		labelOK = func(string) bool { return true }
		nameOK = func(string) bool { return true }
	}
	e := &Entry{}
	f := &e.f
	has := map[string]bool{}
	for _, m := range top.obj {
		k, v := m.key, m.val
		ok := true
		switch k {
		case "v":
			e.v, ok = posInt(v)
		case "room":
			f.Room, ok = strOf(v, isRoom)
		case "author":
			e.author, ok = strOf(v, isNode)
		case "seq":
			f.Seq, ok = posInt(v)
		case "prev":
			f.Prev, ok = strOf(v, isID)
		case "parents":
			f.Parents, ok = idList(v, maxParents)
		case "kind":
			var s string
			s, ok = strOf(v, func(s string) bool {
				switch EntryKind(s) {
				case EntryCreate, EntryAdd, EntryRemove, EntryAdmin:
					return true
				}
				return false
			})
			f.Kind = EntryKind(s)
		case "target":
			f.Target, ok = strOf(v, isNode)
		case "label":
			f.Label, ok = strOf(v, labelOK)
		case "name":
			f.Name, ok = strOf(v, nameOK)
		case "history":
			f.History, ok = v.kind == jTrue, v.kind == jTrue
		case "from":
			f.From, ok = decodeFrom(v)
		case "created":
			var s string
			if s, ok = strOf(v, func(string) bool { return true }); ok {
				f.Created, ok = parseTime(s)
			}
		default:
			// Unknown top-level field: any value but null (section 1).
			if !v.noNull() {
				return nil, refuse("S6", k)
			}
			continue
		}
		if !ok || !v.present() {
			return nil, refuse("S6", k)
		}
		has[k] = true
	}

	// S7.
	for _, k := range []string{"v", "room", "author", "seq", "kind", "created"} {
		if !has[k] {
			return nil, refuse("S7", k)
		}
	}
	if (f.Seq > 1) != has["prev"] {
		return nil, refuse("S7", "prev")
	}
	var required, optional []string
	switch f.Kind {
	case EntryCreate:
		if f.Seq != 1 {
			return nil, refuse("S7", "seq")
		}
		required = []string{"label"}
	case EntryAdd:
		required, optional = []string{"parents", "target", "name"}, []string{"history", "from"}
	case EntryRemove:
		required = []string{"parents", "target"}
	case EntryAdmin:
		required, optional = []string{"parents", "target"}, []string{"name", "history", "from"}
	}
	for _, k := range []string{"parents", "target", "label", "name", "history", "from"} {
		in, opt := slices.Contains(required, k), slices.Contains(optional, k)
		if has[k] && !in && !opt || !has[k] && in {
			return nil, refuse("S7", k)
		}
	}
	if has["prev"] && !slices.Contains(f.Parents, f.Prev) {
		return nil, refuse("S7", "parents")
	}
	return e, nil
}

// idList is a sorted, unique array of 1 to limit record or entry ids.
func idList(v jvalue, limit int) ([]string, bool) {
	if v.kind != jArray || len(v.arr) > limit {
		return nil, false
	}
	out := make([]string, 0, len(v.arr))
	for _, x := range v.arr {
		s, ok := strOf(x, isID)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, strictlySorted(out)
}

func decodeFrom(v jvalue) (map[string][]string, bool) {
	if v.kind != jObject || len(v.obj) > maxFromAuthors {
		return nil, false
	}
	out := make(map[string][]string, len(v.obj))
	for _, m := range v.obj {
		ids, ok := idList(m.val, maxFromHeads)
		if !isNode(m.key) || !ok {
			return nil, false
		}
		out[m.key] = ids
	}
	return out, true
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

func mapKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
