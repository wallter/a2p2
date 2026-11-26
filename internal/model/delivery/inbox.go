package delivery

import "sort"

// EntryKind is what an inbox entry tells the agent.
type EntryKind uint8

const (
	Shown           EntryKind = iota + 1 // a message, with its current text
	WithdrawnNotice                      // a message you were shown was withdrawn
	ReplacedNotice                       // a message you were shown has new text
	ExpiredNotice                        // a message for you expired before you were shown it
	VoidedNotice                         // a message you were shown is gone: its author's admission never counted
)

func (k EntryKind) String() string {
	return [...]string{"?", "shown", "withdrawn", "replaced", "expired", "voided"}[k]
}

// ParentState says what became of a reply's parent, as this machine knows it.
type ParentState uint8

const (
	NoParent ParentState = iota
	ParentHere
	ParentMissing // not held or its text not here: not arrived yet, or before this machine joined
	ParentExpired
	ParentWithdrawn
)

// Entry is one item an agent is shown.
type Entry struct {
	Key      string // what Seen confirms; never shown twice once confirmed
	Kind     EntryKind
	Record   RecordID // the record at this position
	Message  RecordID // the message concerned (same as Record when Shown)
	Pos      Pos
	From     Addr
	Text     string
	Version  RecordID // the record whose text is shown (the message, or a replace)
	Parent   RecordID
	Thread   RecordID
	ParentIs ParentState
	Created  Time
	// Late: the agent already confirmed a record of this author that this
	// one does not follow in the author's history. Only a restore of the
	// author from backup, followed by a write before it caught up, causes it.
	Late bool
	// AuthorRemoved: the author was a member and, in this machine's view,
	// is no longer.
	AuthorRemoved bool
	// Files are the files of the version shown, from its manifest
	// (attach.go); FilesPending: it has files whose list has not arrived.
	// What became of each file is FileStatus.
	Files        []FileRef
	FilesPending bool
}

// Gap is a stream this machine knows is longer than what it holds.
type Gap struct {
	Stream StreamID
	Have   int
	Known  int
	Since  Time // when this machine learned of position Known
	Link   bool // waiting for the record the stream's first record links to
	Text   bool // waiting for text missing here
	At     Pos  // for Text: the record whose text is missing
}

// View is what Inbox returns.
type View struct {
	Entries []Entry
	Waiting []Gap
}

func relevant(r *Record, m MachineID, agent string) bool {
	if r.Kind != Message || (r.Author == m && r.Agent == agent) {
		return false
	}
	if len(r.To) == 0 {
		return true
	}
	for _, a := range r.To {
		if a == (Addr{m, agent}) {
			return true
		}
	}
	return false
}

// newer says whether text version a supersedes b: the higher version wins,
// then the higher id. The message itself is version 0. No clocks, and the
// same answer in whatever order the records arrived.
func (m *Machine) newer(a, b *Record) bool {
	va, vb := 0, 0
	if a.Kind == Replace {
		va = a.Version
	}
	if b.Kind == Replace {
		vb = b.Version
	}
	if va != vb {
		return va > vb
	}
	return a.Kind == Replace && (b.Kind != Replace || a.ID > b.ID)
}

// currentText is a message's text with the newest replacement applied. wait
// names a replacement whose text is missing here, so the text shown could
// be stale. A replacement whose text expired still counts: when it is the
// newest, ver is it (and text is empty), and views show the message as
// expired, never an earlier text as current (newestExpired).
func (m *Machine) currentText(h *held) (text string, ver RecordID, wait *Pos) {
	text, ver = h.body, h.rec.ID
	var best *Record
	for _, id := range m.st.replaced[h.rec.ID] {
		r := m.get(id)
		if r == nil || r.rec.Author != h.rec.Author {
			continue
		}
		if r.dropped == Missing {
			p := r.rec.Pos()
			wait = &p
		}
		if (r.dropped == Live || r.dropped == Expired) && (best == nil || m.newer(r.rec, best)) {
			best, text = r.rec, r.body
		}
	}
	if best != nil {
		ver = best.ID
		if m.get(ver).dropped != Live {
			text = ""
		}
	}
	return text, ver, wait
}

