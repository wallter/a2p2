package node

// The differential test over the real exchange: the harness of
// differential_test.go with a room log on every machine and pulls made by
// Exchange over the in-memory transport, compared with the delivery model
// after every step: every inbox, wait, thread and branch view, every held
// record and its reason, and every copy report.
//
// How the two sides are kept in step:
//
//   - Membership. The model's changes travel out of band (Learn); the real
//     log travels in pulls. One machine, the room's creator and only admin,
//     signs every change, and only while it knows every change made, so the
//     changes every machine knows are a prefix of the model's list on both
//     sides. A real pull carries the log both ways; the model is told the
//     same: the server learns what the requester knows before the request
//     (unless the requester is not a member in its view and the entries
//     would not make it one), and the requester learns what the server
//     knows between the request and its release (Hold, Learn, Release).
//   - Rounds. One real exchange runs rounds until a round makes no
//     progress; the model's counterpart is one Pull per real round, then
//     more while a pull stores something. A real round is a model pull
//     (an offer against the last observation, a page against the current
//     summary); with a limit of one, a real round pages on while a page was
//     cut and a model pull does not, so the model takes more pulls. Both
//     stop at the same state, and the copy reports' dates are the same.
//   - A lost response loses the response of the round's last request: the
//     server has then seen the requester's whole summary, as the model's
//     server has. If the requester had applied the round's first response
//     (its log entries), the model's requester learns those changes too.
//   - A pull whose server knows of its own removal while the requester does
//     not is skipped (and counted): the requester learns it from the first
//     response and stops, so the server has seen only digests, while the
//     model's server has seen the whole summary (see the final report).
//   - Copy reports and known positions carry the observer's clock; the
//     model's carry the true time. The harness keeps each machine's clock
//     settings and compares the model's time read on that clock.

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/model/delivery"
	"github.com/wallter/a2p2/internal/store"
)

// Operations of the real exchange (opMore and after).
const (
	opSleep    opKind = opMore + iota // machine stops; nothing is lost
	opWake                            // and starts again (not a restart)
	opAdmit                           // the admin admits machine (n: 1 with history)
	opRemove                          // the admin removes machine; only it knows until the log travels
	opWithhold                        // machine (a server) leaves out msgs[target] when serving
	opLearn                           // machine is handed the admin's log directly (the model's Learn)
)

// Pull flags in op.n.
const (
	pullOffer = 1 << iota
	pullLimit1
	pullLost
)

func deepModel() bool { return os.Getenv("A2P2_MODEL_DEEP") == "1" }

func pullFlags(n int) string {
	var out []string
	if n&pullOffer != 0 {
		out = append(out, "offer")
	}
	if n&pullLimit1 != 0 {
		out = append(out, "limit 1")
	}
	if n&pullLost != 0 {
		out = append(out, "response lost")
	}
	if len(out) == 0 {
		return ""
	}
	return " (" + strings.Join(out, ", ") + ")"
}

func (o op) moreString() string {
	switch o.kind {
	case opSleep:
		return fmt.Sprintf("%s sleeps", o.machine)
	case opWake:
		return fmt.Sprintf("%s wakes", o.machine)
	case opAdmit:
		return fmt.Sprintf("admit %s history=%v", o.machine, o.n == 1)
	case opRemove:
		return fmt.Sprintf("remove %s", o.machine)
	case opWithhold:
		return fmt.Sprintf("%s withholds #%d", o.machine, o.target)
	case opLearn:
		return fmt.Sprintf("%s is handed the admin's log", o.machine)
	}
	return "?"
}

// pxChange is one model change, as the harness tracks it.
type pxChange struct {
	add     bool
	machine delivery.MachineID
}

// pxCounts are shared by a harness and its clones.
type pxCounts struct {
	pulls, failed, lost, removedServer, modelPulls, requests, copies, texts, repaired, undecided, uncompared, same int
	offerDropped, seenSame, seenSkipped, reoffered, resent                                                         int
}

// pxState is the real exchange's side of the harness.
type pxState struct {
	net       *memNet
	admin     delivery.MachineID
	keys      map[delivery.MachineID]ed25519.PrivateKey
	later     map[delivery.MachineID][]string // agents of machines admitted later
	create    []byte                          // the room's create entry
	changes   []pxChange
	known     map[delivery.MachineID]int // how many changes each machine knows (a prefix)
	asleep    map[delivery.MachineID]bool
	noInbound map[delivery.MachineID]bool
	withhold  map[delivery.MachineID]map[string]bool
	skews     map[delivery.MachineID][]skewChange // each machine's clock settings, in order
	// partial: observer, peer whose last exchange was cut after the
	// observer saw only the peer's digests (see sameCopies).
	partial map[[2]delivery.MachineID]bool
	// lostAfter: requester, server of a pull that applied responses before
	// the lost one.
	lostAfter map[[2]delivery.MachineID]bool
	// gapsUnknown: machines that served a request from a server that knew
	// of its own removal; see knownGaps.
	gapsUnknown map[delivery.MachineID]bool
	c           *pxCounts
	offers      int // records offered by the requests up to a lost response
}

