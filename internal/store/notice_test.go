package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// loadV02 writes the schema-version-1 fixture, a database as v0.2 left
// it, to a fresh file and returns its path.
func loadV02(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "v02-schema1.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "a2p2.db")
	db, err := sql.Open("sqlite", fileURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(b)); err != nil {
		t.Fatal(err)
	}
	return path
}

// tables lists the tables of s.
func tables(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// TestMigrationTenRemovesTheV02DataAndTheNoticeIsShownOnceEver opens a real
// schema-version-1 file holding v0.2 messages and rooms: migration 10
// drops the v0.2 tables and rooms (the agents stay), the notice is due
// once, and once shown never again, across a restart.
func TestMigrationTenRemovesTheV02DataAndTheNoticeIsShownOnceEver(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		strip string // v0.2 state removed before opening
		due   bool
	}{
		{"messages and rooms", "", true},
		{"rooms only", `DELETE FROM messages`, true},
		{"messages only", `DELETE FROM members; DELETE FROM rooms`, true},
		{"neither", `DELETE FROM messages; DELETE FROM members; DELETE FROM rooms`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := loadV02(t)
			if tc.strip != "" {
				db, err := sql.Open("sqlite", fileURI(path))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(tc.strip); err != nil {
					t.Fatal(err)
				}
				db.Close()
			}
			s, err := Open(path) // migrates 1 -> current
			if err != nil {
				t.Fatal(err)
			}
			for _, gone := range []string{"messages", "deliveries", "marks", "members", "invites"} {
				if slices.Contains(tables(t, s), gone) {
					t.Fatalf("table %s is still there: %v", gone, tables(t, s))
				}
			}
			var rooms, seq int
			if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM rooms), (SELECT COUNT(*) FROM meta WHERE key = 'seq')`).Scan(&rooms, &seq); err != nil || rooms != 0 || seq != 0 {
				t.Fatalf("v0.2 rooms %d, seq rows %d: %v", rooms, seq, err)
			}
			if as, err := s.Agents(ctx); err != nil || len(as) != 1 || as[0].ID != "claude-x@abababab" {
				t.Fatalf("agents = %+v %v", as, err)
			}
			for i := 0; i < 2; i++ { // asking does not consume it
				if p, err := s.V02Pending(ctx); err != nil || p != tc.due {
					t.Fatalf("pending %d = %v, %v", i, p, err)
				}
			}
			if !tc.due {
				s.Close()
				return
			}
			if first, err := s.V02Shown(ctx); err != nil || !first {
				t.Fatalf("shown = %v, %v", first, err)
			}
			if first, _ := s.V02Shown(ctx); first {
				t.Fatal("shown twice")
			}
			s.Close()
			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if p, _ := s.V02Pending(ctx); p {
				t.Fatal("pending again after a restart")
			}
		})
	}
	s, err := Open(filepath.Join(t.TempDir(), "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if p, _ := s.V02Pending(ctx); p {
		t.Fatal("pending on a new database")
	}
	if ts := tables(t, s); slices.Contains(ts, "messages") || !slices.Contains(ts, "agents") || !slices.Contains(ts, "meta") {
		t.Fatalf("tables of a new database: %v", ts)
	}
}
