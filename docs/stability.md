# Stability

Brig has not reached 1.0. Some parts are stable enough to write a script
against. Other parts can change without notice.

## What a version number means

Brig releases are tagged `vMAJOR.MINOR.PATCH`. Before v0.2.0, every release
was a `v0.1.0` release candidate.

| Release | Example | What it can change |
| --- | --- | --- |
| Minor | v0.3.0 to v0.4.0 | It can remove a retired spelling and can make other breaking changes. |
| Patch | v0.3.0 to v0.3.1 | Fixes only. It is cut from a maintenance branch. It removes nothing and changes no stable surface. |
| Release candidate | | It is a preview of the minor release it names. |

The main and experimental Homebrew channels publish builds that are not releases.
Their versions promise nothing. See
[The prerelease channels](releasing.md#the-prerelease-channels) for how they
are made.

At 1.0, the stable surfaces stop changing without a new major version.

## Stable enough to script against

These surfaces change only through the [deprecation rule](#retired-spellings),
or as a breaking change that the release notes name.

**The command grammar.** Every verb and flag that [docs/cli.md](cli.md)
teaches. A retired spelling is covered only until the release that removes
it.

**The documented settings.** Every `BRIG_*` variable in
[docs/cli.md](cli.md#environment-variables), read in the order that page
gives. A variable that is not in that table, `BRIG_TEST_*` for example, is
Brig's own and can change or go without notice.

**The profile schema.** The fields that the `brig agent export` header
documents. Brig refuses a profile file with a field it does not know, so a
field that goes away becomes an error you see.

**The `--json` envelope.** Within one `apiVersion`, the JSON only gains
fields. It never renames or drops one, so a script written against it keeps
parsing. No field carries a credential value. `--json` works on the read
verbs. On `run` and `sh`, it reports the agent's own exit status.

**Exit codes.** `script/smoke.sh` asserts the numbers a script branches on.
See [docs/cli.md](cli.md#exit-codes) for the table. brigd reports the same
codes for the same causes.

**The brigd protocol.** Every brigd response carries the protocol version,
`v`, which is 1 today. Within one version, a field can be added, but none is
renamed or removed. A change that breaks a client is a new version. brigd
refuses a request with a version it does not know.
[brigd.md](brigd.md#protocol-version) has the details.

**Where your own files are.**

| Your files | Location |
| --- | --- |
| Profiles | `$XDG_CONFIG_HOME/brig`, default `~/.config/brig` |
| Policies | The `policies` directory inside it |
| A guest home you name with `--home` or `BRIG_WORKSPACE` | Where you put it. It is yours, and Brig never deletes it. |
| Secrets | Under the service name `sh.brig.secret`: in the login keychain on macOS, and in the default collection of your Secret Service keyring on Linux |

How one secret is laid out in that store is Brig's own. See
[Where a value lives](secrets.md#where-a-value-lives).

## Not stable

These can change without a deprecation cycle:

- The layout of `~/.brig`, Brig's state directory, and its session index.
  This includes the default guest home under `~/.brig/homes`, which Brig
  creates and deletes for you.
- Profile file fields other than those the current `brig agent export`
  header documents.
- The wording of any human-readable output. Parse `--json` output and do not
  parse prose.
- Anything marked "example profile" in `brig agent ls`.

## Retired spellings

A retired spelling keeps working. Each time you use it, Brig prints one line
on stderr that names its replacement and the release that removes it.
[migration.md](migration.md) shows the notice, and lists every retired
spelling with its replacement.

A spelling is removed only in a minor release, and at least one release after
its first notice. `brig run` is never removed.

The retired spellings that still work today are removed in v0.5.0. v0.3.0
removed only `brig shell` and the `hostCredential:` profile key, and v0.4.0
removed none.

Two exceptions:

- `brig exec` names no release. It stays until `brig sh` can pipe a command's
  output ([#335](https://github.com/brig-sh/brig/issues/335)). Its notice says
  so. When that lands, its notice will name a release at least one release
  ahead.
- The profile keys `forward:`, `statePaths:`, `shell:` and `gui:`, and the
  `BRIG_TEMPLATE_DIR` setting, print no notice at run time. They are also
  removed in v0.5.0, and [migration.md](migration.md) says so for each.

## Breaking changes

The notes of each release list its breaking changes first, under "Breaking
changes". They come from the commits that are marked as breaking under
[Conventional Commits](../CONTRIBUTING.md#commits). Read that section of the
notes before you upgrade across a minor release.

For the computers Brig runs on, see
[Platform support](install.md#platform-support).

## Report a break

If a version bump breaks a script that used only the stable surfaces, that is
a bug. Open an issue with the command, the version from `brig version`, and
the output. For a security problem, follow [SECURITY.md](../SECURITY.md) and
do not open a public issue.
