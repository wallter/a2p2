package node

// Security group H, files: what may be staged (F33, F40, F23), what
// staging tells an agent (F35), what the person is told (F36) and what one
// author can reserve on a carrier (F10).

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

func (n *fnode) askSend(room string, paths ...string) (FileSendResult, error) {
	return n.f.RequestSend(context.Background(), FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "see"}, Files: paths})
}

// TestAnAgentCannotAttachTheMachineKeyOrTheDatabase (F33, F40): a send
// naming a file of the home, or a hard link to one, is refused with the
// one refusal of staging, and nothing is staged or raised.
// Requirements: L1, P3
func TestAnAgentCannotAttachTheMachineKeyOrTheDatabase(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	home := a.st.FileRoot()
	key := filepath.Join(home, "identity.key")
	if err := os.WriteFile(key, []byte("the machine key"), 0o600); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(t.TempDir(), "build.log")
	if err := os.Link(key, hard); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{key, filepath.Join(home, "a2p2.db"), hard} {
		_, err := a.askSend(room, p)
		var ue *UserError
		if err = cannotAttachErr(err, []string{p}); !errors.As(err, &ue) || ue.Msg != CannotAttach {
			t.Errorf("%s: %v", p, err)
		}
	}
	if qs, err := a.st.Requests(context.Background(), room, store.RequestPending); err != nil || len(qs) != 0 {
		t.Fatalf("requests %d %v", len(qs), err)
	}
	if n := stagingLeft(t, a); n != 0 {
		t.Fatalf("%d staged", n)
	}
	// an honest file still goes
	if _, err := a.askSend(room, writeSource(t, "report.txt", "honest bytes")); err != nil {
		t.Fatal(err)
	}
}

