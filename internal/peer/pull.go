package peer

// POST /p/v2/pull (PROTOCOL section 4): the pull exchange on the
// peer listener, beside /p/v2/join. The requester's node id comes from
// the TLS certificate only; the body is size-checked before it is read and
// strict JSON (checked by internal/node); the per-peer rate and concurrency
// limits and the deadlines are those of 4.6; refusals carry the statuses
// and bodies of 4.5. PullClient is the requester's side: one HTTP/2
// connection per peer shared by its requests, pinned to the peer's key,
// its addresses tried in turn (N2).

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/node"
)

const pathPullV2 = "/p/v2/pull"

// Limits and deadlines of PROTOCOL 4.6. Variables so tests can
// shorten them.
var (
	maxPullBody     int64 = node.MaxRequestBytes
	maxPullResp     int64 = node.MaxResponseBytes
	pullTimeout           = 30 * time.Second // client, other requests; added to wait_ms for a waiting one
	pullSlack             = 30 * time.Second // server write deadline: wait_ms + this
	pullRatePerSec        = 20.0
	pullRateBurst         = 40.0
	pullConcurrent        = 4 // per peer, requests other than waiting pulls
	pullTLSTimeout        = 5 * time.Second
	pullIdleTimeout       = 120 * time.Second
	// Dials to addresses that never completed a pinned handshake, for the
	// whole daemon (one PullClient), and for each peer within that, so one
	// member's addresses cannot spend it all.
	pullDialRate      = 2.0
	pullDialBurst     = 8.0
	pullPeerDialRate  = 0.5
	pullPeerDialBurst = 4.0
)

// PullHandler serves the pull exchange (node.Exchange) on /p/v2/pull.
type PullHandler interface {
	Serve(ctx context.Context, from string, q *node.PullRequest) (*node.PullResponse, error)
}

// pullServer is the endpoint: limits per authenticated peer, then the node.
type pullServer struct {
	h PullHandler
	// lim is the members' buckets; strangers holds those of keys that
	// share no room here, so they never fill the members' table.
	lim, strangers *pullLimiter
	// closing ends when the listener begins to shut down: every request
	// in progress, a held one above all, is cancelled then and refused
	// 503, rather than lasting into the grace period.
	closing context.Context
	mu      sync.Mutex
	// busy counts, per peer, requests in progress that are not held; a
	// waiting pull gives up its slot when it enters a hold (one per room
	// per peer: the node releases the older one).
	busy map[string]int
}

func newPullServer(closing context.Context, h PullHandler) *pullServer {
	return &pullServer{h: h, lim: newPullLimiter(pullRatePerSec, pullRateBurst),
		strangers: newPullLimiter(pullRatePerSec, pullRateBurst), closing: closing, busy: map[string]int{}}
}

