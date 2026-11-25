package store

// What attachments do on disk beside the rows: making a verified part
// live, publishing an agent's copy, the deletion queue, and what a start
// does [F10]. Rulings B3, B4 and B5 of the second review:
//
//   - Validate first, then move bytes: the gate (the caller's rule, asked
//     inside the transaction) decides before a part is renamed into place
//     or a copy's name is linked, and the rows that make them live commit
//     in that same transaction.
//   - Deletion removes only what a2p2 can prove it installed: a copy by the
//     file identity recorded when it was linked (device and inode, or the
//     Windows volume and file index), never a file that only has the same
//     name, size or bytes. Every directory on the way is opened one
//     component at a time from the home, refusing a link. A lookup that
//     fails keeps the entry; it leaves the queue only when the path is gone
//     and its directory synced, and the queue also runs on a timer.
//   - No part outlives a restart: a start drops every part, so a download
//     or upload cut by a crash starts again from zero. Where a directory
//     cannot be synced (Windows) a start also reconciles rows against files
//     in both directions, and a removal stays queued until a later start
//     sees the path gone.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// schemaFiles2 is migration 14: the identity of the file a copy was
// published as (file_copies.ident, carried into file_trash.ident), so a
// deletion removes only that file; and file_trash.unlinked, a removal done
// where a directory cannot be synced, kept until a start sees it gone.
const schemaFiles2 = `
ALTER TABLE file_copies ADD COLUMN ident TEXT NOT NULL DEFAULT '';
ALTER TABLE file_trash ADD COLUMN ident TEXT NOT NULL DEFAULT '';
ALTER TABLE file_trash ADD COLUMN unlinked INTEGER NOT NULL DEFAULT 0;
`

// A Gate is a caller's rule asked inside the transaction that makes bytes
// live: true lets them go live in it. A Gate returning ErrStale is asked
// again in a new transaction (prepare reads what it needs outside one).
type Gate func(*Reader) (bool, error)

// ErrStale: what a gate was prepared against changed before its
// transaction began; it is prepared and asked again.
var ErrStale = errors.New("files: the view changed before the transaction")

// ErrRefused: a gate said no; nothing went live.
var ErrRefused = errors.New("files: the bytes may no longer go live")

// ErrCopyRefused is ErrRefused for an agent's copy.
var ErrCopyRefused = ErrRefused

// staleTries bounds how often a gate is prepared again.
const staleTries = 8

// gated runs fn in one transaction after prepare()'s gate said yes in it.
// A nil prepare asks nothing.
func (s *Store) gated(ctx context.Context, prepare func() Gate, fn func(*sql.Tx) error) error {
	var err error
	for range staleTries {
		var g Gate
		if prepare != nil {
			g = prepare()
		}
		err = s.tx(ctx, func(tx *sql.Tx) error {
			if g != nil {
				ok, err := g(&Reader{tx: tx})
				if err != nil {
					return err
				}
				if !ok {
					return ErrRefused
				}
			}
			return fn(tx)
		})
		if !errors.Is(err, ErrStale) {
			return err
		}
	}
	return err
}

// ---- parts ----

