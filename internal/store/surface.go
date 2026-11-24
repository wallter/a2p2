package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"
)

// RequestNetwork is the kind of a person's decision on networking: Action
// is the state chosen (off, outbound or on) and Name the address listened
// on when on. Such a request belongs to no room and is recorded decided.
const RequestNetwork = "network"

// RecordDecision stores q as already decided (approved at q.DecidedAt): a
// person's decision that signs nothing, such as turning networking on.
func (s *Store) RecordDecision(ctx context.Context, q Request) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO requests (id, kind, action, room, node, name, role, history, seconds, created_at,
			state, decided_at, admins) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, q.ID, q.Kind, q.Action, q.Room, q.Node, q.Name,
			q.Role, q.History, q.Seconds, ms(q.Created), RequestApproved, ms(q.DecidedAt), q.Admins)
		return err
	})
}

// LatestDecision is the most recent approved request of kind, if any.
func (s *Store) LatestDecision(ctx context.Context, kind string) (Request, bool, error) {
	q, err := scanRequest(s.db.QueryRowContext(ctx, `SELECT `+requestCols+` FROM requests
		WHERE kind = ? AND state = ? ORDER BY decided_at DESC, created_at DESC, id DESC LIMIT 1`, kind, RequestApproved))
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, false, nil
	}
	return q, err == nil, err
}

// schemaHistory is migration 9: history pages by (created, id) without
// reading the whole room.
const schemaHistory = `
CREATE INDEX records_history ON records (room, kind, created, id);
`

// MessagesBefore returns up to limit message records of room whose text is
// here, newest first, strictly before the position (created, id); a zero
// created means from the newest.
func (rd *Reader) MessagesBefore(room string, created time.Time, id string, limit int) ([]Held, error) {
	at := int64(math.MaxInt64)
	if !created.IsZero() {
		at = ms(created)
	}
	return rd.query(`WHERE room = ? AND kind = 'message' AND reason = ? AND (created < ? OR (created = ? AND id < ?))
		ORDER BY created DESC, id DESC LIMIT ?`, room, ReasonLive, at, at, id, limit)
}

// MissingRecord is a record whose text has been missing here since before
// some time.
type MissingRecord struct {
	Room    string
	ID      string
	Created time.Time
}

// MissingBefore lists up to limit records whose text is missing here and
// that were written before t, oldest first.
func (s *Store) MissingBefore(ctx context.Context, t time.Time, limit int) ([]MissingRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT room, id, created FROM records
		WHERE reason IN ('live', 'missing') AND created < ? AND reason = 'missing' ORDER BY created, id LIMIT ?`,
		ms(t), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MissingRecord
	for rows.Next() {
		var m MissingRecord
		var c int64
		if err := rows.Scan(&m.Room, &m.ID, &c); err != nil {
			return nil, err
		}
		m.Created = fromMS(c)
		out = append(out, m)
	}
	return out, rows.Err()
}
