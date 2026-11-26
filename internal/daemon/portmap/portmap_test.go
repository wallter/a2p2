package portmap

// Every router here is a fake responder on loopback. Nothing in these
// tests sends a packet anywhere else.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	// grant, if set, is the lifetime granted in seconds; step moves the
	// external port on every grant (a router that loses mappings);
	// silentMap and silentDel drop requests that map or delete.
	grant                uint32
	step                 uint16
	silentMap, silentDel bool
}

func (r *pcpRouter) set(f func(r *pcpRouter)) { r.mu.Lock(); defer r.mu.Unlock(); f(r) }

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
	if (life == 0 && r.silentDel) || (life != 0 && r.silentMap) {
		return nil
	}
	if r.result == 0 {
		if life == 0 {
			delete(r.maps, nonce)
		} else {
			r.extPort += r.step
			r.maps[nonce] = r.extPort
			if r.grant != 0 {
				binary.BigEndian.PutUint32(b[4:], r.grant)
			}
		}
		binary.BigEndian.PutUint16(b[42:], r.extPort)
		ip := r.extIP.As16()
		copy(b[44:], ip[:])
	}
	return b
}

// testTry is the client's first wait for a UDP answer in tests. A fake
// that answers does so at once, so this costs time only where nothing
// answers (3*testTry); it is long enough that a loaded machine running
// the race detector still delivers a fake's answer within it.
const testTry = 250 * time.Millisecond

func client(gw *fakeUDP) *Client {
	return &Client{Gateway: loop, Local: loop, Port: gw.port(), SSDP: "127.0.0.1:9", Try: testTry, Tries: 2}
}

func TestPCPMapsRenewsWithTheSameNonceAndRemoves(t *testing.T) {
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, maps: map[string]uint16{}}
	gw := newFakeUDP(t, r.answer)
	c := client(gw)
	m, err := c.Map(context.Background(), 7423, time.Hour, nil, nil)
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
	again, err := c.Map(context.Background(), 7423, time.Hour, &m, nil)
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
	m, err := c.Map(context.Background(), 7423, time.Hour, nil, nil)
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
	_, err := client(gw).Map(context.Background(), 7423, time.Hour, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "PCP: refused: not authorized") || !strings.Contains(err.Error(), "UPnP:") {
		t.Fatalf("refusal: %v", err)
	}
}

func TestALateAnswerIsTakenAndOneTooLateFallsThrough(t *testing.T) {
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 7423, maps: map[string]uint16{}}
	gw := newFakeUDP(t, r.answer)
	gw.mu.Lock()
	// After the first wait ends (testTry) by half a try, and a try and a
	// half before the second ends (3*testTry).
	gw.delay = testTry * 3 / 2
	gw.mu.Unlock()
	c := client(gw)
	if m, err := c.Map(context.Background(), 7423, time.Hour, nil, nil); err != nil || m.Protocol != PCP {
		t.Fatalf("late: %+v %v", m, err)
	}
	gw.mu.Lock()
	gw.delay = testTry * 5 // after every try (the last ends at 3*testTry)
	gw.mu.Unlock()
	if _, err := c.Map(context.Background(), 7423, time.Hour, nil, nil); err == nil || !strings.Contains(err.Error(), "PCP: no answer") {
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
	ports    map[string]igdEntry
	taken    map[string]bool
	badFault bool // faults cut short
	location string
	control  string
	reply    string // the SSDP answer, with %s for the location
	cut      bool   // SOAP answers cut short
	leases   []string
	// onlyPermanent: refuse finite leases with 725.
	onlyPermanent bool
}

func (g *fakeIGD) called() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, c := range g.calls {
		if c != "describe" {
			out = append(out, c)
		}
	}
	return out
}

func (g *fakeIGD) leasesAsked() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.leases)
}

// igdEntry is one forward the fake gateway holds.
type igdEntry struct{ client, port, desc string }

