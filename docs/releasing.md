# Releasing

How a release of brig is cut. One tag, one release. The release workflow
([.github/workflows/release.yml](../.github/workflows/release.yml)) does the
building, signing, notarizing and drafting; this page is the human part around
it, in order.

## Cut the release

- Bump `VERSION` to the version you are releasing, without the `v`. The tag
  carries the `v`; the file does not. They must name the same version, because
  the workflow asserts it and fails the release if they disagree:

  ```
  [ "$(cat VERSION)" = "$TAG" ] || {
    echo "VERSION file ($(cat VERSION)) does not match tag $TAG" >&2; exit 1; }
  ```

  So land the `VERSION` bump before you tag, not after.

- Tag the release commit and push the tag:

  ```bash
  git tag v0.1.0-rc17
  git push origin v0.1.0-rc17
  ```

  The push starts the workflow. It builds both binaries for every target,
  signs the checksums with cosign, signs and notarizes the macOS binaries, and
  opens a **draft** release for the tag. `draft: true` is deliberate: a tag
  never publishes itself before someone has read the notes.

## Publish the draft

- Read the draft's generated notes. Fix anything the changelog grouped wrong.

- **Publish that draft.** Editing it to "published" is the release. Do not
  create a new release for the tag: that is how `v0.1.0-rc16` ended up with two
  releases of the same name, a Draft beside a Pre-release.

- If you re-run the workflow for a tag that already has a draft (a
  `workflow_dispatch` retry, or a second push of the tag), it targets the
  existing draft rather than opening a second release. `use_existing_draft:
  true` in [.goreleaser.yaml](../.goreleaser.yaml) is what makes that safe.
  Still publish the one draft; never publish two.

## The Homebrew tap

- The cask lives in `brig-sh/homebrew-brig`, a separate repository. The release
  job opens a pull request against it rather than pushing; a maintainer of that
  tap reviews and merges the cask PR.

- The cask is uploaded only on a **stable** tag. `skip_upload: auto` skips it
  for a prerelease, on purpose: an rc must not move what `brew upgrade`
  follows. So a normal rc opens no cask PR, and that is correct.

- Until the first stable tag, the casks in the tap are maintained by hand and
  carry a header saying so. The first stable release is what retires that
  header and hands the tap over to this workflow. Do not hand-edit a cask the
  workflow now owns.

## After the tag

- Ask the module proxy for the new version once, so pkg.go.dev indexes it.
  pkg.go.dev serves only what the proxy has already seen:

  ```bash
  GOPROXY=https://proxy.golang.org go list -m github.com/brig-sh/brig@v0.1.0-rc17
  ```

- Update the documentation a release touches. Grep for the previous version
  before publishing and change every place that still quotes it:

  ```bash
  git grep -n 0.1.0-rc16
  ```

  At least `VERSION`, the compatibility window named in README's deprecation
  section (the hull version brig can pin against), and anything else that
  quotes a version number.

## Check before you walk away

- The tag matches `VERSION`.
- There is exactly one release for the tag, and it is published.
- On a stable tag: the cask PR against `brig-sh/homebrew-brig` is open.
- The tap README describes an install path that actually works for what you
  just shipped.
