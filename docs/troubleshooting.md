# Troubleshooting

Find the message that you saw. Each entry gives the cause, the fix and the
command that confirms the fix.

Run `brig doctor` first. It checks the host, the hypervisor, the runtime,
the broker, the boot assets, cosign, the profiles, the secret store and
brigd, one line each. Under a line marked `!!` it prints the fix.
`brig doctor <agent>` also checks the image of that agent. `--json` prints
the same report as one document.

## Exit codes

Every failure exits with one code from a stable set.
[Exit codes](cli.md#exit-codes) has the table.

`brig rm` and `brig logs` on a ref with no sandbox exit `3` and name the ref
that you typed.

## Limits of `brig doctor`

Only two lines change the exit status of `brig doctor`. A run exits with
the same two codes ([Exit codes](cli.md#exit-codes)).

| Line | Exit status |
|---|---|
| `runtime`, with a missing or broken runtime | `4` |
| `secrets`, with a secret store that does not open | `6` |
| Every other line, a `!!` line included | `0`. The line prints its fix. |

`brig doctor` prints `!!` and exits `0` in these two verification cases, but
a real run stops on each:

- `BRIG_VERIFY` has a value that Brig does not recognize.
- `BRIG_VERIFY=require` is set and cosign is not installed.

A script that reads only the exit status misses both. Read the setting
before you rely on it:

```bash
echo "$BRIG_VERIFY"
```

`require` also refuses every image outside the Brig registry,
`ghcr.io/brig-sh/`. `claude-desktop` and `ubuntu` are both outside it.
`brig doctor claude-desktop` does not report that refusal.

## No runtime found on PATH

```
brig: no runtime found on PATH: brig drives hull on macOS, and none was there.
See https://github.com/brig-sh/brig/blob/main/docs/install.md, or point BRIG_RUNTIME_BIN at a build
```

On Linux the runtime is `nerdctl`, and the message names it:

```
brig: no runtime found on PATH: install nerdctl, or point BRIG_RUNTIME_BIN at one
```

**Exit code:** `4` on both platforms.

**Cause:** The runtime is not installed, or not on PATH. On macOS this
usually follows a from-source install of Brig.

**Fix:**

1. Look for the runtime:

   ```bash
   which hull        # macOS
   which nerdctl     # Linux
   ```

2. If it is missing, install it ([Install Brig](install.md)). On macOS,
   `brew install --cask brig` installs hull too.
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

**Cause:** `BRIG_RUNTIME` names a runtime that Brig does not support.
`brig ls` and `brig info` fail with this message too.

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

## assets missing at ...

Only `brig doctor` prints this message.

```
  !!  boot      assets missing at /Users/alex/.hull/store/assets
          run any agent once to fetch them, or set BRIG_BOOT_ASSETS to a directory that has them
```

**Cause:** Brig has not yet downloaded the kernel and initrd that most
built-in profiles boot.

**Fix:** None. This is normal before the first boot, and the next
`brig run` fetches them. A run does not download into a directory that
`BRIG_BOOT_ASSETS` names.

**Confirm:**

```bash
brig doctor
```

The `boot` line reads `ok  boot      assets present at /Users/alex/.hull/store/assets`.

## Started without the launcher

```
  !!  bundle    this brig is inside the runtime bundle, and its launcher did not run, so brig will not find the bundle's CNI plugins or boot assets
          run brig from /home/alex/.local/bin: export PATH="/home/alex/.local/bin:$PATH", or load the environment: . /home/alex/.local/share/brig/data/etc/brig-env.sh
```

`brig run` prints the same warning before it boots the sandbox.

**Cause:** The bundle's `bin` directory is on `PATH` ahead of the launcher
in `~/.local/bin` (`/usr/local/bin` on a node-wide install). Without the
launcher, the run fails with one of these:

- `needs CNI plugin "bridge" to be installed in CNI_PATH ("/opt/cni/bin")`
- `sandbox did not become ready`, with `exec sleep failed: No such file or
  directory` in the log

**Fix:** Remove the sandbox while the bundle's `bin` directory is first on
`PATH`, then put the launcher directory first:

```bash
PATH="$HOME/.local/share/brig/data/bin:$PATH" brig rm ubuntu
export PATH="$HOME/.local/bin:$PATH"
```

Keep that order. Only a `brig rm` without the launcher removes the old
network. If it stays, the next run through the launcher fails with
`failed to read from init socket: EOF`.

**Confirm:**

```bash
brig doctor
```

The `bundle` line is gone, and the `boot` line reads `ok`.

## Secret store did not answer

```
brig: the brig-mine sandbox needs gh-token from brig's secret store, which
could not be read: <cause>
```

**Exit code:** `6`, the same as for a missing secret.

**Cause:** Brig cannot tell whether the value is there. The usual causes are
a locked keychain, a keyring daemon that is not running, or a permission
that Brig does not have.

**Fix:**

1. Run `brig doctor`. The `secrets` line names the failure.
2. Unlock the keychain, start the keyring daemon, or grant the permission.
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

It reports what reaches the guest and whether the guest will be
authenticated. If a variable that you expected is not listed, one of the
four cases below is the cause.

### On the denylist

```
brig: not forwarding ANTHROPIC_API_KEY: it is on the claude-code denylist
  ↳ it outranks the subscription credential, and would move this sandbox onto metered billing without saying so
  → to forward it anyway:  BRIG_ALLOW_DENIED=1
```

**Cause:** By default Brig refuses a key that moves the sandbox from your
subscription to metered billing.

**Fix:** If you want metered billing, set `BRIG_ALLOW_DENIED=1`.

### Unresolved secret reference

```
brig: not forwarding GH_TOKEN: it looks like an unresolved secret reference (op://...), not a credential
  → resolve it on the host before you run brig
  → to forward it as it is:  BRIG_ALLOW_REFS=1
```

**Cause:** A tool such as direnv left a secret-manager reference
(`scheme://...`) unresolved. Inside the guest, that value produces "Invalid
username or token".

**Fix:** Resolve the reference on the host, then run again.

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

**Fix:** Renew the login on the host, then run the import in the second
line. A renewal on the host alone has no effect, because a run reads the
copy that Brig stored.

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

The credential appears in the list that Brig forwards, and the warning is
gone.

## Login lost after a stop

Brig prints no error. The agent shows its login screen on a sandbox that you
already logged in to.

**Cause:** `claude-code` and `herdr` keep the Claude Code login in memory,
never on host disk, so `brig stop` loses it. The other profiles keep their
login in the guest home, where it survives a stop.

**Fix:** Import the login that is on this Mac into the Brig secret store,
once:

```bash
brig secret import claude-code
```

Brig then delivers the login on every command that reaches the sandbox. See
[Carry your host login in, once](authentication.md#2-carry-your-host-login-in-once).

**Confirm:**

```bash
brig stop claude
brig run claude
```

The agent starts logged in.

## Browser stays in front of Claude Desktop

Brig prints no error. You sign in to `claude-desktop` in the sandbox, and a
browser page that says "Finish sign-in in the Claude app" stays on screen.
Its Open Claude button does not switch to the app.

**Cause:** The sign-in opens a browser in front of the app, and that page
does not close on its own. The sign-in itself finishes in the app behind it.

**Fix:** Close the browser tab, or the whole browser window with its X. The
app behind it shows you signed in.

**Confirm:** The Claude Desktop window shows the signed-in app, with no
sign-in screen.

## cosign is not installed

```
brig: cannot verify image ghcr.io/brig-sh/claude-code-stock:root: cosign is not installed (`brew install cosign`)
  ↳ booting it unchecked
```

On Linux the parentheses read `Brig's install.sh brings one, or see
https://docs.sigstore.dev/cosign/system_config/installation/`.

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

The refusal under `BRIG_VERIFY=require`:

```
brig: refusing to boot image ghcr.io/brig-sh/claude-code-stock:root: cosign is
not installed (`brew install cosign`), so nothing could be checked
(BRIG_VERIFY=require). Set BRIG_VERIFY=warn to boot it unchecked
```

**Cause:** The image is under the Brig registry, and Brig found no cosign to
check its signature.

**Fix:** Install cosign to get the check:

```bash
brew install cosign         # macOS
```

On Linux, run the Brig `install.sh` again, install your distribution's
package, or follow the
[cosign install page](https://docs.sigstore.dev/cosign/system_config/installation/).

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

**Cause:** Brig did not reach the registry, so nothing was checked. The
usual cause is an offline host, or a captive portal.

**Fix:** Reconnect and run again. If you trust the copy on disk enough to
boot it once unverified, answer `y` at the prompt.

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

**Cause:** A Docker credential helper in your Docker `config.json` did not
answer, for example `docker-credential-desktop` while Docker Desktop is not
running. If the message names no helper, see
[Cannot reach the registry](#cannot-reach-the-registry).

**Fix:** Start Docker Desktop, or point `DOCKER_CONFIG` at an empty
directory for the run. The images that Brig boots are public, so cosign
needs no credentials for them.

```bash
DOCKER_CONFIG="$(mktemp -d)" brig run claude
```

If brigd runs your boots, restart brigd with `DOCKER_CONFIG` set.

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

The detail after the colon says which cause applies.

**Cause:** Brig cannot reach the registry, the image reference does not
exist, or the image has no build for your architecture. An architecture
mismatch usually means that `--image` or `BRIG_IMAGE` pins a
single-architecture tag (`:arm64` or `:amd64`) that is not yours.

**Fix:** Read the reference that you boot:

```bash
brig info claude      # shows the image, among other things
```

| Cause | Fix |
|---|---|
| Wrong architecture | Remove the pinned tag, or pin the tag for your machine (`:arm64` or `:amd64`). The published agents default to `:root`, which has builds for `linux/arm64` and `linux/amd64`. |
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

**Cause:** The image is under the Brig registry, and its signature does not
match the workflow that builds Brig images.

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
bundle lists. A file changed after the fetch, or an older tool fetched it.
See
[The kernel, not only the image](security.md#the-kernel-not-only-the-image).

**Fix:**

| Case | Fix |
|---|---|
| Default | Delete the two files in the directory that the message names, then run again. Brig fetches the signed bundle again. |
| The files are your own build | Point `BRIG_BOOT_ASSETS` at their directory. `warn` then states the difference and boots them. |

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

**Cause:** A kernel or initrd file in the Linux runtime bundle changed after
the install, so it no longer matches the signed release. The same refusal
appears when the release's signed checksums do not verify. See
[The kernel, not only the image](security.md#the-kernel-not-only-the-image).

**Fix:**

| Case | Fix |
|---|---|
| Default | Run the Brig `install.sh` again to put the bundle files back. |
| The kernel is your own build | Keep it in a separate directory and point `BRIG_BOOT_ASSETS` there. |
| The bundle was released from a fork | Its signature names the release workflow of the fork. Set `BRIG_VERIFY_RUNTIME_IDENTITY` to that workflow. |

**Confirm:**

```bash
brig run claude
```

The run prints `brig: image and boot assets verified`.

## The sandbox never became ready

```
brig: sandbox did not become ready; check 'brig logs claude-code (or the runtime's own, /opt/homebrew/bin/hull logs brig-claude-code)'
```

**Cause:** The sandbox started, but the guest did not answer in time.

**Fix:**

1. Read the sandbox's log. It holds the errors of the guest. A
   `virtiofsd::limits` warning about open file descriptors is harmless.

   ```bash
   brig logs claude
   ```

2. If the guest is slow, give it more time with `BRIG_READY_TIMEOUT`
   (seconds, default 30):

   ```bash
   BRIG_READY_TIMEOUT=60 brig run claude
   ```

<details><summary>Linux: a host that runs a urunc release</summary>

A Linux host that installed its own urunc (`BRIG_INSTALL_RUNTIME=0`, or Brig
0.2.0 or earlier) also fails here, and `brig doctor` still reports `ok`.
Install the runtime bundle for the whole machine:

```bash
curl -fsSL https://brig.sh/install | sudo sh
```

For a rootless install, see [Rootless installs](install.md#rootless-installs).

</details>

**Confirm:**

```bash
brig run claude
```

The run reaches the agent and does not print "sandbox did not become ready".

## A session hangs

The terminal of a `brig run` or `brig sh` does not answer. You cannot type,
and Ctrl-C does nothing.

**Cause:** Usually the guest. When the guest runs out of memory, it can stop
for a long time before the kernel stops a process, and the agent cannot draw
its screen. Other causes are a VMM or network gateway that stopped.

**Fix:**

1. Open another shell on the host, and keep the stuck terminal open.
2. Write a report. `brig report` prints the path of the file it wrote:

   ```bash
   brig report claude
   ```

3. Read the verdict at the top of the file. If it names a fix, do it. For a
   guest that ran out of memory, stop the sandbox and run it with more
   memory:

   ```bash
   brig stop claude
   brig run claude --mem 8192 -- --resume
   ```

   `brig stop` keeps the guest home, and the conversations of the agent are in
   it. Do not use `brig rm`, which deletes a guest home that Brig created.

4. If the verdict names no fix, attach the file to a
   [bug report](https://github.com/brig-sh/brig/issues/new?template=bug_report.md).
   Read the file first. See [`brig report`](cli.md#brig-report).

## runtimeBin is not there

```
brig: runtime unavailable: this profile's runtimeBin is /old/path/hull, which is
not there: stat /old/path/hull: no such file or directory
```

**Exit code:** `4`

**Cause:** The `runtimeBin:` field of your own profile names a binary that
moved or was removed. A bare `brig doctor` does not check it, so its
`runtime` line can read `ok`.

**Fix:** Open the profile, then correct or remove the line:

```bash
brig agent edit mine
```

**Confirm:**

```bash
brig doctor mine
```

The `runtime` line reads `ok` and names the binary of that profile.

## A required secret is missing

```
brig: missing secret "gh-token" needed by the brig-mine sandbox -- create it
first with: brig secret create gh-token
```

When several secrets are missing, the message lists one line for each.

**Exit code:** `6`

**Cause:** A required secret has no value in the Brig secret store. A secret
is required unless the profile declares `required: false`. No built-in
profile declares a required secret, so the declaration is in your own
profile (`brig agent edit mine`).

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

**Cause:** Most built-in profiles use the `hvi` hypervisor backend, which
needs macOS 15 or newer.

**Fix:** Upgrade to macOS 15 or newer and keep `hvi`.

To stay on the older macOS, use the `vz` backend (Virtualization.framework).
For one run:

```bash
BRIG_HYPERVISOR=vz brig run --network shared claude
```

`vz` cannot give a sandbox an isolated network, so this command uses a
shared one. Brig does not promise that sandboxes on a shared network are
separated.

For later runs, put both `BRIG_HYPERVISOR=vz` and `BRIG_NETWORK=shared` in
your shell profile.

**Confirm:**

```bash
brig run claude
```

With both settings applied, or on macOS 15 or newer with `hvi`, the run
reaches the agent prompt.

## Refused on vz or qemu

```
brig: a policy applies to this sandbox, and hull on vz cannot enforce the egress
policy: vz takes its network from vmnet, which brig does not filter. brig
enforces a policy at the user-mode network gateway that only the hvi backend
uses. Run it on hvi (BRIG_HYPERVISOR=hvi), or detach the policy. brig will not
boot a sandbox under a policy nothing enforces
```

**Exit code:** `7`

An isolated network also needs `hvi`. With no policy bound, its refusal
exits `1`:

```
brig: --network isolated gives the sandbox a network of its own, which brig can
only do on the hvi backend, where it owns the gateway (BRIG_HYPERVISOR is "vz");
vmnet decides what a vz sandbox shares. Run it on hvi, or run sandboxes that
must not reach each other on separate hosts (see docs/security.md)
```

**Cause:** Brig enforces an egress policy and an isolated network only on
the `hvi` backend. On `vz` and `qemu`, Brig does not filter the network.

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

On Linux, only nerdctl can enforce an egress policy. With docker, the
refusal says to use nerdctl or to detach the policy.

On nerdctl, when Brig cannot put the rules in place, the refusal reads
`whether nerdctl on <shim> enforces the egress policy is unknown`. Then:

1. Install `nftables` and the `nsenter` of util-linux.
2. For a rootless install, make sure that rootless containerd runs:
   `$XDG_RUNTIME_DIR/containerd-rootless/child_pid` exists.

When the resolver did not start, the refusal quotes the start of its log,
`~/.brig/egress/<sandbox>.log`.

## docker does not carry annotations

Linux only, for most built-in profiles.

```
brig: could not start the sandbox: this profile boots an unmodified image,
which needs the kernel passed as an OCI annotation; docker does not carry
annotations through to the runtime. Use nerdctl, or point BRIG_RUNTIME_BIN at
it
```

**Cause:** Brig found Docker where it looks for nerdctl. Docker cannot give
these profiles a kernel to boot, so Brig refuses before the boot.

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

**Cause:** `BRIG_CONTAINERD_RUNTIME` names `runc`, `crun`,
`containerd-shim-runc-v1` or `-v2`, by name or by path. Those run the guest
as a plain container on the host kernel. Brig allows a shim that it does not
recognize, such as another microVM shim.

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

## Sandbox restarted on `brig sh`

Brig warns and recreates the sandbox in the five cases below (see
[Sessions, homes and projects](sessions.md)). The guest home stays on the
host. Every other session on that sandbox disconnects at the restart.

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

**Cause:** Egress rules are fixed at boot. A policy that you attach or
detach on a running sandbox changes nothing that it can reach until the next
`brig sh` or `brig run` restarts it.

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

**Cause:** A `--network` or `BRIG_NETWORK` names a different posture than
the one the sandbox started with. A command that names no posture never
causes this restart. The warning names a sandbox that a policy isolated as
`isolated`, even after you detach the policy.

**Fix:** If you did not intend to change the posture, look for an exported
`BRIG_NETWORK` in this shell.

### Different project

```
brig: the running sandbox has /Users/alex/app mounted as its project and this run names /Users/alex/other-app
  ↳ a share cannot be attached to a live sandbox, so brig restarts it
  ↳ any other session using this sandbox will be disconnected
```

**Cause:** The run names a different project directory than the one the
session last used. A project is fixed at boot, like the guest home.

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

### The guest is not answering

```
brig: the log of the sandbox ends with
  ↳ exec sleep failed: No such file or directory
brig: the running sandbox is not answering
  ↳ its guest may have failed while booting
  ↳ brig restarts it, and any other session using this sandbox will be disconnected
```

**Cause:** The guest did not answer within `BRIG_READY_TIMEOUT`. The lines
above the warning are the end of its log, and the restart deletes that log.

**Fix:** Read those lines. If the guest fails again on the restart, see
[The sandbox never became ready](#the-sandbox-never-became-ready).

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

**Cause:** Brig writes state files into the guest home as you, on the host.
It refuses to follow a symlink in place of one. See
[Writing into the workspace](security.md#writing-into-the-workspace).

**Fix:** A retry gives the same refusal. Brig writes only regular files
there, so you or the sandbox created the link.

1. Inspect the path that the message names.
2. Remove the link, or point the guest home at a different directory.

Brig refuses a `--home` that is a symlink in the same way. Name the real
directory.

**Confirm:**

```bash
brig run claude
```

The run reaches the agent.

## Project behind a symlink refused

```
brig: refusing to use /Users/alex/work/app as this run's project:
/Users/alex/work on the way to it is a symlink to "/Volumes/data/work", so the
sandbox would be handed a directory other than the one you named. Name the
real directory instead: a symlink leads out of a directory brig is checking
```

**Cause:** The sandbox can write the project, so it can replace any
directory in it with a link. Brig refuses every link on the path, your own
included. It still follows a link in a directory that you cannot write, such
as `/tmp` on macOS. See
[Mounting a project](security.md#mounting-a-project).

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

**Confirm:**

```bash
brig info claude
```

The PROJECT row names the real directory, and the refusal is gone.
