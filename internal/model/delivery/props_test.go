package delivery

import (
	"fmt"
	"math/rand"
	"os"
	"sync/atomic"
	"testing"
)

// The property checks. A harness runs a schedule of operations on up to
// four machines (A, B, C, and D once someone admits it) and, after every
// step, checks the properties against its own record of what happened,
// kept apart from the model's state.

type opKind uint8

const (
	opSendTo opKind = iota // a addresses the agent on b
	opSendAll
	opPull // a pulls from b
	opPullLimit1
	opPullLost
	opToggle // sleep a if awake, else wake it
	opCrash
	opRead
	opPartialRead // a's agents confirm only the first half of what they are shown
	opPeekCrash   // a's agents are shown their inbox, then a crashes before they confirm
	opHandle
	opAdvance
	opSkew // a's clock moves to the next value in skewSteps, forwards or back
	opBackup
	opRestore
	opWithdraw
	opReplace
	opDiskLost
	opWithhold // a starts leaving its newest held record by another author out
	opAdmit    // a admits D (or grants D again); without history if b is 0
	opOffer    // a pulls from b, offering what it thinks b lacks
	opHold     // a's request to b is held by b
	opRelease  // the held response is sent
	opRemove   // b removes a; only b knows at first
	opVoid     // b finds a's admission never counted; only b knows at first
	opLearn    // a learns the oldest membership change it has not
)

var opNames = [...]string{"sendTo", "sendAll", "pull", "pull1", "pullLost", "toggle", "crash",
	"read", "partialRead", "peekCrash", "handle", "advance", "skew", "backup", "restore",
	"withdraw", "replace", "diskLost", "withhold", "admit", "offer", "hold", "release",
	"remove", "void", "learn"}

// pair says whether an operation names two machines.
func pair(k opKind) bool {
	switch k {
	case opSendTo, opPull, opPullLimit1, opPullLost, opOffer, opHold, opRemove, opVoid:
		return true
	}
	return false
}

var skewSteps = []Time{Day, -Day, 3 * Hour, 0}

type op struct {
	k    opKind
	a, b int
}

func (o op) String() string {
	n := "ABCD"
	switch o.k {
	case opAdmit:
		return fmt.Sprintf("admit(%c,history=%v)", n[o.a], o.b != 0)
	case opAdvance, opRelease:
		return opNames[o.k]
	}
	if pair(o.k) {
		return fmt.Sprintf("%s(%c,%c)", opNames[o.k], n[o.a], n[o.b])
	}
	return fmt.Sprintf("%s(%c)", opNames[o.k], n[o.a])
}

type agentLog struct {
	keys      map[string]bool
	lastSeq   map[StreamID]int
	lastWrite map[MachineID]int
	confirmed map[MachineID][]RecordID // per author, records confirmed, for the late check
	shown     []RecordID               // messages delivered, for opHandle
}

func newLog() *agentLog {
	return &agentLog{keys: map[string]bool{}, lastSeq: map[StreamID]int{}, lastWrite: map[MachineID]int{},
		confirmed: map[MachineID][]RecordID{}}
}

func (l *agentLog) clone() *agentLog {
	c := newLog()
	for k, v := range l.keys {
		c.keys[k] = v
	}
	for k, v := range l.lastSeq {
		c.lastSeq[k] = v
	}
	for k, v := range l.lastWrite {
		c.lastWrite[k] = v
	}
	c.confirmed = copyHeads(l.confirmed)
	c.shown = append([]RecordID(nil), l.shown...)
	return c
}

type hsnap struct {
	snap   *Snapshot
	logs   map[string]*agentLog
	should map[RecordID]bool
}

type harness struct {
	w       *World
	ids     [4]MachineID // "" for a slot not (yet) in the room
	advance Time
	skewAt  map[MachineID]int

	writeIdx  map[RecordID]int
	recs      map[RecordID]*Record // every record written, for the true author history
	restored  map[MachineID]bool   // author was ever restored from backup
	logs      map[MachineID]map[string]*agentLog
	should    map[MachineID]map[RecordID]bool // records the machine must hold (Q5, D2)
	history   map[MachineID]map[RecordID]map[int]bool
	snaps     map[MachineID]*hsnap
	handledBy map[string]bool
	outSlot   int // the slot whose machine cannot accept connections, or -1
	err       error
}

