package store

// Invites a machine issued (the admin side of PROTOCOL 5) and joins
// this machine has made (the joiner side). This is migration 6; migrations
// 2 to 4 have shipped. Used only by the new path in internal/node until the
// surfaces switch over.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// schemaInvites is migration 6.
//
// Differences from PROTOCOL section 8: the table is room_invites,
// because v0.2's invites table (same name, other columns) is still used by
// v0.2 joins. Beside the columns of section 8 it keeps created_at, the
// joiner's name and addresses as its open or accept gave them, reason (why
// an invite was revoked), and the terms the last preview showed the bound
// key (shown_member, shown_role, shown_history), which accept must still
// find. Signed, declined and revoked by a failed confirmation are not
// stored states: they are the state of the request, decided in the
// transaction that stores the approval and the grant, so an invite and its
// request never disagree.
//
// requests gains admins: the number of admins the room had when a request
// that makes an admin was raised. Making an admin takes one admin's entry
// or two (M4), so a request raised under one count is not approved under
// another.
//
// joins is the joiner's durable record of every invite it previewed and a
// person here approved: the terms shown, the admin's full key, where to
// reach it, the room's local generation when it was previewed (gen; 0 for
// a room never held here), and the invite's identifier (SHA-256 of the
// secret, as the admin keys it). It is written before the accept is sent
// and kept after the join ends, so an invite is used once. The secret
// itself is never stored, on either side.
const schemaInvites = `
CREATE TABLE room_invites (
  secret_hash TEXT PRIMARY KEY, room TEXT NOT NULL, role TEXT NOT NULL, history INTEGER NOT NULL,
  created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, bound_key TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '', addrs TEXT NOT NULL DEFAULT '[]',
  state TEXT NOT NULL, request TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '',
  shown_member INTEGER NOT NULL DEFAULT 0, shown_role TEXT NOT NULL DEFAULT '', shown_history INTEGER NOT NULL DEFAULT 0);
CREATE INDEX room_invites_room ON room_invites (room);
CREATE INDEX room_invites_request ON room_invites (request);
ALTER TABLE requests ADD COLUMN admins INTEGER NOT NULL DEFAULT 0;
CREATE TABLE joins (
  invite TEXT PRIMARY KEY, room TEXT NOT NULL, label TEXT NOT NULL, create_id TEXT NOT NULL,
  role TEXT NOT NULL, history INTEGER NOT NULL, member INTEGER NOT NULL, admin TEXT NOT NULL, addrs TEXT NOT NULL,
  gen INTEGER NOT NULL, expires_at INTEGER NOT NULL, state TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX joins_room ON joins (room, state);
`

// schemaJoinPaging is migration 7: every stored log entry gets a position
// in its room allocated once and never reused (arrival, from the room's
// counter arrivals), which the join exchange pages by; a join records the
// generation its own pin gave the room.
const schemaJoinPaging = `
ALTER TABLE log ADD COLUMN arrival INTEGER NOT NULL DEFAULT 0;
ALTER TABLE rooms ADD COLUMN arrivals INTEGER NOT NULL DEFAULT 0;
ALTER TABLE joins ADD COLUMN pinned_gen INTEGER NOT NULL DEFAULT 0;
UPDATE log SET arrival = (SELECT count(*) FROM log l WHERE l.room = log.room
  AND (l.batch < log.batch OR (l.batch = log.batch AND l.batch_pos <= log.batch_pos)));
UPDATE rooms SET arrivals = (SELECT coalesce(max(arrival), 0) FROM log WHERE log.room = rooms.id);
CREATE INDEX log_arrival_position ON log (room, arrival);
`

// Stored states of an invite (PROTOCOL 5.3). An invite starts issued
// (no open yet); signed and refused by a person are read from its request.
const (
	InviteIssued    = "issued"
	InvitePreviewed = "previewed"
	InviteAccepted  = "accepted"
	InviteExpired   = "expired"
	InviteRevoked   = "revoked" // refused at accept: the request could not be raised
)

// ReasonInviteExpired is the failure recorded on a request whose invite
// expired before a person approved it.
const ReasonInviteExpired = "the invite expired before it was approved"

