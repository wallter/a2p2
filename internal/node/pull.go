package node

// The pull exchange between machines (DESIGN section 4, PROTOCOL
// section 4; rules [R3], [R4], [R5], [RM1] to [RM3] of
// internal/model/delivery): the one request type that moves records and
// log entries, carried both ways. Serve is the server's side, in the order
// of PROTOCOL 4.4; Pull is the requester's, one whole exchange of
// rounds. Both work over Rooms and a PullTransport (internal/peer carries
// it over HTTP/2); waiting pulls are in waiting.go, the scheduler in
// sched.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/members"
	"github.com/wallter/a2p2/internal/store"
)

// PullTransport sends one pull request for a room to a peer, identified by
// its node id, and returns its response. A refused request is a
// *PullError. The transport authenticates the peer (pinned TLS, step 5b);
// the server learns the requester's node id from it, never from the body.
type PullTransport interface {
	Pull(ctx context.Context, peer string, req *PullRequest) (*PullResponse, error)
}

// ErrPeerNotMember: the peer is not a member of the room in this machine's
// view, so nothing is pulled from it, or what it sent is not taken [RM1].
var ErrPeerNotMember = &UserError{"the peer is not a member of the room here", ErrorNotAllowed}

// ExchangeOpts bounds what one request or response carries.
type ExchangeOpts struct {
	PageLimit  int // records per page; 0: MaxPageRecords
	OfferLimit int // records per offer; 0: MaxOfferRecords
	// Book is where peers' addresses are learned (addrs in requests,
	// peers in responses) and listed from; nil: none are.
	Book *AddrBook
	// Addrs are this machine's own addresses, sent in opening requests.
	Addrs []string
	// AddrsNow, if set, replaces Addrs: the addresses as they are at each
	// request (verified external ones come and go, DESIGN 11).
	AddrsNow func() []string
	// After is the timer of a held response; nil: time.After.
	After func(time.Duration) <-chan time.Time
}

// Exchange is the pull exchange of one machine.
type Exchange struct {
	r          *Rooms
	t          PullTransport
	pageLimit  int
	offerLimit int
	book       *AddrBook
	addrs      []string
	addrsNow   func() []string
	after      func(time.Duration) <-chan time.Time
	files      *Files      // set by UseFiles: wants in answers, files after pulls
	budget     *pullBudget // the requester's tokens per peer (budget.go)

	// In memory, per room and peer (PROTOCOL 4.5): refused, how the
	// peer last refused this machine as not a member; heard, the log heads
	// of the last request from it; seen, the peer's sum after the last
	// round with it that stored nothing, and this machine's then.
	mu      sync.Mutex
	refused map[[2]string]*refusal
	heard   map[[2]string][]string
	seen    map[[2]string]seenSums
	// resume is, per room and peer, the author whose detail an exchange
	// that ended Capped was taking: the next one starts there (round).
	resume map[[2]string]string
	// As a server, per room and peer: evidence, the held and refused log
	// entries sent that the peer cannot list in its extra (sent once until
	// its log heads change); pages, the streams of the last page and those
	// found blocked at the peer in its continuation (until its next
	// opening request).
	evidence map[[2]string]map[string]bool
	pages    map[[2]string]*pageMemory
	// made counts observations as they are made: a request built, or one
	// received. latest is, per room and peer, the count of the observation
	// stored last; a response to a request built before it is not stored
	// over it [R5]. In memory: no request outlives the process.
	made   int64
	latest map[[2]string]int64
	// waiting is, per room and peer, the held response now waiting; a new
	// one closes its channel, which releases it (waiting.go).
	waiting map[[2]string]chan struct{}
	// asked is, per room and peer, when its peers were last asked for.
	asked map[[2]string]time.Time
	// nextHold is, per room and peer, the channels to close at the next
	// hold registered (NextHold); closed, by Close: no more holds.
	nextHold map[[2]string][]chan struct{}
	closed   chan struct{}
	closing  sync.Once

	// Test hooks. atRelease runs where a held response is released (step
	// 10 of PROTOCOL 4.4; step 5b holds there); withhold makes a
	// server leave a record out of what it sends, as a faulty member would.
	atRelease func(room, peer string, q *PullRequest)
	withhold  func(id string) bool
}

// NewExchange returns the exchange of the machine behind r over t.
func NewExchange(r *Rooms, t PullTransport, o ExchangeOpts) *Exchange {
	x := &Exchange{r: r, t: t, pageLimit: o.PageLimit, offerLimit: o.OfferLimit, refused: map[[2]string]*refusal{},
		heard: map[[2]string][]string{}, seen: map[[2]string]seenSums{}, resume: map[[2]string]string{}, latest: map[[2]string]int64{},
		evidence: map[[2]string]map[string]bool{}, pages: map[[2]string]*pageMemory{},
		book: o.Book, addrs: slices.Clone(o.Addrs), addrsNow: o.AddrsNow, after: o.After, waiting: map[[2]string]chan struct{}{},
		asked: map[[2]string]time.Time{}, nextHold: map[[2]string][]chan struct{}{}, closed: make(chan struct{}),
		budget: newPullBudget(PullBudgetRate, PullBudgetBurst)}
	if x.after == nil {
		x.after = time.After
	}
	if len(x.addrs) > MaxAddrs {
		x.addrs = x.addrs[:MaxAddrs]
	}
	if x.pageLimit <= 0 || x.pageLimit > MaxPageRecords {
		x.pageLimit = MaxPageRecords
	}
	if x.offerLimit <= 0 || x.offerLimit > MaxOfferRecords {
		x.offerLimit = MaxOfferRecords
	}
	return x
}

// ---- summaries ----

// summary is one machine's two-level summary of a room: the authors it
// takes (its Admitted set [R3]), the detail of each it holds anything of,
// and their digests.
type summary struct {
	authors []string
	detail  map[string][]store.StreamDetail
	digests map[string]string
}

// summarize builds this machine's summary of v's room under membership st.
// A missing text is wanted only if this machine's permission lets it hold
// it now: undecided text (a frontier head not held) is not asked for, so
// no page slot goes on text that would not be taken.
func (r *Rooms) summarize(v *roomView, st members.State) (summary, error) {
	s := summary{authors: slices.Sorted(slices.Values(st.Admitted)), detail: map[string][]store.StreamDetail{},
		digests: map[string]string{}}
	rows, err := v.rd.Summary(v.room, v.now)
	if err != nil {
		return s, err
	}
	perm, member := st.Permissions[r.m.ID]
	for _, row := range rows {
		if _, ok := slices.BinarySearch(s.authors, row.Author); !ok {
			continue
		}
		var wanted []int64
		for i, n := range row.Missing {
			if !member && row.Author != r.m.ID {
				break
			}
			x, err := v.get(row.MissingIDs[i])
			if err != nil {
				return s, err
			}
			if x == nil {
				continue
			}
			acc := store.TextReadable
			if row.Author != r.m.ID {
				if acc, err = textAccess(v, perm, x.author, x.f, x.id); err != nil {
					return s, err
				}
			}
			if acc == store.TextReadable {
				wanted = append(wanted, n)
			}
		}
		s.detail[row.Author] = append(s.detail[row.Author], streamDetail(row.Epoch, row.Contig, row.NoBody, wanted))
	}
	for a, d := range s.detail {
		s.digests[a] = digestOf(d)
	}
	return s, nil
}

// streamDetail is one stream's detail from the positions held without
// text (ascending) and those wanted: the floor x is the longest run from 1
// of positions without text and not wanted; nb and w list what is above it.
func streamDetail(epoch string, c int64, noBody, wanted []int64) store.StreamDetail {
	d := store.StreamDetail{E: epoch, C: c, W: wanted}
	i := 0
	for i < len(noBody) && noBody[i] == d.X+1 && (len(wanted) == 0 || wanted[0] != noBody[i]) {
		d.X++
		i++
	}
	if i < len(noBody) {
		d.NB = slices.Clone(noBody[i:])
	}
	return d
}

// sum is the short form of s (PROTOCOL 4.3).
func (s summary) sum() string { return sumOf(s.digests) }

// rows is s as an observation: what a peer holds when its summary equals s
// (an author of s held nothing of has detail []).
func (s summary) rows() []store.Observation {
	var out []store.Observation
	for _, a := range s.authors {
		out = append(out, store.Observation{Author: a, Digest: s.digests[a], Detail: s.detailOf(a)})
	}
	return out
}

// detailOf is author's detail, [] when nothing is held.
func (s summary) detailOf(author string) []store.StreamDetail {
	if d, ok := s.detail[author]; ok {
		return d
	}
	return []store.StreamDetail{}
}

// ---- pages ----

// pageItem is one record of a page or an offer, with or without its text.
type pageItem struct {
	r    *rec
	text bool
}

func (it pageItem) wire() []byte {
	if it.text {
		return it.r.h.Record.Wire()
	}
	return it.r.h.Record.WithoutBody().Wire()
}

