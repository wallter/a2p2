package node

// The surfaces of attachments (PROTOCOL 10.6): what the local API, the CLI
// and the page call. Each is one call into Files or the store; the rules
// are Files'.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// EntryRequest is the entry that tells the agent that asked for a send
// with files what became of it (PROTOCOL 10.6).
const EntryRequest = "request"

// States of a request entry.
const (
	RequestSent     = "sent"
	RequestDeclined = "declined"
)

// requestMark is the shown key that records that an agent confirmed the
// request entry of request id (store.Shown; never a message key).
func requestMark(id string) string { return "q:" + id }

var errNoFiles = codedErr(ErrorUnavailable, "files are not available on this daemon")

// FileSent is the result of a send with files: nothing is written yet.
type FileSent struct {
	FileSendResult
	Label string // the room's label
}

// SendFiles is Send with files: it stages them and raises a request for
// this machine's person [F6]. A replacement takes only new text and files.
func (s *Surface) SendFiles(ctx context.Context, in SendInput, files []string) (FileSent, error) {
	if s.r.files == nil {
		return FileSent{}, errNoFiles
	}
	label, err := s.label(in.Agent)
	if err != nil {
		return FileSent{}, err
	}
	if len(in.Choices) > 0 {
		return FileSent{}, userErr("a send with files carries no choices")
	}
	if strings.TrimSpace(in.Text) == "" {
		return FileSent{}, userErr("text must not be empty: a send with files has text, and a withdraw has no files")
	}
	var room, ref string
	switch {
	case in.Room != "":
		room, err = s.ResolveRoom(ctx, in.Room)
	case in.Replaces != "":
		ref = in.Replaces
		room, err = s.roomOf(ctx, ref)
	case in.ReplyTo != "":
		ref = in.ReplyTo
		room, err = s.roomOf(ctx, ref)
	default:
		room = s.local
	}
	if errors.Is(err, ErrNotHeld) {
		return FileSent{}, codedErr(ErrorNotFound, "no message %s is held here: check the id (inbox and its thread view show ids)", ref)
	}
	if err != nil {
		return FileSent{}, err
	}
	d := Draft{Room: room, Agent: label, Text: in.Text, ReplyTo: in.ReplyTo, AskHandled: in.AskHandled}
	if in.Replaces != "" {
		if in.ReplyTo != "" || len(in.To) > 0 || in.AskHandled {
			return FileSent{}, userErr("replaces takes only new text and files: the addressees and parent stay as they were")
		}
	} else if len(in.To) == 1 && in.To[0] == Everyone {
		d.Everyone = true
	} else if len(in.To) > 0 {
		if d.To, err = s.addressees(room, in.To); err != nil {
			return FileSent{}, err
		}
	} else if in.Thread && in.ReplyTo != "" {
		if d.To, _, err = s.participants(ctx, room, label, in.ReplyTo); err != nil {
			return FileSent{}, err
		}
	}
	// Checks that read nothing on disk keep their own words.
	if len(files) > core.MaxFiles {
		return FileSent{}, codedErr(ErrorTooLarge, "at most %d files", core.MaxFiles)
	}
	for _, p := range files {
		if !strings.HasPrefix(p, KeptPrefix) && !filepath.IsAbs(p) {
			return FileSent{}, userErr("a file is an absolute path, or sha256:<hex> to keep one of the version replaced")
		}
	}
	res, err := s.r.files.RequestSend(ctx, FileSend{Draft: d, Replaces: in.Replaces, Files: files})
	if err != nil {
		return FileSent{}, cannotAttachErr(err, files)
	}
	out := FileSent{FileSendResult: res}
	out.Label, err = s.roomLabel(ctx, room)
	return out, err
}

// CannotAttach is the one refusal of a send whose paths cannot be staged,
// whatever the reason (missing, unreadable, not a regular file, too large,
// over a budget, the same bytes twice): an agent that asked learns
// nothing more about a path than that (DESIGN 12, limits).
const CannotAttach = "cannot attach that path: not a readable regular file within the limits"

// cannotAttachErr turns a refusal of staging into CannotAttach when the
// send names a path. The reason goes to the debug log as the
// store.FileError's op and code, or the refusal's code; never its text,
// a path or a file name.
func cannotAttachErr(err error, files []string) error {
	paths := false
	for _, p := range files {
		paths = paths || !strings.HasPrefix(p, KeptPrefix)
	}
	var fe *store.FileError
	var ue *UserError
	var rr recordRefusal
	switch {
	case errors.As(err, &rr):
		return rr.UserError // about the text or the recipients, never a path [L3]
	case !paths:
		return err
	case errors.As(err, &fe):
		slog.Debug("send with files refused", "op", fe.Op, "code", fe.Code)
	case errors.As(err, &ue) && (ue.Code == ErrorInvalid || ue.Code == ErrorTooLarge || ue.Code == ""):
		slog.Debug("send with files refused", "code", ue.CodeOf())
	default:
		return err
	}
	return &UserError{Code: ErrorInvalid, Msg: CannotAttach}
}

