package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/wallter/a2p2/internal/core"
)

// The statements the split lookups replaced (D56), which these tests take
// as the definition of the right result.
const (
	oldDependants = `WHERE room = ? AND effect = ? AND (ref = ? OR parent = ?)`
	oldByThread   = `WHERE room = ? AND kind = 'message' AND (id = ? OR thread = ?) ORDER BY author, epoch, seq`
)

func heldIDs(hs []Held) []string {
	var out []string
	for _, h := range hs {
		out = append(out, h.Record.ID())
	}
	return out
}

// sameOnce fails unless got lists the ids of want, in want's order, and
// none twice.
func sameOnce(t *testing.T, name string, got, want []Held) {
	t.Helper()
	g, w := heldIDs(got), heldIDs(want)
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s:\n got %v\nwant %v", name, g, w)
	}
	seen := map[string]bool{}
	for _, id := range g {
		if seen[id] {
			t.Errorf("%s: %s listed twice", name, id)
		}
		seen[id] = true
	}
}

func execTest(t *testing.T, s *Store, q string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

// The second lookup of each pair leaves out in SQL what the first
// matched, so the rows read are the old statement's whatever the headers
// say: a row at the root id that is not a message is never decoded (here
// its stored header is broken), a row whose stored ref and parent are both
// the target is a dependant once, and a root whose stored thread is its own
// id is listed once.
func TestSplitLookupsReadTheOldRowsOnceWhateverTheStoredColumnsSay(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a := testKey("a")
	root := mk(t, a, core.RecordFields{Text: "root"})
	h := mk(t, a, core.RecordFields{Seq: 2, Prev: root.ID(), Kind: core.KindHandled, Ref: root.ID()})
	reply := mk(t, a, core.RecordFields{Seq: 3, Prev: h.ID(), Parent: root.ID(), Thread: root.ID(), Text: "r"})
	for _, r := range []*core.VerifiedRecord{root, h, reply} {
		wantOutcome(t, put(t, s, r, t0), OutcomeStored)
	}
	// A message whose parent is not held waits as unresolved.
	missing := mk(t, testKey("b"), core.RecordFields{Text: "not yet here"})
	dep := mk(t, a, core.RecordFields{Epoch: "00000000000000e2", Parent: missing.ID(), Thread: missing.ID(), Text: "d"})
	wantOutcome(t, put(t, s, dep, t0), OutcomeStored)

	// Stored columns that disagree with the headers.
	execTest(t, s, `UPDATE records SET hdr = x'00' WHERE room = ? AND id = ?`, testRoom, h.ID())
	execTest(t, s, `UPDATE records SET thread = ? WHERE room = ? AND id = ?`, h.ID(), testRoom, reply.ID())
	execTest(t, s, `UPDATE records SET ref = parent WHERE room = ? AND id = ?`, testRoom, dep.ID())

	err := s.View(ctx, func(rd *Reader) error {
		got, err := rd.ByThread(testRoom, h.ID())
		if err != nil {
			t.Errorf("thread at a broken handled row: %v", err)
		}
		want, err := rd.query(oldByThread, testRoom, h.ID(), h.ID())
		if err != nil {
			return err
		}
		if len(want) != 1 {
			t.Errorf("old thread statement found %d", len(want))
		}
		sameOnce(t, "thread named by a broken non-message row", got, want)

		got, err = dependants(rd, testRoom, missing.ID())
		if err != nil {
			return err
		}
		want, err = rd.query(oldDependants, testRoom, EffectUnresolved, missing.ID(), missing.ID())
		if err != nil {
			return err
		}
		if len(want) != 1 {
			t.Errorf("old dependants statement found %d", len(want))
		}
		sameOnce(t, "dependant by both ref and parent", got, want)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A root whose stored thread is its own id.
	execTest(t, s, `UPDATE records SET thread = id WHERE room = ? AND id = ?`, testRoom, root.ID())
	execTest(t, s, `UPDATE records SET thread = ? WHERE room = ? AND id = ?`, root.ID(), testRoom, reply.ID())
	err = s.View(ctx, func(rd *Reader) error {
		got, err := rd.ByThread(testRoom, root.ID())
		if err != nil {
			return err
		}
		want, err := rd.query(oldByThread, testRoom, root.ID(), root.ID())
		if err != nil {
			return err
		}
		if len(want) != 2 {
			t.Errorf("old thread statement found %d", len(want))
		}
		sameOnce(t, "root naming itself as its thread", got, want)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The dependant is resolved once its parent arrives, as before.
	wantOutcome(t, put(t, s, missing, t0), OutcomeStored)
	if e := held(t, s, dep.ID()).Effect; e != EffectOK {
		t.Fatalf("dependant effect %s, want ok", e)
	}
}

// A replacement and a withdraw by the same author that name a handled
// record, not a message, are bad references, and the withdraw drops
// nothing: the guard keeps the replacement's text.
func TestAReplacementAndAWithdrawOfAHandledRecordDropNoText(t *testing.T) {
	s, _ := openTest(t)
	a := testKey("a")
	m := mk(t, a, core.RecordFields{Text: "m"})
	h := mk(t, a, core.RecordFields{Seq: 2, Prev: m.ID(), Kind: core.KindHandled, Ref: m.ID()})
	rep := mk(t, a, core.RecordFields{Seq: 3, Prev: h.ID(), Kind: core.KindReplace, Ref: h.ID(), Version: 1, Text: "v1"})
	w := mk(t, a, core.RecordFields{Seq: 4, Prev: rep.ID(), Kind: core.KindWithdraw, Ref: h.ID()})
	for _, r := range []*core.VerifiedRecord{m, h, rep, w} {
		wantOutcome(t, put(t, s, r, t0), OutcomeStored)
	}
	for _, id := range []string{rep.ID(), w.ID()} {
		if e := held(t, s, id).Effect; e != EffectBadReference {
			t.Errorf("%s effect %s, want bad_reference", id, e)
		}
	}
	wantReason(t, s, m.ID(), ReasonLive)
	wantReason(t, s, h.ID(), ReasonLive)
	wantReason(t, s, rep.ID(), ReasonLive)
}

// rowOf is a stored row's reason, effect and text, read without decoding.
func rowOf(t *testing.T, s *Store, room, id string) [3]string {
	t.Helper()
	var reason, effect string
	var body []byte
	if err := s.db.QueryRow(`SELECT reason, effect, body FROM records WHERE room = ? AND id = ?`, room, id).
		Scan(&reason, &effect, &body); err != nil {
		t.Fatal(err)
	}
	return [3]string{reason, effect, string(body)}
}

// Records of another room that name the same id are neither read nor
// changed: a withdraw there does not withdraw the message here, the
// message's arrival here resolves nothing there, and a withdraw here drops
// no replacement there.
func TestLookupsByRefParentAndThreadStayInTheirRoom(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	// other holds a replacement and a reply naming m; third a withdraw of
	// it by the same author.
	other, third := "r_ffeeddccbbaa99887766554433221100", "r_00ffeeddccbbaa998877665544332211"
	for _, r := range []string{other, third} {
		if err := s.AddRoom(ctx, r, "other", "", t0); err != nil {
			t.Fatal(err)
		}
	}
	a := testKey("a")
	m := mk(t, a, core.RecordFields{Text: "m"})
	rep2 := mk(t, a, core.RecordFields{Room: other, Kind: core.KindReplace, Ref: m.ID(), Version: 1, Text: "v1"})
	dep2 := mk(t, a, core.RecordFields{Room: other, Seq: 2, Prev: rep2.ID(), Parent: m.ID(), Thread: m.ID(), Text: "d"})
	w3 := mk(t, a, core.RecordFields{Room: third, Kind: core.KindWithdraw, Ref: m.ID()})
	for _, r := range []*core.VerifiedRecord{rep2, dep2, w3} {
		wantOutcome(t, put(t, s, r, t0), OutcomeStored)
	}
	before := map[string][3]string{}
	for _, r := range []*core.VerifiedRecord{rep2, dep2} {
		before[r.ID()] = rowOf(t, s, other, r.ID())
	}
	if before[rep2.ID()][0] != ReasonLive || before[rep2.ID()][1] != EffectUnresolved || before[dep2.ID()][1] != EffectUnresolved {
		t.Fatalf("other room before: %v", before)
	}
	wantOutcome(t, put(t, s, m, t0), OutcomeStored)
	wantReason(t, s, m.ID(), ReasonLive) // the third room's withdraw does not count here
	err := s.View(ctx, func(rd *Reader) error {
		for name, f := range map[string]func() ([]Held, error){
			"dependants": func() ([]Held, error) { return dependants(rd, testRoom, m.ID()) },
			"by ref":     func() ([]Held, error) { return rd.ByRef(testRoom, m.ID()) },
			"by parent":  func() ([]Held, error) { return rd.ByParent(testRoom, m.ID()) },
			"by thread":  func() ([]Held, error) { return rd.ByThread(testRoom, m.ID()) },
		} {
			hs, err := f()
			if err != nil {
				return err
			}
			for _, h := range hs {
				if h.Record.Fields().Room != testRoom {
					t.Errorf("%s read %s of another room", name, h.Record.ID())
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w := mk(t, a, core.RecordFields{Seq: 2, Prev: m.ID(), Kind: core.KindWithdraw, Ref: m.ID()})
	wantOutcome(t, put(t, s, w, t0), OutcomeStored)
	wantReason(t, s, m.ID(), ReasonWithdrawn)
	for _, r := range []*core.VerifiedRecord{rep2, dep2} {
		if got := rowOf(t, s, other, r.ID()); got != before[r.ID()] {
			t.Errorf("other room's %s changed: %v, was %v", r.ID(), got, before[r.ID()])
		}
	}
}
