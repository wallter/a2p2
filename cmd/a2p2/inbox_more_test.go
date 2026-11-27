package main

import (
	"fmt"
	"strings"
	"testing"
)

// [U4] a2p2 inbox applies no byte budget (D70) and, when its limit leaves
// messages out, says how many are still unread; the next call reads them.
func TestInboxSaysHowManyMoreAreUnreadWhenItsLimitLeftSomeOut(t *testing.T) {
	d := newDaemonForCLI(t)
	if _, _, err := d.run(t, false, "", "inbox", "--as", "reader", "--room", "team"); err != nil { // registers it
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, _, err := d.run(t, false, "", "send", "--as", "bot", "--room", "team", fmt.Sprint("note ", i)); err != nil {
			t.Fatal(err)
		}
	}
	out, _, err := d.run(t, false, "", "inbox", "--as", "reader", "--room", "team", "--limit", "3")
	if err != nil || strings.Count(out, "note ") != 3 || !strings.Contains(out, "(2 more unread: run a2p2 inbox again)") {
		t.Fatalf("first: %v\n%s", err, out)
	}
	out, _, err = d.run(t, false, "", "inbox", "--as", "reader", "--room", "team")
	if err != nil || strings.Count(out, "note ") != 2 || strings.Contains(out, "more unread") {
		t.Fatalf("second: %v\n%s", err, out)
	}
}
