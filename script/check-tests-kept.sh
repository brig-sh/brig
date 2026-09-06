#!/usr/bin/env bash
# Refuse a change that removes a test without saying so.
#
# A revert of a feature takes that feature's tests with it, so the tree that
# remains is an older codebase that passes its own suite: nothing fails, and
# `go test` reports success on a repository that has quietly lost work. That
# is not a hypothetical. brig lost the content of four merged pull requests
# this way, and the branch, the merge and every CI run were green throughout
# (see PR #155). No test can catch its own deletion; only a comparison across
# revisions can.
#
# So compare the test names the base has with the test names the head has, and
# fail on any that vanished. Names, read out of the source, rather than
# `go test -list`: the base of a rebase does not always compile, and a check
# meant to run when something is wrong must not need a working build.
#
# Renaming or deleting a test on purpose is ordinary work. Say so with the
# `removes-tests` label on the pull request, and this step is skipped.
#
# Usage: check-tests-kept.sh <base-ref> [head-ref]
set -euo pipefail

base=${1:?usage: check-tests-kept.sh <base-ref> [head-ref]}
head=${2:-HEAD}

# The merge base, not the base branch's tip: what matters is the tests this
# change started from, not the ones another change has landed since.
fork=$(git merge-base "$base" "$head")

names() {
	# `func TestFoo(` and its Benchmark, Fuzz and Example siblings, in test
	# files only. -h because the file a test lives in is free to change.
	git grep -h -E '^func (Test|Benchmark|Fuzz|Example)[A-Za-z0-9_]*\(' "$1" -- '*_test.go' 2>/dev/null |
		sed -E 's/^func ([A-Za-z0-9_]+).*/\1/' | sort -u
}

removed=$(comm -23 <(names "$fork") <(names "$head"))

if [ -z "$removed" ]; then
	printf 'every test present at %s is still here\n' "$(git rev-parse --short "$fork")"
	exit 0
fi

count=$(printf '%s\n' "$removed" | wc -l | tr -d ' ')
cat >&2 <<MSG
$count test(s) present at $(git rev-parse --short "$fork") are gone at $(git rev-parse --short "$head"):

$(printf '%s\n' "$removed" | sed 's/^/  /')

A test that disappears is either a rename, a deliberate removal, or a change
that reverted more than it meant to -- and the third kind is invisible to
every other check, because the tests that would have failed went with it.

If the removal is intended, label the pull request 'removes-tests'.
Otherwise compare against the merge base and look at what else went with them:

  git diff --stat $(git rev-parse --short "$fork") $(git rev-parse --short "$head")
MSG
exit 1
