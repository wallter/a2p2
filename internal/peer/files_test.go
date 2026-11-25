package peer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// fileH serves the pull exchange and the file route.
type fileH struct {
	*pullH
	f atomic.Pointer[node.Files]
}

func (h *fileH) ServeGet(ctx context.Context, from string, q node.FileGet) (*node.FileServe, error) {
	return h.f.Load().ServeGet(ctx, from, q)
}

func (h *fileH) ServePut(ctx context.Context, from string, q node.FilePut, body io.Reader) (int64, error) {
	return h.f.Load().ServePut(ctx, from, q, body)
}

// newFPM is a pm with attachments; chunk 8 bytes, so a file takes several
// responses.
func newFPM(t *testing.T, name string, listen bool) (*pm, *node.Files) {
	t.Helper()
	p := newPM(t, name, false)
	var addrs []string
	var h *fileH
	if listen {
		h = &fileH{pullH: &pullH{}}
		p.h = h.pullH
		p.addr = serve(t, p.id, h, nil)
		addrs = []string{p.addr}
	}
	p.x = node.NewExchange(p.r, p.cl, node.ExchangeOpts{Book: p.book, Addrs: addrs})
	f := node.NewFiles(p.r, NewFileClient(p.cl), node.FilesOpts{Chunk: 8})
	p.x.UseFiles(f)
	if listen {
		h.x.Store(p.x)
		h.f.Store(f)
	}
	if err := f.Start(bg); err != nil {
		t.Fatal(err)
	}
	return p, f
}

