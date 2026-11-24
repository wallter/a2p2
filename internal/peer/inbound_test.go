package peer

import (
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/node"
)

// inH is a machine's peer handler that records connections in.
type inH struct {
	dbH
	r *node.Reacher
}

func (h inH) Inbound(from, local string) { h.r.ConnectedIn(from, local) }

// Over real TLS on loopback: a member that only connects out reaches this
// machine's listener, and "reachable by peers" becomes yes, naming that
// member and the local address it arrived on; a machine in no shared room
// completes a handshake too and is refused, and counts for nothing.
func TestOverTLSAMemberConnectingInMakesTheListenerReachableAndANonMemberDoesNot(t *testing.T) {
	a, b := newPM(t, "a", true), newPM(t, "b", true)
	group(t, a, b)
	r := node.NewReacher(a.cl, node.ReachOpts{Members: a.r.OtherMembers, Addrs: func() []string { return nil }})
	addr := serve(t, a.id, inH{dbH{a.h, node.NewDialBackServer(a.r, NewProber(a.id))}, r}, nil)

	x := newPM(t, "x", false) // shares no room with a
	x.book.Replace(a.node(), []string{addr}, time.Now())
	if _, err := x.cl.DialBack(bg, a.node(), []string{"127.0.0.1:1"}); err == nil {
		t.Fatal("a non-member was answered")
	}
	if got := r.Get(); got.State == node.ReachYes || len(got.ConnectedIn) != 0 {
		t.Fatalf("a non-member's handshake counted: %+v", got)
	}

	b.book.Keep(func(n string) bool { return n != a.node() })
	b.book.Replace(a.node(), []string{addr}, time.Now())
	if _, err := b.cl.DialBack(bg, a.node(), []string{"127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	got := r.Get()
	if got.State != node.ReachYes || len(got.ConnectedIn) != 1 || got.ConnectedIn[0].Node != b.node() ||
		got.ConnectedIn[0].Local != addr {
		t.Fatalf("after a member connected in: %+v", got)
	}
}