// buildPage is what this machine sends a peer whose detail is want, for
// the given authors (the model's page [R3]): text the peer wants first,
// then every record past the peer's position in each stream, predecessors
// and links first, stopping a stream at anything not sent, with text only
// where readable (the peer's permission as this machine sees it) says so.
// Records in skip were sent already and count as held by the peer;
// streams in blocked are left out. It returns at most limit records and
// maxBytes of wire objects, and whether it cut.
func (x *Exchange) buildPage(v *roomView, readable func(*rec) (bool, error), want map[string][]store.StreamDetail,
	authors []string, skip map[string]bool, blocked map[streamKey]bool, limit, maxBytes int) ([]pageItem, bool, error) {
	reqC := func(k streamKey) int64 {
		for _, s := range want[k.author] {
			if s.E == k.epoch {
				return s.C
			}
		}
		return 0
	}
	var streams []streamKey
	for _, s := range v.streams {
		if k := (streamKey{s.Author, s.Epoch}); s.Contig > 0 && slices.Contains(authors, s.Author) && !blocked[k] {
			streams = append(streams, k)
		}
	}
	slices.SortFunc(streams, func(a, b streamKey) int {
		if a.author != b.author {
			return compareStr(a.author, b.author)
		}
		return compareStr(a.epoch, b.epoch)
	})
	withheld := func(id string) bool { return x.withhold != nil && x.withhold(id) }
	textOf := func(h *rec) (bool, error) {
		if h.reason != store.ReasonLive {
			return false, nil
		}
		if k := h.f.Kind; k != core.KindMessage && k != core.KindReplace {
			return true, nil
		}
		return readable(h)
	}
	var out []pageItem
	for _, k := range streams {
		for _, s := range want[k.author] {
			if s.E != k.epoch {
				continue
			}
			for _, n := range s.W {
				h, err := v.at(k, n)
				if err != nil {
					return nil, false, err
				}
				if h == nil || h.reason != store.ReasonLive || withheld(h.id) || skip[h.id] {
					continue
				}
				ok, err := readable(h)
				if err != nil {
					return nil, false, err
				}
				if ok {
					out = append(out, pageItem{h, true})
				}
			}
		}
	}
	per := map[streamKey][]pageItem{}
	inPage := map[string]bool{}
	for _, k := range streams {
		for n := reqC(k) + 1; n <= v.contig[k]; n++ {
			h, err := v.at(k, n)
			if err != nil {
				return nil, false, err
			}
			if h == nil || withheld(h.id) {
				break
			}
			if skip[h.id] {
				continue
			}
			text, err := textOf(h)
			if err != nil {
				return nil, false, err
			}
			per[k] = append(per[k], pageItem{h, text})
			inPage[h.id] = true
		}
	}
	// Within a stream by position; a stream's first record only after the
	// records it links to when they are in the page too. One whose link the
	// peer lacks and the page does not carry is left out.
	idx := map[streamKey]int{}
	emitted := map[string]bool{}
	for id := range skip {
		emitted[id] = true
	}
	for progress := true; progress; {
		progress = false
		for _, k := range streams {
			for idx[k] < len(per[k]) {
				it := per[k][idx[k]]
				ready := true
				for _, l := range it.r.f.Links {
					if !emitted[l.ID] && (inPage[l.ID] || l.Seq > reqC(streamKey{k.author, l.Epoch})) {
						ready = false
					}
				}
				if !ready {
					break
				}
				out = append(out, it)
				emitted[it.r.id] = true
				idx[k]++
				progress = true
			}
		}
	}
	size := 0
	for i, it := range out {
		if i == limit {
			return out[:i], true, nil
		}
		if size += len(it.wire()) + 1; size > maxBytes {
			return out[:i], true, nil
		}
	}
	return out, false, nil
}

func compareStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// readableBy says whether perm (a peer's permission as this machine sees
// it) lets the peer have a record's text: undecided is no.
func readableBy(v *roomView, perm members.Permission) func(*rec) (bool, error) {
	return func(h *rec) (bool, error) {
		a, err := textAccess(v, perm, h.author, h.f, h.id)
		return a == store.TextReadable, err
	}
}

// ---- the room log ----

// logHeads are rl's accepted entries no accepted entry names as parent,
// and its held and refused entries (PROTOCOL 4.1).
func (rl *roomLog) logHeads() (heads, extra []string) {
	parent := map[string]bool{}
	for id, row := range rl.state {
		if row.State == store.LogAccepted {
			for _, p := range rl.ents[id].Fields().Parents {
				parent[p] = true
			}
		}
	}
	for _, id := range sortedIDs(rl.state) {
		switch rl.state[id].State {
		case store.LogAccepted:
			if !parent[id] {
				heads = append(heads, id)
			}
		case store.LogHeld, store.LogRefused:
			extra = append(extra, id)
		}
	}
	if len(heads) > MaxLogHeads {
		heads = heads[:MaxLogHeads]
	}
	if len(extra) > maxLogExtra {
		extra = extra[:maxLogExtra]
	}
	return heads, extra
}

// entriesFor returns, as rl passes them on (accepted in causal order, then
// held and refused as evidence), the entries not at or before heads and
// not in extra, at most MaxLogEntries and maxBytes of wire objects, and
// whether more were left out. With known false (nothing is known of the
// peer's log) every entry goes. Evidence goes only if evidence says so
// (nil: all of it); unlisted says whether the peer could hold it without
// listing it (its extra is full, or its log is not known), and the ids of
// such evidence sent are returned, for the caller to send once.
func (rl *roomLog) entriesFor(heads, extra []string, known bool, maxBytes int,
	evidence func(id string, unlisted bool) bool) (out []json.RawMessage, more bool, unlisted []string) {
	have := map[string]bool{}
	full := !known || len(extra) >= maxLogExtra
	var top string // the highest id the peer listed
	if known {
		todo := slices.Clone(heads)
		for len(todo) > 0 {
			id := todo[len(todo)-1]
			todo = todo[:len(todo)-1]
			v := rl.ents[id]
			if have[id] || v == nil {
				continue
			}
			have[id] = true
			todo = append(todo, v.Fields().Parents...)
		}
		for _, id := range extra {
			have[id] = true
			top = max(top, id)
		}
	}
	size := 0
	for _, v := range rl.mach.Offer() {
		id := v.ID()
		if have[id] {
			continue
		}
		ev := rl.state[id].State != store.LogAccepted
		un := ev && full && (!known || id > top)
		if ev && evidence != nil && !evidence(id, un) {
			continue
		}
		w := v.Wire()
		if size += len(w) + 1; len(out) == MaxLogEntries || size > maxBytes {
			return out, true, unlisted
		}
		out = append(out, json.RawMessage(w))
		if un {
			unlisted = append(unlisted, id)
		}
	}
	return out, false, unlisted
}

// acceptedOnly is an evidence filter that sends none.
func acceptedOnly(string, bool) bool { return false }

