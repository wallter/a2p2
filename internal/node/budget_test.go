package node

// The requester's budget alone (budget.go): admission is a slot, then a
// token; nothing is charged for a request that never went out; a bucket in
// use is never dropped; past the cap, peers share one overflow bucket; and
// requests are admitted in the order asked. In synctest bubbles: time is
// fake and synctest.Wait tells when every goroutine is blocked.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// state is a bucket's tokens (refilled to now), slots in use and tokens
// taken and kept.
func (b *pullBudget) state(peer string) (tokens float64, slots int, taken int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := b.m[peer]
	if k == nil {
		return b.burst, 0, 0
	}
	return min(b.burst, k.tokens+time.Since(k.last).Seconds()*b.rate), len(k.slots), k.taken
}

func (b *pullBudget) users(peer string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if k := b.m[peer]; k != nil {
		return k.users
	}
	return 0
}

// admitting starts an admission and returns its outcome when it ends.
func admitting(b *pullBudget, ctx context.Context, peer string, stop <-chan struct{}) <-chan admission {
	ch := make(chan admission, 1)
	go func() {
		t, err := b.admit(ctx, peer, stop)
		ch <- admission{t, err}
	}()
	return ch
}

type admission struct {
	t   *pullTicket
	err error
}

func wantState(t *testing.T, b *pullBudget, peer string, tokens float64, slots int, taken int64) {
	t.Helper()
	gt, gs, gk := b.state(peer)
	if gt < tokens-1e-6 || gt > tokens+1e-6 || gs != slots || gk != taken {
		t.Fatalf("%s: tokens %.3f slots %d taken %d; want %.3f, %d, %d", peer, gt, gs, gk, tokens, slots, taken)
	}
}

// Cancelled while waiting for a slot, while waiting for its token (by the
// context or by stop), or after admission and before it is sent: every
// time the token goes back, the slot is free, and nothing is counted
// taken; a sent request keeps its token and frees its slot once.
//
// Requirements: N1
func TestABudgetChargesNothingForARequestCancelledAtAnyPoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newPullBudget(PullBudgetRate, PullBudgetBurst)
		bg := context.Background()
		// Hold the 3 slots.
		var held []*pullTicket
		for range PullBudgetSlots {
			tk, err := b.admit(bg, "p", nil)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, tk)
		}
		wantState(t, b, "p", PullBudgetBurst-3, 3, 3)

		// 1. Waiting for a slot: cancelled by ctx, and by stop.
		ctx, cancel := context.WithCancel(bg)
		c1 := admitting(b, ctx, "p", nil)
		stop := make(chan struct{})
		c2 := admitting(b, bg, "p", stop)
		synctest.Wait()
		wantState(t, b, "p", PullBudgetBurst-3, 3, 3) // queued for a slot: no token
		cancel()
		close(stop)
		if r := <-c1; !errors.Is(r.err, context.Canceled) {
			t.Fatalf("slot wait cancelled: %v", r.err)
		}
		if r := <-c2; !errors.Is(r.err, ErrWaitStopped) {
			t.Fatalf("slot wait stopped: %v", r.err)
		}
		wantState(t, b, "p", PullBudgetBurst-3, 3, 3)

		// 2. A request sent and answered keeps its token, its slot freed;
		// done twice frees it once.
		held[0].done()
		held[0].done()
		wantState(t, b, "p", PullBudgetBurst-3, 2, 3)
		held[1].cancel() // 3. admitted, never sent: token and slot back
		wantState(t, b, "p", PullBudgetBurst-2, 1, 2)
		held[2].done()

		// 4. Waiting for a token (the bucket empty): by ctx, then by stop.
		for range int(PullBudgetBurst) - 2 {
			tk, err := b.admit(bg, "p", nil)
			if err != nil {
				t.Fatal(err)
			}
			tk.done()
		}
		wantState(t, b, "p", 0, 0, int64(PullBudgetBurst))
		ctx, cancel = context.WithCancel(bg)
		c3 := admitting(b, ctx, "p", nil)
		synctest.Wait()
		if _, s, _ := b.state("p"); s != 1 {
			t.Fatalf("waiting for its token, slots %d, want 1", s)
		}
		cancel()
		if r := <-c3; !errors.Is(r.err, context.Canceled) {
			t.Fatalf("token wait cancelled: %v", r.err)
		}
		stop = make(chan struct{})
		c4 := admitting(b, bg, "p", stop)
		synctest.Wait()
		close(stop)
		if r := <-c4; !errors.Is(r.err, ErrWaitStopped) {
			t.Fatalf("token wait stopped: %v", r.err)
		}
		wantState(t, b, "p", 0, 0, int64(PullBudgetBurst))
		if u := b.users("p"); u != 0 {
			t.Fatalf("users %d after every admission ended", u)
		}

		// 5. The bucket refills: one token after 1/rate.
		time.Sleep(tokenGap)
		tk, err := b.admit(bg, "p", nil)
		if err != nil {
			t.Fatal(err)
		}
		tk.done()
		wantState(t, b, "p", 0, 0, int64(PullBudgetBurst)+1)
	})
}

