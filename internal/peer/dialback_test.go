package peer

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/node"
)

// dbH is a machine's peer handler with the dial-back.
type dbH struct {
	*pullH
	db *node.DialBackServer
}

func (h dbH) DialBack(ctx context.Context, from, source string, q *node.DialBackRequest) (*node.DialBackResponse, *node.PullError) {
	return h.db.DialBack(ctx, from, source, q)
}

// dialBackPair is a room of a and b, both listening; a also answers
// dial-backs on a second listener, which b knows a by.
func dialBackPair(t *testing.T) (a, b *pm) {
	t.Helper()
	a, b = newPM(t, "a", true), newPM(t, "b", true)
	group(t, a, b)
	addr := serve(t, a.id, dbH{a.h, node.NewDialBackServer(a.r, NewProber(a.id))}, nil)
	b.book.Keep(func(n string) bool { return n != a.node() }) // only the dial-back listener
	b.book.Replace(a.node(), []string{addr}, time.Now())
	return a, b
}

// silent accepts TCP connections and never answers.
func silent(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			c.Close()
		}
	})
	return ln.Addr().String()
}

// closedPort is an address on which nothing listens.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// Over real TLS on loopback: the responder reaches the asker's own
// listener, and not an address where another key, nothing, or a silent
// listener is; the other key's machine gets a handshake and no request.
//
// Requirements: N1
func TestDialBackOverTLSReachesOnlyTheAskersOwnKey(t *testing.T) {
	a, b := dialBackPair(t)
	other := newPM(t, "x", true) // listens under another key; not in the room
	defer func(d time.Duration) { node.DialBackTimeout = d }(node.DialBackTimeout)
	node.DialBackTimeout = time.Second
	asked := []string{other.addr, b.addr, closedPort(t), silent(t)}
	start := time.Now()
	got, err := b.cl.DialBack(bg, a.node(), asked)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Reached, []string{b.addr}) {
		t.Fatalf("reached %v, want only %s", got.Reached, b.addr)
	}
	if got.Saw != "127.0.0.1" {
		t.Fatalf("saw %q", got.Saw)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %s with a 1 s dial-back timeout", d)
	}
	if n := other.h.calls.Load(); n != 0 {
		t.Fatalf("the other key's machine got %d requests", n)
	}
	if n := b.h.calls.Load() + b.h.pulls.Load(); n != 0 {
		t.Fatalf("the asker got %d requests from the dial-back", n)
	}
}

