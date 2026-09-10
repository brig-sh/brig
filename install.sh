#!/bin/sh
# Install brig.
#
#   curl -fsSL https://brig.sh/install | sh
#
# Downloads the newest release for this platform, checks it against the
# published checksums, and installs brig and brigd. On macOS it also installs
# hull -- the microVM runtime brig drives there, without which a first `brig
# run` fails -- and cosign, which is what verifies the kernel, initrd and guest
# agent every sandbox boots.
#
# Homebrew is still the better path on macOS. It tracks upgrades, and it
# installs the shell completions this script leaves in the archive for you
# (docs/completions.md):
#
#   brew tap brig-sh/brig && brew trust brig-sh/brig && brew install --cask brig
#
# Settings:
#
#   BRIG_INSTALL_DIR      where to install (default /usr/local/bin)
#   BRIG_VERSION          a brig tag instead of the newest release
#   HULL_VERSION          a hull tag instead of the newest release
#   BRIG_INSTALL_HULL=0   skip hull, and leave macOS without a runtime
#   BRIG_INSTALL_COSIGN=0 skip cosign, and leave the boot chain unverified
set -eu

BRIG_REPO=brig-sh/brig
HULL_REPO=brig-sh/hull
DEST="${BRIG_INSTALL_DIR:-/usr/local/bin}"

# cosign is pinned by version and by hash, which the two repos above are not.
# Their checksums are fetched from the release that carries the archive; this
# one is written down here instead, because cosign's own release cannot be
# verified without cosign. Upstream publishes Sigstore bundles and no detached
# signature, so there is nothing openssl can check, and a bundle we half-read
# would look like verification without being it. Pinning moves the trust root
# to this file, which is reviewed, rather than to whatever the CDN served.
#
# Bump both the version and the hashes together. docs/releasing.md carries the
# grep that catches a version quoted in one place and not the other.
COSIGN_VERSION=v3.1.3
COSIGN_SHA_DARWIN_ARM64=5cf948c2f4dfe59687bdd0b8523709067383e03982cc543475c8a7dc70e92a76
COSIGN_SHA_LINUX_AMD64=4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71
COSIGN_SHA_LINUX_ARM64=c5d324e091826b0d7a78eb16fef316450b4eb9aaec045611c08ba06f5e73220a

say() { printf 'brig-install: %s\n' "$1" >&2; }
die() { say "$1"; exit 1; }

command -v curl > /dev/null 2>&1 || die "curl is required"
command -v tar  > /dev/null 2>&1 || die "tar is required"

# Pick the checksum tool before anything is downloaded, and stop if there is
# none. sha256sum is absent on a stock macOS, where shasum covers it, and both
# can be absent on a minimal Linux image -- which is a plausible place to pipe
# an install script. Carrying on without a check is the one outcome a checksum
# step exists to prevent, so this is a die and not a warning.
if command -v sha256sum > /dev/null 2>&1; then
  SHA256_TOOL=sha256sum
elif command -v shasum > /dev/null 2>&1; then
  SHA256_TOOL=shasum
else
  die "need sha256sum or shasum to check what it downloads; install either one"
fi

sha256_check() {
  case "$SHA256_TOOL" in
    sha256sum) sha256sum -c - ;;
    shasum)    shasum -a 256 -c - ;;
  esac
}

# verify <dir> <file> <checksums> checks one file against a checksums list in
# the same directory.
#
# The expected line is pulled out and refused when absent, so a file the
# release does not list cannot pass by handing an empty stream to the checksum
# tool. "checksum ok" prints here and nowhere else, which is the only place it
# is true.
verify() {
  _line=$(grep " $2\$" "$1/$3" || true)
  [ -n "${_line}" ] || die "no checksum published for $2"
  ( cd "$1" && printf '%s\n' "${_line}" | sha256_check > /dev/null ) \
    || die "checksum mismatch for $2"
  say "checksum ok: $2"
}

# newest_tag <repo> names the newest release, prereleases included. Neither
# repo has a stable one yet, so /releases/latest answers 404 for both.
newest_tag() {
  curl -fsSL "https://api.github.com/repos/$1/releases" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1
}

# install_from <dir> <name> puts one executable in DEST.
install_from() {
  if [ -w "$DEST" ]; then
    install -m 0755 "$1/$2" "$DEST/$2"
  else
    say "$DEST is not writable, using sudo"
    sudo install -m 0755 "$1/$2" "$DEST/$2"
  fi
}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin | linux) ;;
  *) die "unsupported OS: $os" ;;
esac

case "$(uname -m)" in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64)  arch=amd64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

# An Intel Mac is refused here rather than later. The release does publish
# darwin/amd64 archives of brig itself, so this used to install cleanly and
# then fail on the first run with "brig drives hull on macOS, and none was
# there" -- which reads as a dependency the user can go and install. They
# cannot: hull drives Virtualization.framework on Apple silicon and has never
# published an amd64 build. Say so now, while nothing has been written.
if [ "$os" = darwin ] && [ "$arch" = amd64 ]; then
  die "brig needs Apple silicon on macOS: hull has no amd64 build, so there would be no runtime to drive (docs/support.md)"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

