package node

// Attachments (DESIGN section 12, PROTOCOL section 10; rules [F1] to [F12]
// of internal/model/delivery, attach.go): the server's side of the file
// route (GET and PUT), carry jobs, and the sweeps that apply F1. Who may
// have bytes is decided in files_grant.go, the requester's side is
// files_fetch.go, sends with files files_send.go. Bytes and rows live in
// internal/store.
//
// Every decision about bytes is taken from one consistent view at the
// moment it is needed, held as a lease while bytes move, and asked again
// inside the transaction that makes bytes live: a file is served, fetched,
// uploaded or copied only if the message's winning version lists it now
// and the peer may read that version now, so a deletion that has not run
// yet never widens anything.

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/wallter/a2p2/internal/core"
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
// can reach peer now: false for a machine that takes no connections. A
// transport ends a transfer when its context ends: it closes the body of
// a response or stops an upload.
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

	wg  sync.WaitGroup
	mu  sync.Mutex
	old map[string]time.Time // peers that answered as 0.1.0, until when they are skipped
	// writing holds the files being written, by room, message and hash: one
	// writer from the first byte through verification and promotion.
	writing map[[3]string]bool

	// chunk is the most bytes of one response or upload (FileChunk; tests
	// lower it).
	chunk int64
	// limits of one transfer
	limits func() RateLimits

	// soon carries rooms whose file work Run does now rather than at the
	// next tick: a header with files arrived, or a quick second ask of a
	// list is due (PROTOCOL 10.2: the manifest follows the pull that
	// brought the header). Sends never block; a full channel drops.
	soon chan string
	// tried holds the records whose list was asked for once since start:
	// that first ask is not a failed answer (the holders may be fetching
	// it at the same moment), the second one is.
	tried map[string]bool
	// staging bounds the sends that open and copy files at once.
	staging chan struct{}
}

// StagingAtOnce is how many sends with files are staged at the same time.
const StagingAtOnce = 4

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
		chunk: o.Chunk, limits: o.Limits, soon: make(chan string, 64), tried: map[string]bool{},
		staging: make(chan struct{}, StagingAtOnce)}
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
		if _, err := f.sweep(ctx, room); err != nil {
			return err
		}
	}
	return nil
}

func (f *Files) sweepRoom(ctx context.Context, room string) {
	_, _ = f.sweep(ctx, room)
}

// sweep applies F1 to room; in a room set aside, whose membership is not
// known, only what retention requires (store.SweepRetained).
func (f *Files) sweep(ctx context.Context, room string) (int, error) {
	if f.r.isQuarantined(room) {
		return f.m.st.SweepRetained(ctx, room, f.m.ID, f.m.now())
	}
	return f.m.st.SweepFiles(ctx, room, f.m.ID, f.m.now())
}

// UseFiles makes the exchange carry wants in its answers and run the
// file exchange after each pull (PROTOCOL 10.4).
func (x *Exchange) UseFiles(f *Files) { x.files = f }

// writer takes (room, msg, sha) for one transfer that writes it; false if
// another holds it.
func (f *Files) writer(room, msg, sha string) (func(), bool) {
	k := [3]string{room, msg, sha}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writing[k] {
		return nil, false
	}
	f.writing[k] = true
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			delete(f.writing, k)
			f.mu.Unlock()
		})
	}, true
}

// ---- the server ----

// FileServe is the server's answer to a GET.
type FileServe struct {
	Status                int
	Total, Offset, Length int64
	Body                  io.ReadCloser // for 200 and 206
	State                 string        // for 202
	RetryAfter            time.Duration
	// Done, for a body, is closed when the response may no longer be sent
	// (a revocation or the clock): the transport then cuts it even while a
	// write is blocked. Limits are the transfer's time bounds.
	Done   <-chan struct{}
	Limits RateLimits
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
// file is opened. The response holds a lease on the decision [F2, B2]:
// every read asks it, and its Done ends when it no longer holds.
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
	l, err := f.grant(ctx, q.Room, f.serveRule(q.Msg, q.SHA256, peer))
	if err != nil {
		return nil, ErrNotHeldFile
	}
	b, err := f.m.st.OpenBlob(q.Room, q.SHA256)
	if err != nil {
		l.end()
		return nil, unavailable(err)
	}
	if _, err := b.Seek(q.Offset, io.SeekStart); err != nil {
		b.Close()
		l.end()
		return nil, unavailable(err)
	}
	n := min(c.size-q.Offset, f.chunk)
	status := 206
	if q.Offset == 0 && n == c.size {
		status = 200
	}
	keep = true
	body := &servedBody{file: b, left: n, lease: l, release: release}
	return &FileServe{Status: status, Total: c.size, Offset: q.Offset, Length: n, Body: body, Done: l.ctx.Done(), Limits: f.limits()}, nil
}

func busy(err error) error {
	if errors.Is(err, errPeerBusy) {
		return &PullError{Status: 429, Code: CodeRateLimited, Detail: "file transfer in progress", RetryAfter: time.Second}
	}
	return &PullError{Status: 503, Code: CodeUnavailable, Detail: "file transfers busy", RetryAfter: time.Second}
}

// servedBody is a response's bytes under its lease.
type servedBody struct {
	file    *os.File
	left    int64
	lease   *lease
	release func()
}

func (b *servedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, io.EOF
	}
	// Before every read, at most 64 KiB: the decision and the bytes are
	// never further apart than one read, whatever changed meanwhile, a
	// commit or only the clock.
	if !b.lease.valid() {
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
	b.lease.end()
	b.release()
	return b.file.Close()
}

