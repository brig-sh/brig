# Install Brig

| Host | Use | What it installs |
| --- | --- | --- |
| macOS | [Homebrew](#macos-with-homebrew) | `brig`, `brigd`, `hull`, `cosign`, shell completions |
| macOS without Homebrew | [install.sh](#installsh) | `brig`, `brigd`, `hull`, `cosign` |
| Linux | [install.sh](#linux) | the runtime bundle (`nerdctl`, containerd, the `urunc` shim), with `brig` and `brigd` from the Brig release, and `cosign` |
| Any | [A source build](#building-from-source) | `brig` and `brigd`, without a runtime |

On macOS, Brig drives the `hull` runtime. On Linux, Brig drives `nerdctl`
over containerd, and `urunc` boots each container as a microVM.
[Platform support](#platform-support) lists the hosts Brig runs on.

## macOS with Homebrew

Prerequisites: a Mac with Apple silicon, macOS 15 or newer, and Homebrew
([brew.sh](https://brew.sh)).

```bash
brew tap brig-sh/brig
brew trust brig-sh/brig
brew install --cask brig
```

The cask installs `brig`, `brigd` and `hull`, and puts them on `PATH`. It
also installs the bash, zsh and fish completions.

| If | Do this |
| --- | --- |
| `brew trust` reports `Unknown command: trust` | Run `brew update`. Then run `brew trust` again. |
| `brew install` fails, or installs an older version than you expected | Use [install.sh](#installsh). The tap can lag the newest release. |
| The Mac runs macOS 14 | Set the two variables in [Platform support](#platform-support) before you run an agent. |

`brew trust` is required because the cask comes from a third-party tap.
Homebrew installs such a cask only after you trust the tap.

### Trying something before it is released

Two more casks carry builds that are not releases. Use them to try a feature
before its release.

```bash
brew install --cask brig-sh/brig/brig@main           # the tip of main
brew install --cask brig-sh/brig/brig@experimental   # a branch someone promoted
```

| Cask | When it moves | The `hull` it installs |
| --- | --- | --- |
| `brig@main` | On every merge to `main`, so `brew upgrade` follows what is coming | `hull@main` |
| `brig@experimental` | Only when a maintainer promotes a ref to it | `hull@experimental` |

To get an unmerged branch on `brig@experimental`, ask on the pull request, or
run the `channel` workflow with that ref.

> [!WARNING]
> These two casks are not supported. They can break, and they move without
> notice. File a bug report against one only when that bug is the reason you
> were asked to install it.

You can install only one of the three casks at a time. To go back to the
supported build, remove the channel cask and the `hull` that came with it:

```bash
brew uninstall --cask brig@main hull@main
brew install --cask brig
```

If you remove only `brig@main`, `hull@main` stays installed. The stable `brig`
cask then asks for `hull`, which conflicts with `hull@main`.

## install.sh

Prerequisites: `curl`, `tar`, and either `sha256sum` or `shasum` on `PATH`.

```bash
curl -fsSL https://raw.githubusercontent.com/brig-sh/brig/main/install.sh | sh
```

| Host | What `install.sh` installs |
| --- | --- |
| macOS | `brig` and `brigd`, plus `hull` with the `vz-runner` and `hvi` executables it drives |
| Linux | The runtime bundle, with `brig` and `brigd` from the Brig release inside it. See [Linux](#linux). |
| Both | `cosign` |

`install.sh` downloads the newest `v`-tagged release for your OS and
architecture, release candidates included. It checks every archive against a
SHA-256 checksum. It installs to `BRIG_INSTALL_DIR`, or to `/usr/local/bin`
by default. It uses `sudo` when that directory is not writable.

`install.sh` stops in two cases:

- The host has neither `sha256sum` nor `shasum`. The install stops, and does
  not skip the check.
- The host is an Intel Mac. The install stops before it writes anything,
  because there is no runtime to drive there. See
  [Platform support](#platform-support).

### The hull executables

`hull` ships as one archive that holds three executables. All three go into
the same directory, because `hull` looks for a runner next to its own
executable.

| Executable | What it is |
| --- | --- |
| `hull` | the CLI Brig drives |
| `vz-runner` | the Virtualization.framework backend |
| `hvi` | the Hypervisor.framework backend |

### cosign

`cosign` verifies the kernel, the initrd and the guest agent that every
sandbox boots, and the container image behind an agent. `install.sh` skips
`cosign` when one is already on `PATH`.

Without `cosign` on `PATH`, those checks report "no tooling". Under the
default mode, that result is not a refusal. Brig still fetches, writes and
boots the assets, and prints a warning. With `cosign` installed, you can use
`HULL_VERIFY=require` and `BRIG_VERIFY=require` on a host without Homebrew.

`cosign` is a 130 MB download, the largest one in the install. `install.sh`
pins it by version and by hash, because a cosign release cannot be verified
without cosign. The `brig` and `hull` archives are not pinned that way.

The macOS build of `cosign` is ad-hoc signed upstream, so Gatekeeper rejects
it. Gatekeeper accepts everything else that `install.sh` installs. `cosign`
runs because a `curl` download carries no quarantine attribute.

### Settings

```bash
BRIG_INSTALL_DIR=~/bin BRIG_VERSION=v0.2.0 sh install.sh
```

| Setting | What it does |
| --- | --- |
| `BRIG_INSTALL_DIR` | Sets the destination. The default is `/usr/local/bin`. |
| `BRIG_VERSION` | Pins a Brig release. The default is the newest one. |
| `HULL_VERSION` | Pins a hull release. Brig and hull are versioned independently. |
| `BRIG_INSTALL_HULL=0` | Skips hull, and leaves macOS without a runtime. |
| `BRIG_INSTALL_COSIGN=0` | Skips cosign, and leaves the boot chain unverified. |

Put `BRIG_INSTALL_DIR` on your `PATH`, because `hull` finds `cosign` there.
If the destination is not on `PATH`, the boot check reports "no tooling"
although `cosign` is installed. `install.sh` warns you in that case.

`install.sh` verifies each archive by hash. It does not check the cosign
signature on `checksums.txt`, even after it installs cosign. To check the
signature yourself, see
[Verify a downloaded release](#verify-a-downloaded-release).

`install.sh` does not install shell completions. To add them, see
[Shell completion](completions.md).

## Linux

Run `install.sh` under `sudo` for a node-wide install. This is the default,
and it needs root.

```bash
curl -fsSL https://raw.githubusercontent.com/brig-sh/brig/main/install.sh | sudo sh
```

A node-wide install serves root only. Run Brig as root afterwards, for
example `sudo brig doctor`. The containerd of this install belongs to root,
and the `nerdctl` of a normal user cannot reach it, so a run as that user
fails. To give other users their own containerd, see
[Rootless installs](#rootless-installs).

### What the bundle installs

`install.sh` installs the runtime bundle that
[brig-standalone-linux](https://github.com/NOFireAI/brig-standalone-linux)
publishes. The bundle packages `nerdctl`, containerd and `urunc` with the
monitors, the guest kernel and a private containerd, under `/var/lib/brig`.

`install.sh` pins the tag of the bundle. The bundle's own `install.sh` is a
release asset, checked against the same signed `checksums.txt` as the bundle.

The bundle carries its own `brig` and `brigd`. `install.sh` replaces them
with the ones from the Brig release, the same release it installs on macOS.
As a result, `BRIG_VERSION` selects the Brig version on Linux too. The
runtime and Brig are versioned separately.

The `brig` on `PATH` is the launcher of the bundle. The launcher sets the
environment that points Brig at the private containerd.

With the bundle, `install.sh` ignores `BRIG_INSTALL_DIR` and says so. The
binaries go into the tree of the bundle.

This runtime makes a Linux sandbox a microVM. A container shares the host
kernel, and this sandbox does not. The boundary differs from the one on
macOS. See [security.md](security.md) for what each one keeps out.

### Rootless installs

Run `install.sh` as a normal user to install everything under `$HOME`:

| Path | What goes there |
| --- | --- |
| `~/.local/share/brig` | The tree |
| `~/.local/bin` | The launchers |
| A systemd user unit | Your own containerd |

This install writes nothing outside your home and never asks for `sudo`. For
that reason, `install.sh` puts nothing in `BRIG_INSTALL_DIR` and uses the
cosign that the bundle carries.

An unprivileged install always takes the rootless bundle. The plain bundle
cannot serve an unprivileged install. It says so and installs nothing.

`BRIG_INSTALL_ROOTLESS=1` selects the rootless bundle for a *node-wide*
install. Each user can then run `brig-ctl rootless` against the shared tree.

For either rootless route, someone with root must prepare the host once:

- a subuid range
- access to `/dev/kvm` and `/dev/vhost-vsock`
- the `uidmap` package
- an AppArmor profile, on Ubuntu 24.04 and later

The installer reports the missing items before it unpacks anything. The
bundle's `docs/rootless.md` explains what each item is for.

<details>
<summary>A host that also has a node-wide install</summary>

On such a host, `/usr/local/bin/brig` is the launcher of the node-wide
install. It runs when `/usr/local/bin` comes first on `PATH`. Put
`~/.local/bin` ahead of it:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

`install.sh` checks the order after it installs the launcher. It prints this
line when another `brig` comes first.

</details>

### Boot bundles and oras

`oras` fetches the boot bundle for a `genericBoot` profile. The boot bundle
is the kernel and the `container-initrd` that let an ordinary container image
boot as a guest.

| Profiles | Need `oras` |
| --- | --- |
| `claude-code`, `codex`, `gemini`, `grok`, `opencode`, `ubuntu` (six of the eight shipped profiles, all `genericBoot`) | Yes |
| `claude-desktop`, `cursor` | No |

Without `oras` on `PATH`, a `genericBoot` run fails. The error names the
artifact to fetch by hand and the directory to put it in.

`claude-desktop` cannot run on Linux. See
[Platform support](#platform-support).

### A runtime you already have

**`BRIG_INSTALL_RUNTIME=0`** skips the bundle and installs `brig` and `brigd`
alone. Use it on a host that already has `nerdctl`, containerd and `urunc`.

- That `urunc` must be a build of the branch the bundle uses. No urunc
  release reads the boot annotations, so a `genericBoot` profile never
  becomes ready on a urunc release. See
  [What Brig requires of each](runtimes.md#what-brig-requires-of-each).
- A `genericBoot` profile also needs `oras` and `cloud-hypervisor`, which the
  bundle carries.
- [runtimes.md](runtimes.md) covers the full command surface each runtime
  needs.

**`docker`** is accepted in place of `nerdctl` for an image that carries its
own kernel. Brig refuses a `genericBoot` profile on `docker` and does not
attempt the run. `docker` does not pass the boot annotations that `urunc`
needs through to the runtime.

**`BRIG_CONTAINERD_RUNTIME`** can name another microVM shim, for a host that
has one. Brig refuses a shim that it knows shares the host kernel: `runc` or
`crun`, by name or by path. A container that shares the host kernel with the
agent is not the boundary Brig provides. `brig info` still names the shim
that a run resolves, and Brig refuses the run.

## Building from source

Prerequisites: Go 1.25.0 or newer, the minimum that `go.mod` states.

```bash
git clone https://github.com/brig-sh/brig
cd brig
make build
```

`make build` writes `brig` and `brigd` into the current directory. The build
needs no signing and no entitlement, because Brig reaches the hypervisor only
through `hull`.

A `hull` that you build from source cannot boot a sandbox without a Developer
ID certificate. See
[Building hull from source on macOS](runtimes.md#building-hull-from-source-on-macos).

## Verify the install

```bash
brig version
brig doctor
```

`brig version` prints the version you installed. `brig doctor` prints one
line per check: brig, host, virtual, runtime, boot, verify, profiles,
secrets, brigd and image. Each line is marked `ok`, `!!` or `--`.

| Mark | Meaning |
| --- | --- |
| `ok` beside `runtime` | Brig found the `hull` or `nerdctl` it drives, and where |
| `!!` beside `boot` | Normal before you run an agent. Brig fetches boot assets on first use. |
| `!!` beside any other check | The line under it names the fix |

Next: [Quickstart](quickstart.md).

## Verify a downloaded release

This step is optional. Prerequisites: cosign, and `checksums.txt`,
`checksums.txt.pem` and `checksums.txt.sig`, downloaded with the archive from
the release page.

```bash
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature checksums.txt.sig \
  --certificate-identity-regexp \
    '^https://github\.com/brig-sh/brig/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

shasum -a 256 -c checksums.txt --ignore-missing
```

The first command verifies `checksums.txt` with keyless cosign. Keyless
cosign has no key to check against. It uses a short-lived certificate bound
to the identity of the release workflow. The second command ties every
archive listed in `checksums.txt` to that verified file.

The macOS binaries carry a second, separate proof: a Developer ID signature,
notarized with Apple. Gatekeeper checks that signature, and cosign does not.

```bash
spctl -a -vv -t install "$(which brig)"   # source=Notarized Developer ID
```

[security.md](security.md) covers both checks, what each one proves, and
why Brig signs releases this way.

## Platform support

| Host | Supported |
| --- | --- |
| Mac, Apple silicon, macOS 15 or newer | Yes |
| Mac, Apple silicon, macOS 14 | Yes, with `BRIG_HYPERVISOR=vz BRIG_NETWORK=shared` |
| Intel Mac | No |
| Linux, x86-64 or arm64 | Yes, with the runtime bundle `install.sh` installs |

The project tests on macOS 26. Brig and hull do not refuse macOS 15 for
being older than 26.

### macOS 14

Brig enforces macOS 15 as the minimum for the `hvi` backend of hull. `hvi`
depends on an in-kernel interrupt controller that Apple first shipped in
macOS 15. On an older macOS, Brig refuses the run, so that the virtual
machine monitor does not crash.

Six of the eight shipped profiles ask for `hvi`, so a first run on macOS 14
hits this minimum. To run them on macOS 14, set two variables:

| Setting | Why |
| --- | --- |
| `BRIG_HYPERVISOR=vz` | Selects the `vz` backend, so the run does not hit the `hvi` minimum |
| `BRIG_NETWORK=shared`, or `--network shared` on each run | Those profiles ask for isolated networking, which `vz` cannot provide |

A `shared` network does not promise to separate sandboxes. See
[Network postures](policies.md#network-postures).

Brig can refuse the run only when it can read the macOS version from the
host. If the host does not report its version, the run proceeds to the boot.

### Intel Mac

An Intel Mac has no runtime. `hull` needs Apple silicon, and it has no
published `amd64` build.

`install.sh` refuses an Intel Mac before it writes anything. Other routes do
not refuse it:

- The release publishes a `darwin/amd64` archive of `brig` and `brigd` next
  to the `arm64` one.
- The source of Brig does not check the host architecture.

As a result, a manual download or a source build of `brig` succeeds on an
Intel Mac. A run then fails at the runtime check, because Brig finds no
`hull` to drive.

### claude-desktop

`claude-desktop` is a graphical profile. It is the one built-in profile that
Linux cannot run.

| Requirement | Detail |
| --- | --- |
| macOS | The Linux runtime refuses a graphical profile, on `nerdctl` and on `docker`. The refusal names macOS as the host where the profile can run. |
| Apple silicon | The image is published for `arm64` only |
| The `vz` backend | `vz` is the only one of the three backends with a console. `hvi` and `qemu` refuse a graphical profile. |

`brig agent ls` lists `claude-desktop` on every platform and marks neither
limit.
