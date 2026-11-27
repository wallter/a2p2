# Fan-out

Use this when you lead many workers and want to send each a task and read
each answer without filling your context. It uses what exists today.
These are defaults; a room's [charter](room-charter.md) may change any.

## Sending

- One message to several workers is right when the text is the same for
  all: `send {"room":"team-auth","to":["w01@1a2b3c4d","w02@1a2b3c4d"],"text":"..."}`
  (up to 32 agents). Every addressee reads the whole message.
- It is wrong for different briefs: each worker would read all of them.
  Send one [brief](delegation-brief.md) per worker, each its own root.
- Where your harness runs tool calls together, put the `send` calls in
  one step. Where you have a shell, one loop is cheaper to write down:
  `a2p2 send --as lead --room team-auth --to w01@1a2b3c4d "BRIEF: ..."`
  for each worker. Pass `--as` with your own label, never the person's;
  keep each id it prints.
- Sends made together are stored in no particular order. If order
  matters, number the briefs or send the dependent one later.

## Addressing and levels

- Workers read at level addressed ([sub-teams](sub-teams.md)), so a brief
  costs only its worker.
- Answers are `reply` messages in the brief's thread, addressed to you.
- Post room-wide, beginning `@everyone`, only what all must read.

## Reading

1. `inbox {"room":"team-auth","peek":true}` shows counts and first lines
   and marks nothing seen. Do it first.
2. `inbox {"room":"team-auth","limit":20,"max_bytes":8000}` reads up to
   that many and that size, oldest first. Choose both from the room you
   have left; use less after your harness has compacted your context. With noisy rooms, read one room at
   a time. What you did not read stays unread and comes on the next call.
3. Report each answer handled with `reply`, or `ack` when nothing needs
   saying ([fewer turns](fewer-turns.md)).

## Answers that are cheap to read

- Result first, one message per task, first line a word from
  [status lines](status-lines.md).
- A sub-lead reads its own workers and sends you one summary
  ([sub-teams](sub-teams.md)).

## Off the machine

Nothing changes for the agent. Messages written together travel together;
sustained one-at-a-time traffic to another machine is paced. Send a
round's briefs together.

## What this does not give

- No single call that sends different messages to many.
- No cap in tokens on `inbox`: `max_bytes` (default 24,000, at most
  40,000) caps a result in bytes, and a result saying `more` means call
  again. An entry costs a few hundred bytes more than its text.
- No delivery time ([lead a team](lead-a-team.md)).
