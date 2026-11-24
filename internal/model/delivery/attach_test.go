package delivery

import (
	"errors"
	"fmt"
	"math/rand"
	"testing"
)

// Attachment rules F1 to F8 (attach.go; FINDINGS.md "Attachments"). Every
// test names its requirements: L1, with U4, D7, D9, T3, T5 and P3.

// sendFile puts data at path on from's disk, asks to send it with text
// (room-wide when to is empty) and has the person approve. It returns the
// message and the file's hash.
func sendFile(t *testing.T, w *World, from, to MachineID, text, path, data string) (RecordID, string) {
	t.Helper()
	w.PutLocalFile(from, path, data)
	msg := Msg{Body: text}
	if to != "" {
		msg.To = []Addr{at(to)}
	}
	n := must(w.SendFiles(from, ag(from), msg, path))
	return must(w.Decide(from, n, true)), hashBody(data)
}

// shownTo has m's agent read its inbox, so it may fetch what it was shown.
func shownTo(t *testing.T, w *World, m MachineID) []Entry {
	t.Helper()
	return read(t, w, m)
}

func fetch(w *World, m MachineID, msg RecordID, hash string, from MachineID) (FetchResult, error) {
	return w.Fetch(m, ag(m), msg, hash, from, FetchOpt{})
}

func wantState(t *testing.T, w *World, m MachineID, msg RecordID, hash string, want FileState) FileInfo {
	t.Helper()
	fi := w.FileStatus(m, msg, hash)
	if fi.State != want {
		t.Fatalf("%s: file is %s (why %s, holders %+v), want %s", m, fi.State, fi.Why, fi.Holders, want)
	}
	return fi
}

// F6. Requirements: L1, P3
func TestAFileIsSentOnlyAfterAPersonApprovesTheBytesStagedWhenTheAgentAsked(t *testing.T) {
	w := room("A", "B")
	w.PutLocalFile("A", "report.txt", "draft one")
	n := must(w.SendFiles("A", "a", Msg{Body: "see attached", To: []Addr{at("B")}}, "report.txt"))
	if !w.Waiting(n) {
		t.Fatal("the request does not wait for a person")
	}
	if r := pull(t, w, "B", "A"); r.Stored != 0 {
		t.Fatalf("something was written before the person decided: %+v", r)
	}
	// The agent changes the file while the person thinks: what was staged
	// and shown to the person is what is sent.
	w.PutLocalFile("A", "report.txt", "draft two, with secrets")
	m := must(w.Decide("A", n, true))
	if _, err := w.Decide("A", n, true); !errors.Is(err, ErrNoProposal) {
		t.Fatalf("a second decision: %v", err)
	}
	pull(t, w, "B", "A")
	es := shownTo(t, w, "B")
	if len(es) != 1 || len(es[0].Files) != 1 || es[0].Files[0].Hash != hashBody("draft one") || es[0].Files[0].Size != 9 {
		t.Fatalf("B was shown %+v", es)
	}
	must(fetch(w, "B", m, hashBody("draft one"), "A"))
	if b, _ := w.FileBytes("B", hashBody("draft one")); b != "draft one" {
		t.Fatalf("B got %q", b)
	}

	// Declined: nothing is ever written, and the request cannot be revived.
	n2 := must(w.SendFiles("A", "a", Msg{Body: "and this"}, "report.txt"))
	if _, err := w.Decide("A", n2, false); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Decide("A", n2, true); !errors.Is(err, ErrNoProposal) {
		t.Fatalf("approving a declined request: %v", err)
	}
	if r := pull(t, w, "B", "A"); r.Stored != 0 {
		t.Fatalf("a declined message was written: %+v", r)
	}
	if w.HoldsAnyOf("A", hashBody("draft two, with secrets")) {
		t.Fatal("the declined file's staged copy is kept")
	}
}

