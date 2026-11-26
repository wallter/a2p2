package node

// The joiner's side of the join exchange (DESIGN section 7,
// PROTOCOL 5): preview an invite, accept it once a person here has
// confirmed the preview, poll until the admin's person decides, then pin
// the room by the create entry id the preview showed and apply the room's
// log through Receive. Before the accept is sent the approved preview is
// recorded durably (the terms, the admin's full key, the room's local
// generation), so a restart resumes polling and an invite is used once;
// the secret is kept only in memory, between the preview and the accept.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// ErrWrongKey: the machine at the invite's address is not the admin the
// token names. Nothing was sent to it.
var ErrWrongKey = &UserError{Msg: CodeWrongKey + ": the machine at the invite's address is not the admin the invite names; nothing was sent"}

// Errors of the joiner that a person is shown.
var (
	ErrInviteUsed     = &UserError{"this invite was used on this machine already; ask for a new one", ErrorAlreadyDone}
	ErrNoPreview      = &UserError{Msg: "preview the invite first"}
	ErrRoomChanged    = &UserError{Msg: "the room was left or joined here since the preview; preview the invite again"}
	ErrTermsChanged   = &UserError{Msg: "the admin machine now shows different terms from those approved here; ask for a new invite"}
	ErrNoRecord       = &UserError{Msg: "the invite was accepted before, but this machine has no record of approving it; ask for a new invite"}
	ErrPreviewChanged = &UserError{Msg: "the invite was previewed again since the preview you approved; approve the latest preview"}
)

// Reasons a join ends without the room, besides the server's codes.
const (
	ReasonJoinNotMember = "the room's log was received, but it does not show this machine as a member"
	ReasonJoinMissing   = "the room's log was received, but an entry the admin machine accepted was not accepted here"
	ReasonJoinNoCreate  = "the admin machine's signed log does not start with the room's create entry"
	ReasonJoinLeft      = "the room was left or joined again here during the join"
	ReasonNotAccepted   = "the accept did not reach the admin machine: preview and accept the invite again"
	// ReasonJoinServerNotMember: the room is held here, and the admin
	// machine is not a member of it here, nor do its entries make it one
	// (F7). Nothing it sent was stored.
	ReasonJoinServerNotMember = "this machine holds the room, and the admin machine is not a member of it here; " +
		"nothing it sent was stored. Ask a current member for an invite, or leave the room here and join again"
	// ReasonJoinBadPage: a signed page with more entries or bytes than a
	// page holds (PROTOCOL 5.2), or one that says more follows but does
	// not move past the last page.
	ReasonJoinBadPage = "the admin machine's signed answer breaks the page limits or does not move on"
	// ReasonJoinTooLong: the signed answer of one join, over all its runs
	// since the daemon started, passed maxJoinPages pages or maxJoinBytes
	// bytes, took joinPagingMax of reading, or joinStallMax of reading
	// without a page that brought an entry this machine lacks. What
	// earlier pages committed stays; nothing more is read.
	ReasonJoinTooLong = "the admin machine's signed answer went past what one join reads " +
		"(256 pages, 64 MiB, 15 minutes, or 2 minutes without an entry this machine lacks)"
)

// maxJoinPending and maxJoinPendingBytes bound the entries a join keeps
// in memory, while the answering machine is not yet a member here, before
// it gives up (ReasonJoinServerNotMember). Entries held here already are
// not kept, so only what this machine lacks counts. Variables so tests
// can lower them.
var (
	maxJoinPending      = 4 * MaxJoinEntries
	maxJoinPendingBytes = 4 << 20
)

// The bounds of one join's signed answer, whatever the admin machine
// sends: pages and bytes read, the time reading may take, and the time it
// may take without a page that brings an entry this machine lacks. They
// count over every run (follow) of the join since the daemon started, not
// one run: a run cut short by an error or a 202 does not start them again
// (Joiner.runs). Time counts only while a run reads the answer, so the
// time between runs does not. Past one, the join ends refused
// (ReasonJoinTooLong). Variables so tests can lower them.
var (
	maxJoinPages  = 256
	maxJoinBytes  = 64 << 20
	joinPagingMax = 15 * time.Minute
	joinStallMax  = 2 * time.Minute
)

// joinTryAgain is how many new entries a join keeps, while the admin
// machine is not a member here, before it gives them all to the rooms
// again: each try checks every entry kept, under the rooms lock, so a
// page of one new entry does not cost a check of all of them. The last
// page, and one that passes a limit, are always tried.
const joinTryAgain = MaxJoinEntries / 4

// labelChanged is the reason a join is refused when the room is held here
// under a label other than the one the person approved (F12).
// Both are shown as ShownLabel shows them, so a stored label that reads
// as local is not named as this machine's room.
func (r *Rooms) labelChanged(room, held, approved string) string {
	return "the room is held here as " + Isolate(EscapeDisplay(r.ShownLabel(room, held))) + ", not " +
		Isolate(EscapeDisplay(r.ShownLabel(room, approved))) + " as the preview showed; the label of a room cannot change"
}

