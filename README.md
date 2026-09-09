<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/brig-lockup-on-dark.svg">
    <img alt="brig" src="assets/brig-lockup-on-light.svg" width="300">
  </picture>
</p>

<p align="center">
  <a href="https://github.com/brig-sh/brig/releases/latest"><img alt="Release" src="https://img.shields.io/github/v/release/brig-sh/brig?include_prereleases"></a>
  <a href="https://github.com/brig-sh/brig/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/brig-sh/brig/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://codecov.io/gh/brig-sh/brig"><img alt="Coverage" src="https://codecov.io/gh/brig-sh/brig/graph/badge.svg"></a>
  <a href="go.mod"><img alt="Go" src="https://img.shields.io/github/go-mod/go-version/brig-sh/brig"></a>
  <a href="LICENSE"><img alt="License: Apache 2.0" src="https://img.shields.io/badge/license-Apache--2.0-blue.svg"></a>
</p>

**brig runs a coding agent inside a microVM on your own machine.** The agent
gets a guest home of its own, plus the one project directory you name on the
command line. Your other files stay where they are: brig mounts nothing else,
and it reads no credential out of your keychain, your SSH agent or your secret
manager to hand to the agent.

That matters when you want to let an agent work unattended. The blast radius of
a bad edit or a bad command is the project you handed it, and you can throw the
sandbox away and start again.

## How it works

One command resolves a session on the host and boots it:

```bash
brig run claude ~/code/demo
```

- **The session** is `claude`, the default session of the `claude-code` agent.
  `claude@refactor` is a second, independent session of the same agent, with
  its own sandbox and its own guest home. Every verb takes this ref.
- **The guest home** is a host directory, `~/brig/claude-code`, mounted as the
  agent's home. The agent's settings and history live there and survive
  restarts.
- **The project** is the directory you named. It is mounted read-write at
  `/work/demo`, and the agent starts there. Name no project and brig remounts
  whatever that session ran with last. `--no-project` mounts none.
- **The sandbox** is a microVM: `hull` on macOS, `nerdctl` with containerd and
  the `urunc` shim on Linux.
- **Credentials** are not carried in by default. The sandbox boots with none,
  so the agent asks you to log in exactly as it does on a new machine.

## Requirements

| Host | Supported |
| --- | --- |
| Mac, Apple silicon, macOS 15 or newer | Yes |
| Mac, Apple silicon, macOS 14 | Yes, with `BRIG_HYPERVISOR=vz` |
| Intel Mac | No, brig needs Apple silicon |
| Linux, x86-64 or arm64 | Yes, with nerdctl and containerd |

Six of the eight built-in profiles ask for hull's `hvi` backend, which needs
macOS 15. On macOS 14 brig refuses the run and names `BRIG_HYPERVISOR=vz` as
the way past it. macOS 26 is what the project tests on.

`cosign` is optional. Without it brig cannot check a guest image signature and
says so on every boot. Full instructions, including Linux and building from
source, are in [docs/install.md](docs/install.md).

## Quickstart

Install on macOS with Homebrew:

```bash
brew tap brig-sh/brig
brew trust brig-sh/brig       # brew refuses untrusted third-party taps
brew install --cask brig
```

