package store

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

const testRoom = "r_00112233445566778899aabbccddeeff"

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func testKey(name string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("store test key " + name))
	return ed25519.NewKeyFromSeed(seed[:])
}

func nodeOf(k ed25519.PrivateKey) string {
	return fmt.Sprintf("%x", []byte(k.Public().(ed25519.PublicKey)))
}

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a2p2.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.AddRoom(context.Background(), testRoom, "team", "", t0); err != nil {
		t.Fatal(err)
	}
	return s, path
}

// mk signs a record; zero fields get test defaults.
func mk(t *testing.T, k ed25519.PrivateKey, f core.RecordFields) *core.VerifiedRecord {
	t.Helper()
	if f.Room == "" {
		f.Room = testRoom
	}
	if f.Epoch == "" {
		f.Epoch = "00000000000000e1"
	}
	if f.Seq == 0 {
		f.Seq = 1
	}
	if f.Kind == "" {
		f.Kind = core.KindMessage
	}
	if f.Agent == "" {
		f.Agent = "alice"
	}
	if f.Created.IsZero() {
		f.Created = t0
	}
	if (f.Kind == core.KindMessage || f.Kind == core.KindReplace) && f.Text == "" {
		f.Text = "hello"
	}
	r, err := core.NewRecord(k, f)
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.Verified()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func put(t *testing.T, s *Store, r *core.VerifiedRecord, now time.Time) Outcome {
	t.Helper()
	o, err := s.PutRecord(context.Background(), r, PutOpts{Now: now, MayRead: true})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func wantOutcome(t *testing.T, got, want Outcome) {
	t.Helper()
	if got != want {
		t.Fatalf("outcome %s, want %s", got, want)
	}
}

func held(t *testing.T, s *Store, id string) Held {
	t.Helper()
	var h Held
	var ok bool
	err := s.View(context.Background(), func(rd *Reader) (err error) {
		h, ok, err = rd.Record(testRoom, id)
		return err
	})
	if err != nil || !ok {
		t.Fatalf("record %s: held %v, %v", id, ok, err)
	}
	return h
}

func wantReason(t *testing.T, s *Store, id, reason string) {
	t.Helper()
	h := held(t, s, id)
	if h.Reason != reason {
		t.Fatalf("record %s reason %s, want %s", id, h.Reason, reason)
	}
	if hasText := h.Record.Body() != nil; hasText != (reason == ReasonLive && h.Record.Size() > 0) {
		t.Fatalf("record %s: text held %v with reason %s", id, hasText, reason)
	}
}

// dump is everything the readers return, for comparing two states.
type dump struct {
	Streams []string
	Records []string
	Cursors []Cursor
	Reads   []Read
	Heads   map[string][]string
}

func snapshot(t *testing.T, s *Store, agent string) dump {
	t.Helper()
	var d dump
	err := s.View(context.Background(), func(rd *Reader) error {
		ss, err := rd.Streams(testRoom)
		if err != nil {
			return err
		}
		for _, st := range ss {
			d.Streams = append(d.Streams, fmt.Sprintf("%s.%s:%d:%s", st.Author[:8], st.Epoch, st.Contig, st.First.Record.ID()))
			hs, err := rd.RecordsFrom(testRoom, st.Author, st.Epoch, 1)
			if err != nil {
				return err
			}
			for _, h := range hs {
				d.Records = append(d.Records, fmt.Sprintf("%s %s %s body=%x", h.Record.ID(), h.Reason, h.Effect, h.Record.Body()))
			}
		}
		if d.Cursors, err = rd.Cursors(agent, testRoom); err != nil {
			return err
		}
		if d.Reads, err = rd.Reads(agent, testRoom); err != nil {
			return err
		}
		d.Heads, err = rd.ReadHeads(agent, testRoom)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// [R4] A machine stores only the next position of a stream, with matching
// prev; a different record at a held position is a fork; the same record
// again changes nothing.
func TestRecordIsStoredOnlyAtTheNextPositionOfItsStream(t *testing.T) {
	s, _ := openTest(t)
	a := testKey("a")
	m1 := mk(t, a, core.RecordFields{Text: "one"})
	m2 := mk(t, a, core.RecordFields{Seq: 2, Prev: m1.ID(), Text: "two"})
	m3 := mk(t, a, core.RecordFields{Seq: 3, Prev: m2.ID(), Text: "three"})

	wantOutcome(t, put(t, s, m2, t0), OutcomeNotNext)
	wantOutcome(t, put(t, s, m1, t0), OutcomeStored)
	wantOutcome(t, put(t, s, m3, t0), OutcomeNotNext)
	wantOutcome(t, put(t, s, m1, t0), OutcomeHeld)

	other1 := mk(t, a, core.RecordFields{Text: "another first"})
	wantOutcome(t, put(t, s, other1, t0), OutcomeFork)
	badPrev := mk(t, a, core.RecordFields{Seq: 2, Prev: other1.ID(), Text: "two"})
	wantOutcome(t, put(t, s, badPrev, t0), OutcomeFork)

	wantOutcome(t, put(t, s, m2, t0), OutcomeStored)
	wantOutcome(t, put(t, s, m3, t0), OutcomeStored)
	other2 := mk(t, a, core.RecordFields{Seq: 2, Prev: m1.ID(), Text: "two again"})
	wantOutcome(t, put(t, s, other2, t0), OutcomeFork)

	d := snapshot(t, s, "x")
	if len(d.Streams) != 1 || len(d.Records) != 3 {
		t.Fatalf("streams %v records %v", d.Streams, d.Records)
	}
}

// [R2, R4] A stream's first record is stored only when every record it
// links to is held, matched by epoch, seq and id.
func TestFirstRecordOfAStreamIsStoredOnlyWhenItsLinksAreHeld(t *testing.T) {
	s, _ := openTest(t)
	a := testKey("a")
	m1 := mk(t, a, core.RecordFields{})
	n1 := mk(t, a, core.RecordFields{Epoch: "00000000000000e2",
		Links: []core.Link{{Epoch: "00000000000000e1", Seq: 1, ID: m1.ID()}}})
	wrong := mk(t, a, core.RecordFields{Epoch: "00000000000000e3",
		Links: []core.Link{{Epoch: "00000000000000e1", Seq: 1, ID: n1.ID()}}})

	wantOutcome(t, put(t, s, n1, t0), OutcomeLinkMissing)
	wantOutcome(t, put(t, s, m1, t0), OutcomeStored)
	wantOutcome(t, put(t, s, wrong, t0), OutcomeLinkMissing)
	wantOutcome(t, put(t, s, n1, t0), OutcomeStored)
}

// [R6] A header held without its text is "missing"; a body repairs it only
// when this machine may read it; without permission it is not_admitted.
func TestHeaderWithoutTextIsMissingAndRepairedOnlyWhenPermitted(t *testing.T) {
	s, _ := openTest(t)
	a := testKey("a")
	m1 := mk(t, a, core.RecordFields{Text: "secret"})
	wantOutcome(t, put(t, s, m1.WithoutBody(), t0), OutcomeStored)
	wantReason(t, s, m1.ID(), ReasonMissing)

	o, err := s.PutRecord(context.Background(), m1, PutOpts{Now: t0, MayRead: false})
	if err != nil {
		t.Fatal(err)
	}
	wantOutcome(t, o, OutcomeHeld)
	wantReason(t, s, m1.ID(), ReasonMissing)

	wantOutcome(t, put(t, s, m1, t0), OutcomeRepaired)
	wantReason(t, s, m1.ID(), ReasonLive)
	if got := held(t, s, m1.ID()).Record.Fields().Text; got != "secret" {
		t.Fatal("repaired text differs")
	}

	m2 := mk(t, a, core.RecordFields{Seq: 2, Prev: m1.ID(), Text: "later"})
	o, err = s.PutRecord(context.Background(), m2, PutOpts{Now: t0, MayRead: false})
	if err != nil {
		t.Fatal(err)
	}
	wantOutcome(t, o, OutcomeStored)
	wantReason(t, s, m2.ID(), ReasonNotAdmitted)
}

// [R7] A withdraw drops the text of its message and of the author's
// replacements, held before or arriving after it, and of nothing else.
func TestWithdrawDropsTheTextOfTheMessageAndOfItsReplacements(t *testing.T) {
	s, _ := openTest(t)
	a, b := testKey("a"), testKey("b")
	m := mk(t, a, core.RecordFields{Text: "v0"})
	r1 := mk(t, a, core.RecordFields{Seq: 2, Prev: m.ID(), Kind: core.KindReplace, Ref: m.ID(), Version: 1, Text: "v1"})
	w := mk(t, a, core.RecordFields{Seq: 3, Prev: r1.ID(), Kind: core.KindWithdraw, Ref: m.ID()})
	r2 := mk(t, a, core.RecordFields{Seq: 4, Prev: w.ID(), Kind: core.KindReplace, Ref: m.ID(), Version: 2, Text: "v2"})
	other := mk(t, b, core.RecordFields{Text: "b's"})
	foreign := mk(t, b, core.RecordFields{Seq: 2, Prev: other.ID(), Kind: core.KindWithdraw, Ref: m.ID()})
	for _, r := range []*core.VerifiedRecord{m, r1, other, foreign} {
		wantOutcome(t, put(t, s, r, t0), OutcomeStored)
	}
	wantReason(t, s, m.ID(), ReasonLive) // a withdraw by another author does nothing
	if e := held(t, s, foreign.ID()).Effect; e != EffectBadReference {
		t.Fatalf("foreign withdraw effect %s", e)
	}
	wantOutcome(t, put(t, s, w, t0), OutcomeStored)
	wantOutcome(t, put(t, s, r2, t0), OutcomeStored)
	for _, id := range []string{m.ID(), r1.ID(), r2.ID()} {
		wantReason(t, s, id, ReasonWithdrawn)
	}
	wantReason(t, s, other.ID(), ReasonLive)
}

// [R7] A withdraw stored before its message (from another stream) means the
// message is stored without text, and the withdraw's reference resolves.
func TestWithdrawStoredBeforeItsMessageDropsTheTextOnArrival(t *testing.T) {
	s, _ := openTest(t)
	a := testKey("a")
	m := mk(t, a, core.RecordFields{Epoch: "00000000000000e1", Text: "gone"})
	w := mk(t, a, core.RecordFields{Epoch: "00000000000000e2", Kind: core.KindWithdraw, Ref: m.ID()})
	wantOutcome(t, put(t, s, w, t0), OutcomeStored)
	if e := held(t, s, w.ID()).Effect; e != EffectUnresolved {
		t.Fatalf("effect %s, want unresolved", e)
	}
	wantOutcome(t, put(t, s, m, t0), OutcomeStored)
	wantReason(t, s, m.ID(), ReasonWithdrawn)
	if e := held(t, s, w.ID()).Effect; e != EffectOK {
		t.Fatalf("effect %s, want ok", e)
	}
}

// [R6] Text expires 30 days after created by this machine's clock, is
// recorded, and stays expired when the clock goes back or the text is
// offered again.
func TestExpiredTextStaysExpiredWhenTheClockGoesBackOrTextIsOfferedAgain(t *testing.T) {
	s, _ := openTest(t)
	a := testKey("a")
	m := mk(t, a, core.RecordFields{Text: "old"})
	wantOutcome(t, put(t, s, m, t0), OutcomeStored)
	ctx := context.Background()
	if n, err := s.ExpireText(ctx, t0.Add(Retention-time.Millisecond)); err != nil || n != 0 {
		t.Fatalf("expired %d early, %v", n, err)
	}
	if n, err := s.ExpireText(ctx, t0.Add(Retention)); err != nil || n != 1 {
		t.Fatalf("expired %d, %v", n, err)
	}
	wantReason(t, s, m.ID(), ReasonExpired)
	if _, err := s.ExpireText(ctx, t0); err != nil {
		t.Fatal(err)
	}
	wantOutcome(t, put(t, s, m, t0), OutcomeHeld)
	wantReason(t, s, m.ID(), ReasonExpired)

	// Stored after its 30 days: expired at once, text not kept.
	late := mk(t, a, core.RecordFields{Seq: 2, Prev: m.ID(), Text: "late"})
	wantOutcome(t, put(t, s, late, t0.Add(Retention)), OutcomeStored)
	wantReason(t, s, late.ID(), ReasonExpired)

	// A withdraw still turns expired into withdrawn, as in the model.
	w := mk(t, a, core.RecordFields{Seq: 3, Prev: late.ID(), Kind: core.KindWithdraw, Ref: m.ID()})
	wantOutcome(t, put(t, s, w, t0), OutcomeStored)
	wantReason(t, s, m.ID(), ReasonWithdrawn)
}

// PROTOCOL P1: a record dated more than 24 hours ahead is not stored.
func TestRecordDatedMoreThanADayAheadIsNotStoredYet(t *testing.T) {
	s, _ := openTest(t)
	m := mk(t, testKey("a"), core.RecordFields{Created: t0.Add(futureSlack + time.Millisecond)})
	wantOutcome(t, put(t, s, m, t0), OutcomeFuture)
	wantOutcome(t, put(t, s, m, t0.Add(time.Millisecond)), OutcomeStored)
}

// [R4] Records of a room this machine does not hold are refused.
func TestRecordOfAnUnknownRoomIsRefused(t *testing.T) {
	s, _ := openTest(t)
	m := mk(t, testKey("a"), core.RecordFields{Room: "r_ffffffffffffffffffffffffffffffff"})
	if _, err := s.PutRecord(context.Background(), m, PutOpts{Now: t0, MayRead: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}
}

// [R7] References resolve against held records of the room, now or when
// the target arrives: a reply's thread must match its parent's; a handled
// needs an addressee; a replace needs the same author.
func TestReferencesResolveWhenTheirTargetArrives(t *testing.T) {
	s, _ := openTest(t)
	a, b := testKey("a"), testKey("b")
	bob := core.Addressee{Node: nodeOf(b), Agent: "bob"}
	root := mk(t, a, core.RecordFields{Epoch: "00000000000000e1", To: []core.Addressee{bob}})
	reply := mk(t, b, core.RecordFields{Epoch: "00000000000000b1", Agent: "bob", Parent: root.ID(), Thread: root.ID()})
	badThread := mk(t, b, core.RecordFields{Epoch: "00000000000000b1", Seq: 2, Prev: reply.ID(), Agent: "bob",
		Parent: root.ID(), Thread: reply.ID()})
	byBob := mk(t, b, core.RecordFields{Epoch: "00000000000000b1", Seq: 3, Prev: badThread.ID(), Agent: "bob",
		Kind: core.KindHandled, Ref: root.ID()})
	byCarol := mk(t, b, core.RecordFields{Epoch: "00000000000000b1", Seq: 4, Prev: byBob.ID(), Agent: "carol",
		Kind: core.KindHandled, Ref: root.ID()})
	replaceByB := mk(t, b, core.RecordFields{Epoch: "00000000000000b1", Seq: 5, Prev: byCarol.ID(), Agent: "bob",
		Kind: core.KindReplace, Ref: root.ID(), Version: 1})
	for _, r := range []*core.VerifiedRecord{reply, badThread, byBob, byCarol, replaceByB} {
		wantOutcome(t, put(t, s, r, t0), OutcomeStored)
		if e := held(t, s, r.ID()).Effect; e != EffectUnresolved {
			t.Fatalf("effect %s before the target", e)
		}
	}
	wantOutcome(t, put(t, s, root, t0), OutcomeStored)
	want := map[string]string{reply.ID(): EffectOK, badThread.ID(): EffectBadReference, byBob.ID(): EffectOK,
		byCarol.ID(): EffectBadReference, replaceByB.ID(): EffectBadReference, root.ID(): EffectOK}
	for id, e := range want {
		if got := held(t, s, id).Effect; got != e {
			t.Errorf("effect %s, want %s", got, e)
		}
	}
	// The other author's replace changes nothing about the message's text.
	wantReason(t, s, root.ID(), ReasonLive)
}

// [R7] The readers: a stream from a position, by ref, by parent, by thread.
func TestReadersReturnRecordsByStreamRefParentAndThread(t *testing.T) {
	s, _ := openTest(t)
	a := testKey("a")
	m := mk(t, a, core.RecordFields{})
	r1 := mk(t, a, core.RecordFields{Seq: 2, Prev: m.ID(), Parent: m.ID(), Thread: m.ID()})
	r2 := mk(t, a, core.RecordFields{Seq: 3, Prev: r1.ID(), Parent: r1.ID(), Thread: m.ID()})
	h := mk(t, a, core.RecordFields{Seq: 4, Prev: r2.ID(), Kind: core.KindReplace, Ref: m.ID(), Version: 1})
	for _, r := range []*core.VerifiedRecord{m, r1, r2, h} {
		wantOutcome(t, put(t, s, r, t0), OutcomeStored)
	}
	ids := func(hs []Held) []string {
		var out []string
		for _, h := range hs {
			out = append(out, h.Record.ID())
		}
		return out
	}
	err := s.View(context.Background(), func(rd *Reader) error {
		from, err := rd.RecordsFrom(testRoom, nodeOf(a), "00000000000000e1", 3)
		if err != nil {
			return err
		}
		if got := ids(from); !reflect.DeepEqual(got, []string{r2.ID(), h.ID()}) {
			t.Errorf("from 3: %v", got)
		}
		byRef, err := rd.ByRef(testRoom, m.ID())
		if err != nil {
			return err
		}
		if got := ids(byRef); !reflect.DeepEqual(got, []string{h.ID()}) {
			t.Errorf("by ref: %v", got)
		}
		byParent, err := rd.ByParent(testRoom, r1.ID())
		if err != nil {
			return err
		}
		if got := ids(byParent); !reflect.DeepEqual(got, []string{r2.ID()}) {
			t.Errorf("by parent: %v", got)
		}
		thread, err := rd.ByThread(testRoom, m.ID())
		if err != nil {
			return err
		}
		if got := ids(thread); !reflect.DeepEqual(got, []string{m.ID(), r1.ID(), r2.ID()}) {
			t.Errorf("thread: %v", got)
		}
		streams, err := rd.Streams(testRoom)
		if err != nil {
			return err
		}
		if len(streams) != 1 || streams[0].Contig != 4 || streams[0].First.Record.ID() != m.ID() {
			t.Errorf("streams %+v", streams)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// [R7] Seen writes exactly the reads, heads and cursors its plan returns,
// in one transaction; the plan sees the state before them; a later Seen
// moves the cursor and replaces heads per author.
func TestSeenAppliesItsPlansReadsHeadsAndCursors(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	err := s.Seen(ctx, "bob", testRoom, func(rd *Reader) (SeenPlan, error) {
		reads, err := rd.Reads("bob", testRoom)
		if err != nil || len(reads) != 0 {
			t.Errorf("plan saw reads %v %v", reads, err)
		}
		return SeenPlan{Reads: []Read{{"k1", "v1", "m1"}, {"x:k2", "", "k2"}},
			Heads:   map[string][]string{"A": {"h1", "h2"}},
			Cursors: []Cursor{{Author: "A", Epoch: "e", Start: 0, Pos: 2}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Seen(ctx, "bob", testRoom, func(*Reader) (SeenPlan, error) {
		return SeenPlan{Reads: []Read{{"k1", "v2", "m1"}}, Heads: map[string][]string{"A": {"h3"}},
			Cursors: []Cursor{{Author: "A", Epoch: "e", Start: 0, Pos: 5}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	d := snapshot(t, s, "bob")
	want := dump{Cursors: []Cursor{{"A", "e", 0, 5}}, Reads: []Read{{"k1", "v2", "m1"}, {"x:k2", "", "k2"}},
		Heads: map[string][]string{"A": {"h3"}}}
	d.Streams, d.Records = nil, nil
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("got %+v\nwant %+v", d, want)
	}
	var byMsg []Read
	if err := s.View(ctx, func(rd *Reader) (err error) { byMsg, err = rd.ReadsOf("bob", testRoom, "m1"); return }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(byMsg, []Read{{"k1", "v2", "m1"}}) {
		t.Fatalf("reads of m1: %v", byMsg)
	}
	if other := snapshot(t, s, "alice"); len(other.Reads) != 0 || len(other.Cursors) != 0 {
		t.Fatalf("another agent's state changed: %+v", other)
	}
}

// AGENTS.md "every store method is one transaction": a failure between any
// two statements of PutRecord or Seen, or a cancelled context, leaves the
// database exactly as it was.
func TestFailureBetweenStatementsLeavesNoHalfAppliedWrite(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a := testKey("a")
	m := mk(t, a, core.RecordFields{})
	wantOutcome(t, put(t, s, m, t0), OutcomeStored)
	w := mk(t, a, core.RecordFields{Seq: 2, Prev: m.ID(), Kind: core.KindWithdraw, Ref: m.ID()})
	before := snapshot(t, s, "bob")
	boom := errors.New("injected failure")

	for _, point := range []string{"put:inserted", "put:stream", "put:withdrawn"} {
		testHook = func(p string) error {
			if p == point {
				return boom
			}
			return nil
		}
		_, err := s.PutRecord(ctx, w, PutOpts{Now: t0, MayRead: true})
		testHook = nil
		if !errors.Is(err, boom) {
			t.Fatalf("%s: err %v", point, err)
		}
		if got := snapshot(t, s, "bob"); !reflect.DeepEqual(got, before) {
			t.Fatalf("%s: state changed:\n%+v\n%+v", point, got, before)
		}
	}
	plan := func(*Reader) (SeenPlan, error) {
		return SeenPlan{Reads: []Read{{"k", "v", "k"}}, Heads: map[string][]string{"A": {"h"}},
			Cursors: []Cursor{{"A", "e", 0, 1}}}, nil
	}
	for _, point := range []string{"seen:reads", "seen:heads"} {
		testHook = func(p string) error {
			if p == point {
				return boom
			}
			return nil
		}
		err := s.Seen(ctx, "bob", testRoom, plan)
		testHook = nil
		if !errors.Is(err, boom) {
			t.Fatalf("%s: err %v", point, err)
		}
		if got := snapshot(t, s, "bob"); !reflect.DeepEqual(got, before) {
			t.Fatalf("%s: state changed", point)
		}
	}
	err := s.Seen(ctx, "bob", testRoom, func(*Reader) (SeenPlan, error) { return SeenPlan{}, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("plan failure: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.PutRecord(cctx, w, PutOpts{Now: t0, MayRead: true}); err == nil {
		t.Fatal("cancelled context stored a record")
	}
	if got := snapshot(t, s, "bob"); !reflect.DeepEqual(got, before) {
		t.Fatal("state changed after failures")
	}
	wantOutcome(t, put(t, s, w, t0), OutcomeStored)
	wantReason(t, s, m.ID(), ReasonWithdrawn)
}

// D1, [R6, R7]: reopening the database gives the same answers from every
// reader.
func TestReopeningTheDatabaseGivesTheSameAnswers(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	a := testKey("a")
	m := mk(t, a, core.RecordFields{Text: "kept"})
	r := mk(t, a, core.RecordFields{Seq: 2, Prev: m.ID(), Kind: core.KindReplace, Ref: m.ID(), Version: 1, Text: "new"})
	x := mk(t, a, core.RecordFields{Seq: 3, Prev: r.ID(), Text: "will expire", Created: t0.Add(-Retention)})
	for _, rec := range []*core.VerifiedRecord{m, r, x} {
		wantOutcome(t, put(t, s, rec, t0), OutcomeStored)
	}
	if _, err := s.ExpireText(ctx, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Seen(ctx, "bob", testRoom, func(*Reader) (SeenPlan, error) {
		return SeenPlan{Reads: []Read{{m.ID(), r.ID(), m.ID()}}, Heads: map[string][]string{nodeOf(a): {m.ID()}},
			Cursors: []Cursor{{nodeOf(a), "00000000000000e1", 0, 2}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, s, "bob")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := snapshot(t, s2, "bob"); !reflect.DeepEqual(got, before) {
		t.Fatalf("after reopen:\n%+v\nbefore:\n%+v", got, before)
	}
	if len(before.Records) != 3 || len(before.Reads) != 1 {
		t.Fatalf("unexpected state %+v", before)
	}
}

// D8, review blocker 1: only a verified record can be stored; a nil or
// empty one is refused, by PutRecord and by Write, and a record whose
// signature fails cannot become verified.
func TestOnlyAVerifiedRecordCanBeStored(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	good := mk(t, testKey("a"), core.RecordFields{})
	sig := good.Sig()
	sig[0] ^= 1
	bad, err := core.RecordFromParts(good.Header(), sig, good.Body())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Verified(); err == nil {
		t.Fatal("a bad signature verified")
	}
	for _, v := range []*core.VerifiedRecord{nil, {}} {
		if _, err := s.PutRecord(ctx, v, PutOpts{Now: t0, MayRead: true}); !errors.Is(err, ErrUnverified) {
			t.Fatalf("PutRecord(%v): %v", v, err)
		}
		v := v
		if _, err := s.Write(ctx, t0, func(*Reader) (*core.VerifiedRecord, error) { return v, nil }); !errors.Is(err, ErrUnverified) {
			t.Fatalf("Write(%v): %v", v, err)
		}
	}
	if d := snapshot(t, s, "x"); len(d.Records) != 0 {
		t.Fatalf("stored %v", d.Records)
	}
}

// [R4] Write runs its build step inside the transaction and stores what it
// returns; a record that is not placed, or a failing build, writes nothing.
func TestWriteStoresWhatItsBuildStepReturnsOrNothing(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a := testKey("a")
	m1 := mk(t, a, core.RecordFields{})
	got, err := s.Write(ctx, t0, func(rd *Reader) (*core.VerifiedRecord, error) {
		ss, err := rd.Streams(testRoom)
		if err != nil || len(ss) != 0 {
			t.Errorf("build saw %v %v", ss, err)
		}
		return m1, nil
	})
	if err != nil || got.ID() != m1.ID() {
		t.Fatalf("write: %v", err)
	}
	gap := mk(t, a, core.RecordFields{Seq: 3, Prev: m1.ID()})
	var np *NotPlaced
	if _, err := s.Write(ctx, t0, func(*Reader) (*core.VerifiedRecord, error) { return gap, nil }); !errors.As(err, &np) || np.Outcome != OutcomeNotNext {
		t.Fatalf("not placed: %v", err)
	}
	boom := errors.New("build failed")
	if _, err := s.Write(ctx, t0, func(*Reader) (*core.VerifiedRecord, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("build failure: %v", err)
	}
	if d := snapshot(t, s, "x"); len(d.Records) != 1 {
		t.Fatalf("records %v", d.Records)
	}
}

// Review note: fork evidence is kept, verified header and signature, at
// most MaxForksPerPosition per position; the held record is unchanged.
func TestForkEvidenceIsKeptUpToTheLimitPerPosition(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a := testKey("a")
	m1 := mk(t, a, core.RecordFields{Text: "held"})
	wantOutcome(t, put(t, s, m1, t0), OutcomeStored)
	var forks []*core.VerifiedRecord
	for i := 0; i < MaxForksPerPosition+2; i++ {
		f := mk(t, a, core.RecordFields{Text: fmt.Sprintf("fork %d", i)})
		forks = append(forks, f)
		wantOutcome(t, put(t, s, f, t0), OutcomeFork)
	}
	wantOutcome(t, put(t, s, forks[0], t0), OutcomeFork) // again: still one copy
	var got []Fork
	if err := s.View(ctx, func(rd *Reader) (err error) {
		got, err = rd.Forks(testRoom, nodeOf(a), "00000000000000e1", 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxForksPerPosition {
		t.Fatalf("%d forks kept, want %d", len(got), MaxForksPerPosition)
	}
	for _, f := range got {
		r, err := core.RecordFromParts(f.Header, f.Sig, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Verified(); err != nil || r.ID() != f.ID {
			t.Fatalf("evidence does not verify: %v", err)
		}
	}
	wantReason(t, s, m1.ID(), ReasonLive)
}

// Review note, step 5: a stream can be known before anything of it is
// held (contig 0), is listed, and its first record is then stored.
func TestStreamKnownBeforeItsFirstRecordIsListedAndThenFilled(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a := testKey("a")
	if err := s.NoteKnown(ctx, testRoom, nodeOf(a), "00000000000000e1", 3, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.NoteKnown(ctx, testRoom, nodeOf(a), "00000000000000e1", 2, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var ss []Stream
	view := func() {
		if err := s.View(ctx, func(rd *Reader) (err error) { ss, err = rd.Streams(testRoom); return err }); err != nil {
			t.Fatal(err)
		}
	}
	view()
	if len(ss) != 1 || ss[0].Contig != 0 || ss[0].Known != 3 || !ss[0].KnownAt.Equal(t0) || ss[0].First.Record != nil {
		t.Fatalf("known stream %+v", ss)
	}
	m1 := mk(t, a, core.RecordFields{})
	wantOutcome(t, put(t, s, m1, t0), OutcomeStored)
	view()
	if ss[0].Contig != 1 || ss[0].Known != 3 || ss[0].First.Record.ID() != m1.ID() {
		t.Fatalf("after the first record %+v", ss[0])
	}
}

// Review should-fix 1: dependants are resolved when any target arrives, so
// a withdraw naming a replace is bad_reference once the replace is held,
// and drops no text.
func TestReferenceToARecordOfTheWrongKindBecomesBadWhenItArrives(t *testing.T) {
	s, _ := openTest(t)
	a := testKey("a")
	m := mk(t, a, core.RecordFields{Epoch: "00000000000000e1", Text: "v0"})
	rep := mk(t, a, core.RecordFields{Epoch: "00000000000000e2", Links: []core.Link{{Epoch: "00000000000000e1", Seq: 1, ID: m.ID()}},
		Kind: core.KindReplace, Ref: m.ID(), Version: 1, Text: "v1"})
	w := mk(t, a, core.RecordFields{Epoch: "00000000000000e3", Kind: core.KindWithdraw, Ref: rep.ID()})
	wantOutcome(t, put(t, s, w, t0), OutcomeStored)
	wantOutcome(t, put(t, s, m, t0), OutcomeStored)
	wantOutcome(t, put(t, s, rep, t0), OutcomeStored)
	if e := held(t, s, w.ID()).Effect; e != EffectBadReference {
		t.Fatalf("effect %s, want bad_reference", e)
	}
	wantReason(t, s, m.ID(), ReasonLive)
	wantReason(t, s, rep.ID(), ReasonLive)
}

// Review should-fix 6: the pruning pass uses the partial index and reads
// of one message use the reads index; a view is a read-only transaction
// that runs while another connection holds the write lock.
func TestExpiryAndConfirmationLookupsUseIndexesAndViewsDoNotWrite(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	plan := func(q string, want string) {
		t.Helper()
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN ` + q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var all []string
		for rows.Next() {
			var a, b, c int
			var d string
			if err := rows.Scan(&a, &b, &c, &d); err != nil {
				t.Fatal(err)
			}
			all = append(all, d)
		}
		for _, d := range all {
			if strings.Contains(d, want) {
				return
			}
		}
		t.Fatalf("%s: plan %v does not use %s", q, all, want)
	}
	plan(`UPDATE records SET body = NULL, reason = 'expired' WHERE reason IN ('live', 'missing') AND created <= 5`, "records_expiry")
	plan(`SELECT key, version, msg FROM reads WHERE agent = 'a' AND room = 'r' AND msg = 'm' ORDER BY key`, "reads_msg")

	wantOutcome(t, put(t, s, mk(t, testKey("a"), core.RecordFields{}), t0), OutcomeStored)
	other, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(50)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO meta VALUES ('held', 1)`); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.View(cctx, func(rd *Reader) error { _, err := rd.Streams(testRoom); return err }); err != nil {
		t.Fatalf("a view needed the write lock: %v", err)
	}
}

// D1, review should-fix 4: registering an agent starts its cursors at the
// current end of every stream of every room, once, in one transaction; a
// room added later starts registered agents at its current end too.
func TestRegisteringAnAgentStartsItsCursorsAtTheCurrentEnd(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a := testKey("a")
	m1 := mk(t, a, core.RecordFields{})
	wantOutcome(t, put(t, s, m1, t0), OutcomeStored)
	wantOutcome(t, put(t, s, mk(t, a, core.RecordFields{Seq: 2, Prev: m1.ID()}), t0), OutcomeStored)
	if fresh, err := s.Register(ctx, "dave", t0); err != nil || !fresh {
		t.Fatalf("register: %v %v", fresh, err)
	}
	want := []Cursor{{nodeOf(a), "00000000000000e1", 2, 2}}
	if d := snapshot(t, s, "dave"); !reflect.DeepEqual(d.Cursors, want) {
		t.Fatalf("cursors %+v", d.Cursors)
	}
	wantOutcome(t, put(t, s, mk(t, a, core.RecordFields{Epoch: "00000000000000e2"}), t0), OutcomeStored)
	if fresh, err := s.Register(ctx, "dave", t0); err != nil || fresh {
		t.Fatalf("second register: %v %v", fresh, err)
	}
	if d := snapshot(t, s, "dave"); !reflect.DeepEqual(d.Cursors, want) {
		t.Fatalf("cursors moved by a second registration: %+v", d.Cursors)
	}
	var ok bool
	if err := s.View(ctx, func(rd *Reader) (err error) { ok, err = rd.Registered("dave"); return }); err != nil || !ok {
		t.Fatalf("registered %v %v", ok, err)
	}
	if err := s.AddRoom(ctx, "r_ffffffffffffffffffffffffffffffff", "other", "", t0); err != nil {
		t.Fatal(err)
	}
}
