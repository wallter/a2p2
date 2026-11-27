package mcpshim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wallter/a2p2/internal/daemon"
	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// The reader's byte budget on inbox (D70): the daemon returns, oldest
// first, what fits max_bytes of the result the model reads, keeps a
// receipt of that only, and the shim confirms that only.

// page is an inbox result as the model reads it.
type page struct {
	MarkedSeen bool `json:"marked_seen"`
	Messages   []struct {
		ID      string `json:"id"`
		Kind    string `json:"kind"`
		Edited  bool   `json:"edited"`
		Room    string `json:"room_label"`
		Created string `json:"created"`
		Text    string `json:"text"`
		TextCut string `json:"text_cut"`
	} `json:"messages"`
	More   int `json:"more"`
	Rooms  []localapi.ToolRoom
	Levels []localapi.ToolLevel `json:"levels"`
}

// readPage calls inbox with args and checks the result against max (0:
// the default): never over it, as the bytes the model reads.
func readPage(t *testing.T, cs *mcp.ClientSession, args map[string]any) (page, string) {
	t.Helper()
	s := ok(t, cs, "inbox", args)
	max := node.DefaultInboxBytes
	if m, ok := args["max_bytes"].(int); ok && m != 0 {
		max = min(m, node.MaxInboxBytes)
	}
	if len(s) > max {
		t.Fatalf("result is %d bytes, over max_bytes %d", len(s), max)
	}
	var p page
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		t.Fatalf("%v: %s", err, s)
	}
	return p, s
}

// writer is an agent that sends through the local API, faster than tools.
type writer struct {
	t  *testing.T
	c  *localapi.Client
	id string
}

func newWriter(t *testing.T, c *localapi.Client, label string) writer {
	t.Helper()
	var a store.Agent
	if err := c.Call(context.Background(), "register", localapi.RegisterReq{Harness: "test", Label: label, Cwd: "/work/" + label}, &a); err != nil {
		t.Fatal(err)
	}
	return writer{t, c, a.ID}
}

func (w writer) send(req localapi.SendReq) string {
	w.t.Helper()
	req.Agent = w.id
	var m localapi.SendResp
	if err := w.c.Call(context.Background(), "send", req, &m); err != nil {
		w.t.Fatalf("send: %v", err)
	}
	return m.ID
}

// unread is what peek counts for cs, marking nothing.
func unread(t *testing.T, cs *mcp.ClientSession) int {
	t.Helper()
	return parseLevels(t, ok(t, cs, "inbox", map[string]any{"peek": true})).Unread
}

// textOf is a message text of n bytes, numbered, some of it characters
// the result escapes (a zero width space becomes \\u{200b}, nine bytes).
func textOf(r *rand.Rand, i, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "m%03d ", i)
	for b.Len() < n {
		switch r.IntN(10) {
		case 0:
			b.WriteString(" \u200b ")
		case 1:
			b.WriteString("\"q\"\n")
		default:
			b.WriteString("word ")
		}
	}
	return b.String()
}

func createRoom(t *testing.T, c *localapi.Client, label string) {
	t.Helper()
	var r localapi.Room
	if err := c.Call(context.Background(), "room/create", localapi.RoomReq{Room: label}, &r); err != nil {
		t.Fatal(err)
	}
}

// [D3, D4, U4] A backlog larger than the budget, in two rooms interleaved, of
// texts from a few bytes to 6 KB, read at several budgets: every page is
// within max_bytes, more is what is still unread, nothing is lost or
// shown twice, the pages come in place order, and after each page exactly
// what it returned is marked seen.
func TestABacklogLargerThanTheBudgetIsReadInPagesWithNothingLostOrRepeated(t *testing.T) {
	c := startDaemon(t)
	createRoom(t, c, "team")
	bob := newWriter(t, c, "bob")
	a := connect(t, c, "alice")
	alice := agentID(t, a)
	r := rand.New(rand.NewPCG(70, 1))
	for _, max := range []int{node.MinInboxBytes * 3, 2500, 9000, node.DefaultInboxBytes, node.MaxInboxBytes} {
		want := map[string]string{}
		var order []string
		for i := 0; i < 60; i++ {
			room := []string{"", "team"}[i%2]
			// Each entry fits the budget alone (its escapes take up to
			// three times its bytes); several fit in all but the smallest.
			size := []int{5, 120, 400, 1500, 6000}[r.IntN(5)]
			text := textOf(r, i, min(size, max/4))
			to := []string(nil)
			if i%3 == 0 {
				to = []string{alice}
			}
			id := bob.send(localapi.SendReq{Room: room, Text: text, To: to})
			want[id] = text
			order = append(order, id)
		}
		var got []string
		last := ""
		for calls := 0; ; calls++ {
			if calls > 200 {
				t.Fatalf("max %d: no end after 200 calls", max)
			}
			p, _ := readPage(t, a, map[string]any{"max_bytes": max})
			if len(p.Messages) == 0 {
				if p.More != 0 {
					t.Fatalf("max %d: empty page with more %d", max, p.More)
				}
				break
			}
			if !p.MarkedSeen {
				t.Fatalf("max %d: page not marked seen", max)
			}
			for _, m := range p.Messages {
				if m.Created < last {
					t.Fatalf("max %d: %s at %s after %s: not in place order", max, m.ID, m.Created, last)
				}
				last = m.Created
				got = append(got, m.ID)
				if want[m.ID] == "" {
					t.Fatalf("max %d: %s was not sent", max, m.ID)
				}
			}
			if left := len(want) - len(got); p.More != left || unread(t, a) != left {
				t.Fatalf("max %d: more %d, peek %d, want %d still unread", max, p.More, unread(t, a), left)
			}
		}
		slices.Sort(got)
		if len(slices.Compact(slices.Clone(got))) != len(got) {
			t.Fatalf("max %d: a message was shown twice", max)
		}
		slices.Sort(order)
		if !slices.Equal(got, order) {
			t.Fatalf("max %d: read %d of %d messages", max, len(got), len(order))
		}
	}
}

