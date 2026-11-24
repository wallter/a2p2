package node

import (
	"context"
	"net"
	"strings"

	"github.com/wallter/a2p2/internal/store"
)

// The three networking states of a machine (N1). A person chooses one;
// the daemon applies it as soon as it is recorded.
const (
	// NetworkOff: no listener, no scheduler, no join polling, no dialling.
	NetworkOff = "off"
	// NetworkOutbound: no listener; the scheduler pulls from the machines
	// in this machine's rooms and joins poll their admin machine.
	NetworkOutbound = "outbound"
	// NetworkOn: outbound, and a listener other machines dial.
	NetworkOn = "on"
)

// Network is a machine's networking decision as recorded.
type Network struct {
	State  string // off, outbound or on
	Listen string // the address listened on, when on
	// MapPort: with on, the router is asked to forward the listening port
	// (a2p2 net on --map-port; DESIGN 11). Recorded in the decision's role
	// as "map_port".
	MapPort bool
	// Request is the decision's id in requests; empty when none was ever
	// taken (networking is then off).
	Request string
}

// LoadNetwork reads the latest networking decision, by the order the
// decisions were recorded; with none, networking is off. Nothing else turns it on: not a config file, not an agent tool.
func LoadNetwork(ctx context.Context, st *store.Store) (Network, error) {
	q, ok, err := st.LatestDecision(ctx, store.RequestNetwork)
	if err != nil || !ok {
		return Network{State: NetworkOff}, err
	}
	n := Network{State: q.Action, Listen: q.Name, Request: q.ID, MapPort: q.Action == NetworkOn && q.Role == roleMapPort}
	if n.State != NetworkOutbound && n.State != NetworkOn {
		n = Network{State: NetworkOff, Request: q.ID}
	}
	return n, nil
}

// roleMapPort marks a networking decision that asks for port mapping.
const roleMapPort = "map_port"

// MapPortText is what a person is told before deciding on port mapping.
const MapPortText = "This machine will also ask the router of this network to forward the listening port to it " +
	"(PCP, NAT-PMP or UPnP), which opens that port on the router to the whole internet. Anyone can then connect; " +
	"only room members get anything from it, since every connection must prove a member's key first. The mapping " +
	"is renewed while networking is on and removed by a2p2 net off or a2p2 stop."

// NetworkText says exactly what choosing n does, for the person deciding.
func NetworkText(n Network) string {
	switch n.State {
	case NetworkOn:
		s := "listen for other machines on " + n.Listen + ", and connect out to the machines in this " +
			"machine's rooms and to admin machines of joins"
		if n.MapPort {
			s += "; and ask the router to forward the listening port from the internet"
		}
		return s
	case NetworkOutbound:
		return "listen on no address; connect out only, to the machines in this machine's rooms and to " +
			"admin machines of joins"
	}
	return "listen on no address and connect to no machine: this machine only"
}

// SetNetwork records a person's networking decision. It is reached only
// from the local API's net call, which has the same authority as confirm:
// no agent tool or hook calls it. Once it is recorded the daemon applies
// it at once (OnNetwork), not depending on the caller.
func (s *Surface) SetNetwork(ctx context.Context, n Network) (Network, error) {
	switch n.State {
	case NetworkOff, NetworkOutbound:
		if n.MapPort {
			return Network{}, userErr("port mapping needs networking on with a listener: a2p2 net on --map-port")
		}
		n.Listen = ""
	case NetworkOn:
		host, port, err := net.SplitHostPort(n.Listen)
		if err != nil || port == "" || strings.ContainsAny(host, " /") {
			return Network{}, userErr("listen on host:port, for example 0.0.0.0:7423")
		}
	default:
		return Network{}, userErr("networking is off, outbound or on")
	}
	now := s.m.now()
	q := store.Request{ID: s.r.newID(), Kind: store.RequestNetwork, Action: n.State, Name: n.Listen,
		Created: now, DecidedAt: now}
	if n.MapPort {
		q.Role = roleMapPort
	}
	if err := s.m.st.RecordDecision(ctx, q); err != nil {
		return Network{}, err
	}
	n.Request = q.ID
	if s.onNet != nil {
		s.onNet(n)
	}
	return n, nil
}

// Network is the networking state this run applies.
func (s *Surface) Network() Network { return s.net }

// dials refuses a call that would connect to another machine while
// networking is off.
func (s *Surface) dials() error {
	if s.net.State == NetworkOff {
		return codedErr(ErrorNetworkingOff, "networking is off on this machine, so it connects to no other machine; "+
			"a person can turn it on at a terminal: a2p2 net on (or a2p2 net on --outbound-only)")
	}
	return nil
}
