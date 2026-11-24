package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

func filesNet(t *testing.T, names ...string) []*fnode {
	t.Helper()
	clock := lclock()
	net := newMemNet()
	fn := newMemFiles(net)
	var out []*fnode
	for _, n := range names {
		out = append(out, newFNode(t, net, fn, n, clock))
	}
	return out
}

func entryOf(t *testing.T, es []Entry, id string) Entry {
	t.Helper()
	for _, e := range es {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("no entry %s in %+v", id, es)
	return Entry{}
}

func listHere(t *testing.T, n *fnode, room, rec string) bool {
	t.Helper()
	var ok bool
	if err := n.st.View(context.Background(), func(rd *store.Reader) (err error) {
		_, ok, err = rd.FileSet(room, rec)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return ok
}

func heldFor(t *testing.T, n *fnode, room, sha, msg string) string {
	t.Helper()
	var how string
	if err := n.st.View(context.Background(), func(rd *store.Reader) (err error) {
		how, err = rd.HeldFor(room, sha, msg)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return how
}

// Requirements: L1, P3
func TestAFileIsSentOnlyAfterAPersonApprovesTheBytesStagedWhenTheAgentAsked(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	src := writeSource(t, "report.txt", "the first bytes")
	res, err := a.f.RequestSend(context.Background(), FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "see the file"}, Files: []string{src}})
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "waiting" || len(res.Files) != 1 || res.Files[0].Name != "report.txt" {
		t.Fatalf("asked: %+v", res)
	}
	if n := len(a.records(room)); n != 0 {
		t.Fatalf("%d records before approval", n)
	}
	// the source changes after the agent asked: the staged bytes go
	if err := os.WriteFile(src, []byte("other bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := a.r.Confirm(context.Background(), res.Request, true)
	if err != nil || c.State != store.RequestApproved {
		t.Fatalf("%+v %v", c, err)
	}
	if how := heldFor(t, a, room, core.SHA256Hex([]byte("the first bytes")), c.Entry); how != store.HowSent {
		t.Fatalf("held %q", how)
	}
	rs := a.records(room)
	if len(rs) != 1 || rs[0].Fields().Files == "" {
		t.Fatalf("records %+v", rs)
	}
	// a second decision changes nothing
	if _, err := a.r.Confirm(context.Background(), res.Request, true); err == nil {
		t.Fatal("approved twice")
	}
}

// Requirements: L1, P3
func TestADeclinedSendWritesNothingAndRemovesItsStaging(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	res, err := a.f.RequestSend(context.Background(), FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "x"},
		Files: []string{writeSource(t, "a.bin", "bytes")}})
	if err != nil {
		t.Fatal(err)
	}
	if c, err := a.r.Confirm(context.Background(), res.Request, false); err != nil || c.State != store.RequestDeclined {
		t.Fatalf("%+v %v", c, err)
	}
	if len(a.records(room)) != 0 {
		t.Fatal("a declined send wrote a record")
	}
	if _, err := os.Stat(a.st.CopyPath("blobs/.staging/" + res.Request)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging left: %v", err)
	}
}

// Requirements: L1, D7
func TestAReaderGetsTheListAtOnceAndTheBytesOnlyWhenItsAgentAsks(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	data := "0123456789abcdef-file"
	msg, res := a.sendFiles(room, "alice", "with a file", writeSource(t, "data.csv", data))
	sha := res.Files[0].SHA256
	b.pullAll(room, a)
	e := entryOf(t, b.inbox(room, "agent-b"), msg)
	if !e.FilesPending {
		t.Fatalf("before the list: %+v", e)
	}
	b.tickFiles()
	if !listHere(t, b, room, msg) {
		t.Fatal("the list did not come after the pull")
	}
	if how := heldFor(t, b, room, sha, msg); how != "" {
		t.Fatal("bytes came without an agent asking")
	}
	e = entryOf(t, b.inbox(room, "agent-b"), msg)
	if len(e.Files) != 1 || e.Files[0].SHA256 != sha || e.Files[0].Name != "data.csv" || e.Files[0].State != FileStateNotFetched {
		t.Fatalf("entry files %+v", e.Files)
	}
	r, err := b.fetch(room, "agent-b", sha)
	if err != nil || r.State != FetchHere {
		t.Fatalf("fetch %+v %v", r, err)
	}
	if got, _ := os.ReadFile(r.Path); string(got) != data {
		t.Fatalf("copy holds %q", got)
	}
	if r.From != a.id {
		t.Fatalf("from %s", r.From)
	}
	e = entryOf(t, b.inbox(room, "agent-b"), msg)
	if e.Files[0].State != FileStateHere {
		t.Fatalf("after fetch %+v", e.Files)
	}
}

// Requirements: L1, D7
func TestAnAgentMayNotFetchAFileItWasNotShown(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	_, res := a.sendFiles(room, "alice", "with a file", writeSource(t, "x.txt", "secret-ish"))
	b.pullAll(room, a)
	b.tickFiles()
	if _, err := b.fetch(room, "agent-b", res.Files[0].SHA256); err == nil {
		t.Fatal("fetched a file of a message the agent was not shown")
	}
}

// Requirements: L1, T5
func TestWithdrawDeletesAFileItsListAndAnyPartOrCopyOnEveryMachineThatLearnsOfIt(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "with a file", writeSource(t, "x.txt", "withdraw me"))
	sha := res.Files[0].SHA256
	b.pullAll(room, a)
	b.tickFiles()
	b.inbox(room, "agent-b")
	r, err := b.fetch(room, "agent-b", sha)
	if err != nil || r.State != FetchHere {
		t.Fatalf("%+v %v", r, err)
	}
	a.tick()
	if _, err := a.m.Withdraw(context.Background(), room, "alice", msg); err != nil {
		t.Fatal(err)
	}
	if heldFor(t, a, room, sha, msg) != "" {
		t.Fatal("the author kept the bytes")
	}
	b.pullAll(room, a)
	if heldFor(t, b, room, sha, msg) != "" {
		t.Fatal("the reader kept the bytes")
	}
	if _, err := os.Stat(r.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the agent's copy survived the withdraw")
	}
	g, err := b.f.Fetch(context.Background(), FetchOpts{Agent: "agent-b", Room: room, SHA256: sha, Message: msg})
	if err != nil || g.State != FetchGone || g.Reason != "withdrawn" {
		t.Fatalf("after withdraw %+v %v", g, err)
	}
}

// Requirements: L1, N1
func TestAReaderThatListensGetsAFileFromAnOutboundOnlyAuthorByUpload(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	a.net.noInbound[a.id] = true
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "from behind a wall", writeSource(t, "w.txt", "uploaded bytes"))
	sha := res.Files[0].SHA256
	a.pullAll(room, b) // b gets the record by the offer
	b.tickFiles()      // the list: the author cannot be dialled, so a want
	a.pullAll(room, b) // the answer carries the want; a uploads the list
	if !listHere(t, b, room, msg) {
		t.Fatal("the list did not come by upload")
	}
	b.inbox(room, "agent-b")
	r, err := b.fetch(room, "agent-b", sha)
	if err != nil || r.State != FetchComing {
		t.Fatalf("first fetch %+v %v", r, err)
	}
	a.pullAll(room, b)
	r, err = b.fetch(room, "agent-b", sha)
	if err != nil || r.State != FetchHere {
		t.Fatalf("second fetch %+v %v", r, err)
	}
	for _, p := range b.fn.puts {
		if p[0] != a.id {
			t.Fatalf("an upload from %s, not the author", p[0])
		}
	}
}

// Requirements: L1, N1
func TestInARoomWithOneListenerAFileFromAnOutboundOnlyAuthorReachesAnOutboundOnlyReader(t *testing.T) {
	ns := filesNet(t, "a", "b", "c")
	a, b, c := ns[0], ns[1], ns[2]
	a.net.noInbound[a.id] = true
	a.net.noInbound[c.id] = true
	room := fileRoom(t, a, b, c)
	_, res := a.sendFiles(room, "alice", "for c", writeSource(t, "c.txt", "carried bytes"))
	sha := res.Files[0].SHA256
	a.pullAll(room, b)
	b.tickFiles()
	a.pullAll(room, b) // the list reaches b
	c.pullAll(room, b)
	c.tickFiles() // the list reaches c from b
	c.inbox(room, "agent-c")
	r, err := c.fetch(room, "agent-c", sha)
	if err != nil || r.State != FetchComing {
		t.Fatalf("c asks: %+v %v", r, err) // b answers not yet and carries
	}
	a.pullAll(room, b) // b's want; a uploads
	r, err = c.fetch(room, "agent-c", sha)
	if err != nil || r.State != FetchHere || r.From != b.id {
		t.Fatalf("c asks again: %+v %v", r, err)
	}
}

// Requirements: L1
func TestWithCarryOffTheListenerAnswersNotHeldAndTheFileStaysUnavailable(t *testing.T) {
	ns := filesNet(t, "a", "b", "c")
	a, b, c := ns[0], ns[1], ns[2]
	a.net.noInbound[a.id] = true
	a.net.noInbound[c.id] = true
	room := fileRoom(t, a, b, c)
	if err := b.st.SetFileMode(context.Background(), room, store.FilesOff, *b.clock); err != nil {
		t.Fatal(err)
	}
	_, res := a.sendFiles(room, "alice", "for c", writeSource(t, "c.txt", "not carried"))
	a.pullAll(room, b)
	b.tickFiles()
	a.pullAll(room, b)
	c.pullAll(room, b)
	c.tickFiles()
	c.inbox(room, "agent-c")
	r, err := c.fetch(room, "agent-c", res.Files[0].SHA256)
	if err != nil || r.State == FetchHere || r.State == FetchComing && len(r.Holders) > 0 && r.Holders[0].Answer == store.AnswerNotYet {
		t.Fatalf("c asks: %+v %v", r, err)
	}
	a.pullAll(room, b)
	if heldFor(t, b, room, res.Files[0].SHA256, r.Message) != "" {
		t.Fatal("a listener with carry off holds the file")
	}
}

// Requirements: L1, T3
func TestAnUploadNobodyAskedForIsRefused(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "unasked"))
	b.pullAll(room, a)
	sha := res.Files[0].SHA256
	_, err := b.f.ServePut(context.Background(), a.id, FilePut{Room: room, Msg: msg, SHA256: sha, Offset: 0, Length: 4, Total: 7},
		strings.NewReader("unas"))
	if err != ErrNotHeldFile {
		t.Fatalf("an upload nobody asked for: %v", err)
	}
	if heldFor(t, b, room, sha, msg) != "" {
		t.Fatal("stored bytes nobody asked for")
	}
}

