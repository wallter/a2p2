package node

// Reading records [R7]: the inbox walk from each agent's cursors, Seen, and
// the thread and branch views. The rules are those of
// internal/model/delivery/inbox.go, over the store's records; the
// differential test in differential_test.go holds the two together.
//
// Expiry at read time [R6]: a read treats text as expired when its durable
// mark says so or when this machine's clock is at or past created + 30
// days. A read never writes; when it finds text expired only by the clock
// it sets PruneDue, and Prune then makes the mark durable: the daemon runs
// it at start, hourly and when PruneDue is set, and every write and Seen
// runs it first when it is due. Text shown as expired therefore stays
// expired when the clock is set back, unless the clock goes back before
// any of those runs (at most the daemon's prune interval).

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// Entry kinds, as the local API shows them (PROTOCOL-next section 7).
const (
	EntryMessage   = "message"   // a message, with its current text
	EntryWithdrawn = "withdrawn" // a message you were shown was withdrawn
	EntryReplaced  = "replaced"  // a message you were shown has new text
	EntryExpired   = "expired"   // a message for you expired before you were shown it
	EntryVoided    = "voided"    // a message you were shown is gone: its author's admission never counted
)

// Parent states of a reply (PROTOCOL-next section 7).
const (
	ParentHere      = "here"
	ParentMissing   = "missing" // not held, or its text not here
	ParentExpired   = "expired"
	ParentWithdrawn = "withdrawn"
)

// Entry is one item an agent is shown.
type Entry struct {
	// Key is what Seen confirms, with Version.
	Key string
	// Version is the record whose text is shown (the message or a
	// replace); empty for notices without text.
	Version string
	Kind    string
	// ID is the message concerned; Record is the record at this position
	// (the same for a message; the withdraw or replace for a notice).
	ID       string
	Record   string
	Room     string
	From     string // agent id, label@short node
	FromNode string
	// AuthorRemoved: the author is not a member of the room now, in this
	// machine's view [RM1]. Always false in a room without a log.
	AuthorRemoved bool
	To            []core.Addressee
	Parent        string
	Thread        string
	ParentState   string // empty when there is no parent
	// Late: the agent already confirmed a record of this author that this
	// one does not follow in the author's history (a restored author that
	// wrote before catching up).
	Late    bool
	Created time.Time
	Text    string
	Choices []string
	Epoch   string
	Seq     int64
}

// Wait is a stream the inbox cannot pass yet (PROTOCOL-next Wait).
type Wait struct {
	Room   string
	Author string
	Epoch  string
	Have   int64 // positions held
	Known  int64 // highest position known of
	Since  time.Time
	Reason string // "text", "link" or "gap"
	At     string // for "text": the record whose text is missing
}

// Wait reasons.
const (
	WaitText = "text"
	WaitLink = "link"
	WaitGap  = "gap"
)

// InboxView is what Inbox returns.
type InboxView struct {
	Entries []Entry
	Waiting []Wait
}

// Why Seen refused one confirmation.
const (
	SeenNotShown       = "not_shown"       // Inbox would not show this key to the agent now
	SeenVersionChanged = "version_changed" // it would, with other text: confirm what Inbox shows now
)

// SeenResult is the outcome for one confirmation given to Seen.
type SeenResult struct {
	Key       string
	Version   string
	Confirmed bool
	Reason    string // when not confirmed
}

// Inbox shows agent what it has not confirmed in room, in each author's
// order, and what the walk waits for. It marks nothing [R7].
func (m *Machine) Inbox(ctx context.Context, room, agent string) (InboxView, error) {
	var out InboxView
	err := m.view(ctx, room, func(v *roomView) error {
		if err := v.requireAgent(agent); err != nil {
			return err
		}
		g, err := v.agent(agent)
		if err != nil {
			return err
		}
		es, _, ws, err := v.walk(g, agent, false)
		out = InboxView{Entries: es, Waiting: ws}
		return err
	})
	return out, err
}

