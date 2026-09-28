#!/bin/bash
# End-to-end check of nested virtualization on a real Mac (brig-sh/brig#376).
#
# It boots real sandboxes through the real hull and hvi, so it needs Apple
# silicon with EL2, a hull that has `hull capabilities` and `run --nested-virt`,
# and an hvi that boots a guest with EL2. It is not part of CI: CI runs on Linux
# and never boots a VM. script/smoke.sh holds everything above the guest.
#
#   make build
#   BRIG_RUNTIME_BIN=/path/to/hull script/nested-e2e.sh
#
# What it checks, positives and the negatives that carry the security claims:
#   - doctor and info report the host as able to nest, and the envelope has
#     the CAPABILITIES row
#   - a kvm profile on vz is refused, and nothing is booted or created
#   - inside a kvm sandbox (L1): /dev/kvm exists and KVM initialized; a
#     Firecracker L2 guest boots and prints BRIG_NESTED_OK
#   - L2 has no shares, no block device and no network device
#   - an undeclared host variable reaches neither L1 nor L2, while a declared
#     one reaches L1 (the control)
#   - with --offline, L1 has no route out, while the same check from an
#     isolated sandbox gets out (the control)
#   - the default profile has no /dev/kvm, and its kernel says so
#   - a sandbox running without kvm, whose profile then gains it, is restarted
#     with a warning, and the restarted guest has /dev/kvm
#   - brig stop ends the hvi process with a running L2 inside it
#
# Side by side with an installed brig: every brig state directory is a fresh
# scratch directory, the sandboxes are named n376-kvm, n376-plain and n376-swap,
# and only those are removed at the end. It never runs `brig rm --all`. hull's own
# store (~/.hull/store) is shared, as it is for every hull on this machine.
#
# Needs: bash, curl, shasum, tar, python3 (standard library only). Downloads,
# once, into ${BRIG_NESTED_CACHE:-~/.cache/brig-nested}: the Firecracker
# v1.17.0 aarch64 release and the Alpine 3.20.10 aarch64 minirootfs, each
# checked against a pinned sha256.
#
# Launch it from your own terminal session. hvi is ad-hoc signed with the
# hypervisor entitlement, and macOS can refuse such a binary started from a
# headless job.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2
REPO="$(pwd)"
exec < /dev/null

FC_VERSION=v1.17.0
FC_URL="https://github.com/firecracker-microvm/firecracker/releases/download/$FC_VERSION/firecracker-$FC_VERSION-aarch64.tgz"
FC_SHA256=e351ebe4f7a16b5873bbd51005d2e6767103cff4d5ebc829df2d3f95a93e2256
FC_MEMBER="release-$FC_VERSION-aarch64/firecracker-$FC_VERSION-aarch64"
ALPINE_VERSION=3.20.10
ALPINE_URL="https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/aarch64/alpine-minirootfs-$ALPINE_VERSION-aarch64.tar.gz"
ALPINE_SHA256=61ac877fdbcee6914731bc22a4ed5668ea3470f201f97a7078931c48b71bbeec

fail=0
ok()  { printf '  ok   %s\n' "$1"; }
bad() { printf '  FAIL %s\n' "$1"; fail=1; }
die() { printf 'nested-e2e: %s\n' "$1" >&2; exit 2; }

[ "$(uname -s)" = Darwin ] && [ "$(uname -m)" = arm64 ] || die "this needs an Apple silicon Mac"
for tool in curl shasum tar python3; do
  command -v "$tool" > /dev/null 2>&1 || die "$tool is not on PATH"
done

BRIG="${BRIG:-$REPO/brig}"
[ -x "$BRIG" ] || die "$BRIG is not built; run make build, or point BRIG at a binary"
if [ -z "${BRIG_RUNTIME_BIN:-}" ]; then
  BRIG_RUNTIME_BIN="$(command -v hull)" || die "no hull on PATH; set BRIG_RUNTIME_BIN"
  printf 'nested-e2e: BRIG_RUNTIME_BIN is unset, using %s\n' "$BRIG_RUNTIME_BIN"
fi
export BRIG_RUNTIME_BIN
HULL="$BRIG_RUNTIME_BIN"

