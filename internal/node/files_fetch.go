package node

// The requester's side of attachments: wants in pull answers and their
// uploads (PROTOCOL 10.4, 10.5), downloads from holders (10.3), the
// automatic manifest fetch with bounded retries [F12], carry jobs and the
// keep setting [F9], and an agent's fetch (10.6).

import (
	"context"
	"errors"
	"io"
	"slices"
	"sort"
	"time"

	"github.com/wallter/a2p2/internal/core"
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
	now := f.m.now()
	ws, err := f.m.st.WantsToSend(ctx, room, puller, now, max, core.WantRepeat, func(rd *store.Reader, w store.FileWant) (bool, error) {
		mf, err := rd.MessageFiles(room, w.Msg, f.m.ID, now)
		if err != nil {
			return false, err
		}
		_, _, listed := mf.Listed(w.SHA256)
		return listed && mf.Winner.ID == w.Version && mf.Author == puller, nil
	})
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

// upload sends w's bytes to peer in chunks, each continuing from the have
// of the last answer.
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
		status, got, err := f.t.PutFile(ctx, peer, FilePut{Room: room, Msg: w.M, SHA256: w.H, Offset: have, Length: n, Total: c.size},
			io.LimitReader(b, n))
		b.Close()
		if err != nil || status != 200 || got <= have {
			return err
		}
		have = got
		// the permission again before the next chunk [F2]
		if c, err = f.check(ctx, room, w.M, w.H, peer); err != nil || !c.servable() {
			return err
		}
	}
	return nil
}

// ---- downloads ----

