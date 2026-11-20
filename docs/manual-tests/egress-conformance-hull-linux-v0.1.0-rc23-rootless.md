# Egress conformance: hull-linux, v0.1.0-rc23, rootless

Written by `script/egress-conformance.sh`. To change it, run the suite again.

| | |
| --- | --- |
| backend | `hull-linux` |
| runtime | `hull v0.2.0 (d0eab08, 2026-11-19, go1.26.8, linux/amd64)` |
| brig | `brig v0.5.3-0.20261120102502-41b422bef887 (41b422b, 2026-11-20, go1.26.4, linux/amd64)` |
| date | 2026-11-20 11:55 UTC |
| image | `ghcr.io/brig-sh/claude-code-stock:root` |
| image digest | `sha256:f6bfd5213245c86ebb4165a4267b45d6b55727e11ae44cb3db311a3397d39b60` |
| runtime bundle | `v0.1.0-rc23, generic-boot, rootless` |
| hull | `brig-sh/hull@d0eab08061f56c0edd5f060109bbee35e19abe64` |
| urunc | `urunc-dev/urunc@5c6bc7060d4f6d33b0d4e8fd388f9f04cc88510b` |
| hvi | `0.1.0-rc.3, commit bde7c58edf3352e112c7c349448090623390aaec` |
| hull-assets | `0.1.14, from nofireai/hull-assets` |
| containerd | `v2.3.5` |
| rootlesskit | `v3.2.0` |
| slirp4netns | `v1.3.5` |
| boot assets | `a local directory, from BRIG_BOOT_ASSETS. The runtime bundle's own copy is built from ghcr.io/nofireai/hull-assets:0.1.14-linux-amd64@sha256:e466779c4fd0daccc772db810bd2dcf81afc0a2c94aaee04cae6b3aafb94589c` |
| probes ran under | `brig sh` |
| result | **PASS**: 18 pass, 4 known gap, 1 no route, 1 unproven |

## Cases

A denied case passes only on the failure its policy causes. The column
with no policy is the same command on a guest with no policy. A denied
case is unproven when that guest did not get to the target either, so
the run says nothing about whether the policy blocks it. Only a case
that says why its target may be shut can be unproven. Any other fails.

| case | what the guest tried | expected | with the policy | with no policy | verdict |
| --- | --- | --- | --- | --- | --- |
| `allowed-name` | a name the policy allows | reached | reached | reached | pass |
| `allowed-glob` | a name under an allowed glob, beside a denied one | reached | reached | reached | pass |
| `allowed-cidr` | an address in an allowed range, beside a denied one | reached | reached | reached | pass |
| `allowed-resolver` | a resolver in an allowed range, over UDP | reached | reached | reached | pass |
| `denied-name` | a name no rule allows | resolve-refused | resolve-refused | reached | pass |
| `ipv4-literal` | an IPv4 literal no cidr rule matches | connect-refused | connect-refused | reached | pass |
| `ipv6-literal` | an IPv6 literal no cidr rule matches | no route | no-route | no-route | no route: no-route with the policy and no-route without one |
| `alternate-resolver` | a resolver other than the gateway, over UDP | timeout | timeout | reached | pass |
| `dns-over-https` | DNS over HTTPS | connect-refused | connect-refused | reached | pass |
| `dns-over-tls` | DNS over TLS | connect-refused | connect-refused | reached | pass |
| `deny-over-allow-host` | a deny host rule inside an allowed glob | resolve-refused | resolve-refused | reached | pass |
| `deny-over-allow-cidr` | a deny cidr rule inside an allowed range | connect-refused | connect-refused | reached | pass |
| `proxy-env` | curl with https_proxy set in the guest, pointed at the host | connect-refused | connect-refused | error-after-connect | pass |
| `host-service` | a service bound on the host's outbound address | connect-refused | connect-refused | reached | pass |
| `metadata` | 169.254.169.254 | connect-refused | connect-refused | connect-refused | unproven: connect-refused with the policy and connect-refused without one. hull's gateway resets every connection to 169.254.0.0/16, with a policy or without one |
| `allow/allowed-name` | a name no rule denies | reached | reached | reached | pass |
| `allow/glob-literal` | the literal of a host a deny glob covers, never looked up | known gap | reached | reached | known gap: default: allow leaves this open |
| `allow/denied-glob` | a name a deny glob covers | connect-refused | connect-refused | reached | pass |
| `allow/denied-name` | a name a deny host rule names | connect-refused | connect-refused | reached | pass |
| `allow/denied-name-literal` | the literal of a host a deny host rule names | connect-refused | connect-refused | reached | pass |
| `allow/denied-cidr` | an address a deny cidr rule covers | connect-refused | connect-refused | reached | pass |
| `allow/alternate-resolver` | a glob-denied name from a resolver other than the gateway | known gap | reached | reached | known gap: default: allow leaves this open |
| `allow/dns-over-https` | a glob-denied name over DNS over HTTPS | known gap | reached | reached | known gap: default: allow leaves this open |
| `allow/dns-over-tls` | a glob-denied name over DNS over TLS | known gap | reached | reached | known gap: default: allow leaves this open |

