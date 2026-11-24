package node

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// A listener whose members only connect out has no dial-back answer, but
// a current member that completed a pinned connection in within
// InboundFor shows it is reachable: yes, saying how it is known and what
// it proves. The window passing, the member's removal, and a key that was
// never a member each count for nothing.
func TestAMemberThatConnectedInRecentlyMakesReachYesUntilTheWindowPassesOrItIsRemoved(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	members := []string{"m1"}
	r := NewReacher(&fakeDialBack{}, ReachOpts{Now: func() time.Time { return now },
		Members: func() []string { mu.Lock(); defer mu.Unlock(); return members },
		Addrs:   func() []string { return []string{"127.0.0.1:17423"} }})
	r.ConnectedIn("stranger", "127.0.0.1:17423") // a handshake from a key in no room
	if got := r.Get(); got.State != ReachUnknown || len(got.ConnectedIn) != 0 {
		t.Fatalf("a non-member's connection counted: %+v", got)
	}
	r.ConnectedIn("m1", "127.0.0.1:17423")
	now = now.Add(12 * time.Second)
	got := r.Get()
	if got.State != ReachYes || len(got.ConnectedIn) != 1 || got.ConnectedIn[0].AgoS != 12 ||
		got.ConnectedIn[0].Local != "127.0.0.1:17423" || !strings.Contains(got.Why, "connected in 12 s ago") ||
		!strings.Contains(got.Why, "does not test the other addresses") {
		t.Fatalf("after a member connected in: %+v", got)
	}
	now = now.Add(InboundFor)
	if got := r.Get(); got.State != ReachUnknown || len(got.ConnectedIn) != 0 {
		t.Fatalf("after the window: %+v", got)
	}
	r.ConnectedIn("m1", "127.0.0.1:17423")
	mu.Lock()
	members = nil // removed from every shared room
	mu.Unlock()
	if got := r.Get(); got.State != ReachUnknown || len(got.ConnectedIn) != 0 {
		t.Fatalf("a removed member's connection counted: %+v", got)
	}
}

// net check on a listener whose only member connects out: the member is
// shown as connecting in, not as a problem, and the advice counts it; the
// advice to check outgoing connections is kept for a member with a known
// address that could not be reached.
func TestNetCheckCountsAMemberThatConnectsInAndBlamesTheNetworkOnlyWhenAKnownAddressFailed(t *testing.T) {
	ci := &ConnectIn{Node: "m1", AgoS: 12}
	nc := NetCheck{State: NetworkOn, Listening: true, Listen: "127.0.0.1:17423",
		Members: []MemberCheck{{RoomLabel: "demo", Node: "m1", ConnectsIn: ci}},
		Reach:   Reach{State: ReachYes, ConnectedIn: []ConnectIn{*ci}}}
	if got := Advice(nc); got != "1 of 1 members connects in to this machine; nothing to do." {
		t.Fatalf("advice %q", got)
	}
	nc.Members[0].ConnectsIn = nil
	if got := Advice(nc); strings.Contains(got, "lets connections out") {
		t.Fatalf("no address known, none connected in: %q", got)
	}
	nc.Members[0].Addrs = []AddrCheck{{Addr: "192.0.2.7:7423", Result: ProbeTimeout}}
	if got := Advice(nc); !strings.Contains(got, "lets connections out") {
		t.Fatalf("a known address failed: %q", got)
	}
}

// Through the surface: status and net check carry the same state.
func TestStatusAndNetCheckShowTheSameConnectedInState(t *testing.T) {
	clock := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	a := newLNode(t, "a", &clock)
	r := NewReacher(&fakeDialBack{}, ReachOpts{Members: func() []string { return []string{"m1"} },
		Now: func() time.Time { return clock }, Addrs: func() []string { return []string{"127.0.0.1:17423"} }})
	s, err := OpenSurface(context.Background(), a.m, a.r, SurfaceOpts{Name: "a", Reach: r,
		Network: Network{State: NetworkOn, Listen: "127.0.0.1:17423"}})
	if err != nil {
		t.Fatal(err)
	}
	r.ConnectedIn("m1", "127.0.0.1:17423")
	if got := s.ReachNow(); got.State != ReachYes || len(got.ConnectedIn) != 1 {
		t.Fatalf("status %+v", got)
	}
	nc, err := s.NetCheck(context.Background())
	if err != nil || nc.Reach.State != ReachYes || len(nc.Reach.ConnectedIn) != 1 {
		t.Fatalf("net check %+v %v", nc.Reach, err)
	}
}
