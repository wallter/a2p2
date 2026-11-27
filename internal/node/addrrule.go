package node

// What this machine dials on another machine's word (DESIGN 11, F5 and
// F54 of the 2026-11-25 review). An address can come from three places:
//
//   - this machine's person: an invite pasted and accepted, and the
//     joins recorded since (AddrBook.Learn, Replace; Surface.SeedBook).
//     Dialled as given, names, loopback and LAN addresses included.
//   - a member, for itself, in its own authenticated pull request
//     (AddrBook.Heard).
//   - a member, for other members, in a pull response (AddrBook.Listed).
//
// The last two are another machine's free choice, so they are kept only
// if RemoteAddrOK holds, and checked again at every dial (RemoteIPOK on
// every address a name resolves to, and only those literal addresses are
// dialled). Private ranges stay allowed: members on one LAN, VPN or
// overlay network (RFC 1918, 100.64.0.0/10, unique-local IPv6) find each
// other that way, and that is this project's main use. What is never
// dialled on another machine's word: an unspecified, "this network"
// (0.0.0.0/8), multicast, broadcast or link-local address, an IPv6
// address with a zone, a port below 1024 other than 443, a name that is
// localhost or ends in a number, and a loopback address unless the member
// is on this computer: its own request came over loopback in this run, or
// the member that lists it answered over loopback (it then reaches nothing
// through this machine it could not reach itself). An IP address that
// completed a pinned handshake with the member is dialled as given
// (AddrBook.DialPolicy).

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/wallter/a2p2/internal/core"
)

var (
	thisNetwork = netip.MustParsePrefix("0.0.0.0/8")
	broadcast4  = netip.AddrFrom4([4]byte{255, 255, 255, 255})
)

// RemoteAddrOK says whether addr, given by another machine, may be kept
// and dialled: host:port of PROTOCOL section 0 (core.ValidAddr), a port of
// 1024 or more or 443, and an IP address RemoteIPOK accepts or a host name
// that is not localhost and does not end in a number (so not an IP
// address in another spelling). loopback: the machine that gave it is
// connected to this one over loopback.
func RemoteAddrOK(addr string, loopback bool) bool {
	if !core.ValidAddr(addr) {
		return false
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if p, err := strconv.Atoi(port); err != nil || !remotePortOK(p) {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return RemoteIPOK(ip, loopback)
	}
	return remoteNameOK(host)
}

// remotePortOK: the well-known ports (below 1024) are where a machine's
// own services listen, and a2p2 never needs one, except 443 where a
// person picks it to pass a firewall that lets only HTTPS through.
func remotePortOK(p int) bool { return p == 443 || p >= 1024 && p <= 65535 }

// RemoteIPOK says whether ip, given by another machine or found by
// looking up a name it gave, may be dialled.
func RemoteIPOK(ip netip.Addr, loopback bool) bool {
	if ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	switch {
	case !ip.IsValid(), ip.IsUnspecified(), thisNetwork.Contains(ip), ip == broadcast4, ip.IsMulticast(),
		ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast():
		return false
	case ip.IsLoopback():
		return loopback
	}
	return true
}

// remoteNameOK refuses localhost and names whose last label is a number
// (127.1 or 0x7f.1 read as an address by some resolvers).
func remoteNameOK(host string) bool {
	h := strings.ToLower(host)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return false
	}
	last := h[strings.LastIndexByte(h, '.')+1:]
	if strings.HasPrefix(last, "0x") {
		return false
	}
	for _, c := range last {
		if c < '0' || c > '9' {
			return true
		}
	}
	return false
}

// viaLoopback says whether a peer reached at addr is on this machine:
// addr is a loopback address, or localhost (only an invite gives that).
func viaLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// isLoopbackAddr says whether addr is a literal loopback address.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// Resolver looks up a host name's addresses.
type Resolver func(ctx context.Context, host string) ([]netip.Addr, error)

// DefaultResolver is the system's.
func DefaultResolver(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// ErrAddrRefused: an address another machine gave is not dialled
// (RemoteAddrOK), or a name it gave resolves to no address that may be.
var ErrAddrRefused = &UserError{"an address another machine gave is not one this machine dials", ErrorNotAllowed}

// DialTargets is what to dial for addr: addr itself when this machine's
// person gave it (given); otherwise, if RemoteAddrOK holds, addr when it
// is a literal address, or the addresses its name resolves to now, looked
// up once with resolve and each checked with RemoteIPOK, as literal
// host:port strings. The dial goes to exactly those, so a name cannot
// resolve to one address when checked and another when dialled (DNS
// rebinding). ErrAddrRefused when nothing is left.
func DialTargets(ctx context.Context, addr string, given, loopback bool, resolve Resolver) ([]string, error) {
	if given {
		return []string{addr}, nil
	}
	if !RemoteAddrOK(addr, loopback) {
		return nil, ErrAddrRefused
	}
	host, port, _ := net.SplitHostPort(addr)
	if _, err := netip.ParseAddr(host); err == nil {
		return []string{addr}, nil
	}
	if resolve == nil {
		resolve = DefaultResolver
	}
	ips, err := resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ip := range ips {
		if RemoteIPOK(ip, loopback) && len(out) < MaxAddrs {
			out = append(out, net.JoinHostPort(ip.Unmap().String(), port))
		}
	}
	if len(out) == 0 {
		return nil, ErrAddrRefused
	}
	return out, nil
}
