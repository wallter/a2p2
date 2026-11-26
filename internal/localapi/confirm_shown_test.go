package localapi

import (
	"context"
	"errors"
	"testing"

	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// E4 of the second security review (2026-11-25): confirm with the text a
// person was shown (a2p2 confirm sends it) decides nothing when the
// request no longer reads that way, whether it approves or declines, as
// the page's confirm; with the text as it is, it decides. A confirm
// without text is decided by id, as before.
func TestConfirmWithTheTextShownRefusesARequestWhoseTextChanged(t *testing.T) {
	ctx := context.Background()
	a, b := newPair(t)
	room := call[Room](t, a.c, "room/create", RoomReq{Room: "team"})
	inv := call[InviteResp](t, a.c, "room/invite", InviteReq{Room: room.ID, Role: node.RoleMember})
	pv := call[PreviewResp](t, b.c, "room/join", JoinReq{Token: inv.Token})
	call[AcceptResp](t, b.c, "room/join/accept", AcceptReq{Token: inv.Token, Preview: pv.Preview})
	qs := call[RequestsResp](t, a.c, "requests", Empty{})
	if len(qs.Requests) != 1 {
		t.Fatalf("requests %+v", qs)
	}
	q := qs.Requests[0]
	stale := q.Text + " (as shown before a pull)"
	for _, approve := range []bool{true, false} {
		err := a.c.Call(ctx, "confirm", ConfirmReq{ID: q.ID, Approve: approve, Text: stale}, nil)
		var e *Error
		if !errors.As(err, &e) || !e.User || e.Msg != node.RequestChangedText {
			t.Fatalf("approve %v with a text not shown: %v", approve, err)
		}
		if got, err := a.st.GetRequest(ctx, q.ID); err != nil || got.State != store.RequestPending {
			t.Fatalf("decided anyway: %+v %v", got, err)
		}
	}
	if cf := call[ConfirmResp](t, a.c, "confirm", ConfirmReq{ID: q.ID, Approve: true, Text: q.Text}); cf.State != store.RequestApproved {
		t.Fatalf("approve with the text shown: %+v", cf)
	}
}