The cask brings [hull](https://github.com/brig-sh/hull) with it. During the
`0.1.0-rc` series the casks are maintained by hand, so if the tap lags the
newest release, use [install.sh](docs/install.md#installsh) instead.

Check what brig found on your host:

```bash
brig doctor
```

Each line names one fact: the host, the hypervisor, the runtime, the boot
assets, cosign, the profiles, the secret store and brigd. A `!!` line prints
the fix beside it. `--` means brig looked and found nothing to report, which is
not a failure.

Run an agent on a throwaway project:

```bash
mkdir -p ~/code/demo && cd ~/code/demo
brig run claude ~/code/demo
```

The first run pulls the guest image once, so it is the slow one. Claude Code
then prompts you to log in, because the sandbox holds no credential. That login
happens inside the guest. On `claude-code` it lands on a tmpfs, so `brig stop`
takes it with the VM and the next run asks again. To carry in the login you
already have on this Mac, see [docs/authentication.md](docs/authentication.md).

You know it worked when the agent's prompt appears and `pwd` inside it prints
`/work/demo`. Leave the agent and the sandbox keeps running, so the next
`brig run claude` is immediate.

Stop it when you are done:

```bash
brig stop claude    # stop the sandbox and keep its name
brig rm claude      # stop it and remove it
```

Neither touches `~/brig/claude-code` or `~/code/demo`. Your project and the
agent's saved state stay on the host.

## Common workflows

```bash
brig ls                          # every sandbox, with its ref and workspace
brig run claude                  # reopen the session on the project it last used
brig run claude@refactor ~/code/demo   # a second session, its own sandbox and home
brig sh claude                   # a login shell inside the sandbox
brig sh claude ls /work          # one command inside it
brig info claude                 # the boundary a run uses, before booting
brig logs claude --follow        # stream the sandbox's log
```

`brig info` is the one to reach for when a run does not do what you expected.
It prints the session, the image, the workspace, the verification mode, the
network and the credentials by name, and it boots nothing.

## The boundary

<p align="center">
  <img alt="brig sandbox architecture. brig run resolves the session on the host. hull drives a microVM on macOS, urunc over KVM on Linux. Both give the guest the same contract: a guest home, a project mount, per-exec credentials, and an image whose signature brig checks" src="assets/architecture.svg" width="900">
</p>

What the sandbox keeps out: every host directory except the guest home and the
project you name, your keychain, your SSH agent, and the API-key environment
variables each profile refuses to forward.

What it does not keep out. An agent can change anything in the project you
mounted, because that mount is read-write and those are your real files. It can
read any credential you chose to deliver into the guest. On the default
`shared` network it can reach the internet, so anything it can read it can also
send. Egress filtering exists, and brig enforces it only on hull's `hvi`
backend. On any other backend a policy-bound run is refused rather than run
unenforced.

Image verification defaults to `warn`, which reports an unverifiable image and
boots anyway. Set `BRIG_VERIFY=require` to refuse one instead.

[docs/security.md](docs/security.md) states the boundary and its limits in
full, and [docs/non-goals.md](docs/non-goals.md) says what brig does not try to
be.

## Documentation

| If you want to | Read |
| --- | --- |
| Install brig on any supported host | [docs/install.md](docs/install.md) |
| Get a first agent running, step by step | [docs/quickstart.md](docs/quickstart.md) |
| Understand homes, projects and sessions | [docs/sessions.md](docs/sessions.md) |
| Log an agent in, or give it Git access | [docs/authentication.md](docs/authentication.md), [docs/secrets.md](docs/secrets.md) |
| Look up a command, a flag or a variable | [docs/cli.md](docs/cli.md) |
| Run your own agent or your own image | [docs/profiles.md](docs/profiles.md), [docs/guest-image.md](docs/guest-image.md) |
| Restrict what the guest can reach | [docs/policies.md](docs/policies.md) |
| Understand the isolation, and its limits | [docs/security.md](docs/security.md) |
| Fix something that went wrong | [docs/troubleshooting.md](docs/troubleshooting.md) |
| Move off a retired command spelling | [docs/migration.md](docs/migration.md) |
| Know what is stable and what is not | [docs/stability.md](docs/stability.md) |

The full index is [docs/README.md](docs/README.md).

## Contributing and support

[CONTRIBUTING.md](CONTRIBUTING.md) covers the build, the tests and the review
norms. [AI_POLICY.md](AI_POLICY.md) says how AI-assisted contributions are
handled. Report a vulnerability through [SECURITY.md](SECURITY.md), not a
public issue. For anything else, [docs/support.md](docs/support.md) says where
to ask.

brig counts a small number of events and reports what it counts. Run
`brig telemetry status` to see the current setting, and `brig telemetry off` to
turn the counting off.

## License

Apache License 2.0. See [LICENSE](LICENSE).

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/nofire-logo-on-dark.svg">
    <img alt="NOFire AI" src="assets/nofire-logo.svg" width="120">
  </picture>
</p>