func newFakeIGD(t *testing.T) *fakeIGD {
	g := &fakeIGD{ports: map[string]igdEntry{}, taken: map[string]bool{}}
	g.http = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.http.Close)
	g.ssdp = newFakeUDP(t, func(q []byte) []byte {
		if !strings.HasPrefix(string(q), "M-SEARCH") {
			return nil
		}
		g.mu.Lock()
		loc, reply := g.location, g.reply
		g.mu.Unlock()
		if loc == "" {
			loc = g.http.URL + "/desc.xml"
		}
		if reply == "" {
			reply = "HTTP/1.1 200 OK\r\nST: " + igdDevice + "\r\nLOCATION: %s\r\n\r\n"
		}
		return []byte(fmt.Sprintf(reply, loc))
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
	if g.cut {
		fmt.Fprint(w, `<s:Envelope><s:Body><u:`+action+`Resp`)
		return
	}
	fault := func(code string) {
		w.WriteHeader(500)
		if g.badFault {
			fmt.Fprint(w, `<s:Envelope><s:Body><s:Fault><detail><UPnPError><errorCode>`+code)
			return
		}
		fmt.Fprint(w, `<s:Envelope><s:Body><s:Fault><detail><UPnPError><errorCode>`+code+`</errorCode>`+
			`</UPnPError></detail></s:Fault></s:Body></s:Envelope>`)
	}
	switch action {
	case "AddPortMapping":
		p := field("NewExternalPort")
		g.leases = append(g.leases, field("NewLeaseDuration"))
		if g.onlyPermanent && field("NewLeaseDuration") != "0" {
			fault("725")
			return
		}
		if g.taken[p] {
			fault("718")
			return
		}
		if field("NewInternalClient") != "127.0.0.1" || field("NewProtocol") != "TCP" {
			w.WriteHeader(500)
			return
		}
		g.ports[p] = igdEntry{client: field("NewInternalClient"), port: field("NewInternalPort"), desc: field("NewPortMappingDescription")}
		fmt.Fprint(w, `<s:Envelope><s:Body><u:AddPortMappingResponse/></s:Body></s:Envelope>`)
	case "GetSpecificPortMappingEntry":
		e, ok := g.ports[field("NewExternalPort")]
		if !ok {
			fault("714")
			return
		}
		fmt.Fprintf(w, `<s:Envelope><s:Body><u:GetSpecificPortMappingEntryResponse><NewInternalPort>%s</NewInternalPort>`+
			`<NewInternalClient>%s</NewInternalClient><NewEnabled>1</NewEnabled><NewPortMappingDescription>%s</NewPortMappingDescription>`+
			`<NewLeaseDuration>0</NewLeaseDuration></u:GetSpecificPortMappingEntryResponse></s:Body></s:Envelope>`, e.port, e.client, e.desc)
	case "GetExternalIPAddress":
		fmt.Fprint(w, `<s:Envelope><s:Body><u:GetExternalIPAddressResponse><NewExternalIPAddress>192.0.2.44</NewExternalIPAddress>`+
			`</u:GetExternalIPAddressResponse></s:Body></s:Envelope>`)
	case "DeletePortMapping":
		if _, ok := g.ports[field("NewExternalPort")]; !ok {
			fault("714")
			return
		}
		delete(g.ports, field("NewExternalPort"))
		fmt.Fprint(w, `<s:Envelope><s:Body><u:DeletePortMappingResponse/></s:Body></s:Envelope>`)
	}
}

func (g *fakeIGD) has(port string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.ports[port]
	return ok
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
	m, err := c.Map(context.Background(), 7423, time.Hour, nil, nil)
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
	if _, err := c.Map(context.Background(), 7423, time.Hour, nil, nil); err == nil {
		t.Fatal("mapped through a description elsewhere")
	}
	g.mu.Lock()
	g.location = ""
	g.control = "http://192.0.2.1:1/ctl" // a control URL elsewhere: refused
	g.mu.Unlock()
	_, err := c.Map(context.Background(), 7423, time.Hour, nil, nil)
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

// record is the real record file in a test's directory, for this
// test network's router (loopback).
func record(t *testing.T) *File {
	return &File{Path: filepath.Join(t.TempDir(), "portmap.json"), Gateway: loop}
}

// get is what the file holds now, read back from disk.
func get(t *testing.T, f *File) []Mapping {
	t.Helper()
	ms, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func runKeeper(k *Keeper) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { k.Run(ctx); close(done) }()
	return func() { cancel(); <-done }
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
	}
}

// A router that loses the mapping gives a new port at each renewal: the
// keeper takes it, removes the one it replaced, renews at half of a
// one-second lease, and removes the last at the end.
func TestAKeeperFollowsARouterThatLosesTheMappingAndRemovesItAtTheEnd(t *testing.T) {
	defer func(d time.Duration) { minWait = d }(minWait)
	minWait = time.Millisecond
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40000, step: 1, grant: 1, maps: map[string]uint16{}}
	gw := newFakeUDP(t, r.answer)
	var mu sync.Mutex
	var seen []string
	rec := record(t)
	k := NewKeeper(client(gw), 7423, rec, func(st State) {
		mu.Lock()
		defer mu.Unlock()
		if st.Mapping != nil && (len(seen) == 0 || seen[len(seen)-1] != st.Mapping.External.String()) {
			seen = append(seen, st.Mapping.External.String())
		}
	})
	stop := runKeeper(k)
	waitFor(t, "renewals", func() bool { mu.Lock(); defer mu.Unlock(); return len(seen) >= 3 })
	stop()
	mu.Lock()
	defer mu.Unlock()
	if seen[0] == seen[1] || r.held() != 0 || len(get(t, rec)) != 0 {
		t.Fatalf("seen %v; router holds %d; record %v", seen, r.held(), get(t, rec))
	}
}

