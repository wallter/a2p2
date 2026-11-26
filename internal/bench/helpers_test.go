//go:build bench

package bench

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/daemon"
	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// ---- results ----

// lat collects latencies; safe for concurrent use.
type lat struct {
	mu sync.Mutex
	d  []time.Duration
}

func (l *lat) add(d time.Duration) { l.mu.Lock(); l.d = append(l.d, d); l.mu.Unlock() }

func (l *lat) pct(p float64) time.Duration {
	if len(l.d) == 0 {
		return 0
	}
	i := int(p*float64(len(l.d))+0.5) - 1
	return l.d[max(0, min(i, len(l.d)-1))]
}

// result prints one row: what, case, count, p50/p95/p99/max and the rate
// (ops per second over wall time elapsed, 0 when it means nothing), and
// appends it to $A2P2_BENCH_OUT as tab-separated values.
func result(t testing.TB, what, cs string, l *lat, elapsed time.Duration, unit string) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	slices.Sort(l.d)
	rate := 0.0
	if elapsed > 0 {
		rate = float64(len(l.d)) / elapsed.Seconds()
	}
	if unit == "" {
		unit = "ops/s"
	}
	ms := func(d time.Duration) string { return fmt.Sprintf("%.3f", float64(d.Microseconds())/1000) }
	row := []string{what, cs, fmt.Sprint(len(l.d)), ms(l.pct(.50)), ms(l.pct(.95)), ms(l.pct(.99)), ms(l.pct(1)), fmt.Sprintf("%.0f", rate), unit}
	fmt.Printf("RESULT  %-22s %-34s n=%-6s p50=%-8s p95=%-8s p99=%-8s max=%-9s %s %s\n", row[0], row[1], row[2], row[3], row[4], row[5], row[6], row[7], row[8])
	if out := os.Getenv("A2P2_BENCH_OUT"); out != "" {
		f, err := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintln(f, strings.Join(row, "\t"))
			f.Close()
		}
	}
}

// note prints a free-text line that goes with the results.
func note(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	fmt.Println("NOTE    " + s)
	if out := os.Getenv("A2P2_BENCH_OUT"); out != "" {
		f, err := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintln(f, "#\t"+s)
			f.Close()
		}
	}
}

// text is n bytes of plain ASCII text.
func text(n int) string {
	const s = "the quick brown fox jumps over the lazy dog 0123456789 "
	return strings.Repeat(s, n/len(s)+1)[:n]
}

// ---- real daemons from the built binary ----

// proc is one real daemon in a throwaway home.
type proc struct {
	t    testing.TB
	bin  string
	home string
	c    *localapi.Client
	node string
	sock string // an in-process daemon's socket (fanDaemon); home is then empty
}

func binary(t testing.TB) string {
	t.Helper()
	bin := os.Getenv("A2P2_BENCH_BIN")
	if bin == "" {
		t.Skip("A2P2_BENCH_BIN is not set: run scripts/bench.sh")
	}
	if !filepath.IsAbs(bin) {
		t.Fatalf("A2P2_BENCH_BIN must be an absolute path, not %s", bin)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatal(err)
	}
	return bin
}

// tmpHome makes a home with mktemp -d /tmp/a2p2x.XXXXXX semantics, mode 700.
func tmpHome(t testing.TB) string {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "a2p2x.")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

// env is the environment of every command run against home: A2P2_HOME
// and HOME both inside the throwaway home, nothing else of a2p2's.
func env(home string) []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "A2P2_") || k == "HOME" || k == "CLAUDE_PROJECT_DIR" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "A2P2_HOME="+home, "HOME="+filepath.Join(home, "fakehome"))
}

