package delivery

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Attachments (requirement L1; 0.2 draft, not built). A message or a
// replacement that has files carries in its signed header only a
// commitment: the SHA-256 of a small salted manifest (Record.Manifest).
// The manifest (salt, then name, size and SHA-256 of each file) and the
// files' bytes are content: they live beside the store, travel on the
// file route on request, are served under exactly the text's permission
// and are deleted with the text. FINDINGS.md, "Attachments", states the
// rules F1 to F11 in plain words; this file is them as code.
//
//   - F1 Referenced. A machine keeps a manifest or a file for a message
//     only while the message's winning version (highest version by header,
//     text or not) commits to that manifest, or lists the file in it, and
//     that version's text is one the machine may keep: not withdrawn, not
//     expired by its own clock, within its permission. Otherwise the bytes,
//     partial downloads and agents' copies go in the step that ended the
//     reference. Partials and copies are scoped to the message they were
//     authorised for, not to the hash.
//   - F2 Served. Bytes go only to a peer the sender sees as a member now
//     that may read the winning version's text in its view, and only bytes
//     the sender holds for that message (it wrote it, or received them for
//     it). Checked at every request and every chunk, in both directions.
//   - F3 Verified. The declared total must equal the size the manifest
//     gives (a manifest at most MaxManifest) before any byte is taken; a
//     partial download is hidden; the whole must match the hash or it is
//     discarded. Permission is checked again before a copy is written for
//     an agent.
//   - F4 Asked. Manifests are fetched automatically by a machine that may
//     read the text (they are the cheap first step, U4); file bytes only
//     when an agent that was shown the version listing them asks, or the
//     machine carries or keeps them for others (F9).
//   - F5 Told. A reader is told whether a file is here, partial, coming,
//     not asked, fetchable, unavailable or gone and why, and whether the
//     list of files has arrived; holders are known only from dated
//     answers, per message.
//   - F6 Confirmed. The bytes staged when the agent asked (opened once,
//     copied and hashed) are what the person approves and what is sent;
//     the digest is checked again on approval and the source never re-read.
//     Declined, expired after a day, failed or voided: nothing is written
//     and staging is deleted.
//   - F7 Budget. Held, partial (at its full declared size), staged and
//     copied bytes count; a transfer that would exceed the budget, or a
//     reference over MaxFileSize, is refused before any byte moves.
//   - F8 Replace. A replacement lists only the files its writer lists
//     again (and a person confirms); nothing is carried forward silently,
//     since a replacement shares its files with whoever may read it.
//   - F9 Carry and want. A member that receives a permitted fetch for a
//     file it may keep but lacks answers "not yet" and wants it (mode
//     Carry, the default; FilesOff answers as if it held nothing; Keep
//     wants every file). A machine that wants bytes from a peer it cannot
//     dial says so in its answer to that peer's pull, only for messages the
//     peer wrote or is known to hold, and the peer uploads over its own
//     connection under F2 and F3. A receiver accepts only uploads it asked
//     for.
//   - F10 Durable cleanup. Bytes are written to disk before their row is
//     committed, and deletions are queued in the transaction that decides
//     them; a restart deletes what the queue names and anything on disk
//     with no row.
//   - F11 Self-void. A machine that learns its own admission was voided
//     drops its staged sends.

// Limits of the model, as in PROTOCOL section 10.
const (
	MaxFileSize = 256 << 20
	MaxManifest = 16384
	MaxFiles    = 8
)

// FileRef is one file of a manifest.
type FileRef struct {
	Name string
	Size int
	Hash string // SHA-256 of the bytes, hex
}

// FileMode is a machine's setting for the room's files (F9).
type FileMode uint8

const (
	Carry    FileMode = iota // default: fetch and hold a file a permitted peer asked for
	FilesOff                 // hold only what this machine's agents asked for
	Keep                     // want every file it may keep
)

// encodeManifest is the manifest's bytes; parseManifest reads them back.
// The salt makes its hash unguessable from the file list.
func encodeManifest(salt string, files []FileRef) string {
	var b strings.Builder
	b.WriteString("a2p2-manifest|" + salt)
	for _, f := range files {
		fmt.Fprintf(&b, "|%d:%s:%s", f.Size, f.Hash, f.Name)
	}
	return b.String()
}

func parseManifest(data string) ([]FileRef, bool) {
	parts := strings.Split(data, "|")
	if len(parts) < 3 || parts[0] != "a2p2-manifest" || len(data) > MaxManifest {
		return nil, false
	}
	var out []FileRef
	for _, p := range parts[2:] {
		f := strings.SplitN(p, ":", 3)
		if len(f) != 3 {
			return nil, false
		}
		n, err := strconv.Atoi(f[0])
		if err != nil {
			return nil, false
		}
		out = append(out, FileRef{Name: f[2], Size: n, Hash: f[1]})
	}
	return out, true
}

type pkey struct {
	msg  RecordID
	hash string
}

type ckey struct {
	agent string
	msg   RecordID
	hash  string
}

