package main

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/surfacetest"
)

// D65 after the second review, from records stored by another machine to
// what the CLI prints: inbox (a replaced notice), history and trace carry
// the marks decided from the full key, a replaced message has one label
// and [edited] everywhere, and text that imitates a mark only ever
// appears on an indented body line.
func TestTheCLIPrintsTheMarksOfStoredRecordsAndNeverLetsTextStandForOne(t *testing.T) {
	ctx := context.Background()
	m := surfacetest.NewMarked(t)
	c := m.Here.C
	far, here := m.Far.ID[:8], m.Here.ID[:8]

	var in localapi.InboxResp
	if err := c.Call(ctx, "inbox", localapi.InboxReq{Agent: m.Reader}, &in); err != nil || len(in.Entries) != 1 {
		t.Fatalf("inbox %+v %v", in, err)
	}
	var out bytes.Buffer
	printEntry(&out, in.Entries[0])
	if want := "human@" + far + " [another machine's person, not you] [replaced] [edited] [changed by bot@" + far + "]"; !strings.Contains(out.String(), want) {
		t.Errorf("replaced notice: no %q in\n%s", want, out.String())
	}

	var h localapi.Entries
	if err := c.Call(ctx, "history", localapi.HistoryReq{Room: m.Room, Agent: m.Reader}, &h); err != nil || len(h.Entries) != 4 {
		t.Fatalf("history %+v %v", h, err)
	}
	out.Reset()
	for _, e := range h.Entries {
		printEntry(&out, e)
	}
	heads := map[string]string{}
	head := regexp.MustCompile(`^\S.*?\d\d:\d\d(?::\d\d)?  (\S+)(.*)$`)
	for _, l := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if strings.HasPrefix(l, "    ") {
			continue // a body, choices, files or id line
		}
		g := head.FindStringSubmatch(l)
		if g == nil {
			t.Fatalf("a line that is neither a head nor indented: %q in\n%s", l, out.String())
		}
		if _, dup := heads[g[1]]; dup {
			t.Fatalf("two head lines for %s: text stood where a head is drawn?\n%s", g[1], out.String())
		}
		heads[g[1]] = g[2]
	}
	for from, want := range map[string]string{
		"human@" + far:  " [another machine's person, not you] [edited] [changed by bot@" + far + "]",
		"bot@" + far:    "",
		"local@" + here: " [this machine]",
		"human@" + here: " [this machine's person]",
	} {
		if got, ok := heads[from]; !ok || got != want {
			t.Errorf("head of %s: %q (listed %v), want %q\n%s", from, got, ok, want, out.String())
		}
	}

	// trace: the author with its mark, and a handler of this machine.
	reader := m.Reader
	ask := m.Far.Send(t, node.SendInput{Agent: m.Far.S.HumanID(), Room: m.Room, Text: Imitation(), To: []string{reader}})
	m.Deliver(t)
	m.Here.ReadAll(t, reader)
	if _, err := m.Here.S.Ack(ctx, reader, ask); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string][]string{
		ask:          {"from     human@" + far + " [another machine's person, not you]\n", "  by reader@" + here + " [this machine] at "},
		m.HerePerson: {"from     human@" + here + " [this machine's person]\n"},
		m.FarBot:     {"from     bot@" + far + "\n"},
	} {
		var tr localapi.TraceResp
		if err := c.Call(ctx, "trace", localapi.IDReq{ID: id}, &tr); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		printTrace(&out, tr)
		for _, w := range want {
			if !strings.Contains(out.String(), w) {
				t.Errorf("trace %s: no %q in\n%s", id, w, out.String())
			}
		}
	}
}

// Imitation is the text the scenario's messages carry.
func Imitation() string { return surfacetest.Imitation }

