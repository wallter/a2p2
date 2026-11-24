package peer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// Handler is what the listener needs from the node. fromNode is always the
// identity proven by the TLS handshake.
type Handler interface {
	Receive(ctx context.Context, fromNode string, req node.DeliverReq, refresh func(room string)) []node.Result
	HandleJoin(ctx context.Context, fromNode string, req node.JoinReq) (store.Room, error)
	HandleRoom(ctx context.Context, fromNode string, req node.RoomReq) (store.Room, error)
}

// Wire limits and timeouts. Variables only so tests can shorten them.
var (
	maxDeliverBody int64 = core.MaxBatch*core.MaxEnvelope + 64<<10 // slack for "versions" and JSON framing
	maxSmallBody   int64 = 16 << 10                                // join and room requests
	maxRespBody    int64 = 1 << 20                                 // results or a 64-member room
	dialTimeout          = 3 * time.Second
	requestTimeout       = 15 * time.Second
	maxClients           = 256 // cached per-destination HTTP clients
	maxConns             = 256 // concurrent inbound TCP connections, counted before TLS
	maxVersions          = 256 // entries accepted in a deliver request's "versions" map
)

const (
	pathDeliver = "/p/v1/deliver"
	pathJoin    = "/p/v1/join"
	pathRoom    = "/p/v1/room"
)

// ---- certificates ----

// selfSigned makes an in-memory certificate for the node key. There is no CA:
// the certificate is only a container that lets TLS carry the public key, and
// the key itself is the identity. Validity dates are therefore not checked by
// either side; they are set wide so ordinary TLS stacks do not choke on them.
func selfSigned(id *Identity) (tls.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: id.NodeID()},
		NotBefore:    now.Add(-24 * time.Hour),
		NotAfter:     now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, id.Key.Public(), id.Key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: id.Key}, nil
}

// certNode checks that the peer presented exactly one self-signed Ed25519
// certificate whose CN is its own public key, and returns that node id.
// crypto/tls has already checked, separately, that the peer holds the private
// key (the CertificateVerify signature), so a valid result here proves the
// peer is that node.
func certNode(rawCerts [][]byte) (string, error) {
	if len(rawCerts) != 1 {
		return "", errors.New("peer: want exactly one certificate")
	}
	c, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return "", fmt.Errorf("peer: bad certificate: %w", err)
	}
	pub, ok := c.PublicKey.(ed25519.PublicKey)
	if !ok {
		return "", errors.New("peer: certificate key is not Ed25519")
	}
	if err := c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature); err != nil {
		return "", errors.New("peer: certificate is not self-signed by its key")
	}
	id := hex.EncodeToString(pub)
	if c.Subject.CommonName != id {
		return "", errors.New("peer: certificate CN does not match its key")
	}
	return id, nil
}

// ---- client ----

// Transport implements node.Transport over mutual TLS.
type Transport struct {
	id   *Identity
	cert tls.Certificate
	err  error // certificate generation failure, reported on every call

	mu      sync.Mutex
	clients map[string]*http.Client // one per destination node id, for connection reuse
}

var _ node.Transport = (*Transport)(nil)

func NewTransport(id *Identity) *Transport {
	cert, err := selfSigned(id)
	return &Transport{id: id, cert: cert, err: err, clients: map[string]*http.Client{}}
}

func (t *Transport) Deliver(ctx context.Context, to store.Member, req node.DeliverReq) ([]node.Result, error) {
	var resp struct {
		Results []node.Result `json:"results"`
	}
	err := t.call(ctx, to, pathDeliver, req, &resp)
	return resp.Results, err
}

func (t *Transport) Join(ctx context.Context, owner store.Member, req node.JoinReq) (store.Room, error) {
	var r store.Room
	err := t.call(ctx, owner, pathJoin, req, &r)
	return r, err
}

func (t *Transport) Room(ctx context.Context, owner store.Member, req node.RoomReq) (store.Room, error) {
	var r store.Room
	err := t.call(ctx, owner, pathRoom, req, &r)
	return r, err
}

