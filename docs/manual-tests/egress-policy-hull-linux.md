# Manual test: is an egress policy enforced on hull on Linux?

*Measured on 2026-11-18 on brig-standalone-linux v0.1.0-rc22, installed
rootless on an amd64 host: hull `4bf76a6`, urunc `5c6bc70` and hvi
0.1.0-rc.3. The guest image was `ubuntu`.*

[egress-policy.md](egress-policy.md) measures the same question on hull's
`hvi` backend on macOS, and [egress-policy-linux.md](egress-policy-linux.md)
measures nerdctl. This file measures hull on Linux, where the sandbox's only
way out is a hull network gateway, the same binary as on macOS.

This run drove hull directly, not `brig run`. Brig hands the gateway the same
arguments, `--egress-default` and `--egress-allow`, but no run of `brig run`
with a policy attached on a Linux host is recorded here.

## The run

The gateway:

```console
$ hull network-gateway --egress-default deny \
    --egress-allow host=example.com \
    --egress-allow host=*.example.com \
    --egress-allow host=*.ubuntu.com
```

The guest, on that gateway:

```console
$ hull run --net shared --gateway-sock <sock> --gateway-cidr 198.18.9.2/30 \
    --hypervisor hvi ubuntu
```

## Results

| request | wanted | result |
| --- | --- | --- |
| `curl https://example.com` | reached | `200` |
| `curl https://www.example.com` | reached | `200` |
| `curl https://github.com` | refused | curl exit 6, no resolution |
| `curl https://one.one.one.one` | refused | curl exit 6, no resolution |

The gateway logged `egress: refused the A query github.com. from 198.18.9.2`,
and the A and AAAA refusals for `one.one.one.one`.

## What this does not show

- A divert, `brig.internal` and a published port. Brig still refuses them on
  hull on Linux.
- An image with a non-root `USER`. See
  [Limitations of hull on Linux](../runtimes.md#limitations-of-hull-on-linux).
