# Troubleshooting

This page is organised by what you saw on the terminal. Find the message,
read the likely cause, apply the fix, then run the command under Confirm to
prove it worked.

Some of these messages are Brig's own. Others come from the layer underneath:
the microVM runtime (`hull` on macOS, `nerdctl` on Linux), cosign, or
Homebrew. Where a message is not Brig's, the entry says so.

Run `brig doctor` first. It prints the build of `brig` you are running,
then checks the host, the hypervisor, the runtime and its version, the boot
assets, cosign, the profiles, the secret store and brigd, one line each.
Under a line marked `!!` it prints the fix. `brig doctor <agent>` checks
that agent's image too. `--json` prints the same report as one document,
for a script or a bug report.

## Exit codes

Every failure exits with one of a small, stable set of codes. The full table,
and what each one means, is at
[docs/cli.md#exit-codes](cli.md#exit-codes).

`brig rm` and `brig logs` on a ref with no sandbox exit `3` and name the ref
you typed. They do not pass the runtime's own "instance not found" back as a
general failure (`1`).

## What `brig doctor` does not catch

Only the `runtime` and `secrets` lines change the exit status of `brig
doctor`: a missing or broken runtime exits `4`, and a secret store that
will not open exits `6`, the codes a run exits with
([Exit codes](cli.md#exit-codes)). Every other line, a `!!` included,
prints its fix and leaves the exit status at `0`.

That includes verification. A `BRIG_VERIFY` value Brig does not recognize,
and `BRIG_VERIFY=require` with no cosign installed, both print `!!` with the
fix. `brig doctor` still exits `0`, and a real run stops on either. A script
that checks only the exit status misses both.

Check the setting yourself before you rely on it:

```bash
echo "$BRIG_VERIFY"
```

`require` also refuses any image outside Brig's own registry,
`ghcr.io/brig-sh/`. `claude-desktop` and `ubuntu` are both outside it.
`brig doctor claude-desktop` reports that image as `--`, informational,
whatever `BRIG_VERIFY` is set to. It does not report the refusal a
`require` run hits.

## No runtime found on PATH

```
brig: no runtime found on PATH: brig drives hull on macOS, and none was there.
See https://github.com/brig-sh/brig#macos, or point BRIG_RUNTIME_BIN at a build
```

On Linux the runtime is `nerdctl`, and the message names it:

```
brig: no runtime found on PATH: install nerdctl, or point BRIG_RUNTIME_BIN at one
```

Either way the exit code is `4`.

Brig boots every sandbox through a runtime, and found none on PATH. On macOS
the cask depends on hull, so this usually means a from-source install
without hull on PATH. [docs/install.md](install.md) has the install
instructions for both platforms.

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

`BRIG_RUNTIME` names a runtime Brig does not drive. The exit code is `4`.
With no runtime on PATH, `brig ls` reports no sandboxes and exits `0`. With
an unknown `BRIG_RUNTIME`, `brig ls` and `brig info` fail with this
message, so the typo is not hidden.

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

`brig doctor` prints this. A run does not:

```
  !!  boot      assets missing at /Users/alex/.hull/store/assets
          run any agent once to fetch them, or set BRIG_BOOT_ASSETS to a directory that has them
```

Six of the eight built-in profiles boot an unmodified OCI image, which
carries no kernel. They need a shared kernel and initrd, which Brig calls
the boot assets, and nothing has downloaded that bundle yet. This is normal
before a first boot and does not stop one: the next `brig run` on any of
those profiles fetches it. A run does not download into a directory
`BRIG_BOOT_ASSETS` names, and the fix line then says so.

Confirm:

```bash
brig doctor
```

The `boot` line reads `ok  boot      assets present at /Users/alex/.hull/store/assets`.

## The secret store could not be read during a run

```
brig: the brig-mine sandbox needs gh-token from brig's secret store, which
could not be read: <cause>
```

The store itself did not answer, so Brig cannot tell whether the value is
there. The usual causes are a locked keychain, a keyring daemon that is not
running, or a permission Brig does not have. The exit code is `6`, the same
as for a missing secret.

Read what the store itself says:

```bash
brig doctor
```

The `secrets` line names the failure. Unlock the keychain, start the
keyring daemon, or grant the missing permission, then run again.

Confirm:

```bash
brig doctor
```

The `secrets` line reads `ok  secrets   keychain reachable`, or names your
platform's own store.

## My credential did not arrive

Ask Brig what it forwards, by name:

```bash
brig info claude
```

That reports what reaches the guest and whether the guest will be
authenticated. If the variable you expected is not listed, one of these is
why.

**It is on the denylist.**

```
brig: not forwarding ANTHROPIC_API_KEY: it is on the claude-code denylist
  ↳ it outranks the subscription credential, and would move this sandbox onto metered billing without saying so
  → to forward it anyway:  BRIG_ALLOW_DENIED=1
```

Brig refuses by default a key that switches the sandbox from your
subscription onto metered billing. Set `BRIG_ALLOW_DENIED=1` to forward it
when you want metered billing.

**It looks like an unresolved reference.**

```
brig: not forwarding GH_TOKEN: it looks like an unresolved secret reference (op://...), not a credential
  → resolve it on the host before you run brig
  → to forward it as it is:  BRIG_ALLOW_REFS=1
```

A `scheme://` value is what tools like direnv leave in the environment when a
secret-manager reference was never resolved. Forwarded as it is, it produces
"Invalid username or token" inside the guest, which reads like a broken
sandbox, so Brig refuses it. Resolve it on the host so the variable holds
the real token, then run again. Brig does not apply this check to a stored
secret or a profile literal.

**It is empty, or it expired.** An unset or empty variable is skipped so it
cannot shadow a value baked into the image.

Brig does not withhold a stored credential that has expired. It forwards
the credential and warns before boot:

```
brig: the imported credential claude-credentials (claude-code) expired 3d ago
  → renew it on the host, then:  brig secret import claude-code
```

Renew the login on the host and import it again, as the second line says.
Renewing on the host alone does not help: a run reads Brig's stored copy,
and only an import reads the host again.

A secret you stored with `--from-command` prints a different second line,
naming that command instead of an import:

```
brig: the imported credential <name> (claude-code) expired 3d ago
  → renew it, then store it again:  brig secret import claude-code <name> --from-command '<command>'
```

Confirm:

```bash
brig info claude
```

The credential you fixed appears in what Brig reports it forwards, and the
warning above it is gone.

## The agent asked me to log in again after a stop

Brig prints no error. The agent shows its login screen on a sandbox you had
already logged into.

On `claude-code` and `claude-desktop`, the in-guest login lives in the
sandbox's memory. It is written to a memory-backed mount that never reaches
host disk. `brig stop` removes it with the microVM, and the next `brig run`
starts without it. Only those two profiles do this. The other six keep the
whole guest home on host disk, so a login written there survives a stop.

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
brig: cannot verify image ghcr.io/brig-sh/claude-code-stock:root: cosign is not installed (`brew install cosign`)
  ↳ booting it unchecked
```

That is `BRIG_VERIFY=warn`, the default. The image is under Brig's own
registry, Brig found no cosign to check its signature with, and it says so
before it boots the image unchecked. Nothing failed: the check did not run.

Under `BRIG_VERIFY=off` cosign is never looked up, and the only line is:

```
brig: BRIG_VERIFY=off, so the signature and digest checks are skipped: the
guest image and the kernel it boots are not checked
```

A profile that boots its own image has no kernel of Brig's to skip, and the
line names the image alone.

Under `BRIG_VERIFY=require` nothing boots, and the exit code is `5`:

```
brig: refusing to boot image ghcr.io/brig-sh/claude-code-stock:root: cosign is
not installed (`brew install cosign`), so nothing could be checked
(BRIG_VERIFY=require). Set BRIG_VERIFY=warn to boot it unchecked
```

`require` with no cosign refuses every image, Brig's own included.

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

cosign prints its version, and the next run under `BRIG_VERIFY=warn` or
`require` checks the image.

## The registry could not be reached to verify an image

```
brig: cannot reach the registry to verify image ghcr.io/brig-sh/claude-code-stock:root: <detail>
  ↳ the copy on disk could not be checked against what the registry serves
brig: Boot the cached copy unverified? [y/N]
```

Nothing was checked. Brig could not reach the registry it verifies the
local copy against, and the copy can still be fine. The usual cause is
being offline, or a captive portal that answers every host with its own
page.

Answering no aborts:

```
brig: aborted: the registry could not be reached, so the image could not be
verified. Try again with the registry reachable, or set BRIG_VERIFY=off to
boot the cached copy unchecked
```

Under `BRIG_VERIFY=require` there is no prompt: Brig refuses, with the same
exit code, `5`.

Reconnect and try again, or answer `y` at the prompt if you trust the copy on
disk enough to boot it once unverified.

Confirm:

```bash
brig run claude
```

Once the registry answers, the run prints `brig: image and boot assets
verified` instead of the prompt.

## cosign did not answer

```
brig: cannot verify image ghcr.io/brig-sh/claude-code-stock:root: cosign did
not answer within 30s. It waits on docker-credential-desktop, set by
credsStore in /Users/you/.docker/config.json. Start the app that helper
belongs to, or run brig with DOCKER_CONFIG set to an empty directory (and
restart brigd with it set, if brigd is running)
  ↳ the copy on disk was not checked against what the registry serves
brig: Boot the cached copy unverified? [y/N]
```

The usual cause is a Docker credential helper that never answers. cosign
reads Docker's `config.json` to find credentials for `ghcr.io`, and when
`credsStore` or `credHelpers` names a helper, cosign runs it and waits. With
`"credsStore": "desktop"` and Docker Desktop not running,
`docker-credential-desktop` blocks. The message names the helper and the
file only when one is set. Without one, it goes from "within 30s" straight
to "the copy on disk was not checked", and the causes in the section above
apply.

Brig gives cosign 30 seconds and then kills its process group, so a
credential helper it started dies with it. A helper that leaves the group
with `setsid` is not killed.

Answering no aborts, and the error repeats the detail:

```
brig: aborted: the image was not verified: cosign did not answer within 30s.
<detail>. Try again once cosign answers, or set BRIG_VERIFY=off to boot it
unchecked
```

Under `BRIG_VERIFY=require` there is no prompt: Brig refuses, with exit
code `5`.

The images Brig boots are public, so cosign needs no credentials for them.
Start Docker Desktop, or point `DOCKER_CONFIG` at an empty directory for the
run:

```bash
DOCKER_CONFIG="$(mktemp -d)" brig run claude
```

cosign inherits the environment of the process that runs it. If brigd runs
your boots, stop it and start it again with `DOCKER_CONFIG` set.

Confirm:

```bash
brig run claude
```

The run prints `brig: image and boot assets verified` instead of the prompt,
within a few seconds.

## The image failed to pull, or the architecture does not match

```
brig: could not start the sandbox: <runtime error>
```

The detail after the colon is the runtime's. The cause is a registry Brig
cannot reach, an image reference that does not exist, or a manifest with no
build for your architecture. The five published agents default to `:root`,
which `brig-sh/community-images` publishes as a multi-arch index covering
`linux/arm64` and `linux/amd64`, so one reference works on an Apple Silicon
Mac and on an x86 Linux host. A mismatch usually means a `--image` or
`BRIG_IMAGE` pinned to a single-architecture tag (`:arm64` or `:amd64`)
that is not yours.

Check the reference you are booting:

```bash
brig info claude      # shows the image, among other things
```

The runtime's error says which it is. For the wrong architecture, drop the
pinned tag or pin the one for your machine (`:arm64` or `:amd64`). If the
registry was unreachable, try again once you can reach it. Under the default
pull policy a republished tag is not pulled again until you ask for it:

```bash
BRIG_PULL=always brig run claude
```

If the image is one Brig does not publish, such as `cursor`, Brig says so
before it reaches the registry. Build the image yourself and pass `--image`.

Confirm:

```bash
brig run claude
```

The sandbox boots instead of failing at `could not start the sandbox`.

## The signature did not verify

```
brig: image ghcr.io/brig-sh/claude-code-stock:root claims to be published by
brig-sh, but its signature DID NOT VERIFY: <detail>
brig: Boot it anyway? [y/N]
```

cosign ran, and the check failed. The image is under Brig's own registry,
and its signature does not match the workflow that builds Brig's images.
Brig stops and asks. With no terminal to ask on, it refuses:

```
brig: not a terminal, so there is nobody to ask: refusing
  → to boot it regardless:  BRIG_VERIFY=off
```

Answering no aborts, with exit code `5`:

```
brig: aborted: the image failed verification. Pull it again (BRIG_PULL=always),
or set BRIG_IMAGE to a digest you have checked yourself
```

Under `BRIG_VERIFY=require` there is no prompt: Brig refuses, with the same
exit code.

The usual harmless cause is a stale local copy. Pull the image again with
`BRIG_PULL=always brig run claude`.

If it still fails and you do not know why, do not boot it. To boot an image
you checked yourself, set `BRIG_IMAGE` to its digest.

An image published by someone else gets a warning and boots, because Brig
supports bring-your-own images.

Confirm:

```bash
brig run claude
```

The run prints `brig: image and boot assets verified` and boots, with no
DID NOT VERIFY prompt.

## The boot assets are not the bundle that verified

```
brig: refusing to boot: the boot assets in ~/.hull/store/assets are not the
bundle that verified, ghcr.io/nofireai/hull-assets:darwin-arm64
(sha256:e82a...): container-initrd is sha256:55d2..., not the sha256:05cb...
it lists. Delete both files there and run again to fetch the bundle, or set
BRIG_BOOT_ASSETS to that directory if they are your own build
```

The kernel and initrd on disk are not the files the signed bundle lists.
They are not an older bundle either: when the record beside the files names
an older bundle, Brig fetches the verified bundle over it and says so. So a file
changed after the fetch, or a Brig or hull that kept no record fetched the
files. Brig refuses under `warn` as well as `require`, with exit `5`.

Delete the two files in the directory the message names and run again. Brig
fetches the bundle whose signature it checked, and compares the files with
it. If the files are your own build, point `BRIG_BOOT_ASSETS` at their
directory: `warn` then states the difference and boots them. See
[security.md](security.md#the-kernel-not-only-the-image).

Confirm:

```bash
brig run claude
```

The run prints `brig: image and boot assets verified`.

## The runtime bundle's kernel is not the one its record lists

```
brig: refusing to boot: the kernel and initrd in
/var/lib/brig/data/share/guest are not the ones the Linux runtime bundle's
signed record lists: bzImage is sha256:9c1e..., not the sha256:4f0a... it
lists. Re-run brig's install.sh to reinstall the bundle, or point
BRIG_BOOT_ASSETS at a directory of your own build
```

On Linux the runtime bundle carries the kernel and initrd, and its release
signs a record of their digests. A file in the bundle's `share/guest` that
differs from the record was changed after the install. Brig refuses under
`warn` as well as `require`, with exit `5`. The same refusal names a record the
release's `checksums.txt` does not list, or a `checksums.txt` whose signature
does not verify.

Run Brig's `install.sh` again to put the bundle's files back. If the kernel is
your own build, keep it in a directory of its own and point `BRIG_BOOT_ASSETS`
there. If the bundle was released from a fork, its signature names the fork's
release workflow: set `BRIG_VERIFY_RUNTIME_IDENTITY` to it. See
[security.md](security.md#the-kernel-not-only-the-image).

Confirm:

```bash
brig run claude
```

The run prints `brig: image and boot assets verified`.

## The sandbox never became ready

```
brig: sandbox did not become ready; check 'brig logs claude-code (or the runtime's own, /opt/homebrew/bin/hull logs brig-claude-code)'
```

On Linux the second half names `nerdctl logs`.

The runtime reported the sandbox running, but the agent inside it never
answered. The microVM starts first, and the guest binds its listener a few
seconds later. Brig waited for the listener and gave up.

Read the log the message names:

```bash
brig logs claude
```

That runs `hull logs`. The message names that command too, for a boot that
never became a sandbox Brig can address by ref.

The guest's own errors are in that log. If the guest is slow, give it
longer with `BRIG_READY_TIMEOUT` (seconds, default 30):

```bash
BRIG_READY_TIMEOUT=60 brig run claude
```

On Linux, a `genericBoot` profile also ends here when the host runs a urunc
release. No release reads the boot annotations Brig passes, and `brig doctor`
still reports the runtime and the boot assets as `ok`. Such a host brought
its own urunc, either with `BRIG_INSTALL_RUNTIME=0` or after an `install.sh`
from Brig 0.2.0 or earlier, which installed no runtime on Linux. Install the
runtime bundle, which carries a urunc that reads them
([runtimes.md](runtimes.md#what-brig-requires-of-each)):

```bash
curl -fsSL https://brig.sh/install | sh
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
removed. The exit code is `4`. A bare `brig doctor` does not catch this: it
reads no profile's `runtimeBin`, so its `runtime` line can read `ok`.
`brig doctor mine` checks that profile's binary.

Open the profile and fix or remove the line:

```bash
brig agent edit mine
```

Confirm:

```bash
brig doctor mine
```

The `runtime` line reads `ok` and names the binary.

## A required secret is missing

```
brig: missing secret "gh-token" needed by the brig-mine sandbox -- create it
first with: brig secret create gh-token
```

A secret the profile declares `required: true` has no value in Brig's secret
store. The exit code is `6`. No built-in profile declares a required secret,
so the declaration is in your own profile (`brig agent edit mine`). When
several secrets are missing, the message lists one line for each.

Store the value with the command the message names:

```bash
brig secret create gh-token
```

For a secret the profile marks importable, the message names `brig secret
import <profile>`, which carries the value in from your host.

Confirm:

```bash
brig info mine
```

The secret no longer shows as missing, and its name appears in the
`CREDENTIALS` row.

## The hvi hypervisor needs macOS 15 or newer

```
brig: the hvi hypervisor needs macOS 15 or newer (this is 14.5): its in-kernel
interrupt controller does not exist here. Set BRIG_HYPERVISOR=vz BRIG_NETWORK=shared
for this run, or upgrade macOS
```

Six of the eight built-in profiles ask for the `hvi` hypervisor backend. It
uses Apple's in-kernel interrupt controller, the `hv_gic_*` calls that
arrived in macOS 15. Brig reads the macOS version before it asks the runtime
for anything, and refuses an `hvi` run on an older one.

For one run, use the `vz` backend (Virtualization.framework):

```bash
BRIG_HYPERVISOR=vz brig run claude --network shared
```

The built-in `hvi` profiles also name `network: isolated`. `vz` cannot
provide it, so this command explicitly chooses a shared network, where
sandboxes are not promised separation. For later runs, put both
`BRIG_HYPERVISOR=vz` and `BRIG_NETWORK=shared` in your shell profile, or
upgrade to macOS 15 or newer and keep `hvi`.

Confirm:

```bash
brig run claude
```

With both settings applied, or on macOS 15 or newer using `hvi`, the run
reaches the agent's prompt instead of refusing.

An older Brig did not check the version first. Its boot failed like this:

```
VMM started (PID 33351)
brig: sandbox did not become ready; check 'brig logs claude (or the runtime's
own, /opt/homebrew/bin/hull logs brig-claude-code)'
```

`VMM started (PID 33351)` is hull's own line. The log that message names
held one line, `dyld[33351]: missing symbol called`, which does not name
the symbol. Upgrade Brig to get the refusal above.

## A policy or an isolated network was refused on vz or qemu

```
brig: a policy applies to this sandbox, and hull on vz cannot enforce the egress
policy: vz takes its network from vmnet, which brig does not filter. brig
enforces a policy at the user-mode network gateway that only the hvi backend
uses. Run it on hvi (BRIG_HYPERVISOR=hvi), or detach the policy. brig will not
boot a sandbox under a policy nothing enforces
```

Brig enforces an egress policy at the network gateway of the `hvi` backend.
On `vz` and `qemu` the sandbox takes its network from vmnet, which Brig does
not filter, so Brig refuses the run. The exit code is `7`. On Linux, nerdctl
and docker cannot enforce a policy either, and the message says to detach
it.

An isolated network needs the same gateway. With no policy bound, its
refusal exits `1`:

```
brig: --network isolated gives the sandbox a network of its own, which brig can
only do on the hvi backend, where it owns the gateway (BRIG_HYPERVISOR is "vz");
vmnet decides what a vz sandbox shares. Run it on hvi, or run sandboxes that
must not reach each other on separate hosts (see docs/security.md)
```

When the profile's `network: isolated` asked for it, the message names the
profile and the way to run it on the shared network.

Run it on `hvi`. To stay on `vz` or `qemu`, detach the policy and pass
`--network shared`. `brig policy check` names what is bound:

```bash
BRIG_HYPERVISOR=hvi brig run claude
brig policy check claude-code
```

Confirm:

```bash
BRIG_HYPERVISOR=hvi brig plan claude
```

The plan has no `REFUSED` row.

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
works on both. Passing the kernel as an OCI annotation does not: Docker
drops it, so the guest would start with no kernel to boot. Brig refuses
before the boot.

Install nerdctl, or point Brig at one you already have:

```bash
BRIG_RUNTIME_BIN=/path/to/nerdctl brig run claude
```

Confirm:

```bash
brig doctor
```

The `runtime` line names `nerdctl`.

## A containerd shim that shares the host kernel was refused

Linux only:

```
brig: refusing BRIG_CONTAINERD_RUNTIME=runc: that shim gives the guest the
host's own kernel, which is not the boundary brig provides. Unset
BRIG_CONTAINERD_RUNTIME to use the default microVM shim io.containerd.urunc.v2.
A sandbox already started on such a shim keeps it, and the next run would join
it by name: stop it first with brig stop <ref>
```

`BRIG_CONTAINERD_RUNTIME` names `runc` or `crun`, by name or by path. Those
shims run the guest as a plain container on the host's kernel. Brig puts a
kernel of the guest's own between the agent and the host, so it refuses the
run. The exit code is `1`. A shim Brig cannot place, such as another microVM
shim, is allowed.

Unset the variable. If an older Brig started this sandbox on such a shim,
stop it first:

```bash
unset BRIG_CONTAINERD_RUNTIME
brig stop claude
```

Confirm:

```bash
brig plan claude
```

The plan has no `REFUSED` row, and its `ISOLATION` row names
`io.containerd.urunc.v2`.

## The sandbox restarted when I ran sh

Four things cause this: a stale share, a stale policy, a different network
posture, or a different project than the session last used (see
[sessions.md](sessions.md)). In each case Brig warns and recreates the
sandbox. The guest home on the host is kept. Any other session on that
sandbox is disconnected when it restarts.

**A different guest home than the one remembered.**

```
brig: the running sandbox is not mounting /Users/alex/work: its share went stale
  ↳ the directory was renamed or replaced, or the workspace changed
  ↳ brig restarts it, and any other session using this sandbox will be disconnected
```

Brig compares the guest home the running sandbox mounts with the one this
command asked for. A `--home` (or `BRIG_WORKSPACE`) that differs from the
one the sandbox mounts causes the restart.

The `WORKSPACE` column shows which guest home a sandbox mounts:

```bash
brig ls
```

If you did not mean to change it, drop the `--home` flag.

**A network policy that no longer matches what is running.**

```
brig: this sandbox is running under a different network policy than the one that applies now
  ↳ rules are fixed when a sandbox boots, so brig restarts it
  ↳ any other session using this sandbox will be disconnected
```

Egress rules are fixed at boot. Attaching or detaching a policy while a
sandbox is up changes nothing it can reach until the next restart. The next
`brig sh` or `brig run` on that session restarts it.

Check what a profile has bound, and whether Brig can enforce it:

```bash
brig policy check claude
```

**A different posture than the one the sandbox was started with.**

```
brig: this sandbox was started with the isolated posture and --network asks for shared
  ↳ rules are fixed when a sandbox boots, so brig restarts it
  ↳ any other session using this sandbox will be disconnected
```

A sandbox keeps the posture it was started with, so a command that names
no posture never causes this. `--network` or `BRIG_NETWORK` naming a
different one does. If you did not mean to change it, check whether
`BRIG_NETWORK` is exported in this shell.

The warning names the posture the sandbox runs with. A sandbox that a
policy isolated is named `isolated`, even after the policy is detached.

**A different project than the one last used.**

```
brig: the running sandbox has /Users/alex/app mounted as its project and this run names /Users/alex/other-app
  ↳ a share cannot be attached to a live sandbox, so brig restarts it
  ↳ any other session using this sandbox will be disconnected
```

A project is a share too, fixed at boot like the guest home. Naming a
directory on the run line that differs from the one the session last used
causes the restart.

Check what a session last used:

```bash
brig info claude
```

The `PROJECT` row, when there is one, names it. Pass the same directory, or
none, to keep the sandbox up.

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

This message is Homebrew's. `brew trust` needs a recent Homebrew.

Check your version and update:

```bash
brew --version
brew update
```

Confirm:

```bash
brew trust brig-sh/brig
```

The command runs, and you can continue the install.

## A symlinked or moved guest home was refused

```
brig: refusing to write /Users/alex/.brig/homes/brig-claude-code/.claude.json:
it is a symlink to "/Users/alex/.ssh/authorized_keys", and brig writes only
regular files inside the workspace. The workspace is mounted read-write as the
sandbox's home, so that link was put there from inside the sandbox, to have brig
-- which runs as you, on the host -- reach a file the sandbox cannot. Nothing
was written; inspect /Users/alex/.brig/homes/brig-claude-code/.claude.json and
remove it before running brig again: a symlink leads out of a directory brig is
checking
```

Brig writes state files into the guest home from the host, as you. A
symlink where one of those files belongs points Brig at a host path the
sandbox cannot reach. Brig refuses to follow it. Brig writes only regular
files there, so it did not create the link: you did, or the sandbox did.
Nothing was written.

Retrying gives the same refusal. Inspect the path the message names and
remove the link, or point the guest home somewhere else. Brig refuses a
`--home` that is a symlink the same way: name the real directory.

Confirm:

```bash
brig run claude
```

The run reaches the agent instead of refusing.
[docs/security.md](security.md#writing-into-the-workspace) explains why the
refusal exists.

## A project reached through a symlink was refused

```
brig: refusing to use /Users/alex/work/app as this run's project:
/Users/alex/work on the way to it is a symlink to "/Volumes/data/work", so the
sandbox would be handed a directory other than the one you named. Name the
real directory instead: a symlink leads out of a directory brig is checking
```

The project is mounted read-write, so the sandbox can replace any directory
at or below it. A link planted there on one run would have the next run hand
the runtime a directory the agent picked. Brig cannot tell that link from one
you made yourself, so it refuses both and names where the link points. A link
in a directory you cannot write, such as `/tmp` on macOS, is still followed.

Name the real directory:

```bash
brig run claude /Volumes/data/work/app
```

When the message ends with "This session's project was remembered from an
earlier run", the project came from an earlier `brig run` of this session.
`brig run` with the real directory after the ref, or with `--no-project`,
replaces it. Until then Brig refuses only the verbs that boot or join the
sandbox. `brig stop`, `brig rm` and `brig info` still work, and `brig info`
repeats the refusal.

Confirm:

```bash
brig info claude
```

The PROJECT row names the real directory, and the refusal is gone.
[docs/security.md](security.md#mounting-a-project) explains why the refusal
exists.
