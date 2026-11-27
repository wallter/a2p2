package node

import (
	"context"
	"sync"
	"time"
)

// The requester's budget (PROTOCOL 4.6, DESIGN 4). Every pull request this
// machine sends a peer, for every room and both anchors, opening,
// continuing, closing or waiting, is admitted in this order: a slot of the
// peer's requests in flight, then a token of its bucket, then it is
// prepared (membership checked again, writes made meanwhile included) and
// sent. Each further address the transport tries takes a token too
// (PullAttempt).
//
// The bucket refills PullBudgetRate a second up to PullBudgetBurst, so at
// most PullBudgetBurst + 60·PullBudgetRate = 930 requests reach one peer in
// any minute, 900 a minute sustained, from one run of the daemon (a
// restart or a networking change starts a full bucket). That is three
// quarters of the listener's limit per peer (internal/peer: 20 a second,
// burst 40, the same in every version), a 25 % margin on rate and burst,
// so an honest requester draws no 429 for its rate. Only a request that
// holds a slot reserves a token, so requests queued for a slot hold none
// and cannot leave together when slots free.
//
// At most PullBudgetSlots requests to one peer are in flight and not yet
// admitted at once, below the listener's 4 concurrent requests per peer (a
// request counts there until it is answered or enters a hold). A request
// that does not wait keeps its slot until its answer arrives; a waiting
// pull until its answer arrives or pullAdmitAfter has passed, by when the
// listener has held it. Slots and tokens are handed out in the order
// asked (a Go channel's senders and the reservations queue in order), so a
// room waits behind at most the requests already queued, two per room
// sharing the peer (one per anchor, an exchange sending one at a time).
const (
	PullBudgetRate  = 15.0
	PullBudgetBurst = 30.0
	PullBudgetSlots = 3
	// pullAdmitAfter is how long a waiting pull keeps its slot at most.
	pullAdmitAfter = time.Second
	// pullMaxRequests bounds the requests of one exchange (Exchange.Pull):
	// a catch-up of 16,000 log entries or 32,000 records at the page
	// limits fits in one; a longer one ends Capped and the next exchange
	// goes on from where it stopped, paid for by the budget.
	pullMaxRequests = 64
	// pullBudgetKeys bounds the buckets kept. Buckets are made only for
	// peers this machine pulls from (members of rooms it holds); one full
	// and unused is dropped to make room, and with none such, peers beyond
	// the cap share one overflow bucket, its tokens and its slots.
	pullBudgetKeys = 1024
)

// pullBudget is the buckets of the peers this machine pulls from.
type pullBudget struct {
	mu          sync.Mutex
	rate, burst float64
	m           map[string]*budgetBucket
	overflow    *budgetBucket
}

type budgetBucket struct {
	slots  chan struct{} // requests in flight and not yet admitted
	tokens float64       // below zero: reservations not yet paid
	last   time.Time
	users  int   // admissions under way or in flight: never dropped while above 0
	taken  int64 // tokens taken and kept, for tests
}

func newPullBudget(rate, burst float64) *pullBudget {
	return &pullBudget{rate: rate, burst: burst, m: map[string]*budgetBucket{}}
}

func (b *pullBudget) fresh(now time.Time) *budgetBucket {
	return &budgetBucket{tokens: b.burst, last: now, slots: make(chan struct{}, PullBudgetSlots)}
}

// use is peer's bucket, counted as in use until done; b.mu is held.
func (b *pullBudget) use(peer string, now time.Time) *budgetBucket {
	k := b.m[peer]
	if k == nil {
		if len(b.m) >= pullBudgetKeys {
			for id, o := range b.m {
				if o.users == 0 && len(o.slots) == 0 && o.tokens+now.Sub(o.last).Seconds()*b.rate >= b.burst {
					delete(b.m, id)
					break
				}
			}
		}
		if len(b.m) >= pullBudgetKeys {
			if b.overflow == nil {
				b.overflow = b.fresh(now)
			}
			k = b.overflow
		} else {
			k = b.fresh(now)
			b.m[peer] = k
		}
	}
	k.users++
	return k
}

