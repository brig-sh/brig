#!/usr/bin/env bash
# Moves a teleportable claude-code session from this Mac to a Linux arm64
# host with `brig teleport`, and checks from here that the guest carried on
# there. It is the run recorded in docs/teleport.md, "Claude Code session,
# Mac to Pi".
#
#   script/teleport-demo-claude.sh [--claude] [--prepare] [--keep] [--replace] DST
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
# `claude -p` with --output-format stream-json, no tools, low effort and
# no thinking, its output and its errors stamped into /root/task.jsonl and
# /root/task.err. The task prints the line COUNT n, as text, before each
# function it writes: with tools, Claude Code writes the program to a file
# through Write and no COUNT line reaches the text, and with thinking on it
# thought for 90 s and more before the first one on the runs of
# 2026-10-16 (MAX_THINKING_TOKENS=0 did not stop it; with the setting
# alwaysThinkingEnabled false it began within seconds on most runs, not
# all). The move waits (up to 60 s) until the stream has at least 2 COUNT
# lines. The API connection that is open at the pause does not survive
# the move: the guest's agent closes it when the guest resumes on DST, and
# Claude Code asks again on a new connection, which may start the answer
# over. So the check passes when the stream comes back on DST within 5 s
# of the resume (the stall: from the guest's own record of its resume to
# the first message_start after it, the start of the response Claude Code
# asks for again; the message_stop that marks the cut-off answer as
# abandoned does not count), no error event comes after that reconnect,
# the task writes more COUNT lines after the move, and the turn completes:
# exit 0 and a result event of subtype success, waited for up to 600 s.
# Whether the COUNT lines go on with no restart, gap or repeat, the error
# events between the resume and the reconnect, the stream's longest
# silence and a message Claude Code gave up on are printed as
# information. Before the agent closed the connection, the
# stream went silent for about 60 s on the runs of 2026-10-16 until
# Claude Code gave up on it. It needs a Claude login brig can forward that
# has not expired.
#
# --prepare runs `brig teleport REF --to DST --prepare` after the boot,
# before the workload starts, and checks that it exited 0 and that DST
# holds the session's base. The move then sends only what the guest
# changed. A DST that holds the base of the image already is sent nothing.
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

