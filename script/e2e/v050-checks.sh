# shellcheck shell=bash
# The checks of what landed on main between v0.4.0 and v0.5.x: the spellings
# and profile keys v0.5.0 removed, BRIG_PROJECT and the .git watch, client
# certificates on a route, and the aws and gcp presets. canary-linux.sh and
# canary-macos.sh source this file, so both hosts run one copy. Written for
# the bash 3.2 that macOS ships.
#
# The canary that sources it supplies res, run, gsh, one_line and
# BROKER_RUN_ARGS, and broker-checks.sh supplies bto, rand_hex, put_policy,
# rm_route_state, broker_store_up, broker_store_down, POLICY_DIR and
# ECHO_ORIGIN, so this file is sourced after it.
#
# The certificates here are drawn by openssl for this run and thrown away.
# None is a credential anywhere, and the guest never sees a key: the checks
# say so by looking.
# Guest command text is single-quoted, so that the guest's shell expands it.
# shellcheck disable=SC2016

# ------------------------------------------------------------- v0.5.0 removals

# check_removed_spellings: each verb v0.5.0 retired is refused before any
# profile loads, as a usage error that names the verb that replaced it; and
# a profile that still carries a key v0.5.0 removed is unusable, with the key
# and its replacement named. Nothing boots.
check_removed_spellings() {
  local G_VERBS="The spellings v0.5.0 removed exit 2 and name their replacement (23e2dd0)"
  local G_KEYS="A profile with a key v0.5.0 removed is unusable, and the hint names the replacement (168e543)"
  local verb want out rc bad="" ok=0
  # verb=the word the refusal must name, from removedVerbs in cmd/brig/main.go.
  for verb in profiles=agent agents=agent create=run env=info reset=rm policies=policy import=agent export=agent; do
    rc=0
    out="$(bto 60 brig "${verb%%=*}" ubuntu < /dev/null 2>&1)" || rc=$?
    echo "brig ${verb%%=*} ubuntu: exit $rc: $(echo "$out" | one_line 160)"
    if [ "$rc" = 2 ] && echo "$out" | grep -q 'was removed' && echo "$out" | grep -q "brig ${verb#*=}"; then
      ok=$((ok + 1))
    else
      bad="$bad ${verb%%=*}:exit=$rc"
    fi
  done
  if [ -z "$bad" ]; then
    res check CLI "$G_VERBS" pass "$ok verbs refused with exit 2, each naming its replacement"
  else
    res check CLI "$G_VERBS" fail "wrong:$bad; last: $(echo "$out" | one_line 200)"
  fi

  local w f ls rc2=0
  w="$(bto 60 brig agent export ubuntu e2e-old --force < /dev/null 2>&1)" || true
  f="$(echo "$w" | sed -n 's/^wrote .* -> //p' | head -n 1)"
  if [ -z "$f" ] || [ ! -f "$f" ]; then
    res check Profile "$G_KEYS" fail "agent export wrote no file: $(echo "$w" | one_line 200)"
    return 0
  fi
  printf '%s\n' 'statePaths:' '  - .e2e-old' >> "$f"
  ls="$(bto 60 brig agent ls < /dev/null 2>&1)" || rc2=$?
  echo "agent ls with statePaths: exit $rc2: $(echo "$ls" | one_line 400)"
  rm -f "$f"
  if echo "$ls" | grep -q 'statePaths: was removed in v0.5.0; use volumes:'; then
    res check Profile "$G_KEYS" pass "$(echo "$ls" | grep 'statePaths' | one_line 200)"
  else
    res check Profile "$G_KEYS" fail "exit $rc2: $(echo "$ls" | one_line 300)"
  fi
}

# ------------------------------------------------------- BRIG_PROJECT and .git

