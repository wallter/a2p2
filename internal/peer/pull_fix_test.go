package peer

// Step 5b fix round over real TLS: items 7, 8, 9 and 10.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/node"
)

// set changes *p to v for the test.
func set[T any](t *testing.T, p *T, v T) {
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// stubPull answers every pull request with a bare same; a request with
// no wait_ms first waits for release.
type stubPull struct {
	countH
	release chan struct{}
	entered chan struct{}
	served  atomic.Int64
}

func (h *stubPull) Serve(ctx context.Context, _ string, q *node.PullRequest) (*node.PullResponse, error) {
	if q.WaitMS == 0 {
		h.entered <- struct{}{}
		select {
		case <-h.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	h.served.Add(1)
	return &node.PullResponse{V: node.PullVersion, Same: true, Log: node.PullLog{Heads: q.Log.Heads}}, nil
}

var (
	tRoom = "r_" + strings.Repeat("ab", 16)
	tHead = strings.Repeat("1", 32)
)

func openingJSON(waitMS int64) []byte {
	extra := ""
	if waitMS > 0 {
		extra = fmt.Sprintf(`,"wait_ms":%d`, waitMS)
	}
	return []byte(fmt.Sprintf(`{"v":2,"room":%q,"sum":%q,"log":{"heads":[%q]}%s}`, tRoom, strings.Repeat("0", 64), tHead, extra))
}

func wantPullError(t *testing.T, what string, status int, body []byte, h map[string][]string, code string) {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error != code {
		t.Fatalf("%s: status %d body %q, want %s", what, status, body, code)
	}
	if (status == 429 || status == 503) && len(h["Retry-After"]) == 0 {
		t.Fatalf("%s: no Retry-After", what)
	}
}

// [item 9] A peer over its rate is refused before its body is read or
// decoded: a 429, not a 400 for the body.
func TestOverTLSARequesterOverItsRateIsRefusedBeforeItsBodyIsRead(t *testing.T) {
	set(t, &pullRateBurst, 1)
	set(t, &pullRatePerSec, 0.001)
	a, b := newPM(t, "alice", true), newPM(t, "bob", false)
	if st, _, _ := rawPost(t, b.id, a, []byte("not json"), -1); st != 400 {
		t.Fatalf("first: %d", st)
	}
	st, body, h := rawPost(t, b.id, a, []byte("not json"), -1)
	if st != 429 {
		t.Fatalf("second: %d, want 429", st)
	}
	wantPullError(t, "second", st, body, h, node.CodeRateLimited)
}

// [item 9] A waiting pull that does not enter a hold counts against the
// concurrency limit like any other request.
func TestOverTLSAWaitingPullCountsAgainstTheConcurrencyLimitUntilItIsHeld(t *testing.T) {
	set(t, &pullConcurrent, 1)
	a, b := newPM(t, "alice", false), newPM(t, "bob", false)
	h := &stubPull{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	a.addr = serve(t, a.id, h, nil, nil)
	busy := make(chan int, 1)
	go func() { st, _, _ := rawPost(t, b.id, a, openingJSON(0), -1); busy <- st }()
	<-h.entered // the first is in progress at the server
	st, body, hd := rawPost(t, b.id, a, openingJSON(1000), -1)
	close(h.release)
	if st != 429 {
		t.Fatalf("a waiting pull beside a busy request: %d, want 429", st)
	}
	wantPullError(t, "concurrent", st, body, hd, node.CodeRateLimited)
	if st := <-busy; st != 200 {
		t.Fatalf("the first: %d", st)
	}
}

// [item 9] A waiting pull gives up its slot once it is held: beside it,
// with one slot, a short request from the same peer is served.
func TestOverTLSAHeldPullGivesUpItsConcurrencySlot(t *testing.T) {
	set(t, &pullConcurrent, 1)
	a, b := newPM(t, "alice", true), newPM(t, "bob", false)
	r1, r2 := group(t, a, b), group(t, a, b)
	held := a.x.NextHold(r1, b.node())
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		_, _ = b.x.Pull(ctx, r1, a.node(), node.PullOpts{WaitMS: 60000})
	}()
	<-held
	if _, err := b.pull(r2, a, node.PullOpts{}); err != nil {
		t.Fatalf("a short request beside a held one: %v", err)
	}
}

// [item 9] The outer guard's refusal of a pull request is the pull error:
// JSON with Retry-After.
func TestOverTLSTheGuardRefusesAPullRequestWithThePullError(t *testing.T) {
	set(t, &rateBurst, 1)
	set(t, &ratePerSec, 0.001)
	a, b := newPM(t, "alice", true), newPM(t, "bob", false)
	rawPost(t, b.id, a, openingJSON(0), -1)
	st, body, h := rawPost(t, b.id, a, openingJSON(0), -1)
	if st != 429 {
		t.Fatalf("status %d", st)
	}
	wantPullError(t, "guard", st, body, h, node.CodeRateLimited)
}

// [item 8] When the listener shuts down, a held request is released at
// once, not at the end of the grace period.
func TestOverTLSShuttingDownReleasesAHeldPullAtOnce(t *testing.T) {
	a, b := newPM(t, "alice", false), newPM(t, "bob", false)
	a.h = &pullH{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a.addr = ln.Addr().String()
	a.x = node.NewExchange(a.r, a.cl, node.ExchangeOpts{Book: a.book, Addrs: []string{a.addr}})
	a.h.x.Store(a.x)
	ctx, cancel := context.WithCancel(bg)
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, ln, a.id, a.h, nil) }()
	room := group(t, a, b)
	held := a.x.NextHold(room, b.node())
	got := make(chan error, 1)
	go func() {
		_, err := b.pull(room, a, node.PullOpts{WaitMS: 60000})
		got <- err
	}()
	<-held
	start := time.Now()
	cancel()
	select {
	case err := <-got:
		if err == nil {
			t.Fatal("answered as if nothing happened")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("never released")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("released %v after shutdown began", took)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

// [item 7] An address that accepts and then stalls costs one attempt:
// the next one starts at the next address.
func TestOverTLSAStalledFirstAddressGivesTheNextAttemptToTheSecond(t *testing.T) {
	set(t, &pullTimeout, 300*time.Millisecond)
	a, b := newPM(t, "alice", true), newPM(t, "bob", false)
	room := group(t, a, b)
	stall, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stall.Close() })
	go func() {
		for {
			c, err := stall.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() }) // accepted, never answered
		}
	}()
	b.book.Replace(a.node(), []string{stall.Addr().String(), a.addr}, time.Now())
	b.book.Worked(a.node(), stall.Addr().String()) // it once completed a handshake, and now stalls
	if got := b.book.Addrs(a.node()); got[0] != stall.Addr().String() {
		t.Fatalf("addresses: %v", got)
	}
	if _, err := b.pull(room, a, node.PullOpts{}); err == nil {
		t.Fatal("the stalled address answered")
	}
	if _, err := b.pull(room, a, node.PullOpts{}); err != nil {
		t.Fatalf("the next attempt: %v", err)
	}
}

// [item 10] Dials to addresses that never completed a pinned handshake
// share one budget (2 a second, burst 8) across every peer.
func TestOverTLSDialsToUnverifiedAddressesShareOneBudget(t *testing.T) {
	var accepted atomic.Int64
	b := newPM(t, "bob", false)
	var peers []string
	for i := 0; i < 3; i++ {
		peer := newID(t).NodeID()
		peers = append(peers, peer)
		var addrs []string
		for j := 0; j < node.MaxAddrs; j++ {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() })
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					accepted.Add(1)
					c.Close()
				}
			}()
			addrs = append(addrs, ln.Addr().String())
		}
		b.book.Replace(peer, addrs, time.Now())
	}
	for _, p := range peers {
		_, _ = b.cl.Pull(bg, p, &node.PullRequest{V: node.PullVersion, Room: tRoom, Sum: strings.Repeat("0", 64),
			Log: node.PullLog{Heads: []string{tHead}}})
	}
	if n := accepted.Load(); n > 9 {
		t.Fatalf("%d dials to unverified addresses at once", n)
	}
}
