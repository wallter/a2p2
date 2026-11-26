package store

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/core"
)

// queryPlan returns the detail lines of EXPLAIN QUERY PLAN for q, with
// every parameter bound to NULL.
func queryPlan(t *testing.T, s *Store, q string) []string {
	t.Helper()
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+q, make([]any, strings.Count(q, "?"))...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, notused int
		var d string
		if err := rows.Scan(&id, &parent, &notused, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// recordsStep matches a plan step over the records table, under its name
// or the alias t the withdraw guard uses: "SCAN records ..." or "SEARCH t
// USING [COVERING] INDEX name (room=? AND ...)".
var recordsStep = regexp.MustCompile(`^(SCAN|SEARCH) (records|t)\b.*?(?:\((.*)\))?$`)

// step is one expected plan step over records: the table or alias, and the
// key column its index constraint must name.
type step struct{ table, key string }

// M7 (perf 2026-11-25): the lookups made for every stored record (its
// dependants, whether it was withdrawn, what a withdraw drops) and for
// every message an inbox shows (what refers to it), and the reply and
// thread views, seek on the column they look up, so their cost is the
// number of matching records and not the size of the room's history.
// Before, each read every record of the room: the planner, which has no
// statistics, took the index of (room) that matched an ORDER BY or a
// (room, kind) index over the index of the looked-up key. Every step over
// records, including the withdraw guard's subquery, must be a SEARCH whose
// constraint names the key.
func TestPerRecordLookupsSeekOnTheirKeyNotOnTheWholeRoom(t *testing.T) {
	s, _ := openTest(t)
	sel := `SELECT ` + heldCols + ` FROM records `
	pos := `SELECT ` + positionedCols + ` FROM records `
	for _, c := range []struct {
		name, q string
		steps   []step
	}{
		{"dependants by ref", sel + whereDependantsByRef, []step{{"records", "ref"}}},
		{"dependants by parent", sel + whereDependantsByParent, []step{{"records", "parent"}}},
		{"withdraws of a message", sqlWithdrawsOf, []step{{"records", "ref"}}},
		{"by ref", pos + whereByRef, []step{{"records", "ref"}}},
		{"by parent", pos + whereByParent, []step{{"records", "parent"}}},
		{"by thread", pos + whereByThread, []step{{"records", "thread"}}},
		{"thread root", pos + whereThreadRoot, []step{{"records", "id"}}},
		{"withdraw a message", sqlWithdrawMessage, []step{{"records", "id"}}},
		{"withdraw its replacements", sqlWithdrawReplaces, []step{{"records", "ref"}, {"t", "id"}}},
	} {
		plan := queryPlan(t, s, c.q)
		var got []step
		for _, st := range plan {
			m := recordsStep.FindStringSubmatch(st)
			if m == nil {
				continue
			}
			i := len(got)
			got = append(got, step{m[2], ""})
			if i >= len(c.steps) {
				t.Errorf("%s: unexpected step %q; plan %q", c.name, st, plan)
				continue
			}
			want := c.steps[i]
			if m[1] == "SCAN" || m[2] != want.table || !regexp.MustCompile(`\b`+want.key+`=`).MatchString(m[3]) {
				t.Errorf("%s: step %q is not a search of %s by %s; plan %q", c.name, st, want.table, want.key, plan)
			}
		}
		if len(got) != len(c.steps) {
			t.Errorf("%s: %d steps over records, want %d; plan %q", c.name, len(got), len(c.steps), plan)
		}
	}
}

// The lookups return what they did when they read the room in SQL order:
// the same records in the order of their positions (author, epoch, seq).
func TestRefParentAndThreadLookupsKeepThePositionOrder(t *testing.T) {
	s, _ := openTest(t)
	keys := []string{"c", "a", "b"}
	root := mk(t, testKey("a"), core.RecordFields{Epoch: "00000000000000f1", Text: "root"})
	wantOutcome(t, put(t, s, root, t0), OutcomeStored)
	reply := mk(t, testKey("b"), core.RecordFields{Epoch: "00000000000000f1", Parent: root.ID(), Thread: root.ID(), Text: "r"})
	wantOutcome(t, put(t, s, reply, t0), OutcomeStored)
	// Three authors, two epochs each, several positions: handled records
	// naming root, replies to root and to the reply, all in root's thread.
	for _, k := range keys {
		for _, ep := range []string{"00000000000000e2", "00000000000000e1"} {
			prev := ""
			for seq := int64(1); seq <= 4; seq++ {
				f := core.RecordFields{Epoch: ep, Seq: seq, Prev: prev, Thread: root.ID(), Text: "x"}
				switch seq % 4 {
				case 0:
					f = core.RecordFields{Epoch: ep, Seq: seq, Prev: prev, Kind: core.KindHandled, Ref: root.ID(), Agent: "bob"}
				case 1:
					f.Parent = root.ID()
				case 2:
					f.Parent = reply.ID()
				case 3:
					f = core.RecordFields{Epoch: ep, Seq: seq, Prev: prev, Kind: core.KindHandled, Ref: reply.ID(), Agent: "bob"}
				}
				r := mk(t, testKey(k), f)
				wantOutcome(t, put(t, s, r, t0), OutcomeStored)
				prev = r.ID()
			}
		}
	}
	ids := func(hs []Held) []string {
		var out []string
		for _, h := range hs {
			out = append(out, h.Record.ID())
		}
		return out
	}
	err := s.View(context.Background(), func(rd *Reader) error {
		for _, c := range []struct {
			name  string
			got   func() ([]Held, error)
			where string
			args  []any
		}{
			{"by ref root", func() ([]Held, error) { return rd.ByRef(testRoom, root.ID()) },
				`WHERE room = ? AND ref = ? ORDER BY author, epoch, seq`, []any{testRoom, root.ID()}},
			{"by ref reply", func() ([]Held, error) { return rd.ByRef(testRoom, reply.ID()) },
				`WHERE room = ? AND ref = ? ORDER BY author, epoch, seq`, []any{testRoom, reply.ID()}},
			{"by parent root", func() ([]Held, error) { return rd.ByParent(testRoom, root.ID()) },
				`WHERE room = ? AND parent = ? AND kind = 'message' ORDER BY author, epoch, seq`, []any{testRoom, root.ID()}},
			{"by parent reply", func() ([]Held, error) { return rd.ByParent(testRoom, reply.ID()) },
				`WHERE room = ? AND parent = ? AND kind = 'message' ORDER BY author, epoch, seq`, []any{testRoom, reply.ID()}},
			{"thread", func() ([]Held, error) { return rd.ByThread(testRoom, root.ID()) },
				`WHERE room = ? AND kind = 'message' AND (id = ? OR thread = ?) ORDER BY author, epoch, seq`,
				[]any{testRoom, root.ID(), root.ID()}},
			{"thread of a reply", func() ([]Held, error) { return rd.ByThread(testRoom, reply.ID()) },
				`WHERE room = ? AND kind = 'message' AND (id = ? OR thread = ?) ORDER BY author, epoch, seq`,
				[]any{testRoom, reply.ID(), reply.ID()}},
		} {
			got, err := c.got()
			if err != nil {
				return err
			}
			want, err := rd.query(c.where, c.args...)
			if err != nil {
				return err
			}
			if len(want) < 2 && c.name != "thread of a reply" {
				t.Errorf("%s: only %d records, the order is not tested", c.name, len(want))
			}
			if !reflect.DeepEqual(ids(got), ids(want)) {
				t.Errorf("%s:\n got %v\nwant %v", c.name, ids(got), ids(want))
			}
			for i := range got {
				if i < len(want) && (got[i].Reason != want[i].Reason || got[i].Effect != want[i].Effect ||
					!reflect.DeepEqual(got[i].Record.Header(), want[i].Record.Header())) {
					t.Errorf("%s: record %d differs", c.name, i)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