// Requirements: L1, T3
func TestAnUploadFromAMachineThatIsNotTheAuthorIsRefused(t *testing.T) {
	ns := filesNet(t, "a", "b", "c")
	a, b, c := ns[0], ns[1], ns[2]
	a.net.noInbound[a.id] = true
	room := fileRoom(t, a, b, c)
	msg, _ := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "listed"))
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
	if w.Author != a.id {
		t.Fatalf("want %+v", w)
	}
	man := "{}"
	if _, err := b.f.ServePut(context.Background(), c.id, FilePut{Room: room, Msg: msg, SHA256: w.SHA256, Length: int64(len(man)),
		Total: int64(len(man))}, strings.NewReader(man)); err != ErrNotHeldFile {
		t.Fatalf("an upload from a member that is not the author: %v", err)
	}
}

// Requirements: L1, T3
func TestAWantGoesOnlyToTheMessagesAuthorNeverToAnotherHolder(t *testing.T) {
	ns := filesNet(t, "a", "b", "c")
	a, b, c := ns[0], ns[1], ns[2]
	a.net.noInbound[a.id] = true
	a.net.noInbound[c.id] = true
	room := fileRoom(t, a, b, c)
	a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "wanted"))
	a.pullAll(room, b)
	c.pullAll(room, b) // c holds the record too
	b.tickFiles()
	var sent []*PullResponse
	b.net.tamper = func(from, to string, resp *PullResponse) {
		if len(resp.Want) > 0 {
			sent = append(sent, resp)
			if to != a.id && from != a.id {
				t.Errorf("a want to %s, from %s", to, from)
			}
		}
	}
	c.pullAll(room, b)
	a.pullAll(room, b)
	if len(sent) == 0 {
		t.Fatal("no want went to the author")
	}
}

