//go:build linux

package daemon

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// IPv6 address flags of /proc/net/if_inet6 (linux/if_addr.h).
const (
	ifaFTemporary  = 0x01
	ifaFDADFailed  = 0x08
	ifaFDeprecated = 0x20
	ifaFTentative  = 0x40
)

// addrFlags reads /proc/net/if_inet6 once.
func addrFlags() func(iface string, ip netip.Addr) v6Flags {
	f, err := os.Open("/proc/net/if_inet6")
	if err != nil {
		return func(string, netip.Addr) v6Flags { return v6Flags{} }
	}
	defer f.Close()
	m := parseIfInet6(f)
	return func(iface string, ip netip.Addr) v6Flags { return m[iface+" "+ip.String()] }
}

// parseIfInet6 reads lines of "addr ifindex prefixlen scope flags name".
func parseIfInet6(r io.Reader) map[string]v6Flags {
	m := map[string]v6Flags{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 6 || len(fs[0]) != 32 {
			continue
		}
		raw, err := hex.DecodeString(fs[0])
		if err != nil {
			continue
		}
		flags, err := strconv.ParseUint(fs[4], 16, 32)
		if err != nil {
			continue
		}
		ip := netip.AddrFrom16([16]byte(raw))
		m[fs[5]+" "+ip.String()] = v6Flags{temporary: flags&ifaFTemporary != 0,
			unusable: flags&(ifaFDADFailed|ifaFDeprecated|ifaFTentative) != 0}
	}
	return m
}

// defaultGateway reads the IPv4 default route from /proc/net/route: its
// next hop and its interface.
func defaultGateway() (netip.Addr, string, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return netip.Addr{}, "", err
	}
	defer f.Close()
	return parseProcRoute(f)
}

// parseProcRoute picks, among the lines with destination 0, mask 0 and
// the up and gateway flags, the one with the lowest metric (the route the
// kernel uses; the first of equals); addresses are hex in host
// (little-endian) order.
func parseProcRoute(r io.Reader) (netip.Addr, string, error) {
	sc := bufio.NewScanner(r)
	var gw netip.Addr
	iface := ""
	best := uint64(1 << 63)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 8 || fs[1] != "00000000" || fs[7] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fs[3], 16, 32)
		if err != nil || flags&0x3 != 0x3 { // RTF_UP | RTF_GATEWAY
			continue
		}
		g, err1 := strconv.ParseUint(fs[2], 16, 32)
		metric, err2 := strconv.ParseUint(fs[6], 10, 32)
		if err1 != nil || err2 != nil || metric >= best {
			continue
		}
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(g))
		gw, iface, best = netip.AddrFrom4(b), fs[0], metric
	}
	if !gw.IsValid() {
		return netip.Addr{}, "", errors.New("no default route")
	}
	return gw, iface, nil
}
