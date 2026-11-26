package node

import (
	"context"
	"sync"
	"time"
)

// The requester's budget (PROTOCOL 4.6, DESIGN 4): every pull request this
// machine sends a peer, for every room and both anchors, opening,
// continuing, closing or waiting, first takes a token of that peer's
// bucket. It refills PullBudgetRate a second up to PullBudgetBurst, so at
// most PullBudgetBurst + 60·PullBudgetRate = 930 requests reach one peer in
// any minute, 900 a minute sustained. That is three quarters of the
// listener's limit per peer (internal/peer: 20 a second, burst 40, the
// same in every version), a 25 % margin on rate and burst, so an honest
// requester draws no 429 for its rate, whatever its rooms and their
// changes. The burst covers a whole exchange of one message after idle
// (an opening with the offer, the closing opening) and the next waiting
// pull many times over; under sustained writing a request waits at most
// 1/PullBudgetRate (about 67 ms) for its token once the burst is spent,
// and writes made meanwhile go in the same request.
//
// And at most PullBudgetSlots requests that do not wait are in flight to
// one peer at once, below the listener's 4 concurrent requests per peer
// (a request counts there until it enters a hold): rooms starting
// exchanges together queue for a slot instead of drawing a 429.
const (
	PullBudgetRate  = 15.0
	PullBudgetBurst = 30.0
	PullBudgetSlots = 3
	// pullMaxRequests bounds the requests of one exchange (Exchange.Pull):
	// a catch-up of 16,000 log entries or 32,000 records at the page
	// limits fits in one; a longer one ends Capped and the next exchange
	// goes on from where it stopped, paid for by the budget.
	pullMaxRequests = 64
	// pullBudgetKeys bounds the buckets kept; full ones are dropped first.
	pullBudgetKeys = 1024
)

// pullBudget is the token buckets of the peers this machine pulls from.
// Tokens are reserved in the order asked (a reservation may leave the
// bucket below zero and waits until it is paid), so the rooms and anchors
// that share a peer take turns.
type pullBudget struct {
	mu          sync.Mutex
	rate, burst float64
	m           map[string]*budgetBucket
}

type budgetBucket struct {
	slots  chan struct{} // requests in flight that do not wait (enter)
	tokens float64
	last   time.Time
	taken  int64 // tokens taken and kept, for tests
}

func newPullBudget(rate, burst float64) *pullBudget {
	return &pullBudget{rate: rate, burst: burst, m: map[string]*budgetBucket{}}
}

// reserve takes a token of peer's bucket and says how long to wait before
// using it.
func (b *pullBudget) reserve(peer string) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	k := b.bucket(peer, now)
	k.tokens = min(b.burst, k.tokens+now.Sub(k.last).Seconds()*b.rate)
	k.last = now
	k.tokens--
	k.taken++
	if k.tokens >= 0 {
		return 0
	}
	return time.Duration(-k.tokens / b.rate * float64(time.Second))
}

// bucket is peer's bucket, made full if there is none; b.mu is held.
// Buckets that are full again are dropped first when there are too many
// (a request in flight keeps its own slots channel).
func (b *pullBudget) bucket(peer string, now time.Time) *budgetBucket {
	k := b.m[peer]
	if k == nil {
		if len(b.m) >= pullBudgetKeys {
			for id, o := range b.m {
				if o.tokens+now.Sub(o.last).Seconds()*b.rate >= b.burst {
					delete(b.m, id)
				}
			}
		}
		k = &budgetBucket{tokens: b.burst, last: now, slots: make(chan struct{}, PullBudgetSlots)}
		b.m[peer] = k
	}
	return k
}

// refund gives back a token reserved and not used.
func (b *pullBudget) refund(peer string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if k := b.m[peer]; k != nil {
		k.tokens = min(b.burst, k.tokens+1)
		k.taken--
	}
}

// takenBy is the tokens of peer's bucket taken and kept.
func (b *pullBudget) takenBy(peer string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if k := b.m[peer]; k != nil {
		return k.taken
	}
	return 0
}

// take waits for a token of peer's bucket. Waiting ends with ctx (the
// room left, the daemon stopping), or with ErrWaitStopped if stop closes
// first; either way the token goes back.
func (b *pullBudget) take(ctx context.Context, peer string, stop <-chan struct{}) error {
	d := b.reserve(peer)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		b.refund(peer)
		return ctx.Err()
	case <-stop:
		b.refund(peer)
		return ErrWaitStopped
	}
}

// enter waits for a slot of peer's requests in flight, for a request that
// does not wait; the release it returns gives the slot back. Waiting ends
// with ctx.
func (b *pullBudget) enter(ctx context.Context, peer string) (func(), error) {
	b.mu.Lock()
	slots := b.bucket(peer, time.Now()).slots
	b.mu.Unlock()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
