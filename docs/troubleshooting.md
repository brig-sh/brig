# Troubleshooting

This page is organised by what you saw on the terminal. Find the message,
read the likely cause, run the check, apply the fix.

Some of these messages are brig's own. Others come from the layer underneath:
the microVM runtime (`hull` on macOS, `nerdctl` on Linux), cosign, or
Homebrew. Where a message is not brig's, it says so. That is the first
thing to know when the wording does not match anything in brig.

Before reading further, run `brig doctor`. It checks the host, the
hypervisor, the runtime and its version, the boot assets, cosign, the
profiles, the secret store and brigd, one line each. It names the fix for
a line that is not `ok`. `brig doctor <agent>` checks that agent's image too.
`--json` prints the same report as one document, for a script or a bug
report.

## Exit codes

Every failure exits with one of a small, stable set of codes. The full table,
and what each one means, is at
[docs/cli.md#exit-codes](cli.md#exit-codes).

One thing that page does not say: `brig rm` and `brig logs` on a ref with no
sandbox exit `3`, naming the ref you typed. They do not pass the runtime's
own "instance not found" back as a general failure (`1`).

## What `brig doctor` does not catch

Only two lines in `brig doctor` change its exit status: a missing or broken
runtime, and a secret store that will not open. Every other line, a `!!`
included, prints its fix and leaves the exit status at `0`.

That includes verification. A `BRIG_VERIFY` value brig does not recognize,
and `BRIG_VERIFY=require` with no cosign installed, both print `!!` with the
right fix. Neither one stops `brig doctor` from exiting `0`, and both stop a
real run cold. A script that checks only the exit status, not the lines,
misses both.

Check the setting yourself before you rely on it:

```bash
echo "$BRIG_VERIFY"
```

`require` also refuses any image outside brig's own registry,
`ghcr.io/brig-sh/`. `claude-desktop` and `ubuntu` are both outside it.
`brig doctor claude-desktop` reports that image as `--`, informational,
whatever `BRIG_VERIFY` is set to. It does not simulate the refusal a
`require` run hits.

## The sandbox never became ready

```
brig: sandbox did not become ready; check 'brig logs claude (or the runtime's own, hull logs brig-claude-code)'
```

On Linux the second half names `nerdctl logs` instead.

The runtime reported the sandbox running, but the agent inside it never
answered. Those are two different moments: the VM process starts, and a few
seconds later the guest binds its listener. brig waits for the second one and
gave up.

Read the log, which is what the message points at:

```bash
brig logs claude
```

That is `hull logs` underneath, and the message names that spelling too, for
a boot that never became a sandbox brig can address by ref.

The guest's own errors are there, not in brig's output. If the guest is only
slow rather than broken, give it longer with `BRIG_READY_TIMEOUT` (seconds,
default 30):

```bash
BRIG_READY_TIMEOUT=60 brig run claude
```

## dyld: missing symbol called

```
VMM started (PID 33351)
brig: sandbox did not become ready; check 'brig logs claude (or the runtime's own, /opt/homebrew/bin/hull logs brig-claude-code)'
$ brig logs claude
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
15 or newer. macOS 26 is what hull is developed and tested on.

Current releases do not get this far. brig reads the macOS version before it
asks the runtime for anything. It refuses an `hvi` run on macOS 14, with the
cause and the way past it:

```
brig: the hvi hypervisor needs macOS 15 or newer (this is 14.5): its in-kernel
interrupt controller does not exist here. Set BRIG_HYPERVISOR=vz for this run,
or upgrade macOS
```

The `dyld` log above is what an older brig left you to find. Confirm by
running the agent again. With `BRIG_HYPERVISOR=vz` set, or on macOS 15 or
newer, the run reaches the agent's prompt instead of dying at `dyld`.

## No runtime found on PATH

```
brig: no runtime found on PATH: brig drives hull on macOS, and none was there.
See https://github.com/brig-sh/brig#macos, or point BRIG_RUNTIME_BIN at a build
```

On Linux the second half is different, because the runtime there is `nerdctl`:

```
brig: no runtime found on PATH: install nerdctl, or point BRIG_RUNTIME_BIN at one
```

Either way the exit code is `4`.

brig delegates every boot to a runtime it does not ship, and none was there.
On macOS the cask depends on hull, so this usually means a from-source
install without hull on PATH. Full install instructions, both platforms, are
at [docs/install.md](install.md).

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

Confirm:

```bash
brig doctor
```

The `runtime` line reads `ok` and names the binary it found.

## unknown BRIG_RUNTIME

```
brig: runtime unavailable: unknown BRIG_RUNTIME "podman" (want hull or nerdctl)
```

This is not the same failure as a missing runtime. brig treats it as a
different mistake on purpose: `BRIG_RUNTIME` names something brig does not
drive. `brig ls` and `brig info` fail this same way rather than reporting
no sandboxes, because reading a typo as "you have none" hides it.

Check what is set:

```bash
echo "$BRIG_RUNTIME"
```

Set it to `hull` or `nerdctl`, or unset it so brig looks on PATH instead:

```bash
unset BRIG_RUNTIME
```

Confirm:

```bash
brig doctor
```

The `runtime` line reads `ok` again.

## this profile's runtimeBin is ... which is not there

```
brig: runtime unavailable: this profile's runtimeBin is /old/path/hull, which is
not there: stat /old/path/hull: no such file or directory
```

Your own profile's `runtimeBin:` field names a binary that moved or was
removed. `brig doctor` does not catch this: its `runtime` line reads
`BRIG_RUNTIME_BIN`, never a single profile's own field. A broken
`runtimeBin` in `mine` reads `ok` there, and fails only when you run `mine`.

Open the profile and fix or remove the line:

```bash
brig agent edit mine
```

Confirm:

```bash
brig info mine
```

No runtime error means the field is fixed.

## assets missing at ...

Seen in `brig doctor`, not from a run:

```
!!  boot      assets missing at /Users/pmoust/.hull/assets
        run any agent once to fetch them, or set BRIG_BOOT_ASSETS to a directory that has them
```

Six of the eight built-in profiles boot an unmodified OCI image rather than
their own. They need a shared kernel and initrd, which brig calls the boot
assets. Nothing has downloaded that bundle yet. This is normal before a first
boot and does not stop one: the next `brig run` on any of those profiles
fetches it.

Confirm it downloaded:

```bash
brig doctor
```

The `boot` line reads `ok  boot  assets present at ...`.

## The image could not be pulled, or the architecture does not match

```
brig: could not start the sandbox: <runtime error>
```

The detail after the colon is the runtime's, not brig's. It is a registry
brig cannot reach, an image reference that does not exist, or a manifest
with no build for your architecture. The five published agents default to
`:latest`, which `brig-sh/community-images` publishes as a multi-arch index
covering `linux/arm64` and `linux/amd64`. One reference is meant to work on
both an Apple Silicon Mac and an x86 Linux host. A mismatch usually means a
`--image` or `BRIG_IMAGE` pinned to a single-architecture tag (`:arm64` or
`:amd64`) that is not yours.

Check the reference you are booting:

```bash
brig info claude      # shows the image, among other things
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
before it reaches the registry. It does not fail with a 404. Build the
image yourself and pass `--image`.

## docker does not carry annotations through to the runtime

Linux only, and only for a profile that boots an unmodified image (six of the
eight built-in ones):

```
brig: could not start the sandbox: this profile boots an unmodified image,
which needs the kernel passed as an OCI annotation; docker does not carry
annotations through to the runtime. Use nerdctl, or point BRIG_RUNTIME_BIN at
it
```

brig accepts Docker where it looks for nerdctl, and most of what it needs
works either way. Passing the kernel as an OCI annotation does not: Docker
drops it, so the guest starts with no kernel to boot. brig refuses
rather than let that fail somewhere further from the cause.

Install nerdctl, or point brig at one you already have:

```bash
BRIG_RUNTIME_BIN=/path/to/nerdctl brig run claude
```

Confirm:

```bash
brig doctor
```

The `runtime` line names `nerdctl`, not `docker`.

## cosign is not installed

```
brig: cannot verify image ghcr.io/brig-sh/claude-code-stock:latest: cosign is not
installed (`brew install cosign`). Booting it unchecked
```

That is `BRIG_VERIFY=warn`, the default. brig checked the image against its
own registry, found no way to run the check, and says so before it boots
anyway. A check that did not run is not a check that failed.

Under `BRIG_VERIFY=off` cosign is never looked up, and the only line is:

```
brig: BRIG_VERIFY=off, so the guest image is not checked before it boots
```

Under `BRIG_VERIFY=require` the same missing-cosign message appears, with the
mode named. This time it refuses the boot instead of continuing (exit
`5`), even though the wording still reads "Booting it unchecked":

```
brig: cannot verify image ghcr.io/brig-sh/claude-code-stock:latest: cosign is not
installed (`brew install cosign`). Booting it unchecked (BRIG_VERIFY=require)
```

Read the exit code, not the last clause of the message: `require` with no
cosign refuses every image, brig's own included.

Install cosign to get the check:

```bash
brew install cosign         # macOS
```

Linux: no package covers every distro. Install a release from
[sigstore/cosign](https://github.com/sigstore/cosign) and put it on PATH.

To boot without the check and stop the warning, set
`BRIG_VERIFY=off`. Leaving cosign installed is the safer choice.

Confirm:

```bash
cosign version
```

## The signature did not verify

```
brig: image ghcr.io/brig-sh/claude-code-stock:latest claims to be published by
brig-sh, but its signature DID NOT VERIFY: <detail>
brig: Boot it anyway? [y/N]
```

This is not the same as a check that did not run. cosign ran and the answer
was no. The image sits under brig's own registry, and its signature does not
match the workflow that is meant to have built it. That combination has no
innocent reading, so brig stops and asks. With no terminal to ask, it refuses:

```
brig: not a terminal, so there is nobody to ask: refusing. Set
BRIG_VERIFY=off to boot it regardless.
```

Answering no aborts, with exit code `5`:

```
brig: aborted: the image failed verification. Pull it again (BRIG_PULL=always),
or set BRIG_IMAGE to a digest you have checked yourself
```

The usual innocent cause is a stale local copy. Pull the image again
(`BRIG_PULL=always brig run claude`) and let the check run against the current
registry.

If it still fails and you do not know why, do not boot it. Naming a digest
you checked yourself is the deliberate way past it. The abort message above
does not offer `BRIG_VERIFY=off`, because disabling the control that caught
it is not a remedy. The no-terminal refusal earlier in this section names
that setting only because a scripted run has no other way to say yes in
advance.

An image published by someone else warns rather than stopping: bring-your-own
images are supported. A failure under brig's own registry is the one case
that stops.

## The agent asked me to log in again after a stop

There is no error here. The agent shows its login screen on a sandbox you had
already logged into.

On `claude-code` and `claude-desktop`, the in-guest login lives in the
sandbox's memory. It is written to a memory-backed mount that never reaches
host disk. `brig stop` takes it with the VM, and the next `brig run` starts
fresh. That is by design, and it is specific to those two profiles. The other
six mount the guest home from host disk, so a login written there survives a
stop.

To make a `claude-code` or `claude-desktop` login survive a stop, import the
one already on this Mac into brig's own store, once:

```bash
brig secret import claude-code
```

After that brig delivers the login on every command that reaches the
sandbox, so a stop no longer loses it. See
[Carry your host login in, once](authentication.md#2-carry-your-host-login-in-once).

## My credential did not arrive

First, ask brig what it forwards, by name:

```bash
brig info claude
```

That reports what reaches the guest and whether the guest will be
authenticated. If the variable you expected is not listed, one of these is
why.

**It is on the denylist.**

```
brig: not forwarding ANTHROPIC_API_KEY: it is on the claude-code denylist,
because it outranks the subscription credential and would move this sandbox
onto metered billing without saying so. Set BRIG_ALLOW_DENIED=1 if that is
what you want
```

A key that switches the sandbox from your subscription onto metered
billing is refused by default. Forward it only when metered billing is
genuinely what you want, with `BRIG_ALLOW_DENIED=1`.

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
cannot shadow a value baked into the image.

A stored credential that has expired is not withheld. brig forwards it as it
is and warns before boot. Dropping it silently looks exactly
like an unexplained login failure with nothing to act on:

```
brig: the imported credential claude-credentials (claude-code) expired 3d ago.
brig: Renew it on the host, then: brig secret import claude-code
```

Renew the login on the host and import it again, as the second line says.
Renewing on the host alone does not help: a run reads brig's stored copy, and
nothing re-reads the host until an import says so.

A secret you stored with `--from-command` prints a different second line,
naming that command instead of an import:

```
brig: the imported credential <name> (claude-code) expired 3d ago.
brig: Renew it, then store it again: brig secret import claude-code <name> --from-command '<command>'
```

## The sandbox restarted when I ran sh

Two different things trigger this, and both recreate the sandbox rather than
fail it. All persistent state lives in the guest home on the host
either way. Any other session on that sandbox is disconnected when it
restarts.

**A different guest home than the one remembered.**

```
brig: the running sandbox is not mounting /Users/alex/work -- its share went
stale (the directory was renamed or replaced, or the workspace changed).
Restarting it; any other session using this sandbox will be disconnected.
```

brig compares the guest home the running sandbox has against the one this
command asked for. Passing an explicit `--home` (or `BRIG_WORKSPACE`) that
does not match what the sandbox already has trips this.

Check which guest home a sandbox is mounting, in the `WORKSPACE` column:

```bash
brig ls
```

If you did not mean to change it, drop the `--home` flag so `sh` addresses
the same session the sandbox already has. There is no flag today that runs
against a different guest home without this restart. Until one lands, an
explicit `--home` that differs from the remembered default restarts the
sandbox.

**A network policy that no longer matches what is running.**

```
brig: this sandbox is running under a different network policy than the one
that applies now. Rules are fixed when a sandbox boots, so it is being
restarted; any other session using this sandbox will be disconnected.
```

Egress rules are fixed at boot. Attaching or detaching a policy after a
sandbox is already up changes nothing it can reach until the next restart.
The next `brig sh` or `brig run` on that session triggers one.

Check what a profile has bound, and whether brig can enforce it:

```bash
brig policy check claude
```

## brew trust is not a command

```
Error: Unknown command: trust
```

This message is Homebrew's, not brig's. `brew trust` needs a recent Homebrew,
and an older one does not have it.

Check your version and update:

```bash
brew --version
brew update
```

After updating, `brew trust brig-sh/brig` works and you can carry on with the
install.

## A symlinked or moved guest home was refused

```
brig: refusing to write /Users/alex/brig/claude-code/.claude.json: it is a
symlink to "/Users/alex/.ssh/authorized_keys", and brig writes only regular
files inside the workspace. The workspace is mounted read-write as the
sandbox's home, so that link was put there from inside the sandbox, to have
brig -- which runs as you, on the host -- reach a file the sandbox cannot.
Nothing was written; inspect /Users/alex/brig/claude-code/.claude.json and
remove it before running brig again: a symlink in the workspace leads out of it
```

brig writes state files into the guest home from the host, as you. A
symlink where one of those files belongs points brig at a host path the
sandbox itself cannot reach. brig refuses rather than following it. It
writes only regular files there, so a link in the way was put there on
purpose, or by the sandbox reaching for the host. Nothing was written.

This is not a case for retrying. Inspect the path the message names and
remove the link, or point the guest home somewhere else. Pointing `--home`
at a symlink is refused the same way, and is fixed by naming the real
directory.

Confirm:

```bash
brig run claude
```

The run reaches the agent instead of refusing.
[docs/security.md](security.md#writing-into-the-workspace) explains why the
refusal exists.
