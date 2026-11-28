package mcpshim

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wallter/a2p2/internal/daemon"
	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// Under Claude Code the inbox result is capped at the harness's token
// limit, read as bytes, less a margin (D74): a result no larger in bytes
// than the limit cannot pass it at any text, as measured.

// capped is an inbox result with its note.
type capped struct {
	page
	Note string `json:"note"`
}

func readCapped(t *testing.T, cs *mcp.ClientSession, args map[string]any) (capped, string) {
	t.Helper()
	s := ok(t, cs, "inbox", args)
	var p capped
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		t.Fatalf("%v: %s", err, s)
	}
	if strings.Count(s, `"note":`) > 1 {
		t.Fatalf("more than one note: %s", s)
	}
	return p, s
}

// claudeSession is an MCP session started as Claude Code starts the server
// (--harness claude), with limit as MAX_MCP_OUTPUT_TOKENS ("" leaves it
// unset).
func claudeSession(t *testing.T, c *localapi.Client, label, limit string) *mcp.ClientSession {
	t.Helper()
	t.Setenv(envClaudeLimit, limit)
	cs := connectAs(t, c, harnessClaude, label)
	agentID(t, cs)
	return cs
}

// [U4, D74] The cap follows the limit Claude Code reads: 25,000 tokens
// unset, the variable when it is a positive integer, never above the
// ceiling or below the floor; anything else is ignored, and any other
// harness has none.
func TestTheInlineCapFollowsClaudeCodesOutputLimit(t *testing.T) {
	for _, c := range []struct {
		harness, env string
		cap          int
		floored      bool
	}{
		{"claude", "", 24000, false},
		{"claude", "8000", 7000, false},
		{"claude", " 8000 ", 7000, false},
		{"claude", "25000", 24000, false},
		{"claude", "100000", 24000, false},
		{"claude", "99999999999999999999999", 24000, false},
		{"claude", "garbage", 24000, false},
		{"claude", "8000tokens", 7000, false},
		{"claude", "8000.5", 7000, false},
		{"claude", "8e3", 7000, false},
		{"claude", "+8000", 7000, false},
		{"claude", "0x1f40", 7000, false},
		{"claude", ".8e4", 7000, false},
		{"claude", "1.5e4", 14000, false},
		{"claude", "0.5", 512, true},
		{"claude", "0", 24000, false},
		{"claude", "-5", 24000, false},
		{"claude", "-8e3", 24000, false},
		{"claude", "Infinity", 24000, false},
		{"claude", "NaN", 24000, false},
		{"claude", "  ", 24000, false},
		{"claude", "1512", 512, false},
		{"claude", "1511", 512, true},
		{"claude", "1", 512, true},
		{"codex", "8000", 0, false},
		{"mcp", "8000", 0, false},
		{"test", "", 0, false},
	} {
		cap, floored := inlineCap(c.harness, func(k string) string {
			if k != envClaudeLimit {
				t.Fatalf("read %s", k)
			}
			return c.env
		})
		if cap != c.cap || floored != c.floored {
			t.Errorf("%s %q: cap %d floored %v, want %d %v", c.harness, c.env, cap, floored, c.cap, c.floored)
		}
	}
	// The widest entry the validators admit, without its text, is within
	// 1,000 bytes of the ceiling less (localapi's
	// TestAnEntryAtItsLargestBesidesItsTextFitsTheCeiling), so with the
	// note that it is over the cap it still fits the ceiling.
	if n := noteBytes(overNote); n >= 1000 {
		t.Fatalf("the over-the-cap note takes %d bytes", n)
	}
}