// Errors of the invite methods.
var (
	ErrInviteBound   = errors.New("invite is bound to another key")
	ErrInviteState   = errors.New("invite is not in the state the change needs")
	ErrInviteExpired = errors.New("invite expired")
	ErrInviteTerms   = errors.New("the terms the preview showed no longer hold")
)

// Terms are what a preview shows the joiner: whether it is a member
// already, and the role and history it will have.
type Terms struct {
	Member  bool
	Role    string
	History bool
}

// Invite is one invite issued here. RequestState and RequestReason are
// those of its request, when it has one.
type Invite struct {
	SecretHash    string // hex SHA-256 of the 16 secret bytes
	Room          string
	Role          string // member or admin
	History       bool
	Created       time.Time
	Expires       time.Time
	BoundKey      string
	Name          string
	Addrs         []string
	State         string
	Request       string
	Reason        string
	Shown         Terms
	RequestState  string
	RequestReason string
}

// Undecided reports whether no person has decided the invite's request
// (there is none, or it is pending) and it is not expired or revoked.
func (inv Invite) Undecided() bool {
	return (inv.RequestState == "" || inv.RequestState == RequestPending) &&
		(inv.State == InviteIssued || inv.State == InvitePreviewed || inv.State == InviteAccepted)
}

// CreateInvite stores a new invite in state issued. The room must be held.
func (s *Store) CreateInvite(ctx context.Context, inv Invite) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := checkGen(tx, inv.Room, 0); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO room_invites (secret_hash, room, role, history, created_at, expires_at, state)
			VALUES (?,?,?,?,?,?,?)`, inv.SecretHash, inv.Room, inv.Role, inv.History, ms(inv.Created), ms(inv.Expires), InviteIssued)
		return err
	})
}

const inviteCols = `i.secret_hash, i.room, i.role, i.history, i.created_at, i.expires_at, i.bound_key, i.name, i.addrs,
	i.state, i.request, i.reason, i.shown_member, i.shown_role, i.shown_history, coalesce(q.state, ''), coalesce(q.reason, '')`

func scanInvite(sc interface{ Scan(...any) error }) (Invite, error) {
	var inv Invite
	var created, expires int64
	var addrs string
	err := sc.Scan(&inv.SecretHash, &inv.Room, &inv.Role, &inv.History, &created, &expires, &inv.BoundKey, &inv.Name,
		&addrs, &inv.State, &inv.Request, &inv.Reason, &inv.Shown.Member, &inv.Shown.Role, &inv.Shown.History,
		&inv.RequestState, &inv.RequestReason)
	if errors.Is(err, sql.ErrNoRows) {
		return Invite{}, ErrNotFound
	}
	if err != nil {
		return Invite{}, err
	}
	inv.Created, inv.Expires, inv.Addrs = fromMS(created), fromMS(expires), parseAddrs(addrs)
	return inv, nil
}

type queryRower interface {
	QueryRow(string, ...any) *sql.Row
}

func getInvite(q queryRower, where string, arg string) (Invite, error) {
	return scanInvite(q.QueryRow(`SELECT `+inviteCols+` FROM room_invites i LEFT JOIN requests q ON q.id = i.request
		WHERE `+where, arg))
}

// dbCtx runs one read statement outside a write transaction.
type dbCtx struct {
	ctx context.Context
	db  *sql.DB
}

func (d dbCtx) QueryRow(q string, args ...any) *sql.Row {
	return d.db.QueryRowContext(d.ctx, q, args...)
}

// GetInvite returns the invite whose secret hashes to hash, or ErrNotFound.
func (s *Store) GetInvite(ctx context.Context, hash string) (Invite, error) {
	return getInvite(dbCtx{ctx, s.db}, `i.secret_hash = ?`, hash)
}

// InviteByRequest returns the invite whose request is id, or ErrNotFound.
func (s *Store) InviteByRequest(ctx context.Context, id string) (Invite, error) {
	if id == "" {
		return Invite{}, ErrNotFound
	}
	return getInvite(dbCtx{ctx, s.db}, `i.request = ?`, id)
}

// expireRow expires one undecided invite past its expiry and fails its
// pending request, inside tx. It reports whether it did.
func expireRow(tx *sql.Tx, inv Invite, now time.Time) (bool, error) {
	if !inv.Undecided() || now.Before(inv.Expires) {
		return false, nil
	}
	if _, err := tx.Exec(`UPDATE room_invites SET state = ? WHERE secret_hash = ?`, InviteExpired, inv.SecretHash); err != nil {
		return false, err
	}
	if inv.Request != "" {
		if _, err := tx.Exec(`UPDATE requests SET state = ?, reason = ?, decided_at = ? WHERE id = ? AND state = ?`,
			RequestFailed, ReasonInviteExpired, ms(now), inv.Request, RequestPending); err != nil {
			return false, err
		}
	}
	return true, nil
}

// ExpireInvite expires the one invite named by hash if it is undecided
// and past its expiry, in one transaction, and returns it as it is after.
func (s *Store) ExpireInvite(ctx context.Context, hash string, now time.Time) (inv Invite, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if inv, err = getInvite(tx, `i.secret_hash = ?`, hash); err != nil {
			return err
		}
		done, err := expireRow(tx, inv, now)
		if done {
			inv, err = getInvite(tx, `i.secret_hash = ?`, hash)
		}
		return err
	})
	return inv, err
}

// OpenInvite is an open (PROTOCOL 5.2), in one transaction: an
// issued invite is bound to key and moves to previewed; one bound to
// another key is ErrInviteBound; one past its expiry and undecided is
// expired (ErrInviteExpired). shown, computed by the caller from the
// room's state, is recorded as what this preview showed, unless the
// invite is accepted or decided already. It returns the invite after.
func (s *Store) OpenInvite(ctx context.Context, hash, key, name string, addrs []string, shown Terms, now time.Time) (inv Invite, err error) {
	expired := false
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if inv, err = getInvite(tx, `i.secret_hash = ?`, hash); err != nil {
			return err
		}
		if inv.BoundKey != "" && inv.BoundKey != key {
			return ErrInviteBound
		}
		if done, err := expireRow(tx, inv, now); err != nil || done {
			expired = done
			return err // the expiry is kept
		}
		if inv.State != InviteIssued && inv.State != InvitePreviewed {
			return nil // accepted or decided: the caller answers its phase
		}
		if _, err := tx.Exec(`UPDATE room_invites SET bound_key = ?, name = ?, addrs = ?, state = ?,
			shown_member = ?, shown_role = ?, shown_history = ? WHERE secret_hash = ?`,
			key, name, jsonText(addrs), InvitePreviewed, shown.Member, shown.Role, shown.History, hash); err != nil {
			return err
		}
		inv.BoundKey, inv.Name, inv.Addrs, inv.State, inv.Shown = key, name, addrs, InvitePreviewed, shown
		return nil
	})
	if err == nil && expired {
		err = ErrInviteExpired
	}
	return inv, err
}

// AcceptInvite is an accept (PROTOCOL 5.2), in one transaction: a
// previewed, unexpired invite bound to key, whose last preview showed
// terms now, moves to accepted and the request q for a person is created.
// ErrInviteState: it is not previewed (accepted or decided meanwhile);
// ErrInviteTerms: the terms changed since the preview; ErrInviteExpired,
// judged by clock, read inside the transaction.
// The room must be held.
func (s *Store) AcceptInvite(ctx context.Context, hash, key, name string, addrs []string, now Terms, q Request, clock func() time.Time) error {
	expired := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		inv, err := getInvite(tx, `i.secret_hash = ?`, hash)
		if err != nil {
			return err
		}
		if done, err := expireRow(tx, inv, clock()); err != nil || done {
			expired = done
			return err // the expiry is kept
		}
		switch {
		case inv.BoundKey != key:
			return ErrInviteBound
		case inv.State != InvitePreviewed:
			return ErrInviteState
		case inv.Shown != now:
			return ErrInviteTerms
		}
		if err := checkGen(tx, inv.Room, 0); err != nil {
			return err
		}
		if err := insertRequest(tx, q); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE room_invites SET state = ?, request = ?, name = ?, addrs = ? WHERE secret_hash = ?`,
			InviteAccepted, q.ID, name, jsonText(addrs), hash)
		return err
	})
	if err == nil && expired {
		err = ErrInviteExpired
	}
	return err
}

