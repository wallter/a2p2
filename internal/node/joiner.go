package node

// The joiner's side of the join exchange (DESIGN-next section 7,
// PROTOCOL-next 5): preview an invite, accept it once a person here has
// confirmed the preview, poll until the admin's person decides, then pin
// the room by the create entry id the preview showed and apply the room's
// log through Receive. A join a person accepted is recorded durably, so a
// restart resumes polling (Resume); the secret is kept only in memory,
// between the preview and the accept.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// ErrWrongKey: the machine at the invite's address is not the admin the
// token names. Nothing was sent to it.
var ErrWrongKey = &UserError{CodeWrongKey + ": the machine at the invite's address is not the admin the invite names; nothing was sent"}

// JoinRefused is an invite the admin machine refused or that ended:
// Code is bad_secret, declined, revoked, expired or removed.
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
}

// Preview is what a person on the joiner confirms (PROTOCOL-next 5.3).
// State is previewed, or accepted or signed when this machine accepted the
// invite before (then the room may not be known yet).
type Preview struct {
	State   string
	Room    string
	Label   string
	Create  string // the create entry id that will be pinned
	Role    string
	History bool
	Admin   string // the admin's node id, checked against the token
	Expires time.Time
}

// JoinOutcome is how a join stands: accepted (still waiting for a person
// on the admin machine), signed (the room is held here), refused (Reason
// is the code), expired or removed.
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
// confirm. The invite is now bound to this machine's key. A refusal is a
// *JoinRefused.
func (j *Joiner) JoinPreview(ctx context.Context, token string) (Preview, error) {
	tok, err := core.DecodeToken(token)
	if err != nil {
		return Preview{}, userErr("bad invite token: %v", err)
	}
	if j.o.Name == "" || !core.ValidLabel(j.o.Name) {
		return Preview{}, userErr("this machine needs a valid name to join")
	}
	var server string
	rp, a, err := j.call(ctx, tok.Addrs, fingerprintCheck(tok, &server), JoinRequest{Op: JoinOpen, Secret: tok.SecretText()})
	if err != nil {
		return Preview{}, err
	}
	p, err := answerPreview(rp, a, server)
	if err != nil {
		return Preview{}, err
	}
	j.mu.Lock()
	j.previews[secretHash(tok.Secret)] = &previewed{tok: tok, preview: p}
	j.mu.Unlock()
	return p, nil
}

// answerPreview reads an open's answer.
func answerPreview(rp JoinReply, a JoinAnswer, server string) (Preview, error) {
	switch {
	case rp.Status == http.StatusTooManyRequests:
		return Preview{}, userErr("the admin machine is limiting invites; try again in %s", rp.RetryAfter.Round(time.Second))
	case rp.Status == http.StatusForbidden:
		return Preview{}, &JoinRefused{Code: a.Error}
	case rp.Status == http.StatusAccepted && a.State == JoinStateAccepted:
		return Preview{State: JoinStateAccepted, Admin: server}, nil
	case rp.Status == http.StatusOK && a.State == JoinStateSigned && validJoinRoom(a.Room):
		return Preview{State: JoinStateSigned, Room: a.Room.ID, Label: a.Room.Label, Create: a.Room.Create, Admin: server}, nil
	case rp.Status != http.StatusOK || a.State != JoinStatePreviewed || !validJoinRoom(a.Room):
		return Preview{}, errors.New("join: unexpected answer from the admin machine")
	}
	exp, err := time.Parse(timeLayout, a.Expires)
	if err != nil || a.Admin != server || a.Role != RoleMember && a.Role != RoleAdmin {
		return Preview{}, errors.New("join: the preview is malformed or names another admin")
	}
	return Preview{State: JoinStatePreviewed, Room: a.Room.ID, Label: a.Room.Label, Create: a.Room.Create,
		Role: a.Role, History: a.History, Admin: a.Admin, Expires: exp}, nil
}

func validJoinRoom(r *JoinRoom) bool {
	return r != nil && core.ValidRoom(r.ID) && len(r.Create) == 32 && isLowerHex(r.Create)
}

// JoinAccept accepts the invite in token, which JoinPreview showed to a
// person here who confirmed it; the local surface calls it only after that
// confirmation. The admin machine then raises a request for its person.
// JoinAccept records the join and polls for up to wait (PROTOCOL-next
// 5.3), returning how it stands; a join still accepted keeps being
// followed by Resume.
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
		return JoinOutcome{}, userErr("preview the invite first")
	}
	var server string
	rp, a, err := j.call(ctx, tok.Addrs, fingerprintCheck(tok, &server), JoinRequest{Op: JoinAccept, Secret: tok.SecretText()})
	if err != nil {
		return JoinOutcome{}, err
	}
	switch {
	case rp.Status == http.StatusForbidden:
		return refusedOutcome(pv.preview.Room, a.Error), nil
	case rp.Status == http.StatusAccepted, rp.Status == http.StatusOK && a.State == JoinStateSigned:
	default:
		return JoinOutcome{}, errors.New("join: unexpected answer from the admin machine")
	}
	now := j.r.m.now()
	p := pv.preview
	jn := store.Join{Invite: h, Room: p.Room, Label: p.Label, CreateID: p.Create, Role: p.Role, History: p.History,
		Admin: server, Addrs: tok.Addrs, Expires: p.Expires, State: store.JoinAccepted, Created: now, Updated: now}
	if err := j.r.m.st.PutJoin(ctx, jn); err != nil {
		return JoinOutcome{}, err
	}
	j.mu.Lock()
	delete(j.previews, h) // the secret is not needed any more: polls name the invite by its hash
	j.mu.Unlock()
	return j.follow(ctx, jn, wait)
}

