package node

// Tests of the fix round of the switch, part A: networking decisions and
// states, the v0.2 notice, history paging, trace, request text, questions
// and the missing alert.

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
	_ "modernc.org/sqlite"
)

func newSurfKey(t *testing.T, b byte, net Network, tr JoinTransport, addrs []string) *surfEnv {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = b
	e := &surfEnv{t: t, path: filepath.Join(t.TempDir(), "a2p2.db"), key: ed25519.NewKeyFromSeed(seed),
		net: net, tr: tr, addrs: addrs}
	e.open()
	return e
}

// recordingJoins is a join transport that records each call and answers
// none: it shows whether a surface dialled at all.
type recordingJoins struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recordingJoins) Join(_ context.Context, addrs []string, _ func(string) error, _ []byte) (JoinReply, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, addrs)
	return JoinReply{}, errors.New("recording transport: no machine answers")
}

func (r *recordingJoins) n() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func TestNetworkingIsOffUntilAPersonsDecisionIsRecorded(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	if n, err := LoadNetwork(ctx, e.st); err != nil || n.State != NetworkOff || n.Request != "" {
		t.Fatalf("no decision: %+v %v", n, err)
	}
	if _, err := e.s.SetNetwork(ctx, Network{State: NetworkOn, Listen: "not an address"}); !isUser(err) {
		t.Fatalf("bad address: %v", err)
	}
	if _, err := e.s.SetNetwork(ctx, Network{State: "sideways"}); !isUser(err) {
		t.Fatalf("bad state: %v", err)
	}
	for _, want := range []Network{{State: NetworkOn, Listen: "127.0.0.1:7423"}, {State: NetworkOutbound}, {State: NetworkOff}} {
		got, err := e.s.SetNetwork(ctx, want)
		if err != nil || got.Request == "" {
			t.Fatalf("set %+v: %+v %v", want, got, err)
		}
		if n, err := LoadNetwork(ctx, e.st); err != nil || n != got {
			t.Fatalf("load after %+v: %+v %v", want, n, err)
		}
	}
	// Each decision is a decided request, listed with the others.
	qs, err := e.st.Requests(ctx, "", store.RequestApproved)
	if err != nil || len(qs) != 3 {
		t.Fatalf("decisions: %+v %v", qs, err)
	}
	for _, q := range qs {
		if q.Kind != store.RequestNetwork || q.DecidedAt.IsZero() {
			t.Fatalf("decision row: %+v", q)
		}
	}
	if pending, _ := e.st.Requests(ctx, "", store.RequestPending); len(pending) != 0 {
		t.Fatalf("a decision waits: %+v", pending)
	}
	// The three decisions may share a timestamp, so the list's order is
	// not theirs: find the "on" one by its action.
	on := qs[0]
	for _, q := range qs {
		if q.Action == NetworkOn {
			on = q
		}
	}
	if v, err := e.s.Describe(ctx, on); err != nil || !strings.Contains(v.Text, "listen for other machines on 127.0.0.1:7423") {
		t.Fatalf("network decision text: %+v %v", v, err)
	}
}

