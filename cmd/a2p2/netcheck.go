package main

// a2p2 net check: observe, decide nothing. Without an argument the daemon
// tries its members and asks them to dial back (local API net/check).
// With HOST:PORT or --invite this process tries the addresses itself,
// with no daemon: a TCP connection (after a DNS lookup if HOST is a
// name) and a TLS handshake presenting a certificate of a fresh throwaway
// key, then closed without an HTTP request. The other side sees this
// machine's source address and that certificate; never the invite, which
// stays unused, nor this machine's key.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/peer"
)

// addrCheckOut is the answer of the HOST:PORT and --invite forms.
type addrCheckOut struct {
	Invite bool             `json:"invite,omitempty"`
	Addrs  []node.AddrCheck `json:"addrs"`
	Advice string           `json:"advice"`
}

func netCheck(c *cmd, args []string) error {
	invite := c.fs.Bool("invite", false, "check the addresses and key of an invite (token as argument or on stdin)")
	if err := c.fs.Parse(permute(c.fs, args)); err != nil {
		return errUsage
	}
	rest := c.fs.Args()
	switch {
	case *invite:
		if len(rest) > 1 {
			return usagef("a2p2 net check --invite [TOKEN]")
		}
		token := ""
		if len(rest) == 1 && rest[0] != "-" {
			token = rest[0]
		} else {
			b, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
			if err != nil {
				return err
			}
			token = strings.TrimSpace(string(b))
		}
		tok, err := core.DecodeToken(token)
		if err != nil {
			return usagef("%v", err)
		}
		want := func(n string) error {
			if fp, ok := core.KeyFingerprint(n); !ok || fp != tok.Fingerprint {
				return errors.New("not the invite's key")
			}
			return nil
		}
		return addrCheck(c, tok.Addrs, want, true)
	case len(rest) == 1:
		return addrCheck(c, rest, nil, false)
	case len(rest) > 1:
		return usagef("a2p2 net check [HOST:PORT | --invite [TOKEN]]")
	}
	var nc node.NetCheck
	return c.call("net/check", nil, &nc, func() { printNetCheck(nc) })
}

// addrCheck tries each address as a throwaway key.
func addrCheck(c *cmd, addrs []string, want func(string) error, invite bool) error {
	p, err := peer.NewThrowawayProber()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()
	out := addrCheckOut{Invite: invite}
	for _, a := range addrs {
		out.Addrs = append(out.Addrs, p.ProbeWith(ctx, a, want))
	}
	out.Advice = addrAdvice(out)
	if *c.json {
		b, _ := json.Marshal(out)
		_, err := fmt.Println(string(b))
		return err
	}
	for _, r := range out.Addrs {
		fmt.Println(addrLine(r))
		if r.Node != "" {
			fmt.Println("    key", node.EscapeDisplay(node.Fingerprint(r.Node)))
		}
	}
	fmt.Println(out.Advice) // its address is escaped (addrAdvice)
	return nil
}

// addrLine is one address tried, in words. A member's address comes from
// the address book, which holds what other machines sent, and a probe's
// detail can quote it: both are escaped, as is a result the words do not
// name.
func addrLine(r node.AddrCheck) string {
	what := map[string]string{
		node.ProbeReached:     "an a2p2 machine answered",
		node.ProbeRefused:     "refused",
		node.ProbeTimeout:     "timed out",
		node.ProbeWrongKey:    "certificate key mismatch",
		node.ProbeNotA2P2:     "not an a2p2 machine",
		node.ProbeIntercepted: "intercepted",
		node.ProbeUnreachable: "unreachable",
		node.ProbeNotTried:    "not tried",
	}[r.Result]
	if what == "" {
		what = node.EscapeDisplay(r.Result)
	}
	s := fmt.Sprintf("  %-28s %s", node.EscapeDisplay(r.Addr), what)
	if r.Result != node.ProbeNotTried {
		s += fmt.Sprintf(" in %d ms", r.MS)
	}
	if r.Detail != "" && r.Result != node.ProbeReached {
		s += ": " + node.EscapeDisplay(r.Detail)
	}
	return s
}

// addrAdvice is the closing sentence of the HOST:PORT and --invite forms.
// The address it names is escaped, as the issuer is (InterceptedAdvice):
// it came from the argument or from an invite another person wrote.
func addrAdvice(out addrCheckOut) string {
	best := ""
	addr := ""
	var top node.AddrCheck
	rank := map[string]int{node.ProbeReached: 7, node.ProbeWrongKey: 6, node.ProbeIntercepted: 5, node.ProbeNotA2P2: 4, node.ProbeRefused: 3,
		node.ProbeTimeout: 2, node.ProbeUnreachable: 1}
	for _, r := range out.Addrs {
		if best == "" || rank[r.Result] > rank[best] {
			best, addr, top = r.Result, node.EscapeDisplay(r.Addr), r
		}
	}
	switch best {
	case node.ProbeReached:
		if out.Invite {
			return "The invite's admin machine answers at " + addr + " with the key the invite names; the check " +
				"stopped after the TLS handshake (a throwaway certificate, no request), and the invite is unused."
		}
		return "An a2p2 machine answers at " + addr + "; compare the key above with the one its person reads to you."
	case node.ProbeWrongKey:
		return "The certificate offered at " + addr + " names another key than this invite's: do not use the " +
			"invite there; ask its person for a new one."
	case node.ProbeNotA2P2:
		return "Something answers at " + addr + ", but it is not an a2p2 machine: check the port."
	case node.ProbeIntercepted:
		return node.InterceptedAdvice(addr, top.Issuer, false)
	case node.ProbeRefused:
		return "Nothing listens at " + addr + ": the other machine may have networking off or listen on another port."
	case node.ProbeTimeout:
		return "No answer from " + addr + ": a firewall or router drops the connection, or the machine is off; " +
			"if it cannot accept connections, it can still take part by connecting out."
	}
	if best == node.ProbeUnreachable && top.OnLink {
		return node.OnLinkAdvice
	}
	return "The address cannot be reached from this network; check the address and this machine's connection."
}

