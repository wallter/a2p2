package core

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Record kinds besides KindMessage (PROTOCOL section 1).
const (
	KindHandled  Kind = "handled"
	KindWithdraw Kind = "withdraw"
	KindReplace  Kind = "replace"
)

// Link names one earlier record of the same author, in a stream's first
// record.
type Link struct {
	Epoch string
	Seq   int64
	ID    string
}

// Addressee is one agent on one node in a message's to.
type Addressee struct {
	Node  string
	Agent string
}

// RecordFields are what an author chooses, and what Record.Fields returns.
// Zero values are omitted on the wire.
type RecordFields struct {
	Room       string
	Epoch      string
	Seq        int64
	Prev       string
	Links      []Link
	Version    int64
	Kind       Kind
	Agent      string
	To         []Addressee
	AskHandled bool
	Ref        string
	Parent     string
	Thread     string
	Created    time.Time
	// Files is the SHA-256 of the manifest of a message or replacement
	// with files (PROTOCOL 10.1), or empty. From a parsed record it is set
	// only when the field is well formed on a kind that may carry it.
	Files string
	// Text and Choices are the body, for message and replace. From a parsed
	// record they are set only if the body was present.
	Text    string
	Choices []string
}

func (f RecordFields) clone() RecordFields {
	f.Links, f.To, f.Choices = slices.Clone(f.Links), slices.Clone(f.To), slices.Clone(f.Choices)
	return f
}

// Record is a parsed record. Its fields cannot be changed after the checks:
// every accessor returns a copy. The header, signature and body are the
// bytes exactly as received; nothing is ever re-encoded.
type Record struct {
	f        RecordFields
	v        int64
	author   string
	bodyHash string
	size     int64
	id       string
	header   []byte
	sig      []byte
	body     []byte // nil when no text came with the header
	// filesBad: the header has a files field that is not a manifest hash
	// on a message or replace; stored as 0.1.0 stores it, read as no files.
	filesBad bool
}

func (r *Record) Fields() RecordFields { return r.f.clone() }
func (r *Record) ID() string           { return r.id }
func (r *Record) Author() string       { return r.author }
func (r *Record) BodyHash() string     { return r.bodyHash }
func (r *Record) Size() int64          { return r.size }
func (r *Record) Header() []byte       { return bytes.Clone(r.header) }
func (r *Record) Sig() []byte          { return bytes.Clone(r.sig) }

// FilesMalformed reports a files header field that is not a manifest hash
// on a message or replace: kept in the bytes and the id, read as no files.
func (r *Record) FilesMalformed() bool { return r.filesBad }

// Body returns the body bytes, or nil if no text is held.
func (r *Record) Body() []byte { return bytes.Clone(r.body) }

// WithoutBody returns the same record with no text: what a relay passes on
// when it has dropped or may not serve the text. It keeps id and signature.
func (r *Record) WithoutBody() *Record {
	c := *r
	c.body, c.f.Text, c.f.Choices = nil, "", nil
	return &c
}

// Wire returns the wire object {"h","s"[,"b"]}.
func (r *Record) Wire() []byte { return encodeWire(r.header, r.sig, r.body) }

// VerifySignature is G1: the Ed25519 signature by the author over the header.
func (r *Record) VerifySignature() error { return verifySig(r.author, r.header, r.sig) }

