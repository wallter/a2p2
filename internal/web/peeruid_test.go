package web

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const (
	procTCPHead  = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	procTCP6Head = "  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
)

// tcpLine and tcp6Line are lines as Linux writes them.
func tcpLine(local, remote, st, uid string) string {
	return "   0: " + local + " " + remote + " " + st + " 00000000:00000000 00:00000000 00000000  " + uid + "        0 31337 1 0000000000000000 20 4 30 10 -1\n"
}

func tcp6Line(local, remote, st, uid string) string {
	return "   0: " + local + " " + remote + " " + st + " 00000000:00000000 00:00000000 00000000  " + uid + "        0 31338 1 0000000000000000 20 4 30 10 -1\n"
}

// F14, third review: the client's socket of a page connection is found by
// its whole four-tuple, local address and port and remote address and
// port, never by the ports alone. Loopback on Linux is all of 127.0.0.0/8,
// so another user can hold a socket on 127.0.0.2 with the same ports as
// the person's browser; that line must be passed over in either order.
func TestThePeerOfAPageConnectionIsFoundByItsFullFourTuple(t *testing.T) {
	v4c := netip.MustParseAddrPort("127.0.0.1:54321") // D431
	v4s := netip.MustParseAddrPort("127.0.0.1:7422")  // 1CFE
	v6c := netip.MustParseAddrPort("[::1]:54321")
	v6s := netip.MustParseAddrPort("[::1]:7422")
	const (
		lo1  = "0100007F"
		lo2  = "0200007F"
		lo6  = "00000000000000000000000001000000"
		map1 = "0000000000000000FFFF00000100007F"
	)
	cases := []struct {
		name           string
		text           string
		order          binary.ByteOrder
		client, server netip.AddrPort
		want           []int
		bad            bool
	}{
		{"decoy on 127.0.0.2 first", procTCPHead +
			tcpLine(lo2+":D431", lo2+":1CFE", "01", "1003") +
			tcpLine(lo1+":D431", lo1+":1CFE", "01", "1002"),
			binary.LittleEndian, v4c, v4s, []int{1002}, false},
		{"decoy on 127.0.0.2 after", procTCPHead +
			tcpLine(lo1+":D431", lo1+":1CFE", "01", "1002") +
			tcpLine(lo2+":D431", lo2+":1CFE", "01", "1003"),
			binary.LittleEndian, v4c, v4s, []int{1002}, false},
		{"decoy to 127.0.0.2 from the same address", procTCPHead +
			tcpLine(lo1+":D431", lo2+":1CFE", "01", "1003") +
			tcpLine(lo1+":D431", lo1+":1CFE", "01", "1002"),
			binary.LittleEndian, v4c, v4s, []int{1002}, false},
		{"only the daemon's own socket and the listener", procTCPHead +
			tcpLine(lo1+":1CFE", "00000000:0000", "0A", "1002") +
			tcpLine(lo1+":1CFE", lo1+":D431", "01", "1002"),
			binary.LittleEndian, v4c, v4s, nil, false},
		{"TIME_WAIT has no owner", procTCPHead +
			tcpLine(lo1+":D431", lo1+":1CFE", "06", "0"),
			binary.LittleEndian, v4c, v4s, nil, false},
		{"only a decoy: no match", procTCPHead +
			tcpLine(lo2+":D431", lo2+":1CFE", "01", "1003"),
			binary.LittleEndian, v4c, v4s, nil, false},
		{"two matching lines", procTCPHead +
			tcpLine(lo1+":D431", lo1+":1CFE", "01", "1002") +
			tcpLine(lo1+":D431", lo1+":1CFE", "01", "1003"),
			binary.LittleEndian, v4c, v4s, []int{1002, 1003}, false},
		{"IPv4-mapped client in tcp6", procTCP6Head +
			tcp6Line(map1+":D431", map1+":1CFE", "01", "1002") +
			tcp6Line("0000000000000000FFFF00000200007F:D431", "0000000000000000FFFF00000200007F:1CFE", "01", "1003"),
			binary.LittleEndian, v4c, v4s, []int{1002}, false},
		{"::1", procTCP6Head +
			tcp6Line(lo6+":D431", lo6+":1CFE", "01", "1002") +
			tcp6Line(map1+":D431", map1+":1CFE", "01", "1003"),
			binary.LittleEndian, v6c, v6s, []int{1002}, false},
		{"big-endian machine", procTCPHead +
			tcpLine("7F000002:D431", "7F000002:1CFE", "01", "1003") +
			tcpLine("7F000001:D431", "7F000001:1CFE", "01", "1002"),
			binary.BigEndian, v4c, v4s, []int{1002}, false},
		{"a line that does not parse", procTCPHead +
			tcpLine(lo1+":D431", lo1+":1CFE", "01", "1002") +
			tcpLine(lo1+":D43", lo1+":1CFE", "01", "1003"),
			binary.LittleEndian, v4c, v4s, nil, true},
		{"a uid that does not parse", procTCPHead +
			tcpLine(lo1+":D431", lo1+":1CFE", "01", "x"),
			binary.LittleEndian, v4c, v4s, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := procNetMatches(c.text, c.order, c.client, c.server)
			if (err != nil) != c.bad || !slices.Equal(got, c.want) {
				t.Fatalf("got %v, %v; want %v, error %v", got, err, c.want, c.bad)
			}
		})
	}
}

