package node

// Rooms is the room log and membership on this machine (DESIGN
// sections 3, 5 and 7): one members.Machine per room, rebuilt on start by
// replaying the stored log in arrival batches, fed by one ingress function
// (Receive). Joining (Joiner) and pull (Exchange) both call Receive.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/members"
	"github.com/wallter/a2p2/internal/store"
)

// Errors of the log operations.
var (
	ErrNoRoom = &UserError{"room not held here", ErrorNotFound}
	// ErrStaleGeneration: the room was left (or left and joined again)
	// since the caller looked; whatever it prepared is not applied.
	ErrStaleGeneration = store.ErrStaleGeneration
)

// Reasons an entry is invalid at ingress, before the membership machine
// (PROTOCOL section 3: S1 to S7 and V1 give core's codes).
const (
	CodeWrongRoom    = "wrong_room"    // R1
	CodeSecondCreate = "second_create" // C1
	CodeBadSignature = "bad_signature" // G1
	// ReasonKnownInvalid: the machine found the entry invalid before but
	// it was over the bound kept, so only the fact is known.
	ReasonKnownInvalid = "it was found invalid before"
	// ReasonOverLimit: the entry was not taken, because the room already
	// keeps as many entries of its kind as the bounds allow.
	ReasonOverLimit = "the room keeps as many invalid or refused entries as it allows; this one was not taken"
)

// Bounds on what a room keeps beyond its effective log. Fork evidence (an
// entry refused because its author signed another at its position) is
// bounded by members.Machine (8 per position) and is not counted here.
const (
	// MaxRefusedDependants: entries refused because an entry they had
	// seen was refused as a fork.
	MaxRefusedDependants = 256
	// MaxInvalidKept: entries the membership machine found invalid (bad
	// chain, invalid parent), kept so a replay judges their dependants
	// as before. Beyond it a further invalid entry is not taken at all.
	MaxInvalidKept = 256
	// MaxPendingRequests: requests waiting for a person, per room.
	MaxPendingRequests = 32
)

// EntryResult is what ingress did with one received entry. ID is empty
// when the bytes did not parse.
type EntryResult struct {
	ID          string
	Disposition members.Disposition
	Reason      string
}

// testHook, set by a test, fails a step of ingress at a named point.
var testHook func(point string) error

func hookAt(point string) error {
	if testHook != nil {
		return testHook(point)
	}
	return nil
}

// Rooms holds the membership of every room with a log on this machine.
type Rooms struct {
	m *Machine
	// mu serialises every change to membership, every decision on a
	// request and every record placed through ReceiveRecord, so that what
	// a room's machine decided and what the store holds never diverge.
	mu    sync.Mutex
	rooms map[string]*roomLog
	// fmu guards fronts, each room's reading frontier for Machine.fronts.
	// It is taken alone, inside store transactions too, never with mu
	// waited for.
	fmu    sync.Mutex
	fronts map[string]readFront
	// smu guards states, each held room's membership as its machine last
	// decided it, for State. It is a leaf like fmu: taken alone, under mu
	// (where states changes) or under the store's file lock (a gate's
	// prepare reads State), and nothing is waited for while it is held.
	// So State never waits for mu, which is what keeps the file lock
	// from ever waiting for mu (K4; the lock order is in store.Store).
	smu    sync.Mutex
	states map[string]members.State
	// pubs, under smu, is per room a count raised when a commit that may
	// change the room's membership begins and again once its state is
	// published (odd: in between), so that a reader that pairs the
	// store's change count with State can tell a membership committed
	// but not yet published (L1). Only ingest opens one, and nothing it
	// does inside waits for the store's file lock; so a reader may wait
	// for one to end under that lock.
	pubs map[string]*publication
	// files, set by NewFiles, is the machine's attachment part (nil: none).
	files *Files
	// signs counts membership entries signed (create entries excluded);
	// tests read it.
	signs int
}

// roomLog is one room's membership as this machine holds it. Nothing in it
// changes until the store has committed what changed it.
type roomLog struct {
	id, label, createID string
	gen                 int64
	mach                *members.Machine
	ents                map[string]*core.VerifiedEntry // kept: accepted, held, refused, invalid
	state               map[string]store.LogRow        // state, reason and arrival of each (no bytes)
	st                  members.State
	perm                string // own membership and permission, to see it change
	cache               map[string]store.RoomMember
	// pulled: since the daemon started, this machine has caught up with
	// the room's log from a current member (an exchange whose advertised
	// heads are all accepted here, or a completed join), or it created
	// the room (DESIGN section 3). See maySign.
	pulled bool
}

