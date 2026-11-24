package delivery

import (
	"errors"
	"sort"
)

// Attachments (requirement L1; 0.2 draft, not built). A message or a
// replacement may refer to files by name, size and hash in its signed
// header (Record.Files). The bytes are not part of the record: they live
// beside the store, are fetched on demand from a member that holds them
// over the pull connection, and are verified against the hash before
// anyone sees them. FINDINGS.md, "Attachments", states the rules F1 to F8
// in plain words; this file is them as code.
//
// The rules, in short:
//
//   - F1 Referenced. A machine keeps a file's bytes for a message only
//     while the message's winning version (the highest version held by
//     header, text or not) lists the file and that version's text is one
//     the machine may keep: not withdrawn, not expired by its own clock,
//     within its permission. Otherwise the bytes and any partial download
//     go at once, in the step that stored the withdraw or replacement,
//     learned the narrower permission or the voided admission, or passed
//     the 30 days.
//   - F2 Served. A server gives bytes for a message only to a peer it sees
//     as a member now, which may read the text of the winning version in
//     the server's view, and only bytes it holds for that message: it wrote
//     the message, or fetched them for it. Checked at every request, so a
//     resumed download is checked again.
//   - F3 Verified. A fetcher takes no more than the declared size, keeps a
//     partial download hidden, and exposes the bytes only when the whole
//     matches the hash; otherwise it discards what it got.
//   - F4 Lazy. Nothing is fetched unless an agent that was shown the
//     message asks (or the person set the machine to keep a room's files
//     for others, Replicate); there is no push.
//   - F5 Told. A reader is told whether a file is here, being fetched,
//     not yet asked for, unavailable from every member asked, or gone and
//     why; holders are known only from dated answers to fetches.
//   - F6 Confirmed. A local file is sent only after a person approves the
//     exact bytes staged when the agent asked; the message is written with
//     its files on approval and never without them.
//   - F7 Budget. A fetch that would take the room's held bytes over the
//     machine's budget is refused before anything is transferred.
//   - F8 Replace. A replacement carries the previous version's files
//     forward unless its writer drops them; a file it no longer lists goes.

// FileRef is one file a record refers to, in its signed header.
type FileRef struct {
	Name string
	Size int
	Hash string // SHA-256 of the bytes, hex
}

// fileHeld is the bytes of one file on one machine, and the messages it
// holds them for. for: messages it may serve them for (it wrote the
// message, or fetched the bytes for it). local: messages whose readers
// here use bytes it already held for another message; never served for
// those, so a new reference to a known hash cannot widen who is served.
type fileHeld struct {
	data  string
	serve map[RecordID]bool
	local map[RecordID]bool
}

// FileObs is a dated answer from one peer about one file: it held the
// bytes for the message (and served them), or it did not.
type FileObs struct {
	Machine MachineID
	At      Time
	Has     bool
}

// fileState is the attachment part of a machine's store: durable, and
// restored with a backup like the rest.
type fileState struct {
	held    map[string]*fileHeld             // by hash
	partial map[string]string                // by hash: bytes received so far, never shown
	obs     map[string]map[MachineID]FileObs // by hash
	budget  int                              // 0: no limit
	keep    bool                             // fetch every file for others (Replicate)
}

func newFileState() *fileState {
	return &fileState{held: map[string]*fileHeld{}, partial: map[string]string{}, obs: map[string]map[MachineID]FileObs{}}
}

func (f *fileState) clone() *fileState {
	c := newFileState()
	c.budget, c.keep = f.budget, f.keep
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
	for h, p := range f.partial {
		c.partial[h] = p
	}
	for h, m := range f.obs {
		n := map[MachineID]FileObs{}
		for k, v := range m {
			n[k] = v
		}
		c.obs[h] = n
	}
	return c
}

func (f *fileState) empty() bool { return len(f.held) == 0 && len(f.partial) == 0 }

// used is the bytes held and partly fetched.
func (f *fileState) used() int {
	n := 0
	for _, x := range f.held {
		n += len(x.data)
	}
	for _, p := range f.partial {
		n += len(p)
	}
	return n
}

// Errors of the attachment operations.
var (
	ErrFileGone    = errors.New("no version of the message this machine may keep refers to the file")
	ErrNotHeldFile = errors.New("the server does not hold the file for that message, or may not serve it to this machine")
	ErrBadFile     = errors.New("the bytes received do not match the reference")
	ErrNoSpace     = errors.New("the room's file budget on this machine would be exceeded")
	ErrNoProposal  = errors.New("no such request waiting for a person")
)

// winner is the version of message h whose text wins by header: the
// message, or its author's replacement with the highest version, then the
// highest id, text held or not (inbox.go's newer). A withdrawn replacement
// does not count.
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

