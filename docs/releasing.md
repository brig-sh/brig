# Releasing

One tag makes one release. The release workflow
([.github/workflows/release.yml](../.github/workflows/release.yml)) builds,
signs, notarizes and drafts the release. A maintainer does the steps below
around the workflow, in order.

## The tag is the version

There is no version file. The Go toolchain embeds the version in the binary
at build time. It derives the version from the nearest `v*` tag reachable
from the commit:

| Commit | `brig version` prints |
| --- | --- |
| tagged `v0.3.0` | `v0.3.0` |
| tagged `v<version>-rc<n>` | `v<version>-rc<n>` |
| after `v0.3.0` | `v0.3.1-0.<commit time>-<commit>` |
| after `v<version>-rc<n>` | `v<version>-rc<n>.0.<commit time>-<commit>` |
| any of these with uncommitted changes | the same, with `+dirty` |

A version bump is a tag. A release candidate is a prerelease tag. Every
build between tags names the release it follows and its commit.

Only tags reachable from the commit count. A maintenance branch cut from
`v0.2.0` keeps counting `v0.2.x`, however far `main` moves.

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

The toolchain needs the tag at build time, so the release workflow clones
with full history. Without the tag, the output changes:

| Build | `brig version` prints |
| --- | --- |
| From a source tarball, with no git history | `dev` and no commit |
| Plain `go build` in a linked git worktree | `dev` and no commit. The `.git` there is a file, and the toolchain does not recognise it as a repository. |
| `make build` in a linked git worktree | What a normal clone prints. `make build` passes the answers from git. |

## Cut the release

1. Read the notes that the release will carry:

   ```bash
   make notes TAG=v0.2.0
   ```

   This command needs `git-cliff` (`brew install git-cliff`). It runs
   git-cliff over the commits since the previous tag, with the same
   [cliff.toml](../cliff.toml) that the workflow uses. Its output is what the
   release will say.

2. If a subject reads badly or sits in the wrong section, fix the commit
   now, before the tag exists.

3. If this release is the one that `retiredGoesIn` in `cmd/brig/main.go`
   names, remove the retired spellings first. As an alternative, move that
   constant and the docs to a later release. v0.3.0 shipped with the retired
   spellings still in, because no step checked.

4. Tag the release commit and push the tag:

   ```bash
   git tag v0.2.0
   git push origin v0.2.0
   ```

   For a release candidate, use a prerelease tag such as `v<version>-rc<n>`.

The push starts the release workflow. The workflow:

- builds both binaries for every target
- signs the checksums with cosign
- signs and notarizes the macOS binaries
- opens a **draft** release for the tag

Because of `draft: true`, a tag never publishes a release before someone
reads the notes.

### Patch an older release

Tag a patch for an older release on its maintenance branch:

```bash
git checkout -b 0.2 v0.2.0        # once, from the release being patched
git cherry-pick <fix>
git push origin 0.2
git tag v0.2.1
git push origin v0.2.1
```

### Signing

Two signing paths run in the workflow. Only one needs a stored secret.

| Path | What it needs |
| --- | --- |
| `cosign` signs the checksum file | No stored secret. The signing is keyless: `cosign` gets a short-lived certificate from the OIDC flow of Sigstore, and no key exists anywhere. |
| The workflow signs and notarizes the macOS binaries | Repository secrets: a Developer ID certificate, its password, a notary API key, a key ID and an issuer ID. |

> [!WARNING]
> `.goreleaser.yaml` gates notarization on the certificate secret. A release
> run without that secret still succeeds, and ships an unsigned macOS binary.

## Publish the draft

1. Read the notes in the draft.

2. If a commit is in the wrong section, fix the type in the commit subject.

3. If a section behaves wrongly, fix the rule in `cliff.toml`.

4. Do not edit the draft by hand. A re-run of the workflow rewrites the body
   of the draft and loses the edit.

