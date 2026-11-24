package node

// Attachments (DESIGN section 12, PROTOCOL section 10; rules [F1] to [F12]
// of internal/model/delivery, attach.go): who may be served a file, the
// server's side of the file route (GET and PUT), carry jobs, and the
// sweeps that apply F1. The requester's side is files_fetch.go, sends
// with files files_send.go. Bytes and rows live in internal/store.
//
// Every decision about bytes is taken from rows, at the moment it is
// needed: a file is served, fetched, uploaded or copied only if the
// message's winning version lists it now and the peer may read that
// version now, so a deletion that has not run yet never widens anything.

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/members"
	"github.com/wallter/a2p2/internal/store"
)

// FileGet asks a peer for bytes of one file of one message, from Offset.
type FileGet struct {
	Room, Msg, SHA256 string
	Offset            int64
}

// FileReply is a peer's answer to a FileGet.
type FileReply struct {
	Status int // 200, 206, 202, 403, 404, 416, 429 or 503; 0: no answer
	// Total is the declared size of the whole; Offset and Length the
	// range in Body.
	Total, Offset, Length int64
	Body                  io.ReadCloser
	State                 string        // for 202: not_yet or gave_up
	RetryAfter            time.Duration // for 202, 429, 503
	Old                   bool          // a 404 in text/plain: the peer runs 0.1.0
}

// FilePut is one chunk of an upload: Length bytes at Offset of Total.
type FilePut struct {
	Room, Msg, SHA256     string
	Offset, Length, Total int64
}

// FileTransport carries the file route to peers (internal/peer over
// HTTP/2; an in-memory one in tests). CanDial says whether this machine
// can reach peer now: false for a machine that takes no connections.
type FileTransport interface {
	GetFile(ctx context.Context, peer string, q FileGet) (*FileReply, error)
	PutFile(ctx context.Context, peer string, q FilePut, body io.Reader) (status int, have int64, err error)
	CanDial(peer string) bool
}

// FileStatusNotYet and the others are the bodies of a 202.
const (
	FileNotYet = "not_yet"
	FileGaveUp = "gave_up"
)

// Files is the attachment part of a machine.
type Files struct {
	m  *Machine
	r  *Rooms
	t  FileTransport
	tr *transfers

	wg      sync.WaitGroup
	mu      sync.Mutex
	old     map[string]time.Time
	writing map[[3]string]bool // downloads and uploads in progress, by room, message and hash // peers that answered as 0.1.0, until when they are skipped

	// chunk is the most bytes of one response or upload (FileChunk; tests
	// lower it).
	chunk int64
	// limits of one transfer
	limits func() RateLimits
}

// FilesOpts shape the attachment part.
type FilesOpts struct {
	Chunk  int64
	Limits func() RateLimits
}

// NewFiles adds attachments to the machine behind r, reaching peers over
// t, and makes the machine sweep files after its writes and prunes. Call
// Start once before serving.
func NewFiles(r *Rooms, t FileTransport, o FilesOpts) *Files {
	f := &Files{m: r.m, r: r, t: t, tr: newTransfers(core.FileTransfers), old: map[string]time.Time{}, writing: map[[3]string]bool{},
		chunk: o.Chunk, limits: o.Limits}
	if f.chunk <= 0 || f.chunk > core.FileChunk {
		f.chunk = core.FileChunk
	}
	if f.limits == nil {
		f.limits = TransferLimits
	}
	r.files = f
	r.m.filesChanged = f.sweepRoom
	return f
}

// Start is what a start does: the store's reconcile [F10], then every
// room's sweep [F1].
func (f *Files) Start(ctx context.Context) error {
	if err := f.m.st.ReconcileFiles(ctx); err != nil {
		return err
	}
	return f.SweepAll(ctx)
}

// SweepAll applies F1 to every room with file rows.
func (f *Files) SweepAll(ctx context.Context) error {
	rooms, err := f.m.st.FileRooms(ctx)
	if err != nil {
		return err
	}
	for _, room := range rooms {
		if _, err := f.m.st.SweepFiles(ctx, room, f.m.ID, f.m.now()); err != nil {
			return err
		}
	}
	return nil
}

func (f *Files) sweepRoom(ctx context.Context, room string) {
	_, _ = f.m.st.SweepFiles(ctx, room, f.m.ID, f.m.now())
}

