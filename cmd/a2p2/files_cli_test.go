package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// daemonForCLI is one machine with attachments behind a real local API,
// for commands run as a person would run them.
type daemonForCLI struct {
	s     *node.Surface
	rooms *node.Rooms
	cl    *localapi.Client
}

func newDaemonForCLI(t *testing.T) *daemonForCLI {
	t.Helper()
	return newDaemonForCLIWith(t, nil)
}

// newDaemonForCLIWith is newDaemonForCLI with wrap around the local API's
// handler.
func newDaemonForCLIWith(t *testing.T, wrap func(http.Handler) http.Handler) *daemonForCLI {
	t.Helper()
	return newDaemonForCLIInfo(t, wrap, localapi.Info{Version: "t"})
}

// newDaemonForCLIInfo is newDaemonForCLIWith whose local API reports info.
func newDaemonForCLIInfo(t *testing.T, wrap func(http.Handler) http.Handler, info localapi.Info) *daemonForCLI {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := node.NewMachine(st, ed25519.NewKeyFromSeed(append([]byte{5}, make([]byte, 31)...)), node.MachineOpts{})
	rooms, err := node.OpenRooms(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.NewFiles(rooms, nil, node.FilesOpts{}).Start(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := node.OpenSurface(ctx, m, rooms, node.SurfaceOpts{Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	base := "/tmp"
	if runtime.GOOS == "windows" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "a2p2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	h := localapi.Handler(s, info)
	if wrap != nil {
		h = wrap(h)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	d := &daemonForCLI{s: s, rooms: rooms, cl: localapi.NewClient(filepath.Join(dir, "s"))}
	if _, err := s.CreateRoom(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	return d
}

// run runs one command against d with stdin answering asks, at a
// terminal or not, and returns what it wrote to stdout and stderr.
func (d *daemonForCLI) run(t *testing.T, atTerminal bool, answers string, name string, args ...string) (string, string, error) {
	t.Helper()
	oldTerm, oldIn, oldOut, oldErr := terminal, stdin, os.Stdout, os.Stderr
	t.Cleanup(func() { terminal, stdin, os.Stdout, os.Stderr = oldTerm, oldIn, oldOut, oldErr })
	terminal = func() bool { return atTerminal }
	stdin = bufio.NewReader(strings.NewReader(answers))
	ro, wo, _ := os.Pipe()
	re, we, _ := os.Pipe()
	os.Stdout, os.Stderr = wo, we
	outc, errc := make(chan string), make(chan string)
	go func() { b, _ := io.ReadAll(ro); outc <- string(b) }()
	go func() { b, _ := io.ReadAll(re); errc <- string(b) }()
	c := newCmd(context.Background(), name)
	c.fs.SetOutput(io.Discard)
	c.cl = d.cl
	var err error
	switch name {
	case "send":
		err = send(c, args)
	case "room":
		err = room(c, args)
	case "confirm":
		err = confirm(c, args)
	case "status":
		if err = c.fs.Parse(args); err == nil {
			err = status(c)
		}
	case "inbox":
		err = inbox(c, args)
	case "ack":
		err = ack(c, args)
	case "history":
		err = history(c, args)
	case "net":
		err = netCmd(c, args)
	case "ui":
		err = uiCmd(c, args)
	default:
		t.Fatalf("no command %s here", name)
	}
	wo.Close()
	we.Close()
	out, serr := <-outc, <-errc
	ro.Close()
	re.Close()
	terminal, stdin, os.Stdout, os.Stderr = oldTerm, oldIn, oldOut, oldErr
	return out, serr, err
}

func (d *daemonForCLI) mode(t *testing.T) string {
	t.Helper()
	v, err := d.s.RoomFiles(context.Background(), "team", "")
	if err != nil {
		t.Fatal(err)
	}
	return v.Mode
}

// Finding 1: changing a room's files setting is a person's decision,
// asked at a terminal; reading it needs none.
func TestRoomFilesChangesTheSettingOnlyAtATerminalAfterAYes(t *testing.T) {
	d := newDaemonForCLI(t)
	if out, _, err := d.run(t, false, "", "room", "files", "team"); err != nil || !strings.Contains(out, "files carry") {
		t.Fatalf("read without a terminal: %q %v", out, err)
	}
	if _, _, err := d.run(t, false, "yes\n", "room", "files", "team", "keep"); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("change without a terminal: %v", err)
	}
	if d.mode(t) != store.FilesCarry {
		t.Fatal("changed without a terminal")
	}
	if _, serr, err := d.run(t, true, "no\n", "room", "files", "team", "keep"); err == nil || !strings.Contains(serr, "fetches and keeps every file") {
		t.Fatalf("change answered no: %q %v", serr, err)
	}
	if d.mode(t) != store.FilesCarry {
		t.Fatal("changed without a yes")
	}
	if out, _, err := d.run(t, true, "yes\n", "room", "files", "team", "keep"); err != nil || !strings.Contains(out, "files keep") {
		t.Fatalf("change at a terminal: %q %v", out, err)
	}
	if d.mode(t) != store.FilesKeep {
		t.Fatal("not changed after a yes at a terminal")
	}
}

// Finding 2 and 9: a2p2 send --file answers with the request alone and
// a relative path is sent as an absolute one; a path that cannot be
// staged gets the one refusal.
func TestSendWithAFilePrintsTheRequestAloneAndSendsAnAbsolutePath(t *testing.T) {
	d := newDaemonForCLI(t)
	dir, err := filepath.EvalSymlinks(t.TempDir()) // the daemon shows the path resolved
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build.log"), []byte("line one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	out, _, err := d.run(t, true, "", "send", "--room", "team", "--file", "build.log", "the log")
	if err != nil || !strings.Contains(out, " waiting: a person must confirm") || strings.Contains(out, "build.log") || strings.Contains(out, "bytes") {
		t.Fatalf("send: %q %v", out, err)
	}
	qs, err := d.s.Waiting(context.Background())
	if err != nil || len(qs) != 1 || qs[0].Send == nil || len(qs[0].Send.Files) != 1 || qs[0].Send.Files[0].Path != filepath.Join(dir, "build.log") {
		t.Fatalf("request: %+v %v", qs, err)
	}
	if _, _, err := d.run(t, true, "", "send", "--room", "team", "--file", "missing.log", "x"); err == nil || !strings.Contains(err.Error(), node.CannotAttach) {
		t.Fatalf("missing file: %v", err)
	}
}

// Findings 5 and 9: the approval of a replacement, rendered by a2p2
// confirm from a real request, shows the recipients it keeps, the version
// it replaces and every line of the new text escaped, and the approval
// carries the terms shown.
func TestConfirmRendersAReplacementWithItsRecipientsVersionAndWholeText(t *testing.T) {
	d := newDaemonForCLI(t)
	ctx := context.Background()
	alice, err := d.s.Register(ctx, "test", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := d.s.Register(ctx, "test", "bob", "")
	if err != nil {
		t.Fatal(err)
	}
	aliceID, bobID := alice.Label+"@"+d.s.HumanID()[len("human@"):], bob.Label+"@"+d.s.HumanID()[len("human@"):]
	p := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := d.s.SendFiles(ctx, node.SendInput{Agent: aliceID, Room: "team", To: []string{bobID}, Text: "first"}, []string{p})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.run(t, true, "yes\n", "confirm", first.Request); err != nil {
		t.Fatal(err)
	}
	qs, _ := d.s.Waiting(ctx)
	if len(qs) != 0 {
		t.Fatalf("still waiting: %+v", qs)
	}
	in, err := d.s.Inbox(ctx, bobID, "team", 0, 0)
	if err != nil || len(in.Entries) != 1 {
		t.Fatalf("bob's inbox: %+v %v", in, err)
	}
	msg := in.Entries[0].ID
	second, err := d.s.SendFiles(ctx, node.SendInput{Agent: aliceID, Replaces: msg, Text: "new first line\nsecond \u202eline\nthird"}, []string{p})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := d.run(t, false, "", "confirm")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"to        " + bobID, "replaces  " + msg + `: "first"`, "its version " + msg,
		"keeps its recipients", "text      | new first line\n", `          | second \u{202e}line` + "\n", "          | third\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.ContainsRune(out, 0x202e) {
		t.Fatalf("an unescaped bidirectional character: %q", out)
	}
	if _, _, err := d.run(t, true, "yes\n", "confirm", second.Request); err != nil {
		t.Fatalf("approve with the terms shown: %v", err)
	}
}
