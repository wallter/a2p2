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

For vulnerabilities, follow [SECURITY.md](SECURITY.md).
