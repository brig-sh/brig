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

**Brig runs a coding agent, such as Claude Code, inside a microVM on your own
machine.**

The agent gets a kernel and a home directory of its own, and works on the
one project you name. It does not see the rest of your disk, your keychain or
your SSH agent. So you can let it work without approving each command, and
the only host files it can change are that project and its own home.

<p align="center">
  <img alt="Terminal recording. brig run opens Claude Code in a sandbox, in /work/demo. Asked to write hello.py and run it, Claude writes the file, runs it and lists the directory. After exit, ls on the host shows hello.py in the project." src="assets/brig-run-claude.gif" width="900">
</p>

## Install

| Host | Supported |
| --- | --- |
| Mac, Apple silicon, macOS 15 or newer | Yes |
| Mac, Apple silicon, macOS 14 | Yes, with `BRIG_HYPERVISOR=vz BRIG_NETWORK=shared` set for each run |
| Intel Mac | No |
| Linux, x86-64 or arm64, with KVM | Yes |

On macOS, install with [Homebrew](https://brew.sh):

```bash
brew tap brig-sh/brig
brew trust brig-sh/brig
brew install --cask brig
```

On Linux, install Brig and its runtime for the whole machine. This needs
root:

```bash
curl -fsSL https://raw.githubusercontent.com/brig-sh/brig/main/install.sh | sudo sh
```

After this install, run every `brig` command as root, for example
`sudo brig doctor`. [Install](docs/install.md) covers a Mac without Homebrew,
a Linux install for one user without root, and a build from source.

## First run

Use a new, empty directory as the project, because the agent can change any
file in it.

```bash
brig doctor                      # check the host
mkdir -p ~/code/demo
brig run claude ~/code/demo      # start a sandbox and run Claude Code in it
```

The first run downloads the guest image and the boot assets. Claude Code
then asks you to log in inside the sandbox, so you need an Anthropic
account. With no account, run
`brig run ubuntu ~/code/demo` instead. It opens a root shell in the sandbox
and needs no login.

The agent starts in `/work/demo`, which is `~/code/demo` on the host. Ask it
to create a file there, then exit the agent. The file is in `~/code/demo` on
the host.

The sandbox keeps running after the agent exits, so the next
`brig run claude` starts at once. When you are done:

```bash
brig stop claude                 # stop the sandbox, keep its guest home
brig rm claude                   # remove the sandbox and its guest home
```

`brig stop` and `brig rm` do not touch `~/code/demo`. Delete it yourself when
you no longer need it. [Quickstart](docs/quickstart.md) shows the same steps
with the output of each.

## What the sandbox can reach

- **Files.** The project and the guest home are real host directories, not
  copies. The agent can change or delete anything in the project, `.git`
  included, and git on the host runs the hooks named there. Check them before
  your next git command. See
  [What the host runs from the project](docs/security.md#what-the-host-runs-from-the-project).
- **Credentials.** Brig forwards only what the agent's profile names. With
  nothing exported or stored, the agent starts logged out. `brig info claude`
  lists what a run forwards, by name.
- **Network.** The sandbox can reach the internet, so the agent can send out
  anything it can read. On Linux, and on macOS with the `hvi` backend, a new
  sandbox gets a network of its own, away from other sandboxes. To limit
  what it reaches, bind an [egress policy](docs/policies.md). Brig enforces
  a policy on those two run paths and refuses a policy-bound run elsewhere.
- **Images.** Brig verifies the signatures of its own guest images and of
  the boot assets. By default it boots an image with no signature it can
  check, such as `ubuntu`, with a warning, and it stops when one of its own
  images fails the check. `BRIG_VERIFY=require` refuses every image it
  cannot verify.

[Security](docs/security.md) states each limit in full.

## Next steps

- [Log an agent in, or give it Git access](docs/authentication.md)
- [Run several sessions, and keep a guest home](docs/sessions.md)
- [Restrict what the sandbox can reach](docs/policies.md)
- [Run an agent of your own](docs/profiles.md)
- [Fix an error](docs/troubleshooting.md)

[The documentation index](docs/README.md) lists every page. The site
[brig.sh/docs](https://brig.sh/docs/) has the same documentation, edited for
the web.

## Project

Brig has not reached 1.0, and a minor release can make breaking changes.
[Stability](docs/stability.md) lists what you can script against, and
[Non-goals](docs/non-goals.md) lists what Brig will not do.

- Questions and bugs: [support](docs/support.md).
- Vulnerabilities: [SECURITY.md](SECURITY.md). Never open a public issue for
  one.
- Contributing: [CONTRIBUTING.md](CONTRIBUTING.md) and
  [AI_POLICY.md](AI_POLICY.md).
- Telemetry: the first run on a terminal asks before Brig sends anything.
  `brig telemetry off` turns it off. See [Telemetry](docs/telemetry.md).
- License: Apache License 2.0. See [LICENSE](LICENSE).

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/nofire-logo-on-dark.svg">
    <img alt="NOFire AI" src="assets/nofire-logo.svg" width="120">
  </picture>
</p>