func newHarness(skews [3]Time, advance Time, outSlot int) *harness {
	h := &harness{w: room("A", "B", "C"), ids: [4]MachineID{"A", "B", "C", ""}, advance: advance, outSlot: outSlot,
		skewAt: map[MachineID]int{}, writeIdx: map[RecordID]int{}, recs: map[RecordID]*Record{}, restored: map[MachineID]bool{},
		logs: map[MachineID]map[string]*agentLog{}, should: map[MachineID]map[RecordID]bool{},
		history: map[MachineID]map[RecordID]map[int]bool{}, snaps: map[MachineID]*hsnap{},
		handledBy: map[string]bool{}}
	for i, id := range h.ids[:3] {
		h.w.SetSkew(id, skews[i])
		h.addMachine(id)
	}
	if outSlot >= 0 {
		h.w.Machine(h.ids[outSlot]).NoInbound = true
	}
	for _, id := range h.ids[:3] {
		h.backup(id)
	}
	h.record()
	return h
}

// exchanged checks one pull's text against the views of the two ends.
func (h *harness) exchanged(p, s MachineID, r PullResult) {
	w := h.w
	// Q13: no text reaches a machine from a server that knows it was removed.
	if len(r.TextTo) > 0 && !w.members[p] && w.knows(s, p) {
		h.fail("Q13: %s sent text to %s, which it knows was removed", s, p)
	}
	if len(r.TextFrom) > 0 && !w.members[s] && w.knows(p, s) {
		h.fail("Q13: %s offered text to %s, which it knows was removed", p, s)
	}
	// Q14: text before a no-history admission never reaches the machine from
	// one that knows its permission, in a response or in an offer.
	for _, id := range r.TextTo {
		if w.knows(s, p) && !mayRead(w.admission[p], h.recs[id]) {
			h.fail("Q14: %s gave %s text from before its admission", s, p)
		}
	}
	for _, id := range r.TextFrom {
		if w.knows(p, s) && !mayRead(w.admission[s], h.recs[id]) {
			h.fail("Q14: %s offered %s text from before its admission", p, s)
		}
	}
}

// completePull extends a's should-set by b's, for records a takes.
func (h *harness) completePull(a, b MachineID) {
	if len(h.w.Machine(b).withhold) != 0 {
		return
	}
	for id := range h.should[b] {
		if au := h.recs[id].Author; h.w.Machine(a).st.view.ever[au] && (au == b || h.w.Machine(b).st.view.ever[au]) {
			h.should[a][id] = true
		}
	}
}

func (h *harness) addMachine(id MachineID) {
	h.logs[id] = map[string]*agentLog{}
	for _, a := range h.w.Machine(id).agents {
		h.logs[id][a] = newLog()
	}
	h.should[id] = map[RecordID]bool{}
}

func (h *harness) fail(format string, args ...any) {
	if h.err == nil {
		h.err = fmt.Errorf(format, args...)
	}
}

func (h *harness) wrote(m MachineID, id RecordID) {
	if _, ok := h.writeIdx[id]; !ok {
		h.writeIdx[id] = len(h.writeIdx) + 1
		h.recs[id] = h.w.Machine(m).get(id).rec
	}
	h.should[m][id] = true
}

// follows reports whether r comes after x in their author's true history,
// computed from the records as written.
func (h *harness) follows(r *Record, x RecordID) bool {
	seen := map[RecordID]bool{}
	todo := []*Record{r}
	for len(todo) > 0 {
		c := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		for _, p := range c.preds() {
			if p.ID == x {
				return true
			}
			if !seen[p.ID] && h.recs[p.ID] != nil {
				seen[p.ID] = true
				todo = append(todo, h.recs[p.ID])
			}
		}
	}
	return false
}

// wantLate is the promise, in one sentence: a record is marked late exactly
// when the agent has already confirmed (or was shown earlier in the same
// listing) a record of the same author that it does not follow.
func (h *harness) wantLate(l *agentLog, r *Record) bool {
	for _, x := range l.confirmed[r.Author] {
		if !h.follows(r, x) {
			return true
		}
	}
	return false
}

func (h *harness) backup(id MachineID) {
	s := &hsnap{snap: h.w.Backup(id), logs: map[string]*agentLog{}, should: map[RecordID]bool{}}
	for a, l := range h.logs[id] {
		s.logs[a] = l.clone()
	}
	for r := range h.should[id] {
		s.should[r] = true
	}
	h.snaps[id] = s
}

