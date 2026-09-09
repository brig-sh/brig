# Support

Where to ask a question, file a bug, or report a vulnerability.

## Ask a question

Open an issue: [github.com/brig-sh/brig/issues](https://github.com/brig-sh/brig/issues).
There is no other channel yet.

## File a bug

Open an issue and pick the bug report template. Include:

- The command you ran.
- The output of `brig version`.
- The output of `brig doctor`.

`brig doctor` reports the host, the hypervisor, the runtime, the boot assets,
cosign, the profiles, the secret store and brigd, one line per fact.
[../CONTRIBUTING.md#issues](../CONTRIBUTING.md#issues) lists what else helps:
logs, your environment, and the steps to reproduce.

If brig or its runtime will not install or start, check whether your platform
is supported first: [install.md](install.md#platform-support) has the full
matrix.

## Report a vulnerability

Do not open a public issue for a security problem. Follow
[../SECURITY.md](../SECURITY.md) instead: it routes the report through
GitHub's private vulnerability reporting, so nothing is disclosed while a fix
is worked out.
