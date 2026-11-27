package node

// What this machine dials on another machine's word (DESIGN 11, F5 and
// F54 of the 2026-11-25 review). An address can come from three places:
//
//   - this machine's person: an invite pasted and accepted, and the
//     joins recorded since (Surface.SeedBook, from JoinAccept and at
//     start), and a HOST:PORT typed for net check. Dialled as given,
//     names, loopback and LAN addresses included.
//   - a member, for itself, in its own authenticated pull request
//     (AddrBook.Heard).
//   - a member, for other members, in a pull response (AddrBook.Listed).
//
// The last two are another machine's free choice, so they are kept only
// if AddrRefusal finds nothing, and checked again at every dial
// (AddrBook.Rule; every address a name resolves to is checked, and only
// those literal addresses are dialled). Private ranges stay allowed:
// members on one LAN, VPN or overlay network (RFC 1918, 100.64.0.0/10,
// unique-local IPv6) find each other that way, and that is this project's
// main use. Never dialled on another machine's word, whoever gave it and
// however it connected: a loopback, unspecified, "this network"
// (0.0.0.0/8), multicast, broadcast or link-local address, an IPv6 address
// with a zone, an IPv6 form that embeds an IPv4 address (NAT64 and 6to4
// when the embedded address is one of those; IPv4-compatible and
// IPv4-translated always), a port below 1024 other than 443, and a name
// that is localhost or ends in a number. No exception: an address that
// once completed a pinned handshake with that member is still another
// machine's word, and is dialled only if this machine's person gave it.
//
// Every path that dials a member asks the same question: pulls and
// dial-back requests (internal/peer PullClient), file transfers
// (FileClient), net check's probes of members (Surface.NetCheck), and
// status and doctor say what is held and not dialled (AddrBook.Held). A
// dial-back answered here (DialBackServer) dials only the asker's own
// source address, by the same rule.

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
	nat64       = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local  = netip.MustParsePrefix("64:ff9b:1::/48")
	sixToFour   = netip.MustParsePrefix("2002::/16")
	v4Compat    = netip.MustParsePrefix("::/96")
	v4Trans     = netip.MustParsePrefix("::ffff:0:0:0/96")
)

// RemoteAddrOK says whether addr, given by another machine, may be kept
// and dialled (AddrRefusal finds nothing).
func RemoteAddrOK(addr string) bool { return AddrRefusal(addr) == "" }

// AddrRefusal is why addr, given by another machine, is not kept or
// dialled, for a person; empty when it may be: host:port of PROTOCOL
// section 0 (core.ValidAddr), a port of 1024 or more or 443, and an IP
// address RemoteIPOK accepts or a host name that is not localhost and does
// not end in a number (so not an IP address in another spelling).
func AddrRefusal(addr string) string {
	if !core.ValidAddr(addr) {
		return "not host:port"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "not host:port"
	}
	if p, err := strconv.Atoi(port); err != nil || !remotePortOK(p) {
		return "a port below 1024 other than 443"
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return IPRefusal(ip)
	}
	if !remoteNameOK(host) {
		return "localhost, or a name that reads as a number"
	}
	return ""
}

// remotePortOK: the well-known ports (below 1024) are where a machine's
// own services listen, and a2p2 never needs one, except 443 where a
// person picks it to pass a firewall that lets only HTTPS through.
func remotePortOK(p int) bool { return p == 443 || p >= 1024 && p <= 65535 }

// RemoteIPOK says whether ip, given by another machine or found by
// looking up a name it gave, may be dialled.
func RemoteIPOK(ip netip.Addr) bool { return IPRefusal(ip) == "" }

// IPRefusal is why ip is not dialled on another machine's word; empty
// when it may be.
func IPRefusal(ip netip.Addr) string {
	if ip.Zone() != "" {
		return "an address with a zone"
	}
	if !ip.IsValid() {
		return "not an address"
	}
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	switch {
	case ip.IsLoopback():
		return "a loopback address"
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return "a link-local address"
	case ip.IsUnspecified(), thisNetwork.Contains(ip):
		return "an unspecified address"
	case ip == broadcast4, ip.IsMulticast(), ip.IsInterfaceLocalMulticast():
		return "a multicast or broadcast address"
	}
	if ip.Is6() {
		b := ip.As16()
		switch {
		case nat64.Contains(ip):
			if IPRefusal(netip.AddrFrom4([4]byte(b[12:16]))) != "" {
				return "a NAT64 address of an address not dialled"
			}
		case nat64Local.Contains(ip):
			return "a NAT64 address of a local prefix"
		case sixToFour.Contains(ip):
			if IPRefusal(netip.AddrFrom4([4]byte(b[2:6]))) != "" {
				return "a 6to4 address of an address not dialled"
			}
		case v4Compat.Contains(ip), v4Trans.Contains(ip):
			return "an IPv4-compatible or IPv4-translated address"
		}
	}
	return ""
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

// Resolver looks up a host name's addresses.
type Resolver func(ctx context.Context, host string) ([]netip.Addr, error)

// DefaultResolver is the system's.
func DefaultResolver(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// ErrAddrRefused: an address another machine gave is not dialled
// (AddrRefusal), or a name it gave resolves to no address that may be.
var ErrAddrRefused = &UserError{"an address another machine gave is not one this machine dials", ErrorNotAllowed}

// RefusedNameDetail is why a name another machine gave is not dialled
// though the name itself passes: none of the addresses it gives may be.
const RefusedNameDetail = "the name gives no address this machine dials on another machine's word"

// DialTargets is what to dial for addr: addr itself when it is dialled as
// given (AddrBook.Rule); otherwise, if AddrRefusal finds nothing, addr
// when it is a literal address, or the addresses its name resolves to now,
// looked up once with resolve and each checked with RemoteIPOK, as literal
// host:port strings, at most MaxAddrs. The dial goes to exactly those, so
// a name cannot resolve to one address when checked and another when
// dialled (DNS rebinding). ErrAddrRefused when nothing is left.
func DialTargets(ctx context.Context, addr string, given bool, resolve Resolver) ([]string, error) {
	if given {
		return []string{addr}, nil
	}
	if !RemoteAddrOK(addr) {
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
		if RemoteIPOK(ip) && len(out) < MaxAddrs {
			out = append(out, net.JoinHostPort(ip.Unmap().String(), port))
		}
	}
	if len(out) == 0 {
		return nil, ErrAddrRefused
	}
	return out, nil
}
