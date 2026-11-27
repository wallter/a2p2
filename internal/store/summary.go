package store

// What a pull summary reads, kept in memory per room (DESIGN section 4,
// "Summaries"). Reader.Summary needs, for every stream held, the positions
// held without text. Those are the records whose reason is not live
// (missing, expired, withdrawn, not admitted), which change only when a
// write changes a record's reason, and the live records past retention by
// the clock, which pruning marks expired. Reading the first set from the
// records table cost a read of every record of the room on every pull
// request served or made: about 14 ms at 10,000 records and growing with
// the history (perf 2026-11-26).
//
// So each Store keeps, per room, the first set as of its last commit: per
// stream the positions without text, and the id and created time of those
// whose text is missing. It is built by one read of the room the first
// time a summary of it is asked for, and then kept by the writes
// themselves: every statement that inserts a record or changes its reason
// notes the record's new state in its transaction (noteText), and the
// notes are applied once that transaction has committed, under the
// cache's lock, which is held across the commit so that no read can see
// the committed rows before the cache does. A transaction that rolls back
// applies nothing. Leaving a room drops its entry. Inside a write
// transaction, Summary lays that transaction's own notes so far over the
// committed entry, so it sees what the transaction wrote. Live records
// past retention are read each time through the expiry index, which holds
// only records not yet pruned.
//
// Nothing here is stored: the database and its schema are unchanged, and
// a restart begins with an empty cache. summaryScan is the reading it
// replaces, kept for the property test that compares the two after every
// kind of write.

import (
	"database/sql"
	"slices"
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

// missingText is a record held with its text missing.
type missingText struct {
	id      string
	created int64
}

// streamText is one stream's records without live text: their positions,
// ascending, and of those the ones whose text is missing.
type streamText struct {
	nb      []int64
	missing map[int64]missingText
}

func (st *streamText) clone() *streamText {
	c := &streamText{nb: slices.Clone(st.nb), missing: make(map[int64]missingText, len(st.missing))}
	for k, v := range st.missing {
		c.missing[k] = v
	}
	return c
}

// roomText is one room's streamText by (author, epoch).
type roomText map[[2]string]*streamText

// textCache is a Store's roomText per room, as of its last commit.
type textCache struct {
	mu    sync.Mutex
	rooms map[string]roomText
}

// apply lays notes over rt in order (a later note of a record wins),
// changing the streams it names in place; clone says to copy each such
// stream first, so that rt's other users do not see the change.
func (rt roomText) apply(notes []textNote, clone bool) {
	type change struct {
		reason string
		m      missingText
	}
	by := map[[2]string]map[int64]change{}
	for _, n := range notes {
		k := [2]string{n.author, n.epoch}
		if by[k] == nil {
			by[k] = map[int64]change{}
		}
		by[k][n.seq] = change{n.reason, missingText{n.id, n.created}}
	}
	for k, cs := range by {
		st := rt[k]
		switch {
		case st == nil:
			st = &streamText{missing: map[int64]missingText{}}
		case clone:
			st = st.clone()
		}
		seqs := make([]int64, 0, len(cs))
		for s := range cs {
			seqs = append(seqs, s)
		}
		slices.Sort(seqs)
		var add []int64
		drop := map[int64]bool{}
		for _, s := range seqs {
			c := cs[s]
			if c.reason == ReasonMissing {
				st.missing[s] = c.m
			} else {
				delete(st.missing, s)
			}
			if c.reason == ReasonLive {
				drop[s] = true
			} else {
				add = append(add, s)
			}
		}
		if len(drop) == 0 && (len(st.nb) == 0 || len(add) > 0 && add[0] > st.nb[len(st.nb)-1]) {
			st.nb = append(st.nb, add...) // the common case: positions past the last
		} else {
			merged := make([]int64, 0, len(st.nb)+len(add))
			i, j := 0, 0
			for i < len(st.nb) || j < len(add) {
				var s int64
				switch {
				case j == len(add) || i < len(st.nb) && st.nb[i] < add[j]:
					s, i = st.nb[i], i+1
				case i == len(st.nb) || add[j] < st.nb[i]:
					s, j = add[j], j+1
				default:
					s, i, j = add[j], i+1, j+1
				}
				if !drop[s] {
					merged = append(merged, s)
				}
			}
			st.nb = merged
		}
		rt[k] = st
	}
}

// committed applies ts's notes to the cache after its transaction has
// committed; c.mu is held.
func (c *textCache) committed(ts *txState) {
	if c.rooms == nil {
		return
	}
	for room := range ts.drop {
		delete(c.rooms, room)
	}
	by := map[string][]textNote{}
	for _, n := range ts.notes {
		by[n.room] = append(by[n.room], n)
	}
	for room, ns := range by {
		if rt := c.rooms[room]; rt != nil {
			rt.apply(ns, false)
		}
	}
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
	rt := roomText{}
	rt.apply(ns, false)
	return rt, nil
}

// roomTextFor is room's roomText as rd's transaction sees it, nil if it
// must be read from the table (the transaction dropped the room's entry).
// The result is not to be changed.
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
	c.mu.Unlock()
	if rt == nil {
		var err error
		if rt, err = loadText(rd.tx, room); err != nil {
			return nil, err
		}
		if len(mine) > 0 {
			return rt, nil // read with this transaction's writes: not the committed state
		}
		// Read in a transaction that has written nothing to this room: the
		// committed state, which no commit can change while it is open (one
		// connection).
		c.mu.Lock()
		if c.rooms == nil {
			c.rooms = map[string]roomText{}
		}
		if c.rooms[room] == nil {
			c.rooms[room] = rt
		} else {
			rt = c.rooms[room]
		}
		c.mu.Unlock()
	}
	if len(mine) == 0 {
		return rt, nil
	}
	over := make(roomText, len(rt))
	for k, v := range rt {
		over[k] = v
	}
	over.apply(mine, true)
	return over, nil
}

// Summary returns, for every stream of room with something held, what is
// held, ordered by author then epoch. Text past retention by now counts as
// expired, as a reader treats it.
func (rd *Reader) Summary(room string, now time.Time) ([]StreamHeld, error) {
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
	for k, i := range idx {
		s := &out[i]
		st := rt[k]
		extra := old[k]
		if st == nil && len(extra) == 0 {
			continue
		}
		if st != nil && len(st.nb) > 0 {
			s.NoBody = slices.Clone(st.nb)
		}
		if len(extra) > 0 {
			s.NoBody = append(s.NoBody, extra...)
			slices.Sort(s.NoBody)
		}
		if st == nil || len(st.missing) == 0 {
			continue
		}
		seqs := make([]int64, 0, len(st.missing))
		for seq := range st.missing {
			seqs = append(seqs, seq)
		}
		slices.Sort(seqs)
		for _, seq := range seqs {
			m := st.missing[seq]
			if !TextExpired(fromMS(m.created), now) {
				s.Missing = append(s.Missing, seq)
				s.MissingIDs = append(s.MissingIDs, m.id)
			}
		}
	}
	return out, nil
}

// sqlLiveExpired reads the records of a room whose text is held or
// missing and past retention, on the expiry index: what pruning has not
// marked yet, normally nothing.
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
// room without live text, and every live one past retention.
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
		if reason == ReasonMissing && !TextExpired(fromMS(created), now) {
			s.Missing = append(s.Missing, seq)
			s.MissingIDs = append(s.MissingIDs, id)
		}
	}
	return out, rows.Err()
}
