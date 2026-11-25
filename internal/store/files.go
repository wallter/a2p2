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

// UnknownFiles is how many files under blobs/ the last start found with
// a name a2p2 does not generate, and left alone [K3].
func (s *Store) UnknownFiles() int64 { return s.unknownFiles.Load() }

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

// MarkListInvalid records that the manifest sha, obtained for message msg,
// matched its hash and broke a rule: invalid for good for msg's versions
// that commit to it, with nothing it lists fetched.
func (s *Store) MarkListInvalid(ctx context.Context, room, msg, sha string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO file_lists (room, record, state) SELECT ?3, record, ?1 FROM (`+versionsSQL+`)
			WHERE true ON CONFLICT (room, record) DO UPDATE SET state = excluded.state`, ListInvalid, nil, room, sha, msg)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM file_wants WHERE room = ? AND msg = ? AND sha256 = ? AND list = 1`, room, msg, sha)
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
			return trash(tx, partRel(room, msg, sha), "part", room, msg, sha, 0, "", now)
		}
		return nil
	})
	if err == nil {
		s.RunTrash(ctx)
	}
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
	_, err = tx.Exec(`DELETE FROM file_lists WHERE room = ?1 AND state != ?2 AND record IN (`+versionsSQL+`)`,
		room, ListInvalid, room, sha, msg)
	return err
}

// versionsSQL selects the versions of message ?5 in room ?3 that commit to
// manifest ?4: the message itself and its author's replacements. A manifest
// is bound to the message it was obtained for [B1]: these, and no other
// record with the same commitment, get its list.
const versionsSQL = `SELECT s.record FROM file_sets s JOIN records r ON r.room = s.room AND r.id = s.record
	WHERE s.room = ?3 AND s.manifest = ?4 AND (r.id = ?5 AND r.kind = 'message' OR r.kind = 'replace' AND r.ref = ?5
	  AND r.author = (SELECT author FROM records WHERE room = ?3 AND id = ?5 AND kind = 'message'))`

// listTx records manifest m (bytes sha, verified and valid, obtained for
// message msg) as the list of msg's versions that commit to it, and of no
// other record: another message that copies the commitment gets nothing
// from it [B1].
func listTx(tx *sql.Tx, room, msg, sha string, m core.Manifest) error {
	rows, err := tx.Query(`SELECT record FROM file_sets WHERE room = ?1 AND ok = 0 AND record IN (`+versionsSQL+`)`,
		room, nil, room, sha, msg)
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
var ErrNotRegular = &FileError{Op: "stat", Code: "not_regular"}

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
	if RemoteOrDevice(path) {
		return Staged{}, ErrRemoteOrDevice
	}
	f, err := openSource(path)
	if err != nil {
		return Staged{}, fileErr("open", err)
	}
	defer f.Close()
	if stageOpened != nil {
		stageOpened()
	}
	st, err := f.Stat()
	if err != nil {
		return Staged{}, fileErr("stat", err)
	}
	if !st.Mode().IsRegular() {
		return Staged{}, ErrNotRegular
	}
	if st.Size() > max {
		return Staged{}, ErrTooLarge
	}
	// the path shown is the one the system reports for the open handle
	resolved, err := handlePath(f)
	if err != nil {
		return Staged{}, fileErr("path", err)
	}
	staged, err := s.stageCopy(req, n, f, max)
	if err != nil {
		return Staged{}, err
	}
	staged.Path = resolved
	return staged, nil
}

// stageOpened, in tests, runs after a source is opened.
var stageOpened func()

// RemoteOrDevice reports a path that is refused before any I/O: on
// Windows a UNC path, a \\?\ or \\.\ namespace or a reserved device name
// [fix-files-b 7]. Callers check it before anything else touches the path.
func RemoteOrDevice(path string) bool { return remoteOrDevice(path) }

// FileError is an error of a file operation that names the operation and
// a code, never a path [fix-files-b 8].
type FileError struct {
	Op   string // open, stat, path, read, write
	Code string // not_found, permission, not_regular, too_large, remote_or_device, changed, empty, io
	err  error  // a sentinel without a path, for errors.Is
}

func (e *FileError) Error() string { return "files: " + e.Op + ": " + e.Code }
func (e *FileError) Unwrap() error { return e.err }

// FileErrorOf is fileErr for a caller outside the store that touched a
// path itself (the node's size check before staging).
func FileErrorOf(op string, err error) error { return fileErr(op, err) }

// fileErr turns an error of the os package (which names its path) into a
// FileError that does not.
func fileErr(op string, err error) error {
	var fe *FileError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &fe):
		return err
	case errors.Is(err, fs.ErrNotExist):
		return &FileError{Op: op, Code: "not_found", err: fs.ErrNotExist}
	case errors.Is(err, fs.ErrPermission):
		return &FileError{Op: op, Code: "permission", err: fs.ErrPermission}
	}
	return &FileError{Op: op, Code: "io"}
}

