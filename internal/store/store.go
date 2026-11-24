// Package store is the node's durable state: rooms, members, messages,
// per-destination deliveries and per-agent read marks, in one SQLite file.
//
// Every exported method is one transaction. A method that returns nil has
// committed; that is the property the delivery guarantees are built on.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wallter/a2p2/internal/core"
	_ "modernc.org/sqlite"
)

// Delivery states. A delivery is always in exactly one of these.
const (
	Queued   = "queued"
	Stored   = "stored"
	Rejected = "rejected"
	Expired  = "expired"
)

// ErrConflict means a message id is already used by different content.
var ErrConflict = errors.New("message id already used by a different message")

// ErrNotFound is returned when a referenced row does not exist.
var ErrNotFound = errors.New("not found")

// ErrNotMember means the sending node is not in the room (checked inside the
// same transaction as the insert, so a concurrent removal cannot be raced).
var ErrNotMember = errors.New("sender is not a member of the room")

// ErrBadReceipt means a receipt does not refer to a message the receiving
// node sent to the receipt's sender in that room.
var ErrBadReceipt = errors.New("receipt does not match a message sent to that node")

const schema = `
CREATE TABLE meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL);
INSERT INTO meta VALUES ('seq', 0);
CREATE TABLE rooms (
  id TEXT PRIMARY KEY, label TEXT NOT NULL, owner TEXT NOT NULL,
  version INTEGER NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE members (
  room TEXT NOT NULL, node TEXT NOT NULL, name TEXT NOT NULL, addrs TEXT NOT NULL,
  PRIMARY KEY (room, node));
CREATE TABLE invites (
  secret_hash TEXT PRIMARY KEY, room TEXT NOT NULL, expires_at INTEGER NOT NULL,
  used_by TEXT, used_at INTEGER);
CREATE TABLE messages (
  n INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT NOT NULL, room TEXT NOT NULL, from_node TEXT NOT NULL,
  sender TEXT NOT NULL, recipients TEXT NOT NULL, kind TEXT NOT NULL,
  reply_to TEXT NOT NULL, thread TEXT NOT NULL, seq INTEGER NOT NULL,
  created_at INTEGER NOT NULL, expires_at INTEGER, body TEXT NOT NULL,
  received_at INTEGER NOT NULL,
  UNIQUE (from_node, id), UNIQUE (from_node, seq));
CREATE INDEX messages_room ON messages (room, n);
CREATE INDEX messages_id ON messages (id);
CREATE TABLE deliveries (
  msg_n INTEGER NOT NULL, node TEXT NOT NULL, state TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL,
  PRIMARY KEY (msg_n, node));
CREATE INDEX deliveries_queue ON deliveries (state, node);
CREATE TABLE agents (
  id TEXT PRIMARY KEY, label TEXT NOT NULL, harness TEXT NOT NULL, cwd TEXT NOT NULL,
  joined_n INTEGER NOT NULL, first_seen_at INTEGER NOT NULL, last_seen_at INTEGER NOT NULL);
CREATE TABLE marks (
  msg_n INTEGER NOT NULL, agent TEXT NOT NULL, state TEXT NOT NULL, at INTEGER NOT NULL,
  PRIMARY KEY (msg_n, agent, state));
`

// migrations[i] upgrades the schema from version i to i+1. Append only.
var migrations = []string{schema, schemaRecords}

// Store wraps the database. Safe for concurrent use.
type Store struct{ db *sql.DB }

// Open opens (creating and migrating if needed) the database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)" +
		"&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: every transaction is serialised, which is what makes
	// seq assignment and delivery bookkeeping trivially race-free.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("database is version %d, this binary understands %d: upgrade a2p2", v, len(migrations))
	}
	for ; v < len(migrations); v++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Check runs SQLite's integrity check (used by doctor).
