# What brig protects, and what it does not

The reason to run an agent in a sandbox is that the agent runs code you did
not write, against a machine that holds everything you have. brig narrows what
"everything" means: the agent sees one directory and the credentials you chose
to give it, and nothing else on the host.

This page is about where that boundary actually is. Some of it is weaker than
it looks, and we would rather write that down than let someone find out later.

## The boundary

The sandbox is a microVM on macOS and a container on Linux. On macOS that is a
separate kernel, which is the stronger of the two. The microVM is booted by
hull, which is not published yet -- see the README for how to get it in the
meantime.

Inside it, the guest has your workspace mounted as its home. It does not have
your keychain, your SSH agent, your secret manager, or any other directory on
the host. That inaccessibility is the isolation boundary, and it is also the
reason credentials have to be forwarded in explicitly: the guest cannot fetch
them for itself.

## Credentials

brig resolves nothing itself. It reads the variables a template names from its
own environment, so whatever populates that environment is your secret
backend:

```bash
CLAUDE_CODE_OAUTH_TOKEN=$(your-secret-tool read claude/token) brig run claude
<your secret manager's run-with-env command> -- brig run claude
```

Values are re-read on every exec, so a rotated credential is picked up without
restarting the sandbox. Nothing is written into the workspace.

Three rules apply to every variable:

- Unset or empty is skipped, so it cannot shadow a value baked into the image.
- A `scheme://` value is refused as an unresolved secret-manager reference.
  direnv and friends leave those in the environment readily, and forwarded
  verbatim it yields "Invalid username or token" in the guest, which looks
  exactly like a broken sandbox. `BRIG_ALLOW_REFS=1` forwards it anyway.
- A variable on the template's `deny` list is refused, with the reason.

`brig env <agent>` prints what would be forwarded, by name. It never prints a
value.

### Not in argv

Forwarded values go into the runtime process's own environment, and only the
variable *name* appears on its command line. So a forwarded credential is not
readable in `ps` by other processes on the host.

The Homebrew wrapper this replaces did put values in argv, and said so.
Closing that was one of the reasons to write brig in Go. `BRIG_ENV_ARGV=1`
restores the old behaviour for a runtime build that does not accept a bare
`--env KEY`, and gives up the guarantee.

### What is still exposed

The credential is readable inside the sandbox by anything running alongside
the agent. That is inherent: the sandbox cannot use a credential it cannot
see. Docker's sandboxes avoid it by rewriting auth headers in a host-side
proxy, which works for proxied HTTP and not for `git push`, a vendor CLI's own
token refresh, or an MCP server holding its own connection.

Our answer is a narrow blast radius rather than a sentinel value. Prefer a
fine-grained `GH_TOKEN` scoped to the repositories you want reachable, over a
classic PAT carrying your whole account.

## Guest images

An image is code that will run with your credentials, so brig checks where it
came from before booting it. The check is cosign's keyless verification, and
the question it asks is not "was this signed?" but "was this built by that
workflow, in that repo?":

```bash
cosign verify \
  --certificate-identity-regexp \
    '^https://github.com/brig-sh/community-images/.github/workflows/build-images.yml@refs/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/brig-sh/claude-code:arm64
```

The identity is anchored on the repository and the workflow file, so a
signature from anywhere else fails -- including one from another workflow in
the same repository.

What brig does with the answer:

| situation | behaviour |
| --- | --- |
| image under `ghcr.io/brig-sh/`, signature verifies | one line saying so, boots |
| image published by somebody else | warning, boots |
| cosign not installed | warning, boots |
| image under `ghcr.io/brig-sh/`, signature fails | stops and asks `[y/N]`; refuses when there is no terminal |

The asymmetry is deliberate. Bring-your-own images are a supported way to use
brig, so refusing to boot one would make the feature useless, and "could not
check" is not the same as "failed". The one case with no innocent reading is
an image sitting under our registry whose signature does not verify, and that
is the case that stops.

`BRIG_VERIFY=require` refuses anything that cannot be positively verified,
third-party images included. `BRIG_VERIFY=off` skips the check. A typo in that
setting falls back to `warn` rather than silently disabling it.

Point `BRIG_VERIFY_REGISTRY`, `BRIG_VERIFY_IDENTITY` and `BRIG_VERIFY_ISSUER`
at your own registry and workflow if you publish signed images yourself.

### Two limitations

Under the default `missing` pull policy, cosign verifies the tag in the
registry, not necessarily the copy already in your local store. Use
`BRIG_PULL=always` when that distinction matters to you.

`claude-desktop` and `ubuntu` still point at `ghcr.io/nofireai` images, which
brig has no signing policy for, so they warn on every boot until those images
move.

## brig's own binaries

Releases are signed with keyless cosign as well. There is no key to
distribute and none for us to lose: the certificate is short-lived, bound to
the release workflow's OIDC identity, and recorded in a public transparency
log.

```bash
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature checksums.txt.sig \
  --certificate-identity-regexp \
    '^https://github.com/brig-sh/brig/.github/workflows/release.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

shasum -a 256 -c checksums.txt --ignore-missing
```

The first command vouches for the checksum file, the second ties every archive
to it. Each archive also ships an SPDX SBOM.

We do not yet sign and notarize the binaries with a Developer ID. cosign
proves provenance, but macOS Gatekeeper wants something else, which is why the
Homebrew cask currently strips the quarantine attribute. That is a stopgap and
it is marked as one in the goreleaser config.

## Things brig does not claim

It does not sandbox the agent from the network. Outbound traffic from the
guest is whatever the runtime allows, and there is no per-sandbox policy yet.

It does not protect the workspace from the agent. Everything in there is
writable by design, since that is the work.

It does not stop an agent from spending your money. The `deny` list keeps a
metered key from being forwarded by accident, which is a different and much
smaller promise.
