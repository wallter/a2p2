package peer

import (
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
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/node"
)

// Handler is what the listener needs from the node: the join exchange
// (node.JoinServer) and the pull exchange (node.Exchange). The requester's
// node id passed to each is always the one the TLS handshake proved.
type Handler interface {
	JoinHandler
	PullHandler
}

// Wire limits and timeouts. Variables only so tests can shorten them.
var (
	dialTimeout    = 3 * time.Second
	requestTimeout = 15 * time.Second
	maxClients     = 256 // cached per-destination HTTP clients
	maxConns       = 256 // concurrent inbound TCP connections, counted before TLS
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

// Intercepted is the certificate check's refusal when the far end is not
// a machine's own one-key certificate but something that re-signed it, as a
// TLS-inspecting proxy does: more than one certificate, or a leaf issued by
// a CA or not signed by its own key, with a machine's key as the subject
// (a server that is not an a2p2 machine at all keeps the plain refusal).
type Intercepted struct {
	// Issuer is the leaf's issuer name, from an untrusted certificate:
	// bounded and escaped (node.EscapeDisplay), so it is safe to show.
	Issuer string
}

func (e *Intercepted) Error() string {
	s := "peer: a TLS-inspecting proxy answers between this machine and the other machine"
	if e.Issuer != "" {
		s += " (certificate issued by \"" + e.Issuer + "\")"
	}
	return s + "; a2p2 refuses intercepted connections, because it could not tell the other machine from the proxy"
}

// maxIssuerRunes bounds the issuer name shown.
const maxIssuerRunes = 64

// issuerText is the name of c's issuer for display: its common name, else
// its first organisation; cut to maxIssuerRunes, controls and bidirectional
// marks escaped.
func issuerText(c *x509.Certificate) string {
	s := c.Issuer.CommonName
	if s == "" && len(c.Issuer.Organization) > 0 {
		s = c.Issuer.Organization[0]
	}
	if r := []rune(s); len(r) > maxIssuerRunes {
		s = string(r[:maxIssuerRunes]) + "..."
	}
	return node.EscapeDisplay(s)
}

// certNode checks that the peer presented exactly one self-signed Ed25519
// certificate whose CN is its own public key, and returns that node id.
// crypto/tls has already checked, separately, that the peer holds the private
// key (the CertificateVerify signature), so a valid result here proves the
// peer is that node. What is accepted is ownCert's rule alone; a refusal is
// then named *Intercepted when it looks re-signed, which changes only what
// the person is told.
func certNode(rawCerts [][]byte) (string, error) {
	id, err := ownCert(rawCerts)
	if err != nil {
		if icpt := resigned(rawCerts); icpt != nil {
			return "", icpt
		}
		return "", err
	}
	return id, nil
}

// ownCert is the acceptance rule.
func ownCert(rawCerts [][]byte) (string, error) {
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

// resigned says whether refused certificates look like a machine's
// certificate re-signed on the way, as a TLS-inspecting proxy does: the
// leaf still names a machine's key as its CN, but it comes with a chain,
// holds another kind of key, or is not signed by its own key. A single
// certificate signed by its own key is not that, whatever its CN; nor is a
// server whose leaf names no machine key.
func resigned(rawCerts [][]byte) *Intercepted {
	if len(rawCerts) == 0 {
		return nil
	}
	c, err := x509.ParseCertificate(rawCerts[0])
	if err != nil || !core.ValidNode(c.Subject.CommonName) {
		return nil
	}
	if _, ok := c.PublicKey.(ed25519.PublicKey); ok && len(rawCerts) == 1 &&
		c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature) == nil {
		return nil
	}
	return &Intercepted{Issuer: issuerText(c)}
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

// ---- server ----

// Serve accepts peer connections on ln until ctx ends: /p/v2/join and
// /p/v2/pull, both answered by h.
func Serve(ctx context.Context, ln net.Listener, id *Identity, h Handler) error {
	cert, err := selfSigned(id)
	if err != nil {
		ln.Close()
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("POST "+pathJoinV2, joinV2Route(h))
	mux.Handle("POST "+pathPullV2, newPullServer(ctx, h))
	if d, ok := h.(DialBackHandler); ok { // PROTOCOL 4.7; without it, 404 as on an older machine
		mux.Handle("POST "+pathDialBackV2, dialBackRoute(d))
	}
	if fh, ok := h.(FileHandler); ok {
		mux.Handle(pathFileV2, fileRoute(fh)) // GET and PUT, with their own limits (PROTOCOL 10.3)
	}
	protos := new(http.Protocols)
	protos.SetHTTP1(true) // the join exchange: HTTP/1.1, one request per connection
	protos.SetHTTP2(true) // the pull exchange: waiting pulls to one peer share one connection (ALPN h2)
	var in func(from, local string)
	if i, ok := h.(InboundHandler); ok {
		in = i.Inbound
	}
	srv := &http.Server{
		Handler:           guard(newLimiter(), mux, in),
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
	// Requests end when ctx does, so that held (waiting) calls return.
	srv.BaseContext = func(net.Listener) context.Context { return ctx }
	down := make(chan struct{})
	go func() {
		defer close(down)
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
	<-down // ServeTLS returns as Shutdown starts; Shutdown waits for the handlers
	return nil
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
// InboundHandler, if the handler has it, is told of every request whose
// TLS client key is known: the key and the local address it arrived on.
type InboundHandler interface {
	Inbound(from, local string)
}

func guard(lim *limiter, next http.Handler, in func(from, local string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, from := time.Now(), requestNode(r)
		if in != nil && from != "" {
			local := ""
			if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
				local = a.String()
			}
			in(from, local)
		}
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		switch {
		case from == "" && r.URL.Path == pathPullV2:
			pullRefuse(sw, &node.PullError{Status: http.StatusForbidden, Code: node.CodeNotMember})
		case from == "": // unreachable with RequireAnyClientCert; refuse rather than trust
			http.Error(sw, "no client identity", http.StatusForbidden)
		case r.URL.Path != pathJoinV2 && !isFileRoute(r.URL.Path) && !lim.allow(from): // join: by source address; files: their own bucket
			if r.URL.Path == pathPullV2 { // the pull exchange's refusals are its own (PROTOCOL 4.5)
				pullRefuse(sw, &node.PullError{Status: http.StatusTooManyRequests, Code: node.CodeRateLimited,
					RetryAfter: time.Second})
			} else {
				http.Error(sw, "slow down", http.StatusTooManyRequests)
			}
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
