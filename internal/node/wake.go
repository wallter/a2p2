package node

// Per-room wake-ups, in memory (step 5b). The store counts every write
// transaction in one global counter (store.Changes), which says that
// something changed but not where. A waiting pull (PROTOCOL-next 4.4 step
// 9) and the scheduler's re-issue on a local write (DESIGN-next 4) need to
// hear of a change to one room only, so that a write in one room does not
// wake every waiting pull: each write that can change a room's summary or
// log touches that room here after it has committed.

import "sync"

// roomSet is the key touched when the set of rooms held changes (a room
// pinned, created or left): the scheduler starts or stops its loops.
const roomSet = ""

// roomWakes hands out, per room, a channel closed at the next change of
// that room. The zero value is ready to use.
type roomWakes struct {
	mu sync.Mutex
	ch map[string]chan struct{}
}

// wait returns a channel closed by the next touch of room. A caller takes
// it before it looks at the room, so a change committed after the look is
// never missed.
func (w *roomWakes) wait(room string) <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ch == nil {
		w.ch = map[string]chan struct{}{}
	}
	c := w.ch[room]
	if c == nil {
		c = make(chan struct{})
		w.ch[room] = c
	}
	return c
}

// touch wakes everything waiting on any of rooms.
func (w *roomWakes) touch(rooms ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, room := range rooms {
		if c := w.ch[room]; c != nil {
			close(c)
			delete(w.ch, room)
		}
	}
}

// touchAll wakes everything waiting on any room (a pass of pruning, which
// can change the summary of every room).
func (w *roomWakes) touchAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for room, c := range w.ch {
		close(c)
		delete(w.ch, room)
	}
}
