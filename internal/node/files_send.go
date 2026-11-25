package node

// Sends with files (PROTOCOL 10.6, DESIGN 12): an agent asks, the files
// are staged from the handles opened, and nothing is written until a
// person approves the request on this machine [F6]; approval checks the
// staged bytes again and, for a replacement, that the replaced version is
// still the winning one [F8], then stores the bytes and the record
// together [F10].

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// FileSend is an agent's send with files: a message (Draft), or with
// Replaces new text for one of this machine's messages. Files are
// absolute paths, or "sha256:<hex>" for a file the replaced version lists
// and this machine holds, kept by listing it again.
type FileSend struct {
	Draft
	Replaces string
	Files    []string
}

// FileSendResult is what asking returns: nothing is written yet.
type FileSendResult struct {
	Request  string            `json:"request"`
	State    string            `json:"state"`
	Room     string            `json:"room"`
	To       []core.Addressee  `json:"to,omitempty"`
	Replaces string            `json:"replaces,omitempty"`
	Files    []core.FileRef    `json:"files"`
	Draft    store.SendDraft   `json:"-"`
	Terms    string            `json:"-"`
	Paths    map[string]string `json:"-"`
}

// KeptPrefix marks a file kept from the replaced version.
const KeptPrefix = "sha256:"

func badFile(msg string) error { return &UserError{Code: ErrorInvalid, Msg: msg} }

// fileRefusal is badFile carrying the store.FileError behind it, so a
// surface can log its op and code (never a path) without reading text.
type fileRefusal struct {
	*UserError
	cause error
}

func (r fileRefusal) Unwrap() []error { return []error{r.UserError, r.cause} }

// badFileFrom is badFile(msg) with cause, a FileError, behind it.
func badFileFrom(msg string, cause error) error {
	return fileRefusal{UserError: &UserError{Code: ErrorInvalid, Msg: msg}, cause: cause}
}

