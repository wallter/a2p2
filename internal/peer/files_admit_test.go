package peer

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/node"
)

// notHeld refuses every file request, as for a file it does not hold.
type notHeld struct{}

func (notHeld) ServeGet(context.Context, string, node.FileGet) (*node.FileServe, error) {
	return nil, node.ErrNotHeldFile
}

func (notHeld) ServePut(context.Context, string, node.FilePut, io.Reader) (int64, error) {
	return 0, node.ErrNotHeldFile
}

// TestFileRequestsDoNotSpendThePullExchangesAdmission (B7): a burst of
// file requests past the bucket's size, with no time passing, leaves the
// same machine's pull admitted; the file route refuses past its own bucket.
// Requirements: L1
func TestFileRequestsDoNotSpendThePullExchangesAdmission(t *testing.T) {
	lim := newLimiter()
	at := time.Now()
	lim.now = func() time.Time { return at }
	mux := http.NewServeMux()
	mux.Handle(pathFileV2, fileRoute(notHeld{}))
	mux.HandleFunc("POST "+pathPullV2, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := guard(limiters{member: lim, stranger: newLimiter()}, mux, nil)
	pub, _, _ := ed25519.GenerateKey(nil)
	state := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{PublicKey: pub}}}
	do := func(method, path string) int {
		r := httptest.NewRequest(method, path, nil)
		r.TLS = state
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	p := filePathOf("r_"+strings.Repeat("0", 32), strings.Repeat("1", 32), strings.Repeat("e", 64))
	limited := 0
	for i := 0; i < int(rateBurst)+20; i++ {
		if do(http.MethodGet, p) == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Error("the file route admitted a burst past its bucket")
	}
	if code := do(http.MethodPost, pathPullV2); code != http.StatusOK {
		t.Fatalf("file requests spent the pull exchange's admission: %d", code)
	}
}
