package store

// The room log, the member cache derived from it, and requests waiting for
// a person (DESIGN-next sections 3, 5 and 7; PROTOCOL-next section 8). This
// is migration 3 (migration 2, the records of records.go, has shipped);
// used only by the new path in internal/node until the surfaces switch
// over (step 3b).

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// schemaLog is migration 3: the room log.
//
// Differences from PROTOCOL-next section 8: the member cache is the table
// room_members, not members, because the v0.2 members table (same name,
// other columns) is still read by the v0.2 trust checks (MemberByNode,
// IsMember) until step 3b drops it, and a node listed only in a
// next-version room must not pass those; 3b may rename it. It also has rows for
// machines that were admitted but are not members now (member = 0,
// admitted = 1), which the reading rules need [RM1]. log has a reason
// column (why an entry is held, refused or invalid) and also keeps entries
// the membership machine found invalid (state invalid, bounded), so that a
// replay judges their dependants as before; requests has action (exactly
// what a person approves), seconds (the entry by another admin a request
// would match) and reason (why a confirmation failed). rooms gains left_at, the local tombstone of a
// room this machine left.
const schemaLog = `
ALTER TABLE rooms ADD COLUMN left_at INTEGER NOT NULL DEFAULT 0;
CREATE TABLE log (
  room TEXT NOT NULL, id TEXT NOT NULL, author TEXT NOT NULL, seq INTEGER NOT NULL,
  bytes BLOB NOT NULL, sig BLOB NOT NULL, state TEXT NOT NULL, reason TEXT NOT NULL,
  batch INTEGER NOT NULL, batch_pos INTEGER NOT NULL,
  PRIMARY KEY (room, id));
CREATE INDEX log_position ON log (room, author, seq);
CREATE UNIQUE INDEX log_arrival ON log (room, batch, batch_pos);
CREATE TABLE room_members (
  room TEXT NOT NULL, node TEXT NOT NULL, member INTEGER NOT NULL, admin INTEGER NOT NULL,
  admitted INTEGER NOT NULL, name TEXT NOT NULL, addrs TEXT NOT NULL DEFAULT '[]',
  seen_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (room, node));
CREATE TABLE requests (
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, room TEXT NOT NULL, node TEXT NOT NULL,
  name TEXT NOT NULL, role TEXT NOT NULL, action TEXT NOT NULL, history INTEGER NOT NULL, seconds TEXT NOT NULL,
  created_at INTEGER NOT NULL, state TEXT NOT NULL, decided_at INTEGER NOT NULL DEFAULT 0,
  entries TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '');
CREATE INDEX requests_room ON requests (room, state);
`

// States of a stored log entry. Dropped entries are not kept. Invalid ones
// are kept only so that a replay judges their dependants as before: they
// reserve nothing and are never passed on.
const (
	LogAccepted = "accepted"
	LogHeld     = "held"
	LogRefused  = "refused"
	LogInvalid  = "invalid"
)

// States of a request.
const (
	RequestPending  = "pending"
	RequestApproved = "approved" // confirmed, revalidated, signed and applied
	RequestDeclined = "declined" // nothing signed
	RequestFailed   = "failed"   // confirmed, but no longer valid: nothing signed
)

// ErrStaleGeneration is returned when a write was prepared under a room
// generation that is no longer current: the room was left (or left and
// joined again) since. Nothing is written.
var ErrStaleGeneration = errors.New("room generation changed")

// ErrLeft is returned for a room this machine has left.
var ErrLeft = errors.New("room was left")

// LogRow is one stored log entry. Header and Sig are the signed bytes
// exactly as received.
type LogRow struct {
	ID     string
	Author string
	Seq    int64
	Header []byte
	Sig    []byte
	State  string
	Reason string
	Batch  int64
	Pos    int
}

// RoomMember is one row of the member cache.
type RoomMember struct {
	Node     string
	Member   bool
	Admin    bool
	Admitted bool
	Name     string
}