// Resume follows every join a person accepted that has no outcome yet, for
// up to wait each (a restart calls it), and returns how each stands.
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
	after, restarted := int64(0), false
	var gen int64
	finish := func(o JoinOutcome) (JoinOutcome, error) {
		st := map[string]string{JoinStateSigned: store.JoinSigned, JoinStateExpired: store.JoinExpired,
			JoinStateRemoved: store.JoinRemoved, JoinStateRefused: store.JoinRefused}[o.State]
		if err := j.r.m.st.SetJoinState(ctx, jn.Invite, st, o.Reason, j.r.m.now()); err != nil {
			return JoinOutcome{}, err
		}
		return o, nil
	}
	for {
		rp, a, err := j.call(ctx, jn.Addrs, check, JoinRequest{Op: JoinPoll, Invite: jn.Invite, After: after})
		var pause time.Duration
		switch {
		case errors.Is(err, ErrWrongKey):
			return JoinOutcome{}, err
		case ctx.Err() != nil:
			return JoinOutcome{}, ctx.Err()
		case err != nil:
			pause = delay // unreachable or a bad answer: try again later
		case rp.Status == http.StatusForbidden:
			return finish(refusedOutcome(jn.Room, a.Error))
		case rp.Status == http.StatusTooManyRequests:
			pause = max(rp.RetryAfter, time.Second)
		case rp.Status == http.StatusAccepted:
			if a.RetryMS > 0 && time.Duration(a.RetryMS)*time.Millisecond > delay {
				delay = min(time.Duration(a.RetryMS)*time.Millisecond, JoinRetryMax)
			}
			pause = delay
		case rp.Status == http.StatusOK && a.State == JoinStateSigned:
			if gen, err = j.apply(ctx, &jn, gen, a); err != nil {
				return JoinOutcome{}, err
			}
			after += int64(len(a.Entries))
			if a.More {
				continue
			}
			if !restarted && j.waiting(jn.Room) {
				// The log changed between pages and something is held for
				// an entry that was paged past: read it again from the start.
				after, restarted = 0, true
				continue
			}
			return finish(JoinOutcome{State: JoinStateSigned, Room: jn.Room})
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

// apply takes one signed page: the first time, it pins the room by the
// create entry id the preview showed (unless the room is held here already
// with that create, as on a machine restored from a copy), then gives the
// entries to Receive as one batch, under the generation it pinned.
func (j *Joiner) apply(ctx context.Context, jn *store.Join, gen int64, a JoinAnswer) (int64, error) {
	if !validJoinRoom(a.Room) || jn.Room != "" && a.Room.ID != jn.Room || jn.CreateID != "" && a.Room.Create != jn.CreateID {
		return 0, errors.New("join: the signed answer names another room")
	}
	if jn.Room == "" { // accepted before this machine saw a preview: take the room the admin names
		jn.Room, jn.Label, jn.CreateID = a.Room.ID, a.Room.Label, a.Room.Create
		jn.Updated = j.r.m.now()
		if err := j.r.m.st.PutJoin(ctx, *jn); err != nil {
			return 0, err
		}
	}
	wires := make([][]byte, len(a.Entries))
	for i, e := range a.Entries {
		wires[i] = e
	}
	if gen == 0 {
		j.r.mu.Lock()
		rl := j.r.rooms[jn.Room]
		if rl != nil {
			gen = rl.gen
			if rl.createID != jn.CreateID {
				j.r.mu.Unlock()
				return 0, userErr("a room with this id but another identity is held here")
			}
		}
		j.r.mu.Unlock()
	}
	if gen == 0 {
		if len(wires) == 0 {
			return 0, errors.New("join: the signed log does not start with the room's create entry")
		}
		e, err := core.ParseEntry(wires[0])
		if err != nil || e.ID() != jn.CreateID {
			return 0, errors.New("join: the signed log does not start with the room's create entry")
		}
		if _, err := j.r.PinRoom(ctx, wires[0]); err != nil {
			return 0, err
		}
		if gen, err = j.r.Gen(jn.Room); err != nil {
			return 0, err
		}
		wires = wires[1:]
	}
	if len(wires) > 0 {
		if _, err := j.r.Receive(ctx, jn.Room, gen, wires...); err != nil {
			return 0, err
		}
	}
	return gen, nil
}

// waiting reports whether room holds entries waiting for others.
func (j *Joiner) waiting(room string) bool {
	j.r.mu.Lock()
	defer j.r.mu.Unlock()
	rl := j.r.rooms[room]
	if rl == nil {
		return false
	}
	for _, row := range rl.state {
		if row.State == store.LogHeld {
			return true
		}
	}
	return false
}
