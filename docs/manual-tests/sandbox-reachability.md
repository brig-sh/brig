# Manual test: can one sandbox reach another?

*Historical evidence, not current validation. It evidences whether one
sandbox can reach another, across hvi, vz, and Linux's shared and isolated
networks. The first runs have no calendar date. They are identified by
version: hull `0.1.0-rc21` on macOS, and Amazon Linux 2023 (kernel 6.18.41)
with nerdctl 2.0.3 and containerd 2.0.2 on Linux. The section
[macOS, `hvi`, hull 0.1.0-rc29](#macos-hvi-hull-010-rc29) is a later run, on
2026-09-27, with hull `0.1.0-rc29` on macOS 26.7.*

`docs/security.md` said that sandboxes share a broadcast domain, and that each
one can reach what the other listens on. Nothing measured it. This file is the
measurement. On hull 0.1.0-rc21 the claim was true on Linux and false on both
macOS backends. On hull 0.1.0-rc29 it is true on `hvi` too. `vz` has not been
measured since rc21.

CI cannot run any of this. `ci.yml` runs on Linux only and never boots a VM.

## Result

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
Nothing in brig checks it now.
