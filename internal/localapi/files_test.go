package localapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// filesMachine is one machine with attachments and its local API, and
// two registered agents, alice and bob.
type filesMachine struct {
	machine
	alice, bob string
}

func newFilesMachine(t *testing.T) *filesMachine {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	key := ed25519.NewKeyFromSeed(append([]byte{7}, make([]byte, 31)...))
	mach := node.NewMachine(st, key, node.MachineOpts{})
	rooms, err := node.OpenRooms(ctx, mach)
	if err != nil {
		t.Fatal(err)
	}
	files := node.NewFiles(rooms, nil, node.FilesOpts{})
	if err := files.Start(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := node.OpenSurface(ctx, mach, rooms, node.SurfaceOpts{Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(sockTemp(), "a2p2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: Handler(s, Info{Version: "t"})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	m := &filesMachine{machine: machine{id: mach.ID, s: s, c: NewClient(filepath.Join(dir, "s")), st: st}}
	m.alice = call[store.Agent](t, m.c, "register", RegisterReq{Harness: "test", Label: "alice"}).ID
	m.bob = call[store.Agent](t, m.c, "register", RegisterReq{Harness: "test", Label: "bob"}).ID
	call[Room](t, m.c, "room/create", RoomReq{Room: "team"})
	return m
}

// writeFile writes body to a new file named name and returns its path.
func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func sha(body string) string {
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:])
}

// requestEntry is the request entry of inbox, or nil.
func requestEntry(es []Entry) *Entry {
	for i := range es {
		if es[i].Kind == node.EntryRequest {
			return &es[i]
		}
	}
	return nil
}

// approve approves request id with the terms requests shows for it.
func (m *filesMachine) approve(t *testing.T, id string) ConfirmResp {
	t.Helper()
	for _, q := range call[RequestsResp](t, m.c, "requests", Empty{}).Requests {
		if q.ID == id {
			return call[ConfirmResp](t, m.c, "confirm", ConfirmReq{ID: id, Approve: true, Terms: q.Terms})
		}
	}
	t.Fatalf("no request %s", id)
	return ConfirmResp{}
}

// P3, DESIGN 12: an approval of a send with files carries the terms the
// person was shown; without them, or with others, nothing is sent. A
// decline needs none.
func TestAnApprovalWithoutTheTermsShownIsRefused(t *testing.T) {
	m := newFilesMachine(t)
	w := call[SendFilesResp](t, m.c, "send", SendReq{Agent: m.alice, Room: "team", Text: "x", Files: []string{writeFile(t, "a.txt", "abc")}})
	q := call[RequestsResp](t, m.c, "requests", Empty{}).Requests[0]
	for _, terms := range []string{"", strings.Repeat("0", 64), strings.ToUpper(q.Terms)} {
		userError(t, m.c, "confirm", ConfirmReq{ID: w.Request, Approve: true, Terms: terms})
	}
	if qs := call[RequestsResp](t, m.c, "requests", Empty{}); len(qs.Requests) != 1 {
		t.Fatalf("a refused approval decided the request: %+v", qs)
	}
	if c := m.approve(t, w.Request); c.State != store.RequestApproved {
		t.Fatalf("approve with the terms shown = %+v", c)
	}
	w = call[SendFilesResp](t, m.c, "send", SendReq{Agent: m.alice, Room: "team", Text: "y", Files: []string{writeFile(t, "b.txt", "b")}})
	if c := call[ConfirmResp](t, m.c, "confirm", ConfirmReq{ID: w.Request}); c.State != store.RequestDeclined {
		t.Fatalf("decline = %+v", c)
	}
}

// L1, P3: an agent's send with a file waits for the person; requests
// shows every term; approval writes the message; a reader sees the file
// and fetches a verified copy; the agent that asked is told it was sent.
func TestASendWithAFileWaitsForThePersonAndTheReaderFetchesIt(t *testing.T) {
	m := newFilesMachine(t)
	body := "line one\nline two\n"
	p := writeFile(t, "build.log", body)
	var w SendFilesResp
	raw, err := m.c.CallRaw(context.Background(), "send", SendReq{Agent: m.alice, Room: "team", Text: "the log", Files: []string{p}}, &w)
	if err != nil || w.State != "waiting" || w.Request == "" {
		t.Fatalf("send = %+v %v", w, err)
	}
	// the agent that asked learns the request and nothing of the file
	if want := `{"request":"` + w.Request + `","state":"waiting"}`; strings.TrimSpace(string(raw)) != want {
		t.Fatalf("send answered %s, want %s", raw, want)
	}
	if in := call[InboxResp](t, m.c, "inbox", InboxReq{Agent: m.bob}); len(in.Entries) != 0 {
		t.Fatalf("nothing is written before approval: %+v", in.Entries)
	}
	qs := call[RequestsResp](t, m.c, "requests", Empty{})
	if len(qs.Requests) != 1 {
		t.Fatalf("requests = %+v", qs)
	}
	q := qs.Requests[0]
	if q.Kind != store.RequestSendFiles || q.Agent != m.alice || q.TextLine != "the log" || len(q.To) != 1 || q.To[0] != "everyone" ||
		q.Members != 1 || len(q.Terms) != 64 || len(q.Files) != 1 || q.Deadline == "" || !strings.Contains(q.Text, "1 file") {
		t.Fatalf("request = %+v", q)
	}
	if f := q.Files[0]; !strings.HasSuffix(f.Path, string(filepath.Separator)+"build.log") || !filepath.IsAbs(f.Path) ||
		f.Size != int64(len(body)) || f.SHA256 != sha(body) {
		t.Fatalf("request file = %+v", f)
	}
	c := call[ConfirmResp](t, m.c, "confirm", ConfirmReq{ID: q.ID, Approve: true, Terms: q.Terms})
	if c.State != store.RequestApproved || c.Effect == "" {
		t.Fatalf("confirm = %+v", c)
	}
	in := call[InboxResp](t, m.c, "inbox", InboxReq{Agent: m.bob})
	if len(in.Entries) != 1 || in.Entries[0].ID != c.Effect || len(in.Entries[0].Files) != 1 {
		t.Fatalf("inbox = %+v", in.Entries)
	}
	if f := in.Entries[0].Files[0]; f.Name != "build.log" || f.SHA256 != sha(body) || f.State != node.FileStateHere {
		t.Fatalf("entry file = %+v", f)
	}
	userError(t, m.c, "fetch", FetchReq{Agent: m.bob, SHA256: "nothex"})
	userError(t, m.c, "fetch", FetchReq{Agent: m.bob, SHA256: sha(body), WaitMS: 200000})
	userError(t, m.c, "fetch", FetchReq{Agent: m.bob, SHA256: sha("not listed")})
	fr := call[FetchResp](t, m.c, "fetch", FetchReq{Agent: m.bob, Room: "team", SHA256: sha(body)})
	if fr.State != node.FetchHere || fr.Size != int64(len(body)) || fr.Message != c.Effect {
		t.Fatalf("fetch = %+v", fr)
	}
	if got, err := os.ReadFile(fr.Path); err != nil || !bytes.Equal(got, []byte(body)) {
		t.Fatalf("fetched copy %s: %q, %v", fr.Path, got, err)
	}
	tr := call[TraceResp](t, m.c, "trace", IDReq{ID: c.Effect})
	if len(tr.Files) != 1 || tr.Files[0].SHA256 != sha(body) || tr.Files[0].State != node.FileStateHere || tr.Files[0].Holders == nil {
		t.Fatalf("trace files = %+v", tr.Files)
	}
	// the agent that asked is told, once confirmed
	ia := call[InboxResp](t, m.c, "inbox", InboxReq{Agent: m.alice})
	r := requestEntry(ia.Entries)
	if r == nil || r.State != node.RequestSent || r.ID != c.Effect || r.Request != q.ID || r.Key != q.ID {
		t.Fatalf("request entry = %+v", ia.Entries)
	}
	sr := call[SeenResp](t, m.c, "seen", SeenReq{Agent: m.alice, Entries: []SeenEntry{{Key: r.Key}}})
	if len(sr.Results) != 1 || !sr.Results[0].OK {
		t.Fatalf("seen = %+v", sr)
	}
	if ia := call[InboxResp](t, m.c, "inbox", InboxReq{Agent: m.alice}); requestEntry(ia.Entries) != nil {
		t.Fatalf("a confirmed request entry is shown again: %+v", ia.Entries)
	}
	// bob may not confirm alice's request entry
	if sr := call[SeenResp](t, m.c, "seen", SeenReq{Agent: m.bob, Entries: []SeenEntry{{Key: q.ID}}}); sr.Results[0].OK {
		t.Fatalf("another agent confirmed the request entry: %+v", sr)
	}
}

// L1: a room-wide message with a file shown in history may have its file
// fetched; showing it does not let the reader report it handled. (A
// reader on another machine: node's
// TestOnAReadersMachineAFileShownInHistoryOrSkippedMayBeFetched.)
func TestAFileShownInHistoryMayBeFetchedButNotReportedHandled(t *testing.T) {
	m := newFilesMachine(t)
	w := call[SendFilesResp](t, m.c, "send", SendReq{Agent: m.alice, Room: "team", Text: "for all", Files: []string{writeFile(t, "a.txt", "abc")}})
	c := m.approve(t, w.Request)
	h := call[Entries](t, m.c, "history", HistoryReq{Agent: m.bob, Room: "team"})
	if len(h.Entries) != 1 || len(h.Entries[0].Files) != 1 {
		t.Fatalf("history = %+v", h.Entries)
	}
	if fr := call[FetchResp](t, m.c, "fetch", FetchReq{Agent: m.bob, SHA256: sha("abc")}); fr.State != node.FetchHere {
		t.Fatalf("fetch after history = %+v", fr)
	}
	userError(t, m.c, "ack", AckReq{Agent: m.bob, ID: c.Effect})
}

// P3: a declined send writes nothing and the agent that asked is told.
func TestADeclinedSendWithAFileTellsTheAgentThatAsked(t *testing.T) {
	m := newFilesMachine(t)
	w := call[SendFilesResp](t, m.c, "send", SendReq{Agent: m.alice, Room: "team", Text: "x", To: []string{m.bob},
		Files: []string{writeFile(t, "a.txt", "a")}})
	q := call[RequestsResp](t, m.c, "requests", Empty{}).Requests[0]
	if w.Request != q.ID || len(q.To) != 1 || q.To[0] != m.bob || q.Members != 0 {
		t.Fatalf("request names its addressee: %+v", q)
	}
	if c := call[ConfirmResp](t, m.c, "confirm", ConfirmReq{ID: q.ID}); c.State != store.RequestDeclined {
		t.Fatalf("decline = %+v", c)
	}
	if in := call[InboxResp](t, m.c, "inbox", InboxReq{Agent: m.bob}); len(in.Entries) != 0 {
		t.Fatalf("a declined send was written: %+v", in.Entries)
	}
	r := requestEntry(call[InboxResp](t, m.c, "inbox", InboxReq{Agent: m.alice}).Entries)
	if r == nil || r.State != node.RequestDeclined || r.ID != q.ID {
		t.Fatalf("request entry = %+v", r)
	}
}

// L1: what a send with files refuses before anything is staged.
func TestASendWithFilesRefusesBadPathsAndTooManyFiles(t *testing.T) {
	m := newFilesMachine(t)
	userError(t, m.c, "send", SendReq{Agent: m.alice, Room: "team", Text: "x", Files: []string{"relative.txt"}})
	userError(t, m.c, "send", SendReq{Agent: m.alice, Room: "team", Text: "x", Files: []string{filepath.Join(t.TempDir(), "missing")}})
	userError(t, m.c, "send", SendReq{Agent: m.alice, Room: "team", Text: "x", Files: []string{t.TempDir()}})
	userError(t, m.c, "send", SendReq{Agent: m.alice, Room: "team", Text: "", Files: []string{writeFile(t, "a", "a")}})
	userError(t, m.c, "send", SendReq{Agent: m.alice, Room: "team", Text: "x", Choices: []string{"a"}, Files: []string{writeFile(t, "a", "a")}})
	var many []string
	for i := range 9 {
		many = append(many, writeFile(t, "f", strings.Repeat("x", i+1)))
	}
	err := m.c.Call(context.Background(), "send", SendReq{Agent: m.alice, Room: "team", Text: "x", Files: many}, nil)
	if e, ok := err.(*Error); !ok || e.Code != node.ErrorTooLarge {
		t.Fatalf("nine files: %v", err)
	}
	if qs := call[RequestsResp](t, m.c, "requests", Empty{}); len(qs.Requests) != 0 {
		t.Fatalf("a refused send raised a request: %+v", qs)
	}
}

// DESIGN 12, limits: a path that cannot be staged gets one refusal,
// whatever the reason, so an agent cannot tell a missing file from a
// directory, an oversized file or two files with the same bytes.
func TestEveryPathThatCannotBeStagedGetsTheSameRefusal(t *testing.T) {
	m := newFilesMachine(t)
	big := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(big, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, 257<<20); err != nil { // sparse: nothing is written
		t.Fatal(err)
	}
	unreadable := writeFile(t, "a.txt", "abc")
	twice := writeFile(t, "b.txt", "abc")
	for name, files := range map[string][]string{
		"missing":    {filepath.Join(t.TempDir(), "missing")},
		"directory":  {t.TempDir()},
		"too large":  {big},
		"same bytes": {unreadable, twice},
	} {
		err := m.c.Call(context.Background(), "send", SendReq{Agent: m.alice, Room: "team", Text: "x", Files: files}, nil)
		var e *Error
		if !errors.As(err, &e) || e.Code != node.ErrorInvalid || e.Msg != node.CannotAttach {
			t.Errorf("%s: %v", name, err)
		}
	}
	if qs := call[RequestsResp](t, m.c, "requests", Empty{}); len(qs.Requests) != 0 {
		t.Fatalf("a refused send raised a request: %+v", qs)
	}
}

// An internal error about a file reaches the log and the answer as its
// operation and cause, without the path.
func TestAnInternalFileErrorIsLoggedAndAnsweredWithoutItsPath(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "private-report.txt")
	_, err := os.Open(secret)
	for _, e := range []error{err, fmt.Errorf("stage: %w", err), &os.LinkError{Op: "link", Old: secret, New: secret + "2", Err: os.ErrExist}} {
		if got := scrub(e); strings.Contains(got, "private-report") || got == "" {
			t.Errorf("scrub(%v) = %q", e, got)
		}
	}
}

// PROTOCOL 10.11: the approval display shows the path in full with
// bidirectional characters escaped, and names the message replaced.
func TestTheRequestEscapesBidiCharactersInThePathAndNamesTheReplacedMessage(t *testing.T) {
	m := newFilesMachine(t)
	first := call[SendFilesResp](t, m.c, "send", SendReq{Agent: m.alice, Room: "team", Text: "first version\nmore",
		Files: []string{writeFile(t, "a.txt", "a")}})
	c := m.approve(t, first.Request)
	// "report\u202etxt.exe" shows as "reportexe.txt" unescaped
	p := writeFile(t, "report\u202etxt.exe", "b")
	w := call[SendFilesResp](t, m.c, "send", SendReq{Agent: m.alice, Text: "second", Replaces: c.Effect,
		Files: []string{p, "sha256:" + sha("a")}})
	q := call[RequestsResp](t, m.c, "requests", Empty{}).Requests[0]
	if q.ID != w.Request {
		t.Fatalf("request %s, send answered %s", q.ID, w.Request)
	}
	if q.Replaces == nil || q.Replaces.ID != c.Effect || q.Replaces.Line != "first version" || q.TextLine != "second" ||
		!strings.Contains(q.Text, "replace message "+c.Effect) {
		t.Fatalf("request = %+v", q)
	}
	f := q.Files[0]
	if strings.ContainsRune(f.Path, 0x202e) || !strings.HasSuffix(f.Path, `report\u{202e}txt.exe`) ||
		strings.ContainsRune(f.Name, 0x202e) {
		t.Fatalf("path not escaped: %q, name %q", f.Path, f.Name)
	}
	if k := q.Files[1]; !k.Kept || k.Path != "sha256:"+sha("a") || k.Name != "a.txt" {
		t.Fatalf("kept file = %+v", k)
	}
}

func TestEscapeDisplayWritesControlsAndBidiAsEscapes(t *testing.T) {
	for in, want := range map[string]string{
		"plain/path.txt":           "plain/path.txt",
		"a\nb\tc\x1b[2J":           `a\u{a}b\u{9}c\u{1b}[2J`,
		"x\u0085y\u009b":           `x\u{85}y\u{9b}`,
		"\u200e\u200f\u061c":       `\u{200e}\u{200f}\u{61c}`,
		"\u202a\u202e\u2066\u2069": `\u{202a}\u{202e}\u{2066}\u{2069}`,
		"bad\xffbyte":              `bad\x{ff}byte`,
		"日本語.txt":                  "日本語.txt",
	} {
		if got := node.EscapeDisplay(in); got != want {
			t.Errorf("EscapeDisplay(%q) = %q, want %q", in, got, want)
		}
	}
}

// The room's files setting is a person's call; status shows the budgets
// in use.
func TestRoomFilesSetsTheSettingAndStatusShowsTheBudgets(t *testing.T) {
	m := newFilesMachine(t)
	rf := call[RoomFiles](t, m.c, "room/files", RoomFilesReq{Room: "team"})
	if rf.Mode != store.FilesCarry || rf.Budget <= 0 || rf.Held != 0 {
		t.Fatalf("room/files = %+v", rf)
	}
	if rf := call[RoomFiles](t, m.c, "room/files", RoomFilesReq{Room: "team", Mode: "keep"}); rf.Mode != store.FilesKeep {
		t.Fatalf("room/files keep = %+v", rf)
	}
	userError(t, m.c, "room/files", RoomFilesReq{Room: "team", Mode: "always"})
	userError(t, m.c, "room/files", RoomFilesReq{Room: "nowhere"})
	call[SendFilesResp](t, m.c, "send", SendReq{Agent: m.alice, Room: "team", Text: "x", Files: []string{writeFile(t, "a", "abc")}})
	st := call[Status](t, m.c, "status", StatusReq{})
	if st.Files.Budget <= 0 || st.Files.Held != 3 {
		t.Fatalf("status files = %+v", st.Files)
	}
	for _, r := range st.Rooms {
		if r.Label == "team" && (r.Files == nil || r.Files.Mode != store.FilesKeep || r.Files.Held != 3) {
			t.Fatalf("status room files = %+v", r.Files)
		}
	}
}
