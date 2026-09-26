#!/bin/bash
# Leaves a shared Mac the way the macOS e2e job found it.
#
# Everything the job made lives under the scratch HOME named by E2E_SCRATCH:
# hull's store, a mounted sparse image, ~/.brig with its gateway sockets,
# ~/.config/brig, the Go caches. This stops every process whose command line
# names that directory, detaches the store, and removes the directory. The
# account's own ~/.hull and ~/.brig are never touched.
#
# It also removes what an earlier job of this runner account left behind.
# The workflow runs one macOS e2e job at a time, so no other scratch HOME of
# this account is in use.
#
# Usage: E2E_SCRATCH=/private/tmp/be2e.XXXXXX script/e2e/cleanup-macos.sh

set -uo pipefail

scratch="${E2E_SCRATCH:-}"
case "$scratch" in
  /private/tmp/be2e.*) ;;
  *) echo "cleanup-macos: E2E_SCRATCH is [$scratch], not a scratch HOME this job made" >&2; exit 0 ;;
esac

tmo() {
  local s=$1
  shift
  perl -e 'alarm shift @ARGV; exec @ARGV or die "exec: $!"' "$s" "$@"
}

# remove DIR: stops what runs under DIR, detaches its store and deletes it.
remove() {
  local dir=$1 bin="$1/.e2e/bin"
  echo "== $dir"
  if [ -x "$bin/brig" ]; then
    HOME="$dir" PATH="$bin:$PATH" BRIG_RUNTIME_BIN="$bin/hull" tmo 300 "$bin/brig" rm --all -y < /dev/null || true
  fi
  # The gateways brig started, and any monitor still up, name the scratch
  # directory in their arguments. Nothing outside this job does.
  echo "processes:"
  pgrep -fl "$dir/" | grep -v -E 'pgrep|cleanup-macos' || echo "(none)"
  pkill -TERM -f "$dir/" 2> /dev/null || true
  sleep 2
  pkill -KILL -f "$dir/" 2> /dev/null || true

  if [ -x "$bin/hull" ]; then
    HOME="$dir" tmo 120 "$bin/hull" store detach --force < /dev/null || true
  fi
  # Any image still attached from under the directory.
  hdiutil info | awk -v d="$dir/" '$1 == "image-path" && index($3, d) == 1 { print $3 }' |
    while read -r image; do
      dev="$(hdiutil info | awk -v i="$image" '$1 == "image-path" { f = ($3 == i) } f && /^\/dev\/disk[0-9]+[ \t]/ { print $1; exit }')"
      echo "detaching $image ($dev)"
      [ -z "$dev" ] || hdiutil detach -force "$dev" || true
    done

  # Go leaves its module cache read-only.
  chmod -R u+w "$dir" 2> /dev/null || true
  rm -rf "$dir"
  if [ -e "$dir" ]; then
    echo "cleanup-macos: could not remove $dir" >&2
    ls -la "$dir" >&2
    return 1
  fi
  echo "removed $dir"
}

rc=0
for old in /private/tmp/be2e.*; do
  if [ -d "$old" ] && [ -O "$old" ] && [ "$old" != "$scratch" ]; then
    echo "an earlier job left $old"
    remove "$old" || rc=1
  fi
done
if [ -d "$scratch" ]; then
  remove "$scratch" || rc=1
fi
echo "left: $(pgrep -fl 'be2e\.' | grep -v -E 'pgrep|cleanup-macos' || echo 'no process')"
exit "$rc"
