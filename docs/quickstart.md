# Quickstart

One agent, running in a sandbox, on a Mac that meets the requirements. This is
the shortest path from nothing to a working Claude Code session. Everything
else brig can do is in the [README](../README.md) and the rest of
[docs/](.); this page is only the first run.

You need a Mac. brig boots the sandbox as a microVM over
Virtualization.framework, and macOS 26 is what it is tested on.

## Install

```bash
brew tap brig-sh/brig
brew trust brig-sh/brig       # brew refuses untrusted third-party taps
brew install --cask brig
```

The cask depends on [hull](https://github.com/brig-sh/hull), the microVM
runtime brig drives, so installing brig brings it along. `cosign` is optional.
Without it brig cannot verify the guest image and says so on every boot; with
it the boot is checked. `brew install cosign` if you want the check.

## Run

```bash
brig run claude
```

That boots the sandbox and starts Claude Code inside it. `claude` is the short
name for the `claude-code` profile.

## What the first run does

The first run is the slow one. In order:

- **The envelope is printed.** Before anything boots, brig names the boundary
  the run is about to trust:

  ```
  PROFILE      claude-code
  SANDBOX      brig-claude-code (hull)
  WORKSPACE    /Users/you/brig/claude-code (read-write)
  IMAGE        ghcr.io/brig-sh/claude-code-stock:latest (pull missing)
  CREDENTIALS  GH_TOKEN
  ```

  Credentials are named, never printed. `brig info claude` shows the same block
  without starting anything, and `--quiet` drops it.
- **The image is pulled.** The guest image comes down from the registry once.
  Later runs use the copy already on disk.
- **The image is verified.** brig checks that the image was built by the
  workflow that publishes it, and prints one line:

  ```
  brig: image ghcr.io/brig-sh/claude-code-stock:latest: signature verified
  ```

  Then it starts the sandbox:

  ```
  brig: starting sandbox brig-claude-code...
  ```

- **The agent asks you to log in.** The sandbox boots with no credential, so
  Claude Code prompts you to log in exactly as it would on a fresh machine.
  That login happens inside the sandbox.

The login lives in the sandbox's memory. It lasts as long as the sandbox does,
and never reaches your disk. Stop the sandbox and the next run asks again. To
carry the login you already have on this Mac into the sandbox, and keep it
across stops, see [Credentials](../README.md#credentials).

## The one directory the agent can see

The agent's whole world is one host directory, mounted as its home:

```
~/brig/claude-code
```

Nothing else on your machine is reachable from inside the sandbox. Put the
projects the agent should work on in that directory. It cannot reach your
keychain, your SSH agent, or any other folder. That directory holds the
agent's settings, its history, and your projects, and it stays on the host
across restarts.

## Keep going, or stop

The sandbox stays up between commands, so a second `brig run claude` is
immediate. When you are done:

```bash
brig stop claude    # stop the sandbox, keep it
brig rm claude      # stop and remove the sandbox
```

`brig stop` keeps the sandbox so starting it again is a boot, not a fresh
creation. It takes the in-sandbox login with it. `brig rm` removes the sandbox
as well. Neither touches `~/brig/claude-code`: your projects and the agent's
saved state stay where they are. To drop those too, remove the directory by
hand.

If a run fails, [troubleshooting.md](troubleshooting.md) is organised by what
you saw on the terminal.
