# Releasing

How a release of Brig is cut. One tag, one release. The release workflow
([.github/workflows/release.yml](../.github/workflows/release.yml)) does the
building, signing, notarizing and drafting. This page is the human part
around it, in order.

## Cut the release

- Bump `VERSION` to the version you are releasing, without the `v`. The tag
  carries the `v`, the file does not. They must name the same version,
  because the workflow asserts it and fails the release if they disagree:

  ```
  [ "$(cat VERSION)" = "$TAG" ] || {
    echo "VERSION file ($(cat VERSION)) does not match tag $TAG" >&2; exit 1; }
  ```

  Land the `VERSION` bump before you tag, not after.

- Tag the release commit and push the tag:

  ```bash
  git tag v0.1.0-rc18
  git push origin v0.1.0-rc18
  ```

  The push starts the workflow. It builds both binaries for every target,
  signs the checksums with cosign, and signs and notarizes the macOS
  binaries. It also opens a **draft** release for the tag. `draft: true` is
  deliberate: a tag never publishes itself before someone has read the
  notes.

- Two signing paths run here, and only one needs a stored secret. `cosign`
  signs the checksum file keylessly: it gets a short-lived certificate from
  Sigstore's OIDC flow, and no key exists anywhere. Signing and notarizing
  the macOS binaries needs repository secrets instead. It needs a Developer
  ID certificate, its password, a notary API key, a key ID and an issuer ID.
  `.goreleaser.yaml` gates notarization on the certificate secret being set,
  so a release run without it still succeeds, and ships an unsigned macOS
  binary.

## Publish the draft

- Read the draft's generated notes. Fix anything the changelog grouped
  wrong.

- **Publish that draft.** Editing it to "published" is the release. Do not
  create a new release for the tag. That is how `v0.1.0-rc16` ended up with
  two releases of the same name, a Draft beside a Pre-release.

- You can re-run the workflow for a tag that already has a draft. This can
  happen with a `workflow_dispatch` retry, or a second push of the tag. It
  targets the existing draft rather than opening a second release.
  `use_existing_draft: true` in [.goreleaser.yaml](../.goreleaser.yaml) is
  what makes that safe.
  Still publish only the one draft, never two.

## The Homebrew tap

- The cask lives in `brig-sh/homebrew-brig`, a separate repository. The
  release job opens a pull request against it rather than pushing. A
  maintainer of that tap reviews and merges the cask PR.

- Publishing the cask needs a token for that repository, because the
  built-in `GITHUB_TOKEN` cannot write there. The workflow first mints a
  short-lived one from the NOFire bot GitHub App
  (`actions/create-github-app-token`, `continue-on-error: true`). If the App
  is not installed, it falls back to the `HOMEBREW_TAP_GITHUB_TOKEN` secret.
  If neither resolves, goreleaser skips the cask and the release
  still goes green, so check that the cask PR actually exists before you
  walk away.

- The cask is uploaded only on a **stable** tag. `skip_upload: auto` skips
  it for a prerelease, on purpose: an rc must not move what `brew upgrade`
  follows. So a normal rc opens no cask PR, and that is correct.

- Until the first stable tag, the casks in the tap are maintained by hand and
  carry a header saying so. The first stable release is what retires that
  header and hands the tap over to this workflow. Do not hand-edit a cask
  the workflow now owns.

## After the tag

- Ask the module proxy for the new version once, so pkg.go.dev indexes it.
  pkg.go.dev serves only what the proxy has already seen:

  ```bash
  GOPROXY=https://proxy.golang.org go list -m github.com/brig-sh/brig@v0.1.0-rc18
  ```

- Update the documentation a release touches. Grep for the previous version
  before publishing and change every place that still quotes it:

  ```bash
  git grep -n 0.1.0-rc17
  ```

  At least `VERSION`, and every page that quotes a hull version:
  [docs/policies.md](policies.md), [docs/runtimes.md](runtimes.md),
  [docs/security.md](security.md), and the files under
  [docs/manual-tests/](manual-tests/).

  This grep exists because a version quoted in a comment survives copy-paste.
  Through `0.1.0-rc18`, `install.sh`'s own `BRIG_VERSION` example still named
  the previous release, `rc17`: the exact staleness this step is meant to
  catch. That example now names no tag at all, which removes it from this
  grep's work rather than relying on the grep to find it. Run the grep
  everywhere a version can hide, not only where you expect one.

- `install.sh` pins `cosign` by version **and** by SHA-256, one hash per
  platform. That pin is deliberate: cosign's own release cannot be verified
  without cosign, so the hash in this repository is the trust root. It
  is the one version here that no release of ours moves. Bump the version
  and all three hashes together, from the checksums file on the cosign
  release, and never one without the others.

## Check before you walk away

- The tag matches `VERSION`.
- There is exactly one release for the tag, and it is published.
- On a stable tag: the cask PR against `brig-sh/homebrew-brig` exists.
  Goreleaser skips it silently if neither tap token resolved, so check
  rather than assume.
- The tap README describes an install path that actually works for the
  release you shipped.