// RequestSend stages a send with files and raises a request for this
// machine's person. Each path is opened once; a file that is not regular
// once opened is refused; the bytes are copied and hashed from that handle
// (PROTOCOL 10.11).
func (f *Files) RequestSend(ctx context.Context, s FileSend) (FileSendResult, error) {
	var res FileSendResult
	if err := checkAgent(s.Agent); err != nil {
		return res, err
	}
	if len(s.Files) > core.MaxFiles {
		return res, &UserError{Code: ErrorTooLarge, Msg: "at most " + strconv.Itoa(core.MaxFiles) + " files"}
	}
	if len(s.Files) == 0 {
		return res, badFile("a send with files names at least one file; a replacement without files is a plain replace")
	}
	if s.Everyone && len(s.To) > 0 {
		return res, userErr("a post to everyone names no addressees")
	}
	if s.Replaces != "" && (s.ReplyTo != "" || len(s.To) > 0 || s.Everyone || s.AskHandled || len(s.Choices) > 0) {
		return res, userErr("a replacement takes only text and files")
	}
	if len(s.Choices) > 0 {
		return res, userErr("a send with files carries no choices")
	}
	mst, err := f.r.State(s.Room)
	if err != nil {
		return res, err
	}
	now := f.m.now()
	d := store.SendDraft{Agent: s.Agent, Text: s.Text, To: s.To, Everyone: s.Everyone, Parent: s.ReplyTo,
		AskHandled: s.AskHandled, Replaces: s.Replaces}
	if d.Parent != "" && len(d.To) == 0 && !d.Everyone {
		// An implicit reply goes to the parent's author: the terms name
		// that recipient, not everyone [S1]. The parent is immutable, so
		// the record sends to the same one.
		if err := f.m.view(ctx, s.Room, func(v *roomView) error {
			p, err := v.get(d.Parent)
			if err != nil {
				return err
			}
			if p == nil || p.f.Kind != core.KindMessage {
				return ErrNotHeld
			}
			d.To = []core.Addressee{{Node: p.author, Agent: p.f.Agent}}
			return nil
		}); err != nil {
			return res, err
		}
	}
	if d.Everyone || len(d.To) == 0 {
		d.Members = len(mst.Members) // "everyone", as many as that is now [fix-files-b 5]
	}
	var mf *store.MessageFiles
	if s.Replaces != "" {
		if !validRecordID(s.Replaces) {
			return res, badFile("replaces is a message id")
		}
		if err := f.m.st.View(ctx, func(rd *store.Reader) (err error) {
			mf, err = rd.MessageFiles(s.Room, s.Replaces, f.m.ID, now)
			return err
		}); err != nil {
			return res, err
		}
		if mf.Author != f.m.ID || mf.Gone || mf.Winner == nil {
			return res, ErrNotAllowed
		}
		d.Base = mf.Winner.ID
	}
	id := f.r.newID()
	seen := map[string]bool{}
	staged := []store.StagedFile{}
	fail := func(err error) (FileSendResult, error) {
		f.m.st.DropStaging(id)
		f.m.st.ReleaseStaging(id)
		return FileSendResult{}, err
	}
	// The budget is reserved for the sizes the files have now, before a
	// byte is copied; a file that grows meanwhile is refused (finding 9).
	sizes := map[int]int64{}
	var total int64
	for i, p := range s.Files {
		if strings.HasPrefix(p, KeptPrefix) || !filepath.IsAbs(p) {
			continue
		}
		if store.RemoteOrDevice(p) { // before anything touches the path [fix-files-b 7]
			return res, badFileFrom("a remote or device path is not sent", store.ErrRemoteOrDevice)
		}
		st, err := os.Stat(p)
		if err != nil {
			return res, badFileFrom("not a regular file once opened, or unreadable", store.FileErrorOf("stat", err))
		}
		if st.Size() > core.MaxFileSize {
			return res, &UserError{Code: ErrorTooLarge, Msg: "a file is at most 256 MiB"}
		}
		sizes[i] = st.Size()
		total += st.Size()
	}
	switch err := f.m.st.ReserveStaging(ctx, s.Room, id, total); {
	case errors.Is(err, store.ErrNoSpace):
		return res, &UserError{Code: ErrorTooLarge, Msg: "the files do not fit the room's or the machine's file budget"}
	case err != nil:
		return res, err
	}
	for i, p := range s.Files {
		var df store.DraftFile
		if strings.HasPrefix(p, KeptPrefix) {
			sha := strings.TrimPrefix(p, KeptPrefix)
			if mf == nil || !core.ValidSHA256(sha) {
				return fail(badFile("sha256:<hex> keeps a file of the version replaced"))
			}
			ref, isList, ok := mf.Listed(sha)
			if !ok || isList {
				return fail(badFile("the version replaced does not list " + sha))
			}
			df = store.DraftFile{StagedFile: store.StagedFile{N: i, SHA256: sha, Size: ref.Size, Kept: true}, Name: ref.Name, Type: ref.Type}
		} else {
			if !filepath.IsAbs(p) {
				return fail(badFile("a file is an absolute path"))
			}
			sf, err := f.m.st.StageFile(id, i, p, sizes[i])
			switch {
			case errors.Is(err, store.ErrTooLarge):
				return fail(&UserError{Code: ErrorTooLarge, Msg: "a file grew while it was staged; ask again"})
			case err != nil:
				return fail(badFileFrom("not a regular file once opened, or unreadable", store.FileErrorOf("open", err)))
			}
			name := filepath.Base(sf.Path)
			if !core.ValidFileName(name) {
				name = store.SafeName(name)
			}
			df = store.DraftFile{StagedFile: store.StagedFile{N: i, SHA256: sf.SHA256, Size: sf.Size}, Path: sf.Path, Name: name}
		}
		if seen[df.SHA256] {
			return fail(badFile("the same bytes are listed twice"))
		}
		seen[df.SHA256] = true
		d.Files = append(d.Files, df)
		staged = append(staged, df.StagedFile)
	}
	if _, err := core.EncodeManifest(draftManifest(d, strings.Repeat("00", 16))); err != nil {
		return fail(badFile("the files break a manifest rule: " + err.Error()))
	}
	draft, err := json.Marshal(d)
	if err != nil {
		return fail(err)
	}
	q := store.Request{ID: id, Kind: store.RequestSendFiles, Room: s.Room, Created: now, State: store.RequestPending,
		Draft: string(draft), Terms: SendTerms(s.Room, d)}
	switch err := f.m.st.CreateSendRequest(ctx, q, staged, MaxPendingRequests); {
	case errors.Is(err, store.ErrNoSpace):
		return fail(&UserError{Code: ErrorTooLarge, Msg: "the files do not fit the room's or the machine's file budget"})
	case errors.Is(err, store.ErrTooManyPending):
		return fail(&UserError{Code: ErrorRateLimited, Msg: err.Error()})
	case err != nil:
		return fail(err)
	}
	res = FileSendResult{Request: id, State: "waiting", Room: s.Room, To: s.To, Replaces: s.Replaces, Draft: d, Terms: q.Terms}
	for _, x := range d.Files {
		res.Files = append(res.Files, core.FileRef{Name: x.Name, Size: x.Size, SHA256: x.SHA256, Type: x.Type})
	}
	return res, nil
}

