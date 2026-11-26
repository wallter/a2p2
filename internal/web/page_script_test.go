package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/node"
)

// TestPageScriptDrawsTheSidebarInNode runs static/app.js, unchanged, in Node
// against the real handler, with a small stand-in for the DOM
// (testdata/page_harness.js), and requires that the first refresh finishes
// without an error and that the sidebar lists every room. A second part
// drives a recipient chosen from the author list across a poll that no
// longer holds that author's messages.
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
	if runtime.GOOS == "windows" {
		t.Skip("not run on Windows: the harness has not been tried there (node paths and the Windows job are untested)")
	}
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

	out, err := exec.Command(nodeBin, "testdata/page_harness.js", "static/index.html", "static/app.js", e.srv.URL, "c="+e.ss.Code(), ids[0]).Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		Errors           []string `json:"errors"`
		AppHiddenBefore  bool     `json:"appHiddenBefore"`
		AppHidden        bool     `json:"appHidden"`
		ConnectionHidden bool     `json:"connectionHidden"`
		Connection       string   `json:"connection"`
		RoomIDs          []string `json:"roomIds"`
		Rooms            []string `json:"rooms"`
		Roomhead         string   `json:"roomhead"`
		Stream           string   `json:"stream"`
		Node             string   `json:"node"`
		ToOptions        []string `json:"toOptions"`
		Scenario         struct {
			Author        string           `json:"author"`
			ToAfterReload string           `json:"toAfterReload"`
			Posts         []map[string]any `json:"posts"`
			RefusedPosts  []map[string]any `json:"refusedPosts"`
			RefusedErr    string           `json:"refusedErr"`
		} `json:"scenario"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("harness output %q: %v", out, err)
	}
	if len(got.Errors) > 0 {
		t.Errorf("errors: %v", got.Errors)
	}
	if !got.AppHiddenBefore || got.AppHidden {
		t.Errorf("the app must start hidden (index.html) and be shown by the script: before=%v after=%v", got.AppHiddenBefore, got.AppHidden)
	}
	if !got.ConnectionHidden {
		t.Errorf("the page shows its status line: %q", got.Connection)
	}
	st := e.state(t)
	want := []string{}
	for _, r := range st.Status.Rooms {
		want = append(want, r.ID)
	}
	have := append([]string(nil), got.RoomIDs...)
	sort.Strings(want)
	sort.Strings(have)
	if strings.Join(have, ",") != strings.Join(want, ",") || len(want) < 3 {
		t.Fatalf("sidebar room ids %q, the server has %q", have, want)
	}
	same := 0
	for i, r := range got.Rooms {
		if strings.HasPrefix(r, "team (") && strings.Contains(r, got.RoomIDs[i]) {
			same++
		}
	}
	if same != 2 {
		t.Errorf("rooms with one label should show their ids: %q", got.Rooms)
	}
	if !strings.Contains(got.Node, "alice") {
		t.Errorf("the agent list is missing alice: %q", got.Node)
	}
	if got.Roomhead != "team ("+ids[0]+")" {
		t.Errorf("room heading %q", got.Roomhead)
	}
	if !strings.Contains(got.Stream, "hello from alice") {
		t.Errorf("the stream does not show alice's message: %q", got.Stream)
	}

	// A recipient chosen before a poll that no longer holds their messages
	// stays the recipient: the message is not sent to the whole room.
	sc := got.Scenario
	if sc.Author == "" || !strings.Contains(sc.Author, "alice") {
		t.Fatalf("the recipient list has no author to choose: %q", got.ToOptions)
	}
	if sc.ToAfterReload != sc.Author {
		t.Errorf("after a reload without %q the recipient is %q", sc.Author, sc.ToAfterReload)
	}
	if len(sc.Posts) != 1 {
		t.Fatalf("posts after choosing a recipient: %v", sc.Posts)
	}
	if to, _ := sc.Posts[0]["to"].([]any); len(to) != 1 || to[0] != sc.Author {
		t.Errorf("the message was sent without its recipient: %v", sc.Posts[0])
	}
	if len(sc.RefusedPosts) != 0 || sc.RefusedErr == "" {
		t.Errorf("a draft whose recipient is lost must be refused with a reason: posts %v, error %q", sc.RefusedPosts, sc.RefusedErr)
	}
}
