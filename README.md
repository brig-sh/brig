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

You name one project, and the agent works there. It gets a kernel and a home
directory of its own. It does not see the rest of your disk, your keychain or
your SSH agent. When you are done, delete the sandbox.

<p align="center">
  <img alt="Terminal recording. brig run opens Claude Code in a sandbox, in /work/demo. Asked to write hello.py and run it, Claude writes the file, runs it and lists the directory. After exit, ls on the host shows hello.py in the project." src="assets/brig-run-claude.gif" width="900">
</p>

## Install

On macOS, with [Homebrew](https://brew.sh):

```bash
brew tap brig-sh/brig
brew trust brig-sh/brig
brew install --cask brig
```

For Linux, `install.sh` or a source build, see
[Install](https://brig.sh/docs/install/).

| Host | Supported |
| --- | --- |
| Mac, Apple silicon, macOS 15 or newer | Yes |
| Mac, Apple silicon, macOS 14 | Yes, with `BRIG_HYPERVISOR=vz BRIG_NETWORK=shared` |
| Intel Mac | No |
| Linux, x86-64 or arm64 | Yes, with the runtime bundle `install.sh` installs |

## First run

```bash
brig doctor                        # check the host
mkdir -p ~/code/demo
brig run claude ~/code/demo        # start a sandbox and run Claude Code in it
brig stop claude                   # stop the sandbox, keep the session
brig rm claude                     # stop it, remove it, delete its guest home
```

Claude Code asks you to log in inside the sandbox, so you need an Anthropic
account. To try Brig without one, run `brig run ubuntu ~/code/demo`, which
opens a root shell. No command here touches `~/code/demo`.

[Quickstart](https://brig.sh/docs/quickstart/) walks through the same steps with the output
of each.

## What you control

- **The files.** The project you name is mounted read-write at `/work/<name>`,
  and those are your real files. The agent can change or delete anything
  under it.
- **The credentials.** Brig forwards only what the agent's profile names.
  With nothing exported or stored, the agent starts logged out.
  `brig info <ref>` lists what a run forwards, by name.
- **The network.** A new sandbox on `hvi` or Linux gets an `isolated` network,
  which keeps it off other sandboxes' networks. It still reaches the internet,
  so anything the agent can read it can also send. To restrict that, bind an
  egress policy. Brig enforces one on hull's `hvi` backend and on Linux with
  nerdctl, and refuses a policy-bound run on any other backend.
- **The images.** Brig checks the signature of the guest images and boot
  assets that brig-sh publishes. Verification defaults to `warn`, which
  reports an image it cannot verify and boots it anyway. Set
  `BRIG_VERIFY=require` to refuse one.

[docs/security.md](docs/security.md) states each limit in full.

## Documentation

The documentation is at **[brig.sh/docs](https://brig.sh/docs/)**. The pages in
[docs/](docs/README.md) are the record that site is written from.

| If you want to | On the site | In this repository |
| --- | --- | --- |
| Install Brig on any supported host | [brig.sh/docs/install](https://brig.sh/docs/install/) | [docs/install.md](docs/install.md) |
| Get a first agent running, step by step | [brig.sh/docs/quickstart](https://brig.sh/docs/quickstart/) | [docs/quickstart.md](docs/quickstart.md) |
| Understand homes, projects and sessions | [brig.sh/docs/sessions](https://brig.sh/docs/sessions/) | [docs/sessions.md](docs/sessions.md) |
| Log an agent in, or give it Git access | [brig.sh/docs/authentication](https://brig.sh/docs/authentication/) | [docs/authentication.md](docs/authentication.md) |
| Store and deliver a secret | [brig.sh/docs/secrets](https://brig.sh/docs/secrets/) | [docs/secrets.md](docs/secrets.md) |
| Look up a command, a flag or a variable | [brig.sh/docs/cli](https://brig.sh/docs/cli/) | [docs/cli.md](docs/cli.md) |
| Run your own agent | [brig.sh/docs/profiles](https://brig.sh/docs/profiles/) | [docs/profiles.md](docs/profiles.md) |
| Run your own image | [brig.sh/docs/guest-image](https://brig.sh/docs/guest-image/) | [docs/guest-image.md](docs/guest-image.md) |
| Restrict what the guest can reach | [brig.sh/docs/policies](https://brig.sh/docs/policies/) | [docs/policies.md](docs/policies.md) |
| Understand the isolation and its limits | [brig.sh/docs/security](https://brig.sh/docs/security/) | [docs/security.md](docs/security.md) |
| Know what Brig counts, and turn it off | [brig.sh/docs/telemetry](https://brig.sh/docs/telemetry/) | [docs/telemetry.md](docs/telemetry.md) |
| Fix something that went wrong | [brig.sh/docs/troubleshooting](https://brig.sh/docs/troubleshooting/) | [docs/troubleshooting.md](docs/troubleshooting.md) |
| Move off a retired command spelling | [brig.sh/docs/migration](https://brig.sh/docs/migration/) | [docs/migration.md](docs/migration.md) |
| Know what is stable | [brig.sh/docs/stability](https://brig.sh/docs/stability/) | [docs/stability.md](docs/stability.md) |

## Project status

Brig has not reached 1.0, and a minor release can make breaking changes.
[docs/stability.md](docs/stability.md) lists what you can script against
today, and [docs/non-goals.md](docs/non-goals.md) lists what Brig will not do.

- Questions and bugs: [docs/support.md](docs/support.md).
- Vulnerabilities: [SECURITY.md](SECURITY.md), never a public issue.
- Contributing: [CONTRIBUTING.md](CONTRIBUTING.md) and
  [AI_POLICY.md](AI_POLICY.md).
- Telemetry: `brig telemetry status` shows the setting, and
  `brig telemetry off` turns it off.
- License: Apache License 2.0. See [LICENSE](LICENSE).

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/nofire-logo-on-dark.svg">
    <img alt="NOFire AI" src="assets/nofire-logo.svg" width="120">
  </picture>
</p>
