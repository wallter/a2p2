package store

// The reference rule [F1] over the rows, the staging expiry and failure of
// [F6] and [F8], and self-void [F11]: SweepFiles decides, in one
// transaction, which bytes, parts, copies, wants, list states and answers
// a room may keep, drops the rest and queues their paths.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wallter/a2p2/internal/core"
)

func isLetter(r rune) bool { return unicode.IsLetter(r) }

func lastRune(s string) (rune, int) { return utf8.DecodeLastRuneInString(s) }

// FileVersion is one version of a message as the file rules see it.
type FileVersion struct {
	ID       string
	Version  int64
	Manifest string // the header's files, or ""
	Keep     bool   // its text is one this machine may keep: not withdrawn, not expired by this clock, permitted
	ListOK   bool   // its manifest is here and valid
	Items    []core.FileRef
}

// MessageFiles is what the file rules need of one message: its author,
// whether it is gone (not held, not a message, withdrawn, or its author's
// admission voided), the winning version by header, and the other
// versions that count.
type MessageFiles struct {
	Msg      string
	Author   string
	Gone     bool
	Winner   *FileVersion
	Versions []*FileVersion
}

// newerVersion: the higher version wins, then the higher id.
func newerVersion(a, b *FileVersion) bool {
	if a.Version != b.Version {
		return a.Version > b.Version
	}
	return a.ID > b.ID
}

// MessageFiles reads what the file rules need of message msg of room, as
// this machine (self) holds it at now.
func (rd *Reader) MessageFiles(room, msg, self string, now time.Time) (*MessageFiles, error) {
	return messageFiles(rd.tx, room, msg, self, now, false)
}

// messageFiles is MessageFiles; aside leaves out the member cache (a room
// set aside: its membership is not known), so only the records decide.
func messageFiles(tx *sql.Tx, room, msg, self string, now time.Time, aside bool) (*MessageFiles, error) {
	rd := &Reader{tx: tx}
	mf := &MessageFiles{Msg: msg}
	h, ok, err := rd.Record(room, msg)
	if err != nil {
		return nil, err
	}
	if !ok || h.Record.Fields().Kind != core.KindMessage || h.Reason == ReasonWithdrawn {
		mf.Gone = true
		return mf, nil
	}
	mf.Author = h.Record.Author()
	hidden := false
	if !aside {
		if hidden, err = authorHidden(tx, room, mf.Author, self); err != nil {
			return nil, err
		}
	}
	if hidden {
		mf.Gone = true
		return mf, nil
	}
	hs := []Held{h}
	reps, err := rd.query(`WHERE room = ? AND ref = ? AND kind = 'replace' AND author = ?`, room, msg, mf.Author)
	if err != nil {
		return nil, err
	}
	for _, r := range reps {
		if r.Reason != ReasonWithdrawn {
			hs = append(hs, r)
		}
	}
	for _, x := range hs {
		f := x.Record.Fields()
		v := &FileVersion{ID: x.Record.ID(), Manifest: f.Files,
			Keep: (x.Reason == ReasonLive || x.Reason == ReasonMissing) && !TextExpired(f.Created, now)}
		if f.Kind == core.KindReplace {
			v.Version = f.Version
		}
		if v.Manifest != "" {
			if _, v.ListOK, err = rd.FileSet(room, v.ID); err != nil {
				return nil, err
			}
			if v.ListOK {
				if v.Items, err = rd.FileItems(room, v.ID); err != nil {
					return nil, err
				}
			}
		}
		mf.Versions = append(mf.Versions, v)
		if mf.Winner == nil || newerVersion(v, mf.Winner) {
			mf.Winner = v
		}
	}
	return mf, nil
}

// authorHidden is the reading rule's hidden author (node roomView.hidden):
// the room has a member cache and author, not this machine, is not in its
// Admitted set (a voided admission).
func authorHidden(tx *sql.Tx, room, author, self string) (bool, error) {
	if author == self {
		return false, nil
	}
	var all, adm int
	err := tx.QueryRow(`SELECT count(*), coalesce(sum(CASE WHEN node = ? AND admitted THEN 1 ELSE 0 END), 0)
		FROM room_members WHERE room = ?`, author, room).Scan(&all, &adm)
	return all > 0 && adm == 0, err
}

