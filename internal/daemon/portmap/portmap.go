// Package portmap asks the router of the local network to forward a TCP
// port to this machine: PCP (RFC 6887) first, NAT-PMP (RFC 6886) when the
// router answers PCP with version 0, then UPnP IGD. Only the standard
// library is used. It is used only after a person decided it (a2p2 net on
// --map-port); nothing here runs by default.
//
// Every address it talks to is given to it: the gateway, the PCP and
// NAT-PMP port and the SSDP address. Tests give fake responders on
// loopback; nothing in the tests reaches a real router.
package portmap

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// Protocols, as shown.
const (
	PCP    = "pcp"
	NATPMP = "nat-pmp"
	UPnP   = "upnp"
)

// Mapping is a TCP port the router forwards to this machine, or one it
// may forward because it was asked: what Unmap needs to remove it. It is
// written to disk before each request (Note), so a later run can remove
// it; record.go is its form on disk.
type Mapping struct {
	Protocol string
	Gateway  netip.Addr // the router asked
	Local    netip.Addr // this machine's address the router forwards to
	Internal uint16
	// External is the router's external address and the port it forwards
	// (the address is unknown until the router says it; the port is
	// known from the request).
	External netip.AddrPort
	// Lifetime is what the router granted (0 with UPnP: permanent);
	// Expires when it ends.
	Lifetime time.Duration
	Expires  time.Time

	Nonce   [12]byte // PCP
	Control string   // UPnP control URL
	Service string   // UPnP service type
}

// Live reports whether the router may still forward m at now: a lease
// that has run out is not, whether or not a renewal is being tried.
func (m Mapping) Live(now time.Time) bool {
	return m.Expires.IsZero() || now.Before(m.Expires)
}

// Key says which mappings are one on the router: PCP by nonce, NAT-PMP
// by internal port (how each deletes), UPnP by control URL and external
// port.
func (m Mapping) Key() string {
	switch m.Protocol {
	case PCP:
		return fmt.Sprintf("pcp %s %x", m.Gateway, m.Nonce)
	case NATPMP:
		return fmt.Sprintf("nat-pmp %s %d", m.Gateway, m.Internal)
	}
	return fmt.Sprintf("upnp %s %d", m.Control, m.External.Port())
}

// String describes m for a person.
func (m Mapping) String() string {
	ext := m.External.String()
	if !m.External.Addr().IsValid() {
		ext = fmt.Sprintf("port %d", m.External.Port())
	}
	return fmt.Sprintf("%s mapping %s on router %s", m.Protocol, ext, m.Gateway)
}

// Note is told about a request before it is sent (asking: something may
// be created that Unmap(m) removes) and after a definite answer that it
// created nothing (asking false). A Note that fails stops the request.
type Note func(m Mapping, asking bool) error

// Client talks to one gateway.
type Client struct {
	// Gateway is the router: the default route's next hop.
	Gateway netip.Addr
	// Local is this machine's address on the gateway's network: PCP's
	// client address and UPnP's internal client.
	Local netip.Addr
	// Port is the PCP and NAT-PMP server port; 0 means 5351.
	Port uint16
	// SSDP is where the UPnP search is sent; empty means
	// 239.255.255.250:1900.
	SSDP string
	// Try is the first wait for a UDP answer, doubled on each of Tries
	// sends; zero means 250 ms and 3.
	Try   time.Duration
	Tries int
	// HTTP fetches UPnP descriptions and sends SOAP; nil means one with a
	// 3-second timeout, no proxy and no redirects.
	HTTP *http.Client
	Now  func() time.Time
}

func (c *Client) port() uint16 {
	if c.Port == 0 {
		return 5351
	}
	return c.Port
}

func (c *Client) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

func (c *Client) tries() (time.Duration, int) {
	d, n := c.Try, c.Tries
	if d <= 0 {
		d = 250 * time.Millisecond
	}
	if n <= 0 {
		n = 3
	}
	return d, n
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("portmap: redirects are not followed") }}
}

// notPublic is the IPv4 ranges a router's external address may not be
// in, beyond what netip names: shared address space (RFC 6598), "this
// network", and the reserved range with the limited broadcast address.
var notPublic = []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("240.0.0.0/4")}

