package store

// Signed records, streams, fork evidence and per-agent reading state
// (DESIGN sections 2, 5 and 6; PROTOCOL section 8). This file is
// migration 2 and the methods over it. Migration 10 (notice.go) drops the
// v0.2 tables.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// schemaRecords is migration 2. The v0.2 rooms table gains the columns of
// PROTOCOL section 8 (its old owner and version columns stay until 3b
// drops the v0.2 data); the v0.2 meta table already has the key/value shape
// section 8 asks for and is reused as it is. log, observed, refusals,
// invites (new shape) and requests come with the steps that use them.
//
// Beyond section 8: forks holds fork evidence (at most MaxForksPerPosition
// refused records per position); reads.msg names the message an entry
// concerns, indexed, so a reader finds an agent's confirmations of one
// message without loading them all; registrations records when each agent
// was first registered.
const schemaRecords = `
ALTER TABLE rooms ADD COLUMN create_id TEXT NOT NULL DEFAULT '';
ALTER TABLE rooms ADD COLUMN gen INTEGER NOT NULL DEFAULT 0;
ALTER TABLE rooms ADD COLUMN joined_at INTEGER NOT NULL DEFAULT 0;
CREATE TABLE records (
  room TEXT NOT NULL, id TEXT NOT NULL, author TEXT NOT NULL, epoch TEXT NOT NULL,
  seq INTEGER NOT NULL, kind TEXT NOT NULL, ref TEXT NOT NULL, parent TEXT NOT NULL,
  thread TEXT NOT NULL, agent TEXT NOT NULL, created INTEGER NOT NULL,
  hdr BLOB NOT NULL, sig BLOB NOT NULL, body BLOB,
  reason TEXT NOT NULL, effect TEXT NOT NULL,
  PRIMARY KEY (room, id), UNIQUE (room, author, epoch, seq));
CREATE INDEX records_ref ON records (room, ref);
CREATE INDEX records_parent ON records (room, parent);
CREATE INDEX records_thread ON records (room, thread);
CREATE INDEX records_expiry ON records (created) WHERE reason IN ('live', 'missing');
CREATE TABLE streams (
  room TEXT NOT NULL, author TEXT NOT NULL, epoch TEXT NOT NULL,
  contig INTEGER NOT NULL, known INTEGER NOT NULL DEFAULT 0, known_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (room, author, epoch));
CREATE TABLE forks (
  room TEXT NOT NULL, author TEXT NOT NULL, epoch TEXT NOT NULL, seq INTEGER NOT NULL,
  id TEXT NOT NULL, hdr BLOB NOT NULL, sig BLOB NOT NULL, at INTEGER NOT NULL,
  PRIMARY KEY (room, id));
CREATE INDEX forks_position ON forks (room, author, epoch, seq);
CREATE TABLE registrations (agent TEXT PRIMARY KEY, at INTEGER NOT NULL);
CREATE TABLE cursors (
  agent TEXT NOT NULL, room TEXT NOT NULL, author TEXT NOT NULL, epoch TEXT NOT NULL,
  start INTEGER NOT NULL, pos INTEGER NOT NULL,
  PRIMARY KEY (agent, room, author, epoch));
CREATE TABLE reads (
  agent TEXT NOT NULL, room TEXT NOT NULL, key TEXT NOT NULL, version TEXT NOT NULL,
  msg TEXT NOT NULL,
  PRIMARY KEY (agent, room, key));
CREATE INDEX reads_msg ON reads (agent, room, msg, key);
CREATE TABLE read_heads (
  agent TEXT NOT NULL, room TEXT NOT NULL, author TEXT NOT NULL, record TEXT NOT NULL,
  PRIMARY KEY (agent, room, author, record));
`

// Reasons a held record has, or lacks, its text [R6]. Every reason but
// ReasonMissing rests on this machine's own evidence.
const (
	ReasonLive        = "live"         // text here (or the kind has none)
	ReasonExpired     = "expired"      // this machine's clock passed 30 days once; never undone
	ReasonWithdrawn   = "withdrawn"    // a withdraw by the same author is held
	ReasonNotAdmitted = "not_admitted" // this machine may not read it
	ReasonMissing     = "missing"      // text not here yet; any holder may repair it
)

// Effects of a record's ref or parent (PROTOCOL section 2, after
// placement). Records of other rooms are never targets. Only a record whose
// effect is ok counts.
const (
	EffectOK           = "ok"
	EffectBadReference = "bad_reference"
	EffectUnresolved   = "unresolved" // the target is not held yet
)

// Retention is how long text is kept: 30 days after its created time, by
// this machine's clock [R6].
const Retention = 30 * 24 * time.Hour

// TextExpired reports whether text created at created is past retention at now.
func TextExpired(created, now time.Time) bool { return !now.Before(created.Add(Retention)) }

// futureSlack is how far ahead of this machine's clock a record may be dated
// and still be stored (PROTOCOL P1).
const futureSlack = 24 * time.Hour

