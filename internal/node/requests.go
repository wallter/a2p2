package node

// Changes to a room's member list wait for a person (P3, DESIGN-next
// section 7). Propose records a request; Confirm, the person's step,
// revalidates it against the room's state at that moment and only then
// signs the entry and applies it through the same ingress as any other
// entry, with the approval, in one transaction. Declining signs nothing.
// An entry by another admin that needs a matching one from this machine
// raises a request by itself (Seconds), so the other admin sees it [M4].

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/members"
	"github.com/wallter/a2p2/internal/store"
)

// Change is a proposed change to a room's member list.
type Change struct {
	Kind   core.EntryKind // add, remove or admin
	Target string         // node id
	Name   string         // the target's label: required to add, optional to make admin
	// History lets a machine admitted by this change read earlier history;
	// without it the grant carries this machine's heads as its frontier.
	// Ignored when the target is already a member.
	History bool
}

// Actions a request approves: exactly what the person agrees to.
const (
	ActionAdmit      = "admit"       // add a machine that is not a member
	ActionAdmitAdmin = "admit_admin" // add a machine that is not a member, as an admin
	ActionPromote    = "promote"     // make an existing member an admin
	ActionRemove     = "remove"      // remove a member
)

// fromWithHistory: every admitting grant carries the signer's heads as
// the reading frontier, with or without history (lead's ruling on [R8]).
// internal/core refuses from together with history today (PROTOCOL-next
// section 3, S7), so a grant with history carries none and its grantee's
// agents start at 0; set this once core accepts both.
const fromWithHistory = true

// maxHeadsPerAuthor is PROTOCOL-next section 3's bound on from heads.
const maxHeadsPerAuthor = 16

// actionOf is what an entry of kind on target would do in state st.
func actionOf(st members.State, kind, target string) string {
	switch {
	case kind == string(core.EntryRemove):
		return ActionRemove
	case kind == string(core.EntryAdd):
		return ActionAdmit
	case slices.Contains(st.Members, target):
		return ActionPromote
	}
	return ActionAdmitAdmin
}

func roleOf(action string) string {
	switch action {
	case ActionAdmit:
		return "member"
	case ActionAdmitAdmin, ActionPromote:
		return "admin"
	}
	return ""
}

// Effects of an applied change, as room/admin and room/remove return them.
const (
	EffectApplied = "applied"
	EffectPending = "pending" // waits for a matching entry from another admin
)

// Confirmation is the outcome of Confirm.
type Confirmation struct {
	ID     string // the request
	State  string // store.RequestApproved, RequestDeclined or RequestFailed
	Reason string // why it failed, or why the entry is pending
	Entry  string // the entry signed, when approved
	Effect string // EffectApplied or EffectPending, when approved
}

// Reasons a request is refused or fails, besides the members package's.
const (
	ReasonSelf        = "a machine cannot add or make itself an admin; to stop carrying a room, leave it"
	ReasonNotWaiting  = "the change it would second is no longer waiting for a second admin"
	ReasonRoomGone    = "the room is no longer held here"
	ReasonNotPulled   = "this machine has not fetched the room's log from another member since it started"
	ReasonActionMoved = "the room changed: signing now would "
	ReasonTooMany     = "too many requests are waiting in this room"
	errNotPendingText = "request is not pending"
)

