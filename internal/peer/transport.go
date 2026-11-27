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
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

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

	// Inbound connections are counted in three pools (PROTOCOL 4.6), so
	// that keys that share no room here can never take a member's slot.
	maxConns         = 256 // connections of keys that are members of a room held here
	maxPerKey        = 16  // of those, connections of one key
	maxStrangerConns = 32  // connections of keys that share no room here, joiners among them
	maxPendingConns  = 64  // connections still in the TLS handshake
	maxKnownPending  = 16  // more of those, kept for source groups a member came from lately
	maxKnownPerGroup = 4   // of those kept slots, the most one source group holds at once
	maxPerSource     = 8   // handshaking and stranger connections from one IPv4 address or IPv6 /64
	maxPerSlash56    = 16  // the same from one IPv6 /56
	maxPerSlash48    = 32  // the same from one IPv6 /48
	maxKnownSources  = 1024
	knownSourceLife  = 24 * time.Hour
	handshakeTimeout = 5 * time.Second
	strangerLife     = 15 * time.Second // the longest a stranger's connection is kept
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
	s := "peer: the certificate presented is not the other machine's own"
	if e.Issuer != "" {
		s += ": it claims the issuer \"" + e.Issuer + "\" (a name anyone can write)"
	}
	return s + "; most likely a TLS-inspecting proxy re-signs the connection; a2p2 refuses it, because it could " +
		"not tell the other machine from whatever answers"
}

// maxIssuerRunes bounds the issuer name shown.
const maxIssuerRunes = 64

// issuerText is the name of c's issuer for display: its common name, else
// its first organisation; cut to maxIssuerRunes, then only letters, digits,
// a space and .-_,()&'/@:+ kept, every other character a "?", so no quote,
// line break or control can change what a person reads around it.
func issuerText(c *x509.Certificate) string {
	s := c.Issuer.CommonName
	if s == "" && len(c.Issuer.Organization) > 0 {
		s = c.Issuer.Organization[0]
	}
	r := []rune(s)
	more := len(r) > maxIssuerRunes
	if more {
		r = r[:maxIssuerRunes]
	}
	for i, x := range r {
		if !unicode.IsLetter(x) && !unicode.IsDigit(x) && !strings.ContainsRune(" .-_,()&'/@:+", x) {
			r[i] = '?'
		}
	}
	s = string(r)
	if more {
		s += "..."
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
	var member func(string) bool
	if m, ok := h.(MemberHandler); ok {
		member = m.SharesRoom
	}
	tlsCfg := serverTLS(cert)
	// The listener completes each TLS handshake itself, so that a
	// connection is placed in a pool only once the client has proved its
	// key; the server is handed connections ready to serve.
	lln := newLimitListener(ln, tlsCfg, member)
	srv := &http.Server{
		Handler:           guard(newLimiters(), mux, in),
		Protocols:         protos,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug),
		// Not used for handshakes (the listener does them); naming "h2"
		// here makes Serve answer HTTP/2 on connections that chose it.
		TLSConfig: tlsCfg,
	}
	// Requests end when ctx does, so that held (waiting) calls return.
	srv.BaseContext = func(net.Listener) context.Context { return ctx }
	// Each request can tell which pool its connection is in.
	srv.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		if tc, ok := c.(*tls.Conn); ok {
			if lc, ok := tc.NetConn().(*limitConn); ok {
				return context.WithValue(ctx, connKey{}, lc)
			}
		}
		return ctx
	}
	down, failed := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(down)
		select {
		case <-ctx.Done():
		case <-failed:
			return
		}
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if srv.Shutdown(sctx) != nil {
			srv.Close()
		}
	}()
	err = srv.Serve(lln)
	if !errors.Is(err, http.ErrServerClosed) {
		// The listener failed for good: the connections it accepted end
		// now, not when ctx does.
		close(failed)
		lln.Close()
		srv.Close()
		lln.wg.Wait()
		return err
	}
	<-down // Serve returns as Shutdown starts; Shutdown waits for the handlers
	lln.wg.Wait()
	return nil
}

