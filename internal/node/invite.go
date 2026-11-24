package node

// Invites and the admin's side of the join exchange (DESIGN-next section 7,
// PROTOCOL-next section 5, requirements T3, T9, P3). An admin issues a
// token; the joiner opens it (the invite binds to the joiner's key and the
// admin answers with a preview), accepts it after a person on the joiner
// confirmed (which raises a request for a person here, through the same
// path as Propose), and polls; once a person here approves, Confirm stores
// the approval and the signed grant in one transaction and poll returns
// the room's accepted log. Exercised only by its own tests until the
// surfaces switch over.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/members"
	"github.com/wallter/a2p2/internal/store"
)

// JoinVersion is the protocol version of a join request.
const JoinVersion = 2

// Join limits and timing (PROTOCOL-next 5.1 to 5.3).
const (
	JoinInviteTTL      = 30 * time.Minute
	JoinRetry          = 5 * time.Second  // retry_ms while accepted
	JoinRetryMax       = 60 * time.Second // the joiner's back-off cap
	OpensPerMinute     = 10               // open requests per source address
	MaxJoinEntries     = 256              // entries in one signed answer
	MaxJoinRequestSize = 16 << 10         // a join request body, in bytes
	maxOpenSources     = 4096             // sources the open limiter remembers
	unknownPolls       = 60               // polls naming an unknown invite, a minute, from all sources together
	queueFullRetry     = 60 * time.Second // Retry-After when too many requests wait
)

// maxJoinPageBytes bounds the entries of one signed answer, so 256 entries
// near the entry size limit still fit a response; tests lower it.
var maxJoinPageBytes = 3 << 20

// Operations, states and refusal codes of PROTOCOL-next 5.2 and 5.3.
const (
	JoinOpen   = "open"
	JoinAccept = "accept"
	JoinPoll   = "poll"

	JoinStatePreviewed = "previewed"
	JoinStateAccepted  = "accepted"
	JoinStateSigned    = "signed"
	JoinStateRefused   = "refused"
	JoinStateExpired   = "expired"
	JoinStateRemoved   = "removed"

	CodeBadSecret   = "bad_secret"
	CodeDeclined    = "declined"
	CodeRevoked     = "revoked"
	CodeExpired     = "expired"
	CodeRemoved     = "removed"
	CodeRateLimited = "rate_limited"
	CodeWrongKey    = "wrong_key"
)

// Roles an invite grants.
const (
	RoleMember = "member"
	RoleAdmin  = "admin"
)

// Invitation is what Invite returns: the token to hand to the joiner.
type Invitation struct {
	Token   string
	Expires time.Time
	Room    string
	Role    string
	History bool
}

// Invite issues a single-use invite to room, valid for JoinInviteTTL. This
// machine must be an admin of the room. role is RoleMember, RoleAdmin, or
// empty for the default: the first three machines of a room are its admins
// (DESIGN-next section 7, T9), so while the room has fewer than three
// members the invite is for an admin. Without history the joiner may not
// read what was said before its grant. addrs are where joiners reach this
// machine (1 to 8 host:port, given by the caller, which knows whether
// networking is on). Only SHA-256 of the secret is stored.
func (r *Rooms) Invite(ctx context.Context, room, role string, history bool, addrs []string) (Invitation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rl, err := r.room(room)
	if err != nil {
		return Invitation{}, err
	}
	if !slices.Contains(rl.st.Admins, r.m.ID) {
		return Invitation{}, userErr("only an admin of the room can invite")
	}
	switch role {
	case RoleMember, RoleAdmin:
	case "":
		role = RoleMember
		if len(rl.st.Members) < 3 {
			role = RoleAdmin
		}
	default:
		return Invitation{}, userErr("role is member or admin")
	}
	fp, _ := core.KeyFingerprint(r.m.ID)
	tok := core.Token{Fingerprint: fp, Addrs: addrs}
	r.m.rand(tok.Secret[:])
	text, err := core.EncodeToken(tok)
	if err != nil {
		return Invitation{}, userErr("cannot make the invite: %v", err)
	}
	now := r.m.now()
	inv := store.Invite{SecretHash: secretHash(tok.Secret), Room: room, Role: role, History: history,
		Created: now, Expires: now.Add(JoinInviteTTL)}
	if err := r.m.st.CreateInvite(ctx, inv); err != nil {
		return Invitation{}, err
	}
	return Invitation{Token: text, Expires: inv.Expires, Room: room, Role: role, History: history}, nil
}

