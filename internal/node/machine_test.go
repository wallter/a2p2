package node

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

const mRoom = "r_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// rig is one machine over a store in a temporary directory, with a clock
// the test moves and counter randomness.
type rig struct {
	t    *testing.T
	path string
	key  ed25519.PrivateKey
	st   *store.Store
	m    *Machine
	now  time.Time
	n    byte
}

func newRig(t *testing.T) *rig {
	seed := sha256.Sum256([]byte("machine test"))
	r := &rig{t: t, path: filepath.Join(t.TempDir(), "a2p2.db"), key: ed25519.NewKeyFromSeed(seed[:]),
		now: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}
	r.open()
	if err := r.st.AddRoom(context.Background(), mRoom, "team", "", r.now); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"alice", "bob", "carol", "dave"} {
		r.register(a)
	}
	t.Cleanup(func() { r.st.Close() })
	return r
}

func (r *rig) open() {
	st, err := store.Open(r.path)
	if err != nil {
		r.t.Fatal(err)
	}
	r.st = st
	r.m = NewMachine(st, r.key, MachineOpts{
		Now:  func() time.Time { return r.now },
		Rand: func(b []byte) { r.n++; b[len(b)-1] = r.n },
	})
}

// restart is a daemon start over the same database [R2].
func (r *rig) restart() {
	r.st.Close()
	r.open()
}

func (r *rig) register(agent string) {
	r.t.Helper()
	if _, err := r.m.Register(context.Background(), agent); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) tick() { r.now = r.now.Add(time.Minute) }

func (r *rig) send(agent string, d Draft) string {
	r.t.Helper()
	r.tick()
	d.Room, d.Agent = mRoom, agent
	if d.Text == "" {
		d.Text = "hello"
	}
	s, err := r.m.Send(context.Background(), d)
	if err != nil {
		r.t.Fatal(err)
	}
	return s.ID
}

func (r *rig) to(agents ...string) []core.Addressee {
	var out []core.Addressee
	for _, a := range agents {
		out = append(out, core.Addressee{Node: r.m.ID, Agent: a})
	}
	return out
}

// inbox lists agent's inbox as the inbox tool does: with a receipt of each
// entry, which Seen requires.
func (r *rig) inbox(agent string) []Entry {
	r.t.Helper()
	v, err := r.m.Inbox(context.Background(), mRoom, agent)
	if err == nil {
		err = r.m.Showed(context.Background(), mRoom, agent, v.Entries, true)
	}
	if err != nil {
		r.t.Fatal(err)
	}
	return v.Entries
}

func (r *rig) seen(agent string, es []Entry) {
	r.t.Helper()
	for _, res := range r.confirm(agent, es) {
		if !res.Confirmed {
			r.t.Fatalf("%s refused: %s", res.Key, res.Reason)
		}
	}
}

// confirm calls Seen and returns its per-entry results.
func (r *rig) confirm(agent string, es []Entry) []SeenResult {
	r.t.Helper()
	var reads []store.Read
	for _, e := range es {
		reads = append(reads, store.Read{Key: e.Key, Version: e.Version})
	}
	res, err := r.m.Seen(context.Background(), mRoom, agent, reads)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

// putOwn stores a record signed by this machine's key, as a received copy
// of it would be (a peer may hold records this machine wrote and lost).
func (r *rig) putOwn(f core.RecordFields) *core.VerifiedRecord {
	r.t.Helper()
	return r.put(r.key, f)
}

func (r *rig) put(k ed25519.PrivateKey, f core.RecordFields) *core.VerifiedRecord {
	r.t.Helper()
	if f.Room == "" {
		f.Room = mRoom
	}
	if f.Seq == 0 {
		f.Seq = 1
	}
	if f.Created.IsZero() {
		f.Created = r.now
	}
	rec, err := core.NewRecord(k, f)
	if err != nil {
		r.t.Fatal(err)
	}
	v, err := rec.Verified()
	if err != nil {
		r.t.Fatal(err)
	}
	if out, err := r.st.PutRecord(context.Background(), v, store.PutOpts{Now: r.now, MayRead: true}); err != nil || out != store.OutcomeStored {
		r.t.Fatalf("put: %s %v", out, err)
	}
	return v
}

func (r *rig) putHeader(f core.RecordFields) *core.VerifiedRecord {
	r.t.Helper()
	f.Room, f.Seq, f.Created = mRoom, 1, r.now
	rec, err := core.NewRecord(r.key, f)
	if err != nil {
		r.t.Fatal(err)
	}
	v, err := rec.WithoutBody().Verified()
	if err != nil {
		r.t.Fatal(err)
	}
	if out, err := r.st.PutRecord(context.Background(), v, store.PutOpts{Now: r.now, MayRead: true}); err != nil || out != store.OutcomeStored {
		r.t.Fatalf("put header: %s %v", out, err)
	}
	return v
}

func (r *rig) read(agent string) []Entry {
	r.t.Helper()
	es := r.inbox(agent)
	r.seen(agent, es)
	return es
}

func (r *rig) record(id string) store.Held {
	r.t.Helper()
	var h store.Held
	err := r.st.View(context.Background(), func(rd *store.Reader) error {
		var ok bool
		var err error
		h, ok, err = rd.Record(mRoom, id)
		if err == nil && !ok {
			err = errors.New("not held")
		}
		return err
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return h
}

func kinds(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Kind+":"+e.Text)
	}
	return out
}

func wantKinds(t *testing.T, es []Entry, want ...string) {
	t.Helper()
	if got := kinds(es); !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
		t.Fatalf("entries %q, want %q", got, want)
	}
}

