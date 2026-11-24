package peer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// joinH serves the join exchange of rooms (and counts pulls, never
// expected here), counting the bodies the join handler was given.
type joinH struct {
	countH
	js     *node.JoinServer
	bodies atomic.Int64
	from   atomic.Value
}

func (h *joinH) Handle(ctx context.Context, from, source string, body []byte) node.JoinReply {
	h.bodies.Add(1)
	h.from.Store(from)
	return h.js.Handle(ctx, from, source, body)
}

// machine is one next-version machine with its own store.
func machine(t *testing.T, id *Identity) *node.Rooms {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r, err := node.OpenRooms(bg, node.NewMachine(st, id.Key, node.MachineOpts{}))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func noSleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

// One whole join between two machines with separate stores over TLS on
// loopback: preview, accept, the admin's person approves, the joiner
// fetches the log; the server saw the joiner's key from TLS.
//
// Requirements: T3, T7, P3
func TestJoinOverTLSBetweenTwoMachines(t *testing.T) {
	ida, idb := newID(t), newID(t)
	a, b := machine(t, ida), machine(t, idb)
	h := &joinH{js: node.NewJoinServer(a)}
	addr := serve(t, ida, h, nil)
	room, err := a.CreateRoom(bg, "team")
	if err != nil {
		t.Fatal(err)
	}
	inv, err := a.Invite(bg, room, "", true, []string{addr})
	if err != nil {
		t.Fatal(err)
	}
	j := node.NewJoiner(b, NewJoinClient(idb), node.JoinerOpts{Name: "bee", Sleep: noSleep})
	p, err := j.JoinPreview(bg, inv.Token)
	if err != nil || p.Room != room || p.Admin != ida.NodeID() || p.Role != node.RoleAdmin || !p.History {
		t.Fatalf("preview: %+v %v", p, err)
	}
	if o, err := j.JoinAccept(bg, inv.Token, p.ID, 0); err != nil || o.State != node.JoinStateAccepted {
		t.Fatalf("accept: %+v %v", o, err)
	}
	qs, err := a.Requests(bg)
	if err != nil || len(qs) != 1 || qs[0].Node != idb.NodeID() {
		t.Fatalf("requests: %+v %v", qs, err)
	}
	if c, err := a.Confirm(bg, qs[0].ID, true); err != nil || c.State != store.RequestApproved {
		t.Fatalf("confirm: %+v %v", c, err)
	}
	os, err := j.Resume(bg, time.Minute)
	if err != nil || len(os) != 1 || os[0].State != node.JoinStateSigned {
		t.Fatalf("resume: %+v %v", os, err)
	}
	sa, _ := a.State(room)
	sb, err := b.State(room)
	if err != nil || !slices.Equal(sa.Members, sb.Members) || !slices.Equal(sa.Admins, sb.Admins) || len(sb.Admins) != 2 {
		t.Fatalf("states: %+v %+v %v", sa, sb, err)
	}
	if h.from.Load() != idb.NodeID() || h.calls.Load() != 0 {
		t.Fatalf("server saw %v; pull calls %d", h.from.Load(), h.calls.Load())
	}
}

// A machine holding another key answers at the invite's address: the
// joiner stops after the TLS handshake, and no request body reached it.
//
// Requirements: T3, T7
func TestJoinOverTLSToTheWrongKeySendsNothing(t *testing.T) {
	ida, idb, idm := newID(t), newID(t), newID(t)
	a, b := machine(t, ida), machine(t, idb)
	h := &joinH{js: node.NewJoinServer(machine(t, idm))}
	addr := serve(t, idm, h, nil)
	room, err := a.CreateRoom(bg, "team")
	if err != nil {
		t.Fatal(err)
	}
	inv, err := a.Invite(bg, room, "", false, []string{addr})
	if err != nil {
		t.Fatal(err)
	}
	j := node.NewJoiner(b, NewJoinClient(idb), node.JoinerOpts{Name: "bee", Sleep: noSleep})
	if _, err := j.JoinPreview(bg, inv.Token); !errors.Is(err, node.ErrWrongKey) {
		t.Fatalf("got %v, want wrong_key", err)
	}
	if h.bodies.Load() != 0 || h.calls.Load() != 0 {
		t.Fatalf("the server was given %d bodies", h.bodies.Load())
	}
}

// The join route refuses a body over its limit before reading it.
//
// Requirements: T3
func TestJoinRouteRefusesOversizeBodies(t *testing.T) {
	id := newID(t)
	h := &joinH{js: node.NewJoinServer(machine(t, id))}
	addr := serve(t, id, h, nil)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	c := rawClient(certFor(t, pub, priv, hex.EncodeToString(pub)))
	big := `{"secret":"` + strings.Repeat("a", int(maxJoinV2Body)) + `"}`
	if code, err := post(c, addr, pathJoinV2, big); err != nil || code != 413 {
		t.Fatalf("oversize join: %d %v", code, err)
	}
	if code, err := post(c, addr, pathJoinV2, `{"v":2,"op":"open"}`); err != nil || code != 400 {
		t.Fatalf("bad join: %d %v", code, err)
	}
	if h.bodies.Load() != 1 {
		t.Fatalf("bodies handed over: %d", h.bodies.Load())
	}
}

// The join route limits each source address to 10 requests a minute
// before it reads a body, whatever key each request presents.
//
// Requirements: T3; PROTOCOL 5.3; second review should-fix 7
func TestJoinRouteLimitsEachSourceAddressBeforeReadingTheBody(t *testing.T) {
	id := newID(t)
	h := &joinH{js: node.NewJoinServer(machine(t, id))}
	addr := serve(t, id, h, nil)
	for i := 0; i <= node.OpensPerMinute; i++ {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		c := rawClient(certFor(t, pub, priv, hex.EncodeToString(pub)))
		code, err := post(c, addr, pathJoinV2, `{"v":2,"op":"open"}`)
		want := 400
		if i == node.OpensPerMinute {
			want = 429
		}
		if err != nil || code != want {
			t.Fatalf("request %d: %d %v", i, code, err)
		}
	}
	if n := h.bodies.Load(); n != int64(node.OpensPerMinute) {
		t.Fatalf("bodies handed over: %d", n)
	}
}

// Keys that rotate cannot push live keys out of the per-key limiter: while
// it is full of keys that have not refilled, a new key is refused and a
// live key keeps its bucket.
//
// Requirements: T3 (review should-fix: work before the secret is checked)
func TestLimiterRefusesNewKeysRatherThanEvictingLiveOnes(t *testing.T) {
	l := newLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }
	live := "live"
	for i := 0; i < 5; i++ {
		l.allow(live)
	}
	for i := 0; i < 3*rateMaxKeys; i++ {
		l.allow(string(rune(i + 1000)))
	}
	if len(l.m) > rateMaxKeys {
		t.Fatalf("limiter holds %d keys", len(l.m))
	}
	if b := l.m[live]; b == nil || b.tokens > rateBurst-5+0.01 {
		t.Fatalf("the live key lost its bucket: %+v", b)
	}
	if l.allow("newest") {
		t.Fatal("a new key was let in while the table is full of live keys")
	}
}
