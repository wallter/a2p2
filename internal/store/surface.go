package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wallter/a2p2/internal/core"
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

// LatestDecision is the approved request of kind recorded last, if any:
// by the order the writes committed (SQLite's rowid, which grows with each
// insert and which nothing here renumbers: no VACUUM), not by a clock that
// can go back or an id.
func (s *Store) LatestDecision(ctx context.Context, kind string) (Request, bool, error) {
	q, err := scanRequest(s.db.QueryRowContext(ctx, `SELECT `+requestCols+` FROM requests
		WHERE kind = ? AND state = ? ORDER BY rowid DESC LIMIT 1`, kind, RequestApproved))
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

// Place is where a message sits in the order readers see: (created,
// author, seq, id), every field signed (shown.go). Created is the author's
// clock, so an author can place a message before a reply written after it,
// or date it so early that its text expires soon after it arrives (F31,
// deferred: DECISIONS D-NEW-N-records).
type Place struct {
	Created time.Time
	Author  string
	Seq     int64
	ID      string
}

// PlaceOf is r's place.
func PlaceOf(r *core.Record) Place {
	f := r.Fields()
	return Place{Created: f.Created, Author: r.Author(), Seq: f.Seq, ID: r.ID()}
}

// MessagesBefore returns up to limit message records of room whose text is
// here, newest first, strictly before place p; a zero p means from the
// newest.
func (rd *Reader) MessagesBefore(room string, p Place, limit int) ([]Held, error) {
	if p.Created.IsZero() {
		return rd.query(`WHERE room = ? AND kind = 'message' AND reason = ?
			ORDER BY created DESC, author DESC, seq DESC, id DESC LIMIT ?`, room, ReasonLive, limit)
	}
	return rd.query(`WHERE room = ? AND kind = 'message' AND reason = ? AND (created, author, seq, id) < (?, ?, ?, ?)
		ORDER BY created DESC, author DESC, seq DESC, id DESC LIMIT ?`, room, ReasonLive,
		ms(p.Created), p.Author, p.Seq, p.ID, limit)
}

// MissingRecord is a record whose text has been missing here since before
// some time.
type MissingRecord struct {
	Room    string
	ID      string
	Created time.Time
}

// MissingBetween lists up to limit records whose text is missing here and
// that were written at or after from and before to, oldest first by
// (created, id), strictly after the record after (zero: from the start).
// from is how a caller leaves out text past retention by its clock before
// a prune has marked it expired.
func (s *Store) MissingBetween(ctx context.Context, from, to time.Time, after MissingRecord, limit int) ([]MissingRecord, error) {
	lo, id := ms(from), ""
	if !after.Created.IsZero() {
		lo, id = ms(after.Created), after.ID
	}
	rows, err := s.db.QueryContext(ctx, `SELECT room, id, created FROM records
		WHERE reason = 'missing' AND created >= ? AND created < ? AND (created > ? OR (created = ? AND id > ?))
		ORDER BY created, id LIMIT ?`, ms(from), ms(to), lo, lo, id, limit)
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

// Writer is an agent seen writing messages in a room: its machine, its
// label, and when it last wrote (by the record's signed creation time).
type Writer struct {
	Node  string
	Agent string
	Last  time.Time
}

// Writers lists, newest first, up to limit agents that wrote a message in
// room, from the records held here (any text state). It reads headers
// only, never text.
func (rd *Reader) Writers(room string, limit int) ([]Writer, error) {
	rows, err := rd.tx.Query(`SELECT author, agent, MAX(created) AS last FROM records
		WHERE room = ? AND kind = 'message' GROUP BY author, agent ORDER BY last DESC, author, agent LIMIT ?`, room, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Writer
	for rows.Next() {
		var w Writer
		var at int64
		if err := rows.Scan(&w.Node, &w.Agent, &at); err != nil {
			return nil, err
		}
		w.Last = fromMS(at)
		out = append(out, w)
	}
	return out, rows.Err()
}
