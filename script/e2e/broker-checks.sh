# shellcheck shell=bash
# The checks of policy documents and of the credential broker (#516-#521),
# on a real runtime. canary-linux.sh and canary-macos.sh source this file, so
# both hosts run one copy. Written for the bash 3.2 that macOS ships.
#
# The canary that sources it supplies res, run, gsh, one_line, field and
# leftovers, and these:
#
#   BROKER_RUN_ARGS   extra flags for a claude-code boot, such as --mem
#   broker_store_up   opens a secret store brig can write to, or returns 1
#                     with the reason on stdout
#   broker_store_down closes what broker_store_up opened
#   broker_transport  prints the runtime's evidence that SANDBOX's guest
#                     reaches brig.internal through the divert, one line
#
# Every secret here is a dummy that this file draws, and no request reaches
# an upstream with a real credential. A value and a session token go to the
# guest's environment, never to a command line, and stay out of the logs:
# the guest's answers that hold one are read with GSH_QUIET=1.

# Guest command text is single-quoted, so that the guest's shell expands it.
# shellcheck disable=SC2016

BROKER_DIR="$HOME/.brig/host/broker"
POLICY_DIR="${BRIG_POLICY_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/brig/policies}"
# The upstream of the test route: a public service that echoes the request
# headers it got. Each run reaches it from the host, so the route's answer
# is compared with a control.
ECHO_ORIGIN="${E2E_ECHO_ORIGIN:-https://postman-echo.com}"

# bto SECONDS CMD...: CMD, ended after SECONDS. macOS has no timeout(1).
bto() {
  local s=$1
  shift
  if command -v timeout > /dev/null 2>&1; then
    timeout --kill-after=10 "$s" "$@"
  else
    perl -e 'alarm shift @ARGV; exec @ARGV or die "exec: $!"' "$s" "$@"
  fi
}

# rand_hex N: N random bytes, as hex.
rand_hex() { head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; }

# count_of NEEDLE TEXT: how many times NEEDLE occurs in TEXT.
count_of() {
  if [ -z "$1" ]; then
    echo 0
    return 0
  fi
  # grep finds nothing with exit 1, and pipefail would make that the answer.
  printf '%s' "$2" | { grep -a -o -F -- "$1" || true; } | wc -l | tr -d ' '
}

# kv NAME TEXT: the value TEXT gives NAME, in NAME=value lines.
kv() { printf '%s\n' "$2" | sed -n "s/^$1=//p" | head -n 1 | tr -d '\r'; }

# broker_pids: this user's brig-broker processes. The bracket keeps pgrep
# from matching a shell whose command line names the pattern. On macOS the
# canary's brig-broker runs from the scratch HOME, and only one whose command
# line names it is this run's.
broker_pids() {
  local p
  { pgrep -u "$(id -u)" -f '[b]rig-broker' 2> /dev/null || true; } | while read -r p; do
    if [ "$(uname -s)" != Darwin ] || ps -o command= -p "$p" 2> /dev/null | grep -q -F "$HOME/"; then
      echo "$p"
    fi
  done
}

# broker_status: the broker's GET /status, on its control socket.
broker_status() {
  bto 10 curl -sS --unix-socket "$BROKER_DIR/control.sock" http://broker/status 2> /dev/null || true
}

# divert_sock SANDBOX: the divert socket the broker keeps for SANDBOX.
divert_sock() {
  local h
  h="$(printf '%s' "$1" | { shasum -a 256 2> /dev/null || sha256sum; } | cut -c1-12)"
  echo "$BROKER_DIR/divert/$h.sock"
}

# put_policy NAME: writes stdin as the policy file NAME.yaml.
put_policy() {
  mkdir -p "$POLICY_DIR"
  cat > "$POLICY_DIR/$1.yaml"
}

# rm_route_state: what an earlier, interrupted run of these checks left, so
# a run starts from nothing.
rm_route_state() {
  local s p
  for s in rt rt2 miss nodiv nostore relay flat; do
    bto 300 brig rm "claude-code@$s" < /dev/null > /dev/null 2>&1 || true
  done
  for p in e2e-route e2e-miss e2e-relay e2e-flat e2e-env; do
    for s in rt rt2 miss nodiv nostore relay flat; do
      bto 60 brig policy detach "$p" claude-code -n "$s" < /dev/null > /dev/null 2>&1 || true
    done
    rm -f "$POLICY_DIR/$p.yaml"
  done
  rm -f "$POLICY_DIR"/e2e-bad-*.yaml
}

# ---------------------------------------------------------------- documents

