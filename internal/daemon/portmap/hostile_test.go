package portmap

// A host on the local network that answers as the router (audit F61 to
// F66). Every router and spoofer here is a fake on loopback.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newRawUDP is a responder on loopback that may send several datagrams
// for one request, each from the gateway's address and port.
func newRawUDP(t *testing.T, answer func(q []byte, send func([]byte))) *fakeUDP {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeUDP{conn: conn}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			q := append([]byte(nil), buf[:n]...)
			f.mu.Lock()
			f.got = append(f.got, q)
			f.mu.Unlock()
			go answer(q, func(b []byte) { _, _ = conn.WriteToUDP(b, from) })
		}
	}()
	return f
}

// v0 is the constant datagram a NAT-PMP server sends to a request of
// another version. It holds nothing of the request.
var v0 = []byte{0, 0x80 | pcpOpMap, 0, 1, 0, 0, 0, 1}

// A spoofer sends the constant NAT-PMP version answer at once; the real
// PCP router answers a little later in the same wait. The PCP answer,
// which carries the request's nonce, wins.
func TestAConstantNATPMPVersionAnswerDoesNotDowngradeAPCPRouter(t *testing.T) {
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, maps: map[string]uint16{}}
	gw := newRawUDP(t, func(q []byte, send func([]byte)) {
		if len(q) < 60 || q[0] != pcpVersion {
			return // the spoofer does not answer NAT-PMP here
		}
		send(v0)
		time.Sleep(20 * time.Millisecond)
		send(r.answer(q))
	})
	m, err := client(gw).Map(context.Background(), 7423, time.Hour, nil, nil)
	if err != nil || m.Protocol != PCP || m.External.String() != "203.0.113.7:40001" {
		t.Fatalf("mapping %+v, %v", m, err)
	}
}

// A version answer that may be forged makes the PCP request's handle stay
// recorded, so the mapping the real router may have made is deleted later
// by its nonce. Here the "router" then refuses NAT-PMP.
func TestADowngradeKeepsThePCPHandleForDeletion(t *testing.T) {
	gw := newRawUDP(t, func(q []byte, send func([]byte)) {
		switch {
		case len(q) >= 60 && q[0] == pcpVersion:
			send(v0)
		case len(q) == 12 && q[1] == pmpOpTCP:
			b := make([]byte, 16)
			b[1] = 128 + pmpOpTCP
			binary.BigEndian.PutUint16(b[2:], 2) // not authorized
			copy(b[8:10], q[4:6])
			send(b)
		}
	})
	rec := record(t)
	if _, err := client(gw).Map(context.Background(), 7423, time.Hour, nil, noteTo(rec)); err == nil {
		t.Fatal("mapped")
	}
	if held := get(t, rec); len(held) != 1 || held[0].Protocol != PCP {
		t.Fatalf("record %+v, want the PCP handle kept", held)
	}
}

// The honest case of the two above: a router that speaks only NAT-PMP is
// mapped in NAT-PMP by a keeper, the PCP handle kept for the version
// answer is let go (a NAT-PMP router holds no PCP mapping), and the stop
// removes the mapping and leaves no record.
func TestARouterThatSpeaksOnlyNATPMPIsMappedAndTheRecordEndsEmpty(t *testing.T) {
	var maps atomic.Int32
	gw := newFakeUDP(t, natpmpRouter([4]byte{198, 51, 100, 9}, 50123, &maps))
	rec := record(t)
	k := NewKeeper(client(gw), 7423, rec, nil)
	stop := runKeeper(t, k)
	waitFor(t, "the mapping", func() bool { return k.State().Mapping != nil })
	waitFor(t, "only the NAT-PMP mapping recorded", func() bool {
		held, _ := rec.Load()
		return len(held) == 1 && held[0].Protocol == NATPMP
	})
	if st := k.State(); st.Mapping.Protocol != NATPMP || len(st.Left) != 0 {
		t.Fatalf("state %+v", st)
	}
	stop()
	waitFor(t, "the deletion", func() bool { return maps.Load() == 0 })
	if held := get(t, rec); len(held) != 0 {
		t.Fatalf("record %+v", held)
	}
}

