package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/node"
)

// L2 (fix-files-a4): confirm shows the replacement of a reply sent
// explicitly to everyone as a send to everyone in the room with its
// member count, never to the parent's author, and approves those terms.
func TestConfirmShowsTheReplacementOfABroadcastReplyAsABroadcast(t *testing.T) {
	d := newDaemonForCLI(t)
	ctx := context.Background()
	alice, err := d.s.Register(ctx, "test", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := d.s.Register(ctx, "test", "bob", "")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := d.s.Send(ctx, node.SendInput{Agent: bob.ID, Room: "team", Text: "a question"})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := d.s.Send(ctx, node.SendInput{Agent: alice.ID, ReplyTo: parent.ID, To: []string{node.Everyone}, Text: "for all"})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	q, err := d.s.SendFiles(ctx, node.SendInput{Agent: alice.ID, Replaces: reply.ID, Text: "for all, with a file"}, []string{p})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := d.run(t, false, "", "confirm")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "to        everyone in the room (1 members)") || strings.Contains(out, "the author of message") {
		t.Fatalf("the replacement of a broadcast reply is shown as:\n%s", out)
	}
	if _, _, err := d.run(t, true, "yes\n", "confirm", q.Request); err != nil {
		t.Fatalf("approve with the terms shown: %v", err)
	}
}
