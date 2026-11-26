package peer

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/node"
)

// ---- several sources on one machine ----

// multiLn accepts on several loopback listeners; a connection to the i-th
// one reports labels[i] as its remote IP address, so that one test can
// play several network sources.
type multiLn struct {
	lns  []net.Listener
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

type fromConn struct {
	net.Conn
	ip net.IP
}

func (c fromConn) RemoteAddr() net.Addr {
	port := 0
	if a, ok := c.Conn.RemoteAddr().(*net.TCPAddr); ok {
		port = a.Port
	}
	return &net.TCPAddr{IP: c.ip, Port: port}
}

func (m *multiLn) Accept() (net.Conn, error) {
	select {
	case c := <-m.ch:
		return c, nil
	case <-m.done:
		return nil, net.ErrClosed
	}
}

func (m *multiLn) Close() error {
	m.once.Do(func() {
		close(m.done)
		for _, l := range m.lns {
			l.Close()
		}
	})
	return nil
}

func (m *multiLn) Addr() net.Addr { return m.lns[0].Addr() }

// serveFrom runs Serve until the test ends and returns one address per
// label: connecting to addrs[i] comes from labels[i].
func serveFrom(t *testing.T, id *Identity, h Handler, labels ...string) []string {
	t.Helper()
	m := &multiLn{ch: make(chan net.Conn), done: make(chan struct{})}
	var addrs []string
	for _, label := range labels {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		m.lns = append(m.lns, ln)
		addrs = append(addrs, ln.Addr().String())
		ip := net.ParseIP(label)
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				select {
				case m.ch <- fromConn{Conn: c, ip: ip}:
				case <-m.done:
					c.Close()
					return
				}
			}
		}()
	}
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, m, id, h) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return addrs
}

// memberH is countH with a fixed set of member keys.
type memberH struct {
	countH
	members sync.Map
}

func (h *memberH) SharesRoom(n string) bool { _, ok := h.members.Load(n); return ok }

func (h *memberH) add(id *Identity) *Identity { h.members.Store(id.NodeID(), true); return id }

// keyClient is an HTTP/1.1 client presenting id's certificate.
func keyClient(t *testing.T, id *Identity) *http.Client {
	t.Helper()
	cert, err := selfSigned(id)
	if err != nil {
		t.Fatal(err)
	}
	c := rawClient(cert)
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// h2KeyClient is an HTTP/2 client presenting id's certificate.
func h2KeyClient(t *testing.T, id *Identity) *http.Client {
	t.Helper()
	cert, err := selfSigned(id)
	if err != nil {
		t.Fatal(err)
	}
	protos := new(http.Protocols)
	protos.SetHTTP2(true)
	tr := &http.Transport{Protocols: protos, TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true}}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Timeout: 5 * time.Second, Transport: tr}
}

