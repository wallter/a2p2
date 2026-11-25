package node

import (
	"context"
	"strings"
	"testing"
)

// answerProber returns a whole AddrCheck for "node addr".
type answerProber map[string]AddrCheck

func (f answerProber) Probe(_ context.Context, node, addr string) AddrCheck {
	r, ok := f[node+" "+addr]
	if !ok {
		panic("probed " + addr + " unexpectedly")
	}
	r.Addr = addr
	return r
}

// The daemon's member check ends with the same words as a2p2 net check
// HOST:PORT when no member could be reached and a member's address was
// intercepted, or was unreachable on a network this machine is attached to;
// an unreachable address that is not on such a network keeps the generic
// sentence.
//
// Requirements: N2
func TestMemberNetCheckClosesWithTheInterceptedAndOnLinkAdvice(t *testing.T) {
	a, b, _, _, _ := dialBackRoom(t)
	addr := "198.51.100.2:7423"
	for _, c := range []struct {
		name string
		got  AddrCheck
		want string
	}{
		{"intercepted", AddrCheck{Result: ProbeIntercepted, Issuer: "Example Corp CA", Detail: InterceptedDetail("Example Corp CA")},
			"No member could be reached. " + InterceptedAdvice(addr, "Example Corp CA", true)},
		{"on a network this machine is attached to", AddrCheck{Result: ProbeUnreachable, OnLink: true, Detail: "connect: no route to host"},
			"No member could be reached. " + OnLinkAdvice},
		{"unreachable elsewhere", AddrCheck{Result: ProbeUnreachable, Detail: "connect: no route to host"},
			"No member could be reached from here: check that this network lets connections out"},
	} {
		t.Run(c.name, func(t *testing.T) {
			book := NewAddrBook()
			book.Replace(b.id, []string{addr}, *a.clock)
			s, err := OpenSurface(context.Background(), a.m, a.r, SurfaceOpts{Name: "a", Book: book,
				Addrs:   func() []string { return []string{"198.51.100.1:7423"} },
				Network: Network{State: NetworkOn, Listen: "0.0.0.0:7423"}, Probe: answerProber{b.id + " " + addr: c.got}})
			if err != nil {
				t.Fatal(err)
			}
			nc, err := s.NetCheck(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(nc.Advice, c.want) {
				t.Fatalf("advice %q, want it to start %q", nc.Advice, c.want)
			}
		})
	}
	if !strings.Contains(InterceptedAdvice(addr, "X", true), `"X" (a name anyone can write)`) ||
		strings.Contains(InterceptedAdvice(addr, "X", true), "Options") ||
		!strings.Contains(InterceptedAdvice(addr, "X", false), "exempt from inspection") {
		t.Fatal("the short and the full advice differ by the options only")
	}
}
