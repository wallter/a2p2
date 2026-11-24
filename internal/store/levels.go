package store

// Reading levels (U1 to U3; DECISIONS D-levels). levels holds each agent's
// level per room when it is not the default, "all": a room with no row is
// "all", so nothing changes for an agent that never chose. skips holds the
// messages a level change marked skipped for an agent: held when the level
// changed, kept out by the old level and let in by the new one, they stay
// out of the live inbox. Both are local reading state, like cursors and
// reads: no other machine stores or learns them. This is migration 11.

import (
	"context"
	"database/sql"
	"errors"
)

const schemaLevels = `
CREATE TABLE levels (
  agent TEXT NOT NULL, room TEXT NOT NULL, level TEXT NOT NULL,
  PRIMARY KEY (agent, room));
CREATE TABLE skips (
  agent TEXT NOT NULL, room TEXT NOT NULL, msg TEXT NOT NULL,
  PRIMARY KEY (agent, room, msg));
`

// The levels (PROTOCOL section 7).
const (
	LevelAll       = "all"
	LevelAddressed = "addressed"
	LevelNone      = "none"
)

// ValidLevel reports whether l is a level.
func ValidLevel(l string) bool { return l == LevelAll || l == LevelAddressed || l == LevelNone }

// AgentLevel is one agent's level in one room.
type AgentLevel struct {
	Agent string `json:"agent"`
	Room  string `json:"room"`
	Level string `json:"level"`
}

// LevelPlan is what a level change writes: the new level and the messages
// it marks skipped.
type LevelPlan struct {
	Level string
	Skip  []string
}

// Level returns agent's level in room: LevelAll when it never chose one.
func (rd *Reader) Level(agent, room string) (string, error) {
	var l string
	err := rd.tx.QueryRow(`SELECT level FROM levels WHERE agent = ? AND room = ?`, agent, room).Scan(&l)
	if errors.Is(err, sql.ErrNoRows) {
		return LevelAll, nil
	}
	return l, err
}

// Skips returns the messages marked skipped for agent in room.
func (rd *Reader) Skips(agent, room string) (map[string]bool, error) {
	rows, err := rd.tx.Query(`SELECT msg FROM skips WHERE agent = ? AND room = ?`, agent, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out[m] = true
	}
	return out, rows.Err()
}

// SetLevel runs plan over the current state, inside one write transaction,
// and applies the level and the skips it returns.
func (s *Store) SetLevel(ctx context.Context, agent, room string, plan func(*Reader) (LevelPlan, error)) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		p, err := plan(&Reader{tx: tx})
		if err != nil {
			return err
		}
		if !ValidLevel(p.Level) {
			return errors.New("store: not a level: " + p.Level)
		}
		for _, m := range p.Skip {
			if _, err := tx.Exec(`INSERT INTO skips (agent, room, msg) VALUES (?,?,?) ON CONFLICT DO NOTHING`,
				agent, room, m); err != nil {
				return err
			}
		}
		if p.Level == LevelAll {
			_, err = tx.Exec(`DELETE FROM levels WHERE agent = ? AND room = ?`, agent, room)
			return err
		}
		_, err = tx.Exec(`INSERT INTO levels (agent, room, level) VALUES (?,?,?)
			ON CONFLICT (agent, room) DO UPDATE SET level = excluded.level`, agent, room, p.Level)
		return err
	})
}

// Levels returns every level that is not the default, by agent and room.
func (s *Store) Levels(ctx context.Context) ([]AgentLevel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent, room, level FROM levels ORDER BY agent, room`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentLevel{}
	for rows.Next() {
		var l AgentLevel
		if err := rows.Scan(&l.Agent, &l.Room, &l.Level); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