// MaxForksPerPosition bounds the fork evidence kept: at most this many
// refused records for one position of one stream.
const MaxForksPerPosition = 3

// Outcome is what placement did with a record (PROTOCOL P1 to P8).
// Only OutcomeStored and OutcomeRepaired change records.
type Outcome string

const (
	OutcomeStored      Outcome = "stored"       // P8: the next position of its stream [R4]
	OutcomeRepaired    Outcome = "repaired"     // P2: text for a header held without it
	OutcomeHeld        Outcome = "held"         // P3: the same record again: nothing to do
	OutcomeFork        Outcome = "fork"         // P4, P6: a different record at a held position; kept as evidence
	OutcomeNotNext     Outcome = "not_next"     // P5: not the next position of its stream
	OutcomeLinkMissing Outcome = "link_missing" // P7: a first record whose links are not all held
	OutcomeFuture      Outcome = "future"       // P1: dated more than 24 h ahead
)

// ErrUnverified is returned for a nil or empty verified record.
var ErrUnverified = errors.New("record is not verified")

// NotPlaced is Write's error when the record its build step returned was
// not stored; nothing was written.
type NotPlaced struct{ Outcome Outcome }

func (e *NotPlaced) Error() string { return "record not placed: " + string(e.Outcome) }

// PutOpts are the facts about this machine that placement needs.
type PutOpts struct {
	Now time.Time
	// MayRead says this machine may hold the record's text (its history
	// permission). False stores the header as not_admitted. Local writes
	// always may.
	MayRead bool
	// Undecided says whether this machine may hold the text is not known
	// yet (a head of its admission frontier is not held): the header is
	// stored as missing whatever its age, and decided when the head
	// arrives.
	Undecided bool
}

// Held is one record as this machine holds it: the signed header (verified
// when it was stored), the body when Reason is live and the kind has one,
// and what its reference does. Reason is the durable mark; a reader also
// treats text past Retention as expired (see TextExpired).
type Held struct {
	Record *core.Record
	Reason string
	Effect string
}

// Stream is one stream of one author: every position in [1, Contig] is
// held. A stream can be known (from a peer's report) before anything of it
// is held: then Contig is 0 and First is empty.
type Stream struct {
	Author string
	Epoch  string
	Contig int64
	// Known is the highest position a peer reported, and KnownAt when this
	// machine learned it.
	Known   int64
	KnownAt time.Time
	First   Held // the record at position 1, whose links order the stream
}

// Cursor is one agent's place in one stream [R7]: every position at or below
// Pos is resolved for the agent; Start is where its time began.
type Cursor struct {
	Author string
	Epoch  string
	Start  int64
	Pos    int64
}

// Read is one confirmed inbox entry: its key, the version of the text shown
// (a record id; empty for notices without text) and the message it
// concerns.
type Read struct {
	Key     string
	Version string
	Msg     string
}

// SeenPlan is what an agent's confirmation writes: the reads it confirms,
// its heads per author (each author listed replaces that author's heads)
// and cursors to move.
type SeenPlan struct {
	Reads   []Read
	Heads   map[string][]string
	Cursors []Cursor
}

// Fork is one refused record kept as evidence of a fork.
type Fork struct {
	Author string
	Epoch  string
	Seq    int64
	ID     string
	Header []byte
	Sig    []byte
	At     time.Time
}

// testHook, when set by a test, runs between the statements of a write and
// can fail it there, to show that a failure leaves nothing half applied.
var testHook func(point string) error

func hook(point string) error {
	if testHook != nil {
		return testHook(point)
	}
	return nil
}

// AddRoom records a room this machine holds records for, if it is not
// already known (gen starts at 1), and starts every registered agent's
// reading in it at the room's current end (step 4 will start them at the
// admission frontier instead; see seedCursors).
func (s *Store) AddRoom(ctx context.Context, id, label, createID string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO rooms (id, label, create_id, gen, joined_at)
			VALUES (?,?,?,1,?) ON CONFLICT (id) DO NOTHING`, id, label, createID, ms(now))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		rows, err := tx.Query(`SELECT agent FROM registrations`)
		if err != nil {
			return err
		}
		var agents []string
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				rows.Close()
				return err
			}
			agents = append(agents, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, a := range agents {
			if err := seedCursors(tx, a, id, currentEnd); err != nil {
				return err
			}
		}
		return nil
	})
}

// Register records agent as registered, if it was not, and in the same
// transaction starts its reading at the current end of every stream of
// every room, so history from before it arrived is not unread for it
// (v0.2's behaviour). It reports whether the agent is new.
func (s *Store) Register(ctx context.Context, agent string, now time.Time) (bool, error) {
	fresh := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO registrations (agent, at) VALUES (?,?) ON CONFLICT DO NOTHING`, agent, ms(now))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		fresh = true
		rows, err := tx.Query(`SELECT id FROM rooms WHERE gen > 0`)
		if err != nil {
			return err
		}
		var rooms []string
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				rows.Close()
				return err
			}
			rooms = append(rooms, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range rooms {
			if err := seedCursors(tx, agent, r, currentEnd); err != nil {
				return err
			}
		}
		return nil
	})
	return fresh, err
}

