package node

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/store"
)

// The reading round (D-reading): every view orders messages by (created,
// author, position), so one author's messages of one millisecond come in
// the order written; skipped history pages; every read that showed text
// lets the agent report it handled.

func ids(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

// [D4, D10, U3] Messages written in the same millisecond keep their
// author's order in history, recent, skipped and thread reads, and the
// order does not change between reads.
func TestMessagesOfOneMillisecondKeepTheOrderWrittenInEveryView(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	at := time.Now().Truncate(time.Millisecond)
	e.now = func() time.Time { return at }
	e.restart()
	alice, bob, carol := e.agent("alice"), e.agent("bob"), e.agent("carol")
	e.level(carol, "", store.LevelNone)
	root := e.send(SendInput{Agent: alice, Text: "root"})
	want := []string{root.ID}
	for i := 0; i < 12; i++ { // two agents of one machine, interleaved
		from := []string{alice, bob}[i%2]
		want = append(want, e.send(SendInput{Agent: from, Text: fmt.Sprint("m", i), ReplyTo: root.ID, To: []string{Everyone}}).ID)
	}
	for round := 0; round < 3; round++ {
		hist, err := e.s.History(ctx, "", "", "", 0)
		if err != nil || !slices.Equal(ids(hist), want) {
			t.Fatalf("history %v %v, want %v", ids(hist), err, want)
		}
		th, err := e.s.Thread(ctx, carol, root.ID)
		if err != nil || !slices.Equal(ids(th), want) {
			t.Fatalf("thread %v %v, want %v", ids(th), err, want)
		}
		sk, err := e.s.Skipped(ctx, carol, "", "", 0)
		if err != nil || !slices.Equal(ids(sk), want) {
			t.Fatalf("skipped %v %v, want %v", ids(sk), err, want)
		}
	}
}

// [D7, U3] A message whose text never arrived, held as a header while a
// level kept it out, is still accounted for when retention runs out:
// skipped lists it as expired, after the level is raised too.
func TestSkippedListsAHeaderWhoseTextNeverArrivedAsExpired(t *testing.T) {
	e := newSurf(t)
	e.agent("alice")
	bob := e.agent("bob")
	e.level(bob, "", store.LevelNone)
	id := putHeaderOnly(t, e, time.Now().Add(-store.Retention-time.Hour))
	e.level(bob, "", store.LevelAll)
	sk := e.skipped(bob)
	if len(sk) != 1 || sk[0].ID != id || sk[0].Kind != EntryExpired {
		t.Fatalf("skipped %+v, want %s as expired", sk, id)
	}
	if got := e.read(bob); len(got) != 0 {
		t.Fatalf("inbox %+v", got)
	}
}

// [D3, D9, U1] Seen needs a receipt of a full inbox listing: an id from
// peek, recent, skipped or a thread is refused. A message listed at all
// and confirmed after a switch to none is confirmed, is not skipped, and
// later replace and withdraw notices reach the agent.
func TestSeenNeedsAnInboxReceiptThatSurvivesALevelChange(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	m := e.send(SendInput{Agent: alice, Text: "to bob", To: []string{bob}})
	seen := func() bool {
		t.Helper()
		res, err := e.s.Seen(ctx, bob, []store.Read{{Key: m.ID, Version: m.ID}})
		if err != nil {
			t.Fatal(err)
		}
		return res[0].Confirmed
	}
	if u, err := e.s.Unread(ctx, bob, "", true); err != nil || len(u.First) != 1 {
		t.Fatalf("peek %+v %v", u, err)
	}
	if _, err := e.s.Thread(ctx, bob, m.ID); err != nil {
		t.Fatal(err)
	}
	if seen() {
		t.Fatal("confirmed an id only peek and a thread showed")
	}
	if _, err := e.s.Inbox(ctx, bob, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	e.level(bob, "", store.LevelNone)
	if !seen() {
		t.Fatal("a listed message was refused after a level change")
	}
	e.level(bob, "", store.LevelAll)
	if sk := e.skipped(bob); len(sk) != 0 {
		t.Fatalf("a confirmed message is skipped: %+v", sk)
	}
	e.send(SendInput{Agent: alice, Replaces: m.ID, Text: "to bob, edited"})
	e.send(SendInput{Agent: alice, Replaces: m.ID})
	var kinds []string
	for _, x := range e.read(bob) {
		kinds = append(kinds, x.Kind)
	}
	if !slices.Equal(kinds, []string{EntryWithdrawn}) && !slices.Equal(kinds, []string{EntryReplaced, EntryWithdrawn}) {
		t.Fatalf("notices %v", kinds)
	}
}

// [U4] peek for one room counts and takes first lines from that room only,
// so a quiet room's lines are not crowded out by a busy one.
func TestPeekForOneRoomTakesItsFirstLinesBeforeTheCap(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	if _, err := e.s.CreateRoom(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxFirstLines+2; i++ {
		e.send(SendInput{Agent: alice, Text: fmt.Sprint("local ", i)})
	}
	e.send(SendInput{Agent: alice, Room: "team", Text: "team news"})
	u, err := e.s.Unread(ctx, bob, "team", true)
	if err != nil || u.Count != 1 || len(u.Rooms) != 1 || len(u.First) != 1 || u.First[0].Line != "team news" {
		t.Fatalf("peek at team: %+v %v", u, err)
	}
	if _, err := e.s.Unread(ctx, bob, "nowhere", true); !isUser(err) {
		t.Fatalf("peek at an unknown room: %v", err)
	}
}

// [U3] Skipped history pages with before, so an agent away at level none
// through more than one page reads all of it; recent marks which entries
// a level kept out, and the pages are the ones the CLI and the tool use.
func TestSkippedHistoryPagesThroughEverythingALevelKeptOut(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	e.level(bob, "", store.LevelNone)
	var want []string
	for i := 0; i < MaxHistory+7; i++ {
		want = append(want, e.send(SendInput{Agent: alice, Text: fmt.Sprint("m", i), To: []string{bob}}).ID)
	}
	e.level(bob, "", store.LevelAll)
	e.restart()
	var got []string
	before := ""
	for {
		page, err := e.s.Skipped(ctx, bob, "", before, 200)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		got = append(ids(page), got...)
		before = page[0].ID
	}
	if !slices.Equal(got, want) {
		t.Fatalf("paged %d skipped, want %d (first %v)", len(got), len(want), got[:min(3, len(got))])
	}
	recent, err := e.s.History(ctx, bob, "", "", 3)
	if err != nil || len(recent) != 3 || !recent[0].Skipped || !recent[2].Skipped {
		t.Fatalf("recent %+v %v", recent, err)
	}
	other := e.agent("carol")
	if recent, err := e.s.History(ctx, other, "", "", 3); err != nil || recent[0].Skipped {
		t.Fatalf("carol's recent marks skipped: %+v %v", recent, err)
	}
	if got := e.read(bob); len(got) != 0 {
		t.Fatalf("paging skipped put %d in the inbox", len(got))
	}
}
