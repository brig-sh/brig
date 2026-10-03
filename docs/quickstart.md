# Quickstart

This page starts Claude Code inside a sandbox on a throwaway project, shows
what each step prints, and removes the sandbox at the end. The examples are
from macOS. The same commands work on Linux, where the paths and the
runtime named in the output differ. After a node-wide Linux install, run
each command as root (`sudo brig ...`).

## Prerequisites

- Brig, installed. [install.md](install.md) covers every platform.
- On macOS: a Mac with Apple silicon and macOS 15 or newer. The default
  profiles, including `claude-code`, use hull's `hvi` hypervisor backend,
  which needs macOS 15. On macOS 14, set `BRIG_HYPERVISOR=vz` and
  `BRIG_NETWORK=shared` before you run an agent. A `shared` network does not
  separate sandboxes from each other.
- On Linux: `nerdctl`, containerd and a `urunc` shim that reads Brig's boot
  annotations. `install.sh` installs all three.
- An Anthropic account, to log Claude Code in. To follow this page without
  one, use `ubuntu` wherever it says `claude`. That agent is a plain root
  shell and needs no login. Its image is not published by brig-sh, so Brig
  prints a note that there is no signature to check, and the summary line
  reads `brig: boot assets verified`.

[install.md#platform-support](install.md#platform-support) has the full
platform matrix.

## Check what Brig found

```bash
brig doctor
```

Brig prints one line per fact, in this shape:

```
  ok  brig      v0.3.0 (91b0c7b, 2026-09-26, go1.26.1, darwin/arm64)
  ok  host      macOS 26.5 on arm64
  ok  virtual   Hypervisor.framework available
  ok  runtime   hull 0.1.0-rc29 at /opt/homebrew/bin/hull
  !!  boot      assets missing at /Users/you/.hull/store/assets
          run any agent once to fetch them, or set BRIG_BOOT_ASSETS to a directory that has them
  ok  verify    cosign at /opt/homebrew/bin/cosign, BRIG_VERIFY=warn
  ok  profiles  8 built in, 0 in /Users/you/.config/brig
  ok  secrets   keychain reachable
  --  brigd     not running (no socket at /Users/you/.brig/brigd.sock)
  --  image     pass an agent to check its image: brig doctor claude
```

- `ok` means Brig found what that line checks.
- `!!` means a check failed, and the fix is printed under it. The `boot`
  line above does not stop you: Brig fetches missing boot assets the first
  time an agent needs them.
- `--` means there is nothing to report. It is not a failure.

`virtual` reports whether this Mac can host a microVM at all. It does not
say which backend a run uses. `brigd` is an optional daemon that this page
does not need.

## Run it

```bash
mkdir -p ~/code/demo
brig run claude ~/code/demo
```

`claude` is the default session of the `claude-code` agent. `~/code/demo`
is the project this run mounts.

The first run downloads two things: the guest image, and the boot assets
(the kernel and the initrd). Both are cached, so later runs start faster.

On a terminal, each download shows a spinner. With stderr redirected, with
the run in the background, or with `TERM=dumb`, Brig prints a line when each
download starts and another when it completes. A download that fails prints
the error. With nerdctl on Linux, Brig announces only the boot assets,
because nerdctl pulls the image itself.

Brig then prints what you may need to act on. On a first run that is a note
that the guest home is temporary. On macOS, Brig also lists each secret the
agent runs without. Neither stops the run.

Once Brig has verified the image and the boot assets, it prints one line and
starts the sandbox:

```
brig: image and boot assets verified
```

On macOS, hull can ask one question about telemetry before the agent
appears. See [telemetry.md](telemetry.md) for what it counts and how to
turn it off.

Then Claude Code asks you to log in, because the sandbox holds no login
for it. The login happens inside the sandbox. On `claude-code` it is
stored on a memory-backed mount, so `brig stop` discards it and the next
run asks again. Other agents keep their login on disk:
[sessions.md#what-survives](sessions.md#what-survives) says which.
[authentication.md](authentication.md) shows how to reuse a login from the
host.

## Check that it worked

The agent's prompt appears, and inside it `pwd` prints `/work/demo`. The
sandbox keeps running after the agent exits, so a second `brig run claude`
starts at once. `brig ls` lists it:

```
REF         SANDBOX          STATE      WORKSPACE
claude-code brig-claude-code running    /Users/you/.brig/homes/brig-claude-code
```

On Linux, `STATE` is nerdctl's own wording, such as `Up`.

## See what a run was given

The execution envelope is the summary of what a sandbox gets: its image, its
mounts, its network and its credentials. `brig info claude` prints it
without booting anything, and `brig --verbose run` prints it before the
boot:

```
PROFILE      claude-code
SANDBOX      brig-claude-code (hull)
ISOLATION    microVM (hull, hvi backend)
WORKSPACE    /Users/you/.brig/homes/brig-claude-code (read-write)
IMAGE        ghcr.io/brig-sh/claude-code-stock:root (pull missing)
VERIFY       warn, against brig's own trust policy
CREDENTIALS  IS_SANDBOX
NETWORK      isolated (a network of this sandbox's own)
```

That is the envelope on a host with no `GH_TOKEN` exported and no stored
secret. With either, the `CREDENTIALS` row names it too.

- `ISOLATION` names the `hvi` backend because `claude-code` asks for it.
  `hvi` drives Apple's Hypervisor.framework directly.
- `WORKSPACE` is the CLI's label for the guest home.
- `CREDENTIALS` lists every variable and secret the run forwards, by name.
  `IS_SANDBOX` is a plain marker the profile sets, with no secret in it.
- `NETWORK` is `isolated` for a new sandbox: it is kept off other
  sandboxes' networks and can still reach the internet. A session created
  by an older release can report `shared`, because an existing session
  keeps the network it already has.

## Where the agent's files live

Two host directories are reachable from inside the sandbox, and nothing
else is:

- The **guest home**, `~/.brig/homes/brig-claude-code`, mounted as the
  agent's home. Its settings and its history live there. Brig created it, so
  it survives `brig stop` and goes with `brig rm`. Pass `--home <dir>` to
  keep a guest home of your own instead.
- The **project**, `~/code/demo` in the run above, mounted read-write at
  `/work/demo`. The agent starts there.

The agent can change anything under `/work/demo`, because that mount is
read-write and those are your real files. It cannot reach your keychain,
your SSH agent, or any host directory you did not name.
[sessions.md](sessions.md) explains the full model: what a session is,
what each mount keeps separate, and what survives which command.

## Stop it and clean up

```bash
brig stop claude   # stop the sandbox, keep the session
brig rm claude     # stop the sandbox and remove the session
```

`brig stop` stops the sandbox. It keeps the sandbox's name, its row in
`brig ls`, and what Brig recorded about the session.

`brig rm` stops the sandbox, removes all of that, and deletes
`~/.brig/homes/brig-claude-code`.

Neither command touches `~/code/demo`. Delete it yourself if you no longer
want it.

## Next steps

- [authentication.md](authentication.md): log an agent in, or give it Git
  access.
- [sessions.md](sessions.md): run several sessions, and keep a guest home.
- [policies.md](policies.md): restrict what the guest can reach.
- [troubleshooting.md](troubleshooting.md): organized by what you saw on the
  terminal.
