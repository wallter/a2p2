package delivery

import "sort"

// Reason says why a held record has no body. Every reason other than
// Missing rests on evidence the holder checks itself; what another machine
// claims about a body it did not send is never believed.
type Reason uint8

const (
	Live        Reason = iota // the body is here
	Expired                   // this machine's clock passed 30 days once; recorded, never undone
	Withdrawn                 // this machine holds the author's signed withdraw
	NotAdmitted               // this machine was admitted without history and may not read it
	Missing                   // text missing here; any holder can repair it
)

func (r Reason) String() string {
	return [...]string{"live", "expired", "withdrawn", "not-admitted", "missing"}[r]
}

// held is one record as one machine holds it.
type held struct {
	rec     *Record // shared and immutable
	body    string
	dropped Reason
}

// wire is a record as passed between machines: the signed header, and the
// body if the sender sends it. No reason travels: a receiver works out its
// own.
type wire struct {
	rec     *Record
	body    string
	hasBody bool
}

// StreamHave is what a machine holds of one stream: every position in
// [1, Contig], and no other. NoBody lists positions held without a body;
// Want, the subset whose text it would take (text missing here). A server
// offers text only for Want, so no page slot goes on text that would be
// refused.
type StreamHave struct {
	Contig int
	NoBody []int
	Want   []int
}

// Summary is what a machine holds, per stream. It is the whole of a pull
// request, and is sent back with every pull response. Copy reports are
// learned from these summaries and nothing else.
type Summary map[StreamID]StreamHave

// HoldsBody reports whether the summary says the body at p was held.
func (s Summary) HoldsBody(p Pos) bool {
	h, ok := s[p.Stream]
	if !ok || p.Seq < 1 || p.Seq > h.Contig {
		return false
	}
	for _, n := range h.NoBody {
		if n == p.Seq {
			return false
		}
	}
	return true
}

type known struct {
	Seq  int
	At   Time
	From MachineID // the peer whose report set it
}

type observation struct {
	At      Time
	Tick    int // the world's operation count, for checking reports exactly
	Have    Summary
	Authors map[MachineID]bool // whose records the peer said it takes
}

// agentState is one agent's reading state on its machine. It is durable and
// goes back in time with a restored backup.
type agentState struct {
	start map[StreamID]int // positions at or below were before this agent's time
	// early, at or below start: a message above it and at or below start
	// is in the agent's time only if it is addressed to the agent (an
	// agent registered after a brief was sent to it; Register, inTime).
	// It is start for an agent the machine began with.
	early   map[StreamID]int
	cursor  map[StreamID]int      // every position at or below is resolved for this agent
	seen    map[string]bool       // entry keys the agent confirmed
	seenMsg map[RecordID]RecordID // message -> version of its text the agent confirmed
	// heads: per author, the confirmed records no other confirmed record
	// follows (one, unless the author wrote on two branches after a restore).
	heads map[MachineID][]RecordID
	told  []RecordID // messages it was shown whose author's admission was voided
	// level is how closely the agent follows the room (levels.go); skip
	// holds the messages a level change marked skipped.
	level Level
	skip  map[RecordID]bool
	// receipts holds what full inbox listings showed the agent (List):
	// entry key -> the version listed. Seen confirms only what has one.
	receipts map[string]RecordID
	// shown holds the messages whose text a read showed the agent: an
	// inbox listing (List) or a thread, branch, recent or skipped view
	// (Showed). The agent may report them handled. It is not seen and
	// moves no cursor.
	shown map[RecordID]bool
	// shownVer: the versions whose text a read showed; an agent fetches
	// only files of a version it was shown (attach.go).
	shownVer map[RecordID]bool
}

func newAgent(start map[StreamID]int) *agentState {
	return &agentState{
		start:    copyInts(start),
		early:    copyInts(start),
		cursor:   copyInts(start),
		seen:     map[string]bool{},
		seenMsg:  map[RecordID]RecordID{},
		heads:    map[MachineID][]RecordID{},
		skip:     map[RecordID]bool{},
		shown:    map[RecordID]bool{},
		shownVer: map[RecordID]bool{},
		receipts: map[string]RecordID{},
	}
}