// open says whether the server still holds c: a read that times out
// means open; EOF, a reset or a TLS alert means it was closed or refused.
func open(c net.Conn) bool {
	_ = c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// holdTLS completes a TLS handshake under a fresh key and sends nothing,
// as an attacker holding a slot does. nil if the handshake failed on the
// client side.
func holdTLS(t *testing.T, addr string) net.Conn {
	t.Helper()
	cert, err := selfSigned(newID(t))
	if err != nil {
		t.Fatal(err)
	}
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", addr, &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true})
	if err != nil {
		return nil
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// holdMany opens n connections to addr at once, raw TCP or TLS under
// fresh keys, and returns those the client side got.
func holdMany(t *testing.T, addr string, n int, handshake bool) []net.Conn {
	t.Helper()
	var mu sync.Mutex
	var out []net.Conn
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Go(func() {
			var c net.Conn
			if handshake {
				c = holdTLS(t, addr)
			} else if d, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
				t.Cleanup(func() { d.Close() })
				c = d
			}
			if c != nil {
				mu.Lock()
				out = append(out, c)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return out
}

// kept counts the connections the server still holds.
func kept(cs []net.Conn) int {
	var n atomic.Int64
	var wg sync.WaitGroup
	for _, c := range cs {
		wg.Go(func() {
			if open(c) {
				n.Add(1)
			}
		})
	}
	wg.Wait()
	return int(n.Load())
}

// One host holds as many connections as it can, before and after the TLS
// handshake; it keeps at most its share, and a member from elsewhere is
// served at once.
//
// Requirements: N1; PROTOCOL 4.6
func TestOneSourceCannotHoldAllInboundSlots(t *testing.T) {
	set(t, &maxConns, 6)
	set(t, &maxPendingConns, 6)
	set(t, &maxStrangerConns, 6)
	set(t, &maxPerSource, 3)
	set(t, &strangerLife, time.Minute)
	h := &memberH{}
	addrs := serveFrom(t, newID(t), h, "198.51.100.7", "203.0.113.5")
	attacker, member := addrs[0], addrs[1]
	held := holdMany(t, attacker, 2*maxConns, true)                  // handshakes under fresh keys, then silence
	held = append(held, holdMany(t, attacker, 2*maxConns, false)...) // raw TCP, never a handshake
	c := keyClient(t, h.add(newID(t)))
	c.Timeout = time.Second
	if code, err := post(c, member, pathPullV2, string(mustBody(t, opening()))); err != nil || code != http.StatusOK {
		t.Fatalf("a member was not served while one source held what it could: %d %v", code, err)
	}
	if n := kept(held); n != maxPerSource {
		t.Errorf("one source holds %d connections, its share is %d", n, maxPerSource)
	}
}

// Many sources fill the strangers' pool with handshakes under fresh keys;
// the pool holds no more than its size, and members are still served.
//
// Requirements: N1; PROTOCOL 4.6
func TestStrangersFromManySourcesCannotTakeMembersSlots(t *testing.T) {
	set(t, &maxConns, 4)
	set(t, &maxPendingConns, 16)
	set(t, &maxStrangerConns, 4)
	set(t, &maxPerSource, 8)
	set(t, &strangerLife, time.Minute)
	h := &memberH{}
	addrs := serveFrom(t, newID(t), h, "198.51.100.1", "198.51.100.2", "198.51.100.3", "198.51.100.4", "203.0.113.5")
	var held []net.Conn
	for _, a := range addrs[:4] {
		held = append(held, holdMany(t, a, maxPerSource, true)...)
	}
	for i := 0; i < maxConns; i++ { // every member slot can be used
		c := keyClient(t, h.add(newID(t)))
		c.Timeout = time.Second
		if code, err := post(c, addrs[4], pathPullV2, string(mustBody(t, opening()))); err != nil || code != http.StatusOK {
			t.Fatalf("member %d was not served while strangers held their pool: %d %v", i, code, err)
		}
	}
	if n := kept(held); n != maxStrangerConns {
		t.Errorf("%d stranger connections held, the pool is %d", n, maxStrangerConns)
	}
}

// Every answer to a key that shares no room closes its connection, over
// HTTP/1.1 and HTTP/2; a member's connection stays open for its next
// request.
//
// Requirements: N1; PROTOCOL 4.6
func TestAKeyThatSharesNoRoomIsDisconnectedAfterItsAnswer(t *testing.T) {
	h := &memberH{}
	addr := serveFrom(t, newID(t), h, "198.51.100.7")[0]
	body := string(mustBody(t, opening()))
	reused := func(c *http.Client) []bool {
		var out []bool
		for i := 0; i < 2; i++ {
			var got bool
			ctx := httptrace.WithClientTrace(bg, &httptrace.ClientTrace{GotConn: func(ci httptrace.GotConnInfo) { got = ci.Reused }})
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+pathPullV2, strings.NewReader(body))
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body) // a body left unread would stop reuse by itself
			resp.Body.Close()
			time.Sleep(50 * time.Millisecond) // let a GOAWAY arrive
			out = append(out, got)
		}
		return out
	}
	for name, mk := range map[string]func(*testing.T, *Identity) *http.Client{"HTTP/1.1": keyClient, "HTTP/2": h2KeyClient} {
		if r := reused(mk(t, newID(t))); r[1] {
			t.Errorf("%s: a stranger's connection was kept for a second request", name)
		}
		if r := reused(mk(t, h.add(newID(t)))); !r[1] {
			t.Errorf("%s: a member's connection was not kept", name)
		}
	}
}

// A connection that never completes a handshake is closed at the
// handshake deadline, well before the server's request timeouts.
//
// Requirements: N1; PROTOCOL 4.6
func TestAConnectionThatNeverHandshakesIsClosedAtTheDeadline(t *testing.T) {
	set(t, &handshakeTimeout, 200*time.Millisecond)
	addr := serveFrom(t, newID(t), &memberH{}, "198.51.100.7")[0]
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("closed after %s: %v", time.Since(start), err)
	}
}

// Keys that share no room fill their own rate tables with live buckets; a
// member's first request is still admitted.
//
// Requirements: N1; PROTOCOL 4.6
func TestRateLimiterFullOfStrangersStillServesMembers(t *testing.T) {
	set(t, &rateMaxKeys, 4)
	set(t, &ratePerSec, 1e-6)
	set(t, &pullRatePerSec, 1e-6)
	h := &memberH{}
	addr := serveFrom(t, newID(t), h, "198.51.100.7")[0]
	body := string(mustBody(t, opening()))
	refused := 0
	for i := 0; i < 3*rateMaxKeys; i++ {
		if code, err := post(keyClient(t, newID(t)), addr, pathPullV2, body); err == nil && code == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused == 0 {
		t.Fatal("the strangers' table never filled")
	}
	if code, err := post(keyClient(t, h.add(newID(t))), addr, pathPullV2, body); err != nil || code != http.StatusOK {
		t.Fatalf("a member was refused while strangers filled their table: %d %v", code, err)
	}
}

// countLog counts records at Info or above with a given message.
type countLog struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *countLog) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelInfo }
func (c *countLog) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	c.n[r.Message]++
	c.mu.Unlock()
	return nil
}
func (c *countLog) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *countLog) WithGroup(string) slog.Handler      { return c }
func (c *countLog) count(msg string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[msg]
}