// UseFiles makes the exchange carry wants in its answers and run the
// file exchange after each pull (PROTOCOL 10.4).
func (x *Exchange) UseFiles(f *Files) { x.files = f }

// ---- who may be served ----

// fileCheck is everything the rules ask of one file of one message,
// read in one transaction.
type fileCheck struct {
	known    bool // the room is held
	member   bool // the peer is a member now in this machine's view
	mf       *store.MessageFiles
	ref      core.FileRef
	isList   bool
	listed   bool // F1's listed: the winning version lists it and may be kept
	mayRead  bool // the peer may read the winning version's text (F2)
	how      string
	size     int64 // of the bytes held in the room, if any
	held     bool  // held in the room, for any message
	mode     string
	listBad  bool // the winning version's manifest is invalid
	lapsed   bool
	wanting  bool
	want     store.FileWant
	part     store.FilePart
	hasPart  bool
	fits     bool
	selfAuth bool
}

// check reads, in one transaction, what serving, carrying or receiving
// sha of msg needs, for peer (empty: no peer).
func (f *Files) check(ctx context.Context, room, msg, sha, peer string) (fileCheck, error) {
	var c fileCheck
	if !core.ValidRoom(room) || !validRecordID(msg) || !core.ValidSHA256(sha) {
		return c, nil
	}
	st, err := f.r.State(room)
	if errors.Is(err, ErrNoRoom) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	c.known = true
	c.member = peer != "" && slices.Contains(st.Members, peer)
	if peer != "" && !c.member {
		return c, nil // nothing looked up for a machine that is not a member (finding 7)
	}
	now := f.m.now()
	err = f.m.view(ctx, room, func(v *roomView) error {
		if peer != "" {
			// The peer's permission to read the message first, before any
			// attachment row is read: an unknown message and one the peer
			// may not read do the same work (finding 7).
			m0, err := v.get(msg)
			if err != nil {
				return err
			}
			if m0 == nil || m0.f.Kind != core.KindMessage {
				return nil
			}
			if m0.author != peer {
				ok, err := readableBy(v, peerPermission(st, peer))(m0)
				if err != nil || !ok {
					return err
				}
			}
		}
		mf, err := v.rd.MessageFiles(room, msg, f.m.ID, now)
		if err != nil {
			return err
		}
		c.mf = mf
		c.ref, c.isList, c.listed = mf.Listed(sha)
		c.selfAuth = mf.Author == f.m.ID
		if mf.Winner != nil && !mf.Gone {
			if l, ok, err := v.rd.FileList(room, mf.Winner.ID); err != nil {
				return err
			} else if ok && l.State == store.ListInvalid {
				c.listBad = true
			}
			if peer != "" {
				w, err := v.get(mf.Winner.ID)
				if err != nil {
					return err
				}
				if w != nil {
					c.mayRead, err = readableBy(v, peerPermission(st, peer))(w)
					if err != nil {
						return err
					}
				}
			}
		}
		if c.how, err = v.rd.HeldFor(room, sha, msg); err != nil {
			return err
		}
		if c.size, c.held, err = v.rd.Held(room, sha); err != nil {
			return err
		}
		if c.mode, err = v.rd.FileMode(room); err != nil {
			return err
		}
		if c.want, c.wanting, err = v.rd.FileWant(room, msg, sha); err != nil {
			return err
		}
		if c.wanting && !c.want.Lapsed.IsZero() {
			c.wanting, c.lapsed = false, true
		}
		if c.part, c.hasPart, err = v.rd.FilePart(room, msg, sha); err != nil {
			return err
		}
		ru, mu, err := v.rd.FileUsage(room)
		if err != nil {
			return err
		}
		br, bm := f.m.st.FileBudgets()
		c.fits = c.listed && ru+c.ref.Size <= br && mu+c.ref.Size <= bm
		return nil
	})
	return c, err
}

func peerPermission(st members.State, peer string) members.Permission { return st.Permissions[peer] }

func validRecordID(s string) bool { return len(s) == 32 && isLowerHex(s) }

// servable is F2: the peer is a member now, the winning version lists sha
// and may be kept, the peer may read it, and the bytes are held here for
// this message (written here or received for it).
func (c fileCheck) servable() bool {
	return c.known && c.member && c.listed && c.mayRead && c.held && (c.how == store.HowSent || c.how == store.HowReceived)
}

