package store

// Attachments (DESIGN section 12, PROTOCOL 10.8): migration 13, the
// content-addressed directory under the home, and the rows that make
// bytes live. Rules [F1], [F7] and [F10] of internal/model/delivery.
//
// Bytes and their directory are synced before the row that makes them
// live commits. A send's record and its file rows commit in one
// transaction after its bytes are safe. Deletion is by rows: the
// transaction that drops a row queues its path in file_trash, and the
// queue runs after the commit, skipping (and taking off the queue) any
// path a live row names again. fileMu serialises every step that puts
// bytes at a path with the queue's run, so a path being acquired again is
// never unlinked between its bytes and its row. A start runs the queue and
// removes temporary files, files no row names and copies still being
// written (ReconcileFiles).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// schemaFiles is migration 13 (PROTOCOL 10.8). Beyond the table there:
// file_parts.at (when the reservation was made), file_wants.list (the want
// is for a manifest) and sent_at (when it last went out in an answer),
// file_copies.size, and file_trash.kind, room, msg, sha256 and size, so
// the queue can tell whether a live row names its path again and, for a
// copy, whether the file is still the one written.
const schemaFiles = `
CREATE TABLE file_sets (
  room TEXT NOT NULL, record TEXT NOT NULL, manifest TEXT NOT NULL, ok INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (room, record));
CREATE INDEX file_sets_manifest ON file_sets (room, manifest);
CREATE TABLE file_items (
  room TEXT NOT NULL, record TEXT NOT NULL, n INTEGER NOT NULL, sha256 TEXT NOT NULL,
  name TEXT NOT NULL, size INTEGER NOT NULL, type TEXT NOT NULL,
  PRIMARY KEY (room, record, n));
CREATE INDEX file_items_hash ON file_items (room, sha256);
CREATE TABLE files (
  room TEXT NOT NULL, sha256 TEXT NOT NULL, size INTEGER NOT NULL, state TEXT NOT NULL,
  PRIMARY KEY (room, sha256));
CREATE TABLE file_parts (
  room TEXT NOT NULL, msg TEXT NOT NULL, sha256 TEXT NOT NULL, size INTEGER NOT NULL, have INTEGER NOT NULL,
  at INTEGER NOT NULL, PRIMARY KEY (room, msg, sha256));
CREATE TABLE file_for (
  room TEXT NOT NULL, sha256 TEXT NOT NULL, msg TEXT NOT NULL, how TEXT NOT NULL,
  PRIMARY KEY (room, sha256, msg));
CREATE INDEX file_for_msg ON file_for (room, msg);
CREATE TABLE file_obs (
  room TEXT NOT NULL, msg TEXT NOT NULL, sha256 TEXT NOT NULL, peer TEXT NOT NULL, answer TEXT NOT NULL,
  at INTEGER NOT NULL, PRIMARY KEY (room, msg, sha256, peer));
CREATE TABLE file_wants (
  room TEXT NOT NULL, msg TEXT NOT NULL, version TEXT NOT NULL, sha256 TEXT NOT NULL, author TEXT NOT NULL,
  size INTEGER NOT NULL, at INTEGER NOT NULL, tries INTEGER NOT NULL DEFAULT 0, lapsed_at INTEGER NOT NULL DEFAULT 0,
  list INTEGER NOT NULL DEFAULT 0, sent_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (room, msg, sha256));
CREATE INDEX file_wants_author ON file_wants (room, author);
CREATE TABLE file_lists (
  room TEXT NOT NULL, record TEXT NOT NULL, state TEXT NOT NULL, fails INTEGER NOT NULL DEFAULT 0,
  next_at INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (room, record));
CREATE TABLE file_copies (
  room TEXT NOT NULL, msg TEXT NOT NULL, sha256 TEXT NOT NULL, agent TEXT NOT NULL, path TEXT NOT NULL,
  state TEXT NOT NULL, size INTEGER NOT NULL, tmp TEXT NOT NULL DEFAULT '', PRIMARY KEY (room, path));
CREATE INDEX file_copies_msg ON file_copies (room, msg, sha256, agent);
CREATE TABLE file_trash (
  path TEXT PRIMARY KEY, at INTEGER NOT NULL, kind TEXT NOT NULL, room TEXT NOT NULL DEFAULT '',
  msg TEXT NOT NULL DEFAULT '', sha256 TEXT NOT NULL DEFAULT '', size INTEGER NOT NULL DEFAULT 0);
CREATE TABLE room_files (room TEXT PRIMARY KEY, mode TEXT NOT NULL, decided_at INTEGER NOT NULL);
ALTER TABLE requests ADD COLUMN draft TEXT NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN terms TEXT NOT NULL DEFAULT '';
INSERT INTO file_sets (room, record, manifest)
  SELECT room, id, json_extract(CAST(hdr AS TEXT), '$.files') FROM records
  WHERE kind IN ('message', 'replace') AND json_valid(CAST(hdr AS TEXT))
    AND json_type(CAST(hdr AS TEXT), '$.files') = 'text'
    AND length(json_extract(CAST(hdr AS TEXT), '$.files')) = 64
    AND NOT json_extract(CAST(hdr AS TEXT), '$.files') GLOB '*[^0-9a-f]*';
`

// A room's setting for files (PROTOCOL 10.5).
const (
	FilesOff   = "off"
	FilesCarry = "carry" // the default
	FilesKeep  = "keep"
)

// How bytes are held for a message (file_for.how).
const (
	HowSent     = "sent"     // this machine wrote the message
	HowReceived = "received" // fetched, carried or uploaded for this message
	HowLocal    = "local"    // held for another message; usable here, never served for this one
)

// Answers about a file, as file_obs records them.
const (
	AnswerHas        = "has"
	AnswerNo         = "no"
	AnswerNotYet     = "not_yet"
	AnswerGaveUp     = "gave_up"
	AnswerOldVersion = "old_version"
	AnswerBadFile    = "bad_file"
	AnswerSlow       = "slow"
	AnswerFailed     = "failed" // unreachable or refused for another reason
)

// States of a manifest that is not here (file_lists.state).
const (
	ListInvalid     = "invalid"
	ListUnavailable = "unavailable"
	ListTrying      = "trying" // failed answers counted, not yet unavailable
)

// RequestSendFiles is the kind of a request to send files (PROTOCOL
// 10.6), beside the log-entry kinds.
const RequestSendFiles = "send_files"

// Errors of the file methods.
var (
	ErrNoSpace  = errors.New("files: the room's or the machine's file budget would be exceeded")
	ErrBadPath  = errors.New("files: not a path a2p2 writes")
	ErrBadBytes = errors.New("files: the bytes do not match their SHA-256")
)

// FilePart is a download in progress for one message.
type FilePart struct {
	Room, Msg, SHA256 string
	Size, Have        int64
}

// FileObs is one dated answer from a peer about a file of a message.
type FileObs struct {
	Peer   string
	Answer string
	At     time.Time
}

// FileWant is one want, or carry job (PROTOCOL 10.4, 10.5).
type FileWant struct {
	Room, Msg, Version, SHA256, Author string
	Size                               int64 // reserved; a manifest's is 0
	List                               bool  // the want is for the version's manifest
	At                                 time.Time
	Tries                              int
	Lapsed                             time.Time // zero while it lives
	Sent                               time.Time
}

// FileList is what is known of a manifest that is not here.
type FileList struct {
	State string
	Fails int
	Next  time.Time
}

// FileCopy is a copy written for an agent.
type FileCopy struct {
	Room, Msg, SHA256, Agent, Path, State string
	Size                                  int64
}

