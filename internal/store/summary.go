package store

// What a pull summary reads, kept in memory per room (DESIGN section 4,
// "How a summary is computed"). Reader.Summary needs, for every stream
// held, the positions held without text. Those are the records whose
// reason is not live (missing, expired, withdrawn, not admitted), which
// change only when a write changes a record's reason, and the live records
// past retention by the clock, which pruning marks expired. Reading the
// first set from the records table cost a read of every record of the room
// on every pull request served or made: about 14 ms at 10,000 records and
// growing with the history (perf 2026-11-26).
//
// So each Store keeps, per room, the first set as of its last commit: per
// stream the runs of positions without text, and the positions, ids and
// created times of those whose text is missing. It is built by one read of
// the room the first time a summary of it is asked for, and then kept by
// the writes themselves: every statement that changes a record's reason,
// or inserts one without live text, notes the record's new state in its
// transaction (noteText); an insert with live text changes nothing here
// and is not noted. The notes are applied once that transaction has
// committed, under the cache's lock, which is held across the commit so
// that no read can see the committed rows before the cache does. A
// transaction that rolls back applies nothing. Leaving a room drops its
// entry. Inside a write transaction, Summary lays that transaction's own
// notes so far over the committed entry, so it sees what the transaction
// wrote. Live records past retention are read each time through the
// expiry index, which holds only records not yet pruned.
//
// Entries are never changed once in the cache: a commit puts new ones in
// their place (copy on write), so what a read took from the cache stays
// as it was, whatever commits while the read goes on. That matters
// because one connection does not keep a read and a write apart when the
// read's context is cancelled: database/sql then rolls the read back and
// gives the connection to the next transaction while the read's callback
// is still running. For the same reason an entry a read built is put in
// the cache only if no write noting that room has committed since the read
// began to build it (textCache.gen); otherwise it is used for that read
// alone.
//
// The cache assumes this Store is the only writer of the database. A
// second Store on the same file, or another program writing it, would
// leave it stale until a restart; the daemon's lock on its home is what
// prevents that. There is no eviction: an entry lives until the room is
// left or the process ends. What bounds it is what this machine holds:
// per stream one run per stretch of positions without text and one entry
// per missing text. A member can make that grow by sending headers whose
// text it never sends (about 80 bytes each until pruning expires them,
// then part of a run), or by withdrawing every other one of its messages
// (16 bytes per run), within what its records may cost the machine anyway.
//
// Nothing here is stored: the database and its schema are unchanged, and
// a restart begins with an empty cache. summaryScan is the reading it
// replaces, kept for the property tests that compare the two.

import (
	"cmp"
	"database/sql"
	"slices"
	"sort"
	"sync"
	"time"
)

// textNote is one record's state after a statement that wrote it: its
// position, id, reason and created time (milliseconds).
type textNote struct {
	room, author, epoch, id, reason string
	seq, created                    int64
}

// txState is what the store knows of one open transaction: the store, and
// for a write transaction the notes of its writes in order and the rooms
// whose entry it drops.
type txState struct {
	s     *Store
	write bool
	notes []textNote
	drop  map[string]bool
}

// summaryHook, when a test sets it, runs at the named points of Summary.
var summaryHook func(point string)

// txStates maps each open transaction (*sql.Tx) to its *txState: what
// lets the package's functions, which are handed only the transaction,
// note their writes and reach the cache.
var txStates sync.Map

func stateOf(tx *sql.Tx) *txState {
	if v, ok := txStates.Load(tx); ok {
		return v.(*txState)
	}
	return nil
}

// noteText records that a statement of tx left record n in its state.
func noteText(tx *sql.Tx, n textNote) {
	if ts := stateOf(tx); ts != nil && ts.write {
		ts.notes = append(ts.notes, n)
	}
}

// dropText records that tx changes room's records in a way the notes do
// not say (a leave deletes them).
func dropText(tx *sql.Tx, room string) {
	if ts := stateOf(tx); ts != nil && ts.write {
		if ts.drop == nil {
			ts.drop = map[string]bool{}
		}
		ts.drop[room] = true
	}
}

// returningNoted ends an UPDATE of records: it returns, for noting, the
// state each changed record is left in.
const returningNoted = `RETURNING room, author, epoch, id, reason, seq, created`

