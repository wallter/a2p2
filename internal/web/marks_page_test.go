package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"testing"
)

// Follow-ups of D65 and D62 on the page: authorMarks (static/app.js), the
// words drawn beside a message's author, mark a human on another machine
// as that machine's person and never as this machine's, mark what this
// machine wrote, and mark a replaced text as edited. The function is run
// as it is in app.js, in node; the stream and question cards draw its
// words as plain text in the existing "mark" spans.
func TestThePageMarksAnotherMachinesPersonThisMachineAndEdits(t *testing.T) {
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
	src, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	fn := regexp.MustCompile(`(?ms)^function authorMarks\(m\) \{.*?^\}`).Find(src)
	if fn == nil {
		t.Fatal("app.js has no authorMarks")
	}
	type entry struct {
		From            string `json:"from,omitempty"`
		FromThisMachine bool   `json:"from_this_machine"`
		Edited          bool   `json:"edited,omitempty"`
	}
	cases := []struct {
		e    entry
		want []string
	}{
		{entry{From: "human@1a2b3c4d"}, []string{"another machine's person, not you"}},
		{entry{From: "HUMAN@1a2b3c4d"}, []string{"another machine's person, not you"}},
		{entry{From: "human@f4bd4652", FromThisMachine: true}, []string{"this machine's person"}},
		{entry{From: "Human@f4bd4652", FromThisMachine: true}, []string{"this machine, not its person"}},
		{entry{From: "bot@f4bd4652", FromThisMachine: true}, []string{"this machine"}},
		{entry{From: "bot@1a2b3c4d"}, []string{}},
		{entry{From: "bot@1a2b3c4d", Edited: true}, []string{"edited"}},
		{entry{From: "human@1a2b3c4d", Edited: true}, []string{"another machine's person, not you", "edited"}},
	}
	in := []entry{}
	for _, c := range cases {
		in = append(in, c.e)
	}
	arg, _ := json.Marshal(in)
	script := string(fn) + "\nprocess.stdout.write(JSON.stringify(JSON.parse(process.argv[1]).map(authorMarks)));"
	out, err := exec.Command(nodeBin, "-e", script, string(arg)).Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got [][]string
	if err := json.Unmarshal(out, &got); err != nil || len(got) != len(cases) {
		t.Fatalf("output %q: %v", out, err)
	}
	for i, c := range cases {
		g, _ := json.Marshal(got[i])
		w, _ := json.Marshal(c.want)
		if string(g) != string(w) {
			t.Errorf("%+v: marks %s, want %s", c.e, g, w)
		}
	}
	// Both cards draw the marks.
	for _, use := range []string{`for (const k of authorMarks(m)) meta.append(el("span", "mark", k));`, `const marks = authorMarks(m).filter`} {
		if !regexp.MustCompile(regexp.QuoteMeta(use)).Match(src) {
			t.Errorf("app.js no longer draws the marks with %q", use)
		}
	}
}