// fileHeld is the verified bytes of one manifest or file on one machine.
// serve: the messages it holds them for and may serve them for (it wrote
// the message, or received the bytes for it). local: messages whose
// readers here use bytes it already held for another message; never
// served for those.
type fileHeld struct {
	data  string
	serve map[RecordID]bool
	local map[RecordID]bool
}

type partHeld struct {
	data string
	size int // the declared total, reserved in the budget
}

// Answer is what a peer said when asked for a file.
type Answer uint8

const (
	AnsHas    Answer = iota + 1 // it served the bytes
	AnsNo                       // not held, or not permitted: the same answer
	AnsNotYet                   // it will carry them; ask again later
)

// FileObs is a dated answer from one peer about one file of one message.
type FileObs struct {
	Machine MachineID
	At      Time
	Answer  Answer
}

type stagedSend struct {
	at   Time              // by this machine's clock
	data map[string]string // by hash: the bytes copied when the agent asked
}

// fileState is the attachment part of a machine's store, with the files
// on its disk beside it: durable, and restored with a backup.
type fileState struct {
	held    map[string]*fileHeld
	partial map[pkey]*partHeld
	obs     map[pkey]map[MachineID]FileObs
	copies  map[ckey]string // what fetch wrote for an agent
	staged  map[int]*stagedSend
	want    map[pkey]bool
	mode    FileMode
	budget  int // 0: no limit

	disk  map[string]bool // paths present on disk
	queue []string        // deletions decided and not yet done
	fail  string          // test hook: crash at "rename" (before a row) or "commit" (before deleting)
	crash bool            // a crash was injected; Crash or Restore reconciles
}

func newFileState() *fileState {
	return &fileState{held: map[string]*fileHeld{}, partial: map[pkey]*partHeld{},
		obs: map[pkey]map[MachineID]FileObs{}, copies: map[ckey]string{}, staged: map[int]*stagedSend{},
		want: map[pkey]bool{}, disk: map[string]bool{}}
}

func (f *fileState) clone() *fileState {
	c := newFileState()
	c.mode, c.budget, c.fail, c.crash = f.mode, f.budget, f.fail, f.crash
	for h, x := range f.held {
		y := &fileHeld{data: x.data, serve: map[RecordID]bool{}, local: map[RecordID]bool{}}
		for k := range x.serve {
			y.serve[k] = true
		}
		for k := range x.local {
			y.local[k] = true
		}
		c.held[h] = y
	}
	for k, p := range f.partial {
		q := *p
		c.partial[k] = &q
	}
	for k, m := range f.obs {
		n := map[MachineID]FileObs{}
		for a, b := range m {
			n[a] = b
		}
		c.obs[k] = n
	}
	for k, v := range f.copies {
		c.copies[k] = v
	}
	for k, v := range f.staged {
		c.staged[k] = v // replaced, never modified
	}
	for k := range f.want {
		c.want[k] = true
	}
	for k := range f.disk {
		c.disk[k] = true
	}
	c.queue = append([]string(nil), f.queue...)
	return c
}

func (f *fileState) empty() bool {
	return len(f.held) == 0 && len(f.partial) == 0 && len(f.copies) == 0 && len(f.staged) == 0 &&
		len(f.want) == 0 && len(f.disk) == 0
}

func blobPath(h string) string         { return "blob/" + h }
func partPath(k pkey) string           { return "part/" + string(k.msg) + "/" + k.hash }
func copyPath(k ckey) string           { return "copy/" + k.agent + "/" + string(k.msg) + "/" + k.hash }
func stagePath(n int, h string) string { return fmt.Sprintf("stage/%d/%s", n, h) }

// rowPaths is the disk the rows describe.
func (f *fileState) rowPaths() map[string]bool {
	out := map[string]bool{}
	for h := range f.held {
		out[blobPath(h)] = true
	}
	for k := range f.partial {
		out[partPath(k)] = true
	}
	for k := range f.copies {
		out[copyPath(k)] = true
	}
	for n, s := range f.staged {
		for h := range s.data {
			out[stagePath(n, h)] = true
		}
	}
	return out
}

// used is what counts against the budget (F7).
func (f *fileState) used() int {
	n := 0
	for _, x := range f.held {
		n += len(x.data)
	}
	for _, p := range f.partial {
		n += p.size
	}
	for _, c := range f.copies {
		n += len(c)
	}
	for _, s := range f.staged {
		for _, d := range s.data {
			n += len(d)
		}
	}
	return n
}

func (f *fileState) fits(n int) bool { return f.budget == 0 || f.used()+n <= f.budget }

// flush does the deletions the queue names, unless a crash is injected
// between the commit and the deletion (F10).
func (f *fileState) flush() {
	if f.crash {
		return // stopped: the restart finishes the queue
	}
	if f.fail == "commit" && len(f.queue) > 0 {
		f.fail, f.crash = "", true
		return
	}
	for _, p := range f.queue {
		delete(f.disk, p)
	}
	f.queue = nil
}

// unqueue takes p off the deletion queue: it is being written again.
func (f *fileState) unqueue(p string) {
	out := f.queue[:0]
	for _, q := range f.queue {
		if q != p {
			out = append(out, q)
		}
	}
	f.queue = out
}

