package node

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/members"
	"github.com/wallter/a2p2/internal/store"
)

// hostileRoom has the attacker's machine create a room with label, make
// the victim its second admin, add a sockpuppet named "helper" and sign
// an admin entry for the sockpuppet (with name, when not empty), which
// waits for the victim's second signature. It returns the room and the
// victim's seconding request.
func hostileRoom(t *testing.T, label, name string) (attacker, victim, puppet *lnode, room string, q store.Request) {
	t.Helper()
	clock := lclock()
	attacker, victim, puppet = newLNode(t, "attacker", clock), newLNode(t, "victim", clock), newLNode(t, "puppet", clock)
	room = attacker.create(label)
	if got := attacker.change(room, Change{Kind: core.EntryAdmin, Target: victim.id, Name: "victim", History: true}); got.Effect != EffectApplied {
		t.Fatalf("victim as admin: %+v", got)
	}
	victim.pin(room, attacker)
	victim.pull(room, attacker)
	if got := attacker.change(room, Change{Kind: core.EntryAdd, Target: puppet.id, Name: "helper", History: true}); got.Effect != EffectApplied {
		t.Fatalf("add puppet: %+v", got)
	}
	first := attacker.change(room, Change{Kind: core.EntryAdmin, Target: puppet.id, Name: name})
	if first.Effect != EffectPending || first.Reason != members.ReasonNeedsSecond {
		t.Fatalf("admin entry for puppet: %+v", first)
	}
	victim.pull(room, attacker)
	qs := victim.requests(room)
	if len(qs) != 1 || qs[0].Seconds != first.Entry || qs[0].Node != puppet.id {
		t.Fatalf("seconding request on victim: %+v", qs)
	}
	return attacker, victim, puppet, room, qs[0]
}