// holders orders whom to ask for sha of msg (PROTOCOL 10.3): the author,
// members whose last answer was that they hold or carry it, then other
// current members; never this machine, a peer that cannot be dialled or
// one marked old_version.
func (f *Files) holders(ctx context.Context, room, msg, sha, author string) ([]string, error) {
	st, err := f.r.State(room)
	if err != nil {
		return nil, err
	}
	var obs []store.FileObs
	if err := f.m.st.View(ctx, func(rd *store.Reader) (err error) {
		obs, err = rd.FileObs(room, msg, sha)
		return err
	}); err != nil {
		return nil, err
	}
	now := f.m.now()
	ok := func(p string) bool {
		if p == f.m.ID || !slices.Contains(st.Members, p) || f.t == nil || !f.t.CanDial(p) {
			return false
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		return !now.Before(f.old[p]) || f.old[p].IsZero()
	}
	var out []string
	add := func(p string) {
		if ok(p) && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	add(author)
	sort.SliceStable(obs, func(i, j int) bool { return obs[i].At.After(obs[j].At) })
	for _, o := range obs {
		if o.Answer == store.AnswerHas || o.Answer == store.AnswerNotYet {
			add(o.Peer)
		}
	}
	for _, p := range st.Members {
		add(p)
	}
	return out, nil
}

// download fetches sha for msg from the first of holders that serves it,
// each asked at most once, resuming a part. It returns who served it.
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

// getFrom asks peer for the rest of sha of msg, chunk by chunk, while it
// serves; done when the whole is verified and held.
func (f *Files) getFrom(ctx context.Context, room, msg, sha, peer string) (bool, error) {
	release, err := f.tr.take(peer, dirIn)
	if err != nil {
		return false, err
	}
	defer release()
	note := func(a string) { _ = f.m.st.NoteFileObs(ctx, room, msg, sha, peer, a, f.m.now()) }
	for {
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
		offset := int64(0)
		if c.hasPart {
			offset = c.part.Have
		}
		rep, err := f.t.GetFile(ctx, peer, FileGet{Room: room, Msg: msg, SHA256: sha, Offset: offset})
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
		total := c.ref.Size
		if c.isList {
			total = rep.Total
			if c.hasPart {
				total = c.part.Size
			}
		}
		bad := rep.Total != total || total < 1 || c.isList && total > core.MaxManifestBytes ||
			rep.Offset != offset || rep.Length < 1 || rep.Length > core.FileChunk || offset+rep.Length > total
		if bad {
			rep.Body.Close()
			note(store.AnswerFailed)
			return false, nil
		}
		have, err := f.receive(ctx, peer, room, msg, sha, total, offset, rep.Length, c.isList, rep.Body, func() bool {
			c, err := f.check(context.WithoutCancel(ctx), room, msg, sha, "")
			return err == nil && c.listed
		})
		rep.Body.Close()
		switch {
		case errors.Is(err, store.ErrBadBytes):
			return false, err
		case err != nil:
			note(store.AnswerFailed)
			return false, err
		case have >= total:
			return true, nil
		case have <= offset:
			note(store.AnswerFailed)
			return false, nil
		}
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
	if c.held {
		// the same bytes are held for another message: verified already
		_ = f.m.st.UseHeld(ctx, room, fr.Manifest, fr.Msg)
		return
	}
	hs, err := f.holders(ctx, room, fr.Msg, fr.Manifest, c.mf.Author)
	if err != nil {
		return
	}
	if _, err := f.download(ctx, room, fr.Msg, fr.Manifest, hs); err == nil {
		return
	}
	if c.mf.Author != f.m.ID && (f.t == nil || !f.t.CanDial(c.mf.Author)) {
		_, _ = f.m.st.IssueWant(ctx, store.FileWant{Room: room, Msg: fr.Msg, Version: fr.Record, SHA256: fr.Manifest,
			Author: c.mf.Author, List: true, At: now})
		if !c.wanting {
			return // the first answer to a want is not a failed answer
		}
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
			_, _ = f.m.st.IssueWant(ctx, store.FileWant{Room: room, Msg: fr.Msg, Version: fr.Record, SHA256: it.SHA256,
				Author: c.mf.Author, Size: it.Size, At: f.m.now()})
		}
	}
}

// Run does Tick every interval until ctx ends.
func (f *Files) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		f.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
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

// shown: the agent was shown the winning version, or its machine wrote it.
func (f *Files) shown(ctx context.Context, agent, room string, c fileCheck) (bool, error) {
	if c.selfAuth {
		return true, nil
	}
	var ok bool
	err := f.m.st.View(ctx, func(rd *store.Reader) (err error) {
		ok, err = rd.ShownVersion(agent, room, c.mf.Msg, c.mf.Winner.ID)
		return err
	})
	return ok, err
}

// shownAny: the agent was shown some version of the message.
func (f *Files) shownAny(ctx context.Context, agent, room string, c fileCheck) bool {
	if c.selfAuth {
		return true
	}
	var ok bool
	_ = f.m.st.View(ctx, func(rd *store.Reader) (err error) {
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
		if c, _ = f.check(ctx, room, msg, o.SHA256, ""); !c.listed {
			if g := goneResult(c, room, msg); g != nil {
				return *g, nil
			}
		}
		if c.how == "" {
			return f.notHere(ctx, res, room, msg, o.SHA256, c)
		}
	}
	cp, done, err := f.m.st.ReserveCopy(ctx, room, msg, o.SHA256, o.Agent, c.ref.Name, c.ref.Size)
	if err != nil {
		return res, err
	}
	if !done {
		version := c.mf.Winner.ID
		err := f.m.st.PublishCopy(ctx, cp, func(rd *store.Reader) (bool, error) {
			mf, err := rd.MessageFiles(room, msg, f.m.ID, f.m.now())
			if err != nil {
				return false, err
			}
			_, isList, listed := mf.Listed(o.SHA256)
			if !listed || isList || mf.Winner.ID != version {
				return false, nil
			}
			if mf.Author == f.m.ID {
				return true, nil
			}
			return rd.ShownVersion(o.Agent, room, msg, version)
		})
		if err != nil {
			_ = f.m.st.DropCopy(ctx, cp, f.m.now())
			if errors.Is(err, store.ErrCopyRefused) {
				if c, cerr := f.check(ctx, room, msg, o.SHA256, ""); cerr == nil {
					if g := goneResult(c, room, msg); g != nil {
						return *g, nil
					}
				}
			}
			return res, err
		}
		// the permission again: a withdraw committed meanwhile deletes it
		f.sweepRoom(ctx, room)
	}
	res.State, res.Path, res.From = FetchHere, f.m.st.CopyPath(cp.Path), from
	return res, nil
}

// notHere says why the bytes are not here yet, issuing a want to an
// author nobody can dial [F9].
func (f *Files) notHere(ctx context.Context, res FetchResult, room, msg, sha string, c fileCheck) (FetchResult, error) {
	var obs []store.FileObs
	_ = f.m.st.View(ctx, func(rd *store.Reader) (err error) {
		obs, err = rd.FileObs(room, msg, sha)
		return err
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
		_, err := f.m.st.IssueWant(ctx, store.FileWant{Room: room, Msg: msg, Version: c.mf.Winner.ID, SHA256: sha,
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
