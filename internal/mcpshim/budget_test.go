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
	if m, ok := args["max_bytes"].(int); ok {
		max = m
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
// default, and returned alone, over the budget, at the ceiling.
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
	f := strings.Fields(ref.Error[strings.Index(ref.Error, "max_bytes "):])
	need, err := strconv.Atoi(f[1])
	if err != nil || need <= 9000 || need > 9600 {
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
	if ref.Code != node.ErrorTooLarge || !strings.Contains(ref.Error, "more than any max_bytes holds") || !strings.Contains(ref.Error, "max_bytes 40000") {
		t.Fatalf("refusal %+v", ref)
	}
	s = ok(t, a, "inbox", map[string]any{"max_bytes": node.MaxInboxBytes})
	var alone page
	if err := json.Unmarshal([]byte(s), &alone); err != nil || len(alone.Messages) != 1 || alone.Messages[0].ID != hidden || alone.More != 1 ||
		!alone.MarkedSeen || len(s) <= node.MaxInboxBytes {
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
// 1 KB messages comes in pages of about twenty, not the limit's fifty.
func TestTheDefaultBudgetAppliesWhenMaxBytesIsAbsent(t *testing.T) {
	c := startDaemon(t)
	bob := newWriter(t, c, "bob")
	a := connect(t, c, "alice")
	agentID(t, a)
	for i := 0; i < 60; i++ {
		bob.send(localapi.SendReq{Text: strings.Repeat("y", 1000)})
	}
	p, s := readPage(t, a, map[string]any{})
	if len(p.Messages) < 15 || len(p.Messages) >= 50 || p.More != 60-len(p.Messages) || len(s) > node.DefaultInboxBytes {
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

// [U4] max_bytes is refused out of its range, and with the views that mark
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
		{"max_bytes": 511}, {"max_bytes": 40001}, {"max_bytes": -1},
		{"max_bytes": 1000, "peek": true}, {"max_bytes": 1000, "recent": 5}, {"max_bytes": 1000, "skipped": true},
		{"max_bytes": 1000, "thread": strings.Repeat("0", 32)}, {"max_bytes": 1000, "fetch": strings.Repeat("0", 64)},
	} {
		if r := refusal(t, a, "inbox", args); r.Code != node.ErrorInvalid || !strings.Contains(r.Error, "max_bytes") {
			t.Errorf("%v: %+v", args, r)
		}
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

// oldDaemon is a local API socket in front of a real daemon that answers
// inbox as a daemon before 0.2.4 does: it drops max_bytes and reserve
// from the request, and more and max_bytes from the answer.
func oldDaemon(t *testing.T, real string) *localapi.Client {
	t.Helper()
	dir, err := os.MkdirTemp(sockTemp(), "a2p2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "old")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	up := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", real)
	}}}
	strip := func(b []byte, keys ...string) []byte {
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
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		inbox := r.URL.Path == "/v1/inbox"
		if inbox {
			body = strip(body, "max_bytes", "reserve")
		}
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://a2p2"+r.URL.Path, bytes.NewReader(body))
		req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
		if k := r.Header.Get(localapi.KeyHeader); k != "" {
			req.Header.Set(localapi.KeyHeader, k)
		}
		res, err := up.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		if inbox && res.StatusCode == http.StatusOK {
			out = strip(out, "more", "max_bytes")
		}
		w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
		w.WriteHeader(res.StatusCode)
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

// [D3, U4] A new MCP server on a daemon before 0.2.4, which ignores max_bytes:
// the server cuts the page by the same measure and rule, confirms only
// that, and says how many are still unread, so the result stays within
// the budget and the walk loses nothing. The oversized refusal holds too.
func TestANewServerOnAnOldDaemonPagesItselfAndConfirmsOnlyThePage(t *testing.T) {
	c, home := startDaemonHome(t)
	bob := newWriter(t, c, "bob")
	a := connect(t, oldDaemon(t, daemon.SockPath(home)), "alice")
	agentID(t, a)
	sent := map[string]bool{}
	for i := 0; i < 70; i++ {
		sent[bob.send(localapi.SendReq{Text: fmt.Sprintf("m%02d %s", i, strings.Repeat("u", 100+10*i))})] = true
	}
	got := 0
	for calls := 0; calls < 100; calls++ {
		p, _ := readPage(t, a, map[string]any{"max_bytes": 5000})
		if len(p.Messages) == 0 {
			break
		}
		for _, m := range p.Messages {
			if !sent[m.ID] {
				t.Fatalf("%s not sent, or shown twice", m.ID)
			}
			delete(sent, m.ID)
			got++
		}
		if p.More != len(sent) || unread(t, a) != len(sent) {
			t.Fatalf("more %d, peek %d, want %d", p.More, unread(t, a), len(sent))
		}
	}
	if got != 70 {
		t.Fatalf("read %d of 70", got)
	}
	bob.send(localapi.SendReq{Text: strings.Repeat("t", 3000)})
	if r := refusal(t, a, "inbox", map[string]any{"max_bytes": 1000}); r.Code != node.ErrorTooLarge {
		t.Fatalf("oversized on an old daemon: %+v", r)
	}
	if n := unread(t, a); n != 1 {
		t.Fatalf("%d unread after the refusal", n)
	}
}
