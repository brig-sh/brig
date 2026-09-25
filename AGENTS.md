# Instructions for coding agents

This file is for an AI coding agent working in this repository: Claude Code,
Codex, Cursor, or any other tool that reads `AGENTS.md`. It collects the rules
a change here has to meet, so an agent can meet them before a person has to
point them out in review.

It adds no rule of its own. [CONTRIBUTING.md](CONTRIBUTING.md) and
[AI_POLICY.md](AI_POLICY.md) are the source; where this file and they
disagree, they win, and this file is the one to fix.

## What Brig is

Brig runs coding agents in a microVM sandbox. `cmd/brig` is the CLI and
`cmd/brigd` is the session daemon. Everything else is under `internal/`, one
package per concern, and each package states its job in its `// Package`
comment. Read that comment before changing a package.

[docs/README.md](docs/README.md) is the map of the documentation.

## The two promises

Most of Brig is ordinary Go. Two properties are the reason it exists, and a
change that weakens either is a bug even when every test passes:

1. **The guest reaches only the host directories Brig names for it.** Paths
   the guest can influence are handled through an `os.Root`, never by joining
   strings (see `internal/wrap/rootio.go`).
2. **The guest gets only the credentials you name for it.** Values are
   forwarded by name, never in argv, and never written into the guest home.

[docs/security.md](docs/security.md) lists the limits of both. If a change
moves either promise, say so in the pull request and update that page in the
same change.

Some of these guarantees are enforced as dependency rules rather than
behaviour. `internal/hostsrc/arch_test.go` fails if the run path can reach the
host credential importer. If a test like that fails, the design is telling
you no. Do not work around it.

A **negative** test is worth more here than a positive one: "the denied
variable was not forwarded", "the planted symlink was refused and the file
outside is untouched". "The sandbox booted" proves little.

## Before you call a change done

Run what CI runs. CI does not call `make`, so `make all` alone is not the
whole gate:

```bash
gofmt -l .
go vet ./...
go test -race ./...
script/smoke.sh
```

`gofmt -l .` must print nothing. `script/smoke.sh` drives the real binary
against a stub runtime, so it runs anywhere, with no VM needed.

For a documentation change, also run:

```bash
script/check-retired-spellings.sh
```

It fails a doc that teaches a command spelling scheduled for removal.
[docs/migration.md](docs/migration.md) lists the current spellings.

Things the local gates cannot catch:

- **The real runtime.** The smoke test stubs `hull` (macOS) and `nerdctl`
  (Linux). A change to how Brig invokes either can pass everything here and
  still be wrong. If the change touches the run, exec or credential path,
  say plainly in the pull request whether it was exercised with a real
  `brig run`. Do not claim it was if it was not.
- **The keychain.** `internal/secret`'s tests use the real login keychain on
  macOS, under service names prefixed `sh.brig.secret.test.`.

## Tests are kept

`script/check-tests-kept.sh` fails CI when a test name disappears, because a
revert once took the work of four merged pull requests with every run green. Do not
delete or rename a test to make a change pass. If a rename is genuinely
intended, say why in the pull request and ask a maintainer for the
`removes-tests` label.

## Dependencies

Brig has three direct dependencies, deliberately. It shells out to `cosign`,
`oras` and `security` rather than linking them. Do not add a module to
`go.mod` without saying in the pull request why the standard library or a
subprocess will not do.

## Commits and pull requests

- [Conventional Commits](https://www.conventionalcommits.org/): `type(scope):
  description`, imperative mood, no full stop, header at most 72 characters.
  Types: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`,
  `ci`, `chore`, `revert`.
- The body says **why**, not what: the problem, the approach, any
  non-obvious consequence.
- Reference issues with a trailer: `Fixes: #<n>` or `Refs: #<n>`.
- Sign off every commit: `git commit -s`.
- One logical change per pull request. Fill in the
  [template](.github/pull_request_template.md), open it as a draft, and mark
  it ready only once CI is green.

## Scope and honesty

From [AI_POLICY.md](AI_POLICY.md), in short:

- Keep changes small and bounded. A large mechanical rewrite, or a
  speculative fix, costs a maintainer more to review than it saves.
- Report what happened. Do not state that tests passed, a bug reproduced or
  a behaviour was verified unless it did.
- Match the code around you. Comments here explain **why** a thing is the
  way it is, often with the failure it prevents. Keep that when you edit, and
  write new comments the same way.
- A vulnerability is never a public issue or pull request. Follow
  [SECURITY.md](SECURITY.md).