// CommitPart makes a complete, verified part live for msg [F10]. Under
// fileMu, one transaction asks the gate (the caller's rule: may this
// machine keep the file, is the uploader still the author with an open
// want), checks that the part's row is still there and complete (its
// budget still reserved), and only then moves the bytes to the room's blob
// (or drops them if the blob is held already), syncs the directory, and
// records the bytes as held for msg (how), drops the part row and the
// want, and, for a manifest (items not nil), records its list for msg's
// versions. If the gate says no, or anything fails, the part goes, and
// bytes moved for it unless a live row names them: ErrNotFound for a
// refusal [B3].
func (s *Store) CommitPart(ctx context.Context, p FilePart, how string, items *core.Manifest, now time.Time, gate func() Gate) error {
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
	moved := false
	err := s.gated(ctx, gate, func(tx *sql.Tx) error {
		var size, have int64
		if err := tx.QueryRow(`SELECT size, have FROM file_parts WHERE room = ? AND msg = ? AND sha256 = ?`,
			p.Room, p.Msg, p.SHA256).Scan(&size, &have); err != nil || size != p.Size || have != p.Size {
			return ErrNotFound
		}
		// valid: now the bytes move, then the rows that make them live
		if st, err := os.Lstat(blob); err == nil && st.Mode().IsRegular() {
			if err := os.Remove(part); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		} else if err := os.Rename(part, blob); err != nil {
			return err
		} else {
			moved = true
		}
		if err := syncDir(filepath.Dir(blob)); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM file_parts WHERE room = ? AND msg = ? AND sha256 = ?`, p.Room, p.Msg, p.SHA256); err != nil {
			return err
		}
		if err := holdTx(tx, p.Room, p.SHA256, p.Msg, how, p.Size); err != nil {
			return err
		}
		if items != nil {
			return listTx(tx, p.Room, p.Msg, p.SHA256, *items)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrRefused) || errors.Is(err, ErrStale) {
			err = ErrNotFound
		}
		_ = s.tx(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
			if _, err := tx.Exec(`DELETE FROM file_parts WHERE room = ? AND msg = ? AND sha256 = ?`, p.Room, p.Msg, p.SHA256); err != nil {
				return err
			}
			if err := trash(tx, partRel(p.Room, p.Msg, p.SHA256), "part", p.Room, p.Msg, p.SHA256, 0, "", now); err != nil {
				return err
			}
			if moved {
				return trash(tx, blobRel(p.Room, p.SHA256), "blob", p.Room, "", p.SHA256, 0, "", now)
			}
			return nil
		})
	}
	s.runTrashLocked(ctx, false)
	return err
}

// ---- copies for agents ----

// CopyReq names a copy of sha for msg to write for agent, named after
// name (sanitised) under files/<room>/.
type CopyReq struct {
	Room, Msg, SHA256, Agent, Name string
	Size                           int64
}

// PublishCopy writes a new copy of the room's verified bytes for an agent
// and publishes it, under fileMu, which every deletion also takes. Each
// call writes a new copy: an earlier copy of the same file for the same
// agent is never handed out again, and goes to the deletion queue when the
// new one is published.
//
//  1. One transaction asks the gate (the permission and the version
//     shown), checks the budget, chooses a free name and records the copy
//     as being written with its temporary name.
//  2. The bytes are copied from the blob to the temporary name in
//     files/<room> (opened one component at a time, never through a link),
//     asking live before every read, synced and checked against the hash.
//  3. One transaction asks the gate again, links the final name to the
//     temporary file (a hard link fails on an existing name, so it never
//     replaces a file, on every system), records the identity of the file
//     it linked, syncs the directory and marks the copy written.
//
// If the gate says no: ErrCopyRefused, and nothing of the copy is left.
func (s *Store) PublishCopy(ctx context.Context, q CopyReq, gate func() Gate, live func() bool) (FileCopy, error) {
	if !checkIDs(q.Room, q.Msg, q.SHA256) || q.Size < 1 {
		return FileCopy{}, ErrBadPath
	}
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	root, err := s.openDir(true, "files", q.Room)
	if err != nil {
		return FileCopy{}, err
	}
	defer root.Close()
	dir := s.abs(copyDirRel(q.Room))
	tmp := tmpName()
	c := FileCopy{Room: q.Room, Msg: q.Msg, SHA256: q.SHA256, Agent: q.Agent, State: "writing", Size: q.Size}
	reserved := false
	err = s.gated(ctx, gate, func(tx *sql.Tx) error {
		ok, err := s.fits(tx, q.Room, q.Size)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNoSpace
		}
		base := SafeName(q.Name)
		for i := 1; i < 1000; i++ {
			cand := base
			if i > 1 {
				cand = numbered(base, i)
			}
			rel := copyRel(q.Room, cand)
			var n int
			if err := tx.QueryRow(`SELECT count(*) FROM file_copies WHERE room = ? AND path = ?`, q.Room, rel).Scan(&n); err != nil {
				return err
			}
			if _, err := root.Lstat(cand); n > 0 || !errors.Is(err, fs.ErrNotExist) {
				continue
			}
			c.Path = rel
			_, err := tx.Exec(`INSERT INTO file_copies (room, msg, sha256, agent, path, state, size, tmp) VALUES (?,?,?,?,?,?,?,?)`,
				q.Room, q.Msg, q.SHA256, q.Agent, rel, c.State, q.Size, tmp)
			reserved = err == nil
			return err
		}
		return errors.New("files: no free name for the copy")
	})
	if err != nil {
		return FileCopy{}, err
	}
	name := path.Base(c.Path)
	linked, done := false, false
	defer func() {
		if !done {
			if linked {
				// not committed: the final name is the file just linked
				if ts, terr := root.Lstat(tmp); terr == nil {
					if fsI, ferr := root.Lstat(name); ferr == nil && os.SameFile(ts, fsI) {
						_ = root.Remove(name)
					}
				}
			}
			if reserved {
				_ = s.tx(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
					_, err := tx.Exec(`DELETE FROM file_copies WHERE room = ? AND path = ? AND state = 'writing'`, c.Room, c.Path)
					return err
				})
			}
		}
		_ = root.Remove(tmp)
		_ = syncDir(dir)
		s.runTrashLocked(context.WithoutCancel(ctx), false)
	}()
	if err := s.writeCopy(root, tmp, q, live); err != nil {
		return FileCopy{}, err
	}
	err = s.gated(ctx, gate, func(tx *sql.Tx) error {
		if err := root.Link(tmp, name); err != nil {
			return err
		}
		linked = true
		ident, err := identOf(root, name)
		if err != nil {
			return err
		}
		if err := syncDir(dir); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE file_copies SET state = 'done', tmp = '', ident = ? WHERE room = ? AND path = ? AND state = 'writing' AND tmp = ?`,
			ident, c.Room, c.Path, tmp)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrCopyRefused
		}
		// the agent's earlier copies of this file are not handed out again
		rows, err := tx.Query(`SELECT path, size, ident FROM file_copies WHERE room = ? AND msg = ? AND sha256 = ? AND agent = ?
			AND state = 'done' AND path != ?`, c.Room, c.Msg, c.SHA256, c.Agent, c.Path)
		if err != nil {
			return err
		}
		var olds []trashRow
		for rows.Next() {
			var t trashRow
			if err := rows.Scan(&t.path, &t.size, &t.ident); err != nil {
				rows.Close()
				return err
			}
			olds = append(olds, t)
		}
		rows.Close()
		for _, t := range olds {
			if err := trash(tx, t.path, "copy", c.Room, c.Msg, c.SHA256, t.size, t.ident, time.Now()); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM file_copies WHERE room = ? AND path = ?`, c.Room, t.path); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return FileCopy{}, err
	}
	done = true
	c.State = "done"
	return c, nil
}

// writeCopy copies the room's blob to tmp in root, asking live before
// every read of at most 64 KiB, synced and checked against size and hash.
func (s *Store) writeCopy(root *os.Root, tmp string, q CopyReq, live func() bool) error {
	src, err := s.OpenBlob(q.Room, q.SHA256)
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), &liveReader{r: src, live: live})
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && (n != q.Size || hex.EncodeToString(h.Sum(nil)) != q.SHA256) {
		err = ErrBadBytes
	}
	return err
}

type liveReader struct {
	r    io.Reader
	live func() bool
}

func (l *liveReader) Read(p []byte) (int, error) {
	if l.live != nil && !l.live() {
		return 0, ErrCopyRefused
	}
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	return l.r.Read(p)
}

// identOf is the identity of the regular file name in root, opened without
// following a link.
func identOf(root *os.Root, name string) (string, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", ErrBadPath
	}
	return fileIdent(f)
}

// openDir opens the directory home/parts[0]/parts[1]/... one component at
// a time, refusing a link or anything but a directory at each (ErrBadPath)
// and checking that what was opened is what was looked at. With create it
// makes what is missing, synced into its parent [B4].
func (s *Store) openDir(create bool, parts ...string) (*os.Root, error) {
	cur, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, err
	}
	at := s.root
	for _, name := range parts {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			cur.Close()
			return nil, ErrBadPath
		}
		st, err := cur.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) && create {
			if err := cur.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				cur.Close()
				return nil, err
			}
			if err := syncDir(at); err != nil {
				cur.Close()
				return nil, err
			}
			st, err = cur.Lstat(name)
		}
		if err != nil {
			cur.Close()
			return nil, err
		}
		if st.Mode()&fs.ModeSymlink != 0 || !st.IsDir() {
			cur.Close()
			return nil, ErrBadPath
		}
		next, err := cur.OpenRoot(name)
		cur.Close()
		if err != nil {
			return nil, err
		}
		if nst, err := next.Stat("."); err != nil || !os.SameFile(nst, st) {
			next.Close()
			return nil, ErrBadPath // swapped for a link between the look and the open
		}
		cur, at = next, filepath.Join(at, name)
	}
	return cur, nil
}

// ---- the deletion queue ----

// trash queues path for deletion after the commit; ident, for a copy, is
// the identity of the file published there.
func trash(tx *sql.Tx, rel, kind, room, msg, sha string, size int64, ident string, now time.Time) error {
	_, err := tx.Exec(`INSERT INTO file_trash (path, at, kind, room, msg, sha256, size, ident) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT (path) DO UPDATE SET at = excluded.at, kind = excluded.kind, room = excluded.room, msg = excluded.msg,
		sha256 = excluded.sha256, size = excluded.size, ident = excluded.ident, unlinked = 0`, rel, ms(now), kind, room, msg, sha, size, ident)
	return err
}

// RunTrash deletes what the queue names, skipping any path a live row
// names again; run after every change that queues, on the background
// timer, and at every start.
func (s *Store) RunTrash(ctx context.Context) {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	s.runTrashLocked(ctx, false)
}

type trashRow struct {
	path, kind, room, msg, sha, ident string
	size                              int64
	unlinked                          bool
}

// runTrashLocked runs the queue. An entry whose removal was not durable
// (unlinked: a directory that cannot be synced) is looked at again only at
// a start.
func (s *Store) runTrashLocked(ctx context.Context, atStart bool) {
	var rows []trashRow
	_ = s.View(ctx, func(rd *Reader) error {
		q, err := rd.tx.Query(`SELECT path, kind, room, msg, sha256, size, ident, unlinked FROM file_trash ORDER BY path`)
		if err != nil {
			return err
		}
		defer q.Close()
		for q.Next() {
			var t trashRow
			if err := q.Scan(&t.path, &t.kind, &t.room, &t.msg, &t.sha, &t.size, &t.ident, &t.unlinked); err != nil {
				return err
			}
			if !t.unlinked || atStart {
				rows = append(rows, t)
			}
		}
		return q.Err()
	})
	for _, t := range rows {
		live := true
		if err := s.View(ctx, func(rd *Reader) (err error) {
			live, err = livePath(rd.tx, t)
			return err
		}); err != nil {
			continue // fail closed: tried again later
		}
		done, durable := true, true
		if !live {
			done, durable = s.unlink(t)
		}
		switch {
		case !done:
			continue // kept: tried again at the next run and at every start
		case !durable:
			_ = s.tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.Exec(`UPDATE file_trash SET unlinked = 1 WHERE path = ?`, t.path)
				return err
			})
		default:
			_ = s.tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.Exec(`DELETE FROM file_trash WHERE path = ?`, t.path)
				return err
			})
		}
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

// unlink removes one queued path through its directory, opened one
// component at a time without following a link. done: the entry may leave
// the queue (the path is gone, or what is there is provably not what a2p2
// put there); durable: the removal is synced. Any lookup or removal error
// is not done [B4].
func (s *Store) unlink(t trashRow) (done, durable bool) {
	parts := strings.Split(t.path, "/")
	if len(parts) < 2 {
		return true, true
	}
	dirs, name := parts[:len(parts)-1], parts[len(parts)-1]
	switch t.kind {
	case "blob", "part", "stage":
		if dirs[0] != "blobs" {
			return true, true // never a path a2p2 queues: nothing to remove
		}
	case "copy":
		if !validRoomID(t.room) || t.path != copyRel(t.room, name) {
			return true, true
		}
	}
	root, err := s.openDir(false, dirs...)
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, ErrBadPath):
		return true, true // no directory of a2p2's there, so nothing of it below
	case err != nil:
		return false, false
	}
	defer root.Close()
	dir := s.abs(strings.Join(dirs, "/"))
	if t.kind == "stage" {
		if err := root.RemoveAll(name); err != nil {
			return false, false
		}
	} else {
		st, err := root.Lstat(name)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return true, true
		case err != nil:
			return false, false
		case !st.Mode().IsRegular():
			return true, true // a link or a directory: not a file a2p2 wrote
		}
		if t.kind == "copy" {
			id, err := identOf(root, name)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				return true, true
			case err != nil:
				return false, false
			case t.ident == "" || id != t.ident:
				return true, true // not the file published there: never removed
			}
		}
		if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, false
		}
	}
	if syncDir(dir) != nil {
		return false, false
	}
	if t.kind == "part" {
		if up, err := s.openDir(false, dirs[:len(dirs)-1]...); err == nil {
			_ = up.Remove(dirs[len(dirs)-1]) // the message's part directory, if now empty
			up.Close()
		}
	}
	return true, dirSyncDurable
}

// ---- the start ----

// ReconcileFiles is what a start does [F10]: finish the deletion queue
// (removals not known durable included), drop every part (a transfer cut
// by the restart starts again from zero), remove every copy still being
// written with its row, drop rows whose files are gone (a held blob, a
// written copy), and remove every temporary file, every file under blobs/
// that no row names and staging that no pending request names.
func (s *Store) ReconcileFiles(ctx context.Context) error {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	if err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT room, msg, sha256 FROM file_parts`)
		if err != nil {
			return err
		}
		var ps []FilePart
		for rows.Next() {
			var p FilePart
			if err := rows.Scan(&p.Room, &p.Msg, &p.SHA256); err != nil {
				rows.Close()
				return err
			}
			ps = append(ps, p)
		}
		rows.Close()
		for _, p := range ps {
			if _, err := tx.Exec(`DELETE FROM file_parts WHERE room = ? AND msg = ? AND sha256 = ?`, p.Room, p.Msg, p.SHA256); err != nil {
				return err
			}
			if err := trash(tx, partRel(p.Room, p.Msg, p.SHA256), "part", p.Room, p.Msg, p.SHA256, 0, "", time.Now()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	s.runTrashLocked(ctx, true)
	if err := s.reconcileCopies(ctx); err != nil {
		return err
	}
	if err := s.reconcileBlobs(ctx); err != nil {
		return err
	}
	base := s.abs("blobs")
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), tmpPrefix) {
			return nil
		}
		_ = os.Remove(p)
		return nil
	})
	return s.removeUnnamed(ctx)
}