// client returns the cached HTTP client pinned to node, creating it if needed.
func (t *Transport) client(nodeID string) *http.Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c := t.clients[nodeID]; c != nil {
		return c
	}
	if len(t.clients) >= maxClients { // bounded: drop an arbitrary one
		for k, c := range t.clients {
			c.CloseIdleConnections()
			delete(t.clients, k)
			break
		}
	}
	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{t.cert},
		// There is no CA and no hostname to check, so the default chain
		// verification cannot apply and is switched off. It is replaced, not
		// dropped: VerifyPeerCertificate runs inside the handshake and fails
		// it unless the server's key is exactly the node we meant to reach.
		// TLS 1.3 then still checks the server's signature with that key.
		// The HTTP request is only written after the handshake succeeds.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			got, err := certNode(raw)
			if err != nil {
				return err
			}
			if got != nodeID {
				return fmt.Errorf("peer: expected node %s, server is %s", core.ShortNode(nodeID), core.ShortNode(got))
			}
			return nil
		},
	}
	c := &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig:       tlsCfg,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: requestTimeout,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       90 * time.Second,
			Proxy:                 nil, // never route peer traffic through an HTTP proxy
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("peer: redirects are not followed")
		},
	}
	t.clients[nodeID] = c
	return c
}

// validAddr accepts host:port with a non-empty host and a numeric port.
func validAddr(a string) bool {
	host, port, err := net.SplitHostPort(a)
	if err != nil || host == "" {
		return false
	}
	p, err := strconv.Atoi(port)
	return err == nil && p > 0 && p < 65536
}

// call POSTs body to each of to's addresses in turn until one answers.
func (t *Transport) call(ctx context.Context, to store.Member, path string, body, out any) error {
	if t.err != nil {
		return t.err
	}
	if !core.ValidNode(to.Node) {
		return errors.New("peer: bad node id")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	c := t.client(to.Node)
	lastErr := errors.New("peer: no usable address")
	for _, addr := range to.Addrs {
		if !validAddr(addr) {
			continue
		}
		u := url.URL{Scheme: "https", Host: addr, Path: path}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			continue // unreachable or wrong key at this address: try the next
		}
		return decodeResp(resp, out)
	}
	return lastErr
}

func decodeResp(resp *http.Response, out any) error {
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBody+1))
	if err != nil {
		return err
	}
	if int64(len(b)) > maxRespBody {
		return errors.New("peer: response too large")
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return json.Unmarshal(b, out)
	case http.StatusForbidden:
		return node.ErrForbidden
	case http.StatusBadRequest:
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if len(e.Error) > 200 {
			e.Error = e.Error[:200]
		}
		return fmt.Errorf("peer refused: %q", e.Error)
	default: // 429, 5xx, anything else: transient as far as the outbox knows
		return fmt.Errorf("peer: HTTP %d", resp.StatusCode)
	}
}

// ---- server ----

// Serve accepts peer connections on ln until ctx ends. refresh is passed to
// Handler.Receive so a stale member list can be re-read from the room owner.
func Serve(ctx context.Context, ln net.Listener, id *Identity, h Handler, refresh func(room string)) error {
	cert, err := selfSigned(id)
	if err != nil {
		ln.Close()
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("POST "+pathDeliver, route(maxDeliverBody, func(ctx context.Context, from string, w deliverWire) (any, error) {
		req, err := w.decode()
		if err != nil {
			return nil, err
		}
		return map[string]any{"results": h.Receive(ctx, from, req, refresh)}, nil
	}))
	mux.Handle("POST "+pathJoin, route(maxSmallBody, func(ctx context.Context, from string, req node.JoinReq) (any, error) {
		return h.HandleJoin(ctx, from, req)
	}))
	mux.Handle("POST "+pathRoom, route(maxSmallBody, func(ctx context.Context, from string, req node.RoomReq) (any, error) {
		return h.HandleRoom(ctx, from, req)
	}))
	if j, ok := h.(JoinHandler); ok {
		mux.Handle("POST "+pathJoinV2, joinV2Route(j))
	}
	if p, ok := h.(PullHandler); ok {
		mux.Handle("POST "+pathPullV2, newPullServer(p))
	}
	protos := new(http.Protocols)
	protos.SetHTTP1(true) // v0.2 clients: HTTP/1.1, one request per connection at a time
	protos.SetHTTP2(true) // the pull exchange: waiting pulls to one peer share one connection (ALPN h2)
	srv := &http.Server{
		Handler:           guard(newLimiter(), mux),
		Protocols:         protos,
		ReadHeaderTimeout: 5 * time.Second, // also bounds the TLS handshake
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug),
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
			// Any self-signed client certificate is accepted by crypto/tls
			// (which still verifies the client's possession of the key);
			// certNode then insists it is a well-formed node certificate.
			// Whether that node may do anything is decided by internal/node.
			ClientAuth: tls.RequireAnyClientCert,
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				_, err := certNode(raw)
				return err
			},
		},
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if srv.Shutdown(sctx) != nil {
			srv.Close()
		}
	}()
	// Limit before TLS so stalled handshakes count against the cap too.
	if err := srv.ServeTLS(newLimitListener(ln, maxConns), "", ""); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// deliverWire is node.DeliverReq with envelopes left undecoded, so the batch
