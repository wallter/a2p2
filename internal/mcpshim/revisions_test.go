package mcpshim

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// C8: which MCP revisions the built shim speaks. The binary is built and
// launched as a harness launches it (`a2p2 mcp` over stdio, against a
// daemon in a temporary home); a client opens it with each revision the SDK
// supports and with ones it does not, and the negotiated revision is
// checked. The 2026-07-28 revision opens with server/discover, the older
// ones with initialize; an unknown old revision gets 2025-11-25 (the
// newest initialize revision) and an unknown newer one 2026-07-28. The
// table is logged so it can be recorded with the harness notes.
func TestTheBuiltShimNegotiatesEachSupportedMCPRevision(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "a2p2")
	build := exec.Command("go", "build", "-o", bin, "github.com/wallter/a2p2/cmd/a2p2")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	_, home := startDaemonHome(t)
	want := map[string]string{"2099-01-01": "2026-07-28", "2024-01-01": "2025-11-25"}
	revisions := mcp.SupportedProtocolVersions()
	for _, v := range revisions {
		want[v] = v
	}
	var table []string
	for _, v := range append(revisions, "2099-01-01", "2024-01-01") {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.Command(bin, "mcp", "--harness", "test", "--name", "rev")
		cmd.Env = append(os.Environ(), "A2P2_HOME="+home, "HOME="+t.TempDir(), "CLAUDE_PROJECT_DIR=")
		cl := mcp.NewClient(&mcp.Implementation{Name: "revisions", Version: "1"}, nil)
		cs, err := cl.Connect(ctx, &mcp.CommandTransport{Command: cmd}, &mcp.ClientSessionOptions{ProtocolVersion: v})
		if err != nil {
			cancel()
			t.Fatalf("open with %s: %v", v, err)
		}
		got := cs.InitializeResult().ProtocolVersion
		tools, err := cs.ListTools(ctx, nil)
		if err != nil || len(tools.Tools) != 7 {
			t.Errorf("%s: tools/list: %v", v, err)
		}
		if got != want[v] {
			t.Errorf("asked for %s: negotiated %s, want %s", v, got, want[v])
		}
		table = append(table, v+" -> "+got)
		cs.Close()
		cancel()
	}
	t.Logf("SDK supports %s; negotiated: %s", strings.Join(revisions, ", "), strings.Join(table, "; "))
}
