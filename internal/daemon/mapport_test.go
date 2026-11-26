package daemon

import (
	"context"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/daemon/portmap"
	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// Only a gateway with a private address on this machine's network is
// asked to map a port: not one on the internet side, not one in shared
// (carrier) address space. The address the router forwards to is still
// found for every gateway on the local network, so that what an earlier
// run recorded there can be removed.
func TestPortMappingAsksOnlyAPrivateGatewayOnTheLocalNetwork(t *testing.T) {
	for gw, ok := range map[string]bool{"192.168.1.1": true, "10.0.0.1": true, "172.16.0.1": true, "127.0.0.1": true,
		"203.0.113.1": false, "100.64.0.1": false, "8.8.8.1": false, "2001:db8::1": false, "fe80::1": false} {
		g := netip.MustParseAddr(gw)
		if err := askable(g); ok != (err == nil) {
			t.Fatalf("%s: %v", gw, err)
		}
		if !g.Is4() {
			continue
		}
		p, _ := g.Prefix(24)
		src := p.Addr().Next().Next()
		nets := []ifNet{{iface: "en0", prefix: netip.PrefixFrom(src, 24)}}
		if got, err := pickLocal(g, "en0", src, nets); err != nil || got != src {
			t.Fatalf("%s: %s %v", gw, got, err)
		}
	}
}

// A record left on a gateway that is no longer asked (here: a public
// one, as an earlier version asked) is still removed at the next start,
// with networking off; --map-port then maps nothing there and says why.
func TestAMappingLeftOnAGatewayThatIsNotAskedIsStillRemoved(t *testing.T) {
	m := newMachineWith(t, "a", nil)
	r := newFakeRouter()
	m.useRouter(r, "")
	gw := netip.MustParseAddr("203.0.113.1")
	m.mu.Lock()
	m.gw = gw
	m.mu.Unlock()
	left := r.mapping(7423)
	left.Gateway = gw
	r.mu.Lock()
	r.held[left.Key()] = true
	r.mu.Unlock()
	rec := &portmap.File{Path: filepath.Join(m.home, "portmap.json")}
	if err := rec.Save([]portmap.Mapping{left}); err != nil {
		t.Fatal(err)
	}
	m.stop()
	m.start() // networking off
	eventually(t, "the left mapping removed", func() bool { held, _ := r.count(); return held == 0 })
	eventually(t, "the record emptied", func() bool { got, err := rec.Load(); return err == nil && len(got) == 0 })
	st := m.decide(localapi.NetReq{State: node.NetworkOn, Listen: m.ln.ln.Addr().String(), MapPort: true})
	if st.Mapping == nil || !strings.Contains(st.Mapping.Error, "does not have a private address") {
		t.Fatalf("mapping %+v", st.Mapping)
	}
	time.Sleep(50 * time.Millisecond)
	if held, _ := r.count(); held != 0 {
		t.Fatal("mapped on a gateway that is not asked")
	}
}

// privateGrant is a router (or a host answering as it) that grants an
// external address in a private range.
type privateGrant struct{ *fakeRouter }

func (p privateGrant) Map(ctx context.Context, internal uint16, l time.Duration, prev *portmap.Mapping, note portmap.Note) (portmap.Mapping, error) {
	m, err := p.fakeRouter.Map(ctx, internal, l, prev, note)
	m.External = netip.MustParseAddrPort("10.0.0.7:40001")
	return m, err
}

// A mapped address that is not public is never put in an invite, status
// says why, and what the router made is removed. The invite check is the
// one that measures the rule: invites and the reacher's candidates, the
// only way a mapped address is advertised, are both mappedAddr. The last
// check alone would pass anyway, since no member confirms anything here.
func TestARouterMappedAddressThatIsNotPublicIsNeitherInvitedNorAdvertised(t *testing.T) {
	m := newMachineWith(t, "a", nil)
	r := newFakeRouter()
	m.useRouter(privateGrant{r}, "")
	listen := m.ln.ln.Addr().String()
	m.decide(localapi.NetReq{State: node.NetworkOn, Listen: listen, MapPort: true})
	eventually(t, "the refusal in status", func() bool {
		st := call[localapi.Status](m, "status", localapi.Empty{})
		return st.Mapping != nil && strings.Contains(st.Mapping.Error, "not a public address") && st.Mapping.External == ""
	})
	eventually(t, "the mapping removed", func() bool { held, _ := r.count(); return held == 0 })
	call[localapi.Room](m, "room/create", localapi.RoomReq{Room: "team"})
	inv := call[localapi.InviteResp](m, "room/invite", localapi.InviteReq{Room: "team"})
	tok, err := core.DecodeToken(inv.Token)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(tok.Addrs, "10.0.0.7:40001") || !slices.Contains(tok.Addrs, listen) {
		t.Fatalf("invite addresses %v", tok.Addrs)
	}
	if st := call[localapi.Status](m, "status", localapi.Empty{}); slices.Contains(st.Listen, "10.0.0.7:40001") {
		t.Fatalf("advertised %v", st.Listen)
	}
}
