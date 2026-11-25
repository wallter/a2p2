package node

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/model/delivery"
	"github.com/wallter/a2p2/internal/store"
)

// fileDiff runs one schedule of attachment steps on the model
// (internal/model/delivery, attach.go) and on three machines over the
// in-memory network, and compares after every step what each machine
// holds: the file's bytes and the agent's copy [F1, F2, F4, F5].
type fileDiff struct {
	t                *testing.T
	w                *delivery.World
	ns               map[delivery.MachineID]*fnode
	room             string
	mMsg             delivery.RecordID // the model's message
	nMsg             string            // the node's
	mHash            string
	nHash            string
	gone             bool
	removed, expired bool
	log              []string
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

// event is one change that can land between a fetch's start and its
// bytes: the author's withdraw, the removal of C, or the text's expiry.
func (d *fileDiff) event(kind string) {
	ctx := context.Background()
	d.log = append(d.log, kind)
	switch kind {
	case "A withdraws":
		d.gone = true
		if _, err := d.w.Withdraw("A", "agent-A", d.mMsg); err != nil {
			d.t.Fatal(err)
		}
		d.ns["A"].tick()
		if _, err := d.ns["A"].m.Withdraw(ctx, d.room, "agent-A", d.nMsg); err != nil {
			d.t.Fatal(err)
		}
	case "A removes C":
		d.removed = true
		if err := d.w.Remove("C", "A"); err != nil {
			d.t.Fatal(err)
		}
		d.ns["A"].change(d.room, Change{Kind: core.EntryRemove, Target: d.ns["C"].id})
	case "the text expires":
		d.expired = true
		d.w.Advance(delivery.Retention + delivery.Day)
		*d.ns["A"].clock = d.ns["A"].clock.Add(store.Retention + 24*time.Hour)
	}
}

// fetch is x's agent reading, then fetching from the author; with during
// set, that event lands inside the fetch, at its first decision.
func (d *fileDiff) fetch(x delivery.MachineID, during string) {
	ag := "agent-" + string(x)
	if v, err := d.w.Inbox(x, ag); err == nil {
		_ = d.w.Showed(x, ag, v.Entries)
	}
	if v, err := d.ns[x].m.Inbox(context.Background(), d.room, ag); err == nil {
		_ = d.ns[x].m.Showed(context.Background(), d.room, ag, v.Entries, true)
	}
	fired := during == ""
	if !fired {
		d.log = append(d.log, fmt.Sprintf("%s fetches, and during it:", x))
		testHook = func(p string) error {
			if p == "files:view" && !fired {
				fired = true
				d.event(during) // the model's event before its fetch, as here
			}
			return nil
		}
	} else {
		d.log = append(d.log, fmt.Sprintf("%s reads and fetches", x))
	}
	nr, nerr := d.ns[x].fetch(d.room, ag, d.nHash)
	testHook = nil
	if !fired {
		d.event(during)
	}
	_, merr := d.w.Fetch(x, ag, d.mMsg, d.mHash, "A", delivery.FetchOpt{})
	mOK, nOK := merr == nil, nerr == nil && nr.State == FetchHere
	if mOK != nOK {
		d.t.Fatalf("%v\nfetch differs: model %v, node %+v %v", d.log, merr, nr, nerr)
	}
}

func (d *fileDiff) step(r *rand.Rand) {
	x := diffNames[r.Intn(3)]
	y := diffNames[r.Intn(3)]
	switch op := r.Intn(7); {
	case op <= 1 && x != y: // x pulls from y; the list follows the pull
		d.log = append(d.log, fmt.Sprintf("%s pulls %s", x, y))
		_, _ = d.w.Pull(x, y, delivery.PullOpt{Offer: true})
		_, _ = d.ns[x].pullFrom(d.room, d.ns[y].pnode, PullOpts{Offer: true})
		d.ns[x].f.Wait()
		d.ns[y].f.Wait()
		d.ns[x].tickFiles()
	case op == 2 && x != "A":
		d.fetch(x, "")
	case op == 3 && x != "A" && !d.gone && !d.removed && !d.expired: // a transfer interleaved with a change
		d.fetch(x, []string{"A withdraws", "A removes C", "the text expires"}[r.Intn(3)])
	case op == 4 && !d.gone && r.Intn(3) == 0:
		d.event("A withdraws")
	case op == 5 && !d.removed && r.Intn(3) == 0:
		d.event("A removes C")
	case op == 6 && !d.expired && r.Intn(4) == 0:
		d.event("the text expires")
	default:
		return
	}
	for _, n := range diffNames { // each machine's sweep and deletion queue, as its timer runs them
		d.ns[n].f.sweepRoom(context.Background(), d.room)
		d.ns[n].st.RunTrash(context.Background())
	}
	d.compare()
}

// compare checks, on every machine, that the bytes held are the model's
// and that what would be served hashes to the file (from the blob), that
// the agent's copy holds exactly those bytes, and that the machines that
// received the file during the schedule (from the transfer events of the
// network, GETs answered with bytes and uploads accepted) are the model's.
func (d *fileDiff) compare() {
	for _, n := range diffNames {
		fn := d.ns[n]
		var held, copied bool
		var cp store.FileCopy
		if err := fn.st.View(context.Background(), func(rd *store.Reader) (err error) {
			if _, held, err = rd.Held(d.room, d.nHash); err != nil {
				return err
			}
			cp, copied, err = rd.FileCopy(d.room, d.nMsg, d.nHash, "agent-"+string(n))
			return err
		}); err != nil {
			d.t.Fatal(err)
		}
		if m := d.w.HoldsAnyOf(n, d.mHash); m != held {
			d.t.Fatalf("%v\n%s holds the bytes: model %v, node %v", d.log, n, m, held)
		}
		if held {
			b, err := fn.st.OpenBlob(d.room, d.nHash)
			if err != nil {
				d.t.Fatalf("%v\n%s: %v", d.log, n, err)
			}
			data, _ := io.ReadAll(b)
			b.Close()
			want, _ := d.w.FileBytes(n, d.mHash)
			if core.SHA256Hex(data) != d.nHash || string(data) != want {
				d.t.Fatalf("%v\n%s would serve %q, the model holds %q", d.log, n, data, want)
			}
		}
		if m := d.w.CopyFor(n, "agent-"+string(n), d.mMsg, d.mHash); n != "A" && m != copied {
			d.t.Fatalf("%v\n%s has the agent's copy: model %v, node %v", d.log, n, m, copied)
		}
		if copied {
			data, err := os.ReadFile(fn.st.CopyPath(cp.Path))
			if err != nil || core.SHA256Hex(data) != d.nHash {
				d.t.Fatalf("%v\n%s's copy holds %q %v", d.log, n, data, err)
			}
		}
	}
	model, node := map[delivery.MachineID]bool{}, map[delivery.MachineID]bool{}
	for _, x := range d.w.Transfers() {
		if x.Hash == d.mHash {
			model[x.To] = true
		}
	}
	byID := map[string]delivery.MachineID{}
	for _, n := range diffNames {
		byID[d.ns[n].id] = n
	}
	fn := d.ns["A"].fn
	for _, ev := range append(append([][3]string(nil), fn.served...), fn.puts...) {
		if ev[2] == d.nHash { // [server or sender, receiver, sha]
			node[byID[ev[1]]] = true
		}
	}
	if fmt.Sprint(model) != fmt.Sprint(node) {
		d.t.Fatalf("%v\nreceived the file: model %v, node %v", d.log, model, node)
	}
}

// TestImplementationAgreesWithTheModelOnAttachmentSchedules: random
// schedules of pulls, reads with fetches, a withdraw, the removal of a
// member and the text's expiry, some landing inside a fetch, the same on
// the model and on machines over memory: after every step they hold the
// same bytes and copies (compared by their content), and the same
// machines received the file.
// Requirements: L1
func TestImplementationAgreesWithTheModelOnAttachmentSchedules(t *testing.T) {
	seeds := 24
	if testing.Short() {
		seeds = 6
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