// newestExpired says whether ver, a version currentText named, has no
// text left here: the message's newest replacement expired.
func (m *Machine) newestExpired(ver RecordID) bool {
	x := m.get(ver)
	return x != nil && x.dropped != Live
}

func (m *Machine) parentState(p RecordID) ParentState {
	if p == "" {
		return NoParent
	}
	h := m.get(p)
	switch {
	case h == nil || h.dropped == Missing || h.dropped == NotAdmitted:
		return ParentMissing
	case h.dropped == Withdrawn:
		return ParentWithdrawn
	case h.dropped == Expired:
		return ParentExpired
	}
	return ParentHere
}

func (m *Machine) entry(h *held, k EntryKind, key string) *Entry {
	r := h.rec
	e := &Entry{Key: key, Kind: k, Record: r.ID, Message: r.ID, Pos: r.Pos(),
		From: Addr{r.Author, r.Agent}, Created: r.Created}
	if r.Kind != Message {
		e.Message = r.Ref
		if t := m.get(r.Ref); t != nil {
			e.Parent, e.Thread = t.rec.Parent, t.rec.Thread
		}
	} else {
		e.Parent, e.Thread = r.Parent, r.Thread
	}
	e.ParentIs = m.parentState(e.Parent)
	e.AuthorRemoved = m.st.view.ever[r.Author] && !m.st.view.now[r.Author]
	return e
}

func (m *Machine) handledByAgent(msg RecordID, agent string) bool {
	for _, hid := range m.st.handled[msg] {
		if hr := m.get(hid).rec; hr.Author == m.ID && hr.Agent == agent {
			return true
		}
	}
	return false
}

// decide says what one position means to one agent: nothing, an entry, or
// wait (its text is missing here and may still come). These are the
// reading rules.
func (m *Machine) decide(g *agentState, agent string, h *held, heads map[MachineID][]RecordID) (*Entry, *Pos) {
	r := h.rec
	var e *Entry
	switch r.Kind {
	case Message:
		if !relevant(r, m.ID, agent) || g.keptOut(h) || g.seen[string(r.ID)] || g.seen["x:"+string(r.ID)] {
			return nil, nil
		}
		if m.handledByAgent(r.ID, agent) {
			// Already reported handled (seen again after a restore, with its
			// own report fetched back): do not ask again.
			return nil, nil
		}
		switch h.dropped {
		case Withdrawn, NotAdmitted:
			return nil, nil // withdrawn before shown, or before this machine's time
		case Missing:
			p := r.Pos()
			return nil, &p
		case Expired:
			e = m.entry(h, ExpiredNotice, "x:"+string(r.ID))
		default:
			text, ver, wait := m.currentText(h)
			if wait != nil {
				return nil, wait
			}
			if m.newestExpired(ver) {
				e = m.entry(h, ExpiredNotice, "x:"+string(r.ID))
				break
			}
			e = m.entry(h, Shown, string(r.ID))
			e.Text, e.Version = text, ver
			e.Files, e.FilesPending = m.filesOf(ver)
		}
	case Withdraw:
		if _, saw := g.seenMsg[r.Ref]; !saw {
			return nil, nil
		}
		if t := m.get(r.Ref); t == nil || t.rec.Author != r.Author {
			return nil, nil
		}
		e = m.entry(h, WithdrawnNotice, string(r.ID))
	case Replace:
		v, saw := g.seenMsg[r.Ref]
		t := m.get(r.Ref)
		if !saw || t == nil || t.rec.Author != r.Author || t.dropped == Withdrawn {
			return nil, nil
		}
		if vr := m.get(v); vr != nil && !m.newer(r, vr.rec) {
			return nil, nil
		}
		for _, id := range m.st.replaced[r.Ref] { // a newer one will be shown instead
			if o := m.get(id); o != nil && o.rec.Author == r.Author && o.dropped != Withdrawn && m.newer(o.rec, r) {
				return nil, nil
			}
		}
		switch h.dropped {
		case Missing:
			p := r.Pos()
			return nil, &p
		case Live:
		default:
			return nil, nil // expired: the newer text is gone too
		}
		e = m.entry(h, ReplacedNotice, string(r.ID))
		e.Text, e.Version = h.body, r.ID
		e.Files, e.FilesPending = m.filesOf(r.ID)
	default:
		return nil, nil // handled reports are for Status, not the inbox
	}
	if g.seen[e.Key] {
		return nil, nil
	}
	e.Late = m.late(heads, r)
	return e, nil
}