// OpenRooms rebuilds the membership of every room with a log from the
// store, replaying each room's log in its arrival batches, and refreshes
// the member cache from the result.
func OpenRooms(ctx context.Context, m *Machine) (*Rooms, error) {
	r := &Rooms{m: m, rooms: map[string]*roomLog{}, fronts: map[string]readFront{}}
	m.fronts = r.frontiers
	m.locked = func(fn func() error) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		return fn()
	}
	lrs, err := m.st.LogRooms(ctx)
	if err != nil {
		return nil, err
	}
	for _, lr := range lrs {
		rl, err := r.load(ctx, lr)
		if err != nil {
			return nil, fmt.Errorf("room %s: %w", lr.ID, err)
		}
		if err := m.st.ReplaceMembers(ctx, rl.id, cacheRows(rl.cache)); err != nil {
			return nil, err
		}
		r.rooms[rl.id] = rl
		r.setState(rl.id, &rl.st)
	}
	return r, nil
}

func (r *Rooms) load(ctx context.Context, lr store.LogRoom) (*roomLog, error) {
	rows, err := r.m.st.Log(ctx, lr.ID)
	if err != nil {
		return nil, err
	}
	rl := &roomLog{id: lr.ID, label: lr.Label, createID: lr.CreateID, gen: lr.Gen,
		ents: map[string]*core.VerifiedEntry{}, state: map[string]store.LogRow{}}
	for _, row := range rows {
		e, err := core.EntryFromParts(row.Header, row.Sig)
		if err != nil {
			return nil, errors.New("stored entry does not parse")
		}
		v, err := e.Verified()
		if err != nil {
			return nil, errors.New("stored entry does not verify")
		}
		row.Header, row.Sig = nil, nil
		rl.ents[v.ID()], rl.state[v.ID()] = v, row
	}
	if err := r.replay(rl); err != nil {
		return nil, err
	}
	return rl, nil
}

// replay rebuilds rl's machine from its kept entries in their arrival
// batches, and the state derived from it. members.Machine is
// deterministic, so the replay decides every kept entry as it was decided
// before; a difference is reported as an error rather than adopted. It
// uses nothing but memory: it cannot be cancelled.
func (r *Rooms) replay(rl *roomLog) error {
	ids := sortedIDs(rl.state)
	sort.SliceStable(ids, func(i, j int) bool {
		a, b := rl.state[ids[i]], rl.state[ids[j]]
		if a.Batch != b.Batch {
			return a.Batch < b.Batch
		}
		return a.Pos < b.Pos
	})
	root := rl.ents[rl.createID]
	if root == nil || rl.state[rl.createID].Batch != 0 {
		return errors.New("log does not start with the pinned create")
	}
	mach, err := members.NewMachine(root)
	if err != nil {
		return err
	}
	final := map[string]members.Result{rl.createID: {ID: rl.createID, Disposition: members.Accepted}}
	var batch []*core.VerifiedEntry
	flush := func() {
		for _, x := range mach.Receive(batch...) {
			final[x.ID] = x
		}
		batch = nil
	}
	cur := int64(0)
	for _, id := range ids {
		row := rl.state[id]
		if id == rl.createID {
			continue
		}
		if row.Batch != cur && len(batch) > 0 {
			flush()
		}
		cur = row.Batch
		batch = append(batch, rl.ents[id])
	}
	if len(batch) > 0 {
		flush()
	}
	for id, row := range rl.state {
		if x := final[id]; string(x.Disposition) != row.State || x.Reason != row.Reason {
			return fmt.Errorf("replay decided entry %s as %s, stored %s", id, x.Disposition, row.State)
		}
	}
	rl.mach = mach
	rl.st = mach.State()
	rl.perm = r.permKey(rl.st)
	rl.cache = r.memberCache(rl.st, rl.lookup(nil))
	r.setFrontier(rl)
	return nil
}

