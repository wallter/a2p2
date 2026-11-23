package delivery

import (
	"errors"
	"fmt"
	"sort"
)

// Admission is how a machine joined the room: the admitting member's
// positions at that moment. Every agent on the new machine starts reading
// after Heads (its inbox starts empty). Without History, members hand it
// records at or before Heads only as headers, so it can carry the chain for
// others but not read the text.
type Admission struct {
	Heads   map[StreamID]int
	History bool
}

// World is the room: its machines, the true time, and the key table every
// member uses to check signatures. Membership is fixed apart from Admit and
// the re-admission after a lost disk; membership changes proper are modelled
// in internal/model/members.
type World struct {
	Now       Time
	machines  map[MachineID]*Machine
	order     []MachineID
	members   map[MachineID]bool
	keys      map[MachineID]string
	admission map[MachineID]Admission
	nextEpoch uint64
	nextKey   int
	// Forks counts records refused because their position was already filled
	// by a different record. The stream-per-start rule keeps it at zero.
	Forks int
	// Tick is an operation counter kept by the caller; copy reports carry it
	// so a test can check each one against the exact state it described.
	Tick int

	tamper func(server MachineID, it wire) wire // test hook: a relay that alters what it passes on

	// continueStreams switches on the rejected rule, for the test that shows
	// why it is rejected: after a start, keep writing the stream of the last
	// own record on disk instead of beginning a new one.
	continueStreams bool
	// persistHeads switches on the reviewed alternative: continue the stream
	// when a head file kept outside the database agrees with the database.
	persistHeads bool
}

// Errors returned by operations.
var (
	ErrAsleep    = errors.New("machine is asleep")
	ErrNoMachine = errors.New("no such machine")
	ErrNotHeld   = errors.New("record not held here")
	ErrNotAllow  = errors.New("not allowed")
)

// NewWorld returns an empty room at time zero.
func NewWorld() *World {
	return &World{
		machines:  map[MachineID]*Machine{},
		members:   map[MachineID]bool{},
		keys:      map[MachineID]string{},
		admission: map[MachineID]Admission{},
	}
}

// Clone returns an independent copy, for exhaustive search.
func (w *World) Clone() *World {
	c := *w
	c.machines = make(map[MachineID]*Machine, len(w.machines))
	for k, v := range w.machines {
		c.machines[k] = v.clone()
	}
	c.order = append([]MachineID(nil), w.order...)
	c.members = map[MachineID]bool{}
	for k, v := range w.members {
		c.members[k] = v
	}
	c.keys = map[MachineID]string{}
	for k, v := range w.keys {
		c.keys[k] = v
	}
	c.admission = map[MachineID]Admission{}
	for k, v := range w.admission {
		c.admission[k] = v
	}
	return &c
}

func (w *World) newMachine(id MachineID, agents []string, adm Admission) *Machine {
	w.nextKey++
	m := &Machine{ID: id, key: fmt.Sprintf("key-%s-%d", id, w.nextKey), Awake: true,
		agents: append([]string(nil), agents...), st: newStore(), withhold: map[RecordID]bool{}}
	m.st.adm = adm
	for _, a := range agents {
		m.st.agents[a] = newAgent(adm.Heads)
	}
	w.machines[id] = m
	w.order = append(w.order, id)
	w.members[id] = true
	w.keys[id] = m.key
	w.admission[id] = adm
	return m
}

// AddMachine adds a founding member, awake, with the given agents.
func (w *World) AddMachine(id MachineID, agents ...string) *Machine {
	return w.newMachine(id, agents, Admission{History: true})
}

// Admit adds a new machine, admitted by member by. Its inbox starts empty
// (U3). history says whether members may hand it records from before.
func (w *World) Admit(id MachineID, by MachineID, history bool, agents ...string) (*Machine, error) {
	b, ok := w.machines[by]
	if !ok || !w.members[by] {
		return nil, ErrNoMachine
	}
	adm := Admission{Heads: copyInts(b.st.contig), History: history}
	return w.newMachine(id, agents, adm), nil
}

// Machine returns a machine by id.
func (w *World) Machine(id MachineID) *Machine { return w.machines[id] }

// Members returns the current members in the order they were added.
func (w *World) Members() []MachineID {
	var out []MachineID
	for _, id := range w.order {
		if w.members[id] {
			out = append(out, id)
		}
	}
	return out
}

func (w *World) awake(id MachineID) (*Machine, error) {
	m, ok := w.machines[id]
	if !ok || m.Gone {
		return nil, ErrNoMachine
	}
	if !m.Awake {
		return nil, ErrAsleep
	}
	m.prune(w.Now)
	return m, nil
}

// Advance moves the true time forward. Awake machines drop expired text as
// the time passes; sleeping ones do so when they wake.
func (w *World) Advance(d Time) {
	w.Now += d
	for _, m := range w.machines {
		if m.Awake && !m.Gone {
			m.prune(w.Now)
		}
	}
}

