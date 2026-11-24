package portmap

// Every router here is a fake responder on loopback. Nothing in these
// tests sends a packet anywhere else.

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var loop = netip.MustParseAddr("127.0.0.1")

// fakeUDP answers each datagram with what answer returns, after delay.
type fakeUDP struct {
	conn   *net.UDPConn
	mu     sync.Mutex
	got    [][]byte
	answer func(q []byte) []byte
	delay  time.Duration
}

func newFakeUDP(t *testing.T, answer func(q []byte) []byte) *fakeUDP {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeUDP{conn: conn, answer: answer}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			q := append([]byte(nil), buf[:n]...)
			f.mu.Lock()
			f.got = append(f.got, q)
			delay, answer := f.delay, f.answer
			f.mu.Unlock()
			go func() {
				time.Sleep(delay)
				if r := answer(q); r != nil {
					_, _ = conn.WriteToUDP(r, from)
				}
			}()
		}
	}()
	return f
}

func (f *fakeUDP) port() uint16 { return uint16(f.conn.LocalAddr().(*net.UDPAddr).Port) }

func (f *fakeUDP) requests() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.got...)
}

// pcpRouter is a PCP server: it grants extPort on extIP, or refuses with
// result, and keeps mappings by nonce.
type pcpRouter struct {
	mu      sync.Mutex
	extIP   netip.Addr
	extPort uint16
	result  byte
	maps    map[string]uint16 // nonce -> external port
}

func (r *pcpRouter) answer(q []byte) []byte {
	if len(q) < 60 || q[0] != 2 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b := make([]byte, 60)
	copy(b, q)
	b[1] = 0x80 | q[1]
	b[2], b[3] = 0, r.result
	copy(b[4:8], q[4:8])
	clear(b[8:24])
	nonce := string(q[24:36])
	life := binary.BigEndian.Uint32(q[4:])
	if r.result == 0 {
		if life == 0 {
			delete(r.maps, nonce)
		} else {
			r.maps[nonce] = r.extPort
		}
		binary.BigEndian.PutUint16(b[42:], r.extPort)
		ip := r.extIP.As16()
		copy(b[44:], ip[:])
	}
	return b
}

func client(gw *fakeUDP) *Client {
	return &Client{Gateway: loop, Local: loop, Port: gw.port(), SSDP: "127.0.0.1:9", Try: 30 * time.Millisecond, Tries: 2}
}

func TestPCPMapsRenewsWithTheSameNonceAndRemoves(t *testing.T) {
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, maps: map[string]uint16{}}
	gw := newFakeUDP(t, r.answer)
	c := client(gw)
	m, err := c.Map(context.Background(), 7423, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The router gave another external port than the one suggested.
	if m.Protocol != PCP || m.External.String() != "203.0.113.7:40001" || m.Lifetime != time.Hour {
		t.Fatalf("mapping %+v", m)
	}
	if q := gw.requests()[0]; binary.BigEndian.Uint16(q[40:]) != 7423 || binary.BigEndian.Uint16(q[42:]) != 7423 ||
		netip.AddrFrom16([16]byte(q[8:24])).Unmap() != loop {
		t.Fatalf("request %x", q)
	}
	again, err := c.Map(context.Background(), 7423, time.Hour, &m)
	if err != nil || again.External != m.External || r.held() != 1 {
		t.Fatalf("renewal %+v %v, router holds %d", again, err, r.held())
	}
	if err := c.Unmap(context.Background(), again); err != nil || r.held() != 0 {
		t.Fatalf("unmap %v, router holds %d", err, r.held())
	}
}

func (r *pcpRouter) held() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.maps)
}

// natpmpRouter answers PCP with version 0 (it speaks NAT-PMP only).
func natpmpRouter(ext [4]byte, port uint16, maps *atomic.Int32) func([]byte) []byte {
	return func(q []byte) []byte {
		switch {
		case len(q) >= 1 && q[0] != 0:
			return []byte{0, 0x80 | q[1], 0, 1, 0, 0, 0, 1} // unsupported version
		case len(q) == 2 && q[1] == 0:
			b := make([]byte, 12)
			b[1] = 128
			copy(b[8:], ext[:])
			return b
		case len(q) == 12 && q[1] == 2:
			b := make([]byte, 16)
			b[1] = 130
			copy(b[8:10], q[4:6])
			life := binary.BigEndian.Uint32(q[8:])
			if life == 0 {
				maps.Add(-1)
			} else {
				maps.Add(1)
				binary.BigEndian.PutUint16(b[10:], port)
			}
			binary.BigEndian.PutUint32(b[12:], life)
			return b
		}
		return nil
	}
}

