package node

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/store"
)

// [U4] FitPage takes the longest prefix whose size fits, for random
// sizes, as a scan from the start would; refuses with the size needed
// when even the first does not fit; and returns an entry larger than any
// budget alone at the ceiling only.
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
		n, err := FitPage(count, max, func(n int) int { calls++; return sizes[n] })
		want := 0
		for want < count && sizes[want+1] <= max {
			want++
		}
		switch {
		case count > 0 && want == 0 && sizes[1] > MaxInboxBytes && max == MaxInboxBytes:
			want = 1
		case count > 0 && want == 0:
			var ue *UserError
			if !errors.As(err, &ue) || ue.CodeOf() != ErrorTooLarge {
				t.Fatalf("first of %d is %d bytes, max %d: %d, %v", count, sizes[1], max, n, err)
			}
			need := min(sizes[1], MaxInboxBytes)
			if !strings.Contains(ue.Msg, fmt.Sprintf("max_bytes %d", need)) || !strings.Contains(ue.Msg, "nothing was marked seen") {
				t.Fatalf("refusal %q, want it to name %d", ue.Msg, need)
			}
			continue
		}
		if err != nil || n != want {
			t.Fatalf("sizes %v max %d: %d %v, want %d", sizes, max, n, err, want)
		}
		if calls > 16 {
			t.Fatalf("%d sizes measured for %d entries", calls, count)
		}
	}
	if n, err := FitPage(1, MaxInboxBytes, func(int) int { return 3 * MaxInboxBytes }); n != 1 || err != nil {
		t.Fatalf("an entry larger than any budget at the ceiling: %d %v", n, err)
	}
}

// pageSize is a stand-in for the MCP result's size: an envelope, each
// entry's text and a fixed share, and more's digits.
func pageSize(page []Entry, _ []Wait, more int) int {
	n := 200 + len(fmt.Sprint(more))
	for _, e := range page {
		n += 150 + len(e.Text)
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
