# brig completion for fish. Printed by `brig completion fish`.
#
# This script knows none of brig's vocabulary. It collects the words left of
# the cursor, asks brig what may stand there, and renders the answer -- so the
# verbs, the flags and the refs come from the binary that is installed rather
# than from whenever this file was written.
#
# The answer is a directive line and then the candidates, one per line.

# __brig_ask puts the line to brig and prints what it said.
#
# Asked again for every completion rather than memoized. What brig answers
# depends on state outside the command line -- the sessions that exist, the
# profiles and policies on disk -- and a cache keyed on the line has no way to
# learn that any of it changed, so a session started after the first TAB would
# never be offered again for the life of the shell. bash and zsh re-ask on
# every keystroke; the cost is one exec of a command that reads two files.
function __brig_ask
    set -l tokens (commandline -opc)
    set -l cur (commandline -ct)
    # The command itself is dropped; the word under the cursor is passed
    # explicitly, because an empty last argument is what tells brig it is being
    # asked "what can come next" rather than "finish this token".
    if set -q tokens[1]
        set -e tokens[1]
    end
    # stderr is dropped rather than shown: brig writes none on this path, and a
    # line from anywhere else would land in the middle of what is being typed.
    brig __complete -- $tokens $cur 2>/dev/null
end

# __brig_says reports whether the answer carries one directive.
function __brig_says
    set -l out (__brig_ask)
    # An answer with no first line means brig could not be reached at all.
    # Reported as "not this directive" rather than left to `test`, which would
    # otherwise print its own complaint across the prompt.
    set -q out[1]; or return 1
    test "$out[1]" = "$argv[1]"
end

function __brig_candidates
    set -l out (__brig_ask)
    if set -q out[2]
        printf '%s\n' $out[2..-1]
    end
end

# -f on the name and directory cases so fish offers nothing of its own: right
# of the ref the words are the agent's, and filenames are a worse guess than
# silence. -F on the path case hands the slot back to fish's own completion.
complete -c brig -f
complete -c brig -f -n '__brig_says :names' -a '(__brig_candidates)'
complete -c brig -f -n '__brig_says :dirs' -a '(__fish_complete_directories (commandline -ct))'
complete -c brig -F -n '__brig_says :files'