// A flood of refused requests, from strangers and from a member over its
// rate, and of refused connections, writes a fixed number of Info lines.
//
// Requirements: N1; PROTOCOL 4.6
func TestRefusedPeerRequestsDoNotGrowLog(t *testing.T) {
	logs := &countLog{n: map[string]int{}}
	prev := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(prev) })
	set(t, &rateBurst, 2.0)
	set(t, &ratePerSec, 1e-6)
	set(t, &maxPerSource, 2)
	h := &memberH{}
	addr := serveFrom(t, newID(t), h, "198.51.100.7")[0]
	stranger := keyClient(t, newID(t))
	member := keyClient(t, h.add(newID(t)))
	codes := map[string]int{}
	for i := 0; i < 200; i++ {
		code, err := post(stranger, addr, "/nowhere", "")
		codes[fmt.Sprintf("stranger %d %v", code, err)]++
		code, err = post(member, addr, pathPullV2, "{}")
		if i >= int(rateBurst) {
			codes[fmt.Sprintf("member %d %v", code, err)]++
		}
	}
	if codes["stranger 404 <nil>"] != int(rateBurst) || codes["stranger 429 <nil>"] != 200-int(rateBurst) ||
		codes["member 429 <nil>"] != 200-int(rateBurst) {
		t.Fatalf("the requests were not refused as the test needs: %v", codes)
	}
	time.Sleep(100 * time.Millisecond) // the strangers' last connection is given back
	var raw []net.Conn
	for i := 0; i < 20; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			t.Cleanup(func() { c.Close() })
			raw = append(raw, c)
		}
	}
	// The member came from this address, so it may also use its group's
	// share of the slots kept for known member sources.
	if n := kept(raw); len(raw) != 20 || n > maxPerSource+maxKnownPerGroup {
		t.Fatalf("of %d connections, %d were kept: the test needs the rest refused", len(raw), n)
	}
	if n := logs.count("peer request"); n > int(logBurst)+1 {
		t.Errorf("400 refused requests wrote %d Info lines", n)
	}
	if n := logs.count("peer connection refused"); n > 1 {
		t.Errorf("refused connections wrote %d Info lines", n)
	}
}

// ---- the join route's limits per source ----

func atClock(l *sourceLimit) *time.Time {
	now := time.Now()
	l.now = func() time.Time { return now }
	return &now
}

// Addresses of one IPv6 /64 together spend one budget of the /64 and
// cannot fill the table against other joiners.
//
// Requirements: T3; PROTOCOL 5.3
func TestOneIPv6Slash64CannotRefuseOtherJoiners(t *testing.T) {
	l := newSourceLimit()
	atClock(l)
	admitted := 0
	for i := 0; i < 2*maxJoinSources; i++ {
		if ok, _ := l.allow(fmt.Sprintf("2001:db8:1:2::%x", i+1), false); ok {
			admitted++
		}
	}
	if want := joinGroupShare * joinAdmits; admitted != want {
		t.Errorf("one /64 was admitted %d times, its budget is %d", admitted, want)
	}
	for _, honest := range []string{"198.51.100.9", "2001:db8:1:3::1"} {
		if ok, _ := l.allow(honest, false); !ok {
			t.Errorf("an honest joiner at %s was refused", honest)
		}
	}
}