// RevokeInvite refuses an issued or previewed invite whose request could
// not be raised (revalidation failed when it was accepted), with reason.
func (s *Store) RevokeInvite(ctx context.Context, hash, reason string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE room_invites SET state = ?, reason = ? WHERE secret_hash = ? AND state IN (?, ?)`,
			InviteRevoked, reason, hash, InviteIssued, InvitePreviewed)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrInviteState
		}
		return nil
	})
}

// ExpireInvites moves every undecided invite past its expiry to expired
// and fails its pending request with ReasonInviteExpired, in one
// transaction; a declined, failed or approved one is never touched. It
// returns how many invites expired.
func (s *Store) ExpireInvites(ctx context.Context, now time.Time) (int, error) {
	const due = `SELECT ` + inviteCols + ` FROM room_invites i LEFT JOIN requests q ON q.id = i.request
		WHERE i.expires_at <= ? AND i.state IN (?, ?, ?) AND coalesce(q.state, '') IN ('', ?)`
	var any int
	// Most calls find nothing: look first, outside a write transaction.
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM (`+due+`)`, ms(now), InviteIssued, InvitePreviewed,
		InviteAccepted, RequestPending).Scan(&any); err != nil || any == 0 {
		return 0, err
	}
	var n int
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(due, ms(now), InviteIssued, InvitePreviewed, InviteAccepted, RequestPending)
		if err != nil {
			return err
		}
		var invs []Invite
		for rows.Next() {
			inv, err := scanInvite(rows)
			if err != nil {
				rows.Close()
				return err
			}
			invs = append(invs, inv)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, inv := range invs {
			done, err := expireRow(tx, inv, now)
			if err != nil {
				return err
			}
			if done {
				n++
			}
		}
		return nil
	})
	return n, err
}

