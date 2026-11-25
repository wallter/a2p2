package node

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// Tests for the findings of the review of the attachments mechanism
// (fix-files-a): each fails without its fix.

// TestAResponseInProgressStopsWhenItsTextExpiresByTheClock (finding 1):
// nothing is committed, only the clock moves; the next read is refused.
// Requirements: L1, T5
func TestAResponseInProgressStopsWhenItsTextExpiresByTheClock(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "a file that expires mid-way"))
	b.pullAll(room, a)
	fs, err := a.f.ServeGet(context.Background(), b.id, FileGet{Room: room, Msg: msg, SHA256: res.Files[0].SHA256})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Body.Close()
	if _, err := fs.Body.Read(make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	*a.clock = a.clock.Add(store.Retention + time.Minute)
	if _, err := io.ReadAll(fs.Body); !errors.Is(err, errRevoked) {
		t.Fatalf("the response went on after its text expired: %v", err)
	}
}

// TestAnUploadInProgressStopsWhenTheWantEnds (finding 1, PUT): the
// upload's body is checked before every read, not only between chunks.
// Requirements: L1, T5
func TestAnUploadInProgressStopsWhenTheWantEnds(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	a.net.noInbound[a.id] = true
	room := fileRoom(t, a, b)
	msg, _ := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "upload me"))
	a.pullAll(room, b)
	b.tickFiles() // b wants the list from a
	var w store.FileWant
	_ = b.st.View(context.Background(), func(rd *store.Reader) (err error) {
		ws, err := rd.FileWants(room)
		if len(ws) == 1 {
			w = ws[0]
		}
		return err
	})
	man := a.manifestBytes(room, msg)
	// the want ends (here: it is dropped) after the upload's headers were checked
	body := &hookReader{data: man, before: func() { _ = b.st.DropWant(context.Background(), room, msg, w.SHA256) }}
	if _, err := b.f.ServePut(context.Background(), a.id, FilePut{Room: room, Msg: msg, SHA256: w.SHA256,
		Length: int64(len(man)), Total: int64(len(man))}, body); err != ErrNotHeldFile {
		t.Fatalf("an upload whose want ended: %v", err)
	}
	if listHere(t, b, room, msg) {
		t.Fatal("bytes nobody wants any more were stored")
	}
}

// hookReader runs before once, at its first read.
type hookReader struct {
	data   []byte
	before func()
}

func (h *hookReader) Read(p []byte) (int, error) {
	if h.before != nil {
		h.before()
		h.before = nil
	}
	if len(h.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, h.data)
	h.data = h.data[n:]
	return n, nil
}

func (n *fnode) manifestBytes(room, msg string) []byte {
	n.t.Helper()
	b, err := n.st.OpenBlob(room, n.manifestOf(room, msg))
	if err != nil {
		n.t.Fatal(err)
	}
	defer b.Close()
	out, _ := io.ReadAll(b)
	return out
}

// TestARefusalOfAMessageThePeerMayNotReadLooksUpNoAttachment (finding 7):
// for a member without history, a known message with files and an unknown
// one are refused before any attachment row is read.
// Requirements: L1, T3
func TestARefusalOfAMessageThePeerMayNotReadLooksUpNoAttachment(t *testing.T) {
	ns := filesNet(t, "a", "b", "c")
	a, b, c := ns[0], ns[1], ns[2]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "before c", writeSource(t, "x.txt", "history"))
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "c"})
	for _, m := range []string{msg, strings.Repeat("1", 32)} {
		ck, err := a.f.check(context.Background(), room, m, res.Files[0].SHA256, c.id)
		if err != nil {
			t.Fatal(err)
		}
		if ck.mf != nil {
			t.Fatalf("attachment rows were read for %s before the permission", m)
		}
	}
}

// TestAnApprovalAfterTheRequestExpiredPublishesNothing (finding 9).
// Requirements: L1, P3
func TestAnApprovalAfterTheRequestExpiredPublishesNothing(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	res, err := a.f.RequestSend(context.Background(), FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "late"},
		Files: []string{writeSource(t, "x.txt", "too late")}})
	if err != nil {
		t.Fatal(err)
	}
	*a.clock = a.clock.Add(core.StagingLife + time.Minute) // no sweep has run
	c, err := a.r.Confirm(context.Background(), res.Request, true)
	if err != nil || c.State == store.RequestApproved {
		t.Fatalf("%+v %v", c, err)
	}
	if len(a.records(room)) != 0 {
		t.Fatal("an expired request published a record")
	}
}

// TestAStagedSendOverTheBudgetIsRefusedBeforeAByteIsCopied (finding 9).
// Requirements: L1
func TestAStagedSendOverTheBudgetIsRefusedBeforeAByteIsCopied(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	a.st.SetFileBudgets(10, 0)
	_, err := a.f.RequestSend(context.Background(), FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "big"},
		Files: []string{writeSource(t, "x.txt", "more than ten bytes")}})
	var ue *UserError
	if !errors.As(err, &ue) || ue.CodeOf() != ErrorTooLarge {
		t.Fatalf("over the budget: %v", err)
	}
}

// TestFileTransfersShareOneCapAcrossBothDirections (finding 10).
// Requirements: L1
func TestFileTransfersShareOneCapAcrossBothDirections(t *testing.T) {
	tr := newTransfers(core.FileTransfers)
	for i := 0; i < core.FileTransfers; i++ {
		dir := dirIn
		if i%2 == 0 {
			dir = dirOut
		}
		if _, err := tr.take("p"+string(rune('a'+i)), dir); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{dirIn, dirOut} {
		if _, err := tr.take("pz", dir); !errors.Is(err, errAllBusy) {
			t.Fatalf("a ninth transfer %s: %v", dir, err)
		}
	}
}
