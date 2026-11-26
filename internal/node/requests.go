package node

// Changes to a room's member list wait for a person (P3, DESIGN
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
	"math"
	"slices"
	"sort"
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
	// DESIGN section 7). One confirmation covers both.
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
// internal/core refuses from together with history today (PROTOCOL
// section 3, S7), so a grant with history carries none and its grantee's
// agents start at 0; set this once core accepts both.
const fromWithHistory = true

// maxHeadsPerAuthor is PROTOCOL section 3's bound on from heads.
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
	ReasonActionMoved = "the room changed: signing now would "
	ReasonTooMany     = "too many requests are waiting in this room"
	errNotPendingText = "request is not pending"
	ReasonNeedsInvite = "a machine joins only through an invite it previewed and accepted; make an invite"
	ReasonTermsMoved  = "the machine's role or history permission changed since it was shown; make a new invite"
	ReasonQuorumMoved = "the number of admins changed since the request was made, so the entries it needs differ; make a new request"
	ReasonNoGrant     = "the machine is still a member: approving lets it fetch the room's log and signs nothing"
	ReasonNotAdmitted = "the room's admins changed since the invite was accepted, so the joiner would not be admitted now; make a new invite"
)

// Propose records a request for a change to room's member list, for a
// person to confirm. It is refused at once if it could not apply now.
//
// Propose never admits: an add, or an admin entry, for a machine that is
// not a member is refused (ReasonNeedsInvite). A machine is admitted only
// through the invite path, where it previewed the terms and its person
// accepted them (JoinServer.accept).
func (r *Rooms) Propose(ctx context.Context, room string, c Change) (store.Request, error) {
	return r.propose(ctx, room, c, false)
}

// proposeAny is Propose without the invite rule, for tests that admit
// machines without running the join exchange. Nothing else calls it.
func (r *Rooms) proposeAny(ctx context.Context, room string, c Change) (store.Request, error) {
	return r.propose(ctx, room, c, true)
}

func (r *Rooms) propose(ctx context.Context, room string, c Change, admit bool) (store.Request, error) {
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
	if !admit && c.Kind != core.EntryRemove && !slices.Contains(rl.st.Members, c.Target) {
		return store.Request{}, &UserError{ReasonNeedsInvite, ErrorNotAllowed}
	}
	q, err := r.prepare(ctx, rl, c)
	if err != nil {
		return store.Request{}, err
	}
	switch err := r.m.st.CreateRequest(ctx, q); {
	case errors.Is(err, store.ErrTooManyPending):
		return store.Request{}, &UserError{ReasonTooMany, ErrorNotAllowed}
	case err != nil:
		return store.Request{}, err
	}
	q.Cap = 0
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
		return store.Request{}, &UserError{Msg: reason}
	}
	pending, err := r.m.st.Requests(ctx, rl.id, store.RequestPending)
	if err != nil {
		return store.Request{}, err
	}
	// Only the requests this machine started count: requests raised to
	// second another admin's entries have their own budgets (raise) [F1].
	// A removal of an admin passes it, once per target, so that agents'
	// sends with files or invite accepts never stop the person proposing
	// one. The store checks the budget again when it inserts (Cap).
	own, removing := 0, false
	for _, p := range pending {
		if p.Seconds == "" {
			own++
			removing = removing || p.Kind == string(core.EntryRemove) && p.Node == q.Node
		}
	}
	q.Cap = MaxPendingRequests
	if q.Kind == string(core.EntryRemove) && slices.Contains(rl.st.Admins, q.Node) && !removing {
		q.Cap = 0
	}
	if q.Cap > 0 && own >= q.Cap {
		return store.Request{}, &UserError{ReasonTooMany, ErrorNotAllowed}
	}
	q.ID = r.newID()
	return q, nil
}

// Requests lists requests waiting for a person, in every room.
func (r *Rooms) Requests(ctx context.Context) ([]store.Request, error) {
	if _, err := r.m.st.ExpireInvites(ctx, r.m.now()); err != nil {
		return nil, err
	}
	qs, err := r.m.st.Requests(ctx, "", store.RequestPending)
	if err != nil {
		return nil, err
	}
	return r.withDeadlines(ctx, qs)
}