// Requirements: L1, T3
func TestEveryNotHeldAnswerIsTheSameBytesAndTimingClass(t *testing.T) {
	ns := filesNet(t, "a", "b", "c")
	a, b, c := ns[0], ns[1], ns[2]
	room := fileRoom(t, a, b, c)
	msg, res := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "only for b"))
	sha := res.Files[0].SHA256
	other := strings.Repeat("e", 64)
	ctx := context.Background()
	// c holds nothing; b holds the record only
	b.pullAll(room, a)
	for name, q := range map[string]FileGet{
		"unknown hash":    {Room: room, Msg: msg, SHA256: other},
		"unknown message": {Room: room, Msg: strings.Repeat("1", 32), SHA256: sha},
		"not held here":   {Room: room, Msg: msg, SHA256: sha},
	} {
		srv := b.f
		if name != "not held here" {
			srv = a.f
		}
		if err := b.f.carryOffFor(ctx, room, srv); err != nil {
			t.Fatal(err)
		}
		_, err := srv.ServeGet(ctx, c.id, q)
		if err != ErrNotHeldFile {
			t.Errorf("%s: %v", name, err)
		}
	}
	body, _ := json.Marshal(ErrNotHeldFile)
	if string(body) != `{"error":"not_held"}` {
		t.Fatalf("404 body %s", body)
	}
}