// limit is checked before any per-envelope allocation.
type deliverWire struct {
	Envelopes []json.RawMessage `json:"envelopes"`
	Versions  map[string]int64  `json:"versions"`
}

// decode enforces the batch, envelope and versions limits, then decodes each
// envelope. node.Receive does the semantic validation.
func (w deliverWire) decode() (node.DeliverReq, error) {
	if len(w.Envelopes) > core.MaxBatch {
		return node.DeliverReq{}, &node.UserError{Msg: "batch too large"}
	}
	if len(w.Versions) > maxVersions {
		return node.DeliverReq{}, &node.UserError{Msg: "too many versions"}
	}
	req := node.DeliverReq{Envelopes: make([]core.Envelope, len(w.Envelopes)), Versions: w.Versions}
	for i, raw := range w.Envelopes {
		if len(raw) > core.MaxEnvelope {
			return node.DeliverReq{}, &node.UserError{Msg: "envelope too large"}
		}
		if err := json.Unmarshal(raw, &req.Envelopes[i]); err != nil {
			return node.DeliverReq{}, &node.UserError{Msg: "bad envelope"}
		}
	}
	return req, nil
}

// limitListener caps concurrent accepted connections. Accept blocks while the
// cap is reached; a slot frees when its conn is closed (exactly once).
type limitListener struct {
	net.Listener
	sem  chan struct{}
	done chan struct{}
	once sync.Once
}

func newLimitListener(ln net.Listener, n int) net.Listener {
	return &limitListener{Listener: ln, sem: make(chan struct{}, n), done: make(chan struct{})}
}

func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.sem <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.sem }}, nil
}

func (l *limitListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// requestNode is the authenticated node id: the key in the TLS client
// certificate that certNode accepted. Nothing in the request can change it.
func requestNode(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	pub, ok := r.TLS.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		return ""
	}
	return hex.EncodeToString(pub)
}

// route decodes a size-limited JSON body of type T and maps call's result.
func route[T any](limit int64, call func(context.Context, string, T) (any, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req T
		r.Body = http.MaxBytesReader(w, r.Body, limit) // before decoding: never buffer more
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request too large"})
			} else {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad JSON"})
			}
			return
		}
		out, err := call(r.Context(), requestNode(r), req)
		var ue *node.UserError
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, out)
		case errors.Is(err, node.ErrForbidden):
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		case errors.As(err, &ue):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": ue.Msg})
		default:
			slog.Error("peer: handler failed", "route", r.URL.Path, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		}
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// statusWriter remembers the status code for the access log.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(code int) { s.code = code; s.ResponseWriter.WriteHeader(code) }

// guard rate-limits per authenticated node and logs every request (never bodies).
func guard(lim *limiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, from := time.Now(), requestNode(r)
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		switch {
		case from == "": // unreachable with RequireAnyClientCert; refuse rather than trust
			http.Error(sw, "no client identity", http.StatusForbidden)
		case !lim.allow(from):
			http.Error(sw, "slow down", http.StatusTooManyRequests)
		default:
			next.ServeHTTP(sw, r)
		}
		lvl := slog.LevelDebug
		if sw.code != http.StatusOK {
			lvl = slog.LevelInfo
		}
		path := r.URL.Path
		if len(path) > 64 {
			path = path[:64]
		}
		slog.Log(r.Context(), lvl, "peer request", "from", core.ShortNode(from), "route", path,
			"status", sw.code, "ms", time.Since(start).Milliseconds())
	})
}

// ---- rate limit ----

// Token bucket per node id. Variables so tests can tune them.
var (
	rateBurst   = 200.0
	ratePerSec  = 50.0
	rateMaxKeys = 1024
)

type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string]*bucket
}

func newLimiter() *limiter { return &limiter{now: time.Now, m: map[string]*bucket{}} }

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.m[key]
	if b == nil {
		if len(l.m) >= rateMaxKeys {
			l.evict(now)
		}
		if len(l.m) >= rateMaxKeys {
			return false // full of live keys: refuse the newest rather than drop one
		}
		b = &bucket{tokens: rateBurst, last: now}
		l.m[key] = b
	}
	b.tokens = min(rateBurst, b.tokens+now.Sub(b.last).Seconds()*ratePerSec)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// evict drops buckets that have refilled: they carry no state. Live
// buckets are never dropped, so rotating keys cannot reset another's limit.
func (l *limiter) evict(now time.Time) {
	for k, b := range l.m {
		if b.tokens+now.Sub(b.last).Seconds()*ratePerSec >= rateBurst {
			delete(l.m, k)
		}
	}
}