func draftManifest(d store.SendDraft, salt string) core.Manifest {
	m := core.Manifest{Salt: salt}
	for _, x := range d.Files {
		m.Files = append(m.Files, core.FileRef{Name: x.Name, Size: x.Size, SHA256: x.SHA256, Type: x.Type})
	}
	return m
}

// SendTerms is what a person approves (PROTOCOL 10.6): the SHA-256 over
// "a2p2 send files 2", the room, the agent, the effective recipients
// (each one, or everyone with the member count), the thread parent, the
// message replaced and the version of it replaced, the whole text's hash
// and each file's path, size and SHA-256 [fix-files-b 5].
func SendTerms(room string, d store.SendDraft) string {
	h := sha256.New()
	put := func(s string) { h.Write([]byte(strconv.Itoa(len(s)) + ":" + s + "\n")) }
	put("a2p2 send files 2")
	put(room)
	put(d.Agent)
	if d.Everyone || len(d.To) == 0 {
		put("everyone")
		put(strconv.Itoa(d.Members))
	}
	for _, a := range d.To {
		put(a.Node + "/" + a.Agent)
	}
	put(d.Parent)
	put(d.Replaces)
	put(d.Base)
	put(core.SHA256Hex([]byte(d.Text)))
	for _, x := range d.Files {
		path := x.Path
		if x.Kept {
			path = KeptPrefix + x.SHA256
		}
		put(path)
		put(strconv.FormatInt(x.Size, 10))
		put(x.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// errSendNeedsTerms refuses an approval of a send with files that does
// not go through ApproveSend: there is one way to approve one.
var errSendNeedsTerms = &UserError{Code: ErrorTermsChanged, Msg: "the terms shown are not the request's; show it again"}

// ApproveSend approves the send with files id only if terms is the digest
// of the terms the person was shown (SendTerms, as the request exposes
// it) [fix-files-b 5]. Surfaces approve sends with files through it.
func (f *Files) ApproveSend(ctx context.Context, id, terms string) (Confirmation, error) {
	q, err := f.m.st.GetRequest(ctx, id)
	if err != nil {
		return Confirmation{}, err
	}
	if q.Kind != store.RequestSendFiles {
		return Confirmation{}, &UserError{Code: ErrorTermsChanged, Msg: "not a send with files"}
	}
	if q.State == store.RequestPending && (terms == "" || terms != q.Terms) {
		return Confirmation{}, errSendNeedsTerms
	}
	return f.confirmSend(ctx, q, true)
}

// confirmSend is a person's decision on a send with files.
func (f *Files) confirmSend(ctx context.Context, q store.Request, approve bool) (Confirmation, error) {
	now := f.m.now()
	if q.State == store.RequestFailed || q.State == store.RequestDeclined {
		return Confirmation{ID: q.ID, State: q.State, Reason: q.Reason}, nil
	}
	if q.State != store.RequestPending {
		return Confirmation{}, &UserError{errNotPendingText, ErrorAlreadyDone}
	}
	decide := func(state, reason string) (Confirmation, error) {
		if err := f.m.st.DecideSend(ctx, q.ID, state, reason, now); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return Confirmation{}, &UserError{errNotPendingText, ErrorAlreadyDone}
			}
			return Confirmation{}, err
		}
		return Confirmation{ID: q.ID, State: state, Reason: reason}, nil
	}
	if !approve {
		return decide(store.RequestDeclined, "")
	}
	var d store.SendDraft
	if err := json.Unmarshal([]byte(q.Draft), &d); err != nil || SendTerms(q.Room, d) != q.Terms {
		return decide(store.RequestFailed, "the request's terms do not match what was staged")
	}
	for _, x := range d.Files {
		if !x.Kept && f.m.st.CheckStaged(q.ID, x.N, x.SHA256, x.Size) != nil {
			return decide(store.RequestFailed, store.ReasonSendChanged)
		}
	}
	if d.Replaces != "" {
		var ok bool
		if err := f.m.st.View(ctx, func(rd *store.Reader) (err error) {
			ok, err = store.StillReplaceable(rd, q.Room, f.m.ID, d, now)
			return err
		}); err != nil {
			return Confirmation{}, err
		}
		if !ok {
			return decide(store.RequestFailed, store.ReasonSendReplaced)
		}
	}
	sf := store.SendFiles{Request: q.ID, Room: q.Room, Clock: f.m.now} // the expiry by the clock inside the transaction [B6]
	var man core.Manifest
	var fields func(*roomView, *core.RecordFields) error
	if d.Replaces != "" {
		fields = amendDecide(f.m.ID, d.Replaces, core.KindReplace, d.Text)
	} else {
		var to []core.Addressee
		fields = sendDecide(Draft{Room: q.Room, Agent: d.Agent, Text: d.Text, To: d.To, Everyone: d.Everyone,
			ReplyTo: d.Parent, AskHandled: d.AskHandled}, &to)
	}
	var rnd [16]byte
	f.m.rand(rnd[:])
	salt, err := core.NewSalt(rnd[:])
	if err != nil {
		return Confirmation{}, err
	}
	man = draftManifest(d, salt)
	if sf.Manifest, err = core.EncodeManifest(man); err != nil {
		return decide(store.RequestFailed, "the files break a manifest rule")
	}
	for _, x := range d.Files {
		sf.Files = append(sf.Files, x.StagedFile)
	}
	msha := core.SHA256Hex(sf.Manifest)
	decideAll := func(v *roomView, rf *core.RecordFields) error {
		if err := fields(v, rf); err != nil {
			return err
		}
		// the approved recipients again, inside the write's transaction:
		// "everyone" as many as the terms said [S1]
		if d.Everyone || len(d.To) == 0 {
			if st, err := f.r.State(q.Room); err != nil || len(st.Members) != d.Members {
				return errTermsChangedMeanwhile
			}
		}
		if d.Replaces != "" {
			// the base again, inside the write's transaction [F8]
			ok, err := store.StillReplaceable(v.rd, q.Room, f.m.ID, d, now)
			if err != nil {
				return err
			}
			if !ok {
				return errReplacedMeanwhile
			}
		}
		rf.Files = msha
		return nil
	}
	sent, err := f.m.writeWith(ctx, q.Room, d.Agent, decideAll, func(ctx context.Context, now time.Time,
		build func(*store.Reader) (*core.VerifiedRecord, error)) (*core.VerifiedRecord, error) {
		_ = hookAt("files:publish") // tests: time spent putting the bytes in place
		return f.m.st.WriteWithFiles(ctx, now, sf, man, build)
	})
	switch {
	case errors.Is(err, errReplacedMeanwhile):
		return decide(store.RequestFailed, store.ReasonSendReplaced)
	case errors.Is(err, errTermsChangedMeanwhile):
		return decide(store.RequestFailed, ErrorTermsChanged)
	case errors.Is(err, store.ErrSendExpired):
		return decide(store.RequestDeclined, store.ReasonSendExpired)
	case errors.Is(err, store.ErrStagingChanged):
		return decide(store.RequestFailed, store.ReasonSendChanged)
	case err != nil:
		return Confirmation{}, err
	}
	f.sweepRoom(ctx, q.Room)
	return Confirmation{ID: q.ID, State: store.RequestApproved, Entry: sent.ID, Effect: sent.ID, Entries: []string{sent.ID}}, nil
}

var errReplacedMeanwhile = errors.New("files: the replaced version changed before approval")

// errTermsChangedMeanwhile: the room's membership is not the one the
// approved terms counted for "everyone".
var errTermsChangedMeanwhile = errors.New("files: the recipients changed before publication")