// SnapshotFile is a new private file under the home for the snapshot a
// page download serves; a start removes one a crash left behind.
func (s *Surface) SnapshotFile() (*os.File, error) { return s.m.st.SnapshotFile() }

// Fetch is an agent's fetch of one file (PROTOCOL 10.6): verified bytes
// copied under files/<room>/ for it, never opened. room and msg may be
// empty; wait is at most two minutes.
func (s *Surface) Fetch(ctx context.Context, agent, room, sha, msg string, wait time.Duration) (FetchResult, error) {
	if s.r.files == nil {
		return FetchResult{}, errNoFiles
	}
	label, err := s.label(agent)
	if err != nil {
		return FetchResult{}, err
	}
	if room != "" {
		if room, err = s.ResolveRoom(ctx, room); err != nil {
			return FetchResult{}, err
		}
	}
	if wait < 0 || wait > time.Duration(core.MaxFetchWaitMS)*time.Millisecond {
		return FetchResult{}, userErr("wait is 0 to 120 seconds")
	}
	if !core.ValidSHA256(sha) {
		return FetchResult{}, userErr("sha256 is 64 lowercase hex: the file's sha256 as inbox shows it")
	}
	return s.r.files.fetchWaiting(ctx, FetchOpts{Agent: label, Room: room, SHA256: sha, Message: msg, Wait: wait})
}

// fetchPoll is how often a fetch that waits asks again while the file is
// coming: a carrier is asked again, and bytes committed here wake it
// sooner.
var fetchPoll = 2 * time.Second

// fetchWaiting is Fetch, asked again while the answer is coming or partial
// and o.Wait lasts: the bytes may arrive by an upload from an author that
// only dials out (PROTOCOL 10.4) or by a member that carries them (10.5),
// and the agent's wait covers that as it covers a download.
func (f *Files) fetchWaiting(ctx context.Context, o FetchOpts) (FetchResult, error) {
	deadline := time.Now().Add(o.Wait)
	for {
		// Taken before the look, so bytes committed after it wake.
		var wake <-chan struct{}
		if o.Room != "" {
			wake = f.m.wakes.wait(o.Room)
		}
		r, err := f.Fetch(ctx, o)
		left := time.Until(deadline)
		if err != nil || r.State != FetchComing && r.State != FetchPartial || left <= 0 {
			return r, err
		}
		if wake == nil {
			wake = f.m.wakes.wait(r.Room)
		}
		o.Wait = left
		select {
		case <-wake:
		case <-time.After(min(fetchPoll, left)):
		case <-ctx.Done():
			return r, nil
		}
	}
}

// RoomFilesView is a room's files setting and what its files take.
type RoomFilesView struct {
	Room   string
	Mode   string // off, carry or keep
	Held   int64  // bytes held, partial, staged, copied and reserved in the room
	Budget int64  // the room's budget
}

// RoomFiles reads room's files setting and, with mode, first sets it: a
// person's decision, not an agent's (no agent tool reaches it).
func (s *Surface) RoomFiles(ctx context.Context, room, mode string) (RoomFilesView, error) {
	id, err := s.ResolveRoom(ctx, room)
	if err != nil {
		return RoomFilesView{}, err
	}
	switch mode {
	case "":
	case store.FilesOff, store.FilesCarry, store.FilesKeep:
		if s.r != nil {
			if err := s.r.setAsideErr(id); err != nil {
				return RoomFilesView{}, err
			}
		}
		if err := s.m.st.SetFileMode(ctx, id, mode, s.m.now()); err != nil {
			return RoomFilesView{}, err
		}
	default:
		return RoomFilesView{}, userErr("the files setting is off, carry or keep")
	}
	v := RoomFilesView{Room: id}
	v.Budget, _ = s.m.st.FileBudgets()
	err = s.m.st.View(ctx, func(rd *store.Reader) (err error) {
		if v.Mode, err = rd.FileMode(id); err != nil {
			return err
		}
		v.Held, _, err = rd.FileUsage(id)
		return err
	})
	return v, err
}

// FilesUsage is what files take on the whole machine.
type FilesUsage struct {
	Held   int64
	Budget int64
	// Unknown counts files under blobs/ whose names a2p2 does not
	// generate: the last start left them alone.
	Unknown int64
}

