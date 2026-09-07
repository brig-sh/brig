# brig completion for bash. Printed by `brig completion bash`.
#
# This script knows none of brig's vocabulary. It collects the words left of
# the cursor, asks brig what may stand there, and renders the answer -- so the
# verbs, the refs and the flags come from the binary that is installed rather
# than from whenever this file was written.
#
# The answer is a directive line and then the candidates, one per line.

_brig_completion() {
	local cur pre out directive
	local -a words reply

	# The words to send: everything after `brig` and left of the cursor, then
	# the word under the cursor -- passed explicitly because it is empty when
	# the cursor sits on fresh whitespace, and an empty last argument is what
	# tells brig it is being asked "what can come next" rather than "finish
	# this token".
	words=("${COMP_WORDS[@]:1:COMP_CWORD-1}")
	cur="${COMP_WORDS[COMP_CWORD]}"

	COMPREPLY=()
	# stderr is dropped rather than shown: brig writes none on this path, and a
	# line from anywhere else would land in the middle of what is being typed.
	out="$("${COMP_WORDS[0]}" __complete -- "${words[@]}" "$cur" 2>/dev/null)" || return
	directive="${out%%$'\n'*}"

	case "$directive" in
	:names)
		# Everything after the directive line, split on newlines only, so a
		# candidate carrying a space stays one candidate.
		local IFS=$'\n'
		reply=(${out#*$'\n'})
		COMPREPLY=("${reply[@]}")
		;;
	:dirs)
		# Directory names, from the shell rather than from brig: the project
		# slot is brig's, the filesystem is not.
		local IFS=$'\n'
		COMPREPLY=($(compgen -d -- "$cur"))
		compopt -o filenames 2>/dev/null
		;;
	:files)
		local IFS=$'\n'
		COMPREPLY=($(compgen -f -- "$cur"))
		compopt -o filenames 2>/dev/null
		;;
	*)
		# :none, or an answer this script does not know. Offer nothing: right
		# of the ref the words are the agent's, and filenames are a worse guess
		# than silence.
		;;
	esac
}

complete -F _brig_completion brig
