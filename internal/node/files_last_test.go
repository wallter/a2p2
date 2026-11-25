package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// Tests for the fourth review of attachments (fix-files-a4): each fails
// without its fix.

// TestApprovalForEveryoneBindsTheMemberSetNotItsSize (L2): a send to
// everyone is approved for the set of members when the agent asked. One
// member removed and another admitted before approval keeps the count and
// changes the set: nothing is sent and the request ends terms_changed.
// Requirements: L1, P3
func TestApprovalForEveryoneBindsTheMemberSetNotItsSize(t *testing.T) {
	ns := filesNet(t, "a", "b", "c", "d")
	a, c, d := ns[0], ns[2], ns[3]
	room := fileRoom(t, a, ns[1], c)
	ctx := context.Background()
	a.tick()
	all, err := a.f.RequestSend(ctx, FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "for everyone"},
		Files: []string{writeSource(t, "e.txt", "for the three of us")}})
	if err != nil {
		t.Fatal(err)
	}
	if !all.Draft.Everyone || all.Draft.Members != 3 || len(all.Draft.MemberSet) != 64 {
		t.Fatalf("the terms of everyone: %+v", all.Draft)
	}
	before := len(a.records(room))
	a.change(room, Change{Kind: core.EntryRemove, Target: c.id})
	a.change(room, Change{Kind: core.EntryAdd, Target: d.id, Name: "d"})
	if st, err := a.r.State(room); err != nil || len(st.Members) != 3 {
		t.Fatalf("the room's members: %+v %v", st, err)
	}
	cf, err := a.f.ApproveSend(ctx, all.Request, all.Terms)
	if err != nil || cf.State != store.RequestFailed || cf.Reason != ErrorTermsChanged {
		t.Fatalf("approved for {a, b, c}, published to {a, b, d}: %+v %v", cf, err)
	}
	if n := len(a.records(room)); n != before {
		t.Fatalf("%d records written after the member set changed", n-before)
	}
}

// unnamedBlobs lists the files under n's blobs/<room>/ that no row holds,
// and the room's usage.
func unnamedBlobs(t *testing.T, n *fnode, room string) ([]string, int64) {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(n.st.FileRoot(), "blobs", room))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var out []string
	var used int64
	_ = n.st.View(context.Background(), func(rd *store.Reader) error {
		for _, e := range ents {
			if !e.Type().IsRegular() {
				continue
			}
			if _, held, err := rd.Held(room, e.Name()); err != nil || !held {
				out = append(out, e.Name())
			}
		}
		used, _, err = rd.FileUsage(room)
		return err
	})
	return out, used
}