# check_project_git: a run with a project gets BRIG_PROJECT, the guest path
# of the project, and no other value can take that name (e26d9fb). A hook
# the guest writes under the project's .git is named by the next brig stop,
# with the advice to read it before git runs it (15398a0); the warning is
# printed once, and the rm after it says nothing. One ubuntu boot, removed
# by the check itself.
check_project_git() {
  local G_VAR="BRIG_PROJECT names the guest path of the project, and nothing else can set it (e26d9fb)"
  local G_WATCH="A hook the guest writes under the project's .git is named by the next brig stop (15398a0)"
  local G_ONCE="The .git warning is printed once, and a command that finds .git unchanged says nothing (15398a0)"
  local proj base rc=0 out g v w st rmo f=""
  proj="$(mktemp -d "${TMPDIR:-/tmp}/e2e-proj.XXXXXX")"
  base="$(basename "$proj")"
  (
    cd "$proj" && git init -q . && echo hello > README &&
      git -c user.name=e2e -c user.email=e2e@example.invalid add README &&
      git -c user.name=e2e -c user.email=e2e@example.invalid commit -q -m first
  )
  bto 300 brig rm e2e-proj@proj < /dev/null > /dev/null 2>&1 || true

  # A profile binding named BRIG_PROJECT must be dropped with a warning.
  w="$(bto 60 brig agent export ubuntu e2e-proj --force < /dev/null 2>&1)" || true
  f="$(echo "$w" | sed -n 's/^wrote .* -> //p' | head -n 1)"
  # The export already has an env: list (GH_TOKEN), so the binding goes
  # under it; a second env: key is a YAML error, not a binding.
  if [ -n "$f" ] && [ -f "$f" ]; then
    awk '{ print } /^env:$/ { print "  - name: BRIG_PROJECT"; print "    ref: env.HOME" }' "$f" > "$f.e2e" && mv "$f.e2e" "$f"
    grep -q '^env:$' "$f" || printf '%s\n' 'env:' '  - name: BRIG_PROJECT' '    ref: env.HOME' >> "$f"
  fi
  # shellcheck disable=SC2086
  out="$(bto 900 brig run -d ${BROKER_RUN_ARGS[@]+"${BROKER_RUN_ARGS[@]}"} e2e-proj@proj "$proj" < /dev/null 2>&1)" || rc=$?
  echo "brig run -d e2e-proj@proj $proj: exit $rc: $(echo "$out" | one_line 300)"
  g="$(gsh e2e-proj@proj 'echo P=$BRIG_PROJECT; test -d "$BRIG_PROJECT" && echo DIR=yes; cat "$BRIG_PROJECT/README"')" || true
  v="$(printf '%s\n' "$g" | sed -n 's/^P=//p' | head -n 1)"
  if [ "$rc" = 0 ] && [ "$v" = "/work/$base" ] && echo "$g" | grep -qx 'DIR=yes' && echo "$g" | grep -qx hello &&
     echo "$out" | grep -q 'BRIG_PROJECT' && echo "$out" | grep -q 'where the project is mounted'; then
    res check Guest "$G_VAR" pass "BRIG_PROJECT=$v, README read; the profile's own BRIG_PROJECT was dropped: $(echo "$out" | grep 'BRIG_PROJECT' | head -n 1 | one_line 160)"
  else
    res check Guest "$G_VAR" fail "exit $rc; guest: [$(echo "$g" | one_line 160)]; run said: $(echo "$out" | one_line 240)"
  fi

  # run, sh, exec, stop and rm look at .git before they act (checksProjectGit
  # in cmd/brig/main.go); ls does not. The run took the first look, the sh
  # that writes the hook compares before the write and so says nothing, the
  # stop after it names the hook, and the rm after that finds the baseline
  # moved and says nothing. docs/security.md shows the stop.
  w="$(gsh e2e-proj@proj 'printf "#!/bin/sh\necho hooked\n" > "$BRIG_PROJECT/.git/hooks/pre-commit"; chmod +x "$BRIG_PROJECT/.git/hooks/pre-commit"; echo wrote')" || true
  echo "the sh that wrote the hook said: $(echo "$w" | one_line 200)"
  # What the host sees of the hook, and the look brig keeps, so a silent
  # stop can be told apart from a hook that never reached the host.
  local onhost=absent looked=none
  [ -x "$proj/.git/hooks/pre-commit" ] && onhost=present
  ls -la "$proj/.git/hooks/pre-commit" 2>&1 || true
  if [ -f "${BRIG_STATE_DIR:-$HOME/.brig}/git-watch.json" ]; then
    looked="$(grep -c '"project"' "${BRIG_STATE_DIR:-$HOME/.brig}/git-watch.json" || true) sandbox(es)"
    head -c 800 "${BRIG_STATE_DIR:-$HOME/.brig}/git-watch.json"; echo
  fi
  st="$(bto 300 brig stop e2e-proj@proj < /dev/null 2>&1)" || true
  echo "brig stop after the hook: $(echo "$st" | one_line 400)"
  rmo="$(bto 300 brig rm e2e-proj@proj < /dev/null 2>&1)" || true
  echo "brig rm after the stop: $(echo "$rmo" | one_line 300)"
  if echo "$st" | grep -q '/.git changed while .* could write to it' &&
     echo "$st" | grep -q 'hooks/pre-commit' && echo "$st" | grep -q 'runs these files as you'; then
    res check Guest "$G_WATCH" pass "$(echo "$st" | grep -E 'changed while|pre-commit' | one_line 240)"
  else
    res check Guest "$G_WATCH" fail "brig stop said: [$(echo "$st" | one_line 200)]; the hook is $onhost on the host, and brig had recorded a look for $looked"
  fi
  if [ "$rc" != 0 ] || [ "$onhost" != present ]; then
    res check Guest "$G_ONCE" fail "not exercised: run exit $rc, hook $onhost on the host"
  elif ! echo "$w" | grep -q '.git changed' && ! echo "$rmo" | grep -q '.git changed'; then
    res check Guest "$G_ONCE" pass "nothing from the sh that wrote the hook, nothing from the rm after the stop that named it"
  else
    res check Guest "$G_ONCE" fail "sh: $(echo "$w" | grep '.git changed' | one_line 120); rm: $(echo "$rmo" | grep '.git changed' | one_line 120)"
  fi
  [ -z "$f" ] || rm -f "$f"
  rm -rf "$proj"
}