func (s *Store) Check(ctx context.Context) error {
	var res string
	if err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return errors.New(res)
	}
	return nil
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func ms(t time.Time) int64     { return t.UnixMilli() }
func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }
func jsonText(v any) string    { b, _ := json.Marshal(v); return string(b) }

// ---- rooms and members ----

// Member is one node in a room. Addrs are dial hints, never credentials.
type Member struct {
	Node  string   `json:"node"`
	Name  string   `json:"name"`
	Addrs []string `json:"addrs"`
}

// Room is a room with its member list as this node currently knows it.
type Room struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Owner   string   `json:"owner"`
	Version int64    `json:"version"`
	Members []Member `json:"members"`
}

// PutRoom replaces a room's member list with a newer version, creating the
// room only if create is set (explicit create or join; a late refresh reply
// must never resurrect a room we dropped). A version that is not newer is
// ignored (returns false). Queued deliveries to nodes that are no longer
// members become rejected in the same transaction.
func (s *Store) PutRoom(ctx context.Context, r Room, create bool, now time.Time) (applied bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var cur int64
		var owner string
		switch err := tx.QueryRow(`SELECT version, owner FROM rooms WHERE id = ?`, r.ID).Scan(&cur, &owner); {
		case errors.Is(err, sql.ErrNoRows):
			if !create {
				return nil
			}
			if _, err := tx.Exec(`INSERT INTO rooms (id, label, owner, version, created_at) VALUES (?,?,?,?,?)`,
				r.ID, r.Label, r.Owner, r.Version, ms(now)); err != nil {
				return err
			}
		case err != nil:
			return err
		case owner != r.Owner:
			return fmt.Errorf("room %s is owned by a different node", r.ID)
		case r.Version <= cur:
			return nil
		default:
			if _, err := tx.Exec(`UPDATE rooms SET version = ?, label = ? WHERE id = ?`, r.Version, r.Label, r.ID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM members WHERE room = ?`, r.ID); err != nil {
			return err
		}
		for _, m := range r.Members {
			if _, err := tx.Exec(`INSERT INTO members (room, node, name, addrs) VALUES (?,?,?,?)`,
				r.ID, m.Node, m.Name, jsonText(m.Addrs)); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`UPDATE deliveries SET state = ?, last_error = 'no longer a member', updated_at = ?
			WHERE state = ? AND msg_n IN (SELECT n FROM messages WHERE room = ?)
			AND node NOT IN (SELECT node FROM members WHERE room = ?)`,
			Rejected, ms(now), Queued, r.ID, r.ID)
		applied = true
		return err
	})
	return applied, err
}

// DeleteRoom forgets a room this node has been removed from, provided it is
// still at version (so a refusal that arrives after we rejoined is ignored).
// Messages already stored are kept; queued deliveries for it are rejected.
func (s *Store) DeleteRoom(ctx context.Context, id string, version int64, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var cur int64
		if err := tx.QueryRow(`SELECT version FROM rooms WHERE id = ?`, id).Scan(&cur); err != nil || cur != version {
			return nil
		}
		if _, err := tx.Exec(`UPDATE deliveries SET state = ?, last_error = 'left room', updated_at = ?
			WHERE state = ? AND msg_n IN (SELECT n FROM messages WHERE room = ?)`,
			Rejected, ms(now), Queued, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM members WHERE room = ?`, id); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM rooms WHERE id = ?`, id)
		return err
	})
}

func scanMembers(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, room string) ([]Member, error) {
	rows, err := q.Query(`SELECT node, name, addrs FROM members WHERE room = ? ORDER BY node`, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		var addrs string
		if err := rows.Scan(&m.Node, &m.Name, &addrs); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(addrs), &m.Addrs)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Rooms lists every room with members.
func (s *Store) Rooms(ctx context.Context) ([]Room, error) {
	var out []Room
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT id, label, owner, version FROM rooms ORDER BY created_at, id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r Room
			if err := rows.Scan(&r.ID, &r.Label, &r.Owner, &r.Version); err != nil {
				rows.Close()
				return err
			}
			out = append(out, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			if out[i].Members, err = scanMembers(tx, out[i].ID); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// Room returns one room, or ErrNotFound.
func (s *Store) Room(ctx context.Context, id string) (Room, error) {
	r := Room{ID: id}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRow(`SELECT label, owner, version FROM rooms WHERE id = ?`, id).Scan(&r.Label, &r.Owner, &r.Version)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		r.Members, err = scanMembers(tx, id)
		return err
	})
	return r, err
}

// IsMember reports whether node is in room according to the local copy.
func (s *Store) IsMember(ctx context.Context, room, node string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM members WHERE room = ? AND node = ?`, room, node).Scan(&n)
	return n > 0, err
}

