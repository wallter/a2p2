package web

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wallter/a2p2/internal/node"
)

// L2 (fix-files-a4): a reply sent explicitly to everyone stays a
// broadcast when it is replaced with a file. The page is given everyone
// and the member count, the request it approves binds them, and nothing
// names the parent's author as the recipient.
func TestThePageShowsTheReplacementOfABroadcastReplyAsABroadcast(t *testing.T) {
	e, alice := setupFiles(t)
	bob := e.agent(t, "bob")
	parent, err := e.s.Send(e.ctx, node.SendInput{Agent: bob, Room: "team", Text: "a question"})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := e.s.Send(e.ctx, node.SendInput{Agent: alice, ReplyTo: parent.ID, To: []string{node.Everyone}, Text: "for all"})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SendFiles(e.ctx, node.SendInput{Agent: alice, Replaces: reply.ID, Text: "for all, with a file"}, []string{p}); err != nil {
		t.Fatal(err)
	}
	st := e.state(t)
	if len(st.Requests) != 1 {
		t.Fatalf("requests: %+v", st.Requests)
	}
	q := st.Requests[0]
	if len(q.To) != 1 || q.To[0] != node.Everyone || q.Members != 1 || q.ReplyAuthor != "" {
		t.Fatalf("the replacement of a broadcast reply is shown as %+v", q)
	}
	e.approve(t, q)
}
