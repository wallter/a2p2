package daemon

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/daemon/portmap"
	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

func TestAdvertisedAddressesAreIPv4AndGlobalIPv6WithoutLinkLocalOrNeedlessTemporaries(t *testing.T) {
	a := func(ip, iface string, temp, unusable bool) ifAddr {
		return ifAddr{IP: netip.MustParseAddr(ip), Iface: iface, Temporary: temp, Unusable: unusable}
	}
	got := chooseAdvertised([]ifAddr{
		a("127.0.0.1", "lo0", false, false),
		a("::1", "lo0", false, false),
		a("192.168.1.5", "en0", false, false),
		a("169.254.3.4", "en0", false, false),
		a("fe80::1", "en0", false, false),
		a("2001:db8:1::5", "en0", false, false),   // stable
		a("2001:db8:1::abcd", "en0", true, false), // temporary beside a stable one: left out
		a("2001:db8:1::dead", "en0", false, true), // deprecated: left out
		a("2001:db8:2::77", "en1", true, false),   // temporary, and en1 has no stable one: kept
		a("fd12:3456:789a::5", "en0", false, false),
	}, 7423)
	want := []string{"192.168.1.5:7423", "[2001:db8:1::5]:7423", "[2001:db8:2::77]:7423", "[fd12:3456:789a::5]:7423"}
	if !slices.Equal(got, want) {
		t.Fatalf("advertised %v\nwant %v", got, want)
	}
}

func TestAUniqueLocalAddressIsDialledOnlyFromTheSameSite(t *testing.T) {
	usable := usableFrom([]ifAddr{{IP: netip.MustParseAddr("fd12:3456:789a:1::5")}})
	for addr, want := range map[string]bool{
		"[fd12:3456:789a:2::9]:7423": true,  // same /48
		"[fd99:1::9]:7423":           false, // another site
		"[2001:db8::9]:7423":         true,
		"192.0.2.4:7423":             true,
		"host.example:7423":          true,
	} {
		if usable(addr) != want {
			t.Errorf("%s: %v", addr, !want)
		}
	}
	if usableFrom(nil)("[fd12::1]:7423") {
		t.Fatal("a machine with no unique-local address dials one")
	}
}

// The peer listener on an unspecified host takes connections over IPv4
// and IPv6 on one socket.
func TestThePeerListenerOnAnUnspecifiedHostTakesIPv4AndIPv6(t *testing.T) {
	ln, err := net.Listen("tcp", "0.0.0.0:0") // as the daemon listens
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	for _, host := range []string{"127.0.0.1", "::1"} {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), time.Second)
		if err != nil {
			if host == "::1" {
				if probe, perr := net.Listen("tcp6", "[::1]:0"); perr != nil {
					t.Skip("no IPv6 loopback here")
				} else {
					probe.Close()
				}
			}
			t.Fatalf("%s: %v", host, err)
		}
		c.Close()
	}
}

// fakeRouter is a portmap.Mapper that keeps the mappings it holds; while
// failUnmap is set it fails every removal.
type fakeRouter struct {
	mu        sync.Mutex
	held      map[string]bool
	unmaps    int
	failUnmap bool
}

func newFakeRouter() *fakeRouter { return &fakeRouter{held: map[string]bool{}} }

func (f *fakeRouter) mapping(internal uint16) portmap.Mapping {
	return portmap.Mapping{Protocol: portmap.PCP, Gateway: netip.MustParseAddr("127.0.0.1"), Internal: internal,
		Nonce: [12]byte{7}, Lifetime: time.Hour, Expires: time.Now().Add(time.Hour),
		External: netip.MustParseAddrPort("198.51.100.7:40001")}
}

func (f *fakeRouter) Map(_ context.Context, internal uint16, _ time.Duration, _ *portmap.Mapping, note portmap.Note) (portmap.Mapping, error) {
	m := f.mapping(internal)
	if err := note(m, true); err != nil {
		return portmap.Mapping{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held[m.Key()] = true
	return m, nil
}

func (f *fakeRouter) Unmap(_ context.Context, m portmap.Mapping) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failUnmap {
		return errors.New("no answer")
	}
	f.unmaps++
	delete(f.held, m.Key())
	return nil
}

func (f *fakeRouter) count() (held, unmaps int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.held), f.unmaps
}

func (f *fakeRouter) setFail(b bool) { f.mu.Lock(); f.failUnmap = b; f.mu.Unlock() }

