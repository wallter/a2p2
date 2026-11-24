package store

// Invites a machine issued (the admin side of PROTOCOL-next 5) and joins
// this machine has in progress (the joiner side). This is migration 5;
// migrations 2 to 4 have shipped. Used only by the new path in
// internal/node until the surfaces switch over.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// schemaInvites is migration 5.
//
// Differences from PROTOCOL-next section 8: the table is room_invites,
// because v0.2's invites table (same name, other columns) is still used by
// v0.2 joins. Beside the columns of section 8 it keeps created_at, the
// joiner's name and addresses as its open or accept gave them, and reason
// (why an invite was revoked). Signed, declined and revoked-by-a-person's-
// confirmation are not stored as states: they are the state of the request
// (requests.state), decided in the transaction that stores the approval and
// the grant, so an invite and its request can never disagree.
//
// joins is the joiner's durable record of a join a person has accepted
// and the admin has not finished answering: what the preview showed, where
// to reach the admin, and the invite's identifier (SHA-256 of the secret,
// as the admin keys it). The secret itself is never stored, on either side.
const schemaInvites = `
CREATE TABLE room_invites (
  secret_hash TEXT PRIMARY KEY, room TEXT NOT NULL, role TEXT NOT NULL, history INTEGER NOT NULL,
  created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, bound_key TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '', addrs TEXT NOT NULL DEFAULT '[]',
  state TEXT NOT NULL, request TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '');
CREATE INDEX room_invites_room ON room_invites (room);
CREATE INDEX room_invites_request ON room_invites (request);
CREATE TABLE joins (
  invite TEXT PRIMARY KEY, room TEXT NOT NULL, label TEXT NOT NULL, create_id TEXT NOT NULL,
  role TEXT NOT NULL, history INTEGER NOT NULL, admin TEXT NOT NULL, addrs TEXT NOT NULL,
  expires_at INTEGER NOT NULL, state TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
`

// Stored states of an invite (PROTOCOL-next 5.3). An invite starts issued
// (no open yet); signed and refused by a person are read from its request.
const (
	InviteIssued    = "issued"
	InvitePreviewed = "previewed"
	InviteAccepted  = "accepted"
	InviteExpired   = "expired"
	InviteRevoked   = "revoked" // refused when accepted: the request could not be raised
)

// ReasonInviteExpired is the failure recorded on a request whose invite
// expired before a person approved it.
const ReasonInviteExpired = "the invite expired before it was approved"