# Scratch for every piece of brig state, so the installed brig's sessions,
# networks and profiles are never read or written. /private/tmp keeps the
# gateway socket paths under the 103 bytes a unix socket allows.
E2E="$(mktemp -d /private/tmp/n376.XXXXXX)"
export BRIG_STATE_DIR="$E2E/state"
export BRIG_PROFILE_DIR="$E2E/profiles"
export BRIG_GATEWAY_DIR="$E2E/gw"
export XDG_RUNTIME_DIR="$E2E/run"
mkdir -p "$XDG_RUNTIME_DIR"
# A setting inherited from the caller's shell would change what is measured.
unset BRIG_HYPERVISOR BRIG_NETWORK BRIG_WORKSPACE BRIG_RUNTIME BRIG_BOOT_ASSETS
unset BRIG_ENV_ARGV BRIG_ALLOW_DENIED BRIG_FORWARD_ENV

cleanup() {
  for ref in n376-kvm n376-plain n376-swap; do
    "$BRIG" rm "$ref" > /dev/null 2>&1
  done
  if [ -n "${KEEP_E2E:-}" ]; then
    printf 'nested-e2e: kept %s\n' "$E2E"
  else
    rm -rf "$E2E"
  fi
}
trap cleanup EXIT

# --- downloads, pinned ---
CACHE="${BRIG_NESTED_CACHE:-$HOME/.cache/brig-nested}"
mkdir -p "$CACHE"
fetch() {
  local url="$1" sum="$2" out
  out="$CACHE/$(basename "$1")"
  if [ ! -f "$out" ]; then
    curl -fsSL --max-time 300 -o "$out.part" "$url" || die "could not download $url"
    mv "$out.part" "$out"
  fi
  # Checked every time, cached or not: the cache is a directory anything
  # running as you can write.
  printf '%s  %s\n' "$sum" "$out" | shasum -a 256 -c - > /dev/null 2>&1 \
    || die "$out does not match the pinned sha256 $sum; delete it and run again"
  printf '%s' "$out"
}
FC_TGZ="$(fetch "$FC_URL" "$FC_SHA256")" || exit 2
ALPINE_TGZ="$(fetch "$ALPINE_URL" "$ALPINE_SHA256")" || exit 2

# --- the L2 payload, shared into L1 as the run's project at /work/l2 ---
L2="$E2E/l2"
mkdir -p "$L2"
tar -xzf "$FC_TGZ" -C "$E2E" "$FC_MEMBER" || die "no $FC_MEMBER in $FC_TGZ"
cp "$E2E/$FC_MEMBER" "$L2/firecracker" && chmod 755 "$L2/firecracker"
ASSETS="$("$HULL" assets dir 2>/dev/null)" || die "$HULL assets dir failed"
[ -s "$ASSETS/Image" ] || die "no kernel at $ASSETS/Image; run any hvi profile once, or hull assets pull"
cp "$ASSETS/Image" "$L2/Image"

# L2's /init. It proves it ran, then reports what it can reach: no share, no
# block device, no network device, no variable from the host. Every marker
# prints a word even when the answer is empty, so a line that never arrived
# cannot pass for "nothing there".
#
# poweroff -f stops the guest at once and does not drain the serial console,
# so lines still queued for the UART are lost; the first run of this script
# saw BRIG_NESTED_OK and none of the lines after it. The sleep lets them out.
cat > "$E2E/init-ok" <<'INIT'
#!/bin/sh
/bin/busybox mount -t proc proc /proc
/bin/busybox mount -t sysfs sysfs /sys
echo BRIG_NESTED_OK
echo "L2_VIRTIOFS:$(/bin/busybox grep -c virtiofs /proc/mounts)"
if [ -e /work ] || [ -e /root/work ]; then echo L2_WORK:present; else echo L2_WORK:absent; fi
# loop and ram devices are the kernel's own, present with nothing behind them.
# A disk L1 handed L2 would be a virtio device, listed on the next line too.
blk="$(/bin/busybox ls /sys/block | /bin/busybox grep -v -E '^(loop|ram)' | /bin/busybox tr '\n' ' ')"
echo "L2_BLOCK:${blk:-none}"
virt="$(/bin/busybox ls /sys/bus/virtio/devices 2>/dev/null | /bin/busybox tr '\n' ' ')"
echo "L2_VIRTIO:${virt:-none}"
echo "L2_NET:$(/bin/busybox ls /sys/class/net | /bin/busybox tr '\n' ' ')"
echo "L2_UNDECLARED:$(/bin/busybox env | /bin/busybox grep -c N376_UNDECLARED)"
echo L2_DONE
/bin/busybox sleep 2
/bin/busybox poweroff -f
INIT
# The long-lived L2 for the stop check: it says it is up and stays up.
cat > "$E2E/init-sleep" <<'INIT'
#!/bin/sh
/bin/busybox mount -t proc proc /proc
echo BRIG_L2_SLEEPING
/bin/busybox sleep 600
/bin/busybox poweroff -f
INIT

