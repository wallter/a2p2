package peer

import (
	"errors"
	"net"
	"net/netip"
	"time"
)

// onLinkFast is how quickly a local block answers: a VPN client or a
// firewall on this machine refuses at once, while a neighbour that does
// not answer takes seconds to fail.
const onLinkFast = 250 * time.Millisecond

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
			if p, ok := attachedPrefix(a); ok {
				out = append(out, p)
			}
		}
	}
	return out
}

// attachedPrefix is the subnet of one interface address. An address with
// no mask or a mask that is not a plain prefix (Size says 0, 0), a prefix
// length of 0 and a link-local subnet (whose zone is unknown) give none.
func attachedPrefix(a net.Addr) (netip.Prefix, bool) {
	n, ok := a.(*net.IPNet)
	if !ok {
		return netip.Prefix{}, false
	}
	ip, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ones, bits := n.Mask.Size()
	if bits == 0 || ones == 0 {
		return netip.Prefix{}, false
	}
	if ip.Is4In6() && bits == 128 {
		ones -= 96
	}
	ip = ip.Unmap()
	if ones <= 0 || ones > ip.BitLen() || ip.IsLinkLocalUnicast() {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(ip, ones).Masked(), true
}

// noRouteOnLink reports whether a failed dial is "no route to host" at an
// address inside one of nets, a subnet this machine is attached to, that
// failed within onLinkFast: the route exists, yet the system refuses it at
// once, which a VPN client or a firewall on this machine does. The address
// is the one dialled (the error's own, so a host name that was resolved
// counts), else host. A link-local address never counts.
func noRouteOnLink(err error, host string, nets []netip.Prefix, took time.Duration) bool {
	if err == nil || !isNoRoute(err) || took >= onLinkFast {
		return false
	}
	var ip netip.Addr
	var oe *net.OpError
	if errors.As(err, &oe) {
		if ta, ok := oe.Addr.(*net.TCPAddr); ok {
			ip, _ = netip.AddrFromSlice(ta.IP)
			ip = ip.WithZone(ta.Zone)
		}
	}
	if !ip.IsValid() {
		ip, _ = netip.ParseAddr(host)
	}
	if !ip.IsValid() || ip.Unmap().IsLinkLocalUnicast() {
		return false
	}
	ip = ip.Unmap().WithZone("")
	for _, p := range nets {
		if p.Bits() > 0 && p.Contains(ip) {
			return true
		}
	}
	return false
}
