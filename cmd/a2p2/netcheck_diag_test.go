package main

import (
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/node"
)

// The words of the two diagnoses: an inspecting proxy names the address
// and the issuer and what to do; a blocked local network says a VPN or
// firewall on this machine; other results keep their advice.
//
// Requirements: N2
func TestNetCheckAdviceForAnInspectingProxyAndABlockedLocalNetwork(t *testing.T) {
	proxy := addrCheckOut{Addrs: []node.AddrCheck{{Addr: "203.0.113.9:7423", Result: node.ProbeIntercepted, MS: 217,
		Issuer: `Example Corp CA`, Detail: node.InterceptedDetail("Example Corp CA")}}}
	proxy.Advice = addrAdvice(proxy)
	for _, want := range []string{`The certificate presented at 203.0.113.9:7423 is not the other machine's own: ` +
		`it claims to be issued by "Example Corp CA" (a name anyone can write). Most likely a TLS-inspecting proxy`,
		"refuses such a connection", "exempt from inspection", "another network"} {
		if !strings.Contains(proxy.Advice, want) {
			t.Errorf("advice %q lacks %q", proxy.Advice, want)
		}
	}
	if strings.Contains(proxy.Advice, "check the port") {
		t.Errorf("advice sends the reader to the port: %q", proxy.Advice)
	}
	if line := addrLine(proxy.Addrs[0]); !strings.Contains(line, `intercepted in 217 ms: the certificate there claims the issuer "Example Corp CA"; it is not the other machine's own`) {
		t.Errorf("line %q", line)
	}
	// An issuer that arrives with a control character is written out.
	proxy.Addrs[0].Issuer = "Evil\x1b[2J"
	if got := addrAdvice(proxy); strings.Contains(got, "\x1b") || !strings.Contains(got, `Evil\u{1b}[2J`) {
		t.Errorf("issuer reached the advice raw: %q", got)
	}
	local := addrCheckOut{Addrs: []node.AddrCheck{{Addr: "192.168.1.241:7423", Result: node.ProbeUnreachable,
		Detail: "connect: no route to host", OnLink: true}}}
	if got := addrAdvice(local); !strings.Contains(got, "a VPN or firewall on this machine is probably blocking local-network access") {
		t.Errorf("on-link advice %q", got)
	}
	local.Addrs[0].OnLink = false
	if got := addrAdvice(local); !strings.HasPrefix(got, "The address cannot be reached from this network") {
		t.Errorf("other unreachable advice %q", got)
	}
	plain := addrCheckOut{Addrs: []node.AddrCheck{{Addr: "h:1", Result: node.ProbeNotA2P2}}}
	if got := addrAdvice(plain); !strings.Contains(got, "not an a2p2 machine: check the port") {
		t.Errorf("not a2p2 advice %q", got)
	}
}
