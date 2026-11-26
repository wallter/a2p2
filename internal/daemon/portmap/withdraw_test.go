package portmap

import (
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain shortens the wait after a failed attempt for every test here:
// a first attempt lost under load (every UDP try of a loaded machine
// running the race detector) is tried again within waitFor's 5 s instead
// of in five minutes. A test that measures the wait sets it itself.
func TestMain(m *testing.M) {
	retryAfter = 100 * time.Millisecond
	os.Exit(m.Run())
}

// scripted is a Mapper whose i-th Map (from 0) answers step(i); a step
// that returns errWait waits for the end. Every Unmap succeeds.
type scripted struct {
	step  func(i int) (Mapping, error)
	calls atomic.Int32
}

var errWait = errors.New("wait for the end")

func (s *scripted) Map(ctx context.Context, internal uint16, _ time.Duration, _ *Mapping, note Note) (Mapping, error) {
	i := int(s.calls.Add(1)) - 1
	m, err := s.step(i)
	if errors.Is(err, errWait) {
		<-ctx.Done()
		return Mapping{}, ctx.Err()
	}
	return m, err
}

func (s *scripted) Unmap(context.Context, Mapping) error { return nil }

// grantOf is a PCP grant of ext for l.
func grantOf(ext string, l time.Duration) Mapping {
	return Mapping{Protocol: PCP, Gateway: loop, Local: loop, Internal: 7423, Nonce: [12]byte{1},
		External: netip.MustParseAddrPort(ext), Lifetime: l, Expires: time.Now().Add(l)}
}

var errSilent = errors.New("the router did not forward the port (PCP: no answer; UPnP: no answer)")

// states records every state a keeper publishes.
type states struct {
	mu  sync.Mutex
	all []State
}

func (s *states) add(st State) { s.mu.Lock(); s.all = append(s.all, st); s.mu.Unlock() }

func (s *states) list() []State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]State(nil), s.all...)
}

// D68, the second review: a renewal answered with an address that cannot
// be used withdraws the mapping given out, and a later renewal with no
// answer does not give it out again, while its lease still runs.
func TestAWithdrawnMappingIsNotGivenOutAgainByARenewalWithNoAnswer(t *testing.T) {
	defer func(l, w time.Duration) { Lifetime, minWait = l, w }(Lifetime, minWait)
	Lifetime, minWait = 400*time.Millisecond, time.Millisecond
	for _, unusable := range []struct {
		name string
		step func() (Mapping, error)
	}{
		// a grant of a private address, refused by the keeper
		{"private", func() (Mapping, error) { return grantOf("10.0.0.7:40001", time.Hour), nil }},
		// a grant the client could not use (success with lifetime 0)
		{"unsure", func() (Mapping, error) { return Mapping{}, unsureErr{errors.New("success with lifetime 0 s")} }},
	} {
		s := &scripted{step: func(i int) (Mapping, error) {
			switch i {
			case 0:
				return grantOf("203.0.113.7:40001", time.Hour), nil // taken as Lifetime
			case 1:
				return unusable.step()
			}
			return Mapping{}, errSilent
		}}
		var seen states
		k := NewKeeper(s, 7423, record(t), seen.add)
		stop := runKeeper(k)
		waitFor(t, unusable.name+": a renewal with no answer", func() bool { return s.calls.Load() >= 3 && len(seen.list()) >= 3 })
		stop()
		all := seen.list()
		if all[0].Mapping == nil || all[1].Mapping != nil {
			t.Fatalf("%s: grant then withdrawal: %+v", unusable.name, all[:2])
		}
		for _, st := range all[2:] {
			if st.Mapping != nil {
				t.Fatalf("%s: given out again: %+v", unusable.name, st)
			}
		}
	}
}

// The honest case of the one above: a renewal with no answer leaves the
// mapping given out until its lease runs out (a short drop of the
// network), and one usable grant after a withdrawal gives it out again.
func TestARenewalWithNoAnswerKeepsTheMappingAndAUsableGrantGivesItOutAgain(t *testing.T) {
	defer func(l, w time.Duration) { Lifetime, minWait = l, w }(Lifetime, minWait)
	Lifetime, minWait = 400*time.Millisecond, time.Millisecond
	s := &scripted{step: func(i int) (Mapping, error) {
		switch i {
		case 0, 3:
			return grantOf("203.0.113.7:40001", time.Hour), nil
		case 1:
			return Mapping{}, errSilent
		case 2:
			return grantOf("10.0.0.7:40001", time.Hour), nil
		}
		return Mapping{}, errWait
	}}
	var seen states
	k := NewKeeper(s, 7423, record(t), seen.add)
	stop := runKeeper(k)
	waitFor(t, "four attempts", func() bool { return len(seen.list()) >= 4 })
	all := seen.list()
	stop()
	if all[0].Mapping == nil || all[1].Mapping == nil || all[2].Mapping != nil || all[3].Mapping == nil {
		t.Fatalf("states %+v", all)
	}
}

