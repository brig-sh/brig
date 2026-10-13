# Teleportable sessions

A teleportable session owns all of its storage. Its guest root is a block
disk, its guest home is a directory on that disk, and no host directory is
shared into it. Brig gives a session this shape so that a later release can
move the running sandbox to another host as a whole. This page describes only
the session mode. Brig does not move a session yet.

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
| Credentials | a `files:` binding is delivered into a tmpfs by `hull exec`; an environment variable is handed to `hull run` | the same, and an environment variable stays on the disk. See [Credentials on the disk](#credentials-on-the-disk) |
| Onboarding seed | written into the host home | written into the guest home, inside the guest |
| `--skills`, `BRIG_GIT_CONFIG` | copy files into the host home | refused: there is no host home |

`brig info` and `brig plan` print a `TELEPORTABLE` row. `brig ls` prints
`(in the sandbox disk)` in place of a workspace.
The `--json` documents of `ls`, `info` and `plan` carry `"teleportable": true`.

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

## Credentials on the disk

A credential bound as an environment variable, such as the `GH_TOKEN` that
`claude-code` forwards, is handed to `hull run` as part of the sandbox's
environment. When the image boots through urunit, hull writes that
environment, values included, into `/urunit.conf` on the root disk, with mode
`0600`. A `files:` binding is not written there. It goes into a tmpfs after
the boot, and it is gone when the sandbox stops.

An ordinary session on a `block` root has the same file in its own disk, and
`brig rm` deletes it with the sandbox. A teleportable session differs in two
ways:

- The disk is the session's storage and lives as long as the session. The
  file keeps the value the sandbox booted with after you rotate the
  credential. Each later command hands the agent the current value, but the
  file is not rewritten.
- A later release that moves the session to another host moves the disk, and
  the file with it.

To keep a credential off the disk, deliver it as a `files:` binding.

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
| A run path other than hull on `hvi` | `7` |

Each refusal names the setting to drop, or `brig rm <ref>` when the session
has to start over.

The record Brig keeps can be lost, for example when the session index file
is deleted. Brig then asks hull how the sandbox was booted before it
recreates it. hull keeps that record on the host, so the guest cannot change
the answer. A sandbox booted on a `block` root with no host directory shared
in is a teleportable session, and Brig refuses to recreate it, running or
stopped. A running one can be used again with `--teleportable`.