// inviteDeadline fails an approval (in applyLog) of a request whose invite
// is past its expiry at the moment of the write: ErrInviteExpired.
func inviteDeadline(tx *sql.Tx, request string, now time.Time) error {
	var exp int64
	err := tx.QueryRow(`SELECT expires_at FROM room_invites WHERE request = ?`, request).Scan(&exp)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	case ms(now) >= exp:
		return ErrInviteExpired
	}
	return nil
}

// ---- the joiner's side ----

// States of a join on the joiner.
const (
	JoinAccepted = "accepted" // approved here; waiting for the admin machine
	JoinSigned   = "signed"   // the room's log was received
	JoinRefused  = "refused"  // declined, revoked, refused, or failed here; Reason says which
	JoinExpired  = "expired"
	JoinRemoved  = "removed"
	JoinLeft     = "left" // the room was left here while the join was in progress
)

// Join is an invite this machine's person approved.
type Join struct {
	Invite   string // hex SHA-256 of the secret
	Room     string
	Label    string
	CreateID string
	Role     string
	History  bool
	Member   bool   // the preview said this machine is a member already
	Admin    string // the admin's full node id, checked against the token
	Addrs    []string
	Gen      int64 // the room's local generation when previewed; 0 if never held
	// PinnedGen is the generation this join's own pin gave the room (0:
	// not pinned by it).
	PinnedGen int64
	Expires   time.Time
	State     string
	Reason    string
	Created   time.Time
	Updated   time.Time
}

// Terms are the terms the join was approved on.
func (j Join) Terms() Terms { return Terms{Member: j.Member, Role: j.Role, History: j.History} }