// [U4] Through the tool, the result the model reads is never over
// max_bytes, for random budgets and entries of every size up to the
// largest text, many of their characters written as escapes; and a page
// is as full as it may be: the next entry would not have fitted.
func TestTheInboxResultNeverExceedsMaxBytes(t *testing.T) {
	c := startDaemon(t)
	bob := newWriter(t, c, "bob")
	a := connect(t, c, "alice")
	agentID(t, a)
	r := rand.New(rand.NewPCG(70, 2))
	for i := 0; i < 80; i++ {
		bob.send(localapi.SendReq{Text: textOf(r, i, []int{1, 60, 900, 5000, 12000}[r.IntN(5)])})
	}
	for calls := 0; calls < 200; calls++ {
		max := node.MinInboxBytes + r.IntN(node.MaxInboxBytes-node.MinInboxBytes+1)
		s, isErr := call(t, a, "inbox", map[string]any{"max_bytes": max})
		if isErr {
			if !strings.Contains(s, node.ErrorTooLarge) {
				t.Fatalf("max %d: %s", max, s)
			}
			continue
		}
		if len(s) > max {
			t.Fatalf("result is %d bytes, over max_bytes %d", len(s), max)
		}
		var p page
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Messages) == 0 {
			return
		}
	}
	t.Fatal("the backlog was not read in 200 calls")
}

// [D3, U4] The oldest unread message, too large for the budget asked, is
// refused with the size it needs; nothing is marked seen or skipped, and
// asking with that size returns it. A message larger than any budget (a
// text of hidden characters, each written as an escape) is refused at the
// default, and returned alone at the ceiling, cut to fit it.
func TestAnOldestMessageTooLargeForTheBudgetIsRefusedWithTheSizeItNeeds(t *testing.T) {
	c := startDaemon(t)
	bob := newWriter(t, c, "bob")
	a := connect(t, c, "alice")
	agentID(t, a)
	big := bob.send(localapi.SendReq{Text: strings.Repeat("abcdefgh ", 1000)}) // 9,000 bytes
	small := bob.send(localapi.SendReq{Text: "after the big one"})
	ref := refusal(t, a, "inbox", map[string]any{"max_bytes": 2000})
	if ref.Code != node.ErrorTooLarge || ref.Retryable || !strings.Contains(ref.Error, "nothing was marked seen") {
		t.Fatalf("refusal %+v", ref)
	}
	f := strings.Fields(ref.Error[strings.LastIndex(ref.Error, "max_bytes "):])
	need, err := strconv.Atoi(f[1])
	if err != nil || need <= 9000 || need > 9600 || !strings.Contains(ref.Error, "at least") {
		t.Fatalf("needs %q in %q", f[1], ref.Error)
	}
	if n := unread(t, a); n != 2 {
		t.Fatalf("after the refusal %d unread, want 2", n)
	}
	p, s := readPage(t, a, map[string]any{"max_bytes": need})
	if len(p.Messages) != 1 || p.Messages[0].ID != big || p.More != 1 || len(s) > need {
		t.Fatalf("with max_bytes %d: %s", need, s)
	}
	if p, _ := readPage(t, a, nil); len(p.Messages) != 1 || p.Messages[0].ID != small || p.More != 0 {
		t.Fatalf("then %+v", p)
	}

	// 5,400 zero width spaces: 16,200 bytes sent, 48,600 bytes shown.
	hidden := bob.send(localapi.SendReq{Text: strings.Repeat("\u200b", 5400)})
	bob.send(localapi.SendReq{Text: "after the hidden one"})
	ref = refusal(t, a, "inbox", nil)
	if ref.Code != node.ErrorTooLarge || !strings.Contains(ref.Error, "more than any max_bytes holds") || !strings.Contains(ref.Error, fmt.Sprintf("max_bytes %d", node.MaxInboxBytes)) {
		t.Fatalf("refusal %+v", ref)
	}
	s = ok(t, a, "inbox", map[string]any{"max_bytes": node.MaxInboxBytes})
	var alone page
	if err := json.Unmarshal([]byte(s), &alone); err != nil || len(alone.Messages) != 1 || alone.Messages[0].ID != hidden || alone.More != 1 ||
		!alone.MarkedSeen || len(s) > node.MaxInboxBytes || alone.Messages[0].TextCut == "" {
		t.Fatalf("at the ceiling: %d bytes, more %d", len(s), alone.More)
	}
	if n := unread(t, a); n != 1 {
		t.Fatalf("%d unread after the hidden one, want 1", n)
	}
}

