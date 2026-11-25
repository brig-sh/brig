#!/bin/bash
# Copyright 2026 NOFire AI. All rights reserved.
# Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.
#
# Smoke test for what Brig Enterprise adds to the core: the real binary's
# help, completion and version, and brig login and brig logout against the
# fake authorization server. The core has its own, script/smoke.sh at the
# root.
#
#   enterprise/script/smoke.sh
set -uo pipefail
cd "$(dirname "$0")/.."

# Nothing here is answered by a person. The refusals below are what brig
# does with nobody to ask, and brig decides that by looking at its stdin.
exec < /dev/null

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
# brig runs with the caller's HOME. Telemetry would read and write that
# home's ~/.hull, so it is off.
export HULL_TELEMETRY_DISABLED=1

# Assertions capture brig's output first rather than piping it into grep -q,
# which can kill brig with SIGPIPE mid-write.
fail=0
ok()  { printf '  ok   %s\n' "$1"; }
bad() { printf '  FAIL %s\n' "$1"; fail=1; }

go build -o "$WORK/brig" ./cmd/brig || { echo "build failed"; exit 1; }

# --- the commands Brig Enterprise adds ---
out="$("$WORK/brig" version 2>&1)"
case "$out" in
  "brig "*", enterprise)") ok "brig version names the enterprise edition" ;;
  *) bad "brig version names the enterprise edition -- got: $out" ;;
esac
out="$("$WORK/brig" help 2>&1)"
case "$out" in
  *"brig login [URL]"*"brig logout [URL]"*"brig telemetry status|on|off"*) ok "brig help lists login and logout" ;;
  *) bad "brig help lists login and logout -- got: $out" ;;
esac
out="$("$WORK/brig" login --help 2>&1)"; rc=$?
case "$out" in
  "brig login -- log in to NOFire AI"*) [ "$rc" = 0 ] && ok "brig login --help prints its usage" \
    || bad "brig login --help exited $rc" ;;
  *) bad "brig login --help prints its usage -- got (exit $rc): $out" ;;
esac
out="$("$WORK/brig" __complete -- lo 2>/dev/null | tr '\n' ' ')"
[ "$out" = ":names login logout " ] && ok "completion offers login and logout" \
  || bad "completion offers login and logout -- got: $out"
out="$("$WORK/brig" __complete -- login - 2>/dev/null | tr '\n' ' ')"
[ "$out" = ":names --browser --device --ttl " ] && ok "completion offers brig login's flags" \
  || bad "completion offers brig login's flags -- got: $out"

# --- brig recordings ---
out="$("$WORK/brig" help 2>&1)"
case "$out" in
  *"brig logout [URL]"*"brig recordings flush"*"brig telemetry status|on|off"*) ok "brig help lists recordings" ;;
  *) bad "brig help lists recordings -- got: $out" ;;
esac
out="$("$WORK/brig" __complete -- rec 2>/dev/null | tr '\n' ' ')"
[ "$out" = ":names recordings " ] && ok "completion offers recordings" \
  || bad "completion offers recordings -- got: $out"
out="$("$WORK/brig" __complete -- recordings "" 2>/dev/null | tr '\n' ' ')"
[ "$out" = ":names flush " ] && ok "completion offers recordings flush" \
  || bad "completion offers recordings flush -- got: $out"
# With nothing waiting, flush sends nothing and creates nothing in the home.
home="$WORK/recordings-home"; mkdir "$home"
out="$(HOME="$home" "$WORK/brig" recordings flush 2>&1)"; rc=$?
case "$out" in
  "recordings: 0 sent and closed, 0 waiting for a login or a live run, 0 not sent yet") [ "$rc" = 0 ] \
    && ok "brig recordings flush with nothing waiting" || bad "brig recordings flush exited $rc" ;;
  *) bad "brig recordings flush with nothing waiting -- got (exit $rc): $out" ;;
esac
[ -z "$(ls -A "$home")" ] && ok "brig recordings flush leaves the home empty" \
  || bad "brig recordings flush wrote into the home: $(ls -A "$home")"

