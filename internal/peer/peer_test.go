package peer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

func init() {
	node.RetryBase, node.RetryMax, node.Tick = 2*time.Millisecond, 20*time.Millisecond, 3*time.Millisecond
}

var bg = context.Background()

func newID(t *testing.T) *Identity {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &Identity{Key: k}
}

// serve runs Serve on a loopback listener (optionally wrapped) until the test ends.
func serve(t *testing.T, id *Identity, h Handler, refresh func(string), wrap func(net.Listener) net.Listener) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if wrap != nil {
		ln = wrap(ln)
	}
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, id, h, refresh) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return addr
}

// ---- real nodes ----

type realNode struct {
	id   *Identity
	n    *node.Node
	m    *node.Mesh
	st   *store.Store
	addr string
}

func startNode(t *testing.T, name string, wrap func(net.Listener) net.Listener) *realNode {
	t.Helper()
	id := newID(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := node.New(bg, st, id.NodeID(), name, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	rn := &realNode{id: id, n: n, st: st}
	rn.m = node.NewMesh(n, NewTransport(id), func() []string { return []string{rn.addr} })
	rn.addr = serve(t, id, n, func(room string) { go rn.m.Refresh(bg, room) }, wrap)
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { rn.m.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; st.Close() }) // runs before the listener's cleanup
	return rn
}

func (r *realNode) member() store.Member {
	return store.Member{Node: r.id.NodeID(), Name: "x", Addrs: []string{r.addr}}
}

func (r *realNode) agent(t *testing.T, label string) string {
	t.Helper()
	a, err := r.n.Register(bg, "test", label, "/tmp/"+label)
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func (r *realNode) state(t *testing.T, msg, to string) string {
	ds, err := r.st.Deliveries(bg, msg)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if d.Node == to {
			return d.State
		}
	}
	return ""
}

func joinAll(t *testing.T, owner *realNode, others ...*realNode) store.Room {
	t.Helper()
	room, err := owner.n.CreateRoom(bg, "team", []string{owner.addr})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range others {
		tok, _, err := owner.n.Invite(bg, room.ID, []string{owner.addr})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.m.Join(bg, tok); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	for _, o := range others {
		o.m.Refresh(bg, room.ID)
	}
	return room
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(3 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func handled(r *realNode, msg, agent string) bool {
	tr, _ := r.n.Trace(bg, msg)
	for _, k := range tr.Marks {
		if k.Agent == agent && k.State == core.ReceiptHandled {
			return true
		}
	}
	return false
}

func sendAndAck(t *testing.T, from *realNode, room string, to ...*realNode) {
	t.Helper()
	alice := from.agent(t, "alice")
	bobs := []string{} // registered first: new agents do not inherit history as unread
	for _, r := range to {
		bobs = append(bobs, r.agent(t, "bob"))
	}
	m, err := from.n.Send(bg, node.SendReq{From: alice, Room: room, Text: "hello over tls"})
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range to {
		eventually(t, "stored", func() bool { return from.state(t, m.ID, r.id.NodeID()) == store.Stored })
		bob := bobs[i]
		in, _ := r.n.Inbox(bg, bob, "", 10, 0)
		if len(in) != 1 || in[0].Body.Text != "hello over tls" || in[0].FromNode != from.id.NodeID() {
			t.Fatalf("inbox = %+v", in)
		}
		if err := r.n.Ack(bg, bob, m.ID); err != nil {
			t.Fatal(err)
		}
		eventually(t, "handled receipt", func() bool { return handled(from, m.ID, bob) })
	}
}

func TestTwoNodesDeliverAndReceipt(t *testing.T) {
	a, b := startNode(t, "a", nil), startNode(t, "b", nil)
	room := joinAll(t, a, b)
	sendAndAck(t, a, room.ID, b)
}

func TestThreeNodesNonOwnerSends(t *testing.T) {
	a, b, c := startNode(t, "a", nil), startNode(t, "b", nil), startNode(t, "c", nil)
	room := joinAll(t, a, b, c)
	sendAndAck(t, b, room.ID, a, c) // b -> c never involves the owner
}

// ---- a fake handler that counts calls ----

type countH struct {
	calls atomic.Int64
	from  atomic.Value
	err   error
}

func (h *countH) Receive(_ context.Context, from string, req node.DeliverReq, _ func(string)) []node.Result {
	h.calls.Add(1)
	h.from.Store(from)
	return []node.Result{}
}
func (h *countH) HandleJoin(_ context.Context, from string, _ node.JoinReq) (store.Room, error) {
	h.calls.Add(1)
	h.from.Store(from)
	return store.Room{ID: "r"}, h.err
}
func (h *countH) HandleRoom(_ context.Context, from string, _ node.RoomReq) (store.Room, error) {
	h.calls.Add(1)
	h.from.Store(from)
	return store.Room{ID: "r"}, h.err
}

func TestPinningRefusesWrongServerKey(t *testing.T) {
	h := &countH{}
	y := newID(t)
	addr := serve(t, y, h, nil, nil)
	tr := NewTransport(newID(t))
	x := newID(t).NodeID()
	if _, err := tr.Deliver(bg, store.Member{Node: x, Addrs: []string{addr}}, node.DeliverReq{}); err == nil {
		t.Fatal("connected to a server holding the wrong key")
	}
	if h.calls.Load() != 0 {
		t.Fatal("request reached the handler despite failed pinning")
	}
	// Same server, right expectation; malformed addresses are skipped.
	if _, err := tr.Room(bg, store.Member{Node: y.NodeID(), Addrs: []string{"nope", "h:x", ":1", addr}}, node.RoomReq{}); err != nil {
		t.Fatalf("pinned to the right key: %v", err)
	}
	if h.from.Load() != tr.id.NodeID() {
		t.Fatalf("handler saw node %v", h.from.Load())
	}
}

func TestStatusMapping(t *testing.T) {
	h := &countH{}
	srv := newID(t)
	m := store.Member{Node: srv.NodeID(), Addrs: []string{serve(t, srv, h, nil, nil)}}
	tr := NewTransport(newID(t))
	h.err = node.ErrForbidden
	if _, err := tr.Join(bg, m, node.JoinReq{}); !errors.Is(err, node.ErrForbidden) {
		t.Errorf("403: %v", err)
	}
	h.err = &node.UserError{Msg: "room is full"}
	if _, err := tr.Join(bg, m, node.JoinReq{}); err == nil || !strings.Contains(err.Error(), "room is full") {
		t.Errorf("400: %v", err)
	}
	h.err = errors.New("disk /secret/path exploded")
	if _, err := tr.Join(bg, m, node.JoinReq{}); err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("500 leaked or missing: %v", err)
	}
}

// ---- raw clients with chosen certificates ----

func certFor(t *testing.T, pub, priv any, cn string) tls.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

func rawClient(certs ...tls.Certificate) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, Certificates: certs, MinVersion: tls.VersionTLS13}}}
}

