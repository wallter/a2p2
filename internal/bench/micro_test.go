//go:build bench

package bench

// In-process micro-benchmarks of the parts of one local send. The
// Surface and the store are the product's own (store.Open, node.OpenSurface,
// localapi.Handler); only the raw SQLite and fsync benchmarks open files
// of their own, with settings named in their names, to price the commit.

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

var sizes = []int{200, 4096, 16300}

func benchKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
}

func fields(size int) core.RecordFields {
	return core.RecordFields{Room: "r_" + fmt.Sprintf("%032x", 7), Epoch: "00000000000000a1", Seq: 1,
		Kind: core.KindMessage, Agent: "alice", Created: time.Now(), Text: text(size)}
}

// ---- signing and the record encoding ----

func BenchmarkEd25519Sign(b *testing.B) {
	k := benchKey()
	r, _ := core.NewRecord(k, fields(200))
	h := r.Header()
	b.SetBytes(int64(len(h)))
	b.ReportMetric(float64(len(h)), "header-bytes")
	for b.Loop() {
		ed25519.Sign(k, h)
	}
}

func BenchmarkEd25519Verify(b *testing.B) {
	k := benchKey()
	r, _ := core.NewRecord(k, fields(200))
	h, sig := r.Header(), r.Sig()
	pub := k.Public().(ed25519.PublicKey)
	for b.Loop() {
		if !ed25519.Verify(pub, h, sig) {
			b.Fatal("bad")
		}
	}
}