// Propose records a request for a change to room's member list, for a
// person to confirm. It is refused at once if it could not apply now.
func (r *Rooms) Propose(ctx context.Context, room string, c Change) (store.Request, error) {
	switch c.Kind {
	case core.EntryAdd, core.EntryRemove, core.EntryAdmin:
	default:
		return store.Request{}, userErr("a change adds, removes or makes an admin")
	}
	if !core.ValidNode(c.Target) {
		return store.Request{}, userErr("bad node id")
	}
	if c.Kind == core.EntryAdd && c.Name == "" || c.Name != "" && !core.ValidLabel(c.Name) {
		return store.Request{}, userErr("bad or missing name for the machine")
	}
	if c.Kind == core.EntryRemove {
		c.Name, c.History = "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rl, err := r.room(room)
	if err != nil {
		return store.Request{}, err
	}
	q := store.Request{Kind: string(c.Kind), Room: room, Node: c.Target, Name: c.Name,
		Action: actionOf(rl.st, string(c.Kind), c.Target), Created: r.m.now(), State: store.RequestPending}
	if q.Action == ActionAdmit || q.Action == ActionAdmitAdmin {
		q.History = c.History // promoting changes no permission
	}
	q.Role = roleOf(q.Action)
	if reason := r.check(rl, q); reason != "" {
		return store.Request{}, &UserError{reason}
	}
	pending, err := r.m.st.Requests(ctx, room, store.RequestPending)
	if err != nil {
		return store.Request{}, err
	}
	if len(pending) >= MaxPendingRequests {
		return store.Request{}, &UserError{ReasonTooMany}
	}
	q.ID = r.newID()
	if err := r.m.st.CreateRequest(ctx, q); err != nil {
		return store.Request{}, err
	}
	return q, nil
}

// Requests lists requests waiting for a person, in every room.
func (r *Rooms) Requests(ctx context.Context) ([]store.Request, error) {
	return r.m.st.Requests(ctx, "", store.RequestPending)
}

// Confirm is the person's decision on request id (P3). Declining records
// it and signs nothing. Approving revalidates the request against the
// room's current state: if it no longer applies (the target was added or
// removed meanwhile, this machine is no longer an admin, the change it
// would second has been decided) it fails with the reason and nothing is
// signed or stored; otherwise the entry is signed and applied with the
// approval in one transaction. Decisions are serialised: two approvals,
// or an approval and a decline, sign at most once.
func (r *Rooms) Confirm(ctx context.Context, id string, approve bool) (Confirmation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	q, err := r.m.st.GetRequest(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return Confirmation{}, userErr("no such request")
	}
	if err != nil {
		return Confirmation{}, err
	}
	if q.State != store.RequestPending {
		return Confirmation{}, &UserError{errNotPendingText}
	}
	now := r.m.now()
	if !approve {
		if err := r.m.st.DecideRequest(ctx, id, store.RequestDeclined, "", now); err != nil {
			return Confirmation{}, err
		}
		return Confirmation{ID: id, State: store.RequestDeclined}, nil
	}
	fail := func(reason string) (Confirmation, error) {
		if err := r.m.st.DecideRequest(ctx, id, store.RequestFailed, reason, now); err != nil {
			return Confirmation{}, err
		}
		return Confirmation{ID: id, State: store.RequestFailed, Reason: reason}, nil
	}
	rl := r.rooms[q.Room]
	if rl == nil {
		return fail(ReasonRoomGone)
	}
	if !r.maySign(rl) {
		return Confirmation{}, &UserError{ReasonNotPulled} // stays pending
	}
	if reason := r.check(rl, q); reason != "" {
		return fail(reason)
	}
	v, err := r.sign(ctx, rl, q)
	if err != nil {
		return Confirmation{}, err
	}
	// What the entry would do, resolved with the room's accepted entries:
	// one that would have no effect (other than waiting for a second admin)
	// is not stored.
	var acc []*core.VerifiedEntry
	for _, eid := range sortedIDs(rl.ents) {
		if rl.state[eid].State == store.LogAccepted {
			acc = append(acc, rl.ents[eid])
		}
	}
	dry, err := members.ResolveRoom(rl.createID, append(acc, v))
	if err != nil {
		return Confirmation{}, err
	}
	if eff := dry.Effects[v.ID()]; !eff.Applied && eff.Reason != members.ReasonNeedsSecond {
		return fail(eff.Reason)
	}
	res, err := r.ingest(ctx, rl, 0, [][]byte{v.Wire()}, &completion{request: id, entry: v.ID()})
	if err != nil {
		return Confirmation{}, err
	}
	if res[0].Disposition != members.Accepted {
		return Confirmation{}, errors.New("own entry not accepted: " + res[0].Reason)
	}
	c := Confirmation{ID: id, State: store.RequestApproved, Entry: v.ID(), Effect: EffectApplied}
	if eff := rl.st.Effects[v.ID()]; !eff.Applied {
		c.Effect, c.Reason = EffectPending, eff.Reason
	}
	return c, nil
}

// maySign is the hook for "since starting, a daemon signs no entry for a
// room until it has pulled that room's log from another member" (DESIGN-next
// section 3). Creating a room and rooms where this machine is the only
// member are exempt. Until pull exists (step 5) nothing sets rl.pulled, so
// the rule is not enforced yet: step 5 sets rl.pulled on the first
// successful pull of the room since start and makes this return
// rl.pulled for the other rooms.
func (r *Rooms) maySign(rl *roomLog) bool {
	if len(rl.st.Members) <= 1 {
		return true
	}
	return true // step 5: return rl.pulled
}

// check says why request q cannot apply to rl's current state, or "".
func (r *Rooms) check(rl *roomLog, q store.Request) string {
	st := rl.st
	self := r.m.ID
	member, admin := slices.Contains(st.Members, q.Node), slices.Contains(st.Admins, q.Node)
	switch {
	case !slices.Contains(st.Admins, self):
		return members.ReasonNotAdmin
	case q.Node == self && q.Kind == string(core.EntryRemove):
		return members.ReasonSelfVote + "; to stop carrying a room, leave it"
	case q.Node == self:
		return ReasonSelf
	case q.Kind == string(core.EntryAdd) && member:
		return members.ReasonAlreadyMember
	case q.Kind == string(core.EntryRemove) && !member:
		return members.ReasonNotMember
	case q.Kind == string(core.EntryAdmin) && admin:
		return members.ReasonAlreadyAdmin
	}
	if now := actionOf(st, q.Kind, q.Node); now != q.Action {
		return ReasonActionMoved + now + " instead of " + q.Action
	}
	if q.Seconds != "" {
		if eff, ok := st.Effects[q.Seconds]; !ok || eff.Applied || eff.Reason != members.ReasonNeedsSecond {
			return ReasonNotWaiting
		}
	}
	return ""
}

// sign builds and signs this machine's next entry for request q: the next
// position of its own chain, naming as parents the heads of the entries
// it has accepted (never a refused one [M11, M12]) and its previous entry.
// A grant that admits carries history or this machine's heads per author.
func (r *Rooms) sign(ctx context.Context, rl *roomLog, q store.Request) (*core.VerifiedEntry, error) {
	f := core.EntryFields{Room: rl.id, Kind: core.EntryKind(q.Kind), Target: q.Node, Name: q.Name,
		Seq: 1, Created: r.m.now()}
	parentOf := map[string]bool{}
	var accepted []string
	for _, id := range sortedIDs(rl.ents) {
		if rl.state[id].State != store.LogAccepted {
			continue
		}
		accepted = append(accepted, id)
		ef := rl.ents[id].Fields()
		for _, p := range ef.Parents {
			parentOf[p] = true
		}
		if rl.ents[id].Author() == r.m.ID && ef.Seq >= f.Seq {
			f.Seq, f.Prev = ef.Seq+1, id
		}
	}
	for _, id := range accepted {
		if !parentOf[id] || id == f.Prev {
			f.Parents = append(f.Parents, id)
		}
	}
	f.Parents = slices.Compact(f.Parents)
	if q.Action == ActionAdmit || q.Action == ActionAdmitAdmin {
		f.History = q.History
		if !q.History || fromWithHistory {
			from, err := r.heads(ctx, rl.id)
			if err != nil {
				return nil, err
			}
			f.From = from
		}
	}
	r.signs++
	e, err := core.NewEntry(r.m.key, f)
	if err != nil {
		return nil, userErr("cannot sign the change: %v", err)
	}
	return e.Verified()
}

// heads are this machine's heads per author in room: the records held
// that no other held record of the same author follows.
func (r *Rooms) heads(ctx context.Context, room string) (map[string][]string, error) {
	out := map[string][]string{}
	err := r.m.view(ctx, room, func(v *roomView) error {
		authors := map[string]bool{}
		for _, s := range v.streams {
			authors[s.Author] = true
		}
		for _, a := range sortedIDs(authors) {
			ls, err := v.ownHeads(a)
			if err != nil {
				return err
			}
			if len(ls) > maxHeadsPerAuthor {
				return userErr("cannot sign the grant: machine %s has %d heads in this room, more than a grant may carry (%d)",
					short(a), len(ls), maxHeadsPerAuthor)
			}
			for _, l := range ls {
				out[a] = append(out[a], l.ID)
			}
		}
		return nil
	})
	if len(out) == 0 {
		out = nil
	}
	return out, err
}

// raise returns the requests a batch raises: for each entry newly
// accepted that waits for a second admin, signed by another admin, that
// this machine could second as an admin, and has not been asked about,
// while the room has fewer than MaxPendingRequests waiting. Each records
// the action the entry would have here and its history setting.
func (r *Rooms) raise(ctx context.Context, rl *roomLog, st members.State, look func(string) *core.VerifiedEntry,
	final map[string]members.Result) ([]store.Request, error) {
	if !slices.Contains(st.Admins, r.m.ID) {
		return nil, nil
	}
	var asked map[string]bool
	pending := 0
	var out []store.Request
	for _, id := range sortedIDs(final) {
		v := look(id)
		if final[id].Disposition != members.Accepted || v == nil || v.Author() == r.m.ID {
			continue
		}
		f := v.Fields()
		if f.Kind != core.EntryAdmin && f.Kind != core.EntryRemove || f.Target == r.m.ID {
			continue
		}
		if eff := st.Effects[id]; eff.Applied || eff.Reason != members.ReasonNeedsSecond {
			continue
		}
		if asked == nil {
			if err := hookAt("ingest:requests"); err != nil {
				return nil, err
			}
			qs, err := r.m.st.Requests(ctx, rl.id, "")
			if err != nil {
				return nil, err
			}
			asked = map[string]bool{}
			for _, q := range qs {
				asked[q.Seconds] = true
				if q.State == store.RequestPending {
					pending++
				}
			}
		}
		if asked[id] || pending >= MaxPendingRequests {
			continue
		}
		asked[id] = true
		pending++
		q := store.Request{ID: r.newID(), Kind: string(f.Kind), Room: rl.id, Node: f.Target, Name: f.Name,
			Seconds: id, Action: actionOf(st, string(f.Kind), f.Target), Created: r.m.now(), State: store.RequestPending}
		if q.Action == ActionAdmitAdmin {
			q.History = f.History
		}
		q.Role = roleOf(q.Action)
		out = append(out, q)
	}
	return out, nil
}

func (r *Rooms) newID() string {
	var b [16]byte
	r.m.rand(b[:])
	return "q_" + hex.EncodeToString(b[:])
}
