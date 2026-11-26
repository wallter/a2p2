package node

// Per-room wake-ups, in memory (step 5b). The store counts every write
// transaction in one global counter (store.Changes), which says that
// something changed but not where. A waiting pull (PROTOCOL 4.4 step
// 9) and the scheduler's re-issue on a local write (DESIGN 4) need to
// hear of a change to one room only, so that a write in one room does not
// wake every waiting pull: each write that can change a room's summary or
// log touches that room here after it has committed.

//
// A waiting Inbox is woken more narrowly (inboxWaiter): a record this
// machine writes wakes only the waiters it can show something to, so that
// with N agents waiting in one room a send costs one look, not N. Every
// other touch wakes every waiting inbox of the room, as before.

import (
	"sync"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// roomSet is the key touched when the set of rooms held changes (a room
// pinned, created or left): the scheduler starts or stops its loops.
const roomSet = ""

// everyRoom is the key under which an Inbox of every room waits: it is
// woken by a change of any room.
const everyRoom = "\x00every"

// roomWakes hands out, per room, a channel closed at the next change of
// that room. The zero value is ready to use.
type roomWakes struct {
	mu  sync.Mutex
	ch  map[string]chan struct{}
	seq map[string]uint64 // touches per room
	all uint64            // touches of every room
	// inbox is, per room (or everyRoom), the Inbox calls waiting in it.
	inbox map[string]map[*inboxWaiter]bool
	// woke counts the inbox waiters woken (tests).
	woke uint64
	// looks is, per room and agent, where the agent's last look that
	// found nothing got to (lookMemo, reading.go), and how many writes
	// since might show it something.
	looks map[string]map[string]*lookState
}

// lookState is one agent's looks in one room. narrow counts the touches
// of the room since the agent first looked, except records this machine
// wrote that cannot show it anything (mayShow, with the memo's level).
type lookState struct {
	narrow uint64
	memo   *lookMemo
}

// lookAt returns agent's narrow count in room and its memo (nil: none),
// to be read before a look's transaction begins.
func (w *roomWakes) lookAt(room, agent string) (uint64, *lookMemo) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.looks == nil {
		w.looks = map[string]map[string]*lookState{}
	}
	if w.looks[room] == nil {
		w.looks[room] = map[string]*lookState{}
	}
	st := w.looks[room][agent]
	if st == nil {
		st = &lookState{}
		w.looks[room][agent] = st
	}
	return st.narrow, st.memo
}

// keepLook keeps memo as agent's in room.
func (w *roomWakes) keepLook(room, agent string, memo *lookMemo) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if st := w.looks[room][agent]; st != nil {
		st.memo = memo
	}
}

// dirty raises the narrow count of every agent's looks in room that
// match (nil: all), and of every room for room roomSet. The caller
// holds mu.
func (w *roomWakes) dirty(room string, match func(agent string, memo *lookMemo) bool) {
	mark := func(as map[string]*lookState) {
		for a, st := range as {
			if match == nil || match(a, st.memo) {
				st.narrow++
			}
		}
	}
	if room != roomSet {
		mark(w.looks[room])
		return
	}
	for _, as := range w.looks {
		mark(as)
	}
}

// inboxWaiter is one Inbox call between its look and the next. Its
// channel is closed by any touch of a room it waits in, except a record
// this machine wrote that cannot show the agent anything (touchWrite).
type inboxWaiter struct {
	agent string
	keys  []string
	ch    chan struct{}
	// levels is the agent's reading level in each room as the look read
	// it; a room without one is not known yet, and any record wakes.
	levels map[string]string
	closed bool
}

// sub is wait, and the number of touches of room so far: a caller that
// remembers it can tell later whether room changed since, though it took
// no channel in between (the scheduler's mark of a write not yet offered).
func (w *roomWakes) sub(room string) (<-chan struct{}, uint64) {
	c := w.wait(room)
	w.mu.Lock()
	defer w.mu.Unlock()
	return c, w.seq[room] + w.all
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
		w.bump(room)
		w.wakeInbox(room, nil)
		w.dirty(room, nil)
	}
}

// bump counts a touch of room and closes its channel. The caller holds mu.
func (w *roomWakes) bump(room string) {
	if w.seq == nil {
		w.seq = map[string]uint64{}
	}
	w.seq[room]++
	if c := w.ch[room]; c != nil {
		close(c)
		delete(w.ch, room)
	}
}

// touchAll wakes everything waiting on any room (a pass of pruning, which
// can change the summary of every room).
func (w *roomWakes) touchAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.all++
	for room, c := range w.ch {
		close(c)
		delete(w.ch, room)
	}
	for _, xs := range w.inbox {
		for x := range xs {
			w.close(x)
		}
	}
	w.dirty(roomSet, nil)
}