func TestARouterThatSpeaksOnlyNATPMPIsAskedInNATPMP(t *testing.T) {
	var maps atomic.Int32
	gw := newFakeUDP(t, natpmpRouter([4]byte{198, 51, 100, 9}, 50123, &maps))
	c := client(gw)
	m, err := c.Map(context.Background(), 7423, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Protocol != NATPMP || m.External.String() != "198.51.100.9:50123" || maps.Load() != 1 {
		t.Fatalf("mapping %+v (router holds %d)", m, maps.Load())
	}
	if err := c.Unmap(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
}

func TestARouterThatRefusesIsReportedWithEachProtocolsReason(t *testing.T) {
	r := &pcpRouter{result: 2, maps: map[string]uint16{}}
	gw := newFakeUDP(t, r.answer)
	_, err := client(gw).Map(context.Background(), 7423, time.Hour, nil)
	if err == nil || !strings.Contains(err.Error(), "PCP: refused: not authorized") || !strings.Contains(err.Error(), "UPnP:") {
		t.Fatalf("refusal: %v", err)
	}
}

func TestALateAnswerIsTakenAndOneTooLateFallsThrough(t *testing.T) {
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 7423, maps: map[string]uint16{}}
	gw := newFakeUDP(t, r.answer)
	gw.mu.Lock()
	gw.delay = 45 * time.Millisecond // after the first wait (30 ms), within the second (60 ms)
	gw.mu.Unlock()
	c := client(gw)
	if m, err := c.Map(context.Background(), 7423, time.Hour, nil); err != nil || m.Protocol != PCP {
		t.Fatalf("late: %+v %v", m, err)
	}
	gw.mu.Lock()
	gw.delay = 500 * time.Millisecond // after every try
	gw.mu.Unlock()
	if _, err := c.Map(context.Background(), 7423, time.Hour, nil); err == nil || !strings.Contains(err.Error(), "PCP: no answer") {
		t.Fatalf("too late: %v", err)
	}
}

// fakeIGD is a UPnP gateway on loopback: an SSDP responder and the HTTP
// description and control. location overrides the Location it gives.
type fakeIGD struct {
	ssdp     *fakeUDP
	http     *httptest.Server
	mu       sync.Mutex
	calls    []string
	ports    map[string]bool
	taken    map[string]bool
	location string
	control  string
}

func newFakeIGD(t *testing.T) *fakeIGD {
	g := &fakeIGD{ports: map[string]bool{}, taken: map[string]bool{}}
	g.http = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.http.Close)
	g.ssdp = newFakeUDP(t, func(q []byte) []byte {
		if !strings.HasPrefix(string(q), "M-SEARCH") {
			return nil
		}
		g.mu.Lock()
		loc := g.location
		g.mu.Unlock()
		if loc == "" {
			loc = g.http.URL + "/desc.xml"
		}
		return []byte("HTTP/1.1 200 OK\r\nST: " + igdDevice + "\r\nLOCATION: " + loc + "\r\n\r\n")
	})
	return g
}

func (g *fakeIGD) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.URL.Path == "/desc.xml" {
		g.calls = append(g.calls, "describe")
		control := g.control
		if control == "" {
			control = "/ctl"
		}
		fmt.Fprintf(w, `<?xml version="1.0"?><root><device><deviceList><device><deviceList><device><serviceList><service>`+
			`<serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType><controlURL>%s</controlURL>`+
			`</service></serviceList></device></deviceList></device></deviceList></device></root>`, control)
		return
	}
	body, _ := io.ReadAll(r.Body)
	action := strings.TrimSuffix(strings.SplitN(r.Header.Get("SOAPAction"), "#", 2)[1], `"`)
	g.calls = append(g.calls, action)
	field := func(name string) string {
		s := string(body)
		i := strings.Index(s, "<"+name+">")
		j := strings.Index(s, "</"+name+">")
		if i < 0 || j < 0 {
			return ""
		}
		return s[i+len(name)+2 : j]
	}
	switch action {
	case "AddPortMapping":
		p := field("NewExternalPort")
		if g.taken[p] {
			w.WriteHeader(500)
			fmt.Fprint(w, `<s:Envelope><s:Body><s:Fault><detail><UPnPError><errorCode>718</errorCode>`+
				`<errorDescription>ConflictInMappingEntry</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`)
			return
		}
		if field("NewInternalClient") != "127.0.0.1" || field("NewProtocol") != "TCP" {
			w.WriteHeader(500)
			return
		}
		g.ports[p] = true
		fmt.Fprint(w, `<s:Envelope><s:Body><u:AddPortMappingResponse/></s:Body></s:Envelope>`)
	case "GetExternalIPAddress":
		fmt.Fprint(w, `<s:Envelope><s:Body><u:GetExternalIPAddressResponse><NewExternalIPAddress>192.0.2.44</NewExternalIPAddress>`+
			`</u:GetExternalIPAddressResponse></s:Body></s:Envelope>`)
	case "DeletePortMapping":
		delete(g.ports, field("NewExternalPort"))
		fmt.Fprint(w, `<s:Envelope><s:Body><u:DeletePortMappingResponse/></s:Body></s:Envelope>`)
	}
}