// serverTLS is the listener's TLS configuration. Any self-signed client
// certificate is accepted by crypto/tls (which still verifies the
// client's possession of the key); certNode then insists it is a
// well-formed node certificate. Whether that node may do anything is
// decided by internal/node.
func serverTLS(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAnyClientCert,
		// HTTP/2 for pulls, HTTP/1.1 for joins.
		NextProtos: []string{"h2", "http/1.1"},
		// A resumed session presents no certificate of its own. No a2p2
		// client resumes one; a client that tries gets a full handshake.
		SessionTicketsDisabled: true,
		// Called before crypto/tls checks the client's signature, so it
		// only checks the form; the pool is chosen after the handshake.
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			_, err := certNode(raw)
			return err
		},
	}
}

// MemberHandler, if the handler has it, says whether a key is now a
// member of a room held here (node.Rooms.SharesRoom). Without it every
// key counts as a member.
type MemberHandler interface {
	SharesRoom(node string) bool
}

// srcCount is one of the groups a connection's source is counted in, with
// the most connections outside the members' pool that group may hold.
type srcCount struct {
	key string
	max int
}

// sourceCounts are the groups addr is counted in: an IPv4 address; or an
// IPv6 /64, /56 and /48, since one customer of an IPv6 network is often
// given a /56 or a /48 and can use every address in it.
func sourceCounts(addr string) []srcCount {
	g := node.SourceGroup(addr)
	out := []srcCount{{g, maxPerSource}}
	p, err := netip.ParsePrefix(g)
	if err != nil || !p.Addr().Is6() {
		return out
	}
	for _, w := range []struct{ bits, max int }{{56, maxPerSlash56}, {48, maxPerSlash48}} {
		if q, err := p.Addr().Prefix(w.bits); err == nil {
			out = append(out, srcCount{q.String(), w.max})
		}
	}
	return out
}

// limitListener counts accepted connections in three pools, and does
// their TLS handshakes. A connection starts pending: it holds one of
// maxPendingConns slots, and a slot of each of its source's groups
// (sourceCounts), while its handshake and the membership lookup after it
// run, for handshakeTimeout at most. When those are full, a source group
// (IPv4 address or IPv6 /64) that completed a member's handshake lately
// may use one of maxKnownPending slots kept for such groups, at most
// maxKnownPerGroup at once, whatever its /56 and /48 hold. A connection
// there that does not prove a member's key is closed, and its group is
// no longer known. A connection that finds no slot is closed at once, so
// the queue of the operating system never fills behind a busy listener.
// Once the handshake has proved the client's key, the connection moves to
// the members' pool (maxConns, at most maxPerKey of one key) or to the
// strangers' pool (maxStrangerConns, still counted against its source);
// with no room there it is closed. A stranger's connection lives
// strangerLife at most, serves one request at a time, and every response
// on it closes it (guard). Membership is checked again for every request
// (limitConn.classify). A slot frees when its connection is closed
// (exactly once).
type limitListener struct {
	raw    net.Listener
	cfg    *tls.Config
	member func(string) bool
	ready  chan net.Conn
	errc   chan error
	ctx    context.Context // ends when the listener is closed
	stop   context.CancelFunc
	wg     sync.WaitGroup // run and the handshakes it started

	mu                 sync.Mutex
	pending, known     int // handshakes in the shared slots, and in those kept for known member sources
	members, strangers int
	lookups            int            // membership lookups after a handshake still running
	perKey             map[string]int // members' connections per key
	per                map[string]int // pending (shared slots) and stranger connections per source group
	knownPer           map[string]int // handshakes in the kept slots per source group
	recent             *node.Recent[time.Time]
	said               atomic.Int64 // unix nanoseconds of the last Info line about a refusal
}

func newLimitListener(ln net.Listener, cfg *tls.Config, member func(string) bool) *limitListener {
	ctx, stop := context.WithCancel(context.Background())
	l := &limitListener{raw: ln, cfg: cfg, member: member, ready: make(chan net.Conn), errc: make(chan error, 1),
		ctx: ctx, stop: stop, perKey: map[string]int{}, per: map[string]int{}, knownPer: map[string]int{},
		recent: node.NewRecent[time.Time](maxKnownSources)}
	l.wg.Go(l.run)
	return l
}

// Accept returns the next connection whose handshake is done and that
// has a slot.
func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ready:
		return c, nil
	case err := <-l.errc:
		return nil, err
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	}
}

func (l *limitListener) Addr() net.Addr { return l.raw.Addr() }

func (l *limitListener) Close() error {
	l.stop()
	return l.raw.Close()
}