// BenchmarkRecordNew is what a send does to make its record: encode, sign,
// parse back (core.NewRecord), then verify its own signature (Verified).
func BenchmarkRecordNewAndVerified(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			k := benchKey()
			f := fields(n)
			for b.Loop() {
				r, err := core.NewRecord(k, f)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := r.Verified(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRecordParse is the decode and syntax checks of a finished wire
// record (what a receiver does before it verifies); NewRecord runs the
// same parse on its own output.
func BenchmarkRecordParse(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			r, _ := core.NewRecord(benchKey(), fields(n))
			w := r.Wire()
			b.SetBytes(int64(len(w)))
			for b.Loop() {
				if _, err := core.ParseRecord(w); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkRecordWire(b *testing.B) {
	r, _ := core.NewRecord(benchKey(), fields(200))
	for b.Loop() {
		_ = r.Wire()
	}
}

// ---- the commit: raw SQLite (modernc, as the store) and fsync ----

// benchSQLite times one write transaction of one ~1 KB row on a WAL
// database with the given synchronous setting. FULL is what store.Open
// uses; NORMAL and OFF are for comparison only (test only).
func BenchmarkSQLiteCommit(b *testing.B) {
	for _, sync := range []string{"FULL", "NORMAL", "OFF"} {
		b.Run(sync, func(b *testing.B) {
			dir := tmpHome(b)
			defer os.RemoveAll(dir)
			dsn := "file:" + filepath.Join(dir, "x.db") + "?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=synchronous(" + sync + ")&_pragma=busy_timeout(5000)&_pragma=secure_delete(1)"
			db, err := sql.Open("sqlite", dsn)
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			if _, err := db.Exec(`CREATE TABLE t (n INTEGER PRIMARY KEY, body BLOB)`); err != nil {
				b.Fatal(err)
			}
			row := make([]byte, 1024)
			for b.Loop() {
				tx, err := db.Begin()
				if err != nil {
					b.Fatal(err)
				}
				if _, err := tx.Exec(`INSERT INTO t (body) VALUES (?)`, row); err != nil {
					b.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFsync prices a 4 KB append and sync of a file in /tmp: with
// fsync(2) (what SQLite calls without PRAGMA fullfsync) and with
// os.File.Sync, which on macOS is fcntl F_FULLFSYNC (a flush of the
// drive's cache).
func BenchmarkFsync(b *testing.B) {
	for _, how := range []string{"fsync", "File.Sync"} {
		b.Run(how, func(b *testing.B) {
			dir := tmpHome(b)
			defer os.RemoveAll(dir)
			f, err := os.Create(filepath.Join(dir, "f"))
			if err != nil {
				b.Fatal(err)
			}
			defer f.Close()
			buf := make([]byte, 4096)
			for b.Loop() {
				if _, err := f.Write(buf); err != nil {
					b.Fatal(err)
				}
				if how == "fsync" {
					err = syscall.Fsync(int(f.Fd()))
				} else {
					err = f.Sync()
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// ---- the product's own surface, in process ----

type inproc struct {
	dir   string
	st    *store.Store
	surf  *node.Surface
	agent string
	c     *localapi.Client
	srv   *http.Server
}

func newInproc(b testing.TB) *inproc {
	b.Helper()
	ctx := context.Background()
	x := &inproc{dir: tmpHome(b)}
	b.Cleanup(func() { os.RemoveAll(x.dir) })
	st, err := store.Open(filepath.Join(x.dir, "a2p2.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { st.Close() })
	x.st = st
	_, key, _ := ed25519.GenerateKey(nil)
	m := node.NewMachine(st, key, node.MachineOpts{})
	rooms, err := node.OpenRooms(ctx, m)
	if err != nil {
		b.Fatal(err)
	}
	if x.surf, err = node.OpenSurface(ctx, m, rooms, node.SurfaceOpts{Name: "bench"}); err != nil {
		b.Fatal(err)
	}
	a, err := x.surf.Register(ctx, "bench", "alice", "/w")
	if err != nil {
		b.Fatal(err)
	}
	x.agent = a.ID
	sock := filepath.Join(x.dir, "sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		b.Fatal(err)
	}
	x.srv = &http.Server{Handler: localapi.Handler(x.surf, localapi.Info{Version: "bench"}), ReadHeaderTimeout: 5 * time.Second}
	go x.srv.Serve(ln)
	b.Cleanup(func() { x.srv.Close() })
	x.c = localapi.NewClient(sock)
	return x
}

// fresh is how many sends a benchmark makes into one store before it
// starts a new one: a send's cost grows with the room's history (see
// BenchmarkSurfaceSendAtHistory), so the small-room numbers are taken over
// a history of 0 to fresh messages.
const fresh = 100

func (x *inproc) close() {
	x.srv.Close()
	x.st.Close()
	os.RemoveAll(x.dir)
}

// benchSend times send over b.N sends, a new store every fresh sends.
func benchSend(b *testing.B, send func(x *inproc) error) {
	x := newInproc(b)
	for i := 0; i < b.N; i++ {
		if i > 0 && i%fresh == 0 {
			b.StopTimer()
			x.close()
			x = newInproc(b)
			b.StartTimer()
		}
		if err := send(x); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	x.close()
}

// BenchmarkSurfaceSend is node.Surface.Send with no HTTP: resolve, the
// write transaction (view, sign, verify, place, commit with its fsync)
// and the room label; history 0 to 100.
func BenchmarkSurfaceSend(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			ctx := context.Background()
			msg := text(n)
			benchSend(b, func(x *inproc) error {
				_, err := x.surf.Send(ctx, node.SendInput{Agent: x.agent, Text: msg})
				return err
			})
		})
	}
}

// BenchmarkSocketSend is the same send through localapi's client and
// handler over a Unix socket, both in this process; history 0 to 100.
func BenchmarkSocketSend(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			ctx := context.Background()
			msg := text(n)
			benchSend(b, func(x *inproc) error {
				var m localapi.SendResp
				return x.c.Call(ctx, "send", localapi.SendReq{Agent: x.agent, Text: msg}, &m)
			})
		})
	}
}

// BenchmarkSurfaceSendAtHistory is Surface.Send of 200 B into a room that
// already holds h messages of 200 B. Only the first 100 iterations send
// (so the history stays within h..h+100) and the result is the metric
// ns/send; ns/op means nothing here.
func BenchmarkSurfaceSendAtHistory(b *testing.B) {
	for _, h := range []int{0, 1000, 5000} {
		b.Run(fmt.Sprint(h), func(b *testing.B) {
			x := newInproc(b)
			ctx := context.Background()
			in := node.SendInput{Agent: x.agent, Text: text(200)}
			for i := 0; i < h; i++ {
				if _, err := x.surf.Send(ctx, in); err != nil {
					b.Fatal(err)
				}
			}
			k := min(b.N, 100)
			b.ResetTimer()
			t0 := time.Now()
			for i := 0; i < k; i++ {
				if _, err := x.surf.Send(ctx, in); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(time.Since(t0).Nanoseconds())/float64(k), "ns/send")
		})
	}
}

// BenchmarkSocketFloor is the cheapest round trip: an unknown call,
// answered 404 by the mux. HTTP over the Unix socket, nothing else.
func BenchmarkSocketFloor(b *testing.B) {
	x := newInproc(b)
	ctx := context.Background()
	for b.Loop() {
		_ = x.c.Call(ctx, "no-such-call", nil, nil)
	}
}

// BenchmarkSocketRequests is the cheapest real call: requests (one read
// of the requests table) with its JSON both ways.
func BenchmarkSocketRequests(b *testing.B) {
	x := newInproc(b)
	ctx := context.Background()
	for b.Loop() {
		var r localapi.RequestsResp
		if err := x.c.Call(ctx, "requests", localapi.Empty{}, &r); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSocketInboxEmpty is an inbox with nothing unread, every room,
// no wait.
func BenchmarkSocketInboxEmpty(b *testing.B) {
	x := newInproc(b)
	ctx := context.Background()
	for b.Loop() {
		var r localapi.InboxResp
		if err := x.c.Call(ctx, "inbox", localapi.InboxReq{Agent: x.agent}, &r); err != nil {
			b.Fatal(err)
		}
	}
}