func (m *machine) useRouter(r portmap.Mapper, local string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.router = r
	if local != "" {
		m.local = netip.MustParseAddr(local)
	}
}

// Port mapping happens only when decided, is shown in status, and is
// removed on net off, on a decision without it, and on a stop.
func TestPortMappingIsShownAndRemovedOnNetOffAnotherDecisionAndStop(t *testing.T) {
	m := newMachineWith(t, "a", nil)
	r := newFakeRouter()
	m.useRouter(r, "")
	listen := m.ln.ln.Addr().String()
	m.setNet(node.NetworkOn)
	if held, _ := r.count(); held != 0 {
		t.Fatal("mapped without --map-port")
	}
	m.decide(localapi.NetReq{State: node.NetworkOn, Listen: listen, MapPort: true})
	eventually(t, "the mapping in status", func() bool {
		st := call[localapi.Status](m, "status", localapi.Empty{})
		return st.Network.MapPort && st.Mapping != nil && st.Mapping.External == "198.51.100.7:40001" &&
			st.Mapping.Protocol == "pcp" && st.Mapping.Expires != "" && !st.Mapping.Reached
	})
	// Not reached by any member: not advertised.
	if st := call[localapi.Status](m, "status", localapi.Empty{}); slices.Contains(st.Listen, "198.51.100.7:40001") {
		t.Fatalf("advertised before a member reached it: %v", st.Listen)
	}
	m.setNet(node.NetworkOff)
	eventually(t, "the mapping removed on net off", func() bool { held, _ := r.count(); return held == 0 })
	m.decide(localapi.NetReq{State: node.NetworkOn, Listen: listen, MapPort: true})
	eventually(t, "mapped again", func() bool { held, _ := r.count(); return held == 1 })
	m.setNet(node.NetworkOn) // the same, without --map-port
	eventually(t, "removed by a decision without it", func() bool { held, _ := r.count(); return held == 0 })
	m.decide(localapi.NetReq{State: node.NetworkOn, Listen: listen, MapPort: true})
	eventually(t, "mapped again", func() bool { held, _ := r.count(); return held == 1 })
	m.stop()
	if held, unmaps := r.count(); held != 0 || unmaps != 3 {
		t.Fatalf("after stop: held %d, removed %d times", held, unmaps)
	}
	if _, err := os.Stat(filepath.Join(m.home, "portmap.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record left after a clean stop: %v", err)
	}
}

// A first invite is for a machine outside the network, and no member can
// reach the mapped address before it is used, so the invite carries it
// while the machine does not advertise it (DESIGN 11).
func TestAFirstInviteCarriesTheRouterMappedAddressThatIsNotYetAdvertised(t *testing.T) {
	m := newMachineWith(t, "a", nil)
	r := newFakeRouter()
	m.useRouter(r, "")
	listen := m.ln.ln.Addr().String()
	m.decide(localapi.NetReq{State: node.NetworkOn, Listen: listen, MapPort: true})
	eventually(t, "the mapping in status", func() bool {
		st := call[localapi.Status](m, "status", localapi.Empty{})
		return st.Mapping != nil && st.Mapping.External == "198.51.100.7:40001"
	})
	call[localapi.Room](m, "room/create", localapi.RoomReq{Room: "team"})
	inv := call[localapi.InviteResp](m, "room/invite", localapi.InviteReq{Room: "team"})
	tok, err := core.DecodeToken(inv.Token)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok.Addrs) == 0 || tok.Addrs[0] != "198.51.100.7:40001" || !slices.Contains(tok.Addrs, listen) {
		t.Fatalf("invite addresses %v, want the mapped address first and %s", tok.Addrs, listen)
	}
	if st := call[localapi.Status](m, "status", localapi.Empty{}); slices.Contains(st.Listen, "198.51.100.7:40001") {
		t.Fatalf("advertised before a member reached it: %v", st.Listen)
	}
}

// Without a router (here: none found), mapping says why and the machine
// listens as decided.
func TestPortMappingWithoutARouterSaysWhyAndStillListens(t *testing.T) {
	m := newMachineWith(t, "a", nil)
	st := m.decide(localapi.NetReq{State: node.NetworkOn, Listen: m.ln.ln.Addr().String(), MapPort: true})
	if st.Network.Applied != "" || st.Mapping == nil || st.Mapping.Error != "no router in tests" {
		t.Fatalf("status %+v %+v", st.Network, st.Mapping)
	}
}

