#!/usr/bin/env bash
# The three panes of the recorded Mac-to-Pi teleport demo
# (script/demo/teleport-mac-to-pi.tape). Run it on the Mac, in a terminal;
# it builds a tmux session and attaches to it.
#
#   left, 55%:          the operator. Each command is typed out, then run
#                       as typed.
#   right top, 45%:     the guest's own feed, guest-feed.sh show.
#   right bottom:       a Claude Code turn in the guest, its stream-json
#                       read through claude-feed.jq.
#
#   teleport-mac-panes.sh turn    start the Claude Code turn in the guest
#   teleport-mac-panes.sh         build the panes and attach
#
# Before it runs, the session is up on the Mac, the destination is
# prepared and the feed is started:
#   brig run --teleportable claude -d
#   brig teleport claude --to pmoust@rp2 --prepare
#   script/demo/guest-feed.sh start claude
# The tape then runs `teleport-mac-panes.sh turn` off camera, waits, and
# runs this script with no argument. The turn is `claude -p` with
# --output-format stream-json, its stream written to /root/turn.jsonl in
# the guest and its errors to /root/turn.err. It runs on in the guest
# across the move: the pane reads the file, not the process. Its one long
# step, `vmstat 1 30`, is there so that the guest pauses while Claude Code
# waits on a command rather than on the API: an API stream that is open
# at the pause goes silent on the destination for about 60 s, until
# Claude Code gives up on it and asks again (docs/teleport.md, "Claude
# Code session, Mac to Pi"). ENABLE_CLAUDEAI_MCP_SERVERS=false and
# --strict-mcp-config are meant to leave out the MCP servers of the account
# Claude Code is logged in with: the turn needs none.
#
# Settings, from the environment:
#   DEMO_ENV  brig's settings, sourced first (default ~/vhsdemo/env.sh)
#   REF       the session (default claude)
#   DST       where it moves (default pmoust@rp2)
#   PROMPT    the turn's prompt
#   HOLD      seconds the panes stay up after the last command (default 60)
set -u
DIR=$(cd "$(dirname "$0")" && pwd)
DEMO_ENV=${DEMO_ENV:-$HOME/vhsdemo/env.sh}
REF=${REF:-claude} DST=${DST:-pmoust@rp2}
PROMPT=${PROMPT:-'Run vmstat 1 30 with Bash, on its own, to watch this machine for 30 seconds. Then read /etc/os-release and /etc/hostname with Read. Reply with only 5 bullets about this machine, at most 10 words each.'}
HOLD=${HOLD:-60}
SESSION=teleport-mac-demo
export DEMO_ENV REF DST PROMPT HOLD

# type_run CMD: print the prompt, type CMD at about 30 ms a character, and
# run it.
type_run() {
	local cmd=$1 i
	printf '\033[1;32mmac\033[0;36m $\033[0m '
	for ((i = 0; i < ${#cmd}; i++)); do
		printf '%s' "${cmd:i:1}"
		sleep 0.03
	done
	sleep 0.4
	printf '\n'
	eval "$cmd"
}

operator() {
	sleep 3
	type_run "brig teleport $REF --to $DST"
	sleep 3
	type_run "brig teleport $REF --status"
	sleep 2.5
	type_run "brig sh $REF -c 'hostname; uptime; cat /proc/sys/kernel/random/boot_id'"
	# Hold the last frame; the recording stops first.
	sleep "$HOLD"
	tmux kill-session -t "$SESSION"
}

# claude_pane: the turn's stream, one line per event. A move ends the brig
# sh session, as it does the feed's; the pane then reattaches and asks tail
# for the line after the last event it printed. Only lines that start with
# "{" are events: a notice from brig is not counted.
claude_pane() {
	local n=0 seen
	seen=$(mktemp)
	trap 'rm -f "$seen"' EXIT
	# Two execs that start at once on the Mac can both write the session's
	# credentials file into the guest, and one then fails ("could not
	# create .claude/.credentials.json in the sandbox"): start after the
	# feed's.
	sleep 1
	while :; do
		brig -q sh "$REF" -c "tail -n +$((n + 1)) -f /root/turn.jsonl" </dev/null 2>/dev/null |
			while IFS= read -r line; do
				line=${line%$'\r'}
				case $line in
				'{'*)
					n=$((n + 1))
					printf '%s\n' "$n" >"$seen"
					printf '%s\n' "$line"
					;;
				esac
			done | jq -R -r --unbuffered -f "$DIR/claude-feed.jq"
		[ -s "$seen" ] && n=$(<"$seen")
		printf '\033[2m  ... reconnecting\033[0m\n'
		while :; do
			sleep 0.3
			brig -q sh "$REF" -c "test -e /root/turn.jsonl" </dev/null >/dev/null 2>&1 && break
		done
	done
}

case ${1:-} in
turn)
	# shellcheck source=/dev/null
	. "$DEMO_ENV"
	# The pause at the end lets setsid take the turn out of the exec's
	# process group before the exec ends, as in guest-feed.sh start.
	read -r -d '' script <<'GUEST'
rm -f /root/turn.jsonl /root/turn.err
cd /etc
ENABLE_CLAUDEAI_MCP_SERVERS=false nohup setsid claude -p "$1" \
  --output-format stream-json --verbose --max-turns 8 --effort low \
  --strict-mcp-config --allowedTools Read 'Bash(vmstat:*)' \
  >/root/turn.jsonl 2>/root/turn.err </dev/null &
sleep 0.5
GUEST
	brig -q sh "$REF" -c "set -- $(printf '%q' "$PROMPT"); $script"
	exit
	;;
operator)
	# shellcheck source=/dev/null
	. "$DEMO_ENV"
	operator
	exit
	;;
feed)
	# shellcheck source=/dev/null
	. "$DEMO_ENV"
	exec "$DIR/guest-feed.sh" show "$REF"
	;;
claude)
	# shellcheck source=/dev/null
	. "$DEMO_ENV"
	claude_pane
	exit
	;;
esac

tmux kill-session -t "$SESSION" 2>/dev/null
tmux new-session -d -s "$SESSION" -x "$(tput cols)" -y "$(tput lines)" "$DIR/teleport-mac-panes.sh operator"
tmux split-window -h -l 45% -t "$SESSION" "$DIR/teleport-mac-panes.sh feed"
tmux split-window -v -l 62% -t "$SESSION:0.1" "$DIR/teleport-mac-panes.sh claude"
tmux set-option -t "$SESSION" status off
tmux set-option -t "$SESSION" pane-border-style 'fg=colour238'
tmux set-option -t "$SESSION" pane-active-border-style 'fg=colour238'
tmux set-option -t "$SESSION" pane-border-status top
tmux set-option -t "$SESSION" pane-border-format '#[fg=colour245] #{pane_title} '
tmux select-pane -t "$SESSION:0.0" -T "the operator, on the Mac"
tmux select-pane -t "$SESSION:0.1" -T "the guest's feed: brig sh $REF -c 'tail -f /tmp/feed'"
tmux select-pane -t "$SESSION:0.2" -T "Claude Code in the guest: its stream-json via claude-feed.jq"
tmux select-pane -t "$SESSION:0.0"
exec tmux attach -t "$SESSION"
