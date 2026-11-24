package node

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/store"
)

// liar answers every dial-back as a lying member would: it says it saw
// this machine at saw and reached every address asked.
type liar struct {
	mu    sync.Mutex
	saw   string
	asked [][]string
	open  map[string]bool // nil: claims every address
}

func (l *liar) DialBack(_ context.Context, _ string, addrs []string) (*DialBackResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked = append(l.asked, slices.Clone(addrs))
	r := &DialBackResponse{V: PullVersion, Saw: l.saw}
	for _, a := range addrs {
		if l.open == nil || l.open[a] {
			r.Reached = append(r.Reached, a)
		}
	}
	return r, nil
}

// One lying member cannot make this machine publish someone else's
// address: what it says it saw is shown and never asked about, verified
// or advertised.
func TestWhatAMemberSaysItSawIsShownAndNeverAdvertised(t *testing.T) {
	l := &liar{saw: "198.51.100.66"}
	r := NewReacher(l, ReachOpts{Members: func() []string { return []string{"p"} },
		Addrs: func() []string { return []string{"192.168.1.5:7423"} }})
	for range 3 {
		r.Check(context.Background(), []string{"p"}, true)
	}
	got := r.Get()
	for _, a := range l.asked {
		if !slices.Equal(a, []string{"192.168.1.5:7423"}) {
			t.Fatalf("asked about %v", a)
		}
	}
	if len(r.Verified()) != 0 || len(got.Verified) != 0 || !slices.Equal(got.Saw, []string{"198.51.100.66"}) {
		t.Fatalf("verified %v, reach %+v", r.Verified(), got)
	}
}

// A mapped address is advertised only while a member has reached it
// within VerifiedFor, while it is still the mapping, and until members
// try it and fail.
func TestAMappedAddressIsAdvertisedOnlyWhileRecentlyReachedAndStillMapped(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	mapped := []string{"198.51.100.7:40001"}
	var mu sync.Mutex
	cands := func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(mapped) }
	l := &liar{open: map[string]bool{"198.51.100.7:40001": true}}
	r := NewReacher(l, ReachOpts{Members: func() []string { return []string{"p"} }, Now: func() time.Time { return now },
		Addrs: func() []string { return []string{"192.168.1.5:7423"} }, Candidates: cands})
	r.Check(context.Background(), []string{"p"}, true)
	if !slices.Equal(r.Verified(), mapped) || !slices.Equal(l.asked[0], []string{"198.51.100.7:40001", "192.168.1.5:7423"}) {
		t.Fatalf("verified %v, asked %v", r.Verified(), l.asked)
	}
	// No member confirms it for VerifiedFor (nobody answers): withdrawn.
	now = now.Add(VerifiedFor)
	if len(r.Verified()) != 0 {
		t.Fatalf("still advertised after %s: %v", VerifiedFor, r.Verified())
	}
	r.Check(context.Background(), []string{"p"}, true)
	if !slices.Equal(r.Verified(), mapped) {
		t.Fatalf("confirmed again: %v", r.Verified())
	}
	// The mapping is gone: withdrawn at once.
	mu.Lock()
	mapped = nil
	mu.Unlock()
	if len(r.Verified()) != 0 {
		t.Fatalf("advertised without a mapping: %v", r.Verified())
	}
	// Mapped again, and members try it and fail: withdrawn.
	mu.Lock()
	mapped = []string{"198.51.100.7:40001"}
	mu.Unlock()
	l.mu.Lock()
	l.open = map[string]bool{}
	l.mu.Unlock()
	r.Check(context.Background(), []string{"p"}, true)
	if len(r.Verified()) != 0 {
		t.Fatalf("advertised after members failed to reach it: %v", r.Verified())
	}
}

// Members that answer but would try none of the addresses (not their
// source and not in their book) leave the answer unknown, saying why.
func TestReachIsUnknownWhenTheMembersThatAnsweredTriedNothing(t *testing.T) {
	f := &fakeDialBack{resp: map[string]*DialBackResponse{"p": {V: PullVersion, NotTried: []string{"192.168.1.5:7423"}}}}
	r := NewReacher(f, ReachOpts{Members: func() []string { return []string{"p"} },
		Addrs: func() []string { return []string{"192.168.1.5:7423"} }})
	got := r.Check(context.Background(), []string{"p"}, true)
	if got.State != ReachUnknown || !strings.Contains(got.Why, NotTriedText) || !slices.Equal(got.NotTried, []string{"192.168.1.5:7423"}) {
		t.Fatalf("%+v", got)
	}
}