// Seen confirms entries for agent [R7]. A confirmation counts only if its
// key and version are exactly an entry Inbox would show the agent now,
// worked out in the same transaction; the others are refused one by one,
// with a reason, and the rest applied. Then the agent's cursors move past
// everything confirmed or not meant for it. Seen is the only thing that
// marks anything seen.
func (m *Machine) Seen(ctx context.Context, room, agent string, reads []store.Read) ([]SeenResult, error) {
	if err := checkAgent(agent); err != nil {
		return nil, err
	}
	if err := m.pruneIfDue(ctx); err != nil {
		return nil, err
	}
	now := m.now()
	var results []SeenResult
	var expiredByClock bool
	err := m.st.Seen(ctx, agent, room, func(rd *store.Reader) (store.SeenPlan, error) {
		v := m.newView(rd, room, now)
		defer func() { expiredByClock = v.expiredByClock }()
		if err := v.load(); err != nil {
			return store.SeenPlan{}, err
		}
		if err := v.requireAgent(agent); err != nil {
			return store.SeenPlan{}, err
		}
		g, err := v.agent(agent)
		if err != nil {
			return store.SeenPlan{}, err
		}
		shown, _, _, err := v.walk(g, agent, false)
		if err != nil {
			return store.SeenPlan{}, err
		}
		byKey := map[string]Entry{}
		for _, e := range shown {
			byKey[e.Key] = e
		}
		before := copyCursor(g.cursor)
		touched := map[string]bool{}
		var p store.SeenPlan
		results = make([]SeenResult, len(reads))
		for i, r := range reads {
			results[i] = SeenResult{Key: r.Key, Version: r.Version}
			e, ok := byKey[r.Key]
			switch {
			case !ok:
				results[i].Reason = SeenNotShown
				continue
			case e.Version != r.Version:
				results[i].Reason = SeenVersionChanged
				continue
			}
			results[i].Confirmed = true
			read := store.Read{Key: e.Key, Version: e.Version, Msg: e.ID}
			g.confirm(read)
			p.Reads = append(p.Reads, read)
			h, err := v.get(e.Record)
			if err != nil {
				return store.SeenPlan{}, err
			}
			if h != nil {
				v.addHead(g.heads, h)
				touched[h.author] = true
			}
		}
		_, pos, _, err := v.walk(g, agent, true)
		if err != nil {
			return store.SeenPlan{}, err
		}
		p.Heads = map[string][]string{}
		for a := range touched {
			p.Heads[a] = g.heads[a]
		}
		for s, n := range pos {
			if before[s] != n {
				p.Cursors = append(p.Cursors, store.Cursor{Author: s.author, Epoch: s.epoch, Start: g.start[s], Pos: n})
			}
		}
		return p, nil
	})
	if expiredByClock {
		m.pruneDue.Store(true)
	}
	return results, err
}

// Thread returns the messages of the thread started by root, root
// included, oldest first: root, if held, must be a message, and a reply
// counts unless its parent or thread is known not to check out (one whose
// parent is not held yet is listed, gap G3). Withdrawn messages, messages
// whose text is not here, and messages whose newest text is still on its
// way (Inbox waits for it) are left out. It marks nothing.
func (m *Machine) Thread(ctx context.Context, room, root string) ([]Entry, error) {
	var out []Entry
	err := m.view(ctx, room, func(v *roomView) error {
		r, err := v.get(root)
		if err != nil || r != nil && r.f.Kind != core.KindMessage {
			return err
		}
		hs, err := v.rd.ByThread(room, root)
		if err != nil {
			return err
		}
		for _, h := range hs {
			x := v.keep(h)
			if v.hidden(x.author) || x.id != root && x.h.Effect == store.EffectBadReference {
				continue
			}
			e, err := v.threadEntry(x)
			if err != nil {
				return err
			}
			if e != nil {
				out = append(out, *e)
			}
		}
		return nil
	})
	sortEntries(out)
	return out, err
}

// Branch returns the message under and the replies below it, to any depth,
// following only replies whose parent and thread check out: a reply whose
// parent is not held appears only in Thread (gap G3). under must be a
// message held here. It marks nothing.
func (m *Machine) Branch(ctx context.Context, room, under string) ([]Entry, error) {
	var out []Entry
	err := m.view(ctx, room, func(v *roomView) error {
		u, err := v.get(under)
		if err != nil || u == nil || u.f.Kind != core.KindMessage {
			return err
		}
		queue := []*rec{u}
		done := map[string]bool{}
		for len(queue) > 0 {
			x := queue[0]
			queue = queue[1:]
			if done[x.id] {
				continue
			}
			done[x.id] = true
			e, err := v.threadEntry(x)
			if err != nil {
				return err
			}
			if e != nil {
				out = append(out, *e)
			}
			kids, err := v.rd.ByParent(room, x.id)
			if err != nil {
				return err
			}
			for _, k := range kids {
				if x := v.keep(k); k.Effect == store.EffectOK && !v.hidden(x.author) {
					queue = append(queue, x)
				}
			}
		}
		return nil
	})
	sortEntries(out)
	return out, err
}

