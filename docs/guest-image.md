# Guest image requirements

Any Linux CLI in an image works under Brig when the image carries the items
listed here. A stock distribution image has all of them. The details matter
when you build a smaller image.

Brig boots an OCI image and runs commands inside it: a readiness probe, the
mounts that keep a credential off host disk, and the file that the agent
reads that credential from.

`script/check-guest-image.sh <image> [profile]` runs the list against a real
image and prints one line per requirement. See
[Checking an image](#checking-an-image).

- Guest images for the built-in profiles are open source, built in
  [brig-sh/community-images](https://github.com/brig-sh/community-images).
- [bring-your-own-image.md](https://github.com/brig-sh/community-images/blob/main/docs/bring-your-own-image.md)
  in that repository documents how to build your own from scratch.
- [profiles.md](profiles.md) covers the profile fields that name this image
  and hand it a credential.

## The binaries

Brig finds each binary through the guest's `PATH`, unless the table gives a
path.

| binary | why Brig runs it |
| --- | --- |
| `/bin/true` | The readiness probe. Brig runs it until it succeeds |
| `cat` | Reads a marker file in the guest home, `/proc/self/mountinfo` and `/proc/swaps`. Also writes a credential, as `sh -c 'cat > "$1"'` with the value on stdin |
| `sh` | Three short scripts: create a credential file with `set -C`, write the value into it, create a mount target |
| `stat` | `stat -c '%F\|%U\|%a'` proves that the target of a credential is a regular file of the right ownership and mode before the value goes into it. `stat -c %s` proves that it is not empty afterwards. `stat -f -c %T` reports the filesystem that a path sits on |
| `mount` | `mount -t tmpfs` covers a directory that the profile declares. `mount --bind` keeps each hostmount in place under it |
| `mkdir` | Creates mount targets, as `mkdir -p` and inside a shell script as `mkdir -p -- "$(dirname "$1")"` |
| `dirname` | Inside that same script. It is an external command and not a shell builtin |
| `chown` | Hands the covered directories to root across a credential write and back to the guest user afterwards. Also sets the owner of the credential file |
| `chmod` | Sets the mode that a `files:` binding declares |
| `rm` | `rm -f --` at the credential path before Brig creates it, so a planted symlink is removed and not followed |
| `sleep` | **Linux only.** The sandbox runs `sleep infinity` so that it outlives each exec |
| `bash` | `brig sh` runs `bash -l`. `brig sh <agent> <command...>` runs `bash -lc '"$@"' bash <command...>`. `brig sh <agent> -c '<script>'` runs `bash -lc '<script>'`. This applies to every profile, whatever its `binary:` field says |
| the profile's `binary:` | `brig run` execs it. `claude` for claude-code, `codex` for codex, and so on |

Not every profile needs every binary:

| binaries | when Brig runs them |
| --- | --- |
| `/bin/true`, `cat`, the profile's own binary | Every profile. A profile with no `volumes:` and no `files:` needs only these, and `sleep` on Linux |
| `sleep` | On Linux only |
| `sh`, `stat`, `mount`, `mkdir`, `dirname`, `chown`, `chmod`, `rm` | Only when the profile declares `volumes:` or `files:` |
| `bash` | Only for `brig sh`, and for `brig run` on a `kind: shell` profile |

- **`sleep`.** An image without `sleep` works on macOS and fails on Linux.
  For an image that must run on both, treat `sleep` as required.
- **`bash`.** Without `bash`, you cannot use `brig sh` to look inside a
  misbehaving sandbox.
- **`volumes:` and `files:`.** Of the shipped profiles, only `claude-code`,
  `claude-desktop` and `herdr` declare them. If your profile forwards
  everything as environment variables, most of the table does not apply to
  it.

> [!WARNING]
> If you build against the narrower list, a `files:` binding added later brings
> the whole table back. The run then fails at delivery, after the sandbox
> boots.

### `stat`

`stat` has the strictest requirement. The `-c` and `-f` format flags are the
GNU coreutils spelling. busybox implements them when compiled with its
format feature. BSD `stat` spells them differently and fails the run.

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
- `bash -l` sources `/etc/profile`, the files in `/etc/profile.d/`, and the
  guest user's `~/.bash_profile` or `~/.profile` before it runs them.
- A top-level `shift` or `set --` in any of them changes the command that
  Brig runs. With a `shift`, `brig sh <agent> echo hi` looks for a command
  called `hi` and exits 127.
- So `brig sh` with a command breaks, and a bare `brig sh` still opens a
  shell.

The guest home is a host directory that Brig mounts at the profile's
`guestHome`, so a `.bash_profile` there counts too:

| `guestHome` | effect |
| --- | --- |
| The guest user's `$HOME`, as `/root` is for `claude-code` | bash sources a `.bash_profile` in the guest home. One that shifts breaks `brig sh` on an image that is otherwise fine |
| Below `$HOME`, as `/root/work` is for `ubuntu` | bash does not source a `.bash_profile` there |

## Paths and kernel interfaces

- **`/proc`, mounted.** Brig reads `/proc/self/mountinfo` to see what is
  already mounted.
- **`/proc/swaps`.** If it holds more than a header line, Brig refuses to
  hand the sandbox a credential. A tmpfs page that reached swap is a
  credential on disk.
- **`/run`, writable by root.** Brig keeps each hostmount under `/run/brig`
  while it mounts the tmpfs over its real location.
- **`/bin/true`, at that literal path**, not resolved through `PATH`.
- **tmpfs in the guest kernel**, which accepts `size=`, `mode=0700`, `nodev`
  and `nosuid`. ramfs does not qualify. It ignores `size=`, so a guest
  process can exhaust the sandbox's memory through a mount that Brig created.
- **No swap.** A guest that turns swap on stops the run.

## The guest user

Brig takes the guest account from the last path element of the profile's
`guestHome:`. `/home/claude` means the user `claude`. There is no field to
set the user separately.

| profiles | `guestHome` | guest user |
| --- | --- | --- |
| `claude-code`, `codex`, `gemini`, `grok`, `herdr`, `opencode` | `/root` | root |
| `claude-desktop` | `/home/claude` | `claude` |
| `cursor` | `/home/cursor` | `cursor` |
| `ubuntu` | `/root/work` | The derived name is `work`, which is not an account in that image. Nothing reads it there, because the image already runs as `root` |

A rootless Linux install maps container uid 0 to the invoking user, so a root
guest can open `/dev/kvm` and own the workspace that it writes.

The image must meet three requirements:

- **That account must exist in the image.** Brig passes the name to `chown`
  inside the guest, so it must resolve in `/etc/passwd`. An image whose home
  directory is `/home/claude` but which has no `claude` user fails the run
  with `could not hand /home/claude/.claude to claude in the sandbox`.
- **The image must run as that account.** The image's `USER` decides who the
  agent runs as. Set it to the guest user, with its home at `guestHome`.
- **Root must be available to exec as.** Brig mounts and writes credential
  files as root.

On a profile whose guest is root, the agent and those privileged commands
share one account. The boundary is the VM, not the guest account. Brig
claims nothing about what the agent can reach inside the sandbox. A
profile whose `guestHome` sits under a real user's home keeps the two
accounts apart, and Brig guards that case against planted symlinks.

### Where the privilege comes from

| boot | result |
| --- | --- |
| Brig | The image comes up as a microVM in which root has every capability |
| A bare `hull run <image>` or `nerdctl run <image>` | The image starts as an ordinary container. Root inside it has no `CAP_SYS_ADMIN`, so `mount -t tmpfs` fails with `permission denied` |

Test an image through Brig, not with a bare container run, which reports
failures that Brig's boot does not have. `script/check-guest-image.sh` boots
through `brig run -d`.

## What `genericBoot` changes

A profile with `genericBoot: true` says that its image has no kernel and no
urunc metadata. An image that carries both leaves `genericBoot` off and
needs none of this section.

Seven of the nine shipped profiles set it. `ubuntu` is one of them, and it
boots `docker.io/library/ubuntu:latest` unmodified. `claude-desktop` and
`cursor` leave it off.

Brig supplies the kernel and the initrd from host files, never from the
image. See [`genericBoot`](profiles.md#genericboot) for where they live and
how Brig fetches them.

Two consequences for the image itself:

- **It does not have to carry a guest agent.** Brig copies one in from the
  initrd, which lets it exec into a stock image.
- **On Linux, Brig refuses docker for a `genericBoot` profile.** Use
  nerdctl, or point `BRIG_RUNTIME_BIN` at it. See
  [runtimes.md](runtimes.md#what-brig-requires-of-each).

## The tmpfs mounts Brig creates

Brig creates one tmpfs per `kind: tmpfs` entry in the profile's `volumes:`. It
mounts each at `guestHome` plus the entry's path, or at the entry's `at:` when
it names one, with options `size=<size>,mode=0700,nodev,nosuid` and a default
size of `64m`. `claude-code` mounts its tmpfs at `/brig/claude`.

These mounts keep a credential off host disk. The guest home is a host
directory, and a tmpfs over part of it has no path to the host. A tmpfs with
`at:` is not on the home share at all. See [`at:`](profiles.md#volumes).

The image needs nothing extra for either runtime. Brig verifies the result
from inside the guest:

- Each covered directory must read as `tmpfs`, and each hostmount must not.
- A guest that cannot answer fails the run.
- The covered directory stays root-owned until every credential is written,
  and Brig hands it to the guest user last. The agent has no window in which
  to plant a symlink at the path of a credential.

## `volumes` and `files`

Both are relative to `guestHome`, and neither can escape it.

**The mount covers the guest home entirely.** A host directory is mounted
over `guestHome`, so anything the image ships there is invisible when the
sandbox is up. Dotfiles baked into `/root` at image build time never appear.
Put them somewhere else, or have the agent create them on first run.

**`volumes:` targets do not have to exist in the image.**

- Brig creates the path in the guest home and the target in the guest. It
  refuses a planted symlink and does not write through it.
- Brig does not change the kind of something already there. A directory in
  the guest home where the profile says `file: true` stops the run with an
  error that names both.

**A tmpfs entry hides what the guest home had under that path.**

- What the agent writes into the tmpfs is gone at shutdown.
- A hostmount under the tmpfs carves a path back out and keeps it.
- Brig checks that every hostmount is mounted and fails the run if one is
  not. Otherwise a path that the agent believes persists can vanish without
  an error.

**`files:` targets must land in a declared tmpfs.** Brig refuses a profile
whose `files:` path is not covered by one. An uncovered target writes a
credential into the guest home, which is host disk.

**A `files:` binding is an ordinary file, never a bind mount.** Agents
rewrite a credential by writing a temp file and renaming it, which fails on
a mountpoint. So Brig creates, checks and fills the file through the three
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

Import the profile first, then check the image under it:

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

- **On Linux, the sandbox exits immediately.** There is no `sleep`.
- **The readiness probe never passes.** There is no `/bin/true`, so Brig waits
  out `BRIG_READY_TIMEOUT` and reports `sandbox did not become ready`. That
  message says nothing about the image.
- **Brig cannot read the guest home.** There is no `cat`, so Brig treats
  the guest as not mounting this guest home.
- **No credential is delivered.** There is no `sh`, `stat`, `mount`, `mkdir`,
  `dirname`, `chown`, `chmod` or `rm`.
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
import it with `brig agent import` first. The profile decides what the script
checks:

| profile field | what the script takes from it |
| --- | --- |
| `guestHome:` | The guest home. Its last path element is the user that `chown` must resolve |
| `binary:` | The agent CLI that the last line looks for |

How the requirements run:

- They run as root inside the sandbox that Brig created, one check per line
  of the table in [The binaries](#the-binaries).
- They test behavior, not only presence.
- If an image does not come up, the script falls back to a list of the image
  filesystem, and says so. Those checks are presence only.
- `BRIG_VERIFY=off` is set for the boot, so Brig does not refuse an image that
  nobody has signed.

> [!WARNING]
> The check is a real run of the profile. The credentials that the profile
> delivers are delivered into the image under test.

CI does not run the script. Run it yourself against an image that you are
building.

| exit status | meaning |
| --- | --- |
| `0` | Every requirement is met |
| `1` | Something is missing |
| `2` | There was nothing to check with: no `brig`, no runtime, or no such profile |

The script never reports a pass that it did not perform.

Set `BRIG_DRY_RUN=1` to print what the script plans to boot and stop
there. This checks the argument handling and the profile lookup on a
machine with no runtime.
