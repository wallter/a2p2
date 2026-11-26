package peer

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/node"
)

// warm makes id's source known: one request from addr, then its
// connection is closed.
func warm(t *testing.T, addr string, id *Identity) {
	t.Helper()
	c := keyClient(t, id)
	if code, err := post(c, addr, pathPullV2, string(mustBody(t, opening()))); err != nil || code != http.StatusOK {
		t.Fatalf("a member before the flood: %d %v", code, err)
	}
	c.CloseIdleConnections()
}

// served says whether a new connection of id from addr is served.
func served(t *testing.T, addr string, id *Identity) bool {
	t.Helper()
	c := keyClient(t, id)
	c.Timeout = time.Second
	code, err := post(c, addr, pathPullV2, string(mustBody(t, opening())))
	c.CloseIdleConnections()
	return err == nil && code == http.StatusOK
}

// A host in another /64 of a known member's IPv6 /56 fills the /56's
// share of handshake slots; the member still opens new connections,
// through the slots kept for known member sources, which no /56 or /48
// count reaches.
//
// Requirements: N1; PROTOCOL 4.6
func TestAStrangerInTheSameSlash56CannotKeepAKnownMemberOut(t *testing.T) {
	set(t, &maxPerSource, 8)
	set(t, &maxPerSlash56, 3)
	set(t, &handshakeTimeout, 10*time.Second)
	h := &memberH{}
	addrs := serveFrom(t, newID(t), h, "2001:db8:0:1::1", "2001:db8:0:2::1")
	member, stranger := addrs[0], addrs[1]
	id := h.add(newID(t))
	warm(t, member, id)
	held := holdMany(t, stranger, maxPerSource, false)
	if n := kept(held); n != maxPerSlash56 {
		t.Fatalf("the stranger holds %d slots, the test needs the /56's %d", n, maxPerSlash56)
	}
	for i := 0; i < 2; i++ {
		if !served(t, member, id) {
			t.Fatalf("a known member's new connection %d was refused while a neighbour filled its /56", i)
		}
	}
}

// While the shared handshake slots are full, a host at a known member's
// address takes at most that address's share of the slots kept for known
// sources, so a known member elsewhere still opens new connections.
//
// Requirements: N1; PROTOCOL 4.6
func TestOneKnownSourceHoldsAtMostItsShareOfTheKeptHandshakeSlots(t *testing.T) {
	set(t, &maxPendingConns, 4)
	set(t, &maxPerSource, 4)
	set(t, &maxKnownPending, 4)
	set(t, &maxKnownPerGroup, 2)
	set(t, &handshakeTimeout, 10*time.Second)
	h := &memberH{}
	addrs := serveFrom(t, newID(t), h, "198.51.100.1", "198.51.100.2", "198.51.100.3")
	x, y, z := addrs[0], addrs[1], addrs[2]
	mx, my := h.add(newID(t)), h.add(newID(t))
	warm(t, x, mx)
	warm(t, y, my)
	if n := kept(holdMany(t, z, maxPerSource, false)); n != maxPendingConns {
		t.Fatalf("the flood holds %d shared slots, the test needs all %d", n, maxPendingConns)
	}
	if n := kept(holdMany(t, x, 3*maxKnownPending, false)); n != maxKnownPerGroup {
		t.Fatalf("a host at a known address holds %d kept slots, its share is %d", n, maxKnownPerGroup)
	}
	if !served(t, y, my) {
		t.Fatal("a known member elsewhere was refused while one known address held its share")
	}
}

// A key that shares no room and completes a handshake in a slot kept for
// known member sources is closed, and its address is no longer known: it
// cannot come back to those slots until a member's handshake there
// succeeds through the shared slots. A known member elsewhere is still
// served.
//
// Requirements: N1; PROTOCOL 4.6
func TestAKeyThatSharesNoRoomInAKeptSlotMakesItsSourceUnknown(t *testing.T) {
	set(t, &maxPendingConns, 4)
	set(t, &maxPerSource, 4)
	set(t, &handshakeTimeout, 10*time.Second)
	h := &memberH{}
	addrs := serveFrom(t, newID(t), h, "198.51.100.1", "198.51.100.2", "198.51.100.3")
	x, y, z := addrs[0], addrs[1], addrs[2]
	mx, my := h.add(newID(t)), h.add(newID(t))
	warm(t, x, mx)
	warm(t, y, my)
	if n := kept(holdMany(t, z, maxPerSource, false)); n != maxPendingConns {
		t.Fatalf("the flood holds %d shared slots, the test needs all %d", n, maxPendingConns)
	}
	if c := holdTLS(t, x); c != nil && open(c) {
		t.Fatal("a key that shares no room was kept in a slot kept for members' sources")
	}
	if n := kept(holdMany(t, x, maxKnownPerGroup, false)); n != 0 {
		t.Fatalf("after a stranger's key there, the address still holds %d kept slots", n)
	}
	if !served(t, y, my) {
		t.Fatal("a known member elsewhere was refused")
	}
}

// slowMemberH is memberH whose lookup of one key waits until release is
// closed, as a lookup waiting for a busy rooms lock does.
type slowMemberH struct {
	*memberH
	slow    string
	release chan struct{}
}

func (h slowMemberH) SharesRoom(n string) bool {
	if n == h.slow {
		<-h.release
	}
	return h.memberH.SharesRoom(n)
}

