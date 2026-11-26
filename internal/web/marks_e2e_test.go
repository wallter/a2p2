package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/surfacetest"
)

// D65 after the second review, from records stored by another machine to
// what the page draws: app.js runs unchanged in node (testdata/
// page_harness.js) against the real handler. Each message and the
// replaced notice carry the marks decided from the full key, each in its
// own element; the notice of a message another agent of its author's
// machine replaced names the message's own label and says edited, as the
// message does; and text that imitates a mark is drawn only in a body.
func TestThePageDrawsTheMarksOfStoredRecordsOnMessagesAndNotices(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("not run on Windows: the harness has not been tried there")
	}
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("A2P2_REQUIRE_NODE") != "" {
			t.Fatal("A2P2_REQUIRE_NODE is set and node is not on PATH")
		}
		t.Skip("node is not on PATH: the page script was not run (make web-js reports this too)")
	}
	m := surfacetest.NewMarked(t)
	ss := testSessions()
	srv := httptest.NewServer(Handler(m.Here.S, ss))
	t.Cleanup(srv.Close)
	out, err := exec.Command(nodeBin, "testdata/page_harness.js", "static/index.html", "static/app.js", srv.URL, "c="+ss.Code(), m.Room).Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		Errors []string `json:"errors"`
		Drawn  []struct {
			Cls   string   `json:"cls"`
			From  string   `json:"from"`
			Marks []string `json:"marks"`
			Body  []string `json:"body"`
		} `json:"drawn"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("harness output %q: %v", out, err)
	}
	if len(got.Errors) > 0 {
		t.Fatalf("errors: %v", got.Errors)
	}
	far, here := "human@"+m.Far.ID[:8], "human@"+m.Here.ID[:8]
	other := []string{"another machine's person, not you", "edited", "changed by bot@" + m.Far.ID[:8]}
	want := map[string][]string{
		"msg " + far:                 other,
		"msg bot@" + m.Far.ID[:8]:    {},
		"msg local@" + m.Here.ID[:8]: {"this machine"},
		"msg mine " + here:           {"this machine's person"},
		"noteline " + far:            other,
	}
	seen := map[string]bool{}
	for _, d := range got.Drawn {
		k := d.Cls + " " + d.From
		w, ok := want[k]
		if !ok {
			t.Errorf("drawn %+v: not expected", d)
			continue
		}
		seen[k] = true
		if d.Marks == nil {
			d.Marks = []string{}
		}
		if !slices.Equal(d.Marks, w) {
			t.Errorf("%s: marks %q, want %q", k, d.Marks, w)
		}
		if len(d.Body) != 1 || !strings.Contains(d.Body[0], "[this machine's person] [edited]") {
			t.Errorf("%s: body %q: the imitation belongs in the body", k, d.Body)
		}
	}
	for k := range want {
		if !seen[k] {
			t.Errorf("not drawn: %s; drawn %+v", k, got.Drawn)
		}
	}
}

// The page's trace dialog writes the author and each handler with their
// marks from the full key: human of another machine is another machine's
// person, an agent of this machine is this machine. It used to write the
// handler's short id alone and no author.
func TestThePageTraceMarksTheAuthorAndTheHandler(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("not run on Windows: the harness has not been tried there")
	}
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("A2P2_REQUIRE_NODE") != "" {
			t.Fatal("A2P2_REQUIRE_NODE is set and node is not on PATH")
		}
		t.Skip("node is not on PATH: the page script was not run (make web-js reports this too)")
	}
	p := surfacetest.NewPair(t)
	reader := p.Here.Agent(t, "reader")
	ask := p.Far.Send(t, node.SendInput{Agent: p.Far.S.HumanID(), Room: p.Room, Text: surfacetest.Imitation, To: []string{reader}})
	p.Deliver(t)
	p.Here.ReadAll(t, reader)
	if _, err := p.Here.S.Ack(context.Background(), reader, ask); err != nil {
		t.Fatal(err)
	}
	bot := p.Far.Agent(t, "bot")
	plain := p.Far.Send(t, node.SendInput{Agent: bot, Room: p.Room, Text: "plain"})
	p.Deliver(t)
	ss := testSessions()
	srv := httptest.NewServer(Handler(p.Here.S, ss))
	t.Cleanup(srv.Close)
	cmd := exec.Command(nodeBin, "testdata/page_harness.js", "static/index.html", "static/app.js", srv.URL, "c="+ss.Code(), p.Room)
	cmd.Env = append(os.Environ(), "TRACE_IDS="+ask+","+plain)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		Errors []string `json:"errors"`
		Traces []struct {
			ID    string     `json:"id"`
			From  []string   `json:"from"`
			Marks [][]string `json:"marks"`
		} `json:"traces"`
	}
	if err := json.Unmarshal(out, &got); err != nil || len(got.Traces) != 2 {
		t.Fatalf("harness output %q: %v", out, err)
	}
	if len(got.Errors) > 0 {
		t.Fatalf("errors: %v", got.Errors)
	}
	marked := func(ms [][]string) []string {
		var o []string
		for _, m := range ms {
			o = append(o, m...)
		}
		return o
	}
	a, b := got.Traces[0], got.Traces[1]
	if !slices.Equal(a.From, []string{"human@" + p.Far.ID[:8], reader}) ||
		!slices.Equal(marked(a.Marks), []string{"another machine's person, not you", "this machine"}) {
		t.Errorf("trace of the question: %+v", a)
	}
	if !slices.Equal(b.From, []string{"bot@" + p.Far.ID[:8]}) || len(marked(b.Marks)) != 0 {
		t.Errorf("trace of an agent's message elsewhere: %+v", b)
	}
}

// D65 after the third review: changed_by is drawn with its own mark when
// it is labelled human, so the person's replacement of a local agent's
// message reads as the person's and a local agent's replacement of the
// person's message does not.
func TestThePageMarksWhoChangedAMessageOnThisMachine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("not run on Windows: the harness has not been tried there")
	}
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("A2P2_REQUIRE_NODE") != "" {
			t.Fatal("A2P2_REQUIRE_NODE is set and node is not on PATH")
		}
		t.Skip("node is not on PATH: the page script was not run (make web-js reports this too)")
	}
	for _, tc := range []struct {
		c    surfacetest.Change
		cls  string
		want func(x *surfacetest.Changed) []string
	}{
		{surfacetest.PersonByAgent, "msg mine", func(x *surfacetest.Changed) []string {
			return []string{"this machine's person", "edited", "changed by " + x.Editor}
		}},
		{surfacetest.AgentByPerson, "msg", func(x *surfacetest.Changed) []string {
			return []string{"this machine", "edited", "changed by " + x.Editor + ", this machine's person"}
		}},
	} {
		x := surfacetest.NewChanged(t, tc.c)
		ss := testSessions()
		srv := httptest.NewServer(Handler(x.Here.S, ss))
		out, err := exec.Command(nodeBin, "testdata/page_harness.js", "static/index.html", "static/app.js", srv.URL, "c="+ss.Code(), x.Room).Output()
		srv.Close()
		if err != nil {
			t.Fatalf("node: %v\n%s", err, out)
		}
		var got struct {
			Errors []string `json:"errors"`
			Drawn  []struct {
				Cls   string   `json:"cls"`
				From  string   `json:"from"`
				Marks []string `json:"marks"`
			} `json:"drawn"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("harness output %q: %v", out, err)
		}
		if len(got.Errors) > 0 {
			t.Fatalf("errors: %v", got.Errors)
		}
		found := false
		for _, d := range got.Drawn {
			if d.Cls == tc.cls && d.From == x.Author {
				found = true
				if w := tc.want(x); !slices.Equal(d.Marks, w) {
					t.Errorf("change %d: marks %q, want %q", tc.c, d.Marks, w)
				}
			}
		}
		if !found {
			t.Errorf("change %d: no %s from %s in %+v", tc.c, tc.cls, x.Author, got.Drawn)
		}
	}
}