func TestPortMappingIsRecordedInTheNetworkingDecisionAndNeedsAListener(t *testing.T) {
	clock := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	a := newLNode(t, "a", &clock)
	s, err := OpenSurface(context.Background(), a.m, a.r, SurfaceOpts{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetNetwork(context.Background(), Network{State: NetworkOutbound, MapPort: true}); err == nil {
		t.Fatal("port mapping accepted with outbound only")
	}
	if _, err := s.SetNetwork(context.Background(), Network{State: NetworkOn, Listen: "0.0.0.0:7423", MapPort: true}); err != nil {
		t.Fatal(err)
	}
	n, err := LoadNetwork(context.Background(), a.st)
	if err != nil || !n.MapPort || n.State != NetworkOn || !strings.Contains(NetworkText(n), "forward the listening port") {
		t.Fatalf("loaded %+v %v", n, err)
	}
	if _, err := s.SetNetwork(context.Background(), Network{State: NetworkOn, Listen: "0.0.0.0:7423"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := LoadNetwork(context.Background(), a.st); n.MapPort {
		t.Fatal("a later decision without --map-port kept it")
	}
}

// A decision recorded before this version (its role lacks "ipv6") stays
// IPv4 only; any decision taken now covers IPv6, and says so.
func TestANetworkingDecisionFromBeforeIPv6StaysIPv4OnlyUntilDecidedAgain(t *testing.T) {
	clock := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	a := newLNode(t, "a", &clock)
	old := store.Request{ID: "old-decision", Kind: store.RequestNetwork, Action: NetworkOn, Name: "0.0.0.0:7423",
		Created: clock, DecidedAt: clock}
	if err := a.st.RecordDecision(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	n, err := LoadNetwork(context.Background(), a.st)
	if err != nil || !n.IPv4Only || !strings.Contains(NetworkText(n), "IPv4 only") {
		t.Fatalf("old decision %+v %v: %s", n, err, NetworkText(n))
	}
	s, err := OpenSurface(context.Background(), a.m, a.r, SurfaceOpts{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Second)
	if _, err := s.SetNetwork(context.Background(), Network{State: NetworkOn, Listen: "0.0.0.0:7423"}); err != nil {
		t.Fatal(err)
	}
	n, _ = LoadNetwork(context.Background(), a.st)
	if n.IPv4Only || !strings.Contains(NetworkText(n), "IPv4 and IPv6") {
		t.Fatalf("new decision %+v: %s", n, NetworkText(n))
	}
}

func TestNetCheckSaysWhenTheRouterIsBehindAnotherNAT(t *testing.T) {
	nc := NetCheck{State: NetworkOn, Listening: true, Members: []MemberCheck{{RoomLabel: "team", Node: "n", Reached: true}},
		Reach:   Reach{State: ReachNo, Saw: []string{"100.64.3.2"}},
		Mapping: &PortMapping{Protocol: "pcp", External: "192.0.2.10:7423"}}
	if got := Advice(nc); !strings.Contains(got, "behind another NAT") {
		t.Fatalf("advice %q", got)
	}
	nc.Reach.Saw = []string{"192.0.2.10"}
	nc.Mapping = &PortMapping{Error: "the router did not forward the port (PCP: no answer)"}
	if got := Advice(nc); !strings.Contains(got, "did not forward the port") {
		t.Fatalf("advice %q", got)
	}
}

// Members see this machine at a public address it does not advertise and
// no mapping was asked for: net check prints the config line for a port
// forwarded by hand, and advertises nothing by itself.
func TestNetCheckGivesTheAdvertiseLineForAPortForwardedByHand(t *testing.T) {
	nc := NetCheck{State: NetworkOn, Listening: true, Listen: "0.0.0.0:7423", Advertised: []string{"192.168.1.5:7423"},
		Members: []MemberCheck{{RoomLabel: "team", Node: "n", Reached: true}},
		Reach:   Reach{State: ReachNo, Saw: []string{"192.168.1.9", "203.0.113.9"}}}
	nc.Forward = forwardLine(nc)
	if nc.Forward != `"advertise": ["203.0.113.9:7423", "192.168.1.5:7423"]` {
		t.Fatalf("forward %q", nc.Forward)
	}
	if got := Advice(nc); !strings.Contains(got, nc.Forward) {
		t.Fatalf("advice %q", got)
	}
	nc.Reach.Saw = []string{"100.64.3.2"} // carrier-grade NAT: forwarding cannot help
	if got := forwardLine(nc); got != "" {
		t.Fatalf("forward behind CGNAT %q", got)
	}
}
