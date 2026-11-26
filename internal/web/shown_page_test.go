package web

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// Tests of the second review of D62 on the page: an answer and a handled
// report are tied to the question text the page drew, and the page's
// background state requests keep no receipt of text it may not draw.

type questionsState struct {
	Questions []localapi.Entry `json:"questions"`
}

func (e *env) questions(t *testing.T) []localapi.Entry {
	t.Helper()
	c, b := e.do(t, "GET", "/api/state", nil, nil)
	var st questionsState
	if c != 200 || json.Unmarshal(b, &st) != nil {
		t.Fatalf("state: %d %s", c, b)
	}
	return st.Questions
}

func (e *env) seenOK(t *testing.T, q localapi.Entry) bool {
	t.Helper()
	c, b := e.do(t, "POST", "/api/seen", map[string]any{"entries": []map[string]string{{"key": q.Key, "version": q.Version}}}, nil)
	if c != 200 {
		t.Fatalf("seen: %d %s", c, b)
	}
	return strings.Contains(string(b), `"ok":true`)
}

// The page drew a question, then its author replaced it. A reply naming
// the text drawn, and a handled report after the state request every
// change makes first (which used to keep a receipt of the new text the
// page had not drawn), went through as answers to text the person never
// saw. Both are refused until the page draws the new text.
func TestThePageCannotAnswerOrReportHandledAQuestionReplacedAfterItWasDrawn(t *testing.T) {
	e := setup(t)
	alice := e.agent(t, "alice")
	q, err := e.s.Send(e.ctx, node.SendInput{Agent: alice, Text: "May I delete the staging database?", To: []string{e.s.HumanID()}})
	if err != nil {
		t.Fatal(err)
	}
	qs := e.questions(t)
	if len(qs) != 1 || qs[0].ID != q.ID {
		t.Fatalf("questions %+v", qs)
	}
	drawn := qs[0]
	if !e.seenOK(t, drawn) {
		t.Fatal("seen of the question drawn was refused")
	}
	if _, err := e.s.Send(e.ctx, node.SendInput{Agent: alice, Replaces: q.ID, Text: "May I delete the production database?"}); err != nil {
		t.Fatal(err)
	}
	e.questions(t) // the page's state request before a change; nothing redrawn
	if c, b := e.do(t, "POST", "/api/send", map[string]any{"text": "yes", "reply_to": q.ID, "reply_version": drawn.Version}, nil); c != 400 ||
		!strings.Contains(string(b), "new text since you were shown it") {
		t.Fatalf("reply to the text drawn before the replacement: %d %s", c, b)
	}
	if c, b := e.do(t, "POST", "/api/ack", map[string]any{"id": q.ID, "version": drawn.Version}, nil); c != 400 ||
		!strings.Contains(string(b), "new text since you were shown it") {
		t.Fatalf("handled report of the text drawn before the replacement: %d %s", c, b)
	}
	// The page draws the new text: then both go through.
	qs = e.questions(t)
	if len(qs) != 1 || qs[0].Version == drawn.Version {
		t.Fatalf("questions after the replacement %+v", qs)
	}
	if !e.seenOK(t, qs[0]) {
		t.Fatal("seen of the new text drawn was refused")
	}
	if c, b := e.do(t, "POST", "/api/send", map[string]any{"text": "no", "reply_to": q.ID, "reply_version": qs[0].Version}, nil); c != 200 {
		t.Fatalf("reply to the new text drawn: %d %s", c, b)
	}
	if c, b := e.do(t, "POST", "/api/ack", map[string]any{"id": q.ID, "version": qs[0].Version}, nil); c != 200 {
		t.Fatalf("handled report of the new text drawn: %d %s", c, b)
	}
}

// The page's state request, which it makes in the background and before
// each change, kept a receipt of every unread entry and question it
// returned, so a handled report by the person from another surface (the
// CLI's ack) went through for text the page never drew. Only a seen of
// what the page drew keeps a receipt now.
func TestThePagesStateRequestKeepsNoReceiptOfWhatItReturns(t *testing.T) {
	e := setup(t)
	alice := e.agent(t, "alice")
	q, err := e.s.Send(e.ctx, node.SendInput{Agent: alice, Text: "deploy?", To: []string{e.s.HumanID()}})
	if err != nil {
		t.Fatal(err)
	}
	qs := e.questions(t)
	if len(qs) != 1 {
		t.Fatalf("questions %+v", qs)
	}
	if _, err := e.s.Ack(e.ctx, e.s.HumanID(), q.ID); err == nil || err.Error() != node.ErrAckNotShown.Error() {
		t.Fatalf("ack after only the page's state request: %v, want %v", err, node.ErrAckNotShown)
	}
	// A seen of an entry the inbox does not list at that version keeps
	// nothing either.
	stale := qs[0]
	stale.Version = "0000"
	if e.seenOK(t, stale) {
		t.Fatal("seen of a version never listed was confirmed")
	}
	if _, err := e.s.Ack(e.ctx, e.s.HumanID(), q.ID); err == nil || err.Error() != node.ErrAckNotShown.Error() {
		t.Fatalf("ack after a seen of another version: %v, want %v", err, node.ErrAckNotShown)
	}
	// The honest case: the page draws it and confirms it; the report goes
	// through, from the page with the version drawn or from the CLI.
	if !e.seenOK(t, qs[0]) {
		t.Fatal("seen of the question drawn was refused")
	}
	if c, b := e.do(t, "POST", "/api/ack", map[string]any{"id": q.ID, "version": qs[0].Version}, nil); c != 200 {
		t.Fatalf("handled report of the text drawn: %d %s", c, b)
	}
}

// The page sends the version it drew with each answer and report: the
// question card's reply and dismissal, and the stream's reply.
func TestThePageSendsTheVersionItDrewWithEachAnswerAndReport(t *testing.T) {
	src, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, use := range []string{
		`await api("/api/ack", { id: m.id, version: m.version });`,
		`await api("/api/send", { room: m.room, text, reply_to: m.id, reply_version: m.version });`,
		`body.reply_to = S.reply.id; body.reply_version = S.reply.version || "";`,
	} {
		if !regexp.MustCompile(regexp.QuoteMeta(use)).Match(src) {
			t.Errorf("app.js no longer sends the version drawn with %q", use)
		}
	}
	if n := len(regexp.MustCompile(`api\("/api/ack"`).FindAll(src, -1)); n != 1 {
		t.Errorf("app.js calls /api/ack %d times, want once (with the version)", n)
	}
}