// setFrontier publishes rl's reading frontier for Machine.fronts.
//
// Removed, this machine keeps reading by the grants that admitted it
// (removedPermission): what it held back waiting for a head stays held
// back, and is decided when the head arrives, as for a member.
func (r *Rooms) setFrontier(rl *roomLog) {
	var f readFront
	p, ok := rl.st.Permissions[r.m.ID]
	if !ok && slices.Contains(rl.st.Admitted, r.m.ID) {
		p, ok = r.removedPermission(rl), true
	}
	if ok {
		f = readFront{From: readFrom(p, rl.lookup(nil)), All: p.All, perm: p}
	}
	r.fmu.Lock()
	defer r.fmu.Unlock()
	if len(f.From) == 0 {
		delete(r.fronts, rl.id)
	} else {
		r.fronts[rl.id] = f
	}
}

// removedPermission is the permission of this machine, no longer a
// member, from every applied grant that targets it: the union of their
// heads, the more restrictive (members.permissionOf for one period).
func (r *Rooms) removedPermission(rl *roomLog) members.Permission {
	var grants []string
	for _, id := range sortedIDs(rl.ents) {
		ef := rl.ents[id].Fields()
		if (ef.Kind == core.EntryAdd || ef.Kind == core.EntryAdmin) && ef.Target == r.m.ID && rl.st.Effects[id].Applied {
			grants = append(grants, id)
		}
	}
	p := members.Permission{Grants: grants, All: len(grants) > 0, Closed: len(grants) == 0}
	for _, g := range grants {
		ef := rl.ents[g].Fields()
		if ef.History {
			continue
		}
		p.All = false
		for a, ids := range ef.From {
			if p.From == nil {
				p.From = map[string][]string{}
			}
			for _, id := range ids {
				if !slices.Contains(p.From[a], id) {
					p.From[a] = append(p.From[a], id)
				}
			}
		}
	}
	return p
}

// frontiers returns every room's reading frontier; the maps are replaced,
// never changed, so the caller may keep them.
func (r *Rooms) frontiers() map[string]readFront {
	r.fmu.Lock()
	defer r.fmu.Unlock()
	return maps.Clone(r.fronts)
}

// restore puts rl's machine back to exactly what the store holds, after
// a batch that was not committed changed it. The store, and rl's maps,
// were not changed, so this is a replay from memory.
func (r *Rooms) restore(rl *roomLog) {
	if err := r.replay(rl); err != nil {
		// Only a non-deterministic machine gets here; the next start
		// reports it. Until then the room is not served.
		delete(r.rooms, rl.id)
		r.setState(rl.id, nil)
		return
	}
	r.setState(rl.id, &rl.st)
}

func (rl *roomLog) lookup(fresh map[string]*core.VerifiedEntry) func(string) *core.VerifiedEntry {
	return func(id string) *core.VerifiedEntry {
		if v := rl.ents[id]; v != nil {
			return v
		}
		return fresh[id]
	}
}

func entryRow(v *core.VerifiedEntry) store.LogRow {
	return store.LogRow{ID: v.ID(), Author: v.Author(), Seq: v.Fields().Seq, Header: v.Header(), Sig: v.Sig()}
}

// CreateRoom creates a room with this machine as its only member and
// admin: a new room id (PROTOCOL section 0) and the create entry,
// signed with this machine's key, which is pinned as the room's identity.
// The create entry is the only log entry a daemon signs without a
// confirmed request.
func (r *Rooms) CreateRoom(ctx context.Context, label string) (string, error) {
	var b [16]byte
	r.m.rand(b[:])
	id := "r_" + hex.EncodeToString(b[:])
	e, err := core.NewEntry(r.m.key, core.EntryFields{Room: id, Seq: 1, Kind: core.EntryCreate, Label: label,
		Created: r.m.now()})
	if err != nil {
		return "", userErr("cannot create the room: %v", err)
	}
	v, err := e.Verified()
	if err != nil {
		return "", err
	}
	if err := r.pin(ctx, v); err != nil {
		return "", err
	}
	r.mu.Lock()
	if rl := r.rooms[id]; rl != nil {
		rl.pulled = true // created here in this run: exempt (maySign)
	}
	r.mu.Unlock()
	return id, nil
}

