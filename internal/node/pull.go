package node

// The pull exchange between machines (DESIGN-next section 4, PROTOCOL-next
// section 4; rules [R3], [R4], [R5], [RM1] to [RM3] of
// internal/model/delivery): the one request type that moves records and
// log entries, carried both ways. Serve is the server's side, in the order
// of PROTOCOL-next 4.4; Pull is the requester's, one whole exchange of
// rounds. Both work over Rooms and a PullTransport; the HTTP endpoint,
// waiting pulls and the scheduler come in step 5b, joining in step 4b.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
var ErrPeerNotMember = &UserError{"the peer is not a member of the room here"}

// ExchangeOpts bounds what one request or response carries.
type ExchangeOpts struct {
	PageLimit  int // records per page; 0: MaxPageRecords
	OfferLimit int // records per offer; 0: MaxOfferRecords
}

// Exchange is the pull exchange of one machine.
type Exchange struct {
	r          *Rooms
	t          PullTransport
	pageLimit  int
	offerLimit int

	// In memory, per room and peer (PROTOCOL-next 4.5): refused, how the
	// peer last refused this machine as not a member; heard, the log heads
	// of the last request from it; seen, the peer's sum after the last
	// round with it that stored nothing, and this machine's then.
	mu      sync.Mutex
	refused map[[2]string]*refusal
	heard   map[[2]string][]string
	seen    map[[2]string]seenSums
	// made counts observations as they are made: a request built, or one
	// received. latest is, per room and peer, the count of the observation
	// stored last; a response to a request built before it is not stored
	// over it [R5]. In memory: no request outlives the process.
	made   int64
	latest map[[2]string]int64

	// Test hooks. atRelease runs where a held response is released (step
	// 10 of PROTOCOL-next 4.4; step 5b holds there); withhold makes a
	// server leave a record out of what it sends, as a faulty member would.
	atRelease func(room, peer string, q *PullRequest)
	withhold  func(id string) bool
}

// NewExchange returns the exchange of the machine behind r over t.
func NewExchange(r *Rooms, t PullTransport, o ExchangeOpts) *Exchange {
	x := &Exchange{r: r, t: t, pageLimit: o.PageLimit, offerLimit: o.OfferLimit, refused: map[[2]string]*refusal{},
		heard: map[[2]string][]string{}, seen: map[[2]string]seenSums{}, latest: map[[2]string]int64{}}
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

// sum is the short form of s (PROTOCOL-next 4.3).
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
// Records in skip were sent already and count as held by the peer. It
// returns at most limit records and maxBytes of wire objects, and whether
// it cut.
func (x *Exchange) buildPage(v *roomView, readable func(*rec) (bool, error), want map[string][]store.StreamDetail,
	authors []string, skip map[string]bool, limit, maxBytes int) ([]pageItem, bool, error) {
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
		if s.Contig > 0 && slices.Contains(authors, s.Author) {
			streams = append(streams, streamKey{s.Author, s.Epoch})
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
// and its held and refused entries (PROTOCOL-next 4.1).
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
	if len(extra) > MaxLogExtra {
		extra = extra[:MaxLogExtra]
	}
	return heads, extra
}

// entriesFor returns, as rl passes them on (accepted in causal order, then
// held and refused as evidence), the entries not at or before heads and
// not in extra, at most MaxLogEntries and maxBytes of wire objects, and
// whether more were left out. With known false (nothing is known of the
// peer's log) every entry goes.
func (rl *roomLog) entriesFor(heads, extra []string, known bool, maxBytes int) ([]json.RawMessage, bool) {
	have := map[string]bool{}
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
		}
	}
	var out []json.RawMessage
	size := 0
	for _, v := range rl.mach.Offer() {
		if have[v.ID()] {
			continue
		}
		w := v.Wire()
		if size += len(w) + 1; len(out) == MaxLogEntries || size > maxBytes {
			return out, true
		}
		out = append(out, json.RawMessage(w))
	}
	return out, false
}