# An Alpine minirootfs written out as a newc cpio with our /init, in memory,
# with no root and no cpio(1): macOS cannot create the device node the kernel
# wants at /dev/console, and a cpio carries it as metadata anyway. The same
# approach as hvi-vmm's tools/mk-initramfs.py.
mkcpio() {
  python3 - "$ALPINE_TGZ" "$1" "$2" <<'PY'
import sys, tarfile
src, init_path, out = sys.argv[1], sys.argv[2], sys.argv[3]
buf, ino = bytearray(), [0]
def add(name, mode, data=b"", rmaj=0, rmin=0):
    ino[0] += 1
    nb = name.encode() + b"\0"
    f = lambda v: b"%08X" % (v & 0xFFFFFFFF)
    hdr = (b"070701" + f(ino[0]) + f(mode) + f(0) + f(0) + f(1) + f(0) + f(len(data))
           + f(0) + f(0) + f(rmaj) + f(rmin) + f(len(nb)) + f(0))
    buf.extend(hdr + nb); buf.extend(b"\0" * ((4 - (len(hdr) + len(nb)) % 4) % 4))
    buf.extend(data); buf.extend(b"\0" * ((4 - len(data) % 4) % 4))
seen = set()
with tarfile.open(src, "r:gz") as t:
    for m in t.getmembers():
        name = m.name.lstrip("./")
        if not name or name == "init" or name in seen:
            continue
        seen.add(name)
        perm = m.mode & 0o7777
        if m.isdir():
            add(name, 0o040000 | perm)
        elif m.issym():
            add(name, 0o120000 | 0o777, m.linkname.encode())
        elif m.isfile() or m.islnk():
            add(name, 0o100000 | perm, t.extractfile(m).read())
for d in ("dev", "proc", "sys"):
    if d not in seen:
        add(d, 0o040000 | 0o755)
add("dev/console", 0o020000 | 0o600, rmaj=5, rmin=1)
add("init", 0o100000 | 0o755, open(init_path, "rb").read())
add("TRAILER!!!", 0)
open(out, "wb").write(bytes(buf))
PY
}
mkcpio "$E2E/init-ok" "$L2/l2.cpio" || die "could not build the L2 initramfs"
mkcpio "$E2E/init-sleep" "$L2/l2-sleep.cpio" || die "could not build the sleeping L2 initramfs"

vmjson() {
  printf '{"boot-source":{"kernel_image_path":"/work/l2/Image","initrd_path":"/work/l2/%s",' "$1"
  printf '"boot_args":"console=ttyS0 reboot=k panic=1 rdinit=/init"},"drives":[],'
  printf '"machine-config":{"vcpu_count":1,"mem_size_mib":256}}\n'
}
vmjson l2.cpio > "$L2/vm.json"
vmjson l2-sleep.cpio > "$L2/vm-sleep.json"

# What runs in L1: KVM's own evidence, the L1-side negatives, then L2.
# Firecracker is started from /tmp because it wants a writable working
# directory, and waits for L1's entropy pool, which can take tens of seconds.
#
# brig runs this on a tty. Plain `timeout` moves its command into a background
# process group, and Firecracker putting that tty into raw mode then stops it
# with SIGTTOU, timeout included, so the run hangs with both in state T.
# --foreground keeps it in the tty's group, and stdin from /dev/null means
# Firecracker has no terminal to change.
cat > "$L2/run-l2.sh" <<'L1'
#!/bin/bash
if [ -c /dev/kvm ]; then echo L1_KVM:present; else echo L1_KVM:absent; fi
dmesg | grep -E 'kvm \[1\]: .*(initialized successfully|HYP mode not available)' | sed 's/^/L1_DMESG:/'
echo "L1_UNDECLARED:$( (env; tr '\0' '\n' < /proc/1/environ) | grep -c N376_UNDECLARED)"
echo "L1_DECLARED:${GH_TOKEN:+set}"
if timeout 5 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' 2>/dev/null; then
  echo L1_EGRESS:open