// run accepts connections from the operating system until the listener
// closes, and starts a handshake for each that finds a slot.
func (l *limitListener) run() {
	var delay time.Duration
	for {
		c, err := l.raw.Accept()
		if err != nil {
			if l.ctx.Err() != nil {
				return
			}
			// Out of file descriptors and the like: wait and go on, as
			// http.Server does.
			var te interface{ Temporary() bool }
			if errors.As(err, &te) && te.Temporary() {
				delay = min(max(2*delay, 5*time.Millisecond), time.Second)
				select {
				case <-time.After(delay):
					continue
				case <-l.ctx.Done():
					return
				}
			}
			l.errc <- err
			return
		}
		delay = 0
		lc, why := l.take(c)
		if lc == nil {
			c.Close()
			l.refused(why)
			continue
		}
		l.wg.Go(func() { l.handshake(lc) })
	}
}

// take gives c a pending slot, or says why there is none: a shared slot
// if each of its source groups is within its share, else a slot kept for
// known member sources, which counts in none of the shared groups.
func (l *limitListener) take(c net.Conn) (*limitConn, string) {
	srcs := sourceCounts(c.RemoteAddr().String())
	l.mu.Lock()
	defer l.mu.Unlock()
	fair := true
	for _, s := range srcs {
		if l.per[s.key] >= s.max {
			fair = false
		}
	}
	lc := &limitConn{Conn: c, l: l, srcs: srcs}
	g := srcs[0].key
	switch {
	case fair && l.pending < maxPendingConns:
		l.pending++
		for _, s := range srcs {
			l.per[s.key]++
		}
	case l.known < maxKnownPending && l.knownPer[g] < maxKnownPerGroup && l.knownSource(g):
		l.known++
		l.knownPer[g]++
		lc.knownSlot = true
	case !fair:
		return nil, "a source over its share of inbound connections"
	default:
		return nil, "the handshake slots are full"
	}
	return lc, ""
}

// unknown gives back c's kept slot; l.mu is held. Unless c proved a
// member's key, its group is no longer known: only a member's handshake
// through the shared slots makes it known again.
func (l *limitListener) unknown(c *limitConn, proved bool) {
	g := c.srcs[0].key
	l.known--
	if l.knownPer[g]--; l.knownPer[g] <= 0 {
		delete(l.knownPer, g)
	}
	if !proved {
		l.recent.Delete(g)
	}
}

// knownSource says whether a member's handshake from group completed
// within knownSourceLife; l.mu is held.
func (l *limitListener) knownSource(group string) bool {
	t := l.recent.Get(group)
	return t != nil && time.Since(*t) < knownSourceLife
}

// handshake completes c's TLS handshake and the membership lookup of its
// key, both within handshakeTimeout, and places it in its pool; Accept
// then returns it.
func (l *limitListener) handshake(c *limitConn) {
	tc := tls.Server(c, l.cfg)
	deadline := time.Now().Add(handshakeTimeout)
	_ = c.SetDeadline(deadline)
	ctx, cancel := context.WithDeadline(l.ctx, deadline)
	err := tc.HandshakeContext(ctx)
	cancel()
	if err != nil {
		tc.Close()
		return
	}
	key := connNode(tc.ConnectionState())
	member, ok := l.isMember(key, deadline)
	if !ok {
		l.refused("the membership lookup did not finish within the handshake deadline")
		tc.Close()
		return
	}
	if err := c.enter(key, member); err != nil {
		switch {
		case errors.Is(err, errNotMember):
			l.refused("a key that shares no room took a slot kept for members' sources; that source is no longer known")
		case !errors.Is(err, errPoolFull):
		case member:
			l.refused("the members' connection slots are full, or this key's share of them")
		default:
			l.refused("the connection slots of keys that share no room are full")
		}
		tc.Close()
		return
	}
	_ = c.SetDeadline(time.Time{})
	select {
	case l.ready <- tc:
	case <-l.ctx.Done():
		tc.Close()
	}
}

