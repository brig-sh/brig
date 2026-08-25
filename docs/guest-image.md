# What a guest image has to provide

brig boots an OCI image and then runs commands inside it: a readiness probe,
the mounts that keep a credential off host disk, the file the agent reads that
credential from. Any Linux CLI in an image works under brig as long as the
image carries what those commands need.

This page is that list, taken from the code that runs it rather than from
memory, so each entry names the file you can go and read. There is also a
script: `script/check-guest-image.sh <image>` runs the list against a real
image and prints one line per requirement.

Nothing here is a brig-specific format. A stock distribution image satisfies
all of it without being told about brig, which is the point. What follows
matters when you are building something smaller than that.

## The binaries

Resolved through the guest's `PATH` unless the table says otherwise.

| binary | why brig runs it | where |
| --- | --- | --- |
| `/bin/true` | the readiness probe. The runtime reports a sandbox running as soon as the VMM starts, seconds before the in-guest agent binds its listener, so brig execs this until it succeeds | `internal/wrap/run.go`, `waitReady` |
| `cat` | reads the workspace marker back out of the guest home, and reads `/proc/self/mountinfo` and `/proc/swaps`. Also the body of the exec that writes a credential: `sh -c 'cat > "$1"'`, with the value on stdin | `internal/wrap/run.go`, `guestMountsWorkspace`; `internal/wrap/secretfiles.go`, `guestMountpoints`, `verifyVolumes`, `writeSecretFile` |
| `sh` | three short scripts: create a credential file with `set -C`, write the value into it, create a mount target | `internal/wrap/secretfiles.go`, `writeSecretFile`, `createGuestTarget` |
| `stat` | `stat -c '%F\|%U\|%a'` proves a credential's target is a regular file of the right ownership and mode before the value goes into it, `stat -c %s` proves it is not empty afterwards, and `stat -f -c %T` reports the filesystem a path sits on | `internal/wrap/secretfiles.go`, `verifySecretFile`, `writeSecretFile`, `guestFstype` |
| `mount` | `mount -t tmpfs` covers a directory the profile declares, and `mount --bind` pins each hostmount out of the way first and binds it back after | `internal/wrap/secretfiles.go`, `mountVolumes` |
| `mkdir` | creates mount targets, as `mkdir -p` and inside a shell script as `mkdir -p -- "$(dirname "$1")"` | `internal/wrap/secretfiles.go`, `createGuestTarget` |
| `dirname` | inside that same script. It is an external command, not a shell builtin, and it is the one nobody guesses | `internal/wrap/secretfiles.go`, `createGuestTarget` |
| `chown` | hands the covered directories to root across a credential write and back to the guest user afterwards, and sets the owner of the credential file itself | `internal/wrap/secretfiles.go`, `chownGuest`, `writeSecretFile` |
| `chmod` | sets the mode a `files:` binding declares, inside the create script | `internal/wrap/secretfiles.go`, `writeSecretFile` |
| `rm` | `rm -f --` at the credential path before creating it, so a planted symlink is removed rather than followed | `internal/wrap/secretfiles.go`, `writeSecretFile` |
| `sleep` | **Linux only.** nerdctl runs the container as `sleep infinity`, because a container exits when its command does and the sandbox has to outlive the exec that uses it | `internal/runtime/nerdctl.go`, `runArgs` |
| `bash` | `brig shell` runs `bash -l`, and `brig shell <agent> '<command>'` runs `bash -lc`. It is also the `binary:` of the `ubuntu` profile | `internal/wrap/run.go`, `Shell` |
| the profile's `binary:` | `brig run` execs it. `claude` for claude-code, `codex` for codex, and so on | `cmd/brig/main.go`, `runAgent` |

Two of these are conditional, and it is worth knowing which.

`sleep` is the container command on the nerdctl path only. hull boots the
image and lets its own entrypoint run, so a macOS-only image can get away
without it and then fail on Linux. Since `:latest` on the published profiles
is a multi-arch index meant to work on both, treat `sleep` as required.

`bash` is only reached by `brig shell`. That is also the verb people reach for
when a sandbox is misbehaving, so an image without it works right up to the
moment somebody needs to look inside it.

Everything from `sh` down to `rm` is only run when the profile declares
`volumes:` or `files:`. A profile with neither returns before any of it
(`deliverSecretFiles` in `internal/wrap/secretfiles.go`), so such an image
needs `/bin/true`, `cat`, `sleep` on Linux and its own binary, and no more.
Two of the eight shipped profiles declare them, `claude-code` and
`claude-desktop`, which are the two that deliver a credential as a file. If
your profile forwards everything as environment variables, most of this table
is dead weight for you.

Build against that narrower list and know what it costs: adding one `files:`
binding later brings the whole table back, and the run then fails at delivery
rather than at boot, well after the sandbox looked fine.

