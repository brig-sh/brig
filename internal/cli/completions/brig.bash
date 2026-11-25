# brig completion for bash. Printed by `brig completion bash`.
#
# This script holds no brig vocabulary. It collects the words left of the
# cursor, asks brig what may stand there, and renders the reply, so verbs, refs
# and flags come from the installed binary rather than from this file.
#
# The reply is a directive line followed by the candidates, one per line.

_brig_completion() {
	local cur pre out directive
	local -a words reply

	# Everything after `brig` and left of the cursor, then the current word.
	# The current word is passed separately because it is empty when the cursor
	# is on fresh whitespace, and an empty final argument is what asks "what
	# comes next" rather than "finish this token".
	words=("${COMP_WORDS[@]:1:COMP_CWORD-1}")
	cur="${COMP_WORDS[COMP_CWORD]-}"

	COMPREPLY=()
	# stderr is discarded: brig writes none here, and anything from elsewhere
	# would print over the line being typed.
	#
	# ${words[@]+"${words[@]}"} rather than "${words[@]}": before bash 4.4 an
	# empty array expanded under `set -u` is an unbound variable. macOS ships
	# bash 3.2, so a user with `set -u` got an error on the first word.
	out="$("${COMP_WORDS[0]}" __complete -- ${words[@]+"${words[@]}"} "$cur" 2>/dev/null)" || return
	directive="${out%%$'\n'*}"

	case "$directive" in
	:names)
		# Split on newlines only, so a candidate containing a space stays one
		# candidate.
		local IFS=$'\n'
		reply=(${out#*$'\n'})
		COMPREPLY=(${reply[@]+"${reply[@]}"})
		;;
	:dirs)
		# Directories come from the shell. The project slot is brig's; the
		# filesystem is not.
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
		# :none, or a directive this script does not know. Offer nothing:
		# inside the agent's argv, filenames are worse than silence.
		;;
	esac
}

complete -F _brig_completion brig