# check_policy_documents: the v1alpha1 envelope loads, with a route; the flat
# form still loads, a trailing --- included, and check says it is retired;
# and the documents brig must refuse are refused, each with its reason.
# Nothing boots.
check_policy_documents() {
  local out rc=0 bad name want missing="" listed
  rm_route_state

  put_policy e2e-env << 'EOF'
apiVersion: brig.sh/v1alpha1
kind: Policy
metadata:
  name: e2e-env
  description: e2e, the envelope with a route
spec:
  egress:
    default: deny
    allow:
      - host: example.com
  routes:
    - name: echo-1
      upstream: https://echo.example.com
      credential:
        secretRef: { name: e2e-none }
      inject:
        headers:
          set: [{ name: X-E2e-Credential, value: "{value}" }]
      guest:
        env: { E2E_ROUTE_URL: "{route.url}", E2E_ROUTE_TOKEN: "{session.token}" }
      request:
        allow:
          methods: [GET]
          paths: [/headers]
EOF
  # The flat form, with no kind, as policies before the envelope were
  # written, ending in a --- with nothing after it.
  printf '%s\n' 'apiVersion: brig.sh/v1alpha1' 'name: e2e-flat' 'egress:' '  default: deny' '  allow:' \
    '    - host: example.com' '---' | put_policy e2e-flat

  out="$(bto 60 brig policy show e2e-env < /dev/null 2>&1)" || rc=$?
  echo "policy show e2e-env: exit $rc"
  echo "$out"
  local rc2=0 out2
  out2="$(bto 60 brig policy show e2e-flat < /dev/null 2>&1)" || rc2=$?
  echo "policy show e2e-flat: exit $rc2"
  echo "$out2"
  if [ "$rc" = 0 ] && echo "$out" | grep -q '^kind: Policy' && echo "$out" | grep -q 'name: echo-1' &&
     [ "$rc2" = 0 ] && echo "$out2" | grep -q '^kind: Policy' && echo "$out2" | grep -q 'example.com'; then
    res check Policy "A v1alpha1 policy with a route loads, and so does the flat form with a trailing --- (#516)" pass \
      "policy show prints both as kind: Policy; e2e-env keeps route echo-1, e2e-flat its allow rule"
  else
    res check Policy "A v1alpha1 policy with a route loads, and so does the flat form with a trailing --- (#516)" fail \
      "e2e-env: exit $rc, $(echo "$out" | one_line 200); e2e-flat: exit $rc2, $(echo "$out2" | one_line 200)"
  fi

  rc=0
  run 60 brig policy attach e2e-flat claude-code -n flat < /dev/null || true
  out="$(bto 60 brig policy check claude-code -n flat < /dev/null 2>&1)" || rc=$?
  echo "policy check claude-code -n flat: exit $rc"
  echo "$out"
  run 60 brig policy detach e2e-flat claude-code -n flat < /dev/null || true
  if [ "$rc" = 0 ] && echo "$out" | grep -q 'e2e-flat.yaml uses the flat policy form'; then
    res check Policy "policy check names a policy still in the flat form (#516)" pass \
      "exit 0: $(echo "$out" | grep 'flat policy form' | one_line 200)"
  else
    res check Policy "policy check names a policy still in the flat form (#516)" fail \
      "exit $rc: $(echo "$out" | one_line 300)"
  fi

  # Each document brig must refuse, and the reason it gives. A route's name
  # also names a guest variable, so a dot, an underscore and upper case are
  # refused. An empty allow list, or one with no value, would read as unset
  # and allow every request.
  # bad_policy NAME ROUTE [LINE...]: a policy whose one route is ROUTE, with
  # LINE added to the route.
  bad_policy() {
    local n=$1 r=$2
    shift 2
    {
      printf '%s\n' 'apiVersion: brig.sh/v1alpha1' 'kind: Policy' 'metadata:' "  name: $n" 'spec:' \
        '  egress:' '    default: deny' '  routes:' "    - name: $r" \
        '      upstream: https://echo.example.com' \
        '      credential: { secretRef: { name: e2e-none } }' \
        '      inject: { headers: { set: [{ name: X-E2e-Credential, value: "{value}" }] } }' \
        '      guest: { env: { E2E_ROUTE_URL: "{route.url}" } }'
      [ "$#" = 0 ] || printf '%s\n' "$@"
    } | put_policy "$n"
  }
  bad_policy e2e-bad-dot an.thropic
  bad_policy e2e-bad-underscore an_thropic
  bad_policy e2e-bad-upper Anthropic
  bad_policy e2e-bad-empty echo '      request: { allow: { methods: [] } }'
  bad_policy e2e-bad-null echo '      request:' '        allow:' '          paths:'
  {
    printf '%s\n' 'apiVersion: brig.sh/v1alpha1' 'kind: Policy' 'metadata:' '  name: e2e-bad-twodocs' 'spec:' \
      '  egress:' '    default: deny' '---' 'apiVersion: brig.sh/v1alpha1' 'kind: Policy' 'metadata:' \
      '  name: e2e-bad-second' 'spec:' '  egress:' '    default: allow'
  } | put_policy e2e-bad-twodocs
  for bad in e2e-bad-dot e2e-bad-underscore e2e-bad-upper e2e-bad-empty e2e-bad-null e2e-bad-twodocs; do
    echo "--- $bad.yaml"
    cat "$POLICY_DIR/$bad.yaml"
  done

  out="$(bto 60 brig policy ls < /dev/null 2>&1)" || true
  echo "policy ls:"
  echo "$out"
  listed="$(bto 60 brig policy ls < /dev/null 2> /dev/null | awk '{ print $1 }' | grep '^e2e-bad' || true)"
  for bad in \
    'e2e-bad-dot|route name "an.thropic" may use only lowercase letters, digits and dashes' \
    'e2e-bad-underscore|route name "an_thropic" may use only lowercase letters, digits and dashes' \
    'e2e-bad-upper|route name "Anthropic" may use only lowercase letters, digits and dashes' \
    'e2e-bad-empty|request.allow.methods is empty' \
    'e2e-bad-null|has no value' \
    'e2e-bad-twodocs|more than one YAML document'; do
    name="${bad%%|*}"
    want="${bad#*|}"
    # The reason is on the line that names the file.
    if ! echo "$out" | grep -F "$name.yaml" | grep -q -F -- "$want"; then
      missing+="$name ($want); "
    fi
  done
  if [ -z "$missing" ] && [ -z "$listed" ]; then
    res check Policy "Bad route names, empty or null allow lists and a second document are refused (#516)" pass \
      "policy ls loads none of the 6 and names each file with its reason: a dot, an underscore or upper case in a route name, methods: [], paths: with no value, a second document"
  else
    res check Policy "Bad route names, empty or null allow lists and a second document are refused (#516)" fail \
      "no reason for: [${missing}]; listed as loaded: [$(echo "$listed" | one_line 120)]; $(echo "$out" | grep e2e-bad | one_line 300)"
  fi
  rm -f "$POLICY_DIR"/e2e-bad-*.yaml

  rc=0
  out="$(bto 60 brig policy show --preset claude-code < /dev/null 2>&1)" || rc=$?
  echo "policy show --preset claude-code: exit $rc"
  echo "$out"
  if [ "$rc" = 0 ] && echo "$out" | grep -q 'upstream: https://api.anthropic.com' &&
     echo "$out" | grep -q 'CLAUDE_CODE_OAUTH_TOKEN' && echo "$out" | grep -A3 '^modes:' | grep -q endpoint; then
    res check Policy "policy show --preset prints the claude-code route (#516)" pass \
      "upstream https://api.anthropic.com, CLAUDE_CODE_OAUTH_TOKEN for the session token, modes: endpoint"
  else
    res check Policy "policy show --preset prints the claude-code route (#516)" fail "exit $rc: $(echo "$out" | one_line 300)"
  fi

  # A route in a mode this build does not serve parses, and check refuses it.
  awk '{ print } /^    - name: echo-1$/ { print "      mode: relay" }' "$POLICY_DIR/e2e-env.yaml" |
    sed 's/name: e2e-env/name: e2e-relay/' | put_policy e2e-relay
  rc=0
  run 60 brig policy attach e2e-relay claude-code -n relay < /dev/null || true
  out="$(bto 60 brig policy check claude-code -n relay < /dev/null 2>&1)" || rc=$?
  echo "policy check claude-code -n relay: exit $rc"
  echo "$out"
  run 60 brig policy detach e2e-relay claude-code -n relay < /dev/null || true
  if [ "$rc" != 0 ] && echo "$out" | grep -q 'route mode relay is not part of this build'; then
    res check Policy "policy check refuses a route in a mode this build does not serve (#521)" pass \
      "exit $rc: $(echo "$out" | grep 'relay is not part' | one_line 240)"
  else
    res check Policy "policy check refuses a route in a mode this build does not serve (#521)" fail \
      "exit $rc: $(echo "$out" | one_line 300)"
  fi
  rm -f "$POLICY_DIR/e2e-env.yaml" "$POLICY_DIR/e2e-flat.yaml" "$POLICY_DIR/e2e-relay.yaml"
}