func stagingLeft(t *testing.T, n *fnode) int {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(n.st.FileRoot(), "blobs", ".staging"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return len(ents)
}

// TestASendWhoseRecordCannotBeWrittenStagesAndCopiesNothing (L3, the
// review's scenario scaled down): text over the record limit with four
// files is refused when asked, before a byte is staged, ten times over;
// nothing is left and nothing counts against the budget.
// Requirements: L1, F7
func TestASendWhoseRecordCannotBeWrittenStagesAndCopiesNothing(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a := ns[0]
	room := fileRoom(t, a, ns[1])
	a.tick()
	long := strings.Repeat("x", core.MaxBodyBytes+1)
	for i := range 10 {
		var paths []string
		for j := range 4 {
			paths = append(paths, writeSource(t, fmt.Sprintf("f%d.bin", j), fmt.Sprintf("file %d of try %d", j, i)))
		}
		_, err := a.f.RequestSend(context.Background(), FileSend{Draft: Draft{Room: room, Agent: "alice", Text: long}, Files: paths})
		var ue *UserError
		if !errors.As(err, &ue) || ue.CodeOf() != ErrorTooLarge {
			t.Fatalf("try %d: a send whose text is over the record limit: %v", i, err)
		}
		if cannotAttachErr(err, paths).Error() == CannotAttach {
			t.Fatalf("try %d: the refusal is about the text, not a path", i)
		}
	}
	if left, used := unnamedBlobs(t, a, room); len(left) != 0 || used != 0 || stagingLeft(t, a) != 0 {
		t.Fatalf("left behind: blobs %v, usage %d, staging %d", left, used, stagingLeft(t, a))
	}
}

// TestAnApprovalWhoseRecordCannotBeWrittenCopiesNothing (L3): a request
// already raised whose record cannot be written (staged directly, as a
// request raised before the rule) fails at approval before any byte is
// copied into place.
// Requirements: L1, F7
func TestAnApprovalWhoseRecordCannotBeWrittenCopiesNothing(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a := ns[0]
	room := fileRoom(t, a, ns[1])
	a.tick()
	ctx := context.Background()
	id := a.r.newID()
	d := store.SendDraft{Agent: "alice", Text: strings.Repeat("x", core.MaxBodyBytes+1), Everyone: true}
	if err := a.st.View(ctx, func(rd *store.Reader) (err error) {
		d.Members, d.MemberSet, err = memberSet(rd, room)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var staged []store.StagedFile
	for i := range 4 {
		p := writeSource(t, fmt.Sprintf("f%d.bin", i), fmt.Sprintf("file %d", i))
		sf, err := a.st.StageFile(id, i, p, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		x := store.DraftFile{StagedFile: store.StagedFile{N: i, SHA256: sf.SHA256, Size: sf.Size}, Path: p, Name: filepath.Base(p)}
		d.Files = append(d.Files, x)
		staged = append(staged, x.StagedFile)
	}
	draft, _ := json.Marshal(d)
	q := store.Request{ID: id, Kind: store.RequestSendFiles, Room: room, Created: *a.clock, State: store.RequestPending,
		Draft: string(draft), Terms: SendTerms(room, d)}
	if err := a.st.CreateSendRequest(ctx, q, staged, MaxPendingRequests); err != nil {
		t.Fatal(err)
	}
	copied := false
	testHook = func(p string) error {
		if p == "files:publish" {
			copied = true
		}
		return nil
	}
	t.Cleanup(func() { testHook = nil })
	c, err := a.f.ApproveSend(ctx, id, q.Terms)
	if err != nil || c.State != store.RequestFailed || c.Reason != ReasonSendUnwritable {
		t.Fatalf("%+v %v", c, err)
	}
	if copied {
		t.Fatal("bytes were put in place for a record that cannot be written")
	}
	if left, used := unnamedBlobs(t, a, room); len(left) != 0 || used != 0 {
		t.Fatalf("left behind: blobs %v, usage %d", left, used)
	}
}

// TestAPublicationThatFailsAfterItsBytesArePlacedLeavesNoBlob (L3): the
// last of four staged files changes after approval began, so the
// publication fails after the manifest and three files were put in place;
// the blobs it wrote go in the same step and nothing counts against the
// budget, ten times over. (An expired request no longer gets this far: it
// is refused before a byte is copied [F3].)
// Requirements: L1, F7
func TestAPublicationThatFailsAfterItsBytesArePlacedLeavesNoBlob(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a := ns[0]
	room := fileRoom(t, a, ns[1])
	ctx := context.Background()
	var last string // the staged path of the request's last file
	testHook = func(p string) error {
		if p == "files:publish" {
			b, err := os.ReadFile(last)
			if err != nil {
				return err
			}
			_ = os.WriteFile(last, []byte(strings.ToUpper(string(b))), 0o600) // the same size, other bytes
		}
		return nil
	}
	t.Cleanup(func() { testHook = nil })
	for i := range 10 {
		a.tick()
		var paths []string
		for j := range 4 {
			paths = append(paths, writeSource(t, fmt.Sprintf("f%d.bin", j), fmt.Sprintf("file %d of try %d", j, i)))
		}
		res, err := a.f.RequestSend(ctx, FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "late"}, Files: paths})
		if err != nil {
			t.Fatal(err)
		}
		last = filepath.Join(a.st.FileRoot(), "blobs", ".staging", res.Request, "3")
		if c, err := a.f.ApproveSend(ctx, res.Request, res.Terms); err != nil || c.State != store.RequestFailed || c.Reason != store.ReasonSendChanged {
			t.Fatalf("try %d: %+v %v", i, c, err)
		}
		if left, used := unnamedBlobs(t, a, room); len(left) != 0 || used != 0 {
			t.Fatalf("try %d: left behind: blobs %v, usage %d", i, left, used)
		}
	}
	if len(a.records(room)) != 0 {
		t.Fatal("a record was published")
	}
}

// TestAReplacementsEligibilityIsDecidedByTheClockInsideThePublication
// (W3): the replaced version's text is live when approval begins and
// expires before the publishing transaction reads the clock; nothing is
// published.
// Requirements: L1, F8
func TestAReplacementsEligibilityIsDecidedByTheClockInsideThePublication(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a := ns[0]
	room := fileRoom(t, a, ns[1])
	ctx := context.Background()
	msg, res := a.sendFiles(room, "alice", "first", writeSource(t, "x.txt", "kept"))
	*a.clock = a.clock.Add(store.Retention - time.Hour)
	a.tick()
	r, err := a.f.RequestSend(ctx, FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "second"}, Replaces: msg,
		Files: []string{KeptPrefix + res.Files[0].SHA256}})
	if err != nil {
		t.Fatal(err)
	}
	before := len(a.records(room))
	var once atomic.Bool
	testHook = func(p string) error {
		if p == "files:replaceable" && once.CompareAndSwap(false, true) {
			*a.clock = a.clock.Add(2 * time.Hour) // the replaced text is past retention
		}
		return nil
	}
	t.Cleanup(func() { testHook = nil })
	c, err := a.f.ApproveSend(ctx, r.Request, r.Terms)
	if err != nil || c.State != store.RequestFailed || c.Reason != store.ReasonSendReplaced {
		t.Fatalf("a replacement of expired text: %+v %v", c, err)
	}
	if n := len(a.records(room)); n != before {
		t.Fatalf("%d records published", n-before)
	}
}