// selfVoided: this machine is in the member cache of room as not admitted.
func selfVoided(tx *sql.Tx, room, self string) (bool, error) {
	var all, adm int
	err := tx.QueryRow(`SELECT count(*), coalesce(sum(CASE WHEN node = ? AND admitted THEN 1 ELSE 0 END), 0)
		FROM room_members WHERE room = ?`, self, room).Scan(&all, &adm)
	return all > 0 && adm == 0, err
}

// Listed is whether the winning version commits to sha as its manifest or
// lists it in a verified manifest, and its text is one to keep: what
// serving, fetching and carrying require. It returns the file (a
// manifest's Size is 0) and whether sha is the manifest.
func (mf *MessageFiles) Listed(sha string) (core.FileRef, bool, bool) {
	v := mf.Winner
	if mf.Gone || v == nil || !v.Keep || v.Manifest == "" {
		return core.FileRef{}, false, false
	}
	if sha == v.Manifest {
		return core.FileRef{SHA256: sha}, true, true
	}
	for _, f := range v.Items {
		if f.SHA256 == sha {
			return f, false, true
		}
	}
	return core.FileRef{}, false, false
}

// earlierLists: a version other than the winner, whose text may still be
// kept, commits to sha as its manifest or lists it (the bound on keeping
// bytes while the winner's manifest is pending).
func (mf *MessageFiles) earlierLists(sha string) bool {
	for _, e := range mf.Versions {
		if e == mf.Winner || !e.Keep || e.Manifest == "" {
			continue
		}
		if e.Manifest == sha {
			return true
		}
		for _, f := range e.Items {
			if f.SHA256 == sha {
				return true
			}
		}
	}
	return false
}

// Referenced is F1: whether this machine may keep sha for the message.
// While the winning version's list is not here, what is held for the
// message stays, unserved (Listed says no), as long as an earlier version
// that lists it may be kept. heldAny says whether something is held,
// partial or copied for the message and sha.
func (mf *MessageFiles) Referenced(sha string, heldAny bool) bool {
	if _, _, ok := mf.Listed(sha); ok {
		return true
	}
	v := mf.Winner
	if mf.Gone || v == nil || !v.Keep || v.Manifest == "" || v.ListOK {
		return false
	}
	return heldAny && mf.earlierLists(sha)
}

// SendDraft is the stored draft of a send with files (requests.draft):
// what approval writes. The node fills it; the store reads what its rules
// need (the replaced message, its version then, and the files).
type SendDraft struct {
	Agent      string           `json:"agent"`
	Text       string           `json:"text"`
	To         []core.Addressee `json:"to,omitempty"`
	Everyone   bool             `json:"everyone,omitempty"`
	Parent     string           `json:"parent,omitempty"`
	AskHandled bool             `json:"ask_handled,omitempty"`
	Replaces   string           `json:"replaces,omitempty"`
	Base       string           `json:"base,omitempty"`    // the winning version of Replaces when the agent asked
	Members    int              `json:"members,omitempty"` // for everyone: the member count when the agent asked
	// MemberSet, for everyone, is the digest of the members' sorted keys
	// when the agent asked: the terms bind that set, not only its size [L2].
	MemberSet string      `json:"member_set,omitempty"`
	Files     []DraftFile `json:"files"`
}

// DraftFile is one file of a draft: staged, or kept from the version
// replaced.
type DraftFile struct {
	StagedFile
	Path string `json:"path,omitempty"` // as resolved from the opened file, for the person
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
	// Links, when above one, is how many names the staged file had on
	// disk: the path shown is only one of them. A draft an older binary
	// wrote has none, and an older binary ignores it.
	Links int `json:"links,omitempty"`
}

// MarshalJSON keeps the draft's files flat.
func (d DraftFile) MarshalJSON() ([]byte, error) {
	type flat struct {
		N      int    `json:"n"`
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
		Kept   bool   `json:"kept,omitempty"`
		Path   string `json:"path,omitempty"`
		Name   string `json:"name"`
		Type   string `json:"type,omitempty"`
		Links  int    `json:"links,omitempty"`
	}
	return json.Marshal(flat{d.N, d.SHA256, d.Size, d.Kept, d.Path, d.Name, d.Type, d.Links})
}