// F3, F4, F5. Requirements: L1, U4
func TestAReaderFetchesOnlyWhenItsAgentAsksAndGetsVerifiedBytes(t *testing.T) {
	w := room("A", "B")
	m, h := sendFile(t, w, "A", "B", "logs", "build.log", "line 1\nline 2\n")
	pull(t, w, "B", "A")
	if w.HoldsAnyOf("B", h) {
		t.Fatal("bytes arrived with the record: fetching is not lazy")
	}
	wantState(t, w, "B", m, h, FileNotAsked)
	if _, err := fetch(w, "B", m, h, "A"); !errors.Is(err, ErrNotAllow) {
		t.Fatalf("fetch before the agent was shown the message: %v", err)
	}
	es := shownTo(t, w, "B")
	if len(es) != 1 || len(es[0].Files) != 1 || es[0].Files[0].Name != "build.log" {
		t.Fatalf("entry %+v", es)
	}
	r := must(fetch(w, "B", m, h, "A"))
	if !r.Complete || r.Got != 14 {
		t.Fatalf("fetch %+v", r)
	}
	fi := wantState(t, w, "B", m, h, FileHere)
	if len(fi.Holders) != 1 || fi.Holders[0].Machine != "A" || !fi.Holders[0].Has {
		t.Fatalf("holders %+v", fi.Holders)
	}
	if b, _ := w.FileBytes("B", h); b != "line 1\nline 2\n" {
		t.Fatalf("bytes %q", b)
	}
}

// F3. Requirements: L1
func TestAMemberServingWrongBytesIsRefusedAndAnHonestHolderStillServes(t *testing.T) {
	w := room("A", "B", "C")
	m, h := sendFile(t, w, "A", "", "all of you", "a.bin", "0123456789")
	for _, x := range []MachineID{"B", "C"} {
		pull(t, w, x, "A")
		shownTo(t, w, x)
	}
	must(fetch(w, "B", m, h, "A"))
	w.TamperFiles(func(s MachineID, _, data string) string {
		if s == "B" {
			return "0123456780" // same size, one byte changed
		}
		return data
	})
	if _, err := fetch(w, "C", m, h, "B"); !errors.Is(err, ErrBadFile) {
		t.Fatalf("wrong bytes: %v", err)
	}
	if w.HoldsAnyOf("C", h) {
		t.Fatal("wrong bytes were kept")
	}
	w.TamperFiles(func(s MachineID, _, data string) string {
		if s == "B" {
			return data + "and more, never ending"
		}
		return data
	})
	if _, err := fetch(w, "C", m, h, "B"); !errors.Is(err, ErrBadFile) || w.HoldsAnyOf("C", h) {
		t.Fatalf("more bytes than declared: %v", err)
	}
	if r := must(fetch(w, "C", m, h, "A")); !r.Complete {
		t.Fatalf("honest holder: %+v", r)
	}
}

// F3. Requirements: L1
func TestAResumedFetchContinuesFromAnotherHolderAndChecksTheWhole(t *testing.T) {
	w := room("A", "B", "C")
	m, h := sendFile(t, w, "A", "", "big one", "big.bin", "abcdefghijklmnop")
	for _, x := range []MachineID{"B", "C"} {
		pull(t, w, x, "A")
		shownTo(t, w, x)
	}
	must(fetch(w, "B", m, h, "A"))
	if r := must(w.Fetch("C", "c", m, h, "A", FetchOpt{Max: 5})); r.Complete || r.Got != 5 {
		t.Fatalf("first part %+v", r)
	}
	wantState(t, w, "C", m, h, FilePartial)
	if _, ok := w.FileBytes("C", h); ok {
		t.Fatal("a partial download is exposed")
	}
	w.Sleep("A")
	if r := must(fetch(w, "C", m, h, "B")); !r.Complete || r.Got != 11 {
		t.Fatalf("rest from B %+v", r)
	}
	if b, _ := w.FileBytes("C", h); b != "abcdefghijklmnop" {
		t.Fatalf("bytes %q", b)
	}
}

