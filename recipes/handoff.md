# Handoff with evidence

Use this when you finish a delegated task, stop part way, or pass work to
someone else. The reader should be able to decide what to do next from
this message and the repository alone, without asking you.

## The message

Reply to the brief, so the handoff lands in its thread:

```text
<DONE|BLOCKED|NEEDS-HUMAN>: <one line: what state the work is in>
Changed: <files, with a word on each>
Commit: <hash and branch, or "none, uncommitted in <worktree path>">
Checks: <each command run and its real exit status or result>
Deviations: <where you did not follow the brief, and why>
Risks: <what might be wrong, what you did not check>
State: done | blocked on <what> | needs a decision from the person
```

Example:

```text
reply {"id":"2d6c44fdc9d52438505cf24c6a6123ea","text":"DONE: expired sessions are refused\nChanged: internal/session/store.go (expiry check), store_test.go (two tests)\nCommit: none, uncommitted in /work/wt-a\nChecks: go test ./internal/session exit 0; go vet ./... exit 0\nDeviations: none\nRisks: the 24 hour limit is a constant, not a setting\nState: done"}
```

## With the tools

- `reply` with the brief's `id` sends to the brief's author and anyone
  else who posted in that thread, and reports the brief handled. Its
  result says whether the report was sent; "sent before" means you had
  already reported it, which is fine.
- If the brief asked you to report to someone other than its author, add
  `to`: `reply {"id":"<brief id>","to":["reviewer@1a2b3c4d"],"text":"..."}`.
  `to` replaces the default recipients: the author and the others in the
  thread no longer receive it unless you list them too.
- A long log does not belong in the message; a message holds about 16 KB.
  Commit it or name the file that holds it.

As the reader: `inbox` shows the reply with `parent` set to the brief's id.
Check the evidence before relying on it: run the named checks yourself or
ask for a [review](review.md).

## Do not

- Do not write "tests pass" without the command and its exit status.
- Do not report `DONE:` with unrun checks. Say `BLOCKED:` or list them
  under Risks.
- Do not hide a deviation. A brief you did not follow exactly is still
  useful work if the reader knows where it differs.
- Do not keep going after the handoff. Stop, or wait in `inbox` for a new
  brief.
