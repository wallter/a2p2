package node

import (
	"context"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/core"
)

// TestTheSendWithFilesCardWritesSharedIDsInFullAndMarksEachAgent (D65,
// group H's open point): from a stored request to the card's text, the
// asking agent and an addressee of this machine are written with the
// full key when another known machine shares this machine's short id,
// and each carries the mark decided from the full key; an agent of
// another machine keeps its short id and no mark.
// Requirements: L1, P3
func TestTheSendWithFilesCardWritesSharedIDsInFullAndMarksEachAgent(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a, b := ns[0], ns[1]
	room := fileRoom(t, a, b)
	twin := a.id[:8] + strings.Repeat("0", len(a.id)-8)
	if twin == a.id {
		twin = a.id[:8] + strings.Repeat("1", len(a.id)-8)
	}
	st := a.state(room)
	st.Members = append(st.Members, twin)
	a.r.setState(room, &st)

	p := writeSource(t, "build.log", "bytes")
	to := []core.Addressee{{Agent: "agent-b", Node: b.id}, {Agent: HumanLabel, Node: a.id}}
	if _, err := a.f.RequestSend(context.Background(), FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "see", To: to}, Files: []string{p}}); err != nil {
		t.Fatal(err)
	}
	vs, err := (&Surface{m: a.m, r: a.r}).Waiting(context.Background())
	if err != nil || len(vs) != 1 || vs[0].Send == nil {
		t.Fatalf("%+v %v", vs, err)
	}
	v := vs[0]
	for _, want := range []string{
		"by agent " + Isolate("alice@"+a.id) + " (" + MarkThisMachine + ")",
		Isolate("agent-b@"+short(b.id)) + ", ",
		Isolate("human@"+a.id) + " (" + MarkThisPerson + ")",
	} {
		if !strings.Contains(v.Text, want) {
			t.Errorf("card text %q lacks %q", v.Text, want)
		}
	}
	if strings.Contains(v.Text, Isolate("agent-b@"+short(b.id))+" (") {
		t.Errorf("an agent of another machine is marked: %q", v.Text)
	}
	if v.Send.Agent != "alice@"+a.id || len(v.Send.To) != 2 || v.Send.To[1] != "human@"+a.id || v.Send.To[0] != "agent-b@"+short(b.id) {
		t.Errorf("send view ids %q %q", v.Send.Agent, v.Send.To)
	}
}
