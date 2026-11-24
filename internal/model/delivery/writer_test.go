package delivery

import (
	"errors"
	"testing"
)

// Corrections found by comparing the implementation with the model.

// Requirements: D9
func TestReplaceAfterTheWinningReplacementExpiredNeverReusesItsVersion(t *testing.T) {
	w := room("A", "B")
	m := send(t, w, "A", "B", "v0")
	pull(t, w, "B", "A")
	read(t, w, "B")
	w.SetSkew("A", -20*Day)
	r1 := must(w.Replace("A", "a", m, "R1")) // dated 20 days back: expires first
	w.SetSkew("A", 0)
	pull(t, w, "B", "A")
	wantTexts(t, read(t, w, "B"), "replaced:R1")
	w.Advance(11 * Day)
	if h := w.Machine("A").get(r1); h.dropped != Expired {
		t.Fatalf("R1 is %v on A, want expired", h.dropped)
	}
	r2 := must(w.Replace("A", "a", m, "R2"))
	v1, v2 := w.Machine("A").get(r1).rec.Version, w.Machine("A").get(r2).rec.Version
	if v2 <= v1 {
		t.Fatalf("new replace has version %d, the expired one %d: a version was reused", v2, v1)
	}
	pull(t, w, "B", "A")
	wantTexts(t, read(t, w, "B"), "replaced:R2")
}

// Requirements: D9, D8
func TestWithdrawNamingAReplaceRatherThanAMessageHasNoEffect(t *testing.T) {
	w := room("A", "B")
	m := send(t, w, "A", "B", "v0")
	r1 := must(w.Replace("A", "a", m, "R1"))
	if _, err := w.Withdraw("A", "a", r1); !errors.Is(err, ErrNotAllow) {
		t.Fatalf("writer allowed a withdraw of a replace: %v", err)
	}
	// Another implementation writes one anyway: it is stored and passed on.
	x := w.write(w.Machine("A"), &Record{Kind: Withdraw, Agent: "a", Ref: r1}, "")
	pull(t, w, "B", "A")
	for _, id := range []RecordID{m, r1} {
		for _, n := range []MachineID{"A", "B"} {
			if !holdsText(w, n, id) {
				t.Fatalf("%s lost the text of %s", n, id)
			}
		}
	}
	if w.Machine("B").get(x.ID) == nil {
		t.Fatal("B did not store the withdraw")
	}
	wantTexts(t, read(t, w, "B"), "shown:R1")
}

// Requirements: D5
func TestRoomWidePostAskingForHandledCannotNameAddressees(t *testing.T) {
	w := room("A", "B")
	_, err := w.Send("A", "a", Msg{Body: "x", Everyone: true, AskHandled: true, To: []Addr{at("B")}})
	if !errors.Is(err, ErrNotAllow) {
		t.Fatalf("writer allowed a room-wide post with addressees: %v", err)
	}
}

// Requirements: D9
func TestMessageOrReplaceWithEmptyTextIsRefusedAtTheWriter(t *testing.T) {
	w := room("A", "B")
	if _, err := w.Send("A", "a", Msg{Body: "", To: []Addr{at("B")}}); !errors.Is(err, ErrNotAllow) {
		t.Fatalf("empty message: %v", err)
	}
	m := send(t, w, "A", "B", "x")
	if _, err := w.Replace("A", "a", m, ""); !errors.Is(err, ErrNotAllow) {
		t.Fatalf("empty replace: %v", err)
	}
}