5. **Publish that draft.** The change of the draft to "published" is the
   release. Do not create a new release for the tag. A new release gives the
   tag two releases of the same name.

git-cliff renders the notes from the conventional-commit subjects since the
previous tag. [cliff.toml](../cliff.toml) defines the sections, in this
order:

1. Breaking changes
2. Features
3. Fixes
4. Refactors
5. Docs
6. The contributors, by GitHub handle
7. The first-time contributors, with their pull request

Each entry links its commit and any `Fixes`/`Refs` issue. The contributor
lists come from the GitHub API. `make notes` uses `GITHUB_TOKEN` when it is
set, and the anonymous rate limit otherwise.

You can re-run the workflow for a tag that already has a draft. Use a
`workflow_dispatch` retry or a second push of the tag. The re-run updates
the existing draft and opens no second release, because
[.goreleaser.yaml](../.goreleaser.yaml) sets `use_existing_draft: true`.

## The Homebrew tap

The cask is in `brig-sh/homebrew-brig`, a separate repository. On a stable
tag, the release job opens a pull request against that repository and does
not push to it.

1. Make sure that the cask PR exists.

2. Publish the draft release before you merge the cask PR. The CI of the tap
   compares every cask against the published release. A draft is invisible
   to its token, so that check fails until the release is published.

3. If the cask PR needs it, run `brew style --fix Casks/brig.rb` on its
   branch. goreleaser writes the cask with its own indentation. So far, each
   release needed a commit that applies `brew style` to the generated file.

4. Merge the cask PR. A maintainer of the tap reviews and merges it.

goreleaser generates `Casks/brig.rb` in the tap, and the next stable release
overwrites it. Make lasting changes in `.goreleaser.yaml`.

### When the cask PR opens

| Tag | Cask PR |
| --- | --- |
| Stable | Opens while the release is still a draft. The cask pipe of goreleaser checks `skip_upload` and the prerelease marker, not the draft flag. |
| Prerelease (a release candidate) | Does not open. `skip_upload: auto` skips the upload, because a release candidate must not move what `brew upgrade` follows. |

### The tap token

The built-in `GITHUB_TOKEN` cannot write to the tap, so the workflow needs
a token for that repository:

1. The workflow first mints a short-lived token from the NOFire bot GitHub
   App (`actions/create-github-app-token`, `continue-on-error: true`).
2. If the App is not installed, the workflow uses the
   `HOMEBREW_TAP_GITHUB_TOKEN` secret.
3. If neither resolves, goreleaser skips the cask and the release still goes
   green.

## The prerelease channels

`.github/workflows/channel.yml` publishes two casks that are not releases:

| Cask | Source |
| --- | --- |
| `brig@main` | Rebuilt on every merge to `main`. |
| `brig@experimental` | Promoted by hand from any ref, with a `workflow_dispatch`. |

