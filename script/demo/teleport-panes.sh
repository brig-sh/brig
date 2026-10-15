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
# The hull line makes, on this host, the spare copy of the session's base
# that a prepare makes on its destination, so the move back here takes it
# instead of copying the base while the guest is paused. <base> is the
# "base digest" --prepare prints. `brig teleport ubuntu --to pmoust@rp1
# --prepare` makes it too, once the session runs on rp2, and a hull that
# makes it after the first move commits needs neither.
#
# Settings, from the environment:
#   DEMO_ENV  brig's settings, sourced first (default ~/vhsdemo/env.sh)
#   REF       the session (default ubuntu)
#   DST       where it moves (default pmoust@rp2)
#   BACK      where it moves back to: this host, as DST reaches it
#             (default pmoust@rp1)
#   HOLD      seconds both panes stay up after the last command (default 60)
#
# The move back is typed here too. brig hands it to the brig on DST, as
# docs/teleport.md, "Moving back", says, after it removes the fenced copy
# the first move left here. The brig on DST reaches this host with its own
# settings: env.sh names a BRIG_TELEPORT_REMOTE_BRIG that sources env.sh
# there and runs brig.
set -u
DIR=$(cd "$(dirname "$0")" && pwd)
DEMO_ENV=${DEMO_ENV:-$HOME/vhsdemo/env.sh}
REF=${REF:-ubuntu} DST=${DST:-pmoust@rp2} BACK=${BACK:-pmoust@rp1}
HOLD=${HOLD:-60}
SESSION=teleport-demo
export DEMO_ENV REF DST BACK HOLD

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
	type_run "brig teleport $REF --to $BACK"
	sleep 4
	type_run "brig ls"
	# Hold the last frame; the recording stops first. The tape holds it
	# for 5 s after `brig ls` prints.
	sleep "$HOLD"
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

tmux kill-session -t "$SESSION" 2>/dev/null
tmux new-session -d -s "$SESSION" -x "$(tput cols)" -y "$(tput lines)" "$DIR/teleport-panes.sh operator"
tmux split-window -h -l 40% -t "$SESSION" "$DIR/teleport-panes.sh feed"
tmux set-option -t "$SESSION" status off
tmux set-option -t "$SESSION" pane-border-style 'fg=colour238'
tmux set-option -t "$SESSION" pane-active-border-style 'fg=colour238'
tmux select-pane -t "$SESSION:0.0"
exec tmux attach -t "$SESSION"