# ----------------------------------------------------------- client certificates

# e2e_mkcert NAME DAYS DIR: a self-signed certificate and its key, valid for
# DAYS days, together in DIR/NAME.pem, the way --type tls reads them.
e2e_mkcert() {
  local name=$1 days=$2 dir=$3
  openssl req -x509 -newkey rsa:2048 -nodes -days "$days" -subj "/CN=$name" \
    -keyout "$dir/$name.key" -out "$dir/$name.crt" > /dev/null 2>&1
  cat "$dir/$name.crt" "$dir/$name.key" > "$dir/$name.pem"
  rm -f "$dir/$name.key" "$dir/$name.crt"
}

# e2e_mkca NAME DIR: a self-signed CA certificate, alone, as DIR/NAME.pem.
# --type ca wants CA:TRUE in the basic constraints, which a plain req -x509
# leaves out.
e2e_mkca() {
  local name=$1 dir=$2
  openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=$name" \
    -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" \
    -keyout "$dir/$name.key" -out "$dir/$name.pem" > /dev/null 2>&1
  rm -f "$dir/$name.key"
}

# check_client_certs: --type tls and --type ca check what they store and
# refuse what they cannot (07a5e5f, 9e63487); a route with inject.tls loads
# and policy check prints its tls row with the secret names (dc64d6d); the
# documents brig must refuse around tls are refused (9bfdfc8); and a run with
# a route whose client certificate expires within 3 days says so at boot,
# while the key stays out of the guest (9d9e4e9, 01f57ef). One claude-code
# boot, removed at the end. Skipped without a secret store.
check_client_certs() {
  local G_TYPE="brig secret --type tls and --type ca check the value, and refuse one that is not a certificate (07a5e5f)"
  local G_ROW="A route with inject.tls loads, and policy check prints its tls row (dc64d6d)"
  local G_REFUSE="tls on a preset, or beside a credential it cannot carry, is refused (9bfdfc8)"
  local G_EXPIRY="A run warns of a route's client certificate expiring within 3 days, and the key never reaches the guest (9d9e4e9)"
  local all=("$G_TYPE" "$G_ROW" "$G_REFUSE" "$G_EXPIRY")
  local why n d cc soon ca bad rc=0 rc2=0 rc_ca=0 rc_bad=0 rc_chk=0 out out2 ca_out bad_out ls chk missing=""
  if ! command -v openssl > /dev/null 2>&1; then
    for n in "${all[@]}"; do res check Broker "$n" skip "no openssl on this host"; done
    return 0
  fi
  if ! why="$(broker_store_up)"; then
    echo "no secret store: $why"
    for n in "${all[@]}"; do res check Broker "$n" skip "no secret store brig can write to on this host: $why"; done
    return 0
  fi
  echo "$why"
  unset GH_TOKEN
  rm_route_state
  bto 300 brig rm claude-code@tls < /dev/null > /dev/null 2>&1 || true
  for n in $(bto 60 brig secret ls < /dev/null 2> /dev/null | awk '$1 ~ /^e2e-(cert|soon|ca|notcert)-[0-9a-f]+$/ { print $1 }'); do
    bto 60 brig secret delete "$n" -y < /dev/null || true
  done
  d="$(mktemp -d "${TMPDIR:-/tmp}/e2e-pki.XXXXXX")"
  chmod 700 "$d"
  cc="e2e-cert-$(rand_hex 4)"; soon="e2e-soon-$(rand_hex 4)"; ca="e2e-ca-$(rand_hex 4)"; bad="e2e-notcert-$(rand_hex 4)"
  e2e_mkcert "$cc" 365 "$d"
  e2e_mkcert "$soon" 2 "$d"
  e2e_mkca "$ca" "$d"

  out="$(bto 60 brig secret create "$cc" --type tls -f "$d/$cc.pem" < /dev/null 2>&1)" || rc=$?
  echo "secret create $cc --type tls: exit $rc: $(echo "$out" | one_line 300)"
  out2="$(bto 60 brig secret create "$soon" --type tls -f "$d/$soon.pem" < /dev/null 2>&1)" || rc2=$?
  echo "secret create $soon --type tls: exit $rc2: $(echo "$out2" | one_line 300)"
  ca_out="$(bto 60 brig secret create "$ca" --type ca -f "$d/$ca.pem" < /dev/null 2>&1)" || rc_ca=$?
  echo "secret create $ca --type ca: exit $rc_ca: $(echo "$ca_out" | one_line 200)"
  bad_out="$(printf 'not a certificate %s\n' "$(rand_hex 8)" | bto 60 brig secret create "$bad" --type tls 2>&1)" || rc_bad=$?
  echo "secret create $bad --type tls with a non-certificate: exit $rc_bad: $(echo "$bad_out" | one_line 200)"
  ls="$(bto 60 brig secret ls < /dev/null 2> /dev/null || true)"
  if [ "$rc" = 0 ] && echo "$out" | grep -q '1 certificate(s) will be sent' && [ "$rc2" = 0 ] && [ "$rc_ca" = 0 ] &&
     [ "$rc_bad" != 0 ] && echo "$bad_out" | grep -q 'the value is not a client certificate' &&
     echo "$ls" | grep -qw "$cc" && echo "$ls" | grep -qw "$ca" && ! echo "$ls" | grep -qw "$bad"; then
    res check Broker "$G_TYPE" pass "tls: $(echo "$out" | grep 'will be sent' | one_line 120); ca stored; a non-certificate: exit $rc_bad, not stored"
  else
    res check Broker "$G_TYPE" fail "tls exit $rc: $(echo "$out" | one_line 160); soon exit $rc2; ca exit $rc_ca; non-certificate exit $rc_bad: $(echo "$bad_out" | one_line 160); ls: $(echo "$ls" | one_line 120)"
  fi

  # The route: certificate-only, so it takes no credential. The upstream is
  # the echo service, which these checks never have to reach.
  local origin_host
  origin_host="${ECHO_ORIGIN#https://}"
  put_policy e2e-tls << EOF
apiVersion: brig.sh/v1alpha1
kind: Policy
metadata:
  name: e2e-tls
  description: e2e, a route with a client certificate
spec:
  egress:
    default: deny
  routes:
    - name: mtls
      upstream: https://$origin_host
      inject:
        tls:
          clientCertificate: { secretRef: { name: $soon } }
          ca: { secretRef: { name: $ca } }
      guest:
        env: { E2E_MTLS_URL: "{route.url}", E2E_MTLS_TOKEN: "{session.token}" }
      request:
        allow:
          methods: [GET]
          paths: ["/headers"]
EOF
  # policy check reads a profile's bindings, so the policy is attached first;
  # the boot below runs under the same binding. Its exit status says whether
  # this host can enforce the route, which the boot check answers; the row
  # is what this check is about.
  run 60 brig policy attach e2e-tls claude-code -n tls || true
  chk="$(bto 60 brig policy check claude-code -n tls < /dev/null 2>&1)" || rc_chk=$?
  echo "policy check claude-code -n tls: exit $rc_chk: $(echo "$chk" | one_line 400)"
  ls="$(bto 60 brig policy ls < /dev/null 2>&1 || true)"
  if echo "$chk" | grep -q "tls  clientCertificate $soon  ca $ca" && echo "$ls" | grep -qE "^e2e-tls( |$)"; then
    res check Broker "$G_ROW" pass "$(echo "$chk" | grep 'tls  clientCertificate' | one_line 160)"
  else
    res check Broker "$G_ROW" fail "exit $rc_chk: $(echo "$chk" | one_line 300); policy ls: $(echo "$ls" | grep e2e-tls | one_line 120)"
  fi

  # The refusals, each worded by api/policy/v1alpha1/routevalidate.go.
  local name want
  for name in preset cred only-ca empty; do
    case "$name" in
      preset)
        want="a route with inject.tls takes no preset"
        put_policy "e2e-bad-tls-$name" << EOF
apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: { name: e2e-bad-tls-$name }
spec:
  routes:
    - name: r
      preset: anthropic-api-key
      credential: { secretRef: { name: $cc } }
      inject: { tls: { ca: { secretRef: { name: $ca } } } }
EOF
        ;;
      cred)
        want="credential is set, and inject.tls.clientCertificate does not carry it"
        put_policy "e2e-bad-tls-$name" << EOF
apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: { name: e2e-bad-tls-$name }
spec:
  routes:
    - name: r
      upstream: https://$origin_host
      credential: { secretRef: { name: $cc } }
      inject: { tls: { clientCertificate: { secretRef: { name: $cc } } } }
      guest: { env: { U: "{route.url}", T: "{session.token}" } }
EOF
        ;;
      only-ca)
        want="credential is set, and inject.tls with only ca does not carry it"
        put_policy "e2e-bad-tls-$name" << EOF
apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: { name: e2e-bad-tls-$name }
spec:
  routes:
    - name: r
      upstream: https://$origin_host
      credential: { secretRef: { name: $cc } }
      inject: { tls: { ca: { secretRef: { name: $ca } } } }
      guest: { env: { U: "{route.url}", T: "{session.token}" } }
EOF
        ;;
      empty)
        want="inject.tls needs clientCertificate or ca"
        put_policy "e2e-bad-tls-$name" << EOF
apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: { name: e2e-bad-tls-$name }
spec:
  routes:
    - name: r
      upstream: https://$origin_host
      inject: { tls: {} }
      guest: { env: { U: "{route.url}", T: "{session.token}" } }
EOF
        ;;
    esac
    out="$(bto 60 brig policy ls < /dev/null 2>&1 || true)"
    if echo "$out" | grep -q "e2e-bad-tls-$name" && echo "$out" | grep -qF "$want"; then
      echo "e2e-bad-tls-$name: refused: $(echo "$out" | grep -F "$want" | one_line 160)"
    else
      missing="$missing $name"
      echo "e2e-bad-tls-$name: wanted [$want]; policy ls said: $(echo "$out" | grep "e2e-bad-tls-$name" | one_line 200)"
    fi
    rm -f "$POLICY_DIR/e2e-bad-tls-$name.yaml"
  done
  if [ -z "$missing" ]; then
    res check Broker "$G_REFUSE" pass "4 documents refused, each with its reason: a preset, a credential beside clientCertificate, one beside ca alone, and an empty tls"
  else
    res check Broker "$G_REFUSE" fail "not refused as expected:$missing"
  fi

  # The boot. The route's certificate expires in 2 days, so the run warns
  # before the guest is up. The year-long one is in the store too, and must
  # not be named.
  rc=0
  # shellcheck disable=SC2086
  out="$(bto 900 brig run -d ${BROKER_RUN_ARGS[@]+"${BROKER_RUN_ARGS[@]}"} claude-code@tls < /dev/null 2>&1)" || rc=$?
  echo "brig run -d claude-code@tls: exit $rc: $(echo "$out" | one_line 400)"
  # The guest is searched for this run's own key, by 40 characters of its
  # body, not for the words PRIVATE KEY: an image carries those words in
  # its own files (a TLS library's test data, say), and the promise is about
  # this key. The mark goes to the guest in the command text and no further;
  # GSH_QUIET keeps the exchange out of the log.
  local g keys toklen url mark
  mark="$(sed -n '/BEGIN PRIVATE KEY/,/END PRIVATE KEY/p' "$d/$soon.pem" | sed -n 3p | cut -c1-40)"
  g="$(GSH_QUIET=1 gsh claude-code@tls 'echo URL=$E2E_MTLS_URL; echo TOKLEN=${#E2E_MTLS_TOKEN}; echo KEYS=$(grep -rlF "'"$mark"'" /brig /root /tmp /run /var/tmp /etc /home 2> /dev/null | wc -l); echo ENVKEYS=$(env | grep -cF "'"$mark"'")')" || true
  keys="$(printf '%s\n' "$g" | sed -n 's/^KEYS=//p' | tr -d ' ' | head -n 1)"
  toklen="$(printf '%s\n' "$g" | sed -n 's/^TOKLEN=//p' | tr -d ' ' | head -n 1)"
  url="$(printf '%s\n' "$g" | sed -n 's/^URL=//p' | head -n 1)"
  echo "guest: URL=$url TOKLEN=$toklen files holding this run's key: [$keys] env: [$(printf '%s\n' "$g" | sed -n 's/^ENVKEYS=//p' | head -n 1)]"
  if [ "$rc" = 0 ] && echo "$out" | grep -q "the client certificate $soon (route mtls) expires in" &&
     echo "$out" | grep -q "brig secret update $soon --type tls" && ! echo "$out" | grep -q "the client certificate $cc" &&
     [ "$keys" = 0 ] && [ "${url#https://brig.internal}" != "$url" ] && [ "${toklen:-0}" -gt 20 ]; then
    res check Broker "$G_EXPIRY" pass "$(echo "$out" | grep 'expires in' | one_line 160); the guest holds the route URL and a token, and no file or variable carries the key"
  else
    res check Broker "$G_EXPIRY" fail "exit $rc; keys in guest: [$keys]; run said: $(echo "$out" | one_line 300); guest: $(echo "$g" | one_line 120)"
  fi
  run 300 brig rm claude-code@tls || true
  bto 60 brig policy detach e2e-tls claude-code -n tls < /dev/null > /dev/null 2>&1 || true
  rm -f "$POLICY_DIR/e2e-tls.yaml"
  for n in "$cc" "$soon" "$ca"; do bto 60 brig secret delete "$n" -y < /dev/null > /dev/null 2>&1 || true; done
  rm -rf "$d"
  broker_store_down || true
}