// SetAddrs updates the dial hints for a node in every room it shares with us.
func (s *Store) SetAddrs(ctx context.Context, node string, addrs []string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE members SET addrs = ? WHERE node = ?`, jsonText(addrs), node)
	return err
}

// ---- invites ----

// AddInvite records the hash of a one-time join secret.
func (s *Store) AddInvite(ctx context.Context, secretHash, room string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO invites (secret_hash, room, expires_at) VALUES (?,?,?)`,
		secretHash, room, ms(expires))
	return err
}

// RedeemInvite atomically consumes an invite for node and returns its room.
// The same node redeeming again gets the room back with fresh=false (a lost
// response must be recoverable, but must not admit a node twice); any other
// node, or an expired invite, gets ErrNotFound.
func (s *Store) RedeemInvite(ctx context.Context, secretHash, node string, now time.Time) (room string, fresh bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var exp int64
		var usedBy sql.NullString
		err := tx.QueryRow(`SELECT room, expires_at, used_by FROM invites WHERE secret_hash = ?`, secretHash).
			Scan(&room, &exp, &usedBy)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if usedBy.Valid {
			if usedBy.String == node {
				return nil
			}
			return ErrNotFound
		}
		if ms(now) > exp {
			return ErrNotFound
		}
		fresh = true
		_, err = tx.Exec(`UPDATE invites SET used_by = ?, used_at = ? WHERE secret_hash = ?`, node, ms(now), secretHash)
		return err
	})
	return room, fresh, err
}

// ---- messages ----

// Message is a stored envelope plus local bookkeeping.
type Message struct {
	core.Envelope
	N          int64     `json:"n"` // local arrival order
	ReceivedAt time.Time `json:"received_at"`
}

const msgCols = `n, id, room, from_node, sender, recipients, kind, reply_to, thread, seq, created_at, expires_at, body, received_at`

func scanMsg(sc interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var to, body string
	var created, received int64
	var expires sql.NullInt64
	err := sc.Scan(&m.N, &m.ID, &m.Room, &m.FromNode, &m.From, &to, &m.Kind, &m.ReplyTo, &m.Thread,
		&m.Seq, &created, &expires, &body, &received)
	if err != nil {
		return m, err
	}
	m.V = core.ProtocolVersion
	_ = json.Unmarshal([]byte(to), &m.To)
	_ = json.Unmarshal([]byte(body), &m.Body)
	m.CreatedAt, m.ReceivedAt = fromMS(created), fromMS(received)
	if expires.Valid {
		t := fromMS(expires.Int64)
		m.ExpiresAt = &t
	}
	return m, nil
}

func insertMsg(tx *sql.Tx, e *core.Envelope, now time.Time) (int64, error) {
	var exp any
	if e.ExpiresAt != nil {
		exp = ms(*e.ExpiresAt)
	}
	res, err := tx.Exec(`INSERT INTO messages (id, room, from_node, sender, recipients, kind, reply_to, thread,
		seq, created_at, expires_at, body, received_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.Room, e.FromNode, e.From, recipientsJSON(e), string(e.Kind), e.ReplyTo, e.Thread,
		e.Seq, ms(e.CreatedAt), exp, jsonText(e.Body), ms(now))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func recipientsJSON(e *core.Envelope) string {
	if e.To == nil {
		return "[]"
	}
	return jsonText(e.To)
}

func mark(tx *sql.Tx, n int64, agent, state string, now time.Time) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO marks (msg_n, agent, state, at) VALUES (?,?,?,?)`, n, agent, state, ms(now))
	return err
}

