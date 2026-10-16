#!/usr/bin/env bash
# Moves a teleportable session between two Linux arm64 hosts with
# `brig teleport`, and checks that the guest carried on, then moves it back.
# It is the run recorded in docs/teleport.md, "On real hardware".
#
#   script/teleport-demo.sh [--faults] SRC DST
#
# SRC and DST are USER@HOST. This machine reaches both over ssh with a key,
# and SRC reaches DST, and DST reaches SRC, under the same names. Each host
# has the same layout under ROOT (default $HOME/b4 on that host):
#
#   ROOT/env.sh     the settings brig runs with there; see docs/teleport.md
#   ROOT/bin/       brig, hull and hvi, the same builds on both hosts
#   ROOT/profiles/  a teleportable, genericBoot profile named REF (default
#                   ubuntu) with `network: offline` and `cpus: 1`
#
# and the portable kernel installed in the hull store with its record:
#   HULL_PORTABLE_KERNEL=<Image> hull --store-dir ROOT/store assets pull --profile portable-arm64-v1
#
# The workload runs in the guest: a counter held in a shell variable that
# writes the count every 200 ms and keeps fd 3 open on a file it deleted, and
# an awk process that holds 64 KiB of random bytes in memory and prints
# "RANDSUM <n> <sum>" every 2 s. After each move the counter has gone on, the
# deleted file still reads, the sum is the one from before, the kernel's
# boot_id is the one from before (a boot makes a new one), and the kernel
# logged one more hvi-time resume line. hull's records on both hosts say
# which one runs the guest.
#
# --faults also runs the refusals and failures, each on a fresh session, and
# checks each exit status and where the guest ended up:
#   - a policy DST cannot enforce: exit 7, no move on record, the session
#     goes on; then the same session, on an isolated network under a deny
#     policy, moves for real and keeps its address and its policy, and one
#     connection the policy allows opens on both hosts and one it denies
#     does not;
#   - a session that is not teleportable: exit 7;
#   - an abort after the transfer: the move exits 1, --abort 0, and the
#     source runs the guest again;
#   - a lost commit answer: the move exits 1, the retry --status names exits
#     0, and DST runs the guest;
#   - the source brig killed with SIGKILL during the commit: exit 137, and
#     hull, which outlives it, commits the move.
# It writes a stand-in ssh to ROOT/fault on SRC, a stand-in hull to
# ROOT/fault on DST, and a profile and a policy for the policy case to ROOT
# on both hosts. The denied connection goes to DENY_TO (default
# 1.1.1.1/443); it counts only when both hosts reach it themselves. The
# allowed one goes to port 22 of DST's address as SRC resolves it.
#
# Exit status: 0 when every check passed, 1 when one failed, 2 for a usage
# error.
# Most command lines here are expanded on the other host, so single quotes
# and \$ are on purpose (SC2016); pass never fails, so A && pass || fail
# is an if-then-else (SC2015).
# shellcheck disable=SC2016,SC2015
set -uo pipefail