// StagedFile is one file of a send waiting for a person: its bytes are
// at StagePath(request, N), or, when Kept, held already for the message
// being replaced.
type StagedFile struct {
	N      int    `json:"n"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Kept   bool   `json:"kept,omitempty"`
}

// the part of a send request's draft the store reads.
type draftFiles struct {
	Files []StagedFile `json:"files"`
}

func validRoomID(s string) bool { return core.ValidRoom(s) }
func validMsgID(s string) bool  { return len(s) == 32 && isLowerHex(s) }
func validReqID(s string) bool  { return strings.HasPrefix(s, "q_") && validMsgID(s[2:]) }
func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return s != ""
}

// Paths, relative to the file root, with forward slashes.
func blobRel(room, sha string) string      { return "blobs/" + room + "/" + sha }
func partRel(room, msg, sha string) string { return "blobs/" + room + "/part/" + msg + "/" + sha }
func stageDirRel(req string) string        { return "blobs/.staging/" + req }
func stageRel(req string, n int) string    { return stageDirRel(req) + "/" + strconv.Itoa(n) }
func copyDirRel(room string) string        { return "files/" + room }
func copyRel(room, name string) string     { return copyDirRel(room) + "/" + name }
func (s *Store) abs(rel string) string     { return filepath.Join(s.root, filepath.FromSlash(rel)) }
func tmpName() string                      { var b [12]byte; rand.Read(b[:]); return tmpPrefix + hex.EncodeToString(b[:]) }
func checkIDs(room, msg, sha string) bool {
	return validRoomID(room) && validMsgID(msg) && core.ValidSHA256(sha)
}
func (s *Store) blobPath(room, sha string) string { return s.abs(blobRel(room, sha)) }

// tmpPrefix starts the name of every temporary file a2p2 writes; a start
// removes them all.
const tmpPrefix = ".a2p2-tmp-"

// FileRoot is the directory under which blobs/ and files/ live: the
// directory of the database.
func (s *Store) FileRoot() string { return s.root }

// CopyPath is the absolute path of a copy written for an agent.
func (s *Store) CopyPath(rel string) string { return s.abs(rel) }

// SetFileBudgets lowers the budgets of [F7] for this store (tests; 0
// keeps the default).
func (s *Store) SetFileBudgets(room, machine int64) {
	s.roomBudget, s.machineBudget = room, machine
}

func (s *Store) budgets() (int64, int64) {
	r, m := int64(core.RoomFileBudget), int64(core.MachineFileBudget)
	if s.roomBudget > 0 {
		r = s.roomBudget
	}
	if s.machineBudget > 0 {
		m = s.machineBudget
	}
	return r, m
}

// ---- reading ----

// FileSet returns the manifest hash a record commits to, and whether its
// list is here and valid.
func (rd *Reader) FileSet(room, record string) (manifest string, ok bool, err error) {
	err = rd.tx.QueryRow(`SELECT manifest, ok FROM file_sets WHERE room = ? AND record = ?`, room, record).Scan(&manifest, &ok)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return manifest, ok, err
}

// FileItems returns the files a record's verified list names, in order.
func (rd *Reader) FileItems(room, record string) ([]core.FileRef, error) {
	rows, err := rd.tx.Query(`SELECT sha256, name, size, type FROM file_items WHERE room = ? AND record = ? ORDER BY n`, room, record)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.FileRef
	for rows.Next() {
		var f core.FileRef
		if err := rows.Scan(&f.SHA256, &f.Name, &f.Size, &f.Type); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// HeldFor says how bytes sha are held for message msg ("" if not).
func (rd *Reader) HeldFor(room, sha, msg string) (string, error) {
	var how string
	err := rd.tx.QueryRow(`SELECT how FROM file_for WHERE room = ? AND sha256 = ? AND msg = ?`, room, sha, msg).Scan(&how)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return how, err
}

// HeldForAny reports whether anything is held, partial or copied for msg
// and sha: what the pending clause of F1 keeps.
func (rd *Reader) HeldForAny(room, msg, sha string) (bool, error) {
	var n int
	err := rd.tx.QueryRow(`SELECT (SELECT count(*) FROM file_for WHERE room = ?1 AND msg = ?2 AND sha256 = ?3)
		+ (SELECT count(*) FROM file_parts WHERE room = ?1 AND msg = ?2 AND sha256 = ?3)
		+ (SELECT count(*) FROM file_copies WHERE room = ?1 AND msg = ?2 AND sha256 = ?3)`, room, msg, sha).Scan(&n)
	return n > 0, err
}

// Held returns the size of verified bytes sha held in room.
func (rd *Reader) Held(room, sha string) (int64, bool, error) {
	var n int64
	err := rd.tx.QueryRow(`SELECT size FROM files WHERE room = ? AND sha256 = ?`, room, sha).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return n, err == nil, err
}

// FilePart returns the download in progress of sha for msg.
func (rd *Reader) FilePart(room, msg, sha string) (FilePart, bool, error) {
	p := FilePart{Room: room, Msg: msg, SHA256: sha}
	err := rd.tx.QueryRow(`SELECT size, have FROM file_parts WHERE room = ? AND msg = ? AND sha256 = ?`, room, msg, sha).Scan(&p.Size, &p.Have)
	if errors.Is(err, sql.ErrNoRows) {
		return FilePart{}, false, nil
	}
	return p, err == nil, err
}

// FileObs returns the dated answers about sha for msg, by peer.
func (rd *Reader) FileObs(room, msg, sha string) ([]FileObs, error) {
	rows, err := rd.tx.Query(`SELECT peer, answer, at FROM file_obs WHERE room = ? AND msg = ? AND sha256 = ? ORDER BY peer`, room, msg, sha)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileObs
	for rows.Next() {
		var o FileObs
		var at int64
		if err := rows.Scan(&o.Peer, &o.Answer, &at); err != nil {
			return nil, err
		}
		o.At = fromMS(at)
		out = append(out, o)
	}
	return out, rows.Err()
}

const wantCols = `room, msg, version, sha256, author, size, list, at, tries, lapsed_at, sent_at`

func scanWant(sc interface{ Scan(...any) error }) (FileWant, error) {
	var w FileWant
	var at, lapsed, sent int64
	if err := sc.Scan(&w.Room, &w.Msg, &w.Version, &w.SHA256, &w.Author, &w.Size, &w.List, &at, &w.Tries, &lapsed, &sent); err != nil {
		return FileWant{}, err
	}
	w.At = fromMS(at)
	if lapsed != 0 {
		w.Lapsed = fromMS(lapsed)
	}
	if sent != 0 {
		w.Sent = fromMS(sent)
	}
	return w, nil
}

// FileWant returns the want or carry job for sha of msg, lapsed or not.
func (rd *Reader) FileWant(room, msg, sha string) (FileWant, bool, error) {
	w, err := scanWant(rd.tx.QueryRow(`SELECT `+wantCols+` FROM file_wants WHERE room = ? AND msg = ? AND sha256 = ?`, room, msg, sha))
	if errors.Is(err, sql.ErrNoRows) {
		return FileWant{}, false, nil
	}
	return w, err == nil, err
}

// FileWants returns the room's wants that live, oldest first.
func (rd *Reader) FileWants(room string) ([]FileWant, error) {
	rows, err := rd.tx.Query(`SELECT `+wantCols+` FROM file_wants WHERE room = ? AND lapsed_at = 0 ORDER BY list DESC, at, msg, sha256`, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileWant
	for rows.Next() {
		w, err := scanWant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// FileList returns what is known of record's manifest while it is not here.
func (rd *Reader) FileList(room, record string) (FileList, bool, error) {
	var l FileList
	var next int64
	err := rd.tx.QueryRow(`SELECT state, fails, next_at FROM file_lists WHERE room = ? AND record = ?`, room, record).Scan(&l.State, &l.Fails, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return FileList{}, false, nil
	}
	if next != 0 {
		l.Next = fromMS(next)
	}
	return l, err == nil, err
}

// FileCopy returns the copy of sha for msg written for agent, if done.
func (rd *Reader) FileCopy(room, msg, sha, agent string) (FileCopy, bool, error) {
	c := FileCopy{Room: room, Msg: msg, SHA256: sha, Agent: agent}
	err := rd.tx.QueryRow(`SELECT path, state, size FROM file_copies WHERE room = ? AND msg = ? AND sha256 = ? AND agent = ?
		AND state = 'done' LIMIT 1`, room, msg, sha, agent).Scan(&c.Path, &c.State, &c.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return FileCopy{}, false, nil
	}
	return c, err == nil, err
}

// FileMode returns the room's setting (carry unless set).
func (rd *Reader) FileMode(room string) (string, error) {
	var m string
	err := rd.tx.QueryRow(`SELECT mode FROM room_files WHERE room = ?`, room).Scan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return FilesCarry, nil
	}
	return m, err
}

// FileUsage returns the bytes counted against the room's budget and the
// machine's [F7]: held, partial at its declared size, staged, copied, and
// reserved by wants with no part yet.
func (rd *Reader) FileUsage(room string) (int64, int64, error) { return fileUsage(rd.tx, room) }

// FileBudgets returns the budgets in force.
func (s *Store) FileBudgets() (room, machine int64) { return s.budgets() }

const usageSQL = `SELECT
	(SELECT coalesce(sum(size), 0) FROM files WHERE ?1 = '' OR room = ?1) +
	(SELECT coalesce(sum(size), 0) FROM file_parts WHERE ?1 = '' OR room = ?1) +
	(SELECT coalesce(sum(size), 0) FROM file_copies WHERE ?1 = '' OR room = ?1) +
	(SELECT coalesce(sum(w.size), 0) FROM file_wants w WHERE (?1 = '' OR w.room = ?1) AND w.lapsed_at = 0
	  AND NOT EXISTS (SELECT 1 FROM file_parts p WHERE p.room = w.room AND p.msg = w.msg AND p.sha256 = w.sha256))`

func fileUsage(tx *sql.Tx, room string) (int64, int64, error) {
	var r, m int64
	if err := tx.QueryRow(usageSQL, room).Scan(&r); err != nil {
		return 0, 0, err
	}
	if err := tx.QueryRow(usageSQL, "").Scan(&m); err != nil {
		return 0, 0, err
	}
	sr, sm, err := stagedUsage(tx, room)
	return r + sr, m + sm, err
}

// stagedUsage counts the staged bytes of pending sends.
func stagedUsage(tx *sql.Tx, room string) (int64, int64, error) {
	rows, err := tx.Query(`SELECT room, draft FROM requests WHERE kind = ? AND state = ?`, RequestSendFiles, RequestPending)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var r, m int64
	for rows.Next() {
		var rm, d string
		if err := rows.Scan(&rm, &d); err != nil {
			return 0, 0, err
		}
		var df draftFiles
		_ = json.Unmarshal([]byte(d), &df)
		for _, f := range df.Files {
			if f.Kept {
				continue
			}
			m += f.Size
			if rm == room {
				r += f.Size
			}
		}
	}
	return r, m, rows.Err()
}

// fits says whether n more bytes fit both budgets, counting the
// reservations of requests still being staged.
func (s *Store) fits(tx *sql.Tx, room string, n int64) (bool, error) {
	r, m, err := fileUsage(tx, room)
	if err != nil {
		return false, err
	}
	sr, sm := s.stagingHeld(room)
	br, bm := s.budgets()
	return r+sr+n <= br && m+sm+n <= bm, nil
}

// FileFits says whether n more bytes fit the budgets of room now.
func (s *Store) FileFits(ctx context.Context, room string, n int64) (ok bool, err error) {
	err = s.View(ctx, func(rd *Reader) error {
		ok, err = s.fits(rd.tx, room, n)
		return err
	})
	return ok, err
}

// ---- small writes ----

// SetFileMode sets the room's setting for files.
func (s *Store) SetFileMode(ctx context.Context, room, mode string, now time.Time) error {
	if mode != FilesOff && mode != FilesCarry && mode != FilesKeep {
		return errors.New("files: the setting is off, carry or keep")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := checkGen(tx, room, 0); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO room_files (room, mode, decided_at) VALUES (?,?,?)
			ON CONFLICT (room) DO UPDATE SET mode = excluded.mode, decided_at = excluded.decided_at`, room, mode, ms(now))
		return err
	})
}