// knownGaps is the model's waiting list mw of a machine that served, after
// its own removal, a requester that stopped after the first response: the
// model's server saw the requester's whole summary, the real one its
// digests only, so it does not know how far streams that differ go (4.3
// item 4). Such a machine is never again a member, so this lasts. Gap
// waits the model shows and the implementation does not, or with another
// known position, are left out and counted; everything else is compared.
func (p *pxState) knownGaps(h *harness, mw, iw []string) []string {
	stream := func(w string) string { return strings.SplitN(w, " known=", 2)[0] }
	got := map[string]bool{}
	for _, w := range iw {
		if strings.Contains(w, " gap ") {
			got[stream(w)] = true
		}
	}
	var out []string
	for _, w := range mw {
		if strings.Contains(w, " gap ") && !slices.Contains(iw, w) {
			p.c.uncompared++
			if got[stream(w)] {
				// Same stream and have: take the implementation's line.
				for _, x := range iw {
					if stream(x) == stream(w) {
						out = append(out, x)
					}
				}
			}
			continue
		}
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

// skewChange is a machine's clock set to skew at true time at.
type skewChange struct{ at, skew delivery.Time }

// skewAt is machine m's skew at true time t: what it read its clock as
// then is t plus this. A clock is never set and read in the same hour.
func (h *harness) skewAt(m delivery.MachineID, t delivery.Time) delivery.Time {
	if h.px == nil {
		return 0
	}
	var s delivery.Time
	for _, c := range h.px.skews[m] {
		if c.at <= t {
			s = c.skew
		}
	}
	return s
}

type pxBackup struct {
	known            int
	partial, lostAft map[delivery.MachineID]bool // peers
}

func (p *pxState) clone() *pxState {
	c := *p
	c.net = newMemNet()
	c.changes = slices.Clone(p.changes)
	c.known, c.asleep, c.noInbound = cloneMap(p.known), cloneMap(p.asleep), cloneMap(p.noInbound)
	c.partial, c.lostAfter = cloneMap(p.partial), cloneMap(p.lostAfter)
	c.gapsUnknown = cloneMap(p.gapsUnknown)
	c.skews = map[delivery.MachineID][]skewChange{}
	for k, v := range p.skews {
		c.skews[k] = slices.Clone(v)
	}
	c.withhold = map[delivery.MachineID]map[string]bool{}
	for k, v := range p.withhold {
		c.withhold[k] = cloneMap(v)
	}
	return &c
}

func (p *pxState) backup(m delivery.MachineID) *pxBackup {
	b := &pxBackup{known: p.known[m], partial: map[delivery.MachineID]bool{}, lostAft: map[delivery.MachineID]bool{}}
	for k := range p.partial {
		if k[0] == m {
			b.partial[k[1]] = true
		}
	}
	for k := range p.lostAfter {
		if k[0] == m {
			b.lostAft[k[1]] = true
		}
	}
	return b
}

func (p *pxState) restore(m delivery.MachineID, b *pxBackup) {
	p.known[m] = b.known
	for _, f := range []struct {
		flags map[[2]delivery.MachineID]bool
		saved map[delivery.MachineID]bool
	}{{p.partial, b.partial}, {p.lostAfter, b.lostAft}} {
		for k := range f.flags {
			if k[0] == m {
				delete(f.flags, k)
			}
		}
		for peer := range f.saved {
			f.flags[[2]delivery.MachineID{m, peer}] = true
		}
	}
}

// open gives s its room log and exchange (a daemon start).
func (p *pxState) open(h *harness, s *sim) {
	r, err := OpenRooms(context.Background(), s.m)
	if err != nil {
		h.t.Fatal(err)
	}
	s.r = r
	s.x = NewExchange(r, p.net.link(s.m.ID), ExchangeOpts{})
	name := s.name
	s.x.withhold = func(id string) bool { return p.withhold[name][id] }
	p.net.attach(s.m.ID, s.x)
}

// skip says whether o, on a machine that is asleep or not yet admitted,
// does nothing on either side.
func (p *pxState) skip(h *harness, o op) bool {
	switch o.kind {
	case opAdvance, opPull, opWake, opAdmit:
		return false
	}
	return p.asleep[o.machine] || h.sims[o.machine] == nil
}

// members is the membership a machine knowing the first k changes sees.
func (p *pxState) members(k int) map[delivery.MachineID]bool {
	out := map[delivery.MachineID]bool{}
	for _, c := range p.changes[:k] {
		out[c.machine] = c.add
	}
	return out
}

// newPullHarness is newHarness with a room log: the first machine (by
// name) creates the room and admits the other founders with history; later
// machines exist only once admitted. Keys sort as names do, for every
// machine, so stream order is the same on both sides.
func newPullHarness(t testing.TB, founders, later map[delivery.MachineID][]string) *harness {
	h := &harness{t: t, w: delivery.NewWorld(), sims: map[delivery.MachineID]*sim{},
		label: map[string]string{}, pair: map[delivery.RecordID]string{}, implPair: map[string]delivery.RecordID{},
		nodes: map[string]delivery.MachineID{}, shown: map[agentKey]listing{}, injected: map[string]bool{},
		reg: map[delivery.MachineID]map[streamKey]int64{}, c: &counts{}}
	p := &pxState{net: newMemNet(), keys: map[delivery.MachineID]ed25519.PrivateKey{}, later: later,
		known: map[delivery.MachineID]int{}, asleep: map[delivery.MachineID]bool{}, noInbound: map[delivery.MachineID]bool{},
		withhold: map[delivery.MachineID]map[string]bool{}, skews: map[delivery.MachineID][]skewChange{},
		partial: map[[2]delivery.MachineID]bool{}, lostAfter: map[[2]delivery.MachineID]bool{}, gapsUnknown: map[delivery.MachineID]bool{}, c: &pxCounts{}}
	h.px = p
	var names, fs []delivery.MachineID
	for n := range founders {
		names, fs = append(names, n), append(fs, n)
	}
	for n := range later {
		names = append(names, n)
	}
	slices.Sort(names)
	slices.Sort(fs)
	for i, k := range keys(names) {
		p.keys[names[i]] = k
	}
	p.admin = fs[0]
	ctx := context.Background()
	for _, n := range fs {
		h.w.AddMachine(n, founders[n]...)
		h.addSim(n, founders[n])
		p.changes = append(p.changes, pxChange{add: true, machine: n})
	}
	e, err := core.NewEntry(p.keys[p.admin], core.EntryFields{Room: diffRoom, Seq: 1, Kind: core.EntryCreate,
		Label: "team", Created: diffBase})
	if err != nil {
		t.Fatal(err)
	}
	p.create = e.Wire()
	for _, n := range fs {
		if _, err := h.sims[n].r.PinRoom(ctx, p.create); err != nil {
			t.Fatal(err)
		}
	}
	adm := h.sims[p.admin]
	for _, n := range fs[1:] {
		h.change(Change{Kind: core.EntryAdd, Target: h.sims[n].m.ID, Name: strings.ToLower(string(n)), History: true})
	}
	h.handLog(fs...)
	for _, n := range fs {
		p.known[n] = len(p.changes)
	}
	_ = adm
	return h
}

// addSim starts the implementation side of machine n with agents.
func (h *harness) addSim(n delivery.MachineID, agents []string) *sim {
	s := &sim{name: n, key: h.px.keys[n], path: filepath.Join(h.t.TempDir(), "a2p2.db")}
	st, err := store.Open(s.path)
	if err != nil {
		h.t.Fatal(err)
	}
	s.st = st
	h.start(s, nil)
	for _, a := range agents {
		if _, err := s.m.Register(context.Background(), a); err != nil {
			h.t.Fatal(err)
		}
		s.agents = append(s.agents, a)
	}
	h.nodes[s.m.ID] = n
	h.sims[n] = s
	h.order = append(h.order, n)
	return s
}

// change has the admin sign one change.
func (h *harness) change(c Change) {
	h.t.Helper()
	r := h.sims[h.px.admin].r
	q, err := r.Propose(context.Background(), diffRoom, c)
	if err != nil {
		h.fail("propose %+v: %v", c, err)
	}
	cf, err := r.Confirm(context.Background(), q.ID, true)
	if err != nil || cf.State != store.RequestApproved {
		h.fail("confirm: %+v %v", cf, err)
	}
}

// handLog gives machines the admin's whole log directly (the join, step 4b).
func (h *harness) handLog(to ...delivery.MachineID) {
	ws, err := h.sims[h.px.admin].r.Offer(diffRoom)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, n := range to {
		if n == h.px.admin {
			continue
		}
		if _, err := h.sims[n].r.Receive(context.Background(), diffRoom, 0, ws...); err != nil {
			h.fail("hand log to %s: %v", n, err)
		}
	}
}

// learnTo has the model's machine m learn changes until it knows k.
func (p *pxState) learnTo(h *harness, m delivery.MachineID, k int) {
	for p.known[m] < k {
		if ok, err := h.w.Learn(m); err != nil || !ok {
			h.fail("model learn %s: %v %v", m, ok, err)
		}
		p.known[m]++
	}
}

func (p *pxState) do(h *harness, o op) {
	h.t.Helper()
	switch o.kind {
	case opSleep:
		h.w.Sleep(o.machine)
		p.asleep[o.machine] = true
	case opWake:
		if !p.asleep[o.machine] {
			return
		}
		h.w.Wake(o.machine)
		p.asleep[o.machine] = false
		h.prune(h.sims[o.machine])
	case opAdmit:
		agents, ok := p.later[o.machine]
		if !ok || h.sims[o.machine] != nil || p.asleep[p.admin] || p.known[p.admin] != len(p.changes) {
			return
		}
		history := o.n == 1
		if _, err := h.w.Grant(o.machine, p.admin, history, agents...); err != nil {
			h.fail("model grant: %v", err)
		}
		s := h.addSim(o.machine, agents)
		if _, err := s.r.PinRoom(context.Background(), p.create); err != nil {
			h.t.Fatal(err)
		}
		h.change(Change{Kind: core.EntryAdd, Target: s.m.ID, Name: strings.ToLower(string(o.machine)), History: history})
		h.handLog(o.machine)
		p.changes = append(p.changes, pxChange{add: true, machine: o.machine})
		p.known[p.admin], p.known[o.machine] = len(p.changes), len(p.changes)
	case opRemove:
		s := h.sims[o.machine]
		if s == nil || o.machine == p.admin || p.asleep[p.admin] || p.known[p.admin] != len(p.changes) ||
			!p.members(len(p.changes))[o.machine] {
			return
		}
		if err := h.w.Remove(o.machine, p.admin); err != nil {
			h.fail("model remove: %v", err)
		}
		h.change(Change{Kind: core.EntryRemove, Target: s.m.ID})
		p.changes = append(p.changes, pxChange{machine: o.machine})
		p.known[p.admin] = len(p.changes)
	case opLearn:
		if h.sims[o.machine] == nil || o.machine == p.admin || p.asleep[p.admin] || p.asleep[o.machine] {
			return
		}
		h.handLog(o.machine)
		p.learnTo(h, o.machine, p.known[p.admin])
	case opWithhold:
		_, iid, ok := h.target(o.target)
		if !ok {
			return
		}
		mid := h.implPair[iid]
		h.w.Withhold(o.machine, mid)
		if p.withhold[o.machine] == nil {
			p.withhold[o.machine] = map[string]bool{}
		}
		p.withhold[o.machine][iid] = true
	default:
		h.fail("unknown operation %v", o.kind)
	}
}

// pull runs one real exchange and its model counterpart, and checks they
// agree on whether it was refused.
func (p *pxState) pull(h *harness, o op) {
	h.t.Helper()
	pm, sm := o.machine, o.server
	ps, ss := h.sims[pm], h.sims[sm]
	if pm == sm || ps == nil || ss == nil {
		return
	}
	p.c.pulls++
	ctx := context.Background()
	opt := delivery.PullOpt{Offer: o.n&pullOffer != 0, LostReply: o.n&pullLost != 0}
	offers := 0
	if o.n&pullLimit1 != 0 {
		opt.Limit = 1
	}
	real := func() (PullStats, error) {
		defer func() { p.offers = offers }()
		ps.x.pageLimit, ps.x.offerLimit = MaxPageRecords, MaxOfferRecords
		ss.x.pageLimit, ss.x.offerLimit = MaxPageRecords, MaxOfferRecords
		if opt.Limit == 1 {
			ss.x.pageLimit, ps.x.offerLimit = 1, 1
		}
		p.net.noInbound, p.net.down = map[string]bool{}, map[string]bool{}
		for n, a := range p.noInbound {
			if a {
				p.net.noInbound[h.sims[n].m.ID] = true
			}
		}
		for n, a := range p.asleep {
			if a {
				p.net.down[h.sims[n].m.ID] = true
			}
		}
		p.net.lose = nil
		offers = 0
		if opt.LostReply {
			// The response that carries the page is lost: the first to a
			// continuing request. The server has then had the detail of
			// every author that differs and built its page, as the model's
			// server has; the requester has applied the opening's log
			// entries and digests, no page. With nothing differing nothing
			// is lost (the exchange is then compared as a whole).
			lost := false
			p.net.lose = func(_, _ string, q *PullRequest, resp *PullResponse) bool {
				if !lost {
					offers += len(q.Offer)
				}
				if !lost && q.Detail != nil {
					lost = true
				}
				return lost
			}
		}
		n0 := len(p.net.reqBytes)
		p.net.pageText, p.net.offerText = nil, nil
		st, err := ps.x.Pull(ctx, diffRoom, ss.m.ID, PullOpts{Offer: opt.Offer})
		p.c.same += st.Same
		p.c.seenSame += st.SeenSame
		p.c.requests += len(p.net.reqBytes) - n0
		p.c.repaired += st.Repaired
		return st, err
	}
	var textTo, textFrom []delivery.RecordID
	// The requester's observation of the server may be older than the
	// model's (made as server, of authors whose digests did not differ; see
	// sameCopies), or newer. An offer built on either may offer again what
	// the server held already; see sameTexts.
	state := func(x store.Held) string { return fmt.Sprintf("%s/%v", x.Reason, x.Record.Body() != nil) }
	var heldBefore map[string]string
	if p.lostAfter[[2]delivery.MachineID{pm, sm}] {
		heldBefore = map[string]string{}
		for id, x := range h.all(ss) {
			heldBefore[id] = state(x)
		}
	}
	sameTexts := func() {
		h.t.Helper()
		for _, c := range []struct {
			what  string
			model []delivery.RecordID
			impl  []string
		}{{"pages", textTo, p.net.pageText}, {"offers", textFrom, p.net.offerText}} {
			want, got := map[string]bool{}, map[string]bool{}
			// With the requester's observation older on one side, either
			// side may offer what the server held already and the offer
			// left as it was (a wasted copy): only those are left out.
			after := map[string]store.Held{}
			if c.what == "offers" && heldBefore != nil {
				after = h.all(ss)
			}
			again := func(id string) bool {
				x, ok := after[id]
				return ok && heldBefore[id] != "" && heldBefore[id] == state(x)
			}
			for _, id := range c.model {
				if x, ok := h.pair[id]; ok && h.hasText(x) {
					if again(x) {
						p.c.reoffered++
						continue
					}
					want[h.id(x)] = true
				}
			}
			for _, id := range c.impl {
				if again(id) {
					p.c.reoffered++
					continue
				}
				got[h.id(id)] = true
			}
			if a, b := slices.Sorted(maps.Keys(want)), slices.Sorted(maps.Keys(got)); !slices.Equal(a, b) {
				h.fail("text sent in %s: model %v, implementation %v", c.what, a, b)
			}
			p.c.texts += len(want)
		}
	}
	fails := func(what string, merr error) {
		_, ierr := real()
		if merr == nil || ierr == nil {
			h.fail("%s: model %v, implementation %v", what, merr, ierr)
		}
		p.c.failed++
	}
	// Who can talk to whom, decided before either side acts.
	if p.asleep[pm] || p.asleep[sm] {
		_, merr := h.w.Pull(pm, sm, opt)
		fails("pull with a machine asleep", merr)
		return
	}
	kp, ks := p.known[pm], p.known[sm]
	if !p.members(kp)[sm] || p.noInbound[sm] {
		_, merr := h.w.Pull(pm, sm, opt)
		fails("pull from a machine the puller does not see as a member, or that takes no connections", merr)
		return
	}
	// The server takes the requester's log entries if the requester is a
	// member in its view, or if they make it one (T4).
	ks2 := ks
	if kp > ks && (p.members(ks)[pm] || p.members(kp)[pm]) {
		ks2 = kp
	}
	if !p.members(ks2)[pm] {
		p.learnTo(h, sm, ks2)
		_, merr := h.w.Pull(pm, sm, opt)
		fails("pull by a machine the server does not see as a member", merr)
		return
	}
	kp2 := max(kp, ks2)
	if !p.members(kp2)[sm] && opt.Offer {
		// Model gap: the model's server that knows of its own removal takes
		// offered text; the implementation keeps headers only (not a member
		// here, access.go). Such pulls run without the offer.
		opt.Offer = false
		p.c.offerDropped++
		h.log[len(h.log)-1] += " (offer left out: the server knows of its own removal)"
	}
	h.prune(ps)
	h.prune(ss)
	p.learnTo(h, sm, ks2)
	switch {
	case !p.members(kp2)[sm]:
		// The server knows of its own removal, the requester does not: the
		// server serves, the requester applies the first response's log
		// entries, learns of the removal and takes nothing more. Each has
		// seen only the other's digests.
		opt.LostReply = false // the response that matters is the first: not lost here
		st, err := real()
		if !errors.Is(err, ErrPeerNotMember) || st.Responses != 1 || st.Entries != 1 {
			h.fail("implementation pull from a server that knows of its own removal: %+v %v", st, err)
		}
		hd, err := h.w.Hold(pm, sm, opt)
		if err != nil {
			h.fail("model pull: %v", err)
		}
		p.learnTo(h, pm, kp2)
		if r, err := h.w.Release(hd); !errors.Is(err, delivery.ErrNotAllow) {
			h.fail("model release from a server that knows of its own removal: %+v %v", r, err)
		} else {
			textTo, textFrom = nil, r.TextFrom
		}
		p.c.modelPulls++
		p.lostAfter[[2]delivery.MachineID{sm, pm}] = true
		p.partial[[2]delivery.MachineID{pm, sm}] = true
		p.gapsUnknown[sm] = true
		p.c.removedServer++
	default:
		p.pullRest(h, pm, sm, kp2, opt, real, &textTo, &textFrom)
	}
	sameTexts()
	h.prune(ps)
	h.prune(ss)
}

// sumOfSim is s's sum of the room now.
func sumOfSim(h *harness, s *sim) string {
	h.t.Helper()
	rl, err := s.r.room(diffRoom)
	if err != nil {
		h.t.Fatal(err)
	}
	var sum string
	if err := s.m.view(context.Background(), diffRoom, func(v *roomView) error {
		own, err := s.r.summarize(v, rl.st)
		sum = own.sum()
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	return sum
}

// pullRest maps a pull between members onto the model: the real exchange
// first; then, with its page-carrying response lost, one lost model pull;
// answered same to seen, nothing; otherwise model pulls repeated while
// they store something, at least as many as the real rounds.
func (p *pxState) pullRest(h *harness, pm, sm delivery.MachineID, kp2 int, opt delivery.PullOpt,
	real func() (PullStats, error), textTo, textFrom *[]delivery.RecordID) {
	h.t.Helper()
	st, err := real()
	requesterOlder := false
	if isNotMember(err) {
		// The requester's entries began where it last saw the server's log,
		// which the server no longer has (it, or the requester, was
		// restored): refused before anything was done. The next attempt
		// carries the whole log (4.5), as the model's one pull does.
		p.c.resent++
		st, err = real()
	}
	switch {
	case opt.LostReply && errors.Is(err, errLost):
		// The model's one request offers what the real requests up to the
		// lost one offered, counted on the wire.
		mopt := opt
		if opt.Limit > 0 {
			mopt.Limit = max(p.offers, 1)
		}
		r, err := h.w.Pull(pm, sm, mopt)
		if err != nil {
			h.fail("model pull: %v", err)
		}
		*textTo, *textFrom = r.TextTo, r.TextFrom
		p.c.modelPulls++
		if st.Responses > 0 {
			p.learnTo(h, pm, kp2)
			p.lostAfter[[2]delivery.MachineID{pm, sm}] = true
		}
		p.lostAfter[[2]delivery.MachineID{sm, pm}] = true
		p.c.lost++
		return
	case err != nil:
		h.fail("implementation pull: %+v %v", st, err)
	case st.SeenSame == 1 && st.Requests == 1 && sumOfSim(h, h.sims[pm]) != sumOfSim(h, h.sims[sm]):
		// Answered same to seen: nothing changed on either side since a
		// round that stored nothing, and neither records an observation.
		// The model's pull would store nothing either, only observe again:
		// it is not run.
		p.c.seenSkipped++
		return
	case st.SeenSame == 1 && st.Requests == 1:
		// Answered same to its sum, though it carried seen too: the server
		// recorded the observation, the requester (which cannot tell which
		// matched) only the log.
		requesterOlder = true
	}
	opt.LostReply = false // nothing differed: nothing was lost
	// A real round is a model pull; the model goes on while a pull
	// stores something, as a real exchange does.
	for i := 0; ; i++ {
		if i == 64 {
			h.fail("model pulls do not end")
		}
		hd, err := h.w.Hold(pm, sm, opt)
		if err != nil {
			h.fail("model pull: %v", err)
		}
		p.learnTo(h, pm, kp2)
		r, err := h.w.Release(hd)
		if err != nil {
			h.fail("model release: %v", err)
		}
		*textTo, *textFrom = append(*textTo, r.TextTo...), append(*textFrom, r.TextFrom...)
		p.c.modelPulls++
		if r.Stored == 0 && r.OfferStored == 0 && i+1 >= st.Rounds {
			break
		}
	}
	for _, k := range [][2]delivery.MachineID{{sm, pm}, {pm, sm}} {
		delete(p.partial, k)
		delete(p.lostAfter, k)
	}
	// The server knows what the requester holds of the authors whose
	// digests differed (their detail); of the rest it learns only from
	// a same at the end (4.3). The model's server observes it all.
	if !st.Closed {
		p.lostAfter[[2]delivery.MachineID{sm, pm}] = true
	}
	if requesterOlder {
		p.lostAfter[[2]delivery.MachineID{pm, sm}] = true
	}
}

// sameCopies compares machine n's copy reports of record x with the
// model's: the same peers, learned at the same time.
func (p *pxState) sameCopies(h *harness, n delivery.MachineID, x written, model []delivery.Report) {
	h.t.Helper()
	s := h.sims[n]
	// After an exchange cut once the observer had the peer's digests only,
	// the observer does not know the peer's detail of an author whose digest
	// changed (4.3 item 4) until they next exchange; the model's peer had
	// the whole summary. Only those reports are left out.
	unknown := func(peer delivery.MachineID) bool {
		if p.lostAfter[[2]delivery.MachineID{n, peer}] {
			// As requester, it applied the responses before a lost one,
			// which the model's lost pull does not; as server, it learned
			// only the detail of the authors that differed, and learns the
			// rest at a closing same (4.3), where the model's server
			// observes the whole summary each pull. None of its reports
			// of that peer compare until such a same.
			return true
		}
		if !p.partial[[2]delivery.MachineID{n, peer}] {
			return false
		}
		var obs []store.Observation
		if err := s.st.View(context.Background(), func(rd *store.Reader) (err error) {
			obs, err = rd.Observed(diffRoom, h.sims[peer].m.ID)
			return err
		}); err != nil {
			h.t.Fatal(err)
		}
		for _, o := range obs {
			if o.Author == h.sims[x.author].m.ID {
				return o.Detail == nil
			}
		}
		return false
	}
	var want []string
	for _, c := range model {
		if unknown(c.Machine) {
			p.c.uncompared++
			continue
		}
		want = append(want, fmt.Sprintf("%s@%d", c.Machine, c.At+h.skewAt(n, c.At)))
	}
	f := h.held(s, x.impl).Record.Fields()
	cs, err := s.st.Copies(context.Background(), diffRoom, h.sims[x.author].m.ID, f.Epoch, f.Seq)
	if err != nil {
		h.t.Fatal(err)
	}
	var got []string
	for _, c := range cs {
		if peer := h.nodes[c.Peer]; p.lostAfter[[2]delivery.MachineID{n, peer}] {
			continue
		} else if unknown(peer) {
			h.fail("copies of %s on %s: reported from %s, whose detail is unknown", h.id(x.impl), n, h.nodes[c.Peer])
		}
		got = append(got, fmt.Sprintf("%s@%d", h.nodes[c.Peer], c.At.Sub(diffBase)/time.Hour))
	}
	sort.Strings(want)
	sort.Strings(got)
	p.c.copies += len(want)
	if !slices.Equal(want, got) {
		h.fail("copies of %s on %s: model %v, implementation %v", h.id(x.impl), n, want, got)
	}
}

// ---- schedules ----

// pullAlphabet is the exhaustive search's operations over founders M and
// P and the later machine N.
func pullAlphabet() []op {
	return []op{
		{kind: opSend, machine: "M", agent: "alice", to: "bob"},
		{kind: opSend, machine: "P", agent: "pat", ask: true},
		{kind: opReply, machine: "P", agent: "pat", target: -1},
		{kind: opWithdraw, machine: "M", agent: "alice", target: 0},
		{kind: opSeenAll, machine: "P", agent: "pat"},
		{kind: opHandled, machine: "P", agent: "pat", target: -1},
		{kind: opPull, machine: "P", server: "M"},
		{kind: opPull, machine: "M", server: "P", n: pullOffer},
		{kind: opPull, machine: "P", server: "M", n: pullLimit1 | pullOffer},
		{kind: opPull, machine: "P", server: "M", n: pullLost | pullOffer},
		{kind: opPull, machine: "N", server: "P", n: pullOffer},
		{kind: opPull, machine: "P", server: "N"},
		{kind: opAdmit, machine: "N"},
		{kind: opAdmit, machine: "N", n: 1},
		{kind: opRemove, machine: "P"},
		{kind: opCrash, machine: "M"},
		{kind: opBackup, machine: "P"},
		{kind: opRestore, machine: "P"},
		{kind: opSleep, machine: "P"},
		{kind: opWake, machine: "P"},
		{kind: opSkew, machine: "P", n: -10},
		{kind: opAdvance, n: 30 * 24},
	}
}

func pullMachines() (founders, later map[delivery.MachineID][]string) {
	return map[delivery.MachineID][]string{"M": {"alice", "bob"}, "P": {"pat"}},
		map[delivery.MachineID][]string{"N": {"nina"}}
}

// randomPullOps is usually one operation of randomPullOp; now and then the
// sequence in which only the server knows of its own removal: the admin
// removes x, x is handed the log (members refuse its pulls, so it learns
// of its removal only out of band), and a member that does not know yet
// pulls from x.
func randomPullOps(r *rand.Rand, h *harness, agents map[delivery.MachineID][]string) []op {
	if r.IntN(10) == 0 {
		var cand []delivery.MachineID
		for _, n := range slices.Sorted(maps.Keys(h.sims)) {
			if n != h.px.admin && h.px.members(len(h.px.changes))[n] {
				cand = append(cand, n)
			}
		}
		if len(cand) >= 2 {
			x, y := cand[r.IntN(len(cand))], cand[r.IntN(len(cand))]
			if x != y {
				return []op{{kind: opRemove, machine: x}, {kind: opLearn, machine: x},
					{kind: opPull, machine: y, server: x}}
			}
		}
	}
	return []op{randomPullOp(r, h, agents)}
}

// randomPullOp draws one operation over the machines h has (admitting
// one it has not yet, now and then).
func randomPullOp(r *rand.Rand, h *harness, agents map[delivery.MachineID][]string) op {
	var names, absent []delivery.MachineID
	for _, n := range slices.Sorted(maps.Keys(agents)) {
		if h.sims[n] != nil {
			names = append(names, n)
		} else {
			absent = append(absent, n)
		}
	}
	if len(absent) > 0 && r.IntN(12) == 0 {
		return op{kind: opAdmit, machine: absent[r.IntN(len(absent))], n: r.IntN(2)}
	}
	m := names[r.IntN(len(names))]
	peers := slices.DeleteFunc(slices.Clone(names), func(x delivery.MachineID) bool { return x == m })
	switch x := r.IntN(100); {
	case x < 4:
		return op{kind: opSleep, machine: m}
	case x < 10:
		return op{kind: opWake, machine: m}
	case x < 12:
		return op{kind: opRemove, machine: m}
	case x < 16:
		return op{kind: opWithhold, machine: m, target: r.IntN(6) - 3}
	case x < 40:
		flags := 0
		if r.IntN(2) == 0 {
			flags |= pullOffer
		}
		if r.IntN(4) == 0 {
			flags |= pullLimit1
		}
		if r.IntN(8) == 0 {
			flags |= pullLost
		}
		return op{kind: opPull, machine: m, server: peers[r.IntN(len(peers))], n: flags}
	}
	o := randomOp(r, m, agents[m], peers, []int{0, -10, 10})
	if o.kind == opInject || o.kind == opRegister {
		return op{kind: opSeenAll, machine: m, agent: agents[m][r.IntN(len(agents[m]))]}
	}
	return o
}

func (p *pxState) report(t testing.TB, what string, d time.Duration, n int, h *counts) {
	t.Logf("%s: %d in %v; %d pulls (%d refused on both sides, %d with the page-carrying response lost, %d from a server that knew of its own removal while the puller did not, %d of them without the offer asked for, %d rounds answered same, %d of them to seen, %d pulls answered same to seen not run in the model, %d refused once and sent again with the whole log), %d real requests, %d model pulls, %d copy reports and %d texts sent compared, %d copy reports left out (observation older or partial on one side), %d texts offered again from an older observation, %d texts repaired, %d records pending a permission decision checked; %d writes retried for id order, %d stopped",
		what, n, d.Round(time.Millisecond), p.c.pulls, p.c.failed, p.c.lost, p.c.removedServer, p.c.offerDropped, p.c.same,
		p.c.seenSame, p.c.seenSkipped, p.c.resent, p.c.requests, p.c.modelPulls, p.c.copies, p.c.texts, p.c.uncompared, p.c.reoffered,
		p.c.repaired, p.c.undecided, h.retries, h.stopped)
}

// [R3, R4, R5, RM1, RM2, RM3, D2, D3, D4, D5, T3, T5, N1] Every sequence of
// operations to a small depth over two founders and a machine admitted
// later, through the real exchange over the in-memory transport: sends,
// replies, withdraws, handled reports, reading; pulls both ways, with and
// without offers, with a page and offer limit of one and with the response
// lost; admitting the newcomer with and without history; removing a
// founder (only the admin knows until the log travels); a restart, a
// backup and a restore from it, sleep and wake, a clock set back and a
// 30-day jump. Every view, held record and copy report agrees with the
// model after every step.
func TestRealExchangeAgreesWithTheDeliveryModelOnEveryShortSchedule(t *testing.T) {
	if testing.Short() {
		t.Skip("exhaustive differential schedules")
	}
	start := time.Now()
	founders, later := pullMachines()
	h := newPullHarness(t, founders, later)
	for _, o := range []op{
		{kind: opSend, machine: "M", agent: "alice", to: "bob"},
		{kind: opSend, machine: "P", agent: "pat"},
		{kind: opPull, machine: "M", server: "P"},
		{kind: opBackup, machine: "P"},
	} {
		h = h.apply(o)
	}
	defer h.close()
	alpha, depth := pullAlphabet(), 2
	switch {
	case deepModel():
		depth = 3
	case raceDetector:
		// The race detector makes a state cost about 70 ms: keep the
		// operations that exercise the exchange (time budget, DESIGN rule).
		alpha = slices.DeleteFunc(alpha, func(o op) bool {
			switch o.kind {
			case opReply, opWithdraw, opSeenAll, opHandled, opBackup, opCrash, opSleep, opWake, opSkew, opAdvance:
				return true
			}
			if o.kind == opSend && o.machine == "P" {
				return true // the prefix wrote on both machines
			}
			return o.kind == opPull && o.machine == "P" && o.server == "N" || o.kind == opAdmit && o.n == 1
		})
	}
	n := exhaust(h, alpha, depth)
	h.px.report(t, fmt.Sprintf("depth %d over %d operations, states checked", depth, len(alpha)),
		time.Since(start), n, h.c)
	if h.c.stopped > 0 {
		t.Errorf("%d schedules stopped at an id-order tie", h.c.stopped)
	}
}

// [R3, R4, R5, RM1, RM2, RM3, D2 to D7, T3, T5, N1] Random schedules with a
// fixed seed over three founders and two machines admitted later, through
// the real exchange: everything the exhaustive search does, plus replaces,
// partial reads, clocks up to 10 hours apart, a server withholding a
// record, and more machines and steps.
func TestRealExchangeAgreesWithTheDeliveryModelOnRandomSchedules(t *testing.T) {
	start := time.Now()
	runs, steps := 6, 40
	if raceDetector {
		runs = 1
	}
	if testing.Short() {
		runs = 2
	}
	if deepModel() {
		runs, steps = 300, 60
	}
	founders := map[delivery.MachineID][]string{"M": {"alice", "bob"}, "P": {"pat"}, "Q": {"quinn"}}
	later := map[delivery.MachineID][]string{"N": {"nina"}, "O": {"otto"}}
	agents := map[delivery.MachineID][]string{}
	for k, v := range founders {
		agents[k] = v
	}
	for k, v := range later {
		agents[k] = v
	}
	r := rand.New(rand.NewPCG(20261124, 5))
	c := &counts{}
	pc := &pxCounts{}
	total := 0
	for i := 0; i < runs; i++ {
		h := newPullHarness(t, founders, later)
		h.c, h.px.c = c, pc
		for j := 0; j < steps && !h.stopped; j++ {
			for _, o := range randomPullOps(r, h, agents) {
				h = h.apply(o)
				total++
			}
		}
		h.close()
	}
	(&pxState{c: pc}).report(t, fmt.Sprintf("%d runs of %d steps, steps", runs, steps), time.Since(start), total, c)
	if c.stopped > 0 {
		t.Errorf("%d runs stopped at an id-order tie", c.stopped)
	}
}

// hasText says whether record id (implementation) is of a kind with text.
func (h *harness) hasText(id string) bool {
	for _, w := range h.writes {
		if w.impl == id {
			return w.kind == core.KindMessage || w.kind == core.KindReplace
		}
	}
	return false
}

// undecided says whether record x on machine n, not_admitted in the model
// and missing here, is so only because this machine does not hold a head of
// its admission frontier yet: a head is named by id, so until it arrives
// the machine cannot tell whether the record is before it, and holds it
// without text, undecided (access.go). The model knows positions. When the
// head arrives the record becomes not_admitted, and is compared again.
func (p *pxState) undecided(h *harness, n delivery.MachineID, x written, model, impl string) bool {
	if model != store.ReasonNotAdmitted || impl != store.ReasonMissing {
		return false
	}
	s := h.sims[n]
	rl, err := s.r.room(diffRoom)
	if err != nil {
		h.t.Fatal(err)
	}
	perm, member := rl.st.Permissions[s.m.ID]
	held := h.held(s, x.impl)
	var acc store.TextAccess
	if err := s.st.View(context.Background(), func(rd *store.Reader) error {
		if !member {
			// Removed: the frontier it had when removed still holds the
			// author back (Rooms.setFrontier).
			u, err := s.m.undecided(rd)
			if u[[2]string{diffRoom, held.Record.Author()}] {
				acc = store.TextUndecided
			}
			return err
		}
		v := s.m.newView(rd, diffRoom, time.Time{})
		if err := v.load(); err != nil {
			return err
		}
		var err error
		acc, err = textAccess(v, perm, held.Record.Author(), held.Record.Fields(), x.impl)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	if acc != store.TextUndecided {
		return false
	}
	// Pending, independently of the model: no text held, no agent's cursor
	// at or past it (its stream is held back, roomView.unseeded); it is
	// compared with the model again once the head arrives.
	if held.Record.Body() != nil {
		h.fail("%s: %s pending a permission decision holds text", n, h.id(x.impl))
	}
	f := held.Record.Fields()
	for _, a := range s.agents {
		var cs []store.Cursor
		if err := s.st.View(context.Background(), func(rd *store.Reader) (err error) {
			cs, err = rd.Cursors(a, diffRoom)
			return err
		}); err != nil {
			h.t.Fatal(err)
		}
		for _, c := range cs {
			if c.Author == held.Record.Author() && c.Epoch == f.Epoch && c.Pos >= f.Seq {
				h.fail("%s: %s's cursor passed %s, pending a permission decision", n, a, h.id(x.impl))
			}
		}
	}
	p.c.undecided++
	return true
}
