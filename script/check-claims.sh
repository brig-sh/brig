#!/bin/bash
# Verify every test named in docs/claims.md actually exists.
#
# docs/claims.md maps each public security claim to the test that defends it.
# The failure this catches is a row that names a test which no longer exists: a
# claim left silently undefended after a rename, a move, or a deletion. Without
# this check the table drifts from the suite and reads as coverage it does not
# have. So it runs in CI, and a stale row fails the build rather than the next
# refactor.
#
# The Defended-by column names each test with a typed, backticked token:
#
#   `go:TestName`      a Go test, checked against `go test -list ./...`
#   `smoke:<ok text>`  a named check in script/*.sh, matched as ok "<ok text>"
#   `vm` or `vm:...`   needs a real VM; defended by the `make claims-vm` target
#   `pending` / `pending:...`  a test on an unmerged branch, not assertable yet
#
# Usage:
#   script/check-claims.sh [claims-file]      # default: docs/claims.md
#   script/check-claims.sh --self-test        # prove it fails on a missing test
set -uo pipefail
cd "$(dirname "$0")/.."

go_tests=""
load_go_tests() {
	# One compile of every package's test binary, names only. The per-package
	# summary lines ("ok pkg", "no test files") do not begin with Test, so a
	# Test-anchored grep keeps only the names themselves.
	go_tests="$(go test -list '.*' ./... 2>/dev/null | grep '^Test' | sort -u)"
}

fail=0

check_file() {
	file="$1"
	n_go=0
	n_smoke=0
	n_vm=0
	n_pending=0

	# Only table rows carry references, and each row is a single line, so pull
	# the typed backticked tokens out of the lines that start with a pipe. The
	# legend and prose above the table never start with one, so an example
	# prefix written there is not mistaken for a real reference.
	tokens="$(grep '^|' "$file" \
		| grep -oE '`(go|smoke|vm|pending)(:[^`]*)?`' \
		| tr -d '`')"
	if [ -z "$tokens" ]; then
		echo "no claim references found in $file"
		fail=1
		return
	fi

	while IFS= read -r tok; do
		[ -z "$tok" ] && continue
		case "$tok" in
		go:*)
			name="${tok#go:}"
			if printf '%s\n' "$go_tests" | grep -qx "$name"; then
				n_go=$((n_go + 1))
			else
				echo "MISSING go test: $name"
				fail=1
			fi
			;;
		smoke:*)
			text="${tok#smoke:}"
			# A named check is ok "<text>" somewhere under script/. Match the
			# literal text with -F so commas and dashes in the sentence stay
			# text rather than becoming a pattern.
			if grep -qF "ok \"$text\"" script/*.sh 2>/dev/null; then
				n_smoke=$((n_smoke + 1))
			else
				echo "MISSING smoke check: $text"
				fail=1
			fi
			;;
		vm*)
			if grep -qE '^claims-vm:' Makefile; then
				n_vm=$((n_vm + 1))
			else
				echo "MISSING make target: claims-vm (named by a vm row)"
				fail=1
			fi
			;;
		pending*)
			n_pending=$((n_pending + 1))
			;;
		esac
	done <<EOF
$tokens
EOF

	echo "$file: $n_go go tests, $n_smoke smoke checks, $n_vm vm rows, $n_pending pending"
}

# --self-test builds a table with one go: row and one smoke: row, each naming a
# reference that cannot exist, and confirms the checker rejects BOTH. This is the
# "dry run" the acceptance criteria asks for: proof each guard is live, not just
# present, runnable from CI. Both token kinds are covered because they resolve
# through independent code paths (go test -list vs a grep over script/*.sh); a
# self-test that exercised only one could stay green while the other silently
# auto-passed every row.
if [ "${1:-}" = "--self-test" ]; then
	load_go_tests
	tmp="$(mktemp)"
	cat >"$tmp" <<'EOF'
| Claim | Where | Defended by | Status |
| --- | --- | --- | --- |
| a claim no go test defends | nowhere | `go:TestThatCannotExist_ClaimsSelfCheck` | new |
| a claim no smoke check defends | nowhere | `smoke:this smoke check cannot exist -- claims self-test` | new |
EOF
	out="$(
		fail=0
		check_file "$tmp"
		echo "SELFTEST_FAIL=$fail"
	)"
	rm -f "$tmp"
	printf '%s\n' "$out"
	# Each guard must fire independently: the go: row and the smoke: row must
	# each be reported missing. Asserting only SELFTEST_FAIL=1 would let a broken
	# smoke branch hide behind the go branch's failure (and vice versa).
	saw_go=0
	saw_smoke=0
	printf '%s\n' "$out" | grep -q 'MISSING go test: TestThatCannotExist_ClaimsSelfCheck' && saw_go=1
	printf '%s\n' "$out" | grep -q 'MISSING smoke check: this smoke check cannot exist' && saw_smoke=1
	if [ "$saw_go" = 1 ] && [ "$saw_smoke" = 1 ]; then
		echo "self-test: PASS -- the checker rejects a missing go test and a missing smoke check"
		exit 0
	fi
	[ "$saw_go" = 0 ] && echo "self-test: FAIL -- the go-token guard did not report the missing test"
	[ "$saw_smoke" = 0 ] && echo "self-test: FAIL -- the smoke-token guard did not report the missing check"
	exit 1
fi

load_go_tests
check_file "${1:-docs/claims.md}"
if [ "$fail" = 0 ]; then
	echo "all claim tests present"
fi
exit "$fail"
