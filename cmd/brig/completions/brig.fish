# brig completion for fish. Printed by `brig completion fish`.
#
# This script holds no brig vocabulary. It collects the words left of the
# cursor, asks brig what may stand there, and renders the reply, so verbs, refs
# and flags come from the installed binary rather than from this file.
#
# The reply is a directive line followed by the candidates, one per line.

# __brig_ask sends the line to brig and prints the reply.
#
# Not memoized. The reply also depends on state the command line does not
# reflect -- the session index, the profile and policy directories -- and a
# cache keyed on the line cannot detect a change in any of them, so a session
# created after the first TAB would never appear again in that shell. bash and
# zsh re-ask on every keystroke; the cost is one exec that reads two files.
function __brig_ask
    set -l tokens (commandline -opc)
    set -l cur (commandline -ct)
    # Drop the command name. The current word is passed separately, because an
    # empty final argument is what asks "what comes next".
    if set -q tokens[1]
        set -e tokens[1]
    end
    # stderr is discarded: brig writes none here, and anything from elsewhere
    # would print over the line being typed.
    brig __complete -- $tokens $cur 2>/dev/null
end

# __brig_says reports whether the reply carries this directive.
function __brig_says
    set -l out (__brig_ask)
    # No first line means brig could not be run. Return false rather than let
    # `test` print its own error over the prompt.
    set -q out[1]; or return 1
    test "$out[1]" = "$argv[1]"
end

function __brig_candidates
    set -l out (__brig_ask)
    if set -q out[2]
        printf '%s\n' $out[2..-1]
    end
end

# -f on the name and directory cases, so fish adds nothing of its own: inside
# the agent's argv, filenames are worse than silence. -F on the path case hands
# the slot to fish's file completion.
complete -c brig -f
complete -c brig -f -n '__brig_says :names' -a '(__brig_candidates)'
complete -c brig -f -n '__brig_says :dirs' -a '(__fish_complete_directories (commandline -ct))'
complete -c brig -F -n '__brig_says :files'
