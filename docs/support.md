# Support matrix

What brig runs on, and how sure we are of each row. brig drives a runtime it
does not own -- hull on macOS, nerdctl on Linux -- so a host that compiles a
brig binary is not yet a host that has booted a sandbox. This page keeps those
two apart.

Every cell is tagged with how much is known about it:

- **tested** -- someone has run this and seen it work, or CI exercises it.
- **expected** -- it follows from a floor we can point at (a deployment
  target, a required symbol), but no boot has confirmed it.
- **known-broken** -- it has been seen to fail, and why.
- **unknown** -- we cannot establish it from hull's docs or brig's own, so we
  do not guess.

One fact sits under the whole page: **CI never boots a VM.** The CI workflow
runs on a Linux runner. It builds, vets, tests and runs `script/smoke.sh`
against a stub runtime, then cross-compiles for macOS and Linux. The macOS
boot is the part no Linux runner can execute, so nothing below is "tested" by
virtue of CI having booted it; where a cell says tested, it says where that
came from.

## macOS

The sandbox is a microVM booted by [hull](https://github.com/brig-sh/hull)
over Virtualization.framework. hull's two backends have different floors, so
they get a row each.

Apple Silicon is required by both backends. An Intel Mac cannot run either, so
the `darwin/amd64` binary the release builds can drive containers on Linux but
cannot boot a sandbox on a Mac.

| Backend | Minimum macOS | Architecture | Runtime | Boot status |
| --- | --- | --- | --- | --- |
| `vz` | 14 [expected] -- hull's `vz-runner` declares a deployment target of macOS 14 (`.macOS(.v14)`) | Apple Silicon [expected]; Intel [known-broken] | hull; exact working version range [unknown]. hull's own README develops and tests on macOS 26 | works on macOS 14.5 [tested], from the report in #4 where `BRIG_HYPERVISOR=vz` was the bypass that booted |
| `hvi` | 15 [known-broken below 15; expected at 15 and above] -- `hvi` calls Apple's in-kernel interrupt controller, the `hv_gic_*` family, which Apple introduced in macOS 15 | Apple Silicon [expected]; Intel [known-broken] | hull; exact working version range [unknown]. hull's own README develops and tests on macOS 26 | macOS 14 [known-broken] -- the symbols are absent and the VMM dies at start with `dyld: missing symbol called` (#4); macOS 15 and above [expected], not booted in CI |

Six of the eight shipped profiles carry `hypervisor: hvi`, so the default
first run on macOS 14 hits the broken row above. brig now refuses that run
before it starts, naming `BRIG_HYPERVISOR=vz` as the bypass, rather than
letting hull die with an unnamed symbol error.

### Image verification on macOS

| Concern | Behaviour | Status |
| --- | --- | --- |
| Digest pin | brig pins a verified image digest on hull 0.1.0-rc23 or newer, and verifies the tag on older hull versions | [expected] -- established from the hull version boundary, not from a boot in CI |

## Linux

brig drives `nerdctl` over containerd and hands the container to the
[urunc](https://github.com/urunc-dev/urunc) shim (`io.containerd.urunc.v2`),
so the sandbox is a microVM here too. `docker` works in place of `nerdctl`
where it is what is installed.

No distribution or kernel floor has been established for the Linux path. Rather
than invent one, the minimum-version cell says so.

| Item | Value | Status |
| --- | --- | --- |
| Minimum distribution / kernel | none established | [unknown] |
| Architecture | `linux/amd64` and `linux/arm64` -- guest images are published for both | [tested] for the images being published; a host boot on either arch is [expected], not exercised in CI |
| Runtime | nerdctl over containerd with the `io.containerd.urunc.v2` shim; `docker` accepted in its place | [expected] -- brig builds the command line for it; the exact runtime version range brig is known to work with is [unknown] |
| Image verification | brig verifies guest image signatures with cosign; the digest-pin boundary above is a hull fact and does not apply here | [unknown] for a Linux-specific digest boundary |

## What CI exercises

Read straight from `.github/workflows/ci.yml` and
`.github/workflows/release.yml`:

| Step | Where | Status |
| --- | --- | --- |
| `gofmt`, `go vet`, `go test -race`, `script/smoke.sh` | Linux runner, stub runtime | [tested] |
| Cross-compile `darwin/arm64` | Linux runner | [tested] as a build; it is not booted |
| Cross-compile `linux/amd64` | Linux runner | [tested] as a build; it is not booted |
| Release binaries | goreleaser builds `darwin` and `linux` for `arm64` and `amd64` | [tested] as builds; signed and notarized for macOS on the release path |
| A booted macOS sandbox | nowhere | [known-broken] as a CI claim -- no Linux runner can boot one, and self-hosted Mac capacity is reserved for work that needs a Mac |

## Installing with Homebrew

The install steps run `brew trust brig-sh/brig` before `brew install --cask
brig`, because Homebrew refuses an untrusted third-party tap. `brew trust`
needs a Homebrew new enough to carry that command; on an older Homebrew it does
not exist. Run `brew update` first so the command is there, or the tap cannot
be trusted and the cask will not install.
