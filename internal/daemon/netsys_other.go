//go:build !linux && !darwin

package daemon

import (
	"errors"
	"net/netip"
)

// addrFlags cannot tell temporary IPv6 addresses here (Windows and other
// systems): every global address counts as stable.
func addrFlags() func(iface string, ip netip.Addr) v6Flags {
	return func(string, netip.Addr) v6Flags { return v6Flags{} }
}

// errMapUnavailable: port mapping needs the default gateway, which this
// build cannot read.
var errMapUnavailable = errors.New("port mapping is not available on this system")

func defaultGateway() (netip.Addr, error) { return netip.Addr{}, errMapUnavailable }
