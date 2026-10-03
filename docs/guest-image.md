# What a guest image has to provide

Brig boots an OCI image and then runs commands inside it. Those commands
are a readiness probe, the mounts that keep a credential off host disk,
and the file the agent reads that credential from. Any Linux CLI in an
image works under Brig as long as the image carries what those commands
need.

This page is that list. Each entry names the source file that runs it.
`script/check-guest-image.sh <image> [profile]` runs the list against a real
image and prints one line per requirement.

None of it is specific to Brig. A stock distribution image satisfies all of
it. The details matter when you build something smaller.

Guest images for the built-in profiles are open source, built in
[brig-sh/community-images](https://github.com/brig-sh/community-images).
Building your own from scratch is documented there, at
[bring-your-own-image.md](https://github.com/brig-sh/community-images/blob/main/docs/bring-your-own-image.md).
[profiles.md](profiles.md) covers the profile fields that name this image
and hand it a credential.

## The binaries

Resolved through the guest's `PATH` unless the table says otherwise.

| binary | why Brig runs it | where |
| --- | --- | --- |
| `/bin/true` | the readiness probe. The runtime reports a sandbox running as soon as the VMM starts, seconds before the in-guest agent binds its listener, so Brig execs this until it succeeds | `internal/wrap/run.go`, `waitReady` |
| `cat` | reads the marker back out of the guest home, and reads `/proc/self/mountinfo` and `/proc/swaps`. Also the body of the exec that writes a credential: `sh -c 'cat > "$1"'`, with the value on stdin | `internal/wrap/run.go`, `guestMountsWorkspace`. `internal/wrap/secretfiles.go`, `guestMountpoints`, `verifyVolumes`, `writeSecretFile` |
| `sh` | three short scripts: create a credential file with `set -C`, write the value into it, create a mount target | `internal/wrap/secretfiles.go`, `writeSecretFile`, `createGuestTarget` |
| `stat` | `stat -c '%F\|%U\|%a'` proves a credential's target is a regular file of the right ownership and mode before the value goes into it, `stat -c %s` proves it is not empty afterwards, and `stat -f -c %T` reports the filesystem a path sits on | `internal/wrap/secretfiles.go`, `verifySecretFile`, `writeSecretFile`, `guestFstype` |
| `mount` | `mount -t tmpfs` covers a directory the profile declares, and `mount --bind` pins each hostmount out of the way first and binds it back after | `internal/wrap/secretfiles.go`, `mountVolumes` |
| `mkdir` | creates mount targets, as `mkdir -p` and inside a shell script as `mkdir -p -- "$(dirname "$1")"` | `internal/wrap/secretfiles.go`, `createGuestTarget` |
| `dirname` | inside that same script. It is an external command, not a shell builtin | `internal/wrap/secretfiles.go`, `createGuestTarget` |
| `chown` | hands the covered directories to root across a credential write and back to the guest user afterwards, and sets the owner of the credential file itself | `internal/wrap/secretfiles.go`, `chownGuest`, `writeSecretFile` |
| `chmod` | sets the mode a `files:` binding declares, inside the create script | `internal/wrap/secretfiles.go`, `writeSecretFile` |
| `rm` | `rm -f --` at the credential path before creating it, so a planted symlink is removed rather than followed | `internal/wrap/secretfiles.go`, `writeSecretFile` |
| `sleep` | **Linux only.** nerdctl runs the container as `sleep infinity`, because a container exits when its command does and the sandbox has to outlive the exec that uses it | `internal/runtime/nerdctl.go`, `runArgs` |
| `bash` | `brig sh` runs `bash -l`, and `brig sh <agent> <command...>` runs `bash -lc '"$@"' bash <command...>`, and `brig sh <agent> -c '<script>'` runs `bash -lc '<script>'`, for every profile regardless of its `binary:` field | `internal/wrap/run.go`, `shellArgv` |
| the profile's `binary:` | `brig run` execs it. `claude` for claude-code, `codex` for codex, and so on | `cmd/brig/main.go`, `runAgent` |

Two of these are conditional.

`sleep` is the container command on the nerdctl path only. On the hull path,
the image's own entrypoint runs instead, so an image without `sleep` works on
macOS and fails on Linux. Treat `sleep` as required if the image is meant to
run on both.

`bash` is run only by `brig sh`, and by `brig run` on a `kind: shell` profile.
`brig sh` is how you look inside a misbehaving sandbox, so an image without
`bash` cannot be inspected that way.

**The login profile must not rewrite the positional parameters.** `brig sh`
hands the command words to bash as `$1`, `$2` and on, and `-l` sources
`/etc/profile` -- which by its own convention sources `/etc/profile.d/*.sh`
-- and then the guest user's `~/.bash_profile` or `~/.profile`, all before
the `"$@"` script runs. A top-level `shift` or `set --` in any of them
rewrites the parameters, and with them the command Brig was asked to run:
with a `shift`, `brig sh <agent> echo hi` looks for a command called `hi` and
exits 127. The `"$@"` script Brig runs cannot defend against it -- `command
"$@"` reads the same rewritten parameters -- so `brig sh` with a command breaks
while a bare `brig sh` still opens a shell. Keep `set --` and `shift` inside
a function, where bash scopes them to the call.

The home half of that is not only the image's to get right. The guest home is
a host directory Brig mounts at the profile's `guestHome`. When that path is
the guest user's `$HOME`, as `/root` is for `claude-code`, a `.bash_profile`
in the guest home is the one bash sources. One that shifts breaks `brig sh` on
an image that is otherwise fine. `ubuntu` mounts the guest home at
`/root/work`, below `$HOME`, so bash does not source a `.bash_profile` there.

Everything from `sh` down to `rm` is only run when the profile declares
`volumes:` or `files:`. A profile with neither returns before any of it
(`deliverSecretFiles` in `internal/wrap/secretfiles.go`). Such an image
needs only `/bin/true`, `cat`, `sleep` on Linux, and its own binary.
Two of the eight shipped profiles declare them, `claude-code` and
`claude-desktop`, which are the two that deliver a credential as a file. If
your profile forwards everything as environment variables, most of this table
does not apply to it.

If you build against that narrower list, adding a `files:` binding later
brings the whole table back. The run then fails at delivery, after the
sandbox has booted.

`stat` has the strictest requirement. The `-c` and `-f` format flags are the GNU
coreutils spelling, which busybox also implements when it was compiled with
its format feature. BSD `stat` spells them differently and fails the run.
Right after Brig creates a file, `%F` has to answer `regular file` or
`regular empty file`, `%U` the owner's name, and `%a` the octal mode.
`-f -c %T` has to answer the filesystem type as a word, `tmpfs` for what
Brig mounted.

## Paths and kernel interfaces

- **`/proc`, mounted.** `cat /proc/self/mountinfo` is how Brig decides what is
  already a mountpoint. It reads the kernel's own table rather than running
  `mountpoint -q`, because a minimal image can lack that binary. A
  missing binary reads as "not mounted", stacking a second mount on
  every run until the guest runs out. `cat /proc/swaps` is the swap tripwire.
  More than a header line there, and Brig refuses to hand the sandbox a
  credential. A tmpfs page that reached swap is a credential on a disk
  Brig never wrote to.
- **`/run`, writable by root.** Each hostmount is pinned at
  `/run/brig/persist/<escaped path>` while the tmpfs goes over its real
  location, then bound back. `/run` keeps the pin off the workspace, so it
  never reaches host disk and never survives a boot. `claude-code` runs the
  guest as root, so the agent can reach the pin. See
  `persistRoot` in `internal/wrap/secretfiles.go`.
- **`/bin/true`, at that literal path.** The probe is not `true` resolved
  through `PATH`.
- **tmpfs in the guest kernel**, accepting `size=`, `mode=0700`, `nodev` and
  `nosuid`. Not ramfs: ramfs ignores `size=` silently, which lets a guest
  process exhaust the sandbox's memory through a mount Brig created. See `TmpfsOptions`
  in `internal/profile/volumes.go`.
- **No swap.** There is none in the guest today, so this is a tripwire rather
  than a mitigation. A guest that turns swap on stops the run.

## The user the image runs as

Brig derives the guest account from the profile's `guestHome:`, taking the
last path element: `/home/claude` means the user `claude` (`GuestUser` in
`internal/profile/profile.go`). The profile states the home once and the user
follows from it. There is no field to set the user separately.

Five of the six shipped agent profiles set `guestHome: /root`, so the guest
is root: `claude-code`, `codex`, `gemini`, `grok` and `opencode`. A rootless
Linux install maps container uid 0 to the invoking user, which is what lets
the guest open `/dev/kvm` and own the workspace it writes. Each of those
specs carries the rationale next to the field. The sixth, `cursor`, uses
`/home/cursor`.

`ubuntu`'s `guestHome` is `/root/work`, so the derived name is `work`, which
is not an account in that image. Nothing reads it there, because the image
already runs as `root`. The derivation only matters for a profile whose guest
home sits inside a real user's home directory.

Three things follow for the image.

**That account has to exist in the image.** The name is passed to `chown`
inside the guest, so it has to resolve in `/etc/passwd`. An image whose home
directory is `/home/claude` but which has no `claude` user fails the run with
`could not hand /home/claude/.claude to claude in the sandbox`.

**The image must run as that account.** Every exec except the privileged
ones goes in with no `-u` flag, so the image's configured `USER` decides who
the agent runs as. Set it to the guest user, and set its home to `guestHome`.

**Root has to be available to exec as.** The mounting and file-writing execs
carry `User: "root"` (`guestRootUser` in `internal/wrap/secretfiles.go`). Only
the mount syscall needs the privilege.

On a profile whose guest is root, the agent and those execs are the same
account. The boundary is the VM, not the guest account: Brig claims nothing
about what the agent can reach inside the sandbox. A profile whose `guestHome`
sits under a real user's home keeps the two apart, and the symlink guards in
`writeSecretFiles` cover that case.

That privilege comes from how Brig boots the image. A bare
`hull run <image>` or `nerdctl run <image>` starts the image as an ordinary
container. Root inside it holds only the runtime's default capability set,
with no `CAP_SYS_ADMIN`, so `mount -t tmpfs` fails there with
`permission denied`. Brig never boots that way. It passes the profile's
hypervisor and rootfs type, and for a `genericBoot` profile the kernel and
initrd annotations, so the image comes up as a microVM in which root has
every capability (`runArgs` in `internal/runtime/hull.go`, and the nerdctl
equivalent in `internal/runtime/nerdctl.go`). Checking an image under a bare
container run reports failures that Brig's own boot does not have. That is
why `script/check-guest-image.sh` boots through `brig run -d`.

## What `genericBoot` changes

A profile with `genericBoot: true` says its image was never built to be a
guest: no kernel inside it, no urunc metadata. Six of the eight shipped
profiles set it, `ubuntu` included, which boots
`docker.io/library/ubuntu:latest` unmodified. `claude-desktop` and `cursor`
leave it off.

Brig supplies the kernel and initrd, and passes them as two OCI annotations:

```
com.urunc.unikernel.bootKernel
com.urunc.unikernel.bootInitrd
```

hull takes them on its command line and urunc reads them out of the
container's OCI spec. The pair is the same on both operating systems
(`internal/runtime/boot.go`). On Linux that takes the urunc the runtime
bundle builds, because no urunc release reads the pair
([runtimes.md](runtimes.md#what-brig-requires-of-each)). They are never read
from the image's own metadata, which is what stops an image nominating a file
on the host.

The kernel file is named `Image` on arm64 and `bzImage` on x86_64. The
initrd is `container-initrd` on both. They come from `BRIG_BOOT_ASSETS` if
it is set, otherwise from whatever `hull assets dir` reports on macOS, or
`$XDG_DATA_HOME/brig/assets` (default `~/.local/share/brig/assets`) on Linux.
The Linux runtime bundle sets `BRIG_BOOT_ASSETS` to the pair it carries.
If they are missing, Brig fetches them: through hull on macOS, and with
[`oras`](install.md#linux) on Linux, where hull does not build. A zero-length
file counts as missing. Brig does not fetch into a directory that
`BRIG_BOOT_ASSETS` names, so it never downloads over a build you are working
on.

Two consequences for the image itself:

- **It does not have to carry a guest agent.** The agent comes out of the
  initrd and is copied into the guest, which lets Brig exec into a stock
  image.
- **On Linux, docker is refused for a `genericBoot` profile.** docker does
  not carry annotations through to the runtime, so a sandbox booted through
  it would have no kernel. Use nerdctl, or point `BRIG_RUNTIME_BIN` at it
  (`internal/runtime/nerdctl.go`).

An image that carries its own kernel and urunc metadata leaves `genericBoot`
off and needs none of the above.

## The tmpfs mounts Brig creates

One per `kind: tmpfs` entry in the profile's `volumes:`, at `guestHome` plus
the entry's path, with options `size=<size>,mode=0700,nodev,nosuid` and a
default size of `64m`. This is what keeps a credential off host disk. The
guest home is a host directory, and a tmpfs over part of it is a region
with no path to the host at all.

How they are mounted differs by runtime. The image needs nothing extra for
either, but the difference explains what you see inside the guest:

- **hull** has no create-time tmpfs, so Brig mounts them with a privileged
  exec, in three phases. It pins every hostmount that sits under a directory
  about to be covered, mounts the tmpfs, then binds the pins back in through
  it. Any other order loses the state it was meant to keep.
- **nerdctl** gets them in the create request instead, as `--tmpfs` and `-v`.
  A container runtime has no privileged exec to mount with
  (`createTimeVolumes` in `internal/wrap/secretfiles.go`).

Either way, Brig verifies the result from inside the guest. Each covered
directory must read as `tmpfs`, each hostmount must not, and
a guest that cannot answer fails the run. The covered directory stays
root-owned until every credential is written, and is handed to the guest user
last. There is no window in which the agent can plant a symlink at a
credential's path.

## What `volumes:` and `files:` assume of the image

Both are relative to `guestHome`, and neither can escape it.

**The mount covers the guest home entirely.** Anything the image
ships inside `guestHome` is invisible once the sandbox is up, because a host
directory is mounted over it. Dotfiles baked into `/root` at image
build time never appear. Put them somewhere else, or have the agent create
them on first run.

**`volumes:` targets do not have to exist in the image.** Brig creates the
host-side path in the guest home before the sandbox is created. It goes
through an `os.Root`, so a planted symlink is refused rather than written
through. It also creates the guest-side target under a directory root
owns. What it will not do is change the kind of something already there.
A bind mount onto the wrong kind of target fails, so a directory in the
guest home where the profile says `file: true` stops the run with an error
that names both.

**A tmpfs entry hides whatever the guest home had under that path**, not what
the image had. The image's copy was already hidden by the mount, one
layer down. What the agent writes into the tmpfs is gone at shutdown. A
hostmount under it is how a path is carved back out and kept. Brig checks
every hostmount really is a mountpoint once the cover is on, and fails the
run if one is not. A path the agent writes to, believing it persists, can
otherwise vanish without an error.

**`files:` targets must land in a declared tmpfs.** Brig refuses a profile
whose `files:` path is not covered by one. An uncovered target writes a
credential into the guest home, which is host disk.

**A `files:` binding is an ordinary file, never a bind mount.** Agents rewrite
a credential atomically, temp file then rename, and rename onto a mountpoint
returns `EBUSY`. So the file is created, checked and filled through the three
execs described above, and the image needs a `stat` that answers them.

## A minimal image

```dockerfile
FROM alpine:3.20

# The utilities above. busybox already provides most of them; coreutils and
# util-linux are here so you do not have to know which busybox features your
# base was compiled with.
RUN apk add --no-cache bash coreutils util-linux

RUN adduser -D -h /home/mine mine
COPY mine /usr/local/bin/mine

USER mine
WORKDIR /home/mine
```

with a profile that says:

```yaml
name: mine
image: docker.io/me/mine:latest
guestHome: /home/mine
binary: mine
genericBoot: true
mem: 2048
cpus: 2
```

`mem:` and `cpus:` are required. Brig refuses a profile that leaves either
at zero.

Then check the image, naming the profile it will run under. Brig has to know
that profile, so import it first:

```bash
brig agent import mine.yaml
script/check-guest-image.sh docker.io/me/mine:latest mine
```

Debian and Ubuntu bases pass as they ship: bash, coreutils and util-linux are
all in the base. Alpine's busybox provides every name on the list too.
Whether a given busybox was compiled with the `stat` format flags Brig
parses depends on the build. The two extra packages above remove that
doubt, and the script confirms it.

## What a scratch image is missing

`FROM scratch` with one static binary is missing the entire list. In rough
order of what you hit:

- **On Linux, the container exits immediately.** Brig runs it as
  `sleep infinity` and there is no `sleep`, so the sandbox is gone before the
  first probe.
- **The readiness probe never passes.** No `/bin/true`, so Brig waits out
  `BRIG_READY_TIMEOUT` and reports `sandbox did not become ready`, which says
  nothing about the image.
- **The stale-share check cannot run.** No `cat`, so Brig cannot read the
  marker back and treats the guest as not mounting this guest home.
- **No credential is delivered.** No `sh`, `stat`, `mount`, `mkdir`,
  `dirname`, `chown`, `chmod` or `rm`, so nothing between the tmpfs and the
  credential file happens.
- **`chown` has no name to resolve.** No `/etc/passwd`, so the guest user does
  not exist even if the binaries did.
- **`brig sh` cannot get you in to look.** No `bash`.

Distroless images fail the same way. The `static` and `base` variants carry
no shell and no coreutils, so every point above applies except the
`/etc/passwd` one: they ship that file with a `nonroot` user. The `:debug`
variants add a busybox shell, which still does not cover the whole list.

To run a static binary in a small image, put it in a distribution base. That
costs a few megabytes of userland. A `scratch` image fails with
`sandbox did not become ready` and nothing more specific.

## Checking an image

```bash
script/check-guest-image.sh <image> [profile]
```

The profile defaults to `claude-code`, and naming one is how the check gets
the boot the contract describes. The script boots the image with
`BRIG_IMAGE=<image> brig run -d <profile>@image-check`, in a scratch
guest home, and tears it down with `brig rm` when it is done. Going through
Brig is what supplies the hypervisor, the rootfs type, the generic-boot
annotations and the capabilities described above. A bare `hull run` or
`nerdctl run` supplies none of them and fails the mount line on an image
that works.

The profile also decides what is checked. Its `guestHome:` is the guest home.
Its last path element is the user `chown` has to resolve, and its `binary:` is
the agent CLI the last line looks for. The check is of this image under
this profile. Import a profile of your own with `brig agent import` first.

The requirements themselves run as root through the runtime's own exec against
the sandbox Brig created, one exec per line of the table above. They test
behavior, not only that a file is present. An image that will not come up
falls back to listing the image filesystem, and says so: those checks are
presence only. `BRIG_VERIFY=off` is set for the boot, so an image nobody
has signed is not refused on the way in.

The script is not part of CI, which has no registry access and no runtime
to boot with. Run it yourself against an image you are building. It is a
real run of the profile: the credentials that profile delivers are delivered
into the image under test.

Exit status `0` means every requirement is met. Exit status `1` means
something is missing. Exit status `2` means there was nothing to check
with: no `brig`, no runtime, or no such profile. The script never reports
a pass it did not perform. Set `BRIG_DRY_RUN=1` to print what it plans to
boot and stop before booting it. This checks the argument handling and the
profile lookup on a machine with no runtime.
