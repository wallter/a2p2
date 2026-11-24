package peer

// POST /p/v2/dialback (PROTOCOL 4.7) and the probes of a2p2 net check:
// a TCP connection, then the TLS handshake pinned to the key expected,
// and nothing else. The rules of who may ask and which addresses are
// dialled are internal/node's (DialBackServer); here are the size limit,
// the transport and the handshake.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/node"
)

const pathDialBackV2 = "/p/v2/dialback"

// Timeouts of a probe: connect, then handshake. Variables for tests.
var (
	probeDialTimeout = 3 * time.Second
	probeTLSTimeout  = 5 * time.Second
)

// DialBackHandler answers the dial-back (node.DialBackServer). A Handler
// that also implements it serves /p/v2/dialback; one that does not
// answers 404, as an older machine does.
type DialBackHandler interface {
	DialBack(ctx context.Context, from, source string, q *node.DialBackRequest) (*node.DialBackResponse, *node.PullError)
}

func dialBackRoute(h DialBackHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		from := requestNode(r)
		if r.ContentLength > node.MaxDialBackBody {
			pullRefuse(w, &node.PullError{Status: http.StatusRequestEntityTooLarge, Code: node.CodeTooLarge})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, node.MaxDialBackBody))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				pullRefuse(w, &node.PullError{Status: http.StatusRequestEntityTooLarge, Code: node.CodeTooLarge})
			} else {
				pullRefuse(w, &node.PullError{Status: http.StatusBadRequest, Code: node.CodeBadRequest, Detail: "body"})
			}
			return
		}
		q, perr := node.DecodeDialBackRequest(body)
		if perr != nil {
			pullRefuse(w, perr)
			return
		}
		resp, perr := h.DialBack(r.Context(), from, r.RemoteAddr, q)
		if perr != nil {
			pullRefuse(w, perr)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

// DialBack asks peer, at its addresses in the book's order, to dial back
// to addrs (node.DialBackTransport). It goes only over an address of the
// same family as addrs[0], since the responder checks the address the
// request comes from. A refusal is a *node.PullError (404 from a machine
// without the dial-back).
func (c *PullClient) DialBack(ctx context.Context, peer string, addrs []string) (*node.DialBackResponse, error) {
	if c.err != nil {
		return nil, c.err
	}
	if !core.ValidNode(peer) {
		return nil, errors.New("peer: bad node id")
	}
	payload, err := json.Marshal(node.DialBackRequest{V: node.PullVersion, Addrs: addrs})
	if err != nil {
		return nil, err
	}
	// The responder dials for at most node.DialBackTimeout.
	ctx, cancel := context.WithTimeout(ctx, node.DialBackTimeout+pullTimeout)
	defer cancel()
	hc := c.client(peer)
	lastErr := errors.New("peer: no usable address")
	fam := ""
	if len(addrs) > 0 {
		fam = node.AddrFamily(addrs[0])
	}
	for _, addr := range c.book.Addrs(peer) {
		if !c.usable(addr) || node.AddrFamily(addr) != fam {
			continue
		}
		u := url.URL{Scheme: "https", Host: addr, Path: pathDialBackV2}
		hr, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
		if err != nil {
			continue
		}
		hr.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(hr)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			continue
		}
		c.book.Worked(peer, addr)
		return readDialBack(resp, addrs)
	}
	return nil, lastErr
}

func readDialBack(resp *http.Response, asked []string) (*node.DialBackResponse, error) {
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, node.MaxDialBackBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > node.MaxDialBackBody {
		return nil, errors.New("peer: dial-back answer too large")
	}
	if resp.StatusCode == http.StatusOK {
		return node.DecodeDialBackResponse(b, asked)
	}
	var retry time.Duration
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 && s <= 3600 {
		retry = time.Duration(s) * time.Second
	}
	return nil, node.DecodePullError(resp.StatusCode, b, retry)
}

// Prober tries addresses as this machine: node.Prober for a2p2 net check
// and node.Handshaker for the dial-back.
type Prober struct {
	cert tls.Certificate
	err  error
	// Usable, if set, says whether an address is worth dialling from here
	// (PullClient.Usable); set before use.
	Usable func(addr string) bool
}

var (
	_ node.Prober     = (*Prober)(nil)
	_ node.Handshaker = (*Prober)(nil)
)