else
  echo L1_EGRESS:closed
fi
[ "${1:-}" = l1-only ] && exit 0
cd /tmp && timeout --foreground 240 /work/l2/firecracker --no-api --config-file /work/l2/vm.json \
  --level Warn < /dev/null
echo "L2_EXIT:$?"
L1
# The long-lived L2, detached from the exec that starts it so it outlives it.
cat > "$L2/start-l2-bg.sh" <<'L1'
#!/bin/bash
cd /tmp && setsid nohup /work/l2/firecracker --no-api --config-file /work/l2/vm-sleep.json \
  --level Warn > /tmp/l2-bg.log 2>&1 < /dev/null &
for _ in $(seq 1 120); do
  grep -q BRIG_L2_SLEEPING /tmp/l2-bg.log 2>/dev/null && break
  sleep 1
done
grep -q BRIG_L2_SLEEPING /tmp/l2-bg.log && echo L2_BG:up || echo L2_BG:down
echo "L2_BG_PROCS:$(pgrep -c -x firecracker)"
L1
chmod 755 "$L2/run-l2.sh" "$L2/start-l2-bg.sh"

# --- the two profiles: the documented example, and the same without kvm ---
mkdir -p "$BRIG_PROFILE_DIR"
sed 's/^name: .*/name: n376-kvm/' "$REPO/docs/manual-tests/ubuntu-kvm.yaml" > "$E2E/n376-kvm.yaml"
sed 's/^name: .*/name: n376-plain/; /^capabilities:/d' "$REPO/docs/manual-tests/ubuntu-kvm.yaml" \
  > "$E2E/n376-plain.yaml"
"$BRIG" agent import "$E2E/n376-kvm.yaml" > /dev/null 2>&1 || die "could not import n376-kvm"
"$BRIG" agent import "$E2E/n376-plain.yaml" > /dev/null 2>&1 || die "could not import n376-plain"

marker() { tr -d '\r' < "$2" | sed -n "s/^$1://p" | head -1; }
hvi_pid() {
  "$HULL" inspect "$1" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("pid") or "")' 2>/dev/null
}

echo "== the host =="
"$BRIG" doctor > "$E2E/doctor.out" 2>&1
if grep -q '^  ok  nested    supported' "$E2E/doctor.out"; then
  ok "doctor: $(grep '  nested ' "$E2E/doctor.out" | sed 's/^ *//')"
else
  bad "doctor does not report this host as able to nest: $(grep '  nested ' "$E2E/doctor.out")"
  die "nothing below can pass on this host and hull; stopping"
fi
"$BRIG" info n376-kvm > "$E2E/info.out" 2>&1
grep -q '^CAPABILITIES  kvm (nested virtualization' "$E2E/info.out" \
  && ok "info prints the CAPABILITIES row" || bad "info prints the CAPABILITIES row: $(cat "$E2E/info.out")"
grep -q 'nested virtualization: supported (backend hvi)' "$E2E/info.out" \
  && ok "info says the host can nest" || bad "info says the host can nest: $(grep nested "$E2E/info.out")"
"$BRIG" info n376-plain > "$E2E/info-plain.out" 2>&1
# The values, with the column alignment squeezed out: the CAPABILITIES label is
# the longest, so the kvm envelope pads every row one space wider.
creds() { grep '^CREDENTIALS' "$1" | tr -s ' '; }
if [ "$(creds "$E2E/info.out")" = "$(creds "$E2E/info-plain.out")" ]; then
  ok "the kvm profile is handed the same credentials as the plain one"
else
  bad "the credential rows differ: $(grep '^CREDENTIALS' "$E2E/info.out" "$E2E/info-plain.out")"
fi

echo "== refused on vz =="
env BRIG_HYPERVISOR=vz "$BRIG" run n376-kvm --home "$E2E/home-vz" -d > "$E2E/vz.out" 2>&1
[ $? != 0 ] && ok "a kvm run on vz is refused" || bad "a kvm run on vz started: $(cat "$E2E/vz.out")"
grep -qF 'nested virtualization (capability kvm) needs the hvi backend (BRIG_HYPERVISOR is "vz")' "$E2E/vz.out" \
  && ok "with the backend named" || bad "the vz refusal: $(cat "$E2E/vz.out")"
