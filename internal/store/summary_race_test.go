package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// cancelRig is a store holding one stream of a room, and the writes a
// test can commit while a cancelled read is still running.
type cancelRig struct {
	t    *testing.T
	s    *Store
	last *core.VerifiedRecord
	msgs []*core.VerifiedRecord
	seq  int64
}

func newCancelRig(t *testing.T, n int) *cancelRig {
	s, _ := openTest(t)
	g := &cancelRig{t: t, s: s}
	for i := 0; i < n; i++ {
		g.msgs = append(g.msgs, g.put(core.RecordFields{Text: fmt.Sprint("m", i)}, true))
	}
	return g
}

// put stores the next record of the stream, with its text or as a header.
func (g *cancelRig) put(f core.RecordFields, text bool) *core.VerifiedRecord {
	g.t.Helper()
	g.seq++
	f.Room, f.Epoch, f.Seq, f.Created = testRoom, "00000000000000e1", g.seq, t0
	if g.last != nil {
		f.Prev = g.last.ID()
	}
	v := mk(g.t, testKey("a"), f)
	rec := v
	if !text {
		rec = v.WithoutBody()
	}
	out, err := g.s.PutRecord(context.Background(), rec, PutOpts{Now: t0, MayRead: true})
	if err != nil || out != OutcomeStored {
		g.t.Fatalf("put: %s %v", out, err)
	}
	g.last = v
	return v
}

// write commits one write that changes what the summary says.
func (g *cancelRig) write(kind int) {
	g.t.Helper()
	switch kind % 3 {
	case 0:
		target := g.msgs[0]
		g.msgs = g.msgs[1:]
		g.put(core.RecordFields{Kind: core.KindWithdraw, Ref: target.ID()}, true)
	case 1:
		g.put(core.RecordFields{Text: "a header"}, false)
	case 2:
		if _, err := g.s.ExpireText(context.Background(), t0.Add(40*24*time.Hour)); err != nil {
			g.t.Fatal(err)
		}
	}
}

// scan is what a fresh read of the table says now.
func (g *cancelRig) scan(at time.Time) []StreamHeld {
	g.t.Helper()
	var out []StreamHeld
	if err := g.s.View(context.Background(), func(rd *Reader) (err error) {
		out, err = rd.summaryScan(testRoom, at)
		return err
	}); err != nil {
		g.t.Fatal(err)
	}
	return out
}

// check fails unless the cached summary equals a read of the table.
func (g *cancelRig) check(what string) {
	g.t.Helper()
	for _, at := range []time.Time{t0, t0.Add(40 * 24 * time.Hour)} {
		var c []StreamHeld
		if err := g.s.View(context.Background(), func(rd *Reader) (err error) {
			c, err = rd.Summary(testRoom, at)
			return err
		}); err != nil {
			g.t.Fatal(err)
		}
		if sc := g.scan(at); !sameSummary(c, sc) {
			g.t.Fatalf("%s: the cache is stale: cached\n%+v\nscanned\n%+v", what, c, sc)
		}
	}
}

func (g *cancelRig) cold() {
	g.s.texts.mu.Lock()
	g.s.texts.rooms = nil
	g.s.texts.mu.Unlock()
}

// cancelledRead runs a summary in a read transaction whose context is
// cancelled at point; there, before the read goes on, a write commits on
// the connection the cancelled transaction gave back. It returns what
// the read returned.
func (g *cancelRig) cancelledRead(point string, kind int) ([]StreamHeld, error) {
	g.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fired := false
	summaryHook = func(p string) {
		if p != point || fired {
			return
		}
		fired = true
		cancel()
		g.write(kind) // waits for the connection the cancelled read gives back
	}
	defer func() { summaryHook = nil }()
	var got []StreamHeld
	err := g.s.View(ctx, func(rd *Reader) (err error) {
		got, err = rd.Summary(testRoom, t0)
		return err
	})
	if !fired {
		g.t.Fatalf("the read never reached %s", point)
	}
	return got, err
}

// A read whose context is cancelled after it has read the room's records
// for the cache, before it puts them there: database/sql gives the one
// connection back, and a write commits on it while the cache holds nothing
// of the room to update. The cancelled read must not then put its older
// reading in the cache.
func TestACancelledColdReadDoesNotPublishWhatAWriteCommittedSinceMadeStale(t *testing.T) {
	g := newCancelRig(t, 4)
	g.cold()
	g.cancelledRead("summary:loaded", 0)
	g.check("after the cancelled cold read")
}

// A read whose context is cancelled after it took the cached entry and
// read the expiry index: a write then commits and changes the cache. What
// the read goes on to return comes from the entry it took, unchanged by
// that write (and, under the race detector, nothing it reads is written
// without synchronisation).
func TestAWriteCommittedAfterACancelledReadTookTheCacheDoesNotChangeWhatItReads(t *testing.T) {
	for kind := 0; kind < 3; kind++ {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			g := newCancelRig(t, 4)
			g.put(core.RecordFields{Text: "a header"}, false)
			g.check("warm") // the entry is in the cache
			before := g.scan(t0)
			got, err := g.cancelledRead("summary:expiry", kind)
			if err == nil && !sameSummary(got, before) {
				t.Fatalf("the read saw the later write: got\n%+v\nwhen it began\n%+v", got, before)
			}
			g.check("after the cancelled warm read")
		})
	}
}

// At every point of a summary, cold or warm, a read cancelled there while
// each kind of write commits leaves no stale entry in the cache.
func TestACancelledSummaryReadNeverLeavesAStaleEntry(t *testing.T) {
	for _, point := range []string{"summary:begin", "summary:loaded", "summary:expiry"} {
		for _, cold := range []bool{true, false} {
			for kind := 0; kind < 3; kind++ {
				t.Run(fmt.Sprint(point, cold, kind), func(t *testing.T) {
					g := newCancelRig(t, 4)
					if cold {
						g.cold()
					} else {
						g.check("warm")
					}
					if point == "summary:loaded" && !cold {
						return // a warm read reads nothing for the cache
					}
					g.cancelledRead(point, kind)
					g.check(fmt.Sprintf("cancelled at %s", point))
				})
			}
		}
	}
}
