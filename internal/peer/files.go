package peer

// The file route (PROTOCOL 10.3, 10.5): GET and PUT
// /p/v2/file/<room>/<message>/<sha256> over the pinned mutual TLS of the
// pull exchange. The requesting machine is the TLS peer key; every
// decision is the node's (node.Files). Its limits are separate from the
// pull route's so files never delay pulls.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/node"
)

const pathFileV2 = "/p/v2/file/"

// FileHandler is what the file route needs from the node (node.Files).
type FileHandler interface {
	ServeGet(ctx context.Context, from string, q node.FileGet) (*node.FileServe, error)
	ServePut(ctx context.Context, from string, q node.FilePut, body io.Reader) (int64, error)
}

// fileDeadline is the most one response or upload may take, plus a margin
// for the headers (the node cuts slower transfers before it).
var fileDeadline = core.FileResponseMax + 5*time.Second

// fileRoute serves the file route with h.
func isFileRoute(p string) bool { return strings.HasPrefix(p, pathFileV2) }

// File requests are admitted by their own bucket per machine, so file
// traffic never spends the pull exchange's (B7).
func fileRoute(h FileHandler) http.Handler {
	lims := newLimiters()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		from := requestNode(r)
		if !lims.of(strangerRequest(r)).allow(from) {
			pullRefuse(w, &node.PullError{Status: http.StatusTooManyRequests, Code: node.CodeRateLimited, RetryAfter: time.Second})
			return
		}
		room, msg, sha, ok := filePath(r.URL.Path)
		rc := http.NewResponseController(w)
		_ = rc.SetWriteDeadline(time.Now().Add(fileDeadline))
		_ = rc.SetReadDeadline(time.Now().Add(fileDeadline))
		switch r.Method {
		case http.MethodGet:
			if !ok {
				pullRefuse(w, node.ErrNotHeldFile)
				return
			}
			off, okRange := rangeStart(r.Header.Get("Range"))
			if !okRange {
				pullRefuse(w, &node.PullError{Status: http.StatusRequestedRangeNotSatisfiable, Code: node.CodeBadRange})
				return
			}
			serveGet(w, r, h, from, node.FileGet{Room: room, Msg: msg, SHA256: sha, Offset: off})
		case http.MethodPut:
			if !ok {
				pullRefuse(w, node.ErrNotHeldFile)
				return
			}
			n, m, total, okRange := contentRange(r.Header.Get("Content-Range"))
			if !okRange || r.ContentLength != m-n+1 {
				pullRefuse(w, node.ErrNotHeldFile)
				return
			}
			have, err := h.ServePut(r.Context(), from, node.FilePut{Room: room, Msg: msg, SHA256: sha, Offset: n, Length: m - n + 1, Total: total},
				http.MaxBytesReader(w, r.Body, m-n+1))
			if err != nil {
				refuseFile(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]int64{"have": have})
		default:
			// HEAD is not served
			w.Header().Set("Allow", "GET, PUT")
			pullRefuse(w, &node.PullError{Status: http.StatusMethodNotAllowed, Code: node.CodeBadRequest})
		}
	})
}