// PinRoom starts holding the log of a room this machine is joining, from
// its create entry as received (the join preview, step 4b), which is
// pinned: any other create entry for the room is refused (C1). The room's
// generation moves on, so nothing prepared under an earlier membership of
// it applies. Entries then arrive through Receive.
func (r *Rooms) PinRoom(ctx context.Context, createWire []byte) (string, error) {
	e, err := core.ParseEntry(createWire)
	if err != nil {
		return "", userErr("bad create entry: %v", err)
	}
	if e.Fields().Kind != core.EntryCreate {
		return "", userErr("not a create entry")
	}
	v, err := e.Verified()
	if err != nil {
		return "", userErr("create entry: %s", CodeBadSignature)
	}
	if err := r.pin(ctx, v); err != nil {
		return "", err
	}
	return v.Fields().Room, nil
}

func (r *Rooms) pin(ctx context.Context, v *core.VerifiedEntry) error {
	_, err := r.pinWith(ctx, v, func(row store.LogRow, ms []store.RoomMember) (int64, error) {
		f := v.Fields()
		return r.m.st.CreateLogRoom(ctx, f.Room, f.Label, row, ms, r.m.now())
	})
	return err
}

// pinJoin pins the room of the join recorded under invite from its create
// entry, in one write that checks the join is still accepted and the
// room's generation is still gen (store.JoinLogRoom), and returns the new
// generation from that write. ErrStaleGeneration: the room was held, left
// or joined since, or the join ended.
func (r *Rooms) pinJoin(ctx context.Context, createWire []byte, invite string, gen int64) (int64, error) {
	e, err := core.ParseEntry(createWire)
	if err != nil || e.Fields().Kind != core.EntryCreate {
		return 0, userErr("bad create entry")
	}
	v, err := e.Verified()
	if err != nil {
		return 0, userErr("create entry: %s", CodeBadSignature)
	}
	return r.pinWith(ctx, v, func(row store.LogRow, ms []store.RoomMember) (int64, error) {
		return r.m.st.JoinLogRoom(ctx, invite, gen, v.Fields().Label, row, ms, r.m.now())
	})
}

func (r *Rooms) pinWith(ctx context.Context, v *core.VerifiedEntry, create func(store.LogRow, []store.RoomMember) (int64, error)) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := v.Fields()
	if r.rooms[f.Room] != nil {
		return 0, codedErr(ErrorAlreadyDone, "room is held here already")
	}
	row := entryRow(v)
	row.State = store.LogAccepted
	rl := &roomLog{id: f.Room, label: f.Label, createID: v.ID(),
		ents: map[string]*core.VerifiedEntry{v.ID(): v}, state: map[string]store.LogRow{}}
	bare := row
	bare.Header, bare.Sig = nil, nil
	rl.state[v.ID()] = bare
	if err := r.replay(rl); err != nil {
		return 0, err
	}
	gen, err := create(row, cacheRows(rl.cache))
	if err != nil {
		return 0, err
	}
	if bare.Arrival, err = r.m.st.LogArrival(ctx, f.Room, v.ID()); err != nil {
		return 0, err
	}
	rl.state[v.ID()] = bare
	rl.gen = gen
	r.rooms[f.Room] = rl
	r.setState(f.Room, &rl.st)
	r.m.wakes.touch(f.Room, roomSet)
	return gen, nil
}

// Gen returns the room's current generation. A caller that prepares
// something to apply later (a pull request, step 5; a join, step 4b)
// records it and passes it back to Receive or ReceiveRecord, or checks it
// with CheckGen; after a leave it no longer matches.
func (r *Rooms) Gen(room string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rl := r.rooms[room]
	if rl == nil {
		return 0, ErrNoRoom
	}
	return rl.gen, nil
}

// CheckGen reports ErrStaleGeneration unless room is held and at
// generation gen. The store repeats the check inside each write.
func (r *Rooms) CheckGen(room string, gen int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rl := r.rooms[room]
	if rl == nil || rl.gen != gen {
		return ErrStaleGeneration
	}
	return nil
}

