package node

// The dial-back (PROTOCOL 4.7): a member asks another member to connect
// back to addresses it says are its own, and hears which of them completed
// a pinned handshake with its own key. Only the IP address the request
// came from is ever dialled, on at most two of the ports asked. It is how a machine learns whether
// others can reach it, which it cannot see from inside. The rules of who
// may ask, for which addresses and how often live here; the dialling
// itself (TCP and TLS) is internal/peer's, behind Handshaker.

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// Limits of the dial-back (PROTOCOL 4.7). Variables so tests can tune them.
var (
	// MaxDialBackBody bounds a request body, before it is read.
	MaxDialBackBody int64 = 4 << 10
	// DialBackTimeout bounds the responder's dials and handshakes, all
	// addresses of one request together.
	DialBackTimeout = 3 * time.Second
	// Per asking peer: a burst of 4, then one every 10 minutes.
	dialBackPeerBurst = 4.0
	dialBackPeerEvery = 10 * time.Minute
	// For the whole responder: a burst of 16, then one every 10 seconds,
	// and at most 4 requests being dialled at once.
	dialBackAllBurst   = 16.0
	dialBackAllEvery   = 10 * time.Second
	dialBackConcurrent = 4
)

// DialBackRequest is the body of POST /p/v2/dialback.
type DialBackRequest struct {
	V     int      `json:"v"`
	Addrs []string `json:"addrs"`
}

// DialBackResponse is its answer: the asked addresses at which the
// asker's own key completed a handshake, in the order asked; those the
// responder did not dial (not at the IP address the request came from, or
// past the second port); and that IP address (shown, never advertised:
// PROTOCOL 4.7).
type DialBackResponse struct {
	V        int      `json:"v"`
	Reached  []string `json:"reached,omitempty"`
	NotTried []string `json:"not_tried,omitempty"`
	Saw      string   `json:"saw,omitempty"`
}

// NotTriedText is why a member did not try an address.
const NotTriedText = "a member dials only the address the request came from, on at most two ports"

// dialBackPorts is how many ports of the source address one request may
// have dialled: the listening port and a mapped external port.
const dialBackPorts = 2

var (
	dialBackRequestFields  = []string{"v", "addrs"}
	dialBackResponseFields = []string{"v", "reached", "not_tried", "saw"}
	dialBackRequestShape   = shape{"v": nil, "addrs": nil}
	dialBackResponseShape  = shape{"v": nil, "reached": nil, "not_tried": nil, "saw": nil}
)

// DecodeDialBackRequest checks a body already within MaxDialBackBody:
// strict JSON, version 2, 1 to MaxAddrs addresses, each a literal IP
// address and port (no names: a peer never makes this machine look one
// up), none repeated. Its refusal is a 400.
func DecodeDialBackRequest(body []byte) (*DialBackRequest, *PullError) {
	var q DialBackRequest
	if !strictPull(body, dialBackRequestFields, dialBackRequestShape) || json.Unmarshal(body, &q) != nil {
		return nil, badRequest("not strict json")
	}
	if q.V != PullVersion {
		return nil, badRequest("v")
	}
	if len(q.Addrs) == 0 || len(q.Addrs) > MaxAddrs {
		return nil, badRequest("addrs")
	}
	seen := map[netip.AddrPort]bool{}
	for _, a := range q.Addrs {
		ap, ok := literalAddr(a)
		if !ok || seen[ap] { // repeats are found after normalising (::ffff:1.2.3.4 is 1.2.3.4)
			return nil, badRequest("addrs")
		}
		seen[ap] = true
	}
	return &q, nil
}

