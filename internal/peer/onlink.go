package peer

import (
	"errors"
	"net"
	"net/netip"
)

// attachedNets are the subnets directly attached to this machine's
// interfaces that are up: not loopback, not point-to-point (a tunnel's
// own address and pool say nothing about the local network).
func attachedNets() []netip.Prefix {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []netip.Prefix
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(n.IP)
			if !ok {
				continue
			}
			ones, bits := n.Mask.Size()
			if ip.Is4In6() && bits == 128 {
				ones -= 96
			}
			ip = ip.Unmap()
			if ones < 0 || ones > ip.BitLen() {
				continue
			}
			out = append(out, netip.PrefixFrom(ip, ones).Masked())
		}
	}
	return out
}

// noRouteOnLink reports whether a failed dial is "no route to host" at an
// address inside one of nets, a subnet this machine is attached to: the
// route exists, yet the system refuses it, which a VPN client or a
// firewall on this machine does. The address is the one dialled (the
// error's own, so a host name that was resolved counts), else host.
func noRouteOnLink(err error, host string, nets []netip.Prefix) bool {
	if err == nil || !isNoRoute(err) {
		return false
	}
	var ip netip.Addr
	var oe *net.OpError
	if errors.As(err, &oe) {
		if ta, ok := oe.Addr.(*net.TCPAddr); ok {
			ip, _ = netip.AddrFromSlice(ta.IP)
		}
	}
	if !ip.IsValid() {
		ip, _ = netip.ParseAddr(host)
	}
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap().WithZone("")
	for _, p := range nets {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
