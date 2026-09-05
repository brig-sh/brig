# Troubleshooting

This page is organised by what you saw on the terminal. Find the message,
read the likely cause, run the check, apply the fix.

Some of these messages are brig's own. Others come from the layer underneath:
the microVM runtime (`hull` on macOS, `nerdctl` on Linux), cosign, or
Homebrew. Where a message is not brig's, it says so, because that is the first
thing to know when the wording does not match anything in brig.

## The sandbox never became ready

```
brig: sandbox did not become ready; check 'hull logs brig-claude-code'
```

On Linux the hint names `nerdctl logs` instead.

The runtime reported the sandbox running, but the agent inside it never
answered. Those are two different moments: the VM process starts, and a few
seconds later the guest binds its listener. brig waits for the second one and
gave up.

Check the runtime's own logs, which is what the message points at:

```bash
hull logs brig-claude-code
```

The guest's own errors are there, not in brig's output. If the guest is only
slow rather than broken, give it longer with `BRIG_READY_TIMEOUT` (seconds,
default 30):

```bash
BRIG_READY_TIMEOUT=60 brig run claude
```

## dyld: missing symbol called

```
VMM started (PID 33351)
brig: sandbox did not become ready; check '/opt/homebrew/bin/hull logs brig-claude-code'
$ hull logs brig-claude-code
dyld[33351]: missing symbol called
```

That is the whole log, and the symbol is never named. The cause is the
hypervisor. Six of the eight built-in profiles ask for `hvi`, and `hvi` uses
Apple's in-kernel interrupt controller, the `hv_gic_*` calls, which arrived in
macOS 15. On macOS 14 those symbols do not exist, so the VMM dies the moment it
starts. The `vz` backend, Virtualization.framework, works on macOS 14.

For one run:

```bash
BRIG_HYPERVISOR=vz brig run claude
```

For good, put `BRIG_HYPERVISOR=vz` in your shell profile, or upgrade to macOS
15 or newer. macOS 26 is what hull is developed and tested on. A check that
refuses `hvi` on an older macOS with this message, instead of letting the VMM
crash, is in flight.

## No microVM runtime found on PATH

```
brig: no microVM runtime found on PATH. brig drives hull on macOS: see
https://github.com/brig-sh/brig#macos. Or point BRIG_RUNTIME_BIN at a build
```

On Linux the message is different, because the runtime there is `nerdctl`:

```
brig: no container runtime found: install nerdctl, or point
BRIG_RUNTIME_BIN at one
```

brig delegates every boot to a runtime it does not ship, and could not find
one. On macOS the cask depends on hull, so this usually means a from-source
install without hull on PATH.

Check whether the runtime is there:

```bash
which hull        # macOS
which nerdctl     # Linux
```

Install it (`brew install --cask brig` brings hull along on macOS), or, if you
have a build somewhere off PATH, point brig at it:

```bash
BRIG_RUNTIME_BIN=/path/to/hull brig run claude
```

## The image could not be pulled, or the architecture does not match

```
brig: could not start the sandbox: <runtime error>
```

The detail after the colon is the runtime's, not brig's: a registry that could
not be reached, an image reference that does not exist, or a manifest with no
build for your architecture. The published agents default to `:latest`, a
multi-arch index covering `linux/arm64` and `linux/amd64`, so one reference is
right on both an Apple Silicon Mac and an x86 Linux host. A mismatch usually
means a `--image` or `BRIG_IMAGE` pinned to a single-architecture tag that is
not yours.

Check the reference you are booting:

```bash
brig env claude       # shows the image, among other things
```

Read the runtime's error for which of the two it is. If it is the wrong
architecture, drop the pinned tag or pin the one for your machine (`:arm64` or
`:amd64`). If the registry was the problem, try again once it is reachable. A
moving tag that was republished stays invisible under the default pull policy
until you ask for it:

```bash
BRIG_PULL=always brig run claude
```

If the image is one brig does not publish, such as `cursor`, brig says so
before it reaches the registry rather than failing on a 404: build the image
yourself and pass `--image`.

## cosign is not installed

```
brig: cannot verify image ghcr.io/brig-sh/claude-code-stock:latest: cosign is not
installed (`brew install cosign`). Booting it unchecked
```

This is a warning, not a failure. brig verifies the guest image before booting
it, and without cosign it cannot run the check. "Could not check" is not the
same as "failed": the boot goes ahead.

If you want the check, install cosign:

```bash
brew install cosign
```

If you would rather boot without it and stop the warning, that is
`BRIG_VERIFY=off`. Leaving cosign installed is the safer choice.

## The signature did not verify

```
brig: image ghcr.io/brig-sh/claude-code-stock:latest claims to be published by
brig-sh, but its signature DID NOT VERIFY: <detail>
brig: Boot it anyway? [y/N]
```

This is not the same as "could not check". cosign ran and the answer was no:
the image sits under brig's own registry and its signature does not match the
workflow that should have built it. That combination has no innocent reading,
so brig stops and asks. With no terminal to ask, it refuses:

