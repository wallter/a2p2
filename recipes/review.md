# Review by a second agent

Use this when work is finished and someone other than its author should
check it. A reviewer with a clean context finds what the author's context
hides; a reviewer from another model family does better still.

## The request

Send it as a brief ([delegation](delegation-brief.md)) to an agent that
did not do the work. Give it the change, not your conclusion:

```text
send {"room":"team-auth","to":["reviewer@1a2b3c4d"],"text":"BRIEF: review the session expiry change\nObjective: find correctness bugs and missing tests.\nConstraints: read only; change no files.\nOwns: nothing\nOutput: a reply with one finding per paragraph, each with file:line and severity, or 'no findings'.\nChecks: run go test ./internal/session yourself.\nDone when: you have read the whole diff (git diff main...wt-a).\nReport to: lead@1a2b3c4d, as a reply to this message."}
```

## The review

The reviewer answers with `reply` on the request's `id`:

```text
DONE: 2 findings in the session expiry change
1. high, internal/session/store.go:88: expiry compares local time with UTC.
2. low, store_test.go: no test at exactly 24 hours.
Checks: go test ./internal/session exit 0 on my copy.
```

The author, or the lead, answers each finding: fixed (with the commit),
or declined with the reason. Put that answer in the same thread with
`reply` on the review's `id`.

## With the tools

- `peers` shows which agents exist, and their labels usually name the
  harness (`codex-myproject@1a2b3c4d`), which is how to pick another model
  family.
- The reviewer reads the change from git, not from the room. The room
  carries the request and the findings.
- `inbox {"thread":"<request id>"}` shows the request, the review and the
  answers together, without marking anything seen.

## Do not

- Do not send the author's summary as the thing to review. Send the diff
  or the commit.
- Do not let the reviewer fix what it finds unless the brief gives it
  `Owns:`; one writer per file still holds.
- Do not treat "no findings" as proof. It is one reader's result.
- Do not apply a reviewer's suggestion that your user's constraints rule
  out. A review is information, not permission.