// Leave stops carrying room (DESIGN section 7): its records, reading
// state, log and requests are deleted here, and the room stays as a local
// tombstone with a new generation, so a late response is not applied.
// Other members still list this machine until an admin removes it.
func (r *Rooms) Leave(ctx context.Context, room string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rl := r.rooms[room]
	if rl == nil {
		return ErrNoRoom
	}
	// The room is gone for State before the commit, so no decision is
	// taken against membership the store no longer has [L1]; LeaveRoom
	// then waits for the file lock with no publication open.
	end := r.beginPublish(room)
	r.setState(room, nil)
	end()
	if _, err := r.m.st.LeaveRoom(ctx, room, r.m.now()); err != nil {
		r.setState(room, &rl.st)
		return err
	}
	delete(r.rooms, room)
	r.fmu.Lock()
	delete(r.fronts, room)
	r.fmu.Unlock()
	r.m.wakes.touch(room, roomSet)
	return nil
}

// Offer returns the room's entries as a member passes them on: accepted
// entries in causal order, then held and refused ones (fork evidence) in
// arrival order, as wire objects. Invalid entries are never passed on.
func (r *Rooms) Offer(room string) ([][]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rl := r.rooms[room]
	if rl == nil {
		return nil, ErrNoRoom
	}
	var out [][]byte
	for _, v := range rl.mach.Offer() {
		out = append(out, v.Wire())
	}
	return out, nil
}

// State returns the room's resolved membership. Open fork evidence is not
// part of it: until a removal settles a fork, the branch accepted first
// stands here [M11], and the evidence counts only as a removal vote [M12].
//
// It reads the copy kept under smu, never waiting for mu, so that it may
// be called with the store's file lock held (a gate's prepare) [K4].
func (r *Rooms) State(room string) (members.State, error) {
	r.smu.Lock()
	defer r.smu.Unlock()
	st, ok := r.states[room]
	if !ok {
		return members.State{}, ErrNoRoom
	}
	return st, nil
}

// setState records room's membership for State; st nil drops it. The
// caller holds mu, or is opening the rooms.
func (r *Rooms) setState(room string, st *members.State) {
	r.smu.Lock()
	defer r.smu.Unlock()
	if r.states == nil {
		r.states = map[string]members.State{}
	}
	if st == nil {
		delete(r.states, room)
		return
	}
	r.states[room] = *st
}

// publication is one room's count of membership publications (pubs).
type publication struct {
	n    int64
	wait chan struct{} // closed when the one in progress ends; nil if none
}

// beginPublish marks a commit that may change room's membership as in
// progress; the returned end, called once its state is published (or
// restored), ends it. The caller holds mu.
func (r *Rooms) beginPublish(room string) (end func()) {
	r.smu.Lock()
	if r.pubs == nil {
		r.pubs = map[string]*publication{}
	}
	p := r.pubs[room]
	if p == nil {
		p = &publication{}
		r.pubs[room] = p
	}
	p.n++
	p.wait = make(chan struct{})
	r.smu.Unlock()
	return func() {
		r.smu.Lock()
		defer r.smu.Unlock()
		p.n++
		close(p.wait)
		p.wait = nil
	}
}

// stateAt is State with the count of room's publications it was read at.
// While a publication is in progress it returns only wait, closed when it
// ends: the state then may be older than what the store has committed.
func (r *Rooms) stateAt(room string) (st members.State, n int64, wait <-chan struct{}, err error) {
	r.smu.Lock()
	defer r.smu.Unlock()
	if p := r.pubs[room]; p != nil {
		if p.wait != nil {
			return members.State{}, p.n, p.wait, nil
		}
		n = p.n
	}
	st, ok := r.states[room]
	if !ok {
		return members.State{}, n, nil, ErrNoRoom
	}
	return st, n, nil, nil
}

// published is the count of room's publications begun or ended.
func (r *Rooms) published(room string) int64 {
	r.smu.Lock()
	defer r.smu.Unlock()
	if p := r.pubs[room]; p != nil {
		return p.n
	}
	return 0
}

func (r *Rooms) room(room string) (*roomLog, error) {
	rl := r.rooms[room]
	if rl == nil {
		return nil, ErrNoRoom
	}
	return rl, nil
}

