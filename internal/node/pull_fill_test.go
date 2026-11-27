package node

// Synthetic writers and a fast fill of a room, for the page bound test
// and the benchmark (pull_bench_test.go).

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// benchAuthors are the synthetic writers of the room.
type benchAuthors struct {
	keys  []ed25519.PrivateKey
	ids   []string
	epoch []string
	last  []*core.VerifiedRecord
	seq   []int64
}

func newBenchAuthors(n int) *benchAuthors {
	b := &benchAuthors{}
	for i := 0; i < n; i++ {
		seed := sha256.Sum256([]byte(fmt.Sprintf("bench author %d", i)))
		k := ed25519.NewKeyFromSeed(seed[:])
		b.keys = append(b.keys, k)
		b.ids = append(b.ids, hex.EncodeToString(k.Public().(ed25519.PublicKey)))
		b.epoch = append(b.epoch, fmt.Sprintf("%016x", 0xbe00+i))
		b.last = append(b.last, nil)
		b.seq = append(b.seq, 0)
	}
	return b
}

// next is author i's next record in room, dated at.
func (b *benchAuthors) next(t testing.TB, room string, i int, at time.Time) *core.VerifiedRecord {
	b.seq[i]++
	f := core.RecordFields{Room: room, Epoch: b.epoch[i], Seq: b.seq[i], Kind: core.KindMessage, Agent: "agent",
		Text: strings.Repeat("x", 200), Created: at}
	if b.last[i] != nil {
		f.Prev = b.last[i].ID()
	}
	r, err := core.NewRecord(b.keys[i], f)
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.Verified()
	if err != nil {
		t.Fatal(err)
	}
	b.last[i] = v
	return v
}

// benchFill places recs on n in transactions of up to 1,000.
func benchFill(t testing.TB, n *lnode, room string, recs []*core.VerifiedRecord) {
	ctx := context.Background()
	for len(recs) > 0 {
		k := min(len(recs), 1000)
		batch := recs[:k]
		recs = recs[k:]
		n.r.mu.Lock()
		rl := n.r.rooms[room]
		now := n.m.now()
		_, err := n.st.Exchange(ctx, room, 0, func(tx *store.Tx) error {
			for _, v := range batch {
				out, err := tx.PutRecordPlanned(v, 0, n.r.recordPlan(rl.st, rl.lookup(nil), v, now))
				if err != nil {
					return err
				}
				if out != store.OutcomeStored {
					return fmt.Errorf("fill: %s", out)
				}
			}
			return nil
		})
		n.r.mu.Unlock()
		n.m.wakes.touch(room)
		if err != nil {
			t.Fatal(err)
		}
	}
}