// NoteFileObs records a peer's dated answer about sha for msg.
func (s *Store) NoteFileObs(ctx context.Context, room, msg, sha, peer, answer string, at time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO file_obs (room, msg, sha256, peer, answer, at) VALUES (?,?,?,?,?,?)
			ON CONFLICT (room, msg, sha256, peer) DO UPDATE SET answer = excluded.answer, at = excluded.at`,
			room, msg, sha, peer, answer, ms(at))
		return err
	})
}

// IssueWant records a want or a carry job, reserving its size, unless one
// lives or lapsed for the same file of the message. It reports whether
// one was issued; ErrNoSpace if its size does not fit.
func (s *Store) IssueWant(ctx context.Context, w FileWant) (bool, error) {
	if !checkIDs(w.Room, w.Msg, w.SHA256) || !validMsgID(w.Version) || !core.ValidNode(w.Author) {
		return false, ErrBadPath
	}
	issued := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM file_wants WHERE room = ? AND msg = ? AND sha256 = ?`,
			w.Room, w.Msg, w.SHA256).Scan(&n); err != nil || n > 0 {
			return err
		}
		if w.Size > 0 {
			ok, err := s.fits(tx, w.Room, w.Size)
			if err != nil {
				return err
			}
			if !ok {
				return ErrNoSpace
			}
		}
		_, err := tx.Exec(`INSERT INTO file_wants (room, msg, version, sha256, author, size, list, at) VALUES (?,?,?,?,?,?,?,?)`,
			w.Room, w.Msg, w.Version, w.SHA256, w.Author, w.Size, w.List, ms(w.At))
		issued = err == nil
		return err
	})
	return issued, err
}

// DropWant removes a want, lapsed or not.
func (s *Store) DropWant(ctx context.Context, room, msg, sha string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM file_wants WHERE room = ? AND msg = ? AND sha256 = ?`, room, msg, sha)
		return err
	})
}

// LapseWant marks a want lapsed at now (a carry job is then answered
// "gave up" for GaveUpLife).
func (s *Store) LapseWant(ctx context.Context, room, msg, sha string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE file_wants SET lapsed_at = ? WHERE room = ? AND msg = ? AND sha256 = ? AND lapsed_at = 0`,
			ms(now), room, msg, sha)
		return err
	})
}

// WantsToSend chooses, in one transaction, the wants for author's
// messages to put in the answer to its pull (PROTOCOL 10.4): at most max,
// manifests first, each at most once per repeat. A want already sent and
// still here counts a try; one with WantTries tries lapses instead of
// going out. ok, if set, filters the candidates (the node's check that the
// want is still for the winning version).
func (s *Store) WantsToSend(ctx context.Context, room, author string, now time.Time, max int, repeat time.Duration,
	ok func(*Reader, FileWant) (bool, error)) ([]FileWant, error) {
	var out []FileWant
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT `+wantCols+` FROM file_wants WHERE room = ? AND author = ? AND lapsed_at = 0
			ORDER BY list DESC, at, msg, sha256`, room, author)
		if err != nil {
			return err
		}
		var all []FileWant
		for rows.Next() {
			w, err := scanWant(rows)
			if err != nil {
				rows.Close()
				return err
			}
			all = append(all, w)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rd := &Reader{tx: tx}
		for _, w := range all {
			if len(out) == max {
				break
			}
			if !w.Sent.IsZero() && now.Sub(w.Sent) < repeat {
				continue
			}
			if ok != nil {
				good, err := ok(rd, w)
				if err != nil {
					return err
				}
				if !good {
					continue
				}
			}
			tries := w.Tries
			if !w.Sent.IsZero() {
				tries++
			}
			if tries >= core.WantTries {
				if _, err := tx.Exec(`UPDATE file_wants SET tries = ?, lapsed_at = ? WHERE room = ? AND msg = ? AND sha256 = ?`,
					tries, ms(now), room, w.Msg, w.SHA256); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(`UPDATE file_wants SET tries = ?, sent_at = ? WHERE room = ? AND msg = ? AND sha256 = ?`,
				tries, ms(now), room, w.Msg, w.SHA256); err != nil {
				return err
			}
			w.Tries, w.Sent = tries, now
			out = append(out, w)
		}
		return nil
	})
	return out, err
}

// ListFailed counts a failed answer for record's missing manifest: after
// ListTries it is unavailable until someone asks again; meanwhile retried
// with backoff from ListRetryFirst, doubling to ListRetryMax.
func (s *Store) ListFailed(ctx context.Context, room, record string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var l FileList
		err := tx.QueryRow(`SELECT state, fails FROM file_lists WHERE room = ? AND record = ?`, room, record).Scan(&l.State, &l.Fails)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case l.State == ListInvalid:
			return nil
		}
		l.Fails++
		l.State = ListTrying
		if l.Fails >= core.ListTries {
			l.State = ListUnavailable
		}
		wait := core.ListRetryFirst << min(l.Fails-1, 10)
		if wait > core.ListRetryMax {
			wait = core.ListRetryMax
		}
		_, err = tx.Exec(`INSERT INTO file_lists (room, record, state, fails, next_at) VALUES (?,?,?,?,?)
			ON CONFLICT (room, record) DO UPDATE SET state = excluded.state, fails = excluded.fails, next_at = excluded.next_at`,
			room, record, l.State, l.Fails, ms(now.Add(wait)))
		return err
	})
}

// AskListAgain forgets the failed answers for record's manifest, unless
// it is invalid (F12): a person or an agent asked.
func (s *Store) AskListAgain(ctx context.Context, room, record string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM file_lists WHERE room = ? AND record = ? AND state != ?`, room, record, ListInvalid)
		return err
	})
}

