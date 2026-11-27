//go:build bench

package bench

// The price of a durable commit. BenchmarkSQLiteDurability opens a database
// the way store.Open does (same driver and pragmas) and times one committed
// transaction of a message-sized row, or of 10 or 100 rows, under the
// store's setting and the alternatives. It reports the p50 and p95 of one
// commit in microseconds and the commits per second. On macOS plain fsync(2)
// does not flush the drive's cache; PRAGMA fullfsync=ON makes SQLite use
// fcntl F_FULLFSYNC (modernc.org/sqlite v1.63.0, _full_fsync in the darwin
// build). On Linux the pragma changes nothing: fsync already flushes.
// Test only: nothing here changes the store.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/wallter/a2p2/internal/store"
)

func BenchmarkSQLiteDurability(b *testing.B) {
	// store-FULL is what internal/store/store.go Open sets: WAL and
	// synchronous=FULL, with neither fullfsync pragma.
	settings := []struct{ name, pragmas string }{
		{"store-FULL", "&_pragma=synchronous(FULL)"},
		{"FULL+fullfsync", "&_pragma=synchronous(FULL)&_pragma=fullfsync(1)"},
		{"FULL+fullfsync+ckptfullfsync", "&_pragma=synchronous(FULL)&_pragma=fullfsync(1)&_pragma=checkpoint_fullfsync(1)"},
		{"NORMAL", "&_pragma=synchronous(NORMAL)"},
	}
	for _, s := range settings {
		for _, rows := range []int{1, 10, 100} {
			b.Run(fmt.Sprintf("%s/rows=%d", s.name, rows), func(b *testing.B) {
				dir := tmpHome(b)
				defer os.RemoveAll(dir)
				dsn := store.FileURI(filepath.Join(dir, "x.db")) + "?_txlock=immediate&_pragma=journal_mode(WAL)" +
					s.pragmas + "&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=secure_delete(1)"
				db, err := sql.Open("sqlite", dsn)
				if err != nil {
					b.Fatal(err)
				}
				defer db.Close()
				db.SetMaxOpenConns(1)
				var ff, sy int
				if err := db.QueryRow(`PRAGMA fullfsync`).Scan(&ff); err != nil {
					b.Fatal(err)
				}
				if err := db.QueryRow(`PRAGMA synchronous`).Scan(&sy); err != nil {
					b.Fatal(err)
				}
				b.Logf("readback: synchronous=%d fullfsync=%d", sy, ff)
				if _, err := db.Exec(`CREATE TABLE t (n INTEGER PRIMARY KEY, body BLOB)`); err != nil {
					b.Fatal(err)
				}
				row := make([]byte, 600) // about one signed record with a short text
				lat := make([]float64, 0, b.N)
				b.ResetTimer()
				start := time.Now()
				for b.Loop() {
					t0 := time.Now()
					tx, err := db.Begin()
					if err != nil {
						b.Fatal(err)
					}
					for i := 0; i < rows; i++ {
						if _, err := tx.Exec(`INSERT INTO t (body) VALUES (?)`, row); err != nil {
							b.Fatal(err)
						}
					}
					if err := tx.Commit(); err != nil {
						b.Fatal(err)
					}
					lat = append(lat, float64(time.Since(t0).Microseconds()))
				}
				el := time.Since(start)
				sort.Float64s(lat)
				if len(lat) > 0 {
					b.ReportMetric(lat[len(lat)/2], "p50-us/commit")
					b.ReportMetric(lat[len(lat)*95/100], "p95-us/commit")
					b.ReportMetric(float64(len(lat))/el.Seconds(), "commits/s")
					b.ReportMetric(float64(len(lat)*rows)/el.Seconds(), "records/s")
				}
			})
		}
	}
}
