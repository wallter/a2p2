package node

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// fakeNet is where keys listen, as the responder's handshaker sees it:
// address to key. It records every handshake asked of it.
type fakeNet struct {
	mu    sync.Mutex
	at    map[string]string
	tries [][2]string // addr, key expected
	block chan struct{}
}

func (f *fakeNet) Handshake(ctx context.Context, addr, key string) bool {
	f.mu.Lock()
	f.tries = append(f.tries, [2]string{addr, key})
	got, block := f.at[addr], f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return false
		}
	}
	return got == key
}

func (f *fakeNet) tried() [][2]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tries)
}

// dialBackRoom is a room of a and b (c is not in it), with a answering
// dial-backs over fn. a's book holds b at 198.51.100.2 and .3 (b listed
// both).
func dialBackRoom(t *testing.T) (a, b, c *lnode, fn *fakeNet, srv *DialBackServer) {
	clock := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	a, b, c = newLNode(t, "a", &clock), newLNode(t, "b", &clock), newLNode(t, "c", &clock)
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
	fn = &fakeNet{at: map[string]string{}}
	old := DialBackTimeout
	DialBackTimeout = 50 * time.Millisecond
	t.Cleanup(func() { DialBackTimeout = old })
	book := NewAddrBook()
	book.Replace(b.id, []string{"198.51.100.2:7423", "198.51.100.3:7423"}, clock)
	return a, b, c, fn, NewDialBackServer(a.r, fn, book)
}

// memDialBack carries a dial-back as the HTTP route does: encoded,
// decoded strictly on both sides, the asker's key and source address as
// the transport saw them.
type memDialBack struct {
	srv    *DialBackServer
	from   string
	source string
}

func (m memDialBack) DialBack(ctx context.Context, peer string, addrs []string) (*DialBackResponse, error) {
	b, _ := json.Marshal(DialBackRequest{V: PullVersion, Addrs: addrs})
	q, perr := DecodeDialBackRequest(b)
	if perr != nil {
		return nil, perr
	}
	resp, perr := m.srv.DialBack(ctx, m.from, m.source, q)
	if perr != nil {
		return nil, perr
	}
	out, _ := json.Marshal(resp)
	return DecodeDialBackResponse(out, addrs)
}