// [U4, D74] Under Claude Code with the limit unset, the default stays
// 18,000 and a larger max_bytes is read at 24,000 with one note saying
// so; with the limit at 8,000 the default is the cap, 7,000, with no note,
// and a request above it is read at it with its note. What each page
// returns is marked seen and the rest stays unread.
func TestUnderClaudeCodeARequestAboveTheCapIsReadAtTheCapAndALowCapLowersTheDefault(t *testing.T) {
	for _, c := range []struct {
		env          string
		cap, deflt   int
		aboveTheCaps []int
	}{
		{"", 24000, node.DefaultInboxBytes, []int{24001, 32000, 40000, 1 << 30}},
		{"8000", 7000, 7000, []int{7001, 18000, 32000, 40000}},
	} {
		t.Run("limit="+c.env, func(t *testing.T) {
			cl := startDaemon(t)
			bob := newWriter(t, cl, "bob")
			a := claudeSession(t, cl, "alice", c.env)
			for i := 0; i < 120; i++ {
				bob.send(localapi.SendReq{Text: fmt.Sprintf("m%03d %s", i, strings.Repeat("y", 1000))})
			}
			left := 120
			p, s := readCapped(t, a, nil)
			if len(s) > c.deflt || len(s) < c.deflt-1500 || p.Note != "" || !p.MarkedSeen {
				t.Fatalf("default: %d bytes, note %q", len(s), p.Note)
			}
			left -= len(p.Messages)
			if p.More != left || unread(t, a) != left {
				t.Fatalf("default: more %d, unread %d, want %d", p.More, unread(t, a), left)
			}
			for _, max := range c.aboveTheCaps {
				p, s := readCapped(t, a, map[string]any{"max_bytes": max})
				if len(s) > c.cap || len(s) < c.cap-1500 || p.Note != capNote(c.cap) || !p.MarkedSeen {
					t.Fatalf("max_bytes %d: %d bytes, note %q", max, len(s), p.Note)
				}
				left -= len(p.Messages)
				if p.More != left || unread(t, a) != left {
					t.Fatalf("max_bytes %d: more %d, unread %d, want %d", max, p.More, unread(t, a), left)
				}
			}
			// At the cap exactly, or below it, no note.
			if p, s := readCapped(t, a, map[string]any{"max_bytes": c.cap}); len(s) > c.cap || p.Note != "" {
				t.Fatalf("at the cap: %d bytes, note %q", len(s), p.Note)
			}
		})
	}
}

// [U4, D74] At caps near the floor and above it, with and without a
// request above the cap, the budget sent is never under 512 and a note is
// added only when the result with it fits the cap: every result is within
// the cap as rendered, but one holding a single entry whose fields alone
// pass it (no text, within 32,000), and every entry is read once. Below
// the floor (a limit of 700) the cap is 512, with no note on top.
func TestUnderClaudeCodeNoNoteTakesAResultOverTheCapNearTheFloor(t *testing.T) {
	for _, c := range []struct {
		env string
		cap int
	}{{"700", 512}, {"1512", 512}, {"1550", 550}, {"1593", 593}, {"1594", 594}, {"8000", 7000}} {
		t.Run("limit="+c.env, func(t *testing.T) {
			cl := startDaemon(t)
			bob := newWriter(t, cl, "bob")
			a := claudeSession(t, cl, "alice", c.env)
			if cap, _ := inlineCap(harnessClaude, func(string) string { return c.env }); cap != c.cap {
				t.Fatalf("cap %d, want %d", cap, c.cap)
			}
			var sent, got []string
			for i := 0; i < 6; i++ {
				sent = append(sent, bob.send(localapi.SendReq{Text: fmt.Sprintf("m%d %s", i, strings.Repeat("z", []int{1, 30, 300}[i%3]))}))
			}
			for i := 0; i < 20 && len(got) < len(sent); i++ {
				for _, args := range []map[string]any{nil, {"max_bytes": c.cap + 1}, {"max_bytes": 40000}} {
					s, isErr := call(t, a, "inbox", args)
					if isErr {
						// Read above the cap, less its note: an entry that
						// needs the whole cap is refused naming it, as at
						// the ceiling (D72), and comes at it.
						if !strings.Contains(s, fmt.Sprintf("max_bytes %d", c.cap)) || args == nil {
							t.Fatalf("%v: %s", args, s)
						}
						s = ok(t, a, "inbox", map[string]any{"max_bytes": c.cap})
					}
					var p capped
					if err := json.Unmarshal([]byte(s), &p); err != nil {
						t.Fatal(err)
					}
					if len(s) > c.cap {
						if len(p.Messages) != 1 || p.Messages[0].Text != "" || p.Messages[0].TextCut == "" || len(s) > node.MaxInboxBytes {
							t.Fatalf("%v: %d bytes over the cap %d: %s", args, len(s), c.cap, s)
						}
					}
					for _, m := range p.Messages {
						got = append(got, m.ID)
					}
				}
			}
			if !slices.Equal(got, sent) {
				t.Fatalf("read %d of %d", len(got), len(sent))
			}
			for _, args := range []map[string]any{nil, {"max_bytes": 40000}} {
				if _, s := readCapped(t, a, args); len(s) > c.cap {
					t.Fatalf("an empty read of %d bytes over the cap %d", len(s), c.cap)
				}
			}
		})
	}
}

