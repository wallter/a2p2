package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/daemon/portmap"
	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// D68, the second review: port mapping that an earlier version turned on
// is not applied after the upgrade. What that version left on the router
// is removed, status says why nothing is mapped and how to map again, and
// a decision taken now maps as before.
func TestPortMappingAnEarlierVersionTurnedOnIsOffAfterTheUpgradeAndSaysSo(t *testing.T) {
	m := newMachineWith(t, "a", nil)
	r := newFakeRouter()
	m.useRouter(r, "")
	listen := m.ln.ln.Addr().String()
	m.stop()
	// What an earlier version leaves: its decision, and a mapping on the
	// router and in the record (a run that did not end cleanly).
	db, err := store.Open(filepath.Join(m.home, "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	err = db.RecordDecision(context.Background(), store.Request{ID: "earlier", Kind: store.RequestNetwork,
		Action: node.NetworkOn, Name: listen, Role: "ipv6 map_port", Created: now, DecidedAt: now})
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	left := r.mapping(7423)
	r.mu.Lock()
	r.held[left.Key()] = true
	r.mu.Unlock()
	if err := (&portmap.File{Path: filepath.Join(m.home, "portmap.json")}).Save([]portmap.Mapping{left}); err != nil {
		t.Fatal(err)
	}
	m.start()
	eventually(t, "the earlier mapping removed", func() bool { _, unmaps := r.count(); return unmaps == 1 })
	st := call[localapi.Status](m, "status", localapi.Empty{})
	if st.Network.State != node.NetworkOn || st.Network.MapPort || !st.Network.MapPortEarlier ||
		st.Mapping == nil || st.Mapping.Error != node.MapPortEarlierText || st.Mapping.External != "" {
		t.Fatalf("status %+v %+v", st.Network, st.Mapping)
	}
	time.Sleep(50 * time.Millisecond)
	if held, _ := r.count(); held != 0 {
		t.Fatal("mapped under a decision of an earlier version")
	}
	// The person decides again.
	st = m.decide(localapi.NetReq{State: node.NetworkOn, Listen: listen, MapPort: true})
	if !st.Network.MapPort || st.Network.MapPortEarlier {
		t.Fatalf("decided again %+v", st.Network)
	}
	eventually(t, "mapped once decided again", func() bool { held, _ := r.count(); return held == 1 })
}

// slowFail is a router whose removals, while failing is set, get no
// answer until the caller gives up.
type slowFail struct {
	*fakeRouter
	failing atomic.Bool
}

func (s *slowFail) Unmap(ctx context.Context, m portmap.Mapping) error {
	if s.failing.Load() {
		<-ctx.Done()
		return errors.New("no answer")
	}
	return s.fakeRouter.Unmap(ctx, m)
}

// D68, the second review: a removal that fails at net off is tried at
// the end of the run and again by the next one, and the status that net
// off shows when the decision is applied already names it; a stop tries
// once more.
func TestARemovalThatFailsAtNetOffIsShownAtOnceAndTriedAgainAtStop(t *testing.T) {
	m := newMachineWith(t, "a", nil)
	r := &slowFail{fakeRouter: newFakeRouter()}
	m.useRouter(r, "")
	m.decide(localapi.NetReq{State: node.NetworkOn, Listen: m.ln.ln.Addr().String(), MapPort: true})
	eventually(t, "mapped", func() bool { held, _ := r.count(); return held == 1 })
	r.failing.Store(true)
	st := m.setNet(node.NetworkOff) // what a2p2 net off shows once applied
	if st.Mapping == nil || len(st.Mapping.NotRemoved) != 1 || !strings.Contains(st.Mapping.NotRemoved[0], "198.51.100.7:40001") {
		t.Fatalf("after net off: %+v", st.Mapping)
	}
	eventually(t, "the failed retry in status", func() bool {
		st := call[localapi.Status](m, "status", localapi.Empty{})
		return st.Mapping != nil && len(st.Mapping.NotRemoved) == 1 &&
			strings.Contains(st.Mapping.NotRemoved[0], "could not remove it: no answer")
	})
	r.failing.Store(false)
	m.stop()
	if held, _ := r.count(); held != 0 {
		t.Fatal("not removed at stop")
	}
}

// The honest case: a removal that works at net off leaves nothing to show.
func TestARemovalThatWorksAtNetOffLeavesNothingInStatus(t *testing.T) {
	m := newMachineWith(t, "a", nil)
	r := newFakeRouter()
	m.useRouter(r, "")
	m.decide(localapi.NetReq{State: node.NetworkOn, Listen: m.ln.ln.Addr().String(), MapPort: true})
	eventually(t, "mapped", func() bool { held, _ := r.count(); return held == 1 })
	if st := m.setNet(node.NetworkOff); st.Mapping != nil {
		t.Fatalf("after net off: %+v", st.Mapping)
	}
	if held, _ := r.count(); held != 0 {
		t.Fatal("not removed at net off")
	}
}

// A record left when no router can be found now: status names what may
// still be on the router, not only that there is no router.
func TestAMappingLeftWhenNoRouterIsFoundIsNamedInStatus(t *testing.T) {
	m := newMachineWith(t, "a", nil)
	left := newFakeRouter().mapping(7423)
	if err := (&portmap.File{Path: filepath.Join(m.home, "portmap.json")}).Save([]portmap.Mapping{left}); err != nil {
		t.Fatal(err)
	}
	m.stop()
	m.start() // no router in tests
	st := call[localapi.Status](m, "status", localapi.Empty{})
	if st.Mapping == nil || !strings.Contains(st.Mapping.Error, "no router in tests") ||
		!strings.Contains(st.Mapping.Error, "198.51.100.7:40001") || !strings.Contains(st.Mapping.Error, "may still be on the router") {
		t.Fatalf("mapping %+v", st.Mapping)
	}
}
