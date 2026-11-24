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

// Mapping is a TCP port the router forwards to this machine.
type Mapping struct {
	Protocol string
	Internal uint16
	// External is the router's external address and the port it forwards.
	External netip.AddrPort
	// Lifetime is what the router granted; Expires when it ends.
	Lifetime time.Duration
	Expires  time.Time

	nonce   [12]byte // PCP
	control string   // UPnP control URL
	service string   // UPnP service type
}

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

// errNoAnswer: nothing answered on the PCP and NAT-PMP port in time.
var errNoAnswer = errors.New("no answer")

// Map asks for internal to be forwarded for lifetime, renewing prev if
// given (same protocol first). It tries PCP, then NAT-PMP if the router
// speaks only that, then UPnP, and says why each failed.
func (c *Client) Map(ctx context.Context, internal uint16, lifetime time.Duration, prev *Mapping) (Mapping, error) {
	if !c.Gateway.IsValid() {
		return Mapping{}, errors.New("portmap: no gateway found")
	}
	var why []string
	if prev == nil || prev.Protocol != UPnP {
		m, err := c.mapPCP(ctx, internal, lifetime, prev)
		var old *oldVersion
		switch {
		case err == nil:
			return m, nil
		case errors.As(err, &old):
			m, err := c.mapNATPMP(ctx, internal, lifetime, prev)
			if err == nil {
				return m, nil
			}
			why = append(why, "NAT-PMP: "+err.Error())
		default:
			why = append(why, "PCP: "+err.Error())
		}
	}
	if ctx.Err() != nil {
		return Mapping{}, ctx.Err()
	}
	m, err := c.mapUPnP(ctx, internal, lifetime, prev)
	if err == nil {
		return m, nil
	}
	why = append(why, "UPnP: "+err.Error())
	return Mapping{}, fmt.Errorf("the router did not forward the port (%s)", strings.Join(why, "; "))
}

// Unmap removes m.
func (c *Client) Unmap(ctx context.Context, m Mapping) error {
	switch m.Protocol {
	case PCP:
		_, err := c.pcp(ctx, m.Internal, 0, &m)
		return err
	case NATPMP:
		_, err := c.natpmp(ctx, m.Internal, 0, 0)
		return err
	case UPnP:
		return c.upnpDelete(ctx, m)
	}
	return nil
}
