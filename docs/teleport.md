# Teleportable sessions

A teleportable session owns all of its storage. Its guest root is a block
disk, its guest home is a directory on that disk, and no host directory is
shared into it. Brig gives a session this shape so that a later release can
move the running sandbox to another host as a whole. This page describes the
session mode and what that move will do. Brig does not move a session yet.

## Start one

```bash
brig run --teleportable claude
```

Or set it in a profile, for every session of that profile:

```yaml
hypervisor: hvi
teleportable: true
```

A session that was started teleportable stays teleportable. Later commands on
its ref do not need the flag. Brig records the property in its session index.

## What changes

| | Ordinary session | Teleportable session |
| --- | --- | --- |
| Guest root | the runtime's default | `block`, always |
| Guest home | a host directory, shared in read-write | a directory on the sandbox's own disk |
| Project | optional, shared in at `/work/<name>` | none |
| Backend | any | hull's `hvi` only |
| Network, policy, published ports | as configured | the same |
| Credentials | a `files:` binding is delivered into a tmpfs by `hull exec`; an environment variable is handed to `hull run` and to each `hull exec` | a `files:` binding the same; an environment variable is handed to each `hull exec` only, never to `hull run`. See [Credentials stay off the disk](#credentials-stay-off-the-disk) |
| Onboarding seed | written into the host home | written into the guest home, inside the guest |
| `--skills`, `BRIG_GIT_CONFIG` | copy files into the host home | refused: there is no host home |

`brig info` and `brig plan` print a `TELEPORTABLE` row. `brig ls` prints
`(in the sandbox disk)` in place of a workspace.
The `--json` documents of `ls`, `info` and `plan` carry `"teleportable": true`.

Once hull has checkpointed the sandbox, `brig info` also prints a `TELEPORT`
row with hull's record of it: the state (`checkpointed`, `fenced`,
`restoring`, `restored-paused` or `restored`) and the checkpoint it refers
to. `brig info --json` carries the same record as a `teleport` object. Both
are absent until hull has a record, and absent when hull cannot be read.

## What you lose

- **A host-visible home.** Nothing from the host is shared in. The guest
  home lives in the sandbox's disk image, not in a host directory, so there
  is no folder on the host to open. Use `brig sh <ref>` to reach the agent's
  files. The disk image is a file in hull's store, owned by your user, so a
  process running as you can still read it.
- **A project mount.** Copy the work in from inside the sandbox, for example
  with `git clone`.
- **`brig stop`.** hull cannot start a stopped sandbox again without making
  a new one, and a new one has a new disk. So stopping a teleportable session
  ends it. The next run refuses and names `brig rm <ref>`. A host reboot
  stops the sandbox too, with the same result.
- **Changes that recreate the sandbox.** An ordinary session restarts onto a
  new sandbox when its network or policy changes. A teleportable session
  refuses instead, because the restart deletes its disk.

`brig rm` deletes the disk and everything in it.

## Credentials stay off the disk

hull writes the environment it boots a sandbox with into a file of that
sandbox, values included. An image that boots through urunit gets
`/urunit.conf` on its root disk. A generic boot gets `/urunc-env` in the
boot initrd, which hull keeps in the sandbox's instance directory. On a
teleportable session the root disk is the session's storage. It lives as
long as the session, and a later release will move it to another host. A
credential written there would keep the value the sandbox booted with after
you rotate it, and would travel with the session.

So Brig hands a teleportable session's boot no credential. An environment
credential, such as the `GH_TOKEN` that `claude-code` forwards, is left off
`hull run`. That holds whether the value comes from your shell or from
Brig's secret store, and whatever `BRIG_ENV_ARGV` says. The run still
passes `HOME`, `PATH` and the other names hull reads for itself, even when
you forward them from your shell, the literal `value:` entries of the
profile, and Brig's git settings. None of these is a credential. hull keeps
its own values of those names, of its own `HULL_` settings such as
`HULL_REGISTRY_TOKEN`, and of the proxy settings it pulls through, such as
`HTTPS_PROXY`.