// carryOffFor sets the room's setting off on srv, so a not-held file is
// answered 404 instead of carried.
func (f *Files) carryOffFor(ctx context.Context, room string, srv *Files) error {
	return srv.m.st.SetFileMode(ctx, room, store.FilesOff, srv.m.now())
}

// Requirements: L1, T5
func TestARevocationStopsATransferInProgress(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "a longer file, several chunks"))
	b.pullAll(room, a)
	ctx := context.Background()
	fs, err := a.f.ServeGet(ctx, b.id, FileGet{Room: room, Msg: msg, SHA256: res.Files[0].SHA256})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Body.Close()
	buf := make([]byte, 2)
	if _, err := fs.Body.Read(buf); err != nil {
		t.Fatal(err)
	}
	a.tick()
	if _, err := a.m.Withdraw(ctx, room, "alice", msg); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(fs.Body); !errors.Is(err, errRevoked) {
		t.Fatalf("the response went on after the withdraw: %v", err)
	}
}

// Requirements: L1
func TestFileTransfersAreLimitedPerPeerAndInAll(t *testing.T) {
	tr := newTransfers(core.FileTransfers)
	r1, err := tr.take("p0", dirOut)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.take("p0", dirOut); !errors.Is(err, errPeerBusy) {
		t.Fatalf("second to one peer: %v", err)
	}
	if r, err := tr.take("p0", dirIn); err != nil {
		t.Fatalf("the other direction: %v", err)
	} else {
		r()
	}
	for i := 1; i < core.FileTransfers; i++ {
		if _, err := tr.take("p"+string(rune('0'+i)), dirOut); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tr.take("p9", dirOut); !errors.Is(err, errAllBusy) {
		t.Fatalf("over the machine's limit: %v", err)
	}
	r1()
	r1() // twice is once
	if _, err := tr.take("p9", dirOut); err != nil {
		t.Fatalf("after one ended: %v", err)
	}
	if b := busy(errPeerBusy).(*PullError); b.Status != 429 || b.Code != CodeRateLimited {
		t.Fatalf("%+v", b)
	}
	if b := busy(errAllBusy).(*PullError); b.Status != 503 {
		t.Fatalf("%+v", b)
	}
}

// blockingReader returns its bytes and then blocks until closed.
type blockingReader struct {
	data []byte
	done chan struct{}
}

func (b *blockingReader) Read(p []byte) (int, error) {
	if len(b.data) > 0 {
		n := copy(p, b.data)
		b.data = b.data[n:]
		return n, nil
	}
	<-b.done
	return 0, io.ErrClosedPipe
}

func (b *blockingReader) Close() error {
	select {
	case <-b.done:
	default:
		close(b.done)
	}
	return nil
}

// Requirements: L1
func TestASlowTransferIsCut(t *testing.T) {
	// a trickle: a few bytes, then nothing, is cut by the window check
	br := &blockingReader{data: []byte("ab"), done: make(chan struct{})}
	rr := LimitRate(br, RateLimits{Total: time.Hour, Window: 20 * time.Millisecond, MinRate: 1 << 20})
	stop := rr.Watch(func() { br.Close() })
	defer stop()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(rr); done <- err }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled transfer was not cut")
	}
	if !rr.Slow() {
		t.Fatal("not counted as slow")
	}
	// the total time: a transfer older than Total reads nothing more
	clock := time.Unix(0, 0)
	rr = LimitRate(strings.NewReader("abc"), RateLimits{Total: time.Second, Window: time.Hour, MinRate: 1,
		Now: func() time.Time { return clock }})
	clock = clock.Add(2 * time.Second)
	if _, err := rr.Read(make([]byte, 3)); !errors.Is(err, ErrSlow) {
		t.Fatalf("past the total time: %v", err)
	}
}