// stageCopy copies the opened source into staging file n of req, hashing
// as it copies.
func (s *Store) stageCopy(req string, n int, f *os.File, max int64) (Staged, error) {
	dir := s.abs(stageDirRel(req))
	if err := s.mkdirSynced(dir); err != nil { // each new directory synced into its parent [B5]
		return Staged{}, fileErr("write", err)
	}
	tmp := filepath.Join(dir, tmpName())
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Staged{}, fileErr("write", err)
	}
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(f, max+1))
	if err == nil && size > max {
		err = ErrTooLarge
	}
	if err == nil && size == 0 {
		err = ErrEmptyFile
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
		return Staged{}, fileErr("write", err)
	}
	return Staged{Size: size, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// ErrTooLarge: a file over the size limit.
var ErrTooLarge = &FileError{Op: "read", Code: "too_large"}

// ErrEmptyFile: an empty file is not sent.
var ErrEmptyFile = &FileError{Op: "read", Code: "empty"}

// ErrRemoteOrDevice: a path refused before any I/O (RemoteOrDevice).
var ErrRemoteOrDevice = &FileError{Op: "open", Code: "remote_or_device"}

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
	// Clock is read inside the transaction that publishes, for the
	// request's expiry [B6]; nil uses the time WriteWithFiles was given.
	Clock func() time.Time
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
	if err := s.mkdirSynced(dir); err != nil { // each new directory synced into its parent [B5]
		return nil, fileErr("write", err) // an operation and a code, never a path [S3]
	}
	if err := s.putBlob(sf.Room, msha, func(w io.Writer) error { _, err := w.Write(sf.Manifest); return err }); err != nil {
		return nil, err
	}
	for _, f := range sf.Files {
		if f.Kept {
			continue
		}
		// A fresh private copy, hashed as it is written, put in place only
		// if its size and digest are the approved ones; staging is read
		// once, here, and never linked [fix-files-b 3].
		if err := s.copyApproved(sf.Room, stageRel(sf.Request, f.N), f.SHA256, f.Size); err != nil {
			return nil, err
		}
	}
	if err := syncDir(dir); err != nil {
		return nil, fileErr("write", err)
	}
	var r *core.VerifiedRecord
	err := s.tx(ctx, func(tx *sql.Tx) error {
		// the request's expiry, by the clock read in the transaction that
		// publishes, after the bytes were put in place [B6]
		at := now
		if sf.Clock != nil {
			at = sf.Clock()
		}
		var created int64
		var state string
		if err := tx.QueryRow(`SELECT created_at, state FROM requests WHERE id = ?`, sf.Request).Scan(&created, &state); err != nil {
			return ErrNotFound
		}
		if state != RequestPending {
			return ErrNotFound
		}
		if !at.Before(fromMS(created).Add(core.StagingLife)) {
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
		if err := listTx(tx, sf.Room, msg, msha, m); err != nil {
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
		return trash(tx, stageDirRel(sf.Request), "stage", "", sf.Request, "", 0, "", now)
	})
	if err != nil {
		return nil, err
	}
	s.runTrashLocked(ctx, false)
	return r, nil
}

// putBlob writes a blob under a temporary name, syncs it and renames it
// into place; the caller holds fileMu and syncs the directory.
// ErrStagingChanged: a staged file no longer has the size and digest a
// person approved; nothing is published.
var ErrStagingChanged = &FileError{Op: "read", Code: "changed"}

// copyApproved copies the staged file rel into a temporary file beside the
// room's blob for sha, hashing while it copies, and renames it into place
// only if exactly size bytes with digest sha were copied.
func (s *Store) copyApproved(room, rel, sha string, size int64) error {
	in, err := os.OpenFile(s.abs(rel), os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return ErrStagingChanged
	}
	defer in.Close()
	dst := s.blobPath(room, sha)
	tmp := filepath.Join(filepath.Dir(dst), tmpName())
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fileErr("write", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, size+1))
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && (n != size || hex.EncodeToString(h.Sum(nil)) != sha) {
		err = ErrStagingChanged
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return fileErr("write", err)
}

// putBlob writes bytes sha of room with write, unless the file at the
// blob's path already hashes to sha: a file there that does not is
// replaced, never adopted [K2]. Its errors name no path [S3].
func (s *Store) putBlob(room, sha string, write func(io.Writer) error) error {
	dst := s.blobPath(room, sha)
	if blobVerified(dst, sha, -1) {
		return nil
	}
	tmp := filepath.Join(filepath.Dir(dst), tmpName())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fileErr("write", err)
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
	return fileErr("write", err)
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
		return trash(tx, stageDirRel(id), "stage", "", id, "", 0, "", now)
	})
	if err == nil {
		s.RunTrash(ctx)
	}
	return err
}

// fileSetTx records, when a record with files is stored, the manifest it
// commits to. Its list is pending until the manifest is obtained for its
// own message: a list verified for another record is never copied, even
// with the same commitment [B1].
func fileSetTx(tx *sql.Tx, room, record, manifest string) error {
	_, err := tx.Exec(`INSERT INTO file_sets (room, record, manifest) VALUES (?,?,?) ON CONFLICT DO NOTHING`,
		room, record, manifest)
	return err
}

// leaveFiles drops every file row of a room being left and queues its
// paths: nothing of it may be kept.
func leaveFiles(tx *sql.Tx, room string, now time.Time) error {
	for _, q := range []struct{ sql, kind string }{
		{`SELECT '', sha256, 0, '', '' FROM files WHERE room = ?`, "blob"},
		{`SELECT msg, sha256, 0, '', '' FROM file_parts WHERE room = ?`, "part"},
		{`SELECT msg, sha256, size, path, ident FROM file_copies WHERE room = ?`, "copy"},
		{`SELECT id, '', 0, '', '' FROM requests WHERE room = ? AND kind = 'send_files' AND state = 'pending'`, "stage"},
	} {
		rows, err := tx.Query(q.sql, room)
		if err != nil {
			return err
		}
		var ts []trashRow
		for rows.Next() {
			var t trashRow
			if err := rows.Scan(&t.msg, &t.sha, &t.size, &t.path, &t.ident); err != nil {
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
			if err := trash(tx, rel, q.kind, room, t.msg, t.sha, t.size, t.ident, now); err != nil {
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
