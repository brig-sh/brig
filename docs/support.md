# Support

Where to ask a question, file a bug, or report a vulnerability.

## Ask a question

Open an issue: [github.com/brig-sh/brig/issues](https://github.com/brig-sh/brig/issues).
There is no other channel yet.

## File a bug

Open an issue and pick the bug report template. Attach the zip from
`brig doctor bundle`: the doctor report, host and runtime versions, your
sandboxes and their last boot, and your own profiles and policies, with
identifying values replaced by placeholders. It replaces pasting
`brig version` and `brig doctor` output, and most of the template's
Environment section.

To not attach a file, paste the two commands' output instead:

- The output of `brig version`.
- The output of `brig doctor`.

`brig doctor` prints one line per check: host, virtual, runtime, boot,
verify, profiles, secrets, brigd and image.
[../CONTRIBUTING.md#issues](../CONTRIBUTING.md#issues) lists what else helps:
logs, your environment, and the steps to reproduce.

With `brig doctor bundle --include-logs`, read `logs/` in the zip before
attaching it: a running sandbox's log can hold agent output the bundle
cannot redact.

If Brig or its runtime will not install or start, check whether your platform
is supported first: [install.md](install.md#platform-support) has the full
matrix.

## Report a vulnerability

Do not open a public issue for a security problem. Follow
[../SECURITY.md](../SECURITY.md) instead: it routes the report through
GitHub's private vulnerability reporting, so nothing is disclosed while a fix
is worked out.