// secretHash is how both sides name an invite: hex SHA-256 of the secret.
func secretHash(secret [core.TokenSecretBytes]byte) string {
	h := sha256.Sum256(secret[:])
	return hex.EncodeToString(h[:])
}

// inviteOf, for Confirm, reports whether request q was raised by an
// invite's accept, and whether that invite has expired by now; if so the
// invite is recorded expired and the request failed, in one transaction,
// and nothing is signed.
func (r *Rooms) inviteOf(ctx context.Context, q store.Request, now time.Time) (invited, expired bool, err error) {
	inv, err := r.m.st.InviteByRequest(ctx, q.ID)
	if errors.Is(err, store.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if now.Before(inv.Expires) {
		return true, false, nil
	}
	_, err = r.m.st.ExpireInvites(ctx, now)
	return true, true, err
}

// ---- the wire ----

// JoinRequest is the body of POST /p/v2/join (PROTOCOL-next 5.2). Invite
// is this implementation's reading for a joiner that restarted: poll may
// name the invite by its hash instead of the secret, which is never stored
// (see JoinServer.Handle).
type JoinRequest struct {
	V      int      `json:"v"`
	Op     string   `json:"op"`
	Secret string   `json:"secret,omitempty"`
	Invite string   `json:"invite,omitempty"`
	Name   string   `json:"name,omitempty"`
	Addrs  []string `json:"addrs,omitempty"`
	After  int64    `json:"after,omitempty"`
}

var joinRequestFields = []string{"v", "op", "secret", "invite", "name", "addrs", "after"}

// JoinRoom is the room as a preview and a signed answer name it.
type JoinRoom struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Create string `json:"create"`
}

// JoinAnswer is every answer of PROTOCOL-next 5.3; fields whose value
// would be zero or false are omitted (section 0).
type JoinAnswer struct {
	State   string    `json:"state,omitempty"`
	Room    *JoinRoom `json:"room,omitempty"`
	Role    string    `json:"role,omitempty"`
	History bool      `json:"history,omitempty"`
	// Member (a reading of 5.3): the joiner is a member already; role and
	// history are what it has, and approval returns the log and changes
	// nothing.
	Member  bool              `json:"member,omitempty"`
	Admin   string            `json:"admin,omitempty"`
	Expires string            `json:"expires,omitempty"`
	RetryMS int64             `json:"retry_ms,omitempty"`
	Entries []json.RawMessage `json:"entries,omitempty"`
	More    bool              `json:"more,omitempty"`
	Error   string            `json:"error,omitempty"`
}

var joinAnswerFields = []string{"state", "room", "role", "history", "member", "admin", "expires", "retry_ms", "entries", "more", "error"}

// JoinReply is an answer as the transport carries it.
type JoinReply struct {
	Status     int
	Body       []byte
	RetryAfter time.Duration // with 429 and 503
}

// JoinTransport carries one join request to the admin machine at one of
// addrs, in order, until one answers. Before any byte of body is sent it
// calls check with the node id of the key the server proved it holds; if
// check fails it sends nothing, tries no other address and returns check's
// error. The server is given the caller's authenticated node id.
type JoinTransport interface {
	Join(ctx context.Context, addrs []string, check func(server string) error, body []byte) (JoinReply, error)
}

const timeLayout = "2006-01-02T15:04:05.000Z"

func joinTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func joinReply(status int, a JoinAnswer) JoinReply {
	b, _ := json.Marshal(a)
	return JoinReply{Status: status, Body: b}
}

func joinRefusal(code string) JoinReply {
	return joinReply(http.StatusForbidden, JoinAnswer{Error: code})
}