// ---- the server ----

// FileServe is the server's answer to a GET.
type FileServe struct {
	Status                int
	Total, Offset, Length int64
	Body                  io.ReadCloser // for 200 and 206
	State                 string        // for 202
	RetryAfter            time.Duration
}

// The file route's refusals besides the pull route's.
const (
	CodeNotHeld  = "not_held"
	CodeBadRange = "bad_range"
)

// ErrNotHeldFile is every refusal a peer that may not have a file gets:
// one status, one body, decided from rows (PROTOCOL 10.3).
var ErrNotHeldFile = &PullError{Status: 404, Code: CodeNotHeld}

// ServeGet answers GET /p/v2/file/<room>/<msg>/<sha256> from peer, the
// node id the transport authenticated, in the order of PROTOCOL 10.3.
// Every refusal of a member is the same 404, decided from rows before any
// file is opened. The body checks again, at every chunk read after
// anything was written here, that the peer may still be served, and fails
// the response when it may not [F2].
func (f *Files) ServeGet(ctx context.Context, peer string, q FileGet) (*FileServe, error) {
	release, err := f.tr.take(peer, dirOut)
	if err != nil {
		return nil, busy(err)
	}
	keep := false
	defer func() {
		if !keep {
			release()
		}
	}()
	c, err := f.check(ctx, q.Room, q.Msg, q.SHA256, peer)
	if err != nil {
		return nil, unavailable(err)
	}
	switch {
	case !c.known || !c.member:
		return nil, notMember()
	case !c.listed || !c.mayRead:
		return nil, ErrNotHeldFile
	case !c.servable():
		return f.carry(ctx, peer, q, c)
	case q.Offset < 0 || q.Offset >= c.size:
		return nil, &PullError{Status: 416, Code: CodeBadRange}
	}
	b, err := f.m.st.OpenBlob(q.Room, q.SHA256)
	if err != nil {
		return nil, unavailable(err)
	}
	if _, err := b.Seek(q.Offset, io.SeekStart); err != nil {
		b.Close()
		return nil, unavailable(err)
	}
	n := min(c.size-q.Offset, f.chunk)
	status := 206
	if q.Offset == 0 && n == c.size {
		status = 200
	}
	keep = true
	body := &servedBody{f: f, file: b, left: n, release: release,
		recheck: func() bool {
			c, err := f.check(context.WithoutCancel(ctx), q.Room, q.Msg, q.SHA256, peer)
			return err == nil && c.servable()
		}}
	return &FileServe{Status: status, Total: c.size, Offset: q.Offset, Length: n, Body: body}, nil
}

func busy(err error) error {
	if errors.Is(err, errPeerBusy) {
		return &PullError{Status: 429, Code: CodeRateLimited, Detail: "file transfer in progress", RetryAfter: time.Second}
	}
	return &PullError{Status: 503, Code: CodeUnavailable, Detail: "file transfers busy", RetryAfter: time.Second}
}

// errRevoked: the server learned of a change that ends the peer's
// permission while it sent the file.
var errRevoked = errors.New("files: the transfer was stopped: the file may no longer be served")

// servedBody is a response's bytes, checked again whenever the store
// changed since the last check.
type servedBody struct {
	f       *Files
	file    *os.File
	left    int64
	recheck func() bool
	release func()
}

