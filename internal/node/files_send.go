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
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
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
	if err := f.r.setAsideErr(s.Room); err != nil {
		return res, err
	}
	if _, err := f.r.State(s.Room); err != nil {
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
		// A replacement has the audience of the message it replaces, which
		// is immutable: the terms name it, a broadcast reply included [L2].
		if err := f.m.view(ctx, s.Room, func(v *roomView) (err error) {
			d.To, err = replacedAudience(v, s.Replaces)
			return err
		}); err != nil {
			return res, err
		}
	}
	// The audience as written: no addressee is everyone, whatever made it so.
	d.Everyone = len(d.To) == 0
	if d.Everyone {
		// "everyone" is the set of members now, bound by its digest and
		// shown by its size [fix-files-b 5, L2]
		if err := f.m.st.View(ctx, func(rd *store.Reader) (err error) {
			d.Members, d.MemberSet, err = memberSet(rd, s.Room)
			return err
		}); err != nil {
			return res, err
		}
	}
	// The whole record, as approval would write it, is checked before a
	// byte is staged: a send that cannot be published copies nothing [L3].
	if err := f.m.checkWrite(ctx, s.Room, d.Agent, f.publishDecide(s.Room, d, placeholderSHA(0))); err != nil {
		return res, unwritable(err)
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
	// The manifest's rules too, with the names the paths give and stand-in
	// digests, before anything is copied; checked again once staged [L3].
	pre := store.SendDraft{}
	for i, p := range s.Files {
		name := filepath.Base(p)
		if !core.ValidFileName(name) {
			name = store.SafeName(name)
		}
		x := store.DraftFile{StagedFile: store.StagedFile{N: i, SHA256: placeholderSHA(i + 1), Size: sizes[i]}, Name: name}
		if strings.HasPrefix(p, KeptPrefix) {
			var ref core.FileRef
			var isList, ok bool
			if mf != nil {
				ref, isList, ok = mf.Listed(strings.TrimPrefix(p, KeptPrefix))
			}
			if !ok || isList {
				continue // refused with its own words below
			}
			x.Name, x.Size, x.Type = ref.Name, ref.Size, ref.Type
		}
		pre.Files = append(pre.Files, x)
	}
	if _, err := core.EncodeManifest(draftManifest(pre, strings.Repeat("00", 16))); err != nil {
		return res, badFile("the files break a manifest rule: " + err.Error())
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

// replacedAudience is the addressees of message ref as written (none:
// everyone): what a replacement of it is delivered to.
func replacedAudience(v *roomView, ref string) ([]core.Addressee, error) {
	t, err := v.get(ref)
	if err != nil {
		return nil, err
	}
	if t == nil || t.f.Kind != core.KindMessage {
		return nil, ErrNotAllowed
	}
	return slices.Clone(t.f.To), nil
}

// memberSet is the room's members as committed in rd's transaction: how
// many, and the SHA-256 over "a2p2 members 1" and their sorted keys.
func memberSet(rd *store.Reader, room string) (int, string, error) {
	ms, err := rd.RoomMembers(room)
	if err != nil {
		return 0, "", err
	}
	var keys []string
	for _, m := range ms {
		if m.Member {
			keys = append(keys, m.Node)
		}
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte("a2p2 members 1\n"))
	for _, k := range keys {
		h.Write([]byte(k + "\n"))
	}
	return len(keys), hex.EncodeToString(h.Sum(nil)), nil
}

// SendTerms is what a person approves (PROTOCOL 10.6): the SHA-256 over
// "a2p2 send files 3", the room, the agent, the effective recipients
// (each one, or everyone with the member count and the digest of the
// member set), the thread parent, the message replaced and the version of
// it replaced, the whole text's hash and each file's path, size and
// SHA-256 [fix-files-b 5, L2]. A replacement's recipients are those of
// the message it replaces.
func SendTerms(room string, d store.SendDraft) string {
	h := sha256.New()
	put := func(s string) { h.Write([]byte(strconv.Itoa(len(s)) + ":" + s + "\n")) }
	put("a2p2 send files 3")
	put(room)
	put(d.Agent)
	if d.Everyone || len(d.To) == 0 {
		put("everyone")
		put(strconv.Itoa(d.Members))
		put(d.MemberSet)
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
	var rnd [16]byte
	f.m.rand(rnd[:])
	salt, err := core.NewSalt(rnd[:])
	if err != nil {
		return Confirmation{}, err
	}
	man := draftManifest(d, salt)
	if sf.Manifest, err = core.EncodeManifest(man); err != nil {
		return decide(store.RequestFailed, "the files break a manifest rule")
	}
	for _, x := range d.Files {
		sf.Files = append(sf.Files, x.StagedFile)
	}
	decideAll := f.publishDecide(q.Room, d, core.SHA256Hex(sf.Manifest))
	// The whole record is built and checked before a byte is copied: a
	// send that cannot be published copies nothing [L3].
	switch err := f.m.checkWrite(ctx, q.Room, d.Agent, decideAll); {
	case errors.Is(err, errReplacedMeanwhile):
		return decide(store.RequestFailed, store.ReasonSendReplaced)
	case errors.Is(err, errTermsChangedMeanwhile):
		return decide(store.RequestFailed, ErrorTermsChanged)
	case err != nil:
		var ue *UserError
		if errors.As(err, &ue) {
			return decide(store.RequestFailed, ReasonSendUnwritable)
		}
		return Confirmation{}, err
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

// publishDecide is what publishing draft d in room writes and checks,
// inside the write's own transaction: the record (a message, or the
// replacement), the approved recipients again ("everyone" the member set
// the terms bound, read from the membership the transaction sees
// committed [S1, L2]; a replacement's the replaced message's addressees),
// for a replacement the base again with the clock read there [F8, W3],
// and the manifest's hash msha.
func (f *Files) publishDecide(room string, d store.SendDraft, msha string) func(*roomView, *core.RecordFields) error {
	var fields func(*roomView, *core.RecordFields) error
	if d.Replaces != "" {
		fields = amendDecide(f.m.ID, d.Replaces, core.KindReplace, d.Text)
	} else {
		var to []core.Addressee
		fields = sendDecide(Draft{Room: room, Agent: d.Agent, Text: d.Text, To: d.To, Everyone: d.Everyone,
			ReplyTo: d.Parent, AskHandled: d.AskHandled}, &to)
	}
	return func(v *roomView, rf *core.RecordFields) error {
		if err := fields(v, rf); err != nil {
			return err
		}
		if d.Everyone || len(d.To) == 0 {
			n, set, err := memberSet(v.rd, room)
			if err != nil {
				return err
			}
			if n != d.Members || set != d.MemberSet {
				return errTermsChangedMeanwhile
			}
		}
		if d.Replaces != "" {
			to, err := replacedAudience(v, d.Replaces)
			if err != nil {
				return err
			}
			if !slices.Equal(to, d.To) {
				return errTermsChangedMeanwhile
			}
			_ = hookAt("files:replaceable") // tests: time passes before the clock is read
			ok, err := store.StillReplaceable(v.rd, room, f.m.ID, d, f.m.now())
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
}

// ReasonSendUnwritable: the message a send with files would publish
// breaks a rule of records (its text too long, too many addressees);
// nothing was copied [L3].
const ReasonSendUnwritable = "the message cannot be written: its text or recipients break a limit of a record"

// placeholderSHA is a stand-in digest, distinct for each i, of the length
// of a real one.
func placeholderSHA(i int) string { return fmt.Sprintf("%064x", i) }

// unwritable is a refusal of a send whose record cannot be written: its
// own words reach the agent, since they are about the text or the
// recipients, never about a path.
func unwritable(err error) error {
	switch {
	case errors.Is(err, errTermsChangedMeanwhile):
		return &UserError{Code: ErrorTermsChanged, Msg: "the room's members changed while the send was asked; ask again"}
	case errors.Is(err, errReplacedMeanwhile):
		return ErrNotAllowed
	}
	var ue *UserError
	if errors.As(err, &ue) {
		return recordRefusal{ue}
	}
	return err
}

// recordRefusal is a user error about the record, not a path: surfaces
// pass it on as it is (cannotAttachErr).
type recordRefusal struct{ *UserError }

func (r recordRefusal) Unwrap() error { return r.UserError }

var errReplacedMeanwhile = errors.New("files: the replaced version changed before approval")

// errTermsChangedMeanwhile: the room's membership is not the one the
// approved terms counted for "everyone".
var errTermsChangedMeanwhile = errors.New("files: the recipients changed before publication")
