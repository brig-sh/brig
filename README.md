<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/brig-lockup-on-dark.svg">
    <img alt="brig" src="assets/brig-lockup-on-light.svg" width="240">
  </picture>
</p>

<p align="center">
  <a href="https://github.com/brig-sh/brig/releases/latest"><img alt="Release" src="https://img.shields.io/github/v/release/brig-sh/brig?include_prereleases"></a>
  <a href="https://github.com/brig-sh/brig/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/brig-sh/brig/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://codecov.io/gh/brig-sh/brig"><img alt="Coverage" src="https://codecov.io/gh/brig-sh/brig/graph/badge.svg"></a>
  <a href="go.mod"><img alt="Go" src="https://img.shields.io/github/go-mod/go-version/brig-sh/brig"></a>
  <a href="LICENSE"><img alt="License: Apache 2.0" src="https://img.shields.io/badge/license-Apache--2.0-blue.svg"></a>
</p>

**Brig runs a coding agent inside a microVM on your own machine.**

An agent working unattended can only damage what you handed it. Point it at one
project, and a bad edit or a bad command reaches no further than that project.
When you are done, throw the sandbox away and start clean.

## How it works

One command starts a sandbox and runs the agent in it:

```bash
brig run claude ~/code/demo
```

A session is `<agent>` or `<agent>@<label>`, the ref every command takes.
`claude` and `claude@refactor` are two independent sessions of the same
agent, each with its own sandbox. The guest home is the host directory
holding a session's settings and history. `claude` resolves to the
`claude-code` agent, so its guest home is `~/brig/claude-code`, and
`claude@refactor`'s is the sibling `~/brig/claude-code-refactor`, not a
directory inside it.

Name a project on the run line, and Brig mounts it read-write at
`/work/<name>`, where the agent starts. Credentials reach the guest only
when you deliver them: the sandbox boots with none, and the agent asks you
to log in. What the guest does not get: every other host directory, your
keychain, and your SSH agent. [docs/sessions.md](docs/sessions.md) is the
full model.

## Requirements

| Host | Supported |
| --- | --- |
| Mac, Apple silicon, macOS 15 or newer | Yes |
| Mac, Apple silicon, macOS 14 | Yes, with `BRIG_HYPERVISOR=vz` |
| Intel Mac | No |
| Linux, x86-64 or arm64 | Yes, with `nerdctl`, containerd and the `urunc` shim |

macOS 15 is the floor: six of the eight built-in profiles need the `hvi`
backend. See [docs/install.md#platform-support](docs/install.md#platform-support)
for the rest.

## Install

```bash
brew tap brig-sh/brig
brew trust brig-sh/brig
brew install --cask brig
```

The cask brings [hull](https://github.com/brig-sh/hull) and `cosign` with
it. For `install.sh`, Linux, or a source build, see [docs/install.md](docs/install.md).

## First run

```bash
brig doctor
```

Each line is a check. A `!!` line prints its fix beside it.

```bash
mkdir -p ~/code/demo
brig run claude ~/code/demo
```

Brig prints `brig: image and boot assets verified`, Claude Code asks you to log
in inside the sandbox, and `pwd` inside the agent prints `/work/demo`. The
first run pulls the guest image and the boot assets, so it is slow.
[docs/authentication.md](docs/authentication.md) covers the login and how
to carry one in from the host.

```bash
brig stop claude    # stop the sandbox, keep its name
brig rm claude      # stop it and remove it
```

Neither touches `~/brig/claude-code` or `~/code/demo`.
[docs/quickstart.md](docs/quickstart.md) walks through all of this, explained.

## The boundary

<p align="center">
  <img alt="brig sandbox architecture. brig run resolves the session on the host. hull drives a microVM on macOS, urunc over KVM on Linux. Both give the guest the same contract: a guest home, a project mount, per-exec credentials, and an image whose signature brig checks" src="assets/architecture.svg" width="900">
</p>

The project mount is read-write, and those are your real files: the agent
can change anything under it. On the default `shared` network the agent
reaches the internet, so anything it can read it can also send. Brig
enforces egress policy only on hull's `hvi` backend, and refuses a
policy-bound run on any other backend rather than run it unenforced. Image
verification defaults to `warn`, which reports an unverifiable image and
boots it anyway. Set `BRIG_VERIFY=require` to refuse one instead.
[docs/security.md](docs/security.md) has the full picture.

## Documentation

| If you want to | Read |
| --- | --- |
| Install Brig on any supported host | [docs/install.md](docs/install.md) |
| Get a first agent running, step by step | [docs/quickstart.md](docs/quickstart.md) |
| Understand homes, projects and sessions | [docs/sessions.md](docs/sessions.md) |
| Log an agent in, or give it Git access | [docs/authentication.md](docs/authentication.md), [docs/secrets.md](docs/secrets.md) |
| Look up a command, a flag or a variable | [docs/cli.md](docs/cli.md) |
| Run your own agent or your own image | [docs/profiles.md](docs/profiles.md), [docs/guest-image.md](docs/guest-image.md) |
| Restrict what the guest can reach | [docs/policies.md](docs/policies.md) |
| Understand the isolation, and its limits | [docs/security.md](docs/security.md) |
| Know what Brig counts, and turn it off | [docs/telemetry.md](docs/telemetry.md) |
| Fix something that went wrong | [docs/troubleshooting.md](docs/troubleshooting.md) |
| Move off a retired command spelling | [docs/migration.md](docs/migration.md) |
| Know what is stable and what is not | [docs/stability.md](docs/stability.md) |

The full index is [docs/README.md](docs/README.md).

## Project status

Brig is a prerelease, in the `0.1.0-rc` series. `brig version` prints yours,
and [docs/stability.md](docs/stability.md) says what you can script against
today. [docs/support.md](docs/support.md) says where to ask a question or
file a bug, and [SECURITY.md](SECURITY.md) is where to report a
vulnerability instead of a public issue. [CONTRIBUTING.md](CONTRIBUTING.md)
covers the build, the tests and the review norms, and
[AI_POLICY.md](AI_POLICY.md) says how AI-assisted contributions are
handled. Brig ships under the Apache License 2.0. See [LICENSE](LICENSE).

Brig itself counts nothing. On macOS the hull runtime it drives counts a
few events, and on Linux nothing is sent. `brig telemetry status` shows the
current setting, and `brig telemetry off` turns it off.
[docs/telemetry.md](docs/telemetry.md) has the field-by-field detail.

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/nofire-logo-on-dark.svg">
    <img alt="NOFire AI" src="assets/nofire-logo.svg" width="120">
  </picture>
</p>
