package store

import (
	"context"
	"database/sql"
)

// schemaDropV02 is migration 10: the v0.2 delivery tables go. Before they
// do, meta records (v02Removed) whether they held any messages or rooms,
// so that a person is still told once that the upgrade removed them. The
// v0.2 rooms are the rows without a create entry (empty create_id) that the
// next version never wrote to (gen 0); the v0.2 message counter goes from
// meta, and the v0.2 columns of rooms and agents with it. agents stays.
const schemaDropV02 = `
INSERT OR IGNORE INTO meta (key, value)
  SELECT 'v02_removed', 1
  WHERE EXISTS (SELECT 1 FROM messages) OR EXISTS (SELECT 1 FROM rooms WHERE create_id = '' AND gen = 0);
DELETE FROM rooms WHERE create_id = '' AND gen = 0;
DELETE FROM meta WHERE key = 'seq';
DROP TABLE marks;
DROP TABLE deliveries;
DROP TABLE messages;
DROP TABLE members;
DROP TABLE invites;
ALTER TABLE rooms DROP COLUMN owner;
ALTER TABLE rooms DROP COLUMN version;
ALTER TABLE rooms DROP COLUMN created_at;
ALTER TABLE agents DROP COLUMN joined_n;
`

// v02Removed is the meta key migration 10 sets when it removed v0.2
// messages or rooms.
const v02Removed = "v02_removed"

// NoticeV02Key is the meta key that records that the v0.2 notice was
// shown to a person (PROTOCOL-next section 8).
const NoticeV02Key = "v02_dropped_notice"

// V02Pending reports whether the v0.2 notice is still to be shown: the
// upgrade removed v0.2 messages or rooms (migration 10) and the notice has
// not been shown yet. It changes nothing; V02Shown records the showing.
func (s *Store) V02Pending(ctx context.Context) (bool, error) {
	var pending bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM meta WHERE key = ?)
		AND NOT EXISTS (SELECT 1 FROM meta WHERE key = ?)`, v02Removed, NoticeV02Key).Scan(&pending)
	return pending, err
}

// V02Shown records that the v0.2 notice was shown; it is not shown again.
// It reports whether this call was the one that recorded it, so that two
// readers racing do not both show it.
func (s *Store) V02Shown(ctx context.Context) (first bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT OR IGNORE INTO meta (key, value) VALUES (?, 1)`, NoticeV02Key)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		first = n == 1
		return err
	})
	return first, err
}