// The record is written to disk before the router is asked: here the
// router itself reads the file and checks that the nonce it is asked
// about is there.
func TestTheMappingIsRecordedBeforeTheRouterIsAsked(t *testing.T) {
	rec := record(t)
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, maps: map[string]uint16{}}
	var unrecorded atomic.Int32
	gw := newFakeUDP(t, func(q []byte) []byte {
		found := false
		ms, _ := rec.Load()
		for _, m := range ms {
			if len(q) >= 36 && string(m.Nonce[:]) == string(q[24:36]) {
				found = true
			}
		}
		if !found && binary.BigEndian.Uint32(q[4:]) != 0 {
			unrecorded.Add(1)
		}
		return r.answer(q)
	})
	k := NewKeeper(client(gw), 7423, rec, nil)
	stop := runKeeper(k)
	waitFor(t, "the mapping", func() bool { return k.State().Mapping != nil })
	if held := get(t, rec); len(held) != 1 || held[0].External.String() != "203.0.113.7:40001" {
		t.Fatalf("recorded %+v", held)
	}
	// A file's mode protects it on Unix; on Windows the home's access
	// control list does (D47), and Go reports no such mode there.
	if fi, err := os.Stat(rec.Path); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("record mode %v %v", fi, err)
	}
	stop()
	if unrecorded.Load() != 0 || r.held() != 0 || len(get(t, rec)) != 0 {
		t.Fatalf("asked before recording %d times; router holds %d; record %v", unrecorded.Load(), r.held(), get(t, rec))
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(rec.Path), "*")); len(left) != 0 {
		t.Fatalf("files left %v", left)
	}
}

// noteTo records each request in f as a Keeper does, and nothing after:
// the process dies once the router has answered.
func noteTo(f *File) Note {
	var held []Mapping
	return func(m Mapping, asking bool) error {
		held = slices.DeleteFunc(held, func(x Mapping) bool { return x.Key() == m.Key() })
		if asking {
			held = append(held, m)
		}
		return f.Save(held)
	}
}