// [R2, R4] Every daemon start writes a new stream whose first record links
// every head of this machine's history held; a start from a restored older
// database links only what it holds, and positions are never reused.
func TestEveryStartWritesANewStreamLinkedToEveryHeadHeld(t *testing.T) {
	r := newRig(t)
	a := r.send("alice", Draft{})
	b := r.send("alice", Draft{})
	fa, fb := r.record(a).Record.Fields(), r.record(b).Record.Fields()
	if fa.Seq != 1 || len(fa.Links) != 0 || fb.Seq != 2 || fb.Prev != a || fb.Epoch != fa.Epoch {
		t.Fatalf("first stream: %+v %+v", fa, fb)
	}
	r.st.Close()
	old := filepath.Join(t.TempDir(), "old.db")
	copyDB(t, r.path, old)
	r.open()

	c := r.send("bob", Draft{})
	fc := r.record(c).Record.Fields()
	if fc.Seq != 1 || fc.Epoch == fa.Epoch || !reflect.DeepEqual(fc.Links, []core.Link{{Epoch: fa.Epoch, Seq: 2, ID: b}}) {
		t.Fatalf("second stream: %+v", fc)
	}

	// Restore the copy taken before c: the next start links b again, in a
	// third stream, and nothing forks.
	c2, err := r.record(c).Record.Verified()
	if err != nil {
		t.Fatal(err)
	}
	r.st.Close()
	copyDB(t, old, r.path)
	r.open()
	d := r.send("bob", Draft{})
	fd := r.record(d).Record.Fields()
	if fd.Seq != 1 || fd.Epoch == fc.Epoch || fd.Epoch == fa.Epoch || len(fd.Links) != 1 || fd.Links[0].ID != b {
		t.Fatalf("stream after restore: %+v", fd)
	}

	// Two streams that each follow b: a new start links both heads.
	if out, err := r.st.PutRecord(context.Background(), c2, store.PutOpts{Now: r.now, MayRead: true}); err != nil || out != store.OutcomeStored {
		t.Fatalf("the lost stream comes back from a copy: %s %v", out, err)
	}
	r.restart()
	e := r.send("bob", Draft{})
	if got := len(r.record(e).Record.Fields().Links); got != 2 {
		t.Fatalf("links %d, want both heads", got)
	}
}

