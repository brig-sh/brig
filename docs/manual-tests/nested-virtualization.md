# Manual test: nested virtualization on macOS

*An engineering record for brig-sh/brig#376, identified by build and host
below. It evidences what the `capabilities: [kvm]` profile field gives a
guest, and part of the limits in
[security.md](../security.md#nested-virtualization-opt-in): shares,
credentials and lifetime for an L2 guest. The network limit is measured only
as far as L1. The L2 recipe gives L2 no network device at all, so the run shows
that an offline L1 has no route out, and does not show L2 traffic passing
through L1 under a posture or a policy.*

CI cannot run any of this. `ci.yml` runs on Linux and never boots a VM.
`script/smoke.sh` holds the half above the guest: `--nested-virt` on hull's
command line only when a profile asks, brig's refusal on `vz`, hull's refusal
on a host that cannot nest reaching the user, the restart of a running
sandbox whose nested state changed, and the refusal, before anything is
stopped, of a restart into kvm on a host that cannot nest. Everything that needs a kernel booted
with EL2 is measured here.

## Host and builds

Apple M5 Pro, macOS 26.7. Hypervisor.framework reports EL2 support
(`hv_vm_config_get_el2_supported()` returns true).

| component | build |
| --- | --- |
| brig | this branch, `make build` |
| hull | the #376 branch, `make urunc_macos`, run as `dist/hull_arm64` |
| hvi | the #376 branch, `cargo build --release`, ad-hoc signed, symlinked as `dist/hvi` beside hull |
| L1 and L2 kernel | hull's boot bundle `Image`, Linux 6.12.95, `CONFIG_KVM=y` |
| L2 monitor | Firecracker v1.17.0, aarch64 static release |

## hvi on its own

Recorded by the hvi side of the change with `hvi boot --nested-virt`, and
relayed here.

- L1 starts at EL2, and KVM starts in nVHE mode:
  `kvm [1]: Hyp nVHE mode initialized successfully`.
- `/dev/kvm` exists in L1, with 2 and with 4 vCPUs.
- Firecracker inside L1 boots an L2 guest that prints `BRIG_NESTED_OK`.
- Booted without `--nested-virt`, L1 has no `/dev/kvm`, and its kernel says
  `kvm [1]: HYP mode not available`.
- Under nVHE the L1 kernel runs at EL1. A vCPU stopped while it runs L2 shows
  L2's EL1 registers, and hvi's plugin view cannot tell them from L1's.

## Brig end to end

`script/nested-e2e.sh` runs every check below against a real hull. The L2
payload it builds:

- Firecracker v1.17.0, `firecracker-v1.17.0-aarch64.tgz`, sha256
  `e351ebe4f7a16b5873bbd51005d2e6767103cff4d5ebc829df2d3f95a93e2256`.
- L2 kernel: a copy of `$(hull assets dir)/Image`.
- L2 initramfs: the Alpine 3.20.10 aarch64 minirootfs, sha256
  `61ac877fdbcee6914731bc22a4ed5668ea3470f201f97a7078931c48b71bbeec`, with an
  `/init` that prints `BRIG_NESTED_OK` and what L2 can reach, then powers off.
- `vm.json`: the kernel and initramfs, `console=ttyS0 reboot=k panic=1
  rdinit=/init`, no drives, 1 vCPU, 256 MiB.
- Run in L1 as `firecracker --no-api --config-file vm.json --level Warn`,
  from `/tmp`. Firecracker waits for L1's entropy pool before it boots L2.

### Result

Run on 2026-09-27 on the builds above with
`BRIG_RUNTIME_BIN=<hull worktree>/dist/hull_arm64 script/nested-e2e.sh`,
every brig state directory in a scratch directory under `/private/tmp`, and
the installed brig and hull left alone. The script exited 0 and left no
sandbox, instance or `hvi` process behind.

```
== the host ==
  ok   doctor: ok  nested    supported (hull hvi backend; Hypervisor.framework reports EL2)
  ok   info prints the CAPABILITIES row
  ok   info says the host can nest
  ok   the kvm profile is handed the same credentials as the plain one
== refused on vz ==
  ok   a kvm run on vz is refused
  ok   with the backend named
  ok   and no guest home was created
  ok   and no instance exists
== L1 and L2, offline ==
  rc 0; L1 and L2 output in /private/tmp/n376.h78W7V/kvm.out
  ok   the run's envelope has the CAPABILITIES row
  ok   L1 has /dev/kvm
  ok   L1: [    0.083665] kvm [1]: Hyp nVHE mode initialized successfully
  ok   a Firecracker L2 guest booted under KVM and printed BRIG_NESTED_OK
  ok   L2 mounts no virtiofs share
  ok   L2 has no /work or /root/work
  ok   L2 reported everything it was asked
  ok   L2 has no virtio device at all
  ok   L2 has no block device
  ok   L2 has no network device but lo
  ok   the declared GH_TOKEN reached L1 (the control)
  ok   an undeclared host variable did not reach L1
  ok   nor L2
  ok   offline: L1 has no route out
== the default profile, isolated ==
  rc 0; output in /private/tmp/n376.h78W7V/plain.out
  ok   a profile without kvm has no /dev/kvm
  ok   its kernel says: [    0.070614] kvm [1]: HYP mode not available
  ok   the same egress check gets out from a networked sandbox (the control)
== a running sandbox whose profile gains kvm ==
  ok   the sandbox first runs without /dev/kvm
  rc 0; output in /private/tmp/n376.h78W7V/swap2.out
  ok   the run restarts it, and says why
  ok   the restarted guest has /dev/kvm
== stop ends L1 and the L2 inside it ==
  ok   a long-lived L2 is running in L1
  ok   and outlives the exec that started it
  ok   hull records the hvi process for the sandbox (pid 48232)
  ok   after brig stop the hvi process is gone, and the L2 it held with it
  ok   hull no longer lists it as running
PASS
```

What the less obvious checks measure:

- `N376_UNDECLARED` is set in brig's environment for the kvm run and is not in
  the profile. `GH_TOKEN` is set to a dummy value and is in the profile, which
  is the control that forwarding works at all.
- The offline kvm run and the isolated plain run make the same connection
  attempt to `1.1.1.1:443` from L1. Only the isolated one gets out.
- "L2 has no block device" leaves out loop and ram devices, which the L2
  kernel creates on its own with nothing behind them. A disk handed to L2
  would be a virtio device, and L2 has none.
- The stop check reads the hvi pid from `hull inspect` while an L2 guest is
  running inside L1, then checks that the pid is gone after `brig stop`.