func TestDialBackReachesOnlyTheAskersOwnKeyAndReportsTheSourceSeen(t *testing.T) {
	_, b, c, fn, srv := dialBackRoom(t)
	fn.at["198.51.100.2:7423"] = b.id
	fn.at["198.51.100.3:7423"] = c.id // another machine's listener: b listed it, but c answers
	got, err := memDialBack{srv, b.id, "203.0.113.9:50123"}.DialBack(context.Background(), "",
		[]string{"198.51.100.2:7423", "198.51.100.3:7423", "203.0.113.9:7423"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Reached, []string{"198.51.100.2:7423"}) || got.Saw != "203.0.113.9" || len(got.NotTried) != 0 {
		t.Fatalf("answer %+v", got)
	}
	tries := fn.tried()
	if len(tries) != 3 {
		t.Fatalf("tries %v", tries)
	}
	for _, tr := range tries {
		if tr[1] != b.id {
			t.Fatalf("dialled %s expecting %s, not the asker's key", tr[0], core.ShortNode(tr[1]))
		}
	}
}

// How long the answer takes tells nothing of a third party: an answer in
// which any address was not reached comes at the deadline, however fast
// that address refused.
func TestDialBackAnswersAtTheDeadlineUnlessEveryAddressWasReached(t *testing.T) {
	_, b, _, fn, srv := dialBackRoom(t)
	fn.at["198.51.100.2:7423"] = b.id
	ask := func(addrs ...string) time.Duration {
		start := time.Now()
		if _, perr := srv.DialBack(context.Background(), b.id, "203.0.113.9:1",
			&DialBackRequest{V: PullVersion, Addrs: addrs}); perr != nil {
			t.Fatal(perr)
		}
		return time.Since(start)
	}
	if d := ask("198.51.100.3:7423"); d < DialBackTimeout {
		t.Fatalf("a refused third party answered in %s, before the %s deadline", d, DialBackTimeout)
	}
	if d := ask("198.51.100.2:7423", "198.51.100.3:7423"); d < DialBackTimeout {
		t.Fatalf("one of two reached: answered in %s", d)
	}
	if d := ask("198.51.100.2:7423"); d >= DialBackTimeout {
		t.Fatalf("all reached, yet answered at the deadline (%s)", d)
	}
}

// A member can make the responder dial only its own source address (any
// port) or an address the responder already holds for its key; any other
// endpoint, a third party's included, is answered as not tried, at once,
// with nothing dialled.
func TestADialBackDialsOnlyTheAskersSourceAddressOrWhatTheBookHoldsForIt(t *testing.T) {
	_, b, _, fn, srv := dialBackRoom(t)
	asked := []string{"192.0.2.50:22", "198.51.100.4:7423", "[2001:db8::9]:443", "198.51.100.2:7423", "203.0.113.9:9"}
	start := time.Now()
	got, err := memDialBack{srv, b.id, "203.0.113.9:50123"}.DialBack(context.Background(), "", asked)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.NotTried, asked[:3]) || len(got.Reached) != 0 {
		t.Fatalf("answer %+v", got)
	}
	var dialled []string
	for _, tr := range fn.tried() {
		dialled = append(dialled, tr[0])
	}
	if !slices.Equal(dialled, []string{"198.51.100.2:7423", "203.0.113.9:9"}) && !slices.Equal(dialled, []string{"203.0.113.9:9", "198.51.100.2:7423"}) {
		t.Fatalf("dialled %v", dialled)
	}
	// Only third parties: nothing dialled, and no reason to wait.
	if got, err := (memDialBack{srv, b.id, "203.0.113.9:50123"}).DialBack(context.Background(), "",
		[]string{"192.0.2.50:22"}); err != nil || !slices.Equal(got.NotTried, []string{"192.0.2.50:22"}) {
		t.Fatalf("third party only: %+v %v", got, err)
	}
	if d := time.Since(start); d > 10*DialBackTimeout {
		t.Fatalf("took %s", d)
	}
	// The same endpoint twice, once written as IPv4-mapped IPv6, is a repeat.
	if _, perr := DecodeDialBackRequest([]byte(`{"v":2,"addrs":["198.51.100.2:7423","[::ffff:198.51.100.2]:7423"]}`)); perr == nil {
		t.Fatal("accepted a repeat written two ways")
	}
}