func (h *harness) newestOwnMessage(m *Machine) RecordID {
	var best *Record
	for _, x := range m.st.recs {
		if x.rec.Author == m.ID && x.rec.Kind == Message && x.dropped == Live &&
			(best == nil || h.writeIdx[x.rec.ID] > h.writeIdx[best.ID]) {
			best = x.rec
		}
	}
	if best == nil {
		return ""
	}
	return best.ID
}

// step applies one operation. Operations that do not apply (a pull from a
// sleeping machine, say) do nothing, as they would in life.
func (h *harness) step(o op) {
	w := h.w
	w.Tick++
	a, b := h.ids[o.a], h.ids[o.b]
	if a == "" || (b == "" && pair(o.k)) {
		h.record()
		h.check()
		return
	}
	ma := w.Machine(a)
	switch o.k {
	case opSendTo, opSendAll:
		msg := Msg{Body: fmt.Sprintf("m%d", len(h.writeIdx)+1), To: []Addr{at(b)}}
		if o.k == opSendAll {
			msg.To = nil
		}
		if id, err := w.Send(a, ag(a), msg); err == nil {
			h.wrote(a, id)
		}
	case opPull, opPullLimit1, opPullLost:
		opt := PullOpt{}
		if o.k == opPullLimit1 {
			opt.Limit = 1
		}
		if o.k == opPullLost {
			opt.LostReply = true
		}
		r, err := w.Pull(a, b, opt)
		if err == nil && !opt.LostReply && r.Sent != r.Stored {
			h.fail("Q12: %s pulled from %s: %+v; a page slot went on something refused", a, b, r)
		}
		h.exchanged(a, b, r)
		if err == nil && o.k == opPull {
			h.completePull(a, b)
		}
	case opOffer:
		r, err := w.Pull(a, b, PullOpt{Offer: true})
		if err == nil && r.Sent != r.Stored {
			h.fail("Q12: %s pulled from %s with an offer: %+v", a, b, r)
		}
		h.exchanged(a, b, r)
		if err == nil {
			h.completePull(a, b)
		}
	case opHold:
		if len(w.pending) == 0 {
			_, _ = w.Hold(a, b, PullOpt{})
		}
	case opRelease:
		if len(w.pending) > 0 {
			x := w.pending[0]
			r, _ := w.Release(0)
			h.exchanged(x.puller, x.server, r)
		}
	case opRemove:
		_ = w.Remove(a, b)
	case opVoid:
		_ = w.VoidAdmission(a, b)
	case opLearn:
		_, _ = w.Learn(a)
	case opToggle:
		if ma.Awake {
			w.Sleep(a)
		} else {
			w.Wake(a)
		}
	case opCrash:
		w.Crash(a)
	case opRead:
		h.read(a, false)
	case opPartialRead:
		h.read(a, true)
	case opPeekCrash:
		if ma.Awake {
			before := map[string][]Entry{}
			for _, g := range ma.agents {
				v, _ := w.Inbox(a, g)
				for _, e := range v.Entries {
					h.checkShown(ma, g, e)
				}
				before[g] = v.Entries
			}
			w.Crash(a)
			// Nothing was confirmed, so everything shown is shown again, the
			// same way.
			for _, g := range ma.agents {
				v, _ := w.Inbox(a, g)
				if fmt.Sprint(keysLate(v.Entries)) != fmt.Sprint(keysLate(before[g])) {
					h.fail("D3: after a crash before confirming, %s/%s shown %v, before %v", a, g, keysLate(v.Entries), keysLate(before[g]))
				}
			}
		}
	case opHandle:
		if !ma.Awake {
			break
		}
		for _, id := range h.logs[a][ag(a)].shown {
			key := string(a) + "|" + string(id)
			if h.handledBy[key] {
				continue
			}
			if r, err := w.Handle(a, ag(a), id); err == nil {
				h.handledBy[key] = true
				h.wrote(a, r)
				break
			}
		}
	case opAdvance:
		w.Advance(h.advance)
	case opSkew:
		i := h.skewAt[a] % len(skewSteps)
		h.skewAt[a]++
		w.SetSkew(a, skewSteps[i])
	case opBackup:
		h.backup(a)
	case opRestore:
		s := h.snaps[a]
		w.Restore(a, s.snap)
		h.restored[a] = true
		for ag, l := range s.logs {
			h.logs[a][ag] = l.clone()
		}
		h.should[a] = map[RecordID]bool{}
		for r := range s.should {
			h.should[a][r] = true
		}
	case opWithdraw, opReplace:
		if !ma.Awake {
			break
		}
		if t := h.newestOwnMessage(ma); t != "" {
			var id RecordID
			var err error
			if o.k == opWithdraw {
				id, err = w.Withdraw(a, ag(a), t)
			} else {
				id, err = w.Replace(a, ag(a), t, "new text")
			}
			if err == nil {
				h.wrote(a, id)
			}
		}
	case opDiskLost:
		if !w.members[a] {
			break // only a member is re-added
		}
		n := w.DiskLost(a)
		w.Machine(n).NoInbound = o.a == h.outSlot
		h.ids[o.a] = n
		h.addMachine(n)
		h.backup(n)
	case opWithhold:
		var best *Record
		for _, x := range ma.st.recs {
			if x.rec.Author != a && (best == nil || h.writeIdx[x.rec.ID] > h.writeIdx[best.ID]) {
				best = x.rec
			}
		}
		if best != nil {
			w.Withhold(a, best.ID)
		}
	case opAdmit:
		if h.ids[3] != "" {
			_, _ = w.Grant(h.ids[3], a, o.b != 0) // a second grant
			break
		}
		if _, err := w.Grant("D", a, o.b != 0, "d"); err == nil {
			w.SetSkew("D", 0)
			h.ids[3] = "D"
			h.addMachine("D")
			h.backup("D")
		}
	}
	h.record()
	h.check()
}

