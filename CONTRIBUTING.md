# Contributing to Brig

## Build and test locally

- `make build` builds `./brig` and `./brigd`.
- `make test` runs `go test -race ./...`.
- `make vet` runs `go vet ./...`.
- `make all` runs vet, test and build, in that order. Run it before you push.

`internal/secret`'s tests touch the real login keychain on macOS. They
create and delete items under service names prefixed `sh.brig.secret.test.`
(`internal/secret/keychain_darwin_test.go:21`).

`script/smoke.sh` drives the real binary against a stub runtime. It cannot
catch a change to how Brig invokes the real one: `hull` on macOS, `nerdctl`
on Linux. Boot a real sandbox before you open a pull request that touches the
run, exec or credential path.

## What CI checks

CI does not call `make`. It runs its own steps
([.github/workflows/ci.yml](.github/workflows/ci.yml)):

- `gofmt -l .`, and fails the build if any file is not formatted.
- `go vet ./...`.
- `go test -race -covermode=atomic -coverprofile=coverage.out ./...`, with
  coverage uploaded to Codecov on pushes to `main`.
- `script/smoke.sh`.
- A cross-compile for `darwin/arm64` and one for `linux/amd64`.
- A check that no test disappeared (`script/check-tests-kept.sh`). Label a
  pull request that renames or deliberately removes a test `removes-tests`,
  and say why in the description. `removes-tests` is the only label CI
  reads.
- `goreleaser check`, and a full snapshot build, so the release config stays
  exercised before a tag depends on it.

There is no linter. No `golangci-lint` configuration exists in this
repository, and the Makefile has no lint target. `gofmt` and `go vet` are the
only static gates.

## Dependencies

Brig has three direct dependencies: `sigs.k8s.io/yaml` for profiles,
`golang.org/x/sys` for terminal and process calls, and
`github.com/godbus/dbus/v5` for the Linux secret store. That list is
deliberately short. Brig shells out to `cosign`, `oras` and `security`
rather than linking them, which keeps the attack surface of a tool that
handles credentials small. Do not add a dependency without saying in the
pull request why shelling out or using the standard library will not do.

## The two promises

Most of Brig is ordinary Go. Two properties are the reason the tool
exists, and a change that weakens either is a bug even when every test
passes:

1. **The guest reaches only the host directories Brig names for it.** The
   guest home is mounted as the sandbox's home. Name a project on the run
   line and Brig mounts that project too, read-write, as a second host
   directory at `/work/<name>`. The agent can change those real project
   files. [docs/security.md](docs/security.md) names further limits,
   including what a profile's own hostmount can add. Brig writes everything
   into either directory from the host, as you, and handles those
   attacker-controlled paths through an `os.Root` rather than by joining
   strings.
2. **The guest gets only the credentials you name for it.** Brig reads
   values from your environment per invocation and forwards them by name, so
   they never appear in `ps`. It never writes them into the guest home.

[docs/security.md](docs/security.md) lists the limits of both promises,
including the ones weaker than they look. If a change moves either promise,
say so in the pull request and update that page in the same change. People
read it before trusting Brig with a credential, so keep it true.

A note on tests for these two: a **negative** test is worth more than a
positive one. "The denied variable was not forwarded" is one assertion that
catches a regression. "The planted symlink was refused, and the file outside
the guest home is untouched" is another. "The sandbox booted" is not.

## Commits

Brig follows [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>[optional scope]: <description>

[optional body]

[optional footer(s)/trailers]
```

- Limit the header to 72 characters. Write the description in the imperative
  mood ("add", not "added") and do not end it with a full stop.
- `type` is one of `feat`, `fix`, `docs`, `style`, `refactor`, `perf`,
  `test`, `build`, `ci`, `chore` or `revert`. The release changelog
  (`.goreleaser.yaml`) groups `feat`, `fix`, `refactor` and `docs` commits,
  and drops `test`, `chore`, `ci`, `build` and `style` commits entirely.
- Use a scope when it adds clarity, for example `fix(secret): ...`.
- The body says why the change exists, not what it does. Cover the problem,
  the approach you chose, and any non-obvious consequence.
- Reference an issue with a trailer: `Fixes: #<number>` when the commit
  resolves it, `Refs: #<number>` when it does not.
- Sign off every commit: `git commit -s`. This adds the `Signed-off-by`
  trailer.

## Pull requests and review

- One logical change per pull request. Put an unrelated fix in its own.
- Fill in the pull request template.
- Open the pull request as a draft, and mark it ready for review only once
  CI is green.
- Merging needs at least one approval.
- At merge, add the `Reviewed-by` trailer to the commits. A rebase-and-merge
  will not add it for you.
- Rebase-and-merge is preferred, to keep the commits as distinct units in
  `main`'s history.

## Docs

Run `script/check-retired-spellings.sh` before you open a documentation pull
request. It fails a doc that teaches a command spelling scheduled for
removal. [docs/README.md](docs/README.md) is the map of the documentation.

Brand assets (logos, marks, the architecture diagram) live under `assets/`.
See [assets/README.md](assets/README.md) for the rules.

## Releasing

Cutting a release is maintainers-only work, covered in
[docs/releasing.md](docs/releasing.md).

## Issues

Use issues to track bugs and feature requests. **A vulnerability is not a
public issue.** Brig handles credentials, so a flaw in how it does that must
reach the maintainers privately. Do not open an issue or a pull request for
one. Follow [SECURITY.md](SECURITY.md) instead, which routes it through
GitHub's private vulnerability reporting.

For a bug report, fill in the issue template. Include the problem, steps to
reproduce, the `brig version` and runtime version, your environment, and the
full `brig doctor` output.

For a feature request, read [docs/non-goals.md](docs/non-goals.md) first. It
lists what Brig will not do for now and why, so a proposal can start from
the existing answer.

## AI policy

AI-assisted development is welcome in Brig. See [AI_POLICY.md](AI_POLICY.md).