With these casks you can try a feature before a release or a release
candidate has it. See
[Trying something before it is released](install.md#trying-something-before-it-is-released).

To promote a branch:

1. Run `gh workflow run channel.yml -f ref=<branch>` on `brig-sh/brig`.
2. If the feature needs both repositories, run the same command on
   `brig-sh/hull`.

### How a build is published

Each build is a separate prerelease, tagged `channel-<channel>-<version>`.

Releases in this org are immutable. A published release takes no more
assets. Its tag cannot move, and cannot be deleted while the release exists.
As a result, the workflow does these steps in order:

1. It drafts the build.
2. It gives the draft its assets.
3. It publishes the build. The cask in the tap names the release of that
   build.
4. After the tap moves, it deletes the older builds of the same channel with
   their tags. The newest three stay.
5. It also removes a draft, or a tag with no release, that a failed run left.

No channel tag matches `v*`. So no channel tag starts the release workflow,
and `skip_upload: auto` on the stable cask is untouched.

### Channel tags

A channel tag points at a commit made for it: an empty commit on top of the
commit of the build, with the same tree. Nothing is built on that commit, so
it is never an ancestor of `main`. goreleaser reads the nearest tag for the
version, so it never finds a channel tag.

`git.ignore_tags` matches whole names only, so it cannot keep per-build tags
out. The `tag_pattern` in cliff.toml matches `v*` only, so the release notes
skip channel tags too.

### Channel signing

The macOS binaries are signed and notarized like the binaries of a release,
with the same certificate and notary key. Homebrew quarantines what a cask
downloads. Gatekeeper then checks each binary on its first run and blocks
one that it cannot verify. A channel upgrade is a new binary every time.

A build promoted to `brig@experimental` is also signed with the Developer
ID, from any branch.

### The channel cask

The workflow pushes the channel cask to the tap directly, with no pull
request, because the cask is regenerated on every merge.

- The `main` ruleset of the tap requires a reviewed pull request. So the
  push depends on the `brig-release-bot` App being a bypass actor there.
- The workflow mints the token of that App, the same one `release.yml`
  uses.
- The commit is signed off for the `DCO sign-off` check of the tap.
- The channel of hull publishes the same way.

`script/render-cask.py` renders the cask. goreleaser does not, because a
channel has no version and its cask pipe has nothing to release. The
renderer emits what `brew style` wants, so a channel cask needs no `--fix`
pass.

## After the tag

1. Ask the module proxy for the new version once, so that pkg.go.dev indexes
   it. pkg.go.dev serves only what the proxy has seen:

   ```bash
   GOPROXY=https://proxy.golang.org go list -m github.com/brig-sh/brig@v0.2.0
   ```

2. Before you publish, grep for the previous version:

   ```bash
   git grep -n <previous version>
   ```

   Run the grep over the whole repository, comments included.

3. Change every place that still quotes the previous version. These pages
   quote a hull version: [docs/policies.md](policies.md),
   [docs/runtimes.md](runtimes.md), [docs/security.md](security.md), and the
   files under [docs/manual-tests/](manual-tests/).

4. If the release is on a hull version with no conformance record, make the
   record before the docs quote that version. See
   [Egress conformance record](#egress-conformance-record).

### Egress conformance record

1. Use a Mac with that hull on `PATH`.

2. Run the script:

   ```bash
   script/egress-conformance.sh
   ```

   The script boots three sandboxes on `hvi`, one at a time. It runs the
   network cases from #264 in each sandbox. Then it writes
   `docs/manual-tests/egress-conformance-hvi-<hull version>.md`.

3. Read the record:

   | Result | Meaning | What to do |
   | --- | --- | --- |
   | `FAIL` | The egress claims in [docs/policies.md](policies.md) and [docs/security.md](security.md) do not hold on that hull, or the run proved nothing for a case. | Read the reason. Then fix the claims, run the script again or hold the release. |
   | `unproven` | The case is no evidence for the claim: the guest got no further without a policy. | Only `metadata` can end there, because hull resets every connection to 169.254.0.0/16. Any other `unproven` case fails the run. |

4. Commit the record.

An interrupted run writes no record. CI never boots a guest, so this run is
the only check of these cases on a guest.

### The cosign pin

`install.sh` pins `cosign` by version **and** by SHA-256, one hash per
platform. The release of cosign cannot be verified without cosign, so the
hash in this repository is the trust root. No release of Brig moves this
version.

When you bump the pin, change the version and all three hashes together.
Take the hashes from the checksums file on the cosign release.

## Final checks

1. Download a binary and run `brig version`. The line must start with
   `brig v0.2.0`.

   | Output | Cause |
   | --- | --- |
   | A pseudo-version | The workflow built without the tag in reach. |
   | `v0.2.0+dirty` | The tree changed during the build. |

2. Make sure that the tag has one release only, and that the release is
   published.

3. On a stable tag, make sure that the cask PR against
   `brig-sh/homebrew-brig` exists. goreleaser skips the cask silently if
   neither tap token resolved.

4. Make sure that the install path in the tap README works for the release
   that you shipped.