[ -e "$E2E/home-vz" ] && bad "the refused run created its guest home" || ok "and no guest home was created"
"$HULL" inspect brig-n376-kvm > /dev/null 2>&1 \
  && bad "the refused run left a hull instance behind" || ok "and no instance exists"

echo "== L1 and L2, offline =="
env GH_TOKEN=n376-declared N376_UNDECLARED=n376-planted \
  "$BRIG" --verbose run n376-kvm --home "$E2E/home-kvm" --offline "$L2" -- /work/l2/run-l2.sh \
  > "$E2E/kvm.out" 2>&1
printf '  rc %s; L1 and L2 output in %s\n' "$?" "$E2E/kvm.out"
grep -q '^CAPABILITIES  kvm (nested virtualization' "$E2E/kvm.out" \
  && ok "the run's envelope has the CAPABILITIES row" || bad "the run's envelope has no CAPABILITIES row"
[ "$(marker L1_KVM "$E2E/kvm.out")" = present ] && ok "L1 has /dev/kvm" || bad "L1 has no /dev/kvm"
grep -q '^L1_DMESG:.*initialized successfully' "$E2E/kvm.out" \
  && ok "L1: $(marker L1_DMESG "$E2E/kvm.out")" || bad "KVM did not initialize in L1: $(grep L1_DMESG "$E2E/kvm.out")"
grep -q '^BRIG_NESTED_OK' "$E2E/kvm.out" \
  && ok "a Firecracker L2 guest booted under KVM and printed BRIG_NESTED_OK" \
  || bad "no BRIG_NESTED_OK from L2 (L2_EXIT $(marker L2_EXIT "$E2E/kvm.out"))"
[ "$(marker L2_VIRTIOFS "$E2E/kvm.out")" = 0 ] && ok "L2 mounts no virtiofs share" \
  || bad "L2 virtiofs mounts: $(marker L2_VIRTIOFS "$E2E/kvm.out")"
[ "$(marker L2_WORK "$E2E/kvm.out")" = absent ] && ok "L2 has no /work or /root/work" \
  || bad "L2 can see a workspace path"
grep -q '^L2_DONE' "$E2E/kvm.out" && ok "L2 reported everything it was asked" \
  || bad "L2 did not finish its report, so the L2 checks below read nothing"
[ "$(marker L2_VIRTIO "$E2E/kvm.out")" = none ] && ok "L2 has no virtio device at all" \
  || bad "L2 virtio devices: $(marker L2_VIRTIO "$E2E/kvm.out")"
[ "$(marker L2_BLOCK "$E2E/kvm.out")" = none ] && ok "L2 has no block device" \
  || bad "L2 block devices: $(marker L2_BLOCK "$E2E/kvm.out")"
[ "$(marker L2_NET "$E2E/kvm.out" | tr -d ' ')" = lo ] && ok "L2 has no network device but lo" \
  || bad "L2 network devices: $(marker L2_NET "$E2E/kvm.out")"
[ "$(marker L1_DECLARED "$E2E/kvm.out")" = set ] && ok "the declared GH_TOKEN reached L1 (the control)" \
  || bad "the declared GH_TOKEN did not reach L1, so the next check measures nothing"
[ "$(marker L1_UNDECLARED "$E2E/kvm.out")" = 0 ] && ok "an undeclared host variable did not reach L1" \
  || bad "an undeclared host variable reached L1"
[ "$(marker L2_UNDECLARED "$E2E/kvm.out")" = 0 ] && ok "nor L2" \
  || bad "an undeclared host variable reached L2"
[ "$(marker L1_EGRESS "$E2E/kvm.out")" = closed ] && ok "offline: L1 has no route out" \
  || bad "offline: L1 reached 1.1.1.1:443"

echo "== the default profile, isolated =="
"$BRIG" run n376-plain --home "$E2E/home-plain" --network isolated "$L2" -- /work/l2/run-l2.sh l1-only \
  > "$E2E/plain.out" 2>&1
printf '  rc %s; output in %s\n' "$?" "$E2E/plain.out"
[ "$(marker L1_KVM "$E2E/plain.out")" = absent ] && ok "a profile without kvm has no /dev/kvm" \
  || bad "a profile without kvm has /dev/kvm"