func badJoin(detail string) JoinReply {
	return joinReply(http.StatusBadRequest, JoinAnswer{Error: CodeBadRequest + ": " + detail})
}

// decodeJoinRequest checks a request body: strict JSON (PROTOCOL-next
// section 0), then the fields each operation takes.
func decodeJoinRequest(body []byte) (JoinRequest, string) {
	var q JoinRequest
	if len(body) > MaxJoinRequestSize {
		return q, "too large"
	}
	if core.CheckStrictObject(body, joinRequestFields...) != nil || json.Unmarshal(body, &q) != nil {
		return q, "not strict json"
	}
	switch {
	case q.V != JoinVersion:
		return q, "v"
	case q.Op != JoinOpen && q.Op != JoinAccept && q.Op != JoinPoll:
		return q, "op"
	case q.Op != JoinPoll && (q.Secret == "" || q.Invite != "" || q.After != 0):
		return q, "open and accept carry the secret and no after"
	case q.Op == JoinPoll && (q.Secret == "") == (q.Invite == ""):
		return q, "poll carries the secret or the invite"
	case q.Invite != "" && !hash(q.Invite):
		return q, "invite"
	case q.Op != JoinPoll && !core.ValidLabel(q.Name), q.Name != "" && !core.ValidLabel(q.Name):
		return q, "name"
	case len(q.Addrs) > core.TokenMaxAddrs:
		return q, "addrs"
	case q.After < 0:
		return q, "after"
	}
	for _, a := range q.Addrs {
		if !core.ValidAddr(a) {
			return q, "addr"
		}
	}
	return q, ""
}

// ---- the server ----

// JoinServer answers POST /p/v2/join for a machine's rooms.
type JoinServer struct {
	r       *Rooms
	mu      sync.Mutex
	opens   map[string][]time.Time // per source address, requests carrying a secret in the last minute
	unknown []time.Time            // polls naming an unknown invite in the last minute, from anyone
}

// NewJoinServer serves the join exchange for r's rooms.
func NewJoinServer(r *Rooms) *JoinServer {
	return &JoinServer{r: r, opens: map[string][]time.Time{}}
}

func lastMinute(ts []time.Time, now time.Time) []time.Time {
	return slices.DeleteFunc(ts, func(t time.Time) bool { return !now.Before(t.Add(time.Minute)) })
}

// allow counts a request carrying the secret from source; at
// OpensPerMinute in the last minute it refuses and says when the oldest
// leaves the window. While the table of sources is full, a source not in
// it is refused, so new sources never push out live ones.
func (s *JoinServer) allow(source string, now time.Time) (bool, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, known := s.opens[source]
	ts = lastMinute(ts, now)
	if len(ts) >= OpensPerMinute {
		s.opens[source] = ts
		return false, ts[0].Add(time.Minute).Sub(now)
	}
	if !known && len(s.opens) >= maxOpenSources {
		for k, v := range s.opens {
			if len(lastMinute(v, now)) == 0 {
				delete(s.opens, k)
			}
		}
		if len(s.opens) >= maxOpenSources {
			return false, time.Minute
		}
	}
	s.opens[source] = append(ts, now)
	return true, 0
}

// allowUnknown counts a poll naming an invite not held here against one
// budget shared by every source.
func (s *JoinServer) allowUnknown(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unknown = lastMinute(s.unknown, now)
	if len(s.unknown) >= unknownPolls {
		return false
	}
	s.unknown = append(s.unknown, now)
	return true
}