func TestNetworkingOffDialsNothingAndOutboundDials(t *testing.T) {
	ctx := context.Background()
	admin := newSurfKey(t, 1, Network{State: NetworkOn, Listen: "127.0.0.1:7423"}, nil, []string{"127.0.0.1:7423"})
	if _, err := admin.s.CreateRoom(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	inv, err := admin.s.Invite(ctx, "team", "member", false)
	if err != nil {
		t.Fatal(err)
	}

	off := &recordingJoins{}
	j := newSurfKey(t, 2, Network{State: NetworkOff}, off, nil)
	_, err = j.s.JoinPreview(ctx, inv.Token)
	if !isUser(err) || !strings.Contains(err.Error(), "networking is off") {
		t.Fatalf("preview with networking off: %v", err)
	}
	if _, err := j.s.JoinAccept(ctx, inv.Token, "pv", 0); !isUser(err) || !strings.Contains(err.Error(), "networking is off") {
		t.Fatalf("accept with networking off: %v", err)
	}
	if o, err := j.s.ResumeJoins(ctx, 0); err != nil || o != nil {
		t.Fatalf("resume with networking off: %+v %v", o, err)
	}
	if _, err := j.s.CreateRoom(ctx, "mine"); err != nil {
		t.Fatal(err)
	}
	if _, err := j.s.Invite(ctx, "mine", "member", false); !isUser(err) || !strings.Contains(err.Error(), "networking is off") {
		t.Fatalf("invite with networking off: %v", err)
	}
	if off.n() != 0 {
		t.Fatalf("networking off dialled: %v", off.calls)
	}

	out := &recordingJoins{}
	o := newSurfKey(t, 3, Network{State: NetworkOutbound}, out, nil)
	if _, err := o.s.JoinPreview(ctx, inv.Token); err == nil || isUser(err) && strings.Contains(err.Error(), "networking is off") {
		t.Fatalf("outbound preview: %v", err)
	}
	if out.n() == 0 {
		t.Fatal("outbound did not dial the admin machine")
	}
	if _, err := o.s.CreateRoom(ctx, "mine"); err != nil {
		t.Fatal(err)
	}
	if _, err := o.s.Invite(ctx, "mine", "member", false); !isUser(err) || !strings.Contains(err.Error(), "networking is outbound") {
		t.Fatalf("invite without a listener: %v", err)
	}
}

// loadV02 copies the schema-version-1 fixture (a database as v0.2 left
// it) into a fresh file.
func loadV02(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "store", "testdata", "v02-schema1.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "a2p2.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(b)); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestV02NoticeFromASchemaOneDatabaseIsShownToAPersonOnceEver(t *testing.T) {
	ctx := context.Background()
	seed := make([]byte, ed25519.SeedSize)
	e := &surfEnv{t: t, path: loadV02(t), key: ed25519.NewKeyFromSeed(seed)}
	e.open()
	// An agent's status does not carry it; a person's status carries it,
	// however often and across restarts, until a surface reports that it
	// was shown (printed by the CLI, drawn by the page).
	if st, err := e.s.Status(ctx, false); err != nil || st.Notice != "" {
		t.Fatalf("agent status: %q %v", st.Notice, err)
	}
	if !strings.Contains(NoticeV02, "removed the messages and rooms") || !strings.Contains(NoticeV02, "room create") {
		t.Fatalf("notice wording: %q", NoticeV02)
	}
	for i := 0; i < 2; i++ {
		if st, err := e.s.Status(ctx, true); err != nil || st.Notice != NoticeV02 {
			t.Fatalf("for a person %d, not yet shown: %q %v", i, st.Notice, err)
		}
		e.restart()
	}
	if err := e.s.NoticeShown(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.s.Status(ctx, true); st.Notice != "" {
		t.Fatalf("after it was shown: %q", st.Notice)
	}
	e.restart()
	if st, _ := e.s.Status(ctx, true); st.Notice != "" {
		t.Fatalf("shown again after a restart: %q", st.Notice)
	}
	if hist, err := e.s.History(ctx, "", "", "", 0); err != nil || len(hist) != 0 {
		t.Fatalf("v0.2 message shown: %+v %v", hist, err)
	}
	if rs, _ := e.s.RoomList(ctx, ""); len(rs) != 1 || rs[0].Label != LocalLabel {
		t.Fatalf("v0.2 room listed: %+v", rs)
	}
	fresh := newSurf(t)
	if st, _ := fresh.s.Status(ctx, true); st.Notice != "" {
		t.Fatal("notice on an empty database")
	}
}

// [D4] History pages by (created, author, seq, id): one author's messages
// of one millisecond come in the order written, whatever their ids.
func TestHistoryPagesInTheOrderWrittenAndABoundaryWithdrawnSinceStillWorks(t *testing.T) {
	ctx := context.Background()
	// Every message is written in the same millisecond: the pages are
	// told apart by position.
	e := newSurf(t)
	at := time.Now().Truncate(time.Millisecond)
	e.now = func() time.Time { return at }
	e.restart()
	alice := e.agent("alice")
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, e.send(SendInput{Agent: alice, Text: "m" + string(rune('0'+i))}).ID)
	}
	page := func(before string, n int) []string {
		t.Helper()
		es, err := e.s.History(ctx, "", "", before, n)
		if err != nil {
			t.Fatalf("history before %q: %v", before, err)
		}
		var out []string
		for _, x := range es {
			out = append(out, x.ID)
		}
		return out
	}
	if got := page("", 2); strings.Join(got, ",") != strings.Join(ids[3:], ",") {
		t.Fatalf("newest page %v, want %v", got, ids[3:])
	}
	// The boundary of the page shown is withdrawn; the page before it is
	// still found, and the withdrawn message is not in any page.
	e.send(SendInput{Agent: alice, Replaces: ids[3]})
	if got := page(ids[3], 2); strings.Join(got, ",") != strings.Join(ids[1:3], ",") {
		t.Fatalf("page before a withdrawn boundary %v, want %v", got, ids[1:3])
	}
	if got := page("", 10); strings.Join(got, ",") != strings.Join([]string{ids[0], ids[1], ids[2], ids[4]}, ",") {
		t.Fatalf("all %v", got)
	}
	if got := page(ids[1], 10); strings.Join(got, ",") != ids[0] {
		t.Fatalf("first page %v", got)
	}
	if _, err := e.s.History(ctx, "", "", strings.Repeat("a", 32), 2); !isUser(err) {
		t.Fatalf("unknown boundary: %v", err)
	}
}