// Sleep stops a machine; nothing is lost.
func (w *World) Sleep(id MachineID) {
	if m := w.machines[id]; m != nil {
		m.Awake = false
	}
}

// Wake starts a sleeping machine again. Waking is not a restart: the daemon
// kept running, so it keeps writing to the same stream.
func (w *World) Wake(id MachineID) {
	if m := w.machines[id]; m != nil && !m.Gone {
		m.Awake = true
		m.prune(w.Now)
	}
}

// Crash kills and restarts the daemon. Everything on disk survives; the next
// write starts a new stream.
func (w *World) Crash(id MachineID) {
	if m := w.machines[id]; m != nil && !m.Gone {
		m.epoch, m.seq, m.last = 0, 0, nil
		m.Awake = true
		m.prune(w.Now)
	}
}

// Snapshot is a backup of a machine's database, key included, and of the
// head file beside it (used only by the rejected alternative).
type Snapshot struct {
	st       *store
	headFile *Pos
	at       Time
}

// Backup copies a machine's disk.
func (w *World) Backup(id MachineID) *Snapshot {
	m := w.machines[id]
	return &Snapshot{st: m.st.clone(), headFile: m.headFile, at: w.Now}
}

// restoreWhole restores the head file along with the database, as a
// whole-disk restore would.
func (w *World) restoreWhole(id MachineID, s *Snapshot) {
	w.Restore(id, s)
	w.machines[id].headFile = s.headFile
}

// SetSkew sets a machine's clock, forwards or back.
func (w *World) SetSkew(id MachineID, skew Time) {
	m := w.machines[id]
	m.Skew = skew
	if m.Awake && !m.Gone {
		m.prune(w.Now)
	}
}

// Restore puts an older backup back on a machine and starts its daemon. The
// machine keeps its key; other machines keep what they had. Nothing tells
// the machine that it was restored.
func (w *World) Restore(id MachineID, s *Snapshot) {
	m := w.machines[id]
	m.st = s.st.clone()
	m.epoch, m.seq, m.last = 0, 0, nil
	m.Awake = true
	m.prune(w.Now)
}

// DiskLost destroys a machine's disk and key. The person re-adds the machine
// (by the first other member, with history, so it can hold copies again), so it comes back as a new
// machine with the same agent labels. The old id stays in the key table so
// its records still verify. It returns the new id.
func (w *World) DiskLost(id MachineID) MachineID {
	m := w.machines[id]
	m.Gone, m.Awake = true, false
	delete(w.members, id)
	nid := id + "'"
	for w.machines[nid] != nil {
		nid += "'"
	}
	var by MachineID
	for _, o := range w.Members() {
		by = o
		break
	}
	if by == "" {
		w.newMachine(nid, m.agents, Admission{History: true})
	} else if _, err := w.Admit(nid, by, true, m.agents...); err != nil {
		panic(err)
	}
	w.machines[nid].Skew = m.Skew
	return nid
}

// Withhold makes a machine leave a record out whenever it serves. It still
// reports holding it. This stands for a buggy or dishonest member.
func (w *World) Withhold(server MachineID, id RecordID) { w.machines[server].withhold[id] = true }

// write signs and stores a new record from m. The first write after a start
// begins a new stream linked to every head of the machine's own history.
func (w *World) write(m *Machine, r *Record, body string) *Record {
	var links []Link
	if m.epoch == 0 {
		// A real machine picks a random epoch, so the model scrambles its
		// counter: nothing may depend on epochs sorting in creation order.
		w.nextEpoch++
		m.epoch, m.seq, m.last = (w.nextEpoch*2654435761)%4294967291, 0, nil
		links = m.ownHeads()
		cont := w.continueStreams && len(links) > 0
		if w.persistHeads && len(links) == 1 && m.headFile != nil {
			cont = *m.headFile == links[0].At // no head file: new stream
		}
		if cont {
			m.last = m.get(links[0].ID).rec
			m.epoch, m.seq = m.last.Epoch, m.last.Seq
		}
	}
	m.seq++
	r.Author, r.Epoch, r.Seq = m.ID, m.epoch, m.seq
	if r.Seq > 1 {
		r.Prev = m.last.ID
	} else {
		r.Links = links
	}
	r.Created = m.clock(w.Now)
	r.BodyHash = hashBody(body)
	r.ID = r.computeID()
	r.Sig = sign(m.key, r.ID)
	m.insert(r, body, true, w.Now)
	m.last = r
	p := r.Pos()
	m.headFile = &p
	return r
}

// Msg is what an agent sends.
type Msg struct {
	Body string
	// To names the addressees. Empty means everyone, except for a reply, which
	// goes to the author of its parent unless Everyone is set.
	To         []Addr
	Everyone   bool
	Parent     RecordID
	AskHandled bool
}