// lossyPCP is a real PCP router whose first request of each kind (map,
// delete) is lost on the way, behind a spoofer that sends the version
// answer to every PCP request at once.
func lossyPCP(t *testing.T, r *pcpRouter) *fakeUDP {
	var maps, dels atomic.Int32
	return newRawUDP(t, func(q []byte, send func([]byte)) {
		if len(q) < 60 || q[0] != pcpVersion {
			return
		}
		send(v0)
		n := &maps
		if binary.BigEndian.Uint32(q[4:]) == 0 {
			n = &dels
		}
		if n.Add(1) == 1 {
			return // lost
		}
		send(r.answer(q))
	})
}

// A version answer in the first wait does not end the exchange: the
// request is sent again, and the PCP answer to a later try is taken. So
// a lost request does not let a spoofer downgrade the mapping, nor make
// a deletion count as done while the router still holds the mapping.
func TestAVersionAnswerCountsOnlyAfterEveryTryOfAMappingAndADeletion(t *testing.T) {
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, maps: map[string]uint16{}}
	gw := lossyPCP(t, r)
	c := client(gw)
	rec := record(t)
	m, err := c.Map(context.Background(), 7423, time.Hour, nil, noteTo(rec))
	if err != nil || m.Protocol != PCP || r.held() != 1 {
		t.Fatalf("mapping %+v, %v, router holds %d", m, err, r.held())
	}
	if held := get(t, rec); len(held) != 1 || held[0].Protocol != PCP {
		t.Fatalf("record %+v", held)
	}
	if err := c.Unmap(context.Background(), m); err != nil || r.held() != 0 {
		t.Fatalf("unmap %v, router holds %d", err, r.held())
	}
}

// The router makes the mapping and every reply is lost: the request's
// handle is let go, but only after a deletion was sent for it.
func TestAPCPRequestWithNoAnswerIsDeletedBeforeItIsLetGo(t *testing.T) {
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, maps: map[string]uint16{}}
	gw := newRawUDP(t, func(q []byte, send func([]byte)) {
		r.answer(q) // made, or deleted; the reply is lost
	})
	rec := record(t)
	c := client(gw)
	c.Tries = 1
	if _, err := c.mapPCP(context.Background(), 7423, time.Hour, nil, noteTo(rec)); !errors.Is(err, errNoAnswer) {
		t.Fatalf("mapped: %v", err)
	}
	waitFor(t, "the deletion", func() bool { return len(gw.requests()) == 2 && r.held() == 0 })
	if held := get(t, rec); len(held) != 0 {
		t.Fatalf("record %+v", held)
	}
}