// One address of an IPv6 /64 that spends its own join budget does not
// spend another's: a second host of the same /64 is admitted as often as
// one address was in 0.2.3, and both together stay within the /64's.
//
// Requirements: T3; PROTOCOL 5.3
func TestOneAddressOfASharedSlash64KeepsItsOwnJoinBudget(t *testing.T) {
	l := newSourceLimit()
	atClock(l)
	count := func(src string, poll bool, n int) int {
		ok := 0
		for range n {
			if y, _ := l.allow(src, poll); y {
				ok++
			}
		}
		return ok
	}
	if got := count("2001:db8:1:2::1", false, 3*joinAdmits); got != joinAdmits {
		t.Fatalf("one address was admitted %d opens, its budget is %d", got, joinAdmits)
	}
	if got := count("2001:db8:1:2::2", false, joinAdmits); got != joinAdmits {
		t.Fatalf("a neighbour in the same /64 was admitted %d of %d opens", got, joinAdmits)
	}
	if got := count("2001:db8:1:2::2", true, joinPolls); got != joinPolls {
		t.Fatalf("the neighbour's polls: %d of %d", got, joinPolls)
	}
}

// Once a source has spent its reads, the join route answers 429 before it
// reads a body, whatever the request would have been.
//
// Requirements: T3; PROTOCOL 5.3
func TestJoinRouteReadsNoBodyOnceASourceSpentItsReads(t *testing.T) {
	route := joinV2Route(&countH{})
	var read atomic.Int64
	do := func(op string) int {
		body := &countReader{r: strings.NewReader(`{"v":2,"op":"` + op + `","secret":"x"}`), n: &read}
		r := httptest.NewRequest(http.MethodPost, pathJoinV2, body)
		r.RemoteAddr = "198.51.100.7:4000"
		w := httptest.NewRecorder()
		route.ServeHTTP(w, r)
		return w.Code
	}
	for range joinReads {
		do(node.JoinOpen) // most are refused after their body is read
	}
	before := read.Load()
	if code := do(node.JoinPoll); code != http.StatusTooManyRequests {
		t.Fatalf("a request past the reads budget: %d", code)
	}
	if read.Load() != before {
		t.Fatal("the body of a request past the reads budget was read")
	}
}

// countReader counts the bytes read from r.
type countReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// While the table is full of sources heard from in the last minute, a new
// source pushes out the one heard from least recently, never itself.
//
// Requirements: T3; PROTOCOL 5.3
func TestAFullJoinSourceTableForgetsTheOldestNotTheNewcomer(t *testing.T) {
	set(t, &maxJoinSources, 4)
	l := newSourceLimit()
	now := atClock(l)
	for i := 0; i < maxJoinSources; i++ {
		l.allow(fmt.Sprintf("198.51.100.%d", i+1), false)
		*now = now.Add(time.Second)
	}
	if ok, _ := l.allow("203.0.113.5", false); !ok {
		t.Fatal("a new source was refused while the table was full")
	}
	if l.groups.Len() > maxJoinSources {
		t.Fatalf("the table holds %d sources", l.groups.Len())
	}
	if l.groups.Get("198.51.100.1") != nil || l.groups.Get("198.51.100.4") == nil {
		t.Fatal("the source heard from least recently was not the one forgotten")
	}
}