// F2 (security review 2026-11-25). A hostile room creator picks a label
// with a carriage return, an erase-line escape, a right-to-left override,
// a newline and a conceal escape. The label reaches the victim admin
// through the signed create entry. The text the victim approves, in the
// request and in its alert, shows every one of them escaped, and still
// names the real action.
func TestAMembershipRequestEscapesControlsAndBidiInTheRoomLabel(t *testing.T) {
	ctx := context.Background()
	hostile := "\r\x1b[2K\u202eok\nIf approved: nothing\x1b[8m"
	if len(hostile) > 64 {
		t.Fatalf("label is %d bytes, over the 64-byte limit", len(hostile))
	}
	_, victim, puppet, room, q := hostileRoom(t, hostile, "")

	// The label is stored as signed: display escapes it, receipt does not.
	lr, err := victim.st.LogRoom(ctx, room)
	if err != nil || lr.Label != hostile {
		t.Fatalf("victim stored label %q (%v), want the raw %q", lr.Label, err, hostile)
	}
	alerts, err := victim.r.Alerts(ctx)
	if err != nil || len(alerts) != 1 || alerts[0].Kind != AlertRequest {
		t.Fatalf("alerts on victim: %+v %v", alerts, err)
	}
	info, err := victim.r.Info(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	for what, s := range map[string]string{"request": requestText(q, lr.Label, info.Members), "alert": alerts[0].Text} {
		for _, c := range []string{"\r", "\x1b", "\u202e", "\n"} {
			if strings.Contains(s, c) {
				t.Errorf("%s text carries a raw %q: %q", what, c, s)
			}
		}
		for _, e := range []string{`\u{d}`, `\u{1b}[2K`, `\u{202e}`, `\u{a}`, `\u{1b}[8m`} {
			if !strings.Contains(s, e) {
				t.Errorf("%s text lacks %s: %q", what, e, s)
			}
		}
		if !strings.Contains(s, ", an admin, seconding another admin's entry") || !strings.Contains(s, Fingerprint(puppet.id)) {
			t.Errorf("%s text lacks the real action: %q", what, s)
		}
	}
}

// The honest case: a room label of ordinary letters, accents and an
// emoji is shown as it is.
func TestAMembershipRequestShowsAnOrdinaryRoomLabelAsItIs(t *testing.T) {
	ctx := context.Background()
	label := "équipe ops 🚀"
	_, victim, _, room, q := hostileRoom(t, label, "")
	info, err := victim.r.Info(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestText(q, label, info.Members); !strings.Contains(got, "room "+Isolate(label)+" ("+room+")") {
		t.Errorf("request text %q does not name room %q", got, label)
	}
}

// F45 (security review 2026-11-25). A seconding request carries the name
// the other admin wrote into its entry. The text says who chose that name
// and, when the room records the machine under another name, says that
// name too, so an admin cannot pass a sockpuppet off as a colleague. The
// view tells the page and the CLI who chose the name.
func TestASecondingRequestSaysTheOtherAdminChoseTheNameAndShowsTheRoomsName(t *testing.T) {
	ctx := context.Background()
	_, victim, _, room, q := hostileRoom(t, "ops", "alice-laptop")
	if q.Name != "alice-laptop" {
		t.Fatalf("request name %q", q.Name)
	}
	info, err := victim.r.Info(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	text := requestText(q, "ops", info.Members)
	if !strings.Contains(text, `named "`+Isolate("alice-laptop")+`" by the admin who signed the first entry`) ||
		!strings.Contains(text, `recorded in the room as "`+Isolate("helper")+`"`) {
		t.Errorf("request text %q does not say who named the machine and what the room records", text)
	}
	s := &Surface{m: victim.m, r: victim.r}
	v, err := s.Describe(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if v.NameFrom != NameFromAdmin || v.RecordedName != "helper" || v.Text != text {
		t.Errorf("view: name from %q, recorded %q, text %q", v.NameFrom, v.RecordedName, v.Text)
	}
}

// The honest case: a seconding request whose entry names no one shows the
// name the room records for the machine.
func TestASecondingRequestWithoutANameShowsTheRoomsName(t *testing.T) {
	ctx := context.Background()
	_, victim, _, room, q := hostileRoom(t, "ops", "")
	info, err := victim.r.Info(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	text := requestText(q, "ops", info.Members)
	if !strings.Contains(text, `(recorded in the room as "`+Isolate("helper")+`")`) || strings.Contains(text, "named") {
		t.Errorf("request text %q", text)
	}
	v, err := (&Surface{m: victim.m, r: victim.r}).Describe(ctx, q)
	if err != nil || v.NameFrom != "" || v.RecordedName != "helper" {
		t.Errorf("view %+v %v", v, err)
	}
}

// F45: a name that another member already has is flagged, so a
// sockpuppet named after this machine, or after a colleague's, is seen.
func TestASecondingRequestNamingAnotherMembersNameSaysSo(t *testing.T) {
	ctx := context.Background()
	_, victim, _, room, q := hostileRoom(t, "ops", "victim")
	info, err := victim.r.Info(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	text := requestText(q, "ops", info.Members)
	if want := "machine " + Fingerprint(victim.id) + " in the room has this name too"; !strings.Contains(text, want) {
		t.Errorf("request text %q lacks %q", text, want)
	}
}

// F2: the alert that this machine may have been removed from a room names
// the room by the label its creator chose, escaped, so `a2p2 alerts` and
// the MCP alert text carry no raw control or bidi character from it.
func TestARoomThatMayHaveRemovedThisMachineIsAlertedWithItsLabelEscaped(t *testing.T) {
	ctx := context.Background()
	hostile := "\r\x1b[2K\u202eok\nfine"
	attacker, victim, puppet, room, _ := hostileRoom(t, hostile, "")
	s, err := OpenSurface(ctx, victim.m, victim.r, SurfaceOpts{Name: "victim"})
	if err != nil {
		t.Fatal(err)
	}
	long := victim.m.now().Add(-MaybeRemovedAfter - time.Minute)
	s.notMb = func(string) map[string]time.Time { return map[string]time.Time{attacker.id: long, puppet.id: long} }
	as, err := s.Alerts(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range as {
		if a.Kind != AlertRemoved || a.Room != room {
			continue
		}
		found = true
		for _, c := range []string{"\r", "\x1b", "\u202e", "\n"} {
			if strings.Contains(a.Text, c) {
				t.Errorf("removed alert carries a raw %q: %q", c, a.Text)
			}
		}
		if !strings.Contains(a.Text, "room "+Isolate(EscapeDisplay(hostile))+": ") {
			t.Errorf("removed alert %q does not name the room escaped", a.Text)
		}
	}
	if !found {
		t.Fatalf("no removed alert for %s: %+v", room, as)
	}
}

// F45: the members a request's text names can change between the page
// showing it and the person's approval. An approval carrying the text
// shown is compared again under the decision lock: after another machine
// named like the target joins, the old text is refused and nothing is
// signed; the new text is approved.
func TestAnApprovalOfARequestWhoseTextChangedSinceItWasShownIsRefused(t *testing.T) {
	ctx := context.Background()
	attacker, victim, _, room, q := hostileRoom(t, "ops", "")
	info, err := victim.r.Info(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	shown := requestText(q, "ops", info.Members)
	other := newLNode(t, "other", attacker.clock)
	if got := attacker.change(room, Change{Kind: core.EntryAdd, Target: other.id, Name: "Helper"}); got.Effect != EffectApplied {
		t.Fatalf("add other: %+v", got)
	}
	victim.pull(room, attacker)
	info, err = victim.r.Info(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	now := requestText(q, "ops", info.Members)
	if now == shown || !strings.Contains(now, "machine "+Fingerprint(other.id)+" in the room has this name too") {
		t.Fatalf("text after the join %q (before %q)", now, shown)
	}
	if _, err := victim.r.ConfirmWith(ctx, q.ID, true, ConfirmOpts{Shown: shown}); err == nil || err.Error() != RequestChangedText {
		t.Fatalf("approval with the old text: %v", err)
	}
	if got, err := victim.st.GetRequest(ctx, q.ID); err != nil || got.State != store.RequestPending {
		t.Fatalf("request after the refused approval: %+v %v", got, err)
	}
	c, err := victim.r.ConfirmWith(ctx, q.ID, true, ConfirmOpts{Shown: now})
	if err != nil || c.State == store.RequestPending || c.State == store.RequestFailed {
		t.Fatalf("approval with the text shown now: %+v %v", c, err)
	}
}

// F45: the note on other members with the name shown is bounded, so a
// room where many machines share a name cannot make a request's text too
// long to approve, and a nameless seconding request flags other members
// with the name the room records.
func TestTheSameNameNoteIsBoundedAndCoversTheRecordedName(t *testing.T) {
	target := strings.Repeat("a", 64)
	members := []MemberInfo{{Node: target, Name: "helper"}}
	for i := range 5000 {
		members = append(members, MemberInfo{Node: strings.Repeat(string(rune('b'+i%20)), 60) + strconv.Itoa(1000+i), Name: "HELPER"})
	}
	q := store.Request{Kind: "admin", Action: "promote", Room: "r_x", Node: target, Seconds: "e_1", Role: "admin"}
	text := requestText(q, "ops", members)
	if len(text) > 1024 || !strings.Contains(text, `recorded in the room as "`+Isolate("helper")+`"`) ||
		!strings.Contains(text, " and 4997 more in the room have this name too") {
		t.Errorf("request text, %d bytes: %q", len(text), text)
	}
}
