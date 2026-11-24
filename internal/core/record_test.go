package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func testMessage(t *testing.T) *Record {
	t.Helper()
	r, err := NewRecord(keyA, RecordFields{Room: vRoom, Epoch: vEpoch, Seq: 2, Prev: fakeID("p"),
		Kind: KindMessage, Agent: "claude", To: []Addressee{{nodeB, "codex"}},
		Parent: fakeID("parent"), Thread: fakeID("thread"), Created: vCreated,
		Text: "the secret plan", Choices: []string{"yes", "no"}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// accepted is the stateless part of receiving: parse, then signature.
func accepted(wire []byte) (*Record, error) {
	r, err := ParseRecord(wire)
	if err != nil {
		return nil, err
	}
	return r, r.VerifySignature()
}

// TestRelayCannotAlterAnySignedField flips bits at every header byte and in
// the signature; no altered copy is accepted, whichever check refuses it.
// Requirements: D8, T2
func TestRelayCannotAlterAnySignedField(t *testing.T) {
	r := testMessage(t)
	if _, err := accepted(r.Wire()); err != nil {
		t.Fatalf("original refused: %v", err)
	}
	for i := range r.Header() {
		for _, mask := range []byte{0x01, 0x02, 0x20, 0x80} {
			h := bytes.Clone(r.Header())
			h[i] ^= mask
			if _, err := accepted(encodeWire(h, r.Sig(), r.Body())); err == nil {
				t.Fatalf("header byte %d ^ %#x accepted", i, mask)
			}
		}
	}
	for i := range r.Sig() {
		s := bytes.Clone(r.Sig())
		s[i] ^= 0x01
		if _, err := accepted(encodeWire(r.Header(), s, r.Body())); RefusalCode(err) != CodeBadSignature {
			t.Fatalf("signature byte %d: got %v", i, err)
		}
	}
	// Re-encoding the same fields differently is also a different signed
	// message: whitespace added by a relay breaks the signature.
	spaced := bytes.Replace(r.Header(), []byte(`,"kind"`), []byte(`, "kind"`), 1)
	if _, err := accepted(encodeWire(spaced, r.Sig(), r.Body())); RefusalCode(err) != CodeBadSignature {
		t.Fatalf("re-encoded header: got %v", err)
	}
}

// TestRelayMayDropTextAndTheHeaderStillVerifies: the body is in the id only
// as a hash, so a header passed on without text keeps its id and signature.
// Requirements: D7, D8
func TestRelayMayDropTextAndTheHeaderStillVerifies(t *testing.T) {
	r := testMessage(t)
	got, err := accepted(r.WithoutBody().Wire())
	if err != nil {
		t.Fatal(err)
	}
	if got.ID() != r.ID() || got.Body() != nil || got.Fields().Text != "" || !bytes.Equal(got.Header(), r.Header()) {
		t.Fatalf("header-only copy differs: id %s vs %s", got.ID(), r.ID())
	}
	if r.Fields().Text != "the secret plan" || len(r.Fields().Choices) != 2 {
		t.Fatalf("body not parsed: %d choices", len(r.Fields().Choices))
	}
}

// TestBodyThatDoesNotMatchItsHashOrSizeIsRefused: any change to the body
// bytes, including length, is refused with bad_body.
// Requirements: D8
func TestBodyThatDoesNotMatchItsHashOrSizeIsRefused(t *testing.T) {
	r := testMessage(t)
	for i := range r.Body() {
		b := bytes.Clone(r.Body())
		b[i] ^= 0x01
		if _, err := ParseRecord(encodeWire(r.Header(), r.Sig(), b)); RefusalCode(err) != CodeBadBody {
			t.Fatalf("body byte %d: got %v", i, err)
		}
	}
	for _, b := range [][]byte{r.Body()[:len(r.Body())-1], append(bytes.Clone(r.Body()), ' ')} {
		if _, err := ParseRecord(encodeWire(r.Header(), r.Sig(), b)); RefusalCode(err) != CodeBadBody {
			t.Fatalf("body of %d bytes: got %v", len(b), err)
		}
	}
}

// TestDuplicateJSONKeysAreRefusedAtEveryDepth, including two "room" keys
// with different values, which a lenient decoder resolves silently and two
// receivers could resolve differently.
// Requirements: D8, T1
func TestDuplicateJSONKeysAreRefusedAtEveryDepth(t *testing.T) {
	r := testMessage(t)
	other := `"room":"r_ffeeddccbbaa99887766554433221100"`
	for name, h := range map[string][]byte{
		"room twice, other value first": insert(r.Header(), other),
		"room twice, other value last":  []byte(strings.TrimSuffix(string(r.Header()), "}") + "," + other + "}"),
		"inside to":                     bytes.Replace(r.Header(), []byte(`"agent":"codex"`), []byte(`"agent":"codex","agent":"codex"`), 1),
		"escaped key":                   insert(r.Header(), `"`+"\x5c"+`u0072oom":"`+vRoom+`"`),
	} {
		if _, err := ParseRecord([]byte(signWire(keyA, h, r.Body()))); RefusalCode(err) != CodeBadJSON {
			t.Errorf("%s: got %v", name, err)
		}
	}
	body := []byte(`{"text":"a","text":"b"}`)
	if _, err := ParseRecord([]byte(signWire(keyA, messageHeader(fakeID("p"), body), body))); RefusalCode(err) != CodeBadBody {
		t.Errorf("body: got %v", err)
	}
	h, s := b64(r.Header()), b64(r.Sig())
	if _, err := ParseRecord([]byte(`{"h":"` + h + `","s":"` + s + `","s":"` + s + `"}`)); RefusalCode(err) != CodeBadWire {
		t.Errorf("wire: got %v", err)
	}
}

// TestStrictJSON covers the section 0 rules the vectors do not.
// Requirements: D8
func TestStrictJSON(t *testing.T) {
	ok := []string{` {"a" : [1, "x", {"b": true}] } `, `{"a":"😀"}`, `{"a":"\u0000"}`, `{"A":1,"a":2}`, `"x"`, `-0.5e+3`}
	for _, s := range ok {
		if _, err := parseStrict([]byte(s)); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	bad := map[string]error{
		"\xef\xbb\xbf{}":        errJSONSyntax, // BOM
		`{"a":1,}`:              errJSONSyntax,
		`{"a":"x` + "\n" + `"}`: errJSONSyntax,
		`{'a':1}`:               errJSONSyntax,
		`{"a":01}`:              errJSONSyntax,
		`{"a":+1}`:              errJSONSyntax,
		`{"a":1.}`:              errJSONSyntax,
		`{"a":NaN}`:             errJSONSyntax,
		`{"a":"\x"}`:            errJSONSyntax,
		`{"a":tru}`:             errJSONSyntax,
		`{"a":1}}`:              errJSONSyntax,
		``:                      errJSONSyntax,
		`{"a":"\udc00"}`:        errJSONUTF8,
		`{"a":"\ud800A"}`:       errJSONUTF8,
		`{"a":"\ud800"}`:        errJSONUTF8,
		"{\"a\":\"\xc0\xaf\"}":  errJSONUTF8, // overlong
		`{"a":{"b":1,"b":2}}`:   errJSONSyntax,
	}
	for s, want := range bad {
		if _, err := parseStrict([]byte(s)); err != want {
			t.Errorf("%q: got %v, want %v", s, err, want)
		}
	}
	// An escaped backslash before "u" is not an escape: not a surrogate.
	if !validText([]byte(`{"a":"\\ud800"}`)) {
		t.Error(`\\ud800 taken for a surrogate`)
	}
}

// TestIntegersAndTimesHaveOneSpelling: section 0's Integer and Time formats.
// Requirements: D8
func TestIntegersAndTimesHaveOneSpelling(t *testing.T) {
	for s, want := range map[string]bool{"0": true, "1": true, "9007199254740991": true, "9007199254740992": false,
		"-1": false, "-0": false, "1.0": false, "1e0": false, "10000000000000000000": false} {
		if _, ok := (jvalue{kind: jNumber, str: s}).integer(); ok != want {
			t.Errorf("integer %s: %v", s, ok)
		}
	}
	for s, want := range map[string]bool{"2026-11-23T12:00:00.000Z": true, "2026-11-23T12:00:00Z": false,
		"2026-11-23T12:00:00.000+00:00": false, "2026-11-23T12:00:00.000z": false, "2026-02-30T12:00:00.000Z": false,
		"2026-11-23T24:00:00.000Z": false, "2026-12-31T23:59:60.000Z": false, "2026-11-23 12:00:00.000Z": false} {
		if _, ok := parseTime(s); ok != want {
			t.Errorf("time %s: %v", s, ok)
		}
	}
}

// TestBase64HasOneSpelling: padding required, pad bits zero, no whitespace.
// Requirements: D8
func TestBase64HasOneSpelling(t *testing.T) {
	for s, want := range map[string]bool{"": true, "AA==": true, "AAA=": true, "AAAA": true, "AB==": false, "AA": false,
		"AA=": false, "A===": false, "AA==AA==": false, "AA\n==": false, "AA\r\n==": false, " AA==": false, "AA-_": false} {
		if _, ok := decodeBase64([]byte(s)); ok != want {
			t.Errorf("%q: %v", s, ok)
		}
	}
}

// TestHeaderLimitDependsOnSeq: 4,096 bytes, or 8,192 for a stream's first
// record, checked before the header is decoded further.
// Requirements: P4, P5
func TestHeaderLimitDependsOnSeq(t *testing.T) {
	first, err := NewRecord(keyA, RecordFields{Room: vRoom, Epoch: vEpoch, Seq: 1, Kind: KindHandled,
		Ref: fakeID("r"), Agent: "claude", Created: vCreated})
	if err != nil {
		t.Fatal(err)
	}
	pad := func(h []byte, n int) []byte {
		return insert(h, `"pad":"`+strings.Repeat("a", n-len(h)-9)+`"`)
	}
	if _, err := ParseRecord([]byte(signWire(keyA, pad(first.Header(), maxFirstHeader), nil))); err != nil {
		t.Errorf("8,192-byte first header: %v", err)
	}
	if _, err := ParseRecord([]byte(signWire(keyA, pad(first.Header(), maxFirstHeader+1), nil))); RefusalCode(err) != CodeTooLarge {
		t.Errorf("8,193-byte first header: %v", err)
	}
	r := testMessage(t)
	if _, err := ParseRecord([]byte(signWire(keyA, pad(r.Header(), maxHeader), r.Body()))); err != nil {
		t.Errorf("4,096-byte header: %v", err)
	}
	if _, err := ParseRecord([]byte(signWire(keyA, pad(r.Header(), 5000), r.Body()))); RefusalCode(err) != CodeTooLarge {
		t.Errorf("5,000-byte header with seq 2: %v", err)
	}
}

// TestWriterRefusesTooManyHeads: a first record that cannot carry all its
// links is not written.
// Requirements: P4
func TestWriterRefusesTooManyHeads(t *testing.T) {
	links := make([]Link, maxLinks+1)
	for i := range links {
		links[i] = Link{Epoch: vEpoch, Seq: int64(i + 1), ID: fakeID(fmt.Sprint(i))}
	}
	f := RecordFields{Room: vRoom, Epoch: "1111111111111111", Seq: 1, Kind: KindMessage, Agent: "claude",
		Created: vCreated, Text: "hi", Links: links}
	if _, err := NewRecord(keyA, f); RefusalCode(err) != CodeTooManyHeads {
		t.Fatalf("101 links: %v", err)
	}
	f.Links = links[:maxLinks]
	if _, err := NewRecord(keyA, f); err != nil {
		t.Fatalf("100 links: %v", err)
	}
}

// TestRecordFieldsPerKindAndSeq: section 1's required and forbidden table,
// enforced on the writer path too.
// Requirements: D9, D10
func TestRecordFieldsPerKindAndSeq(t *testing.T) {
	base := RecordFields{Room: vRoom, Epoch: vEpoch, Seq: 2, Prev: fakeID("p"), Agent: "claude", Created: vCreated}
	many := make([]Addressee, maxTo+1)
	for i := range many {
		many[i] = Addressee{nodeB, fmt.Sprintf("a%d", i)}
	}
	bad := map[string]func(*RecordFields){
		"handled with to":         func(f *RecordFields) { f.Kind, f.Ref, f.To = KindHandled, fakeID("r"), []Addressee{{nodeB, "x"}} },
		"withdraw without ref":    func(f *RecordFields) { f.Kind = KindWithdraw },
		"replace without version": func(f *RecordFields) { f.Kind, f.Ref, f.Text = KindReplace, fakeID("r"), "t" },
		"replace with choices": func(f *RecordFields) {
			f.Kind, f.Ref, f.Version, f.Text, f.Choices = KindReplace, fakeID("r"), 1, "t", []string{"a"}
		},
		"thread without parent": func(f *RecordFields) { f.Kind, f.Text, f.Thread = KindMessage, "t", fakeID("t") },
		"parent without thread": func(f *RecordFields) { f.Kind, f.Text, f.Parent = KindMessage, "t", fakeID("t") },
		"message without text":  func(f *RecordFields) { f.Kind = KindMessage },
		"version on message":    func(f *RecordFields) { f.Kind, f.Text, f.Version = KindMessage, "t", 1 },
		"seq 1 with prev":       func(f *RecordFields) { f.Kind, f.Text, f.Seq = KindMessage, "t", 1 },
		"links after seq 1":     func(f *RecordFields) { f.Kind, f.Text, f.Links = KindMessage, "t", []Link{{vEpoch, 1, fakeID("l")}} },
		"33 addressees":         func(f *RecordFields) { f.Kind, f.Text, f.To = KindMessage, "t", many },
		"to self twice": func(f *RecordFields) {
			f.Kind, f.Text, f.To = KindMessage, "t", []Addressee{{nodeB, "x"}, {nodeB, "x"}}
		},
		"links same position": func(f *RecordFields) {
			f.Seq, f.Prev, f.Kind, f.Text, f.Links = 1, "", KindMessage, "t", []Link{{vEpoch, 1, fakeID("a")}, {vEpoch, 1, fakeID("b")}}
		},
		"unknown kind":    func(f *RecordFields) { f.Kind = "exec" },
		"all-zero epoch":  func(f *RecordFields) { f.Kind, f.Text, f.Epoch = KindMessage, "t", "0000000000000000" },
		"bad agent label": func(f *RecordFields) { f.Kind, f.Text, f.Agent = KindMessage, "t", "-x" },
		"nine choices": func(f *RecordFields) {
			f.Kind, f.Text, f.Choices = KindMessage, "t", strings.Fields("a b c d e f g h i")
		},
		"duplicate choices": func(f *RecordFields) { f.Kind, f.Text, f.Choices = KindMessage, "t", []string{"a", "a"} },
		"choice over 200 bytes": func(f *RecordFields) {
			f.Kind, f.Text, f.Choices = KindMessage, "t", []string{strings.Repeat("é", 101)}
		},
		"invalid utf-8 text":     func(f *RecordFields) { f.Kind, f.Text = KindMessage, "\xff" },
		"local room":             func(f *RecordFields) { f.Kind, f.Text, f.Room = KindMessage, "t", "local" },
		"ask_handled on handled": func(f *RecordFields) { f.Kind, f.Ref, f.AskHandled = KindHandled, fakeID("r"), true },
		"parent on replace": func(f *RecordFields) {
			f.Kind, f.Ref, f.Version, f.Text, f.Parent = KindReplace, fakeID("r"), 1, "t", fakeID("x")
		},
		"thread on handled":       func(f *RecordFields) { f.Kind, f.Ref, f.Thread = KindHandled, fakeID("r"), fakeID("x") },
		"version on withdraw":     func(f *RecordFields) { f.Kind, f.Ref, f.Version = KindWithdraw, fakeID("r"), 2 },
		"room label as agent":     func(f *RecordFields) { f.Kind, f.Text, f.Agent = KindMessage, "t", "has space" },
		"uppercase ref on handle": func(f *RecordFields) { f.Kind, f.Ref = KindHandled, strings.ToUpper(fakeID("r")) },
	}
	for name, mutate := range bad {
		f := base
		mutate(&f)
		if _, err := NewRecord(keyA, f); err == nil {
			t.Errorf("%s: accepted", name)
		} else if testing.Verbose() {
			t.Logf("%s: %v", name, err)
		}
	}
	good := base
	good.Kind, good.Text, good.Choices = KindMessage, "  padded text  ", []string{strings.Repeat("é", 100)}
	r, err := NewRecord(keyA, good)
	if err != nil || r.Fields().Text != good.Text {
		t.Fatalf("valid message refused or trimmed: %v", err)
	}
}

// TestUnknownFieldsOnlyAtTopLevel: an unknown top-level field may hold any
// JSON value but null and is kept in the signed bytes and ignored; the
// omission and Integer rules are for known fields; an unknown key inside
// to, links or a body is refused.
// Requirements: I5
func TestUnknownFieldsOnlyAtTopLevel(t *testing.T) {
	r := testMessage(t)
	for name, c := range map[string]struct {
		h    []byte
		code string
	}{
		"object":          {insert(r.Header(), `"later":{"x":[1,2]}`), ""},
		"empty object":    {insert(r.Header(), `"later":{}`), ""},
		"float":           {insert(r.Header(), `"later":1.5`), ""},
		"false nested":    {insert(r.Header(), `"later":{"x":false}`), ""},
		"zero and empty":  {insert(r.Header(), `"later":[0,"",[]]`), ""},
		"inside to":       {bytes.Replace(r.Header(), []byte(`"agent":"codex"`), []byte(`"agent":"codex","x":1`), 1), CodeBadField},
		"null at top":     {insert(r.Header(), `"later":null`), CodeBadField},
		"null nested":     {insert(r.Header(), `"later":{"x":[1,null]}`), CodeBadField},
		"known float":     {bytes.Replace(r.Header(), []byte(`"seq":2,`), []byte(`"seq":2.5,`), 1), CodeBadField},
		"duplicate inner": {insert(r.Header(), `"later":{"x":1,"x":2}`), CodeBadJSON},
	} {
		got, err := ParseRecord([]byte(signWire(keyA, c.h, r.Body())))
		if RefusalCode(err) != c.code {
			t.Errorf("%s: got %v, want %q", name, err, c.code)
		}
		if err == nil && (got.ID() != RecordID(c.h) || got.ID() == r.ID()) {
			t.Errorf("%s: unknown field not in the id", name)
		}
	}
	e, _ := NewEntry(keyA, EntryFields{Room: vRoom, Seq: 1, Kind: EntryCreate, Label: "x", Created: vCreated})
	for h, code := range map[string]string{
		string(insert(e.Header(), `"later":{"a":false}`)): "",
		string(insert(e.Header(), `"later":[null]`)):      CodeBadField,
	} {
		if _, err := ParseEntry([]byte(signWire(keyA, []byte(h), nil))); RefusalCode(err) != code {
			t.Errorf("entry unknown field: got %v, want %q", err, code)
		}
	}
	first, _ := NewRecord(keyA, RecordFields{Room: vRoom, Epoch: vEpoch, Seq: 1, Kind: KindHandled, Ref: fakeID("r"),
		Agent: "claude", Created: vCreated, Links: []Link{{"1111111111111111", 1, fakeID("l")}}})
	h := bytes.Replace(first.Header(), []byte(`"seq":1,"id"`), []byte(`"seq":1,"x":1,"id"`), 1)
	if _, err := ParseRecord([]byte(signWire(keyA, h, nil))); RefusalCode(err) != CodeBadField {
		t.Errorf("inside links: %v", err)
	}
}

// TestRecordAndEntryCannotBeTakenForEachOther: ids are domain separated and
// the kinds are disjoint, so signed bytes of one type never parse as the
// other.
// Requirements: D8, T3
func TestRecordAndEntryCannotBeTakenForEachOther(t *testing.T) {
	e, err := NewEntry(keyA, EntryFields{Room: vRoom, Seq: 1, Kind: EntryCreate, Label: "x", Created: vCreated})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRecord(e.Wire()); RefusalCode(err) != CodeBadField {
		t.Errorf("entry as record: %v", err)
	}
	h, _ := NewRecord(keyA, RecordFields{Room: vRoom, Epoch: vEpoch, Seq: 1, Kind: KindHandled, Ref: fakeID("r"), Agent: "claude", Created: vCreated})
	if _, err := ParseEntry(h.Wire()); RefusalCode(err) != CodeBadField {
		t.Errorf("record as entry: %v", err)
	}
	if RecordID(e.Header()) == EntryID(e.Header()) {
		t.Error("record and entry ids of the same bytes are equal")
	}
	sum := sha256.Sum256(append([]byte("a2p2 entry v2\x00"), e.Header()...))
	if e.ID() != hex.EncodeToString(sum[:16]) {
		t.Errorf("entry id %s is not the section 0 formula", e.ID())
	}
	sum = sha256.Sum256(append([]byte("a2p2 record v2\x00"), h.Header()...))
	if h.ID() != hex.EncodeToString(sum[:16]) {
		t.Errorf("record id %s is not the section 0 formula", h.ID())
	}
}

// TestEntryFieldsPerKind: section 3's table, including parents containing
// prev, and from allowed with or without history.
// Requirements: T3, T5, U3
func TestEntryFieldsPerKind(t *testing.T) {
	base := EntryFields{Room: vRoom, Seq: 2, Prev: fakeID("prev"), Parents: []string{fakeID("prev")}, Target: nodeB, Created: vCreated}
	heads := make([]string, maxFromHeads+1)
	for i := range heads {
		heads[i] = fakeID(fmt.Sprint(i))
	}
	bad := map[string]func(*EntryFields){
		"create at seq 2":    func(f *EntryFields) { f.Kind, f.Label, f.Parents, f.Target = EntryCreate, "x", nil, "" },
		"create with target": func(f *EntryFields) { f.Kind, f.Label, f.Seq, f.Prev, f.Parents = EntryCreate, "x", 1, "", nil },
		"create label too long": func(f *EntryFields) {
			*f = EntryFields{Room: vRoom, Seq: 1, Kind: EntryCreate, Label: strings.Repeat("x", 65), Created: vCreated}
		},
		"add without name":        func(f *EntryFields) { f.Kind = EntryAdd },
		"add without parents":     func(f *EntryFields) { f.Kind, f.Name, f.Parents = EntryAdd, "n", nil },
		"add with label":          func(f *EntryFields) { f.Kind, f.Name, f.Label = EntryAdd, "n", "x" },
		"prev not in parents":     func(f *EntryFields) { f.Kind, f.Name, f.Parents = EntryAdd, "n", []string{fakeID("other")} },
		"seq 2 without prev":      func(f *EntryFields) { f.Kind, f.Name, f.Prev = EntryAdd, "n", "" },
		"remove with name":        func(f *EntryFields) { f.Kind, f.Name = EntryRemove, "n" },
		"remove with history":     func(f *EntryFields) { f.Kind, f.History = EntryRemove, true },
		"admin without target":    func(f *EntryFields) { f.Kind, f.Target = EntryAdmin, "" },
		"17 heads for one author": func(f *EntryFields) { f.Kind, f.Name, f.From = EntryAdd, "n", map[string][]string{nodeA: heads} },
		"from key not a node":     func(f *EntryFields) { f.Kind, f.Name, f.From = EntryAdd, "n", map[string][]string{"x": {fakeID("h")}} },
		"duplicate parents": func(f *EntryFields) {
			f.Kind, f.Name, f.Parents = EntryAdd, "n", []string{fakeID("prev"), fakeID("prev")}
		},
		"unknown kind":   func(f *EntryFields) { f.Kind = "promote" },
		"65 parents":     func(f *EntryFields) { f.Kind, f.Name, f.Parents = EntryAdd, "n", manyIDs(65, fakeID("prev")) },
		"bad name label": func(f *EntryFields) { f.Kind, f.Name = EntryAdd, "a b" },
	}
	for name, mutate := range bad {
		f := base
		mutate(&f)
		if _, err := NewEntry(keyA, f); RefusalCode(err) != CodeBadField {
			t.Errorf("%s: got %v", name, err)
		}
	}
	for name, mutate := range map[string]func(*EntryFields){
		"admin with only target": func(f *EntryFields) { f.Kind = EntryAdmin },
		"add with history and from": func(f *EntryFields) {
			f.Kind, f.Name, f.History, f.From = EntryAdd, "n", true, map[string][]string{nodeA: {fakeID("h")}}
		},
		"admin with history and from": func(f *EntryFields) {
			f.Kind, f.History, f.From = EntryAdmin, true, map[string][]string{nodeA: {fakeID("h")}}
		},
		"admin with name and from": func(f *EntryFields) {
			f.Kind, f.Name, f.From = EntryAdmin, "n", map[string][]string{nodeA: {fakeID("h")}}
		},
		"add at seq 1": func(f *EntryFields) { f.Kind, f.Name, f.Seq, f.Prev = EntryAdd, "n", 1, "" },
		"remove":       func(f *EntryFields) { f.Kind = EntryRemove },
	} {
		f := base
		mutate(&f)
		if _, err := NewEntry(keyA, f); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Parents out of order on the wire (NewEntry sorts, so write it by hand).
	h := fmt.Appendf(nil, `{"v":2,"room":%q,"author":%q,"seq":1,"parents":[%q,%q],"kind":"remove","target":%q,"created":"2026-11-23T12:00:00.000Z"}`,
		vRoom, nodeA, "ff"+fakeID("a")[2:], "00"+fakeID("b")[2:], nodeB)
	if _, err := ParseEntry([]byte(signWire(keyA, h, nil))); RefusalCode(err) != CodeBadField {
		t.Errorf("unsorted parents: %v", err)
	}
	if _, err := ParseEntry([]byte(strings.TrimSuffix(signWire(keyA, h, nil), "}") + `,"b":"AA=="}`)); RefusalCode(err) != CodeBadWire {
		t.Errorf("entry wire with b: %v", err)
	}
}

// manyIDs returns n distinct ids, unsorted, that include must.
func manyIDs(n int, must string) []string {
	out := []string{must}
	for i := 1; i < n; i++ {
		out = append(out, fakeID(fmt.Sprint("many", i)))
	}
	return out
}

// TestRefusalsNeverContainBodiesOrValues: refusal text names a field or a
// reason, never a body or a value a peer chose.
// Requirements: T1, T8
func TestRefusalsNeverContainBodiesOrValues(t *testing.T) {
	r := testMessage(t)
	cases := [][]byte{
		[]byte(signWire(keyA, messageHeader(fakeID("p"), []byte(`{"text":"secret","secretkey":"secret"}`)), []byte(`{"text":"secret","secretkey":"secret"}`))),
		[]byte(signWire(keyA, insert(r.Header(), `"secretfield":null`), r.Body())),
		[]byte(signWire(keyA, bytes.Replace(r.Header(), []byte(`"agent":"claude"`), []byte(`"agent":"secret agent"`), 1), r.Body())),
		encodeWire(r.Header(), r.Sig(), bytes.Replace(r.Body(), []byte("secret"), []byte("Secret"), 1)),
		[]byte(`{"h":"secret","s":"x"}`),
		[]byte(`{"secret":"x"}`),
		[]byte(`{"secret":0}`),
		[]byte(signWire(keyA, insert(r.Header(), `"secret":{"a":null}`), r.Body())),
	}
	for i, w := range cases {
		_, err := accepted(w)
		if err == nil {
			t.Fatalf("case %d accepted", i)
		}
		if strings.Contains(strings.ToLower(err.Error()), "secret") {
			t.Errorf("case %d: refusal %q leaks a value", i, err)
		}
	}
}

// TestWhitespaceAroundTheWireObjectIsAccepted: section 0 allows
// insignificant whitespace, and the size of the wire object is bounded by
// the request it came in, not by core. Whitespace around a broken object
// still gives S1.
// Requirements: D2, I5
func TestWhitespaceAroundTheWireObjectIsAccepted(t *testing.T) {
	r := testMessage(t)
	pad := strings.Repeat(" ", 33793)
	if _, err := accepted([]byte(pad + string(r.Wire()) + "\n\t")); err != nil {
		t.Errorf("padded wire: %v", err)
	}
	inner := strings.Replace(string(r.Wire()), `","s":"`, `" , "s" : "`, 1)
	if _, err := accepted([]byte(inner)); err != nil {
		t.Errorf("spaced wire: %v", err)
	}
	if _, err := ParseRecord([]byte(pad + "{")); RefusalCode(err) != CodeBadWire {
		t.Errorf("padded broken wire: %v", err)
	}
}

// TestEmptyBodyKeyFollowsS8: "b":"" is allowed where no body may be sent
// (handled, withdraw) and refused for a message or replace, which then must
// carry a body.
// Requirements: D9
func TestEmptyBodyKeyFollowsS8(t *testing.T) {
	h, err := NewRecord(keyA, RecordFields{Room: vRoom, Epoch: vEpoch, Seq: 1, Kind: KindHandled, Ref: fakeID("r"), Agent: "claude", Created: vCreated})
	if err != nil {
		t.Fatal(err)
	}
	withB := func(w []byte) []byte { return []byte(strings.TrimSuffix(string(w), "}") + `,"b":""}`) }
	got, err := accepted(withB(h.Wire()))
	if err != nil || got.ID() != h.ID() || got.Body() != nil {
		t.Errorf("handled with empty b: %v", err)
	}
	r := testMessage(t)
	_, err = ParseRecord(withB(r.WithoutBody().Wire()))
	var ref *Refusal
	if !errors.As(err, &ref) || ref.Code != CodeBadBody || ref.Check != "S9" {
		t.Errorf("message with empty b: %v", err)
	}
}

// TestNothingLargeIsAllocatedBeforeTheSizeChecks: a 24 KB signature or a
// deeply nested wire value is refused without decoding or building it, and
// signed JSON nests at most 16 deep.
// Requirements: P5
func TestNothingLargeIsAllocatedBeforeTheSizeChecks(t *testing.T) {
	for name, w := range map[string][]byte{
		"big signature": []byte(`{"h":"W10=","s":"` + strings.Repeat("A", 32768) + `"}`),
		"deep wire":     []byte(`{"h":` + strings.Repeat("[", 16000) + "0" + strings.Repeat("]", 16000) + "}"),
		"big body":      []byte(`{"h":"W10=","s":"` + strings.Repeat("A", 88) + `","b":"` + strings.Repeat("A", 32768) + `"}`),
	} {
		var err error
		allocs := allocBytes(func() { _, err = ParseRecord(w) })
		if err == nil || allocs > 4096 {
			t.Errorf("%s: %v after %d bytes allocated", name, err, allocs)
		}
	}
	r := testMessage(t)
	nest := func(n int) []byte {
		return insert(r.Header(), `"later":`+strings.Repeat("[", n-1)+"1"+strings.Repeat("]", n-1))
	}
	if _, err := ParseRecord([]byte(signWire(keyA, nest(maxDepth), r.Body()))); err != nil {
		t.Errorf("depth %d: %v", maxDepth, err)
	}
	if _, err := ParseRecord([]byte(signWire(keyA, nest(maxDepth+1), r.Body()))); RefusalCode(err) != CodeBadJSON {
		t.Errorf("depth %d: %v", maxDepth+1, err)
	}
}

func allocBytes(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// TestVerifiedFieldsCannotBeChanged: what was checked and signed cannot be
// altered through the values a parsed record or entry hands out.
// Requirements: D8, T1
func TestVerifiedFieldsCannotBeChanged(t *testing.T) {
	r := testMessage(t)
	f := r.Fields()
	f.Room, f.To[0].Node, f.Choices[0] = "r_ffeeddccbbaa99887766554433221100", nodeC, "changed"
	r.Header()[10] ^= 1
	r.Body()[0] ^= 1
	g := r.Fields()
	if g.Room != vRoom || g.To[0].Node != nodeB || g.Choices[0] != "yes" || r.VerifySignature() != nil {
		t.Fatal("parsed record changed through a returned value")
	}
	if _, err := RecordFromParts(r.Header(), r.Sig(), r.Body()); err != nil {
		t.Fatal(err)
	}
	e, err := NewEntry(keyA, EntryFields{Room: vRoom, Seq: 1, Parents: []string{fakeID("p")}, Kind: EntryAdd,
		Target: nodeB, Name: "n", From: map[string][]string{nodeA: {fakeID("h")}}, Created: vCreated})
	if err != nil {
		t.Fatal(err)
	}
	ef := e.Fields()
	ef.From[nodeA][0], ef.Parents[0] = "x", "y"
	ef.From[nodeC] = nil
	if g := e.Fields(); g.From[nodeA][0] != fakeID("h") || g.Parents[0] != fakeID("p") || len(g.From) != 1 {
		t.Fatal("parsed entry changed through a returned value")
	}
}

// TestStoredPartsGiveTheSameRecord: reloading header, signature and body
// from storage runs the same checks and gives the same record.
// Requirements: D1, D8
func TestStoredPartsGiveTheSameRecord(t *testing.T) {
	r := testMessage(t)
	got, err := RecordFromParts(r.Header(), r.Sig(), r.Body())
	if err != nil || got.ID() != r.ID() || !bytes.Equal(got.Wire(), r.Wire()) {
		t.Fatalf("from parts: %v", err)
	}
	bare, err := RecordFromParts(r.Header(), r.Sig(), nil)
	if err != nil || bare.ID() != r.ID() || bare.Body() != nil {
		t.Fatalf("from parts without body: %v", err)
	}
	if _, err := RecordFromParts(r.Header(), r.Sig(), []byte(`{"text":"other"}`)); RefusalCode(err) != CodeBadBody {
		t.Fatalf("stored body that does not match: %v", err)
	}
	if _, err := RecordFromParts(r.Header(), r.Sig()[:63], r.Body()); RefusalCode(err) != CodeTooLarge {
		t.Fatalf("short stored signature: %v", err)
	}
	e, _ := NewEntry(keyA, EntryFields{Room: vRoom, Seq: 1, Kind: EntryCreate, Label: "x", Created: vCreated})
	if got, err := EntryFromParts(e.Header(), e.Sig()); err != nil || got.ID() != e.ID() {
		t.Fatalf("entry from parts: %v", err)
	}
}

// TestWritersRefuseRatherThanDrop: counts and byte limits both apply, and a
// writer over either is told which; nothing is dropped to fit.
// Requirements: P4, T3
func TestWritersRefuseRatherThanDrop(t *testing.T) {
	to := make([]Addressee, maxTo)
	for i := range to {
		to[i] = Addressee{nodeOf(testKey(fmt.Sprint(i))), strings.Repeat("a", 63) + fmt.Sprint(i%10)}
	}
	f := RecordFields{Room: vRoom, Epoch: vEpoch, Seq: 2, Prev: fakeID("p"), Kind: KindMessage, Agent: "claude", To: to, Created: vCreated, Text: "x"}
	if _, err := NewRecord(keyA, f); err == nil || err.Error() != "too_large: header size" {
		t.Errorf("32 long addressees: %v", err)
	}
	f.To = append(to, Addressee{nodeA, "x"})
	if _, err := NewRecord(keyA, f); err == nil || err.Error() != "bad_field: to" {
		t.Errorf("33 addressees: %v", err)
	}
	links := make([]Link, maxLinks)
	for i := range links {
		links[i] = Link{Epoch: vEpoch, Seq: maxInt - int64(i), ID: fakeID(fmt.Sprint(i))}
	}
	lf := RecordFields{Room: vRoom, Epoch: "1111111111111111", Seq: 1, Kind: KindMessage, Agent: "claude", Created: vCreated, Text: "x", Links: links}
	if _, err := NewRecord(keyA, lf); err == nil || err.Error() != "too_many_heads: header size" {
		t.Errorf("100 long links: %v", err)
	}
	from := map[string][]string{}
	for i := 0; i < maxFromAuthors; i++ {
		var ids []string
		for j := 0; j < maxFromHeads; j++ {
			ids = append(ids, fakeID(fmt.Sprint(i, j)))
		}
		from[nodeOf(testKey(fmt.Sprint(i)))] = ids
	}
	ef := EntryFields{Room: vRoom, Seq: 1, Parents: []string{fakeID("x")}, Kind: EntryAdd, Target: nodeB, Name: "n", From: from, Created: vCreated}
	if _, err := NewEntry(keyA, ef); err == nil || err.Error() != "too_large: header size" {
		t.Errorf("64 x 16 heads: %v", err)
	}
	from[nodeA] = append(from[nodeOf(testKey("0"))], fakeID("one more"))
	if _, err := NewEntry(keyA, ef); err == nil || err.Error() != "bad_field: from" {
		t.Errorf("65 authors / 17 heads: %v", err)
	}
}
