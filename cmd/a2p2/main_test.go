package main

import (
	"bytes"
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