// A crash right after the router made the mapping, for each protocol:
// the record on disk holds only what was written before the request, and
// the next run reads it back from the file and removes the mapping. For
// UPnP the external port (here 7424, as 7423 was taken) is in the record
// although no external address was learned.
func TestACrashAfterTheRouterMadeTheMappingIsUndoneFromTheRecordFile(t *testing.T) {
	t.Run("pcp", func(t *testing.T) {
		r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, maps: map[string]uint16{}}
		c := client(newFakeUDP(t, r.answer))
		rec := record(t)
		if _, err := c.Map(context.Background(), 7423, time.Hour, nil, noteTo(rec)); err != nil || r.held() != 1 {
			t.Fatalf("%v, router holds %d", err, r.held())
		}
		runKeeper(NewKeeper(c, 0, rec, nil))()
		if r.held() != 0 || len(get(t, rec)) != 0 {
			t.Fatalf("router holds %d (removed by the nonce kept), record %v", r.held(), get(t, rec))
		}
	})
	t.Run("nat-pmp", func(t *testing.T) {
		var maps atomic.Int32
		c := client(newFakeUDP(t, natpmpRouter([4]byte{198, 51, 100, 9}, 50123, &maps)))
		rec := record(t)
		if _, err := c.Map(context.Background(), 7423, time.Hour, nil, noteTo(rec)); err != nil || maps.Load() != 1 {
			t.Fatalf("%v, router holds %d", err, maps.Load())
		}
		runKeeper(NewKeeper(c, 0, rec, nil))()
		waitFor(t, "the deletion", func() bool { return maps.Load() == 0 })
		if len(get(t, rec)) != 0 {
			t.Fatalf("record %v", get(t, rec))
		}
	})
	t.Run("upnp", func(t *testing.T) {
		g := newFakeIGD(t)
		g.mu.Lock()
		g.taken["7423"] = true
		g.mu.Unlock()
		c := client(noPCP(t))
		c.SSDP = fmt.Sprintf("127.0.0.1:%d", g.ssdp.port())
		rec := record(t)
		var last []byte
		note := noteTo(rec)
		if _, err := c.Map(context.Background(), 7423, time.Hour, nil, func(m Mapping, asking bool) error {
			err := note(m, asking)
			last, _ = os.ReadFile(rec.Path)
			return err
		}); err != nil || !g.has("7424") {
			t.Fatalf("%v", err)
		}
		if !strings.Contains(string(last), `"external_port":7424`) || strings.Contains(string(last), "external_ip") {
			t.Fatalf("record before the add %s", last)
		}
		runKeeper(NewKeeper(c, 0, rec, nil))()
		if g.has("7424") || len(get(t, rec)) != 0 || !slices.Contains(g.called(), "DeletePortMapping") {
			t.Fatalf("calls %v, record %v", g.called(), get(t, rec))
		}
	})
}

