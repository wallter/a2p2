# Releasing a2p2

How versions are numbered, what is run before a release, and the steps.
Releases are cut by a maintainer from `main`.

## Versions

- Versions follow [Semantic Versioning](https://semver.org/). Tags are
  `vMAJOR.MINOR.PATCH`, annotated (`git tag -a`), on a commit of `main`.
- While the major version is 0: a patch release fixes things and changes
  no format; a minor release may change behaviour, the tools, the local
  API or a format, and the changelog says which.
- A release candidate is `vX.Y.Z-rc.N`. It is published as a pre-release.
  It is optional: tag one only for a release that changes the wire
  protocol or the database schema, where trying the published binaries
  against an existing installation before the final tag is worth the
  extra cycle. Other releases go straight to the final tag.
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
the section). The notes are the section's first paragraph (its summary),
then the install block in `scripts/release-notes-head.md`, then the rest;
`scripts/release-notes.sh <tag>` prints the whole notes, and
`make verify-release` checks that the published notes contain that block.
The release is titled `a2p2 <tag>`.

## What is tested where

Most testing is local, on purpose: it keeps the hosted CI small.

| Gate | Where | What |
|---|---|---|
| `make check` | locally, before every push | fmt, vet, the Windows build gate, licences, the requirement register, the recipes check, the race tests, the launcher's tests, all at the same time; the slow test packages run as shards ([TESTING.md](TESTING.md#how-make-test-runs)) |
| `make prerelease` | locally, before every tag | `make check`, `make lint`, `make vuln`, the same tests on Linux in a container (`make check-linux`), and one `make chaos` run at its default length (real daemons killed and partitioned in containers; a shorter run is reported inconclusive), all at the same time, each to its own log in `.prerelease/`. When every step passed on a clean tree it writes `.prerelease/stamp` with the commit and the time |
| `make e2e-local`, `make e2e HARNESS=...` | locally, by hand, before a minor release | real harnesses; these spend model tokens or need local models |
| CI on GitHub | pull requests that touch code, and on request | one Linux job: `make check` (two shards on the two-core runner) and `make lint`. A run by hand chooses with `what`: `linux` (the default), `windows` or `all`; the Windows job runs `go build`, `go vet` and `go test` on `windows-latest` without the deep `internal/node` schedules, with full output only for a failing package and every skipped test listed |
| Release workflow on GitHub | a pushed `v*` tag | `go vet`, the changelog section exists, build, the binary reports the tag, publish. It does not repeat the tests: they ran in `make prerelease` on the same commit, and `make tag` refuses a commit without that stamp |

Nothing runs on GitHub for a push to `main`. macOS is covered by local
development and `make prerelease`; Linux by the container run and
pull-request CI.

The gate is enforced by `make tag`, not by GitHub. A tag made by hand
with `git tag` and `git push` skips it, and the release workflow would
publish that commit untested. Do not tag by hand.

## Steps

1. `main` is clean and pushed. Move the entries under `[Unreleased]` in
   CHANGELOG.md into a new `## [X.Y.Z] - YYYY-MM-DD` section, add the
   compare links at the bottom, commit and push.
2. For a minor release, or when a harness changed: run the real-harness
   checks by hand and record the versions in [HARNESSES.md](HARNESSES.md).
3. `make prerelease` (about four minutes, most of it the chaos run). Fix
   what it finds and run it again; a release does not go out on a failure
   or on an inconclusive chaos run. The log of each step is in
   `.prerelease/`.
4. `make tag VERSION=vX.Y.Z`. It refuses unless the stamp names HEAD, the
   tree is clean, HEAD is on `origin/main`, CHANGELOG.md has the section
   (`scripts/changelog.sh`) and the tag exists neither here nor on
   `origin`. Then it makes the annotated tag and pushes it.
5. `make verify-release VERSION=vX.Y.Z` (needs `gh`, signed in). It waits
   for the release workflow, checks that the release has exactly the six
   binaries and `SHA256SUMS`, downloads the binary for this machine and
   `SHA256SUMS` into a temporary directory, checks the checksum, runs
   `gh attestation verify` on the binary (pinned to this repository's
   `release.yml`; it fails if there is none), checks that `version` reports
   the tag, and prints the asset list and the first
   lines of the notes. Read the notes once.
6. For a release candidate (wire protocol or database changes only), do
   steps 3 to 5 with `vX.Y.Z-rc.N`, try the binaries, then tag the final
   version on the same commit: its stamp still holds.
7. Start the next cycle: CHANGELOG.md already has an empty `[Unreleased]`.

## What a release contains

Static binaries `a2p2_<os>_<arch>` for linux and darwin on amd64 and
arm64, and `a2p2_windows_<arch>.exe` for windows on amd64 and arm64 (from
0.2.0), built by GitHub Actions from the tag with no build cache, CGO off
and `-trimpath`, and `SHA256SUMS`. From the next version after 0.2.3,
the release workflow also makes a GitHub artifact attestation (SLSA build
provenance) for the binaries before it publishes. It is signed through the
public Sigstore instance, so each release adds a public log entry that
names the repository, the commit and the workflow. The binaries are not
code-signed or notarised for macOS or Windows. Before a
release that changes Windows code, run the ci workflow by hand once with
`what` set to `windows` and read its log.
`go install github.com/wallter/a2p2/cmd/a2p2@vX.Y.Z` builds the same source.