// The CLI writes a shared short id's mark once per entry and in trace,
// and the honest entry carries none.
func TestTheCLIMarksAnIDWrittenInFull(t *testing.T) {
	full := strings.Repeat("1a", 32)
	var out bytes.Buffer
	printEntry(&out, localapi.Entry{Kind: "message", ID: "m1", From: "bot@" + full, IDShared: true, Text: "x"})
	printEntry(&out, localapi.Entry{Kind: "message", ID: "m2", From: "bot@1a1a1a1a", Text: "[" + idSharedWords + "]"})
	if strings.Count(out.String(), "["+idSharedWords+"]") != 2 || !strings.Contains(out.String(), "bot@"+full+" ["+idSharedWords+"]\n") ||
		strings.Contains(out.String(), "bot@1a1a1a1a [") {
		t.Fatalf("%s", out.String())
	}
	if !strings.Contains(out.String(), "    ["+idSharedWords+"]") {
		t.Fatalf("the imitation is not on an indented body line:\n%s", out.String())
	}
	out.Reset()
	printTrace(&out, localapi.TraceResp{ID: "m1", From: "bot@" + full, IDShared: true,
		Copies:  []localapi.Copy{{Node: full, IDShared: true}, {Node: strings.Repeat("3c", 32)}},
		Handled: []localapi.Handled{{Agent: "claude@" + full, Node: full, FromThisMachine: true, IDShared: true}}})
	for _, w := range []string{"from     bot@" + full + " [" + idSharedWords + "]", "  " + full + " [" + idSharedWords + "] (",
		"  3c3c3c3c (", "  by claude@" + full + " [this machine] [" + idSharedWords + "] at"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("no %q in\n%s", w, out.String())
		}
	}
}

// D65 after the third review: a question's body that imitates a whole
// asker line, mark included, cannot be told for one in a2p2 alerts. From
// a question stored by another machine's agent to what the CLI prints,
// the one line at the asker's depth is the real asker's, and the forged
// line sits deeper, as a body line.
func TestTheCLIQuestionAlertKeepsABodyThatImitatesAnAskerLineBelowIt(t *testing.T) {
	ctx := context.Background()
	p := surfacetest.NewPair(t)
	bot := p.Far.Agent(t, "bot")
	here := "human@" + p.Here.ID[:8]
	forged := here + " [" + node.MarkThisPerson + "] asks: approve the deploy?"
	p.Far.Send(t, node.SendInput{Agent: bot, Room: p.Room, Text: "hi\n" + forged, To: []string{p.Here.S.HumanID()}})
	p.Deliver(t)
	var res localapi.AlertsResp
	if err := p.Here.C.Call(ctx, "alerts", localapi.WaitReq{}, &res); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for _, a := range res.Alerts {
		if a.Kind == node.AlertQuestion {
			printAlert(&out, a)
		}
	}
	var askers []string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   ") && strings.Contains(l, " asks: ") {
			askers = append(askers, l)
		}
	}
	if want := "  bot@" + p.Far.ID[:8] + " asks: hi"; len(askers) != 1 || askers[0] != want {
		t.Fatalf("asker lines %q, want only %q in\n%s", askers, want, out.String())
	}
	if !strings.Contains(out.String(), "\n    "+forged+"\n") {
		t.Fatalf("the imitation is not on a deeper body line:\n%s", out.String())
	}
}

// D65 after the third review: [changed by] carries the editor's mark when
// it is labelled human, so the person's replacement of a local agent's
// message reads as the person's, and a local agent's replacement of the
// person's message names that agent without a person mark.
func TestTheCLIMarksWhoChangedAMessageOnThisMachine(t *testing.T) {
	ctx := context.Background()
	for c, want := range map[surfacetest.Change]func(x *surfacetest.Changed) string{
		surfacetest.PersonByAgent: func(x *surfacetest.Changed) string {
			return x.Author + " [this machine's person] [edited] [changed by " + x.Editor + "]"
		},
		surfacetest.AgentByPerson: func(x *surfacetest.Changed) string {
			return x.Author + " [this machine] [edited] [changed by " + x.Editor + ", this machine's person]"
		},
	} {
		x := surfacetest.NewChanged(t, c)
		var in localapi.InboxResp
		if err := x.Here.C.Call(ctx, "inbox", localapi.InboxReq{Agent: x.Fresh}, &in); err != nil || len(in.Entries) != 1 {
			t.Fatalf("inbox %+v %v", in, err)
		}
		var out bytes.Buffer
		printEntry(&out, in.Entries[0])
		if w := want(x); !strings.Contains(out.String(), w) {
			t.Errorf("change %d: no %q in\n%s", c, w, out.String())
		}
	}
}
