# Manual test: is an attached policy actually enforced?

`docs/policies.md` used to say that nothing read a policy's rules at boot. It
now says a bound policy is enforced at the gateway brig gives the sandbox.
This file is the measurement behind that sentence.

CI cannot run any of this. `ci.yml` runs on Linux only and never boots a VM,
and the enforcement point is a macOS backend.

## Result

A guest behind a gateway started with `--egress-default deny` and one
`--egress-allow host=api.anthropic.com`:

| what the guest tried | result |
| --- | --- |
| `https://api.anthropic.com/` -- allowed by name | reached it: HTTP 404 from Anthropic's edge |
| `https://example.com/` -- not allowed | `curl` exit 6, could not resolve the host |
| `https://1.1.1.1/` -- straight to an address, no `cidr` allow | `curl` exit 7, could not connect |

The three are three different mechanisms, which is why all three are here.
Under a `deny` default the gateway's resolver answers only names an allow glob
covers, so a denied name fails at resolution rather than at connect. An address
dialled directly never asks for a name, so it is the filter itself that refuses
it. And the allowed name proves the sandbox is not simply offline -- without
that row the other two would pass on a guest with no network at all.

## Method

Versions: hull built from `main` at `a7a5d1e` (the first hull that takes the
egress flags; `0.1.0-rc21` and earlier have no `--egress-default`), brig from
this branch, on macOS arm64.

Start a gateway on one sandbox's `/30` with a policy on it, exactly as brig
starts one:

```bash
hull network-gateway \
  --socket "$D/e2e.sock" --qemu-socket "$D/e2e.sock.qemu" \
  --subnet 198.18.1.0/30 --gateway-ip 198.18.1.1 \
  --egress-default deny --egress-allow 'host=api.anthropic.com'
```

It says what it understood, which is the first thing worth checking:

```
network-gateway on .../e2e.sock (subnet 198.18.1.0/30, gw 198.18.1.1,
0 forwards, egress default deny (1 allow, 0 deny))
```

Boot a guest behind it, on the guest address brig would assign:

```bash
hull run --detach --name it-egress --hypervisor hvi --net shared \
  --mem 2048 --cpus 2 \
  --gateway-sock "$D/e2e.sock" --gateway-cidr 198.18.1.2/30 \
  ghcr.io/brig-sh/opencode-stock:latest
```

Confirm the guest is on the `/30` and routes through the gateway, so that a
later failure cannot be a guest that never had a network:

```console
$ hull exec it-egress -- ip addr show eth0
    inet 198.18.1.2/30 brd 198.18.1.3 scope global eth0
$ hull exec it-egress -- ip route
default via 198.18.1.1 dev eth0
198.18.1.0/30 dev eth0 proto kernel scope link src 198.18.1.2
```

Then the three attempts:

```console
$ hull exec it-egress -- curl -s -o /dev/null -w '%{http_code}\n' --max-time 12 https://api.anthropic.com/
404
$ hull exec it-egress -- curl -s -o /dev/null -w '%{http_code}\n' --max-time 12 https://example.com/
000   # exit 6: could not resolve host
$ hull exec it-egress -- curl -s -o /dev/null -w '%{http_code}\n' --max-time 10 https://1.1.1.1/
000   # exit 7: could not connect
$ hull exec it-egress -- getent hosts example.com
      # no output: the resolver refused the name
```

## What this does not measure

That two sandboxes behind separate gateways cannot reach each other. That is
the same question `sandbox-reachability.md` answers for the shared network, and
a network per sandbox only narrows it -- but it is not measured here, and one
guest cannot measure it.

The filter itself is hull's, and hull tests it directly over its own netstack:
`internal/netgw/egress_e2e_test.go` carries real TCP through the stack for the
deny default, an allowed CIDR, a pinned name, a deny under an allow default,
and the unfiltered case. Those ran green against the same build used here. What
this file adds is that the rules survive the trip through a real guest, a real
kernel and a real resolver, which no unit test on either side covers.