// reconcile is what a start does (F10): finish the queue, then delete
// every file on disk that no row names.
func (f *fileState) reconcile() {
	for _, p := range f.queue {
		delete(f.disk, p)
	}
	f.queue = nil
	rows := f.rowPaths()
	for p := range f.disk {
		if !rows[p] {
			delete(f.disk, p)
		}
	}
	f.crash = false
}

// Errors of the attachment operations.
var (
	ErrFileGone    = errors.New("no version of the message this machine may keep refers to the file")
	ErrNotHeldFile = errors.New("not held: the server does not hold the file for that message, or may not serve it")
	ErrNotYet      = errors.New("not yet: the server will carry the file; ask again later")
	ErrNoDial      = errors.New("the server takes no connections; it was asked to upload when it next connects")
	ErrBadFile     = errors.New("the bytes received do not match the reference")
	ErrNoSpace     = errors.New("the room's file budget on this machine would be exceeded")
	ErrTooLarge    = errors.New("the reference is over the size limit")
	ErrNoProposal  = errors.New("no such request waiting for a person")
)

// winner is the version of message h whose text wins by header: the
// message, or its author's replacement with the highest version, then the
// highest id, text held or not. A withdrawn replacement does not count.
func (m *Machine) winner(h *held) *held {
	best := h
	for _, id := range m.st.replaced[h.rec.ID] {
		r := m.get(id)
		if r == nil || r.rec.Author != h.rec.Author || r.dropped == Withdrawn {
			continue
		}
		if m.newer(r.rec, best.rec) {
			best = r
		}
	}
	return best
}

// keepable says whether the text of a held record is one this machine may
// keep now, by its own evidence and clock.
func (m *Machine) keepable(h *held, now Time) bool {
	return (h.dropped == Live || h.dropped == Missing) && m.textGone(h.rec, now) == Live
}

// manifestOf is the file list of version v as this machine holds it:
// known says whether the manifest is here.
func (m *Machine) manifestOf(v *held) (files []FileRef, known bool) {
	if v.rec.Manifest == "" {
		return nil, true
	}
	x := m.st.files.held[v.rec.Manifest]
	if x == nil {
		return nil, false
	}
	files, ok := parseManifest(x.data)
	return files, ok
}

// referenced is F1: whether this machine may keep hash (a manifest or a
// file) for message msg, with the winning version and the reference. While
// the winning version's list has not arrived, what the machine already has
// for the message stays (unserved and unshown: listed says no) until the
// list says whether it is still listed.
func (m *Machine) referenced(msg RecordID, hash string, now Time) (*held, FileRef, bool) {
	v, ref, ok := m.listed(msg, hash, now)
	if ok || v == nil || !m.keepable(v, now) || v.rec.Manifest == "" {
		return v, ref, ok
	}
	if _, known := m.manifestOf(v); known {
		return v, ref, false
	}
	f := m.st.files
	k := pkey{msg, hash}
	if x := f.held[hash]; x != nil && (x.serve[msg] || x.local[msg]) {
		return v, FileRef{Hash: hash, Size: len(x.data)}, true
	}
	if p := f.partial[k]; p != nil {
		return v, FileRef{Hash: hash, Size: p.size}, true
	}
	for c := range f.copies {
		if c.msg == msg && c.hash == hash {
			return v, FileRef{Hash: hash}, true
		}
	}
	return v, ref, false
}

// listed is whether the winning version of msg commits to hash as its
// manifest or lists it in a manifest held here, and its text is one this
// machine may keep: what serving, fetching and carrying require.
func (m *Machine) listed(msg RecordID, hash string, now Time) (*held, FileRef, bool) {
	h := m.get(msg)
	if h == nil || h.rec.Kind != Message || h.dropped == Withdrawn {
		return nil, FileRef{}, false
	}
	v := m.winner(h)
	if !m.keepable(v, now) || v.rec.Manifest == "" {
		return v, FileRef{}, false
	}
	if hash == v.rec.Manifest {
		return v, FileRef{Hash: hash}, true // a manifest: its size is bounded by MaxManifest
	}
	files, _ := m.manifestOf(v)
	for _, f := range files {
		if f.Hash == hash {
			return v, f, true
		}
	}
	return v, FileRef{}, false
}

// filesOf is what an inbox entry shows for version id: its files, or that
// it has files whose list has not arrived.
func (m *Machine) filesOf(id RecordID) (files []FileRef, pending bool) {
	h := m.get(id)
	if h == nil || h.rec.Manifest == "" {
		return nil, false
	}
	files, known := m.manifestOf(h)
	return append([]FileRef(nil), files...), !known
}