// NewProber presents id's certificate.
func NewProber(id *Identity) *Prober {
	cert, err := selfSigned(id)
	return &Prober{cert: cert, err: err}
}

// NewThrowawayProber presents a fresh key made for this one use, so that
// testing an address before any invite tells the other side nothing about
// this machine.
func NewThrowawayProber() (*Prober, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return NewProber(&Identity{Key: key}), nil
}

// Probe tries addr expecting key (node.Prober).
func (p *Prober) Probe(ctx context.Context, key, addr string) node.AddrCheck {
	return p.ProbeWith(ctx, addr, func(got string) error {
		if got != key {
			return errWrongKey
		}
		return nil
	})
}

// Handshake reports whether key completed the handshake at addr
// (node.Handshaker).
func (p *Prober) Handshake(ctx context.Context, addr, key string) bool {
	return p.Probe(ctx, key, addr).Result == node.ProbeReached
}

var errWrongKey = errors.New("peer: certificate key mismatch")

// ProbeWith connects to addr and starts a TLS 1.3 handshake, presenting
// p's certificate, and accepts the server whose certificate names the
// node check accepts (nil: any a2p2 node). It then closes the connection:
// no HTTP request is sent, so nothing of an invite or a room reaches the
// other side. What the other side does learn: this machine's source
// address, a TLS ClientHello and, once its own certificate has been
// accepted, p's certificate (a throwaway one for net check HOST:PORT and
// --invite). A certificate naming another key is reported before the
// server proved it holds that key: a mismatch, not an identification.
func (p *Prober) ProbeWith(ctx context.Context, addr string, check func(node string) error) node.AddrCheck {
	out := node.AddrCheck{Addr: addr}
	start := time.Now()
	done := func(result, detail string) node.AddrCheck {
		out.Result, out.Detail, out.MS = result, detail, time.Since(start).Milliseconds()
		return out
	}
	if p.err != nil {
		return done(node.ProbeUnreachable, p.err.Error())
	}
	if !validAddr(addr) {
		return done(node.ProbeUnreachable, "not host:port")
	}
	if p.Usable != nil && !p.Usable(addr) {
		return done(node.ProbeNotTried, "a unique-local address of another site, which cannot be routed from here")
	}
	dctx, cancel := context.WithTimeout(ctx, probeDialTimeout)
	raw, err := (&net.Dialer{}).DialContext(dctx, "tcp", addr)
	cancel()
	if err != nil {
		switch {
		case isRefused(err):
			return done(node.ProbeRefused, "connection refused: nothing listens there")
		case isTimeout(err):
			return done(node.ProbeTimeout, fmt.Sprintf("no TCP answer within %s", probeDialTimeout))
		}
		return done(node.ProbeUnreachable, dialText(err))
	}
	defer raw.Close()
	var got string
	var checkErr error
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{p.cert},
		NextProtos:   []string{"h2"},
		// No CA: the key is checked inside the handshake, as for pulls.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			n, err := certNode(rawCerts)
			if err != nil {
				return err
			}
			got = n
			if check != nil {
				checkErr = check(n)
			}
			return checkErr
		},
	}
	tc := tls.Client(raw, cfg)
	hctx, cancel := context.WithTimeout(ctx, probeTLSTimeout)
	err = tc.HandshakeContext(hctx)
	cancel()
	out.Node = got
	switch {
	case checkErr != nil:
		return done(node.ProbeWrongKey, "certificate key mismatch: the certificate offered there names key "+
			core.ShortNode(got)+" (not proven to be held)")
	case err != nil && got != "":
		return done(node.ProbeNotA2P2, "handshake failed: "+err.Error())
	case err != nil && isTimeout(err):
		return done(node.ProbeTimeout, fmt.Sprintf("TCP connected, but no TLS answer within %s", probeTLSTimeout))
	case err != nil:
		return done(node.ProbeNotA2P2, "TCP connected, but it is not an a2p2 machine: "+err.Error())
	}
	_ = tc.Close()
	return done(node.ProbeReached, "")
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.As(err, &ne) && ne.Timeout()
}

// dialText is a dial error without the repeated "dial tcp" prefix.
func dialText(err error) string {
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		return oe.Err.Error()
	}
	return err.Error()
}
