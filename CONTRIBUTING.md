# Contributing to a2p2

Start with the [repository guide](AGENTS.md). The
[documentation index](docs/README.md) points to the current design,
protocol, requirements and test evidence.

Use the Go version in [go.mod](go.mod). Run `make check` for formatting,
vet, the requirement register and race-enabled tests. If your change
touches a harness, see the [test guide](docs/TESTING.md) for the difference
between automated checks and a run with the real harness, and record any
real-harness run with its version in the [harness notes](docs/HARNESSES.md).

A change that a user or an agent would notice adds a line under
`[Unreleased]` in [CHANGELOG.md](CHANGELOG.md). See
[releasing](docs/RELEASING.md) for how versions are numbered.

Contributions use the [Apache-2.0 license](LICENSE). Sign off each commit
with `git commit -s` to certify your right to submit the contribution,
including work produced with an AI agent. See the
[Developer Certificate of Origin](https://developercertificate.org/).

## Where things go

- A problem: an issue. The [issue form](.github/ISSUE_TEMPLATE) asks for
  what a report needs.
- An idea or a question: a discussion, in `ideas` or `q-a`.
- A change: a pull request. The template lists what it needs.
- A vulnerability: follow [SECURITY.md](SECURITY.md). Do not post it.

Never paste message text from rooms, invite tokens, keys, home paths or
host names anywhere public. The
[reporting recipe](recipes/reporting-a-problem.md) says more.

An idea becomes work like this. A maintainer answers in the discussion.
If it is accepted, the maintainer opens an issue labelled `accepted` and
adds an entry to [DECISIONS.md](docs/DECISIONS.md) when the rules for
changes in [AGENTS.md](AGENTS.md) require one. Open a pull request after
that, or earlier if the change is small.

Contributions by agents are welcome under the same rules. The agent's user
is responsible for what it submits. Agents can follow the
[idea recipe](recipes/suggesting-an-idea.md). See also the
[code of conduct](CODE_OF_CONDUCT.md) and [support](SUPPORT.md).
