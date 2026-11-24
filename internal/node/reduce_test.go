package node

// Reducing a failing random schedule by hand is slow: a failure of the
// random differential tests logs its operations (A2P2_MODEL_DUMP=1), and
// TestReduceAFailingSchedule shrinks them to a schedule that still fails
// with a message containing A2P2_MODEL_MATCH (A2P2_MODEL_REDUCE=file).
// Not a check: it is skipped unless asked for.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/model/delivery"
)

// dumpOps writes ops one per line, as parseOps reads them.
func dumpOps(ops []op) string {
	var b strings.Builder
	for _, o := range ops {
		fmt.Fprintf(&b, "%d|%s|%s|%s|%s|%v|%v|%d|%d|%v\n", o.kind, o.machine, o.server, o.agent, o.to,
			o.everyone, o.ask, o.target, o.n, o.same)
	}
	return b.String()
}

func parseOps(t testing.TB, text string) []op {
	var out []op
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		f := strings.Split(line, "|")
		if len(f) != 10 {
			continue
		}
		num := func(s string) int {
			n, err := strconv.Atoi(s)
			if err != nil {
				t.Fatalf("%q: %v", line, err)
			}
			return n
		}
		out = append(out, op{kind: opKind(num(f[0])), machine: delivery.MachineID(f[1]), server: delivery.MachineID(f[2]),
			agent: f[3], to: f[4], everyone: f[5] == "true", ask: f[6] == "true", target: num(f[7]), n: num(f[8]),
			same: f[9] == "true"})
	}
	return out
}

// goOps writes ops as Go literals, for a scenario test.
func goOps(ops []op) string {
	var b strings.Builder
	for _, o := range ops {
		fmt.Fprintf(&b, "\t%#v,\n", o)
	}
	return strings.ReplaceAll(b.String(), "node.op", "")
}

// catcher is a testing.TB whose failures end one replay, not the test.
type catcher struct {
	testing.TB
	msg string
}

type caught struct{}

func (c *catcher) Fatalf(format string, a ...any) { c.msg = fmt.Sprintf(format, a...); panic(caught{}) }
func (c *catcher) Fatal(a ...any)                 { c.msg = fmt.Sprint(a...); panic(caught{}) }
func (c *catcher) Errorf(format string, a ...any) { c.msg = fmt.Sprintf(format, a...); panic(caught{}) }
func (c *catcher) Logf(string, ...any)            {}

// replayPull runs ops through the real-exchange harness of the random test
// and settles; it returns the failure message, or "".
func replayPull(t *testing.T, ops []op) (msg string) {
	c := &catcher{TB: t}
	founders := map[delivery.MachineID][]string{"M": {"alice", "bob"}, "P": {"pat"}, "Q": {"quinn"}}
	later := map[delivery.MachineID][]string{"N": {"nina"}, "O": {"otto"}}
	var h *harness
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(caught); !ok {
				panic(r)
			}
			msg = c.msg
		}
		if h != nil {
			func() { defer func() { _ = recover() }(); h.close() }()
		}
	}()
	h = newPullHarness(c, founders, later)
	for _, o := range ops {
		h = h.apply(o)
	}
	if !h.stopped {
		h.px.settle(h)
	}
	return ""
}

func TestReduceAFailingSchedule(t *testing.T) {
	file, match := os.Getenv("A2P2_MODEL_REDUCE"), os.Getenv("A2P2_MODEL_MATCH")
	if file == "" {
		t.Skip("set A2P2_MODEL_REDUCE to a file of operations a failure logged")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	ops := parseOps(t, string(b))
	fails := func(ops []op) bool {
		m := replayPull(t, ops)
		return m != "" && strings.Contains(m, match)
	}
	if !fails(ops) {
		t.Fatalf("the %d operations do not fail with %q", len(ops), match)
	}
	// Remove chunks, halving their size, until no single operation can go.
	for chunk := max(len(ops)/2, 1); ; {
		removed := false
		for i := 0; i+chunk <= len(ops); {
			try := append(append([]op{}, ops[:i]...), ops[i+chunk:]...)
			if fails(try) {
				ops, removed = try, true
				continue
			}
			i += chunk
		}
		if !removed {
			if chunk == 1 {
				break
			}
			chunk /= 2
		}
	}
	t.Logf("%d operations; failure:\n%s\n%s", len(ops), replayPull(t, ops), goOps(ops))
}
