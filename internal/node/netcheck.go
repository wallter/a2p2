package node

// a2p2 net check: can this machine talk to the members of its rooms, can
// they reach it, and what to do if not. The probes themselves (TCP, then
// the pinned TLS handshake) are internal/peer's, behind Prober.

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The outcome of trying one address.
const (
	ProbeReached     = "reached"     // TCP connected and the expected key completed the handshake
	ProbeRefused     = "refused"     // nothing listens there (connection refused)
	ProbeTimeout     = "timed_out"   // no answer within the dial or handshake timeout
	ProbeWrongKey    = "wrong_key"   // the certificate offered names another key than expected (not proof that key is held)
	ProbeNotA2P2     = "not_a2p2"    // something answered TCP, but not an a2p2 machine's TLS
	ProbeIntercepted = "intercepted" // a TLS-inspecting proxy answered: re-signed certificate (Issuer says by whom)
	ProbeUnreachable = "unreachable" // no route, network down, or another dial error
	ProbeNotTried    = "not_tried"   // over the limit of one check, or not routable from here (Detail says)
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
	// Issuer is the name of the CA that issued the certificate of an
	// intercepted connection: bounded and escaped, from an untrusted
	// certificate.
	Issuer string `json:"issuer,omitempty"`
	// OnLink: unreachable with "no route to host" at an address inside a
	// network this machine is attached to (local-network access is blocked
	// here, often by a VPN or firewall).
	OnLink bool `json:"on_link,omitempty"`
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
	// ConnectsIn is its latest pinned connection to this machine's
	// listener, when within InboundFor (Reach.ConnectedIn).
	ConnectsIn *ConnectIn `json:"connects_in,omitempty"`
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
	// IPv4Only: the decision was recorded before a2p2 listened on IPv6
	// (Network.IPv4Only).
	IPv4Only bool `json:"ipv4_only,omitempty"`
	// NotAdvertised is the configured "advertise" entries the listener
	// cannot serve, each with why.
	NotAdvertised []string `json:"not_advertised,omitempty"`
	// Mapping is the router's port mapping, when a person asked for one
	// or one is left to remove.
	Mapping *PortMapping `json:"mapping,omitempty"`
	// Forward, when members see this machine at a public address it does
	// not advertise and no mapping was asked for, is the config line that
	// advertises it, for a person who forwarded the port by hand.
	Forward string `json:"forward,omitempty"`
	Advice  string `json:"advice"`
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
	// NotRemoved is the mappings (of this run or an earlier one) that may
	// still be on the router and could not be removed yet, each with why.
	NotRemoved []string `json:"not_removed,omitempty"`
}

// MappingNow is the mapping as shown: nil when none was asked for and
// none is left to remove.
func (s *Surface) MappingNow() *PortMapping {
	if s.mapping == nil {
		return nil
	}
	m := s.mapping()
	asked := s.net.State == NetworkOn && s.net.MapPort
	if m == nil {
		if !asked {
			return nil
		}
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
		Members: []MemberCheck{}, IPv4Only: s.net.State == NetworkOn && s.net.IPv4Only, NotAdvertised: s.notAdv}
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
			results[k] = AddrCheck{Addr: a, Result: ProbeNotTried, Detail: "over the limit of one check"}
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
	byFamily := map[string][]string{} // members reached over each family: asked over it
	for i := range nc.Members {
		m := &nc.Members[i]
		if s.book != nil {
			for _, a := range s.book.Addrs(m.Node) {
				if r, ok := results[key{m.Node, a}]; ok {
					m.Addrs = append(m.Addrs, r)
					m.Reached = m.Reached || r.Result == ProbeReached
					if f := AddrFamily(a); r.Result == ProbeReached && f != "" && !slices.Contains(byFamily[f], m.Node) {
						byFamily[f] = append(byFamily[f], m.Node)
					}
				}
			}
		}
		if m.Reached && !slices.Contains(reached, m.Node) {
			reached = append(reached, m.Node)
		}
	}
	if nc.Listening && s.reach != nil && len(reached) > 0 {
		nc.Reach = s.reach.CheckFamilies(ctx, byFamily, true)
	} else {
		nc.Reach = s.ReachNow()
	}
	for i := range nc.Members {
		for _, ci := range nc.Reach.ConnectedIn {
			if ci.Node == nc.Members[i].Node {
				c := ci
				nc.Members[i].ConnectsIn = &c
			}
		}
	}
	nc.Mapping = s.MappingNow()
	nc.Forward = forwardLine(nc)
	nc.Advice = Advice(nc)
	return nc, nil
}