func post(c *http.Client, addr, path, body string) (int, error) {
	resp, err := c.Post("https://"+addr+path, "application/json", strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func TestServerRejectsBadClientCerts(t *testing.T) {
	h := &countH{}
	addr := serve(t, newID(t), h, nil, nil)
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	edPub, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	for name, c := range map[string]*http.Client{
		"rsa":         rawClient(certFor(t, &rk.PublicKey, rk, "x")),
		"ecdsa":       rawClient(certFor(t, &ek.PublicKey, ek, "x")),
		"cn mismatch": rawClient(certFor(t, edPub, edPriv, newID(t).NodeID())),
		"no cert":     rawClient(),
	} {
		if code, err := post(c, addr, pathRoom, `{}`); err == nil {
			t.Errorf("%s: accepted with status %d", name, code)
		}
	}
	if h.calls.Load() != 0 {
		t.Fatal("a rejected client reached the handler")
	}
	good := rawClient(certFor(t, edPub, edPriv, hex.EncodeToString(edPub)))
	if code, err := post(good, addr, pathRoom, `{}`); err != nil || code != 200 {
		t.Fatalf("valid node cert refused: %d %v", code, err)
	}
}

func TestServerRefusesOversizeAndBadRoutes(t *testing.T) {
	h := &countH{}
	addr := serve(t, newID(t), h, nil, nil)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	c := rawClient(certFor(t, pub, priv, hex.EncodeToString(pub)))
	big := `{"secret":"` + strings.Repeat("a", 20<<10) + `"}`
	if code, err := post(c, addr, pathJoin, big); err != nil || (code != 413 && code != 400) {
		t.Errorf("oversize join: %d %v", code, err)
	}
	resp, err := c.Get("https://" + addr + pathDeliver)
	if err != nil || resp.StatusCode == 200 {
		t.Errorf("GET accepted: %v", err)
	} else {
		resp.Body.Close()
	}
	if code, err := post(c, addr, "/p/v1/nope", `{}`); err != nil || code != 404 {
		t.Errorf("unknown path: %d %v", code, err)
	}
	if h.calls.Load() != 0 {
		t.Fatal("handler called for a refused request")
	}
}

// ---- strangers and spoofing ----

func envelope(fromNode, fromLabelNode, room, text string) core.Envelope {
	return core.Envelope{V: core.ProtocolVersion, ID: core.NewID(time.Now()), Room: room, FromNode: fromNode,
		From: core.AgentID("mallory", fromLabelNode), Kind: core.KindMessage, Seq: 1,
		CreatedAt: time.Now(), Body: core.Body{Text: text}}
}

func TestStrangerIsRefused(t *testing.T) {
	a, b := startNode(t, "a", nil), startNode(t, "b", nil)
	room := joinAll(t, a, b)
	s := newID(t)
	tr := NewTransport(s) // valid node cert, but not a member of anything
	res, err := tr.Deliver(bg, a.member(), node.DeliverReq{Envelopes: []core.Envelope{
		envelope(s.NodeID(), s.NodeID(), room.ID, "let me in"),
		envelope(b.id.NodeID(), b.id.NodeID(), room.ID, "i am b"), // claims to be a member
	}})
	if err != nil || len(res) != 2 {
		t.Fatalf("deliver: %v %v", res, err)
	}
	for _, r := range res {
		if r.Status != node.ResRejected {
			t.Errorf("stranger envelope %s: %+v", r.ID, r)
		}
	}
	if _, err := tr.Room(bg, a.member(), node.RoomReq{Room: room.ID}); !errors.Is(err, node.ErrForbidden) {
		t.Errorf("room: %v", err)
	}
	if _, err := tr.Join(bg, a.member(), node.JoinReq{Secret: "wrong"}); !errors.Is(err, node.ErrForbidden) {
		t.Errorf("join: %v", err)
	}
}

func TestFromNodeComesFromTLS(t *testing.T) {
	a, b, c := startNode(t, "a", nil), startNode(t, "b", nil), startNode(t, "c", nil)
	room := joinAll(t, a, b, c)
	tr := NewTransport(b.id) // b speaks, claiming in the payload to be c
	spoof := envelope(c.id.NodeID(), c.id.NodeID(), room.ID, "i am c")
	relabel := envelope(c.id.NodeID(), b.id.NodeID(), room.ID, "still b")
	res, err := tr.Deliver(bg, a.member(), node.DeliverReq{Envelopes: []core.Envelope{spoof, relabel}})
	if err != nil || len(res) != 2 {
		t.Fatalf("deliver: %v %v", res, err)
	}
	if res[0].Status != node.ResRejected {
		t.Errorf("spoofed sender accepted: %+v", res[0])
	}
	if res[1].Status != node.ResStored {
		t.Fatalf("relabelled: %+v", res[1])
	}
	if got, _ := a.st.Get(bg, relabel.ID); got.FromNode != b.id.NodeID() {
		t.Errorf("stored as from %s, want b", core.ShortNode(got.FromNode))
	}
}

// ---- wire privacy ----

type recLn struct {
	net.Listener
	mu  sync.Mutex
	buf bytes.Buffer
}

type recConn struct {
	net.Conn
	l *recLn
}

func (l *recLn) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return recConn{c, l}, nil
}

func (l *recLn) record(b []byte) { l.mu.Lock(); l.buf.Write(b); l.mu.Unlock() }

func (c recConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.l.record(b[:n])
	return n, err
}
func (c recConn) Write(b []byte) (int, error) { c.l.record(b); return c.Conn.Write(b) }

func TestWireIsEncrypted(t *testing.T) {
	rec := &recLn{}
	a := startNode(t, "a", nil)
	b := startNode(t, "b", func(ln net.Listener) net.Listener { rec.Listener = ln; return rec })
	room := joinAll(t, a, b)
	const secret = "PURPLE-ELEPHANT-7731"
	alice := a.agent(t, "alice")
	m, err := a.n.Send(bg, node.SendReq{From: alice, Room: room.ID, Text: secret})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "stored on b", func() bool { return a.state(t, m.ID, b.id.NodeID()) == store.Stored })
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.buf.Len() < 1000 {
		t.Fatalf("recorder saw only %d bytes", rec.buf.Len())
	}
	for _, needle := range []string{secret, room.ID, "envelopes"} {
		if bytes.Contains(rec.buf.Bytes(), []byte(needle)) {
			t.Errorf("%q visible on the wire", needle)
		}
	}
}