// [U4] limit and max_bytes together: whichever binds first decides the
// page, and more counts what either left out.
func TestLimitAndMaxBytesTogetherWhicheverBindsFirst(t *testing.T) {
	c := startDaemon(t)
	bob := newWriter(t, c, "bob")
	a := connect(t, c, "alice")
	agentID(t, a)
	for i := 0; i < 30; i++ {
		bob.send(localapi.SendReq{Text: fmt.Sprintf("m%02d %s", i, strings.Repeat("x", 300))})
	}
	p, _ := readPage(t, a, map[string]any{"limit": 4, "max_bytes": node.MaxInboxBytes})
	if len(p.Messages) != 4 || p.More != 26 {
		t.Fatalf("limit binds: %d, more %d", len(p.Messages), p.More)
	}
	p, _ = readPage(t, a, map[string]any{"limit": 20, "max_bytes": 2000})
	if len(p.Messages) < 2 || len(p.Messages) > 4 || p.More != 26-len(p.Messages) {
		t.Fatalf("max_bytes binds: %d, more %d", len(p.Messages), p.More)
	}
	n := len(p.Messages)
	p, _ = readPage(t, a, map[string]any{"limit": 200})
	if len(p.Messages) != 26-n || p.More != 0 || p.Messages[0].Text[:3] != fmt.Sprintf("m%02d", 4+n) {
		t.Fatalf("the rest: %d, more %d", len(p.Messages), p.More)
	}
}

// [U4] Without max_bytes the inbox tool applies the default: a backlog of
// 1 KB messages comes in pages of about fifteen, not the limit's fifty.
func TestTheDefaultBudgetAppliesWhenMaxBytesIsAbsent(t *testing.T) {
	c := startDaemon(t)
	bob := newWriter(t, c, "bob")
	a := connect(t, c, "alice")
	agentID(t, a)
	for i := 0; i < 60; i++ {
		bob.send(localapi.SendReq{Text: strings.Repeat("y", 1000)})
	}
	p, s := readPage(t, a, map[string]any{})
	if len(p.Messages) < 10 || len(p.Messages) >= 50 || p.More != 60-len(p.Messages) || len(s) > node.DefaultInboxBytes {
		t.Fatalf("default page: %d messages, more %d, %d bytes", len(p.Messages), p.More, len(s))
	}
	// The local API applies no budget unless asked: an older shim reads as
	// before, with more now saying what the limit left out.
	var in localapi.InboxResp
	if err := c.Call(context.Background(), "inbox", localapi.InboxReq{Agent: agentID(t, a)}, &in); err != nil {
		t.Fatal(err)
	}
	if left := 60 - len(p.Messages); len(in.Entries) != min(left, node.DefaultInboxLimit) || in.More != left-len(in.Entries) || in.MaxBytes != 0 {
		t.Fatalf("local API without max_bytes: %d entries, more %d, max_bytes %d", len(in.Entries), in.More, in.MaxBytes)
	}
}

// [U4] max_bytes is refused below its range, and with the views that mark
// nothing seen (peek, recent, skipped, thread, fetch), which do not take
// it; a level set with a read counts its levels in the budget.
func TestMaxBytesIsCheckedAndCountsTheLevelsOfTheSameCall(t *testing.T) {
	c := startDaemon(t)
	for i := 0; i < 6; i++ {
		createRoom(t, c, fmt.Sprint("room", i))
	}
	bob := newWriter(t, c, "bob")
	a := connect(t, c, "alice")
	agentID(t, a)
	for _, args := range []map[string]any{
		{"max_bytes": 511}, {"max_bytes": 1}, {"max_bytes": -1},
		{"max_bytes": 1000, "peek": true}, {"max_bytes": 1000, "recent": 5}, {"max_bytes": 1000, "skipped": true},
		{"max_bytes": 1000, "thread": strings.Repeat("0", 32)}, {"max_bytes": 1000, "fetch": strings.Repeat("0", 64)},
	} {
		if r := refusal(t, a, "inbox", args); r.Code != node.ErrorInvalid || !strings.Contains(r.Error, "max_bytes") {
			t.Errorf("%v: %+v", args, r)
		}
	}
	// 0 is the default, as when it is left out: a caller written against a
	// schema without the field sends neither (D72).
	bob.send(localapi.SendReq{Text: strings.Repeat("y", 1000)})
	if p, s := readPage(t, a, map[string]any{"max_bytes": 0}); len(p.Messages) != 1 || len(s) > node.DefaultInboxBytes {
		t.Fatalf("max_bytes 0: %s", s)
	}
	for i := 0; i < 20; i++ {
		bob.send(localapi.SendReq{Room: fmt.Sprint("room", i%6), Text: strings.Repeat("z", 200)})
	}
	p, _ := readPage(t, a, map[string]any{"level": "all", "max_bytes": 2500})
	if len(p.Levels) != 7 || len(p.Messages) == 0 || p.More != 20-len(p.Messages) {
		t.Fatalf("levels %d, messages %d, more %d", len(p.Levels), len(p.Messages), p.More)
	}
}

