#!/bin/bash
# Resolve every row of docs/claims.md against the repository.
#
# docs/security.md makes promises, and a promise nothing checks holds only
# until a refactor ends it. docs/claims.md ties each quoted sentence on that
# page to the tests that defend it. This script fails when a row quotes a
# sentence the page no longer carries, or names a defence that no longer
# exists. A claim then cannot lose its test without CI saying so.
#
# A table starts at its delimiter row, and every non-blank line after that is
# a row until the first blank line. That is where Markdown ends the table. The
# outer pipes and the indent are optional there, so a row is read with or
# without them. A line in the table that is not three cells is refused, and so
# is a pipe line outside any table. Neither can drop out of the check unseen.
#
# A row has three cells: the claim, the section it sits in, and the defences.
# Only the double-quoted part of the claim cell is matched against the page,
# so a row can add a note in parentheses after it. The quote has to sit under
# the heading the section cell names, or under a heading nested in it. The
# page wraps its prose, so whitespace is collapsed on both sides before the
# match. Each defence is a backticked token, and the cell holds nothing else:
#
#   `go:TestName`     a func TestName( in a _test.go file
#   `smoke:<text>`    an ok "<text>" line in script/smoke.sh
#   `vm:<check>`      a vm_check <check> line in script/claims-vm.sh
#
# A vm check needs a booted sandbox, so it runs under `make claims-vm` and
# never here. This script resolves the name and lists the row as not yet run.
#
# Usage:
#   script/check-claims.sh               check docs/claims.md
#   script/check-claims.sh --self-test   prove each guard refuses a seeded row
#
# macOS ships bash 3.2, so there is no mapfile here and no associative array.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

collapse() { tr '\n\t' '  ' | tr -s ' '; }
trim() { sed -e 's/^ *//' -e 's/ *$//'; }

# cells LINE prints the line with its indent, its outer pipes and the blanks
# around them removed. The cells are what is left between the pipes.
cells() {
	local s="$1"
	s="$(printf '%s' "$s" | tr '\t' ' ' | trim)"
	s="${s#|}"
	s="${s%|}"
	printf '%s' "$s"
}

