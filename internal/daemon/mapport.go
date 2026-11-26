package daemon

// Port mapping (a2p2 net on --map-port; DESIGN 11), as the daemon uses
// it: which router to ask, the keeper of a run, and what status, net
// check, invites and the reacher see of it. Everything of port mapping is
// here and in package portmap, but for defaultGateway (netsys_*.go), the
// run's few calls of this file (daemon.go) and RunOpts.Gateway and
// RunOpts.Mapper. Removing the feature is deleting this file and that
// package and fixing those calls.

import (
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"time"

	"github.com/wallter/a2p2/internal/daemon/portmap"
	"github.com/wallter/a2p2/internal/node"
)

// keeper prepares this run's port mapping, or only the removal of what
// an earlier run left (internal port 0), or says why there is neither.
// A mapping is made only under a decision with --map-port, for a listener
// bound to the wildcard address or to this machine's address on the
// gateway's network: never for one that would forward to another service.
// changed is called when the mapped address changes.
func (r *run) keeper(netw node.Network, ln net.Listener, changed func()) (*portmap.Keeper, string) {
	// A record that cannot be used whole stops everything: nothing is sent
	// to the router, the file stays, and status says what to remove by hand.
	path := filepath.Join(r.home, "portmap.json")
	left, err := (&portmap.File{Path: path}).Load()
	if err != nil {
		return nil, portmap.UnusableText(err)
	}
	want := netw.MapPort && ln != nil
	if !want && len(left) == 0 {
		return nil, ""
	}
	gwf := gatewayLocal
	if r.o.Gateway != nil {
		gwf = r.o.Gateway
	}
	gw, local, err := gwf()
	if err != nil {
		return nil, err.Error()
	}
	rec := &portmap.File{Path: path, Gateway: gw}
	if _, err := rec.Load(); err != nil {
		return nil, portmap.UnusableText(err)
	}
	var m portmap.Mapper = &portmap.Client{Gateway: gw, Local: local}
	if r.o.Mapper != nil {
		m = r.o.Mapper(gw, local)
	}
	port := uint16(0)
	why := ""
	if want && askable(gw) != nil {
		// Not asked for a mapping, but what an earlier run recorded on
		// this same gateway is still removed.
		why, want = askable(gw).Error(), false
	}
	if want {
		tcp := ln.Addr().(*net.TCPAddr)
		ip, _ := netip.AddrFromSlice(tcp.IP)
		if ip = ip.Unmap(); ip.IsUnspecified() || ip == local {
			port = uint16(tcp.Port)
		} else {
			why = "not mapped: the listener is on " + ip.String() + ", but the router would forward to " +
				local.String() + "; listen on 0.0.0.0 or on " + local.String()
		}
	}
	if port == 0 && len(left) == 0 {
		return nil, why
	}
	last := ""
	k := portmap.NewKeeper(m, port, rec, func(st portmap.State) {
		// What the router answered (UPnP error text) is escaped as it is
		// logged; status gives it raw and the CLI escapes it there.
		if st.Err != "" {
			slog.Warn("port mapping", "err", node.EscapeDisplay(st.Err))
		}
		if len(st.Left) > 0 {
			slog.Warn("port mapping not removed", "left", node.EscapeDisplay(portmap.LeftText(st.Left)))
		}
		ext := ""
		if st.Mapping != nil {
			ext = st.Mapping.External.String()
			if ext != last {
				slog.Info("port mapping", "protocol", st.Mapping.Protocol, "external", ext,
					"lifetime", st.Mapping.Lifetime.String())
			}
		}
		if ext != last {
			last = ext
			changed()
		}
	})
	return k, why
}

// mappedAddr is the mapped external address, to verify by a dial-back
// before it is advertised: none once the lease has run out, whether or
// not a renewal is still being tried, and none that is not public (the
// keeper takes no such mapping; this checks again where it is given out).
func mappedAddr(k *portmap.Keeper) []string {
	return mappedAt(k, time.Now())
}