// Receive is the single ingress for log entries, whoever signed them:
// this machine on confirmation, a joiner's poll (step 4b), a pull or an
// offer (step 5). wires are wire objects exactly as received, in arrival
// order, and are one arrival batch. For each, in order: decode with the
// strict checks (S1 to S7, V1), the room (R1), the pinned create (C1) and
// the signature (G1); then the entries not held yet go to the room's
// members.Machine (C2 and C3: chain, parents, forks, settlement); then the
// batch is stored with the dispositions the machine returned, the member
// cache updated, requests raised for changes this machine's person can
// second, and, if this machine's own permission changed, the text it may
// hold re-decided and its agents' cursors seeded at its admission
// frontier, all in one transaction. gen, if not zero, is the generation
// the caller prepared under; a mismatch applies nothing
// (ErrStaleGeneration). A batch that changes nothing writes nothing.
//
// It returns one result per wire, in order; entries already held report
// their current disposition. On an error nothing was stored and the room
// is exactly as it was, so the same batch can be retried as if for the
// first time.
func (r *Rooms) Receive(ctx context.Context, room string, gen int64, wires ...[]byte) ([]EntryResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rl, err := r.room(room)
	if err != nil {
		return nil, err
	}
	if gen != 0 && gen != rl.gen {
		return nil, ErrStaleGeneration
	}
	return r.ingest(ctx, rl, gen, wires, nil)
}

// completion is the request a batch approves.
type completion struct {
	request, entry string
	notCaughtUp    bool // approved with ConfirmOpts.NotCaughtUpOK
}

func (r *Rooms) ingest(ctx context.Context, rl *roomLog, gen int64, wires [][]byte, done *completion) ([]EntryResult, error) {
	return r.ingestWith(ctx, rl, gen, wires, done, nil)
}

// commitFn writes a decided batch (nil when no kept entry changed) together
// with whatever else its caller writes in the same transaction (the pull
// exchange: an offer or a page, and an observation). st and look are the
// room's membership as the batch leaves it. It returns the batch number
// the store gave. An error writes nothing and leaves the room as it was.
type commitFn func(ctx context.Context, b *store.LogBatch, st members.State, look func(string) *core.VerifiedEntry) (int64, error)

// ingestWith is ingest with the store write done by commit, if not nil.
func (r *Rooms) ingestWith(ctx context.Context, rl *roomLog, gen int64, wires [][]byte, done *completion, commit commitFn) ([]EntryResult, error) {
	out := make([]EntryResult, len(wires))
	idx := make([]string, len(wires)) // the verified entry of each wire
	var fresh []*core.VerifiedEntry
	seen := map[string]bool{}
	for i, w := range wires {
		e, err := core.ParseEntry(w)
		if err != nil {
			out[i] = EntryResult{Disposition: members.Invalid, Reason: refusalCode(err)}
			continue
		}
		f := e.Fields()
		out[i].ID = e.ID()
		switch {
		case f.Room != rl.id:
			out[i].Disposition, out[i].Reason = members.Invalid, CodeWrongRoom
			continue
		case f.Kind == core.EntryCreate && e.ID() != rl.createID:
			out[i].Disposition, out[i].Reason = members.Invalid, CodeSecondCreate
			continue
		}
		v, err := e.Verified()
		if err != nil {
			out[i].Disposition, out[i].Reason = members.Invalid, CodeBadSignature
			continue
		}
		idx[i] = v.ID()
		if _, kept := rl.state[v.ID()]; !kept && !seen[v.ID()] {
			fresh = append(fresh, v)
		}
		seen[v.ID()] = true
	}
	final := map[string]members.Result{}
	fill := func() {
		for i, id := range idx {
			switch x, ok := final[id]; {
			case id == "":
			case ok:
				out[i].Disposition, out[i].Reason = x.Disposition, x.Reason
			default:
				if row, ok := rl.state[id]; ok {
					out[i].Disposition, out[i].Reason = members.Disposition(row.State), row.Reason
				} else {
					out[i].Disposition, out[i].Reason = members.Invalid, ReasonKnownInvalid
				}
			}
		}
	}
	if len(fresh) == 0 {
		fill() // nothing new: nothing to decide or write
		if commit != nil {
			if _, err := commit(ctx, nil, rl.st, rl.lookup(nil)); err != nil {
				return nil, err
			}
		}
		return out, nil
	}

	excluded := map[string]bool{}
	for {
		var batch []*core.VerifiedEntry
		for _, v := range fresh {
			if !excluded[v.ID()] {
				batch = append(batch, v)
			}
		}
		final = map[string]members.Result{}
		for _, x := range rl.mach.Receive(batch...) {
			if x.ID != "" {
				final[x.ID] = x
			}
		}
		for id := range excluded {
			final[id] = members.Result{ID: id, Disposition: members.Dropped, Reason: ReasonOverLimit}
		}
		if over := r.overBounds(rl, batch, final); len(over) > 0 {
			// Undo and decide the batch again without them.
			r.restore(rl)
			if r.rooms[rl.id] != rl {
				return nil, errors.New("room could not be restored")
			}
			for _, id := range over {
				excluded[id] = true
			}
			continue
		}
		b, publish, err := r.stage(ctx, rl, gen, batch, final, done)
		if err == nil && b == nil {
			fill() // decided, but nothing kept changed: the machine is as the store
			if commit != nil {
				if _, err := commit(ctx, nil, rl.st, rl.lookup(nil)); err != nil {
					return nil, err
				}
			}
			return out, nil
		}
		if err == nil {
			err = hookAt("ingest:apply")
			b.Now, b.Clock = r.m.now(), r.m.now // read again inside the write: an approval is judged against its invite's expiry then
		}
		var n int64
		// From before the commit until its state is published, a reader
		// of the membership waits (stateAt) [L1]: the store's change count
		// moves at the commit, State only at publish.
		end := r.beginPublish(rl.id)
		switch {
		case err != nil:
		case commit != nil:
			byID := map[string]*core.VerifiedEntry{}
			for _, v := range batch {
				byID[v.ID()] = v
			}
			n, err = commit(ctx, b, rl.mach.State(), rl.lookup(byID))
		default:
			n, err = r.m.st.ApplyLog(ctx, *b)
		}
		if err != nil {
			r.restore(rl)
			end()
			return nil, err
		}
		_ = hookAt("ingest:publish") // tests: committed, not yet published
		publish(n)
		end()
		r.m.wakes.touch(rl.id)
		fill()
		// After the commit memory and disk agree: a failure here changes
		// nothing, and the committed batch is not reported as failed.
		_ = hookAt("ingest:committed")
		return out, nil
	}
}