// What a member says it saw is an IP address literal or nothing: control
// characters, names, zones and ports are refused before anything stores
// or prints it; not-tried and reached addresses must be ones asked, and
// not both.
func TestADialBackAnswerSaysOnlyAPlainIPAddressSeen(t *testing.T) {
	asked := []string{"192.0.2.1:1", "192.0.2.1:2"}
	for _, body := range []string{
		`{"v":2,"saw":"\u001b[31m203.0.113.9"}`,
		`{"v":2,"saw":"example.com"}`,
		`{"v":2,"saw":"fe80::1%en0"}`,
		`{"v":2,"saw":"203.0.113.9:7423"}`,
		`{"v":2,"not_tried":["192.0.2.9:1"]}`,
		`{"v":2,"reached":["192.0.2.1:1"],"not_tried":["192.0.2.1:1"]}`,
	} {
		if _, err := DecodeDialBackResponse([]byte(body), asked); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	got, err := DecodeDialBackResponse([]byte(`{"v":2,"saw":"::ffff:203.0.113.9","not_tried":["192.0.2.1:2"]}`), asked)
	if err != nil || got.Saw != "203.0.113.9" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestDialBackIsRefusedToAMachineThatSharesNoRoomAndDialsNothing(t *testing.T) {
	a, _, c, fn, srv := dialBackRoom(t)
	for _, from := range []string{c.id, a.id, "not a key"} {
		q := &DialBackRequest{V: PullVersion, Addrs: []string{"198.51.100.3:7423"}}
		if _, perr := srv.DialBack(context.Background(), from, "203.0.113.9:1", q); perr == nil || perr.Status != 403 ||
			perr.Code != CodeNotMember {
			t.Fatalf("from %s: %v", core.ShortNode(from), perr)
		}
	}
	if n := len(fn.tried()); n != 0 {
		t.Fatalf("dialled %d times for non-members", n)
	}
}

func TestDialBackRequestsAreStrictAndNameNoHosts(t *testing.T) {
	nine := `"198.51.100.1:1","198.51.100.1:2","198.51.100.1:3","198.51.100.1:4","198.51.100.1:5",` +
		`"198.51.100.1:6","198.51.100.1:7","198.51.100.1:8","198.51.100.1:9"`
	for _, body := range []string{
		`{"v":2,"addrs":["example.com:7423"]}`, // a name: the responder never looks one up
		`{"v":2,"addrs":["[fe80::1%en0]:7423"]}`,
		`{"v":2,"addrs":["198.51.100.1:0"]}`,
		`{"v":2,"addrs":["198.51.100.1:07423"]}`,
		`{"v":2,"addrs":["198.51.100.1:7423","198.51.100.1:7423"]}`,
		`{"v":2,"addrs":[` + nine + `]}`,
		`{"v":2,"addrs":[]}`,
		`{"v":3,"addrs":["198.51.100.1:7423"]}`,
		`{"v":2,"addrs":["198.51.100.1:7423"],"v":2}`,
		`{"v":2,"Addrs":["198.51.100.1:7423"]}`,
		`{"v":2,"addrs":null}`,
	} {
		if _, perr := DecodeDialBackRequest([]byte(body)); perr == nil || perr.Status != 400 {
			t.Errorf("accepted %s", body)
		}
	}
	if q, perr := DecodeDialBackRequest([]byte(`{"v":2,"addrs":["198.51.100.1:7423","[2001:db8::1]:7423"],"x":1}`)); perr != nil ||
		len(q.Addrs) != 2 {
		t.Fatalf("refused a good request: %v", perr)
	}
	// A response may only name addresses that were asked.
	if _, err := DecodeDialBackResponse([]byte(`{"v":2,"reached":["192.0.2.1:1"]}`), []string{"192.0.2.1:2"}); err == nil {
		t.Fatal("accepted a reached address that was not asked")
	}
}

func TestDialBackNeverDialsLoopbackOrSpecialAddressesForARemoteAsker(t *testing.T) {
	_, b, _, fn, srv := dialBackRoom(t)
	special := []string{"127.0.0.1:7423", "[::1]:7423", "0.0.0.0:7423", "224.0.0.1:7423", "169.254.1.1:7423",
		"255.255.255.255:7423", "[ff02::1]:7423", "[fe80::1]:7423"}
	q := &DialBackRequest{V: PullVersion, Addrs: special}
	if _, perr := srv.DialBack(context.Background(), b.id, "203.0.113.9:1", q); perr != nil {
		t.Fatal(perr)
	}
	if tr := fn.tried(); len(tr) != 0 {
		t.Fatalf("dialled %v for a remote asker", tr)
	}
	// An asker on this machine itself (it connected over loopback) may be
	// dialled on loopback: that reaches nothing it could not reach.
	fn.at["127.0.0.1:7423"] = b.id
	resp, perr := srv.DialBack(context.Background(), b.id, "127.0.0.1:50000",
		&DialBackRequest{V: PullVersion, Addrs: []string{"127.0.0.1:7423"}})
	if perr != nil || !slices.Equal(resp.Reached, []string{"127.0.0.1:7423"}) {
		t.Fatalf("loopback asker: %+v %v", resp, perr)
	}
}

func TestDialBackIsRateLimitedPerPeerAndForTheWholeMachine(t *testing.T) {
	a, b, _, fn, srv := dialBackRoom(t)
	d := newLNode(t, "d", a.clock)
	room := a.r.OtherMembers() // just b so far
	if !slices.Equal(room, []string{b.id}) {
		t.Fatalf("other members %v", room)
	}
	rooms, _ := a.r.List(context.Background())
	for _, r := range rooms {
		if r.Label == "team" {
			a.change(r.ID, Change{Kind: core.EntryAdd, Target: d.id, Name: "dee", History: true})
		}
	}
	srv.book.(*AddrBook).Replace(d.id, []string{"198.51.100.2:7423"}, *a.clock)
	defer func(v float64) { dialBackAllBurst = v }(dialBackAllBurst)
	dialBackAllBurst = 6
	ask := func(from string) *PullError {
		_, perr := srv.DialBack(context.Background(), from, "203.0.113.9:1",
			&DialBackRequest{V: PullVersion, Addrs: []string{"198.51.100.2:7423"}})
		return perr
	}
	for i := range 4 {
		if perr := ask(b.id); perr != nil {
			t.Fatalf("b's request %d: %v", i+1, perr)
		}
	}
	perr := ask(b.id)
	if perr == nil || perr.Status != 429 || perr.RetryAfter < 9*time.Minute {
		t.Fatalf("b's fifth request: %+v", perr)
	}
	// b over its own limit spent nothing of the machine's: d gets the two left.
	for i := range 2 {
		if perr := ask(d.id); perr != nil {
			t.Fatalf("d's request %d: %v", i+1, perr)
		}
	}
	if perr := ask(d.id); perr == nil || perr.Status != 429 {
		t.Fatalf("over the machine's limit: %v", perr)
	}
	if n := len(fn.tried()); n != 6 {
		t.Fatalf("dialled %d times for 6 requests allowed", n)
	}
	// Ten minutes give b one more, and the machine one more.
	*a.clock = a.clock.Add(dialBackPeerEvery)
	if perr := ask(b.id); perr != nil {
		t.Fatalf("after ten minutes: %v", perr)
	}
}

func TestDialBackServesAtMostFourRequestsAtOnce(t *testing.T) {
	a, b, _, fn, srv := dialBackRoom(t)
	DialBackTimeout = 10 * time.Second // the four stay in flight until released
	fn.block = make(chan struct{})
	var wg sync.WaitGroup
	for range dialBackConcurrent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = srv.DialBack(context.Background(), b.id, "203.0.113.9:1",
				&DialBackRequest{V: PullVersion, Addrs: []string{"198.51.100.2:7423"}})
		}()
	}
	for deadline := time.Now().Add(5 * time.Second); len(fn.tried()) < dialBackConcurrent; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("requests did not start")
		}
	}
	*a.clock = a.clock.Add(time.Hour) // tokens are not what stops the next one
	_, perr := srv.DialBack(context.Background(), b.id, "203.0.113.9:1",
		&DialBackRequest{V: PullVersion, Addrs: []string{"198.51.100.2:7423"}})
	close(fn.block)
	wg.Wait()
	if perr == nil || perr.Status != 429 {
		t.Fatalf("a fifth request at once: %v", perr)
	}
}