// Requests queued behind the 3 slots hold no token while they wait, so
// when the slots free after a long stall they do not leave together: in
// any minute at most burst + 60·rate are admitted, and at most 3 are in
// flight at once.
//
// Requirements: N1
func TestRequestsQueuedForASlotHoldNoTokenAndCannotLeaveTogether(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newPullBudget(PullBudgetRate, PullBudgetBurst)
		bg := context.Background()
		var mu sync.Mutex
		var at []time.Time
		inflight, peak := 0, 0
		start := time.Now()
		stall := true
		var wg sync.WaitGroup
		for i := range 200 {
			wg.Go(func() {
				for time.Since(start) < 2*time.Minute {
					tk, err := b.admit(bg, "p", nil)
					if err != nil {
						t.Error(err)
						return
					}
					mu.Lock()
					at = append(at, time.Now())
					inflight++
					peak = max(peak, inflight)
					s := stall
					mu.Unlock()
					if s {
						time.Sleep(10 * time.Second) // the peer hangs
					} else {
						time.Sleep(time.Duration(i%7) * time.Millisecond)
					}
					mu.Lock()
					inflight--
					mu.Unlock()
					tk.done()
				}
			})
		}
		time.Sleep(5 * time.Second)
		mu.Lock()
		stall = false
		mu.Unlock()
		wg.Wait()
		ceiling := int(PullBudgetBurst + 60*PullBudgetRate)
		worst := 0
		for i := range at {
			n := 0
			for j := i; j < len(at) && at[j].Sub(at[i]) < time.Minute; j++ {
				n++
			}
			worst = max(worst, n)
		}
		// In any second, at most burst + rate (before, the requests queued
		// through the stall had paid for 180 tokens and left at once).
		burst := 0
		for i := range at {
			n := 0
			for j := i; j < len(at) && at[j].Sub(at[i]) < time.Second; j++ {
				n++
			}
			burst = max(burst, n)
		}
		t.Logf("admitted %d; most in a minute %d (ceiling %d); most at once %d; most in a second %d",
			len(at), worst, ceiling, peak, burst)
		if worst > ceiling {
			t.Fatalf("%d admitted in a minute, ceiling %d", worst, ceiling)
		}
		if peak > PullBudgetSlots {
			t.Fatalf("%d in flight at once", peak)
		}
		if burst > int(PullBudgetBurst+PullBudgetRate) {
			t.Fatalf("%d admitted as the stall ended: queued requests left together", burst)
		}
		if _, _, taken := b.state("p"); taken != int64(len(at)) {
			t.Fatalf("taken %d, admitted %d", taken, len(at))
		}
	})
}

// A waiting pull keeps its slot until it is admitted (answered) or
// pullAdmitAfter has passed, whichever is first.
//
// Requirements: N1
func TestAWaitingPullsSlotIsFreedAfterTheAdmissionIntervalOrItsAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newPullBudget(PullBudgetRate, PullBudgetBurst)
		bg := context.Background()
		tk, _ := b.admit(bg, "p", nil)
		tk.admitAfter(pullAdmitAfter)
		time.Sleep(pullAdmitAfter - time.Millisecond)
		wantState(t, b, "p", PullBudgetBurst, 1, 1)
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if _, s, _ := b.state("p"); s != 0 {
			t.Fatalf("slots %d after the admission interval", s)
		}
		tk.done() // answered later: nothing freed twice
		if _, s, _ := b.state("p"); s != 0 {
			t.Fatalf("slots %d", s)
		}
		tk2, _ := b.admit(bg, "p", nil)
		tk2.admitAfter(pullAdmitAfter)
		tk2.done() // answered first
		time.Sleep(2 * pullAdmitAfter)
		if _, s, _ := b.state("p"); s != 0 {
			t.Fatalf("slots %d", s)
		}
	})
}

