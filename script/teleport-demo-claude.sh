#!/usr/bin/env bash
# Moves a teleportable claude-code session from this Mac to a Linux arm64
# host with `brig teleport`, and checks from here that the guest carried on
# there. It is the run recorded in docs/teleport.md, "Claude Code session,
# Mac to Pi".
#
#   script/teleport-demo-claude.sh [--claude] [--keep] [--replace] DST
#
# DST is USER@HOST. Run it on the Mac with brig's settings for this Mac in
# the environment: BRIG_RUNTIME_BIN, BRIG_HULL_STORE_DIR and the other
# settings the session runs with, and BRIG_TELEPORT_REMOTE_BRIG and
# BRIG_TELEPORT_REMOTE_ENV for the brig on DST. DST has the layout that
# script/teleport-demo.sh describes under ROOT (default $HOME/b4 there), the
# claude-code image pulled into its hull store, and the portable kernel
# installed with hull's record of it. This Mac reaches DST over ssh with a
# key. REF (default claude@demo) names the session.
#
# The guest runs the workload of script/teleport-demo.sh: a counter that
# keeps fd 3 open on a file it deleted, and an awk process that holds 64 KiB
# of random bytes and prints the same sum every 2 s. Beside it runs a
# stand-in for an agent's HTTPS stream: a rate-limited download that gives
# up after 20 s without data and starts again, every line stamped. After the
# move the counter has gone on, the deleted file still reads, the sum and
# the kernel's boot_id are the ones from before, and the kernel logged one
# more hvi-time resume line. The first `brig sh REF -c true` after the move
# is timed: it runs over ssh on DST. An exec there carries none of the
# secrets this Mac forwards (SECRETS, default GH_TOKEN), and brig says so,
# while a process started before the move still holds them. The stand-in
# shows what an open connection does across the move.
#
# --claude also starts a Claude Code task in the guest before the move:
# `claude -p` with --output-format stream-json, its output and its errors
# stamped into /root/task.jsonl and /root/task.err. The task prints the
# line COUNT n before each function it writes. After the move the COUNT
# lines read from the stream are the ones from before the move, in order,
# followed by new ones: no restart, no gap, and more of them 20 s later
# than at the first look on DST, or the task ended with exit 0. Any error
# event written after the move fails the check, and so does a task that
# exited with another status. It needs a Claude login brig can forward that
# has not expired. This path has not run to the end yet: on the recorded
# run the login brig held had expired, and `claude -p` failed to
# authenticate. Its parser was checked only against made-up streams.
#
# --keep leaves the session on DST and the fenced copy here. Otherwise both
# go at the end: one `brig rm` here removes the copy and forgets the session
# here, and one on DST removes the session there.
#
# The script removes nothing it did not start: when this host or DST
# already has a session REF, it stops, unless --replace says to remove
# that session first, on both hosts, with its guest home.
#
# Exit status: 0 when every check passed, 1 when one failed, 2 for a usage
# error or a session REF that is already there.

# The guest scripts are expanded in the guest, so single quotes and \$ are on
# purpose (SC2016); pass never fails, so A && pass || fail is an
# if-then-else (SC2015).
# shellcheck disable=SC2016,SC2015
set -uo pipefail

CLAUDE=0 KEEP=0 REPLACE=0
while [ $# -gt 1 ]; do
  case $1 in
    --claude) CLAUDE=1 ;;
    --keep) KEEP=1 ;;
    --replace) REPLACE=1 ;;
    *) break ;;
  esac
  shift
