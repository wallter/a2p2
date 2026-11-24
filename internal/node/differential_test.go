package node

// The differential test: the same schedules run through the delivery model
// (internal/model/delivery, the oracle) and through Machine over real
// SQLite stores, and after every step every inbox listing, wait, thread
// view and branch view is compared, and so is the durable state: which
// records each machine holds and why their text is or is not there. Ids and
// times are normalised; epochs are the same on both sides, because each
// Machine draws its epochs from the model's counter.
//
// What the model does not have is checked against invariants instead:
// registration of an agent mid-schedule (the model's agents exist from the
// start; the implementation's start at the current end, so the model's
// listing is compared without the positions before it), records the model
// cannot express (references of the wrong kind or author, placed directly:
// every view must be as if they were not there), and cursors (walking from
// where each agent's time began gives exactly what walking from its
// cursors gives).
//
// Ties: record ids order messages of equal created time (thread views) and
// replacements of equal version, and the two sides' ids differ. Every
// message and replace is written, on clones, with different texts until
// both sides' ids order every tie the same way and fall in the same half
// of the id space; a schedule where no text does so stops (none has).

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
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

const diffRoom = "r_0123456789abcdef0123456789abcdef"

var diffBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// lateAgent is registered only by opRegister; the model has it from the
// start.
const lateAgent = "dave"

// sim is one machine on the implementation side.
type sim struct {
	name   delivery.MachineID
	agents []string // the model's agents; lateAgent only once registered here
	key    ed25519.PrivateKey
	path   string
	st     *store.Store
	m      *Machine
	r      *Rooms    // with the real exchange (pull_differential_test.go)
	x      *Exchange // with the real exchange
}

type backup struct {
	machine delivery.MachineID
	model   *delivery.Snapshot
	path    string // a copy of the database files, never written again
	reg     map[streamKey]int64
	isReg   bool
	px      *pxBackup // with the real exchange
}

type agentKey struct {
	machine delivery.MachineID
	agent   string
}

// listing is what an agent was last shown, on both sides.
type listing struct {
	model []delivery.Entry
	impl  []Entry
}

// written is what the harness knows of one record both sides wrote.
type written struct {
	model   delivery.RecordID
	impl    string
	created int64 // hours since diffBase, by the writer's clock
	kind    core.Kind
	author  delivery.MachineID
	ref     string // impl id
	version int64
}

// counts are shared by a harness and its clones.
type counts struct{ retries, stopped, injected, repaired, missing int }

type harness struct {
	t        testing.TB
	w        *delivery.World
	sims     map[delivery.MachineID]*sim
	order    []delivery.MachineID
	foreign  ed25519.PrivateKey // a key the model has never heard of
	label    map[string]string  // model or implementation record id -> "#n"
	pair     map[delivery.RecordID]string
	implPair map[string]delivery.RecordID
	nodes    map[string]delivery.MachineID
	writes   []written
	msgs     []delivery.RecordID // messages written, model ids, in order
	reps     []string            // replaces written, impl ids
	shown    map[agentKey]listing
	backups  []backup
	injected map[string]bool
	// reg is where lateAgent's time began on each machine where it is
	// registered (its cursors right after registration).
	reg     map[delivery.MachineID]map[streamKey]int64
	epochs  uint64 // mirrors the model's epoch counter
	ids     uint64 // other random bytes
	texts   int
	log     []string
	stopped bool
	c       *counts
	// px, when set, runs the real exchange and the room log instead of
	// pull below (pull_differential_test.go).
	px *pxState
}

// keys returns one key per name whose node ids sort as the names do, so
// stream order, which sorts by author, is the same on both sides.
func keys(names []delivery.MachineID) []ed25519.PrivateKey {
	var out []ed25519.PrivateKey
	prev := ""
	for _, n := range names {
		for i := 0; ; i++ {
			seed := sha256.Sum256([]byte(fmt.Sprintf("differential %s %d", n, i)))
			k := ed25519.NewKeyFromSeed(seed[:])
			id := hex.EncodeToString(k.Public().(ed25519.PublicKey))
			if id > prev {
				out, prev = append(out, k), id
				break
			}
		}
	}
	return out
}

