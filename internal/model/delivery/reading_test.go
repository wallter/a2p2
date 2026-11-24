package delivery

import (
	"errors"
	"testing"
)

// What counts as shown for a handled report (rule 7, widened 2026-11-24,
// D-reading): text a thread, branch, recent or skipped view returned to the
// agent counts as well as text its inbox showed. Recording it moves no
// cursor and marks nothing seen.

// Requirements: D5, D10, U3. A machine admitted with history after a
// kickoff that asked for handled reports: its agent's inbox never shows the
// kickoff (its time began later), so it reads it in the thread, and may
// then report it handled. Before the thread read it may not.
func TestAgentAdmittedAfterAKickoffReportsItHandledAfterReadingTheThread(t *testing.T) {
	w := room("A", "B")
	kick := must(w.Send("A", "a", Msg{Body: "kickoff", AskHandled: true}))
	must(w.Admit("C", "A", true, "c"))
	pull(t, w, "C", "A")
	wantTexts(t, must(w.Inbox("C", "c")).Entries, "")
	if _, err := w.Handle("C", "c", kick); !errors.Is(err, ErrNotAllow) {
		t.Fatalf("handled before any read: %v, want not allowed", err)
	}
	th := w.Thread("C", kick)
	wantTexts(t, th, "shown:kickoff")
	if err := w.Showed("C", "c", th); err != nil {
		t.Fatal(err)
	}
	wantTexts(t, must(w.Inbox("C", "c")).Entries, "") // nothing new in the inbox
	must(w.Handle("C", "c", kick))
	pull(t, w, "A", "C")
	if st := w.Status("A", kick); len(st.Handled) != 1 || st.Handled[0].Addr != at("C") {
		t.Fatalf("the author sees handled reports %v, want c's", st.Handled)
	}
}

// Requirements: D5, U1, U3. An agent at level none reads a message
// addressed to it with Skipped and may then report it handled; the message
// stays out of its inbox and stays listed as skipped until it is handled.
func TestAgentAtLevelNoneReportsHandledWhatSkippedShowedIt(t *testing.T) {
	w := levelRoom()
	if err := w.SetLevel("A", "a", None); err != nil {
		t.Fatal(err)
	}
	id := post(w, Msg{Body: "to a", To: []Addr{{"A", "a"}}})
	pull(t, w, "A", "B")
	if _, err := w.Handle("A", "a", id); !errors.Is(err, ErrNotAllow) {
		t.Fatalf("handled before any read: %v, want not allowed", err)
	}
	sk := w.Skipped("A", "a")
	if err := w.Showed("A", "a", sk); err != nil {
		t.Fatal(err)
	}
	wantTexts(t, w.Skipped("A", "a"), "shown:to a") // recording marks nothing
	wantTexts(t, readAs(t, w, "A", "a"), "")
	must(w.Handle("A", "a", id))
	wantTexts(t, w.Skipped("A", "a"), "") // handled: nothing left to catch up on
}

// Requirements: D5. The addressee rule is unchanged: a read shows an agent
// a message addressed to someone else, or a room-wide post that did not
// ask, and it still may not report it handled.
func TestReadingAThreadDoesNotLetANonAddresseeReportHandled(t *testing.T) {
	w := levelRoom()
	toB := post(w, Msg{Body: "to b", To: []Addr{{"A", "b"}}})
	plain := post(w, Msg{Body: "plain"})
	pull(t, w, "A", "B")
	for _, id := range []RecordID{toB, plain} {
		if err := w.Showed("A", "a", w.Thread("A", id)); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Handle("A", "a", id); !errors.Is(err, ErrNotAllow) {
			t.Fatalf("a reported %s handled: %v, want not allowed", id, err)
		}
	}
}

// Requirements: D3, D9, U1, U2. A message the inbox listed at level all,
// confirmed after the agent switched to none, is confirmed: it is not put
// in skipped, and later replace and withdraw notices still reach the agent.
func TestMessageListedAtAllAndConfirmedAfterSwitchingToNoneStaysConfirmed(t *testing.T) {
	w := levelRoom()
	id := post(w, Msg{Body: "to a", To: []Addr{{"A", "a"}}})
	pull(t, w, "A", "B")
	v := must(w.List("A", "a"))
	if err := w.SetLevel("A", "a", None); err != nil {
		t.Fatal(err)
	}
	if ok := must(w.Confirm("A", "a", v.Entries)); len(ok) != 1 || !ok[0] {
		t.Fatalf("confirmation after the level change accepted %v, want [true]", ok)
	}
	if err := w.SetLevel("A", "a", All); err != nil {
		t.Fatal(err)
	}
	wantTexts(t, w.Skipped("A", "a"), "")
	w.Advance(Hour)
	must(w.Replace("B", "x", id, "new text"))
	pull(t, w, "A", "B")
	wantTexts(t, readAs(t, w, "A", "a"), "replaced:new text")
	w.Advance(Hour)
	must(w.Withdraw("B", "x", id))
	pull(t, w, "A", "B")
	wantTexts(t, readAs(t, w, "A", "a"), "withdrawn:")
}