// MarkListInvalid records that the manifest sha matched its hash and
// broke a rule: invalid for good for every record of room that commits to
// it, with nothing it lists fetched.
func (s *Store) MarkListInvalid(ctx context.Context, room, sha string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO file_lists (room, record, state) SELECT room, record, ? FROM file_sets
			WHERE room = ? AND manifest = ? ON CONFLICT (room, record) DO UPDATE SET state = excluded.state`, ListInvalid, room, sha)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM file_wants WHERE room = ? AND sha256 = ? AND list = 1`, room, sha)
		return err
	})
}

// ---- bytes ----

// ReservePart reserves size bytes for a download of sha for msg and
// returns the part: the one in progress, or a new one at 0. ErrNoSpace if
// a new one does not fit [F7].
func (s *Store) ReservePart(ctx context.Context, room, msg, sha string, size int64, now time.Time) (FilePart, error) {
	if !checkIDs(room, msg, sha) || size < 1 {
		return FilePart{}, ErrBadPath
	}
	p := FilePart{Room: room, Msg: msg, SHA256: sha, Size: size}
	// Under fileMu: the queue's unlink of this part's path cannot run
	// between its liveness check and the removal (finding 5).
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRow(`SELECT size, have FROM file_parts WHERE room = ? AND msg = ? AND sha256 = ?`, room, msg, sha).Scan(&p.Size, &p.Have)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// a want's reservation passes to the part
		var reserved int64
		if err := tx.QueryRow(`SELECT coalesce(sum(size), 0) FROM file_wants WHERE room = ? AND msg = ? AND sha256 = ? AND lapsed_at = 0`,
			room, msg, sha).Scan(&reserved); err != nil {
			return err
		}
		ok, err := s.fits(tx, room, size-min(reserved, size))
		if err != nil {
			return err
		}
		if !ok {
			return ErrNoSpace
		}
		if _, err := tx.Exec(`DELETE FROM file_trash WHERE path = ?`, partRel(room, msg, sha)); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO file_parts (room, msg, sha256, size, have, at) VALUES (?,?,?,?,0,?)`, room, msg, sha, size, ms(now))
		return err
	})
	return p, err
}

// OpenPart opens the part file of p for writing at offset have, cutting
// whatever is past it (bytes written after the last row are not counted).
func (s *Store) OpenPart(p FilePart, have int64) (*os.File, error) {
	if !checkIDs(p.Room, p.Msg, p.SHA256) || have < 0 || have > p.Size {
		return nil, ErrBadPath
	}
	path := s.abs(partRel(p.Room, p.Msg, p.SHA256))
	if err := s.mkdirSynced(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err == nil && st.Size() < have {
		err = fmt.Errorf("files: the part is shorter than its row")
	}
	if err == nil {
		err = f.Truncate(have)
	}
	if err == nil {
		_, err = f.Seek(have, io.SeekStart)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// PartHave records that have bytes of p are on disk and synced. ErrNotFound
// if the part was dropped meanwhile.
func (s *Store) PartHave(ctx context.Context, p FilePart, have int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE file_parts SET have = ? WHERE room = ? AND msg = ? AND sha256 = ? AND size >= ?`,
			have, p.Room, p.Msg, p.SHA256, have)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return nil
	})
}

// PartBytes reads a whole part (used for a manifest, at most 16 KiB).
func (s *Store) PartBytes(p FilePart, max int64) ([]byte, error) {
	if !checkIDs(p.Room, p.Msg, p.SHA256) {
		return nil, ErrBadPath
	}
	f, err := os.Open(s.abs(partRel(p.Room, p.Msg, p.SHA256)))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max+1))
}

// PartSHA256 hashes the first have bytes of a part.
func (s *Store) PartSHA256(p FilePart) (string, error) {
	if !checkIDs(p.Room, p.Msg, p.SHA256) {
		return "", ErrBadPath
	}
	f, err := os.Open(s.abs(partRel(p.Room, p.Msg, p.SHA256)))
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, p.Size)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DropPart removes a download (bytes that did not match, or a refusal
// that ends it), and deletes its file.
func (s *Store) DropPart(ctx context.Context, room, msg, sha string, now time.Time) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM file_parts WHERE room = ? AND msg = ? AND sha256 = ?`, room, msg, sha)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return trash(tx, partRel(room, msg, sha), "part", room, msg, sha, 0, now)
		}
		return nil
	})
	if err == nil {
		s.RunTrash(ctx)
	}
	return err
}

// CommitPart makes a complete, verified part live for msg [F10]: under
// fileMu, the part is renamed to the room's blob (or dropped if the blob
// is held already), the directory synced, and then one transaction
// records the bytes as held for msg (how), drops the part row and the
// want, and, for a manifest (items not nil), records its list for every
// record of the room that commits to it.
//
// ok, if set, is the caller's authorisation, asked again inside that
// transaction with the part's row: if the part is gone, not complete, or
// ok says no, nothing is recorded, the bytes are queued for deletion and
// ErrNotFound returned (finding 2).
func (s *Store) CommitPart(ctx context.Context, p FilePart, how string, items *core.Manifest, now time.Time,
	ok ...func(*Reader) (bool, error)) error {
	if !checkIDs(p.Room, p.Msg, p.SHA256) {
		return ErrBadPath
	}
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	blob := s.blobPath(p.Room, p.SHA256)
	if err := s.mkdirSynced(filepath.Dir(blob)); err != nil {
		return err
	}
	part := s.abs(partRel(p.Room, p.Msg, p.SHA256))
	if _, err := os.Stat(blob); err == nil {
		if err := os.Remove(part); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	} else if err := os.Rename(part, blob); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(blob)); err != nil {
		return err
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var size, have int64
		if err := tx.QueryRow(`SELECT size, have FROM file_parts WHERE room = ? AND msg = ? AND sha256 = ?`,
			p.Room, p.Msg, p.SHA256).Scan(&size, &have); err != nil || size != p.Size || have != p.Size {
			return ErrNotFound
		}
		for _, f := range ok {
			good, err := f(&Reader{tx: tx})
			if err != nil {
				return err
			}
			if !good {
				return ErrNotFound
			}
		}
		if _, err := tx.Exec(`DELETE FROM file_parts WHERE room = ? AND msg = ? AND sha256 = ?`, p.Room, p.Msg, p.SHA256); err != nil {
			return err
		}
		if err := holdTx(tx, p.Room, p.SHA256, p.Msg, how, p.Size); err != nil {
			return err
		}
		if items != nil {
			return listTx(tx, p.Room, p.SHA256, *items)
		}
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		// Refused at publication: the part row goes, and the bytes unless
		// a live row names them.
		_ = s.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`DELETE FROM file_parts WHERE room = ? AND msg = ? AND sha256 = ?`, p.Room, p.Msg, p.SHA256); err != nil {
				return err
			}
			return trash(tx, blobRel(p.Room, p.SHA256), "blob", p.Room, "", p.SHA256, 0, now)
		})
	}
	s.runTrashLocked(ctx)
	return err
}

// mkdirSynced creates dir and any missing parent, syncing each parent a
// directory was created in, so rows that depend on the directory never
// commit before it is durable (finding 6).
func (s *Store) mkdirSynced(dir string) error {
	if st, err := os.Lstat(dir); err == nil {
		if !st.IsDir() {
			return ErrBadPath
		}
		return nil
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := s.mkdirSynced(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return syncDir(parent)
}

// holdTx records bytes sha (on disk at its blob path) as held for msg.
func holdTx(tx *sql.Tx, room, sha, msg, how string, size int64) error {
	if _, err := tx.Exec(`INSERT INTO files (room, sha256, size, state) VALUES (?,?,?,'held') ON CONFLICT DO NOTHING`,
		room, sha, size); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO file_for (room, sha256, msg, how) VALUES (?,?,?,?)
		ON CONFLICT (room, sha256, msg) DO UPDATE SET how = excluded.how WHERE file_for.how = 'local'`, room, sha, msg, how); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM file_trash WHERE path = ?`, blobRel(room, sha)); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM file_wants WHERE room = ? AND msg = ? AND sha256 = ?`, room, msg, sha)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`DELETE FROM file_lists WHERE room = ? AND state != ? AND record IN
		(SELECT record FROM file_sets WHERE room = ? AND manifest = ?)`, room, ListInvalid, room, sha)
	return err
}

