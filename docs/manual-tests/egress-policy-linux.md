# Manual test: is an attached policy enforced on Linux?

*Measured on 2026-10-04 with brig from the change that added this file, on
an Ubuntu 24.04 amd64 host (kernel 6.8.0-110). The runtime was a rootless
home install of the Linux runtime bundle: nerdctl 2.3.5, rootlesskit 3.2.0
with slirp4netns 1.3.5, and the urunc shim. nftables 1.0.9.*

[egress-policy.md](egress-policy.md) measures the same question on hull's
`hvi` backend. This file measures nerdctl, where Brig enforces a policy in
nftables on the sandbox's own bridge and answers its DNS. See
[How nerdctl enforces a policy](../policies.md#how-nerdctl-enforces-a-policy).

CI cannot run any of this, since it boots no VM. `internal/egress` tests the
rules, the DNS handling and the resolver over loopback. This file adds a real
guest, a real kernel and real resolvers.

## The conformance cases

The cases are the ones `test/conformance` runs on `hvi`, with its two policy
documents unchanged. That runner boots `hvi` only, so the cases were run by
hand. The guest image was `ghcr.io/brig-sh/claude-code-stock:root`, which
carries curl and a CA bundle, and the probe was `test/netprobe`, as the
suite uses them. Each case ran under its policy and again in a sandbox with
no policy.

`conformance-deny`, `default: deny`:

| case | wanted | under the policy | with no policy |
| --- | --- | --- | --- |
| allowed-name `tcp example.com:443` | reached | reached | reached |
| allowed-glob `tcp deb.debian.org:443` | reached | reached | reached |
| allowed-cidr `tcp 1.1.1.2:443` | reached | reached | reached |
| allowed-resolver `dns 1.1.1.2 example.com` | reached | reached | reached |
| denied-name `tcp example.net:443` | resolve-refused | resolve-refused | reached |
| ipv4-literal `tcp 9.9.9.9:443` | connect-refused | connect-refused | reached |
| ipv6-literal `tcp [2606:4700:4700::1111]:443` | no route | no-route | no-route |
| alternate-resolver `dns 8.8.8.8 example.net` | timeout | timeout | reached |
| dns-over-https `doh https://8.8.8.8/dns-query` | connect-refused | connect-refused | reached |
| dns-over-tls `dot 8.8.8.8` | connect-refused | connect-refused | reached |
| deny-over-allow-host `tcp www.debian.org:443` | resolve-refused | resolve-refused | reached |
| deny-over-allow-cidr `tcp 1.1.1.1:443` | connect-refused | connect-refused | reached |
| proxy-env, curl through a proxy on the host | connect-refused | connect-refused | error-after-connect |
| host-service, a listener on the host's address | connect-refused | connect-refused | reached |
| metadata `tcp 169.254.169.254:80` | connect-refused | connect-refused | timeout |

`conformance-allow`, `default: allow`:

| case | wanted | under the policy | with no policy |
| --- | --- | --- | --- |
| allowed-name `tcp example.com:443` | reached | reached | reached |
| glob-literal, the address of `git.kernel.org` never looked up | known gap | reached | reached |
| denied-glob `tcp git.kernel.org:443` | connect-refused | connect-refused | reached |
| denied-name `tcp example.net:443` | connect-refused | connect-refused | reached |
| denied-name-literal, the address of `example.net` | connect-refused | connect-refused | reached |
| denied-cidr `tcp 9.9.9.9:443` | connect-refused | connect-refused | reached |
| alternate-resolver `dns 8.8.8.8 git.kernel.org` | known gap | reached | reached |
| dns-over-https, a glob-denied name | known gap | reached | reached |
| dns-over-tls, a glob-denied name | known gap | reached | reached |

Every case under a policy ended the way the suite requires on `hvi`. Every
denied case reached its target, or got past the step the policy stops, with
no policy. The terminal capture lost the no-policy line of ipv6-literal, and
a probe through `nerdctl exec` gave `no-route` for it. The guest has no IPv6
route on this host with or without a policy, as on `hvi`. The metadata
address does not answer this host, so that case is unproven on Linux too.

The `denied-name-literal` case connects to an address the guest never looked
up. It is refused because the resolver re-resolves a rule that names exactly
one host, and denies what that host answers with.

## The lifecycle

Sandbox `a` ran under `default: deny` with `allow: example.com`. Sandbox `b`
ran under `default: allow` with `deny: example.com`. A third ran with no
policy. Where `brig sh` would have restarted a sandbox first, the probe ran
through `nerdctl exec`.

| what was done | what happened |
| --- | --- |
| all three running | `a` reached `example.com` and was refused `9.9.9.9`. `b` reached `9.9.9.9` and was refused `example.com`. The one with no policy reached `9.9.9.9`. None of the `brig sh` calls restarted a sandbox |
| `a`'s `/etc/resolv.conf` | `nameserver 10.0.2.3`, `nameserver 10.4.1.1`. Rootless nerdctl adds slirp4netns's resolver ahead of `--dns`. DNS sent to `10.0.2.3` was answered by Brig's resolver: `NOERROR` for `example.com` and `REFUSED` for `example.net` |
| `b` to its bridge address on port 22, to the namespace's tap address `10.0.2.100`, and to `a`'s resolver | refused, refused, and timed out |
| `b`'s resolver killed | `9.9.9.9` still connected at once. After 17 seconds `9.9.9.9` and `1.1.1.2` were refused |
| the next `brig sh` on `b` | Brig restarted `b` under a new resolver. `9.9.9.9` reached, and `example.com` was refused |
| `b`'s table deleted while its resolver ran | the table was back within 7 seconds, and the resolver logged `the table brig_egress_egconf-b is gone, installing it again`. `example.com`'s address, which the guest had not looked up since, was refused. `9.9.9.9` reached |
| `b` booted again, and probed at once | `deny_host` already held `example.com`'s addresses when the guest came up, and the guest was refused `example.com`'s address without looking it up |
| `brig policy detach` on `a`, then `brig sh` | Brig restarted `a`. `9.9.9.9` reached, and no table or record was left |
| `brig stop` on `b`, then `brig rm` on all three | the table, the resolver and its records went with the stop. No network, container, table or resolver was left after the rm |

## Method

```bash
brig policy attach conformance-deny egconf -n deny
brig run -d egconf@deny ~/proj
brig sh egconf@deny /work/proj/netprobe tcp example.net:443
```

The table and the resolver, from the host:

```console
$ PID=$(cat $XDG_RUNTIME_DIR/containerd-rootless/child_pid)
$ NS="--net=/proc/$PID/root$XDG_RUNTIME_DIR/containerd-rootless/netns"
$ nsenter -U --preserve-credentials -t $PID $NS -- nft list chain inet brig_egress_egconf-deny egress
table inet brig_egress_egconf-deny {
	chain egress {
		type filter hook forward priority filter - 10; policy accept;
		iifname != "br-8ba3091b96ff" return
		meta nfproto ipv6 goto refuse
		ip saddr != 10.4.2.0/24 drop
		ct state established,related ct mark 0x90670000/16 accept
		iifname != @alive goto refuse
		meta l4proto != { icmp, tcp, udp } goto refuse
		ip daddr @deny_net goto refuse
		ip daddr @deny_ip goto refuse
		ip daddr @deny_host goto refuse
		ip daddr @allow_net goto admit
		ip daddr @allow_ip goto admit
		ip daddr @allow_host goto admit
		goto refuse
	}
}
$ head -1 ~/.brig/egress/brig-egconf-deny.log
2026/10/04 10:43:59 egress resolver on 10.4.2.1:53 for table brig_egress_egconf-deny, default deny (3 allow, 2 deny), upstream 10.0.2.3:53
```

## What this does not measure

A rootful nerdctl, an arm64 host, and rootlesskit's `pasta` network driver.
DNS over TCP from a guest: `internal/egress` tests it over loopback only. A
connection that outlives a boot of its sandbox, and a `brig stop` that
fails: `internal/egress` and `internal/runtime` test the rules and the
teardown for both. `test/conformance` itself does not run on nerdctl yet.
