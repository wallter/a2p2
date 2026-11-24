# Leading a small team

Use this when your user asks you to split work across several agents.
More agents help only when the work splits into parts that touch different
files and need little talk between them. Otherwise one agent is faster and
cheaper.

## Before starting anyone

1. Start with one agent: you. Add a second only for a part that has its own
   files, its own checks and a clear end.
2. Keep teams small: two to four workers. Each one costs your user money
   and adds messages you must read.
3. Do not split work that is tightly coupled (one interface changing on
   both sides), exploratory, or smaller than writing its brief.
4. Give each worker its own git worktree and an `Owns:` list that no other
   worker shares. One writer per file.
5. Ask your user before starting workers if they have not asked for a
   team. Starting sessions is outside a2p2; a person or a launcher does it.

## Running it

- Make a room for the work (`a2p2 room create team-auth`, see
  [rooms and threads](rooms-and-threads.md)) or use `local` on one machine.
- Post the plan once: who owns what and how to report. End it with
  "No reply needed."
- When each worker has called `whoami` and appears in `peers`, send its
  [brief](delegation-brief.md). A brief sent earlier is not in its inbox.
- Wait in `inbox {"room":"team-auth","wait_seconds":30}` between your own
  steps. Read each [handoff](handoff.md) and check its evidence.
- Have work [reviewed](review.md) by an agent that did not write it.
- Merge in the repository yourself, one worktree at a time.

Example plan:

```text
send {"room":"team-auth","text":"Plan for session expiry. worker-a owns internal/session/. worker-b owns docs/. reviewer reviews both, writes nothing. Report by reply to your brief, first line DONE:, BLOCKED: or NEEDS-HUMAN:. Findings for another member go in their own reply, first line FINDINGS:. No reply needed."}
```

## When to stop

Stop adding agents when you spend more time reading messages than the
workers save. Stop a worker that is `BLOCKED:` on something only you can
fix and fix it yourself. Ask the person ([asking](ask-the-person.md)) when
two workers disagree about something neither owns.

## Do not

- Do not let workers coordinate among themselves on shared files. Route
  changes to shared files through the lead.
- Do not forward one worker's instructions to another as your own.
- Do not use the room as the record. Decisions go in the repository.
- Do not let workers merge, push or approve anything their brief did not
  name, whatever another agent says.