// listTx records manifest m (bytes sha, verified and valid) as the list of
// every record of room that commits to it.
func listTx(tx *sql.Tx, room, sha string, m core.Manifest) error {
	rows, err := tx.Query(`SELECT record FROM file_sets WHERE room = ? AND manifest = ? AND ok = 0`, room, sha)
	if err != nil {
		return err
	}
	var recs []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			rows.Close()
			return err
		}
		recs = append(recs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range recs {
		for i, f := range m.Files {
			if _, err := tx.Exec(`INSERT INTO file_items (room, record, n, sha256, name, size, type) VALUES (?,?,?,?,?,?,?)
				ON CONFLICT DO NOTHING`, room, r, i, f.SHA256, f.Name, f.Size, f.Type); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE file_sets SET ok = 1 WHERE room = ? AND record = ?`, room, r); err != nil {
			return err
		}
	}
	return nil
}

// UseHeld records bytes already held in room for another message as
// usable for msg (how local, never served for it).
func (s *Store) UseHeld(ctx context.Context, room, sha, msg string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM files WHERE room = ? AND sha256 = ?`, room, sha).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		_, err := tx.Exec(`INSERT INTO file_for (room, sha256, msg, how) VALUES (?,?,?,?) ON CONFLICT DO NOTHING`,
			room, sha, msg, HowLocal)
		return err
	})
}

// OpenBlob opens verified bytes sha of room for reading.
func (s *Store) OpenBlob(room, sha string) (*os.File, error) {
	if !validRoomID(room) || !core.ValidSHA256(sha) {
		return nil, ErrBadPath
	}
	return os.Open(s.blobPath(room, sha))
}

// trash queues path for deletion after the commit.
func trash(tx *sql.Tx, rel, kind, room, msg, sha string, size int64, now time.Time) error {
	_, err := tx.Exec(`INSERT INTO file_trash (path, at, kind, room, msg, sha256, size) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT (path) DO NOTHING`, rel, ms(now), kind, room, msg, sha, size)
	return err
}

// RunTrash deletes what the queue names, skipping any path a live row
// names again; best effort, and run again at every start.
func (s *Store) RunTrash(ctx context.Context) {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	s.runTrashLocked(ctx)
}

type trashRow struct {
	path, kind, room, msg, sha string
	size                       int64
}

func (s *Store) runTrashLocked(ctx context.Context) {
	var rows []trashRow
	_ = s.View(ctx, func(rd *Reader) error {
		q, err := rd.tx.Query(`SELECT path, kind, room, msg, sha256, size FROM file_trash ORDER BY path`)
		if err != nil {
			return err
		}
		defer q.Close()
		for q.Next() {
			var t trashRow
			if err := q.Scan(&t.path, &t.kind, &t.room, &t.msg, &t.sha, &t.size); err != nil {
				return err
			}
			rows = append(rows, t)
		}
		return q.Err()
	})
	for _, t := range rows {
		live := false
		_ = s.View(ctx, func(rd *Reader) error {
			var err error
			live, err = livePath(rd.tx, t)
			return err
		})
		if !live && !s.unlink(t) {
			continue // kept: tried again at the next run and at every start
		}
		_ = s.tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`DELETE FROM file_trash WHERE path = ?`, t.path)
			return err
		})
	}
}

// livePath says whether a live row names t's path again.
func livePath(tx *sql.Tx, t trashRow) (bool, error) {
	var n int
	var err error
	switch t.kind {
	case "blob":
		err = tx.QueryRow(`SELECT count(*) FROM files WHERE room = ? AND sha256 = ?`, t.room, t.sha).Scan(&n)
	case "part":
		err = tx.QueryRow(`SELECT count(*) FROM file_parts WHERE room = ? AND msg = ? AND sha256 = ?`, t.room, t.msg, t.sha).Scan(&n)
	case "copy":
		err = tx.QueryRow(`SELECT count(*) FROM file_copies WHERE room = ? AND path = ?`, t.room, t.path).Scan(&n)
	case "stage":
		err = tx.QueryRow(`SELECT count(*) FROM requests WHERE id = ? AND state = ?`, t.msg, RequestPending).Scan(&n)
	}
	return n > 0, err
}

// unlink removes one queued path and reports whether the entry is done:
// the path is gone and its directory synced, or (a copy) it is no longer
// the file a2p2 wrote (another size or SHA-256), which is never removed.
// A failure (a file still open on Windows, an error syncing) keeps the
// entry (finding 5).
func (s *Store) unlink(t trashRow) bool {
	p := s.abs(t.path)
	switch t.kind {
	case "copy":
		return s.unlinkCopy(t)
	case "stage":
		if err := os.RemoveAll(p); err != nil {
			return false
		}
		return syncDir(filepath.Dir(p)) == nil
	}
	err := os.Remove(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true
	case err != nil:
		return false
	}
	if syncDir(filepath.Dir(p)) != nil {
		return false
	}
	if t.kind == "part" {
		_ = os.Remove(filepath.Dir(p)) // the message's part directory, if now empty
	}
	return true
}

// unlinkCopy removes a copy through the room's files directory, opened
// without following a link (copyDir).
func (s *Store) unlinkCopy(t trashRow) bool {
	if !validRoomID(t.room) || !strings.HasPrefix(t.path, copyDirRel(t.room)+"/") {
		return true
	}
	root, err := s.copyDir(t.room, false)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		return errors.Is(err, ErrBadPath) // a link where the directory was: nothing of a2p2's is behind it
	}
	defer root.Close()
	name := path.Base(t.path)
	st, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	if !st.Mode().IsRegular() || st.Size() != t.size {
		return true
	}
	f, err := root.Open(name)
	if err != nil {
		return false
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil {
		return false
	}
	if hex.EncodeToString(h.Sum(nil)) != t.sha {
		return true
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	return syncDir(s.abs(copyDirRel(t.room))) == nil
}

// copyDir opens files/<room> under the home as an os.Root, refusing a
// link or anything but a directory at files and at files/<room>
// (ErrBadPath), and checking that what was opened is what was looked at.
// With create it makes what is missing, synced into its parent (finding
// 4).
func (s *Store) copyDir(room string, create bool) (*os.Root, error) {
	if !validRoomID(room) {
		return nil, ErrBadPath
	}
	home, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, err
	}
	defer home.Close()
	var last fs.FileInfo
	for _, rel := range []string{"files", "files/" + room} {
		st, err := home.Lstat(rel)
		if errors.Is(err, fs.ErrNotExist) && create {
			if err := home.Mkdir(rel, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return nil, err
			}
			if err := syncDir(s.abs(path.Dir(rel))); err != nil {
				return nil, err
			}
			st, err = home.Lstat(rel)
		}
		if err != nil {
			return nil, err
		}
		if st.Mode()&fs.ModeSymlink != 0 || !st.IsDir() {
			return nil, ErrBadPath
		}
		last = st
	}
	root, err := home.OpenRoot("files/" + room)
	if err != nil {
		return nil, err
	}
	if st, err := root.Stat("."); err != nil || !os.SameFile(st, last) {
		root.Close()
		return nil, ErrBadPath // swapped for a link between the look and the open
	}
	return root, nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---- copies for agents ----

// ReserveCopy chooses the path of a copy of sha for msg for agent, under
// files/<room>/, from name sanitised (SafeName), with " (2)" and so on for
// a clash, and records it as being written. A copy already written for
// the agent is returned with done set. ErrNoSpace if it does not fit.
func (s *Store) ReserveCopy(ctx context.Context, room, msg, sha, agent, name string, size int64) (c FileCopy, done bool, err error) {
	if !checkIDs(room, msg, sha) {
		return FileCopy{}, false, ErrBadPath
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRow(`SELECT path, state, size FROM file_copies WHERE room = ? AND msg = ? AND sha256 = ? AND agent = ? AND state = 'done'`,
			room, msg, sha, agent).Scan(&c.Path, &c.State, &c.Size)
		if err == nil {
			c.Room, c.Msg, c.SHA256, c.Agent, done = room, msg, sha, agent, true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		ok, err := s.fits(tx, room, size)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNoSpace
		}
		base := SafeName(name)
		for i := 1; i < 1000; i++ {
			cand := base
			if i > 1 {
				cand = numbered(base, i)
			}
			rel := copyRel(room, cand)
			var n int
			if err := tx.QueryRow(`SELECT count(*) FROM file_copies WHERE room = ? AND path = ?`, room, rel).Scan(&n); err != nil {
				return err
			}
			if _, err := os.Lstat(s.abs(rel)); n > 0 || err == nil {
				continue
			}
			c = FileCopy{Room: room, Msg: msg, SHA256: sha, Agent: agent, Path: rel, State: "writing", Size: size}
			_, err := tx.Exec(`INSERT INTO file_copies (room, msg, sha256, agent, path, state, size) VALUES (?,?,?,?,?,?,?)`,
				room, msg, sha, agent, rel, c.State, size)
			return err
		}
		return errors.New("files: no free name for the copy")
	})
	return c, done, err
}