// hiddenText renders to about 9 bytes of result for each of its n
// characters: zero width spaces, each written \\u{200b}.
func hiddenText(n int) string { return strings.Repeat("\u200b", n) }

// [U4, D74] An entry that fits the ceiling but not the cap is never stuck.
// With the limit unset (cap 24,000), 3,000 zero width spaces render to
// about 27 KB: the default read is refused naming the cap, a read above
// the cap is refused naming it too, and a read at the cap returns it alone
// with its text cut and text_cut, marked seen. With the limit at 8,000
// (cap 7,000), a 5 KB text of control characters, quotes and backslashes
// renders to over 18 KB, and the default read, at the cap, returns it cut.
func TestUnderClaudeCodeAnEntryThatFitsTheCeilingButNotTheCapComesCutAtTheCap(t *testing.T) {
	cl := startDaemon(t)
	bob := newWriter(t, cl, "bob")
	a := claudeSession(t, cl, "alice", "")
	big := bob.send(localapi.SendReq{Text: hiddenText(3000)})
	bob.send(localapi.SendReq{Text: "after it"})
	for _, args := range []map[string]any{nil, {"max_bytes": 40000}} {
		ref := refusal(t, a, "inbox", args)
		if ref.Code != node.ErrorTooLarge || !strings.Contains(ref.Error, "max_bytes 24000 to read it with its text cut") {
			t.Fatalf("%v: refusal %+v", args, ref)
		}
		if n := unread(t, a); n != 2 {
			t.Fatalf("%d unread after a refusal, want 2", n)
		}
	}
	p, s := readCapped(t, a, map[string]any{"max_bytes": 24000})
	if len(s) > 24000 || len(p.Messages) != 1 || p.Messages[0].ID != big || p.Messages[0].TextCut == "" || p.Messages[0].Text == "" ||
		!p.MarkedSeen || p.More != 1 || p.Note != "" {
		t.Fatalf("at the cap: %d bytes: %+v", len(s), p)
	}
	if n := unread(t, a); n != 1 {
		t.Fatalf("%d unread, want 1", n)
	}

	b := claudeSession(t, cl, "carol", "8000")
	ctrl := controlText()
	id := bob.send(localapi.SendReq{Text: ctrl})
	shown, _ := json.Marshal(node.EscapeDisplayText(ctrl))
	if len(shown) <= 18000 {
		t.Fatalf("the control text renders to %d bytes only", len(shown))
	}
	p, s = readCapped(t, b, nil)
	for len(p.Messages) > 0 && p.Messages[0].ID != id {
		p, s = readCapped(t, b, nil) // what was sent before carol registered is not hers
	}
	if len(s) > 7000 || len(p.Messages) != 1 || p.Messages[0].TextCut == "" || !p.MarkedSeen {
		t.Fatalf("control text at a cap of 7,000: %d bytes: %+v", len(s), p)
	}
	t.Logf("control text: %d bytes sent, %d rendered; result %d", len(ctrl), len(shown), len(s))
}

