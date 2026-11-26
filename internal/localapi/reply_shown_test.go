package localapi

import (
	"context"
	"errors"
	"testing"

	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// notShown asserts a call is refused as not_shown, a user error.
func notShown(t *testing.T, c *Client, name string, req any) {
	t.Helper()
	err := c.Call(context.Background(), name, req, nil)
	var e *Error
	if !errors.As(err, &e) || !e.User || e.Code != node.ErrorNotShown {
		t.Fatalf("%s %+v: %v, want a %s refusal", name, req, err, node.ErrorNotShown)
	}
}

// Second review of D62: no client passed the version of the text it
// answered, so a reply from the CLI or the MCP reply tool, which calls
// send and then ack, committed against text replaced after the agent read
// it. Through the local API, a reply and an ack to a message replaced
// since the read are refused, with or without reply_version, until a read
// shows the new text.
func TestAReplyAndAnAckThroughTheLocalAPIToTextReplacedSinceTheReadAreRefused(t *testing.T) {
	a, _ := newPair(t)
	alice := call[store.Agent](t, a.c, "register", RegisterReq{Harness: "test", Label: "alice"}).ID
	bob := call[store.Agent](t, a.c, "register", RegisterReq{Harness: "test", Label: "bob"}).ID
	q := call[SendResp](t, a.c, "send", SendReq{Agent: alice, Text: "May I delete the staging database?", To: []string{bob}})
	in := call[InboxResp](t, a.c, "inbox", InboxReq{Agent: bob})
	if len(in.Entries) != 1 || in.Entries[0].ID != q.ID {
		t.Fatalf("inbox %+v", in)
	}
	old := in.Entries[0].Version
	call[SeenResp](t, a.c, "seen", SeenReq{Agent: bob, Entries: []SeenEntry{{Key: in.Entries[0].Key, Version: old}}})
	call[SendResp](t, a.c, "send", SendReq{Agent: alice, Replaces: q.ID, Text: "May I delete the production database?"})

	notShown(t, a.c, "send", SendReq{Agent: bob, Text: "yes", ReplyTo: q.ID})
	notShown(t, a.c, "send", SendReq{Agent: bob, Text: "yes", ReplyTo: q.ID, ReplyVersion: old})
	notShown(t, a.c, "send", SendReq{Agent: bob, Text: "yes", ReplyTo: q.ID, Thread: true}) // the MCP reply tool
	notShown(t, a.c, "ack", AckReq{Agent: bob, ID: q.ID})

	// The read shows the replacement: then both go through.
	in = call[InboxResp](t, a.c, "inbox", InboxReq{Agent: bob})
	if len(in.Entries) != 1 || in.Entries[0].Kind != node.EntryReplaced {
		t.Fatalf("inbox after the replacement %+v", in)
	}
	notShown(t, a.c, "send", SendReq{Agent: bob, Text: "yes", ReplyTo: q.ID, ReplyVersion: old})
	call[SendResp](t, a.c, "send", SendReq{Agent: bob, Text: "no", ReplyTo: q.ID, ReplyVersion: in.Entries[0].Version})
	call[SendResp](t, a.c, "send", SendReq{Agent: bob, Text: "still no", ReplyTo: q.ID})
	call[AckResp](t, a.c, "ack", AckReq{Agent: bob, ID: q.ID})
}

// The honest case: a reply and an ack to text never replaced go through
// without a version, and so does a reply by an agent no read showed the
// message, as before.
func TestAReplyAndAnAckThroughTheLocalAPIToTextShownGoThrough(t *testing.T) {
	a, _ := newPair(t)
	alice := call[store.Agent](t, a.c, "register", RegisterReq{Harness: "test", Label: "alice"}).ID
	bob := call[store.Agent](t, a.c, "register", RegisterReq{Harness: "test", Label: "bob"}).ID
	carol := call[store.Agent](t, a.c, "register", RegisterReq{Harness: "test", Label: "carol"}).ID
	q := call[SendResp](t, a.c, "send", SendReq{Agent: alice, Text: "May I read the logs?", To: []string{bob, carol}})
	in := call[InboxResp](t, a.c, "inbox", InboxReq{Agent: bob})
	call[SendResp](t, a.c, "send", SendReq{Agent: bob, Text: "yes", ReplyTo: q.ID, ReplyVersion: in.Entries[0].Version})
	call[SendResp](t, a.c, "send", SendReq{Agent: bob, Text: "and yes", ReplyTo: q.ID, Thread: true})
	call[AckResp](t, a.c, "ack", AckReq{Agent: bob, ID: q.ID})
	call[SendResp](t, a.c, "send", SendReq{Agent: alice, Replaces: q.ID, Text: "May I read the logs today?"})
	call[SendResp](t, a.c, "send", SendReq{Agent: carol, Text: "sure", ReplyTo: q.ID})
}
