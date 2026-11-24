package node

// The joiner's side of the join exchange (DESIGN-next section 7,
// PROTOCOL-next 5): preview an invite, accept it once a person here has
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
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/members"
	"github.com/wallter/a2p2/internal/store"
)

// ErrWrongKey: the machine at the invite's address is not the admin the
// token names. Nothing was sent to it.
var ErrWrongKey = &UserError{CodeWrongKey + ": the machine at the invite's address is not the admin the invite names; nothing was sent"}

// Errors of the joiner that a person is shown.
var (
	ErrInviteUsed   = &UserError{"this invite was used on this machine already; ask for a new one"}
	ErrNoPreview    = &UserError{"preview the invite first"}
	ErrRoomChanged  = &UserError{"the room was left or joined here since the preview; preview the invite again"}
	ErrTermsChanged = &UserError{"the admin machine now shows different terms from those approved here; ask for a new invite"}
	ErrNoRecord     = &UserError{"the invite was accepted before, but this machine has no record of approving it; ask for a new invite"}
)

// Reasons a join ends without the room, besides the server's codes.
const (
	ReasonJoinNotMember = "the room's log was received, but it does not show this machine as a member on the terms approved"
	ReasonJoinConflict  = "this machine holds room log entries that conflict with the admin machine's log"
	ReasonJoinLeft      = "the room was left or joined again here during the join"
	ReasonNotAccepted   = "the accept did not reach the admin machine: preview and accept the invite again"
)

// joinCodes are the refusal codes of PROTOCOL-next 5.3: nothing else a
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
}

type previewed struct {
	tok     core.Token
	preview Preview
	gen     int64 // the room's local generation when previewed
}

// Preview is what a person on the joiner confirms (PROTOCOL-next 5.3).
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
}

func (p Preview) terms() store.Terms {
	return store.Terms{Member: p.Member, Role: p.Role, History: p.History}
}

// JoinOutcome is how a join stands: accepted (waiting for a person on the
// admin machine), previewed (the accept never arrived: Reason says so),
// signed (the room is held here), refused (Reason is the code or what
// failed here), expired or removed.
type JoinOutcome struct {
	State  string
	Room   string
	Reason string
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
	return &Joiner{r: r, t: t, o: o, previews: map[string]*previewed{}}
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
		return Preview{State: JoinStateAccepted, Room: rec.Room, Label: rec.Label, Create: rec.CreateID, Role: rec.Role,
			History: rec.History, Member: rec.Member, Admin: rec.Admin, Expires: rec.Expires}, nil
	case p.State != JoinStatePreviewed:
		return Preview{}, ErrNoRecord
	}
	gen, _, err := j.r.m.st.RoomGen(ctx, p.Room)
	if err != nil {
		return Preview{}, err
	}
	if recorded {
		gen = rec.Gen
	}
	j.mu.Lock()
	j.previews[h] = &previewed{tok: tok, preview: p, gen: gen}
	j.mu.Unlock()
	return p, nil
}

func sameTerms(rec store.Join, p Preview) bool {
	return rec.Room == p.Room && rec.CreateID == p.Create && rec.Admin == p.Admin && rec.Terms() == p.terms()
}

