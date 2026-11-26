package store

// What the pull exchange keeps (DESIGN section 4, PROTOCOL
// sections 4 and 8): each peer's last observed summary, the only source of
// copy reports [R5], and counts of what each peer sent that was refused.
// This is migration 4; migrations 2 and 3 have shipped.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// schemaPull is migration 4.
//
// observed holds, per room, peer and author, the digest the peer last
// reported for that author, its detail (PROTOCOL 4.3, as JSON) when
// known, and when this machine learned it (its own clock). detail is NULL
// while unknown: the digest changed and the detail has not arrived yet.
// streams.known_from is the peer whose report set a stream's known
// position (Tx.NoteKnown); ” if none. Beyond section 8: a row with an empty digest and detail '[]' says the
// peer takes the author but holds nothing of it; and the row with an empty author
// holds the peer's last log heads and held or refused entry ids, as JSON
// {"heads":[…],"extra":[…]}, so a request carries only the entries the
// peer lacks.
const schemaPull = `
CREATE TABLE observed (
  room TEXT NOT NULL, peer TEXT NOT NULL, author TEXT NOT NULL,
  digest TEXT NOT NULL, detail TEXT, at INTEGER NOT NULL,
  PRIMARY KEY (room, peer, author));
CREATE TABLE refusals (
  room TEXT NOT NULL, peer TEXT NOT NULL, reason TEXT NOT NULL,
  count INTEGER NOT NULL, last_at INTEGER NOT NULL,
  PRIMARY KEY (room, peer, reason));
`

// schemaKnownFrom is migration 5: the peer whose report set a stream's
// known position, so that the same peer's later report can lower it.
const schemaKnownFrom = `
ALTER TABLE streams ADD COLUMN known_from TEXT NOT NULL DEFAULT '';
`

// StreamDetail is one stream of an author's detail (PROTOCOL 4.3):
// its epoch, contiguous position, the floor X (no position at or below it
// is held with text or wanted), the positions above X held without text
// (ascending) and the subset whose text is wanted. Text is held at p iff
// X < p <= C and p is not in NB.
type StreamDetail struct {
	E  string  `json:"e"`
	C  int64   `json:"c"`
	X  int64   `json:"x"`
	NB []int64 `json:"nb,omitempty"`
	W  []int64 `json:"w,omitempty"`
}

// Observation is what a peer last reported for one author: its digest and,
// when known, its detail (nil: unknown). The row for the room log has
// Author "" and Log set instead.
type Observation struct {
	Author string
	Digest string
	Detail []StreamDetail // nil: unknown; empty: holds nothing
	Log    *PeerLog
	At     time.Time
}

// PeerLog is a peer's last reported log heads and held or refused entries.
type PeerLog struct {
	Heads []string `json:"heads"`
	Extra []string `json:"extra,omitempty"`
}

// Copy is a dated report that a peer held a record's text [R5].
type Copy struct {
	Peer string
	At   time.Time
}

// Refusal counts what one peer sent that was refused, by reason code.
type Refusal struct {
	Peer   string
	Reason string
	Count  int64
	LastAt time.Time
}

// StreamHeld is what this machine holds of one stream, for its summary:
// every position in [1, Contig]; NoBody, the positions held without text
// (by its durable mark, or past retention by now); Missing, the subset
// whose text is missing here.
type StreamHeld struct {
	Author  string
	Epoch   string
	Contig  int64
	NoBody  []int64
	Missing []int64
	// MissingIDs are the records at Missing, in the same order.
	MissingIDs []string
}