// UnmarshalJSON reads the flat form.
func (d *DraftFile) UnmarshalJSON(b []byte) error {
	var f struct {
		N      int    `json:"n"`
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
		Kept   bool   `json:"kept"`
		Path   string `json:"path"`
		Name   string `json:"name"`
		Type   string `json:"type"`
		Links  int    `json:"links"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*d = DraftFile{StagedFile: StagedFile{N: f.N, SHA256: f.SHA256, Size: f.Size, Kept: f.Kept}, Path: f.Path, Name: f.Name, Type: f.Type,
		Links: f.Links}
	return nil
}

// StillReplaceable is S3: the version a staged replacement was asked
// against is still the winning one, its text may be kept, and every file
// it keeps is listed by it and held here for the message.
func StillReplaceable(rd *Reader, room, self string, d SendDraft, now time.Time) (bool, error) {
	if d.Replaces == "" {
		return true, nil
	}
	mf, err := rd.MessageFiles(room, d.Replaces, self, now)
	if err != nil {
		return false, err
	}
	if mf.Gone || mf.Winner == nil || mf.Winner.ID != d.Base || !mf.Winner.Keep {
		return false, nil
	}
	for _, f := range d.Files {
		if !f.Kept {
			continue
		}
		_, isList, ok := mf.Listed(f.SHA256)
		how, err := rd.HeldFor(room, f.SHA256, d.Replaces)
		if err != nil {
			return false, err
		}
		if !ok || isList || how == "" || how == HowLocal {
			return false, nil
		}
	}
	return true, nil
}

// Reasons a staged send ended without a person approving it.
const (
	ReasonSendExpired  = "expired: nobody confirmed it within a day"
	ReasonSendVoided   = "this machine's admission to the room was voided"
	ReasonSendReplaced = "the message it replaces changed, or a file it keeps is gone"
	ReasonSendChanged  = "a staged file changed or is gone"
	ReasonSendHomeFile = "a staged file is one of a2p2's own files"
)

// SweepFiles applies [F1], the staging rules of [F6] and [F8], and [F11]
// to room in one transaction, as this machine (self) at now, then runs the
// deletion queue. It reports how many rows it dropped, a staged send it
// decided (expired, voided or replaced) counting as one.
func (s *Store) SweepFiles(ctx context.Context, room, self string, now time.Time) (int, error) {
	return s.sweepFiles(ctx, room, self, now, false)
}

// SweepRetained is SweepFiles for a room set aside, whose membership and
// reading frontier are not known: it drops only what retention requires.
// Files go when their message's text is not kept here (withdrawn, or past
// retention by this clock), and a staged send goes when its staging life
// is over. The member cache, wants, and a staged send's other checks
// (voided, replaced) are left alone.
func (s *Store) SweepRetained(ctx context.Context, room, self string, now time.Time) (int, error) {
	return s.sweepFiles(ctx, room, self, now, true)
}

func (s *Store) sweepFiles(ctx context.Context, room, self string, now time.Time, aside bool) (int, error) {
	n := 0
	err := s.tx(ctx, func(tx *sql.Tx) (err error) {
		n, err = sweepTx(tx, room, self, now, aside)
		return err
	})
	if err == nil && n > 0 {
		s.RunTrash(ctx)
	}
	return n, err
}

type fkey struct{ msg, sha string }

func sweepTx(tx *sql.Tx, room, self string, now time.Time, aside bool) (int, error) {
	dropped := 0
	msgs := map[string]*MessageFiles{}
	info := func(msg string) (*MessageFiles, error) {
		if mf := msgs[msg]; mf != nil {
			return mf, nil
		}
		mf, err := messageFiles(tx, room, msg, self, now, aside)
		if err == nil {
			msgs[msg] = mf
		}
		return mf, err
	}
	keys := func(q string) ([]fkey, error) {
		rows, err := tx.Query(q, room)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []fkey
		for rows.Next() {
			var k fkey
			if err := rows.Scan(&k.msg, &k.sha); err != nil {
				return nil, err
			}
			out = append(out, k)
		}
		return out, rows.Err()
	}
	heldAny := func(k fkey) (bool, error) { return (&Reader{tx: tx}).HeldForAny(room, k.msg, k.sha) }
	referenced := func(k fkey) (bool, error) {
		mf, err := info(k.msg)
		if err != nil {
			return false, err
		}
		any, err := heldAny(k)
		if err != nil {
			return false, err
		}
		return mf.Referenced(k.sha, any), nil
	}
	exec := func(q string, args ...any) error {
		res, err := tx.Exec(q, args...)
		if err == nil {
			if c, _ := res.RowsAffected(); c > 0 {
				dropped += int(c)
			}
		}
		return err
	}

	// Decide everything first, from the rows as they are, then drop.
	type drop struct {
		kind string
		k    fkey
	}
	var drops []drop
	for _, t := range []struct{ kind, q string }{
		{"for", `SELECT msg, sha256 FROM file_for WHERE room = ?`},
		{"part", `SELECT msg, sha256 FROM file_parts WHERE room = ?`},
		{"copy", `SELECT DISTINCT msg, sha256 FROM file_copies WHERE room = ?`},
		{"obs", `SELECT DISTINCT msg, sha256 FROM file_obs WHERE room = ?`},
	} {
		ks, err := keys(t.q)
		if err != nil {
			return 0, err
		}
		for _, k := range ks {
			ok, err := referenced(k)
			if err != nil {
				return 0, err
			}
			if !ok {
				drops = append(drops, drop{t.kind, k})
			}
		}
	}
	touched := map[string]bool{}
	for _, d := range drops {
		switch d.kind {
		case "for":
			if err := exec(`DELETE FROM file_for WHERE room = ? AND msg = ? AND sha256 = ?`, room, d.k.msg, d.k.sha); err != nil {
				return 0, err
			}
			touched[d.k.sha] = true
		case "part":
			if err := exec(`DELETE FROM file_parts WHERE room = ? AND msg = ? AND sha256 = ?`, room, d.k.msg, d.k.sha); err != nil {
				return 0, err
			}
			if err := trash(tx, partRel(room, d.k.msg, d.k.sha), "part", room, d.k.msg, d.k.sha, 0, "", now); err != nil {
				return 0, err
			}
		case "copy":
			rows, err := tx.Query(`SELECT path, size, ident FROM file_copies WHERE room = ? AND msg = ? AND sha256 = ?`, room, d.k.msg, d.k.sha)
			if err != nil {
				return 0, err
			}
			type cp struct {
				path, ident string
				size        int64
			}
			var cps []cp
			for rows.Next() {
				var c cp
				if err := rows.Scan(&c.path, &c.size, &c.ident); err != nil {
					rows.Close()
					return 0, err
				}
				cps = append(cps, c)
			}
			rows.Close()
			for _, c := range cps {
				if err := trash(tx, c.path, "copy", room, d.k.msg, d.k.sha, c.size, c.ident, now); err != nil {
					return 0, err
				}
			}
			if err := exec(`DELETE FROM file_copies WHERE room = ? AND msg = ? AND sha256 = ?`, room, d.k.msg, d.k.sha); err != nil {
				return 0, err
			}
		case "obs":
			if err := exec(`DELETE FROM file_obs WHERE room = ? AND msg = ? AND sha256 = ?`, room, d.k.msg, d.k.sha); err != nil {
				return 0, err
			}
		}
	}
	for sha := range touched {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM file_for WHERE room = ? AND sha256 = ?`, room, sha).Scan(&n); err != nil {
			return 0, err
		}
		if n == 0 {
			// queued with its size: its bytes count against the budgets
			// until its file is removed (usageSQL)
			var size int64
			if err := tx.QueryRow(`SELECT size FROM files WHERE room = ? AND sha256 = ?`, room, sha).Scan(&size); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, err
			}
			if err := exec(`DELETE FROM files WHERE room = ? AND sha256 = ?`, room, sha); err != nil {
				return 0, err
			}
			if err := trash(tx, blobRel(room, sha), "blob", room, "", sha, size, "", now); err != nil {
				return 0, err
			}
		}
	}

	// Wants and carry jobs: for the winning version they were issued for,
	// lapsing after WantLife; a lapsed one is "gave up" for GaveUpLife.
	ws, err := (&Reader{tx: tx}).allWants(room)
	if err != nil {
		return 0, err
	}
	if aside {
		ws = nil // nothing is fetched for a room set aside
	}
	for _, w := range ws {
		mf, err := info(w.Msg)
		if err != nil {
			return 0, err
		}
		_, _, listed := mf.Listed(w.SHA256)
		switch {
		case !listed || mf.Winner.ID != w.Version:
			// An edit: the want goes, and so does what an upload against it
			// left, or the part would hold its size with no want left to
			// count it or finish it (F10).
			err = exec(`DELETE FROM file_wants WHERE room = ? AND msg = ? AND sha256 = ?`, room, w.Msg, w.SHA256)
			if err == nil {
				err = dropPartFrom(tx, room, w.Msg, w.SHA256, w.Lapsed, now)
			}
		case !w.Lapsed.IsZero() && !now.Before(w.Lapsed.Add(core.GaveUpLife)):
			err = exec(`DELETE FROM file_wants WHERE room = ? AND msg = ? AND sha256 = ?`, room, w.Msg, w.SHA256)
			if err == nil {
				err = dropPartFrom(tx, room, w.Msg, w.SHA256, w.Lapsed, now)
			}
		case w.Lapsed.IsZero() && (!now.Before(w.At.Add(core.WantLife)) || w.Tries >= core.WantTries):
			err = exec(`UPDATE file_wants SET lapsed_at = ? WHERE room = ? AND msg = ? AND sha256 = ?`, ms(now), room, w.Msg, w.SHA256)
			if err == nil {
				err = dropLapsedPart(tx, room, w.Msg, w.SHA256, now)
			}
		case !w.Lapsed.IsZero():
			// lapsed by a 0.2.3 binary, which kept the part (F10)
			err = dropPartFrom(tx, room, w.Msg, w.SHA256, w.Lapsed, now)
		}
		if err != nil {
			return 0, err
		}
	}

	// List states of versions that are no longer the one to keep.
	rows, err := tx.Query(`SELECT l.record, l.state, s.manifest FROM file_lists l JOIN file_sets s
		ON s.room = l.room AND s.record = l.record WHERE l.room = ?`, room)
	if err != nil {
		return 0, err
	}
	type lrow struct{ record, state, manifest string }
	var ls []lrow
	for rows.Next() {
		var l lrow
		if err := rows.Scan(&l.record, &l.state, &l.manifest); err != nil {
			rows.Close()
			return 0, err
		}
		ls = append(ls, l)
	}
	rows.Close()
	for _, l := range ls {
		if l.state == ListInvalid {
			continue // for good (F12)
		}
		msg, err := msgOfRecord(tx, room, l.record)
		if err != nil {
			return 0, err
		}
		mf, err := info(msg)
		if err != nil {
			return 0, err
		}
		if mf.Gone || mf.Winner == nil || mf.Winner.ID != l.record || !mf.Winner.Keep {
			if err := exec(`DELETE FROM file_lists WHERE room = ? AND record = ?`, room, l.record); err != nil {
				return 0, err
			}
		}
	}

	// Parsed lists of versions no longer retained (withdrawn, expired,
	// outside this machine's permission, its author voided): their names,
	// sizes and hashes go and the list is no longer verified, so they
	// never seed another record's list (finding 8).
	srows, err := tx.Query(`SELECT record FROM file_sets WHERE room = ? AND ok = 1`, room)
	if err != nil {
		return 0, err
	}
	var okRecs []string
	for srows.Next() {
		var r string
		if err := srows.Scan(&r); err != nil {
			srows.Close()
			return 0, err
		}
		okRecs = append(okRecs, r)
	}
	srows.Close()
	for _, rec := range okRecs {
		msg, err := msgOfRecord(tx, room, rec)
		if err != nil {
			return 0, err
		}
		mf, err := info(msg)
		if err != nil {
			return 0, err
		}
		kept := false
		for _, v := range mf.Versions {
			kept = kept || v.ID == rec && v.Keep
		}
		if mf.Winner != nil && mf.Winner.ID == rec && mf.Winner.Keep {
			kept = true
		}
		if mf.Gone || !kept {
			if err := exec(`DELETE FROM file_items WHERE room = ? AND record = ?`, room, rec); err != nil {
				return 0, err
			}
			if err := exec(`UPDATE file_sets SET ok = 0 WHERE room = ? AND record = ?`, room, rec); err != nil {
				return 0, err
			}
			dropped++
		}
	}

	// Staged sends: expired, voided, or replacing what changed.
	sends, err := pendingSends(tx, room)
	if err != nil {
		return 0, err
	}
	voided := false
	if !aside {
		if voided, err = selfVoided(tx, room, self); err != nil {
			return 0, err
		}
	}
	for _, q := range sends {
		reason := ""
		var d SendDraft
		_ = json.Unmarshal([]byte(q.Draft), &d)
		switch {
		case voided:
			reason = ReasonSendVoided
		case !now.Before(q.Created.Add(core.StagingLife)):
			reason = ReasonSendExpired
		case aside:
		default:
			ok, err := StillReplaceable(&Reader{tx: tx}, room, self, d, now)
			if err != nil {
				return 0, err
			}
			if !ok {
				reason = ReasonSendReplaced
			}
		}
		if reason == "" {
			continue
		}
		state := RequestFailed
		if reason == ReasonSendExpired {
			state = RequestDeclined
		}
		if err := exec(`UPDATE requests SET state = ?, reason = ?, decided_at = ? WHERE id = ? AND state = ?`,
			state, reason, ms(now), q.ID, RequestPending); err != nil {
			return 0, err
		}
		if err := trash(tx, stageDirRel(q.ID), "stage", "", q.ID, "", 0, "", now); err != nil {
			return 0, err
		}
		dropped++ // a decided send: its writer's inbox lists it, and its stage is queued for deletion
	}
	return dropped, nil
}