// F4, F5: the limit of lazy fetching, and its remedy. Requirements: L1, U4
func TestAFileWhoseOnlyHolderIsGoneIsUnavailableAndTheReaderIsToldSo(t *testing.T) {
	w := room("A", "B", "C")
	m, h := sendFile(t, w, "A", "", "the plan", "plan.md", "# plan")
	pull(t, w, "B", "A")
	pull(t, w, "C", "A")
	shownTo(t, w, "B")
	nid := w.DiskLost("A")
	pull(t, w, nid, "B") // the new machine holds the record again, not the bytes
	if _, err := fetch(w, "B", m, h, nid); !errors.Is(err, ErrNotHeldFile) {
		t.Fatalf("fetch from the re-added machine: %v", err)
	}
	if _, err := fetch(w, "B", m, h, "C"); !errors.Is(err, ErrNotHeldFile) {
		t.Fatalf("fetch from a member that never fetched: %v", err)
	}
	fi := wantState(t, w, "B", m, h, FileUnavailable)
	if len(fi.Holders) != 2 || fi.Holders[0].Has || fi.Holders[1].Has {
		t.Fatalf("holders %+v", fi.Holders)
	}
	if es := w.Thread("B", m); len(es) != 1 || es[0].Text != "the plan" {
		t.Fatalf("the text must stay readable without the file: %+v", es)
	}
}

// F4: a machine the person set to keep files serves them after the author
// is gone. Requirements: L1, D2
func TestAMachineSetToKeepFilesServesThemAfterTheAuthorIsGone(t *testing.T) {
	w := room("A", "B", "K")
	w.SetKeep("K", true)
	m, h := sendFile(t, w, "A", "B", "for B only", "notes.txt", "hello B")
	pull(t, w, "K", "A")
	if n := must(w.Replicate("K", "A")); n != 1 {
		t.Fatalf("replicated %d", n)
	}
	if n := must(w.Replicate("B", "A")); n != 0 {
		t.Fatalf("a machine not set to keep fetched %d", n)
	}
	w.DiskLost("A")
	pull(t, w, "B", "K")
	shownTo(t, w, "B")
	if r := must(fetch(w, "B", m, h, "K")); !r.Complete {
		t.Fatalf("from the keeper %+v", r)
	}
}

// F1: withdrawal. Requirements: L1, D9
func TestWithdrawDeletesAFileAndAnyPartOfItOnEveryMachineThatLearnsOfIt(t *testing.T) {
	w := room("A", "B", "C")
	m, h := sendFile(t, w, "A", "", "oops", "secret.env", "TOKEN=abc123")
	for _, x := range []MachineID{"B", "C"} {
		pull(t, w, x, "A")
		shownTo(t, w, x)
	}
	must(fetch(w, "B", m, h, "A"))
	must(w.Fetch("C", "c", m, h, "A", FetchOpt{Max: 4}))
	must(w.Withdraw("A", "a", m))
	if w.HoldsAnyOf("A", h) {
		t.Fatal("the author keeps the bytes of its withdrawn message")
	}
	if !w.HoldsAnyOf("B", h) {
		t.Fatal("B dropped the bytes before it learned of the withdraw")
	}
	pull(t, w, "B", "A")
	pull(t, w, "C", "B")
	for _, x := range []MachineID{"B", "C"} {
		if w.HoldsAnyOf(x, h) {
			t.Fatalf("%s keeps bytes of a withdrawn message", x)
		}
		if fi := wantState(t, w, x, m, h, FileGone); fi.Why != Withdrawn {
			t.Fatalf("%s: gone because %s", x, fi.Why)
		}
	}
	if _, err := fetch(w, "C", m, h, "B"); !errors.Is(err, ErrFileGone) {
		t.Fatalf("fetch after the withdraw: %v", err)
	}
}

// F2: every request is checked, so a withdraw between two parts stops
// the rest. Requirements: L1, D9
func TestAWithdrawBetweenTwoPartsOfAFetchStopsTheRest(t *testing.T) {
	w := room("A", "B", "C")
	m, h := sendFile(t, w, "A", "", "large", "x.bin", "0123456789abcdef")
	for _, x := range []MachineID{"B", "C"} {
		pull(t, w, x, "A")
		shownTo(t, w, x)
	}
	must(fetch(w, "B", m, h, "A"))
	must(w.Fetch("C", "c", m, h, "B", FetchOpt{Max: 6}))
	must(w.Withdraw("A", "a", m))
	pull(t, w, "B", "A") // B learns; C does not yet
	if _, err := fetch(w, "C", m, h, "B"); !errors.Is(err, ErrNotHeldFile) {
		t.Fatalf("the rest after B learned of the withdraw: %v", err)
	}
	if _, ok := w.FileBytes("C", h); ok {
		t.Fatal("C completed a withdrawn file")
	}
	pull(t, w, "C", "B")
	if w.HoldsAnyOf("C", h) {
		t.Fatal("C keeps the part after learning of the withdraw")
	}
}