func keysLate(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, fmt.Sprintf("%s/%v", e.Key, e.Late))
	}
	return out
}

// checkShown is Q10: a message withdrawn before an agent was shown it, or
// whose text the machine may not read, is never shown.
func (h *harness) checkShown(m *Machine, a string, e Entry) {
	if e.Kind != Shown {
		return
	}
	if x := m.get(e.Record); x.dropped != Live || m.withdrawnRef(x.rec.ID, x.rec.Author) {
		h.fail("Q10: %s/%s shown %s whose text is %v", m.ID, a, e.Record, x.dropped)
	}
}

// read shows every agent on id its inbox, checks Q1, Q2 and Q10 on what it
// confirms, and confirms all of it (or the first half).
func (h *harness) read(id MachineID, half bool) {
	m := h.w.Machine(id)
	if !m.Awake || m.Gone {
		return
	}
	for _, a := range m.agents {
		v, err := h.w.List(id, a)
		if err != nil {
			h.fail("inbox: %v", err)
			return
		}
		es := v.Entries
		if half {
			es = es[:len(es)/2]
		}
		l := h.logs[id][a]
		for _, e := range es {
			if e.Kind == VoidedNotice {
				if l.keys[e.Key] || !l.keys[string(e.Record)] {
					h.fail("R1: %s/%s told of a voided message it was not shown, or twice", id, a)
				}
				l.keys[e.Key] = true
				continue
			}
			h.checkShown(m, a, e)
			if l.keys[e.Key] {
				h.fail("Q1: %s/%s shown %s %s twice", id, a, e.Kind, e.Key)
			}
			l.keys[e.Key] = true
			s := e.Pos.Stream
			if e.Pos.Seq <= l.lastSeq[s] {
				h.fail("Q2: %s/%s shown %v after seq %d", id, a, e.Pos, l.lastSeq[s])
			}
			l.lastSeq[s] = e.Pos.Seq
			wi := h.writeIdx[e.Record]
			if want := h.wantLate(l, h.recs[e.Record]); e.Late != want {
				h.fail("Q2: %s/%s shown %s's record #%d with late=%v, want %v", id, a, s.Author, wi, e.Late, want)
			}
			if wi < l.lastWrite[s.Author] && !e.Late {
				h.fail("Q2: %s/%s shown %s's record #%d after #%d, not marked late", id, a, s.Author, wi, l.lastWrite[s.Author])
			}
			if e.Late && !h.restored[s.Author] {
				h.fail("Q2: late flag without a restore of %s", s.Author)
			}
			if wi > l.lastWrite[s.Author] {
				l.lastWrite[s.Author] = wi
			}
			l.confirmed[s.Author] = append(l.confirmed[s.Author], e.Record)
			if e.Kind == Shown {
				l.shown = append(l.shown, e.Record)
			}
		}
		if err := h.w.Seen(id, a, es); err != nil {
			h.fail("seen: %v", err)
		}
	}
}