// WriteCopy writes and publishes the copy c with no further check; see
// PublishCopy.
func (s *Store) WriteCopy(c FileCopy) error { return s.PublishCopy(context.Background(), c, nil) }

// CopyDone is kept for callers that wrote with WriteCopy: the copy is
// already marked written by the publication.
func (s *Store) CopyDone(ctx context.Context, c FileCopy) error {
	var state string
	err := s.View(ctx, func(rd *Reader) error {
		return rd.tx.QueryRow(`SELECT state FROM file_copies WHERE room = ? AND path = ?`, c.Room, c.Path).Scan(&state)
	})
	if errors.Is(err, sql.ErrNoRows) || err == nil && state != "done" {
		return ErrNotFound
	}
	return err
}

// ErrCopyRefused: the copy was not published because ok said no or its
// row is gone; nothing of it is left.
var ErrCopyRefused = errors.New("files: the copy may no longer be written")

// PublishCopy writes the copy c from the room's blob and publishes it in
// one step, under fileMu, which every deletion also takes (finding 3):
// the bytes go under a temporary name in files/<room> (opened by
// copyDir, never through a link), synced and checked against the hash,
// and the temporary name is recorded with the row; then one transaction
// asks ok again (the permission and the version shown), links the final
// name to the temporary file (a hard link, which fails on an existing
// name and so never replaces a file, on every system), and marks the
// copy written. The temporary name is removed after. A failure after the
// final name exists removes it at once, or queues it.
func (s *Store) PublishCopy(ctx context.Context, c FileCopy, ok func(*Reader) (bool, error)) error {
	if !validRoomID(c.Room) || !strings.HasPrefix(c.Path, copyDirRel(c.Room)+"/") || strings.Count(c.Path, "/") != 2 {
		return ErrBadPath
	}
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	root, err := s.copyDir(c.Room, true)
	if err != nil {
		return err
	}
	defer root.Close()
	dir := s.abs(copyDirRel(c.Room))
	name := path.Base(c.Path)
	src, err := s.OpenBlob(c.Room, c.SHA256)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := tmpName()
	if err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE file_copies SET tmp = ? WHERE room = ? AND path = ? AND state = 'writing'`, tmp, c.Room, c.Path)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrCopyRefused
		}
		return nil
	}); err != nil {
		return err
	}
	out, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), src)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && (n != c.Size || hex.EncodeToString(h.Sum(nil)) != c.SHA256) {
		err = ErrBadBytes
	}
	if err == nil {
		linked := false
		err = s.tx(ctx, func(tx *sql.Tx) error {
			if ok != nil {
				good, err := ok(&Reader{tx: tx})
				if err != nil {
					return err
				}
				if !good {
					return ErrCopyRefused
				}
			}
			res, err := tx.Exec(`UPDATE file_copies SET state = 'done', tmp = '' WHERE room = ? AND path = ? AND state = 'writing' AND tmp = ?`,
				c.Room, c.Path, tmp)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return ErrCopyRefused
			}
			if err := root.Link(tmp, name); err != nil {
				return err
			}
			linked = true
			return syncDir(dir)
		})
		if err != nil && linked {
			// not committed: the final name is this copy's; remove it
			if rerr := root.Remove(name); rerr != nil {
				_ = s.tx(ctx, func(tx *sql.Tx) error {
					return trash(tx, c.Path, "copy", c.Room, c.Msg, c.SHA256, c.Size, time.Now())
				})
			}
		}
	}
	_ = root.Remove(tmp)
	_ = syncDir(dir)
	return err
}

// DropCopy removes a copy row whose write failed, and its file.
func (s *Store) DropCopy(ctx context.Context, c FileCopy, now time.Time) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM file_copies WHERE room = ? AND path = ?`, c.Room, c.Path); err != nil {
			return err
		}
		return trash(tx, c.Path, "copy", c.Room, c.Msg, c.SHA256, c.Size, now)
	})
	if err == nil {
		s.RunTrash(ctx)
	}
	return err
}

// windowsReserved are names Windows will not create, with any extension.
var windowsReserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true}

// SafeName is a manifest name made safe as one path element (PROTOCOL
// 10.8): every character other than a letter, a digit, a space or ._-+()
// becomes _ ; no leading or trailing dots or spaces; a Windows reserved
// name is prefixed with _ ; at most 200 bytes; never empty, "." or "..".
// Unicode is not normalised (that would need a dependency): a combining
// mark is not a letter, so it becomes _.
func SafeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == ' ' || r == '.' || r == '_' || r == '-' || r == '+' || r == '(' || r == ')',
			r >= '0' && r <= '9', isLetter(r):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), ". ")
	for len(out) > 200 {
		_, size := lastRune(out)
		out = strings.TrimRight(out[:len(out)-size], ". ")
	}
	if out == "" {
		out = "file"
	}
	stem := strings.ToUpper(strings.SplitN(out, ".", 2)[0])
	if windowsReserved[strings.TrimRight(stem, " ")] {
		out = "_" + out
	}
	return out
}

func numbered(name string, i int) string {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if stem == "" {
		stem, ext = name, ""
	}
	return fmt.Sprintf("%s (%d)%s", stem, i, ext)
}

// ---- staging ----

// Staged is a file staged for a send: copied and hashed from one handle.
type Staged struct {
	Path   string // as resolved from the opened file
	Size   int64
	SHA256 string
}

// ErrNotRegular: the path, once opened, is not a regular file.
var ErrNotRegular = errors.New("files: not a regular file")

