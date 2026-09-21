# Releasing

How a release of Brig is cut. One tag, one release. The release workflow
([.github/workflows/release.yml](../.github/workflows/release.yml)) does the
building, signing, notarizing and drafting. This page is the human part
around it, in order.

## The tag is the version

There is no version file. The binary reads its version from what the Go
toolchain embeds at build time, which is derived from the nearest `v*` tag
reachable from the commit:

| Commit | `brig version` prints |
| --- | --- |
| tagged `v0.3.0` | `v0.3.0` |
| tagged `v0.4.0-rc1` | `v0.4.0-rc1` |
| after `v0.3.0` | `v0.3.1-0.<commit time>-<commit>` |
| after `v0.4.0-rc1` | `v0.4.0-rc1.0.<commit time>-<commit>` |
| any of these with uncommitted changes | the same, with `+dirty` |

So a bump is a tag, a release candidate is a prerelease tag, and every build
between tags names the release it follows and the commit it is. Only tags
reachable from the commit count: a maintenance branch cut from `v0.2.0` keeps
counting `v0.2.x` however far `main` has moved.

```
        main                                          0.2
          │                                            │
          ● e5e5e5e ── tag v0.4.0-rc1                  ● d4d4d4d  v0.2.2-0.20260919120000-d4d4d4d4d4d4
          │ v0.4.0-rc1                                 │
          │                                            ● c3c3c3c ── tag v0.2.1
          ● b2b2b2b ── tag v0.3.0                      │ v0.2.1
          │ v0.3.0                                     ● a1a1a1a  v0.2.1-0.20260917120000-a1a1a1a1a1a1
          ● 9999999  v0.2.1-0.20260915120000-…         │
          │   ┌────────────────────────────────────────╯
          ●───┘   git checkout -b 0.2 v0.2.0
        0000000 ── tag v0.2.0
```

The toolchain needs the tag to be present when it builds, which is why the
release workflow clones with full history. A build from a source tarball,
with no git history at all, prints `dev` and no commit. So does a plain
`go build` in a linked git worktree: its `.git` is a file, and the toolchain
does not recognise that as a repository. `make build` passes git's own
answers in that case, and the binary prints what a normal clone would.

## Cut the release

- Read the notes the release will carry before the tag exists:

  ```bash
  make notes TAG=v0.2.0
  ```

  This runs git-cliff over the commits since the previous tag, with the
  same [cliff.toml](../cliff.toml) the workflow uses, so what it prints is
  what the release will say. It needs `git-cliff` (`brew install git-cliff`).
  A subject that reads badly, or sits in the wrong section, is fixed in the
  commit now, while it is still cheap.

- Tag the release commit and push the tag:

  ```bash
  git tag v0.2.0
  git push origin v0.2.0
  ```

  A release candidate is the same with a prerelease tag, `v0.3.0-rc1`. A
  patch for an older release is tagged on its maintenance branch:

  ```bash
  git checkout -b 0.2 v0.2.0        # once, from the release being patched
  git cherry-pick <fix>
  git push origin 0.2
  git tag v0.2.1
  git push origin v0.2.1
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

- Read the draft's notes. They are rendered by git-cliff from the
  conventional-commit subjects since the previous tag, in the sections
  [cliff.toml](../cliff.toml) defines: breaking changes first, then
  features, fixes, refactors and docs, each entry linking its commit and any
  `Fixes`/`Refs` issue, then the contributors by GitHub handle and the
  first-time contributors with their pull request. These come from the
  GitHub API; `make notes` uses `GITHUB_TOKEN` when set, else the anonymous
  rate limit. A commit in the wrong section has the wrong type in
  its subject; a section that behaves wrongly is a rule in `cliff.toml`. Fix
  the one at fault rather than the draft: a re-run of the workflow rewrites
  the draft's body, and a hand-edit is lost with it.

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

- The cask PR opens while the release is still a draft. goreleaser's cask
  pipe checks `skip_upload` and the prerelease marker, not the draft flag.
  The tap's CI compares every cask against the published release, and a
  draft is invisible to its token, so that check fails until the draft is
  published. Publish the draft first, then merge the cask PR. Expect the PR
  to need `brew style --fix Casks/brig.rb` on its branch as well: goreleaser
  writes the cask with its own indentation.

## After the tag

- Ask the module proxy for the new version once, so pkg.go.dev indexes it.
  pkg.go.dev serves only what the proxy has already seen:

  ```bash
  GOPROXY=https://proxy.golang.org go list -m github.com/brig-sh/brig@v0.2.0
  ```

- Update the documentation a release touches. Grep for the previous version
  before publishing and change every place that still quotes it:

  ```bash
  git grep -n 0.1.0-rc18
  ```

  Every page that quotes a hull version:
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

- A downloaded binary prints the tag: `brig version` says `brig v0.2.0`, not
  a pseudo-version, which would mean the workflow built without the tag in
  reach, and not `v0.2.0+dirty`, which would mean the tree changed during
  the build.
- There is exactly one release for the tag, and it is published.
- On a stable tag: the cask PR against `brig-sh/homebrew-brig` exists.
  Goreleaser skips it silently if neither tap token resolved, so check
  rather than assume.
- The tap README describes an install path that actually works for the
  release you shipped.
