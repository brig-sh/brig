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

`brig doctor` prints one line per check: brig (the build), host, virtual,
runtime, boot, verify, profiles, secrets, brigd and image.
[../CONTRIBUTING.md#issues](../CONTRIBUTING.md#issues) lists what else helps:
the runtime version, your environment, and the steps to reproduce.

If Brig or its runtime will not install or start, first check that your
platform is supported: [install.md](install.md#platform-support) has the
matrix.

## Report a vulnerability

Do not open a public issue for a security problem. Follow
[../SECURITY.md](../SECURITY.md): it sends the report through GitHub's
private vulnerability reporting, so nothing is disclosed before a fix
exists.
