package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// F45 (security review 2026-11-25): a seconding request's name was written
// by the other admin, and confirm says so instead of "the machine chose
// its own name".
func TestConfirmSaysTheOtherAdminChoseTheNameOfASecondingRequest(t *testing.T) {
	for from, want := range map[string]string{
		node.NameFromAdmin:   "The admin who signed the first entry chose this name.",
		node.NameFromMachine: "The machine chose its own name.",
		node.NameFromHere:    "The name was given on this machine.",
		"":                   "Check the machine's fingerprint, not its name.",
	} {
		var out bytes.Buffer
		printTerms(&out, localapi.Request{ID: "q_1", Name: "alice-laptop", NameFrom: from, Text: "make machine 0123 an admin"})
		got := out.String()
		if !strings.Contains(got, want) || from != node.NameFromMachine && strings.Contains(got, "chose its own name") {
			t.Errorf("name from %q: %q", from, got)
		}
	}
}

// F2: the join preview names the room by the label its creator chose, on
// another machine. It is escaped as the text of a request is
// (node.EscapeDisplay), so a label cannot clear the line or reorder the
// terms; an ordinary label is shown as it is.
func TestTheJoinPreviewEscapesTheRoomLabel(t *testing.T) {
	pv := localapi.PreviewResp{Room: localapi.JoinRoom{ID: "r_1", Label: "\r\x1b[2K\u202eops\U000E0061\nok"}, Role: "member",
		Admin: "0123456789abcdef", Expires: "2026-11-25T12:00:00Z"}
	got := previewText(pv)
	for _, c := range []string{"\r", "\x1b", "\u202e", "\U000E0061", "\n"} {
		if strings.Contains(got, c) {
			t.Errorf("preview holds a raw %q: %q", c, got)
		}
	}
	if !strings.Contains(got, `Invite to room "\u{d}\u{1b}[2K\u{202e}ops\u{e0061}\u{a}ok" (r_1), as member`) {
		t.Errorf("preview %q", got)
	}
	pv.Room.Label = "équipe 🚀"
	if got := previewText(pv); !strings.Contains(got, `room "équipe 🚀" (r_1)`) {
		t.Errorf("ordinary label: %q", got)
	}
}
