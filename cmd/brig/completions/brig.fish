# brig completion for fish. Printed by `brig completion fish`.
#
# This script knows none of brig's vocabulary. It collects the words left of
# the cursor, asks brig what may stand there, and renders the answer -- so the
# verbs, the refs and the flags come from the binary that is installed rather
# than from whenever this file was written.
#
# The answer is a directive line and then the candidates, one per line.

# __brig_ask runs the engine once per command line and caches what it said.
#
# fish evaluates each `complete` condition below in turn, so without the cache
# one keystroke would run brig three times to learn the same answer.
function __brig_ask
	set -l line (commandline -cp)
	if test "$__brig_line" != "$line"
		set -l tokens (commandline -opc)
		set -l cur (commandline -ct)
		# The command itself is dropped; the word under the cursor is passed
		# explicitly, because an empty last argument is what tells brig it is
		# being asked "what can come next" rather than "finish this token".
		if set -q tokens[1]
			set -e tokens[1]
		end
		set -g __brig_line $line
		# stderr is dropped rather than shown: brig writes none on this path,
		# and a line from anywhere else would land in the middle of what is
		# being typed.
		set -g __brig_out (brig __complete -- $tokens $cur 2>/dev/null)
	end
end

# __brig_says reports whether the engine answered with one directive.
function __brig_says
	__brig_ask
	test "$__brig_out[1]" = "$argv[1]"
end

function __brig_candidates
	__brig_ask
	if set -q __brig_out[2]
		printf '%s\n' $__brig_out[2..-1]
	end
end

# -f on the name and directory cases so fish offers nothing of its own: right
# of the ref the words are the agent's, and filenames are a worse guess than
# silence. -F on the path case hands the slot back to fish's own completion.
complete -c brig -f
complete -c brig -f -n '__brig_says :names' -a '(__brig_candidates)'
complete -c brig -f -n '__brig_says :dirs' -a '(__fish_complete_directories (commandline -ct))'
complete -c brig -F -n '__brig_says :files'