`stat` is the fussiest entry. The `-c` and `-f` format flags are the GNU
coreutils spelling, which busybox also implements when it was compiled with
its format feature. BSD `stat` spells them differently and fails the run.
`%F` has to answer `regular file` or `regular empty file` for a file brig just
created, `%U` the owner's name, `%a` the octal mode, and `-f -c %T` the
filesystem type as a word, `tmpfs` for what brig mounted.

## Paths and kernel interfaces

- **`/proc`, mounted.** `cat /proc/self/mountinfo` is how brig decides what is
  already a mountpoint. It reads the kernel's own table rather than running
  `mountpoint -q` precisely because a minimal image may not carry that binary,
  and a missing binary would read as "not mounted" and stack a second mount on
  every run until the guest ran out. `cat /proc/swaps` is the swap tripwire:
  more than a header line there and brig refuses to hand the sandbox a
  credential, because a tmpfs page that reached swap is a credential on a disk
  brig never wrote to.
- **`/run`, writable by root.** Each hostmount is pinned at
  `/run/brig/persist/<escaped path>` while the tmpfs goes over its real
  location, then bound back. `/run` rather than anywhere in the workspace is
  deliberate: it is root-owned and the agent runs as an ordinary user, so
  nothing the sandbox can write to sits between a privileged mount and its
  source. See `persistRoot` in `internal/wrap/secretfiles.go`.
- **`/bin/true`, at that literal path.** The probe is not `true` resolved
  through `PATH`.
- **tmpfs in the guest kernel**, accepting `size=`, `mode=0700`, `nodev` and
  `nosuid`. Not ramfs: ramfs ignores `size=` silently, which would let a guest
  process exhaust VM memory through a mount brig created. See `TmpfsOptions`
  in `internal/profile/volumes.go`.
- **No swap.** There is none in the guest today, so this is a tripwire rather
  than a mitigation, but a guest that turns swap on stops the run.

## The user the image runs as

brig derives the guest account from the profile's `guestHome:`, taking the
last path element: `/home/claude` means the user `claude` (`GuestUser` in
`internal/profile/profile.go`). The profile states the home once and the user
follows from it. There is no field to set the user separately.

Three things follow for the image.

**That account has to exist in the image.** The name is passed to `chown`
inside the guest, so it has to resolve in `/etc/passwd`. An image whose home
directory is `/home/claude` but which has no `claude` user fails the run with
`could not hand /home/claude/.claude to claude in the sandbox`.

**The image should run as that account.** Every exec except the privileged
ones goes in with no `-u` flag, so the image's configured `USER` decides who
the agent runs as. Set it to the guest user, and set its home to `guestHome`.

**Root has to be available to exec as.** The mounting and file-writing execs
carry `User: "root"` (`guestRootUser` in `internal/wrap/secretfiles.go`). Only
the mount syscall actually needs the privilege; every target sits under a
directory root already owns, which is what keeps the rest of it unprivileged.

## What `genericBoot` changes

A profile with `genericBoot: true` says its image was never built to be a
guest: no kernel inside it, no urunc metadata. Six of the eight shipped
profiles set it, `ubuntu` included, which boots
`docker.io/library/ubuntu:latest` unmodified. `claude-desktop` and `cursor`
leave it off.

brig supplies the kernel and initrd, and passes them as two OCI annotations:

```
com.urunc.unikernel.bootKernel
com.urunc.unikernel.bootInitrd
```

hull takes them on its command line and urunc reads them out of the
container's OCI spec, so the pair is the whole contract on both operating
systems (`internal/runtime/boot.go`). They are never read from the image's own
metadata, which is what stops an image nominating a file on the host.

The kernel file is named `Image` on arm64 and `bzImage` on x86_64; the initrd
is `container-initrd` on both. They come from `BRIG_BOOT_ASSETS` if it is set,
otherwise from whatever `hull assets dir` reports on macOS, otherwise from
`$XDG_DATA_HOME/brig/assets` (default `~/.local/share/brig/assets`) on Linux.
Missing, they are fetched: with hull on macOS, which downloads the same bundle
for its own use, and with `oras` on Linux, where hull does not build. A
zero-length file counts as missing rather than passing an existence check and
failing at boot. The one case brig refuses to fix is `BRIG_BOOT_ASSETS`
pointing at a directory you chose: it will not download over a build somebody
is iterating on.

Two consequences for the image itself:

- **It does not have to carry a guest agent.** The agent comes out of the
  initrd and is copied into the guest, which is what lets brig exec into a
  stock image at all.
- **On Linux, docker is refused for a `genericBoot` profile.** docker does not
  carry annotations through to the runtime, so the sandbox would boot without
  a kernel and fail somewhere further from the cause. Use nerdctl, or point
  `BRIG_RUNTIME_BIN` at it (`internal/runtime/nerdctl.go`).

An image that carries its own kernel and urunc metadata leaves `genericBoot`
off and needs none of the above.

## The tmpfs mounts brig creates

One per `kind: tmpfs` entry in the profile's `volumes:`, at `guestHome` plus
the entry's path, with options `size=<size>,mode=0700,nodev,nosuid` and a
default size of `64m`. This is what keeps a credential off host disk: the
workspace is a host directory mounted as the guest home, and a tmpfs over part
of it is a region with no path to the host at all.

