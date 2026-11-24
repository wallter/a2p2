// Package store is the machine's durable state in one SQLite file: signed
// records and streams, the room log and its member cache, requests,
// invites and joins, what peers hold, their addresses, agents and each
// agent's reading.
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
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a referenced row does not exist.
var ErrNotFound = errors.New("not found")

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

// schema is migration 1, the schema v0.2 shipped; migration 10 drops what
// of it the next version does not use.
//
// migrations[i] upgrades the schema from version i to i+1. Append only.
var migrations = []string{schema, schemaRecords, schemaLog, schemaPull, schemaKnownFrom, schemaInvites, schemaJoinPaging, schemaPeerAddrs, schemaHistory, schemaDropV02, schemaLevels}

// Store wraps the database. Safe for concurrent use.
type Store struct {
	db *sql.DB
	// changes counts write transactions, raised inside each one just
	// before its commit. With one connection, a read that starts after
	// changes was read sees every write counted by then (see Changes).
	changes atomic.Int64
}

// Changes is the number of write transactions begun committing. Read it
// before a read transaction: if it is the same later, nothing that read
// saw has changed since (a raise for a commit that failed only makes the
// caller look again).
func (s *Store) Changes() int64 { return s.changes.Load() }

// Open opens (creating and migrating if needed) the database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)" +
		"&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)" +
		// Dropped text (withdrawn, expired, voided) is overwritten in the
		// database file, not only unlinked; see Scrub for the log file.
		"&_pragma=secure_delete(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: every transaction is serialised, which is what makes
	// assigning positions and arrival order trivially race-free.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Scrub moves everything in the write-ahead log into the database file and
// truncates the log, so that text dropped since the last scrub (withdrawn,
// expired, voided) is no longer in any file of the store. It is not a
// forensic guarantee: blocks the operating system has freed are its own.
// The daemon calls it every minute and it is cheap when little was written.
func (s *Store) Scrub(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

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
	s.changes.Add(1)
	return tx.Commit()
}

func ms(t time.Time) int64     { return t.UnixMilli() }
func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }
func jsonText(v any) string    { b, _ := json.Marshal(v); return string(b) }

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
	_, err := s.db.ExecContext(ctx, `INSERT INTO agents (id, label, harness, cwd, first_seen_at, last_seen_at)
		VALUES (?,?,?,?,?,?)
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