FAULTS=0
if [ "${1:-}" = --faults ]; then FAULTS=1; shift; fi
if [ $# -ne 2 ]; then
  sed -n '2,/^$/s/^# \{0,1\}//p' "$0" >&2
  exit 2
fi
SRC=$1 DST=$2
ROOT=${ROOT:-'$HOME/b4'}
REF=${REF:-ubuntu}
DENY_TO=${DENY_TO:-1.1.1.1/443}
# vmof REF: the sandbox brig names for REF.
vmof() { printf 'brig-%s' "${1/@/-}"; }
VM=$(vmof "$REF")
FAILED=0 NPASS=0 NFAIL=0
OUT=$(mktemp -d "${TMPDIR:-/tmp}/teleport-demo.XXXXXX")

say() { printf '\n== %s\n' "$*"; }
pass() { printf 'PASS %s\n' "$*"; NPASS=$((NPASS + 1)); }
fail() { printf 'FAIL %s\n' "$*"; FAILED=1; NFAIL=$((NFAIL + 1)); }
# expect LABEL WANT-RC GOT-RC
expect() { [ "$3" = "$2" ] && pass "$1: exit $3" || fail "$1: exit ${3:-(none)}, want $2"; }
# on HOST CMD...: CMD in a shell on HOST, with ROOT/env.sh read first.
on() { local h=$1; shift; ssh -o BatchMode=yes "$h" "source $ROOT/env.sh && $*"; }
# A teleport destination as the other host names it: the host part only.
hostof() { printf '%s' "${1#*@}"; }

# The guest scripts go to `brig sh -c` on stdin, so no shell on the way
# quotes them again.
START='mkdir -p /tmp/wl
nohup setsid bash -c '\''
  tok=$(od -An -tx1 -N8 /dev/urandom | tr -d " \n")
  exec 3<>/tmp/wl/held
  echo "HELD $tok" >&3
  rm /tmp/wl/held
  echo $$ > /tmp/wl/counter.pid
  echo "$tok" > /tmp/wl/token.start
  i=0
  while :; do
    i=$((i+1)); t=$(date +%s.%N)
    echo "$i $t" > /tmp/wl/count.new && mv /tmp/wl/count.new /tmp/wl/count
    echo "$i $t" >> /tmp/wl/ticks
    sleep 0.2
  done'\'' >/dev/null 2>&1 </dev/null &
nohup setsid sh -c '\''od -An -tu1 -v -N65536 /dev/urandom | awk -v out=/tmp/wl/randsum "
  { for (i = 1; i <= NF; i++) a[n++] = \$i }
  END { while (1) { s = 0; for (i = 0; i < n; i++) s = (s * 31 + a[i]) % 2147483647
          k++; printf \"RANDSUM %d %d\\n\", k, s >> out; close(out); system(\"sleep 2\") } }"'\'' \
  >/dev/null 2>&1 </dev/null &
sleep 3
echo "TOKEN $(cat /tmp/wl/token.start)"
rm -f /tmp/wl/token.start
echo "BOOTID $(cat /proc/sys/kernel/random/boot_id)"
tail -1 /tmp/wl/randsum'
CHECK='pid=$(cat /tmp/wl/counter.pid)
echo "COUNT $(cut -d" " -f1 /tmp/wl/count)"
echo "HELD-FD $(readlink /proc/$pid/fd/3)"
echo "$(cat /proc/$pid/fd/3)"
echo "SUMS $(awk "{print \$3}" /tmp/wl/randsum | sort -u | tr "\n" " ")"
tail -1 /tmp/wl/randsum
echo "BOOTID $(cat /proc/sys/kernel/random/boot_id)"
echo "RESUMES $(dmesg | grep -c "hvi-time: generation [0-9]*: .*downtime total")"
echo "ADDR $(awk "/32 host LOCAL/ { print p } { p = \$2 }" /proc/net/fib_trie | grep -v "^127\." | sort -u | tr "\n" " ")"
echo "CLOCKSOURCE $(cat /sys/devices/system/clocksource/clocksource0/current_clocksource)"
echo "UPTIME $(cut -d" " -f1 /proc/uptime)"
dmesg | grep "hvi-time: generation [0-9]*: "
awk "NR > 1 { g = \$2 - t; if (g > 0.5) printf \"GAP %.3f s between count %d and %d\\n\", g, c, \$1 } { c = \$1; t = \$2 }" /tmp/wl/ticks | sort -k2 -rn | head -1'

# guest HOST REF SCRIPT: SCRIPT in REF's guest, through brig on HOST.
guest() { on "$1" "brig sh $2 -c \"\$(cat)\"" <<<"$3"; }
# field KEY: the rest of the first line that starts with KEY. A guest's
# output that came through ssh can end its lines in CR.
field() { tr -d '\r' | awk -v k="$1" '$1 == k { $1 = ""; sub(/^ /, ""); sub(/ +$/, ""); print; exit }'; }

# check_after LABEL BEFORE AFTER TOKEN BOOTID RESTORES: the guest carried
# on, with RESTORES (0 or 1) restores of it between BEFORE and AFTER.
check_after() {
  local label=$1 before after token=$4 bootid=$5 restores=$6 c0 c1 r0 r1
  before=$(tr -d '\r' <<<"$2") after=$(tr -d '\r' <<<"$3")
  c0=$(field COUNT <<<"$before") c1=$(field COUNT <<<"$after")
  [ -n "$c1" ] && [ "$c1" -gt "$c0" ] && pass "$label: the counter went on ($c0 -> $c1)" \
    || fail "$label: the counter did not go on ($c0 -> $c1)"
  [ "$(field HELD <<<"$after")" = "$token" ] && grep -q '(deleted)$' <<<"$after" \
    && pass "$label: the deleted file held open still reads $token" \
    || fail "$label: the deleted file held open: $(grep HELD <<<"$after" | tr '\n' ' ')"
  local sums
  sums=$(field SUMS <<<"$after")
  [ "$(wc -w <<<"$sums" | tr -d ' ')" = 1 ] && [ "$sums" = "$(field SUMS <<<"$before")" ] \
    && pass "$label: RANDSUM is the sum from before ($sums)" \
    || fail "$label: RANDSUM sums before: $(field SUMS <<<"$before"), after: $sums"
  [ -n "$bootid" ] && [ "$(field BOOTID <<<"$after")" = "$bootid" ] \
    && pass "$label: the kernel did not boot again (boot_id $bootid)" \
    || fail "$label: boot_id was ${bootid:-(none)}, is $(field BOOTID <<<"$after")"
  r0=$(field RESUMES <<<"$before") r1=$(field RESUMES <<<"$after")
  [ -n "$r0" ] && [ "$r1" = $((r0 + restores)) ] \
    && pass "$label: the kernel logged $restores more hvi-time resume line(s) ($r0 -> $r1)" \
    || fail "$label: hvi-time resume lines $r0 -> $r1, want $restores more"
}

# runs HOST VM, migrated HOST VM, absent HOST VM: hull's record of VM on HOST.
hullps() { on "$1" "hull --store-dir \$BRIG_HULL_STORE_DIR ps 2>/dev/null"; }
runs() { hullps "$1" | grep -q "^$2  *running " && pass "$1 runs $2" || fail "$1 does not run $2"; }
migrated() {
  hullps "$1" | grep -q "^$2 .* migrated\$" && pass "$1 keeps a stopped copy of $2 whose checkpoint is migrated" \
    || fail "$1's copy of $2 is not migrated"
}
absent() { hullps "$1" | grep -q "^$2 " && fail "$1 has an instance $2" || pass "$1 has no instance $2"; }
# has LABEL FILE TEXT: FILE holds TEXT.
has() { grep -qF -- "$3" "$2" && pass "$1" || fail "$1: no \"$3\" in $(basename "$2")"; }
# rcof PREFIX FILE: the status a line "PREFIXrc=N" in FILE gives.
rcof() { tr -d '\r' <"$2" | sed -n "s/^$1rc=//p" | tail -1; }
# start HOST REF: start the workload in REF's guest; sets TOK and BID.
start() {
  local s
  s=$(guest "$1" "$2" "$START")
  TOK=$(field TOKEN <<<"$s") BID=$(field BOOTID <<<"$s")
  [ -n "$TOK" ] && [ -n "$BID" ] && pass "the workload started in $2 (token $TOK)" || fail "the workload did not start in $2"
}

# timings HOST TXID LOG: the move's numbers, from hull's records on both hosts
# and the stamped brig output in LOG on HOST, with the clock offset between
# the two hosts measured over one ssh session.
timings() {
  on "$1" "python3 - \"\$BRIG_HULL_STORE_DIR\" $VM $2 $3" <<'PY'
import json, re, subprocess, sys, time
from datetime import datetime
store, vm, txid, log = sys.argv[1:]
src = json.load(open(f"{store}/instances/{vm}/migration.json"))
peer = src["remote"]["target"]
def ts(s):
    base, _, frac = s.rstrip("Z").partition(".")
    return datetime.fromisoformat(base + "+00:00").timestamp() + float("0." + (frac or "0"))
p = subprocess.Popen(["ssh", "-T", "-o", "BatchMode=yes", peer, "while read l; do date +%s.%N; done"],
                     stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, bufsize=1)
samples = []
for i in range(10):
    a = time.time(); p.stdin.write("x\n"); p.stdin.flush(); b = float(p.stdout.readline()); c = time.time()
    if i:
        samples.append((b - (a + c) / 2, c - a))
p.stdin.close(); p.wait()
off = sorted(samples)[len(samples) // 2][0]
dst = json.loads(subprocess.run(["ssh", "-o", "BatchMode=yes", peer,
    f"{src['remote']['hull']} --store-dir {src['remote']['store']} migrate-status --txid {txid}"],
    capture_output=True, text=True).stdout)["record"]
h = {e["state"]: ts(e["at"]) for e in src["history"]}
d = {e["state"]: ts(e["at"]) - off for e in dst["history"]}
lines = [l.split(" ", 1) for l in open(log) if l[:1].isdigit()]
t0 = float(lines[0][0])
# brig says "moving REF from HERE to DST" as it hands the move to hull;
# a brig before 304689b said "the guest pauses here".
moving = next(float(t) for t, l in lines if "the guest pauses here" in l or re.search(r"brig: moving \S+ from \S+ to ", l))
end = float(lines[-1][0])
tr = src["transfer"]
print(f"clock offset {peer} minus here: {off * 1000:+.1f} ms")
print(f"brig preflight (checks, probe, receive, dry run): {moving - t0:.2f} s")
print(f"hull: reserve {h['reserved'] - h['preflight']:.2f} s, fence {src['timingsMs']['fence'] / 1000:.2f} s, "
      f"transfer {tr['seconds']:.2f} s, restore paused {src['timingsMs']['prepare'] / 1000:.2f} s, "
      f"commit {src['timingsMs']['commit'] / 1000:.2f} s")
print(f"transfer: {tr['bytes']} bytes ({tr['bytes'] / 2**20:.1f} MiB) in {tr['seconds']:.2f} s, "
      f"{tr['bytesPerSec'] / 2**20:.1f} MiB/s; disk data {src['disk']['dataBytes']} of {src['disk']['size']} bytes")
print(f"downtime, reserved here to running there: {d['running'] - h['reserved']:.2f} s")
print(f"downtime, fenced here to running there: {d['running'] - h['fenced']:.2f} s")
print(f"brig teleport, start to end: {end - t0:.2f} s")
print(f"epoch {dst['epoch']}, destination {dst['state']}")
PY
}

# move FROM TO [REF]: brig teleport on FROM, its output stamped into a log
# there. Prints the txid of a move that committed, and nothing otherwise.
move() {
  local log ref=${3:-$REF}
  log=$ROOT/logs/demo-move-$(hostof "$1")${3:+-$3}.log
  on "$1" "mkdir -p $ROOT/logs; { echo \"\$(date +%s.%N) start\"; brig teleport $ref --to $2 2>&1; rc=\$?; echo \"\$(date +%s.%N) rc=\$rc\"; } | while IFS= read -r l; do case \$l in [0-9]*) echo \"\$l\";; *) echo \"\$(date +%s.%N) \$l\";; esac; done > $log; cat $log" >&2
  on "$1" "grep -q ' rc=0\$' $log && sed -n 's/.*(move \\([0-9a-f]\\{32\\}\\)).*/\\1/p' $log | head -1"
}

# PTYTIME: run argv on a terminal, and print the time to the first shell
# prompt it shows; then exit the shell. Gives up after 60 s.
PTYTIME='import os, pty, re, select, signal, sys, time
t0 = time.time()
pid, fd = pty.fork()
if pid == 0:
    os.execvp(sys.argv[1], sys.argv[1:])
buf, said = b"", "REATTACH no prompt within 60 s"
while time.time() - t0 < 60:
    if not select.select([fd], [], [], 1)[0]:
        continue
    try:
        d = os.read(fd, 4096)
    except OSError:
        break
    if not d:
        break
    buf += d
    if re.search(rb"[#$]\s*$", buf):
        said = f"REATTACH {time.time() - t0:.3f} s to the first prompt"
        os.write(fd, b"exit\n")
        break
print(said)
end = time.time() + 10
while time.time() < end and os.waitpid(pid, os.WNOHANG) == (0, 0):
    try:
        select.select([fd], [], [], 0.5) and os.read(fd, 4096)
    except OSError:
        pass
try:
    os.kill(pid, signal.SIGKILL)
except ProcessLookupError:
    pass'

# reattach HOST: time from `brig sh` on HOST to its first prompt.
reattach() { on "$1" "python3 -c \"\$(cat)\" brig sh $REF" <<<"$PTYTIME"; }

clean() {
  local h
  for h in "$DST" "$SRC" "$DST" "$SRC"; do on "$h" "brig rm $1 >/dev/null 2>&1; true"; done
}

say "hosts"
for h in "$SRC" "$DST"; do
  on "$h" 'echo "$(hostname): brig $(brig --version | cut -d" " -f2-3), hull $(hull --version | cut -d" " -f2-3), sha256 $(cd $(dirname $(command -v brig)) && sha256sum brig hull hvi | cut -c1-12 | tr "\n" " ")"'
done
clean "$REF"

say "the portable kernel, and hull's record of it"
for h in "$SRC" "$DST"; do
  if on "$h" 'brig doctor 2>&1 | grep -q "ok  portable" && test -f "$(hull --store-dir "$BRIG_HULL_STORE_DIR" assets dir --profile portable-arm64-v1)/provenance.json"'; then
    pass "$h has the pinned portable kernel and hull's record of it"
  else
    fail "$h: brig doctor does not report the portable kernel ok, or hull has no record of it (hull assets pull --profile portable-arm64-v1)"
  fi
done

say "boot on $SRC"
on "$SRC" "t0=\$(date +%s%N); brig -q run --teleportable $REF -d; echo \"run rc=\$? in \$(( (\$(date +%s%N) - t0) / 1000000 )) ms\"; brig info $REF | grep -E '^(TELEPORTABLE|MACHINE|NETWORK) '"
start "$SRC" "$REF"; TOKEN=$TOK BOOTID=$BID
sleep 3
BEFORE=$(guest "$SRC" "$REF" "$CHECK"); echo "$BEFORE"

say "teleport $SRC -> $DST"
TX=$(move "$SRC" "$DST")
[ -n "$TX" ] && pass "brig teleport committed move $TX" || fail "brig teleport did not commit"

say "after the move, from here: ssh $SRC brig sh $REF lands on $(hostof "$DST")"
AFTER=$(guest "$SRC" "$REF" "$CHECK"); echo "$AFTER"
check_after "$SRC -> $DST" "$BEFORE" "$AFTER" "$TOKEN" "$BOOTID" 1
runs "$DST" "$VM"
migrated "$SRC" "$VM"
on "$SRC" "brig ls; brig teleport $REF --status; brig info $REF 2>/dev/null | grep -E '^(TELEPORT|MACHINE) '"
reattach "$SRC"
printf 'from here, ssh -t %s brig sh %s: ' "$SRC" "$REF"
python3 -c "$PTYTIME" ssh -t -o BatchMode=yes "$SRC" "source $ROOT/env.sh && brig sh $REF"
say "numbers, $SRC -> $DST"
timings "$SRC" "$TX" "$ROOT/logs/demo-move-$(hostof "$SRC").log"

say "move back: $DST -> $SRC, after brig rm twice on $SRC"
on "$SRC" "brig rm $REF; brig rm $REF"
BEFORE=$(guest "$DST" "$REF" "$CHECK")
TX2=$(move "$DST" "$SRC")
[ -n "$TX2" ] && pass "brig teleport on $DST committed move $TX2" || fail "the move back did not commit"
AFTER=$(guest "$SRC" "$REF" "$CHECK"); echo "$AFTER"
check_after "$DST -> $SRC" "$BEFORE" "$AFTER" "$TOKEN" "$BOOTID" 1
runs "$SRC" "$VM"
migrated "$DST" "$VM"
say "numbers, $DST -> $SRC"
timings "$DST" "$TX2" "$ROOT/logs/demo-move-$(hostof "$DST").log"

say "brig rm"
on "$DST" "brig rm $REF; brig rm $REF"
on "$SRC" "brig rm $REF"
for h in "$SRC" "$DST"; do
  if on "$h" "brig ls --json | grep -q '\"sandbox\": \"$VM\"' || hull --store-dir \$BRIG_HULL_STORE_DIR ps 2>/dev/null | grep -q '^$VM '"; then
    fail "$h still has $VM"
  else
    pass "$h has nothing of $REF left"
  fi
done

if [ "$FAULTS" = 1 ]; then
  FROOT=$ROOT/fault

  say "faults: a policy $DST cannot enforce"
  # The policy allows DST's address, as SRC resolves it, and 10.0.0.0/8.
  ALLOW_IP=$(on "$SRC" "getent ahostsv4 $(hostof "$DST") | awk 'NR == 1 { print \$1 }'")
  ALLOW_TO=$ALLOW_IP/22
  for h in "$SRC" "$DST"; do
    on "$h" "mkdir -p \$BRIG_POLICY_DIR && sed -e 's/^name: .*/name: worker/' -e 's/^kind: .*/kind: agent/' -e 's/^network: .*/network: isolated/' \"\$BRIG_PROFILE_DIR/$REF.yaml\" > \"\$BRIG_PROFILE_DIR/worker.yaml\" && printf 'apiVersion: brig.sh/v1alpha1\nname: locked\ndesc: deny by default (teleport-demo)\negress:\n  default: deny\n  allow:\n    - cidr: 10.0.0.0/8\n    - cidr: $ALLOW_IP/32\n' > \"\$BRIG_POLICY_DIR/locked.yaml\""
  done
  # A stand-in hull on DST whose network gateway does not start: the probe
  # there finds no gateway that enforces a policy. Every other verb runs hull.
  on "$DST" "mkdir -p $FROOT/nogw && sed \"s|@HULL@|\$BRIG_RUNTIME_BIN|\" > $FROOT/nogw/hull && chmod +x $FROOT/nogw/hull && ln -sf \"\$(dirname \"\$BRIG_RUNTIME_BIN\")/hvi\" $FROOT/nogw/hvi" <<'EOF'
#!/bin/sh
for a; do [ "$a" = network-gateway ] && { echo "network-gateway: unavailable (teleport-demo)" >&2; exit 1; }; done
exec "@HULL@" "$@"
EOF
  clean worker
  WVM=$(vmof worker)
  on "$SRC" "brig policy attach locked worker >/dev/null; brig -q run --teleportable worker -d"
  start "$SRC" worker; WTOK=$TOK WBID=$BID
  B=$(guest "$SRC" worker "$CHECK")
  on "$SRC" "BRIG_TELEPORT_REMOTE_ENV=\"\$BRIG_TELEPORT_REMOTE_ENV BRIG_RUNTIME_BIN=$FROOT/nogw/hull\" brig teleport worker --to $DST; echo \"rc=\$?\"" >"$OUT/pol.out" 2>&1
  cat "$OUT/pol.out"
  expect "a policy $DST cannot enforce" 7 "$(rcof '' "$OUT/pol.out")"
  has "the refusal says nothing was paused" "$OUT/pol.out" "Nothing was paused"
  on "$SRC" "hull --store-dir \$BRIG_HULL_STORE_DIR migrate --status $WVM 2>&1 | grep -q 'no move on record'" \
    && pass "nothing was paused: hull has no move of $WVM" || fail "hull recorded a move of $WVM"
  runs "$SRC" "$WVM"
  absent "$DST" "$WVM"
  A=$(guest "$SRC" worker "$CHECK"); echo "$A"
  check_after "refused move" "$B" "$A" "$WTOK" "$WBID" 0

  say "an isolated session under a deny policy moves: $SRC -> $DST"
  # conns HOST: the allowed and the denied connection from worker's guest,
  # through brig on HOST.
  conns() {
    guest "$1" worker "for t in $ALLOW_TO $DENY_TO; do if timeout 5 bash -c \"exec 3<>/dev/tcp/\$t\" 2>/dev/null; then echo \"CONN \$t open\"; else echo \"CONN \$t closed\"; fi; done" | tr -d '\r'
  }
  # check_conns LABEL OUTPUT: the allowed connection opened, and the denied
  # one did not, where both hosts reach DENY_TO themselves.
  DENY_REACHED=1
  for h in "$SRC" "$DST"; do
    on "$h" "timeout 5 bash -c 'exec 3<>/dev/tcp/$DENY_TO'" 2>/dev/null || DENY_REACHED=0
  done
  check_conns() {
    grep -qx "CONN $ALLOW_TO open" <<<"$2" && pass "$1: the policy lets $ALLOW_TO open" \
      || fail "$1: $ALLOW_TO, which the policy allows, did not open"
    if [ "$DENY_REACHED" = 1 ]; then
      grep -qx "CONN $DENY_TO closed" <<<"$2" && pass "$1: the policy keeps $DENY_TO closed" \
        || fail "$1: $DENY_TO, which the policy denies, opened"
    else
      echo "SKIP $1: a host does not reach $DENY_TO itself, so its refusal proves nothing"
    fi
  }
  C=$(conns "$SRC"); echo "$C"
  check_conns "before the move" "$C"
  B=$(guest "$SRC" worker "$CHECK"); echo "$B"
  on "$SRC" "brig info worker 2>/dev/null | grep -E '^(NETWORK|POLICY) '"
  TXW=$(move "$SRC" "$DST" worker)
  [ -n "$TXW" ] && pass "brig teleport committed move $TXW of worker" || fail "brig teleport did not move worker"
  A=$(guest "$SRC" worker "$CHECK"); echo "$A"
  check_after "policy move" "$B" "$A" "$WTOK" "$WBID" 1
  [ -n "$(field ADDR <<<"$B")" ] && [ "$(field ADDR <<<"$A")" = "$(field ADDR <<<"$B")" ] \
    && pass "the guest kept its address ($(field ADDR <<<"$A"))" \
    || fail "the guest's address was $(field ADDR <<<"$B"), is $(field ADDR <<<"$A")"
  on "$SRC" "brig info worker 2>/dev/null | grep -E '^(TELEPORT|NETWORK|POLICY) '" | tr -d '\r' >"$OUT/pinfo.out"
  cat "$OUT/pinfo.out"
  grep -q "^POLICY  *locked (carried from $SRC)" "$OUT/pinfo.out" \
    && pass "brig info on $DST shows the policy carried from $SRC" || fail "brig info shows no policy carried from $SRC"
  runs "$DST" "$WVM"
  migrated "$SRC" "$WVM"
  C=$(conns "$SRC"); echo "$C"
  check_conns "after the move" "$C"
  clean worker
  on "$SRC" "brig policy detach locked worker >/dev/null"

  say "faults: a session that is not teleportable"
  # Linux hull shares no host directory, so an ordinary session cannot boot
  # there. A sandbox hull runs under the name brig would give one stands in.
  on "$SRC" "hull --store-dir \$BRIG_HULL_STORE_DIR run -d --name brig-plain --hypervisor hvi --rootfs-type block --net none docker.io/library/ubuntu:latest sleep infinity >/dev/null 2>&1; sed -e 's/^name: .*/name: plain/' -e '/^teleportable:/d' \"\$BRIG_PROFILE_DIR/$REF.yaml\" > \"\$BRIG_PROFILE_DIR/plain.yaml\"; brig teleport plain --to $DST; echo \"rc=\$?\"; hull --store-dir \$BRIG_HULL_STORE_DIR rm --force brig-plain >/dev/null 2>&1" >"$OUT/plain.out" 2>&1
  cat "$OUT/plain.out"
  expect "a session that is not teleportable" 7 "$(rcof '' "$OUT/plain.out")"

  # A stand-in ssh on SRC, ahead of ssh on PATH, for hull's steps: it passes
  # every call to ssh except the one FROOT/mode names.
  on "$SRC" "mkdir -p $FROOT/ssh && cat > $FROOT/ssh/ssh && chmod +x $FROOT/ssh/ssh && echo none > $FROOT/mode" <<'EOF'
#!/bin/sh
dir=$(dirname "$(dirname "$0")")
mode=$(cat "$dir/mode" 2>/dev/null)
for a; do last=$a; done
case "$mode:$last" in
prepare:*" migrate-prepare "*) echo "teleport-demo: link cut before migrate-prepare" >&2; exit 255 ;;
commit:*" migrate-commit "*) /usr/bin/ssh "$@" >/dev/null; echo "teleport-demo: the reply to migrate-commit was lost" >&2; exit 255 ;;
stall:*" migrate-commit "*) echo stalled > "$dir/stalled"; sleep 20 ;;
esac
exec /usr/bin/ssh "$@"
EOF
  FSSH="PATH=$FROOT/ssh:\$PATH"

  say "faults: an abort after the transfer resumes the source"
  R=$REF@ab RVM=$(vmof "$REF@ab")
  clean "$R"
  on "$SRC" "brig -q run --teleportable $R -d"
  start "$SRC" "$R"; B=$(guest "$SRC" "$R" "$CHECK")
  on "$SRC" "echo prepare > $FROOT/mode; $FSSH brig teleport $R --to $DST; echo \"rc=\$?\"; echo none > $FROOT/mode; brig teleport $R --status 2>&1 | head -1; brig teleport $R --abort; echo \"abort rc=\$?\"" >"$OUT/ab.out" 2>&1
  cat "$OUT/ab.out"
  has "the link was cut before migrate-prepare" "$OUT/ab.out" "teleport-demo: link cut before migrate-prepare"
  expect "the move with the link cut" 1 "$(rcof '' "$OUT/ab.out")"
  has "--status says the move has not finished" "$OUT/ab.out" "has not finished"
  expect "--abort" 0 "$(rcof 'abort ' "$OUT/ab.out")"
  runs "$SRC" "$RVM"
  absent "$DST" "$RVM"
  A=$(guest "$SRC" "$R" "$CHECK"); echo "$A"
  check_after "abort" "$B" "$A" "$TOK" "$BID" 1
  clean "$R"

  say "faults: a lost commit answer, settled by a retry"
  R=$REF@la RVM=$(vmof "$REF@la")
  clean "$R"
  on "$SRC" "brig -q run --teleportable $R -d"
  start "$SRC" "$R"; B=$(guest "$SRC" "$R" "$CHECK")
  on "$SRC" "echo commit > $FROOT/mode; $FSSH brig teleport $R --to $DST; echo \"rc=\$?\"; echo none > $FROOT/mode" >"$OUT/la-move.out" 2>&1
  cat "$OUT/la-move.out"
  has "the reply to migrate-commit was lost" "$OUT/la-move.out" "teleport-demo: the reply to migrate-commit was lost"
  expect "the move whose commit answer was lost" 1 "$(rcof '' "$OUT/la-move.out")"
  on "$SRC" "brig teleport $R --status" >"$OUT/la.out" 2>&1
  cat "$OUT/la.out"
  has "--status says the move has not finished" "$OUT/la.out" "has not finished"
  RETRY=$(sed -n 's/.*`\(hull [^`]* --txid [0-9a-f]*\)`.*/\1/p' "$OUT/la.out")
  [ -n "$RETRY" ] && pass "--status names the retry: $RETRY" || fail "--status names no retry"
  on "$SRC" "$RETRY; echo \"retry rc=\$?\"; brig teleport $R --status" >"$OUT/la-retry.out" 2>&1
  cat "$OUT/la-retry.out"
  expect "the retry --status names" 0 "$(rcof 'retry ' "$OUT/la-retry.out")"
  has "--status then records the host" "$OUT/la-retry.out" "teleported $R to $DST"
  runs "$DST" "$RVM"
  migrated "$SRC" "$RVM"
  A=$(guest "$SRC" "$R" "$CHECK"); echo "$A"
  check_after "lost answer" "$B" "$A" "$TOK" "$BID" 1
  clean "$R"

  say "faults: the source brig killed during the commit"
  R=$REF@kb RVM=$(vmof "$REF@kb")
  clean "$R"
  on "$SRC" "brig -q run --teleportable $R -d"
  start "$SRC" "$R"; B=$(guest "$SRC" "$R" "$CHECK")
  # brig is this shell's own child, so wait gives its status.
  on "$SRC" "rm -f $FROOT/stalled; echo stall > $FROOT/mode; $FSSH brig teleport $R --to $DST >/dev/null 2>&1 & pid=\$!; for i in \$(seq 60); do [ -f $FROOT/stalled ] && break; sleep 0.5; done; [ -f $FROOT/stalled ] && echo 'ssh holds migrate-commit'; kill -KILL \$pid; wait \$pid; echo \"rc=\$?\"; echo none > $FROOT/mode; brig teleport $R --status 2>&1 | head -2" >"$OUT/kb.out" 2>&1
  cat "$OUT/kb.out"
  has "ssh held migrate-commit when brig was killed" "$OUT/kb.out" "ssh holds migrate-commit"
  expect "the source brig killed with SIGKILL" 137 "$(rcof '' "$OUT/kb.out")"
  has "--status says the move has not finished" "$OUT/kb.out" "has not finished"
  sleep 25
  on "$SRC" "brig teleport $R --status" >"$OUT/kb2.out" 2>&1
  cat "$OUT/kb2.out"
  has "hull outlived brig and committed; --status records the host" "$OUT/kb2.out" "teleported $R to $DST"
  runs "$DST" "$RVM"
  migrated "$SRC" "$RVM"
  A=$(guest "$SRC" "$R" "$CHECK"); echo "$A"
  check_after "killed brig" "$B" "$A" "$TOK" "$BID" 1
  clean "$R"
fi

say "result"
rm -rf "$OUT"
echo "$NPASS passed, $NFAIL failed"
if [ "$FAILED" = 0 ]; then echo "every check passed"; else echo "a check failed"; fi
exit "$FAILED"