// Summary returns, for every stream of room with something held, what is
// held, ordered by author then epoch. Text past retention by now counts as
// expired, as a reader treats it.
func (rd *Reader) Summary(room string, now time.Time) ([]StreamHeld, error) {
	rows, err := rd.tx.Query(`SELECT author, epoch, contig FROM streams WHERE room = ? AND contig > 0
		ORDER BY author, epoch`, room)
	if err != nil {
		return nil, err
	}
	var out []StreamHeld
	idx := map[[2]string]int{}
	for rows.Next() {
		var s StreamHeld
		if err := rows.Scan(&s.Author, &s.Epoch, &s.Contig); err != nil {
			rows.Close()
			return nil, err
		}
		idx[[2]string{s.Author, s.Epoch}] = len(out)
		out = append(out, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = rd.tx.Query(`SELECT author, epoch, seq, id, reason, created FROM records WHERE room = ?
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

// Observed returns what peer last reported in room, by author (the log row
// first, with Author "").
func (rd *Reader) Observed(room, peer string) ([]Observation, error) {
	rows, err := rd.tx.Query(`SELECT author, digest, detail, at FROM observed WHERE room = ? AND peer = ?
		ORDER BY author`, room, peer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Observation
	for rows.Next() {
		var o Observation
		var detail sql.NullString
		var at int64
		if err := rows.Scan(&o.Author, &o.Digest, &detail, &at); err != nil {
			return nil, err
		}
		o.At = fromMS(at)
		if detail.Valid {
			if o.Author == "" {
				o.Log = &PeerLog{}
				if err := json.Unmarshal([]byte(detail.String), o.Log); err != nil {
					return nil, errors.New("stored peer log does not parse")
				}
			} else {
				o.Detail = []StreamDetail{}
				if err := json.Unmarshal([]byte(detail.String), &o.Detail); err != nil {
					return nil, errors.New("stored detail does not parse")
				}
			}
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Copies returns the peers whose last observed summary said they held the
// text of the record at (author, epoch, seq) of room, with when that was
// learned, ordered by peer [R5]. Only observed summaries are used.
func (rd *Reader) Copies(room, author, epoch string, seq int64) ([]Copy, error) {
	rows, err := rd.tx.Query(`SELECT peer, detail, at FROM observed WHERE room = ? AND author = ?
		AND detail IS NOT NULL ORDER BY peer`, room, author)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Copy
	for rows.Next() {
		var peer, detail string
		var at int64
		if err := rows.Scan(&peer, &detail, &at); err != nil {
			return nil, err
		}
		var d []StreamDetail
		if err := json.Unmarshal([]byte(detail), &d); err != nil {
			return nil, errors.New("stored detail does not parse")
		}
		if HoldsText(d, epoch, seq) {
			out = append(out, Copy{Peer: peer, At: fromMS(at)})
		}
	}
	return out, rows.Err()
}

// HoldsText reports whether detail says the text at (epoch, seq) is held.
func HoldsText(detail []StreamDetail, epoch string, seq int64) bool {
	for _, s := range detail {
		if s.E != epoch {
			continue
		}
		if seq <= s.X || seq > s.C {
			return false
		}
		i := sort.Search(len(s.NB), func(i int) bool { return s.NB[i] >= seq })
		return i == len(s.NB) || s.NB[i] != seq
	}
	return false
}

// Copies is Reader.Copies in its own read transaction.
func (s *Store) Copies(ctx context.Context, room, author, epoch string, seq int64) (out []Copy, err error) {
	err = s.View(ctx, func(rd *Reader) error {
		out, err = rd.Copies(room, author, epoch, seq)
		return err
	})
	return out, err
}

// Refusals returns the refusal counts of room, by peer then reason.
func (s *Store) Refusals(ctx context.Context, room string) ([]Refusal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT peer, reason, count, last_at FROM refusals WHERE room = ?
		ORDER BY peer, reason`, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Refusal
	for rows.Next() {
		var r Refusal
		var at int64
		if err := rows.Scan(&r.Peer, &r.Reason, &r.Count, &at); err != nil {
			return nil, err
		}
		r.LastAt = fromMS(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Tx is one write transaction of the pull exchange: applying one request's
// log entries, offer and observation, or one response's log entries, page
// and observation, together (PROTOCOL 4.4).
type Tx struct{ tx *sql.Tx }

// Exchange runs fn inside one write transaction after checking that room
// is held, not left, and (if gen is not zero) at generation gen. If fn
// returns an error nothing is written. It returns Changes as it was after
// this transaction was counted: a later read of the same value means
// nothing was written since.
func (s *Store) Exchange(ctx context.Context, room string, gen int64, fn func(*Tx) error) (int64, error) {
	var ver int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := checkGen(tx, room, gen); err != nil {
			return err
		}
		if err := fn(&Tx{tx: tx}); err != nil {
			return err
		}
		ver = s.changes.Load() + 1 // the raise s.tx makes before committing
		return nil
	})
	return ver, err
}

// Reader reads inside the transaction, seeing what it wrote so far.
func (t *Tx) Reader() *Reader { return &Reader{tx: t.tx} }

// ApplyLog is Store.ApplyLog inside the transaction.
func (t *Tx) ApplyLog(b LogBatch) (int64, error) { return applyLog(t.tx, b) }

// PutRecordPlanned is Store.PutRecordPlanned inside the transaction.
func (t *Tx) PutRecordPlanned(r *core.VerifiedRecord, gen int64, plan func(*Reader) (RecordPlan, error)) (Outcome, error) {
	if r == nil || r.ID() == "" {
		return "", ErrUnverified
	}
	return putPlanned(t.tx, r, gen, plan)
}

// MaxHintedStreams bounds the streams one peer's reports can make this
// machine keep for one author of a room while none of them is held: a
// report names any epoch it likes, unsigned (F9). A stream counts against
// the peer whose report set its known position, also when that peer took
// it over from another with a higher report.
const MaxHintedStreams = 32

// NoteKnown records peer's report, at at, of the positions it holds of
// author's streams in room (epoch to contiguous position): a stream not
// held yet is created with contig 0; a known position (and its date)
// rises to a higher report from any peer, and the peer whose report set it
// can lower it, also by no longer reporting the stream, which deletes a
// stream nothing of is held. Known positions are hints. At most
// MaxHintedStreams streams that hold nothing are kept per author on this
// peer's word, lowest epochs first: past that, a report neither creates
// one nor takes over one another peer's report set.
func (t *Tx) NoteKnown(room, peer, author string, known map[string]int64, at time.Time) error {
	type row struct {
		contig, known int64
		from          string
	}
	have := map[string]row{}
	rows, err := t.tx.Query(`SELECT epoch, contig, known, known_from FROM streams WHERE room = ? AND author = ?`, room, author)
	if err != nil {
		return err
	}
	for rows.Next() {
		var e string
		var r row
		if err := rows.Scan(&e, &r.contig, &r.known, &r.from); err != nil {
			rows.Close()
			return err
		}
		have[e] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	hinted := 0 // streams that hold nothing, on this peer's word, that it still reports
	var gone []string
	for e, r := range have {
		if r.from != peer {
			continue
		}
		if _, ok := known[e]; !ok {
			gone = append(gone, e)
		} else if r.contig == 0 {
			hinted++
		}
	}
	sort.Strings(gone)
	for _, e := range gone {
		if err := t.lowerKnown(room, peer, author, e, have[e].contig, 0, at, true); err != nil {
			return err
		}
	}
	epochs := make([]string, 0, len(known))
	for e := range known {
		epochs = append(epochs, e)
	}
	sort.Strings(epochs)
	for _, epoch := range epochs {
		c := known[epoch]
		r, ok := have[epoch]
		switch {
		case !ok:
			if c <= 0 || hinted >= MaxHintedStreams {
				continue
			}
			if _, err := t.tx.Exec(`INSERT INTO streams (room, author, epoch, contig, known, known_at, known_from)
				VALUES (?,?,?,0,?,?,?)`, room, author, epoch, c, ms(at), peer); err != nil {
				return err
			}
			hinted++
			continue
		case c > r.known && r.from != peer && r.contig == 0:
			// Taking over a hint another peer set counts against this one.
			if hinted >= MaxHintedStreams {
				continue
			}
			hinted++
		case c > r.known, c < r.known && r.from == peer:
		default:
			continue
		}
		if c < r.known {
			if err := t.lowerKnown(room, peer, author, epoch, r.contig, c, at, false); err != nil {
				return err
			}
			continue
		}
		if _, err := t.tx.Exec(`UPDATE streams SET known = ?, known_at = ?, known_from = ? WHERE room = ? AND author = ? AND epoch = ?`,
			c, ms(at), peer, room, author, epoch); err != nil {
			return err
		}
	}
	return nil
}

// lowerKnown lowers a stream's known position, which peer's report set,
// to c on peer's word at at, or forgets it (gone: peer no longer names the
// stream). If another peer's last observed detail reports the stream
// higher than c, the known position becomes that report instead, with
// that peer and the time it was observed: one peer's report never erases
// another's (a peer could otherwise raise a hint another peer set and
// then drop it). Such a stream counts against the peer whose report it
// takes, and is not taken while that peer is at MaxHintedStreams. A
// stream that holds nothing and is lowered to nothing goes.
func (t *Tx) lowerKnown(room, peer, author, epoch string, contig, c int64, at time.Time, gone bool) error {
	k, from, kat, err := t.otherKnown(room, peer, author, epoch)
	if err != nil {
		return err
	}
	if k > c && k > 0 {
		n := 0
		if contig == 0 {
			if err := t.tx.QueryRow(`SELECT count(*) FROM streams WHERE room = ? AND author = ? AND known_from = ? AND contig = 0
				AND epoch <> ?`, room, author, from, epoch).Scan(&n); err != nil {
				return err
			}
		}
		if n < MaxHintedStreams {
			_, err := t.tx.Exec(`UPDATE streams SET known = ?, known_at = ?, known_from = ? WHERE room = ? AND author = ? AND epoch = ?`,
				k, kat, from, room, author, epoch)
			return err
		}
	}
	if gone || c <= 0 && contig == 0 {
		return t.dropKnown(room, author, epoch, contig)
	}
	_, err = t.tx.Exec(`UPDATE streams SET known = ?, known_at = ?, known_from = ? WHERE room = ? AND author = ? AND epoch = ?`,
		c, ms(at), peer, room, author, epoch)
	return err
}

// otherKnown is the highest position of stream (author, epoch) of room
// that a peer other than peer reports in its last observed detail, the
// latest such report on a tie, with that peer and when it was observed
// (this machine's clock, in milliseconds); 0 when none reports it.
func (t *Tx) otherKnown(room, peer, author, epoch string) (known int64, from string, at int64, err error) {
	rows, err := t.tx.Query(`SELECT peer, detail, at FROM observed WHERE room = ? AND author = ? AND peer <> ?
		AND detail IS NOT NULL ORDER BY peer`, room, author, peer)
	if err != nil {
		return 0, "", 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var p, detail string
		var when int64
		if err := rows.Scan(&p, &detail, &when); err != nil {
			return 0, "", 0, err
		}
		var ds []StreamDetail
		if json.Unmarshal([]byte(detail), &ds) != nil {
			continue
		}
		for _, d := range ds {
			if d.E == epoch && (d.C > known || d.C == known && known > 0 && when > at) {
				known, from, at = d.C, p, when
			}
		}
	}
	return known, from, at, rows.Err()
}

// dropKnown forgets a stream's known position: a stream that holds
// nothing (contig 0) is deleted, any other keeps its row.
func (t *Tx) dropKnown(room, author, epoch string, contig int64) error {
	q := `UPDATE streams SET known = 0, known_from = '' WHERE room = ? AND author = ? AND epoch = ?`
	if contig == 0 {
		q = `DELETE FROM streams WHERE room = ? AND author = ? AND epoch = ? AND contig = 0`
	}
	_, err := t.tx.Exec(q, room, author, epoch)
	return err
}

// dropHints forgets every known position peer's reports set in room for
// an author not in keep: a whole observation that leaves an author out
// says the peer reports none of its streams (F9).
func (t *Tx) dropHints(room, peer string, keep map[string]bool) error {
	rows, err := t.tx.Query(`SELECT DISTINCT author FROM streams WHERE room = ? AND known_from = ?`, room, peer)
	if err != nil {
		return err
	}
	var gone []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return err
		}
		if !keep[a] {
			gone = append(gone, a)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, a := range gone {
		if err := t.NoteKnown(room, peer, a, nil, time.Time{}); err != nil {
			return err
		}
	}
	return nil
}

// Observe replaces what peer reported in room with obs, an observation
// dated at: every author row not in obs is deleted (the peer no longer
// takes it), except the log row when obs has none. A detail is stored as
// given (nil: unknown). The caller decides which of two observations is
// the later: one made from a request built earlier never replaces one
// made later (node.Exchange orders them by when they were made, not by
// the clock, which can step back).
func (t *Tx) Observe(room, peer string, at time.Time, obs []Observation) error {
	return t.observe(room, peer, at, obs, true)
}

// ObservePart replaces only the rows of obs (authors whose detail a peer
// sent, and its log), keeping the rest of what was observed of it.
func (t *Tx) ObservePart(room, peer string, at time.Time, obs []Observation) error {
	return t.observe(room, peer, at, obs, false)
}

// ObserveLog replaces only peer's log row.
func (t *Tx) ObserveLog(room, peer string, at time.Time, log PeerLog) error {
	_, err := t.tx.Exec(`INSERT INTO observed (room, peer, author, digest, detail, at) VALUES (?,?,'','',?,?)
		ON CONFLICT (room, peer, author) DO UPDATE SET detail = excluded.detail, at = excluded.at`,
		room, peer, jsonText(log), ms(at))
	return err
}

// ForgetLog deletes what was observed of peer's log in room, so the next
// request sends the accepted log from the start (PROTOCOL 4.4).
func (t *Tx) ForgetLog(room, peer string) error {
	_, err := t.tx.Exec(`DELETE FROM observed WHERE room = ? AND peer = ? AND author = ''`, room, peer)
	return err
}

func (t *Tx) observe(room, peer string, at time.Time, obs []Observation, whole bool) error {
	keep := map[string]bool{}
	for _, o := range obs {
		keep[o.Author] = true
		var detail any
		switch {
		case o.Log != nil:
			detail = jsonText(o.Log)
		case o.Detail != nil:
			detail = jsonText(o.Detail)
		}
		if _, err := t.tx.Exec(`INSERT INTO observed (room, peer, author, digest, detail, at) VALUES (?,?,?,?,?,?)
			ON CONFLICT (room, peer, author) DO UPDATE SET digest = excluded.digest, detail = excluded.detail,
			at = excluded.at`, room, peer, o.Author, o.Digest, detail, ms(at)); err != nil {
			return err
		}
	}
	if !whole {
		return nil
	}
	rows, err := t.tx.Query(`SELECT author FROM observed WHERE room = ? AND peer = ?`, room, peer)
	if err != nil {
		return err
	}
	var gone []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return err
		}
		if !keep[a] && a != "" {
			gone = append(gone, a)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, a := range gone {
		if _, err := t.tx.Exec(`DELETE FROM observed WHERE room = ? AND peer = ? AND author = ?`, room, peer, a); err != nil {
			return err
		}
	}
	return t.dropHints(room, peer, keep)
}

// Refuse adds n to peer's count of reason in room.
func (t *Tx) Refuse(room, peer, reason string, n int, at time.Time) error {
	if n <= 0 {
		return nil
	}
	_, err := t.tx.Exec(`INSERT INTO refusals (room, peer, reason, count, last_at) VALUES (?,?,?,?,?)
		ON CONFLICT (room, peer, reason) DO UPDATE SET count = count + excluded.count, last_at = excluded.last_at`,
		room, peer, reason, n, ms(at))
	return err
}