// TestAWantThatEndsWhileItsAnswerWaitsForTheDatabaseIsNotAdvertised (W4):
// time passes while choosing the wants waits for its transaction; the
// clock read inside it ends the want's life.
// Requirements: F9
func TestAWantThatEndsWhileItsAnswerWaitsForTheDatabaseIsNotAdvertised(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	a.net.noInbound[a.id] = true
	room := fileRoom(t, a, b)
	a.sendFiles(room, "alice", "from behind a wall", writeSource(t, "w.txt", "uploaded bytes"))
	a.pullAll(room, b)
	b.tickFiles()
	w := wantOf(t, b, room)
	ctx := context.Background()
	if !w.Lapsed.IsZero() || !w.Sent.IsZero() || !b.clock.Before(w.At.Add(core.WantLife)) {
		t.Fatalf("not a live want never sent: %+v", w)
	}
	testHook = func(p string) error {
		if p == "files:wants" {
			*b.clock = w.At.Add(core.WantLife + time.Second)
		}
		return nil
	}
	t.Cleanup(func() { testHook = nil })
	if ws := b.f.wantsFor(ctx, room, a.id, 1<<20); len(ws) != 0 {
		t.Fatalf("a want past its life by the clock of the transaction was advertised: %+v", ws)
	}
}

// TestAFormerHoldersNotYetDoesNotKeepAFetchComing (W5): C answered "not
// yet" and was then removed; the author answers not held. The fetch is
// unavailable, not coming, and C's answer is not shown.
// Requirements: F9
func TestAFormerHoldersNotYetDoesNotKeepAFetchComing(t *testing.T) {
	ns := filesNet(t, "a", "b", "c")
	a, b, c := ns[0], ns[1], ns[2]
	room := fileRoom(t, a, b, c)
	ctx := context.Background()
	msg, res := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "never held by c"))
	sha := res.Files[0].SHA256
	b.pullAll(room, a)
	b.tickFiles()
	if err := b.st.NoteFileObs(ctx, room, msg, sha, c.id, store.AnswerNotYet, *b.clock); err != nil {
		t.Fatal(err)
	}
	a.change(room, Change{Kind: core.EntryRemove, Target: c.id})
	b.pullAll(room, a)
	b.inbox(room, "agent-b")
	b.f.t = &beforeGet{memFileLink: memFileLink{b.fn, b.id}, peer: a.id, before: func() {}}
	r, err := b.fetch(room, "agent-b", sha)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != FetchUnavailable {
		t.Fatalf("a removed member's old answer kept the fetch at %+v", r)
	}
	for _, h := range r.Holders {
		if h.Node == c.id {
			t.Fatalf("a removed member's answer was shown: %+v", r.Holders)
		}
	}
}

