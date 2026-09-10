# Install brig

This page covers every way to install brig: Homebrew and `install.sh` on
macOS, the Linux runtime, and building from source. Pick the path for your
platform, then check what brig found with `brig doctor`.

Every path installs two things: the `brig` CLI itself, and the sandbox runtime
it drives underneath, `hull` on macOS or `nerdctl` with containerd on Linux.
Both have to be present before `brig run` works.

## macOS with Homebrew

Outcome: `brig` and `hull` installed and on `PATH`.

Prerequisites: a Mac on Apple silicon, and Homebrew.

```bash
brew tap brig-sh/brig
brew trust brig-sh/brig
brew install --cask brig
```

`brew trust` is required because the cask comes from a third-party tap, and
Homebrew refuses to install one that has not been trusted. `brew trust` needs
a recent Homebrew. If it reports `Unknown command: trust`, run `brew update`
first, then try the command again.

The cask depends on the `hull` cask, so this one command installs both, and it
installs the bash, zsh and fish completions with them.

During the `0.1.0-rc` series, the casks in `brig-sh/homebrew-brig` are
maintained by hand rather than published by the release pipeline. The release
workflow opens a cask pull request only on a stable tag, and there is no
stable tag yet. So the tap can lag the newest release.

If `brew install` gives you an older version than you expected, or fails
outright, use [install.sh](#installsh) instead. Or read `Casks/brig.rb` and
`Casks/hull.rb` in the tap for what an install currently gives you.

## install.sh

Outcome: a working install without Homebrew. `brig` and `brigd` on macOS and
Linux, `hull` and its two runners on macOS, and `cosign` on both.

Prerequisites: `curl`, `tar`, and either `sha256sum` or `shasum` on `PATH`.

```bash
curl -fsSL https://raw.githubusercontent.com/brig-sh/brig/main/install.sh | sh
```

This downloads the newest release for your OS and architecture. There is no
stable release yet, so that includes prereleases. Everything it fetches is
checked against a SHA-256 before it is installed, and the destination is
`BRIG_INSTALL_DIR` or `/usr/local/bin`. It uses `sudo` when that directory is
not writable.

A host with neither `sha256sum` nor `shasum` stops the install. A checksum step
that quietly downgrades to no check is the one outcome it exists to prevent, so
this is a refusal rather than a warning.

An Intel Mac is refused before anything is written. brig itself publishes a
`darwin/amd64` archive, but `hull` drives Virtualization.framework on Apple
silicon and has never published an `amd64` build, so there would be no runtime
to drive. See [support.md](support.md).

### What it installs on macOS

`hull` comes from its own release, as the `hull-<version>-arm64.tar.gz`
archive: the same one the Homebrew cask uses. It holds three executables, and
all three go into the same directory, because `hull` discovers a runner next to
its own executable:

| Executable | What it is |
| --- | --- |
| `hull` | the CLI brig drives |
| `vz-runner` | the Virtualization.framework backend |
| `hvi` | the Hypervisor.framework backend |

`hull.dmg` on the same release page holds the same three binaries in an app
bundle, for dragging to Applications by hand. `install.sh` does not use it.

### What it installs on both

`cosign` is what verifies the kernel, initrd and guest agent every sandbox
boots, and the container image behind an agent. Without it on `PATH` those
checks report "no tooling", which under the default mode is not a refusal: the
boot assets are fetched, written and booted with a printed warning. Installing
it is what makes `HULL_VERIFY=require` and `BRIG_VERIFY=require` usable on a
host without Homebrew.

Two things to know about it. It is a 130 MB download, by far the largest thing
here. And its macOS build is ad-hoc signed upstream, so Gatekeeper rejects it
on its own -- unlike everything else `install.sh` places, which is Developer ID
signed and notarized. It runs because a `curl` download carries no quarantine
attribute. `install.sh` prints this rather than leaving you to find it.

`install.sh` skips `cosign` entirely when one is already on `PATH`.

Unlike the brig and hull archives, whose checksums come from the release that
carries them, cosign is pinned in `install.sh` by version *and* by hash. Its
own release cannot be verified without cosign: upstream publishes Sigstore
bundles and no detached signature, so there is nothing `openssl` can check.
Writing the hash down moves the trust root to a reviewed file in this
repository.

### Settings

```bash
BRIG_INSTALL_DIR=~/bin BRIG_VERSION=v0.1.0-rc18 sh install.sh
```

- `BRIG_INSTALL_DIR` overrides the destination. Unset, it installs to
  `/usr/local/bin`. Put it on your `PATH`: `hull` finds `cosign` there, and a
  destination that is not on `PATH` leaves the boot check reporting "no
  tooling" even though the binary is installed. `install.sh` warns when this
  is the case.
- `BRIG_VERSION` pins a brig release rather than fetching the newest one.
- `HULL_VERSION` does the same for hull. The two are versioned independently.
- `BRIG_INSTALL_HULL=0` skips hull, and leaves macOS without a runtime.
- `BRIG_INSTALL_COSIGN=0` skips cosign, and leaves the boot chain unverified.

`install.sh` does not check the cosign signature on `checksums.txt`, even when
it has just installed cosign: the archive is already verified by hash, and the
signature is the stronger separate claim. See
[Verifying a downloaded release with cosign](#verifying-a-downloaded-release-with-cosign)
below to check it yourself.

The archive also carries the shell completion scripts, under `completions/`.
`install.sh` does not install them for you. See [completions.md](completions.md)
for how.

## Verifying a downloaded release with cosign

Outcome: proof that a downloaded `checksums.txt`, and the archive it covers,
came from brig's own release workflow rather than somewhere else.

Prerequisites: cosign, and `checksums.txt`, `checksums.txt.pem` and
`checksums.txt.sig` downloaded alongside the archive from the release page.

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

The first command vouches for `checksums.txt` itself: brig's releases use
keyless cosign, so there is no key to check against. Instead the certificate
is short-lived, bound to the release workflow's own identity, and recorded in
a public transparency log, and `--certificate-identity-regexp` names exactly
that workflow. The second command ties every archive listed in
`checksums.txt` to the file the first command already vouched for.

The macOS binaries carry a second, separate proof: they are signed with a
Developer ID certificate and notarized with Apple. Gatekeeper checks that
one, not cosign's.

```bash
spctl -a -vv -t install "$(which brig)"   # source=Notarized Developer ID
```

[docs/security.md](security.md) covers both checks, and what each one does
and does not prove, in full.

## Linux

Outcome: a working sandbox runtime on Linux.

Prerequisites: `nerdctl`, containerd, and the `urunc` containerd shim
installed.

brig drives `nerdctl` over containerd, with `urunc` as the shim that boots the
container as a microVM instead of a plain process. That combination is what
makes a Linux sandbox a microVM rather than a container sharing the host
kernel. What that boundary does and does not keep out is not identical to
macOS: [docs/security.md](security.md) covers the difference.

`docker` is accepted in `nerdctl`'s place for an image that carries its own
kernel. A `genericBoot` profile, which is six of the eight shipped ones, is
refused on `docker` rather than attempted. `docker` does not pass the boot
annotations `urunc` needs through to the runtime. See
[runtimes.md](runtimes.md) for the full command surface each runtime needs.

`BRIG_CONTAINERD_RUNTIME=runc` asks for a plain container instead, using
`runc` directly:

```bash
BRIG_CONTAINERD_RUNTIME=runc brig run claude
```

A `runc` sandbox shares the host kernel with the agent, rather than running it
in its own microVM. That is a weaker boundary, and `brig info` reports which
one a run got.

`oras` fetches the boot bundle, the kernel and `container-initrd` that let an
ordinary container image boot as a guest, for a `genericBoot` profile. Six of
the eight shipped profiles need it: `claude-code`, `codex`, `gemini`, `grok`,
`opencode` and `ubuntu`. `claude-desktop` and `cursor` do not. Without `oras`
on `PATH`, a `genericBoot` run on Linux fails, naming the exact artifact to
fetch by hand and the directory to put it in. `claude-desktop` cannot run on
Linux at all, for a reason the [platform support matrix](#platform-support)
below covers.

## Building from source

Outcome: a `brig` and `brigd` binary built from this repository.

Prerequisites: Go 1.25.0 or newer, the floor `go.mod` states.

```bash
git clone https://github.com/brig-sh/brig
cd brig
make build
```

`make build` writes `brig` and `brigd` into the current directory.

Building brig from source needs no signing and no entitlement: brig reaches
the hypervisor only by shelling out to `hull`, never directly.

Building `hull` from source on macOS is a different matter. A from-source
`hull` cannot boot a VM at all without a Developer ID certificate for an
Apple entitlement. macOS honours that entitlement only on a binary signed
with a real Apple identity. See
[runtimes.md#building-hull-from-source-on-macos](runtimes.md#building-hull-from-source-on-macos)
for exactly what that needs and why.

The released `hull` that Homebrew installs is already signed, notarized and
stapled. `brew install --cask brig` is the path to prefer unless you are
working on `hull` itself.

## Platform support

| Host | Supported |
| --- | --- |
| Mac, Apple silicon, macOS 15 or newer | Yes |
| Mac, Apple silicon, macOS 14 | Yes, with `BRIG_HYPERVISOR=vz` |
| Intel Mac | No |
| Linux, x86-64 or arm64 | Yes, with `nerdctl`, containerd and the `urunc` shim |

macOS 15 is the floor brig enforces for hull's `hvi` backend. Apple shipped
the in-kernel interrupt controller `hvi` depends on first in macOS 15. brig
refuses the run on an older one rather than let the virtual machine monitor
crash. That refusal needs a version it can read from the host. A host that
will not report its version proceeds to the boot instead of being refused.
Six of the eight shipped profiles ask for `hvi`, so a first run on macOS 14
hits this floor unless you set `BRIG_HYPERVISOR=vz`.

macOS 26 is what the project tests on, which is a separate fact from the
floor. Nothing in brig or hull refuses macOS 15 or macOS 16 for being older
than 26.

Nothing in brig's own source checks the host architecture. The release
publishes a `darwin/amd64` archive of `brig` and `brigd` alongside the
`arm64` one, and `install.sh` installs it on an Intel Mac without a warning.
The limit sits in `hull`, the microVM runtime brig drives on macOS: it needs
Apple silicon. Installing brig on an Intel Mac succeeds. Running an agent
then fails at the runtime check, because brig finds no `hull` to drive.

`claude-desktop` is the one built-in profile Linux cannot run at all. It is a
graphical profile. The Linux runtime refuses a graphical profile outright, on
`nerdctl` and on `docker` alike, and names macOS as where it can run instead.

Its image is also published for `arm64` only, so on macOS it needs Apple
silicon too. It needs the `vz` backend specifically as well. `vz` is the only
one of the three backends with a console, and a graphical profile is refused
on `hvi` and `qemu`.

`brig agent ls` lists `claude-desktop` on every platform with no marker of
either limit.
