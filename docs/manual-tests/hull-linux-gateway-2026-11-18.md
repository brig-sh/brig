# Manual test: hull on Linux on a network gateway

*Measured on 2026-11-18 on a fresh rootless install of
brig-standalone-linux v0.1.0-rc22 (hull `4bf76a6`, urunc `5c6bc70`, hvi
`0.1.0-rc.3`, hull-assets `0.1.12`, guest kernel 6.18.59) with Brig built
from `main` at `cf80f90`, on an amd64 host (Ubuntu 24.04, kernel 6.8, 16
cores, KVM). The runtime was `BRIG_RUNTIME=hull`. The `ubuntu` profile was
the guest, a root shell on `docker.io/library/ubuntu:latest`. Raw logs are
on the host, not in this repository.*

Brig puts a hull on Linux sandbox on a hull network gateway since #667:
a gateway of its own when `isolated`, the shared one when `shared`. This
page records what that gives on a real host, and what the gateway does
for the things Brig still refuses there: an egress policy, a divert and
a published port. CI boots no VM, so this run is the only check of these
cases on a guest.

## Results

| Case | Result |
| --- | --- |
| `--network offline`, rootless | Boots in 5 s. Guest is root, kernel 6.18.59. No cgroup failure on this install. |
| No posture named | `isolated`, on a gateway of the sandbox's own (`198.18.1.2/30`); `https://example.com` answers 200. |
| Two isolated sandboxes | Each on its own `/30` and its own gateway socket. A listener in one, confirmed on loopback, times out from the other (curl exit 28). |
| Two shared sandboxes | Both on `gateway-198-18-0-0_24.sock`; the second reaches the first's HTTP server (200). |
| Shared to isolated | Times out (curl exit 28). |
| `brig rm --all -y` | With two shared and one isolated sandbox: every gateway, socket and sandbox gone at 0, 5 and 20 s. One earlier run left a sandbox behind with `hull rm: instance is running, use --force`; a second `brig rm` removed it. |
| Egress policy, at hull level | See below: enforced. |
| Diverts, at hull level | See below: `brig.internal` and a link-local divert both answered by the host socket. |
| Published port, at hull level | See below: `/forwards` publishes and withdraws. |
| Image with a named `USER` | Refused before boot: hull on Linux takes only a numeric uid and gid, or root. The community images set a name, so only their `:root` tags run. |

### Egress policy

Brig refuses a policy on hull on Linux at the time of this run, so the
gateway was started by hand with the flags Brig passes on macOS:

```bash
hull network-gateway --socket "$GW" --api "$API" \
  --subnet 198.18.9.0/30 --gateway-ip 198.18.9.1 \
  --egress-default deny \
  --egress-allow host=example.com --egress-allow 'host=*.example.com' \
  --egress-allow 'host=*.ubuntu.com' &
hull run -d --name eg2 --net shared --gateway-sock "$GW" \
  --gateway-cidr 198.18.9.2/30 --hypervisor hvi \
  docker.io/library/ubuntu:latest -- sleep infinity
```

From the guest, after `apt-get install curl` through the allowed
`*.ubuntu.com`:

| Host | curl |
| --- | --- |
| `https://example.com/` | 200 |
| `https://www.example.com/` | 200 |
| `https://github.com/` | exit 6, name not resolved |
| `https://one.one.one.one/` | exit 6, name not resolved |

The gateway logged `egress: refused the A query github.com. from
198.18.9.2` and the same for the AAAA query and for `one.one.one.one`.
The rules hold for a urunc/hvi guest on Linux as they do on macOS: it is
the same binary and the same gateway.

### Diverts

```bash
hull network-gateway --socket "$GW" --api "$API" \
  --subnet 198.18.10.0/30 --gateway-ip 198.18.10.1 \
  --host brig.internal=198.18.10.1 \
  --divert 198.18.10.1:443="$D1" --divert 169.254.170.2:80="$D2" &
```

`$D1` and `$D2` were unix sockets on the host with a small HTTP server
behind each. From the guest, `getent hosts brig.internal` gave
`198.18.10.1`; `curl http://brig.internal:443/` answered with the body of
the server on `$D1`; `curl http://169.254.170.2/` was answered by the
server on `$D2` (exit 0); `curl http://198.18.10.1:8443/`, a port of the
gateway with no divert, was refused (exit 7).

### Published port

With an HTTP server on the guest's port 8080 and the gateway's `--api`
socket:

| Step | Answer |
| --- | --- |
| `POST /forwards {"protocol":"tcp","local":"127.0.0.1:18080","remote":"198.18.10.2:8080"}` | 201, the forward as installed |
| `curl http://127.0.0.1:18080/` on the host | the guest's page |
| `GET /forwards` | the one forward |
| `DELETE /forwards?protocol=tcp&local=127.0.0.1:18080` | 200 |
| `curl http://127.0.0.1:18080/` on the host | refused (exit 7) |
| `GET /members` | the guest, transport `qemu` |

## Notes for the next run

- Set `DEBIAN_FRONTEND=noninteractive` inside the guest command. The
  host's environment does not reach the login shell `brig sh` opens, and
  `python3` pulls `tzdata`, whose debconf prompt parks `apt-get` on
  `dpkg-preconfigure` until the sandbox is removed.
- `ubuntu:latest` ships no `curl`, `ip`, `nc` or `python3`; install them
  through the gateway first. `apt-get` fails with exit 100 while an earlier
  one holds the dpkg lock.
- A one-shot `nc -l` server answers on loopback and gives curl exit 56 from
  another guest; use `python3 -m http.server` for the reachability cases.
- The shared gateway's log repeats `cannot receive packets from @,
  disconnecting: cannot read size from socket: EOF` while sandboxes come
  and go. Nothing failed because of it.