# ncells BODY counts the cells in what cells printed.
ncells() {
	local pipes
	pipes="$(printf '%s' "$1" | tr -cd '|')"
	echo $((${#pipes} + 1))
}

# is_delimiter LINE succeeds for a delimiter row: every cell is dashes, with
# an optional colon at either end. A line of dashes with no pipe is a
# heading underline or a rule, not a delimiter row.
is_delimiter() {
	case "$1" in
	*'|'*) grep -qE '^ *:?-+:? *(\| *:?-+:? *)*$' <<<"$(cells "$1")" ;;
	*) return 1 ;;
	esac
}

# section_text PAGE HEADING prints the lines under HEADING, down to the next
# heading at its level or above, so a nested heading stays inside. It fails
# when the page has no such heading. A # line in a code fence is not a
# heading.
section_text() {
	awk -v want="$2" '
	/^ *(```|~~~)/ { fence = !fence }
	!fence && /^#+ / {
		level = match($0, /[^#]/) - 1
		title = substr($0, level + 1)
		sub(/^ +/, "", title)
		sub(/ +#* *$/, "", title)
		if (on && level <= on_level) on = 0
		if (title == want) { on = 1; on_level = level; found = 1; next }
	}
	on { print }
	END { exit !found }' "$1"
}

# check TABLE PAGE SMOKE VM GOROOT
#
# Every path is an argument so the self-test can point the same code at a
# seeded table and a seeded repository.
check() {
	local table="$1" page="$2" smoke="$3" vm="$4" goroot="$5"
	local fail=0 rows=0 n_go=0 n_smoke=0 n_vm=0 not_run=""
	local flat n=0 line body k in_table=0 pending="" pending_n=0 prev=""
	local claim section defended quote sect_flat tokens rest tok name text where
	flat="$(collapse <"$page")"

	while IFS= read -r line || [ -n "$line" ]; do
		n=$((n + 1))
		body="$(cells "$line")"
		if [ -z "$(printf '%s' "$line" | tr -d ' \t')" ]; then
			in_table=0
			if [ -n "$pending" ]; then
				echo "$table:$pending_n: a pipe line outside any table"
				fail=1
			fi
			pending="" prev=""
			continue
		fi
		if [ "$in_table" = 0 ]; then
			if [ -n "$prev" ] && is_delimiter "$line"; then
				# prev is the header. A claims table has three columns.
				k="$(ncells "$body")"
				if [ "$k" != 3 ] || [ "$(ncells "$(cells "$prev")")" != 3 ]; then
					echo "$table:$n: a table that is not three columns"
					fail=1
				fi
				in_table=1 pending="" prev=""
				continue
			fi
			if [ -n "$pending" ]; then
				echo "$table:$pending_n: a pipe line outside any table"
				fail=1
			fi
			pending="" prev="$line"
			case "$(printf '%s' "$line" | trim)" in
			'|'*) pending="$line" pending_n=$n ;;
			esac
			continue
		fi

		rows=$((rows + 1))
		where="$table:$n"
		k="$(ncells "$body")"
		if [ "$k" != 3 ]; then
			echo "$where: not three cells (found $k)"
			fail=1
			continue
		fi
		IFS='|' read -r claim section defended <<<"$body"

		section="$(printf '%s' "$section" | trim)"
		sect_flat=""
		if [ -z "$section" ]; then
			echo "$where: no section in the Section cell"
			fail=1
		elif ! sect_flat="$(section_text "$page" "$section" | collapse)"; then
			echo "$where: no heading on the page: $section"
			fail=1
			sect_flat=""
		fi

		case "$claim" in
		*'"'*'"'*)
			quote="${claim#*\"}"
			quote="${quote%\"*}"
			quote="$(printf '%s' "$quote" | collapse | trim)"
			# grep -F takes a newline as a second pattern. collapse leaves
			# none, so the whole quote is one literal. The page goes in as a
			# here-string: piped from printf, grep -q can exit on an early
			# match while printf still writes, and pipefail then turns the
			# SIGPIPE into a refusal of a quote that is on the page.
			if [ -z "$quote" ] || ! grep -qF -- "$quote" <<<"$flat"; then
				echo "$where: not on the page: $quote"
				fail=1
			elif [ -n "$sect_flat" ] && ! grep -qF -- "$quote" <<<"$sect_flat"; then
				echo "$where: not under the heading $section: $quote"
				fail=1
			fi
			;;
		*)
			echo "$where: no double-quoted sentence in the claim cell"
			fail=1
			;;
		esac

		# shellcheck disable=SC2016 # the backticks are markdown, not a command
		tokens="$(printf '%s\n' "$defended" | grep -oE '`[^`]*`' | tr -d '`')"
		# A reader takes an unquoted go:TestName for a defence too, and
		# nothing would resolve it.
		# shellcheck disable=SC2016
		rest="$(printf '%s' "$defended" | sed 's/`[^`]*`//g' | trim)"
		if [ -n "$rest" ]; then
			echo "$where: text outside a token in the Defended by cell: $rest"
			fail=1
		fi
		if [ -z "$tokens" ]; then
			echo "$where: no token in the Defended by cell"
			fail=1
			continue
		fi
		while IFS= read -r tok; do
			[ -z "$tok" ] && continue
			case "$tok" in
			go:Test*)
				name="${tok#go:}"
				# The open bracket stops TestFoo from resolving against a
				# TestFooBar. .claude holds other checkouts of this
				# repository, whose tests are not this tree's.
				if grep -rqF --include='*_test.go' --exclude-dir=.git --exclude-dir=.claude \
					-- "func $name(" "$goroot"; then
					n_go=$((n_go + 1))
				else
					echo "$where: missing go test: $name"
					fail=1
				fi
				;;
			smoke:?*)
				text="${tok#smoke:}"
				if grep -qF -- "ok \"$text\"" "$smoke"; then
					n_smoke=$((n_smoke + 1))
				else
					echo "$where: missing smoke assertion: $text"
					fail=1
				fi
				;;
			vm:?*)
				name="${tok#vm:}"
				case "$name" in
				*[!a-z0-9-]*)
					echo "$where: unknown token: $tok"
					fail=1
					continue
					;;
				esac
				if grep -qE "^vm_check $name( |\$)" "$vm" 2>/dev/null; then
					n_vm=$((n_vm + 1))
					not_run="$not_run$where: vm:$name
"
				else
					echo "$where: missing vm check: $name"
					fail=1
				fi
				;;
			*)
				echo "$where: unknown token: $tok"
				fail=1
				;;
			esac
		done <<EOF
