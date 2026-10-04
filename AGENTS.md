# Instructions for coding agents

This file is for an AI coding agent that works in this repository: Claude
Code, Codex, Cursor, or any other tool that reads `AGENTS.md`. It lists the
rules that a change here must meet. Meet them before review.

This file adds no rule. [CONTRIBUTING.md](CONTRIBUTING.md) and
[AI_POLICY.md](AI_POLICY.md) are the source. If this file disagrees with
them, they win, and this file must be corrected.

## What Brig is

Brig runs coding agents in a microVM sandbox. `cmd/brig` is the CLI and
`cmd/brigd` is the session daemon. Everything else is under `internal/`, one
package per concern. Each package states its job in its `// Package`
comment. Read that comment before you change a package.

[docs/README.md](docs/README.md) is the map of the documentation.

## The two promises

Brig exists for two properties. A change that weakens either one is a bug,
even when every test passes.

1. **The guest reaches only the host directories Brig names for it.** Brig
   handles paths that the guest can influence through an `os.Root`, never
   by joining strings (see `internal/wrap/rootio.go`).
2. **The guest gets only the credentials you name for it.** Brig forwards
   values by name, never in argv. It never writes them into the guest home.

[docs/security.md](docs/security.md) lists the limits of both promises. If
a change moves either promise, say so in the pull request and update that
page in the same change.

Dependency rules enforce some of these guarantees. For example,
`internal/hostsrc/arch_test.go` fails if the run path can reach the host
credential importer. If a test of that kind fails, the design forbids the
change. Do not work around the test.

For these promises, a **negative** test is worth more than a positive test.
Good examples are "the denied variable was not forwarded" and "the planted
symlink was refused and the file outside is untouched". "The sandbox booted"
proves little.

## Before a change is done

Run what CI runs. CI does not call `make`, so `make all` alone is not the
whole gate:

```bash
gofmt -l .
go vet ./...
go test -race ./...
script/smoke.sh
```

`gofmt -l .` must print nothing. `script/smoke.sh` runs the real binary
against a stub runtime, so it runs anywhere and needs no VM.

For a documentation change, also run:

```bash
script/check-retired-spellings.sh
script/check-claims.sh
```

| Script | Fails when |
| --- | --- |
| `script/check-retired-spellings.sh` | A doc teaches a command spelling scheduled for removal. [docs/migration.md](docs/migration.md) lists the current spellings. |
| `script/check-claims.sh` | A sentence that [docs/claims.md](docs/claims.md) quotes from [docs/security.md](docs/security.md) no longer matches. |

The local gates cannot catch these:

- **The real runtime.** The smoke test stubs `hull` (macOS) and `nerdctl`
  (Linux). A change to how Brig invokes either one can pass every local gate
  and still be wrong. If the change touches the run, exec or credential
  path, say in the pull request whether you ran it with a real `brig run`.
  Do not claim that you did if you did not.
- **The keychain.** On macOS, the tests of `internal/secret` use the real
  login keychain, under service names prefixed `sh.brig.secret.test.`.

## Tests are kept

`script/check-tests-kept.sh` fails CI when a test name disappears. It exists
because a revert once removed the work of four merged pull requests while
every run was green.

Do not delete or rename a test to make a change pass. If you intend a
rename, say why in the pull request and ask a maintainer for the
`removes-tests` label.

## Dependencies

Brig keeps its direct dependencies to three. It runs `cosign`, `oras` and
`security` as subprocesses and does not link them. Do not add a module to
`go.mod` unless the pull request says why the standard library or a
subprocess is not sufficient.

## Commits and pull requests

- Use [Conventional Commits](https://www.conventionalcommits.org/):
  `type(scope): description`. Use the imperative mood and no full stop. The
  header is at most 72 characters. The types are `feat`, `fix`, `docs`,
  `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`.
- In the body, say **why**, not what: the problem, the approach, and any
  consequence that is not obvious.
- Reference issues with a trailer: `Fixes: #<n>` or `Refs: #<n>`.
- Sign off every commit: `git commit -s`.
- Put one logical change in each pull request.
- Fill in the [template](.github/pull_request_template.md).
- Open the pull request as a draft. Mark it ready only when CI is green.

## Scope and honesty

These rules are the short form of [AI_POLICY.md](AI_POLICY.md):

- Keep changes small and bounded. A large mechanical rewrite, or a
  speculative fix, costs a maintainer more to review than it saves.
- Report what happened. Do not state that tests passed, a bug reproduced or
  a behaviour was verified unless it did.
- Match the code around you. Comments here explain **why** the code is the
  way it is, often with the failure that it prevents. Keep those comments
  when you edit, and write new comments the same way.
- A vulnerability is never a public issue or pull request. Follow
  [SECURITY.md](SECURITY.md).