// dropFiles applies F1 and the staging expiry of F6, then deletes from
// disk what it dropped (F10).
func (m *Machine) dropFiles(now Time) {
	f := m.st.files
	if f.empty() {
		return
	}
	for hash, x := range f.held {
		for _, set := range []map[RecordID]bool{x.serve, x.local} {
			for msg := range set {
				if _, _, ok := m.referenced(msg, hash, now); !ok {
					delete(set, msg)
				}
			}
		}
		if len(x.serve) == 0 && len(x.local) == 0 {
			delete(f.held, hash)
			f.queue = append(f.queue, blobPath(hash))
		}
	}
	for k := range f.partial {
		if _, _, ok := m.referenced(k.msg, k.hash, now); !ok {
			delete(f.partial, k)
			f.queue = append(f.queue, partPath(k))
		}
	}
	for k := range f.copies {
		if _, _, ok := m.referenced(k.msg, k.hash, now); !ok {
			delete(f.copies, k)
			f.queue = append(f.queue, copyPath(k))
		}
	}
	for k := range f.want {
		if _, _, ok := m.referenced(k.msg, k.hash, now); !ok {
			delete(f.want, k)
		}
	}
	for k := range f.obs {
		if _, _, ok := m.referenced(k.msg, k.hash, now); !ok {
			delete(f.obs, k)
		}
	}
	for n, s := range f.staged {
		if m.clock(now) >= s.at+Day {
			m.dropStaged(n)
		}
	}
	sort.Strings(f.queue)
	f.flush()
}

func (m *Machine) dropStaged(n int) {
	f := m.st.files
	s := f.staged[n]
	if s == nil {
		return
	}
	delete(f.staged, n)
	for h := range s.data {
		f.queue = append(f.queue, stagePath(n, h))
	}
}

// commitBlob stores verified bytes for msg: on disk first, then the row
// (F10); a crash injected between leaves a file no row names.
func (m *Machine) commitBlob(hash, data string, msg RecordID) bool {
	f := m.st.files
	if f.crash {
		return false
	}
	f.unqueue(blobPath(hash))
	f.disk[blobPath(hash)] = true
	if f.fail == "rename" {
		f.fail, f.crash = "", true
		return false
	}
	x := f.held[hash]
	if x == nil {
		x = &fileHeld{data: data, serve: map[RecordID]bool{}, local: map[RecordID]bool{}}
		f.held[hash] = x
	}
	x.serve[msg] = true
	delete(x.local, msg)
	k := pkey{msg, hash}
	if f.partial[k] != nil {
		delete(f.partial, k)
		f.queue = append(f.queue, partPath(k))
	}
	delete(f.want, k)
	return true
}

func (m *Machine) observeFile(k pkey, peer MachineID, a Answer, now Time) {
	o := m.st.files.obs[k]
	if o == nil {
		o = map[MachineID]FileObs{}
		m.st.files.obs[k] = o
	}
	o[peer] = FileObs{Machine: peer, At: now, Answer: a}
}

// mayServe is F2 on sender s, for bytes of hash for msg to peer p.
func (s *Machine) mayServe(p MachineID, msg RecordID, hash string, now Time) (string, bool) {
	if !s.st.view.now[p] {
		return "", false
	}
	x := s.st.files.held[hash]
	if x == nil || !x.serve[msg] {
		return "", false
	}
	v, _, ok := s.listed(msg, hash, now)
	if !ok || !mayRead(s.st.view.adm[p], v.rec) {
		return "", false
	}
	return x.data, true
}

// carry is F9 on server s for a permitted request from p that s cannot
// serve: whether to answer "not yet", having wanted what it needs (the
// manifest first, if that is what it lacks).
func (s *Machine) carry(p MachineID, msg RecordID, hash string, now Time) bool {
	f := s.st.files
	if f.mode == FilesOff || !s.st.view.now[p] {
		return false
	}
	h := s.get(msg)
	if h == nil || h.rec.Kind != Message || h.dropped == Withdrawn {
		return false
	}
	v := s.winner(h)
	if !s.keepable(v, now) || v.rec.Manifest == "" || !mayRead(s.st.view.adm[p], v.rec) {
		return false
	}
	if _, known := s.manifestOf(v); !known {
		f.want[pkey{msg, v.rec.Manifest}] = true // the list first; the file once a request names it again
		return true
	}
	_, ref, ok := s.listed(msg, hash, now)
	if !ok || hash == v.rec.Manifest || !f.fits(ref.Size) || ref.Size > MaxFileSize {
		return false
	}
	f.want[pkey{msg, hash}] = true
	return true
}

// Transfer is one movement of bytes, recorded with what the sender's view
// and the true grants said at that moment.
type Transfer struct {
	From, To  MachineID
	Msg       RecordID
	Hash      string
	Bytes     int
	Upload    bool
	Permitted bool // the sender saw To as a member, and the true grants let To read the version
	Keeps     bool // the receiver may keep it (F1)
}

func (w *World) noteTransfer(from, to *Machine, k pkey, n int, upload bool) {
	v, _, _ := from.listed(k.msg, k.hash, w.Now)
	_, _, keeps := to.referenced(k.msg, k.hash, w.Now)
	w.transfers = append(w.transfers, Transfer{From: from.ID, To: to.ID, Msg: k.msg, Hash: k.hash, Bytes: n,
		Upload: upload, Keeps: keeps,
		Permitted: from.st.view.now[to.ID] && v != nil && mayRead(w.admission[to.ID], v.rec) && !w.voided(to.ID)})
}

