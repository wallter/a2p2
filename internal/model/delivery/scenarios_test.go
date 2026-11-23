package delivery

import (
	"errors"
	"strings"
	"testing"
)

// room makes a world of awake machines; machine "A" has one agent "a".
func room(names ...string) *World {
	w := NewWorld()
	for _, n := range names {
		w.AddMachine(MachineID(n), strings.ToLower(n))
	}
	return w
}

func ag(m MachineID) string { return strings.ToLower(strings.TrimRight(string(m), "'")) }
func at(m MachineID) Addr   { return Addr{m, ag(m)} }

// must panics on error; a panic fails the test with the error.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func send(t *testing.T, w *World, from, to MachineID, body string) RecordID {
	t.Helper()
	return must(w.Send(from, ag(from), Msg{Body: body, To: []Addr{at(to)}}))
}

func pull(t *testing.T, w *World, p, s MachineID) PullResult {
	t.Helper()
	return must(w.Pull(p, s, PullOpt{}))
}

// read shows the machine's agent its inbox and confirms all of it.
func read(t *testing.T, w *World, m MachineID) []Entry {
	t.Helper()
	v := must(w.Inbox(m, ag(m)))
	if err := w.Seen(m, ag(m), v.Entries); err != nil {
		t.Fatal(err)
	}
	return v.Entries
}

