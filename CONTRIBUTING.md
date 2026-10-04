# Contributing to Brig

## Build and test locally

| Command | What it does |
| --- | --- |
| `make build` | Builds `./brig` and `./brigd`. |
| `make test` | Runs `go test -race ./...`. |
| `make vet` | Runs `go vet ./...`. |
| `make all` | Runs vet, test and build, in that order. |

`script/smoke.sh` runs the real binary against a stub runtime, so it needs
no VM. It cannot catch a change to how Brig invokes the real runtime: `hull`
on macOS, `nerdctl` on Linux. If your pull request touches the run, exec or
credential path, boot a real sandbox before you open it.

On macOS, the tests of `internal/secret` use the real login keychain. They
create and delete items under service names prefixed `sh.brig.secret.test.`
(`internal/secret/keychain_darwin_test.go:21`).

## What CI checks

CI does not call `make`. It runs these steps
([.github/workflows/ci.yml](.github/workflows/ci.yml)):

- `gofmt -l .`. The build fails if a file is not formatted.
- `go vet ./...`.
- `go test -race -covermode=atomic -coverprofile=coverage.out ./...`. On a
  push to `main`, CI uploads the coverage to Codecov.
- `script/smoke.sh`.
- `script/check-claims.sh`, with its `--self-test` and the `--self-test` of
  `script/claims-vm.sh`. It checks the claims table in `docs/claims.md`.
- `script/check-retired-spellings.sh`. It fails a doc that teaches a command
  spelling scheduled for removal.
- `script/check-binaries.sh`, with its `--self-test`. It fails when a tracked
  file is a compiled executable or object file.
- `sh -n` and `shellcheck` over `install.sh`, and `script/test-install.sh`,
  which runs its Linux path against a stub curl and fixture releases.
- `bash -n`, `shellcheck` and `--self-test` for
  `script/network-isolation-vm.sh`. The self-test checks the guards of the
  script. The comparison on a real VM is a manual run.
- A cross-compile for `darwin/arm64` and one for `linux/amd64`.
- `script/check-tests-kept.sh`. It fails when a test disappears. If your
  pull request renames a test, or removes one on purpose, label it
  `removes-tests`. Give the reason in the description. `removes-tests` is
  the only label that CI reads.
- `goreleaser check` and a full snapshot build. They run the release config
  before a tag depends on it.

`gofmt` and `go vet` are the only static checks. The repository has no
`golangci-lint` configuration and no lint target in the Makefile.

## Dependencies

Brig has four direct dependencies:

| Module | Use |
| --- | --- |
| `sigs.k8s.io/yaml` | Profiles |
| `golang.org/x/sys` | Terminal and process calls |
| `github.com/godbus/dbus/v5` | The Linux secret store |
| `github.com/brig-sh/hull/pkg/telemetry` | Usage and crash telemetry, under the answer hull keeps. Its only dependency is `golang.org/x/sys` |

The project keeps this list short. Brig runs `cosign`, `oras` and `security`
as subprocesses and does not link them. That keeps the attack surface small
for a tool that handles credentials. Do not add a dependency unless the pull
request says why a subprocess or the standard library is not sufficient.

## The two promises

Brig exists for two properties. A change that weakens either one is a bug,
even when every test passes.

1. **The guest reaches only the host directories Brig names for it.** Brig
   mounts the guest home as the home of the sandbox. If you name a project
   on the run line, Brig also mounts that project read-write at
   `/work/<name>`, as a second host directory. The agent can change those
   real project files. [docs/security.md](docs/security.md) names further
   limits, including what the hostmount of a profile can add. Brig writes
   everything into either directory from the host, as you. It handles those
   attacker-controlled paths through an `os.Root` and does not join strings
   to build them.
2. **The guest gets only the credentials you name for it.** Brig reads
   values from your environment per invocation and forwards them by name, so
   they never appear in `ps`. It never writes them into the guest home.

[docs/security.md](docs/security.md) lists the limits of both promises. If
a change moves either promise, say so in the pull request and update that
page in the same change. People read that page before they trust Brig with a
credential.

For these two promises, a **negative** test is worth more than a positive
test. These tests catch a regression:

- "The denied variable was not forwarded."
- "The planted symlink was refused, and the file outside the guest home is
  untouched."

"The sandbox booted" does not catch one.

## Commits

Brig uses [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>[optional scope]: <description>

[optional body]

[optional footer(s)/trailers]
```

- Limit the header to 72 characters.
- Write the description in the imperative mood ("add", not "added"). Do not
  end it with a full stop.
- `type` is one of `feat`, `fix`, `docs`, `style`, `refactor`, `perf`,
  `test`, `build`, `ci`, `chore` or `revert`. The release notes
  ([cliff.toml](cliff.toml)) group `feat`, `fix`, `refactor` and `docs`
  commits. They leave out `test`, `chore`, `ci`, `build` and `style`
  commits.
- Use a scope when it adds clarity, for example `fix(secret): ...`.
- In the body, say why the change exists, not what it does. Cover the
  problem, the approach you chose, and any consequence that is not obvious.
- Reference an issue with a trailer. Use `Fixes: #<number>` when the commit
  resolves the issue, and `Refs: #<number>` when it does not.
- Sign off every commit: `git commit -s`. This adds the `Signed-off-by`
  trailer, which a CI check requires.

## Pull requests and review

- Put one logical change in each pull request. Put an unrelated fix in a
  separate pull request.
- Fill in the pull request template.
- Open the pull request as a draft. Mark it ready for review only when CI is
  green.
- A merge needs at least one approval.
- At merge, add the `Reviewed-by` trailer to the commits. A rebase-and-merge
  does not add it.
- Rebase-and-merge is the preferred merge method. It keeps the commits as
  distinct units in the history of `main`.

## Docs

Before you open a documentation pull request, run
`script/check-retired-spellings.sh`. [docs/README.md](docs/README.md) is the
map of the documentation.

Brand assets (logos, marks, the architecture diagram) are in `assets/`. See
[assets/README.md](assets/README.md) for the rules.

## Releasing

Only maintainers cut a release. See [Releasing](docs/releasing.md).

## Issues

Use issues to track bugs and feature requests.

**A vulnerability is not a public issue.** Brig handles credentials, so a
flaw in that handling must reach the maintainers privately. Do not open an
issue or a pull request for a vulnerability. Follow
[SECURITY.md](SECURITY.md), which uses the private vulnerability reporting
of GitHub.

For a bug report, fill in the issue template. Include the problem, the steps
to reproduce, the `brig version` and runtime version, your environment, and
the full `brig doctor` output.

For a feature request, first read [What Brig will not do](docs/non-goals.md).
It lists what Brig will not do for now, and why.

## AI policy

AI-assisted development is welcome in Brig. See [AI_POLICY.md](AI_POLICY.md).
