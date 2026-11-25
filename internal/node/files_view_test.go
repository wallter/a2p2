package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/members"
	"github.com/wallter/a2p2/internal/store"
)

// Tests for rulings B2 and B3 of the second review (fix-files-a2):
// permission is decided under one view and held by the transfer; bytes go
// live only if the rule holds in the transaction that publishes them.

// wantOf returns n's one want in room.
func wantOf(t *testing.T, n *fnode, room string) store.FileWant {
	t.Helper()
	var w store.FileWant
	_ = n.st.View(context.Background(), func(rd *store.Reader) error {
		ws, err := rd.FileWants(room)
		if len(ws) != 1 {
			t.Fatalf("wants %+v", ws)
		}
		w = ws[0]
		return err
	})
	return w
}

// TestARemovalCommittedBetweenTheMembershipAndTheRowsIsSeen (B2): a
// removal that commits after the membership was read and before the rows
// are makes the decision read both again; the removed machine is refused.
// Requirements: L1, T3
func TestARemovalCommittedBetweenTheMembershipAndTheRowsIsSeen(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "members only, decided once"))
	b.pullAll(room, a)
	// the second view is the one the response's lease is granted under
	removed, views := false, 0
	testHook = func(p string) error {
		if p == "files:view" {
			if views++; views == 2 {
				removed = true
				a.change(room, Change{Kind: core.EntryRemove, Target: b.id})
			}
		}
		return nil
	}
	defer func() { testHook = nil }()
	fs, err := a.f.ServeGet(context.Background(), b.id, FileGet{Room: room, Msg: msg, SHA256: res.Files[0].SHA256})
	if !removed {
		t.Fatal("the hook did not run")
	}
	if err == nil {
		fs.Body.Close()
		t.Fatal("a machine removed between the two reads was served")
	}
}

// TestAnUndecidedGrantIsARefusal (B2): while this machine's own permission
// for an author is undecided (a head of its frontier not held), it neither
// keeps nor serves that author's files, though the record reads as live.
// Requirements: L1, T3
func TestAnUndecidedGrantIsARefusal(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "decided or not"))
	sha := res.Files[0].SHA256
	b.pullAll(room, a)
	b.tickFiles()
	b.inbox(room, "agent-b")
	if r, err := b.fetch(room, "agent-b", sha); err != nil || r.State != FetchHere {
		t.Fatalf("%+v %v", r, err)
	}
	decideWith := func(change func(*members.State)) fileCheck {
		var c fileCheck
		if err := b.f.within(context.Background(), room, *b.clock, func(v *roomView, st members.State, _ int64) (err error) {
			if change != nil {
				old := st.Permissions
				st.Permissions = map[string]members.Permission{}
				for k, p := range old {
					st.Permissions[k] = p
				}
				change(&st)
			}
			c, err = b.f.decide(v, st, msg, sha, a.id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := decideWith(nil); !c.listed || !c.servable() {
		t.Fatalf("decided grant: %+v", c)
	}
	c := decideWith(func(st *members.State) {
		st.Permissions[b.id] = members.Permission{From: map[string][]string{a.id: {strings.Repeat("9", 32)}}}
		st.Permissions[a.id] = members.Permission{All: true}
	})
	if c.listed || c.servable() {
		t.Fatalf("an undecided grant was taken as permission: %+v", c)
	}
}

// TestAResponseBlockedOnTheNetworkIsCutWhenItsPermissionEnds (B2): the
// reader stops reading; the withdraw ends the response's Done without
// another read, which the transport turns into a cut write.
// Requirements: L1, T5
func TestAResponseBlockedOnTheNetworkIsCutWhenItsPermissionEnds(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "a reader that stopped reading"))
	b.pullAll(room, a)
	fs, err := a.f.ServeGet(context.Background(), b.id, FileGet{Room: room, Msg: msg, SHA256: res.Files[0].SHA256})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Body.Close()
	if _, err := fs.Body.Read(make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	a.tick()
	if _, err := a.m.Withdraw(context.Background(), room, "alice", msg); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fs.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("a response blocked on the network went on after the withdraw")
	}
}

// TestAnUploadBlockedOnTheNetworkIsCutWhenTheWantEnds (B2): the uploader
// stops sending mid-body; the end of the want closes the body, and
// nothing is kept.
// Requirements: L1, T5
func TestAnUploadBlockedOnTheNetworkIsCutWhenTheWantEnds(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	a.net.noInbound[a.id] = true
	room := fileRoom(t, a, b)
	msg, _ := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "upload me, slowly"))
	a.pullAll(room, b)
	b.tickFiles() // b wants the list from a
	w := wantOf(t, b, room)
	man := a.manifestBytes(room, msg)
	body := &stallingReader{blockingReader: blockingReader{data: man[:5], done: make(chan struct{})}, stalled: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := b.f.ServePut(context.Background(), a.id, FilePut{Room: room, Msg: msg, SHA256: w.SHA256,
			Length: int64(len(man)), Total: int64(len(man))}, body)
		done <- err
	}()
	// the five bytes are read, then the body blocks
	select {
	case <-body.stalled:
	case <-time.After(5 * time.Second):
		t.Fatal("the body was never read past its bytes")
	}
	if err := b.st.DropWant(context.Background(), room, msg, w.SHA256); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != ErrNotHeldFile {
			t.Fatalf("an upload whose want ended: %v", err)
		}
	case <-time.After(5 * time.Second):
		body.Close()
		t.Fatal("an upload blocked on the network went on after its want ended")
	}
	if listHere(t, b, room, msg) || b.hasPart(room, msg, w.SHA256) {
		t.Fatal("bytes nobody wants any more were kept")
	}
}