// StageFile opens path once (following links), refuses anything but a
// regular file from the handle's own information, and copies and hashes
// from that handle into blobs/.staging/<request>/<n>, synced before it
// returns. The path it reports is the one the system resolves, checked to
// be the same file as the handle. Larger than max: ErrNoSpace's sibling,
// core.MaxFileSize, refused before a byte is written.
func (s *Store) StageFile(req string, n int, path string, max int64) (Staged, error) {
	if !validReqID(req) || n < 0 || n >= core.MaxFiles || !filepath.IsAbs(path) {
		return Staged{}, ErrBadPath
	}
	f, err := openSource(path)
	if err != nil {
		return Staged{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Staged{}, err
	}
	if !st.Mode().IsRegular() {
		return Staged{}, ErrNotRegular
	}
	if st.Size() > max {
		return Staged{}, ErrTooLarge
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		resolved, err = filepath.Abs(resolved)
	}
	if err != nil {
		return Staged{}, err
	}
	if rs, err := os.Stat(resolved); err != nil || !os.SameFile(rs, st) {
		return Staged{}, errors.New("files: the file changed while it was opened")
	}
	dir := s.abs(stageDirRel(req))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Staged{}, err
	}
	tmp := filepath.Join(dir, tmpName())
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Staged{}, err
	}
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(f, max+1))
	if err == nil && size > max {
		err = ErrTooLarge
	}
	if err == nil && size == 0 {
		err = errors.New("files: the file is empty")
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, s.abs(stageRel(req, n)))
	}
	if err == nil {
		err = syncDir(dir)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return Staged{}, err
	}
	return Staged{Path: resolved, Size: size, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// ErrTooLarge: a file over the size limit.
var ErrTooLarge = errors.New("files: over the size limit")

// DropStaging removes the staging of a request that was never stored
// (refused before its row).
func (s *Store) DropStaging(req string) {
	if validReqID(req) {
		_ = os.RemoveAll(s.abs(stageDirRel(req)))
	}
}

// CheckStaged hashes staged file n of req again: what approval checks
// before sending [F6].
func (s *Store) CheckStaged(req string, n int, sha string, size int64) error {
	if !validReqID(req) {
		return ErrBadPath
	}
	p := s.abs(stageRel(req, n))
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() != size {
		return ErrBadBytes
	}
	if got, err := fileSHA256(p); err != nil || got != sha {
		return ErrBadBytes
	}
	return nil
}

// CreateSendRequest stores a request to send files, with its draft and
// terms, after its files are staged: the staging is the row's, and a
// start removes staging no pending request names. The budget is checked
// in the same transaction.
func (s *Store) CreateSendRequest(ctx context.Context, q Request, staged []StagedFile, pendingMax int) error {
	if q.Kind != RequestSendFiles || !validReqID(q.ID) {
		return ErrBadPath
	}
	var total int64
	for _, f := range staged {
		if !f.Kept {
			total += f.Size
		}
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := checkGen(tx, q.Room, 0); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM requests WHERE room = ? AND state = ?`, q.Room, RequestPending).Scan(&n); err != nil {
			return err
		}
		if pendingMax > 0 && n >= pendingMax {
			return ErrTooManyPending
		}
		// this request's own staging reservation is replaced by its draft
		s.resMu.Lock()
		own := s.staging[q.ID].n
		s.resMu.Unlock()
		ok, err := s.fits(tx, q.Room, total-own)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNoSpace
		}
		return insertRequest(tx, q)
	})
	if err == nil {
		s.ReleaseStaging(q.ID)
	}
	return err
}

// ErrSendExpired: the send waited longer than StagingLife for a person.
var ErrSendExpired = errors.New("files: the request expired before it was approved")

// ErrTooManyPending: too many requests wait in the room.
var ErrTooManyPending = errors.New("too many requests are waiting in this room")

// SendFiles is what a send's approval stores with its record (PROTOCOL
// 10.8): the manifest's bytes, and the staged or kept files.
type SendFiles struct {
	Request  string
	Room     string
	Manifest []byte
	Files    []StagedFile
}

// WriteWithFiles is Write for a send with files [F10]. Under fileMu, the
// manifest and every staged file are put at the room's blob paths (linked
// from staging, or copied), synced with their directory; then one
// transaction builds and places the record, records the manifest and the
// files as held for the message (sent), the manifest's list, and the
// request approved with the record as its effect, and queues the staging
// for deletion. If the record is not stored, the request still waits and
// a start removes the blobs no row names. msgOf returns the message the
// record is for (itself, or the message it replaces).
func (s *Store) WriteWithFiles(ctx context.Context, now time.Time, sf SendFiles, m core.Manifest,
	build func(*Reader) (*core.VerifiedRecord, error)) (*core.VerifiedRecord, error) {
	if !validReqID(sf.Request) || !validRoomID(sf.Room) {
		return nil, ErrBadPath
	}
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	msha := core.SHA256Hex(sf.Manifest)
	dir := s.abs("blobs/" + sf.Room)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := s.putBlob(sf.Room, msha, func(w io.Writer) error { _, err := w.Write(sf.Manifest); return err }); err != nil {
		return nil, err
	}
	for _, f := range sf.Files {
		if f.Kept {
			continue
		}
		src := s.abs(stageRel(sf.Request, f.N))
		dst := s.blobPath(sf.Room, f.SHA256)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := os.Link(src, dst); err != nil {
			if err := s.putBlob(sf.Room, f.SHA256, func(w io.Writer) error {
				in, err := os.Open(src)
				if err != nil {
					return err
				}
				defer in.Close()
				_, err = io.Copy(w, in)
				return err
			}); err != nil {
				return nil, err
			}
		}
	}
	if err := syncDir(dir); err != nil {
		return nil, err
	}
	var r *core.VerifiedRecord
	err := s.tx(ctx, func(tx *sql.Tx) error {
		// the request's expiry, in the transaction that publishes (finding 9)
		var created int64
		var state string
		if err := tx.QueryRow(`SELECT created_at, state FROM requests WHERE id = ?`, sf.Request).Scan(&created, &state); err != nil {
			return ErrNotFound
		}
		if state != RequestPending {
			return ErrNotFound
		}
		if !now.Before(fromMS(created).Add(core.StagingLife)) {
			return ErrSendExpired
		}
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
		f := r.Fields()
		msg := r.ID()
		if f.Kind == core.KindReplace {
			msg = f.Ref
		}
		if err := holdTx(tx, sf.Room, msha, msg, HowSent, int64(len(sf.Manifest))); err != nil {
			return err
		}
		for _, x := range sf.Files {
			if err := holdTx(tx, sf.Room, x.SHA256, msg, HowSent, x.Size); err != nil {
				return err
			}
		}
		if err := listTx(tx, sf.Room, msha, m); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE requests SET state = ?, decided_at = ?, entries = ? WHERE id = ? AND state = ?`,
			RequestApproved, ms(now), r.ID(), sf.Request, RequestPending)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return trash(tx, stageDirRel(sf.Request), "stage", "", sf.Request, "", 0, now)
	})
	if err != nil {
		return nil, err
	}
	s.runTrashLocked(ctx)
	return r, nil
}