# ---------------------------------------------------------------- routed runs

# REQ is guest text that defines req: req METHOD URL [CURL_ARGS...] sends one
# request with the guest's own trust, the bundle in SSL_CERT_FILE, and
# prints HTTP=<status> and the body. The token goes in a header from the
# guest's environment, so it is never on a command line.
# A request curl could not make prints HTTP=000 and curl's error.
REQ='req() { m=$1; u=$2; shift 2; rm -f /tmp/e2e-body; c=$(curl -sS -m 30 -X "$m" -o /tmp/e2e-body -w "%{http_code}" "$@" "$u" 2> /tmp/e2e-err); r=$?; if [ "$r" = 0 ]; then echo "HTTP=$c"; else echo "HTTP=000 curl exit $r: $(head -c 160 /tmp/e2e-err | tr "\n" " ")"; fi; echo "BODY=$(head -c 3000 /tmp/e2e-body 2> /dev/null | tr "\n" " ")"; }'

# SCAN is guest text that prints everything a process in the guest can read
# about its credentials: the environment of every process, every command
# line, and every small file under the places brig writes. The host counts
# the dummy values in it.
SCAN='env; for f in /proc/[0-9]*/environ /proc/[0-9]*/cmdline; do tr "\0" "\n" < "$f" 2> /dev/null; echo; done; cat /proc/cmdline; find /brig /root /tmp /run /var/tmp -xdev -type f -size -1024k 2> /dev/null | while read -r f; do tr -c "[:print:]\n" " " < "$f" 2> /dev/null; echo; done'