// A membership lookup that does not finish is cut at the handshake
// deadline: the connection is closed and its handshake slot freed, so a
// member is served. A lookup that answers in time places its connection.
//
// Requirements: N1; PROTOCOL 4.6
func TestASlowMembershipLookupIsCutAtTheHandshakeDeadline(t *testing.T) {
	set(t, &handshakeTimeout, 300*time.Millisecond)
	set(t, &maxPendingConns, 1)
	set(t, &maxKnownPending, 0)
	slow := newID(t)
	h := slowMemberH{memberH: &memberH{}, slow: slow.NodeID(), release: make(chan struct{})}
	h.add(slow)
	addrs := serveFrom(t, newID(t), h, "198.51.100.7", "203.0.113.5")
	t.Cleanup(func() { close(h.release) }) // before Serve is stopped
	c := holdKey(t, addrs[0], slow)
	time.Sleep(3 * handshakeTimeout)
	if open(c) {
		t.Fatal("a connection whose lookup never finished was kept past the handshake deadline")
	}
	if !served(t, addrs[1], h.add(newID(t))) {
		t.Fatal("a member was refused after the stuck lookup's deadline")
	}
}

// brokenLn hands over its first connection, then fails for good once
// broken is closed.
type brokenLn struct {
	net.Listener
	first  atomic.Bool
	broken chan struct{}
}

func (l *brokenLn) Accept() (net.Conn, error) {
	if !l.first.Swap(true) {
		return l.Listener.Accept()
	}
	<-l.broken
	return nil, errors.New("the listener broke")
}

// When the listener fails for good, Serve returns its error and closes
// the connections it accepted, instead of leaving them to the context.
//
// Requirements: N1
func TestAFailedListenerClosesTheConnectionsItAccepted(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bl := &brokenLn{Listener: ln, broken: make(chan struct{})}
	t.Cleanup(func() { ln.Close() })
	h := &memberH{}
	ctx, cancel := context.WithCancel(bg)
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, bl, newID(t), h) }()
	c := holdKey(t, ln.Addr().String(), h.add(newID(t)))
	br := bufio.NewReader(c)
	if r := do(t, c, br, string(mustBody(t, opening()))); r.StatusCode != http.StatusOK {
		t.Fatalf("before the failure: %d", r.StatusCode)
	}
	close(bl.broken)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Serve returned no error for a broken listener")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the listener broke")
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := br.ReadByte(); err == nil || isTimeout(err) {
		t.Fatalf("a connection was kept after the listener broke: %v", err)
	}
}

// testConn is a limitConn of key in pool, on a pipe, under a listener
// whose rooms say member().
func testConn(t *testing.T, pool connPool, member func(string) bool) *limitConn {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	l := &limitListener{member: member, perKey: map[string]int{}, per: map[string]int{}, knownPer: map[string]int{},
		recent: node.NewRecent[time.Time](4)}
	c := &limitConn{Conn: a, l: l, srcs: sourceCounts("198.51.100.7:1"), key: "k", pool: pool}
	switch pool {
	case poolMember:
		l.members, l.perKey["k"] = 1, 1
	case poolStranger:
		l.strangers, l.per["198.51.100.7"] = 1, 1
	}
	return c
}

// A member removed and added back before its connection's stranger life
// ends is a member's connection again: it is served as one and not cut.
//
// Requirements: N1, T5; PROTOCOL 4.6
func TestAMemberRemovedAndAddedBackKeepsItsConnection(t *testing.T) {
	set(t, &strangerLife, 200*time.Millisecond)
	var in atomic.Bool
	in.Store(true)
	c := testConn(t, poolMember, func(string) bool { return in.Load() })
	if c.classify() {
		t.Fatal("a member's request was served as a stranger's")
	}
	in.Store(false)
	if !c.classify() {
		t.Fatal("a removed key's request was served as a member's")
	}
	in.Store(true)
	if c.classify() {
		t.Fatal("a key added back was served as a stranger's")
	}
	time.Sleep(3 * strangerLife)
	c.mu.Lock()
	pool := c.pool
	c.mu.Unlock()
	if pool == poolClosed {
		t.Fatal("the connection of a key added back was cut at the stranger's life")
	}
}

// A stranger's connection serves one request at a time: a second one
// started before the first is answered, as HTTP/2 allows, is refused
// before its body is read. A member's connection serves several at once.
//
// Requirements: N1; PROTOCOL 4.6
func TestAStrangersConnectionServesOneRequestAtATime(t *testing.T) {
	cert, err := selfSigned(newID(t))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		pool connPool
		want int
	}{{"stranger", poolStranger, http.StatusTooManyRequests}, {"member", poolMember, http.StatusOK}} {
		t.Run(tc.name, func(t *testing.T) {
			lc := testConn(t, tc.pool, func(string) bool { return tc.pool == poolMember })
			entered, block := make(chan struct{}, 4), make(chan struct{})
			g := guard(newLimiters(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				entered <- struct{}{}
				<-block
			}), nil)
			var read atomic.Int64
			req := func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/p/v2/other", &countReader{r: strings.NewReader("body"), n: &read})
				r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
				return r.WithContext(context.WithValue(r.Context(), connKey{}, lc))
			}
			var wg sync.WaitGroup
			wg.Go(func() { g.ServeHTTP(httptest.NewRecorder(), req()) })
			<-entered
			w := httptest.NewRecorder()
			second := make(chan struct{})
			go func() { g.ServeHTTP(w, req()); close(second) }()
			select {
			case <-entered:
			case <-second:
			case <-time.After(2 * time.Second):
				t.Fatal("the second request neither ran nor was refused")
			}
			close(block)
			<-second
			wg.Wait()
			if w.Code != tc.want {
				t.Fatalf("a second request at once on a %s's connection: %d, want %d", tc.name, w.Code, tc.want)
			}
			if tc.pool == poolStranger && read.Load() != 0 {
				t.Fatal("a refused request's body was read")
			}
		})
	}
}