$tokens
EOF
	done <"$table"
	if [ -n "$pending" ]; then
		echo "$table:$pending_n: a pipe line outside any table"
		fail=1
	fi

	if [ "$rows" = 0 ]; then
		echo "$table: no rows"
		fail=1
	fi
	echo "$table: $rows rows, $n_go go tests, $n_smoke smoke assertions, $n_vm vm checks"
	if [ -n "$not_run" ]; then
		echo "not yet run, these need a booted sandbox (make claims-vm):"
		printf '%s' "$not_run" | sed 's/^/  /'
	fi
	return "$fail"
}

# One seeded bad row per guard, plus good rows, all in a repository built in
# a temporary directory. Each guard has to print its own refusal. A single
# failed status lets a dead guard hide behind a live one, so the self-test
# looks for every message by name. The good rows have to pass, or a checker
# that refused everything passes the self-test too.
self_test() {
	local tmp out status bad=0 nl='
'
	tmp="$(mktemp -d)" && [ -n "$tmp" ] || return 1
	mkdir -p "$tmp/go"
	printf 'package x\n\nfunc TestClaimsSelfTestReal(t *testing.T) {}\n' >"$tmp/go/x_test.go"
	printf 'ok "a real smoke assertion"\n' >"$tmp/smoke.sh"
	printf 'vm_check real-check probe_real\n' >"$tmp/claims-vm.sh"
	# The page wraps the good sentence across a line, as docs/security.md
	# does. Other is nested in Page, and a # line in a fence is no heading.
	cat >"$tmp/page.md" <<'EOF'
# Page

The guest reaches only
what you   name.

## Other

A sentence under another heading.

```
# Fenced
```
EOF
	# Lines 17 and 18 are a row without its outer pipes and an indented one,
	# each naming a test that does not exist. Lines 19 and 20 are the same
	# shapes with a good row. Line 21 is a one-cell row. The table ends at
	# line 22, so line 23 is a pipe line with no table. Lines 25 and 26 are
	# a header with no delimiter row, and lines 28 to 30 a table of two
	# columns.
	cat >"$tmp/claims.md" <<'EOF'
| Claim | Section | Defended by |
| --- | --- | --- |
| "The guest  reaches only what you name." (a note the page does not carry) | Page | `go:TestClaimsSelfTestReal` `smoke:a real smoke assertion` `vm:real-check` |
| "The guest reaches only what you name." | Page | `go:TestClaimsSelfTestMissing` |
| "The guest reaches only what you name." | Page | `smoke:a smoke assertion nobody wrote` |
| "A sentence the page never carried." | Page | `go:TestClaimsSelfTestReal` |
| "The guest reaches only what you name." | Page | `pending:TestClaimsSelfTestReal` |
| "The guest reaches only what you name." | Page | |
| "The guest reaches only what you name." | Page | `vm:no-such-check` |
| The guest reaches only what you name. | Page | `go:TestClaimsSelfTestReal` |
| "The guest reaches only what you name." | Page | `go:TestClaimsSelfTestReal` | extra |
| "The guest reaches only what you name." | Page | `vm:Bad_Name` |
| "The guest reaches only what you name." | Page | `go:TestClaimsSelfTestReal` go:TestClaimsSelfTestLoose |
| "The guest reaches only what you name." | | `go:TestClaimsSelfTestReal` |
| "The guest reaches only what you name." | Fenced | `go:TestClaimsSelfTestReal` |
| "The guest reaches only what you name." | Other | `go:TestClaimsSelfTestReal` |
"The guest reaches only what you name." | Page | `go:TestClaimsSelfTestNoPipe` |
 | "The guest reaches only what you name." | Page | `go:TestClaimsSelfTestIndented` |
"The guest reaches only what you name." | Page | `go:TestClaimsSelfTestReal`
   | "A sentence under another heading." | Page | `go:TestClaimsSelfTestReal` |
a line with no cells

| "The guest reaches only what you name." | Page | `go:TestClaimsSelfTestReal` |

| Claim | Section | Defended by |
| "The guest reaches only what you name." | Page | `go:TestClaimsSelfTestReal` |

| Claim | Defended by |
| --- | --- |
| "The guest reaches only what you name." | `go:TestClaimsSelfTestReal` |
EOF
	# A table with a header and no rows proves nothing, so it fails too.
	printf '| Claim | Section | Defended by |\n| --- | --- | --- |\n' >"$tmp/empty.md"
	out="$(check "$tmp/claims.md" "$tmp/page.md" "$tmp/smoke.sh" "$tmp/claims-vm.sh" "$tmp/go" 2>&1)"
	status=$?
	local empty empty_status
	empty="$(check "$tmp/empty.md" "$tmp/page.md" "$tmp/smoke.sh" "$tmp/claims-vm.sh" "$tmp/go" 2>&1)"
	empty_status=$?
	rm -rf "$tmp"
	printf '%s\n%s\n' "$out" "$empty"

	expect() {
		if grep -qF -- "$tmp/claims.md:$1: $2" <<<"$out"; then
			echo "self-test: ok   line $1 refused: $2"
		else
			echo "self-test: FAIL line $1 was not refused with: $2"
			bad=1
		fi
	}
	expect 4 "missing go test: TestClaimsSelfTestMissing"
	expect 5 "missing smoke assertion: a smoke assertion nobody wrote"
	expect 6 "not on the page: A sentence the page never carried."
	expect 7 "unknown token: pending:TestClaimsSelfTestReal"
	expect 8 "no token in the Defended by cell"
	expect 9 "missing vm check: no-such-check"
	expect 10 "no double-quoted sentence in the claim cell"
	expect 11 "not three cells (found 4)"
	expect 12 "unknown token: vm:Bad_Name"
	expect 13 "text outside a token in the Defended by cell: go:TestClaimsSelfTestLoose"
	expect 14 "no section in the Section cell"
	expect 15 "no heading on the page: Fenced"
	expect 16 "not under the heading Other: The guest reaches only what you name."
	expect 17 "missing go test: TestClaimsSelfTestNoPipe"
	expect 18 "missing go test: TestClaimsSelfTestIndented"
	expect 21 "not three cells (found 1)"
	expect 23 "a pipe line outside any table"
	expect 25 "a pipe line outside any table"
	expect 29 "a table that is not three columns"
	if [ "$empty_status" != 0 ] && grep -qF -- "$tmp/empty.md: no rows" <<<"$empty"; then
		echo "self-test: ok   a table with no rows refused"
	else
		echo "self-test: FAIL a table with no rows was not refused"
		bad=1
	fi
	# A refusal starts its line with the row's place. The not-yet-run list
	# names the first good row too, indented, and is not a refusal.
	local good
	for good in 3 19 20; do
		case "$nl$out" in
		*"$nl$tmp/claims.md:$good: "*)
			echo "self-test: FAIL the good row on line $good was refused"
			bad=1
			;;
		*) echo "self-test: ok   the good row on line $good resolved" ;;
		esac
	done
	if [ "$status" = 0 ]; then
		echo "self-test: FAIL the check exited 0 on a table with bad rows"
		bad=1
	fi
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

check docs/claims.md docs/security.md script/smoke.sh script/claims-vm.sh .