## Boot: no policy

- allowed controls reached their targets first and last: yes
- connections the host listener accepted: 2

```console
$ netprobe tcp example.com:443
reached tcp example.com:443 connected to 172.66.147.243:443
$ netprobe tcp deb.debian.org:443
reached tcp deb.debian.org:443 connected to 151.101.194.132:443
$ netprobe tcp 1.1.1.2:443
reached tcp 1.1.1.2:443 connected to 1.1.1.2:443
$ netprobe dns -timeout 15s 1.1.1.2 example.com
reached dns 1.1.1.2 1.1.1.2:53 answered NOERROR with 2 answers
$ netprobe tcp example.net:443
reached tcp example.net:443 connected to 104.20.21.8:443
$ netprobe tcp 9.9.9.9:443
reached tcp 9.9.9.9:443 connected to 9.9.9.9:443
$ netprobe tcp [2606:4700:4700::1111]:443
no-route tcp [2606:4700:4700::1111]:443 dial tcp [2606:4700:4700::1111]:443: connect: network is unreachable
$ netprobe dns -timeout 15s 8.8.8.8 example.net
reached dns 8.8.8.8 8.8.8.8:53 answered NOERROR with 2 answers
$ netprobe doh https://8.8.8.8/dns-query example.net
reached doh https://8.8.8.8/dns-query 8.8.8.8:443 answered NOERROR with 2 answers over HTTPS
$ netprobe dot 8.8.8.8 example.net
reached dot 8.8.8.8 8.8.8.8:853 answered NOERROR with 2 answers over TLS
$ netprobe tcp www.debian.org:443
reached tcp www.debian.org:443 connected to 130.89.148.77:443
$ netprobe tcp 1.1.1.1:443
reached tcp 1.1.1.1:443 connected to 1.1.1.1:443
$ https_proxy=http://{hostsvc} HTTPS_PROXY=http://{hostsvc} netprobe doh -client curl https://dns.google/dns-query example.com
error-after-connect doh https://dns.google/dns-query curl exit 56: curl: (56) Proxy CONNECT aborted
$ netprobe tcp {hostsvc}
reached tcp {hostsvc} connected to {hostsvc}
$ netprobe tcp 169.254.169.254:80
connect-refused tcp 169.254.169.254:80 dial tcp 169.254.169.254:80: connect: connection refused
$ netprobe tcp example.com:443
reached tcp example.com:443 connected to 172.66.147.243:443
$ netprobe tcp {addr:git.kernel.org}:443
reached tcp 172.105.64.184:443 connected to 172.105.64.184:443
$ netprobe tcp git.kernel.org:443
reached tcp git.kernel.org:443 connected to 172.105.64.184:443
$ netprobe tcp example.net:443
reached tcp example.net:443 connected to 172.66.175.59:443
$ netprobe tcp {addr:example.net}:443
reached tcp 104.20.21.8:443 connected to 104.20.21.8:443
$ netprobe tcp 9.9.9.9:443
reached tcp 9.9.9.9:443 connected to 9.9.9.9:443
$ netprobe dns -timeout 15s 8.8.8.8 git.kernel.org
reached dns 8.8.8.8 8.8.8.8:53 answered NOERROR with 3 answers
$ netprobe doh https://8.8.8.8/dns-query git.kernel.org
reached doh https://8.8.8.8/dns-query 8.8.8.8:443 answered NOERROR with 3 answers over HTTPS
$ netprobe dot 8.8.8.8 git.kernel.org
reached dot 8.8.8.8 8.8.8.8:853 answered NOERROR with 3 answers over TLS
```

## Boot: conformance-deny

```yaml
apiVersion: brig.sh/v1alpha1
name: conformance-deny
desc: network conformance suite, deny default
egress:
  default: deny
  allow:
    - host: example.com
    - host: "*.debian.org"
    - cidr: 1.1.1.0/24
  deny:
    - host: www.debian.org
    - cidr: 1.1.1.1/32
```