// NewRecord encodes f once, signs it with key and returns the record as a
// receiver would parse it. It never drops addressees or links to fit: a
// first record over 100 links or 8,192 header bytes is refused with
// CodeTooManyHeads, any other header over 4,096 bytes with CodeTooLarge
// ("header size"), and a count over its limit with CodeBadField naming the
// field.
func NewRecord(key ed25519.PrivateKey, f RecordFields) (*Record, error) {
	if f.Created.IsZero() {
		return nil, refuseWrite(CodeBadField, "created")
	}
	if !utf8.ValidString(f.Text) {
		return nil, refuseWrite(CodeBadField, "text")
	}
	for _, c := range f.Choices {
		if !utf8.ValidString(c) {
			return nil, refuseWrite(CodeBadField, "choices")
		}
	}
	switch {
	case f.Seq == 1 && len(f.Links) > maxLinks:
		return nil, refuseWrite(CodeTooManyHeads, "links")
	case len(f.To) > maxTo:
		return nil, refuseWrite(CodeBadField, "to")
	case f.Files != "" && (f.Kind != KindMessage && f.Kind != KindReplace || !isHex(f.Files, 64)):
		return nil, refuseWrite(CodeBadField, "files")
	}
	var body []byte
	if f.Kind == KindMessage || f.Kind == KindReplace {
		var bw jwriter
		bw.str("text", f.Text)
		bw.strs("choices", f.Choices)
		body = bw.end()
	}
	sum := sha256.Sum256(body)

	to := slices.Clone(f.To)
	slices.SortFunc(to, func(a, b Addressee) int {
		if c := strings.Compare(a.Node, b.Node); c != 0 {
			return c
		}
		return strings.Compare(a.Agent, b.Agent)
	})
	links := slices.Clone(f.Links)
	slices.SortFunc(links, func(a, b Link) int { return strings.Compare(a.ID, b.ID) })

	var w jwriter
	w.int("v", WireVersion)
	w.str("room", f.Room)
	w.str("author", hex.EncodeToString(key.Public().(ed25519.PublicKey)))
	w.str("epoch", f.Epoch)
	w.int("seq", f.Seq)
	w.str("prev", f.Prev)
	if len(links) > 0 {
		b := []byte{'['}
		for i, l := range links {
			if i > 0 {
				b = append(b, ',')
			}
			var lw jwriter
			lw.str("epoch", l.Epoch)
			lw.int("seq", l.Seq)
			lw.str("id", l.ID)
			b = append(b, lw.end()...)
		}
		w.raw("links", append(b, ']'))
	}
	w.int("version", f.Version)
	w.str("kind", string(f.Kind))
	w.str("agent", f.Agent)
	if len(to) > 0 {
		b := []byte{'['}
		for i, a := range to {
			if i > 0 {
				b = append(b, ',')
			}
			var aw jwriter
			aw.str("node", a.Node)
			aw.str("agent", a.Agent)
			b = append(b, aw.end()...)
		}
		w.raw("to", append(b, ']'))
	}
	w.bool("ask_handled", f.AskHandled)
	w.str("ref", f.Ref)
	w.str("parent", f.Parent)
	w.str("thread", f.Thread)
	w.str("created", formatTime(f.Created))
	w.str("body", hex.EncodeToString(sum[:]))
	w.int("size", int64(len(body)))
	w.str("files", f.Files)
	header := w.end()
	switch {
	case f.Seq == 1 && len(header) > maxFirstHeader:
		return nil, refuseWrite(CodeTooManyHeads, "header size")
	case f.Seq != 1 && len(header) > maxHeader:
		return nil, refuseWrite(CodeTooLarge, "header size")
	}
	return RecordFromParts(header, ed25519.Sign(key, header), body)
}