# --- brig login against the fake authorization server ---
# fakenofire grants a token to whoever asks (-approve auto). brig-stored is a
# test binary of internal/login that is Brig Enterprise itself, with the
# login tokens in the JSON file BRIG_TEST_LOGIN_STORE names, so that the
# login and the logout below, two processes, share one store without
# touching the keychain of whoever runs this.
go build -o "$WORK/fakenofire" ./internal/nofireauth/fakeas/fakenofire || { echo "build failed"; exit 1; }
go test -c -o "$WORK/brig-stored.test" ./internal/login || { echo "build failed"; exit 1; }
printf '#!/bin/sh\nBRIG_TEST_BRIG_HELPER=1 exec %q "$@"\n' "$WORK/brig-stored.test" > "$WORK/brig-stored"
chmod +x "$WORK/brig-stored"
"$WORK/fakenofire" -addr 127.0.0.1:0 -approve auto -url-file "$WORK/fakenofire.url" > "$WORK/fakenofire.out" 2>&1 &
fakenofire_pid=$!
trap 'kill "$fakenofire_pid" 2>/dev/null; rm -rf "$WORK"' EXIT
for _ in $(seq 100); do [ -s "$WORK/fakenofire.url" ] && break; sleep 0.1; done
LOGIN_URL="$(cat "$WORK/fakenofire.url" 2>/dev/null)"
login_store="$WORK/login-store.json"
if [ -z "$LOGIN_URL" ]; then
  bad "fakenofire did not report its URL -- got: $(cat "$WORK/fakenofire.out")"
else
  out="$(BRIG_TEST_LOGIN_STORE="$login_store" "$WORK/brig-stored" login "$LOGIN_URL" --device --ttl 1d 2>&1)"; rc=$?
  case "$out" in
    *"Logged in to"*) [ "$rc" = 0 ] && ok "brig login --device against the fake logs in" \
      || bad "brig login --device exited $rc -- got: $out" ;;
    *) bad "brig login --device prints Logged in to -- got (exit $rc): $out" ;;
  esac
  grep -qF "$LOGIN_URL" "$login_store" 2>/dev/null \
    && ok "the login store holds the deployment URL after login" \
    || bad "the login store holds $LOGIN_URL after login -- got: $(cat "$login_store" 2>&1)"

  out="$(BRIG_TEST_LOGIN_STORE="$login_store" "$WORK/brig-stored" logout "$LOGIN_URL" 2>&1)"; rc=$?
  if [ "$rc" = 0 ] && ! grep -qF "$LOGIN_URL" "$login_store"; then
    ok "brig logout exits 0 and removes the token from the store"
  else
    bad "brig logout exited $rc, store: $(cat "$login_store") -- got: $out"
  fi

  # A deployment named only by the environment is confirmed, and without a
  # terminal nobody can: the login is refused before the store is touched.
  printf '{}' > "$login_store"
  out="$(BRIG_NOFIRE_URL="$LOGIN_URL" BRIG_TEST_LOGIN_STORE="$login_store" "$WORK/brig-stored" login --device 2>&1 </dev/null)"; rc=$?
  case "$out" in
    *"(from BRIG_NOFIRE_URL)"*"BRIG_NOFIRE_URL names $LOGIN_URL, and brig needs a terminal to confirm it. Pass the URL as an argument to log in without one"*)
      if [ "$rc" != 0 ] && [ "$(cat "$login_store")" = '{}' ]; then
        ok "an environment deployment without a terminal is named, refused, and stores nothing"
      else
        bad "the environment deployment refusal exited $rc, store: $(cat "$login_store")"
      fi ;;
    *) bad "the environment deployment without a terminal is named and refused -- got (exit $rc): $out" ;;
  esac
fi

out="$(BRIG_TEST_LOGIN_STORE="$WORK/login-evil.json" "$WORK/brig-stored" login https://user@evil.example 2>&1)"; rc=$?
case "$out" in
  *"has a user name or password"*)
    [ "$rc" != 0 ] && [ ! -e "$WORK/login-evil.json" ] \
      && ok "brig login refuses a URL with user info" \
      || bad "the user info refusal exited $rc or wrote a store" ;;
  *) bad "brig login refuses a URL with user info -- got (exit $rc): $out" ;;
esac

[ "$fail" = 0 ] && echo PASS || echo FAILURES
exit "$fail"