// record notes which bodies every machine holds after this tick, for Q7.
func (h *harness) record() {
	for id, m := range h.w.machines {
		if m.Gone {
			continue
		}
		hm := h.history[id]
		if hm == nil {
			hm = map[RecordID]map[int]bool{}
			h.history[id] = hm
		}
		for rid, x := range m.st.recs {
			if x.dropped != Live {
				continue
			}
			if hm[rid] == nil {
				hm[rid] = map[int]bool{}
			}
			hm[rid][h.w.Tick] = true
		}
	}
}

func (h *harness) check() {
	w := h.w
	if w.Forks != 0 {
		h.fail("fork: %d", w.Forks)
	}
	for _, id := range w.order {
		m := w.Machine(id)
		if m.Gone {
			continue
		}
		// R1: a machine holds records only by authors that are or were
		// members in its view.
		for _, x := range m.st.recs {
			if x.rec.Author != id && !m.st.view.ever[x.rec.Author] {
				h.fail("R1: %s holds a record by %s, never a member in its view", id, x.rec.Author)
			}
		}
	}
	for _, id := range w.Members() {
		m := w.Machine(id)
		// Q8: every machine holds a gap-free prefix of every stream, closed
		// under links.
		n := 0
		for s, c := range m.st.contig {
			for k := 1; k <= c; k++ {
				x := m.at(Pos{s, k})
				if x == nil {
					h.fail("Q8: %s has a hole at %v.%d", id, s, k)
					continue
				}
				n++
				for _, ln := range x.rec.Links {
					if l := m.at(ln.At); l == nil || l.rec.ID != ln.ID {
						h.fail("Q8: %s holds %v without its link %v", id, x.rec.Pos(), ln.At)
					}
				}
			}
		}
		if n != len(m.st.recs) {
			h.fail("Q8: %s holds %d records outside its prefixes", id, len(m.st.recs)-n)
		}
		for rid, x := range m.st.recs {
			// Q11: a body is absent only for a reason this machine can check,
			// or is marked missing (and so repairable).
			if m.Awake && !m.reasonHolds(x, w.Now) {
				h.fail("Q11: %s holds %s as %v without evidence", id, rid, x.dropped)
			}
			// Q9 and Q10: no text past 30 days by the holder's clock, none
			// that was withdrawn, none it may not read.
			if x.dropped == Live && m.Awake && m.textGone(x.rec, w.Now) != Live {
				h.fail("Q9/Q10: %s keeps text of %s that is %v", id, rid, m.textGone(x.rec, w.Now))
			}
		}
		// Q5 and D2: a time-ordered chain of complete pulls carries every record.
		for rid := range h.should[id] {
			if m.st.recs[rid] == nil && m.st.view.ever[h.recs[rid].Author] {
				h.fail("Q5: %s lacks %s, carried to it by a chain of complete pulls", id, rid)
			}
		}
		// Q7: every copy report was true of the state it described (the end
		// of the operation before the report was made).
		for peer, ob := range m.st.obs {
			for rid, x := range m.st.recs {
				// A response's summary is taken after the server stored the
				// offer, so it may describe the end of the operation itself.
				if ob.Have.HoldsBody(x.rec.Pos()) && !h.history[peer][rid][ob.Tick-1] && !h.history[peer][rid][ob.Tick] {
					h.fail("Q7: %s says %s held %s at tick %d; it did not", id, peer, rid, ob.Tick)
				}
			}
		}
		// Q6: every position an agent's cursor has passed that was for it was
		// shown, told expired, withdrawn before it was shown, before the
		// agent's time, or handled by it.
		for _, a := range m.agents {
			g := m.st.agents[a]
			l := h.logs[id][a]
			for s, c := range g.cursor {
				for k := g.start[s] + 1; k <= c; k++ {
					x := m.at(Pos{s, k})
					if x == nil {
						h.fail("Q6: %s/%s cursor past a position not held: %v.%d", id, a, s, k)
						continue
					}
					rid := x.rec.ID
					if !relevant(x.rec, id, a) || l.keys[string(rid)] ||
						l.keys["x:"+string(rid)] || x.dropped == Withdrawn || x.dropped == NotAdmitted ||
						h.handledBy[string(id)+"|"+string(rid)] {
						continue
					}
					h.fail("Q6: %s/%s silently passed %v.%d (%v)", id, a, s, k, x.dropped)
				}
			}
		}
	}
}