// A record is used whole or not at all: an unknown field, a value out of
// range, a mapping on another router, data after the list or a file that
// does not parse, and the keeper sends nothing, keeps the file and says
// what to remove by hand.
func TestARecordThatCannotBeUsedWholeSendsNothingAndIsKept(t *testing.T) {
	ok := `{"protocol":"pcp","gateway":"127.0.0.1","internal_port":7423,"external_port":7423,"lifetime_s":0,"nonce":"010000000000000000000000"}`
	for name, body := range map[string]string{
		"unknown field":  `[{"protocol":"pcp","gateway":"127.0.0.1","internal_port":7423,"external_port":7423,"lifetime_s":0,"nonce":"010000000000000000000000","x":1}]`,
		"port too large": `[{"protocol":"pcp","gateway":"127.0.0.1","internal_port":70000,"external_port":7423,"lifetime_s":0,"nonce":"010000000000000000000000"}]`,
		"no nonce":       `[{"protocol":"pcp","gateway":"127.0.0.1","internal_port":7423,"external_port":7423,"lifetime_s":0}]`,
		"upnp no port":   `[{"protocol":"upnp","gateway":"127.0.0.1","local":"127.0.0.1","internal_port":7423,"external_port":0,"lifetime_s":0,"control":"http://127.0.0.1:1/ctl","service":"urn:schemas-upnp-org:service:WANIPConnection:1"}]`,
		"upnp elsewhere": `[{"protocol":"upnp","gateway":"127.0.0.1","local":"127.0.0.1","internal_port":7423,"external_port":7423,"lifetime_s":0,"control":"http://192.0.2.1:1/ctl","service":"urn:schemas-upnp-org:service:WANIPConnection:1"}]`,
		"other router":   `[{"protocol":"pcp","gateway":"192.0.2.1","internal_port":7423,"external_port":7423,"lifetime_s":0,"nonce":"010000000000000000000000"}]`,
		"trailing data":  `[` + ok + `] []`,
		"not json":       `[{"protocol":`,
	} {
		r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, maps: map[string]uint16{}}
		gw := newFakeUDP(t, r.answer)
		rec := record(t)
		if err := os.WriteFile(rec.Path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := rec.Load(); !errors.Is(err, ErrUnusable) {
			t.Fatalf("%s: loaded: %v", name, err)
		}
		k := NewKeeper(client(gw), 7423, rec, nil)
		stop := runKeeper(k)
		waitFor(t, name, func() bool { return k.State().Err != "" })
		time.Sleep(50 * time.Millisecond)
		stop()
		after, _ := os.ReadFile(rec.Path)
		if st := k.State(); len(gw.requests()) != 0 || string(after) != body ||
			!strings.Contains(st.Err, "nothing was sent to the router") || !strings.Contains(st.Err, rec.Path) {
			t.Fatalf("%s: %d requests sent, file %q, state %+v", name, len(gw.requests()), after, st)
		}
	}
	rec := record(t)
	if err := os.WriteFile(rec.Path, []byte("["+ok+"]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ms, err := rec.Load(); err != nil || len(ms) != 1 || ms[0].Nonce != [12]byte{1} {
		t.Fatalf("a good record: %+v %v", ms, err)
	}
}

// A UPnP forward on the recorded port that points at another machine or
// carries another description is not this machine's: it is left on the
// router, and the record forgets it.
func TestAUPnPForwardThatIsNotThisMachinesIsNotDeleted(t *testing.T) {
	g := newFakeIGD(t)
	c := client(noPCP(t))
	c.SSDP = fmt.Sprintf("127.0.0.1:%d", g.ssdp.port())
	m, err := c.Map(context.Background(), 7423, time.Hour, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []igdEntry{{client: "127.0.0.9", port: "7423", desc: description}, {client: "127.0.0.1", port: "7423", desc: "game console"},
		{client: "127.0.0.1", port: "22", desc: description}} {
		g.mu.Lock()
		g.ports["7423"] = e
		g.mu.Unlock()
		if err := c.Unmap(context.Background(), m); !errors.Is(err, ErrNotOurs) || !g.has("7423") {
			t.Fatalf("%+v: %v", e, err)
		}
	}
	rec := record(t)
	if err := rec.Save([]Mapping{m}); err != nil {
		t.Fatal(err)
	}
	runKeeper(NewKeeper(c, 0, rec, nil))()
	if !g.has("7423") || len(get(t, rec)) != 0 || slices.Contains(g.called(), "DeletePortMapping") {
		t.Fatalf("calls %v, record %v", g.called(), get(t, rec))
	}
}

// A fault body that is not a complete SOAP Fault with a UPnP error code is
// not a refusal: a cut-short 718 does not move to the next port and
// leaves the request recorded, to be removed; a cut-short 714 on removal
// is not taken for "already gone".
func TestAMalformedUPnPFaultDrivesNoFallbackAndNoCleanup(t *testing.T) {
	g := newFakeIGD(t)
	g.mu.Lock()
	g.taken["7423"], g.badFault = true, true
	g.mu.Unlock()
	c := client(noPCP(t))
	c.SSDP = fmt.Sprintf("127.0.0.1:%d", g.ssdp.port())
	rec := record(t)
	_, err := c.Map(context.Background(), 7423, time.Hour, nil, noteTo(rec))
	if err == nil || !strings.Contains(err.Error(), "not a complete UPnP fault") {
		t.Fatalf("map: %v", err)
	}
	adds := 0
	for _, call := range g.called() {
		if call == "AddPortMapping" {
			adds++
		}
	}
	if held := get(t, rec); adds != 1 || len(held) != 1 || held[0].External.Port() != 7423 {
		t.Fatalf("%d adds, record %+v", adds, held)
	}
	g.mu.Lock()
	g.taken, g.badFault = map[string]bool{}, false
	g.mu.Unlock()
	m, err := c.Map(context.Background(), 7423, time.Hour, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	delete(g.ports, "7423") // gone, but the router says so badly
	g.badFault = true
	g.mu.Unlock()
	if err := c.Unmap(context.Background(), m); err == nil {
		t.Fatal("a malformed 714 was taken for a removal")
	}
}

// A failed renewal keeps the mapping held and shown, but once its lease
// has run out it is no longer live (not advertised); a removal that fails
// is reported, kept in the record file and done by the next run, which
// here only cleans up. Against a PCP router on loopback.
func TestAFailedRenewalKeepsTheMappingAndAFailedRemovalIsReportedAndRetried(t *testing.T) {
	defer func(d, w time.Duration) { retryAfter, minWait = d, w }(retryAfter, minWait)
	retryAfter, minWait = 10*time.Millisecond, time.Millisecond
	// A four-second lease, renewed at two: the failed renewal (PCP and
	// UPnP each unanswered, 3*testTry apiece) ends well inside it.
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, grant: 4, maps: map[string]uint16{}}
	c := client(newFakeUDP(t, r.answer))
	rec := record(t)
	k := NewKeeper(c, 7423, rec, nil)
	stop := runKeeper(k)
	waitFor(t, "the mapping", func() bool { return k.State().Mapping != nil })
	r.set(func(r *pcpRouter) { r.silentMap = true })
	waitFor(t, "a failed renewal", func() bool { return k.State().Err != "" })
	st := k.State()
	if st.Mapping == nil || !st.Mapping.Live(time.Now()) {
		t.Fatalf("after a failed renewal %+v", st)
	}
	waitFor(t, "the lease to run out", func() bool {
		st := k.State()
		return st.Mapping != nil && !st.Mapping.Live(time.Now())
	})
	r.set(func(r *pcpRouter) { r.silentDel = true })
	stop()
	if st := k.State(); len(st.Left) != 1 || !strings.Contains(st.Left[0], "could not remove it: no answer") || len(get(t, rec)) != 1 {
		t.Fatalf("left %v, record %v", st.Left, get(t, rec))
	}
	r.set(func(r *pcpRouter) { r.silentDel = false })
	clean := NewKeeper(c, 0, rec, nil) // the next run, with or without --map-port
	runKeeper(clean)()
	if r.held() != 0 || len(get(t, rec)) != 0 || len(clean.State().Left) != 0 {
		t.Fatalf("router holds %d, record %v, left %v", r.held(), get(t, rec), clean.State().Left)
	}
}

// Answers that look like success but are not: a PCP grant of no lifetime
// or no port, a version-0 datagram that is not NAT-PMP's version refusal,
// an SSDP answer that is not 200 or not for a gateway, and a SOAP 200 that
// is cut short. None counts as a mapping.
func TestMalformedRouterAnswersAreNotTakenForAMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(q []byte) []byte
	}{
		{"zero lifetime", func(q []byte) []byte {
			b := (&pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, maps: map[string]uint16{}}).answer(q)
			if b != nil {
				binary.BigEndian.PutUint32(b[4:], 0)
			}
			return b
		}},
		{"zero port", (&pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 0, maps: map[string]uint16{}}).answer},
	} {
		rec := record(t)
		gw := newFakeUDP(t, tc.answer)
		k := NewKeeper(client(gw), 7423, rec, nil)
		stop := runKeeper(k)
		waitFor(t, tc.name, func() bool { return k.State().Err != "" })
		if st := k.State(); st.Mapping != nil || !strings.Contains(st.Err, "cannot be used") {
			t.Fatalf("%s: %+v", tc.name, st)
		}
		// Kept to be removed, and removed after the failed attempt: a
		// deletion (lifetime 0) by the request's nonce.
		waitFor(t, tc.name+" removed", func() bool {
			for _, q := range gw.requests() {
				if len(q) >= 60 && binary.BigEndian.Uint32(q[4:]) == 0 && string(q[24:36]) == string(gw.requests()[0][24:36]) {
					return len(get(t, rec)) == 0
				}
			}
			return false
		})
		stop()
	}
	junk := newFakeUDP(t, func(q []byte) []byte { return []byte{0, 0x99, 0, 0, 0, 0, 0, 0} })
	if _, err := client(junk).Map(context.Background(), 7423, time.Hour, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "PCP: no answer") {
		t.Fatalf("junk version 0: %v", err)
	}
	for name, reply := range map[string]string{
		"not 200":     "HTTP/1.1 404 Not Found\r\nST: " + igdDevice + "\r\nLOCATION: %s\r\n\r\n",
		"not gateway": "HTTP/1.1 200 OK\r\nST: urn:schemas-upnp-org:device:MediaServer:1\r\nLOCATION: %s\r\n\r\n",
	} {
		g := newFakeIGD(t)
		g.mu.Lock()
		g.reply = reply
		g.mu.Unlock()
		c := client(noPCP(t))
		c.SSDP = fmt.Sprintf("127.0.0.1:%d", g.ssdp.port())
		if _, err := c.Map(context.Background(), 7423, time.Hour, nil, nil); err == nil || len(g.called()) != 0 {
			t.Fatalf("%s: %v, calls %v", name, err, g.called())
		}
	}
	g := newFakeIGD(t)
	g.mu.Lock()
	g.cut = true
	g.mu.Unlock()
	c := client(noPCP(t))
	c.SSDP = fmt.Sprintf("127.0.0.1:%d", g.ssdp.port())
	if _, err := c.Map(context.Background(), 7423, time.Hour, nil, nil); err == nil || !strings.Contains(err.Error(), "not a complete") {
		t.Fatalf("cut short: %v", err)
	}
}

