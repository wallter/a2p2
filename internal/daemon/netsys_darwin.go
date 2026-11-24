//go:build darwin

// The routing-table calls in syscall are frozen, not removed; their
// replacement, golang.org/x/net/route, would be a new dependency for one
// read of the default route (D-net).
//
//lint:file-ignore SA1019 the frozen syscall routing calls avoid a dependency (D-net)

package daemon

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"unsafe"
)

// The SIOCGIFAFLAG_IN6 ioctl (netinet6/in6_var.h): an in6_ifreq of 288
// bytes, the interface name in the first 16 and a sockaddr_in6 after it;
// the answer's flags overwrite the sockaddr's first four bytes.
const (
	siocgifaflagIn6  = 0xc1206949
	in6IfreqSize     = 288
	in6IffTentative  = 0x0002
	in6IffDuplicated = 0x0004
	in6IffDetached   = 0x0008
	in6IffDeprecated = 0x0010
	in6IffTemporary  = 0x0080
)

// addrFlags asks the kernel about each IPv6 address (a local ioctl; no
// packet is sent).
func addrFlags() func(iface string, ip netip.Addr) v6Flags {
	return func(iface string, ip netip.Addr) v6Flags {
		fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_DGRAM, 0)
		if err != nil {
			return v6Flags{}
		}
		defer syscall.Close(fd)
		return ioctlFlags(fd, iface, ip)
	}
}

func ioctlFlags(fd int, iface string, ip netip.Addr) v6Flags {
	if len(iface) >= 16 {
		return v6Flags{}
	}
	var req [in6IfreqSize]byte
	copy(req[:16], iface)
	req[16], req[17] = 28, syscall.AF_INET6 // sin6_len, sin6_family
	a := ip.As16()
	copy(req[24:40], a[:])
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), siocgifaflagIn6, uintptr(unsafe.Pointer(&req[0])))
	if e != 0 {
		return v6Flags{}
	}
	f := binary.LittleEndian.Uint32(req[16:20])
	return v6Flags{temporary: f&in6IffTemporary != 0,
		unusable: f&(in6IffTentative|in6IffDuplicated|in6IffDetached|in6IffDeprecated) != 0}
}

// rtfIfScope marks a route scoped to one interface (a secondary
// uplink's default route), not the one the system uses by default.
const rtfIfScope = 0x1000000

// defaultGateway reads the IPv4 default route from the routing table (a
// sysctl; no packet is sent): the unscoped one, which the system uses,
// with its interface.
func defaultGateway() (netip.Addr, string, error) {
	rib, err := syscall.RouteRIB(syscall.NET_RT_DUMP, 0)
	if err != nil {
		return netip.Addr{}, "", err
	}
	msgs, err := syscall.ParseRoutingMessage(rib)
	if err != nil {
		return netip.Addr{}, "", err
	}
	for _, m := range msgs {
		rm, ok := m.(*syscall.RouteMessage)
		if !ok || rm.Header.Flags&syscall.RTF_GATEWAY == 0 || rm.Header.Flags&syscall.RTF_UP == 0 ||
			rm.Header.Flags&rtfIfScope != 0 {
			continue
		}
		sas, err := syscall.ParseRoutingSockaddr(rm)
		if err != nil || len(sas) <= syscall.RTAX_GATEWAY {
			continue
		}
		dst, ok1 := sas[syscall.RTAX_DST].(*syscall.SockaddrInet4)
		gw, ok2 := sas[syscall.RTAX_GATEWAY].(*syscall.SockaddrInet4)
		if !ok1 || !ok2 || dst.Addr != [4]byte{} {
			continue
		}
		name := ""
		if ifc, err := net.InterfaceByIndex(int(rm.Header.Index)); err == nil {
			name = ifc.Name
		}
		return netip.AddrFrom4(gw.Addr), name, nil
	}
	return netip.Addr{}, "", errors.New("no default route")
}
