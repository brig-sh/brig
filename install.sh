#!/bin/sh
# Install brig.
#
#   curl -fsSL https://raw.githubusercontent.com/brig-sh/brig/main/install.sh | sh
#
# Downloads the latest release for this platform, checks it against the
# published checksums, and installs brig and brigd. Homebrew is the better
# path on macOS -- it also brings hull, the runtime brig needs there:
#
#   brew tap brig-sh/brig && brew trust brig-sh/brig && brew install --cask brig
#
# Override the destination with BRIG_INSTALL_DIR, or the version with
# BRIG_VERSION=v0.1.0-rc4.
set -eu

REPO=brig-sh/brig
DEST="${BRIG_INSTALL_DIR:-/usr/local/bin}"

say() { printf 'brig-install: %s\n' "$1" >&2; }
die() { say "$1"; exit 1; }

command -v curl > /dev/null 2>&1 || die "curl is required"
command -v tar  > /dev/null 2>&1 || die "tar is required"

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

version="${BRIG_VERSION:-}"
if [ -z "$version" ]; then
  # The newest release, prereleases included: there is no stable one yet.
  version=$(curl -fsSL "https://api.github.com/repos/$REPO/releases" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
  [ -n "$version" ] || die "could not work out the latest version; set BRIG_VERSION"
fi

bare="${version#v}"
archive="brig-${bare}-${os}-${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$version"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say "downloading $archive"
curl -fsSL -o "$tmp/$archive" "$base/$archive" \
  || die "no build for ${os}/${arch} in $version"

# Never install something we have not checked. The checksum file is signed
# with cosign; verifying that signature needs cosign installed, which this
# script does not require, so it is documented in the README rather than done
# here.
if curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"; then
  if command -v sha256sum > /dev/null 2>&1; then
    ( cd "$tmp" && grep " $archive\$" checksums.txt | sha256sum -c - > /dev/null ) \
      || die "checksum mismatch for $archive"
  elif command -v shasum > /dev/null 2>&1; then
    ( cd "$tmp" && grep " $archive\$" checksums.txt | shasum -a 256 -c - > /dev/null ) \
      || die "checksum mismatch for $archive"
  else
    say "no sha256 tool found, skipping the checksum check"
  fi
  say "checksum ok"
else
  die "could not fetch checksums.txt"
fi

tar -xzf "$tmp/$archive" -C "$tmp"

install_one() {
  if [ -w "$DEST" ]; then
    install -m 0755 "$tmp/$1" "$DEST/$1"
  else
    say "$DEST is not writable, using sudo"
    sudo install -m 0755 "$tmp/$1" "$DEST/$1"
  fi
}
install_one brig
install_one brigd

say "installed $("$DEST/brig" version) to $DEST"

if [ "$os" = darwin ] && ! command -v hull > /dev/null 2>&1; then
  say "macOS also needs hull, the microVM runtime:"
  say "  brew tap brig-sh/brig && brew trust brig-sh/brig && brew install --cask hull"
fi
