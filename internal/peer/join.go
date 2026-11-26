package peer

// POST /p/v2/join (PROTOCOL 5.2): the join exchange on the peer
// listener, beside /p/v2/pull. It is reached by a key the server has not
// pinned: the listener accepts any well-formed
// node certificate, and the node decides. The body is strict JSON, checked
// by internal/node; here only its size is bounded, before it is read, and
// the requester's identity (from TLS) and source address are passed on.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/wallter/a2p2/internal/node"
)

const pathJoinV2 = "/p/v2/join"

// Limits of the join exchange. Variables so tests can shorten them.
var (
	maxJoinV2Body int64 = node.MaxJoinRequestSize
	maxJoinV2Resp int64 = 8 << 20 // 256 log entries near their size limit, with framing
)

// JoinHandler answers the join exchange (node.JoinServer) on /p/v2/join.
type JoinHandler interface {
	Handle(ctx context.Context, from, source string, body []byte) node.JoinReply
}

// sourceLimit counts join requests per source group (node.SourceGroup:
// an IPv4 address or an IPv6 /64) over the last minute, in two budgets
// (PROTOCOL 5.3): opens and accepts, which make the admin machine do
// work, and polls, which a joiner sends while it waits. While its table
// is full, a new group pushes out the one heard from least recently, in
// constant time, so that many addresses cannot refuse every other source.
type sourceLimit struct {
	mu  sync.Mutex
	now func() time.Time
	m   *node.Recent[sourceUse]
}

// sourceUse is one group's requests in the last minute.
type sourceUse struct {
	admit, poll []time.Time
}

// Limits per source group, a minute. Variables so tests can change them.
var (
	maxJoinSources = 4096
	joinAdmits     = node.OpensPerMinute
	joinPolls      = 30
)

func newSourceLimit() *sourceLimit {
	return &sourceLimit{now: time.Now, m: node.NewRecent[sourceUse](maxJoinSources)}
}

func (l *sourceLimit) allow(src string, poll bool) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	u := l.m.Use(node.SourceGroup(src))
	ts, most := &u.admit, joinAdmits
	if poll {
		ts, most = &u.poll, joinPolls
	}
	*ts = slices.DeleteFunc(*ts, func(t time.Time) bool { return !now.Before(t.Add(time.Minute)) })
	if len(*ts) >= most {
		return false, (*ts)[0].Add(time.Minute).Sub(now)
	}
	*ts = append(*ts, now)
	return true, 0
}

// joinOp reads only the op of a join request, to pick its budget; the
// node checks the whole request strictly. Anything but a poll counts as
// an open.
func joinOp(body []byte) string {
	var q struct {
		Op string `json:"op"`
	}
	_ = json.Unmarshal(body, &q)
	return q.Op
}

// joinV2Route serves /p/v2/join: each source group is limited once the
// bounded body is read, and the per-key limit of the other routes does
// not apply here (guard), so keys that never presented a valid secret
// take no room in it; the join server limits keys that did.
func joinV2Route(j JoinHandler) http.Handler {
	lim := newSourceLimit()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		source, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			source = r.RemoteAddr
		}
		if r.ContentLength > maxJoinV2Body {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": node.CodeTooLarge})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJoinV2Body))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": node.CodeTooLarge})
			} else {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": node.CodeBadRequest})
			}
			return
		}
		if ok, wait := lim.allow(source, joinOp(body) == node.JoinPoll); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": node.CodeRateLimited})
			return
		}
		rp := j.Handle(r.Context(), requestNode(r), source, body)
		w.Header().Set("Content-Type", "application/json")
		if rp.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int((rp.RetryAfter+time.Second-1)/time.Second)))
		}
		w.WriteHeader(rp.Status)
		_, _ = w.Write(rp.Body)
	})
}

// JoinClient implements node.JoinTransport over TLS: the joiner's own
// node certificate, and the server's key handed to the joiner's check
// inside the handshake, before any request byte is written.
type JoinClient struct {
	cert tls.Certificate
	err  error
	// Usable, if set, says whether an address is worth dialling from here
	// (PullClient.Usable); set before use.
	Usable func(addr string) bool
}

var _ node.JoinTransport = (*JoinClient)(nil)

// NewJoinClient joins as id.
func NewJoinClient(id *Identity) *JoinClient {
	cert, err := selfSigned(id)
	return &JoinClient{cert: cert, err: err}
}

// Join tries addrs in order. A key check that fails stops it at once:
// that address got a TLS handshake and nothing else.
func (c *JoinClient) Join(ctx context.Context, addrs []string, check func(string) error, body []byte) (node.JoinReply, error) {
	if c.err != nil {
		return node.JoinReply{}, c.err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	lastErr := errors.New("peer: no usable address")
	for _, addr := range addrs {
		if !validAddr(addr) || c.Usable != nil && !c.Usable(addr) {
			continue
		}
		var checkErr error
		cfg := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{c.cert},
			// No CA, so chain verification is replaced by the check of
			// the key itself, inside the handshake (as in PullClient).
			InsecureSkipVerify: true,
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				got, err := certNode(raw)
				if err != nil {
					return err
				}
				if err := check(got); err != nil {
					checkErr = err
					return err
				}
				return nil
			},
		}
		tr := &http.Transport{
			DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
			TLSClientConfig:       cfg,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: requestTimeout,
			DisableKeepAlives:     true,
			Proxy:                 nil,
		}
		hc := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("peer: redirects are not followed")
		}}
		u := url.URL{Scheme: "https", Host: addr, Path: pathJoinV2}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if checkErr != nil {
			if resp != nil {
				resp.Body.Close()
			}
			return node.JoinReply{}, checkErr
		}
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			continue
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxJoinV2Resp+1))
		resp.Body.Close()
		tr.CloseIdleConnections()
		if err != nil {
			return node.JoinReply{}, err
		}
		if int64(len(b)) > maxJoinV2Resp {
			return node.JoinReply{}, errors.New("peer: join answer too large")
		}
		rp := node.JoinReply{Status: resp.StatusCode, Body: b}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			rp.RetryAfter = time.Duration(s) * time.Second
		}
		return rp, nil
	}
	return node.JoinReply{}, lastErr
}