// controlText is about 5 KB of control characters, quotes and
// backslashes, as long as a send takes: the text that grows most when
// shown.
func controlText() string {
	var b strings.Builder
	for i := 0; ; i++ {
		c := []string{"\x01", "\x02", "\x1b", "\"", "\\", "\x7f", "\x0b", "\x1f"}[i%8]
		next := b.String() + c
		if j, _ := json.Marshal(next); len(j) > core16K-64 {
			return b.String()
		}
		b.WriteString(c)
	}
}

// core16K is the largest text a send takes, JSON-escaped.
const core16K = 16384

// [U4, D74] An entry whose fields besides its text pass the cap is still
// delivered: it comes alone, without its text, with text_cut and a note
// that the result is larger than the harness shows inline, and is marked
// seen. No other result exceeds the cap: at a limit of 2,500 (cap 1,500),
// a backlog of ordinary messages and one addressed to 32 long labels is
// read whole, every result within the cap but the one holding that entry
// alone, every entry once, and unread is more after each page.
func TestUnderClaudeCodeAnEntryWhoseFieldsPassTheCapIsStillDeliveredAlone(t *testing.T) {
	cl := startDaemon(t)
	bob := newWriter(t, cl, "bob")
	a := claudeSession(t, cl, "alice", "2500")
	_, machine, _ := strings.Cut(agentID(t, a), "@")
	var many []string
	for i := 0; i < 23; i++ {
		many = append(many, fmt.Sprintf("%s%02d@%s", strings.Repeat("w", 40), i, machine))
	}
	r := rand.New(rand.NewPCG(74, 1))
	var sent []string
	wide := ""
	for i := 0; i < 40; i++ {
		req := localapi.SendReq{Text: textOf(r, i, []int{1, 80, 400, 2000}[r.IntN(4)])}
		if i == 17 {
			req = localapi.SendReq{Text: "to many", To: append([]string{agentID(t, a)}, many...)}
		}
		id := bob.send(req)
		if i == 17 {
			wide = id
		}
		sent = append(sent, id)
	}
	var got []string
	over := false
	for calls := 0; calls < 400; calls++ {
		s, isErr := call(t, a, "inbox", map[string]any{"max_bytes": []int{0, 512, 1500, 1501, 40000}[r.IntN(5)]})
		if isErr {
			var ref localapi.Refusal
			if json.Unmarshal([]byte(s), &ref); ref.Code != node.ErrorTooLarge {
				t.Fatalf("refusal %s", s)
			}
			// An entry too large for the budget asked: ask with what it names.
			s = ok(t, a, "inbox", map[string]any{"max_bytes": 1500})
		}
		var p capped
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			t.Fatal(err)
		}
		if len(s) > 1500 {
			if len(p.Messages) != 1 || p.Messages[0].ID != wide || p.Messages[0].Text != "" || p.Messages[0].TextCut == "" || p.Note != overNote ||
				!p.MarkedSeen || len(s) > node.MaxInboxBytes {
				t.Fatalf("a result of %d bytes over the cap: %s", len(s), s)
			}
			over = true
		}
		for _, m := range p.Messages {
			got = append(got, m.ID)
		}
		if n := unread(t, a); n != p.More {
			t.Fatalf("%d unread, more %d", n, p.More)
		}
		if p.More == 0 {
			break
		}
	}
	if !slices.Equal(got, sent) || !over {
		t.Fatalf("read %d of %d; the wide entry came over the cap: %v", len(got), len(sent), over)
	}
}

