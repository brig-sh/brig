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
the move fenced, which can never run again. It does so only after the brig on
the destination says that the session runs there, and it asks nothing else.
It keeps the record that reaches the session. A second `brig rm <ref>` with
no copy left forgets the session on this host and touches nothing on the
destination. `ssh <user>@<host> brig rm <ref>` removes the session there.
`brig rm --all` leaves the copy of a moved session alone. On the source, the
network gateway brig started for the sandbox keeps running until `brig rm`
removes the copy.

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
ssh that runs the destination on the same machine. hull's own `hull migrate`
has committed a move between two stores on one Mac (see hull's
`docs/migration.md`). Brig's move needs real ssh from the source to the
destination, logging in with a key, because Brig hands hull no other
transport.

#### The Linux pair

On 2026-10-14 Brig ran on two Raspberry Pi 5 hosts, `rp1` and `rp2` (Linux
6.12, KVM with a vGICv2), with `BRIG_RUNTIME=hull`, a state directory and a
hull store of its own on each, and real ssh from `rp1` to `rp2` with a key.
Both hosts ran the same brig, hull and hvi builds. The profile was a copy of
`ubuntu` with `teleportable: true`, `network: offline`, 1 vCPU and 1 GiB.

What ran:

- `brig doctor` on `rp1` reported the portable kernel present and the
  pinned build.
- `brig run --teleportable ubuntu -d` booted the `portable-arm64-v1` machine
  on KVM: hull recorded the profile, the hvi command line carried
  `--profile portable-arm64-v1`, and the guest ran the portable kernel with
  `hvi-time` as its clocksource. `brig info` printed
  `MACHINE portable-arm64-v1`.
- `brig sh ubuntu` started a counter loop in the guest and read it back.
- `brig teleport-probe` ran on `rp2` over ssh and reported hull, hvi, the
  store and a gateway that enforces egress.
- `brig teleport ubuntu --to pmoust@rp2` ran its checks, the probe, the
  receive on `rp2` and hull's dry run. The dry run refused: the hvi that
  could boot the portable machine took no control socket. Brig gave `rp2`
  its network and index back, the counter kept counting on `rp1`, and
  `--status` reported no move.

What is still to run: the move itself, which needs an hvi that boots the
portable machine, checkpoints it and restores it on KVM. The hvi on
`feat/teleport` had none of the three at the time. Until a move has
committed on real hardware, read the move rows of
[claims.md](claims.md#rows-checked-at-the-command-line-only) as true of the
command lines only.

To run the move on such a pair, give the destination's brig its settings,
then run the same commands:

```bash
B3=/home/me/b3   # the same layout on both hosts
export BRIG_TELEPORT_REMOTE_BRIG=$B3/bin/brig
export BRIG_TELEPORT_REMOTE_ENV="PATH=$B3/bin:/usr/bin:/bin BRIG_RUNTIME=hull BRIG_RUNTIME_BIN=$B3/bin/hull BRIG_STATE_DIR=$B3/state BRIG_HULL_STORE_DIR=$B3/store BRIG_PROFILE_DIR=$B3/profiles BRIG_BOOT_ASSETS=$B3/assets"
brig run --teleportable ubuntu -d
brig sh ubuntu -c 'nohup setsid sh -c "i=0; while :; do i=\$((i+1)); echo \$i > /tmp/count; sleep 0.2; done" >/dev/null 2>&1 </dev/null &'
brig teleport ubuntu --to me@rp2
brig sh ubuntu -c 'cat /tmp/count'   # runs on rp2, over ssh
brig ls
brig teleport ubuntu --status
brig rm ubuntu                       # removes the fenced copy left here
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