// beforeGet is a file link that runs before, once, ahead of the first GET
// to peer, and answers that GET not held.
type beforeGet struct {
	memFileLink
	peer   string
	before func()
}

func (l *beforeGet) GetFile(ctx context.Context, peer string, q FileGet) (*FileReply, error) {
	if peer == l.peer && l.before != nil {
		l.memFileLink.n.mu.Lock()
		l.memFileLink.n.asked = append(l.memFileLink.n.asked, [3]string{l.from, peer, q.SHA256})
		l.memFileLink.n.mu.Unlock()
		l.before()
		l.before = nil
		return &FileReply{Status: 404}, nil
	}
	return l.memFileLink.GetFile(ctx, peer, q)
}

// TestAMemberRemovedWhileAnEarlierCandidateIsAskedIsNeverAsked (L1 a): B
// fetches with the author, then C, as candidates. While the author is
// being asked, C's removal commits at B; the author answers not held. No
// request naming the hash reaches C.
// Requirements: L1, F2
func TestAMemberRemovedWhileAnEarlierCandidateIsAskedIsNeverAsked(t *testing.T) {
	ns := filesNet(t, "a", "b", "c")
	a, b, c := ns[0], ns[1], ns[2]
	room := fileRoom(t, a, b, c)
	msg, res := a.sendFiles(room, "alice", "for b and c", writeSource(t, "x.txt", "members only"))
	sha := res.Files[0].SHA256
	b.pullAll(room, a)
	b.tickFiles()
	if !listHere(t, b, room, msg) {
		t.Fatal("b has no list")
	}
	b.f.t = &beforeGet{memFileLink: memFileLink{b.fn, b.id}, peer: a.id, before: func() {
		a.change(room, Change{Kind: core.EntryRemove, Target: c.id})
		b.pullAll(room, a)
	}}
	if _, err := b.f.download(context.Background(), room, msg, sha, []string{a.id, c.id}); err == nil {
		t.Fatal("served by nobody, yet the download succeeded")
	}
	if st, _ := b.r.State(room); len(st.Members) != 2 {
		t.Fatalf("c's removal did not reach b: %+v", st.Members)
	}
	b.fn.mu.Lock()
	defer b.fn.mu.Unlock()
	for _, q := range b.fn.asked {
		if q[1] == c.id {
			t.Fatal("a request naming the file's hash reached c after its removal committed")
		}
	}
}

// TestAGetBetweenARemovalsCommitAndItsPublicationIsRefused (L1 b): a GET
// from B arrives at A after B's removal has committed at A and before A's
// membership in memory is updated. It is refused: a lease is never taken
// against membership older than the database.
// Requirements: L1, T3
func TestAGetBetweenARemovalsCommitAndItsPublicationIsRefused(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "until b is removed"))
	b.pullAll(room, a)
	type got struct {
		served bool
		err    error
	}
	out := make(chan got, 1)
	var armed atomic.Bool
	testHook = func(p string) error {
		if p == "ingest:publish" && armed.CompareAndSwap(true, false) {
			go func() {
				fs, err := a.f.ServeGet(context.Background(), b.id, FileGet{Room: room, Msg: msg, SHA256: res.Files[0].SHA256})
				if err != nil {
					out <- got{err: err}
					return
				}
				_, err = io.ReadAll(fs.Body)
				fs.Body.Close()
				out <- got{served: err == nil, err: err}
			}()
			time.Sleep(100 * time.Millisecond) // the GET runs in the window
		}
		return nil
	}
	t.Cleanup(func() { testHook = nil })
	armed.Store(true)
	a.change(room, Change{Kind: core.EntryRemove, Target: b.id})
	select {
	case g := <-out:
		if g.served {
			t.Fatal("a machine whose removal had committed was served the file")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the GET did not finish")
	}
}
