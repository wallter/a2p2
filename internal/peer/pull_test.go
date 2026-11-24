package peer

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// pullH serves the pull exchange (and counts joins, never expected here).
type pullH struct {
	countH
	x     atomic.Pointer[node.Exchange]
	pulls atomic.Int64
}

func (h *pullH) Serve(ctx context.Context, from string, q *node.PullRequest) (*node.PullResponse, error) {
	h.pulls.Add(1)
	return h.x.Load().Serve(ctx, from, q)
}

// pm is one next-version machine with its own store, its pull client and,
// if it listens, the pull endpoint on a loopback listener.
type pm struct {
	t     *testing.T
	name  string
	id    *Identity
	st    *store.Store
	m     *node.Machine
	r     *node.Rooms
	book  *node.AddrBook
	cl    *PullClient
	x     *node.Exchange
	h     *pullH
	addr  string
	agent string
}

// newPM makes a machine; wrap, if given, wraps its listener.
func newPM(t *testing.T, name string, listen bool, wrap ...func(net.Listener) net.Listener) *pm {
	t.Helper()
	id := newID(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := node.NewMachine(st, id.Key, node.MachineOpts{})
	r, err := node.OpenRooms(bg, m)
	if err != nil {
		t.Fatal(err)
	}
	p := &pm{t: t, name: name, id: id, st: st, m: m, r: r, book: node.NewAddrBook(), agent: name}
	p.cl = NewPullClient(id, p.book)
	t.Cleanup(p.cl.Close)
	var addrs []string
	if listen {
		p.h = &pullH{}
		var w func(net.Listener) net.Listener
		if len(wrap) > 0 {
			w = wrap[0]
		}
		p.addr = serve(t, id, p.h, w)
		addrs = []string{p.addr}
	}
	p.x = node.NewExchange(r, p.cl, node.ExchangeOpts{Book: p.book, Addrs: addrs})
	if listen {
		p.h.x.Store(p.x)
	}
	if _, err := m.Register(bg, name); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *pm) node() string { return p.id.NodeID() }

// knows tells p where o listens (what an invite or join would).
func (p *pm) knows(o ...*pm) {
	for _, x := range o {
		if x.addr != "" {
			p.book.Learn(x.node(), []string{x.addr}, time.Now())
		}
	}
}

func (p *pm) pull(room string, from *pm, o node.PullOpts) (node.PullStats, error) {
	return p.x.Pull(bg, room, from.node(), o)
}

func (p *pm) mustPull(room string, from *pm, o node.PullOpts) {
	p.t.Helper()
	if _, err := p.pull(room, from, o); err != nil {
		p.t.Fatalf("%s pulls from %s: %v", p.name, from.name, err)
	}
}

func (p *pm) send(room, text string, ask bool) string {
	p.t.Helper()
	s, err := p.m.Send(bg, node.Draft{Room: room, Agent: p.agent, Text: text, AskHandled: ask})
	if err != nil {
		p.t.Fatal(err)
	}
	return s.ID
}

func (p *pm) holds(room, id string) bool {
	p.t.Helper()
	found := false
	err := p.st.View(bg, func(rd *store.Reader) error {
		_, ok, err := rd.Record(room, id)
		found = ok
		return err
	})
	if err != nil {
		p.t.Fatal(err)
	}
	return found
}

func (p *pm) inboxText(room string) []string {
	p.t.Helper()
	v, err := p.m.Inbox(bg, room, p.agent)
	if err != nil {
		p.t.Fatal(err)
	}
	var out []string
	for _, e := range v.Entries {
		out = append(out, e.Text)
	}
	return out
}

// memJoin carries a join request straight to the admin machine's join
// server, as the joiner's key: the join exchange has its own tests over
// TLS (join_test.go); here it only gets machines into a room.
type memJoin struct {
	js     *node.JoinServer
	server string
	from   string
}

func (l memJoin) Join(ctx context.Context, _ []string, check func(string) error, body []byte) (node.JoinReply, error) {
	if err := check(l.server); err != nil {
		return node.JoinReply{}, err
	}
	return l.js.Handle(ctx, l.from, l.from, body), nil
}

// group makes a room on admin and joins the others to it as members with
// history, each through an invite its person and the admin's approve;
// then each fetches the log from admin over TLS, so all know all.
func group(t *testing.T, admin *pm, others ...*pm) string {
	t.Helper()
	room, err := admin.r.CreateRoom(bg, "team")
	if err != nil {
		t.Fatal(err)
	}
	js := node.NewJoinServer(admin.r)
	for _, o := range others {
		inv, err := admin.r.Invite(bg, room, node.RoleMember, true, []string{"127.0.0.1:1"})
		if err != nil {
			t.Fatal(err)
		}
		j := node.NewJoiner(o.r, memJoin{js: js, server: admin.node(), from: o.node()},
			node.JoinerOpts{Name: o.name, Sleep: noSleep})
		pv, err := j.JoinPreview(bg, inv.Token)
		if err != nil {
			t.Fatalf("%s previews: %v", o.name, err)
		}
		if out, err := j.JoinAccept(bg, inv.Token, pv.ID, 0); err != nil || out.State != node.JoinStateAccepted {
			t.Fatalf("%s accepts: %+v %v", o.name, out, err)
		}
		qs, err := admin.r.Requests(bg)
		if err != nil || len(qs) != 1 {
			t.Fatalf("requests: %+v %v", qs, err)
		}
		if c, err := admin.r.Confirm(bg, qs[0].ID, true); err != nil || c.State != store.RequestApproved {
			t.Fatalf("confirm: %+v %v", c, err)
		}
		if os, err := j.Resume(bg, time.Minute); err != nil || len(os) != 1 || os[0].State != node.JoinStateSigned {
			t.Fatalf("%s resumes: %+v %v", o.name, os, err)
		}
	}
	for _, o := range others {
		o.knows(admin)
		o.mustPull(room, admin, node.PullOpts{})
		if st, _ := o.r.State(room); len(st.Members) != len(others)+1 {
			t.Fatalf("%s members after the first pull: %v", o.name, st.Members)
		}
	}
	return room
}

// within polls cond every 2 ms until it holds or 10 s pass.
func within(t *testing.T, what string, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !cond() {
		if time.Since(start) > 10*time.Second {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
	return time.Since(start)
}

func runSched(t *testing.T, ps ...*pm) {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{}, len(ps))
	for _, p := range ps {
		s := node.NewScheduler(p.x, node.SchedOpts{})
		go func() { s.Run(ctx); done <- struct{}{} }()
	}
	t.Cleanup(func() {
		cancel()
		for range ps {
			<-done
		}
	})
}

// Three machines with their own stores over TLS on loopback, each running
// the scheduler: a message reaches the other two through waiting pulls
// released by its arrival, and a reader's handled report comes back to the
// author's machine.
//
// Requirements: D4
func TestOverTLSAMessageReachesThreeMachinesAndItsHandledReportReturns(t *testing.T) {
	a, b, c := newPM(t, "alice", true), newPM(t, "bob", true), newPM(t, "carol", true)
	room := group(t, a, b, c)
	for _, p := range []*pm{a, b, c} {
		p.knows(a, b, c)
	}
	runSched(t, a, b, c)
	warm := a.send(room, "warm up", false)
	within(t, "the first message everywhere", func() bool { return b.holds(room, warm) && c.holds(room, warm) })
	// Measured from the write, once b and c each hold a waiting pull at a
	// again (after the warm-up released theirs).
	bHeld, cHeld := a.x.NextHold(room, b.node()), a.x.NextHold(room, c.node())
	_ = a.send(room, "warm up again", false)
	<-bHeld
	<-cHeld
	start := time.Now()
	id := a.send(room, "please look", true)
	within(t, "the message everywhere", func() bool { return b.holds(room, id) && c.holds(room, id) })
	t.Logf("a message reached the other two machines in %v", time.Since(start))
	for _, p := range []*pm{b, c} {
		if got := p.inboxText(room); !strings.Contains(strings.Join(got, "|"), "please look") {
			t.Fatalf("%s inbox: %q", p.name, got)
		}
	}
	v, err := c.m.Inbox(bg, room, c.agent)
	if err != nil {
		t.Fatal(err)
	}
	var reads []store.Read
	for _, e := range v.Entries {
		reads = append(reads, store.Read{Key: e.Key, Version: e.Version})
	}
	if _, err := c.m.Seen(bg, room, c.agent, reads); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	h, err := c.m.Handled(bg, room, c.agent, id)
	if err != nil {
		t.Fatal(err)
	}
	within(t, "the handled report back at the author", func() bool { return a.holds(room, h.ID) })
	t.Logf("the handled report came back in %v", time.Since(start))
}

// A waiting pull is held while nothing changes and released by an arrival
// at the server within milliseconds; the requester then takes it.
//
// Requirements: D4
func TestOverTLSAWaitingPullIsReleasedByAnArrival(t *testing.T) {
	a, b := newPM(t, "alice", true), newPM(t, "bob", false)
	room := group(t, a, b)
	got := make(chan error, 1)
	var returned atomic.Int64
	held := a.x.NextHold(room, b.node())
	go func() {
		_, err := b.pull(room, a, node.PullOpts{WaitMS: 60000})
		returned.Store(time.Now().UnixNano())
		got <- err
	}()
	<-held
	if returned.Load() != 0 {
		t.Fatal("the waiting pull returned before anything changed")
	}
	sent := time.Now()
	id := a.send(room, "now", false)
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	took := time.Duration(returned.Load() - sent.UnixNano())
	t.Logf("released and applied %v after the write", took)
	if took > time.Second || !b.holds(room, id) {
		t.Fatalf("released after %v, holds %v", took, b.holds(room, id))
	}
}

// A peer removed while its pull waits gets the 403 and no page.
//
// Requirements: T5
func TestOverTLSAPeerRemovedWhileItsPullWaitsGetsNotMember(t *testing.T) {
	a, b, c := newPM(t, "alice", true), newPM(t, "bob", false), newPM(t, "carol", false)
	room := group(t, a, b, c)
	got := make(chan error, 1)
	held := a.x.NextHold(room, b.node())
	go func() {
		_, err := b.pull(room, a, node.PullOpts{WaitMS: 60000})
		got <- err
	}()
	<-held
	q, err := a.r.Propose(bg, room, node.Change{Kind: core.EntryRemove, Target: b.node()})
	if err != nil {
		t.Fatal(err)
	}
	if cf, err := a.r.Confirm(bg, q.ID, true); err != nil || cf.State != store.RequestApproved {
		t.Fatalf("remove: %+v %v", cf, err)
	}
	id := a.send(room, "after the removal", false)
	err = <-got
	var pe *node.PullError
	if !errors.As(err, &pe) || pe.Status != 403 || pe.Code != node.CodeNotMember || pe.Detail != "" {
		t.Fatalf("want a bare 403 not_member, got %v", err)
	}
	if b.holds(room, id) {
		t.Fatal("the removed peer got a page")
	}
}

// The requester's TLS pins the server's key: a stranger at the address
// fails the handshake and is sent no request. A stranger's own request is
// refused with the one not_member body.
//
// Requirements: T1, T2, N2
func TestOverTLSAStrangersHandshakeFailsAndItsRequestIsRefused(t *testing.T) {
	a, b, s := newPM(t, "alice", true), newPM(t, "bob", false), newPM(t, "stranger", true)
	room := group(t, a, b)
	forget(b.book, a.node())
	b.book.Replace(a.node(), []string{s.addr}, time.Now()) // a's address now answers with another key
	_, err := b.pull(room, a, node.PullOpts{})
	var pe *node.PullError
	if err == nil || errors.As(err, &pe) || !strings.Contains(err.Error(), "expected node") {
		t.Fatalf("want a handshake failure, got %v", err)
	}
	if n := s.h.pulls.Load(); n != 0 {
		t.Fatalf("the stranger was sent %d requests", n)
	}
	// The stranger asks a for the room with a well-formed request.
	s.knows(a)
	q := &node.PullRequest{V: node.PullVersion, Room: room, Sum: strings.Repeat("0", 64),
		Log: node.PullLog{Heads: []string{strings.Repeat("1", 32)}}}
	_, err = s.cl.Pull(bg, a.node(), q)
	if !errors.As(err, &pe) || pe.Status != 403 || pe.Code != node.CodeNotMember || pe.Detail != "" {
		t.Fatalf("want 403 not_member, got %v", err)
	}
	status, body, _ := rawPost(t, s.id, a, mustBody(t, q), -1)
	if status != 403 || string(body) != `{"error":"not_member"}` {
		t.Fatalf("stranger: %d %s", status, body)
	}
}

// forget drops what book holds of n.
func forget(book *node.AddrBook, n string) { book.Keep(func(o string) bool { return o != n }) }

func mustBody(t *testing.T, q *node.PullRequest) []byte {
	t.Helper()
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rawPost posts body to to's pull endpoint as from, over HTTP/2 with
// pinned TLS; size, if not -1, is the Content-Length announced and body is
// then a lazy reader of that many bytes, counting what was read.
func rawPost(t *testing.T, from *Identity, to *pm, body []byte, size int64) (int, []byte, http.Header) {
	t.Helper()
	cert, err := selfSigned(from)
	if err != nil {
		t.Fatal(err)
	}
	protos := new(http.Protocols)
	protos.SetHTTP2(true)
	tr := &http.Transport{Protocols: protos, TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			got, err := certNode(raw)
			if err == nil && got != to.node() {
				err = errors.New("wrong key")
			}
			return err
		}}}
	defer tr.CloseIdleConnections()
	var rd io.Reader = bytes.NewReader(body)
	if size >= 0 {
		rd = io.LimitReader(zeros{}, size)
	}
	req, err := http.NewRequestWithContext(bg, http.MethodPost, "https://"+to.addr+pathPullV2, rd)
	if err != nil {
		t.Fatal(err)
	}
	if size >= 0 {
		req.ContentLength = size
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("pull over %s, want HTTP/2", resp.Proto)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

// A body over the limit gets 413 before it is read: the node never sees
// it.
//
// Requirements: PROTOCOL-next 4.4 step 2, 4.6
func TestOverTLSAnOversizedBodyGets413BeforeItIsRead(t *testing.T) {
	a, b := newPM(t, "alice", true), newPM(t, "bob", false)
	group(t, a, b)
	before := a.h.pulls.Load()
	status, body, _ := rawPost(t, b.id, a, nil, node.MaxRequestBytes+1)
	if status != 413 || string(body) != `{"error":"too_large"}` {
		t.Fatalf("oversized: %d %s", status, body)
	}
	if a.h.pulls.Load() != before {
		t.Fatal("the node was handed an oversized body")
	}
}

// Malformed and non-strict JSON (PROTOCOL-next section 0) get 400.
//
// Requirements: PROTOCOL-next section 0, 4.4 step 2
func TestOverTLSMalformedOrNonStrictJSONGets400(t *testing.T) {
	a, b := newPM(t, "alice", true), newPM(t, "bob", false)
	room := group(t, a, b)
	sum, head := strings.Repeat("0", 64), strings.Repeat("1", 32)
	ok := fmt.Sprintf(`{"v":2,"room":%q,"sum":%q,"log":{"heads":[%q]}}`, room, sum, head)
	if status, _, _ := rawPost(t, b.id, a, []byte(ok), -1); status != 200 && status != 403 {
		t.Fatalf("the well-formed request: %d", status)
	}
	for name, body := range map[string]string{
		"not json":       `{"v":2,`,
		"duplicate key":  fmt.Sprintf(`{"v":2,"v":2,"room":%q,"sum":%q,"log":{"heads":[%q]}}`, room, sum, head),
		"null":           fmt.Sprintf(`{"v":2,"room":%q,"sum":%q,"seen":null,"log":{"heads":[%q]}}`, room, sum, head),
		"present zero":   fmt.Sprintf(`{"v":2,"room":%q,"sum":%q,"wait_ms":0,"log":{"heads":[%q]}}`, room, sum, head),
		"case of a key":  fmt.Sprintf(`{"v":2,"Room":%q,"sum":%q,"log":{"heads":[%q]}}`, room, sum, head),
		"trailing bytes": ok + `{}`,
		"wrong version":  fmt.Sprintf(`{"v":3,"room":%q,"sum":%q,"log":{"heads":[%q]}}`, room, sum, head),
		"bad utf-8":      fmt.Sprintf(`{"v":2,"room":%q,"sum":%q,"x":"\xff","log":{"heads":[%q]}}`, room, sum, head),
		"wait too long":  fmt.Sprintf(`{"v":2,"room":%q,"sum":%q,"wait_ms":60001,"log":{"heads":[%q]}}`, room, sum, head),
	} {
		status, resp, _ := rawPost(t, b.id, a, []byte(body), -1)
		if status != 400 || !strings.HasPrefix(string(resp), `{"error":"bad_request"`) {
			t.Errorf("%s: %d %s", name, status, resp)
		}
	}
}

// More than the burst of requests at once: 429 rate_limited with a
// Retry-After the client maps back.
//
// Requirements: PROTOCOL-next 4.5, 4.6
func TestOverTLSTheRateLimitGives429WithRetryAfter(t *testing.T) {
	a, b := newPM(t, "alice", true), newPM(t, "bob", false)
	room := group(t, a, b)
	q := &node.PullRequest{V: node.PullVersion, Room: "r_" + strings.Repeat("ab", 16), Sum: strings.Repeat("0", 64),
		Log: node.PullLog{Heads: []string{strings.Repeat("1", 32)}}}
	var limited *node.PullError
	for i := 0; i < 200 && limited == nil; i++ {
		_, err := b.cl.Pull(bg, a.node(), q)
		var pe *node.PullError
		if errors.As(err, &pe) && pe.Status == 429 {
			limited = pe
		}
	}
	if limited == nil || limited.Code != node.CodeRateLimited || limited.RetryAfter < time.Second {
		t.Fatalf("no 429 with Retry-After: %+v", limited)
	}
	status, body, hdr := rawPost(t, b.id, a, mustBody(t, q), -1)
	if status != 429 || hdr.Get("Retry-After") == "" || !strings.HasPrefix(string(body), `{"error":"rate_limited"`) {
		t.Fatalf("raw: %d %s %v", status, body, hdr)
	}
	_ = room
}

// N1: a machine with no listener converges with two others through its
// own requests only: its writes go out as offers, and it learns the third
// machine's address from a response's peers.
//
// Requirements: N1
func TestOverTLSAMachineWithNoListenerConvergesThroughItsOwnRequests(t *testing.T) {
	a, c, n := newPM(t, "alice", true), newPM(t, "carol", true), newPM(t, "nomad", false)
	// c's first pull told a where c is (its addrs); n's first pull, after
	// it, was answered with c among the peers.
	room := group(t, a, c, n)
	// n had no address for c, so its pull in group asked for peers, and
	// the same answer listed c.
	if got := n.book.Addrs(c.node()); len(got) != 1 || got[0] != c.addr {
		t.Fatalf("n learned c at %v, want %s", got, c.addr)
	}
	c.knows(a)
	runSched(t, a, c, n)
	fromN := n.send(room, "from the road", false)
	fromA := a.send(room, "from the office", false)
	fromC := c.send(room, "from home", false)
	within(t, "convergence", func() bool {
		for _, p := range []*pm{a, c, n} {
			for _, id := range []string{fromN, fromA, fromC} {
				if !p.holds(room, id) {
					return false
				}
			}
		}
		return true
	})
	if a.book.Addrs(n.node()) != nil || c.book.Addrs(n.node()) != nil {
		t.Fatal("someone learned an address for the machine with no listener")
	}
}

// N2: a peer whose first address is dead is reached on its second, which
// is then tried first.
//
// Requirements: N2
func TestOverTLSAPeerWithADeadFirstAddressIsReachedOnItsSecond(t *testing.T) {
	a, b := newPM(t, "alice", true), newPM(t, "bob", false)
	room := group(t, a, b)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()
	forget(b.book, a.node())
	b.book.Replace(a.node(), []string{dead}, time.Now())
	b.book.Learn(a.node(), []string{a.addr}, time.Now())
	if got := b.book.Addrs(a.node()); got[0] != dead {
		t.Fatalf("addresses: %v", got)
	}
	id := a.send(room, "hello", false)
	b.mustPull(room, a, node.PullOpts{})
	if !b.holds(room, id) {
		t.Fatal("not reached")
	}
	if got := b.book.Addrs(a.node()); got[0] != a.addr {
		t.Fatalf("the address that worked is not first: %v", got)
	}
}

// countListener counts the connections it accepts.
type countListener struct {
	net.Listener
	n *atomic.Int64
}

func (l countListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.n.Add(1)
	}
	return c, err
}

// Waiting pulls of two rooms to one peer share one HTTP/2 connection.
//
// Requirements: P5
func TestOverTLSWaitingPullsToOnePeerShareOneConnection(t *testing.T) {
	var accepted atomic.Int64
	a := newPM(t, "alice", false)
	a.h = &pullH{}
	a.addr = serve(t, a.id, a.h, func(ln net.Listener) net.Listener { return countListener{ln, &accepted} })
	a.x = node.NewExchange(a.r, a.cl, node.ExchangeOpts{Book: a.book, Addrs: []string{a.addr}})
	a.h.x.Store(a.x)
	b := newPM(t, "bob", false)
	r1, r2 := group(t, a, b), group(t, a, b)
	errs := make(chan error, 2)
	h1, h2 := a.x.NextHold(r1, b.node()), a.x.NextHold(r2, b.node())
	for _, room := range []string{r1, r2} {
		go func() {
			_, err := b.pull(room, a, node.PullOpts{WaitMS: 60000})
			errs <- err
		}()
	}
	<-h1
	<-h2
	id1, id2 := a.send(r1, "one", false), a.send(r2, "two", false)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if !b.holds(r1, id1) || !b.holds(r2, id2) {
		t.Fatal("not released")
	}
	if n := accepted.Load(); n != 1 {
		t.Fatalf("%d connections, want 1", n)
	}
}

// The server's write deadline for a waiting pull is wait_ms plus the
// slack: a hold within it is answered, one past it is cut.
//
// Requirements: PROTOCOL-next 4.6
func TestOverTLSTheWriteDeadlineOfAWaitingPullIsItsWaitPlusTheSlack(t *testing.T) {
	a, b := newPM(t, "alice", true), newPM(t, "bob", false)
	room := group(t, a, b)
	old := pullSlack
	t.Cleanup(func() { pullSlack = old })
	pullSlack = 50 * time.Millisecond
	st, err := b.pull(room, a, node.PullOpts{WaitMS: 100})
	if err != nil || st.Same != 1 {
		t.Fatalf("within the deadline: %+v %v", st, err)
	}
	pullSlack = -80 * time.Millisecond // the deadline passes 80 ms before the hold ends
	t0 := time.Now()
	st, err = b.pull(room, a, node.PullOpts{WaitMS: 100})
	var pe *node.PullError
	if err == nil || errors.As(err, &pe) {
		t.Fatalf("past the deadline: %v %+v %v", err, st, time.Since(t0))
	}
}
