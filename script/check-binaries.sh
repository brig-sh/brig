#!/usr/bin/env bash
# Refuse a compiled binary in the tree.
#
# `go build ./test/conformance` run from the repository root leaves a
# `conformance` executable next to go.mod, and one reached main in #394.
# Review did not catch it, because GitHub shows a binary as one line with no
# diff. This script fails when a tracked file starts with an executable or
# object-file header: ELF, Mach-O, PE, WebAssembly or an ar archive.
#
# Only the files git itself treats as binary are read. A text file that starts
# with "MZ" is never refused. The images under assets/ are binary, but they
# carry no such header.
#
# Usage:
#   script/check-binaries.sh               check the files git tracks
#   script/check-binaries.sh --self-test   prove each header is refused
#
# macOS ships bash 3.2, so there is no mapfile here and no associative array.
set -euo pipefail
cd "$(dirname "$0")/.."

# Prints what a file's first four bytes, in hex, say it is. Prints nothing for
# a file that is not an executable or an object file.
kind() {
	case "$1" in
	7f454c46) echo "ELF executable or object" ;;
	feedface | cefaedfe) echo "32-bit Mach-O" ;;
	feedfacf | cffaedfe) echo "64-bit Mach-O" ;;
	cafebabe | bebafeca) echo "universal Mach-O or Java class" ;;
	4d5a*) echo "DOS or PE executable" ;;
	0061736d) echo "WebAssembly module" ;;
	213c6172) echo "ar archive" ;;
	esac
}

# Prints "<path>: <kind>" for each tracked file in the repository at $1 that
# starts with an executable header. Returns 1 when it prints any, and 2 when
# git cannot list the files. The return codes are explicit because errexit is
# off inside a command substitution and an if condition, which is where the
# callers run this.
check() {
	local list entry info path magic what status=0 tab=$'\t'
	list=$(mktemp) || return 2
	if ! git -C "$1" ls-files -z --eol >"$list"; then
		rm -f "$list"
		return 2
	fi
	while IFS= read -r -d '' entry; do
		info=${entry%%"$tab"*}
		path=${entry#*"$tab"}
		case "$info" in
		*w/-text*) ;;
		*) continue ;;
		esac
		if [ ! -f "$1/$path" ] || [ -L "$1/$path" ]; then
			continue
		fi
		magic=$(head -c 4 -- "$1/$path" | od -An -tx1 | tr -d ' \n')
		what=$(kind "$magic")
		if [ -n "$what" ]; then
			printf '%s: %s\n' "$path" "$what"
			status=1
		fi
	done <"$list"
	rm -f "$list"
	return "$status"
}

# One tracked file per header, each refused by name, so a dead case cannot
# hide behind a live one. A binary with no executable header, a text file that
# starts with "MZ" and an untracked executable have to pass, or a check that
# refused everything would pass the self-test too. With the bad files out of
# the index, the same repository has to pass outright.
self_test() {
	local tmp out status bad=0
	tmp=$(mktemp -d) && [ -n "$tmp" ] || return 1
	if ! git -C "$tmp" init -q; then
		rm -rf "$tmp"
		return 1
	fi
	mkdir "$tmp/dir with space"
	printf '\177ELF\002\001\001\000' >"$tmp/dir with space/elf bin"
	printf '\376\355\372\316\000\000\000\007' >"$tmp/macho32-be"
	printf '\316\372\355\376\007\000\000\000' >"$tmp/macho32-le"
	printf '\376\355\372\317\001\000\000\007' >"$tmp/macho64-be"
	printf '\317\372\355\376\007\000\000\001' >"$tmp/macho64-le"
	printf '\312\376\272\276\000\000\000\002' >"$tmp/universal-be"
	printf '\276\272\376\312\002\000\000\000' >"$tmp/universal-le"
	printf 'MZ\220\000\003\000\000\000' >"$tmp/app.exe"
	printf '\000asm\001\000\000\000' >"$tmp/module.wasm"
	printf '!<arch>\n/               0           \000\000' >"$tmp/lib.a"
	printf '\211PNG\r\n\032\n\000\000\000\rIHDR' >"$tmp/image.png"
	printf 'MZ is a text file.\n' >"$tmp/notes.md"
	git -C "$tmp" add -A
	printf '\177ELF\002\001\001\000' >"$tmp/untracked"

	if out=$(check "$tmp" 2>&1); then status=0; else status=$?; fi
	printf '%s\n' "$out"

	expect() {
		if grep -qxF -- "$1: $2" <<<"$out"; then
			echo "self-test: ok   $1 refused: $2"
		else
			echo "self-test: FAIL $1 was not refused with: $2"
			bad=1
		fi
	}
	expect "dir with space/elf bin" "ELF executable or object"
	expect macho32-be "32-bit Mach-O"
	expect macho32-le "32-bit Mach-O"
	expect macho64-be "64-bit Mach-O"
	expect macho64-le "64-bit Mach-O"
	expect universal-be "universal Mach-O or Java class"
	expect universal-le "universal Mach-O or Java class"
	expect app.exe "DOS or PE executable"
	expect module.wasm "WebAssembly module"
	expect lib.a "ar archive"

	local good
	for good in image.png notes.md untracked; do
		if grep -q -- "^$good: " <<<"$out"; then
			echo "self-test: FAIL $good was refused"
			bad=1
		else
			echo "self-test: ok   $good passed"
		fi
	done
	if [ "$status" != 1 ]; then
		echo "self-test: FAIL the check returned $status on a tree with binaries"
		bad=1
	fi

	git -C "$tmp" rm -q --cached -- "dir with space/elf bin" macho32-be \
		macho32-le macho64-be macho64-le universal-be universal-le app.exe \
		module.wasm lib.a
	if out=$(check "$tmp" 2>&1); then status=0; else status=$?; fi
	if [ "$status" = 0 ] && [ -z "$out" ]; then
		echo "self-test: ok   a tree with no binaries passed"
	else
		echo "self-test: FAIL a tree with no binaries returned $status:"
		printf '%s\n' "$out"
		bad=1
	fi

	rm -rf "$tmp"
	return "$bad"
}

if [ "${1:-}" = --self-test ]; then
	if self_test; then
		echo "self-test: PASS"
		exit 0
	fi
	echo "self-test: FAIL"
	exit 1
fi

status=0
check . || status=$?
case "$status" in
0) ;;
1)
	cat <<'EOF'

The files above are compiled binaries, and a build output does not belong in
the repository. Take each one out of the index with `git rm --cached <path>`.
If a build leaves it in the repository root, add the path to .gitignore.
EOF
	;;
*) echo "check-binaries: git could not list the tracked files" >&2 ;;
esac
exit "$status"
