package node

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/store"
)

// D68: a decision with port mapping that an earlier version recorded (its
// role lacks map_port_v2) is not applied after the upgrade, and says so;
// a decision taken now is applied, and an earlier version still reads it
// as port mapping (it looks for map_port alone).
func TestPortMappingAnEarlierVersionRecordedIsNotAppliedUntilDecidedAgain(t *testing.T) {
	clock := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	a := newLNode(t, "a", &clock)
	ctx := context.Background()
	for _, role := range []string{"ipv6 map_port", "map_port"} {
		clock = clock.Add(time.Second)
		old := store.Request{ID: "old-" + strings.ReplaceAll(role, " ", "-"), Kind: store.RequestNetwork, Action: NetworkOn,
			Name: "0.0.0.0:7423", Role: role, Created: clock, DecidedAt: clock}
		if err := a.st.RecordDecision(ctx, old); err != nil {
			t.Fatal(err)
		}
		n, err := LoadNetwork(ctx, a.st)
		if err != nil || n.MapPort || !n.MapPortEarlier || n.State != NetworkOn {
			t.Fatalf("%q: loaded %+v %v", role, n, err)
		}
	}
	s, err := OpenSurface(ctx, a.m, a.r, SurfaceOpts{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if m := s.MappingNow(); m != nil {
		t.Fatalf("mapping shown as asked for: %+v", m)
	}
	clock = clock.Add(time.Second)
	if _, err := s.SetNetwork(ctx, Network{State: NetworkOn, Listen: "0.0.0.0:7423", MapPort: true}); err != nil {
		t.Fatal(err)
	}
	n, err := LoadNetwork(ctx, a.st)
	if err != nil || !n.MapPort || n.MapPortEarlier {
		t.Fatalf("decided again: %+v %v", n, err)
	}
	q, ok, err := a.st.LatestDecision(ctx, store.RequestNetwork)
	if err != nil || !ok || !strings.Contains(" "+q.Role+" ", " map_port ") || !strings.Contains(q.Role, "map_port_v2") {
		t.Fatalf("recorded role %q %v", q.Role, err)
	}
	clock = clock.Add(time.Second)
	if _, err := s.SetNetwork(ctx, Network{State: NetworkOn, Listen: "0.0.0.0:7423"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := LoadNetwork(ctx, a.st); n.MapPort || n.MapPortEarlier {
		t.Fatalf("without --map-port: %+v", n)
	}
}
