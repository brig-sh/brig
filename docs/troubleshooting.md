# Troubleshooting

Find the message that you saw on the terminal. Each entry gives the cause,
the fix and the command that confirms the fix.

Some messages come from Brig. Others come from the microVM runtime (`hull`
on macOS, `nerdctl` on Linux), from cosign or from Homebrew. An entry says
so when its message does not come from Brig.

Run `brig doctor` first. It prints the build of `brig` that you run. Then it
checks eight items, one line each: the host, the hypervisor, the runtime and
its version, the boot assets, cosign, the profiles, the secret store and
brigd. Under a line marked `!!` it prints the fix. `brig doctor <agent>`
checks the image of that agent too. `--json` prints the same report as one
document, for a script or a bug report.

## Exit codes

Every failure exits with one code from a small, stable set. See
[Exit codes](cli.md#exit-codes) for the full table.

`brig rm` and `brig logs` on a ref with no sandbox exit `3` and name the ref
that you typed. They do not return the runtime message "instance not found"
as a general failure (`1`).

## Limits of `brig doctor`

Only two lines change the exit status of `brig doctor`. A run exits with
the same two codes ([Exit codes](cli.md#exit-codes)).

| Line | Exit status |
|---|---|
| `runtime`, with a missing or broken runtime | `4` |
| `secrets`, with a secret store that does not open | `6` |
| Every other line, a `!!` line included | `0`. The line prints its fix. |

In these two verification cases, `brig doctor` prints `!!` with the fix and
exits `0`. A real run stops on each:

- `BRIG_VERIFY` has a value that Brig does not recognize.
- `BRIG_VERIFY=require` is set and cosign is not installed.

A script that reads only the exit status misses both cases. Before you rely
on the setting, read it:

```bash
echo "$BRIG_VERIFY"
```

`require` also refuses every image outside the Brig registry,
`ghcr.io/brig-sh/`. `claude-desktop` and `ubuntu` are both outside it.
`brig doctor claude-desktop` reports that image as `--` (informational) for
every value of `BRIG_VERIFY`. It does not report the refusal that a
`require` run gets.

## No runtime found on PATH

```
brig: no runtime found on PATH: brig drives hull on macOS, and none was there.
See https://github.com/brig-sh/brig#macos, or point BRIG_RUNTIME_BIN at a build
```

On Linux the runtime is `nerdctl`, and the message names it:

```
brig: no runtime found on PATH: install nerdctl, or point BRIG_RUNTIME_BIN at one
```

**Exit code:** `4` on both platforms.

**Cause:** Brig boots every sandbox through a runtime, and found no runtime
on PATH. On macOS the cask depends on hull, so the usual cause is a
from-source install without hull on PATH.

**Fix:**

1. Look for the runtime:

   ```bash
   which hull        # macOS
   which nerdctl     # Linux
   ```

2. If the runtime is missing, install it. On macOS,
   `brew install --cask brig` installs hull too. See [Install Brig](install.md)
   for both platforms.
3. If you have a build that is not on PATH, point Brig at it:

   ```bash
   BRIG_RUNTIME_BIN=/path/to/hull brig run claude
   ```

**Confirm:**

```bash
brig doctor
```

The `runtime` line reads `ok` and names the binary that it found.

## unknown BRIG_RUNTIME

```
brig: runtime unavailable: unknown BRIG_RUNTIME "podman" (want hull or nerdctl)
```

**Exit code:** `4`

**Cause:** `BRIG_RUNTIME` names a runtime that Brig does not drive. `brig ls`
and `brig info` fail with this message too, so a typo stays visible.

**Fix:**

1. Read the value:

   ```bash
   echo "$BRIG_RUNTIME"
   ```

2. Set it to `hull` or `nerdctl`. Or unset it, and Brig looks on PATH:

   ```bash
   unset BRIG_RUNTIME
   ```

**Confirm:**

```bash
brig doctor
```

The `runtime` line reads `ok` again.

With no runtime on PATH, `brig ls` reports no sandboxes and exits `0`.

## assets missing at ...

`brig doctor` prints this message. A run does not print it.

```
  !!  boot      assets missing at /Users/alex/.hull/store/assets
          run any agent once to fetch them, or set BRIG_BOOT_ASSETS to a directory that has them
```

**Cause:** Six of the eight built-in profiles boot an unmodified OCI image,
which carries no kernel. They need a shared kernel and initrd (the boot
assets), and nothing downloaded that bundle yet.

**Fix:** None is necessary. This state is normal before a first boot and
does not stop a boot. The next `brig run` on any of those profiles fetches
the bundle.

A run does not download into a directory that `BRIG_BOOT_ASSETS` names. The
fix line then says so.

**Confirm:**

```bash
brig doctor
```

The `boot` line reads `ok  boot      assets present at /Users/alex/.hull/store/assets`.

## Secret store did not answer

```
brig: the brig-mine sandbox needs gh-token from brig's secret store, which
could not be read: <cause>
```

**Exit code:** `6`, the same as for a missing secret.

**Cause:** The secret store did not answer, so Brig cannot tell whether the
value is there. The usual causes are a locked keychain, a keyring daemon that is
not running, or a permission that Brig does not have.

**Fix:**

1. Run `brig doctor`. The `secrets` line names the failure.
2. Unlock the keychain, start the keyring daemon, or grant the missing
   permission.
3. Run the command again.

**Confirm:**

```bash
brig doctor
```

The `secrets` line reads `ok  secrets   keychain reachable`, or names the
store of your platform.

## My credential did not arrive

Ask Brig what it forwards, by name:

```bash
brig info claude
```

The command reports what reaches the guest and whether the guest will be
authenticated. If the variable that you expected is not listed, one of the
four cases below is the cause.

### On the denylist

```
brig: not forwarding ANTHROPIC_API_KEY: it is on the claude-code denylist
  ↳ it outranks the subscription credential, and would move this sandbox onto metered billing without saying so
  → to forward it anyway:  BRIG_ALLOW_DENIED=1
```

**Cause:** By default Brig refuses a key that moves the sandbox from your
subscription to metered billing.

**Fix:** If you want metered billing, set `BRIG_ALLOW_DENIED=1` to forward
the key.

### Unresolved secret reference

```
brig: not forwarding GH_TOKEN: it looks like an unresolved secret reference (op://...), not a credential
  → resolve it on the host before you run brig
  → to forward it as it is:  BRIG_ALLOW_REFS=1
```

**Cause:** Tools such as direnv leave a `scheme://` value in the environment
when a secret-manager reference was never resolved. Forwarded as it is, the
value produces "Invalid username or token" inside the guest, which reads
like a broken sandbox, so Brig refuses it.

**Fix:** Resolve the reference on the host so that the variable holds the
real token. Then run again.

Brig does not apply this check to a stored secret or a profile literal.

### Empty variable

**Cause:** Brig skips an unset or empty variable, so that it cannot shadow a
value baked into the image.

### Expired credential

Brig forwards a stored credential that expired, and warns before boot:

```
brig: the imported credential claude-credentials (claude-code) expired 3d ago
  → renew it on the host, then:  brig secret import claude-code
```

**Fix:**

1. Renew the login on the host.
2. Import the login again, with the command in the second line.

A renewal on the host alone has no effect. A run reads the copy that Brig
stored, and only an import reads the host again.

For a secret that you stored with `--from-command`, the second line names
that command instead of an import:

```
brig: the imported credential <name> (claude-code) expired 3d ago
  → renew it, then store it again:  brig secret import claude-code <name> --from-command '<command>'
```

### Confirm the credential

```bash
brig info claude
```

The credential that you fixed appears in the list that Brig forwards, and
the warning is gone.

## Login lost after a stop

Brig prints no error. The agent shows its login screen on a sandbox that you
already logged in to.

**Cause:** Only `claude-code` and `claude-desktop` keep the in-guest login
in a memory-backed mount that never reaches host disk, and `brig stop`
removes it with the microVM. The other six profiles keep the whole guest
home on host disk, so a login written there survives a stop.

**Fix:** Import the login that is already on this Mac into the Brig secret
store, one time:

```bash
brig secret import claude-code
```

Then Brig delivers the login on every command that reaches the sandbox. See
[Carry your host login in, once](authentication.md#2-carry-your-host-login-in-once).

**Confirm:**

```bash
brig stop claude
brig run claude
```

The agent starts logged in and does not show its login screen.

## cosign is not installed

```
brig: cannot verify image ghcr.io/brig-sh/claude-code-stock:root: cosign is not installed (`brew install cosign`)
  ↳ booting it unchecked
```

The result depends on `BRIG_VERIFY`:

| `BRIG_VERIFY` | Result |
|---|---|
| `warn` (the default) | Brig prints the message above, then boots the image unchecked. Nothing failed: the check did not run. |
| `off` | Brig does not look for cosign. It prints one line. |
| `require` | Nothing boots. The exit code is `5`. Brig refuses every image, Brig images included. |

The line under `BRIG_VERIFY=off`:

```
brig: BRIG_VERIFY=off, so the signature and digest checks are skipped: the
guest image and the kernel it boots are not checked
```

For a profile that boots its own image, there is no Brig kernel to skip, and
the line names only the image.

The refusal under `BRIG_VERIFY=require`:

```
brig: refusing to boot image ghcr.io/brig-sh/claude-code-stock:root: cosign is
not installed (`brew install cosign`), so nothing could be checked
(BRIG_VERIFY=require). Set BRIG_VERIFY=warn to boot it unchecked
```

**Cause:** The image is under the Brig registry, and Brig found no cosign to
check its signature with.

**Fix:** Install cosign to get the check:

```bash
brew install cosign         # macOS
```

On Linux, no package covers every distro. Install a release from
[sigstore/cosign](https://github.com/sigstore/cosign) and put it on PATH.

To boot without the check and stop the warning, set `BRIG_VERIFY=off`. It is
safer to keep cosign installed.

**Confirm:**

```bash
cosign version
```

cosign prints its version, and the next run under `BRIG_VERIFY=warn` or
`require` checks the image.

## Cannot reach the registry

```
brig: cannot reach the registry to verify image ghcr.io/brig-sh/claude-code-stock:root: <detail>
  ↳ the copy on disk could not be checked against what the registry serves
brig: Boot the cached copy unverified? [y/N]
```

| Case | Result |
|---|---|
| You answer no | The run aborts with the message below. |
| `BRIG_VERIFY=require` | Brig does not prompt. It refuses, with the same exit code, `5`. |

```
brig: aborted: the registry could not be reached, so the image could not be
verified. Try again with the registry reachable, or set BRIG_VERIFY=off to
boot the cached copy unchecked
```

**Cause:** Brig did not reach the registry that it verifies the local copy
against, so nothing was checked. The usual cause is an offline host, or a
captive portal that answers every host with its own page.

**Fix:** Reconnect and run again. The copy can still be good. If you trust
the copy on disk enough to boot it one time unverified, answer `y` at the
prompt.

**Confirm:**

```bash
brig run claude
```

When the registry answers, the run prints
`brig: image and boot assets verified` and no prompt.

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

| Case | Result |
|---|---|
| You answer no | The run aborts, and the error below repeats the detail. |
| `BRIG_VERIFY=require` | Brig does not prompt. It refuses, with exit code `5`. |

```
brig: aborted: the image was not verified: cosign did not answer within 30s.
<detail>. Try again once cosign answers, or set BRIG_VERIFY=off to boot it
unchecked
```

**Cause:** The usual cause is a Docker credential helper that never answers.
cosign reads the Docker `config.json` to find credentials for `ghcr.io`,
runs the helper that `credsStore` or `credHelpers` names, and waits.

**Fix:** Start Docker Desktop, or point `DOCKER_CONFIG` at an empty
directory for the run. The images that Brig boots are public, so cosign
needs no credentials for them.

```bash
DOCKER_CONFIG="$(mktemp -d)" brig run claude
```

cosign inherits the environment of the process that runs it. If brigd runs
your boots, stop brigd and start it again with `DOCKER_CONFIG` set.

- With `"credsStore": "desktop"` and Docker Desktop not running,
  `docker-credential-desktop` blocks.
- The message names the helper and the file only when a helper is set.
  Without one, the message goes from "within 30s" directly to "the copy on
  disk was not checked". Then the causes in
  [Cannot reach the registry](#cannot-reach-the-registry) apply.
- Brig gives cosign 30 seconds and then kills its process group, so a
  credential helper that cosign started stops with it. A helper that leaves
  the group with `setsid` is not killed.

**Confirm:**

```bash
brig run claude
```

In a few seconds the run prints `brig: image and boot assets verified` and
no prompt.

## Image pull or architecture failure

```
brig: could not start the sandbox: <runtime error>
```

The detail after the colon comes from the runtime, and says which cause
applies.

**Cause:** Brig cannot reach the registry, the image reference does not
exist, or the manifest has no build for your architecture. An architecture
mismatch usually means that `--image` or `BRIG_IMAGE` pins a
single-architecture tag (`:arm64` or `:amd64`) that is not yours.

**Fix:** Read the reference that you boot:

```bash
brig info claude      # shows the image, among other things
```

| Cause | Fix |
|---|---|
| Wrong architecture | Remove the pinned tag, or pin the tag for your machine (`:arm64` or `:amd64`). The five published agents default to `:root`, which `brig-sh/community-images` publishes as a multi-arch index for `linux/arm64` and `linux/amd64`. It works on an Apple Silicon Mac and on an x86 Linux host. |
| Registry not reachable | Run again when you can reach the registry. |
| An image that Brig does not publish, such as `cursor` | Brig says so before it reaches the registry. Build the image yourself and pass `--image`. |

Under the default pull policy, Brig does not pull a republished tag again
until you ask for it:

```bash
BRIG_PULL=always brig run claude
```

**Confirm:**

```bash
brig run claude
```

The sandbox boots and does not fail at `could not start the sandbox`.

## The signature did not verify

```
brig: image ghcr.io/brig-sh/claude-code-stock:root claims to be published by
brig-sh, but its signature DID NOT VERIFY: <detail>
brig: Boot it anyway? [y/N]
```

Brig stops and asks.

| Case | Result |
|---|---|
| No terminal to ask on | Brig refuses, with the first message below. |
| You answer no | The run aborts with exit code `5`, with the second message below. |
| `BRIG_VERIFY=require` | Brig does not prompt. It refuses, with the same exit code. |
| An image that someone else published | Brig warns and boots it, because Brig supports bring-your-own images. |

```
brig: not a terminal, so there is nobody to ask: refusing
  → to boot it regardless:  BRIG_VERIFY=off
```

```
brig: aborted: the image failed verification. Pull it again (BRIG_PULL=always),
or set BRIG_IMAGE to a digest you have checked yourself
```

**Cause:** cosign ran, and the check failed. The image is under the Brig
registry, and its signature does not match the workflow that builds Brig
images.

**Fix:**

1. Pull the image again with `BRIG_PULL=always brig run claude`. The usual
   harmless cause is a stale local copy.
2. If the check still fails and you do not know why, do not boot the image.
3. To boot an image that you checked yourself, set `BRIG_IMAGE` to its
   digest.

**Confirm:**

```bash
brig run claude
```

The run prints `brig: image and boot assets verified` and boots, with no
DID NOT VERIFY prompt.

## Boot assets do not match

```
brig: refusing to boot: the boot assets in ~/.hull/store/assets are not the
bundle that verified, ghcr.io/nofireai/hull-assets:darwin-arm64
(sha256:e82a...): container-initrd is sha256:55d2..., not the sha256:05cb...
it lists. Delete both files there and run again to fetch the bundle, or set
BRIG_BOOT_ASSETS to that directory if they are your own build
```

**Exit code:** `5`. Brig refuses under `warn` as well as `require`.

**Cause:** The kernel and initrd on disk are not the files that the signed
bundle lists. Either a file changed after the fetch, or a Brig or hull that
kept no record fetched the files.

**Fix:**

| Case | Fix |
|---|---|
| Default | Delete the two files in the directory that the message names. Then run again. Brig fetches the bundle whose signature it checked, and compares the files with it. |
| The files are your own build | Point `BRIG_BOOT_ASSETS` at their directory. `warn` then states the difference and boots them. |

An older bundle does not cause this refusal. When the record beside the
files names an older bundle, Brig fetches the verified bundle over it and
says so. See
[The kernel, not only the image](security.md#the-kernel-not-only-the-image).

**Confirm:**

```bash
brig run claude
```

The run prints `brig: image and boot assets verified`.

## Bundle kernel does not match

```
brig: refusing to boot: the kernel and initrd in
/var/lib/brig/data/share/guest are not the ones the Linux runtime bundle's
signed record lists: bzImage is sha256:9c1e..., not the sha256:4f0a... it
lists. Re-run brig's install.sh to reinstall the bundle, or point
BRIG_BOOT_ASSETS at a directory of your own build
```

**Exit code:** `5`. Brig refuses under `warn` as well as `require`.

**Cause:** On Linux the runtime bundle carries the kernel and initrd, and
its release signs a record of their digests. A file in the `share/guest`
directory of the bundle that differs from the record changed after the
install.

**Fix:**

| Case | Fix |
|---|---|
| Default | Run the Brig `install.sh` again to put the bundle files back. |
| The kernel is your own build | Keep it in a separate directory and point `BRIG_BOOT_ASSETS` there. |
| The bundle was released from a fork | Its signature names the release workflow of the fork. Set `BRIG_VERIFY_RUNTIME_IDENTITY` to that workflow. |

The same refusal names a record that the `checksums.txt` of the release does
not list, or a `checksums.txt` whose signature does not verify. See
[The kernel, not only the image](security.md#the-kernel-not-only-the-image).

**Confirm:**

```bash
brig run claude
```

The run prints `brig: image and boot assets verified`.

## The sandbox never became ready

```
brig: sandbox did not become ready; check 'brig logs claude-code (or the runtime's own, /opt/homebrew/bin/hull logs brig-claude-code)'
```

On Linux the second half names `nerdctl logs`.

**Cause:** The runtime reported the sandbox as running, but the agent inside
it never answered. The guest binds its listener a few seconds after the
microVM starts, and Brig stopped waiting for that listener.

**Fix:**

1. Read the log that the message names. The errors of the guest are in that
   log.

   ```bash
   brig logs claude
   ```

   This command runs `hull logs`. The message names `hull logs` too, for a
   boot that never became a sandbox that Brig can address by ref.
2. If the guest is slow, give it more time with `BRIG_READY_TIMEOUT`
   (seconds, default 30):

   ```bash
   BRIG_READY_TIMEOUT=60 brig run claude
   ```

<details><summary>Linux: a host that runs a urunc release</summary>

On Linux, a `genericBoot` profile also ends here when the host runs a urunc
release. No release reads the boot annotations that Brig passes. `brig doctor`
still reports the runtime and the boot assets as `ok`.

Such a host brought its own urunc in one of two ways:

- with `BRIG_INSTALL_RUNTIME=0`
- after an `install.sh` from Brig 0.2.0 or earlier, which installed no
  runtime on Linux

Install the runtime bundle, which carries a urunc that reads the
annotations. See
[What Brig requires of each](runtimes.md#what-brig-requires-of-each).

```bash
curl -fsSL https://brig.sh/install | sh
```

</details>

**Confirm:**

```bash
brig run claude
```

The run reaches the agent and does not print "sandbox did not become ready".

## runtimeBin is not there

```
brig: runtime unavailable: this profile's runtimeBin is /old/path/hull, which is
not there: stat /old/path/hull: no such file or directory
```

**Exit code:** `4`

**Cause:** The `runtimeBin:` field of your own profile names a binary that
moved or was removed.

**Fix:** Open the profile, then correct or remove the line:

```bash
brig agent edit mine
```

**Confirm:**

```bash
brig doctor mine
```

The `runtime` line reads `ok` and names the binary.

A bare `brig doctor` does not find this error. It reads the `runtimeBin` of
no profile, so its `runtime` line can read `ok`. `brig doctor mine` checks
the binary of that profile.

## A required secret is missing

```
brig: missing secret "gh-token" needed by the brig-mine sandbox -- create it
first with: brig secret create gh-token
```

When several secrets are missing, the message lists one line for each.

**Exit code:** `6`

**Cause:** A secret that the profile declares `required: true` has no value
in the Brig secret store. No built-in profile declares a required secret, so
the declaration is in your own profile (`brig agent edit mine`).

**Fix:** Store the value with the command that the message names:

```bash
brig secret create gh-token
```

For a secret that the profile marks importable, the message names
`brig secret import <profile>`, which brings the value in from your host.

**Confirm:**

```bash
brig info mine
```

The secret does not show as missing, and its name appears in the
`CREDENTIALS` row.

## hvi needs macOS 15

```
brig: the hvi hypervisor needs macOS 15 or newer (this is 14.5): its in-kernel
interrupt controller does not exist here. Set BRIG_HYPERVISOR=vz BRIG_NETWORK=shared
for this run, or upgrade macOS
```

**Cause:** Six of the eight built-in profiles ask for the `hvi` hypervisor
backend. It uses Apple's in-kernel interrupt controller, the `hv_gic_*`
calls, which arrived in macOS 15. Brig reads the macOS version before
it asks the runtime for anything, and refuses an `hvi` run on an older
version.

**Fix:** Upgrade to macOS 15 or newer and keep `hvi`.

To stay on the older macOS, use the `vz` backend (Virtualization.framework).
For one run:

```bash
BRIG_HYPERVISOR=vz brig run claude --network shared
```

The built-in `hvi` profiles also name `network: isolated`, which `vz` cannot
provide. This command thus chooses a shared network, where sandboxes are not
promised separation.

For later runs, put both `BRIG_HYPERVISOR=vz` and `BRIG_NETWORK=shared` in
your shell profile.

**Confirm:**

```bash
brig run claude
```

With both settings applied, or on macOS 15 or newer with `hvi`, the run
reaches the agent prompt.

<details><summary>The same failure on an older Brig</summary>

An older Brig did not read the version first. Its boot failed like this:

```
VMM started (PID 33351)
brig: sandbox did not become ready; check 'brig logs claude (or the runtime's
own, /opt/homebrew/bin/hull logs brig-claude-code)'
```

`VMM started (PID 33351)` is a hull line. The log that the message names
held one line, `dyld[33351]: missing symbol called`, which does not name the
symbol. Upgrade Brig to get the macOS 15 refusal.

</details>

## Refused on vz or qemu

```
brig: a policy applies to this sandbox, and hull on vz cannot enforce the egress
policy: vz takes its network from vmnet, which brig does not filter. brig
enforces a policy at the user-mode network gateway that only the hvi backend
uses. Run it on hvi (BRIG_HYPERVISOR=hvi), or detach the policy. brig will not
boot a sandbox under a policy nothing enforces
```

**Exit code:** `7`

An isolated network needs the same gateway. With no policy bound, its
refusal exits `1`:

```
brig: --network isolated gives the sandbox a network of its own, which brig can
only do on the hvi backend, where it owns the gateway (BRIG_HYPERVISOR is "vz");
vmnet decides what a vz sandbox shares. Run it on hvi, or run sandboxes that
must not reach each other on separate hosts (see docs/security.md)
```

When `network: isolated` in the profile asked for the isolated network, the
message names the profile and the way to run it on the shared network.

**Cause:** Brig enforces an egress policy at the network gateway of the
`hvi` backend. On `vz` and `qemu` the sandbox takes its network from vmnet,
which Brig does not filter, so Brig refuses the run.

**Fix:** Run the sandbox on `hvi`. To stay on `vz` or `qemu`, detach the
policy and pass `--network shared`. `brig policy check` names what is bound:

```bash
BRIG_HYPERVISOR=hvi brig run claude
brig policy check claude-code
```

**Confirm:**

```bash
BRIG_HYPERVISOR=hvi brig plan claude
```

The plan has no `REFUSED` row.

### Policy refused on Linux

On Linux, Brig enforces an egress policy at the bridge of the network that a
nerdctl sandbox owns.

docker cannot enforce a policy, and the message says to use nerdctl or to
detach the policy.

On nerdctl, when Brig cannot put the rules in place, the refusal reads
`whether nerdctl on <shim> enforces the egress policy is unknown`. Then:

1. Install `nftables` and the `nsenter` of util-linux.
2. For a rootless install, make sure that rootless containerd runs:
   `$XDG_RUNTIME_DIR/containerd-rootless/child_pid` exists.

When the resolver did not start, the refusal quotes the start of its log,
`~/.brig/egress/<sandbox>.log`.

## docker does not carry annotations

Linux only, for a profile that boots an unmodified image (six of the eight
built-in profiles).

```
brig: could not start the sandbox: this profile boots an unmodified image,
which needs the kernel passed as an OCI annotation; docker does not carry
annotations through to the runtime. Use nerdctl, or point BRIG_RUNTIME_BIN at
it
```

**Cause:** Brig accepts Docker where it looks for nerdctl, and most of what
Brig needs works on both. Docker drops the OCI annotation that passes the
kernel, so the guest has no kernel to boot, and Brig refuses before the
boot.

**Fix:** Install nerdctl, or point Brig at one that you have:

```bash
BRIG_RUNTIME_BIN=/path/to/nerdctl brig run claude
```

**Confirm:**

```bash
brig doctor
```

The `runtime` line names `nerdctl`.

## Shim shares the host kernel

Linux only.

```
brig: refusing BRIG_CONTAINERD_RUNTIME=runc: that shim gives the guest the
host's own kernel, which is not the boundary brig provides. Unset
BRIG_CONTAINERD_RUNTIME to use the default microVM shim io.containerd.urunc.v2.
A sandbox already started on such a shim keeps it, and the next run would join
it by name: stop it first with brig stop <ref>
```

**Exit code:** `1`

**Cause:** `BRIG_CONTAINERD_RUNTIME` names `runc` or `crun`, by name or by
path, and those shims run the guest as a plain container on the host kernel.
Brig puts a separate guest kernel between the agent and the host.

**Fix:** Unset the variable. If an older Brig started this sandbox on such a
shim, also stop the sandbox:

```bash
unset BRIG_CONTAINERD_RUNTIME
brig stop claude
```

**Confirm:**

```bash
brig plan claude
```

The plan has no `REFUSED` row, and its `ISOLATION` row names
`io.containerd.urunc.v2`.

Brig allows a shim that it cannot place, such as another microVM shim.

## Sandbox restarted on `brig sh`

Brig warns and recreates the sandbox in the four cases below (see
[Sessions, homes and projects](sessions.md)). Brig keeps the guest home on
the host. Every other session on that sandbox disconnects at the restart.

### Different guest home

```
brig: the running sandbox is not mounting /Users/alex/work: its share went stale
  ↳ the directory was renamed or replaced, or the workspace changed
  ↳ brig restarts it, and any other session using this sandbox will be disconnected
```

**Cause:** A `--home` (or `BRIG_WORKSPACE`) differs from the guest home that
the running sandbox mounts.

**Fix:** The `WORKSPACE` column shows which guest home a sandbox mounts:

```bash
brig ls
```

If you did not intend to change the guest home, remove the `--home` flag.

### Different network policy

```
brig: this sandbox is running under a different network policy than the one that applies now
  ↳ rules are fixed when a sandbox boots, so brig restarts it
  ↳ any other session using this sandbox will be disconnected
```

**Cause:** Egress rules are fixed at boot, so a policy that you attach or
detach on a running sandbox changes nothing that it can reach. The next
`brig sh` or `brig run` on that session restarts the sandbox.

**Fix:** See what a profile has bound, and whether Brig can enforce it:

```bash
brig policy check claude
```

### Different network posture

```
brig: this sandbox was started with the isolated posture and --network asks for shared
  ↳ rules are fixed when a sandbox boots, so brig restarts it
  ↳ any other session using this sandbox will be disconnected
```

**Cause:** A sandbox keeps the posture that it started with, so a command
that names no posture never causes this restart. A `--network` or
`BRIG_NETWORK` that names a different posture does.

**Fix:** If you did not intend to change the posture, look for an exported
`BRIG_NETWORK` in this shell.

The warning names the posture that the sandbox runs with. A sandbox that a
policy isolated is named `isolated`, even after the policy is detached.

### Different project

```
brig: the running sandbox has /Users/alex/app mounted as its project and this run names /Users/alex/other-app
  ↳ a share cannot be attached to a live sandbox, so brig restarts it
  ↳ any other session using this sandbox will be disconnected
```

**Cause:** A project is a share too, fixed at boot like the guest home. A
directory on the run line that differs from the one the session last used
causes the restart.

**Fix:** See what a session last used:

```bash
brig info claude
```

The `PROJECT` row, when there is one, names the directory. To keep the
sandbox up, pass the same directory or none.

### Confirm the restart

```bash
brig sh claude
```

`brig ls` lists the ref as `running` again, and a second `brig sh` on it
prints no restart warning.

## Unknown command: trust

```
Error: Unknown command: trust
```

This message comes from Homebrew.

**Cause:** `brew trust` needs a recent Homebrew.

**Fix:** Read your version and update:

```bash
brew --version
brew update
```

**Confirm:**

```bash
brew trust brig-sh/brig
```

The command runs, and you can continue the install.

## Symlinked guest home refused

```
brig: refusing to write /Users/alex/.brig/homes/brig-claude-code/.claude/.claude.json:
it is a symlink to "/Users/alex/.ssh/authorized_keys", and brig writes only
regular files inside the workspace. The workspace is mounted read-write as the
sandbox's home, so that link was put there from inside the sandbox, to have brig
-- which runs as you, on the host -- reach a file the sandbox cannot. Nothing
was written; inspect /Users/alex/.brig/homes/brig-claude-code/.claude/.claude.json and
remove it before running brig again: a symlink leads out of a directory brig is
checking
```

**Cause:** Brig writes state files into the guest home from the host, as
you, and refuses to follow a symlink in the place of one. The symlink points
Brig at a host path that the sandbox cannot reach.

**Fix:** Nothing was written, and a retry gives the same refusal. Brig
writes only regular files there, so you or the sandbox created the link.

1. Inspect the path that the message names.
2. Remove the link, or point the guest home at a different directory.

Brig refuses a `--home` that is a symlink in the same way. Name the real
directory.

**Confirm:**

```bash
brig run claude
```

The run reaches the agent. See
[Writing into the workspace](security.md#writing-into-the-workspace) for the
reason.

## Project behind a symlink refused

```
brig: refusing to use /Users/alex/work/app as this run's project:
/Users/alex/work on the way to it is a symlink to "/Volumes/data/work", so the
sandbox would be handed a directory other than the one you named. Name the
real directory instead: a symlink leads out of a directory brig is checking
```

**Cause:** The project is mounted read-write, so the sandbox can replace any
directory at or below it. Brig cannot tell a link that the sandbox planted
there from your own link, so it refuses both and names where the link
points.

**Fix:** Name the real directory:

```bash
brig run claude /Volumes/data/work/app
```

When the message ends with "This session's project was remembered from an
earlier run", the project came from an earlier `brig run` of this session.

- To replace the project, use `brig run` with the real directory after the
  ref, or with `--no-project`.
- Until then, Brig refuses only the verbs that boot or join the sandbox.
  `brig stop`, `brig rm` and `brig info` still work, and `brig info` repeats
  the refusal.

Brig still follows a link in a directory that you cannot write, such as
`/tmp` on macOS.

**Confirm:**

```bash
brig info claude
```

The PROJECT row names the real directory, and the refusal is gone. See
[Mounting a project](security.md#mounting-a-project) for the reason.
