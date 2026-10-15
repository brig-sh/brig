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
| `brig teleport --to` of a session to the host it runs on | `1` |
| `brig teleport --to`, `--prepare` or `--abort` of a session that runs on another host, which the brig there refuses or fails | that brig's code |
| `brig teleport --to`, `--prepare` or `--abort` of a session that runs on another host, when ssh to that host fails or drops before the brig there answers | `1` |
| `brig teleport --to`, `--prepare` or `--abort` of a session whose move to this host has not finished: the source drives it, and `brig teleport <ref> --status` there settles it | `1` |
| `brig teleport --to` or `--prepare` back to this host, when the fenced copy the move away left here is not shown as stale: hull's record here does not say that move committed away, and the brig on the session's host does not say it runs there | `1` |

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

The streamed move sends most of it while the guest runs. It is in hull
`d2bb5e9` on `feat/teleport`, built with the hvi it pins, `c4131f5` on
hvi's `feat/teleport`, whose `migrate` stream carries the disk rounds with
the memory rounds. hull falls back to the whole-bundle move, and
says why, when either host lacks it. When hull boots a
teleportable sandbox, it keeps a read-only copy of the disk as built, its
base, named by its digest. `brig teleport <ref> --to <user>@<host>
--prepare` has hull put that base on the destination, with the image and
the portable kernel. A move to a host that was not prepared does this step
first, before the pause. The move then copies in rounds while the guest
runs. hvi records which pages of memory and which 64 KiB disk blocks the
guest writes. Round 0 sends the memory that is not zero and the disk blocks
that differ from the base. Each later round sends what the guest changed
during the round before. hvi pauses the guest when the pages written during
a round come to at most 8 MiB, or 2% of the guest's memory that is not zero
if that is more, or after 5 rounds, or after 60 s, whichever comes first.
These are hvi's defaults. It then sends the last changes, the CPU state and the device
state, and the destination resumes the guest. Only that last round is
downtime. A guest that writes faster than the link can carry never gets a
small round. The cap still ends the copy, and the last round is then
larger. What a move keeps true does not change: the source is fenced before
the destination may resume, the last memory and disk changes are taken in
the same pause, and the destination checks every part against its sha256
before it runs anything.

By default `brig teleport` prints one short line on stderr for each step
hull reports, as it arrives: each round of the copy with its size and time
(`ram-round-0 40.6 MiB in 1.00 s`), the pause (`paused after 2 RAM rounds
(converged)`), the seal (`sealed 19 ms after the pause`), what was sent
and the restore on the destination, and the resume there (`resumed on
pmoust@rp2 (epoch 1)`). For a move that sends the whole checkpoint and
disk, the steps are the fence, the restore and the resume. hull's
warnings and errors are printed as hull wrote them, and so are the two
lines that explain a long wait: a base that hull sends first to a host
that does not hold it, and a destination that copied the base while the
guest was paused. So is the line that says where the guest is when a
streamed move falls through before hvi fenced it (`hvi did not fence
brig-ubuntu: its stream ended before the manifest, and the guest runs on
here`). `--verbose` prints every line hull prints, as hull
printed it, after `brig: hull:`: how hull moves the guest, each round with
its rate, and for a disk round after the first, how many blocks a second
the guest wrote (hull prints no such rate for memory), hvi's figures for
the pause, hull's timings of each step and its timeline from the seal on
both hosts. `--json` carries those numbers in its `move` object. When the
move commits, `brig teleport` prints how long hull's move took, whether hull streamed it,
hull's estimate of how long the guest was paused, the bytes sent, and the
rounds: how many of the disk and of memory, and the size and time of the
final disk round, which the guest waits for, then what it was sent as.
The size and time are one pair hull gave: the round's raw size and hvi's
time for it from hull's last line, "the final disk round 6.3 MiB in
21 ms (sent as 161172 bytes)", or else the size and time on the round's
own line. hull `d2bb5e9`'s line for a round gives only what the round came
to in hvi's stream, and Brig never gives that the raw round's time. A
size or time hull did not print is left out. hull prints "last disk round
0 bytes in 0 ms" when hvi's reply has no figures for the round, so beside
a round that sent something Brig takes that as no figures and gives the
round's own line, "the final disk round sent as 161172 bytes in
0.01 s". It reads hull's
names for the disk rounds as hull 511877d printed them
(`disk round 3 (final)`) and as hull `d2bb5e9` prints hvi's own
(`disk-round-0`, `disk-round-final`). A line Brig cannot read is still
printed under `--verbose`, and adds nothing to the summary. hull's `downtime about` is an estimate: it is
the time the destination resumed the guest less the time the last memory
round began to arrive here, read on two hosts' clocks. Brig prints hull's lines from a queue of its own, so hull
never waits on Brig's stderr, which hull writes to while the guest is
paused. When that stderr falls a few thousand lines behind, Brig drops
lines and then says how many. A stderr whose reader has gone does not
stop the move.