func texts(es []Entry) string {
	var out []string
	for _, e := range es {
		s := e.Kind.String() + ":" + e.Text
		if e.Late {
			s += "(late)"
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func wantTexts(t *testing.T, es []Entry, want string) {
	t.Helper()
	if got := texts(es); got != want {
		t.Fatalf("shown %q, want %q", got, want)
	}
}

// awakeLog records, per step, which machines were awake, to prove two of
// them never were at the same time.
type awakeLog struct{ together bool }

func (l *awakeLog) check(w *World, a, b MachineID) {
	if w.Machine(a).Awake && w.Machine(b).Awake {
		l.together = true
	}
}

// Scenario 1. Requirements: D2, D5, P2
func TestRelayDeliversAndReportsHandledWithoutAuthorAndReaderEverAwakeTogether(t *testing.T) {
	w := room("A", "B", "C")
	var log awakeLog
	w.Sleep("C")
	m := send(t, w, "A", "C", "deploy?")
	log.check(w, "A", "C")
	pull(t, w, "B", "A")
	w.Sleep("A")
	w.Advance(8 * Hour)
	w.Wake("C")
	log.check(w, "A", "C")
	pull(t, w, "C", "B")
	wantTexts(t, read(t, w, "C"), "shown:deploy?")
	must(w.Handle("C", "c", m))
	pull(t, w, "B", "C")
	w.Sleep("C")
	w.Advance(8 * Hour)
	w.Wake("A")
	log.check(w, "A", "C")
	pull(t, w, "A", "B")
	if log.together {
		t.Fatal("A and C were awake together")
	}
	st := w.Status("A", m)
	if len(st.Handled) != 1 || st.Handled[0].Addr != at("C") {
		t.Fatalf("handled = %+v, want C/c", st.Handled)
	}
	if len(st.Copies) != 1 || st.Copies[0].Machine != "B" {
		t.Fatalf("copies = %+v, want B only (A never met C)", st.Copies)
	}
}

// Scenario 2. Requirements: D5, limit "never awake together"
func TestTwoMachinesNeverAwakeTogetherExchangeNothingAndSenderSeesNoCopy(t *testing.T) {
	w := room("A", "B")
	w.Sleep("B")
	m := send(t, w, "A", "B", "hello")
	if _, err := w.Pull("A", "B", PullOpt{}); !errors.Is(err, ErrAsleep) {
		t.Fatalf("pull with B asleep: %v", err)
	}
	w.Sleep("A")
	w.Wake("B")
	if _, err := w.Pull("B", "A", PullOpt{}); !errors.Is(err, ErrAsleep) {
		t.Fatalf("pull with A asleep: %v", err)
	}
	wantTexts(t, read(t, w, "B"), "")
	w.Sleep("B")
	w.Wake("A")
	st := w.Status("A", m)
	if !st.Saved || len(st.Copies) != 0 {
		t.Fatalf("status = %+v, want saved here, no copies, not durable", st)
	}
}

// Scenario 3. Requirements: D1, D3, D4, D6
func TestLostReplyRetryAndRestartsGiveOneCopyInOrder(t *testing.T) {
	w := room("A", "B")
	send(t, w, "A", "B", "one")
	if r := must(w.Pull("B", "A", PullOpt{LostReply: true})); r.Sent != 1 {
		t.Fatalf("lost reply sent %d", r.Sent)
	}
	wantTexts(t, read(t, w, "B"), "")
	pull(t, w, "B", "A") // retry
	pull(t, w, "B", "A") // and again: nothing new
	w.Crash("B")         // receiver restart
	pull(t, w, "B", "A")
	w.Crash("A") // sender restart: the next write starts a new stream
	send(t, w, "A", "B", "two")
	if r := must(w.Pull("B", "A", PullOpt{LostReply: true})); r.Sent != 1 {
		t.Fatalf("second lost reply sent %d", r.Sent)
	}
	w.Crash("B")
	pull(t, w, "B", "A")
	pull(t, w, "B", "A")
	wantTexts(t, read(t, w, "B"), "shown:one shown:two")
	wantTexts(t, read(t, w, "B"), "")
	if w.Forks != 0 {
		t.Fatal("fork")
	}
}

// Scenario 4. Requirements: D3, D4, D6
func TestSwitchingSourcesPastAHoleLosesSkipsAndReordersNothing(t *testing.T) {
	w := room("A", "H1", "H2", "C")
	w.Sleep("C")
	var ids []RecordID
	for i := 1; i <= 12; i++ {
		ids = append(ids, send(t, w, "A", "C", string(rune('a'-1+i))))
	}
	pull(t, w, "H1", "A")
	pull(t, w, "H2", "A")
	w.Sleep("A")
	w.Withhold("H1", ids[10]) // H1 holds 12 but will not hand over 11
	w.Wake("C")
	r := pull(t, w, "C", "H1")
	if r.Stored != 10 || r.Sent != 10 {
		t.Fatalf("from H1: %+v, want 10 sent and stored, nothing past the hole", r)
	}
	v := must(w.Inbox("C", "c"))
	if len(v.Waiting) != 1 || v.Waiting[0].Have != 10 || v.Waiting[0].Known != 12 {
		t.Fatalf("waiting = %+v, want a visible gap 11..12", v.Waiting)
	}
	wantTexts(t, read(t, w, "C"), "shown:a shown:b shown:c shown:d shown:e shown:f shown:g shown:h shown:i shown:j")
	pull(t, w, "C", "H2")
	wantTexts(t, read(t, w, "C"), "shown:k shown:l")
	if v := must(w.Inbox("C", "c")); len(v.Waiting) != 0 {
		t.Fatalf("still waiting: %+v", v.Waiting)
	}
}

// Scenario 4, alternating one record at a time. Requirements: D3, D4
func TestAlternatingSourcesOneRecordAtATimeKeepsOrder(t *testing.T) {
	w := room("A", "H1", "H2", "C")
	for i := 1; i <= 6; i++ {
		send(t, w, "A", "C", string(rune('0'+i)))
	}
	pull(t, w, "H1", "A")
	must(w.Pull("H2", "A", PullOpt{Limit: 3}))
	w.Sleep("A")
	for i := 0; i < 10; i++ {
		src := MachineID("H1")
		if i%2 == 1 {
			src = "H2"
		}
		must(w.Pull("C", src, PullOpt{Limit: 1}))
	}
	wantTexts(t, read(t, w, "C"), "shown:1 shown:2 shown:3 shown:4 shown:5 shown:6")
}

// Scenario 5. Requirements: D7, limit "away longer than 30 days"
func TestAwayThirtyOneDaysIsToldWhatIsMissingThenContinues(t *testing.T) {
	w := room("A", "B", "C")
	send(t, w, "A", "C", "old1")
	send(t, w, "A", "C", "old2")
	pull(t, w, "B", "A")
	w.Sleep("C")
	w.Advance(31 * Day)
	send(t, w, "A", "C", "new1")
	pull(t, w, "B", "A")
	w.Sleep("A")
	w.Wake("C")
	pull(t, w, "C", "B")
	wantTexts(t, read(t, w, "C"), "expired: expired: shown:new1")
	w.Wake("A")
	send(t, w, "A", "C", "new2")
	pull(t, w, "C", "A")
	wantTexts(t, read(t, w, "C"), "shown:new2")
}

// Scenario 5, clocks that differ. Requirements: D7
func TestExpiryWithDifferingClocksIsByEachHoldersOwnClockAndNeverResurrects(t *testing.T) {
	w := room("A", "Fast", "Slow", "Late", "Later")
	w.SetSkew("Fast", 2*Day)
	w.SetSkew("Slow", -2*Day)
	m := send(t, w, "A", "Slow", "x")
	y := send(t, w, "A", "Late", "y")
	must(w.Send("A", "a", Msg{Body: "z", To: []Addr{at("Later")}}))
	pull(t, w, "Fast", "A")
	pull(t, w, "Slow", "A")
	w.Sleep("A")
	w.Advance(29 * Day) // Fast reads 31 days, Slow 27, Late and Later 29
	if h := w.Machine("Fast").get(m); h.dropped != Expired {
		t.Fatal("fast clock kept it past its 30 days")
	}
	pull(t, w, "Slow", "Fast")
	wantTexts(t, read(t, w, "Slow"), "shown:x")
	// Late fetches from Fast, which sends only headers. Late's own clock says
	// the text should still exist, so it waits rather than call it expired...
	pull(t, w, "Late", "Fast")
	v := must(w.Inbox("Late", "late"))
	if len(v.Entries) != 0 || len(v.Waiting) == 0 || !v.Waiting[0].Text {
		t.Fatalf("Late shown %s waiting %+v", texts(v.Entries), v.Waiting)
	}
	// ...and gets it from Slow.
	pull(t, w, "Late", "Slow")
	wantTexts(t, read(t, w, "Late"), "shown:y")
	// Once Later's own clock passes 30 days, text offered by a machine with a
	// slower clock is not taken.
	w.Advance(2 * Day)
	pull(t, w, "Later", "Slow")
	if h := w.Machine("Later").get(y); h.dropped != Expired {
		t.Fatalf("Later took expired text: %v", h.dropped)
	}
	wantTexts(t, read(t, w, "Later"), "expired:")
}

// Scenario 6, the hard case. Requirements: D3, D4, limit "restore from backup"
func TestAuthorRestoredWithEveryNewerHolderAsleepWritesANewStreamWithoutFork(t *testing.T) {
	w := room("A", "B", "C")
	w.Sleep("C")
	send(t, w, "A", "C", "m1")
	backup := w.Backup("A")
	w.Advance(Hour)
	send(t, w, "A", "C", "m2")
	send(t, w, "A", "C", "m3")
	pull(t, w, "B", "A")
	w.Sleep("B")
	w.Advance(Hour)
	w.Restore("A", backup) // A has only m1; B (asleep) has m1..m3
	m4 := send(t, w, "A", "C", "m4")
	st := w.Status("A", m4)
	if !st.Saved || len(st.Copies) != 0 {
		t.Fatalf("status = %+v", st)
	}
	w.Wake("C")
	pull(t, w, "C", "A")
	wantTexts(t, read(t, w, "C"), "shown:m1 shown:m4")
	w.Wake("B")
	pull(t, w, "C", "B")
	wantTexts(t, read(t, w, "C"), "shown:m2(late) shown:m3(late)")
	pull(t, w, "B", "A")
	pull(t, w, "A", "B") // A gets its own m2, m3 back
	if w.Forks != 0 {
		t.Fatalf("%d forks", w.Forks)
	}
	for _, id := range []MachineID{"A", "B", "C"} {
		if n := len(w.Machine(id).st.recs); n != 4 {
			t.Fatalf("%s holds %d records, want 4", id, n)
		}
	}
	// A peer awake at restore time removes even the lateness: A fetches its
	// own newer records before it writes.
	w2 := room("A", "B", "C")
	send(t, w2, "A", "C", "m1")
	bk := w2.Backup("A")
	send(t, w2, "A", "C", "m2")
	pull(t, w2, "B", "A")
	w2.Restore("A", bk)
	pull(t, w2, "A", "B")
	send(t, w2, "A", "C", "m3")
	pull(t, w2, "C", "A")
	wantTexts(t, read(t, w2, "C"), "shown:m1 shown:m2 shown:m3")
}

// Scenario 6, the rejected rule. Requirements: D3
func TestContinuingTheOldStreamAfterRestoreLosesTheNewMessageSilently(t *testing.T) {
	w := room("A", "B", "C")
	w.continueStreams = true
	send(t, w, "A", "C", "m1")
	backup := w.Backup("A")
	m2 := send(t, w, "A", "C", "m2")
	pull(t, w, "B", "A")
	w.Restore("A", backup)
	n2 := send(t, w, "A", "C", "m2-new") // same position as m2, different record
	pull(t, w, "B", "A")
	if w.Machine("B").get(n2) != nil || w.Forks != 0 {
		t.Fatal("expected B to take m2-new's position as already filled, silently")
	}
	pull(t, w, "C", "B")
	pull(t, w, "C", "A")
	if got := texts(read(t, w, "C")); got != "shown:m1 shown:m2" {
		t.Fatalf("C shown %q", got)
	}
	// Only the next record, whose predecessor does not match, reveals it.
	send(t, w, "A", "C", "m3-new")
	pull(t, w, "B", "A")
	if w.Forks == 0 {
		t.Fatal("expected a fork once A wrote past the reused position")
	}
	_ = m2
}

// Scenario 7. Requirements: D3, D5, limit "an agent may see a message again"
func TestReaderRestoredMaySeeAgainButHandledIsNotDuplicatedAndNothingSkipped(t *testing.T) {
	w := room("A", "B", "C")
	backup := w.Backup("C")
	m1 := send(t, w, "A", "C", "m1")
	pull(t, w, "C", "A")
	wantTexts(t, read(t, w, "C"), "shown:m1")
	must(w.Handle("C", "c", m1))
	pull(t, w, "B", "C")
	pull(t, w, "A", "C")
	w.Restore("C", backup) // C forgets m1, that it read it, and that it handled it
	m2 := send(t, w, "A", "C", "m2")
	pull(t, w, "C", "B") // m1 and C's own handled report come back together
	pull(t, w, "C", "A")
	wantTexts(t, read(t, w, "C"), "shown:m2")
	must(w.Handle("C", "c", m1)) // a second report is a no-op
	if n := len(w.Machine("C").st.handled[m1]); n != 1 {
		t.Fatalf("%d handled reports", n)
	}
	// Restored again, this time fetching only from A, which lacks nothing:
	// still m1 is not asked again. From a machine without C's report, m1
	// would be shown again (the stated limit), and handling it again writes
	// a second report that the author's status counts once.
	w2 := room("A", "C")
	bk := w2.Backup("C")
	x := send(t, w2, "A", "C", "x")
	pull(t, w2, "C", "A")
	read(t, w2, "C")
	must(w2.Handle("C", "c", x))
	w2.Restore("C", bk) // A never got the report
	pull(t, w2, "C", "A")
	wantTexts(t, read(t, w2, "C"), "shown:x")
	must(w2.Handle("C", "c", x))
	pull(t, w2, "A", "C")
	if st := w2.Status("A", x); len(st.Handled) != 1 {
		t.Fatalf("handled = %+v", st.Handled)
	}
	_ = m2
}

// currentCopies counts dated reports from machines that are still members.
func currentCopies(st Status) int {
	n := 0
	for _, c := range st.Copies {
		if c.Member {
			n++
		}
	}
	return n
}

// Scenario 8. Requirements: D2, D5, limit "a lost disk loses its copies"
func TestDiskLostOnOnlyOtherHolderRecordSurvivesOnAuthorAndRepairContinues(t *testing.T) {
	w := room("A", "B", "C")
	w.Sleep("C")
	m := send(t, w, "A", "C", "keep me")
	pull(t, w, "B", "A")
	pull(t, w, "A", "B")
	if st := w.Status("A", m); currentCopies(st) != 1 {
		t.Fatalf("status = %+v, want one copy", st)
	}
	b2 := w.DiskLost("B")
	w.Wake("C")
	pull(t, w, "C", b2) // nothing there
	pull(t, w, "C", "A")
	wantTexts(t, read(t, w, "C"), "shown:keep me")
	pull(t, w, "A", "C")
	// The old report about B stays dated and true, but B is no longer a member.
	if st := w.Status("A", m); currentCopies(st) != 1 || len(st.Copies) != 2 {
		t.Fatalf("status = %+v, want B (former member) and C", st)
	}
	// Nothing stops: every member keeps pulling from every other.
	pull(t, w, b2, "C")
	pull(t, w, "A", b2)
	if st := w.Status("A", m); currentCopies(st) != 2 {
		t.Fatalf("not two current copies after B' and C both hold it: %+v", st)
	}
}

// Scenario 9. Requirements: D9
func TestWithdrawAndReplaceBeforeAndAfterTheReaderWasShown(t *testing.T) {
	w := room("A", "C")
	w.Sleep("C")
	m := send(t, w, "A", "C", "secret")
	must(w.Withdraw("A", "a", m))
	if h := w.Machine("A").get(m); h.body != "" || h.dropped != Withdrawn {
		t.Fatal("author kept withdrawn text")
	}
	w.Wake("C")
	pull(t, w, "C", "A")
	wantTexts(t, read(t, w, "C"), "")
	if h := w.Machine("C").get(m); h == nil || h.body != "" {
		t.Fatal("reader got withdrawn text")
	}
	m2 := send(t, w, "A", "C", "oops")
	pull(t, w, "C", "A")
	wantTexts(t, read(t, w, "C"), "shown:oops")
	must(w.Withdraw("A", "a", m2))
	pull(t, w, "C", "A")
	wantTexts(t, read(t, w, "C"), "withdrawn:")
	if w.Machine("C").get(m2).body != "" {
		t.Fatal("reader kept withdrawn text")
	}
	m3 := send(t, w, "A", "C", "draft")
	must(w.Replace("A", "a", m3, "final"))
	pull(t, w, "C", "A")
	wantTexts(t, read(t, w, "C"), "shown:final")
	must(w.Replace("A", "a", m3, "final2"))
	pull(t, w, "C", "A")
	wantTexts(t, read(t, w, "C"), "replaced:final2")
}

// Scenario 9, a withdraw that arrives before its message. Requirements: D9
func TestWithdrawArrivingBeforeItsMessageMeansTheMessageIsNeverShown(t *testing.T) {
	w := room("A", "B", "C")
	send(t, w, "A", "C", "x")
	bk := w.Backup("A")
	m := send(t, w, "A", "C", "regret")
	pull(t, w, "B", "A")
	w.Restore("A", bk)
	send(t, w, "A", "C", "y") // new stream, linked to x, not to m
	pull(t, w, "A", "B")      // A gets m back
	must(w.Withdraw("A", "a", m))
	w.Withhold("A", m) // so C gets the withdraw first
	pull(t, w, "C", "A")
	if w.Machine("C").get(m) != nil {
		t.Fatal("setup: C already has m")
	}
	pull(t, w, "C", "B") // now the text arrives from B, which never saw the withdraw
	if h := w.Machine("C").get(m); h == nil || h.dropped != Withdrawn || h.body != "" {
		t.Fatalf("C kept text of a message it knew was withdrawn: %+v", h)
	}
	wantTexts(t, read(t, w, "C"), "shown:x shown:y")
}

// Scenario 10. Requirements: D10
func TestThreadsBranchesAndRepliesWhoseParentIsMissing(t *testing.T) {
	w := room("A", "B", "C")
	sync := func() {
		w.Advance(Hour)
		for _, p := range w.Members() {
			for _, s := range w.Members() {
				if p != s {
					pull(t, w, p, s)
				}
			}
		}
	}
	root := must(w.Send("A", "a", Msg{Body: "plan?"}))
	sync()
	r1 := must(w.Send("B", "b", Msg{Body: "option 1", Parent: root}))
	sync()
	r2 := must(w.Send("C", "c", Msg{Body: "why 1", Parent: r1}))
	other := must(w.Send("C", "c", Msg{Body: "option 2", Parent: root}))
	unrelated := must(w.Send("A", "a", Msg{Body: "lunch"}))
	sync()
	h := w.Machine("A").get(r1).rec
	if len(h.To) != 1 || h.To[0] != at("A") || h.Thread != root {
		t.Fatalf("reply addressed %v thread %s", h.To, h.Thread)
	}
	if h := w.Machine("A").get(r2).rec; h.Thread != root || h.Parent != r1 || h.To[0] != at("B") {
		t.Fatal("reply to a reply")
	}
	wantTexts(t, w.Thread("A", root), "shown:plan? shown:option 1 shown:why 1 shown:option 2")
	wantTexts(t, w.Branch("A", r1), "shown:option 1 shown:why 1")
	_, _ = other, unrelated

	// Parent withdrawn: the reply is still listed, with the parent marked.
	must(w.Withdraw("B", "b", r1))
	sync()
	for _, e := range w.Thread("A", root) {
		if e.Record == r2 && e.ParentIs != ParentWithdrawn {
			t.Fatalf("parent state %v", e.ParentIs)
		}
	}
	// Parent not arrived yet: D fetches only from C, which holds back B's r1.
	if _, err := w.Admit("D", "A", true, "d"); err != nil {
		t.Fatal(err)
	}
	w.Withhold("C", r1)
	r3 := must(w.Send("C", "c", Msg{Body: "to d", Parent: r2, To: []Addr{at("D")}}))
	pull(t, w, "D", "C")
	es := read(t, w, "D")
	if len(es) != 1 || es[0].Record != r3 {
		t.Fatalf("D shown %s", texts(es))
	}
	for _, e := range w.Thread("D", root) {
		if e.Record == r2 && e.ParentIs != ParentMissing {
			t.Fatalf("r2 parent state at D: %v", e.ParentIs)
		}
	}
	// Parent expired.
	w.Advance(25 * Day)
	late := must(w.Send("A", "a", Msg{Body: "late reply", Parent: other, To: []Addr{at("B")}}))
	w.Advance(6 * Day)
	pull(t, w, "B", "A")
	es = read(t, w, "B")
	found := false
	for _, e := range es {
		if e.Record == late {
			found = true
			if e.ParentIs != ParentExpired {
				t.Fatalf("parent state %v", e.ParentIs)
			}
		}
	}
	if !found {
		t.Fatalf("B not shown the late reply: %s", texts(es))
	}
}

// Scenario 11. Requirements: D5
func TestHandledReportsOnlyFromAddresseesAndRoomWidePostsOnlyWhenAsked(t *testing.T) {
	w := room("A", "B", "C")
	m := send(t, w, "A", "C", "do it")
	post := must(w.Send("A", "a", Msg{Body: "fyi"}))
	ask := must(w.Send("A", "a", Msg{Body: "all: confirm", AskHandled: true}))
	pull(t, w, "B", "A")
	pull(t, w, "C", "A")
	read(t, w, "B")
	read(t, w, "C")
	if _, err := w.Handle("C", "c", post); !errors.Is(err, ErrNotAllow) {
		t.Fatalf("handled on a room-wide post: %v", err)
	}
	if _, err := w.Handle("B", "b", m); !errors.Is(err, ErrNotAllow) {
		t.Fatalf("handled by a non-addressee: %v", err)
	}
	must(w.Send("B", "b", Msg{Body: "I can help", Parent: m}))
	must(w.Send("C", "c", Msg{Body: "on it", Parent: m}))
	must(w.Handle("B", "b", ask))
	// A dishonest machine writes a report the API would refuse.
	b := w.Machine("B")
	w.write(b, &Record{Kind: Handled, Agent: "c", Ref: m}, "")
	pull(t, w, "A", "B")
	pull(t, w, "A", "C")
	if st := w.Status("A", m); len(st.Handled) != 0 {
		t.Fatalf("replies or a forged label marked it handled: %+v", st.Handled)
	}
	must(w.Handle("C", "c", m))
	pull(t, w, "A", "C")
	if st := w.Status("A", m); len(st.Handled) != 1 || st.Handled[0].Addr != at("C") {
		t.Fatalf("handled = %+v", st.Handled)
	}
	if st := w.Status("A", ask); len(st.Handled) != 1 || st.Handled[0].Addr != at("B") {
		t.Fatalf("asked post handled = %+v", st.Handled)
	}
}

// Scenario 12. Requirements: U3, T3, limit "kept by the members"
func TestNewMachineInboxStartsEmptyAndHistoryFlagFiltersWhatMembersHandOver(t *testing.T) {
	w := room("A", "B")
	old := must(w.Send("A", "a", Msg{Body: "before"}))
	pull(t, w, "B", "A")
	must(w.Admit("N", "B", false, "n"))
	must(w.Admit("H", "B", true, "h"))
	for _, x := range []MachineID{"N", "H"} {
		pull(t, w, x, "A")
		pull(t, w, x, "B")
		wantTexts(t, read(t, w, x), "")
	}
	if h := w.Machine("N").get(old); h == nil || h.body != "" || h.dropped != NotAdmitted {
		t.Fatal("machine admitted without history was handed earlier text, or not the header")
	}
	if w.Machine("H").get(old) == nil {
		t.Fatal("machine admitted with history did not get it")
	}
	if es := w.Thread("H", old); len(es) != 1 {
		t.Fatal("history not readable apart from the inbox")
	}
	reply := must(w.Send("A", "a", Msg{Body: "after", Parent: old, Everyone: true}))
	for _, x := range []MachineID{"N", "H"} {
		pull(t, w, x, "A")
		es := read(t, w, x)
		wantTexts(t, es, "shown:after")
		want := ParentHere
		if x == "N" {
			want = ParentMissing
		}
		if es[0].Record != reply || es[0].ParentIs != want {
			t.Fatalf("%s: parent state %v want %v", x, es[0].ParentIs, want)
		}
	}
}

// Requirements: D8
func TestRelayThatAltersARecordIsRefused(t *testing.T) {
	w := room("A", "B", "C")
	send(t, w, "A", "C", "pay 10")
	pull(t, w, "B", "A")
	w.Sleep("A")
	w.tamper = func(srv MachineID, it wire) wire {
		if srv != "B" {
			return it
		}
		if it.hasBody {
			it.body = "pay 1000"
		}
		return it
	}
	if r := pull(t, w, "C", "B"); r.Stored != 0 {
		t.Fatal("altered body accepted")
	}
	w.tamper = func(srv MachineID, it wire) wire {
		r := *it.rec
		r.Author = "B"
		it.rec = &r
		return it
	}
	if r := pull(t, w, "C", "B"); r.Stored != 0 {
		t.Fatal("re-attributed record accepted")
	}
	// A relay that strips the text: C holds the header with its text missing,
	// waits, and takes the text from the next holder that has it.
	w.tamper = func(srv MachineID, it wire) wire {
		it.body, it.hasBody = "", false
		return it
	}
	pull(t, w, "C", "B")
	if v := must(w.Inbox("C", "c")); len(v.Entries) != 0 || len(v.Waiting) != 1 || !v.Waiting[0].Text {
		t.Fatalf("stripped text: shown %s, waiting %+v", texts(v.Entries), v.Waiting)
	}
	w.tamper = nil
	pull(t, w, "C", "B")
	wantTexts(t, read(t, w, "C"), "shown:pay 10")
}