// readableSomewhere reports whether a member holds r's text and may give it
// to machine to.
func (h *harness) readableSomewhere(r *Record, to MachineID) bool {
	for _, o := range h.w.Members() {
		if x := h.w.Machine(o).get(r.ID); x != nil && x.dropped == Live && mayRead(h.w.admission[to], r) {
			return true
		}
	}
	return false
}

func (h *harness) pullAll() {
	w := h.w
	members := w.Members()
	for rounds := 0; ; rounds++ {
		if rounds > 4 {
			h.fail("Q4: no convergence after 4 rounds")
			return
		}
		moved := false
		for _, p := range members {
			for _, s := range members {
				if p == s || w.Machine(s).NoInbound {
					continue
				}
				w.Tick++
				r, err := w.Pull(p, s, PullOpt{Offer: true})
				if err != nil {
					h.fail("quiesce pull: %v", err)
				}
				h.exchanged(p, s, r)
				if r.Stored > 0 || r.Offered > 0 {
					moved = true
				}
				h.record()
			}
		}
		h.check()
		if !moved && rounds > 0 {
			return
		}
	}
}

func (h *harness) readAll() {
	for _, id := range h.w.Members() {
		h.w.Tick++
		h.read(id, false)
		h.record()
		h.check()
	}
}

// boundedProgress is Q12 at rest: with a page limit of one, no new sends
// and withholding left in place, repeated pulls between every pair make
// progress (every slot is stored) until the server has nothing left that
// it would send.
func (h *harness) boundedProgress() {
	w := h.w
	members := w.Members()
	// At least two rounds: an offer needs a summary from an earlier contact.
	for moved, rounds := true, 0; moved || rounds < 2; rounds++ {
		if rounds > 200 {
			h.fail("Q12: limited pulls did not settle")
			return
		}
		moved = false
		for _, p := range members {
			for _, s := range members {
				if p == s || w.Machine(s).NoInbound {
					continue
				}
				w.Tick++
				r, err := w.Pull(p, s, PullOpt{Limit: 1, Offer: true})
				if err != nil {
					h.fail("bounded pull: %v", err)
				}
				if r.Sent != r.Stored {
					h.fail("Q12: %s from %s: %+v under a page limit", p, s, r)
				}
				h.exchanged(p, s, r)
				// A refused offer was built on a stale summary; the response
				// refreshed it, so try again.
				moved = moved || r.Stored > 0 || r.Offered > 0
				h.record()
			}
		}
		h.check()
	}
	for _, p := range members {
		for _, s := range members {
			mp, ms := w.Machine(p), w.Machine(s)
			if p != s && len(w.page(ms, p, mp.summary(), mp.takes())) != 0 {
				h.fail("Q12: %s still has records for %s after limited pulls settled", s, p)
			}
			if p != s && ms.NoInbound && len(w.page(ms, p, mp.summary(), mp.takes())) != 0 {
				h.fail("Q15: %s, which only pulls, still has records for %s", s, p)
			}
		}
	}
}

