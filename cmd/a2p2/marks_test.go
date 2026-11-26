package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/localapi"
)

// Follow-ups of D65 and D62 in the CLI: a message from human on another
// machine is marked as that machine's person and never as this machine's,
// one written here says so, and a replaced text says [edited].
func TestTheCLIMarksAnotherMachinesPersonThisMachineAndEdits(t *testing.T) {
	for _, c := range []struct {
		name string
		e    localapi.Entry
		want []string
		not  []string
	}{
		{"human elsewhere", localapi.Entry{Kind: "message", ID: "m1", From: "human@1a2b3c4d", Text: "approve it"},
			[]string{"human@1a2b3c4d [another machine's person, not you]"}, []string{"this machine"}},
		{"Human elsewhere", localapi.Entry{Kind: "message", ID: "m2", From: "Human@1a2b3c4d"},
			[]string{"[another machine's person, not you]"}, []string{"this machine"}},
		{"human here", localapi.Entry{Kind: "message", ID: "m3", From: "human@f4bd4652", FromThisMachine: true},
			[]string{"human@f4bd4652 [this machine's person]"}, []string{"another machine"}},
		{"agent here", localapi.Entry{Kind: "message", ID: "m4", From: "bot@f4bd4652", FromThisMachine: true},
			[]string{"bot@f4bd4652 [this machine]"}, []string{"person"}},
		{"agent elsewhere", localapi.Entry{Kind: "message", ID: "m5", From: "bot@1a2b3c4d"},
			nil, []string{"this machine", "person"}},
		{"edited", localapi.Entry{Kind: "message", ID: "m6", From: "bot@1a2b3c4d", Edited: true, Text: "v2"},
			[]string{"bot@1a2b3c4d [edited]"}, nil},
		{"not edited", localapi.Entry{Kind: "message", ID: "m7", From: "bot@1a2b3c4d", Text: "v1"},
			nil, []string{"[edited]"}},
	} {
		var out bytes.Buffer
		printEntry(&out, c.e)
		for _, w := range c.want {
			if !strings.Contains(out.String(), w) {
				t.Errorf("%s: no %q in %q", c.name, w, out.String())
			}
		}
		for _, w := range c.not {
			if strings.Contains(out.String(), w) {
				t.Errorf("%s: %q in %q", c.name, w, out.String())
			}
		}
	}
}