// Requirements: L1, T3
func TestAnOffsetPastTheSizeIsRefusedBeforeArithmetic(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "ten bytes!"))
	b.pullAll(room, a)
	for _, off := range []int64{10, 1 << 62, -1} {
		_, err := a.f.ServeGet(context.Background(), b.id, FileGet{Room: room, Msg: msg, SHA256: res.Files[0].SHA256, Offset: off})
		var pe *PullError
		if !errors.As(err, &pe) || pe.Status != 416 {
			t.Errorf("offset %d: %v", off, err)
		}
	}
}

// Requirements: L1, R5
func TestPullsAreAnsweredWhileFilesTransfer(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "held open"))
	b.pullAll(room, a)
	// every file slot of a in use
	var bodies []io.Closer
	fs, err := a.f.ServeGet(context.Background(), b.id, FileGet{Room: room, Msg: msg, SHA256: res.Files[0].SHA256})
	if err != nil {
		t.Fatal(err)
	}
	bodies = append(bodies, fs.Body)
	defer func() {
		for _, b := range bodies {
			b.Close()
		}
	}()
	a.send(room, "alice", "while a file is open")
	done := make(chan struct{})
	go func() { b.mustPull(room, a.pnode, PullOpts{}); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a pull waited for a file transfer")
	}
}

// Requirements: L1
func TestAManifestThatDoesNotArriveIsUnavailableAfterBoundedTriesUntilAskedAgain(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	msg, _ := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "never listed"))
	b.pullAll(room, a)
	a.net.down[a.id] = true
	for i := 0; i < core.ListTries+2; i++ {
		*b.clock = b.clock.Add(core.ListRetryMax)
		b.tickFiles()
	}
	var l store.FileList
	_ = b.st.View(context.Background(), func(rd *store.Reader) (err error) {
		l, _, err = rd.FileList(room, msg)
		return err
	})
	if l.State != store.ListUnavailable || l.Fails != core.ListTries {
		t.Fatalf("list %+v", l)
	}
	e := entryOf(t, b.inbox(room, "agent-b"), msg)
	if e.FilesNote != "file list unavailable" {
		t.Fatalf("entry %+v", e)
	}
}

// Requirements: L1, T3
func TestAManifestThatMatchesItsHashButIsInvalidIsInvalidForGoodAndNothingIsFetched(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	// a record whose committed manifest breaks a rule (a name with a slash)
	bad := []byte(`{"v":1,"salt":"` + strings.Repeat("0a", 16) + `","files":[{"name":"a/b","size":1,"sha256":"` + strings.Repeat("a", 64) + `"}]}`)
	msha := core.SHA256Hex(bad)
	msg := a.sendWithManifest(room, "alice", "bad list", msha, bad)
	b.pullAll(room, a)
	b.tickFiles()
	var l store.FileList
	_ = b.st.View(context.Background(), func(rd *store.Reader) (err error) {
		l, _, err = rd.FileList(room, msg)
		return err
	})
	if l.State != store.ListInvalid {
		t.Fatalf("list %+v", l)
	}
	e := entryOf(t, b.inbox(room, "agent-b"), msg)
	if e.FilesNote != "invalid file list" || len(e.Files) != 0 {
		t.Fatalf("entry %+v", e)
	}
}

// sendWithManifest writes a message committing to msha with man's bytes
// held as its manifest, bypassing the manifest rules (an author that
// breaks them).
func (n *fnode) sendWithManifest(room, agent, text, msha string, man []byte) string {
	n.t.Helper()
	n.tick()
	s, err := n.m.write(context.Background(), room, agent, func(v *roomView, f *core.RecordFields) error {
		f.Kind, f.Text, f.Files = core.KindMessage, text, msha
		return nil
	})
	if err != nil {
		n.t.Fatal(err)
	}
	p, err := n.st.ReservePart(context.Background(), room, s.ID, msha, int64(len(man)), *n.clock)
	if err != nil {
		n.t.Fatal(err)
	}
	w, err := n.st.OpenPart(p, 0)
	if err != nil {
		n.t.Fatal(err)
	}
	w.Write(man)
	w.Close()
	if err := n.st.PartHave(context.Background(), p, int64(len(man))); err != nil {
		n.t.Fatal(err)
	}
	p.Have = int64(len(man))
	if err := n.st.CommitPart(context.Background(), p, store.HowSent, nil, *n.clock); err != nil {
		n.t.Fatal(err)
	}
	return s.ID
}

