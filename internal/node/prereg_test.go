package node

// D73: a message addressed to an agent before the agent first
// registers is unread for it when it does; room-wide history from before
// is not.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// [R7, U3] A lead briefs workers before their sessions exist. Each worker's
// first inbox holds the briefs addressed to it, those to several
// addressees included, and none of the room-wide history from before it
// registered; what follows is read as usual, across a restart, and
// registering again changes nothing.
func TestABriefSentBeforeTheWorkerRegistersIsUnreadWhenItDoesButRoomWideHistoryIsNot(t *testing.T) {
	r := newRig(t)
	r.send("alice", Draft{Text: "room-wide before"})
	r.send("alice", Draft{Text: "brief", To: r.to("w1")})
	r.send("alice", Draft{Text: "for w2", To: r.to("w2")})
	r.send("bob", Draft{Text: "for both", To: r.to("w2", "w1")})
	r.send("alice", Draft{Text: "room-wide between"})
	r.register("w1")
	r.restart()
	wantKinds(t, r.read("w1"), "message:brief", "message:for both")
	r.send("alice", Draft{Text: "room-wide after"})
	wantKinds(t, r.read("w1"), "message:room-wide after")
	r.register("w1")
	wantKinds(t, r.inbox("w1"))
	r.register("w2")
	wantKinds(t, r.inbox("w2"), "message:for w2", "message:for both")
}

// [R7] A brief replaced before the worker registers is shown once, with its
// newest text, as edited; one withdrawn before is not shown at all. After
// the worker read the brief, a replacement or a withdraw is a notice, as
// for any agent.
func TestABriefReplacedBeforeTheWorkerRegistersShowsItsNewTextAndOneWithdrawnShowsNothing(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := r.send("alice", Draft{Text: "brief v1", To: r.to("w1")})
	b := r.send("alice", Draft{Text: "cancelled", To: r.to("w1")})
	if _, err := r.m.Replace(ctx, mRoom, "alice", a, "brief v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Withdraw(ctx, mRoom, "alice", b); err != nil {
		t.Fatal(err)
	}
	r.register("w1")
	es := r.read("w1")
	wantKinds(t, es, "message:brief v2")
	if !es[0].Edited || es[0].ID != a {
		t.Fatalf("entry %+v", es[0])
	}
	if _, err := r.m.Replace(ctx, mRoom, "alice", a, "brief v3"); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, r.read("w1"), "replaced:brief v3")
	if _, err := r.m.Withdraw(ctx, mRoom, "alice", a); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, r.read("w1"), "withdrawn:")
}

// [R6, R7, D7] A brief whose text expired before the worker registered is
// shown as expired, not dropped in silence, and holds back nothing: what
// its author wrote after it, before and after the registration, is read
// in the same listing as for any agent.
func TestABriefWhoseTextExpiredBeforeTheWorkerRegisteredIsShownAsExpired(t *testing.T) {
	r := newRig(t)
	id := r.send("alice", Draft{Text: "old brief", To: r.to("w1")})
	r.now = r.now.Add(store.Retention + time.Hour)
	if err := r.m.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.send("alice", Draft{Text: "room-wide before"})
	r.send("bob", Draft{Text: "for w1 before", To: r.to("w1")})
	r.register("w1")
	r.send("alice", Draft{Text: "after"})
	es := r.read("w1")
	wantKinds(t, es, "expired:", "message:for w1 before", "message:after")
	if es[0].ID != id || es[0].Key != "x:"+id {
		t.Fatalf("entry %+v", es[0])
	}
	wantKinds(t, r.inbox("w1"))
}

// [R7, D10] A reply addressed to a worker that registers later, in a
// thread whose room-wide root it never saw: the reply alone is unread,
// with its parent named and held.
func TestAReplyAddressedToANewWorkerInAThreadItNeverSawIsUnreadWithItsParentHere(t *testing.T) {
	r := newRig(t)
	root := r.send("alice", Draft{Text: "plan"})
	r.send("bob", Draft{Text: "w1, take step 2", ReplyTo: root, To: r.to("w1", "alice")})
	r.register("w1")
	es := r.inbox("w1")
	wantKinds(t, es, "message:w1, take step 2")
	if es[0].Parent != root || es[0].Thread != root || es[0].ParentState != ParentHere {
		t.Fatalf("entry %+v", es[0])
	}
}

// [U1, U3] At level addressed a new worker's brief is in its inbox; at
// none it is not, and Skipped lists it even after a confirmation moved
// the cursor past it, and after the level is raised again.
func TestANewWorkerAtAddressedReadsItsBriefAndAtNoneFindsItInSkipped(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.send("alice", Draft{Text: "room-wide"})
	r.send("alice", Draft{Text: "brief 1", To: r.to("w1")})
	r.send("alice", Draft{Text: "brief 2", To: r.to("w2")})
	r.register("w1")
	r.register("w2")
	if err := r.m.SetLevel(ctx, mRoom, "w1", store.LevelAddressed); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, r.read("w1"), "message:brief 1")
	if err := r.m.SetLevel(ctx, mRoom, "w2", store.LevelNone); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, r.read("w2"))
	skipped := func() []Entry {
		es, err := r.m.Skipped(ctx, mRoom, "w2")
		if err != nil {
			t.Fatal(err)
		}
		return es
	}
	wantKinds(t, skipped(), "message:brief 2")
	if err := r.m.SetLevel(ctx, mRoom, "w2", store.LevelAll); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, r.inbox("w2"))
	wantKinds(t, skipped(), "message:brief 2")
}