// Over HTTP: a machine that shares no room is refused 403 and nothing is
// dialled; the sixth request of one peer within minutes is refused 429
// with Retry-After; a machine without the route answers 404.
//
// Requirements: N1
func TestDialBackOverTLSRefusesNonMembersLimitsRateAndOldMachinesSay404(t *testing.T) {
	a, b := dialBackPair(t)
	stranger := newPM(t, "s", true)
	stranger.book.Replace(a.node(), b.book.Addrs(a.node()), time.Now())
	var pe *node.PullError
	if _, err := stranger.cl.DialBack(bg, a.node(), []string{stranger.addr}); !errors.As(err, &pe) || pe.Status != 403 {
		t.Fatalf("stranger: %v", err)
	}
	for i := range 4 {
		if _, err := b.cl.DialBack(bg, a.node(), []string{b.addr}); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	_, err := b.cl.DialBack(bg, a.node(), []string{b.addr})
	if !errors.As(err, &pe) || pe.Status != 429 || pe.Code != node.CodeRateLimited || pe.RetryAfter < time.Minute {
		t.Fatalf("over the limit: %v", err)
	}
	// a's ordinary listener has no dial-back handler: as an older machine.
	b.book.Keep(func(n string) bool { return n != a.node() })
	b.book.Replace(a.node(), []string{a.addr}, time.Now())
	_, err = b.cl.DialBack(bg, a.node(), []string{b.addr})
	if !errors.As(err, &pe) || pe.Status != 404 {
		t.Fatalf("old machine: %v", err)
	}
}

// Each outcome of a probe over real sockets: reached, wrong key, refused,
// timed out, and something that is not an a2p2 machine.
//
// Requirements: N2
func TestProbeOutcomesOverRealSockets(t *testing.T) {
	defer func(d time.Duration) { probeTLSTimeout = d }(probeTLSTimeout)
	probeTLSTimeout = 300 * time.Millisecond
	a := newPM(t, "a", true)
	p := NewProber(newID(t))
	https := httptest.NewTLSServer(nil)
	defer https.Close()
	for _, c := range []struct {
		addr, key, want string
	}{
		{a.addr, a.node(), node.ProbeReached},
		{a.addr, newID(t).NodeID(), node.ProbeWrongKey},
		{closedPort(t), a.node(), node.ProbeRefused},
		{silent(t), a.node(), node.ProbeTimeout},
		{strings.TrimPrefix(https.URL, "https://"), a.node(), node.ProbeNotA2P2},
	} {
		got := p.Probe(bg, c.key, c.addr)
		if got.Result != c.want {
			t.Errorf("%s: %+v, want %s", c.addr, got, c.want)
		}
		if c.want == node.ProbeReached || c.want == node.ProbeWrongKey {
			if got.Node != a.node() {
				t.Errorf("%s: answered by %s", c.addr, core.ShortNode(got.Node))
			}
		}
	}
	if n := a.h.calls.Load() + a.h.pulls.Load(); n != 0 {
		t.Fatalf("probes made %d requests", n)
	}
	// A throwaway key presents itself, not this machine.
	tp, err := NewThrowawayProber()
	if err != nil {
		t.Fatal(err)
	}
	if got := tp.ProbeWith(bg, a.addr, nil); got.Result != node.ProbeReached || got.Node != a.node() {
		t.Fatalf("throwaway: %+v", got)
	}
}

// The asker's whole path over TLS: a Reacher asks the member it can reach,
// and the answer is yes; with b's listener gone, no.
//
// Requirements: N1
func TestReachOverTLSIsYesThenNoWhenTheListenerGoes(t *testing.T) {
	a, b := dialBackPair(t)
	defer func(d time.Duration) { node.DialBackTimeout = d }(node.DialBackTimeout)
	node.DialBackTimeout = time.Second
	addrs := []string{b.addr}
	r := node.NewReacher(b.cl, node.ReachOpts{Members: func() []string { return []string{a.node()} },
		Book: b.book, Addrs: func() []string { return addrs }})
	if got := r.Check(bg, []string{a.node()}, true); got.State != node.ReachYes {
		t.Fatalf("listening: %+v", got)
	}
	addrs = []string{closedPort(t)}
	if got := r.Check(bg, []string{a.node()}, true); got.State != node.ReachNo {
		t.Fatalf("not listening: %+v", got)
	}
}

// A candidate address (a mapped port, or the address members see) is
// verified over TLS like any other: it counts only where the asker's own
// key answers. Over IPv6 loopback too, where there is one.
//
// Requirements: N2
func TestOverTLSACandidateIsVerifiedOnlyWhereTheAskersKeyAnswersAndIPv6Works(t *testing.T) {
	a, b := dialBackPair(t)
	defer func(d time.Duration) { node.DialBackTimeout = d }(node.DialBackTimeout)
	node.DialBackTimeout = time.Second
	second := serve(t, b.id, b.h, nil) // b's key on another port, as a router forwarding to it
	other := newPM(t, "x", true)
	cands := []string{second, other.addr}
	if l6, err := net.Listen("tcp6", "[::1]:0"); err == nil {
		ctx, cancel := context.WithCancel(bg)
		done := make(chan error, 1)
		go func() { done <- Serve(ctx, l6, b.id, b.h) }()
		t.Cleanup(func() { cancel(); <-done })
		cands = append(cands, l6.Addr().String())
	}
	r := node.NewReacher(b.cl, node.ReachOpts{Members: func() []string { return []string{a.node()} }, Book: b.book,
		Addrs: func() []string { return []string{b.addr} }, Candidates: func() []string { return cands }})
	r.Check(bg, []string{a.node()}, true)
	want := slices.DeleteFunc(slices.Clone(cands), func(c string) bool { return c == other.addr })
	if got := r.Verified(); !slices.Equal(got, want) {
		t.Fatalf("verified %v, want %v", got, want)
	}
}