// streams lists held streams, each author's in the order they began, so that
// streams the history does not order (after a restore) come out oldest first.
func (m *Machine) streams() []StreamID {
	var out []StreamID
	for s := range m.st.contig {
		out = append(out, s)
	}
	began := func(s StreamID) Time { return m.at(Pos{s, 1}).rec.Created }
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Author != b.Author || began(a) == began(b) {
			return a.less(b)
		}
		return began(a) < began(b)
	})
	return out
}

// late reports whether r fails to follow some record of its author already
// shown.
func (m *Machine) late(heads map[MachineID][]RecordID, r *Record) bool {
	for _, id := range heads[r.Author] {
		if h := m.get(id); h != nil && !m.descends(r, h.rec) {
			return true
		}
	}
	return false
}

// addHead records that r was shown: it replaces the heads it follows.
func (m *Machine) addHead(heads map[MachineID][]RecordID, r *Record) {
	var keep []RecordID
	for _, id := range heads[r.Author] {
		if h := m.get(id); h != nil && !m.descends(r, h.rec) && !m.descends(h.rec, r) {
			keep = append(keep, id)
		} else if h != nil && m.descends(h.rec, r) {
			return // already behind a shown record
		}
	}
	heads[r.Author] = append(keep, r.ID)
}

// walk goes through every stream from the agent's cursor, in order. A
// stream waits at a position whose text is missing here, and a stream's
// first record waits until the record it links to has been passed. In
// inbox mode it collects entries. In advance mode it stops at the first
// entry not yet confirmed and returns how far it got.
func (m *Machine) walk(g *agentState, agent string, advance bool) ([]Entry, map[StreamID]int, []Gap) {
	pos := copyInts(g.cursor)
	heads := copyHeads(g.heads)
	var entries []Entry
	wait := map[StreamID]Gap{}
	streams := m.streams()
	for progress := true; progress; {
		progress = false
		for _, s := range streams {
			for {
				n := pos[s] + 1
				h := m.at(Pos{s, n})
				if h == nil {
					break
				}
				r := h.rec
				linked := true
				for _, l := range r.Links {
					if pos[l.At.Stream] < l.At.Seq {
						linked = false
					}
				}
				if !linked {
					wait[s] = Gap{Stream: s, Have: m.contigOf(s), Known: m.contigOf(s), Link: true}
					break
				}
				e, block := m.decide(g, agent, h, heads)
				if block != nil {
					wait[s] = Gap{Stream: s, Have: m.contigOf(s), Known: m.contigOf(s), Text: true, At: *block}
					break
				}
				delete(wait, s)
				if e != nil {
					if advance {
						break
					}
					entries = append(entries, *e)
					m.addHead(heads, r)
				}
				pos[s] = n
				progress = true
			}
		}
	}
	var gaps []Gap
	for s, k := range m.st.known {
		if c := m.contigOf(s); k.Seq > c {
			gaps = append(gaps, Gap{Stream: s, Have: c, Known: k.Seq, Since: k.At})
		}
	}
	for _, g := range wait {
		gaps = append(gaps, g)
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].Stream.less(gaps[j].Stream) })
	return entries, pos, gaps
}

// Inbox shows an agent what it has not confirmed, in each author's order,
// and the gaps this machine knows of. It changes nothing.
func (w *World) Inbox(id MachineID, agent string) (View, error) {
	m, err := w.awake(id)
	if err != nil {
		return View{}, err
	}
	g := m.st.agents[agent]
	if g == nil {
		return View{}, ErrNoMachine
	}
	var es []Entry
	for _, id := range g.told {
		if k := "v:" + string(id); !g.seen[k] {
			es = append(es, Entry{Key: k, Kind: VoidedNotice, Record: id, Message: id})
		}
	}
	e, _, gaps := m.walk(g, agent, false)
	return View{Entries: append(es, e...), Waiting: gaps}, nil
}