func (a *agentState) clone() *agentState {
	b := *a
	b.start, b.early, b.cursor = copyInts(a.start), copyInts(a.early), copyInts(a.cursor)
	b.seen = make(map[string]bool, len(a.seen))
	for k, v := range a.seen {
		b.seen[k] = v
	}
	b.seenMsg = make(map[RecordID]RecordID, len(a.seenMsg))
	for k, v := range a.seenMsg {
		b.seenMsg[k] = v
	}
	b.heads = copyHeads(a.heads)
	b.told = append([]RecordID(nil), a.told...)
	b.skip = make(map[RecordID]bool, len(a.skip))
	for k, v := range a.skip {
		b.skip[k] = v
	}
	b.receipts = make(map[string]RecordID, len(a.receipts))
	for k, v := range a.receipts {
		b.receipts[k] = v
	}
	b.shown = make(map[RecordID]bool, len(a.shown))
	for k, v := range a.shown {
		b.shown[k] = v
	}
	b.shownVer = make(map[RecordID]bool, len(a.shownVer))
	for k, v := range a.shownVer {
		b.shownVer[k] = v
	}
	return &b
}

// store is everything a machine keeps on disk. A crash keeps it; a restore
// replaces it with an older copy; a lost disk loses it.
type store struct {
	recs      map[RecordID]*held
	at        map[StreamID]map[int]RecordID
	contig    map[StreamID]int
	adm       Admission
	withdrawn map[RecordID][]RecordID
	replaced  map[RecordID][]RecordID
	handled   map[RecordID][]RecordID
	children  map[RecordID][]RecordID
	agents    map[string]*agentState
	known     map[StreamID]known
	obs       map[MachineID]observation
	view      view // who this machine believes is, and was, a member
	files     *fileState
}

func newStore() *store {
	return &store{
		recs:      map[RecordID]*held{},
		at:        map[StreamID]map[int]RecordID{},
		contig:    map[StreamID]int{},
		withdrawn: map[RecordID][]RecordID{},
		replaced:  map[RecordID][]RecordID{},
		handled:   map[RecordID][]RecordID{},
		children:  map[RecordID][]RecordID{},
		agents:    map[string]*agentState{},
		known:     map[StreamID]known{},
		obs:       map[MachineID]observation{},
		view:      newView(),
		files:     newFileState(),
	}
}

func (s *store) clone() *store {
	c := newStore()
	for k, v := range s.recs {
		h := *v
		c.recs[k] = &h
	}
	for k, v := range s.at {
		m := make(map[int]RecordID, len(v))
		for n, id := range v {
			m[n] = id
		}
		c.at[k] = m
	}
	c.contig = copyInts(s.contig)
	c.adm = s.adm // replaced, never modified
	c.view = s.view.clone()
	c.files = s.files.clone()
	c.withdrawn = copyLists(s.withdrawn)
	c.replaced = copyLists(s.replaced)
	c.handled = copyLists(s.handled)
	c.children = copyLists(s.children)
	for k, v := range s.agents {
		c.agents[k] = v.clone()
	}
	for k, v := range s.known {
		c.known[k] = v
	}
	for k, v := range s.obs {
		c.obs[k] = v // summaries are never modified after creation
	}
	return c
}