func serveGet(w http.ResponseWriter, r *http.Request, h FileHandler, from string, q node.FileGet) {
	fs, err := h.ServeGet(r.Context(), from, q)
	if err != nil {
		refuseFile(w, err)
		return
	}
	if fs.Status == http.StatusAccepted {
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(fs.RetryAfter)))
		writeJSON(w, http.StatusAccepted, map[string]string{"state": fs.State})
		return
	}
	defer fs.Body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(fs.Length, 10))
	if fs.Status == http.StatusPartialContent {
		w.Header().Set("Content-Range", "bytes "+core.FormatOffset(fs.Offset)+"-"+core.FormatOffset(fs.Offset+fs.Length-1)+"/"+
			core.FormatOffset(fs.Total))
	}
	w.WriteHeader(fs.Status)
	// A revocation or an error mid-body ends the stream short: the
	// requester sees fewer bytes than Content-Length and keeps none of them
	// past what it verified. A reader slower than the minimum rate, one past
	// the total time, or a response whose permission ended (fs.Done) is cut
	// by moving the write deadline to now, which ends a write blocked on the
	// peer.
	limits := fs.Limits
	if limits.Window <= 0 {
		limits = node.TransferLimits()
	}
	rr := node.LimitRate(fs.Body, limits)
	rc := http.NewResponseController(w)
	// cut never touches the response once the handler is returning: a
	// watcher that wakes late, with both of its channels closed, does nothing.
	var mu sync.Mutex
	finished := false
	cut := func() {
		mu.Lock()
		defer mu.Unlock()
		if !finished {
			_ = rc.SetWriteDeadline(time.Now())
		}
	}
	defer func() { mu.Lock(); finished = true; mu.Unlock() }()
	stop := rr.Watch(cut)
	defer stop()
	if fs.Done != nil {
		ended := make(chan struct{})
		defer close(ended)
		go func() {
			select {
			case <-fs.Done:
				cut()
			case <-ended:
			}
		}()
	}
	_, _ = io.Copy(w, rr)
}

func refuseFile(w http.ResponseWriter, err error) {
	var pe *node.PullError
	if errors.As(err, &pe) {
		pullRefuse(w, pe)
		return
	}
	pullRefuse(w, &node.PullError{Status: http.StatusServiceUnavailable, Code: node.CodeUnavailable, RetryAfter: time.Second})
}