// UPnP asks for a finite lease first and a permanent one only when the
// router refuses finite ones (725); deleting an entry the router no
// longer has counts as removed (714).
func TestUPnPAsksForAPermanentLeaseOnlyWhenRefusedAFiniteOne(t *testing.T) {
	g := newFakeIGD(t)
	c := client(noPCP(t))
	c.SSDP = fmt.Sprintf("127.0.0.1:%d", g.ssdp.port())
	m, err := c.Map(context.Background(), 7423, time.Hour, nil, nil)
	if err != nil || m.Lifetime != time.Hour || g.leasesAsked()[0] != "3600" {
		t.Fatalf("finite: %+v %v %v", m, err, g.leasesAsked())
	}
	g.mu.Lock()
	g.onlyPermanent = true
	g.leases = nil
	g.mu.Unlock()
	m, err = c.Map(context.Background(), 7425, time.Hour, nil, nil)
	if err != nil || m.Lifetime != 0 || !slices.Equal(g.leasesAsked(), []string{"3600", "0"}) {
		t.Fatalf("permanent: %+v %v %v", m, err, g.leasesAsked())
	}
	if err := c.Unmap(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err := c.Unmap(context.Background(), m); err != nil { // already gone: 714
		t.Fatalf("second removal: %v", err)
	}
}
