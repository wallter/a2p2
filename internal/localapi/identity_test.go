package localapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// Group G (security review 2026-11-25): an entry says whether this
// machine wrote it, always, and writes in full the ids whose short id
// another known machine shares. Status marks such members and peers.
func TestEntriesSayWhetherThisMachineWroteThemAndWriteASharedShortIDInFull(t *testing.T) {
	self := strings.Repeat("1a", 32)
	twin := self[:8] + strings.Repeat("0", 56)
	far := EntryOf(node.Entry{ID: "m1", From: "human@9f9f9f9f", FromNode: strings.Repeat("9f", 32)})
	b, _ := json.Marshal(far)
	if far.FromThisMachine || !strings.Contains(string(b), `"from_this_machine":false`) {
		t.Fatalf("remote entry %s", b)
	}
	near := EntryOf(node.Entry{ID: "m2", From: "claude@" + self, FromNode: self, Here: true, IDShared: true,
		To: []core.Addressee{{Node: self, Agent: "human"}}, ToIDs: []string{"human@" + self}})
	if !near.FromThisMachine || !near.IDShared || len(near.To) != 1 || near.To[0] != "human@"+self {
		t.Fatalf("own entry %+v", near)
	}
	honest := EntryOf(node.Entry{ID: "m3", FromNode: self, Here: true, To: []core.Addressee{{Node: self, Agent: "bob"}}})
	if honest.IDShared || honest.To[0] != "bob@"+self[:8] {
		t.Fatalf("honest entry %+v", honest)
	}
	st := StatusOf(node.StatusView{Node: self, Shared: map[string]bool{self: true, twin: true},
		Rooms:   []node.RoomInfo{{ID: "r_x", Members: []node.MemberInfo{{Node: self}, {Node: twin}, {Node: strings.Repeat("3c", 32)}}}},
		Peers:   []node.PeerView{{Node: twin}},
		Writers: map[string][]node.WriterView{}}, nil, Info{})
	m := st.Rooms[0].Members
	if !st.IDShared || !m[0].IDShared || !m[1].IDShared || m[2].IDShared || !st.Peers[0].IDShared {
		t.Fatalf("status %+v", st)
	}
}

// Two real machine keys with one short id, joined in a room: room/list
// and status mark both on each machine, the person's id is written with
// the full key, a targeted send returns the full id, and the full id is
// accepted back as this machine's agent. The seeds were found by a
// birthday search (node's surface_twin_test.go uses the same pair).
func TestTwoMachinesSharingAShortIDAreMarkedThroughTheLocalAPI(t *testing.T) {
	sa, sb := sha256.Sum256([]byte("rooms test twin-41883")), sha256.Sum256([]byte("rooms test twin-74347"))
	a, b := newPairSeeds(t, sa[:], sb[:])
	if a.id == b.id || a.id[:8] != b.id[:8] {
		t.Fatalf("keys %s %s", a.id, b.id)
	}
	room := call[Room](t, a.c, "room/create", RoomReq{Room: "team"})
	inv := call[InviteResp](t, a.c, "room/invite", InviteReq{Room: room.ID, Role: node.RoleMember})
	pv := call[PreviewResp](t, b.c, "room/join", JoinReq{Token: inv.Token})
	call[AcceptResp](t, b.c, "room/join/accept", AcceptReq{Token: inv.Token, Preview: pv.Preview})
	qs := call[RequestsResp](t, a.c, "requests", Empty{})
	if len(qs.Requests) != 1 {
		t.Fatalf("requests %+v", qs)
	}
	call[ConfirmResp](t, a.c, "confirm", ConfirmReq{ID: qs.Requests[0].ID, Approve: true})
	call[AcceptResp](t, b.c, "room/join/accept", AcceptReq{Token: inv.Token, Preview: pv.Preview})

	for name, m := range map[string]*machine{"admin": a, "joiner": b} {
		rl := call[RoomsResp](t, m.c, "room/list", RoomListReq{Room: room.ID})
		if len(rl.Rooms) != 1 || len(rl.Rooms[0].Members) != 2 {
			t.Fatalf("%s: room/list %+v", name, rl)
		}
		for _, mb := range rl.Rooms[0].Members {
			if !mb.IDShared {
				t.Errorf("%s: room/list member %+v not marked", name, mb)
			}
		}
		st := call[Status](t, m.c, "status", nil)
		if !st.IDShared || st.Human != "human@"+m.id {
			t.Errorf("%s: status id_shared %v human %q", name, st.IDShared, st.Human)
		}
	}
	alice := call[store.Agent](t, a.c, "register", RegisterReq{Harness: "test", Label: "alice", Cwd: "/w/alice"}).ID
	sent := call[SendResp](t, a.c, "send", SendReq{Agent: alice, Room: room.ID, Text: "deploy?", To: []string{"human"}})
	if len(sent.To) != 1 || sent.To[0] != "human@"+a.id {
		t.Fatalf("send to %v, want the person by the full key", sent.To)
	}
	in := call[InboxResp](t, a.c, "inbox", InboxReq{Agent: "human@" + a.id})
	if len(in.Entries) != 1 || !in.Entries[0].FromThisMachine || !in.Entries[0].IDShared || in.Entries[0].From != "alice@"+a.id {
		t.Fatalf("the person's inbox by the full key: %+v", in.Entries)
	}

	// The joiner pulls a message to the room. Its first line in unread
	// is from a key that shares the joiner's short id: not from this
	// machine, and marked.
	bob := call[store.Agent](t, b.c, "register", RegisterReq{Harness: "test", Label: "bob", Cwd: "/w/bob"}).ID
	call[SendResp](t, a.c, "send", SendReq{Agent: alice, Room: room.ID, Text: "all of you"})
	pullFrom(t, b, a, room.ID)
	u := call[Unread](t, b.c, "unread", UnreadReq{Agent: bob, Room: room.ID, Lines: true})
	if len(u.First) != 1 || u.First[0].FromThisMachine || !u.First[0].IDShared || u.First[0].From != "alice@"+a.id {
		t.Fatalf("joiner's unread first lines %+v", u.First)
	}
}

// pullNet hands a pull request to the exchange of the machine it names,
// as from, the way pinned TLS would.
type pullNet struct {
	from  string
	serve map[string]*node.Exchange
}

func (n pullNet) Pull(ctx context.Context, peer string, q *node.PullRequest) (*node.PullResponse, error) {
	return n.serve[peer].Serve(ctx, n.from, q)
}

// pullFrom has to pull room once from the machine from.
func pullFrom(t *testing.T, to, from *machine, room string) {
	t.Helper()
	ex := map[string]*node.Exchange{}
	ex[from.id] = node.NewExchange(from.rooms, pullNet{from: from.id, serve: ex}, node.ExchangeOpts{})
	x := node.NewExchange(to.rooms, pullNet{from: to.id, serve: ex}, node.ExchangeOpts{})
	if _, err := x.Pull(context.Background(), room, from.id, node.PullOpts{}); err != nil {
		t.Fatalf("pull: %v", err)
	}
}