// D10, [R7] A reply goes to its parent's author unless addressees are named
// or it is sent to everyone; parent and thread are filled in.
func TestReplyGoesToTheParentsAuthorUnlessAddresseesAreNamed(t *testing.T) {
	r := newRig(t)
	root := r.send("alice", Draft{To: r.to("bob")})
	reply := r.send("bob", Draft{ReplyTo: root})
	f := r.record(reply).Record.Fields()
	if f.Parent != root || f.Thread != root || !reflect.DeepEqual(f.To, r.to("alice")) {
		t.Fatalf("reply %+v", f)
	}
	deeper := r.send("alice", Draft{ReplyTo: reply, To: r.to("carol", "bob", "carol")})
	f = r.record(deeper).Record.Fields()
	if f.Parent != reply || f.Thread != root || !reflect.DeepEqual(f.To, r.to("bob", "carol")) {
		t.Fatalf("named reply %+v", f)
	}
	all := r.send("carol", Draft{ReplyTo: reply, Everyone: true, AskHandled: true})
	if f = r.record(all).Record.Fields(); len(f.To) != 0 || !f.AskHandled {
		t.Fatalf("reply to everyone %+v", f)
	}
	if _, err := r.m.Send(context.Background(), Draft{Room: mRoom, Agent: "bob", Text: "x", ReplyTo: "00000000000000000000000000000000"}); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("reply to unknown: %v", err)
	}
	if _, err := r.m.Send(context.Background(), Draft{Room: mRoom, Agent: "bob", Text: "x", To: r.to("alice"), AskHandled: true}); err == nil {
		t.Fatal("ask_handled with addressees accepted")
	}
}

// [R7] Only an addressee, or anyone but the author of a room-wide post that
// asked, may report handled, and only after a read showed it the text (an
// inbox listing is enough, D-reading); a second report returns the first.
func TestHandledOnlyByAnAddresseeAfterItWasShownTheText(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	direct := r.send("alice", Draft{To: r.to("bob")})
	plain := r.send("alice", Draft{})
	asked := r.send("alice", Draft{AskHandled: true})

	if _, err := r.m.Handled(ctx, mRoom, "bob", direct); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("before shown: %v", err)
	}
	if _, err := r.m.Handled(ctx, mRoom, "carol", asked); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("carol before shown: %v", err)
	}
	shown := r.inbox("bob")
	r.seen("bob", shown)
	r.read("carol")
	h1, err := r.m.Handled(ctx, mRoom, "bob", direct)
	if err != nil {
		t.Fatal(err)
	}
	if h2, err := r.m.Handled(ctx, mRoom, "bob", direct); err != nil || h2.ID != h1.ID {
		t.Fatalf("second report %v %v", h2, err)
	}
	for _, c := range []struct{ agent, id string }{{"carol", direct}, {"carol", plain}, {"alice", asked}} {
		if _, err := r.m.Handled(ctx, mRoom, c.agent, c.id); !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("%s on %s: %v", c.agent, c.id, err)
		}
	}
	if _, err := r.m.Handled(ctx, mRoom, "carol", asked); err != nil {
		t.Fatal(err)
	}
	if e := r.record(h1.ID).Effect; e != store.EffectOK {
		t.Fatalf("effect %s", e)
	}
}

// [R7] Replace and withdraw are only of this machine's own messages (any
// agent here, T2); a replace's version is one above the winning version.
func TestReplaceAndWithdrawOnlyOfThisMachinesOwnMessages(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	m := r.send("alice", Draft{})
	r1, err := r.m.Replace(ctx, mRoom, "bob", m, "second")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := r.m.Replace(ctx, mRoom, "alice", m, "third")
	if err != nil {
		t.Fatal(err)
	}
	if v1, v2 := r.record(r1.ID).Record.Fields().Version, r.record(r2.ID).Record.Fields().Version; v1 != 1 || v2 != 2 {
		t.Fatalf("versions %d %d", v1, v2)
	}
	if _, err := r.m.Replace(ctx, mRoom, "alice", r1.ID, "of a replace"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("replace of a replace: %v", err)
	}

	seed := sha256.Sum256([]byte("someone else"))
	other := r.put(ed25519.NewKeyFromSeed(seed[:]), core.RecordFields{Epoch: "00000000000000aa",
		Kind: core.KindMessage, Agent: "zed", Text: "theirs"})
	if _, err := r.m.Withdraw(ctx, mRoom, "alice", other.ID()); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("withdraw of another machine's message: %v", err)
	}
	if _, err := r.m.Withdraw(ctx, mRoom, "alice", "00000000000000000000000000000000"); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("withdraw of nothing: %v", err)
	}
	if _, err := r.m.Withdraw(ctx, mRoom, "alice", m); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{m, r1.ID, r2.ID} {
		if h := r.record(id); h.Reason != store.ReasonWithdrawn || h.Record.Body() != nil {
			t.Fatalf("%s reason %s", id, h.Reason)
		}
	}
}