// A source's opens and its polls have separate budgets: opens past theirs
// are refused while a waiting joiner's polls from the same address go on.
//
// Requirements: T3; PROTOCOL 5.3
func TestJoinPollsAreNotRefusedByOpensFromTheSameSource(t *testing.T) {
	h := &countH{}
	route := joinV2Route(h)
	do := func(op string) int {
		body := `{"v":2,"op":"` + op + `","secret":"x"}`
		r := httptest.NewRequest(http.MethodPost, pathJoinV2, bytes.NewReader([]byte(body)))
		r.RemoteAddr = "198.51.100.7:4000"
		w := httptest.NewRecorder()
		route.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < joinAdmits; i++ {
		if code := do(node.JoinOpen); code != http.StatusOK {
			t.Fatalf("open %d: %d", i, code)
		}
	}
	if code := do(node.JoinOpen); code != http.StatusTooManyRequests {
		t.Fatalf("an open past the budget: %d", code)
	}
	for i := 0; i < joinPolls; i++ {
		if code := do(node.JoinPoll); code != http.StatusOK {
			t.Fatalf("poll %d after the opens were spent: %d", i, code)
		}
	}
	if code := do(node.JoinPoll); code != http.StatusTooManyRequests {
		t.Fatalf("a poll past its budget: %d", code)
	}
}

// memberJoinH is joinH whose listener sorts keys by the rooms of r.
type memberJoinH struct {
	*joinH
	r *node.Rooms
}

func (h memberJoinH) SharesRoom(n string) bool { return h.r.SharesRoom(n) }

// A joiner shares no room until it is signed in: it previews, accepts and
// resumes over the strangers' pool, one connection per request, and once
// signed the listener counts it as a member.
//
// Requirements: T3; PROTOCOL 4.6, 5.3
func TestAJoinerThatSharesNoRoomYetJoinsThroughTheStrangersPool(t *testing.T) {
	set(t, &maxStrangerConns, 1)
	ida, idb := newID(t), newID(t)
	a, b := machine(t, ida), machine(t, idb)
	h := memberJoinH{joinH: &joinH{js: node.NewJoinServer(a)}, r: a}
	addr := serve(t, ida, h, nil)
	room, err := a.CreateRoom(bg, "team")
	if err != nil {
		t.Fatal(err)
	}
	inv, err := a.Invite(bg, room, "", false, []string{addr})
	if err != nil {
		t.Fatal(err)
	}
	j := node.NewJoiner(b, NewJoinClient(idb), node.JoinerOpts{Name: "bee", Sleep: noSleep})
	p, err := j.JoinPreview(bg, inv.Token)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if o, err := j.JoinAccept(bg, inv.Token, p.ID, 0); err != nil || o.State != node.JoinStateAccepted {
		t.Fatalf("accept: %+v %v", o, err)
	}
	if a.SharesRoom(idb.NodeID()) {
		t.Fatal("the joiner counts as a member before it is signed")
	}
	qs, err := a.Requests(bg)
	if err != nil || len(qs) != 1 {
		t.Fatalf("requests: %+v %v", qs, err)
	}
	if _, err := a.Confirm(bg, qs[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if os, err := j.Resume(bg, time.Minute); err != nil || len(os) != 1 || os[0].State != node.JoinStateSigned {
		t.Fatalf("resume: %+v %v", os, err)
	}
	if !a.SharesRoom(idb.NodeID()) {
		t.Fatal("the signed joiner does not count as a member")
	}
}

// ---- authentication, wider prefixes, keys and membership ----

// noKey signs with garbage: a client that copied a member's certificate
// but does not hold its key.
type noKey struct{ pub ed25519.PublicKey }

func (k noKey) Public() crypto.PublicKey { return k.pub }
func (noKey) Sign(io.Reader, []byte, crypto.SignerOpts) ([]byte, error) {
	return make([]byte, ed25519.SignatureSize), nil
}

// cutConn passes the client's first write (its hello), then of what
// follows only the records up to the first encrypted one, the client's
// Certificate: the server holds the certificate and waits for the
// signature that would prove the key, which never comes.
type cutConn struct {
	net.Conn
	writes int
	buf    []byte
	cut    bool
}

func (c *cutConn) Write(p []byte) (int, error) {
	if c.writes++; c.writes == 1 {
		return c.Conn.Write(p)
	}
	if c.cut {
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	for len(c.buf) >= 5 && !c.cut {
		n := 5 + int(binary.BigEndian.Uint16(c.buf[3:5]))
		if len(c.buf) < n {
			break
		}
		if _, err := c.Conn.Write(c.buf[:n]); err != nil {
			return 0, err
		}
		c.cut = c.buf[0] == 23 // application data: the first encrypted record
		c.buf = c.buf[n:]
	}
	return len(p), nil
}

// A client that presents a member's certificate without its key, and
// stops before the signature that proves the key, stays in the handshake
// pool: its connections count against its source, and it takes no
// member's slot.
//
// Requirements: N1; PROTOCOL 4.6
func TestACopiedMemberCertificateWithoutItsKeyTakesNoMemberSlot(t *testing.T) {
	set(t, &maxPerSource, 3)
	set(t, &handshakeTimeout, 10*time.Second)
	h := &memberH{}
	addr := serveFrom(t, newID(t), h, "198.51.100.7")[0]
	victim := h.add(newID(t))
	cert, err := selfSigned(victim)
	if err != nil {
		t.Fatal(err)
	}
	copied := tls.Certificate{Certificate: cert.Certificate, PrivateKey: noKey{victim.Key.Public().(ed25519.PublicKey)}}
	var raw []net.Conn
	for i := 0; i < 4*maxPerSource; i++ {
		d, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })
		tc := tls.Client(&cutConn{Conn: d}, &tls.Config{MinVersion: tls.VersionTLS13,
			Certificates: []tls.Certificate{copied}, InsecureSkipVerify: true})
		_ = tc.SetDeadline(time.Now().Add(time.Second))
		_ = tc.Handshake() // the client's side ends once it has written its flight
		raw = append(raw, d)
	}
	if n := kept(raw); n != maxPerSource {
		t.Fatalf("%d connections that never proved the key were kept, the source's share is %d", n, maxPerSource)
	}
}

// Hosts in several source groups keep every shared handshake slot full;
// a member that completed a handshake from its address before still opens
// new connections, through the slots kept for known member sources.
//
// Requirements: N1; PROTOCOL 4.6
func TestAHandshakeFloodFromManySourcesDoesNotKeepOutAKnownMember(t *testing.T) {
	set(t, &maxPendingConns, 8)
	set(t, &maxKnownPending, 2)
	set(t, &maxPerSource, 8)
	set(t, &handshakeTimeout, 10*time.Second)
	h := &memberH{}
	addrs := serveFrom(t, newID(t), h, "198.51.100.1", "198.51.100.2", "198.51.100.3", "198.51.100.4", "203.0.113.5")
	id := h.add(newID(t))
	body := string(mustBody(t, opening()))
	first := keyClient(t, id)
	if code, err := post(first, addrs[4], pathPullV2, body); err != nil || code != http.StatusOK {
		t.Fatalf("before the flood: %d %v", code, err)
	}
	first.CloseIdleConnections()
	var held []net.Conn
	for _, a := range addrs[:4] {
		held = append(held, holdMany(t, a, maxPerSource, false)...)
	}
	if n := kept(held); n != maxPendingConns {
		t.Fatalf("the flood holds %d handshake slots, the test needs all %d", n, maxPendingConns)
	}
	for i := 0; i < 3; i++ { // each a new connection
		c := keyClient(t, id)
		c.Timeout = time.Second
		if code, err := post(c, addrs[4], pathPullV2, body); err != nil || code != http.StatusOK {
			t.Fatalf("a known member's new connection %d was not served during the flood: %d %v", i, code, err)
		}
		c.CloseIdleConnections()
	}
}

// Addresses in many /64s of one IPv6 /56, or of one /48, hold no more
// connections outside the members' pool than that prefix's share.
//
// Requirements: N1; PROTOCOL 4.6
func TestOneIPv6Slash56OrSlash48HoldsNoMoreThanItsShare(t *testing.T) {
	set(t, &maxPerSource, 2)
	set(t, &maxPerSlash56, 3)
	set(t, &maxPerSlash48, 5)
	for _, tc := range []struct {
		name   string
		labels []string
		share  *int
	}{
		{"/56", []string{"2001:db8:0:1::1", "2001:db8:0:2::1", "2001:db8:0:3::1"}, &maxPerSlash56},
		{"/48", []string{"2001:db8:0:100::1", "2001:db8:0:200::1", "2001:db8:0:300::1", "2001:db8:0:400::1"}, &maxPerSlash48},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var held []net.Conn
			for _, a := range serveFrom(t, newID(t), &memberH{}, tc.labels...) {
				held = append(held, holdMany(t, a, maxPerSource, false)...)
			}
			if n := kept(held); n != *tc.share {
				t.Fatalf("one %s holds %d connections, its share is %d", tc.name, n, *tc.share)
			}
		})
	}
}

