# Troubleshooting

This page is organised by what you saw on the terminal. Find the message,
read the likely cause, apply the fix, then run the command under Confirm to
prove it worked.

Some of these messages are Brig's own. Others come from the layer underneath:
the microVM runtime (`hull` on macOS, `nerdctl` on Linux), cosign, or
Homebrew. Where a message is not Brig's, it says so. That is the first
thing to know when the wording does not match anything in Brig.

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

Only two lines in `brig doctor` change its exit status, using the codes a
run exits with: see [Exit codes](cli.md#exit-codes). Every other line, a
`!!` included, prints its fix and leaves the exit status at `0`.

That includes verification. A `BRIG_VERIFY` value Brig does not recognize,
and `BRIG_VERIFY=require` with no cosign installed, both print `!!` with the
right fix. Neither one stops `brig doctor` from exiting `0`, and both stop a
real run cold. A script that checks only the exit status, not the lines,
misses both.

Check the setting yourself before you rely on it:

```bash
echo "$BRIG_VERIFY"
```

`require` also refuses any image outside Brig's own registry,
`ghcr.io/brig-sh/`. `claude-desktop` and `ubuntu` are both outside it.
`brig doctor claude-desktop` reports that image as `--`, informational,
whatever `BRIG_VERIFY` is set to. It does not simulate the refusal a
`require` run hits.

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

Brig delegates every boot to a runtime it does not ship, and none was there.
On macOS the cask depends on hull, so this usually means a from-source
install without hull on PATH. Full install instructions, both platforms, are
at [docs/install.md](install.md).

Check whether the runtime is there:

```bash
which hull        # macOS
which nerdctl     # Linux
```

Install it (`brew install --cask brig` brings hull along on macOS), or, if you
have a build somewhere off PATH, point Brig at it:

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

This is not the same failure as a missing runtime. Brig treats it as a
different mistake on purpose: `BRIG_RUNTIME` names something Brig does not
drive. `brig ls` and `brig info` fail this same way rather than reporting
no sandboxes, because reading a typo as "you have none" hides it.

Check what is set:

```bash
echo "$BRIG_RUNTIME"
```

Set it to `hull` or `nerdctl`, or unset it so Brig looks on PATH instead:

```bash
unset BRIG_RUNTIME
```

Confirm:

```bash
brig doctor
```

The `runtime` line reads `ok` again.

## assets missing at ...

Seen in `brig doctor`, not from a run:

```
!!  boot      assets missing at /Users/alex/.hull/assets
        run any agent once to fetch them, or set BRIG_BOOT_ASSETS to a directory that has them
```

Six of the eight built-in profiles boot an unmodified OCI image rather than
their own. They need a shared kernel and initrd, which Brig calls the boot
assets. Nothing has downloaded that bundle yet. This is normal before a first
boot and does not stop one: the next `brig run` on any of those profiles
fetches it.

Confirm:

```bash
brig doctor
```

The `boot` line reads `ok  boot      assets present at /Users/alex/.hull/assets`.

## The secret store could not be read during a run

```
brig: the mine sandbox needs gh-token from brig's secret store, which could
not be read: <cause>
```

This is not the same as a missing secret. The store itself did not answer,
so Brig cannot tell whether the value is there. The usual causes: a locked
keychain, a keyring daemon that is not running, or a permission Brig does
not have. It exits with code `6`, the same code a missing secret does,
because both stop the run for a credential it needed.

Read what the store itself says:

```bash
brig doctor
```

The `secrets` line's own finding names the failure. Fix what it names: open
a locked keychain, start a keyring that is not running, or grant a missing
permission, then run again.

Confirm:

```bash
brig doctor
```

The `secrets` line reads `ok  secrets   keychain reachable`, or names your
platform's own store.

## My credential did not arrive

First, ask Brig what it forwards, by name:

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
broken sandbox, so Brig refuses it. Resolve it on the host so the variable
holds the real token, then run again. A stored secret or a profile literal is
exempt from this check, because it was put there on purpose.

**It is empty, or it expired.** An unset or empty variable is skipped so it
cannot shadow a value baked into the image.

A stored credential that has expired is not withheld. Brig forwards it as it
is and warns before boot. Dropping it silently looks exactly
like an unexplained login failure with nothing to act on:

```
brig: the imported credential claude-credentials (claude-code) expired 3d ago.
brig: Renew it on the host, then: brig secret import claude-code
```

Renew the login on the host and import it again, as the second line says.
Renewing on the host alone does not help: a run reads Brig's stored copy, and
nothing re-reads the host until an import says so.

A secret you stored with `--from-command` prints a different second line,
naming that command instead of an import:

```
brig: the imported credential <name> (claude-code) expired 3d ago.
brig: Renew it, then store it again: brig secret import claude-code <name> --from-command '<command>'
```

Confirm:

```bash
brig info claude
```

The credential you fixed appears in what Brig reports it forwards, and the
warning above it is gone.

## The agent asked me to log in again after a stop

There is no error here. The agent shows its login screen on a sandbox you had
already logged into.

On `claude-code` and `claude-desktop`, the in-guest login lives in the
sandbox's memory. It is written to a memory-backed mount that never reaches
host disk. `brig stop` takes it with the microVM, and the next `brig run`
starts fresh. That is by design, and it is specific to those two profiles. The other
six mount the guest home from host disk, so a login written there survives a
stop.

To make a `claude-code` or `claude-desktop` login survive a stop, import the
one already on this Mac into Brig's own store, once:

```bash
brig secret import claude-code
```

After that Brig delivers the login on every command that reaches the
sandbox, so a stop no longer loses it. See
[Carry your host login in, once](authentication.md#2-carry-your-host-login-in-once).

Confirm:

```bash
brig stop claude
brig run claude
```

The agent starts already logged in instead of showing its login screen.

## cosign is not installed

```
brig: cannot verify image ghcr.io/brig-sh/claude-code-stock:latest: cosign is not
installed (`brew install cosign`). Booting it unchecked
```

That is `BRIG_VERIFY=warn`, the default. Brig checked the image against its
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
cosign refuses every image, Brig's own included.

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

Cosign prints its version instead of "command not found", and the next run under `BRIG_VERIFY=warn` or `require` checks the image instead of skipping it.

## The registry could not be reached to verify an image

```
brig: cannot reach the registry to verify image
ghcr.io/brig-sh/claude-code-stock:latest: <detail>. The copy on disk could
not be checked against what the registry serves
brig: Boot the cached copy unverified? [y/N]
```

Nothing has been checked either way. The registry that verifies the local
copy's signature was not reachable, and the copy can still be fine. The
usual cause is being offline, or a captive portal that answers every host
with its own page instead of the one you asked for.

Answering no aborts:

```
brig: aborted: the registry could not be reached, so the image could not be
verified. Try again with the registry reachable, or set BRIG_VERIFY=off to
boot the cached copy unchecked
```

Under `BRIG_VERIFY=require` there is no prompt: Brig refuses outright, with
the same exit code, `5`.

Reconnect and try again, or answer `y` at the prompt if you trust the copy on
disk enough to boot it once unverified.

Confirm:

```bash
brig run claude
```

Once the registry answers, the run reaches `signature verified` instead of
the prompt.

## The image failed to pull, or the architecture does not match

```
brig: could not start the sandbox: <runtime error>
```

The detail after the colon is the runtime's, not Brig's. It is a registry
Brig cannot reach, an image reference that does not exist, or a manifest
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

If the image is one Brig does not publish, such as `cursor`, Brig says so
before it reaches the registry. It does not fail with a 404. Build the
image yourself and pass `--image`.

Confirm:

```bash
brig run claude
```

The sandbox boots instead of failing at `could not start the sandbox`.

## The signature did not verify

```
brig: image ghcr.io/brig-sh/claude-code-stock:latest claims to be published by
brig-sh, but its signature DID NOT VERIFY: <detail>
brig: Boot it anyway? [y/N]
```

This is not the same as a check that did not run. cosign ran and the answer
was no. The image sits under Brig's own registry, and its signature does not
match the workflow that is meant to have built it. That combination has no
innocent reading, so Brig stops and asks. With no terminal to ask, it refuses:

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
you checked yourself is the deliberate way past it.

An image published by someone else warns rather than stopping: bring-your-own
images are supported. A failure under Brig's own registry is the one case
that stops.

Confirm:

```bash
brig run claude
```

A pull that now verifies reaches `signature verified` and boots, instead of
the DID NOT VERIFY prompt.

## The sandbox never became ready

```
brig: sandbox did not become ready; check 'brig logs claude (or the runtime's own, hull logs brig-claude-code)'
```

On Linux the second half names `nerdctl logs` instead.

The runtime reported the sandbox running, but the agent inside it never
answered. Those are two different moments: the microVM starts, and a few
seconds later the guest binds its listener. Brig waits for the second one and
gave up.

Read the log, which is what the message points at:

```bash
brig logs claude
```

That is `hull logs` underneath, and the message names that spelling too, for
a boot that never became a sandbox Brig can address by ref.

The guest's own errors are there, not in Brig's output. If the guest is only
slow rather than broken, give it longer with `BRIG_READY_TIMEOUT` (seconds,
default 30):

```bash
BRIG_READY_TIMEOUT=60 brig run claude
```

Confirm:

```bash
brig run claude
```

The run reaches the agent instead of "sandbox did not become ready".

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

## A required secret is missing

```
brig: missing secret "gh-token" needed by the mine sandbox -- create it
first with: brig secret create gh-token
```

A profile's secret declared `required: true` has no value in Brig's secret
store yet. This run exits with code `6`. None of the built-in profiles ship
a required secret, so this is your own profile's declaration (`brig agent
edit mine`). Two secrets missing at once list one line each instead of one
sentence.

Supply the value the message names:

```bash
brig secret create gh-token
```

A secret the profile marks importable names `brig secret import <profile>`
instead, to carry the value in from your host rather than typing it in.

Confirm:

```bash
brig info mine
```

The secret no longer shows as missing, and its name appears in the
`CREDENTIALS` row.

## The hvi hypervisor needs macOS 15 or newer

```
brig: the hvi hypervisor needs macOS 15 or newer (this is 14.5): its in-kernel
interrupt controller does not exist here. Set BRIG_HYPERVISOR=vz for this run,
or upgrade macOS
```

Six of the eight built-in profiles ask for the `hvi` hypervisor backend. It
uses Apple's in-kernel interrupt controller, the `hv_gic_*` calls that
arrived in macOS 15. Brig reads the macOS version before it asks the runtime
for anything. It refuses an `hvi` run on an older one rather than let the
boot fail further in with nothing to name.

For one run, use the `vz` backend, Virtualization.framework, instead:

```bash
BRIG_HYPERVISOR=vz brig run claude
```

For good, put `BRIG_HYPERVISOR=vz` in your shell profile, or upgrade to
macOS 15 or newer.

Confirm:

```bash
brig run claude
```

With `BRIG_HYPERVISOR=vz` set, or on macOS 15 or newer, the run reaches the
agent's prompt instead of refusing.

An older Brig did not check the version first, and left this to find another
way. Its boot log:

```
VMM started (PID 33351)
brig: sandbox did not become ready; check 'brig logs claude (or the runtime's
own, /opt/homebrew/bin/hull logs brig-claude-code)'
```

`VMM started (PID 33351)` is hull's own line, not Brig's. Reading the log
the message points at:

```bash
brig logs claude
```

```
dyld[33351]: missing symbol called
```

is the whole log, and the symbol is never named. That is what an unnamed
crash looked like before Brig added the refusal above.

## docker does not carry annotations through to the runtime

Linux only, and only for a profile that boots an unmodified image (six of the
eight built-in ones):

```
brig: could not start the sandbox: this profile boots an unmodified image,
which needs the kernel passed as an OCI annotation; docker does not carry
annotations through to the runtime. Use nerdctl, or point BRIG_RUNTIME_BIN at
it
```

Brig accepts Docker where it looks for nerdctl, and most of what it needs
works either way. Passing the kernel as an OCI annotation does not: Docker
drops it, so the guest starts with no kernel to boot. Brig refuses
rather than let that fail somewhere further from the cause.

Install nerdctl, or point Brig at one you already have:

```bash
BRIG_RUNTIME_BIN=/path/to/nerdctl brig run claude
```

Confirm:

```bash
brig doctor
```

The `runtime` line names `nerdctl`, not `docker`.

## The sandbox restarted when I ran sh

Three things trigger this: a stale share, a stale policy, or a session run
against a different project than it last used (see
[sessions.md](sessions.md)). Each recreates the sandbox rather than failing
it. All persistent state lives in the guest home on the host either way.
Any other session on that sandbox is disconnected when it restarts.

**A different guest home than the one remembered.**

```
brig: the running sandbox is not mounting /Users/alex/work -- its share went
stale (the directory was renamed or replaced, or the workspace changed).
Restarting it; any other session using this sandbox will be disconnected.
```

Brig compares the guest home the running sandbox has against the one this
command asked for. Passing an explicit `--home` (or `BRIG_WORKSPACE`) that
does not match what the sandbox already has trips this.

Check which guest home a sandbox is mounting, in the `WORKSPACE` column:

```bash
brig ls
```

If you did not mean to change it, drop the `--home` flag so `sh` addresses
the same session the sandbox already has.

**A network policy that no longer matches what is running.**

```
brig: this sandbox is running under a different network policy than the one
that applies now. Rules are fixed when a sandbox boots, so it is being
restarted; any other session using this sandbox will be disconnected.
```

Egress rules are fixed at boot. Attaching or detaching a policy after a
sandbox is already up changes nothing it can reach until the next restart.
The next `brig sh` or `brig run` on that session triggers one.

Check what a profile has bound, and whether Brig can enforce it:

```bash
brig policy check claude
```

**A different project than the one last used.**

```
brig: the running sandbox has /Users/alex/app mounted as its project and
this run names /Users/alex/other-app. A share cannot be attached to a live
sandbox, so it is being restarted; any other session using this sandbox
will be disconnected.
```

A project is a share too, fixed at boot the same as the guest home. Naming
a directory on the run line that differs from the one the session last used
trips this.

Check what a session last used:

```bash
brig info claude
```

The `PROJECT` row, when there is one, names it. Pass the same directory, or
none, to keep the sandbox up instead of restarting it.

Confirm:

```bash
brig sh claude
```

`brig ls` lists the ref as `running` again, and a second `brig sh` on it
does not print another restart warning.

## brew trust is not a command

```
Error: Unknown command: trust
```

This message is Homebrew's, not Brig's. `brew trust` needs a recent Homebrew,
and an older one does not have it.

Check your version and update:

```bash
brew --version
brew update
```

Confirm:

```bash
brew trust brig-sh/brig
```

The command runs, and you can carry on with the install.

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

Brig writes state files into the guest home from the host, as you. A
symlink where one of those files belongs points Brig at a host path the
sandbox itself cannot reach. Brig refuses rather than following it. It
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
