# Releasing a2p2

How versions are numbered, what is run before a release, and the steps.
Releases are cut by a maintainer from `main`.

## Versions

- Versions follow [Semantic Versioning](https://semver.org/). Tags are
  `vMAJOR.MINOR.PATCH`, annotated (`git tag -a`), on a commit of `main`.
- While the major version is 0: a patch release fixes things and changes
  no format; a minor release may change behaviour, the tools, the local
  API or a format, and the changelog says which.
- A release candidate is `vX.Y.Z-rc.N`. It is published as a pre-release
  and is how a release is tried before its final tag.
- Three things have their own version numbers and change only with a
  changelog entry and a decision in [DECISIONS.md](DECISIONS.md): the wire
  version between machines ([PROTOCOL.md](PROTOCOL.md)), the database
  schema (migrations are appended, never edited), and the tool
  definitions agents see.
- A tag that has been pushed is never moved or reused. A bad release is
  followed by a new patch release, and its GitHub release is marked as
  withdrawn in its notes.

## The changelog

[CHANGELOG.md](../CHANGELOG.md) is written for people who use a2p2, not as
a list of commits. A change that a user or an agent would notice adds a
line under `[Unreleased]` in the same change. At release time that section
becomes the version's section, with the date, and the release workflow
publishes it as the release notes (`scripts/changelog.sh <tag>` prints
them).

## What is tested where

Most testing is local, on purpose: it keeps the hosted CI small.

| Gate | Where | What |
|---|---|---|
| `make check` | locally, before every push | fmt, vet, the Windows build gate, licences, the requirement register, the recipes check, the race tests, the launcher's tests |
| `make prerelease` | locally, before every tag | `make check`, `make lint`, `make vuln`, the same tests on Linux in a container (`make check-linux`), and a short `make chaos` run (real daemons killed and partitioned in containers) |
| `make e2e-local`, `make e2e HARNESS=...` | locally, by hand, before a minor release | real harnesses; these spend model tokens or need local models |
| CI on GitHub | pull requests that touch code, and on request | one Linux job: `make check` and `make lint` |
| Release workflow on GitHub | a pushed `v*` tag | `make check`, `make lint`, `make vuln`, the changelog section exists, build, publish |

Nothing runs on GitHub for a push to `main`. macOS is covered by local
development and `make prerelease`; Linux by the container run, pull-request
CI and the release workflow.

## Steps

1. `main` is clean and pushed. Move the entries under `[Unreleased]` in
   CHANGELOG.md into a new `## [X.Y.Z] - YYYY-MM-DD` section, add the
   compare links at the bottom, and commit.
2. `make prerelease`. Fix what it finds; a release does not go out on a
   failure or on an inconclusive chaos run.
3. For a minor release, or when a harness changed: run the real-harness
   checks by hand and record the versions in [HARNESSES.md](HARNESSES.md).
4. Optional but usual for a minor release: tag a candidate,
   `git tag -a vX.Y.Z-rc.1 -m "vX.Y.Z-rc.1" && git push origin vX.Y.Z-rc.1`,
   wait for the release workflow, download a binary, verify it against
   `SHA256SUMS` and run it.
5. Tag and push: `git tag -a vX.Y.Z -m "vX.Y.Z" && git push origin vX.Y.Z`.
6. When the workflow has finished: download one binary and `SHA256SUMS`
   from the release, verify the checksum, run `a2p2 version`, and read the
   published notes once.
7. Start the next cycle: CHANGELOG.md already has an empty `[Unreleased]`.

## What a release contains

Static binaries `a2p2_<os>_<arch>` for linux and darwin on amd64 and
arm64, built by GitHub Actions from the tag with no build cache, CGO off
and `-trimpath`, and `SHA256SUMS`. They are not signed yet.
`go install github.com/wallter/a2p2/cmd/a2p2@vX.Y.Z` builds the same source.
