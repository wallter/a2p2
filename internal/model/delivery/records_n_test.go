package delivery

import (
	"errors"
	"testing"
)

// Requirements: D5, D7. F67 in the model: a handled report of a message
// whose first text expired while a replacement is live needs the
// replacement shown, but only from an agent that confirmed text of it,
// since only its inbox shows the replace notice. An agent that only
// listed the message, or read its thread, is shown the expired notice by
// every view, and its report goes through as before (node
// TestAHandledReportOfAnExpiredMessageNeedsItsLiveReplacementShown).
func TestAHandledReportOfAnExpiredMessageWithALiveReplacementNeedsItShownOnlyAfterAConfirmedRead(t *testing.T) {
	w := levelRoom()
	read := post(w, Msg{Body: "v0", To: []Addr{{"A", "a"}}})
	listed := post(w, Msg{Body: "w0", To: []Addr{{"A", "b"}}})
	pull(t, w, "A", "B")
	readAs(t, w, "A", "a")
	must(w.List("A", "b"))
	w.Advance(Retention - 3*Hour)
	must(w.Replace("B", "x", read, "v1"))
	must(w.Replace("B", "x", listed, "w1"))
	pull(t, w, "A", "B")
	w.Advance(3 * Hour) // the first texts expire, the replacements are live

	if _, err := w.Handle("A", "a", read); !errors.Is(err, ErrNotAllow) {
		t.Fatalf("handled with only the expired text confirmed: %v, want not allowed", err)
	}
	wantTexts(t, must(w.List("A", "a")).Entries, "replaced:v1")
	must(w.Handle("A", "a", read))

	wantTexts(t, must(w.List("A", "b")).Entries, "expired:")
	th := w.Thread("A", listed)
	wantTexts(t, th, "expired:")
	if err := w.Showed("A", "b", th); err != nil {
		t.Fatal(err)
	}
	must(w.Handle("A", "b", listed))
}
