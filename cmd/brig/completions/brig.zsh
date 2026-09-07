#compdef brig
# brig completion for zsh. Printed by `brig completion zsh`.
#
# This script knows none of brig's vocabulary. It collects the words left of
# the cursor, asks brig what may stand there, and renders the answer -- so the
# verbs, the refs and the flags come from the binary that is installed rather
# than from whenever this file was written.
#
# The answer is a directive line and then the candidates, one per line.

_brig_completion() {
	local -a out reply
	local directive cur

	# words[1] is `brig` itself; CURRENT indexes the word under the cursor,
	# which is empty when the cursor sits on fresh whitespace. It is passed
	# explicitly, because an empty last argument is what tells brig it is being
	# asked "what can come next" rather than "finish this token".
	cur="${words[CURRENT]}"
	# stderr is dropped rather than shown: brig writes none on this path, and a
	# line from anywhere else would land in the middle of what is being typed.
	out=("${(@f)$(${words[1]} __complete -- "${(@)words[2,CURRENT-1]}" "$cur" 2>/dev/null)}")
	directive="${out[1]}"
	shift out

	case "$directive" in
	:names)
		# A ref that can continue past the agent -- `claude` and
		# `claude@refactor` both -- comes back as two candidates, so zsh inserts
		# the common prefix and appends nothing of its own. A unique candidate
		# is a finished word and gets the usual space.
		compadd -a out
		;;
	:dirs)
		# Directories from zsh rather than from brig: the project slot is
		# brig's, the filesystem is not.
		_path_files -/
		;;
	:files)
		_path_files -f
		;;
	*)
		# :none, or an answer this script does not know. Offer nothing: right of
		# the ref the words are the agent's, and filenames are a worse guess
		# than silence.
		return 1
		;;
	esac
}

_brig_completion "$@"