// filePath splits /p/v2/file/<room>/<msg>/<sha256>.
func filePath(p string) (room, msg, sha string, ok bool) {
	rest, found := strings.CutPrefix(p, pathFileV2)
	if !found {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || !core.ValidRoom(parts[0]) || len(parts[1]) != 32 || !isHex(parts[1]) || !core.ValidSHA256(parts[2]) {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// rangeStart reads an optional "bytes=<N>-": N digits only, at most 15,
// parsed before any arithmetic.
func rangeStart(h string) (int64, bool) {
	if h == "" {
		return 0, true
	}
	s, ok := strings.CutPrefix(h, "bytes=")
	if !ok {
		return 0, false
	}
	s, ok = strings.CutSuffix(s, "-")
	if !ok {
		return 0, false
	}
	return core.ParseOffset(s)
}

// contentRange reads "bytes N-M/<size>" with N ≤ M < size.
func contentRange(h string) (n, m, total int64, ok bool) {
	s, found := strings.CutPrefix(h, "bytes ")
	if !found {
		return 0, 0, 0, false
	}
	rng, size, found := strings.Cut(s, "/")
	if !found {
		return 0, 0, 0, false
	}
	a, b, found := strings.Cut(rng, "-")
	if !found {
		return 0, 0, 0, false
	}
	var ok1, ok2, ok3 bool
	n, ok1 = core.ParseOffset(a)
	m, ok2 = core.ParseOffset(b)
	total, ok3 = core.ParseOffset(size)
	if !ok1 || !ok2 || !ok3 || n > m || m >= total || total > core.MaxFileSize {
		return 0, 0, 0, false
	}
	return n, m, total, true
}

// ---- the requester ----

// FileClient is the file route's requester, on the pull client's
// connections (HTTP/2, pinned).
type FileClient struct {
	pc *PullClient
}

var _ node.FileTransport = (*FileClient)(nil)

// NewFileClient reaches peers through pc's clients and address book.
func NewFileClient(pc *PullClient) *FileClient { return &FileClient{pc: pc} }

// CanDial: this machine holds an address for peer that it would dial
// (the same rule as pulls: PullClient.usable and node.AddrBook.Rule).
func (c *FileClient) CanDial(peer string) bool {
	for _, a := range c.pc.book.Addrs(peer) {
		if c.pc.usable(a) {
			if _, refused := c.pc.book.Rule(peer, a); refused == "" {
				return true
			}
		}
	}
	return false
}

func (c *FileClient) do(ctx context.Context, peer, method, path string, hdr http.Header, body func() io.Reader, n int64) (*http.Response, error) {
	if c.pc.err != nil {
		return nil, c.pc.err
	}
	if !core.ValidNode(peer) {
		return nil, errors.New("peer: bad node id")
	}
	hc := c.pc.client(peer)
	lastErr := errors.New("peer: no usable address")
	for _, addr := range c.pc.book.Addrs(peer) {
		if !c.pc.dialled(peer, addr) {
			continue
		}
		u := url.URL{Scheme: "https", Host: addr, Path: path}
		var b io.Reader
		if body != nil {
			b = body()
		}
		hr, err := http.NewRequestWithContext(ctx, method, u.String(), b)
		if err != nil {
			continue
		}
		hr.Header = hdr.Clone()
		if b != nil {
			hr.ContentLength = n
		}
		resp, err := hc.Do(hr)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil || body != nil {
				break // an upload's body is read once
			}
			continue
		}
		c.pc.book.Worked(peer, addr)
		return resp, nil
	}
	return nil, lastErr
}

func filePathOf(room, msg, sha string) string { return pathFileV2 + room + "/" + msg + "/" + sha }

// GetFile asks peer for the bytes of q from q.Offset. A body returned is
// the caller's to close; its length is checked by the node.
func (c *FileClient) GetFile(ctx context.Context, peer string, q node.FileGet) (*node.FileReply, error) {
	ctx, cancel := context.WithTimeout(ctx, fileDeadline)
	hdr := http.Header{}
	if q.Offset > 0 {
		hdr.Set("Range", "bytes="+core.FormatOffset(q.Offset)+"-")
	}
	resp, err := c.do(ctx, peer, http.MethodGet, filePathOf(q.Room, q.Msg, q.SHA256), hdr, nil, 0)
	if err != nil {
		cancel()
		return nil, err
	}
	rep := &node.FileReply{Status: resp.StatusCode}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		rep.Offset, rep.Length, rep.Total = 0, resp.ContentLength, resp.ContentLength
		if resp.StatusCode == http.StatusPartialContent {
			n, m, total, ok := contentRange(resp.Header.Get("Content-Range"))
			if !ok || m-n+1 != resp.ContentLength {
				resp.Body.Close()
				cancel()
				return &node.FileReply{Status: 0}, errors.New("peer: bad Content-Range")
			}
			rep.Offset, rep.Total = n, total
		}
		rep.Body = &cancelBody{resp.Body, cancel}
		return rep, nil
	case http.StatusAccepted:
		var b struct {
			State string `json:"state"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 256)).Decode(&b)
		rep.State = b.State
		rep.RetryAfter = retryAfter(resp)
	case http.StatusNotFound:
		rep.Old = !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json")
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		rep.RetryAfter = retryAfter(resp)
	}
	resp.Body.Close()
	cancel()
	return rep, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel func()
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func retryAfter(resp *http.Response) time.Duration {
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 && s <= 86400 {
		return time.Duration(s) * time.Second
	}
	return 0
}

// PutFile uploads one chunk of q to peer and returns the status and the
// bytes it now holds.
func (c *FileClient) PutFile(ctx context.Context, peer string, q node.FilePut, body io.Reader) (int, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, fileDeadline)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/octet-stream")
	hdr.Set("Content-Range", "bytes "+core.FormatOffset(q.Offset)+"-"+core.FormatOffset(q.Offset+q.Length-1)+"/"+core.FormatOffset(q.Total))
	resp, err := c.do(ctx, peer, http.MethodPut, filePathOf(q.Room, q.Msg, q.SHA256), hdr, func() io.Reader { return body }, q.Length)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, 0, nil
	}
	var b struct {
		Have int64 `json:"have"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256)).Decode(&b); err != nil {
		return 0, 0, err
	}
	return http.StatusOK, b.Have, nil
}
