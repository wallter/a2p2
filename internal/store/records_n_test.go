package store

// Tests of the records group of the 2026-11-25 security review: pruning
// with many undecided pairs (F32), stream rows from unsigned peer reports
// (F9) and receipts for a room left, or left and joined again, after the
// read (F70).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

func putUndecided(t *testing.T, s *Store, r *core.VerifiedRecord, now time.Time) {
	t.Helper()
	o, err := s.PutRecord(context.Background(), r, PutOpts{Now: now, MayRead: true, Undecided: true})
	if err != nil {
		t.Fatal(err)
	}
	wantOutcome(t, o, OutcomeStored)
}

// F32: the pairs a prune leaves alone were one SQL variable each, so more
// of them than SQLite takes failed the whole pass and no text expired.
func TestPruningWithMoreUndecidedPairsThanSQLiteTakesVariablesStillExpiresText(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a, b, c := testKey("a"), testKey("b"), testKey("c")
	kept := mk(t, a, core.RecordFields{})
	putUndecided(t, s, kept, t0)
	other := mk(t, b, core.RecordFields{})
	putUndecided(t, s, other, t0)
	live := mk(t, c, core.RecordFields{})
	wantOutcome(t, put(t, s, live, t0), OutcomeStored)

	keep := map[[2]string]bool{{testRoom, nodeOf(a)}: true}
	for i := range 40000 {
		keep[[2]string{testRoom, fmt.Sprintf("%064x", i)}] = true
	}
	n, err := s.ExpireTextWith(ctx, t0.Add(Retention+time.Hour), func(*Reader) (map[[2]string]bool, error) { return keep, nil })
	if err != nil {
		t.Fatalf("prune with %d undecided pairs: %v", len(keep), err)
	}
	if n != 2 {
		t.Fatalf("prune changed %d records, want 2", n)
	}
	wantReason(t, s, kept.ID(), ReasonMissing)
	wantReason(t, s, other.ID(), ReasonExpired)
	wantReason(t, s, live.ID(), ReasonExpired)
}