// Handle answers one join request. from is the requester's node id as the
// transport authenticated it (never a field of the request); source is its
// network address, for the rate limit. The secret never appears in what
// is logged, stored or answered.
//
// Readings (PROTOCOL-next 5.2): poll may carry, instead of the secret,
// "invite": the hex SHA-256 of the secret, accepted only from the key the
// invite is bound to, so a joiner that restarted can resume polling
// although it never stores the secret; polls naming an unknown invite
// share one budget. The limit of 10 a minute per source counts every
// request that carries the secret (an accept binds an unopened invite as
// an open does).
func (s *JoinServer) Handle(ctx context.Context, from, source string, body []byte) JoinReply {
	q, bad := decodeJoinRequest(body)
	if bad != "" {
		return badJoin(bad)
	}
	if !core.ValidNode(from) {
		return joinRefusal(CodeBadSecret)
	}
	now := s.r.m.now()
	if q.Secret != "" {
		if ok, wait := s.allow(source, now); !ok {
			return limited(http.StatusTooManyRequests, CodeRateLimited, wait)
		}
	}
	rp, err := s.serve(ctx, from, q, now)
	if err != nil {
		return limited(http.StatusServiceUnavailable, CodeUnavailable, JoinRetry)
	}
	return rp
}

func limited(status int, code string, wait time.Duration) JoinReply {
	rp := joinReply(status, JoinAnswer{Error: code})
	rp.RetryAfter = wait
	return rp
}

func (s *JoinServer) serve(ctx context.Context, from string, q JoinRequest, now time.Time) (JoinReply, error) {
	st := s.r.m.st
	h := q.Invite
	if q.Secret != "" {
		secret, err := core.DecodeSecret(q.Secret)
		if err != nil {
			return joinRefusal(CodeBadSecret), nil
		}
		h = secretHash(secret)
	}
	// The credential first: nothing is written for one not held here.
	inv, err := st.GetInvite(ctx, h)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if q.Invite != "" && !s.allowUnknown(now) {
			return limited(http.StatusTooManyRequests, CodeRateLimited, time.Minute), nil
		}
		return joinRefusal(CodeBadSecret), nil
	case err != nil:
		return JoinReply{}, err
	case inv.BoundKey != "" && inv.BoundKey != from, inv.BoundKey == "" && q.Op == JoinPoll:
		return joinRefusal(CodeBadSecret), nil
	}
	if inv.Undecided() && !now.Before(inv.Expires) {
		if inv, err = st.ExpireInvite(ctx, h, now); err != nil {
			return JoinReply{}, err
		}
	}
	switch inv.State {
	case store.InviteIssued:
		if q.Op == JoinPoll {
			return joinRefusal(CodeBadSecret), nil
		}
		fallthrough
	case store.InvitePreviewed:
		switch q.Op {
		case JoinOpen:
			return s.open(ctx, inv, from, q, now)
		case JoinAccept:
			if inv.State == store.InviteIssued {
				// An accept binds an unopened invite as an open does, then
				// accepts what that open would have shown.
				if rp, err := s.open(ctx, inv, from, q, now); err != nil || rp.Status != http.StatusOK {
					return rp, err
				}
			}
			return s.accept(ctx, h, from, q, now)
		}
		return s.preview(inv, inv.Shown), nil
	}
	return s.phase(inv, from, q.After), nil
}

// phase answers an invite that is accepted or decided: 202, signed,
// or the refusal code.
func (s *JoinServer) phase(inv store.Invite, from string, after int64) JoinReply {
	switch p := invitePhase(inv); p {
	case JoinStateSigned:
		return s.signed(inv, from, after)
	case JoinStateAccepted:
		return joinReply(http.StatusAccepted, JoinAnswer{State: JoinStateAccepted, RetryMS: JoinRetry.Milliseconds()})
	case CodeExpired, CodeDeclined, CodeRevoked:
		return joinRefusal(p)
	}
	return joinRefusal(CodeRevoked)
}

// invitePhase is the state of PROTOCOL-next 5.3 an invite is in, or the
// refusal code once refused or expired.
func invitePhase(inv store.Invite) string {
	switch {
	case inv.RequestState == store.RequestApproved:
		return JoinStateSigned
	case inv.State == store.InviteExpired:
		return CodeExpired
	case inv.State == store.InviteRevoked:
		return CodeRevoked
	case inv.RequestState == store.RequestDeclined:
		return CodeDeclined
	case inv.RequestState == store.RequestFailed && inv.RequestReason == store.ReasonInviteExpired:
		return CodeExpired
	case inv.RequestState == store.RequestFailed:
		return CodeRevoked
	}
	return inv.State
}

