# Manual test: can one sandbox reach another?

*The [2026-09-30 run](#2026-09-30-default-isolation-on-macos-hvi) validates
the default-network change on macOS `hvi`. Earlier sections are historical
evidence across hvi, vz, and Linux's shared and isolated networks. The first
runs have no calendar date. They are identified by version: hull
`0.1.0-rc21` on macOS, and Amazon Linux 2023 (kernel 6.18.41) with nerdctl
2.0.3 and containerd 2.0.2 on Linux. The section
[macOS, `hvi`, hull 0.1.0-rc29](#macos-hvi-hull-010-rc29) is a later run, on
2026-09-27, with hull `0.1.0-rc29` on macOS 26.7.*

`docs/security.md` said that sandboxes share a broadcast domain, and that each
one can reach what the other listens on. Nothing measured it. This file is the
measurement. On hull 0.1.0-rc21 the claim was true on Linux and false on both
macOS backends. On hull 0.1.0-rc29 it is true on `hvi` too. `vz` has not been
measured since rc21.

CI cannot run any of this. `ci.yml` runs on Linux only and never boots a VM.

## Recheck the default with real VMs

[`script/network-isolation-vm.sh`](../../script/network-isolation-vm.sh)
checks what a new sandbox gets when the run names no network. Run it on
macOS with hull's `hvi` backend, or on Linux with nerdctl and the
`io.containerd.urunc.v2` shim. It requires host `curl` and a guest image
with `bash`, `curl`, `ip` and `python3`; the default image is
`ghcr.io/brig-sh/claude-code-stock:root`.

From the repository root, compare a binary from before the default change
with the new build:

```bash
BRIG=/absolute/path/to/previous-brig script/network-isolation-vm.sh --expect reachable
BRIG="$PWD/brig" script/network-isolation-vm.sh --expect reachable # must fail: exit 1
BRIG="$PWD/brig" script/network-isolation-vm.sh --expect isolated
```

The first command reproduces the old behavior and exits `0`. The same
reachability expectation against the fixed binary must exit `1`: the
client, started without `--network`, cannot reach the other sandbox's
listener. The third command is the regression check and must exit `0`.
Each command first runs a shared control: a separate pair with explicit
`--network shared` must connect. `--skip-shared-control` omits that check.

A blocked request only counts after the host has reached the listener
through a published port and the client has reached an outbound HTTPS
endpoint. The default controls use `127.0.0.1:18369` and
`https://example.com`; `--publish-port` and `--outbound-url` override them.
`--image` selects another image with the required guest tools.

The script creates private temporary profile, state, guest-home and gateway
directories, and cleans up only the new sandbox refs it created. Exit `0`
means the controls passed and the observed result matched `--expect`;
exit `1` means the opposite reachability was observed; exit `2` means
setup or a control left the result uncertain. `--self-test` exercises the
harness without a VM and does not establish runtime isolation.

## 2026-09-30: default isolation on macOS hvi

Tested on macOS 26.6.2 (25G83), arm64, with hull `0.1.0-rc29`
(`e54923f`) and its `hvi` backend. The baseline was built from `2e4de3f83736`
before the production edits for #369; the patched binary was built from the
working tree based on that same commit. Both reported
`v0.3.1-0.20260929191113-2e4de3f83736+dirty`. The guest image was
`ghcr.io/brig-sh/claude-code-stock:root`, verified and booted at digest
`sha256:070d601482c07c9fe09bbda5ee0d841abed9924a2b36a44e6dcf9a265cb99d96`.

With the saved baseline named `brig-before` and the patched build named
`brig`, these were the three invocations (binary locations normalized):

```bash
BRIG="$PWD/brig-before" script/network-isolation-vm.sh --expect reachable # exit 0
BRIG="$PWD/brig" script/network-isolation-vm.sh --expect reachable        # exit 1
BRIG="$PWD/brig" script/network-isolation-vm.sh --expect isolated         # exit 0
```

Each invocation first passed its explicit `--network shared` positive
control. The target pair received no `--network` flag:

| binary and expectation | B to A | script exit |
| --- | --- | --- |
| baseline, reachable | A's unique marker from `198.18.0.2:8080`; curl `0`, peer `198.18.0.2` | `0` |
| patched, reachable | timeout to `198.18.1.2:8080`; curl `28`, empty peer | `1` |
| patched, isolated | timeout to `198.18.1.2:8080`; curl `28`, empty peer | `0` |

Before **and** after each cross-guest probe, A fetched its own marker, the
host fetched it through `127.0.0.1:18369`, and B fetched the outbound HTTPS
control at `https://example.com`. The guests kept their boot IDs. Probes
and the listener used native runtime exec after Brig booted the sandboxes;
they could not silently replace a sandbox through `brig sh`. Cleanup
removed the test sandboxes, private gateways and scratch directories, and
`hull ps` reported no remaining instances.

During the patched pair's boot, its two separate gateway processes used
32,752 KiB and 32,528 KiB RSS (65,280 KiB combined). These are process
snapshots, including any shared pages counted in both; guest RAM and other
VMM processes are additional. They do not establish a timing or memory
performance bound.

Separate one-sandbox runs of the patched binary measured the complete
`brig run -d` command, including verification, with the image and boot
assets already cached:

| network | fresh-VM command wall time | gateway RSS |
| --- | --- | --- |
| explicit shared | 12.795 s | 30,624 KiB |
| default isolated | 12.476 s | 32,656 KiB |

There was one sample per mode, with no distribution or repeated timing
trial. These observations do not establish a speed improvement.

Both modes also passed dynamic publication with an existing guest listener:

```bash
brig network publish "$ref" 18470:8080
brig network unpublish "$ref" 18470
```

Between these commands, host `curl` fetched the listener's marker through
`127.0.0.1:18470`. After unpublishing, the host connection failed with curl
exit `7`; the listener still answered through native guest exec and the
guest boot ID was unchanged.

Three earlier attempts were rejected as inconclusive: a harness output
overwrite, a failed host-publish control, and a changed guest boot ID while
probing through `brig sh`. They are not isolation evidence. The accepted
runs above used the corrected harness and a fresh gateway for each pair.
Linux's real nerdctl/urunc path was **not tested** in this validation.

### Follow-up: preserve sessions without a posture record

The review of `87b4a83` exposed a separate upgrade failure: an indexed,
running sandbox without a posture record inherited the new profile default.
The regression test `TestAnUnrecordedSandboxKeepsItsActualNetworkOnUpgrade`
failed before the follow-up fix using the shipped `ubuntu` profile. Shared
and offline guests were reported as isolated; the hvi stub restarted them,
while the nerdctl stub reused them with the wrong report. The test now checks
all three postures through `Load` and `EnsureRunning`, with no change to the
shipped profile's `network:` field.

On the same macOS/hull setup described above, the follow-up working tree
based on `87b4a83` also passed a real hvi check. This simulated missing legacy
metadata on current hull; it did not install an older Brig or hull release.
For each posture, a credential-free scratch profile declared
`network: isolated`, and the test:

1. Booted it with an explicit `--network shared`, `isolated` or `offline`.
2. Captured its boot ID using native `hull exec`, then deleted only its
   entry in the private gateway directory's `networks.json`, leaving the
   session index and address allocations intact.
3. Checked both text and JSON `brig info`, then ran `brig run -d` without
   a network flag and captured the boot ID again through native exec.

| existing network | `brig info` after removing the record | flagless run |
| --- | --- | --- |
| shared | shared | same boot ID |
| isolated | isolated | same boot ID |
| offline | offline | same boot ID |

Neither form of `info` changed the session index or posture file. All three
guests were removed afterward, private gateways and scratch directories
were cleaned up, and the temporary runtime store was detached. Linux's
real nerdctl/urunc path remains **untested**; its recovery and explicit
posture changes are covered by fixture and lifecycle tests only.

## 2026-10-01: legacy Hull gateway spec recovery

A working tree based on `0af38cc`, with `.spec` recovery added, passed a real
HVI check on macOS with Hull `v0.1.0-rc29`. The test used private profile,
state, workspace and gateway directories and a cloned runtime store. Legacy
metadata was simulated by removing only the guest's posture entry; this did
not boot an older Brig binary.

| case | result |
| --- | --- |
| Running isolated guest, posture entry removed, `.spec` retained | Text and JSON `info` reported isolated without writing records. A flagless run kept the boot ID, PID and VMM argv, then saved the recovered posture. |
| Posture entry removed again, then ordinary `brig stop` | Stop deleted `.spec` and retained Hull's stopped argv. `info` reported unknown. A flagless run was refused, with no new VM or gateway and no changed runtime or posture record. |
| Explicit `--network isolated` after that refusal | Boot succeeded with a new boot ID and a recreated `.spec`. |

The test removed its guest and gateway; the private store was detached.
The tested binary's SHA256 was
`9364f0825fb981197ac0a29724710016245fd82bf51afeb3f78576b1a92b5ece`.
Unit tests also cover an explicit shared restart, unreadable or empty specs,
old spec formats, hashed socket names and reading beside a previous gateway
path. This does not verify a legacy shared override with a stale isolated
spec; that [recovery ambiguity](../policies.md#network-postures) remains.

## Result

This table summarizes the earlier backend measurements below; the dated
section above records validation of the new default.

| backend | can one sandbox reach another? |
| --- | --- |
| `hvi` on macOS, shared network | **yes** (hull 0.1.0-rc29) |
| `hvi` on macOS, `--network isolated` | no (hull 0.1.0-rc29) |
| `vz` on macOS | not measured on a current hull |
| Linux, shared network | **yes** |
| Linux, `--network isolated` | no |

## Method

Two sandboxes run on one network. Sandbox A listens on `0.0.0.0`. Sandbox B
connects to A.

A failed connection alone proves nothing. A closed port, a listener on
loopback, and a wrong address all look the same to B. So before a "no" counts,
each run below shows that A's listener works: the host reaches it, or a packet
capture shows why B got nothing. A "yes" needs no such control, because B gets
content that only A holds.

These controls are not ceremony. On Linux the first attempt used `httpd`, which
the image does not contain. The controls caught the dead listener before anyone
read it as isolation.

## macOS, `hvi`, hull 0.1.0-rc29

2026-09-27. macOS 26.7 (25G229), hull 0.1.0-rc29, `hvi`, the `claude`
profile. The shared run was done twice: once with the released brig v0.3.0,
and once with a pre-release build that differs from v0.3.0 only in docs and
comments. Both gave the same answer. The isolated run used that build.
[#364](https://github.com/brig-sh/brig/issues/364) reported the shared result
first.

A is `claude@r364b` and serves a file that holds a fresh UUID and exists only
in A. B is `claude@r364a`. The v0.3.0 run used the labels `r364d` and `r364c`
and got the same addresses.

```bash
brig run -d claude@r364a /tmp/pa     # B, 198.18.0.2
brig run -d claude@r364b /tmp/pb     # A, 198.18.0.3
brig sh claude@r364b 'mkdir -p /tmp/srv && cd /tmp/srv &&
  echo "only-in-r364b $(cat /proc/sys/kernel/random/uuid)" > who.txt &&
  exec python3 -m http.server 8080 --bind 0.0.0.0' &
brig sh claude@r364a 'curl -sS --max-time 10 http://198.18.0.3:8080/who.txt'
```

The isolated run is the same, with `--network isolated` on both `brig run`
lines. Each sandbox then has a `/30` of its own, in `198.18.1.0/24`.

| check | shared network | `--network isolated` |
| --- | --- | --- |
| addresses | A `198.18.0.3/24`, B `198.18.0.2/24` | A `198.18.1.6/30`, B `198.18.1.2/30` |
| A to its own address | the file | the file |
| host to A, through `--publish 18364:8080` | not run | the file, logged from `198.18.1.5` |
| B to the internet | not run | HTTP 200 from `example.com` |
| **B to A** | **the file**, with A's UUID | **timeout** (curl exit 28), twice |
| B to an unused address | `198.18.0.77`: curl exit 7 after 3 s | `198.18.1.77`: timeout |
| access log of A | a request from `198.18.0.2` | requests from A and the gateway, none from `198.18.1.2` |

On the shared network, B got a file that only A holds. The content proves the
listener, so the shared column needs no other control. The unused address
fails, so the gateway does not answer for every address in the subnet.

On the isolated network, the host reached A's listener through a published
port, and B reached the internet. B still timed out to A, once right after
the host control, and A's log has no request from B. `--publish` gives the
host control that the rc21 section says this backend lacks.

What changed the answer since rc21 is not known. At least three things
differ between the two runs: the hull release, the brig release, and the
shared subnet (`10.87.0.0/24` then, `198.18.0.0/24` now). Brig hands out
those addresses itself. The rc21 run did not record its macOS version.

## macOS, `hvi`, hull 0.1.0-rc21 (superseded)

This is no longer the current answer for `hvi`. See the rc29 section above.

hull 0.1.0-rc21, `claude-code-stock`, sandboxes at `10.87.0.4` and `10.87.0.5`
on the shared subnet of the time.

A packet capture on both guests, with a raw `AF_PACKET` socket, shows the
mechanism:

| where | what it saw |
| --- | --- |
| A | `3x ARP who-has 10.87.0.4 tell 10.87.0.5` |
| A | `7x ARP reply` sent back to B |
| A | no TCP at all |
| B | its own 3 ARP requests, and no reply |

So the guests do share a broadcast domain. B's ARP broadcast reaches A, and A
answers. The gateway does not forward the unicast reply back to B. B never
learns the MAC address of A, so B never sends a SYN, and A receives no TCP.

The host cannot reach the guest either, because the gvisor gateway runs in user
space and the host has no route to that subnet. This is why the capture matters
here: the usual host control is not available on this backend.

## macOS, `vz`, hull 0.1.0-rc21

Not measured since. Same host as the rc21 `hvi` run. vmnet gave the
sandboxes `192.168.64.7` and `192.168.64.8`.

| check | result |
| --- | --- |
| A to its own address | HTTP 200 |
| **host to A** | **HTTP 200**, logged as `192.168.64.1` |
| **B to A** | **no connection**, twice |
| B's ARP entry for A | `FAILED` |
| B to the internet | HTTP 404 from `api.anthropic.com` |
| access log of A | one entry from A, one from the host, none from B |

The host reaches the listener, so the failure at B is not a firewall in the
guest, a bad bind, or a closed port.

## Linux

Amazon Linux 2023, kernel 6.18.41, x86_64, root, no user-mode network. nerdctl
2.0.3, containerd 2.0.2, CNI from the `nerdctl-full` bundle, `alpine`.

Two containers start with no `--network` flag, which is what brig passes for
the shared posture. Both land on the default bridge `nerdctl0`, `10.4.0.0/24`.

| check | shared network | `--network isolated` |
| --- | --- | --- |
| A to its own address | `ok` | `ok` |
| host to A | `ok` | `ok` |
| **B to A** | **`ok`** | **timeout** |
| B's ARP entry for A | `REACHABLE` | none |
| B to the internet | `ok` | `ok` |
| subnets | one, `10.4.0.0/24` | two, `10.4.1.0/24` and `10.4.2.0/24` |

The isolated column is the same test with a network for each sandbox. brig
creates that network in `Run` and removes it in `Remove`.

## Offline

`--network offline` (`--net none` at hull, `--network none` at nerdctl) gives
a guest with `lo` only, no route, and no egress. The
same image on the default bridge has `eth0` and reaches the internet. This is
true on Linux and on both macOS backends.

## What it means

On `hvi` and on Linux, the shared network does not separate sandboxes.
`--network isolated` does. `vz` is not measured on a current hull, and `vz`
refuses `--network isolated`.

The `hvi` answer changed between rc21 and rc29, and no test in brig noticed.
The real-VM harness now checks the default and an explicit shared control.
CI still does not boot a guest to run it.
