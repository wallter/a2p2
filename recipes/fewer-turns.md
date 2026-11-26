# Fewer turns

Use this when a team's cost is model turns and tokens. These are
defaults; a room's [charter](room-charter.md) may change any of them.

## Does the work need a team

For steps that depend on each other, one agent may do as well, with fewer
turns and less cost. A team may help work that splits into parts with
their own files ([lead a team](lead-a-team.md)).

## Send whole answers

A message the reader must question costs two turns. Put in it:

- the result, first;
- the evidence: the command and its exit status, or `file:line`;
- what you did not do or check;
- what is needed next, and from whom.

Send all your findings in one message. A handoff ([handoff](handoff.md))
is the full form. Send `CLAIM:` once, not "working on it" again.

## Cap the critique rounds

Decide the number before the first round, in the brief or the charter. The
default is three rounds of critique and fix, then stop; it is a chosen
number, and a charter may change it.

- Each round the critic sends one `FINDINGS:` message, numbered, each with
  `file:line` and a severity ([review](review.md)). "No findings" ends it.
- The author answers all findings in one reply: fixed, or declined with
  the reason.
- Whoever set the number decides when rounds end. A finding still open
  after the last round goes to the lead as `NEEDS-HUMAN:` or `BLOCKED:`,
  not into another round.

## Do not spend a turn to acknowledge

- If a message ends "No reply needed.", send nothing back. End yours the
  same way when none is needed.
- `ack {"id":"<id>"}` reports a message handled and sends no text. Use it
  for one addressed to you, after you read it, when nothing needs saying.
- `reply` sends text and also reports it handled. Use it only when the
  reader needs the text.

## Address narrowly

- Name the readers in `to`; a critic's message is not for the room.
- Read other rooms at level addressed; `@everyone` only for what all
  must read ([sub-teams](sub-teams.md)).
- Send the lead one summary per round, not each exchange.

## Waiting

A message does not wake an idle agent; it sees it when it calls `inbox`.
While an exchange is live, wait with `inbox {"wait_seconds":30}` between
steps; 30 is the most one call waits. Stop when the work or the round
limit ends. If a reader may be idle, do other work meanwhile.
