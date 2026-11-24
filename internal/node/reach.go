package node

// Whether other members can reach this machine (status, net check): the
// asker's side of the dial-back (PROTOCOL 4.7). A machine cannot see from
// inside whether its listener is reachable; a member that dials back to
// it can.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// The answers to "can peers reach this machine?".
const (
	ReachYes     = "yes"
	ReachNo      = "no"
	ReachUnknown = "unknown"
)

// Reach is what this machine knows of whether its members can reach it.
type Reach struct {
	State string `json:"state"` // yes, no or unknown
	// Checked is when a member last answered a dial-back (PROTOCOL 0
	// time); empty if none has.
	Checked string `json:"checked,omitempty"`
	// Reached is the advertised addresses some member reached; By the
	// members that answered the last check.
	Reached []string `json:"reached,omitempty"`
	By      []string `json:"by,omitempty"`
	// NotTried is the addresses no answering member would dial for this
	// machine (DialBackResponse.NotTried).
	NotTried []string `json:"not_tried,omitempty"`
	// Saw is the IP addresses members said this machine's requests came
	// from: shown, compared with a router's external address, and never
	// advertised (DESIGN 11).
	Saw []string `json:"saw,omitempty"`
	// Verified is the router's mapped external addresses that a member
	// reached within VerifiedFor: advertised while they are.
	Verified []string `json:"verified,omitempty"`
	Why      string   `json:"why,omitempty"`
}

// DialBackTransport asks peer to dial back to addrs (internal/peer).
type DialBackTransport interface {
	DialBack(ctx context.Context, peer string, addrs []string) (*DialBackResponse, error)
}

// Timing of the automatic check. Variables so tests can shorten them.
var (
	// ReachEvery is how often the daemon asks again; never a peer more
	// often than this, except when a person runs a2p2 net check or the
	// router's mapping changes.
	ReachEvery = time.Hour
	// VerifiedFor is how long a member's dial-back to a mapped address
	// lets it be advertised: three hourly checks may miss before it stops.
	VerifiedFor = 3 * time.Hour
	// reachFirst is the wait after a run starts (so the first pulls have
	// filled the address book), reachRetry the wait after a check no
	// member answered.
	reachFirst = 20 * time.Second
	reachRetry = 10 * time.Minute
	// reachAsk is how many members one check asks.
	reachAsk = 3
)

// ReachOpts configure a Reacher.
type ReachOpts struct {
	// Members lists the other machines that are members of a room held
	// here now.
	Members func() []string
	// Book orders them: members with an address that completed a pinned
	// handshake first.
	Book *AddrBook
	// Addrs is what this machine advertises from its interfaces: what
	// members are asked to dial.
	Addrs func() []string
	// Candidates, if set, is the external address of the router mapping
	// this machine asked for, if it has one now: verified by a dial-back
	// before it is advertised. Nothing else is ever a candidate.
	Candidates func() []string
	Now        func() time.Time
}

// Reacher keeps the answer up to date for a run with a listener.
type Reacher struct {
	t     DialBackTransport
	o     ReachOpts
	nudge chan struct{}

	mu       sync.Mutex
	asked    map[string]time.Time
	cur      Reach
	verified map[string]time.Time // candidate: when a member last reached it
}

// NewReacher asks through t.
func NewReacher(t DialBackTransport, o ReachOpts) *Reacher {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Reacher{t: t, o: o, nudge: make(chan struct{}, 1), asked: map[string]time.Time{},
		verified: map[string]time.Time{}, cur: Reach{State: ReachUnknown, Why: "not checked yet"}}
}

// Get is the latest answer.
func (r *Reacher) Get() Reach {
	v := r.Verified()
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.cur
	c.Reached, c.By, c.Saw = slices.Clone(c.Reached), slices.Clone(c.By), slices.Clone(c.Saw)
	c.NotTried = slices.Clone(c.NotTried)
	c.Verified = v
	return c
}

// order puts members with a verified address first, then the rest, each
// group in a stable order.
func (r *Reacher) order(peers []string) []string {
	out := slices.Clone(peers)
	slices.Sort(out)
	out = slices.Compact(out)
	if r.o.Book != nil {
		good := func(n string) bool {
			d := r.o.Book.Dial(n)
			return len(d) > 0 && d[0].Good
		}
		sort.SliceStable(out, func(i, j int) bool { return good(out[i]) && !good(out[j]) })
	}
	return out
}

func (r *Reacher) candidates() []string {
	if r.o.Candidates == nil {
		return nil
	}
	return r.o.Candidates()
}