// Transfers returns every movement of bytes so far.
func (w *World) Transfers() []Transfer { return append([]Transfer(nil), w.transfers...) }

// receive is F3 for bytes coming to m for k from sender, declared total
// total: checked before any byte is taken, verified as a whole.
func (w *World) receive(m, from *Machine, k pkey, ref FileRef, data string, max int, upload bool) (FetchResult, error) {
	f := m.st.files
	isManifest := m.isManifestKey(k)
	switch {
	case isManifest && len(data) > MaxManifest, !isManifest && len(data) != ref.Size:
		m.observeFile(k, from.ID, AnsNo, w.Now)
		return FetchResult{}, ErrBadFile // the declared total is wrong: refused before receiving
	case isManifest && f.partial[k] == nil && !f.fits(len(data)):
		return FetchResult{}, ErrNoSpace // a manifest's total is known from the response, before its bytes
	case f.crash:
		return FetchResult{}, ErrNotAllow // crashed: nothing until the restart
	}
	have := ""
	if p := f.partial[k]; p != nil {
		have = p.data
	}
	part := ""
	if len(have) < len(data) {
		part = data[len(have):]
	}
	if max > 0 && len(part) > max {
		part = part[:max]
	}
	w.noteTransfer(from, m, k, len(part), upload)
	m.observeFile(k, from.ID, AnsHas, w.Now)
	got := have + part
	if len(got) < len(data) {
		if f.partial[k] == nil {
			f.unqueue(partPath(k))
			f.disk[partPath(k)] = true
		}
		f.partial[k] = &partHeld{data: got, size: len(data)}
		return FetchResult{Got: len(part)}, nil
	}
	if hashBody(got) != k.hash {
		if f.partial[k] != nil {
			delete(f.partial, k)
			f.queue = append(f.queue, partPath(k))
			f.flush()
		}
		return FetchResult{Got: len(part)}, ErrBadFile
	}
	if !m.commitBlob(k.hash, got, k.msg) {
		return FetchResult{Got: len(part)}, nil // crashed before the row: lost, as on a real crash
	}
	if isManifest {
		m.dropFiles(w.Now) // the list may no longer list what was kept while it was pending
	}
	f.flush()
	return FetchResult{Got: len(part), Complete: true}, nil
}

// PutLocalFile puts a file on machine id's disk, outside a2p2.
func (w *World) PutLocalFile(id MachineID, path, data string) {
	if w.localFiles == nil {
		w.localFiles = map[MachineID]map[string]string{}
	}
	if w.localFiles[id] == nil {
		w.localFiles[id] = map[string]string{}
	}
	w.localFiles[id][path] = data
}

// proposal is a send with files waiting for a person (F6).
type proposal struct {
	machine MachineID
	agent   string
	msg     Msg
	replace RecordID
	files   []FileRef
	state   int // 0 waiting, 1 approved, 2 declined, 3 failed
}

// SendFiles asks to send msg with the local files at paths. The files are
// copied into staging now; nothing is written until a person decides.
func (w *World) SendFiles(id MachineID, agent string, msg Msg, paths ...string) (int, error) {
	return w.propose(id, agent, msg, "", paths)
}

// ReplaceWithFiles asks to replace one of this machine's messages with new
// text and files. A path "sha256:<hash>" names a file the version being
// replaced lists, kept by listing it again (F8).
func (w *World) ReplaceWithFiles(id MachineID, agent string, ref RecordID, text string, paths ...string) (int, error) {
	return w.propose(id, agent, Msg{Body: text}, ref, paths)
}

func (w *World) propose(id MachineID, agent string, msg Msg, ref RecordID, paths []string) (int, error) {
	m, err := w.awake(id)
	if err != nil {
		return 0, err
	}
	if msg.Body == "" || len(paths) == 0 || len(paths) > MaxFiles {
		return 0, ErrNotAllow
	}
	p := &proposal{machine: id, agent: agent, msg: msg, replace: ref}
	st := &stagedSend{at: m.clock(w.Now), data: map[string]string{}}
	total := 0
	for _, path := range paths {
		var data, name string
		if h, ok := strings.CutPrefix(path, "sha256:"); ok {
			t := m.get(ref)
			if ref == "" || t == nil {
				return 0, ErrNotHeld
			}
			_, fr, ok := m.listed(ref, h, w.Now)
			x := m.st.files.held[h]
			if !ok || x == nil || !x.serve[ref] || h == m.winner(t).rec.Manifest {
				return 0, ErrNotHeld
			}
			data, name = x.data, fr.Name
		} else {
			d, ok := w.localFiles[id][path] // opened once; these bytes are what is staged
			if !ok || d == "" {
				return 0, ErrNotHeld
			}
			data, name = d, path
		}
		if len(data) > MaxFileSize {
			return 0, ErrTooLarge
		}
		h := hashBody(data)
		if _, dup := st.data[h]; dup {
			return 0, ErrNotAllow
		}
		p.files = append(p.files, FileRef{Name: name, Size: len(data), Hash: h})
		st.data[h] = data
		total += len(data)
	}
	if !m.st.files.fits(total) {
		return 0, ErrNoSpace
	}
	n := len(w.proposals)
	w.proposals = append(w.proposals, p)
	m.st.files.staged[n] = st
	for h := range st.data {
		m.st.files.disk[stagePath(n, h)] = true
	}
	return n, nil
}