// [U4, D74] At a limit below the floor every entry is wider than the cap:
// each comes alone without its text, saying so, and none is skipped.
func TestUnderClaudeCodeBelowTheFloorEveryEntryIsStillDelivered(t *testing.T) {
	cl := startDaemon(t)
	bob := newWriter(t, cl, "bob")
	a := claudeSession(t, cl, "alice", "1000")
	var sent []string
	for i := 0; i < 5; i++ {
		sent = append(sent, bob.send(localapi.SendReq{Text: fmt.Sprintf("m%d %s", i, strings.Repeat("z", 300))}))
	}
	for i, id := range sent {
		p, s := readCapped(t, a, nil)
		if len(p.Messages) != 1 || p.Messages[0].ID != id || p.Messages[0].TextCut == "" || p.Note != overNote || !p.MarkedSeen || p.More != len(sent)-i-1 {
			t.Fatalf("entry %d: %s", i, s)
		}
	}
}

// [U4, D74] The worst text one can send, at random budgets, the default
// and above the cap: under Claude Code with the limit unset, every result
// is at most 24,000 bytes as rendered, escapes included; every entry is
// read once, in order; only what a result returned is marked seen.
func TestUnderClaudeCodeNoResultOfTheWorstTextExceedsTheCapInBytes(t *testing.T) {
	cl := startDaemon(t)
	bob := newWriter(t, cl, "bob")
	a := claudeSession(t, cl, "alice", "")
	r := rand.New(rand.NewPCG(74, 2))
	worst := []func(int) string{
		hiddenText,
		func(n int) string { return strings.Repeat("\x7f", n) },
		func(n int) string { return strings.Repeat("\"\\", n/2) },
		func(n int) string { return strings.Repeat("\x1b[31m\u202e", n/8) },
		func(n int) string { return strings.Repeat("!#%&*+-/<=>?@^_|~", n/17) },
		func(n int) string { return strings.Repeat("😀\u200d👩", n/11) },
		func(int) string { return controlText() },
	}
	var sent []string
	for i := 0; i < 45; i++ {
		n := []int{40, 900, 2700, 5400}[r.IntN(4)]
		text := worst[r.IntN(len(worst))](n)
		if j, _ := json.Marshal(text); len(j) > core16K-64 {
			text = text[:len(text)/4]
		}
		sent = append(sent, bob.send(localapi.SendReq{Text: fmt.Sprint(i) + text}))
	}
	var got []string
	maxSeen := 0
	for calls := 0; calls < 400 && len(got) < len(sent); calls++ {
		budget := []int{0, 24000, 40000, node.MinInboxBytes + r.IntN(40000)}[r.IntN(4)]
		before := unread(t, a)
		s, isErr := call(t, a, "inbox", map[string]any{"max_bytes": budget})
		if isErr {
			if !strings.Contains(s, node.ErrorTooLarge) {
				t.Fatalf("max %d: %s", budget, s)
			}
			if n := unread(t, a); n != before {
				t.Fatalf("a refusal changed unread from %d to %d", before, n)
			}
			continue
		}
		if len(s) > 24000 {
			t.Fatalf("max_bytes %d: result is %d bytes, over the cap", budget, len(s))
		}
		maxSeen = max(maxSeen, len(s))
		var p capped
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			t.Fatal(err)
		}
		for _, m := range p.Messages {
			got = append(got, m.ID)
		}
		if n := unread(t, a); n != p.More || n != before-len(p.Messages) {
			t.Fatalf("unread %d after a page of %d from %d, more %d", n, len(p.Messages), before, p.More)
		}
	}
	if !slices.Equal(got, sent) {
		t.Fatalf("read %d of %d", len(got), len(sent))
	}
	t.Logf("largest result %d bytes", maxSeen)
}