# ----------------------------------------------------------------- presets

# check_vend_presets: the aws and gcp presets ship and print with their
# modes; a gcp route with a list of services loads; and a gcp route with a
# service brig does not know, or services on another preset, is refused
# (7889b34, eac9a1d). Nothing boots, and no cloud is reached.
check_vend_presets() {
  local G_SHOW="brig policy show --preset prints the aws and gcp presets with their modes (7889b34, eac9a1d)"
  local G_GCP="A gcp route with services loads, and an unknown service or services off the gcp preset is refused (eac9a1d)"
  local p out rc bad="" ls good badsvc badpre
  for p in aws gcp; do
    rc=0
    out="$(bto 60 brig policy show --preset "$p" < /dev/null 2>&1)" || rc=$?
    echo "policy show --preset $p: exit $rc: $(echo "$out" | one_line 300)"
    if [ "$rc" != 0 ] || ! echo "$out" | grep -q '^modes:' || ! echo "$out" | grep -q 'endpoint'; then
      bad="$bad $p:exit=$rc"
    fi
  done
  if [ -z "$bad" ]; then
    res check Broker "$G_SHOW" pass "aws and gcp print, each with modes: and endpoint"
  else
    res check Broker "$G_SHOW" fail "wrong:$bad; last: $(echo "$out" | one_line 200)"
  fi

  rm -f "$POLICY_DIR"/e2e-gcp*.yaml
  put_policy e2e-gcp << 'EOF'
apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: { name: e2e-gcp }
spec:
  routes:
    - name: gcp
      preset: gcp
      credential: { secretRef: { name: e2e-gcp-none } }
      services:
        - name: storage
        - name: compute
EOF
  put_policy e2e-gcp-bad-service << 'EOF'
apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: { name: e2e-gcp-bad-service }
spec:
  routes:
    - name: gcp
      preset: gcp
      credential: { secretRef: { name: e2e-gcp-none } }
      services:
        - name: not-a-google-service
EOF
  put_policy e2e-gcp-bad-preset << 'EOF'
apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: { name: e2e-gcp-bad-preset }
spec:
  routes:
    - name: aws
      preset: aws
      credential: { secretRef: { name: e2e-gcp-none } }
      services:
        - name: storage
EOF
  ls="$(bto 60 brig policy ls < /dev/null 2>&1 || true)"
  echo "policy ls: $(echo "$ls" | grep e2e-gcp | one_line 600)"
  good="$(echo "$ls" | grep -E '^e2e-gcp( |$)' || true)"
  badsvc="$(echo "$ls" | grep 'e2e-gcp-bad-service' | grep 'not-a-google-service' || true)"
  badpre="$(echo "$ls" | grep 'e2e-gcp-bad-preset' | grep 'services' || true)"
  rm -f "$POLICY_DIR"/e2e-gcp*.yaml
  if [ -n "$good" ] && [ -n "$badsvc" ] && [ -n "$badpre" ]; then
    res check Broker "$G_GCP" pass "e2e-gcp listed; $(echo "$badsvc" | one_line 120); $(echo "$badpre" | one_line 120)"
  else
    res check Broker "$G_GCP" fail "e2e-gcp: [$(echo "$good" | one_line 80)]; bad service: [$(echo "$badsvc" | one_line 120)]; services on aws: [$(echo "$badpre" | one_line 120)]"
  fi
}