// Request is a change waiting for a person (P3).
type Request struct {
	ID   string
	Kind string // the entry kind: add, remove, admin
	// Action is exactly what is approved: admit, admit_admin, promote or
	// remove; with Role (member, admin, or empty for a removal) and History.
	Action  string
	Room    string
	Node    string // the target
	Name    string
	Role    string
	History bool
	// Seconds is the entry by another admin this request would match,
	// when it was raised by that entry rather than proposed here.
	Seconds   string
	Created   time.Time
	State     string
	DecidedAt time.Time
	Entries   string // the entry signed on approval
	Reason    string // why it failed
}

// LogRoom is a room as the log path sees it.
type LogRoom struct {
	ID       string
	Label    string
	CreateID string
	Gen      int64
	Left     bool
}

// TextAccess is whether this machine may hold a record's text.
type TextAccess int

const (
	TextReadable TextAccess = iota
	TextHidden              // permanently, under the current permission: not_admitted
	// TextUndecided: the permission cannot be decided yet (a frontier
	// head is not held). The text is not held, but the record is missing,
	// not not_admitted, so a reader waits at it instead of passing it.
	TextUndecided
)

// Access is how membership changes what this machine holds of a room's
// records, applied inside the same transaction as the change.
type Access struct {
	// Readable, if set, re-decides the text of every record whose text is
	// held, missing or not admitted: hidden text is dropped (not_admitted);
	// undecided text is dropped and the record is missing; a record not
	// admitted that is readable or undecided now becomes missing, for a
	// holder to repair. Expired and withdrawn text is never touched.
	Readable func(author string, f core.RecordFields, id string) (TextAccess, error)
	// Frontier, if set, starts every registered agent's reading of each
	// held stream at Frontier(stream) when it has no cursor there yet (the
	// seedCursors hook): the admission frontier of a machine admitted
	// without history.
	Frontier func(Stream) (int64, error)
}

// LogBatch is what one arrival of log entries changes, written in one
// transaction.
type LogBatch struct {
	Room string
	// Gen, if not zero, must be the room's current generation.
	Gen int64
	// New are the entries that arrived in this batch and were kept, in
	// arrival order; Changed are earlier rows whose state changed; Gone
	// are earlier rows no longer kept (dropped or found invalid).
	New     []LogRow
	Changed []LogRow
	Gone    []string
	// Members are written into the member cache (inserted or replaced);
	// MembersGone are removed from it.
	Members     []RoomMember
	MembersGone []string
	// Requests are created; Complete, if set, is the request this batch
	// approves (its signed entry is in New), with Entries naming it.
	Requests []Request
	Complete string
	Entries  string
	// Access, if set, is called after the rows are written, with a Reader
	// over the same transaction.
	Access func(*Reader) (Access, error)
	Now    time.Time
}

func roomGen(tx *sql.Tx, room string) (gen int64, left bool, err error) {
	var leftAt int64
	err = tx.QueryRow(`SELECT gen, left_at FROM rooms WHERE id = ?`, room).Scan(&gen, &leftAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, ErrNotFound
	}
	return gen, leftAt != 0, err
}

// checkGen fails unless room is held, not left, and (if want is not zero)
// at generation want.
func checkGen(tx *sql.Tx, room string, want int64) error {
	gen, left, err := roomGen(tx, room)
	switch {
	case err != nil:
		return err
	case want != 0 && gen != want:
		return ErrStaleGeneration
	case left:
		return ErrLeft
	}
	return nil
}