// [R7] The inbox shows the winning text; notices of a replace or withdraw go
// only to agents who saw the original; a message withdrawn before it was
// shown is never shown.
func TestNoticesGoOnlyToAgentsWhoSawTheOriginal(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	m := r.send("alice", Draft{Text: "v0"})
	wantKinds(t, r.read("bob"), "message:v0")
	r.tick()
	rep, err := r.m.Replace(ctx, mRoom, "alice", m, "v1")
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, r.inbox("carol"), "message:v1")
	es := r.inbox("bob")
	wantKinds(t, es, "replaced:v1")
	if es[0].ID != m || es[0].Record != rep.ID || es[0].Version != rep.ID {
		t.Fatalf("notice %+v", es[0])
	}
	r.seen("bob", es)
	r.tick()
	if _, err := r.m.Withdraw(ctx, mRoom, "alice", m); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, r.inbox("bob"), "withdrawn:")
	wantKinds(t, r.inbox("carol"))
	wantKinds(t, r.inbox("alice"))
}

// [R6] A message that expired before an agent was shown it is an expired
// notice, once. Expiry is decided at read time and made durable by Prune,
// which a read asks for and Seen runs first: a clock set back afterwards
// does not bring the text back.
func TestTextThatExpiredBeforeItWasShownIsAnExpiredNoticeEvenAfterTheClockGoesBack(t *testing.T) {
	r := newRig(t)
	m := r.send("alice", Draft{})
	r.now = r.now.Add(store.Retention)
	es := r.inbox("bob")
	wantKinds(t, es, "expired:")
	if es[0].Key != "x:"+m || r.record(m).Reason != store.ReasonLive || !r.m.PruneDue() {
		t.Fatalf("read-time expiry: key %s reason %s due %v", es[0].Key, r.record(m).Reason, r.m.PruneDue())
	}
	r.seen("bob", es) // runs the due Prune first
	if h := r.record(m); h.Reason != store.ReasonExpired || h.Record.Body() != nil || r.m.PruneDue() {
		t.Fatalf("after Seen: reason %s", h.Reason)
	}
	r.now = r.now.Add(-40 * 24 * time.Hour)
	wantKinds(t, r.inbox("bob"))
	wantKinds(t, r.inbox("carol"), "expired:")
	th, err := r.m.Thread(context.Background(), mRoom, m)
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, th, "expired:")

	// Shown expired, then the daemon's own Prune, then the clock goes back.
	n := r.send("alice", Draft{Text: "later"})
	r.now = r.now.Add(store.Retention + time.Hour)
	wantKinds(t, r.inbox("dave"), "expired:", "expired:")
	if err := r.m.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(-40 * 24 * time.Hour)
	if h := r.record(n); h.Reason != store.ReasonExpired {
		t.Fatalf("reason %s", h.Reason)
	}
	wantKinds(t, r.inbox("dave"), "expired:", "expired:")
}

// [R7], D4: Only Seen moves the cursor. A daemon that stops between showing
// entries and their confirmation shows them again; a partial Seen confirms
// exactly what it names.
func TestCrashBetweenShowAndSeenShowsTheEntriesAgain(t *testing.T) {
	r := newRig(t)
	r.send("alice", Draft{Text: "one"})
	r.send("alice", Draft{Text: "two"})
	r.send("alice", Draft{Text: "three"})
	es := r.inbox("bob")
	wantKinds(t, es, "message:one", "message:two", "message:three")
	r.restart()
	again := r.inbox("bob")
	wantKinds(t, again, "message:one", "message:two", "message:three")
	r.seen("bob", []Entry{again[0], again[2]})
	r.restart()
	wantKinds(t, r.inbox("bob"), "message:two")
}