// [U4, D74] Another harness, or none named, keeps today's sizes whatever
// MAX_MCP_OUTPUT_TOKENS says: the default page of 18,000 and the ceiling's
// note at 32,000.
func TestAnotherHarnessKeepsTodaysSizesWhateverTheClaudeCodeLimit(t *testing.T) {
	for _, h := range []string{"codex", "mcp"} {
		t.Run(h, func(t *testing.T) {
			cl := startDaemon(t)
			bob := newWriter(t, cl, "bob")
			t.Setenv(envClaudeLimit, "2000")
			a := connectAs(t, cl, h, "alice")
			agentID(t, a)
			for i := 0; i < 80; i++ {
				bob.send(localapi.SendReq{Text: strings.Repeat("y", 1000)})
			}
			p, s := readCapped(t, a, nil)
			if len(s) > node.DefaultInboxBytes || len(s) < node.DefaultInboxBytes-1500 || p.Note != "" {
				t.Fatalf("default: %d bytes, note %q", len(s), p.Note)
			}
			p, s = readCapped(t, a, map[string]any{"max_bytes": 40000})
			if len(s) > node.MaxInboxBytes || len(s) < node.MaxInboxBytes-1500 || p.Note != ceilingNote {
				t.Fatalf("above the ceiling: %d bytes, note %q", len(s), p.Note)
			}
		})
	}
}

// [U4, D74, R7] Under Claude Code on a daemon that takes max_bytes but not
// ceiling (0.3.0), the server reads as today: no cap, so no entry
// is stranded by a cap the daemon would not cut at.
func TestUnderClaudeCodeADaemonWithoutTheCeilingReadsAsToday(t *testing.T) {
	c, home := startDaemonHome(t)
	bob := newWriter(t, c, "bob")
	pc := proxy(t, daemon.SockPath(home), func(path string, b []byte) []byte {
		if path == "/v1/inbox" {
			return strip(b, "ceiling")
		}
		return b
	}, func(path string, status int, b []byte) (int, []byte) {
		if status == http.StatusOK && path == "/v1/status" {
			var m map[string]any
			json.Unmarshal(b, &m)
			m["features"] = []string{localapi.FeatureInboxMaxBytes}
			b, _ = json.Marshal(m)
		}
		return status, b
	})
	a := claudeSession(t, pc, "alice", "8000")
	for i := 0; i < 40; i++ {
		bob.send(localapi.SendReq{Text: strings.Repeat("y", 1000)})
	}
	p, s := readCapped(t, a, nil)
	if len(s) > node.DefaultInboxBytes || len(s) < 8000 || p.Note != "" || !p.MarkedSeen {
		t.Fatalf("%d bytes, note %q", len(s), p.Note)
	}
}

// downgradable is a proxy to a daemon that, once down is set, answers as
// 0.3.0 does: its inbox takes max_bytes but not ceiling, and its status
// does not list inbox_ceiling.
func downgradable(t *testing.T, home string, down *atomic.Bool) *localapi.Client {
	return proxy(t, daemon.SockPath(home), func(path string, b []byte) []byte {
		if down.Load() && path == "/v1/inbox" {
			return strip(b, "ceiling")
		}
		return b
	}, func(path string, status int, b []byte) (int, []byte) {
		if down.Load() && status == http.StatusOK && path == "/v1/status" {
			var m map[string]any
			json.Unmarshal(b, &m)
			m["features"] = []string{localapi.FeatureInboxMaxBytes}
			b, _ = json.Marshal(m)
		}
		return status, b
	})
}

// [U4, D74, R7] The cap was learnt, then the daemon is replaced by one
// that ignores ceiling (0.3.0 after a downgrade): an entry over the cap is
// refused once, at the cap, and the next read, which asks the daemon's
// status again, reads without the cap and delivers it.
func TestUnderClaudeCodeADaemonThatStopsTakingTheCeilingDoesNotStrandAnEntry(t *testing.T) {
	c, home := startDaemonHome(t)
	bob := newWriter(t, c, "bob")
	var down atomic.Bool
	a := claudeSession(t, downgradable(t, home, &down), "alice", "8000")
	if p, _ := readCapped(t, a, nil); len(p.Messages) != 0 {
		t.Fatal("not empty")
	}
	down.Store(true)
	big := bob.send(localapi.SendReq{Text: strings.Repeat("a", 9000)})
	if r := refusal(t, a, "inbox", nil); r.Code != node.ErrorTooLarge {
		t.Fatalf("first read %+v", r)
	}
	p, s := readCapped(t, a, nil)
	if len(p.Messages) != 1 || p.Messages[0].ID != big || p.Messages[0].TextCut != "" || !p.MarkedSeen || len(s) > node.DefaultInboxBytes {
		t.Fatalf("second read: %d bytes: %+v", len(s), p)
	}
}