// A listener bound to one address that is not this machine's address on
// the router's network is never mapped: the router would forward to
// whatever else listens there.
func TestNoMappingForAListenerThatDoesNotCoverTheAddressTheRouterForwardsTo(t *testing.T) {
	m := newMachineWith(t, "a", nil)
	r := newFakeRouter()
	m.useRouter(r, "192.0.2.5") // the router's side of this machine; the listener is on 127.0.0.1
	st := m.decide(localapi.NetReq{State: node.NetworkOn, Listen: m.ln.ln.Addr().String(), MapPort: true})
	if st.Mapping == nil || !strings.Contains(st.Mapping.Error, "the router would forward to 192.0.2.5") {
		t.Fatalf("mapping %+v", st.Mapping)
	}
	time.Sleep(50 * time.Millisecond)
	if held, _ := r.count(); held != 0 {
		t.Fatal("mapped for a listener that does not cover the forwarded address")
	}
}

// The mapping ends with the listener it forwards to, however the
// listener ends: here it stops accepting while the daemon runs.
func TestTheMappingIsRemovedWhenTheListenerStops(t *testing.T) {
	m := newMachineWith(t, "a", nil)
	r := newFakeRouter()
	m.useRouter(r, "")
	m.decide(localapi.NetReq{State: node.NetworkOn, Listen: m.ln.ln.Addr().String(), MapPort: true})
	eventually(t, "mapped", func() bool { held, _ := r.count(); return held == 1 })
	m.ln.mu.Lock()
	v := m.ln.cur
	m.ln.mu.Unlock()
	v.Close() // Accept now fails: the listener has stopped
	eventually(t, "removed with the listener", func() bool { held, _ := r.count(); return held == 0 })
	// And no longer given out: not in status, not in a new invite.
	eventually(t, "withdrawn from status", func() bool {
		st := call[localapi.Status](m, "status", localapi.Empty{})
		return st.Mapping == nil || st.Mapping.External == ""
	})
	call[localapi.Room](m, "room/create", localapi.RoomReq{Room: "team"})
	inv := call[localapi.InviteResp](m, "room/invite", localapi.InviteReq{Room: "team"})
	tok, err := core.DecodeToken(inv.Token)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(tok.Addrs, "198.51.100.7:40001") {
		t.Fatalf("invite addresses %v after the listener stopped", tok.Addrs)
	}
}

// A mapping is recorded in the home before the router is asked. A crash
// leaves it on the router and in the record; the next start removes it,
// whatever the decision (here networking off). A removal that fails is
// shown in status and kept for the next attempt.
func TestAMappingLeftByACrashIsRemovedAtTheNextStartAndAFailureIsShown(t *testing.T) {
	m := newMachineWith(t, "a", nil)
	r := newFakeRouter()
	m.useRouter(r, "")
	left := r.mapping(7423)
	r.held[left.Key()] = true // on the router since the crash
	rec := &portmap.File{Path: filepath.Join(m.home, "portmap.json")}
	if err := rec.Save([]portmap.Mapping{left}); err != nil {
		t.Fatal(err)
	}
	r.setFail(true)
	m.stop()
	m.start() // networking off: the removal is still tried, and fails
	eventually(t, "the failed removal in status", func() bool {
		st := call[localapi.Status](m, "status", localapi.Empty{})
		return st.Mapping != nil && len(st.Mapping.NotRemoved) == 1 &&
			strings.Contains(st.Mapping.NotRemoved[0], "could not remove it: no answer")
	})
	if got, _ := rec.Load(); len(got) != 1 {
		t.Fatalf("record %v", got)
	}
	r.setFail(false)
	m.stop() // retried at stop
	if held, _ := r.count(); held != 0 {
		t.Fatal("not removed at stop")
	}
	if got, _ := rec.Load(); len(got) != 0 {
		t.Fatalf("record after removal %v", got)
	}
}