// [R7], review blocker 2: Seen accepts a key and version only if Inbox
// would show exactly that entry now. A replacement that arrived after the
// listing makes the old version refused, the rest are applied, and the
// newer text is then shown as the message.
func TestSeenOfAVersionNoLongerShownIsRefusedAndTheRestApplied(t *testing.T) {
	r := newRig(t)
	m := r.send("alice", Draft{Text: "v0"})
	n := r.send("alice", Draft{Text: "other"})
	es := r.inbox("bob")
	r.tick()
	if _, err := r.m.Replace(context.Background(), mRoom, "alice", m, "v1"); err != nil {
		t.Fatal(err)
	}
	res := r.confirm("bob", es)
	if res[0].Confirmed || res[0].Reason != SeenVersionChanged || !res[1].Confirmed || res[1].Key != n {
		t.Fatalf("results %+v", res)
	}
	wantKinds(t, r.inbox("bob"), "message:v1")
}

// Review blocker 2: Seen refuses what Inbox would not show this agent now
// (a made-up key, another agent's message, a notice of something never
// shown), and a refused confirmation does not let the agent report
// handled. Since D-reading an entry needs a receipt: one Inbox would show
// is refused until a listing showed it, and then only at the version
// listed.
func TestSeenRefusesEntriesInboxWouldNotShowAndHandledStaysTiedToShownText(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	m := r.send("alice", Draft{To: r.to("bob")})
	for _, rd := range []store.Read{{Key: m, Version: m}, {Key: "x:" + m}, {Key: "nonsense", Version: m}} {
		res, err := r.m.Seen(ctx, mRoom, "carol", []store.Read{rd})
		if err != nil || res[0].Confirmed || res[0].Reason != SeenNotShown {
			t.Fatalf("carol %+v: %+v %v", rd, res, err)
		}
	}
	if _, err := r.m.Handled(ctx, mRoom, "carol", m); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("carol handled: %v", err)
	}
	res, err := r.m.Seen(ctx, mRoom, "bob", []store.Read{{Key: m, Version: m}})
	if err != nil || res[0].Confirmed || res[0].Reason != SeenNotShown {
		t.Fatalf("bob before a listing: %+v %v", res, err)
	}
	if _, err := r.m.Handled(ctx, mRoom, "bob", m); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("bob handled before a listing: %v", err)
	}
	r.inbox("bob")
	res, err = r.m.Seen(ctx, mRoom, "bob", []store.Read{{Key: m, Version: "deadbeef"}, {Key: m, Version: m}})
	if err != nil || res[0].Confirmed || res[0].Reason != SeenNotShown || !res[1].Confirmed {
		t.Fatalf("bob: %+v %v", res, err)
	}
	if _, err := r.m.Handled(ctx, mRoom, "bob", m); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Seen(ctx, mRoom, "nobody", nil); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("unregistered agent: %v", err)
	}
}

// [R7] A notice confirmed in a listing taken before the machine was
// restored to a copy that lacks the replace is refused: Inbox no longer
// shows it, so it does not count as having seen the message.
func TestConfirmationOfAnEntryLostToARestoreIsRefused(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	m := r.send("alice", Draft{Text: "v0", To: r.to("bob")})
	r.st.Close()
	old := filepath.Join(t.TempDir(), "old.db")
	copyDB(t, r.path, old)
	r.open()
	r.read("bob")
	r.tick()
	if _, err := r.m.Replace(ctx, mRoom, "alice", m, "v1"); err != nil {
		t.Fatal(err)
	}
	shown := r.inbox("bob")
	wantKinds(t, shown, "replaced:v1")
	r.st.Close()
	copyDB(t, old, r.path)
	r.open()
	if res := r.confirm("bob", shown); res[0].Confirmed || res[0].Reason != SeenNotShown {
		t.Fatalf("results %+v", res)
	}
	wantKinds(t, r.inbox("bob"), "message:v0")
}

// [R7], review should-fix 3: a replace's version is one above the highest
// version of the message held, text or not, so a replacement written after
// the winning one expired still supersedes it.
func TestReplaceWrittenAfterTheWinningReplacementExpiredTakesTheNextVersion(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	m := r.send("alice", Draft{Text: "v0", To: r.to("bob")})
	r.tick()
	old, err := r.m.Replace(ctx, mRoom, "alice", m, "v1")
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, r.read("bob"), "message:v1")
	r.now = r.now.Add(store.Retention)
	if err := r.m.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	fresh, err := r.m.Replace(ctx, mRoom, "alice", m, "v2")
	if err != nil {
		t.Fatal(err)
	}
	vo, vf := r.record(old.ID).Record.Fields().Version, r.record(fresh.ID).Record.Fields().Version
	if vo != 1 || vf != 2 {
		t.Fatalf("versions %d and %d", vo, vf)
	}
	wantKinds(t, r.inbox("bob"), "replaced:v2")
}