// TestTheDatabaseIsFreeWhileATransferWaitsOnTheNetwork (B7): an upload
// whose body has stalled holds no transaction; reads and writes of the
// store go on.
// Requirements: L1
func TestTheDatabaseIsFreeWhileATransferWaitsOnTheNetwork(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	a.net.noInbound[a.id] = true
	room := fileRoom(t, a, b)
	msg, _ := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "stalled upload"))
	a.pullAll(room, b)
	b.tickFiles()
	w := wantOf(t, b, room)
	man := a.manifestBytes(room, msg)
	body := &stallingReader{blockingReader: blockingReader{data: man[:5], done: make(chan struct{})}, stalled: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = b.f.ServePut(context.Background(), a.id, FilePut{Room: room, Msg: msg, SHA256: w.SHA256,
			Length: int64(len(man)), Total: int64(len(man))}, body)
	}()
	defer func() { body.Close(); <-done }()
	select {
	case <-body.stalled:
	case <-time.After(5 * time.Second):
		t.Fatal("the body was never read past its bytes")
	}
	free := make(chan error, 1)
	go func() {
		free <- b.st.View(context.Background(), func(rd *store.Reader) error { _, err := rd.FileWants(room); return err })
	}()
	select {
	case err := <-free:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		body.Close()
		t.Fatal("the store waited on a transfer's network read")
	}
}

// stallingReader is a blockingReader that says when a read found no bytes
// left and blocked.
type stallingReader struct {
	blockingReader
	stalled chan struct{}
	once    bool
}

func (s *stallingReader) Read(p []byte) (int, error) {
	if len(s.data) == 0 && !s.once {
		s.once = true
		close(s.stalled)
	}
	return s.blockingReader.Read(p)
}

func (n *fnode) hasPart(room, msg, sha string) bool {
	var ok bool
	_ = n.st.View(context.Background(), func(rd *store.Reader) (err error) {
		_, ok, err = rd.FilePart(room, msg, sha)
		return err
	})
	return ok
}

// TestAnUploadWhoseWantEndsJustBeforeItGoesLiveIsNotPromoted (B3): every
// byte arrived and matched; the want ends before the publishing
// transaction; the bytes do not go live and the part goes.
// Requirements: L1, T3
func TestAnUploadWhoseWantEndsJustBeforeItGoesLiveIsNotPromoted(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	a.net.noInbound[a.id] = true
	room := fileRoom(t, a, b)
	msg, _ := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "complete, then unwanted"))
	a.pullAll(room, b)
	b.tickFiles()
	w := wantOf(t, b, room)
	man := a.manifestBytes(room, msg)
	testHook = func(p string) error {
		if p == "files:commit" {
			_ = b.st.DropWant(context.Background(), room, msg, w.SHA256)
		}
		return nil
	}
	defer func() { testHook = nil }()
	_, err := b.f.ServePut(context.Background(), a.id, FilePut{Room: room, Msg: msg, SHA256: w.SHA256,
		Length: int64(len(man)), Total: int64(len(man))}, strings.NewReader(string(man)))
	if err != ErrNotHeldFile {
		t.Fatalf("promoted after the want ended: %v", err)
	}
	if listHere(t, b, room, msg) || heldFor(t, b, room, w.SHA256, msg) != "" || b.hasPart(room, msg, w.SHA256) {
		t.Fatal("the bytes went live, or their part stayed")
	}
	if _, err := os.Stat(b.st.FileRoot() + "/blobs/" + room + "/" + w.SHA256); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the blob is on disk: %v", err)
	}
}

