# Guest image requirements

Any Linux CLI in an image works under Brig when the image carries the items
listed here. None of them is specific to Brig, and a stock distribution image
satisfies all of them. The details matter when you build a smaller image.

Brig boots an OCI image and then runs commands inside it. Those commands are a
readiness probe, the mounts that keep a credential off host disk, and the file
that the agent reads that credential from. Each entry in the list names the
source file that runs it.

To run the list against a real image, use
`script/check-guest-image.sh <image> [profile]`. It prints one line per
requirement. See [Checking an image](#checking-an-image).

- Guest images for the built-in profiles are open source, built in
  [brig-sh/community-images](https://github.com/brig-sh/community-images).
- [bring-your-own-image.md](https://github.com/brig-sh/community-images/blob/main/docs/bring-your-own-image.md)
  in that repository documents how to build your own from scratch.
- [profiles.md](profiles.md) covers the profile fields that name this image
  and hand it a credential.

## The binaries

Brig resolves each binary through the guest's `PATH`, unless the table gives a
path.

| binary | why Brig runs it | where |
| --- | --- | --- |
| `/bin/true` | The readiness probe. The runtime reports a sandbox as running when the VMM starts, seconds before the in-guest agent binds its listener. So Brig execs this until it succeeds | `internal/wrap/run.go`, `waitReady` |
| `cat` | Reads the marker back out of the guest home, and reads `/proc/self/mountinfo` and `/proc/swaps`. Also the body of the exec that writes a credential: `sh -c 'cat > "$1"'`, with the value on stdin | `internal/wrap/run.go`, `guestMountsWorkspace`. `internal/wrap/secretfiles.go`, `guestMountpoints`, `verifyVolumes`, `writeSecretFile` |
| `sh` | Three short scripts: create a credential file with `set -C`, write the value into it, create a mount target | `internal/wrap/secretfiles.go`, `writeSecretFile`, `createGuestTarget` |
| `stat` | `stat -c '%F\|%U\|%a'` proves that the target of a credential is a regular file of the right ownership and mode before the value goes into it. `stat -c %s` proves that it is not empty afterwards. `stat -f -c %T` reports the filesystem that a path sits on | `internal/wrap/secretfiles.go`, `verifySecretFile`, `writeSecretFile`, `guestFstype` |
| `mount` | `mount -t tmpfs` covers a directory that the profile declares. `mount --bind` pins each hostmount out of the way first and binds it back after | `internal/wrap/secretfiles.go`, `mountVolumes` |
| `mkdir` | Creates mount targets, as `mkdir -p` and inside a shell script as `mkdir -p -- "$(dirname "$1")"` | `internal/wrap/secretfiles.go`, `createGuestTarget` |
| `dirname` | Inside that same script. It is an external command and not a shell builtin | `internal/wrap/secretfiles.go`, `createGuestTarget` |
| `chown` | Hands the covered directories to root across a credential write and back to the guest user afterwards. Also sets the owner of the credential file itself | `internal/wrap/secretfiles.go`, `chownGuest`, `writeSecretFile` |
| `chmod` | Sets the mode that a `files:` binding declares, inside the create script | `internal/wrap/secretfiles.go`, `writeSecretFile` |
| `rm` | `rm -f --` at the credential path before Brig creates it, so a planted symlink is removed and not followed | `internal/wrap/secretfiles.go`, `writeSecretFile` |
| `sleep` | **Linux only.** nerdctl runs the container as `sleep infinity`. A container exits when its command does, and the sandbox must outlive the exec that uses it | `internal/runtime/nerdctl.go`, `runArgs` |
| `bash` | `brig sh` runs `bash -l`. `brig sh <agent> <command...>` runs `bash -lc '"$@"' bash <command...>`. `brig sh <agent> -c '<script>'` runs `bash -lc '<script>'`. This applies to every profile, whatever its `binary:` field says | `internal/wrap/run.go`, `shellArgv` |
| the profile's `binary:` | `brig run` execs it. `claude` for claude-code, `codex` for codex, and so on | `cmd/brig/main.go`, `runAgent` |

Not every profile needs every binary:

| binaries | when Brig runs them |
| --- | --- |
| `/bin/true`, `cat`, the profile's own binary | A profile with no `volumes:` and no `files:` needs only these, and `sleep` on Linux |
| `sleep` | On the nerdctl path only. On the hull path, the image's own entrypoint runs instead |
| `sh`, `stat`, `mount`, `mkdir`, `dirname`, `chown`, `chmod`, `rm` | Only when the profile declares `volumes:` or `files:`. A profile with neither returns before any of them (`deliverSecretFiles` in `internal/wrap/secretfiles.go`) |
| `bash` | Only for `brig sh`, and for `brig run` on a `kind: shell` profile |

- **`sleep`.** An image without `sleep` works on macOS and fails on Linux. If
  the image must run on both, treat `sleep` as required.
- **`bash`.** `brig sh` is how you look inside a misbehaving sandbox. You
  cannot inspect an image without `bash` that way.
- **`volumes:` and `files:`.** Two of the eight shipped profiles declare them,
  `claude-code` and `claude-desktop`. They are the two that deliver a
  credential as a file. If your profile forwards everything as environment
  variables, most of the table does not apply to it.

> [!WARNING]
> If you build against the narrower list, a `files:` binding added later brings
> the whole table back. The run then fails at delivery, after the sandbox
> boots.

### `stat`

`stat` has the strictest requirement. The `-c` and `-f` format flags are the
GNU coreutils spelling. busybox also implements them when it was compiled with
its format feature. BSD `stat` spells them differently and fails the run.

| format | required answer |
| --- | --- |
| `%F`, right after Brig creates a file | `regular file` or `regular empty file` |
| `%U` | The name of the owner |
| `%a` | The octal mode |
| `-f -c %T` | The filesystem type as a word: `tmpfs` for what Brig mounted |

### The login profile

**The login profile must not rewrite the positional parameters.** Keep
`set --` and `shift` inside a function, where bash scopes them to the call.

- `brig sh` hands the command words to bash as `$1`, `$2` and on.
- `-l` sources `/etc/profile`, which by its own convention sources
  `/etc/profile.d/*.sh`. Then it sources the guest user's `~/.bash_profile` or
  `~/.profile`. All of them run before the `"$@"` script.
- A top-level `shift` or `set --` in any of them rewrites the parameters, and
  with them the command that Brig was asked to run. With a `shift`,
  `brig sh <agent> echo hi` looks for a command called `hi` and exits 127.
- The `"$@"` script that Brig runs cannot defend against it, because
  `command "$@"` reads the same rewritten parameters.
- So `brig sh` with a command breaks, and a bare `brig sh` still opens a
  shell.

The home half of this does not depend on the image alone. The guest home is a
host directory that Brig mounts at the profile's `guestHome`.

| `guestHome` | effect |
| --- | --- |
| The guest user's `$HOME`, as `/root` is for `claude-code` | bash sources a `.bash_profile` in the guest home. One that shifts breaks `brig sh` on an image that is otherwise fine |
| Below `$HOME`, as `/root/work` is for `ubuntu` | bash does not source a `.bash_profile` there |

## Paths and kernel interfaces

- **`/proc`, mounted.** `cat /proc/self/mountinfo` is how Brig decides what is
  already a mountpoint. Brig reads the kernel's own table and does not run
  `mountpoint -q`, because a minimal image can lack that binary. A missing
  binary reads as "not mounted", and that stacks a second mount on every run
  until the guest runs out.
- **`/proc/swaps`.** `cat /proc/swaps` is the swap tripwire. If it holds more
  than a header line, Brig refuses to hand the sandbox a credential. A tmpfs
  page that reached swap is a credential on a disk that Brig never wrote to.
- **`/run`, writable by root.** Brig pins each hostmount at
  `/run/brig/persist/<escaped path>` while the tmpfs goes over its real
  location, then binds it back. `/run` keeps the pin off the workspace, so it
  never reaches host disk and never survives a boot. `claude-code` runs the
  guest as root, so the agent can reach the pin. See `persistRoot` in
  `internal/wrap/secretfiles.go`.
- **`/bin/true`, at that literal path.** The probe is not `true` resolved
  through `PATH`.
- **tmpfs in the guest kernel**, which accepts `size=`, `mode=0700`, `nodev`
  and `nosuid`. ramfs does not qualify. ramfs ignores `size=` silently, which
  lets a guest process exhaust the sandbox's memory through a mount that Brig
  created. See `TmpfsOptions` in `internal/profile/volumes.go`.
- **No swap.** There is none in the guest today, so this is a tripwire and not
  a mitigation. A guest that turns swap on stops the run.

## The guest user

Brig derives the guest account from the profile's `guestHome:`. It takes the
last path element: `/home/claude` means the user `claude` (`GuestUser` in
`internal/profile/profile.go`). There is no field to set the user separately.

| profiles | `guestHome` | guest user |
| --- | --- | --- |
| `claude-code`, `codex`, `gemini`, `grok`, `opencode` | `/root` | root |
| `cursor` | `/home/cursor` | `cursor` |
| `ubuntu` | `/root/work` | The derived name is `work`, which is not an account in that image. Nothing reads it there, because the image already runs as `root` |

- Five of the six shipped agent profiles set `guestHome: /root`. `cursor` is
  the sixth.
- A rootless Linux install maps container uid 0 to the invoking user. That
  lets a root guest open `/dev/kvm` and own the workspace that it writes.
- The derivation only matters for a profile whose guest home sits inside a
  real user's home directory.

The image must meet three requirements:

- **That account must exist in the image.** Brig passes the name to `chown`
  inside the guest, so it must resolve in `/etc/passwd`. An image whose home
  directory is `/home/claude` but which has no `claude` user fails the run
  with `could not hand /home/claude/.claude to claude in the sandbox`.
- **The image must run as that account.** Every exec except the privileged
  ones goes in with no `-u` flag, so the image's configured `USER` decides who
  the agent runs as. Set it to the guest user, and set its home to
  `guestHome`.
- **Root must be available to exec as.** The mounting and file-writing execs
  carry `User: "root"` (`guestRootUser` in `internal/wrap/secretfiles.go`).
  Only the mount syscall needs the privilege.

On a profile whose guest is root, the agent and those execs are the same
account. The boundary is the VM, and the guest account is not a boundary. Brig
claims nothing about what the agent can reach inside the sandbox. A profile
whose `guestHome` sits under a real user's home keeps the two accounts apart,
and the symlink guards in `writeSecretFiles` cover that case.

### Where the privilege comes from

The privilege comes from how Brig boots the image:

| boot | result |
| --- | --- |
| Brig | Brig passes the profile's hypervisor and rootfs type. For a `genericBoot` profile, it also passes the kernel and initrd annotations. The image comes up as a microVM in which root has every capability (`runArgs` in `internal/runtime/hull.go`, and the nerdctl equivalent in `internal/runtime/nerdctl.go`) |
| A bare `hull run <image>` or `nerdctl run <image>` | The image starts as an ordinary container. Root inside it holds only the runtime's default capability set, with no `CAP_SYS_ADMIN`. So `mount -t tmpfs` fails there with `permission denied` |

Brig never boots the bare way. A check of an image under a bare container run
reports failures that Brig's own boot does not have. For that reason,
`script/check-guest-image.sh` boots through `brig run -d`.

## What `genericBoot` changes

A profile with `genericBoot: true` says that its image was never built to be a
guest: no kernel inside it, no urunc metadata. An image that carries its own
kernel and urunc metadata leaves `genericBoot` off and needs none of this
section.

Six of the eight shipped profiles set it. `ubuntu` is one of them, and it
boots `docker.io/library/ubuntu:latest` unmodified. `claude-desktop` and
`cursor` leave it off.

Brig supplies the kernel and initrd, and passes them as two OCI annotations:

```
com.urunc.unikernel.bootKernel
com.urunc.unikernel.bootInitrd
```

- hull takes them on its command line, and urunc reads them out of the
  container's OCI spec.
- The pair is the same on both operating systems (`internal/runtime/boot.go`).
- On Linux, this needs the urunc that the runtime bundle builds, because no
  urunc release reads the pair
  ([runtimes.md](runtimes.md#what-brig-requires-of-each)).
- Brig never reads them from the image's own metadata. That stops an image
  from nominating a file on the host.

The kernel file is named `Image` on arm64 and `bzImage` on x86_64. The initrd
is `container-initrd` on both. Brig looks for them here:

| case | location |
| --- | --- |
| `BRIG_BOOT_ASSETS` is set | The directory that it names. The Linux runtime bundle sets it to the pair that the bundle carries |
| macOS | The directory that `hull assets dir` reports |
| Linux | `$XDG_DATA_HOME/brig/assets` (default `~/.local/share/brig/assets`) |

If the files are missing, Brig fetches them. A zero-length file counts as
missing.

- On macOS, Brig fetches through hull.
- On Linux, where hull does not build, Brig fetches with
  [`oras`](install.md#linux).
- Brig does not fetch into a directory that `BRIG_BOOT_ASSETS` names, so it
  never downloads over a build you are working on.

Two consequences for the image itself:

- **It does not have to carry a guest agent.** The agent comes out of the
  initrd and is copied into the guest, which lets Brig exec into a stock
  image.
- **On Linux, docker is refused for a `genericBoot` profile.** docker does not
  carry annotations through to the runtime, so a sandbox booted through it has
  no kernel. Use nerdctl, or point `BRIG_RUNTIME_BIN` at it
  (`internal/runtime/nerdctl.go`).

## The tmpfs mounts Brig creates

Brig creates one tmpfs per `kind: tmpfs` entry in the profile's `volumes:`. It
mounts each at `guestHome` plus the entry's path, with options
`size=<size>,mode=0700,nodev,nosuid` and a default size of `64m`.

These mounts keep a credential off host disk. The guest home is a host
directory, and a tmpfs over part of it is a region with no path to the host.

The image needs nothing extra for either runtime. The runtime decides how Brig
mounts them, which explains what you see inside the guest:

| runtime | how the mounts are made |
| --- | --- |
| hull | hull has no create-time tmpfs, so Brig mounts them with a privileged exec, in three phases. It pins every hostmount that sits under a directory about to be covered. It mounts the tmpfs. Then it binds the pins back in through it. Any other order loses the state that the hostmount keeps |
| nerdctl | nerdctl gets them in the create request, as `--tmpfs` and `-v`. A container runtime has no privileged exec to mount with (`createTimeVolumes` in `internal/wrap/secretfiles.go`) |

On both runtimes, Brig verifies the result from inside the guest:

- Each covered directory must read as `tmpfs`, and each hostmount must not.
- A guest that cannot answer fails the run.
- The covered directory stays root-owned until every credential is written,
  and Brig hands it to the guest user last. So there is no window in which the
  agent can plant a symlink at the path of a credential.

## `volumes` and `files`

Both are relative to `guestHome`, and neither can escape it. They assume the
following of the image.

**The mount covers the guest home entirely.** Anything that the image ships
inside `guestHome` is invisible when the sandbox is up, because a host
directory is mounted over it. Dotfiles baked into `/root` at image build time
never appear. Put them somewhere else, or have the agent create them on first
run.

**`volumes:` targets do not have to exist in the image.**

- Brig creates the host-side path in the guest home before it creates the
  sandbox. It goes through an `os.Root`, so Brig refuses a planted symlink and
  does not write through it.
- Brig also creates the guest-side target, under a directory that root owns.
- Brig does not change the kind of something already there. A bind mount onto
  the wrong kind of target fails. So a directory in the guest home where the
  profile says `file: true` stops the run with an error that names both.

**A tmpfs entry hides what the guest home had under that path**, and not what
the image had. The mount already hid the image's copy, one layer down.

- What the agent writes into the tmpfs is gone at shutdown.
- A hostmount under the tmpfs carves a path back out and keeps it.
- When the cover is on, Brig checks that every hostmount is a mountpoint, and
  fails the run if one is not. Otherwise a path that the agent writes to, in
  the belief that it persists, can vanish without an error.

**`files:` targets must land in a declared tmpfs.** Brig refuses a profile
whose `files:` path is not covered by one. An uncovered target writes a
credential into the guest home, which is host disk.

**A `files:` binding is an ordinary file, never a bind mount.** Agents rewrite
a credential atomically, temp file then rename, and rename onto a mountpoint
returns `EBUSY`. So Brig creates, checks and fills the file through the three
`sh` scripts in [The binaries](#the-binaries), and the image needs a `stat`
that answers them.

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

`mem:` and `cpus:` are required. Brig refuses a profile that leaves either at
zero.

Brig must know the profile, so import it first. Then check the image, and name
the profile that it will run under:

```bash
brig agent import mine.yaml
script/check-guest-image.sh docker.io/me/mine:latest mine
```

| base | result |
| --- | --- |
| Debian, Ubuntu | Pass as they ship. bash, coreutils and util-linux are all in the base |
| Alpine | busybox provides every name on the list. Whether a given busybox was compiled with the `stat` format flags that Brig parses depends on the build. The two extra packages in the example remove that doubt, and the script confirms it |

## Scratch and distroless images

`FROM scratch` with one static binary is missing the entire list. In rough
order of what you hit:

- **On Linux, the container exits immediately.** Brig runs it as
  `sleep infinity` and there is no `sleep`, so the sandbox is gone before the
  first probe.
- **The readiness probe never passes.** There is no `/bin/true`, so Brig waits
  out `BRIG_READY_TIMEOUT` and reports `sandbox did not become ready`. That
  message says nothing about the image.
- **The stale-share check cannot run.** There is no `cat`, so Brig cannot read
  the marker back and treats the guest as not mounting this guest home.
- **No credential is delivered.** There is no `sh`, `stat`, `mount`, `mkdir`,
  `dirname`, `chown`, `chmod` or `rm`, so nothing between the tmpfs and the
  credential file happens.
- **`chown` has no name to resolve.** There is no `/etc/passwd`, so the guest
  user does not exist even if the binaries did.
- **`brig sh` cannot get you in to look.** There is no `bash`.

Distroless images fail the same way:

- The `static` and `base` variants carry no shell and no coreutils. Every
  point in the list applies except the `/etc/passwd` one, because they ship
  that file with a `nonroot` user.
- The `:debug` variants add a busybox shell, which still does not cover the
  whole list.

To run a static binary in a small image, put it in a distribution base. That
costs a few megabytes of userland.

## Checking an image

```bash
script/check-guest-image.sh <image> [profile]
```

The script boots the image through Brig, in a scratch guest home, with
`BRIG_IMAGE=<image> brig run -d <profile>@image-check`. It removes the sandbox
with `brig rm` when it is done.

The profile defaults to `claude-code`. To check under a profile of your own,
import it with `brig agent import` first. The check is of this image under
this profile, and the profile decides what the script checks:

| profile field | what the script takes from it |
| --- | --- |
| `guestHome:` | The guest home. Its last path element is the user that `chown` must resolve |
| `binary:` | The agent CLI that the last line looks for |

The boot through Brig supplies the hypervisor, the rootfs type, the
generic-boot annotations and the capabilities. A bare `hull run` or
`nerdctl run` supplies none of them, and fails the mount line on an image that
works. See [Where the privilege comes from](#where-the-privilege-comes-from).

How the requirements run:

- They run as root, through the runtime's own exec, against the sandbox that
  Brig created. There is one exec per line of the table in
  [The binaries](#the-binaries).
- They test behavior, and not only that a file is present.
- If an image does not come up, the script falls back to a list of the image
  filesystem, and says so. Those checks are presence only.
- `BRIG_VERIFY=off` is set for the boot, so Brig does not refuse an image that
  nobody has signed.

> [!WARNING]
> The check is a real run of the profile. The credentials that the profile
> delivers are delivered into the image under test.

The script is not part of CI, which has no registry access and no runtime to
boot with. Run it yourself against an image that you are building.

| exit status | meaning |
| --- | --- |
| `0` | Every requirement is met |
| `1` | Something is missing |
| `2` | There was nothing to check with: no `brig`, no runtime, or no such profile |

The script never reports a pass that it did not perform.

Set `BRIG_DRY_RUN=1` to print what the script plans to boot and stop before
the boot. This checks the argument handling and the profile lookup on a
machine with no runtime.
