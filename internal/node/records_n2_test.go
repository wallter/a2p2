package node

// Tests of the second review of D62: an edited mark an expiring
// replacement used to erase, and a reply without a version to text
// replaced since the read.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// putBackdated stores a replacement of msg by alice, version v, dated so
// that its text expires left after it arrives, as a remote author can.
func (r *rig) putBackdated(msg, epoch string, v int64, text string, left time.Duration) string {
	r.t.Helper()
	rec, err := core.NewRecord(r.key, core.RecordFields{Room: mRoom, Epoch: epoch, Seq: 1, Created: r.now.Add(-store.Retention + left),
		Kind: core.KindReplace, Agent: "alice", Ref: msg, Version: v, Text: text})
	if err != nil {
		r.t.Fatal(err)
	}
	vr, err := rec.Verified()
	if err != nil {
		r.t.Fatal(err)
	}
	if out, err := r.st.PutRecord(context.Background(), vr, store.PutOpts{Now: r.now, MayRead: true}); err != nil || out != store.OutcomeStored {
		r.t.Fatalf("put the replacement: %s %v", out, err)
	}
	return vr.ID()
}

// viewEntry is the entry of message id in es, or fails.
func viewEntry(t *testing.T, what string, es []Entry, id string) Entry {
	t.Helper()
	for _, e := range es {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("%s: no entry for %s in %+v", what, id, es)
	return Entry{}
}

// Gemini's must-fix 4: an author posted "deploy A", replaced it with
// "deploy B" dated to expire minutes after it arrived, and once it expired
// every view fell back to "deploy A" with no edited mark, as if the
// message had never changed. Every view now shows the message as expired
// and edited, and none shows the first text as current.
func TestAnExpiredReplacementKeepsTheMessageMarkedEditedAndNeverShowsItsFirstText(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	q := r.send("alice", Draft{To: r.to("bob", "carol"), Text: "deploy A"})
	r.putBackdated(q, "00000000000000d2", 1, "deploy B", 5*time.Minute)
	if e := viewEntry(t, "bob's inbox", r.inbox("bob"), q); e.Text != "deploy B" || !e.Edited {
		t.Fatalf("bob's inbox before the expiry: %+v", e)
	}
	r.now = r.now.Add(10 * time.Minute)
	if err := r.m.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	th, err := r.m.Thread(ctx, mRoom, q)
	if err != nil {
		t.Fatal(err)
	}
	br, err := r.m.Branch(ctx, mRoom, q)
	if err != nil {
		t.Fatal(err)
	}
	for what, es := range map[string][]Entry{"thread": th, "branch": br, "carol's inbox": r.inbox("carol")} {
		e := viewEntry(t, what, es, q)
		if e.Kind != EntryExpired || !e.Edited || e.Text != "" || e.Version != "" {
			t.Fatalf("%s after the replacement expired: %+v, want an expired entry marked edited", what, e)
		}
	}
	// Bob was shown the replacement: his report goes through, as for any
	// message whose text expired.
	if _, err := r.m.Handled(ctx, mRoom, "bob", q); err != nil {
		t.Fatalf("handled by the reader shown the replacement: %v", err)
	}
}

// The honest case: a newer live replacement after one that expired is
// shown, marked edited, and a message never replaced that expires is not
// marked.
func TestALiveReplacementAfterAnExpiredOneIsShownAndAnExpiredMessageNeverReplacedIsNotMarked(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	q := r.send("alice", Draft{To: r.to("bob"), Text: "deploy A"})
	r.putBackdated(q, "00000000000000d2", 1, "deploy B", 5*time.Minute)
	r.now = r.now.Add(10 * time.Minute)
	if err := r.m.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Replace(ctx, mRoom, "alice", q, "deploy C"); err != nil {
		t.Fatal(err)
	}
	th, err := r.m.Thread(ctx, mRoom, q)
	if err != nil {
		t.Fatal(err)
	}
	if e := viewEntry(t, "thread", th, q); e.Kind != EntryMessage || e.Text != "deploy C" || !e.Edited {
		t.Fatalf("thread with a newer live replacement: %+v", e)
	}
	if e := viewEntry(t, "inbox", r.inbox("bob"), q); e.Kind != EntryMessage || e.Text != "deploy C" || !e.Edited {
		t.Fatalf("inbox with a newer live replacement: %+v", e)
	}

	plain := r.send("alice", Draft{To: r.to("carol"), Text: "plain"})
	r.now = r.now.Add(store.Retention + time.Hour)
	if err := r.m.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if e := viewEntry(t, "carol's inbox", r.inbox("carol"), plain); e.Kind != EntryExpired || e.Edited {
		t.Fatalf("an expired message never replaced: %+v, want expired and not edited", e)
	}
}

// GPT's must-fix 2: the CLI's reply and the MCP reply tool pass no
// version, so a reply by an addressee shown the first text committed
// against a replacement it never saw. Without a version, a reply by an
// agent that may report the message handled and was shown some text of
// it is held to Handled's rule.
func TestAReplyWithoutAVersionToTextReplacedSinceTheReadIsRefused(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	q := r.send("alice", Draft{To: r.to("bob"), Text: "May I delete the staging database?"})
	r.read("bob")
	r.tick()
	if _, err := r.m.Replace(ctx, mRoom, "alice", q, "May I delete the production database?"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Send(ctx, Draft{Room: mRoom, Agent: "bob", Text: "yes", ReplyTo: q}); !errors.Is(err, ErrReplyTextChanged) {
		t.Fatalf("reply without a version after a confirmed read and a replacement: %v, want %v", err, ErrReplyTextChanged)
	}
	// Listed and not confirmed: the same.
	q2 := r.send("alice", Draft{To: r.to("bob"), Text: "May I read the logs?"})
	r.inbox("bob")
	r.tick()
	if _, err := r.m.Replace(ctx, mRoom, "alice", q2, "May I delete the logs?"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Send(ctx, Draft{Room: mRoom, Agent: "bob", Text: "yes", ReplyTo: q2}); !errors.Is(err, ErrReplyTextChanged) {
		t.Fatalf("reply without a version after a listing and a replacement: %v, want %v", err, ErrReplyTextChanged)
	}
}

// The honest case: once the new text is shown the reply goes through;
// a reply by an addressee no read showed the message, or by an agent the
// message is not addressed to, is not checked, as before.
func TestAReplyWithoutAVersionGoesThroughOnceTheNewTextIsShownOrWhenNothingWasShown(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	q := r.send("alice", Draft{To: r.to("bob", "dave"), Text: "v0"})
	r.read("bob")
	r.tick()
	if _, err := r.m.Replace(ctx, mRoom, "alice", q, "v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Send(ctx, Draft{Room: mRoom, Agent: "dave", Text: "unread", ReplyTo: q}); err != nil {
		t.Fatalf("reply by an addressee never shown the message: %v", err)
	}
	if _, err := r.m.Send(ctx, Draft{Room: mRoom, Agent: "carol", Text: "not mine", ReplyTo: q}); err != nil {
		t.Fatalf("reply by an agent the message is not addressed to: %v", err)
	}
	r.inbox("bob")
	if _, err := r.m.Send(ctx, Draft{Room: mRoom, Agent: "bob", Text: "yes", ReplyTo: q}); err != nil {
		t.Fatalf("reply once the new text was shown: %v", err)
	}
}