// fakeDialBack answers per peer: a response or an error; it counts asks.
type fakeDialBack struct {
	mu    sync.Mutex
	resp  map[string]*DialBackResponse
	err   map[string]error
	asked []string
}

func (f *fakeDialBack) DialBack(_ context.Context, peer string, addrs []string) (*DialBackResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, peer)
	if e := f.err[peer]; e != nil {
		return nil, e
	}
	if r := f.resp[peer]; r != nil {
		return r, nil
	}
	return &DialBackResponse{V: PullVersion}, nil
}

func TestReachIsYesNoOrUnknownFromTheAnswersAndAsksAPeerAtMostHourly(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	f := &fakeDialBack{resp: map[string]*DialBackResponse{}, err: map[string]error{}}
	r := NewReacher(f, ReachOpts{Members: func() []string { return []string{"p1", "p2"} },
		Addrs: func() []string { return []string{"198.51.100.2:7423"} }, Now: func() time.Time { return now }})
	if got := r.Get(); got.State != ReachUnknown {
		t.Fatalf("before any check: %+v", got)
	}
	// Nobody answers: unknown, with why.
	f.err["p1"], f.err["p2"] = pullErr(404, CodeUnavailable, ""), errors.New("refused")
	if got := r.Check(context.Background(), []string{"p1", "p2"}, false); got.State != ReachUnknown ||
		!strings.Contains(got.Why, "without the dial-back") {
		t.Fatalf("no answers: %+v", got)
	}
	// Within the hour, an automatic check asks nobody again.
	f.err = map[string]error{}
	f.resp["p1"] = &DialBackResponse{V: PullVersion, Reached: []string{"198.51.100.2:7423"}, Saw: "203.0.113.9"}
	asked := len(f.asked)
	r.Check(context.Background(), []string{"p1", "p2"}, false)
	if len(f.asked) != asked {
		t.Fatalf("asked again within the hour: %v", f.asked[asked:])
	}
	// A person's check asks at once: yes.
	got := r.Check(context.Background(), []string{"p1", "p2"}, true)
	if got.State != ReachYes || got.Checked == "" || !slices.Equal(got.Reached, []string{"198.51.100.2:7423"}) ||
		!slices.Equal(got.Saw, []string{"203.0.113.9"}) {
		t.Fatalf("reached: %+v", got)
	}
	// A later check nobody answers does not take back what is known.
	now = now.Add(ReachEvery)
	f.err["p1"], f.err["p2"] = errors.New("down"), errors.New("down")
	if got := r.Check(context.Background(), []string{"p1", "p2"}, false); got.State != ReachYes {
		t.Fatalf("an unanswered check replaced yes: %+v", got)
	}
	// Members that answer and reach nothing: no.
	now = now.Add(ReachEvery)
	f.err = map[string]error{}
	f.resp = map[string]*DialBackResponse{}
	if got := r.Check(context.Background(), []string{"p1", "p2"}, false); got.State != ReachNo || len(got.By) != 2 {
		t.Fatalf("not reached: %+v", got)
	}
}

