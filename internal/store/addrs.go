package store

// Peers' addresses (migration 8): where each node said it is, or was
// said to be, with how dialling it went, kept so that a machine that does
// not listen can still reach its peers after a restart. The node layer
// decides which to keep (node.AddrBook); this is its durable copy. The
// same migration records an approval given before the room's log was
// caught up with (requests.not_caught_up).

import (
	"context"
	"database/sql"
	"time"
)

const schemaPeerAddrs = `
ALTER TABLE requests ADD COLUMN not_caught_up INTEGER NOT NULL DEFAULT 0;
CREATE TABLE peer_addrs (
  node TEXT NOT NULL, addr TEXT NOT NULL, source TEXT NOT NULL,
  seen_at INTEGER NOT NULL, failed_at INTEGER NOT NULL DEFAULT 0, fails INTEGER NOT NULL DEFAULT 0,
  good INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (node, addr));
`

// Sources of an address.
const (
	AddrSelf = "self" // the node said so in its own authenticated request
	AddrPeer = "peer" // a third party listed it
)

// PeerAddr is one address of a node, in the order it is tried.
type PeerAddr struct {
	Node, Addr, Source string
	Seen               time.Time
	Failed             time.Time // the first failure since it last worked; zero: none
	Fails              int       // failures since it last worked
	Good               bool      // completed a pinned handshake
}

// PeerAddrs lists every stored address, per node in the order stored.
func (s *Store) PeerAddrs(ctx context.Context) ([]PeerAddr, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node, addr, source, seen_at, failed_at, fails, good FROM peer_addrs
		ORDER BY node, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PeerAddr
	for rows.Next() {
		var a PeerAddr
		var seen, failed int64
		if err := rows.Scan(&a.Node, &a.Addr, &a.Source, &seen, &failed, &a.Fails, &a.Good); err != nil {
			return nil, err
		}
		a.Seen = fromMS(seen)
		if failed != 0 {
			a.Failed = fromMS(failed)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetPeerAddrs replaces node's stored addresses with addrs, in order, in
// one transaction; none forgets the node.
func (s *Store) SetPeerAddrs(ctx context.Context, node string, addrs []PeerAddr) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM peer_addrs WHERE node = ?`, node); err != nil {
			return err
		}
		for _, a := range addrs {
			var failed int64
			if !a.Failed.IsZero() {
				failed = ms(a.Failed)
			}
			if _, err := tx.Exec(`INSERT INTO peer_addrs (node, addr, source, seen_at, failed_at, fails, good)
				VALUES (?,?,?,?,?,?,?)`, node, a.Addr, a.Source, ms(a.Seen), failed, a.Fails, a.Good); err != nil {
				return err
			}
		}
		return nil
	})
}