// Decide is the person's decision on machine id about request n.
// Approved, the staged bytes are checked against their digests again and
// the message is written with its manifest; the machine holds the
// manifest and the files for it. Declined, expired, voided or failed:
// nothing is written and staging is deleted.
func (w *World) Decide(id MachineID, n int, approve bool) (RecordID, error) {
	if n < 0 || n >= len(w.proposals) || w.proposals[n].machine != id || w.proposals[n].state != 0 {
		return "", ErrNoProposal
	}
	p := w.proposals[n]
	m, err := w.awake(id)
	if err != nil {
		return "", err
	}
	f := m.st.files
	st := f.staged[n]
	if st == nil { // expired, or the machine's admission was voided
		p.state = 2
		return "", ErrNoProposal
	}
	fail := func(state int, e error) (RecordID, error) {
		p.state = state
		m.dropStaged(n)
		f.flush()
		return "", e
	}
	if !approve {
		return fail(2, nil)
	}
	for _, fr := range p.files {
		if hashBody(st.data[fr.Hash]) != fr.Hash {
			return fail(3, ErrBadFile)
		}
	}
	w.nextSalt++
	man := encodeManifest(fmt.Sprintf("salt-%d", w.nextSalt), p.files)
	var rid, msg RecordID
	if p.replace != "" {
		rid, err = w.amend(id, p.agent, p.replace, Replace, p.msg.Body, hashBody(man))
		msg = p.replace
	} else {
		rid, err = w.send(id, p.agent, p.msg, hashBody(man))
		msg = rid
	}
	if err != nil {
		return fail(3, err)
	}
	w.anyFiles = true
	p.state = 1
	m.commitBlob(hashBody(man), man, msg)
	for _, fr := range p.files {
		m.commitBlob(fr.Hash, st.data[fr.Hash], msg)
	}
	m.dropStaged(n)
	m.dropFiles(w.Now) // flushes; a replacement may have dropped an earlier file
	return rid, nil
}

// Waiting reports whether request n still waits for a person.
func (w *World) Waiting(n int) bool {
	return n >= 0 && n < len(w.proposals) && w.proposals[n].state == 0 &&
		w.machines[w.proposals[n].machine].st.files.staged[n] != nil
}

// Staged reports whether machine id holds staged bytes, in rows or on disk.
func (w *World) Staged(id MachineID) bool {
	f := w.machines[id].st.files
	if len(f.staged) > 0 {
		return true
	}
	for p := range f.disk {
		if strings.HasPrefix(p, "stage/") {
			return true
		}
	}
	return false
}

// FetchOpt shapes one fetch request.
type FetchOpt struct {
	Max int // at most this many bytes in this response (0: the rest)
}

// FetchResult says what one fetch did.
type FetchResult struct {
	Got      int  // bytes received in this request
	Complete bool // the whole file is here and verified
	Local    bool // already held here for another message: nothing transferred
}

// shownVersion says whether agent g was shown version v's text.
func shownVersion(g *agentState, msg RecordID, v *held) bool {
	return g.shownVer[v.rec.ID] || g.seenMsg[msg] == v.rec.ID
}

// Fetch is agent on machine id asking server for file hash of message
// msg. The agent must have been shown the version that lists the file
// (F4); a complete file is copied for the agent after permission is
// checked again (F3).
func (w *World) Fetch(id MachineID, agent string, msg RecordID, hash string, server MachineID, opt FetchOpt) (FetchResult, error) {
	m, err := w.awake(id)
	if err != nil {
		return FetchResult{}, err
	}
	g := m.st.agents[agent]
	v, _, ok := m.listed(msg, hash, w.Now)
	if g == nil || v == nil || !shownVersion(g, msg, v) {
		return FetchResult{}, ErrNotAllow
	}
	if !ok || hash == v.rec.Manifest {
		return FetchResult{}, ErrFileGone
	}
	r, err := w.fetch(m, msg, hash, server, opt)
	if err != nil || !r.Complete {
		return r, err
	}
	v, _, ok = m.listed(msg, hash, w.Now)
	x := m.st.files.held[hash]
	if !ok || x == nil || !shownVersion(g, msg, v) {
		return r, ErrFileGone
	}
	k := ckey{agent, msg, hash}
	if _, done := m.st.files.copies[k]; !done {
		m.st.files.unqueue(copyPath(k))
		m.st.files.disk[copyPath(k)] = true
		m.st.files.copies[k] = x.data
	}
	return r, nil
}