func (p *proc) run(args ...string) (string, error) {
	cmd := exec.Command(p.bin, args...)
	cmd.Env = env(p.home)
	cmd.Dir = filepath.Join(p.home, "fakehome")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// startDaemon starts a daemon named name from the binary in a new home;
// it is stopped with `a2p2 stop` and its home removed at cleanup.
func startDaemon(t testing.TB, name string) *proc {
	t.Helper()
	p := &proc{t: t, bin: binary(t), home: tmpHome(t)}
	if err := os.Mkdir(filepath.Join(p.home, "fakehome"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := daemon.SaveConfig(p.home, daemon.Config{Name: name, UI: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stop)
	if out, err := p.run("status"); err != nil { // starts the daemon in the background
		t.Fatalf("status: %v\n%s", err, out)
	}
	p.c = localapi.NewClient(daemon.SockPath(p.home))
	var st localapi.Status
	if err := p.c.Call(context.Background(), "status", nil, &st); err != nil {
		t.Fatal(err)
	}
	p.node = st.Node
	return p
}

func (p *proc) stop() {
	home := p.home
	if home == "" {
		return
	}
	_, _ = p.run("stop")
	ok := false
	for i := 0; i < 80; i++ {
		out, _ := p.run("stop")
		if strings.TrimSpace(out) == "not running" {
			ok = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !ok {
		p.t.Errorf("daemon under %s did not stop; left in place", home)
		return
	}
	if err := os.RemoveAll(home); err != nil {
		p.t.Errorf("remove %s: %v", home, err)
	}
}

// grepLog counts lines of the daemon's log containing s.
func (p *proc) grepLog(s string) int {
	f, err := os.Open(filepath.Join(p.home, "daemon.log"))
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if strings.Contains(sc.Text(), s) {
			n++
		}
	}
	return n
}

// tailLog prints the last n lines of the daemon's log (no message text is
// logged by the daemon).
func (p *proc) tailLog(n int) {
	b, err := os.ReadFile(filepath.Join(p.home, "daemon.log"))
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	fmt.Printf("--- last lines of the log of %s\n%s\n", filepath.Base(p.home), strings.Join(lines[max(0, len(lines)-n):], "\n"))
}

// client is a new client of p's socket: one per simulated agent, as each
// agent's shim process has its own.
func (p *proc) client() *localapi.Client {
	if p.sock != "" {
		return localapi.NewClient(p.sock)
	}
	return localapi.NewClient(daemon.SockPath(p.home))
}

func call[T any](t testing.TB, c *localapi.Client, name string, req any) T {
	t.Helper()
	var out T
	if err := c.Call(context.Background(), name, req, &out); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

func (p *proc) agent(label string) string {
	return call[store.Agent](p.t, p.c, "register", localapi.RegisterReq{Harness: "bench", Label: label, Cwd: "/w/" + label}).ID
}

// seen confirms every entry of an inbox answer.
func seen(c *localapi.Client, agent string, es []localapi.Entry) error {
	if len(es) == 0 {
		return nil
	}
	req := localapi.SeenReq{Agent: agent}
	for _, e := range es {
		req.Entries = append(req.Entries, localapi.SeenEntry{Key: e.Key, Version: e.Version})
	}
	var res localapi.SeenResp
	if err := c.Call(context.Background(), "seen", req, &res); err != nil {
		return err
	}
	for _, r := range res.Results {
		if !r.OK {
			return fmt.Errorf("seen %s: %s", r.Key, r.Reason)
		}
	}
	return nil
}

func freePort(t testing.TB) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// setNet records a networking decision, as a2p2 net does after a person
// says yes, and waits for the daemon to apply it.
func (p *proc) setNet(state, listen string) {
	p.t.Helper()
	res := call[localapi.NetResp](p.t, p.c, "net", localapi.NetReq{State: state, Listen: listen})
	eventually(p.t, "net "+state, func() bool {
		var st localapi.Status
		err := p.c.Call(context.Background(), "status", localapi.Empty{}, &st)
		return err == nil && st.Network.Request == res.Request && st.Network.Reason != "being applied"
	})
}

func eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); !cond(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// join takes j into room through admin a over loopback, as the daemon
// tests do: invite, preview, accept, the admin's confirm.
func join(t testing.TB, a, j *proc, room string) {
	t.Helper()
	tok := call[localapi.InviteResp](t, a.c, "room/invite", localapi.InviteReq{Room: room})
	pv := call[localapi.PreviewResp](t, j.c, "room/join", localapi.JoinReq{Token: tok.Token})
	if pv.State != node.JoinStatePreviewed {
		t.Fatalf("preview = %+v", pv)
	}
	acc := call[localapi.AcceptResp](t, j.c, "room/join/accept", localapi.AcceptReq{Token: tok.Token, Preview: pv.Preview})
	if acc.State != node.JoinStateAccepted {
		t.Fatalf("accept = %+v", acc)
	}
	var qid string
	eventually(t, "the join request on the admin", func() bool {
		qs := call[localapi.RequestsResp](t, a.c, "requests", localapi.Empty{})
		for _, q := range qs.Requests {
			if q.Node == j.node {
				qid = q.ID
				return true
			}
		}
		return false
	})
	cf := call[localapi.ConfirmResp](t, a.c, "confirm", localapi.ConfirmReq{ID: qid, Approve: true})
	if cf.State != store.RequestApproved {
		t.Fatalf("confirm = %+v", cf)
	}
	eventually(t, "the joiner to hold the room", func() bool {
		var rl localapi.RoomsResp
		err := j.c.Call(context.Background(), "room/list", localapi.RoomListReq{Room: room}, &rl)
		return err == nil && len(rl.Rooms) == 1 && slices.ContainsFunc(rl.Rooms[0].Members, func(m localapi.Member) bool { return m.Node == j.node })
	})
}

// scaled is n, or n/10 (at least min) when A2P2_BENCH_QUICK is set.
func scaled(n, least int) int {
	if os.Getenv("A2P2_BENCH_QUICK") != "" {
		return max(n/10, least)
	}
	return n
}