// List is Inbox as an agent reads it: it shows the agent its inbox and
// keeps a receipt of each entry listed (key and version), which Seen
// requires, and notes the messages whose text it showed, which the agent
// may then report handled. It moves no cursor and marks nothing seen.
func (w *World) List(id MachineID, agent string) (View, error) {
	v, err := w.Inbox(id, agent)
	if err != nil {
		return v, err
	}
	return v, w.Listed(id, agent, v.Entries)
}

// Listed is List's second half: it keeps a receipt of each of es, which an
// inbox listing showed agent, and notes the messages whose text they show.
func (w *World) Listed(id MachineID, agent string, es []Entry) error {
	m, err := w.awake(id)
	if err != nil {
		return err
	}
	g := m.st.agents[agent]
	if g == nil {
		return ErrNoMachine
	}
	for _, e := range es {
		g.receipts[e.Key] = e.Version
	}
	g.noteShown(es)
	return nil
}

// noteShown notes the messages whose text es showed.
func (g *agentState) noteShown(es []Entry) {
	for _, e := range es {
		if e.Kind == Shown || e.Kind == ReplacedNotice {
			g.shown[e.Message] = true
			g.shownVer[e.Version] = true
		}
	}
}

// Seen confirms what Confirm accepts.
func (w *World) Seen(id MachineID, agent string, entries []Entry) error {
	_, err := w.Confirm(id, agent, entries)
	return err
}

// Confirm confirms entries the agent was shown, and says which it
// accepted: an entry counts only if a full inbox listing (List) showed it
// with that key and version, and the inbox shows it so now, with a listed
// message never kept out by a level changed since. It is the only thing
// that marks anything seen.
func (w *World) Confirm(id MachineID, agent string, entries []Entry) ([]bool, error) {
	m, err := w.awake(id)
	if err != nil {
		return nil, err
	}
	g := m.st.agents[agent]
	if g == nil {
		return nil, ErrNoMachine
	}
	v, err := w.Inbox(id, agent)
	if err != nil {
		return nil, err
	}
	now := v.Entries
	ok := make([]bool, len(entries))
	var accept []Entry
	for i, e := range entries {
		if v, listed := g.receipts[e.Key]; !listed || v != e.Version {
			continue
		}
		for _, x := range now {
			if x.Key == e.Key && x.Version == e.Version {
				ok[i] = true
				accept = append(accept, x)
				break
			}
		}
	}
	for _, e := range accept {
		g.seen[e.Key] = true
		switch e.Kind {
		case Shown:
			g.seenMsg[e.Record] = e.Version
		case ReplacedNotice:
			g.seenMsg[e.Message] = e.Record
		}
		if r := m.get(e.Record); r != nil {
			m.addHead(g.heads, r.rec)
		}
	}
	_, pos, _ := m.walk(g, agent, true)
	g.cursor = pos
	return ok, nil
}

// Showed records that a read other than Inbox (a thread, branch, recent or
// skipped view) returned entries es to agent on machine id: an agent shown
// a message's text that way may report it handled, as if its inbox had
// shown it. It moves no cursor and marks nothing seen, so the inbox shows
// exactly what it showed before.
func (w *World) Showed(id MachineID, agent string, es []Entry) error {
	m, err := w.awake(id)
	if err != nil {
		return err
	}
	g := m.st.agents[agent]
	if g == nil {
		return ErrNoMachine
	}
	g.noteShown(es)
	return nil
}

// Report is a dated report that a machine held a record's text.
type Report struct {
	Machine MachineID
	At      Time
	Member  bool // the machine is still a member
}

// Handler is an agent that reported handling a message.
type Handler struct {
	Addr Addr
	At   Time // by the handler's clock
}

// Status is what a machine can tell its user about one record (D5, A4).
type Status struct {
	Saved   bool
	Dropped Reason
	// Copies: other machines that, the last time they were in contact with
	// this one, said they held the text, and when. A report can be stale.
	Copies  []Report
	Handled []Handler
}

