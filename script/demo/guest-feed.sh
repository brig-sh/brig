#!/usr/bin/env bash
# The guest's own feed for the recorded teleport demo
# (script/demo/teleport-pi-pair.tape).
#
#   guest-feed.sh start [REF]   start the writer inside the guest
#   guest-feed.sh show  [REF]   print the feed, and reattach after a move
#
# REF defaults to ubuntu. Run it on the host the session started on, with
# brig's settings in the environment (the demo's env.sh).
#
# start gives the guest a hostname (it boots with none) and starts a loop
# in it that appends one line to /tmp/feed five times a second: the tick,
# the guest's hostname, the first 8 characters of the kernel's boot_id, and
# the downtime total the guest's hvi-time driver keeps across restores, read
# from /sys/kernel/hvi_time/snapshot. The tick is a shell variable in the
# guest, so it goes on from where it stopped only if the guest's memory did.
# A boot would make a new boot_id.
#
# show runs `brig sh REF -c 'tail -f /tmp/feed'`. A move ends that session:
# it is a vsock connection to the guest agent, and the guest leaves the host
# it was opened on. show then prints one "reconnecting" line, runs brig sh
# again until it attaches, and asks tail for the line after the last tick it
# printed, so no tick is missing and none is printed twice. Line N of the
# feed is tick N. The first attach shows the last 30 lines.
#
# GATE, if set, names a file that holds "go" or "hold". show reattaches only
# while it holds "go" (or is missing). The demo's launcher writes "hold"
# before `brig rm` on the old host: between that rm and the move back, this
# host has no record of the session, and a brig sh there would boot a new,
# empty one under the same name.
set -uo pipefail

mode=${1:?usage: guest-feed.sh start|show [REF]}
REF=${2:-ubuntu}

case $mode in
start)
	# The writer, as the guest's shell gets it. The pause at the end lets
	# setsid take the writer out of the exec's process group before the
	# exec ends: the guest agent kills that group when the exec returns.
	read -r -d '' script <<'GUEST'
hostname "brig-$1"
rm -f /tmp/feed
nohup setsid bash -c '
  i=0
  while :; do
    i=$((i+1))
    read -r host < /proc/sys/kernel/hostname
    read -r boot < /proc/sys/kernel/random/boot_id
    read -r snap < /sys/kernel/hvi_time/snapshot
    d=${snap#*downtime_total_ns=}; d=${d%% *}
    printf "tick %05d %s boot %.8s down %d.%d ms\n" "$i" "$host" "$boot" $((d/1000000)) $((d/100000%10)) >> /tmp/feed
    sleep 0.2
  done' >/dev/null 2>&1 </dev/null &
sleep 0.5
GUEST
	brig -q sh "$REF" -c "set -- $REF; $script"
	;;
show)
	last=0
	seen=$(mktemp)
	trap 'rm -f "$seen"' EXIT
	while :; do
		# The first attach starts at the last 30 lines; a reattach at the
		# line after the last tick printed. Each line goes to the screen,
		# and its tick to $seen.
		from=30
		[ "$last" -gt 0 ] && from=+$((last + 1))
		brig -q sh "$REF" -c "tail -n $from -f /tmp/feed" </dev/null 2>/dev/null |
			while IFS= read -r line; do
				printf '%s\n' "$line"
				t=${line#tick }
				printf '%s\n' "$((10#${t%% *}))" >"$seen"
			done
		[ -s "$seen" ] && last=$(<"$seen")
		printf '\033[2m  ... reconnecting\033[0m\n'
		# Retry quietly until brig sh attaches again: during the move it
		# refuses, as the session is being moved.
		while :; do
			sleep 0.3
			if [ -n "${GATE:-}" ] && [ -f "$GATE" ] && [ "$(<"$GATE")" != go ]; then
				continue
			fi
			brig -q sh "$REF" -c "test -s /tmp/feed" </dev/null >/dev/null 2>&1 && break
		done
	done
	;;
*)
	echo "usage: guest-feed.sh start|show [REF]" >&2
	exit 2
	;;
esac
