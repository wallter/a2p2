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
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/node"
)

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
func serve(t *testing.T, id *Identity, h Handler, wrap func(net.Listener) net.Listener) string {
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
	go func() { done <- Serve(ctx, ln, id, h) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return addr
}

// ---- a fake handler that counts calls ----

// countH answers every pull with a bare same and every join with 200 {},
// counting the calls and remembering who the transport said made them.
type countH struct {
	calls atomic.Int64
	from  atomic.Value
}

func (h *countH) Serve(_ context.Context, from string, q *node.PullRequest) (*node.PullResponse, error) {
	h.calls.Add(1)
	h.from.Store(from)
	return &node.PullResponse{V: node.PullVersion, Same: true, Log: node.PullLog{Heads: q.Log.Heads}}, nil
}

func (h *countH) Handle(_ context.Context, from, _ string, _ []byte) node.JoinReply {
	h.calls.Add(1)
	h.from.Store(from)
	return node.JoinReply{Status: http.StatusOK, Body: []byte(`{}`)}
}

// opening is a well-formed opening pull request for tRoom.
func opening() *node.PullRequest {
	return &node.PullRequest{V: node.PullVersion, Room: tRoom, Sum: strings.Repeat("0", 64), Log: node.PullLog{Heads: []string{tHead}}}
}

// The requester pins the server's key: a server holding another key gets
// a handshake and no request. With the right key, malformed addresses are
// skipped, and the server sees the requester's key from TLS.
//
// Requirements: T2
func TestPinningRefusesWrongServerKey(t *testing.T) {
	h := &countH{}
	y := newID(t)
	addr := serve(t, y, h, nil)
	client := newID(t)
	book := node.NewAddrBook()
	cl := NewPullClient(client, book)
	t.Cleanup(cl.Close)
	x := newID(t).NodeID()
	book.Replace(x, []string{addr}, time.Now())
	if _, err := cl.Pull(bg, x, opening()); err == nil || !strings.Contains(err.Error(), "expected node") {
		t.Fatalf("connected to a server holding the wrong key: %v", err)
	}
	if h.calls.Load() != 0 {
		t.Fatal("request reached the handler despite failed pinning")
	}
	book.Replace(y.NodeID(), []string{"nope", "h:x", ":1", addr}, time.Now())
	if _, err := cl.Pull(bg, y.NodeID(), opening()); err != nil {
		t.Fatalf("pinned to the right key: %v", err)
	}
	if h.from.Load() != client.NodeID() {
		t.Fatalf("handler saw node %v", h.from.Load())
	}
}

// The requester's node id is the key of its TLS certificate, whatever it
// sends: two keys posting the same bytes are seen as themselves.
//
// Requirements: T2
func TestFromNodeComesFromTLS(t *testing.T) {
	h := &countH{}
	addr := serve(t, newID(t), h, nil)
	for i := 0; i < 2; i++ {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		c := rawClient(certFor(t, pub, priv, hex.EncodeToString(pub)))
		if code, err := post(c, addr, pathPullV2, string(mustBody(t, opening()))); err != nil || code != 200 {
			t.Fatalf("pull: %d %v", code, err)
		}
		if got := h.from.Load(); got != hex.EncodeToString(pub) {
			t.Fatalf("handler saw %v, want the certificate's key", got)
		}
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

// Requirements: T7
func TestServerRejectsBadClientCerts(t *testing.T) {
	h := &countH{}
	addr := serve(t, newID(t), h, nil)
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	edPub, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	body := string(mustBody(t, opening()))
	for name, c := range map[string]*http.Client{
		"rsa":         rawClient(certFor(t, &rk.PublicKey, rk, "x")),
		"ecdsa":       rawClient(certFor(t, &ek.PublicKey, ek, "x")),
		"cn mismatch": rawClient(certFor(t, edPub, edPriv, newID(t).NodeID())),
		"no cert":     rawClient(),
	} {
		for _, path := range []string{pathPullV2, pathJoinV2} {
			if code, err := post(c, addr, path, body); err == nil {
				t.Errorf("%s on %s: accepted with status %d", name, path, code)
			}
		}
	}
	if h.calls.Load() != 0 {
		t.Fatal("a rejected client reached the handler")
	}
	good := rawClient(certFor(t, edPub, edPriv, hex.EncodeToString(edPub)))
	if code, err := post(good, addr, pathPullV2, body); err != nil || code != 200 {
		t.Fatalf("valid node cert refused: %d %v", code, err)
	}
}

// Only POST to the two v2 routes is served; the v0.2 routes are gone.
//
// Requirements: T8
func TestServerRefusesOversizeAndBadRoutes(t *testing.T) {
	h := &countH{}
	addr := serve(t, newID(t), h, nil)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	c := rawClient(certFor(t, pub, priv, hex.EncodeToString(pub)))
	for _, path := range []string{pathPullV2, pathJoinV2} {
		resp, err := c.Get("https://" + addr + path)
		if err != nil || resp.StatusCode == 200 {
			t.Errorf("GET %s accepted: %v", path, err)
		} else {
			resp.Body.Close()
		}
	}
	for _, path := range []string{"/p/v1/deliver", "/p/v1/join", "/p/v1/room", "/p/v2/nope"} {
		if code, err := post(c, addr, path, `{}`); err != nil || code != 404 {
			t.Errorf("%s: %d %v", path, code, err)
		}
	}
	big := `{"v":2,"room":"` + strings.Repeat("a", int(maxPullBody)) + `"}`
	if code, err := post(c, addr, pathPullV2, big); err != nil || code != 413 {
		t.Errorf("oversize pull: %d %v", code, err)
	}
	if h.calls.Load() != 0 {
		t.Fatal("handler called for a refused request")
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

// A message pulled over the wire is not visible in it: neither its text,
// its room id nor the exchange's field names.
//
// Requirements: T7
func TestWireIsEncrypted(t *testing.T) {
	rec := &recLn{}
	a := newPM(t, "alice", true, func(ln net.Listener) net.Listener { rec.Listener = ln; return rec })
	b := newPM(t, "bob", false)
	room := group(t, a, b)
	const secret = "PURPLE-ELEPHANT-7731"
	id := a.send(room, secret, false)
	b.mustPull(room, a, node.PullOpts{})
	if !b.holds(room, id) {
		t.Fatal("the message did not cross")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.buf.Len() < 1000 {
		t.Fatalf("recorder saw only %d bytes", rec.buf.Len())
	}
	for _, needle := range []string{secret, room, `"records"`, `"log"`} {
		if bytes.Contains(rec.buf.Bytes(), []byte(needle)) {
			t.Errorf("%q visible on the wire", needle)
		}
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

// Requirements: T2
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

// Connections that never finish a handshake hold at most maxConns slots;
// once they close, requests are served again.
func TestConnectionLimit(t *testing.T) {
	set(t, &maxConns, 3)
	h := &countH{}
	addr := serve(t, newID(t), h, nil)
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
	body := string(mustBody(t, opening()))
	if code, err := post(c, addr, pathPullV2, body); err == nil {
		t.Fatalf("served while at the connection limit: %d", code)
	}
	for _, ic := range idle {
		ic.Close()
	}
	c.Timeout = 5 * time.Second
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if code, err := post(c, addr, pathPullV2, body); err == nil && code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no request served after the slots freed")
		}
	}
}

// A peer over its rate is refused, recovers as its bucket refills, and
// another peer's bucket is its own.
func TestRateLimitTripsAndRecovers(t *testing.T) {
	addr := serve(t, newID(t), &countH{}, nil)
	client := func() *http.Client {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		return rawClient(certFor(t, pub, priv, hex.EncodeToString(pub)))
	}
	c, body := client(), string(mustBody(t, opening()))
	tripped := false
	for i := 0; i < 2000 && !tripped; i++ {
		code, err := post(c, addr, pathPullV2, body)
		tripped = err == nil && code == 429
	}
	if !tripped {
		t.Fatal("burst never hit the rate limit")
	}
	time.Sleep(200 * time.Millisecond) // a few tokens at 20 a second
	if code, err := post(c, addr, pathPullV2, body); err != nil || code != 200 {
		t.Fatalf("did not recover: %d %v", code, err)
	}
	if code, err := post(client(), addr, pathPullV2, body); err != nil || code != 200 {
		t.Fatalf("limit leaked across nodes: %d %v", code, err)
	}
}
