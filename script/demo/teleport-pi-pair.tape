# A teleportable session moves from rp1 to rp2 and back while the guest's
# own feed keeps printing. Render from the repository root:
#
#   vhs script/demo/teleport-pi-pair.tape
#
# The terminal is an ssh session into rp1, which runs
# script/demo/teleport-panes.sh from ~/vhsdemo/demo. Before rendering, on
# rp1, with ~/vhsdemo/env.sh sourced and rp2 holding no copy of the
# session, set it up as that script's header says:
#
#   brig run --teleportable ubuntu -d
#   sync; sleep 20         # let the boot's disk writes settle
#   brig teleport ubuntu --to pmoust@rp2 --prepare
#   hull --store-dir "$BRIG_HULL_STORE_DIR" migrate-spare-base --sha256 <base digest>
#   sync; ssh rp2 sync
#   script/demo/guest-feed.sh start
#
# Without the spare copy on rp1, the move back copies the base while the
# guest is paused: 22.9 s of downtime in one run. Both moves are typed on
# rp1: the brig there hands the move back to the brig on rp2, which
# reaches rp1 with the settings env.sh gives it through
# BRIG_TELEPORT_REMOTE_BRIG (~/vhsdemo/bin/brig-env, a script that sources
# env.sh and runs brig). See docs/teleport.md, "Watch it".

Output docs/media/teleport-pi-pair.gif
Output docs/media/teleport-pi-pair.mp4

Require ssh

Set Shell "bash"
Set FontSize 16
Set Width 1400
Set Height 800
Set Padding 14
Set WindowBar ""
Set CursorBlink false
Set Framerate 25
Set Theme { "name": "brig", "background": "#0b0c0d", "foreground": "#fbfbf9", "cursor": "#5fffd2", "selection": "#2a2d2f", "black": "#1b1d1f", "red": "#ff6b6b", "green": "#5fffd2", "yellow": "#ffd479", "blue": "#7aa7ff", "magenta": "#d49cff", "cyan": "#00a693", "white": "#e6e6e3", "brightBlack": "#8a8f93", "brightRed": "#ff8787", "brightGreen": "#8affe0", "brightYellow": "#ffe0a3", "brightBlue": "#a3c2ff", "brightMagenta": "#e3bfff", "brightCyan": "#33c7b3", "brightWhite": "#fbfbf9" }

# The login stays off camera; the viewer sees the panes.
Hide
Type "ssh -t rp1 vhsdemo/demo/teleport-panes.sh"
Enter
Sleep 2.5s
Show

# The moves take a few seconds each, a little more or less on each run:
# wait for the `brig ls` header, then hold the last frame for 5 s.
Wait+Screen@120s /SANDBOX +STATE/
Sleep 5s
