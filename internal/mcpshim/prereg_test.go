package mcpshim

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/localapi"
)

// [R7] D73 through the tools: a lead briefs a worker whose session
// does not exist yet. send says the worker has not registered and will
// read it; the worker's first whoami counts it unread and its first inbox
// shows it, without the room-wide post sent before it registered. A send
// to a registered agent says nothing of the kind.
func TestALeadBriefsAWorkerBeforeItsSessionStartsAndTheWorkerReadsItFirst(t *testing.T) {
	c := startDaemon(t)
	lead := connect(t, c, "lead")
	machine := strings.TrimPrefix(agentID(t, lead), "lead@")
	worker := "worker@" + machine
	ok(t, lead, "send", map[string]any{"text": "room-wide before"})
	s := ok(t, lead, "send", map[string]any{"text": "your brief", "to": []string{worker}})
	var r struct {
		Note string `json:"note"`
	}
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	if want := noteNotRegistered + worker + "."; !strings.HasSuffix(r.Note, want) {
		t.Fatalf("send note %q, want it to end %q", r.Note, want)
	}
	t.Logf("bytes the note adds for one addressee: %d", len(" "+noteNotRegistered+worker+"."))
	w := connect(t, c, "worker")
	if who := ok(t, w, "whoami", nil); !strings.Contains(who, `"unread":1`) {
		t.Fatalf("whoami %s", who)
	}
	es := parseEntries(t, ok(t, w, "inbox", nil))
	if len(es) != 1 || es[0].Text != "your brief" {
		t.Fatalf("first inbox %+v", es)
	}
	if s := ok(t, lead, "send", map[string]any{"text": "again", "to": []string{worker}}); strings.Contains(s, noteNotRegistered) {
		t.Fatalf("a send to a registered worker: %s", s)
	}
}

// [A2] The largest send result the note can make: 32 addressees (core.MaxTo),
// each a 64-character label written with a full 64-hex machine key (a
// shared short id), none registered. It stays under 10,000 bytes, well
// within the 10,000-token output limit D72 measured against.
func TestTheLargestSendResultWithTheNotRegisteredNoteStaysSmall(t *testing.T) {
	var ids []string
	for i := 0; i < core.MaxTo; i++ {
		ids = append(ids, fmt.Sprintf("%064d@%064x", i, i))
	}
	out, err := jsonOut(sentOf(localapi.SendResp{ID: strings.Repeat("a", 32), Room: "r_" + strings.Repeat("b", 32),
		RoomLabel: strings.Repeat("c", 64), To: ids, NotRegistered: ids}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("largest send result: %d bytes", len(out))
	if len(out) >= 10000 {
		t.Fatalf("send result of %d bytes", len(out))
	}
}