CLAUDE=0 PREPARE=0 KEEP=0 REPLACE=0
while [ $# -gt 1 ]; do
  case $1 in
    --claude) CLAUDE=1 ;;
    --prepare) PREPARE=1 ;;
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
claude -p "Reply in text only and write no files: write a 1000-line Python program one function at a time; before each function print the line COUNT n" \
  --tools "" --effort low --settings "{\"alwaysThinkingEnabled\":false}" --output-format stream-json --verbose --include-partial-messages 2>/root/task/err.fifo | stamp > /root/task.jsonl
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
# TASKREAD: the task's stream, whole, then the end of the task. The echo
# ends a last line the task had only half written. RESUME lines give the
# wall-clock time of each resume the guest's hvi-time driver logged: the
# kernel log stamps it on a clock that stops while the guest is paused, as
# CLOCK_MONOTONIC does, so the wall clock of that moment is now on the wall
# clock minus the time since then on the monotonic one.
TASKREAD='cat /root/task.jsonl; echo; echo "TASKPROC $(pgrep -c -x claude)"; cat /root/task.exit 2>/dev/null
dmesg | python3 -c "
import re, sys, time
off = time.time() - time.monotonic()
for l in sys.stdin:
    m = re.match(r\"^\\[ *([0-9.]+)\\] hvi-time: generation ([0-9]+): \", l)
    if m:
        print(\"RESUME %s %.6f\" % (m.group(2), float(m.group(1)) + off))
"'

# TASKJUDGE BEFORE AFTER1 AFTER2: the --claude check, from three reads of
# the task's stream (TASKREAD's output): right before the move, on DST, and
# on DST once the task ended (or 600 s passed). TASKJUDGE count READ prints
# how many COUNT lines one read has and whether the task exited, for the
# waits. Each line of the stream is a stamp and a stream-json
# event. "After the move" is a stamp later than the last one the first read
# has, which takes in the second or so of brig's checks before the pause. A
# read on DST goes through ssh with a terminal, so its lines end in CR LF
# and brig's notices come first: a line counts as an event only when it is
# a stamp and a JSON object. The text is put back together from the
# text_delta events alone, or from the assistant messages when the stream
# has none. A text block can end with no newline, and a task that writes
# its program through tools sends a short block between tool calls, so the
# end of each block and message ends a line: COUNT n at the start of the
# next one is then still at the start of a line. A COUNT n counts only once
# a byte other than a digit follows it, so a COUNT 1 cut off on its way to
# COUNT 12 is not read, and markdown around it (**COUNT 3**) is allowed. It
# prints PASS and FAIL lines.
TASKJUDGE='
import json, re, sys

def read(path):
    events, proc, exits = [], None, []
    global resumes
    for raw in open(path, encoding="utf-8", errors="replace"):
        raw = raw.strip()
        m = re.match(r"^TASKPROC ([0-9]+)$", raw)
        if m:
            proc = int(m.group(1))
            continue
        m = re.match(r"^RESUME ([0-9]+) ([0-9.]+)$", raw)
        if m:
            resumes[int(m.group(1))] = float(m.group(2))
            continue
        m = re.match(r"^[0-9.]+ EXIT (-?[0-9]+)$", raw)
        if m:
            exits.append(int(m.group(1)))
            continue
        m = re.match(r"^([0-9]+[.][0-9]+) ([{].*)$", raw)
        if not m:
            continue
        try:
            e = json.loads(m.group(2))
        except ValueError:
            continue
        if isinstance(e, dict):
            events.append((float(m.group(1)), m.group(2), e))
    return events, proc, exits

def text(events):
    deltas, whole, block, some = [], [], None, False
    for _, _, e in events:
        if e.get("type") == "stream_event":
            ev = e.get("event") or {}
            d = ev.get("delta") or {}
            kind = ev.get("type")
            if kind in ("content_block_start", "content_block_stop", "message_start", "message_stop"):
                deltas.append("\n")
                block = None
            elif kind == "content_block_delta" and d.get("type") == "text_delta":
                if block is not None and ev.get("index") != block:
                    deltas.append("\n")
                block = ev.get("index")
                deltas.append(d.get("text", ""))
                some = True
        elif e.get("type") == "assistant":
            for c in (e.get("message") or {}).get("content") or []:
                if isinstance(c, dict) and c.get("type") == "text":
                    whole.append(c.get("text", "") + "\n")
    return "".join(deltas) if some else "".join(whole)

def counts(events):
    return [int(n) for n in re.findall(r"(?m)^\W*COUNT ([0-9]+)(?=[^0-9])", text(events))]

def errors(events):
    out = []
    for _, body, e in events:
        if "api_error" in body or e.get("is_error") is True or e.get("type") == "error" or (
                e.get("type") == "result" and e.get("subtype") not in (None, "success")):
            out.append(body[:160])
    return out

def say(ok, what):
    print(("PASS " if ok else "FAIL ") + what)

def info(ok, what):
    print("INFO " + ("yes: " if ok else "no: ") + what)

resumes = {}

if sys.argv[1] == "count":
    events, _, exits = read(sys.argv[2])
    print(len(counts(events)), "exited" if exits else "running")
    sys.exit(0)

reads = [read(p) for p in sys.argv[1:4]]
(b, _, bx), (a1, _, x1), (a2, proc, x2) = reads
cb, c1, c2 = counts(b), counts(a1), counts(a2)
say(len(cb) >= 2, "the task had written %d COUNT lines before the move, at least 2" % len(cb))
say(not bx, "the task was running when the guest paused" + (" (it had exited %s)" % bx if bx else ""))
paused = max([t for t, _, _ in b] or [0.0])
# The stall: from the resume of this move, the last one the guest logged,
# to the start of the response Claude Code asks for again: the first
# message_start after it, or the first content delta when the stream has
# none. The message_stop that marks a cut-off answer as abandoned comes as
# soon as the connection fails, before any new connection, so it does not
# count.
resume = resumes[max(resumes)] if resumes else None
def kind(e):
    return (e.get("event") or {}).get("type") if e.get("type") == "stream_event" else None
after = [(t, body, e) for t, body, e in a2 if resume is not None and t > resume and "abandoned" not in body]
back = [t for t, _, e in after if kind(e) == "message_start"] or \
    [t for t, _, e in after if kind(e) == "content_block_delta"]
if resume is None:
    say(False, "the stream came back on DST within 5 s of the resume: the guest logged no resume")
elif not back:
    say(False, "the stream came back on DST within 5 s of the resume: no API stream event after it")
else:
    say(back[0] - resume < 5, "the stream came back on DST %.2f s after the guest resumed (the stall), under 5 s" % (back[0] - resume))
say(len(c2) > len(cb), "the task wrote %d COUNT lines after the move (%d before, %d now)" % (
    len(c2) - len(cb), len(cb), len(c2)))
result = [e for _, _, e in a2 if e.get("type") == "result"]
done = x2 == [0] and bool(result) and result[-1].get("subtype") == "success" and result[-1].get("is_error") is not True
say(done, "the turn completed: exit %s, result %s" % (
    x2[-1] if x2 else "none yet (%s claude running)" % proc,
    (result[-1].get("subtype"), "error" if result[-1].get("is_error") else "ok") if result else "none"))
info(c1[:len(cb)] == cb and c2[:len(c1)] == c1,
    "the COUNT lines after the move go on from the %d before it, with no restart" % len(cb))
info(c2 == list(range(1, len(c2) + 1)), "the COUNT lines run 1 to %d with no gap or repeat" % len(c2))
# Claude Code reports an error when the guest agent closes the old
# connection, between the resume and the reconnect. An error event after
# the reconnect, or any when there was no reconnect to time, fails.
late = [ev for ev in a2 if ev[0] > paused]
window = [ev for ev in late if back and resume is not None and resume < ev[0] < back[0]]
late = errors([ev for ev in late if ev not in window])
say(not late, "no error event in the stream after the move, but for the reconnect" + ("" if not late else ": " + late[0]))
excused = errors(window)
if excused:
    print("INFO %d error event(s) between the resume and the reconnect, the first: %s" % (len(excused), excused[0]))
# The longest silence of the stream that takes in the move: from the last event
# before the move to the first after it, or a later gap.
stamps = sorted({t for t, _, _ in a2})
gaps = [(n - t, t) for t, n in zip(stamps, stamps[1:]) if n > paused]
if gaps:
    g, t = max(gaps)
    print("INFO the longest silence of the stream from the move on: %.1f s, after the event at %.3f (the last before the move: %.3f)" % (g, t, paused))
abandoned = sum(1 for _, body, _ in a2 if "abandoned_blocks" in body)
if abandoned:
    print("INFO Claude Code gave up on %d message cut off mid-stream and asked again" % abandoned)
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
if [ "$PREPARE" = 1 ]; then
  say "prepare $DST"
  t0=$(now); brig teleport "$REF" --to "$DST" --prepare >"$OUT/prepare.log" 2>&1; rc=$?; t1=$(now)
  cat "$OUT/prepare.log"
  [ "$rc" = 0 ] && grep -q 'base [0-9a-f]*: \(held\|sent\)' "$OUT/prepare.log" \
    && pass "brig teleport --prepare: exit 0 in $(awk "BEGIN { printf \"%.1f\", $t1 - $t0 }") s, $DST holds the base" \
    || fail "brig teleport --prepare: exit $rc, $DST does not say it holds the base"
fi
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
  # Wait, up to 60 s, until the stream has 2 COUNT lines: a move before the
  # first one could not tell a restart from a task that goes on.
  for _ in $(seq 30); do
    guest "$TASKREAD" >"$OUT/task.0" 2>/dev/null
    n=$(python3 -c "$TASKJUDGE" count "$OUT/task.0" | cut -d' ' -f1)
    [ "${n:-0}" -ge 2 ] && break
    sleep 2
  done
  echo "COUNT lines before the move: ${n:-0}"
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
  n1=$(python3 -c "$TASKJUDGE" count "$OUT/task.1" | cut -d' ' -f1)
  # The second look: once the task ended, or 600 s passed.
  t0=$(now)
  for _ in $(seq 120); do
    sleep 5
    guest "$TASKREAD" >"$OUT/task.2" 2>/dev/null
    read -r n2 st < <(python3 -c "$TASKJUDGE" count "$OUT/task.2")
    [ "$st" = exited ] && break
  done
  echo "the second look, $(awk "BEGIN { printf \"%.0f\", $(now) - $t0 }") s after the first: ${n1:-0} -> ${n2:-0} COUNT lines, task $st"
  while IFS= read -r l; do
    case $l in
      "PASS "*) pass "${l#PASS }" ;;
      "INFO "*) echo "${l#INFO }" ;;
      *) fail "${l#FAIL }" ;;
    esac
  done < <(python3 -c "$TASKJUDGE" "$OUT/task.0" "$OUT/task.1" "$OUT/task.2" 2>&1 || echo "FAIL the task check did not run")
