package node

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/core"
)

// The 0.1.0 decoder of a pull response: the field list and shapes as they
// were before want (PROTOCOL 10.5, what 0.1.0 does with each new thing).
var (
	oldResponseFields = []string{"v", "same", "authors", "digests", "sum", "log", "page", "more", "peers"}
	oldResponseShape  = shape{"v": nil, "same": nil, "authors": nil, "digests": shape{"*": nil}, "sum": nil,
		"detail": detailShape, "log": logShape, "page": recordsShape, "more": nil,
		"peers": shape{"[]": shape{"node": nil, "addrs": nil, "seen": nil}}}
)

// Requirements: L1, A2
func TestAnOldVersionPullerIgnoresWant(t *testing.T) {
	resp := PullResponse{V: PullVersion, Same: true, Log: PullLog{Heads: []string{strings.Repeat("a", 32)}},
		Want: []PullWant{{M: strings.Repeat("1", 32), V: strings.Repeat("2", 32), H: strings.Repeat("3", 64)}}}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"want":[`)) {
		t.Fatalf("no want in %s", b)
	}
	if !strictPull(b, oldResponseFields, oldResponseShape) {
		t.Fatalf("the 0.1.0 decoder refuses an answer with want: %s", b)
	}
	var old struct {
		V    int     `json:"v"`
		Same bool    `json:"same"`
		Log  PullLog `json:"log"`
	}
	if err := json.Unmarshal(b, &old); err != nil || !old.Same {
		t.Fatalf("%+v %v", old, err)
	}
	// and this version's own decoder takes it, with the want
	got, err := DecodePullResponse(b)
	if err != nil || len(got.Want) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
}

// Requirements: L1
func TestWantsFitInsideTheAnswerSizeLimit(t *testing.T) {
	ws := make([]PullWant, core.MaxWants)
	for i := range ws {
		ws[i] = PullWant{M: strings.Repeat("f", 32), V: strings.Repeat("f", 32), H: strings.Repeat("f", 64)}
	}
	if n := jsonSize(ws); n > core.MaxWants*wantBytes {
		t.Fatalf("%d wants take %d bytes, over the %d reserved", core.MaxWants, n, core.MaxWants*wantBytes)
	}
	f := &Files{}
	if got := f.wantsFor(t.Context(), "r", "p", wantBytes-1); got != nil {
		t.Fatal("wants in an answer with no room left")
	}
}

// Requirements: L1, A2
func TestARecordWithFilesIsStoredAndRelayedByteForByteByAnOldVersionNode(t *testing.T) {
	clock := lclock()
	net := newMemNet()
	fn := newMemFiles(net)
	a := newFNode(t, net, fn, "a", clock)
	old := newPNode(t, net, "old", newLNode(t, "old", clock)) // no attachments: as 0.1.0 handles records
	c := newFNode(t, net, fn, "c", clock)
	room := a.create("team")
	for _, o := range []*lnode{old.lnode, c.lnode} {
		a.change(room, Change{Kind: core.EntryAdd, Target: o.id, Name: o.name, History: true})
	}
	for _, o := range []*lnode{old.lnode, c.lnode} {
		o.pin(room, a.lnode)
		o.pull(room, a.lnode)
		o.register("agent-" + o.name)
	}
	a.register("alice")
	msg, res := a.sendFiles(room, "alice", "relayed", writeSource(t, "r.txt", "relayed bytes"))
	old.mustPull(room, a.pnode, PullOpts{})
	net.down[a.id] = true
	c.mustPull(room, old, PullOpts{})
	wire := func(n *lnode) []byte {
		for _, r := range n.records(room) {
			if r.ID() == msg {
				return r.Wire()
			}
		}
		t.Fatalf("%s lacks %s", n.name, msg)
		return nil
	}
	if !bytes.Equal(wire(a.lnode), wire(old.lnode)) || !bytes.Equal(wire(a.lnode), wire(c.lnode)) {
		t.Fatal("the record changed on its way through the old node")
	}
	es := old.inbox(room, "agent-old")
	if i := slices.IndexFunc(es, func(e Entry) bool { return e.ID == msg }); i < 0 || es[i].Text != "relayed" {
		t.Fatalf("the old node does not show the text: %+v", es)
	}
	net.down[a.id] = false
	c.tickFiles()
	c.inbox(room, "agent-c")
	if r, err := c.fetch(room, "agent-c", res.Files[0].SHA256); err != nil || r.State != FetchHere {
		t.Fatalf("%+v %v", r, err)
	}
}