func (rd *Reader) allWants(room string) ([]FileWant, error) {
	rows, err := rd.tx.Query(`SELECT `+wantCols+` FROM file_wants WHERE room = ?`, room)
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

// msgOfRecord is the message a version is of: itself, or its ref.
func msgOfRecord(tx *sql.Tx, room, record string) (string, error) {
	var kind, ref string
	err := tx.QueryRow(`SELECT kind, ref FROM records WHERE room = ? AND id = ?`, room, record).Scan(&kind, &ref)
	if errors.Is(err, sql.ErrNoRows) {
		return record, nil
	}
	if kind == string(core.KindReplace) {
		return ref, err
	}
	return record, err
}

func pendingSends(tx *sql.Tx, room string) ([]Request, error) {
	rows, err := tx.Query(`SELECT `+requestCols+` FROM requests WHERE room = ? AND kind = ? AND state = ?`,
		room, RequestSendFiles, RequestPending)
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

// FileRooms lists the rooms that have any file row or pending send.
func (s *Store) FileRooms(ctx context.Context) ([]string, error) {
	var out []string
	err := s.View(ctx, func(rd *Reader) error {
		rows, err := rd.tx.Query(`SELECT room FROM file_for UNION SELECT room FROM file_parts UNION SELECT room FROM file_copies
			UNION SELECT room FROM file_wants UNION SELECT room FROM file_obs UNION SELECT room FROM file_lists
			UNION SELECT room FROM requests WHERE kind = ? AND state = ?`, RequestSendFiles, RequestPending)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	slices.Sort(out)
	return out, err
}

// FileRecord is a version that commits to a manifest, and its message.
type FileRecord struct {
	Record, Msg, Manifest string
	OK                    bool // the manifest is here and its items stored
}

// FileRecords lists room's versions that commit to a manifest.
func (rd *Reader) FileRecords(room string) ([]FileRecord, error) {
	rows, err := rd.tx.Query(`SELECT f.record, f.manifest, f.ok, r.kind, r.ref FROM file_sets f
		JOIN records r ON r.room = f.room AND r.id = f.record WHERE f.room = ? ORDER BY f.record`, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileRecord
	for rows.Next() {
		var fr FileRecord
		var kind, ref string
		if err := rows.Scan(&fr.Record, &fr.Manifest, &fr.OK, &kind, &ref); err != nil {
			return nil, err
		}
		fr.Msg = fr.Record
		if kind == string(core.KindReplace) {
			fr.Msg = ref
		}
		out = append(out, fr)
	}
	return out, rows.Err()
}

// MessagesListing lists the messages of room with a version that commits
// to sha as its manifest or lists it.
func (rd *Reader) MessagesListing(room, sha string) ([]string, error) {
	rows, err := rd.tx.Query(`SELECT DISTINCT CASE r.kind WHEN ?3 THEN r.ref ELSE r.id END FROM records r
		WHERE r.room = ?1 AND r.id IN (SELECT record FROM file_sets WHERE room = ?1 AND manifest = ?2
		UNION SELECT record FROM file_items WHERE room = ?1 AND sha256 = ?2) ORDER BY 1`, room, sha, string(core.KindReplace))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ShownVersion reports whether a read showed agent version of message msg
// in room: a receipt or text mark at that version, or a confirmation of
// it.
func (rd *Reader) ShownVersion(agent, room, msg, version string) (bool, error) {
	var n int
	err := rd.tx.QueryRow(`SELECT (SELECT count(*) FROM shown WHERE agent = ?1 AND room = ?2 AND msg = ?3 AND version = ?4)
		+ (SELECT count(*) FROM reads WHERE agent = ?1 AND room = ?2 AND msg = ?3 AND version = ?4)`,
		agent, room, msg, version).Scan(&n)
	return n > 0, err
}