// ---- rate limit ----

func TestRateLimitTripsAndRecovers(t *testing.T) {
	h := &countH{}
	srv := newID(t)
	m := store.Member{Node: srv.NodeID(), Addrs: []string{serve(t, srv, h, nil, nil)}}
	tr := NewTransport(newID(t))
	tripped := false
	for i := 0; i < 2000 && !tripped; i++ {
		_, err := tr.Room(bg, m, node.RoomReq{})
		tripped = err != nil && strings.Contains(err.Error(), "429")
	}
	if !tripped {
		t.Fatal("burst never hit the rate limit")
	}
	time.Sleep(100 * time.Millisecond) // ~5 tokens at 50/s
	if _, err := tr.Room(bg, m, node.RoomReq{}); err != nil {
		t.Fatalf("did not recover: %v", err)
	}
	other := NewTransport(newID(t)) // another node has its own bucket
	if _, err := other.Room(bg, m, node.RoomReq{}); err != nil {
		t.Fatalf("limit leaked across nodes: %v", err)
	}
}

func TestLimiterBoundsKeys(t *testing.T) {
	l := newLimiter()
	for i := 0; i < 3*rateMaxKeys; i++ {
		l.allow(string(rune(i)))
	}
	if len(l.m) > rateMaxKeys {
		t.Fatalf("limiter holds %d keys", len(l.m))
	}
}