// withDeadlines fills the deadline of each request an invite raised.
func (r *Rooms) withDeadlines(ctx context.Context, qs []store.Request) ([]store.Request, error) {
	for i, q := range qs {
		inv, err := r.m.st.InviteByRequest(ctx, q.ID)
		switch {
		case err == nil:
			qs[i].Deadline = inv.Expires
		case !errors.Is(err, store.ErrNotFound):
			return nil, err
		}
	}
	return qs, nil
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
	return r.ConfirmWith(ctx, id, approve, ConfirmOpts{})
}

// ReasonNotCaughtUp is why an approval that would sign was refused: this
// machine has not caught up with the room's log since it started.
const ReasonNotCaughtUp = "not_caught_up"

// ErrNotCaughtUp: the approval would sign an entry for a room whose log
// this machine has not caught up with since it started (DESIGN 3);
// nothing was signed and the request stays pending. Signing now risks
// reusing a position this machine signed before a restore, or building on
// a log other members have moved past.
var ErrNotCaughtUp = &UserError{Code: ErrorNotCaughtUp, Msg: ReasonNotCaughtUp + ": this machine has not caught up with the room's log from " +
	"another member since it started; signing now risks a fork if it was restored from an older copy"}

// ConfirmOpts are a person's choices on one approval.
type ConfirmOpts struct {
	// NotCaughtUpOK signs although the room's log has not been caught up
	// with since start (every other member may be gone for good). The
	// surface asks for it only after showing the risk; it is recorded
	// with the request.
	NotCaughtUpOK bool
	// Shown, when set, is the request's text as the person was shown it.
	// An approval is refused, with nothing signed, unless the text built
	// now, under the decision lock, is the same: the text names the
	// room's members, and a pull may have changed them since.
	Shown string
}

// RequestChangedText is the refusal when a request's text changed since
// the person was shown it (ConfirmOpts.Shown).
const RequestChangedText = "the request changed since the page showed it; reload and decide again"