hull measured both paths on the Linux pair, with the 1 GiB `ubuntu` guest
under a writing workload, over real ssh. The numbers below are from hull's
`docs/migration.md` at `6bfc09f`. hull was built at `1ecb5c8` (the same
tree as `81e118b`), and hvi at `ef8833a`, a merge of hvi's two
`feat/teleport-fast` branches. These moves ran `hull migrate` directly, not
through `brig teleport`. "Guest stopped" is what the guest's own `hvi-time`
driver logged:

| `rp1` to `rp2` | Before: whole bundle | After: streamed |
| --- | --- | --- |
| `hull migrate`, start to end | 8.2 s | 5.4 s (5.7 s for `rp2` to `rp1`) |
| Guest stopped | 7.43 s | 279 ms (340 ms for `rp2` to `rp1`) |
| Sent | 288.2 MiB in 5.1 s | 109.5 MiB: disk 287 MiB, sent as 34.6 MiB, and 74.9 MiB of memory in 3 rounds |

The same streamed move through `brig teleport`, on the same pair with the
same hull and hvi, brig `e1a9c2d` there and `e38f18d` back, and the same
guest writing a counter to its disk five times a second. `--prepare` first
sent the 110.8 MiB base as 49.2 MiB in 1.1 s (3.0 s in all), and found
everything held on the way back. After each move the counter went on, and
the guest's `boot_id` and the sha256 of a 32 MiB file it wrote were the
same:

| | `rp1` to `rp2` | `rp2` to `rp1`, `rp2` busy with another project's builds |
| --- | --- | --- |
| `brig teleport`, start to end | 7.6 s | 11.9 s |
| `hull migrate`, as Brig timed it | 6.0 s | 10.6 s |
| Guest stopped, hull's `downtime about` | 358 ms | 4842 ms |
| Guest stopped, the change in the guest's `hvi-time` `downtime total` | 346 ms | 4837 ms |
| Sent | 109.5 MiB in 5.4 s: disk 274.9 MiB as 34.4 MiB, memory 75.0 MiB | 109.6 MiB in 10.3 s |

On the move back, hvi on `rp2` sealed its stream 1163 ms after the last
memory round began, and the last disk round took 717 ms. The guest's own
figure agrees with hull's in both directions. The guest's `hvi-time`
driver logs `downtime total` at each restore, a total since boot, so the
downtime of one move is the change from the restore before it: 346 ms at
generation 4, after the move to `rp2`, and 5183.6 less 346.2, 4837 ms, at
generation 6, after the move back. The same line's `wall clock moved` does
not measure the downtime: after the first move it read 10.34 s, against
346 ms. After the move back it read 4.84 s.

The `claude-code` guest from a Mac to a Pi, beside the same streamed path
between the two Pis. The links were not the same. On 2026-10-15 the Mac
reached the Pis from another network, at 1.0 to 1.1 MiB/s, so the Mac
column after measures that link. The Pi column after is hull's own run
on the Linux pair that day, over their wired LAN, with the 1 GiB `ubuntu`
guest (1 vCPU) and `hull migrate` run directly, not through `brig
teleport`. Its numbers are from hull's `docs/migration.md` at `9fb5e62`:

| | Before: hull `0021196`, hvi `8697de0`, Mac to `rp1`, 4 runs | Target | After: hull `d2bb5e9`, hvi `c4131f5`, Mac to `rp2`, 2 runs | After: hull `ef78fb6`, hvi `c4131f5`, `rp1` to `rp2`, `ubuntu` idle and writer, 1 run each |
| --- | --- | --- | --- | --- |
| The link | Mac to Pi, 32.0 to 37.1 MiB/s | | Mac to Pi, 1.0 to 1.1 MiB/s | Pi to Pi: the name `rp2` goes over Tailscale, which went direct over one wired gigabit LAN when checked (see below) |
| Sent once per image and host (`--prepare`) | nothing | the base | the 1.5 GiB base as 742.8 MiB in 733.8 s; the prepares after it found it held | the base, by `hull migrate-prepare` first |
| Sent while the guest runs | nothing | the rounds | disk round 0, 3834 and 3839 blocks (240 MiB) as 2.5 MiB, and memory round 0, 62.8 and 63.3 MiB in 60.9 and 59.7 s; on the second run also a disk round of 102 blocks and a memory round of 5.5 MiB | idle: disk rounds of 4393 and 7 blocks, and 2 memory rounds; writer: disk rounds of 5426, 487, 0 and 0 blocks, and 4 memory rounds |
| Sent while the guest is paused | 1.80 GB in 46.3 to 53.7 s | the last round | the last memory round, 5.4 and 4.4 MiB, and the last disk round, 101 and 7 blocks (161,299 and 9,396 bytes on the wire) | the last memory round, the 3rd idle and the 5th with the writer; the last disk round was empty in both |
| Sent in all | 1.80 GB | | 70.9 and 75.9 MiB | idle 109.5 MiB (disk 275.0 MiB as 34.5 MiB, memory 74.9 MiB); writer 310.4 MiB (disk 369.6 MiB as 128.5 MiB, memory 181.8 MiB) |
| hvi's pause to its seal | (hull's fence, with the capture: 1.63 to 2.08 s) | | 5087 and 3677 ms | |
| hvi's seal to the destination's resume | | | 2.9 and 3.1 s | 55 to 90 ms, hull's range over all four of its idle and writer moves on the Pis, `rp1` to `rp2` and back |
| Restore paused, then commit | 1.23 to 1.38 s, then 0.22 to 0.25 s | | restored as the stream arrived; commit 0.07 and 0.09 s | restored as the stream arrived |
| Downtime | 49.95 to 57.43 s, the source pauses to `running` on the destination | 1 to 2 s | 7962 and 6819 ms, the rise in the guest's `hvi-time` `downtime total`; hull's estimate 7958 and 6813 ms | idle 78.6 ms, writer 94.7 ms, the rise in the guest's `hvi-time` `downtime total` |
| `hull migrate`, start to end | | | 70.1 and 73.1 s | idle 2.29 s, writer 5.13 s |
| `brig teleport`, start to end | 52.18 to 59.52 s | under 10 s | 71.73 and 75.00 s | not run through Brig |