// overBounds returns the entries of batch, latest first, that would take
// the room over MaxRefusedDependants or MaxInvalidKept.
func (r *Rooms) overBounds(rl *roomLog, batch []*core.VerifiedEntry, final map[string]members.Result) []string {
	const (
		other = iota
		invalid
		refusedDep
	)
	class := func(d, reason string) int {
		switch {
		case d == string(members.Invalid):
			return invalid
		case d == string(members.Refused) && reason != members.ReasonForkRefused:
			return refusedDep
		}
		return other
	}
	n := map[int]int{}
	for id, row := range rl.state {
		if x, ok := final[id]; ok {
			n[class(string(x.Disposition), x.Reason)]++
		} else {
			n[class(row.State, row.Reason)]++
		}
	}
	for _, v := range batch {
		if x, ok := final[v.ID()]; ok {
			n[class(string(x.Disposition), x.Reason)]++
		}
	}
	limit := map[int]int{invalid: MaxInvalidKept, refusedDep: MaxRefusedDependants}
	var over []string
	for i := len(batch) - 1; i >= 0; i-- {
		x, ok := final[batch[i].ID()]
		if !ok {
			continue
		}
		if c := class(string(x.Disposition), x.Reason); c != other && n[c] > limit[c] {
			n[c]--
			over = append(over, batch[i].ID())
		}
	}
	return over
}