func sortEntries(es []Entry) {
	sort.SliceStable(es, func(i, j int) bool {
		if !es[i].Created.Equal(es[j].Created) {
			return es[i].Created.Before(es[j].Created)
		}
		return es[i].Record < es[j].Record
	})
}

// ---- the room as one transaction sees it ----

type streamKey struct{ author, epoch string }

// rec is one held record with its fields decoded once and its reason as
// read now: the durable mark, or expired if this machine's clock says so.
type rec struct {
	h      store.Held
	f      core.RecordFields
	id     string
	author string
	reason string
}

func (r *rec) stream() streamKey { return streamKey{r.author, r.f.Epoch} }

// roomView reads one room inside one store transaction, at one time,
// caching what it reads.
type roomView struct {
	rd      *store.Reader
	room    string
	self    string
	now     time.Time
	streams []store.Stream
	contig  map[streamKey]int64
	first   map[streamKey]*rec
	byID    map[string]*rec
	absent  map[string]bool
	byPos   map[streamKey]map[int64]*rec
	loaded  map[streamKey]int64 // lowest position loaded of each stream
	byRef   map[string][]*rec
	// expiredByClock: some text read is past retention but not yet marked.
	expiredByClock bool
	// members is the room's member cache, nil for a room without a log
	// (step 3a's rooms), where every author counts.
	members map[string]store.RoomMember
	// hiddenAuthors: some held stream's author is hidden (see hidden).
	hiddenAuthors bool
	// front is this machine's reading frontier (Machine.fronts), and
	// undecided caches, per author, whether a head of it is not held.
	front     readFront
	undecided map[string]bool
}

// undecidedAuthor reports whether a head of author's reading frontier is
// not held: until it is, missing text of author is not shown as expired
// (it may be text this machine may not read: not_admitted).
func (v *roomView) undecidedAuthor(author string) bool {
	if v.front.All || len(v.front.From[author]) == 0 {
		return false
	}
	if u, ok := v.undecided[author]; ok {
		return u
	}
	u, err := headsMissing(v.rd, v.room, author, v.front.From[author])
	if err != nil {
		u = true // fail closed: the read that follows reports the error
	}
	if v.undecided == nil {
		v.undecided = map[string]bool{}
	}
	v.undecided[author] = u
	return u
}

// unseeded reports whether the agent has no cursor in stream k while a
// head of its author's reading frontier is not held: its cursor will be
// seeded at that head when it arrives (Access.Frontier), so until then
// nothing of the stream is shown and nothing waits in it, as an agent
// admitted then starts at the admitter's positions (U3), with or without
// history.
func (v *roomView) unseeded(g *agentState, k streamKey) (bool, error) {
	if _, ok := g.cursor[k]; ok {
		return false, nil
	}
	for _, head := range v.front.From[k.author] {
		h, err := v.get(head)
		if err != nil {
			return false, err
		}
		if h == nil || h.author != k.author {
			return true, nil
		}
	}
	return false, nil
}

// hidden reports whether author's records are hidden here: it is not this
// machine and is not in the room's Admitted set, which can shrink [M13];
// its records are kept, and shown again if it is admitted again.
func (v *roomView) hidden(author string) bool {
	return v.members != nil && author != v.self && !v.members[author].Admitted
}

// removed reports whether author is not a member now [RM1]. This machine
// included: a removed machine may still write locally, and its own reader
// marks those records too.
func (v *roomView) removed(author string) bool {
	return v.members != nil && !v.members[author].Member
}

func (m *Machine) newView(rd *store.Reader, room string, now time.Time) *roomView {
	var front readFront
	if m.fronts != nil {
		front = m.fronts()[room]
	}
	return &roomView{rd: rd, room: room, self: m.ID, now: now, front: front, contig: map[streamKey]int64{},
		first: map[streamKey]*rec{}, byID: map[string]*rec{}, absent: map[string]bool{},
		byPos: map[streamKey]map[int64]*rec{}, loaded: map[streamKey]int64{}, byRef: map[string][]*rec{}}
}

