package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/node"
)

// TestPageScriptDrawsTheSidebarInNode runs static/app.js, unchanged, in Node
// against the real handler, with a small stand-in for the DOM
// (testdata/page_harness.js), and requires that the first refresh finishes
// without an error and that the sidebar lists every room.
//
// It exists because 0.2.2 shipped a page that never drew: a `const roomLabel`
// inside renderSide made an earlier call to the file's own roomLabel throw
// "Cannot access 'roomLabel' before initialization", which the page shows as
// "Could not load page data". No Go test ran the script, so nothing saw it.
//
// It needs node on PATH. Without it the test is skipped, unless
// A2P2_REQUIRE_NODE is set (make web-js sets it, and prints its own skip
// line when node is missing).
func TestPageScriptDrawsTheSidebarInNode(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("A2P2_REQUIRE_NODE") != "" {
			t.Fatal("A2P2_REQUIRE_NODE is set and node is not on PATH")
		}
		t.Skip("node is not on PATH: the page script was not run (make web-js reports this too)")
	}
	e := setup(t)
	alice := e.agent(t, "alice")
	// Two rooms with one label: the sidebar must tell them apart.
	ids := []string{}
	for i := 0; i < 2; i++ {
		r, err := e.s.CreateRoom(e.ctx, "team")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	if _, err := e.s.Send(e.ctx, node.SendInput{Agent: alice, Room: ids[0], Text: "hello from alice"}); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(nodeBin, "testdata/page_harness.js", "static/app.js", e.srv.URL, tok).Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		Errors     []string `json:"errors"`
		Connection string   `json:"connection"`
		AppHidden  bool     `json:"appHidden"`
		Rooms      []string `json:"rooms"`
		Roomhead   string   `json:"roomhead"`
		Stream     string   `json:"stream"`
		Node       string   `json:"node"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("harness output %q: %v", out, err)
	}
	if len(got.Errors) > 0 {
		t.Errorf("errors: %v", got.Errors)
	}
	if got.Connection != "" {
		t.Errorf("the page shows its status line: %q", got.Connection)
	}
	if got.AppHidden {
		t.Error("the app is still hidden")
	}
	st := e.state(t)
	if len(got.Rooms) != len(st.Status.Rooms) || len(got.Rooms) < 3 {
		t.Fatalf("sidebar rooms %q, the server has %d", got.Rooms, len(st.Status.Rooms))
	}
	same := 0
	for _, r := range got.Rooms {
		if strings.Contains(r, "team (") {
			same++
		}
	}
	if same != 2 {
		t.Errorf("rooms with one label should show their ids: %q", got.Rooms)
	}
	if !strings.Contains(got.Node, "alice") {
		t.Errorf("the agent list is missing alice: %q", got.Node)
	}
}
