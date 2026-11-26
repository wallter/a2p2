package portmap

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// NAT-PMP (RFC 6886): an external address request (opcode 0) and a TCP
// mapping request (opcode 2), each answered with opcode + 128.
const (
	pmpOpAddr = 0
	pmpOpTCP  = 2
)

var pmpResults = map[uint16]string{1: "unsupported version", 2: "not authorized (mapping refused)",
	3: "network failure", 4: "out of resources", 5: "unsupported opcode"}

func pmpText(r uint16) string {
	if s, ok := pmpResults[r]; ok {
		return s
	}
	return fmt.Sprintf("result %d", r)
}

func (c *Client) mapNATPMP(ctx context.Context, internal uint16, lifetime time.Duration, prev *Mapping, note Note) (Mapping, error) {
	suggest := internal
	if prev != nil && prev.External.IsValid() {
		suggest = prev.External.Port()
	}
	handle := Mapping{Protocol: NATPMP, Gateway: c.Gateway, Local: c.Local, Internal: internal,
		External: netip.AddrPortFrom(netip.Addr{}, suggest)}
	if err := note(handle, true); err != nil {
		return Mapping{}, err
	}
	m, err := c.natpmp(ctx, internal, suggest, lifetime)
	if err != nil {
		if ctx.Err() == nil && !errors.Is(err, errUnsure) {
			_ = note(handle, false)
		}
		return Mapping{}, err
	}
	// The mapping exists from here: a failure below leaves it noted, to be
	// removed.
	ip, err := c.natpmpAddr(ctx)
	if err != nil {
		return Mapping{}, err
	}
	m.External = netip.AddrPortFrom(ip, m.External.Port())
	return m, nil
}

// natpmp sends one TCP mapping request (lifetime 0 deletes it).
func (c *Client) natpmp(ctx context.Context, internal, suggest uint16, lifetime time.Duration) (Mapping, error) {
	q := make([]byte, 12)
	q[0], q[1] = 0, pmpOpTCP
	binary.BigEndian.PutUint16(q[4:], internal)
	if lifetime > 0 {
		binary.BigEndian.PutUint16(q[6:], suggest)
	}
	binary.BigEndian.PutUint32(q[8:], uint32(lifetime/time.Second))
	var out Mapping
	err := c.exchange(ctx, q, func(b []byte) (bool, error) {
		if len(b) != 16 || b[0] != 0 || b[1] != 128+pmpOpTCP || binary.BigEndian.Uint16(b[8:]) != internal {
			return false, nil
		}
		if r := binary.BigEndian.Uint16(b[2:]); r != 0 {
			return true, fmt.Errorf("refused: %s", pmpText(r))
		}
		granted := grantedFor(binary.BigEndian.Uint32(b[12:]), lifetime)
		port := binary.BigEndian.Uint16(b[10:])
		out = Mapping{Protocol: NATPMP, Gateway: c.Gateway, Local: c.Local, Internal: internal, Lifetime: granted,
			Expires: c.now().Add(granted), External: netip.AddrPortFrom(netip.Addr{}, port)}
		if lifetime > 0 && (granted == 0 || port == 0) {
			return true, fmt.Errorf("%w: success with lifetime %d s, external port %d", errUnsure, granted/time.Second, port)
		}
		return true, nil
	})
	return out, err
}

// natpmpAddr asks for the router's external address.
func (c *Client) natpmpAddr(ctx context.Context) (netip.Addr, error) {
	var ip netip.Addr
	err := c.exchange(ctx, []byte{0, pmpOpAddr}, func(b []byte) (bool, error) {
		if len(b) != 12 || b[0] != 0 || b[1] != 128+pmpOpAddr {
			return false, nil
		}
		if r := binary.BigEndian.Uint16(b[2:]); r != 0 {
			return true, fmt.Errorf("external address refused: %s", pmpText(r))
		}
		ip = netip.AddrFrom4([4]byte(b[8:12]))
		if ip.IsUnspecified() {
			return true, errors.New("the router has no external address")
		}
		return true, nil
	})
	return ip, err
}