// forwardLine is the config line for a port forwarded by hand: when no
// mapping was asked for, members could not connect, and they see this
// machine at a public address it does not advertise. What members say
// they see is never advertised by itself (DESIGN 11); a person who knows
// the router forwards the port says so in config.json.
func forwardLine(nc NetCheck) string {
	_, port, err := net.SplitHostPort(nc.Listen)
	if err != nil || nc.Mapping != nil || nc.Reach.State == ReachYes || !nc.Listening {
		return ""
	}
	for _, x := range nc.Reach.Saw {
		ip, err := netip.ParseAddr(x)
		if err != nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ula.Contains(ip) || cgnat.Contains(ip) {
			continue
		}
		a := net.JoinHostPort(ip.String(), port)
		if slices.Contains(nc.Advertised, a) {
			continue
		}
		list := append([]string{a}, nc.Advertised...)
		if len(list) > MaxAddrs {
			list = list[:MaxAddrs]
		}
		q := make([]string, len(list))
		for i, l := range list {
			q[i] = strconv.Quote(l)
		}
		return `"advertise": [` + strings.Join(q, ", ") + `]`
	}
	return ""
}

var (
	ula   = netip.MustParsePrefix("fc00::/7")
	cgnat = netip.MustParsePrefix("100.64.0.0/10") // shared address space (RFC 6598)
)

// behindAnotherNAT: the router's external IP address is not one members
// saw this machine come from.
func behindAnotherNAT(external string, saw []string) bool {
	host, _, err := net.SplitHostPort(external)
	if err != nil || len(saw) == 0 {
		return false
	}
	return !slices.Contains(saw, host)
}

// InterceptedDetail is the address line's reason for an intercepted
// result. issuer is the name the certificate claims, already bounded and
// escaped (it is untrusted; nothing here verifies it).
func InterceptedDetail(issuer string) string {
	if issuer == "" {
		return "the certificate there is not the other machine's own"
	}
	return "the certificate there claims the issuer \"" + issuer + "\"; it is not the other machine's own"
}

// InterceptedAdvice is the closing advice for an intercepted address, in
// the CLI's HOST:PORT check and the daemon's member check alike. short
// leaves out the options.
func InterceptedAdvice(addr, issuer string, short bool) string {
	s := "The certificate presented at " + addr + " is not the other machine's own"
	if issuer != "" {
		s += ": it claims to be issued by \"" + EscapeDisplay(issuer) + "\" (a name anyone can write)"
	}
	s += ". Most likely a TLS-inspecting proxy on this network re-signs connections. a2p2 refuses such a " +
		"connection, because it could not tell the other machine from whatever answers."
	if short {
		return s
	}
	return s + " Options: use a2p2 between machines inside this network; ask the network's administrators whether " +
		"this destination can be exempt from inspection; or connect from another network."
}

// OnLinkAdvice is the closing advice for an unreachable address on a
// network this machine is attached to (AddrCheck.OnLink).
const OnLinkAdvice = "The address is on a network this machine is attached to, yet cannot be reached: a VPN or " +
	"firewall on this machine is probably blocking local-network access. Reach the other machine another way, " +
	"for example its public address with a forwarded port, or a member both machines can reach."

// Advice is the one sentence that ends a check: what to do.
func Advice(nc NetCheck) string {
	nodes, reached, in := map[string]bool{}, map[string]bool{}, map[string]bool{}
	known := false // some member has an address to try
	for _, m := range nc.Members {
		nodes[m.Node] = true
		if m.Reached {
			reached[m.Node] = true
		}
		if m.ConnectsIn != nil {
			in[m.Node] = true
		}
		known = known || len(m.Addrs) > 0
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
	case len(reached) == 0 && !known && nc.Listening && len(in) > 0:
		verb := "connect"
		if len(in) == 1 {
			verb = "connects"
		}
		s := fmt.Sprintf("%d of %d members %s in to this machine", len(in), len(nodes), verb)
		if len(in) == len(nodes) {
			return s + "; nothing to do."
		}
		return s + "; the others have said nowhere they listen and have not connected in within the last " +
			fmt.Sprintf("%d minutes (they may be asleep or offline).", int(InboundFor/time.Minute))
	case len(reached) == 0 && !known && nc.Listening:
		return fmt.Sprintf("No member has said where it listens, and none has connected in within the last %d "+
			"minutes: each may connect out only and be asleep or offline; nothing to fix on this machine.",
			int(InboundFor/time.Minute))
	case len(reached) == 0 && firstAddr(nc, ProbeIntercepted, false) != nil:
		a := firstAddr(nc, ProbeIntercepted, false)
		return "No member could be reached. " + InterceptedAdvice(a.Addr, a.Issuer, true)
	case len(reached) == 0 && firstAddr(nc, ProbeUnreachable, true) != nil:
		return "No member could be reached. " + OnLinkAdvice
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
	case nc.Reach.State == ReachNo && nc.Forward != "":
		return "Members cannot connect to this machine; if you forwarded port " + port + " on the router to it by " +
			"hand, add " + nc.Forward + " to config.json and restart, otherwise run it as outbound-only " +
			"(a2p2 net on --outbound-only)."
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

// firstAddr is the first member address tried with result (and, with
// onLink, found on a network this machine is attached to), or nil.
func firstAddr(nc NetCheck, result string, onLink bool) *AddrCheck {
	for _, m := range nc.Members {
		for i := range m.Addrs {
			if a := &m.Addrs[i]; a.Result == result && (!onLink || a.OnLink) {
				return a
			}
		}
	}
	return nil
}