// ConfirmWith is Confirm with the person's options.
func (r *Rooms) ConfirmWith(ctx context.Context, id string, approve bool, o ConfirmOpts) (Confirmation, error) {
	if r.files != nil {
		// A send with files signs no log entry: decided outside mu. It is
		// approved only through Files.ApproveSend, with the terms shown;
		// here it may only be declined.
		if q, err := r.m.st.GetRequest(ctx, id); err == nil && q.Kind == store.RequestSendFiles {
			if approve && q.State == store.RequestPending {
				return Confirmation{}, errSendNeedsTerms
			}
			return r.files.confirmSend(ctx, q, approve)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	q, err := r.m.st.GetRequest(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return Confirmation{}, codedErr(ErrorNotFound, "no such request")
	}
	if err != nil {
		return Confirmation{}, err
	}
	if q.State == store.RequestFailed {
		// Already failed (its invite expired, or revalidation): what was recorded.
		return Confirmation{ID: id, State: q.State, Reason: q.Reason}, nil
	}
	if q.State != store.RequestPending {
		return Confirmation{}, &UserError{errNotPendingText, ErrorAlreadyDone}
	}
	if approve && o.Shown != "" && q.Kind != store.RequestSendFiles {
		label := ""
		if q.Room != "" {
			lr, err := r.m.st.LogRoom(ctx, q.Room)
			if err != nil {
				return Confirmation{}, err
			}
			label = r.ShownLabel(q.Room, lr.Label) // as Describe and Alerts show it
		}
		if requestText(q, label, r.memberNamesLocked(q.Room)) != o.Shown {
			return Confirmation{}, &UserError{Msg: RequestChangedText}
		}
	}
	now := r.m.now()
	// A decision frees a place in the room's budgets: an entry that waited
	// for one is raised now. The decision is committed already, so a
	// failure here is left to the room's next batch or pull to retry
	// (raiseWaiting).
	raiseNext := func() {
		if rl := r.rooms[q.Room]; rl != nil {
			_ = r.raiseWaiting(ctx, rl)
		}
	}
	if !approve {
		if err := r.m.st.DecideRequest(ctx, id, store.RequestDeclined, "", now); err != nil {
			return Confirmation{}, err
		}
		raiseNext()
		return Confirmation{ID: id, State: store.RequestDeclined}, nil
	}
	fail := func(reason string) (Confirmation, error) {
		if err := r.m.st.DecideRequest(ctx, id, store.RequestFailed, reason, now); err != nil {
			return Confirmation{}, err
		}
		raiseNext()
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
	if reason := r.check(rl, q); reason != "" {
		return fail(reason)
	}
	// Approving a rejoin signs nothing; anything else waits for the
	// catch-up unless the person accepted the risk (stays pending).
	override := false
	if q.Action != ActionRejoin && !r.maySign(rl) {
		if !o.NotCaughtUpOK {
			return Confirmation{}, ErrNotCaughtUp
		}
		override = true
	}
	if q.Action == ActionRejoin {
		// Nothing to sign: the approval alone is stored.
		_, err := r.m.st.ApplyLog(ctx, store.LogBatch{Room: rl.id, Gen: rl.gen, Complete: id, Now: r.m.now(), Clock: r.m.now})
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
		// The joiner was shown that it would be admitted (DESIGN 7).
		return fail(ReasonNotAdmitted)
	}
	res, err := r.ingest(ctx, rl, 0, wires, &completion{request: id, entry: strings.Join(ids, ","), notCaughtUp: override})
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

// maySign enforces "since starting, a daemon signs no entry for a room
// until it has caught up with that room's log from another member"
// (DESIGN section 3). rl.pulled is set by an exchange with a current
// member in which every log head that member advertised is accepted here
// (Exchange.Pull), by a join that completes with every head of its last
// page accepted (Joiner.verify), and for a room created in this run.
// Rooms where this machine is the only member are exempt. The rule
// narrows the restore problem; it cannot detect a restore.
func (r *Rooms) maySign(rl *roomLog) bool {
	if len(rl.st.Members) <= 1 {
		return true
	}
	return rl.pulled
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

// raising is what raise decides for a room: the requests to create, and
// the pending requests to close as failed (ReasonNotWaiting) because the
// entry they would second no longer waits.
type raising struct {
	reqs  []store.Request
	stale []string
}

// raise decides the requests the room's state st raises: one for each
// accepted entry still waiting for a second admin, signed by another
// admin, that this machine could second as an admin and has not been
// asked about. It looks at every waiting entry, not only a batch's new
// ones, so an entry left out while a budget was full is raised once
// there is room [F1]. The budgets count pending requests whose entry
// still waits: MaxRaisedRequests in all, and MaxRaisedPerSigner per
// signing admin while the room-wide budget is contended. Past its own
// budget a signer takes only a spare place: one left after every other
// admin of the room but this machine keeps the places of its own budget
// it has not used. The first removal of an admin per signing admin and
// target has an allowance of its own: it passes both budgets and takes
// no place in them, so a person can always be asked to remove an admin
// and such removals waiting do not crowd out other requests. A removal
// of an admin goes first, then the entries in
// arrival order: kept entries by batch and position, then the batch being
// stored in its order (fresh gives each one's position). A pending
// request whose entry no longer waits is closed (stale), except keep, the
// request this batch approves; if its entry waits again, it is raised
// again. Each request records the action the entry would have here and
// its history setting.
func (r *Rooms) raise(ctx context.Context, rl *roomLog, st members.State, look func(string) *core.VerifiedEntry,
	fresh map[string]int, keep string) (raising, error) {
	var out raising
	if !slices.Contains(st.Admins, r.m.ID) {
		return out, nil
	}
	waiting := func(id string) *core.VerifiedEntry {
		if eff, ok := st.Effects[id]; !ok || eff.Applied || eff.Reason != members.ReasonNeedsSecond {
			return nil
		}
		return look(id)
	}
	removesAdmin := func(f core.EntryFields) bool {
		return f.Kind == core.EntryRemove && slices.Contains(st.Admins, f.Target)
	}
	var cands []string
	for id := range st.Effects {
		v := waiting(id)
		if v == nil || v.Author() == r.m.ID {
			continue
		}
		if f := v.Fields(); f.Kind != core.EntryAdmin && f.Kind != core.EntryRemove || f.Target == r.m.ID {
			continue
		}
		cands = append(cands, id)
	}
	if err := hookAt("ingest:requests"); err != nil {
		return out, err
	}
	// The pending ones only (an index lookup): every batch, so that a
	// request whose entry stopped waiting is closed and frees its place.
	pending, err := r.m.st.SecondingRequests(ctx, rl.id, true)
	if err != nil {
		return out, err
	}
	asked := map[string]bool{}
	bySigner := map[string]int{}
	removing := map[[2]string]bool{} // signer, target of a pending removal of an admin
	total := 0
	for _, q := range pending {
		asked[q.Seconds] = true
		v := waiting(q.Seconds)
		if v == nil {
			if q.ID != keep {
				out.stale = append(out.stale, q.ID)
			}
			continue
		}
		// The first removal of an admin per signer and target has its own
		// allowance: it takes no place from the budgets.
		if f := v.Fields(); removesAdmin(f) && !removing[[2]string{v.Author(), f.Target}] {
			removing[[2]string{v.Author(), f.Target}] = true
			continue
		}
		bySigner[v.Author()]++
		total++
	}
	// free reports whether one more ordinary request for signer fits: the
	// room-wide budget has a place, and either signer is under its own
	// budget or the place is spare, left once every other admin that
	// could sign (not this machine) still has its own budget's places.
	free := func(signer string) bool {
		if total >= MaxRaisedRequests {
			return false
		}
		if bySigner[signer] < MaxRaisedPerSigner {
			return true
		}
		reserved := 0
		for _, a := range st.Admins {
			if a != signer && a != r.m.ID && bySigner[a] < MaxRaisedPerSigner {
				reserved += MaxRaisedPerSigner - bySigner[a]
			}
		}
		return total+reserved < MaxRaisedRequests
	}
	if len(cands) == 0 {
		return out, nil
	}
	all, err := r.m.st.SecondingRequests(ctx, rl.id, false)
	if err != nil {
		return out, err
	}
	for _, q := range all {
		// One closed because its entry stopped waiting is not an answer.
		if q.State != store.RequestFailed || q.Reason != ReasonNotWaiting {
			asked[q.Seconds] = true
		}
	}
	type order struct {
		removal bool
		batch   int64
		pos     int
		id      string
	}
	orderOf := func(id string) order {
		o := order{removal: removesAdmin(look(id).Fields()), batch: math.MaxInt64, pos: math.MaxInt, id: id}
		if row, ok := rl.state[id]; ok {
			o.batch, o.pos = row.Batch, row.Pos
		} else if p, ok := fresh[id]; ok {
			o.pos = p
		}
		return o
	}
	keys := make(map[string]order, len(cands))
	for _, id := range cands {
		keys[id] = orderOf(id)
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := keys[cands[i]], keys[cands[j]]
		switch {
		case a.removal != b.removal:
			return a.removal
		case a.batch != b.batch:
			return a.batch < b.batch
		case a.pos != b.pos:
			return a.pos < b.pos
		}
		return a.id < b.id
	})
	for _, id := range cands {
		if asked[id] {
			continue
		}
		v := look(id)
		f, signer := v.Fields(), v.Author()
		if k := [2]string{signer, f.Target}; removesAdmin(f) && !removing[k] {
			removing[k] = true
		} else if !free(signer) {
			continue
		} else {
			bySigner[signer]++
			total++
		}
		asked[id] = true
		q := store.Request{ID: r.newID(), Kind: string(f.Kind), Room: rl.id, Node: f.Target, Name: f.Name,
			Seconds: id, Action: actionOf(st, string(f.Kind), f.Target), Created: r.m.now(), State: store.RequestPending}
		if q.Action == ActionAdmitAdmin {
			q.History = f.History
		}
		q.Role = roleOf(q.Action)
		out.reqs = append(out.reqs, q)
	}
	return out, nil
}

// raiseWaiting stores, in one transaction, what rl's current state raises
// (raise): at a start, after a decision frees a place in a budget, and
// after a failed attempt, at the room's next batch or unchanged pull. A
// failure marks the room so the next one retries. The caller holds mu, or
// is opening the rooms.
func (r *Rooms) raiseWaiting(ctx context.Context, rl *roomLog) error {
	x, err := r.raise(ctx, rl, rl.st, rl.lookup(nil), nil, "")
	if err == nil && (len(x.reqs) > 0 || len(x.stale) > 0) {
		_, err = r.m.st.ApplyLog(ctx, store.LogBatch{Room: rl.id, Gen: rl.gen, Now: r.m.now(), Requests: x.reqs,
			Fail: x.stale, FailReason: ReasonNotWaiting})
		if err == nil {
			r.m.wakes.touch(rl.id)
		}
	}
	rl.raiseDue = err != nil
	return err
}

func (r *Rooms) newID() string {
	var b [16]byte
	r.m.rand(b[:])
	return "q_" + hex.EncodeToString(b[:])
}
