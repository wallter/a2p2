package node

// Bounds on file transfers (PROTOCOL 10.3, DESIGN 12 item 9): one
// transfer per peer per direction and FileTransfers in all, a total time
// per response and a minimum rate, all separate from the pull exchange's
// limits so files never delay pulls.

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// Directions of a transfer, from this machine's side.
const (
	dirIn  = "in"
	dirOut = "out"
)

// transfers is the limiter of file transfers: one per peer per
// direction, and max in all, both directions together (finding 10).
type transfers struct {
	mu    sync.Mutex
	max   int
	peers map[[2]string]bool
	count int
}

func newTransfers(max int) *transfers {
	return &transfers{max: max, peers: map[[2]string]bool{}}
}

// Why a transfer was not started.
var (
	errPeerBusy = errors.New("files: a transfer with this peer in this direction is in progress")
	errAllBusy  = errors.New("files: the machine's file transfers are all in use")
)

// take reserves a slot for one transfer with peer in dir.
func (t *transfers) take(peer, dir string) (func(), error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := [2]string{peer, dir}
	switch {
	case t.peers[k]:
		return nil, errPeerBusy
	case t.count >= t.max:
		return nil, errAllBusy
	}
	t.peers[k] = true
	t.count++
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			delete(t.peers, k)
			t.count--
		})
	}, nil
}

// ErrSlow: a transfer took longer than FileResponseMax in all, or fell
// below FileMinRate over a window.
var ErrSlow = errors.New("files: the transfer was too slow and was cut")

// Limits of one transfer; variables so tests can shorten them.
var (
	fileResponseMax = core.FileResponseMax
	fileRateWindow  = core.FileRateWindow
	fileMinRate     = int64(core.FileMinRate)
)

// RateLimits are the time bounds of one transfer.
type RateLimits struct {
	Total   time.Duration // the whole response or upload
	Window  time.Duration // MinRate is kept over any window this long
	MinRate int64         // bytes a second
	Now     func() time.Time
}

// TransferLimits are the bounds in force.
func TransferLimits() RateLimits {
	return RateLimits{Total: fileResponseMax, Window: fileRateWindow, MinRate: fileMinRate, Now: time.Now}
}

// RateReader cuts a transfer that lasts longer than Total in all, or
// moves fewer than MinRate bytes a second over a window (a trickle). Its
// Watch also cuts one whose reads block: it calls cancel.
type RateReader struct {
	r     io.Reader
	l     RateLimits
	start time.Time
	moved atomic.Int64
	slow  atomic.Bool
}

// LimitRate wraps r with the bounds l.
func LimitRate(r io.Reader, l RateLimits) *RateReader {
	if l.Now == nil {
		l.Now = time.Now
	}
	return &RateReader{r: r, l: l, start: l.Now()}
}

func (r *RateReader) Read(p []byte) (int, error) {
	if r.slow.Load() || r.l.Now().Sub(r.start) > r.l.Total {
		r.slow.Store(true)
		return 0, ErrSlow
	}
	n, err := r.r.Read(p)
	r.moved.Add(int64(n))
	return n, err
}

// Slow reports whether the transfer was cut for being slow.
func (r *RateReader) Slow() bool { return r.slow.Load() }

// Watch checks the transfer every window, in its own goroutine, and calls
// cancel when the window moved too little or the total time passed; the
// returned stop ends the watch.
func (r *RateReader) Watch(cancel func()) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(r.l.Window)
		defer t.Stop()
		var last int64
		for {
			select {
			case <-done:
				return
			case <-t.C:
				now := r.moved.Load()
				if float64(now-last) < float64(r.l.MinRate)*r.l.Window.Seconds() || r.l.Now().Sub(r.start) > r.l.Total {
					r.slow.Store(true)
					cancel()
					return
				}
				last = now
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}
