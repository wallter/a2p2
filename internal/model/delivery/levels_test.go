package delivery

import (
	"math/rand/v2"
	"testing"
)

// levelRoom is machine A with agents a and b, and machine B with agent x.
func levelRoom() *World {
	w := NewWorld()
	w.AddMachine("A", "a", "b")
	w.AddMachine("B", "x")
	return w
}

// post sends from x on B an hour after the last step, so that created
// times order the messages.
func post(w *World, msg Msg) RecordID {
	w.Advance(Hour)
	return must(w.Send("B", "x", msg))
}

func readAs(t *testing.T, w *World, m MachineID, agent string) []Entry {
	t.Helper()
	v := must(w.List(m, agent))
	if err := w.Seen(m, agent, v.Entries); err != nil {
		t.Fatal(err)
	}
	return v.Entries
}

// Requirements: U1
func TestAddressedLevelShowsWhatIsAddressedAndPostsMarkedForEveryone(t *testing.T) {
	w := levelRoom()
	if err := w.SetLevel("A", "a", Addressed); err != nil {
		t.Fatal(err)
	}
	post(w, Msg{Body: "room-wide"})
	post(w, Msg{Body: "@everyone marked"})
	post(w, Msg{Body: "asks only", AskHandled: true})
	post(w, Msg{Body: "to a", To: []Addr{{"A", "a"}}})
	root := must(w.Send("A", "a", Msg{Body: "a asks"}))
	pull(t, w, "A", "B")
	pull(t, w, "B", "A")
	post(w, Msg{Body: "reply to a", Parent: root})
	post(w, Msg{Body: "reply to all", Parent: root, Everyone: true})
	pull(t, w, "A", "B")
	wantTexts(t, readAs(t, w, "A", "a"), "shown:@everyone marked shown:to a shown:reply to a")
	// Another agent on the same machine is unaffected.
	wantTexts(t, readAs(t, w, "A", "b"), "shown:a asks shown:room-wide shown:@everyone marked shown:asks only shown:reply to all")
	wantTexts(t, w.Skipped("A", "a"), "shown:room-wide shown:asks only shown:reply to all")
}

// Requirements: U1, U3
func TestNoneLevelShowsNothingUntilTheAgentAsks(t *testing.T) {
	w := levelRoom()
	if err := w.SetLevel("A", "a", None); err != nil {
		t.Fatal(err)
	}
	post(w, Msg{Body: "to a", To: []Addr{{"A", "a"}}})
	post(w, Msg{Body: "@Everyone: marked"})
	pull(t, w, "A", "B")
	wantTexts(t, readAs(t, w, "A", "a"), "")
	wantTexts(t, w.Skipped("A", "a"), "shown:to a shown:@Everyone: marked")
	wantTexts(t, w.Skipped("A", "a"), "shown:to a shown:@Everyone: marked") // asking marks nothing
}

// Requirements: U2, U3
func TestRaisingTheLevelKeepsWhatWasSkippedOutOfTheInboxAndReadableApart(t *testing.T) {
	w := levelRoom()
	if err := w.SetLevel("A", "a", Addressed); err != nil {
		t.Fatal(err)
	}
	post(w, Msg{Body: "one"})
	pull(t, w, "A", "B")
	readAs(t, w, "A", "a") // the cursor moves past "one"
	post(w, Msg{Body: "two"})
	pull(t, w, "A", "B")
	if err := w.SetLevel("A", "a", All); err != nil {
		t.Fatal(err)
	}
	post(w, Msg{Body: "three"})
	pull(t, w, "A", "B")
	wantTexts(t, readAs(t, w, "A", "a"), "shown:three")
	wantTexts(t, w.Skipped("A", "a"), "shown:one shown:two")
	// Lowering again: nothing new is marked, the current level keeps the
	// room-wide posts out, and raising once more shows nothing old.
	if err := w.SetLevel("A", "a", None); err != nil {
		t.Fatal(err)
	}
	post(w, Msg{Body: "four"})
	pull(t, w, "A", "B")
	if err := w.SetLevel("A", "a", All); err != nil {
		t.Fatal(err)
	}
	wantTexts(t, readAs(t, w, "A", "a"), "")
	wantTexts(t, w.Skipped("A", "a"), "shown:one shown:two shown:four")
}