# check_broker boots claude-code under a policy with two routes: echo, a
# route of its own to an echo service with a dummy secret, and anthropic, on
# the claude-code preset with a dummy OAuth document under the name
# claude-credentials. No request goes to the anthropic route, so the dummy
# never reaches Anthropic and nothing renews it.
check_broker() {
  local why sb url tok rc=0 out
  local G_ROUTE="The guest gets the route URL and a session token, and never the credential (#519)"
  local G_SCAN="No dummy credential is anywhere the guest can read (#519)"
  local G_FWD="A request through brig.internal reaches the upstream with the credential added (#518, #521)"
  local G_ALLOW="A request outside request.allow is refused with 403 and its rule (#518)"
  local G_CARRIER="A session token outside its carrier is refused with 403 (#520)"
  local G_BADTOK="A request with no token, or a forged one, is refused with 401 (#518)"
  local G_EGRESS="The guest cannot reach the upstream directly under default: deny (#521)"
  local G_INFO="brig info and info --json name the broker, and no secret (#520)"
  local G_TRANSPORT="The runtime diverts brig.internal to the broker (#521)"
  local G_ONE="One brig-broker serves every sandbox on the host (#519)"
  local G_STOP="After brig stop and a new boot, the old token is refused with 401 (#518)"
  local G_RM="brig rm removes the sandbox from the broker, and its divert socket (#519)"
  local G_LAST="brig-broker stops when the last sandbox is removed (#519)"
  local G_LOGS="The broker's audit.log and broker.log hold no credential or token (#518)"
  local G_MISS="A run whose route secret is not in the store is refused with exit 6 (#519)"
  local all=("$G_ROUTE" "$G_SCAN" "$G_FWD" "$G_ALLOW" "$G_CARRIER" "$G_BADTOK" "$G_EGRESS" "$G_INFO"
    "$G_TRANSPORT" "$G_ONE" "$G_STOP" "$G_RM" "$G_LAST" "$G_LOGS" "$G_MISS")
  local n

  # The job's token would reach claude-code's guest as GH_TOKEN, and only the
  # dummies belong in what the guest can read.
  unset GH_TOKEN
  rm_route_state

  if ! why="$(broker_store_up)"; then
    echo "no secret store: $why"
    for n in "${all[@]}"; do
      res check Broker "$n" skip "no secret store brig can write to on this host: $why"
    done
    return 0
  fi
  echo "$why"
  # A run that died before its cleanup left its e2e- secrets.
  for n in $(bto 60 brig secret ls < /dev/null 2> /dev/null | awk '$1 ~ /^e2e-(echo|cc)-[0-9a-f]+$/ { print $1 }'); do
    bto 60 brig secret delete "$n" -y < /dev/null || true
  done

  # The dummies. The echo route injects one; the anthropic route's OAuth
  # document holds two more, and an expiry far ahead, so nothing renews it.
  local echo_secret dummy oauth_at oauth_rt cc_name=claude-credentials cc_mine=no
  echo_secret="e2e-echo-$(rand_hex 4)"
  dummy="e2e-dummy-$(rand_hex 16)"
  oauth_at="e2e-dummy-at-$(rand_hex 16)"
  oauth_rt="e2e-dummy-rt-$(rand_hex 16)"
  if bto 60 brig secret ls < /dev/null 2> /dev/null | awk '{ print $1 }' | grep -qx claude-credentials; then
    # Never replace a real login. The files: drop is then not checked.
    cc_name="e2e-cc-$(rand_hex 4)"
  else
    cc_mine=yes
  fi
  # The block runs in a subshell, so this ends with it, however it ends.
  # shellcheck disable=SC2064
  trap "broker_cleanup '$echo_secret' '$cc_name'" EXIT
  printf '%s' "$dummy" | bto 60 brig secret create "$echo_secret"
  printf '{"claudeAiOauth":{"accessToken":"%s","refreshToken":"%s","expiresAt":4102444800000,"scopes":["user:inference"]}}' \
    "$oauth_at" "$oauth_rt" | bto 60 brig secret create "$cc_name"
  echo "secrets: $echo_secret and $cc_name (dummies)"

  put_policy e2e-route << EOF
apiVersion: brig.sh/v1alpha1
kind: Policy
metadata:
  name: e2e-route
spec:
  egress:
    default: deny
  routes:
    - name: echo
      upstream: $ECHO_ORIGIN
      credential:
        secretRef: { name: $echo_secret }
      inject:
        headers:
          set: [{ name: X-E2e-Credential, value: "{value}" }]
          allow: [{ name: X-E2e-Beta, values: [e2e-reviewed] }]
      guest:
        env: { E2E_ROUTE_URL: "{route.url}", E2E_ROUTE_TOKEN: "{session.token}" }
      request:
        allow:
          methods: [GET]
          paths: [/headers]
    - name: anthropic
      preset: claude-code
      credential:
        secretRef: { name: $cc_name }
EOF
  cat "$POLICY_DIR/e2e-route.yaml"
  run 60 brig policy attach e2e-route claude-code -n rt < /dev/null || true
  run 60 brig policy attach e2e-route claude-code -n rt2 < /dev/null || true

  # check prints a row per route and the posture. It goes first, so it is
  # recorded however the boot goes.
  out="$(bto 60 brig policy check claude-code -n rt < /dev/null 2>&1)" || rc=$?
  echo "policy check claude-code -n rt: exit $rc"
  echo "$out"
  if [ "$rc" = 0 ] && echo "$out" | grep -q 'route echo .*mode endpoint' &&
     echo "$out" | grep -q 'route anthropic .*preset claude-code' && echo "$out" | grep -q '^posture: isolated'; then
    res check Policy "policy check reports each route, its mode and the posture (#516, #521)" pass \
      "exit 0: $(echo "$out" | grep -E '^ *route [a-z]|^posture:' | one_line 240)"
  else
    res check Policy "policy check reports each route, its mode and the posture (#516, #521)" fail \
      "exit $rc: $(echo "$out" | one_line 300)"
  fi

  # ------------------------------------------------ boot
  rc=0
  out="$(bto 900 brig run -d claude-code@rt ${BROKER_RUN_ARGS[@]+"${BROKER_RUN_ARGS[@]}"} < /dev/null 2>&1)" || rc=$?
  echo "brig run -d claude-code@rt: exit $rc"
  echo "$out"
  if [ "$rc" != 0 ]; then
    for n in "${all[@]}"; do
      [ "$n" = "$G_MISS" ] || res check Broker "$n" fail "brig run -d claude-code@rt exit $rc: $(echo "$out" | one_line 300)"
    done
    broker_missing_secret
    broker_cleanup "$echo_secret" "$cc_name"
    return 0
  fi

  # The guest's view, with nothing printed but lengths and names.
  local g
  g="$(GSH_QUIET=1 gsh claude-code@rt 'echo URL=$E2E_ROUTE_URL; echo TOK=$E2E_ROUTE_TOKEN; echo ABU=$ANTHROPIC_BASE_URL; echo CCT=$CLAUDE_CODE_OAUTH_TOKEN; echo AKEY=${ANTHROPIC_API_KEY:-UNSET}; echo SCF=$SSL_CERT_FILE; echo NEC=$NODE_EXTRA_CA_CERTS; echo CA=$(test -s /brig/ca/brig.pem && grep -c "BEGIN CERTIFICATE" /brig/ca/brig.pem); echo CRED=$(test -e /brig/claude/.credentials.json && echo present || echo absent); echo DNS=$(getent hosts brig.internal | awk "{ print \$1 }" | head -n 1); echo CURL=$(command -v curl || echo none)')" || true
  url="$(kv URL "$g")"
  tok="$(kv TOK "$g")"
  local abu cct akey scf nec ca cred dns
  abu="$(kv ABU "$g")"; cct="$(kv CCT "$g")"; akey="$(kv AKEY "$g")"
  scf="$(kv SCF "$g")"; nec="$(kv NEC "$g")"; ca="$(kv CA "$g")"; cred="$(kv CRED "$g")"; dns="$(kv DNS "$g")"
  sb="${url#https://brig.internal/}"
  sb="${sb%%/*}"
  local shape='^[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}$'
  local tok_ok=no cct_ok=no
  printf '%s\n' "$tok" | grep -Eq "$shape" && tok_ok=yes
  printf '%s\n' "$cct" | grep -Eq "$shape" && cct_ok=yes
  echo "guest: URL=$url ABU=$abu tokens: echo ${#tok} chars (shape $tok_ok), claude ${#cct} chars (shape $cct_ok);" \
    "ANTHROPIC_API_KEY=$akey SSL_CERT_FILE=$scf NODE_EXTRA_CA_CERTS=$nec CA certs=$ca .credentials.json $cred brig.internal=$dns curl=$(kv CURL "$g")"
  local drop_note="claude-credentials is not ours here, so the files: drop is not checked"
  local drop_ok=yes
  if [ "$cc_mine" = yes ]; then
    drop_note="/brig/claude/.credentials.json, which claude-code's files: binds from claude-credentials, is $cred"
    [ "$cred" = absent ] || drop_ok=no
  fi
  if [ "$url" = "https://brig.internal/$sb/r/echo" ] && [ -n "$sb" ] && [ "$tok_ok" = yes ] &&
     [ "$abu" = "https://brig.internal/$sb/r/anthropic" ] && [ "$cct_ok" = yes ] && [ "$tok" != "$cct" ] &&
     [ "$scf" = /brig/ca/bundle.pem ] && [ "$nec" = /brig/ca/brig.pem ] && [ "$ca" = 1 ] && [ -n "$dns" ] &&
     [ "$drop_ok" = yes ]; then
    res check Broker "$G_ROUTE" pass \
      "E2E_ROUTE_URL=$url, ANTHROPIC_BASE_URL=$abu, a session token in E2E_ROUTE_TOKEN and in CLAUDE_CODE_OAUTH_TOKEN; SSL_CERT_FILE=$scf, NODE_EXTRA_CA_CERTS=$nec; brig.internal is $dns; $drop_note"
  else
    res check Broker "$G_ROUTE" fail \
      "URL=[$url] ABU=[$abu] token shapes: echo $tok_ok, claude $cct_ok; SCF=[$scf] NEC=[$nec] CA=[$ca] brig.internal=[$dns]; $drop_note"
  fi

  # Every dummy is counted in what the guest can read, and the session token
  # in the same text is the control: a scan that finds the token can see.
  local dump hits ctl
  dump="$(GSH_TIMEOUT=600 GSH_QUIET=1 gsh claude-code@rt "$SCAN")" || true
  hits=$(( $(count_of "$dummy" "$dump") + $(count_of "$oauth_at" "$dump") + $(count_of "$oauth_rt" "$dump") ))
  ctl="$(count_of "$(printf '%s' "$dump" | sed -n 's/^E2E_ROUTE_TOKEN=//p' | head -n 1 | tr -d '\r')" "$dump")"
  echo "scan: ${#dump} bytes, dummy hits $hits, control (the scan's own session token) $ctl"
  if [ "$hits" = 0 ] && [ "${ctl:-0}" -ge 2 ] && [ "${#dump}" -gt 1000 ]; then
    res check Broker "$G_SCAN" pass \
      "0 hits of the 3 dummies in ${#dump} bytes: every process's environment and command line, /proc/cmdline, and the files under /brig, /root, /tmp, /run and /var/tmp. Control: the scan's own session token, $ctl hits"
  else
    res check Broker "$G_SCAN" fail "dummy hits $hits; control $ctl; ${#dump} bytes read"
  fi

  # ------------------------------------------------ requests
  local host_ctl r body code
  host_ctl="$(bto 30 curl -sS -m 20 -o /dev/null -w '%{http_code}' "$ECHO_ORIGIN/headers" 2>&1 || true)"
  echo "host control: $ECHO_ORIGIN/headers answers $host_ctl"
  r="$(GSH_QUIET=1 gsh claude-code@rt "$REQ; req GET \"\$E2E_ROUTE_URL/headers\" -H \"Authorization: Bearer \$E2E_ROUTE_TOKEN\" -H 'X-E2e-Beta: e2e-reviewed,e2e-unreviewed'")" || true
  code="$(kv HTTP "$r")"
  body="$(kv BODY "$r")"
  echo "GET /headers through the route: HTTP $code"
  echo "body, tokens withheld: $(printf '%s' "$body" | sed 's/ey[A-Za-z0-9_-]*\.[A-Za-z0-9_-]*/<token>/g' | cut -c1-600)"
  local fwd_tok fwd_dummy
  fwd_tok="$(count_of "$tok" "$body")"
  fwd_dummy="$(count_of "$dummy" "$body")"
  if [ "$code" = 200 ] && echo "$body" | grep -q '"x-e2e-credential":"\[redacted\]"' &&
     echo "$body" | grep -q '"x-e2e-beta":"e2e-reviewed"' && ! echo "$body" | grep -q -i '"authorization"' &&
     [ "$fwd_tok" = 0 ] && [ "$fwd_dummy" = 0 ]; then
    res check Broker "$G_FWD" pass \
      "200 from $ECHO_ORIGIN/headers through https://brig.internal/$sb/r/echo: the upstream got x-e2e-credential, which the broker redacted on the way back as [redacted]; no Authorization reached it; X-E2e-Beta kept e2e-reviewed and dropped e2e-unreviewed; neither the token nor the dummy is in the answer"
  elif [ "$code" = 502 ] && [ "$host_ctl" != 200 ]; then
    res check Broker "$G_FWD" skip "the upstream $ECHO_ORIGIN did not answer from the host either ($host_ctl), so nothing was measured; the route answered 502"
  else
    res check Broker "$G_FWD" fail \
      "HTTP $code; host control $host_ctl; token in the answer $fwd_tok, dummy $fwd_dummy: $(printf '%s' "$body" | sed 's/ey[A-Za-z0-9_-]*\.[A-Za-z0-9_-]*/<token>/g' | cut -c1-300)"
  fi

  # Outside request.allow: a path, then a method.
  local p_code p_body m_code
  r="$(GSH_QUIET=1 gsh claude-code@rt "$REQ; req GET \"\$E2E_ROUTE_URL/get\" -H \"Authorization: Bearer \$E2E_ROUTE_TOKEN\"; req POST \"\$E2E_ROUTE_URL/headers\" -H \"Authorization: Bearer \$E2E_ROUTE_TOKEN\"")" || true
  p_code="$(printf '%s\n' "$r" | sed -n 's/^HTTP=//p' | sed -n 1p)"
  m_code="$(printf '%s\n' "$r" | sed -n 's/^HTTP=//p' | sed -n 2p)"
  p_body="$(printf '%s\n' "$r" | sed -n 's/^BODY=//p' | sed -n 1p)"
  echo "GET /get: $p_code: $p_body"
  echo "POST /headers: $m_code"
  if [ "$p_code" = 403 ] && echo "$p_body" | grep -q 'rule: *spec.routes\[echo\].request.allow.paths' &&
     [ "$m_code" = 403 ] && grep -q 'check=request.allow.methods' "$BROKER_DIR/audit.log" &&
     grep -q 'check=request.allow.paths' "$BROKER_DIR/audit.log"; then
    res check Broker "$G_ALLOW" pass \
      "GET /get: 403, $(echo "$p_body" | grep -o 'rule: *[^ ]*' | head -n 1); POST /headers: 403; the audit log has check=request.allow.paths and check=request.allow.methods"
  else
    res check Broker "$G_ALLOW" fail "GET /get: [$p_code] $(echo "$p_body" | one_line 200); POST /headers: [$m_code]"
  fi

  # The token in the query, beside the carrier it belongs in.
  local c_code c_body
  r="$(GSH_QUIET=1 gsh claude-code@rt "$REQ; req GET \"\$E2E_ROUTE_URL/headers?q=\$E2E_ROUTE_TOKEN\" -H \"Authorization: Bearer \$E2E_ROUTE_TOKEN\"")" || true
  c_code="$(kv HTTP "$r")"
  c_body="$(kv BODY "$r")"
  echo "token in the query: $c_code: $c_body"
  if [ "$c_code" = 403 ] && echo "$c_body" | grep -q 'the session token was sent outside its carrier (route echo, in query' &&
     grep -q 'check=placeholder' "$BROKER_DIR/audit.log"; then
    res check Broker "$G_CARRIER" pass "403: $(echo "$c_body" | one_line 160); audit: check=placeholder"
  else
    res check Broker "$G_CARRIER" fail "[$c_code] $(echo "$c_body" | one_line 240)"
  fi

  # No token, and a token whose signature is changed. Both are 401, and the
  # audit log says which check refused each.
  local forged a_code f_code
  forged="${tok%?}$(if [ "${tok: -1}" = A ]; then echo B; else echo A; fi)"
  r="$(GSH_QUIET=1 gsh claude-code@rt "$REQ; req GET \"\$E2E_ROUTE_URL/headers\"; F='$forged'; req GET \"\$E2E_ROUTE_URL/headers\" -H \"Authorization: Bearer \$F\"")" || true
  a_code="$(printf '%s\n' "$r" | sed -n 's/^HTTP=//p' | sed -n 1p)"
  f_code="$(printf '%s\n' "$r" | sed -n 's/^HTTP=//p' | sed -n 2p)"
  echo "no token: $a_code; forged signature: $f_code"
  if [ "$a_code" = 401 ] && [ "$f_code" = 401 ] && grep -q 'check=token' "$BROKER_DIR/audit.log" &&
     grep -q 'check=signature' "$BROKER_DIR/audit.log"; then
    res check Broker "$G_BADTOK" pass "no token: 401 (check=token); the last character of the signature changed: 401 (check=signature)"
  else
    res check Broker "$G_BADTOK" fail "no token: [$a_code]; forged: [$f_code]"
  fi

  # Straight to the upstream: by name, and by an address the host resolves
  # it to, so the refusal is not only DNS.
  local host ip d_code i_code
  host="${ECHO_ORIGIN#https://}"
  host="${host%%:*}"
  ip="$(python3 -c 'import socket, sys; print(socket.gethostbyname(sys.argv[1]))' "$host" 2> /dev/null || true)"
  r="$(gsh claude-code@rt "$REQ; req GET https://$host/headers; req GET https://$host/headers --resolve $host:443:${ip:-127.0.0.2}")" || true
  d_code="$(printf '%s\n' "$r" | sed -n 's/^HTTP=//p' | sed -n 1p)"
  i_code="$(printf '%s\n' "$r" | sed -n 's/^HTTP=//p' | sed -n 2p)"
  echo "direct by name: $d_code; by $ip: $i_code; host control: $host_ctl"
  if [ "$host_ctl" = 200 ] && [ "${d_code%% *}" = 000 ] && [ "${i_code%% *}" = 000 ] && [ -n "$ip" ]; then
    res check Broker "$G_EGRESS" pass \
      "from the guest, https://$host/ fails by name ($(echo "$d_code" | one_line 80)) and at $ip ($(echo "$i_code" | one_line 80)); from the host it answers 200"
  elif [ "$host_ctl" != 200 ]; then
    res check Broker "$G_EGRESS" skip "the host itself got $host_ctl from $ECHO_ORIGIN, so a refusal from the guest proves nothing"
  else
    res check Broker "$G_EGRESS" fail "by name: [$d_code]; at [$ip]: [$i_code]"
  fi

  # ------------------------------------------------ brig info
  local info infoj pidf jpid jrun jtr jmodes jph
  info="$(bto 60 brig info claude-code@rt < /dev/null 2> /dev/null | grep -E '^BROKER' || true)"
  infoj="$(bto 60 brig info claude-code@rt --json < /dev/null 2> /dev/null || true)"
  pidf="$(head -n 1 "$BROKER_DIR/broker.pid" 2> /dev/null | awk '{ print $1 }' || true)"
  jpid="$(printf '%s' "$infoj" | field data.broker.pid 2> /dev/null || true)"
  jrun="$(printf '%s' "$infoj" | field data.broker.running 2> /dev/null || true)"
  jtr="$(printf '%s' "$infoj" | field data.broker.transport 2> /dev/null || true)"
  jmodes="$(printf '%s' "$infoj" | python3 -c 'import json, sys; print(",".join(json.load(sys.stdin)["data"]["broker"].get("modes") or []))' 2> /dev/null || true)"
  jph="$(printf '%s' "$infoj" | field data.broker.placeholder 2> /dev/null || true)"
  echo "info: $info"
  echo "info --json broker: running $jrun, pid $jpid, transport $jtr, modes $jmodes, placeholder $jph; broker.pid names $pidf"
  local leak
  leak=$(( $(count_of "$dummy" "$info$infoj") + $(count_of "$oauth_at" "$info$infoj") + $(count_of "$tok" "$info$infoj") ))
  if echo "$info" | grep -q 'transport brig.internal, routes echo, anthropic' && echo "$info" | grep -q "brig-broker pid $pidf on 127.0.0.1:" &&
     echo "$info" | grep -q 'modes endpoint' && [ "$jrun" = true ] && [ "$jpid" = "$pidf" ] && [ "$jtr" = brig.internal ] &&
     [ "$jmodes" = endpoint ] && [ "${jph:-0}" -ge 1 ] 2> /dev/null && [ "$leak" = 0 ]; then
    res check Broker "$G_INFO" pass \
      "$(echo "$info" | one_line 300). --json: running, pid $jpid as broker.pid says, transport $jtr, modes $jmodes, placeholder refusals $jph. No dummy or token in either"
  else
    res check Broker "$G_INFO" fail \
      "row: [$(echo "$info" | one_line 300)]; --json running [$jrun] pid [$jpid] (broker.pid [$pidf]) transport [$jtr] modes [$jmodes] placeholder [$jph]; dummy or token hits $leak"
  fi

  local tr_ev
  tr_ev="$(broker_transport "$sb")"
  echo "transport: $tr_ev"
  if [ -n "$tr_ev" ] && [ -S "$(divert_sock "$sb")" ]; then
    res check Broker "$G_TRANSPORT" pass "$tr_ev; divert socket $(basename "$(divert_sock "$sb")")"
  else
    res check Broker "$G_TRANSPORT" fail "runtime: [$tr_ev]; divert socket $(divert_sock "$sb") is $(ls -l "$(divert_sock "$sb")" 2>&1)"
  fi

  # ------------------------------------------------ lifecycle
  local p1 p2 n2
  p1="$(broker_pids | paste -s -d, -)"
  run 900 brig run -d claude-code@rt2 ${BROKER_RUN_ARGS[@]+"${BROKER_RUN_ARGS[@]}"} < /dev/null || true
  p2="$(broker_pids | paste -s -d, -)"
  n2="$(broker_status | python3 -c 'import json, sys; print(len(json.load(sys.stdin).get("sandboxes") or []))' 2> /dev/null || echo '?')"
  echo "brig-broker before the second sandbox: [$p1]; after: [$p2]; sandboxes registered: $n2"
  if [ -n "$p1" ] && [ "$p1" = "$p2" ] && [ "$p1" = "$pidf" ] && [ "$n2" = 2 ]; then
    res check Broker "$G_ONE" pass "pid $p1 before and after a second routed sandbox, and its /status lists 2 sandboxes"
  else
    res check Broker "$G_ONE" fail "brig-broker before [$p1], after [$p2], broker.pid [$pidf]; sandboxes $n2"
  fi

  # The token of the first boot, from a guest of the next one. A token that
  # still verifies is refused by request.allow with 403, so the control is
  # the new guest's own token on the same path.
  local o_code n_code
  run 300 brig stop claude-code@rt < /dev/null || true
  local stopped_sock=present
  [ -S "$(divert_sock "$sb")" ] || stopped_sock=gone
  run 900 brig run -d claude-code@rt ${BROKER_RUN_ARGS[@]+"${BROKER_RUN_ARGS[@]}"} < /dev/null || true
  r="$(GSH_QUIET=1 gsh claude-code@rt "$REQ; O='$tok'; req GET \"\$E2E_ROUTE_URL/get\" -H \"Authorization: Bearer \$O\"; req GET \"\$E2E_ROUTE_URL/get\" -H \"Authorization: Bearer \$E2E_ROUTE_TOKEN\"")" || true
  o_code="$(printf '%s\n' "$r" | sed -n 's/^HTTP=//p' | sed -n 1p)"
  n_code="$(printf '%s\n' "$r" | sed -n 's/^HTTP=//p' | sed -n 2p)"
  echo "after stop and a new boot: divert socket while stopped $stopped_sock; old token $o_code; new token $n_code"
  if [ "$o_code" = 401 ] && [ "$n_code" = 403 ] && [ "$stopped_sock" = gone ]; then
    res check Broker "$G_STOP" pass "the divert socket went with brig stop; in the new guest, the first boot's token: 401, its own: 403 (verified, then refused by request.allow)"
  else
    res check Broker "$G_STOP" fail "old token [$o_code], new token [$n_code]; divert socket while stopped: $stopped_sock"
  fi

  local st_after sock_after alive
  run 300 brig rm claude-code@rt < /dev/null || true
  st_after="$(broker_status)"
  sock_after=present
  [ -S "$(divert_sock "$sb")" ] || sock_after=gone
  alive="$(broker_pids | paste -s -d, -)"
  echo "after rm: divert socket $sock_after; brig-broker [$alive]; status: $(echo "$st_after" | one_line 200)"
  if [ "$sock_after" = gone ] && ! echo "$st_after" | grep -q "\"$sb\"" && echo "$st_after" | grep -q 'brig-claude-code-rt2' && [ -n "$alive" ]; then
    res check Broker "$G_RM" pass "the divert socket is gone, /status no longer lists $sb, and the broker stays up for claude-code@rt2"
  else
    res check Broker "$G_RM" fail "divert socket $sock_after; brig-broker [$alive]; /status: $(echo "$st_after" | one_line 200)"
  fi

  local waited=0 left lastrow
  run 300 brig rm claude-code@rt2 < /dev/null || true
  while [ -n "$(broker_pids)" ] && [ "$waited" -lt 30 ]; do
    sleep 1
    waited=$((waited + 1))
  done
  left="$(broker_pids | paste -s -d, -)"
  lastrow="$(bto 60 brig info claude-code@rt < /dev/null 2> /dev/null | grep -E '^BROKER' || true)"
  echo "after the last rm: brig-broker [$left] after $waited s; broker.pid $(ls "$BROKER_DIR/broker.pid" 2>&1); $lastrow"
  if [ -z "$left" ] && [ ! -e "$BROKER_DIR/broker.pid" ] && echo "$lastrow" | grep -q 'brig-broker not running'; then
    res check Broker "$G_LAST" pass "no brig-broker $waited s after the last brig rm, no broker.pid, and brig info says: brig-broker not running; the run starts it"
  else
    res check Broker "$G_LAST" fail "brig-broker [$left] after $waited s; broker.pid: $(ls "$BROKER_DIR/broker.pid" 2>&1); row: $(echo "$lastrow" | one_line 200)"
  fi

  # The broker logs never hold a dummy or a token.
  local logs_hits
  logs_hits=$(( $(count_of "$dummy" "$(cat "$BROKER_DIR"/audit.log* "$BROKER_DIR"/broker.log* 2> /dev/null)") +
    $(count_of "$tok" "$(cat "$BROKER_DIR"/audit.log* "$BROKER_DIR"/broker.log* 2> /dev/null)") ))
  echo "audit.log and broker.log: $logs_hits hits of the dummy or the token"
  echo "audit.log, the lines of this run:"
  grep -E 'sandbox=brig-claude-code-rt|arrived=brig-claude-code-rt' "$BROKER_DIR/audit.log" 2> /dev/null | tail -n 30 || true
  if [ "$logs_hits" = 0 ] && [ -s "$BROKER_DIR/audit.log" ]; then
    res check Broker "$G_LOGS" pass "0 hits of the echo dummy or the first session token in audit.log and broker.log, $(wc -l < "$BROKER_DIR/audit.log" | tr -d ' ') audit lines"
  else
    res check Broker "$G_LOGS" fail "$logs_hits hits of the dummy or the token; audit.log: $(ls -l "$BROKER_DIR/audit.log" 2>&1)"
  fi

  broker_missing_secret
  broker_cleanup "$echo_secret" "$cc_name"
}