- allowed controls reached their targets first and last: yes
- connections the host listener accepted: 0

```console
$ netprobe tcp example.com:443
reached tcp example.com:443 connected to 104.20.23.154:443
$ netprobe tcp deb.debian.org:443
reached tcp deb.debian.org:443 connected to 151.101.194.132:443
$ netprobe tcp 1.1.1.2:443
reached tcp 1.1.1.2:443 connected to 1.1.1.2:443
$ netprobe dns -timeout 15s 1.1.1.2 example.com
reached dns 1.1.1.2 1.1.1.2:53 answered NOERROR with 2 answers
$ netprobe tcp example.net:443
resolve-refused tcp example.net:443 lookup example.net on 198.18.1.1:53: server misbehaving; asked again, 198.18.1.1:53 answered REFUSED with 0 answers
$ netprobe tcp 9.9.9.9:443
connect-refused tcp 9.9.9.9:443 dial tcp 9.9.9.9:443: connect: connection refused
$ netprobe tcp [2606:4700:4700::1111]:443
no-route tcp [2606:4700:4700::1111]:443 dial tcp [2606:4700:4700::1111]:443: connect: network is unreachable
$ netprobe dns -timeout 15s 8.8.8.8 example.net
timeout dns 8.8.8.8 read udp 198.18.1.2:47595->8.8.8.8:53: i/o timeout
$ netprobe doh https://8.8.8.8/dns-query example.net
connect-refused doh https://8.8.8.8/dns-query dial tcp 8.8.8.8:443: connect: connection refused
$ netprobe dot 8.8.8.8 example.net
connect-refused dot 8.8.8.8 dial tcp 8.8.8.8:853: connect: connection refused
$ netprobe tcp www.debian.org:443
resolve-refused tcp www.debian.org:443 lookup www.debian.org on 198.18.1.1:53: server misbehaving; asked again, 198.18.1.1:53 answered REFUSED with 0 answers
$ netprobe tcp 1.1.1.1:443
connect-refused tcp 1.1.1.1:443 dial tcp 1.1.1.1:443: connect: connection refused
$ https_proxy=http://{hostsvc} HTTPS_PROXY=http://{hostsvc} netprobe doh -client curl https://dns.google/dns-query example.com
connect-refused doh https://dns.google/dns-query curl exit 7: curl: (7) Failed to connect to {host} port 46029 after 0 ms: Couldn't connect to server
$ netprobe tcp {hostsvc}
connect-refused tcp {hostsvc} dial tcp {hostsvc}: connect: connection refused
$ netprobe tcp 169.254.169.254:80
connect-refused tcp 169.254.169.254:80 dial tcp 169.254.169.254:80: connect: connection refused
```

## Boot: conformance-allow

```yaml
apiVersion: brig.sh/v1alpha1
name: conformance-allow
desc: network conformance suite, allow default
egress:
  default: allow
  deny:
    - host: example.net
    - host: "*.kernel.org"
    - cidr: 9.9.9.9/32
```

- allowed controls reached their targets first and last: yes
- connections the host listener accepted: 0

```console
$ netprobe tcp example.com:443
reached tcp example.com:443 connected to 104.20.23.154:443
$ netprobe tcp {addr:git.kernel.org}:443
reached tcp 172.105.64.184:443 connected to 172.105.64.184:443
$ netprobe tcp git.kernel.org:443
connect-refused tcp git.kernel.org:443 dial tcp 172.105.64.184:443: connect: connection refused
$ netprobe tcp example.net:443
connect-refused tcp example.net:443 dial tcp 104.20.21.8:443: connect: connection refused
$ netprobe tcp {addr:example.net}:443
connect-refused tcp 104.20.21.8:443 dial tcp 104.20.21.8:443: connect: connection refused
$ netprobe tcp 9.9.9.9:443
connect-refused tcp 9.9.9.9:443 dial tcp 9.9.9.9:443: connect: connection refused
$ netprobe dns -timeout 15s 8.8.8.8 git.kernel.org
reached dns 8.8.8.8 8.8.8.8:53 answered NOERROR with 3 answers
$ netprobe doh https://8.8.8.8/dns-query git.kernel.org
reached doh https://8.8.8.8/dns-query 8.8.8.8:443 answered NOERROR with 3 answers over HTTPS
$ netprobe dot 8.8.8.8 git.kernel.org
reached dot 8.8.8.8 8.8.8.8:853 answered NOERROR with 3 answers over TLS
```

