package node

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wallter/a2p2/internal/store"
)

// [U4] FitPage takes the longest prefix whose size fits, for random
// sizes, as a scan from the start would, and gives the size of the first
// alone when not even it fits, in a handful of measures.
func TestFitPageTakesTheLongestPrefixThatFitsAndNeverSkipsTheFirst(t *testing.T) {
	r := rand.New(rand.NewPCG(70, 3))
	for i := 0; i < 5000; i++ {
		count := r.IntN(40)
		sizes := make([]int, count+1)
		sizes[0] = 200 + r.IntN(100)
		for k := 1; k <= count; k++ {
			sizes[k] = sizes[k-1] + 1 + r.IntN([]int{50, 2000, 50000}[r.IntN(3)])
		}
		max := MinInboxBytes + r.IntN(MaxInboxBytes-MinInboxBytes+1)
		calls := 0
		n, need := FitPage(count, max, func(n int) int { calls++; return sizes[n] })
		want := 0
		for want < count && sizes[want+1] <= max {
			want++
		}
		if n != want {
			t.Fatalf("sizes %v max %d: %d, want %d", sizes, max, n, want)
		}
		if wantNeed := 0; n == 0 && count > 0 {
			wantNeed = sizes[1]
			if need != wantNeed {
				t.Fatalf("need %d, want %d", need, wantNeed)
			}
		}
		if calls > 16 {
			t.Fatalf("%d sizes measured for %d entries", calls, count)
		}
	}
}

// pageSize is a stand-in for the MCP result's size: an envelope, each
// entry's text and a fixed share, a cut entry's sentence, more's digits
// and each level set.
func pageSize(v InboxView) int {
	n := 200 + len(fmt.Sprint(v.More)) + 100*len(v.LevelsSet)
	for _, e := range v.Entries {
		n += 150 + len(e.Text)
		if e.TextCut > 0 {
			n += 100 + len(fmt.Sprint(e.TextCut))
		}
	}
	return n
}

// [D3, U4, R7] A page cut by max_bytes keeps a receipt of what it returned
// only: Seen confirms those and refuses the rest as not shown, which the
// next page offers again; More counts what is left out by either limit.
func TestAPageCutByItsBudgetKeepsNoReceiptOfTheRest(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice, bob, carol := e.agent("alice"), e.agent("bob"), e.agent("carol")
	for i := 0; i < 10; i++ {
		e.send(SendInput{Agent: bob, Text: fmt.Sprintf("m%d %s", i, strings.Repeat("x", 300))})
	}
	all := e.inbox(alice) // a listing of everything, confirming nothing
	o := InboxOpts{MaxBytes: 1500, Size: pageSize}
	v, err := e.s.InboxPage(ctx, alice, "", o)
	if err != nil || len(v.Entries) != 2 || v.More != 8 {
		t.Fatalf("page %d more %d %v", len(v.Entries), v.More, err)
	}
	// What the listing of everything showed has receipts; a fresh agent's
	// page shows what has none.
	v, err = e.s.InboxPage(ctx, carol, "", o)
	if err != nil || len(v.Entries) != 2 {
		t.Fatalf("carol's page %d %v", len(v.Entries), err)
	}
	var reads []store.Read
	for _, x := range all {
		reads = append(reads, store.Read{Key: x.Key, Version: x.Version})
	}
	res, err := e.s.Seen(ctx, carol, reads)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range res {
		if r.Confirmed != (i < 2) || !r.Confirmed && r.Reason != SeenNotShown {
			t.Fatalf("entry %d confirmed %v (%s), want only the page's two", i, r.Confirmed, r.Reason)
		}
	}
	v, err = e.s.InboxPage(ctx, carol, "", InboxOpts{Limit: 3, MaxBytes: MaxInboxBytes, Size: pageSize})
	if err != nil || len(v.Entries) != 3 || v.Entries[0].Text[:2] != "m2" || v.More != 5 {
		t.Fatalf("next page %d more %d %v", len(v.Entries), v.More, err)
	}
	for _, bad := range []InboxOpts{{MaxBytes: MinInboxBytes - 1, Size: pageSize}, {MaxBytes: MaxInboxBytes + 1, Size: pageSize}} {
		if _, err := e.s.InboxPage(ctx, carol, "", bad); !isUser(err) {
			t.Fatalf("max_bytes %d: %v", bad.MaxBytes, err)
		}
	}
	if _, err := e.s.InboxPage(ctx, carol, "", InboxOpts{MaxBytes: 1000}); err == nil || isUser(err) {
		t.Fatalf("a budget without a size: %v", err)
	}
	if _, err := e.s.InboxPage(ctx, carol, "", InboxOpts{MaxBytes: 600, Size: pageSize}); !isUser(err) {
		t.Fatalf("the first does not fit: %v", err)
	}
}

