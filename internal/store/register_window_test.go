package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// D73: a registration reads the messages among the newest
// RegisterWindow records by a seek on the rowid, the order this machine
// stored its records in, never by a scan of the table.
func TestTheNewestRecordsAreReadBySeekingOnTheRowid(t *testing.T) {
	s, _ := openTest(t)
	plan := queryPlan(t, s, sqlNewestMessages)
	var seek bool
	for _, st := range plan {
		if strings.HasPrefix(st, "SCAN records") || strings.Contains(st, "TEMP B-TREE") {
			t.Fatalf("plan %q", plan)
		}
		seek = seek || strings.HasPrefix(st, "SEARCH records USING INTEGER PRIMARY KEY (rowid>?)")
	}
	if !seek {
		t.Fatalf("plan %q does not seek on the rowid", plan)
	}
}

// bulkStream fills stream (a, epoch e1) from seq from to to with copies of
// the stored row of tmpl, and with the header of brief at seq at, in one
// statement: a history far larger than the window, without signing each
// record. A registration reads only the stream columns and the header.
func bulkStream(t *testing.T, s *Store, tmpl, brief string, from, to, at int64) {
	t.Helper()
	_, err := s.db.Exec(`WITH RECURSIVE c(n) AS (SELECT ? UNION ALL SELECT n + 1 FROM c WHERE n < ?)
		INSERT INTO records (room, id, author, epoch, seq, kind, ref, parent, thread, agent, created, hdr, sig, body, reason, effect)
		SELECT t.room, t.id || '-' || c.n, t.author, t.epoch, c.n, t.kind, t.ref, t.parent, t.thread, t.agent, t.created,
			CASE WHEN c.n = ? THEN (SELECT hdr FROM records WHERE id = ?) ELSE t.hdr END, t.sig, t.body, t.reason, t.effect
		FROM c, records t WHERE t.id = ?`, from, to, at, brief, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE streams SET contig = ? WHERE room = ?`, to, testRoom); err != nil {
		t.Fatal(err)
	}
}

// [U3] With 100,000 records held and briefs at positions 10 and 95,000, a
// registration decodes at most RegisterWindow headers: the brief at 10,
// outside the window, is not unread for its agent, whose cursor starts at
// the end as before D73; the brief inside it is, for its own agent; and
// nothing else held changes.
func TestRegisteringReadsOnlyTheNewestRecordsWhateverTheHistory(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	self := nodeOf(testKey("self"))
	a := testKey("a")
	var prev, tmpl string
	var briefs []string
	for i := int64(1); i <= 10; i++ {
		f := core.RecordFields{Seq: i, Prev: prev}
		switch i {
		case 9:
			f.To = []core.Addressee{{Node: self, Agent: "erin"}}
		case 10:
			f.To = []core.Addressee{{Node: self, Agent: "dave"}}
		}
		r := mk(t, a, f)
		wantOutcome(t, put(t, s, r, t0), OutcomeStored)
		prev = r.ID()
		if i == 8 {
			tmpl = r.ID()
		}
		if i >= 9 {
			briefs = append(briefs, r.ID())
		}
	}
	const total = 100000
	bulkStream(t, s, tmpl, briefs[0], 11, total, 95000)
	if _, err := s.Register(ctx, self, "carol", t0); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, s, "carol")
	var decoded []int
	seedAddressedHook = func(n int) { decoded = append(decoded, n) }
	defer func() { seedAddressedHook = nil }()
	for _, agent := range []string{"dave", "erin"} {
		at := time.Now()
		if _, err := s.Register(ctx, self, agent, t0); err != nil {
			t.Fatal(err)
		}
		t.Logf("registering %s with %d records held took %v", agent, total, time.Since(at))
	}
	if !reflect.DeepEqual(decoded, []int{RegisterWindow, RegisterWindow}) {
		t.Fatalf("decoded %v headers, want %d each", decoded, RegisterWindow)
	}
	e1 := "00000000000000e1"
	if got := snapshot(t, s, "dave").Cursors; !reflect.DeepEqual(got, []Cursor{{nodeOf(a), e1, total, total, total}}) {
		t.Fatalf("dave: %+v", got)
	}
	if got := snapshot(t, s, "erin").Cursors; !reflect.DeepEqual(got, []Cursor{{nodeOf(a), e1, total, 94999, 94999}}) {
		t.Fatalf("erin: %+v", got)
	}
	if after := snapshot(t, s, "carol"); !reflect.DeepEqual(after, before) {
		t.Fatalf("carol's state changed:\n%+v\n%+v", before, after)
	}
}