// With pullBudgetKeys buckets in use (slots held), a new peer neither
// drops one of them nor gets a fresh bucket of its own: peers past the cap
// share one overflow bucket, its 3 slots and its tokens. Once a bucket is
// full and unused it makes room for a new peer.
//
// Requirements: N1
func TestABucketInUseIsNeverDroppedAndPeersPastTheCapShareOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newPullBudget(PullBudgetRate, PullBudgetBurst)
		bg := context.Background()
		var held []*pullTicket
		for i := range pullBudgetKeys {
			tk, err := b.admit(bg, fmt.Sprintf("p%04d", i), nil)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, tk)
		}
		time.Sleep(time.Hour) // every bucket full of tokens again, its slot still held
		var over []*pullTicket
		for i := range PullBudgetSlots {
			tk, err := b.admit(bg, fmt.Sprintf("q%d", i), nil)
			if err != nil {
				t.Fatal(err)
			}
			over = append(over, tk)
		}
		if len(b.m) != pullBudgetKeys {
			t.Fatalf("%d buckets, cap %d", len(b.m), pullBudgetKeys)
		}
		for i := range pullBudgetKeys {
			if _, s, _ := b.state(fmt.Sprintf("p%04d", i)); s != 1 {
				t.Fatalf("p%04d dropped, or its slot lost, while in use", i)
			}
		}
		// A fourth peer past the cap waits: the overflow bucket's 3 slots
		// are held by the other three.
		ctx, cancel := context.WithCancel(bg)
		c := admitting(b, ctx, "q9", nil)
		synctest.Wait()
		select {
		case r := <-c:
			t.Fatalf("a 4th request in flight through the overflow bucket: %v", r.err)
		default:
		}
		cancel()
		<-c
		if b.overflow.taken != int64(PullBudgetSlots) {
			t.Fatalf("overflow taken %d", b.overflow.taken)
		}
		// One bucket ends its use and is full: a new peer takes its place.
		held[7].done()
		tk, err := b.admit(bg, "r", nil)
		if err != nil {
			t.Fatal(err)
		}
		if b.m["r"] == nil || b.m["p0007"] != nil || len(b.m) != pullBudgetKeys {
			t.Fatalf("the unused full bucket made no room: r %v, p0007 %v, %d buckets", b.m["r"] != nil, b.m["p0007"] != nil, len(b.m))
		}
		tk.done()
		for _, o := range over {
			o.done()
		}
		for _, h := range held {
			h.done()
		}
	})
}

// Bounded unfairness: one room keeps the peer's slots and tokens busy
// (16 requests always queued); a quiet room's request, asked later, is
// admitted after at most the requests queued before it, and waits at most
// what they cost (about 17 tokens at 15 a second), not for the busy room
// to stop.
//
// Requirements: N1
func TestAQuietRoomsRequestGetsItsTurnWhileABusyRoomIsPaced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newPullBudget(PullBudgetRate, PullBudgetBurst)
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		var mu sync.Mutex
		busy := 0
		for range 16 {
			wg.Go(func() {
				for {
					tk, err := b.admit(ctx, "p", nil)
					if err != nil {
						return
					}
					mu.Lock()
					busy++
					mu.Unlock()
					time.Sleep(5 * time.Millisecond)
					tk.done()
				}
			})
		}
		time.Sleep(10 * time.Second) // the bucket long empty: every request paced
		mu.Lock()
		before := busy
		mu.Unlock()
		asked := time.Now()
		tk, err := b.admit(context.Background(), "p", nil)
		if err != nil {
			t.Fatal(err)
		}
		waited := time.Since(asked)
		mu.Lock()
		ahead := busy - before
		mu.Unlock()
		tk.done()
		cancel()
		wg.Wait()
		limit := 17 * tokenGap
		t.Logf("the quiet room waited %v behind %d of the busy room's requests", waited, ahead)
		if ahead > 16 || waited > limit {
			t.Fatalf("waited %v behind %d requests; bound 16 requests, %v", waited, ahead, limit)
		}
	})
}