// reconcileCopies removes what copies still being written left (their
// temporary file, and the final name if it is that same file: a crash
// between the link and the commit), with their rows, and drops the row of
// a written copy whose file is gone or is no longer the one published.
func (s *Store) reconcileCopies(ctx context.Context) error {
	type row struct {
		FileCopy
		tmp, ident string
	}
	var all []row
	if err := s.View(ctx, func(rd *Reader) error {
		rows, err := rd.tx.Query(`SELECT room, msg, sha256, agent, path, state, size, tmp, ident FROM file_copies`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c row
			if err := rows.Scan(&c.Room, &c.Msg, &c.SHA256, &c.Agent, &c.Path, &c.State, &c.Size, &c.tmp, &c.ident); err != nil {
				return err
			}
			all = append(all, c)
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	for _, c := range all {
		root, err := s.openDir(false, "files", c.Room)
		if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, ErrBadPath) {
			continue // fail closed: looked at again at the next start
		}
		name := path.Base(c.Path)
		drop := root == nil
		if root != nil {
			switch c.State {
			case "writing":
				drop = true
				if c.tmp != "" && strings.HasPrefix(c.tmp, tmpPrefix) && !strings.ContainsAny(c.tmp, `/\`) {
					ts, terr := root.Lstat(c.tmp)
					fsI, ferr := root.Lstat(name)
					if terr == nil && ferr == nil && os.SameFile(ts, fsI) {
						_ = root.Remove(name)
					}
					_ = root.Remove(c.tmp)
					_ = syncDir(s.abs(copyDirRel(c.Room)))
				}
			default:
				id, err := identOf(root, name)
				switch {
				case errors.Is(err, fs.ErrNotExist), errors.Is(err, ErrBadPath):
					drop = true
				case err == nil && id != c.ident:
					drop = true // not the file published: a2p2 no longer counts or removes it
				}
			}
			root.Close()
		}
		if drop {
			if err := s.tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.Exec(`DELETE FROM file_copies WHERE room = ? AND path = ?`, c.Room, c.Path)
				return err
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcileBlobs drops the rows of held bytes whose blob is gone (a
// publication a crash undid where a directory cannot be synced): they
// are fetched again if asked.
func (s *Store) reconcileBlobs(ctx context.Context) error {
	type key struct{ room, sha string }
	var ks []key
	if err := s.View(ctx, func(rd *Reader) error {
		rows, err := rd.tx.Query(`SELECT room, sha256 FROM files`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k key
			if err := rows.Scan(&k.room, &k.sha); err != nil {
				return err
			}
			ks = append(ks, k)
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	for _, k := range ks {
		if !validRoomID(k.room) || !core.ValidSHA256(k.sha) {
			continue
		}
		root, err := s.openDir(false, "blobs", k.room)
		gone := errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrBadPath)
		if err == nil {
			st, lerr := root.Lstat(k.sha)
			gone = errors.Is(lerr, fs.ErrNotExist) || lerr == nil && !st.Mode().IsRegular()
			root.Close()
		}
		if !gone {
			continue
		}
		if err := s.tx(ctx, func(tx *sql.Tx) error {
			for _, q := range []string{`DELETE FROM files WHERE room = ? AND sha256 = ?`, `DELETE FROM file_for WHERE room = ? AND sha256 = ?`} {
				if _, err := tx.Exec(q, k.room, k.sha); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
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
				live := true
				_ = s.View(ctx, func(rd *Reader) (e error) {
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
		case len(parts) == 4 && parts[1] == ".staging":
			return nil // within a live staging directory
		default:
			_ = os.Remove(p) // nothing a2p2 keeps across a start has this name (parts are dropped)
			return nil
		}
		live := true
		_ = s.View(ctx, func(rd *Reader) (e error) {
			live, e = livePath(rd.tx, t)
			return e
		})
		if !live {
			_ = os.Remove(p)
		}
		return nil
	})
}
