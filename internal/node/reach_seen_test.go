package node

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// seenNet answers dial-backs as members behind which this machine is seen
// at 203.0.113.9; it reaches the addresses in open.
type seenNet struct {
	mu    sync.Mutex
	open  map[string]bool
	asked [][]string
}

func (s *seenNet) DialBack(_ context.Context, _ string, addrs []string) (*DialBackResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, slices.Clone(addrs))
	r := &DialBackResponse{V: PullVersion, Saw: "203.0.113.9"}
	for _, a := range addrs {
		if s.open[a] {
			r.Reached = append(r.Reached, a)
		}
	}
	return r, nil
}

// The address members see is a hint: it is tried in the same check, and
// advertised only if a member reached it.
func TestASeenAddressIsAdvertisedOnlyAfterAMemberReachesIt(t *testing.T) {
	for _, forwarded := range []bool{false, true} {
		n := &seenNet{open: map[string]bool{"192.168.1.5:7423": true, "203.0.113.9:7423": forwarded}}
		r := NewReacher(n, ReachOpts{Members: func() []string { return []string{"p"} }, Port: 7423,
			Addrs: func() []string { return []string{"192.168.1.5:7423"} }})
		got := r.Check(context.Background(), []string{"p"}, true)
		if len(n.asked) != 2 || !slices.Contains(n.asked[1], "203.0.113.9:7423") || slices.Contains(n.asked[0], "203.0.113.9:7423") {
			t.Fatalf("asked %v", n.asked)
		}
		if v := r.Verified(); forwarded != slices.Equal(v, []string{"203.0.113.9:7423"}) || !forwarded && len(v) != 0 {
			t.Fatalf("forwarded %v: verified %v (%+v)", forwarded, v, got)
		}
	}
}

// A private address seen (members on the same network) is not a
// candidate; a mapped address is, and is verified the same way.
func TestAMappedAddressIsVerifiedAndAPrivateSeenAddressIsNotACandidate(t *testing.T) {
	n := &seenNet{open: map[string]bool{"198.51.100.7:40001": true}}
	r := NewReacher(n, ReachOpts{Members: func() []string { return []string{"p"} }, Port: 7423,
		Addrs:      func() []string { return []string{"192.168.1.5:7423"} },
		Candidates: func() []string { return []string{"198.51.100.7:40001"} }})
	if got := r.candidates([]string{"192.168.1.9", "10.0.0.2", "fd00::1"}); !slices.Equal(got, []string{"198.51.100.7:40001"}) {
		t.Fatalf("candidates %v", got)
	}
	r.Check(context.Background(), []string{"p"}, true)
	if v := r.Verified(); !slices.Contains(v, "198.51.100.7:40001") {
		t.Fatalf("verified %v", v)
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
