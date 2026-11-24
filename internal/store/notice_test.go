package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
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
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(b)); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestV02StateIsDetectedInASchemaOneDatabaseAndShownOnceEver(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		strip string // v0.2 state removed before opening
	}{
		{"messages and rooms", ""},
		{"rooms only", `DELETE FROM messages`},
		{"messages only", `DELETE FROM members; DELETE FROM rooms`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := loadV02(t)
			if tc.strip != "" {
				db, err := sql.Open("sqlite", "file:"+path)
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
			for i := 0; i < 2; i++ { // asking does not consume it
				if p, err := s.V02Pending(ctx); err != nil || !p {
					t.Fatalf("pending %d = %v, %v", i, p, err)
				}
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
			var n int
			if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM messages) + (SELECT COUNT(*) FROM rooms)`).Scan(&n); err != nil || n == 0 && tc.strip == "" {
				t.Fatalf("v0.2 rows were deleted: %d %v", n, err)
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
}