// filesOf is the files record id lists, as an inbox entry shows them.
func (m *Machine) filesOf(id RecordID) []FileRef {
	if h := m.get(id); h != nil && len(h.rec.Files) > 0 {
		return append([]FileRef(nil), h.rec.Files...)
	}
	return nil
}

func refers(r *Record, hash string) bool {
	for _, f := range r.Files {
		if f.Hash == hash {
			return true
		}
	}
	return false
}

func fileRef(r *Record, hash string) (FileRef, bool) {
	for _, f := range r.Files {
		if f.Hash == hash {
			return f, true
		}
	}
	return FileRef{}, false
}

// keepable says whether the text of a held record is one this machine may
// keep now, by its own evidence and clock (F1): what decides whether the
// files it lists may be kept.
func (m *Machine) keepable(h *held, now Time) bool {
	return (h.dropped == Live || h.dropped == Missing) && m.textGone(h.rec, now) == Live
}

// referenced says whether this machine may keep the bytes of hash for
// message msg (F1), and returns the winning version.
func (m *Machine) referenced(msg RecordID, hash string, now Time) (*held, bool) {
	h := m.get(msg)
	if h == nil || h.rec.Kind != Message || h.dropped == Withdrawn {
		return nil, false
	}
	v := m.winner(h)
	return v, refers(v.rec, hash) && m.keepable(v, now)
}

// referencedAny says whether some message held here lets this machine keep
// hash.
func (m *Machine) referencedAny(hash string, now Time) bool {
	for id, h := range m.st.recs {
		if h.rec.Kind == Message {
			if _, ok := m.referenced(id, hash, now); ok {
				return true
			}
		}
	}
	return false
}

// dropFiles applies F1: bytes go when no message keeps them, and a
// message stops counting when its winning version no longer refers to the
// file or its text may not be kept here. It runs whenever a record is
// stored, the clock is checked, the machine's own permission changes or
// an author is voided.
func (m *Machine) dropFiles(now Time) {
	f := m.st.files
	if f.empty() {
		return
	}
	for hash, x := range f.held {
		for _, set := range []map[RecordID]bool{x.serve, x.local} {
			for msg := range set {
				if _, ok := m.referenced(msg, hash, now); !ok {
					delete(set, msg)
				}
			}
		}
		if len(x.serve) == 0 && len(x.local) == 0 {
			delete(f.held, hash)
		}
	}
	for hash := range f.partial {
		if !m.referencedAny(hash, now) {
			delete(f.partial, hash)
		}
	}
}

// mayServe is F2, on server s, for a request from peer p.
func (s *Machine) mayServe(p MachineID, msg RecordID, hash string, now Time) (string, bool) {
	if !s.st.view.now[p] {
		return "", false
	}
	x := s.st.files.held[hash]
	if x == nil || !x.serve[msg] {
		return "", false
	}
	v, ok := s.referenced(msg, hash, now)
	if !ok || !mayRead(s.st.view.adm[p], v.rec) {
		return "", false
	}
	return x.data, true
}

// PutLocalFile puts a file on machine id's disk, outside a2p2: what an
// agent may ask to send.
func (w *World) PutLocalFile(id MachineID, path, data string) {
	if w.localFiles == nil {
		w.localFiles = map[MachineID]map[string]string{}
	}
	if w.localFiles[id] == nil {
		w.localFiles[id] = map[string]string{}
	}
	w.localFiles[id][path] = data
}

// proposal is a message with local files waiting for a person (F6). The
// bytes were copied and hashed when the agent asked; that copy is what
// the person approves and what is sent.
type proposal struct {
	machine MachineID
	agent   string
	msg     Msg
	replace RecordID // a replacement, instead of a new message
	files   []FileRef
	data    map[string]string // by hash
	state   int               // 0 waiting, 1 approved, 2 declined
}

// SendFiles asks to send msg with the local files at paths. Nothing is
// written: the request waits for a person (Decide). It returns the
// request's number.
func (w *World) SendFiles(id MachineID, agent string, msg Msg, paths ...string) (int, error) {
	return w.propose(id, agent, msg, "", paths)
}

// ReplaceWithFiles asks to replace one of this machine's messages with new
// text and the local files at paths; like SendFiles, nothing is written
// until a person decides.
func (w *World) ReplaceWithFiles(id MachineID, agent string, ref RecordID, text string, paths ...string) (int, error) {
	return w.propose(id, agent, Msg{Body: text}, ref, paths)
}

func (w *World) propose(id MachineID, agent string, msg Msg, ref RecordID, paths []string) (int, error) {
	if _, err := w.awake(id); err != nil {
		return 0, err
	}
	if msg.Body == "" || len(paths) == 0 {
		return 0, ErrNotAllow
	}
	p := &proposal{machine: id, agent: agent, msg: msg, replace: ref, data: map[string]string{}}
	for _, path := range paths {
		data, ok := w.localFiles[id][path]
		if !ok || data == "" {
			return 0, ErrNotHeld
		}
		h := hashBody(data)
		if _, dup := p.data[h]; dup {
			return 0, ErrNotAllow // a file once per record
		}
		p.files = append(p.files, FileRef{Name: path, Size: len(data), Hash: h})
		p.data[h] = data
	}
	w.proposals = append(w.proposals, p)
	return len(w.proposals) - 1, nil
}

