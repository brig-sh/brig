# Quickstart

This page takes you from an empty terminal to Claude Code running inside a
sandbox, on a throwaway project. It also shows you how to stop it when you
are done. It is one path, start to finish. The [README](../README.md) and
the rest of [docs/](.) cover everything else brig can do.

## Prerequisites

You need a Mac with Apple silicon. The default agent profiles, including
`claude-code`, ask for hull's `hvi` backend, and `hvi` needs macOS 15 or
newer. On macOS 14, set `BRIG_HYPERVISOR=vz` before you run one. See
[docs/support.md](support.md) for the full platform matrix.

brig itself must already be installed. [docs/install.md](install.md) covers
every platform, and how to verify a download.

## Check what brig found

```bash
brig doctor
```

brig prints one line per fact, in this shape:

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

`ok` means brig found what that line checks. `virtual` reports only whether
this Mac can host a microVM at all, not which backend a run uses. `!!`
prints a fix beside the line and does not stop you here. brig fetches
missing boot assets itself, the first time an agent needs them. `--` means
brig looked and found nothing to report, not a failure. `brigd` is an
optional daemon this quickstart does not need.

## Run it

```bash
mkdir -p ~/code/demo && cd ~/code/demo
brig run claude ~/code/demo
```

`claude` is the default session of the `claude-code` agent. `~/code/demo`
is the project this run mounts.

The first run is the slow one: brig pulls the guest image once, and later
runs reuse the copy already on disk. `brig --verbose run` prints the
execution envelope before it boots, and `brig info claude` prints the same
thing without running anything:

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

`claude-code` asks for hull's `hvi` backend, which is why `ISOLATION` names
it: `hvi` drives Apple's Hypervisor.framework directly, not
Virtualization.framework. `WORKSPACE` here is brig's own label for the
guest home, the row this page calls the guest home everywhere else.

Once brig verifies the image and the boot assets, it prints one line and
starts the sandbox:

```
brig: image and boot assets verified
```

Then Claude Code asks you to log in, because the sandbox holds no
credential. That login happens inside the sandbox. On `claude-code` it
lands on a memory-backed mount, not disk, so `brig stop` takes it with the
VM and the next run asks again. Not every agent works this way: see
[sessions.md](sessions.md#what-survives) for which do.

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

`brig stop` keeps the sandbox's name, its row in `brig ls`, and what brig
recorded about the session. `brig rm` drops the last of those too. Neither
touches `~/brig/claude-code` or `~/code/demo`: your project and the
agent's saved state stay where they are.

If a run does not do what you expected,
[troubleshooting.md](troubleshooting.md) is organized by what you saw on
the terminal.
