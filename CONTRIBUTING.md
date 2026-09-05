# Contributing to brig

This repository follows the NOFire AI engineering Git guidelines,
reproduced in full below so the repo is self-contained. Repo-specific
notes come first.

## Repo specifics

- Build: `make build` produces `./brig` and `./brigd`. `make all` is
  `vet test build`, which is what CI gates on.
- Dependencies: brig has **one** direct dependency (`sigs.k8s.io/yaml`), and
  that is deliberate. It shells out to `cosign`, `oras` and `security` rather
  than linking them, which keeps the attack surface of a tool that handles
  credentials small. Please do not add a dependency without saying in the PR
  why shelling out or using the standard library will not do.
- Go version: see `go.mod`. `go build` and `go test` are more permissive than
  `go vet` about standard-library symbols newer than the `go` directive, so
  run vet before assuming a version bump is unnecessary.
- Tests: `go test ./...` runs everywhere. `go test ./... -race` for anything
  touching concurrency, subprocesses or `brigd`. Note that
  `internal/secret` exercises the **real** login keychain on macOS, so those
  tests create and delete items under the `sh.brig.secret` service.
- End-to-end: `script/smoke.sh` drives the real binary against a stub runtime
  and a stub cosign, so it needs neither a VM nor macOS and runs in CI. It
  covers profile resolution, credential forwarding, the image-verification
  decision table and the workspace lifecycle.
- What `script/smoke.sh` cannot cover: brig drives a runtime it does not own
  — `hull` on macOS, `nerdctl` on Linux. A change to how brig *invokes* either
  can pass the whole suite and still be wrong. If you touch the run, exec or
  credential path, boot a real sandbox before opening the PR.

## The two promises

Most of brig is ordinary Go, but two properties are the reason the tool
exists, and a change that weakens either is a bug even when every test passes:

1. **The guest sees one directory.** The workspace is mounted as the sandbox's
   home and nothing else on the host is reachable from inside it. Everything
   brig writes into that directory it writes from the host, as you — so those
   paths are attacker-controlled input, and they are handled through an
   `os.Root` rather than by joining strings.
2. **The guest gets the credentials it was named, and no others.** Values are
   read from your environment per invocation, forwarded by name so they never
   appear in `ps`, and never written into the workspace.

`docs/security.md` is where the limits of both are written down, including the
ones that are weaker than they look. If a change moves either promise, say so
in the PR and update that document in the same change — it is the page people
read before deciding what credential to trust brig with, and it is only useful
while it is true.

A note on tests for these two: a **negative** test is worth more than a
positive one. "The denied variable was not forwarded" and "the planted symlink
was refused, and the file outside the workspace is untouched" are the
assertions that catch a regression; "the sandbox booted" is not.

# Working with Git at NOFire AI

This document describes how we work with Git at NOFire AI: how we shape
commits, write commit messages, open Pull Requests, and review each other's
work. It draws on practices that have served us well over years of building
and maintaining systems software in the open.

A note before we start: Git workflows are an opinionated space, and reasonable
people disagree on many of the points below. What follows is not meant as
dogma, but as a shared baseline that keeps our history readable, our reviews
pleasant, and our debugging sessions short. If you believe a guideline gets in
the way of good work, raise it; these conventions should serve us, not the
other way around. That said, until we collectively decide to change something,
we ask everyone to follow the same conventions so the result stays consistent.

## Table of contents