// refusedFor is the refusal of err, or a failure if it is not too_large.
func refusedFor(t *testing.T, err error) string {
	t.Helper()
	ue, ok := err.(*UserError)
	if !ok || ue.CodeOf() != ErrorTooLarge {
		t.Fatalf("want too_large, got %v", err)
	}
	return ue.Msg
}

// [U2, U4, D70] A read that sets a level and is refused for its budget
// changes nothing: the level stays, nothing is marked skipped and nothing
// gets a receipt. When the list of rooms set alone does not fit, the
// refusal says to set the level in a call of its own. A read that fits
// sets the level and returns the page as it is at that level.
func TestAReadThatSetsALevelAndIsRefusedForItsBudgetChangesNothing(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	to := []string{alice}
	big := e.send(SendInput{Agent: bob, To: to, Text: strings.Repeat("b", 2000)})
	e.send(SendInput{Agent: bob, Text: "room-wide, not marked"})
	levelOf := func() string {
		t.Helper()
		ls, err := e.s.Levels(ctx, alice, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return ls[0].Level
	}
	_, err := e.s.InboxPage(ctx, alice, "", InboxOpts{Level: store.LevelAddressed, MaxBytes: 1000, Size: pageSize})
	if msg := refusedFor(t, err); !strings.Contains(msg, "nothing was marked seen") || strings.Contains(msg, "level was set") {
		t.Fatalf("refusal %q", msg)
	}
	if l := levelOf(); l != store.LevelAll {
		t.Fatalf("level %s after a refused read, want all", l)
	}
	if sk, err := e.s.Skipped(ctx, alice, "", "", 0); err != nil || len(sk) != 0 {
		t.Fatalf("skipped after a refused read: %d %v", len(sk), err)
	}
	if res, err := e.s.Seen(ctx, alice, []store.Read{{Key: big.ID, Version: big.ID}}); err != nil || res[0].Confirmed {
		t.Fatalf("a refused read kept a receipt: %+v %v", res, err)
	}
	// Three more rooms: the list of four rooms set does not fit 512.
	for _, l := range []string{"r1", "r2", "r3"} {
		if _, err := e.s.CreateRoom(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	_, err = e.s.InboxPage(ctx, alice, "", InboxOpts{Level: store.LevelNone, MaxBytes: MinInboxBytes, Size: pageSize})
	if msg := refusedFor(t, err); !strings.Contains(msg, "4 rooms") || !strings.Contains(msg, "a call of its own") {
		t.Fatalf("refusal %q", msg)
	}
	if l := levelOf(); l != store.LevelAll {
		t.Fatalf("level %s after a refused read, want all", l)
	}
	// Fitting: the level is set, and the room-wide post it keeps out is
	// not on the page.
	v, err := e.s.InboxPage(ctx, alice, "", InboxOpts{Level: store.LevelAddressed, MaxBytes: 4000, Size: pageSize})
	if err != nil || len(v.Entries) != 1 || v.Entries[0].ID != big.ID || len(v.LevelsSet) != 4 || v.More != 0 {
		t.Fatalf("page %d more %d levels %d %v", len(v.Entries), v.More, len(v.LevelsSet), err)
	}
	if l := levelOf(); l != store.LevelAddressed {
		t.Fatalf("level %s, want addressed", l)
	}
	if res, err := e.s.Seen(ctx, alice, []store.Read{{Key: big.ID, Version: big.ID}}); err != nil || !res[0].Confirmed {
		t.Fatalf("the page's entry: %+v %v", res, err)
	}
	if sk, err := e.s.Skipped(ctx, alice, "", "", 0); err != nil || len(sk) != 1 {
		t.Fatalf("skipped: %d %v", len(sk), err)
	}
}

// escapedSize measures text as the tool writes it, escaped and quoted,
// three times over, so that a 16 KiB text fits no budget.
func escapedSize(v InboxView) int {
	n := 300 + len(fmt.Sprint(v.More))
	for _, e := range v.Entries {
		b, _ := json.Marshal(EscapeDisplayText(e.Text))
		n += 3 * len(b)
		if e.TextCut > 0 {
			n += 150 + len(fmt.Sprint(e.TextCut))
		}
	}
	return n
}

// [U4, D70] An oldest entry larger than any budget is returned at the
// ceiling with its text cut between characters, never inside an escape
// or a character of several bytes, as long as fits, and says how many
// bytes of the rendering were left out; below the ceiling the refusal
// names it. Seen confirms the cut entry as it is.
func TestAnOldestEntryLargerThanAnyBudgetIsCutBetweenCharacters(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	var b strings.Builder
	for b.Len() < 16000 {
		b.WriteString("é\x7f😀\u200bx")
	}
	sent := e.send(SendInput{Agent: bob, Text: b.String()})
	e.send(SendInput{Agent: bob, Text: "after it"})
	_, err := e.s.InboxPage(ctx, alice, "", InboxOpts{MaxBytes: MaxInboxBytes - 1, Size: escapedSize})
	if msg := refusedFor(t, err); !strings.Contains(msg, fmt.Sprintf("max_bytes %d to read it with its text cut", MaxInboxBytes)) {
		t.Fatalf("refusal %q", msg)
	}
	v, err := e.s.InboxPage(ctx, alice, "", InboxOpts{MaxBytes: MaxInboxBytes, Size: escapedSize})
	if err != nil || len(v.Entries) != 1 || v.More != 1 {
		t.Fatalf("page %d more %d %v", len(v.Entries), v.More, err)
	}
	c := v.Entries[0]
	if !utf8.ValidString(c.Text) || !strings.HasPrefix(b.String(), c.Text) || len(c.Text) == 0 || len(c.Text) >= b.Len() {
		t.Fatalf("cut text of %d bytes, not a start of the text between characters", len(c.Text))
	}
	if got := escapedSize(v); got > MaxInboxBytes {
		t.Fatalf("result %d bytes over %d", got, MaxInboxBytes)
	}
	full, _ := json.Marshal(EscapeDisplayText(b.String()))
	cut, _ := json.Marshal(EscapeDisplayText(c.Text))
	if c.TextCut != 3*(len(full)-len(cut)) {
		t.Fatalf("text_cut %d, want %d", c.TextCut, 3*(len(full)-len(cut)))
	}
	// One character more would not fit.
	_, w := utf8.DecodeRuneInString(b.String()[len(c.Text):])
	longer := c
	longer.Text = b.String()[:len(c.Text)+w]
	if escapedSize(InboxView{Entries: []Entry{longer}, More: 2}) <= MaxInboxBytes {
		t.Fatalf("the cut leaves out a character that fits")
	}
	if res, err := e.s.Seen(ctx, alice, []store.Read{{Key: c.Key, Version: c.Version}}); err != nil || !res[0].Confirmed || c.Key != sent.ID {
		t.Fatalf("the cut entry: %+v %v", res, err)
	}
}

// [D3, D7, U4] Messages whose text expires between two pages: the first
// page confirms what it returned, and the next pages show the rest as
// expired, confirmed in turn, with More counting down to nothing; none
// is lost or shown twice.
func TestMessagesThatExpireBetweenPagesComeAsExpiredInTheNextPages(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	at := time.Now()
	e.now = func() time.Time { return at }
	e.restart()
	alice, bob := e.agent("alice"), e.agent("bob")
	sent := map[string]bool{}
	for i := 0; i < 6; i++ {
		sent[e.send(SendInput{Agent: bob, Text: fmt.Sprintf("m%d %s", i, strings.Repeat("x", 300))}).ID] = true
	}
	o := InboxOpts{MaxBytes: 1500, Size: pageSize}
	kinds := map[string]string{}
	for calls := 0; calls < 10; calls++ {
		if calls == 1 {
			at = at.Add(store.Retention + time.Hour)
		}
		v, err := e.s.InboxPage(ctx, alice, "", o)
		if err != nil {
			t.Fatal(err)
		}
		if len(v.Entries) == 0 {
			break
		}
		var reads []store.Read
		for _, x := range v.Entries {
			if _, dup := kinds[x.ID]; dup {
				t.Fatalf("%s shown twice", x.ID)
			}
			kinds[x.ID] = x.Kind
			reads = append(reads, store.Read{Key: x.Key, Version: x.Version})
		}
		res, err := e.s.Seen(ctx, alice, reads)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res {
			if !r.Confirmed {
				t.Fatalf("%s not confirmed: %s", r.Key, r.Reason)
			}
		}
		if left := len(sent) - len(kinds); v.More != left {
			t.Fatalf("more %d, want %d", v.More, left)
		}
	}
	expired := 0
	for id := range sent {
		switch kinds[id] {
		case EntryMessage:
		case EntryExpired:
			expired++
		default:
			t.Fatalf("%s came as %q", id, kinds[id])
		}
	}
	if expired != len(sent)-2 {
		t.Fatalf("%d expired, want %d: the first page held two", expired, len(sent)-2)
	}
}

// growingSize is pageSize plus extra bytes once the read has started to
// wait: a field of the result (streams still fetching, more) that grows
// while it waits.
func growingSize(e *surfEnv, extra int) func(InboxView) int {
	grown := false
	e.s.waiting = func() { grown = true }
	return func(v InboxView) int {
		if grown {
			return pageSize(v) + extra
		}
		return pageSize(v)
	}
}

// [U4, D72] A wait that ends with nothing returns a view sized like any
// other: when what it shows grew past max_bytes while it waited, the read
// is refused as a too small budget is, not answered over the budget.
func TestAWaitThatEndsWithNothingStaysWithinItsBudget(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice := e.agent("alice")
	o := InboxOpts{Wait: 150 * time.Millisecond, MaxBytes: MinInboxBytes, Size: growingSize(e, 400)}
	_, err := e.s.InboxPage(ctx, alice, "", o)
	if msg := refusedFor(t, err); !strings.Contains(msg, "before any message") {
		t.Fatalf("refusal %q", msg)
	}
	e.s.waiting = nil
	if v, err := e.s.InboxPage(ctx, alice, "", InboxOpts{Wait: 50 * time.Millisecond, MaxBytes: MinInboxBytes, Size: pageSize}); err != nil || len(v.Entries) != 0 {
		t.Fatalf("a wait within its budget: %d %v", len(v.Entries), err)
	}
}

// [U2, U4, D72] A read that sets a level fits at first, then waits, and
// what it shows grows past max_bytes while it waits: it is refused, saying
// the level was set, and returns nothing over the budget.
func TestAReadThatSetsALevelAndWaitsStaysWithinItsBudget(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice := e.agent("alice")
	o := InboxOpts{Level: store.LevelAddressed, Wait: 150 * time.Millisecond, MaxBytes: 600, Size: growingSize(e, 400)}
	_, err := e.s.InboxPage(ctx, alice, "", o)
	if msg := refusedFor(t, err); !strings.Contains(msg, "the level was set") {
		t.Fatalf("refusal %q", msg)
	}
}

// [U2, U4, D72] The list of rooms a read sets fits alone, but with it the
// oldest entry fits no budget even cut to nothing: the refusal says to set
// the level in a call of its own, and changes nothing; the read without
// the level then returns the entry.
func TestLevelsThatLeaveNoRoomForTheFirstEntrySayToSetTheLevelAlone(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	big := e.send(SendInput{Agent: bob, To: []string{alice}, Text: strings.Repeat("b", 16000)})
	size := func(v InboxView) int {
		n := pageSize(v)
		if len(v.LevelsSet) > 0 {
			n += MaxInboxBytes - 400
		}
		return n
	}
	_, err := e.s.InboxPage(ctx, alice, "", InboxOpts{Level: store.LevelAddressed, MaxBytes: MaxInboxBytes, Size: size})
	if msg := refusedFor(t, err); !strings.Contains(msg, "a call of its own") || !strings.Contains(msg, "nothing was changed") {
		t.Fatalf("refusal %q", msg)
	}
	if ls, err := e.s.Levels(ctx, alice, "", ""); err != nil || ls[0].Level != store.LevelAll {
		t.Fatalf("level after a refused read: %+v %v", ls, err)
	}
	v, err := e.s.InboxPage(ctx, alice, "", InboxOpts{MaxBytes: MaxInboxBytes, Size: size})
	if err != nil || len(v.Entries) != 1 || v.Entries[0].ID != big.ID || v.Entries[0].TextCut != 0 {
		t.Fatalf("without the level: %d %v", len(v.Entries), err)
	}
}

// [U4, D74] A reader's ceiling below MaxInboxBytes takes its place: an
// oldest entry that fits no budget up to it is refused naming it below
// it, and at it comes alone with its text cut to fit, confirmed by Seen.
func TestAnOldestEntryLargerThanTheReadersCeilingIsCutAtIt(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	text := strings.Repeat("\u200b", 1000)
	sent := e.send(SendInput{Agent: bob, Text: text})
	e.send(SendInput{Agent: bob, Text: "after it"})
	if escapedSize(InboxView{Entries: []Entry{{Text: text}}}) > MaxInboxBytes {
		t.Fatal("the text does not fit the general ceiling")
	}
	_, err := e.s.InboxPage(ctx, alice, "", InboxOpts{MaxBytes: 6999, Ceiling: 7000, Size: escapedSize})
	if msg := refusedFor(t, err); !strings.Contains(msg, "max_bytes 7000 to read it with its text cut") {
		t.Fatalf("refusal %q", msg)
	}
	if _, err := e.s.InboxPage(ctx, alice, "", InboxOpts{MaxBytes: 7001, Ceiling: 7000, Size: escapedSize}); err == nil {
		t.Fatal("a budget above the ceiling was taken")
	}
	v, err := e.s.InboxPage(ctx, alice, "", InboxOpts{MaxBytes: 7000, Ceiling: 7000, Size: escapedSize})
	if err != nil || len(v.Entries) != 1 || v.More != 1 || v.Entries[0].TextCut == 0 || len(v.Entries[0].Text) == 0 {
		t.Fatalf("page %d more %d %v", len(v.Entries), v.More, err)
	}
	if got := escapedSize(v); got > 7000 {
		t.Fatalf("result %d bytes over the ceiling 7000", got)
	}
	c := v.Entries[0]
	if res, err := e.s.Seen(ctx, alice, []store.Read{{Key: c.Key, Version: c.Version}}); err != nil || !res[0].Confirmed || c.Key != sent.ID {
		t.Fatalf("the cut entry: %+v %v", res, err)
	}
}

// [U4, D74] An entry whose fields besides its text pass the reader's
// ceiling comes alone, with no text and TextCut set, over the ceiling but
// within MaxInboxBytes, and is confirmed; below the ceiling it is refused
// naming it; one wider than MaxInboxBytes is refused, ceiling or not.
func TestAnEntryWhoseFieldsPassTheReadersCeilingComesAloneWithoutItsText(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	sent := e.send(SendInput{Agent: bob, Text: "its fields are wide"})
	e.send(SendInput{Agent: bob, Text: "after it"})
	// Every entry's fields take 3,000 bytes besides its text.
	wide := func(v InboxView) int { return escapedSize(v) + 3000*len(v.Entries) }
	_, err := e.s.InboxPage(ctx, alice, "", InboxOpts{MaxBytes: 1999, Ceiling: 2000, Size: wide})
	if msg := refusedFor(t, err); !strings.Contains(msg, "max_bytes 2000") {
		t.Fatalf("refusal %q", msg)
	}
	huge := func(v InboxView) int { return escapedSize(v) + 40000*len(v.Entries) }
	for _, o := range []InboxOpts{{MaxBytes: MaxInboxBytes, Size: huge}, {MaxBytes: 2000, Ceiling: 2000, Size: huge}} {
		if _, err := e.s.InboxPage(ctx, alice, "", o); err == nil {
			t.Fatalf("an entry wider than the general ceiling was returned, ceiling %d", o.Ceiling)
		}
	}
	v, err := e.s.InboxPage(ctx, alice, "", InboxOpts{MaxBytes: 2000, Ceiling: 2000, Size: wide})
	if err != nil || len(v.Entries) != 1 || v.More != 1 || v.Entries[0].Key != sent.ID || v.Entries[0].Text != "" || v.Entries[0].TextCut == 0 {
		t.Fatalf("page %+v %v", v.Entries, err)
	}
	if got := wide(v); got <= 2000 || got > MaxInboxBytes {
		t.Fatalf("result %d bytes", got)
	}
	if res, err := e.s.Seen(ctx, alice, []store.Read{{Key: sent.ID, Version: v.Entries[0].Version}}); err != nil || !res[0].Confirmed {
		t.Fatalf("seen %+v %v", res, err)
	}
}

// [U2, U4, D74] A read sets a level, its empty page fits and the level is
// set, and it waits; an entry that arrives then fits alone but not beside
// the list of rooms set. The read returns it whole without that list
// (the level is set), not cut and not stripped of its text; an entry
// that fits no budget even alone is cut with no list, as much text as
// fits.
func TestAnEntryArrivingDuringALevelReadsWaitComesWholeWithoutTheLevelList(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice, bob := e.agent("alice"), e.agent("bob")
	size := func(v InboxView) int {
		n := pageSize(v)
		if len(v.LevelsSet) > 0 {
			n += 1500
		}
		return n
	}
	sendWhileWaiting := func(text string) <-chan Sent {
		c := make(chan Sent, 1)
		e.s.waiting = func() {
			go func() {
				s, err := e.s.Send(ctx, SendInput{Agent: bob, To: []string{alice}, Text: text})
				if err != nil {
					t.Error(err)
				}
				c <- s
			}()
		}
		return c
	}
	sentc := sendWhileWaiting(strings.Repeat("w", 400))
	v, err := e.s.InboxPage(ctx, alice, "", InboxOpts{Level: store.LevelAddressed, Wait: 5 * time.Second, MaxBytes: 2000, Ceiling: 2000, Size: size})
	sent := <-sentc
	e.s.waiting = nil
	if err != nil || len(v.Entries) != 1 || v.Entries[0].ID != sent.ID || v.Entries[0].TextCut != 0 || len(v.Entries[0].Text) != 400 || len(v.LevelsSet) != 0 {
		t.Fatalf("page %+v levels %d: %v", v.Entries, len(v.LevelsSet), err)
	}
	if got := size(v); got > 2000 {
		t.Fatalf("result %d bytes over 2000", got)
	}
	if ls, err := e.s.Levels(ctx, alice, "", ""); err != nil || ls[0].Level != store.LevelAddressed {
		t.Fatalf("level %+v %v", ls, err)
	}
	if _, err := e.s.Seen(ctx, alice, []store.Read{{Key: v.Entries[0].Key, Version: v.Entries[0].Version}}); err != nil {
		t.Fatal(err)
	}

	sentc = sendWhileWaiting(strings.Repeat("x", 3000))
	v, err = e.s.InboxPage(ctx, alice, "", InboxOpts{Level: store.LevelAddressed, Wait: 5 * time.Second, MaxBytes: 2000, Ceiling: 2000, Size: size})
	<-sentc
	e.s.waiting = nil
	if err != nil || len(v.Entries) != 1 || v.Entries[0].TextCut == 0 || len(v.Entries[0].Text) < 1000 || len(v.LevelsSet) != 0 || size(v) > 2000 {
		t.Fatalf("too large alone: %+v levels %d: %v", v.Entries, len(v.LevelsSet), err)
	}
}

// [U4, D74] A ceiling out of range is refused whatever max_bytes is, and
// an error repeats at most MaxErrorEcho bytes of a caller's input.
func TestACeilingOutOfRangeIsRefusedAndAnErrorEchoesAShortInput(t *testing.T) {
	ctx := context.Background()
	e := newSurf(t)
	alice := e.agent("alice")
	for _, c := range []int{-1, 1, MinInboxBytes - 1, MaxInboxBytes + 1} {
		for _, mb := range []int{0, MinInboxBytes} {
			if _, err := e.s.InboxPage(ctx, alice, "", InboxOpts{MaxBytes: mb, Ceiling: c, Size: pageSize}); err == nil {
				t.Fatalf("ceiling %d with max_bytes %d was taken", c, mb)
			}
		}
	}
	if _, err := e.s.InboxPage(ctx, alice, "", InboxOpts{MaxBytes: 600, Ceiling: 599, Size: pageSize}); err == nil {
		t.Fatal("a ceiling under max_bytes was taken")
	}
	long := strings.Repeat("é", 40000)
	_, err := e.s.InboxPage(ctx, alice, long, InboxOpts{})
	ue, ok := err.(*UserError)
	if !ok || len(ue.Msg) > MaxErrorEcho+100 || !utf8.ValidString(ue.Msg) || !strings.Contains(ue.Msg, "more bytes") {
		t.Fatalf("a room of %d bytes echoed: %v", len(long), err)
	}
}