func (s *pullServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	from := requestNode(r) // step 1: from the certificate, never the body
	// The rate limit for this peer, before anything of the body is read.
	lim := s.lim
	if strangerRequest(r) {
		lim = s.strangers
	}
	if ok, wait := lim.take(from); !ok {
		pullRefuse(w, &node.PullError{Status: http.StatusTooManyRequests, Code: node.CodeRateLimited, RetryAfter: wait})
		return
	}
	// And its concurrency: a slot until the request is answered or held.
	if !s.enter(from) {
		pullRefuse(w, &node.PullError{Status: http.StatusTooManyRequests, Code: node.CodeRateLimited,
			Detail: "concurrent", RetryAfter: time.Second})
		return
	}
	var once sync.Once
	leave := func() { once.Do(func() { s.leave(from) }) }
	defer leave()
	// Step 2: size before reading, strict JSON, version and field limits.
	if r.ContentLength > maxPullBody {
		pullRefuse(w, &node.PullError{Status: http.StatusRequestEntityTooLarge, Code: node.CodeTooLarge})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPullBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			pullRefuse(w, &node.PullError{Status: http.StatusRequestEntityTooLarge, Code: node.CodeTooLarge})
		} else {
			pullRefuse(w, &node.PullError{Status: http.StatusBadRequest, Code: node.CodeBadRequest, Detail: "body"})
		}
		return
	}
	q, perr := node.DecodePullRequest(body)
	if perr != nil {
		pullRefuse(w, perr)
		return
	}
	// The write deadline: wait_ms + 30 s (the server's own is 30 s).
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Duration(q.WaitMS)*time.Millisecond + pullSlack)); err != nil {
		pullRefuse(w, &node.PullError{Status: http.StatusServiceUnavailable, Code: node.CodeUnavailable})
		return
	}
	ctx, cancel := context.WithCancel(node.WithHoldNotice(r.Context(), leave))
	defer cancel()
	defer context.AfterFunc(s.closing, cancel)()
	// The source, by which requests that carry entries from keys that are
	// not members are shared out (node.WithSource); it grants nothing.
	resp, err := s.h.Serve(node.WithSource(ctx, r.RemoteAddr), from, q)
	if err != nil {
		var pe *node.PullError
		switch {
		case errors.As(err, &pe):
		case s.closing.Err() != nil:
			pe = &node.PullError{Status: http.StatusServiceUnavailable, Code: node.CodeUnavailable, Detail: "closing"}
		case r.Context().Err() != nil:
			return // the requester went away
		default:
			pe = &node.PullError{Status: http.StatusServiceUnavailable, Code: node.CodeUnavailable}
		}
		pullRefuse(w, pe)
		return
	}
	b, err := json.Marshal(resp)
	if err != nil || int64(len(b)) > maxPullResp {
		pullRefuse(w, &node.PullError{Status: http.StatusServiceUnavailable, Code: node.CodeUnavailable})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// Unwrap lets http.ResponseController reach the connection beneath the
// access log's writer, so a waiting pull can move its write deadline.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *pullServer) enter(from string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[from] >= pullConcurrent {
		return false
	}
	s.busy[from]++
	return true
}

func (s *pullServer) leave(from string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[from]--; s.busy[from] <= 0 {
		delete(s.busy, from)
	}
}

// pullRefuse writes a refusal of PROTOCOL 4.5, with Retry-After (in
// whole seconds, at least 1) on a 429 or 503.
func pullRefuse(w http.ResponseWriter, e *node.PullError) {
	w.Header().Set("Content-Type", "application/json")
	if e.Status == http.StatusTooManyRequests || e.Status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(e.RetryAfter)))
	}
	w.WriteHeader(e.Status)
	_, _ = w.Write(node.PullErrorBody(e))
}

func retrySeconds(d time.Duration) int {
	return max(1, int(math.Ceil(d.Seconds())))
}

// pullLimiter is a token bucket per peer: rate per second, burst. Full
// buckets are dropped when the map is large; live ones never are.
type pullLimiter struct {
	mu          sync.Mutex
	now         func() time.Time
	rate, burst float64
	m           map[string]*bucket
}

func newPullLimiter(rate, burst float64) *pullLimiter {
	return &pullLimiter{now: time.Now, rate: rate, burst: burst, m: map[string]*bucket{}}
}