grep -q '^L1_DMESG:.*HYP mode not available' "$E2E/plain.out" \
  && ok "its kernel says: $(marker L1_DMESG "$E2E/plain.out")" \
  || bad "its kernel does not say HYP mode is unavailable: $(grep L1_DMESG "$E2E/plain.out")"
[ "$(marker L1_EGRESS "$E2E/plain.out")" = open ] \
  && ok "the same egress check gets out from a networked sandbox (the control)" \
  || bad "the egress check fails even with a network, so the offline result measures nothing"
"$BRIG" rm n376-plain > /dev/null 2>&1

echo "== a running sandbox whose profile gains kvm =="
# The same name, first without the capability and then with it. The second run
# joins a sandbox hull booted without --nested-virt, so it has to restart it,
# say so, and boot the new guest nested.
sed 's/^name: .*/name: n376-swap/' "$E2E/n376-plain.yaml" > "$E2E/n376-swap.yaml"
"$BRIG" agent import "$E2E/n376-swap.yaml" > /dev/null 2>&1 || die "could not import n376-swap"
"$BRIG" run n376-swap --home "$E2E/home-swap" --offline "$L2" -- /work/l2/run-l2.sh l1-only \
  > "$E2E/swap1.out" 2>&1
[ "$(marker L1_KVM "$E2E/swap1.out")" = absent ] && ok "the sandbox first runs without /dev/kvm" \
  || bad "the first run of n376-swap: $(tr -d '\r' < "$E2E/swap1.out" | tail -5)"
sed 's/^name: .*/name: n376-swap/' "$E2E/n376-kvm.yaml" > "$E2E/n376-swap.yaml"
"$BRIG" agent import "$E2E/n376-swap.yaml" > /dev/null 2>&1 || die "could not re-import n376-swap"
"$BRIG" run n376-swap --home "$E2E/home-swap" --offline "$L2" -- /work/l2/run-l2.sh l1-only \
  > "$E2E/swap2.out" 2>&1
printf '  rc %s; output in %s\n' "$?" "$E2E/swap2.out"
grep -q 'started without nested virtualization and n376-swap now asks for it' "$E2E/swap2.out" \
  && ok "the run restarts it, and says why" \
  || bad "no restart warning: $(tr -d '\r' < "$E2E/swap2.out" | grep -i -m3 'nested\|brig:')"
[ "$(marker L1_KVM "$E2E/swap2.out")" = present ] && ok "the restarted guest has /dev/kvm" \
  || bad "the restarted guest has no /dev/kvm"
"$BRIG" rm n376-swap > /dev/null 2>&1

echo "== stop ends L1 and the L2 inside it =="
"$BRIG" sh n376-kvm /work/l2/start-l2-bg.sh > "$E2E/bg.out" 2>&1
[ "$(marker L2_BG "$E2E/bg.out")" = up ] && ok "a long-lived L2 is running in L1" \
  || bad "the long-lived L2 did not come up: $(cat "$E2E/bg.out")"
"$BRIG" sh n376-kvm pgrep -c -x firecracker > "$E2E/bg2.out" 2>&1
[ "$(tr -d '\r' < "$E2E/bg2.out" | tail -1)" -ge 1 ] 2>/dev/null \
  && ok "and outlives the exec that started it" || bad "the L2 did not outlive its exec: $(cat "$E2E/bg2.out")"
pid="$(hvi_pid brig-n376-kvm)"
if [ -n "$pid" ] && ps -o command= -p "$pid" | grep -q hvi; then
  ok "hull records the hvi process for the sandbox (pid $pid)"
else
  bad "no hvi process on record for brig-n376-kvm (pid '${pid}')"
fi
"$BRIG" stop n376-kvm > /dev/null 2>&1
if [ -n "$pid" ] && kill -0 "$pid" 2> /dev/null; then
  bad "hvi pid $pid is still alive after brig stop"
else
  ok "after brig stop the hvi process is gone, and the L2 it held with it"
fi
"$HULL" ps 2>/dev/null | grep -q '^brig-n376-kvm *running' \
  && bad "hull still lists brig-n376-kvm as running" || ok "hull no longer lists it as running"

[ "$fail" = 0 ] && echo PASS || echo FAILURES
exit "$fail"