// TestAFetchAfterTheAgentEditedItsCopyGetsVerifiedBytes (B3): a copy is
// never handed out again; a second fetch writes and checks a new one.
// Requirements: L1
func TestAFetchAfterTheAgentEditedItsCopyGetsVerifiedBytes(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	data := "the bytes the author sent"
	_, res := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", data))
	sha := res.Files[0].SHA256
	b.pullAll(room, a)
	b.tickFiles()
	b.inbox(room, "agent-b")
	r1, err := b.fetch(room, "agent-b", sha)
	if err != nil || r1.State != FetchHere {
		t.Fatalf("%+v %v", r1, err)
	}
	if err := os.WriteFile(r1.Path, []byte("the agent changed this"), 0o600); err != nil {
		t.Fatal(err)
	}
	r2, err := b.fetch(room, "agent-b", sha)
	if err != nil || r2.State != FetchHere {
		t.Fatalf("%+v %v", r2, err)
	}
	got, err := os.ReadFile(r2.Path)
	if err != nil || string(got) != data {
		t.Fatalf("the second fetch handed out %q %v", got, err)
	}
}

// TestARequestThatExpiresWhileItsFilesArePutInPlaceIsNotPublished (B6):
// the request is live when approved; copying its files takes it past its
// life; the publishing transaction reads the clock and refuses.
// Requirements: L1, P3
func TestARequestThatExpiresWhileItsFilesArePutInPlaceIsNotPublished(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a := ns[0]
	room := fileRoom(t, a, ns[1])
	res, err := a.f.RequestSend(context.Background(), FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "slow copy"},
		Files: []string{writeSource(t, "x.txt", "copied slowly")}})
	if err != nil {
		t.Fatal(err)
	}
	*a.clock = a.clock.Add(core.StagingLife - time.Second)
	testHook = func(p string) error {
		if p == "files:publish" {
			*a.clock = a.clock.Add(time.Minute)
		}
		return nil
	}
	defer func() { testHook = nil }()
	c, err := a.r.Confirm(context.Background(), res.Request, true)
	if err == nil && c.State == store.RequestApproved {
		t.Fatalf("published after the request expired: %+v", c)
	}
	if len(a.records(room)) != 0 {
		t.Fatal("an expired request published a record")
	}
}

// TestAnApprovalOfStagingChangedAfterItWasShownPublishesNothing
// (fix-files-b 3): the staged copy is changed (same size) after the
// person was shown its digest; approval copies and hashes it, finds
// another digest and publishes nothing.
// Requirements: L1, P3
func TestAnApprovalOfStagingChangedAfterItWasShownPublishesNothing(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a := ns[0]
	room := fileRoom(t, a, ns[1])
	res, err := a.f.RequestSend(context.Background(), FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "changed"},
		Files: []string{writeSource(t, "x.txt", "the approved bytes")}})
	if err != nil {
		t.Fatal(err)
	}
	// after the digests were checked again, before the bytes are put in place
	staged := filepath.Join(a.st.FileRoot(), "blobs", ".staging", res.Request, "0")
	testHook = func(p string) error {
		if p == "files:publish" {
			_ = os.WriteFile(staged, []byte("THE APPROVED BYTES"), 0o600)
		}
		return nil
	}
	defer func() { testHook = nil }()
	c, err := a.r.Confirm(context.Background(), res.Request, true)
	if err != nil || c.State != store.RequestFailed || c.Reason != store.ReasonSendChanged {
		t.Fatalf("%+v %v", c, err)
	}
	if len(a.records(room)) != 0 {
		t.Fatal("changed staging was published")
	}
	if _, err := os.Stat(filepath.Join(a.st.FileRoot(), "blobs", room, res.Files[0].SHA256)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a blob is at the approved digest: %v", err)
	}
}

// TestOnTheAuthorsMachineOnlyTheSendingAgentMayFetchWithoutBeingShown
// (fix-files-b 6): the agent that sent the message fetches its file; a
// second agent on the same machine must be shown the message first.
// Requirements: L1, D7
func TestOnTheAuthorsMachineOnlyTheSendingAgentMayFetchWithoutBeingShown(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a := ns[0]
	room := fileRoom(t, a, ns[1])
	msg, res := a.sendFiles(room, "alice", "mine", writeSource(t, "x.txt", "sent by alice"))
	sha := res.Files[0].SHA256
	if r, err := a.fetch(room, "alice", sha); err != nil || r.State != FetchHere {
		t.Fatalf("the sending agent: %+v %v", r, err)
	}
	a.register("carol")
	if _, err := a.fetch(room, "carol", sha); err == nil {
		t.Fatal("another agent on the author's machine fetched a file it was not shown")
	}
	// carol is shown it in a thread view, as the thread tool does
	es, err := a.m.Thread(context.Background(), room, msg)
	if err == nil {
		err = a.m.Showed(context.Background(), room, "carol", es, false)
	}
	if err != nil {
		t.Fatal(err)
	}
	if r, err := a.fetch(room, "carol", sha); err != nil || r.State != FetchHere {
		t.Fatalf("after carol was shown it: %+v %v", r, err)
	}
}