// The user is known only from exactly one match across tcp and tcp6; no
// match, two, a table that cannot be read or does not parse leave it not
// known, which lets the code alone decide (D67). A missing tcp6 (IPv6 off)
// is no problem.
func TestAPeerLookupWithNoSingleMatchIsNotKnown(t *testing.T) {
	c := netip.MustParseAddrPort("127.0.0.1:54321")
	s := netip.MustParseAddrPort("127.0.0.1:7422")
	dir := t.TempDir()
	file := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	person := procTCPHead + tcpLine("0100007F:D431", "0100007F:1CFE", "01", "1002")
	mapped := procTCP6Head + tcp6Line("0000000000000000FFFF00000100007F:D431", "0000000000000000FFFF00000100007F:1CFE", "01", "1003")
	empty6 := file("empty6", procTCP6Head)
	cases := []struct {
		name  string
		files []string
		uid   int
		known bool
	}{
		{"one match", []string{file("one", person), empty6}, 1002, true},
		{"one match, no tcp6", []string{file("one2", person), filepath.Join(dir, "absent")}, 1002, true},
		{"one in tcp, one in tcp6", []string{file("both", person), file("mapped", mapped)}, 0, false},
		{"no match", []string{file("none", procTCPHead), empty6}, 0, false},
		{"tcp cannot be read", []string{filepath.Join(dir, "absent"), empty6}, 0, false},
		{"tcp6 does not parse", []string{file("one3", person), file("bad6", procTCP6Head+"   0: zz\n")}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uid, known := procNetLookup(tc.files, binary.LittleEndian, c, s)
			if uid != tc.uid || known != tc.known {
				t.Fatalf("got %d %v, want %d %v", uid, known, tc.uid, tc.known)
			}
		})
	}
}

// On Linux, the client of a real loopback connection is found as this
// user, by the kernel's own text, while another socket on 127.0.0.2 holds
// the same two ports; by the ports alone the two would both match.
// Elsewhere the user is not known and the code alone decides.
func TestTheClientOfARealLoopbackConnectionIsThisUser(t *testing.T) {
	var uid int
	var known, tuple bool
	var byPorts, byTuple int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var c, s netip.AddrPort
		c, s, tuple = connTuple(r)
		uid, known = peerUID(r)
		if b, err := os.ReadFile("/proc/net/tcp"); err == nil {
			m, _ := procNetMatches(string(b), binary.NativeEndian, c, s)
			byTuple = len(m)
			for _, line := range strings.Split(string(b), "\n") {
				f := strings.Fields(line)
				if len(f) > 3 && strings.HasSuffix(f[1], ":"+hex4(c.Port())) && strings.HasSuffix(f[2], ":"+hex4(s.Port())) {
					byPorts++
				}
			}
		}
	}))
	defer srv.Close()
	client := http.DefaultClient
	if runtime.GOOS == "linux" {
		port := netip.MustParseAddrPort(srv.Listener.Addr().String()).Port()
		// The decoy: a listener on 127.0.0.2 at the server's port, and a
		// connection to it from 127.0.0.2, whose source port the real
		// connection then uses from 127.0.0.1.
		ln, err := net.Listen("tcp", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.2"), port).String())
		if err != nil {
			t.Skipf("no 127.0.0.2 here: %v", err)
		}
		defer ln.Close()
		// Bound before it connects, so its port is held by address.
		dd := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2)}}
		decoy, err := dd.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer decoy.Close()
		from := net.TCPAddrFromAddrPort(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), netip.MustParseAddrPort(decoy.LocalAddr().String()).Port()))
		d := &net.Dialer{LocalAddr: from}
		client = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, n, a string) (net.Conn, error) { return d.DialContext(ctx, n, a) }}}
	}
	res, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if !tuple {
		t.Fatal("no four-tuple for a TCP connection")
	}
	if runtime.GOOS == "linux" {
		if !known || uid != os.Getuid() {
			t.Fatalf("the client of a loopback connection: uid %d, known %v", uid, known)
		}
		if byTuple != 1 || byPorts != 2 {
			t.Fatalf("matches by four-tuple %d (want 1), by ports alone %d (want 2, with the decoy)", byTuple, byPorts)
		}
	} else if known {
		t.Fatal("a user was found where the system does not say")
	}
}

func hex4(p uint16) string {
	const digits = "0123456789ABCDEF"
	return string([]byte{digits[p>>12], digits[p>>8&15], digits[p>>4&15], digits[p&15]})
}
