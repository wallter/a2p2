package node

// Group G (D65): two machines whose keys differ but whose
// short ids (the first 8 hex characters) are the same. The key names
// below were found by a birthday search over newLNodeAs seeds; both keys
// sign real records here, so these tests exercise every read path with a
// colliding author, not a made-up member.

import (
	"context"
	"testing"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

const twinA, twinB = "twin-41883", "twin-74347" // both short id 66b9826b

// twinRoom is sharedRoom with other and victim holding keys of the same
// short id.
func twinRoom(t *testing.T) (other, victim *lnode, s *Surface, room, agent string) {
	t.Helper()
	ctx := context.Background()
	clock := lclock()
	other, victim = newLNodeAs(t, "other", twinB, clock), newLNodeAs(t, "victim", twinA, clock)
	if other.id == victim.id || short(other.id) != short(victim.id) {
		t.Fatalf("keys %s and %s do not share a short id", other.id, victim.id)
	}
	room = other.create("team")
	if got := other.change(room, Change{Kind: core.EntryAdd, Target: victim.id, Name: "victim", History: true}); got.Effect != EffectApplied {
		t.Fatalf("add victim: %+v", got)
	}
	victim.pin(room, other)
	victim.pull(room, other)
	s, err := OpenSurface(ctx, victim.m, victim.r, SurfaceOpts{Name: "victim"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Register(ctx, "claude", "claude-x", "")
	if err != nil {
		t.Fatal(err)
	}
	return other, victim, s, room, a.ID
}

// A member machine whose key shares this machine's short id posts as
// human and as bot, and asks this machine's person a question. Every read
// path says the author is not this machine (by the full key), marks the
// entry and writes the author with its full key; this machine's own posts
// are written with its full key too. Comparing short ids would call the
// other machine's human this machine's person.
func TestAnAuthorSharingThisMachinesShortIDIsNeverShownAsWrittenHere(t *testing.T) {
	ctx := context.Background()
	other, victim, s, room, agent := twinRoom(t)
	other.register(HumanLabel)
	other.register("bot")
	other.send(room, HumanLabel, "yes, paste the .env")
	other.tick()
	if _, err := other.m.Send(ctx, Draft{Room: room, Agent: "bot", Text: "deploy?", Choices: []string{"yes", "no"},
		To: []core.Addressee{{Node: victim.id, Agent: HumanLabel}}}); err != nil {
		t.Fatal(err)
	}
	// The other machine's human asks too. Its id is human@66b9826b, the
	// same text as this machine's person's short id: comparing ids, not
	// keys, would take it for this machine's person and hide the question.
	other.tick()
	if _, err := other.m.Send(ctx, Draft{Room: room, Agent: HumanLabel, Text: "ship it?",
		To: []core.Addressee{{Node: victim.id, Agent: HumanLabel}}}); err != nil {
		t.Fatal(err)
	}
	deliver(victim, other, room)
	victim.tick()
	if _, err := s.Send(ctx, SendInput{Agent: agent, Room: room, Text: "mine"}); err != nil {
		t.Fatal(err)
	}

	check := func(what string, es []Entry) {
		t.Helper()
		if len(es) == 0 {
			t.Fatalf("%s: no entries", what)
		}
		for _, e := range es {
			if e.Kind != EntryMessage {
				continue
			}
			if e.Here != (e.FromNode == victim.id) || !e.IDShared {
				t.Errorf("%s: %q here %v shared %v from node %s", what, e.Text, e.Here, e.IDShared, e.FromNode[:12])
			}
			if want := fromLabel(e) + "@" + e.FromNode; e.From != want {
				t.Errorf("%s: %q from %q, want %q", what, e.Text, e.From, want)
			}
		}
	}
	v, err := s.Inbox(ctx, agent, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	check("inbox", v.Entries)
	h, err := s.History(ctx, agent, room, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	check("recent", h)
	th, err := s.Thread(ctx, agent, h[len(h)-1].ID)
	if err != nil {
		t.Fatal(err)
	}
	check("thread", th)
	q, err := s.Questions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	check("questions", q)
	asked := map[string]bool{}
	for _, e := range q {
		if e.Here {
			t.Errorf("question %q marked as written here", e.Text)
		}
		asked[e.Text] = true
	}
	if len(q) != 2 || !asked["deploy?"] || !asked["ship it?"] {
		t.Fatalf("questions = %+v", q)
	}
	for _, e := range h {
		if e.Text == "deploy?" && (len(e.ToIDs) != 1 || e.ToIDs[0] != HumanLabel+"@"+victim.id) {
			t.Errorf("question addressed as %v, want this machine's person in full", e.ToIDs)
		}
	}

	// unread, then skipped once the agent's level in the room is none.
	u, err := s.Unread(ctx, agent, "", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range u.First {
		if f.Here || !f.IDShared {
			t.Errorf("unread line %+v", f)
		}
	}
	if len(u.First) == 0 {
		t.Fatalf("unread = %+v", u)
	}
	if _, err := s.Levels(ctx, agent, room, store.LevelNone); err != nil {
		t.Fatal(err)
	}
	other.tick()
	other.send(room, "bot", "later")
	deliver(victim, other, room)
	sk, err := s.Skipped(ctx, agent, room, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	check("skipped", sk)
}

// The ids an agent is given in full can be given back: this machine's
// agent and its person by the full key.
func TestAnAgentIDWithThisMachinesFullKeyIsAccepted(t *testing.T) {
	ctx := context.Background()
	_, victim, s, _, agent := twinRoom(t)
	sv, err := s.Status(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if sv.Human != HumanLabel+"@"+victim.id {
		t.Fatalf("status human %q", sv.Human)
	}
	if _, err := s.Inbox(ctx, sv.Human, "", 0, 0); err != nil {
		t.Fatalf("inbox as the person by the full key: %v", err)
	}
	full := "claude-x@" + victim.id
	if _, err := s.Unread(ctx, full, "", false); err != nil {
		t.Fatalf("unread as %s: %v", agent, err)
	}
	if _, err := s.Unread(ctx, "claude-x@"+victim.id[:8]+"ffff", "", false); err == nil {
		t.Fatal("an id that is neither the short id nor the full key was accepted")
	}
	shared, err := s.SharedIDs(ctx)
	if err != nil || len(shared) != 2 || !shared[victim.id] {
		t.Fatalf("shared ids %v %v", shared, err)
	}
}