// take spends a token of key, or says how long until one is there.
func (l *pullLimiter) take(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.m[key]
	if b == nil {
		if len(l.m) >= rateMaxKeys {
			for k, o := range l.m {
				if o.tokens+now.Sub(o.last).Seconds()*l.rate >= l.burst {
					delete(l.m, k)
				}
			}
		}
		if len(l.m) >= rateMaxKeys {
			return false, time.Second
		}
		b = &bucket{tokens: l.burst, last: now}
		l.m[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	}
	b.tokens--
	return true, 0
}

// give returns a token taken from key that was not used.
func (l *pullLimiter) give(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.m[key]; b != nil {
		b.tokens = min(l.burst, b.tokens+1)
	}
}

// ---- client ----

// PullClient implements node.PullTransport over pinned mutual TLS 1.3 and
// HTTP/2: one transport per peer, so that its requests (waiting pulls of
// several rooms among them) share one connection.
type PullClient struct {
	cert  tls.Certificate
	err   error
	book  *node.AddrBook
	dials *pullLimiter // to addresses never verified, for every peer
	// peerDials is that budget per peer.
	peerDials *pullLimiter
	// Usable, if set, says whether an address is worth dialling from here
	// (a unique-local IPv6 address of another site is not); set before use.
	Usable func(addr string) bool
	// Resolve looks up a name another machine gave (node.DialTargets); nil:
	// the system's resolver. Set before use.
	Resolve node.Resolver

	mu      sync.Mutex
	clients map[string]*http.Client
}

var _ node.PullTransport = (*PullClient)(nil)

// NewPullClient pulls as id, reaching peers at the addresses in book.
func NewPullClient(id *Identity, book *node.AddrBook) *PullClient {
	cert, err := selfSigned(id)
	return &PullClient{cert: cert, err: err, book: book, dials: newPullLimiter(pullDialRate, pullDialBurst),
		peerDials: newPullLimiter(pullPeerDialRate, pullPeerDialBurst), clients: map[string]*http.Client{}}
}

// Close closes idle connections.
func (c *PullClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, hc := range c.clients {
		hc.CloseIdleConnections()
	}
}

func (c *PullClient) client(peer string) *http.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if hc := c.clients[peer]; hc != nil {
		return hc
	}
	if len(c.clients) >= maxClients {
		for k, hc := range c.clients {
			hc.CloseIdleConnections()
			delete(c.clients, k)
			break
		}
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{c.cert},
		// As in Transport.client: no CA, so chain verification is replaced
		// by the check of the key itself, inside the handshake, before any
		// request byte is written.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			got, err := certNode(raw)
			if err != nil {
				return err
			}
			if got != peer {
				return fmt.Errorf("peer: expected node %s, server is %s", core.ShortNode(peer), core.ShortNode(got))
			}
			return nil
		},
	}
	protos := new(http.Protocols)
	protos.SetHTTP2(true)
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		// An address another machine gave is checked before anything is
		// spent on it, and a name it gave is looked up here, once, and
		// only the addresses found that pass are dialled (F5, F54). An
		// address the rule refuses costs nothing. Each literal address
		// dialled is charged to the dial budgets unless that literal itself
		// completed a pinned handshake with peer. A name, verified or not,
		// may resolve anywhere since: it is charged once before it is looked
		// up (the lookup and its first address), and again for each further
		// address it gives that did not complete a handshake.
		given, refused := c.book.Rule(peer, addr)
		if refused != "" {
			return nil, node.ErrAddrRefused
		}
		_, perr := netip.ParseAddrPort(addr)
		name := perr != nil
		if name && !c.charge(peer) {
			return nil, errDialBudget
		}
		targets, err := node.DialTargets(ctx, addr, given, c.Resolve)
		if err != nil {
			return nil, err
		}
		var last error
		for i, t := range targets {
			free := (name && i == 0) || c.verified(peer, t)
			if !free && !c.charge(peer) {
				if last == nil {
					last = errDialBudget
				}
				return nil, last
			}
			conn, err := dialer.DialContext(ctx, network, t)
			if err == nil {
				return conn, nil
			}
			last = err
			if ctx.Err() != nil {
				break
			}
		}
		return nil, last
	}
	hc := &http.Client{
		Transport: &http.Transport{
			DialContext:         dial,
			TLSClientConfig:     cfg,
			TLSHandshakeTimeout: pullTLSTimeout,
			IdleConnTimeout:     pullIdleTimeout,
			Protocols:           protos,
			HTTP2:               &http.HTTP2Config{SendPingTimeout: 30 * time.Second, PingTimeout: 15 * time.Second},
			Proxy:               nil, // never route peer traffic through an HTTP proxy
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("peer: redirects are not followed")
		},
	}
	c.clients[peer] = hc
	return hc
}