// [D3] Two sessions of one agent page at once: each page is within
// the budget, and together they read every message; one may show a
// message the other confirmed first (reading is two steps), but none is
// lost, and the inbox ends empty.
func TestTwoSessionsOfOneLabelPagingAtOnceLoseNothing(t *testing.T) {
	c := startDaemon(t)
	bob := newWriter(t, c, "bob")
	a1, a2 := connect(t, c, "alice"), connect(t, c, "alice")
	if agentID(t, a1) != agentID(t, a2) {
		t.Fatal("two sessions of one label are not one agent")
	}
	sent := map[string]bool{}
	for i := 0; i < 120; i++ {
		sent[bob.send(localapi.SendReq{Text: fmt.Sprintf("m%03d %s", i, strings.Repeat("w", 150))})] = true
	}
	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	for _, cs := range []*mcp.ClientSession{a1, a2} {
		wg.Go(func() {
			for calls := 0; calls < 100; calls++ {
				s, isErr := call(t, cs, "inbox", map[string]any{"max_bytes": 3000})
				var p page
				if isErr || len(s) > 3000 || json.Unmarshal([]byte(s), &p) != nil {
					t.Errorf("%v %d bytes: %.200s", isErr, len(s), s)
					return
				}
				if len(p.Messages) == 0 {
					return
				}
				mu.Lock()
				for _, m := range p.Messages {
					got[m.ID]++
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	twice := 0
	for id, n := range got {
		if !sent[id] {
			t.Fatalf("%s was not sent", id)
		}
		twice += n - 1
	}
	if len(got) != len(sent) || unread(t, a1) != 0 {
		t.Fatalf("read %d of %d, %d still unread", len(got), len(sent), unread(t, a1))
	}
	t.Logf("%d messages shown to both sessions", twice)
}

// [D3, D9] Between pages, a message not yet shown is replaced, another is
// withdrawn, and one already shown is replaced: the next pages show the
// first with its new text, never the second, and a replaced notice for
// the third. Nothing else changes.
func TestAMessageReplacedOrWithdrawnBetweenPagesIsShownAsItIsNow(t *testing.T) {
	c := startDaemon(t)
	bob := newWriter(t, c, "bob")
	a := connect(t, c, "alice")
	agentID(t, a)
	var ids []string
	for i := 0; i < 12; i++ {
		ids = append(ids, bob.send(localapi.SendReq{Text: fmt.Sprintf("m%02d %s", i, strings.Repeat("v", 400))}))
	}
	p, _ := readPage(t, a, map[string]any{"max_bytes": 2000})
	n := len(p.Messages)
	if n == 0 || n > 4 || p.Messages[0].ID != ids[0] {
		t.Fatalf("first page %d", n)
	}
	bob.send(localapi.SendReq{Replaces: ids[0], Text: "m00 shown, then replaced"})
	bob.send(localapi.SendReq{Replaces: ids[n], Text: "replaced before it was shown"})
	bob.send(localapi.SendReq{Replaces: ids[n+1]}) // withdrawn before it was shown
	seen := map[string][]string{}
	for calls := 0; calls < 50; calls++ {
		p, _ := readPage(t, a, map[string]any{"max_bytes": 2000})
		if len(p.Messages) == 0 {
			break
		}
		for _, m := range p.Messages {
			kind := m.Kind
			if kind == "" {
				kind = "message"
			}
			seen[m.ID] = append(seen[m.ID], kind+":"+m.Text)
		}
	}
	if s := seen[ids[0]]; len(s) != 1 || s[0] != "replaced:m00 shown, then replaced" {
		t.Fatalf("shown then replaced: %q", s)
	}
	if s := seen[ids[n]]; len(s) != 1 || s[0] != "message:replaced before it was shown" {
		t.Fatalf("replaced before shown: %q", s)
	}
	if s, ok := seen[ids[n+1]]; ok {
		t.Fatalf("withdrawn before shown, yet shown: %q", s)
	}
	for _, id := range ids[n+2:] {
		if len(seen[id]) != 1 {
			t.Fatalf("%s shown %d times", id, len(seen[id]))
		}
	}
}

// proxy is a local API socket in front of a real daemon, which can change
// each request (req) and each answer (resp) by the path called.
func proxy(t *testing.T, real string, req func(path string, body []byte) []byte, resp func(path string, status int, body []byte) (int, []byte)) *localapi.Client {
	t.Helper()
	dir, err := os.MkdirTemp(sockTemp(), "a2p2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "proxy")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	up := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", real)
	}}}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		body = req(r.URL.Path, body)
		rq, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://a2p2"+r.URL.Path, bytes.NewReader(body))
		rq.Header.Set("Content-Type", r.Header.Get("Content-Type"))
		if k := r.Header.Get(localapi.KeyHeader); k != "" {
			rq.Header.Set(localapi.KeyHeader, k)
		}
		res, err := up.Do(rq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		status, out := resp(r.URL.Path, res.StatusCode, out)
		w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
		w.WriteHeader(status)
		w.Write(out)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	key := func() string {
		k, _ := os.ReadFile(filepath.Join(filepath.Dir(real), "local.key"))
		return strings.TrimSpace(string(k))
	}
	return localapi.NewClient(sock).WithKey(key)
}

// strip is the JSON object b without keys.
func strip(b []byte, keys ...string) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return b
	}
	for _, k := range keys {
		delete(m, k)
	}
	out, _ := json.Marshal(m)
	return out
}

// oldDaemon is a proxy that answers as a daemon before 0.2.4 does: its
// inbox takes neither max_bytes nor level and answers without more,
// max_bytes or levels, and its status lists no features.
func oldDaemon(t *testing.T, real string) *localapi.Client {
	return proxy(t, real, func(path string, b []byte) []byte {
		if path == "/v1/inbox" {
			return strip(b, "max_bytes", "level")
		}
		return b
	}, func(path string, status int, b []byte) (int, []byte) {
		switch {
		case status != http.StatusOK:
		case path == "/v1/inbox":
			b = strip(b, "more", "max_bytes", "levels")
		case path == "/v1/status":
			b = strip(b, "features")
		}
		return status, b
	})
}

// [D3, R7, U4, D70] A new MCP server on a daemon before 0.2.4, which ignores
// max_bytes: the server reads as 0.2.3 did, the daemon's own page of up
// to fifty, whole, and confirms all of it, and says the budget was not
// applied and how to start the new daemon, with max_bytes given or not.
// Nothing the page left out has a receipt: an ack of it is refused, and
// level none keeps it out. A level set in the same call is set first, as
// 0.2.3 did.
func TestANewServerOnAnOldDaemonReadsAsTheOldServerDidAndSaysTheBudgetWasNotApplied(t *testing.T) {
	c, home := startDaemonHome(t)
	bob := newWriter(t, c, "bob")
	oc := oldDaemon(t, daemon.SockPath(home))
	a := connect(t, oc, "alice")
	alice := agentID(t, a)
	var ids []string
	for i := 0; i < 120; i++ {
		ids = append(ids, bob.send(localapi.SendReq{Text: fmt.Sprintf("m%02d %s", i, strings.Repeat("u", 400)), To: []string{alice}}))
	}
	for _, args := range []map[string]any{{"max_bytes": 5000}, nil} {
		s := ok(t, a, "inbox", args)
		var p struct {
			page
			Note string `json:"note"`
		}
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			t.Fatal(err)
		}
		want := min(node.DefaultInboxLimit, len(ids))
		if len(p.Messages) != want || !p.MarkedSeen || p.More != 0 || p.Note != oldDaemonNote || !strings.Contains(p.Note, "a2p2 stop") {
			t.Fatalf("%v: %d messages, marked %v, more %d, note %q", args, len(p.Messages), p.MarkedSeen, p.More, p.Note)
		}
		for i, m := range p.Messages {
			if m.ID != ids[i] {
				t.Fatalf("%v: message %d is %s, want %s", args, i, m.ID, ids[i])
			}
		}
		// The result is the 0.2.3 one with the note: the same messages,
		// rendered by the same code, and nothing else.
		var fields map[string]json.RawMessage
		json.Unmarshal([]byte(s), &fields)
		for k := range fields {
			if !slices.Contains([]string{"notice", "marked_seen", "messages", "rooms", "note"}, k) {
				t.Fatalf("%v: field %s in an old daemon's read", args, k)
			}
		}
		ids = ids[want:]
		if n := unread(t, a); n != len(ids) {
			t.Fatalf("%v: %d unread, want %d", args, n, len(ids))
		}
	}
	// Twenty are left with no receipt: an ack by id is refused.
	if r := refusal(t, a, "ack", map[string]any{"id": ids[len(ids)-1]}); r.Code != node.ErrorNotShown {
		t.Fatalf("ack of a message never returned: %+v", r)
	}
	// Level none in the same call: set first, so nothing comes.
	p, _ := readPage(t, a, map[string]any{"level": "none"})
	if len(p.Messages) != 0 || len(p.Levels) != 1 || p.Levels[0].Level != "none" {
		t.Fatalf("level none on an old daemon: %+v", p)
	}
}