// putHeaderOnly stores a message of this machine in the local room as a
// peer's copy without its text would be: header only.
func putHeaderOnly(t *testing.T, e *surfEnv, created time.Time) string {
	t.Helper()
	rec, err := core.NewRecord(e.key, core.RecordFields{Room: e.s.LocalRoom(), Epoch: "00000000000000e1", Seq: 1,
		Kind: core.KindMessage, Agent: "alice", Created: created, Text: "lost text"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := rec.WithoutBody().Verified()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := e.st.PutRecord(context.Background(), v, store.PutOpts{Now: time.Now(), MayRead: true}); err != nil || out != store.OutcomeStored {
		t.Fatalf("put: %s %v", out, err)
	}
	return rec.ID()
}

func TestTraceSavedMeansTheHeaderIsHeldAndAnUnknownIDIsAnError(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	e.agent("alice")
	id := putHeaderOnly(t, e, time.Now())
	tr, err := e.s.Trace(ctx, id)
	if err != nil || !tr.Saved || tr.Reason != store.ReasonMissing {
		t.Fatalf("header-only trace: %+v %v", tr, err)
	}
	if _, err := e.s.Trace(ctx, strings.Repeat("b", 32)); !isUser(err) || !strings.Contains(err.Error(), "no message") {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestTextMissingForADayIsAnAlert(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	e.agent("alice")
	fresh := putHeaderOnly(t, e, time.Now().Add(-time.Hour))
	as, err := e.s.Alerts(ctx, 0)
	if err != nil || len(as) != 0 {
		t.Fatalf("an hour missing: %+v %v", as, err)
	}
	e2 := newSurfKey(t, 9, Network{}, nil, nil)
	e2.agent("alice")
	old := putHeaderOnly(t, e2, time.Now().Add(-25*time.Hour))
	as, err = e2.s.Alerts(ctx, 0)
	if err != nil || len(as) != 1 || as[0].Kind != AlertMissing || as[0].ID != old || as[0].Room != e2.s.LocalRoom() {
		t.Fatalf("a day missing: %+v %v (fresh %s)", as, err, fresh)
	}
}

func TestRequestTextNamesTheFullFingerprintAndEachTerm(t *testing.T) {
	node := strings.Repeat("c8a48e29", 8)
	fp := Fingerprint(node)
	if len(strings.Fields(fp)) != 16 || strings.ReplaceAll(fp, " ", "") != node {
		t.Fatalf("fingerprint %q", fp)
	}
	for _, tc := range []struct {
		q    store.Request
		want string
	}{
		{store.Request{Kind: "add", Action: ActionAdmitAdmin, Room: "r_1", Node: node, Name: "box", History: true},
			"add machine " + fp + " (box) to room team (r_1) as an admin, with earlier history"},
		{store.Request{Kind: "admin", Action: ActionAdmitAdmin, Room: "r_1", Node: node, Name: "box", History: true},
			"add machine " + fp + " (box) to room team (r_1) as an admin, with earlier history"},
		{store.Request{Kind: "add", Action: ActionAdmit, Room: "r_1", Node: node, Name: "box"},
			"add machine " + fp + " (box) to room team (r_1) as a member, without earlier history"},
		{store.Request{Kind: "admin", Action: ActionPromote, Room: "r_1", Node: node, Name: "box"},
			"make machine " + fp + " (box), a member of room team (r_1), an admin"},
		{store.Request{Kind: "remove", Action: ActionRemove, Room: "r_1", Node: node},
			"remove machine " + fp + " from room team (r_1)"},
		{store.Request{Kind: "add", Action: ActionRejoin, Room: "r_1", Node: node, Name: "box", Role: "member", History: true},
			"let machine " + fp + " (box), already a member of room team (r_1) as a member, with earlier history, " +
				"fetch the room's log again; approving changes nothing"},
	} {
		if got := requestText(tc.q, "team"); got != tc.want {
			t.Errorf("%s/%s:\n got %q\nwant %q", tc.q.Kind, tc.q.Action, got, tc.want)
		}
	}
}

func TestQuestionsForThePersonAreListedUntilAnsweredWhateverTheBacklog(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice := e.agent("alice")
	human := e.s.HumanID()
	for i := 0; i < MaxInboxLimit+5; i++ { // a backlog larger than any inbox page
		e.send(SendInput{Agent: alice, Text: "noise", To: []string{Everyone}})
	}
	q1 := e.send(SendInput{Agent: alice, Text: "Choose a branch", To: []string{human}})
	q2 := e.send(SendInput{Agent: alice, Text: "Ship?", Choices: []string{"yes", "no"}, To: []string{human}})
	ids := func() []string {
		qs, err := e.s.Questions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, q := range qs {
			out = append(out, q.ID)
		}
		return out
	}
	// Both, in (created, id) order; the two may share a millisecond.
	if got := ids(); len(got) != 2 || !slices.Contains(got, q1.ID) || !slices.Contains(got, q2.ID) {
		t.Fatalf("questions %v, want %s %s", got, q1.ID, q2.ID)
	}
	// Answered: the reply, then seen and handled, as the page does it.
	e.send(SendInput{Agent: human, ReplyTo: q1.ID, Text: "main"})
	qs, _ := e.s.Questions(ctx)
	i := slices.IndexFunc(qs, func(q Entry) bool { return q.ID == q1.ID })
	if err := e.s.Listed(ctx, human, qs); err != nil {
		t.Fatal(err)
	}
	if res, err := e.s.Seen(ctx, human, []store.Read{{Key: qs[i].Key, Version: qs[i].Version}}); err != nil || !res[0].Confirmed {
		t.Fatalf("seen: %+v %v", res, err)
	}
	if got := ids(); len(got) != 1 || got[0] != q2.ID {
		t.Fatalf("after answering: %v", got)
	}
}
