#!/bin/sh
# Run install.sh's Linux path against stubs and check what it says about a
# brig that is already on PATH.
#
# curl serves fixtures from a scratch directory: a brig release and a runtime
# installer that only lays down a launcher and a brig for install.sh to
# replace. uname says Linux, so this runs on macOS too, and id says root for
# the node-wide case. Nothing outside the scratch directory is written.
#
# Usage: script/test-install.sh

set -eu

here="$(cd "$(dirname "$0")/.." && pwd)"

skip() { echo "SKIP: $*"; exit 0; }
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok - $*"; }

# A cosign on the base PATH makes install.sh check a signature the fixtures do
# not carry.
if PATH=/usr/bin:/bin command -v cosign > /dev/null 2>&1; then
  skip "cosign is in /usr/bin or /bin"
fi

T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/stub" "$T/root-stub" "$T/home" "$T/pkg" "$T/usrlocal/bin"

sha() {
  if command -v sha256sum > /dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# The brig release.
rel="$T/srv/brig-sh/brig/releases/download/v9.9.9"
mkdir -p "$rel"
printf '#!/bin/sh\necho brig 9.9.9\n' > "$T/pkg/brig"
printf '#!/bin/sh\nexit 0\n' > "$T/pkg/brigd"
chmod 0755 "$T/pkg/brig" "$T/pkg/brigd"
tar -C "$T/pkg" -czf "$rel/brig-9.9.9-linux-amd64.tar.gz" brig brigd
echo "$(sha "$rel/brig-9.9.9-linux-amd64.tar.gz")  brig-9.9.9-linux-amd64.tar.gz" > "$rel/checksums.txt"

# The runtime installer. As a user it puts a launcher in ~/.local/bin and a
# brig in the tree, as the bundle's does. As root it fails: the node-wide
# checks run before it.
rt="$T/srv/NOFireAI/brig-standalone-linux/releases/download/v0.0.0-test"
mkdir -p "$rt"
cat > "$rt/install.sh" <<'RT'
#!/bin/sh
set -eu
[ "$(id -u)" != 0 ] || exit 1
mkdir -p "$HOME/.local/share/brig/data/bin" "$HOME/.local/bin"
printf '#!/bin/sh\nexit 0\n' > "$HOME/.local/share/brig/data/bin/brig"
printf '#!/bin/sh\nexec "$HOME/.local/share/brig/data/bin/brig" "$@"\n' > "$HOME/.local/bin/brig"
chmod 0755 "$HOME/.local/share/brig/data/bin/brig" "$HOME/.local/bin/brig"
RT
echo "$(sha "$rt/install.sh")  install.sh" > "$rt/checksums.txt"

cat > "$T/stub/curl" <<STUB
#!/bin/sh
out=""; url=""
while [ \$# -gt 0 ]; do
  case "\$1" in
    -o) out="\$2"; shift 2 ;;
    -*) shift ;;
    *) url="\$1"; shift ;;
  esac
done
src="$T/srv/\${url#https://github.com/}"
[ -f "\$src" ] || exit 22
if [ -n "\$out" ]; then cp "\$src" "\$out"; else cat "\$src"; fi
STUB
cat > "$T/stub/uname" <<'STUB'
#!/bin/sh
case "$1" in
  -s) echo Linux ;;
  -m) echo x86_64 ;;
  *) exec /usr/bin/uname "$@" ;;
esac
STUB
cat > "$T/root-stub/id" <<'STUB'
#!/bin/sh
[ "$*" = -u ] && { echo 0; exit 0; }
exec /usr/bin/id "$@"
STUB
chmod 0755 "$T/stub"/* "$T/root-stub"/*

# The brig an earlier install left in /usr/local/bin.
printf '#!/bin/sh\necho old brig\n' > "$T/usrlocal/bin/brig"
chmod 0755 "$T/usrlocal/bin/brig"

# run_install <PATH>: install.sh with the given PATH, output in $T/out.log.
run_install() {
  rm -rf "${T:?}/home"
  mkdir -p "$T/home"
  set +e
  env -u XDG_DATA_HOME -u BRIG_VERSION -u BRIG_RUNTIME_VERSION \
    HOME="$T/home" PATH="$1" \
    BRIG_VERSION=v9.9.9 BRIG_RUNTIME_VERSION=v0.0.0-test \
    BRIG_INSTALL_COSIGN=0 BRIG_INSTALL_DIR="$T/usrlocal/bin" \
    sh "$here/install.sh" > "$T/out.log" 2>&1
  rc=$?
  set -e
}

base="/usr/bin:/bin:/usr/sbin:/sbin"
user_bin="$T/home/.local/bin"

# 1. A user whose PATH has /usr/local/bin first: say which brig runs, and how
#    to put ~/.local/bin first. Nothing about removing a file they cannot.
run_install "$T/stub:$T/usrlocal/bin:$user_bin:$base"
[ "$rc" = 0 ] || { cat "$T/out.log" >&2; fail "the user install failed"; }
grep -qF "\`brig\` on your PATH is $T/usrlocal/bin/brig" "$T/out.log" \
  || { cat "$T/out.log" >&2; fail "no word about the brig ahead on PATH"; }
# The literal line a user copies, so no expansion.
# shellcheck disable=SC2016
grep -qF 'export PATH="$HOME/.local/bin:$PATH"' "$T/out.log" \
  || { cat "$T/out.log" >&2; fail "no PATH line to copy"; }
if grep -q "remove it" "$T/out.log"; then
  fail "a user install was told to remove a file it does not own"
fi
ok "a user install with /usr/local/bin first gets the PATH change"

# 2. The same with ~/.local/bin off PATH altogether.
run_install "$T/stub:$T/usrlocal/bin:$base"
grep -qF "\`brig\` on your PATH is $T/usrlocal/bin/brig" "$T/out.log" \
  || { cat "$T/out.log" >&2; fail "no word about the brig on PATH"; }
ok "a user install without ~/.local/bin on PATH gets the PATH change"

# 3. ~/.local/bin already first: nothing to say.
run_install "$T/stub:$user_bin:$T/usrlocal/bin:$base"
[ "$rc" = 0 ] || { cat "$T/out.log" >&2; fail "the user install failed"; }
if grep -q -e "on your PATH is" -e "remove it" "$T/out.log"; then
  cat "$T/out.log" >&2
  fail "a user install with the right PATH was warned"
fi
ok "a user install with ~/.local/bin first is not warned"

# 4. Node-wide: the earlier brig in DEST is root's to remove.
run_install "$T/root-stub:$T/stub:$T/usrlocal/bin:$base"
grep -qF "$T/usrlocal/bin/brig is from an earlier install, and may run instead of the bundle's launcher; remove it" "$T/out.log" \
  || { cat "$T/out.log" >&2; fail "a node-wide install lost the advice to remove it"; }
ok "a node-wide install is still told to remove it"
