# Quickstart

This page takes you from an empty terminal to Claude Code running inside a
sandbox, on a throwaway project. It also shows you how to stop it when you
are done. It is one path, start to finish, on macOS.

The same agent runs on Linux: see [install.md#linux](install.md#linux), and
the rest of this page applies there too.

## Prerequisites

Brig itself must already be installed. [install.md](install.md) covers
every platform, and how to verify a download.

You need a Mac with Apple silicon, and macOS 15 or newer. On macOS 14, set
`BRIG_HYPERVISOR=vz` before you run an agent. The default profiles,
including `claude-code`, ask for hull's `hvi` hypervisor backend, and `hvi`
needs macOS 15. On Linux you need `nerdctl`, containerd and the `urunc`
shim instead. [install.md#platform-support](install.md#platform-support)
has the full platform matrix.

## Check what Brig found

```bash
brig doctor
```

Brig prints one line per fact, in this shape:

```
  ok  host      macOS 26.5 on arm64
  ok  virtual   Hypervisor.framework available
  ok  runtime   hull 0.1.0-rc21 at /opt/homebrew/bin/hull
  !!  boot      assets missing at /Users/you/.hull/assets
          run any agent once to fetch them, or set BRIG_BOOT_ASSETS to a directory that has them
  ok  verify    cosign at /opt/homebrew/bin/cosign, BRIG_VERIFY=warn
  ok  profiles  8 built in, 0 in /Users/you/.config/brig
  ok  secrets   keychain reachable
  --  brigd     not running (no socket at /Users/you/.brig/brigd.sock)
  --  image     pass an agent to check its image: brig doctor claude
```

`ok` means Brig found what that line checks. `virtual` reports only
whether this Mac can host a microVM at all, not which backend a run uses.
`!!` prints a fix beside the line and does not stop you here. Brig fetches
missing boot assets itself, the first time an agent needs them. `--` means
Brig looked and found nothing to report, not a failure. `brigd` is an
optional daemon this quickstart does not need.

## Run it

```bash
mkdir -p ~/code/demo && cd ~/code/demo
brig run claude ~/code/demo
```

`claude` is the default session of the `claude-code` agent. `~/code/demo`
is the project this run mounts.

The first run downloads two things: the guest image, and the boot assets,
the kernel and the initrd. Both are cached, and later runs reuse the copy
already on disk. `brig --verbose run` prints the execution envelope before
it boots, and `brig info claude` prints the same thing without running
anything:

```
PROFILE      claude-code
SANDBOX      brig-claude-code (hull)
ISOLATION    microVM (hull, hvi backend)
WORKSPACE    /Users/you/brig/claude-code (read-write)
IMAGE        ghcr.io/brig-sh/claude-code-stock:latest (pull missing)
VERIFY       warn, against brig's own trust policy
CREDENTIALS  (none)
NETWORK      shared (one network for every sandbox on this host)
```

`claude-code` asks for hull's `hvi` backend, which is why `ISOLATION`
names it: `hvi` drives Apple's Hypervisor.framework directly, not
Virtualization.framework. `WORKSPACE` is the CLI's label for the guest
home.

On macOS, hull can ask one question about telemetry before the agent
appears. See [telemetry.md](telemetry.md) for what it counts and how to
turn it off.

Once Brig verifies the image and the boot assets, it prints one line and
starts the sandbox:

```
brig: image and boot assets verified
```

Then Claude Code asks you to log in, because the sandbox holds no
credential. That login happens inside the sandbox. On `claude-code` it
lands on a memory-backed mount, not disk, so `brig stop` takes it with the
sandbox and the next run asks again. Not every agent works this way: see
[sessions.md#what-survives](sessions.md#what-survives) for which do.

## Where the agent's files live

Two host directories are reachable from inside the sandbox, and nothing
else is:

- The **guest home**, `~/brig/claude-code`, mounted as the agent's home.
  Its settings and its history live there, and it survives everything
  short of you deleting it by hand.
- The **project**, `~/code/demo` in the run above, mounted read-write at
  `/work/demo`. The agent starts there.

The agent can change anything under `/work/demo`, because that mount is
read-write and those are your real files. It cannot reach your keychain,
your SSH agent, or any host directory you did not name.
[sessions.md](sessions.md) explains the full model: what a session is,
what each mount keeps separate, and what survives which command.

## You know it worked

The agent's prompt appears, and inside it `pwd` prints `/work/demo`. Leave
the agent running and the sandbox stays up, so a second `brig run claude`
is immediate.

## Stop it

```bash
brig stop claude   # stop the sandbox, keep its name
brig rm claude      # stop and remove it
```

`brig stop` stops the sandbox and keeps its name, its row in `brig ls`,
and what Brig recorded about the session. `brig rm` stops the sandbox and
drops all of that too. Neither touches `~/brig/claude-code` or
`~/code/demo`: your project and the agent's saved state stay where they
are.

If a run does not do what you expected,
[troubleshooting.md](troubleshooting.md) is organized by what you saw on
the terminal.