// [U2, U4, D70] The budget covers the whole result: with nothing unread and
// many rooms, a read that would set their levels at 512 bytes is refused
// with a refusal of its own, naming a call of its own, and sets nothing;
// that call (level with peek) lists twenty rooms and counts the rest; a
// read with the level at a budget that holds the list fits it.
func TestTheBudgetCoversTheLevelsOfTheSameCallAndARefusedReadSetsNothing(t *testing.T) {
	c := startDaemon(t)
	for i := 0; i < 24; i++ {
		createRoom(t, c, fmt.Sprintf("room%02d", i))
	}
	a := connect(t, c, "alice")
	agentID(t, a)
	ref := refusal(t, a, "inbox", map[string]any{"level": "none", "max_bytes": node.MinInboxBytes})
	if ref.Code != node.ErrorTooLarge || !strings.Contains(ref.Error, "25 rooms") || !strings.Contains(ref.Error, "a call of its own") || !strings.Contains(ref.Error, "nothing was changed") {
		t.Fatalf("refusal %+v", ref)
	}
	lv := parseLevels(t, ok(t, a, "whoami", nil))
	for _, r := range lv.Rooms {
		if r.Level != "all" {
			t.Fatalf("a refused read set %s to %s", r.Room, r.Level)
		}
	}
	s := ok(t, a, "inbox", map[string]any{"level": "addressed", "peek": true})
	var pk struct {
		Levels         []localapi.ToolLevel `json:"levels"`
		LevelsUnlisted int                  `json:"levels_unlisted"`
	}
	if err := json.Unmarshal([]byte(s), &pk); err != nil || len(pk.Levels) != localapi.MaxToolLevels || pk.LevelsUnlisted != 25-localapi.MaxToolLevels {
		t.Fatalf("level with peek: %d listed, %d unlisted: %.300s", len(pk.Levels), pk.LevelsUnlisted, s)
	}
	for _, max := range []int{4000, node.DefaultInboxBytes} {
		p, s := readPage(t, a, map[string]any{"level": "all", "max_bytes": max})
		if len(p.Levels) != localapi.MaxToolLevels || !strings.Contains(s, `"levels_unlisted":5`) {
			t.Fatalf("max %d: %s", max, s)
		}
	}
	// With messages too, the page holds what fits with the list.
	bob := newWriter(t, c, "bob")
	for i := 0; i < 20; i++ {
		bob.send(localapi.SendReq{Room: fmt.Sprintf("room%02d", i), Text: strings.Repeat("z", 200)})
	}
	p, _ := readPage(t, a, map[string]any{"level": "all", "max_bytes": 3500})
	if len(p.Levels) != localapi.MaxToolLevels || len(p.Messages) == 0 || p.More != 20-len(p.Messages) {
		t.Fatalf("levels %d, messages %d, more %d", len(p.Levels), len(p.Messages), p.More)
	}
}