```
brig: not a terminal, so there is nobody to ask: refusing. Set
BRIG_VERIFY=off to boot it regardless.
```

Answering no aborts:

```
brig: aborted: the image failed verification. Pull it again, or set
BRIG_VERIFY=off if you know why it fails
```

The usual innocent cause is a stale local copy. Pull the image again
(`BRIG_PULL=always brig run claude`) and let the check run against the current
registry. If it still fails and you do not know why, do not boot it. An image
published by someone else warns rather than stopping, because bring-your-own
images are supported; a failure under brig's registry is the one case that
stops.

## The agent asked me to log in again after a stop

There is no error here. The agent shows its login screen on a sandbox you had
already logged into.

The in-sandbox login lives in the sandbox's memory. It is written to a
memory-backed mount that never reaches host disk, so `brig stop` takes it with
the VM and the next `brig run` starts fresh. That is by design.

To make a login survive a stop, import the one already on this Mac into brig's
own store, once:

```bash
brig secret import claude-code
```

After that brig writes the login back in on every boot, so a stop no longer
loses it. See [Credentials](../README.md#credentials).

## My credential did not arrive

First, ask brig what it would forward, by name:

```bash
brig env claude
```

That reports what reaches the guest and whether the guest will be
authenticated. If the variable you expected is not listed, one of these is why.

**It is on the denylist.**

```
brig: not forwarding ANTHROPIC_API_KEY: it is on the claude-code denylist,
because it outranks the subscription credential and would move this sandbox
onto metered billing without saying so. Set BRIG_ALLOW_DENIED=1 if that is
what you want
```

A key that would switch the sandbox from your subscription onto metered
billing is refused by default. Forward it only if metered billing is genuinely
what you want, with `BRIG_ALLOW_DENIED=1`.

**It looks like an unresolved reference.**

```
brig: not forwarding GH_TOKEN: it looks like an unresolved secret reference
(op://...), not a credential. Resolve it on the host before invoking brig,
or set BRIG_ALLOW_REFS=1 to forward it as-is
```

A `scheme://` value is what tools like direnv leave in the environment when a
secret-manager reference was never resolved. Forwarded as-is it produces
"Invalid username or token" inside the guest, which looks exactly like a
broken sandbox, so brig refuses it. Resolve it on the host so the variable
holds the real token, then run again. A stored secret or a profile literal is
exempt from this check, because it was put there on purpose.

**It is empty, or it expired.** An unset or empty variable is skipped so it
cannot shadow a value baked into the image. A stored login that has expired is
withheld, with:

```
brig: the imported credential claude-credentials (claude-code) expired 3d ago.
brig: Renew it on the host, then: brig secret import claude-code
```

Renew the login on the host and import it again, as the second line says.
Renewing on the host alone does not help: a run reads brig's stored copy, and
nothing re-reads the host until an import says so.

## The sandbox restarted when I ran exec

```
brig: the running sandbox is not mounting /Users/alex/work -- its share went
stale (the directory was renamed or replaced, or the workspace changed).
Restarting it; any other session using this sandbox will be disconnected.
```

brig compares the workspace the running sandbox has against the one this
command asked for, and restarts the sandbox when they differ. Passing an
explicit `--workspace` (or `BRIG_WORKSPACE`) that does not match the default
the sandbox was started with trips this: brig reads it as the share having
gone stale and recreates the VM.

Check which workspace the sandbox is on:

```bash
brig ls               # shows each sandbox and its workspace
```

If you did not mean to change the workspace, drop the `--workspace` flag so
exec addresses the same session the sandbox already has. There is no flag today
that runs against a different workspace without this restart; a fix is in
flight, and until it lands the honest description is that an explicit
`--workspace` that differs from the remembered default restarts the sandbox.

## brew trust is not a command

```
Error: Unknown command: trust
```

This message is Homebrew's, not brig's. `brew trust` was added to Homebrew at a
certain version, and an older Homebrew does not have it.

Check your version and update:

```bash
brew --version
brew update
```

After updating, `brew trust brig-sh/brig` works and you can carry on with the
install.

## A symlinked or moved workspace was refused

```
brig: refusing to write /Users/alex/brig/claude-code/.claude.json: it is a
symlink to "/Users/alex/.ssh/authorized_keys", and brig writes only regular
files inside the workspace. The workspace is mounted read-write as the
sandbox's home, so that link was put there from inside the sandbox, to have
brig -- which runs as you, on the host -- reach a file the sandbox cannot.
Nothing was written; inspect /Users/alex/brig/claude-code/.claude.json and
remove it before running brig again
```

brig writes state files into the workspace from the host, as you. A symlink
where one of those files belongs points brig at a host path the sandbox itself
cannot reach, so brig refuses rather than following it. It writes only regular
files in there, so a link in the way was put there on purpose or by the
sandbox reaching for the host. Nothing was written.

This is not a case for retrying. Inspect the path the message names and remove
the link, or point the workspace somewhere else. Pointing `--workspace` at a
symlink is refused the same way, and is fixed by naming the real directory.
[docs/security.md](security.md#writing-into-the-workspace) explains why the
refusal exists.
