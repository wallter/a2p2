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

	// refused is, per room and peer, the server's log heads when it last
	// answered not_member: this machine sends its whole log again only
	// once they have changed (PROTOCOL-next 4.5).
	mu      sync.Mutex
	refused map[[2]string][]string
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
	x := &Exchange{r: r, t: t, pageLimit: o.PageLimit, offerLimit: o.OfferLimit, refused: map[[2]string][]string{}, latest: map[[2]string]int64{}}
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
// here, as the model's observe does.
func observe(tx *store.Tx, room, peer string, st members.State, rows []store.Observation, log store.PeerLog, at time.Time) error {
	all := append(slices.Clone(rows), store.Observation{Author: "", Log: &log})
	if err := tx.Observe(room, peer, at, all); err != nil {
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

// built is a response as step 8 built it, and what it was built from.
type built struct {
	rl    *roomLog
	gen   int64
	ver   int64 // store.Changes the response reflects
	resp  *PullResponse
	items []pageItem
}

// known is a summary of this machine computed at store version ver, and,
// for a short-form request, whether it matched.
type known struct {
	own  summary
	ver  int64
	ok   bool
	same bool
}

// Serve answers one pull request from peer, the node id the transport
// authenticated, in the order of PROTOCOL-next 4.4: the request's log
// entries (from a non-member only if they make it a member), the peer a
// member now, its offer and its summary stored, all in one transaction;
// then the response, built against the peer's summary within its
// permission; then, at the point where a held response is released, the
// room's generation and the peer's membership checked again and the
// response built again if anything was written since [RM2].
func (x *Exchange) Serve(ctx context.Context, peer string, q *PullRequest) (*PullResponse, error) {
	if perr := q.validate(); perr != nil {
		return nil, perr
	}
	if !core.ValidNode(peer) || peer == x.r.m.ID {
		return nil, errNotMember
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
		return nil, notMember(nil)
	}
	now := r.m.now()
	order := x.next() // received now: later than anything stored
	heads0, _ := rl.logHeads()
	wasMember := slices.Contains(rl.st.Members, peer)
	member := false
	var k known
	refused := map[string]int{}
	commit := func(ctx context.Context, b *store.LogBatch, st members.State, look func(string) *core.VerifiedEntry) (int64, error) {
		member = slices.Contains(st.Members, peer)
		if !member && !wasMember {
			return 0, errNotMember // T4: a non-member's entries count only if they make it a member
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
			if _, err := r.place(tx, rl, st, look, q.Offer, refused, now); err != nil {
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
			k = known{own: own, ok: true}
			plog := store.PeerLog{Heads: q.Log.Heads, Extra: q.Log.Extra}
			switch {
			case q.Sum != "":
				// Equal sums and log heads: the peer holds what this machine
				// holds, as of now.
				if k.same = b == nil && sameSet(heads0, q.Log.Heads) && own.sum() == q.Sum; k.same {
					if err := observe(tx, rl.id, peer, st, own.rows(), plog, now); err != nil {
						return err
					}
				} else if err := tx.ObserveLog(rl.id, peer, now, plog); err != nil {
					return err
				}
			default:
				old, err := tx.Reader().Observed(rl.id, peer)
				if err != nil {
					return err
				}
				om, _ := observedMap(old)
				rows := observation(q.Authors, q.Authors, q.Digests, q.Detail, own, om)
				if err := observe(tx, rl.id, peer, st, rows, plog, now); err != nil {
					return err
				}
			}
			return refuseAll(tx, rl.id, peer, refused, now)
		})
		k.ver = ver
		if err == nil && member {
			x.stored(rl.id, peer, order)
		}
		return n, err
	}
	res, err := r.ingestWith(ctx, rl, 0, rawBytes(q.Log.Entries), nil, commit)
	if err != nil {
		if isNotMember(err) {
			return nil, notMember(heads0)
		}
		return nil, unavailable(err)
	}
	if err := r.countInvalid(ctx, rl, peer, res, now); err != nil {
		return nil, unavailable(err)
	}
	if !member {
		heads, _ := rl.logHeads()
		return nil, notMember(heads)
	}
	b, err := x.respond(ctx, rl, peer, q, k)
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

// respond is step 8: for a short-form request, same or full; otherwise
// snap and differ for a request without detail, or this machine's detail
// for the request's detail authors and their page; and the log entries the
// peer lacks; all within the response limit. k is a summary to reuse if
// nothing was written since it was made. r.mu is held.
func (x *Exchange) respond(ctx context.Context, rl *roomLog, peer string, q *PullRequest, k known) (*built, error) {
	r := x.r
	st := rl.st
	heads, extra := rl.logHeads()
	b := &built{rl: rl, gen: rl.gen}
	ver := r.m.st.Changes()
	reuse := k.ok && k.ver == ver
	b.ver = ver
	withView := func(fn func(v *roomView) error) error {
		return r.m.view(ctx, rl.id, func(v *roomView) error {
			if !reuse {
				own, err := r.summarize(v, st)
				if err != nil {
					return err
				}
				k = known{own: own, ok: true, same: sameSet(heads, q.Log.Heads) && own.sum() == q.Sum}
			}
			if fn != nil {
				return fn(v)
			}
			return nil
		})
	}
	resp := &PullResponse{V: PullVersion, Log: PullLog{Heads: heads, Extra: extra}}
	b.resp = resp
	entries := func(budget int) {
		resp.Log.Entries, resp.Log.More = rl.entriesFor(q.Log.Heads, q.Log.Extra, true, budget)
	}
	if !reuse && q.Detail == nil {
		if err := withView(nil); err != nil {
			return nil, err
		}
	}
	if q.Sum != "" {
		if k.same {
			resp.Same = true
			return b, nil
		}
		resp.Full = true
		entries(maxResponseBytes - jsonSize(resp) - wireMargin)
		return b, nil
	}
	resp.Authors = slices.Sorted(slices.Values(st.Admitted))
	digests := func() {
		resp.Digests = map[string]string{}
		for _, a := range q.Authors {
			if d, ok := k.own.digests[a]; ok {
				resp.Digests[a] = d
			}
		}
		if len(resp.Digests) == 0 {
			resp.Digests = nil
		}
	}
	if q.Detail == nil {
		digests()
		resp.Snap = snapOf(resp.Digests)
		resp.Differ = differ(q.Digests, resp.Digests)
		entries(maxResponseBytes - jsonSize(resp) - wireMargin)
		return b, nil
	}
	reuse = false // the page needs a view: summarize in it
	err := withView(func(v *roomView) error {
		digests()
		resp.Detail = map[string][]store.StreamDetail{}
		var authors []string
		for _, a := range sortedIDs(q.Detail) {
			resp.Detail[a] = k.own.detailOf(a)
			if slices.Contains(q.Authors, a) && slices.Contains(st.Admitted, a) {
				authors = append(authors, a)
			}
		}
		budget := maxResponseBytes - jsonSize(resp) - wireMargin
		entries(budget / 2)
		budget -= wiresSize(resp.Log.Entries)
		var cut bool
		var err error
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
		return nil, notMember(nil)
	case rl != b.rl || rl.gen != b.gen:
		return nil, pullErr(503, CodeUnavailable, "room changed")
	case !slices.Contains(rl.st.Members, peer):
		heads, _ := rl.logHeads()
		return nil, notMember(heads)
	}
	if r.m.st.Changes() != b.ver {
		nb, err := x.respond(ctx, rl, peer, q, known{})
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
	// FullForm opens every round in the full form, never with the short
	// form's sum.
	FullForm bool
}

// PullStats says what one exchange did.
type PullStats struct {
	Requests  int // requests sent
	Responses int // responses applied
	Rounds    int
	Stored    int // records stored from pages
	Repaired  int // texts repaired from pages
	Entries   int // responses whose log entries changed the room log
	Offered   int // records offered
	Same      int // rounds answered same to the short form
	Stalled   int // continuations ended because they did not advance
	TooLarge  int // authors whose detail alone did not fit in a request
	Capped    bool
}

// Pull runs one exchange with peer for room: rounds of a request with
// this machine's sum or digests (and an offer and the log entries the
// peer lacks), then requests carrying this machine's detail for the
// authors whose digests differ, 16 at a time within the request limit,
// each answered with a page, repeated while something was cut and the
// continuation advances. A round that stored nothing on either side ends
// the exchange (PROTOCOL-next 4.3, step 3). Each response is applied in
// one transaction, only if the room's generation is unchanged: its log
// entries, then its page under the gap-free rule [R4] if the peer is still
// a member here [RM1], then what the peer said it holds [R5].
func (x *Exchange) Pull(ctx context.Context, room, peer string, o PullOpts) (PullStats, error) {
	var st PullStats
	gen, err := x.r.Gen(room)
	if err != nil {
		return st, err
	}
	for st.Rounds < pullRoundsPerPull {
		st.Rounds++
		progress, err := x.round(ctx, room, peer, gen, o, &st)
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

// Forms of a request.
const (
	formShort  = iota // sum in place of authors and digests
	formFirst         // authors and digests: the first of a round
	formDetail        // with detail for some authors of the round's differ
)

// prepared is one request and what it was built from.
type prepared struct {
	req      *PullRequest
	own      summary
	old      map[string]store.Observation
	oldLog   *store.PeerLog
	at       time.Time // when it was built: the date of what its response tells
	order    int64     // and its count among observations (Exchange.next)
	base     int       // its size without entries and offer
	sentNew  int       // records offered that were not offered before
	offerCut bool
	logCut   bool
	partial  bool // the log entries left out some the peer may lack
}

// prepare builds a request of form, with detail for chunk (and snap), with
// an offer if offer is set (built against base, the peer's summary as
// observed when the round began, or as last observed if base is nil, less
// what sent holds: one offer for the round, sent in pages), and with the log entries not at or before peerLog
// (the peer's heads; nil: as last observed; fullLog: all), all within the
// request limit: detail first, then entries, then the offer.
func (x *Exchange) prepare(ctx context.Context, room, peer string, gen int64, form int, chunk []string, snap string,
	offer bool, base map[string]store.Observation, sent map[string]bool, peerLog *store.PeerLog, fullLog bool) (prepared, error) {
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
		p.old, p.oldLog = observedMap(obs)
		pl := peerLog
		if pl == nil {
			pl = p.oldLog
		}
		q := &PullRequest{V: PullVersion, Room: room, Log: PullLog{Heads: heads, Extra: extra}}
		if form == formShort {
			q.Sum = own.sum()
		} else {
			q.Authors, q.Digests = own.authors, own.digests
			if len(q.Digests) == 0 {
				q.Digests = nil
			}
		}
		if form == formDetail {
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
		offering := offer && form != formShort && len(base) > 0
		entryBudget := budget
		if offering {
			entryBudget = budget / 2
		}
		if fullLog || pl == nil {
			q.Log.Entries, p.logCut = rl.entriesFor(nil, nil, false, entryBudget)
		} else {
			q.Log.Entries, p.logCut = rl.entriesFor(pl.Heads, pl.Extra, true, entryBudget)
			p.partial = true
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
				p.sentNew = len(items)
				q.OfferMore, p.offerCut = cut && len(items) > 0, cut
			}
		}
		p.req = q
		return nil
	})
	return p, err
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
// progress: a record, text or entry stored on either side (this side's
// from what it applied, the peer's from its digests and log heads changing
// since they were last observed or during the round). Cuts only justify
// the next request of the round; one that does not advance ends quietly.
func (x *Exchange) round(ctx context.Context, room, peer string, gen int64, o PullOpts, st *PullStats) (bool, error) {
	sent := map[string]bool{}
	var peerLog *store.PeerLog
	var base map[string]store.Observation // the observation the round's offer is built on
	progress := false
	send := func(form int, chunk []string, snap string, fullLog bool) (prepared, *PullResponse, error) {
		p, err := x.prepare(ctx, room, peer, gen, form, chunk, snap, o.Offer, base, sent, peerLog, fullLog)
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
		resp, err := x.t.Pull(ctx, peer, p.req)
		if err != nil {
			return p, nil, err
		}
		if err := checkResponse(resp, p.req); err != nil {
			return p, nil, err
		}
		return p, resp, nil
	}
	apply := func(p prepared, resp *PullResponse) (appliedResp, error) {
		// What the peer sent, it holds: never offer it back this round.
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

	form := formFirst
	if !o.FullForm && !o.Offer {
		form = formShort
	}
	p, resp, err := x.open(send, room, peer, form)
	if err != nil {
		return false, err
	}
	if resp.Same || resp.Full {
		if _, err := apply(p, resp); err != nil {
			return false, err
		}
		if resp.Same {
			st.Same++
			return progress, nil
		}
		if p, resp, err = x.open(send, room, peer, formFirst); err != nil {
			return false, err
		}
	}
	first, before := resp, p
	a, err := apply(p, resp)
	if err != nil {
		return false, err
	}
	if changedFrom(before, resp) {
		progress = true
	}
	// The first request again while its log or offer was cut.
	for resp.Log.More || p.logCut || p.offerCut {
		if !advanced(p, resp, a, first.Log.Heads) && !resp.Log.More {
			st.Stalled++
			break
		}
		prev := resp.Log.Heads
		if p, resp, err = send(formFirst, nil, "", false); err != nil {
			return false, err
		}
		if a, err = apply(p, resp); err != nil {
			return false, err
		}
		if !advanced(p, resp, a, prev) {
			st.Stalled++
			break
		}
		first = resp
	}
	cs, tooLarge := chunks(p.own, p.base, first.Differ)
	if len(tooLarge) > 0 {
		st.TooLarge += len(tooLarge)
		if err := x.refuse(ctx, room, peer, gen, CodeTooLarge, len(tooLarge)); err != nil {
			return false, err
		}
	}
	for _, chunk := range cs {
		for active := chunk; len(active) > 0; {
			prev := resp.Log.Heads
			if p, resp, err = send(formDetail, active, first.Snap, false); err != nil {
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
	if !sameDigests(first.Digests, resp.Digests) || !sameSet(first.Log.Heads, resp.Log.Heads) {
		progress = true
	}
	return progress, nil
}

// open sends the first request of a round. Refused as not a member while
// the entries sent may have left out some the peer lacks (it was restored
// from an older copy), it sends the whole log once, but only if the peer's
// log heads changed since it last refused: a peer that cannot be made to
// see this machine as a member is not asked again in a loop (4.5).
func (x *Exchange) open(send func(int, []string, string, bool) (prepared, *PullResponse, error), room, peer string,
	form int) (prepared, *PullResponse, error) {
	p, resp, err := send(form, nil, "", false)
	var pe *PullError
	if !errors.As(err, &pe) || pe.Code != CodeNotMember {
		if err == nil {
			x.setRefused(room, peer, nil)
		}
		return p, resp, err
	}
	var heads []string
	if pe.Log != nil {
		heads = pe.Log.Heads
	}
	last, seen := x.refusedHeads(room, peer)
	x.setRefused(room, peer, heads)
	if !p.partial || seen && sameSet(last, heads) {
		return p, nil, err
	}
	p, resp, err = send(form, nil, "", true)
	if err == nil {
		x.setRefused(room, peer, nil)
	}
	return p, resp, err
}

func (x *Exchange) refusedHeads(room, peer string) ([]string, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	h, ok := x.refused[[2]string{room, peer}]
	return h, ok
}

func (x *Exchange) setRefused(room, peer string, heads []string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if heads == nil {
		delete(x.refused, [2]string{room, peer})
		return
	}
	x.refused[[2]string{room, peer}] = heads
}

// refuse counts n of code against peer.
func (x *Exchange) refuse(ctx context.Context, room, peer string, gen int64, code string, n int) error {
	_, err := x.r.m.st.Exchange(ctx, room, gen, func(tx *store.Tx) error {
		return tx.Refuse(room, peer, code, n, x.r.m.now())
	})
	return err
}

// changedFrom reports whether the peer's digests or log heads in its
// first response differ from what was last observed of it.
func changedFrom(p prepared, resp *PullResponse) bool {
	if p.oldLog == nil || !sameSet(p.oldLog.Heads, resp.Log.Heads) {
		return true
	}
	for _, a := range p.own.authors {
		d, ok := resp.Digests[a]
		if old, seen := p.old[a]; !seen {
			if ok || slices.Contains(resp.Authors, a) {
				return true
			}
		} else if old.Digest != d {
			return true
		}
	}
	return false
}

func sameDigests(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// checkResponse checks a response against its request before anything of
// it is applied: its form for the request's form, the exact snap and
// differ, the detail of exactly the authors asked and consistent with the
// digests beside it, the limits and the whole size (PROTOCOL-next 4.2).
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
		case resp.Same == resp.Full:
			return bad("same or full")
		case resp.Authors != nil || resp.Digests != nil || resp.Snap != "" || resp.Differ != nil || resp.Detail != nil ||
			resp.Page != nil || resp.More:
			return bad("short answer")
		case resp.Same && len(resp.Log.Entries) > 0:
			return bad("same with entries")
		}
		return checkSize(resp, bad)
	}
	switch {
	case resp.Same || resp.Full:
		return bad("same or full")
	case len(resp.Authors) < 1 || len(resp.Authors) > MaxPullAuthors || !uniqueNodes(resp.Authors):
		return bad("authors")
	}
	for a, d := range resp.Digests {
		if !slices.Contains(q.Authors, a) || !slices.Contains(resp.Authors, a) || len(d) != 64 || !isLowerHex(d) {
			return bad("digests")
		}
	}
	if q.Detail == nil {
		switch {
		case resp.Snap != snapOf(resp.Digests):
			return bad("snap")
		case !slices.Equal(resp.Differ, differ(q.Digests, resp.Digests)):
			return bad("differ")
		case resp.Detail != nil || resp.Page != nil || resp.More:
			return bad("page or detail without detail asked")
		}
		return checkSize(resp, bad)
	}
	if resp.Snap != "" || resp.Differ != nil || len(resp.Detail) != len(q.Detail) {
		return bad("detail form")
	}
	for a, d := range resp.Detail {
		if _, asked := q.Detail[a]; !asked || d == nil || checkDetail(d) != nil {
			return bad("detail")
		}
		if dg, ok := resp.Digests[a]; (len(d) == 0) == ok || ok && digestOf(d) != dg {
			return bad("detail and digest")
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
// peer said it holds, dated when p was built (PROTOCOL-next 4.4).
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
			switch {
			case !x.later(room, peer, p.order):
				// A later observation is stored: this older one is not.
			case resp.Same:
				err = observe(tx, room, peer, st, p.own.rows(), plog, p.at)
				a.observed = true
			case resp.Full:
				err = tx.ObserveLog(room, peer, p.at, plog)
				a.observed = true
			default:
				a.observed = true
				var old []store.Observation
				if old, err = tx.Reader().Observed(room, peer); err == nil {
					om, _ := observedMap(old)
					rows := observation(resp.Authors, p.req.Authors, resp.Digests, resp.Detail, p.own, om)
					err = observe(tx, room, peer, st, rows, plog, p.at)
				}
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