fi

say "numbers"
# The move's numbers, from hull's records here and on DST, as in
# script/teleport-demo.sh, and the guest's own count of the pause: the rise
# in the downtime total its hvi-time driver logs at each restore, a total
# since boot.
dtotal() { tr -d '\r' | grep -o 'downtime total [0-9]*' | tail -1 | awk '{ print $3 }'; }
python3 - "${BRIG_HULL_STORE_DIR:-$HOME/.hull/store}" "$VM" "$TX" "$OUT/move.log" \
  "$(dtotal <<<"$BEFORE")" "$(dtotal <<<"$AFTER")" <<'PY'
import json, re, subprocess, sys, time
from datetime import datetime
store, vm, txid, log, dt0, dt1 = sys.argv[1:]
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
tm = src.get("timingsMs") or {}
ms = lambda k: f"{tm[k] / 1000:.2f} s" if k in tm else "none"
print(f"clock offset {peer} minus here: {off * 1000:+.1f} ms")
print(f"brig preflight (checks, probe, receive, dry run): {moving - t0:.2f} s")
print(f"hull: transport {src.get('transport', 'bundle')}, reserve {h['reserved'] - h['preflight']:.2f} s, "
      f"fence {ms('fence')}, transfer {tr['seconds']:.2f} s, restore paused {ms('prepare')}, commit {ms('commit')}")
