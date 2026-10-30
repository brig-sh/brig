#!/usr/bin/env bash
# Refuse documentation that teaches a command spelling brig retired or removed.
#
# brig renamed most of its verbs during the 0.1 series. Each old spelling
# worked for a while and printed one line naming its replacement, and v0.5.0
# removed all of them but `brig exec`, which still works. That grace period
# was what made the problem invisible: a doc teaching `brig profiles` was not
# broken while the alias worked, so nothing failed and no reader complained,
# and it became wrong on the release that dropped the alias. Issue #111 asks
# for this to be checked rather than reviewed by eye, so it does not rot the
# first time a doc is edited.
#
# Only COMMANDS are checked, not prose. A retired spelling is reported when it
# appears inside inline code or in a fenced code block, because that is where a
# reader copies from. The sentence "six of the shipped brig profiles ask for
# hvi" uses "profiles" as an ordinary noun and is left alone, and so is prose
# about exec'ing into a container.
#
# Three files are exempt, because naming the retired spellings is their job:
# docs/migration.md holds the old-to-new mapping, docs/stability.md documents
# the deprecation window, and a changelog is allowed to say what a release
# retired or removed.
#
# docs/herd.md is exempt from one pattern only, the exec one, because it has
# to teach `brig exec`. Its ssh ProxyCommand carries an SSH stream over the
# exec, which needs a command with no guest pty and no login shell -- `brig
# sh` asks for a pty and runs `bash -lc`, whose startup output would corrupt
# the stream. `brig exec` is the only verb that gives both until #335 lands,
# so removing it breaks herd, and skips_pattern is where that shows. The
# other patterns stay live on the page. The retired-ok marker cannot do this:
# the ProxyCommand is in a fenced block.
#
# Elsewhere, a single PROSE line can opt out with a `retired-ok` marker in an
# HTML comment, for a passage that names an old spelling to describe it rather
# than to teach it: explaining a limitation that only the retired spelling has,
# for instance. Keep these rare. A marker on a line that is really teaching the
# old spelling defeats the check. The marker does not work inside a fenced
# block, where it would become part of the code a reader copies.
#
# Short flags are scoped to the run line. `-n` was removed from `brig run`,
# where it became `<agent>@<label>`, and is current on `brig policy attach`,
# so the verb has to be part of the pattern.
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

# skips_pattern <file> <pattern>: one pattern switched off for one file.
skips_pattern() {
	case "$1:$2" in
	"docs/herd.md:brig (exec|shell)\b") return 0 ;;
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
	'brig (run|sh|create)\b.* (--name|--workspace)( |=|$)'
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
	'--name / --workspace on the run line -> <agent>@<label> / --home'
)

# Print "<line>:<command text>" for every place a reader could copy a command
# from: each inline-code span, and each line inside a fenced code block.
#
# An inline span can wrap across a line break, and a `brig run claude --name
# desktop` split that way went unseen until a reviewer caught it by eye. So a
# line holding an odd number of backticks has an open span, and the next line
# is joined to it before matching. The pair is reported at the first line's
# number, which is where a reader looks.
extract_commands() {
	awk '
		/^[[:space:]]*```/ { fence = !fence; next }
		/retired-ok/ { next }
		fence { print NR ":" $0; next }
		{
			line = $0
			start = NR
			# gsub returns the count, which is what says the span is open.
			probe = line
			if (gsub(/`/, "`", probe) % 2 == 1) {
				if ((getline nextline) > 0) line = line " " nextline
			}
			while (match(line, /`[^`]+`/)) {
				span = substr(line, RSTART + 1, RLENGTH - 2)
				print start ":" span
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
		if skips_pattern "$f" "${patterns[$i]}"; then
			i=$((i + 1))
			continue
		fi
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

The commands above teach a spelling that brig removed in v0.5.0, or, for
`brig exec`, one it removes once `brig sh` can pipe a command's output.
Every current spelling, and the whole mapping, is in docs/migration.md.

If a passage legitimately needs to name a retired spelling, put that discussion
in docs/migration.md, or mark the one prose line with an HTML comment
containing `retired-ok`.
EOF
fi

exit "$status"
