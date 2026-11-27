//go:build bench

package node

// The cost of serving and making pull requests as a room's history grows,
// in process, without Docker (scripts/bench.sh does not run it; run it
// with go test -tags bench -run TestBenchPullHistory ./internal/node/).
// Two machines a and b hold one room of N records by A authors (synthetic
// keys admitted to the room, one stream each); b pulls from a over the
// in-memory network. Every request is replayed against a's Exchange.Serve
// directly, so what is timed is the server's own work (steps 4 to 10 of
// PROTOCOL 4.4) including its store transactions and their fsync.
//
// Environment: A2P2_BENCH_SIZES (default 1000,10000,50000,100000),
// A2P2_BENCH_AUTHORS (default 5,50), A2P2_BENCH_REPS (default 20),
// A2P2_BENCH_PROFILE (a directory: CPU, mutex and block profiles of the
// largest case are written there).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

func benchInts(name string, def []int) []int {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	var out []int
	for _, f := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

// cpuNow is the process's user and system CPU time.
func cpuNow() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// cost is CPU and wall time per operation.
type cost struct{ cpu, wall time.Duration }

func (c cost) String() string {
	return fmt.Sprintf("%7.2f ms CPU %7.2f ms wall", float64(c.cpu)/1e6, float64(c.wall)/1e6)
}

func measure(n int, f func()) cost {
	runtime.GC()
	c0, t0 := cpuNow(), time.Now()
	for i := 0; i < n; i++ {
		f()
	}
	return cost{(cpuNow() - c0) / time.Duration(n), time.Since(t0) / time.Duration(n)}
}

// timedLink is a transport that counts the time spent inside the server.
type timedLink struct {
	PullTransport
	ns atomic.Int64
	n  atomic.Int64
}

func (l *timedLink) Pull(ctx context.Context, peer string, q *PullRequest) (*PullResponse, error) {
	t0 := time.Now()
	resp, err := l.PullTransport.Pull(ctx, peer, q)
	l.ns.Add(int64(time.Since(t0)))
	l.n.Add(1)
	return resp, err
}

// wireRequest is q as the server's endpoint decodes it.
func wireRequest(t testing.TB, q *PullRequest) *PullRequest {
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	d, perr := DecodePullRequest(b)
	if perr != nil {
		t.Fatal(perr)
	}
	return d
}

func TestBenchPullHistory(t *testing.T) {
	sizes := benchInts("A2P2_BENCH_SIZES", []int{1000, 10000, 50000, 100000})
	authors := benchInts("A2P2_BENCH_AUTHORS", []int{5, 50})
	reps := benchInts("A2P2_BENCH_REPS", []int{20})[0]
	prof := os.Getenv("A2P2_BENCH_PROFILE")
	for _, na := range authors {
		for _, n := range sizes {
			last := na == authors[len(authors)-1] && n == sizes[len(sizes)-1]
			dir := ""
			if last {
				dir = prof
			}
			t.Run(fmt.Sprintf("%d_records_%d_authors", n, na), func(t *testing.T) {
				benchPullHistory(t, n, na, reps, dir)
			})
		}
	}
}

func benchPullHistory(t *testing.T, total, na, reps int, profDir string) {
	ctx := context.Background()
	clock := lclock()
	net := newMemNet()
	a := newPNode(t, net, "a", newLNode(t, "a", clock))
	b := newPNode(t, net, "b", newLNode(t, "b", clock))
	link := &timedLink{PullTransport: net.link(b.id)}
	b.x.t = link
	b.x.budget = newPullBudget(1e6, 1e6) // the server's cost, not the requester's pacing
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
	b.pin(room, a.lnode)
	au := newBenchAuthors(na)
	for i, id := range au.ids {
		a.change(room, Change{Kind: core.EntryAdd, Target: id, Name: fmt.Sprintf("w%02d", i), History: true})
	}
	b.pull(room, a.lnode)
	// The history: round-robin over the authors, dated in the last day.
	catch := min(5000, total/2)
	start := clock.Add(-24 * time.Hour)
	var recs []*core.VerifiedRecord
	for i := 0; i < total; i++ {
		recs = append(recs, au.next(t, room, i%na, start.Add(time.Duration(i)*time.Millisecond)))
	}
	t0 := time.Now()
	benchFill(t, a.lnode, room, recs)
	benchFill(t, b.lnode, room, recs[:total-catch])
	t.Logf("filled %d records (%d authors) in %.1f s; b lacks the last %d", total, na, time.Since(t0).Seconds(), catch)
	gen, err := b.r.Gen(room)
	if err != nil {
		t.Fatal(err)
	}
	report := func(what string, c cost) { t.Logf("%-28s %s", what, c) }

	// Catch-up: b takes the last records from a, page by page.
	link.ns.Store(0)
	link.n.Store(0)
	c0, w0 := cpuNow(), time.Now()
	st := b.mustPull(room, a, PullOpts{})
	cpu, wall := cpuNow()-c0, time.Since(w0)
	if st.Stored != catch {
		t.Fatalf("catch-up stored %d of %d: %+v", st.Stored, catch, st)
	}
	nreq := time.Duration(link.n.Load())
	served := time.Duration(link.ns.Load())
	t.Logf("catch-up of %d: %d requests, %d rounds; per request %.2f ms CPU, %.2f ms wall, of which %.2f ms inside the server",
		catch, nreq, st.Rounds, float64(cpu/nreq)/1e6, float64(wall/nreq)/1e6, float64(served/nreq)/1e6)
	b.mustPull(room, a, PullOpts{})

	var stopProf func()
	if profDir != "" {
		stopProf = startProfiles(t, profDir)
	}

	// Server: an idle opening (same).
	idle, err := b.x.prepare(ctx, room, a.id, gen, nil, "", false, false, 0, nil, map[string]bool{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	qIdle := wireRequest(t, idle.req)
	report("serve idle opening (same)", measure(reps, func() {
		resp, err := a.x.Serve(ctx, b.id, qIdle)
		if err != nil || !resp.Same {
			t.Fatalf("idle: %+v %v", resp, err)
		}
	}))
	report("prepare idle opening", measure(reps, func() {
		if _, err := b.x.prepare(ctx, room, a.id, gen, nil, "", false, false, 0, nil, map[string]bool{}, nil, false); err != nil {
			t.Fatal(err)
		}
	}))
	report("exchange idle (b pulls a)", measure(reps, func() { b.mustPull(room, a, PullOpts{}) }))

	// Server: an opening after one new record of author 0 at a.
	benchFill(t, a.lnode, room, []*core.VerifiedRecord{au.next(t, room, 0, *clock)})
	open, err := b.x.prepare(ctx, room, a.id, gen, nil, "", false, false, 0, nil, map[string]bool{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	qOpen := wireRequest(t, open.req)
	var digests *PullResponse
	report("serve opening, one new", measure(reps, func() {
		resp, err := a.x.Serve(ctx, b.id, qOpen)
		if err != nil || resp.Same {
			t.Fatalf("open: %+v %v", resp, err)
		}
		digests = resp
	}))
	// Server: the detail request for that author, answered with its page.
	det, err := b.x.prepare(ctx, room, a.id, gen, []string{au.ids[0]}, digests.Sum, false, false, 0, nil, map[string]bool{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	qDet := wireRequest(t, det.req)
	forget := func() { // as the opening that comes before each continuation does
		a.x.mu.Lock()
		delete(a.x.pages, [2]string{room, b.id})
		a.x.mu.Unlock()
	}
	report("serve detail, page of 1", measure(reps, func() {
		forget()
		resp, err := a.x.Serve(ctx, b.id, qDet)
		if err != nil || len(resp.Page) != 1 {
			t.Fatalf("detail: %d records %v", len(resp.Page), err)
		}
	}))
	// Server: a page of a catch-up, for a requester behind by catch in
	// author 0's stream.
	qCatch := wireRequest(t, det.req)
	d := qCatch.Detail[au.ids[0]]
	d[0].C = max(0, d[0].C-int64(catch/na))
	d[0].X, d[0].NB, d[0].W = 0, nil, nil
	report("serve catch-up page", measure(max(3, reps/4), func() {
		forget()
		resp, err := a.x.Serve(ctx, b.id, qCatch)
		if err != nil || len(resp.Page) == 0 {
			t.Fatalf("catch: %d records %v", len(resp.Page), err)
		}
	}))
	b.mustPull(room, a, PullOpts{})

	// Requester: a whole exchange for one new record (three requests),
	// split into the server's part and the rest.
	var inServer, reqs time.Duration
	c := measure(reps, func() {
		benchFill(t, a.lnode, room, []*core.VerifiedRecord{au.next(t, room, 1%na, *clock)})
		link.ns.Store(0)
		link.n.Store(0)
		st := b.mustPull(room, a, PullOpts{})
		if st.Stored != 1 {
			t.Fatalf("one new: %+v", st)
		}
		inServer += time.Duration(link.ns.Load())
		reqs += time.Duration(link.n.Load())
	})
	report("exchange, one new record", c)
	t.Logf("  %.1f requests each; %.2f ms wall inside the server per exchange, %.2f ms outside",
		float64(reqs)/float64(reps), float64(inServer/time.Duration(reps))/1e6, float64(c.wall-inServer/time.Duration(reps))/1e6)

	// Concurrency: four peers' idle openings served at once, against local
	// writes, for the mutex and block profiles.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	served2 := atomic.Int64{}
	t1 := time.Now()
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := a.x.Serve(ctx, b.id, qIdle); err != nil {
					// a write in between makes it digests: still served
					_ = err
				}
				served2.Add(1)
			}
		}()
	}
	time.Sleep(2 * time.Second)
	close(stop)
	wg.Wait()
	t.Logf("four concurrent idle servers: %.1f served a second", float64(served2.Load())/time.Since(t1).Seconds())
	if stopProf != nil {
		stopProf()
	}
}

func startProfiles(t *testing.T, dir string) func() {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "cpu.pprof"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.SetMutexProfileFraction(1)
	runtime.SetBlockProfileRate(1)
	if err := pprof.StartCPUProfile(f); err != nil {
		t.Fatal(err)
	}
	return func() {
		pprof.StopCPUProfile()
		f.Close()
		for _, name := range []string{"mutex", "block"} {
			g, err := os.Create(filepath.Join(dir, name+".pprof"))
			if err == nil {
				_ = pprof.Lookup(name).WriteTo(g, 0)
				g.Close()
			}
		}
		runtime.SetMutexProfileFraction(0)
		runtime.SetBlockProfileRate(0)
	}
}
