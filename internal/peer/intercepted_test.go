package peer

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/node"
)

// inspector is a TLS listener that answers as an inspecting proxy does: the
// subject of the leaf is the listener's own 64-hex key, the issuer is the
// proxy's CA.
type inspector struct {
	addr string
	node string // the key named by the leaf's CN
}

// startInspector serves a certificate issued by a CA named issuer. chain:
// send the CA certificate after the leaf (two certificates); otherwise the
// leaf alone.
func startInspector(t *testing.T, issuer string, chain bool) inspector {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: issuer},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(pub)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: id},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, pub, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: key}
	if chain {
		cert.Certificate = append(cert.Certificate, caDER)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				_ = tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}).Handshake()
			}()
		}
	}()
	return inspector{addr: ln.Addr().String(), node: id}
}

// A network that inspects TLS re-signs the listener's certificate: the leaf
// keeps the listener's CN, a CA of the network issued it, and the chain has
// two certificates or one. Both are reported as intercepted, with the
// issuer's name, not as "not an a2p2 machine".
//
// Requirements: N2
func TestProbeOfAnInspectedConnectionIsInterceptedWithItsIssuer(t *testing.T) {
	p := NewProber(newID(t))
	for _, c := range []struct {
		name  string
		chain bool
	}{{"a two-certificate chain", true}, {"a single CA-issued leaf", false}} {
		t.Run(c.name, func(t *testing.T) {
			in := startInspector(t, "Example Corp Inspection CA", c.chain)
			got := p.Probe(bg, in.node, in.addr)
			if got.Result != node.ProbeIntercepted || got.Issuer != "Example Corp Inspection CA" ||
				!strings.Contains(got.Detail, `certificate issued by "Example Corp Inspection CA"`) {
				t.Fatalf("%+v", got)
			}
			if got := p.ProbeWith(bg, in.addr, nil); got.Result != node.ProbeIntercepted {
				t.Fatalf("any key: %+v", got)
			}
		})
	}
}

// The issuer's name is the other side's text: it is cut, and a control
// character or a bidirectional mark is written out, never sent to the
// terminal.
//
// Requirements: N2
func TestAnInterceptedIssuerNameIsBoundedAndEscaped(t *testing.T) {
	in := startInspector(t, "Evil\x1b[2J‮CA "+strings.Repeat("x", 300), true)
	got := NewProber(newID(t)).Probe(bg, in.node, in.addr)
	if got.Result != node.ProbeIntercepted {
		t.Fatalf("%+v", got)
	}
	for _, s := range []string{got.Issuer, got.Detail} {
		if strings.ContainsAny(s, "\x1b‮") || len(s) > 400 {
			t.Fatalf("not safe to show: %q", s)
		}
	}
	if !strings.Contains(got.Issuer, `\u{1b}`) || !strings.HasSuffix(got.Issuer, "...") {
		t.Fatalf("%q", got.Issuer)
	}
}

// A join meets the same condition: the error says what it is, in the same
// words as the check, not "want exactly one certificate".
//
// Requirements: N2
func TestAJoinThroughAnInspectingProxySaysSo(t *testing.T) {
	in := startInspector(t, "Example Corp Inspection CA", true)
	_, err := NewJoinClient(newID(t)).Join(bg, []string{in.addr}, func(string) error { return nil }, []byte("{}"))
	var ic *Intercepted
	if !errors.As(err, &ic) || ic.Issuer != "Example Corp Inspection CA" ||
		!strings.Contains(err.Error(), "TLS-inspecting proxy") || strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("%v", err)
	}
}

// What is not an inspector keeps its answer: a web server with a CA-issued
// certificate that names no machine key, a plain TCP listener, a real a2p2
// machine, and an a2p2 machine under another key.
//
// Requirements: N2
func TestProbeKeepsItsOtherAnswersNextToIntercepted(t *testing.T) {
	a := newPM(t, "a", true)
	p := NewProber(newID(t))
	plain := silent(t)
	if got := p.Probe(bg, a.node(), a.addr); got.Result != node.ProbeReached {
		t.Fatalf("a2p2 machine: %+v", got)
	}
	if got := p.Probe(bg, newID(t).NodeID(), a.addr); got.Result != node.ProbeWrongKey {
		t.Fatalf("wrong key: %+v", got)
	}
	defer func(d time.Duration) { probeTLSTimeout = d }(probeTLSTimeout)
	probeTLSTimeout = time.Second
	if got := p.Probe(bg, a.node(), plain); got.Result != node.ProbeTimeout {
		t.Fatalf("plain TCP that never answers: %+v", got)
	}
	// A TLS server whose leaf names no machine key is not a2p2 at all.
	if err := certNodeOfWebServer(t); err == nil || errors.As(err, new(*Intercepted)) {
		t.Fatalf("a certificate naming no key: %v", err)
	}
}

