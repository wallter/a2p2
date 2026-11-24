package node

// Named scenarios of the pull exchange. Those built on newPullHarness are
// compared with the delivery model after every step, as the differential
// test is; the explicit checks at the end say what the scenario is about.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/model/delivery"
	"github.com/wallter/a2p2/internal/store"
)

func runPull(t *testing.T, founders, later map[delivery.MachineID][]string, ops ...op) *harness {
	t.Helper()
	h := newPullHarness(t, founders, later)
	for _, o := range ops {
		h = h.apply(o)
	}
	t.Cleanup(h.close)
	return h
}

func threeFounders() map[delivery.MachineID][]string {
	return map[delivery.MachineID][]string{"M": {"alice"}, "P": {"pat"}, "Q": {"quinn"}}
}

// heldText is the text machine m holds of the record written i-th (h.writes),
// and whether it holds the record at all.
func heldText(h *harness, m delivery.MachineID, i int) (string, bool) {
	h.t.Helper()
	got, ok := h.all(h.sims[m])[h.writes[i].impl]
	if !ok {
		return "", false
	}
	return got.Record.Fields().Text, true
}

func writesOf(h *harness, kind core.Kind) []int {
	var out []int
	for i, w := range h.writes {
		if w.kind == kind {
			out = append(out, i)
		}
	}
	return out
}

func sleep(m delivery.MachineID) op { return op{kind: opSleep, machine: m} }
func wake(m delivery.MachineID) op  { return op{kind: opWake, machine: m} }
func pullOp(m, s delivery.MachineID, flags int) op {
	return op{kind: opPull, machine: m, server: s, n: flags}
}

// [R3, R5, D4] The author P and the reader Q are never awake together; M
// relays by pulling. Q reads P's question and reports it handled; the
// report comes back to P the same way.
func TestRelayCarriesAMessageAndItsHandledReportBetweenAnAuthorAndAReaderNeverAwakeTogether(t *testing.T) {
	h := runPull(t, threeFounders(), nil,
		sleep("Q"),
		op{kind: opSend, machine: "P", agent: "pat", ask: true},
		pullOp("M", "P", 0),
		sleep("P"),
		wake("Q"),
		pullOp("Q", "M", 0),
		op{kind: opSeenAll, machine: "Q", agent: "quinn"},
		op{kind: opHandled, machine: "Q", agent: "quinn", target: -1},
		pullOp("M", "Q", 0),
		sleep("Q"),
		wake("P"),
		pullOp("P", "M", 0),
	)
	if text, ok := heldText(h, "Q", 0); !ok || text == "" {
		t.Fatalf("the reader holds the question: %q %v", text, ok)
	}
	hs := writesOf(h, core.KindHandled)
	if len(hs) != 1 {
		t.Fatalf("handled reports written: %d", len(hs))
	}
	if _, ok := heldText(h, "P", hs[0]); !ok {
		t.Fatal("the author does not hold the handled report")
	}
}

// [R3, R5] P and Q are never awake together and the relay M never pulls:
// each of them offers its messages to M while pulling, and takes the
// other's.
func TestTwoMachinesNeverAwakeTogetherExchangeMessagesThroughOffersToARelay(t *testing.T) {
	h := runPull(t, threeFounders(), nil,
		sleep("Q"),
		op{kind: opSend, machine: "P", agent: "pat"},
		pullOp("P", "M", pullOffer),
		sleep("P"),
		wake("Q"),
		op{kind: opSend, machine: "Q", agent: "quinn"},
		pullOp("Q", "M", pullOffer),
		sleep("Q"),
		wake("P"),
		pullOp("P", "M", 0),
	)
	if text, ok := heldText(h, "Q", 0); !ok || text == "" {
		t.Fatalf("Q holds P's message: %q %v", text, ok)
	}
	if text, ok := heldText(h, "P", 1); !ok || text == "" {
		t.Fatalf("P holds Q's message: %q %v", text, ok)
	}
}

// [R4, D2] A source that leaves out a record stops the stream there for
// the puller (gap-free); the puller then takes the rest, past the hole,
// from another source.
func TestSwitchingSourcesPastAHoleFillsTheStream(t *testing.T) {
	sendM := op{kind: opSend, machine: "M", agent: "alice"}
	h := runPull(t, threeFounders(), nil,
		sendM, sendM, sendM,
		pullOp("P", "M", 0),
		op{kind: opWithhold, machine: "M", target: 1},
		pullOp("Q", "M", 0),
	)
	if _, ok := heldText(h, "Q", 0); !ok {
		t.Fatal("Q lacks the record before the hole")
	}
	for _, i := range []int{1, 2} {
		if _, ok := heldText(h, "Q", i); ok {
			t.Fatalf("Q holds record %d past the hole", i)
		}
	}
	h = h.apply(pullOp("Q", "P", 0))
	for i := 0; i < 3; i++ {
		if text, ok := heldText(h, "Q", i); !ok || text == "" {
			t.Fatalf("after switching sources Q lacks %d: %q %v", i, text, ok)
		}
	}
}

