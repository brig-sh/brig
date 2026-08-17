---
name: Bug report
about: Report a defect or incorrect behavior
title: ''
labels: bug
assignees: ''
---

<!--
Write a specific, imperative title, e.g. "brig env omits a forwarded variable
when the profile is file-backed". Search open and closed issues first to avoid
duplicates.
-->

## Describe the bug

<!-- A clear description of what is wrong. -->

## To reproduce

<!--
The exact steps, including the command line. `brig env <agent>` prints what
would be forwarded, by name and never by value, and is usually the quickest way
to show a credential problem without pasting a secret.

If the sandbox boots but misbehaves, say which runtime is underneath: `brig
status` reports the backend, and `hull ps` / `nerdctl ps` shows the instance.
-->

1.
2.

## Expected behavior

<!-- What you expected to happen. -->

## Actual behavior

<!-- What happened instead. Paste the error verbatim if there is one. -->

## Environment

<!-- Fill in what applies; delete the rest. -->

- Host OS: <!-- e.g. macOS 26.3, or Ubuntu 24.04 -->
- Architecture: <!-- arm64 | amd64 -->
- brig version: <!-- brig --version -->
- Runtime and version: <!-- hull --version, or nerdctl --version -->
- Profile: <!-- claude-code, codex, ... or a file-backed one -->
- Installed how: <!-- Homebrew cask | install.sh | make build -->
- Commit: <!-- git rev-parse HEAD, if built from source -->

## Logs and additional context

<!--
Relevant output, fenced. Link related issues inline with #NN.

Please do not paste credential values. brig is careful never to print one --
`brig env` shows names only -- so if you find a value in output anywhere, that
is itself the bug, and worth saying so rather than pasting it.
-->
