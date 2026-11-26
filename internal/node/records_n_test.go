package node

// Tests of the records group of the 2026-11-25 security review: a
// replaced message is marked edited in the node's views (F30), a handled
// report or a reply is tied to the text the agent was shown (F67), and
// receipts are kept only for the room generation the read saw (F70).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

func editedOf(t *testing.T, what string, es []Entry, id string) bool {
	t.Helper()
	for _, e := range es {
		if e.ID == id && e.Kind == EntryMessage {
			return e.Edited
		}
	}
	t.Fatalf("%s: no message entry for %s in %+v", what, id, es)
	return false
}

// F30: the author replaced a question after it was answered, and thread,
// branch and history showed the new text above the answer as if it was
// the text answered, with nothing to tell them apart.
func TestThreadBranchAndHistoryMarkAMessageReplacedAfterItsReply(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	q := e.send(SendInput{Agent: alice, Text: "May I run the read-only tests?", To: []string{bob}, Choices: []string{"Yes", "No"}})
	e.read(bob)
	yes := e.send(SendInput{Agent: bob, Text: "Yes", ReplyTo: q.ID})
	e.send(SendInput{Agent: alice, Replaces: q.ID, Text: "May I force-push main?"})

	th, err := e.s.Thread(ctx, bob, q.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !editedOf(t, "thread", th, q.ID) || editedOf(t, "thread", th, yes.ID) {
		t.Fatalf("thread: question edited %v, answer edited %v; want true, false",
			editedOf(t, "thread", th, q.ID), editedOf(t, "thread", th, yes.ID))
	}
	br, err := e.s.m.Branch(ctx, e.s.local, q.ID)
	if err != nil || !editedOf(t, "branch", br, q.ID) {
		t.Fatalf("branch %+v %v: question not marked edited", br, err)
	}
	hist, err := e.s.History(ctx, "", "", "", 0)
	if err != nil || !editedOf(t, "history", hist, q.ID) || editedOf(t, "history", hist, yes.ID) {
		t.Fatalf("history %+v %v: question not marked edited", hist, err)
	}
}

// The honest case: a message never replaced is not marked, in the inbox
// or in its thread, and a replaced one shown fresh in an inbox is.
func TestAMessageNeverReplacedIsNotMarkedEditedAndOneShownFreshAfterAReplacementIs(t *testing.T) {
	r := newRig(t)
	plain := r.send("alice", Draft{To: r.to("bob"), Text: "plain"})
	changed := r.send("alice", Draft{To: r.to("bob"), Text: "v0"})
	r.tick()
	if _, err := r.m.Replace(context.Background(), mRoom, "alice", changed, "v1"); err != nil {
		t.Fatal(err)
	}
	es := r.inbox("bob")
	if editedOf(t, "inbox", es, plain) || !editedOf(t, "inbox", es, changed) {
		t.Fatalf("inbox %+v: want only %s edited", es, changed)
	}
	th, err := r.m.Thread(context.Background(), mRoom, plain)
	if err != nil || editedOf(t, "thread", th, plain) {
		t.Fatalf("thread of a plain message %+v %v", th, err)
	}
}

// F67: bob was shown "May I delete the staging database?"; its author
// then replaced it with "...the production database?". Bob's handled
// report and his reply to the version shown went through, so they read
// as answers to text he never saw.
func TestAHandledReportOrReplyToAVersionReplacedSinceTheReadIsRefused(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	q := r.send("alice", Draft{To: r.to("bob"), Text: "May I delete the staging database?"})
	shown := r.inbox("bob")
	r.tick()
	if _, err := r.m.Replace(ctx, mRoom, "alice", q, "May I delete the production database?"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Handled(ctx, mRoom, "bob", q); !errors.Is(err, ErrNotAllowed) || !errors.Is(err, ErrAckTextChanged) {
		t.Fatalf("handled after a replacement since the read: %v, want %v", err, ErrAckTextChanged)
	}
	_, err := r.m.Send(ctx, Draft{Room: mRoom, Agent: "bob", Text: "yes", ReplyTo: q, ReplyVersion: shown[0].Version})
	if !errors.Is(err, ErrReplyTextChanged) {
		t.Fatalf("reply to the version shown before the replacement: %v, want %v", err, ErrReplyTextChanged)
	}
	// Confirmed from the inbox, then replaced: the same.
	q2 := r.send("alice", Draft{To: r.to("bob"), Text: "May I read the logs?"})
	r.read("bob")
	r.tick()
	if _, err := r.m.Replace(ctx, mRoom, "alice", q2, "May I delete the logs?"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Handled(ctx, mRoom, "bob", q2); !errors.Is(err, ErrAckTextChanged) {
		t.Fatalf("handled after a confirmed read and a replacement: %v", err)
	}
}

// The honest case: once the new text is shown, the report and the reply
// go through; a reply without a version is not checked, as before.
func TestAHandledReportOrReplyAfterTheNewTextIsShownGoesThrough(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	q := r.send("alice", Draft{To: r.to("bob"), Text: "v0"})
	r.read("bob")
	r.tick()
	v1, err := r.m.Replace(ctx, mRoom, "alice", q, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Send(ctx, Draft{Room: mRoom, Agent: "bob", Text: "no version", ReplyTo: q}); err != nil {
		t.Fatalf("reply without a version: %v", err)
	}
	es := r.inbox("bob")
	if len(es) != 1 || es[0].Kind != EntryReplaced || es[0].Version != v1.ID {
		t.Fatalf("inbox %+v, want the replacement", es)
	}
	if _, err := r.m.Handled(ctx, mRoom, "bob", q); err != nil {
		t.Fatalf("handled once the new text was shown: %v", err)
	}
	if _, err := r.m.Send(ctx, Draft{Room: mRoom, Agent: "bob", Text: "yes", ReplyTo: q, ReplyVersion: v1.ID}); err != nil {
		t.Fatalf("reply to the version shown: %v", err)
	}
	// A message never replaced: handled after a read, as before.
	m := r.send("alice", Draft{To: r.to("bob"), Text: "plain"})
	r.inbox("bob")
	if _, err := r.m.Handled(ctx, mRoom, "bob", m); err != nil {
		t.Fatalf("handled a plain message: %v", err)
	}
}

// F70: the receipts of an inbox read are written after it, in their own
// transaction. Entries carry the room's generation the read saw, and a
// receipt is written only while the room is still at it, so a read from
// before a leave and a rejoin cannot let Seen confirm in the new one.
func TestReceiptsOfAReadAreKeptOnlyForTheGenerationItSaw(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.send("alice", Draft{To: r.to("bob"), Text: "hi"})
	v, err := r.m.Inbox(ctx, mRoom, "bob")
	if err != nil || len(v.Entries) != 1 || v.Entries[0].gen == 0 {
		t.Fatalf("inbox %+v %v: want one entry with its generation", v.Entries, err)
	}
	stale := append([]Entry(nil), v.Entries...)
	stale[0].gen++ // as if the room was left and joined again since the read
	if err := r.m.Showed(ctx, mRoom, "bob", stale, true); err != nil {
		t.Fatal(err)
	}
	if res := r.confirm("bob", v.Entries); res[0].Confirmed || res[0].Reason != SeenNotShown {
		t.Fatalf("seen after receipts of another generation: %+v, want %s", res[0], SeenNotShown)
	}
	// The honest case: the read's own generation keeps its receipts.
	if err := r.m.Showed(ctx, mRoom, "bob", v.Entries, true); err != nil {
		t.Fatal(err)
	}
	if res := r.confirm("bob", v.Entries); !res[0].Confirmed {
		t.Fatalf("seen after a read of this generation: %+v", res[0])
	}
}

// F67: a replacement whose header arrived without its text made the inbox
// and the thread wait, yet a handled report or a reply naming the old
// version went through, as answers to text that was already replaced.
// Both wait like the views, and go through once the text is repaired and
// shown.
func TestAHandledReportOrReplyWaitsForAReplacementWhoseTextIsMissingUntilItIsRepairedAndShown(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	q := r.send("alice", Draft{To: r.to("bob"), Text: "May I read the logs?"})
	shown := r.read("bob")
	rec, err := core.NewRecord(r.key, core.RecordFields{Room: mRoom, Epoch: "00000000000000d2", Seq: 1, Created: r.now,
		Kind: core.KindReplace, Agent: "alice", Ref: q, Version: 1, Text: "May I delete the logs?"})
	if err != nil {
		t.Fatal(err)
	}
	header, err := rec.WithoutBody().Verified()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := r.st.PutRecord(ctx, header, store.PutOpts{Now: r.now, MayRead: true}); err != nil || out != store.OutcomeStored {
		t.Fatalf("put header: %s %v", out, err)
	}
	if _, err := r.m.Handled(ctx, mRoom, "bob", q); !errors.Is(err, ErrAckTextPending) {
		t.Fatalf("handled while the replacement's text is missing: %v, want %v", err, ErrAckTextPending)
	}
	if _, err := r.m.Send(ctx, Draft{Room: mRoom, Agent: "bob", Text: "yes", ReplyTo: q, ReplyVersion: shown[0].Version}); !errors.Is(err, ErrReplyTextPending) {
		t.Fatalf("reply while the replacement's text is missing: %v, want %v", err, ErrReplyTextPending)
	}
	// The text arrives: still refused until a read shows it.
	full, err := rec.Verified()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.PutRecord(ctx, full, store.PutOpts{Now: r.now, MayRead: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Handled(ctx, mRoom, "bob", q); !errors.Is(err, ErrAckTextChanged) {
		t.Fatalf("handled after the repair, before a read: %v, want %v", err, ErrAckTextChanged)
	}
	es := r.inbox("bob")
	if len(es) != 1 || es[0].Version != full.ID() {
		t.Fatalf("inbox after the repair %+v, want the replacement", es)
	}
	if _, err := r.m.Handled(ctx, mRoom, "bob", q); err != nil {
		t.Fatalf("handled once the repaired text was shown: %v", err)
	}
	if _, err := r.m.Send(ctx, Draft{Room: mRoom, Agent: "bob", Text: "no", ReplyTo: q, ReplyVersion: full.ID()}); err != nil {
		t.Fatalf("reply to the repaired version shown: %v", err)
	}
}

// F67: once the first text expired, the check was skipped, although the
// inbox still showed the live replacement, so a handled report went
// through for a reader shown only the first text. A withdrawn message,
// and an expired one never replaced, are reported as before.
func TestAHandledReportOfAnExpiredMessageNeedsItsLiveReplacementShown(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	q := r.send("alice", Draft{To: r.to("bob"), Text: "v0"})
	plain := r.send("alice", Draft{To: r.to("bob"), Text: "plain"})
	gone := r.send("alice", Draft{To: r.to("bob"), Text: "taken back"})
	r.read("bob")
	r.now = r.now.Add(store.Retention - time.Hour)
	if _, err := r.m.Replace(ctx, mRoom, "alice", q, "v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Withdraw(ctx, mRoom, "alice", gone); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(2 * time.Hour) // the first texts expire, the replacement is live
	if _, err := r.m.Handled(ctx, mRoom, "bob", q); !errors.Is(err, ErrAckTextChanged) {
		t.Fatalf("handled with only the expired text shown: %v, want %v", err, ErrAckTextChanged)
	}
	if _, err := r.m.Handled(ctx, mRoom, "bob", plain); err != nil {
		t.Fatalf("handled an expired message never replaced: %v", err)
	}
	if _, err := r.m.Handled(ctx, mRoom, "bob", gone); err != nil {
		t.Fatalf("handled a withdrawn message: %v", err)
	}
	r.inbox("bob")
	if _, err := r.m.Handled(ctx, mRoom, "bob", q); err != nil {
		t.Fatalf("handled once the replacement was shown: %v", err)
	}

	// A reader that listed the message without confirming it, or read it
	// in its thread, is shown only the expired notice by every view: the
	// inbox shows the replace notice only after a confirmed read. The
	// version is then not checked, as in 0.2.3, or no read could clear
	// the refusal.
	r = newRig(t)
	q = r.send("alice", Draft{To: r.to("bob"), Text: "v0"})
	r.inbox("bob")
	r.now = r.now.Add(store.Retention - time.Hour)
	if _, err := r.m.Replace(ctx, mRoom, "alice", q, "w1"); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(2 * time.Hour)
	if err := r.m.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	for _, e := range r.inbox("bob") {
		if e.Text != "" || e.Kind == EntryReplaced {
			t.Fatalf("the inbox of an unconfirmed reader shows the replacement: %+v", e)
		}
	}
	th, err := r.m.Thread(ctx, mRoom, q)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.m.Showed(ctx, mRoom, "bob", th, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Handled(ctx, mRoom, "bob", q); err != nil {
		t.Fatalf("handled with every view showing only the expired notice: %v", err)
	}
}