// Decide is the person's decision on machine id about request n. Approved,
// the message (or replacement) is written with its files and the machine
// holds their bytes for it; declined, nothing is ever written and the
// staged copies go. A decision is made once.
func (w *World) Decide(id MachineID, n int, approve bool) (RecordID, error) {
	if n < 0 || n >= len(w.proposals) || w.proposals[n].machine != id || w.proposals[n].state != 0 {
		return "", ErrNoProposal
	}
	p := w.proposals[n]
	m, err := w.awake(id)
	if err != nil {
		return "", err
	}
	if !approve {
		p.state, p.data = 2, nil
		return "", nil
	}
	var rid, msg RecordID
	if p.replace != "" {
		rid, err = w.amendFiles(id, p.agent, p.replace, p.msg.Body, p.files)
		msg = p.replace
	} else {
		rid, err = w.send(id, p.agent, p.msg, p.files)
		msg = rid
	}
	if err != nil {
		return "", err
	}
	p.state = 1
	for _, f := range p.files {
		x := m.st.files.held[f.Hash]
		if x == nil {
			x = &fileHeld{data: p.data[f.Hash], serve: map[RecordID]bool{}, local: map[RecordID]bool{}}
			m.st.files.held[f.Hash] = x
		}
		x.serve[msg] = true
	}
	p.data = nil
	m.dropFiles(w.Now) // a replacement may have dropped an earlier file
	return rid, nil
}

// Waiting reports whether request n still waits for a person.
func (w *World) Waiting(n int) bool {
	return n >= 0 && n < len(w.proposals) && w.proposals[n].state == 0
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

// Fetch is agent on machine id asking server for the bytes of hash, a file
// of message msg. The agent must have been shown the message (F4); the
// machine must be allowed to keep the file (F1); the server serves by F2;
// what arrives is checked by F3; the budget by F7. A partial download
// continues from where it stopped, from any server.
func (w *World) Fetch(id MachineID, agent string, msg RecordID, hash string, server MachineID, opt FetchOpt) (FetchResult, error) {
	m, err := w.awake(id)
	if err != nil {
		return FetchResult{}, err
	}
	g := m.st.agents[agent]
	if g == nil || (!g.shown[msg] && !g.seen[string(msg)]) {
		return FetchResult{}, ErrNotAllow
	}
	return w.fetch(m, msg, hash, server, opt)
}

func (w *World) fetch(m *Machine, msg RecordID, hash string, server MachineID, opt FetchOpt) (FetchResult, error) {
	v, ok := m.referenced(msg, hash, w.Now)
	if !ok {
		return FetchResult{}, ErrFileGone
	}
	ref, _ := fileRef(v.rec, hash)
	f := m.st.files
	if x := f.held[hash]; x != nil {
		if !x.serve[msg] {
			x.local[msg] = true // usable here; never served for msg
		}
		return FetchResult{Complete: true, Local: true}, nil
	}
	have := f.partial[hash]
	if f.budget > 0 && f.used()-len(have)+ref.Size > f.budget {
		return FetchResult{}, ErrNoSpace
	}
	s, err := w.awake(server)
	if err != nil {
		return FetchResult{}, err
	}
	if server == m.ID || s.NoInbound || !m.st.view.now[server] {
		return FetchResult{}, ErrNotAllow
	}
	data, ok := s.mayServe(m.ID, msg, hash, w.Now)
	if !ok {
		m.observeFile(hash, server, false, w.Now)
		return FetchResult{}, ErrNotHeldFile
	}
	if w.tamperFile != nil {
		data = w.tamperFile(server, hash, data)
	}
	// The server sends from the requested offset, at most Max bytes; the
	// requester never takes more than the declared size.
	part := ""
	if len(have) < len(data) {
		part = data[len(have):]
	}
	if opt.Max > 0 && len(part) > opt.Max {
		part = part[:opt.Max]
	}
	got := have + part
	m.observeFile(hash, server, true, w.Now)
	if len(got) > ref.Size || (len(got) == ref.Size && hashBody(got) != hash) || (len(part) == 0 && len(got) < ref.Size) {
		delete(f.partial, hash)
		return FetchResult{Got: len(part)}, ErrBadFile
	}
	if len(got) < ref.Size {
		f.partial[hash] = got
		return FetchResult{Got: len(part)}, nil
	}
	delete(f.partial, hash)
	f.held[hash] = &fileHeld{data: got, serve: map[RecordID]bool{msg: true}, local: map[RecordID]bool{}}
	return FetchResult{Got: len(part), Complete: true}, nil
}

func (m *Machine) observeFile(hash string, peer MachineID, has bool, now Time) {
	o := m.st.files.obs[hash]
	if o == nil {
		o = map[MachineID]FileObs{}
		m.st.files.obs[hash] = o
	}
	o[peer] = FileObs{Machine: peer, At: now, Has: has}
}

// SetFileBudget sets machine id's budget for held file bytes (0: none).
func (w *World) SetFileBudget(id MachineID, bytes int) { w.machines[id].st.files.budget = bytes }

// SetKeep sets whether machine id keeps the room's files for others: a
// person's choice, so that a file outlives its author's machine (F4).
func (w *World) SetKeep(id MachineID, on bool) { w.machines[id].st.files.keep = on }

// Replicate is what a keeping machine's scheduler does after a pull from
// server: fetch, for every message held here, every file it may keep and
// lacks, from server. Machines not set to keep do nothing. It returns how
// many files completed.
func (w *World) Replicate(id, server MachineID) (int, error) {
	m, err := w.awake(id)
	if err != nil {
		return 0, err
	}
	if !m.st.files.keep {
		return 0, nil
	}
	var msgs []RecordID
	for rid, h := range m.st.recs {
		if h.rec.Kind == Message {
			msgs = append(msgs, rid)
		}
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i] < msgs[j] })
	n := 0
	for _, msg := range msgs {
		v, _ := m.referenced(msg, "", w.Now)
		if v == nil {
			continue
		}
		for _, f := range v.rec.Files {
			if x := m.st.files.held[f.Hash]; x != nil && x.serve[msg] {
				continue
			}
			if m.st.files.held[f.Hash] != nil {
				continue // held for another message; a keeper serves only what it fetched for
			}
			if r, err := w.fetch(m, msg, f.Hash, server, FetchOpt{}); err == nil && r.Complete {
				n++
			}
		}
	}
	return n, nil
}

