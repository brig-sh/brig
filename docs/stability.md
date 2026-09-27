# What is stable, and what is not

Brig has not reached 1.0. This page says which parts you can write a script
against, which parts can change without notice, and how Brig removes a
spelling it has retired.

## What a version number means

Brig releases are tagged `vMAJOR.MINOR.PATCH`. Before v0.2.0, every release
was a `v0.1.0` release candidate.

- A minor release, for example v0.3.0 to v0.4.0, can remove a retired
  spelling and can make other breaking changes.
- A patch release, for example v0.3.0 to v0.3.1, is cut from a maintenance
  branch and carries fixes only. It removes nothing and changes no stable
  surface.
- A release candidate, for example `v0.4.0-rc1`, is a preview of the minor
  release it names.
- The main and experimental Homebrew channels publish builds, not releases.
  Their versions promise nothing.
  [releasing.md](releasing.md#the-prerelease-channels) says how they are
  made.

At 1.0, the stable surfaces below stop changing without a new major version.

## Stable enough to script against

These surfaces change only through the [deprecation rule](#retired-spellings)
below, or as a breaking change that the release notes name.

**The command grammar.** Every verb and flag that [docs/cli.md](cli.md)
teaches. A retired spelling is covered only until the release that removes
it.

**The documented settings.** Every `BRIG_*` variable in
[docs/cli.md](cli.md#environment-variables), read in the order that page
gives. A variable that is not in that table, `BRIG_TEST_*` for example, is
Brig's own and can change or go without notice.

**The profile schema.** The fields that the `brig agent export` header
documents. Brig refuses a profile file with a field it does not know, so a
field that goes away is an error you see, not a setting Brig ignores.

**The `--json` envelope.** Within one `apiVersion`, the JSON only gains
fields. It never renames or drops one, and no field carries a credential value.
A script written against it keeps parsing, and a machine-readable dump stays a
place a secret cannot leak. `--json` works on the read verbs, and on `run` and
`sh` it reports the agent's own exit status.

**Exit codes.** The numbers a script branches on are pinned end to end by
`script/smoke.sh`. See [docs/cli.md](cli.md#exit-codes) for the table. brigd
reports the same codes for the same causes.

**The brigd protocol.** Every brigd response carries the protocol version,
`v`, which is 1 today. Within one version, a field can be added, but none is
renamed or removed. A change that breaks a client is a new version. brigd
refuses a request with a version it does not know.
[brigd.md](brigd.md#protocol-version) has the details.

**Where your own files are.** Your profiles are in `$XDG_CONFIG_HOME/brig`,
default `~/.config/brig`, and your policies are in its `policies`
directory. A guest home you name with `--home` or `BRIG_WORKSPACE` is yours,
and Brig never deletes it. Brig keeps secrets under the service name
`sh.brig.secret`: in the login keychain on macOS, and in the default
collection of your Secret Service keyring on Linux. How one secret is laid
out in that store is Brig's own, and
[secrets.md](secrets.md#where-a-value-lives) describes it.

## Not stable

Treat the following as subject to change without a deprecation cycle:

- The layout of `~/.brig`, Brig's state directory, and its session index.
  This includes the default guest home under `~/.brig/homes`, which Brig
  creates and deletes for you.
- Profile file fields other than those the current `brig agent export`
  header documents.
- The wording of any human-readable output. Parse `--json`, not prose.
- Anything marked "example profile" in `brig agent ls`.

## Retired spellings

A retired spelling keeps working. Each time you use it, Brig prints one line
on stderr that names its replacement and the release that removes it.
[migration.md](migration.md) shows the notice.

A spelling is removed only in a minor release, and at least one release after
its first notice. `brig run` is never removed.

The retired spellings that still work today are removed in v0.4.0. The first
date was v0.3.0, which removed only `brig shell`. Two kinds of exception:

- `brig exec` names no release. It stays until `brig sh` can pipe a command's
  output ([#335](https://github.com/brig-sh/brig/issues/335)). Its notice says
  so. When that lands, its notice will name a release at least one release
  ahead.
- The profile keys `forward:`, `statePaths:`, `shell:` and `gui:`, and the
  `BRIG_TEMPLATE_DIR` setting, print no notice at run time. They are also
  removed in v0.4.0, and [migration.md](migration.md) says so for each.

[docs/migration.md](migration.md) has the full list of retired spellings and
what replaces each one.

## Where a breaking change is written down

Each release's notes list its breaking changes first, under "Breaking
changes". They come from the commits that are marked as breaking under
[Conventional Commits](../CONTRIBUTING.md#commits). Read that section
before you upgrade across a minor release.

Which computers Brig runs on is in
[docs/install.md](install.md#platform-support).

## Reporting a break

If a version bump breaks a script that used only the surfaces above, that is a
bug worth filing. Open an issue with the command, the version from
`brig version`, and the output. For anything security-related, follow
[SECURITY.md](../SECURITY.md) instead of opening a public issue.
