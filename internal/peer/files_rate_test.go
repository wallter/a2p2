package peer

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/node"
)

// slowServe answers every GET with size bytes and says when the response
// ended (its body is closed when the handler returns).
type slowServe struct {
	size   int
	limits node.RateLimits
	once   sync.Once
	closed chan struct{}
}

type closeBody struct {
	io.Reader
	s *slowServe
}

func (b closeBody) Close() error { b.s.once.Do(func() { close(b.s.closed) }); return nil }

func (s *slowServe) ServeGet(context.Context, string, node.FileGet) (*node.FileServe, error) {
	return &node.FileServe{Status: http.StatusOK, Total: int64(s.size), Length: int64(s.size),
		Body: closeBody{bytes.NewReader(make([]byte, s.size)), s}, Limits: s.limits}, nil
}

func (s *slowServe) ServePut(context.Context, string, node.FilePut, io.Reader) (int64, error) {
	return 0, node.ErrNotHeldFile
}

// TestAnOutboundResponseToAReaderBelowTheMinimumRateIsCut (B7): over a
// real connection on loopback, a requester that stops reading after the
// headers has the response cut by the minimum rate, long before the
// response's total time.
// Requirements: L1
func TestAnOutboundResponseToAReaderBelowTheMinimumRateIsCut(t *testing.T) {
	s := &slowServe{size: 64 << 20, closed: make(chan struct{}),
		limits: node.RateLimits{Total: time.Minute, Window: 300 * time.Millisecond, MinRate: 1 << 20, Now: time.Now}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: fileRoute(s)}
	go srv.Serve(ln)
	defer srv.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	path := filePathOf("r_"+strings.Repeat("0", 32), strings.Repeat("1", 32), strings.Repeat("e", 64))
	if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	// the requester reads nothing more
	select {
	case <-s.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("a response to a reader that stopped was not cut")
	}
}