1. [Guiding principles](#guiding-principles)
2. [Branches](#branches)
3. [Commits](#commits)
4. [Commit messages](#commit-messages)
5. [Pull Requests](#pull-requests)
6. [Review process](#review-process)
7. [CI and merge criteria](#ci-and-merge-criteria)
8. [Issues](#issues)
9. [A few words on flexibility](#a-few-words-on-flexibility)

## Guiding principles

Everything below derives from a few simple ideas:

- **The history is a first-class artifact.** We read `git log`, `git blame`
  and `git bisect` far more often than we write commits. A clean, honest
  history is documentation that never goes stale.
- **Every commit should stand on its own.** Each commit should build and pass
  tests, so that `git bisect` remains usable and reverts stay surgical.
- **Reviews are conversations, not gates.** We optimize for making the
  reviewer's life easy; in return, reviews are faster and friendlier for
  everyone.
- **Small and focused beats large and complete.** A series of small, logical
  changes is almost always easier to review, test, and revert than one big
  drop.

## Branches

- `main` is always releasable. It is protected; changes land only through
  Pull Requests.
- Work happens on short-lived feature branches. A simple, descriptive naming
  scheme helps when browsing branches, e.g.:
  - `feat/<short-description>`
  - `fix/<issue-number>-<short-description>`
  - `docs/<short-description>`
- Prefer rebasing your branch on top of `main` over merging `main` into it.
  This keeps the eventual history linear and the PR diff honest. (If you have
  a strong preference for merge-based updates during development, that is fine
  too, just make sure the branch is tidy before review.)
- Delete branches after they are merged.

## Commits

We care about commit hygiene, and this is probably the most "opinionated" part
of this document. Our experience is that the effort pays off many times over.

- **Organize changes into logical commits.** Each commit should represent a
  single, specific change, avoiding both overly large and overly small
  commits. "Implement feature X" and "fix typo from previous commit" are both
  signs that a rebase is in order.
- **No commit should break the build.** Every commit in a PR should compile
  and pass the test suite on its own, not just the final one. This keeps
  `git bisect` useful.
- **Keep refactoring separate from behavior changes.** If a fix requires
  moving code around first, put the mechanical move in its own commit so the
  actual change is visible in a small diff.
- **Don't mix unrelated changes.** If you spot an unrelated problem while
  working on something, fix it in a separate commit (or better, a separate
  PR/issue).
- **Rewrite history freely before review, carefully after.** `git rebase -i`
  is your friend while a branch is yours alone. Once review has started,
  prefer appending fixup commits during the discussion and squashing them
  before merge, so reviewers can follow what changed between rounds. The one
  sanctioned exception is adding review trailers (e.g. `Reviewed-by`) when the
  PR is approved (see [Review process](#review-process)): that final amend
  happens at merge time, not mid-review.
- **Always sign off your commits** (`git commit -s`). This adds the
  `Signed-off-by` trailer and certifies the
  [Developer Certificate of Origin](https://developercertificate.org/).

## Commit messages

This is where we deliberately hold ourselves to a high standard. A good commit
message explains *why* a change exists, to a reader who
has none of the context you currently have, including future you.

### Format

We follow the [Conventional Commits](https://www.conventionalcommits.org/)
specification:

```
<type>[optional scope]: <description>

[optional body]

[optional footer(s)/trailers]
```

- Limit the header (first line) to **72 characters**.
- Limit body and footer lines to **80 characters**.
- The `description` is written in the imperative mood ("add", not "added" or
  "adds") and does not end with a full stop.
- `type` is one of:
  - *feat*: A new feature
  - *fix*: A bug fix
  - *docs*: Documentation-only changes
  - *style*: Changes that do not affect the meaning of the code
    (white-space, formatting, missing semi-colons, etc.)
  - *refactor*: A code change that neither fixes a bug nor adds a feature
  - *perf*: A code change that improves performance
  - *test*: Adding or fixing tests
  - *build*: Changes that affect the build system or external dependencies
  - *ci*: Changes to CI configuration files and scripts
  - *chore*: Other changes that don't modify src or test files
  - *revert*: Reverts a previous commit
- Use a `scope` when it adds clarity, e.g. `fix(api): ...` or
  `feat(agent): ...`.
- If the change relates to an issue, reference it with a git trailer:
  `Fixes: #<issue-number>` (or `Refs: #<issue-number>` when it does not fully
  resolve it).
- Always include the `Signed-off-by` trailer (`git commit -s`).

### Body

The header says *what*; the body says *why*. For anything beyond trivial
changes, include a body that covers:

- What the problem or motivation was.
- Why this approach was chosen (especially if alternatives were considered).
- Any non-obvious consequences, limitations, or follow-up work.

A reasonable test: could a colleague reviewing `git log` in two years
understand the change without opening the diff or the PR discussion?

### Example

```
fix(detector): avoid false positives on slow-start deployments

The anomaly detector treated the warm-up phase of newly deployed
services as a regression, because baseline metrics were computed
over a window that included pre-deployment data.

Skip baseline computation until the service has reported healthy
for at least one full evaluation window. An alternative would have
been to weight recent samples more heavily, but that would change
behavior for all services rather than only newly deployed ones.

Fixes: #142
Signed-off-by: Jane Doe <jane@nofire.ai>
```

## Pull Requests

- **Start from an issue when the change is non-trivial.** For significant
  changes, open an issue (or pick up an existing one) and discuss the
  approach before investing in an implementation. For small fixes, a PR alone
  is fine; use judgement.
- **Keep PRs focused.** One PR per logical change. Avoid drive-by changes
  unrelated to the PR's purpose; they slow down review and complicate
  reverts.
- **Complete the PR template** (where one exists) and write a meaningful PR
  title and description: what the change does, why, how it was tested, and
  anything reviewers should pay particular attention to.
- **Test locally before opening.** The build should not break, all tests
  should pass, and new functionality should come with new or updated tests.
- **Use draft PRs** for work in progress or early feedback. Mark the PR ready
  for review only when CI is green and the commits are in their final,
  logical shape.

## Review process

The flow we follow:

1. The author opens a PR (draft, if work is in progress) and ensures CI
   passes.
2. The author marks the PR ready for review and requests reviewers (or relies
   on the default assignment).
3. Reviewers submit comments and reviews. We aim for at least one approval
   before merging; for sensitive areas, two is a good idea.
4. The author addresses comments (through discussion or follow-up commits)
   and re-requests review.
5. Upon approval, the appropriate trailers (e.g. `Reviewed-by`) are added to
   the commits (the one sanctioned post-review amend) and the PR is merged.
   Add them before merging: a rebase-and-merge will not insert them for you.

A few norms that make reviews pleasant on both sides:

- **As an author:** respond to every comment, even if just with a 👍 or a
  short rationale for disagreeing. Don't take review comments personally;
  the review targets the code, not you.
- **As a reviewer:** be kind and specific. Distinguish blocking concerns from
  preferences ("nit:" is a fine prefix for the latter). Suggest, don't
  command, where the choice is genuinely a matter of taste.
- **Disagreements** are resolved through discussion; if no consensus emerges,
  the maintainers of the affected area make the call. It is fine to record a
  "disagree and commit" in the thread and move on.

## CI and merge criteria

A PR is mergeable when:

- CI is green: linting (including commit-message linting), builds, unit tests
  and end-to-end tests pass.
- The required approvals are in place.
- The branch is up to date with `main` (rebased, with a clean, logical commit
  series).
- All commits are signed off and follow the message conventions above.

We prefer **rebase-and-merge** to keep the individual, carefully crafted
commits as distinct units in `main`'s history: their granularity and messages,
though rebasing necessarily rewrites their SHAs. Squash-merge is acceptable for
PRs that are genuinely a single logical change; plain merge commits are
generally avoided to keep the history linear. (Again: this is a preference, not
a religion; if a repository has good reasons for a different merge strategy,
document it in that repository.)

Where supported by the tooling, labels can control CI behavior (e.g.
`ok-to-test`, `skip-build`, `skip-lint`), skipping the heavy
build-and-test workflows for documentation-only PRs, or deferring lint checks
on drafts. Check each repository's docs for the labels it supports.

## Issues

We use issues to track bugs and feature requests, and to anchor PRs to a
discussion.

**A security vulnerability is not a public issue.** brig handles credentials,
so a flaw in how it does that should reach the maintainers privately before it
reaches everyone else. Do not open an issue or a pull request for one: follow
[SECURITY.md](SECURITY.md), which routes it through GitHub's private
vulnerability reporting instead. The rest of this section is for ordinary bugs.

When reporting a bug, please include:

- A short, clear description of the problem.
- Relevant logs at the highest useful verbosity.
- The version in use (release version or commit hash).
- Environment details (architecture, deployment type, relevant
  configuration).
- Steps to reproduce.
- The `bug` label, and a willingness to answer follow-up questions from the
  maintainers.

For feature requests, use the `enhancement` label and describe the proposed
feature and its motivation. Read [docs/non-goals.md](docs/non-goals.md) first:
it lists what brig will not do for now, with the reason and the change in
circumstances that would reopen each one, so a proposal can start from the
existing answer. Proposals for improvements are always welcome, including
proposals to change this document.

## A few words on flexibility

These guidelines describe how we *prefer* to work, based on what has served us
well in practice. They are not a substitute for judgement:

- Trivial changes (a typo fix, a one-line doc tweak) do not need the full
  ceremony, though they still deserve a well-formed commit message.
- New repositories or experimental projects may relax some rules early on,
  tightening them as the project matures. If a repository deviates, it should
  say so in its own `CONTRIBUTING.md`.
- If a rule consistently creates friction without adding value, that is a
  signal worth acting on. Open an issue or a PR against this document and
  let's discuss it.

The goal, in the end, is simple: a history we can trust, reviews we enjoy, and
a codebase that explains itself. Everything here exists in service of that.

## AI policy

AI-assisted development is welcome in brig. See [AI_POLICY.md](AI_POLICY.md).
