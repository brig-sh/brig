#!/usr/bin/env bash
# The two panes of the recorded teleport demo
# (script/demo/teleport-pi-pair.tape). Run it on the host the session
# starts on, in a terminal; it builds a tmux session and attaches to it.
#
#   left, 60%:  the operator. Each command is typed out, then run as typed.
#   right, 40%: the guest's own feed, guest-feed.sh show.
#
# Before it runs, the session is up, both hosts are prepared and the feed
# is started:
#   brig run --teleportable ubuntu -d
#   sync; sleep 20         # let the boot's disk writes settle
#   brig teleport ubuntu --to pmoust@rp2 --prepare
#   hull --store-dir "$BRIG_HULL_STORE_DIR" migrate-spare-base --sha256 <base>
#   sync; ssh rp2 sync
#   script/demo/guest-feed.sh start
# The third line makes, on this host, the spare copy of the session's base
# that a prepare makes on its destination, so the move back here takes it
# instead of copying the base while the guest is paused. <base> is the
# "base digest" --prepare prints.
#
# Settings, from the environment:
#   DEMO_ENV  brig's settings, sourced first (default ~/vhsdemo/env.sh)
#   REF       the session (default ubuntu)
#   DST       where it moves (default pmoust@rp2)
#   BACK      where it moves back to, as DST names this host (default pmoust@rp1)
#   DST_SSH   the ssh name of DST for the move back (default rp2)
#   DST_ENV   brig's settings on DST, as a path there (default vhsdemo/env.sh)
#   HOLD      seconds both panes stay up after the last command (default 60)
#
# The move back goes from DST, as docs/teleport.md, "Moving back", says:
# `brig rm` here first removes the fenced copy the first move left, then
# brig on DST moves the session here.
set -u
DIR=$(cd "$(dirname "$0")" && pwd)
DEMO_ENV=${DEMO_ENV:-$HOME/vhsdemo/env.sh}
REF=${REF:-ubuntu} DST=${DST:-pmoust@rp2} BACK=${BACK:-pmoust@rp1}
DST_SSH=${DST_SSH:-rp2} DST_ENV=${DST_ENV:-vhsdemo/env.sh}
HOLD=${HOLD:-60}
SESSION=teleport-demo
# The feed pane reattaches only while this file says "go". See guest-feed.sh.
GATE=${GATE:-$(mktemp "${TMPDIR:-/tmp}/teleport-gate.XXXXXX")}
export DEMO_ENV REF DST BACK DST_SSH DST_ENV HOLD GATE

# type_run CMD: print the prompt, type CMD at about 30 ms a character, and
# run it.
type_run() {
	local cmd=$1 i
	printf '\033[1;32mrp1\033[0;36m $\033[0m '
	for ((i = 0; i < ${#cmd}; i++)); do
		printf '%s' "${cmd:i:1}"
		sleep 0.03
	done
	sleep 0.4
	printf '\n'
	eval "$cmd"
}

operator() {
	sleep 3.5
	type_run "brig teleport $REF --to $DST"
	sleep 5
	type_run "brig teleport $REF --status"
	sleep 2.5
	type_run "brig sh $REF -c 'hostname; uptime; cat /proc/sys/kernel/random/boot_id'"
	sleep 3.5
	echo hold >"$GATE"
	type_run "brig rm $REF"
	sleep 1.5
	type_run "ssh $DST_SSH '. $DST_ENV; brig teleport $REF --to $BACK'"
	echo go >"$GATE"
	sleep 4
	type_run "brig ls"
	# Hold the last frame; the recording stops first. The tape holds it
	# for 5 s after `brig ls` prints.
	sleep "$HOLD"
	rm -f "$GATE"
	tmux kill-session -t "$SESSION"
}

case ${1:-} in
operator)
	# shellcheck source=/dev/null
	. "$DEMO_ENV"
	operator
	exit
	;;
feed)
	# shellcheck source=/dev/null
	. "$DEMO_ENV"
	printf "\033[2m  the guest's feed: brig sh %s -c 'tail -f /tmp/feed'\033[0m\n" "$REF"
	exec "$DIR/guest-feed.sh" show "$REF"
	;;
esac

echo go >"$GATE"
tmux kill-session -t "$SESSION" 2>/dev/null
tmux new-session -d -s "$SESSION" -x "$(tput cols)" -y "$(tput lines)" "$DIR/teleport-panes.sh operator"
tmux split-window -h -l 40% -t "$SESSION" "$DIR/teleport-panes.sh feed"
tmux set-option -t "$SESSION" status off
tmux set-option -t "$SESSION" pane-border-style 'fg=colour238'
tmux set-option -t "$SESSION" pane-active-border-style 'fg=colour238'
tmux select-pane -t "$SESSION:0.0"
exec tmux attach -t "$SESSION"