// CreateLogRoom records a room this machine holds the log of, with its
// pinned create entry, in one transaction: a new room (created here), or
// one joined (its create from the join preview). A room never seen gets
// generation 1; a room left before gets the next generation and loses its
// tombstone. A room held now is an error. Every registered agent's reading
// starts at the room's current end, which is empty. The create entry is
// the log's first row; members is the member cache.
func (s *Store) CreateLogRoom(ctx context.Context, id, label string, root LogRow, members []RoomMember, now time.Time) (int64, error) {
	var gen int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		g, left, err := roomGen(tx, id)
		switch {
		case errors.Is(err, ErrNotFound):
			gen = 1
			if _, err := tx.Exec(`INSERT INTO rooms (id, label, owner, version, created_at, create_id, gen, joined_at)
				VALUES (?,?,'',0,?,?,1,?)`, id, label, ms(now), root.ID, ms(now)); err != nil {
				return err
			}
		case err != nil:
			return err
		case !left:
			return errors.New("room is held already")
		default:
			gen = g + 1
			if _, err := tx.Exec(`UPDATE rooms SET label = ?, create_id = ?, gen = ?, joined_at = ?, left_at = 0
				WHERE id = ?`, label, root.ID, gen, ms(now), id); err != nil {
				return err
			}
		}
		root.State, root.Reason, root.Batch, root.Pos = LogAccepted, "", 0, 0
		if err := insertLog(tx, id, root); err != nil {
			return err
		}
		return replaceMembers(tx, id, members)
	})
	return gen, err
}

func insertLog(tx *sql.Tx, room string, r LogRow) error {
	_, err := tx.Exec(`INSERT INTO log (room, id, author, seq, bytes, sig, state, reason, batch, batch_pos)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, room, r.ID, r.Author, r.Seq, r.Header, r.Sig, r.State, r.Reason, r.Batch, r.Pos)
	return err
}

// ApplyLog stores one arrival batch with the dispositions the membership
// machine returned, and everything that follows from it, in one
// transaction (see LogBatch). It returns the batch number given.
func (s *Store) ApplyLog(ctx context.Context, b LogBatch) (int64, error) {
	var batch int64
	err := s.tx(ctx, func(tx *sql.Tx) (err error) {
		batch, err = applyLog(tx, b)
		return err
	})
	return batch, err
}

func applyLog(tx *sql.Tx, b LogBatch) (int64, error) {
	var batch int64
	err := func() error {
		if err := checkGen(tx, b.Room, b.Gen); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT coalesce(max(batch), 0) + 1 FROM log WHERE room = ?`, b.Room).Scan(&batch); err != nil {
			return err
		}
		for i, r := range b.New {
			r.Batch, r.Pos = batch, i+1
			if err := insertLog(tx, b.Room, r); err != nil {
				return err
			}
		}
		if err := hook("log:new"); err != nil {
			return err
		}
		for _, r := range b.Changed {
			if _, err := tx.Exec(`UPDATE log SET state = ?, reason = ? WHERE room = ? AND id = ?`,
				r.State, r.Reason, b.Room, r.ID); err != nil {
				return err
			}
		}
		for _, id := range b.Gone {
			if _, err := tx.Exec(`DELETE FROM log WHERE room = ? AND id = ?`, b.Room, id); err != nil {
				return err
			}
		}
		for _, m := range b.Members {
			if err := putMember(tx, b.Room, m); err != nil {
				return err
			}
		}
		for _, n := range b.MembersGone {
			if _, err := tx.Exec(`DELETE FROM room_members WHERE room = ? AND node = ?`, b.Room, n); err != nil {
				return err
			}
		}
		for _, q := range b.Requests {
			if err := insertRequest(tx, q); err != nil {
				return err
			}
		}
		if b.Complete != "" {
			res, err := tx.Exec(`UPDATE requests SET state = ?, decided_at = ?, entries = ?, reason = ''
				WHERE id = ? AND room = ? AND state = ?`, RequestApproved, ms(b.Now), b.Entries, b.Complete, b.Room, RequestPending)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return errors.New("request is not pending")
			}
		}
		if err := hook("log:access"); err != nil {
			return err
		}
		if b.Access != nil {
			a, err := b.Access(&Reader{tx: tx})
			if err != nil {
				return err
			}
			if err := applyAccess(tx, b.Room, a); err != nil {
				return err
			}
		}
		return nil
	}()
	return batch, err
}

