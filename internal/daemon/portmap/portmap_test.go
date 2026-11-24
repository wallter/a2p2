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
	gw.delay = 45 * time.Millisecond // after the first wait (30 ms), within the second (60 ms)
	gw.mu.Unlock()
	c := client(gw)
	if m, err := c.Map(context.Background(), 7423, time.Hour, nil, nil); err != nil || m.Protocol != PCP {
		t.Fatalf("late: %+v %v", m, err)
	}
	gw.mu.Lock()
	gw.delay = 500 * time.Millisecond // after every try
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
	ports    map[string]bool
	taken    map[string]bool
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

func newFakeIGD(t *testing.T) *fakeIGD {
	g := &fakeIGD{ports: map[string]bool{}, taken: map[string]bool{}}
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
	switch action {
	case "AddPortMapping":
		p := field("NewExternalPort")
		g.leases = append(g.leases, field("NewLeaseDuration"))
		if g.onlyPermanent && field("NewLeaseDuration") != "0" {
			w.WriteHeader(500)
			fmt.Fprint(w, `<s:Envelope><s:Body><s:Fault><detail><UPnPError><errorCode>725</errorCode>`+
				`</UPnPError></detail></s:Fault></s:Body></s:Envelope>`)
			return
		}
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
		if !g.ports[field("NewExternalPort")] {
			w.WriteHeader(500)
			fmt.Fprint(w, `<s:Envelope><s:Body><s:Fault><detail><UPnPError><errorCode>714</errorCode>`+
				`</UPnPError></detail></s:Fault></s:Body></s:Envelope>`)
			return
		}
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

// lossy is a Mapper whose router forgets each mapping: every renewal
// comes back on another external port, with a lifetime far under the old
// 30-second floor.
type lossy struct {
	mu     sync.Mutex
	next   uint16
	unmaps []Mapping
}

func (l *lossy) Map(_ context.Context, internal uint16, life time.Duration, _ *Mapping, note Note) (Mapping, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	m := Mapping{Protocol: UPnP, Control: "http://127.0.0.1:1/ctl", Internal: internal, Lifetime: 40 * time.Millisecond,
		External: netip.AddrPortFrom(netip.MustParseAddr("203.0.113.7"), 40000+l.next)}
	_ = note(m, true)
	return m, nil
}

func (l *lossy) Unmap(_ context.Context, m Mapping) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.unmaps = append(l.unmaps, m)
	return nil
}

func (l *lossy) removed() []Mapping {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.unmaps)
}

// memRecord is the on-disk record, in memory; saved lets a test see what
// was written when.
type memRecord struct {
	mu    sync.Mutex
	held  []Mapping
	saves int
}

func (r *memRecord) Load() ([]Mapping, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.held), nil
}

func (r *memRecord) Save(m []Mapping) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.held = slices.Clone(m)
	r.saves++
	return nil
}

func (r *memRecord) get() []Mapping {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.held)
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
// keeper takes it, removes the one it replaced, renews well before a
// 40-millisecond lifetime ends, and removes the last at the end.
func TestAKeeperFollowsARouterThatLosesTheMappingAndRemovesItAtTheEnd(t *testing.T) {
	defer func(d time.Duration) { minWait = d }(minWait)
	minWait = time.Millisecond
	l := &lossy{}
	var mu sync.Mutex
	var seen []string
	k := NewKeeper(l, 7423, &memRecord{}, func(st State) {
		mu.Lock()
		defer mu.Unlock()
		if st.Mapping != nil {
			seen = append(seen, st.Mapping.External.String())
		}
	})
	stop := runKeeper(k)
	waitFor(t, "renewals", func() bool { mu.Lock(); defer mu.Unlock(); return len(seen) >= 3 })
	stop()
	mu.Lock()
	defer mu.Unlock()
	if seen[0] == seen[1] {
		t.Fatalf("the new external port was not taken: %v", seen)
	}
	last := k.State().Mapping
	got := l.removed()
	if len(got) < len(seen) || last == nil || got[len(got)-1].External != last.External {
		t.Fatalf("removed %v of %v, last %v", got, seen, last)
	}
}

