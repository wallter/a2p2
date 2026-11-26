package delivery

import (
	"errors"
	"testing"
)

// Requirement: D5. The second review of D62 in the model: a reply without
// a version, by an agent that may report the message handled and was
// shown an older text of it, is refused until a read shows the newest
// text, as a handled report is (node
// TestAReplyWithoutAVersionToTextReplacedSinceTheReadIsRefused). A reply by
// an addressee never shown the message, or by an agent it is not
// addressed to, is not checked.
func TestAReplyToTextReplacedSinceTheReadWaitsForTheNewTextAsAHandledReportDoes(t *testing.T) {
	w := levelRoom()
	q := post(w, Msg{Body: "v0", To: []Addr{{"A", "a"}}})
	pull(t, w, "A", "B")
	readAs(t, w, "A", "a")
	w.Advance(Hour)
	must(w.Replace("B", "x", q, "v1"))
	pull(t, w, "A", "B")
	if _, err := w.Send("A", "a", Msg{Body: "yes", Parent: q}); !errors.Is(err, ErrNotAllow) {
		t.Fatalf("reply after the replacement, before a read: %v, want not allowed", err)
	}
	must(w.Send("A", "b", Msg{Body: "not mine", Parent: q}))
	wantTexts(t, must(w.List("A", "a")).Entries, "replaced:v1")
	must(w.Send("A", "a", Msg{Body: "yes", Parent: q}))
}

// The model's hints: a peer that raises another peer's hint and then no
// longer reports the stream leaves the other peer's last report, so no
// peer erases another's (store TestAPeerThatRaisesAndThenDropsAnotherPeersHintLeavesThatPeersReport).
func TestAHintRaisedAndDroppedByOnePeerFallsBackToAnotherPeersReport(t *testing.T) {
	m := &Machine{ID: "A", st: newStore()}
	sid := StreamID{Author: "C", Epoch: 1}
	m.observe("B", Summary{sid: {Contig: 5}}, nil, 10, 1)
	m.observe("H", Summary{sid: {Contig: 9}}, nil, 20, 2)
	if k := m.st.known[sid]; k.Seq != 9 || k.From != "H" {
		t.Fatalf("after the higher report: %+v", k)
	}
	m.observe("H", Summary{}, nil, 30, 3)
	if k := m.st.known[sid]; k.Seq != 5 || k.From != "B" || k.At != 10 {
		t.Fatalf("after the higher peer dropped it: %+v, want B's report of 5 at 10", k)
	}
	m.observe("B", Summary{}, nil, 40, 4)
	if k, ok := m.st.known[sid]; ok {
		t.Fatalf("after every peer dropped it: %+v, want none", k)
	}
}