func mappedAt(k *portmap.Keeper, now time.Time) []string {
	if k == nil {
		return nil
	}
	if m := k.State().Mapping; m != nil && portmap.Public(m.External.Addr()) && m.External.Port() != 0 && m.Live(now) {
		return []string{m.External.String()}
	}
	return nil
}

// mappingView is the mapping as status and net check show it: nil when
// there is nothing to say.
func mappingView(k *portmap.Keeper, why string) *node.PortMapping {
	if k == nil {
		if why == "" {
			return nil
		}
		return &node.PortMapping{Error: why}
	}
	st := k.State()
	v := &node.PortMapping{NotRemoved: st.Left}
	switch {
	case st.Mapping != nil:
		v.Protocol, v.External, v.Error = st.Mapping.Protocol, st.Mapping.External.String(), st.Err
		if !st.Mapping.Expires.IsZero() && st.Mapping.Lifetime > 0 {
			v.Expires = st.Mapping.Expires.UTC().Format("2006-01-02T15:04:05.000Z")
		}
	case st.Err != "":
		v.Error = st.Err
	case why != "":
		v.Error = why
	case len(st.Left) == 0:
		return nil // not tried yet, or nothing left
	}
	return v
}

// gatewayLocal is the default route's gateway and this machine's IPv4
// address on that route's interface (the source address its packets to
// the gateway carry): the one in the gateway's subnet. With several
// uplinks only the default route is used: the one with the lowest metric
// on Linux, the unscoped one on macOS.
func gatewayLocal() (gw, local netip.Addr, err error) {
	gw, iface, err := defaultGateway()
	if err != nil {
		return netip.Addr{}, netip.Addr{}, err
	}
	src, err := sourceFor(gw)
	if err != nil {
		return netip.Addr{}, netip.Addr{}, err
	}
	var nets []ifNet
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		as, _ := ifc.Addrs()
		for _, a := range as {
			if ipn, ok := a.(*net.IPNet); ok {
				if p, ok := netipPrefix(ipn); ok {
					nets = append(nets, ifNet{iface: ifc.Name, prefix: p})
				}
			}
		}
	}
	local, err = pickLocal(gw, iface, src, nets)
	return gw, local, err
}

// sourceFor is the source address the system selects to reach gw: a
// route lookup by connecting a UDP socket, which sends nothing.
func sourceFor(gw netip.Addr) (netip.Addr, error) {
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(gw, 5351)))
	if err != nil {
		return netip.Addr{}, err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap(), nil
}

// ifNet is one address of an interface, with its prefix.
type ifNet struct {
	iface  string
	prefix netip.Prefix
}

func netipPrefix(ipn *net.IPNet) (netip.Prefix, bool) {
	ip, ok := netip.AddrFromSlice(ipn.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ones, _ := ipn.Mask.Size()
	return netip.PrefixFrom(ip.Unmap(), ones), true
}

// askable says why gw is not asked for a new mapping, or nil. Only a
// gateway with a private IPv4 address is asked: a home or office router
// on the local network, never one on the internet side or in shared
// (carrier) address space. A loopback gateway is this machine itself
// (the fake routers of tests). A gateway that is not asked still gets the
// deletions of what an earlier run recorded on it, since a record names
// its gateway and is used only on that same one.
func askable(gw netip.Addr) error {
	if gw.Is4() && (gw.IsPrivate() || gw.IsLoopback()) {
		return nil
	}
	return fmt.Errorf("not mapped: the default gateway %s does not have a private address, so it is not asked to map a port", gw)
}

// pickLocal is the address the router forwards to: the source the system
// selected for gw (src), if it is an address of this machine on the
// gateway's network (and on iface, when the route names one).
func pickLocal(gw netip.Addr, iface string, src netip.Addr, nets []ifNet) (netip.Addr, error) {
	for _, n := range nets {
		if n.prefix.Addr() == src && n.prefix.Contains(gw) && (iface == "" || n.iface == iface) {
			return src, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("the address this machine uses to reach the default gateway %s (%s) is not on the gateway's network", gw, src)
}