// D68, the second review: once the lease of the mapping held has run out,
// a router that does not answer is asked again only retryAfter later, not
// every minWait.
func TestARouterThatStopsAnsweringIsNotAskedEveryMinWaitOnceTheLeaseRanOut(t *testing.T) {
	defer func(l, w, ra time.Duration) { Lifetime, minWait, retryAfter = l, w, ra }(Lifetime, minWait, retryAfter)
	Lifetime, minWait, retryAfter = 100*time.Millisecond, time.Millisecond, time.Hour
	s := &scripted{step: func(i int) (Mapping, error) {
		if i == 0 {
			return grantOf("203.0.113.7:40001", time.Hour), nil
		}
		return Mapping{}, errSilent
	}}
	k := NewKeeper(s, 7423, record(t), nil)
	stop := runKeeper(k)
	defer stop()
	time.Sleep(700 * time.Millisecond) // the lease ends at 100 ms
	// Before it ends the renewals halve the time left (about 7 tries from
	// 50 ms down to minWait); after it, one more and then retryAfter.
	if n := s.calls.Load(); n > 20 {
		t.Fatalf("%d attempts in 700 ms, want a few", n)
	}
}

// D68, the second review: a keeper that ends with a mapping it could not
// remove says so, even when no mapping was given out at the end (here it
// was withdrawn), so the end of a run (a2p2 stop, a2p2 net off) logs it.
func TestAKeeperThatEndsReportsWhatItCouldNotRemoveWithNoMappingGivenOut(t *testing.T) {
	defer func(l, w time.Duration) { Lifetime, minWait = l, w }(Lifetime, minWait)
	Lifetime, minWait = 2*time.Second, time.Millisecond
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, maps: map[string]uint16{}}
	gw := newFakeUDP(t, r.answer)
	var seen states
	k := NewKeeper(client(gw), 7423, record(t), seen.add)
	stop := runKeeper(k)
	waitFor(t, "the mapping", func() bool { return k.State().Mapping != nil })
	r.set(func(r *pcpRouter) { r.extIP = netip.MustParseAddr("10.0.0.7") })
	waitFor(t, "the withdrawal", func() bool { st := k.State(); return st.Mapping == nil && st.Err != "" })
	r.set(func(r *pcpRouter) { r.silentDel = true })
	n := len(seen.list())
	stop()
	all := seen.list()
	if len(all) <= n || len(all[len(all)-1].Left) != 1 || !strings.Contains(all[len(all)-1].Left[0], "could not remove it") {
		t.Fatalf("published at the end: %+v", all[n:])
	}
}

// What a record holds is shown as not removed yet from before the first
// removal is tried, and the outcome replaces it.
func TestPendingMappingsAreShownUntilTheirRemovalIsTried(t *testing.T) {
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, maps: map[string]uint16{}}
	c := client(newFakeUDP(t, r.answer))
	m, err := c.Map(context.Background(), 7423, time.Hour, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := record(t)
	if err := rec.Save([]Mapping{m}); err != nil {
		t.Fatal(err)
	}
	k := NewKeeper(c, 0, rec, nil)
	k.Pending([]Mapping{m}, "an earlier run could not remove it; trying again")
	if st := k.State(); len(st.Left) != 1 || !strings.Contains(st.Left[0], "trying again") {
		t.Fatalf("before the removal: %+v", st)
	}
	runKeeper(k)()
	if st := k.State(); len(st.Left) != 0 || r.held() != 0 || len(get(t, rec)) != 0 {
		t.Fatalf("after the removal: %+v, router holds %d", st, r.held())
	}
}

// A PCP success that cannot be used (lifetime 0) makes Map's error one
// the keeper counts as answered; a router that does not answer does not.
func TestAGrantThatCannotBeUsedCountsAsAnAnswer(t *testing.T) {
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, maps: map[string]uint16{}}
	gw := newFakeUDP(t, func(q []byte) []byte {
		b := r.answer(q)
		if len(b) >= 8 && binary.BigEndian.Uint32(q[4:]) != 0 {
			binary.BigEndian.PutUint32(b[4:], 0)
		}
		return b
	})
	if _, err := client(gw).Map(context.Background(), 7423, time.Hour, nil, nil); err == nil || !errors.Is(err, errUnsure) {
		t.Fatalf("lifetime 0: %v", err)
	}
	if _, err := client(noPCP(t)).Map(context.Background(), 7423, time.Hour, nil, nil); err == nil || errors.Is(err, errUnsure) {
		t.Fatalf("no answer: %v", err)
	}
}
