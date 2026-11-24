//go:build linux

package daemon

import (
	"strings"
	"testing"
)

func TestLinuxRouteAndIPv6FlagsAreReadFromProc(t *testing.T) {
	route := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"eth0\t00000000\t0101A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"
	gw, err := parseProcRoute(strings.NewReader(route))
	if err != nil || gw.String() != "192.168.1.1" {
		t.Fatalf("gateway %v %v", gw, err)
	}
	m := parseIfInet6(strings.NewReader("20010db8000000000000000000000005 02 40 00 00 eth0\n" +
		"20010db800000000000000000000abcd 02 40 00 01 eth0\n" +
		"20010db800000000000000000000dead 02 40 00 20 eth0\n"))
	if m["eth0 2001:db8::5"].temporary || !m["eth0 2001:db8::abcd"].temporary || !m["eth0 2001:db8::dead"].unusable {
		t.Fatalf("flags %+v", m)
	}
}