// Status reports what machine id knows about record rid.
func (w *World) Status(id MachineID, rid RecordID) Status {
	m := w.machines[id]
	m.prune(w.Now)
	h := m.get(rid)
	if h == nil {
		return Status{}
	}
	st := Status{Saved: true, Dropped: h.dropped}
	p := h.rec.Pos()
	var peers []MachineID
	for o := range m.st.obs {
		peers = append(peers, o)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i] < peers[j] })
	for _, o := range peers {
		if ob := m.st.obs[o]; o != m.ID && ob.Have.HoldsBody(p) {
			st.Copies = append(st.Copies, Report{Machine: o, At: ob.At, Member: w.members[o]})
		}
	}
	seen := map[Addr]bool{}
	for _, hid := range m.st.handled[rid] {
		hr := m.get(hid).rec
		a := Addr{hr.Author, hr.Agent}
		if !canHandle(h.rec, hr.Author, hr.Agent) || seen[a] {
			continue
		}
		seen[a] = true
		st.Handled = append(st.Handled, Handler{Addr: a, At: hr.Created})
	}
	sort.Slice(st.Handled, func(i, j int) bool { return st.Handled[i].Addr.String() < st.Handled[j].Addr.String() })
	return st
}

// threadEntry is a message as a thread view shows it, or nil when there is
// nothing this machine may show.
func (m *Machine) threadEntry(h *held) *Entry {
	switch h.dropped {
	case Expired:
		return m.entry(h, ExpiredNotice, "x:"+string(h.rec.ID))
	case Live:
		if _, ver, _ := m.currentText(h); m.newestExpired(ver) {
			return m.entry(h, ExpiredNotice, "x:"+string(h.rec.ID))
		}
		e := m.entry(h, Shown, string(h.rec.ID))
		e.Text, e.Version, _ = m.currentText(h)
		e.Files, e.FilesPending = m.filesOf(e.Version)
		return e
	}
	return nil
}

// sortEntries orders a view of messages for a reader: by created time;
// messages of one time by author, so that an author's own are in the
// order written (their position) and the order does not depend on the
// machine or the read; then by id, which only orders one author's
// messages at one time and one position in two streams (after a restore).
// Every field is signed, so every machine orders alike.
func sortEntries(es []Entry) {
	sort.Slice(es, func(i, j int) bool {
		a, b := es[i], es[j]
		switch {
		case a.Created != b.Created:
			return a.Created < b.Created
		case a.Pos.Stream.Author != b.Pos.Stream.Author:
			return a.Pos.Stream.Author < b.Pos.Stream.Author
		case a.Pos.Seq != b.Pos.Seq:
			return a.Pos.Seq < b.Pos.Seq
		}
		return a.Record < b.Record
	})
}

// Thread returns every message held in the thread started by root, root
// included, oldest first. Withdrawn messages, and messages whose text is not
// here, are left out; their replies show the parent as such. It marks
// nothing seen.
func (w *World) Thread(id MachineID, root RecordID) []Entry {
	m := w.machines[id]
	m.prune(w.Now)
	var out []Entry
	for _, h := range m.st.recs {
		r := h.rec
		if r.Kind != Message || (r.ID != root && r.Thread != root) {
			continue
		}
		if e := m.threadEntry(h); e != nil {
			out = append(out, *e)
		}
	}
	sortEntries(out)
	return out
}

// Branch returns the message under and the replies below it, to any depth,
// as far as this machine can link them: a reply whose parent is not held
// cannot be placed under it, and appears only in Thread.
func (w *World) Branch(id MachineID, under RecordID) []Entry {
	m := w.machines[id]
	m.prune(w.Now)
	var out []Entry
	queue := []RecordID{under}
	done := map[RecordID]bool{}
	for len(queue) > 0 {
		x := queue[0]
		queue = queue[1:]
		if done[x] {
			continue
		}
		done[x] = true
		if h := m.get(x); h != nil {
			if e := m.threadEntry(h); e != nil {
				out = append(out, *e)
			}
		}
		queue = append(queue, m.st.children[x]...)
	}
	sortEntries(out)
	return out
}