func rawBytes(xs []json.RawMessage) [][]byte {
	out := make([][]byte, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

// ---- placing records ----

// placed counts what placing a page or an offer did; blocked are the
// streams of records that could not be stored (refused, or behind one
// that was).
type placed struct {
	stored, repaired int
	blocked          map[streamKey]bool
}

// place checks and places each received record wire (PROTOCOL
// section 2) inside tx under membership st, counting refusals by code.
func (r *Rooms) place(tx *store.Tx, rl *roomLog, st members.State, look func(string) *core.VerifiedEntry,
	wires []json.RawMessage, refused map[string]int, now time.Time) (placed, error) {
	p := placed{blocked: map[streamKey]bool{}}
	for _, w := range wires {
		rec, err := core.ParseRecord(w)
		if err != nil {
			refused[refusalCodeOr(err, "bad_wire")]++
			continue
		}
		a := rec.Author()
		k := streamKey{a, rec.Fields().Epoch}
		if rec.Fields().Room != rl.id {
			refused[CodeWrongRoom]++
			continue
		}
		if a != r.m.ID && !slices.Contains(st.Admitted, a) {
			refused[string(store.OutcomeAuthorNotAdmitted)]++
			continue
		}
		v, err := rec.Verified()
		if err != nil {
			refused[CodeBadSignature]++
			p.blocked[k] = true
			continue
		}
		out, err := tx.PutRecordPlanned(v, 0, r.recordPlan(st, look, v, now))
		if err != nil {
			return p, err
		}
		switch out {
		case store.OutcomeStored:
			p.stored++
		case store.OutcomeRepaired:
			p.repaired++
		case store.OutcomeHeld:
		default: // fork, future, not_next, link_missing
			refused[string(out)]++
			p.blocked[k] = true
		}
	}
	return p, nil
}

func refusalCodeOr(err error, def string) string {
	if c := core.RefusalCode(err); c != "" {
		return c
	}
	return def
}

func refuseAll(tx *store.Tx, room, peer string, refused map[string]int, now time.Time) error {
	for _, code := range sortedIDs(refused) {
		if err := tx.Refuse(room, peer, code, refused[code], now); err != nil {
			return err
		}
	}
	return nil
}

// countInvalid counts, as refusals of peer, the log entries it sent that
// were invalid (bad bytes, signature or room). Held and refused entries are
// fork evidence that members pass on, not faults of the sender.
func (r *Rooms) countInvalid(ctx context.Context, rl *roomLog, peer string, res []EntryResult, now time.Time) error {
	refused := map[string]int{}
	for _, x := range res {
		if x.Disposition == members.Invalid {
			refused["entry_"+x.Reason]++
		}
	}
	if len(refused) == 0 {
		return nil
	}
	_, err := r.m.st.Exchange(ctx, rl.id, 0, func(tx *store.Tx) error {
		return refuseAll(tx, rl.id, peer, refused, now)
	})
	return err
}

// ---- observations ----

// observation builds the rows recording what a peer reported: for each
// author it takes, its digest and detail. asked are the authors whose
// digests it was asked for; one of them without a digest holds nothing. A
// detail is known when the peer sent it (and it matches the digest), when
// the digest equals this machine's own (so the summaries are equal), or
// when the digest is the one stored before with its detail; otherwise it
// is unknown until sent (PROTOCOL 4.3, item 4).
func observation(takes, asked []string, digests map[string]string, detail map[string][]store.StreamDetail,
	own summary, old map[string]store.Observation) []store.Observation {
	var out []store.Observation
	for _, a := range takes {
		o := store.Observation{Author: a}
		d, held := digests[a]
		sent, hasSent := detail[a]
		switch {
		case !held && !slices.Contains(asked, a):
			// It takes the author, but its digest was not asked for.
		case !held && len(sent) == 0:
			o.Detail = []store.StreamDetail{}
		case !held:
			// A detail without a digest: not believed.
		case hasSent && len(sent) > 0 && digestOf(sent) == d:
			o.Digest, o.Detail = d, sent
		case own.digests[a] == d:
			o.Digest, o.Detail = d, own.detail[a]
		case old[a].Digest == d && old[a].Detail != nil:
			o.Digest, o.Detail = d, old[a].Detail
		default:
			o.Digest = d
		}
		out = append(out, o)
	}
	return out
}

// detailRows are the rows a sent detail says: per author, what it holds
// ([] and no digest when nothing).
func detailRows(detail map[string][]store.StreamDetail) []store.Observation {
	var out []store.Observation
	for _, a := range sortedIDs(detail) {
		o := store.Observation{Author: a, Detail: detail[a]}
		if len(o.Detail) > 0 {
			o.Digest = digestOf(o.Detail)
		} else {
			o.Detail = []store.StreamDetail{}
		}
		out = append(out, o)
	}
	return out
}

func observedMap(obs []store.Observation) (map[string]store.Observation, *store.PeerLog) {
	m := map[string]store.Observation{}
	var log *store.PeerLog
	for _, o := range obs {
		if o.Author == "" {
			log = o.Log
			continue
		}
		m[o.Author] = o
	}
	return m, log
}

// next returns the count of a new observation.
func (x *Exchange) next() int64 {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.made++
	return x.made
}

// later reports whether an observation counted n is later than the one
// stored for room and peer.
func (x *Exchange) later(room, peer string, n int64) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return n > x.latest[[2]string{room, peer}]
}

// stored records that the observation counted n is stored. r.mu is held
// from later to stored, so no other observation is stored between.
func (x *Exchange) stored(room, peer string, n int64) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if k := [2]string{room, peer}; n > x.latest[k] {
		x.latest[k] = n
	}
}

// observe writes the rows, an observation dated at, and notes the known
// position of every stream a known detail reports for an author admitted
// here, as the model's observe does: a hint, which the same peer's later
// report lowers. whole replaces what was observed of
// the peer; otherwise only the rows given (and the log) are.
func observe(tx *store.Tx, room, peer string, st members.State, rows []store.Observation, log store.PeerLog, at time.Time,
	whole bool) error {
	all := append(slices.Clone(rows), store.Observation{Author: "", Log: &log})
	write := tx.ObservePart
	if whole {
		write = tx.Observe
	}
	if err := write(room, peer, at, all); err != nil {
		return err
	}
	for _, o := range rows {
		if !slices.Contains(st.Admitted, o.Author) || o.Detail == nil {
			continue
		}
		known := map[string]int64{}
		for _, s := range o.Detail {
			known[s.E] = s.C
		}
		if err := tx.NoteKnown(room, peer, o.Author, known, at); err != nil {
			return err
		}
	}
	return nil
}

// ---- the server ----

// served is what steps 4 to 6 of 4.4 found, for step 8: this machine's
// summary at store version ver, and whether the request's log entries or
// offer stored anything.
type served struct {
	own    summary
	ver    int64
	ok     bool
	stored bool
}

// built is a response as step 8 built it, and what it was built from.
type built struct {
	rl       *roomLog
	gen      int64
	ver      int64 // store.Changes the response reflects
	sv       served
	resp     *PullResponse
	items    []pageItem
	blocked  map[streamKey]bool // streams left out of the page
	unlisted []string           // evidence sent the peer could hold unlisted
	held     bool               // released after a hold: built again
}

// expiresBy says whether text chosen for the page has passed its
// retention time by now.
func (b *built) expiresBy(now time.Time) bool {
	for _, it := range b.items {
		if it.text && hasText(it.r.f.Kind) && store.TextExpired(it.r.f.Created, now) {
			return true
		}
	}
	return false
}

// Answers to an opening request.
const (
	answerDigests = iota // authors, digests and sum
	answerSame           // same: the summaries are equal now
	answerSeen           // same: nothing changed since a round that stored nothing
)

// answer decides the answer to the opening request q (PROTOCOL 4.3):
// same if the request was quiet (its entries and offer stored nothing,
// and this machine has no log entry the requester lacks and evidence lets
// it send) and this machine's sum equals the request's sum, or its seen;
// otherwise the digests. A waiting pull (step 5b) is held exactly while
// this is same.
func answer(rl *roomLog, q *PullRequest, own summary, stored bool, evidence func(string, bool) bool) int {
	if stored {
		return answerDigests
	}
	if es, _, _ := rl.entriesFor(q.Log.Heads, q.Log.Extra, true, maxResponseBytes, evidence); len(es) > 0 {
		return answerDigests
	}
	switch own.sum() {
	case q.Sum:
		return answerSame
	case q.Seen:
		return answerSeen
	}
	return answerDigests
}

// Serve answers one pull request from peer, the node id the transport
// authenticated, in the order of PROTOCOL 4.4: the request's log
// entries (from a non-member only if they make it a member), the peer a
// member now, its offer and what it said it holds stored, all in one
// transaction; then the response; then, at the point where a held
// response is released, the room's generation and the peer's membership
// checked again and the response built again if anything was written
// since [RM2].
func (x *Exchange) Serve(ctx context.Context, peer string, q *PullRequest) (*PullResponse, error) {
	if perr := q.validate(); perr != nil {
		return nil, perr
	}
	if !core.ValidNode(peer) || peer == x.r.m.ID {
		return nil, notMember()
	}
	if x.isClosed() {
		return nil, pullErr(503, CodeUnavailable, "closing")
	}
	b, err := x.serve(ctx, peer, q)
	if err != nil {
		return nil, err
	}
	if q.WaitMS > 0 && b.resp.Same && (x.files == nil || !x.files.wantsDue(ctx, q.Room, peer)) {
		// Step 9: held while the answer would stay same (waiting.go), and
		// not at all while a want for the puller is due (PROTOCOL 10.4).
		if b, err = x.hold(ctx, peer, q, b); err != nil {
			return nil, err
		}
		b.held = true
	}
	if x.atRelease != nil {
		x.atRelease(q.Room, peer, q)
	}
	return x.release(ctx, peer, q, b)
}

func unavailable(err error) error {
	var pe *PullError
	if errors.As(err, &pe) {
		return pe
	}
	return pullErr(503, CodeUnavailable, "")
}

func isNotMember(err error) bool {
	var pe *PullError
	return errors.As(err, &pe) && pe.Code == CodeNotMember
}

