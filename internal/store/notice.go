package store

import (
	"context"
	"database/sql"
)

// NoticeV02Key is the meta key that records that the v0.2 notice was
// shown to a person (PROTOCOL-next section 8).
const NoticeV02Key = "v02_dropped_notice"

// V02Pending reports whether the v0.2 notice is still to be shown: the
// database holds v0.2 state (messages, or rooms without a create entry,
// which only v0.2 made) and the notice has not been shown yet. It changes
// nothing; V02Shown records the showing. The v0.2 tables are never touched.
func (s *Store) V02Pending(ctx context.Context) (bool, error) {
	var pending bool
	err := s.db.QueryRowContext(ctx, `SELECT NOT EXISTS (SELECT 1 FROM meta WHERE key = ?)
		AND (EXISTS (SELECT 1 FROM messages) OR EXISTS (SELECT 1 FROM rooms WHERE create_id = ''))`,
		NoticeV02Key).Scan(&pending)
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