How they get there differs by runtime, and the image does not have to care,
but it explains what you will see inside the guest:

- **hull** has no create-time tmpfs, so brig mounts them with a privileged
  exec, in three phases: pin every hostmount that sits under a directory about
  to be covered, mount the tmpfs, bind the pins back in through it. Any other
  order loses the state it was meant to keep.
- **nerdctl** gets them in the create request instead, as `--tmpfs` and `-v`,
  because a container runtime has no privileged exec to mount with
  (`createTimeVolumes` in `internal/wrap/secretfiles.go`).

Either way brig verifies the result from inside the guest rather than assuming
it: each covered directory must read as `tmpfs`, each hostmount must not, and
a guest that cannot answer fails the run. The covered directory stays
root-owned until every credential is written and is handed to the guest user
last, so there is no window in which the agent could plant a symlink at a
credential's path.

## What `volumes:` and `files:` assume of the image

Both are relative to `guestHome` and neither may escape it.

**The workspace mount covers the guest home entirely.** Anything the image
ships inside `guestHome` is invisible once the sandbox is up, because a host
directory is mounted over it. Dotfiles baked into `/home/claude` at image
build time never appear. Put them somewhere else, or have the agent create
them on first run.

**`volumes:` targets do not have to exist in the image.** brig creates the
host-side path in the workspace before the sandbox is created, through an
`os.Root` so a planted symlink is refused rather than written through, and
creates the guest-side target under a directory root owns. What it will not do
is change the kind of something already there: a bind mount onto the wrong
kind of target fails, so a directory in the workspace where the profile says
`file: true` stops the run and names both, rather than surfacing later as the
kernel's own `EINVAL`.

**A tmpfs entry hides whatever the workspace had under that path**, not what
the image had: the image's copy was already hidden by the workspace mount, one
layer down. What the agent writes into the tmpfs is gone at shutdown, and a
hostmount under it is how a path is carved back out and kept. brig checks
every hostmount really is a mountpoint once the cover is on, and fails the run
if one is not, because a path the agent writes to believing it persists is the
kind of loss nobody notices until they go looking for last week's work.

**`files:` targets must land in a declared tmpfs.** That is validation, not
convention: a `files:` path not covered by a tmpfs is a refused profile,
because an uncovered target writes a credential into the workspace, which is
host disk.

**A `files:` binding is an ordinary file, never a bind mount.** Agents rewrite
a credential atomically, temp file then rename, and rename onto a mountpoint
returns `EBUSY`. So the file is created, checked and filled through the three
execs described above, and the image needs a `stat` that answers them.

## A minimal image

Small is fine. Empty is not.

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
```

Then check it rather than trusting this file:

```bash
script/check-guest-image.sh docker.io/me/mine:latest /home/mine mine
```

Debian and Ubuntu bases pass as they ship: bash, coreutils and util-linux are
all in the base. Alpine's busybox provides every name on the list too, but
whether a given busybox was compiled with the `stat` format flags brig parses
depends on the build, which is the doubt the two extra packages above remove.
Run the script rather than taking either sentence on trust.

## What a scratch image is missing

`FROM scratch` with one static binary is missing the entire list, and the
failure order is the unhelpful part. In rough order of what you would hit:

- **On Linux, the container exits immediately.** brig runs it as
  `sleep infinity` and there is no `sleep`, so the sandbox is gone before the
  first probe.
- **The readiness probe never passes.** No `/bin/true`, so brig waits out
  `BRIG_READY_TIMEOUT` and reports `sandbox did not become ready`, which says
  nothing about the image.
- **The stale-share check cannot run.** No `cat`, so brig cannot read the
  workspace marker back and treats the guest as not mounting this workspace.
- **No credential is delivered.** No `sh`, `stat`, `mount`, `mkdir`,
  `dirname`, `chown`, `chmod` or `rm`, so nothing between the tmpfs and the
  credential file happens.
- **`chown` has no name to resolve.** No `/etc/passwd`, so the guest user does
  not exist even if the binaries did.
- **`brig shell` cannot get you in to look.** No `bash`.

Distroless is the same story with a tidier base. The `static` and `base`
variants carry no shell and no coreutils, so every point above applies except
the last two: they do ship an `/etc/passwd` with a `nonroot` user, and the
missing `bash` is the least of it. The `:debug` variants add a busybox shell,
which is closer and still not the whole list.

If a static binary in a tiny image is what you want, put it in a distribution
base rather than in `scratch`. The difference is a few megabytes of userland
against a sandbox that fails with `sandbox did not become ready` and no
further clue.

## Checking an image

```bash
script/check-guest-image.sh <image> [guest-home] [binary]
```

It uses the container runtime on your `PATH`, boots the image the way brig
does, runs each requirement above as a separate exec, and prints one line per
requirement. It is not part of CI, which has no registry access, so it is
something you run yourself against an image you are building.