// FileState is what a reader is told about one file of one message (F5).
type FileState uint8

const (
	FileGone        FileState = iota + 1 // no version this machine may keep refers to it: see Why
	FileHere                             // verified bytes are here
	FilePartial                          // part of it is here; fetching again continues
	FileNotAsked                         // nobody has been asked yet
	FileFetchable                        // a member answered that it held it
	FileUnavailable                      // every member asked answered that it did not, or is no longer a member
)

func (s FileState) String() string {
	return [...]string{"?", "gone", "here", "partial", "not-asked", "fetchable", "unavailable"}[s]
}

// FileInfo is FileStatus's answer.
type FileInfo struct {
	State   FileState
	Why     Reason    // for FileGone: the text's reason, or Live when a replacement dropped the file
	Holders []FileObs // dated answers, by machine
}

// FileStatus reports what machine id can tell a reader about file hash of
// message msg, and who answered when asked (for trace).
func (w *World) FileStatus(id MachineID, msg RecordID, hash string) FileInfo {
	m := w.machines[id]
	m.prune(w.Now)
	var info FileInfo
	var peers []MachineID
	for p := range m.st.files.obs[hash] {
		peers = append(peers, p)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i] < peers[j] })
	for _, p := range peers {
		info.Holders = append(info.Holders, m.st.files.obs[hash][p])
	}
	v, ok := m.referenced(msg, hash, w.Now)
	switch {
	case !ok:
		info.State = FileGone
		if h := m.get(msg); h != nil {
			switch {
			case h.dropped == Withdrawn:
				info.Why = Withdrawn
			case v != nil && !refers(v.rec, hash):
				info.Why = Live // a replacement no longer lists it
			case v != nil:
				info.Why = m.textGone(v.rec, w.Now)
				if info.Why == Live {
					info.Why = v.dropped
				}
			}
		} else {
			info.Why = Missing
		}
	case m.st.files.held[hash] != nil:
		info.State = FileHere
	case m.st.files.partial[hash] != "":
		info.State = FilePartial
	default:
		info.State = FileNotAsked
		any := false
		for _, o := range info.Holders {
			any = true
			if o.Has && m.st.view.now[o.Machine] {
				info.State = FileFetchable
				break
			}
		}
		if any && info.State == FileNotAsked {
			info.State = FileUnavailable
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

// HoldsAnyOf reports whether machine id holds bytes, complete or partial,
// of hash.
func (w *World) HoldsAnyOf(id MachineID, hash string) bool {
	f := w.machines[id].st.files
	return f.held[hash] != nil || f.partial[hash] != ""
}

// TamperFiles makes every server pass bytes through fn: a member that
// serves wrong bytes (test hook).
func (w *World) TamperFiles(fn func(server MachineID, hash, data string) string) { w.tamperFile = fn }