// Send writes a message. Once it returns, the message is on disk (D1).
func (w *World) Send(id MachineID, agent string, msg Msg) (RecordID, error) {
	m, err := w.awake(id)
	if err != nil {
		return "", err
	}
	r := &Record{Kind: Message, Agent: agent, AskHandled: msg.AskHandled}
	if !msg.Everyone {
		r.To = append([]Addr(nil), msg.To...)
	}
	if msg.Parent != "" {
		p := m.get(msg.Parent)
		if p == nil || p.rec.Kind != Message {
			return "", ErrNotHeld
		}
		r.Parent, r.Thread = p.rec.ID, p.rec.Thread
		if r.Thread == "" {
			r.Thread = p.rec.ID
		}
		if len(msg.To) == 0 && !msg.Everyone {
			r.To = []Addr{{p.rec.Author, p.rec.Agent}}
		}
	}
	sort.Slice(r.To, func(i, j int) bool { return r.To[i].String() < r.To[j].String() })
	return w.write(m, r, msg.Body).ID, nil
}

// canHandle says whether agent on machine m may report handling msg: it is
// an addressee, or msg is a room-wide post that asked for reports.
func canHandle(msg *Record, m MachineID, agent string) bool {
	if msg.Kind != Message {
		return false
	}
	if len(msg.To) == 0 {
		return msg.AskHandled && !(msg.Author == m && msg.Agent == agent)
	}
	for _, a := range msg.To {
		if a == (Addr{m, agent}) {
			return true
		}
	}
	return false
}

// Handle records that agent dealt with message ref. It travels like any
// record. Reporting twice is a no-op.
func (w *World) Handle(id MachineID, agent string, ref RecordID) (RecordID, error) {
	m, err := w.awake(id)
	if err != nil {
		return "", err
	}
	t := m.get(ref)
	if t == nil {
		return "", ErrNotHeld
	}
	if !canHandle(t.rec, m.ID, agent) {
		return "", ErrNotAllow
	}
	for _, h := range m.st.handled[ref] {
		if r := m.get(h).rec; r.Author == m.ID && r.Agent == agent {
			return r.ID, nil
		}
	}
	if g := m.st.agents[agent]; g == nil || !g.seen[string(ref)] {
		return "", ErrNotAllow // only an agent that was shown the text may report it
	}
	return w.write(m, &Record{Kind: Handled, Agent: agent, Ref: ref}, "").ID, nil
}

// Withdraw takes back one of this machine's own messages.
func (w *World) Withdraw(id MachineID, agent string, ref RecordID) (RecordID, error) {
	return w.amend(id, agent, ref, Withdraw, "")
}

// Replace gives new text for one of this machine's own messages.
func (w *World) Replace(id MachineID, agent string, ref RecordID, text string) (RecordID, error) {
	return w.amend(id, agent, ref, Replace, text)
}

func (w *World) amend(id MachineID, agent string, ref RecordID, k Kind, body string) (RecordID, error) {
	m, err := w.awake(id)
	if err != nil {
		return "", err
	}
	t := m.get(ref)
	if t == nil {
		return "", ErrNotHeld
	}
	if t.rec.Author != m.ID || t.rec.Kind != Message {
		return "", ErrNotAllow
	}
	r := &Record{Kind: k, Agent: agent, Ref: ref}
	if k == Replace {
		if _, ver, _ := m.currentText(t); ver != ref {
			r.Version = m.get(ver).rec.Version
		}
		r.Version++
	}
	return w.write(m, r, body).ID, nil
}

// PullOpt injects faults into one pull.
type PullOpt struct {
	Limit     int  // at most this many records in the response (0: no limit)
	LostReply bool // the server answered; the answer never arrived
}

// PullResult says what one pull did.
type PullResult struct {
	Sent, Stored, Refused int
}

// Pull is the one operation that moves data. Puller opens a connection to
// server and sends its summary; server answers with every record puller
// lacks, in order, and its own summary. Each side records what the other
// said it held. Puller stores the answer in one step. Both must be awake.
func (w *World) Pull(puller, server MachineID, opt PullOpt) (PullResult, error) {
	if puller == server {
		return PullResult{}, ErrNotAllow
	}
	p, err := w.awake(puller)
	if err != nil {
		return PullResult{}, err
	}
	s, err := w.awake(server)
	if err != nil {
		return PullResult{}, err
	}
	if !w.members[puller] || !w.members[server] {
		return PullResult{}, ErrNotAllow
	}
	req := p.summary()
	s.observe(p.ID, req, w.Now, w.Tick)
	items := w.page(s, p.ID, req)
	if opt.Limit > 0 && len(items) > opt.Limit {
		items = items[:opt.Limit]
	}
	res := PullResult{Sent: len(items)}
	if opt.LostReply {
		return res, nil
	}
	resp := s.summary()
	for _, it := range items {
		ok, fork := p.accept(w, it)
		if fork {
			w.Forks++
		}
		if ok {
			res.Stored++
		} else {
			res.Refused++
		}
	}
	p.observe(s.ID, resp, w.Now, w.Tick)
	return res, nil
}