// receiptTarget finds the message a receipt refers to: written by author, in
// the receipt's room, and not itself a receipt.
func receiptTarget(tx *sql.Tx, e *core.Envelope, author string) (int64, error) {
	var n int64
	err := tx.QueryRow(`SELECT n FROM messages WHERE id = ? AND from_node = ? AND room = ? AND kind != 'receipt'`,
		e.Body.Ref, author, e.Room).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrBadReceipt
	}
	return n, err
}

func isMember(tx *sql.Tx, room, node string) (bool, error) {
	var c int
	err := tx.QueryRow(`SELECT COUNT(*) FROM members WHERE room = ? AND node = ?`, room, node).Scan(&c)
	return c > 0, err
}

// Send commits a message written on this node and queues one delivery per
// destination node, all in one transaction. It assigns e.Seq. If this node
// already sent e.ID the original is returned and nothing is written, so a
// caller that lost the response can retry with the same id.
//
// Destinations are every other member of the room at this instant, except for
// receipts, which go only to onlyTo (the node that wrote the original). A
// reply marks the message it answers as seen and handled by the replier in
// the same transaction.
func (s *Store) Send(ctx context.Context, e *core.Envelope, onlyTo string, now time.Time) (Message, error) {
	var out Message
	err := s.tx(ctx, func(tx *sql.Tx) error {
		prev, err := scanMsg(tx.QueryRow(`SELECT `+msgCols+` FROM messages WHERE id = ? AND from_node = ?`, e.ID, e.FromNode))
		if err == nil {
			if prev.Room != e.Room || prev.From != e.From {
				return ErrConflict
			}
			out = prev
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if ok, err := isMember(tx, e.Room, e.FromNode); err != nil {
			return err
		} else if !ok {
			return ErrNotFound // the room was dropped since the caller looked it up
		}
		if err := tx.QueryRow(`UPDATE meta SET value = value + 1 WHERE key = 'seq' RETURNING value`).Scan(&e.Seq); err != nil {
			return err
		}
		if err := e.Validate(); err != nil {
			return err
		}
		n, err := insertMsg(tx, e, now)
		if err != nil {
			return err
		}
		switch {
		case e.Kind == core.KindReceipt:
			target, err := receiptTarget(tx, e, onlyTo)
			if err != nil {
				return err
			}
			if err := mark(tx, target, e.From, e.Body.State, now); err != nil {
				return err
			}
		case e.ReplyTo != "":
			var target int64
			err := tx.QueryRow(`SELECT n FROM messages WHERE id = ? AND room = ? ORDER BY n LIMIT 1`, e.ReplyTo, e.Room).Scan(&target)
			if err == nil {
				for _, st := range []string{core.ReceiptSeen, core.ReceiptHandled} {
					if err := mark(tx, target, e.From, st, now); err != nil {
						return err
					}
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		q := `INSERT INTO deliveries (msg_n, node, state, updated_at)
			SELECT ?, node, ?, ? FROM members WHERE room = ? AND node != ?`
		args := []any{n, Queued, ms(now), e.Room, e.FromNode}
		if onlyTo != "" {
			q += ` AND node = ?`
			args = append(args, onlyTo)
		}
		if _, err := tx.Exec(q, args...); err != nil {
			return err
		}
		out, err = scanMsg(tx.QueryRow(`SELECT `+msgCols+` FROM messages WHERE n = ?`, n))
		return err
	})
	return out, err
}

// Receive commits a message from another node; self is this node's id.
// Everything that decides whether the message is acceptable happens in the
// one transaction that inserts it:
//   - the sender must be a member of the room (ErrNotMember);
//   - an exact repeat of a message we hold is a no-op reporting success;
//   - the same id or seq from that sender with different content is
//     ErrConflict. Ids are scoped to their author, so one member can never
//     block or overwrite another member's message by reusing its id;
//   - a receipt must refer to a message we sent to that node in that room
//     (ErrBadReceipt).
//
// The caller must have validated the envelope and set e.FromNode from the
// authenticated connection.
func (s *Store) Receive(ctx context.Context, e *core.Envelope, self string, now time.Time) (duplicate bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if ok, err := isMember(tx, e.Room, e.FromNode); err != nil {
			return err
		} else if !ok {
			return ErrNotMember
		}
		var room, kind, body, to, replyTo string
		var seq int64
		err := tx.QueryRow(`SELECT room, seq, kind, body, recipients, reply_to FROM messages WHERE from_node = ? AND id = ?`,
			e.FromNode, e.ID).Scan(&room, &seq, &kind, &body, &to, &replyTo)
		if err == nil {
			if room != e.Room || seq != e.Seq || kind != string(e.Kind) || body != jsonText(e.Body) ||
				to != recipientsJSON(e) || replyTo != e.ReplyTo {
				return ErrConflict
			}
			duplicate = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var target int64
		if e.Kind == core.KindReceipt {
			if target, err = receiptTarget(tx, e, self); err != nil {
				return err
			}
			var sent int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE msg_n = ? AND node = ?`, target, e.FromNode).Scan(&sent); err != nil {
				return err
			}
			if sent == 0 {
				return ErrBadReceipt
			}
		}
		if _, err := insertMsg(tx, e, now); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") { // same (from_node, seq), different id
				return ErrConflict
			}
			return err
		}
		if e.Kind == core.KindReceipt {
			return mark(tx, target, e.From, e.Body.State, now)
		}
		return nil
	})
	return duplicate, err
}

// Get returns a message by id. Ids are unique per author; in the unlikely
// case that two authors used the same id, the one stored first is returned.
func (s *Store) Get(ctx context.Context, id string) (Message, error) {
	m, err := scanMsg(s.db.QueryRowContext(ctx, `SELECT `+msgCols+` FROM messages WHERE id = ? ORDER BY n LIMIT 1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

func (s *Store) queryMsgs(ctx context.Context, q string, args ...any) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Unread returns messages addressed to agent (or to everyone) that the agent
// has not been shown, oldest first. Reading does not mark anything; call
// Mark once the messages have actually been handed over.
func (s *Store) Unread(ctx context.Context, agent, room string, limit int) ([]Message, error) {
	q := `SELECT ` + msgCols + ` FROM messages m WHERE kind != 'receipt' AND sender != ?1
		AND n > COALESCE((SELECT joined_n FROM agents WHERE id = ?1), 0)
		AND (recipients = '[]' OR EXISTS (SELECT 1 FROM json_each(m.recipients) WHERE value = ?1))
		AND NOT EXISTS (SELECT 1 FROM marks k WHERE k.msg_n = m.n AND k.agent = ?1 AND k.state = 'seen')`
	args := []any{agent}
	if room != "" {
		q += ` AND room = ?2`
		args = append(args, room)
	}
	q += fmt.Sprintf(` ORDER BY n LIMIT %d`, limit)
	return s.queryMsgs(ctx, q, args...)
}

// History returns the newest messages in a room with n < before (0 = newest),
// in ascending order, receipts excluded.
func (s *Store) History(ctx context.Context, room string, before int64, limit int) ([]Message, error) {
	if before <= 0 {
		before = 1<<62 - 1
	}
	return s.queryMsgs(ctx, `SELECT * FROM (SELECT `+msgCols+` FROM messages
		WHERE room = ? AND kind != 'receipt' AND n < ? ORDER BY n DESC LIMIT ?) ORDER BY n`, room, before, limit)
}

// Mark records that agent has seen or handled messages. Idempotent; unknown
// ids are ignored.
func (s *Store) Mark(ctx context.Context, agent, state string, now time.Time, ids ...string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO marks (msg_n, agent, state, at)
				SELECT n, ?, ?, ? FROM messages WHERE id = ?`, agent, state, ms(now), id); err != nil {
				return err
			}
		}
		return nil
	})
}

// Prune deletes messages received before cutoff, with their deliveries and
// marks. The cutoff must be older than the delivery retry deadline so that a
// retried message is still recognised as a duplicate.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	var removed int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		for _, t := range []string{"deliveries", "marks"} {
			if _, err := tx.Exec(`DELETE FROM `+t+` WHERE msg_n IN (SELECT n FROM messages WHERE received_at < ?)`, ms(cutoff)); err != nil {
				return err
			}
		}
		res, err := tx.Exec(`DELETE FROM messages WHERE received_at < ?`, ms(cutoff))
		if err != nil {
			return err
		}
		removed, err = res.RowsAffected()
		return err
	})
	return removed, err
}

// ---- deliveries ----

// Delivery is the state of one message for one destination node.
type Delivery struct {
	MessageID string    `json:"message_id"`
	Node      string    `json:"node"`
	State     string    `json:"state"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// QueuedNodes lists destination nodes with at least one queued delivery.
func (s *Store) QueuedNodes(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT node FROM deliveries WHERE state = ?`, Queued)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NextBatch returns the oldest queued messages for node in send order
// (local n, which for our own messages is seq order).
func (s *Store) NextBatch(ctx context.Context, node string, limit int) ([]Message, error) {
	return s.queryMsgs(ctx, `SELECT `+msgCols+` FROM messages WHERE n IN
		(SELECT msg_n FROM deliveries WHERE node = ? AND state = ?) ORDER BY n LIMIT ?`, node, Queued, limit)
}

// SetDelivery moves a queued delivery of message n to a terminal state. A
// delivery that is already terminal is left alone, so a late response cannot
// rewrite history.
func (s *Store) SetDelivery(ctx context.Context, n int64, node, state, reason string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deliveries SET state = ?, last_error = ?, attempts = attempts + 1, updated_at = ?
		WHERE msg_n = ? AND node = ? AND state = ?`, state, reason, ms(now), n, node, Queued)
	return err
}

// Defer records a failed attempt for everything queued to node.
func (s *Store) Defer(ctx context.Context, node, reason string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deliveries SET attempts = attempts + 1, last_error = ?, updated_at = ?
		WHERE node = ? AND state = ?`, reason, ms(now), node, Queued)
	return err
}

// Expire gives up on queued deliveries whose message is older than cutoff or
// past its own expiry. "expired" means the retry deadline passed without
// confirmation; the recipient may or may not hold the message.
func (s *Store) Expire(ctx context.Context, cutoff, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE deliveries SET state = ?, last_error = 'retry deadline passed', updated_at = ?
		WHERE state = ? AND msg_n IN (SELECT n FROM messages WHERE created_at < ? OR (expires_at IS NOT NULL AND expires_at < ?))`,
		Expired, ms(now), Queued, ms(cutoff), ms(now))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Deliveries returns the per-node delivery state of a message.
func (s *Store) Deliveries(ctx context.Context, id string) ([]Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, d.node, d.state, d.attempts, d.last_error, d.updated_at
		FROM deliveries d JOIN messages m ON m.n = d.msg_n WHERE m.id = ? ORDER BY d.node`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		var at int64
		if err := rows.Scan(&d.MessageID, &d.Node, &d.State, &d.Attempts, &d.LastError, &at); err != nil {
			return nil, err
		}
		d.UpdatedAt = fromMS(at)
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkRow says an agent saw or handled a message.
type MarkRow struct {
	Agent string    `json:"agent"`
	State string    `json:"state"`
	At    time.Time `json:"at"`
}

// Marks lists who has seen or handled a message.
func (s *Store) Marks(ctx context.Context, id string) ([]MarkRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT k.agent, k.state, k.at FROM marks k JOIN messages m ON m.n = k.msg_n
		WHERE m.id = ? ORDER BY k.at, k.agent, k.state`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MarkRow{}
	for rows.Next() {
		var m MarkRow
		var at int64
		if err := rows.Scan(&m.Agent, &m.State, &at); err != nil {
			return nil, err
		}
		m.At = fromMS(at)
		out = append(out, m)
	}
	return out, rows.Err()
}

// OutboxStats summarises queued work per destination node.
type OutboxStats struct {
	Node      string    `json:"node"`
	Queued    int       `json:"queued"`
	Oldest    time.Time `json:"oldest"`
	LastError string    `json:"last_error,omitempty"`
}

// Outbox reports queued deliveries per destination.
func (s *Store) Outbox(ctx context.Context) ([]OutboxStats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.node, COUNT(*), MIN(m.created_at), MAX(d.last_error)
		FROM deliveries d JOIN messages m ON m.n = d.msg_n WHERE d.state = ? GROUP BY d.node ORDER BY d.node`, Queued)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutboxStats{}
	for rows.Next() {
		var o OutboxStats
		var oldest int64
		if err := rows.Scan(&o.Node, &o.Queued, &oldest, &o.LastError); err != nil {
			return nil, err
		}
		o.Oldest = fromMS(oldest)
		out = append(out, o)
	}
	return out, rows.Err()
}

// ---- agents ----

// Agent is a local participant: a harness session, a script, or the human.
type Agent struct {
	ID       string    `json:"id"`
	Label    string    `json:"label"`
	Harness  string    `json:"harness"`
	Cwd      string    `json:"cwd,omitempty"`
	LastSeen time.Time `json:"last_seen"`
}

// TouchAgent registers an agent or refreshes its last-seen time. A new agent
// starts with an empty inbox: history before it first appeared is not unread.
func (s *Store) TouchAgent(ctx context.Context, a Agent, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO agents (id, label, harness, cwd, joined_n, first_seen_at, last_seen_at)
		VALUES (?,?,?,?, COALESCE((SELECT MAX(n) FROM messages), 0), ?, ?)
		ON CONFLICT (id) DO UPDATE SET harness = excluded.harness, cwd = excluded.cwd, last_seen_at = excluded.last_seen_at`,
		a.ID, a.Label, a.Harness, a.Cwd, ms(now), ms(now))
	return err
}

// Agents lists local agents, most recently seen first.
func (s *Store) Agents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, label, harness, cwd, last_seen_at FROM agents ORDER BY last_seen_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		var a Agent
		var seen int64
		if err := rows.Scan(&a.ID, &a.Label, &a.Harness, &a.Cwd, &seen); err != nil {
			return nil, err
		}
		a.LastSeen = fromMS(seen)
		out = append(out, a)
	}
	return out, rows.Err()
}

// Senders lists the distinct agent ids that have written in a room, newest
// first. This is how a node learns which agents exist on other nodes.
func (s *Store) Senders(ctx context.Context, room string, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sender FROM messages WHERE room = ? AND kind != 'receipt'
		GROUP BY sender ORDER BY MAX(n) DESC LIMIT ?`, room, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MemberByNode returns the dial hints we hold for a node (from any shared room).
func (s *Store) MemberByNode(ctx context.Context, node string) (Member, error) {
	var m Member
	var addrs string
	err := s.db.QueryRowContext(ctx, `SELECT node, name, addrs FROM members WHERE node = ? ORDER BY room LIMIT 1`, node).
		Scan(&m.Node, &m.Name, &addrs)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	_ = json.Unmarshal([]byte(addrs), &m.Addrs)
	return m, err
}