// holdKey completes a handshake under id's key and sends nothing.
func holdKey(t *testing.T, addr string, id *Identity) net.Conn {
	t.Helper()
	cert, err := selfSigned(id)
	if err != nil {
		t.Fatal(err)
	}
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", addr, &tls.Config{MinVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// One member's key holds at most its share of the members' slots; another
// member is still served.
//
// Requirements: N1; PROTOCOL 4.6
func TestOneMemberKeyHoldsAtMostItsShareOfMemberSlots(t *testing.T) {
	set(t, &maxConns, 8)
	set(t, &maxPerKey, 3)
	h := &memberH{}
	addr := serveFrom(t, newID(t), h, "198.51.100.7")[0]
	hostile := h.add(newID(t))
	var held []net.Conn
	for i := 0; i < maxConns; i++ {
		held = append(held, holdKey(t, addr, hostile))
	}
	if n := kept(held); n != maxPerKey {
		t.Fatalf("one key holds %d member connections, its share is %d", n, maxPerKey)
	}
	c := keyClient(t, h.add(newID(t)))
	c.Timeout = time.Second
	if code, err := post(c, addr, pathPullV2, string(mustBody(t, opening()))); err != nil || code != http.StatusOK {
		t.Fatalf("another member was not served: %d %v", code, err)
	}
}

// do sends one HTTP/1.1 pull over c and reads the answer.
func do(t *testing.T, c net.Conn, br *bufio.Reader, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "https://peer"+pathPullV2, strings.NewReader(body))
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if err := req.Write(c); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	_ = c.SetDeadline(time.Time{})
	return resp
}

