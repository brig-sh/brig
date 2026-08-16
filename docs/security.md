# What brig protects, and what it does not

The reason to run an agent in a sandbox is that the agent runs code you did
not write, against a machine that holds everything you have. brig narrows what
"everything" means: the agent sees one directory and the credentials you chose
to give it, and nothing else on the host.

This page is about where that boundary actually is. Some of it is weaker than
it looks, and we would rather write that down than let someone find out later.

## The boundary

The sandbox is a microVM on macOS and a sandboxed container on Linux. On macOS
that is a separate kernel, which is the stronger of the two; the direction on
Linux is to run over urunc so that it is a separate kernel there too. The microVM is booted by
hull, which is not published yet -- see the README for how to get it in the
meantime.

Inside it, the guest has your workspace mounted as its home. It does not have
your keychain, your SSH agent, your secret manager, or any other directory on
the host. That inaccessibility is the isolation boundary, and it is also the
reason credentials have to be forwarded in explicitly: the guest cannot fetch
them for itself.

## Credentials

brig resolves nothing itself. It reads the variables a profile names from its
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
- A variable on the profile's `deny` list is refused, with the reason.

`brig env <agent>` prints what would be forwarded, by name. It never prints a
value.

### Not in argv

Forwarded values go into the runtime process's own environment, and only the
variable *name* appears on its command line. So a forwarded credential is not
readable in `ps` by other processes on the host.

`BRIG_ENV_ARGV=1` puts them back on the command line for a runtime build that
does not accept a bare `--env KEY`, and gives up the guarantee.

### What is still exposed

The credential is readable inside the sandbox by anything running alongside
the agent. That is inherent: the sandbox cannot use a credential it cannot
see. Docker's sandboxes avoid it by rewriting auth headers in a host-side
proxy, which works for proxied HTTP and not for `git push`, a vendor CLI's own
token refresh, or an MCP server holding its own connection.

Our answer is a narrow blast radius rather than a sentinel value. Prefer a
fine-grained `GH_TOKEN` scoped to the repositories you want reachable, over a
classic PAT carrying your whole account.

## The secret store

`brig secret` is the one place brig stores something rather than reading it.
It is a store of its own, and nothing above reaches into it yet: a profile
still names environment variables, so what you keep here does not reach a
sandbox until the profile side lands.

On macOS the backend is the login keychain. Every item is a generic password
under the service `sh.brig.secret`, with the secret's name as the account:

```bash
printf %s "$TOKEN" | brig secret create gh-token
brig secret create deploy-key -f ~/.ssh/id_ed25519
brig secret ls
```

What that means for the things this document is about:

- **The value never appears in argv.** brig passes `-w` last and with no
  argument, which makes `security` read the value from stdin, so the command
  line ends at `-w`. This is the same guarantee the forwarding path makes
  above, for the same reason.

  Confirming it by watching `ps` during a create does not work, which is worth
  saying because an empty result looks the same as a mistyped `grep`: brig
  hands `security` a complete reader, so it lives a few milliseconds and a
  sampling loop will not catch it. Pin it in the process table instead, by
  running it the way brig does with its stdin on a fifo nothing closes:

  ```bash
  mkfifo /tmp/v
  security add-generic-password -s sh.brig.probe -a probe -D "brig secret" -w < /tmp/v &
  exec 3>/tmp/v && printf 'canary\n' >&3   # a writer, but never an EOF
  ps -Ao args | grep '[a]dd-generic-password'
  exec 3>&-; kill %1; rm /tmp/v
  ```
- **The item's ACL is the default one**, which is what makes brig's own
  secrets read back without a keychain dialog: `security` created them, so
  `security` is trusted to read them. The consequence is the part worth being
  clear about — **anything that can run `/usr/bin/security` as you can read
  them back too**. That is the same boundary as your own shell, and it is
  weaker than a per-application ACL would be. brig does not ask for the
  broad `-A`, but it does not narrow the default either.
- **Values are base64-encoded.** The prompt `security` reads from is
  line-based, so a raw newline would end the read and no SSH key or binary
  value could round-trip. The visible cost is that Keychain Access, and
  `security find-generic-password -w` by hand, show base64 rather than the
  secret. It is encoding, not encryption, and protects nothing on its own —
  the keychain does that.
- **`brig secret ls` never decrypts.** It reads attributes only, which is why
  listing raises no access prompt and why it can show names and dates but
  never values. Worth being exact about what it reads, though:
  `security dump-keychain` takes no service filter — its options are
  `[-adhir] [keychain...]` and nothing else — so `ls` enumerates the
  attributes of *every* item in your login keychain and discards the ones that
  are not brig's. Nothing is decrypted and nothing leaves the process, but
  names and dates belonging to other applications do pass through brig on
  their way to being thrown away.
- **brig only ever writes under its own service.** Every command that creates,
  changes or removes an item carries `-s sh.brig.secret`, so brig cannot
  modify one it did not create. On the write path the keychain enforces that;
  on the read path above, brig's own filter does. The `hostCredential` item a
  profile reads — Claude Code's own, say — belongs to the application that
  wrote it and is read, never written. That one *does* raise a dialog on first
  read, for exactly the reason above: `security` did not create it.

There is no store on Linux yet; `brig secret` says so rather than falling back
to a file, which would be a downgrade nothing told you about.

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
  ghcr.io/brig-sh/claude-code-stock:latest
```

That works anonymously -- the images are public, and the signature lives in
Sigstore's transparency log, not behind a registry login. `:latest` is a
multi-arch index; the per-architecture tags (`:arm64`, `:amd64`) and the
immutable `:<arch>-<sha>` pins are signed the same way and verify with the
same command.


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