// Check asks up to three of peers to dial back to this machine's
// advertised addresses and its candidate (DESIGN 11). Unless force (a
// person asked, or the mapping changed), a peer asked in the last
// ReachEvery is not asked again. A check that no member answers changes
// nothing already known.
func (r *Reacher) Check(ctx context.Context, peers []string, force bool) Reach {
	base := r.o.Addrs()
	cands := r.candidates()
	if len(base) == 0 && len(cands) == 0 {
		r.set(Reach{State: ReachNo, Why: "this machine advertises no address"}, true)
		return r.Get()
	}
	now := r.o.Now()
	var ask []string
	r.mu.Lock()
	for _, p := range r.order(peers) {
		if len(ask) == reachAsk {
			break
		}
		if last, ok := r.asked[p]; !force && ok && now.Sub(last) < ReachEvery {
			continue
		}
		r.asked[p] = now
		ask = append(ask, p)
	}
	r.mu.Unlock()
	if len(ask) == 0 {
		return r.Get()
	}
	addrs := askList(cands, base)
	rd := r.round(ctx, ask, addrs)
	at := r.o.Now()
	if len(rd.by) > 0 {
		r.mu.Lock()
		for _, c := range cands {
			switch {
			case slices.Contains(rd.reached, c):
				r.verified[c] = at
			case slices.Contains(rd.tried, c):
				delete(r.verified, c) // members tried it and could not connect
			}
		}
		r.mu.Unlock()
	}
	stamp := at.UTC().Format(timeFormat)
	switch {
	case len(rd.reached) > 0:
		r.set(Reach{State: ReachYes, Checked: stamp, Reached: rd.reached, By: rd.by, NotTried: rd.notTried, Saw: rd.saw}, true)
	case len(rd.tried) > 0:
		r.set(Reach{State: ReachNo, Checked: stamp, By: rd.by, NotTried: rd.notTried, Saw: rd.saw,
			Why: fmt.Sprintf("%d member(s) tried %s and could not connect", len(rd.by), strings.Join(rd.tried, ", "))}, true)
	case len(rd.by) > 0:
		r.set(Reach{State: ReachUnknown, Checked: stamp, By: rd.by, NotTried: rd.notTried, Saw: rd.saw,
			Why: "the members that answered tried none of " + strings.Join(addrs, ", ") + ": " + NotTriedText}, false)
	default:
		r.set(Reach{State: ReachUnknown, Why: "no member answered the dial-back: " + strings.Join(rd.failed, "; ")}, false)
	}
	return r.Get()
}

// askList is the candidates, then the advertised addresses, without
// repeats, at most MaxAddrs.
func askList(cands, base []string) []string {
	var out []string
	for _, a := range append(slices.Clone(cands), base...) {
		if !slices.Contains(out, a) && len(out) < MaxAddrs {
			out = append(out, a)
		}
	}
	return out
}

// Verified is the candidates a member reached within VerifiedFor that
// are still candidates: the mapped external addresses that may be
// advertised now. A mapping that is gone, or not confirmed in time, is
// not.
func (r *Reacher) Verified() []string {
	cands := r.candidates()
	now := r.o.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range cands {
		if at, ok := r.verified[c]; ok && now.Sub(at) < VerifiedFor {
			out = append(out, c)
		}
	}
	return out
}

// Nudge asks Run to check again at once with the current candidates (the
// router's mapping changed). It never blocks.
func (r *Reacher) Nudge() {
	select {
	case r.nudge <- struct{}{}:
	default:
	}
}

// roundResult is what one round of dial-backs gave: who answered, the
// addresses reached, tried (by at least one answering member) and not
// tried (by any), the addresses seen and why others did not answer.
type roundResult struct{ by, reached, tried, notTried, saw, failed []string }

// round asks each of peers at once to dial back to addrs.
func (r *Reacher) round(ctx context.Context, peers, addrs []string) roundResult {
	type answer struct {
		peer string
		resp *DialBackResponse
		err  error
	}
	answers := make([]answer, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := r.t.DialBack(ctx, p, addrs)
			answers[i] = answer{p, resp, err}
		}()
	}
	wg.Wait()
	var rd roundResult
	add := func(to *[]string, x string) {
		if !slices.Contains(*to, x) {
			*to = append(*to, x)
		}
	}
	for _, a := range answers {
		if a.err != nil {
			rd.failed = append(rd.failed, core.ShortNode(a.peer)+": "+dialBackErrText(a.err))
			continue
		}
		rd.by = append(rd.by, a.peer)
		for _, x := range a.resp.Reached {
			add(&rd.reached, x)
		}
		for _, x := range addrs {
			if !slices.Contains(a.resp.NotTried, x) {
				add(&rd.tried, x)
			}
		}
		if a.resp.Saw != "" {
			add(&rd.saw, a.resp.Saw)
		}
	}
	for _, x := range addrs {
		if len(rd.by) > 0 && !slices.Contains(rd.tried, x) {
			rd.notTried = append(rd.notTried, x)
		}
	}
	return rd
}

// set replaces the answer; one that is not conclusive only replaces
// another that is not.
func (r *Reacher) set(x Reach, conclusive bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if conclusive || r.cur.State == ReachUnknown {
		r.cur = x
	}
}

// dialBackErrText says why a member did not answer, in a few words.
func dialBackErrText(err error) string {
	var pe *PullError
	if errors.As(err, &pe) {
		switch {
		case pe.Status == 404:
			return "it runs an a2p2 without the dial-back"
		case pe.Code == CodeRateLimited:
			return "it asked to wait (rate limited)"
		case pe.Code == CodeNotMember:
			return "it does not count this machine as a member"
		}
		return pe.Code
	}
	return "not reached"
}

// Run checks after the run starts, then every ReachEvery (sooner when no
// member answered, at once when nudged), until ctx ends.
func (r *Reacher) Run(ctx context.Context) {
	wait := reachFirst
	for {
		force := false
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-r.nudge:
			force = true
		}
		got := r.Check(ctx, r.o.Members(), force)
		wait = ReachEvery
		if got.State == ReachUnknown {
			wait = reachRetry
		}
	}
}