// view runs fn over room in one read-only transaction.
func (m *Machine) view(ctx context.Context, room string, fn func(*roomView) error) error {
	now := m.now()
	return m.st.View(ctx, func(rd *store.Reader) error {
		v := m.newView(rd, room, now)
		defer func() {
			if v.expiredByClock {
				m.pruneDue.Store(true)
			}
		}()
		if err := v.load(); err != nil {
			return err
		}
		return fn(v)
	})
}

func (v *roomView) requireAgent(agent string) error {
	if err := checkAgent(agent); err != nil {
		return err
	}
	ok, err := v.rd.Registered(agent)
	if err == nil && !ok {
		err = ErrUnknownAgent
	}
	return err
}

// load reads the room's streams, ordered as the model's streams(): by
// author, then by when each began, so that streams the history does not
// order come out oldest first. Streams only known of (nothing held) come
// last and hold nothing to walk.
func (v *roomView) load() error {
	ms, err := v.rd.RoomMembers(v.room)
	if err != nil {
		return err
	}
	if len(ms) > 0 {
		v.members = map[string]store.RoomMember{}
		for _, m := range ms {
			v.members[m.Node] = m
		}
	}
	ss, err := v.rd.Streams(v.room)
	if err != nil {
		return err
	}
	for _, s := range ss {
		k := streamKey{s.Author, s.Epoch}
		v.contig[k] = s.Contig
		if s.First.Record != nil {
			v.first[k] = v.keep(s.First)
		}
	}
	sort.SliceStable(ss, func(i, j int) bool {
		a, b := ss[i], ss[j]
		if a.Author != b.Author {
			return a.Author < b.Author
		}
		fa, fb := v.first[streamKey{a.Author, a.Epoch}], v.first[streamKey{b.Author, b.Epoch}]
		if (fa == nil) != (fb == nil) {
			return fb == nil
		}
		if fa != nil && !fa.f.Created.Equal(fb.f.Created) {
			return fa.f.Created.Before(fb.f.Created)
		}
		return a.Epoch < b.Epoch
	})
	v.streams = slices.DeleteFunc(ss, func(s store.Stream) bool {
		if v.hidden(s.Author) {
			v.hiddenAuthors = true
			return true
		}
		return false
	})
	return nil
}

func (v *roomView) keep(h store.Held) *rec {
	id := h.Record.ID()
	if r := v.byID[id]; r != nil {
		return r
	}
	r := &rec{h: h, f: h.Record.Fields(), id: id, author: h.Record.Author(), reason: h.Reason}
	if (r.reason == store.ReasonLive || r.reason == store.ReasonMissing && !v.undecidedAuthor(r.author)) &&
		store.TextExpired(r.f.Created, v.now) {
		r.reason = store.ReasonExpired
		v.expiredByClock = true
	}
	v.byID[id] = r
	k := r.stream()
	if v.byPos[k] == nil {
		v.byPos[k] = map[int64]*rec{}
	}
	v.byPos[k][r.f.Seq] = r
	return r
}

func (v *roomView) get(id string) (*rec, error) {
	if id == "" || v.absent[id] {
		return nil, nil
	}
	if r := v.byID[id]; r != nil {
		if v.hidden(r.author) {
			return nil, nil
		}
		return r, nil
	}
	h, ok, err := v.rd.Record(v.room, id)
	if err != nil || !ok {
		v.absent[id] = err == nil
		return nil, err
	}
	if r := v.keep(h); !v.hidden(r.author) {
		return r, nil
	}
	return nil, nil
}

// at returns the record at position n of stream k, loading the stream from
// n on when it is not loaded yet.
func (v *roomView) at(k streamKey, n int64) (*rec, error) {
	if n < 1 || n > v.contig[k] {
		return nil, nil
	}
	if r := v.byPos[k][n]; r != nil {
		return r, nil
	}
	if lo, ok := v.loaded[k]; !ok || n < lo {
		hs, err := v.rd.RecordsFrom(v.room, k.author, k.epoch, n)
		if err != nil {
			return nil, err
		}
		for _, h := range hs {
			v.keep(h)
		}
		v.loaded[k] = n
	}
	return v.byPos[k][n], nil
}

// refs returns the records whose ref is id, whatever their effect.
func (v *roomView) refs(id string) ([]*rec, error) {
	if rs, ok := v.byRef[id]; ok {
		return rs, nil
	}
	hs, err := v.rd.ByRef(v.room, id)
	if err != nil {
		return nil, err
	}
	rs := make([]*rec, 0, len(hs))
	for _, h := range hs {
		if r := v.keep(h); !v.hidden(r.author) {
			rs = append(rs, r)
		}
	}
	v.byRef[id] = rs
	return rs, nil
}

