package core

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Golden vectors, PROTOCOL section 9. Regenerate after a deliberate
// format change with:
//
//	go test ./internal/core -run TestGoldenVectors -update
var update = flag.Bool("update", false, "rewrite testdata/vectors")

const vectorDir = "testdata/vectors"

// Fixed test keys: Ed25519 keys from fixed seeds, so signatures (which are
// deterministic) and ids never change.
var (
	keyA  = testKey("a2p2 test key A")
	keyB  = testKey("a2p2 test key B")
	keyC  = testKey("a2p2 test key C")
	nodeA = nodeOf(keyA)
	nodeB = nodeOf(keyB)
	nodeC = nodeOf(keyC)

	vRoom    = "r_00112233445566778899aabbccddeeff"
	vEpoch   = "0123456789abcdef"
	vCreated = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
)

func testKey(seed string) ed25519.PrivateKey {
	s := sha256.Sum256([]byte(seed))
	return ed25519.NewKeyFromSeed(s[:])
}

func nodeOf(k ed25519.PrivateKey) string { return hex.EncodeToString(k.Public().(ed25519.PublicKey)) }

// fakeID is a well-formed id for references the vectors do not need to hold.
func fakeID(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:16])
}

// vector is one file under testdata/vectors. Room is the room of the request
// the wire object arrived in (R1); Create is the room's pinned create id (C1).
type vector struct {
	Type   string `json:"type"`
	Note   string `json:"note"`
	Room   string `json:"room"`
	Create string `json:"create,omitempty"`
	Wire   string `json:"wire"`
	ID     string `json:"id,omitempty"`
	Code   string `json:"code,omitempty"`
	Check  string `json:"check,omitempty"` // the check that fails, PROTOCOL sections 2 and 3
}

// outcome runs the stateless receive checks in the order of PROTOCOL
// sections 2 and 3, doing the caller's part (R1, A1, C1) as a node will. It
// returns the id, or the code and check of the first failure.
func outcome(v vector) (id, code, check string) {
	admitted := map[string]bool{nodeA: true, nodeB: true, nodeC: true}
	wire := []byte(v.Wire)
	fail := func(err error) (string, string, string) {
		var r *Refusal
		if !errors.As(err, &r) {
			return "", "not a refusal", ""
		}
		return "", r.Code, r.Check
	}
	switch v.Type {
	case "record":
		r, err := ParseRecord(wire)
		switch {
		case err != nil:
			return fail(err)
		case r.Fields().Room != v.Room:
			return "", CodeWrongRoom, "R1"
		case !admitted[r.Author()]:
			return "", CodeAuthorNotAdmitted, "A1"
		}
		if err := r.VerifySignature(); err != nil {
			return fail(err)
		}
		return r.ID(), "", ""
	case "entry":
		e, err := ParseEntry(wire)
		switch {
		case err != nil:
			return fail(err)
		case e.Fields().Room != v.Room:
			return "", CodeWrongRoom, "R1"
		case e.Fields().Kind == EntryCreate && e.ID() != v.Create:
			return "", CodeSecondCreate, "C1"
		}
		if err := e.VerifySignature(); err != nil {
			return fail(err)
		}
		return e.ID(), "", ""
	case "manifest":
		if _, err := ParseManifest(wire); err != nil {
			code := "invalid_manifest"
			if errors.Is(err, ErrManifestVersion) {
				code = "unsupported_manifest"
			}
			return "", code, manifestCheck(err)
		}
		return SHA256Hex(wire), "", ""
	}
	return "", "unknown vector type", ""
}

// manifestCheck names the rule of PROTOCOL 10.2 a manifest broke.
func manifestCheck(err error) string {
	switch {
	case errors.Is(err, ErrManifestJSON):
		return "M1"
	case errors.Is(err, ErrManifestVersion):
		return "M2"
	}
	return "M3"
}

func signWire(k ed25519.PrivateKey, header, body []byte) string {
	return string(encodeWire(header, ed25519.Sign(k, header), body))
}