// F8. Requirements: L1, D9
func TestAReplacementKeepsAFileItStillListsAndDropsOneItDoesNot(t *testing.T) {
	w := room("A", "B")
	m, h := sendFile(t, w, "A", "B", "v0", "a.txt", "aaa")
	pull(t, w, "B", "A")
	shownTo(t, w, "B")
	must(fetch(w, "B", m, h, "A"))
	must(w.Replace("A", "a", m, "v1, same file"))
	pull(t, w, "B", "A")
	es := shownTo(t, w, "B")
	if len(es) != 1 || es[0].Kind != ReplacedNotice || len(es[0].Files) != 1 || es[0].Files[0].Hash != h {
		t.Fatalf("replaced notice %+v", es)
	}
	wantState(t, w, "B", m, h, FileHere)

	// A replacement with a new file needs the person again; the old file goes.
	w.PutLocalFile("A", "b.txt", "bbb")
	n := must(w.ReplaceWithFiles("A", "a", m, "v2, new file", "b.txt"))
	pull(t, w, "B", "A")
	wantState(t, w, "B", m, h, FileHere) // nothing written yet
	must(w.Decide("A", n, true))
	pull(t, w, "B", "A")
	if fi := wantState(t, w, "B", m, h, FileGone); fi.Why != Live {
		t.Fatalf("why %s, want the replacement dropped it", fi.Why)
	}
	if w.HoldsAnyOf("B", h) || w.HoldsAnyOf("A", h) {
		t.Fatal("bytes of a file no version lists are kept")
	}
	shownTo(t, w, "B")
	if r := must(fetch(w, "B", m, hashBody("bbb"), "A")); !r.Complete {
		t.Fatalf("new file %+v", r)
	}
	must(w.ReplaceWithoutFiles("A", "a", m, "v3, no files"))
	pull(t, w, "B", "A")
	if w.HoldsAnyOf("B", hashBody("bbb")) || w.HoldsAnyOf("A", hashBody("bbb")) {
		t.Fatal("a replacement without files left the bytes")
	}
}

// F1: expiry by each holder's clock, never undone. Requirements: L1, D7
func TestAFileExpiresWithItsTextByEachHoldersClock(t *testing.T) {
	w := room("A", "B")
	m, h := sendFile(t, w, "A", "B", "monthly", "m.csv", "1,2,3")
	pull(t, w, "B", "A")
	shownTo(t, w, "B")
	must(fetch(w, "B", m, h, "A"))
	w.SetSkew("B", 2*Day)
	w.Advance(28*Day + Hour)
	if w.HoldsAnyOf("B", h) {
		t.Fatal("B's clock is past 30 days and the bytes are still there")
	}
	if fi := wantState(t, w, "B", m, h, FileGone); fi.Why != Expired {
		t.Fatalf("why %s", fi.Why)
	}
	if !w.HoldsAnyOf("A", h) {
		t.Fatal("A's clock is not past 30 days")
	}
	w.SetSkew("B", 0) // the clock goes back: expired stays expired
	if _, err := fetch(w, "B", m, h, "A"); !errors.Is(err, ErrFileGone) {
		t.Fatalf("fetch after expiry with the clock set back: %v", err)
	}
	w.Advance(2 * Day)
	if w.HoldsAnyOf("A", h) {
		t.Fatal("the author keeps bytes past 30 days")
	}
}