// Requirements: D3, U1. A level change never takes back what the inbox
// listed: a message listed at all and not yet confirmed stays in the inbox
// at none, is never listed as skipped, and is confirmed once.
func TestLevelChangeDoesNotHideAMessageTheInboxListed(t *testing.T) {
	w := levelRoom()
	post(w, Msg{Body: "to a", To: []Addr{{"A", "a"}}})
	pull(t, w, "A", "B")
	must(w.List("A", "a"))
	if err := w.SetLevel("A", "a", None); err != nil {
		t.Fatal(err)
	}
	wantTexts(t, w.Skipped("A", "a"), "")
	wantTexts(t, readAs(t, w, "A", "a"), "shown:to a")
	wantTexts(t, readAs(t, w, "A", "a"), "")
}

// Requirements: D3. Seen confirms only what a full inbox listing showed:
// a key and version learned from a thread or skipped view are refused.
func TestSeenRefusesWhatOnlyAThreadOrSkippedViewShowed(t *testing.T) {
	w := levelRoom()
	id := post(w, Msg{Body: "to a", To: []Addr{{"A", "a"}}})
	pull(t, w, "A", "B")
	if ok := must(w.Confirm("A", "a", w.Thread("A", id))); len(ok) != 1 || ok[0] {
		t.Fatalf("confirmation from a thread view accepted %v, want [false]", ok)
	}
	if err := w.SetLevel("A", "a", None); err != nil {
		t.Fatal(err)
	}
	if ok := must(w.Confirm("A", "a", w.Skipped("A", "a"))); len(ok) != 1 || ok[0] {
		t.Fatalf("confirmation from skipped accepted %v, want [false]", ok)
	}
	wantTexts(t, w.Skipped("A", "a"), "shown:to a")
}

// Requirements: D7, U3. Text that expires while a level keeps its message
// out of the inbox is still accounted for: skipped lists it as expired.
func TestSkippedListsAMessageWhoseTextExpiredAsExpired(t *testing.T) {
	w := levelRoom()
	if err := w.SetLevel("A", "a", None); err != nil {
		t.Fatal(err)
	}
	post(w, Msg{Body: "to a", To: []Addr{{"A", "a"}}})
	pull(t, w, "A", "B")
	w.Advance(Retention)
	wantTexts(t, w.Skipped("A", "a"), "expired:")
}

// Requirements: D10. The branch under a message this machine does not hold
// (its server withheld it) still lists the replies to it, with the parent
// marked missing. Unlike gap G3, nothing is lost here: the replies name
// the message under, so they are found without holding it.
func TestBranchUnderAnUnheldMessageListsItsRepliesWithTheParentMissing(t *testing.T) {
	w := room("A", "B", "C")
	p := must(w.Send("A", "a", Msg{Body: "parent"}))
	pull(t, w, "B", "A")
	w.Advance(Hour)
	must(w.Send("B", "b", Msg{Body: "reply", Parent: p}))
	w.Withhold("B", p)
	pull(t, w, "C", "B")
	b := w.Branch("C", p)
	wantTexts(t, b, "shown:reply")
	if b[0].ParentIs != ParentMissing {
		t.Fatalf("parent is %v, want missing", b[0].ParentIs)
	}
}

// Requirements: D10, T3. The branch under a message a machine added
// without history holds only as a header lists the later reply to it, with
// the parent marked missing.
func TestBranchUnderAHeaderWithoutTextListsItsRepliesWithTheParentMissing(t *testing.T) {
	w := room("A", "B")
	p := must(w.Send("A", "a", Msg{Body: "parent"}))
	must(w.Admit("C", "A", false, "c"))
	pull(t, w, "B", "A")
	w.Advance(Hour)
	must(w.Send("B", "b", Msg{Body: "reply", Parent: p}))
	pull(t, w, "C", "A")
	pull(t, w, "C", "B")
	b := w.Branch("C", p)
	wantTexts(t, b, "shown:reply")
	if b[0].ParentIs != ParentMissing {
		t.Fatalf("parent is %v, want missing", b[0].ParentIs)
	}
}

// Requirements: U1. "Marked for everyone" is the text's own "@everyone"
// mark, not ask_handled.
func TestMarkedForEveryoneIsTheTextsOwnMark(t *testing.T) {
	for text, want := range map[string]bool{
		"@everyone": true, "@everyone please read": true, "@EVERYONE: stop": true,
		"@Everyone, hi": true, "@everyone\nnext": true, "@everyone-wide": true,
		"@everyones": false, "@everyone2": false, " @everyone": false, "hi @everyone": false,
		"everyone": false, "@every": false, "": false,
	} {
		if got := MarkedForEveryone(text); got != want {
			t.Errorf("MarkedForEveryone(%q) = %v, want %v", text, got, want)
		}
	}
	w := levelRoom()
	if err := w.SetLevel("A", "a", Addressed); err != nil {
		t.Fatal(err)
	}
	post(w, Msg{Body: "asks", AskHandled: true})
	post(w, Msg{Body: "@everyone look"})
	pull(t, w, "A", "B")
	wantTexts(t, readAs(t, w, "A", "a"), "shown:@everyone look")
	wantTexts(t, w.Skipped("A", "a"), "shown:asks")
}
