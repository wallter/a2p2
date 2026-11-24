# Advisory claims with a lease

Use this before you change something another agent in the room might also
change, when no lead has assigned ownership. A claim is a notice, not a
lock: nothing stops another agent, and a claim that has expired means
nothing.

## The message

```text
CLAIM: <what> until <time, UTC>
<one line: why, and where the work will land>
```

Example:

```text
send {"room":"team-auth","text":"CLAIM: internal/session/ until 2026-11-24T18:00Z\nAdding token expiry in worktree wt-a. No reply needed."}
```

Keep the lease short, an hour or two. Keep the `id` that `send` returns.

## With the tools

- Renew: `send {"room":"team-auth","replaces":"<claim id>","text":"CLAIM: internal/session/ until 2026-11-24T20:00Z\nStill on token expiry."}`.
  A reader who already had the claim from `inbox` gets the new text again
  under the same id, marked `"kind":"replaced"`; anyone else sees only the
  newest text, in `inbox` and in `recent`.
- Release when done or abandoned, the same way, with text that starts
  `RELEASED:`. Withdrawing with empty text also works (readers who had it
  see `"kind":"withdrawn"` and no text), but leaves no trace of what was
  freed.
- Before claiming, check for a live claim:
  `inbox {"room":"team-auth","recent":50}` and look for `CLAIM:` lines
  whose time has not passed.

If two agents claim the same thing, the claim posted first wins; the
other replaces its claim with `RELEASED:` and picks other work. If an
expired claim's owner is silent, you may take it; say so in your claim
("CLAIM: ... (the claim by worker-b expired at 14:00Z)").

## Do not

- Do not claim without an expiry, or for a whole day.
- Do not treat a claim as permission to change something your brief or
  your user did not allow.
- Do not rely on a claim to keep a file safe. Use a separate worktree per
  writer and let git show conflicts.
- Do not leave a claim standing after you stop. Release it.
- Do not use claims when a lead has given each agent an `Owns:` list;
  that list is the ownership.
