package node

// D73: a registration looks only at the newest store.RegisterWindow
// records this machine holds, so the late agent's first reads are bounded
// by the same window; a brief whose text is not here holds back only its
// own stream, and one whose text expired is a notice that blocks nothing.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

func preregPeer() ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("prereg peer"))
	return ed25519.NewKeyFromSeed(seed[:])
}

// chain puts records of peer's stream e1 from seq from to to, each with
// the fields f gives it, and returns the id of the last.
func (r *rig) chain(peer ed25519.PrivateKey, prev string, from, to int64, f func(n int64) core.RecordFields) string {
	r.t.Helper()
	for n := from; n <= to; n++ {
		x := f(n)
		x.Epoch, x.Seq, x.Prev, x.Kind, x.Agent = "00000000000000f1", n, prev, core.KindMessage, "lead"
		if x.Text == "" {
			x.Text = "history"
		}
		prev = r.put(peer, x).ID()
	}
	return prev
}

// [U3, A2] A late agent whose brief is the oldest record of the window,
// with the whole window after it room-wide history: its first inbox, a
// level change and Skipped each decode about RegisterWindow records, not
// the history before, and show only the brief.
func TestALateAgentsFirstReadsDecodeAtMostTheRegistrationWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("puts store.RegisterWindow records")
	}
	r := newRig(t)
	ctx := context.Background()
	peer := preregPeer()
	n := int64(store.RegisterWindow)
	r.chain(peer, "", 1, n+1, func(i int64) core.RecordFields {
		if i == 2 {
			return core.RecordFields{Text: "brief", To: r.to("w1")}
		}
		return core.RecordFields{}
	})
	r.register("w1")
	var kept atomic.Int64
	keepHook = func() { kept.Add(1) }
	defer func() { keepHook = nil }()
	measure := func(what string, fn func()) {
		t.Helper()
		kept.Store(0)
		at := time.Now()
		fn()
		t.Logf("%s: %d records decoded in %v", what, kept.Load(), time.Since(at))
		if k := kept.Load(); k > n+10 {
			t.Fatalf("%s decoded %d records, the window is %d", what, k, n)
		}
	}
	measure("first inbox", func() {
		v, err := r.m.Inbox(ctx, mRoom, "w1")
		if err != nil {
			t.Fatal(err)
		}
		wantKinds(t, v.Entries, "message:brief")
	})
	measure("level addressed", func() {
		if err := r.m.SetLevel(ctx, mRoom, "w1", store.LevelAddressed); err != nil {
			t.Fatal(err)
		}
	})
	measure("level none", func() {
		if err := r.m.SetLevel(ctx, mRoom, "w1", store.LevelNone); err != nil {
			t.Fatal(err)
		}
	})
	measure("skipped", func() {
		es, err := r.m.Skipped(ctx, mRoom, "w1")
		if err != nil {
			t.Fatal(err)
		}
		wantKinds(t, es, "message:brief")
	})
}

// [R7, D2] A brief whose text this machine does not hold yet, at the
// position where the late agent's time begins in its author's stream,
// holds back that stream alone, exactly as a missing text does after
// registration: what other authors wrote is read now; the author's later
// messages wait behind the brief and come with it when its text arrives.
// The model agrees (TestABriefWhoseTextIsNotHereYetHoldsBackOnlyItsAuthorsStream).
func TestABriefWhoseTextIsNotHereYetHoldsBackOnlyItsOwnStream(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	peer := preregPeer()
	first := r.chain(peer, "", 1, 1, func(int64) core.RecordFields { return core.RecordFields{} })
	full, err := core.NewRecord(peer, core.RecordFields{Room: mRoom, Epoch: "00000000000000f1", Seq: 2, Prev: first,
		Kind: core.KindMessage, Agent: "lead", Created: r.now, Text: "brief", To: r.to("w1")})
	if err != nil {
		t.Fatal(err)
	}
	header, err := full.WithoutBody().Verified()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := r.st.PutRecord(ctx, header, store.PutOpts{Now: r.now, MayRead: true}); err != nil || out != store.OutcomeStored {
		t.Fatalf("put the header: %s %v", out, err)
	}
	third := r.chain(peer, header.ID(), 3, 3, func(int64) core.RecordFields { return core.RecordFields{} })
	r.register("w1")
	r.send("alice", Draft{Text: "local after"})
	r.chain(peer, third, 4, 4, func(int64) core.RecordFields { return core.RecordFields{Text: "remote after"} })
	v, err := r.m.Inbox(ctx, mRoom, "w1")
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, v.Entries, "message:local after")
	if len(v.Waiting) != 1 || v.Waiting[0].Reason != WaitText || v.Waiting[0].At != header.ID() {
		t.Fatalf("waiting %+v", v.Waiting)
	}
	wantKinds(t, r.read("w1"), "message:local after")
	body, err := full.Verified()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.PutRecord(ctx, body, store.PutOpts{Now: r.now, MayRead: true}); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, r.read("w1"), "message:brief", "message:remote after")
}