# broker_missing_secret: a route whose secret the store does not hold is
# refused before boot, with exit 6 and the command that stores it.
broker_missing_secret() {
  local miss rc=0 out ls
  miss="e2e-missing-$(rand_hex 4)"
  sed -e 's/name: e2e-route/name: e2e-miss/' -e "s/secretRef: { name: e2e-echo-[0-9a-f]* }/secretRef: { name: $miss }/" \
    "$POLICY_DIR/e2e-route.yaml" | put_policy e2e-miss
  run 60 brig policy attach e2e-miss claude-code -n miss < /dev/null || true
  out="$(bto 300 brig run -d claude-code@miss ${BROKER_RUN_ARGS[@]+"${BROKER_RUN_ARGS[@]}"} < /dev/null 2>&1)" || rc=$?
  echo "brig run -d claude-code@miss: exit $rc: $out"
  ls="$(bto 60 brig ls -q < /dev/null 2> /dev/null | grep 'miss' || true)"
  run 60 brig policy detach e2e-miss claude-code -n miss < /dev/null || true
  rm -f "$POLICY_DIR/e2e-miss.yaml"
  bto 60 brig rm claude-code@miss < /dev/null > /dev/null 2>&1 || true
  if [ "$rc" = 6 ] && echo "$out" | grep -q "does not hold" && echo "$out" | grep -q -F "$miss" && [ -z "$ls" ]; then
    res check Broker "A run whose route secret is not in the store is refused with exit 6 (#519)" pass \
      "exit 6, nothing booted: $(echo "$out" | grep 'does not hold' | one_line 240)"
  else
    res check Broker "A run whose route secret is not in the store is refused with exit 6 (#519)" fail \
      "exit $rc; brig ls: [$ls]; $(echo "$out" | one_line 300)"
  fi
}

# broker_cleanup SECRET...: what check_broker made, whatever happened.
broker_cleanup() {
  local s
  rm_route_state
  for s in "$@"; do
    bto 60 brig secret delete "$s" -y < /dev/null || true
  done
  broker_store_down || true
}