// certNodeOfWebServer is certNode's answer for a CA-issued two-certificate
// chain whose leaf has an ordinary host name as subject.
func certNodeOfWebServer(t *testing.T) error {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caT := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Public CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caT, caT, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	lt := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "www.example.org"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, lt, ca, pub, caKey)
	if err != nil {
		t.Fatal(err)
	}
	_, err = certNode([][]byte{der, caDER})
	return err
}

// "No route to host" at an address inside a subnet attached to this
// machine means the system has the route and refuses it (a VPN client or
// a firewall); at any other address, or with another error, it is the
// ordinary unreachable.
//
// Requirements: N2
func TestNoRouteToAnAttachedSubnetIsAVPNOrFirewallBlock(t *testing.T) {
	nets := []netip.Prefix{netip.MustParsePrefix("192.168.86.0/24"), netip.MustParsePrefix("fe80::/64")}
	dial := func(ip string, err error) error {
		return &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP(ip), Port: 7423}, Err: err}
	}
	for _, c := range []struct {
		name string
		err  error
		host string
		want bool
	}{
		{"no route on the attached subnet", dial("192.168.86.241", syscall.EHOSTUNREACH), "192.168.86.241", true},
		{"no route on the attached IPv6 link", dial("fe80::1", syscall.EHOSTUNREACH), "fe80::1", true},
		{"no route elsewhere", dial("203.0.113.9", syscall.EHOSTUNREACH), "203.0.113.9", false},
		{"network unreachable on the subnet", dial("192.168.86.241", syscall.ENETUNREACH), "192.168.86.241", false},
		{"refused on the subnet", dial("192.168.86.241", syscall.ECONNREFUSED), "192.168.86.241", false},
		{"no route, host name resolved to the subnet", dial("192.168.86.7", syscall.EHOSTUNREACH), "printer.local", true},
		{"no route, no address known", errors.New("connect: no route to host"), "printer.local", false},
	} {
		if got := noRouteOnLink(c.err, c.host, nets); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if noRouteOnLink(dial("192.168.86.241", syscall.EHOSTUNREACH), "192.168.86.241", nil) {
		t.Error("no attached subnet is known")
	}
}

// TestWhatIsAcceptedDidNotChangeAndOnlyAReSignedLeafIsCalledIntercepted pins
// the two halves apart: ownCert alone decides acceptance, and a refusal is
// named Intercepted only for a leaf that names a machine's key yet was
// re-signed. A proxy's leaf usually holds the proxy's own key, not Ed25519.
func TestWhatIsAcceptedDidNotChangeAndOnlyAReSignedLeafIsCalledIntercepted(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(pub)
	mk := func(cn, issuer string, leafPub any, signer any) []byte {
		t.Helper()
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
		parent := tmpl
		if issuer != "" {
			parent = &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: issuer},
				NotBefore: tmpl.NotBefore, NotAfter: tmpl.NotAfter, IsCA: true, BasicConstraintsValid: true}
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, leafPub, signer)
		if err != nil {
			t.Fatal(err)
		}
		return der
	}
	proxyKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	own := mk(id, "", pub, key)

	if got, err := certNode([][]byte{own}); err != nil || got != id {
		t.Fatalf("a machine's own certificate: %q, %v", got, err)
	}
	var icpt *Intercepted
	for name, certs := range map[string][][]byte{
		"a leaf with the proxy's own key, issued by its CA": {mk(id, "Inspection CA", &proxyKey.PublicKey, proxyKey)},
		"the machine's key, signed by a CA":                 {mk(id, "Inspection CA", pub, proxyKey)},
		"the machine's certificate with a second one":       {own, own},
	} {
		if _, err := certNode(certs); !errors.As(err, &icpt) {
			t.Errorf("%s: want Intercepted, got %v", name, err)
		}
	}
	for name, certs := range map[string][][]byte{
		"one self-signed certificate that names another machine's key": {mk(id, "", otherPub, otherKey)},
		"a server that names no machine key":                           {mk("example.com", "Some CA", &proxyKey.PublicKey, proxyKey)},
		"no certificate":                                               {},
	} {
		if _, err := certNode(certs); err == nil || errors.As(err, &icpt) {
			t.Errorf("%s: want a plain refusal, got %v", name, err)
		}
	}
}