// A grant of the longest lifetime the protocols carry (136 years) is
// taken as what was asked: it expires and is renewed on a2p2's terms.
func TestAGrantLongerThanAskedIsClampedAndRenewedAtHalfOfWhatWasAsked(t *testing.T) {
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, grant: 0xFFFFFFFF, maps: map[string]uint16{}}
	gw := newFakeUDP(t, r.answer)
	m, err := client(gw).Map(context.Background(), 7423, time.Hour, nil, nil)
	if err != nil || m.Lifetime != time.Hour || m.Expires.After(time.Now().Add(time.Hour)) {
		t.Fatalf("mapping %+v, %v", m, err)
	}
	var maps atomic.Int32
	pmp := newFakeUDP(t, func(q []byte) []byte {
		b := natpmpRouter([4]byte{198, 51, 100, 9}, 50123, &maps)(q)
		if len(q) == 12 && len(b) == 16 && binary.BigEndian.Uint32(q[8:]) != 0 {
			binary.BigEndian.PutUint32(b[12:], 0xFFFFFFFF)
		}
		return b
	})
	if m, err := client(pmp).Map(context.Background(), 7423, time.Hour, nil, nil); err != nil || m.Lifetime != time.Hour {
		t.Fatalf("NAT-PMP mapping %+v, %v", m, err)
	}

	defer func(l, w time.Duration) { Lifetime, minWait = l, w }(Lifetime, minWait)
	Lifetime, minWait = 2*time.Second, time.Millisecond
	// A keeper whose Mapper does not clamp still renews at half of what it
	// asked (1 s here), not at half of what it was told, nor at once.
	k := NewKeeper(longGrant{client(gw)}, 7423, record(t), nil)
	before := len(gw.requests())
	stop := runKeeper(t, k)
	defer stop()
	waitFor(t, "the mapping", func() bool { return k.State().Mapping != nil })
	granted := time.Now()
	if m := k.State().Mapping; m.Lifetime != Lifetime || m.Expires.After(granted.Add(Lifetime)) {
		t.Fatalf("held %+v, want the lifetime asked", m)
	}
	waitFor(t, "a renewal", func() bool { return len(gw.requests()) >= before+2 })
	if d := time.Since(granted); d < Lifetime/2-300*time.Millisecond || d >= Lifetime {
		t.Fatalf("renewed %v after the grant, want about %v", d, Lifetime/2)
	}
}

// longGrant reports every grant as the longest lifetime there is.
type longGrant struct{ *Client }

func (l longGrant) Map(ctx context.Context, internal uint16, lifetime time.Duration, prev *Mapping, note Note) (Mapping, error) {
	m, err := l.Client.Map(ctx, internal, lifetime, prev, note)
	m.Lifetime, m.Expires = maxLifetime, time.Now().Add(maxLifetime)
	return m, err
}

// A router (or a host answering as it) that grants an external address
// that is not public: multicast, loopback, private, shared, link-local
// or reserved. The keeper takes no mapping, shows why, keeps a record
// that the next start can read, and removes what the router made.
func TestAMappedAddressThatIsNotPublicIsNeverTakenOrLeftUnusable(t *testing.T) {
	for _, ext := range []string{"224.0.0.1", "127.0.0.1", "192.168.1.1", "10.1.2.3", "100.64.0.1", "169.254.1.1", "240.0.0.1", "0.1.2.3"} {
		r := &pcpRouter{extIP: netip.MustParseAddr(ext), extPort: 40001, maps: map[string]uint16{}}
		gw := newFakeUDP(t, r.answer)
		rec := record(t)
		k := NewKeeper(patient(client(gw)), 7423, rec, nil)
		stop := runKeeper(t, k)
		waitFor(t, ext, func() bool { return strings.Contains(k.State().Err, "not a public address") })
		if st := k.State(); st.Mapping != nil || !strings.Contains(st.Err, "not a public address") {
			t.Fatalf("%s: state %+v", ext, st)
		}
		if _, err := rec.Load(); err != nil {
			t.Fatalf("%s: the record cannot be used: %v", ext, err)
		}
		waitFor(t, ext+" removed", func() bool { return r.held() == 0 })
		stop()
		if held := get(t, rec); len(held) != 0 {
			t.Fatalf("%s: record %+v", ext, held)
		}
	}
}

func TestOnlyPublicAddressesCountAsARoutersExternalAddress(t *testing.T) {
	for a, want := range map[string]bool{
		"8.8.8.8": true, "203.0.113.7": true, "::ffff:8.8.8.8": true,
		// IPv6, which no request of a2p2 asks for, and the IPv6 forms of
		// a private IPv4 address: IPv4-compatible, NAT64, 6to4, SIIT,
		// site-local.
		"2001:4860::1": false, "::a00:1": false, "::7f00:1": false, "64:ff9b::a00:1": false,
		"2002:a00:1::1": false, "::ffff:0:a00:1": false, "fec0::1": false,
		"224.0.0.1": false, "127.0.0.1": false, "192.168.1.1": false, "172.16.0.1": false, "10.0.0.1": false,
		"100.64.0.1": false, "100.127.255.254": false, "169.254.1.1": false, "0.0.0.0": false, "0.1.2.3": false,
		"240.0.0.1": false, "255.255.255.255": false, "::1": false, "fe80::1": false, "fd00::1": false, "ff02::1": false, "::": false,
	} {
		if got := Public(netip.MustParseAddr(a)); got != want {
			t.Errorf("Public(%s) = %v", a, got)
		}
	}
	if Public(netip.Addr{}) {
		t.Error("the zero address is public")
	}
}