// [U4, D74] A status that cannot be read does not block reading: the read
// goes on without the cap, and the cap is learnt on a later read.
func TestUnderClaudeCodeAFailedStatusReadsWithoutTheCap(t *testing.T) {
	c, home := startDaemonHome(t)
	bob := newWriter(t, c, "bob")
	var fail atomic.Bool
	pc := proxy(t, daemon.SockPath(home), func(path string, b []byte) []byte { return b },
		func(path string, status int, b []byte) (int, []byte) {
			if fail.Load() && path == "/v1/status" {
				return http.StatusInternalServerError, []byte(`{"error":"down for a test","code":"internal"}`)
			}
			return status, b
		})
	a := claudeSession(t, pc, "alice", "8000")
	fail.Store(true)
	for i := 0; i < 40; i++ {
		bob.send(localapi.SendReq{Text: strings.Repeat("y", 1000)})
	}
	if _, s := readCapped(t, a, nil); len(s) <= 7000 || len(s) > node.DefaultInboxBytes {
		t.Fatalf("without status: %d bytes", len(s))
	}
	fail.Store(false)
	if _, s := readCapped(t, a, nil); len(s) > 7000 {
		t.Fatalf("with status: %d bytes", len(s))
	}
}

// [U4, D74] Errors repeat at most a short start of a caller's input: an
// unknown room named with 40 KB comes back in a few hundred bytes.
func TestAnInboxErrorDoesNotEchoALongInputWhole(t *testing.T) {
	cl := startDaemon(t)
	a := claudeSession(t, cl, "alice", "")
	for _, args := range []map[string]any{{"room": strings.Repeat("r", 40000)}, {"room": strings.Repeat("r", 40000), "peek": true}} {
		s, isErr := call(t, a, "inbox", args)
		if !isErr || len(s) > 600 || !strings.Contains(s, "more bytes") {
			t.Fatalf("%d bytes: %.300s", len(s), s)
		}
	}
}

// [U2, U4, D74] Under Claude Code, a read that sets a level in eight
// rooms and waits: its empty page fits the cap and the level is set; an
// entry that arrives during the wait fits the cap alone but not beside the
// list of rooms. It comes whole, without the list, and the result stays
// within the cap, with a note only if that fits.
func TestUnderClaudeCodeAnEntryArrivingDuringALevelReadsWaitComesWhole(t *testing.T) {
	cl := startDaemon(t)
	for i := 0; i < 8; i++ {
		createRoom(t, cl, fmt.Sprintf("room-with-a-long-label-%d", i))
	}
	bob := newWriter(t, cl, "bob")
	a := claudeSession(t, cl, "alice", "2500")
	alice := agentID(t, a)
	text := strings.Repeat("v", 700)
	done := make(chan string, 1)
	go func() {
		time.Sleep(500 * time.Millisecond)
		done <- bob.send(localapi.SendReq{Text: text, To: []string{alice}})
	}()
	p, s := readCapped(t, a, map[string]any{"level": "addressed", "wait_seconds": 5})
	id := <-done
	if len(p.Messages) != 1 || p.Messages[0].ID != id || p.Messages[0].Text != text || p.Messages[0].TextCut != "" || len(p.Levels) != 0 || len(s) > 1500 || !p.MarkedSeen {
		t.Fatalf("%d bytes: %s", len(s), s)
	}
	if p.Note != "" && p.Note != levelsNotNote {
		t.Fatalf("note %q", p.Note)
	}
	t.Logf("%d bytes, note %q", len(s), p.Note)
	lv := parseLevels(t, ok(t, a, "inbox", map[string]any{"peek": true, "level": "addressed"}))
	if len(lv.Levels) == 0 || lv.Levels[0].Level != "addressed" {
		t.Fatalf("levels %+v", lv.Levels)
	}
}
