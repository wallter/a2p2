package daemon

// Which of this machine's addresses to advertise (DESIGN 11): IPv4 that is
// not loopback or link-local, global IPv6 (a temporary one only where the
// interface has no stable one), then unique-local IPv6. Whether an IPv6
// address is temporary or deprecated is read from the system where it can
// be (addrFlags, build-tagged).

import (
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ifAddr is one address of an interface, as the system reports it.
type ifAddr struct {
	IP    netip.Addr
	Iface string
	// Temporary: an RFC 8981 privacy address; Unusable: deprecated,
	// tentative or duplicated, so not to be given out.
	Temporary, Unusable bool
}

// ula is fc00::/7, unique-local IPv6.
var ula = netip.MustParsePrefix("fc00::/7")

// chooseAdvertised orders the addresses to give peers for port.
func chooseAdvertised(addrs []ifAddr, port int) []string {
	stable := map[string]bool{} // interfaces with a stable global or unique-local IPv6 address
	for _, a := range addrs {
		if a.IP.Is6() && !a.IP.Is4In6() && a.IP.IsGlobalUnicast() && !a.Temporary && !a.Unusable {
			stable[a.Iface] = true
		}
	}
	var v4, v6, local []string
	p := strconv.Itoa(port)
	for _, a := range addrs {
		ip := a.IP.Unmap()
		switch {
		case !ip.IsValid(), ip.IsLoopback(), ip.IsUnspecified(), ip.IsLinkLocalUnicast(), ip.IsMulticast(), a.Unusable:
		case ip.Is4():
			v4 = append(v4, net.JoinHostPort(ip.String(), p))
		case !ip.IsGlobalUnicast(), ip.Zone() != "":
		case a.Temporary && stable[a.Iface]:
		case ula.Contains(ip):
			local = append(local, net.JoinHostPort(ip.String(), p))
		default:
			v6 = append(v6, net.JoinHostPort(ip.String(), p))
		}
	}
	return append(append(v4, v6...), local...)
}

// localAddrs lists this machine's interface addresses.
func localAddrs() []ifAddr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	flags := addrFlags()
	var out []ifAddr
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		as, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range as {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			x := ifAddr{IP: ip, Iface: ifc.Name}
			if ip.Is6() {
				f := flags(ifc.Name, ip)
				x.Temporary, x.Unusable = f.temporary, f.unusable
			}
			out = append(out, x)
		}
	}
	return out
}

// v6Flags is what the system says of one IPv6 address.
type v6Flags struct{ temporary, unusable bool }

// sameSite48 reports whether two unique-local addresses share their /48
// (the site prefix of RFC 4193).
func sameSite48(a, b netip.Addr) bool {
	p, err := a.Prefix(48)
	return err == nil && p.Contains(b)
}

// FamiliesText says, for the person deciding a2p2 net on listen, which
// address families the listener takes and the IPv6 addresses it would
// advertise now (DESIGN 11).
func FamiliesText(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	ip, perr := netip.ParseAddr(host)
	if err != nil || (perr != nil && host != "") {
		return ""
	}
	if host != "" && !ip.IsUnspecified() {
		if ip.Is4() {
			return "It listens on IPv4 only, at that one address."
		}
		return "It listens on IPv6 only, at that one address."
	}
	p, _ := strconv.Atoi(port)
	var v6 []string
	for _, a := range advertisable(localAddrs(), p, false) {
		if strings.HasPrefix(a, "[") {
			v6 = append(v6, a)
		}
	}
	s := "It listens on IPv4 and IPv6. "
	if len(v6) == 0 {
		s += "This machine has no global IPv6 address now, so it advertises none."
	} else {
		s += "It advertises these IPv6 addresses beside its IPv4 ones: " + strings.Join(v6, ", ") + "."
	}
	return s + " For IPv4 only, set listen to one IPv4 address of this machine, for example 192.168.1.5:" + port + "."
}

// advertisable is chooseAdvertised, IPv4 only for a decision recorded
// before IPv6 (Network.IPv4Only).
func advertisable(addrs []ifAddr, port int, ipv4Only bool) []string {
	out := chooseAdvertised(addrs, port)
	if ipv4Only {
		out = slices.DeleteFunc(out, func(a string) bool { return strings.HasPrefix(a, "[") })
	}
	return out
}

// liveUsable is usableFrom over the interfaces as they are now, read
// again at most every 30 seconds: for every dial path (pulls, dial-backs
// asked, net check probes and joins).
func liveUsable() func(addr string) bool {
	var mu sync.Mutex
	var at time.Time
	var f func(string) bool
	return func(addr string) bool {
		mu.Lock()
		if f == nil || time.Since(at) > 30*time.Second {
			f, at = usableFrom(localAddrs()), time.Now()
		}
		g := f
		mu.Unlock()
		return g(addr)
	}
}

// usableFrom says whether a peer's address is worth dialling from here:
// a unique-local IPv6 address only if this machine holds one in the same
// /48 itself (otherwise it cannot be routed from here).
func usableFrom(local []ifAddr) func(addr string) bool {
	var sites []netip.Addr
	for _, a := range local {
		if a.IP.Is6() && ula.Contains(a.IP) {
			sites = append(sites, a.IP)
		}
	}
	return func(addr string) bool {
		ap, err := netip.ParseAddrPort(addr)
		if err != nil || !ula.Contains(ap.Addr()) {
			return true
		}
		for _, s := range sites {
			if sameSite48(s, ap.Addr()) {
				return true
			}
		}
		return false
	}
}