func (c *PullClient) usable(addr string) bool {
	return validAddr(addr) && (c.Usable == nil || c.Usable(addr))
}

// dialled says whether addr of peer is one this machine dials at all
// (node.AddrBook.Rule) and can route from here (usable). An address
// refused here is noted as failing, so one an earlier version stored
// ages out, and is charged nothing: no token of the requester's budget
// or of the dial budgets.
func (c *PullClient) dialled(peer, addr string) bool {
	if !c.usable(addr) {
		return false
	}
	if _, refused := c.book.Rule(peer, addr); refused != "" {
		c.book.Failed(peer, addr, time.Now())
		return false
	}
	return true
}

// errDialBudget: the daemon's budget of dials to unverified addresses is
// spent for now; the address is not tried and not counted as failing.
var errDialBudget = errors.New("peer: dial budget for unverified addresses spent")

// charge takes one dial from peer's budget and the machine's; false, and
// nothing taken, when either is spent.
func (c *PullClient) charge(peer string) bool {
	if ok, _ := c.peerDials.take(peer); !ok {
		return false
	}
	if ok, _ := c.dials.take(""); !ok {
		c.peerDials.give(peer) // not dialled: the peer's share is not spent
		return false
	}
	return true
}

// verified says whether addr of peer ever completed a pinned handshake.
func (c *PullClient) verified(peer, addr string) bool {
	for _, d := range c.book.Dial(peer) {
		if d.Addr == addr {
			return d.Good
		}
	}
	return false
}

// Pull sends req to peer at each of its addresses in turn, in the book's
// order (good ones first, failing ones last), until one answers. An
// address that fails (unreachable, another key, a timeout) is noted, so
// the next attempt starts at the next one; one that answers is marked
// good. A refusal is a *node.PullError.
func (c *PullClient) Pull(ctx context.Context, peer string, req *node.PullRequest) (*node.PullResponse, error) {
	caller := ctx
	if c.err != nil {
		return nil, c.err
	}
	if !core.ValidNode(peer) {
		return nil, errors.New("peer: bad node id")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > maxPullBody {
		return nil, &node.PullError{Status: http.StatusRequestEntityTooLarge, Code: node.CodeTooLarge, Detail: "not sent"}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.WaitMS)*time.Millisecond+pullTimeout)
	defer cancel()
	hc := c.client(peer)
	lastErr := errors.New("peer: no usable address")
	tried := false
	for _, addr := range c.book.Addrs(peer) {
		if !c.dialled(peer, addr) {
			continue
		}
		// Each attempt may reach the peer: one after the first pays the
		// requester's budget again (node.PullAttempt).
		if tried {
			if err := node.PullAttempt(ctx); err != nil {
				return nil, errors.Join(lastErr, err)
			}
		}
		tried = true
		u := url.URL{Scheme: "https", Host: addr, Path: pathPullV2}
		hr, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
		if err != nil {
			continue
		}
		hr.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(hr)
		if err != nil {
			lastErr = err
			if caller.Err() != nil {
				break // the caller gave up (a waiting pull abandoned): no fault of the address
			}
			if !errors.Is(err, errDialBudget) {
				c.book.Failed(peer, addr, time.Now())
			}
			if ctx.Err() != nil {
				break // timed out at this address: the next attempt starts at the next one
			}
			continue // unreachable, or another key at this address: the next one
		}
		c.book.Worked(peer, addr)
		return readPull(resp)
	}
	return nil, lastErr
}

func readPull(resp *http.Response) (*node.PullResponse, error) {
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxPullResp+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxPullResp {
		return nil, errors.New("peer: pull response too large")
	}
	if resp.StatusCode == http.StatusOK {
		return node.DecodePullResponse(b)
	}
	var retry time.Duration
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 && s <= 3600 {
		retry = time.Duration(s) * time.Second
	}
	return nil, node.DecodePullError(resp.StatusCode, b, retry)
}
