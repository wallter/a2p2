package core

import (
	"bytes"
	"testing"
)

// TestVerifiedTypesNeedAGoodSignature: a record or entry whose same-length
// signature has one corrupt byte parses but cannot become verified.
// Requirements: D8, T3
func TestVerifiedTypesNeedAGoodSignature(t *testing.T) {
	r := testMessage(t)
	if v, err := r.Verified(); err != nil || v.ID() != r.ID() || v.Author() != nodeA {
		t.Fatalf("good record: %v", err)
	}
	sig := r.Sig()
	sig[len(sig)-1] ^= 0x01
	bad, err := RecordFromParts(r.Header(), sig, r.Body())
	if err != nil {
		t.Fatalf("corrupt signature should still parse: %v", err)
	}
	if v, err := bad.Verified(); v != nil || RefusalCode(err) != CodeBadSignature {
		t.Fatalf("record with corrupt signature verified: %v", err)
	}

	e, err := NewEntry(keyA, EntryFields{Room: vRoom, Seq: 1, Kind: EntryCreate, Label: "x", Created: vCreated})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := e.Verified(); err != nil || v.ID() != e.ID() {
		t.Fatalf("good entry: %v", err)
	}
	esig := e.Sig()
	esig[0] ^= 0x80
	badE, err := EntryFromParts(e.Header(), esig)
	if err != nil {
		t.Fatalf("corrupt signature should still parse: %v", err)
	}
	if v, err := badE.Verified(); v != nil || RefusalCode(err) != CodeBadSignature {
		t.Fatalf("entry with corrupt signature verified: %v", err)
	}
}

// TestZeroVerifiedValuesHoldNothing: a zero or nil VerifiedRecord or
// VerifiedEntry gives no id, author or bytes that could pass for real ones.
// Requirements: D8, T1
func TestZeroVerifiedValuesHoldNothing(t *testing.T) {
	for _, v := range []*VerifiedRecord{{}, nil, (&VerifiedRecord{}).WithoutBody()} {
		f := v.Fields()
		if v.ID() != "" || v.Author() != "" || v.BodyHash() != "" || v.Size() != 0 || v.Header() != nil ||
			v.Sig() != nil || v.Body() != nil || v.Wire() != nil || f.Room != "" || f.Kind != "" {
			t.Errorf("zero VerifiedRecord yields data")
		}
	}
	for _, v := range []*VerifiedEntry{{}, nil} {
		if v.ID() != "" || v.Author() != "" || v.Header() != nil || v.Sig() != nil || v.Wire() != nil || v.Fields().Room != "" {
			t.Errorf("zero VerifiedEntry yields data")
		}
	}
}

// TestVerifiedAccessorsReturnCopies: changing what a verified value hands
// out does not change the value, which still verifies.
// Requirements: D8
func TestVerifiedAccessorsReturnCopies(t *testing.T) {
	r := testMessage(t)
	v, err := r.Verified()
	if err != nil {
		t.Fatal(err)
	}
	v.Header()[0] ^= 1
	v.Sig()[0] ^= 1
	v.Body()[0] ^= 1
	f := v.Fields()
	f.To[0].Node, f.Choices[0] = nodeC, "changed"
	if g := v.Fields(); g.To[0].Node != nodeB || g.Choices[0] != "yes" {
		t.Fatal("verified record fields changed through a returned value")
	}
	if !bytes.Equal(v.Wire(), r.Wire()) || r.VerifySignature() != nil {
		t.Fatal("verified record bytes changed through a returned value")
	}
	if bare := v.WithoutBody(); bare.ID() != v.ID() || bare.Body() != nil || v.Body() == nil {
		t.Fatal("WithoutBody changed the original or the id")
	}

	e, err := NewEntry(keyA, EntryFields{Room: vRoom, Seq: 1, Parents: []string{fakeID("p")}, Kind: EntryAdd,
		Target: nodeB, Name: "n", From: map[string][]string{nodeA: {fakeID("h")}}, Created: vCreated})
	if err != nil {
		t.Fatal(err)
	}
	ve, err := e.Verified()
	if err != nil {
		t.Fatal(err)
	}
	ef := ve.Fields()
	ef.From[nodeA][0], ef.Parents[0] = "x", "y"
	ve.Header()[0] ^= 1
	ve.Sig()[0] ^= 1
	if g := ve.Fields(); g.From[nodeA][0] != fakeID("h") || g.Parents[0] != fakeID("p") || !bytes.Equal(ve.Wire(), e.Wire()) {
		t.Fatal("verified entry changed through a returned value")
	}
}
