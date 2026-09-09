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

Outcome: the `brig` and `brigd` binaries installed directly, without
Homebrew.

Prerequisites: `curl` and `tar` on `PATH`.

```bash
curl -fsSL https://raw.githubusercontent.com/brig-sh/brig/main/install.sh | sh
```

This downloads the newest release for your OS and architecture. There is no
stable release yet, so that includes prereleases. It checks the archive
against the published `checksums.txt` with `sha256sum` or `shasum`, then
installs `brig` and `brigd` into `BRIG_INSTALL_DIR` or `/usr/local/bin`. It
uses `sudo` when that directory is not writable.

CAUTION: on a host with neither `sha256sum` nor `shasum` on `PATH`, the
checksum check is skipped and the install proceeds anyway. If you want the
check enforced rather than skipped, install `sha256sum` or `shasum` first.

`install.sh` does not check the cosign signature on `checksums.txt`. That
check needs cosign, which `install.sh` does not require you to have. See
[Verifying a downloaded release with cosign](#verifying-a-downloaded-release-with-cosign)
below to check it yourself.

Two variables change what it does:

```bash
BRIG_INSTALL_DIR=~/bin BRIG_VERSION=v0.1.0-rc18 sh install.sh
```

- `BRIG_INSTALL_DIR` overrides the destination. Unset, it installs to
  `/usr/local/bin`.
- `BRIG_VERSION` pins a release rather than fetching the newest one.

The archive also carries the shell completion scripts, under `completions/`.
`install.sh` does not install them for you. See [completions.md](completions.md)
for how.

On macOS, if `hull` is not already on `PATH`, `install.sh` prints the Homebrew
command to install it. It does not install `hull` itself, since Homebrew is
the maintained path for a runtime that needs signing.

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