// AddJoin records an approved join in state accepted, in one transaction.
// A join with the same invite already recorded is returned unchanged and
// nothing is written: an approved record is never overwritten.
func (s *Store) AddJoin(ctx context.Context, j Join) (got Join, added bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if got, err = getJoin(tx, j.Invite); err == nil {
			return nil
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		_, err := tx.Exec(`INSERT INTO joins (invite, room, label, create_id, role, history, member, admin, addrs, gen,
			expires_at, state, reason, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			j.Invite, j.Room, j.Label, j.CreateID, j.Role, j.History, j.Member, j.Admin, jsonText(j.Addrs), j.Gen,
			ms(j.Expires), JoinAccepted, "", ms(j.Created), ms(j.Created))
		got, added = j, true
		got.State, got.Updated = JoinAccepted, j.Created
		return err
	})
	return got, added, err
}

// GetJoin returns the join recorded for invite, or ErrNotFound.
func (s *Store) GetJoin(ctx context.Context, invite string) (Join, error) {
	return getJoin(dbCtx{ctx, s.db}, invite)
}

// EndJoin records the outcome of a join that is still accepted, in one
// transaction: ErrNotFound if it is not (it ended, or the room was left).
func (s *Store) EndJoin(ctx context.Context, invite, state, reason string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE joins SET state = ?, reason = ?, updated_at = ? WHERE invite = ? AND state = ?`,
			state, reason, ms(now), invite, JoinAccepted)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return nil
	})
}

const joinCols = `invite, room, label, create_id, role, history, member, admin, addrs, gen, expires_at,
	state, reason, created_at, updated_at, pinned_gen`

func scanJoin(sc interface{ Scan(...any) error }) (Join, error) {
	var j Join
	var addrs string
	var exp, created, updated int64
	err := sc.Scan(&j.Invite, &j.Room, &j.Label, &j.CreateID, &j.Role, &j.History, &j.Member, &j.Admin, &addrs, &j.Gen,
		&exp, &j.State, &j.Reason, &created, &updated, &j.PinnedGen)
	if errors.Is(err, sql.ErrNoRows) {
		return Join{}, ErrNotFound
	}
	if err != nil {
		return Join{}, err
	}
	j.Addrs, j.Expires, j.Created, j.Updated = parseAddrs(addrs), fromMS(exp), fromMS(created), fromMS(updated)
	return j, nil
}

func getJoin(q queryRower, invite string) (Join, error) {
	return scanJoin(q.QueryRow(`SELECT `+joinCols+` FROM joins WHERE invite = ?`, invite))
}

// Joins lists joins, oldest first, in one state if state is set.
func (s *Store) Joins(ctx context.Context, state string) ([]Join, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+joinCols+` FROM joins WHERE (? = '' OR state = ?)
		ORDER BY created_at, invite`, state, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Join
	for rows.Next() {
		j, err := scanJoin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// JoinLogRoom pins a room for the join recorded under invite, in one
// transaction: the join must still be accepted and name the room, and the
// room's generation must still be gen, the one recorded when the preview
// was approved (0: never held; otherwise left at gen). It then does what
// CreateLogRoom does, records the new generation on the join, and returns
// it. ErrStaleGeneration: the join ended, or the room was held, left or
// joined again since.
func (s *Store) JoinLogRoom(ctx context.Context, invite string, gen int64, label string, root LogRow, members []RoomMember, now time.Time) (int64, error) {
	var got int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		j, err := getJoin(tx, invite)
		if errors.Is(err, ErrNotFound) || err == nil && (j.State != JoinAccepted || j.PinnedGen != 0) {
			return ErrStaleGeneration
		} else if err != nil {
			return err
		}
		g, left, err := roomGen(tx, j.Room)
		switch {
		case errors.Is(err, ErrNotFound):
			g = 0
		case err != nil:
			return err
		case !left:
			return ErrStaleGeneration
		}
		if g != gen {
			return ErrStaleGeneration
		}
		if got, err = createLogRoom(tx, j.Room, label, root, members, now); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE joins SET pinned_gen = ?, updated_at = ? WHERE invite = ?`, got, ms(now), invite)
		return err
	})
	return got, err
}

// RoomGen is a room's local generation (0 if never held here) and whether
// it is held now.
func (s *Store) RoomGen(ctx context.Context, room string) (gen int64, held bool, err error) {
	var leftAt int64
	var create string
	err = s.db.QueryRowContext(ctx, `SELECT gen, left_at, create_id FROM rooms WHERE id = ?`, room).Scan(&gen, &leftAt, &create)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return gen, err == nil && leftAt == 0 && create != "", err
}

func parseAddrs(s string) []string {
	var out []string
	_ = json.Unmarshal([]byte(s), &out)
	return out
}