// Review should-fix 5: the version, heads and position are chosen inside
// the store's write transaction, so two writers over one database (two
// Machines, as two processes would be) never share a version or position.
func TestTwoWritersOverOneDatabaseNeverShareAVersion(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	m := r.send("alice", Draft{})
	st2, err := store.Open(r.path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	other := NewMachine(st2, r.key, MachineOpts{Now: func() time.Time { return r.now }, Rand: func(b []byte) { b[0] = 0x77 }})
	done := make(chan Sent)
	first := NewMachine(r.st, r.key, MachineOpts{Now: func() time.Time { return r.now }, Rand: func(b []byte) {
		b[0] = 0x66
		// Inside the first writer's transaction: start the second writer
		// and give it time to read before this one commits, as it could
		// if the decision were taken outside the transaction.
		go func() {
			s, err := other.Replace(ctx, mRoom, "bob", m, "from other")
			if err != nil {
				t.Error(err)
			}
			done <- s
		}()
		time.Sleep(200 * time.Millisecond)
	}})
	a, err := first.Replace(ctx, mRoom, "alice", m, "from first")
	if err != nil {
		t.Fatal(err)
	}
	b := <-done
	va, vb := r.record(a.ID).Record.Fields().Version, r.record(b.ID).Record.Fields().Version
	if va == vb {
		t.Fatalf("both writers chose version %d", va)
	}
}

// Review should-fix 6: Inbox is a read-only transaction and does not wait
// for another connection holding the write lock.
func TestInboxRunsWhileAnotherWriterHoldsTheLock(t *testing.T) {
	r := newRig(t)
	r.send("alice", Draft{})
	db, err := sql.Open("sqlite", store.FileURI(r.path)+"?_pragma=busy_timeout(100)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO meta VALUES ('lock', 1)`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if v, err := r.m.Inbox(ctx, mRoom, "bob"); err != nil || len(v.Entries) != 1 {
		t.Fatalf("inbox while locked: %v %v", v, err)
	}
}

// D1, review should-fix 4: an agent registered after messages were written
// starts at the current end and sees only what comes after; an agent never
// registered is refused.
func TestAgentRegisteredLaterSeesOnlyWhatComesAfter(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.send("alice", Draft{Text: "before"})
	if _, err := r.m.Inbox(ctx, mRoom, "erin"); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("unregistered: %v", err)
	}
	r.register("erin")
	wantKinds(t, r.inbox("erin"))
	r.restart()
	r.send("alice", Draft{Text: "after"})
	wantKinds(t, r.inbox("erin"), "message:after")
	wantKinds(t, r.inbox("bob"), "message:before", "message:after")
}

// Review should-fix 1: a handled report counts only when its reference
// checks out: one by an agent that may not report (a room-wide post that
// did not ask) is bad_reference and hides nothing from that agent.
func TestInvalidHandledReportDoesNotHideTheMessage(t *testing.T) {
	r := newRig(t)
	m := r.send("alice", Draft{Text: "for all"})
	bad := r.putOwn(core.RecordFields{Epoch: "00000000000000b1", Kind: core.KindHandled, Agent: "carol", Ref: m})
	if e := r.record(bad.ID()).Effect; e != store.EffectBadReference {
		t.Fatalf("effect %s", e)
	}
	wantKinds(t, r.inbox("carol"), "message:for all")
	// A withdraw of a replace is bad_reference and announces nothing.
	wantKinds(t, r.read("bob"), "message:for all")
	rep, err := r.m.Replace(context.Background(), mRoom, "alice", m, "v1")
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, r.read("bob"), "replaced:v1")
	w := r.putOwn(core.RecordFields{Epoch: "00000000000000b2", Kind: core.KindWithdraw, Agent: "alice", Ref: rep.ID})
	if e := r.record(w.ID()).Effect; e != store.EffectBadReference {
		t.Fatalf("withdraw effect %s", e)
	}
	wantKinds(t, r.inbox("bob"))
}