done
if [ $# -ne 1 ] || [ "${1#-}" != "$1" ]; then
  sed -n '2,/^$/s/^# \{0,1\}//p' "$0" >&2
  exit 2
fi
DST=$1
ROOT=${ROOT:-'$HOME/b4'}
REF=${REF:-claude@demo}
SECRETS=${SECRETS:-GH_TOKEN}
FAILED=0 NPASS=0 NFAIL=0
OUT=$(mktemp -d "${TMPDIR:-/tmp}/teleport-demo-claude.XXXXXX")

say() { printf '\n== %s\n' "$*"; }
pass() { printf 'PASS %s\n' "$*"; NPASS=$((NPASS + 1)); }
fail() { printf 'FAIL %s\n' "$*"; FAILED=1; NFAIL=$((NFAIL + 1)); }
# now: this host's clock in seconds with microseconds. macOS date has no %N.
now() { perl -MTime::HiRes=time -e 'printf "%.6f\n", time'; }
# on CMD...: CMD in a shell on DST, with ROOT/env.sh read first.
on() { ssh -o BatchMode=yes "$DST" "source $ROOT/env.sh && $*"; }
# guest SCRIPT: SCRIPT in the session's guest, through brig here.
guest() { brig sh "$REF" -c "$1"; }
# field KEY: the rest of the first line that starts with KEY. Output that came
# through ssh can end its lines in CR.
field() { tr -d '\r' | awk -v k="$1" '$1 == k { $1 = ""; sub(/^ /, ""); sub(/ +$/, ""); print; exit }'; }

# The workload of script/teleport-demo.sh.
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
echo "BOOTID $(cat /proc/sys/kernel/random/boot_id)"
echo "RESUMES $(dmesg | grep -c "hvi-time: generation [0-9]*: .*downtime total")"
echo "ADDR $(awk "/32 host LOCAL/ { print p } { p = \$2 }" /proc/net/fib_trie | grep -v "^127\." | sort -u | tr "\n" " ")"
echo "UPTIME $(cut -d" " -f1 /proc/uptime)"
dmesg | grep "hvi-time: generation [0-9]*: "
awk "NR > 1 { g = \$2 - t; if (g > 0.5) printf \"GAP %.3f s between count %d and %d\\n\", g, c, \$1 } { c = \$1; t = \$2 }" /tmp/wl/ticks | sort -k2 -rn | head -1'

# The guest has no /dev/fd, so these pipelines use no >(...).
STANDIN='mkdir -p /root/net
cat > /root/net/run.sh <<'\''EOS'\''
#!/bin/bash
cd /root/net
stamp() { while IFS= read -r l; do printf "%s %s\n" "$(date +%s.%N)" "$l"; done; }
for n in 1 2 3; do
  echo "$(date +%s.%N) START attempt $n" >> /root/net/log
  curl -sS --limit-rate 50k --speed-limit 1000 --speed-time 20 \
    -o /root/net/dl.$n "https://speed.cloudflare.com/__down?bytes=50000000" 2>&1 | stamp >> /root/net/log
  echo "$(date +%s.%N) EXIT attempt $n rc=${PIPESTATUS[0]}" >> /root/net/log
  sleep 1
done
EOS
cat > /root/net/sample.sh <<'\''EOS'\''
#!/bin/bash
while :; do
  f=$(ls -t /root/net/dl.* 2>/dev/null | head -1)
  echo "$(date +%s.%N) ${f##*/} $(stat -c %s "$f" 2>/dev/null)" >> /root/net/progress
  sleep 1
done
EOS
chmod +x /root/net/run.sh /root/net/sample.sh
setsid nohup /root/net/run.sh </dev/null >/dev/null 2>&1 &
setsid nohup /root/net/sample.sh </dev/null >/dev/null 2>&1 &
sleep 5
cat /root/net/log; tail -1 /root/net/progress'
# NET: the stand-in's log, and the bytes it has, and what a process started
# before the move holds, by name only.
NET='cat /root/net/log
echo "BYTES $(cat /root/net/dl.* 2>/dev/null | wc -c)"
p=$(pgrep -f "^/bin/bash /root/net/run.sh" | head -1)
echo "HELD-NAMES $(tr "\0" "\n" </proc/$p/environ | cut -d= -f1 | sort | tr "\n" " ")"'

TASK='mkdir -p /root/task
cat > /root/task/run.sh <<'\''EOS'\''
#!/bin/bash
cd /root/task
stamp() { while IFS= read -r l; do printf "%s %s\n" "$(date +%s.%N)" "$l"; done; }
rm -f /root/task/err.fifo && mkfifo /root/task/err.fifo
stamp < /root/task/err.fifo > /root/task.err &
claude -p "Write a 400-line Python program one function at a time; before each function print the line COUNT n" \
  --output-format stream-json --verbose --include-partial-messages 2>/root/task/err.fifo | stamp > /root/task.jsonl
echo "$(date +%s.%N) EXIT ${PIPESTATUS[0]}" >> /root/task.exit
wait
EOS
chmod +x /root/task/run.sh
setsid nohup /root/task/run.sh </dev/null >/dev/null 2>&1 &
sleep 20
echo "TASKLINES $(wc -l < /root/task.jsonl)"
cat /root/task.exit 2>/dev/null'
TASKCHECK='echo "TASKLINES $(wc -l < /root/task.jsonl)"
echo "TASKPROC $(pgrep -c -x claude)"
cat /root/task.exit /root/task.err 2>/dev/null
tail -c 600 /root/task.jsonl'
# TASKREAD: the task's stream, whole, then the end of the task.
TASKREAD='cat /root/task.jsonl; echo "TASKPROC $(pgrep -c -x claude)"; cat /root/task.exit 2>/dev/null'

# TASKJUDGE BEFORE AFTER1 AFTER2: the --claude check, from three reads of
# the task's stream (TASKREAD's output): right before the move, on DST, and
# on DST 20 s later. "After the move" is what the stream has past the first
# read, which takes in the second or so of brig's checks before the pause. Each line of the stream is a stamp and a stream-json event.
# The text is put back together from the text_delta events alone, or from
# the assistant messages when the stream has none, and a COUNT n counts only
# once a newline ends it, so a COUNT 1 cut off on its way to COUNT 12 is not
# read. It prints PASS and FAIL lines.
TASKJUDGE='
import json, re, sys

def read(path):
    events, proc, exits = [], None, []
    for raw in open(path, encoding="utf-8", errors="replace"):
        raw = raw.rstrip("\r\n")
        if raw.startswith("TASKPROC "):
            proc = int(raw.split()[1]) if raw.split()[1:] and raw.split()[1].isdigit() else 0
            continue
        m = re.match(r"^[0-9.]+ EXIT (-?[0-9]+)$", raw)
        if m:
            exits.append(int(m.group(1)))
            continue
        stamp, _, body = raw.partition(" ")
        try:
            events.append((body, json.loads(body)))
        except ValueError:
            events.append((body, None))
    return events, proc, exits

def text(events):
    deltas, whole = [], []
    for _, e in events:
        if not isinstance(e, dict):
            continue
        if e.get("type") == "stream_event":
            ev = e.get("event") or {}
            d = ev.get("delta") or {}
            if ev.get("type") == "content_block_delta" and d.get("type") == "text_delta":
                deltas.append(d.get("text", ""))
        elif e.get("type") == "assistant":
            for c in (e.get("message") or {}).get("content") or []:
                if isinstance(c, dict) and c.get("type") == "text":
                    whole.append(c.get("text", "") + "\n")
    return "".join(deltas) if deltas else "".join(whole)

def counts(events):
    return [int(n) for n in re.findall(r"(?m)^COUNT ([0-9]+)[ \t]*\n", text(events))]

def errors(events):
    out = []
    for body, e in events:
        bad = "api_error" in body
        if isinstance(e, dict):
            bad = bad or e.get("is_error") is True or e.get("type") == "error" or (
                e.get("type") == "result" and e.get("subtype") not in (None, "success"))
        if bad:
            out.append(body[:160])
    return out

def say(ok, what):
    print(("PASS " if ok else "FAIL ") + what)

(b, _, bx), (a1, _, x1), (a2, proc, x2) = (read(p) for p in sys.argv[1:4])
cb, c1, c2 = counts(b), counts(a1), counts(a2)
say(not bx, "the task was running when the guest paused" + (" (it had exited %s)" % bx if bx else ""))
say(c1[:len(cb)] == cb and c2[:len(c1)] == c1,
    "the COUNT lines after the move go on from the %d before it, with no restart" % len(cb))
say(c2 == list(range(1, len(c2) + 1)), "the COUNT lines run 1 to %d with no gap or repeat" % len(c2))
say(len(c2) > len(cb), "the task wrote %d COUNT lines after the move (%d before, %d now)" % (
    len(c2) - len(cb), len(cb), len(c2)))
alive = len(c2) > len(c1)
say(alive or x2 == [0], "the stream is alive on DST: %d COUNT lines at the first look, %d 20 s later%s" % (
    len(c1), len(c2), ", and the task ended with exit 0" if x2 == [0] else ""))
say(x2 in ([], [0]) and (x2 == [0] or (proc or 0) >= 1),
    "the task is running (%s claude) or ended with exit 0 (exits %s)" % (proc, x2))
late = errors(a2[len(b):])
say(not late, "no error event in the stream after the move" + ("" if not late else ": " + late[0]))
'

# have [on]: whether `brig ls` lists the session's sandbox VM, here, or on
# DST with on. A ref can be spelled with an alias, the sandbox name cannot.
have() {
  if [ "${1:-}" = on ]; then on "brig ls --json"; else brig ls --json; fi 2>/dev/null | python3 -c 'import json, sys
d = json.load(sys.stdin).get("data") or {}
sys.exit(0 if any(s.get("sandbox") == sys.argv[1] for s in d.get("sandboxes") or []) else 1)' "$VM"
}
clean() {
  if ! have && ! have on; then return 0; fi
  if [ "$REPLACE" = 0 ]; then
    echo "a session $REF is already here or on $DST; --replace removes it on both hosts, with its guest home" >&2
    exit 2
  fi
  brig rm "$REF"
  on "brig rm $REF; true"
  brig rm "$REF" 2>/dev/null
  true
}

say "hosts"
echo "here: brig $(brig --version | cut -d' ' -f2-3), hull $("${BRIG_RUNTIME_BIN:-hull}" --version | cut -d' ' -f2-3)"
on 'echo "$(hostname): brig $(brig --version | cut -d" " -f2-3), hull $(hull --version | cut -d" " -f2-3), free $(free -m | awk "/^Mem/ {print \$7}") MiB"'
VM=$(brig plan "$REF" 2>/dev/null | awk '$1 == "SANDBOX" { print $2 }')
[ -n "$VM" ] || { echo "brig plan $REF names no sandbox" >&2; exit 2; }
clean

say "boot here"
t0=$(now); brig -q run --teleportable "$REF" -d; rc=$?; t1=$(now)
[ "$rc" = 0 ] && pass "brig run --teleportable $REF: exit 0 in $(awk "BEGIN { printf \"%.1f\", $t1 - $t0 }") s" \
  || { fail "brig run --teleportable $REF: exit $rc"; exit 1; }
brig info "$REF" 2>/dev/null | grep -E '^(TELEPORTABLE|MACHINE|NETWORK|CREDENTIALS) '
S=$(guest "$START" 2>/dev/null)
TOKEN=$(field TOKEN <<<"$S") BOOTID=$(field BOOTID <<<"$S")
[ -n "$TOKEN" ] && [ -n "$BOOTID" ] && pass "the workload started (token $TOKEN)" || fail "the workload did not start"
# Each grep below reads a variable, not a pipe: with pipefail, grep -q
# quitting early would fail the pipe on its writer's SIGPIPE.
N=$(guest "$STANDIN" 2>/dev/null)
grep -q 'START attempt 1' <<<"$N" && pass "the HTTPS stand-in started" || fail "the HTTPS stand-in did not start"
if [ "$CLAUDE" = 1 ]; then
  T=$(guest "$TASK" 2>&1); echo "$T"
  L0=$(field TASKLINES <<<"$T")
  [ "${L0:-0}" -gt 0 ] && ! grep -q '^[0-9.]* EXIT' <<<"$T" \
    && pass "the Claude Code task is streaming ($L0 lines)" || fail "the Claude Code task is not streaming"
fi
sleep 3
BEFORE=$(guest "$CHECK" 2>/dev/null); echo "$BEFORE"

[ "$CLAUDE" = 1 ] && guest "$TASKREAD" >"$OUT/task.0" 2>/dev/null

say "teleport here -> $DST"
# The group runs in a subshell of the pipeline, so brig's status goes out
# through a file: $? after the group would be tee's, and inside an echo's
# $(now) it would be that substitution's.
{ echo "$(now) start"; brig teleport "$REF" --to "$DST" 2>&1; rc=$?; echo "$rc" >"$OUT/move.rc"; echo "$(now) rc=$rc"; } \
  | perl -MTime::HiRes=time -ne '$|=1; if (/^\d{10}\.\d+ /) { print } else { printf "%.6f %s", time, $_ }' \
  | tee "$OUT/move.log"
MRC=$(cat "$OUT/move.rc" 2>/dev/null)
TX=$(sed -n 's/.*teleported .*(move \([0-9a-f]\{32\}\)).*/\1/p' "$OUT/move.log" | head -1)
[ "$MRC" = 0 ] && [ -n "$TX" ] && pass "brig teleport exited 0 and committed move $TX" \
  || { fail "brig teleport did not commit (exit ${MRC:-unknown}, move ${TX:-not named})"; exit 1; }

say "the first exec after the move, from here"
# Over ssh with a terminal, the brig there writes its notices to stdout.
t0=$(now); brig sh "$REF" -c true >"$OUT/first.err" 2>&1; rc=$?; t1=$(now)
cat "$OUT/first.err"
[ "$rc" = 0 ] && pass "brig sh $REF -c true on $DST: exit 0 in $(awk "BEGIN { printf \"%.3f\", $t1 - $t0 }") s" \
  || fail "brig sh $REF -c true on $DST: exit $rc in $(awk "BEGIN { printf \"%.3f\", $t1 - $t0 }") s"
grep -q "runs on $DST" "$OUT/first.err" && pass "brig says the session runs on $DST" || fail "brig did not say where the session runs"

say "the guest after the move"
AFTER=$(guest "$CHECK" 2>/dev/null); echo "$AFTER"
before=$(tr -d '\r' <<<"$BEFORE") after=$(tr -d '\r' <<<"$AFTER")
c0=$(field COUNT <<<"$before") c1=$(field COUNT <<<"$after")
[ -n "$c1" ] && [ "$c1" -gt "${c0:-0}" ] && pass "the counter went on ($c0 -> $c1)" || fail "the counter did not go on ($c0 -> $c1)"
[ "$(field HELD <<<"$after")" = "$TOKEN" ] && grep -q '(deleted)$' <<<"$after" \
  && pass "the deleted file held open still reads $TOKEN" || fail "the deleted file held open: $(grep HELD <<<"$after" | tr '\n' ' ')"
sums=$(field SUMS <<<"$after")
[ "$(wc -w <<<"$sums" | tr -d ' ')" = 1 ] && [ "$sums" = "$(field SUMS <<<"$before")" ] \
  && pass "RANDSUM is the sum from before ($sums)" || fail "RANDSUM before: $(field SUMS <<<"$before"), after: $sums"
[ "$(field BOOTID <<<"$after")" = "$BOOTID" ] && pass "the kernel did not boot again (boot_id $BOOTID)" \
  || fail "boot_id was $BOOTID, is $(field BOOTID <<<"$after")"
r0=$(field RESUMES <<<"$before") r1=$(field RESUMES <<<"$after")
[ -n "$r0" ] && [ "$r1" = $((r0 + 1)) ] && pass "the kernel logged one more hvi-time resume line ($r0 -> $r1)" \
  || fail "hvi-time resume lines $r0 -> $r1, want one more"
[ "$(field ADDR <<<"$after")" = "$(field ADDR <<<"$before")" ] && pass "the guest kept its address $(field ADDR <<<"$after")" \
  || fail "the address was $(field ADDR <<<"$before"), is $(field ADDR <<<"$after")"
PS=$(on "hull --store-dir \$BRIG_HULL_STORE_DIR ps")
grep -q "^$VM  *running " <<<"$PS" && pass "$DST runs $VM" || fail "$DST does not run $VM"
PS=$("${BRIG_RUNTIME_BIN:-hull}" ${BRIG_HULL_STORE_DIR:+--store-dir "$BRIG_HULL_STORE_DIR"} ps); grep "^$VM " <<<"$PS"
grep -q "^$VM .* migrated\$" <<<"$PS" \
  && pass "this Mac keeps a stopped copy of $VM whose checkpoint is migrated" || fail "this Mac's copy of $VM is not migrated"

say "credentials on $DST"
guest 'echo "ENV $(env | cut -d= -f1 | sort | tr "\n" " ")"' >"$OUT/env.err" 2>&1
E=$(field ENV <"$OUT/env.err"); echo "an exec on $DST has: $E"
grep -qw PATH <<<"$E" && pass "an exec on $DST ran and printed its environment's names" \
  || fail "an exec on $DST printed no environment: $(tr '\n' ' ' <"$OUT/env.err")"
grep -q 'this host has no value for' "$OUT/env.err" && pass "brig on $DST says this exec carries no secret: $(grep -o 'has no value for .*' "$OUT/env.err")" \
  || fail "brig on $DST did not say which secrets it could not hand in"
sleep 20
N=$(guest "$NET" 2>/dev/null); echo "$N"
for s in $SECRETS; do
  if [ -z "$E" ]; then fail "whether an exec on $DST got $s is not known: none printed its environment"
  elif grep -qw "$s" <<<"$E"; then fail "an exec on $DST got $s"
  else pass "an exec on $DST got no $s"; fi
  grep -qw "$s" <<<"$(field HELD-NAMES <<<"$N")" && pass "a process started before the move still holds $s" \
    || fail "the process started before the move lost $s"
done

say "the HTTPS stand-in across the move"
b0=$(field BYTES <<<"$N"); sleep 10; b1=$(guest "$NET" 2>/dev/null | field BYTES)
[ -n "$b1" ] && [ "$b1" -gt "${b0:-0}" ] && pass "the stand-in downloads on $DST ($b0 -> $b1 bytes)" || fail "the stand-in stopped ($b0 -> $b1 bytes)"
if [ "$CLAUDE" = 1 ]; then
  say "the Claude Code task across the move"
  K=$(guest "$TASKCHECK" 2>/dev/null); echo "$K"
  guest "$TASKREAD" >"$OUT/task.1" 2>/dev/null
  sleep 20
  guest "$TASKREAD" >"$OUT/task.2" 2>/dev/null
  while IFS= read -r l; do
    case $l in
      "PASS "*) pass "${l#PASS }" ;;
      *) fail "${l#FAIL }" ;;
    esac
  done < <(python3 -c "$TASKJUDGE" "$OUT/task.0" "$OUT/task.1" "$OUT/task.2" 2>&1 || echo "FAIL the task check did not run")
fi

say "numbers"
# The move's numbers, from hull's records here and on DST, as in
# script/teleport-demo.sh.
python3 - "${BRIG_HULL_STORE_DIR:-$HOME/.hull/store}" "$VM" "$TX" "$OUT/move.log" <<'PY'
import json, subprocess, sys, time
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
moving = next(float(t) for t, l in lines if "the guest pauses here" in l)
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

if [ "$KEEP" = 0 ]; then
  say "clean up: the fenced copy here, then the session on $DST"
  # One rm here removes the fenced copy and forgets the session here.
  brig rm "$REF"
  on "brig rm $REF"
fi

say "result"
rm -rf "$OUT"
echo "$NPASS passed, $NFAIL failed"
if [ "$FAILED" = 0 ]; then echo "every check passed"; else echo "a check failed"; fi
exit "$FAILED"
