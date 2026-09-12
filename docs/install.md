# Install Brig

Homebrew is the path to prefer on macOS. Use `install.sh` when Homebrew is
not available.

No single path installs a full working setup everywhere. Homebrew and
`install.sh` both install `hull` on macOS. On Linux, `install.sh` installs
`brig`, `brigd` and `cosign` only: you install the runtime, `nerdctl` with
containerd and the `urunc` shim, yourself. Building from source writes only
`brig` and `brigd`, never a runtime.

## macOS with Homebrew

Prerequisites: a Mac with Apple silicon, macOS 15 or newer, and Homebrew
([brew.sh](https://brew.sh)). On macOS 14, set `BRIG_HYPERVISOR=vz` before
you run an agent. See [Platform support](#platform-support).

```bash
brew tap brig-sh/brig
brew trust brig-sh/brig
brew install --cask brig
```

This installs `brig`, `brigd` and `hull`, puts them on `PATH`, and installs the bash,
zsh and fish completions with them.

`brew trust` is required because the cask comes from a third-party tap, and
Homebrew refuses to install one that has not been trusted. If it reports
`Unknown command: trust`, run `brew update` first, then run the command
again.

During the `0.1.0-rc` series, the tap can lag the newest release. If
`brew install` gives you an older version than you expected, or fails, use
[install.sh](#installsh) instead.

## install.sh

Prerequisites: `curl`, `tar`, and either `sha256sum` or `shasum` on `PATH`.

```bash
curl -fsSL https://raw.githubusercontent.com/brig-sh/brig/main/install.sh | sh
```

This installs `brig` and `brigd` on macOS and Linux. On macOS it also
installs `hull` with the `vz-runner` and `hvi` executables it drives, and on
both platforms it installs `cosign`.

It downloads the newest release for your OS and architecture. There is no
stable release yet, so that includes prereleases. It checks every archive
against a SHA-256 checksum, and installs to `BRIG_INSTALL_DIR` or
`/usr/local/bin`. It uses `sudo` when that directory is not writable.

A host with neither `sha256sum` nor `shasum` stops the install, instead of
silently skipping the check.

An Intel Mac is refused before anything is written. The release publishes a
`darwin/amd64` archive of `brig`. `hull` drives Virtualization.framework on
Apple silicon only, and has never published an `amd64` build. There is no
runtime to drive on an Intel Mac.

`hull` ships as one archive holding three executables. All three go into the
same directory, because `hull` discovers a runner next to its own
executable:

| Executable | What it is |
| --- | --- |
| `hull` | the CLI Brig drives |
| `vz-runner` | the Virtualization.framework backend |
| `hvi` | the Hypervisor.framework backend |

`cosign` verifies the kernel, initrd and guest agent every sandbox boots,
and the container image behind an agent. Without it on `PATH`, those checks
report "no tooling". Under the default mode that is not a refusal: Brig
fetches, writes and boots the assets anyway, with a printed warning.
Installing `cosign` is what makes `HULL_VERIFY=require` and
`BRIG_VERIFY=require` usable on a host without Homebrew. `install.sh` skips
it when one is already on `PATH`.

It is a 130 MB download, by far the largest thing here. Its macOS build is
ad-hoc signed upstream, so Gatekeeper rejects it on its own, unlike
everything else `install.sh` places. It runs because a `curl` download
carries no quarantine attribute. Unlike the `brig` and `hull` archives,
`cosign` is pinned in `install.sh` by version and by hash, because its own
release cannot be verified without cosign.

### Settings

```bash
BRIG_INSTALL_DIR=~/bin BRIG_VERSION=v0.1.0-rc18 sh install.sh
```

- `BRIG_INSTALL_DIR` overrides the destination. Unset, it installs to
  `/usr/local/bin`. Put it on your `PATH`. `hull` finds `cosign` there, and
  a destination that is not on `PATH` leaves the boot check reporting "no
  tooling" even though the binary is installed. `install.sh` warns when
  this is the case.
- `BRIG_VERSION` pins a Brig release instead of fetching the newest one.
- `HULL_VERSION` does the same for hull. The two are versioned
  independently.
- `BRIG_INSTALL_HULL=0` skips hull, and leaves macOS without a runtime.
- `BRIG_INSTALL_COSIGN=0` skips cosign, and leaves the boot chain
  unverified.

`install.sh` does not check the cosign signature on `checksums.txt`, even
after installing cosign: the archive is already verified by hash. See
[Verify a downloaded release with cosign](#verify-a-downloaded-release-with-cosign)
to check the signature yourself.

`install.sh` does not install shell completions. See
[completions.md](completions.md) for how to add them.

## Linux

Prerequisites: `nerdctl`, containerd, and the `urunc` containerd shim. A
`genericBoot` profile also needs `oras`.

Brig drives `nerdctl` over containerd, with `urunc` as the shim that boots
the container as a microVM instead of a plain process. `install.sh` does
not install any of this on Linux: it installs `brig`, `brigd` and `cosign`
only. Install `nerdctl`, containerd and `urunc` yourself before you run an
agent. [runtimes.md](runtimes.md) covers the full command surface each one
needs.

That combination is what makes a Linux sandbox a microVM rather than a
container sharing the host kernel. What that boundary does and does not
keep out is not identical to macOS: [security.md](security.md) covers the
difference.

`docker` is accepted in `nerdctl`'s place for an image that carries its own
kernel. A `genericBoot` profile is refused on `docker` rather than
attempted: six of the eight shipped profiles are `genericBoot`. `docker`
does not pass the boot annotations `urunc` needs through to the runtime.

`BRIG_CONTAINERD_RUNTIME=runc` asks for a plain container instead, using
`runc` directly:

```bash
BRIG_CONTAINERD_RUNTIME=runc brig run claude
```

A `runc` sandbox shares the host kernel with the agent, instead of running
it in its own microVM. That is a weaker boundary, and `brig info` reports
which one a run got.

`oras` fetches the boot bundle, the kernel and `container-initrd` that let
an ordinary container image boot as a guest, for a `genericBoot` profile.
Six of the eight shipped profiles need it: `claude-code`, `codex`,
`gemini`, `grok`, `opencode` and `ubuntu`. `claude-desktop` and `cursor` do
not. Without `oras` on `PATH`, a `genericBoot` run fails, naming the exact
artifact to fetch by hand and the directory to put it in.

`claude-desktop` cannot run on Linux at all. [Platform support](#platform-support)
below covers why.

## Building from source

Prerequisites: Go 1.25.0 or newer, the floor `go.mod` states.

```bash
git clone https://github.com/brig-sh/brig
cd brig
make build
```

`make build` writes `brig` and `brigd` into the current directory. Building
Brig from source needs no signing and no entitlement: Brig reaches the
hypervisor only by shelling out to `hull`, never directly.

A from-source `hull` cannot boot a sandbox without a Developer ID
certificate. See
[runtimes.md#building-hull-from-source-on-macos](runtimes.md#building-hull-from-source-on-macos)
for what that needs and why.

## Verify the install

```bash
brig version
brig doctor
```

`brig version` prints the version you installed. `brig doctor` prints one
line per check: host, virtual, runtime, boot, verify, profiles, secrets,
brigd and image. Each line is marked `ok`, `!!` or `--`.

`ok` beside `runtime` means Brig found the `hull` or `nerdctl` it drives,
and where. `!!` beside `boot` is normal before you run an agent: Brig
fetches boot assets on first use. Anything else marked `!!` names the fix
beside it.

Next: [quickstart.md](quickstart.md).

## Verify a downloaded release with cosign

Optional. Prerequisites: cosign, and `checksums.txt`, `checksums.txt.pem`
and `checksums.txt.sig`, downloaded alongside the archive from the release
page.

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

The first command vouches for `checksums.txt` with keyless cosign: no key
to check against, a short-lived certificate bound to the release
workflow's identity instead. The second command ties every archive listed
in `checksums.txt` to the file the first command already vouched for.

The macOS binaries carry a second, separate proof: a Developer ID
signature, notarized with Apple. Gatekeeper checks that one, not cosign's.

```bash
spctl -a -vv -t install "$(which brig)"   # source=Notarized Developer ID
```

[security.md](security.md) covers both checks, what each one proves, and
why Brig signs releases this way.

## Platform support

| Host | Supported |
| --- | --- |
| Mac, Apple silicon, macOS 15 or newer | Yes |
| Mac, Apple silicon, macOS 14 | Yes, with `BRIG_HYPERVISOR=vz` |
| Intel Mac | No |
| Linux, x86-64 or arm64 | Yes, with `nerdctl`, containerd and the `urunc` shim |

macOS 15 is the floor Brig enforces for hull's `hvi` backend. Apple shipped
the in-kernel interrupt controller `hvi` depends on first in macOS 15.
Brig refuses the run on an older one rather than let the virtual machine
monitor crash. That refusal needs a version it can read from the host. A
host that will not report its version proceeds to the boot instead of
being refused. Six of the eight shipped profiles ask for `hvi`, so a first
run on macOS 14 hits this floor unless you set `BRIG_HYPERVISOR=vz`.

macOS 26 is what the project tests on, a separate fact from the floor.
Nothing in Brig or hull refuses macOS 15 or macOS 16 for being older than
26.

Nothing in Brig's own source checks the host architecture. The release
publishes a `darwin/amd64` archive of `brig` and `brigd` alongside the
`arm64` one. `install.sh` refuses an Intel Mac before writing anything, but
a manual download or a source build of `brig` succeeds there. A run then
fails at the runtime check, because Brig finds no `hull` to drive. `hull`
needs Apple silicon, and has never published an `amd64` build.

`claude-desktop` is the one built-in profile Linux cannot run at all. It
is a graphical profile. The Linux runtime refuses a graphical profile
outright, on `nerdctl` and on `docker` alike, and names macOS as where it
can run instead.

Its image is also published for `arm64` only, so on macOS it needs Apple
silicon too. It needs the `vz` backend specifically as well. `vz` is the
only one of the three backends with a console, and a graphical profile is
refused on `hvi` and `qemu`.

`brig agent ls` lists `claude-desktop` on every platform with no marker of
either limit.