// acceptTries is how many times JoinAccept sends an accept that the
// admin machine answers 429 or 503, waiting its Retry-After (at most
// JoinRetryMax) between them.
const acceptTries = 3

// joinCodes are the refusal codes of PROTOCOL 5.3: nothing else a
// server sends is shown.
var joinCodes = []string{CodeBadSecret, CodeDeclined, CodeRevoked, CodeExpired, CodeRemoved}

// knownCode maps a server's error to one of joinCodes, or "refused".
func knownCode(s string) string {
	if slices.Contains(joinCodes, s) {
		return s
	}
	return "refused"
}

// JoinRefused is an invite the admin machine refused or that ended: Code
// is bad_secret, declined, revoked, expired, removed or refused.
type JoinRefused struct{ Code string }

func (e *JoinRefused) Error() string { return "the invite was refused: " + e.Code }

// JoinerOpts configures a Joiner.
type JoinerOpts struct {
	Name  string   // this machine's label, as the admin will list it
	Addrs []string // where members can reach this machine (optional, at most 8)
	// Sleep waits d or until ctx ends; default a timer. Tests inject one
	// that moves their clock.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Joiner joins rooms on this machine.
type Joiner struct {
	r *Rooms
	t JoinTransport
	o JoinerOpts

	mu       sync.Mutex
	previews map[string]*previewed // by invite hash; holds the token only in memory
	// following holds one slot per invite hash: one caller at a time
	// accepts or follows a join, and a caller for another invite never
	// waits behind it.
	following map[string]chan struct{}
	// runs holds, by invite hash, the paging of each join still accepted:
	// the cursor, the entries kept and the bounds, carried from one run of
	// follow to the next. Only memory: a restart starts them again.
	runs map[string]*paging
}

type previewed struct {
	tok     core.Token
	preview Preview
	gen     int64 // the room's local generation when previewed
}

// Preview is what a person on the joiner confirms (PROTOCOL 5.3).
type Preview struct {
	State   string // previewed; accepted when a recorded join is waiting
	Room    string
	Label   string
	Create  string // the create entry id that will be pinned
	Role    string
	History bool
	// Member: this machine is a member already; role and history are
	// what it has, and approval returns the log and changes nothing.
	Member  bool
	Admin   string // the admin's node id, checked against the token
	Expires time.Time
	// ID identifies everything above but State (previewID): JoinAccept
	// takes it, so what is accepted is exactly what the person approved.
	ID string
}

// JoinOutcome is how a join stands: accepted (waiting for a person on the
// admin machine), previewed (the accept never arrived: Reason says so),
// signed (the room is held here), refused (Reason is the code or what
// failed here), expired or removed. A signed join reports the role and
// history permission this machine's own copy of the log gives it; Note
// says so when they differ from the preview (a change that landed since).
type JoinOutcome struct {
	State   string
	Room    string
	Reason  string
	Role    string
	History bool
	Note    string
}

// NewJoiner joins rooms over t.
func NewJoiner(r *Rooms, t JoinTransport, o JoinerOpts) *Joiner {
	if o.Sleep == nil {
		o.Sleep = func(ctx context.Context, d time.Duration) error {
			tm := time.NewTimer(d)
			defer tm.Stop()
			select {
			case <-tm.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return &Joiner{r: r, t: t, o: o, previews: map[string]*previewed{},
		following: map[string]chan struct{}{}, runs: map[string]*paging{}}
}

// hold takes invite h's slot, waiting until it is free or ctx ends; with
// try it does not wait and reports false if the slot is taken.
func (j *Joiner) hold(ctx context.Context, h string, try bool) (release func(), ok bool, err error) {
	j.mu.Lock()
	c := j.following[h]
	if c == nil {
		c = make(chan struct{}, 1)
		j.following[h] = c
	}
	j.mu.Unlock()
	release = func() { <-c }
	if try {
		select {
		case c <- struct{}{}:
			return release, true, nil
		default:
			return nil, false, nil
		}
	}
	select {
	case c <- struct{}{}:
		return release, true, nil
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

// call sends q to one of addrs after check accepted the server's key, and
// decodes the answer strictly. A wrong key stops everything.
func (j *Joiner) call(ctx context.Context, addrs []string, check func(string) error, q JoinRequest) (JoinReply, JoinAnswer, error) {
	q.V = JoinVersion
	if q.Op != JoinPoll {
		q.Name, q.Addrs = j.o.Name, j.o.Addrs
	}
	body, err := json.Marshal(q)
	if err != nil {
		return JoinReply{}, JoinAnswer{}, err
	}
	rp, err := j.t.Join(ctx, addrs, check, body)
	if err != nil {
		if errors.Is(err, ErrWrongKey) {
			return JoinReply{}, JoinAnswer{}, ErrWrongKey
		}
		return JoinReply{}, JoinAnswer{}, err
	}
	var a JoinAnswer
	if core.CheckStrictObject(rp.Body, joinAnswerFields...) != nil || json.Unmarshal(rp.Body, &a) != nil {
		return rp, a, errors.New("join: the admin machine's answer is not strict json")
	}
	a.Error = knownCode(a.Error)
	return rp, a, nil
}

// fingerprintCheck accepts only the key the token names, and remembers it.
func fingerprintCheck(tok core.Token, got *string) func(string) error {
	return func(server string) error {
		fp, ok := core.KeyFingerprint(server)
		if !ok || fp != tok.Fingerprint {
			return ErrWrongKey
		}
		*got = server
		return nil
	}
}

// JoinPreview opens the invite in token: it checks the admin's key against
// the token's fingerprint before sending anything (a mismatch stops with
// ErrWrongKey), sends the secret, and returns the preview for a person to
// confirm. The invite is now bound to this machine's key. An invite this
// machine finished with is refused here (ErrInviteUsed); one it approved
// and is still waiting on may be previewed again, but only on the terms
// approved (ErrTermsChanged). A refusal is a *JoinRefused.
func (j *Joiner) JoinPreview(ctx context.Context, token string) (Preview, error) {
	tok, err := core.DecodeToken(token)
	if err != nil {
		return Preview{}, userErr("bad invite token: %v", err)
	}
	if j.o.Name == "" || !core.ValidLabel(j.o.Name) {
		return Preview{}, userErr("this machine needs a valid name to join")
	}
	h := secretHash(tok.Secret)
	rec, err := j.r.m.st.GetJoin(ctx, h)
	switch {
	case err == nil && rec.State != store.JoinAccepted:
		return Preview{}, ErrInviteUsed
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return Preview{}, err
	}
	recorded := err == nil
	var server string
	rp, a, err := j.call(ctx, tok.Addrs, fingerprintCheck(tok, &server), JoinRequest{Op: JoinOpen, Secret: tok.SecretText()})
	if err != nil {
		return Preview{}, err
	}
	p, err := answerPreview(rp, a, server)
	if err != nil {
		return Preview{}, err
	}
	switch {
	case recorded && p.State == JoinStatePreviewed && !sameTerms(rec, p):
		return Preview{}, ErrTermsChanged
	case recorded && p.State != JoinStatePreviewed:
		p := recordPreview(rec)
		p.State = JoinStateAccepted
		return p, nil
	case p.State != JoinStatePreviewed:
		return Preview{}, ErrNoRecord
	}
	gen, err := j.r.checkPreview(ctx, p)
	if err != nil {
		return Preview{}, err
	}
	if recorded {
		gen, p = rec.Gen, recordPreview(rec)
	}
	j.mu.Lock()
	j.previews[h] = &previewed{tok: tok, preview: p, gen: gen}
	j.mu.Unlock()
	return p, nil
}

func sameTerms(rec store.Join, p Preview) bool { return recordPreview(rec).ID == p.ID }

// recordPreview is the preview a recorded join was approved on.
func recordPreview(rec store.Join) Preview {
	p := Preview{State: JoinStatePreviewed, Room: rec.Room, Label: rec.Label, Create: rec.CreateID, Role: rec.Role,
		History: rec.History, Member: rec.Member, Admin: rec.Admin, Expires: rec.Expires}
	p.ID = previewID(p)
	return p
}

// answerPreview reads an open's answer.
func answerPreview(rp JoinReply, a JoinAnswer, server string) (Preview, error) {
	switch {
	case rp.Status == http.StatusTooManyRequests:
		return Preview{}, codedErr(ErrorRateLimited, "the admin machine is limiting invites; try again in %s", rp.RetryAfter.Round(time.Second))
	case rp.Status == http.StatusForbidden:
		return Preview{}, &JoinRefused{Code: a.Error}
	case rp.Status == http.StatusAccepted && a.State == JoinStateAccepted,
		rp.Status == http.StatusOK && a.State == JoinStateSigned:
		return Preview{State: a.State}, nil
	case rp.Status != http.StatusOK || a.State != JoinStatePreviewed || !validJoinRoom(a.Room):
		return Preview{}, errors.New("join: unexpected answer from the admin machine")
	}
	exp, err := time.Parse(timeLayout, a.Expires)
	if err != nil || a.Admin != server || a.Role != RoleMember && a.Role != RoleAdmin {
		return Preview{}, errors.New("join: the preview is malformed or names another admin")
	}
	p := Preview{State: JoinStatePreviewed, Room: a.Room.ID, Label: a.Room.Label, Create: a.Room.Create,
		Role: a.Role, History: a.History, Member: a.Member, Admin: a.Admin, Expires: exp}
	p.ID = previewID(p)
	return p, nil
}

// checkPreview refuses a preview a person here must not be asked to
// approve, and returns the room's generation here, read with the same
// view of the rooms (a pin or a leave holds r.mu while it writes). For a
// room held here, the admin machine must name the create entry and the
// label this machine holds: a different label is shown as a change, not
// as the room's name; a room held here whose label reads as local, which
// an older version could store, is not offered again under that name.
// For any other room, the label must pass remoteLabelProblem.
func (r *Rooms) checkPreview(ctx context.Context, p Preview) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rl := r.rooms[p.Room]
	switch {
	case rl == nil:
		if why := remoteLabelProblem(p.Label); why != "" {
			return 0, userErr("this invite's room cannot be joined here: %s", why)
		}
	case rl.createID != p.Create:
		return 0, userErr("this machine holds room %s with another create entry; the invite cannot be used here", p.Room)
	case rl.label != p.Label:
		return 0, userErr("the admin machine names room %s %s, but it is held here as %s; the label of a room cannot change, so the invite is refused",
			p.Room, Isolate(EscapeDisplay(p.Label)), Isolate(EscapeDisplay(r.ShownLabel(p.Room, rl.label))))
	case p.Room != localRoomID(r.m.ID) && reservedLocal(rl.label):
		return 0, userErr("room %s is held here with a label that reads as %s, the name of this machine's own room; "+
			"it still works here, but an invite cannot be used for it", p.Room, LocalLabel)
	}
	gen, _, err := r.m.st.RoomGen(ctx, p.Room)
	return gen, err
}

// unheld is wires without the entries room holds here already and those
// that do not parse, with the id of each: what a join must keep to give
// again. It also counts the entries of wires held here, and all those the
// room holds.
func (r *Rooms) unheld(room string, wires [][]byte) (out [][]byte, ids []string, held, holds int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rl := r.rooms[room]
	if rl != nil {
		holds = len(rl.state)
	}
	for _, w := range wires {
		e, err := core.ParseEntry(w)
		if err != nil {
			continue
		}
		if rl != nil {
			if _, kept := rl.state[e.ID()]; kept {
				held++
				continue
			}
		}
		out, ids = append(out, w), append(ids, e.ID())
	}
	return out, ids, held, holds
}

func validJoinRoom(r *JoinRoom) bool {
	return r != nil && core.ValidRoom(r.ID) && len(r.Create) == 32 && isLowerHex(r.Create)
}

// JoinAccept accepts the invite in token on the preview preview (its ID),
// which JoinPreview showed to a person here who confirmed it; the local
// surface calls it only after that confirmation. It fails with
// ErrPreviewChanged unless that preview is the one stored here: the
// latest preview, or the one an earlier accept recorded, which nothing
// replaces. It first records the approved preview, then sends the accept
// carrying its identifier; the admin machine then raises a request for
// its person. An accept answered 429 or 503 is sent again after the
// answer's Retry-After, up to acceptTries in all. JoinAccept polls for up to wait (PROTOCOL 5.3) and
// returns how the join stands; a join still accepted keeps being followed
// by Resume. If the accept's answer is lost, calling JoinAccept again is
// safe.
func (j *Joiner) JoinAccept(ctx context.Context, token, preview string, wait time.Duration) (JoinOutcome, error) {
	tok, err := core.DecodeToken(token)
	if err != nil {
		return JoinOutcome{}, userErr("bad invite token: %v", err)
	}
	h := secretHash(tok.Secret)
	release, _, err := j.hold(ctx, h, false)
	if err != nil {
		return JoinOutcome{}, err
	}
	defer release()
	j.mu.Lock()
	pv := j.previews[h]
	j.mu.Unlock()
	if pv == nil {
		// Accepted before (its answer lost, or a restart since): the
		// record holds the terms approved.
		rec, err := j.r.m.st.GetJoin(ctx, h)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return JoinOutcome{}, ErrNoPreview
		case err != nil:
			return JoinOutcome{}, err
		case rec.State != store.JoinAccepted:
			return JoinOutcome{}, ErrInviteUsed
		}
		pv = &previewed{tok: tok, gen: rec.Gen, preview: recordPreview(rec)}
	}
	p := pv.preview
	if p.ID != preview {
		return JoinOutcome{}, ErrPreviewChanged
	}
	if gen, _, err := j.r.m.st.RoomGen(ctx, p.Room); err != nil {
		return JoinOutcome{}, err
	} else if gen != pv.gen {
		return JoinOutcome{}, ErrRoomChanged
	}
	now := j.r.m.now()
	rec, _, err := j.r.m.st.AddJoin(ctx, store.Join{Invite: h, Room: p.Room, Label: p.Label, CreateID: p.Create,
		Role: p.Role, History: p.History, Member: p.Member, Admin: p.Admin, Addrs: tok.Addrs, Gen: pv.gen,
		Expires: p.Expires, Created: now})
	switch {
	case err != nil:
		return JoinOutcome{}, err
	case rec.State != store.JoinAccepted:
		return JoinOutcome{}, ErrInviteUsed
	case !sameTerms(rec, p):
		return JoinOutcome{}, ErrTermsChanged
	}
	check := func(server string) error {
		if server != rec.Admin {
			return ErrWrongKey
		}
		return nil
	}
	var rp JoinReply
	var a JoinAnswer
	for try := 1; ; try++ {
		rp, a, err = j.call(ctx, tok.Addrs, check, JoinRequest{Op: JoinAccept, Secret: tok.SecretText(), Terms: p.ID})
		if err != nil {
			return JoinOutcome{}, err // the record stays; JoinAccept again, or Resume, finds out
		}
		if rp.Status != http.StatusTooManyRequests && rp.Status != http.StatusServiceUnavailable {
			break
		}
		// Limited or busy: the accept was not taken. Wait as the admin
		// machine says, and send it again.
		if try == acceptTries {
			return JoinOutcome{}, codedErr(ErrorRateLimited,
				"the admin machine is limiting joins; accept again in %s", max(rp.RetryAfter, time.Second).Round(time.Second))
		}
		if err := j.o.Sleep(ctx, min(max(rp.RetryAfter, time.Second), JoinRetryMax)); err != nil {
			return JoinOutcome{}, err
		}
	}
	switch {
	case rp.Status == http.StatusForbidden:
		j.forget(h)
		return j.end(ctx, rec, refusedOutcome(rec.Room, a.Error))
	case rp.Status == http.StatusAccepted, rp.Status == http.StatusOK && a.State == JoinStateSigned:
	default:
		return JoinOutcome{}, errors.New("join: unexpected answer from the admin machine")
	}
	j.forget(h) // the secret is not needed any more: polls name the invite by its hash
	return j.follow(ctx, rec, wait)
}

func (j *Joiner) forget(h string) {
	j.mu.Lock()
	delete(j.previews, h)
	j.mu.Unlock()
}

// Resume follows every recorded join that has no outcome yet, for up to
// wait each (a restart calls it), and returns how each stands.
func (j *Joiner) Resume(ctx context.Context, wait time.Duration) ([]JoinOutcome, error) {
	js, err := j.r.m.st.Joins(ctx, store.JoinAccepted)
	if err != nil {
		return nil, err
	}
	j.prune(ctx, js)
	var out []JoinOutcome
	for _, jn := range js {
		release, ok, _ := j.hold(ctx, jn.Invite, true)
		if !ok {
			continue // an accept is following it now
		}
		o, err := j.follow(ctx, jn, wait)
		release()
		if err != nil {
			return out, err
		}
		out = append(out, o)
	}
	return out, nil
}

// prune drops the paging of joins that ended without follow seeing it
// (the room was left), keeping those of js and those a caller follows.
func (j *Joiner) prune(ctx context.Context, js []store.Join) {
	j.mu.Lock()
	var gone []string
	for h := range j.runs {
		if !slices.ContainsFunc(js, func(jn store.Join) bool { return jn.Invite == h }) {
			gone = append(gone, h)
		}
	}
	j.mu.Unlock()
	for _, h := range gone {
		if release, ok, _ := j.hold(ctx, h, true); ok {
			j.mu.Lock()
			delete(j.runs, h)
			j.mu.Unlock()
			release()
		}
	}
}

func refusedOutcome(room, code string) JoinOutcome {
	switch code {
	case CodeExpired:
		return JoinOutcome{State: JoinStateExpired, Room: room, Reason: code}
	case CodeRemoved:
		return JoinOutcome{State: JoinStateRemoved, Room: room, Reason: code}
	}
	return JoinOutcome{State: JoinStateRefused, Room: room, Reason: code}
}

// end records outcome o of jn. A join ended meanwhile (the room was left
// here) reports that instead.
func (j *Joiner) end(ctx context.Context, jn store.Join, o JoinOutcome) (JoinOutcome, error) {
	st := map[string]string{JoinStateSigned: store.JoinSigned, JoinStateExpired: store.JoinExpired,
		JoinStateRemoved: store.JoinRemoved, JoinStateRefused: store.JoinRefused}[o.State]
	err := j.r.m.st.EndJoin(ctx, jn.Invite, st, o.Reason, j.r.m.now())
	if errors.Is(err, store.ErrNotFound) {
		return JoinOutcome{State: JoinStateRefused, Room: jn.Room, Reason: ReasonJoinLeft}, nil
	}
	if err != nil {
		return JoinOutcome{}, err
	}
	return o, nil
}

// follow polls the admin machine for jn until an outcome, or until wait
// has passed (then the join stays accepted). It polls every retry_ms,
// doubling to JoinRetryMax. The caller holds jn's slot. The paging of the
// join, its cursor and bounds, carries over to the next run while the
// join stays accepted, and is dropped once it ends.
func (j *Joiner) follow(ctx context.Context, jn store.Join, wait time.Duration) (JoinOutcome, error) {
	j.mu.Lock()
	pg := j.runs[jn.Invite]
	if pg == nil {
		pg = &paging{}
		j.runs[jn.Invite] = pg
	}
	j.mu.Unlock()
	o, err := j.run(ctx, jn, pg, wait)
	pg.stop(j.r.m.now())
	// A join answered previewed stays recorded as accepted, so its paging
	// stays too: an answer cannot start the bounds again by saying so.
	if err == nil && o.State != JoinStateAccepted && o.State != JoinStatePreviewed {
		j.mu.Lock()
		delete(j.runs, jn.Invite)
		j.mu.Unlock()
	}
	return o, err
}

// run is one run of follow, with the join's paging pg.
func (j *Joiner) run(ctx context.Context, jn store.Join, pg *paging, wait time.Duration) (JoinOutcome, error) {
	// A failed run leaves only a room its own pin made: between runs the
	// room may have become this machine's another way (another join).
	pg.pinned = false
	deadline := j.r.m.now().Add(wait)
	delay := JoinRetry
	check := func(server string) error {
		if server != jn.Admin {
			return ErrWrongKey
		}
		return nil
	}
	ended := func() (bool, error) {
		cur, err := j.r.m.st.GetJoin(ctx, jn.Invite)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
		if err == nil {
			jn.PinnedGen = cur.PinnedGen
		}
		return err != nil || cur.State != store.JoinAccepted, nil
	}
	left := JoinOutcome{State: JoinStateRefused, Room: jn.Room, Reason: ReasonJoinLeft}
	fail := func(reason string) (JoinOutcome, error) {
		o, err := j.end(ctx, jn, JoinOutcome{State: JoinStateRefused, Room: jn.Room, Reason: reason})
		if err == nil && pg.pinned {
			// Nothing stays installed by a join that failed.
			if err := j.r.Leave(ctx, jn.Room); err != nil && !errors.Is(err, ErrNoRoom) {
				return JoinOutcome{}, err
			}
		}
		return o, err
	}
	for {
		if done, err := ended(); err != nil || done {
			return left, err
		}
		sent := j.r.m.now()
		rp, a, err := j.call(ctx, jn.Addrs, check, JoinRequest{Op: JoinPoll, Invite: jn.Invite, After: pg.after})
		var pause time.Duration
		switch {
		case errors.Is(err, ErrWrongKey):
			return JoinOutcome{}, err
		case ctx.Err() != nil:
			return JoinOutcome{}, ctx.Err()
		case err != nil:
			pause = delay // unreachable or a bad answer: try again later
		case rp.Status == http.StatusForbidden:
			return j.end(ctx, jn, refusedOutcome(jn.Room, a.Error))
		case rp.Status == http.StatusTooManyRequests, rp.Status == http.StatusServiceUnavailable:
			pause = max(rp.RetryAfter, time.Second)
		case rp.Status == http.StatusOK && a.State == JoinStatePreviewed:
			// The accept never arrived (lost before it was sent, or its
			// request was lost): the person must accept again.
			p, err := answerPreview(rp, a, jn.Admin)
			if err != nil || !sameTerms(jn, p) {
				return j.end(ctx, jn, JoinOutcome{State: JoinStateRefused, Room: jn.Room, Reason: ErrTermsChanged.Msg})
			}
			return JoinOutcome{State: JoinStatePreviewed, Room: jn.Room, Reason: ReasonNotAccepted}, nil
		case rp.Status == http.StatusAccepted:
			if a.RetryMS > 0 && time.Duration(a.RetryMS)*time.Millisecond > delay {
				delay = min(time.Duration(a.RetryMS)*time.Millisecond, JoinRetryMax)
			}
			pause = delay
		case rp.Status == http.StatusOK && a.State == JoinStateSigned:
			// A page marked more is read at once, past wait: the answer
			// has bounds of its own over all runs (ReasonJoinTooLong).
			if pg.from.IsZero() {
				pg.from = sent // the clock runs from this run's first page
			}
			if pg.pages == 0 {
				pg.moved = pg.read(j.r.m.now())
			}
			pg.pages++
			pg.bytes += len(rp.Body)
			if pg.pages > maxJoinPages || pg.bytes > maxJoinBytes {
				return fail(ReasonJoinTooLong)
			}
			reason, err := j.apply(ctx, jn, pg, a)
			switch {
			case err != nil:
				return JoinOutcome{}, err
			case reason == ReasonJoinLeft:
				return left, nil
			case reason != "":
				return fail(reason)
			case a.More:
				if d := pg.read(j.r.m.now()); d > joinPagingMax || d-pg.moved > joinStallMax {
					return fail(ReasonJoinTooLong)
				}
				continue
			}
			o, reason := j.verify(jn, pg, a.Heads)
			switch {
			case reason == ReasonJoinLeft:
				return left, nil
			case reason != "":
				return fail(reason)
			}
			return j.end(ctx, jn, o)
		default:
			pause = delay
		}
		if j.r.m.now().Add(pause).After(deadline) {
			// A poll that came back after the room was left: ended, not pending.
			if done, err := ended(); err != nil || done {
				return left, err
			}
			return JoinOutcome{State: JoinStateAccepted, Room: jn.Room}, nil
		}
		if err := j.o.Sleep(ctx, pause); err != nil {
			return JoinOutcome{}, err
		}
		if pause == delay {
			delay = min(2*delay, JoinRetryMax)
		}
	}
}

// paging is the reading of one join's signed log, over its runs.
type paging struct {
	after  int64 // the last page's next
	gen    int64 // the generation the room is held at, once pinned or found
	pinned bool  // this run's pin made it; each run starts without it
	// held: the room was held here before this join, so the admin
	// machine's entries count only if it is a member here or they make it
	// one (receiveJoin); pending are the entries kept until they do.
	held    bool
	pending [][]byte
	kept    map[string]bool // the ids of pending
	fresh   int             // entries kept since pending was last given
	// The bounds of the join over its runs: signed pages and bytes read;
	// the time spent reading in runs that ended (spent), and since from in
	// the current one (zero between runs); the reading time at the last
	// page that moved the join on (moved); and how many entries held here
	// the pages brought (again).
	pages, bytes int
	spent, moved time.Duration
	from         time.Time
	again        int
}

// read is the time spent reading the answer so far, at now.
func (pg *paging) read(now time.Time) time.Duration {
	if pg.from.IsZero() {
		return pg.spent
	}
	return pg.spent + now.Sub(pg.from)
}

// stop ends the current run's clock at now.
func (pg *paging) stop(now time.Time) {
	pg.spent, pg.from = pg.read(now), time.Time{}
}

// keep adds to pending the entries of wires (with ids) it does not hold
// yet, and reports how many it added.
func (pg *paging) keep(wires [][]byte, ids []string) int {
	if pg.kept == nil {
		pg.kept = map[string]bool{}
	}
	n := 0
	for i, w := range wires {
		if !pg.kept[ids[i]] {
			pg.kept[ids[i]] = true
			pg.pending = append(pg.pending, w)
			n++
		}
	}
	pg.fresh += n
	return n
}

// over reports whether pending passes maxJoinPending or maxJoinPendingBytes.
func (pg *paging) over() bool {
	size := 0
	for _, w := range pg.pending {
		size += len(w)
	}
	return len(pg.pending) > maxJoinPending || size > maxJoinPendingBytes
}

// pageProblem says why a signed page breaks the paging rules (PROTOCOL
// 5.2), or "": at most MaxJoinEntries entries and maxJoinPageBytes bytes,
// as a server of any version pages them, and a page that says more
// follows must hold entries and move past the last one.
func pageProblem(pg *paging, a JoinAnswer) string {
	size := 0
	for _, e := range a.Entries {
		size += len(e) + 1
	}
	switch {
	case len(a.Entries) > MaxJoinEntries, size > maxJoinPageBytes:
		return ReasonJoinBadPage
	case a.More && (len(a.Entries) == 0 || a.Next <= pg.after):
		return ReasonJoinBadPage
	}
	return ""
}

// apply takes one signed page. The first time, it finds the generation to
// apply under: the one this join's own pin recorded (a run that pinned and
// was interrupted); or the room held at the generation recorded with the
// preview, with that create entry (a machine restored from a copy, or one
// still holding it); or else it pins the room by the create entry id the
// preview showed, after checking the entry, in one write that checks the
// join is still accepted and the generation still the one recorded, and
// takes the generation from that write. It then gives the entries to
// Receive as one batch under that generation. It returns what failed, or
// "".
func (j *Joiner) apply(ctx context.Context, jn store.Join, pg *paging, a JoinAnswer) (string, error) {
	if !validJoinRoom(a.Room) || a.Room.ID != jn.Room || a.Room.Create != jn.CreateID {
		return "the admin machine's signed answer names another room", nil
	}
	if why := pageProblem(pg, a); why != "" {
		return why, nil
	}
	wires := make([][]byte, len(a.Entries))
	for i, e := range a.Entries {
		wires[i] = e
	}
	if pg.gen == 0 {
		j.r.mu.Lock()
		gen, held, err := j.r.m.st.RoomGen(ctx, jn.Room)
		rl := j.r.rooms[jn.Room]
		sameRoom := rl != nil && rl.createID == jn.CreateID
		heldLabel := ""
		if rl != nil {
			heldLabel = rl.label
		}
		j.r.mu.Unlock()
		if err != nil {
			return "", err
		}
		switch {
		case jn.PinnedGen != 0 && sameRoom && heldLabel != jn.Label:
			// Pinned by an earlier run under a version that did not
			// compare the labels: the room stays, the join goes no further.
			return j.r.labelChanged(jn.Room, heldLabel, jn.Label), nil
		case jn.PinnedGen != 0:
			pg.gen = jn.PinnedGen // receiveJoin refuses it if the room was left since
		case held && sameRoom && gen == jn.Gen && heldLabel != jn.Label:
			// A join recorded by an older version, or a preview raced by a pin.
			return j.r.labelChanged(jn.Room, heldLabel, jn.Label), nil
		case held && sameRoom && gen == jn.Gen:
			pg.gen, pg.held = gen, true
		case held:
			return ReasonJoinLeft, nil
		default:
			if pg.after != 0 || len(wires) == 0 {
				return ReasonJoinNoCreate, nil
			}
			e, err := core.ParseEntry(wires[0])
			if err != nil || e.ID() != jn.CreateID || e.Fields().Room != jn.Room || e.Fields().Kind != core.EntryCreate {
				return ReasonJoinNoCreate, nil
			}
			// The label pinned is the one the person approved (F12).
			if l := e.Fields().Label; l != jn.Label {
				return "the room's signed create entry labels it " + Isolate(EscapeDisplay(l)) +
					", not " + Isolate(EscapeDisplay(jn.Label)) + " as the preview showed", nil
			}
			if why := remoteLabelProblem(jn.Label); why != "" {
				return "the room cannot be held here: " + why, nil
			}
			if err := hookAt("join:pin"); err != nil {
				return "", err
			}
			gen, err := j.r.pinJoin(ctx, wires[0], jn.Invite, jn.Gen)
			if errors.Is(err, store.ErrStaleGeneration) {
				return ReasonJoinLeft, nil
			}
			if err != nil {
				return "", err
			}
			pg.gen, pg.pinned = gen, true
		}
	}
	// A room held here, or one this machine is a member of already: the
	// answer counts only from a member here, or once it makes the admin
	// machine one (F7, receiveJoin). Entries that do not are kept, without
	// those held here or kept already, within maxJoinPending and
	// maxJoinPendingBytes. Once some are kept, they are given again when
	// joinTryAgain more are, on the last page, and before a limit ends the
	// join; a page that brings nothing new is not given again at all.
	// A page moves the join on if it brings an entry this machine lacks,
	// or entries it holds while those pages brought no more of them than
	// it holds: a log read again from the start after a restart moves on,
	// a page of held entries sent over and over does not for long.
	lack, ids, held, holds := j.r.unheld(jn.Room, wires)
	pg.again += held
	if slices.ContainsFunc(ids, func(id string) bool { return !pg.kept[id] }) || held > 0 && pg.again <= holds {
		pg.moved = pg.read(j.r.m.now())
	}
	var err error
	if len(pg.pending) == 0 {
		_, err = j.r.receiveJoin(ctx, jn.Room, pg.gen, jn.Admin, pg.held, wires...)
		if errors.Is(err, errServerNotMember) {
			pg.keep(lack, ids)
			pg.fresh = 0 // all of it was just tried
			if !a.More || pg.over() {
				return ReasonJoinServerNotMember, nil
			}
			err = nil
		}
	} else {
		pg.keep(lack, ids)
		over := pg.over()
		if !a.More || over || pg.fresh >= joinTryAgain {
			_, err = j.r.receiveJoin(ctx, jn.Room, pg.gen, jn.Admin, pg.held, pg.pending...)
			switch {
			case errors.Is(err, errServerNotMember) && (!a.More || over):
				return ReasonJoinServerNotMember, nil
			case errors.Is(err, errServerNotMember):
				pg.fresh, err = 0, nil
			case err == nil:
				pg.pending, pg.kept, pg.fresh = nil, nil, 0
			}
		}
	}
	if errors.Is(err, ErrStaleGeneration) || errors.Is(err, ErrNoRoom) {
		return ReasonJoinLeft, nil
	}
	if err != nil {
		return "", err
	}
	if len(a.Entries) > 0 {
		pg.after = a.Next
	}
	return "", nil
}

// verify, after the last page. The log is the truth: this machine must
// have accepted every head the admin machine advertised (otherwise an
// entry it accepted was refused or never arrived here) and be a member of
// the pinned room in its own state. The join lifts the not_caught_up
// gate only if those heads are this log's heads. The outcome then
// reports the role and history permission that state gives, with a note
// if they differ from the preview.
func (j *Joiner) verify(jn store.Join, pg *paging, heads []string) (JoinOutcome, string) {
	j.r.mu.Lock()
	defer j.r.mu.Unlock()
	rl := j.r.rooms[jn.Room]
	if rl == nil || rl.gen != pg.gen {
		return JoinOutcome{}, ReasonJoinLeft
	}
	for _, h := range heads {
		if rl.state[h].State != store.LogAccepted {
			return JoinOutcome{}, ReasonJoinMissing
		}
	}
	self := j.r.m.ID
	st := rl.st
	if !slices.Contains(st.Members, self) {
		return JoinOutcome{}, ReasonJoinNotMember
	}
	// The heads of the admin machine's last page are the heads of the log
	// here, every one and nothing else, and it is a member here: the
	// room's log is caught up (maySign). An answer with no heads, from a
	// machine that is not a member, or whose heads are older entries this
	// log has gone past (a list of ancestors that hides newer entries)
	// opens nothing (F7). A log here ahead of the admin machine's waits
	// for a pull.
	if len(heads) > 0 && slices.Contains(st.Members, jn.Admin) && sameSet(heads, acceptedHeads(rl)) {
		rl.pulled = true
	}
	o := JoinOutcome{State: JoinStateSigned, Room: jn.Room, Role: RoleMember, History: st.Permissions[self].All}
	if slices.Contains(st.Admins, self) {
		o.Role = RoleAdmin
	}
	if o.Role != jn.Role || o.History != jn.History {
		o.Note = fmt.Sprintf("the room's log gives this machine the role %s and %s, not what the preview showed (%s, %s): "+
			"a change was made since", o.Role, historyText(o.History), jn.Role, historyText(jn.History))
	}
	return o, ""
}

func historyText(h bool) string {
	if h {
		return "earlier history"
	}
	return "no earlier history"
}
