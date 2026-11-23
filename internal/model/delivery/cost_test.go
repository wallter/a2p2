package delivery

import (
	"fmt"
	"testing"
)

// streamCost runs 64 machines for 30 days: each restarts once a day and
// writes one message; each pulls from a hub and the hub from each. It returns
// the streams held by the hub and the size of one full summary, at 14 bytes
// per stream (author index, epoch, contiguous position) plus 4 per position
// without a body.
func streamCost(persist bool) (streams, bytes int) {
	w := NewWorld()
	w.persistHeads = persist
	var ids []MachineID
	for i := 0; i < 64; i++ {
		id := MachineID(fmt.Sprintf("M%02d", i))
		w.AddMachine(id, "a")
		ids = append(ids, id)
	}
	for day := 0; day < 30; day++ {
		for _, id := range ids {
			w.Crash(id)
			if _, err := w.Send(id, "a", Msg{Body: "status"}); err != nil {
				panic(err)
			}
		}
		for _, id := range ids[1:] {
			if _, err := w.Pull(ids[0], id, PullOpt{}); err != nil {
				panic(err)
			}
		}
		for _, id := range ids[1:] {
			if _, err := w.Pull(id, ids[0], PullOpt{}); err != nil {
				panic(err)
			}
		}
		w.Advance(Day)
	}
	s := w.Machine(ids[0]).summary()
	for _, h := range s {
		bytes += 14 + 4*len(h.NoBody)
	}
	return len(s), bytes
}

// Requirements: P5 (cost of one stream per start, measured)
func TestStreamCostForSixtyFourMachinesOverThirtyDays(t *testing.T) {
	if !deep() {
		t.Skip("measurement; run with A2P2_MODEL_DEEP=1")
	}
	n, b := streamCost(false)
	t.Logf("new stream on every start: %d streams, full summary about %d bytes, %d cached rows per machine", n, b, 63*n)
	n, b = streamCost(true)
	t.Logf("head file outside the backup: %d streams, full summary about %d bytes", n, b)
}
