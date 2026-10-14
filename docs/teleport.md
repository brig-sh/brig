# Teleportable sessions

A teleportable session owns all of its storage. Its guest root is a block
disk, its guest home is a directory on that disk, and no host directory is
shared into it. Brig gives a session this shape so that it can move the
running sandbox to another host as a whole, with `brig teleport`. This page
describes the session mode and the move.

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
| Machine | hvi's native machine | `portable-arm64-v1` on an arm64 host. See [The machine it boots](#the-machine-it-boots) |
| Network, policy, published ports | as configured | the same |
| Credentials | a `files:` binding is delivered into a tmpfs by `hull exec`; an environment variable is handed to `hull run` and to each `hull exec` | a `files:` binding the same; an environment variable is handed to each `hull exec` only, never to `hull run`. See [Credentials stay off the disk](#credentials-stay-off-the-disk) |
| Onboarding seed | written into the host home | written into the guest home, inside the guest |
| `--skills`, `BRIG_GIT_CONFIG` | copy files into the host home | refused: there is no host home |

`brig info` and `brig plan` print a `TELEPORTABLE` row and a `MACHINE` row.
`brig ls` prints `(in the sandbox disk)` in place of a workspace.
The `--json` documents of `ls`, `info` and `plan` carry `"teleportable": true`,
and those of `info` and `plan` carry the machine as `machine`.

Once hull has checkpointed the sandbox, `brig info` also prints a `TELEPORT`
row with hull's record of it: the state (`checkpointed`, `fenced`,
`restoring`, `restored-paused` or `restored`) and the checkpoint it refers
to. `brig info --json` carries the same record as a `teleport` object. Both
are absent until hull has a record, and absent when hull cannot be read.

## The machine it boots

On an arm64 host, a teleportable session boots hvi's `portable-arm64-v1`
machine. The guest sees the same machine on a Mac and on a Linux arm64 host:
a GICv2, a Cortex-A76 identity with a reduced feature set, and an `hvi-time`
device for its clocks and timers. A checkpoint of it restores on either kind
of host. An ordinary session boots hvi's native machine, which is the host's
own. A guest of the native machine has cached its host's counter rate, GIC
and CPU features, so its checkpoint restores only on the same kind of host.
hvi's `docs/portable-profile.md` describes the portable machine and what the
guest loses on it.

Brig hands `hull run` the flag `--profile portable-arm64-v1`, after
`--rootfs-type block`. The kernel is the only other change to the run line.
Before the run, Brig asks `hull run --help` whether this hull takes
`--profile`. A hull from before machine profiles does not, and the run is
refused with exit `7` and hull's version.

Brig hands hull the portable kernel with the boot assets, and only a profile
with `genericBoot: true` has boot assets. On an arm64 host, Brig refuses a
teleportable session of any other profile before the runtime is asked, and
`brig plan` shows the refusal. Such a profile boots its image's own kernel or
one hull picks, Brig checks neither, and a kernel without the `hvi-time`
driver has no clock on the portable machine. Of the built-in profiles,
`cursor` has no `genericBoot`. `claude-desktop` is refused for a reason of its
own: it reads its credentials at boot. A session of such a profile that
already started teleportable can still be stopped, inspected and removed, but
`brig run` and `brig sh` refuse it, and the message names `brig rm`.

The portable machine needs a kernel of its own, built with the `hvi-time`
clock driver. The boot bundle's kernel does not have that driver. Brig boots
the portable kernel with the boot bundle's initrd, and looks for the kernel
here, in this order:

1. `BRIG_PORTABLE_KERNEL`, the path of a kernel `Image`.
2. `portable-arm64-v1/Image` in the boot asset directory. This is where
   `hull assets pull --profile portable-arm64-v1` installs it, for example
   from a local file named in `HULL_PORTABLE_KERNEL`.

Nothing publishes the portable kernel yet, so Brig never downloads it. A
teleportable run with no portable kernel is refused before the boot, and the
message names `BRIG_PORTABLE_KERNEL`. Brig compares the kernel with a sha256
that Brig pins, before the boot, as described in
[The portable kernel](security.md#the-portable-kernel). The `portable` row of
`brig doctor` says whether the kernel is there and whether it is the pinned
build.

`brig info` reads the `MACHINE` row from hull's record of the sandbox. A
teleportable session that started before Brig chose the portable machine
shows `native, as hull records the boot`. That session cannot move to a host
of another kind. To move it, `brig rm` it and run it again.

On an amd64 host there is no portable machine. A teleportable session there
boots hvi's native machine, the run line names no profile, and the session
moves only to a host of the same kind.

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
long as the session, and `brig teleport` moves it to another host. A
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
| `brig teleport` of a ref with no sandbox | `3` |
| `brig teleport` of a session that is not teleportable, or not on hull's `hvi` | `7` |
| `brig teleport` of a session under an egress policy, to a host whose gateway cannot enforce one. An offline session needs no gateway, and is not refused | `7` |
| `brig teleport` that the destination cannot take: no brig, hull or hvi there, no such agent, the sandbox name or the guest's address taken, too little room, or a policy bound to the session there whose rules differ from the ones it runs under | `1` |
| `brig teleport` of a session that another `brig teleport` on this host is moving or asking about | `1` |
| `brig teleport` of a stopped or parked sandbox, one renamed with `BRIG_NAME`, or one whose network or policy changed since it booted | `1` |
| `brig teleport` with none, or more than one, of `--to`, `--status` and `--abort`, or a destination that is not `USER@HOST` | `2` |

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

## Move a session

```bash
brig teleport <ref> --to <user>@<host>
```

`brig teleport` moves the running sandbox of a teleportable session to
another host over `ssh`. The guest keeps its memory, its processes, its open
files and its disk. It does not boot again.

The destination needs Brig, hull and hvi, and an agent of the same name.
Brig reaches it as `ssh <user>@<host> brig ...`, so the account there needs
`brig` on the `PATH` that a non-interactive ssh session gets, and an ssh key
that logs in without a password prompt. `BRIG_TELEPORT_REMOTE_BRIG` names
another `brig` there.

### What a move does

1. **Checks on this host.** The session is teleportable and runs on hull's
   `hvi` backend. hull's record of the boot shows a `block` root with no host
   directory shared in. The sandbox is running, and no checkpoint or earlier
   move has parked it. `BRIG_NAME` is unset, because the brig on the
   destination derives the sandbox name from the ref alone. The network and
   the policy that Brig resolves for the session now are the ones the
   sandbox booted with, because those are the rules the move carries.
2. **The probe.** Brig runs `brig teleport-probe` on the destination over
   ssh. That command changes nothing. It reports the brig version there, the
   runtime that `BRIG_RUNTIME` resolves to, hull and hvi and their versions,
   whether hull's network gateway there takes egress rules (the same
   `hull network-gateway --help` check a boot runs), the free space in its
   hull store, whether it has the agent, whether the sandbox name and the
   guest's address are free there, and the policy that host's own files bind
   to the session. A binding there with other rules is refused: the brig
   there resolves its own binding first, and would refuse every verb on the
   moved session as a changed policy.
