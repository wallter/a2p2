package node

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/store"
)

// Sources are grouped as the limits count them: an IPv4 address, IPv4
// mapped into IPv6 as that address, and IPv6 by its /64.
//
// Requirements: T3; PROTOCOL 5.3
func TestSourceGroupKeysIPv4ByAddressAndIPv6BySlash64(t *testing.T) {
	for in, want := range map[string]string{
		"198.51.100.7:4000":          "198.51.100.7",
		"198.51.100.7":               "198.51.100.7",
		"[::ffff:198.51.100.7]:4000": "198.51.100.7",
		"[2001:db8:1:2::1]:7423":     "2001:db8:1:2::/64",
		"[2001:db8:1:2:ffff::9]:1":   "2001:db8:1:2::/64",
		"2001:db8:1:3::1":            "2001:db8:1:3::/64",
		"[fe80::1%en0]:7423":         "fe80::/64",
		"not an address":             "not an address",
	} {
		if got := SourceGroup(in); got != want {
			t.Errorf("SourceGroup(%q) = %q, want %q", in, got, want)
		}
	}
}

// One IPv6 /64 that spends its budget of polls naming unknown invites
// refuses only itself: a joiner from another source that resumes by the
// invite's hash is still answered.
//
// Requirements: T3; PROTOCOL 5.3
func TestUnknownHashPollsFromOneSlash64DoNotRefuseOtherJoiners(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	ctx := context.Background()
	tok := a.invite(room, RoleMember, false)
	j := b.joiner(jn)
	b.preview(j, tok)
	b.accept(j, tok, 0)
	s := jn.srv[a.r]
	for i := 0; i < 5000; i++ {
		src := fmt.Sprintf("[2001:db8:1:2::%x]:7423", i+1)
		s.Handle(ctx, b.id, src, []byte(fmt.Sprintf(`{"v":2,"op":"poll","invite":"%064x"}`, i+1)))
	}
	poll := []byte(`{"v":2,"op":"poll","invite":"` + secretHash(mustToken(t, tok).Secret) + `"}`)
	if rp := s.Handle(ctx, b.id, "[2001:db8:1:2::ffff]:7423", poll); rp.Status != http.StatusTooManyRequests {
		t.Fatalf("the /64 that spent its budget: %d", rp.Status)
	}
	for _, src := range []string{"198.51.100.9:7423", "[2001:db8:9::1]:7423"} {
		if rp := s.Handle(ctx, b.id, src, poll); rp.Status != http.StatusAccepted || !strings.Contains(string(rp.Body), JoinStateAccepted) {
			t.Fatalf("an honest resume by hash from %s: %d %s", src, rp.Status, rp.Body)
		}
	}
	// An honest resume is charged nothing: one source resumes more often
	// than the budget of polls naming unknown invites.
	for i := 0; i <= unknownPolls; i++ {
		if rp := s.Handle(ctx, b.id, "198.51.100.10:7423", poll); rp.Status != http.StatusAccepted {
			t.Fatalf("resume %d by hash: %d", i, rp.Status)
		}
	}
}

// Polls naming unknown invites that arrive together from one source are
// charged before their lookups, so no more lookups run than the budget.
//
// Requirements: T3; PROTOCOL 5.3
func TestUnknownHashPollsInFlightTogetherLookUpNoMoreThanTheBudget(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	s := NewJoinServer(a.r)
	const n = 3 * unknownPolls
	var lookups atomic.Int64
	arrived := make(chan struct{})
	var once sync.Once
	get := s.getInvite
	s.getInvite = func(ctx context.Context, h string) (store.Invite, error) {
		if lookups.Add(1) == n {
			once.Do(func() { close(arrived) })
		}
		select { // hold every lookup until all are in flight, or a moment
		case <-arrived:
		case <-time.After(300 * time.Millisecond):
		}
		return get(ctx, h)
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Go(func() {
			s.Handle(context.Background(), b.id, "198.51.100.7:4000", []byte(fmt.Sprintf(`{"v":2,"op":"poll","invite":"%064x"}`, i+1)))
		})
	}
	wg.Wait()
	if got := lookups.Load(); got > unknownPolls {
		t.Fatalf("%d polls in flight together ran %d lookups, the budget is %d", n, got, unknownPolls)
	}
}
