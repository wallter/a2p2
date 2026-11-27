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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
var migrations = []string{schema, schemaRecords, schemaLog, schemaPull, schemaKnownFrom, schemaInvites, schemaJoinPaging, schemaPeerAddrs, schemaHistory, schemaDropV02, schemaLevels, schemaReading, schemaFiles, schemaFiles2, schemaEarly}

// Store wraps the database. Safe for concurrent use.
type Store struct {
	db *sql.DB
	// changes counts write transactions, raised inside each one just
	// before its commit. With one connection, a read that starts after
	// changes was read sees every write counted by then (see Changes).
	changes atomic.Int64
	// root is the directory under which attachments live (files.go):
	// the database's own directory. fileMu serialises putting bytes at a
	// path with the deletion queue's run.
	//
	// Lock order [K4]. A lock may be taken while holding only those
	// before it: node.Rooms.mu, then node.Machine.mu (a write), then
	// fileMu or stageMu (never both), then a database transaction, then
	// the leaves, each taken alone and never held while waiting for
	// anything (node.Rooms.smu and fmu, node.Files.mu, resMu). So under
	// fileMu nothing
	// waits for Rooms.mu: a gate's prepare reads Rooms.State, which takes
	// only smu; and a deletion run from under Rooms.mu (a leave) may wait
	// for fileMu, since no holder of fileMu waits for Rooms.mu.
	// texts.mu (the summary cache, summary.go) is a leaf too, taken inside
	// a transaction; a transaction that wrote records holds it across its
	// commit, which waits for nothing but the disk.
	root   string
	fileMu sync.Mutex
	// unknownFiles counts what the last start left under blobs/ because
	// a2p2 does not generate its name [K3].
	unknownFiles atomic.Int64
	// unverified, under vmu (a leaf), names the held blobs the last start
	// could not examine: OpenBlob hashes one before serving it [W1].
	vmu        sync.Mutex
	unverified map[[2]string]bool
	// stageMu serialises staging reservations; resMu guards staging, the
	// bytes reserved by requests being staged (finding 9).
	stageMu, resMu            sync.Mutex
	staging                   map[string]stageRes
	roomBudget, machineBudget int64
	// texts is, per room, its records without live text as of the last
	// commit, for Reader.Summary (summary.go). Its lock is a leaf, held
	// across the commit of a transaction that wrote records.
	texts textCache
}

// Changes is the number of write transactions begun committing. Read it
// before a read transaction: if it is the same later, nothing that read
// saw has changed since (a raise for a commit that failed only makes the
// caller look again).
func (s *Store) Changes() int64 { return s.changes.Load() }

// Open opens (creating and migrating if needed) the database at path.
func Open(path string) (*Store, error) {
	if err := checkMisplaced(path); err != nil {
		return nil, err
	}
	dsn := fileURI(path) + "?_txlock=immediate" +
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
	s := &Store{db: db, root: filepath.Dir(path)}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

// FileURI is fileURI, for tests and benchmarks that open a database file
// of their own: append "?" and parameters to it.
func FileURI(path string) string { return fileURI(path) }

// fileURI is path as an SQLite URI filename (sqlite.org/uri.html), with
// every byte but letters, digits, "-._~", '/' and ':' percent-escaped, so
// that SQLite opens exactly path ('?', '#' and '%' have a meaning in a
// URI) and the driver reads only our parameters. An absolute path takes
// an empty authority, file:///dir/file, and on Windows the documented
// form file:///C:/dir/file; a relative one stays relative, file:dir/file.
func fileURI(path string) string {
	p := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	var b strings.Builder
	b.WriteString("file:")
	if strings.HasPrefix(p, "/") {
		b.WriteString("//")
	}
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', strings.IndexByte("-._~/:", c) >= 0:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}

// misplacedPath is where a2p2 0.2.3 and earlier opened path, which they
// gave SQLite unescaped, as SQLite's URI parser read it: cut at the first
// '?' or '#', each %XX decoded, and cut at a %00 (SQLite ignores the rest
// of the path there). ok is false if that is path itself.
func misplacedPath(path string) (string, bool) {
	p := filepath.ToSlash(path)
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '%' && i+2 < len(p) && isHex(p[i+1]) && isHex(p[i+2]) {
			c := unhex(p[i+1])<<4 | unhex(p[i+2])
			if c == 0 {
				break
			}
			b.WriteByte(c)
			i += 2
			continue
		}
		b.WriteByte(p[i])
	}
	old := filepath.FromSlash(b.String())
	return old, old != path
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c <= 'F':
		return c - 'A' + 10
	}
	return c - 'a' + 10
}

// checkMisplaced refuses to create a new database at path when there is
// none yet and an a2p2 database is where an older a2p2 opened path
// (misplacedPath): opening would start an empty node beside it. Anything
// else there (no file, a directory, another program's file or SQLite
// database) does not stop the open.
func checkMisplaced(path string) error {
	old, ok := misplacedPath(path)
	if !ok {
		return nil
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	version, ok := isA2P2Database(old)
	if !ok {
		return nil
	}
	return fmt.Errorf("%q is an a2p2 database (schema version %d), at the path a2p2 0.2.3 and earlier opened "+
		"for %q: they cut a path at its '?' or '#' and decoded its '%%'. If it is this home's, stop a2p2 and "+
		"move it, with its %q and %q files if they exist, to %q (and %q, %q), which must not exist yet; then start "+
		"again. If it is not, move it away from %q",
		old, version, path, old+"-wal", old+"-shm", path, path+"-wal", path+"-shm", old)
}

// isA2P2Database reports whether the file at path (a link is followed, as
// SQLite did) is an SQLite database with this store's marks: a schema
// version it sets, and its tables meta, rooms and agents. It opens it
// read-only and changes nothing in it.
func isA2P2Database(path string) (int, bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return 0, false
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	head := make([]byte, 16)
	_, err = io.ReadFull(f, head)
	f.Close()
	if err != nil || string(head) != "SQLite format 3\x00" {
		return 0, false
	}
	db, err := sql.Open("sqlite", fileURI(path)+"?mode=ro")
	if err != nil {
		return 0, false
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var v, n int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v < 1 {
		return 0, false
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('meta', 'rooms', 'agents')`).Scan(&n); err != nil || n != 3 {
		return 0, false
	}
	return v, true
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
	if h := txHook; h != nil {
		defer h(true, time.Now())
	}
	ts := &txState{s: s, write: true}
	txStates.Store(tx, ts)
	defer txStates.Delete(tx)
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	s.changes.Add(1)
	if len(ts.notes) == 0 && len(ts.drop) == 0 {
		return tx.Commit()
	}
	// The notes reach the cache before any read can see what they say:
	// a read asks the cache under its lock, held from before the commit.
	s.texts.mu.Lock()
	defer s.texts.mu.Unlock()
	if err := tx.Commit(); err != nil {
		s.texts.dropped() // committed or not: read again
		return err
	}
	s.texts.committed(ts)
	return nil
}

// txHook, when set, is told of every transaction once it ends: whether it
// could write, and when it began (it holds the one connection from then).
// Only the benchmark build sets it (bench_hooks.go).
var txHook func(write bool, began time.Time)

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