// queryNoted runs an UPDATE ending in returningNoted, notes each record it
// changed, and returns how many.
func queryNoted(tx *sql.Tx, q string, args ...any) (int64, error) {
	rows, err := tx.Query(q, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var c int64
	for rows.Next() {
		var n textNote
		if err := rows.Scan(&n.room, &n.author, &n.epoch, &n.id, &n.reason, &n.seq, &n.created); err != nil {
			return c, err
		}
		noteText(tx, n)
		c++
	}
	return c, rows.Err()
}

func execNoted(tx *sql.Tx, q string, args ...any) error {
	_, err := queryNoted(tx, q, args...)
	return err
}

// span is the positions lo to hi of a stream, both included.
type span struct{ lo, hi int64 }

// missingText is a record held with its text missing, at position seq.
type missingText struct {
	seq     int64
	id      string
	created int64
}

// streamText is one stream's records without live text: the runs of their
// positions, ascending, apart and not adjacent; and of those the ones
// whose text is missing, by position. Once in the cache it is not changed.
type streamText struct {
	runs    []span
	missing []missingText
}

// roomText is one room's streamText by (author, epoch); a stream with
// every record's text live has none. Once in the cache it is not changed.
type roomText map[[2]string]*streamText

// textCache is a Store's roomText per room, as of its last commit, and
// per room a count of the commits that noted it (gen), with reset counting
// the times the whole cache was dropped.
type textCache struct {
	mu    sync.Mutex
	rooms map[string]roomText
	gen   map[string]uint64
	reset uint64
}

// change is a record's reason after a transaction, with what a missing
// text needs.
type change struct {
	seq     int64
	reason  string
	id      string
	created int64
}

// with returns rt with notes laid over it in order (a later note of a
// record wins), as a new roomText: rt and its streams are not changed.
func (rt roomText) with(notes []textNote) roomText {
	by := map[[2]string]map[int64]change{}
	for _, n := range notes {
		k := [2]string{n.author, n.epoch}
		if by[k] == nil {
			by[k] = map[int64]change{}
		}
		by[k][n.seq] = change{n.seq, n.reason, n.id, n.created}
	}
	out := make(roomText, len(rt)+len(by))
	for k, st := range rt {
		out[k] = st
	}
	for k, m := range by {
		cs := make([]change, 0, len(m))
		for _, c := range m {
			cs = append(cs, c)
		}
		slices.SortFunc(cs, func(a, b change) int { return cmp.Compare(a.seq, b.seq) })
		var old streamText
		if st := rt[k]; st != nil {
			old = *st
		}
		st := &streamText{runs: mergeRuns(old.runs, cs), missing: mergeMissing(old.missing, cs)}
		if len(st.runs) == 0 {
			delete(out, k)
		} else {
			out[k] = st
		}
	}
	return out
}

// mergeRuns is runs with the positions of cs (ascending, one each) set to
// live or not by their reason, as new runs.
func mergeRuns(runs []span, cs []change) []span {
	out := make([]span, 0, len(runs)+len(cs))
	push := func(lo, hi int64) {
		if n := len(out); n > 0 && out[n-1].hi+1 >= lo {
			out[n-1].hi = max(out[n-1].hi, hi)
			return
		}
		out = append(out, span{lo, hi})
	}
	j := 0
	for _, r := range runs {
		for ; j < len(cs) && cs[j].seq < r.lo; j++ {
			if cs[j].reason != ReasonLive {
				push(cs[j].seq, cs[j].seq)
			}
		}
		lo := r.lo
		for ; j < len(cs) && cs[j].seq <= r.hi; j++ {
			if cs[j].reason == ReasonLive {
				if lo < cs[j].seq {
					push(lo, cs[j].seq-1)
				}
				lo = cs[j].seq + 1
			}
		}
		if lo <= r.hi {
			push(lo, r.hi)
		}
	}
	for ; j < len(cs); j++ {
		if cs[j].reason != ReasonLive {
			push(cs[j].seq, cs[j].seq)
		}
	}
	return out
}

// mergeMissing is ms with the positions of cs (ascending, one each) set to
// missing or not by their reason, as a new list.
func mergeMissing(ms []missingText, cs []change) []missingText {
	out := make([]missingText, 0, len(ms))
	i := 0
	for _, c := range cs {
		for ; i < len(ms) && ms[i].seq < c.seq; i++ {
			out = append(out, ms[i])
		}
		if i < len(ms) && ms[i].seq == c.seq {
			i++
		}
		if c.reason == ReasonMissing {
			out = append(out, missingText{c.seq, c.id, c.created})
		}
	}
	return append(out, ms[i:]...)
}

// committed applies ts's notes to the cache after its transaction has
// committed; c.mu is held.
func (c *textCache) committed(ts *txState) {
	by := map[string][]textNote{}
	for _, n := range ts.notes {
		by[n.room] = append(by[n.room], n)
	}
	if c.gen == nil {
		c.gen = map[string]uint64{}
	}
	for room := range ts.drop {
		c.gen[room]++
		delete(c.rooms, room)
	}
	for room, ns := range by {
		c.gen[room]++
		if rt := c.rooms[room]; rt != nil {
			c.rooms[room] = rt.with(ns)
		}
	}
}

// dropped forgets every entry: a commit failed, and what it wrote is not
// known. c.mu is held.
func (c *textCache) dropped() {
	c.rooms = nil
	c.reset++
}

// loadText reads room's records without live text into a roomText.
func loadText(tx *sql.Tx, room string) (roomText, error) {
	rows, err := tx.Query(`SELECT author, epoch, seq, id, reason, created FROM records WHERE room = ? AND reason != 'live'`, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ns []textNote
	for rows.Next() {
		n := textNote{room: room}
		if err := rows.Scan(&n.author, &n.epoch, &n.seq, &n.id, &n.reason, &n.created); err != nil {
			return nil, err
		}
		ns = append(ns, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return roomText{}.with(ns), nil
}

// roomTextFor is room's roomText as rd's transaction sees it, nil if it
// must be read from the table (the transaction dropped the room's entry).
// It is not changed by anything that commits later.
func (rd *Reader) roomTextFor(ts *txState, room string) (roomText, error) {
	var mine []textNote
	if ts.write {
		if ts.drop[room] {
			return nil, nil
		}
		for _, n := range ts.notes {
			if n.room == room {
				mine = append(mine, n)
			}
		}
	}
	c := &ts.s.texts
	c.mu.Lock()
	rt := c.rooms[room]
	gen, reset := c.gen[room], c.reset
	c.mu.Unlock()
	if rt == nil {
		var err error
		if rt, err = loadText(rd.tx, room); err != nil {
			return nil, err
		}
		if summaryHook != nil {
			summaryHook("summary:loaded")
		}
		if len(mine) > 0 {
			return rt, nil // read with this transaction's writes: not the committed state
		}
		// The committed state as of when the read began, unless a write
		// that noted the room has committed since: then this transaction
		// was cancelled and gave up the connection (one connection keeps
		// them apart otherwise), and what it read is for it alone.
		c.mu.Lock()
		switch {
		case c.gen[room] != gen || c.reset != reset:
		case c.rooms[room] != nil:
			rt = c.rooms[room]
		default:
			if c.rooms == nil {
				c.rooms = map[string]roomText{}
			}
			c.rooms[room] = rt
		}
		c.mu.Unlock()
	}
	if len(mine) == 0 {
		return rt, nil
	}
	return rt.with(mine), nil
}

// Summary returns, for every stream of room with something held, what is
// held, ordered by author then epoch. Text past retention by now counts as
// expired, as a reader treats it.
func (rd *Reader) Summary(room string, now time.Time) ([]StreamHeld, error) {
	if summaryHook != nil {
		summaryHook("summary:begin")
	}
	ts := stateOf(rd.tx)
	if ts == nil {
		return rd.summaryScan(room, now)
	}
	out, idx, err := rd.heldStreams(room)
	if err != nil {
		return nil, err
	}
	rt, err := rd.roomTextFor(ts, room)
	if err != nil {
		return nil, err
	}
	if rt == nil {
		return rd.summaryScan(room, now)
	}
	// Live text past retention that pruning has not marked yet, through
	// the expiry index (+room keeps the planner off the room's indexes).
	old := map[[2]string][]int64{}
	rows, err := rd.tx.Query(sqlLiveExpired, ms(now.Add(-Retention)), room)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a, e, reason string
		var seq int64
		if err := rows.Scan(&a, &e, &seq, &reason); err != nil {
			rows.Close()
			return nil, err
		}
		if reason == ReasonLive {
			old[[2]string{a, e}] = append(old[[2]string{a, e}], seq)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if summaryHook != nil {
		summaryHook("summary:expiry")
	}
	for k, i := range idx {
		s := &out[i]
		st, extra := rt[k], old[k]
		if st == nil && len(extra) == 0 {
			continue
		}
		slices.Sort(extra)
		s.PastRetention = len(extra) > 0
		var runs []span
		var missing []missingText
		if st != nil {
			runs, missing = st.runs, st.missing
		}
		// Only what the streams table holds: a read cancelled after the
		// streams were read may see a later commit's entry.
		if n := sort.Search(len(runs), func(j int) bool { return runs[j].lo > s.Contig }); n < len(runs) {
			runs = runs[:n]
		}
		if len(runs) > 0 && runs[0].lo == 1 {
			s.Floor = min(runs[0].hi, s.Contig)
			runs = runs[1:]
		}
		s.NoBody = expandMerged(runs, extra, s.Contig)
		for _, m := range missing {
			if m.seq <= s.Contig && !TextExpired(fromMS(m.created), now) {
				s.Missing = append(s.Missing, m.seq)
				s.MissingIDs = append(s.MissingIDs, m.id)
			}
		}
	}
	return out, nil
}

// expandMerged lists the positions of runs and of extra (ascending, apart
// from runs) up to contig, in order.
func expandMerged(runs []span, extra []int64, contig int64) []int64 {
	var out []int64
	j := 0
	for _, r := range runs {
		for n := r.lo; n <= min(r.hi, contig); n++ {
			for ; j < len(extra) && extra[j] < n; j++ {
				out = append(out, extra[j])
			}
			out = append(out, n)
		}
	}
	return append(out, extra[j:]...)
}

// sqlLiveExpired reads the records of a room whose text is held or
// missing and past retention, on the expiry index: what pruning has not
// marked yet, normally nothing. The index is over every room, so it also
// passes the unpruned rows of other rooms: missing text kept undecided,
// and the rooms set aside.
const sqlLiveExpired = `SELECT author, epoch, seq, reason FROM records WHERE reason IN ('live', 'missing') AND created <= ?
	AND +room = ?`

// heldStreams is every stream of room with something held, ordered by
// author then epoch, and its index by (author, epoch).
func (rd *Reader) heldStreams(room string) ([]StreamHeld, map[[2]string]int, error) {
	rows, err := rd.tx.Query(`SELECT author, epoch, contig FROM streams WHERE room = ? AND contig > 0
		ORDER BY author, epoch`, room)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []StreamHeld
	idx := map[[2]string]int{}
	for rows.Next() {
		var s StreamHeld
		if err := rows.Scan(&s.Author, &s.Epoch, &s.Contig); err != nil {
			return nil, nil, err
		}
		idx[[2]string{s.Author, s.Epoch}] = len(out)
		out = append(out, s)
	}
	return out, idx, rows.Err()
}

// summaryScan is Summary read from the records table: every record of the
// room without live text, and every live one past retention. It lists
// every position in NoBody (Floor 0).
func (rd *Reader) summaryScan(room string, now time.Time) ([]StreamHeld, error) {
	out, idx, err := rd.heldStreams(room)
	if err != nil {
		return nil, err
	}
	rows, err := rd.tx.Query(`SELECT author, epoch, seq, id, reason, created FROM records WHERE room = ?
		AND (reason != 'live' OR created <= ?) ORDER BY author, epoch, seq`, room, ms(now.Add(-Retention)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a, e, id, reason string
		var seq, created int64
		if err := rows.Scan(&a, &e, &seq, &id, &reason, &created); err != nil {
			return nil, err
		}
		i, ok := idx[[2]string{a, e}]
		if !ok {
			continue
		}
		s := &out[i]
		s.NoBody = append(s.NoBody, seq)
		if reason == ReasonLive {
			s.PastRetention = true
		}
		if reason == ReasonMissing && !TextExpired(fromMS(created), now) {
			s.Missing = append(s.Missing, seq)
			s.MissingIDs = append(s.MissingIDs, id)
		}
	}
	return out, rows.Err()
}
