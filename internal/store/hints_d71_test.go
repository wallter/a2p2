package store

// The review of D71 (F9): a stream hint expires by the age of the reports
// that could still hold it up.

import (
	"context"
	"testing"
	"time"
)

// Two members that both went silent cannot keep a hint alive by passing
// it between them. When the hint of one goes, it is handed only to
// another report observed lately; the other silent member's day-old
// detail does not take it over, at this pass or the next.
func TestTwoSilentMembersCannotPassAHintBetweenThem(t *testing.T) {
	s, _ := openTest(t)
	author := nodeOf(testKey("a"))
	one, two := nodeOf(testKey("one")), nodeOf(testKey("two"))
	observeDetail(t, s, one, author, map[string]int64{epochN(1): 3}, t0)
	observeDetail(t, s, two, author, map[string]int64{epochN(1): 2}, t0.Add(time.Minute))
	members := []string{author, one, two}
	for pass := range 2 {
		dropReports(t, s, members, t0.Add(time.Duration(24+pass)*time.Hour))
		for _, st := range streamsOf(t, s) {
			if st.Epoch == epochN(1) {
				t.Fatalf("pass %d: the hint is kept at %d", pass, st.Known)
			}
		}
	}
}

// The honest case: a silent member's hint is handed to a member observed
// lately that reports the stream, at that member's position.
func TestASilentMembersHintGoesToAMemberObservedLately(t *testing.T) {
	s, _ := openTest(t)
	author := nodeOf(testKey("a"))
	silent, live := nodeOf(testKey("silent")), nodeOf(testKey("live"))
	observeDetail(t, s, silent, author, map[string]int64{epochN(1): 3}, t0)
	observeDetail(t, s, live, author, map[string]int64{epochN(1): 2}, t0.Add(30*time.Hour))
	dropReports(t, s, []string{author, silent, live}, t0.Add(24*time.Hour))
	for _, st := range streamsOf(t, s) {
		if st.Epoch == epochN(1) && st.Known == 2 {
			return
		}
	}
	t.Fatalf("the hint was not handed to the live member: %+v", streamsOf(t, s))
}

// A fresh observation of a member's log alone does not hold up its
// day-old stream detail: the hint goes.
func TestAFreshLogOnlyObservationDoesNotKeepADayOldHint(t *testing.T) {
	s, _ := openTest(t)
	author := nodeOf(testKey("a"))
	p := nodeOf(testKey("p"))
	observeDetail(t, s, p, author, map[string]int64{epochN(1): 3}, t0)
	if _, err := s.Exchange(context.Background(), testRoom, 0, func(tx *Tx) error {
		return tx.ObserveLog(testRoom, p, t0.Add(30*time.Hour), PeerLog{})
	}); err != nil {
		t.Fatal(err)
	}
	dropReports(t, s, []string{author, p}, t0.Add(24*time.Hour))
	for _, st := range streamsOf(t, s) {
		if st.Epoch == epochN(1) {
			t.Fatalf("a day-old hint is kept by a fresh log row: %+v", st)
		}
	}
}