install_brig() {
  version="${BRIG_VERSION:-}"
  if [ -z "$version" ]; then
    version=$(newest_tag "$BRIG_REPO")
    [ -n "$version" ] || die "could not work out the newest version; set BRIG_VERSION"
  fi

  bare="${version#v}"
  archive="brig-${bare}-${os}-${arch}.tar.gz"
  base="https://github.com/$BRIG_REPO/releases/download/$version"

  say "downloading $archive"
  curl -fsSL -o "$tmp/$archive" "$base/$archive" \
    || die "no build for ${os}/${arch} in $version"
  # The checksum file is signed with cosign as well. Verifying that signature
  # needs a cosign this script may be about to install, so the signature is
  # documented in the README rather than checked here.
  curl -fsSL -o "$tmp/brig-checksums.txt" "$base/checksums.txt" \
    || die "could not fetch checksums.txt for $version"
  verify "$tmp" "$archive" brig-checksums.txt

  # Both archives carry a LICENSE and a README.md, so each unpacks into its own
  # directory rather than over the other.
  mkdir -p "$tmp/brig"
  tar -xzf "$tmp/$archive" -C "$tmp/brig"
  install_from "$tmp/brig" brig
  install_from "$tmp/brig" brigd
  say "installed $("$DEST/brig" version) to $DEST"
}

# hull ships one arm64 archive holding all three executables it needs: the CLI,
# and the two runners that carry the entitlements their backends require --
# virtualization for vz-runner, hypervisor for hvi. They go into the same
# directory on purpose: hull discovers a runner next to its own executable, and
# splitting them breaks that.
#
# The archive is what the Homebrew cask installs too. hull.dmg holds the same
# binaries in an app bundle for people who would rather drag it to
# Applications, which is not a thing a script should be doing.
install_hull() {
  hull_version="${HULL_VERSION:-}"
  if [ -z "$hull_version" ]; then
    hull_version=$(newest_tag "$HULL_REPO")
    [ -n "$hull_version" ] || die "could not work out hull's newest release; set HULL_VERSION"
  fi

  hull_bare="${hull_version#v}"
  hull_archive="hull-${hull_bare}-arm64.tar.gz"
  hull_base="https://github.com/$HULL_REPO/releases/download/$hull_version"

  say "downloading $hull_archive"
  curl -fsSL -o "$tmp/$hull_archive" "$hull_base/$hull_archive" \
    || die "no hull build for arm64 in $hull_version"
  curl -fsSL -o "$tmp/hull-checksums.txt" "$hull_base/checksums.txt" \
    || die "could not fetch hull's checksums.txt for $hull_version"
  verify "$tmp" "$hull_archive" hull-checksums.txt

  mkdir -p "$tmp/hull"
  tar -xzf "$tmp/$hull_archive" -C "$tmp/hull"
  install_from "$tmp/hull" hull
  install_from "$tmp/hull" vz-runner
  install_from "$tmp/hull" hvi
  say "installed $("$DEST/hull" --version) to $DEST"
}

# cosign is what turns hull's boot-asset check from a printed warning into an
# answer. Without it on PATH the check reports "no tooling", and under the
# default mode that is not a refusal: the kernel a sandbox boots is fetched,
# written and booted unverified. Installing it is what makes HULL_VERIFY=require
# and BRIG_VERIFY=require usable on a host without Homebrew.
install_cosign() {
  if command -v cosign > /dev/null 2>&1; then
    say "cosign is already on PATH, leaving it alone"
    return
  fi

  case "$os/$arch" in
    darwin/arm64) cosign_sha=$COSIGN_SHA_DARWIN_ARM64 ;;
    linux/amd64)  cosign_sha=$COSIGN_SHA_LINUX_AMD64 ;;
    linux/arm64)  cosign_sha=$COSIGN_SHA_LINUX_ARM64 ;;
    *) say "no pinned cosign for $os/$arch, skipping it"; return ;;
  esac

  # Worth saying out loud: this is a 130 MB download for one subprocess, and it
  # is the largest thing this script pulls by an order of magnitude.
  say "downloading cosign $COSIGN_VERSION (about 130 MB)"
  curl -fsSL -o "$tmp/cosign" \
    "https://github.com/sigstore/cosign/releases/download/$COSIGN_VERSION/cosign-${os}-${arch}" \
    || die "could not fetch cosign $COSIGN_VERSION for ${os}/${arch}"
  printf '%s  cosign\n' "$cosign_sha" > "$tmp/cosign-checksums.txt"
  verify "$tmp" cosign cosign-checksums.txt
  install_from "$tmp" cosign
  say "installed cosign $COSIGN_VERSION to $DEST"
  if [ "$os" = darwin ]; then
    # Everything else this script installs is Developer ID signed and
    # notarized. cosign's macOS build is ad-hoc signed upstream, so Gatekeeper
    # rejects it on its own and it runs here only because a curl download
    # carries no quarantine attribute. Say it rather than let someone find out.
    say "note: cosign's macOS build is ad-hoc signed upstream, unlike the rest of this install"
  fi
}

install_brig

if [ "$os" = darwin ] && [ "${BRIG_INSTALL_HULL:-1}" != 0 ]; then
  install_hull
fi

if [ "${BRIG_INSTALL_COSIGN:-1}" != 0 ]; then
  install_cosign
fi

# hull finds cosign on PATH, so a DEST that is not on it leaves the boot check
# reporting "no tooling" even though the binary is installed.
case ":${PATH}:" in
  *":$DEST:"*) ;;
  *) say "$DEST is not on your PATH; add it, or hull will not find cosign there" ;;
esac

if [ "$os" = darwin ] && ! command -v hull > /dev/null 2>&1 \
   && [ "${BRIG_INSTALL_HULL:-1}" = 0 ]; then
  say "hull was skipped, so brig has no runtime here:"
  say "  brew tap brig-sh/brig && brew trust brig-sh/brig && brew install --cask hull"
fi

say "next: brig doctor"