The agent loses nothing. Brig starts it with `hull exec`, and every exec,
the agent's and every later `brig sh`, hands the credentials in as on any
session: by name on the command line, with the values in the environment of
the `hull exec` process. `BRIG_ENV_ARGV` changes that for an exec as it does
everywhere; see [Not in argv](security.md#not-in-argv). A process the agent
starts inherits the values. A `files:` binding goes into a tmpfs after the
boot, as before, and is gone when the sandbox stops, unless a checkpoint was
taken while it ran.

This covers what Brig hands hull. The agent holds the values, and it can
still write one into a file on the disk.

A checkpoint is not the disk. It holds the guest's memory, so it holds every
credential the guest held when it was taken: the `files:` tmpfs, and the
values in the agent's environment. hull writes it on the host, by default
under the sandbox's instance directory in a `checkpoint` directory it
creates with mode `0700`. It stays there after the sandbox stops, until
`brig rm` removes the sandbox. A checkpoint written to a directory you name
stays until you delete it.

A profile whose workload reads its credentials when the sandbox boots
cannot work this way. A `kind: gui` profile is one: its window is the boot
process, and Brig execs nothing after it. Brig refuses `teleportable: true`
on such a profile when the profile loads, and `--teleportable` on it before
anything boots. Both refusals say that the profile reads its credentials at
boot.

## Older Brig releases

Do not run a Brig release from before teleportable sessions on the ref of a
teleportable session. That release resolves the same sandbox name, finds no
guest home shared in, reads that as a stale share, and recreates the sandbox.
That deletes the disk and the guest home on it. This release refuses each of
those recreates, and an older one does not know to.

An older release that only rewrites Brig's session index does no harm. It
drops the field that records the property, and this release still reads the
entry as teleportable from its empty guest home.

## What Brig refuses

| Request | Exit code |
| --- | --- |
| `--teleportable` with a project or `--home` on the same line | `2` |
| A project, `--home` or `BRIG_WORKSPACE` for a session that is teleportable from its profile or its record | `1` |
| `--skills`, `BRIG_SKILLS`, `BRIG_GIT_CONFIG` or a `BRIG_ROOTFS_TYPE` other than `block` | `1` |
| `--teleportable` for a session that already has a host home | `1` |
| A run that would recreate a sandbox Brig has no record of, when hull's record of its boot shows a `block` root and no share | `1` |
| `--teleportable` for a profile whose workload reads its credentials at boot (`kind: gui`) | `1` |
| A run on a sandbox whose guest hull's record shows `fenced` by a checkpoint or still `restoring` | `1` |
| A run path other than hull on `hvi` | `7` |

Each refusal names the setting to drop, or `brig rm <ref>` when the session
has to start over.

A fenced guest, and one a restore has not brought back, still has its VMM
running, so the sandbox reads as up. Brig refuses to use it again rather
than run the agent in a guest whose vCPUs are stopped. The refusal names
`hull restore` and `brig rm`. A guest that hull's record shows as
`restored-paused` is not refused, because hull keeps that state on its
record after `hull resume`.

The record Brig keeps can be lost, for example when the session index file
is deleted. Brig then asks hull how the sandbox was booted before it
recreates it. hull keeps that record on the host, so the guest cannot change
the answer. A sandbox booted on a `block` root with no host directory shared
in is a teleportable session, and Brig refuses to recreate it, running or
stopped. A running one can be used again with `--teleportable`.

## What teleport will do

This release does not move a session. The next one adds a verb for it:

```bash
brig teleport <ref> --to <user>@<host>
```

The destination needs Brig, hull and hvi, reached over `ssh`. The move goes
through these steps:

1. Brig checks the session is teleportable, that the destination's hull can
   enforce the session's network policy, and that the two hosts can run the
   same machine. The second check covers the backend, the hvi build, the CPU,
   the counter frequency and the interrupt controller. A move between hosts
   that differ is refused before anything is paused.
2. hull checkpoints the guest and fences it. It pauses the guest, writes its
   memory, its vCPUs, its devices and its interrupt controller, and syncs the
   disk. From then on the source never runs again in that process: hull
   refuses to resume it. The disk is copied after this step, so a source that
   kept running would write to a disk that no longer matches the memory that
   was shipped.
3. The checkpoint and the disk are copied to the destination over `ssh`.
4. The destination's hull restores the sandbox paused.
5. The destination resumes the guest. From then on, `brig sh`, `brig stop`
   and the other verbs on that ref go to the destination host.

The invariant the steps keep is that at most one host executes the VM. The
source is fenced before anything leaves it, and the destination runs only
once it holds the whole checkpoint. If a step fails before Brig asks the
destination to resume, the destination's copy is discarded. The session is
then restored on the source from its own checkpoint, or the copy is tried
again. Brig never restores the source once it has asked the destination to
resume. A connection lost at that moment can leave the session stopped on
both hosts, with its checkpoint on both. It never leaves it running on both.

What the guest sees:

- Its processes, its memory and its open files continue. It does not boot
  again.
- Its monotonic clock continues from where it stopped, so the time the move
  took does not appear there. Its wall clock is behind by that time until
  the guest corrects it.
- Connections to the outside network may reset and reconnect.
- Every connection to the guest agent is reset. A `brig sh` open across a
  move ends, and a new one connects to the destination.

The checkpoint holds the guest's memory, and so whatever credential the
agent holds there. It leaves the host only over `ssh`, to the host you name,
and on each host it stays at rest as described in
[Credentials stay off the disk](#credentials-stay-off-the-disk).
The disk holds no credential value Brig resolved, as described in
[Credentials stay off the disk](#credentials-stay-off-the-disk).
