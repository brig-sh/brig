# What brig protects, and what it does not

The reason to run an agent in a sandbox is that the agent runs code you did
not write, against a machine that holds everything you have. brig narrows what
"everything" means: the agent sees one directory and the credentials you chose
to give it, and nothing else on the host.

This page is about where that boundary actually is. Some of it is weaker than
it looks, and we would rather write that down than let someone find out later.

If you have found a flaw in one of these boundaries, [SECURITY.md](../SECURITY.md)
is how to report it privately. The section below on
[things brig does not claim](#things-brig-does-not-claim) is the line between a
vulnerability and a known limitation.

## The boundary

The sandbox is a microVM on both. On macOS it is booted by
[hull](https://github.com/brig-sh/hull) over Virtualization.framework, which
`brew install --cask brig` brings along. On Linux brig drives `nerdctl` and
hands the container to the urunc shim (`io.containerd.urunc.v2`), which is the
default rather than the direction, so the guest has a kernel of its own there
too. `BRIG_CONTAINERD_RUNTIME=runc` asks for a plain container instead, which
shares the host kernel: that is the weaker of the two, and it is something you
have to choose rather than something you get.

Inside it, the guest has your workspace mounted as its home. It does not have
your keychain, your SSH agent, your secret manager, or any other directory on
the host. That inaccessibility is the isolation boundary, and it is also the
reason credentials have to be forwarded in explicitly: the guest cannot fetch
them for itself.

## Credentials

**A run on a shipped profile reads no host source.** Nothing on the `brig run`,
`exec` or `shell` path reaches a keychain item brig did not write, a file
outside the workspace, or a host command. Your host login enters brig's own
store once, when you type `brig secret import <profile>`, and every run
afterwards reads only that store:

```bash
brig run claude-code               # log in inside the sandbox, or:
brig secret import claude-code     # carry the host login in, once
```

That is a change from earlier releases, which read Claude Code's own keychain
item on every invocation as a fallback. The profile key that did it,
`hostCredential:`, is deprecated and has left the shipped profiles; brig
removes the field in the next release. `BRIG_CREDENTIALS_CMD`, which pointed
that read at a host command of your own, is already gone: a run that sets it
fails, naming `brig secret import <profile> <name> --from-command '<command>'`.

Until the field goes too, the sentence above is about the profiles brig ships.
A profile of your own still carrying `hostCredential:` keeps the old
behaviour, because that is what deprecating rather than deleting the key
means: every run reads the host item it names, and raises the approval dialog
that comes with it.
`brig run` warns when it finds one. Moving it to `secrets:` with `sources:` is
what makes the promise above true for your profile too.

A credential reaches the guest by one of two channels, and the profile picks
per secret. `files:` writes it into the guest at the path the agent already
reads; `env:` binds it as an environment variable, for credentials whose
consumer offers no file interface. For an `env.<name>` binding -- or the
deprecated `forward:` spelling of one -- brig still reads the named variable
from its own environment, so whatever populates that environment remains a
usable backend for those.

There is no secret store on Linux yet. A profile whose secrets are *optional*
degrades there rather than failing: the run boots and the agent asks for a
login, which is what `claude-code` does. A **required** secret does fail
outright, because there is nowhere on that host to read it from.

Values are re-read on every exec, so a rotated credential is picked up without
restarting the sandbox. Nothing is written into the workspace.

### What file delivery buys, and what it costs

A credential delivered as a file stays out of `/proc/<pid>/environ`, is not
inherited by processes the agent spawns, and can be rewritten under a running
agent -- so a rotated secret can reach a live session, which no environment
variable can. The bytes land on a memory-backed mount covering the agent's
whole config directory, verified to be `tmpfs` with no swap before anything is
written, so there is no path from the credential to your disk to check.

Four costs, and none of them is small enough to leave implied.

- **brig stores and hands over a refresh token.** There is no way to give
  Claude Code a working `.credentials.json` without `refreshToken` and
  `refreshTokenExpiresAt` -- a file carrying only an access token is *worse*
  than an environment variable, because the agent attempts a refresh, fails,
  and prompts. So a compromised agent inside the sandbox can mint access
  tokens indefinitely, and keeps doing so after the host's own token has
  expired. The compensating argument is real and belongs beside it: the guest
  refreshes for itself, so a long session stops breaking every few hours. It
  is a trade, taken deliberately.
- **brig's copy is less protected than the item it came from.** The host's
  Claude item is ACL-scoped to the application that wrote it, which is why it
  raises a dialog the first time something else reads it. The copy brig writes
  carries the **default ACL**, so anything that can run `/usr/bin/security` as
  you reads it back with no dialog at all. Narrowing the ACL does not fix this
  -- any process can invoke `security` -- so the only real mitigation is
  keeping the stored copy low-value, and a refresh token is not low-value.
  When you are not using it, delete it: `brig secret delete claude-credentials`.
- **The denylist stays env-scoped.** A `files:` binding bypasses the deny check
  entirely, and no name check could fix that: a profile could deliver a metered
  API key inside a `settings.json` and nothing would see it. That is defensible
  rather than a hole, because `deny` exists to catch **accident** -- an ambient
  variable swept into the guest because it happened to be in your shell. A file
  binding takes an explicit stored secret and an explicit binding written by
  the profile author; nobody file-binds an `ANTHROPIC_API_KEY` by mistake. The
  two names on `claude-code`'s denylist are env-shaped by the agent's own
  design, so the guard still covers the channel the risk uses.
- **The stored copy does not rotate.** A credential renewed on the host does
  not update brig's copy, and one *revoked* on the host stays valid in brig's
  store until you re-import or delete it. brig warns before boot when the
  stored copy has expired, and names the command that refreshes it; it cannot
  see a revocation at all.

A `0600` file is also still readable by anything running as the agent's uid
inside the sandbox. Files narrow the exposure; they do not draw a boundary.
See [what is still exposed](#what-is-still-exposed) below.

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
`GH_TOKEN(secret)`, never with the value itself. A credential delivered as a
file is not an environment variable and does not appear in that list at all.

### What reaches host disk

The credential file is the part that does not. It lands on a `tmpfs` mount
covering `~/.claude`, checked to be `tmpfs` with no swap before anything is
written, so `~/.claude/.credentials.json` and the temp file the agent renames
onto it never touch your disk. `brig stop` takes that mount with the VM, which
is why an in-sandbox login does not outlive a stop.

The rest of `~/.claude` is not on that mount. Seven paths under it are
hostmounted, so they live in the workspace on host disk and persist across
boots:

- `settings.json` and `CLAUDE.md`, your permission allowlist and your
  user-level memory, written by hand or by the agent on your instruction.
- `sessions`, `projects` and `history.jsonl`, which are the conversation.
- `plugins` and `skills`, which are also where `--skills` copies your own, so
  leaving either off would make that flag do nothing.

Anything else under `~/.claude` is ephemeral, including anything a future
Claude Code version starts writing there. This list is the `volumes:` block of
the `claude-code` profile, which is the source it follows rather than a
restatement that can drift from it.

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

`brig secret` is the one place brig stores something rather than reading it,
and after the switchover above it is the **only** store a run reads. A profile
names what it wants out of it under `secrets:`, and `brig secret import` is how
a host login gets in.

Prefer that over composing a value into brig's environment, for one concrete
reason: a value brig resolved on your behalf is exempt from the
`BRIG_ENV_ARGV` escape hatch above, and an ambient one is not. The two paths
end at the same variable in the same guest. Only one of them stays off the
command line the host logs no matter what anyone sets later.

Using it is [secrets.md](secrets.md), and the profile side is
[profiles.md](profiles.md#secrets-and-env-for-a-credential-brig-resolves-itself).
This section is only about what the keychain does and does not protect.

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
  process table instead, by running `security` the way brig does and then
  holding it open. `security -i` executes each line as it arrives and blocks
  for the next, so a fifo nothing closes leaves the process sitting there with
  the write already done:

  ```bash
  mkfifo /tmp/probe
  security -i < /tmp/probe &
  exec 3>/tmp/probe                          # a writer, but never an EOF
  printf 'add-generic-password -s sh.brig.probe -a probe -w %s\n' \
    "$(printf %s hunter2 | base64)" >&3      # the real command, down the pipe
  ps -Ao args | grep -x '[s]ecurity -i'      # the whole argv, command and all
  ps -Ao args | grep '[a]dd-generic-password'  # no process is carrying it
  security find-generic-password -s sh.brig.probe -a probe -w  # yet it stored
  exec 3>&-; kill %1
  security delete-generic-password -s sh.brig.probe -a probe
  rm /tmp/probe
  ```

  The read-back is what makes this a test rather than a tautology: the item is
  really in the keychain, so the command really was carried, and it was still
  never an argument.
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
  `[-adhir] [keychain...]` and nothing else -- and brig names no keychain
  either, so the dump covers the whole keychain *search list*. On a stock Mac
  that is your login keychain **and the System keychain**, which is a wider
  net than the login keychain alone; `security list-keychains` shows yours.
  `ls` enumerates the attributes of every item in all of them and discards the
  ones that are not brig's. Nothing is decrypted and nothing leaves the
  process, but names and dates belonging to other applications, and to the
  system, do pass through brig on their way to being thrown away.
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
  presented as yours. An item belonging to another application -- Claude Code's
  own, say -- is read by `brig secret import` and never written, and that read
  *does* raise a dialog, for exactly the ACL reason above: `security` did not
  create it. A run never performs that read at all, which is the point of the
  import verb: the dialog appears when you asked for it, once, and never again
  on the boot path.

There is no store on Linux yet; `brig secret` says so rather than falling back
to a file, which would be a downgrade nothing told you about.

## Writing into the workspace

The workspace is the guest's home, mounted read-write, so its contents are the
sandbox's to choose. brig also writes into it from the host on every
invocation: the stale-share marker, the onboarding seed, the trust key, the
guest git files, the skills copied in by `--skills`. Put those two facts
together and you have the one place where the sandbox gets to influence what
happens on the host, so it is worth naming precisely.

brig runs as you and outside the sandbox. A guest that plants a symlink where
brig writes next has aimed brig at a host path the guest itself can never
reach -- your `~/.ssh`, your shell profile -- and brig would follow it,
because to brig it is just a path inside the workspace. Nothing about the
microVM boundary stops this: the write is on the host side of it.

So every host-side read and write brig makes inside the workspace goes through
an `os.Root` opened on the workspace. It resolves each path itself, refuses an
absolute symlink outright, will not let a relative one climb past the root,
and leaves no window between the check and the open for the guest to swap the
file in. A symlink that stays *inside* the workspace is refused too: brig
writes only regular files in there, so a link where a state file belongs was
put there rather than left there.

The one escape a root cannot see is a symlink *at* the workspace itself, since
resolving that still leaves every path below it honestly "inside the root".
That is checked separately, before anything is created.

What this looks like when it fires is a failed run, before anything is
written, naming the link and where it points:

```console
$ brig run claude
brig: refusing to write /Users/alex/brig/claude-code/.claude.json: it is a
symlink to "/Users/alex/.ssh/authorized_keys", and brig writes only regular
files inside the workspace. The workspace is mounted read-write as the
sandbox's home, so that link was put there from inside the sandbox, to have
brig -- which runs as you, on the host -- reach a file the sandbox cannot.
Nothing was written; inspect /Users/alex/brig/claude-code/.claude.json and
remove it before running brig again
```

Read it as what it says. brig does not create the links it writes through, so
one in the way is either something you put there deliberately, or the sandbox
reaching for the host. Neither is a case for retrying: remove the link, or
point the workspace somewhere else.

`--workspace` pointed at a symlink is refused for the same reason, with the
same kind of message, and is fixed by naming the real directory.

## Guest images

An image is code that will run with your credentials, so brig checks where it
came from before booting it. The check is cosign's keyless verification, and
the question it asks is not "was this signed?" but "was this built by that
workflow, in that repo?":

```bash
cosign verify \
  --certificate-identity-regexp \
    '^https://github\.com/brig-sh/community-images/\.github/workflows/build-images\.yml@refs/heads/main$' \
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

### The digest, not the tag

A tag is a name, and a name can resolve to different bytes in the registry and
in your local store. The provenance claim is about the bytes that run, so brig
resolves the reference to the digest the registry serves before the check,
verifies that digest, and boots it. The object cosign checked is the object that
runs, and the success line names the digest rather than the tag it came from. A
local store holding a different digest under the tag is treated as the
signature-failure row above: it stops, and a yes boots the verified digest
rather than the copy on disk. A registry that cannot be reached stops in the
same way, because one of our images could not be checked, and a network
failure must not be what turns the default mode into "unchecked";
`BRIG_VERIFY=require` refuses outright. Every cosign call is bounded, so an
outage fails in seconds rather than hanging the boot.

All of this is for images under `ghcr.io/brig-sh/`. An image brig did not
publish carries no signature of ours to check, so brig makes no cosign call for
it at all: it boots by tag, as it always has, with the one line saying whose
image it is. Resolving a digest for it would cost a registry round trip on every
boot and buy a pin brig cannot vouch for.

This holds wherever the runtime's store answers a digest: containerd on Linux,
and hull from 0.1.0-rc23 on macOS. brig asks the hull it is driving rather than
assuming, because the one on PATH may be older than brig. An older hull cannot
find a digest reference in its store, so a pinned boot there would re-pull on
every run and fail outright under `BRIG_PULL=never` with the bytes on disk.
brig therefore verifies and boots the tag on such a hull, prints one line
saying so, and the gap remains there until you upgrade: under the default
`missing` pull policy cosign checks the tag in the registry, not necessarily
the copy hull already holds, and `BRIG_PULL=always` is the workaround. brig
never prints the digest-named line on a boot that did not pin a digest, so the
message never claims more than the runtime delivered.

Two things are still narrower on macOS than on Linux. An image pulled under an
older hull has no index digest on record, and a multi-arch tag resolves to its
index digest, so the first pinned boot of such an image after upgrading hull
misses the cache and pulls once; under `BRIG_PULL=never` it fails until the
image is pulled again. And hull does not yet expose the digest its store holds
for a reference, so the report that the local copy differs from what the
registry serves is Linux-only for now. The boot is pinned either way.

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
    '^https://github\.com/brig-sh/brig/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

shasum -a 256 -c checksums.txt --ignore-missing
```

The first command vouches for the checksum file, the second ties every archive
to it. Each archive also ships an SPDX SBOM.

The macOS binaries are signed with a Developer ID certificate and notarized
with Apple as well, because cosign proves provenance and Gatekeeper wants
something else. The two answer different questions and neither substitutes for
the other. Nothing strips the quarantine attribute: it is still on the files
Homebrew installed, and Gatekeeper accepts them anyway, which is what being
notarized buys.

```bash
spctl -a -vv -t install "$(which brig)"  # source=Notarized Developer ID
xattr -l "$(which brig)"                 # com.apple.quarantine, still there
```

A from-source build is neither signed nor notarized, so it is yours to trust
by having built it.

## Telemetry

brig is local-first and asks you to create no account, and a reader who takes
that at face value will not expect a network call to a collector. There is
one, on macOS, so here is what it is.

The runtime brig drives reports usage events over OTLP to an OpenTelemetry
collector run by NOFire AI, with no third-party analytics service in the path.
brig tags those events with its own name, because brig is what you installed
and hull is an implementation detail of it. On Linux the backend is `nerdctl`,
which has no telemetry of any kind, and nothing is sent.

The switch is `brig telemetry off`, which records the answer where the runtime
keeps it, so it holds for every later invocation whether brig is the caller or
not. `DO_NOT_TRACK=1` or `HULL_TELEMETRY_DISABLED=1` in the environment does
the same for one shell without writing anything down, and beats a recorded
yes. `brig telemetry status` reports which of those is currently deciding.

**Nothing is sent before you have answered.** The runtime defaults telemetry
on when it cannot put the question to anyone, and brig's boot invocation gives
it no terminal, so left alone a fresh install would report its first boot
before anyone had been asked. brig suppresses counting for every operation
that runs without a terminal: the boot and the stop until an answer is on
file, and the plumbing always. The one invocation that does inherit your
terminal -- the exec that hands the sandbox to the agent -- is left able to
ask, which is where the question appears. A default is not consent, but
neither is a prompt nobody can ever see.

An event carries a fixed envelope -- schema version, event type, product name
and version, OS version, CPU architecture, an install identifier generated
locally, a timestamp, a checksum -- plus, depending on the event: the brig
subcommand and whether it succeeded, with failures reduced to coarse classes
and never the error text; the hypervisor backend and whether the boot worked;
how long a sandbox lived; sampled RSS and CPU of the VM process while a
command is attached; and a panic type with a path-trimmed stack trace when
something crashes. The install identifier is a random value in
`~/.hull/telemetry.json`; deleting the file rotates it. Crash reports queue in
`~/.hull/crashes/` and can be read or deleted before they are uploaded. IP
addresses are dropped at ingestion, raw events are kept for a year, and
widening what is collected re-asks the question rather than proceeding on the
old answer. Field by field, that is
[hull's telemetry documentation](https://github.com/brig-sh/hull/blob/main/docs/telemetry.md).

These are never collected, and this is a commitment about what brig will send
rather than a note about what it happens to send today:

- workspace paths, and host paths generally
- repository names, branches or remotes
- command arguments, brig's own and the agent's
- agent prompts, and anything the agent read or wrote
- secret names, secret values, and which credentials a run forwarded
- image names and registry references
- network destinations the guest reached
- file metadata: names, sizes, timestamps, counts

The list is the one that matters for this page, because each entry is
something the sandbox boundary above exists to keep private. A workspace path
names the project; an image reference names what you are building; a forwarded
credential name says which vendor you have an account with. Telemetry that
carried any of them would leak across the boundary the rest of this document
describes, out of a channel the user did not think was there. If you find one
of them in a payload, that is a bug and
[SECURITY.md](../SECURITY.md) is how to report it.

`HULL_TELEMETRY_DEBUG=1` prints payloads to stderr instead of sending them,
which is how to check any of this for yourself rather than taking our word for
it.

## Things brig does not claim

It does not sandbox the agent from the network. Outbound traffic from the
guest is whatever the runtime allows, and there is no per-sandbox policy yet.

It does not promise that one sandbox cannot reach another, and what actually
happens depends on the backend. brig asks every runtime for its shared
network, unconditionally and with no setting to say otherwise, so this is a
property of what the runtime does with that request rather than of anything
brig arranges. Measured, in
[docs/manual-tests/sandbox-reachability.md](manual-tests/sandbox-reachability.md):

| backend | can one sandbox reach another? |
| --- | --- |
| `hvi` on macOS | no. The gvisor gateway gives each guest a point-to-point link and translates outbound traffic; there is no segment between guests for one to reach another on |
| `vz` on macOS | no. vmnet does not bridge guest to guest in the mode hull uses |
| Linux | **yes.** The CNI bridge is an ordinary layer 2 segment, and two sandboxes on it reach each other the way two containers do |

So on Linux, two agents you gave *different* credentials sit on one broadcast
domain, each able to reach whatever the other is listening on. That is a real
hole in the narrow-blast-radius argument above: there the radius is narrow per
workspace and per token, not per sandbox. If it matters that two agents cannot
reach each other, run them on separate hosts, or on macOS.

On macOS the separation is real but incidental: it falls out of how the
backends move packets, not from anything brig asks for, and nothing today
would notice if a runtime change took it away. Treat it as a property that
holds rather than a guarantee brig makes. #15 is the work that turns it into
one, and that closes the Linux gap.

It does not filter what the agent writes to your terminal. `brig` hands the
tty over with `syscall.Exec` and is gone before the agent produces a byte,
which is what buys correct `^C` handling and a truthful exit status -- and it
means every byte the agent emits reaches your terminal emulator unexamined.
That is a real surface: OSC 52 writes to, and reads from, the system
clipboard; DCS sequences are forwarded verbatim by `tmux` and `screen` to the
*outer* terminal; and a cursor-position query makes the terminal type its
reply onto your shell's standard input. An agent that has read a hostile
README can do any of those. `hull exec` and `hull logs` do filter, because
hull stays in the middle of that stream; brig deliberately does not stay. If
this matters for your threat model, run brig inside a terminal you are willing
to lose, or through `hull exec`.

It does not protect the workspace from the agent. Everything in there is
writable by design, since that is the work.

It does not stop an agent from spending your money. The `deny` list keeps a
metered key from being forwarded by accident, which is a different and much
smaller promise.
