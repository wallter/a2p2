package mcpshim

// Group G of the security review (2026-11-25): what an agent reads says
// which messages this machine wrote, which "human" is this machine's
// person and which room is the local one (D-NEW-G-mcp-identity).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// A member machine posts as human@<its id> and as Human@<its id>. The
// entries a model reads carry from_this_machine false and person
// "another machine's person, not yours"; only human written here is
// "yours". The field is never left out, even when false.
func TestAHumanOnAnotherMachineIsMarkedAsAnotherMachinesPerson(t *testing.T) {
	local := "r_" + strings.Repeat("a", 32)
	es := []localapi.Entry{
		{ID: "1", Room: "r_b", RoomLabel: "team", From: "human@9f9f9f9f", FromNode: strings.Repeat("9f", 32)},
		{ID: "2", Room: "r_b", RoomLabel: "team", From: "Human@9f9f9f9f", FromNode: strings.Repeat("9f", 32)},
		{ID: "3", Room: local, RoomLabel: "local", From: "human@1a2b3c4d", FromThisMachine: true},
		{ID: "4", Room: local, RoomLabel: "local", From: "claude-x@1a2b3c4d", FromThisMachine: true},
		{ID: "5", Room: "r_b", RoomLabel: "team", From: "bot@9f9f9f9f"},
	}
	out, err := jsonOut(view(es, local))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []map[string]any `json:"messages"`
		Rooms    []map[string]any `json:"rooms"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Messages) != 5 {
		t.Fatalf("%v %s", err, out)
	}
	want := map[string][2]any{
		"1": {false, personOther}, "2": {false, personOther}, "3": {true, personYours}, "4": {true, nil}, "5": {false, nil},
	}
	for _, m := range got.Messages {
		w := want[m["id"].(string)]
		if v, ok := m["from_this_machine"]; !ok || v != w[0] {
			t.Errorf("message %v: from_this_machine %v (present %v), want %v", m["id"], v, ok, w[0])
		}
		if m["person"] != w[1] {
			t.Errorf("message %v: person %v, want %v", m["id"], m["person"], w[1])
		}
	}
	for _, r := range got.Rooms {
		if (r["room"] == local) != (r["local"] == true) {
			t.Errorf("room %v: local %v", r["room"], r["local"])
		}
	}
	if person("human@1a2b3c4d", false) == personYours || person("HUMAN@1a2b3c4d", true) == personYours {
		t.Fatal("a human not written here, or not exactly human, is marked as yours")
	}
}

// The honest case on one machine: messages between local agents, in
// inbox, a thread, recent and peek, say from_this_machine true; the
// local room is marked local; ask_human names the exact id the answer
// comes from, and that answer is marked as this machine's person.
func TestMessagesOnThisMachineAreMarkedAndAskHumanNamesTheExactAnswerID(t *testing.T) {
	c := startDaemon(t)
	a, b := connect(t, c, "alice"), connect(t, c, "bob")
	agentID(t, a)
	agentID(t, b)
	id := idOf(t, ok(t, a, "send", map[string]any{"text": "hi"}))
	type msg struct {
		ID   string `json:"id"`
		Here *bool  `json:"from_this_machine"`
	}
	type res struct {
		Messages []msg `json:"messages"`
		First    []msg `json:"first_lines"`
		Rooms    []struct {
			Local bool `json:"local"`
		} `json:"rooms"`
		Local      bool   `json:"local"`
		AnswerFrom string `json:"answer_from"`
	}
	decode := func(s string) res {
		t.Helper()
		var r res
		if err := json.Unmarshal([]byte(s), &r); err != nil {
			t.Fatalf("%v: %s", err, s)
		}
		return r
	}
	for what, s := range map[string]string{
		"peek":   ok(t, b, "inbox", map[string]any{"peek": true}),
		"recent": ok(t, b, "inbox", map[string]any{"recent": 5}),
		"thread": ok(t, b, "inbox", map[string]any{"thread": id}),
		"inbox":  ok(t, b, "inbox", nil),
	} {
		r := decode(s)
		ms := append(r.Messages, r.First...)
		if len(ms) != 1 || ms[0].Here == nil || !*ms[0].Here || len(r.Rooms) != 1 || !r.Rooms[0].Local {
			t.Errorf("%s: %s", what, s)
		}
	}

	out := ok(t, a, "ask_human", map[string]any{"question": "deploy?"})
	r := decode(out)
	var st localapi.Status
	ctx := context.Background()
	if err := c.Call(ctx, "status", nil, &st); err != nil {
		t.Fatal(err)
	}
	// answer_from is the person by the full key: it stays the same
	// whatever machine joins a room later.
	if r.AnswerFrom != "human@"+st.Node || !r.Local || !strings.Contains(out, "reply to "+idOf(t, out)) {
		t.Fatalf("ask_human = %s, human %s", out, st.Human)
	}
	var in localapi.InboxResp
	if err := c.Call(ctx, "inbox", localapi.InboxReq{Agent: st.Human}, &in); err != nil || len(in.Entries) == 0 {
		t.Fatalf("human inbox %+v %v", in, err)
	}
	q := in.Entries[len(in.Entries)-1]
	if !q.FromThisMachine {
		t.Fatalf("question entry in the local API: %+v", q)
	}
	if err := c.Call(ctx, "send", localapi.SendReq{Agent: st.Human, Text: "no", ReplyTo: q.ID}, nil); err != nil {
		t.Fatal(err)
	}
	ans := ok(t, a, "inbox", nil)
	if !strings.Contains(ans, `"from":"`+st.Human+`","from_this_machine":true,"person":"`+personYours+`"`) {
		t.Fatalf("answer = %s", ans)
	}
}

// F42. a2p2 mcp never acts as the person: a server started with the
// harness name human, in any case, refuses every tool call and Run
// refuses to start.
func TestAnMCPServerRefusesThePersonsHarness(t *testing.T) {
	c := startDaemon(t)
	for _, h := range []string{"human", "Human"} {
		cs := connectAs(t, c, h, "human")
		if s, isErr := call(t, cs, "whoami", nil); !isErr || !strings.Contains(s, "reserved for the person") {
			t.Errorf("harness %q: whoami %v %s", h, isErr, s)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // so that a Run that did start returns at once
		if err := Run(ctx, c, h, "", "/w", "v0"); err == nil || !strings.Contains(err.Error(), "reserved for the person") {
			t.Errorf("harness %q: Run: %v", h, err)
		}
	}
	var st localapi.Status
	if err := c.Call(context.Background(), "status", nil, &st); err != nil {
		t.Fatal(err)
	}
	for _, a := range st.Agents {
		if strings.EqualFold(a.Harness, "human") && a.ID != st.Human {
			t.Errorf("registered %+v", a)
		}
	}
}

// connectAs is connect with a harness name of its own.
func connectAs(t *testing.T, c *localapi.Client, harness, label string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ct, st := mcp.NewInMemoryTransports()
	go NewServer(c, harness, label, "/work/"+label, "v0").Run(ctx, st)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close(); cancel() })
	return cs
}

// F44. A remote machine can pick any name. peers then shows every
// machine of a shared name with "#" and its id, this machine included,
// so a name alone never stands for this machine or a colleague.
func TestPeersMarksAMachineNameAnotherMachineAlsoUses(t *testing.T) {
	self, twin, other := strings.Repeat("1a", 32), strings.Repeat("9f", 32), strings.Repeat("3c", 32)
	st := localapi.Status{Node: self, Name: "tyler-mbp", Rooms: []localapi.Room{{ID: "r_x", Members: []localapi.Member{
		{Node: self, Name: "tyler-mbp"}, {Node: twin, Name: "Tyler-MBP"}, {Node: other, Name: "ci"},
	}}}}
	names := machineNames(st)
	for n, want := range map[string]string{
		self: "tyler-mbp#1a1a1a1a", twin: "Tyler-MBP#9f9f9f9f", other: "ci",
	} {
		name := map[string]string{self: "tyler-mbp", twin: "Tyler-MBP", other: "ci"}[n]
		if got := machineName(n, name, names, false); got != want {
			t.Errorf("%s: %q, want %q", n[:8], got, want)
		}
	}
	if got := machineName(twin, "Tyler-MBP", names, true); got != "Tyler-MBP#"+twin {
		t.Errorf("shared short id: %q", got)
	}
	if got := machineName(other, "", names, false); got != other[:8] {
		t.Errorf("no name: %q", got)
	}
}

// A room labelled local on another machine is shown with its id in
// whoami and peers, and only the local room says local.
func TestWhoamiAndPeersShowOnlyTheLocalRoomAsLocal(t *testing.T) {
	local, shared := "r_"+strings.Repeat("a", 32), "r_"+strings.Repeat("b", 32)
	got := shownLabels(localapi.Status{Local: local, Rooms: []localapi.Room{{ID: local, Label: "local"}, {ID: shared, Label: "local"}}})
	if got[local] != "local" || got[shared] != "local#bbbbbbbb" {
		t.Fatalf("labels %v", got)
	}
	if r := ref(shared, got[shared], local); r.Local || r.Label == node.LocalLabel {
		t.Fatalf("shared room ref %+v", r)
	}
}

// An author whose key shares this machine's short id: the message as a
// model reads it is written with the full key, marked id_shared, not from
// this machine, and its human is another machine's person. The node
// tests (surface_twin_test.go) show the daemon fills these fields in from
// two real keys of one short id.
func TestAnAuthorSharingAShortIDIsMarkedInWhatAModelReads(t *testing.T) {
	twin := "66b9826b" + strings.Repeat("0", 56)
	out, err := jsonOut(view([]localapi.Entry{{ID: "1", Room: "r_b", RoomLabel: "team", From: "human@" + twin, FromNode: twin, IDShared: true}}, ""))
	if err != nil {
		t.Fatal(err)
	}
	want := `"from":"human@` + twin + `","from_this_machine":false,"person":"` + personOther + `","id_shared":true`
	if !strings.Contains(out, want) {
		t.Fatalf("got %s, want %s", out, want)
	}
	// peek: a line from another machine's human, and one from a twin
	// key, keep what the daemon decided.
	pl, err := jsonOut(peekLines([]localapi.FirstLine{
		{ID: "2", Room: "r_b", From: "human@9f9f9f9f", Line: "yes"},
		{ID: "3", Room: "r_b", From: "bot@" + twin, IDShared: true, Line: "go"},
	}, map[string]string{"r_b": "team"}))
	if err != nil {
		t.Fatal(err)
	}
	var lines []map[string]any
	if err := json.Unmarshal([]byte(pl), &lines); err != nil || len(lines) != 2 {
		t.Fatalf("%v %s", err, pl)
	}
	if lines[0]["from_this_machine"] != false || lines[0]["person"] != personOther || lines[0]["room_label"] != "team" {
		t.Errorf("peek line from another machine's human %v", lines[0])
	}
	if lines[1]["from_this_machine"] != false || lines[1]["id_shared"] != true {
		t.Errorf("peek line from a twin key %v", lines[1])
	}
}

// F44, compared as drawn: a machine with no name is shown by its id, and
// another machine named as that id is marked with its own id, so it
// cannot pass as the unnamed one. A local agent's id and a writer's id
// are written with the full key when status marks it shared.
func TestAMachineNamedAsAnotherMachinesIDIsMarked(t *testing.T) {
	self, other := strings.Repeat("1a", 32), strings.Repeat("9f", 32)
	st := localapi.Status{Node: self, Rooms: []localapi.Room{{ID: "r_x", Members: []localapi.Member{
		{Node: self}, {Node: other, Name: self[:8]},
	}}}}
	names := machineNames(st)
	if got := machineName(self, "", names, false); got != self[:8] {
		t.Errorf("unnamed: %q", got)
	}
	if got := machineName(other, self[:8], names, false); got != self[:8]+"#"+other[:8] {
		t.Errorf("named as another's id: %q", got)
	}
	if got := localID("claude@"+self[:8], localapi.Status{Node: self, IDShared: true}); got != "claude@"+self {
		t.Errorf("local agent id %q", got)
	}
	if got := localID("claude@"+self[:8], localapi.Status{Node: self}); got != "claude@"+self[:8] {
		t.Errorf("honest local agent id %q", got)
	}
	shared := sharedOf(localapi.Status{Node: self, IDShared: true, Peers: []localapi.Peer{{Node: other, IDShared: true}}})
	if !shared[self] || !shared[other] || len(shared) != 2 {
		t.Errorf("shared %v", shared)
	}
}