// isMember looks key up in the rooms, outside the locks since it may wait
// for them, and reports false for ok when the answer has not come by
// deadline. The lookup then goes on alone. Lookups running at once are
// bounded by twice the number of handshake slots, so stuck ones cannot
// pile up, and the handshakes after one deadline still get theirs.
func (l *limitListener) isMember(key string, deadline time.Time) (member, ok bool) {
	if l.member == nil {
		return true, true
	}
	l.mu.Lock()
	if l.lookups >= 2*(maxPendingConns+maxKnownPending) {
		l.mu.Unlock()
		return false, false
	}
	l.lookups++
	l.mu.Unlock()
	done := make(chan bool, 1)
	go func() {
		m := l.member(key)
		l.mu.Lock()
		l.lookups--
		l.mu.Unlock()
		done <- m
	}()
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case m := <-done:
		return m, true
	case <-t.C:
	case <-l.ctx.Done():
	}
	return false, false
}

// connNode is the key a finished handshake proved.
func connNode(cs tls.ConnectionState) string {
	if len(cs.PeerCertificates) == 0 {
		return ""
	}
	pub, ok := cs.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		return ""
	}
	return hex.EncodeToString(pub)
}

// unsource gives back c's slot of each of its source's groups; l.mu is
// held.
func (l *limitListener) unsource(c *limitConn) {
	for _, s := range c.srcs {
		if l.per[s.key]--; l.per[s.key] <= 0 {
			delete(l.per, s.key)
		}
	}
}

// addMember counts c, of key, in the members' pool if there is room; l.mu
// is held.
func (l *limitListener) addMember(c *limitConn, key string) bool {
	if l.members >= maxConns || l.perKey[key] >= maxPerKey {
		return false
	}
	l.members++
	l.perKey[key]++
	*l.recent.Use(c.srcs[0].key) = time.Now()
	return true
}

func (l *limitListener) dropMember(key string) {
	l.members--
	if l.perKey[key]--; l.perKey[key] <= 0 {
		delete(l.perKey, key)
	}
}

// refused logs a refused connection: at Debug, and at Info at most once
// a minute, so that a flood of them cannot fill the log.
func (l *limitListener) refused(why string) {
	now, last := time.Now().UnixNano(), l.said.Load()
	lvl := slog.LevelDebug
	if now-last >= int64(time.Minute) && l.said.CompareAndSwap(last, now) {
		lvl = slog.LevelInfo
	}
	slog.Log(context.Background(), lvl, "peer connection refused", "why", why)
}

type connPool int

const (
	poolPending connPool = iota
	poolMember
	poolStranger
	poolClosed
)

var (
	errPoolFull  = errors.New("peer: no free connection slot for this key")
	errNotMember = errors.New("peer: a slot kept for members' sources taken by a key that is not a member")
)

type limitConn struct {
	net.Conn
	l         *limitListener
	srcs      []srcCount
	knownSlot bool // its pending slot is one of those kept for known member sources

	mu       sync.Mutex
	key      string // set once, by enter
	member   bool   // set with key: the rooms named key a member's
	pool     connPool
	revoked  bool        // a member's connection whose key is no longer a member: served as a stranger's
	timer    *time.Timer // a stranger's (or revoked member's) end of life; set and stopped under mu
	inflight int         // requests on it being served
}

// enter moves c out of the handshake into the pool of key, which the
// finished handshake proved. A full pool refuses it, and so does a slot
// kept for known member sources when key is not a member's (Close then
// gives the slot back).
func (c *limitConn) enter(key string, member bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pool != poolPending {
		return net.ErrClosed
	}
	c.key, c.member = key, member
	l := c.l
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case member && l.addMember(c, key):
		if c.knownSlot {
			l.unknown(c, true)
		} else {
			l.pending--
			l.unsource(c)
		}
		c.pool = poolMember
		return nil
	case !member && c.knownSlot:
		return errNotMember
	case !member && l.strangers < maxStrangerConns:
		l.strangers++
		l.pending--
		c.pool = poolStranger
		c.timer = time.AfterFunc(strangerLife, func() { _ = c.Close() })
		return nil
	}
	return errPoolFull
}

