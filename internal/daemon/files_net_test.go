package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// These run real daemons over pinned mutual TLS on loopback, with real
// pull holds and the daemon's own file tick (a minute), for the three
// rows of DESIGN 12 where a machine only dials out. The in-memory network
// drives pulls and ticks by hand, so it cannot see that a list or a want
// waits for the next tick or for a held pull to time out; these give each
// step a deadline well under a minute.

// fileDeadline is how long a list or a file may take to arrive. The file
// tick is a minute and a held pull lasts up to a minute, so code that
// waits for either misses it.
const fileDeadline = 25 * time.Second

// newOutbound starts a machine that only dials out from its first start.
func newOutbound(t *testing.T, name string) *machine {
	m := newMachineWith(t, name, nil)
	m.setNet(node.NetworkOutbound)
	return m
}

// sendFile has agent on m send text to room with one file holding data,
// approved with the terms the person was shown, and returns the message
// id and the file's sha256.
func sendFile(t *testing.T, m *machine, agent, room, name, data string) (string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	w := call[localapi.SendFilesResp](m, "send", localapi.SendReq{Agent: agent, Room: room, Text: "numbers attached", Files: []string{p}})
	for _, q := range call[localapi.RequestsResp](m, "requests", localapi.Empty{}).Requests {
		if q.ID == w.Request {
			c := call[localapi.ConfirmResp](m, "confirm", localapi.ConfirmReq{ID: q.ID, Approve: true, Terms: q.Terms})
			if c.State != store.RequestApproved || c.Effect == "" {
				t.Fatalf("confirm = %+v", c)
			}
			return c.Effect, core.SHA256Hex([]byte(data))
		}
	}
	t.Fatalf("no request %s", w.Request)
	return "", ""
}

// within retries cond until it holds or d passes.
func within(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %s", what, d)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// listArrives waits for reader's history to show msg with its one file
// listed, then has agent (shown it by that view) fetch it, waiting as the
// CLI does, and checks the copy holds exactly data.
func listArrives(t *testing.T, reader *machine, agent, room, msg, sha, data string) {
	t.Helper()
	start := time.Now()
	within(t, fileDeadline, "the file list on "+reader.node[:8], func() bool {
		h := call[localapi.Entries](reader, "history", localapi.HistoryReq{Room: room, Agent: agent})
		i := slices.IndexFunc(h.Entries, func(e localapi.Entry) bool { return e.ID == msg })
		return i >= 0 && !h.Entries[i].FilesPending && len(h.Entries[i].Files) == 1 && h.Entries[i].Files[0].SHA256 == sha
	})
	left := fileDeadline - time.Since(start)
	fr := call[localapi.FetchResp](reader, "fetch", localapi.FetchReq{Agent: agent, Room: room, SHA256: sha, WaitMS: left.Milliseconds()})
	if fr.State != node.FetchHere {
		t.Fatalf("fetch after %s = %+v", time.Since(start), fr)
	}
	got, err := os.ReadFile(fr.Path)
	if err != nil || string(got) != data {
		t.Fatalf("the copy at %s holds %q, %v", fr.Path, got, err)
	}
}

// Row "author only dials out, reader listens": the reader cannot dial the
// author, so the list and then the file travel as wants in its answers to
// the author's pulls and come back as uploads.
//
// Requirements: L1, N1
func TestAListenerGetsTheListAndTheFileFromAnAuthorThatOnlyDialsOut(t *testing.T) {
	if testing.Short() {
		t.Skip("starts daemons")
	}
	a, b := newMachine(t, "alpha"), newOutbound(t, "beta")
	room := call[localapi.Room](a, "room/create", localapi.RoomReq{Room: "team"}).ID
	ann := a.agent("ann")
	join(t, a, b, room, localapi.InviteReq{}, false)
	bea := b.agent("bea")
	data := "q2,revenue\nnorth,12\nsouth,7\n"
	msg, sha := sendFile(t, b, bea, room, "report.csv", data)
	listArrives(t, a, ann, room, msg, sha, data)
}

// Row "author listens, reader only dials out": the reader fetches the list
// and the file from the author after the pull that brought the header.
//
// Requirements: L1, N1
func TestAReaderThatOnlyDialsOutGetsTheListAndTheFileFromAListeningAuthor(t *testing.T) {
	if testing.Short() {
		t.Skip("starts daemons")
	}
	a, b := newMachine(t, "alpha"), newOutbound(t, "beta")
	room := call[localapi.Room](a, "room/create", localapi.RoomReq{Room: "team"}).ID
	ann := a.agent("ann")
	join(t, a, b, room, localapi.InviteReq{}, false)
	bea := b.agent("bea")
	data := "from the machine that listens\n"
	msg, sha := sendFile(t, a, ann, room, "notes.txt", data)
	listArrives(t, b, bea, room, msg, sha, data)
}

// Row "author and reader both only dial out, one member listens": the
// listener gets the list as a reader itself and carries the file; the
// reader gets both from it.
//
// Requirements: L1, N1
func TestTwoMachinesThatOnlyDialOutShareAFileThroughTheMemberThatListensAndCarries(t *testing.T) {
	if testing.Short() {
		t.Skip("starts daemons")
	}
	a, b, c := newMachine(t, "alpha"), newOutbound(t, "beta"), newOutbound(t, "gamma")
	room := call[localapi.Room](a, "room/create", localapi.RoomReq{Room: "team"}).ID
	join(t, a, b, room, localapi.InviteReq{}, false)
	join(t, a, c, room, localapi.InviteReq{}, false)
	bea, cy := b.agent("bea"), c.agent("cy")
	eventually(t, "the outbound machines to know each other as members", func() bool {
		var rl localapi.RoomsResp
		return b.c.Call(t.Context(), "room/list", localapi.RoomListReq{Room: room}, &rl) == nil && len(rl.Rooms) == 1 && len(rl.Rooms[0].Members) == 3
	})
	data := "behind two walls, through the one that listens\n"
	msg, sha := sendFile(t, b, bea, room, "wall.txt", data)
	listArrives(t, c, cy, room, msg, sha, data)
}