3. **The record.** Brig makes the move's txid and records the move in its
   session index, before hull is asked for anything.
4. **The destination takes the network.** `brig teleport-receive` on the
   destination gives the sandbox the network it had: the same isolated `/30`,
   or the same address on the shared network, or no network. It starts the
   gateway with the session's egress rules, by value, before hull resumes
   the guest there. An offline session's rules go with it as its policy,
   with no gateway, as on the source. It records the posture in that host's `networks.json`,
   and files the session in that host's index as a teleportable session.
5. **hull's dry run.** `hull migrate --dry-run` checks both hosts, including
   hull's own machine check of the destination against this host. It pauses
   nothing.
6. **The move.** `hull migrate` fences the guest here, sends its checkpoint
   and its disk over ssh, restores it paused on the destination, and resumes
   it there. See hull's `docs/migration.md` for the steps and the states.
7. **The commit.** Once hull's record says the move committed, the
   destination's brig marks the arrival finished. This host records the host
   and the hull store the session now runs in.

A refusal in steps 1 and 2 comes before anything is paused, and says why. If
steps 4 or 5 fail, Brig gives the destination's network back and drops its
record. Nothing was paused. Step 4 failing includes an answer lost on the
way back, so Brig asks for the network back even when it does not know the
destination took it.

A move holds a lock on the session on this host from step 1 to the end.
Another `brig teleport` of the same session, including `--status` and
`--abort`, refuses while it is held.

### How long it takes

The guest is paused from hull's fence until it runs on the destination.
That pause is the downtime. The guest's processes see it as a gap: a timer
that was due fires late, and a connection to the outside may reset.

