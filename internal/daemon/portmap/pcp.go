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

func (c *Client) mapPCP(ctx context.Context, internal uint16, lifetime time.Duration, prev *Mapping, note Note) (Mapping, error) {
	var m Mapping
	if prev != nil && prev.Protocol == PCP {
		m = *prev // the same nonce renews the same mapping
	} else if _, err := rand.Read(m.Nonce[:]); err != nil {
		return Mapping{}, err
	}
	m.Protocol, m.Internal, m.Gateway, m.Local = PCP, internal, c.Gateway, c.Local
	suggest := internal
	if prev != nil && prev.External.IsValid() {
		suggest = prev.External.Port()
	}
	m.External = netip.AddrPortFrom(netip.Addr{}, suggest)
	if err := note(m, true); err != nil {
		return Mapping{}, err
	}
	got, err := c.pcp(ctx, internal, lifetime, &m)
	// A definite answer, or none at all: nothing was made. The version
	// answer is not definite: it holds nothing of the request, so a host
	// on the network can send it while the router made the mapping. That
	// handle stays noted, and a PCP deletion of it costs nothing. With no
	// answer at all the router may still have made it and lost every
	// reply: one deletion is sent, not waited for, before it is let go.
	// Keeping such a handle would fill the keeper on a network without
	// PCP, where no deletion is ever answered either. A renewal is never
	// deleted so: its handle is the mapping held, which a short loss of
	// the network must not remove from the router.
	renewal := prev != nil && prev.Protocol == PCP
	if err != nil && ctx.Err() == nil && !errors.Is(err, errUnsure) && !errors.As(err, new(*oldVersion)) {
		if errors.Is(err, errNoAnswer) && !renewal {
			c.send(pcpRequest(c.Local, internal, 0, &m))
		}
		_ = note(m, false)
	}
	return got, err
}

// errUnsure: the router answered that a mapping was made, but not in a
// form that can be used; it is kept to be removed.
var errUnsure = errors.New("the router's answer cannot be used")

// pcp sends one MAP request for m (lifetime 0 deletes it) and returns the
// mapping granted.
func (c *Client) pcp(ctx context.Context, internal uint16, lifetime time.Duration, m *Mapping) (Mapping, error) {
	q := pcpRequest(c.Local, internal, lifetime, m)
	var out Mapping
	err := c.exchange(ctx, q, func(b []byte) (bool, error) {
		if len(b) >= 8 && b[0] == 0 {
			// A NAT-PMP server's answer to a request of another version
			// (RFC 6886 section 3.5, RFC 6887 section 9): opcode 128 + 1,
			// result 1, unsupported version. Any other version-0 datagram
			// is not an answer. It holds nothing of the request, so it is
			// taken only if no PCP answer comes in any of the tries.
			if len(b) <= 16 && b[1] == 0x80|pcpOpMap && binary.BigEndian.Uint16(b[2:]) == 1 {
				return false, &oldVersion{}
			}
			return false, nil
		}
		if len(b) < pcpReqLen || len(b) > 1100 || len(b)%4 != 0 || b[0] != pcpVersion || b[1] != 0x80|pcpOpMap ||
			string(b[24:36]) != string(m.Nonce[:]) || b[36] != pcpProtoTCP || binary.BigEndian.Uint16(b[40:]) != internal {
			return false, nil // not the answer to this request
		}
		if r := b[3]; r != 0 {
			return true, fmt.Errorf("refused: %s", pcpText(r))
		}
		granted := grantedFor(binary.BigEndian.Uint32(b[4:]), lifetime)
		ip := netip.AddrFrom16([16]byte(b[44:60])).Unmap()
		port := binary.BigEndian.Uint16(b[42:])
		out = *m
		out.External = netip.AddrPortFrom(ip, port)
		out.Lifetime, out.Expires = granted, c.now().Add(granted)
		if lifetime > 0 && (granted == 0 || port == 0 || !ip.IsValid() || ip.IsUnspecified()) {
			return true, fmt.Errorf("%w: success with lifetime %d s, external %s", errUnsure, granted/time.Second, out.External)
		}
		return true, nil
	})
	return out, err
}

// pcpRequest is the MAP request for m from local (lifetime 0 deletes it).
func pcpRequest(local netip.Addr, internal uint16, lifetime time.Duration, m *Mapping) []byte {
	q := make([]byte, pcpReqLen)
	q[0], q[1] = pcpVersion, pcpOpMap
	binary.BigEndian.PutUint32(q[4:], uint32(lifetime/time.Second))
	l := v4mapped(local)
	copy(q[8:24], l[:])
	copy(q[24:36], m.Nonce[:])
	q[36] = pcpProtoTCP
	binary.BigEndian.PutUint16(q[40:], internal)
	if lifetime > 0 {
		binary.BigEndian.PutUint16(q[42:], m.External.Port())
	}
	ext := v4mapped(netip.Addr{})
	copy(q[44:60], ext[:])
	return q
}

// send sends q once to the gateway's PCP and NAT-PMP port and waits for
// nothing.
func (c *Client) send(q []byte) {
	conn, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(c.Gateway, c.port())))
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write(q)
}

// grantedFor is the lifetime a router granted in seconds, taken as at most
// what was asked: a longer grant, which a host answering as the router
// can send, would stop renewal and keep its address for the whole run.
// The router may still hold the mapping that long after a crash.
func grantedFor(seconds uint32, asked time.Duration) time.Duration {
	g := time.Duration(seconds) * time.Second
	if asked > 0 && g > asked {
		return asked
	}
	return g
}

func pcpText(r byte) string {
	if s, ok := pcpResults[r]; ok {
		return s
	}
	return fmt.Sprintf("result %d", r)
}

// exchange sends q to the gateway's PCP and NAT-PMP port, resending with
// a doubling wait, until accept takes an answer (true) or the tries run
// out. Datagrams from anywhere but the gateway's port are ignored. An
// answer accept does not take but returns an error for is the outcome
// only if nothing is taken in all the tries: a forged one, which holds
// nothing of the request, then outlasts only a router that never answers.
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
	var tentative error
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
			done, err := accept(buf[:n])
			if done {
				return err
			}
			if err != nil {
				tentative = err
			}
		}
		wait *= 2
	}
	if tentative != nil {
		return tentative
	}
	return errNoAnswer
}