func (g *fakeIGD) has(port string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ports[port]
}

// noPCP is a gateway port on which nothing answers PCP or NAT-PMP.
func noPCP(t *testing.T) *fakeUDP { return newFakeUDP(t, func([]byte) []byte { return nil }) }

func TestUPnPMapsATakenPortToTheNextAndRemovesIt(t *testing.T) {
	g := newFakeIGD(t)
	g.mu.Lock()
	g.taken["7423"] = true
	g.mu.Unlock()
	c := client(noPCP(t))
	c.SSDP = fmt.Sprintf("127.0.0.1:%d", g.ssdp.port())
	m, err := c.Map(context.Background(), 7423, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Protocol != UPnP || m.External.String() != "192.0.2.44:7424" || !g.has("7424") {
		t.Fatalf("mapping %+v", m)
	}
	if err := c.Unmap(context.Background(), m); err != nil || g.has("7424") {
		t.Fatalf("unmap %v", err)
	}
}

func TestUPnPFollowsNoURLThatIsNotTheGateway(t *testing.T) {
	g := newFakeIGD(t)
	g.mu.Lock()
	g.location = "http://192.0.2.1:1/desc.xml" // another host: never fetched
	g.mu.Unlock()
	c := client(noPCP(t))
	c.SSDP = fmt.Sprintf("127.0.0.1:%d", g.ssdp.port())
	if _, err := c.Map(context.Background(), 7423, time.Hour, nil); err == nil {
		t.Fatal("mapped through a description elsewhere")
	}
	g.mu.Lock()
	g.location = ""
	g.control = "http://192.0.2.1:1/ctl" // a control URL elsewhere: refused
	g.mu.Unlock()
	_, err := c.Map(context.Background(), 7423, time.Hour, nil)
	if err == nil || !strings.Contains(err.Error(), "not on the gateway") {
		t.Fatalf("control elsewhere: %v", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, call := range g.calls {
		if call != "describe" {
			t.Fatalf("calls %v", g.calls)
		}
	}
}

// lossy is a Mapper whose router forgets each mapping: every renewal
// comes back on another external port.
type lossy struct {
	mu     sync.Mutex
	next   uint16
	unmaps []Mapping
}

func (l *lossy) Map(_ context.Context, internal uint16, life time.Duration, _ *Mapping) (Mapping, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	return Mapping{Protocol: PCP, Internal: internal, Lifetime: 2 * minRenew,
		External: netip.AddrPortFrom(netip.MustParseAddr("203.0.113.7"), 40000+l.next)}, nil
}

func (l *lossy) Unmap(_ context.Context, m Mapping) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.unmaps = append(l.unmaps, m)
	return nil
}

func TestAKeeperFollowsARouterThatLosesTheMappingAndRemovesItAtTheEnd(t *testing.T) {
	defer func(d time.Duration) { minRenew = d }(minRenew)
	minRenew = 20 * time.Millisecond
	l := &lossy{}
	var mu sync.Mutex
	var seen []string
	k := NewKeeper(l, 7423, func(st State) {
		mu.Lock()
		defer mu.Unlock()
		if st.Mapping != nil {
			seen = append(seen, st.Mapping.External.String())
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { k.Run(ctx); close(done) }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no renewal")
		}
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if seen[0] == seen[1] {
		t.Fatalf("the new external port was not taken: %v", seen)
	}
	last := k.State().Mapping
	if len(l.unmaps) != 1 || last == nil || l.unmaps[0].External != last.External {
		t.Fatalf("removed %v, last %v", l.unmaps, last)
	}
}