// mappingText is the router's port mapping in one line. What the router
// answered (its address, its errors) is escaped.
func mappingText(m *node.PortMapping) string {
	left := ""
	if len(m.NotRemoved) > 0 {
		left = "; not removed yet, retried at stop and at the next start: " + escapeAll(m.NotRemoved, "; ")
	}
	if m.External == "" {
		if m.Error == "" {
			return "port mapping: none" + left
		}
		return "port mapping: none (" + node.EscapeDisplay(m.Error) + ")" + left
	}
	s := "port mapping: " + node.EscapeDisplay(m.Protocol) + ", " + node.EscapeDisplay(m.External)
	if m.Expires != "" {
		s += ", expires " + clock(m.Expires) + " unless renewed"
	} else {
		s += ", permanent until removed"
	}
	if m.Reached {
		s += "; a member reached it, so it is advertised"
	} else {
		s += "; not advertised until a member reaches it; invites carry it"
	}
	return s + left
}

func inAgo(s int64) string {
	if s < 60 {
		return fmt.Sprintf("%d s ago", s)
	}
	return fmt.Sprintf("%d min ago", s/60)
}

// printNetCheck prints the daemon's check in plain words. Much of it can
// come from other machines: members' addresses and names, room labels,
// what members say they saw, their dial-back errors and the router's
// answers. Every string that is not this function's own words is escaped
// as one line (node.EscapeDisplay); --json gives the data as it is.
func printNetCheck(nc node.NetCheck) {
	esc := node.EscapeDisplay
	switch {
	case nc.State == node.NetworkOn && nc.Listening:
		fmt.Printf("network    on, listening on %s\n", addressText(nc.Listen))
	case nc.State == node.NetworkOn:
		fmt.Printf("network    on, but not listening: %s\n", esc(nc.Fault))
	case nc.State == node.NetworkOutbound:
		fmt.Println("network    outbound only, listening on nothing")
	default:
		fmt.Println("network    off: nothing was tried")
	}
	if nc.IPv4Only {
		fmt.Println("           IPv4 only: this decision was recorded before a2p2 listened on IPv6; run a2p2 net on " +
			"again to add it")
	}
	if len(nc.Advertised) > 0 {
		fmt.Println("advertised", escapeAll(nc.Advertised, ", "))
	}
	if nc.State != node.NetworkOff {
		fmt.Println("members")
		if len(nc.Members) == 0 {
			fmt.Println("  none: no other machine is in this machine's rooms")
		}
		for _, m := range nc.Members {
			who := short(m.Node)
			if m.Name != "" {
				who = esc(m.Name) + " (" + who + ")"
			}
			fmt.Printf("  %s / %s\n", esc(m.RoomLabel), who)
			switch {
			case len(m.Addrs) == 0 && m.ConnectsIn != nil:
				fmt.Println("    connects in (last " + inAgo(m.ConnectsIn.AgoS) + ")")
			case len(m.Addrs) == 0:
				fmt.Println("    no address known: it has not said where it listens (it may connect out only)")
			case m.ConnectsIn != nil:
				fmt.Println("    also connects in (last " + inAgo(m.ConnectsIn.AgoS) + ")")
			}
			for _, a := range m.Addrs {
				fmt.Println("  " + addrLine(a))
			}
		}
	}
	line := "reachable by peers: " + esc(nc.Reach.State)
	if nc.Reach.Checked != "" {
		line += ", checked " + clock(nc.Reach.Checked)
	}
	if len(nc.Reach.Reached) > 0 {
		line += " at " + escapeAll(nc.Reach.Reached, ", ")
	}
	if nc.Reach.Why != "" {
		line += " (" + esc(nc.Reach.Why) + ")"
	}
	fmt.Println(line)
	if nc.Mapping != nil {
		fmt.Println(mappingText(nc.Mapping))
	}
	if len(nc.Reach.NotTried) > 0 {
		fmt.Println("not tried by members:", escapeAll(nc.Reach.NotTried, ", "), "("+node.NotTriedText+")")
	}
	for _, n := range nc.Reach.NotChecked {
		if len(nc.Reach.ConnectedIn) > 0 {
			n = "dial-back " + n // the connection in answers the question; the dial-back did not run
		}
		fmt.Println(esc(n))
	}
	for _, a := range nc.NotAdvertised {
		fmt.Println("not advertised:", esc(a))
	}
	if len(nc.Reach.Saw) > 0 {
		fmt.Println("members see this machine as", escapeAll(nc.Reach.Saw, ", "), "(what they say; shown, never advertised)")
	}
	if nc.Forward != "" {
		fmt.Println("if the router forwards the port by hand, config.json needs:", esc(nc.Forward))
	}
	// The advice is one sentence that can quote room labels, addresses and
	// members' errors: escaped as one line, so no line break in them
	// starts a line of its own.
	fmt.Println(esc(nc.Advice))
}
