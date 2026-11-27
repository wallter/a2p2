//go:build bench

package bench

import (
	"sync/atomic"
	"time"

	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// fanCount is what the in-process fan-in counts through the benchmark
// build's hooks (node.BenchOnScan, store.BenchOnTx).
type fanCount struct {
	scans, scanRooms, scanNS atomic.Int64
	reads, readNS            atomic.Int64
	writes, writeNS          atomic.Int64
}

// fanNow is the count of the case running; the store's hook is global.
var fanNow atomic.Pointer[fanCount]

func init() {
	store.BenchOnTx(func(write bool, began time.Time) {
		c := fanNow.Load()
		if c == nil {
			return
		}
		d := int64(time.Since(began))
		if write {
			c.writes.Add(1)
			c.writeNS.Add(d)
		} else {
			c.reads.Add(1)
			c.readNS.Add(d)
		}
	})
}

// benchHooks counts x's inbox looks and every store transaction from now,
// and returns the function that prints them for a case once it is done:
// per question (a question and its answer are two sends) and in total.
func benchHooks(x *inproc) func(cs string, questions int64) {
	c := &fanCount{}
	node.BenchOnScan(x.surf, func(rooms int, began time.Time) {
		c.scans.Add(1)
		c.scanRooms.Add(int64(rooms))
		c.scanNS.Add(int64(time.Since(began)))
	})
	fanNow.Store(c)
	start := time.Now()
	return func(cs string, q int64) {
		el := time.Since(start)
		fanNow.CompareAndSwap(c, nil)
		if q == 0 {
			return
		}
		per := func(n *atomic.Int64) float64 { return float64(n.Load()) / float64(q) }
		avg := func(ns, n *atomic.Int64) float64 {
			if n.Load() == 0 {
				return 0
			}
			return float64(ns.Load()) / float64(n.Load()) / 1e6
		}
		note("M8 %s [in process]: per question: %.1f inbox looks (%.1f room walks, %.3f ms each), %.1f read transactions (%.3f ms each), %.1f write transactions (%.3f ms each)",
			cs, per(&c.scans), per(&c.scanRooms), avg(&c.scanNS, &c.scans), per(&c.reads), avg(&c.readNS, &c.reads), per(&c.writes), avg(&c.writeNS, &c.writes))
		note("M8 %s [in process]: the store's one connection was in a transaction %.0f%% of the %.1f s (reads %.0f%%, writes %.0f%%)",
			cs, 100*float64(c.readNS.Load()+c.writeNS.Load())/float64(el), el.Seconds(),
			100*float64(c.readNS.Load())/float64(el), 100*float64(c.writeNS.Load())/float64(el))
	}
}
