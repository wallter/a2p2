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
	"strings"

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
	// Admin, with Kind add, also makes the machine an admin: on approval
	// this machine signs the add and then its own admin entry, which waits
	// for a matching one from another admin [M4] (the third admin,
	// DESIGN-next section 7). One confirmation covers both.
	Admin bool
}

// Actions a request approves: exactly what the person agrees to.
const (
	ActionAdmit      = "admit"       // add a machine that is not a member
	ActionAdmitAdmin = "admit_admin" // add a machine that is not a member, as an admin
	ActionPromote    = "promote"     // make an existing member an admin
	ActionRemove     = "remove"      // remove a member
	// ActionRejoin lets a machine that is still a member, and joins again
	// with a fresh invite, fetch the room's log: no entry is signed.
	ActionRejoin = "rejoin"
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

// requestAction is what request q would do in state st now: actionOf, but
// an add that also makes an admin (Change.Admin) stays admit_admin while
// its target is not a member, and a rejoin stays a rejoin while it is.
func requestAction(st members.State, q store.Request) string {
	now := actionOf(st, q.Kind, q.Node)
	switch {
	case q.Action == ActionRejoin && slices.Contains(st.Members, q.Node):
		return ActionRejoin
	case q.Kind == string(core.EntryAdd) && q.Action == ActionAdmitAdmin && now == ActionAdmit:
		return ActionAdmitAdmin
	}
	return now
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
	Entry  string // the (last) entry signed, when approved
	Effect string // EffectApplied or EffectPending of Entry, when approved
	// Entries are every entry signed, in order: two for an add that also
	// makes an admin, none for a rejoin.
	Entries []string
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
	ReasonTermsMoved  = "the machine's role or history permission changed since it was shown; make a new invite"
	ReasonQuorumMoved = "the number of admins changed since the request was made, so the entries it needs differ; make a new request"
	ReasonNoGrant     = "the machine is still a member: approving lets it fetch the room's log and signs nothing"
	ReasonNotAdmitted = "the room's admins changed since the invite was accepted, so the joiner would not be admitted now; make a new invite"
)

// Propose records a request for a change to room's member list, for a
// person to confirm. It is refused at once if it could not apply now.
func (r *Rooms) Propose(ctx context.Context, room string, c Change) (store.Request, error) {
	c, err := checkChange(c)
	if err != nil {
		return store.Request{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rl, err := r.room(room)
	if err != nil {
		return store.Request{}, err
	}
	q, err := r.prepare(ctx, rl, c)
	if err != nil {
		return store.Request{}, err
	}
	if err := r.m.st.CreateRequest(ctx, q); err != nil {
		return store.Request{}, err
	}
	return q, nil
}

// checkChange checks the form of c and clears what its kind ignores.
func checkChange(c Change) (Change, error) {
	switch c.Kind {
	case core.EntryAdd, core.EntryRemove, core.EntryAdmin:
	default:
		return c, userErr("a change adds, removes or makes an admin")
	}
	if !core.ValidNode(c.Target) {
		return c, userErr("bad node id")
	}
	if c.Kind == core.EntryAdd && c.Name == "" || c.Name != "" && !core.ValidLabel(c.Name) {
		return c, userErr("bad or missing name for the machine")
	}
	if c.Kind == core.EntryRemove {
		c.Name, c.History = "", false
	}
	if c.Kind != core.EntryAdd {
		c.Admin = false
	}
	return c, nil
}

// prepare builds the request for change c (checked by checkChange) in
// rl's current state, refused at once if it could not apply now or too
// many wait. The caller holds r.mu and stores the request.
func (r *Rooms) prepare(ctx context.Context, rl *roomLog, c Change) (store.Request, error) {
	q := store.Request{Kind: string(c.Kind), Room: rl.id, Node: c.Target, Name: c.Name,
		Action: actionOf(rl.st, string(c.Kind), c.Target), Created: r.m.now(), State: store.RequestPending}
	if c.Admin && q.Action == ActionAdmit {
		q.Action = ActionAdmitAdmin
	}
	return r.prepared(ctx, rl, q, c.History)
}

// prepared finishes request q: its history (only a grant that admits
// carries one), role, the checks and an id.
func (r *Rooms) prepared(ctx context.Context, rl *roomLog, q store.Request, history bool) (store.Request, error) {
	if q.Action == ActionAdmit || q.Action == ActionAdmitAdmin {
		q.History = history // promoting changes no permission
	}
	if q.Action != ActionRejoin { // a rejoin names the role and history the member has
		q.Role = roleOf(q.Action)
	}
	if q.Action == ActionAdmitAdmin || q.Action == ActionPromote {
		q.Admins = len(rl.st.Admins) // one admin's entry, or two [M4]
	}
	if reason := r.check(rl, q); reason != "" {
		return store.Request{}, &UserError{reason}
	}
	pending, err := r.m.st.Requests(ctx, rl.id, store.RequestPending)
	if err != nil {
		return store.Request{}, err
	}
	if len(pending) >= MaxPendingRequests {
		return store.Request{}, &UserError{ReasonTooMany}
	}
	q.ID = r.newID()
	return q, nil
}

// Requests lists requests waiting for a person, in every room.
func (r *Rooms) Requests(ctx context.Context) ([]store.Request, error) {
	if _, err := r.m.st.ExpireInvites(ctx, r.m.now()); err != nil {
		return nil, err
	}
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
	invited, expired, err := r.inviteOf(ctx, q, now)
	if err != nil || expired {
		if err != nil {
			return Confirmation{}, err
		}
		return Confirmation{ID: id, State: store.RequestFailed, Reason: store.ReasonInviteExpired}, nil
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
	if q.Action == ActionRejoin {
		// Nothing to sign: the approval alone is stored.
		_, err := r.m.st.ApplyLog(ctx, store.LogBatch{Room: rl.id, Gen: rl.gen, Complete: id, Now: r.m.now()})
		if errors.Is(err, store.ErrInviteExpired) {
			if _, err := r.m.st.ExpireInvites(ctx, r.m.now()); err != nil {
				return Confirmation{}, err
			}
			return Confirmation{ID: id, State: store.RequestFailed, Reason: store.ReasonInviteExpired}, nil
		}
		if err != nil {
			return Confirmation{}, err
		}
		return Confirmation{ID: id, State: store.RequestApproved, Effect: EffectApplied, Reason: ReasonNoGrant}, nil
	}
	// An add that also makes an admin is the add, then this machine's own
	// admin entry naming the new member, both under this one approval.
	steps := []store.Request{q}
	if q.Kind == string(core.EntryAdd) && q.Action == ActionAdmitAdmin {
		add, promote := q, q
		add.Action = ActionAdmit
		promote.Kind, promote.Action, promote.History = string(core.EntryAdmin), ActionPromote, false
		steps = []store.Request{add, promote}
	}
	var signed []*core.VerifiedEntry
	for _, step := range steps {
		v, err := r.sign(ctx, rl, step, signed...)
		if err != nil {
			return Confirmation{}, err
		}
		signed = append(signed, v)
	}
	// What the entries would do, resolved with the room's accepted
	// entries: one that would have no effect (other than waiting for a
	// second admin) is not stored.
	var acc []*core.VerifiedEntry
	for _, eid := range sortedIDs(rl.ents) {
		if rl.state[eid].State == store.LogAccepted {
			acc = append(acc, rl.ents[eid])
		}
	}
	dry, err := members.ResolveRoom(rl.createID, append(acc, signed...))
	if err != nil {
		return Confirmation{}, err
	}
	var wires [][]byte
	var ids []string
	for _, v := range signed {
		if eff := dry.Effects[v.ID()]; !eff.Applied && eff.Reason != members.ReasonNeedsSecond {
			return fail(eff.Reason)
		}
		wires, ids = append(wires, v.Wire()), append(ids, v.ID())
	}
	if invited && !slices.Contains(dry.Members, q.Node) {
		// The joiner was shown that it would be admitted (DESIGN-next 7).
		return fail(ReasonNotAdmitted)
	}
	res, err := r.ingest(ctx, rl, 0, wires, &completion{request: id, entry: strings.Join(ids, ",")})
	if errors.Is(err, store.ErrInviteExpired) {
		// The invite ran out while this was being signed: nothing was stored.
		if _, err := r.m.st.ExpireInvites(ctx, r.m.now()); err != nil {
			return Confirmation{}, err
		}
		return Confirmation{ID: id, State: store.RequestFailed, Reason: store.ReasonInviteExpired}, nil
	}
	if err != nil {
		return Confirmation{}, err
	}
	for _, x := range res {
		if x.Disposition != members.Accepted {
			return Confirmation{}, errors.New("own entry not accepted: " + x.Reason)
		}
	}
	last := ids[len(ids)-1]
	c := Confirmation{ID: id, State: store.RequestApproved, Entry: last, Effect: EffectApplied, Entries: ids}
	if eff := rl.st.Effects[last]; !eff.Applied {
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
	case q.Action == ActionRejoin && !member:
		return members.ReasonNotMember
	case q.Action == ActionRejoin && effectiveTerms(st, "", false, q.Node) != store.Terms{Member: true, Role: q.Role, History: q.History}:
		return ReasonTermsMoved
	case q.Action == ActionRejoin:
		return ""
	case q.Admins != 0 && q.Seconds == "" && len(st.Admins) != q.Admins:
		return ReasonQuorumMoved
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
	if now := requestAction(st, q); now != q.Action {
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
// before are entries this machine signed under the same approval and has
// not stored yet; they count as accepted.
func (r *Rooms) sign(ctx context.Context, rl *roomLog, q store.Request, before ...*core.VerifiedEntry) (*core.VerifiedEntry, error) {
	f := core.EntryFields{Room: rl.id, Kind: core.EntryKind(q.Kind), Target: q.Node, Name: q.Name,
		Seq: 1, Created: r.m.now()}
	parentOf := map[string]bool{}
	var accepted []string
	look := map[string]*core.VerifiedEntry{}
	for _, v := range before {
		look[v.ID()] = v
	}
	for id, v := range rl.ents {
		if rl.state[id].State == store.LogAccepted {
			look[id] = v
		}
	}
	for _, id := range sortedIDs(look) {
		accepted = append(accepted, id)
		ef := look[id].Fields()
		for _, p := range ef.Parents {
			parentOf[p] = true
		}
		if look[id].Author() == r.m.ID && ef.Seq >= f.Seq {
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