// Requirements: U1, U3, D3. Random level changes by one agent: whatever
// the changes, every message for it is either shown in its inbox once or
// listed as skipped, never both; another agent at the default level sees
// exactly what an agent that never had levels sees.
func TestRandomLevelChangesNeitherLoseNorRepeatAMessage(t *testing.T) {
	r := rand.New(rand.NewPCG(20261124, 1))
	for run := 0; run < 200; run++ {
		w := levelRoom()
		shown := map[RecordID]int{}
		var forA []RecordID
		var forB []RecordID
		for step := 0; step < 30; step++ {
			switch r.IntN(6) {
			case 0:
				if err := w.SetLevel("A", "a", Level(r.IntN(3))); err != nil {
					t.Fatal(err)
				}
			case 1, 2:
				msg := Msg{Body: "m"}
				switch r.IntN(3) {
				case 0:
					msg.To = []Addr{{"A", "a"}}
				case 1:
					msg.Body = "@everyone m"
				}
				id := post(w, msg)
				forA = append(forA, id)
				if len(msg.To) == 0 {
					forB = append(forB, id)
				}
				pull(t, w, "A", "B")
			case 3:
				for _, e := range readAs(t, w, "A", "a") {
					shown[e.Message]++
				}
			case 4:
				must(w.Send("A", "b", Msg{Body: "own"}))
			default:
				pull(t, w, "A", "B")
			}
		}
		for _, e := range readAs(t, w, "A", "a") {
			shown[e.Message]++
		}
		skipped := map[RecordID]bool{}
		for _, e := range w.Skipped("A", "a") {
			skipped[e.Message] = true
		}
		for _, id := range forA {
			if n := shown[id]; n > 1 || (n == 1) == skipped[id] {
				t.Fatalf("run %d: message shown %d times, skipped %v", run, n, skipped[id])
			}
		}
		var gotB []RecordID
		for _, e := range readAs(t, w, "A", "b") {
			gotB = append(gotB, e.Message)
		}
		if len(gotB) != len(forB) {
			t.Fatalf("run %d: agent b at the default level was shown %d of %d", run, len(gotB), len(forB))
		}
		if len(w.Skipped("A", "b")) != 0 {
			t.Fatalf("run %d: agent b at the default level has skipped messages", run)
		}
	}
}

// Requirements: R7, U3. An agent registered after a brief was sent to it,
// from this machine or another, reads the brief; room-wide history from
// before, and a message to the same label on another machine, it does
// not. At level none the brief is in Skipped. Registering again changes
// nothing.
func TestAgentRegisteredLaterReadsWhatWasAddressedToItButNotTheRoomsHistory(t *testing.T) {
	w := levelRoom()
	post(w, Msg{Body: "room-wide"})
	post(w, Msg{Body: "remote brief", To: []Addr{{"A", "w"}}})
	post(w, Msg{Body: "for w on B", To: []Addr{{"B", "w"}}})
	pull(t, w, "A", "B")
	w.Advance(Hour)
	must(w.Send("A", "a", Msg{Body: "local brief", To: []Addr{{"A", "w"}, {"A", "v"}}}))
	if _, err := w.Inbox("A", "w"); err == nil {
		t.Fatal("an agent never registered has an inbox")
	}
	for _, a := range []string{"w", "v"} {
		if err := w.Register("A", a); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.SetLevel("A", "v", None); err != nil {
		t.Fatal(err)
	}
	wantTexts(t, readAs(t, w, "A", "w"), "shown:local brief shown:remote brief")
	wantTexts(t, readAs(t, w, "A", "v"), "")
	wantTexts(t, w.Skipped("A", "v"), "shown:local brief")
	if err := w.Register("A", "w"); err != nil {
		t.Fatal(err)
	}
	post(w, Msg{Body: "after"})
	pull(t, w, "A", "B")
	wantTexts(t, readAs(t, w, "A", "w"), "shown:after")
}
