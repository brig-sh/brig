# What is stable, and what is not

Brig is a prerelease. Every published version so far is a `0.1.0-rc` tag, and
the grammar is still settling. This page says which parts you can write a
script against today and which parts are expected to move.

## Not stable yet

Nothing in Brig carries a compatibility guarantee before 1.0. The command
grammar was renamed once already, during the 0.1 series, and
[docs/migration.md](migration.md) is the record of that change.

Treat the following as subject to change without a deprecation cycle:

- Profile file fields other than those the current `brig agent export` header
  documents.
- The layout of `~/.brig`, Brig's state directory, and its session index.
- The wording of any human-readable output. Parse `--json`, not prose.
- Anything marked "example profile" in `brig agent ls`.

## Stable enough to script against

Three surfaces are built to be depended on, and each has tests that pin it.

**The `--json` envelope.** Within one `apiVersion`, the JSON only gains
fields. It never renames or drops one, and no field carries a credential value.
A script written against it keeps parsing, and a machine-readable dump stays a
place a secret cannot leak. `--json` works on the read verbs, and on `run` and
`sh` it reports the agent's own exit status.

**Exit codes.** The numbers a script branches on are pinned end to end by
`script/smoke.sh`. See [docs/cli.md](cli.md#exit-codes) for the table.

**Retired spellings.** Every retired command, subverb and flag on this page
still works and prints one line naming its replacement. They are scheduled for
removal in 0.3. `brig run` is never removed.

## The deprecation window

A retired spelling keeps working for at least one release after Brig prints
its notice, and the current window closes at 0.3.
[docs/migration.md](migration.md) has the notice example and the full mapping
of retired spellings.

## Reporting a break

If a version bump breaks a script that used only the surfaces above, that is a
bug worth filing. Open an issue with the command, the version from
`brig version`, and the output. For anything security-related, follow
[SECURITY.md](../SECURITY.md) instead of opening a public issue.
