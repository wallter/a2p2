//go:build linux

package daemon

import (
	"strings"
	"testing"
)

// The default route is the one with the lowest metric among those up
// with a gateway; with none, there is no gateway and nothing is mapped.
// The real /proc of the machine running the test is read too, and must
// not fail whether or not it has a default route or IPv6.
func TestLinuxRouteAndIPv6FlagsAreReadFromProc(t *testing.T) {
	route := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"wlan0\t00000000\t0102A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
		"eth0\t00000000\t0101A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"tun0\t00000000\t0A000001\t0002\t0\t0\t50\t00000000\t0\t0\t0\n" // not up
	gw, iface, err := parseProcRoute(strings.NewReader(route))
	if err != nil || gw.String() != "192.168.1.1" || iface != "eth0" {
		t.Fatalf("gateway %v on %q: %v", gw, iface, err)
	}
	if _, _, err := parseProcRoute(strings.NewReader("Iface\tDestination\n")); err == nil {
		t.Fatal("a gateway from a table with no default route")
	}
	m := parseIfInet6(strings.NewReader("20010db8000000000000000000000005 02 40 00 00 eth0\n" +
		"20010db800000000000000000000abcd 02 40 00 01 eth0\n" +
		"20010db800000000000000000000dead 02 40 00 20 eth0\n"))
	if m["eth0 2001:db8::5"].temporary || !m["eth0 2001:db8::abcd"].temporary || !m["eth0 2001:db8::dead"].unusable {
		t.Fatalf("flags %+v", m)
	}
	// This system's own files: read without error or panic, whatever they
	// hold (a container may have no default route and no IPv6).
	_ = addrFlags()
	_ = localAddrs()
	if _, _, err := defaultGateway(); err != nil && !strings.Contains(err.Error(), "no default route") &&
		!strings.Contains(err.Error(), "no such file") {
		t.Fatalf("reading /proc/net/route: %v", err)
	}
}
