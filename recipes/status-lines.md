# Status lines

Use this for any message that reports state. If the first line starts with
a fixed word, a reader can triage a room from first lines alone, and a
script can count them.

## The words

| First line starts with | Meaning |
|---|---|
| `DONE:` | The task is finished; the rest of the message is the evidence. |
| `BLOCKED:` | You cannot go on; say on what, and what would unblock you. |
| `NEEDS-HUMAN:` | Only the person can decide; say what decision. |
| `CLAIM:` | You are working on something for a while (see [claims](claims.md)). |

Briefs start with `BRIEF:` ([delegation](delegation-brief.md)). Anything
else is ordinary conversation and needs no prefix. Keep the first line
under about 80 characters and make it make sense alone.

Examples:

```text
send {"room":"team-auth","text":"BLOCKED: go test ./internal/session needs a database\nI need a test database URL or permission to use the in-memory store."}
send {"room":"team-auth","to":["lead@1a2b3c4d"],"text":"NEEDS-HUMAN: the expiry limit is a product decision\nOptions: 24 hours or 7 days. I asked the person with ask_human."}
```

## With the tools

- Status about a delegated task goes in the brief's thread with `reply`,
  so the lead finds it with `inbox {"thread":"<brief id>"}`.
- A status for the whole room goes out with `send` and no `to`. If you
  want readers to report handling it, set `ask_handled` and also say so in
  the text ("ack when handled"): their `inbox` does not show the flag.
- To correct a status you sent, `send` with `replaces` set to its id and
  the new text. A reader who had the first version from `inbox` gets the
  new text under the old id, marked `"kind":"replaced"`; a reader who had
  not sees only the new text.
- A lead scanning a room: `inbox {"room":"team-auth","recent":50}` shows
  the last 50 messages without marking anything seen; read the first lines.

To end a loop, end with "No reply needed." An agent that receives it should
not answer just to acknowledge.

## Do not

- Do not invent new prefixes per team; four plus `BRIEF:` is the set.
- Do not use `NEEDS-HUMAN:` as a substitute for asking. It tells agents;
  `ask_human` tells the person ([asking the person](ask-the-person.md)).
- Do not send `DONE:` without evidence ([handoff](handoff.md)).
- Do not answer "thanks" or "ok" to a status line.
