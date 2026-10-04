# Brig documentation

Start with [the README](../README.md) for what Brig is and a first run. The
other pages follow, in the order a new reader needs them.

## Install

| Page | What it gives you |
| --- | --- |
| [install.md](install.md) | How to install Brig on each platform, and how to verify a download |

## First run

| Page | What it gives you |
| --- | --- |
| [quickstart.md](quickstart.md) | One agent on a throwaway project, from start to cleanup |

## Understand

| Page | What it gives you |
| --- | --- |
| [sessions.md](sessions.md) | Guest homes, projects, named sessions, and what survives which command |
| [security.md](security.md) | What the sandbox isolates, and what an agent can still do |
| [claims.md](claims.md) | The tests behind each claim on the security page |
| [non-goals.md](non-goals.md) | What Brig does not try to be |
| [runtimes.md](runtimes.md) | The macOS backends, the Linux runtimes, and how Brig chooses one |
| [brigd.md](brigd.md) | What the daemon is for |

## Everyday tasks

| Page | What it gives you |
| --- | --- |
| [authentication.md](authentication.md) | How to log an agent in, and Git access inside the guest |
| [secrets.md](secrets.md) | The secret store, profile secret fields, and `brig secret import` |
| [policies.md](policies.md) | Networking modes and egress policy |
| [profiles.md](profiles.md) | The profile file format, and how to run your own agent |
| [guest-image.md](guest-image.md) | The contract an image must meet to boot as a guest |
| [completions.md](completions.md) | Shell completion for bash, zsh and fish |

## Reference

| Page | What it gives you |
| --- | --- |
| [cli.md](cli.md) | Every verb and flag, the environment variables, the JSON output and the exit codes |
| [migration.md](migration.md) | Every retired spelling and its replacement |
| [stability.md](stability.md) | What you can script against, and what can change |
| [telemetry.md](telemetry.md) | What Brig counts, and how to turn it off |

## Fix

| Page | What it gives you |
| --- | --- |
| [troubleshooting.md](troubleshooting.md) | Fixes organized by the error you saw, each with the command that confirms it |
| [support.md](support.md) | Where to ask a question, file a bug or report a vulnerability |

## Contribute

| Page | What it gives you |
| --- | --- |
| [../CONTRIBUTING.md](../CONTRIBUTING.md) | The build, the tests, the review norms |
| [../AI_POLICY.md](../AI_POLICY.md) | How AI-assisted contributions are handled |
| [../AGENTS.md](../AGENTS.md) | The contribution rules, gathered for a coding agent that works in the repository |
| [../SECURITY.md](../SECURITY.md) | How to report a vulnerability |
| [releasing.md](releasing.md) | How to cut a release, for maintainers only |
| [../assets/README.md](../assets/README.md) | The logo and diagram assets, and how to use them |

[manual-tests/](manual-tests/) holds dated engineering records from specific
changes. Each one records what was measured on that date. None is
revalidated against the current release.