// Errors of the invite methods.
var (
	ErrInviteBound   = errors.New("invite is bound to another key")
	ErrInviteState   = errors.New("invite is not in the state the change needs")
	ErrInviteExpired = errors.New("invite expired")
)

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
	RequestState  string
	RequestReason string
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
	i.state, i.request, i.reason, coalesce(q.state, ''), coalesce(q.reason, '')`

func scanInvite(sc interface{ Scan(...any) error }) (Invite, error) {
	var inv Invite
	var created, expires int64
	var addrs string
	err := sc.Scan(&inv.SecretHash, &inv.Room, &inv.Role, &inv.History, &created, &expires, &inv.BoundKey, &inv.Name,
		&addrs, &inv.State, &inv.Request, &inv.Reason, &inv.RequestState, &inv.RequestReason)
	if errors.Is(err, sql.ErrNoRows) {
		return Invite{}, ErrNotFound
	}
	if err != nil {
		return Invite{}, err
	}
	inv.Created, inv.Expires, inv.Addrs = fromMS(created), fromMS(expires), parseAddrs(addrs)
	return inv, nil
}

func getInvite(q interface {
	QueryRow(string, ...any) *sql.Row
}, where string, arg string) (Invite, error) {
	return scanInvite(q.QueryRow(`SELECT `+inviteCols+` FROM room_invites i LEFT JOIN requests q ON q.id = i.request
		WHERE `+where, arg))
}

// GetInvite returns the invite whose secret hashes to hash, or ErrNotFound.
func (s *Store) GetInvite(ctx context.Context, hash string) (Invite, error) {
	return getInvite(dbCtx{ctx, s.db}, `i.secret_hash = ?`, hash)
}

// dbCtx runs one read statement outside a write transaction.
type dbCtx struct {
	ctx context.Context
	db  *sql.DB
}

func (d dbCtx) QueryRow(q string, args ...any) *sql.Row {
	return d.db.QueryRowContext(d.ctx, q, args...)
}

// InviteByRequest returns the invite whose request is id, or ErrNotFound.
func (s *Store) InviteByRequest(ctx context.Context, id string) (Invite, error) {
	if id == "" {
		return Invite{}, ErrNotFound
	}
	return getInvite(dbCtx{ctx, s.db}, `i.request = ?`, id)
}

// BindInvite is a first open (PROTOCOL-next 5.2): an issued, unexpired
// invite is bound to key and moves to previewed; one already bound to key
// is returned as it is; one bound to another key is ErrInviteBound. The
// joiner's name and addresses are kept. It returns the invite after.
func (s *Store) BindInvite(ctx context.Context, hash, key, name string, addrs []string, now time.Time) (inv Invite, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if inv, err = getInvite(tx, `i.secret_hash = ?`, hash); err != nil {
			return err
		}
		switch {
		case inv.BoundKey != "" && inv.BoundKey != key:
			return ErrInviteBound
		case inv.BoundKey == key:
			return nil
		case inv.State != InviteIssued:
			return ErrInviteState
		case !now.Before(inv.Expires):
			return ErrInviteExpired
		}
		if _, err := tx.Exec(`UPDATE room_invites SET bound_key = ?, name = ?, addrs = ?, state = ? WHERE secret_hash = ?`,
			key, name, jsonText(addrs), InvitePreviewed, hash); err != nil {
			return err
		}
		inv.BoundKey, inv.Name, inv.Addrs, inv.State = key, name, addrs, InvitePreviewed
		return nil
	})
	return inv, err
}

// AcceptInvite is an accept (PROTOCOL-next 5.2): a previewed, unexpired
// invite bound to key moves to accepted and the request q for a person is
// created, in one transaction. The room must be held.
func (s *Store) AcceptInvite(ctx context.Context, hash, key, name string, addrs []string, q Request, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		inv, err := getInvite(tx, `i.secret_hash = ?`, hash)
		switch {
		case err != nil:
			return err
		case inv.BoundKey != key:
			return ErrInviteBound
		case inv.State != InvitePreviewed:
			return ErrInviteState
		case !now.Before(inv.Expires):
			return ErrInviteExpired
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
}

// RevokeInvite refuses a previewed invite whose request could not be
// raised (revalidation failed when it was accepted), with reason.
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

// ExpireInvites moves every invite that is past its expiry and not signed
// (its request, if any, not approved) to expired, and fails its pending
// request with ReasonInviteExpired, in one transaction. It returns how
// many invites expired.
func (s *Store) ExpireInvites(ctx context.Context, now time.Time) (int, error) {
	var due int
	// Most calls find nothing: look first, outside a write transaction.
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM room_invites i LEFT JOIN requests q ON q.id = i.request
		WHERE i.expires_at <= ? AND i.state IN (?, ?, ?) AND coalesce(q.state, '') != ?`,
		ms(now), InviteIssued, InvitePreviewed, InviteAccepted, RequestApproved).Scan(&due); err != nil || due == 0 {
		return 0, err
	}
	var n int
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT i.secret_hash, i.request FROM room_invites i LEFT JOIN requests q ON q.id = i.request
			WHERE i.expires_at <= ? AND i.state IN (?, ?, ?) AND coalesce(q.state, '') != ?`,
			ms(now), InviteIssued, InvitePreviewed, InviteAccepted, RequestApproved)
		if err != nil {
			return err
		}
		var hashes, reqs []string
		for rows.Next() {
			var h, q string
			if err := rows.Scan(&h, &q); err != nil {
				rows.Close()
				return err
			}
			hashes, reqs = append(hashes, h), append(reqs, q)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i, h := range hashes {
			if _, err := tx.Exec(`UPDATE room_invites SET state = ? WHERE secret_hash = ?`, InviteExpired, h); err != nil {
				return err
			}
			if reqs[i] != "" {
				if _, err := tx.Exec(`UPDATE requests SET state = ?, reason = ?, decided_at = ? WHERE id = ? AND state = ?`,
					RequestFailed, ReasonInviteExpired, ms(now), reqs[i], RequestPending); err != nil {
					return err
				}
			}
		}
		n = len(hashes)
		return nil
	})
	return n, err
}

// ---- the joiner's side ----

// States of a join in progress on the joiner.
const (
	JoinAccepted = "accepted" // waiting for a person on the admin machine
	JoinSigned   = "signed"   // the room's log was received
	JoinRefused  = "refused"  // declined, revoked or the secret refused; Reason says which
	JoinExpired  = "expired"
	JoinRemoved  = "removed"
)

// Join is a join this machine's person accepted.
type Join struct {
	Invite   string // hex SHA-256 of the secret
	Room     string
	Label    string
	CreateID string
	Role     string
	History  bool
	Admin    string // the admin's node id, checked against the token
	Addrs    []string
	Expires  time.Time
	State    string
	Reason   string
	Created  time.Time
	Updated  time.Time
}

// PutJoin stores a join, replacing one with the same invite.
func (s *Store) PutJoin(ctx context.Context, j Join) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO joins (invite, room, label, create_id, role, history, admin, addrs, expires_at,
			state, reason, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (invite) DO UPDATE SET room = excluded.room, label = excluded.label, create_id = excluded.create_id,
			role = excluded.role, history = excluded.history, admin = excluded.admin, addrs = excluded.addrs,
			expires_at = excluded.expires_at, state = excluded.state, reason = excluded.reason, updated_at = excluded.updated_at`,
			j.Invite, j.Room, j.Label, j.CreateID, j.Role, j.History, j.Admin, jsonText(j.Addrs), ms(j.Expires),
			j.State, j.Reason, ms(j.Created), ms(j.Updated))
		return err
	})
}

// SetJoinState records a join's state and reason.
func (s *Store) SetJoinState(ctx context.Context, invite, state, reason string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE joins SET state = ?, reason = ?, updated_at = ? WHERE invite = ?`,
			state, reason, ms(now), invite)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return nil
	})
}

// Joins lists joins, oldest first, in one state if state is set.
func (s *Store) Joins(ctx context.Context, state string) ([]Join, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT invite, room, label, create_id, role, history, admin, addrs, expires_at,
		state, reason, created_at, updated_at FROM joins WHERE (? = '' OR state = ?) ORDER BY created_at, invite`, state, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Join
	for rows.Next() {
		var j Join
		var addrs string
		var exp, created, updated int64
		if err := rows.Scan(&j.Invite, &j.Room, &j.Label, &j.CreateID, &j.Role, &j.History, &j.Admin, &addrs, &exp,
			&j.State, &j.Reason, &created, &updated); err != nil {
			return nil, err
		}
		j.Addrs, j.Expires, j.Created, j.Updated = parseAddrs(addrs), fromMS(exp), fromMS(created), fromMS(updated)
		out = append(out, j)
	}
	return out, rows.Err()
}

func parseAddrs(s string) []string {
	var out []string
	_ = json.Unmarshal([]byte(s), &out)
	return out
}