// Membership is checked again for every request on a connection: a key
// that became a member after its handshake is served as a member, and
// its connection is kept past a stranger's life; a key removed since its
// handshake is served as a stranger, and its connection ends.
//
// Requirements: N1, T5; PROTOCOL 4.6
func TestMembershipIsCheckedAgainForEveryRequestOnAConnection(t *testing.T) {
	set(t, &strangerLife, 300*time.Millisecond)
	h := &memberH{}
	addr := serveFrom(t, newID(t), h, "198.51.100.7")[0]
	body := string(mustBody(t, opening()))

	joiner := newID(t)
	c := holdKey(t, addr, joiner)
	time.Sleep(100 * time.Millisecond) // the server places it as a stranger's
	h.add(joiner)
	br := bufio.NewReader(c)
	if r := do(t, c, br, body); r.StatusCode != http.StatusOK || r.Close {
		t.Fatalf("a key that became a member: %d, close %v", r.StatusCode, r.Close)
	}
	time.Sleep(2 * strangerLife)
	if r := do(t, c, br, body); r.StatusCode != http.StatusOK {
		t.Fatalf("after a stranger's life: %d", r.StatusCode)
	}

	removed := h.add(newID(t))
	c = holdKey(t, addr, removed)
	br = bufio.NewReader(c)
	if r := do(t, c, br, body); r.StatusCode != http.StatusOK || r.Close {
		t.Fatalf("a member: %d, close %v", r.StatusCode, r.Close)
	}
	h.members.Delete(removed.NodeID())
	if r := do(t, c, br, body); !r.Close {
		t.Fatal("a removed key's connection was kept as a member's")
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("a removed key's connection was not closed")
	}
}

// memberFileH serves files to everyone, with members fixed as in memberH.
type memberFileH struct {
	*memberH
	*slowServe
}

// The file route's rate table, filled by keys that share no room, still
// admits a member's first request.
//
// Requirements: N1, B7; PROTOCOL 4.6
func TestFileRateTableFullOfStrangersStillServesMembers(t *testing.T) {
	set(t, &rateMaxKeys, 4)
	set(t, &ratePerSec, 1e-6)
	m := &memberH{}
	h := memberFileH{m, &slowServe{size: 1, closed: make(chan struct{}),
		limits: node.RateLimits{Total: time.Minute, Window: time.Second, MinRate: 1, Now: time.Now}}}
	addr := serveFrom(t, newID(t), h, "198.51.100.7")[0]
	url := "https://" + addr + filePathOf("r_"+strings.Repeat("0", 32), strings.Repeat("1", 32), strings.Repeat("e", 64))
	get := func(c *http.Client) int {
		resp, err := c.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	refused := 0
	for i := 0; i < 3*rateMaxKeys; i++ {
		if get(keyClient(t, newID(t))) == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused == 0 {
		t.Fatal("the strangers' file table never filled")
	}
	if code := get(keyClient(t, m.add(newID(t)))); code != http.StatusOK {
		t.Fatalf("a member's file request was refused while strangers filled their table: %d", code)
	}
}