func (x *Exchange) serve(ctx context.Context, peer string, q *PullRequest) (*built, error) {
	r := x.r
	if err := r.m.pruneIfDue(ctx); err != nil {
		return nil, unavailable(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rl := r.rooms[q.Room]
	if rl == nil {
		return nil, notMember()
	}
	x.hear(q.Room, peer, q.Log.Heads, slices.Contains(rl.st.Members, peer))
	now := r.m.now()
	if x.book != nil && len(q.Addrs) > 0 && slices.Contains(rl.st.Members, peer) {
		x.book.Replace(peer, q.Addrs, now) // its own word on where it is, from the authenticated peer
	}
	order := x.next() // received now: later than anything stored
	wasMember := slices.Contains(rl.st.Members, peer)
	member := false
	var sv served
	refused := map[string]int{}
	commit := func(ctx context.Context, b *store.LogBatch, st members.State, look func(string) *core.VerifiedEntry) (int64, error) {
		member = slices.Contains(st.Members, peer)
		if !member && !wasMember {
			return 0, notMember() // T4: a non-member's entries count only if they make it a member
		}
		var n int64
		ver, err := r.m.st.Exchange(ctx, rl.id, rl.gen, func(tx *store.Tx) error {
			if b != nil {
				var err error
				if n, err = tx.ApplyLog(*b); err != nil {
					return err
				}
			}
			if !member {
				return nil
			}
			pl, err := r.place(tx, rl, st, look, q.Offer, refused, now)
			if err != nil {
				return err
			}
			v := r.m.newView(tx.Reader(), rl.id, now)
			if err := v.load(); err != nil {
				return err
			}
			own, err := r.summarize(v, st)
			if err != nil {
				return err
			}
			sv = served{own: own, ok: true, stored: n > 0 || pl.stored+pl.repaired > 0}
			plog := store.PeerLog{Heads: q.Log.Heads, Extra: q.Log.Extra}
			switch {
			case q.Sum == "":
				// What it holds of the authors of its detail.
				err = observe(tx, rl.id, peer, st, detailRows(q.Detail), plog, now, false)
			case answer(rl, q, own, sv.stored, x.evidenceFor(rl.id, peer, false)) == answerSame:
				// Its digests equal this machine's, as of now.
				err = observe(tx, rl.id, peer, st, own.rows(), plog, now, true)
			default:
				err = tx.ObserveLog(rl.id, peer, now, plog)
			}
			if err != nil {
				return err
			}
			return refuseAll(tx, rl.id, peer, refused, now)
		})
		sv.ver = ver
		if err == nil && member {
			x.stored(rl.id, peer, order)
		}
		return n, err
	}
	res, err := r.ingestWith(ctx, rl, 0, rawBytes(q.Log.Entries), nil, commit)
	if err != nil {
		if isNotMember(err) {
			return nil, notMember()
		}
		return nil, unavailable(err)
	}
	if err := r.countInvalid(ctx, rl, peer, res, now); err != nil {
		return nil, unavailable(err)
	}
	if !member {
		return nil, notMember()
	}
	if sv.stored {
		r.m.wakes.touch(rl.id)
		if x.files != nil {
			x.files.Soon(rl.id) // manifests of headers the request brought (PROTOCOL 10.2)
		}
	}
	b, err := x.respond(ctx, rl, peer, q, sv)
	if err != nil {
		return nil, unavailable(err)
	}
	return b, nil
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := slices.Clone(a)
	y := slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

func jsonSize(v any) int {
	b, _ := json.Marshal(v)
	return len(b)
}

func wiresSize(ws []json.RawMessage) int {
	n := 0
	for _, w := range ws {
		n += len(w) + 1
	}
	return n
}

// wireMargin is left free in every budget for separators and field names
// the size of a message without its lists does not count.
const wireMargin = 1 << 10

// respond is step 8: to an opening request, same or this machine's
// authors, digests and sum with the log entries the peer lacks; to a
// continuing one, this machine's detail for the request's detail authors
// and their page, with log entries; all within the response limit. sv is
// a summary to reuse if nothing was written since it was made. r.mu is
// held.
func (x *Exchange) respond(ctx context.Context, rl *roomLog, peer string, q *PullRequest, sv served) (*built, error) {
	r := x.r
	st := rl.st
	heads, extra := rl.logHeads()
	b := &built{rl: rl, gen: rl.gen, ver: r.m.st.Changes(), sv: sv}
	resp := &PullResponse{V: PullVersion, Log: PullLog{Heads: heads, Extra: extra}}
	b.resp = resp
	entries := func(budget int) {
		resp.Log.Entries, resp.Log.More, b.unlisted = rl.entriesFor(q.Log.Heads, q.Log.Extra, true, budget,
			x.evidenceFor(rl.id, peer, false))
	}
	own := sv.own
	if q.Sum != "" {
		if !sv.ok || sv.ver != b.ver {
			if err := r.m.view(ctx, rl.id, func(v *roomView) (err error) {
				own, err = r.summarize(v, st)
				return err
			}); err != nil {
				return nil, err
			}
		}
		if answer(rl, q, own, sv.stored, x.evidenceFor(rl.id, peer, false)) != answerDigests {
			resp.Same = true
			return b, nil
		}
		resp.Authors = slices.Sorted(slices.Values(st.Admitted))
		if len(own.digests) > 0 {
			resp.Digests = own.digests
		}
		resp.Sum = own.sum()
		entries(maxResponseBytes - jsonSize(resp) - wireMargin)
		return b, nil
	}
	err := r.m.view(ctx, rl.id, func(v *roomView) error {
		own, err := r.summarize(v, st)
		if err != nil {
			return err
		}
		// Detail first, in the order asked: the first author's in the
		// whole budget, the others' while the detail stays within half of
		// it, so the page has room; the rest is left out for the requester
		// to ask again (4.3 step 5).
		resp.Detail = map[string][]store.StreamDetail{}
		budget := maxResponseBytes - jsonSize(resp) - wireMargin
		whole := budget
		var authors []string
		for i, a := range sortedIDs(q.Detail) {
			d := own.detailOf(a)
			n := jsonSize(d) + len(a) + 4
			if n > budget || i > 0 && whole-budget+n > whole/2 {
				break
			}
			resp.Detail[a], budget = d, budget-n
			if slices.Contains(st.Admitted, a) {
				authors = append(authors, a)
			}
		}
		entries(budget / 2)
		budget -= wiresSize(resp.Log.Entries)
		var cut bool
		b.blocked = x.blockedFor(rl.id, peer, q)
		b.items, cut, err = x.buildPage(v, readableBy(v, st.Permissions[peer]), q.Detail, authors, nil,
			b.blocked, x.pageLimit, budget)
		resp.More = cut
		return err
	})
	if err != nil {
		return nil, err
	}
	return b, nil
}

// release is step 10: the room must be the one the response was built in
// (not left and joined again) and the peer still a member; if anything was
// written since, or text chosen for the page has passed its retention
// time, the response is built again (prune, log, digests, page), so no
// text withdrawn, voided, expired or no longer permitted meanwhile goes
// out.
func (x *Exchange) release(ctx context.Context, peer string, q *PullRequest, b *built) (*PullResponse, error) {
	r := x.r
	if err := r.m.pruneIfDue(ctx); err != nil {
		return nil, unavailable(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rl := r.rooms[q.Room]
	switch {
	case rl == nil, !slices.Contains(rl.st.Members, peer):
		return nil, notMember() // membership first, in whatever generation is held now
	case rl != b.rl || rl.gen != b.gen:
		return nil, pullErr(503, CodeUnavailable, "room changed")
	}
	if b.held || r.m.st.Changes() != b.ver || b.expiresBy(r.m.now()) {
		// Built again: after a hold always (text can pass its retention
		// time with nothing written), otherwise if anything was written
		// or text on the page has expired since it was built.
		sv := b.sv
		sv.ok = false
		nb, err := x.respond(ctx, rl, peer, q, sv)
		if err != nil {
			return nil, unavailable(err)
		}
		b = nb
	}
	resp := b.resp
	resp.Page = nil
	for _, it := range b.items {
		resp.Page = append(resp.Page, json.RawMessage(it.wire()))
	}
	resp.Peers = nil
	if q.Sum != "" && q.Peers && x.book != nil {
		// Asked for: in a same answer as in a digests one, never otherwise.
		// Other members with addresses, within the response limit.
		var others []string
		for _, n := range rl.st.Members {
			if n != peer && n != r.m.ID {
				others = append(others, n)
			}
		}
		if ps := x.book.peers(others); len(ps) > 0 && jsonSize(resp)+jsonSize(ps)+wireMargin <= maxResponseBytes {
			resp.Peers = ps
		}
	}
	resp.Want = nil
	if x.files != nil {
		// Wants of the puller's files when it cannot be dialled, in what
		// the answer leaves of its limit (PROTOCOL 10.4).
		resp.Want = x.files.wantsFor(ctx, rl.id, peer, maxResponseBytes-jsonSize(resp)-wireMargin-16)
	}
	// What goes out is noted: evidence the peer cannot list, once; the
	// page's streams, to find those blocked at the peer.
	note := x.evidenceFor(rl.id, peer, true)
	for _, id := range b.unlisted {
		note(id, true)
	}
	x.paged(rl.id, peer, q, b.blocked, b.items)
	return resp, nil
}

func hasText(k core.Kind) bool { return k == core.KindMessage || k == core.KindReplace }

// ---- the requester ----

// PullOpts are the requester's choices for one exchange.
type PullOpts struct {
	// Offer attaches, to each request, records the peer lacks by its last
	// observed summary [RM3]; none without one.
	Offer bool
	// OfferLimit, if above 0 and below the exchange's, bounds the records
	// of one offer (the scheduler halves it after a 413).
	OfferLimit int
	// WaitMS, above 0, makes the exchange's first request a waiting pull
	// (PROTOCOL 4.4 step 9) if it gives the peer nothing (no records, no
	// log entries): the server holds a same answer up to it. A request
	// that gives something never waits, so its answer comes at once.
	WaitMS int64
	// Stop, closed while that first request waits for its token or its
	// answer, abandons it: Pull returns ErrWaitStopped (a local change
	// re-issues it with an offer). A request that gives something is
	// never abandoned for it.
	Stop <-chan struct{}
}

// ErrWaitStopped: Pull's waiting request was abandoned because Stop closed.
var ErrWaitStopped = errors.New("pull: waiting request stopped")

// PullStats says what one exchange did.
type PullStats struct {
	Requests  int // requests sent
	Responses int // responses applied
	Rounds    int
	Stored    int  // records stored from pages
	Repaired  int  // texts repaired from pages
	Entries   int  // responses whose log entries changed the room log
	Offered   int  // records offered
	Given     int  // log entries given
	Same      int  // rounds answered same
	SeenSame  int  // of them, to a request that carried seen
	Closed    bool // the exchange ended with a same to a request without seen
	Stalled   int  // continuations ended because they did not advance
	TooLarge  int  // authors whose detail alone did not fit in a request
	Capped    bool // ended at pullRoundsPerPull rounds or pullMaxRequests requests

	// heads are the log heads the peer advertised in its last response.
	heads []string
	// relog: the peer's last answer removed it here and its log has more
	// (round): the next request only reads the rest of the log.
	relog bool
	// resume is the first author of the detail being taken (round).
	resume string
}

// refusal is how a peer last refused this machine as not a member: full,
// the next opening request sends the whole accepted log (once); muted, it
// was refused that too, and openings carry no entries until this
// machine's log heads (own) or those heard from the peer change.
type refusal struct {
	full, muted bool
	own, heard  []string
	since       time.Time // the first not_member of this run of refusals
}

// seenSums is a peer's sum, and this machine's, after a round with it
// that stored nothing on either side and refused, skipped or cut nothing;
// for the room's generation gen, from at, for at most seenFor.
type seenSums struct {
	server, own string
	gen         int64
	at          time.Time
}

// seenFor bounds how long seen is kept: a safety net against a change
// that moves neither hash.
const seenFor = time.Hour

// Pull runs one exchange with peer for room: rounds of an opening request
// with this machine's sum (and seen, an offer and the log entries the peer
// lacks), answered same or with the peer's digests; then continuing
// requests carrying this machine's detail for the authors whose digests
// differ, 16 at a time within the request limit, each answered with a
// page, repeated while something was cut and the continuation advances. A
// round that stored nothing on either side ends the exchange, and the
// peer's sum is remembered as seen (PROTOCOL 4.3). Each response is
// applied in one transaction, only if the room's generation is unchanged:
// its log entries, then its page under the gap-free rule [R4] if the peer
// is still a member here [RM1], then what the peer said it holds [R5]. A
// refusal is returned as it is: the scheduler decides when to try again.
func (x *Exchange) Pull(ctx context.Context, room, peer string, o PullOpts) (PullStats, error) {
	var st PullStats
	gen, err := x.r.Gen(room)
	if err != nil {
		return st, err
	}
	sent := map[string]bool{} // records and log entries given to the peer in this exchange
	if x.files != nil {
		// what the exchange stored may end files here [F1], and the
		// manifests of headers it brought are fetched now (PROTOCOL 10.2)
		defer func() {
			x.files.sweepRoom(context.WithoutCancel(ctx), room)
			if st.Stored > 0 {
				x.files.Soon(room)
			}
		}()
	}
	for st.Rounds < pullRoundsPerPull {
		st.Rounds++
		progress, err := x.round(ctx, room, peer, gen, o, &st, sent)
		if errors.Is(err, errPullCapped) {
			// What was applied is kept; the next exchange goes on, its
			// detail from where this one stopped.
			st.Capped = true
			x.setResume(room, peer, st.resume)
			return st, nil
		}
		if err != nil {
			return st, err
		}
		if !progress {
			x.pulled(room, peer, gen, st.heads)
			return st, nil
		}
	}
	// Only a peer changing all the time gets here: counted, not an error.
	st.Capped = true
	x.pulled(room, peer, gen, st.heads)
	return st, nil
}

// pulled marks the room caught up (maySign) after an exchange with peer,
// a current member, in which every log head it advertised last is
// accepted here: a peer that names newer heads and does not hand them
// over, or hands over entries that are not accepted, unlocks nothing.
func (x *Exchange) pulled(room, peer string, gen int64, heads []string) {
	x.r.caughtUp(room, peer, gen, heads)
}

// caughtUp marks room caught up with peer's log if peer is a current
// member and every one of heads (what it advertised) is accepted here.
func (r *Rooms) caughtUp(room, peer string, gen int64, heads []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rl := r.rooms[room]
	if rl == nil || gen != 0 && rl.gen != gen || len(heads) == 0 || !slices.Contains(rl.st.Members, peer) {
		return
	}
	for _, h := range heads {
		if rl.state[h].State != store.LogAccepted {
			return
		}
	}
	rl.pulled = true
}

// prepared is one request and what it was built from.
type prepared struct {
	req      *PullRequest
	own      summary
	old      map[string]store.Observation
	at       time.Time       // when it was built: the date of what its response tells
	order    int64           // and its count among observations (Exchange.next)
	base     int             // its size without entries and offer
	heads    []string        // the peer's log heads it was built against (nil: not known)
	offered  map[string]bool // authors of the records offered
	offerCut bool
	logCut   bool
	relog    bool // only reads the rest of the log of a peer removed here (prepare)
}

// prepare builds an opening request (with seen if useSeen and still
// valid) or a continuing one with detail for chunk and snap; with an offer
// if offer is set (built against base, the peer's summary as observed when
// the round began, less what sent holds: one offer for the round, sent in
// pages); and with the log entries not at or before peerLog (the peer's
// heads; nil: as last observed), evidence the peer could hold unlisted at
// most once in the exchange; or after a refusal as refused says (the
// accepted log from the start, then nothing; never an offer); all within
// the request limit: detail first, then entries, then the offer. With
// relog, the peer's last answer removed it here and said its log has more
// (round): the request goes to it although it is not a member and only
// reads the rest of its log. It is an opening with the sum of an empty
// summary and this machine's log heads and extra: no seen, addresses,
// peers, entries, detail or offer, so the removed peer learns nothing
// more of this machine.
func (x *Exchange) prepare(ctx context.Context, room, peer string, gen int64, chunk []string, snap string, useSeen,
	offer bool, offerLimit int, base map[string]store.Observation, sent map[string]bool, peerLog *store.PeerLog, relog bool) (prepared, error) {
	r := x.r
	var p prepared
	r.mu.Lock()
	defer r.mu.Unlock()
	rl := r.rooms[room]
	if rl == nil || rl.gen != gen {
		return p, ErrStaleGeneration
	}
	st := rl.st
	if !slices.Contains(st.Members, peer) {
		if !relog {
			return p, ErrPeerNotMember
		}
		p.relog = true
	}
	heads, extra := rl.logHeads()
	p.at, p.order = r.m.now(), x.next()
	key := [2]string{room, peer}
	err := r.m.view(ctx, room, func(v *roomView) error {
		own, err := r.summarize(v, st)
		if err != nil {
			return err
		}
		p.own = own
		obs, err := v.rd.Observed(room, peer)
		if err != nil {
			return err
		}
		var oldLog *store.PeerLog
		p.old, oldLog = observedMap(obs)
		q := &PullRequest{V: PullVersion, Room: room, Log: PullLog{Heads: heads, Extra: extra}}
		if p.relog {
			q.Sum = sumOf(nil)
			p.req, p.base = q, jsonSize(q)
			return nil
		}
		if snap == "" {
			q.Sum = own.sum()
			if s, ok := x.seenOf(key, gen); useSeen && ok && s.own == q.Sum {
				q.Seen = s.server
			}
			q.Addrs = x.addrs
			if x.addrsNow != nil {
				q.Addrs = x.addrsNow()
				if len(q.Addrs) > MaxAddrs {
					q.Addrs = q.Addrs[:MaxAddrs]
				}
			}
			q.Peers = x.wantPeers(rl, peer, p.at)
		} else {
			q.Snap, q.Detail = snap, map[string][]store.StreamDetail{}
			for _, a := range chunk {
				q.Detail[a] = own.detailOf(a)
			}
		}
		p.base = jsonSize(q)
		budget := maxRequestBytes - p.base - wireMargin
		if base == nil {
			base = p.old
		}
		rf := x.refusalOf(key, heads)
		offering := offer && len(base) > 0 && rf == nil
		entryBudget := budget
		if offering {
			entryBudget = budget / 2
		}
		pl := peerLog
		if pl == nil {
			pl = oldLog
		}
		once := func(id string, unlisted bool) bool { return !unlisted || !sent["x:"+id] }
		var unlisted []string
		switch {
		case rf != nil && rf.full:
			q.Log.Entries, p.logCut, _ = rl.entriesFor(nil, nil, false, budget, acceptedOnly)
		case rf != nil:
			// Refused even the whole log: none until something changes.
		case pl == nil:
			q.Log.Entries, p.logCut, unlisted = rl.entriesFor(nil, nil, false, entryBudget, once)
		default:
			p.heads = pl.Heads
			q.Log.Entries, p.logCut, unlisted = rl.entriesFor(pl.Heads, pl.Extra, true, entryBudget, once)
		}
		for _, id := range unlisted {
			sent["x:"+id] = true
		}
		budget -= wiresSize(q.Log.Entries)
		if offering {
			if perm, ok := st.Permissions[peer]; ok {
				want := map[string][]store.StreamDetail{}
				var authors []string
				for _, a := range sortedIDs(base) {
					if d := base[a].Detail; d != nil && slices.Contains(st.Admitted, a) {
						want[a], authors = d, append(authors, a)
					}
				}
				limit := x.offerLimit
				if offerLimit > 0 && offerLimit < limit {
					limit = offerLimit
				}
				items, cut, err := x.buildPage(v, readableBy(v, perm), want, authors, sent, nil, limit, budget)
				if err != nil {
					return err
				}
				p.offered = map[string]bool{}
				for _, it := range items {
					q.Offer = append(q.Offer, json.RawMessage(it.wire()))
					sent[it.r.id] = true
					p.offered[it.r.author] = true
				}
				q.OfferMore, p.offerCut = cut && len(items) > 0, cut
			}
		}
		p.req = q
		return nil
	})
	return p, err
}

// offerable says whether an offer built against detail, the peer's detail
// as learned in a round, would carry a record not in sent.
func (x *Exchange) offerable(ctx context.Context, room, peer string, gen int64, detail map[string][]store.StreamDetail,
	sent map[string]bool) (bool, error) {
	r := x.r
	r.mu.Lock()
	defer r.mu.Unlock()
	rl := r.rooms[room]
	if rl == nil || rl.gen != gen {
		return false, ErrStaleGeneration
	}
	perm, ok := rl.st.Permissions[peer]
	if !ok {
		return false, nil
	}
	var authors []string
	for _, a := range sortedIDs(detail) {
		if slices.Contains(rl.st.Admitted, a) {
			authors = append(authors, a)
		}
	}
	found := false
	err := r.m.view(ctx, room, func(v *roomView) error {
		items, _, err := x.buildPage(v, readableBy(v, perm), detail, authors, sent, nil, 1, maxRequestBytes)
		found = len(items) > 0
		return err
	})
	return found, err
}

// ownSummary is this machine's summary of room now.
func (x *Exchange) ownSummary(ctx context.Context, room string, gen int64) (summary, error) {
	r := x.r
	r.mu.Lock()
	defer r.mu.Unlock()
	rl := r.rooms[room]
	if rl == nil || rl.gen != gen {
		return summary{}, ErrStaleGeneration
	}
	var own summary
	err := r.m.view(ctx, room, func(v *roomView) (err error) {
		own, err = r.summarize(v, rl.st)
		return err
	})
	return own, err
}

// refusalOf is the refusal state for key, nil if none or no longer in
// force: a muted one ends when this machine's log heads are no longer
// what they were, or (hear) when a request from the peer, a current
// member, shows log heads other than those heard when it was muted. The
// one place the scheduler and the exchange both read.
func (x *Exchange) refusalOf(key [2]string, heads []string) *refusal {
	x.mu.Lock()
	defer x.mu.Unlock()
	rf := x.refused[key]
	if rf != nil && rf.muted && !sameSet(rf.own, heads) {
		delete(x.refused, key)
		return nil
	}
	return rf
}

// muted says whether peer refused this machine twice running for room
// and neither side's log heads have changed since (own: this machine's).
func (x *Exchange) muted(room, peer string, own []string) bool {
	rf := x.refusalOf([2]string{room, peer}, own)
	return rf != nil && rf.muted
}

// NotMemberSince lists the peers that refused this machine as not a member
// of room twice running (and so are not asked again until a log changes),
// each with when the first of those refusals came. Read-only: for the
// surfaces' "may have been removed" display.
func (x *Exchange) NotMemberSince(room string) map[string]time.Time {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := map[string]time.Time{}
	for key, rf := range x.refused {
		if key[0] == room && rf.muted {
			out[key[1]] = rf.since
		}
	}
	return out
}

// answered records how the peer answered request q (4.4): any not_member
// forgets what was observed of the peer's log; the first, the next
// request sends the accepted log from the start; refused that too, the
// requests after carry neither entries nor offers. A success clears it.
func (x *Exchange) answered(ctx context.Context, room, peer string, gen int64, q *PullRequest, err error) error {
	key := [2]string{room, peer}
	x.mu.Lock()
	switch rf := x.refused[key]; {
	case err == nil:
		delete(x.refused, key)
	case !isNotMember(err):
	case rf == nil:
		x.refused[key] = &refusal{full: true, since: x.r.m.now()}
	case rf.full:
		x.refused[key] = &refusal{muted: true, own: q.Log.Heads, heard: x.heard[key], since: rf.since}
	}
	x.mu.Unlock()
	if !isNotMember(err) {
		return nil
	}
	_, ferr := x.r.m.st.Exchange(ctx, room, gen, func(tx *store.Tx) error { return tx.ForgetLog(room, peer) })
	return ferr
}

// hear records the log heads of a request from peer; when they change,
// the evidence it could not list is sent again; and if peer is a current
// member that this machine muted with other heads heard, it is muted no
// longer and the room's loops are woken to pull from it again.
func (x *Exchange) hear(room, peer string, heads []string, member bool) {
	x.mu.Lock()
	key := [2]string{room, peer}
	if !sameSet(x.heard[key], heads) {
		delete(x.evidence, key)
	}
	x.heard[key] = slices.Clone(heads)
	wake := false
	if rf := x.refused[key]; member && rf != nil && rf.muted && !sameSet(rf.heard, heads) {
		delete(x.refused, key)
		wake = true
	}
	x.mu.Unlock()
	if wake {
		x.r.m.wakes.touch(room)
	}
}

// keepBook drops from the address book every node that is not a member
// of a room held here.
func (x *Exchange) keepBook() {
	if x.book == nil {
		return
	}
	r := x.r
	r.mu.Lock()
	members := map[string]bool{}
	for _, rl := range r.rooms {
		for _, n := range rl.st.Members {
			members[n] = true
		}
	}
	r.mu.Unlock()
	x.book.Keep(func(n string) bool { return members[n] })
}

// evidenceFor is the server's evidence filter for peer: evidence the peer
// could hold unlisted goes once (record: what passes is noted as sent).
func (x *Exchange) evidenceFor(room, peer string, record bool) func(string, bool) bool {
	key := [2]string{room, peer}
	return func(id string, unlisted bool) bool {
		if !unlisted {
			return true
		}
		x.mu.Lock()
		defer x.mu.Unlock()
		if x.evidence[key][id] {
			return false
		}
		if record {
			if x.evidence[key] == nil {
				x.evidence[key] = map[string]bool{}
			}
			x.evidence[key][id] = true
		}
		return true
	}
}

// pageMemory is, for one peer's continuation, the streams of the last
// page with the peer's position in each when it was built, and the
// streams found blocked at the peer: sent its next record, the peer's
// position did not move.
type pageMemory struct {
	last    map[streamKey]int64
	blocked map[streamKey]bool
}

// blockedFor is the streams to leave out of the page for continuing
// request q from peer.
func (x *Exchange) blockedFor(room, peer string, q *PullRequest) map[streamKey]bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := map[streamKey]bool{}
	m := x.pages[[2]string{room, peer}]
	if m == nil {
		return out
	}
	for k := range m.blocked {
		out[k] = true
	}
	for k, c := range m.last {
		if detailC(q.Detail[k.author], k.epoch) == c {
			out[k] = true
		}
	}
	return out
}

// paged notes the page released to peer for q: its streams, and those
// left out as blocked; an opening request forgets both.
func (x *Exchange) paged(room, peer string, q *PullRequest, blocked map[streamKey]bool, items []pageItem) {
	x.mu.Lock()
	defer x.mu.Unlock()
	key := [2]string{room, peer}
	if q.Sum != "" {
		delete(x.pages, key)
		return
	}
	m := &pageMemory{last: map[streamKey]int64{}, blocked: blocked}
	for _, it := range items {
		k := it.r.stream()
		if c := detailC(q.Detail[k.author], k.epoch); it.r.f.Seq > c {
			m.last[k] = c
		}
	}
	x.pages[key] = m
}

func detailC(d []store.StreamDetail, epoch string) int64 {
	for _, s := range d {
		if s.E == epoch {
			return s.C
		}
	}
	return 0
}

// seenOf is seen for key in the room's generation gen, if still in force.
func (x *Exchange) seenOf(key [2]string, gen int64) (seenSums, bool) {
	now := x.r.m.now()
	x.mu.Lock()
	defer x.mu.Unlock()
	s, ok := x.seen[key]
	if ok && (s.gen != gen || !now.Before(s.at.Add(seenFor)) || now.Before(s.at)) {
		delete(x.seen, key)
		return seenSums{}, false
	}
	return s, ok
}

func (x *Exchange) setSeen(key [2]string, s *seenSums) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if s == nil {
		delete(x.seen, key)
	} else {
		x.seen[key] = *s
	}
}

// setResume records where the detail of the next exchange with peer
// starts ("" from the first author).
func (x *Exchange) setResume(room, peer, author string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if author == "" {
		delete(x.resume, [2]string{room, peer})
	} else {
		x.resume[[2]string{room, peer}] = author
	}
}

// resumed is differ, sorted, turned to start at the author an exchange
// that ended Capped was taking, so a difference that persists early in the
// order cannot keep later authors from their turn.
func (x *Exchange) resumed(room, peer string, differ []string) []string {
	x.mu.Lock()
	at := x.resume[[2]string{room, peer}]
	x.mu.Unlock()
	if at == "" {
		return differ
	}
	i, _ := slices.BinarySearch(differ, at)
	return append(slices.Clone(differ[i:]), differ[:i]...)
}

// chunks packs differ into detail requests of at most MaxDetailAuthors
// authors whose detail fits in one request beside a request of size base;
// an author whose detail alone does not fit is left out (too_large, 4.3).
func chunks(own summary, base int, differ []string) (out [][]string, tooLarge []string) {
	budget := maxRequestBytes - base - 4*wireMargin
	var cur []string
	size := 0
	for _, a := range differ {
		n := jsonSize(own.detailOf(a)) + len(a) + 4
		if n > budget {
			tooLarge = append(tooLarge, a)
			continue
		}
		if len(cur) == MaxDetailAuthors || size+n > budget {
			out, cur, size = append(out, cur), nil, 0
		}
		cur, size = append(cur, a), size+n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out, tooLarge
}

// round runs one round of an exchange and reports whether it made
// progress: a record, text or entry stored here, or stored by the peer as
// its response shows (its log heads moved after entries were given, or
// its digest or detail of an author whose records were offered changed);
// or, when it stored nothing, whether the peer's detail first learned in
// it lets the next round offer something (4.3 step 3). Cuts only justify
// the next request of the round; one that does not advance ends quietly.
// A round that stored nothing and was clean (nothing refused, skipped,
// stalled or too large) leaves the peer's sum as seen.
func (x *Exchange) round(ctx context.Context, room, peer string, gen int64, o PullOpts, st *PullStats,
	sent map[string]bool) (bool, error) {
	key := [2]string{room, peer}
	var peerLog *store.PeerLog
	var base map[string]store.Observation // the observation the round's offer is built on
	progress := false
	clean := true
	// What the peer was last seen to hold of each author, to tell whether
	// it stored what it was given; and its detail learned in this round
	// that differs from base.
	digest, detail, baseDetail := map[string]string{}, map[string]string{}, map[string]string{}
	learned := map[string][]store.StreamDetail{}
	send := func(chunk []string, snap string, useSeen bool) (prepared, *PullResponse, error) {
		// An exchange sends at most pullMaxRequests. Every request is
		// admitted by the peer's budget first, a slot and then a token;
		// only then is it prepared, so the membership check is the one at
		// dispatch and writes made while waiting go in it.
		if st.Requests >= pullMaxRequests {
			return prepared{}, nil, errPullCapped
		}
		var stop <-chan struct{}
		if o.WaitMS > 0 && st.Requests == 0 && snap == "" {
			stop = o.Stop
		}
		ticket, err := x.budget.admit(ctx, peer, stop)
		if err != nil {
			return prepared{}, nil, err
		}
		p, err := x.prepare(ctx, room, peer, gen, chunk, snap, useSeen, o.Offer, o.OfferLimit, base, sent, peerLog, st.relog)
		if err != nil {
			ticket.cancel()
			return p, nil, err
		}
		waiting := o.WaitMS > 0 && st.Requests == 0 && snap == "" && !p.relog &&
			len(p.req.Offer) == 0 && len(p.req.Log.Entries) == 0
		if waiting {
			p.req.WaitMS = min(o.WaitMS, MaxWaitMS)
			// Held at the peer once it has it: the slot goes back then, at
			// the latest.
			ticket.admitAfter(pullAdmitAfter)
		}
		if base == nil {
			base = p.old
			if base == nil {
				base = map[string]store.Observation{}
			}
			for a, ob := range base {
				digest[a] = ob.Digest
				if ob.Detail != nil {
					detail[a] = string(mustJSON(ob.Detail))
					baseDetail[a] = detail[a]
				}
			}
		}
		st.Requests++
		st.Offered += len(p.req.Offer)
		st.Given += len(p.req.Log.Entries)
		var resp *PullResponse
		tctx := context.WithValue(ctx, attemptKey{}, ticket) // further addresses pay too (PullAttempt)
		if waiting && o.Stop != nil {
			resp, err = x.pullStoppable(tctx, peer, p.req, o.Stop)
		} else {
			resp, err = x.t.Pull(tctx, peer, p.req)
		}
		ticket.done()
		if aerr := x.answered(ctx, room, peer, gen, p.req, err); aerr != nil {
			return p, nil, errors.Join(err, aerr)
		}
		if err != nil {
			return p, nil, err
		}
		if err := checkResponse(resp, p.req); err != nil {
			return p, nil, errors.Join(err, x.refuse(ctx, room, peer, gen, CodeBadResponse, 1))
		}
		return p, resp, nil
	}
	// apply applies resp and says whether something was stored, here or
	// by the peer as resp shows.
	apply := func(p prepared, resp *PullResponse) (appliedResp, bool, error) {
		// What the peer sent, it holds: never offer it back.
		for _, w := range resp.Page {
			if rec, err := core.ParseRecord(w); err == nil {
				sent[rec.ID()] = true
			}
		}
		peerLog = &store.PeerLog{Heads: resp.Log.Heads, Extra: resp.Log.Extra}
		st.heads = resp.Log.Heads
		a, err := x.apply(ctx, room, peer, gen, p, resp)
		if err != nil {
			return a, false, err
		}
		if a.stored+a.repaired > 0 {
			x.r.m.wakes.touch(room)
		}
		st.Responses++
		st.Stored += a.stored
		st.Repaired += a.repaired
		if a.entries {
			st.Entries++
		}
		if a.gone {
			// Its entries removed the peer here. If more follow, they may
			// admit it again (a batch that does is taken, Exchange.apply):
			// the log goes on, and nothing else of the answer is taken.
			if !a.entries || !resp.Log.More {
				return a, false, ErrPeerNotMember
			}
			st.relog, progress = true, true
			return a, true, nil
		}
		st.relog = false
		// Only from a member: a refused answer leaves nothing behind.
		x.learnPeers(room, peer, resp.Peers)
		if x.files != nil {
			x.files.answerWants(room, peer, resp.Want)
		}
		moved := a.entries || a.stored+a.repaired > 0
		if len(p.req.Log.Entries) > 0 && !resp.Same && p.heads != nil && !sameSet(p.heads, resp.Log.Heads) {
			moved = true
		}
		if p.req.Sum != "" && !resp.Same {
			for au := range p.offered {
				if resp.Digests[au] != digest[au] {
					moved = true
				}
			}
			digest = maps.Clone(resp.Digests)
		}
		for au, d := range resp.Detail {
			j := string(mustJSON(d))
			if p.offered[au] && j != detail[au] {
				moved = true
			}
			if j != baseDetail[au] {
				learned[au] = d
			} else {
				delete(learned, au)
			}
			detail[au] = j
		}
		if moved {
			progress = true
		}
		if a.refused > 0 || len(a.blocked) > 0 {
			clean = false
		}
		return a, moved, nil
	}
	same := func(p prepared) {
		st.Same++
		st.Closed = p.req.Seen == ""
		if p.req.Seen != "" {
			st.SeenSame++
		}
	}

	// relogged reads the rest of the log of a peer whose answer removed
	// it here, until a page admits it again (the round is over: the next
	// one starts afresh) or one does not (ErrPeerNotMember).
	relogged := func() (bool, error) {
		for st.relog {
			p, resp, err := send(nil, "", false)
			if err != nil {
				return false, err
			}
			if _, _, err := apply(p, resp); err != nil {
				return false, err
			}
		}
		return true, nil
	}

	st.Closed = false
	p, resp, err := send(nil, "", st.Rounds == 1)
	if err != nil {
		return false, err
	}
	a, _, err := apply(p, resp)
	if err != nil {
		return false, err
	}
	if st.relog {
		return relogged()
	}
	if resp.Same {
		same(p)
		return progress, nil
	}
	open := resp
	fresh := a.entries // the room log changed: Admitted may have
	// The opening again while its log or offer was cut. A page can move
	// nothing because another member delivered the same entries while it
	// was on the way: the opening goes on until pullQuietPages running
	// moved nothing.
	quiet := 0
	for resp.Log.More || p.logCut || p.offerCut {
		var moved bool
		if p, resp, err = send(nil, "", false); err != nil {
			return false, err
		}
		if a, moved, err = apply(p, resp); err != nil {
			return false, err
		}
		if st.relog {
			return relogged()
		}
		if resp.Same {
			same(p)
			return progress, nil
		}
		open, fresh = resp, fresh || a.entries
		if quiet = quietAfter(quiet, moved); quiet >= pullQuietPages {
			st.Stalled++
			clean = false
			break
		}
	}
	own := p.own
	if fresh {
		if own, err = x.ownSummary(ctx, room, gen); err != nil {
			return false, err
		}
	}
	var takes []string
	for _, au := range own.authors {
		if slices.Contains(open.Authors, au) {
			takes = append(takes, au)
		}
	}
	tooLarge := func(n int) error {
		st.TooLarge += n
		clean = false
		return x.refuse(ctx, room, peer, gen, CodeTooLarge, n)
	}
	queue, big := chunks(own, p.base, x.resumed(room, peer, differ(takes, own.digests, open.Digests)))
	if len(big) > 0 {
		if err := tooLarge(len(big)); err != nil {
			return false, err
		}
	}
	for len(queue) > 0 {
		active := queue[0]
		queue = queue[1:]
		blocked := map[streamKey]bool{}
		quiet := 0
		for len(active) > 0 {
			var moved bool
			st.resume = active[0]
			if p, resp, err = send(active, open.Sum, false); err != nil {
				return false, err
			}
			if a, moved, err = apply(p, resp); err != nil {
				return false, err
			}
			if st.relog {
				return relogged()
			}
			// Detail the response left out is asked for again; an author
			// whose detail alone did not fit is skipped (4.3 step 5).
			if n := len(resp.Detail); n < len(active) {
				rest := active[n:]
				if n == 0 {
					if err := tooLarge(1); err != nil {
						return false, err
					}
					rest = rest[1:]
				}
				if len(rest) > 0 {
					queue = append([][]string{rest}, queue...)
				}
				active = active[:n]
			}
			if len(active) == 0 || !resp.More && !resp.Log.More && !p.offerCut && !p.logCut {
				break
			}
			// A stream newly blocked here: the peer leaves it out of the next
			// page, so the others go on.
			grew := false
			for k := range a.blocked {
				if !blocked[k] {
					blocked[k], grew = true, true
				}
			}
			if quiet = quietAfter(quiet, moved || grew); quiet >= pullQuietPages {
				st.Stalled++
				clean = false
				break
			}
		}
	}
	st.resume = ""
	x.setResume(room, peer, "")
	if !progress && o.Offer && len(learned) > 0 {
		ok, err := x.offerable(ctx, room, peer, gen, learned, sent)
		if err != nil {
			return false, err
		}
		progress = ok // one more round carries the offer
	}
	if progress || !clean {
		x.setSeen(key, nil)
	} else {
		x.setSeen(key, &seenSums{server: open.Sum, own: p.own.sum(), gen: gen, at: x.r.m.now()})
	}
	return progress, nil
}

// pullQuietPages is how many continuations running may move nothing
// before the request chain ends (round).
const pullQuietPages = 2

// quietAfter counts a continuation that moved nothing, or ends the count.
func quietAfter(quiet int, moved bool) int {
	if moved {
		return 0
	}
	return quiet + 1
}

// errPullCapped: the exchange has sent pullMaxRequests (Pull).
var errPullCapped = errors.New("pull: request cap of the exchange reached")

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// CodeBadResponse counts a response refused because its shape did not fit
// its request (PROTOCOL 4.2).
const CodeBadResponse = "bad_response"

// refuse counts n of code against peer.
func (x *Exchange) refuse(ctx context.Context, room, peer string, gen int64, code string, n int) error {
	_, err := x.r.m.st.Exchange(ctx, room, gen, func(tx *store.Tx) error {
		return tx.Refuse(room, peer, code, n, x.r.m.now())
	})
	return err
}

// checkResponse checks a response against its request before anything of
// it is applied: its form for the request's form; to an opening, same
// with no entries, or authors and digests whose sum is the one sent; to a
// continuing request, the detail of the first authors asked, in order,
// and page records only by them; the limits and the whole size
// (PROTOCOL 4.2, 4.3 step 5).
func checkResponse(resp *PullResponse, q *PullRequest) error {
	bad := func(what string) error { return fmt.Errorf("pull: bad response: %s", what) }
	if resp == nil || resp.V != PullVersion {
		return bad("v")
	}
	switch {
	case len(resp.Log.Entries) > MaxLogEntries || len(resp.Log.Heads) > MaxLogHeads || len(resp.Log.Extra) > MaxLogExtra ||
		!uniqueIDs(resp.Log.Heads) || !uniqueIDs(resp.Log.Extra):
		return bad("log")
	case len(resp.Page) > MaxPageRecords:
		return bad("page")
	case len(resp.Log.Heads) == 0:
		return bad("log heads")
	case len(resp.Peers) > MaxPeersListed, resp.Peers != nil && !q.Peers:
		return bad("peers")
	case !checkWants(resp.Want):
		return bad("want")
	}
	for _, p := range resp.Peers {
		if !validPeer(p) {
			return bad("peer")
		}
	}
	if q.Sum != "" {
		switch {
		case resp.Detail != nil || resp.Page != nil || resp.More:
			return bad("page or detail to an opening")
		case resp.Same && (resp.Authors != nil || resp.Digests != nil || resp.Sum != "" ||
			len(resp.Log.Entries) > 0 || resp.Log.More):
			return bad("same with more than the log heads")
		case resp.Same:
			return checkSize(resp, bad)
		case len(resp.Authors) < 1 || len(resp.Authors) > MaxPullAuthors || !uniqueNodes(resp.Authors):
			return bad("authors")
		}
		for a, d := range resp.Digests {
			if !slices.Contains(resp.Authors, a) || len(d) != 64 || !isLowerHex(d) {
				return bad("digests")
			}
		}
		if resp.Sum != sumOf(resp.Digests) {
			return bad("sum")
		}
		return checkSize(resp, bad)
	}
	if resp.Same || resp.Authors != nil || resp.Digests != nil || resp.Sum != "" {
		return bad("detail form")
	}
	// The detail of the first authors asked, in order: the rest left out.
	asked, got := sortedIDs(q.Detail), sortedIDs(resp.Detail)
	if len(got) > len(asked) || !slices.Equal(got, asked[:len(got)]) {
		return bad("detail")
	}
	for _, d := range resp.Detail {
		if d == nil || checkDetail(d) != nil {
			return bad("detail")
		}
	}
	for _, w := range resp.Page {
		if rec, err := core.ParseRecord(w); err == nil {
			if _, ok := resp.Detail[rec.Author()]; !ok {
				return bad("page record of an author not in the detail")
			}
		}
	}
	return checkSize(resp, bad)
}

func checkSize(resp *PullResponse, bad func(string) error) error {
	if jsonSize(resp) > maxResponseBytes {
		return bad("size")
	}
	return nil
}

// appliedResp is what applying one response did.
type appliedResp struct {
	entries          bool
	stored, repaired int
	blocked          map[streamKey]bool
	refused          int  // records and entries of it refused
	gone             bool // the peer is not a member here: its page was not taken
	observed         bool // what the peer said it holds was stored
}

// errPeerGone: the answering peer is not a member here and its answer's
// entries do not make it one (Exchange.apply); nothing of it is stored.
var errPeerGone = errors.New("pull: the answering peer is not a member here")

// apply applies one response to the request p in one transaction: its log
// entries, then, if the peer is still a member here, its page and what the
// peer said it holds, dated when p was built (PROTOCOL 4.4): equal to
// this machine's summary on same (not after a same to seen, which says
// only that nothing changed), its digests, or the detail it sent.
func (x *Exchange) apply(ctx context.Context, room, peer string, gen int64, p prepared, resp *PullResponse) (appliedResp, error) {
	r := x.r
	r.mu.Lock()
	defer r.mu.Unlock()
	rl := r.rooms[room]
	if rl == nil || rl.gen != gen {
		return appliedResp{}, ErrStaleGeneration
	}
	now := r.m.now()
	var a appliedResp
	refused := map[string]int{}
	// T4, as Exchange.serve and Rooms.receiveJoin: a peer's log entries
	// count only if it is a member here before them or they make it one.
	// One removed while its answer was on the way gets nothing stored: the
	// batch is refused and the room's state rolls back.
	wasMember := slices.Contains(rl.st.Members, peer)
	commit := func(ctx context.Context, b *store.LogBatch, st members.State, look func(string) *core.VerifiedEntry) (int64, error) {
		if !wasMember && !slices.Contains(st.Members, peer) {
			return 0, errPeerGone
		}
		a = appliedResp{entries: b != nil}
		var n int64
		_, err := r.m.st.Exchange(ctx, room, gen, func(tx *store.Tx) error {
			if b != nil {
				var err error
				if n, err = tx.ApplyLog(*b); err != nil {
					return err
				}
			}
			if !slices.Contains(st.Members, peer) {
				a.gone = true
				return nil
			}
			pl, err := r.place(tx, rl, st, look, resp.Page, refused, now)
			if err != nil {
				return err
			}
			a.stored, a.repaired, a.blocked = pl.stored, pl.repaired, pl.blocked
			plog := store.PeerLog{Heads: resp.Log.Heads, Extra: resp.Log.Extra}
			a.observed = x.later(room, peer, p.order)
			switch {
			case !a.observed:
				// A later observation is stored: this older one is not.
			case resp.Same && (p.req.Seen == "" || p.req.Seen == p.req.Sum):
				err = observe(tx, room, peer, st, p.own.rows(), plog, p.at, true)
			case resp.Same:
				err = tx.ObserveLog(room, peer, p.at, plog)
			case p.req.Sum != "":
				var old []store.Observation
				if old, err = tx.Reader().Observed(room, peer); err == nil {
					om, _ := observedMap(old)
					rows := observation(resp.Authors, resp.Authors, resp.Digests, nil, p.own, om)
					err = observe(tx, room, peer, st, rows, plog, p.at, true)
				}
			default:
				err = observe(tx, room, peer, st, detailRows(resp.Detail), plog, p.at, false)
			}
			if err != nil {
				return err
			}
			for _, n := range refused {
				a.refused += n
			}
			return refuseAll(tx, room, peer, refused, now)
		})
		if err == nil && a.observed {
			x.stored(room, peer, p.order)
		}
		return n, err
	}
	res, err := r.ingestWith(ctx, rl, gen, rawBytes(resp.Log.Entries), nil, commit)
	if errors.Is(err, errPeerGone) {
		return appliedResp{gone: true}, nil
	}
	if err != nil {
		return appliedResp{}, err
	}
	if err := r.countInvalid(ctx, rl, peer, res, now); err != nil {
		return appliedResp{}, err
	}
	for _, x := range res {
		if x.Disposition == members.Invalid {
			a.refused++
		}
	}
	return a, nil
}