// TestStagingAnswersTheSameWhetherTwoFilesAreEqualOrNotWhenRequestsAreFull
// (F35): with the room's requests full, a send is refused before any path
// is touched, so equal files, different files and a missing file get the
// same answer. Below that, equal files are listed once instead of being
// refused, so the answer does not say they are equal.
// Requirements: L1, P3
func TestStagingAnswersTheSameWhetherTwoFilesAreEqualOrNotWhenRequestsAreFull(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	secret := writeSource(t, "secret.txt", "1234")
	same := writeSource(t, "guess1.txt", "1234")
	other := writeSource(t, "guess2.txt", "9999")
	r, err := a.askSend(room, secret, same)
	if err != nil || len(r.Files) != 1 {
		t.Fatalf("equal files: %+v %v", r, err)
	}
	r, err = a.askSend(room, secret, other)
	if err != nil || len(r.Files) != 2 {
		t.Fatalf("different files: %+v %v", r, err)
	}
	for i := 2; i < MaxPendingRequests; i++ {
		if _, err := a.askSend(room, writeSource(t, "f"+strconv.Itoa(i), "filler "+strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	var answers []string
	for _, paths := range [][]string{{secret, same}, {secret, other}, {filepath.Join(t.TempDir(), "missing")}} {
		_, err := a.askSend(room, paths...)
		err = cannotAttachErr(err, paths)
		var ue *UserError
		if !errors.As(err, &ue) || ue.Code != ErrorRateLimited {
			t.Fatalf("%v: %v", paths, err)
		}
		answers = append(answers, ue.Code+" "+ue.Msg)
	}
	if answers[0] != answers[1] || answers[1] != answers[2] {
		t.Fatalf("answers differ: %q", answers)
	}
}

// TestASendOfAFileWithSeveralLinksSaysSoInTheRequest (F36): the path
// shown is the name opened; a hard link has no other name, so the person
// is told the file has several.
// Requirements: L1, P3
func TestASendOfAFileWithSeveralLinksSaysSoInTheRequest(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	p := writeSource(t, "build.log", "linked bytes")
	if err := os.Link(p, filepath.Join(t.TempDir(), "other-name")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.askSend(room, p, writeSource(t, "plain.txt", "one name")); err != nil {
		t.Fatal(err)
	}
	vs, err := (&Surface{m: a.m, r: a.r}).Waiting(context.Background())
	if err != nil || len(vs) != 1 {
		t.Fatalf("%d %v", len(vs), err)
	}
	v := vs[0]
	if !strings.Contains(v.Text, "file 1 has 2 names on disk") || strings.Contains(v.Text, "file 2 has") {
		t.Fatalf("text %q", v.Text)
	}
	if v.Send.Files[0].Links != 2 || v.Send.Files[1].Links != 0 {
		t.Fatalf("files %+v", v.Send.Files)
	}
}

// carryRoom is a room where a, the author, takes no connections and b
// listens and carries; b has a's list of msg.
func carryRoom(t *testing.T, a, b *fnode, more ...*fnode) string {
	t.Helper()
	a.net.noInbound[a.id] = true
	return fileRoom(t, a, b, more...)
}

// TestAnAuthorCannotMakeACarrierReserveItsOwnFile (F10): a request for
// a file from its own author starts no carry job on a member that lacks
// it, so it reserves nothing; another member's request still does.
// Requirements: L1
func TestAnAuthorCannotMakeACarrierReserveItsOwnFile(t *testing.T) {
	ns := filesNet(t, "a", "b", "c")
	a, b, c := ns[0], ns[1], ns[2]
	room := carryRoom(t, a, b, c)
	msg, res := a.sendFiles(room, "alice", "big", writeSource(t, "big.bin", "author's bytes"))
	sha := res.Files[0].SHA256
	a.pullAll(room, b)
	b.tickFiles()
	a.pullAll(room, b) // b has the list
	ctx := context.Background()
	before := usage(t, b, room)
	if _, err := b.f.ServeGet(ctx, a.id, FileGet{Room: room, Msg: msg, SHA256: sha}); !errors.Is(err, ErrNotHeldFile) {
		t.Fatalf("the author's own ask: %v", err)
	}
	if got := usage(t, b, room); got != before {
		t.Fatalf("reserved %d for the author", got-before)
	}
	fs, err := b.f.ServeGet(ctx, c.id, FileGet{Room: room, Msg: msg, SHA256: sha})
	if err != nil || fs.Status != 202 || fs.State != FileNotYet {
		t.Fatalf("another member's ask: %+v %v", fs, err)
	}
}

func usage(t *testing.T, n *fnode, room string) int64 {
	t.Helper()
	var u int64
	if err := n.st.View(context.Background(), func(rd *store.Reader) (err error) {
		u, _, err = rd.FileUsage(room)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return u
}

// TestOneAuthorsUnansweredWantsDoNotStopThisMachinesOwnSendsWithFiles
// (F10): an author that never uploads, with a member asking for its
// files, makes a carrier reserve at most a quarter of the room budget, so
// the carrier's own sends with files still fit.
// Requirements: L1
func TestOneAuthorsUnansweredWantsDoNotStopThisMachinesOwnSendsWithFiles(t *testing.T) {
	ns := filesNet(t, "a", "b", "c")
	a, b, c := ns[0], ns[1], ns[2]
	room := carryRoom(t, a, b, c)
	b.st.SetFileBudgets(2000, 20000)
	var paths []string
	for i := 0; i < 4; i++ {
		paths = append(paths, writeSource(t, "f"+strconv.Itoa(i), strings.Repeat(strconv.Itoa(i), 400)))
	}
	msg, res := a.sendFiles(room, "alice", "four files", paths...)
	a.pullAll(room, b)
	b.tickFiles()
	a.pullAll(room, b)
	ctx := context.Background()
	carried := 0
	for _, f := range res.Files {
		fs, err := b.f.ServeGet(ctx, c.id, FileGet{Room: room, Msg: msg, SHA256: f.SHA256})
		if err == nil && fs.Status == 202 {
			carried++
		}
	}
	if carried != 1 {
		t.Fatalf("%d carry jobs for one author, want 1", carried)
	}
	if _, err := b.f.RequestSend(ctx, FileSend{Draft: Draft{Room: room, Agent: "agent-b", Text: "mine"},
		Files: []string{writeSource(t, "mine.bin", strings.Repeat("m", 1000))}}); err != nil {
		t.Fatalf("own send: %v", err)
	}
}

// TestAnAuthorCannotHoldACarriersBudgetWithOneByteOfEachFile (F10): an
// author that uploads one byte of each file it was asked for still has
// the rest of each file counted in its share, so one byte does not open a
// second carry job; and the part goes when its want lapses, so the room
// budget is free again a day later.
// Requirements: L1
func TestAnAuthorCannotHoldACarriersBudgetWithOneByteOfEachFile(t *testing.T) {
	ns := filesNet(t, "a", "b", "c")
	a, b, c := ns[0], ns[1], ns[2]
	room := carryRoom(t, a, b, c)
	b.st.SetFileBudgets(2000, 20000)
	var paths []string
	for i := 0; i < 4; i++ {
		paths = append(paths, writeSource(t, "f"+strconv.Itoa(i), strings.Repeat(strconv.Itoa(i), 450)))
	}
	msg, res := a.sendFiles(room, "alice", "four files", paths...)
	a.pullAll(room, b)
	b.tickFiles()
	a.pullAll(room, b)
	ctx := context.Background()
	carried := 0
	for _, f := range res.Files {
		fs, err := b.f.ServeGet(ctx, c.id, FileGet{Room: room, Msg: msg, SHA256: f.SHA256})
		if err != nil || fs.Status != 202 {
			continue
		}
		carried++
		if got, err := b.f.ServePut(ctx, a.id, FilePut{Room: room, Msg: msg, SHA256: f.SHA256, Offset: 0, Length: 1, Total: f.Size},
			strings.NewReader("x")); err != nil || got != 1 {
			t.Fatalf("one byte: %d, %v", got, err)
		}
	}
	if carried != 1 {
		t.Fatalf("%d carry jobs for one author after one byte of each, want 1", carried)
	}
	own := func() error {
		_, err := b.f.RequestSend(ctx, FileSend{Draft: Draft{Room: room, Agent: "agent-b", Text: "mine"},
			Files: []string{writeSource(t, "mine.bin", strings.Repeat("m", 1500))}})
		return err
	}
	// 450 held of 2000: 1500 more does not fit while the part lives
	if err := own(); err == nil {
		t.Fatal("a send over the budget was taken while the part held its size")
	}
	*b.clock = b.clock.Add(core.WantLife + time.Minute)
	b.tickFiles()
	if err := own(); err != nil {
		t.Fatalf("own send a day later: %v", err)
	}
}

// fourFilesCarried sets up a carrier b with a room budget of 2000 that
// has one carry job for a's four 450-byte files, each with one byte
// uploaded where a want allowed it. It returns the message and the files.
func fourFilesCarried(t *testing.T) (a, b, c *fnode, room, msg string, res FileSendResult) {
	t.Helper()
	ns := filesNet(t, "a", "b", "c")
	a, b, c = ns[0], ns[1], ns[2]
	room = carryRoom(t, a, b, c)
	b.st.SetFileBudgets(2000, 20000)
	var paths []string
	for i := 0; i < 4; i++ {
		paths = append(paths, writeSource(t, "f"+strconv.Itoa(i), strings.Repeat(strconv.Itoa(i), 450)))
	}
	msg, res = a.sendFiles(room, "alice", "four files", paths...)
	a.pullAll(room, b)
	b.tickFiles()
	a.pullAll(room, b)
	return a, b, c, room, msg, res
}

// carryOneByte asks b, as c, for each file it does not yet carry, and
// uploads one byte, as a, against each carry job that starts. It returns
// how many started.
func carryOneByte(t *testing.T, a, b, c *fnode, room, msg string, res FileSendResult) int {
	t.Helper()
	ctx := context.Background()
	n := 0
	for _, f := range res.Files {
		if b.hasPart(room, msg, f.SHA256) {
			continue
		}
		fs, err := b.f.ServeGet(ctx, c.id, FileGet{Room: room, Msg: msg, SHA256: f.SHA256})
		if err != nil || fs.Status != 202 {
			continue
		}
		if got, err := b.f.ServePut(ctx, a.id, FilePut{Room: room, Msg: msg, SHA256: f.SHA256, Length: 1, Total: f.Size},
			strings.NewReader("x")); err == nil && got == 1 {
			n++
		}
	}
	return n
}

// TestAnEditDoesNotLeaveACarriersPartHoldingItsBudget (F10): an author
// that uploads one byte against a carry job and then edits its message,
// keeping every file, does not leave the part behind: the want goes with
// the version it was for, and so does its part. Editing again and again
// opens no more than one carry job at a time, and the carrier's own send
// still fits.
// Requirements: L1
func TestAnEditDoesNotLeaveACarriersPartHoldingItsBudget(t *testing.T) {
	a, b, c, room, msg, res := fourFilesCarried(t)
	ctx := context.Background()
	var kept []string
	for _, f := range res.Files {
		kept = append(kept, KeptPrefix+f.SHA256)
	}
	for i := 0; i < 4; i++ {
		if n := carryOneByte(t, a, b, c, room, msg, res); n > 1 {
			t.Fatalf("round %d: %d carry jobs for one author, want at most 1", i, n)
		}
		r, err := a.f.RequestSend(ctx, FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "edit " + strconv.Itoa(i)},
			Replaces: msg, Files: kept})
		if err != nil {
			t.Fatal(err)
		}
		if cf, err := a.f.ApproveSend(ctx, r.Request, r.Terms); err != nil || cf.State != store.RequestApproved {
			t.Fatalf("edit: %+v %v", cf, err)
		}
		a.pullAll(room, b)
		b.tickFiles()
		for _, f := range res.Files {
			if b.hasPart(room, msg, f.SHA256) {
				t.Fatalf("round %d: a part of %s outlived the version its want was for", i, f.Name)
			}
		}
	}
	if _, err := b.f.RequestSend(ctx, FileSend{Draft: Draft{Room: room, Agent: "agent-b", Text: "mine"},
		Files: []string{writeSource(t, "mine.bin", strings.Repeat("m", 1000))}}); err != nil {
		t.Fatalf("own send after the edits: %v", err)
	}
}

// TestAPartAnOlderBinaryLeftUnderALapsedWantGoes (F10, compatibility): a
// 0.2.3 binary lapsed a want and kept the part an upload had left. The
// sweep drops that part, so a carrier attacked before the upgrade is free
// again; a part reserved after the lapse (a fetch in progress) stays.
// Requirements: L1, I5
func TestAPartAnOlderBinaryLeftUnderALapsedWantGoes(t *testing.T) {
	a, b, c, room, msg, res := fourFilesCarried(t)
	before := usage(t, b, room)
	if n := carryOneByte(t, a, b, c, room, msg, res); n != 1 {
		t.Fatalf("%d carry jobs, want 1", n)
	}
	var held, other core.FileRef
	for _, f := range res.Files {
		if b.hasPart(room, msg, f.SHA256) {
			held = f
		} else {
			other = f
		}
	}
	// as 0.2.3 lapsed it: lapsed_at set, the part left
	lapsed := b.clock.Add(time.Minute)
	db, err := sql.Open("sqlite", store.FileURI(b.path)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE file_wants SET lapsed_at = ? WHERE room = ? AND msg = ? AND sha256 = ?`,
		lapsed.UnixMilli(), room, msg, held.SHA256); err != nil {
		t.Fatal(err)
	}
	// a fetch in progress for another file under a lapsed want: reserved later
	if _, err := db.Exec(`INSERT INTO file_wants (room, msg, version, sha256, author, size, at, lapsed_at) VALUES (?,?,?,?,?,?,?,?)`,
		room, msg, msg, other.SHA256, a.id, other.Size, b.clock.UnixMilli(), lapsed.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO file_parts (room, msg, sha256, size, have, at) VALUES (?,?,?,?,0,?)`,
		room, msg, other.SHA256, other.Size, lapsed.Add(time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	*b.clock = lapsed.Add(2 * time.Minute)
	b.tickFiles()
	if b.hasPart(room, msg, held.SHA256) {
		t.Fatal("the part an older binary left under a lapsed want is still held")
	}
	if !b.hasPart(room, msg, other.SHA256) {
		t.Fatal("a part reserved after its want lapsed was dropped")
	}
	if u := usage(t, b, room); u != before+other.Size {
		t.Fatalf("usage %d, want %d", u, before+other.Size)
	}
}

// TestLiveWantsAnOlderBinaryIssuedOverTheAuthorsShareStillWorkAndLapse
// (F10, compatibility): wants of one author for a message that is here,
// issued past the share as 0.2.3 did (no cap), are kept: a member's ask
// is answered "not yet", the author can still upload against them, and
// they lapse after their life as before; only a new carry job waits.
// Requirements: L1, I5
func TestLiveWantsAnOlderBinaryIssuedOverTheAuthorsShareStillWorkAndLapse(t *testing.T) {
	ns := filesNet(t, "a", "b", "c")
	a, b, c := ns[0], ns[1], ns[2]
	room := carryRoom(t, a, b, c)
	b.st.SetFileBudgets(2000, 20000)
	var paths []string
	for i := 0; i < 4; i++ {
		paths = append(paths, writeSource(t, "f"+strconv.Itoa(i), strings.Repeat(strconv.Itoa(i), 450)))
	}
	msg, res := a.sendFiles(room, "alice", "four files", paths...)
	a.pullAll(room, b)
	b.tickFiles()
	a.pullAll(room, b)
	ctx := context.Background()
	before := usage(t, b, room)       // the manifest
	for _, f := range res.Files[:3] { // 1350 bytes for one author: over its quarter
		if ok, err := b.st.IssueWant(ctx, store.FileWant{Room: room, Msg: msg, Version: msg, SHA256: f.SHA256,
			Author: a.id, Size: f.Size, At: *b.clock}); err != nil || !ok {
			t.Fatalf("an older binary's want: %v %v", ok, err)
		}
	}
	b.tickFiles()
	for _, f := range res.Files[:3] {
		if fs, err := b.f.ServeGet(ctx, c.id, FileGet{Room: room, Msg: msg, SHA256: f.SHA256}); err != nil ||
			fs.Status != 202 || fs.State != FileNotYet {
			t.Fatalf("an older want, asked: %+v %v", fs, err)
		}
	}
	if fs, err := b.f.ServeGet(ctx, c.id, FileGet{Room: room, Msg: msg, SHA256: res.Files[3].SHA256}); !errors.Is(err, ErrNotHeldFile) {
		t.Fatalf("a new carry job past the share: %+v %v", fs, err)
	}
	f0 := res.Files[0]
	if got, err := b.f.ServePut(ctx, a.id, FilePut{Room: room, Msg: msg, SHA256: f0.SHA256, Length: 1, Total: f0.Size},
		strings.NewReader("0")); err != nil || got != 1 {
		t.Fatalf("an upload against an older want: %d %v", got, err)
	}
	*b.clock = b.clock.Add(core.WantLife + time.Minute)
	b.tickFiles()
	for _, f := range res.Files[:3] {
		if fs, err := b.f.ServeGet(ctx, c.id, FileGet{Room: room, Msg: msg, SHA256: f.SHA256}); err != nil ||
			fs.Status != 202 || fs.State != FileGaveUp {
			t.Fatalf("an older want after its life: %+v %v", fs, err)
		}
	}
	if u := usage(t, b, room); u != before {
		t.Fatalf("%d bytes still reserved after the wants lapsed", u-before)
	}
}

// TestARequestStagedFromTheMachineKeyBeforeTheFixIsNotPublished (F33,
// compatibility): a pending request an older binary staged from one of
// a2p2's own files (here the bytes of the machine key) fails at approval
// with a reason, and nothing is sent; an honest pending request is still
// approved.
// Requirements: L1, P3
func TestARequestStagedFromTheMachineKeyBeforeTheFixIsNotPublished(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	ctx := context.Background()
	// staged as an older binary would have: the bytes left as they were
	bad, err := a.askSend(room, writeSource(t, "key-copy.bin", "the machine key"))
	if err != nil {
		t.Fatal(err)
	}
	honest, err := a.askSend(room, writeSource(t, "report.txt", "honest bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.st.FileRoot(), "identity.key"), []byte("the machine key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := a.f.ApproveSend(ctx, bad.Request, bad.Terms); err != nil || c.State != store.RequestFailed ||
		c.Reason != store.ReasonSendHomeFile || c.Effect != "" {
		t.Fatalf("a request with the key's bytes: %+v %v", c, err)
	}
	if c, err := a.f.ApproveSend(ctx, honest.Request, honest.Terms); err != nil || c.State != store.RequestApproved {
		t.Fatalf("an honest request: %+v %v", c, err)
	}
}

// TestARequestQueueThatFillsWhileFilesAreStagedGetsTheOneRefusal (F35):
// the queue was not full when the send began, and filled while its files
// were read; the send then gets the one staging refusal, whether its two
// files were equal or not, and leaves nothing staged.
// Requirements: L1, P3
func TestARequestQueueThatFillsWhileFilesAreStagedGetsTheOneRefusal(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	secret := writeSource(t, "secret.txt", "1234")
	same := writeSource(t, "guess1.txt", "1234")
	other := writeSource(t, "guess2.txt", "9999")
	filled := 0
	inHook := false
	testHook = func(p string) error {
		if p != "files:staged" || inHook {
			return nil
		}
		inHook = true
		defer func() { inHook = false }()
		for ; filled < MaxPendingRequests; filled++ {
			if _, err := a.askSend(room, writeSource(t, "f"+strconv.Itoa(filled), "filler "+strconv.Itoa(filled))); err != nil {
				break // full
			}
		}
		return nil
	}
	t.Cleanup(func() { testHook = nil })
	var answers []string
	for _, paths := range [][]string{{secret, same}, {secret, other}} {
		_, err := a.askSend(room, paths...)
		err = cannotAttachErr(err, paths)
		var ue *UserError
		if !errors.As(err, &ue) || ue.Code != ErrorInvalid || ue.Msg != CannotAttach {
			t.Fatalf("%v: %v", paths, err)
		}
		answers = append(answers, ue.Code+" "+ue.Msg)
		testHook = nil
		// one request less, so that the next send starts below the limit
		qs, err := a.st.Requests(context.Background(), room, store.RequestPending)
		if err != nil || len(qs) != MaxPendingRequests {
			t.Fatalf("requests %d %v", len(qs), err)
		}
		if _, err := a.r.Confirm(context.Background(), qs[0].ID, false); err != nil {
			t.Fatal(err)
		}
		filled = MaxPendingRequests - 1
		testHook = func(p string) error {
			if p == "files:staged" && !inHook {
				inHook = true
				defer func() { inHook = false }()
				_, _ = a.askSend(room, writeSource(t, "last", "last filler"))
			}
			return nil
		}
	}
	if answers[0] != answers[1] {
		t.Fatalf("answers differ: %q", answers)
	}
	// the last decline leaves one place: only the waiting requests' staging is left
	if n := stagingLeft(t, a); n != MaxPendingRequests-1 {
		t.Fatalf("%d staged, want only the %d requests'", n, MaxPendingRequests-1)
	}
}

// TestAtMostStagingAtOnceSendsStageFilesAtTheSameTime (F35): sends past
// the bound wait before they open any file, and one whose caller gives up
// meanwhile opens nothing.
// Requirements: L1
func TestAtMostStagingAtOnceSendsStageFilesAtTheSameTime(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	in := make(chan struct{}, StagingAtOnce)
	release := make(chan struct{})
	testHook = func(p string) error {
		if p == "files:staged" {
			in <- struct{}{}
			<-release
		}
		return nil
	}
	t.Cleanup(func() { testHook = nil })
	done := make(chan error, StagingAtOnce)
	for i := 0; i < StagingAtOnce; i++ {
		p := writeSource(t, "f"+strconv.Itoa(i), "bytes "+strconv.Itoa(i))
		go func() { _, err := a.askSend(room, p); done <- err }()
		<-in // one at a time into the hook: the test's random source is not shared safely
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.f.RequestSend(ctx, FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "one more"},
		Files: []string{writeSource(t, "more", "more bytes")}})
	close(release)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a send past the bound: %v", err)
	}
	for i := 0; i < StagingAtOnce; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