// carry is [F9] at step 5 of PROTOCOL 10.3: a permitted request for a
// file this machine may keep but does not hold for the message. With the
// room's setting carry or keep and the file within the budget it answers
// "not yet" and starts one carry job (a want of the author); a lapsed job
// is answered "gave up" for a day; otherwise the same 404. The author
// itself is never carried for: it wrote the file, and a want it asked for
// would only hold this machine's budget while it never uploads (F10). One
// author's live carry jobs, counted by the bytes not yet received, reserve
// at most a quarter of the room budget (store.FileWant.Capped).
func (f *Files) carry(ctx context.Context, peer string, q FileGet, c fileCheck) (*FileServe, error) {
	if c.mode == store.FilesOff || c.isList || c.ref.Size < 1 || c.ref.Size > core.MaxFileSize || c.selfAuth ||
		peer == c.mf.Author {
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
	_, err := f.issueWant(ctx, store.FileWant{Room: q.Room, Msg: q.Msg, Version: c.mf.Winner.ID, SHA256: q.SHA256,
		Author: c.mf.Author, Size: c.ref.Size, At: f.m.now(), Capped: true})
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
// chunk starts at what is held (a chunk at 0 starts the upload again).
// Any refusal is the same 404, and nothing is stored that was not asked
// for. The body is read under a lease on that decision, and the bytes go
// live only if it holds inside the transaction that records them. It
// returns the bytes now held.
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
	have := int64(0)
	if c.hasPart {
		have = c.part.Have
	}
	switch {
	case !putAllowed(peer)(c):
		return 0, ErrNotHeldFile
	case c.isList && (q.Total < 1 || q.Total > core.MaxManifestBytes || q.Offset != 0 || q.Length != q.Total):
		return 0, ErrNotHeldFile
	case !c.isList && (q.Total != c.ref.Size || q.Total > c.want.Size):
		return 0, ErrNotHeldFile
	case q.Offset != have && q.Offset != 0 || q.Length < 1 || !c.isList && q.Length > f.chunk || q.Offset+q.Length > q.Total:
		return 0, ErrNotHeldFile
	}
	unlock, ok := f.writer(q.Room, q.Msg, q.SHA256)
	if !ok {
		return 0, ErrNotHeldFile
	}
	defer unlock()
	if q.Offset == 0 && have > 0 {
		// an upload that starts again starts from zero
		if err := f.m.st.DropPart(ctx, q.Room, q.Msg, q.SHA256, f.m.now()); err != nil {
			return 0, unavailable(err)
		}
	}
	r := f.putRule(q.Msg, q.SHA256, peer)
	l, err := f.grant(ctx, q.Room, r)
	if err != nil {
		return 0, ErrNotHeldFile
	}
	defer l.end()
	got, err := f.receive(peer, q.Room, q.Msg, q.SHA256, q.Total, q.Offset, q.Length, c.isList, body, l, r)
	if err != nil {
		return got, ErrNotHeldFile
	}
	return got, nil
}

// receive writes length bytes at offset of a download of sha for msg
// (declared total) under the lease l, checks the whole when it is complete
// [F3], and makes it live if publish still holds inside the transaction
// that records it [B3]. The caller holds the writer of (room, msg, sha).
// A failure drops the part: a transfer that fails starts again from zero.
// It returns the bytes now held.
func (f *Files) receive(peer, room, msg, sha string, total, offset, length int64, isList bool,
	body io.Reader, l *lease, publish rule) (have int64, err error) {
	ctx := context.WithoutCancel(l.ctx)
	now := f.m.now()
	defer func() {
		if err != nil {
			_ = f.m.st.DropPart(ctx, room, msg, sha, f.m.now())
			have = 0
		}
	}()
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
	rr := LimitRate(&leasedReader{r: io.LimitReader(body, length), l: l}, f.limits())
	if cl, ok := body.(io.Closer); ok {
		// a slow peer, a revocation or the clock end a read blocked on the network
		stop := rr.Watch(func() { cl.Close() })
		defer stop()
		after := context.AfterFunc(l.ctx, func() { cl.Close() })
		defer after()
	}
	n, err := io.Copy(w, rr)
	if err == nil && n != length {
		err = io.ErrUnexpectedEOF
	}
	if serr := w.Sync(); err == nil {
		err = serr
	}
	w.Close()
	if rr.Slow() {
		_ = f.m.st.NoteFileObs(ctx, room, msg, sha, peer, store.AnswerSlow, now)
	}
	if err != nil {
		if l.ctx.Err() != nil {
			err = errRevoked
		}
		return offset, err
	}
	if err := f.m.st.PartHave(ctx, p, offset+n); err != nil {
		return offset, err
	}
	have = offset + n
	if have < total {
		return have, nil
	}
	p.Have = have
	got, err := f.m.st.PartSHA256(p)
	if err != nil {
		return have, err
	}
	if got != sha {
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
			_ = f.m.st.MarkListInvalid(ctx, room, msg, sha)
			return 0, perr
		}
		items = &m
	}
	// The rule again inside the transaction that makes the bytes live,
	// before they move.
	_ = hookAt("files:commit") // tests: a change after the last byte
	if err := f.m.st.CommitPart(ctx, p, store.HowReceived, items, now, f.gate(room, publish)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, ErrFileGone
		}
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

// ErrFileGone: no version of the message this machine may keep lists the
// file.
var ErrFileGone = &UserError{Code: ErrorNotFound, Msg: "no version of the message this machine may keep lists that file"}