// reserve takes a token of k and says how long to wait before using it;
// b.mu is held.
func (b *pullBudget) reserve(k *budgetBucket, now time.Time) time.Duration {
	k.tokens = min(b.burst, k.tokens+now.Sub(k.last).Seconds()*b.rate)
	k.last = now
	k.tokens--
	k.taken++
	if k.tokens >= 0 {
		return 0
	}
	return time.Duration(-k.tokens / b.rate * float64(time.Second))
}

// refund gives back a token of k reserved and not used; b.mu is held.
func (b *pullBudget) refund(k *budgetBucket) {
	k.tokens = min(b.burst, k.tokens+1)
	k.taken--
}

// pay waits d for a token reserved of k; a cancellation refunds it.
func (b *pullBudget) pay(ctx context.Context, k *budgetBucket, d time.Duration, stop <-chan struct{}) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	var err error
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		err = ctx.Err()
	case <-stop:
		err = ErrWaitStopped
	}
	b.mu.Lock()
	b.refund(k)
	b.mu.Unlock()
	return err
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

// RequestsCharged is the tokens of peer's budget this exchange has taken
// and kept: one for each request or wire attempt that went out (for tests
// and diagnostics).
func (x *Exchange) RequestsCharged(peer string) int64 { return x.budget.takenBy(peer) }

// pullTicket is one admitted request: a slot and a token of its peer.
type pullTicket struct {
	b       *pullBudget
	k       *budgetBucket
	release sync.Once
	ended   sync.Once
}

// admit waits for a slot of peer's requests in flight, then for a token.
// Waiting ends with ctx (the room left, the daemon stopping), or with
// ErrWaitStopped if stop closes first; either way nothing is charged.
func (b *pullBudget) admit(ctx context.Context, peer string, stop <-chan struct{}) (*pullTicket, error) {
	b.mu.Lock()
	k := b.use(peer, time.Now())
	b.mu.Unlock()
	t := &pullTicket{b: b, k: k}
	select {
	case k.slots <- struct{}{}:
	case <-ctx.Done():
		t.end()
		return nil, ctx.Err()
	case <-stop:
		t.end()
		return nil, ErrWaitStopped
	}
	b.mu.Lock()
	d := b.reserve(k, time.Now())
	b.mu.Unlock()
	if err := b.pay(ctx, k, d, stop); err != nil {
		t.admitted()
		t.end()
		return nil, err
	}
	return t, nil
}

// admitted gives the slot back (once).
func (t *pullTicket) admitted() { t.release.Do(func() { <-t.k.slots }) }

// admitAfter gives the slot back after d, unless admitted first.
func (t *pullTicket) admitAfter(d time.Duration) {
	time.AfterFunc(d, t.admitted)
}

// cancel undoes a ticket whose request was never sent: the token goes
// back, and the slot.
func (t *pullTicket) cancel() {
	t.b.mu.Lock()
	t.b.refund(t.k)
	t.b.mu.Unlock()
	t.admitted()
	t.end()
}

// done ends a ticket whose request was sent and answered or failed.
func (t *pullTicket) done() {
	t.admitted()
	t.end()
}

// end stops counting the bucket as in use (once).
func (t *pullTicket) end() {
	t.ended.Do(func() {
		t.b.mu.Lock()
		t.k.users--
		t.b.mu.Unlock()
	})
}

// attempt takes a token for a further wire attempt of the ticket's request
// (another address of the peer); the slot is already held.
func (t *pullTicket) attempt(ctx context.Context) error {
	t.b.mu.Lock()
	d := t.b.reserve(t.k, time.Now())
	t.b.mu.Unlock()
	return t.b.pay(ctx, t.k, d, nil)
}

type attemptKey struct{}

// PullAttempt is called by a transport before each attempt of one request
// after the first (another address of the peer): it waits for a token of
// the peer's budget, so every attempt that may reach the wire is charged.
// For a context that is not a request's, it does nothing.
func PullAttempt(ctx context.Context) error {
	if t, ok := ctx.Value(attemptKey{}).(*pullTicket); ok {
		return t.attempt(ctx)
	}
	return nil
}
