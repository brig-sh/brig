# Install Brig

| Host | Use | What it installs |
| --- | --- | --- |
| macOS | [Homebrew](#macos-with-homebrew) | `brig`, `brigd`, `brig-broker`, `hull`, `cosign`, shell completions |
| macOS without Homebrew | [install.sh](#installsh) | `brig`, `brigd`, `brig-broker`, `hull`, `cosign` |
| Linux | [install.sh](#linux) | the runtime bundle (`nerdctl`, containerd, the `urunc` shim), with `brig`, `brigd` and `brig-broker` from the Brig release, and `cosign` |
| Any | [A source build](#building-from-source) | `brig`, `brigd` and `brig-broker`, without a runtime |

[Platform support](#platform-support) lists the hosts Brig runs on.

## macOS with Homebrew

Prerequisites: a Mac with Apple silicon, macOS 15 or newer, and Homebrew
([brew.sh](https://brew.sh)).

```bash
brew tap brig-sh/brig
brew trust brig-sh/brig
brew install --cask brig
```

The cask puts every binary on `PATH`. `brig-broker` is the
[credential broker](broker.md#hosting) that a run with a route starts.

| If | Do this |
| --- | --- |
| `brew trust` reports `Unknown command: trust` | Run `brew update`. Then run `brew trust` again. |
| `brew install` fails, or installs an older version than you expected | Use [install.sh](#installsh). The tap can lag the newest release. |
| The Mac runs macOS 14 | Set the two variables in [Platform support](#platform-support) before you run an agent. |

### Trying something before it is released

Two more casks carry builds that are not releases.

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

If you remove only `brig@main`, the `hull@main` that stays conflicts with
the `hull` that the stable `brig` cask asks for.

## install.sh

Prerequisites: `curl`, `tar`, and either `sha256sum` or `shasum` on `PATH`.

```bash
curl -fsSL https://raw.githubusercontent.com/brig-sh/brig/main/install.sh | sh
```

| Host | What `install.sh` installs |
| --- | --- |
| macOS | `brig`, `brigd`, `brig-broker` and `hull` |
| Linux | The runtime bundle, with `brig`, `brigd` and `brig-broker` from the Brig release inside it. See [Linux](#linux). |
| Both | `cosign` |

`install.sh` downloads the newest `v`-tagged release, release candidates
included. It checks every archive against a SHA-256 checksum. It installs to
`/usr/local/bin`, and uses `sudo` when that directory is not writable.

`install.sh` stops in two cases:

- The host has neither `sha256sum` nor `shasum`.
- The host is an Intel Mac. `install.sh` stops before it writes anything.
  See [Platform support](#platform-support).

### The hull executables

`hull` ships as one archive of three executables: `hull`, `vz-runner` and
`hvi`. If you move them, keep all three in the same directory.

### cosign

`cosign` verifies the container image behind an agent. For a `genericBoot`
profile, seven of the nine shipped profiles, it also verifies the kernel, the
initrd and the guest agent that the sandbox boots. `install.sh` skips
`cosign` when one is already on `PATH`.

Without `cosign` on `PATH`, those checks report "no tooling". In the default
mode Brig prints a warning, and still fetches and boots the assets. To make
that a refusal, install `cosign` and set `HULL_VERIFY=require` and
`BRIG_VERIFY=require`.

`cosign` is a 130 MB download, the largest one in the install. `install.sh`
pins it by version and by hash.

Gatekeeper rejects the macOS build of `cosign`, which is ad-hoc signed
upstream. It runs only because a `curl` download carries no quarantine
attribute.

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

Put `BRIG_INSTALL_DIR` on your `PATH`. Otherwise the boot check reports
"no tooling" although `cosign` is installed. `install.sh` warns you.

`install.sh` does not check the cosign signature on `checksums.txt`, even
after it installs cosign. To check it yourself, see
[Verify a downloaded release](#verify-a-downloaded-release).

`install.sh` does not install shell completions. To add them, see
[Shell completion](completions.md).

## Linux

Run `install.sh` under `sudo` for a node-wide install. This is the default,
and it needs root.

```bash
curl -fsSL https://raw.githubusercontent.com/brig-sh/brig/main/install.sh | sudo sh
```

A node-wide install serves root only, so run Brig as root, for example
`sudo brig doctor`. A run as a normal user fails. To give other users their
own runtime, see [Rootless installs](#rootless-installs).

### What the bundle installs

`install.sh` installs the runtime bundle that
[brig-standalone-linux](https://github.com/NOFireAI/brig-standalone-linux)
publishes. The bundle holds the runtime, the guest kernel and a private
containerd, under `/var/lib/brig`. `install.sh` pins the tag of the bundle
and checks it against its checksums.

`install.sh` replaces the bundle's own `brig` and `brigd` with the Brig
release, and adds `brig-broker`. So `BRIG_VERSION` selects the Brig version
on Linux too.

The `brig` on `PATH` is the bundle's launcher. Do not put the bundle's bin
directory on `PATH`: a `brig` from there runs without the launcher's
environment, and Brig warns about it. See
[Started without the launcher](troubleshooting.md#started-without-the-launcher).

With the bundle, `install.sh` ignores `BRIG_INSTALL_DIR`, says so, and puts
the binaries into the bundle's tree.

A Linux sandbox is a microVM, which does not share the host kernel as a
container does. The boundary differs from the one on macOS. See
[security.md](security.md) for what each one keeps out.

### Rootless installs

Run `install.sh` as a normal user to install everything under `$HOME`:

| Path | What goes there |
| --- | --- |
| `~/.local/share/brig` | The tree |
| `~/.local/bin` | The launchers |
| A systemd user unit | Your own containerd |

This install writes nothing outside your home and never asks for `sudo`.
It ignores `BRIG_INSTALL_DIR`. An unprivileged install always takes the
rootless bundle. The plain bundle refuses one and installs nothing.

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

`install.sh` warns when another `brig` comes first on `PATH`.

</details>

### Boot bundles and oras

A host with the runtime bundle does not need `oras`. Brig boots the kernel
and initrd that the bundle carries.

On a host without the bundle, `oras` fetches the boot bundle for a
`genericBoot` profile. The boot bundle is the kernel and the
`container-initrd` that let an ordinary container image boot as a guest.

| Profiles | Need `oras` without the bundle |
| --- | --- |
| `claude-code`, `codex`, `gemini`, `grok`, `herdr`, `opencode`, `ubuntu` (seven of the nine shipped profiles, all `genericBoot`) | Yes |
| `claude-desktop`, `cursor` | No |

On such a host, a `genericBoot` run fails when the kernel and initrd are
missing and `oras` is not on `PATH`. The error names the artifact to fetch
by hand and the directory to put it in.

`claude-desktop` cannot run on Linux. See
[Platform support](#platform-support).

### A runtime you already have

**`BRIG_INSTALL_RUNTIME=0`** skips the bundle and installs `brig`, `brigd`
and `brig-broker` alone. Use it on a host that already has `nerdctl`,
containerd and `urunc`.

- That `urunc` must be a build of the branch the bundle uses. A
  `genericBoot` profile never becomes ready on a urunc release. See
  [What Brig requires of each](runtimes.md#what-brig-requires-of-each).
- A `genericBoot` profile also needs `hvi` and `oras`. Without the bundle,
  you install both yourself.
- [runtimes.md](runtimes.md) covers the full command surface each runtime
  needs.

**`docker`** is accepted in place of `nerdctl` for an image that carries its
own kernel. Brig refuses a `genericBoot` profile on `docker` and does not
attempt the run.

**`BRIG_CONTAINERD_RUNTIME`** can name another microVM shim. Brig refuses a
shim that it knows shares the host kernel: `runc`, `crun` or the runc shim
binaries `containerd-shim-runc-v1` and `-v2`, by name or by path.

## Building from source

Prerequisites: Go 1.26.0 or newer.

```bash
git clone https://github.com/brig-sh/brig
cd brig
make build
```

`make build` writes `brig`, `brigd` and `brig-broker` into the current
directory. The build needs no signing and no entitlement.

A `hull` that you build from source cannot boot a sandbox without a Developer
ID certificate. See
[Building hull from source on macOS](runtimes.md#building-hull-from-source-on-macos).

## Verify the install

```bash
brig version
brig doctor
```

`brig version` prints the version you installed. `brig doctor` prints one
line per check, marked `ok`, `!!` or `--`.

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

The first command verifies that the Brig release workflow signed
`checksums.txt`. The second command checks every archive against that file.

The macOS binaries also carry a Developer ID signature, notarized with
Apple. Gatekeeper checks it:

```bash
spctl -a -vv -t install "$(which brig)"   # source=Notarized Developer ID
```

[security.md](security.md) covers what each check proves.

## Platform support

| Host | Supported |
| --- | --- |
| Mac, Apple silicon, macOS 15 or newer | Yes |
| Mac, Apple silicon, macOS 14 | Yes, with `BRIG_HYPERVISOR=vz BRIG_NETWORK=shared` |
| Intel Mac | No |
| Linux, x86-64 or arm64 | Yes, with the runtime bundle `install.sh` installs |

The project tests on macOS 26. Brig also runs on macOS 15.

### macOS 14

The `hvi` backend needs macOS 15. On an older macOS, Brig refuses a run
on `hvi`. Seven of the nine shipped profiles ask for `hvi`. To run them on
macOS 14, set two variables:

| Setting | Why |
| --- | --- |
| `BRIG_HYPERVISOR=vz` | Selects the `vz` backend, so the run does not hit the `hvi` minimum |
| `BRIG_NETWORK=shared`, or `--network shared` on each run | Those profiles ask for isolated networking, which `vz` cannot provide |

A `shared` network does not promise to separate sandboxes. See
[Network postures](policies.md#network-postures).

### Intel Mac

An Intel Mac has no runtime, because `hull` needs Apple silicon.

`install.sh` refuses an Intel Mac before it writes anything. Other routes do
not. The release publishes a `darwin/amd64` archive of `brig` and `brigd`,
and a source build succeeds. A run then fails at the runtime check, because
Brig finds no `hull`.

### claude-desktop

`claude-desktop` is a graphical profile, and the one built-in profile that
Linux cannot run.

| Requirement | Detail |
| --- | --- |
| macOS | The Linux runtime refuses a graphical profile, on `nerdctl` and on `docker`. The refusal names macOS as the host where the profile can run. |
| Apple silicon | The image is published for `arm64` only |
| The `vz` backend | `vz` is the only one of the three backends with a console. `hvi` and `qemu` refuse a graphical profile. |

`brig agent ls` lists `claude-desktop` on every platform and marks neither
limit.