// quiesce wakes everyone; first checks bounded-page progress with
// withholding still in place; then clears it, has every machine pull from
// every other until nothing moves, has every agent read, and checks Q3 and
// Q4. Then it lets 33 days pass and checks that nothing is left waiting.
func (h *harness) quiesce() {
	w := h.w
	w.pending = nil
	for _, id := range w.order {
		w.Wake(id)
		for more := true; more && !w.Machine(id).Gone; {
			more, _ = w.Learn(id)
		}
	}
	w.Tick++
	h.record()
	h.check()
	members := w.Members()
	h.boundedProgress()
	for _, id := range members {
		w.Machine(id).withhold = map[RecordID]bool{}
	}
	h.pullAll()
	h.readAll()
	ref := w.Machine(members[0])
	for _, id := range members[1:] {
		m := w.Machine(id)
		if len(m.st.recs) != len(ref.st.recs) {
			h.fail("Q4: %s holds %d records, %s holds %d", id, len(m.st.recs), ref.ID, len(ref.st.recs))
		}
		for rid := range ref.st.recs {
			if m.st.recs[rid] == nil {
				h.fail("Q4: %s lacks %s held by %s", id, rid, ref.ID)
			}
		}
	}
	for _, id := range members {
		m := w.Machine(id)
		for rid, x := range m.st.recs {
			if x.dropped == Missing && h.readableSomewhere(x.rec, id) {
				h.fail("Q4: %s still lacks the text of %s that another member holds", id, rid)
			}
		}
		for _, a := range m.agents {
			v, _ := w.Inbox(id, a)
			if len(v.Entries) != 0 {
				h.fail("Q3: %s/%s still has %d unread after reading", id, a, len(v.Entries))
			}
			lostText := map[MachineID]bool{}
			for _, gp := range v.Waiting {
				if gp.Text {
					lostText[gp.Stream.Author] = true
				}
			}
			for _, gp := range v.Waiting {
				// A link may wait only behind text of the same author that no
				// member holds; that wait ends when the text would expire.
				if gp.Link && !lostText[gp.Stream.Author] {
					h.fail("Q3: %s waits forever on the link of %v", id, gp.Stream)
				}
				if gp.Text && h.readableSomewhere(m.at(gp.At).rec, id) {
					h.fail("Q3: %s/%s waits for text another member holds", id, a)
				}
			}
		}
	}
	// Q13 at rest: once every member knows, a machine that is no longer a
	// member gets nothing and gives nothing.
	for _, x := range w.order {
		if w.members[x] || w.Machine(x).Gone {
			continue
		}
		for _, s := range members {
			if _, err := w.Pull(x, s, PullOpt{Offer: true}); err == nil {
				h.fail("Q13: %s, no longer a member, pulled from %s", x, s)
			}
			if _, err := w.Pull(s, x, PullOpt{Offer: true}); err == nil {
				h.fail("Q13: %s pulled from %s, no longer a member", s, x)
			}
		}
	}
	// Text lost with every copy leaves a reader waiting until it would have
	// expired; after that, nothing may be left.
	w.Advance(33 * Day)
	h.readAll()
	for _, id := range w.Members() {
		m := w.Machine(id)
		for _, a := range m.agents {
			g := m.st.agents[a]
			for s := range m.st.contig {
				if g.cursor[s] < m.contigOf(s) {
					h.fail("Q3: %s/%s stuck at %v.%d of %d after everything expired", id, a, s, g.cursor[s], m.contigOf(s))
				}
			}
		}
	}
}

func runSchedule(skews [3]Time, adv Time, ops []op, outSlot int) error {
	h := newHarness(skews, adv, outSlot)
	for i, o := range ops {
		h.step(o)
		if h.err != nil {
			return fmt.Errorf("after step %d of %v: %w", i+1, ops, h.err)
		}
	}
	h.quiesce()
	if h.err != nil {
		return fmt.Errorf("at quiescence after %v: %w", ops, h.err)
	}
	return nil
}

// alphabet is every operation on machines 0..n-1.
func alphabet(n int) []op {
	var ops []op
	for a := 0; a < n; a++ {
		for b := 0; b < n; b++ {
			if a != b {
				ops = append(ops, op{opSendTo, a, b}, op{opPull, a, b}, op{opPullLost, a, b}, op{opPullLimit1, a, b},
					op{opOffer, a, b}, op{opHold, a, b}, op{opRemove, a, b}, op{opVoid, a, b})
			}
		}
		for _, k := range []opKind{opSendAll, opToggle, opCrash, opRead, opPartialRead, opPeekCrash,
			opHandle, opSkew, opRestore, opWithdraw, opBackup, opReplace, opWithhold, opLearn} {
			ops = append(ops, op{k: k, a: a})
		}
		if a < 3 {
			ops = append(ops, op{k: opAdmit, a: a}, op{k: opAdmit, a: a, b: 1})
		}
	}
	return append(ops, op{k: opAdvance}, op{k: opRelease})
}

func deep() bool { return os.Getenv("A2P2_MODEL_DEEP") == "1" }

// seeds are short prefixes the exhaustive search starts from, so that small
// depths reach interesting states.
var seeds = [][]op{
	nil,
	{{opSendTo, 0, 2}, {opBackup, 0, 0}, {opSendTo, 0, 2}, {opPull, 1, 0}},
	{{opSendTo, 0, 2}, {opPull, 2, 0}, {opRead, 2, 0}, {opBackup, 2, 0}},
	{{opSendTo, 0, 2}, {opPull, 2, 0}, {opAdmit, 2, 0}, {opSendTo, 2, 0}, {opPull, 3, 2}},
	// D admitted by C without history, told to A; A then removed by B.
	{{opSendTo, 0, 2}, {opPull, 1, 0}, {opAdmit, 2, 0}, {opLearn, 0, 0}, {opOffer, 3, 2}, {opRemove, 0, 1}},
}