// ParseRecord decodes a received wire object and runs the syntax checks of
// PROTOCOL section 2 (S1 to S9) and V1, in that order, returning the
// first failure as a *Refusal. It does not bound the wire object itself:
// the caller has already bounded the request it came in (PROTOCOL
// 4.6), and nothing larger than the section 1 limits is decoded. It does
// not check the signature (G1, see VerifySignature), which comes after the
// caller's room (R1) and admission (A1) checks, nor anything that needs
// stored state.
func ParseRecord(wire []byte) (*Record, error) {
	h, s, b, hasB, err := wireParts(wire, true, maxFirstHeader)
	if err != nil {
		return nil, err
	}
	r, err := recordFromParts(h, s, b, hasB)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// RecordFromParts runs the same checks (from S3) on stored or already
// unwrapped parts: header and signature bytes and the body, nil or empty if
// no text is held. It copies its inputs.
func RecordFromParts(header, sig, body []byte) (*Record, error) {
	r, err := recordFromParts(bytes.Clone(header), bytes.Clone(sig), bytes.Clone(body), false)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func recordFromParts(h, s, b []byte, hasB bool) (*Record, *Refusal) {
	switch {
	case len(s) != sigSize:
		return nil, refuse("S3", "signature")
	case len(b) > maxBody:
		return nil, refuse("S3", "body")
	// The header limit depends on seq, so a header between the two limits
	// must parse with seq 1 to be allowed.
	case len(h) > maxFirstHeader || (len(h) > maxHeader && !firstRecord(h)):
		return nil, refuse("S3", "header")
	}
	top, err := headerObject(h) // S4, S5
	if err != nil {
		return nil, err
	}
	r, err := recordFields(top) // S6, S7
	if err != nil {
		return nil, err
	}
	if err := r.checkBody(b, hasB); err != nil { // S8, S9
		return nil, err
	}
	if r.v != WireVersion { // V1
		return nil, refuse("V1", "")
	}
	r.id = RecordID(h)
	r.header, r.sig = h, s
	if len(b) > 0 {
		r.body = b
	}
	return r, nil
}

func firstRecord(h []byte) bool {
	v, err := parseStrict(h)
	if err != nil || v.kind != jObject {
		return false
	}
	for _, m := range v.obj {
		if m.key == "seq" {
			n, ok := m.val.integer()
			return ok && n == 1
		}
	}
	return false
}

// recordFields is S6 (types and formats) then S7 (fields per kind and seq,
// order and uniqueness).
func recordFields(top jvalue) (*Record, *Refusal) {
	r := &Record{}
	f := &r.f
	has := map[string]bool{}
	var files *jvalue
	for i, m := range top.obj {
		k, v := m.key, m.val
		ok := true
		switch k {
		case "v":
			r.v, ok = posInt(v)
		case "room":
			f.Room, ok = strOf(v, isRoom)
		case "author":
			r.author, ok = strOf(v, isNode)
		case "epoch":
			f.Epoch, ok = strOf(v, isEpoch)
		case "seq":
			f.Seq, ok = posInt(v)
		case "prev":
			f.Prev, ok = strOf(v, isID)
		case "links":
			f.Links, ok = decodeLinks(v)
		case "version":
			f.Version, ok = posInt(v)
		case "kind":
			var s string
			s, ok = strOf(v, func(s string) bool {
				switch Kind(s) {
				case KindMessage, KindHandled, KindWithdraw, KindReplace:
					return true
				}
				return false
			})
			f.Kind = Kind(s)
		case "agent":
			f.Agent, ok = strOf(v, ValidLabel)
		case "to":
			f.To, ok = decodeTo(v)
		case "ask_handled":
			f.AskHandled, ok = v.kind == jTrue, v.kind == jTrue
		case "ref":
			f.Ref, ok = strOf(v, isID)
		case "parent":
			f.Parent, ok = strOf(v, isID)
		case "thread":
			f.Thread, ok = strOf(v, isID)
		case "created":
			var s string
			if s, ok = strOf(v, func(string) bool { return true }); ok {
				f.Created, ok = parseTime(s)
			}
		case "body":
			r.bodyHash, ok = strOf(v, func(s string) bool { return isHex(s, 64) })
		case "size":
			r.size, ok = posInt(v)
		default:
			// Unknown top-level field: any value but null, kept in the
			// bytes, ignored (section 1).
			if !v.noNull() {
				return nil, refuse("S6", k)
			}
			if k == "files" {
				files = &top.obj[i].val // read below, once the kind is known (PROTOCOL 10.1)
			}
			continue
		}
		if !ok || !v.present() {
			return nil, refuse("S6", k)
		}
		has[k] = true
	}

	// S7.
	for _, k := range []string{"v", "room", "author", "epoch", "seq", "kind", "agent", "created", "body"} {
		if !has[k] {
			return nil, refuse("S7", k)
		}
	}
	if (f.Seq > 1) != has["prev"] {
		return nil, refuse("S7", "prev")
	}
	if f.Seq > 1 && has["links"] {
		return nil, refuse("S7", "links")
	}
	allowed := map[Kind][]string{
		KindMessage:  {"to", "ask_handled", "parent", "thread", "size"},
		KindHandled:  {"ref"},
		KindWithdraw: {"ref"},
		KindReplace:  {"ref", "version", "size"},
	}[f.Kind]
	for _, k := range []string{"to", "ask_handled", "parent", "thread", "ref", "version", "size"} {
		if has[k] && !slices.Contains(allowed, k) {
			return nil, refuse("S7", k)
		}
	}
	switch f.Kind {
	case KindMessage:
		if has["to"] && has["ask_handled"] {
			return nil, refuse("S7", "ask_handled")
		}
		if has["parent"] != has["thread"] {
			return nil, refuse("S7", "thread")
		}
		if !has["size"] {
			return nil, refuse("S7", "size")
		}
	case KindHandled, KindWithdraw:
		if !has["ref"] {
			return nil, refuse("S7", "ref")
		}
	case KindReplace:
		for _, k := range []string{"ref", "version", "size"} {
			if !has[k] {
				return nil, refuse("S7", k)
			}
		}
	}
	for i := 1; i < len(f.To); i++ {
		a, b := f.To[i-1], f.To[i]
		if a.Node > b.Node || (a.Node == b.Node && a.Agent >= b.Agent) {
			return nil, refuse("S7", "to")
		}
	}
	at := map[Link]bool{}
	for i, l := range f.Links {
		p := Link{Epoch: l.Epoch, Seq: l.Seq}
		if i > 0 && f.Links[i-1].ID >= l.ID || at[p] {
			return nil, refuse("S7", "links")
		}
		at[p] = true
	}
	if files != nil {
		if (f.Kind == KindMessage || f.Kind == KindReplace) && files.kind == jString && isHex(files.str, 64) {
			f.Files = files.str
		} else {
			r.filesBad = true
		}
	}
	return r, nil
}

// object returns v's members by key if v is an object with exactly keys.
func object(v jvalue, keys ...string) (map[string]jvalue, bool) {
	if v.kind != jObject {
		return nil, false
	}
	out := make(map[string]jvalue, len(v.obj))
	for _, m := range v.obj {
		if !slices.Contains(keys, m.key) {
			return nil, false
		}
		out[m.key] = m.val
	}
	return out, len(out) == len(keys)
}

func decodeLinks(v jvalue) ([]Link, bool) {
	if v.kind != jArray || len(v.arr) > maxLinks {
		return nil, false
	}
	out := make([]Link, 0, len(v.arr))
	for _, e := range v.arr {
		m, ok := object(e, "epoch", "seq", "id")
		if !ok {
			return nil, false
		}
		var l Link
		var ok1, ok2, ok3 bool
		l.Epoch, ok1 = strOf(m["epoch"], isEpoch)
		l.Seq, ok2 = posInt(m["seq"])
		l.ID, ok3 = strOf(m["id"], isID)
		if !ok1 || !ok2 || !ok3 {
			return nil, false
		}
		out = append(out, l)
	}
	return out, true
}

func decodeTo(v jvalue) ([]Addressee, bool) {
	if v.kind != jArray || len(v.arr) > maxTo {
		return nil, false
	}
	out := make([]Addressee, 0, len(v.arr))
	for _, e := range v.arr {
		m, ok := object(e, "node", "agent")
		if !ok {
			return nil, false
		}
		var a Addressee
		var ok1, ok2 bool
		a.Node, ok1 = strOf(m["node"], isNode)
		a.Agent, ok2 = strOf(m["agent"], ValidLabel)
		if !ok1 || !ok2 {
			return nil, false
		}
		out = append(out, a)
	}
	return out, true
}

// checkBody is S8 and S9. hasB says the wire object had a b key, which for a
// message or replace must then hold a body.
func (r *Record) checkBody(b []byte, hasB bool) *Refusal {
	switch r.f.Kind {
	case KindHandled, KindWithdraw:
		if len(b) != 0 || r.bodyHash != emptyHash {
			return refuse("S8", "not empty")
		}
		return nil
	}
	if len(b) == 0 {
		if hasB {
			return refuse("S9", "empty")
		}
		return nil // header only: no text sent, or dropped by a relay
	}
	if int64(len(b)) != r.size {
		return refuse("S9", "size")
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != r.bodyHash {
		return refuse("S9", "hash")
	}
	text, choices, ok := decodeBody(r.f.Kind, b)
	if !ok {
		return refuse("S9", "content")
	}
	r.f.Text, r.f.Choices = text, choices
	return nil
}

func decodeBody(k Kind, b []byte) (string, []string, bool) {
	v, err := parseStrict(b)
	if err != nil || v.kind != jObject || !v.present() {
		return "", nil, false
	}
	var text string
	var choices []string
	for _, m := range v.obj {
		switch {
		case m.key == "text" && m.val.kind == jString:
			text = m.val.str
		case m.key == "choices" && k == KindMessage && m.val.kind == jArray:
			if len(m.val.arr) > maxChoices {
				return "", nil, false
			}
			for _, c := range m.val.arr {
				if c.kind != jString || len(c.str) > maxChoiceBytes || slices.Contains(choices, c.str) {
					return "", nil, false
				}
				choices = append(choices, c.str)
			}
		default:
			return "", nil, false
		}
	}
	blank := strings.TrimFunc(text, func(r rune) bool { return unicode.Is(unicode.White_Space, r) }) == ""
	return text, choices, !blank
}