// F2, F1: permission. Requirements: L1, T3
func TestANewcomerWithoutHistoryIsNotServedAnEarlierFile(t *testing.T) {
	w := room("A", "B")
	m, h := sendFile(t, w, "A", "", "before D", "old.txt", "old")
	pull(t, w, "B", "A")
	must(w.Admit("D", "B", false, "d"))
	pull(t, w, "D", "B")
	if fi := wantState(t, w, "D", m, h, FileGone); fi.Why != NotAdmitted {
		t.Fatalf("why %s", fi.Why)
	}
	// A requester that skips its own check is still refused by the server.
	if _, ok := w.Machine("A").mayServe("D", m, h, w.Now); ok {
		t.Fatal("the author would serve a pre-admission file to a no-history newcomer")
	}
	// A file posted after the admission is D's to fetch.
	m2, h2 := sendFile(t, w, "A", "", "after D", "new.txt", "new")
	pull(t, w, "D", "A")
	shownTo(t, w, "D")
	if r := must(fetch(w, "D", m2, h2, "A")); !r.Complete {
		t.Fatalf("%+v", r)
	}
}

// F2: a server serves bytes only for the message it holds them for, so a
// member that copies a hash it may not read into a message of its own
// gets nothing. Requirements: L1, T3
func TestANewReferenceToAKnownHashDoesNotWidenWhoIsServed(t *testing.T) {
	w := room("A", "B")
	m1, h := sendFile(t, w, "A", "", "before D", "payroll.csv", "alice,100")
	pull(t, w, "B", "A")
	shownTo(t, w, "B")
	must(fetch(w, "B", m1, h, "A"))
	must(w.Admit("D", "B", false, "d"))
	pull(t, w, "D", "B")
	// D holds m1's header, so it knows the hash; it writes a message of
	// its own listing it, without having the bytes.
	ref := w.Machine("D").get(m1).rec.Files[0]
	m2 := must(w.send("D", "d", Msg{Body: "mine"}, []FileRef{ref}))
	for _, s := range []MachineID{"A", "B"} {
		pull(t, w, s, "D")
	}
	if _, err := w.fetch(w.Machine("D"), m2, h, "B", FetchOpt{}); !errors.Is(err, ErrNotHeldFile) {
		t.Fatalf("B served a known hash for D's new message: %v", err)
	}
	// B's agent may use its copy for m2 too, but that does not make B
	// serve it for m2.
	shownTo(t, w, "B")
	if r := must(fetch(w, "B", m2, h, "D")); !r.Local {
		t.Fatalf("B should use the bytes it holds: %+v", r)
	}
	if _, ok := w.Machine("B").mayServe("D", m2, h, w.Now); ok {
		t.Fatal("B would serve D's copied reference")
	}
}

// F2. Requirements: L1, T5
func TestARemovedMachineIsNotServedAFile(t *testing.T) {
	w := room("A", "B", "C")
	m, h := sendFile(t, w, "A", "", "all", "f.txt", "data")
	pull(t, w, "C", "A")
	shownTo(t, w, "C")
	if err := w.Remove("C", "A"); err != nil {
		t.Fatal(err)
	}
	if _, err := fetch(w, "C", m, h, "A"); !errors.Is(err, ErrNotHeldFile) {
		t.Fatalf("a removed machine was served: %v", err)
	}
	// B, not yet told, still serves C: removal is not instant, as for text.
	pull(t, w, "B", "A")
	shownTo(t, w, "B")
	must(fetch(w, "B", m, h, "A"))
	if r := must(fetch(w, "C", m, h, "B")); !r.Complete {
		t.Fatalf("stale member: %+v", r)
	}
	must(w.Learn("B"))
	if _, ok := w.Machine("B").mayServe("C", m, h, w.Now); ok {
		t.Fatal("B serves C after learning of the removal")
	}
}

// F7. Requirements: L1
func TestAFetchOverTheRoomsBudgetIsRefusedBeforeAnythingMoves(t *testing.T) {
	w := room("A", "B")
	w.SetFileBudget("B", 10)
	m1, h1 := sendFile(t, w, "A", "B", "one", "1.bin", "123456")
	m2, h2 := sendFile(t, w, "A", "B", "two", "2.bin", "abcdef")
	pull(t, w, "B", "A")
	shownTo(t, w, "B")
	must(fetch(w, "B", m1, h1, "A"))
	if r, err := fetch(w, "B", m2, h2, "A"); !errors.Is(err, ErrNoSpace) || r.Got != 0 {
		t.Fatalf("over budget: %+v %v", r, err)
	}
	must(w.Withdraw("A", "a", m1))
	pull(t, w, "B", "A")
	if r := must(fetch(w, "B", m2, h2, "A")); !r.Complete {
		t.Fatalf("after the first went: %+v", r)
	}
}