// DecodeDialBackResponse decodes a 200 body and checks it against the
// request it answers: every reached and not-tried address is one that was
// asked, none is both, and saw is empty or an IP address literal without
// a zone, so nothing else is ever stored or printed.
func DecodeDialBackResponse(body []byte, asked []string) (*DialBackResponse, error) {
	var r DialBackResponse
	if !strictPull(body, dialBackResponseFields, dialBackResponseShape) || json.Unmarshal(body, &r) != nil ||
		r.V != PullVersion || len(r.Reached)+len(r.NotTried) > len(asked) {
		return nil, errNotStrictResponse
	}
	for _, a := range append(slices.Clone(r.Reached), r.NotTried...) {
		if !slices.Contains(asked, a) {
			return nil, errNotStrictResponse
		}
	}
	for _, a := range r.NotTried {
		if slices.Contains(r.Reached, a) {
			return nil, errNotStrictResponse
		}
	}
	if r.Saw != "" {
		ip, err := netip.ParseAddr(r.Saw)
		if err != nil || ip.Zone() != "" || len(r.Saw) > MaxAddrBytes {
			return nil, errNotStrictResponse
		}
		r.Saw = ip.Unmap().String()
	}
	return &r, nil
}

// literalAddr parses "ip:port" with a literal IP address (IPv6 in
// brackets, no zone) and a port from 1 to 65535.
func literalAddr(a string) (netip.AddrPort, bool) {
	if a == "" || len(a) > MaxAddrBytes {
		return netip.AddrPort{}, false
	}
	host, port, err := net.SplitHostPort(a)
	if err != nil {
		return netip.AddrPort{}, false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" {
		return netip.AddrPort{}, false
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 || strconv.Itoa(p) != port {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(ip.Unmap(), uint16(p)), true
}

// Handshaker completes the pinned handshake of the dial-back: a TCP
// connection to addr, then TLS expecting exactly key, presenting this
// machine's own certificate, and then closing without sending anything
// else. It reports whether key answered (internal/peer).
type Handshaker interface {
	Handshake(ctx context.Context, addr, key string) bool
}

// DialBackServer answers dial-back requests for one machine.
type DialBackServer struct {
	r   *Rooms
	hs  Handshaker
	now func() time.Time
	// LoopbackForTests lets the asker's loopback source address be dialled,
	// for tests of two machines on one computer over real TLS; the daemon
	// never sets it, and no setting does (D71).
	LoopbackForTests bool

	mu   sync.Mutex
	per  map[string]*tokens
	all  tokens
	busy int
}

// tokens is a token bucket: up to burst, one more every every.
type tokens struct {
	n    float64
	last time.Time
}

func (t *tokens) take(now time.Time, burst float64, every time.Duration) (bool, time.Duration) {
	if t.last.IsZero() {
		t.n, t.last = burst, now
	}
	t.n = min(burst, t.n+float64(now.Sub(t.last))/float64(every))
	t.last = now
	if t.n < 1 {
		return false, time.Duration((1 - t.n) * float64(every))
	}
	t.n--
	return true, 0
}

// wait is how long until a token is there, without taking it (0: now).
func (t *tokens) wait(now time.Time, burst float64, every time.Duration) time.Duration {
	n := burst
	if !t.last.IsZero() {
		n = min(burst, t.n+float64(now.Sub(t.last))/float64(every))
	}
	if n >= 1 {
		return 0
	}
	return time.Duration((1 - n) * float64(every))
}

// full says whether the bucket is full again at now.
func (t *tokens) full(now time.Time, burst float64, every time.Duration) bool {
	return t.last.IsZero() || t.n+float64(now.Sub(t.last))/float64(every) >= burst
}

// maxDialBackPeers bounds the per-peer table; only members ever enter it.
const maxDialBackPeers = 1024

// NewDialBackServer answers for the machine whose rooms are r, dialling
// through hs.
func NewDialBackServer(r *Rooms, hs Handshaker) *DialBackServer {
	return &DialBackServer{r: r, hs: hs, now: r.m.now, per: map[string]*tokens{}}
}

// SharesRoom reports whether node is, now, a member of a room this
// machine holds (never this machine itself).
func (r *Rooms) SharesRoom(node string) bool {
	if node == r.m.ID {
		return false
	}
	// The membership as last published (State), under its leaf lock: this
	// runs for every handshake and request, so it does not wait for the
	// rooms lock (F18).
	r.smu.Lock()
	defer r.smu.Unlock()
	for _, st := range r.states {
		if slices.Contains(st.Members, node) {
			return true
		}
	}
	return false
}

// OtherMembers lists, once each, the other machines that are members
// now of a room held here.
func (r *Rooms) OtherMembers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, rl := range r.rooms {
		for _, n := range rl.st.Members {
			if n != r.m.ID && !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	slices.Sort(out)
	return out
}

// DialBack is the responder's side (PROTOCOL 4.7). from is the asker's
// key as TLS proved it, source the address its connection came from.
// The order of checks: membership, then the per-peer and global rate
// limits and concurrency, then the addresses; only then is anything
// dialled, every address only with from as the key expected.
func (s *DialBackServer) DialBack(ctx context.Context, from, source string, q *DialBackRequest) (*DialBackResponse, *PullError) {
	if !core.ValidNode(from) || !s.r.SharesRoom(from) {
		return nil, pullErr(403, CodeNotMember, "")
	}
	if perr := s.enter(from); perr != nil {
		return nil, perr
	}
	defer s.leave()
	src, _ := netip.ParseAddrPort(source)
	srcIP := src.Addr().Unmap()
	ctx, cancel := context.WithTimeout(ctx, DialBackTimeout)
	defer cancel()
	ok := make([]bool, len(q.Addrs))
	tried := make([]bool, len(q.Addrs))
	var wg sync.WaitGroup
	ports := 0
	for i, a := range q.Addrs {
		// Only the IP address the request came from, on at most two ports,
		// and only as the address rule allows another machine's word
		// (AddrRefusal: never loopback, link-local or a port below 1024
		// other than 443, whatever the source; behind a tunnel or proxy on
		// this machine every source is loopback): a member can make this
		// machine connect to itself and nothing else.
		ap, valid := literalAddr(a)
		if !valid || ap.Addr() != srcIP || ports == dialBackPorts ||
			!RemoteAddrOK(ap.String()) && !(s.LoopbackForTests && srcIP.IsLoopback() && remotePortOK(int(ap.Port()))) {
			continue
		}
		ports++
		tried[i] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok[i] = s.hs.Handshake(ctx, ap.String(), from)
		}()
	}
	wg.Wait()
	resp := &DialBackResponse{V: PullVersion}
	n := 0
	for i, a := range q.Addrs {
		switch {
		case ok[i]:
			resp.Reached = append(resp.Reached, a)
		case !tried[i]:
			resp.NotTried = append(resp.NotTried, a)
		}
		if tried[i] {
			n++
		}
	}
	// Unless every address tried was reached, answer at the deadline: how
	// long a refused or a silent address took is not told either.
	if len(resp.Reached) < n {
		<-ctx.Done()
	}
	if src.IsValid() {
		resp.Saw = srcIP.String() // the address only: the source port of a connection out says nothing of where one listens
	}
	return resp, nil
}

// enter spends a token of from and of the whole responder and takes a
// concurrency slot, or says how long to wait.
func (s *DialBackServer) enter(from string) *PullError {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	limited := func(wait time.Duration) *PullError {
		e := pullErr(429, CodeRateLimited, "dialback")
		e.RetryAfter = max(wait, time.Second)
		return e
	}
	if s.busy >= dialBackConcurrent {
		return limited(time.Second)
	}
	t := s.per[from]
	if t == nil {
		if len(s.per) >= maxDialBackPeers {
			for k, o := range s.per {
				if o.n+float64(now.Sub(o.last))/float64(dialBackPeerEvery) >= dialBackPeerBurst {
					delete(s.per, k)
				}
			}
		}
		if len(s.per) >= maxDialBackPeers {
			return limited(dialBackPeerEvery)
		}
		t = &tokens{}
		s.per[from] = t
	}
	// Check the peer's own bucket first, so that one peer over its limit
	// spends nothing of everyone's.
	probe := *t
	if ok, wait := probe.take(now, dialBackPeerBurst, dialBackPeerEvery); !ok {
		*t = probe
		return limited(wait)
	}
	if ok, wait := s.all.take(now, dialBackAllBurst, dialBackAllEvery); !ok {
		return limited(wait)
	}
	*t = probe
	s.busy++
	return nil
}

func (s *DialBackServer) leave() {
	s.mu.Lock()
	s.busy--
	s.mu.Unlock()
}