func wireOf(s, h, b string) string {
	w := fmt.Sprintf(`{"h":%q,"s":%q`, h, s)
	if b != "" {
		w += fmt.Sprintf(`,"b":%q`, b)
	}
	return w + "}"
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// edit replaces old with new in header exactly once, or panics: a vector
// that silently stopped testing anything would be worse than none.
func edit(header []byte, old, new string) []byte {
	if strings.Count(string(header), old) != 1 {
		panic(fmt.Sprintf("vector edit: %q not found once", old))
	}
	return []byte(strings.Replace(string(header), old, new, 1))
}

// insert adds a member at the start of a header object.
func insert(header []byte, member string) []byte {
	return append([]byte("{"+member+","), header[1:]...)
}

func mustRecord(k ed25519.PrivateKey, f RecordFields) *Record {
	r, err := NewRecord(k, f)
	if err != nil {
		panic(err)
	}
	return r
}

func mustEntry(k ed25519.PrivateKey, f EntryFields) *Entry {
	e, err := NewEntry(k, f)
	if err != nil {
		panic(err)
	}
	return e
}

// messageHeader is a seq-2 message header by A with the given body, written
// by hand so a vector can carry a body NewRecord would refuse.
func messageHeader(prev string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return fmt.Appendf(nil, `{"v":2,"room":%q,"author":%q,"epoch":%q,"seq":2,"prev":%q,"kind":"message","agent":"claude","created":"2026-11-23T12:00:00.000Z","body":%q,"size":%d}`,
		vRoom, nodeA, vEpoch, prev, hex.EncodeToString(sum[:]), len(body))
}

type namedVector struct {
	name string
	v    vector
}

// buildVectors derives every vector from the fixed keys and inputs. want is
// the intended outcome: "" for valid, else the refusal code.
func buildVectors() []namedVector {
	var out []namedVector
	add := func(name, typ, note, want, wire string) {
		v := vector{Type: typ, Note: note, Room: vRoom, Wire: wire, Code: want}
		if want != "" { // the note of a failing vector starts with its check id
			v.Check = strings.FieldsFunc(note, func(r rune) bool { return r == ':' || r == ',' })[0]
		}
		out = append(out, namedVector{name, v})
	}
	rec := func(name, note, want, wire string) { add(name, "record", note, want, wire) }

	base := RecordFields{Room: vRoom, Epoch: vEpoch, Agent: "claude", Created: vCreated}
	first := base
	first.Seq, first.Kind, first.Text = 1, KindMessage, "first"
	r1 := mustRecord(keyA, first)

	msg := base
	msg.Seq, msg.Prev, msg.Kind = 2, r1.ID(), KindMessage
	msg.To = []Addressee{{nodeB, "codex"}, {nodeA, "reviewer"}} // sorted by NewRecord
	msg.Parent, msg.Thread = fakeID("parent"), fakeID("thread")
	msg.Text, msg.Choices = "Which one?", []string{"left", "right"}
	rm := mustRecord(keyA, msg)
	rec("record-message", "a message with to, parent, thread and choices: id, signature and body hash", "", string(rm.Wire()))

	ask := base
	ask.Seq, ask.Prev, ask.Kind, ask.AskHandled, ask.Text = 3, rm.ID(), KindMessage, true, "Anyone?"
	ra := mustRecord(keyA, ask)
	rec("record-message-everyone-ask", "to omitted (everyone), ask_handled", "", string(ra.Wire()))

	fl := base
	fl.Epoch, fl.Seq, fl.Kind, fl.Text = "fedcba9876543210", 1, KindMessage, "after a restart"
	fl.Links = []Link{{Epoch: vEpoch, Seq: 6, ID: fakeID("head two")}, {Epoch: "1111111111111111", Seq: 7, ID: fakeID("head one")}}
	rfl := mustRecord(keyA, fl)
	rec("record-first-links", "seq 1 of a new stream with two links, sorted by id", "", string(rfl.Wire()))

	hd := base
	hd.Seq, hd.Prev, hd.Kind, hd.Ref = 4, ra.ID(), KindHandled, rm.ID()
	rh := mustRecord(keyA, hd)
	rec("record-handled", "handled: empty-body hash, no b, no size", "", string(rh.Wire()))

	wd := base
	wd.Seq, wd.Prev, wd.Kind, wd.Ref = 5, rh.ID(), KindWithdraw, rm.ID()
	rw := mustRecord(keyA, wd)
	rec("record-withdraw", "withdraw: empty-body hash, no b, no size", "", string(rw.Wire()))

	rp := base
	rp.Seq, rp.Prev, rp.Kind, rp.Ref, rp.Version, rp.Text = 6, rw.ID(), KindReplace, ra.ID(), 1, "Anyone at all?"
	rr := mustRecord(keyA, rp)
	rec("record-replace", "replace: version, ref, body {\"text\"}", "", string(rr.Wire()))

	unk := insert(ra.Header(), `"x_future":{"k":[0,1.5,false,{},"",[]]}`)
	rec("record-unknown-field", "an unknown top-level field, any value but null, is kept in the id and ignored", "", signWire(keyA, unk, ra.Body()))
	rec("record-wire-whitespace", "record-handled with insignificant whitespace in and around the wire object", "",
		" \n{ \"h\" : "+fmt.Sprintf("%q", b64(rh.Header()))+" ,\t\"s\":"+fmt.Sprintf("%q", b64(rh.Sig()))+"}\r\n")
	rec("record-handled-empty-b", "record-handled with \"b\":\"\", which S8 allows", "",
		strings.TrimSuffix(string(rh.Wire()), "}")+`,"b":""}`)

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rr.Header(), &fields); err != nil {
		panic(err)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	slices.Reverse(keys)
	reordered := []byte("{")
	for i, k := range keys {
		if i > 0 {
			reordered = append(reordered, ',')
		}
		reordered = fmt.Appendf(reordered, "%q:%s", k, fields[k])
	}
	reordered = append(reordered, '}')
	rec("record-key-order", "record-replace's fields in another order: a different id, still valid", "", signWire(keyA, reordered, rr.Body()))

	// Attachments (PROTOCOL 10.1, 10.2): a message and a replacement with
	// the files field, a files field where it does not belong (stored, read
	// as no files), and manifests.
	man := []byte(`{"v":1,"salt":"000102030405060708090a0b0c0d0e0f","files":[{"name":"build.log","size":14,"sha256":"` +
		SHA256Hex([]byte("build log text")) + `","type":"text/plain"}]}`)
	mf := base
	mf.Seq, mf.Prev, mf.Kind, mf.Text, mf.Files = 7, rr.ID(), KindMessage, "The log is attached.", SHA256Hex(man)
	rmf := mustRecord(keyA, mf)
	rec("record-message-files", "a message with files: the SHA-256 of its manifest (manifest-valid), nothing else about the files", "", string(rmf.Wire()))
	rpf := base
	rpf.Seq, rpf.Prev, rpf.Kind, rpf.Ref, rpf.Version, rpf.Text, rpf.Files = 8, rmf.ID(), KindReplace, rmf.ID(), 1, "The log, again.", SHA256Hex(man)
	rrf := mustRecord(keyA, rpf)
	rec("record-replace-files", "a replacement that lists the same manifest again", "", string(rrf.Wire()))
	rec("record-files-on-handled", "files on a handled report: an unknown field there, stored and kept in the id, read as no files", "",
		signWire(keyA, insert(rh.Header(), `"files":"`+SHA256Hex(man)+`"`), nil))
	rec("record-files-malformed", "files that is not 64 lowercase hex: stored, kept in the id, read as no files", "",
		signWire(keyA, insert(ra.Header(), `"files":"`+strings.ToUpper(SHA256Hex(man))+`"`), ra.Body()))
	mfst := func(name, note, want string, b []byte) { add(name, "manifest", note, want, string(b)) }
	mfst("manifest-valid", "a manifest with one file: its id is its SHA-256, the value of a header's files", "", man)
	mfst("manifest-unknown-key", "an unknown key in a manifest and in a file is ignored", "",
		[]byte(`{"v":1,"salt":"000102030405060708090a0b0c0d0e0f","files":[{"name":"a b.txt","size":1,"sha256":"`+SHA256Hex([]byte("x"))+`","later":[1]}],"later":{"k":"v"}}`))
	mfst("manifest-bad-json", "M1: a duplicate key", "invalid_manifest",
		[]byte(`{"v":1,"v":1,"salt":"000102030405060708090a0b0c0d0e0f","files":[]}`))
	mfst("manifest-version", "M2: v 2", "unsupported_manifest",
		[]byte(`{"v":2,"salt":"000102030405060708090a0b0c0d0e0f","files":[{"name":"a","size":1,"sha256":"`+SHA256Hex([]byte("x"))+`"}]}`))
	mfst("manifest-name-slash", "M3: a name with /", "invalid_manifest",
		[]byte(`{"v":1,"salt":"000102030405060708090a0b0c0d0e0f","files":[{"name":"../a","size":1,"sha256":"`+SHA256Hex([]byte("x"))+`"}]}`))
	mfst("manifest-duplicate-hash", "M3: the same sha256 twice", "invalid_manifest",
		[]byte(`{"v":1,"salt":"000102030405060708090a0b0c0d0e0f","files":[{"name":"a","size":1,"sha256":"`+SHA256Hex([]byte("x"))+
			`"},{"name":"b","size":1,"sha256":"`+SHA256Hex([]byte("x"))+`"}]}`))
	mfst("manifest-too-big", "M3: a size over 268,435,456", "invalid_manifest",
		[]byte(`{"v":1,"salt":"000102030405060708090a0b0c0d0e0f","files":[{"name":"a","size":268435457,"sha256":"`+SHA256Hex([]byte("x"))+`"}]}`))

	// Entries: one per kind, by A, in one chain.
	ent := func(name, note, want string, e *Entry) { add(name, "entry", note, want, string(e.Wire())) }
	ec := mustEntry(keyA, EntryFields{Room: vRoom, Seq: 1, Kind: EntryCreate, Label: "design review", Created: vCreated})
	ent("entry-create", "create: seq 1, label", "", ec)
	ea := mustEntry(keyA, EntryFields{Room: vRoom, Seq: 2, Prev: ec.ID(), Parents: []string{ec.ID()}, Kind: EntryAdd,
		Target: nodeB, Name: "codex", History: true, Created: vCreated})
	ent("entry-add", "add with history", "", ea)
	eaf := mustEntry(keyA, EntryFields{Room: vRoom, Seq: 3, Prev: ea.ID(), Parents: []string{ea.ID()}, Kind: EntryAdd,
		Target: nodeC, Name: "gemini", Created: vCreated,
		From: map[string][]string{nodeA: {rr.ID(), rfl.ID()}, nodeB: {fakeID("b head")}}})
	ent("entry-add-from", "add without history: from with two authors", "", eaf)
	er := mustEntry(keyA, EntryFields{Room: vRoom, Seq: 4, Prev: eaf.ID(), Parents: []string{eaf.ID()}, Kind: EntryRemove,
		Target: nodeC, Created: vCreated})
	ent("entry-remove", "remove", "", er)
	ead := mustEntry(keyA, EntryFields{Room: vRoom, Seq: 5, Prev: er.ID(), Parents: []string{er.ID()}, Kind: EntryAdmin,
		Target: nodeB, Name: "codex", Created: vCreated})
	ent("entry-admin", "admin of an existing member", "", ead)
	eafh := mustEntry(keyA, EntryFields{Room: vRoom, Seq: 6, Prev: ead.ID(), Parents: []string{ead.ID()}, Kind: EntryAdd,
		Target: nodeC, Name: "gemini", History: true, Created: vCreated,
		From: map[string][]string{nodeA: {rr.ID()}}})
	ent("entry-add-from-history", "add with history and from: from is where reading starts, history whether earlier text is readable", "", eafh)
	e2 := mustEntry(keyB, EntryFields{Room: vRoom, Seq: 1, Kind: EntryCreate, Label: "impostor", Created: vCreated})
	ent("entry-second-create", "C1: a create entry that is not the room's pinned create", CodeSecondCreate, e2)

	// Refusals. Each changes one thing in record-message (or another valid
	// record) and re-signs, so only the check named fails.
	h, s, b := b64(rm.Header()), b64(rm.Sig()), b64(rm.Body())
	sign := func(header []byte) string { return signWire(keyA, header, rm.Body()) }

	rec("bad-wire-extra-key", "S1: a key other than h, s, b", CodeBadWire, strings.TrimSuffix(wireOf(s, h, b), "}")+`,"x":"y"}`)
	rec("bad-base64-unpadded", "S2: signature without its = padding", CodeBadBase64, wireOf(strings.TrimRight(s, "="), h, b))
	padded := []byte(s) // 64 bytes: two pad characters, four pad bits in the last data character
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	padded[len(padded)-3] = alphabet[strings.IndexByte(alphabet, padded[len(padded)-3])|1]
	rec("bad-base64-pad-bits", "S2: non-zero pad bits", CodeBadBase64, wireOf(string(padded), h, b))
	rec("bad-base64-newline", "S2: a line break inside h", CodeBadBase64, wireOf(s, h[:40]+"\n"+h[40:], b))

	big := insert(rm.Header(), `"pad":"`+strings.Repeat("a", maxHeader-len(rm.Header())-8)+`"`)
	if len(big) != maxHeader+1 {
		panic(len(big))
	}
	rec("too-large-header", "S3: 4,097-byte header with seq 2", CodeTooLarge, sign(big))
	bigBody := []byte(`{"text":"` + strings.Repeat("a", maxBody-10) + `"}`)
	rec("too-large-body", "S3: 16,385-byte body whose hash and size match", CodeTooLarge,
		signWire(keyA, messageHeader(r1.ID(), bigBody), bigBody))
	rec("bad-signature-length", "S3: 63-byte signature", CodeTooLarge, wireOf(b64(rm.Sig()[:63]), h, b))

	rec("bad-utf8", "S4: a raw 0xFF byte in a header string", CodeBadUTF8, sign(insert(rm.Header(), "\"x\":\"\xff\"")))
	rec("bad-utf8-lone-surrogate", `S4: \ud800 with no low surrogate`, CodeBadUTF8, sign(insert(rm.Header(), `"x":"\ud800"`)))

	rec("bad-json-duplicate-key", "S5: two room keys", CodeBadJSON, sign(insert(rm.Header(), `"room":"`+vRoom+`"`)))
	rec("bad-json-duplicate-after-unescape", "S5: room and r\\u006fom", CodeBadJSON, sign(insert(rm.Header(), `"r`+"\x5c"+`u006fom":"`+vRoom+`"`)))
	rec("bad-json-too-deep", "S5: an unknown field nested 17 deep (signed JSON nests at most 16)", CodeBadJSON,
		sign(insert(rm.Header(), `"later":`+strings.Repeat("[", maxDepth)+"1"+strings.Repeat("]", maxDepth))))
	rec("bad-json-trailing", "S5: a second object after the header", CodeBadJSON, sign(append(slices.Clone(rm.Header()), "{}"...)))

	rec("bad-field-null", "S6: parent null", CodeBadField, sign(edit(rm.Header(), `"parent":"`+msg.Parent+`"`, `"parent":null`)))
	rec("bad-field-false", "S6: ask_handled false", CodeBadField, signWire(keyA, edit(ra.Header(), `"ask_handled":true`, `"ask_handled":false`), ra.Body()))
	rec("bad-field-empty-array", "S6: to []", CodeBadField, signWire(keyA, insert(r1.Header(), `"to":[]`), r1.Body()))
	rec("bad-field-float", "S6: seq 2.0", CodeBadField, sign(edit(rm.Header(), `"seq":2,`, `"seq":2.0,`)))
	rec("bad-field-exponent", "S6: seq 2e0", CodeBadField, sign(edit(rm.Header(), `"seq":2,`, `"seq":2e0,`)))
	rec("bad-field-leading-zero", "S5, not S6: seq 02 is not RFC 8259 JSON", CodeBadJSON,
		sign(edit(rm.Header(), `"seq":2,`, `"seq":02,`)))
	rec("bad-field-too-big", "S6: seq 2^53", CodeBadField, sign(edit(rm.Header(), `"seq":2,`, `"seq":9007199254740992,`)))
	rec("bad-field-uppercase-hex", "S6: prev in upper case", CodeBadField, sign(edit(rm.Header(), r1.ID(), strings.ToUpper(r1.ID()))))
	rec("bad-field-time", "S6: created without milliseconds", CodeBadField, sign(edit(rm.Header(), `12:00:00.000Z`, `12:00:00Z`)))

	rec("bad-field-forbidden-ref-on-message", "S7: ref on a message", CodeBadField, sign(insert(rm.Header(), `"ref":"`+fakeID("x")+`"`)))
	rec("bad-field-missing-prev", "S7: seq 2 without prev", CodeBadField, sign(edit(rm.Header(), `"prev":"`+r1.ID()+`",`, ``)))
	links := fmt.Sprintf(`[{"epoch":%q,"seq":%d,"id":%q},{"epoch":%q,"seq":%d,"id":%q}]`,
		rfl.Fields().Links[1].Epoch, rfl.Fields().Links[1].Seq, rfl.Fields().Links[1].ID, rfl.Fields().Links[0].Epoch, rfl.Fields().Links[0].Seq, rfl.Fields().Links[0].ID)
	sortedLinks := fmt.Sprintf(`[{"epoch":%q,"seq":%d,"id":%q},{"epoch":%q,"seq":%d,"id":%q}]`,
		rfl.Fields().Links[0].Epoch, rfl.Fields().Links[0].Seq, rfl.Fields().Links[0].ID, rfl.Fields().Links[1].Epoch, rfl.Fields().Links[1].Seq, rfl.Fields().Links[1].ID)
	rec("bad-field-links-unsorted", "S7: links not sorted by id", CodeBadField, signWire(keyA, edit(rfl.Header(), sortedLinks, links), rfl.Body()))
	one := fmt.Sprintf(`{"node":%q,"agent":%q}`, rm.Fields().To[0].Node, rm.Fields().To[0].Agent)
	two := fmt.Sprintf(`{"node":%q,"agent":%q}`, rm.Fields().To[1].Node, rm.Fields().To[1].Agent)
	rec("bad-field-to-duplicate", "S7: the same addressee twice", CodeBadField,
		sign(edit(rm.Header(), `"to":[`+one+`,`+two+`]`, `"to":[`+one+`,`+one+`]`)))
	rec("bad-field-ask-with-to", "S7: ask_handled with to", CodeBadField, sign(insert(rm.Header(), `"ask_handled":true`)))

	rec("bad-body-handled-nonempty", "S8: handled with a body", CodeBadBody, wireOf(b64(rh.Sig()), b64(rh.Header()), b64([]byte(`{"text":"x"}`))))
	tampered := bytes.Replace(rm.Body(), []byte("left"), []byte("lefT"), 1)
	rec("bad-body-empty-b", "S9: a message with \"b\":\"\" (absent b means no text; present b must hold the body)", CodeBadBody,
		strings.TrimSuffix(string(rm.WithoutBody().Wire()), "}")+`,"b":""}`)
	rec("bad-body-hash", "S9: body changed, same length", CodeBadBody, wireOf(s, h, b64(tampered)))
	rec("bad-body-size", "S9: size one more than the body", CodeBadBody,
		sign(edit(rm.Header(), fmt.Sprintf(`"size":%d`, len(rm.Body())), fmt.Sprintf(`"size":%d`, len(rm.Body())+1))))
	blank := []byte(`{"text":" \t 　"}`)
	rec("bad-body-blank-text", "S9: text is only Unicode White_Space", CodeBadBody, signWire(keyA, messageHeader(r1.ID(), blank), blank))
	extra := []byte(`{"text":"hi","x":"y"}`)
	rec("bad-body-unknown-field", "S9: a field other than text and choices", CodeBadBody, signWire(keyA, messageHeader(r1.ID(), extra), extra))

	rec("bad-version", "V1: v 3", CodeBadVersion, sign(edit(rm.Header(), `"v":2,`, `"v":3,`)))
	add("wrong-room", "record", "R1: record-message arriving in a request for another room", CodeWrongRoom, string(rm.Wire()))
	out[len(out)-1].v.Room = "r_ffeeddccbbaa99887766554433221100"
	keyD := testKey("a2p2 test key D, not admitted")
	dm := msg
	dm.Prev = fakeID("d prev")
	rec("author-not-admitted", "A1: a valid record whose author is not admitted (admitted here: keys A, B, C)", CodeAuthorNotAdmitted,
		string(mustRecord(keyD, dm).Wire()))
	rec("bad-signature", "G1: record-message signed by another key", CodeBadSignature, signWire(keyB, rm.Header(), rm.Body()))

	for i := range out {
		if out[i].v.Type == "entry" {
			out[i].v.Create = ec.ID()
		}
	}
	return out
}

func encodeVector(v vector) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// TestGoldenVectors re-derives every vector from the fixed key and inputs,
// checks it gives its intended outcome, and compares it with the committed
// file byte for byte.
// Requirements: D8, I5, P4
func TestGoldenVectors(t *testing.T) {
	vs := buildVectors()
	if *update {
		if err := os.RemoveAll(vectorDir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(vectorDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	names := map[string]bool{}
	for _, nv := range vs {
		v := nv.v
		id, code, check := outcome(v)
		if code != v.Code || check != v.Check {
			t.Errorf("%s: outcome %q at %q, want %q at %q", nv.name, code, check, v.Code, v.Check)
			continue
		}
		v.ID = id
		got := encodeVector(v)
		path := filepath.Join(vectorDir, nv.name+".json")
		names[nv.name+".json"] = true
		if *update {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(want, got) {
			t.Errorf("%s differs from %s (run with -update after a deliberate change)", nv.name, path)
		}
	}
	files, _ := os.ReadDir(vectorDir)
	for _, f := range files {
		if !names[f.Name()] {
			t.Errorf("stale vector file %s", f.Name())
		}
	}
}

// TestVectorFilesGiveTheirStatedOutcome decodes the committed files alone,
// as another implementation would, without the generator.
// Requirements: D8, I5
func TestVectorFilesGiveTheirStatedOutcome(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(vectorDir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no vectors in %s", vectorDir)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var v vector
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if (v.ID == "") == (v.Code == "") {
			t.Errorf("%s: exactly one of id and code must be set", f)
		}
		if id, code, check := outcome(v); id != v.ID || code != v.Code || check != v.Check {
			t.Errorf("%s: got id %q code %q check %q, file says %q %q %q", f, id, code, check, v.ID, v.Code, v.Check)
		}
	}
}

// TestEveryReceiveCheckHasAFailingVector: each syntax check S1 to S9 and
// each stateless check V1, R1, A1, G1, C1 has at least one vector that fails
// at that check, as reported by the decoder, not only with its code.
// Requirements: P4
func TestEveryReceiveCheckHasAFailingVector(t *testing.T) {
	checks := map[string]bool{}
	for _, nv := range buildVectors() {
		if _, _, c := outcome(nv.v); c != "" && c == nv.v.Check {
			checks[c] = true
		}
	}
	for _, c := range []string{"S1", "S2", "S3", "S4", "S5", "S6", "S7", "S8", "S9", "V1", "R1", "A1", "G1", "C1"} {
		if !checks[c] {
			t.Errorf("no vector fails at %s", c)
		}
	}
}
