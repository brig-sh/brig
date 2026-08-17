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

brig resolves one thing itself: whatever a profile's `secrets:` list
declares, read from brig's own store before the sandbox is created. There is
no store on Linux yet, so a profile that declares `secrets:` fails outright
there rather than silently forwarding nothing.

Everything else still works the way it always did: for an `env.<name>`
binding -- or the deprecated `forward:` spelling of one -- brig reads the
named variable from its own environment, so whatever populates that
environment is your secret backend:

```bash
CLAUDE_CODE_OAUTH_TOKEN=$(your-secret-tool read claude/token) brig run claude
<your secret manager's run-with-env command> -- brig run claude
```

Values are re-read on every exec, so a rotated credential -- from either
source -- is picked up without restarting the sandbox. Nothing is written
into the workspace.

Three rules apply, though the second only to a value read from the ambient
environment:

- Unset or empty is skipped, so it cannot shadow a value baked into the image.
- A `scheme://` value read from the environment is refused as an unresolved
  secret-manager reference. direnv and friends leave those in the environment
  readily, and forwarded verbatim it yields "Invalid username or token" in
  the guest, which looks exactly like a broken sandbox. A `value:` literal or
  a value from brig's own secret store skips this check: it was put there on
  purpose rather than left behind by a tool that never got to resolve it, so
  refusing it would reject a perfectly good credential for merely looking
  like one it is not. `BRIG_ALLOW_REFS=1` forwards an ambient reference
  anyway.
- A variable on the profile's `deny` list is refused, with the reason.

`brig env <agent>` reports the guest's environment, by name, and fails the
same way a run would if a declared secret cannot be resolved. It never
prints a value: a secret-sourced variable comes back annotated, e.g.
`CLAUDE_CODE_OAUTH_TOKEN(secret)`, never with the value itself.

### Not in argv

Forwarded values go into the runtime process's own environment, and only the
variable *name* appears on its command line. So a forwarded credential is not
readable in `ps` by other processes on the host.

`BRIG_ENV_ARGV=1` puts them back on the command line for a runtime build that
does not accept a bare `--env KEY`, and gives up the guarantee -- for a value
read from the environment. A value brig resolved on your behalf is exempt from
the hatch and stays off the command line regardless -- one bound from its own
secret store, and the host credential too, however that was obtained --
because the host durably logs every exec's argv, and an opt-in debugging
escape hatch has no business turning that log into a credential leak.

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

Using it is [secrets.md](secrets.md). This section is only about what the
keychain does and does not protect.

On macOS the backend is the login keychain. Every item is a generic password
under the service `sh.brig.secret`, with the secret's name as the account:

```bash
printf %s "$TOKEN" | brig secret create gh-token
brig secret create deploy-key -f ~/.ssh/id_ed25519
brig secret ls
```

What that means for the things this document is about:

- **The value never appears in argv.** The whole `add-generic-password`
  command, base64 value and all, goes to `security -i` down a pipe, so brig's
  own command line is `security -i` and nothing else. This is the same
  guarantee the forwarding path makes above, for the same reason.

  Confirming it by watching `ps` during a create does not work, which is worth
  saying because an empty result looks the same as a mistyped `grep`: the write
  lives a few milliseconds and a sampling loop will not catch it. Pin it in the
  process table instead, by running `security` the way brig does, with its
  stdin on a fifo nothing closes:

  ```bash
  mkfifo /tmp/probe
  security -i < /tmp/probe &
  exec 3>/tmp/probe                 # a writer, but never an EOF
  ps -Ao args | grep '[s]ecurity'   # `security -i`, and no command after it
  exec 3>&-; kill %1; rm /tmp/probe
  ```
- **The item's ACL is the default one**, which is what makes brig's own
  secrets read back without a keychain dialog: `security` created them, so
  `security` is trusted to read them. The consequence is the part worth being
  clear about -- **anything that can run `/usr/bin/security` as you can read
  them back too**. That is the same boundary as your own shell, and it is
  weaker than a per-application ACL would be. brig does not ask for the
  broad `-A`, but it does not narrow the default either.
- **Values are base64-encoded.** `security -i` reads one command per line, so a
  raw newline in a value would end the command early -- and everything after it
  would be read as a *second command*. Encoding removes that, and with it the
  need to quote anything: base64 spells everything in letters, digits, `+`, `/`
  and `=`, so no `"` and no `\` ever reaches the line. It also makes the NUL
  byte and the non-UTF8 byte ordinary, which is what lets an SSH key or a
  binary value round-trip at all. The visible cost is that Keychain Access, and
  `security find-generic-password -w` by hand, show base64 rather than the
  secret. It is encoding, not encryption, and protects nothing on its own --
  the keychain does that.
- **A value has a size ceiling, and brig refuses rather than stores short.**
  That 4096-byte line is the budget for the *whole* command, so a longer name
  leaves fewer bytes for the value it names -- about 3KB of raw value in
  practice. Every API key and SSH key fits; a 4096-bit RSA private key does
  not. brig checks the length up front and reads back what it wrote, because
  `security` answers a line it cannot fit by shortening it and reporting
  success. See [secrets.md](secrets.md#the-size-limit) for the numbers.
- **`brig secret ls` never decrypts.** It reads attributes only, which is why
  listing raises no access prompt and why it can show names and dates but
  never values. Worth being exact about what it reads, though:
  `security dump-keychain` takes no service filter -- its options are
  `[-adhir] [keychain...]` and nothing else -- so `ls` enumerates the
  attributes of *every* item in your login keychain and discards the ones that
  are not brig's. Nothing is decrypted and nothing leaves the process, but
  names and dates belonging to other applications do pass through brig on
  their way to being thrown away.
- **brig writes only under its own service, which contains brig rather than
  vouching for what it finds.** Every command that creates, changes or removes
  an item carries `-s sh.brig.secret`, so brig cannot reach outside that
  namespace. The converse does not follow, and it would be the more
  comfortable thing to claim: the service name is a label, not an authenticity
  check, and nothing stops another process running as you from adding an item
  under it -- which brig would then read, update and delete as its own. What
  brig does instead is degrade honestly. `read` says plainly when a value is
  not in brig's encoding, and `ls` skips a name outside brig's grammar, so an
  item brig did not write is either reported or passed over rather than
  presented as yours. The `hostCredential` item a profile reads -- Claude
  Code's own, say -- belongs to the application that wrote it and is read,
  never written. That one *does* raise a dialog on first read, for exactly the
  ACL reason above: `security` did not create it.

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