func (w *World) fetch(m *Machine, msg RecordID, hash string, server MachineID, opt FetchOpt) (FetchResult, error) {
	_, ref, ok := m.listed(msg, hash, w.Now)
	if !ok {
		return FetchResult{}, ErrFileGone
	}
	f := m.st.files
	k := pkey{msg, hash}
	isManifest := m.isManifestKey(k)
	if !isManifest && (ref.Size > MaxFileSize || ref.Size < 1) {
		return FetchResult{}, ErrTooLarge // refused before anything is scheduled
	}
	if x := f.held[hash]; x != nil {
		if !x.serve[msg] {
			x.local[msg] = true
		}
		return FetchResult{Complete: true, Local: true}, nil
	}
	if !isManifest && f.partial[k] == nil && !f.fits(ref.Size) {
		return FetchResult{}, ErrNoSpace
	}
	s, err := w.awake(server)
	if err != nil {
		return FetchResult{}, err
	}
	if server == m.ID || !m.st.view.now[server] {
		return FetchResult{}, ErrNotAllow
	}
	if s.NoInbound {
		if !m.NoInbound {
			f.want[k] = true // the server uploads when it next pulls from here (F9)
		}
		return FetchResult{}, ErrNoDial
	}
	data, ok := s.mayServe(m.ID, msg, hash, w.Now)
	if !ok {
		if s.carry(m.ID, msg, hash, w.Now) {
			m.observeFile(k, server, AnsNotYet, w.Now)
			return FetchResult{}, ErrNotYet
		}
		m.observeFile(k, server, AnsNo, w.Now)
		return FetchResult{}, ErrNotHeldFile
	}
	if w.tamperFile != nil {
		data = w.tamperFile(server, hash, data)
	}
	return w.receive(m, s, k, ref, data, opt.Max, false)
}

// upload sends from from to to what to wants for k, over
// from's own connection: F2 on the sender, F3 and "asked for" on the
// receiver.
func (w *World) upload(from, to *Machine, k pkey) {
	if !to.st.files.want[k] || !to.st.view.now[from.ID] {
		return
	}
	data, ok := from.mayServe(to.ID, k.msg, k.hash, w.Now)
	if !ok {
		return
	}
	_, ref, ok := to.listed(k.msg, k.hash, w.Now)
	isManifest := to.isManifestKey(k)
	if !ok || !isManifest && (ref.Size > MaxFileSize || ref.Size < 1 || to.st.files.partial[k] == nil && !to.st.files.fits(ref.Size)) {
		return
	}
	if w.tamperFile != nil {
		data = w.tamperFile(from.ID, k.hash, data)
	}
	w.receive(to, from, k, ref, data, 0, true)
}

// wants is what machine m asks for: what it wants, the manifests of the
// versions it may read, and, when it keeps, every file it may keep.
func (m *Machine) wants(now Time) []pkey {
	set := map[pkey]bool{}
	for k := range m.st.files.want {
		set[k] = true
	}
	for id, h := range m.st.recs {
		if h.rec.Kind != Message || h.dropped == Withdrawn {
			continue
		}
		v := m.winner(h)
		if v.rec.Manifest == "" || !m.keepable(v, now) {
			continue
		}
		files, known := m.manifestOf(v)
		if !known {
			set[pkey{id, v.rec.Manifest}] = true
			continue
		}
		if m.st.files.mode == Keep {
			for _, f := range files {
				if x := m.st.files.held[f.Hash]; x == nil && f.Size <= MaxFileSize && f.Size > 0 {
					set[pkey{id, f.Hash}] = true
				}
			}
		}
	}
	var out []pkey
	for k := range set {
		out = append(out, k)
	}
	// manifests before files, then by message and hash
	sort.Slice(out, func(i, j int) bool {
		mi, mj := m.isManifestKey(out[i]), m.isManifestKey(out[j])
		if mi != mj {
			return mi
		}
		if out[i].msg != out[j].msg {
			return out[i].msg < out[j].msg
		}
		return out[i].hash < out[j].hash
	})
	return out
}

func (m *Machine) isManifestKey(k pkey) bool {
	h := m.get(k.msg)
	return h != nil && m.winner(h).rec.Manifest == k.hash
}

// exchangeFiles runs after every pull from p to s that delivered its
// answer: p fetches over its connection what it wants and s holds, and s's
// answer names what s wants that p wrote or is known to hold, which p
// uploads (F9). Manifests go first, so a file can be checked against its
// list.
func (w *World) exchangeFiles(p, s *Machine) {
	if !w.anyFiles {
		return
	}
	for pass := 0; pass < 2; pass++ { // the second pass asks for files of lists the first one got
		for _, k := range p.wants(w.Now) {
			if _, err := w.fetch(p, k.msg, k.hash, s.ID, FetchOpt{}); err == nil {
				if x := p.st.files.held[k.hash]; x != nil {
					delete(p.st.files.want, k)
				}
			}
		}
		for _, k := range s.wants(w.Now) {
			h := p.get(k.msg)
			wrote := h != nil && h.rec.Author == p.ID
			if !wrote && s.st.files.obs[k][p.ID].Answer != AnsHas {
				continue
			}
			s.st.files.want[k] = true
			w.upload(p, s, k)
			if x := s.st.files.held[k.hash]; x != nil && x.serve[k.msg] {
				delete(s.st.files.want, k)
			}
		}
	}
	p.st.files.flush()
	s.st.files.flush()
}

