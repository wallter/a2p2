package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"unicode/utf8"
)

// Each fuzz case feeds three inputs: a raw wire object; a header (plus body)
// signed with the test key, so mutations reach the checks behind the
// signature; and fields for NewRecord or NewEntry, whose output must always
// be accepted and verify.
//
//	go test ./internal/core -run '^$' -fuzz '^FuzzParseRecord$' -fuzztime 60s

func vectorSeeds(typ string) (wires, headers, bodies [][]byte) {
	for _, nv := range buildVectors() {
		if nv.v.Type != typ {
			continue
		}
		var w struct{ H, S, B string }
		_ = json.Unmarshal([]byte(nv.v.Wire), &w)
		h, _ := decodeBase64([]byte(w.H))
		b, _ := decodeBase64([]byte(w.B))
		wires, headers, bodies = append(wires, []byte(nv.v.Wire)), append(headers, h), append(bodies, b)
	}
	return
}

// independentParts decodes a wire object with encoding/json and
// encoding/base64, not with the code under test.
func independentParts(t *testing.T, wire []byte) (h, s, b []byte) {
	var m map[string]string
	if err := json.Unmarshal(wire, &m); err != nil {
		t.Fatalf("accepted wire object is not JSON to encoding/json: %v", err)
	}
	dec := func(k string) []byte {
		out, err := base64.StdEncoding.DecodeString(m[k])
		if err != nil {
			t.Fatalf("accepted %s is not base64 to encoding/base64: %v", k, err)
		}
		return out
	}
	return dec("h"), dec("s"), dec("b")
}

func independentID(domain string, h []byte) string {
	sum := sha256.Sum256(append(append([]byte(domain), 0), h...))
	return hex.EncodeToString(sum[:16])
}

// FuzzParseRecord: no input panics; whatever is accepted holds exactly the
// input's parts, has the id of section 0, round-trips unchanged and
// verifies when signed by its author; whatever NewRecord makes verifies.
// Requirements: D8, D3
func FuzzParseRecord(f *testing.F) {
	w, h, b := vectorSeeds("record")
	for i := range w {
		f.Add(w[i], h[i], b[i], "text", uint16(i), uint8(i))
	}
	f.Fuzz(func(t *testing.T, wire, header, body []byte, text string, seq uint16, kind uint8) {
		checkRecord(t, wire, false)
		checkRecord(t, []byte(signWire(keyA, header, body)), true)

		if !utf8.ValidString(text) || len(text) > 2000 {
			return
		}
		fields := RecordFields{Room: vRoom, Epoch: vEpoch, Seq: int64(seq) + 1, Agent: "claude", Created: vCreated}
		if fields.Seq > 1 {
			fields.Prev = fakeID(text)
		}
		switch kind % 4 {
		case 0:
			fields.Kind, fields.Text = KindMessage, "t"+text
		case 1:
			fields.Kind, fields.Ref = KindHandled, fakeID(text)
		case 2:
			fields.Kind, fields.Ref = KindWithdraw, fakeID(text)
		case 3:
			fields.Kind, fields.Ref, fields.Version, fields.Text = KindReplace, fakeID(text), int64(kind)+1, text+"t"
		}
		r, err := NewRecord(keyA, fields)
		if err != nil {
			t.Fatalf("NewRecord refused well-formed fields: %v", err)
		}
		if err := r.VerifySignature(); err != nil {
			t.Fatalf("NewRecord output does not verify: %v", err)
		}
		checkRecord(t, r.Wire(), true)
	})
}

func checkRecord(t *testing.T, wire []byte, signedByA bool) {
	r, err := ParseRecord(wire)
	if err != nil {
		var ref *Refusal
		if !errors.As(err, &ref) || ref.Code == "" || ref.Check == "" {
			t.Fatalf("error without a code and check: %v", err)
		}
		return
	}
	h, s, b := independentParts(t, wire)
	if !bytes.Equal(r.Header(), h) || !bytes.Equal(r.Sig(), s) || !bytes.Equal(r.Body(), b) {
		t.Fatal("accepted parts differ from the input's")
	}
	if r.ID() != independentID("a2p2 record v2", h) {
		t.Fatal("id is not the section 0 formula")
	}
	again, err := ParseRecord(r.Wire())
	if err != nil {
		t.Fatalf("accepted record refused after a round trip: %v", err)
	}
	if again.ID() != r.ID() || !bytes.Equal(again.Wire(), r.Wire()) {
		t.Fatal("round trip changed the record")
	}
	if signedByA && r.Author() == nodeA {
		if err := r.VerifySignature(); err != nil {
			t.Fatalf("record signed by its author does not verify: %v", err)
		}
	}
	if hdr, err := ParseRecord(r.WithoutBody().Wire()); err != nil || hdr.ID() != r.ID() {
		t.Fatalf("header without its body refused or re-identified: %v", err)
	}
}

// FuzzParseEntry: the same properties for log entries.
// Requirements: T3, D3
func FuzzParseEntry(f *testing.F) {
	w, h, _ := vectorSeeds("entry")
	for i := range w {
		f.Add(w[i], h[i], "label", uint16(i), uint8(i))
	}
	f.Fuzz(func(t *testing.T, wire, header []byte, label string, seq uint16, kind uint8) {
		checkEntry(t, wire, false)
		checkEntry(t, []byte(signWire(keyA, header, nil)), true)

		if !utf8.ValidString(label) || len(label) == 0 || len(label) > maxRoomLabel {
			return
		}
		fields := EntryFields{Room: vRoom, Seq: int64(seq) + 1, Target: nodeB, Parents: []string{fakeID(label)}, Created: vCreated}
		if fields.Seq > 1 {
			fields.Prev = fields.Parents[0]
		}
		switch kind % 4 {
		case 0:
			fields = EntryFields{Room: vRoom, Seq: 1, Kind: EntryCreate, Label: label, Created: vCreated}
		case 1:
			fields.Kind, fields.Name, fields.From = EntryAdd, "n", map[string][]string{nodeA: {fakeID(label + "h")}}
		case 2:
			fields.Kind = EntryRemove
		case 3:
			fields.Kind, fields.History = EntryAdmin, true
		}
		e, err := NewEntry(keyA, fields)
		if err != nil {
			t.Fatalf("NewEntry refused well-formed fields: %v", err)
		}
		if err := e.VerifySignature(); err != nil {
			t.Fatalf("NewEntry output does not verify: %v", err)
		}
		checkEntry(t, e.Wire(), true)
	})
}

func checkEntry(t *testing.T, wire []byte, signedByA bool) {
	e, err := ParseEntry(wire)
	if err != nil {
		var ref *Refusal
		if !errors.As(err, &ref) || ref.Code == "" || ref.Check == "" {
			t.Fatalf("error without a code and check: %v", err)
		}
		return
	}
	h, s, _ := independentParts(t, wire)
	if !bytes.Equal(e.Header(), h) || !bytes.Equal(e.Sig(), s) {
		t.Fatal("accepted parts differ from the input's")
	}
	if e.ID() != independentID("a2p2 entry v2", h) {
		t.Fatal("id is not the section 0 formula")
	}
	again, err := ParseEntry(e.Wire())
	if err != nil {
		t.Fatalf("accepted entry refused after a round trip: %v", err)
	}
	if again.ID() != e.ID() || !bytes.Equal(again.Wire(), e.Wire()) {
		t.Fatal("round trip changed the entry")
	}
	if signedByA && e.Author() == nodeA {
		if err := e.VerifySignature(); err != nil {
			t.Fatalf("entry signed by its author does not verify: %v", err)
		}
	}
}
