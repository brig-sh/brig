#!/usr/bin/env bash
# Refuse documentation that teaches a command spelling brig is about to remove.
#
# brig renamed most of its verbs during the 0.1 series. Every old spelling still
# works and prints one line naming its replacement, and all of them are
# scheduled for removal in 0.3. That grace period is exactly what makes the
# problem invisible: a doc teaching `brig profiles` is not broken today, so
# nothing fails and no reader complains, and it becomes wrong on the release
# that drops the alias. Issue #111 asks for this to be checked rather than
# reviewed by eye, so it does not rot the first time a doc is edited after 0.3.
#
# Only COMMANDS are checked, not prose. A retired spelling is reported when it
# appears inside inline code or in a fenced code block, because that is where a
# reader copies from. The sentence "six of the shipped brig profiles ask for
# hvi" uses "profiles" as an ordinary noun and is left alone, and so is prose
# about exec'ing into a container.
#
# Three files are exempt, because naming the retired spellings is their job:
# docs/migration.md holds the old-to-new mapping, docs/stability.md documents
# the deprecation window and quotes the notice, and a changelog is allowed to
# say what a release retired.
#
# Elsewhere, a single PROSE line can opt out with a `retired-ok` marker in an
# HTML comment, for a passage that names an old spelling to describe it rather
# than to teach it: explaining a limitation that only the retired spelling has,
# for instance. Keep these rare. A marker on a line that is really teaching the
# old spelling defeats the check. The marker does not work inside a fenced
# block, where it would become part of the code a reader copies.
#
# Short flags are scoped to the run line. `-n` is retired on `brig run`, where
# it became `<agent>@<label>`, and is current on `brig policy attach`, so the
# verb has to be part of the pattern.
#
# Usage: check-retired-spellings.sh [file...]
#        With no argument, every tracked Markdown file is checked.
#
# macOS ships bash 3.2, so there is no mapfile here and no associative array.

set -uo pipefail

is_exempt() {
	case "$1" in
	docs/migration.md | docs/stability.md | CHANGELOG.md) return 0 ;;
	esac
	return 1
}

patterns=(
	'brig (profiles|agents|template)\b|brig profile [a-z]'
	'brig policies\b'
	'brig (create|reset|env)\b'
	'brig (exec|shell)\b'
	'brig (import|export)\b'
	'brig agent (list|save|load)\b'
	'brig policy list\b'
	'brig secret (list|rm)\b'
	'brig (run|sh|create)\b.* (-t|-m|-n|-w)( |=|$)'
	'brig (run|sh|create)\b.* (--name|--workspace|--memory)( |=|$)'
)

names=(
	'brig profiles / brig agents / brig template / brig profile <verb> -> brig agent <verb>'
	'brig policies -> brig policy ls'
	'brig create / brig reset / brig env -> brig run -d / brig rm --all / brig info'
	'brig exec / brig shell -> brig sh'
	'brig import / brig export -> brig agent import / brig agent export'
	'brig agent list|save|load -> brig agent ls|export|import'
	'brig policy list -> brig policy ls'
	'brig secret list|rm -> brig secret ls|delete'
	'the run-line short flags -t -m -n -w -> --image --mem <agent>@<label> --home'
	'--name / --workspace / --memory on the run line -> <agent>@<label> / --home / --mem'
)

# Print "<line>:<command text>" for every place a reader could copy a command
# from: each inline-code span, and each line inside a fenced code block.
extract_commands() {
	awk '
		/^[[:space:]]*```/ { fence = !fence; next }
		/retired-ok/ { next }
		fence { print NR ":" $0; next }
		{
			line = $0
			while (match(line, /`[^`]+`/)) {
				span = substr(line, RSTART + 1, RLENGTH - 2)
				print NR ":" span
				line = substr(line, RSTART + RLENGTH)
			}
		}
	' "$1"
}

if [ "$#" -gt 0 ]; then
	files=$(printf '%s\n' "$@")
else
	# Tracked Markdown only. An untracked scratch file is not the project's problem.
	files=$(git ls-files '*.md')
fi

status=0
while IFS= read -r f; do
	[ -n "$f" ] || continue
	[ -f "$f" ] || continue
	is_exempt "$f" && continue
	commands=$(extract_commands "$f")
	[ -n "$commands" ] || continue
	i=0
	while [ "$i" -lt "${#patterns[@]}" ]; do
		hits=$(printf '%s\n' "$commands" | grep -E ":.*${patterns[$i]}" || true)
		if [ -n "$hits" ]; then
			while IFS= read -r hit; do
				[ -n "$hit" ] || continue
				printf '%s:%s: retired spelling (%s)\n' \
					"$f" "${hit%%:*}" "${names[$i]}"
				printf '    %s\n' "${hit#*:}"
				status=1
			done <<-EOH
				$hits
			EOH
		fi
		i=$((i + 1))
	done
done <<EOF
$files
EOF

if [ "$status" -ne 0 ]; then
	cat <<'EOF'

The commands above teach a spelling that brig removes in 0.3.
Every current spelling, and the whole mapping, is in docs/migration.md.

If a passage legitimately needs to name a retired spelling, put that discussion
in docs/migration.md, or mark the one prose line with an HTML comment
containing `retired-ok`.
EOF
fi

exit "$status"