// Requirements: D2, D3, D4, D5, D6, D7, D9, U3, R1-R3 (Q1-Q15 over every schedule to a small depth)
func TestEverySmallScheduleKeepsTheDeliveryProperties(t *testing.T) {
	full := alphabet(4)
	for a := 0; a < 4; a++ {
		full = append(full, op{k: opDiskLost, a: a})
	}
	var member []op // the membership, offer and held-response operations
	for _, o := range full {
		if o.k >= opOffer {
			member = append(member, o)
		}
	}
	var count atomic.Int64
	var rec func(t *testing.T, prefix []op, levels [][]op, out int)
	rec = func(t *testing.T, prefix []op, levels [][]op, out int) {
		if t.Failed() {
			return
		}
		count.Add(1)
		if err := runSchedule([3]Time{0, 0, 0}, 16*Day, prefix, out); err != nil {
			t.Fatalf("outbound-only slot %d: %v", out, err)
		}
		if len(levels) == 0 {
			return
		}
		for _, o := range levels[0] {
			rec(t, append(append([]op(nil), prefix...), o), levels[1:], out)
		}
	}
	t.Run("seeds", func(t *testing.T) {
		for i, s := range seeds {
			levels := [][]op{full}
			switch {
			case deep() && (i == 0 || i == 3):
				levels = [][]op{full, full, member}
			case deep():
				levels = [][]op{full, full}
			case i == 3:
				levels = [][]op{full, member} // the full product is in the deep run
			}
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()
				if len(levels) == 3 { // split the largest searches by first step
					rec(t, s, nil, -1)
					for k, o := range levels[0] {
						prefix := append(append([]op(nil), s...), o)
						t.Run(fmt.Sprint(k), func(t *testing.T) {
							t.Parallel()
							rec(t, prefix, levels[1:], -1)
						})
					}
				} else {
					rec(t, s, levels, -1)
				}
				rec(t, s, [][]op{full}, 2)
			})
		}
	})
	t.Logf("%d schedules, alphabet %d (%d membership)", count.Load(), len(full), len(member))
}

// Requirements: D2, D3, D4, D5, D6, D7, D9, U3 (Q1-Q11 over long random schedules)
func TestRandomSchedulesKeepTheDeliveryProperties(t *testing.T) {
	runs, steps := 1000, 40
	if deep() {
		runs, steps = 20000, 80
	}
	seed := int64(20261123)
	rng := rand.New(rand.NewSource(seed))
	ops := alphabet(4)
	for a := 0; a < 4; a++ {
		ops = append(ops, op{k: opDiskLost, a: a})
	}
	weights := func(o op) int {
		switch o.k {
		case opDiskLost, opRestore, opAdvance, opSkew, opAdmit, opRemove, opVoid:
			return 1
		case opPull, opSendTo, opRead:
			return 6
		}
		return 2
	}
	var bag []op
	for _, o := range ops {
		for i := 0; i < weights(o); i++ {
			bag = append(bag, o)
		}
	}
	skewSet := []Time{0, Day, -Day, 3 * Hour}
	for run := 0; run < runs; run++ {
		out := -1
		if run%2 == 1 {
			out = 2 // C only ever pulls
		}
		var skews [3]Time
		for i := range skews {
			skews[i] = skewSet[rng.Intn(len(skewSet))]
		}
		var sched []op
		for i := 0; i < steps; i++ {
			sched = append(sched, bag[rng.Intn(len(bag))])
		}
		if err := runSchedule(skews, 10*Day, sched, out); err != nil {
			small := shrink(skews, 10*Day, sched, out)
			t.Fatalf("seed %d run %d skews %v outbound-only slot %d: %v\nsmallest failing schedule: %v\n%v",
				seed, run, skews, out, err, small, runSchedule(skews, 10*Day, small, out))
		}
	}
}

// shrink removes operations from a failing schedule while it still fails.
func shrink(skews [3]Time, adv Time, ops []op, out int) []op {
	for again := true; again; {
		again = false
		for i := range ops {
			try := append(append([]op(nil), ops[:i]...), ops[i+1:]...)
			if runSchedule(skews, adv, try, out) != nil {
				ops, again = try, true
				break
			}
		}
	}
	return ops
}
