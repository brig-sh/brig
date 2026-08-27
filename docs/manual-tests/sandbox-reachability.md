# Manual test: can one sandbox reach another?

`docs/security.md` said that sandboxes share a broadcast domain, and that each
one can reach what the other listens on. Nothing measured it. This file is the
measurement. The claim is true on Linux and false on both macOS backends.

CI cannot run any of this. `ci.yml` runs on Linux only and never boots a VM.

## Result

| backend | can one sandbox reach another? |
| --- | --- |
| `hvi` on macOS | no |
| `vz` on macOS | no |
| Linux, shared network | **yes** |
| Linux, `--network isolated` | no |

## Method

Two sandboxes run on one network. Sandbox A listens on `0.0.0.0`. Sandbox B
connects to A.

A failed connection alone proves nothing. A closed port, a listener on
loopback, and a wrong address all look the same to B. Each run below first
proves that the listener answers from outside the guest. Only then does B try.

These controls are not ceremony. On Linux the first attempt used `httpd`, which
the image does not contain. The controls caught the dead listener before anyone
read it as isolation.

## macOS, `hvi`

hull 0.1.0-rc21, `claude-code-stock`, sandboxes at `10.87.0.4` and `10.87.0.5`.

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

## macOS, `vz`

Same host. vmnet gave the sandboxes `192.168.64.7` and `192.168.64.8`.

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

`--network none` gives a guest with `lo` only, no route, and no egress. The
same image on the default bridge has `eth0` and reaches the internet. This is
true on Linux and on both macOS backends.

## What it means

On macOS the separation is real, but it is a property of the backend. brig
asks for a shared network and gets guests that cannot address each other. No
test in brig would notice if a runtime change removed this.

On Linux the separation does not exist by default. `--network isolated` adds
it.