// stage works out what a decided batch writes, and returns the batch and
// the function that publishes it to rl once committed; a nil batch means
// nothing kept changed.
func (r *Rooms) stage(ctx context.Context, rl *roomLog, gen int64, batch []*core.VerifiedEntry,
	final map[string]members.Result, done *completion) (*store.LogBatch, func(int64), error) {
	kept := func(d members.Disposition) bool {
		return d == members.Accepted || d == members.Held || d == members.Refused || d == members.Invalid
	}
	b := &store.LogBatch{Room: rl.id, Gen: gen, Now: r.m.now()}
	byID := map[string]*core.VerifiedEntry{}
	for _, v := range batch {
		byID[v.ID()] = v
		if x, ok := final[v.ID()]; ok && kept(x.Disposition) {
			row := entryRow(v)
			row.State, row.Reason = string(x.Disposition), x.Reason
			b.New = append(b.New, row)
		}
	}
	for _, id := range sortedIDs(final) {
		x := final[id]
		old, was := rl.state[id]
		switch {
		case !was:
		case !kept(x.Disposition):
			b.Gone = append(b.Gone, id)
		case string(x.Disposition) != old.State || x.Reason != old.Reason:
			b.Changed = append(b.Changed, store.LogRow{ID: id, State: string(x.Disposition), Reason: x.Reason})
		}
	}
	if len(b.New) == 0 && len(b.Changed) == 0 && len(b.Gone) == 0 && done == nil {
		return nil, nil, nil
	}
	st := rl.mach.State()
	look := rl.lookup(byID)
	cache := r.memberCache(st, look)
	for _, n := range sortedIDs(cache) {
		if m, ok := rl.cache[n]; !ok || m != cache[n] {
			b.Members = append(b.Members, cache[n])
		}
	}
	for _, n := range sortedIDs(rl.cache) {
		if _, ok := cache[n]; !ok {
			b.MembersGone = append(b.MembersGone, n)
		}
	}
	reqs, err := r.raise(ctx, rl, st, look, final)
	if err != nil {
		return nil, nil, err
	}
	b.Requests = reqs
	if done != nil {
		b.Complete, b.Entries, b.NotCaughtUp = done.request, done.entry, done.notCaughtUp
	}
	perm := r.permKey(st)
	if p, member := st.Permissions[r.m.ID]; member && perm != rl.perm {
		from := readFrom(p, look)
		b.Access = func(rd *store.Reader) (store.Access, error) {
			return r.access(rd, rl.id, p, from), nil
		}
	}
	publish := func(n int64) {
		for i, row := range b.New {
			row.Header, row.Sig, row.Batch, row.Pos = nil, nil, n, i+1
			rl.ents[row.ID], rl.state[row.ID] = byID[row.ID], row
		}
		for _, row := range b.Changed {
			old := rl.state[row.ID]
			old.State, old.Reason = row.State, row.Reason
			rl.state[row.ID] = old
		}
		for _, id := range b.Gone {
			delete(rl.ents, id)
			delete(rl.state, id)
		}
		rl.st, rl.perm, rl.cache = st, perm, cache
		r.setState(rl.id, &rl.st)
		r.setFrontier(rl)
	}
	return b, publish, nil
}

func refusalCode(err error) string {
	var rf *core.Refusal
	if errors.As(err, &rf) {
		return rf.Code
	}
	return "bad_json"
}

func sortedIDs[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// memberCache is the member cache for state st: every machine ever
// admitted, with whether it is a member and an admin now, and its name
// from the newest effective grant that named it.
func (r *Rooms) memberCache(st members.State, look func(string) *core.VerifiedEntry) map[string]store.RoomMember {
	names := map[string]string{}
	newest := map[string]int64{}
	for _, id := range sortedIDs(st.Effects) {
		v := look(id)
		if v == nil || !st.Effects[id].Applied {
			continue
		}
		f := v.Fields()
		if f.Name == "" {
			continue
		}
		if t := f.Created.UnixMilli(); t >= newest[f.Target] {
			names[f.Target], newest[f.Target] = f.Name, t
		}
	}
	out := map[string]store.RoomMember{}
	for _, n := range st.Admitted {
		out[n] = store.RoomMember{Node: n, Admitted: true, Member: slices.Contains(st.Members, n),
			Admin: slices.Contains(st.Admins, n), Name: names[n]}
	}
	return out
}

func cacheRows(c map[string]store.RoomMember) []store.RoomMember {
	out := make([]store.RoomMember, 0, len(c))
	for _, n := range sortedIDs(c) {
		out = append(out, c[n])
	}
	return out
}

// permKey identifies this machine's membership and history permission.
func (r *Rooms) permKey(st members.State) string {
	p, ok := st.Permissions[r.m.ID]
	if !ok {
		return ""
	}
	return fmt.Sprintf("%v|%v|%v|%v", p.All, p.Closed, p.From, p.Grants)
}