// The record is written before the router is asked: here the router
// itself checks that the nonce it is asked about was already recorded.
func TestTheMappingIsRecordedBeforeTheRouterIsAsked(t *testing.T) {
	rec := &memRecord{}
	r := &pcpRouter{extIP: netip.MustParseAddr("203.0.113.7"), extPort: 40001, maps: map[string]uint16{}}
	var unrecorded atomic.Int32
	gw := newFakeUDP(t, func(q []byte) []byte {
		found := false
		for _, m := range rec.get() {
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
	if held := rec.get(); len(held) != 1 || held[0].External.String() != "203.0.113.7:40001" {
		t.Fatalf("recorded %+v", held)
	}
	stop()
	if unrecorded.Load() != 0 || r.held() != 0 || len(rec.get()) != 0 {
		t.Fatalf("asked before recording %d times; router holds %d; record %v", unrecorded.Load(), r.held(), rec.get())
	}
}

// NAT-PMP maps the port, then the external address request fails: the
// mapping exists although Map failed, so it is kept and removed.
func TestAMappingMadeBeforeAFailedAddressLookupIsStillRemoved(t *testing.T) {
	var maps atomic.Int32
	inner := natpmpRouter([4]byte{198, 51, 100, 9}, 50123, &maps)
	gw := newFakeUDP(t, func(q []byte) []byte {
		if len(q) == 2 { // the external address: refused, network failure
			return []byte{0, 128, 0, 3, 0, 0, 0, 1, 0, 0, 0, 0}
		}
		return inner(q)
	})
	rec := &memRecord{}
	k := NewKeeper(client(gw), 7423, rec, nil)
	stop := runKeeper(k)
	waitFor(t, "the failed attempt", func() bool { return k.State().Err != "" })
	if maps.Load() != 1 || len(rec.get()) != 1 || rec.get()[0].Protocol != NATPMP {
		t.Fatalf("router holds %d, record %+v", maps.Load(), rec.get())
	}
	stop()
	if maps.Load() != 0 || len(rec.get()) != 0 {
		t.Fatalf("after the end: router holds %d, record %+v", maps.Load(), rec.get())
	}
}

// flaky grants once, then fails every renewal; Unmap fails while
// failUnmap is set.
type flaky struct {
	mu        sync.Mutex
	maps      int
	failUnmap bool
	unmapped  []Mapping
}

func (f *flaky) Map(_ context.Context, internal uint16, _ time.Duration, prev *Mapping, note Note) (Mapping, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.maps++
	m := Mapping{Protocol: PCP, Gateway: loop, Internal: internal, Nonce: [12]byte{1}, Lifetime: 40 * time.Millisecond,
		Expires: time.Now().Add(40 * time.Millisecond), External: netip.MustParseAddrPort("203.0.113.7:40001")}
	if prev != nil {
		m = *prev
	}
	_ = note(m, true)
	if f.maps > 1 {
		return Mapping{}, errors.New("no answer")
	}
	return m, nil
}

func (f *flaky) Unmap(_ context.Context, m Mapping) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failUnmap {
		return errors.New("no answer")
	}
	f.unmapped = append(f.unmapped, m)
	return nil
}

func (f *flaky) set(fail bool) { f.mu.Lock(); f.failUnmap = fail; f.mu.Unlock() }

func (f *flaky) count() (int, int) { f.mu.Lock(); defer f.mu.Unlock(); return f.maps, len(f.unmapped) }

// A failed renewal keeps the mapping held, shown and removed at the end;
// a removal that fails is reported, kept in the record and done by the
// next run, which here only cleans up.
func TestAFailedRenewalKeepsTheMappingAndAFailedRemovalIsReportedAndRetried(t *testing.T) {
	defer func(d time.Duration) { retryAfter = d }(retryAfter)
	retryAfter = 10 * time.Millisecond
	f := &flaky{}
	rec := &memRecord{}
	k := NewKeeper(f, 7423, rec, nil)
	stop := runKeeper(k)
	waitFor(t, "failed renewals", func() bool { n, _ := f.count(); return n >= 3 })
	if st := k.State(); st.Mapping == nil || st.Err == "" {
		t.Fatalf("after a failed renewal %+v", st)
	}
	f.set(true)
	stop()
	if st := k.State(); len(st.Left) != 1 || !strings.Contains(st.Left[0], "could not remove it: no answer") || len(rec.get()) != 1 {
		t.Fatalf("left %v, record %v", st.Left, rec.get())
	}
	f.set(false)
	clean := NewKeeper(f, 0, rec, nil) // the next run, with or without --map-port
	runKeeper(clean)()
	if _, n := f.count(); n != 1 || len(rec.get()) != 0 || len(clean.State().Left) != 0 {
		t.Fatalf("removed %d, record %v, left %v", n, rec.get(), clean.State().Left)
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
		rec := &memRecord{}
		gw := newFakeUDP(t, tc.answer)
		k := NewKeeper(client(gw), 7423, rec, nil)
		stop := runKeeper(k)
		waitFor(t, tc.name, func() bool { return k.State().Err != "" })
		if st := k.State(); st.Mapping != nil || !strings.Contains(st.Err, "cannot be used") || len(rec.get()) != 1 {
			t.Fatalf("%s: %+v, record %v (kept, to be removed)", tc.name, st, rec.get())
		}
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