// applyAccess re-decides text access and seeds cursors (see Access).
func applyAccess(tx *sql.Tx, room string, a Access) error {
	if a.Readable != nil {
		rows, err := tx.Query(`SELECT id, hdr, sig, reason FROM records WHERE room = ?
			AND reason IN ('live', 'missing', 'not_admitted')`, room)
		if err != nil {
			return err
		}
		type change struct{ id, reason string }
		var changes []change
		for rows.Next() {
			var id, reason string
			var hdr, sig []byte
			if err := rows.Scan(&id, &hdr, &sig, &reason); err != nil {
				rows.Close()
				return err
			}
			r, err := core.RecordFromParts(hdr, sig, nil)
			if err != nil {
				rows.Close()
				return errors.New("stored record does not parse")
			}
			acc, err := a.Readable(r.Author(), r.Fields(), id)
			if err != nil {
				rows.Close()
				return err
			}
			switch {
			case acc == TextHidden && reason != ReasonNotAdmitted:
				changes = append(changes, change{id, ReasonNotAdmitted})
			case acc == TextUndecided && reason == ReasonLive && hasText(r.Fields().Kind),
				acc != TextHidden && reason == ReasonNotAdmitted:
				changes = append(changes, change{id, ReasonMissing})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, c := range changes {
			if _, err := tx.Exec(`UPDATE records SET body = NULL, reason = ? WHERE room = ? AND id = ?`,
				c.reason, room, c.id); err != nil {
				return err
			}
		}
	}
	if a.Frontier != nil {
		agents, err := registered(tx)
		if err != nil {
			return err
		}
		// The frontier of each stream is worked out once, before seeding:
		// seedCursors takes a plain function.
		ss, err := (&Reader{tx: tx}).Streams(room)
		if err != nil {
			return err
		}
		at := map[[2]string]int64{}
		for _, st := range ss {
			n, err := a.Frontier(st)
			if err != nil {
				return err
			}
			at[[2]string{st.Author, st.Epoch}] = n
		}
		for _, ag := range agents {
			if err := seedCursors(tx, ag, room, func(st Stream) int64 { return at[[2]string{st.Author, st.Epoch}] }); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasText(k core.Kind) bool { return k == core.KindMessage || k == core.KindReplace }

func registered(tx *sql.Tx) ([]string, error) {
	rows, err := tx.Query(`SELECT agent FROM registrations ORDER BY agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RecordPlan is what the membership rules decide about one received
// record, inside the transaction that places it.
type RecordPlan struct {
	// Refuse, if set, is returned and nothing is stored.
	Refuse Outcome
	Now    time.Time
	// Text is whether this machine may hold the record's text; an
	// undecided record is stored without it, as missing (see TextAccess).
	Text TextAccess
	// Access, if set, is applied after the record is stored.
	Access *Access
}

// OutcomeAuthorNotAdmitted: the record's author was never admitted to the
// room in this machine's view, so it is not stored [RM1].
const OutcomeAuthorNotAdmitted Outcome = "author_not_admitted"

// PutRecordPlanned places one verified record as PutRecord does, after
// plan, run over the same transaction, has decided whether it may be
// stored at all and whether this machine may hold its text; then applies
// the plan's Access. gen, if not zero, must be the room's generation.
func (s *Store) PutRecordPlanned(ctx context.Context, r *core.VerifiedRecord, gen int64, plan func(*Reader) (RecordPlan, error)) (Outcome, error) {
	if r == nil || r.ID() == "" {
		return "", ErrUnverified
	}
	var out Outcome
	err := s.tx(ctx, func(tx *sql.Tx) (err error) {
		out, err = putPlanned(tx, r, gen, plan)
		return err
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

func putPlanned(tx *sql.Tx, r *core.VerifiedRecord, gen int64, plan func(*Reader) (RecordPlan, error)) (Outcome, error) {
	room := r.Fields().Room
	if err := checkGen(tx, room, gen); err != nil {
		return "", err
	}
	p, err := plan(&Reader{tx: tx})
	if err != nil {
		return "", err
	}
	if p.Refuse != "" {
		return p.Refuse, nil
	}
	opts, rec := PutOpts{Now: p.Now, MayRead: p.Text != TextHidden}, r
	if p.Text == TextUndecided {
		rec = r.WithoutBody()
	}
	out, err := place(tx, rec, opts)
	if err != nil {
		return "", err
	}
	if (out == OutcomeStored || out == OutcomeRepaired) && p.Access != nil {
		return out, applyAccess(tx, room, *p.Access)
	}
	return out, nil
}

// LogRooms lists the rooms that have a log (created or joined on the new
// path) and are not left.
func (s *Store) LogRooms(ctx context.Context) ([]LogRoom, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, label, create_id, gen, left_at FROM rooms
		WHERE create_id != '' AND left_at = 0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogRoom
	for rows.Next() {
		var r LogRoom
		var left int64
		if err := rows.Scan(&r.ID, &r.Label, &r.CreateID, &r.Gen, &left); err != nil {
			return nil, err
		}
		r.Left = left != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// LogRoom returns one room's log facts, left or not, or ErrNotFound.
func (s *Store) LogRoom(ctx context.Context, id string) (LogRoom, error) {
	r := LogRoom{ID: id}
	var left int64
	err := s.db.QueryRowContext(ctx, `SELECT label, create_id, gen, left_at FROM rooms WHERE id = ?`, id).
		Scan(&r.Label, &r.CreateID, &r.Gen, &left)
	if errors.Is(err, sql.ErrNoRows) {
		return LogRoom{}, ErrNotFound
	}
	r.Left = left != 0
	return r, err
}

// Log returns a room's stored log in arrival order: by batch, then by
// position in the batch. The create entry is batch 0. Replaying the
// batches in this order into a fresh membership machine gives the state
// it had.
func (s *Store) Log(ctx context.Context, room string) ([]LogRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, author, seq, bytes, sig, state, reason, batch, batch_pos
		FROM log WHERE room = ? ORDER BY batch, batch_pos`, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogRow
	for rows.Next() {
		var r LogRow
		if err := rows.Scan(&r.ID, &r.Author, &r.Seq, &r.Header, &r.Sig, &r.State, &r.Reason, &r.Batch, &r.Pos); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReplaceMembers replaces a room's member cache.
func (s *Store) ReplaceMembers(ctx context.Context, room string, members []RoomMember) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return replaceMembers(tx, room, members) })
}

func replaceMembers(tx *sql.Tx, room string, members []RoomMember) error {
	if _, err := tx.Exec(`DELETE FROM room_members WHERE room = ?`, room); err != nil {
		return err
	}
	for _, m := range members {
		if err := putMember(tx, room, m); err != nil {
			return err
		}
	}
	return nil
}

func putMember(tx *sql.Tx, room string, m RoomMember) error {
	_, err := tx.Exec(`INSERT INTO room_members (room, node, member, admin, admitted, name) VALUES (?,?,?,?,?,?)
		ON CONFLICT (room, node) DO UPDATE SET member = excluded.member, admin = excluded.admin,
		admitted = excluded.admitted, name = excluded.name`, room, m.Node, m.Member, m.Admin, m.Admitted, m.Name)
	return err
}

// RoomMembers returns a room's member cache, ordered by node. A room
// without a log has none.
func (rd *Reader) RoomMembers(room string) ([]RoomMember, error) {
	rows, err := rd.tx.Query(`SELECT node, member, admin, admitted, name FROM room_members WHERE room = ? ORDER BY node`, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoomMember
	for rows.Next() {
		var m RoomMember
		if err := rows.Scan(&m.Node, &m.Member, &m.Admin, &m.Admitted, &m.Name); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RoomMembers is Reader.RoomMembers in its own read transaction.
func (s *Store) RoomMembers(ctx context.Context, room string) (out []RoomMember, err error) {
	err = s.View(ctx, func(rd *Reader) error {
		out, err = rd.RoomMembers(room)
		return err
	})
	return out, err
}

// CreateRequest stores a new pending request.
func (s *Store) CreateRequest(ctx context.Context, q Request) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := checkGen(tx, q.Room, 0); err != nil {
			return err
		}
		return insertRequest(tx, q)
	})
}

func insertRequest(tx *sql.Tx, q Request) error {
	_, err := tx.Exec(`INSERT INTO requests (id, kind, action, room, node, name, role, history, seconds, created_at, state)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`, q.ID, q.Kind, q.Action, q.Room, q.Node, q.Name, q.Role, q.History, q.Seconds,
		ms(q.Created), RequestPending)
	return err
}

const requestCols = `id, kind, action, room, node, name, role, history, seconds, created_at, state, decided_at, entries, reason`

func scanRequest(sc interface{ Scan(...any) error }) (Request, error) {
	var q Request
	var created, decided int64
	if err := sc.Scan(&q.ID, &q.Kind, &q.Action, &q.Room, &q.Node, &q.Name, &q.Role, &q.History, &q.Seconds,
		&created, &q.State, &decided, &q.Entries, &q.Reason); err != nil {
		return Request{}, err
	}
	q.Created = fromMS(created)
	if decided != 0 {
		q.DecidedAt = fromMS(decided)
	}
	return q, nil
}

// Requests lists requests, oldest first: of one room if room is set, and
// in one state if state is set.
func (s *Store) Requests(ctx context.Context, room, state string) ([]Request, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+requestCols+` FROM requests
		WHERE (? = '' OR room = ?) AND (? = '' OR state = ?) ORDER BY created_at, id`, room, room, state, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		q, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// GetRequest returns one request, or ErrNotFound.
func (s *Store) GetRequest(ctx context.Context, id string) (Request, error) {
	q, err := scanRequest(s.db.QueryRowContext(ctx, `SELECT `+requestCols+` FROM requests WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	return q, err
}

// DecideRequest records a pending request as declined or failed (with
// reason). Approval is recorded by ApplyLog, with the entry it signed.
func (s *Store) DecideRequest(ctx context.Context, id, state, reason string, now time.Time) error {
	if state != RequestDeclined && state != RequestFailed {
		return errors.New("DecideRequest records only declined or failed")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE requests SET state = ?, reason = ?, decided_at = ? WHERE id = ? AND state = ?`,
			state, reason, ms(now), id, RequestPending)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return nil
	})
}

// LeaveRoom is leaving a room locally (DESIGN-next section 7): the room's
// records, reading state, log, member cache and requests are deleted, and
// the room row stays as a tombstone with the next generation, so a
// response prepared before is refused (ErrStaleGeneration). It returns the
// new generation.
func (s *Store) LeaveRoom(ctx context.Context, room string, now time.Time) (int64, error) {
	var gen int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		g, left, err := roomGen(tx, room)
		if err != nil {
			return err
		}
		if left {
			return ErrLeft
		}
		gen = g + 1
		for _, q := range []string{
			`DELETE FROM records WHERE room = ?`, `DELETE FROM streams WHERE room = ?`,
			`DELETE FROM forks WHERE room = ?`, `DELETE FROM cursors WHERE room = ?`,
			`DELETE FROM reads WHERE room = ?`, `DELETE FROM read_heads WHERE room = ?`,
			`DELETE FROM log WHERE room = ?`, `DELETE FROM room_members WHERE room = ?`,
			`DELETE FROM requests WHERE room = ?`,
			`DELETE FROM observed WHERE room = ?`, `DELETE FROM refusals WHERE room = ?`,
			`DELETE FROM room_invites WHERE room = ?`,
		} {
			if _, err := tx.Exec(q, room); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`UPDATE rooms SET gen = ?, left_at = ? WHERE id = ?`, gen, ms(now), room)
		return err
	})
	return gen, err
}