// counted returns the records of kind k whose ref is id and whose
// reference checks out: only those count [R7].
func (v *roomView) counted(id string, k core.Kind) ([]*rec, error) {
	rs, err := v.refs(id)
	if err != nil {
		return nil, err
	}
	var out []*rec
	for _, r := range rs {
		if r.f.Kind == k && r.h.Effect == store.EffectOK {
			out = append(out, r)
		}
	}
	return out, nil
}

// descends reports whether a comes after b in their author's history: b is
// earlier in a's stream, or in a stream a's stream links back to.
func (v *roomView) descends(a, b *rec) bool {
	if a.author != b.author || a.id == b.id {
		return false
	}
	reach := map[string]int64{a.f.Epoch: a.f.Seq - 1}
	todo := []string{a.f.Epoch}
	for len(todo) > 0 {
		s := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if reach[s] < 1 && s != a.f.Epoch { // a itself follows its own links
			continue
		}
		first := v.first[streamKey{a.author, s}]
		if first == nil {
			continue
		}
		for _, l := range first.f.Links {
			if l.Seq > reach[l.Epoch] {
				reach[l.Epoch] = l.Seq
				todo = append(todo, l.Epoch)
			}
		}
	}
	return b.f.Seq <= reach[b.f.Epoch]
}