hull `0021196` with hvi `8697de0` fences the guest first, and then sends
all of it: the guest's memory, and every block of its disk that holds data.
Most of that disk is the image, which the destination could have had
before the move began. The downtime grows with the guest's memory and its
disk. The `claude-code` move from a Mac to a Pi in
[Claude Code session, Mac to Pi](#claude-code-session-mac-to-pi) sent
1.8 GB while the guest was paused, for 50 to 57 s of downtime.

The streamed move (H7 in hvi and hull, being built on `feat/teleport` when
this was written) sends most of it while the guest runs. When hull boots a
teleportable sandbox, it keeps a read-only copy of the disk as built, its
base, named by its digest. `brig teleport <ref> --to <user>@<host>
--prepare` has hull put that base on the destination, with the image and
the portable kernel. A move to a host that was not prepared does this step
first, before the pause. The move then copies in rounds while the guest
runs. hvi records which pages of memory and which 64 KiB disk blocks the
guest writes. Round 0 sends the memory that is not zero and the disk blocks
that differ from the base. Each later round sends what the guest changed
during the round before. hull pauses the guest when a round is at most
8 MiB, or 2% of the guest's memory if that is more, or after 5 rounds, or
after 60 s. It then sends the last changes, the CPU state and the device
state, and the destination resumes the guest. Only that last round is
downtime. A guest that writes faster than the link can carry never gets a
small round. The cap still ends the copy, and the last round is then
larger. What a move keeps true does not change: the source is fenced before
the destination may resume, the last memory and disk changes are taken in
the same pause, and the destination checks every part against its sha256
before it runs anything.

`brig teleport` prints hull's lines on stderr as they arrive, after
`brig: hull:`: each round, the bytes it sent, the rate, and how fast the
guest dirtied pages. When the move commits, it prints how long hull's move
took, how long the guest was paused, and the bytes sent, as far as hull
reported them. `--json` carries the same in its `move` object.

| | Before: hull `0021196`, hvi `8697de0`, Mac to `rp1`, 4 runs | Target | After: streamed move, Mac to Pi |
| --- | --- | --- | --- |
| Sent while the guest runs | nothing | the base once (`--prepare`), then the rounds | pending |
| Sent while the guest is paused | 1.80 GB in 46.3 to 53.7 s, at 32.0 to 37.1 MiB/s | the last round | pending |
| hull's fence, with the capture | 1.63 to 2.08 s | | pending |
| Restore paused, then commit | 1.23 to 1.38 s, then 0.22 to 0.25 s | | pending |
| Downtime: the source pauses, to `running` on the destination | 49.95 to 57.43 s | 1 to 2 s | pending |
| `brig teleport`, start to end | 52.18 to 59.52 s | under 10 s | pending |

The before column is the four moves to `rp1` in
[Claude Code session, Mac to Pi](#claude-code-session-mac-to-pi). No
streamed move had run when this was written, so the after column is
pending.

#### Prepare the destination

```bash
brig teleport <ref> --to <user>@<host> --prepare
```

`--prepare` runs the checks of steps 1 and 2 above, and then
`hull migrate-prepare --to <user>@<host> --json <sandbox>`, with the hull
store and the hull that the destination's brig drives. hull puts the
image, the portable kernel and the sandbox's base on the destination,
each only if it is not there yet. Brig passes hull's lines on as they
arrive, then prints what hull did for each part (`held`, `sent`,
`skipped` or `failed`) and the digest of the base the destination holds.
A prepare pauses nothing, and the guest keeps running here.

Brig records the host in its session index only when hull says that a move
there will send only what the guest changed. `brig teleport --status` and
`--json` then name it, and a move to that host says it was prepared. A
committed move clears the record, because the next move starts from the
destination. When hull says a move there would still send everything, for
example because the hull or hvi there predates the streamed move, Brig says
why and records nothing. The prepare does not fail.

A prepare takes no network on the destination. The session's gateway, with
its egress rules, is still started by the move (step 4). You can run it
again at any time: a host that holds everything is sent nothing.

### After a move

`brig sh`, `brig run`, `brig stop`, `brig info` and `brig logs` on the ref
run on the destination. Brig hands the same verb, the same ref and the same
flags to the brig there: `ssh -t <user>@<host> brig sh <ref>`. For `brig sh`
and `brig run`, brig replaces itself with ssh, the way it replaces itself with
hull for a local session. This host's runtime and secret store are not asked
anything.

`brig ls` lists the session with the state `moved` and the workspace
`(in the sandbox disk on <user>@<host>)`. Its `--json` carries a `host`
field. `brig plan` and the `brig network` verbs act on this host, so they
refuse a moved session and name the ssh line that works.

`brig rm <ref>` removes the copy the move left on this host: a guest that
the move fenced, which can never run again. It does so when hull's own record
here says the move committed (the copy's checkpoint is `migrated`, by the move
Brig recorded), or else once the brig on the destination says that the
session runs there, and it asks nothing else. hull marks the checkpoint
`migrated` only after the destination answered the commit, so that record
needs no word from the destination, which may since have removed the session
or gone away. It keeps the record that reaches the session. A second `brig rm <ref>` with
no copy left forgets the session on this host and touches nothing on the
destination. `ssh <user>@<host> brig rm <ref>` removes the session there.
`brig rm --all` leaves the copy of a moved session alone. On the source, the
network gateway brig started for the sandbox keeps running until `brig rm`
removes the copy.

### Moving back

A second move goes from the host the session runs on: `brig teleport` there,
with this host as the destination. This host still has the fenced copy and
its record of the move, and the destination refuses both: the probe finds the
sandbox name taken, and `brig teleport-receive` finds the session already
filed. So on the old host, first remove the copy and then forget the session:

```bash
brig rm <ref>       # on the old host: removes the fenced copy
brig rm <ref>       # on the old host: forgets the session there
ssh <user>@<host> brig teleport <ref> --to <you>@<old host>
```

Between the second `brig rm` and the move, the old host has no record of the
session. Run nothing for the ref there. `brig sh <ref>` or `brig run <ref>`
on the old host does not reach the session: it boots a new, empty session
under the same sandbox name, and the move back is then refused, as the name
is taken. Remove that new session with `brig rm <ref>` before the move. Run
your commands on the host the session runs on. hull counts the owner epoch
on: 0 where the session started, 1 after the first move, 2 back home.

To remove a moved session everywhere, remove the fenced copy on the old host
first, then the session where it runs. In the other order, `brig rm` of the
old copy still works, because hull's record of it says `migrated`.

### Credentials after a move

The session takes its credentials with it in the guest's memory, which is what
moved. A later exec on the destination, the agent's or a `brig sh`, carries
only what the destination's brig resolves from its own secret store and its
own environment. Brig asks no other host for a credential.

A secret the destination does not have is not a refusal there, unlike on a
boot. The exec runs without it, and brig prints which secrets it could not
hand in and why. The agent keeps the values it already holds. To hand a fresh
value in, store the secret on the destination with `brig secret`.

### What reconnects and what does not

| | After a move |
| --- | --- |
| The guest's processes, memory and open files | continue. The guest does not boot again |
| The disk and the guest home on it | moved with the guest |
| The guest's address and its egress rules | the same, on a gateway the destination's brig started |
| `brig sh`, the agent's terminal | a new `brig sh` connects on the destination. One open across the move ends |
| Connections to the guest agent (vsock) | reset. The guest sees them close |
| Connections to the outside network | may reset, and reconnect from the destination |
| Published ports | stay on the source. Publish them again on the destination |
| Host NAT and the path out | the destination's |
| The monotonic clock | continues from where it stopped |
| The wall clock | behind by the time the move took, until the guest corrects it |

### What a move keeps true

At most one host runs the guest. hull fences the source before the
destination may resume, and the destination resumes only on the commit.

Brig never resumes the source once a move began. Two things can do it,
and both belong to hull: an abort that the destination confirmed, and a
re-run of `hull migrate` that learns the commit. A move whose end Brig did not
see stays recorded as unfinished. That includes a move after which Brig could
not read hull's record, whatever `hull migrate` returned. While it is unfinished, every verb on the
ref except `brig teleport --status` and `--abort` refuses, and names those
two.

### Recovery

| What you see | What it means | What to do |
| --- | --- | --- |
| A refusal that ends in "Nothing was paused" | the move never began | fix what it names, and run the move again |
| "did not report how it ended" | the move may have committed, or not. hull's record is the authority | `brig teleport <ref> --status` |
| `--status` says the move has not finished | hull's record is between the fence and the commit | `hull migrate <sandbox> --txid <txid>` takes it on from where it stopped. `brig teleport <ref> --abort` calls it off if it has not committed |
| `--status` says the move committed | the session runs on the destination | nothing: `--status` recorded it |
| `--abort` says hull did not confirm | the destination did not answer, or it already runs the guest | retry when it answers. `hull migrate --status <sandbox>` says which |
| `--abort` says the move is called off and hull reported an error | hull recorded the abort, then could not restore the guest here. It may be stopped with its fenced checkpoint | `hull restore <sandbox>` |
| `--status` says hull has no record and this host has no sandbox | the source was removed, and its record with it. The move may have committed | ask the destination: `ssh <user>@<host> hull migrate --status <sandbox>` |
| "is running on this host" | another `brig teleport` of the session holds its lock | let it finish, then run the command again |

When `BRIG_HULL_STORE_DIR` names a store, each hull line above needs
`--store-dir <dir>` before the verb, or hull looks in its default store and
finds no such instance. The lines Brig prints carry it.

`brig teleport <ref> --status` reads hull's record (`hull migrate --status`).
If the record says the move committed, it records the host. If it says hull
aborted the move, it gives the destination's network back. If hull has no
record, the move never began, but only while hull still has the sandbox on
this host. `hull rm` of a source whose move committed removes the record
too. Otherwise it says where the move stands. `brig teleport <ref> --abort` asks hull to abort the
move. Brig then gives the destination's network back, but only once hull's
record says the move is aborted. hull aborts only on the destination's word
that it is not running the guest, and then restores the guest on this host
itself. hull records the abort before that restore, so an abort whose restore
failed still ends the move. Brig then reports hull's error and names
`hull restore`.

### Settings

| Setting | Default | Meaning |
| --- | --- | --- |
| `BRIG_TELEPORT_SSH` | `ssh` on `PATH` | the ssh brig runs to reach the destination |
| `BRIG_TELEPORT_REMOTE_BRIG` | `brig` | the brig to run on the destination |
| `BRIG_TELEPORT_REMOTE_ENV` | (unset) | `NAME=VALUE` words, space-separated, put in front of every brig command on the destination. Use it for a destination whose login does not give brig the settings it needs. The words go on the ssh command line, where process listings on this host and the shell on the destination show them. Never put a credential there |
| `BRIG_HULL_STORE_DIR` | hull's own default, `~/.hull/store` | the hull store brig drives. A move lands in the store the destination's brig drives, and later verbs there name it |

hull carries the checkpoint and the disk over ssh with your ssh
configuration, so the destination's host key and the `authorized_keys`
there decide who can receive the guest. Brig does not hand hull
`HULL_MIGRATE_SSH`, the setting with which hull runs another program in
place of ssh, for the move or for its status or its abort, each of which
reaches the destination too.

### On real hardware

Every step above runs in `script/smoke.sh` against a stub hull, with a fake
ssh that runs the destination on the same machine. Brig's move needs real
ssh from the source to the destination, logging in with a key, because Brig
hands hull no other transport.

#### The Linux pair

On 2026-10-14 a teleportable session moved between two Raspberry Pi 5 hosts,
`rp1` and `rp2` (Linux 6.12, KVM with a vGICv2), and back, with its
processes, its memory and its open files, and without a boot. Both hosts ran
the same builds: hull `a167a07`, hvi `46a18dc` (`feat/teleport` at the time;
now `b3aea90`) with the portable kernel (sha256 `32725b9b...`), and the brig
each run below names. Each host
had a state directory, a gateway directory and a hull store of its own under
`~/b4`, and each reached the other over ssh with a key.

The session was a copy of `ubuntu` with `teleportable: true`,
`genericBoot: true`, `network: offline`, 1 vCPU and 1 GiB. Its guest ran two
processes, started with `brig sh`:

- a counter held in a shell variable, which wrote the count every 200 ms and
  kept a file open that it had deleted;
- an awk process holding 64 KiB of random bytes in memory, which printed
  `RANDSUM <n> <sum>` every 2 s.

[`script/teleport-demo.sh`](../script/teleport-demo.sh) runs the whole
sequence from a third machine and checks each step:

```bash
script/teleport-demo.sh --faults pmoust@rp1 pmoust@rp2
```

Its run with brig `ed78222` gave the two moves in the "busy" columns below.
The demo now also checks each failure case's exit codes and where the guest
ended up, the kernel's `boot_id`, and the move of a session under a policy.
That version ran with brig `4d905b8` and passed all 85 of its checks.

After each move, through `brig sh` on `rp1`:

- the counter had gone on from where it stopped;
- the deleted file still read the token written before the move;
- the sum was the one from before;
- the clocksource was still `hvi-time`;
- the kernel's `boot_id` was the one from before. A boot makes a new one;
- the kernel had logged one more
  `hvi-time: generation N: ... downtime total ...` line, one for each
  restore.

The `ed78222` run checked all of these but the last two. It counted boot
banners in `dmesg` instead, which a reboot does not change, and printed the
last `hvi-time` line.

`hull ps` showed the guest running on the destination, and the copy on the
source stopped with its checkpoint `migrated`; the `4d905b8` run checks both
after each move. `brig ls` on `rp1` showed the
session `moved`, `(in the sandbox disk on pmoust@rp2)`. `--status` said
`ubuntu runs on pmoust@rp2`. `brig info` ran on `rp2` and showed
`TELEPORT restored, arrived from pmoust@rp1`.

The numbers, from hull's records on both hosts, with the clocks of the two
hosts 4 ms apart:

| | `rp1` to `rp2`, both hosts idle | `rp1` to `rp2`, `rp2` busy | `rp2` to `rp1`, `rp2` busy |
| --- | --- | --- | --- |
| Brig's checks, probe, receive and dry run | 1.02 s | 3.29 s | 1.55 s |
| hull's fence, with the capture | 1.17 s | 1.14 s | 1.66 s |
| Transfer | 226.8 MB (216.3 MiB) in 3.45 s, 62.8 MiB/s | 216.2 MiB in 27.0 s, 8.0 MiB/s | 216.6 MiB in 5.43 s, 39.9 MiB/s |
| Restore paused, then commit | 1.30 s, 0.30 s | 1.58 s, 0.35 s | 1.42 s, 0.30 s |
| Downtime: the source pauses for the fence, to `running` on the destination | 6.51 s | 30.44 s | 9.18 s |
| Downtime: `fenced` (the capture written) to `running` | 5.34 s | 29.29 s | 7.51 s |
| `brig teleport`, start to end | 8.87 s | 35.14 s | 12.47 s |
| `brig sh` on `rp1` to the first prompt from the guest on `rp2` | 0.34 to 0.40 s | 0.42 to 0.68 s; once 5.98 s | |
| The same from a Mac, `ssh -t rp1 brig sh ubuntu` | 0.52 to 0.57 s | 0.60 to 0.67 s | |

The guest's own count of the pause, the `downtime total` hvi-time logs,
agreed with the first downtime row to 10 ms. The transfer carries the
checkpoint (108 MB of the 1 GiB RAM) and the 118 MB of the disk that holds
data, out of 16 GB. "Busy" is a build of another project that was running on
`rp2` at the time, at a load of 3 to 4. The first column is a run by hand
before `script/teleport-demo.sh` existed, with brig `cb0333c`; the brig
changes since do not touch the move's path. The 5.98 s prompt was the demo's
one sample; three later runs of the same step on the busy pair took 0.42 to
0.68 s. The `4d905b8` run, with `rp2` running other work again, had 9.44 s
of downtime from `rp1` to `rp2` (40.1 MiB/s), and 7.45 s back
(51.5 MiB/s).

The move back went from `rp2`, as [Moving back](#moving-back) says, after
two `brig rm` on `rp1`. hull's owner epoch was 2 at home.

In the `4d905b8` run, a session on an `isolated` network moved the same way
under an egress policy, `default: deny`, that allows `10.0.0.0/8` and
`rp2`'s address. On `rp2` it ran behind a gateway that `brig
teleport-receive` started, with the same address, `198.18.1.2`, and `brig
info` showed `POLICY locked (carried from pmoust@rp1)`. From the guest, port
22 of `rp2`'s address, which the policy allows, opened before and after the
move. `1.1.1.1:443`, which the policy denies and both hosts reach, did not
open. With an earlier brig, the moved guest reached nothing, not even its
gateway; see below.

The failure cases, each on a fresh session. The `4d905b8` run checks each
exit code in the table, and hull's record on both hosts of where the guest
ended up:

| Case | What ran | Exit | What was left |
| --- | --- | --- | --- |
| A policy the destination cannot enforce | the session above, with the destination's hull replaced by a wrapper whose `network-gateway` fails | `7`: "worker is under an egress policy, and pmoust@rp2 cannot enforce one ... Nothing was paused" | hull has no move on record; `rp1` runs the guest and `rp2` has no instance; the counter went on |
| A session that is not teleportable | a sandbox that `hull run` started under the name Brig derives, standing in for an ordinary session, which Linux hull cannot boot, as it shares no host directory | `7`: "plain is not a teleportable session ... Nothing was paused" | nothing |
| An abort before the commit | ssh failed `migrate-prepare`, so hull stopped at `transferred` with the source fenced | `1`, "did not report how it ended"; `--status` "has not finished"; `--abort` `0` (2.1 s in the run by hand) | the source ran again from its fenced checkpoint, and `rp2` had no instance |
| A lost commit answer | ssh ran `migrate-commit` on `rp2` and lost its reply | `1`; `--status` "has not finished", hull `committing`, `rp2` `running`; in the run by hand, `--abort` `1`, refused because the move committed there | the retry `--status` names, `hull --store-dir ... migrate <sandbox> --txid <txid>`, exited `0` and recorded the commit; `--status` then recorded the host; `rp2` runs the guest and `rp1`'s copy is `migrated` |
| The source brig killed during the commit | ssh held `migrate-commit` for 20 s and `brig` was killed with SIGKILL | `137`; `--status` "has not finished" | hull, which outlived brig, committed the move; the next `--status` recorded the host; `rp2` runs the guest and `rp1`'s copy is `migrated` |

In each of the last three cases, the guest went on where it stopped, with
the same counter, file, sum and `boot_id`.

What did not hold on the first runs, and is fixed in Brig:

- The hull lines Brig prints as remedies named no store. With
  `BRIG_HULL_STORE_DIR` set, the `hull migrate <sandbox> --txid <txid>` that
  `--status` printed failed with "instance not found". They now name the
  store.
- `brig rm` of the old copy, after the session had been removed on the host
  it ran on, refused for good: it waited for that host to say the session ran
  there. It now takes hull's own `migrated` record as the proof.
- A moved session on an `isolated` network kept its address, but on the
  destination it reached nothing, not even its gateway. Brig handed hull the
  gateway's control socket, and hull gives that socket to `hvi restore` as
  the one for the guest's network frames. On the control socket the gateway
  waits for something else, so it dropped them. Brig now hands hull the
  gateway's stream socket, `<socket>.qemu`, the one hull's own run gives
  hvi.
- The refusal of a teleportable run with no portable kernel, and the
  `portable` row of `brig doctor`, named an install line that missed the
  store or `HULL_PORTABLE_KERNEL`. Both now print the line under Known
  issues.

#### Claude Code session, Mac to Pi

On 2026-10-14 and 15 a teleportable `claude-code` session moved from a Mac
(Apple silicon, Hypervisor.framework) to Raspberry Pi 5 hosts (Linux 6.12,
KVM). It is the profile as shipped: the image
`ghcr.io/brig-sh/claude-code-stock:root`, hvi, an `isolated` network,
`genericBoot`, 4 vCPUs and 4096 MB. The move committed every time. As built,
hvi on the Pi then died on the first exec into the moved guest. hvi
`9274685` fixes that; see Known issues. With hvi's allocator capped, as a
stand-in for the fix, the guest carried on and the terminal reattached from
the Mac. The Claude Code task
itself did not run, because the login brig held had expired.

The builds were brig `658ade4`, hull `0021196` and hvi `8697de0`, with the
four-patch portable kernel (sha256 `e85e4338...`) installed in each hull
store with `HULL_PORTABLE_KERNEL=<Image> hull --store-dir <store> assets pull
--profile portable-arm64-v1 --force`. On the Mac, brig was built with
`go build ./cmd/brig`, hull with `make urunc_macos`, and hvi with
`cargo build --release` and signed ad hoc with `hvi.entitlements`. For the
Pis, brig and hull were cross-built (`GOOS=linux GOARCH=arm64`, `make
linux`), and hvi was built on `rp2` from a bundle of `8697de0`
(`CARGO_BUILD_JOBS=3`, 1 min 6 s) and copied to `rp1`. The Pis used the
`~/b4` layout of [Running it on two hosts](#running-it-on-two-hosts). The
Mac had a state directory, a gateway directory and a hull store of its own,
and named the brig on the Pi with `BRIG_TELEPORT_REMOTE_BRIG` and
`BRIG_TELEPORT_REMOTE_ENV`, with the same values as `env.sh` there. A
socket path on macOS can have at most 104 bytes, so those directories must
have a short path: a gateway socket is
`<gateway dir>/sandbox-<sandbox>-n.sock.qemu`.

The sequence, with what each step gave:

1. **Pull the image on each host.** 634,430,785 bytes, shown as 605.0 MB by
   `hull images`. The Mac took 32.8 s through `hull pull`, and its store grew
   by 1.67 GB. `rp2` took 120.0 s and `rp1` 27.4 s, and each store grew by
   1.60 GB.
2. **Look at what the session forwards.** `brig plan claude` lists
   `GH_TOKEN (env) from the secret gh-token` and
   `.claude/.credentials.json (file) from the secret claude-credentials`. On
   `rp2`, where no secret store answers, both say `unknown`.
3. **Boot on the Mac.** `brig run --teleportable claude -d` took 21.6 s, with
   the image already pulled. brig said:
   `the imported credential claude-credentials (claude-code) expired 11d ago`,
   `renew it on the host, then:  brig secret import claude-code`.
4. **Start the work in the guest.** Through `brig sh claude -c ...`,
   detached with `setsid nohup ... </dev/null &`:
   - the counter and the deleted file that `script/teleport-demo.sh`
     starts, and its awk sum;
   - a stand-in for an agent's HTTPS stream: `curl --limit-rate 50k` of
     50 MB from `speed.cloudflare.com`, every line stamped, which gives up
     after 20 s without data and starts again.

   The guest has no `/dev/fd`, so a pipeline there cannot use `>(...)`.
5. **The first Claude Code turn.** In `/root/memo`, `claude -p 'Remember
   the code word PINEAPPLE-42 and reply OK' --output-format json` exited 1
   after 1.39 s with `"is_error":true` and `"result":"Failed to
   authenticate: OAuth session expired and could not be refreshed"`. So the
   long task did not run. It would have been
   `claude -p 'Write a 400-line Python program one function at a time;
   before each function print the line COUNT n' --output-format stream-json
   --verbose --include-partial-messages`, with its output stamped into
   `/root/task.jsonl`. Claude Code keeps its session under
   `CLAUDE_CONFIG_DIR`, which this profile sets to `/brig/claude`, a 512 MB
   tmpfs. The failed turn's session file, 32,601 bytes, was there. A
   conversation of this profile lives in the guest's memory, not on its
   disk: it moves with the guest, and is gone when the sandbox stops.
6. **Move.** `brig teleport claude --to pmoust@rp2`. The numbers, from hull's
   records on both hosts (the clocks were 82 to 83 ms apart):

   | | to `rp2`, as built | to `rp1`, as built (2 runs) | to `rp1`, allocator capped |
   | --- | --- | --- | --- |
   | Brig's checks, probe, receive and dry run | 0.94 s | 1.05 s, 0.94 s | 0.98 s; script 0.95 s |
   | hull's fence, with the capture | 2.07 s | 1.84 s, 1.63 s | 2.08 s; 1.70 s |
   | Transfer | 2,077,823,293 bytes in 125.52 s, 15.8 MiB/s | 1,804,846,253 bytes in 46.34 s, 37.1 MiB/s; 1,804,137,886 bytes in 50.13 s, 34.3 MiB/s | 1,803,342,976 bytes in 48.74 s, 35.3 MiB/s; 1,802,347,665 bytes in 53.74 s, 32.0 MiB/s |
   | Restore paused, then commit | 2.95 s, 0.28 s | 1.26 s, 0.22 s; 1.23 s, 0.23 s | 1.26 s, 0.25 s; 1.38 s, 0.22 s |
   | Downtime: the source pauses for the fence, to `running` on the destination | 131.12 s | 49.95 s; 53.53 s | 52.64 s; 57.43 s |
   | `brig teleport`, start to end | 133.54 s | 52.18 s; 55.64 s | 54.92 s; 59.52 s |
   | The first `brig sh claude -c true` after the move, from the Mac | 0.42 s, exit 1 | 0.58 s, exit 1; 1.17 s with gdb attached, exit 1 | 0.68 s, then 0.65 to 0.68 s; script 0.67 s; exit 0 |

   The transfer is the guest's memory (463 MB of the 4 GB on the `rp2` run)
   and the 1.6 GB of the 16 GB disk that hold data, almost all of it the
   image. The guest is paused for the whole transfer, so most of the
   downtime is the transfer. Tailscale showed the path to `rp2` through a
   relay, not direct, which explains that rate. `rp2` was running another
   project's builds. They ran it out of memory once during the session,
   while a debug copy of the guest also ran there, so the runs after the
   first went to `rp1`, which was idle. The guest's own count
   of the pause, `downtime total` in its `hvi-time: generation 4` line, was
   131.19 s on the `rp2` run and 52.70 s on the capped run, within 0.1 s of
   the downtime row.
7. **The first exec there.** On every as-built run, the move committed and
   hvi on the destination resumed the guest, which logged its
   `hvi-time: generation 4` line and ran on. hvi was still running 11 s
   after the move on the `rp1` run that waited. The first
   `brig sh claude -c true` reached the brig on the Pi, which failed:
   `could not make the guest home /root in the sandbox: exit status 1`. On
   the two `rp1` runs, with strace and then gdb on hvi, hvi died of SIGSYS
   on that exec and printed nothing. On `rp2`, which was not traced, hvi
   also printed nothing, and the journal shows its session scope ending
   during that exec. Each later
   `brig sh` said
   `the teleportable sandbox brig-claude-code is stopped ... Remove it with
   brig rm claude-code to start the session over`. The source copy was
   fenced, so the session was lost.
8. **After the move, allocator capped.** With `MALLOC_ARENA_MAX=4` in the
   environment of the destination's hull, which hvi inherits, every exec
   worked. From the Mac, `brig sh` said
   `claude-code@r3 runs on pmoust@rp1` and ran there:
   - the counter went on, 96 to 131, after a 53.5 s gap in its ticks;
   - the deleted file still read the token from before;
   - the awk sum was the one from before;
   - `boot_id` was the one from before;
   - the kernel had logged exactly one more `hvi-time: generation` line;
   - the guest kept its address, `198.18.1.14`.

   On `rp1`, `hull ps` showed the guest `running`. On the Mac its copy was
   `stopped` with the checkpoint `migrated`. `brig ls` on the Mac showed
   the session `moved`, `(in the sandbox disk on pmoust@rp1)`, `--status`
   said `claude-code@r3 runs on pmoust@rp1`, and `brig info` showed
   `TELEPORT restored, arrived from pmoust@Panagiotiss-MacBook-Pro.local`.
9. **The open HTTPS stream.** The download that ran across the move ended
   when the guest resumed: `curl: (56) Recv failure: Connection reset by
   peer`, exit 56. The next attempt began 1 s later, from `rp1`, and
   downloaded. The moved guest's connections went through a new gateway on
   `rp1`, and the old one's state stayed on the Mac. An agent's open stream
   breaks the same way, and continues only if the agent connects again.
10. **Credentials on the Pi.** Each exec there printed:

    ```text
    brig: brig-claude-code-r3 was moved here from pmoust@Panagiotiss-MacBook-Pro.local, and this host has no value for claude-credentials, gh-token
      ↳ this exec carries none of them; the agent keeps the values it already holds in memory
      ↳ brig asks no other host for a credential. Store one here with `brig secret` to hand it in
    ```

    An exec there had no `GH_TOKEN` in its environment. The download, which
    started on the Mac, still held `GH_TOKEN`; only the names were read.
    The credentials file in the `/brig/claude` tmpfs moved in the guest's
    memory and was still there, unchanged since the last exec on the Mac.
11. **The follow-up turn** (`claude -p --continue "What is the code word?"`)
    could not run. The session that held the first turn was the one on
    `rp2`, which had stopped, and brig refused the exec there. The login
    had also expired.

[`script/teleport-demo-claude.sh`](../script/teleport-demo-claude.sh) runs
steps 3, 4 and 6 to 10 from the Mac and checks each one, and cleans up:

```bash
script/teleport-demo-claude.sh pmoust@rp1
```

With brig `658ade4`, run with the capped allocator on `rp1` and brig's
settings for this Mac in the environment, it passed all 18 of its checks.
Its `--claude` option also starts the Claude Code task of step 5 and
checks that its output still grows after the move. That option has not run,
as it needs a login that has not expired.

#### Known issues

- hull `a167a07` names `hull migrate <sandbox> --txid <txid>` without the
  store when it refuses an abort of a committed move
  (`cmd/hull/migrate_source.go`, lines 973 to 975, in `sourceAbort`), although
  it was run with `--store-dir`. Add the store by hand. Brig passes hull's
  text through as is.
- hull `a167a07` refuses a run with `--profile portable-arm64-v1` when its
  store has no record of the portable kernel (`VerifyProfileKernel`,
  `internal/bootassets/profile.go`, line 352) ("no portable-arm64-v1
  kernel is installed ... `hull assets pull --profile portable-arm64-v1`
  installs one"), even with the kernel file in place. `brig doctor` checks the
  file and the pin, not hull's record, so it reports `ok` for a kernel hull
  then refuses. Install the kernel through hull:

  ```bash
  HULL_PORTABLE_KERNEL=/path/to/Image hull --store-dir "$BRIG_HULL_STORE_DIR" \
      assets pull --profile portable-arm64-v1
  ```

- The refusal of a session that is not teleportable says its guest home is a
  host directory shared in, even for a sandbox Brig has no record of, whose
  hull record shows no share.
- Fixed in hvi `9274685`: hvi `8697de0` on Linux died of SIGSYS on the
  first exec into a 4-vCPU guest that moved from a Mac, and printed
  nothing. The vsock bridge starts a thread for each agent connection. That
  thread's first `free()` went through glibc's `tcache_init` and
  `arena_get2` to `__get_nprocs`, which opens
  `/sys/devices/system/cpu/online`, and hvi's seccomp filter does not allow
  `openat`. A process with fewer threads may never reach that call, which
  may be why the 1-vCPU moves above did not; that was not tested. hvi
  `9274685` calls `mallopt(M_ARENA_MAX, 1)` first in `main`, so glibc never
  counts the CPUs and has no other heap to shrink. With that hvi, the
  destination's hull needs no wrapper.

  The runs above came before that commit. As a stand-in for the fix, they
  ran the destination's hull through a wrapper that set
  `MALLOC_ARENA_MAX=4`, and hvi inherited it. That cap does not cover every
  case: hvi's commit records that a 60 s agent stress test with it still
  died of SIGSYS, when glibc read `/proc/sys/vm/overcommit_memory` the
  first time the heap of another arena shrank. The moves above have not
  been run again with hvi `9274685`.

#### Running it on two hosts

Each host needs the same brig, hull and hvi, the profile, and an `env.sh`
that sets Brig up and names the destination's brig for the source. This is
the one the runs above read, as `~/b4/env.sh` on both hosts:

```bash
B4=$HOME/b4
export PATH=$B4/bin:$PATH
export BRIG_RUNTIME=hull BRIG_RUNTIME_BIN=$B4/bin/hull
export BRIG_STATE_DIR=$B4/state BRIG_GATEWAY_DIR=$B4/gw BRIG_HULL_STORE_DIR=$B4/store
export BRIG_PROFILE_DIR=$B4/profiles BRIG_POLICY_DIR=$B4/policies BRIG_BOOT_ASSETS=$B4/assets
export BRIG_PORTABLE_KERNEL=$B4/assets/portable-arm64-v1/Image
export BRIG_VERIFY=warn DO_NOT_TRACK=1 HULL_TELEMETRY_DISABLED=1 BRIG_TELEMETRY=off
export BRIG_TELEPORT_REMOTE_BRIG=$B4/bin/brig
export BRIG_TELEPORT_REMOTE_ENV="PATH=$B4/bin:/usr/local/bin:/usr/bin:/bin BRIG_RUNTIME=hull BRIG_RUNTIME_BIN=$B4/bin/hull BRIG_STATE_DIR=$B4/state BRIG_GATEWAY_DIR=$B4/gw BRIG_HULL_STORE_DIR=$B4/store BRIG_PROFILE_DIR=$B4/profiles BRIG_POLICY_DIR=$B4/policies BRIG_BOOT_ASSETS=$B4/assets BRIG_PORTABLE_KERNEL=$B4/assets/portable-arm64-v1/Image BRIG_VERIFY=warn DO_NOT_TRACK=1 HULL_TELEMETRY_DISABLED=1 BRIG_TELEMETRY=off"
```

Install the portable kernel through hull on each host, as above. Then, on
`rp1`, after `source ~/b4/env.sh`:

```bash
brig run --teleportable ubuntu -d
brig sh ubuntu -c 'nohup setsid sh -c "i=0; while :; do i=\$((i+1)); echo \$i > /tmp/count; sleep 0.2; done" >/dev/null 2>&1 </dev/null &'
brig teleport ubuntu --to me@rp2
brig sh ubuntu -c 'cat /tmp/count'   # runs on rp2, over ssh
brig ls
brig teleport ubuntu --status
brig info ubuntu                     # runs on rp2
brig rm ubuntu                       # removes the fenced copy left here
brig rm ubuntu                       # forgets the session here
ssh me@rp2 'source ~/b4/env.sh && brig teleport ubuntu --to me@rp1'
brig sh ubuntu -c 'cat /tmp/count'   # runs here again
ssh me@rp2 'source ~/b4/env.sh && brig rm ubuntu && brig rm ubuntu'
brig rm ubuntu
```

#### One Mac

On one Mac with Remote Login on, a move between two stores goes like this:

```bash
# The destination's brig: its own state, gateway directory and hull store.
export BRIG_TELEPORT_REMOTE_ENV="BRIG_STATE_DIR=$HOME/.brig-b BRIG_GATEWAY_DIR=$HOME/.brig-b BRIG_HULL_STORE_DIR=$HOME/.hull/store-b"
brig run --teleportable claude -d
brig teleport claude --to "$USER@localhost"
brig sh claude -c 'uptime'          # runs in the moved guest, over ssh
brig rm claude                      # removes the fenced copy left here
```
