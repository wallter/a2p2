package node

// a2p2 net check: can this machine talk to the members of its rooms, can
// they reach it, and what to do if not. The probes themselves (TCP, then
// the pinned TLS handshake) are internal/peer's, behind Prober.

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
)

// The outcome of trying one address.
const (
	ProbeReached     = "reached"     // TCP connected and the expected key completed the handshake
	ProbeRefused     = "refused"     // nothing listens there (connection refused)
	ProbeTimeout     = "timed_out"   // no answer within the dial or handshake timeout
	ProbeWrongKey    = "wrong_key"   // an a2p2 machine answered with another key
	ProbeNotA2P2     = "not_a2p2"    // something answered TCP, but not an a2p2 machine's TLS
	ProbeUnreachable = "unreachable" // no route, network down, or another dial error
	ProbeNotTried    = "not_tried"   // over the limit of one check
)

// maxProbes bounds the addresses one check tries; probeParallel how many
// at once.
const (
	maxProbes     = 64
	probeParallel = 8
)

// AddrCheck is one address tried, with the time taken in milliseconds.
type AddrCheck struct {
	Addr   string `json:"addr"`
	Result string `json:"result"`
	MS     int64  `json:"ms"`
	// Node is the key that answered, when an a2p2 machine did.
	Node   string `json:"node,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Prober tries one address of node now (internal/peer).
type Prober interface {
	Probe(ctx context.Context, node, addr string) AddrCheck
}

// MemberCheck is one member of one room and its addresses as tried.
type MemberCheck struct {
	Room      string      `json:"room"`
	RoomLabel string      `json:"room_label"`
	Node      string      `json:"node"`
	Name      string      `json:"name,omitempty"`
	Reached   bool        `json:"reached"`
	Addrs     []AddrCheck `json:"addrs"`
}

// NetCheck is the whole answer of a2p2 net check.
type NetCheck struct {
	State      string        `json:"state"` // what runs: off, outbound or on
	Listening  bool          `json:"listening"`
	Listen     string        `json:"listen,omitempty"`
	Fault      string        `json:"fault,omitempty"`
	Advertised []string      `json:"advertised"`
	Members    []MemberCheck `json:"members"`
	Reach      Reach         `json:"reach"`
	// Mapping is the router's port mapping, when a person asked for one.
	Mapping *PortMapping `json:"mapping,omitempty"`
	Advice  string       `json:"advice"`
}

// PortMapping is the router's forwarding of the listening port (a2p2 net
// on --map-port), as shown.
type PortMapping struct {
	Protocol string `json:"protocol,omitempty"` // pcp, nat-pmp or upnp
	External string `json:"external,omitempty"` // the router's external address and port
	Expires  string `json:"expires,omitempty"`  // empty: permanent until removed
	// Reached: a member reached External, so it is advertised.
	Reached bool   `json:"reached"`
	Error   string `json:"error,omitempty"` // why the last attempt failed
}

// MappingNow is the mapping as shown, nil when none was asked for.
func (s *Surface) MappingNow() *PortMapping {
	if s.mapping == nil || s.net.State != NetworkOn || !s.net.MapPort {
		return nil
	}
	m := s.mapping()
	if m == nil {
		return &PortMapping{Error: "not mapped yet"}
	}
	c := *m
	if c.External != "" && s.reach != nil {
		c.Reached = slices.Contains(s.reach.Verified(), c.External)
	}
	return &c
}

// ReachNow is what status shows: the latest answer for a run that
// listens, otherwise no and why.
func (s *Surface) ReachNow() Reach {
	switch {
	case s.net.State == NetworkOff:
		return Reach{State: ReachNo, Why: "networking is off"}
	case s.net.State == NetworkOutbound:
		return Reach{State: ReachNo, Why: "outbound only: this machine listens on no address"}
	case s.fault() != "":
		return Reach{State: ReachNo, Why: s.fault()}
	case s.reach == nil:
		return Reach{State: ReachUnknown, Why: "not checked"}
	}
	return s.reach.Get()
}

// NetCheck tries every known address of every other member of every room
// now and, if one was reached and this machine listens, asks the members
// reached to dial back. With networking off it dials nothing.
func (s *Surface) NetCheck(ctx context.Context) (NetCheck, error) {
	nc := NetCheck{State: s.net.State, Listen: s.net.Listen, Fault: s.fault(), Advertised: s.Addrs(),
		Members: []MemberCheck{}}
	nc.Listening = nc.State == NetworkOn && nc.Fault == ""
	if nc.Advertised == nil {
		nc.Advertised = []string{}
	}
	if nc.State == NetworkOff {
		nc.Reach = s.ReachNow()
		nc.Advice = Advice(nc)
		return nc, nil
	}
	st, err := s.Status(ctx, false)
	if err != nil {
		return NetCheck{}, err
	}
	for _, r := range st.Rooms {
		if r.ID == s.local {
			continue
		}
		for _, m := range r.Members {
			if m.Node != s.m.ID {
				nc.Members = append(nc.Members, MemberCheck{Room: r.ID, RoomLabel: r.Label, Node: m.Node, Name: m.Name,
					Addrs: []AddrCheck{}})
			}
		}
	}
	// One probe per node and address, whatever the number of rooms.
	type key struct{ node, addr string }
	results := map[key]AddrCheck{}
	var todo []key
	for _, m := range nc.Members {
		if s.book == nil {
			break
		}
		for _, a := range s.book.Addrs(m.Node) {
			k := key{m.Node, a}
			if _, ok := results[k]; ok {
				continue
			}
			results[k] = AddrCheck{Addr: a, Result: ProbeNotTried}
			if len(todo) < maxProbes && s.probe != nil {
				todo = append(todo, k)
			}
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, probeParallel)
	for _, k := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			got := s.probe.Probe(ctx, k.node, k.addr)
			got.Addr = k.addr
			mu.Lock()
			results[k] = got
			mu.Unlock()
		}()
	}
	wg.Wait()
	var reached []string
	for i := range nc.Members {
		m := &nc.Members[i]
		if s.book != nil {
			for _, a := range s.book.Addrs(m.Node) {
				if r, ok := results[key{m.Node, a}]; ok {
					m.Addrs = append(m.Addrs, r)
					m.Reached = m.Reached || r.Result == ProbeReached
				}
			}
		}
		if m.Reached && !slices.Contains(reached, m.Node) {
			reached = append(reached, m.Node)
		}
	}
	if nc.Listening && s.reach != nil && len(reached) > 0 {
		nc.Reach = s.reach.Check(ctx, reached, true)
	} else {
		nc.Reach = s.ReachNow()
	}
	nc.Mapping = s.MappingNow()
	nc.Advice = Advice(nc)
	return nc, nil
}

// behindAnotherNAT: the router's external IP address is not one members
// saw this machine come from.
func behindAnotherNAT(external string, saw []string) bool {
	host, _, err := net.SplitHostPort(external)
	if err != nil || len(saw) == 0 {
		return false
	}
	return !slices.Contains(saw, host)
}

// Advice is the one sentence that ends a check: what to do.
func Advice(nc NetCheck) string {
	nodes, reached := map[string]bool{}, map[string]bool{}
	for _, m := range nc.Members {
		nodes[m.Node] = true
		if m.Reached {
			reached[m.Node] = true
		}
	}
	// Rooms in which no member was reached.
	var cut []string
	roomReached := map[string]bool{}
	for _, m := range nc.Members {
		roomReached[m.RoomLabel] = roomReached[m.RoomLabel] || m.Reached
	}
	for label, ok := range roomReached {
		if !ok {
			cut = append(cut, label)
		}
	}
	slices.Sort(cut)
	port := ""
	if _, p, err := net.SplitHostPort(nc.Listen); err == nil {
		port = p
	}
	switch {
	case nc.State == NetworkOff:
		return "Networking is off, so nothing was tried; to take part, run a2p2 net on (or a2p2 net on --outbound-only " +
			"if this machine cannot accept connections)."
	case nc.State == NetworkOn && nc.Fault != "":
		return "This machine could not listen (" + nc.Fault + "); choose another address with a2p2 net on ADDR, " +
			"or run it as outbound-only with a2p2 net on --outbound-only."
	case len(nodes) == 0:
		return "No other machine is in this machine's rooms yet; to test an address before an invite, " +
			"run a2p2 net check HOST:PORT."
	case len(reached) == 0:
		return "No member could be reached from here: check that this network lets connections out (a firewall, " +
			"proxy or VPN may block them) and that at least one member is running with networking on."
	case nc.State == NetworkOutbound:
		s := fmt.Sprintf("This machine connects out only, which works while one member of each room accepts "+
			"connections; %d of %d members were reached", len(reached), len(nodes))
		if len(cut) > 0 {
			s += ", none in " + strings.Join(cut, ", ")
		}
		return s + "."
	case nc.Reach.State != ReachYes && nc.Mapping != nil && behindAnotherNAT(nc.Mapping.External, nc.Reach.Saw):
		return "The router forwards port " + nc.Mapping.External + ", but members see this machine as " +
			strings.Join(nc.Reach.Saw, ", ") + ": the router is itself behind another NAT (carrier-grade NAT or a " +
			"second router), so the mapping cannot help; run outbound-only (a2p2 net on --outbound-only) and make " +
			"sure at least one member of each room is reachable."
	case nc.Reach.State == ReachNo && nc.Mapping != nil && nc.Mapping.Error != "":
		return "Members cannot connect to this machine and the router did not forward the port (" +
			nc.Mapping.Error + "): run it as outbound-only (a2p2 net on --outbound-only) and make sure at least one " +
			"member of each room is reachable."
	case nc.Reach.State == ReachNo:
		s := "Members cannot connect to this machine: run it as outbound-only (a2p2 net on --outbound-only) and make " +
			"sure at least one member of each room is reachable"
		if port != "" {
			s += ", or let connections in to port " + port + " (firewall, router)"
		}
		return s + "."
	case nc.Reach.State == ReachUnknown:
		return "Members were reached, but none could test connecting back (" + nc.Reach.Why + "); ask a member's " +
			"person to run a2p2 net check there."
	case len(reached) < len(nodes):
		return fmt.Sprintf("This machine reaches %d of %d members and they can reach it; the others may be "+
			"asleep, offline or reachable only by connecting out themselves.", len(reached), len(nodes))
	}
	return "This machine reaches every member and they can reach it; nothing to do."
}
