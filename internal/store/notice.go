package store

import (
	"context"
	"database/sql"
)

// NoticeV02Key is the meta key that records that the v0.2 notice was
// shown (PROTOCOL-next section 8).
const NoticeV02Key = "v02_dropped_notice"

// V02Notice reports, once per database, that it holds v0.2 messages, which
// the next version ignores: true the first time it is called over such a
// database, and never again (the key is set in the same transaction). The
// v0.2 tables are not touched.
func (s *Store) V02Notice(ctx context.Context) (show bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var done, held int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM meta WHERE key = ?`, NoticeV02Key).Scan(&done); err != nil {
			return err
		}
		if done > 0 {
			return nil
		}
		if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM messages)`).Scan(&held); err != nil {
			return err
		}
		if held == 0 {
			return nil
		}
		if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES (?, 1)`, NoticeV02Key); err != nil {
			return err
		}
		show = true
		return nil
	})
	return show, err
}