// F1: a voided author's records go, and their files with them.
// Requirements: L1, T5
func TestAVoidedAuthorsFilesAreDeleted(t *testing.T) {
	w := room("A", "B", "C")
	m, h := sendFile(t, w, "C", "", "from C", "c.txt", "ccc")
	pull(t, w, "B", "C")
	shownTo(t, w, "B")
	must(fetch(w, "B", m, h, "C"))
	if err := w.VoidAdmission("C", "B"); err != nil {
		t.Fatal(err)
	}
	if w.HoldsAnyOf("B", h) {
		t.Fatal("B keeps the file of a voided author")
	}
}

// F1: a narrower grant learned later drops the files it hides, as it
// drops their text (gap G7: silently). Requirements: L1, T3
func TestANarrowerGrantLearnedLaterDropsTheFilesItHides(t *testing.T) {
	w := room("A", "B")
	m, h := sendFile(t, w, "A", "", "early", "e.txt", "eee")
	pull(t, w, "B", "A") // B's grant below will hide what B holds
	must(w.Admit("D", "A", true, "d"))
	pull(t, w, "D", "A")
	// D's inbox starts after its admission (U3); a thread view shows it the
	// earlier message, which lets its agent fetch the file.
	if err := w.Showed("D", "d", w.Thread("D", m)); err != nil {
		t.Fatal(err)
	}
	must(fetch(w, "D", m, h, "A"))
	must(w.Grant("D", "B", false))
	if w.HoldsAnyOf("D", h) {
		t.Fatal("D keeps a file its narrower permission hides")
	}
}

// F1 to F3 on random schedules: after every step, every machine's held
// bytes match their hash and are kept for a message whose winning
// version it may keep and, by the true grants, read; no machine that
// holds the withdraw holds the bytes; no machine the truth says was
// removed gets bytes from a server that knows it. Requirements: L1, D7,
// D9, T3, T5
func TestRandomSchedulesWithFilesKeepTheAttachmentRules(t *testing.T) {
	runs, steps := 400, 100
	if testing.Short() {
		runs = 100
	}
	var total fileStats
	for seed := 0; seed < runs; seed++ {
		st, err := fileSchedule(int64(seed), steps)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		total.complete += st.complete
		total.refused += st.refused
		total.dropped += st.dropped
	}
	// Not vacuous: files were fetched, refused and dropped.
	if total.complete < runs/2 || total.refused < runs || total.dropped < runs/4 {
		t.Fatalf("schedules exercised too little: %+v", total)
	}
	t.Logf("%d schedules: %+v", runs, total)
}

type fileStats struct{ complete, refused, dropped int }