// The honest case: with no undecided pair, or a few, pruning expires what
// is past retention and keeps what is not.
func TestPruningWithFewOrNoUndecidedPairsExpiresOnlyTextPastRetention(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a, b := testKey("a"), testKey("b")
	old := mk(t, a, core.RecordFields{})
	putUndecided(t, s, old, t0)
	young := mk(t, b, core.RecordFields{Created: t0.Add(10 * 24 * time.Hour)})
	putUndecided(t, s, young, t0.Add(10*24*time.Hour))
	now := t0.Add(Retention + time.Hour)
	n, err := s.ExpireTextWith(ctx, now, func(*Reader) (map[[2]string]bool, error) {
		return map[[2]string]bool{{testRoom, "someone else"}: true}, nil
	})
	if err != nil || n != 1 {
		t.Fatalf("prune: %d changed, %v; want 1", n, err)
	}
	wantReason(t, s, old.ID(), ReasonExpired)
	wantReason(t, s, young.ID(), ReasonMissing)
	if n, err := s.ExpireText(ctx, now.Add(10*24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("second prune: %d changed, %v; want 1", n, err)
	}
	wantReason(t, s, young.ID(), ReasonExpired)
}

func noteKnown(t *testing.T, s *Store, peer, author string, known map[string]int64, at time.Time) {
	t.Helper()
	if _, err := s.Exchange(context.Background(), testRoom, 0, func(tx *Tx) error {
		return tx.NoteKnown(testRoom, peer, author, known, at)
	}); err != nil {
		t.Fatal(err)
	}
}

func streamsOf(t *testing.T, s *Store) []Stream {
	t.Helper()
	var ss []Stream
	if err := s.View(context.Background(), func(rd *Reader) (err error) { ss, err = rd.Streams(testRoom); return err }); err != nil {
		t.Fatal(err)
	}
	return ss
}

func epochN(i int) string { return fmt.Sprintf("%016x", i+1) }

// F9: a peer's pull report is unsigned, and every epoch it named for an
// admitted author became a stream row here that nothing deleted. Now at
// most MaxHintedStreams per author are kept on one peer's word, and they
// go when the peer stops naming them.
func TestPeerReportOfManyUnknownEpochsKeepsABoundedNumberOfStreamsAndNoneAfterItStops(t *testing.T) {
	s, _ := openTest(t)
	victim, liar := nodeOf(testKey("victim")), nodeOf(testKey("liar"))
	fake := map[string]int64{}
	for i := range 1000 {
		fake[epochN(i)] = 5
	}
	noteKnown(t, s, liar, victim, fake, t0)
	if n := len(streamsOf(t, s)); n != MaxHintedStreams {
		t.Fatalf("%d streams kept from one report of 1000 epochs, want %d", n, MaxHintedStreams)
	}
	// Reporting the same again, or other epochs, adds none.
	more := map[string]int64{}
	for i := 1000; i < 2000; i++ {
		more[epochN(i)] = 5
	}
	for e, c := range fake {
		more[e] = c
	}
	noteKnown(t, s, liar, victim, more, t0.Add(time.Minute))
	if n := len(streamsOf(t, s)); n != MaxHintedStreams {
		t.Fatalf("%d streams after a second report, want %d", n, MaxHintedStreams)
	}
	noteKnown(t, s, liar, victim, map[string]int64{}, t0.Add(2*time.Minute))
	if ss := streamsOf(t, s); len(ss) != 0 {
		t.Fatalf("%d streams left after the peer stopped naming them, want none: %+v", len(ss), ss[0])
	}
}

// The honest case: a stream a peer reports is listed as known until its
// records arrive, a held stream keeps its row when the peer stops naming
// it, and one peer's hints do not use up another peer's.
func TestPeerReportOfARealStreamIsKeptAndAHeldStreamStaysWhenNoLongerReported(t *testing.T) {
	s, _ := openTest(t)
	a := testKey("a")
	author, p1, p2 := nodeOf(a), nodeOf(testKey("p1")), nodeOf(testKey("p2"))
	m1 := mk(t, a, core.RecordFields{})
	wantOutcome(t, put(t, s, m1, t0), OutcomeStored)
	fill := map[string]int64{"00000000000000e1": 3}
	for i := range 2 * MaxHintedStreams {
		fill[epochN(i)] = 1
	}
	noteKnown(t, s, p1, author, fill, t0)
	noteKnown(t, s, p2, author, map[string]int64{"00000000000000e2": 4}, t0)
	var held, e2 *Stream
	ss := streamsOf(t, s)
	for i := range ss {
		switch ss[i].Epoch {
		case "00000000000000e1":
			held = &ss[i]
		case "00000000000000e2":
			e2 = &ss[i]
		}
	}
	if held == nil || held.Contig != 1 || held.Known != 3 {
		t.Fatalf("held stream %+v, want contig 1 known 3", held)
	}
	if e2 == nil || e2.Contig != 0 || e2.Known != 4 {
		t.Fatalf("stream known from a second peer %+v, want known 4", e2)
	}
	noteKnown(t, s, p1, author, map[string]int64{}, t0.Add(time.Minute))
	ss = streamsOf(t, s)
	if len(ss) != 2 || ss[0].Epoch != "00000000000000e1" || ss[0].Contig != 1 || ss[0].Known != 0 ||
		ss[1].Epoch != "00000000000000e2" || ss[1].Known != 4 {
		t.Fatalf("after the first peer stopped: %+v", ss)
	}
}

// Rows written before the bound (more than MaxHintedStreams for one
// author) still load and are lowered and dropped as before.
func TestStreamRowsHintedBeyondTheBoundBeforeItStillLoadAndAreDroppedWhenNoLongerReported(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	author, peer := nodeOf(testKey("a")), nodeOf(testKey("p"))
	for i := range 3 * MaxHintedStreams {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO streams (room, author, epoch, contig, known, known_at, known_from)
			VALUES (?,?,?,0,7,?,?)`, testRoom, author, epochN(i), ms(t0), peer); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(streamsOf(t, s)); n != 3*MaxHintedStreams {
		t.Fatalf("%d streams, want %d", n, 3*MaxHintedStreams)
	}
	noteKnown(t, s, peer, author, map[string]int64{epochN(0): 9}, t0.Add(time.Minute))
	ss := streamsOf(t, s)
	if len(ss) != 1 || ss[0].Known != 9 {
		t.Fatalf("after a report of one: %+v", ss)
	}
}

// F9: a higher report takes over a hint another peer set. Two members
// took turns: one named 32 new epochs, the other reported them all one
// higher, which moved them off the first one's count, so each round added
// 32 rows. A hint taken over counts against the peer that took it.
func TestTwoPeersHandingHintsBackAndForthKeepABoundedNumberOfStreams(t *testing.T) {
	s, _ := openTest(t)
	victim := nodeOf(testKey("victim"))
	seeder, claimer := nodeOf(testKey("seeder")), nodeOf(testKey("claimer"))
	claimed := map[string]int64{}
	for round := range 10 {
		seed := map[string]int64{}
		for i := range MaxHintedStreams {
			seed[epochN(round*MaxHintedStreams+i)] = 5
		}
		noteKnown(t, s, seeder, victim, seed, t0.Add(time.Duration(round)*time.Minute))
		for e := range seed {
			claimed[e] = 6
		}
		noteKnown(t, s, claimer, victim, claimed, t0.Add(time.Duration(round)*time.Minute+time.Second))
	}
	if n := len(streamsOf(t, s)); n > 2*MaxHintedStreams {
		t.Fatalf("%d streams after ten rounds of two peers, want at most %d", n, 2*MaxHintedStreams)
	}
	// Each peer stops: nothing is left on either's word.
	noteKnown(t, s, claimer, victim, map[string]int64{}, t0.Add(time.Hour))
	noteKnown(t, s, seeder, victim, map[string]int64{}, t0.Add(time.Hour))
	if ss := streamsOf(t, s); len(ss) != 0 {
		t.Fatalf("%d streams left after both peers stopped, want none", len(ss))
	}
}

// The honest case of a take-over: a second peer that holds more of a
// stream raises its known position, and the first peer's lower report
// then leaves it.
func TestAHigherReportFromAnotherPeerRaisesAKnownPosition(t *testing.T) {
	s, _ := openTest(t)
	author, p1, p2 := nodeOf(testKey("a")), nodeOf(testKey("p1")), nodeOf(testKey("p2"))
	noteKnown(t, s, p1, author, map[string]int64{"00000000000000e1": 3}, t0)
	noteKnown(t, s, p2, author, map[string]int64{"00000000000000e1": 7}, t0.Add(time.Minute))
	noteKnown(t, s, p1, author, map[string]int64{"00000000000000e1": 2}, t0.Add(2*time.Minute))
	ss := streamsOf(t, s)
	if len(ss) != 1 || ss[0].Known != 7 {
		t.Fatalf("streams %+v, want one known at 7", ss)
	}
}

func observeWhole(t *testing.T, s *Store, peer string, rows []Observation, hints map[string]map[string]int64, at time.Time) {
	t.Helper()
	if _, err := s.Exchange(context.Background(), testRoom, 0, func(tx *Tx) error {
		if err := tx.Observe(testRoom, peer, at, rows); err != nil {
			return err
		}
		for a, k := range hints {
			if err := tx.NoteKnown(testRoom, peer, a, k, at); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// F9: a whole report that leaves an author out deleted the observation of
// it but kept the stream rows the peer's earlier report had made, listed
// as still being fetched for the life of the room.
func TestAWholeReportThatLeavesAnAuthorOutDropsThePeersHintsForIt(t *testing.T) {
	s, _ := openTest(t)
	a := testKey("a")
	author, other, peer, p2 := nodeOf(a), nodeOf(testKey("o")), nodeOf(testKey("p")), nodeOf(testKey("p2"))
	held := mk(t, a, core.RecordFields{})
	wantOutcome(t, put(t, s, held, t0), OutcomeStored)
	heldEpoch := streamsOf(t, s)[0].Epoch
	fake := map[string]int64{heldEpoch: 4}
	for i := range 5 {
		fake[epochN(i)] = 5
	}
	observeWhole(t, s, peer, []Observation{{Author: author, Digest: "d", Detail: []StreamDetail{{E: heldEpoch, C: 4}}},
		{Author: other, Digest: "d2", Detail: []StreamDetail{{E: "00000000000000e9", C: 2}}}},
		map[string]map[string]int64{author: fake, other: {"00000000000000e9": 2}}, t0)
	noteKnown(t, s, p2, author, map[string]int64{"00000000000000e2": 3}, t0)
	if n := len(streamsOf(t, s)); n != 8 {
		t.Fatalf("%d streams before, want 8", n)
	}
	// The next whole report leaves author out and keeps other.
	observeWhole(t, s, peer, []Observation{{Author: other, Digest: "d2", Detail: []StreamDetail{{E: "00000000000000e9", C: 2}}}},
		map[string]map[string]int64{other: {"00000000000000e9": 2}}, t0.Add(time.Minute))
	got := map[string]Stream{}
	for _, st := range streamsOf(t, s) {
		got[st.Author+"/"+st.Epoch] = st
	}
	if len(got) != 3 {
		t.Fatalf("streams after the author was left out: %+v", got)
	}
	if st := got[author+"/"+heldEpoch]; st.Contig != 1 || st.Known != 0 {
		t.Fatalf("held stream %+v, want contig 1 and no known position", st)
	}
	if st := got[author+"/00000000000000e2"]; st.Known != 3 {
		t.Fatalf("another peer's hint %+v, want kept", st)
	}
	if st := got[other+"/00000000000000e9"]; st.Known != 2 {
		t.Fatalf("hint for an author still reported %+v, want kept", st)
	}
}

// F70: an inbox read and its receipts are two transactions. A leave that
// commits between them left receipt rows for the room left.
func TestReceiptsOfAReadAreNotWrittenForARoomLeftMeanwhile(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if _, err := s.LeaveRoom(ctx, testRoom, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Show(ctx, "alice", testRoom, []Shown{{Key: "k", Version: "v", Msg: "m"}}); err != nil {
		t.Fatalf("show after leave: %v", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM shown`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d receipts for a room left, %v", n, err)
	}
}

// The honest case: receipts for a room held are written.
func TestReceiptsOfAReadAreWrittenForARoomHeld(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if err := s.Show(ctx, "alice", testRoom, []Shown{{Key: "k", Version: "v", Msg: "m"}}); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := s.View(ctx, func(rd *Reader) (err error) { got, err = rd.Receipts("alice", testRoom); return err }); err != nil {
		t.Fatal(err)
	}
	if got["k"] != "v" {
		t.Fatalf("receipts %v", got)
	}
}

// F70: a read before a leave and a rejoin of the same room wrote its
// receipts into the room joined again, where the same signed record can
// come back, so Seen could confirm what no read of this generation
// showed. Receipts carry the generation the read saw.
func TestReceiptsOfAReadBeforeALeaveAndARejoinAreNotWritten(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	root := testCreate(t)
	if _, err := s.CreateLogRoom(ctx, logRoom, "team", root, nil, t0); err != nil {
		t.Fatal(err)
	}
	var read int64
	if err := s.View(ctx, func(rd *Reader) (err error) { read, err = rd.RoomGen(logRoom); return err }); err != nil || read == 0 {
		t.Fatalf("generation %d, %v", read, err)
	}
	if _, err := s.LeaveRoom(ctx, logRoom, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var left int64
	if err := s.View(ctx, func(rd *Reader) (err error) { left, err = rd.RoomGen(logRoom); return err }); err != nil || left != 0 {
		t.Fatalf("generation of a room left %d, %v; want 0", left, err)
	}
	now, err := s.CreateLogRoom(ctx, logRoom, "team", root, nil, t0.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if now == read {
		t.Fatalf("rejoin kept generation %d", now)
	}
	if err := s.ShowAt(ctx, "alice", logRoom, read, []Shown{{Key: "k", Version: "v", Msg: "m"}}); err != nil {
		t.Fatalf("show of an earlier generation: %v", err)
	}
	var got map[string]string
	if err := s.View(ctx, func(rd *Reader) (err error) { got, err = rd.Receipts("alice", logRoom); return err }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("receipts %v written into the rejoined room from a read before the leave", got)
	}
	// The honest case: a read of the current generation keeps its receipts.
	if err := s.ShowAt(ctx, "alice", logRoom, now, []Shown{{Key: "k", Version: "v", Msg: "m"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, func(rd *Reader) (err error) { got, err = rd.Receipts("alice", logRoom); return err }); err != nil {
		t.Fatal(err)
	}
	if got["k"] != "v" {
		t.Fatalf("receipts %v of the current generation", got)
	}
}