func TestNetCheckTriesEachMemberAndAsksThoseReachedToDialBack(t *testing.T) {
	a, b, c, _, _ := dialBackRoom(t)
	rooms, _ := a.r.List(context.Background())
	for _, r := range rooms {
		if r.Label == "team" {
			a.change(r.ID, Change{Kind: core.EntryAdd, Target: c.id, Name: "cee", History: true})
		}
	}
	book := NewAddrBook()
	book.Replace(b.id, []string{"198.51.100.2:7423", "198.51.100.20:7423"}, *a.clock)
	book.Replace(c.id, []string{"198.51.100.3:7423"}, *a.clock)
	pr := fakeProber{b.id + " 198.51.100.2:7423": ProbeReached, b.id + " 198.51.100.20:7423": ProbeTimeout,
		c.id + " 198.51.100.3:7423": ProbeRefused}
	f := &fakeDialBack{resp: map[string]*DialBackResponse{b.id: {V: PullVersion, Reached: []string{"198.51.100.1:7423"}}}}
	reach := NewReacher(f, ReachOpts{Members: a.r.OtherMembers, Book: book,
		Addrs: func() []string { return []string{"198.51.100.1:7423"} }})
	s, err := OpenSurface(context.Background(), a.m, a.r, SurfaceOpts{Name: "a", Book: book,
		Addrs: func() []string { return []string{"198.51.100.1:7423"} }, Network: Network{State: NetworkOn,
			Listen: "0.0.0.0:7423"}, Probe: pr, Reach: reach})
	if err != nil {
		t.Fatal(err)
	}
	nc, err := s.NetCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !nc.Listening || len(nc.Members) != 2 {
		t.Fatalf("check %+v", nc)
	}
	for _, m := range nc.Members {
		switch m.Node {
		case b.id:
			if !m.Reached || len(m.Addrs) != 2 || m.Addrs[0].Result != ProbeReached || m.Addrs[1].Result != ProbeTimeout {
				t.Fatalf("b: %+v", m)
			}
		case c.id:
			if m.Reached || len(m.Addrs) != 1 || m.Addrs[0].Result != ProbeRefused {
				t.Fatalf("c: %+v", m)
			}
		}
	}
	// Only the member reached was asked, and its answer is the status's.
	if !slices.Equal(f.asked, []string{b.id}) || nc.Reach.State != ReachYes || s.ReachNow().State != ReachYes {
		t.Fatalf("asked %v, reach %+v", f.asked, nc.Reach)
	}
	if !strings.Contains(nc.Advice, "reaches 1 of 2 members and they can reach it") {
		t.Fatalf("advice %q", nc.Advice)
	}
}