// touchWrite is touch(room) after this machine (self) wrote the record
// with fields f in room, except that of the inbox waiters it wakes only
// those f may show something to (mayShow). Everything else waiting on room
// is woken, as by touch.
func (w *roomWakes) touchWrite(room, self string, f core.RecordFields) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.bump(room)
	w.wakeInbox(room, func(x *inboxWaiter) bool {
		l, known := x.levels[room]
		return mayShow(self, f, x.agent, l, known)
	})
	w.dirty(room, func(agent string, memo *lookMemo) bool {
		if memo == nil { // a look under way: its level is not known here
			return mayShow(self, f, agent, "", false)
		}
		return mayShow(self, f, agent, memo.level, true)
	})
}

// touchInbox wakes the inbox waiters of agent in room ("" every agent),
// and nothing else: a change of what only Inbox reads (a reading level).
func (w *roomWakes) touchInbox(room, agent string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.wakeInbox(room, func(x *inboxWaiter) bool { return agent == "" || x.agent == agent })
	w.dirty(room, func(a string, _ *lookMemo) bool { return agent == "" || a == agent })
}

// wakeInbox closes the inbox waiters of room, and of every room, that
// match (nil: all of them); a touch of roomSet wakes every waiter. The
// caller holds mu.
func (w *roomWakes) wakeInbox(room string, match func(*inboxWaiter) bool) {
	keys := []string{room, everyRoom}
	if room == roomSet {
		keys = keys[:0]
		for k := range w.inbox {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		for x := range w.inbox[k] {
			if match == nil || match(x) {
				w.close(x)
			}
		}
	}
}

// close wakes x and takes it off every list. The caller holds mu.
func (w *roomWakes) close(x *inboxWaiter) {
	if x.closed {
		return
	}
	x.closed = true
	w.woke++
	close(x.ch)
	for _, k := range x.keys {
		delete(w.inbox[k], x)
		if len(w.inbox[k]) == 0 {
			delete(w.inbox, k)
		}
	}
}

// waitInbox registers an Inbox of agent in room ("": every room) before it
// looks, so that a change committed after the look is never missed; its
// level in each room is not known until setLevels. The caller ends it with
// doneInbox.
func (w *roomWakes) waitInbox(agent, room string) *inboxWaiter {
	key := room
	if room == "" {
		key = everyRoom
	}
	x := &inboxWaiter{agent: agent, keys: []string{key}, ch: make(chan struct{})}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inbox == nil {
		w.inbox = map[string]map[*inboxWaiter]bool{}
	}
	if w.inbox[key] == nil {
		w.inbox[key] = map[*inboxWaiter]bool{}
	}
	w.inbox[key][x] = true
	return x
}

// setLevels gives x the agent's level in each room its look read. A level
// changed since that look has already woken x (touchInbox after SetLevel).
func (w *roomWakes) setLevels(x *inboxWaiter, levels map[string]string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	x.levels = levels
}

// doneInbox takes x off the lists; it is no longer woken.
func (w *roomWakes) doneInbox(x *inboxWaiter) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !x.closed {
		x.closed = true
		for _, k := range x.keys {
			delete(w.inbox[k], x)
			if len(w.inbox[k]) == 0 {
				delete(w.inbox, k)
			}
		}
	}
}

// inboxWoken is how many inbox waiters were woken so far (tests).
func (w *roomWakes) inboxWoken() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.woke
}

// mayShow says whether a record with fields f, just written by this
// machine (self), can change what Inbox shows agent, reading the room at
// level (known: the level is the agent's now). A look that showed agent
// nothing shows it nothing more after a record of which this is false: the
// record is at a new position, and no earlier position reads it. These
// are the reading rules of [R7] (decide, relevant, admits):
//
//   - a message is shown only to an agent it is relevant to (not its own
//     writer; an addressee, or anyone when it has none) and that the level
//     admits (a new message is neither listed nor skipped);
//   - a handled report changes only its writer's inbox (handledByAgent);
//   - anything else (a replace or withdraw) may change what any agent is
//     shown: it wakes every waiter.
func mayShow(self string, f core.RecordFields, agent, level string, known bool) bool {
	switch f.Kind {
	case core.KindMessage:
		if f.Agent == agent {
			return false
		}
		if len(f.To) > 0 {
			to := false
			for _, a := range f.To {
				if a.Node == self && a.Agent == agent {
					to = true
				}
			}
			if !to {
				return false
			}
		}
		if !known {
			return true
		}
		switch level {
		case store.LevelAddressed:
			return len(f.To) > 0 || core.MarkedForEveryone(f.Text)
		case store.LevelNone:
			return false
		}
		return true
	case core.KindHandled:
		return f.Agent == agent
	}
	return true
}
