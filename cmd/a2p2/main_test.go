package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// notCaughtUp answers not_caught_up until an approval carries the
// override, as the daemon does for a room not checked since start.
func notCaughtUp(sent *[]localapi.ConfirmReq) func(localapi.ConfirmReq) (localapi.ConfirmResp, []byte, error) {
	return func(q localapi.ConfirmReq) (localapi.ConfirmResp, []byte, error) {
		*sent = append(*sent, q)
		if q.Approve && !q.NotCaughtUpOK {
			return localapi.ConfirmResp{ID: q.ID, State: store.RequestFailed, Reason: node.ReasonNotCaughtUp}, nil, nil
		}
		if !q.Approve {
			return localapi.ConfirmResp{ID: q.ID, State: store.RequestDeclined}, nil, nil
		}
		return localapi.ConfirmResp{ID: q.ID, State: store.RequestApproved}, nil, nil
	}
}

func TestConfirmNotCaughtUpShowsTheRiskAndNeedsASecondYes(t *testing.T) {
	var sent []localapi.ConfirmReq
	var warn bytes.Buffer
	r, _, err := decide(notCaughtUp(&sent), func(string) string { return "yes" }, &warn, localapi.ConfirmReq{ID: "q", Approve: true})
	if err != nil || r.State != store.RequestApproved || len(sent) != 2 || sent[0].NotCaughtUpOK || !sent[1].NotCaughtUpOK {
		t.Fatalf("approved anyway: %+v %v, sent %+v", r, err, sent)
	}
	if !strings.Contains(warn.String(), node.NotCaughtUpWarning) {
		t.Fatalf("the warning shown: %q", warn.String())
	}
}

func TestConfirmNotCaughtUpWithoutASecondYesDecidesNothing(t *testing.T) {
	var sent []localapi.ConfirmReq
	var warn bytes.Buffer
	if _, _, err := decide(notCaughtUp(&sent), func(string) string { return "no" }, &warn, localapi.ConfirmReq{ID: "q", Approve: true}); err == nil || len(sent) != 1 {
		t.Fatalf("without the second yes: %v, sent %+v", err, sent)
	}
	// A decline never asks and never carries the override.
	sent = nil
	asked := false
	r, _, err := decide(notCaughtUp(&sent), func(string) string { asked = true; return "yes" }, &warn, localapi.ConfirmReq{ID: "q"})
	if err != nil || asked || len(sent) != 1 || sent[0].NotCaughtUpOK || r.State != store.RequestDeclined {
		t.Fatalf("decline: %+v %v asked %v sent %+v", r, err, asked, sent)
	}
}

func TestConfirmShowsExactTermsOnceAndInviteDeadline(t *testing.T) {
	q := localapi.Request{
		ID: "q_1", Name: "laptop", Deadline: "2026-11-24T12:35:00Z",
		Text: "add machine 0123 4567 (laptop) to room team (r_1) as an admin, without earlier history",
	}
	var out bytes.Buffer
	printTerms(&out, q)
	got := out.String()
	if strings.Count(got, q.Text) != 1 || strings.Count(got, "0123 4567") != 1 ||
		!strings.Contains(got, q.ID) || !strings.Contains(got, clock(q.Deadline)) ||
		!strings.Contains(got, "chose its own name") {
		t.Fatalf("request terms shown: %q", got)
	}
}

func TestAlertsPointAtTheCommandForRequestsAndQuestions(t *testing.T) {
	var out bytes.Buffer
	printAlert(&out, localapi.Alert{Kind: node.AlertRequest, ID: "q_1", Room: "r_1", Text: "add machine 0123"})
	printAlert(&out, localapi.Alert{Kind: node.AlertQuestion, ID: "m1", Room: "r_1", Text: "bot@1a2b3c4d asks: Ship?"})
	printAlert(&out, localapi.Alert{Kind: node.AlertNetwork, Text: "networking is on, not listening: busy"})
	got := out.String()
	for _, want := range []string{"Networking\n  networking is on, not listening: busy", "a2p2 net off", "Review with a2p2 confirm q_1", "Question m1 in room r_1", "bot@1a2b3c4d asks: Ship?",
		"a2p2 send --room r_1 --reply-to m1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("alerts output %q lacks %q", got, want)
		}
	}
}

func TestTraceSaysWhatACopyReportIsInPlainWords(t *testing.T) {
	var out bytes.Buffer
	printTrace(&out, localapi.TraceResp{ID: "m1", Room: "r_1", Saved: true, Reason: store.ReasonLive,
		Copies: []localapi.Copy{{Node: "0123456789abcdef", At: "2026-11-24T12:00:00Z", Member: true}}})
	got := out.String()
	if !strings.Contains(got, copyReportWords) || !strings.Contains(got, "available on this machine") ||
		strings.Contains(got, "same position") {
		t.Fatalf("trace: %q", got)
	}
}

func TestCLIErrorsSayWhatToDoWithoutRuleCodes(t *testing.T) {
	for in, want := range map[string]string{
		"bad invite token: bad_base64 (S2): token":                 "Paste the whole invite",
		`"bob@12345678" is not an agent of a machine in this room`: "a2p2 room list ROOM",
		"no such request": "a2p2 confirm to list them",
		"ask_handled is only for a post to everyone":       "leave out --to",
		`"x" is not an agent id (label@node, as in peers)`: "human@1a2b3c4d",
	} {
		got := cliError(&localapi.Error{User: true, Msg: in}).Error()
		if !strings.Contains(got, want) || strings.Contains(got, "(S") || strings.Contains(got, "as in peers") {
			t.Errorf("%q -> %q, want it to contain %q", in, got, want)
		}
	}
}

