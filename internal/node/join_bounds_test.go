package node

// Group D after the second review of 2026-11-26: the bounds of one join's
// signed answer, the heads a join counts as caught up, and the label a
// room labelled local elsewhere is shown under for the life of the room.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// createWire is room's create entry as a holds it.
func createWire(a *lnode, room string) json.RawMessage {
	a.r.mu.Lock()
	defer a.r.mu.Unlock()
	rl := a.r.rooms[room]
	return json.RawMessage(rl.ents[rl.createID].Wire())
}

// lower sets *v to x until the test ends.
func lower[T any](t *testing.T, v *T, x T) {
	old := *v
	*v = x
	t.Cleanup(func() { *v = old })
}

// countHook counts the calls of hook point p until the test ends.
func countHook(t *testing.T, p string) func() int {
	var mu sync.Mutex
	n := 0
	testHook = func(at string) error {
		if at == p {
			mu.Lock()
			n++
			mu.Unlock()
		}
		return nil
	}
	t.Cleanup(func() { testHook = nil })
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// GPT must-fix 3a. An admin machine that pages its answer without end,
// each page marked more and moving on but holding only an entry held here
// already, is read only within the bounds of one join: pages, bytes, the
// time paging takes, and the time without an entry this machine lacks.
// The join ends refused with ReasonJoinTooLong and nothing stays held.
// pagingMost is how many signed pages each bound lets the hostile answer
// of these tests send: tight enough that each bound alone is tested, since
// the next bound would let more through (the 15 minutes about 16 pages,
// the hour without moving on about 61).
var pagingMost = map[string]int{"pages": 21, "bytes": 32, "stall": 6, "time": 7}

func TestJoinAnswerThatPagesWithoutEndIsRefusedWithinTheBoundsOfOneJoin(t *testing.T) {
	for _, mode := range []string{"pages", "bytes", "stall", "time"} {
		t.Run(mode, func(t *testing.T) {
			clock := lclock()
			a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
			jn := newJoinNet()
			jn.serve(a)
			room := a.create("team")
			create := createWire(a, room)
			switch mode {
			case "pages":
				lower(t, &maxJoinPages, 20)
			case "bytes":
				lower(t, &maxJoinBytes, 16<<10)
			case "time":
				lower(t, &joinStallMax, time.Hour)
				lower(t, &joinPagingMax, 5*time.Minute)
			}
			pages, next := 0, int64(0)
			link := newRewriteLink(jn, b, func(an *JoinAnswer) {
				if an.State != JoinStateSigned {
					return
				}
				if pages++; pages > 300 {
					return // a joiner that does not stop gets an honest end
				}
				if mode == "stall" || mode == "time" {
					*clock = clock.Add(time.Minute)
				}
				if pages == 1 {
					an.More, an.Heads, next = true, nil, an.Next
					return
				}
				next++
				an.Entries, an.More, an.Next, an.Heads = []json.RawMessage{create}, true, next, nil
			})
			j := b.linkJoiner(link)
			tok := a.invite(room, RoleMember, false)
			b.preview(j, tok)
			b.accept(j, tok, 0)
			a.confirm(a.onlyRequest(room).ID)
			wantOutcome(t, b.resume(j, time.Minute), JoinStateRefused, ReasonJoinTooLong)
			if pages > pagingMost[mode] {
				t.Fatalf("%d signed pages read", pages)
			}
			if _, held, _ := b.st.RoomGen(context.Background(), room); held {
				t.Fatal("the room is held after a refused join")
			}
		})
	}
}

// GPT must-fix 3a, the honest case: a room whose log takes several pages,
// each a minute apart, each bringing entries this machine lacks, joins
// and catches up.
func TestJoinOfALongLogOverASlowLinkIsSignedWithinTheBounds(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	signChainAfter(t, a, room, 700)
	pages := 0
	link := newRewriteLink(jn, b, func(an *JoinAnswer) {
		if an.State == JoinStateSigned {
			pages++
			*clock = clock.Add(time.Minute + 30*time.Second)
		}
	})
	j := b.linkJoiner(link)
	tok := a.invite(room, RoleMember, false)
	b.preview(j, tok)
	b.accept(j, tok, 0)
	a.confirm(a.onlyRequest(room).ID)
	wantOutcome(t, b.resume(j, time.Minute), JoinStateSigned, "")
	if pages < 3 {
		t.Fatalf("%d signed pages: the log should take three", pages)
	}
	sameState(t, "b joined", a.state(room), b.state(room))
	if !b.pulled(room) {
		t.Fatal("the join did not catch up")
	}
}

// heldRoomThroughRemovedAdmin is the setting of the review's join denial
// of service: v holds room, and x, an admin removed there that has not
// heard of it, invites v. x's chain of entries v lacks is returned.
func heldRoomThroughRemovedAdmin(t *testing.T) (a, v, x *lnode, jn *joinNet, room string, chain [][]byte) {
	clock := lclock()
	a, b, x, v := newLNode(t, "a", clock), newLNode(t, "b", clock), newLNode(t, "x", clock), newLNode(t, "v", clock)
	jn = newJoinNet()
	jn.serve(a)
	jn.serve(x)
	room = a.create("team")
	joinRoom(t, jn, a, b, room, "", false)
	joinRoom(t, jn, a, x, room, "", false)
	b.pull(room, a)
	b.confirm(b.onlyRequest(room).ID)
	a.pull(room, b)
	joinRoom(t, jn, a, v, room, RoleMember, false)
	x.pull(room, a)
	a.change(room, Change{Kind: core.EntryRemove, Target: x.id})
	b.pull(room, a)
	b.confirm(b.onlyRequest(room).ID)
	a.pull(room, b)
	v.pull(room, a)
	chain = signChainAfter(t, x, room, 400)
	return a, v, x, jn, room, chain
}

// Gemini must-fix 6. A hostile admin machine that is not a member of a
// room held here pages it one entry at a time, each page marked more:
// either an entry this machine lacks, or one it holds. The entries kept
// are not checked again under the rooms lock on every page: a page that
// brings nothing new is not tried at all, and new entries are tried once
// a quarter page of them is kept. The join ends refused, nothing stored.
func TestJoinThatPagesOneEntryAtATimeFromANonMemberDoesNotCheckEverythingKeptEachPage(t *testing.T) {
	for _, mode := range []string{"new", "held"} {
		t.Run(mode, func(t *testing.T) {
			_, v, x, jn, room, chain := heldRoomThroughRemovedAdmin(t)
			held := createWire(x, room)
			lower(t, &maxJoinPages, map[string]int{"new": 600, "held": 100}[mode])
			k := 0
			link := newRewriteLink(jn, v, func(an *JoinAnswer) {
				if an.State != JoinStateSigned {
					return
				}
				k++
				an.Heads = nil
				switch {
				case k > len(chain)+50:
					an.Entries, an.More = nil, false // a joiner that does not stop gets an end
				case mode == "held" && k > 10:
					an.Entries, an.More, an.Next = []json.RawMessage{held}, true, int64(k)
				case k <= len(chain):
					an.Entries, an.More, an.Next = []json.RawMessage{chain[k-1]}, true, int64(k)
				default:
					an.Entries, an.More = nil, false
				}
			})
			j := v.linkJoiner(link)
			tok := x.invite(room, RoleMember, false)
			v.preview(j, tok)
			v.accept(j, tok, 0)
			x.confirm(x.onlyRequest(room).ID)
			rows := v.logRows(room)
			tries := countHook(t, "join:receive")
			got := v.resume(j, time.Minute)
			want := map[string]string{"new": ReasonJoinServerNotMember, "held": ReasonJoinTooLong}[mode]
			wantOutcome(t, got, JoinStateRefused, want)
			if n := tries(); n > len(chain)/joinTryAgain+3 {
				t.Fatalf("what the join kept was given to the rooms %d times in %d pages", n, k)
			}
			if n := v.logRows(room); n != rows {
				t.Fatalf("v's log has %d rows, had %d", n, rows)
			}
		})
	}
}

// GPT must-fix 3b. A current member's answer whose heads are older
// entries the log has gone past (a list of ancestors, which would hide
// newer entries) is signed but does not lift the not_caught_up gate; the
// honest heads do, after a restart cleared it.
func TestJoinWhoseHeadsAreOlderEntriesDoesNotCountAsCaughtUp(t *testing.T) {
	clock := lclock()
	a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(a)
	room := a.create("team")
	joinRoom(t, jn, a, b, room, RoleMember, false)
	a.change(room, Change{Kind: core.EntryAdd, Target: fmt.Sprintf("%064x", 5), Name: "later"})
	b.restart()
	a.r.mu.Lock()
	rl := a.r.rooms[room]
	createID := rl.createID
	a.r.mu.Unlock()

	stale := newRewriteLink(jn, b, func(an *JoinAnswer) {
		if len(an.Heads) > 0 {
			an.Heads = []string{createID}
		}
	})
	j := b.linkJoiner(stale)
	tok := a.invite(room, RoleMember, false)
	b.preview(j, tok)
	b.accept(j, tok, 0)
	a.confirm(a.onlyRequest(room).ID)
	wantOutcome(t, b.resume(j, time.Minute), JoinStateSigned, "")
	if b.pulled(room) {
		t.Fatal("an answer whose heads are older entries opened the not_caught_up gate")
	}

	j = b.joiner(jn)
	tok = a.invite(room, RoleMember, false)
	b.preview(j, tok)
	b.accept(j, tok, 0)
	a.confirm(a.onlyRequest(room).ID)
	wantOutcome(t, b.resume(j, time.Minute), JoinStateSigned, "")
	if !b.pulled(room) {
		t.Fatal("a join through a member with its heads did not catch up")
	}
}

// GPT should-fix. A room from another machine labelled local, held as an
// older version stored it, with a request waiting for the person: another
// room arrives whose label reads as the first one's shown form. The
// request's text does not change, so the approval of the text shown goes
// through.
func TestPendingApprovalTextOfALegacyLocalRoomDoesNotChangeWhenAnotherRoomArrives(t *testing.T) {
	ctx := context.Background()
	clock := lclock()
	net := newMemNet()
	y := newPNode(t, net, "y", newLNode(t, "y", clock))
	b := newPNode(t, net, "b", newLNode(t, "b", clock))
	room := y.create(LocalLabel)
	y.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee", History: true})
	y.change(room, Change{Kind: core.EntryAdmin, Target: b.id})
	b.pin(room, y.lnode) // as 0.2.3 held it
	b.mustPull(room, y, PullOpts{})
	s, err := OpenSurface(ctx, b.m, b.r, SurfaceOpts{Name: "b", Book: NewAddrBook()})
	if err != nil {
		t.Fatal(err)
	}
	q := b.propose(room, Change{Kind: core.EntryAdd, Target: fmt.Sprintf("%064x", 79), Name: "zed"})
	before, err := s.Describe(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	want := LocalLabel + "#" + roomTag(room)
	if before.RoomLabel != want {
		t.Fatalf("shown %q, want %q", before.RoomLabel, want)
	}

	other := y.create(want) // a label that reads as the first room's shown form
	b.pin(other, y.lnode)
	after, err := s.Describe(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if after.Text != before.Text || after.RoomLabel != want {
		t.Fatalf("the request's text changed when another room arrived:\n%q\n%q", before.Text, after.Text)
	}
	if c, err := s.ConfirmShown(ctx, q.ID, true, true, "", before.Text); err != nil || c.State != store.RequestApproved {
		t.Fatalf("approve the text shown before the other room came: %+v %v", c, err)
	}
}

// Gemini should-fix. A join recorded by an older version, for a room held
// here under a label that reads as local, under another label, is refused
// naming the room as it is shown here, not as local.
func TestRefusedLegacyJoinNamesARoomLabelledLocalAsItIsShown(t *testing.T) {
	clock := lclock()
	y, b := newLNode(t, "y", clock), newLNode(t, "b", clock)
	jn := newJoinNet()
	jn.serve(y)
	room := y.create(LocalLabel)
	y.change(room, Change{Kind: core.EntryAdd, Target: b.id, Name: "bee"})
	b.pin(room, y) // as 0.2.3 held it
	b.pull(room, y)
	legacyJoin(t, jn, y, b, room, "payroll", false)
	got := b.resume(b.joiner(jn), time.Minute)
	if len(got) != 1 || got[0].State != JoinStateRefused || !strings.Contains(got[0].Reason, "held here as") {
		t.Fatalf("outcome %+v", got)
	}
	if !strings.Contains(got[0].Reason, Isolate(LocalLabel+"#"+roomTag(room))) {
		t.Fatalf("the reason names the room %q", got[0].Reason)
	}
}

// cutLink is a join link that fails the polls cut picks, before they
// reach the server: a link that drops.
type cutLink struct {
	JoinTransport
	mu    sync.Mutex
	polls int
	cut   func(poll int) bool
}

func (l *cutLink) Join(ctx context.Context, addrs []string, check func(string) error, body []byte) (JoinReply, error) {
	var q JoinRequest
	if json.Unmarshal(body, &q) == nil && q.Op == JoinPoll {
		l.mu.Lock()
		l.polls++
		n := l.polls
		l.mu.Unlock()
		if l.cut(n) {
			return JoinReply{}, fmt.Errorf("the link dropped")
		}
	}
	return l.JoinTransport.Join(ctx, addrs, check, body)
}

// The verifier's must-fix 1 (GPT 3a). The bounds of a join's signed
// answer hold over all its runs, not each run: an admin machine that cuts
// every run short before a bound is reached, so that the daemon's resume
// starts a new run each tick, still has the join refused with
// ReasonJoinTooLong within the bounds. Each run
// here reads three pages, under every bound of one run.
func TestJoinAnswerThatPagesWithoutEndIsRefusedAcrossResumes(t *testing.T) {
	for _, mode := range []string{"pages", "bytes", "stall", "time"} {
		t.Run(mode, func(t *testing.T) {
			clock := lclock()
			a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
			jn := newJoinNet()
			jn.serve(a)
			room := a.create("team")
			create := createWire(a, room)
			switch mode {
			case "pages":
				lower(t, &maxJoinPages, 20)
			case "bytes":
				lower(t, &maxJoinBytes, 16<<10)
			case "time":
				lower(t, &joinStallMax, time.Hour)
				lower(t, &joinPagingMax, 5*time.Minute)
			}
			pages, next := 0, int64(0)
			link := newRewriteLink(jn, b, func(an *JoinAnswer) {
				if an.State != JoinStateSigned {
					return
				}
				pages++
				if mode == "stall" || mode == "time" {
					*clock = clock.Add(time.Minute)
				}
				if pages == 1 {
					an.More, an.Heads, next = true, nil, an.Next
					return
				}
				next++
				an.Entries, an.More, an.Next, an.Heads = []json.RawMessage{create}, true, next, nil
			})
			confirmed := false
			cut := &cutLink{JoinTransport: link, cut: func(n int) bool { return confirmed && n%4 == 0 }}
			j := b.linkJoiner(cut)
			tok := a.invite(room, RoleMember, false)
			b.preview(j, tok)
			b.accept(j, tok, 0)
			a.confirm(a.onlyRequest(room).ID)
			confirmed = true
			var got []JoinOutcome
			for range 100 {
				if got = b.resume(j, 0); len(got) != 1 || got[0].State != JoinStateAccepted {
					break
				}
			}
			wantOutcome(t, got, JoinStateRefused, ReasonJoinTooLong)
			if pages > pagingMost[mode] {
				t.Fatalf("%d signed pages read", pages)
			}
			// The room the first run pinned stays, as before: a later run
			// leaves only a room its own pin made, since the room may have
			// become this machine's another way between runs.
			if len(j.runs) != 0 {
				t.Fatalf("the paging of an ended join stays: %d", len(j.runs))
			}
		})
	}
}

// The verifier's must-fix 2. An honest join of a long log whose link
// drops once, then reads slowly, is signed: the next run goes on from
// where the last one stopped, and a run that reads the log again from
// the start (after a restart, with a new Joiner) counts the pages of
// entries held here as moving on, as far as this machine holds them.
func TestJoinOfALongLogOverALinkThatDropsOnceThenReadsSlowlyIsSigned(t *testing.T) {
	for _, mode := range []string{"resume", "restart"} {
		t.Run(mode, func(t *testing.T) {
			clock := lclock()
			a, b := newLNode(t, "a", clock), newLNode(t, "b", clock)
			jn := newJoinNet()
			jn.serve(a)
			room := a.create("team")
			signChainAfter(t, a, room, 2000)
			pages := 0
			link := newRewriteLink(jn, b, func(an *JoinAnswer) {
				if an.State == JoinStateSigned {
					pages++
					*clock = clock.Add(30 * time.Second)
				}
			})
			confirmed, dropped := false, false
			cut := &cutLink{JoinTransport: link, cut: func(int) bool {
				if confirmed && !dropped && pages == 6 {
					dropped = true
					return true
				}
				return false
			}}
			j := b.linkJoiner(cut)
			tok := a.invite(room, RoleMember, false)
			b.preview(j, tok)
			b.accept(j, tok, 0)
			a.confirm(a.onlyRequest(room).ID)
			confirmed = true
			wantOutcome(t, b.resume(j, 0), JoinStateAccepted, "")
			if !dropped {
				t.Fatal("the link did not drop")
			}
			if mode == "restart" {
				j = b.linkJoiner(cut)
			}
			wantOutcome(t, b.resume(j, 0), JoinStateSigned, "")
			if want := map[string]int{"resume": 8, "restart": 14}[mode]; pages != want {
				t.Fatalf("%d signed pages read, want %d", pages, want)
			}
			sameState(t, "b joined", a.state(room), b.state(room))
		})
	}
}
