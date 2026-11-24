package store

import (
	"context"
	"errors"
	"testing"

	"github.com/wallter/a2p2/internal/core"
)

const logRoom = "r_ffeeddccbbaa99887766554433221100"

func testCreate(t *testing.T) LogRow {
	t.Helper()
	k := testKey("creator")
	e, err := core.NewEntry(k, core.EntryFields{Room: logRoom, Seq: 1, Kind: core.EntryCreate, Label: "team", Created: t0})
	if err != nil {
		t.Fatal(err)
	}
	return LogRow{ID: e.ID(), Author: e.Author(), Seq: 1, Header: e.Header(), Sig: e.Sig()}
}

// A log batch is one transaction: a failure after its rows are written
// leaves no row, no member cache change and no approval behind.
//
// Requirements: DESIGN-next section 5 (one transaction each)
func TestLogBatchThatFailsLeavesNothingApplied(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	root := testCreate(t)
	gen, err := s.CreateLogRoom(ctx, logRoom, "team", root, []RoomMember{{Node: root.Author, Member: true, Admin: true, Admitted: true}}, t0)
	if err != nil || gen != 1 {
		t.Fatalf("create: %d %v", gen, err)
	}
	q := Request{ID: "q_1", Kind: "add", Room: logRoom, Node: nodeOf(testKey("b")), Name: "bee", Created: t0}
	if err := s.CreateRequest(ctx, q); err != nil {
		t.Fatal(err)
	}
	row := root
	row.ID, row.State = "00000000000000000000000000000001", LogAccepted
	b := LogBatch{Room: logRoom, Gen: gen, New: []LogRow{row}, Members: []RoomMember{}, Complete: q.ID, Entries: row.ID, Now: t0}
	testHook = func(p string) error {
		if p == "log:access" {
			return errors.New("injected")
		}
		return nil
	}
	_, err = s.ApplyLog(ctx, b)
	testHook = nil
	if err == nil {
		t.Fatal("injected failure not returned")
	}
	rows, _ := s.Log(ctx, logRoom)
	ms, _ := s.RoomMembers(ctx, logRoom)
	got, _ := s.GetRequest(ctx, q.ID)
	if len(rows) != 1 || len(ms) != 1 || got.State != RequestPending {
		t.Fatalf("half applied: %d rows, %d members, request %s", len(rows), len(ms), got.State)
	}
	if _, err := s.ApplyLog(ctx, b); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetRequest(ctx, q.ID); got.State != RequestApproved || got.Entries != row.ID {
		t.Fatalf("approval: %+v", got)
	}
	// Leaving moves the generation; a batch prepared before is refused.
	if g, err := s.LeaveRoom(ctx, logRoom, t0); err != nil || g != 2 {
		t.Fatalf("leave: %d %v", g, err)
	}
	if _, err := s.ApplyLog(ctx, b); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("after leave: %v", err)
	}
	if rows, _ := s.Log(ctx, logRoom); len(rows) != 0 {
		t.Fatal("log kept after leaving")
	}
	if err := s.AddRoom(ctx, logRoom, "team", "", t0); err != nil {
		t.Fatal(err)
	}
	if lr, _ := s.LogRoom(ctx, logRoom); !lr.Left {
		t.Fatal("AddRoom revived a left room")
	}
}

// A database created by step 3a (schema version 2) is upgraded by
// migration 3, which adds the log; migration 2 is left as it shipped.
//
// Requirements: AGENTS.md rule 5 (shipped migrations are never edited)
func TestDatabaseAtVersionTwoUpgradesToTheLogSchema(t *testing.T) {
	path := t.TempDir() + "/a2p2.db"
	saved := migrations
	migrations = saved[:2]
	s, err := Open(path)
	migrations = saved
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.AddRoom(ctx, logRoom, "team", "", t0); err != nil {
		t.Fatal(err)
	}
	var v int
	s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	s.Close()
	if v != 2 {
		t.Fatalf("version %d", v)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != len(migrations) {
		t.Fatalf("after upgrade: version %d", v)
	}
	if _, err := s.LogRooms(ctx); err != nil {
		t.Fatal(err)
	}
	if lr, err := s.LogRoom(ctx, logRoom); err != nil || lr.Left || lr.Gen != 1 {
		t.Fatalf("room kept: %+v %v", lr, err)
	}
	if _, err := s.LeaveRoom(ctx, logRoom, t0); err != nil {
		t.Fatal(err)
	}
}