// effectiveTerms are what an invite gives key in state st: for a machine
// that is a member now, the role and history permission it has (nothing
// is signed for it); otherwise the invite's.
func effectiveTerms(st members.State, role string, history bool, key string) store.Terms {
	if !slices.Contains(st.Members, key) {
		return store.Terms{Role: role, History: history}
	}
	t := store.Terms{Member: true, Role: RoleMember, History: st.Permissions[key].All}
	if slices.Contains(st.Admins, key) {
		t.Role = RoleAdmin
	}
	return t
}

// open binds and previews, recording the terms shown.
func (s *JoinServer) open(ctx context.Context, inv store.Invite, from string, q JoinRequest, now time.Time) (JoinReply, error) {
	r := s.r
	r.mu.Lock()
	rl := r.rooms[inv.Room]
	var terms store.Terms
	if rl != nil {
		terms = effectiveTerms(rl.st, inv.Role, inv.History, from)
	}
	r.mu.Unlock()
	if rl == nil {
		return joinRefusal(CodeRevoked), nil
	}
	inv, err := r.m.st.OpenInvite(ctx, inv.SecretHash, from, q.Name, q.Addrs, terms, now)
	switch {
	case errors.Is(err, store.ErrInviteBound):
		return joinRefusal(CodeBadSecret), nil
	case errors.Is(err, store.ErrInviteExpired):
		return joinRefusal(CodeExpired), nil
	case err != nil:
		return JoinReply{}, err
	case inv.State != store.InvitePreviewed:
		return s.phase(inv, from, 0), nil
	}
	return s.preview(inv, terms), nil
}

func (s *JoinServer) preview(inv store.Invite, t store.Terms) JoinReply {
	r := s.r
	r.mu.Lock()
	defer r.mu.Unlock()
	rl := r.rooms[inv.Room]
	if rl == nil {
		return joinRefusal(CodeRevoked)
	}
	return joinReply(http.StatusOK, JoinAnswer{State: JoinStatePreviewed,
		Room: &JoinRoom{ID: rl.id, Label: rl.label, Create: rl.createID},
		Role: t.Role, History: t.History, Member: t.Member, Admin: r.m.ID, Expires: joinTime(inv.Expires)})
}

// Reasons an accept revokes an invite.
const ReasonTermsChanged = "the room changed since the preview, so its terms no longer hold; make a new invite"

// accept raises the request for a person (DESIGN-next section 7) through
// the path Propose uses, and moves the invite to accepted with it, in one
// transaction, if the terms the last preview showed still hold. An invite
// accepted or decided meanwhile is answered as it now stands.
func (s *JoinServer) accept(ctx context.Context, h, from string, q JoinRequest, now time.Time) (JoinReply, error) {
	r := s.r
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.m.st
	inv, err := st.GetInvite(ctx, h)
	if err != nil {
		return JoinReply{}, err
	}
	if inv.State != store.InvitePreviewed || !inv.Undecided() {
		return s.phaseLocked(inv, from), nil
	}
	revoke := func(reason string) (JoinReply, error) {
		if err := st.RevokeInvite(ctx, h, reason); err != nil {
			return JoinReply{}, err
		}
		return joinRefusal(CodeRevoked), nil
	}
	rl := r.rooms[inv.Room]
	if rl == nil {
		return revoke(ReasonRoomGone)
	}
	terms := effectiveTerms(rl.st, inv.Role, inv.History, from)
	if terms != inv.Shown {
		return revoke(ReasonTermsChanged)
	}
	var req store.Request
	if terms.Member {
		// Still a member (it left locally, or was restored from a copy):
		// no shortcut, but no new grant either; the request names the
		// role and history it has, which approval must still find.
		req, err = r.prepared(ctx, rl, store.Request{Kind: string(core.EntryAdd), Action: ActionRejoin, Room: rl.id,
			Node: from, Name: q.Name, Role: terms.Role, History: terms.History, Created: now, State: store.RequestPending}, terms.History)
	} else {
		c := Change{Kind: core.EntryAdd, Target: from, Name: q.Name, History: inv.History}
		if inv.Role == RoleAdmin {
			// One admin so far: one admin entry admits and promotes [M4].
			// More: the add, and this machine's admin entry, which waits
			// for a second admin (the third admin, T9).
			c.Kind = core.EntryAdmin
			if len(rl.st.Admins) > 1 {
				c.Kind, c.Admin = core.EntryAdd, true
			}
		}
		if c, err = checkChange(c); err == nil {
			req, err = r.prepare(ctx, rl, c)
		}
	}
	var ue *UserError
	switch {
	case errors.As(err, &ue) && ue.Msg == ReasonTooMany:
		return limited(http.StatusServiceUnavailable, CodeUnavailable, queueFullRetry), nil
	case errors.As(err, &ue):
		return revoke(ue.Msg)
	case err != nil:
		return JoinReply{}, err
	}
	err = st.AcceptInvite(ctx, h, from, q.Name, q.Addrs, terms, req, now)
	switch {
	case errors.Is(err, store.ErrInviteExpired):
		return joinRefusal(CodeExpired), nil
	case errors.Is(err, store.ErrInviteTerms):
		return revoke(ReasonTermsChanged)
	case errors.Is(err, store.ErrInviteState):
		inv, err := st.GetInvite(ctx, h)
		if err != nil {
			return JoinReply{}, err
		}
		return s.phaseLocked(inv, from), nil
	case err != nil:
		return JoinReply{}, err
	}
	return joinReply(http.StatusAccepted, JoinAnswer{State: JoinStateAccepted, RetryMS: JoinRetry.Milliseconds()}), nil
}

