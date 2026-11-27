package store

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// summaryPair is Summary and summaryScan of room in one transaction.
func summaryPair(rd *Reader, room string, now time.Time) (cached, scanned []StreamHeld, err error) {
	if cached, err = rd.Summary(room, now); err != nil {
		return nil, nil, err
	}
	scanned, err = rd.summaryScan(room, now)
	return cached, scanned, err
}

func sameSummary(a, b []StreamHeld) bool {
	norm := func(xs []StreamHeld) []StreamHeld {
		out := make([]StreamHeld, len(xs))
		for i, x := range xs {
			if len(x.NoBody) == 0 {
				x.NoBody = nil
			}
			if len(x.Missing) == 0 {
				x.Missing, x.MissingIDs = nil, nil
			}
			out[i] = x
		}
		return out
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

// The summary a pull reads (Reader.Summary) keeps the records without
// live text in memory, updated by the writes themselves. On random
// schedules of every kind of write that changes what a summary says
// (records stored with text, as headers, not admitted, undecided or
// already past retention; repairs; withdraws; replacements; pruning, with
// and without undecided pairs kept; access re-decided by a log batch; a
// room left and joined again; transactions rolled back; a summary read inside a write
// transaction before it commits; the clock passing retention with nothing
// pruned; the store reopened), the summary equals a fresh read of the
// records table after every step, read now and 31 days later.
func TestSummaryCacheEqualsAReadOfTheRoomAfterEveryKindOfWrite(t *testing.T) {
	for seed := int64(1); seed <= 12; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { summaryCacheSchedule(t, seed, 400) })
	}
}

