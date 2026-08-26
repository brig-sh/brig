# What a version number promises

This page says which parts of brig you can build on and which you cannot, how
long a name survives after it is deprecated, and where a break is written down.
It is meant to stay short enough to stay true, so it names surfaces rather than
restating what each one does; the README and the other docs are where the
detail lives.

## What 0.x means

brig has no stable release yet. There are twelve `v0.1.0-rc` tags and nothing
without the `-rc`, so every version to date is a release candidate. Until a
`1.0` ships, treat the whole tool as prerelease: a covered surface can still
change between releases, but it changes under the deprecation rule below and
the change is recorded where [breaking changes](#where-breaking-changes-are-recorded)
go. `1.0` is the point at which the covered surfaces stop moving without a
major version bump. The unstable surfaces below carry no such promise even
then, and they say so on purpose.

## What the promise covers

These are the surfaces a script or a profile can depend on. Within `0.x` they
change only through a deprecation, and across a `1.0` they will not change
without a major bump.

- **The CLI verbs.** `run`, `create`, `exec`, `shell`, `stop`, `rm`, `ls`,
  `reset`, `env`, `version`, `help`, the profile verbs (`profiles`, and
  `profile ls|export|import|edit|rm`, plus the top-level `export` and `import`
  aliases), and the secret verbs (`secret create|read|update|delete|ls|import`).
  The list is `cmd/brig/main.go`.
- **The run-line flags.** `-n`/`--name`, `-t`/`--image`, `-w`/`--workspace`,
  `-m`/`--memory`, `--cpus`, `-d`/`--detach`, and `--skills`. Everything else
  on a run line belongs to the agent, by design, so the set brig owns is the
  set here and no more.
- **The settings documented in the README.** Every `BRIG_*` variable in the
  README's [environment variables](../README.md#environment-variables) tables,
  read under the `BRIG_<AGENT>_<KEY>` then `BRIG_<KEY>` order stated there. A
  variable that is not in those tables is not part of this promise; see below.
- **The profile schema.** The fields `brig profile export` writes and
  documents in its header: `name`, `desc`, `kind`, `image`, `guestHome`,
  `binary`, `secrets`, `env`, `files`, `volumes`, `deny`, `mem`, `cpus`,
  `hypervisor`, `runtimeBin`, `rootfsType`, `genericBoot` and `reserved`. The
  types are `internal/profile`. The parser refuses an unknown field rather than
  ignoring it, so adding one is a change you will see, not a silent no-op.
- **The exit status, coarsely.** `0` is success and non-zero is failure. On
  `run`, `exec` and `shell` brig replaces itself with the runtime, so the
  status you get back is the agent's own. The split between success and failure
  is covered; a specific non-zero number is not a contract yet.
- **The filesystem layout.** Workspaces under `~/brig/<agent>` (a named session
  appends `-<slug>`); brig's own runtime sockets under `~/.brig`; and custom
  profiles under the config directory, `$XDG_CONFIG_HOME/brig` or
  `~/.config/brig` when that is unset. `brig secret` stores each item in the
  macOS login keychain under the service `sh.brig.secret`.

## What is explicitly not covered

Depending on any of these is depending on something we intend to move.

- **The `brigd` line protocol.** The JSON operations in
  [docs/brigd.md](brigd.md) are unstable on purpose. `brigd` is a small,
  optional daemon that most people never run, and its protocol is free to
  change between releases without a deprecation. Drive brig through the CLI if
  you want the promise above.
- **The JSON output.** There is little of it today, `brig profile export
  --json` being the main case, and its shape is not fixed. Do not parse it in
  anything you need to keep working across releases.
- **The human-readable output.** The wording of messages on stdout and stderr,
  the warnings, and the column layout of `ls` and `profiles` are for people to
  read, not for `grep` to match. They get reworded whenever a clearer sentence
  turns up.
- **Undocumented and test-only settings.** Any `BRIG_*` variable not in the
  README tables, including the `BRIG_TEST_*` ones, exists for brig's own use
  and can change or vanish without notice.
- **Deprecated spellings.** Anything named in the next section is already on its
  way out and covered only for its remaining window.

## How long a deprecated spelling lives

When a replacement ships, the old spelling keeps working for at least one
further release, and the removal release is named in the deprecation notice or
here. That named release is the authority, so the deadline is a version number
you can read rather than a vague "soon".

The spellings deprecated before this policy are scheduled unevenly, and this is
what each actually does today:

- `BRIG_CREDENTIALS_CMD` is gone. It was deprecated in `v0.1.0-rc16` and
  removed; a run that still sets it fails and names the replacement, rather
  than warning and carrying on.
- The `hostCredential:` profile key prints a runtime notice naming
  `v0.1.0-rc17` as the release that removes it. It was deprecated in
  `v0.1.0-rc16` and has left the shipped profiles.
- `brig agents` and `brig template` print a one-line notice pointing at the new
  spelling (`brig profiles`, `brig profile`), with no removal release named yet.
  They keep working until one is.
- The profile keys `forward:` and `statePaths:` are noted only in the exported
  profile header, not at runtime, and carry no removal release yet.
- `BRIG_TEMPLATE_DIR` is not deprecated: it is an accepted alias for
  `BRIG_PROFILE_DIR` and keeps working.

## Where breaking changes are recorded

A change that breaks a covered surface carries a `BREAKING CHANGE:` footer on
its commit, per the Conventional Commits rule in
[CONTRIBUTING.md](../CONTRIBUTING.md#commit-messages). Those footers are what
the generated changelog collects, so the changelog for a release is the list of
what you have to change on the way to it.

## The support matrix

Which operating system versions and architectures brig is supported on is part
of this surface too, and "recommended and tested" is a statement about what we
run, not a statement of support. This page does not carry the matrix; it lives
with the platform notes in the README's [macOS](../README.md#macos) and
[Linux](../README.md#linux) sections, which another change owns.