func TestNetCheckWithNetworkingOffTriesNothing(t *testing.T) {
	a, b, _, _, _ := dialBackRoom(t)
	book := NewAddrBook()
	book.Replace(b.id, []string{"198.51.100.2:7423"}, *a.clock)
	pr := fakeProber{}
	s, err := OpenSurface(context.Background(), a.m, a.r, SurfaceOpts{Name: "a", Book: book, Probe: pr})
	if err != nil {
		t.Fatal(err)
	}
	nc, err := s.NetCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pr) != 0 || len(nc.Members) != 0 || nc.Reach.State != ReachNo || !strings.HasPrefix(nc.Advice, "Networking is off") {
		t.Fatalf("off: %+v", nc)
	}
}

// fakeProber answers "node addr" with a result; a missing pair is a test
// failure, recorded by adding the key.
type fakeProber map[string]string

func (f fakeProber) Probe(_ context.Context, node, addr string) AddrCheck {
	r, ok := f[node+" "+addr]
	if !ok {
		panic("probed " + addr + " unexpectedly")
	}
	return AddrCheck{Addr: addr, Result: r, MS: 1}
}

func TestNetCheckAdviceSaysWhatToDoInEachSituation(t *testing.T) {
	m := func(room string, reached bool) MemberCheck {
		return MemberCheck{RoomLabel: room, Node: room + "-node", Reached: reached}
	}
	for _, c := range []struct {
		nc   NetCheck
		want string
	}{
		{NetCheck{State: NetworkOff}, "Networking is off"},
		{NetCheck{State: NetworkOn, Fault: "address in use"}, "could not listen (address in use)"},
		{NetCheck{State: NetworkOn, Listening: true}, "No other machine"},
		{NetCheck{State: NetworkOutbound, Members: []MemberCheck{m("team", false)}}, "No member could be reached"},
		{NetCheck{State: NetworkOutbound, Members: []MemberCheck{m("team", true), m("ops", false)}},
			"connects out only, which works while one member of each room accepts connections; 1 of 2 members were reached, none in ops"},
		{NetCheck{State: NetworkOn, Listening: true, Listen: "0.0.0.0:7423", Members: []MemberCheck{m("team", true)},
			Reach: Reach{State: ReachNo}}, "run it as outbound-only (a2p2 net on --outbound-only) and make sure at least one member of each room is reachable, or let connections in to port 7423"},
		{NetCheck{State: NetworkOn, Listening: true, Members: []MemberCheck{m("team", true)},
			Reach: Reach{State: ReachUnknown, Why: "x"}}, "none could test connecting back (x)"},
		{NetCheck{State: NetworkOn, Listening: true, Members: []MemberCheck{m("team", true)},
			Reach: Reach{State: ReachYes}}, "nothing to do"},
	} {
		if got := Advice(c.nc); !strings.Contains(got, c.want) {
			t.Errorf("%+v: %q, want %q", c.nc, got, c.want)
		}
	}
}