// [D3, U4, D70] more counts every entry a read leaves unread, of every kind
// the inbox returns: notices of the agent's sends with files declined,
// paged one at a time; a page whose seen failed, which comes again; and a
// refusal names a size with room for a message arriving before the retry.
func TestMoreCountsEveryEntryTheReadLeavesUnread(t *testing.T) {
	c, home := startDaemonHome(t)
	a := connect(t, c, "alice")
	agentID(t, a)
	// Two sends with files, declined by the person: two request entries.
	file := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		var w struct {
			Request string `json:"request"`
		}
		if err := json.Unmarshal([]byte(ok(t, a, "send", map[string]any{"text": fmt.Sprint("files ", i), "files": []string{file}})), &w); err != nil {
			t.Fatal(err)
		}
		if err := c.Call(context.Background(), "confirm", localapi.ConfirmReq{ID: w.Request, Approve: false}, nil); err != nil {
			t.Fatal(err)
		}
	}
	for want := 1; want >= 0; want-- {
		p, _ := readPage(t, a, map[string]any{"limit": 1})
		if len(p.Messages) != 1 || p.Messages[0].Kind != node.EntryRequest || p.More != want {
			t.Fatalf("decision page: %d, kind %q, more %d, want %d", len(p.Messages), p.Messages[0].Kind, p.More, want)
		}
	}

	// A seen that fails: what the page returned stays unread, and more
	// says so.
	bob := newWriter(t, c, "bob")
	for i := 0; i < 3; i++ {
		bob.send(localapi.SendReq{Text: fmt.Sprint("s", i)})
	}
	failing := proxy(t, daemon.SockPath(home), func(path string, b []byte) []byte {
		if path == "/v1/seen" {
			return strip(b, "entries") // the daemon confirms nothing
		}
		return b
	}, func(path string, status int, b []byte) (int, []byte) {
		if path == "/v1/seen" {
			return http.StatusInternalServerError, []byte(`{"error":"injected"}`)
		}
		return status, b
	})
	f := connect(t, failing, "alice")
	p, _ := readPage(t, f, map[string]any{"limit": 2})
	if len(p.Messages) != 2 || p.MarkedSeen || p.More != 3 {
		t.Fatalf("a page whose seen failed: %d, marked %v, more %d", len(p.Messages), p.MarkedSeen, p.More)
	}
	for want := 1; want >= 0; want-- {
		if p, _ := readPage(t, a, map[string]any{"limit": 2}); p.More != want || !p.MarkedSeen {
			t.Fatalf("then more %d, want %d", p.More, want)
		}
	}

	// A refusal with nine held; a tenth arrives before the retry, which
	// lengthens more by a digit: the size named still holds it.
	bob.send(localapi.SendReq{Text: strings.Repeat("big ", 1500)})
	for i := 0; i < 8; i++ {
		bob.send(localapi.SendReq{Text: fmt.Sprint("small ", i)})
	}
	ref := refusal(t, a, "inbox", map[string]any{"max_bytes": 1000})
	f2 := strings.Fields(ref.Error[strings.LastIndex(ref.Error, "max_bytes "):])
	need, err := strconv.Atoi(strings.TrimSuffix(f2[1], ";"))
	if err != nil || !strings.Contains(ref.Error, "at least") {
		t.Fatalf("refusal %q", ref.Error)
	}
	bob.send(localapi.SendReq{Text: "the tenth"})
	if p, _ := readPage(t, a, map[string]any{"max_bytes": need}); len(p.Messages) == 0 || p.More != 10-len(p.Messages) {
		t.Fatalf("retry at %d: %d, more %d", need, len(p.Messages), p.More)
	}
}