// putBlob writes a blob under a temporary name, syncs it and renames it
// into place; the caller holds fileMu and syncs the directory.
func (s *Store) putBlob(room, sha string, write func(io.Writer) error) error {
	dst := s.blobPath(room, sha)
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	tmp := filepath.Join(filepath.Dir(dst), tmpName())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = write(f)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// DecideSend records a pending send as declined or failed and queues its
// staging for deletion [F6].
func (s *Store) DecideSend(ctx context.Context, id, state, reason string, now time.Time) error {
	if state != RequestDeclined && state != RequestFailed {
		return errors.New("DecideSend records only declined or failed")
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE requests SET state = ?, reason = ?, decided_at = ? WHERE id = ? AND state = ? AND kind = ?`,
			state, reason, ms(now), id, RequestPending, RequestSendFiles)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return trash(tx, stageDirRel(id), "stage", "", id, "", 0, now)
	})
	if err == nil {
		s.RunTrash(ctx)
	}
	return err
}

// fileSetTx records, when a record with files is stored, the manifest it
// commits to; if that manifest is held and its list known here (another
// version listed the same one), the list is the record's too.
func fileSetTx(tx *sql.Tx, room, record, manifest string) error {
	if _, err := tx.Exec(`INSERT INTO file_sets (room, record, manifest) VALUES (?,?,?) ON CONFLICT DO NOTHING`,
		room, record, manifest); err != nil {
		return err
	}
	var other string
	err := tx.QueryRow(`SELECT record FROM file_sets WHERE room = ? AND manifest = ? AND ok = 1 AND record != ? LIMIT 1`,
		room, manifest, record).Scan(&other)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO file_items (room, record, n, sha256, name, size, type)
		SELECT room, ?, n, sha256, name, size, type FROM file_items WHERE room = ? AND record = ?`, record, room, other); err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE file_sets SET ok = 1 WHERE room = ? AND record = ?`, room, record)
	return err
}

// leaveFiles drops every file row of a room being left and queues its
// paths: nothing of it may be kept.
func leaveFiles(tx *sql.Tx, room string, now time.Time) error {
	for _, q := range []struct{ sql, kind string }{
		{`SELECT '', sha256, 0, '' FROM files WHERE room = ?`, "blob"},
		{`SELECT msg, sha256, 0, '' FROM file_parts WHERE room = ?`, "part"},
		{`SELECT msg, sha256, size, path FROM file_copies WHERE room = ?`, "copy"},
		{`SELECT id, '', 0, '' FROM requests WHERE room = ? AND kind = 'send_files' AND state = 'pending'`, "stage"},
	} {
		rows, err := tx.Query(q.sql, room)
		if err != nil {
			return err
		}
		var ts []trashRow
		for rows.Next() {
			var t trashRow
			if err := rows.Scan(&t.msg, &t.sha, &t.size, &t.path); err != nil {
				rows.Close()
				return err
			}
			ts = append(ts, t)
		}
		rows.Close()
		for _, t := range ts {
			rel := t.path
			switch q.kind {
			case "blob":
				rel = blobRel(room, t.sha)
			case "part":
				rel = partRel(room, t.msg, t.sha)
			case "stage":
				rel = stageDirRel(t.msg)
			}
			if err := trash(tx, rel, q.kind, room, t.msg, t.sha, t.size, now); err != nil {
				return err
			}
		}
	}
	for _, q := range []string{`DELETE FROM files WHERE room = ?`, `DELETE FROM file_parts WHERE room = ?`,
		`DELETE FROM file_for WHERE room = ?`, `DELETE FROM file_obs WHERE room = ?`, `DELETE FROM file_wants WHERE room = ?`,
		`DELETE FROM file_lists WHERE room = ?`, `DELETE FROM file_copies WHERE room = ?`, `DELETE FROM file_sets WHERE room = ?`,
		`DELETE FROM file_items WHERE room = ?`, `DELETE FROM room_files WHERE room = ?`} {
		if _, err := tx.Exec(q, room); err != nil {
			return err
		}
	}
	return nil
}

// ---- the start ----

// ReconcileFiles is what a start does [F10]: finish the deletion queue,
// remove every temporary file, every file under blobs/ that no row names
// (a part longer than its row is cut back), staging that no pending
// request names, and every copy still being written, with its row.
func (s *Store) ReconcileFiles(ctx context.Context) error {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	s.runTrashLocked(ctx)
	type writingCopy struct {
		FileCopy
		tmp string
	}
	var writing []writingCopy
	if err := s.View(ctx, func(rd *Reader) error {
		rows, err := rd.tx.Query(`SELECT room, msg, sha256, agent, path, size, tmp FROM file_copies WHERE state = 'writing'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c writingCopy
			if err := rows.Scan(&c.Room, &c.Msg, &c.SHA256, &c.Agent, &c.Path, &c.Size, &c.tmp); err != nil {
				return err
			}
			writing = append(writing, c)
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	for _, c := range writing {
		// Only what the row proves it wrote: its temporary file, and the
		// final name if it is a link to that same file (a crash between
		// the link and the commit). Anything else at the name is not ours.
		if root, err := s.copyDir(c.Room, false); err == nil {
			if c.tmp != "" && strings.HasPrefix(c.tmp, tmpPrefix) && !strings.ContainsAny(c.tmp, `/\`) {
				ts, terr := root.Lstat(c.tmp)
				fsI, ferr := root.Lstat(path.Base(c.Path))
				if terr == nil && ferr == nil && os.SameFile(ts, fsI) {
					_ = root.Remove(path.Base(c.Path))
				}
				_ = root.Remove(c.tmp)
				_ = syncDir(s.abs(copyDirRel(c.Room)))
			}
			root.Close()
		}
		if err := s.tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`DELETE FROM file_copies WHERE room = ? AND path = ? AND state = 'writing'`, c.Room, c.Path)
			return err
		}); err != nil {
			return err
		}
	}
	if err := s.reconcileParts(ctx); err != nil {
		return err
	}
	for _, top := range []string{"blobs"} { // files/ holds only what rows prove (above)
		base := s.abs(top)
		_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() || !strings.HasPrefix(d.Name(), tmpPrefix) {
				return nil
			}
			_ = os.Remove(p)
			return nil
		})
	}
	return s.removeUnnamed(ctx)
}

// removeUnnamed removes files under blobs/ no row names.
func (s *Store) removeUnnamed(ctx context.Context) error {
	base := s.abs("blobs")
	return filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(s.root, p)
		if rerr != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if d.IsDir() {
			if len(parts) == 3 && parts[1] == ".staging" {
				live := false
				_ = s.View(ctx, func(rd *Reader) error {
					var e error
					live, e = livePath(rd.tx, trashRow{kind: "stage", msg: parts[2]})
					return e
				})
				if !live {
					_ = os.RemoveAll(p)
					return filepath.SkipDir
				}
			}
			return nil
		}
		var t trashRow
		switch {
		case len(parts) == 3 && validRoomID(parts[1]) && core.ValidSHA256(parts[2]):
			t = trashRow{kind: "blob", room: parts[1], sha: parts[2]}
		case len(parts) == 5 && validRoomID(parts[1]) && parts[2] == "part" && validMsgID(parts[3]) && core.ValidSHA256(parts[4]):
			t = trashRow{kind: "part", room: parts[1], msg: parts[3], sha: parts[4]}
			var have int64 = -1
			_ = s.View(ctx, func(rd *Reader) error {
				return rd.tx.QueryRow(`SELECT have FROM file_parts WHERE room = ? AND msg = ? AND sha256 = ?`,
					t.room, t.msg, t.sha).Scan(&have)
			})
			if have >= 0 {
				_ = os.Truncate(p, have)
				return nil
			}
		case len(parts) == 4 && parts[1] == ".staging":
			return nil // within a live staging directory
		default:
			_ = os.Remove(p) // nothing a2p2 writes has this name
			return nil
		}
		live := false
		_ = s.View(ctx, func(rd *Reader) error {
			var e error
			live, e = livePath(rd.tx, t)
			return e
		})
		if !live {
			_ = os.Remove(p)
		}
		return nil
	})
}

// reconcileParts sets every part row back to what its file holds: a part
// whose file is missing restarts from 0, a shorter one from its length
// (finding 6).
func (s *Store) reconcileParts(ctx context.Context) error {
	var ps []FilePart
	if err := s.View(ctx, func(rd *Reader) error {
		rows, err := rd.tx.Query(`SELECT room, msg, sha256, size, have FROM file_parts`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p FilePart
			if err := rows.Scan(&p.Room, &p.Msg, &p.SHA256, &p.Size, &p.Have); err != nil {
				return err
			}
			ps = append(ps, p)
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	for _, p := range ps {
		if !checkIDs(p.Room, p.Msg, p.SHA256) {
			continue
		}
		var have int64
		if st, err := os.Lstat(s.abs(partRel(p.Room, p.Msg, p.SHA256))); err == nil && st.Mode().IsRegular() {
			have = min(st.Size(), p.Have)
		}
		if have == p.Have {
			continue
		}
		if err := s.tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE file_parts SET have = ? WHERE room = ? AND msg = ? AND sha256 = ?`, have, p.Room, p.Msg, p.SHA256)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// ReserveStaging reserves n bytes of the room's and the machine's budget
// for request req before anything is copied (finding 9): reservations of
// requests still being staged count with the rows. The reservation ends
// with CreateSendRequest (the request's draft counts from then) or
// ReleaseStaging.
func (s *Store) ReserveStaging(ctx context.Context, room, req string, n int64) error {
	if !validReqID(req) || !validRoomID(room) || n < 0 {
		return ErrBadPath
	}
	s.stageMu.Lock()
	defer s.stageMu.Unlock()
	return s.View(ctx, func(rd *Reader) error {
		ok, err := s.fits(rd.tx, room, n)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNoSpace
		}
		s.resMu.Lock()
		defer s.resMu.Unlock()
		if s.staging == nil {
			s.staging = map[string]stageRes{}
		}
		s.staging[req] = stageRes{room: room, n: n}
		return nil
	})
}

// ReleaseStaging ends req's reservation.
func (s *Store) ReleaseStaging(req string) {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	delete(s.staging, req)
}

type stageRes struct {
	room string
	n    int64
}

// stagingHeld is what reservations of requests being staged hold, in room
// and in all.
func (s *Store) stagingHeld(room string) (r, m int64) {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	for _, x := range s.staging {
		m += x.n
		if x.room == room {
			r += x.n
		}
	}
	return r, m
}