// filesStatus is the machine's file usage and each room's setting.
func (s *Surface) filesStatus(ctx context.Context, rooms []RoomInfo) (FilesUsage, map[string]RoomFilesView, error) {
	var u FilesUsage
	per := map[string]RoomFilesView{}
	rb, mb := s.m.st.FileBudgets()
	u.Budget, u.Unknown = mb, s.m.st.UnknownFiles()
	err := s.m.st.View(ctx, func(rd *store.Reader) error {
		for _, r := range rooms {
			v := RoomFilesView{Room: r.ID, Budget: rb}
			var err error
			if v.Mode, err = rd.FileMode(r.ID); err != nil {
				return err
			}
			if v.Held, u.Held, err = rd.FileUsage(r.ID); err != nil {
				return err
			}
			per[r.ID] = v
		}
		if len(rooms) == 0 {
			var err error
			_, u.Held, err = rd.FileUsage("")
			return err
		}
		return nil
	})
	return u, per, err
}

// TraceFile is one file of a message's winning version, as trace shows it.
type TraceFile struct {
	SHA256  string
	Name    string
	Size    int64
	State   string // as an entry's file state
	Holders []FetchHolder
}

// traceFiles lists the files of msg's winning version with what this
// machine holds and what each member last answered.
func (s *Surface) traceFiles(ctx context.Context, room, msg string) ([]TraceFile, error) {
	var out []TraceFile
	err := s.m.st.View(ctx, func(rd *store.Reader) error {
		mf, err := rd.MessageFiles(room, msg, s.m.ID, s.m.now())
		if err != nil || mf.Gone || mf.Winner == nil || !mf.Winner.Keep {
			return err
		}
		for _, it := range mf.Winner.Items {
			tf := TraceFile{SHA256: it.SHA256, Name: it.Name, Size: it.Size, State: itemState(rd, room, msg, it.SHA256)}
			obs, err := rd.FileObs(room, msg, it.SHA256)
			if err != nil {
				return err
			}
			for _, ob := range obs {
				tf.Holders = append(tf.Holders, FetchHolder{Node: ob.Peer, At: ob.At.UTC().Format(time.RFC3339), Answer: ob.Answer})
			}
			out = append(out, tf)
		}
		return nil
	})
	return out, err
}

// ---- the request a person decides ----

// SendView is a send with files as its person is shown it before deciding
// (PROTOCOL 10.6, DESIGN 12 item 8). Paths, names and lines are escaped
// for display (EscapeDisplay).
type SendView struct {
	Agent        string   // agent id
	To           []string // agent ids; empty with Everyone
	Everyone     bool
	Parent       string // a reply without To goes to the parent's author
	ParentAuthor string // that author's agent id, when the parent is held
	Members      int    // the room's members when the agent asked, for a post to everyone
	Replaces     string
	Base         string // the version of Replaces the replacement replaces
	ReplacesLine string // the first line of the replaced message as held now
	TextLine     string
	// Body is the whole text, each line escaped as EscapeDisplay does
	// and joined by "\n", so that a person reads every line approved.
	Body  string
	Files []SendFileView
	Terms string
}

// SendFileView is one file of a send: Path as resolved from the opened
// file, or "sha256:<hex>" for a file kept from the replaced version.
type SendFileView struct {
	Path   string
	Name   string
	Size   int64
	SHA256 string
	Type   string
	Kept   bool
}

// describeSend fills v for a send with files.
func (s *Surface) describeSend(ctx context.Context, q store.Request, v *RequestView) error {
	var d store.SendDraft
	if err := json.Unmarshal([]byte(q.Draft), &d); err != nil {
		return err
	}
	// The recipients are the ones the terms bind, as the draft holds them
	// (for a replacement, the replaced message's addressees): no
	// addressee is everyone, a reply sent to everyone included [L2].
	sv := &SendView{Agent: s.agentID(d.Agent, s.m.ID), Everyone: d.Everyone || len(d.To) == 0,
		Parent: d.Parent, Replaces: d.Replaces, Base: d.Base, TextLine: EscapeDisplay(firstLineOf(d.Text)),
		Body: EscapeDisplayLines(d.Text), Terms: q.Terms}
	for _, a := range d.To {
		sv.To = append(sv.To, AgentID(a))
	}
	// The member count the terms were made with, not the live one: the
	// person approves "everyone" as that set of members.
	sv.Members = d.Members
	_ = s.m.view(ctx, q.Room, func(rv *roomView) error {
		if d.Replaces != "" {
			// A replacement keeps its message's parent: show it.
			h, err := rv.get(d.Replaces)
			if err != nil || h == nil {
				return err
			}
			sv.Parent = h.f.Parent
			t, _, _, err := rv.currentText(h)
			if err == nil && t != nil {
				sv.ReplacesLine = EscapeDisplay(firstLineOf(t.f.Text))
			}
			if err != nil {
				return err
			}
		}
		if sv.Parent != "" && len(sv.To) == 0 && !sv.Everyone {
			if p, err := rv.get(sv.Parent); err == nil && p != nil {
				sv.ParentAuthor = s.agentID(p.f.Agent, p.author)
			}
		}
		return nil
	})
	var total int64
	for _, x := range d.Files {
		f := SendFileView{Path: EscapeDisplay(x.Path), Name: EscapeDisplay(x.Name), Size: x.Size, SHA256: x.SHA256, Type: x.Type, Kept: x.Kept}
		if x.Kept {
			f.Path = KeptPrefix + x.SHA256
		}
		total += x.Size
		sv.Files = append(sv.Files, f)
	}
	v.Send = sv
	v.Text = sendText(sv, v.RoomLabel, q.Room, total)
	return nil
}