// Public says whether a may be a router's external address, as given to
// members and in invites: a global unicast IPv4 address that is not
// private, shared, loopback, link-local, multicast or reserved. The
// answers it is checked on carry no authentication (DESIGN 11). Every
// request a2p2 sends is IPv4 (an IPv4 gateway, an IPv4 suggestion in PCP,
// NAT-PMP), so an honest answer is IPv4, written in PCP as an
// IPv4-mapped address; any other IPv6 address is refused, since IPv6 can
// encode a private IPv4 address in many ways (IPv4-compatible, NAT64,
// 6to4). Documentation ranges pass: nobody holds them, so they cost a
// dial a timeout only.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.Is4() || !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// Usable says why a mapping a router granted cannot be used, or nil: it
// must be Valid, as a record needs, with a Public external address and a
// port.
func (m Mapping) Usable() error {
	if !Public(m.External.Addr()) || m.External.Port() == 0 {
		return fmt.Errorf("%w: the external address %s is not a public address", errUnsure, m.External)
	}
	if err := m.Valid(); err != nil {
		return fmt.Errorf("%w: %v", errUnsure, err)
	}
	return nil
}

// errNoAnswer: nothing answered on the PCP and NAT-PMP port in time.
var errNoAnswer = errors.New("no answer")

// Map asks for internal to be forwarded for lifetime, renewing prev if
// given (same protocol first). It tries PCP, then NAT-PMP if the router
// speaks only that, then UPnP, and says why each failed. note is told of
// every request that could create a mapping before it is sent.
func (c *Client) Map(ctx context.Context, internal uint16, lifetime time.Duration, prev *Mapping, note Note) (Mapping, error) {
	if !c.Gateway.IsValid() {
		return Mapping{}, errors.New("portmap: no gateway found")
	}
	if note == nil {
		note = func(Mapping, bool) error { return nil }
	}
	var why []string
	unsure := false // a protocol answered with a grant that cannot be used
	if prev == nil || prev.Protocol != UPnP {
		m, err := c.mapPCP(ctx, internal, lifetime, prev, note)
		var old *oldVersion
		switch {
		case err == nil:
			return m, nil
		case errors.As(err, &old):
			m, err := c.mapNATPMP(ctx, internal, lifetime, prev, note)
			if err == nil {
				return m, nil
			}
			unsure = unsure || errors.Is(err, errUnsure)
			why = append(why, "NAT-PMP: "+err.Error())
		default:
			unsure = unsure || errors.Is(err, errUnsure)
			why = append(why, "PCP: "+err.Error())
		}
	}
	if ctx.Err() != nil {
		return Mapping{}, ctx.Err()
	}
	m, err := c.mapUPnP(ctx, internal, lifetime, prev, note)
	if err == nil {
		return m, nil
	}
	unsure = unsure || errors.Is(err, errUnsure)
	why = append(why, "UPnP: "+err.Error())
	err = fmt.Errorf("the router did not forward the port (%s)", strings.Join(why, "; "))
	if unsure {
		err = unsureErr{err}
	}
	return Mapping{}, err
}

// unsureErr is a failed Map in which a protocol answered with a grant
// that cannot be used: it is errUnsure, so the keeper counts the attempt
// as answered.
type unsureErr struct{ error }

func (unsureErr) Is(target error) bool { return target == errUnsure }

// Unmap removes m. A mapping the router no longer holds counts as
// removed; one on another router than this client's cannot be. Each
// deletion reaches only this machine's own mapping: PCP deletes by the
// nonce kept from the request (RFC 6887 section 15), NAT-PMP deletes only
// the sending host's mapping of the internal port (RFC 6886 section
// 3.4), and a UPnP forward is deleted only after the router says it
// points at this machine's address with a2p2's description.
func (c *Client) Unmap(ctx context.Context, m Mapping) error {
	if m.Gateway.IsValid() && m.Gateway != c.Gateway {
		return fmt.Errorf("it is on router %s, not on this network's router %s", m.Gateway, c.Gateway)
	}
	switch m.Protocol {
	case PCP:
		// A router that speaks only NAT-PMP holds no PCP mapping. Its
		// version answer counts only if no PCP answer came in any of the
		// tries, each of which resends the deletion.
		_, err := c.pcp(ctx, m.Internal, 0, &m)
		if errors.As(err, new(*oldVersion)) {
			return nil
		}
		return err
	case NATPMP:
		_, err := c.natpmp(ctx, m.Internal, 0, 0)
		return err
	case UPnP:
		return c.upnpDelete(ctx, m)
	}
	return nil
}
