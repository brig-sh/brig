# A teleportable claude-code session moves from this Mac to rp2, a
# Raspberry Pi 5, while the guest's feed keeps printing and a Claude Code
# turn in the guest runs on. Render from the repository root, on the Mac:
#
#   DEMO_ENV=<brig's settings for this Mac> vhs script/demo/teleport-mac-to-pi.tape
#
# The terminal runs script/demo/teleport-mac-panes.sh. Before rendering,
# with DEMO_ENV sourced and rp2 holding no copy of the session, set it up
# as that script's header says:
#
#   brig run --teleportable claude -d
#   brig teleport claude --to pmoust@rp2 --prepare
#   script/demo/guest-feed.sh start claude
#
# Off camera, the tape starts the Claude Code turn in the guest
# (`teleport-mac-panes.sh turn`) and waits 4 s, so that the turn's
# `vmstat 1 30` has begun when the panes appear: the move then pauses the
# guest while Claude Code waits on that command, which ends on rp2, and
# the turn finishes there. See docs/teleport.md, "From a Mac to a Pi".

Output docs/media/teleport-mac-to-pi.gif
Output docs/media/teleport-mac-to-pi.mp4

Require tmux
Require jq

Set Shell "bash"
Set FontSize 16
Set Width 1400
Set Height 800
Set Padding 14
Set WindowBar ""
Set CursorBlink false
Set Framerate 25
Set Theme { "name": "brig", "background": "#0b0c0d", "foreground": "#fbfbf9", "cursor": "#5fffd2", "selection": "#2a2d2f", "black": "#1b1d1f", "red": "#ff6b6b", "green": "#5fffd2", "yellow": "#ffd479", "blue": "#7aa7ff", "magenta": "#d49cff", "cyan": "#00a693", "white": "#e6e6e3", "brightBlack": "#8a8f93", "brightRed": "#ff8787", "brightGreen": "#8affe0", "brightYellow": "#ffe0a3", "brightBlue": "#a3c2ff", "brightMagenta": "#e3bfff", "brightCyan": "#33c7b3", "brightWhite": "#fbfbf9" }

# The turn starts off camera; the viewer sees the panes.
Hide
Type "script/demo/teleport-mac-panes.sh turn && sleep 4 && clear && script/demo/teleport-mac-panes.sh"
Enter
Sleep 6.5s
Show

# The move and the two commands after it take about 20 s, and the turn
# ends about 33 s in, a little more or less on each run: wait for the
# boot_id the last command prints, then for the turn's result, then hold
# the last frame for 8 s.
Wait+Screen@45s /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/
Wait+Screen@40s /done: /
Sleep 8s