print(f"transfer: {tr['bytes']} bytes ({tr['bytes'] / 2**20:.1f} MiB) in {tr['seconds']:.2f} s, "
      f"{tr['bytesPerSec'] / 2**20:.2f} MiB/s")
st = src.get("stream")
if st:
    # A streamed move: the rounds as hull relayed them, and hull's estimate
    # of the pause, from hvi's pause to the destination's resume.
    print(f"stream {st.get('mode')}: disk {st.get('diskBytes', 0)} bytes as {st.get('diskWireBytes', 0)} on the wire, "
          f"RAM {st.get('ramBytes', 0)}, state {st.get('stateBytes', 0)}; stop {st.get('stopReason')}, "
          f"hvi paused {st.get('hviPausedMs')} ms, sealed {st.get('sealedMs')} ms after the pause, "
          f"hull's downtime estimate {st.get('downtimeMs')} ms")
    for r in st.get("rounds") or []:
        if r.get("kind") != "state":
            print(f"  {r['name']}: {r['bytes']} bytes, {r['wireBytes']} on the wire, {r['seconds']:.2f} s"
                  + (f", {r['blocks']} blocks" if r.get("blocks") is not None else "")
                  + (", taken paused" if r.get("final") else ""))
else:
    print(f"disk data {src['disk']['dataBytes']} of {src['disk']['size']} bytes")
    print(f"downtime, reserved here to running there: {d['running'] - h['reserved']:.2f} s")
if "fenced" in h and "running" in d:
    print(f"fenced here to running there: {d['running'] - h['fenced']:.2f} s")
if dt1:
    print(f"the guest's hvi-time downtime total rose by {(int(dt1) - int(dt0 or 0)) / 1e6:.1f} ms")
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