// measured is the size the daemon measures for result s: marked_seen
// false, and more as every entry held.
func measured(t *testing.T, s string, held int) int {
	t.Helper()
	var r localapi.ToolInbox
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	r.MarkedSeen, r.More = false, held
	out, _ := localapi.ToolJSON(r)
	return len(out)
}

// [U4, D70] Messages with files, choices, a person's identity, an edit and
// an edit by another agent of the author's machine fit max_bytes to the
// byte: three agents see the same room-wide messages; at the size the
// daemon measures for one's page of them all, another gets them all, and
// a byte less, a third gets all but the last.
func TestEntriesWithEveryMarkFitMaxBytesToTheByte(t *testing.T) {
	c := startDaemon(t)
	readers := []*mcp.ClientSession{connect(t, c, "alice"), connect(t, c, "carol"), connect(t, c, "erin")}
	for _, r := range readers {
		agentID(t, r)
	}
	d := connect(t, c, "dave")
	agentID(t, d)
	bob := newWriter(t, c, "bob")
	var st localapi.Status
	if err := c.Call(context.Background(), "status", nil, &st); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "report \u202e.txt")
	if err := os.WriteFile(file, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	ed := bob.send(localapi.SendReq{Text: "first text"})
	bob.send(localapi.SendReq{Text: "pick one \u200b", Choices: []string{"yes", "no \u2066"}})
	if err := c.Call(context.Background(), "send", localapi.SendReq{Agent: st.Human, Text: "from the person"}, nil); err != nil {
		t.Fatal(err)
	}
	bob.send(localapi.SendReq{Replaces: ed, Text: "edited text"})
	other := bob.send(localapi.SendReq{Text: "to be edited by dave"})
	if _, isErr := call(t, d, "send", map[string]any{"replaces": other, "text": "dave's edit"}); isErr {
		t.Log("an edit by another agent of the machine is refused: changed_by is not covered")
	}
	var w struct {
		Request string `json:"request"`
	}
	if err := json.Unmarshal([]byte(ok(t, d, "send", map[string]any{"text": "with a file", "files": []string{file}})), &w); err != nil {
		t.Fatal(err)
	}
	var qs localapi.RequestsResp
	if err := c.Call(context.Background(), "requests", localapi.Empty{}, &qs); err != nil || len(qs.Requests) != 1 {
		t.Fatalf("requests: %+v %v", qs, err)
	}
	if err := c.Call(context.Background(), "confirm", localapi.ConfirmReq{ID: w.Request, Approve: true, Terms: qs.Requests[0].Terms}, nil); err != nil {
		t.Fatal(err)
	}
	k := unread(t, readers[0])
	whole := ok(t, readers[0], "inbox", map[string]any{"max_bytes": node.MaxInboxBytes})
	for _, mark := range []string{`"files"`, `"choices"`, `"person"`, `"edited":true`} {
		if !strings.Contains(whole, mark) {
			t.Fatalf("no %s in the page: %s", mark, whole)
		}
	}
	if !strings.Contains(whole, `"changed_by"`) {
		t.Log("no changed_by in the page")
	}
	size := measured(t, whole, k)
	if p, s := readPage(t, readers[1], map[string]any{"max_bytes": size}); len(p.Messages) != k || s != whole {
		t.Fatalf("at %d bytes: %d of %d, or a page other than the first reader's", size, len(p.Messages), k)
	}
	if p, _ := readPage(t, readers[2], map[string]any{"max_bytes": size - 1}); len(p.Messages) != k-1 || p.More != 1 {
		t.Fatalf("at %d bytes: %d of %d, more %d", size-1, len(p.Messages), k, p.More)
	}
}