func (b *servedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, io.EOF
	}
	// Before every read, at most 64 KiB: the decision and the bytes are
	// never further apart than one read, whatever changed meanwhile, a
	// commit or only the clock (finding 1).
	if !b.recheck() {
		return 0, errRevoked
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	n, err := b.file.Read(p)
	b.left -= int64(n)
	if err == io.EOF && b.left > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (b *servedBody) Close() error {
	b.release()
	return b.file.Close()
}

// carry is [F9] at step 5 of PROTOCOL 10.3: a permitted request for a
// file this machine may keep but does not hold for the message. With the
// room's setting carry or keep and the file within the budget it answers
// "not yet" and starts one carry job (a want of the author); a lapsed job
// is answered "gave up" for a day; otherwise the same 404.
func (f *Files) carry(ctx context.Context, peer string, q FileGet, c fileCheck) (*FileServe, error) {
	if c.mode == store.FilesOff || c.isList || c.ref.Size < 1 || c.ref.Size > core.MaxFileSize || c.selfAuth {
		return nil, ErrNotHeldFile
	}
	switch {
	case c.lapsed:
		return &FileServe{Status: 202, State: FileGaveUp, RetryAfter: core.GaveUpLife}, nil
	case c.wanting || c.hasPart:
		return &FileServe{Status: 202, State: FileNotYet, RetryAfter: time.Minute}, nil
	case !c.fits:
		return nil, ErrNotHeldFile
	}
	_, err := f.m.st.IssueWant(ctx, store.FileWant{Room: q.Room, Msg: q.Msg, Version: c.mf.Winner.ID, SHA256: q.SHA256,
		Author: c.mf.Author, Size: c.ref.Size, At: f.m.now()})
	if errors.Is(err, store.ErrNoSpace) {
		return nil, ErrNotHeldFile
	}
	if err != nil {
		return nil, unavailable(err)
	}
	return &FileServe{Status: 202, State: FileNotYet, RetryAfter: time.Minute}, nil
}

// ServePut takes one chunk of an upload from peer (PROTOCOL 10.5). Before
// reading a byte of body it checks that peer is a member now and the
// author of the message, that this machine issued a want for this
// message, its winning version and the hash to that peer, unexpired, that
// the version still lists the hash, that the declared total is the listed
// size (a manifest: at most 16 KiB) within the reservation, and that the
// chunk starts at what is held. Any refusal is the same 404, and nothing
// is stored that was not asked for. It returns the bytes now held.
func (f *Files) ServePut(ctx context.Context, peer string, q FilePut, body io.Reader) (int64, error) {
	release, err := f.tr.take(peer, dirIn)
	if err != nil {
		return 0, busy(err)
	}
	defer release()
	c, err := f.check(ctx, q.Room, q.Msg, q.SHA256, peer)
	if err != nil {
		return 0, unavailable(err)
	}
	now := f.m.now()
	have := int64(0)
	if c.hasPart {
		have = c.part.Have
	}
	switch {
	case !c.known || !c.member || c.mf == nil || c.mf.Gone || c.mf.Author != peer:
		return 0, ErrNotHeldFile
	case !c.wanting || c.want.Author != peer || c.want.Version != c.mf.Winner.ID || !now.Before(c.want.At.Add(core.WantLife)):
		return 0, ErrNotHeldFile
	case !c.listed || c.how == store.HowSent || c.how == store.HowReceived:
		return 0, ErrNotHeldFile
	case c.isList && (q.Total < 1 || q.Total > core.MaxManifestBytes || q.Offset != 0 || q.Length != q.Total):
		return 0, ErrNotHeldFile
	case !c.isList && (q.Total != c.ref.Size || q.Total > c.want.Size):
		return 0, ErrNotHeldFile
	case q.Offset != have || q.Length < 1 || !c.isList && q.Length > f.chunk || q.Offset+q.Length > q.Total:
		return 0, ErrNotHeldFile
	}
	still := func() bool {
		c, err := f.check(context.WithoutCancel(ctx), q.Room, q.Msg, q.SHA256, peer)
		return err == nil && c.member && c.listed && c.wanting && c.mf.Author == peer && c.want.Version == c.mf.Winner.ID
	}
	got, err := f.receive(ctx, peer, q.Room, q.Msg, q.SHA256, q.Total, q.Offset, q.Length, c.isList, body, still)
	if err != nil {
		return got, ErrNotHeldFile
	}
	return got, nil
}

// receive writes length bytes at offset of a download of sha for msg
// (declared total), checks the whole when it is complete [F3], and makes
// it live. It returns the bytes now held.
//
// One writer holds (room, msg, sha) from the first byte through the
// verification and promotion, whoever the peer (finding 2); still, asked
// before every read of at most 64 KiB, stops the transfer when it may no
// longer continue (finding 1).
func (f *Files) receive(ctx context.Context, peer, room, msg, sha string, total, offset, length int64, isList bool,
	body io.Reader, still func() bool) (int64, error) {
	k := [3]string{room, msg, sha}
	f.mu.Lock()
	if f.writing[k] {
		f.mu.Unlock()
		return offset, errWriting
	}
	f.writing[k] = true
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.writing, k)
		f.mu.Unlock()
	}()
	now := f.m.now()
	p, err := f.m.st.ReservePart(ctx, room, msg, sha, total, now)
	if err != nil {
		return 0, err
	}
	if p.Size != total || p.Have != offset {
		return p.Have, errors.New("files: the range does not continue the part")
	}
	w, err := f.m.st.OpenPart(p, offset)
	if err != nil {
		return p.Have, err
	}
	rr := LimitRate(&checkedReader{r: io.LimitReader(body, length), still: still}, f.limits())
	if cl, ok := body.(io.Closer); ok {
		stop := rr.Watch(func() { cl.Close() })
		defer stop()
	}
	n, err := io.Copy(w, rr)
	if err == nil && n != length {
		err = io.ErrUnexpectedEOF
	}
	if serr := w.Sync(); err == nil {
		err = serr
	}
	w.Close()
	if n > 0 && err == nil {
		if perr := f.m.st.PartHave(ctx, p, offset+n); perr != nil {
			return offset, perr
		}
	}
	if rr.Slow() {
		_ = f.m.st.NoteFileObs(ctx, room, msg, sha, peer, store.AnswerSlow, now)
	}
	if err != nil {
		return offset, err
	}
	have := offset + n
	if have < total {
		return have, nil
	}
	p.Have = have
	got, err := f.m.st.PartSHA256(p)
	if err != nil {
		return have, err
	}
	if got != sha {
		_ = f.m.st.DropPart(ctx, room, msg, sha, now)
		_ = f.m.st.NoteFileObs(ctx, room, msg, sha, peer, store.AnswerBadFile, now)
		f.countRefusal(ctx, room, peer, store.AnswerBadFile)
		return 0, store.ErrBadBytes
	}
	var items *core.Manifest
	if isList {
		b, err := f.m.st.PartBytes(p, core.MaxManifestBytes)
		if err != nil {
			return have, err
		}
		m, perr := core.ParseManifest(b)
		if perr != nil {
			// its hash is signed: no other holder can have other bytes (F12)
			_ = f.m.st.MarkListInvalid(ctx, room, sha)
			_ = f.m.st.DropPart(ctx, room, msg, sha, now)
			return 0, perr
		}
		items = &m
	}
	// Permission again before the bytes go live: the version may have
	// changed while they came.
	c, err := f.check(ctx, room, msg, sha, "")
	if err != nil {
		return have, err
	}
	if !c.listed && !(c.mf != nil && c.mf.Referenced(sha, true)) {
		_ = f.m.st.DropPart(ctx, room, msg, sha, now)
		return 0, ErrFileGone
	}
	// and once more inside the transaction that publishes
	if err := f.m.st.CommitPart(ctx, p, store.HowReceived, items, now, func(rd *store.Reader) (bool, error) {
		mf, err := rd.MessageFiles(room, msg, f.m.ID, f.m.now())
		if err != nil {
			return false, err
		}
		_, _, listed := mf.Listed(sha)
		return listed || mf.Referenced(sha, true), nil
	}); err != nil {
		return 0, err
	}
	_ = f.m.st.NoteFileObs(ctx, room, msg, sha, peer, store.AnswerHas, now)
	f.sweepRoom(ctx, room) // a list may no longer list what was kept while it was pending
	f.m.wakes.touch(room)
	return have, nil
}

func (f *Files) countRefusal(ctx context.Context, room, peer, code string) {
	_, _ = f.m.st.Exchange(ctx, room, 0, func(tx *store.Tx) error {
		return tx.Refuse(room, peer, code, 1, f.m.now())
	})
}

// errWriting: another transfer writes the same file of the same message.
var errWriting = errors.New("files: another transfer is writing this file")

// checkedReader asks still before every read, each at most 64 KiB.
type checkedReader struct {
	r     io.Reader
	still func() bool
}

func (c *checkedReader) Read(p []byte) (int, error) {
	if c.still != nil && !c.still() {
		return 0, errRevoked
	}
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	return c.r.Read(p)
}

// ErrFileGone: no version of the message this machine may keep lists the
// file.
var ErrFileGone = &UserError{Code: ErrorNotFound, Msg: "no version of the message this machine may keep lists that file"}