// ---- identity ----

func TestLoadIdentity(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "identity.key")
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode: %v %v", st.Mode(), err)
	}
	b, err := LoadIdentity(dir)
	if err != nil || b.NodeID() != a.NodeID() || !core.ValidNode(a.NodeID()) {
		t.Fatalf("reload: %v %v", b, err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadIdentity(dir); err == nil {
			t.Errorf("accepted key file with mode %o", mode)
		}
	}
}

func TestCertIsNodeID(t *testing.T) {
	id := newID(t)
	c, err := selfSigned(id)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := certNode(c.Certificate); err != nil || got != id.NodeID() {
		t.Fatalf("certNode = %s, %v", got, err)
	}
}

func TestDeliverBatchLimitBeforeDecode(t *testing.T) {
	h := &countH{}
	addr := serve(t, newID(t), h, nil, nil)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	c := rawClient(certFor(t, pub, priv, hex.EncodeToString(pub)))
	body := func(n int) string {
		return `{"envelopes":[` + strings.TrimSuffix(strings.Repeat("{},", n), ",") + `]}`
	}
	for _, n := range []int{core.MaxBatch + 1, 100000} {
		start := time.Now()
		if code, err := post(c, addr, pathDeliver, body(n)); err != nil || (code != 400 && code != 413) {
			t.Errorf("%d envelopes: %d %v", n, code, err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%d envelopes took %v", n, d)
		}
	}
	var vs []string
	for i := 0; i <= maxVersions; i++ {
		vs = append(vs, `"r`+strconv.Itoa(i)+`":1`)
	}
	if code, err := post(c, addr, pathDeliver, `{"envelopes":[],"versions":{`+strings.Join(vs, ",")+`}}`); err != nil || code != 400 {
		t.Errorf("many versions: %d %v", code, err)
	}
	big := `{"envelopes":[{"text":"` + strings.Repeat("a", core.MaxEnvelope) + `"}]}`
	if code, err := post(c, addr, pathDeliver, big); err != nil || code != 400 {
		t.Errorf("big envelope: %d %v", code, err)
	}
	if h.calls.Load() != 0 {
		t.Fatal("handler called for an over-limit request")
	}
	if code, err := post(c, addr, pathDeliver, body(core.MaxBatch)); err != nil || code != 200 {
		t.Fatalf("max batch refused: %d %v", code, err)
	}
	if h.calls.Load() != 1 {
		t.Fatal("handler not called for a valid batch")
	}
}

func TestConnectionLimit(t *testing.T) {
	old := maxConns
	maxConns = 3
	defer func() { maxConns = old }()
	h := &countH{}
	addr := serve(t, newID(t), h, nil, nil)
	var idle []net.Conn
	for i := 0; i < maxConns; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		idle = append(idle, c) // never handshakes: holds a slot until the server times it out
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	c := rawClient(certFor(t, pub, priv, hex.EncodeToString(pub)))
	c.Timeout = 700 * time.Millisecond
	if code, err := post(c, addr, pathRoom, `{}`); err == nil {
		t.Fatalf("served while at the connection limit: %d", code)
	}
	for _, ic := range idle {
		ic.Close()
	}
	c.Timeout = 5 * time.Second
	eventually(t, "request served after slots free", func() bool {
		code, err := post(c, addr, pathRoom, `{}`)
		return err == nil && code == 200
	})
}