func newHarness(t testing.TB, machines map[delivery.MachineID][]string) *harness {
	h := &harness{t: t, w: delivery.NewWorld(), sims: map[delivery.MachineID]*sim{},
		label: map[string]string{}, pair: map[delivery.RecordID]string{}, implPair: map[string]delivery.RecordID{},
		nodes: map[string]delivery.MachineID{}, shown: map[agentKey]listing{}, injected: map[string]bool{},
		reg: map[delivery.MachineID]map[streamKey]int64{}, c: &counts{}}
	var names []delivery.MachineID
	for n := range machines {
		names = append(names, n)
	}
	slices.Sort(names)
	seed := sha256.Sum256([]byte("differential foreign"))
	h.foreign = ed25519.NewKeyFromSeed(seed[:])
	for i, k := range keys(names) {
		n := names[i]
		h.w.AddMachine(n, machines[n]...)
		s := &sim{name: n, key: k, path: filepath.Join(t.TempDir(), "a2p2.db")}
		st, err := store.Open(s.path)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.AddRoom(context.Background(), diffRoom, "team", "", diffBase); err != nil {
			t.Fatal(err)
		}
		s.st = st
		h.start(s, nil)
		for _, a := range machines[n] {
			if a == lateAgent {
				continue
			}
			if _, err := s.m.Register(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			s.agents = append(s.agents, a)
		}
		h.nodes[s.m.ID] = n
		h.sims[n] = s
		h.order = append(h.order, n)
	}
	return h
}

// start runs a daemon over s.st: a fresh Machine, or one that continues
// the streams out names (a clone of a running daemon).
func (h *harness) start(s *sim, out map[string]string) {
	name := s.name
	s.m = NewMachine(s.st, s.key, MachineOpts{
		Now: func() time.Time {
			return diffBase.Add(time.Duration(h.w.Now+h.w.Machine(name).Skew) * time.Hour)
		},
		Rand: func(b []byte) {
			if len(b) != 8 { // not an epoch (request ids): keep the epoch counter in step
				h.ids++
				x := sha256.Sum256([]byte(fmt.Sprintf("differential ids %s %d", name, h.ids)))
				copy(b, x[:])
				return
			}
			h.epochs++
			clear(b)
			binary.BigEndian.PutUint64(b[len(b)-8:], (h.epochs*2654435761)%4294967291)
		},
	})
	for room, e := range out {
		s.m.out[room] = e
	}
	if h.px != nil {
		h.px.open(h, s)
	}
}

func (h *harness) close() {
	for _, s := range h.sims {
		s.st.Close()
	}
}

func cloneMap[K comparable, V any](m map[K]V) map[K]V {
	c := make(map[K]V, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// clone copies the whole world: the model, every database and every
// daemon's stream state.
func (h *harness) clone() *harness {
	c := *h
	c.w = h.w.Clone()
	c.sims = map[delivery.MachineID]*sim{}
	c.label, c.pair, c.implPair = cloneMap(h.label), cloneMap(h.pair), cloneMap(h.implPair)
	c.writes, c.msgs, c.reps = slices.Clone(h.writes), slices.Clone(h.msgs), slices.Clone(h.reps)
	c.shown, c.injected = cloneMap(h.shown), cloneMap(h.injected)
	c.reg = map[delivery.MachineID]map[streamKey]int64{}
	for k, v := range h.reg {
		c.reg[k] = cloneMap(v)
	}
	c.backups = slices.Clone(h.backups)
	c.log = slices.Clone(h.log)
	if h.px != nil {
		c.px = h.px.clone()
	}
	for n, s := range h.sims {
		cs := &sim{name: n, agents: slices.Clone(s.agents), key: s.key, path: filepath.Join(h.t.TempDir(), "a2p2.db")}
		copyDB(h.t, s.path, cs.path)
		st, err := store.Open(cs.path)
		if err != nil {
			h.t.Fatal(err)
		}
		cs.st = st
		c.start(cs, s.m.out)
		c.sims[n] = cs
	}
	return &c
}

// ---- operations ----

type opKind uint8

const (
	opSend opKind = iota + 1
	opReply
	opReplace
	opWithdraw
	opHandled
	opShow     // list the inbox and remember it, confirming nothing
	opSeenPart // confirm the first n entries of what was shown
	opSeenAll  // list and confirm everything (the inbox tool)
	opCrash    // the daemon restarts; the database survives
	opBackup
	opRestore // the latest backup is put back and the daemon starts
	opAdvance
	opSkew
	opRegister // lateAgent is registered here (implementation only)
	opPull     // machine pulls from server
	opInject   // a record the model cannot express is placed (implementation only)
	opLevel    // agent sets its level (n: delivery.Level)
	opRead     // agent reads a thread view (n 0) or skipped (n 1), marking what it showed
	opMore     // the real exchange's operations follow (pull_differential_test.go)
)

// What opInject places. Every one is a reference that does not check out.
const (
	injBadHandled      = iota // handled by an agent that may not report it
	injWithdrawOther          // the foreign key withdraws someone else's message
	injWithdrawReplace        // a withdraw naming a replace
	injReplaceReplace         // a replace naming a replace
	injHandledReplace         // a handled report naming a replace
	injBadThread              // a reply whose thread is not its parent's
	injKinds
)

type op struct {
	kind     opKind
	machine  delivery.MachineID
	server   delivery.MachineID // pull
	agent    string
	to       string // send: "" everyone, or an agent label on the same machine
	everyone bool   // reply: to everyone instead of the parent's author
	ask      bool
	target   int  // index into msgs (or reps); negative counts from the end
	n        int  // seenPart: how many; advance and skew: hours; inject: what
	same     bool // the step takes no time: created times can tie
}

func (o op) String() string {
	at := ""
	if o.same {
		at = " (same hour)"
	}
	switch o.kind {
	case opSend:
		return fmt.Sprintf("%s/%s send to=%q ask=%v%s", o.machine, o.agent, o.to, o.ask, at)
	case opReply:
		return fmt.Sprintf("%s/%s reply #%d everyone=%v ask=%v%s", o.machine, o.agent, o.target, o.everyone, o.ask, at)
	case opReplace:
		return fmt.Sprintf("%s/%s replace #%d%s", o.machine, o.agent, o.target, at)
	case opWithdraw:
		return fmt.Sprintf("%s/%s withdraw #%d%s", o.machine, o.agent, o.target, at)
	case opHandled:
		return fmt.Sprintf("%s/%s handled #%d%s", o.machine, o.agent, o.target, at)
	case opShow:
		return fmt.Sprintf("%s/%s show", o.machine, o.agent)
	case opSeenPart:
		return fmt.Sprintf("%s/%s seen first %d", o.machine, o.agent, o.n)
	case opSeenAll:
		return fmt.Sprintf("%s/%s inbox+seen", o.machine, o.agent)
	case opCrash:
		return fmt.Sprintf("%s crash", o.machine)
	case opBackup:
		return fmt.Sprintf("%s backup", o.machine)
	case opRestore:
		return fmt.Sprintf("%s restore", o.machine)
	case opAdvance:
		return fmt.Sprintf("advance %dh", o.n)
	case opSkew:
		return fmt.Sprintf("%s skew %dh", o.machine, o.n)
	case opRegister:
		return fmt.Sprintf("%s register %s", o.machine, lateAgent)
	case opPull:
		return fmt.Sprintf("%s pulls from %s%s", o.machine, o.server, pullFlags(o.n))
	case opInject:
		return fmt.Sprintf("%s inject %d #%d", o.machine, o.n, o.target)
	case opLevel:
		return fmt.Sprintf("%s/%s level %s", o.machine, o.agent, delivery.Level(o.n))
	case opRead:
		if o.n == 0 {
			return fmt.Sprintf("%s/%s reads thread #%d", o.machine, o.agent, o.target)
		}
		return fmt.Sprintf("%s/%s reads skipped", o.machine, o.agent)
	}
	return o.moreString()
}

func (h *harness) fail(format string, a ...any) {
	h.t.Helper()
	h.t.Fatalf("after:\n  %s\n%s", strings.Join(h.log, "\n  "), fmt.Sprintf(format, a...))
}

func pick[T any](xs []T, i int) (T, bool) {
	var zero T
	if i < 0 {
		i += len(xs)
	}
	if i < 0 || i >= len(xs) {
		return zero, false
	}
	return xs[i], true
}

func (h *harness) target(i int) (delivery.RecordID, string, bool) {
	id, ok := pick(h.msgs, i)
	return id, h.pair[id], ok
}

// registered says whether agent may act on machine m on both sides.
func (h *harness) registered(m delivery.MachineID, agent string) bool {
	s := h.sims[m]
	return s != nil && slices.Contains(s.agents, agent)
}

// errClass maps both sides' errors to one vocabulary.
func errClass(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, delivery.ErrNotHeld), errors.Is(err, ErrNotHeld):
		return "not held"
	case errors.Is(err, delivery.ErrNotAllow), errors.Is(err, ErrNotAllowed):
		return "not allowed"
	}
	return "error: " + err.Error()
}

// wrote checks a write's outcome on both sides and pairs the ids.
func (h *harness) wrote(m delivery.MachineID, mid delivery.RecordID, merr error, sent Sent, ierr error) {
	h.t.Helper()
	if a, b := errClass(merr), errClass(ierr); a != b {
		h.fail("model %s, implementation %s", a, b)
	}
	if merr != nil {
		return
	}
	if old, ok := h.pair[mid]; ok { // a repeated handled report
		if old != sent.ID {
			h.fail("model repeated record %s, implementation wrote a new one", h.label[string(mid)])
		}
		return
	}
	if _, ok := h.label[sent.ID]; ok {
		h.fail("implementation reused record %s", h.label[sent.ID])
	}
	l := fmt.Sprintf("#%d", len(h.pair)+1)
	h.pair[mid], h.implPair[sent.ID] = sent.ID, mid
	h.label[string(mid)], h.label[sent.ID] = l, l
	f := h.held(h.sims[m], sent.ID).Record.Fields()
	h.writes = append(h.writes, written{model: mid, impl: sent.ID, created: int64(f.Created.Sub(diffBase) / time.Hour),
		kind: f.Kind, author: m, ref: f.Ref, version: f.Version})
	switch f.Kind {
	case core.KindMessage:
		h.msgs = append(h.msgs, mid)
	case core.KindReplace:
		h.reps = append(h.reps, sent.ID)
	}
}

func (h *harness) held(s *sim, id string) store.Held {
	h.t.Helper()
	var x store.Held
	err := s.st.View(context.Background(), func(rd *store.Reader) error {
		var ok bool
		var err error
		x, ok, err = rd.Record(diffRoom, id)
		if err == nil && !ok {
			err = errors.New("not held")
		}
		return err
	})
	if err != nil {
		h.fail("record %s on %s: %v", h.id(id), s.name, err)
	}
	return x
}

// agrees reports whether every two records whose order is decided by id
// (messages of equal created time, which thread views sort by id;
// replacements of one message by one author at one
// version) are ordered the same by both sides' ids.
func (h *harness) agrees() bool {
	for i, a := range h.writes {
		for _, b := range h.writes[i+1:] {
			tie := (a.created == b.created && a.kind == core.KindMessage && b.kind == core.KindMessage) ||
				(a.kind == core.KindReplace && b.kind == core.KindReplace && a.author == b.author && a.ref == b.ref && a.version == b.version)
			if tie && (a.impl < b.impl) != (a.model < b.model) {
				return false
			}
		}
	}
	return true
}

// halves reports whether the newest write's ids, if any since n writes,
// fall in the same half of the id space on both sides. A record whose two
// ids are at opposite ends would make every later tie with it unlikely to
// order the same way, so every message and replace is written that way:
// then a later tie with it orders the same unless both fall in one half.
func (h *harness) halves(n int) bool {
	if len(h.writes) == n {
		return true
	}
	x := h.writes[len(h.writes)-1]
	return (x.impl[0] < '8') == (string(x.model)[0] < '8')
}

// apply runs one operation on both sides and compares everything. It
// returns the harness to continue with: a write that could tie is run on
// clones, with different texts, until one keeps both sides' id order.
func (h *harness) apply(o op) *harness {
	h.t.Helper()
	if h.stopped {
		return h
	}
	write := o.kind == opSend || o.kind == opReply || o.kind == opReplace
	if write && h.registered(o.machine, o.agent) {
		for k := 1; k <= 40; k++ {
			c := h.clone()
			c.do(o, k)
			if c.agrees() && c.halves(len(h.writes)) {
				h.c.retries += k - 1
				c.compare()
				h.close()
				return c
			}
			c.close()
		}
		h.c.stopped++
		h.stopped = true
		h.log = append(h.log, o.String()+" (stopped: no text keeps the id order)")
		h.t.Logf("stopped:\n  %s", strings.Join(h.log, "\n  "))
		return h
	}
	h.do(o, 0)
	if !h.agrees() {
		h.fail("ids order a tie differently after a write that was not retried")
	}
	h.compare()
	return h
}

func (h *harness) do(o op, variant int) {
	h.t.Helper()
	h.log = append(h.log, o.String())
	ctx := context.Background()
	if !o.same {
		h.advance(delivery.Hour)
	}
	if h.px != nil && h.px.skip(h, o) {
		return // the machine is asleep: neither side acts
	}
	s := h.sims[o.machine]
	text := func(prefix string) string {
		h.texts++
		t := fmt.Sprintf("%s %d", prefix, h.texts)
		if h.texts%3 == 0 { // marked for everyone: level addressed lets it in
			t = "@everyone " + t
		}
		if variant > 1 {
			t += fmt.Sprintf(" (%d)", variant)
		}
		return t
	}
	if o.agent != "" && !h.registered(o.machine, o.agent) {
		return // the model's agent exists; the implementation's does not yet
	}
	switch o.kind {
	case opSend, opReply:
		body := text("text")
		msg := delivery.Msg{Body: body, AskHandled: o.ask}
		d := Draft{Room: diffRoom, Agent: o.agent, Text: body, AskHandled: o.ask}
		if o.to != "" {
			msg.To = []delivery.Addr{{Machine: o.machine, Agent: o.to}}
			d.To = []core.Addressee{{Node: s.m.ID, Agent: o.to}}
		}
		if o.kind == opReply {
			mid, iid, ok := h.target(o.target)
			if !ok {
				return
			}
			msg.Parent, msg.Everyone = mid, o.everyone
			d.ReplyTo, d.Everyone = iid, o.everyone
		}
		mid, merr := h.w.Send(o.machine, o.agent, msg)
		sent, ierr := s.m.Send(ctx, d)
		h.wrote(o.machine, mid, merr, sent, ierr)
	case opReplace, opWithdraw, opHandled:
		mid, iid, ok := h.target(o.target)
		if !ok {
			return
		}
		var rid delivery.RecordID
		var merr, ierr error
		var sent Sent
		switch o.kind {
		case opReplace:
			body := text("new text")
			want := int64(1) // one above the highest version held here, text or not
			for _, x := range h.all(s) {
				if f := x.Record.Fields(); f.Kind == core.KindReplace && f.Ref == iid && x.Record.Author() == s.m.ID && f.Version >= want {
					want = f.Version + 1
				}
			}
			rid, merr = h.w.Replace(o.machine, o.agent, mid, body)
			sent, ierr = s.m.Replace(ctx, diffRoom, o.agent, iid, body)
			if ierr == nil {
				if v := h.held(s, sent.ID).Record.Fields().Version; v != want {
					h.fail("replace version %d, want %d", v, want)
				}
			}
		case opWithdraw:
			rid, merr = h.w.Withdraw(o.machine, o.agent, mid)
			sent, ierr = s.m.Withdraw(ctx, diffRoom, o.agent, iid)
		case opHandled:
			rid, merr = h.w.Handle(o.machine, o.agent, mid)
			sent, ierr = s.m.Handled(ctx, diffRoom, o.agent, iid)
		}
		h.wrote(o.machine, rid, merr, sent, ierr)
	case opShow:
		mv, iv := h.list(o.machine, o.agent)
		h.shown[agentKey{o.machine, o.agent}] = listing{mv.Entries, iv.Entries}
	case opRead:
		h.read(o)
	case opSeenPart, opSeenAll:
		h.seen(o)
	case opCrash:
		h.w.Crash(o.machine)
		h.reopen(s, "")
	case opBackup:
		b := backup{machine: o.machine, model: h.w.Backup(o.machine), path: filepath.Join(h.t.TempDir(), "backup.db"),
			reg: cloneMap(h.reg[o.machine]), isReg: h.registered(o.machine, lateAgent)}
		copyDB(h.t, s.path, b.path)
		if h.px != nil {
			b.px = h.px.backup(o.machine)
		}
		h.backups = append(h.backups, b)
	case opRestore:
		var b *backup
		for i := len(h.backups) - 1; i >= 0; i-- {
			if h.backups[i].machine == o.machine {
				b = &h.backups[i]
				break
			}
		}
		if b == nil {
			return
		}
		h.w.Restore(o.machine, b.model)
		if h.px != nil {
			h.px.restore(o.machine, b.px)
		}
		// What agents were shown before the restore stays shown: a Seen
		// after it confirms entries the restored store may not hold.
		h.reopen(s, b.path)
		s.agents = slices.DeleteFunc(s.agents, func(a string) bool { return a == lateAgent })
		delete(h.reg, o.machine)
		if b.isReg {
			s.agents = append(s.agents, lateAgent)
			h.reg[o.machine] = cloneMap(b.reg)
		}
	case opAdvance:
		h.advance(delivery.Time(o.n))
	case opSkew:
		// The hour this step took passed under the old clock: the
		// daemon's hourly pass ran in it, before the clock was changed.
		h.prune(s)
		h.w.SetSkew(o.machine, delivery.Time(o.n))
		if h.px != nil {
			h.px.skews[o.machine] = append(h.px.skews[o.machine], skewChange{h.w.Now, delivery.Time(o.n)})
		}
	case opRegister:
		h.register(s)
	case opPull:
		if h.px != nil {
			h.px.pull(h, o)
		} else {
			h.pull(o.machine, o.server)
		}
	case opInject:
		h.inject(s, o)
	case opLevel:
		l := delivery.Level(o.n)
		merr := h.w.SetLevel(o.machine, o.agent, l)
		ierr := s.m.SetLevel(ctx, diffRoom, o.agent, l.String())
		if a, b := errClass(merr), errClass(ierr); a != b {
			h.fail("level: model %s, implementation %s", a, b)
		}
	default:
		h.px.do(h, o)
	}
}

// list is the inbox as an agent reads it on both sides: listed, compared,
// and a receipt kept of each entry (the model's List, Machine.Showed).
func (h *harness) list(m delivery.MachineID, agent string) (delivery.View, InboxView) {
	h.t.Helper()
	mv, iv := h.inbox(m, agent)
	if err := h.w.Listed(m, agent, mv.Entries); err != nil {
		h.fail("model listed: %v", err)
	}
	if err := h.sims[m].m.Showed(context.Background(), diffRoom, agent, iv.Entries, true); err != nil {
		h.fail("implementation showed: %v", err)
	}
	return mv, iv
}

// seen confirms what was shown (or lists and confirms) on both sides. Each
// side accepts an entry only if a listing showed it with that key and
// version and the inbox shows it so now; both must accept the same.
func (h *harness) seen(o op) {
	h.t.Helper()
	k := agentKey{o.machine, o.agent}
	l, ok := h.shown[k]
	if o.kind == opSeenAll || !ok {
		mv, iv := h.list(o.machine, o.agent)
		l = listing{mv.Entries, iv.Entries}
	}
	delete(h.shown, k)
	n := len(l.model)
	if o.kind == opSeenPart && o.n < n {
		n = o.n
	}
	var reads []store.Read
	for i := 0; i < n; i++ {
		reads = append(reads, store.Read{Key: l.impl[i].Key, Version: l.impl[i].Version})
	}
	res, ierr := h.sims[o.machine].m.Seen(context.Background(), diffRoom, o.agent, reads)
	if ierr != nil {
		h.fail("implementation seen: %v", ierr)
	}
	want, err := h.w.Confirm(o.machine, o.agent, l.model[:n])
	if err != nil {
		h.fail("model seen: %v", err)
	}
	for i := range want {
		if res[i].Confirmed != want[i] {
			h.fail("seen %s: implementation confirmed=%v (%s), the model %v",
				h.key(reads[i].Key), res[i].Confirmed, res[i].Reason, want[i])
		}
	}
}

// read is a read other than the inbox on both sides (o.n: 0 the thread or
// branch view of the target, as the thread tool picks it; 1 skipped): the
// views are compared, and what they showed is recorded, so that the agent
// may report a message whose text it was shown handled.
func (h *harness) read(o op) {
	h.t.Helper()
	ctx := context.Background()
	s := h.sims[o.machine]
	var me []delivery.Entry
	var ie []Entry
	var err error
	what := "skipped"
	if o.n == 0 {
		mid, iid, ok := h.target(o.target)
		if !ok {
			return
		}
		what = "thread view"
		var root string
		if err := s.st.View(ctx, func(rd *store.Reader) error {
			x, ok, err := rd.Record(diffRoom, iid)
			if ok {
				root = x.Record.Fields().Thread
			}
			return err
		}); err != nil {
			h.t.Fatal(err)
		}
		if root == "" { // a root, or not held (lost to a restore)
			me = h.w.Thread(o.machine, mid)
			ie, err = s.m.Thread(ctx, diffRoom, iid)
		} else {
			me = h.w.Branch(o.machine, mid)
			ie, err = s.m.Branch(ctx, diffRoom, iid)
		}
	} else {
		me = slices.DeleteFunc(h.w.Skipped(o.machine, o.agent), func(e delivery.Entry) bool { return h.before(o.machine, o.agent, e.Pos) })
		ie, err = s.m.Skipped(ctx, diffRoom, o.agent)
	}
	if err != nil {
		h.fail("implementation %s: %v", what, err)
	}
	h.sameEntries(what+" read by "+string(o.machine)+"/"+o.agent, me, ie)
	if err := h.w.Showed(o.machine, o.agent, me); err != nil {
		h.fail("model showed: %v", err)
	}
	if err := s.m.Showed(ctx, diffRoom, o.agent, ie, false); err != nil {
		h.fail("implementation showed: %v", err)
	}
}

// register registers lateAgent on s. Its time begins at the current end,
// which the model does not have: the model's listings are compared without
// the positions before. Only in schedules without missing text, where the
// model's walk passes those positions as the implementation's cursor does.
func (h *harness) register(s *sim) {
	h.t.Helper()
	if h.registered(s.name, lateAgent) {
		return
	}
	if _, err := s.m.Register(context.Background(), lateAgent); err != nil {
		h.fail("register: %v", err)
	}
	s.agents = append(s.agents, lateAgent)
	start := map[streamKey]int64{}
	for _, st := range h.streams(s) {
		if st.Contig > 0 {
			start[streamKey{st.Author, st.Epoch}] = st.Contig
		}
	}
	got := map[streamKey]int64{}
	err := s.st.View(context.Background(), func(rd *store.Reader) error {
		cs, err := rd.Cursors(lateAgent, diffRoom)
		for _, c := range cs {
			if c.Start != c.Pos {
				return fmt.Errorf("cursor %+v", c)
			}
			got[streamKey{c.Author, c.Epoch}] = c.Pos
		}
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(start) {
		h.fail("registered at %v, want the current end %v", got, start)
	}
	h.reg[s.name] = start
}

// pull is the model's Pull and, on the implementation side, what step 5's
// pull will do with a page: the server's text for records the puller holds
// without it, then every record past the puller's positions, each placed by
// PutRecord with its text when the server holds it, until nothing more can
// be placed; each side notes what the other said it held.
func (h *harness) pull(puller, server delivery.MachineID) {
	h.t.Helper()
	if puller == server {
		return
	}
	if _, err := h.w.Pull(puller, server, delivery.PullOpt{}); err != nil {
		h.fail("model pull: %v", err)
	}
	ctx := context.Background()
	p, s := h.sims[puller], h.sims[server]
	at := diffBase.Add(time.Duration(h.w.Now) * time.Hour)
	note := func(to, from *sim) {
		// What from reports, author by author: a known position is a hint
		// the same peer's later report lowers (Tx.NoteKnown).
		known := map[string]map[string]int64{}
		for _, st := range append(h.streams(from), h.streams(to)...) {
			if known[st.Author] == nil {
				known[st.Author] = map[string]int64{}
			}
		}
		for _, st := range h.streams(from) {
			// Injected streams are not reported: the model has no gap for them.
			if st.Contig > 0 && !strings.HasPrefix(st.Epoch, "ff") {
				known[st.Author][st.Epoch] = st.Contig
			}
		}
		if _, err := to.st.Exchange(ctx, diffRoom, 0, func(tx *store.Tx) error {
			for a, k := range known {
				if err := tx.NoteKnown(diffRoom, from.m.ID, a, k, at); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			h.t.Fatal(err)
		}
	}
	note(s, p) // the server learns from the request
	srv := h.all(s)
	put := func(x store.Held) store.Outcome {
		r := x.Record
		if x.Reason != store.ReasonLive {
			r = r.WithoutBody()
		}
		v, err := r.Verified()
		if err != nil {
			h.t.Fatal(err)
		}
		out, err := p.st.PutRecord(ctx, v, store.PutOpts{Now: p.m.now(), MayRead: true})
		if err != nil {
			h.t.Fatal(err)
		}
		if out == store.OutcomeStored && x.Reason != store.ReasonLive && !store.TextExpired(v.Fields().Created, p.m.now()) {
			if k := v.Fields().Kind; k == core.KindMessage || k == core.KindReplace {
				h.c.missing++ // held without text, unless a withdraw is held
			}
		}
		return out
	}
	for _, x := range h.all(p) {
		if x.Reason == store.ReasonMissing {
			if y, ok := srv[x.Record.ID()]; ok && y.Reason == store.ReasonLive {
				if put(y) == store.OutcomeRepaired {
					h.c.repaired++
				}
			}
		}
	}
	byPos := map[streamKey][]store.Held{}
	for _, x := range srv {
		f := x.Record.Fields()
		k := streamKey{x.Record.Author(), f.Epoch}
		byPos[k] = append(byPos[k], x)
	}
	for k := range byPos {
		slices.SortFunc(byPos[k], func(a, b store.Held) int { return int(a.Record.Fields().Seq - b.Record.Fields().Seq) })
	}
	for progress := true; progress; {
		progress = false
		have := map[streamKey]int64{}
		for _, st := range h.streams(p) {
			have[streamKey{st.Author, st.Epoch}] = st.Contig
		}
		for k, xs := range byPos {
			for _, x := range xs {
				if x.Record.Fields().Seq <= have[k] {
					continue
				}
				if put(x) != store.OutcomeStored {
					break
				}
				progress = true
			}
		}
	}
	note(p, s) // the puller learns from the response
	h.prune(p)
}

func (h *harness) streams(s *sim) []store.Stream {
	var ss []store.Stream
	if err := s.st.View(context.Background(), func(rd *store.Reader) (err error) {
		ss, err = rd.Streams(diffRoom)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	return ss
}

// all returns every record s holds, by id, with its reason as a reader
// sees it now.
func (h *harness) all(s *sim) map[string]store.Held {
	out := map[string]store.Held{}
	now := s.m.now()
	err := s.st.View(context.Background(), func(rd *store.Reader) error {
		ss, err := rd.Streams(diffRoom)
		if err != nil {
			return err
		}
		und, err := s.m.undecided(rd) // missing text not yet decided does not expire (reading.go)
		if err != nil {
			return err
		}
		for _, st := range ss {
			hs, err := rd.RecordsFrom(diffRoom, st.Author, st.Epoch, 1)
			if err != nil {
				return err
			}
			for _, x := range hs {
				if (x.Reason == store.ReasonLive || x.Reason == store.ReasonMissing && !und[[2]string{diffRoom, st.Author}]) &&
					store.TextExpired(x.Record.Fields().Created, now) {
					x.Reason = store.ReasonExpired
				}
				out[x.Record.ID()] = x
			}
		}
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

// inject places on s a signed record whose reference does not check out,
// in a new stream: by s's own key, or by a key the model never heard of.
// The model has no such record; every view must be as if it were absent.
func (h *harness) inject(s *sim, o op) {
	h.t.Helper()
	ctx := context.Background()
	f := core.RecordFields{Room: diffRoom, Seq: 1, Agent: "zed", Created: s.m.now()}
	key := s.key
	_, msg, okMsg := h.target(o.target)
	rep, okRep := pick(h.reps, o.target)
	switch o.n {
	case injBadHandled:
		if !okMsg {
			return
		}
		t, ok := h.all(s)[msg]
		if !ok {
			return
		}
		// An agent here the message is for but who may not report it, so a
		// report that counted would hide the message from it.
		f.Agent = ""
		for _, a := range s.agents {
			x := h.keepOne(t)
			if relevant(x, s.m.ID, a) && !store.CanHandle(t.Record, s.m.ID, a) {
				f.Agent = a
				break
			}
		}
		if f.Agent == "" {
			return
		}
		f.Kind, f.Ref = core.KindHandled, msg
	case injBadThread:
		if !okMsg {
			return
		}
		t, ok := h.all(s)[msg]
		if !ok {
			return
		}
		right := t.Record.Fields().Thread
		if right == "" {
			right = msg
		}
		for _, m := range h.msgs {
			if id := h.pair[m]; id != right {
				f.Thread = id
			}
		}
		if f.Thread == "" {
			return
		}
		// A reply for nobody here, so only the thread and branch views
		// could show it.
		f.Kind, f.Parent, f.Text = core.KindMessage, msg, "wrong thread"
		f.To = []core.Addressee{{Node: s.m.ID, Agent: "zed"}}
	case injWithdrawOther:
		if !okMsg {
			return
		}
		key, f.Kind, f.Ref = h.foreign, core.KindWithdraw, msg
	case injWithdrawReplace:
		if !okRep {
			return
		}
		f.Kind, f.Ref = core.KindWithdraw, rep
	case injReplaceReplace:
		if !okRep {
			return
		}
		f.Kind, f.Ref, f.Version, f.Text = core.KindReplace, rep, 1, "never shown"
	case injHandledReplace:
		if !okRep {
			return
		}
		f.Kind, f.Ref = core.KindHandled, rep
	}
	h.c.injected++
	f.Epoch = fmt.Sprintf("ff%014x", h.c.injected)
	r, err := core.NewRecord(key, f)
	if err != nil {
		h.t.Fatal(err)
	}
	v, err := r.Verified()
	if err != nil {
		h.t.Fatal(err)
	}
	out, err := s.st.PutRecord(ctx, v, store.PutOpts{Now: s.m.now(), MayRead: true})
	if err != nil || out != store.OutcomeStored {
		h.fail("inject: %s %v", out, err)
	}
	h.injected[v.ID()] = true
	h.label[v.ID()] = "inj"
}

// advance moves the clock. The model's awake machines drop expired text as
// time passes; the daemon's hourly pruning pass is its counterpart, and it
// has surely run in a jump of more than an hour. Within one hour, and after
// a clock change, the implementation decides expiry at read time and
// prunes when a read asks for it (compare runs Prune when PruneDue).
func (h *harness) advance(d delivery.Time) {
	h.w.Advance(d)
	if d > delivery.Hour {
		for _, n := range h.order {
			if h.px == nil || !h.px.asleep[n] {
				h.prune(h.sims[n])
			}
		}
	}
}

func (h *harness) keepOne(x store.Held) *rec {
	return &rec{h: x, f: x.Record.Fields(), id: x.Record.ID(), author: x.Record.Author(), reason: x.Reason}
}

func (h *harness) prune(s *sim) {
	if err := s.m.Prune(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

// reopen stops the daemon, optionally puts a backup in place, and starts it
// again over the same path; the daemon prunes at start.
func (h *harness) reopen(s *sim, from string) {
	if err := s.st.Close(); err != nil {
		h.t.Fatal(err)
	}
	if from != "" {
		copyDB(h.t, from, s.path)
	}
	st, err := store.Open(s.path)
	if err != nil {
		h.t.Fatal(err)
	}
	s.st = st
	h.start(s, nil)
	h.prune(s)
}

// ---- comparison ----

func epochHex(e uint64) string { return fmt.Sprintf("%016x", e) }

// before reports whether model entry e is at a position before lateAgent's
// time began on m.
func (h *harness) before(m delivery.MachineID, agent string, p delivery.Pos) bool {
	if agent != lateAgent {
		return false
	}
	start := h.reg[m]
	return int64(p.Seq) <= start[streamKey{h.sims[p.Stream.Author].m.ID, epochHex(p.Stream.Epoch)}]
}

func (h *harness) inbox(m delivery.MachineID, agent string) (delivery.View, InboxView) {
	h.t.Helper()
	mv, err := h.w.Inbox(m, agent)
	if err != nil {
		h.fail("model inbox: %v", err)
	}
	mv.Entries = slices.DeleteFunc(slices.Clone(mv.Entries), func(e delivery.Entry) bool { return h.before(m, agent, e.Pos) })
	iv, err := h.sims[m].m.Inbox(context.Background(), diffRoom, agent)
	if err != nil {
		h.fail("implementation inbox %s/%s: %v", m, agent, err)
	}
	h.sameEntries("inbox "+string(m)+"/"+agent, mv.Entries, iv.Entries)
	var mw, iw []string
	for _, g := range mv.Waiting {
		reason, at, since := WaitGap, "", ""
		switch {
		case g.Link:
			reason = WaitLink
		case g.Text:
			reason = WaitText
			at = fmt.Sprintf("%s.%s.%d", g.At.Stream.Author, epochHex(g.At.Stream.Epoch), g.At.Seq)
		default:
			since = fmt.Sprint(int64(g.Since + h.skewAt(m, g.Since)))
		}
		mw = append(mw, fmt.Sprintf("%s.%s have=%d known=%d %s %s %s", g.Stream.Author, epochHex(g.Stream.Epoch), g.Have, g.Known, reason, at, since))
	}
	for _, g := range iv.Waiting {
		at, since := "", ""
		if g.Reason == WaitText {
			x := h.held(h.sims[m], g.At)
			f := x.Record.Fields()
			at = fmt.Sprintf("%s.%s.%d", h.nodes[x.Record.Author()], f.Epoch, f.Seq)
		}
		if g.Reason == WaitGap {
			since = fmt.Sprint(int64(g.Since.Sub(diffBase) / time.Hour))
		}
		iw = append(iw, fmt.Sprintf("%s.%s have=%d known=%d %s %s %s", h.nodes[g.Author], g.Epoch, g.Have, g.Known, g.Reason, at, since))
	}
	sort.Strings(mw)
	sort.Strings(iw)
	if h.px != nil && h.px.gapsUnknown[m] {
		mw = h.px.knownGaps(h, mw, iw)
	}
	if !slices.Equal(mw, iw) {
		h.fail("waiting %s/%s:\nmodel %v\nimplementation %v", m, agent, mw, iw)
	}
	return mv, iv
}

func (h *harness) id(s string) string {
	if s == "" {
		return ""
	}
	if l, ok := h.label[s]; ok {
		return l
	}
	return "unknown:" + s
}

func (h *harness) key(k string) string {
	if strings.HasPrefix(k, "x:") {
		return "x:" + h.id(k[2:])
	}
	return h.id(k)
}

var modelKind = map[delivery.EntryKind]string{
	delivery.Shown: EntryMessage, delivery.WithdrawnNotice: EntryWithdrawn, delivery.ReplacedNotice: EntryReplaced,
	delivery.ExpiredNotice: EntryExpired, delivery.VoidedNotice: EntryVoided,
}

var modelParent = map[delivery.ParentState]string{
	delivery.NoParent: "", delivery.ParentHere: ParentHere, delivery.ParentMissing: ParentMissing,
	delivery.ParentExpired: ParentExpired, delivery.ParentWithdrawn: ParentWithdrawn,
}

func (h *harness) modelEntry(e delivery.Entry) string {
	return fmt.Sprintf("%s key=%s rec=%s msg=%s from=%s text=%q ver=%s parent=%s thread=%s ps=%s created=%d late=%v removed=%v at=%s.%s.%d",
		modelKind[e.Kind], h.key(e.Key), h.id(string(e.Record)), h.id(string(e.Message)), e.From,
		e.Text, h.id(string(e.Version)), h.id(string(e.Parent)), h.id(string(e.Thread)), modelParent[e.ParentIs],
		e.Created, e.Late, e.AuthorRemoved, e.Pos.Stream.Author, epochHex(e.Pos.Stream.Epoch), e.Pos.Seq)
}

func (h *harness) implEntry(e Entry) string {
	d := e.Created.Sub(diffBase)
	created := fmt.Sprintf("%dh", d/time.Hour)
	if d%time.Hour == 0 {
		created = fmt.Sprint(int64(d / time.Hour))
	}
	agent, _, _ := strings.Cut(e.From, "@")
	return fmt.Sprintf("%s key=%s rec=%s msg=%s from=%s/%s text=%q ver=%s parent=%s thread=%s ps=%s created=%s late=%v removed=%v at=%s.%s.%d",
		e.Kind, h.key(e.Key), h.id(e.Record), h.id(e.ID), h.nodes[e.FromNode], agent,
		e.Text, h.id(e.Version), h.id(e.Parent), h.id(e.Thread), e.ParentState,
		created, e.Late, e.AuthorRemoved, h.nodes[e.FromNode], e.Epoch, e.Seq)
}

func (h *harness) sameEntries(what string, ms []delivery.Entry, is []Entry) {
	h.t.Helper()
	var a, b []string
	for _, e := range ms {
		a = append(a, h.modelEntry(e))
	}
	for _, e := range is {
		b = append(b, h.implEntry(e))
	}
	if !slices.Equal(a, b) {
		h.fail("%s differs:\nmodel:\n  %s\nimplementation:\n  %s", what, strings.Join(a, "\n  "), strings.Join(b, "\n  "))
	}
}

var modelReason = map[delivery.Reason]string{
	delivery.Live: store.ReasonLive, delivery.Expired: store.ReasonExpired, delivery.Withdrawn: store.ReasonWithdrawn,
	delivery.NotAdmitted: store.ReasonNotAdmitted, delivery.Missing: store.ReasonMissing,
}

// compare checks, on every machine: every registered agent's inbox, and
// that walking from where its time began gives the same; the thread and
// branch view of every message written; and the durable state, which
// records are held and with what reason.
func (h *harness) compare() {
	h.t.Helper()
	ctx := context.Background()
	for _, n := range h.order {
		s := h.sims[n]
		if h.px != nil && h.px.asleep[n] {
			continue // the model reads nothing of a sleeping machine
		}
		for _, a := range s.agents {
			_, iv := h.inbox(n, a)
			ms := slices.DeleteFunc(h.w.Skipped(n, a), func(e delivery.Entry) bool { return h.before(n, a, e.Pos) })
			is, err := s.m.Skipped(ctx, diffRoom, a)
			if err != nil {
				h.fail("skipped: %v", err)
			}
			h.sameEntries("skipped "+string(n)+"/"+a, ms, is)
			fromStart := h.inboxFromStart(s, a)
			if !slices.Equal(fromStart, h.implListing(iv)) {
				h.fail("%s/%s: walking from the start gives\n  %s\nfrom the cursors\n  %s", n, a,
					strings.Join(fromStart, "\n  "), strings.Join(h.implListing(iv), "\n  "))
			}
		}
		for _, mid := range h.msgs {
			iid := h.pair[mid]
			mt := h.w.Thread(n, mid)
			it, err := s.m.Thread(ctx, diffRoom, iid)
			if err != nil {
				h.fail("thread: %v", err)
			}
			h.sameEntries("thread "+h.id(iid)+" on "+string(n), mt, it)
			mb := h.w.Branch(n, mid)
			ib, err := s.m.Branch(ctx, diffRoom, iid)
			if err != nil {
				h.fail("branch: %v", err)
			}
			h.sameEntries("branch "+h.id(iid)+" on "+string(n), mb, ib)
		}
		held := h.all(s)
		for id := range held {
			if _, ok := h.implPair[id]; !ok && !h.injected[id] {
				h.fail("%s holds %s, which nobody wrote", n, h.id(id))
			}
		}
		for _, x := range h.writes {
			st := h.w.Status(n, x.model)
			got, ok := held[x.impl]
			if st.Saved != ok {
				h.fail("%s: model holds %s %v, implementation %v", n, h.id(x.impl), st.Saved, ok)
			}
			if ok && modelReason[st.Dropped] != got.Reason && !(h.px != nil && h.px.undecided(h, n, x, modelReason[st.Dropped], got.Reason)) {
				h.fail("%s: %s is %s in the model, %s in the implementation", n, h.id(x.impl), modelReason[st.Dropped], got.Reason)
			}
			if ok && h.px != nil {
				h.px.sameCopies(h, n, x, st.Copies)
			}
		}
		if s.m.PruneDue() {
			h.prune(s)
		}
	}
}

func (h *harness) implListing(v InboxView) []string {
	var out []string
	for _, e := range v.Entries {
		out = append(out, h.implEntry(e))
	}
	for _, w := range v.Waiting {
		out = append(out, fmt.Sprintf("wait %+v", w))
	}
	return out
}

// inboxFromStart walks agent's inbox from where each of its cursors
// started instead of from the cursors.
func (h *harness) inboxFromStart(s *sim, agent string) []string {
	var v InboxView
	err := s.m.view(context.Background(), diffRoom, func(rv *roomView) error {
		g, err := rv.agent(agent)
		if err != nil {
			return err
		}
		g.cursor = copyCursor(g.start)
		v.Entries, _, v.Waiting, err = rv.walk(g, agent, false)
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return h.implListing(v)
}

// ---- schedules ----

// alphabet is the exhaustive search's choice of operations on machine M.
func alphabet() []op {
	const m = "M"
	return []op{
		{kind: opSend, machine: m, agent: "alice"},
		{kind: opSend, machine: m, agent: "bob", to: "alice", same: true},
		{kind: opSend, machine: m, agent: "alice", ask: true},
		{kind: opReply, machine: m, agent: "bob", target: -1},
		{kind: opReply, machine: m, agent: "alice", target: 0, everyone: true},
		{kind: opReplace, machine: m, agent: "alice", target: -1},
		{kind: opWithdraw, machine: m, agent: "alice", target: 0},
		{kind: opHandled, machine: m, agent: "bob", target: -1},
		{kind: opShow, machine: m, agent: "bob"},
		{kind: opSeenPart, machine: m, agent: "bob", n: 1},
		{kind: opSeenAll, machine: m, agent: "bob"},
		{kind: opSeenAll, machine: m, agent: lateAgent},
		{kind: opRegister, machine: m},
		{kind: opCrash, machine: m},
		{kind: opBackup, machine: m},
		{kind: opRestore, machine: m},
		{kind: opAdvance, n: 31 * 24},
		{kind: opSkew, machine: m, n: -40 * 24},
		{kind: opInject, machine: m, n: injBadHandled, target: -1},
		{kind: opInject, machine: m, n: injReplaceReplace, target: -1},
		{kind: opInject, machine: m, n: injBadThread, target: 0},
		{kind: opLevel, machine: m, agent: "bob", n: int(delivery.Addressed)},
		{kind: opLevel, machine: m, agent: "bob", n: int(delivery.All)},
		{kind: opRead, machine: m, agent: "bob", target: -1},
	}
}

// exhaust runs every sequence of depth operations from h, cloning the
// world at each step. It returns the number of states checked.
func exhaust(h *harness, ops []op, depth int) int {
	if depth == 0 {
		return 1
	}
	n := 1
	for _, o := range ops {
		c := h.clone().apply(o)
		if !c.stopped {
			n += exhaust(c, ops, depth-1)
		}
		c.close()
	}
	return n
}

// levelOp draws, now and then, a level change by one of agents on m. It
// has its own source, so that the other operations of a seeded schedule
// stay what they were before levels existed.
func levelOp(r *rand.Rand, m delivery.MachineID, agents []string) (op, bool) {
	if r.IntN(12) != 0 {
		return op{}, false
	}
	return op{kind: opLevel, machine: m, agent: agents[r.IntN(len(agents))], n: r.IntN(3), same: true}, true
}

// readOp draws, now and then, a thread or skipped read by one of agents on
// m. Like levelOp it has its own source, so that the other operations of a
// seeded schedule stay what they were.
func readOp(r *rand.Rand, m delivery.MachineID, agents []string) (op, bool) {
	if r.IntN(8) != 0 {
		return op{}, false
	}
	return op{kind: opRead, machine: m, agent: agents[r.IntN(len(agents))], n: r.IntN(2), target: r.IntN(6) - 3, same: true}, true
}

// randomOp draws one operation over machine m with agents; peers are the
// other machines, for pulls.
func randomOp(r *rand.Rand, m delivery.MachineID, agents []string, peers []delivery.MachineID, skews []int) op {
	a := agents[r.IntN(len(agents))]
	other := agents[r.IntN(len(agents))]
	target := r.IntN(6) - 3 // the last three or the first three
	same := r.IntN(6) == 0
	switch x := r.IntN(100); {
	case x < 14:
		o := op{kind: opSend, machine: m, agent: a, same: same}
		switch r.IntN(3) {
		case 0:
			o.to = other
		case 1:
			o.ask = true
		}
		return o
	case x < 24:
		o := op{kind: opReply, machine: m, agent: a, target: target, everyone: r.IntN(3) == 0, same: same}
		o.ask = o.everyone && r.IntN(2) == 0
		return o
	case x < 31:
		return op{kind: opReplace, machine: m, agent: a, target: target, same: same}
	case x < 36:
		return op{kind: opWithdraw, machine: m, agent: a, target: target, same: same}
	case x < 44:
		return op{kind: opHandled, machine: m, agent: a, target: target, same: same}
	case x < 50:
		return op{kind: opShow, machine: m, agent: a}
	case x < 56:
		return op{kind: opSeenPart, machine: m, agent: a, n: r.IntN(3)}
	case x < 66:
		return op{kind: opSeenAll, machine: m, agent: a, same: same}
	case x < 69:
		return op{kind: opCrash, machine: m}
	case x < 72:
		return op{kind: opBackup, machine: m}
	case x < 75:
		return op{kind: opRestore, machine: m}
	case x < 79:
		if len(peers) > 0 { // near 30 days, where clocks disagree on expiry
			return op{kind: opAdvance, n: []int{1, 6, 29*24 + 12, 30*24 - 6}[r.IntN(4)]}
		}
		return op{kind: opAdvance, n: []int{1, 5 * 24, 29 * 24, 30*24 - 3, 31 * 24}[r.IntN(5)]}
	case x < 82:
		return op{kind: opSkew, machine: m, n: skews[r.IntN(len(skews))]}
	case x < 86:
		return op{kind: opInject, machine: m, n: r.IntN(injKinds), target: target}
	case x < 89 && len(peers) == 0:
		return op{kind: opRegister, machine: m}
	}
	if len(peers) == 0 {
		return op{kind: opSeenAll, machine: m, agent: a}
	}
	return op{kind: opPull, machine: m, server: peers[r.IntN(len(peers))]}
}

// [R2, R4, R6, R7] Every sequence of three operations (two under -race)
// from a short prefix, over one machine: sends (some in the same hour as
// the step before), replies, replaces, withdraws, handled reports, shows,
// partial and full Seen, registering an agent, crashes, backups and
// restores, a 31-day jump, a clock set back 40 days, and references that
// do not check out. Every view and the durable state agree with the model
// after every step.
func TestImplementationAgreesWithTheDeliveryModelOnEveryShortSchedule(t *testing.T) {
	if testing.Short() {
		t.Skip("exhaustive differential schedules")
	}
	start := time.Now()
	h := newHarness(t, map[delivery.MachineID][]string{"M": {"alice", "bob", lateAgent}})
	for _, o := range []op{
		{kind: opSend, machine: "M", agent: "alice", to: "bob"},
		{kind: opBackup, machine: "M"},
	} {
		h = h.apply(o)
	}
	defer h.close()
	depth := 3
	if raceDetector {
		depth = 2
	}
	n := exhaust(h, alphabet(), depth)
	t.Logf("depth %d over %d operations: %d states checked in %v; %d writes retried for id order, %d schedules stopped",
		depth, len(alphabet()), n, time.Since(start).Round(time.Millisecond), h.c.retries, h.c.stopped)
	if h.c.stopped > 0 {
		t.Errorf("%d schedules stopped at an id-order tie", h.c.stopped)
	}
}

// [R2, R4, R6, R7] Random schedules with fixed seeds: over one machine with
// four agents (one registered mid-schedule) and a clock set back 40 days;
// and over three machines that pull from each other, with clocks up to 10
// hours apart (within the day a record may be dated ahead), so text expires
// on one machine before another and is repaired from a third. Every view
// and the durable state agree with the model after every step.
func TestImplementationAgreesWithTheDeliveryModelOnRandomSchedules(t *testing.T) {
	start := time.Now()
	runs, steps := 60, 40
	if raceDetector {
		runs = 6
	}
	if testing.Short() {
		runs = 3
	}
	r := rand.New(rand.NewPCG(20261123, 3))
	lr := rand.New(rand.NewPCG(20261124, 11))
	rr := rand.New(rand.NewPCG(20261124, 17))
	c := &counts{}
	total := 0
	for i := 0; i < runs; i++ {
		agents := []string{"alice", "bob", "carol", lateAgent}
		h := newHarness(t, map[delivery.MachineID][]string{"M": agents})
		h.c = c
		for j := 0; j < steps && !h.stopped; j++ {
			h = h.apply(randomOp(r, "M", agents, nil, []int{0, 0, -40 * 24, 2 * 24}))
			total++
			if o, ok := levelOp(lr, "M", agents); ok {
				h = h.apply(o)
			}
			if o, ok := readOp(rr, "M", agents); ok {
				h = h.apply(o)
			}
		}
		h.close()
	}
	single := time.Since(start)
	multiStart := time.Now()
	machines := map[delivery.MachineID][]string{"M": {"alice", "bob"}, "P": {"pat"}, "Q": {"quinn"}}
	names := []delivery.MachineID{"M", "P", "Q"}
	for i := 0; i < runs; i++ {
		h := newHarness(t, machines)
		h.c = c
		for j := 0; j < steps && !h.stopped; j++ {
			m := names[r.IntN(len(names))]
			peers := slices.DeleteFunc(slices.Clone(names), func(x delivery.MachineID) bool { return x == m })
			h = h.apply(randomOp(r, m, machines[m], peers, []int{0, -10, 10}))
			total++
			if o, ok := levelOp(lr, m, machines[m]); ok {
				h = h.apply(o)
			}
			if o, ok := readOp(rr, m, machines[m]); ok {
				h = h.apply(o)
			}
		}
		h.close()
	}
	t.Logf("%d single-machine and %d three-machine runs, %d steps: %v and %v; %d writes retried for id order, %d runs stopped; %d records injected, %d headers placed without text, %d texts repaired",
		runs, runs, total, single.Round(time.Millisecond), time.Since(multiStart).Round(time.Millisecond),
		c.retries, c.stopped, c.injected, c.missing, c.repaired)
	if c.stopped > 0 {
		t.Errorf("%d runs stopped at an id-order tie", c.stopped)
	}
}

// [R6, R4] Body repair, scripted: text that expired on the machine a
// header came from, but not yet by the puller's slower clock, is held
// missing (the inbox waits for it) and is repaired from a third machine
// that still has it; meanwhile a withdraw naming a replace is placed
// before the replace arrives and becomes a bad reference when it does.
func TestImplementationAgreesWithTheDeliveryModelOnBodyRepairAcrossMachines(t *testing.T) {
	h := newHarness(t, map[delivery.MachineID][]string{"M": {"alice"}, "P": {"pat"}, "Q": {"quinn"}})
	defer h.close()
	for _, o := range []op{
		{kind: opSkew, machine: "P", n: -10},
		{kind: opSkew, machine: "Q", n: -10},
		{kind: opSend, machine: "M", agent: "alice"},
		{kind: opReplace, machine: "M", agent: "alice", target: -1},
		{kind: opPull, machine: "P", server: "M"},
		{kind: opInject, machine: "Q", n: injWithdrawReplace, target: -1},
		{kind: opAdvance, n: 30 * 24},
		{kind: opPull, machine: "Q", server: "M"},
		{kind: opShow, machine: "Q", agent: "quinn"},
		{kind: opPull, machine: "Q", server: "P"},
		{kind: opSeenAll, machine: "Q", agent: "quinn"},
		{kind: opAdvance, n: 12},
		{kind: opSeenAll, machine: "Q", agent: "quinn"},
	} {
		h = h.apply(o)
	}
	if h.c.repaired == 0 {
		t.Fatal("no text was repaired")
	}
	var waited bool
	for _, l := range h.log {
		waited = waited || strings.Contains(l, "Q/quinn show")
	}
	if !waited {
		t.Fatal("schedule did not run")
	}
}
