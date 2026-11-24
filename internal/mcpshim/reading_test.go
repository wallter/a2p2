package mcpshim

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The reading round through the tools, across two daemons (D-reading):
// a worker that joins after a kickoff reads it with recent and reports it
// handled; an agent at level none reads what it skipped page by page.

func entryIDs(t *testing.T, s string) []string {
	t.Helper()
	var out []string
	for _, e := range parseEntries(t, s) {
		out = append(out, e.ID)
	}
	return out
}

// [D5, U3, T3] A machine added with history: its worker registered after a
// kickoff that asked for handled reports reads it through recent (marked
// as asking) and reports it handled. Added without history, the worker
// cannot read the kickoff's text and may not report it.
func TestWorkerThatJoinsAfterAKickoffReadsItWithRecentAndReportsItHandled(t *testing.T) {
	if testing.Short() {
		t.Skip("starts daemons")
	}
	for _, history := range []bool{true, false} {
		t.Run(fmt.Sprint("history=", history), func(t *testing.T) {
			var kick string
			_, b := pairDaemonsWith(t, "team", history, func(a *netDaemon) {
				lead := connect(t, a.c, "lead")
				kick = idOf(t, ok(t, lead, "send", map[string]any{"text": "@everyone kickoff", "room": "team", "ask_handled": true}))
			})
			worker := connect(t, b.c, "worker")
			if es := parseEntries(t, ok(t, worker, "inbox", map[string]any{"room": "team"})); len(es) != 0 {
				t.Fatalf("the kickoff reached the inbox of an agent registered after it: %+v", es)
			}
			var recent []string
			until(t, "the room's history to arrive", func() bool {
				s := ok(t, worker, "inbox", map[string]any{"room": "team", "recent": 5})
				recent = entryIDs(t, s)
				return !history || slices.Contains(recent, kick)
			})
			s, isErr := call(t, worker, "ack", map[string]any{"id": kick})
			switch {
			case history && (isErr || !strings.Contains(s, kick)):
				t.Fatalf("ack after recent showed the kickoff: %s", s)
			case history && !strings.Contains(ok(t, worker, "inbox", map[string]any{"room": "team", "recent": 5}), `"ask_handled":true`):
				t.Fatal("recent does not say the kickoff asked for handled reports")
			case !history && (!isErr || !strings.Contains(s, "not_shown")) && !strings.Contains(s, "no message"):
				t.Fatalf("ack of a kickoff never shown: %s", s)
			}
		})
	}
}

// [U1, U3] An agent at level none on a machine added without history
// reads, page by page with before, everything addressed to it that its
// level kept out; recent marks those entries skipped; reading them moves
// nothing into the inbox, and each may then be reported handled.
func TestAgentAtNoneReadsWhatItSkippedPageByPageThroughTheTool(t *testing.T) {
	if testing.Short() {
		t.Skip("starts daemons")
	}
	a, b := pairDaemons(t, "team")
	lead, worker := connect(t, a.c, "lead"), connect(t, b.c, "worker")
	workerID := agentID(t, worker)
	ok(t, worker, "inbox", map[string]any{"room": "team", "level": "none"})
	var sent []string
	for i := 0; i < 5; i++ {
		sent = append(sent, idOf(t, ok(t, lead, "send", map[string]any{"text": fmt.Sprint("task ", i), "room": "team", "to": []string{workerID}})))
	}
	skipped := func(args map[string]any) []string {
		args["room"], args["skipped"] = "team", true
		return entryIDs(t, ok(t, worker, "inbox", args))
	}
	until(t, "the tasks to arrive", func() bool { return len(skipped(map[string]any{"recent": 50})) == len(sent) })
	var got []string
	for before := ""; ; {
		args := map[string]any{"recent": 2}
		if before != "" {
			args["before"] = before
		}
		page := skipped(args)
		if len(page) == 0 {
			break
		}
		got, before = append(slices.Clone(page), got...), page[0]
	}
	if !slices.Equal(got, sent) {
		t.Fatalf("paged %v, want %v", got, sent)
	}
	recent := parseEntries(t, ok(t, worker, "inbox", map[string]any{"room": "team", "recent": 1}))
	if len(recent) != 1 || !recent[0].Skipped {
		t.Fatalf("recent does not mark what the level kept out: %+v", recent)
	}
	if es := parseEntries(t, ok(t, worker, "inbox", map[string]any{"room": "team"})); len(es) != 0 {
		t.Fatalf("reading skipped filled the inbox: %+v", es)
	}
	ok(t, worker, "ack", map[string]any{"id": sent[0]})
}
