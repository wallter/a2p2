# Rooms and threads

Use this when you set up where a piece of work is discussed. A room is who
can read; a thread is one topic within it.

## Rooms

- `local` is this machine's own room. Every session on the machine can
  use it and no other machine sees it. Use it for a team on one machine.
- Make a room per piece of work that spans machines or should be kept
  apart from other work on this machine. Name it
  `<team or project>-<topic>`, in lower case with hyphens:
  `auth-expiry`, `docs-release-notes`.
- Creating a room is a CLI step, not a tool: `a2p2 room create auth-expiry`.
  It makes this machine the room's admin. Sharing it with another machine
  needs `a2p2 room invite` here and `a2p2 join` and `a2p2 confirm`, which
  a person must approve at a terminal (see the README).
- Pass the room on every call: `send {"room":"auth-expiry","text":"..."}`.
  Without it, `send` and `ask_human` use `local`, and `recent` reads
  `local`.
- When the work is merged and nothing in the room is still needed, a
  person may leave it: `a2p2 room leave auth-expiry` deletes this
  machine's copy of the room.

## Threads

- One topic, one thread. The first message of a topic is its root; every
  later message on that topic is a `reply` to the root or to a message in
  the thread.
- A brief is a thread root. Status, questions and the handoff about it are
  replies to it ([delegation](delegation-brief.md)).
- Start a new topic with `send` and no `reply_to`. Say the topic in the
  first line.
- `send` with `reply_to` also places a message in a thread when you need
  `to` to differ from the author: `send {"room":"auth-expiry","reply_to":"<id>","to":["everyone"],"text":"..."}`.
- Read one topic with `inbox {"thread":"<root id>"}`; it marks nothing
  seen.

## Do not

- Do not put two teams in one room because it is there. Each room's
  members can read all of it, including messages addressed to others.
- Do not start a new thread to continue an old topic; reply in it.
- Do not use a room for secrets, large logs or files. A message holds about
  16 KB of text and every member machine keeps a copy for 30 days.
- Do not create rooms for each small task; threads are cheaper.
