package portmap

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// PCP (RFC 6887): a MAP request is a 24-byte common header and a 36-byte
// MAP payload; the answer has the same shape with R set.
const (
	pcpVersion  = 2
	pcpOpMap    = 1
	pcpProtoTCP = 6
	pcpReqLen   = 60
)

// oldVersion: the server answered with version 0, so it speaks NAT-PMP
// only (RFC 6887 section 9).
type oldVersion struct{}

func (*oldVersion) Error() string { return "the router speaks NAT-PMP only" }

var pcpResults = map[byte]string{1: "unsupported version", 2: "not authorized (mapping refused)", 3: "malformed request",
	4: "unsupported opcode", 5: "unsupported option", 6: "malformed option", 7: "network failure",
	8: "out of resources", 9: "unsupported protocol", 10: "user quota exceeded", 11: "cannot provide an external port",
	12: "address mismatch (this machine is behind another NAT)", 13: "excessive remote peers"}

// v4mapped writes ip as the 16-byte address of RFC 6887 section 5.
func v4mapped(ip netip.Addr) [16]byte {
	if !ip.IsValid() {
		return netip.IPv6Unspecified().As16()
	}
	return ip.As16() // As16 maps an IPv4 address into ::ffff:0:0/96
}

func (c *Client) mapPCP(ctx context.Context, internal uint16, lifetime time.Duration, prev *Mapping) (Mapping, error) {
	var m Mapping
	if prev != nil && prev.Protocol == PCP {
		m = *prev // the same nonce renews the same mapping
	} else if _, err := rand.Read(m.nonce[:]); err != nil {
		return Mapping{}, err
	}
	m.Protocol, m.Internal = PCP, internal
	suggest := internal
	if prev != nil && prev.External.IsValid() {
		suggest = prev.External.Port()
	}
	m.External = netip.AddrPortFrom(netip.Addr{}, suggest)
	return c.pcp(ctx, internal, lifetime, &m)
}

// pcp sends one MAP request for m (lifetime 0 deletes it) and returns the
// mapping granted.
func (c *Client) pcp(ctx context.Context, internal uint16, lifetime time.Duration, m *Mapping) (Mapping, error) {
	q := make([]byte, pcpReqLen)
	q[0], q[1] = pcpVersion, pcpOpMap
	binary.BigEndian.PutUint32(q[4:], uint32(lifetime/time.Second))
	local := v4mapped(c.Local)
	copy(q[8:24], local[:])
	copy(q[24:36], m.nonce[:])
	q[36] = pcpProtoTCP
	binary.BigEndian.PutUint16(q[40:], internal)
	if lifetime > 0 {
		binary.BigEndian.PutUint16(q[42:], m.External.Port())
	}
	ext := v4mapped(netip.Addr{})
	copy(q[44:60], ext[:])
	var out Mapping
	err := c.exchange(ctx, q, func(b []byte) (bool, error) {
		if len(b) >= 4 && b[0] == 0 {
			return true, &oldVersion{} // a NAT-PMP answer to a PCP request
		}
		if len(b) < pcpReqLen || b[0] != pcpVersion || b[1] != 0x80|pcpOpMap || string(b[24:36]) != string(m.nonce[:]) ||
			b[36] != pcpProtoTCP || binary.BigEndian.Uint16(b[40:]) != internal {
			return false, nil // not the answer to this request
		}
		if r := b[3]; r != 0 {
			if r == 1 && b[0] == 0 {
				return true, &oldVersion{}
			}
			return true, fmt.Errorf("refused: %s", pcpText(r))
		}
		granted := time.Duration(binary.BigEndian.Uint32(b[4:])) * time.Second
		ip := netip.AddrFrom16([16]byte(b[44:60])).Unmap()
		out = *m
		out.External = netip.AddrPortFrom(ip, binary.BigEndian.Uint16(b[42:]))
		out.Lifetime, out.Expires = granted, c.now().Add(granted)
		return true, nil
	})
	return out, err
}

func pcpText(r byte) string {
	if s, ok := pcpResults[r]; ok {
		return s
	}
	return fmt.Sprintf("result %d", r)
}

// exchange sends q to the gateway's PCP and NAT-PMP port, resending with
// a doubling wait, until accept takes an answer (true) or the tries run
// out. Datagrams from anywhere but the gateway's port are ignored.
func (c *Client) exchange(ctx context.Context, q []byte, accept func([]byte) (bool, error)) error {
	server := net.UDPAddrFromAddrPort(netip.AddrPortFrom(c.Gateway, c.port()))
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Now()) })
	defer stop()
	wait, tries := c.tries()
	buf := make([]byte, 1100)
	for range tries {
		if _, err := conn.WriteToUDP(q, server); err != nil {
			return err
		}
		deadline := time.Now().Add(wait)
		_ = conn.SetReadDeadline(deadline)
		for {
			n, from, err := conn.ReadFromUDPAddrPort(buf)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break
				}
				return err
			}
			if from.Addr().Unmap() != c.Gateway || from.Port() != c.port() {
				continue
			}
			if done, err := accept(buf[:n]); done {
				return err
			}
		}
		wait *= 2
	}
	return errNoAnswer
}
