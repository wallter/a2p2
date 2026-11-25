package node

// The requester's side of attachments: wants in pull answers and their
// uploads (PROTOCOL 10.4, 10.5), downloads from holders (10.3), the
// automatic manifest fetch with bounded retries [F12], carry jobs and the
// keep setting [F9], and an agent's fetch (10.6).

import (
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/members"
	"github.com/wallter/a2p2/internal/store"
)

// PullWant is one entry of a pull answer's want (PROTOCOL 10.4).
type PullWant struct {
	M string `json:"m"`
	V string `json:"v"`
	H string `json:"h"`
}

// wantBytes bounds the wire size of one want entry.
const wantBytes = 32 + 32 + 64 + 24

// wantsFor is the want field of an answer to puller's pull of room: only
// when this machine cannot dial puller, only wants of puller's messages
// that are still for the winning version, within room bytes left [F9].
func (f *Files) wantsFor(ctx context.Context, room, puller string, room0 int) []PullWant {
	if f.t == nil || f.t.CanDial(puller) {
		return nil
	}
	max := min(core.MaxWants, room0/wantBytes)
	if max < 1 {
		return nil
	}
	// the clock read inside the transaction that chooses the wants [W4]
	clock := func() time.Time { return f.m.now() }
	_ = hookAt("files:wants") // tests: time passes while the transaction is waited for
	ws, err := f.m.st.WantsToSend(ctx, room, puller, clock, max, core.WantRepeat, f.wantFor(room, puller))
	if err != nil {
		return nil
	}
	out := make([]PullWant, 0, len(ws))
	seen := map[[2]string]bool{}
	for _, w := range ws {
		k := [2]string{w.Msg, w.SHA256}
		if !seen[k] {
			seen[k] = true
			out = append(out, PullWant{M: w.Msg, V: w.Version, H: w.SHA256})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// wantFor is the check that a want may go out in an answer to puller: it
// is still for the winning version of puller's message, which lists it,
// and it is within its life.
func (f *Files) wantFor(room, puller string) func(*store.Reader, store.FileWant, time.Time) (bool, error) {
	return func(rd *store.Reader, w store.FileWant, now time.Time) (bool, error) {
		mf, err := rd.MessageFiles(room, w.Msg, f.m.ID, now)
		if err != nil {
			return false, err
		}
		_, _, listed := mf.Listed(w.SHA256)
		live := now.Before(w.At.Add(core.WantLife)) // past its life it is not advertised, swept or not [S6]
		return live && listed && mf.Winner != nil && mf.Winner.ID == w.Version && mf.Author == puller, nil
	}
}

// wantsDue reports whether an answer to puller's pull of room would carry
// a want now (wantsFor, without recording anything). A pull of puller held
// at this machine ends when one is due, so a want reaches an author that
// only dials out at once, not when the hold times out.
func (f *Files) wantsDue(ctx context.Context, room, puller string) bool {
	if f.t == nil || f.t.CanDial(puller) {
		return false
	}
	var due bool
	err := f.m.st.View(ctx, func(rd *store.Reader) (err error) {
		due, err = rd.WantsDue(room, puller, f.m.now(), core.WantRepeat, f.wantFor(room, puller))
		return err
	})
	return err == nil && due
}

// issueWant records w and, when it is new and its author cannot be
// dialled, wakes the room, so that a pull of the author held here ends and
// carries it (wantsDue).
func (f *Files) issueWant(ctx context.Context, w store.FileWant) (bool, error) {
	issued, err := f.m.st.IssueWant(ctx, w)
	if err == nil && issued && (f.t == nil || !f.t.CanDial(w.Author)) {
		f.m.wakes.touch(w.Room)
	}
	return issued, err
}

// Soon asks Run to do room's file work now: the manifests of headers that
// just arrived are fetched after the pull that brought them, not at the
// next tick (PROTOCOL 10.2). It never blocks.
func (f *Files) Soon(room string) {
	select {
	case f.soon <- room:
	default:
	}
}

// soonAfter is Soon after d.
func (f *Files) soonAfter(room string, d time.Duration) {
	time.AfterFunc(d, func() { f.Soon(room) })
}

// checkWants is the response check of a want field.
func checkWants(ws []PullWant) bool {
	if len(ws) > core.MaxWants {
		return false
	}
	seen := map[[2]string]bool{}
	for _, w := range ws {
		k := [2]string{w.M, w.H}
		if !validRecordID(w.M) || !validRecordID(w.V) || !core.ValidSHA256(w.H) || seen[k] {
			return false
		}
		seen[k] = true
	}
	return true
}

// answerWants uploads, in the background, each entry this machine still
// holds as the author of that message and version, to the peer whose
// answer wanted it; the rest are skipped silently (PROTOCOL 10.4).
func (f *Files) answerWants(room, peer string, ws []PullWant) {
	if len(ws) == 0 {
		return
	}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		for _, w := range ws {
			_ = f.upload(ctx, room, peer, w)
		}
	}()
}

// Wait waits for the uploads and downloads started in the background.
func (f *Files) Wait() { f.wg.Wait() }

// upload sends w's bytes to peer in chunks from 0, each continuing from
// the have of the last answer, under a lease on the decision that peer may
// have them [F2, B2]: every read asks it, and its end stops the upload.
func (f *Files) upload(ctx context.Context, room, peer string, w PullWant) error {
	c, err := f.check(ctx, room, w.M, w.H, peer)
	if err != nil {
		return err
	}
	if !c.servable() || !c.selfAuth || c.mf.Winner.ID != w.V {
		return nil
	}
	release, err := f.tr.take(peer, dirOut)
	if err != nil {
		return err
	}
	defer release()
	l, err := f.grant(ctx, room, f.uploadRule(w.M, w.H, peer, w.V))
	if err != nil {
		return nil
	}
	defer l.end()
	have := int64(0)
	for have < c.size {
		n := min(c.size-have, f.chunk)
		if c.isList {
			n = c.size // a manifest is sent whole
		}
		b, err := f.m.st.OpenBlob(room, w.H)
		if err != nil {
			return err
		}
		if _, err := b.Seek(have, io.SeekStart); err != nil {
			b.Close()
			return err
		}
		status, got, err := f.t.PutFile(l.ctx, peer, FilePut{Room: room, Msg: w.M, SHA256: w.H, Offset: have, Length: n, Total: c.size},
			&leasedReader{r: io.LimitReader(b, n), l: l})
		b.Close()
		if err != nil || status != 200 || got <= have || !l.valid() {
			return err
		}
		have = got
	}
	return nil
}

// ---- downloads ----

// holders orders whom to ask for sha of msg (PROTOCOL 10.3): the author,
// members whose last answer was that they hold or carry it, then other
// current members; never this machine, a peer that cannot be dialled or
// one marked old_version.
//
// K1: a request names the hash in its path, so it goes only to a machine
// that may read the message. Each candidate other than the author must
// pass the serving side's own rule from this machine's view (decide: a
// member with a permission that reads the message and its winning
// version), all decided under one consistent view; a peer this machine
// cannot affirm is never asked.
func (f *Files) holders(ctx context.Context, room, msg, sha, author string) ([]string, error) {
	now := f.m.now()
	f.mu.Lock() // read before the view: f.mu is never taken inside a transaction
	old := maps.Clone(f.old)
	f.mu.Unlock()
	dialable := func(p string) bool {
		if p == f.m.ID || f.t == nil || !f.t.CanDial(p) {
			return false
		}
		return !now.Before(old[p]) || old[p].IsZero()
	}
	var out []string
	err := f.within(ctx, room, now, func(v *roomView, st members.State, _ int64) error {
		obs, err := v.rd.FileObs(room, msg, sha)
		if err != nil {
			return err
		}
		add := func(p string) error {
			if slices.Contains(out, p) || !slices.Contains(st.Members, p) || !dialable(p) {
				return nil
			}
			c, err := f.decide(v, st, msg, sha, p)
			if err != nil {
				return err
			}
			if !askable(c, p, author) {
				return nil
			}
			out = append(out, p)
			return nil
		}
		if err := add(author); err != nil {
			return err
		}
		sort.SliceStable(obs, func(i, j int) bool { return obs[i].At.After(obs[j].At) })
		for _, o := range obs {
			if o.Answer == store.AnswerHas || o.Answer == store.AnswerNotYet {
				if err := add(o.Peer); err != nil {
					return err
				}
			}
		}
		for _, p := range st.Members {
			if err := add(p); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// askable is the rule a fetch's candidates are chosen by [K1]: peer is a
// member with a permission and is the message's author, or may read the
// message and its winning version.
func askable(c fileCheck, peer, author string) bool {
	return c.member && (peer == author || c.mayRead)
}

// fetchRule: this machine may ask peer for sha of msg now and keep what it
// sends: keepRule, and peer passes askable [K1, L1]. A fetch holds a lease
// on it and asks it again before each request, which names the hash.
func (f *Files) fetchRule(msg, sha, peer string) rule {
	return f.ruleOf(msg, sha, peer, func(c fileCheck) bool {
		return c.listed && !c.heldForMsg() && c.mf != nil && askable(c, peer, c.mf.Author)
	}, nil)
}

// download fetches sha for msg from the first of holders that serves it,
// each asked at most once, each from byte 0. It returns who served it.
func (f *Files) download(ctx context.Context, room, msg, sha string, holders []string) (string, error) {
	var last error = errNoHolder
	for _, p := range holders {
		done, err := f.getFrom(ctx, room, msg, sha, p)
		if done {
			return p, nil
		}
		if err != nil {
			last = err
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	return "", last
}

var errNoHolder = errors.New("files: no holder served the file")

// getFrom asks peer for sha of msg from 0, chunk by chunk, while it
// serves; done when the whole is verified and held. One transfer holds the
// writer of (room, msg, sha) and a lease on this machine's permission to
// keep the file from start to end [B2]; one that fails starts again from
// zero (no part outlives its transfer).
func (f *Files) getFrom(ctx context.Context, room, msg, sha, peer string) (bool, error) {
	release, err := f.tr.take(peer, dirIn)
	if err != nil {
		return false, err
	}
	defer release()
	unlock, ok := f.writer(room, msg, sha)
	if !ok {
		return false, errWriting
	}
	defer unlock()
	note := func(a string) { _ = f.m.st.NoteFileObs(ctx, room, msg, sha, peer, a, f.m.now()) }
	c, err := f.check(ctx, room, msg, sha, "")
	if err != nil {
		return false, err
	}
	if !c.listed || c.listBad && c.isList {
		return false, ErrFileGone
	}
	if c.how != "" {
		return true, nil
	}
	if c.hasPart {
		if err := f.m.st.DropPart(ctx, room, msg, sha, f.m.now()); err != nil {
			return false, err
		}
	}
	keep := f.keepRule(msg, sha)
	// The lease is on asking this peer as well as on keeping the file: a
	// peer removed, or no longer permitted, while an earlier candidate or
	// chunk was being asked is not asked (L1).
	l, err := f.grant(ctx, room, f.fetchRule(msg, sha, peer))
	if err != nil {
		if ok, _, _, _ := f.ask(ctx, room, f.m.now(), keep); !ok {
			return false, ErrFileGone
		}
		return false, nil // this peer may not be asked; another may
	}
	defer l.end()
	offset, total, done := int64(0), c.ref.Size, false
	defer func() {
		if !done && offset > 0 {
			_ = f.m.st.DropPart(context.WithoutCancel(ctx), room, msg, sha, f.m.now())
		}
	}()
	for {
		// the rule again immediately before each request that names the hash
		if !l.valid() {
			return false, nil
		}
		rep, err := f.t.GetFile(l.ctx, peer, FileGet{Room: room, Msg: msg, SHA256: sha, Offset: offset})
		if err != nil {
			note(store.AnswerFailed)
			return false, err
		}
		switch rep.Status {
		case 200, 206:
		case 202:
			if rep.State == FileGaveUp {
				note(store.AnswerGaveUp)
			} else {
				note(store.AnswerNotYet)
			}
			return false, nil
		case 404:
			if rep.Old {
				f.mu.Lock()
				f.old[peer] = f.m.now().Add(core.OldVersionLife)
				f.mu.Unlock()
				note(store.AnswerOldVersion)
			} else {
				note(store.AnswerNo)
			}
			return false, nil
		case 416:
			_ = f.m.st.DropPart(ctx, room, msg, sha, f.m.now())
			note(store.AnswerFailed)
			return false, nil
		default:
			note(store.AnswerFailed)
			return false, nil
		}
		if c.isList && offset == 0 {
			total = rep.Total // a manifest's size is known from its first answer
		}
		bad := rep.Total != total || total < 1 || c.isList && total > core.MaxManifestBytes ||
			rep.Offset != offset || rep.Length < 1 || rep.Length > core.FileChunk || offset+rep.Length > total
		if bad {
			rep.Body.Close()
			note(store.AnswerFailed)
			return false, nil
		}
		have, err := f.receive(peer, room, msg, sha, total, offset, rep.Length, c.isList, rep.Body, l, keep)
		rep.Body.Close()
		switch {
		case errors.Is(err, store.ErrBadBytes):
			offset = 0 // dropped by receive
			return false, err
		case err != nil:
			offset = 0
			note(store.AnswerFailed)
			return false, err
		case have >= total:
			done = true
			return true, nil
		case have <= offset:
			note(store.AnswerFailed)
			return false, nil
		}
		offset = have
	}
}

// ---- the background work ----

// Tick does one round of the background work in every room: missing
// manifests fetched with bounded retries [F12], carry jobs fetched from an
// author that can be dialled, and with the setting keep a want for every
// file this machine may keep within its budget [F9]. Wants that may no
// longer be kept go in the sweep.
func (f *Files) Tick(ctx context.Context) {
	f.r.mu.Lock()
	rooms := make([]string, 0, len(f.r.rooms))
	for id := range f.r.rooms {
		rooms = append(rooms, id)
	}
	f.r.mu.Unlock()
	sort.Strings(rooms)
	for _, room := range rooms {
		f.sweepRoom(ctx, room)
		f.tickRoom(ctx, room)
	}
	// The deletion queue on a timer too, not only after a change: an entry
	// that failed (a file still open on Windows) is tried again [B4].
	f.m.st.RunTrash(ctx)
}

func (f *Files) tickRoom(ctx context.Context, room string) {
	var recs []store.FileRecord
	var wants []store.FileWant
	var mode string
	now := f.m.now()
	if err := f.m.st.View(ctx, func(rd *store.Reader) (err error) {
		if recs, err = rd.FileRecords(room); err != nil {
			return err
		}
		if wants, err = rd.FileWants(room); err != nil {
			return err
		}
		mode, err = rd.FileMode(room)
		return err
	}); err != nil {
		return
	}
	for _, fr := range recs {
		if !fr.OK {
			f.fetchList(ctx, room, fr, now)
		}
	}
	for _, w := range wants {
		if w.Lapsed.IsZero() && f.t != nil && f.t.CanDial(w.Author) {
			_, _ = f.download(ctx, room, w.Msg, w.SHA256, []string{w.Author})
		}
	}
	if mode == store.FilesKeep {
		f.keep(ctx, room, recs)
	}
}

// fetchList fetches the manifest of fr if fr is its message's winning
// version, its list is neither invalid nor unavailable and a retry is due.
func (f *Files) fetchList(ctx context.Context, room string, fr store.FileRecord, now time.Time) {
	c, err := f.check(ctx, room, fr.Msg, fr.Manifest, "")
	if err != nil || !c.listed || !c.isList || c.mf.Winner.ID != fr.Record || c.how != "" {
		return
	}
	var l store.FileList
	var have bool
	if err := f.m.st.View(ctx, func(rd *store.Reader) (err error) {
		l, have, err = rd.FileList(room, fr.Record)
		return err
	}); err != nil {
		return
	}
	if have && (l.State == store.ListInvalid || l.State == store.ListUnavailable || now.Before(l.Next)) {
		return
	}
	// Bytes held for another message are not this message's list: it is
	// obtained for this message, under its own permission, from its author
	// or a holder that may serve it for this message [B1].
	hs, err := f.holders(ctx, room, fr.Msg, fr.Manifest, c.mf.Author)
	if err != nil {
		return
	}
	if _, err := f.download(ctx, room, fr.Msg, fr.Manifest, hs); err == nil {
		return
	}
	// The first ask after the header arrived is not a failed answer: a
	// member that holds the list may be getting it at this moment (from an
	// author only it can reach, by a want). It is asked again soon, once.
	f.mu.Lock()
	first := !f.tried[fr.Record]
	f.tried[fr.Record] = true
	f.mu.Unlock()
	if first && len(hs) > 0 {
		f.soonAfter(room, core.ListRetrySoon)
	}
	if c.mf.Author != f.m.ID && (f.t == nil || !f.t.CanDial(c.mf.Author)) {
		_, _ = f.issueWant(ctx, store.FileWant{Room: room, Msg: fr.Msg, Version: fr.Record, SHA256: fr.Manifest,
			Author: c.mf.Author, List: true, At: now})
		if !c.wanting {
			return // the first answer to a want is not a failed answer
		}
	}
	if first && !have {
		return
	}
	_ = f.m.st.ListFailed(ctx, room, fr.Record, now)
}

// keep wants every file of every message this machine may keep, within
// the budget (the setting keep).
func (f *Files) keep(ctx context.Context, room string, recs []store.FileRecord) {
	for _, fr := range recs {
		if !fr.OK {
			continue
		}
		var items []core.FileRef
		if err := f.m.st.View(ctx, func(rd *store.Reader) (err error) {
			items, err = rd.FileItems(room, fr.Record)
			return err
		}); err != nil {
			return
		}
		for _, it := range items {
			c, err := f.check(ctx, room, fr.Msg, it.SHA256, "")
			if err != nil || !c.listed || c.mf.Winner.ID != fr.Record || c.how != "" || c.wanting || c.lapsed || c.hasPart ||
				!c.fits || c.selfAuth {
				continue
			}
			_, _ = f.issueWant(ctx, store.FileWant{Room: room, Msg: fr.Msg, Version: fr.Record, SHA256: it.SHA256,
				Author: c.mf.Author, Size: it.Size, At: f.m.now()})
		}
	}
}

// Run does Tick every interval until ctx ends, and one room's work at
// once when Soon asks for it.
func (f *Files) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	f.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.Tick(ctx)
		case room := <-f.soon:
			f.sweepRoom(ctx, room)
			f.tickRoom(ctx, room)
		}
	}
}

// ---- an agent's fetch ----

// States of an agent's fetch (PROTOCOL 10.6).
const (
	FetchHere        = "here"
	FetchPartial     = "partial"
	FetchComing      = "coming"
	FetchFetching    = "fetching"
	FetchUnavailable = "unavailable"
	FetchGone        = "gone"
	FetchTooLarge    = "too_large"
)

// FetchHolder is one member's last answer about a file.
type FetchHolder struct {
	Node   string `json:"node"`
	At     string `json:"at"`
	Answer string `json:"answer"`
}

// FetchResult is the answer to an agent's fetch.
type FetchResult struct {
	State   string        `json:"state"`
	Room    string        `json:"room,omitempty"`
	Message string        `json:"message,omitempty"`
	Name    string        `json:"name,omitempty"`
	Path    string        `json:"path,omitempty"`
	Size    int64         `json:"size"`
	Have    int64         `json:"have,omitempty"`
	From    string        `json:"from,omitempty"`
	Reason  string        `json:"reason,omitempty"`
	Holders []FetchHolder `json:"holders,omitempty"`
}

// FetchOpts name the file an agent asks for.
type FetchOpts struct {
	Agent, Room, SHA256, Message string
	Wait                         time.Duration
}

var errNotShownFile = &UserError{Code: ErrorNotShown, Msg: "no message shown to this agent lists that file; read it first"}

// Fetch is an agent's request for the bytes of one file: of a message whose
// winning version the agent was shown and lists the file [F4, F5]. The
// bytes are verified, then copied under files/<room>/ for the agent; they
// are never opened (PROTOCOL 10.6, 10.8).
func (f *Files) Fetch(ctx context.Context, o FetchOpts) (FetchResult, error) {
	if !core.ValidSHA256(o.SHA256) || o.Message != "" && !validRecordID(o.Message) || o.Room != "" && !core.ValidRoom(o.Room) {
		return FetchResult{}, &UserError{Code: ErrorInvalid, Msg: "fetch needs a sha256 of 64 lowercase hex, and a valid message and room if given"}
	}
	rooms := []string{o.Room}
	if o.Room == "" {
		f.r.mu.Lock()
		rooms = rooms[:0]
		for id := range f.r.rooms {
			rooms = append(rooms, id)
		}
		f.r.mu.Unlock()
		sort.Strings(rooms)
	}
	var gone *FetchResult
	for _, room := range rooms {
		var msgs []string
		if err := f.m.st.View(ctx, func(rd *store.Reader) (err error) {
			msgs, err = rd.MessagesListing(room, o.SHA256)
			return err
		}); err != nil {
			return FetchResult{}, err
		}
		for _, msg := range msgs {
			if o.Message != "" && msg != o.Message {
				continue
			}
			c, err := f.check(ctx, room, msg, o.SHA256, "")
			if err != nil {
				return FetchResult{}, err
			}
			if c.mf == nil || c.mf.Winner == nil && !c.mf.Gone {
				continue
			}
			if !c.listed || c.isList {
				if g := goneResult(c, room, msg); gone == nil && g != nil && f.shownAny(ctx, o.Agent, room, c) {
					gone = g
				}
				continue
			}
			if ok, err := f.shown(ctx, o.Agent, room, c); err != nil {
				return FetchResult{}, err
			} else if !ok {
				continue
			}
			return f.fetchOne(ctx, o, room, msg, c)
		}
	}
	if gone == nil && o.Message != "" {
		// A list no longer retained is forgotten with its names (finding
		// 8): a message named, shown and gone is reported gone.
		for _, room := range rooms {
			c, err := f.check(ctx, room, o.Message, o.SHA256, "")
			if err != nil {
				return FetchResult{}, err
			}
			if c.mf != nil && (c.mf.Gone || c.mf.Winner == nil || !c.mf.Winner.Keep) && f.shownAny(ctx, o.Agent, room, c) {
				gone = goneResult(c, room, o.Message)
				break
			}
		}
	}
	if gone != nil {
		return *gone, nil
	}
	return FetchResult{}, errNotShownFile
}

func goneResult(c fileCheck, room, msg string) *FetchResult {
	if c.mf == nil {
		return nil
	}
	r := &FetchResult{State: FetchGone, Room: room, Message: msg, Reason: "replaced"}
	switch {
	case c.mf.Gone:
		r.Reason = "withdrawn"
	case c.mf.Winner == nil || !c.mf.Winner.Keep:
		r.Reason = "expired"
	}
	return r
}

// shown: the agent was shown the winning version, or it is the agent that
// wrote that version on this machine. Every other agent on the author's
// machine, the message's first sender included when another agent wrote
// the winner, must have been shown it [fix-files-b 6, S2].
func (f *Files) shown(ctx context.Context, agent, room string, c fileCheck) (bool, error) {
	var ok bool
	err := f.m.st.View(ctx, func(rd *store.Reader) (err error) {
		ok, err = shownTo(rd, agent, room, c)
		return err
	})
	return ok, err
}

func shownTo(rd *store.Reader, agent, room string, c fileCheck) (bool, error) {
	if c.selfAuth {
		// only the agent that wrote the version fetched (the message
		// itself when it was not replaced), not the original sender when
		// another agent wrote the winner [S2]
		h, ok, err := rd.Record(room, c.mf.Winner.ID)
		if err != nil {
			return false, err
		}
		if ok && h.Record.Fields().Agent == agent {
			return true, nil
		}
	}
	return rd.ShownVersion(agent, room, c.mf.Msg, c.mf.Winner.ID)
}

// shownAny: the agent was shown some version of the message.
func (f *Files) shownAny(ctx context.Context, agent, room string, c fileCheck) bool {
	var ok bool
	_ = f.m.st.View(ctx, func(rd *store.Reader) (err error) {
		if c.selfAuth { // the agent that sent it, on this machine
			if h, held, err := rd.Record(room, c.mf.Msg); err != nil || held && h.Record.Fields().Agent == agent {
				ok = err == nil
				return err
			}
		}
		// a confirmed read replaced its receipt: it counts too
		if _, ok, err = rd.Read(agent, room, c.mf.Msg); err != nil || ok {
			return err
		}
		ok, err = rd.ShownText(agent, room, c.mf.Msg)
		return err
	})
	return ok
}

func (f *Files) fetchOne(ctx context.Context, o FetchOpts, room, msg string, c fileCheck) (FetchResult, error) {
	res := FetchResult{Room: room, Message: msg, Name: c.ref.Name, Size: c.ref.Size}
	if c.how == "" && c.held {
		if err := f.m.st.UseHeld(ctx, room, o.SHA256, msg); err != nil {
			return res, err
		}
		c.how = store.HowLocal
	}
	from := ""
	if c.how == "" {
		if !c.fits && !c.hasPart && !c.wanting {
			res.State = FetchTooLarge
			return res, nil
		}
		hs, err := f.holders(ctx, room, msg, o.SHA256, c.mf.Author)
		if err != nil {
			return res, err
		}
		wait := min(max(o.Wait, 0), time.Duration(core.MaxFetchWaitMS)*time.Millisecond)
		dctx, cancel := context.WithTimeout(ctx, max(wait, time.Second))
		from, _ = f.download(dctx, room, msg, o.SHA256, hs)
		cancel()
		_ = hookAt("files:downloaded") // tests: the room left during the download [F1 of the fifth review]
		if c, err = f.check(ctx, room, msg, o.SHA256, ""); err != nil {
			return res, err
		}
		if !c.listed || c.mf == nil || c.mf.Winner == nil {
			if g := goneResult(c, room, msg); g != nil {
				return *g, nil
			}
			return res, errNotShownFile
		}
		if c.how == "" {
			return f.notHere(ctx, res, room, msg, o.SHA256, c)
		}
	}
	// A new copy every time, decided like the first [B3]: under a lease on
	// the rule (the version shown still wins, lists the file and may be
	// kept), asked again inside each transaction of the publication.
	r := f.copyRule(msg, o.SHA256, o.Agent, c.mf.Winner.ID)
	refused := func() (FetchResult, error) {
		if c, cerr := f.check(ctx, room, msg, o.SHA256, ""); cerr == nil {
			if g := goneResult(c, room, msg); g != nil {
				return *g, nil
			}
		}
		return res, errNotShownFile
	}
	l, err := f.grant(ctx, room, r)
	if errors.Is(err, errRevoked) {
		return refused()
	}
	if err != nil {
		return res, err
	}
	defer l.end()
	cp, err := f.m.st.PublishCopy(ctx, store.CopyReq{Room: room, Msg: msg, SHA256: o.SHA256, Agent: o.Agent, Name: c.ref.Name,
		Size: c.ref.Size}, f.gate(room, r), l.valid)
	switch {
	case errors.Is(err, store.ErrCopyRefused):
		return refused()
	case errors.Is(err, store.ErrNoSpace):
		res.State = FetchTooLarge
		return res, nil
	case err != nil:
		return res, err
	}
	res.State, res.Path, res.From = FetchHere, f.m.st.CopyPath(cp.Path), from
	return res, nil
}

// notHere says why the bytes are not here yet, issuing a want to an
// author nobody can dial [F9].
func (f *Files) notHere(ctx context.Context, res FetchResult, room, msg, sha string, c fileCheck) (FetchResult, error) {
	if c.mf == nil || c.mf.Winner == nil {
		return res, errNotShownFile // no list obtained: nothing to say about it
	}
	// Only what current members answered, read in one transaction with
	// the membership: a former holder's old "not yet" says nothing now [W5].
	var obs []store.FileObs
	_ = f.m.st.View(ctx, func(rd *store.Reader) error {
		all, err := rd.FileObs(room, msg, sha)
		if err != nil {
			return err
		}
		ms, err := rd.RoomMembers(room)
		if err != nil {
			return err
		}
		member := map[string]bool{}
		for _, m := range ms {
			member[m.Node] = m.Member
		}
		for _, ob := range all {
			if member[ob.Peer] {
				obs = append(obs, ob)
			}
		}
		return nil
	})
	for _, ob := range obs {
		res.Holders = append(res.Holders, FetchHolder{Node: ob.Peer, At: ob.At.UTC().Format(time.RFC3339), Answer: ob.Answer})
	}
	notYet := false
	for _, ob := range obs {
		notYet = notYet || ob.Answer == store.AnswerNotYet
	}
	switch {
	case c.lapsed:
		res.State, res.Reason = FetchUnavailable, "not_carried"
	case c.hasPart:
		res.State, res.Have = FetchPartial, c.part.Have
	case c.wanting || notYet:
		res.State = FetchComing
	case !c.selfAuth && (f.t == nil || !f.t.CanDial(c.mf.Author)):
		_, err := f.issueWant(ctx, store.FileWant{Room: room, Msg: msg, Version: c.mf.Winner.ID, SHA256: sha,
			Author: c.mf.Author, Size: c.ref.Size, At: f.m.now()})
		switch {
		case errors.Is(err, store.ErrNoSpace):
			res.State = FetchTooLarge
		case err != nil:
			return res, err
		default:
			res.State = FetchComing
		}
	default:
		res.State = FetchUnavailable
	}
	return res, nil
}
