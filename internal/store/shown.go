package store

// What a read showed an agent, and the order readers see (D-reading).
// This is migration 12.
//
// History pages by (created, author, seq, id), the order of every view of
// messages (node.sortEntries): an author's own messages of one millisecond
// come in the order written, and others' by author, the same on every
// machine. records_reading replaces records_history's (created, id).
//
// shown holds two kinds of row, both local reading state like reads and skips:
//
//   - A receipt: a full inbox listing showed the agent the entry with this
//     key, at this version. Seen confirms only an entry with a receipt, and
//     a message with one is never kept out of the inbox by a level changed
//     since. Confirming the entry deletes its receipt: the read replaces it.
//   - A text mark: key "s:" + msg, written when a thread, branch, recent or
//     skipped view showed the agent the message's text.
//
// Either, with a non-empty version, lets the agent report the message
// handled. Neither moves a cursor or marks anything seen. The reads table
// could not carry them: a row there is a confirmation, and its versions
// decide which replace and withdraw notices an agent gets.

import (
	"context"
	"database/sql"
	"errors"
)

const schemaReading = `
DROP INDEX records_history;
CREATE INDEX records_reading ON records (room, kind, created, author, seq, id);
CREATE TABLE shown (
  agent TEXT NOT NULL, room TEXT NOT NULL, key TEXT NOT NULL, version TEXT NOT NULL, msg TEXT NOT NULL,
  PRIMARY KEY (agent, room, key));
CREATE INDEX shown_msg ON shown (agent, room, msg);
`

// TextMarkKey is the key of the text mark of message msg.
func TextMarkKey(msg string) string { return "s:" + msg }

// Shown is one row of what a read showed an agent: a receipt (Key is the
// entry's key) or a text mark (Key is TextMarkKey(Msg)).
type Shown struct {
	Key     string
	Version string
	Msg     string
}

// Show records rows for agent in room, in one transaction: ShowAt with
// no generation, so only a room no longer held is refused.
func (s *Store) Show(ctx context.Context, agent, room string, rows []Shown) error {
	return s.ShowAt(ctx, agent, room, 0, rows)
}

// ShowAt records rows for agent in room, in one transaction. A row for a
// key already recorded takes the new version. The read that showed them
// was an earlier transaction, at generation gen of room (RoomGen; 0: not
// known): if room is no longer held, or was left and joined again since
// (another generation), nothing is recorded, so no receipt outlives the
// leave into a rejoin (F70).
func (s *Store) ShowAt(ctx context.Context, agent, room string, gen int64, rows []Shown) error {
	if len(rows) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		switch err := checkGen(tx, room, gen); {
		case errors.Is(err, ErrLeft), errors.Is(err, ErrNotFound), errors.Is(err, ErrStaleGeneration):
			return nil
		case err != nil:
			return err
		}
		for _, r := range rows {
			if _, err := tx.Exec(`INSERT INTO shown (agent, room, key, version, msg) VALUES (?,?,?,?,?)
				ON CONFLICT (agent, room, key) DO UPDATE SET version = excluded.version, msg = excluded.msg`,
				agent, room, r.Key, r.Version, r.Msg); err != nil {
				return err
			}
		}
		return nil
	})
}

// RoomGen returns room's generation, or 0 when room is not held (never
// joined, or left).
func (rd *Reader) RoomGen(room string) (int64, error) {
	gen, left, err := roomGen(rd.tx, room)
	switch {
	case errors.Is(err, ErrNotFound):
		return 0, nil
	case err != nil:
		return 0, err
	case left:
		return 0, nil
	}
	return gen, nil
}

// Receipts returns agent's receipts in room: entry key -> version listed.
// Text marks are not receipts and are left out.
func (rd *Reader) Receipts(agent, room string) (map[string]string, error) {
	rows, err := rd.tx.Query(`SELECT key, version FROM shown WHERE agent = ? AND room = ? AND key NOT LIKE 's:%'`,
		agent, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ShownText reports whether a read showed agent the text of message msg
// in room: a receipt or text mark with a version.
func (rd *Reader) ShownText(agent, room, msg string) (bool, error) {
	var n int
	err := rd.tx.QueryRow(`SELECT count(*) FROM shown WHERE agent = ? AND room = ? AND msg = ? AND version <> ''`,
		agent, room, msg).Scan(&n)
	return n > 0, err
}
