package delivery

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// Attachment rules F1 to F11 (attach.go; FINDINGS.md "Attachments"). Every
// test names its requirements: L1, with N1, U4, D7, D9, T3, T5 and P3.

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

// shownTo has m's agent read its inbox and confirm it.
func shownTo(t *testing.T, w *World, m MachineID) []Entry {
	t.Helper()
	return read(t, w, m)
}

// showThread has m's agent look at msg's thread, which shows it the
// current version without touching its inbox.
func showThread(t *testing.T, w *World, m MachineID, msg RecordID) {
	t.Helper()
	if err := w.Showed(m, ag(m), w.Thread(m, msg)); err != nil {
		t.Fatal(err)
	}
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

func manifestHash(w *World, m MachineID, msg RecordID) string {
	return w.Machine(m).get(msg).rec.Manifest
}

// outboundOnly makes machines take no connections.
func outboundOnly(w *World, ids ...MachineID) {
	for _, id := range ids {
		w.Machine(id).NoInbound = true
	}
}

// F6, F10. Requirements: L1, P3
func TestAFileIsSentOnlyAfterAPersonApprovesTheBytesStagedWhenTheAgentAsked(t *testing.T) {
	w := room("A", "B")
	w.PutLocalFile("A", "report.txt", "draft one")
	n := must(w.SendFiles("A", "a", Msg{Body: "see attached", To: []Addr{at("B")}}, "report.txt"))
	if !w.Waiting(n) || !w.Staged("A") {
		t.Fatal("the request does not wait for a person with its bytes staged")
	}
	if r := pull(t, w, "B", "A"); r.Stored != 0 {
		t.Fatalf("something was written before the person decided: %+v", r)
	}
	// The agent changes the file while the person thinks: what was staged
	// and shown to the person is what is sent; the source is not read again.
	w.PutLocalFile("A", "report.txt", "draft two, with secrets")
	m := must(w.Decide("A", n, true))
	if w.Staged("A") {
		t.Fatal("staging outlives the approval")
	}
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

	// Declined: nothing is ever written, and staging is gone from disk.
	n2 := must(w.SendFiles("A", "a", Msg{Body: "and this"}, "report.txt"))
	if !w.Staged("A") {
		t.Fatal("not staged")
	}
	if _, err := w.Decide("A", n2, false); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Decide("A", n2, true); !errors.Is(err, ErrNoProposal) {
		t.Fatalf("approving a declined request: %v", err)
	}
	if r := pull(t, w, "B", "A"); r.Stored != 0 {
		t.Fatalf("a declined message was written: %+v", r)
	}
	if w.Staged("A") || w.HoldsAnyOf("A", hashBody("draft two, with secrets")) {
		t.Fatal("the declined file's staged copy is kept")
	}
}

// F6, F11. Requirements: L1, P3
func TestAStagedSendExpiresAfterADayAndASelfVoidDropsIt(t *testing.T) {
	w := room("A", "B", "C")
	w.PutLocalFile("C", "x", "xxx")
	n := must(w.SendFiles("C", "c", Msg{Body: "x"}, "x"))
	w.Advance(Day)
	if w.Staged("C") || w.Waiting(n) {
		t.Fatal("a request a day old still waits")
	}
	if _, err := w.Decide("C", n, true); !errors.Is(err, ErrNoProposal) {
		t.Fatalf("approving an expired request: %v", err)
	}
	n2 := must(w.SendFiles("C", "c", Msg{Body: "x"}, "x"))
	if err := w.VoidAdmission("C", "A"); err != nil {
		t.Fatal(err)
	}
	for w.machines["C"].st.view.out["C"] != Void {
		must(w.Learn("C"))
	}
	if w.Staged("C") {
		t.Fatal("a voided machine keeps its staged send")
	}
	if _, err := w.Decide("C", n2, true); !errors.Is(err, ErrNoProposal) {
		t.Fatalf("approving after a self-void: %v", err)
	}
}

// F1, F4, F5. Requirements: L1, U4
func TestAReaderGetsTheListAtOnceAndTheBytesOnlyWhenItsAgentAsks(t *testing.T) {
	w := room("A", "B")
	m, h := sendFile(t, w, "A", "B", "logs", "build.log", "line 1\nline 2\n")
	pull(t, w, "B", "A")
	if _, known := w.ManifestOf("B", m); !known {
		t.Fatal("the manifest, the cheap first step, did not come with the pull")
	}
	if w.HoldsAnyOf("B", h) {
		t.Fatal("file bytes came without being asked for")
	}
	wantState(t, w, "B", m, h, FileNotAsked)
	if _, err := fetch(w, "B", m, h, "A"); !errors.Is(err, ErrNotAllow) {
		t.Fatalf("fetch before the agent was shown the message: %v", err)
	}
	es := shownTo(t, w, "B")
	if len(es) != 1 || len(es[0].Files) != 1 || es[0].Files[0].Name != "build.log" || es[0].FilesPending {
		t.Fatalf("entry %+v", es)
	}
	r := must(fetch(w, "B", m, h, "A"))
	if !r.Complete || r.Got != 14 {
		t.Fatalf("fetch %+v", r)
	}
	fi := wantState(t, w, "B", m, h, FileHere)
	if len(fi.Holders) != 1 || fi.Holders[0].Machine != "A" || fi.Holders[0].Answer != AnsHas {
		t.Fatalf("holders %+v", fi.Holders)
	}
	if !w.CopyFor("B", "b", m, h) {
		t.Fatal("no copy was written for the agent")
	}
}

// F5: before the list arrives, the reader is told the message has files.
// Requirements: L1, U4
func TestAReaderIsToldAMessageHasFilesBeforeTheirListArrives(t *testing.T) {
	w := room("A", "B")
	outboundOnly(w, "A", "B") // nobody can dial anybody: text by offer, no list
	m, h := sendFile(t, w, "A", "B", "see list", "l.txt", "l")
	w.Machine("A").NoInbound = false
	pull(t, w, "B", "A")
	w.Machine("A").NoInbound = true
	// The list came with that pull; take it away to see the pending state.
	delete(w.Machine("B").st.files.held, manifestHash(w, "B", m))
	es := shownTo(t, w, "B")
	if len(es) != 1 || !es[0].FilesPending || es[0].Files != nil || es[0].Text != "see list" {
		t.Fatalf("entry %+v", es)
	}
	wantState(t, w, "B", m, h, FileListPending)
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
	if _, err := fetch(w, "C", m, h, "B"); !errors.Is(err, ErrBadFile) || w.HoldsAnyOf("C", h) {
		t.Fatalf("wrong bytes: %v", err)
	}
	before := len(w.Transfers())
	w.TamperFiles(func(s MachineID, _, data string) string {
		if s == "B" {
			return data + "and more, never ending"
		}
		return data
	})
	if _, err := fetch(w, "C", m, h, "B"); !errors.Is(err, ErrBadFile) || w.HoldsAnyOf("C", h) {
		t.Fatalf("more bytes than declared: %v", err)
	}
	if len(w.Transfers()) != before {
		t.Fatal("a wrong declared total was received before it was refused")
	}
	w.TamperFiles(nil)
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
	if _, ok := w.FileBytes("C", h); ok || w.CopyFor("C", "c", m, h) {
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

// F5: the limit of lazy fetching. Requirements: L1, U4
func TestAFileWhoseOnlyHolderIsGoneIsUnavailableAndTheReaderIsToldSo(t *testing.T) {
	w := room("A", "B", "C")
	w.SetFileMode("C", FilesOff)
	m, h := sendFile(t, w, "A", "", "the plan", "plan.md", "# plan")
	pull(t, w, "B", "A")
	pull(t, w, "C", "A")
	shownTo(t, w, "B")
	nid := w.DiskLost("A")
	w.SetFileMode(nid, FilesOff)
	pull(t, w, nid, "B")
	for _, s := range []MachineID{nid, "C"} {
		if _, err := fetch(w, "B", m, h, s); !errors.Is(err, ErrNotHeldFile) {
			t.Fatalf("fetch from %s: %v", s, err)
		}
	}
	fi := wantState(t, w, "B", m, h, FileUnavailable)
	if len(fi.Holders) != 2 || fi.Holders[0].Answer != AnsNo || fi.Holders[1].Answer != AnsNo {
		t.Fatalf("holders %+v", fi.Holders)
	}
	if es := w.Thread("B", m); len(es) != 1 || es[0].Text != "the plan" {
		t.Fatalf("the text must stay readable without the file: %+v", es)
	}
}

// F9 Keep: a machine its person set to keep files fetches them all and
// serves them after the author is gone. Requirements: L1, D2
func TestAMachineSetToKeepFilesServesThemAfterTheAuthorIsGone(t *testing.T) {
	w := room("A", "B", "K")
	w.SetFileMode("K", Keep)
	m, h := sendFile(t, w, "A", "B", "for B only", "notes.txt", "hello B")
	pull(t, w, "K", "A")
	if !w.HoldsAnyOf("K", h) {
		t.Fatal("the keeper did not fetch the file")
	}
	pull(t, w, "B", "A")
	if w.HoldsAnyOf("B", h) {
		t.Fatal("a machine not set to keep fetched the file")
	}
	w.DiskLost("A")
	pull(t, w, "B", "K")
	shownTo(t, w, "B")
	if r := must(fetch(w, "B", m, h, "K")); !r.Complete {
		t.Fatalf("from the keeper %+v", r)
	}
}

// F9: the owner's room. Requirements: L1, N1
func TestInARoomWithOneListenerAFileFromAnOutboundOnlyAuthorReachesAnOutboundOnlyReader(t *testing.T) {
	w := room("A", "S", "R")
	outboundOnly(w, "A", "R")
	m, h := sendFile(t, w, "A", "R", "from the laptop", "diff.patch", "--- a\n+++ b\n")
	pull(t, w, "A", "S")
	must(w.Pull("A", "S", PullOpt{Offer: true})) // the text reaches S by offer; S wants the list, A uploads it
	if _, known := w.ManifestOf("S", m); !known {
		t.Fatal("S did not get the list from the outbound-only author")
	}
	pull(t, w, "R", "S")
	if _, known := w.ManifestOf("R", m); !known {
		t.Fatal("R did not get the list from S")
	}
	shownTo(t, w, "R")
	if _, err := fetch(w, "R", m, h, "A"); !errors.Is(err, ErrNoDial) {
		t.Fatalf("R cannot dial A: %v", err)
	}
	if _, err := fetch(w, "R", m, h, "S"); !errors.Is(err, ErrNotYet) {
		t.Fatalf("S should carry the file: %v", err)
	}
	wantState(t, w, "R", m, h, FileComing)
	pull(t, w, "A", "S") // S's answer wants the file; A uploads it over its own connection
	if r := must(fetch(w, "R", m, h, "S")); !r.Complete {
		t.Fatalf("R after the upload: %+v", r)
	}
	if b, _ := w.FileBytes("R", h); b != "--- a\n+++ b\n" {
		t.Fatalf("bytes %q", b)
	}
	for _, tr := range w.Transfers() {
		if !tr.Permitted || !tr.Keeps {
			t.Fatalf("transfer %+v", tr)
		}
	}
}

// F9: a reader that takes connections gets the file by upload when the
// author next pulls from it. Requirements: L1, N1
func TestAReaderThatListensGetsAFileFromAnOutboundOnlyAuthorByUpload(t *testing.T) {
	w := room("A", "B")
	outboundOnly(w, "A")
	m, h := sendFile(t, w, "A", "B", "for B", "b.txt", "bee")
	pull(t, w, "A", "B")
	must(w.Pull("A", "B", PullOpt{Offer: true}))
	shownTo(t, w, "B")
	if _, err := fetch(w, "B", m, h, "A"); !errors.Is(err, ErrNoDial) {
		t.Fatalf("%v", err)
	}
	wantState(t, w, "B", m, h, FileComing)
	pull(t, w, "A", "B")
	if r := must(fetch(w, "B", m, h, "A")); !r.Local {
		t.Fatalf("after the upload the file should be here: %+v", r)
	}
}

// F9 negative: carry off. Requirements: L1, N1
func TestWithCarryOffTheListenerAnswersNotHeldAndTheFileStaysUnavailable(t *testing.T) {
	w := room("A", "S", "R")
	outboundOnly(w, "A", "R")
	w.SetFileMode("S", FilesOff)
	m, h := sendFile(t, w, "A", "R", "x", "x.txt", "xx")
	pull(t, w, "A", "S")
	must(w.Pull("A", "S", PullOpt{Offer: true}))
	pull(t, w, "R", "S")
	shownTo(t, w, "R")
	if _, err := fetch(w, "R", m, h, "S"); !errors.Is(err, ErrNotHeldFile) {
		t.Fatalf("carry off: %v", err)
	}
	pull(t, w, "A", "S")
	if w.HoldsAnyOf("S", h) {
		t.Fatal("S took the file with carry off")
	}
	wantState(t, w, "R", m, h, FileUnavailable)
}

// F9, F7 negative: over budget. Requirements: L1, N1
func TestAListenerOverItsBudgetDoesNotCarryAFile(t *testing.T) {
	w := room("A", "S", "R")
	outboundOnly(w, "A", "R")
	w.SetFileBudget("S", 150) // room for the list, not for the file
	m, h := sendFile(t, w, "A", "R", "big", "big", string(make([]byte, 200)))
	pull(t, w, "A", "S")
	must(w.Pull("A", "S", PullOpt{Offer: true}))
	pull(t, w, "R", "S")
	shownTo(t, w, "R")
	if _, err := fetch(w, "R", m, h, "S"); !errors.Is(err, ErrNotHeldFile) {
		t.Fatalf("over budget: %v", err)
	}
	pull(t, w, "A", "S")
	if w.HoldsAnyOf("S", h) {
		t.Fatal("S took a file over its budget")
	}
}

// F2, F9 negative: a listener that may not read the text neither gets the
// list nor carries the file, so the outbound-only pair has no path.
// Requirements: L1, N1, T3
func TestAListenerThatMayNotReadTheTextNeitherCarriesNorReceivesTheFile(t *testing.T) {
	w := room("A", "R")
	outboundOnly(w, "A", "R")
	m, h := sendFile(t, w, "A", "R", "before S", "s.txt", "secret")
	w.Machine("A").NoInbound = false
	pull(t, w, "R", "A") // R has it all
	w.Machine("A").NoInbound = true
	must(w.Admit("S", "R", false, "s"))
	pull(t, w, "A", "S")
	must(w.Pull("A", "S", PullOpt{Offer: true}))
	must(w.Pull("R", "S", PullOpt{Offer: true}))
	if _, known := w.ManifestOf("S", m); known || w.HoldsAnyOf("S", manifestHash(w, "S", m)) {
		t.Fatal("a listener without history got the list")
	}
	// R asks S as if S could help: S neither serves nor carries.
	if _, err := w.fetch(w.Machine("R"), m, h, "S", FetchOpt{}); !errors.Is(err, ErrNotHeldFile) {
		t.Fatalf("%v", err)
	}
	if w.Machine("S").st.files.want[pkey{m, h}] {
		t.Fatal("S wants a file it may not read")
	}
}

// F9: a receiver takes only uploads it asked for. Requirements: L1
func TestAnUploadNobodyAskedForIsRefused(t *testing.T) {
	w := room("A", "B")
	m, h := sendFile(t, w, "A", "B", "x", "x", "xyz")
	pull(t, w, "B", "A")
	w.upload(w.Machine("A"), w.Machine("B"), pkey{m, h})
	if w.HoldsAnyOf("B", h) {
		t.Fatal("B took bytes it did not ask for")
	}
}

// F1, F10: withdrawal. Requirements: L1, D9
func TestWithdrawDeletesAFileItsListAndAnyPartOrCopyOnEveryMachineThatLearnsOfIt(t *testing.T) {
	w := room("A", "B", "C")
	m, h := sendFile(t, w, "A", "", "oops", "secret.env", "TOKEN=abc123")
	man := manifestHash(w, "A", m)
	for _, x := range []MachineID{"B", "C"} {
		pull(t, w, x, "A")
		shownTo(t, w, x)
	}
	must(fetch(w, "B", m, h, "A"))
	must(w.Fetch("C", "c", m, h, "A", FetchOpt{Max: 4}))
	must(w.Withdraw("A", "a", m))
	if w.HoldsAnyOf("A", h) || w.HoldsAnyOf("A", man) {
		t.Fatal("the author keeps the bytes or the list of its withdrawn message")
	}
	if !w.HoldsAnyOf("B", h) {
		t.Fatal("B dropped the bytes before it learned of the withdraw")
	}
	pull(t, w, "B", "A")
	pull(t, w, "C", "B")
	for _, x := range []MachineID{"B", "C"} {
		if w.HoldsAnyOf(x, h) || w.HoldsAnyOf(x, man) || len(w.Machine(x).st.files.disk) != 0 {
			t.Fatalf("%s keeps something of a withdrawn message: %v", x, w.Machine(x).st.files.disk)
		}
		if fi := wantState(t, w, x, m, h, FileGone); fi.Why != Withdrawn {
			t.Fatalf("%s: gone because %s", x, fi.Why)
		}
	}
}

// F2: every request is checked, so a withdraw between two parts stops the
// rest. Requirements: L1, D9
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
	pull(t, w, "C", "B")
	if w.HoldsAnyOf("C", h) {
		t.Fatal("C keeps the part after learning of the withdraw")
	}
}

// F8: nothing is carried forward silently. Requirements: L1, D9, P3
func TestAReplacementListsOnlyTheFilesItsWriterListsAgain(t *testing.T) {
	w := room("A", "B")
	m, h := sendFile(t, w, "A", "B", "v0", "a.txt", "aaa")
	pull(t, w, "B", "A")
	shownTo(t, w, "B")
	must(fetch(w, "B", m, h, "A"))

	// Keeping a file means listing it again, and a person confirms.
	n := must(w.ReplaceWithFiles("A", "a", m, "v1, same file", "sha256:"+h))
	must(w.Decide("A", n, true))
	pull(t, w, "B", "A")
	es := shownTo(t, w, "B")
	if len(es) != 1 || es[0].Kind != ReplacedNotice || len(es[0].Files) != 1 || es[0].Files[0].Hash != h {
		t.Fatalf("replaced notice %+v", es)
	}
	wantState(t, w, "B", m, h, FileHere)

	// A plain replacement drops the files, and their list.
	must(w.Replace("A", "a", m, "v2, no files"))
	pull(t, w, "B", "A")
	if fi := wantState(t, w, "B", m, h, FileGone); fi.Why != Live {
		t.Fatalf("why %s, want the replacement dropped it", fi.Why)
	}
	if w.HoldsAnyOf("B", h) || w.HoldsAnyOf("A", h) {
		t.Fatal("bytes of a file no version lists are kept")
	}
	if _, err := w.ReplaceWithFiles("A", "a", m, "v3", "sha256:"+h); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("a dropped file cannot be listed again by hash: %v", err)
	}
}

// F4: an agent fetches only files of a version it was shown.
// Requirements: L1, D9
func TestAnAgentMayNotFetchAFileOfAReplacementItWasNotShown(t *testing.T) {
	w := room("A", "B")
	m, _ := sendFile(t, w, "A", "B", "v0", "a.txt", "aaa")
	pull(t, w, "B", "A")
	shownTo(t, w, "B")
	w.PutLocalFile("A", "b.txt", "bbb")
	n := must(w.ReplaceWithFiles("A", "a", m, "v1", "b.txt"))
	must(w.Decide("A", n, true))
	pull(t, w, "B", "A")
	if _, err := fetch(w, "B", m, hashBody("bbb"), "A"); !errors.Is(err, ErrNotAllow) {
		t.Fatalf("fetch of a version never shown: %v", err)
	}
	shownTo(t, w, "B")
	must(fetch(w, "B", m, hashBody("bbb"), "A"))
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
	if w.HoldsAnyOf("B", h) || w.HoldsAnyOf("B", manifestHash(w, "B", m)) {
		t.Fatal("B's clock is past 30 days and the bytes or the list are still there")
	}
	if fi := wantState(t, w, "B", m, h, FileGone); fi.Why != Expired {
		t.Fatalf("why %s", fi.Why)
	}
	if !w.HoldsAnyOf("A", h) {
		t.Fatal("A's clock is not past 30 days")
	}
	w.SetSkew("B", 0) // the clock goes back: expired stays expired
	if _, err := fetch(w, "B", m, h, "A"); !errors.Is(err, ErrFileGone) && !errors.Is(err, ErrNotAllow) {
		t.Fatalf("fetch after expiry with the clock set back: %v", err)
	}
	w.Advance(2 * Day)
	if w.HoldsAnyOf("A", h) {
		t.Fatal("the author keeps bytes past 30 days")
	}
}

// F2, F1: the list and the bytes are behind the text's permission.
// Requirements: L1, T3
func TestANewcomerWithoutHistoryGetsNeitherTheListNorTheBytesOfAnEarlierFile(t *testing.T) {
	w := room("A", "B")
	m, h := sendFile(t, w, "A", "", "before D", "old.txt", "old")
	pull(t, w, "B", "A")
	must(w.Admit("D", "B", false, "d"))
	pull(t, w, "D", "B")
	pull(t, w, "D", "A")
	man := manifestHash(w, "D", m)
	if _, known := w.ManifestOf("D", m); known || w.HoldsAnyOf("D", man) {
		t.Fatal("D holds the list of a message it may not read")
	}
	if fi := wantState(t, w, "D", m, h, FileGone); fi.Why != NotAdmitted {
		t.Fatalf("why %s", fi.Why)
	}
	for _, s := range []MachineID{"A", "B"} {
		for _, x := range []string{man, h} {
			if _, ok := w.Machine(s).mayServe("D", m, x, w.Now); ok {
				t.Fatalf("%s would serve %s to a no-history newcomer", s, x[:8])
			}
		}
	}
	m2, h2 := sendFile(t, w, "A", "", "after D", "new.txt", "new")
	pull(t, w, "D", "A")
	shownTo(t, w, "D")
	if r := must(fetch(w, "D", m2, h2, "A")); !r.Complete {
		t.Fatalf("%+v", r)
	}
}

// F2, defence in depth: even a member that learned a hash elsewhere
// cannot have it served by listing it in a message of its own.
// Requirements: L1, T3
func TestANewReferenceToAKnownHashDoesNotWidenWhoIsServed(t *testing.T) {
	w := room("A", "B")
	m1, h := sendFile(t, w, "A", "", "before D", "payroll.csv", "alice,100")
	pull(t, w, "B", "A")
	shownTo(t, w, "B")
	must(fetch(w, "B", m1, h, "A"))
	must(w.Admit("D", "B", false, "d"))
	pull(t, w, "D", "B")
	// D writes a message whose manifest lists the hash, without the bytes.
	man := encodeManifest("d-salt", []FileRef{{Name: "x", Size: len("alice,100"), Hash: h}})
	m2 := must(w.send("D", "d", Msg{Body: "mine"}, hashBody(man)))
	w.anyFiles = true
	w.Machine("D").commitBlob(hashBody(man), man, m2)
	for _, s := range []MachineID{"A", "B"} {
		pull(t, w, s, "D")
	}
	if _, err := w.fetch(w.Machine("D"), m2, h, "B", FetchOpt{}); !errors.Is(err, ErrNotHeldFile) && !errors.Is(err, ErrNotYet) {
		t.Fatalf("B served a known hash for D's new message: %v", err)
	}
	if w.HoldsAnyOf("D", h) {
		t.Fatal("D got the bytes")
	}
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
	pull(t, w, "B", "A")
	shownTo(t, w, "B")
	must(fetch(w, "B", m, h, "A"))
	if r := must(fetch(w, "C", m, h, "B")); !r.Complete { // removal is not instant, as for text
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
	m1, h1 := sendFile(t, w, "A", "B", "one", "1.bin", "123456")
	m2, h2 := sendFile(t, w, "A", "B", "two", "2.bin", "abcdef")
	pull(t, w, "B", "A")
	shownTo(t, w, "B")
	w.SetFileBudget("B", w.Machine("B").st.files.used()+16)
	must(fetch(w, "B", m1, h1, "A"))
	before := len(w.Transfers())
	if r, err := fetch(w, "B", m2, h2, "A"); !errors.Is(err, ErrNoSpace) || r.Got != 0 || len(w.Transfers()) != before {
		t.Fatalf("over budget: %+v %v", r, err)
	}
	must(w.Withdraw("A", "a", m1))
	pull(t, w, "B", "A")
	if r := must(fetch(w, "B", m2, h2, "A")); !r.Complete {
		t.Fatalf("after the first went: %+v", r)
	}
}

// F7, F3: an impossible reference is refused before anything is
// scheduled. Requirements: L1
func TestAnImpossibleReferenceIsRefusedBeforeAnyTransfer(t *testing.T) {
	w := room("A", "B")
	h := hashBody("x")
	man := encodeManifest("s", []FileRef{{Name: "huge", Size: MaxFileSize + 1, Hash: h}})
	m := must(w.send("A", "a", Msg{Body: "huge"}, hashBody(man)))
	w.anyFiles = true
	w.Machine("A").commitBlob(hashBody(man), man, m)
	w.Machine("A").commitBlob(h, "x", m)
	pull(t, w, "B", "A")
	shownTo(t, w, "B")
	before := len(w.Transfers())
	if _, err := fetch(w, "B", m, h, "A"); !errors.Is(err, ErrTooLarge) || len(w.Transfers()) != before {
		t.Fatalf("%v", err)
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
	if w.HoldsAnyOf("B", h) || len(w.Machine("B").st.files.disk) != 0 {
		t.Fatal("B keeps the file of a voided author")
	}
}

// F1: a narrower grant learned later drops the files it hides, as it
// drops their text (gap G7: silently). Requirements: L1, T3
func TestANarrowerGrantLearnedLaterDropsTheFilesItHides(t *testing.T) {
	w := room("A", "B")
	m, h := sendFile(t, w, "A", "", "early", "e.txt", "eee")
	pull(t, w, "B", "A")
	must(w.Admit("D", "A", true, "d"))
	pull(t, w, "D", "A")
	showThread(t, w, "D", m) // D's inbox starts after its admission (U3)
	must(fetch(w, "D", m, h, "A"))
	must(w.Grant("D", "B", false))
	if w.HoldsAnyOf("D", h) || w.HoldsAnyOf("D", manifestHash(w, "D", m)) {
		t.Fatal("D keeps a file its narrower permission hides")
	}
}

// F10: a crash after a file is written and before its row.
// Requirements: L1, D9
func TestACrashBetweenWritingAFileAndItsRowLeavesNothingAfterTheRestart(t *testing.T) {
	w := room("A", "B")
	m, h := sendFile(t, w, "A", "B", "x", "x", "xyz")
	pull(t, w, "B", "A")
	shownTo(t, w, "B")
	w.FailFiles("B", "rename")
	fetch(w, "B", m, h, "A")
	if !w.Machine("B").st.files.disk[blobPath(h)] || w.Machine("B").st.files.held[h] != nil {
		t.Fatal("the injected crash did not happen where it should")
	}
	w.Crash("B")
	if w.HoldsAnyOf("B", h) {
		t.Fatal("the restart left a file no row names")
	}
	if r := must(fetch(w, "B", m, h, "A")); !r.Complete {
		t.Fatalf("again after the restart: %+v", r)
	}
}

// F10: a crash after a deletion is committed and before the file goes.
// Requirements: L1, D9
func TestACrashBetweenDroppingAFileAndDeletingItCompletesTheDeletionAtTheRestart(t *testing.T) {
	w := room("A", "B")
	m, h := sendFile(t, w, "A", "B", "x", "x", "xyz")
	pull(t, w, "B", "A")
	shownTo(t, w, "B")
	must(fetch(w, "B", m, h, "A"))
	must(w.Withdraw("A", "a", m))
	w.FailFiles("B", "commit")
	pull(t, w, "B", "A")
	f := w.Machine("B").st.files
	if f.held[h] != nil || !f.disk[blobPath(h)] || len(f.queue) == 0 {
		t.Fatal("the injected crash did not happen where it should")
	}
	w.Crash("B")
	if w.HoldsAnyOf("B", h) || len(w.Machine("B").st.files.disk) != 0 {
		t.Fatal("the restart did not finish the deletion")
	}
}

// F1 to F10 on random schedules. After every step: every held file
// matches its hash and is kept for a message that lets the machine keep
// it and, by the true grants, read it; no machine that holds the withdraw
// holds the bytes; the disk is what the rows say (except right after an
// injected crash, until the restart). Every transfer went to a machine the
// sender saw as a member, that the true grants let read the version, and
// that could keep it. Requirements: L1, N1, D7, D9, T3, T5
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
		total.uploads += st.uploads
		total.crashes += st.crashes
	}
	if total.complete < runs/2 || total.refused < runs || total.dropped < runs/4 || total.uploads < runs/4 || total.crashes < runs/8 {
		t.Fatalf("schedules exercised too little: %+v", total)
	}
	t.Logf("%d schedules: %+v", runs, total)
}

type fileStats struct{ complete, refused, dropped, uploads, crashes int }

func fileSchedule(seed int64, steps int) (fileStats, error) {
	var st fileStats
	rng := rand.New(rand.NewSource(seed))
	w := room("A", "B", "C")
	for _, id := range []MachineID{"A", "B", "C"} {
		if rng.Intn(3) == 0 {
			w.Machine(id).NoInbound = true
		}
		w.SetFileMode(id, FileMode(rng.Intn(3)))
	}
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
		switch rng.Intn(16) {
		case 0, 1:
			op = "send"
			file++
			data := fmt.Sprintf("file-%d-%s", file, a)
			msg := Msg{Body: "m"}
			if rng.Intn(2) == 0 && a != b {
				msg.To = []Addr{at(b)}
			}
			w.PutLocalFile(a, "f", data)
			if n, err := w.SendFiles(a, ag(a), msg, "f"); err == nil {
				if id, err := w.Decide(a, n, rng.Intn(5) > 0); err == nil && id != "" {
					msgs = append(msgs, sent{id, hashBody(data), a})
				}
			}
		case 2, 3, 4:
			op = "pull"
			w.Pull(a, b, PullOpt{Offer: rng.Intn(2) == 0})
		case 5, 6, 7:
			op = "fetch"
			if len(msgs) > 0 {
				s := msgs[rng.Intn(len(msgs))]
				from := b
				if rng.Intn(2) == 0 {
					from = s.from
				}
				w.Pull(a, from, PullOpt{})
				if es, err := w.List(a, ag(a)); err == nil {
					_ = w.Seen(a, ag(a), es.Entries)
					_ = w.Showed(a, ag(a), w.Thread(a, s.msg))
					opt := FetchOpt{}
					if rng.Intn(3) == 0 {
						opt.Max = 4
					}
					if r, err := w.Fetch(a, ag(a), s.msg, s.hash, from, opt); err == nil && r.Complete { // fetched now, or carried or kept here before
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
				} else if n, err := w.ReplaceWithFiles(s.from, ag(s.from), s.msg, "r", "sha256:"+s.hash); err == nil {
					w.Decide(s.from, n, true)
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
		case 14:
			op = "crash"
			if m := w.machines[a]; m.Awake && !m.Gone {
				w.FailFiles(a, []string{"rename", "commit"}[rng.Intn(2)])
				st.crashes++
			}
		case 15:
			op = "restart"
			w.Crash(a)
		}
		if heldCount(w) < before {
			st.dropped++
		}
		if err := checkFiles(w); err != nil {
			return st, fmt.Errorf("step %d (%s %s %s): %v", step, op, a, b, err)
		}
	}
	for _, tr := range w.transfers {
		if tr.Upload {
			st.uploads++
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
	for i, tr := range w.transfers {
		if !tr.Permitted || !tr.Keeps {
			return fmt.Errorf("transfer %d %+v", i, tr)
		}
	}
	ids := append([]MachineID(nil), w.order...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
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
					v, _, ok := m.referenced(msg, hash, w.Now)
					if !ok {
						dbg := ""
						if h := m.get(msg); h != nil {
							wv := m.winner(h)
							_, known := m.manifestOf(wv)
							dbg = fmt.Sprintf(" [dropped %v winner %s v%d man %q known %v keepable %v crash %v gone %v]", h.dropped, wv.rec.ID, wv.rec.Version, wv.rec.Manifest, known, m.keepable(wv, w.Now), f.crash, m.textGone(wv.rec, w.Now))
						}
						return fmt.Errorf("%s holds %s for %s, which no longer lets it keep the file%s", id, hash[:8], msg, dbg)
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
		for k := range f.partial {
			if _, _, ok := m.referenced(k.msg, k.hash, w.Now); !ok {
				return fmt.Errorf("%s keeps part of %s that nothing lets it keep", id, k.hash[:8])
			}
		}
		for k := range f.copies {
			if _, _, ok := m.referenced(k.msg, k.hash, w.Now); !ok {
				return fmt.Errorf("%s keeps a copy of %s for %s that nothing lets it keep", id, k.hash[:8], k.agent)
			}
		}
		if f.crash {
			continue
		}
		rows := f.rowPaths()
		for p := range f.disk {
			if !rows[p] {
				return fmt.Errorf("%s has %s on disk with no row", id, p)
			}
		}
		for p := range rows {
			if !f.disk[p] {
				return fmt.Errorf("%s has a row for %s and no file", id, p)
			}
		}
	}
	return nil
}