// Requirements: L1, T5
func TestAFileExpiresWithItsTextByEachHoldersClock(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "short lived"))
	sha := res.Files[0].SHA256
	b.pullAll(room, a)
	b.tickFiles()
	b.inbox(room, "agent-b")
	if r, err := b.fetch(room, "agent-b", sha); err != nil || r.State != FetchHere {
		t.Fatalf("%+v %v", r, err)
	}
	*b.clock = b.clock.Add(store.Retention + time.Minute)
	b.tickFiles()
	if heldFor(t, b, room, sha, msg) != "" {
		t.Fatal("bytes outlived their text")
	}
}

// Requirements: L1, T3
func TestARemovedMachineIsNotServedAFile(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "x", writeSource(t, "x.txt", "members only"))
	b.pullAll(room, a)
	a.change(room, Change{Kind: core.EntryRemove, Target: b.id})
	_, err := a.f.ServeGet(context.Background(), b.id, FileGet{Room: room, Msg: msg, SHA256: res.Files[0].SHA256})
	var pe *PullError
	if !errors.As(err, &pe) || pe.Code != CodeNotMember {
		t.Fatalf("a removed machine: %v", err)
	}
}

// Requirements: L1, T3
func TestANewcomerWithoutHistoryGetsNeitherTheListNorTheBytesOfAnEarlierFile(t *testing.T) {
	ns := filesNet(t, "a", "b", "c")
	a, b, c := ns[0], ns[1], ns[2]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "before c", writeSource(t, "x.txt", "history"))
	b.pullAll(room, a)
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "c"})
	c.pin(room, a.lnode)
	for _, sha := range []string{res.Files[0].SHA256, a.manifestOf(room, msg)} {
		if _, err := a.f.ServeGet(context.Background(), c.id, FileGet{Room: room, Msg: msg, SHA256: sha}); err != ErrNotHeldFile {
			t.Fatalf("a newcomer without history was served %s: %v", sha, err)
		}
	}
}

func (n *fnode) manifestOf(room, msg string) string {
	var m string
	_ = n.st.View(context.Background(), func(rd *store.Reader) (err error) {
		m, _, err = rd.FileSet(room, msg)
		return err
	})
	return m
}

// Requirements: L1, T3
func TestAReplacementListsOnlyTheFilesItsWriterListsAgain(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "two files", writeSource(t, "keep.txt", "kept"), writeSource(t, "drop.txt", "dropped"))
	keep, drop := res.Files[0].SHA256, res.Files[1].SHA256
	a.tick()
	r2, err := a.f.RequestSend(context.Background(), FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "one file"}, Replaces: msg,
		Files: []string{KeptPrefix + keep}})
	if err != nil {
		t.Fatal(err)
	}
	if c, err := a.r.Confirm(context.Background(), r2.Request, true); err != nil || c.State != store.RequestApproved {
		t.Fatalf("%+v %v", c, err)
	}
	if heldFor(t, a, room, keep, msg) == "" || heldFor(t, a, room, drop, msg) != "" {
		t.Fatal("the replacement did not keep exactly the file listed again")
	}
}

// Requirements: L1, T3
func TestKeepingAFileFailsIfTheReplacedVersionChangesBeforeApproval(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	msg, res := a.sendFiles(room, "alice", "one", writeSource(t, "k.txt", "kept?"))
	r2, err := a.f.RequestSend(context.Background(), FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "two"}, Replaces: msg,
		Files: []string{KeptPrefix + res.Files[0].SHA256}})
	if err != nil {
		t.Fatal(err)
	}
	a.tick()
	if _, err := a.m.Replace(context.Background(), room, "alice", msg, "meanwhile"); err != nil {
		t.Fatal(err)
	}
	c, err := a.r.Confirm(context.Background(), r2.Request, true)
	if err != nil || c.State != store.RequestFailed {
		t.Fatalf("%+v %v", c, err)
	}
}