func copyInts(m map[StreamID]int) map[StreamID]int {
	c := make(map[StreamID]int, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func copyHeads(m map[MachineID][]RecordID) map[MachineID][]RecordID {
	c := make(map[MachineID][]RecordID, len(m))
	for k, v := range m {
		c[k] = append([]RecordID(nil), v...)
	}
	return c
}

func copyLists(m map[RecordID][]RecordID) map[RecordID][]RecordID {
	c := make(map[RecordID][]RecordID, len(m))
	for k, v := range m {
		c[k] = append([]RecordID(nil), v...)
	}
	return c
}

// Machine is one computer running the daemon.
type Machine struct {
	ID     MachineID
	key    string
	Skew   Time // this machine's clock minus the true time
	Awake  bool
	Gone   bool     // lost its disk; replaced by a new machine
	agents []string // agent labels, in a fixed order

	st *store

	// headFile is the rejected alternative's marker: the machine's own stream
	// head, kept in a file beside the database but outside its backup.
	headFile *Pos

	// Not durable: the stream this run of the daemon writes to.
	epoch uint64
	seq   int
	last  *Record

	// NoInbound: the machine cannot accept connections; it only pulls.
	NoInbound bool

	// Faults injected by tests: records this machine leaves out when serving.
	withhold map[RecordID]bool
}

func (m *Machine) clone() *Machine {
	c := *m
	c.agents = append([]string(nil), m.agents...)
	c.st = m.st.clone()
	c.withhold = map[RecordID]bool{}
	for k, v := range m.withhold {
		c.withhold[k] = v
	}
	return &c
}

func (m *Machine) clock(now Time) Time { return now + m.Skew }

func (m *Machine) contigOf(s StreamID) int { return m.st.contig[s] }

func (m *Machine) get(id RecordID) *held { return m.st.recs[id] }

func (m *Machine) at(p Pos) *held {
	if id, ok := m.st.at[p.Stream][p.Seq]; ok {
		return m.st.recs[id]
	}
	return nil
}

// summary is what this machine holds.
func (m *Machine) summary() Summary {
	s := Summary{}
	for sid, c := range m.st.contig {
		h := StreamHave{Contig: c}
		for n := 1; n <= c; n++ {
			switch m.st.recs[m.st.at[sid][n]].dropped {
			case Live:
			case Missing:
				h.NoBody, h.Want = append(h.NoBody, n), append(h.Want, n)
			default:
				h.NoBody = append(h.NoBody, n)
			}
		}
		s[sid] = h
	}
	return s
}

// mayRead says whether a machine admitted as adm may have r's text. Without
// history it may not have anything at or before the admitter's positions.
// (A cut-off by creation time as well was tried and rejected: with clocks
// that differ it hides messages written after the admission.)
func mayRead(adm Admission, r *Record) bool {
	return adm.History || r.Seq > adm.Heads[r.Stream()]
}

// withdrawnRef reports whether this machine holds a withdraw of ref signed
// by author.
func (m *Machine) withdrawnRef(ref RecordID, author MachineID) bool {
	for _, w := range m.st.withdrawn[ref] {
		if h := m.st.recs[w]; h != nil && h.rec.Author == author {
			return true
		}
	}
	return false
}

// textGone says why this machine must not hold r's text, from its own
// evidence, or Live if it may. Permission comes before expiry: text from
// before this machine's time is NotAdmitted however old it is.
func (m *Machine) textGone(r *Record, now Time) Reason {
	switch {
	case r.Kind == Message && m.withdrawnRef(r.ID, r.Author),
		r.Kind == Replace && m.withdrawnRef(r.Ref, r.Author):
		return Withdrawn
	case !mayRead(m.st.adm, r):
		return NotAdmitted
	case m.clock(now) >= r.Created+Retention:
		return Expired
	}
	return Live
}

// reasonHolds says whether this machine's own evidence supports a held
// record's reason for having no body.
func (m *Machine) reasonHolds(h *held, now Time) bool {
	r := h.rec
	switch h.dropped {
	case Withdrawn:
		return (r.Kind == Message && m.withdrawnRef(r.ID, r.Author)) || (r.Kind == Replace && m.withdrawnRef(r.Ref, r.Author))
	case Expired:
		return true // a recorded local fact: the clock passed 30 days here once
	case NotAdmitted:
		return !mayRead(m.st.adm, r)
	case Missing:
		return m.textGone(r, now) == Live
	}
	return true
}

// setBody applies the rules to one held record: a body is kept only when
// nothing says it must go; a missing body stays Missing unless there is
// evidence why it is gone.
func (m *Machine) setBody(h *held, body string, hasBody bool, now Time) {
	if !hasBody && h.rec.BodyHash == hashBody("") {
		body, hasBody = "", true // an empty body needs no copy
	}
	if g := m.textGone(h.rec, now); h.dropped == Expired && g != Withdrawn && g != NotAdmitted {
		return // expired here stays expired, whatever the clock says later
	}
	if g := m.textGone(h.rec, now); g != Live {
		h.dropped, h.body = g, ""
		return
	}
	if hasBody {
		h.dropped, h.body = Live, body
	} else {
		h.dropped, h.body = Missing, ""
	}
}

// prune re-checks every body against this machine's clock.
func (m *Machine) prune(now Time) {
	c := m.clock(now)
	for _, h := range m.st.recs {
		if (h.dropped == Live || h.dropped == Missing) && c >= h.rec.Created+Retention {
			h.dropped, h.body = Expired, ""
		}
	}
	m.dropFiles(now)
}

// descends reports whether a comes after b in its author's history: b is
// earlier in a's stream, or in a stream a's stream links back to.
func (m *Machine) descends(a, b *Record) bool {
	if a.Author != b.Author || a.ID == b.ID {
		return false
	}
	// Walk back through streams; reach[s] is the highest position of s that
	// a follows.
	reach := map[StreamID]int{a.Stream(): a.Seq - 1}
	todo := []StreamID{a.Stream()}
	for len(todo) > 0 {
		s := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if reach[s] < 1 && s != a.Stream() { // a itself follows its own links
			continue
		}
		first := m.at(Pos{s, 1})
		if first == nil {
			continue
		}
		for _, l := range first.rec.Links {
			if l.At.Seq > reach[l.At.Stream] {
				reach[l.At.Stream] = l.At.Seq
				todo = append(todo, l.At.Stream)
			}
		}
	}
	return b.Seq <= reach[b.Stream()]
}

// ownHeads are this machine's own records that no other own record it holds
// follows: what a new stream links to.
func (m *Machine) ownHeads() []Link {
	var cands []*Record
	for s, c := range m.st.contig {
		if s.Author == m.ID && c > 0 {
			cands = append(cands, m.at(Pos{s, c}).rec)
		}
	}
	var out []Link
	for _, a := range cands {
		top := true
		for _, b := range cands {
			if m.descends(b, a) {
				top = false
				break
			}
		}
		if top {
			out = append(out, Link{a.ID, a.Pos()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// insert stores a record at the next position of its stream. The caller has
// checked that it belongs there.
func (m *Machine) insert(r *Record, body string, hasBody bool, now Time) {
	h := &held{rec: r}
	st := m.st
	st.recs[r.ID] = h
	s := r.Stream()
	if st.at[s] == nil {
		st.at[s] = map[int]RecordID{}
	}
	st.at[s][r.Seq] = r.ID
	st.contig[s] = r.Seq
	switch r.Kind {
	case Withdraw:
		st.withdrawn[r.Ref] = append(st.withdrawn[r.Ref], r.ID)
		if t := st.recs[r.Ref]; t != nil && t.rec.Kind != Message {
			break // a withdraw refers to a message; naming anything else does nothing
		}
		for _, id := range append([]RecordID{r.Ref}, st.replaced[r.Ref]...) {
			if t := st.recs[id]; t != nil && t.rec.Author == r.Author {
				m.setBody(t, "", false, now)
			}
		}
	case Replace:
		st.replaced[r.Ref] = append(st.replaced[r.Ref], r.ID)
	case Handled:
		st.handled[r.Ref] = append(st.handled[r.Ref], r.ID)
	case Message:
		if r.Parent != "" {
			st.children[r.Parent] = append(st.children[r.Parent], r.ID)
		}
	}
	m.setBody(h, body, hasBody, now)
	m.dropFiles(now) // a withdraw or a replacement may end a file's reference (F1)
}

// accept is the receiving rule. A record is stored only if it verifies and
// is the next position of its stream, its predecessor matches, and (for the
// first record of a stream) the record it links to is held. So every
// machine holds a gap-free prefix of every stream, closed under links. A
// body for a record held with its text missing repairs it.
func (m *Machine) accept(w *World, it wire) (stored, fork bool) {
	r := it.rec
	if !m.st.view.ever[r.Author] || r.verify(w.keys, it.body, it.hasBody) != nil {
		return false, false
	}
	if h, ok := m.st.recs[r.ID]; ok {
		if h.dropped == Missing && it.hasBody {
			m.setBody(h, it.body, true, w.Now)
			return h.dropped == Live, false
		}
		return false, false
	}
	s := r.Stream()
	c := m.contigOf(s)
	if r.Seq <= c {
		return false, m.st.at[s][r.Seq] != r.ID
	}
	if r.Seq != c+1 {
		return false, false
	}
	if r.Seq > 1 && m.st.at[s][r.Seq-1] != r.Prev {
		return false, true // a different history at the same position: a fork
	}
	for _, l := range r.Links {
		if x := m.at(l.At); x == nil || x.rec.ID != l.ID {
			return false, false
		}
	}
	m.insert(r, it.body, it.hasBody, w.Now)
	return true, false
}

// observe records what a peer said it held, and the highest positions it
// knows of, so gaps can be shown. A known position is a hint: a higher
// report from any peer raises it, and the peer whose report set it lowers
// it by reporting less, or nothing, of a stream; a peer reports only the
// authors it takes, so one it does not take drops its hints too
// (store.Tx.NoteKnown). What it lowers or drops falls back to the highest
// report another peer's last observation holds, so no peer erases
// another's hint (store.Tx.lowerKnown). The implementation also bounds
// the hints one peer can set for streams that hold nothing
// (store.MaxHintedStreams); honest machines here never reach that bound.
func (m *Machine) observe(peer MachineID, s Summary, authors map[MachineID]bool, now Time, tick int) {
	m.st.obs[peer] = observation{At: now, Tick: tick, Have: s, Authors: authors}
	takes := func(a MachineID) bool { return authors == nil || authors[a] }
	for sid, h := range s {
		if !takes(sid.Author) {
			continue
		}
		switch k := m.st.known[sid]; {
		case h.Contig > k.Seq:
			m.st.known[sid] = known{Seq: h.Contig, At: now, From: peer}
		case k.From == peer && h.Contig < k.Seq:
			if o, ok := m.otherKnown(sid, peer); ok && o.Seq > h.Contig {
				m.st.known[sid] = o
			} else {
				m.st.known[sid] = known{Seq: h.Contig, At: now, From: peer}
			}
		}
	}
	for sid, k := range m.st.known {
		if _, ok := s[sid]; k.From == peer && (!ok || !takes(sid.Author)) {
			if o, ok := m.otherKnown(sid, peer); ok {
				m.st.known[sid] = o
			} else {
				delete(m.st.known, sid)
			}
		}
	}
}

// otherKnown is the highest position of stream sid that a peer other than
// peer reports in its last observation, the latest such report on a tie;
// false when none reports it above 0.
func (m *Machine) otherKnown(sid StreamID, peer MachineID) (known, bool) {
	var best known
	for q, o := range m.st.obs {
		if q == peer || o.Authors != nil && !o.Authors[sid.Author] {
			continue
		}
		h, ok := o.Have[sid]
		if !ok || h.Contig <= 0 {
			continue
		}
		if h.Contig > best.Seq || h.Contig == best.Seq && (o.At > best.At || o.At == best.At && q < best.From) {
			best = known{Seq: h.Contig, At: o.At, From: q}
		}
	}
	return best, best.Seq > 0
}

// page is what server sends puller. First, text for positions the puller
// holds without it, where the server has it. Then every record past the
// puller's position in each stream, predecessors first, stopping a stream
// at the first record the server will not send, so that everything sent can
// be stored and a refused record can never block another stream. Text the
// puller may not read, by the server's view of its permission, is sent as a
// header only. Streams by authors the puller said it does not take are left
// out (takes nil: not known, send all).
func (w *World) page(srv *Machine, puller MachineID, req Summary, takes map[MachineID]bool) []wire {
	adm := srv.st.view.adm[puller]
	var streams []StreamID
	for s := range srv.st.contig {
		if takes == nil || takes[s.Author] {
			streams = append(streams, s)
		}
	}
	sort.Slice(streams, func(i, j int) bool { return streams[i].less(streams[j]) })
	mk := func(h *held) wire {
		it := wire{rec: h.rec}
		if h.dropped == Live && mayRead(adm, h.rec) {
			it.body, it.hasBody = h.body, true
		}
		if w.tamper != nil {
			it = w.tamper(srv.ID, it)
		}
		return it
	}
	var out []wire
	for _, s := range streams {
		for _, n := range req[s].Want {
			if h := srv.at(Pos{s, n}); h != nil && h.dropped == Live && !srv.withhold[h.rec.ID] && mayRead(adm, h.rec) {
				out = append(out, mk(h))
			}
		}
	}
	per := map[StreamID][]wire{}
	inPage := map[RecordID]bool{}
	for _, s := range streams {
		for n := req[s].Contig + 1; n <= srv.st.contig[s]; n++ {
			id := srv.st.at[s][n]
			if srv.withhold[id] {
				break
			}
			per[s] = append(per[s], mk(srv.st.recs[id]))
			inPage[id] = true
		}
	}
	// Order: within a stream by sequence; a stream's first record only after
	// the record it links to, if that is also in the page. A first record
	// whose link the puller lacks and the page does not carry is dropped.
	idx := map[StreamID]int{}
	emitted := map[RecordID]bool{}
	for progress := true; progress; {
		progress = false
		for _, s := range streams {
			for idx[s] < len(per[s]) {
				it := per[s][idx[s]]
				ready := true
				for _, l := range it.rec.Links {
					if !emitted[l.ID] && (inPage[l.ID] || l.At.Seq > req[l.At.Stream].Contig) {
						ready = false
					}
				}
				if !ready {
					break
				}
				out = append(out, it)
				emitted[it.rec.ID] = true
				idx[s]++
				progress = true
			}
		}
	}
	return out
}