// SetFileBudget sets machine id's budget for file bytes (0: none).
func (w *World) SetFileBudget(id MachineID, bytes int) { w.machines[id].st.files.budget = bytes }

// SetFileMode sets machine id's file mode: Carry (default), FilesOff or Keep.
func (w *World) SetFileMode(id MachineID, mode FileMode) { w.machines[id].st.files.mode = mode }

// FailFiles makes machine id crash at its next point (test hook): "rename"
// after a file is written and before its row, "commit" after rows are
// removed and before the files are deleted.
func (w *World) FailFiles(id MachineID, point string) { w.machines[id].st.files.fail = point }

// FileState is what a reader is told about one file of one message (F5).
type FileState uint8

const (
	FileGone        FileState = iota + 1 // no version this machine may keep lists it: see Why
	FileListPending                      // the version has files, and their list has not arrived
	FileHere                             // verified bytes are here
	FilePartial                          // part of it is here
	FileComing                           // a member said "not yet", or this machine asked a peer it cannot dial
	FileNotAsked                         // nobody has been asked yet
	FileFetchable                        // a current member answered that it held it
	FileUnavailable                      // every member asked said not held
)

func (s FileState) String() string {
	return [...]string{"?", "gone", "list-pending", "here", "partial", "coming", "not-asked", "fetchable", "unavailable"}[s]
}

// FileInfo is FileStatus's answer.
type FileInfo struct {
	State   FileState
	Why     Reason    // for FileGone: the text's reason, or Live when a replacement dropped the file
	Holders []FileObs // dated answers for this message, by machine
}

// FileStatus reports what machine id can tell a reader about file hash of
// message msg.
func (w *World) FileStatus(id MachineID, msg RecordID, hash string) FileInfo {
	m := w.machines[id]
	m.prune(w.Now)
	var info FileInfo
	k := pkey{msg, hash}
	var peers []MachineID
	for p := range m.st.files.obs[k] {
		peers = append(peers, p)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i] < peers[j] })
	for _, p := range peers {
		info.Holders = append(info.Holders, m.st.files.obs[k][p])
	}
	v, _, ok := m.referenced(msg, hash, w.Now)
	switch {
	case !ok:
		info.State = FileGone
		h := m.get(msg)
		switch {
		case h == nil:
			info.Why = Missing
		case h.dropped == Withdrawn:
			info.Why = Withdrawn
		case !m.keepable(v, w.Now):
			info.Why = m.textGone(v.rec, w.Now)
			if info.Why == Live {
				info.Why = v.dropped
			}
		default:
			if _, known := m.manifestOf(v); !known {
				info.State = FileListPending
			} else {
				info.Why = Live // a replacement does not list it
			}
		}
	case m.st.files.held[hash] != nil:
		info.State = FileHere
	case m.st.files.partial[k] != nil:
		info.State = FilePartial
	case m.st.files.want[k]:
		info.State = FileComing
	default:
		info.State = FileNotAsked
		for _, o := range info.Holders {
			if !m.st.view.now[o.Machine] {
				continue
			}
			switch {
			case o.Answer == AnsNotYet:
				info.State = FileComing
			case o.Answer == AnsHas && info.State != FileComing:
				info.State = FileFetchable
			case info.State == FileNotAsked:
				info.State = FileUnavailable
			}
		}
	}
	return info
}

// FileBytes returns the verified bytes of hash on machine id, if held.
func (w *World) FileBytes(id MachineID, hash string) (string, bool) {
	x := w.machines[id].st.files.held[hash]
	if x == nil {
		return "", false
	}
	return x.data, true
}

// HoldsAnyOf reports whether machine id holds anything of hash: bytes, a
// part, an agent's copy, a staged copy, or a file on disk.
func (w *World) HoldsAnyOf(id MachineID, hash string) bool {
	f := w.machines[id].st.files
	if f.held[hash] != nil {
		return true
	}
	for k := range f.partial {
		if k.hash == hash {
			return true
		}
	}
	for k := range f.copies {
		if k.hash == hash {
			return true
		}
	}
	for _, s := range f.staged {
		if _, ok := s.data[hash]; ok {
			return true
		}
	}
	for p := range f.disk {
		if strings.HasSuffix(p, "/"+hash) {
			return true
		}
	}
	return false
}

// CopyFor reports whether agent on machine id has a copy of hash for msg.
func (w *World) CopyFor(id MachineID, agent string, msg RecordID, hash string) bool {
	_, ok := w.machines[id].st.files.copies[ckey{agent, msg, hash}]
	return ok
}

// ManifestOf returns machine id's file list for message msg's winning
// version, and whether it holds it.
func (w *World) ManifestOf(id MachineID, msg RecordID) ([]FileRef, bool) {
	m := w.machines[id]
	h := m.get(msg)
	if h == nil {
		return nil, false
	}
	return m.manifestOf(m.winner(h))
}

// TamperFiles makes servers pass bytes through fn: a member that serves
// wrong bytes (test hook).
func (w *World) TamperFiles(fn func(server MachineID, hash, data string) string) { w.tamperFile = fn }
