# The runtimes brig drives

brig boots nothing itself. It decides what a sandbox should be and then asks
another program to make one: `hull` on macOS, `nerdctl` over containerd on
Linux. Those programs are separate projects with their own repositories,
releases and licences, and installing brig means installing them too.

This page says what each one is, what it is licensed under, what brig asks of
it, and exactly which commands brig runs against it. If you are deciding
whether to adopt this stack, the short answer is that on macOS you are taking
brig and hull, and on Linux you are taking brig plus three projects it does
not own.

## What you are installing

Licences were read from each repository's `LICENSE` file on 2026-08-26.

| component | what it does | where it comes from | licence |
| --- | --- | --- | --- |
| `brig`, `brigd` | resolves the profile, the workspace and the credentials, then drives the runtime | this repository | Apache-2.0 |
| `hull` | CLI that pulls an OCI image and boots it as a microVM on Apple Silicon | [brig-sh/hull](https://github.com/brig-sh/hull) | Apache-2.0 |
| `vz-runner` | Swift helper hull launches for its `vz` backend, which is what talks to Virtualization.framework | same repository as hull, installed beside it | Apache-2.0 |
| `hvi` | separate VM monitor for hull's `hvi` backend, which talks to Hypervisor.framework directly | a git submodule of hull (`hvi-vmm`), installed beside hull | unconfirmed: `hvi-vmm` is a private submodule of hull |
| `nerdctl` | Docker-compatible CLI for containerd, the binary brig drives on Linux | [containerd/nerdctl](https://github.com/containerd/nerdctl) | Apache-2.0 |
| `containerd` | daemon underneath nerdctl: it holds the image store and hands each container to a shim | [containerd/containerd](https://github.com/containerd/containerd) | Apache-2.0 |
| `urunc` | the containerd shim `io.containerd.urunc.v2`, which boots the container as a microVM instead of a process | [urunc-dev/urunc](https://github.com/urunc-dev/urunc) | Apache-2.0 |
| `cosign` | checks the signature on a guest image before it boots. Optional, and the check degrades to a warning without it | [sigstore/cosign](https://github.com/sigstore/cosign) | Apache-2.0 |
| `oras` | pulls the boot bundle on Linux for a `genericBoot` profile. Optional otherwise | [oras-project/oras](https://github.com/oras-project/oras) | Apache-2.0 |
| boot bundle | the kernel, `container-initrd` and the in-guest agent that let brig exec into an image built as an ordinary container. Published as an OCI artifact at `ghcr.io/nofireai/hull-assets`, one tag per guest platform | fetched by hull on macOS, by oras on Linux | unconfirmed: signed with keyless cosign, but its source repository is not public and states no licence |

hull is published by the same organisation as brig and exists because brig
needed it, but it is a usable runtime on its own and its command surface is
not brig-shaped. nerdctl, containerd and urunc predate brig, are used far
outside it, and brig is an ordinary caller of all three: it passes flags any
other caller could pass.

## Where the boundary sits

brig decides, and the runtime never sees the reasoning:

- which image, and whether its signature verified (`internal/verify`)
- which host directory is the workspace, and that it is the guest's home
- which credentials are resolved, which are denied for billing safety, and
  which are handed in by name rather than by value
- the sandbox name, memory, CPU count, network mode, pull policy, root
  filesystem type, hypervisor backend and shared directories
- on macOS, the kernel and initrd paths for an image that carries no kernel,
  and the gateway address each sandbox takes

The runtime decides everything mechanical: how the image is pulled and stored,
how the VM is configured and booted, how a command gets into the guest, and
what a stopped instance means.

Nothing is linked in. brig has one direct Go dependency and every interaction
below is a subprocess, which is the same rule that applies to `cosign` and
`oras`.

## Every command brig runs

Taken from the source. On macOS, from `internal/runtime/hull.go` unless another
file is named:

```
hull assets pull                        # HULL_BOOT_ASSETS=<dir> in the environment
hull assets dir
hull ps
hull ps -a                              # falls back to `hull ps` if -a is refused
hull run --detach --name <name>
     --hypervisor <vz|hvi|qemu> --net <shared|none>
     --pull <missing|always|never> --mem <MB> --cpus <n>
     [--rootfs-type <block|virtiofs|9pfs>]
     [--annotation com.urunc.unikernel.bootKernel=<path>]
     [--annotation com.urunc.unikernel.bootInitrd=<path>]
     [--gateway-sock <path> --gateway-cidr <cidr>]
     [--shared-dir <host>:<guest>[:ro]]...
     [--gui [--gui-title <title>]]
     [--env <NAME>]... <image>
hull exec [-t] [--cwd <dir>] [-u <user>] [--env <NAME>]... <name> -- <cmd>...
hull stop <name>
hull rm <name>
hull network-gateway --socket <path> --qemu-socket <path>.qemu
     --subnet 198.18.0.0/24 --gateway-ip 198.18.0.1   # internal/runtime/gateway.go
```

brig picks that subnet from 198.18.0.0/15, the range RFC 2544 reserves for
network benchmarking: it is never routed on the public internet and almost
nothing claims it, so a sandbox network is unlikely to collide with something
the workspace needs to reach. The alternatives are all crowded -- 10.0.0.0/8 by
corporate VPNs and cloud VPCs, 172.16.0.0/12 by Docker, 192.168.0.0/16 by home
routers and by vmnet on macOS, and 100.64.0.0/10 by Tailscale. The sibling
198.19.0.0/16 is left alone because OrbStack uses it.

`hull exec` is the whole exec path: the reachability probe, the captured read,
the credential written over stdin and the terminal handover all build the same
argv (`execArgs`), and the handover replaces the brig process with hull so the
guest gets a real terminal. `hull logs <name>` appears in brig's error output
as a suggestion and is never run.

On Linux, from `internal/runtime/nerdctl.go`:

```
nerdctl ps --filter name=^<name>$ --format {{.Names}}
nerdctl ps -a --format {{.Names}}\t{{.Status}}
nerdctl run --detach --name <name>
     --runtime io.containerd.urunc.v2         # BRIG_CONTAINERD_RUNTIME overrides
     --memory <MB>m --cpus <n>
     [--pull <missing|always|never>] [--network none]
     [--annotation com.urunc.unikernel.bootKernel=<path>]
     [--annotation com.urunc.unikernel.bootInitrd=<path>]
     [-v <host>:<guest>[:ro]]... [--tmpfs <path>:<options>]...
     [-e <NAME>]... <image> sleep infinity
nerdctl exec -i [-t] [-w <dir>] [-u <user>] [-e <NAME>]... <name> <cmd>...
nerdctl stop <name>
nerdctl rm <name>
```

The container is parked on `sleep infinity` because a container exits when its
command does, and the sandbox has to outlive the exec that used it. As on
macOS, `nerdctl logs <name>` is only ever suggested.

Two commands are not aimed at a runtime but run on the same paths:

```
oras pull ghcr.io/nofireai/hull-assets:<os>-<arch> --output <dir>
                                                # internal/runtime/bootfetch.go
cosign verify --certificate-identity-regexp <identity>
     --certificate-oidc-issuer <issuer> <image> # internal/verify/verify.go
```

Every runtime command carries `HULL_TELEMETRY_PRODUCT=brig` and
`HULL_TELEMETRY_SUPPRESS=1` in its environment, with the suppression lifted
only for the operations a user asked for, so one brig command counts once.
`DO_NOT_TRACK` and `HULL_TELEMETRY_DISABLED` pass through untouched and win.

Forwarded values travel in that same environment and only the bare variable
name goes in argv, so nothing readable in `ps` carries a secret. `BRIG_ENV_ARGV=1`
puts ordinary values back on the command line for a runtime build that cannot
take a bare `--env NAME`, and a value brig resolved on your behalf stays off
the command line even then.

## How brig finds the runtime

`internal/runtime/runtime.go`, in order:

1. `BRIG_RUNTIME` names the backend, `hull` or `nerdctl`. Anything else is
   refused by name. Unset, it is `hull` on macOS and `nerdctl` everywhere else.
2. `BRIG_RUNTIME_BIN` is the executable to run, and skips the PATH lookup.
3. A profile's `runtimeBin` does the same thing without a variable per shell,
   and loses to `BRIG_RUNTIME_BIN`. A leading `~` is expanded, and a path that
   is missing or not executable is reported against the profile that named it.
4. Otherwise PATH: `hull` for the hull backend, and `nerdctl` then `docker` for
   the other one.

What brig asks the runtime about itself is short. It never asks for a version.
It asks hull where its boot assets live (`hull assets dir`), because they sit
under hull's own store and a path compiled into brig would drift silently, and
it asks either runtime which sandboxes exist and what state they are in (`ps`).
`brig status` prints what it settled on, as `runtime hull (/opt/homebrew/bin/hull)`.

The absence of a version check is deliberate but has a consequence worth
knowing: brig degrades one feature at a time rather than refusing an old build
outright. A hull without `assets dir` falls back to `~/.hull/assets`, and a
runtime without `ps -a` falls back to the plain listing. A hull too old for a
flag brig passes fails at that flag, with hull's own message.

## What brig requires of each

**hull** has to accept the verbs and flags listed above, and three things
beyond them:

- a bare `--env NAME`, taking the value from its own environment. Without it
  the only way to forward a credential is `BRIG_ENV_ARGV=1`, which puts values
  where `ps` can read them.
- `exec -u root`, which is how brig mounts a tmpfs inside a running sandbox.
  Container runtimes get their tmpfs at create time instead
  (`internal/wrap/secretfiles.go`).
- `network-gateway`, for the `hvi` backend. That backend has no egress of its
  own, so brig starts one shared gateway and joins every sandbox to it, and
  hands out the addresses on that network itself. Sharing a gateway is not the
  same as sharing a segment: the gateway gives each guest a point-to-point
  link and translates outbound traffic, so guests reach the internet through
  it and not each other. See
  [security.md](security.md#things-brig-does-not-claim) for what that means per
  backend, which is not the same answer on Linux.

Six of the eight shipped profiles ask for `hvi` and set `genericBoot: true`
(`internal/profile/specs`), so the default macOS path needs the `hvi` binary
beside hull, a working gateway, and the boot bundle. `BRIG_HYPERVISOR=vz` moves
to the other backend, and the graphical profile is refused anywhere but `vz`,
which is the backend with a console.

**nerdctl** has to carry `--annotation` through to the shim, take `--runtime`,
and honour `-v`, `--tmpfs` and a bare `-e NAME`. `docker` is accepted in its
place and works for an image that carries its own kernel; a `genericBoot`
profile is refused on docker rather than attempted, because docker does not
pass annotations to the runtime and the sandbox would boot without a kernel and
fail somewhere far from the cause.

**urunc** has to read `com.urunc.unikernel.bootKernel` and
`com.urunc.unikernel.bootInitrd` from the container's OCI spec and boot the
image with them. That pair is the entire contract between brig and urunc, and
it is the same pair hull takes on its command line.

**containerd** has to be running with the urunc shim installed.
`BRIG_CONTAINERD_RUNTIME=runc` asks for a plain container instead, which shares
the host kernel. That is the weaker boundary, and `docs/security.md` says what
it costs.

### Versions and pins

brig's source pins no version of hull, nerdctl, containerd or urunc, and
verifies no digest of any of them. The pin lives on the install path: the hull
cask in `brig-sh/homebrew-brig` names one release tarball and its sha256, and
brig's cask depends on that cask, so `brew install --cask brig` gets the exact
build the tap names. When this page was written that was hull 0.1.0-rc21, and
hull had newer tags: both casks are hand-written for the prerelease series, so
read `Casks/hull.rb` for what an install will actually give you.

The boot bundle is the other thing with a digest attached. hull verifies its
signature with cosign against the publishing workflow before writing it, and
records the digest it verified; brig delegates the whole fetch to hull on macOS
for exactly that reason. On Linux the same bundle arrives through `oras` with
no such check, and `BRIG_BOOT_ASSETS_REF` is how you pin a version or point at a
mirror.

## Swapping one out

Cheap swaps, no code:

- a different build of the same runtime: `BRIG_RUNTIME_BIN`, or `runtimeBin` in
  a profile.
- docker instead of nerdctl: it is already in the PATH search, with the
  `genericBoot` limitation above.
- a different containerd shim: `BRIG_CONTAINERD_RUNTIME`. Anything that reads
  the two boot annotations replaces urunc without brig noticing.
- a different hypervisor backend under hull: `BRIG_HYPERVISOR`, or
  `hypervisor:` in a profile.

Replacing hull or nerdctl entirely is a code change, not a setting.
`BRIG_RUNTIME` accepts those two words and nothing else, so a third backend
means implementing the `Runtime` interface in `internal/runtime/runtime.go`
(kind, binary, running, list, run, probe, output, feed, replace, stop, remove,
logs hint) and adding a case to `DetectFor`. The interface is the whole of what
brig needs from a runtime, which is the useful part of the answer: if hull
stopped tomorrow, what would have to be rebuilt is a program that boots an OCI
image as a VM and can exec into it, not anything about workspaces, credentials
or profiles. Those live above the seam and are written once for both operating
systems.

### What is shared, and what is brig's alone

Shared with other projects: containerd, nerdctl and urunc, none of which know
brig exists. cosign and oras likewise.

Shared between brig and hull: the boot bundle, the on-disk layout it lands in,
and the two annotation names. A machine that has run either runtime has already
seeded the other.

hull, `vz-runner` and `hvi` exist for this stack, though hull is a general
microVM runtime and does not depend on brig.

brig's alone: profiles, the secret store and its provenance records, the
credential forwarding rules, the billing denylist, the workspace-as-home
contract, image verification policy and `brigd`.

## Building hull from source on macOS

A from-source hull cannot boot a VM unless it is signed with an Apple identity.
This is the requirement that surprises people, so it is worth being exact about
it.

The backends do not talk to the hypervisor themselves. `vz-runner` does, and it
needs the `com.apple.security.virtualization` entitlement; `hvi` talks to
Hypervisor.framework instead and needs `com.apple.security.hypervisor`. macOS
honours an entitlement only on a binary signed with a real Apple identity, an
Apple Development or Developer ID Application certificate. An ad-hoc signature
(`codesign --sign -`) gets the entitlement honoured only with AMFI disabled,
which means disabling SIP and setting a boot argument. Copying an entitled
binary strips its signature, so it has to be re-signed after every build and
every copy.

hull's `make macos` builds and signs all three binaries when given a
`CODESIGN_IDENTITY`, and its README documents the entitlement plists. Since the
shipped brig profiles ask for the `hvi` backend, a from-source build needs the
`hvi` binary signed too, not only `vz-runner`.

The released hull is signed, notarized and stapled, so
`brew install --cask brig` gives you a runtime that boots without any of this.
That is the path to prefer unless you are working on hull itself.

Building brig from source needs none of it. brig holds no entitlement and
drives whatever hull it finds.