// [R3] Q takes no connections: pulls from it fail on both sides, yet its
// own pulls, with offers, carry its messages out and bring others in.
func TestAMachineThatOnlyOpensRequestsConverges(t *testing.T) {
	h := newPullHarness(t, threeFounders(), nil)
	t.Cleanup(func() { h.close() })
	h.px.noInbound["Q"] = true
	h.w.Machine("Q").NoInbound = true
	for _, o := range []op{
		op{kind: opSend, machine: "Q", agent: "quinn"},
		pullOp("M", "Q", 0), // refused on both sides
		op{kind: opSend, machine: "M", agent: "alice"},
		pullOp("Q", "M", pullOffer),
		op{kind: opSend, machine: "P", agent: "pat"},
		pullOp("P", "M", pullOffer),
		pullOp("Q", "M", 0),
	} {
		h = h.apply(o)
	}
	if h.px.c.failed != 1 {
		t.Fatalf("pulls refused: %d, want 1", h.px.c.failed)
	}
	for _, m := range []delivery.MachineID{"M", "P", "Q"} {
		for i := 0; i < 3; i++ {
			if text, ok := heldText(h, m, i); !ok || text == "" {
				t.Fatalf("%s lacks record %d", m, i)
			}
		}
	}
}

// [RM1, RM2] A removed machine's pull is refused by a server that knows of
// the removal (both sides), and it is given nothing.
func TestARemovedMachineIsRefused(t *testing.T) {
	h := runPull(t, threeFounders(), nil,
		op{kind: opRemove, machine: "P"},
		op{kind: opSend, machine: "M", agent: "alice"},
		pullOp("P", "M", pullOffer),
	)
	if h.px.c.failed != 1 {
		t.Fatalf("refused: %d", h.px.c.failed)
	}
	if _, ok := heldText(h, "P", 0); ok {
		t.Fatal("the removed machine was given a record")
	}
	_, err := h.sims["P"].x.Pull(context.Background(), diffRoom, h.sims["M"].m.ID, PullOpts{})
	var pe *PullError
	if !errors.As(err, &pe) || pe.Status != 403 || pe.Code != CodeNotMember {
		t.Fatalf("refusal: %v", err)
	}
}