// sendText says what approving a send with files does.
func sendText(sv *SendView, label, room string, total int64) string {
	files := strconv.Itoa(len(sv.Files)) + " files"
	if len(sv.Files) == 1 {
		files = "1 file"
	}
	// The label and agent ids can come from other machines: each is
	// escaped and isolated, as in requestText.
	where := "room " + Isolate(EscapeDisplay(label)) + " (" + room + ")"
	agent := func(id string) string { return Isolate(EscapeDisplay(id)) }
	to := "everyone in " + where + " (" + strconv.Itoa(sv.Members) + " members)"
	switch {
	case len(sv.To) > 0:
		ids := make([]string, len(sv.To))
		for i, a := range sv.To {
			ids[i] = agent(a)
		}
		to = strings.Join(ids, ", ") + " in " + where
	case !sv.Everyone && sv.ParentAuthor != "":
		to = agent(sv.ParentAuthor) + ", the author of message " + sv.Parent + ", in " + where
	case !sv.Everyone:
		to = "the author of message " + sv.Parent + " in " + where
	}
	if sv.Replaces != "" {
		msg := sv.Replaces
		if sv.Base != "" {
			msg += " (version " + sv.Base + ")"
		}
		return fmt.Sprintf("replace message %s, which goes to %s, by agent %s, with new text and %s (%d bytes); "+
			"the replacement has only the files listed", msg, to, agent(sv.Agent), files, total)
	}
	return fmt.Sprintf("send a message by agent %s to %s with %s (%d bytes): the files are the bytes copied when the agent asked",
		agent(sv.Agent), to, files, total)
}

func firstLineOf(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	if utf8.RuneCountInString(line) > firstLineRunes {
		line = string([]rune(line)[:firstLineRunes]) + "..."
	}
	return line
}

// ---- telling the agent that asked ----

// requestEntries are agent's sends with files that a person decided and
// the agent has not confirmed: kind request, state sent (ID the message)
// or declined (with the reason), in room or every room.
func (s *Surface) requestEntries(ctx context.Context, label, room string) ([]Entry, error) {
	qs, err := s.m.st.Requests(ctx, room, "")
	if err != nil {
		return nil, err
	}
	var out []Entry
	told := map[string]map[string]string{}
	for _, q := range qs {
		if q.Kind != store.RequestSendFiles || q.State == store.RequestPending {
			continue
		}
		var d store.SendDraft
		if json.Unmarshal([]byte(q.Draft), &d) != nil || d.Agent != label {
			continue
		}
		if told[q.Room] == nil {
			var rs map[string]string
			if err := s.m.st.View(ctx, func(rd *store.Reader) (err error) {
				rs, err = rd.Receipts(label, q.Room)
				return err
			}); err != nil {
				return nil, err
			}
			told[q.Room] = rs
		}
		if _, ok := told[q.Room][requestMark(q.ID)]; ok {
			continue
		}
		e := Entry{Key: q.ID, Kind: EntryRequest, ID: q.ID, Room: q.Room, From: s.agentID(label, s.m.ID), FromNode: s.m.ID,
			Created: q.DecidedAt, Request: q.ID, RequestState: RequestDeclined, RequestReason: q.Reason}
		if q.State == store.RequestApproved {
			e.RequestState, e.RequestReason, e.ID = RequestSent, "", q.Entries
			if d.Replaces != "" {
				e.ID = d.Replaces
			}
		} else if q.State == store.RequestFailed && q.Reason == "" {
			e.RequestReason = "failed"
		}
		out = append(out, e)
	}
	return out, nil
}

// seenRequest confirms agent's request entry id: it is not listed again.
func (s *Surface) seenRequest(ctx context.Context, label, id string) (SeenResult, error) {
	res := SeenResult{Key: id, Reason: SeenNotShown}
	q, err := s.m.st.GetRequest(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	var d store.SendDraft
	if q.Kind != store.RequestSendFiles || q.State == store.RequestPending ||
		json.Unmarshal([]byte(q.Draft), &d) != nil || d.Agent != label {
		return res, nil
	}
	if err := s.m.st.Show(ctx, label, q.Room, []store.Shown{{Key: requestMark(id), Msg: id}}); err != nil {
		return res, err
	}
	return SeenResult{Key: id, Confirmed: true}, nil
}