func summaryCacheSchedule(t *testing.T, seed int64, steps int) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(seed))
	s, path := openTest(t)
	const room2 = "r_ffeeddccbbaa99887766554433221100"
	if err := s.AddRoom(ctx, room2, "two", "", t0); err != nil {
		t.Fatal(err)
	}
	rooms := []string{testRoom, room2}
	left := map[string]bool{}
	keys := []ed25519.PrivateKey{testKey("a"), testKey("b"), testKey("c")}
	epochs := []string{"00000000000000e1", "00000000000000e2"}
	type sk struct {
		room string
		k    int
		e    int
	}
	last := map[sk]*core.VerifiedRecord{}
	seq := map[sk]int64{}
	msgs := map[sk][]*core.VerifiedRecord{} // messages, for withdraws and replacements
	var headers []*core.VerifiedRecord      // stored without text: repairable
	now := t0
	next := func(r string, k, e int, f core.RecordFields) *core.VerifiedRecord {
		key := sk{r, k, e}
		f.Room, f.Epoch, f.Seq = r, epochs[e], seq[key]+1
		if p := last[key]; p != nil {
			f.Prev = p.ID()
		}
		if f.Created.IsZero() {
			f.Created = now.Add(-time.Duration(rng.Intn(40*24)) * time.Hour)
		}
		return mk(t, keys[k], f)
	}
	stored := func(key sk, v *core.VerifiedRecord, out Outcome) {
		if out == OutcomeStored {
			last[key], seq[key] = v, v.Fields().Seq
			if v.Fields().Kind == core.KindMessage {
				msgs[key] = append(msgs[key], v)
			}
		}
	}
	check := func(step int, what string) {
		t.Helper()
		err := s.View(ctx, func(rd *Reader) error {
			for _, r := range rooms {
				for _, at := range []time.Time{now, now.Add(31 * 24 * time.Hour)} {
					c, sc, err := summaryPair(rd, r, at)
					if err != nil {
						return err
					}
					if !sameSummary(c, sc) {
						return fmt.Errorf("room %.6s at %s: cached\n%+v\nscanned\n%+v", r, at.Sub(now), c, sc)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("seed %d step %d (%s): %v", seed, step, what, err)
		}
	}
	errRollback := errors.New("rolled back")
	for step := 0; step < steps; step++ {
		r := rooms[rng.Intn(len(rooms))]
		if left[r] {
			r = testRoom
		}
		k, e := rng.Intn(len(keys)), rng.Intn(len(epochs))
		key := sk{r, k, e}
		var what string
		switch op := rng.Intn(15); {
		case op < 4:
			what = "message"
			v := next(r, k, e, core.RecordFields{Text: fmt.Sprint("m", step)})
			o := PutOpts{Now: now, MayRead: rng.Intn(5) > 0, Undecided: rng.Intn(6) == 0}
			rec := v
			if o.Undecided {
				rec = v.WithoutBody()
			}
			out, err := s.PutRecord(ctx, rec, o)
			if err != nil {
				t.Fatal(err)
			}
			stored(key, v, out)
		case op == 4:
			what = "header"
			v := next(r, k, e, core.RecordFields{Text: fmt.Sprint("h", step)})
			out, err := s.PutRecord(ctx, v.WithoutBody(), PutOpts{Now: now, MayRead: true})
			if err != nil {
				t.Fatal(err)
			}
			stored(key, v, out)
			if out == OutcomeStored {
				headers = append(headers, v)
			}
		case op == 5 && len(headers) > 0:
			what = "repair"
			v := headers[rng.Intn(len(headers))]
			if _, err := s.PutRecord(ctx, v, PutOpts{Now: now, MayRead: rng.Intn(4) > 0}); err != nil && !errors.Is(err, ErrNotFound) {
				t.Fatal(err)
			}
		case op == 6 && len(msgs[key]) > 0:
			what = "withdraw"
			target := msgs[key][rng.Intn(len(msgs[key]))]
			v := next(r, k, e, core.RecordFields{Kind: core.KindWithdraw, Ref: target.ID(), Created: now})
			out, err := s.PutRecord(ctx, v, PutOpts{Now: now, MayRead: true})
			if err != nil {
				t.Fatal(err)
			}
			stored(key, v, out)
		case op == 7 && len(msgs[key]) > 0:
			what = "replace"
			target := msgs[key][rng.Intn(len(msgs[key]))]
			v := next(r, k, e, core.RecordFields{Kind: core.KindReplace, Ref: target.ID(), Version: int64(step + 1), Text: fmt.Sprint("r", step)})
			out, err := s.PutRecord(ctx, v, PutOpts{Now: now, MayRead: rng.Intn(4) > 0})
			if err != nil {
				t.Fatal(err)
			}
			stored(key, v, out)
		case op == 8:
			what = "prune"
			if _, err := s.ExpireText(ctx, now); err != nil {
				t.Fatal(err)
			}
		case op == 9:
			what = "prune keeping undecided"
			kept := [2]string{r, nodeOf(keys[k])}
			if _, err := s.ExpireTextWith(ctx, now, func(*Reader) (map[[2]string]bool, error) {
				return map[[2]string]bool{kept: true}, nil
			}); err != nil {
				t.Fatal(err)
			}
		case op == 10:
			what = "access"
			hidden, undecided := nodeOf(keys[rng.Intn(len(keys))]), nodeOf(keys[rng.Intn(len(keys))])
			_, err := s.ApplyLog(ctx, LogBatch{Room: r, Now: now, Access: func(*Reader) (Access, error) {
				return Access{Readable: func(author string, f core.RecordFields, id string) (TextAccess, error) {
					switch {
					case author == hidden && f.Seq%2 == 0:
						return TextHidden, nil
					case author == undecided && f.Seq%3 == 0:
						return TextUndecided, nil
					}
					return TextReadable, nil
				}}, nil
			}})
			if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrLeft) {
				t.Fatal(err)
			}
		case op == 11:
			what = "rolled back"
			v := next(r, k, e, core.RecordFields{Text: fmt.Sprint("x", step)})
			_, err := s.Exchange(ctx, r, 0, func(tx *Tx) error {
				if _, err := tx.PutRecordPlanned(v, 0, func(*Reader) (RecordPlan, error) {
					return RecordPlan{Now: now}, nil
				}); err != nil {
					return err
				}
				if _, err := queryNoted(tx.tx, `UPDATE records SET body = NULL, reason = ? WHERE room = ? AND seq = 1 `+returningNoted,
					ReasonWithdrawn, r); err != nil {
					return err
				}
				return errRollback
			})
			if !errors.Is(err, errRollback) && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrLeft) {
				t.Fatal(err)
			}
		case op == 12:
			what = "read inside a write"
			v := next(r, k, e, core.RecordFields{Text: fmt.Sprint("w", step)})
			commit := rng.Intn(2) == 0
			var out Outcome
			_, err := s.Exchange(ctx, r, 0, func(tx *Tx) error {
				var err error
				if out, err = tx.PutRecordPlanned(v, 0, func(*Reader) (RecordPlan, error) {
					return RecordPlan{Now: now, Text: TextAccess(rng.Intn(3))}, nil
				}); err != nil {
					return err
				}
				if _, err := queryNoted(tx.tx, `UPDATE records SET body = NULL, reason = ? WHERE room = ? AND seq = 2 AND reason = 'live' `+returningNoted,
					ReasonExpired, r); err != nil {
					return err
				}
				for _, at := range []time.Time{now, now.Add(31 * 24 * time.Hour)} {
					c, sc, err := summaryPair(tx.Reader(), r, at)
					if err != nil {
						return err
					}
					if !sameSummary(c, sc) {
						return fmt.Errorf("inside the write: cached\n%+v\nscanned\n%+v", c, sc)
					}
				}
				if !commit {
					return errRollback
				}
				return nil
			})
			switch {
			case err == nil:
				stored(key, v, out)
			case errors.Is(err, errRollback), errors.Is(err, ErrNotFound), errors.Is(err, ErrLeft):
			default:
				t.Fatalf("seed %d step %d: %v", seed, step, err)
			}
		case op == 13:
			what = "clock"
			now = now.Add(time.Duration(rng.Intn(10*24)) * time.Hour)
		case op == 14 && !left[room2] && rng.Intn(4) == 0:
			what = "leave"
			if _, err := s.LeaveRoom(ctx, room2, now); err != nil {
				t.Fatal(err)
			}
			left[room2] = true
		case op == 14 && left[room2] && rng.Intn(2) == 0:
			// Joined again: the same records arrive anew, from the start.
			what = "join again"
			ent, err := core.NewEntry(testKey("creator"), core.EntryFields{Room: room2, Seq: 1, Kind: core.EntryCreate,
				Label: "two", Created: t0})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateLogRoom(ctx, room2, "two", LogRow{ID: ent.ID(), Author: ent.Author(), Seq: 1,
				Header: ent.Header(), Sig: ent.Sig()}, nil, now); err != nil {
				t.Fatal(err)
			}
			left[room2] = false
			for key := range seq {
				if key.room == room2 {
					delete(seq, key)
					delete(last, key)
					delete(msgs, key)
				}
			}
			// What it held before comes back with its text (the old
			// positions); a fresh stream writes on.
			for k := range keys {
				for e := range epochs {
					key := sk{room2, k, e}
					for i := 0; i < 1+rng.Intn(3); i++ {
						v := next(room2, k, e, core.RecordFields{Text: fmt.Sprint("j", step, i)})
						out, err := s.PutRecord(ctx, v, PutOpts{Now: now, MayRead: true})
						if err != nil {
							t.Fatal(err)
						}
						stored(key, v, out)
					}
				}
			}
		case op == 14 && rng.Intn(8) == 0:
			what = "reopen"
			s.Close()
			var err error
			if s, err = Open(path); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
		default:
			continue
		}
		check(step, what)
	}
	// The cache was used: some stream had positions without text.
	nb := 0
	if err := s.View(ctx, func(rd *Reader) error {
		ss, err := rd.Summary(testRoom, now)
		for _, x := range ss {
			nb += len(x.NoBody)
		}
		return err
	}); err != nil || nb == 0 {
		t.Fatalf("nothing without text (%v): the schedule tested nothing", err)
	}
}

// The reads of the summary seek: the expiry index for live text past
// retention, never the room's history (TestPerRecordLookupsSeekOnTheirKeyNotOnTheWholeRoom
// checks the per-record lookups the same way).
func TestSummaryReadsOnlyTheExpiryIndexAndTheStreams(t *testing.T) {
	s, _ := openTest(t)
	for _, c := range []struct{ name, q, want string }{
		{"live past retention", sqlLiveExpired, "USING INDEX records_expiry (created<?)"},
		{"streams held", `SELECT author, epoch, contig FROM streams WHERE room = ? AND contig > 0 ORDER BY author, epoch`, "streams USING INDEX sqlite_autoindex_streams_1 (room=?)"},
	} {
		plan := strings.Join(queryPlan(t, s, c.q), "; ")
		if !strings.Contains(plan, c.want) || strings.Contains(plan, "SCAN records") {
			t.Fatalf("%s: plan %q, want %q", c.name, plan, c.want)
		}
	}
}