// [U4, D70] A message larger than any budget, 16,372 DEL characters, each
// written \u{7f} (about 115 KB shown), is refused below the ceiling, and
// at it comes alone, its text cut between escapes to fit, saying it is
// cut, by how much, and how a person reads the rest; it is marked seen,
// and the result is within the ceiling, 32,000 bytes.
func TestAMessageLargerThanAnyBudgetComesCutToFitAndSaysSo(t *testing.T) {
	c := startDaemon(t)
	bob := newWriter(t, c, "bob")
	a := connect(t, c, "alice")
	agentID(t, a)
	text := strings.Repeat("\x7f", 16372)
	id := bob.send(localapi.SendReq{Text: text})
	bob.send(localapi.SendReq{Text: "after it"})
	ref := refusal(t, a, "inbox", nil)
	if ref.Code != node.ErrorTooLarge || !strings.Contains(ref.Error, "more than any max_bytes holds") || !strings.Contains(ref.Error, fmt.Sprintf("max_bytes %d to read it with its text cut", node.MaxInboxBytes)) {
		t.Fatalf("refusal %+v", ref)
	}
	s := ok(t, a, "inbox", map[string]any{"max_bytes": node.MaxInboxBytes})
	var r localapi.ToolInbox
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	if len(s) > node.MaxInboxBytes || len(r.Messages) != 1 || r.Messages[0].ID != id || !r.MarkedSeen || r.More != 1 {
		t.Fatalf("%d bytes, %d messages, marked %v, more %d", len(s), len(r.Messages), r.MarkedSeen, r.More)
	}
	m := r.Messages[0]
	shown := node.EscapeDisplayText(text)
	if !strings.HasPrefix(shown, m.Text) || len(m.Text)%len(`\u{7f}`) != 0 || len(m.Text) == 0 {
		t.Fatalf("cut text of %d bytes is not whole escapes from the start", len(m.Text))
	}
	b, _ := json.Marshal(shown)
	cb, _ := json.Marshal(m.Text)
	left := len(b) - len(cb)
	if !strings.Contains(m.TextCut, fmt.Sprintf("%d bytes", left)) || !strings.Contains(m.TextCut, "a2p2 inbox --thread "+id) || !strings.Contains(m.TextCut, "only the start") {
		t.Fatalf("text_cut %q, want %d bytes left out", m.TextCut, left)
	}
	if n := unread(t, a); n != 1 {
		t.Fatalf("%d unread, want 1", n)
	}
	t.Logf("whole: %d bytes shown; result %d bytes", len(b), len(s))
}

// [U4, D72] A request above the ceiling (40,000 was allowed by earlier
// builds, and a model passes it on its own) is not refused: it is read at
// the ceiling, the result stays within 32,000 bytes with its note saying
// so, and what it returned is marked seen while the rest stays unread. A
// message larger than any budget asked for that way is refused naming the
// ceiling, then comes cut at it, and nothing is marked seen in between.
func TestARequestAboveTheCeilingIsReadAtTheCeilingAndSaysSo(t *testing.T) {
	c := startDaemon(t)
	bob := newWriter(t, c, "bob")
	a := connect(t, c, "alice")
	agentID(t, a)
	for i := 0; i < 50; i++ {
		bob.send(localapi.SendReq{Text: fmt.Sprintf("m%02d %s", i, strings.Repeat("y", 1000))})
	}
	note := fmt.Sprintf(`"note":"max_bytes above %d was read as %d`, node.MaxInboxBytes, node.MaxInboxBytes)
	for _, max := range []int{node.MaxInboxBytes + 1, 40000, 1 << 30} {
		s := ok(t, a, "inbox", map[string]any{"max_bytes": max, "limit": 1})
		if len(s) > node.MaxInboxBytes || !strings.Contains(s, note) {
			t.Fatalf("max_bytes %d: %d bytes: %s", max, len(s), s)
		}
	}
	s := ok(t, a, "inbox", map[string]any{"max_bytes": 40000})
	var p localapi.ToolInbox
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		t.Fatal(err)
	}
	if len(s) > node.MaxInboxBytes || len(p.Messages) <= 20 || !p.MarkedSeen || p.More != 47-len(p.Messages) || !strings.Contains(s, note) {
		t.Fatalf("%d bytes, %d messages, marked %v, more %d", len(s), len(p.Messages), p.MarkedSeen, p.More)
	}
	if n := unread(t, a); n != p.More {
		t.Fatalf("%d unread, more %d", n, p.More)
	}
	for more := p.More; more > 0; {
		next, _ := readPage(t, a, nil)
		more = next.More
	}

	big := bob.send(localapi.SendReq{Text: strings.Repeat("\x7f", 16000)})
	ref := refusal(t, a, "inbox", map[string]any{"max_bytes": 40000})
	if ref.Code != node.ErrorTooLarge || !strings.Contains(ref.Error, fmt.Sprintf("max_bytes %d to read it with its text cut", node.MaxInboxBytes)) {
		t.Fatalf("refusal %+v", ref)
	}
	if n := unread(t, a); n != 1 {
		t.Fatalf("%d unread after the refusal, want 1", n)
	}
	q, s := readPage(t, a, map[string]any{"max_bytes": node.MaxInboxBytes})
	if len(q.Messages) != 1 || q.Messages[0].ID != big || q.Messages[0].TextCut == "" || !q.MarkedSeen || strings.Contains(s, `"note"`) {
		t.Fatalf("at the ceiling: %s", s)
	}
}