// currentEnd is where an agent registered now starts in a stream: after
// everything held.
func currentEnd(s Stream) int64 { return s.Contig }

// seedCursors starts agent's reading in room at frontier(stream) for every
// stream held, leaving existing cursors alone. This is the hook for the
// admission frontier of step 4: a machine admitted without history starts
// its agents at the admitter's positions, not at the current end.
func seedCursors(tx *sql.Tx, agent, room string, frontier func(Stream) int64) error {
	ss, err := (&Reader{tx: tx}).Streams(room)
	if err != nil {
		return err
	}
	for _, st := range ss {
		n := frontier(st)
		if n <= 0 {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO cursors (agent, room, author, epoch, start, pos) VALUES (?,?,?,?,?,?)
			ON CONFLICT DO NOTHING`, agent, room, st.Author, st.Epoch, n, n); err != nil {
			return err
		}
	}
	return nil
}

// PutRecord places one verified record under the gap-free rule [R4], in one
// transaction: the path for records received from peers. Local writes use
// Write, which places the same way.
//
// A record is stored only at the next position of its stream with matching
// prev; a first record only when every link is held. A different record at
// a held position is a fork: refused, and kept as evidence. The same record
// again is a no-op, except that a body for a header held with its text
// missing repairs it when this machine may read it. A stored withdraw drops
// the text of its message and of the author's replacements of it [R7].
// Text is kept only when no evidence here says it must go [R6].
func (s *Store) PutRecord(ctx context.Context, r *core.VerifiedRecord, o PutOpts) (Outcome, error) {
	if r == nil || r.ID() == "" {
		return "", ErrUnverified
	}
	var out Outcome
	err := s.tx(ctx, func(tx *sql.Tx) (err error) {
		out, err = place(tx, r, o)
		return err
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// Write runs build inside one write transaction, with a Reader over it,
// and stores the verified record build returns, under the same placement
// as PutRecord with MayRead set. Whatever build decides (a version, the
// heads to link, the next position) cannot be raced by another writer. If
// the record is not stored, the error is a *NotPlaced and nothing is
// written.
func (s *Store) Write(ctx context.Context, now time.Time, build func(*Reader) (*core.VerifiedRecord, error)) (*core.VerifiedRecord, error) {
	var r *core.VerifiedRecord
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		if r, err = build(&Reader{tx: tx}); err != nil {
			return err
		}
		if r == nil || r.ID() == "" {
			return ErrUnverified
		}
		out, err := place(tx, r, PutOpts{Now: now, MayRead: true})
		if err != nil {
			return err
		}
		if out != OutcomeStored {
			return &NotPlaced{out}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

func place(tx *sql.Tx, r *core.VerifiedRecord, o PutOpts) (Outcome, error) {
	f := r.Fields()
	rd := &Reader{tx: tx}
	var x int
	switch err := tx.QueryRow(`SELECT 1 FROM rooms WHERE id = ? AND left_at = 0`, f.Room).Scan(&x); {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrNotFound
	case err != nil:
		return "", err
	}
	if f.Created.After(o.Now.Add(futureSlack)) { // P1
		return OutcomeFuture, nil
	}
	if h, ok, err := rd.Record(f.Room, r.ID()); err != nil {
		return "", err
	} else if ok { // P2, P3
		return repair(tx, rd, h, r, o)
	}
	var contig int64
	switch err := tx.QueryRow(`SELECT contig FROM streams WHERE room = ? AND author = ? AND epoch = ?`,
		f.Room, r.Author(), f.Epoch).Scan(&contig); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", err
	}
	switch {
	case f.Seq <= contig: // P4: the id is not held, so the record there differs
		return OutcomeFork, keepFork(tx, r, f, o.Now)
	case f.Seq != contig+1: // P5
		return OutcomeNotNext, nil
	}
	if f.Seq > 1 { // P6
		id, err := idAt(tx, f.Room, r.Author(), f.Epoch, f.Seq-1)
		if err != nil {
			return "", err
		}
		if id != f.Prev {
			return OutcomeFork, keepFork(tx, r, f, o.Now)
		}
	}
	for _, l := range f.Links { // P7
		id, err := idAt(tx, f.Room, r.Author(), l.Epoch, l.Seq)
		if err != nil {
			return "", err
		}
		if id != l.ID {
			return OutcomeLinkMissing, nil
		}
	}

	// P8.
	reason, err := reasonFor(rd, r.ID(), r.Author(), f, o, r.Body() != nil)
	if err != nil {
		return "", err
	}
	effect, err := effectOf(rd, r.Author(), f)
	if err != nil {
		return "", err
	}
	var body []byte
	if reason == ReasonLive {
		body = r.Body()
	}
	if _, err := tx.Exec(`INSERT INTO records (room, id, author, epoch, seq, kind, ref, parent, thread, agent,
		created, hdr, sig, body, reason, effect) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		f.Room, r.ID(), r.Author(), f.Epoch, f.Seq, string(f.Kind), f.Ref, f.Parent, f.Thread, f.Agent,
		ms(f.Created), r.Header(), r.Sig(), body, reason, effect); err != nil {
		return "", err
	}
	noteText(tx, textNote{f.Room, r.Author(), f.Epoch, r.ID(), reason, f.Seq, ms(f.Created)})
	if err := hook("put:inserted"); err != nil {
		return "", err
	}
	if f.Files != "" && (f.Kind == core.KindMessage || f.Kind == core.KindReplace) {
		if err := fileSetTx(tx, f.Room, r.ID(), f.Files); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(`INSERT INTO streams (room, author, epoch, contig) VALUES (?,?,?,?)
		ON CONFLICT (room, author, epoch) DO UPDATE SET contig = excluded.contig`,
		f.Room, r.Author(), f.Epoch, f.Seq); err != nil {
		return "", err
	}
	if err := hook("put:stream"); err != nil {
		return "", err
	}
	if f.Kind == core.KindWithdraw {
		// [R7]: the message and every replacement of it by the same author
		// lose their text, whatever reason they had (withdrawn overrides
		// expired, as in the model's setBody). A withdraw naming anything
		// but a message does nothing.
		if err := execNoted(tx, sqlWithdrawMessage, ReasonWithdrawn, f.Room, f.Ref, r.Author()); err != nil {
			return "", err
		}
		if err := execNoted(tx, sqlWithdrawReplaces, ReasonWithdrawn, f.Room, f.Ref, r.Author(), f.Room, f.Ref); err != nil {
			return "", err
		}
		if err := hook("put:withdrawn"); err != nil {
			return "", err
		}
	}
	if err := resolveDependants(tx, rd, f.Room, r.ID()); err != nil {
		return "", err
	}
	return OutcomeStored, nil
}

// keepFork keeps a refused record's verified header and signature as
// evidence, at most MaxForksPerPosition per position.
func keepFork(tx *sql.Tx, r *core.VerifiedRecord, f core.RecordFields, now time.Time) error {
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM forks WHERE room = ? AND author = ? AND epoch = ? AND seq = ?`,
		f.Room, r.Author(), f.Epoch, f.Seq).Scan(&n); err != nil {
		return err
	}
	if n >= MaxForksPerPosition {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO forks (room, author, epoch, seq, id, hdr, sig, at) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT DO NOTHING`, f.Room, r.Author(), f.Epoch, f.Seq, r.ID(), r.Header(), r.Sig(), ms(now))
	return err
}

// repair is P2 and P3 for a record already held.
func repair(tx *sql.Tx, rd *Reader, h Held, r *core.VerifiedRecord, o PutOpts) (Outcome, error) {
	if h.Reason != ReasonMissing || r.Body() == nil || !o.MayRead {
		return OutcomeHeld, nil
	}
	f := h.Record.Fields()
	reason, err := reasonFor(rd, r.ID(), r.Author(), f, o, true)
	if err != nil {
		return "", err
	}
	var body []byte
	if reason == ReasonLive {
		body = r.Body()
	}
	if _, err := tx.Exec(`UPDATE records SET body = ?, reason = ? WHERE room = ? AND id = ?`,
		body, reason, f.Room, r.ID()); err != nil {
		return "", err
	}
	noteText(tx, textNote{f.Room, h.Record.Author(), f.Epoch, r.ID(), reason, f.Seq, ms(f.Created)})
	if reason == ReasonLive {
		return OutcomeRepaired, nil
	}
	return OutcomeHeld, nil
}

// reasonFor is the model's textGone then setBody: withdrawn, then expired by
// this machine's clock, then not admitted; otherwise live if the text is
// here (or the kind has none) and missing if not.
func reasonFor(rd *Reader, id, author string, f core.RecordFields, o PutOpts, hasBody bool) (string, error) {
	target := ""
	switch f.Kind {
	case core.KindMessage:
		target = id
	case core.KindReplace:
		target = f.Ref
	}
	if target != "" {
		var n int
		if err := rd.tx.QueryRow(sqlWithdrawsOf, f.Room, target, author).Scan(&n); err != nil {
			return "", err
		}
		if n > 0 {
			return ReasonWithdrawn, nil
		}
	}
	switch { // permission first: what is not this machine's to read is not_admitted, expired or not
	case !o.MayRead:
		return ReasonNotAdmitted, nil
	case o.Undecided:
		return ReasonMissing, nil
	case TextExpired(f.Created, o.Now):
		return ReasonExpired, nil
	case hasBody || (f.Kind != core.KindMessage && f.Kind != core.KindReplace):
		return ReasonLive, nil
	}
	return ReasonMissing, nil
}

// effectOf resolves a record's ref or parent against what is held
// (PROTOCOL section 2): a withdraw or replace needs a message by the
// same author; a handled one its author's agent may report; a reply's
// thread must be its parent's thread, or the parent's id if it has none.
func effectOf(rd *Reader, author string, f core.RecordFields) (string, error) {
	target := f.Ref
	if f.Kind == core.KindMessage {
		target = f.Parent
	}
	if target == "" {
		return EffectOK, nil
	}
	t, ok, err := rd.Record(f.Room, target)
	if err != nil || !ok {
		return EffectUnresolved, err
	}
	tf := t.Record.Fields()
	good := tf.Kind == core.KindMessage
	switch f.Kind {
	case core.KindMessage:
		want := tf.Thread
		if want == "" {
			want = t.Record.ID()
		}
		good = good && f.Thread == want
	case core.KindWithdraw, core.KindReplace:
		good = good && t.Record.Author() == author
	case core.KindHandled:
		good = good && CanHandle(t.Record, author, f.Agent)
	}
	if good {
		return EffectOK, nil
	}
	return EffectBadReference, nil
}

// CanHandle says whether agent on node may report handling msg: it is an
// addressee, or msg is a room-wide post that asked and agent is not its
// author (the model's canHandle).
func CanHandle(msg *core.Record, node, agent string) bool {
	f := msg.Fields()
	if f.Kind != core.KindMessage {
		return false
	}
	if len(f.To) == 0 {
		return f.AskHandled && !(msg.Author() == node && f.Agent == agent)
	}
	for _, a := range f.To {
		if a.Node == node && a.Agent == agent {
			return true
		}
	}
	return false
}

// The lookups by one key column: a record's dependants, the withdraws of
// a message, what refers to a message, its replies and its thread. Each
// seeks on the index of its key (records_ref, records_parent,
// records_thread or the primary key), so it reads only the records that
// match and not the room's history (D56). Two things would make the
// planner, which has no statistics here, read the whole room instead:
//
//   - An ORDER BY author, epoch, seq: it prefers the unique (room, author,
//     epoch, seq) index, which returns that order, over the key's. So these
//     return their rows unordered and byPosition sorts them.
//   - A term on kind: records_reading leads with (room, kind), which ties
//     with (room, key). So kind is written +kind, which no index can use.
//
// An OR of two keys is two lookups, the second leaving out in SQL what the
// first matched, so the rows decoded are exactly those the OR named, each
// once, whatever the header says.
// TestPerRecordLookupsSeekOnTheirKeyNotOnTheWholeRoom checks each plan.
const (
	whereDependantsByRef    = `WHERE room = ? AND ref = ? AND effect = ?`
	whereDependantsByParent = `WHERE room = ? AND parent = ? AND effect = ? AND ref != ?`
	sqlWithdrawsOf          = `SELECT count(*) FROM records WHERE room = ? AND ref = ? AND author = ? AND +kind = 'withdraw'`
	whereByRef              = `WHERE room = ? AND ref = ?`
	whereByParent           = `WHERE room = ? AND parent = ? AND +kind = 'message'`
	whereByThread           = `WHERE room = ? AND thread = ? AND +kind = 'message'`
	whereThreadRoot         = `WHERE room = ? AND id = ? AND +kind = 'message' AND thread != ?`
	// A withdraw drops the text of its message and of the author's
	// replacements of it, if the target is a message (or not held).
	// Both return what they changed, for the summary cache (noted).
	sqlWithdrawMessage = `UPDATE records SET body = NULL, reason = ? WHERE room = ? AND id = ? AND author = ? AND +kind = 'message'
		` + returningNoted
	sqlWithdrawReplaces = `UPDATE records SET body = NULL, reason = ? WHERE room = ? AND ref = ? AND author = ? AND +kind = 'replace'
		AND NOT EXISTS (SELECT 1 FROM records t WHERE t.room = ? AND t.id = ? AND t.kind != 'message') ` + returningNoted
)

// positioned is a record with its stored position (author, epoch, seq),
// unique in a room: the key ByRef, ByParent and ByThread sort by, read from
// the same columns SQLite's ORDER BY author, epoch, seq used.
type positioned struct {
	author, epoch string
	seq           int64
	h             Held
}

const positionedCols = `author, epoch, seq, ` + heldCols

func (rd *Reader) queryPositioned(where string, args ...any) ([]positioned, error) {
	rows, err := rd.tx.Query(`SELECT `+positionedCols+` FROM records `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []positioned
	for rows.Next() {
		var p positioned
		if p.h, err = scanHeld(rows, &p.author, &p.epoch, &p.seq); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// byPosition sorts ps as ORDER BY author, epoch, seq does (BINARY collation
// compares text bytewise, as Go compares strings) and returns the records,
// nil when there are none, as Reader.query returns.
func byPosition(ps []positioned) []Held {
	if len(ps) == 0 {
		return nil
	}
	sort.Slice(ps, func(i, j int) bool {
		a, b := ps[i], ps[j]
		if a.author != b.author {
			return a.author < b.author
		}
		if a.epoch != b.epoch {
			return a.epoch < b.epoch
		}
		return a.seq < b.seq
	})
	out := make([]Held, len(ps))
	for i, p := range ps {
		out[i] = p.h
	}
	return out
}

// resolveDependants re-resolves the records that were waiting for id,
// whatever its kind: a reference to a record of the wrong kind or author
// becomes bad_reference as soon as the target is learned.
func resolveDependants(tx *sql.Tx, rd *Reader, room, id string) error {
	deps, err := dependants(rd, room, id)
	if err != nil {
		return err
	}
	for _, d := range deps {
		e, err := effectOf(rd, d.Record.Author(), d.Record.Fields())
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE records SET effect = ? WHERE room = ? AND id = ?`,
			e, room, d.Record.ID()); err != nil {
			return err
		}
	}
	return nil
}

// dependants returns the unresolved records of room whose ref or parent is
// id, each once (in no order that matters: each is re-resolved alone).
func dependants(rd *Reader, room, id string) ([]Held, error) {
	deps, err := rd.query(whereDependantsByRef, room, id, EffectUnresolved)
	if err != nil {
		return nil, err
	}
	byParent, err := rd.query(whereDependantsByParent, room, id, EffectUnresolved, id)
	return append(deps, byParent...), err
}

func idAt(tx *sql.Tx, room, author, epoch string, seq int64) (string, error) {
	var id string
	err := tx.QueryRow(`SELECT id FROM records WHERE room = ? AND author = ? AND epoch = ? AND seq = ?`,
		room, author, epoch, seq).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// ExpireText is the pruning pass [R6]: every record whose text is held or
// missing and whose created time is 30 days or more before now (this
// machine's clock) is marked expired. Nothing turns an expired record back.
// It returns how many records changed.
func (s *Store) ExpireText(ctx context.Context, now time.Time) (int64, error) {
	return s.ExpireTextWith(ctx, now, nil)
}

// ExpireTextWith is ExpireText leaving missing records alone whose room
// and author undecided returns, read in the same transaction: whether this
// machine may read them is not decided yet. A missing record arrived before
// its retention passed (one that arrives later is stored expired); if it
// turns out to be text this machine may not read, it is not_admitted, which
// expiry never overrides [R6].
func (s *Store) ExpireTextWith(ctx context.Context, now time.Time, undecided func(*Reader) (map[[2]string]bool, error)) (int64, error) {
	var n int64
	cutoff := ms(now.Add(-Retention))
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var keep map[[2]string]bool
		if undecided != nil {
			var err error
			if keep, err = undecided(&Reader{tx: tx}); err != nil {
				return err
			}
		}
		const q = `UPDATE records SET body = NULL, reason = ? WHERE reason IN ('live', 'missing') AND created <= ? ` + returningNoted
		if len(keep) == 0 {
			c, err := queryNoted(tx, q, ReasonExpired, cutoff)
			n = c
			return err
		}
		// The pairs to keep are left out in Go, not as one SQL variable
		// each: a room can have more undecided pairs than SQLite takes
		// variables (F32). Live text expires in one statement. Missing
		// text past retention is expired one (room, author) pair at a
		// time, for the pairs not kept: one statement per such pair, on
		// the (room, author) prefix of the records position index.
		var err error
		if n, err = queryNoted(tx, `UPDATE records SET body = NULL, reason = ? WHERE reason = 'live' AND created <= ? `+returningNoted,
			ReasonExpired, cutoff); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT DISTINCT room, author FROM records WHERE reason = 'missing' AND created <= ?`, cutoff)
		if err != nil {
			return err
		}
		var pairs [][2]string
		for rows.Next() {
			var k [2]string
			if err := rows.Scan(&k[0], &k[1]); err != nil {
				rows.Close()
				return err
			}
			if !keep[k] {
				pairs = append(pairs, k)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, k := range pairs {
			c, err := queryNoted(tx, `UPDATE records SET body = NULL, reason = ? WHERE reason = 'missing' AND created <= ?
				AND room = ? AND author = ? `+returningNoted, ReasonExpired, cutoff, k[0], k[1])
			if err != nil {
				return err
			}
			n += c
		}
		return nil
	})
	return n, err
}

// NoteKnown records that a peer reported holding stream (author, epoch) of
// room up to position known, learned at at. A stream not held yet is
// created with contig 0. Known only grows.
func (s *Store) NoteKnown(ctx context.Context, room, author, epoch string, known int64, at time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO streams (room, author, epoch, contig, known, known_at) VALUES (?,?,?,0,?,?)
			ON CONFLICT (room, author, epoch) DO UPDATE SET known = excluded.known, known_at = excluded.known_at
			WHERE excluded.known > streams.known`, room, author, epoch, known, ms(at))
		return err
	})
}

// View runs fn with a Reader over one read-only transaction, so everything
// it reads is one consistent state. Nothing is written.
func (s *Store) View(ctx context.Context, fn func(*Reader) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	if h := txHook; h != nil {
		defer h(false, time.Now())
	}
	defer tx.Rollback()
	txStates.Store(tx, &txState{s: s})
	defer txStates.Delete(tx)
	return fn(&Reader{tx: tx})
}

// Seen runs plan over the current state, inside one write transaction, and
// applies the reads, heads and cursors it returns [R7]. The plan decides
// which confirmations are valid; a read confirmed again takes the new
// version.
func (s *Store) Seen(ctx context.Context, agent, room string, plan func(*Reader) (SeenPlan, error)) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		p, err := plan(&Reader{tx: tx})
		if err != nil {
			return err
		}
		for _, r := range p.Reads {
			if _, err := tx.Exec(`INSERT INTO reads (agent, room, key, version, msg) VALUES (?,?,?,?,?)
				ON CONFLICT (agent, room, key) DO UPDATE SET version = excluded.version, msg = excluded.msg`,
				agent, room, r.Key, r.Version, r.Msg); err != nil {
				return err
			}
			// The confirmation replaces the receipt it needed (shown.go).
			if _, err := tx.Exec(`DELETE FROM shown WHERE agent = ? AND room = ? AND key = ?`,
				agent, room, r.Key); err != nil {
				return err
			}
		}
		if err := hook("seen:reads"); err != nil {
			return err
		}
		for author, ids := range p.Heads {
			if _, err := tx.Exec(`DELETE FROM read_heads WHERE agent = ? AND room = ? AND author = ?`,
				agent, room, author); err != nil {
				return err
			}
			for _, id := range ids {
				if _, err := tx.Exec(`INSERT INTO read_heads (agent, room, author, record) VALUES (?,?,?,?)
					ON CONFLICT DO NOTHING`, agent, room, author, id); err != nil {
					return err
				}
			}
		}
		if err := hook("seen:heads"); err != nil {
			return err
		}
		for _, c := range p.Cursors {
			if _, err := tx.Exec(`INSERT INTO cursors (agent, room, author, epoch, start, pos) VALUES (?,?,?,?,?,?)
				ON CONFLICT (agent, room, author, epoch) DO UPDATE SET pos = excluded.pos`,
				agent, room, c.Author, c.Epoch, c.Start, c.Pos); err != nil {
				return err
			}
		}
		return nil
	})
}

// Reader reads one consistent state inside a transaction. It is only valid
// inside the View, Write or Seen call that made it.
type Reader struct{ tx *sql.Tx }

const heldCols = `hdr, sig, body, reason, effect`

// scanHeld scans heldCols, after the columns pre names when there are any.
func scanHeld(sc interface{ Scan(...any) error }, pre ...any) (Held, error) {
	var hdr, sig, body []byte
	var h Held
	if err := sc.Scan(append(pre, &hdr, &sig, &body, &h.Reason, &h.Effect)...); err != nil {
		return Held{}, err
	}
	r, err := core.RecordFromParts(hdr, sig, body)
	if err != nil {
		// Never include bytes: a stored record that no longer parses is
		// corruption, reported by its check only.
		return Held{}, fmt.Errorf("stored record does not parse: %w", err)
	}
	h.Record = r
	return h, nil
}

func (rd *Reader) query(where string, args ...any) ([]Held, error) {
	rows, err := rd.tx.Query(`SELECT `+heldCols+` FROM records `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Held
	for rows.Next() {
		h, err := scanHeld(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Record returns the record with id in room, if held.
func (rd *Reader) Record(room, id string) (Held, bool, error) {
	h, err := scanHeld(rd.tx.QueryRow(`SELECT `+heldCols+` FROM records WHERE room = ? AND id = ?`, room, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Held{}, false, nil
	case err != nil:
		return Held{}, false, err
	}
	return h, true, nil
}

// RecordsFrom returns the records of one stream at position from and after,
// in order.
func (rd *Reader) RecordsFrom(room, author, epoch string, from int64) ([]Held, error) {
	return rd.query(`WHERE room = ? AND author = ? AND epoch = ? AND seq >= ? ORDER BY seq`,
		room, author, epoch, from)
}

// RecordsFromN is RecordsFrom of at most n records: what a page can carry
// of a stream, without reading the rest of it.
func (rd *Reader) RecordsFromN(room, author, epoch string, from int64, n int) ([]Held, error) {
	return rd.query(`WHERE room = ? AND author = ? AND epoch = ? AND seq >= ? ORDER BY seq LIMIT ?`,
		room, author, epoch, from, n)
}

// ByRef returns the records whose ref is id (handled, withdraw, replace),
// whatever their effect, ordered by position.
func (rd *Reader) ByRef(room, id string) ([]Held, error) {
	ps, err := rd.queryPositioned(whereByRef, room, id)
	return byPosition(ps), err
}

// ByParent returns the messages whose parent is id, whatever their effect.
func (rd *Reader) ByParent(room, id string) ([]Held, error) {
	ps, err := rd.queryPositioned(whereByParent, room, id)
	return byPosition(ps), err
}

// ByThread returns the messages that name root as their thread, and root,
// whatever their effect.
func (rd *Reader) ByThread(room, root string) ([]Held, error) {
	ps, err := rd.queryPositioned(whereByThread, room, root)
	if err != nil {
		return nil, err
	}
	r, err := rd.queryPositioned(whereThreadRoot, room, root, root)
	if err != nil {
		return nil, err
	}
	return byPosition(append(ps, r...)), nil
}

// Streams returns every stream of room, held or only known, with its first
// record when held, ordered by author then epoch.
func (rd *Reader) Streams(room string) ([]Stream, error) {
	rows, err := rd.tx.Query(`SELECT s.author, s.epoch, s.contig, s.known, s.known_at, r.hdr, r.sig, r.body, r.reason, r.effect
		FROM streams s LEFT JOIN records r ON r.room = s.room AND r.author = s.author AND r.epoch = s.epoch AND r.seq = 1
		WHERE s.room = ? ORDER BY s.author, s.epoch`, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Stream
	for rows.Next() {
		var s Stream
		var hdr, sig, body []byte
		var knownAt int64
		var reason, effect sql.NullString
		if err := rows.Scan(&s.Author, &s.Epoch, &s.Contig, &s.Known, &knownAt, &hdr, &sig, &body, &reason, &effect); err != nil {
			return nil, err
		}
		if hdr != nil {
			r, err := core.RecordFromParts(hdr, sig, body)
			if err != nil {
				return nil, fmt.Errorf("stored record does not parse: %w", err)
			}
			s.First = Held{Record: r, Reason: reason.String, Effect: effect.String}
		}
		if knownAt != 0 {
			s.KnownAt = fromMS(knownAt)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Forks returns the fork evidence kept for one position.
func (rd *Reader) Forks(room, author, epoch string, seq int64) ([]Fork, error) {
	rows, err := rd.tx.Query(`SELECT id, hdr, sig, at FROM forks WHERE room = ? AND author = ? AND epoch = ? AND seq = ?
		ORDER BY id`, room, author, epoch, seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Fork
	for rows.Next() {
		f := Fork{Author: author, Epoch: epoch, Seq: seq}
		var at int64
		if err := rows.Scan(&f.ID, &f.Header, &f.Sig, &at); err != nil {
			return nil, err
		}
		f.At = fromMS(at)
		out = append(out, f)
	}
	return out, rows.Err()
}

// Registered reports whether agent is registered.
func (rd *Reader) Registered(agent string) (bool, error) {
	var n int
	err := rd.tx.QueryRow(`SELECT count(*) FROM registrations WHERE agent = ?`, agent).Scan(&n)
	return n > 0, err
}

// Cursors returns agent's cursors in room.
func (rd *Reader) Cursors(agent, room string) ([]Cursor, error) {
	rows, err := rd.tx.Query(`SELECT author, epoch, start, pos FROM cursors WHERE agent = ? AND room = ?
		ORDER BY author, epoch`, agent, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Cursor
	for rows.Next() {
		var c Cursor
		if err := rows.Scan(&c.Author, &c.Epoch, &c.Start, &c.Pos); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Read returns agent's confirmation of key in room, if any.
func (rd *Reader) Read(agent, room, key string) (Read, bool, error) {
	r := Read{Key: key}
	err := rd.tx.QueryRow(`SELECT version, msg FROM reads WHERE agent = ? AND room = ? AND key = ?`,
		agent, room, key).Scan(&r.Version, &r.Msg)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Read{}, false, nil
	case err != nil:
		return Read{}, false, err
	}
	return r, true, nil
}

// ReadsOf returns agent's confirmations in room of entries concerning
// message msg (by index).
func (rd *Reader) ReadsOf(agent, room, msg string) ([]Read, error) {
	return rd.reads(`WHERE agent = ? AND room = ? AND msg = ? ORDER BY key`, agent, room, msg)
}

// Reads returns all of agent's confirmations in room.
func (rd *Reader) Reads(agent, room string) ([]Read, error) {
	return rd.reads(`WHERE agent = ? AND room = ? ORDER BY key`, agent, room)
}

func (rd *Reader) reads(where string, args ...any) ([]Read, error) {
	rows, err := rd.tx.Query(`SELECT key, version, msg FROM reads `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Read
	for rows.Next() {
		var r Read
		if err := rows.Scan(&r.Key, &r.Version, &r.Msg); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReadHeads returns, per author, the records agent confirmed that no other
// confirmed record of that author follows.
func (rd *Reader) ReadHeads(agent, room string) (map[string][]string, error) {
	rows, err := rd.tx.Query(`SELECT author, record FROM read_heads WHERE agent = ? AND room = ?
		ORDER BY author, record`, agent, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var a, id string
		if err := rows.Scan(&a, &id); err != nil {
			return nil, err
		}
		out[a] = append(out[a], id)
	}
	return out, rows.Err()
}
