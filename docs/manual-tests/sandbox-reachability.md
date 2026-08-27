# Manual test: can one sandbox reach another?

`docs/security.md` said, under "Things brig does not claim", that sandboxes
share a broadcast domain and that each can reach whatever the other is
listening on. Nothing measured it. This is the measurement, and it says the
claim was true on one of the three backends and false on the other two.

CI cannot run any of it: `ci.yml` is Linux-only and never boots a VM, so the
macOS half has no machine-verifiable form at all, and the Linux half needs a
real containerd rather than the stub `script/smoke.sh` drives.

## The method, and why it is shaped this way

Two sandboxes on the same network. A listener bound to `0.0.0.0` in the first,
then a connection attempted from the second.

A failed connection on its own proves nothing: a closed port, a listener bound
to loopback, or a wrong address all look the same from the far end. So every
run below first proves the listener is reachable **from off the guest** --
from the host, and from the guest's own non-loopback address -- and only then
tries the peer. Without those two controls a negative result is not evidence.
They earned it: on Linux the first attempt used `httpd`, which is absent from
the image, and the controls caught the broken listener before it could be
read as isolation.

The ARP entry on the far side is the second signal. `FAILED` means the peer
never answered at layer 2; a resolved `lladdr` means it did.

## macOS, `hvi`

Host: macOS 26.x, Apple Silicon, hull 0.1.0-rc21, `claude-code-stock`.
Sandboxes `10.87.0.4` and `10.87.0.5` on the shared `/24` of the day.

| check | result |
| --- | --- |
| listener in A, via A's own address | HTTP 200 |
| host to A | not run on this backend |
| **B to A** | **failed to connect** |
| B's ARP for A | `FAILED` |
| B's own egress | HTTP 404 from `api.anthropic.com`, so B's network works |
| A's access log | only A's own request; B's never arrived |

## macOS, `vz`

Same host. vmnet DHCP put the sandboxes on `192.168.64.7` and `192.168.64.8`.

| check | result |
| --- | --- |
| listener in A, via A's own address | HTTP 200 |
| **host to A** | **HTTP 200**, logged as `192.168.64.1` |
| **B to A** | **failed to connect**, twice |
| B's ARP for A | `FAILED` |
| B's own egress | HTTP 404 from `api.anthropic.com` |
| A's access log | two entries, A's own and the host's; nothing from B |

The host request is what makes this conclusive rather than suggestive. The
listener is provably reachable from outside the guest, so the peer's failure
is not a firewall in the guest, a bad bind, or a closed port.

## Linux, nerdctl

Host: Amazon Linux 2023, kernel 6.18.41, x86_64, root, no user-mode
networking. nerdctl 2.0.3, containerd 2.0.2, the CNI plugins from the
`nerdctl-full` bundle, `alpine`.

Two containers started with **no `--network` flag**, which is exactly what
brig passes in the shared case (`internal/runtime/nerdctl.go`), so they land
on the default bridge `nerdctl0`, `10.4.0.0/24`, at `.3` and `.4`.

| check | result |
| --- | --- |
| listener in A (`nc`), via A's own address | `ok` |
| **host to A** | `ok` |
| **B to A** | **`ok` -- it reached the listener** |
| B's ARP for A | `lladdr 52:40:33:73:d9:98 ... REACHABLE` |
| B's own network | internet reachable, bridge gateway reachable |

## Offline

`--network none`, which is what `brig --offline` becomes on this runtime,
gives a guest with only `lo`, zero routes and no egress. The same image on the
default bridge has `eth0` and reaches the internet. Checked on Linux here and
on both macOS backends when `--offline` was added.

## What it means

The backends do not agree, so no single sentence covers them:

| backend | can one sandbox reach another? | why |
| --- | --- | --- |
| `hvi` | no | the gvisor gateway gives each guest a point-to-point link and NATs outbound; there is no segment between guests |
| `vz` | no | vmnet does not bridge guest to guest in the mode hull uses |
| Linux | yes | the CNI bridge is an ordinary layer 2 segment |

On macOS the separation is a property of the backends rather than something
brig asks for, so it holds today and nothing enforces that it keeps holding.
That is what the regression test alongside this file is for. On Linux it is
absent and has to be built.
