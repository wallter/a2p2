//go:build bench

package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wallter/a2p2/internal/localapi"
)

// shim is one real `a2p2 mcp` process, driven over its stdin and stdout
// as a harness drives it.
type shim struct {
	cs *mcp.ClientSession
}

func startShim(t testing.TB, p *proc, label string) *shim {
	t.Helper()
	cmd := exec.Command(p.bin, "mcp", "--harness", "bench", "--name", label)
	cmd.Env = env(p.home)
	cmd.Dir = filepath.Join(p.home, "fakehome")
	cl := mcp.NewClient(&mcp.Implementation{Name: "bench", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, err := cl.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return &shim{cs: cs}
}

func (s *shim) tool(name string, args map[string]any) (string, error) {
	res, err := s.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", err
	}
	out := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out += tc.Text
		}
	}
	if res.IsError {
		return "", &localapi.Error{Msg: out}
	}
	return out, nil
}

// M4: send and inbox through the real MCP shim process, on new daemons
// every m1Chunk sends of each phase, as M1, so the history matches M1's.
func TestM4MCPShim(t *testing.T) {
	n := scaled(3000, 300)
	var a m4acc
	for done := 0; done < n; done += m1Chunk {
		m4Chunk(t, min(m1Chunk, n-done), &a)
	}
	cs := fmt.Sprintf(" history<=%d", 2*m1Chunk)
	result(t, "M4 shim send", "200B serial (MCP tools/call)"+cs, &a.send, a.sendEl, "sends/s")
	result(t, "M4 shim whoami", "serial", &a.who, a.whoEl, "calls/s")
	result(t, "M4 shim send->visible", "200B to=bob, inbox tool waiting"+cs, &a.vis, a.visEl, "msgs/s (serial)")
	result(t, "M4 shim send->visible", "[send tool only]", &a.visSend, 0, "-")
}

type m4acc struct {
	send, who, vis, visSend lat
	sendEl, whoEl, visEl    time.Duration
}

func m4Chunk(t *testing.T, n int, a *m4acc) {
	p := startDaemon(t, "m4")
	defer func() { p.stop(); p.home = "" }()
	sa, sb := startShim(t, p, "alice"), startShim(t, p, "bob")
	bob := p.agent("bob") // the same label the shim registered: its id
	// warm
	for i := 0; i < 20; i++ {
		if _, err := sa.tool("send", map[string]any{"text": "warm"}); err != nil {
			t.Fatal(err)
		}
	}
	// send through the shim, serial
	start := time.Now()
	for i := 0; i < n; i++ {
		t0 := time.Now()
		if _, err := sa.tool("send", map[string]any{"text": text(200)}); err != nil {
			t.Fatal(err)
		}
		a.send.add(time.Since(t0))
	}
	a.sendEl += time.Since(start)
	// the cheapest tool: whoami (register is cached; level and unread)
	start = time.Now()
	for i := 0; i < n/3; i++ {
		t0 := time.Now()
		if _, err := sa.tool("whoami", map[string]any{}); err != nil {
			t.Fatal(err)
		}
		a.who.add(time.Since(t0))
	}
	a.whoEl += time.Since(start)
	// send to visible: alice's send tool, bob's waiting inbox tool (which
	// also confirms seen inside the same tool call)
	type vis struct {
		ids []string
		at  time.Time
	}
	got := make(chan vis, 1<<12)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			out, err := sb.tool("inbox", map[string]any{"wait_seconds": 2})
			now := time.Now()
			if err != nil {
				t.Errorf("inbox: %v", err)
				return
			}
			var r struct {
				Messages []struct {
					ID string `json:"id"`
				} `json:"messages"`
			}
			if err := json.Unmarshal([]byte(out), &r); err != nil {
				t.Errorf("inbox: %v", err)
				return
			}
			v := vis{at: now}
			for _, m := range r.Messages {
				v.ids = append(v.ids, m.ID)
			}
			if len(v.ids) > 0 {
				got <- v
			}
		}
	}()
	for i := 0; i < n; i++ {
		time.Sleep(2 * time.Millisecond)
		t0 := time.Now()
		out, err := sa.tool("send", map[string]any{"text": text(200), "to": []string{bob}})
		if err != nil {
			t.Fatal(err)
		}
		a.visSend.add(time.Since(t0))
		var m struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatal(err)
		}
		found := false
		for !found {
			select {
			case v := <-got:
				for _, id := range v.ids {
					if id == m.ID {
						found = true
						a.vis.add(v.at.Sub(t0))
						a.visEl += v.at.Sub(t0)
					}
				}
			case <-time.After(30 * time.Second):
				t.Fatal("never visible")
			}
		}
	}
	close(stop)
	<-done
	sa.cs.Close()
	sb.cs.Close()
}
