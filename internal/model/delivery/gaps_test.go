package delivery

import "testing"

// Known gaps. Each test builds the smallest case and skips with
// "KNOWN GAP" when the gap shows; if a later rule closes it, the test passes.

// Requirements: D5, A4
func TestAuthorLearnsOfCopiesOnMachinesItNeverMet(t *testing.T) {
	w := room("A", "B", "C")
	m := send(t, w, "A", "C", "x")
	pull(t, w, "B", "A")
	w.Sleep("A")
	pull(t, w, "C", "B")
	w.Wake("A")
	w.Sleep("C")
	pull(t, w, "A", "B")
	for _, c := range w.Status("A", m).Copies {
		if c.Machine == "C" {
			return
		}
	}
	t.Skip("KNOWN GAP: copies are learned only from machines met directly. A never met C, " +
		"so it cannot report C's copy. Closing it means passing on other machines' summaries " +
		"in every pull (about members x streams numbers per pull); not done in stage 1.")
}

// Requirements: T3, U3
func TestNewcomerWithoutHistoryIsNotHandedPostsItsAdmitterHadNotSeen(t *testing.T) {
	w := room("A", "B", "C")
	old := must(w.Send("C", "c", Msg{Body: "room-wide, before N joined"}))
	// B admits N before B has heard of C's post.
	must(w.Admit("N", "B", false, "n"))
	pull(t, w, "N", "C")
	es := read(t, w, "N")
	if h := w.Machine("N").get(old); h != nil && h.dropped == NotAdmitted && len(es) == 0 {
		return
	}
	t.Skip("KNOWN GAP: 'no earlier history' is drawn at the admitting member's positions. A " +
		"stream the admitter had not yet seen counts as new, so N is handed the text of a post " +
		"written before it joined, and its inbox shows it (" + texts(es) + "). A creation-time " +
		"cut-off was tried and rejected: with clocks that differ it hides messages written " +
		"after the admission, which is worse. Stated as a limit.")
}

// Requirements: D10
func TestBranchUnderAMessageIncludesRepliesBelowAMissingReply(t *testing.T) {
	w := room("A", "B", "C", "D")
	root := must(w.Send("A", "a", Msg{Body: "root"}))
	pull(t, w, "B", "A")
	mid := must(w.Send("B", "b", Msg{Body: "mid", Parent: root}))
	pull(t, w, "C", "B")
	leaf := must(w.Send("C", "c", Msg{Body: "leaf", Parent: mid}))
	w.Withhold("C", mid)
	pull(t, w, "D", "C") // D gets root and leaf, not mid
	for _, e := range w.Branch("D", root) {
		if e.Record == leaf {
			return
		}
	}
	t.Skip("KNOWN GAP: a reply records only its parent and its thread's root, so the branch " +
		"under a message cannot reach replies below a reply this machine does not hold. Thread " +
		"view still lists them (parent marked missing). Closing it needs the reply to carry its " +
		"ancestors' ids.")
}

// Requirements: D2, limit "a member could withhold one"
func TestWithholderThatUnderstatesWhatItHoldsIsDetected(t *testing.T) {
	w := room("A", "B", "C")
	send(t, w, "A", "C", "1")
	m2 := send(t, w, "A", "C", "2")
	pull(t, w, "B", "A")
	w.Sleep("A")
	b := w.Machine("B")
	w.Withhold("B", m2)
	// B also lies in its summary, as a dishonest member would.
	b.st.contig[w.Machine("B").get(m2).rec.Stream()] = 1
	pull(t, w, "C", "B")
	if v := must(w.Inbox("C", "c")); len(v.Waiting) > 0 {
		return
	}
	t.Skip("KNOWN GAP: a member that leaves out the end of a stream and says it holds no more " +
		"is not caught by a machine that pulls only from it. Signatures stop forgery, not " +
		"omission. Pulling from more than one member closes it in practice.")
}

// Requirements: D5
func TestCopyReportsDoNotOutliveAHolderRestoredToBeforeTheCopy(t *testing.T) {
	w := room("A", "B", "C")
	bb, bc := w.Backup("B"), w.Backup("C")
	m := send(t, w, "A", "C", "x")
	pull(t, w, "B", "A")
	pull(t, w, "C", "A")
	pull(t, w, "A", "B")
	pull(t, w, "A", "C")
	w.Restore("B", bb)
	w.Restore("C", bc)
	w.Sleep("B")
	w.Sleep("C")
	if len(w.Status("A", m).Copies) == 0 {
		return
	}
	t.Skip("KNOWN GAP: a copy report is true when made and dated, but it is not withdrawn " +
		"when the holder loses the copy: both holders were restored to before the copy and A " +
		"still lists two dated reports. Repair does not depend on reports (every machine keeps " +
		"pulling from every other). Only fresh contact corrects them.")
}

// Requirements: T5, D6
func TestWithdrawThatOnlyTheRemovedAuthorHoldsStillReachesMembers(t *testing.T) {
	w := room("A", "B", "C")
	m := send(t, w, "A", "B", "oops")
	pull(t, w, "B", "A")
	must(w.Withdraw("A", "a", m))
	must(0, w.Remove("A", "C"))
	learnAll(w)
	_, _ = w.Pull("B", "A", PullOpt{})
	if !holdsText(w, "B", m) {
		return
	}
	t.Skip("KNOWN GAP: records are taken only from members, so a withdraw (or anything) the " +
		"removed machine wrote and no member fetched before learning of the removal never " +
		"arrives; B keeps showing the text. Closing it means taking a removed author's own " +
		"records up to a cut-off named in the removal, which needs the removal to carry the " +
		"author's heads; not done.")
}

// Requirements: T3, U3
func TestAMoreRestrictiveGrantLearnedLaterDoesNotTakeBackTextAlreadyShown(t *testing.T) {
	w := room("A", "B", "C")
	must(w.Grant("D", "A", true, "d"))
	learnAll(w)
	b := must(w.Send("B", "b", Msg{Body: "before"}))
	pull(t, w, "A", "B")
	pull(t, w, "D", "B")
	wantTexts(t, read(t, w, "D"), "shown:before")
	must(w.Grant("D", "A", false)) // A's later grant: no history, heads past "before"
	if holdsText(w, "D", b) {
		t.Fatal("D kept text its narrower grant forbids")
	}
	if es := read(t, w, "D"); len(es) != 0 {
		return // told: closed
	}
	t.Skip("KNOWN GAP: the more restrictive grant applies from when a machine learns it. D " +
		"drops the text but its agent was already shown it, and is not told. The members " +
		"model decides whether a second grant may narrow the first; here it is a limit.")
}
