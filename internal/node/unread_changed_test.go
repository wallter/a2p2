package node_test

// D65 after the third review: unread (the hook's notice and MCP peek)
// named every entry's sender by the message's author and dropped
// ChangedBy and edited, so a local agent's replacement of the person's
// message was summed up as the person's. From stored records to
// Surface.Unread, the sender named is the writer of what is shown, and
// each first line carries the author, the editor and edited.

import (
	"context"
	"slices"
	"testing"

	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/surfacetest"
)

func TestUnreadNamesTheWriterOfAReplacementAndMarksIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    surfacetest.Change
	}{
		{"the person's message replaced by a local agent", surfacetest.PersonByAgent},
		{"an agent's message replaced by the person", surfacetest.AgentByPerson},
		{"a remote author's message replaced by its author", surfacetest.RemoteByAuthor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := surfacetest.NewChanged(t, tc.c)
			here := tc.c != surfacetest.RemoteByAuthor
			changedBy := x.Editor
			if x.Editor == x.Author {
				changedBy = ""
			}
			for _, reader := range []string{x.Fresh, x.Old} {
				u, err := x.Here.S.Unread(context.Background(), reader, "", true)
				if err != nil {
					t.Fatal(err)
				}
				if u.Count != 1 || !slices.Equal(u.From, []string{x.Editor}) {
					t.Fatalf("%s: count %d, from %v, want only the writer %s", reader, u.Count, u.From, x.Editor)
				}
				if wantHere := []string(nil); here {
					if !slices.Equal(u.FromHere, []string{x.Editor}) {
						t.Fatalf("%s: from here %v", reader, u.FromHere)
					}
				} else if !slices.Equal(u.FromHere, wantHere) {
					t.Fatalf("%s: from here %v", reader, u.FromHere)
				}
				if len(u.First) != 1 {
					t.Fatalf("%s: first lines %+v", reader, u.First)
				}
				f := u.First[0]
				line := surfacetest.ChangedText
				if reader == x.Old {
					line = "(" + node.EntryReplaced + ")"
				}
				if f.ID != x.ID || f.From != x.Author || f.ChangedBy != changedBy || !f.Edited || f.Here != here || f.Line != line {
					t.Fatalf("%s: first line %+v, want from %s changed by %q edited, line %q", reader, f, x.Author, changedBy, line)
				}
			}
		})
	}
}

// A question alert names the asker with its mark and, when another agent
// of its machine wrote the text shown, that agent with its own mark: an
// agent's question replaced by the person on this machine says the person
// changed it.
func TestAQuestionAlertNamesTheEditorWithItsMark(t *testing.T) {
	ctx := context.Background()
	p := surfacetest.NewPair(t)
	local, human := p.Here.Agent(t, "local"), p.Here.S.HumanID()
	id := p.Here.Send(t, node.SendInput{Agent: local, Room: p.Room, To: []string{human}, Text: "ship it?"})
	if _, err := p.Here.S.Send(ctx, node.SendInput{Agent: human, Replaces: id, Text: "pick one"}); err != nil {
		t.Fatal(err)
	}
	as, err := p.Here.S.Alerts(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := local + " [" + node.MarkThisMachine + "] [edited] [changed by " + human + ", " + node.MarkThisPerson + "] asks: pick one"
	for _, a := range as {
		if a.Kind == node.AlertQuestion {
			if a.Text != want {
				t.Fatalf("question alert %q\nwant %q", a.Text, want)
			}
			return
		}
	}
	t.Fatalf("no question alert in %+v", as)
}

// The person's questions leave out the person's own messages: the
// person's inbox never holds them, so a local agent's replacement of one,
// addressed to the person, is not offered to the person as a question.
func TestALocalAgentsReplacementOfThePersonsMessageIsNotAQuestion(t *testing.T) {
	ctx := context.Background()
	p := surfacetest.NewPair(t)
	local, human := p.Here.Agent(t, "local"), p.Here.S.HumanID()
	id := p.Here.Send(t, node.SendInput{Agent: human, Room: p.Room, To: []string{human}, Text: "note to self"})
	if _, err := p.Here.S.Send(ctx, node.SendInput{Agent: local, Replaces: id, Text: "approve?"}); err != nil {
		t.Fatal(err)
	}
	if qs, err := p.Here.S.Questions(ctx); err != nil || len(qs) != 0 {
		t.Fatalf("questions %+v %v", qs, err)
	}
}
