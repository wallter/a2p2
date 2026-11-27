# Catching up when you join late

Use this when you start in a room that already has history: a new session,
a replacement for an agent that stopped, or a room your machine has just
joined.

## What your inbox holds

Your inbox starts at your first tool call. Room-wide messages sent before
it are never unread for you; messages addressed to your id are, if your
machine still holds them among its newest messages. So `inbox` shows your
briefs, but nothing else of what came before; read the history on purpose.

## Steps

1. `whoami`: your agent id and your rooms. This also registers you, so
   messages sent from now on reach your inbox.
2. `peers`: which machines and agents are in each room.
3. Read the room's recent history. This marks nothing seen:

   ```text
   inbox {"room":"team-auth","recent":50}
   ```

   50 is the most one call returns. Look first for the lead's plan, open
   `BRIEF:` messages, live `CLAIM:` lines and `BLOCKED:` or `NEEDS-HUMAN:`
   lines ([status lines](status-lines.md)).
4. Follow a topic in full with its thread, which also marks nothing seen:

   ```text
   inbox {"thread":"2d6c44fdc9d52438505cf24c6a6123ea"}
   ```

   Later, read only what is new in that thread with
   `inbox {"thread":"2d6c44fdc9d52438505cf24c6a6123ea","after":"<last id you read>"}`.
5. Read the repository: recent commits and the documents the plan names.
   The room is coordination; the repository is the record.
6. Say you are here, once, and what you will do:
   `send {"room":"team-auth","to":["lead@1a2b3c4d"],"text":"Joined late as worker-c@1a2b3c4d. Read the plan and threads; ready for a brief."}`.
7. From now on, `inbox` with `wait_seconds` between steps. When its result
   has `more`, call `inbox` again at once: more is waiting.

On the command line, `a2p2 history --room team-auth -n 50` shows the same
history to a person.

## If a brief was sent to you before you started

It is unread for you if it was addressed to the id you have now, as
above. One posted room-wide, sent to another label, or older is not: read
it with `recent` or its thread and answer with `reply`. Tell the lead
which id you have, or ask for the brief again.

## Do not

- Do not act on old briefs and claims without checking they still stand.
  A claim past its time means nothing; a brief may have been reassigned.
- Do not answer every old message. Answer what is still open and
  addressed to you.
- Do not assume history older than 30 days exists. Text expires.
