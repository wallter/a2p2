# A README review by four members

On 2026-11-24 (UTC) the lead asked a team for a last editorial pass over
README.md. One member edited; three posted numbered findings as replies to
the kickoff, and the editor took or declined each. See the
[manifest](manifest.json), with local paths replaced, and what
`team report` wrote: [report.md](report.md) and [report.json](report.json).

| Member | Harness and model | Job | Time | Findings |
|---|---|---|---|---|
| sol-editor | Codex CLI, gpt-6.2-sol | edit README.md and llms.txt | 337s | answered 18 |
| luna-check | Codex CLI, gpt-6-luna, read only | check commands, flags and tools | 51s | 0 |
| hydrogen-reader | Antigravity CLI, gemini-4-hydrogen-high | read it cold | 98s | 10 |
| sonnet-installer | Claude Code, sonnet, Read/Grep/Glob only | walk through the install | 39s | 8 |

The run took 337 seconds. Every member connected within 5.1 seconds and
exited 0. The room held 17 messages: 8 from the launcher, 1 from the lead
and none from the person.

What worked: three model families read the same text for different jobs,
and the editor answered all 18 numbered findings by number.

What went wrong: sonnet-installer sent one message that began with its
plan, then its eight findings, then `DONE:`. The report shows no final
report for it, because it reads first lines. The editor took those
findings in README.md only, declined the SKILL.md changes as outside its
scope, and worked around the flag-order trap in the text rather than the
CLI. The rest were applied afterwards by hand. The lesson, now in the
[status lines](../../../../recipes/status-lines.md) recipe: findings go in
their own message whose first line says so, for example `FINDINGS:`.
