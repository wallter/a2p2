package mcpshim

import (
	"encoding/json"
	"testing"

	"github.com/wallter/a2p2/internal/surfacetest"
)

// D65 after the second review, from records stored by another machine to
// what a model reads: the replaced notice of a message that another agent
// of its author's machine edited carries the message's own label, edited
// and the person of another machine, as the thread does; recent marks
// each author from the full key; and text that imitates the marks stays
// in text, never in from_this_machine or person.
func TestWhatAModelReadsCarriesTheMarksOfStoredRecords(t *testing.T) {
	m := surfacetest.NewMarked(t)
	cs := connect(t, m.Here.C, "reader")
	far, here := "human@"+m.Far.ID[:8], "human@"+m.Here.ID[:8]

	type msg struct {
		ID              string `json:"id"`
		Kind            string `json:"kind"`
		Edited          bool   `json:"edited"`
		From            string `json:"from"`
		ChangedBy       string `json:"changed_by"`
		FromThisMachine *bool  `json:"from_this_machine"`
		Person          string `json:"person"`
		Text            string `json:"text"`
	}
	read := func(args map[string]any) []msg {
		t.Helper()
		var r struct {
			Messages []msg `json:"messages"`
		}
		if err := json.Unmarshal([]byte(ok(t, cs, "inbox", args)), &r); err != nil {
			t.Fatal(err)
		}
		for _, x := range r.Messages {
			if x.FromThisMachine == nil {
				t.Fatalf("from_this_machine left out: %+v", x)
			}
		}
		return r.Messages
	}
	in := read(nil)
	by := "bot@" + m.Far.ID[:8]
	if len(in) != 1 || in[0].Kind != "replaced" || in[0].From != far || in[0].ChangedBy != by || !in[0].Edited || *in[0].FromThisMachine || in[0].Person != personOther {
		t.Fatalf("replaced notice %+v", in)
	}
	th := read(map[string]any{"thread": m.FarPerson})
	if len(th) != 1 || th[0].From != far || th[0].ChangedBy != by || !th[0].Edited || th[0].Person != personOther || th[0].Text != in[0].Text {
		t.Fatalf("thread %+v, notice %+v", th, in)
	}
	want := map[string][2]any{
		m.FarPerson:  {false, personOther},
		m.FarBot:     {false, ""},
		m.HereAgent:  {true, ""},
		m.HerePerson: {true, personYours},
	}
	recent := read(map[string]any{"recent": 10, "room": m.Room})
	if len(recent) != 4 {
		t.Fatalf("recent %+v", recent)
	}
	for _, x := range recent {
		w, ok := want[x.ID]
		if !ok || *x.FromThisMachine != w[0] || x.Person != w[1] {
			t.Errorf("recent %+v: want from_this_machine %v person %q", x, w[0], w[1])
		}
		if x.ID == m.HerePerson && x.From != here {
			t.Errorf("this machine's person written as %q", x.From)
		}
	}
}

// D65 after the third review: peek's first lines gave only the message's
// author, so a local agent's text in the person's message read as the
// person's. From stored records to the peek result, each line carries the
// author, the editor with its person field, and edited.
func TestPeekCarriesTheEditorAndEditedOfAReplacement(t *testing.T) {
	type line struct {
		ID              string `json:"id"`
		From            string `json:"from"`
		Edited          bool   `json:"edited"`
		ChangedBy       string `json:"changed_by"`
		ChangedByPerson string `json:"changed_by_person"`
		FromThisMachine bool   `json:"from_this_machine"`
		Person          string `json:"person"`
		Line            string `json:"line"`
	}
	for _, tc := range []struct {
		name                  string
		c                     surfacetest.Change
		person, changedPerson string
		changed, here         bool
	}{
		{"the person's message replaced by a local agent", surfacetest.PersonByAgent, personYours, "", true, true},
		{"an agent's message replaced by the person", surfacetest.AgentByPerson, "", personYours, true, true},
		{"a remote author's message replaced by its author", surfacetest.RemoteByAuthor, "", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := surfacetest.NewChanged(t, tc.c)
			by := ""
			if tc.changed {
				by = x.Editor
			}
			for _, reader := range []string{"fresh", "old"} {
				cs := connect(t, x.Here.C, reader)
				var r struct {
					First []line `json:"first_lines"`
				}
				if err := json.Unmarshal([]byte(ok(t, cs, "inbox", map[string]any{"peek": true})), &r); err != nil {
					t.Fatal(err)
				}
				text := surfacetest.ChangedText
				if reader == "old" {
					text = "(replaced)"
				}
				want := line{ID: x.ID, From: x.Author, Edited: true, ChangedBy: by, ChangedByPerson: tc.changedPerson,
					FromThisMachine: tc.here, Person: tc.person, Line: text}
				if len(r.First) != 1 || r.First[0] != want {
					t.Fatalf("%s: peek lines %+v\nwant %+v", reader, r.First, want)
				}
			}
		})
	}
}