// classify checks c's key against the rooms again, for a request on it,
// and says whether the request is a stranger's. A stranger's connection
// whose key has become a member moves to the members' pool if there is
// room; a member's connection whose key is no longer one is served as a
// stranger's from now on and ends within strangerLife.
func (c *limitConn) classify() bool {
	c.mu.Lock()
	key, pool := c.key, c.pool
	c.mu.Unlock()
	if pool != poolMember && pool != poolStranger {
		return true
	}
	member := c.l.member == nil || c.l.member(key) // outside the locks
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.pool == poolMember && !member && !c.revoked:
		c.revoked = true
		c.timer = time.AfterFunc(strangerLife, func() { _ = c.Close() })
	case c.pool == poolMember && member && c.revoked:
		// Removed and added back: a member's connection again, not cut.
		c.revoked = false
		c.timer.Stop()
		c.timer = nil
	case c.pool == poolStranger && member:
		l := c.l
		l.mu.Lock()
		if l.addMember(c, key) {
			l.strangers--
			l.unsource(c)
			c.pool = poolMember
			c.timer.Stop()
			c.timer = nil
		}
		l.mu.Unlock()
	}
	return c.pool != poolMember || c.revoked
}

// stranger says whether requests on c are served as a stranger's.
func (c *limitConn) stranger() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pool != poolMember || c.revoked
}

// begin counts a request on c. A stranger's connection serves one request
// at a time: on HTTP/2 it could otherwise start many before the first
// answer closes it. It reports false, counting nothing, for a request
// beyond that; end undoes a true.
func (c *limitConn) begin(stranger bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if stranger && c.inflight >= 1 {
		return false
	}
	c.inflight++
	return true
}

func (c *limitConn) end() {
	c.mu.Lock()
	c.inflight--
	c.mu.Unlock()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pool == poolClosed {
		return err
	}
	if c.timer != nil {
		c.timer.Stop()
	}
	l := c.l
	l.mu.Lock()
	switch c.pool {
	case poolPending:
		if c.knownSlot {
			l.unknown(c, c.member)
		} else {
			l.pending--
			l.unsource(c)
		}
	case poolMember:
		l.dropMember(c.key)
	case poolStranger:
		l.strangers--
		l.unsource(c)
	}
	l.mu.Unlock()
	c.pool = poolClosed
	return err
}

type connKey struct{}

// strangerRequest says whether r came on a connection whose key shares no
// room here.
func strangerRequest(r *http.Request) bool {
	lc, ok := r.Context().Value(connKey{}).(*limitConn)
	return ok && lc.stranger()
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

// InboundHandler, if the handler has it, is told of every request whose
// TLS client key is known: the key and the local address it arrived on.
type InboundHandler interface {
	Inbound(from, local string)
}

// guard rate-limits per authenticated node and logs every request (never
// bodies). A key that shares no room here has its own buckets, so strangers can
// never fill the members' table; a stranger's connection serves one
// request at a time, and every response to one closes it. Refusals are logged at Info only for members, a few a
// minute each; the rest at Debug, so that a flood cannot fill the log.
func guard(lims limiters, next http.Handler, in func(from, local string)) http.Handler {
	said := newPullLimiter(logRatePerSec, logBurst)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, from := time.Now(), requestNode(r)
		strange, busy := false, false
		if lc, ok := r.Context().Value(connKey{}).(*limitConn); ok {
			strange = lc.classify() // the rooms now, not at the handshake
			if busy = !lc.begin(strange); !busy {
				defer lc.end()
			}
		}
		if strange {
			w.Header().Set("Connection", "close") // HTTP/2: a GOAWAY, then the connection ends when idle
		}
		lim := lims.of(strange)
		if in != nil && from != "" && !strange { // a stranger's keys never push a member out of the record (F25)
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
		case busy: // a second request at once on a stranger's connection, refused before its body is read
			if r.URL.Path == pathPullV2 {
				pullRefuse(sw, &node.PullError{Status: http.StatusTooManyRequests, Code: node.CodeRateLimited,
					Detail: "concurrent", RetryAfter: time.Second})
			} else {
				w.Header().Set("Retry-After", "1")
				http.Error(sw, "one request at a time", http.StatusTooManyRequests)
			}
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
		if sw.code != http.StatusOK && !strange {
			if ok, _ := said.take(from); ok {
				lvl = slog.LevelInfo
			}
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

// limiters keeps members' buckets apart from those of keys that share no
// room here.
type limiters struct{ member, stranger *limiter }

func newLimiters() limiters { return limiters{member: newLimiter(), stranger: newLimiter()} }

func (l limiters) of(stranger bool) *limiter {
	if stranger {
		return l.stranger
	}
	return l.member
}

// The log budget per member key for refused requests at Info.
var (
	logRatePerSec = 0.1
	logBurst      = 5.0
)

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
