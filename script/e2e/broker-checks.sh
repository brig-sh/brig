# shellcheck shell=bash
# The checks of policy documents and of the credential broker (#516-#521),
# on a real runtime. canary-linux.sh and canary-macos.sh source this file, so
# both hosts run one copy. Written for the bash 3.2 that macOS ships.
#
# The canary that sources it supplies res, run, one_line and field.

# Guest command text is single-quoted, so that the guest's shell expands it.
# shellcheck disable=SC2016

POLICY_DIR="${BRIG_POLICY_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/brig/policies}"

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

# put_policy NAME: writes stdin as the policy file NAME.yaml.
put_policy() {
  mkdir -p "$POLICY_DIR"
  cat > "$POLICY_DIR/$1.yaml"
}

# rm_route_state: what an earlier, interrupted run of these checks left, so
# a run starts from nothing.
rm_route_state() {
  local s p
  for p in e2e-relay e2e-flat e2e-env; do
    for s in relay flat; do
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