// phaseLocked is phase with r.mu held.
func (s *JoinServer) phaseLocked(inv store.Invite, from string) JoinReply {
	if invitePhase(inv) == JoinStateSigned {
		return s.signedLocked(inv, from, 0)
	}
	return s.phase(inv, from, 0)
}

// signed is the answer once a person approved, while from is a member
// (removed after): the room's log in the order this machine stored it,
// from index after.
func (s *JoinServer) signed(inv store.Invite, from string, after int64) JoinReply {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	return s.signedLocked(inv, from, after)
}

// signedLocked pages every entry the room keeps (accepted, held, refused
// as evidence, and invalid ones kept for replay) in arrival order: the
// order in which this machine stored them, by batch and position, which
// only ever appends. An index is therefore stable between polls, which an
// order computed from the current accepted set (as Offer's) is not: an
// entry held or refused when one page was read can be accepted before the
// next, and an entry that arrives later can sort before the page boundary.
// The joiner's replay of this order decides every entry as this machine
// did.
func (s *JoinServer) signedLocked(inv store.Invite, from string, after int64) JoinReply {
	r := s.r
	rl := r.rooms[inv.Room]
	if rl == nil {
		return joinRefusal(CodeRevoked)
	}
	if !slices.Contains(rl.st.Members, from) {
		return joinRefusal(CodeRemoved)
	}
	a := JoinAnswer{State: JoinStateSigned, Room: &JoinRoom{ID: rl.id, Label: rl.label, Create: rl.createID}}
	ids := arrivalOrder(rl)
	size := 0
	for i := after; i < int64(len(ids)); i++ {
		w := rl.ents[ids[i]].Wire()
		if size += len(w) + 1; len(a.Entries) == MaxJoinEntries || size > maxJoinPageBytes && len(a.Entries) > 0 {
			a.More = true
			break
		}
		a.Entries = append(a.Entries, json.RawMessage(w))
	}
	return joinReply(http.StatusOK, a)
}

// arrivalOrder is rl's kept entries by batch, then position in the batch.
func arrivalOrder(rl *roomLog) []string {
	ids := sortedIDs(rl.state)
	sort.SliceStable(ids, func(i, j int) bool {
		a, b := rl.state[ids[i]], rl.state[ids[j]]
		if a.Batch != b.Batch {
			return a.Batch < b.Batch
		}
		return a.Pos < b.Pos
	})
	return ids
}