// [RM2] The server removes the requester between building the response
// and releasing it (where step 5b holds responses): nothing is released,
// in the model as in the implementation.
func TestAHeldResponseIsNotReleasedToAPeerRemovedWhileItWaited(t *testing.T) {
	w := delivery.NewWorld()
	w.AddMachine("A", "alice")
	w.AddMachine("B", "bob")
	if _, err := w.Send("A", "alice", delivery.Msg{Body: "x"}); err != nil {
		t.Fatal(err)
	}
	hd, err := w.Hold("B", "A", delivery.PullOpt{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Remove("B", "A"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Release(hd); !errors.Is(err, delivery.ErrNotAllow) {
		t.Fatalf("model release: %v", err)
	}

	clock := lclock()
	net := newMemNet()
	a := newPNode(t, net, "a", newLNode(t, "a", clock))
	b := newPNode(t, net, "b", newLNode(t, "b", clock))
	room := a.create("team")
	a.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
	b.pin(room, a.lnode)
	a.register("alice")
	id := a.send(room, "alice", "x")
	released := 0
	a.x.atRelease = func(r, peer string, q *PullRequest) {
		if peer == b.id && q.Detail != nil && released == 0 { // a data page is built
			released++
			a.change(r, Change{Kind: core.EntryRemove, Target: b.id})
		}
	}
	_, err = b.pullFrom(room, a, PullOpts{})
	var pe *PullError
	if !errors.As(err, &pe) || pe.Code != CodeNotMember {
		t.Fatalf("pull: %v", err)
	}
	if _, ok := b.held(room, id); ok {
		t.Fatal("the removed peer was given the record")
	}
}

// [RM2, N1] N is admitted without history after M and P wrote: no page
// and no offer carries text written before, though N holds the headers;
// what is written after reaches it with text.
func TestANewcomerWithoutHistoryIsNeverGivenEarlierTextIncludingThroughAnOffer(t *testing.T) {
	later := map[delivery.MachineID][]string{"N": {"nina"}}
	h := runPull(t, threeFounders(), later,
		op{kind: opSend, machine: "M", agent: "alice"},
		op{kind: opSend, machine: "P", agent: "pat"},
		pullOp("M", "P", 0),
		op{kind: opAdmit, machine: "N", n: 0},
		pullOp("P", "M", 0), // P learns of N's admission
		pullOp("N", "M", 0),
		pullOp("P", "N", pullOffer), // P offers what N lacks
		pullOp("N", "P", pullOffer),
	)
	ns := h.sims["N"]
	before := map[string]bool{h.writes[0].impl: true, h.writes[1].impl: true}
	for id := range before {
		got, ok := h.all(ns)[id]
		if !ok {
			t.Fatalf("N lacks the header of %s", h.id(id))
		}
		if got.Record.Body() != nil || got.Reason != store.ReasonNotAdmitted {
			t.Fatalf("N holds earlier text: %+v", got)
		}
	}
	// A write makes a new harness; close the last one, whose databases
	// Windows cannot remove while they are open.
	t.Cleanup(func() { h.close() })
	h = h.apply(op{kind: opSend, machine: "P", agent: "pat"})
	h.px.net.pageText, h.px.net.offerText = nil, nil
	h = h.apply(pullOp("P", "N", pullOffer))
	if !slices.Contains(h.px.net.offerText, h.writes[2].impl) {
		t.Fatalf("the later message was not offered with text: %v", h.px.net.offerText)
	}
	for _, id := range append(h.px.net.offerText, h.px.net.pageText...) {
		if before[id] {
			t.Fatalf("earlier text sent to N: %s", h.id(id))
		}
	}
	if text, ok := heldText(h, "N", 2); !ok || text == "" {
		t.Fatalf("N lacks the later text: %q %v", text, ok)
	}
}

// [R4, R6] A relay that alters text has the record refused (the body does
// not match its signed hash) and the refusal counted; one that strips text
// leaves the record held without it, and an honest source repairs it.
func TestARelayThatAltersOrStripsTextIsNotBelieved(t *testing.T) {
	clock := lclock()
	net := newMemNet()
	a := newPNode(t, net, "a", newLNode(t, "a", clock))
	relay := newPNode(t, net, "r", newLNode(t, "r", clock))
	b := newPNode(t, net, "b", newLNode(t, "b", clock))
	room := a.create("team")
	for _, n := range []*pnode{relay, b} {
		a.change(room, Change{Kind: core.EntryAdd, Target: n.id, Name: n.name + "x", History: true})
	}
	for _, n := range []*pnode{relay, b} {
		n.pin(room, a.lnode)
		n.pull(room, a.lnode)
	}
	a.register("alice")
	b.register("bob")
	m1 := a.send(room, "alice", "one")
	relay.mustPull(room, a, PullOpts{})
	alter := true
	net.tamper = func(from, to string, resp *PullResponse) {
		if to != relay.id {
			return
		}
		for i, w := range resp.Page {
			r, err := core.ParseRecord(w)
			if err != nil || r.Body() == nil {
				continue
			}
			var body []byte // stripped
			if alter {      // the same size, one letter changed
				body = bytes.Replace(r.Body(), []byte("one"), []byte("onf"), 1)
			}
			resp.Page[i] = wireOf(r.Header(), r.Sig(), body)
		}
	}
	st, err := b.pullFrom(room, relay, PullOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.held(room, m1); ok {
		t.Fatalf("an altered record was stored: %+v", st)
	}
	rs, err := b.st.Refusals(context.Background(), room)
	if err != nil || len(rs) == 0 || rs[0].Peer != relay.id {
		t.Fatalf("refusals: %+v %v", rs, err)
	}
	alter = false
	// The pull stored nothing, so b remembers the relay's sum as seen and
	// would be answered same until something changes: as after a change.
	b.x.setSeen([2]string{room, relay.id}, nil)
	b.mustPull(room, relay, PullOpts{})
	if h, ok := b.held(room, m1); !ok || h.Reason != store.ReasonMissing {
		t.Fatalf("stripped record: %+v %v", h, ok)
	}
	if es := b.inbox(room, "bob"); len(es) != 0 {
		t.Fatalf("shown without its text: %+v", es)
	}
	net.tamper = nil
	if st := b.mustPull(room, a, PullOpts{}); st.Repaired != 1 {
		t.Fatalf("repair from the honest source: %+v", st)
	}
	if es := b.inbox(room, "bob"); len(es) != 1 || es[0].Text != "one" {
		t.Fatalf("bob after the repair: %+v", es)
	}
}

// wireOf is a record's wire form, {"h","s","b"} in base64, built by hand so
// that a body need not match.
func wireOf(h, sig, body []byte) []byte {
	m := map[string]string{"h": base64.StdEncoding.EncodeToString(h), "s": base64.StdEncoding.EncodeToString(sig)}
	if len(body) > 0 {
		m["b"] = base64.StdEncoding.EncodeToString(body)
	}
	out, _ := json.Marshal(m)
	return out
}