The before column is the four moves to `rp1` in
[Claude Code session, Mac to Pi](#claude-code-session-mac-to-pi), and the
Mac column after the two streamed moves to `rp2` there. The streamed move
sent 23 to 24 times fewer bytes. On the Mac's link it met neither target.
Most of the time is memory round 0, about 63 MiB at the link's rate, with
the guest running. The pause holds the last memory round, which took 5.05
and 3.66 s to send. The 2.9 and 3.1 s from hvi's seal to the
destination's resume are hull's part, against 55 to 90 ms between the
Pis; that difference was not explained. A run of the `claude-code` guest
on a faster link has not been made.

On the Linux pair the streamed move, hull `ef78fb6` (`d2bb5e9` adds the
fixes of its review) with hvi `c4131f5`, moved the idle guest in 2.29 s
with 78.6 ms of downtime, and the writer (about 10 MiB/s to its disk) in
5.13 s with 94.7 ms. That pair is not the `claude-code` guest: it has a
quarter of the memory and one vCPU, so it shows the path where the link
does not limit it, not what the Mac move would take on a fast link.
`rp1` and `rp2` each reach the LAN on `eth0` at 1000 Mb/s, and the name
`rp2` resolves on `rp1` to its Tailscale address, which goes direct over
that LAN: on 2026-10-15 `tailscale ping rp2` from `rp1` answered "via
192.168.17.94" in 1 ms, and a plain ping of that address averaged 0.27
to 0.30 ms in three checks. hull's record of its runs does not say which
path its ssh took; this is the path that name took when checked.

#### Prepare the destination

```bash
brig teleport <ref> --to <user>@<host> --prepare
```

`--prepare` runs the checks of steps 1 and 2 above, and then
`hull migrate-prepare --to <user>@<host> --json <sandbox>`, with the hull
store and the hull that the destination's brig drives. hull puts the
image, the portable kernel and the sandbox's base on the destination,
each only if it is not there yet. Brig prints what hull did for each part
(`held`, `sent`, `skipped` or `failed`) and the digest of the base the
destination holds. `--verbose` also passes hull's lines on as they arrive.
A prepare pauses nothing, and the guest keeps running here.

Brig records the host in its session index once it holds the sandbox's
base, with whether hull says a move there sends only what the guest
changed. `brig teleport --status` and `--json` then name it, and a move to
that host says when it was prepared and what hull said then. That is a
record of the prepare, not a check made at the move. hull decides again
when the move starts, and says how on its `Transport:` line. When hull
sends the whole checkpoint and disk to a host that the prepare said would
stream, Brig says so, and the move's `--json` carries hull's word in
`transport` and `transportWhy`. A committed move
clears the record, because the next move starts from the destination. When
hull says a move there would still send everything, for example because
the hull or hvi there predates the streamed move, Brig says why. A host
that holds no base of the sandbox is not recorded. The prepare does not
fail in either case.

A prepare takes no network on the destination. The session's gateway, with
its egress rules, is still started by the move (step 4). You can run it
again at any time: a host that holds everything for this boot of the
session is sent nothing.

With hull `d2bb5e9` the base is once per image and host. `hull run`
builds it once per image digest and disk size, keeps it in its store, and
boots each sandbox of that image against it. On the runs in
[Claude Code session, Mac to Pi](#claude-code-session-mac-to-pi), four
sessions of `claude-code` on the Mac booted against the same base,
`9890a88a4845`. The first prepare of `rp2` sent it: 1.5 GiB as 742.8 MiB
in 733.8 s on a 1.0 MiB/s link, 790 s for the whole prepare. The
prepares of the next two sessions found it held, in 12.1 and 47.1 s,
each making the spare copy of the base the destination keeps for the
next move. A move that finds no spare copies the base first: on the one
that did, the destination took the stream 23.5 s late.

The way back is prepared the same way, with the old host as the
destination: `brig teleport <ref> --to <you>@<old host> --prepare`, typed
on either host (see [Moving back](#moving-back)). The old host still
holds the base the session booted from, so hull sends nothing and only
makes the spare copy that the move back takes. Without the spare, the
move back copies the base on the old host while the guest is paused: 22.9
s of downtime in one run on the Linux pair. A hull that makes the spare on
the source by itself once a move commits needs no such prepare. hull
`7b3f7b1` does not.

### After a move

`brig sh`, `brig run`, `brig stop`, `brig info` and `brig logs` on the ref
run on the destination, and so do `brig teleport --to`, `--prepare` and
`--abort`: see [Moving back](#moving-back). Brig hands the same verb, the same ref and the same
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
or gone away. It then forgets the session on this host, and touches nothing
on the destination. With no copy left here, as after `hull rm` of the copy,
`brig rm <ref>` only forgets the session here. `ssh <user>@<host> brig rm
<ref>` removes the session there.
`brig rm --all` leaves the copy of a moved session alone. On the source, the
network gateway brig started for the sandbox keeps running until `brig rm`
removes the copy.

### Moving back

A second move goes from the host the session runs on, because hull fences
the guest where it runs and the brig there holds the move's record. Type it
on either host. On the old host, `brig teleport --to`, `--prepare` and
`--abort` go to the brig where the session runs, over ssh, as `brig sh`
does, and its lines come back as it prints them:

```bash
brig teleport <ref> --to <you>@<old host>             # on the old host
ssh <user>@<host> brig teleport <ref> --to <you>@<old host>   # the same, typed there
```

The old host still has the copy the first move left, fenced, under the
sandbox name the move back arrives as. The probe, the arrival and hull
each refuse a destination that has it. When the move back, or its
prepare, is typed on the old host, that host's brig removes the copy
before it asks the brig there, once the copy is shown to be stale, as
`brig rm` shows it: hull's record on the old host says that move
committed away, or the brig on the session's host says the session runs
there. It keeps its record that the session runs on the other host. The
move back replaces that record, and an abort of the move back puts it
back. The copy is gone either way: it could never run again. Typed on the
host the session runs on, the move back is refused while the old host has
the copy. Run `brig rm <ref>` on the old host first: it removes the copy
and forgets the session there.

After a move typed on the old host commits, that host's brig asks the
brig there where the session runs now, and records it. When the move came
back, the arrival has already filed the session as running on the old
host. The copy that the move back left on the other host is what `brig rm
<ref>` there removes. When the move went on to a third host, the old host
records that host, and its verbs go there. When that brig says the session
came back, and the old host has no arrival of it filed, as when that brig
reached the old host with another `BRIG_STATE_DIR`, the old host keeps its
record and says so: a record that named the old host itself would send its
verbs to itself. If the old host cannot ask, it
keeps its record, and its verbs go to the host it names, whose brig hands
them on. `--abort` on the old host calls off a move that the session's
host began and has not finished. A move that committed is not undone: the
brig there answers that it has no move of the session to call off, and
`brig teleport` from there moves the session again. A forwarded line that
fails exits with the code of the brig
there, or `1` when ssh fails or drops before that brig answers. The move
may then have run, and `ssh <user>@<host> brig teleport <ref> --status`
says where it stands. `brig teleport <ref> --status` on the old host reads
that host's record only, and says that the moves of the session are driven
from the host where it runs.

A move back that fails can leave the old host changed. The fenced copy is
gone: brig removed it before it handed the line on. When the brig there
had begun the move, the old host also has its arrival recorded, which
files the session there as running until that brig settles the move. A
move called off gives the old record back. While the arrival is recorded,
`brig teleport <ref> --status` on the old host says the move is arriving
and names the host it comes from, and `--to`, `--prepare` and `--abort`
there are refused with the lines to run on that host instead. The message
of the failed line says which of these the old host has.

The brig on the session's host reaches the destination with its own
settings: the `BRIG_TELEPORT_*` settings that its ssh login gives it, not
the old host's. A brig that runs with settings its login does not give it
needs a `BRIG_TELEPORT_REMOTE_BRIG` on the old host that sets them, for
example a script there that sources them and runs brig. See
[Settings](#settings).

A record that says the session runs on another host does not refuse the move
back. An older Brig's `brig rm`, or `hull rm` of the copy, leaves that record,
and the move back replaces it. If the move back is called off, the old host
gets the record back, as the session still runs on the other host. A move
out of the old host that has not finished still refuses: `brig teleport
<ref> --status` there settles it first. The record is taken by the ref and
sandbox name alone, as the session may have moved on from the host it names
before coming back. When that host is not the one the move comes from, the
brig moving it says which record the old host had and that the move
replaced it: a different session under the same name would still run on
that host, and the old host would no longer reach it by the ref. A record
under another sandbox name, as after `BRIG_NAME` changed, still refuses.

After `brig rm <ref>` on the old host, the old host has no record of the
session until the move back. Run nothing for the ref there in that time.
`brig sh <ref>` or `brig run <ref>` on the old host does not reach the
session: it boots a new, empty session under the same sandbox name, and the
move back is then refused, as the name is taken. Remove that new session
with `brig rm <ref>` before the move. A move back typed on the old host
keeps the record, so its verbs go to the session until the move back
commits. hull counts the owner epoch on: 0 where the session started, 1
after the first move, 2 back home.

To remove a moved session everywhere, run `brig rm <ref>` on the old host,
which removes the fenced copy and forgets the session there, then on the
host where it runs. In the other order, `brig rm` of the old copy still
works, because hull's record of it says `migrated`.

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
| `BRIG_TELEPORT_REMOTE_ENV` | (unset) | `NAME=VALUE` words, space-separated, put in front of every brig command on the destination. Use it for a destination whose login does not give brig the settings it needs. The words go on the ssh command line, where process listings on this host and the shell on the destination show them. Never put a credential there. The brig on the destination does not pass them on: a `brig teleport` that the old host hands to the session's host runs with that host's own `BRIG_TELEPORT_*`, so a brig that needs settings to reach back is better named by a `BRIG_TELEPORT_REMOTE_BRIG` script that sets them |
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
two `brig rm` on `rp1`, which the brig of that run needed; one does it now.
hull's owner epoch was 2 at home.

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
hvi on the Pi then died on the first exec into the moved guest. hvi caps
its own allocator since `ebc1116`, which fixes that; see Known issues. On
the runs before that fix, with the allocator capped from outside hvi as a
stand-in, the guest carried on, and `brig sh claude -c ...` from the Mac
reached it on the Pi. Every exec after a move was such a command: no
interactive terminal was attached after a move. The Claude Code task itself
did not run, because the login brig held had expired. On 2026-10-15 the
session moved again, to `rp2`, on the streamed path and with hvi's fix:
every exec after the move worked, and nothing capped the allocator from
outside hvi. See [The streamed move](#the-streamed-move). The login had
still expired.

The builds of the runs before the fix were brig `658ade4`, hull
`0021196` and hvi `8697de0`, with the
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
unix socket path on macOS can have at most 104 bytes. The gateway's stream
socket is `<gateway dir>/sandbox-<sandbox>.sock.qemu`, as
`sandbox-brig-claude-code.sock.qemu` was on these runs. For a sandbox name
too long for that, Brig names the socket after a hash of the name,
`sandbox-<8 hex digits>.sock.qemu` (the control socket beside it is the
same name without `.qemu`), so it is the gateway directory that must
leave room for it; Brig refuses one that does not. The Mac's directories
were reached through a short link for that reason.

The sequence, with what each step gave:

1. **Pull the image on each host.** 634,430,785 bytes, shown as 605.0 MB by
   `hull images`. The Mac took 32.8 s through `hull pull`, and its store grew
   by 1.67 GB. `rp2` took 120.0 s and `rp1` 27.4 s, and each store grew by
   1.60 GB.
2. **Look at what the session forwards.** `brig plan claude` lists
   `GH_TOKEN (env) from the secret gh-token` and
   `.claude/.credentials.json (file) from the secret claude-credentials`.
   That was recorded on the Mac only.
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
   tmpfs. The failed turn's session file, 32,601 bytes, was there. So a
   conversation of this profile is expected to live in the guest's memory,
   not on its disk, to move with the guest, and to be gone when the sandbox
   stops. No session file was read after a move, so that is not shown yet.
6. **Move.** `brig teleport claude --to pmoust@rp2`. The numbers, from hull's
   records on both hosts (the clocks were 82 to 83 ms apart):

   | | to `rp2`, as built | to `rp1`, as built (2 runs) | to `rp1`, allocator capped outside hvi, before its fix |
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
   downtime is the transfer. The rate to `rp2` was not explained: the path
   there was not shown to be direct. `rp2` was running another project's
   builds. During the session `rp2` ran out of memory while a debug copy of
   this 4 GiB guest ran there beside those builds, and the debug copy
   probably triggered it. That copy also logged an RCU stall in its disk
   driver, so it is no clean control. The runs after the first went to
   `rp1`, which was idle. The guest's own count
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
8. **After the move, allocator capped outside hvi.** These runs came before
   hvi's fix. They capped glibc's malloc arenas through the environment of
   the destination's hull, which hvi inherits. That stand-in is obsolete:
   hvi caps itself now, and the cap the runs used is not enough in every
   case (see Known issues). With it, every exec worked. From the Mac,
   `brig sh` said
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
   is expected to break the same way, and to go on only if the agent
   connects again. The Claude Code task did not run, so that was not seen.
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
script/teleport-demo-claude.sh --prepare pmoust@rp2   # the streamed move
```

With brig `658ade4`, run before hvi's fix with the allocator capped
outside hvi on `rp1`, and brig's settings for this Mac in the environment,
it printed 18 passed and 0 failed. One of those checks, that
`brig teleport` exited 0, could not fail in that version: it read the
status of a `$(...)` substitution, not brig's. That move's commit is shown
by its `teleported` line and by the destination's hull record, `running`.
The script now takes brig's own status. Its `--claude` option also starts
the Claude Code task of step 5 and checks that the task's `COUNT` lines go
on after the move, from where they were, with no restart and no error
event. That option has not run, as it needs a login that has not expired;
its check has run only on made-up streams. `--prepare` prepares the
destination after the boot, and the script's numbers show a streamed
move's rounds and the rise in the guest's `downtime total`.

##### The streamed move

On 2026-10-15 the session moved from the Mac to `rp2` on the streamed
path, twice, through `script/teleport-demo-claude.sh --prepare
pmoust@rp2`, as of `2b39954`. The builds were brig `06340ae`, hull
`d2bb5e9` and hvi `c4131f5`, hvi's `feat/teleport`, which has the
allocator fix. hvi was built with `HVI_GIT_SHA` set to
that commit on both hosts, as hvi restores only a stream from the same
commit of hvi:
on the Mac with `cargo build --release`, signed ad hoc with
`hvi.entitlements`, and on `rp2` with `CARGO_BUILD_JOBS=3` in 56 s. brig
and hull for the Pi were cross-built. Each host had a state directory, a
gateway directory and a hull store of its own, the Mac's under a short
link as above and `rp2`'s under `~/h8d`, with an `env.sh` like the one
in [Running it on two hosts](#running-it-on-two-hosts). The image was
copied into both stores from the stores of the earlier runs.

That day the Mac reached the Pis from another network, over the
tailnet, at 1.0 to 1.1 MiB/s: 32 MiB through ssh took 31 s. The numbers
are in [How long it takes](#how-long-it-takes). The login brig held had
still expired (`expired 11d ago` at each boot), so the script ran
without `--claude`: the workload of `script/teleport-demo.sh` and the
HTTPS stand-in, and no Claude Code task.

Before the first run, a session of the same image on the Mac prepared
`rp2` once, which sent the base (790 s). Each run then printed 20 passed
and 0 failed:

- the prepare found the image, the portable kernel and the base held on
  `rp2`, in 12.1 and 47.1 s;
- `brig teleport` exited 0 and printed hull's rounds as they ended: disk
  round 0 in 0.06 to 0.07 s, memory round 0 in about 60 s, hvi's time
  cap, and the rounds taken in the pause;
- on `rp2` the counter went on (61 to 382, and 61 to 402), the deleted
  file still read its token, the sum and the `boot_id` were the ones from
  before, the kernel had logged one more `hvi-time` line, and the guest
  kept its address, `198.18.1.2`;
- hvi on `rp2` kept running. The first `brig sh claude@demo -c true`
  exited 0 in 0.82 and 0.84 s, and so did every exec after it. The
  kernel log on `rp2` has no seccomp record from these runs;
- the stand-in's download was reset when the guest resumed
  (`curl: (56) Recv failure: Connection reset by peer`), and the next
  attempt, 1 s later, downloaded from `rp2`;
- an exec on `rp2` had no `GH_TOKEN`, and brig there said it had no value
  for `claude-credentials` or `gh-token`. The stand-in, started on the
  Mac, still held `GH_TOKEN`;
- `rp2` ran the guest, and the Mac kept a stopped copy whose checkpoint
  is `migrated`.

The two fault cases, each on a fresh session of the same image, with
`rp2` prepared:

| Case | What ran | Exit | What was left |
| --- | --- | --- | --- |
| brig killed during memory round 0 | `kill -KILL` of `brig teleport` alone, 10 s into the move | brig `137`. hull outlived brig and committed the move 55 s after it began. `--abort` `1`: "committed, and it runs there. A committed move is not called off", from the brig of that run (from brig `304689b`, `--abort` of a moved session goes to the brig where it runs); `--status` `0`, and it recorded the host | `rp2` ran the guest, with 4.03 s of downtime by the guest's count; the Mac's copy was `migrated` |
| brig, hull and ssh killed during memory round 0 | `kill -KILL` of brig's process group, 33 s into the move, 23.5 s of which the destination spent copying the base | brig `137`. `--status` "has not finished", with `rp2` "receiving, last error: receive: the stream ended in hvi's ram-round-0: unexpected EOF". `--abort` `0` in 0.90 s: "hull runs it on this host again" | the guest ran on the Mac the whole time, not fenced: its counter went on with no gap over 0.5 s, and its kernel logged no restore. `rp2` had no instance and no record of the session |

The move back did not run. It goes from `rp2` to the Mac over ssh, and
the Mac had Remote Login off.

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
- Fixed in hvi `ebc1116`, with `fc10511`, on hvi's `feat/teleport` since
  (`0c4cdfd` when this was written): hvi `8697de0` on Linux died of SIGSYS
  on the first exec into a 4-vCPU guest that moved from a Mac, and printed
  nothing. Its vsock bridge started a reader and a writer thread for each
  agent connection. A new thread's first `free()` went through glibc's
  `tcache_init` and `arena_get2` to `__get_nprocs`, which opens
  `/sys/devices/system/cpu/online`, and hvi's seccomp filter does not allow
  `openat`. hvi's commit says that read happens only once more than eight
  arenas exist, which takes many threads allocating at once. That is likely
  why the 1-vCPU moves above did not hit it; it was not tested. glibc reads
  a second file, `/proc/sys/vm/overcommit_memory`, the first time the heap
  of an arena other than the main one shrinks, and a cap of more than one
  arena leaves that read in place: hvi records that a guest with heavy agent
  traffic still killed the VMM within a minute with a cap of four.

  So hvi caps itself at one arena. For a confined boot or restore,
  `one_malloc_arena()` calls `mallopt(M_ARENA_MAX, 1)` after hvi parses its
  arguments and before it starts any thread. glibc then never counts the
  CPUs, and has no other heap to shrink. Two probes of
  `hvi seccomp-selftest` reach the two reads under the `vmm` filter,
  "allocate on many threads at once (vmm)" and "free a thread's heap memory
  (vmm)". hvi's commit says both fail with SIGSYS on the code before it,
  and that the second still fails with an arena cap of four: a cap above
  one still leaves a heap other than the main one to shrink. The
  destination's hull needs no setting for it.

  The first Claude Code runs above came before that fix, and stood in for
  it with an arena cap in the environment of the destination's hull. That
  stand-in is obsolete. The streamed runs used hvi `c4131f5`, which has
  the fix, with no cap from outside, and every exec into the moved guest
  worked.
- hull `d2bb5e9`'s `migrate --status`, during a streamed move's rounds,
  says "The source is fenced and stays so until the move commits or is
  aborted", while the guest still runs on the source. The guest is fenced
  only from hvi's seal. `brig teleport --status` passes hull's text on as
  hull wrote it and decides only on hull's state word, so a hull that
  corrects the text needs no change in Brig.

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
brig rm ubuntu                       # removes the fenced copy left here, forgets the session
ssh me@rp2 'source ~/b4/env.sh && brig teleport ubuntu --to me@rp1'
brig sh ubuntu -c 'cat /tmp/count'   # runs here again
ssh me@rp2 'source ~/b4/env.sh && brig rm ubuntu'
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
brig rm claude                      # removes the fenced copy left here, forgets the session
```
