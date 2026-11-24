package node

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/model/delivery"
	"github.com/wallter/a2p2/internal/store"
)

// fileDiff runs one schedule of attachment steps on the model
// (internal/model/delivery, attach.go) and on three machines over the
// in-memory network, and compares after every step what each machine
// holds: the file's bytes and the agent's copy [F1, F2, F4, F5].
type fileDiff struct {
	t     *testing.T
	w     *delivery.World
	ns    map[delivery.MachineID]*fnode
	room  string
	mMsg  delivery.RecordID // the model's message
	nMsg  string            // the node's
	mHash string
	nHash string
	gone  bool
	log   []string
}

var diffNames = []delivery.MachineID{"A", "B", "C"}

func newFileDiff(t *testing.T) *fileDiff {
	t.Helper()
	d := &fileDiff{t: t, w: delivery.NewWorld(), ns: map[delivery.MachineID]*fnode{}}
	for _, n := range diffNames {
		d.w.AddMachine(n, "agent-"+string(n))
	}
	ns := filesNet(t, "A", "B", "C")
	for i, n := range diffNames {
		d.ns[n] = ns[i]
	}
	a := d.ns["A"]
	d.room = a.create("team")
	for _, n := range diffNames[1:] {
		a.change(d.room, Change{Kind: core.EntryAdd, Target: d.ns[n].id, Name: string(n), History: true})
	}
	for _, n := range diffNames[1:] {
		d.ns[n].pin(d.room, a.lnode)
		d.ns[n].pull(d.room, a.lnode)
	}
	for _, n := range diffNames {
		d.ns[n].register("agent-" + string(n))
	}
	// the message with a file, on both
	data := "differential bytes"
	d.w.PutLocalFile("A", "f.txt", data)
	k, err := d.w.SendFiles("A", "agent-A", delivery.Msg{Body: "with a file"}, "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if d.mMsg, err = d.w.Decide("A", k, true); err != nil {
		t.Fatal(err)
	}
	d.mHash = core.SHA256Hex([]byte(data))
	msg, res := a.sendFiles(d.room, "agent-A", "with a file", writeSource(t, "f.txt", data))
	d.nMsg, d.nHash = msg, res.Files[0].SHA256
	return d
}

func (d *fileDiff) step(r *rand.Rand) {
	ctx := context.Background()
	x := diffNames[r.Intn(3)]
	y := diffNames[r.Intn(3)]
	switch op := r.Intn(4); {
	case op <= 1 && x != y: // x pulls from y; the list follows the pull
		d.log = append(d.log, fmt.Sprintf("%s pulls %s", x, y))
		if _, err := d.w.Pull(x, y, delivery.PullOpt{}); err != nil {
			d.t.Fatalf("model pull: %v", err)
		}
		d.ns[x].pullAll(d.room, d.ns[y])
		d.ns[x].tickFiles()
	case op == 2 && x != "A": // x's agent reads and fetches from the author
		d.log = append(d.log, fmt.Sprintf("%s reads and fetches", x))
		ag := "agent-" + string(x)
		if v, err := d.w.Inbox(x, ag); err == nil {
			_ = d.w.Showed(x, ag, v.Entries)
		}
		_, merr := d.w.Fetch(x, ag, d.mMsg, d.mHash, "A", delivery.FetchOpt{})
		d.ns[x].inbox(d.room, ag)
		nr, nerr := d.ns[x].fetch(d.room, ag, d.nHash)
		mOK, nOK := merr == nil, nerr == nil && nr.State == FetchHere
		if mOK != nOK {
			d.t.Fatalf("%v\nfetch differs: model %v, node %+v %v", d.log, merr, nr, nerr)
		}
	case op == 3 && !d.gone && r.Intn(3) == 0: // the author withdraws
		d.log = append(d.log, "A withdraws")
		d.gone = true
		if _, err := d.w.Withdraw("A", "agent-A", d.mMsg); err != nil {
			d.t.Fatal(err)
		}
		d.ns["A"].tick()
		if _, err := d.ns["A"].m.Withdraw(ctx, d.room, "agent-A", d.nMsg); err != nil {
			d.t.Fatal(err)
		}
	default:
		return
	}
	d.compare()
}

func (d *fileDiff) compare() {
	for _, n := range diffNames {
		fn := d.ns[n]
		var held, copied bool
		if err := fn.st.View(context.Background(), func(rd *store.Reader) (err error) {
			if _, held, err = rd.Held(d.room, d.nHash); err != nil {
				return err
			}
			_, copied, err = rd.FileCopy(d.room, d.nMsg, d.nHash, "agent-"+string(n))
			return err
		}); err != nil {
			d.t.Fatal(err)
		}
		if m := d.w.HoldsAnyOf(n, d.mHash); m != held {
			d.t.Fatalf("%v\n%s holds the bytes: model %v, node %v", d.log, n, m, held)
		}
		if m := d.w.CopyFor(n, "agent-"+string(n), d.mMsg, d.mHash); n != "A" && m != copied {
			d.t.Fatalf("%v\n%s has the agent's copy: model %v, node %v", d.log, n, m, copied)
		}
	}
}

// TestImplementationAgreesWithTheModelOnAttachmentSchedules: random
// schedules of pulls, reads with fetches and a withdraw, the same on the
// model and on machines over memory, hold the same bytes and copies after
// every step.
// Requirements: L1
func TestImplementationAgreesWithTheModelOnAttachmentSchedules(t *testing.T) {
	seeds := 12
	if testing.Short() {
		seeds = 4
	}
	for seed := 0; seed < seeds; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			d := newFileDiff(t)
			r := rand.New(rand.NewSource(int64(seed)))
			for i := 0; i < 14; i++ {
				d.step(r)
			}
		})
	}
}