// [R7, T2] A brief another machine wrote to an agent of this machine counts
// like one written here; a message to the same label on another machine
// is not this agent's.
func TestABriefFromAnotherMachineCountsAndOneToTheSameLabelElsewhereDoesNot(t *testing.T) {
	r := newRig(t)
	seed := sha256.Sum256([]byte("prereg peer"))
	peer := ed25519.NewKeyFromSeed(seed[:])
	peerNode := fmt.Sprintf("%x", []byte(peer.Public().(ed25519.PublicKey)))
	first := r.put(peer, core.RecordFields{Epoch: "00000000000000f1", Kind: core.KindMessage, Agent: "lead",
		Text: "remote brief", To: []core.Addressee{{Node: r.m.ID, Agent: "w1"}}})
	r.put(peer, core.RecordFields{Epoch: "00000000000000f1", Seq: 2, Prev: first.ID(), Kind: core.KindMessage, Agent: "lead",
		Text: "for w1 elsewhere", To: []core.Addressee{{Node: peerNode, Agent: "w1"}}})
	r.register("w1")
	wantKinds(t, r.read("w1"), "message:remote brief")
}

// [R2, R7] A restore to a backup from before the worker registered takes
// the registration back; registering again shows the briefs the restored
// store holds, once more unread.
func TestAWorkerWhoseRegistrationARestoreTookBackReadsItsBriefsWhenItRegistersAgain(t *testing.T) {
	r := newRig(t)
	r.send("alice", Draft{Text: "brief", To: r.to("w1")})
	r.st.Close()
	old := filepath.Join(t.TempDir(), "old.db")
	copyDB(t, r.path, old)
	r.open()
	r.register("w1")
	wantKinds(t, r.read("w1"), "message:brief")
	r.send("alice", Draft{Text: "lost brief", To: r.to("w1")})
	r.st.Close()
	copyDB(t, old, r.path)
	r.open()
	if _, err := r.m.Inbox(context.Background(), mRoom, "w1"); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("after the restore: %v", err)
	}
	r.register("w1")
	wantKinds(t, r.inbox("w1"), "message:brief")
}

// [R7, A2] A flood of briefs to a label that has not registered is paged
// like any inbox: one read returns at most the limit, says how many more
// there are, and pages through them in order.
func TestAFloodOfBriefsToAFutureLabelIsPagedLikeAnyInbox(t *testing.T) {
	e := newSurf(t)
	ctx := context.Background()
	lead := e.agent("lead")
	w1 := "w1@" + short(e.s.m.ID)
	n := DefaultInboxLimit + 7
	for i := 0; i < n; i++ {
		e.send(SendInput{Agent: lead, Text: fmt.Sprint("brief ", i), To: []string{w1}})
	}
	if got := e.agent("w1"); got != w1 {
		t.Fatalf("registered as %s", got)
	}
	v, err := e.s.Inbox(ctx, w1, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Entries) != DefaultInboxLimit || v.More != n-DefaultInboxLimit || v.Entries[0].Text != "brief 0" {
		t.Fatalf("%d entries, %d more, first %q", len(v.Entries), v.More, v.Entries[0].Text)
	}
	seen := len(e.read(w1))
	if rest := e.read(w1); seen+len(rest) != n || rest[len(rest)-1].Text != fmt.Sprint("brief ", n-1) {
		t.Fatalf("read %d then %d", seen, len(rest))
	}
}

// [R7] send names the addressees on this machine that have not registered
// yet, and no other: not one already registered, not the person, not an
// agent of another machine (this machine cannot know those).
func TestSendNamesTheAddresseesOnThisMachineThatHaveNotRegisteredYet(t *testing.T) {
	e := newSurf(t)
	lead, w2 := e.agent("lead"), e.agent("w2")
	w1 := "w1@" + short(e.s.m.ID)
	s := e.send(SendInput{Agent: lead, Text: "briefs", To: []string{w1, w2, HumanLabel}})
	if got := e.s.AgentIDs(context.Background(), s.NotRegistered); !slices.Equal(got, []string{w1}) {
		t.Fatalf("not registered %v", got)
	}
	if s := e.send(SendInput{Agent: lead, Text: "room-wide"}); len(s.NotRegistered) != 0 {
		t.Fatalf("room-wide post: %v", s.NotRegistered)
	}
	e.agent("w1")
	if s := e.send(SendInput{Agent: lead, Text: "again", To: []string{w1}}); len(s.NotRegistered) != 0 {
		t.Fatalf("after registering: %v", s.NotRegistered)
	}
	if got := texts(e.inbox(w1)); got != "again, briefs" {
		t.Fatalf("w1 read %q", got)
	}
}