// TestTheApprovedTermsCoverEveryEffectiveTermAndAStaleDigestIsRefused
// (fix-files-b 5): changing the thread parent, the version replaced, the
// member count of "everyone" or the text changes the digest; approval
// with a digest that is not the request's current one sends nothing.
// Requirements: L1, P3
func TestTheApprovedTermsCoverEveryEffectiveTermAndAStaleDigestIsRefused(t *testing.T) {
	base := store.SendDraft{Agent: "alice", Text: "line one\nline two", Everyone: true, Members: 3,
		Files: []store.DraftFile{{StagedFile: store.StagedFile{SHA256: strings.Repeat("a", 64), Size: 4}, Path: "/x"}}}
	t0 := SendTerms("r", base)
	for name, change := range map[string]func(*store.SendDraft){
		"parent":  func(d *store.SendDraft) { d.Parent = strings.Repeat("1", 32) },
		"base":    func(d *store.SendDraft) { d.Base = strings.Repeat("2", 32) },
		"members": func(d *store.SendDraft) { d.Members = 4 },
		"text":    func(d *store.SendDraft) { d.Text = "line one\nline 2" },
	} {
		d := base
		change(&d)
		if SendTerms("r", d) == t0 {
			t.Errorf("the digest does not cover the %s", name)
		}
	}
	ns := filesNet(t, "a", "b")
	a := ns[0]
	room := fileRoom(t, a, ns[1])
	res, err := a.f.RequestSend(context.Background(), FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "terms"},
		Files: []string{writeSource(t, "x.txt", "approved by digest")}})
	if err != nil {
		t.Fatal(err)
	}
	var ue *UserError
	if _, err := a.f.ApproveSend(context.Background(), res.Request, strings.Repeat("0", 64)); !errors.As(err, &ue) ||
		ue.CodeOf() != ErrorTermsChanged {
		t.Fatalf("a stale digest: %v", err)
	}
	if len(a.records(room)) != 0 {
		t.Fatal("sent with a stale digest")
	}
	if c, err := a.f.ApproveSend(context.Background(), res.Request, res.Terms); err != nil || c.State != store.RequestApproved {
		t.Fatalf("the current digest: %+v %v", c, err)
	}
}

// TestTwoTransfersOfTheSameFileNeverWriteTheSamePart (finding 2): while one
// transfer holds (room, message, hash), a download and an upload of the
// same file are refused, whoever their peer.
// Requirements: L1, T3
func TestTwoTransfersOfTheSameFileNeverWriteTheSamePart(t *testing.T) {
	ns := filesNet(t, "a", "b", "c")
	a, b, c := ns[0], ns[1], ns[2]
	a.net.noInbound[a.id] = true
	room := fileRoom(t, a, b, c)
	msg, _ := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "written once"))
	a.pullAll(room, b)
	b.tickFiles()
	w := wantOf(t, b, room)
	man := a.manifestBytes(room, msg)
	unlock, ok := b.f.writer(room, msg, w.SHA256) // a transfer in progress
	if !ok {
		t.Fatal("no writer")
	}
	if done, err := b.f.getFrom(context.Background(), room, msg, w.SHA256, c.id); done || !errors.Is(err, errWriting) {
		t.Fatalf("a second download of the same file: %v %v", done, err)
	}
	if _, err := b.f.ServePut(context.Background(), a.id, FilePut{Room: room, Msg: msg, SHA256: w.SHA256,
		Length: int64(len(man)), Total: int64(len(man))}, strings.NewReader(string(man))); err != ErrNotHeldFile {
		t.Fatalf("an upload of the same file: %v", err)
	}
	if b.hasPart(room, msg, w.SHA256) {
		t.Fatal("a second writer wrote a part")
	}
	unlock()
	if _, err := b.f.ServePut(context.Background(), a.id, FilePut{Room: room, Msg: msg, SHA256: w.SHA256,
		Length: int64(len(man)), Total: int64(len(man))}, strings.NewReader(string(man))); err != nil {
		t.Fatalf("after the first ended: %v", err)
	}
	if !listHere(t, b, room, msg) {
		t.Fatal("the list did not go live")
	}
}