// A listener on one of the last ports, both taken on the router: the
// next port is not 0 or 1 (a wrap), and the record stays usable.
func TestAUPnPNextPortDoesNotWrapToZero(t *testing.T) {
	g := newFakeIGD(t)
	g.mu.Lock()
	g.taken["65534"], g.taken["65535"] = true, true
	g.mu.Unlock()
	c := client(noPCP(t))
	c.SSDP = fmt.Sprintf("127.0.0.1:%d", g.ssdp.port())
	rec := record(t)
	_, err := c.Map(context.Background(), 65534, time.Hour, nil, noteTo(rec))
	if err == nil {
		t.Fatal("mapped")
	}
	if g.has("0") || g.has("1") {
		t.Fatal("asked for a port past 65535")
	}
	if _, err := rec.Load(); err != nil {
		t.Fatalf("the record cannot be used: %v", err)
	}
}

// A keeper holds at most a few mappings it may have to remove, so the
// record always loads: past that it asks the router for nothing more,
// and a handle the record could not load is never written.
func TestAKeeperNeverRecordsMoreThanTheRecordLoads(t *testing.T) {
	rec := record(t)
	k := NewKeeper(nil, 7423, rec, nil)
	// On an empty keeper, so only Valid can refuse it: a UPnP handle with
	// no control URL.
	if err := k.note(Mapping{Protocol: UPnP, Gateway: loop, Internal: 7423}, true); err == nil ||
		!strings.Contains(err.Error(), "could not hold") {
		t.Fatalf("a handle the record cannot load: %v", err)
	}
	if held := get(t, rec); len(held) != 0 || len(k.held) != 0 {
		t.Fatalf("recorded %+v", held)
	}
	h := Mapping{Protocol: PCP, Gateway: loop, Internal: 7423}
	for i := range maxHeld {
		h.Nonce = [12]byte{byte(i + 1)}
		if err := k.note(h, true); err != nil {
			t.Fatalf("handle %d: %v", i, err)
		}
	}
	h.Nonce = [12]byte{0xff}
	if err := k.note(h, true); err == nil {
		t.Fatal("recorded one more than the keeper holds")
	}
	if held := get(t, rec); len(held) != maxHeld {
		t.Fatalf("record holds %d", len(held))
	}
}

// When a keeper's run ends (the listener stopped, net off, a stop), the
// mapping is no longer given out, even when its removal failed: it
// forwards to a listener that is gone. A permanent UPnP mapping has no
// expiry that would end it.
func TestAKeeperThatEndsGivesOutNoMappingEvenIfItsRemovalFailed(t *testing.T) {
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, maps: map[string]uint16{}}
	gw := newFakeUDP(t, r.answer)
	k := NewKeeper(client(gw), 7423, record(t), nil)
	stop := runKeeper(t, k)
	waitFor(t, "the mapping", func() bool { return k.State().Mapping != nil })
	r.set(func(r *pcpRouter) { r.silentDel = true })
	stop()
	if st := k.State(); st.Mapping != nil || len(st.Left) != 1 {
		t.Fatalf("state after the end %+v", st)
	}
}

