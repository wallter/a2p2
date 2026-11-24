package node

// Whether other members can reach this machine (status, net check): the
// asker's side of the dial-back (PROTOCOL 4.7). A machine cannot see from
// inside whether its listener is reachable; a member that dials back to
// it can.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strconv"
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
	// Saw is the addresses members saw this machine's requests come from,
	// as they said: hints only (PROTOCOL 4.7).
	Saw []string `json:"saw,omitempty"`
	// Verified is the external addresses (seen by members, or mapped by
	// the router) that a member reached: advertised while they are.
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
	// often than this, except when a person runs a2p2 net check.
	ReachEvery = time.Hour
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
	// Addrs is what this machine advertises: what members are asked to
	// dial.
	Addrs func() []string
	// Port is the listening port, joined to an address members saw to
	// make a candidate; 0: seen addresses are not tried.
	Port int
	// Candidates, if set, lists more addresses to verify (a mapped
	// external address).
	Candidates func() []string
	Now        func() time.Time
}

// Reacher keeps the answer up to date for a run with a listener.
type Reacher struct {
	t DialBackTransport
	o ReachOpts

	mu    sync.Mutex
	asked map[string]time.Time
	cur   Reach
}

// NewReacher asks through t.
func NewReacher(t DialBackTransport, o ReachOpts) *Reacher {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Reacher{t: t, o: o, asked: map[string]time.Time{}, cur: Reach{State: ReachUnknown, Why: "not checked yet"}}
}

// Get is the latest answer.
func (r *Reacher) Get() Reach {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.cur
	c.Reached, c.By, c.Saw = slices.Clone(c.Reached), slices.Clone(c.By), slices.Clone(c.Saw)
	c.Verified = slices.Clone(c.Verified)
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

// Check asks up to three of peers to dial back to this machine's
// advertised addresses and its candidates (DESIGN 11). Unless force (a
// person asked), a peer asked in the last ReachEvery is not asked again.
// A candidate seen for the first time in the answers is tried at once, in
// one more round to the members that answered. A check that no member
// answers changes nothing already known.
func (r *Reacher) Check(ctx context.Context, peers []string, force bool) Reach {
	base := r.o.Addrs()
	if len(base) == 0 && len(r.candidates(nil)) == 0 {
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
	cands := r.candidates(nil)
	addrs := askList(cands, base)
	rd := r.round(ctx, ask, addrs)
	// The members saw this machine at addresses not yet asked: try them now.
	if more := r.candidates(rd.saw); len(rd.by) > 0 && len(more) > len(cands) {
		cands = more
		extra := askList(more, base)
		rd.merge(r.round(ctx, rd.by, extra))
		addrs = extra
	}
	var verified []string
	for _, c := range cands {
		if slices.Contains(rd.reached, c) {
			verified = append(verified, c)
		}
	}
	at := r.o.Now().UTC().Format(timeFormat)
	switch {
	case len(rd.reached) > 0:
		r.set(Reach{State: ReachYes, Checked: at, Reached: rd.reached, By: rd.by, Saw: rd.saw, Verified: verified}, true)
	case len(rd.by) > 0:
		r.set(Reach{State: ReachNo, Checked: at, By: rd.by, Saw: rd.saw,
			Why: fmt.Sprintf("%d member(s) tried %s and could not connect", len(rd.by), strings.Join(addrs, ", "))}, true)
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

// candidates are the addresses to verify before advertising: what the
// daemon offers (a mapped external address) and, for each global IP
// address members saw (now, or in the last answer) that is not already
// advertised, that IP with the listening port.
func (r *Reacher) candidates(saw []string) []string {
	var out []string
	if r.o.Candidates != nil {
		out = append(out, r.o.Candidates()...)
	}
	if r.o.Port == 0 {
		return out
	}
	base := r.o.Addrs()
	r.mu.Lock()
	all := append(slices.Clone(r.cur.Saw), saw...)
	r.mu.Unlock()
	for _, s := range all {
		ip, err := netip.ParseAddr(s)
		if err != nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
			continue
		}
		c := net.JoinHostPort(ip.String(), strconv.Itoa(r.o.Port))
		if !slices.Contains(base, c) && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// Verified is the candidates the last conclusive check reached: external
// addresses that may now be advertised.
func (r *Reacher) Verified() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.cur.Verified)
}

// roundResult is what one round of dial-backs gave.
type roundResult struct{ by, reached, saw, failed []string }

func (a *roundResult) merge(b roundResult) {
	add := func(to *[]string, from []string) {
		for _, x := range from {
			if !slices.Contains(*to, x) {
				*to = append(*to, x)
			}
		}
	}
	add(&a.by, b.by)
	add(&a.reached, b.reached)
	add(&a.saw, b.saw)
}

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
	for _, a := range answers {
		if a.err != nil {
			rd.failed = append(rd.failed, core.ShortNode(a.peer)+": "+dialBackErrText(a.err))
			continue
		}
		rd.by = append(rd.by, a.peer)
		for _, x := range a.resp.Reached {
			if !slices.Contains(rd.reached, x) {
				rd.reached = append(rd.reached, x)
			}
		}
		if a.resp.Saw != "" && !slices.Contains(rd.saw, a.resp.Saw) {
			rd.saw = append(rd.saw, a.resp.Saw)
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
// member answered), until ctx ends.
func (r *Reacher) Run(ctx context.Context) {
	wait := reachFirst
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		got := r.Check(ctx, r.o.Members(), false)
		wait = ReachEvery
		if got.State == ReachUnknown {
			wait = reachRetry
		}
	}
}