func sendFile(t *testing.T, p *pm, f *node.Files, room, name, data string) (string, string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(src, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := f.RequestSend(bg, node.FileSend{Draft: node.Draft{Room: room, Agent: p.agent, Text: "a file"}, Files: []string{src}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.ApproveSend(bg, res.Request, res.Terms)
	if err != nil || c.State != store.RequestApproved {
		t.Fatalf("%+v %v", c, err)
	}
	return c.Entry, res.Files[0].SHA256
}

func showAll(t *testing.T, p *pm, room string) {
	t.Helper()
	v, err := p.m.Inbox(bg, room, p.agent)
	if err == nil {
		err = p.m.Showed(bg, room, p.agent, v.Entries, true)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// TestTheFileRouteServesAndResumesOverPinnedTLS: the list and the bytes
// come by GET over the pull connection, 8 bytes a response, each range
// continuing the last, and the copy holds exactly the author's bytes.
// Requirements: L1, T3
func TestTheFileRouteServesAndResumesOverPinnedTLS(t *testing.T) {
	a, af := newFPM(t, "alice", true)
	b, bf := newFPM(t, "bob", true)
	room := group(t, a, b)
	a.knows(b)
	data := "twenty-nine bytes of content"
	msg, sha := sendFile(t, a, af, room, "notes.txt", data)
	b.mustPull(room, a, node.PullOpts{})
	bf.Tick(bg)
	showAll(t, b, room)
	r, err := bf.Fetch(bg, node.FetchOpts{Agent: b.agent, Room: room, SHA256: sha, Wait: 10 * time.Second})
	if err != nil || r.State != node.FetchHere || r.Message != msg {
		t.Fatalf("fetch %+v %v", r, err)
	}
	if got, _ := os.ReadFile(r.Path); string(got) != data {
		t.Fatalf("copy %q", got)
	}
}

// TestAnAuthorThatTakesNoConnectionsUploadsWhatItsPullAnswerWants: the
// listener's answer carries the want; the author PUTs the bytes.
// Requirements: L1, N1
func TestAnAuthorThatTakesNoConnectionsUploadsWhatItsPullAnswerWants(t *testing.T) {
	b, bf := newFPM(t, "bob", true)
	a, af := newFPM(t, "alice", false)
	room := group(t, b, a)
	msg, sha := sendFile(t, a, af, room, "w.bin", "behind a wall, uploaded")
	a.mustPull(room, b, node.PullOpts{Offer: true})
	bf.Tick(bg) // the author cannot be dialled: a want of the list
	a.mustPull(room, b, node.PullOpts{})
	af.Wait()
	showAll(t, b, room)
	r, err := bf.Fetch(bg, node.FetchOpts{Agent: b.agent, Room: room, SHA256: sha, Wait: time.Second})
	if err != nil || r.State != node.FetchComing {
		t.Fatalf("first fetch %+v %v", r, err)
	}
	a.mustPull(room, b, node.PullOpts{})
	af.Wait()
	r, err = bf.Fetch(bg, node.FetchOpts{Agent: b.agent, Room: room, SHA256: sha, Wait: time.Second})
	if err != nil || r.State != node.FetchHere || r.Message != msg {
		t.Fatalf("second fetch %+v %v", r, err)
	}
}

// rawFile sends one request on the file route as from, over pinned TLS.
func rawFile(t *testing.T, from *Identity, to *pm, method, path, rng string) (int, string, []byte) {
	t.Helper()
	cert, err := selfSigned(from)
	if err != nil {
		t.Fatal(err)
	}
	protos := new(http.Protocols)
	protos.SetHTTP2(true)
	tr := &http.Transport{Protocols: protos, TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			got, err := certNode(raw)
			if err == nil && got != to.node() {
				err = errors.New("wrong key")
			}
			return err
		}}}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(bg, method, "https://"+to.addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), b
}

// TestEveryNotHeldAnswerOnTheWireIsTheSameBytes: an
// unknown hash, an unknown message, a file of a message the peer may not
// read and a malformed path all get one status, type and body, decided
// before any file is opened; HEAD is not served and a range of 16 digits
// is refused before arithmetic.
// Requirements: L1, T3
func TestEveryNotHeldAnswerOnTheWireIsTheSameBytes(t *testing.T) {
	a, af := newFPM(t, "alice", true)
	b, _ := newFPM(t, "bob", true)
	room := group(t, a, b)
	if err := a.st.SetFileMode(bg, room, store.FilesOff, time.Now()); err != nil {
		t.Fatal(err)
	}
	msg, sha := sendFile(t, a, af, room, "x.txt", "only some may read")
	type ans struct {
		code  int
		ctype string
		body  string
	}
	want := ans{404, "application/json", `{"error":"not_held"}`}
	paths := map[string]string{
		"unknown hash":    filePathOf(room, msg, strings.Repeat("e", 64)),
		"unknown message": filePathOf(room, strings.Repeat("1", 32), sha),
		"malformed":       pathFileV2 + room + "/" + msg,
		"upper hex":       filePathOf(room, msg, strings.ToUpper(sha)),
	}
	start := time.Now()
	for name, p := range paths {
		code, ct, body := rawFile(t, b.id, a, http.MethodGet, p, "")
		if got := (ans{code, ct, string(body)}); got != want {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("not-held answers took %v", d)
	}
	if code, _, _ := rawFile(t, b.id, a, http.MethodHead, filePathOf(room, msg, sha), ""); code != http.StatusMethodNotAllowed {
		t.Errorf("HEAD: %d", code)
	}
	if code, _, _ := rawFile(t, b.id, a, http.MethodGet, filePathOf(room, msg, sha), "bytes=1000000000000000-"); code != 416 {
		t.Errorf("16 digits: %d", code)
	}
	if code, _, body := rawFile(t, b.id, a, http.MethodGet, filePathOf(room, msg, sha), "bytes=3-"); code != 206 || string(body) != "y some m" {
		t.Errorf("a range: %d %q", code, body)
	}
}

// TestAnOffsetPastTheSizeIsRefusedBeforeArithmetic, the parser half.
// Requirements: L1, T3
func TestTheRangeParsersTakeOnlyDigitsWithinBounds(t *testing.T) {
	for h, ok := range map[string]bool{"": true, "bytes=0-": true, "bytes=15-": true, "bytes=-5": false, "bytes=1-2": false,
		"bytes=+1-": false, "bytes=01-": false, "bytes=" + strings.Repeat("9", 16) + "-": false, "items=1-": false} {
		if _, got := rangeStart(h); got != ok {
			t.Errorf("Range %q: %v", h, got)
		}
	}
	for h, ok := range map[string]bool{"bytes 0-7/8": true, "bytes 8-7/9": false, "bytes 0-8/8": false, "bytes 0-1/*": false,
		"bytes 0-1/" + core.FormatOffset(core.MaxFileSize+1): false, "bytes -1-2/9": false} {
		if _, _, _, got := contentRange(h); got != ok {
			t.Errorf("Content-Range %q: %v", h, got)
		}
	}
}