// A record that cannot be used whole (here: one entry for another router,
// then one that does not parse) makes the daemon send nothing to the
// router, keep the file, and say in status what to remove by hand.
func TestARecordThatCannotBeUsedIsKeptAndNothingIsSent(t *testing.T) {
	m := newMachineWith(t, "a", nil)
	r := newFakeRouter()
	m.useRouter(r, "")
	path := filepath.Join(m.home, "portmap.json")
	other := r.mapping(7423)
	other.Gateway = netip.MustParseAddr("192.0.2.1") // another network's router
	if err := (&portmap.File{Path: path}).Save([]portmap.Mapping{r.mapping(7423), other}); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", `[{"protocol":"pcp","gateway":"127.0.0.1","internal_port":70000}]`} {
		if body != "" {
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		before, _ := os.ReadFile(path)
		m.stop()
		m.start()
		st := m.decide(localapi.NetReq{State: node.NetworkOn, Listen: m.ln.ln.Addr().String(), MapPort: true})
		if st.Mapping == nil || !strings.Contains(st.Mapping.Error, "cannot be used") ||
			!strings.Contains(st.Mapping.Error, "nothing was sent to the router") || !strings.Contains(st.Mapping.Error, path) {
			t.Fatalf("%q: mapping %+v", body, st.Mapping)
		}
		time.Sleep(50 * time.Millisecond)
		m.stop()
		if held, unmaps := r.count(); held != 0 || unmaps != 0 {
			t.Fatalf("%q: sent to the router: held %d, removals %d", body, held, unmaps)
		}
		if after, _ := os.ReadFile(path); string(after) != string(before) {
			t.Fatalf("%q: the record changed", body)
		}
		m.start()
	}
}

// An "advertise" entry the listener cannot serve under the recorded
// decision is not advertised, and is returned with why for net check.
func TestAConfiguredIPv6AddressIsNotAdvertisedUnderAnIPv4OnlyDecision(t *testing.T) {
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg := Config{Advertise: []string{"192.0.2.1:7423", "[2001:db8::1]:7423", "host.example:7423"}}
	addrs, dropped := advertised(cfg, ln, true)
	if !slices.Equal(addrs, []string{"192.0.2.1:7423", "host.example:7423"}) || len(dropped) != 1 ||
		!strings.Contains(dropped[0], "[2001:db8::1]:7423 (an IPv6 address; the listener takes IPv4 only)") {
		t.Fatalf("IPv4 only: %v, dropped %v", addrs, dropped)
	}
	if addrs, dropped := advertised(cfg, ln, false); len(addrs) != 3 || len(dropped) != 0 {
		t.Fatalf("both families: %v, dropped %v", addrs, dropped)
	}
}

// The address the router forwards to is the source the system selects
// for the gateway, not the first address in the gateway's subnet; one
// that is not on the gateway's network is refused. The route lookup
// itself is tried only on loopback.
func TestTheMappedAddressIsTheSourceTheSystemSelectsForTheGateway(t *testing.T) {
	gw := netip.MustParseAddr("192.168.1.1")
	nets := []ifNet{{iface: "en0", prefix: netip.MustParsePrefix("192.168.1.5/24")},
		{iface: "en0", prefix: netip.MustParsePrefix("192.168.1.9/24")}, {iface: "en1", prefix: netip.MustParsePrefix("10.0.0.2/8")}}
	if got, err := pickLocal(gw, "en0", netip.MustParseAddr("192.168.1.9"), nets); err != nil || got.String() != "192.168.1.9" {
		t.Fatalf("picked %s %v", got, err)
	}
	if _, err := pickLocal(gw, "en0", netip.MustParseAddr("10.0.0.2"), nets); err == nil {
		t.Fatal("took a source off the gateway's network")
	}
	if src, err := sourceFor(netip.MustParseAddr("127.0.0.1")); err != nil || src.String() != "127.0.0.1" {
		t.Fatalf("loopback route lookup: %s %v", src, err)
	}
}

func TestAnIPv4OnlyDecisionAdvertisesNoIPv6(t *testing.T) {
	addrs := []ifAddr{{IP: netip.MustParseAddr("192.168.1.5"), Iface: "en0"}, {IP: netip.MustParseAddr("2001:db8:1::5"), Iface: "en0"}}
	if got := advertisable(addrs, 7423, true); !slices.Equal(got, []string{"192.168.1.5:7423"}) {
		t.Fatalf("IPv4 only: %v", got)
	}
	if got := advertisable(addrs, 7423, false); len(got) != 2 {
		t.Fatalf("both: %v", got)
	}
	if got := FamiliesText("192.168.1.5:7423"); !strings.Contains(got, "IPv4 only") {
		t.Fatalf("%q", got)
	}
	if got := FamiliesText("0.0.0.0:7423"); !strings.Contains(got, "IPv4 and IPv6") || !strings.Contains(got, "For IPv4 only") {
		t.Fatalf("%q", got)
	}
}