func TestAckAlreadyReportedSaysSoAndNamesTheFirstReport(t *testing.T) {
	if got := ackText(localapi.AckResp{Handled: "h1"}); got != "handled, report h1" {
		t.Fatalf("first ack: %q", got)
	}
	if got := ackText(localapi.AckResp{Handled: "h1", Already: true}); got != "already reported handled, report h1" {
		t.Fatalf("repeated ack: %q", got)
	}
}

func TestJSONOutputWritesAngleBracketsAndAmpersandsAsWritten(t *testing.T) {
	var out bytes.Buffer
	if err := writeJSON(&out, map[string]string{"info": "<a> & b"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"<a> & b"`) || strings.Contains(out.String(), `\u003c`) {
		t.Fatalf("doctor --json: %q", out.String())
	}
}

func TestInstallReportsEndWithANewline(t *testing.T) {
	for _, in := range []string{"register MCP server", "one\ntwo", "done\n"} {
		var out bytes.Buffer
		printReport(&out, in)
		if !strings.HasSuffix(out.String(), "\n") || strings.HasSuffix(out.String(), "\n\n") {
			t.Fatalf("%q printed as %q", in, out.String())
		}
	}
	var out bytes.Buffer
	printReport(&out, "")
	if out.Len() != 0 {
		t.Fatalf("an empty report printed %q", out.String())
	}
}

func TestAskHandledWithAnAddresseeExplainsWhy(t *testing.T) {
	got := cliError(&localapi.Error{User: true, Msg: "ask_handled is only for a post to everyone"}).Error()
	for _, want := range []string{"posts to the whole room", "leave out --to", "addressee can always report a message handled"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q lacks %q", got, want)
		}
	}
}

func TestInstallAndUninstallAcceptFlagsOnEitherSideOfTheHarnessName(t *testing.T) {
	for _, name := range []string{"install", "uninstall"} {
		for _, args := range [][]string{
			{"claude", "--dry-run", "--on-stop"},
			{"--dry-run", "claude", "--on-stop"},
			{"--dry-run", "--on-stop", "claude"},
			{"claude", "--on-stop=true", "--dry-run", "--json"},
		} {
			c := newCmd(context.Background(), name)
			target, o, err := installArgs(c.fs, name, args)
			if err != nil || target != "claude" || !o.DryRun || !o.OnStop {
				t.Fatalf("%s %v: %q %+v %v", name, args, target, o, err)
			}
		}
		for _, args := range [][]string{{}, {"claude", "codex"}, {"claude", "--nope"}} {
			c := newCmd(context.Background(), name)
			c.fs.SetOutput(io.Discard)
			if _, _, err := installArgs(c.fs, name, args); !errors.Is(err, errUsage) {
				t.Fatalf("%s %v: want a usage error, got %v", name, args, err)
			}
		}
	}
}

// Every flag a usage line shows after a positional argument works there,
// including flags that take a value.
func TestFlagsAfterPositionalArgumentsAreParsed(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	b := fs.Bool("b", false, "")
	s := fs.String("s", "", "")
	n := fs.Int("wait", 0, "")
	for _, tc := range []struct {
		args []string
		pos  []string
		b    bool
		s    string
		n    int
	}{
		{[]string{"invite", "ROOM", "--b"}, []string{"invite", "ROOM"}, true, "", 0},
		{[]string{"ID", "--s", "bob", "--b"}, []string{"ID"}, true, "bob", 0},
		{[]string{"--wait", "5", "--b"}, nil, true, "", 5},
		{[]string{"on", "--wait=3", "ADDR"}, []string{"on", "ADDR"}, false, "", 3},
		{[]string{"-", "--b"}, []string{"-"}, true, "", 0},
		{[]string{"ID", "--", "--b"}, []string{"ID", "--b"}, false, "", 0},
	} {
		*b, *s, *n = false, "", 0
		if err := fs.Parse(permute(fs, tc.args)); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if strings.Join(fs.Args(), " ") != strings.Join(tc.pos, " ") || *b != tc.b || *s != tc.s || *n != tc.n {
			t.Fatalf("%v: args %q b=%v s=%q wait=%d", tc.args, fs.Args(), *b, *s, *n)
		}
	}
}

func TestUIPrintOnlyPrintsTheAddressAndOpensNothing(t *testing.T) {
	const url = "http://127.0.0.1:7777/#token"
	for _, tc := range []struct {
		printOnly bool
		opened    bool
	}{{true, false}, {false, true}} {
		var out bytes.Buffer
		opened := false
		showUI(&out, url, tc.printOnly, func(u string) { opened = u == url })
		if out.String() != url+"\n" {
			t.Errorf("--print=%v printed %q", tc.printOnly, out.String())
		}
		if opened != tc.opened {
			t.Errorf("--print=%v: opened=%v, want %v", tc.printOnly, opened, tc.opened)
		}
	}
}

func TestUIPrintRefusesJSON(t *testing.T) {
	// Refused before the daemon is asked, so nothing needs to run.
	err := run(context.Background(), "ui", []string{"--print", "--json"})
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "--json") {
		t.Fatalf("a2p2 ui --print --json: %v", err)
	}
}

func TestUIPrintIsAFlagOfUIOnly(t *testing.T) {
	if err := run(context.Background(), "status", []string{"--print"}); err == nil {
		t.Fatal("a2p2 status --print should be refused")
	}
}