// A renewal that the router answers with an address that is not public
// withdraws the address given out before, while the mapping stays held
// to be renewed and removed.
func TestARenewalAnsweredWithAnUnusableAddressWithdrawsTheOneGivenOut(t *testing.T) {
	defer func(l, w, ra time.Duration) { Lifetime, minWait, retryAfter = l, w, ra }(Lifetime, minWait, retryAfter)
	// retryAfter is short so that a first attempt lost under load (two
	// tries of 250 ms) is tried again within waitFor's deadline, not in 5
	// minutes; renewals answered with the unusable address then repeat,
	// which changes nothing the test checks.
	Lifetime, minWait, retryAfter = 2*time.Second, time.Millisecond, 50*time.Millisecond
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, maps: map[string]uint16{}}
	gw := newFakeUDP(t, r.answer)
	k := NewKeeper(client(gw), 7423, record(t), nil)
	stop := runKeeper(t, k)
	defer stop()
	waitFor(t, "the mapping", func() bool { return k.State().Mapping != nil })
	r.set(func(r *pcpRouter) { r.extIP = netip.MustParseAddr("10.0.0.7") })
	waitFor(t, "the withdrawal", func() bool {
		st := k.State()
		return st.Mapping == nil && strings.Contains(st.Err, "not a public address")
	})
	if r.held() != 1 {
		t.Fatalf("router holds %d, want the mapping kept to be renewed", r.held())
	}
}

// A renewal whose every reply is lost (a short drop of the network) sends
// no deletion: the router keeps the mapping held and given out.
func TestARenewalWithNoAnswerLeavesTheRoutersMappingInPlace(t *testing.T) {
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, maps: map[string]uint16{}}
	var n atomic.Int32
	gw := newRawUDP(t, func(q []byte, send func([]byte)) {
		b := r.answer(q)
		if n.Add(1) == 1 {
			send(b) // only the first mapping is answered
		}
	})
	c := client(gw)
	m, err := c.Map(context.Background(), 7423, time.Hour, nil, nil)
	if err != nil || r.held() != 1 {
		t.Fatalf("mapping %+v, %v", m, err)
	}
	c.Tries = 1
	noop := func(Mapping, bool) error { return nil }
	if _, err := c.mapPCP(context.Background(), 7423, time.Hour, &m, noop); !errors.Is(err, errNoAnswer) {
		t.Fatalf("renewal: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // a deletion, if sent, reaches the router
	if got := len(gw.requests()); got != 2 || r.held() != 1 {
		t.Fatalf("%d requests, router holds %d; want 2 and the mapping kept", got, r.held())
	}
}

// requestsAfter counts the requests gw receives during d.
func requestsAfter(gw *fakeUDP, d time.Duration) int {
	before := len(gw.requests())
	time.Sleep(d)
	return len(gw.requests()) - before
}

// A router that answers renewals at once with an address that is not
// public is asked again only retryAfter later, not every minWait once the
// mapping held has expired.
func TestARenewalAnsweredWithAnUnusableAddressIsNotRetriedEveryMinWait(t *testing.T) {
	defer func(l, w, ra time.Duration) { Lifetime, minWait, retryAfter = l, w, ra }(Lifetime, minWait, retryAfter)
	Lifetime, minWait, retryAfter = time.Second, time.Millisecond, time.Hour
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, maps: map[string]uint16{}}
	gw := newFakeUDP(t, r.answer)
	// retryAfter is an hour here, so a first mapping lost under load would
	// never be tried again: the client waits long for each answer instead.
	k := NewKeeper(patient(client(gw)), 7423, record(t), nil)
	stop := runKeeper(t, k)
	defer stop()
	waitFor(t, "the mapping", func() bool { return k.State().Mapping != nil })
	r.set(func(r *pcpRouter) { r.extIP = netip.MustParseAddr("10.0.0.7") })
	waitFor(t, "the unusable renewal", func() bool { return strings.Contains(k.State().Err, "not a public address") })
	// The mapping held expires within this wait.
	if n := requestsAfter(gw, 1500*time.Millisecond); n > 1 {
		t.Fatalf("%d requests in 1.5 s, want at most 1", n)
	}
	if r.held() != 1 {
		t.Fatalf("router holds %d, want the mapping kept to be removed", r.held())
	}
}
