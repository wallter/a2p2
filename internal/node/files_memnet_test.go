package node

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// memFiles is the file route between Files over memory, beside a memNet:
// a machine in the memNet's noInbound cannot be dialled here either.
type memFiles struct {
	net   *memNet
	mu    sync.Mutex
	nodes map[string]*Files
	// served records each GET answered with bytes: server, requester, sha.
	served [][3]string
	// puts records each upload accepted: sender, receiver, sha.
	puts [][3]string
	// asked records each GET sent, answered or not: requester, server, sha.
	asked [][3]string
}

func newMemFiles(net *memNet) *memFiles { return &memFiles{net: net, nodes: map[string]*Files{}} }

type memFileLink struct {
	n    *memFiles
	from string
}

func (l memFileLink) CanDial(peer string) bool {
	l.n.net.mu.Lock()
	defer l.n.net.mu.Unlock()
	return !l.n.net.noInbound[peer] && !l.n.net.down[peer]
}

func (l memFileLink) server(peer string) *Files {
	l.n.mu.Lock()
	defer l.n.mu.Unlock()
	return l.n.nodes[peer]
}

func (l memFileLink) GetFile(ctx context.Context, peer string, q FileGet) (*FileReply, error) {
	l.n.mu.Lock()
	l.n.asked = append(l.n.asked, [3]string{l.from, peer, q.SHA256})
	l.n.mu.Unlock()
	s := l.server(peer)
	if s == nil || !l.CanDial(peer) {
		return nil, errUnreachable
	}
	fs, err := s.ServeGet(ctx, l.from, q)
	var pe *PullError
	if errors.As(err, &pe) {
		return &FileReply{Status: pe.Status, RetryAfter: pe.RetryAfter}, nil
	}
	if err != nil {
		return nil, err
	}
	if fs.Body == nil {
		return &FileReply{Status: fs.Status, State: fs.State, RetryAfter: fs.RetryAfter}, nil
	}
	// the bytes go through a buffer, as over a connection
	b, err := io.ReadAll(fs.Body)
	fs.Body.Close()
	if err != nil {
		return nil, err
	}
	l.n.mu.Lock()
	l.n.served = append(l.n.served, [3]string{peer, l.from, q.SHA256})
	l.n.mu.Unlock()
	return &FileReply{Status: fs.Status, Total: fs.Total, Offset: fs.Offset, Length: int64(len(b)),
		Body: io.NopCloser(bytes.NewReader(b))}, nil
}

func (l memFileLink) PutFile(ctx context.Context, peer string, q FilePut, body io.Reader) (int, int64, error) {
	s := l.server(peer)
	if s == nil {
		return 0, 0, errUnreachable
	}
	// an upload goes over the connection the receiver's pull opened
	have, err := s.ServePut(ctx, l.from, q, body)
	var pe *PullError
	if errors.As(err, &pe) {
		return pe.Status, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	l.n.mu.Lock()
	l.n.puts = append(l.n.puts, [3]string{l.from, peer, q.SHA256})
	l.n.mu.Unlock()
	return 200, have, nil
}

// fnode is a pnode with attachments.
type fnode struct {
	*pnode
	f  *Files
	fn *memFiles
}

func newFNode(t *testing.T, net *memNet, fn *memFiles, name string, clock *time.Time) *fnode {
	t.Helper()
	p := newPNode(t, net, name, newLNode(t, name, clock))
	n := &fnode{pnode: p, fn: fn}
	n.attachFiles()
	return n
}

func (n *fnode) attachFiles() {
	n.f = NewFiles(n.r, memFileLink{n.fn, n.id}, FilesOpts{Chunk: 4})
	n.x.UseFiles(n.f)
	n.fn.mu.Lock()
	n.fn.nodes[n.id] = n.f
	n.fn.mu.Unlock()
	if err := n.f.Start(context.Background()); err != nil {
		n.t.Fatal(err)
	}
}

// sendFiles asks to send text with files at paths and approves it.
func (n *fnode) sendFiles(room, agent, text string, paths ...string) (string, FileSendResult) {
	n.t.Helper()
	n.tick()
	res, err := n.f.RequestSend(context.Background(), FileSend{Draft: Draft{Room: room, Agent: agent, Text: text}, Files: paths})
	if err != nil {
		n.t.Fatal(err)
	}
	c, err := n.f.ApproveSend(context.Background(), res.Request, res.Terms)
	if err != nil || c.State != store.RequestApproved {
		n.t.Fatalf("approve: %+v %v", c, err)
	}
	return c.Entry, res
}

func (n *fnode) fetch(room, agent, sha string) (FetchResult, error) {
	// Fetch returns as soon as its download ends (done, refused, or no
	// holder to dial); the wait is the longest the local API allows, which
	// only a real hang reaches, so a loaded machine never cuts a download.
	r, err := n.f.Fetch(context.Background(), FetchOpts{Agent: agent, Room: room, SHA256: sha,
		Wait: time.Duration(core.MaxFetchWaitMS) * time.Millisecond})
	n.f.Wait()
	return r, err
}

// tickFiles runs the background work and waits for its uploads.
func (n *fnode) tickFiles() {
	n.f.Tick(context.Background())
	n.f.Wait()
}

func (n *fnode) pullAll(room string, s *fnode) {
	n.t.Helper()
	n.mustPull(room, s.pnode, PullOpts{Offer: true})
	n.f.Wait()
	s.f.Wait()
}

func writeSource(t *testing.T, name, data string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// fileRoom is a room of a and b, b with history, both with agents.
func fileRoom(t *testing.T, a, b *fnode, more ...*fnode) string {
	t.Helper()
	room := a.create("team")
	all := append([]*fnode{b}, more...)
	for _, o := range all {
		a.change(room, Change{Kind: core.EntryAdd, Target: o.id, Name: o.name, History: true})
	}
	for _, o := range all {
		o.pin(room, a.lnode)
		o.pull(room, a.lnode)
		o.register("agent-" + o.name)
	}
	a.register("alice")
	return room
}
