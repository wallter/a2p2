package daemon

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

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

// fakeRouter is a portmap.Mapper that counts mappings held.
type fakeRouter struct {
	mu     sync.Mutex
	held   int
	maps   int
	unmaps int
}

func (f *fakeRouter) Map(_ context.Context, internal uint16, _ time.Duration, prev *portmap.Mapping) (portmap.Mapping, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.maps++
	if prev == nil {
		f.held++
	}
	return portmap.Mapping{Protocol: portmap.PCP, Internal: internal, Lifetime: time.Hour, Expires: time.Now().Add(time.Hour),
		External: netip.MustParseAddrPort("198.51.100.7:40001")}, nil
}

func (f *fakeRouter) Unmap(context.Context, portmap.Mapping) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unmaps++
	f.held--
	return nil
}

func (f *fakeRouter) count() (held, unmaps int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held, f.unmaps
}

// Port mapping happens only when decided, is shown in status, and is
// removed on net off, on a decision without it, and on a stop.
func TestPortMappingIsShownAndRemovedOnNetOffAnotherDecisionAndStop(t *testing.T) {
	m := newMachineWith(t, "a", nil)
	r := &fakeRouter{}
	m.mu.Lock()
	m.router = r
	m.mu.Unlock()
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