// answerPreview reads an open's answer.
func answerPreview(rp JoinReply, a JoinAnswer, server string) (Preview, error) {
	switch {
	case rp.Status == http.StatusTooManyRequests:
		return Preview{}, userErr("the admin machine is limiting invites; try again in %s", rp.RetryAfter.Round(time.Second))
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
	return Preview{State: JoinStatePreviewed, Room: a.Room.ID, Label: a.Room.Label, Create: a.Room.Create,
		Role: a.Role, History: a.History, Member: a.Member, Admin: a.Admin, Expires: exp}, nil
}

func validJoinRoom(r *JoinRoom) bool {
	return r != nil && core.ValidRoom(r.ID) && len(r.Create) == 32 && isLowerHex(r.Create)
}

// JoinAccept accepts the invite in token, which JoinPreview showed to a
// person here who confirmed it; the local surface calls it only after that
// confirmation. It first records the approved preview, then sends the
// accept; the admin machine then raises a request for its person.
// JoinAccept polls for up to wait (PROTOCOL-next 5.3) and returns how the
// join stands; a join still accepted keeps being followed by Resume. If
// the accept's answer is lost, calling JoinAccept again is safe.
func (j *Joiner) JoinAccept(ctx context.Context, token string, wait time.Duration) (JoinOutcome, error) {
	tok, err := core.DecodeToken(token)
	if err != nil {
		return JoinOutcome{}, userErr("bad invite token: %v", err)
	}
	h := secretHash(tok.Secret)
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
		pv = &previewed{tok: tok, gen: rec.Gen, preview: Preview{State: JoinStatePreviewed, Room: rec.Room, Label: rec.Label,
			Create: rec.CreateID, Role: rec.Role, History: rec.History, Member: rec.Member, Admin: rec.Admin, Expires: rec.Expires}}
	}
	p := pv.preview
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
	rp, a, err := j.call(ctx, tok.Addrs, check, JoinRequest{Op: JoinAccept, Secret: tok.SecretText()})
	if err != nil {
		return JoinOutcome{}, err // the record stays; JoinAccept again, or Resume, finds out
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
	var out []JoinOutcome
	for _, jn := range js {
		o, err := j.follow(ctx, jn, wait)
		if err != nil {
			return out, err
		}
		out = append(out, o)
	}
	return out, nil
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
// doubling to JoinRetryMax.
func (j *Joiner) follow(ctx context.Context, jn store.Join, wait time.Duration) (JoinOutcome, error) {
	deadline := j.r.m.now().Add(wait)
	delay := JoinRetry
	check := func(server string) error {
		if server != jn.Admin {
			return ErrWrongKey
		}
		return nil
	}
	pg := &paging{}
	for {
		if cur, err := j.r.m.st.GetJoin(ctx, jn.Invite); err != nil || cur.State != store.JoinAccepted {
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return JoinOutcome{}, err
			}
			return JoinOutcome{State: JoinStateRefused, Room: jn.Room, Reason: ReasonJoinLeft}, nil
		}
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
			if reason, err := j.apply(ctx, jn, pg, a); err != nil || reason != "" {
				if err != nil {
					return JoinOutcome{}, err
				}
				return j.end(ctx, jn, JoinOutcome{State: JoinStateRefused, Room: jn.Room, Reason: reason})
			}
			if a.More {
				continue
			}
			if reason := j.verify(jn, pg); reason != "" {
				return j.end(ctx, jn, JoinOutcome{State: JoinStateRefused, Room: jn.Room, Reason: reason})
			}
			return j.end(ctx, jn, JoinOutcome{State: JoinStateSigned, Room: jn.Room})
		default:
			pause = delay
		}
		if j.r.m.now().Add(pause).After(deadline) {
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

// paging is one run of reading a signed log.
type paging struct {
	after int64
	gen   int64           // the generation the room is held at, once pinned or found
	sent  map[string]bool // every entry the admin machine sent
	bad   string          // what failed in a page
}

// apply takes one signed page. The first time, it checks that the room
// here is as it was when the preview was approved: never held (or left)
// at the generation recorded, so it pins the room by the create entry id
// the preview showed, after checking the entry's room; or held at that
// generation with that create entry (a machine restored from a copy), so
// the entries go into what it holds. It then gives the entries to Receive
// as one batch under that generation and checks what Receive did. It
// returns what failed, or "".
func (j *Joiner) apply(ctx context.Context, jn store.Join, pg *paging, a JoinAnswer) (string, error) {
	if !validJoinRoom(a.Room) || a.Room.ID != jn.Room || a.Room.Create != jn.CreateID {
		return "the admin machine's signed answer names another room", nil
	}
	wires := make([][]byte, len(a.Entries))
	for i, e := range a.Entries {
		wires[i] = e
	}
	if pg.sent == nil {
		pg.sent = map[string]bool{}
	}
	if pg.gen == 0 {
		gen, held, err := j.r.m.st.RoomGen(ctx, jn.Room)
		if err != nil {
			return "", err
		}
		j.r.mu.Lock()
		rl := j.r.rooms[jn.Room]
		sameRoom := rl != nil && rl.createID == jn.CreateID
		j.r.mu.Unlock()
		switch {
		// Held at the generation previewed (a member restored from a
		// copy), or at the next one, which only this join's own pin makes:
		// a leave moves the generation on and marks this join left.
		case held && sameRoom && (gen == jn.Gen || gen == jn.Gen+1):
			pg.gen = gen
		case held || gen != jn.Gen:
			return ReasonJoinLeft, nil
		default:
			if pg.after != 0 || len(wires) == 0 {
				return "the admin machine's signed log does not start with the room's create entry", nil
			}
			e, err := core.ParseEntry(wires[0])
			if err != nil || e.ID() != jn.CreateID || e.Fields().Room != jn.Room || e.Fields().Kind != core.EntryCreate {
				return "the admin machine's signed log does not start with the room's create entry", nil
			}
			if _, err := j.r.PinRoom(ctx, wires[0]); err != nil {
				return "", err
			}
			if pg.gen, err = j.r.Gen(jn.Room); err != nil {
				return "", err
			}
		}
	}
	res, err := j.r.Receive(ctx, jn.Room, pg.gen, wires...)
	if errors.Is(err, ErrStaleGeneration) || errors.Is(err, ErrNoRoom) {
		return ReasonJoinLeft, nil
	}
	if err != nil {
		return "", err
	}
	for _, x := range res {
		if x.ID != "" {
			pg.sent[x.ID] = true
		}
		if x.Disposition == members.Dropped {
			return "an entry of the room's log could not be kept here: " + x.Reason, nil
		}
	}
	pg.after += int64(len(a.Entries))
	return "", nil
}

// verify, after the last page: everything accepted here came from the
// admin machine (a restored copy holds no conflicting branch), and this
// machine is a member of the pinned room on the terms approved.
func (j *Joiner) verify(jn store.Join, pg *paging) string {
	j.r.mu.Lock()
	defer j.r.mu.Unlock()
	rl := j.r.rooms[jn.Room]
	if rl == nil || rl.gen != pg.gen {
		return ReasonJoinLeft
	}
	for id, row := range rl.state {
		if row.State == store.LogAccepted && !pg.sent[id] {
			return ReasonJoinConflict
		}
	}
	self := j.r.m.ID
	st := rl.st
	switch {
	case !slices.Contains(st.Members, self):
		return ReasonJoinNotMember
	case st.Permissions[self].All != jn.History:
		return ReasonJoinNotMember
	case jn.Role == RoleMember && slices.Contains(st.Admins, self):
		return ReasonJoinNotMember
	case jn.Member && jn.Role == RoleAdmin && !slices.Contains(st.Admins, self):
		return ReasonJoinNotMember
	}
	return ""
}