func rawBytes(xs []json.RawMessage) [][]byte {
	out := make([][]byte, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

// ---- placing records ----

// placed counts what placing a page or an offer did; blocked are the
// authors of records that could not be stored (refused, or behind one that
// was), whose streams a continuation moves past.
type placed struct {
	stored, repaired int
	blocked          map[string]bool
}

// place checks and places each received record wire (PROTOCOL-next
// section 2) inside tx under membership st, counting refusals by code.
func (r *Rooms) place(tx *store.Tx, rl *roomLog, st members.State, look func(string) *core.VerifiedEntry,
	wires []json.RawMessage, refused map[string]int, now time.Time) (placed, error) {
	p := placed{blocked: map[string]bool{}}
	for _, w := range wires {
		rec, err := core.ParseRecord(w)
		if err != nil {
			refused[refusalCodeOr(err, "bad_wire")]++
			continue
		}
		a := rec.Author()
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
			p.blocked[a] = true
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
		case store.OutcomeFork, store.OutcomeFuture:
			refused[string(out)]++
			p.blocked[a] = true
		default:
			p.blocked[a] = true
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
// is unknown until sent (PROTOCOL-next 4.3, item 4).
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

// observe writes the rows, an observation dated at, and raises the known
// position of every stream a known detail reports for an author admitted
// here, as the model's observe does. whole replaces what was observed of
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
		if !slices.Contains(st.Admitted, o.Author) {
			continue
		}
		for _, s := range o.Detail {
			if err := tx.NoteKnown(room, o.Author, s.E, s.C, at); err != nil {
				return err
			}
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
	rl    *roomLog
	gen   int64
	ver   int64 // store.Changes the response reflects
	sv    served
	resp  *PullResponse
	items []pageItem
}

// Answers to an opening request.
const (
	answerDigests = iota // authors, digests and sum
	answerSame           // same: the summaries are equal now
	answerSeen           // same: nothing changed since a round that stored nothing
)

// answer decides the answer to the opening request q (PROTOCOL-next 4.3):
// same if the request was quiet (its entries and offer stored nothing,
// and this machine has no log entry the requester lacks) and this
// machine's sum equals the request's sum, or its seen; otherwise the
// digests. A waiting pull (step 5b) is held exactly while this is same.
func answer(rl *roomLog, q *PullRequest, own summary, stored bool) int {
	if stored {
		return answerDigests
	}
	if es, _ := rl.entriesFor(q.Log.Heads, q.Log.Extra, true, maxResponseBytes); len(es) > 0 {
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
// authenticated, in the order of PROTOCOL-next 4.4: the request's log
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
	b, err := x.serve(ctx, peer, q)
	if err != nil {
		return nil, err
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
	x.hear(q.Room, peer, q.Log.Heads)
	now := r.m.now()
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
			case answer(rl, q, own, sv.stored) == answerSame:
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
		resp.Log.Entries, resp.Log.More = rl.entriesFor(q.Log.Heads, q.Log.Extra, true, budget)
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
		if answer(rl, q, own, sv.stored) != answerDigests {
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
		resp.Detail = map[string][]store.StreamDetail{}
		var authors []string
		for _, a := range sortedIDs(q.Detail) {
			resp.Detail[a] = own.detailOf(a)
			if slices.Contains(st.Admitted, a) {
				authors = append(authors, a)
			}
		}
		budget := maxResponseBytes - jsonSize(resp) - wireMargin
		entries(budget / 2)
		budget -= wiresSize(resp.Log.Entries)
		var cut bool
		b.items, cut, err = x.buildPage(v, readableBy(v, st.Permissions[peer]), q.Detail, authors, nil,
			x.pageLimit, budget)
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
// written since, the response is built again (prune, log, digests, page),
// so no text withdrawn, voided or no longer permitted meanwhile goes out.
func (x *Exchange) release(ctx context.Context, peer string, q *PullRequest, b *built) (*PullResponse, error) {
	r := x.r
	if err := r.m.pruneIfDue(ctx); err != nil {
		return nil, unavailable(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rl := r.rooms[q.Room]
	switch {
	case rl == nil:
		return nil, notMember()
	case rl != b.rl || rl.gen != b.gen:
		return nil, pullErr(503, CodeUnavailable, "room changed")
	case !slices.Contains(rl.st.Members, peer):
		return nil, notMember()
	}
	if r.m.st.Changes() != b.ver {
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
	return resp, nil
}

func hasText(k core.Kind) bool { return k == core.KindMessage || k == core.KindReplace }

// ---- the requester ----

// PullOpts are the requester's choices for one exchange.
type PullOpts struct {
	// Offer attaches, to each request, records the peer lacks by its last
	// observed summary [RM3]; none without one.
	Offer bool
}

// PullStats says what one exchange did.
type PullStats struct {
	Requests  int // requests sent
	Responses int // responses applied
	Rounds    int
	Stored    int  // records stored from pages
	Repaired  int  // texts repaired from pages
	Entries   int  // responses whose log entries changed the room log
	Offered   int  // records offered
	Same      int  // rounds answered same
	SeenSame  int  // of them, to a request that carried seen
	Closed    bool // the exchange ended with a same to a request without seen
	Stalled   int  // continuations ended because they did not advance
	TooLarge  int  // authors whose detail alone did not fit in a request
	Capped    bool
}

// refusal is how a peer last refused this machine as not a member: full,
// the next opening request sends the whole accepted log (once); muted, it
// was refused that too, and openings carry no entries until this
// machine's log heads (own) or those heard from the peer change.
type refusal struct {
	full, muted bool
	own, heard  []string
}

// seenSums is a peer's sum, and this machine's, after a round with it
// that stored nothing on either side.
type seenSums struct{ server, own string }

// Pull runs one exchange with peer for room: rounds of an opening request
// with this machine's sum (and seen, an offer and the log entries the peer
// lacks), answered same or with the peer's digests; then continuing
// requests carrying this machine's detail for the authors whose digests
// differ, 16 at a time within the request limit, each answered with a
// page, repeated while something was cut and the continuation advances. A
// round that stored nothing on either side ends the exchange, and the
// peer's sum is remembered as seen (PROTOCOL-next 4.3). Each response is
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
	for st.Rounds < pullRoundsPerPull {
		st.Rounds++
		progress, err := x.round(ctx, room, peer, gen, o, &st, sent)
		if err != nil {
			return st, err
		}
		if !progress {
			x.pulled(room, gen)
			return st, nil
		}
	}
	// Only a peer changing all the time gets here: counted, not an error.
	st.Capped = true
	x.pulled(room, gen)
	return st, nil
}

func (x *Exchange) pulled(room string, gen int64) {
	x.r.mu.Lock()
	defer x.r.mu.Unlock()
	if rl := x.r.rooms[room]; rl != nil && rl.gen == gen {
		rl.pulled = true
	}
}

// prepared is one request and what it was built from.
type prepared struct {
	req      *PullRequest
	own      summary
	old      map[string]store.Observation
	at       time.Time // when it was built: the date of what its response tells
	order    int64     // and its count among observations (Exchange.next)
	base     int       // its size without entries and offer
	sentNew  int       // records and entries not given to the peer before
	offerCut bool
	logCut   bool
}

// prepare builds an opening request (with seen if useSeen and still
// valid) or a continuing one with detail for chunk and snap; with an offer
// if offer is set (built against base, the peer's summary as observed when
// the round began, less what sent holds: one offer for the round, sent in
// pages); and with the log entries not at or before peerLog (the peer's
// heads; nil: as last observed), or after a refusal as refused says; all
// within the request limit: detail first, then entries, then the offer.
func (x *Exchange) prepare(ctx context.Context, room, peer string, gen int64, chunk []string, snap string, useSeen,
	offer bool, base map[string]store.Observation, sent map[string]bool, peerLog *store.PeerLog) (prepared, error) {
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
		return p, ErrPeerNotMember
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
		if snap == "" {
			q.Sum = own.sum()
			if s, ok := x.seenOf(key); useSeen && ok && s.own == q.Sum {
				q.Seen = s.server
			}
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
		offering := offer && len(base) > 0
		entryBudget := budget
		if offering {
			entryBudget = budget / 2
		}
		pl := peerLog
		if pl == nil {
			pl = oldLog
		}
		switch rf := x.refusalOf(key, heads); {
		case snap == "" && rf != nil && rf.full:
			q.Log.Entries, p.logCut = rl.entriesFor(nil, nil, false, entryBudget)
		case snap == "" && rf != nil:
			// Refused even the whole log: none until something changes.
		case pl == nil:
			q.Log.Entries, p.logCut = rl.entriesFor(nil, nil, false, entryBudget)
		default:
			q.Log.Entries, p.logCut = rl.entriesFor(pl.Heads, pl.Extra, true, entryBudget)
		}
		for _, e := range q.Log.Entries {
			if k := "e:" + string(e); !sent[k] {
				sent[k] = true
				p.sentNew++
			}
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
				items, cut, err := x.buildPage(v, readableBy(v, perm), want, authors, sent, x.offerLimit, budget)
				if err != nil {
					return err
				}
				for _, it := range items {
					q.Offer = append(q.Offer, json.RawMessage(it.wire()))
					sent[it.r.id] = true
				}
				p.sentNew += len(items)
				q.OfferMore, p.offerCut = cut && len(items) > 0, cut
			}
		}
		p.req = q
		return nil
	})
	return p, err
}

// refusalOf is the refusal state for key, nil if none or no longer in
// force: a muted one ends when this machine's log heads, or those last
// heard from the peer, are no longer what they were.
func (x *Exchange) refusalOf(key [2]string, heads []string) *refusal {
	x.mu.Lock()
	defer x.mu.Unlock()
	rf := x.refused[key]
	if rf != nil && rf.muted && (!sameSet(rf.own, heads) || !sameSet(rf.heard, x.heard[key])) {
		delete(x.refused, key)
		return nil
	}
	return rf
}

// opened records how the peer answered an opening request q (4.5): on
// not_member, the next opening sends the whole log; refused that too, the
// openings after carry no entries.
func (x *Exchange) opened(key [2]string, q *PullRequest, err error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	switch rf := x.refused[key]; {
	case err == nil:
		delete(x.refused, key)
	case !isNotMember(err):
	case rf == nil:
		x.refused[key] = &refusal{full: true}
	case rf.full:
		x.refused[key] = &refusal{muted: true, own: q.Log.Heads, heard: x.heard[key]}
	}
}

func (x *Exchange) hear(room, peer string, heads []string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.heard[[2]string{room, peer}] = slices.Clone(heads)
}

func (x *Exchange) seenOf(key [2]string) (seenSums, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	s, ok := x.seen[key]
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
// progress: a record, text or entry stored here, or one given to the peer
// that it had not been given in this exchange. Cuts only justify the next
// request of the round; one that does not advance ends quietly. A round
// that made no progress leaves the peer's sum as seen.
func (x *Exchange) round(ctx context.Context, room, peer string, gen int64, o PullOpts, st *PullStats,
	sent map[string]bool) (bool, error) {
	key := [2]string{room, peer}
	var peerLog *store.PeerLog
	var base map[string]store.Observation // the observation the round's offer is built on
	progress := false
	send := func(chunk []string, snap string, useSeen bool) (prepared, *PullResponse, error) {
		p, err := x.prepare(ctx, room, peer, gen, chunk, snap, useSeen, o.Offer, base, sent, peerLog)
		if err != nil {
			return p, nil, err
		}
		if base == nil {
			base = p.old
			if base == nil {
				base = map[string]store.Observation{}
			}
		}
		st.Requests++
		st.Offered += len(p.req.Offer)
		if p.sentNew > 0 {
			progress = true
		}
		resp, err := x.t.Pull(ctx, peer, p.req)
		if snap == "" {
			x.opened(key, p.req, err)
		}
		if err != nil {
			return p, nil, err
		}
		if err := checkResponse(resp, p.req); err != nil {
			return p, nil, err
		}
		return p, resp, nil
	}
	apply := func(p prepared, resp *PullResponse) (appliedResp, error) {
		// What the peer sent, it holds: never offer it back.
		for _, w := range resp.Page {
			if rec, err := core.ParseRecord(w); err == nil {
				sent[rec.ID()] = true
			}
		}
		peerLog = &store.PeerLog{Heads: resp.Log.Heads, Extra: resp.Log.Extra}
		a, err := x.apply(ctx, room, peer, gen, p, resp)
		if err != nil {
			return a, err
		}
		st.Responses++
		st.Stored += a.stored
		st.Repaired += a.repaired
		if a.entries {
			st.Entries++
		}
		if a.gone {
			return a, ErrPeerNotMember
		}
		if a.entries || a.stored+a.repaired > 0 {
			progress = true
		}
		return a, nil
	}
	// advanced: the request p and its response moved something on.
	advanced := func(p prepared, resp *PullResponse, a appliedResp, prevHeads []string) bool {
		return a.entries || a.stored+a.repaired > 0 || p.sentNew > 0 ||
			(p.logCut && !sameSet(prevHeads, resp.Log.Heads))
	}
	same := func(p prepared) {
		st.Same++
		st.Closed = p.req.Seen == ""
		if p.req.Seen != "" {
			st.SeenSame++
		}
	}

	st.Closed = false
	p, resp, err := send(nil, "", st.Rounds == 1)
	if err != nil {
		return false, err
	}
	a, err := apply(p, resp)
	if err != nil {
		return false, err
	}
	if resp.Same {
		same(p)
		return progress, nil
	}
	open := resp
	// The opening again while its log or offer was cut.
	for resp.Log.More || p.logCut || p.offerCut {
		prev := resp.Log.Heads
		if p, resp, err = send(nil, "", false); err != nil {
			return false, err
		}
		if a, err = apply(p, resp); err != nil {
			return false, err
		}
		if resp.Same {
			same(p)
			return progress, nil
		}
		open = resp
		if !advanced(p, resp, a, prev) {
			st.Stalled++
			break
		}
	}
	var takes []string
	for _, au := range p.own.authors {
		if slices.Contains(open.Authors, au) {
			takes = append(takes, au)
		}
	}
	cs, tooLarge := chunks(p.own, p.base, differ(takes, p.own.digests, open.Digests))
	if len(tooLarge) > 0 {
		st.TooLarge += len(tooLarge)
		if err := x.refuse(ctx, room, peer, gen, CodeTooLarge, len(tooLarge)); err != nil {
			return false, err
		}
	}
	for _, chunk := range cs {
		for active := chunk; len(active) > 0; {
			prev := resp.Log.Heads
			if p, resp, err = send(active, open.Sum, false); err != nil {
				return false, err
			}
			if a, err = apply(p, resp); err != nil {
				return false, err
			}
			if !resp.More && !resp.Log.More && !p.offerCut && !p.logCut {
				break
			}
			// Move past streams that cannot be stored: the rest of the chunk
			// goes on without their authors.
			var rest []string
			for _, au := range active {
				if !resp.More || !a.blocked[au] {
					rest = append(rest, au)
				}
			}
			if len(rest) < len(active) {
				active = rest
				continue
			}
			if !advanced(p, resp, a, prev) {
				st.Stalled++
				break
			}
		}
	}
	if progress {
		x.setSeen(key, nil)
	} else {
		x.setSeen(key, &seenSums{server: open.Sum, own: p.own.sum()})
	}
	return progress, nil
}

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
// continuing request, the detail of exactly the authors asked; the limits
// and the whole size (PROTOCOL-next 4.2).
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
	case len(resp.Peers) > MaxPeersListed:
		return bad("peers")
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
	if resp.Same || resp.Authors != nil || resp.Digests != nil || resp.Sum != "" || len(resp.Detail) != len(q.Detail) {
		return bad("detail form")
	}
	for a, d := range resp.Detail {
		if _, asked := q.Detail[a]; !asked || d == nil || checkDetail(d) != nil {
			return bad("detail")
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
	blocked          map[string]bool
	gone             bool // the peer is not a member here: its page was not taken
	observed         bool // what the peer said it holds was stored
}

// apply applies one response to the request p in one transaction: its log
// entries, then, if the peer is still a member here, its page and what the
// peer said it holds, dated when p was built (PROTOCOL-next 4.4): equal to
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
	commit := func(ctx context.Context, b *store.LogBatch, st members.State, look func(string) *core.VerifiedEntry) (int64, error) {
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
			return refuseAll(tx, room, peer, refused, now)
		})
		if err == nil && a.observed {
			x.stored(room, peer, p.order)
		}
		return n, err
	}
	res, err := r.ingestWith(ctx, rl, gen, rawBytes(resp.Log.Entries), nil, commit)
	if err != nil {
		return appliedResp{}, err
	}
	if err := r.countInvalid(ctx, rl, peer, res, now); err != nil {
		return appliedResp{}, err
	}
	return a, nil
}