func fileSchedule(seed int64, steps int) (fileStats, error) {
	var st fileStats
	rng := rand.New(rand.NewSource(seed))
	w := room("A", "B", "C")
	type sent struct {
		msg  RecordID
		hash string
		from MachineID
	}
	var msgs []sent
	ids := func() []MachineID {
		var out []MachineID
		for _, id := range w.order {
			if !w.machines[id].Gone {
				out = append(out, id)
			}
		}
		return out
	}
	pick := func() MachineID { xs := ids(); return xs[rng.Intn(len(xs))] }
	file := 0
	for step := 0; step < steps; step++ {
		a, b := pick(), pick()
		before := heldCount(w)
		var op string
		switch rng.Intn(14) {
		case 0, 1:
			op = "send"
			file++
			data := fmt.Sprintf("file-%d-%s", file, a)
			var to MachineID
			if rng.Intn(2) == 0 && a != b {
				to = b
			}
			w.PutLocalFile(a, "f", data)
			msg := Msg{Body: "m"}
			if to != "" {
				msg.To = []Addr{at(to)}
			}
			if n, err := w.SendFiles(a, ag(a), msg, "f"); err == nil {
				if id, err := w.Decide(a, n, rng.Intn(5) > 0); err == nil && id != "" {
					msgs = append(msgs, sent{id, hashBody(data), a})
				}
			}
		case 2, 3, 4:
			op = "pull"
			w.Pull(a, b, PullOpt{})
		case 5, 6, 7:
			op = "fetch"
			if len(msgs) > 0 {
				s := msgs[rng.Intn(len(msgs))]
				from := b
				if rng.Intn(2) == 0 {
					from = s.from
				}
				w.Pull(a, from, PullOpt{})
				// The agent reads what it was sent, and looks at the thread.
				if es, err := w.List(a, ag(a)); err == nil {
					_ = w.Seen(a, ag(a), es.Entries)
					_ = w.Showed(a, ag(a), w.Thread(a, s.msg))
					opt := FetchOpt{}
					if rng.Intn(3) == 0 {
						opt.Max = 4
					}
					if r, err := w.Fetch(a, ag(a), s.msg, s.hash, from, opt); err == nil && r.Complete && !r.Local {
						st.complete++
					} else if err != nil {
						st.refused++
					}
				}
			}
		case 8:
			op = "withdraw"
			if len(msgs) > 0 {
				s := msgs[rng.Intn(len(msgs))]
				w.Withdraw(s.from, ag(s.from), s.msg)
			}
		case 9:
			op = "replace"
			if len(msgs) > 0 {
				s := msgs[rng.Intn(len(msgs))]
				if rng.Intn(2) == 0 {
					w.Replace(s.from, ag(s.from), s.msg, "r")
				} else {
					w.ReplaceWithoutFiles(s.from, ag(s.from), s.msg, "r")
				}
			}
		case 10:
			op = "advance"
			w.Advance(Time(rng.Intn(5)) * Day)
		case 11:
			op = "toggle"
			if m := w.machines[a]; m.Awake {
				w.Sleep(a)
			} else {
				w.Wake(a)
			}
		case 12:
			op = "membership"
			switch rng.Intn(4) {
			case 0:
				if w.machines["D"] == nil {
					w.Admit("D", a, rng.Intn(2) == 0, "d")
				}
			case 1:
				w.Remove(a, b)
			case 2:
				w.Learn(a)
			case 3:
				if rng.Intn(4) == 0 {
					w.DiskLost(a)
				}
			}
		case 13:
			op = "tamper"
			if rng.Intn(2) == 0 {
				bad := pick()
				w.TamperFiles(func(s MachineID, _, data string) string {
					if s == bad {
						return data[:len(data)-1] + "!"
					}
					return data
				})
			} else {
				w.TamperFiles(nil)
			}
		}
		if heldCount(w) < before {
			st.dropped++
		}
		if err := checkFiles(w); err != nil {
			return st, fmt.Errorf("step %d (%s %s %s): %v", step, op, a, b, err)
		}
	}
	return st, nil
}

func heldCount(w *World) int {
	n := 0
	for _, m := range w.machines {
		n += len(m.st.files.held)
	}
	return n
}

func checkFiles(w *World) error {
	for _, id := range w.order {
		m := w.machines[id]
		if m.Gone || !m.Awake {
			continue // a sleeping machine checks its clock when it wakes, as for text
		}
		f := m.st.files
		for hash, x := range f.held {
			if hashBody(x.data) != hash {
				return fmt.Errorf("%s holds bytes that do not match %s", id, hash[:8])
			}
			if len(x.serve)+len(x.local) == 0 {
				return fmt.Errorf("%s holds %s for no message", id, hash[:8])
			}
			for _, set := range []map[RecordID]bool{x.serve, x.local} {
				for msg := range set {
					v, ok := m.referenced(msg, hash, w.Now)
					if !ok {
						return fmt.Errorf("%s holds %s for %s, which no longer lets it keep the file", id, hash[:8], msg)
					}
					if !mayRead(w.admission[id], v.rec) {
						return fmt.Errorf("%s holds %s, which its true permission hides", id, hash[:8])
					}
					if m.withdrawnRef(msg, v.rec.Author) {
						return fmt.Errorf("%s holds %s of a message it holds the withdraw of", id, hash[:8])
					}
				}
			}
		}
		for hash, p := range f.partial {
			if !m.referencedAny(hash, w.Now) {
				return fmt.Errorf("%s keeps part of %s that nothing refers to", id, hash[:8])
			}
			if p == "" {
				return fmt.Errorf("%s keeps an empty part", id)
			}
		}
	}
	return nil
}
