#compdef brig
# brig completion for zsh. Printed by `brig completion zsh`.
#
# This script holds no brig vocabulary. It collects the words left of the
# cursor, asks brig what may stand there, and renders the reply, so verbs, refs
# and flags come from the installed binary rather than from this file.
#
# The reply is a directive line followed by the candidates, one per line.

_brig_completion() {
	local -a out reply
	local directive cur

	# words[1] is `brig`; CURRENT indexes the current word, which is empty when
	# the cursor is on fresh whitespace. Passed separately, because an empty
	# final argument is what asks "what comes next".
	cur="${words[CURRENT]}"
	# stderr is discarded: brig writes none here, and anything from elsewhere
	# would print over the line being typed.
	out=("${(@f)$(${words[1]} __complete -- "${(@)words[2,CURRENT-1]}" "$cur" 2>/dev/null)}")
	directive="${out[1]}"
	shift out

	case "$directive" in
	:names)
		# No -S needed: a ref that can continue past the agent comes back as
		# two candidates (`claude` and `claude@refactor`), so zsh inserts the
		# common prefix and adds no space. A unique candidate is complete and
		# gets the usual space.
		compadd -a out
		;;
	:dirs)
		# Directories come from zsh. The project slot is brig's; the filesystem
		# is not.
		_path_files -/
		;;
	:files)
		_path_files -f
		;;
	*)
		# :none, or a directive this script does not know. Offer nothing:
		# inside the agent's argv, filenames are worse than silence.
		return 1
		;;
	esac
}

_brig_completion "$@"