// Review should-fix 2: Thread and Branch follow only replies whose parent
// and thread check out, need a message at the root, and leave out a
// message whose newest text is still on its way, as Inbox waits for it.
func TestThreadAndBranchFollowOnlyValidRepliesAndWaitLikeInbox(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	root := r.send("alice", Draft{Text: "root"})
	other := r.send("alice", Draft{Text: "other"})
	liar := r.putOwn(core.RecordFields{Epoch: "00000000000000d1", Kind: core.KindMessage, Agent: "bob",
		Parent: root, Thread: other, Text: "liar"})
	if e := r.record(liar.ID()).Effect; e != store.EffectBadReference {
		t.Fatalf("effect %s", e)
	}
	br, err := r.m.Branch(ctx, mRoom, root)
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, br, "message:root")
	th, err := r.m.Thread(ctx, mRoom, other)
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, th, "message:other")
	for _, id := range []string{"00000000000000000000000000000000"} {
		if br, err := r.m.Branch(ctx, mRoom, id); err != nil || len(br) != 0 {
			t.Fatalf("branch of nothing: %v %v", br, err)
		}
	}
	rep, err := r.m.Replace(ctx, mRoom, "alice", other, "other v1")
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range []func(context.Context, string, string) ([]Entry, error){r.m.Thread, r.m.Branch} {
		if es, err := view(ctx, mRoom, rep.ID); err != nil || len(es) != 0 {
			t.Fatalf("view rooted at a replace: %v %v", es, err)
		}
	}

	m := r.send("alice", Draft{Text: "v0", To: r.to("bob")})
	r.putHeader(core.RecordFields{Epoch: "00000000000000d2", Kind: core.KindReplace, Agent: "alice", Ref: m, Version: 1, Text: "v1"})
	v, err := r.m.Inbox(ctx, mRoom, "bob")
	if err != nil {
		t.Fatal(err)
	}
	th, err = r.m.Thread(ctx, mRoom, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Waiting) == 0 || len(th) != 0 {
		t.Fatalf("inbox waits %v; thread %q", v.Waiting, kinds(th))
	}
}

// [R7], G3: the thread view lists the held messages of a thread, the branch
// view follows parent down; neither marks anything seen.
func TestThreadAndBranchViewsMarkNothing(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	root := r.send("alice", Draft{Text: "root"})
	a := r.send("bob", Draft{Text: "a", ReplyTo: root})
	r.send("alice", Draft{Text: "b", ReplyTo: a})
	r.send("carol", Draft{Text: "c", ReplyTo: root})
	th, err := r.m.Thread(ctx, mRoom, root)
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, th, "message:root", "message:a", "message:b", "message:c")
	if th[2].ParentState != ParentHere || th[2].Thread != root || th[0].ParentState != "" {
		t.Fatalf("thread entries %+v", th)
	}
	br, err := r.m.Branch(ctx, mRoom, a)
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, br, "message:a", "message:b")
	if _, err := r.m.Withdraw(ctx, mRoom, "bob", a); err != nil {
		t.Fatal(err)
	}
	th, err = r.m.Thread(ctx, mRoom, root)
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, th, "message:root", "message:b", "message:c")
	if th[1].ParentState != ParentWithdrawn {
		t.Fatalf("parent state %s", th[1].ParentState)
	}
	wantKinds(t, r.inbox("dave"), "message:root") // replies go to their parents' authors
}

// [R4] A write refused before it is stored (blank text) leaves no gap: the
// next write is the next position.
func TestRefusedWriteLeavesNoGapInTheStream(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := r.send("alice", Draft{})
	if _, err := r.m.Send(ctx, Draft{Room: mRoom, Agent: "alice", Text: " \n"}); err == nil {
		t.Fatal("blank text accepted")
	}
	if _, err := r.m.Send(ctx, Draft{Room: mRoom, Agent: "bad label!", Text: "x"}); err == nil {
		t.Fatal("bad agent label accepted")
	}
	b := r.send("alice", Draft{})
	if f := r.record(b).Record.Fields(); f.Seq != 2 || f.Prev != a {
		t.Fatalf("next write %+v", f)
	}
}