// ownHeads are this machine's own records in the room that no other own
// record held follows: what a new stream links to [R2].
func (v *roomView) ownHeads(self string) ([]core.Link, error) {
	var cands []*rec
	for _, s := range v.streams {
		if s.Author != self || s.Contig == 0 {
			continue
		}
		r, err := v.at(streamKey{s.Author, s.Epoch}, s.Contig)
		if err != nil {
			return nil, err
		}
		cands = append(cands, r)
	}
	var out []core.Link
	for _, a := range cands {
		top := true
		for _, b := range cands {
			if v.descends(b, a) {
				top = false
				break
			}
		}
		if top {
			out = append(out, core.Link{Epoch: a.f.Epoch, Seq: a.f.Seq, ID: a.id})
		}
	}
	slices.SortFunc(out, func(a, b core.Link) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// ---- one agent's reading state ----

// agentState is one agent's cursors and heads, with its confirmations read
// on demand through the store's indexes, plus those Seen is adding.
type agentState struct {
	rd      *store.Reader
	agent   string
	room    string
	cursor  map[streamKey]int64
	start   map[streamKey]int64
	heads   map[string][]string
	keys    map[string]*store.Read // nil: not confirmed
	byMsg   map[string][]store.Read
	pending map[string]store.Read
}

func copyCursor(c map[streamKey]int64) map[streamKey]int64 {
	out := make(map[streamKey]int64, len(c))
	for k, n := range c {
		out[k] = n
	}
	return out
}

func copyHeads(h map[string][]string) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, ids := range h {
		out[k] = slices.Clone(ids)
	}
	return out
}

func (v *roomView) agent(agent string) (*agentState, error) {
	cs, err := v.rd.Cursors(agent, v.room)
	if err != nil {
		return nil, err
	}
	g := &agentState{rd: v.rd, agent: agent, room: v.room, cursor: map[streamKey]int64{}, start: map[streamKey]int64{},
		keys: map[string]*store.Read{}, byMsg: map[string][]store.Read{}, pending: map[string]store.Read{}}
	for _, c := range cs {
		k := streamKey{c.Author, c.Epoch}
		g.cursor[k], g.start[k] = c.Pos, c.Start
	}
	if g.heads, err = v.rd.ReadHeads(agent, v.room); err != nil {
		return nil, err
	}
	return g, nil
}

// confirmed reports whether the agent confirmed key.
func (g *agentState) confirmed(key string) (bool, error) {
	if _, ok := g.pending[key]; ok {
		return true, nil
	}
	if r, ok := g.keys[key]; ok {
		return r != nil, nil
	}
	r, ok, err := g.rd.Read(g.agent, g.room, key)
	if err != nil {
		return false, err
	}
	if ok {
		g.keys[key] = &r
	} else {
		g.keys[key] = nil
	}
	return ok, nil
}

func (g *agentState) readsOf(msg string) ([]store.Read, error) {
	rs, ok := g.byMsg[msg]
	if !ok {
		var err error
		if rs, err = g.rd.ReadsOf(g.agent, g.room, msg); err != nil {
			return nil, err
		}
		g.byMsg[msg] = rs
	}
	for _, p := range g.pending {
		if p.Msg == msg {
			rs = append(slices.Clone(rs), p)
		}
	}
	return rs, nil
}

func (g *agentState) confirm(r store.Read) { g.pending[r.Key] = r }

// seenVersion is the model's seenMsg[msg]: whether the agent confirmed
// msg's text (its own entry, or a replaced notice of it), and the newest
// held version it confirmed. Confirmations only move to newer versions,
// since Seen accepts only what Inbox shows now.
func (v *roomView) seenVersion(g *agentState, msg string) (bool, *rec, error) {
	rs, err := g.readsOf(msg)
	if err != nil {
		return false, nil, err
	}
	saw := false
	var best *rec
	for _, r := range rs {
		if r.Version == "" { // notices without text: expired, withdrawn
			continue
		}
		saw = true
		x, err := v.get(r.Version)
		if err != nil {
			return false, nil, err
		}
		if x != nil && (best == nil || newer(x, best)) {
			best = x
		}
	}
	return saw, best, nil
}

// newer says whether text version a supersedes b: the higher version wins,
// then the higher id. The message itself is version 0 [R7].
func newer(a, b *rec) bool {
	va, vb := int64(0), int64(0)
	if a.f.Kind == core.KindReplace {
		va = a.f.Version
	}
	if b.f.Kind == core.KindReplace {
		vb = b.f.Version
	}
	if va != vb {
		return va > vb
	}
	return a.f.Kind == core.KindReplace && (b.f.Kind != core.KindReplace || a.id > b.id)
}

// currentText is a message's text with the newest replacement held applied.
// wait names a replacement whose text is missing here.
func (v *roomView) currentText(h *rec) (text *rec, ver *rec, wait *rec, err error) {
	text, ver = h, h
	var best *rec
	rs, err := v.counted(h.id, core.KindReplace)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, r := range rs {
		if r.reason == store.ReasonMissing {
			wait = r
		}
		if r.reason == store.ReasonLive && (best == nil || newer(r, best)) {
			best = r
		}
	}
	if best != nil {
		text, ver = best, best
	}
	return text, ver, wait, nil
}

func (v *roomView) parentState(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	h, err := v.get(p)
	if err != nil {
		return "", err
	}
	switch {
	case h == nil || h.reason == store.ReasonMissing || h.reason == store.ReasonNotAdmitted:
		return ParentMissing, nil
	case h.reason == store.ReasonWithdrawn:
		return ParentWithdrawn, nil
	case h.reason == store.ReasonExpired:
		return ParentExpired, nil
	}
	return ParentHere, nil
}

func (v *roomView) entry(h *rec, kind, key string) (*Entry, error) {
	e := &Entry{Key: key, Kind: kind, ID: h.id, Record: h.id, Room: v.room,
		From: core.AgentID(h.f.Agent, h.author), FromNode: h.author, Created: h.f.Created,
		Epoch: h.f.Epoch, Seq: h.f.Seq, AuthorRemoved: v.removed(h.author)}
	msg := h
	if h.f.Kind != core.KindMessage {
		e.ID = h.f.Ref
		t, err := v.get(h.f.Ref)
		if err != nil {
			return nil, err
		}
		msg = t
	}
	if msg != nil {
		e.To = msg.f.To
		// A reply whose parent fails its check is shown without a parent
		// (PROTOCOL-next section 2).
		if msg.h.Effect != store.EffectBadReference {
			e.Parent, e.Thread = msg.f.Parent, msg.f.Thread
		}
	}
	var err error
	e.ParentState, err = v.parentState(e.Parent)
	return e, err
}

func relevant(r *rec, self, agent string) bool {
	if r.f.Kind != core.KindMessage || (r.author == self && r.f.Agent == agent) {
		return false
	}
	if len(r.f.To) == 0 {
		return true
	}
	for _, a := range r.f.To {
		if a.Node == self && a.Agent == agent {
			return true
		}
	}
	return false
}

func (v *roomView) handledByAgent(msg, agent string) (bool, error) {
	rs, err := v.counted(msg, core.KindHandled)
	if err != nil {
		return false, err
	}
	for _, r := range rs {
		if r.author == v.self && r.f.Agent == agent {
			return true, nil
		}
	}
	return false, nil
}

// decide says what one position means to one agent: nothing, an entry, or
// wait (its text, or a replacement's, is missing here). These are the
// reading rules of [R7]; a handled, withdraw or replace counts only if its
// reference checks out.
func (v *roomView) decide(g *agentState, agent string, h *rec, heads map[string][]string) (*Entry, *rec, error) {
	var e *Entry
	var err error
	switch h.f.Kind {
	case core.KindMessage:
		if !relevant(h, v.self, agent) {
			return nil, nil, nil
		}
		for _, k := range []string{h.id, "x:" + h.id} {
			if seen, err := g.confirmed(k); err != nil || seen {
				return nil, nil, err
			}
		}
		if done, err := v.handledByAgent(h.id, agent); err != nil || done {
			return nil, nil, err
		}
		switch h.reason {
		case store.ReasonWithdrawn, store.ReasonNotAdmitted:
			return nil, nil, nil // withdrawn before shown, or before this machine's time
		case store.ReasonMissing:
			return nil, h, nil
		case store.ReasonExpired:
			if e, err = v.entry(h, EntryExpired, "x:"+h.id); err != nil {
				return nil, nil, err
			}
		default:
			text, ver, wait, err := v.currentText(h)
			if err != nil || wait != nil {
				return nil, wait, err
			}
			if e, err = v.entry(h, EntryMessage, h.id); err != nil {
				return nil, nil, err
			}
			e.Text, e.Choices, e.Version = text.f.Text, text.f.Choices, ver.id
		}
	case core.KindWithdraw:
		if h.h.Effect != store.EffectOK {
			return nil, nil, nil
		}
		saw, _, err := v.seenVersion(g, h.f.Ref)
		if err != nil || !saw {
			return nil, nil, err
		}
		if e, err = v.entry(h, EntryWithdrawn, h.id); err != nil {
			return nil, nil, err
		}
	case core.KindReplace:
		if h.h.Effect != store.EffectOK {
			return nil, nil, nil
		}
		saw, vr, err := v.seenVersion(g, h.f.Ref)
		if err != nil {
			return nil, nil, err
		}
		t, err := v.get(h.f.Ref)
		if err != nil || !saw || t == nil || t.reason == store.ReasonWithdrawn {
			return nil, nil, err
		}
		if vr != nil && !newer(h, vr) {
			return nil, nil, nil
		}
		rs, err := v.counted(h.f.Ref, core.KindReplace)
		if err != nil {
			return nil, nil, err
		}
		for _, o := range rs { // a newer one will be shown instead
			if o.reason != store.ReasonWithdrawn && newer(o, h) {
				return nil, nil, nil
			}
		}
		switch h.reason {
		case store.ReasonMissing:
			return nil, h, nil
		case store.ReasonLive:
		default:
			return nil, nil, nil // expired: the newer text is gone too
		}
		if e, err = v.entry(h, EntryReplaced, h.id); err != nil {
			return nil, nil, err
		}
		e.Text, e.Choices, e.Version = h.f.Text, h.f.Choices, h.id
	default:
		return nil, nil, nil // handled reports are for trace, not the inbox
	}
	if seen, err := g.confirmed(e.Key); err != nil || seen {
		return nil, nil, err
	}
	e.Late = v.late(heads, h)
	return e, nil, nil
}

// late reports whether r fails to follow some record of its author the
// agent already confirmed or was shown in this listing.
func (v *roomView) late(heads map[string][]string, r *rec) bool {
	for _, id := range heads[r.author] {
		if h, _ := v.get(id); h != nil && !v.descends(r, h) {
			return true
		}
	}
	return false
}

// addHead records that r was shown: it replaces the heads it follows.
func (v *roomView) addHead(heads map[string][]string, r *rec) {
	var keep []string
	for _, id := range heads[r.author] {
		h, _ := v.get(id)
		if h != nil && !v.descends(r, h) && !v.descends(h, r) {
			keep = append(keep, id)
		} else if h != nil && v.descends(h, r) {
			return // already behind a shown record
		}
	}
	heads[r.author] = append(keep, r.id)
}

// walk goes through every stream from the agent's cursor, in order. A
// stream waits at a position whose text is missing here, and a stream's
// first record waits until the records it links to have been passed. In
// inbox mode it collects entries; in advance mode it stops each stream at
// the first entry not yet confirmed and returns how far it got.
func (v *roomView) walk(g *agentState, agent string, advance bool) ([]Entry, map[streamKey]int64, []Wait, error) {
	pos := copyCursor(g.cursor)
	heads := copyHeads(g.heads)
	var entries []Entry
	wait := map[streamKey]Wait{}
	for progress := true; progress; {
		progress = false
		for _, s := range v.streams {
			k := streamKey{s.Author, s.Epoch}
			if u, err := v.unseeded(g, k); err != nil {
				return nil, nil, nil, err
			} else if u {
				continue
			}
			for {
				n := pos[k] + 1
				h, err := v.at(k, n)
				if err != nil {
					return nil, nil, nil, err
				}
				if h == nil {
					break
				}
				linked := true
				for _, l := range h.f.Links {
					if pos[streamKey{h.author, l.Epoch}] < l.Seq {
						linked = false
					}
				}
				if !linked {
					wait[k] = Wait{Room: v.room, Author: k.author, Epoch: k.epoch, Have: s.Contig, Known: s.Contig, Reason: WaitLink}
					break
				}
				e, block, err := v.decide(g, agent, h, heads)
				if err != nil {
					return nil, nil, nil, err
				}
				if block != nil {
					wait[k] = Wait{Room: v.room, Author: k.author, Epoch: k.epoch, Have: s.Contig, Known: s.Contig,
						Reason: WaitText, At: block.id}
					break
				}
				delete(wait, k)
				if e != nil {
					if advance {
						break
					}
					entries = append(entries, *e)
					v.addHead(heads, h)
				}
				pos[k] = n
				progress = true
			}
		}
	}
	if !advance {
		vs, err := v.voided(g)
		if err != nil {
			return nil, nil, nil, err
		}
		entries = append(entries, vs...)
	}
	var ws []Wait
	for _, s := range v.streams {
		if s.Known > s.Contig {
			ws = append(ws, Wait{Room: v.room, Author: s.Author, Epoch: s.Epoch, Have: s.Contig, Known: s.Known,
				Since: s.KnownAt, Reason: WaitGap})
		}
		if w, ok := wait[streamKey{s.Author, s.Epoch}]; ok {
			ws = append(ws, w)
		}
	}
	return entries, pos, ws, nil
}

// voided returns a "voided" notice for each message whose text the agent
// confirmed and whose author is now hidden (no longer in Admitted [M13],
// [RM1]): the text is no longer shown, and the agent is told once. The
// stored record stays.
func (v *roomView) voided(g *agentState) ([]Entry, error) {
	if !v.hiddenAuthors {
		return nil, nil
	}
	rs, err := v.rd.Reads(g.agent, v.room)
	if err != nil {
		return nil, err
	}
	var out []Entry
	done := map[string]bool{}
	for _, r := range rs {
		if r.Version == "" || done[r.Msg] {
			continue
		}
		done[r.Msg] = true
		h, ok, err := v.rd.Record(v.room, r.Msg)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		x := v.keep(h)
		if !v.hidden(x.author) {
			continue
		}
		key := "v:" + x.id
		if seen, err := g.confirmed(key); err != nil || seen {
			if err != nil {
				return nil, err
			}
			continue
		}
		out = append(out, Entry{Key: key, Kind: EntryVoided, ID: x.id, Record: x.id, Room: v.room,
			From: core.AgentID(x.f.Agent, x.author), FromNode: x.author, AuthorRemoved: true,
			Created: x.f.Created, Epoch: x.f.Epoch, Seq: x.f.Seq})
	}
	return out, nil
}

// threadEntry is a message as the thread and branch views show it, or nil
// when there is nothing to show: its text is not here, or (as for Inbox) a
// newer replacement's text is on its way.
func (v *roomView) threadEntry(h *rec) (*Entry, error) {
	switch h.reason {
	case store.ReasonExpired:
		return v.entry(h, EntryExpired, "x:"+h.id)
	case store.ReasonLive:
		text, ver, wait, err := v.currentText(h)
		if err != nil || wait != nil {
			return nil, err
		}
		e, err := v.entry(h, EntryMessage, h.id)
		if err != nil {
			return nil, err
		}
		e.Text, e.Choices, e.Version = text.f.Text, text.f.Choices, ver.id
		return e, nil
	}
	return nil, nil
}
