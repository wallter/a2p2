package node

// The dial-back (PROTOCOL 4.7): a member asks another member to connect
// back to addresses it says are its own, and hears which of them completed
// a pinned handshake with its own key. It is how a machine learns whether
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
// asker's own key completed a handshake, in the order asked; and the
// IP address the responder saw the request come from (a hint, never an
// identity: PROTOCOL 4.7).
type DialBackResponse struct {
	V       int      `json:"v"`
	Reached []string `json:"reached,omitempty"`
	Saw     string   `json:"saw,omitempty"`
}

var (
	dialBackRequestFields  = []string{"v", "addrs"}
	dialBackResponseFields = []string{"v", "reached", "saw"}
	dialBackRequestShape   = shape{"v": nil, "addrs": nil}
	dialBackResponseShape  = shape{"v": nil, "reached": nil, "saw": nil}
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
	for i, a := range q.Addrs {
		if _, ok := literalAddr(a); !ok || slices.Contains(q.Addrs[:i], a) {
			return nil, badRequest("addrs")
		}
	}
	return &q, nil
}

// DecodeDialBackResponse decodes a 200 body and checks it against the
// request it answers: every reached address is one that was asked.
func DecodeDialBackResponse(body []byte, asked []string) (*DialBackResponse, error) {
	var r DialBackResponse
	if !strictPull(body, dialBackResponseFields, dialBackResponseShape) || json.Unmarshal(body, &r) != nil ||
		r.V != PullVersion || len(r.Reached) > len(asked) || len(r.Saw) > MaxAddrBytes {
		return nil, errNotStrictResponse
	}
	for _, a := range r.Reached {
		if !slices.Contains(asked, a) {
			return nil, errNotStrictResponse
		}
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

// dialable says whether the responder may dial ip for an asker that
// connected from source: never an unspecified, multicast, broadcast or
// link-local address; a loopback address only when the asker itself
// connected over loopback (it is on this machine, so the dial reaches
// nothing the asker could not reach itself).
func dialable(ip netip.Addr, source netip.Addr) bool {
	switch {
	case ip.IsUnspecified(), ip.IsMulticast(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(), ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return false
	case ip.IsLoopback():
		return source.IsLoopback()
	}
	return true
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
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rl := range r.rooms {
		if slices.Contains(rl.st.Members, node) {
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
	var wg sync.WaitGroup
	for i, a := range q.Addrs {
		ap, valid := literalAddr(a)
		if !valid || !dialable(ap.Addr(), srcIP) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok[i] = s.hs.Handshake(ctx, ap.String(), from)
		}()
	}
	wg.Wait()
	resp := &DialBackResponse{V: PullVersion}
	for i, a := range q.Addrs {
		if ok[i] {
			resp.Reached = append(resp.Reached, a)
		}
	}
	// Unless every address asked was reached, answer at the deadline: how
	// long a refused or a silent third party took is not told either.
	if len(resp.Reached) < len(q.Addrs) {
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
