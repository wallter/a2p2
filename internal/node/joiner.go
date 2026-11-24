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
var ErrWrongKey = &UserError{CodeWrongKey + ": the machine at the invite's address is not the admin the invite names; nothing was sent"}

// Errors of the joiner that a person is shown.
var (
	ErrInviteUsed     = &UserError{"this invite was used on this machine already; ask for a new one"}
	ErrNoPreview      = &UserError{"preview the invite first"}
	ErrRoomChanged    = &UserError{"the room was left or joined here since the preview; preview the invite again"}
	ErrTermsChanged   = &UserError{"the admin machine now shows different terms from those approved here; ask for a new invite"}
	ErrNoRecord       = &UserError{"the invite was accepted before, but this machine has no record of approving it; ask for a new invite"}
	ErrPreviewChanged = &UserError{"the invite was previewed again since the preview you approved; approve the latest preview"}
)

// Reasons a join ends without the room, besides the server's codes.
const (
	ReasonJoinNotMember = "the room's log was received, but it does not show this machine as a member"
	ReasonJoinMissing   = "the room's log was received, but an entry the admin machine accepted was not accepted here"
	ReasonJoinNoCreate  = "the admin machine's signed log does not start with the room's create entry"
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
	// ID identifies everything above but State (previewID): JoinAccept
	// takes it, so what is accepted is exactly what the person approved.
	ID string
}

func (p Preview) terms() store.Terms {
	return store.Terms{Member: p.Member, Role: p.Role, History: p.History}
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
		p := recordPreview(rec)
		p.State = JoinStateAccepted
		return p, nil
	case p.State != JoinStatePreviewed:
		return Preview{}, ErrNoRecord
	}
	gen, _, err := j.r.m.st.RoomGen(ctx, p.Room)
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
	p := Preview{State: JoinStatePreviewed, Room: a.Room.ID, Label: a.Room.Label, Create: a.Room.Create,
		Role: a.Role, History: a.History, Member: a.Member, Admin: a.Admin, Expires: exp}
	p.ID = previewID(p)
	return p, nil
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
// its person. JoinAccept polls for up to wait (PROTOCOL-next 5.3) and
// returns how the join stands; a join still accepted keeps being followed
// by Resume. If the accept's answer is lost, calling JoinAccept again is
// safe.
func (j *Joiner) JoinAccept(ctx context.Context, token, preview string, wait time.Duration) (JoinOutcome, error) {
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
	rp, a, err := j.call(ctx, tok.Addrs, check, JoinRequest{Op: JoinAccept, Secret: tok.SecretText(), Terms: p.ID})
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
			reason, err := j.apply(ctx, jn, pg, a)
			switch {
			case err != nil:
				return JoinOutcome{}, err
			case reason == ReasonJoinLeft:
				return left, nil
			case reason != "":
				return fail(reason)
			case a.More:
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

// paging is one run of reading a signed log.
type paging struct {
	after  int64 // the last page's next
	gen    int64 // the generation the room is held at, once pinned or found
	pinned bool  // this run's pin made it
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
	wires := make([][]byte, len(a.Entries))
	for i, e := range a.Entries {
		wires[i] = e
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
		case jn.PinnedGen != 0:
			pg.gen = jn.PinnedGen // Receive refuses it if the room was left since
		case held && sameRoom && gen == jn.Gen:
			pg.gen = gen
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
	_, err := j.r.Receive(ctx, jn.Room, pg.gen, wires...)
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
// the pinned room in its own state. The outcome then reports the role and
// history permission that state gives, with a note if they differ from
// the preview.
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
