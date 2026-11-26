package store

// Tests of the second review of D62: one peer's report cannot erase the
// gap hint another peer's report set.

import (
	"fmt"
	"testing"
	"time"
)

// observeAuthor stores peer's whole report of author, its detail and its
// hints, in one exchange, as node.observe does.
func observeAuthor(t *testing.T, s *Store, peer, author string, known map[string]int64, at time.Time) {
	t.Helper()
	detail := []StreamDetail{}
	for e, c := range known {
		detail = append(detail, StreamDetail{E: e, C: c})
	}
	observeWhole(t, s, peer, []Observation{{Author: author, Digest: fmt.Sprint(known), Detail: detail}},
		map[string]map[string]int64{author: known}, at)
}

// A hostile peer reported a higher known position on a stream an honest
// peer had reported, which made it the hint's setter, and then left the
// stream out of its next report, which deleted the honest peer's hint
// with it. Its next report now falls back to the honest peer's last
// report, whether it drops the stream, lowers it, or leaves the author out.
func TestAPeerThatRaisesAndThenDropsAnotherPeersHintLeavesThatPeersReport(t *testing.T) {
	s, _ := openTest(t)
	author, honest, hostile := nodeOf(testKey("a")), nodeOf(testKey("honest")), nodeOf(testKey("hostile"))
	const e = "00000000000000e1"
	observeAuthor(t, s, honest, author, map[string]int64{e: 5}, t0)
	observeAuthor(t, s, hostile, author, map[string]int64{e: 9}, t0.Add(time.Minute))
	if ss := streamsOf(t, s); len(ss) != 1 || ss[0].Known != 9 {
		t.Fatalf("after the higher report: %+v, want known 9", ss)
	}
	observeAuthor(t, s, hostile, author, map[string]int64{}, t0.Add(2*time.Minute))
	ss := streamsOf(t, s)
	if len(ss) != 1 || ss[0].Known != 5 || !ss[0].KnownAt.Equal(t0) {
		t.Fatalf("after the hostile peer dropped the stream: %+v, want the honest report (known 5 at %v)", ss, t0)
	}
	observeAuthor(t, s, hostile, author, map[string]int64{e: 9}, t0.Add(3*time.Minute))
	observeAuthor(t, s, hostile, author, map[string]int64{e: 1}, t0.Add(4*time.Minute))
	if ss := streamsOf(t, s); len(ss) != 1 || ss[0].Known != 5 {
		t.Fatalf("after the hostile peer lowered the stream: %+v, want known 5", ss)
	}
	observeAuthor(t, s, hostile, author, map[string]int64{e: 9}, t0.Add(5*time.Minute))
	observeWhole(t, s, hostile, nil, nil, t0.Add(6*time.Minute))
	if ss := streamsOf(t, s); len(ss) != 1 || ss[0].Known != 5 {
		t.Fatalf("after a whole report without the author: %+v, want known 5", ss)
	}
	// The honest peer's own report still lowers and drops it.
	observeAuthor(t, s, honest, author, map[string]int64{}, t0.Add(7*time.Minute))
	if ss := streamsOf(t, s); len(ss) != 0 {
		t.Fatalf("after the honest peer stopped naming the stream: %+v, want none", ss)
	}
}

// The honest case: when no other peer reports the stream, the peer whose
// report set a hint still lowers it and drops it, as before, and a hint
// another peer set stays.
func TestAHintNoOtherPeerReportsIsStillLoweredAndDroppedByItsPeer(t *testing.T) {
	s, _ := openTest(t)
	author, p1, p2 := nodeOf(testKey("a")), nodeOf(testKey("p1")), nodeOf(testKey("p2"))
	const e, f = "00000000000000e1", "00000000000000e2"
	observeAuthor(t, s, p1, author, map[string]int64{e: 5}, t0)
	observeAuthor(t, s, p2, author, map[string]int64{f: 3}, t0)
	observeAuthor(t, s, p1, author, map[string]int64{e: 2}, t0.Add(time.Minute))
	got := map[string]int64{}
	for _, st := range streamsOf(t, s) {
		got[st.Epoch] = st.Known
	}
	if got[e] != 2 || got[f] != 3 {
		t.Fatalf("after a lower report: %v, want %s at 2 and %s at 3", got, e, f)
	}
	observeAuthor(t, s, p1, author, map[string]int64{}, t0.Add(2*time.Minute))
	ss := streamsOf(t, s)
	if len(ss) != 1 || ss[0].Epoch != f || ss[0].Known != 3 {
		t.Fatalf("after the peer stopped naming its stream: %+v, want only %s at 3", ss, f)
	}
}
