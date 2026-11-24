-- A database as a2p2 v0.2 left it: schema version 1 (store.migrations[0]
-- exactly as shipped), with v0.2 rooms, members, a message and an agent.
-- Tests load it into a fresh file to exercise the upgrade path.
CREATE TABLE meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL);
INSERT INTO meta VALUES ('seq', 0);
CREATE TABLE rooms (
  id TEXT PRIMARY KEY, label TEXT NOT NULL, owner TEXT NOT NULL,
  version INTEGER NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE members (
  room TEXT NOT NULL, node TEXT NOT NULL, name TEXT NOT NULL, addrs TEXT NOT NULL,
  PRIMARY KEY (room, node));
CREATE TABLE invites (
  secret_hash TEXT PRIMARY KEY, room TEXT NOT NULL, expires_at INTEGER NOT NULL,
  used_by TEXT, used_at INTEGER);
CREATE TABLE messages (
  n INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT NOT NULL, room TEXT NOT NULL, from_node TEXT NOT NULL,
  sender TEXT NOT NULL, recipients TEXT NOT NULL, kind TEXT NOT NULL,
  reply_to TEXT NOT NULL, thread TEXT NOT NULL, seq INTEGER NOT NULL,
  created_at INTEGER NOT NULL, expires_at INTEGER, body TEXT NOT NULL,
  received_at INTEGER NOT NULL,
  UNIQUE (from_node, id), UNIQUE (from_node, seq));
CREATE INDEX messages_room ON messages (room, n);
CREATE INDEX messages_id ON messages (id);
CREATE TABLE deliveries (
  msg_n INTEGER NOT NULL, node TEXT NOT NULL, state TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL,
  PRIMARY KEY (msg_n, node));
CREATE INDEX deliveries_queue ON deliveries (state, node);
CREATE TABLE agents (
  id TEXT PRIMARY KEY, label TEXT NOT NULL, harness TEXT NOT NULL, cwd TEXT NOT NULL,
  joined_n INTEGER NOT NULL, first_seen_at INTEGER NOT NULL, last_seen_at INTEGER NOT NULL);
CREATE TABLE marks (
  msg_n INTEGER NOT NULL, agent TEXT NOT NULL, state TEXT NOT NULL, at INTEGER NOT NULL,
  PRIMARY KEY (msg_n, agent, state));
-- v0.2 state: one shared room with a peer, one message in it, one agent.
INSERT INTO rooms VALUES ('r_v02shared', 'team', 'abababababababababababababababababababababababababababababababab', 1, 1700000000000);
INSERT INTO members VALUES ('r_v02shared', 'abababababababababababababababababababababababababababababababab', 'old', '[]');
INSERT INTO members VALUES ('r_v02shared', 'cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd', 'peer', '["192.0.2.1:7423"]');
INSERT INTO messages (id, room, from_node, sender, recipients, kind, reply_to, thread, seq, created_at, body, received_at)
  VALUES ('m_v02', 'r_v02shared', 'abababababababababababababababababababababababababababababababab', 'human@abababab',
          '[]', 'message', '', 'm_v02', 1, 1700000000000, 'from v0.2', 1700000000000);
UPDATE meta SET value = 1 WHERE key = 'seq';
INSERT INTO agents VALUES ('claude-x@abababab', 'claude-x', 'claude', '/tmp/x', 0, 1700000000000, 1700000000000);
PRAGMA user_version = 1;
