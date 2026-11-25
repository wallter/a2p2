package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// Tests for the known limits of attachments: each fails without its fix.

// TestAnApprovalOfAnExpiredSendCopiesNoBlob (F3): a send with files is
// approved after its request's life, by the machine's clock, before any
// sweep. It is refused as expired, and nothing was put in the room's blob
// directory: no blob, no part, no temporary file, not the directory
// itself; nothing counts against the budget.
// Requirements: L1, F7
func TestAnApprovalOfAnExpiredSendCopiesNoBlob(t *testing.T) {
	ns := filesNet(t, "a", "b")
	a := ns[0]
	room := fileRoom(t, a, ns[1])
	ctx := context.Background()
	res, err := a.f.RequestSend(ctx, FileSend{Draft: Draft{Room: room, Agent: "alice", Text: "too late"},
		Files: []string{writeSource(t, "x.txt", "never copied"), writeSource(t, "y.txt", "nor this")}})
	if err != nil {
		t.Fatal(err)
	}
	*a.clock = a.clock.Add(core.StagingLife + time.Minute) // no sweep has run
	c, err := a.f.ApproveSend(ctx, res.Request, res.Terms)
	if err != nil || c.State != store.RequestDeclined || c.Reason != store.ReasonSendExpired {
		t.Fatalf("an approval after the request's life: %+v %v", c, err)
	}
	dir := filepath.Join(a.st.FileRoot(), "blobs", room) // its parts live under it too
	if ents, err := os.ReadDir(dir); !errors.Is(err, os.ErrNotExist) {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("the room's blob directory was written for an expired request: %v %v", names, err)
	}
	if left, used := unnamedBlobs(t, a, room); len(left) != 0 || used != 0 {
		t.Fatalf("left behind: blobs %v, usage %d", left, used)
	}
	if len(a.records(room)) != 0 {
		t.Fatal("an expired request published a record")
	}
}
