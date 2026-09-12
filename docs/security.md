# What Brig protects, and what it does not

An agent runs code you did not write. You run it in a sandbox because that
code executes against a machine that holds everything you have. Brig narrows
what "everything" means. On the host, the agent can reach its guest home and
the credentials you gave it. It can also reach any project you name on the
run line. No other host directory is mounted.

On every profile Brig ships, no host credential source is read on the run
path. A profile of your own carrying the deprecated `hostCredential:` key is
the exception: it reads the macOS keychain item it names on every run. See
[secrets.md](secrets.md) for that exception. What the agent can reach over
the network is a separate question, with a much weaker answer, covered below.

If you have found a flaw in one of these boundaries, [SECURITY.md](../SECURITY.md)
is how to report it privately. The section below on
[things brig does not claim](#things-brig-does-not-claim) is the line between a
vulnerability and a known limitation.

## What the agent can reach

The guest can access:

- its guest home, read-write
- the project you name on the run line, read-write at `/work/<name>`
- the credentials you deliver to it
- the internet, on the default `shared` network
- any hostmount volume a profile declares

The guest cannot access:

- any other host directory
- your keychain
- your SSH agent
- your secret manager
- an environment variable a profile's `deny` list refuses

Mounting a project or delivering a credential changes both lists. The agent
can change the real files at those two mounts, not a copy of them. Anything
it can read there it can also send over the network. A credential you deliver
is usable by the agent for as long as it holds it. The agent can misuse it
the same way a person holding it can.

Every hostmount in a shipped profile already lives inside the guest home.
That means no shipped hostmount exposes anything today, a property of the
shipped profiles rather than a guarantee about a profile you write yourself.

A `deny` list only ever covers an environment variable. A `files:` binding
reaches the guest by a different channel, and the list does not see it.

What "the internet" means in practice is in
[Things brig does not claim](#things-brig-does-not-claim) below. So is
whether the guest can reach a service bound on the host.

## The boundary

The sandbox is a microVM on both. On macOS it is booted by
[hull](https://github.com/brig-sh/hull) over Virtualization.framework, which
`brew install --cask brig` brings along. On Linux Brig drives `nerdctl` and
hands the container to the urunc shim (`io.containerd.urunc.v2`), which is the
default rather than the direction. That gives the guest a kernel of its own
there too. `BRIG_CONTAINERD_RUNTIME=runc` asks for a plain container instead, which
shares the host kernel. That is the weaker of the two, and it is something you
have to choose rather than something you get.

Which of them you got is the `ISOLATION` row of the execution envelope, printed
before every boot and by `brig info`:

```
ISOLATION    microVM (hull, hvi backend)
ISOLATION    microVM (hull, vz backend)
ISOLATION    microVM (nerdctl over containerd, io.containerd.urunc.v2)
ISOLATION    container (docker over containerd, runc: the guest shares the host kernel)
```

The row reports what this run resolved: the binary in hand, the backend it
settled on, and the shim it will name. That is not the same as what the
paragraph above promises. A shim Brig does not recognise can still boot a
sandbox, and Brig cannot establish the isolation that sandbox gets from a
shim name alone. So the row says it cannot tell, instead of claiming the
stronger boundary.

Inside it, the guest has your guest home mounted as its home, read-write. Name
a project on the run line and that project is a second host directory, also
mounted read-write, at `/work/<name>`. The agent can change those files too.

A profile's own hostmount volumes are further shares. Every one in a shipped
profile lives inside the guest home already, so nothing extra is exposed
today. That is a property of the shipped profiles, not a guarantee. A
hostmount your own profile declares outside a tmpfs cover is a host path the
guest can see.

Beyond those, the guest does not have your keychain, your SSH agent, your
secret manager, or any other directory on the host. That inaccessibility is
the isolation boundary for everything else. It is also the reason credentials
have to be forwarded in explicitly: the guest cannot fetch them for itself.

## Credentials

**A run on a shipped profile reads no host credential source.** Nothing on the
`brig run`, `exec` or `shell` path reaches a keychain item Brig did not write.
Nothing on that path reaches a credential file outside the guest home, or a
host command that produces one. Two host reads do happen, and no setting
turns either off. Brig runs `git
config --get` in the directory you invoked it from, for `user.name`,
`user.email` and `github.user`, to resolve the commit identity forwarded into
the guest. If `github.user` comes back empty, it then reads the `user:` line
from the stanza for your git host in gh's `hosts.yml`, under `$GH_CONFIG_DIR`
or `~/.config/gh`. That line names the login that pairs with the forwarded
token. That file usually carries gh's own OAuth token as well. Brig takes the login and
nothing else. `BRIG_GIT_IDENTITY=0`, `BRIG_GIT_CONFIG=0` and `BRIG_GIT_USER`
change what Brig does with the answers, not whether it asks. Your host login
enters Brig's own store once, when you type `brig secret import <profile>`,
and every run afterwards reads only that store:

```bash
brig run claude-code               # log in inside the sandbox, or:
brig secret import claude-code     # carry the host login in, once
```

The profile key that read Claude Code's own keychain item on every invocation,
`hostCredential:`, is deprecated and has left the shipped profiles. Brig
removes the field in the next release. `BRIG_CREDENTIALS_CMD`, which pointed
that read at a host command of your own, is already gone: a run that sets it
fails, naming `brig secret import <profile> <name> --from-command '<command>'`.

Until the field goes too, the sentence above is about the profiles Brig ships.
A profile of your own still carrying `hostCredential:` keeps the old
behaviour. That is what deprecating rather than deleting the key means: every
run reads the host item it names. It also raises the approval dialog that
comes with it.
`brig run` warns when it finds one. Moving it to `secrets:` with `sources:` is
what makes the promise above true for your profile too.

A credential reaches the guest by one of two channels, and the profile picks
per secret. `files:` writes it into the guest at the path the agent already
reads. `env:` binds it as an environment variable, for credentials whose
consumer offers no file interface. For an `env.<name>` binding, or the
deprecated `forward:` spelling of one, Brig still reads the named variable
from its own environment. Whatever populates that environment remains a
usable backend for those.

On Linux the store is a Secret Service keyring on your session bus,
gnome-keyring or KWallet ([secrets.md](secrets.md#linux)). A host with no
keyring has no store. A profile whose secrets are *optional* degrades there
rather than failing: the run boots and the agent asks for a login. That is
what `claude-code` does. A **required** secret does fail outright,
because there is nowhere on that host to read it from.

Values are re-read on every exec, so a rotated credential is picked up without
restarting the sandbox. Nothing is written into the guest home from the host
for this.

### What file delivery buys, and what it costs

A credential delivered as a file stays out of `/proc/<pid>/environ` and is not
inherited by processes the agent spawns. It can also be rewritten under a
running agent, so a rotated secret can reach a live session, which no
environment variable can. The bytes land on a memory-backed mount covering
the agent's whole config directory, verified to be `tmpfs` with no swap
before anything is written. So there is no path from the credential to your
disk to check.

Weigh these costs before you rely on file delivery. None of them is small
enough to leave implied.

- **Brig stores and hands over a refresh token.** There is no way to give
  Claude Code a working `.credentials.json` without `refreshToken` and
  `refreshTokenExpiresAt`. A file carrying only an access token is *worse*
  than an environment variable, because the agent attempts a refresh, fails,
  and prompts. So a compromised agent inside the sandbox can mint access
  tokens indefinitely, and keeps doing so after the host's own token has
  expired. The compensating argument is real and belongs beside it: the guest
  refreshes for itself, so a long session stops breaking every few hours. It
  is a trade, taken deliberately.
- **Brig's copy is less protected than the item it came from.** The host's
  Claude item is ACL-scoped to the application that wrote it. That is why it
  raises a dialog the first time something else reads it. The copy Brig
  writes carries the default ACL, the same one every secret in
  [the secret store](#the-secret-store) carries. The same consequence
  applies: keeping the stored copy low-value is the only real mitigation, and
  a refresh token is not low-value. When you are not using it, delete it:
  `brig secret delete claude-credentials`.
- **The denylist stays env-scoped.** A `files:` binding bypasses the deny check
  entirely, and no name check can fix that. A profile can deliver a metered
  API key inside a `settings.json`, and nothing sees it. That is defensible
  rather than a hole. `deny` exists to catch **accident**, an ambient
  variable swept into the guest because it happened to be in your shell. A file
  binding takes an explicit stored secret and an explicit binding written by
  the profile author. Nobody file-binds an `ANTHROPIC_API_KEY` by mistake. The
  two names on `claude-code`'s denylist are env-shaped by the agent's own
  design, so the guard still covers the channel the risk uses.
- **The stored copy does not rotate.** A credential renewed on the host does
  not update Brig's copy. One *revoked* on the host stays valid in Brig's
  store until you re-import or delete it. Brig warns before boot when the
  stored copy has expired, and names the command that refreshes it. It cannot
  see a revocation at all.

A `0600` file is also still readable by anything running as the agent's uid
inside the sandbox. Files narrow the exposure. They do not draw a boundary.
See [what is still exposed](#what-is-still-exposed) below.

Brig applies these rules when it resolves a secret:

- Unset or empty is skipped, so it cannot shadow a value baked into the image.
- A `scheme://` value read from the environment is refused as an unresolved
  secret-manager reference. direnv and friends leave those in the environment
  readily. Forwarded verbatim, it yields "Invalid username or token" in
  the guest, which looks exactly like a broken sandbox. A `value:` literal or
  a value from Brig's own secret store skips this check. Brig put it there
  on purpose, not left behind by a tool that never resolved it. Refusing it
  rejects a perfectly good credential for merely looking like one it is not.
  `BRIG_ALLOW_REFS=1` forwards an ambient reference anyway.
- A variable on the profile's `deny` list is refused, with the reason.

`brig info <agent>` reports the guest's environment, by name, and fails the
same way a run does if a declared secret cannot be resolved. It never
prints a value: a secret-sourced variable comes back annotated, for example
`GH_TOKEN(secret)`, never with the value itself. A credential delivered as a
file is not an environment variable and does not appear in that list at all.

### What reaches host disk

The credential file is the part that does not. It lands on a `tmpfs` mount
covering `~/.claude`, checked to be `tmpfs` with no swap before anything is
written. So `~/.claude/.credentials.json` and the temp file the agent
renames onto it never touch your disk. `brig stop` takes that mount with the sandbox,
which is why an in-sandbox login on this profile does not outlive a stop.

The rest of `~/.claude` is not on that mount. Seven paths under it are
hostmounted, so they live in the guest home on host disk and persist across
boots:

- `settings.json` and `CLAUDE.md`, your permission allowlist and your
  user-level memory, written by hand or by the agent on your instruction.
- `sessions`, `projects` and `history.jsonl`, which are the conversation.
- `plugins` and `skills`, which are also where `--skills` copies your own, so
  leaving either off makes that flag do nothing.

Anything else under `~/.claude` is ephemeral, including anything a future
Claude Code version starts writing there. This list is the `volumes:` block of
the `claude-code` profile, which is the source it follows rather than a
restatement that can drift from it.

### Not in argv

Forwarded values go into the runtime process's own environment, and only the
variable *name* appears on its command line. So a forwarded credential is not
readable in `ps` by other processes on the host.

`BRIG_ENV_ARGV=1` puts them back on the command line for a runtime build that
does not accept a bare `--env KEY`. That gives up the guarantee for a value
read from the environment. A value Brig resolved on your behalf is exempt
from the hatch. It stays off the command line regardless: one bound from its
own secret store, and the host credential too, however that was obtained.
The host durably logs every exec's argv. An opt-in debugging escape hatch has
no business turning that log into a credential leak.

### What is still exposed

The credential is readable inside the sandbox by anything running alongside
the agent. That is inherent: the sandbox cannot use a credential it cannot
see. Docker's sandboxes avoid it by rewriting auth headers in a host-side
proxy. That works for proxied HTTP, but not for `git push`, a vendor CLI's
own token refresh, or an MCP server holding its own connection.

Our answer is a narrow blast radius rather than a sentinel value. Prefer a
fine-grained `GH_TOKEN` scoped to the repositories you want reachable, over a
classic PAT carrying your whole account.

## The secret store

`brig secret` is the one place Brig stores something rather than reading it,
and after the switchover above it is the **only** store a run reads. A profile
names what it wants out of it under `secrets:`, and `brig secret import` is how
a host login gets in.

Prefer that over composing a value into Brig's environment, for one concrete
reason. A value Brig resolved on your behalf is exempt from the
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
  command, base64 value and all, goes to `security -i` down a pipe. Brig's
  own command line is `security -i` and nothing else. This is the same
  guarantee the forwarding path makes above, for the same reason. `security -i`
  reads one command per line and blocks for the next. So the write is on the
  process table only for as long as the pipe stays open. Reproduce it by
  running `security -i` against a fifo, holding the fifo open with an idle
  writer. Send the real `add-generic-password` line down it, then read
  `ps -Ao args` while it sits there. The argv shows `security -i` and nothing
  more. `security find-generic-password` afterwards confirms the value
  really was stored. Closing the fifo and deleting the probe item cleans up.
- **The item's ACL is the default one.** `security` created these items, so
  `security` is trusted to read them back, with no keychain dialog. The
  consequence is the part worth being clear about: **anything that can run
  `/usr/bin/security` as you can read them back too.** That is the same
  boundary as your own shell, and it is weaker than a per-application ACL.
  Brig does not ask for the broad `-A`, but it does not narrow the
  default either. This is the same fact [file delivery](#what-file-delivery-buys-and-what-it-costs)
  states for the Claude credential copy specifically.
- **Values are base64-encoded.** `security -i` reads one command per line, so a
  raw newline in a value ends the command early. Everything after it
  reads as a *second command*. Encoding removes that, and with it the
  need to quote anything. base64 spells everything in letters, digits, `+`, `/`
  and `=`, so no `"` and no `\` ever reaches the line. It also makes the NUL
  byte and the non-UTF8 byte ordinary, which is what lets an SSH key or a
  binary value round-trip at all. The visible cost is that Keychain Access, and
  `security find-generic-password -w` by hand, show base64 rather than the
  secret. It is encoding, not encryption, and protects nothing on its own.
  The keychain does that.
- **A value has a size ceiling, and Brig refuses rather than stores short.**
  That 4096-byte line is the budget for the *whole* command. A longer name
  leaves fewer bytes for the value it names, about 3KB of raw value in
  practice. Every API key and SSH key fits. A 4096-bit RSA private key does
  not. Brig checks the length up front and reads back what it wrote. Rather
  than fail outright, `security` answers a line it cannot fit by shortening
  it and reporting success. See [secrets.md](secrets.md#the-size-limit) for
  the numbers.
- **`brig secret ls` never decrypts.** It reads attributes only, which is why
  listing raises no access prompt and why it can show names and dates but
  never values. Worth being exact about what it reads, though:
  `security dump-keychain` takes no service filter. Its options are
  `[-adhir] [keychain...]` and nothing else, and Brig names no keychain
  either, so the dump covers the whole keychain *search list*. On a stock Mac
  that is your login keychain **and the System keychain**, which is a wider
  net than the login keychain alone. `security list-keychains` shows yours.
  `ls` enumerates the attributes of every item in all of them and discards the
  ones that are not Brig's. Nothing is decrypted and nothing leaves the
  process. Names and dates belonging to other applications, and to the
  system, do pass through Brig on their way to being thrown away.
- **Brig writes only under its own service, which contains Brig rather than
  vouching for what it finds.** Every command that creates, changes or removes
  an item carries `-s sh.brig.secret`, so Brig cannot reach outside that
  namespace. The converse does not follow, though it is the more
  comfortable thing to claim. The service name is a label, not an authenticity
  check, and nothing stops another process running as you from adding an item
  under it. Brig then reads, updates and deletes that item as its own. What
  Brig does instead is degrade honestly. `read` says plainly when a value is
  not in Brig's encoding. `ls` skips a name outside Brig's grammar, so an
  item Brig did not write is either reported or passed over rather than
  presented as yours. An item belonging to another application, Claude Code's
  own, say, is read by `brig secret import` and never written. That read
  *does* raise a dialog, for exactly the ACL reason above: `security` did not
  create it. A run never performs that read at all, which is the point of the
  import verb. The dialog appears when you asked for it, once, and never
  again on the boot path.

On Linux the store is a Secret Service keyring, and a host without one has no
store. `brig secret` says which half is missing, rather than falling back to
a file, which is a downgrade nothing told you about.

## Writing into the workspace

The guest home is mounted read-write, so its contents are the
sandbox's to choose. Brig also writes into it from the host on every
invocation. Those writes are the stale-share marker, the onboarding seed,
the trust key, the guest git files, and the skills copied in by `--skills`.
Put those two facts together and you have the one place where the sandbox
gets to influence what happens on the host. That is worth naming precisely.

Brig runs as you and outside the sandbox. A guest can plant a symlink where
Brig writes next. That aims Brig at a host path the guest itself can never
reach, your `~/.ssh` or your shell profile. Brig follows it anyway, because
to Brig it is just a path inside the guest home. Nothing about the microVM
boundary stops this: the write is on the host side of it.

So every host-side read and write Brig makes inside the guest home goes
through an `os.Root` opened on it. It resolves each path itself, refuses an
absolute symlink outright, and will not let a relative one climb past the
root. A symlink that escapes the guest home this way is refused everywhere,
on a read or a write. There is no window between the check and the open for
the guest to swap the file in.

A symlink that stays *inside* the guest home is a different story, and reading
and writing do not treat it alike. Where Brig writes a state file, the
symlink is refused even though it does not escape. Brig writes only regular
files there, so a link where a state file belongs was put there rather than
left there.

Where Brig reads a file, an internal symlink is followed instead. A
`.gitconfig` symlinked to your own dotfiles, inside the guest home, has to
keep working. The refusal it replaced used to claim such a link "leads out
of" the guest home, which was never true for one that stays inside.

Either way, Brig decides from what it actually opened, not from a check made
beforehand. A fifo, socket, device or directory where a regular file belongs
is refused there. That closes the window where the guest renames one file
type over another between the two.

The one escape a root cannot see is a symlink *at* the guest home or on the
way to it. Resolving that still leaves every path below it honestly "inside
the root". That is checked separately, before anything is created,
and the check has a shape worth knowing. The guest writes as you, so it can
swap an entry only inside a directory you can write. The path to the guest
home is split where the first such directory appears. Above it, every entry
sits where the guest cannot reach, and that part is opened by name. It
follows the links the system or an administrator put there (`/tmp` and
`/var` on macOS are links). From there down, Brig descends one component at
a time against the directory it already holds, and refuses every symlink.
It confirms after each step that what it opened is what it looked at. The
guest home itself is always in the descended part, so a link there is
refused wherever it sits.

Whether you can write a directory is what the kernel says, asked through
`access(2)`, plus ownership: a directory you own you can always `chmod`. So a
group membership past the first sixteen and an ACL both count, on both
platforms. A link above the split whose target passes through a directory
you can write is refused like any other link the guest can reach. A
root-owned `/data` pointing into your home is one example. Name the real
directory instead.

Running Brig as root, ownership tells it nothing, because the guest's writes
are root's too. It then trusts only files that only root was able to write.
That keeps the system's own links usable, but protects nothing: a sandbox
run as root can reach a nested guest home's parents like anything else.
This is not the control for that.

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
remove it before running brig again: a symlink in the workspace leads out of it
```

Read it as what it says. Brig does not create the links it writes through.
One in the way is either something you put there deliberately, or the
sandbox reaching for the host. Neither is a case for retrying: remove the link, or
point the guest home somewhere else.

`--home` pointed at a symlink is refused for the same reason, with the
same kind of message, and is fixed by naming the real directory.

## Guest images

An image is code that will run with your credentials, so Brig checks where it
came from before booting it. The check is cosign's keyless verification. The
question it asks is not "was this signed?" but "was this built by that
workflow, in that repo?":

```bash
cosign verify \
  --certificate-identity-regexp \
    '^https://github\.com/brig-sh/community-images/\.github/workflows/build-images\.yml@refs/heads/main$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/brig-sh/claude-code-stock:latest
```

That works anonymously: the images are public, and the signature lives in
Sigstore's transparency log, not behind a registry login. `:latest` is a
multi-arch index. The per-architecture tags (`:arm64`, `:amd64`) and the
immutable `:<arch>-<sha>` pins are signed the same way and verify with the
same command.

The identity is anchored on the repository and the workflow file. A
signature from anywhere else fails, including one from another workflow in
the same repository.

`BRIG_VERIFY` has three modes: `off`, `warn` and `require`. `warn` is the
default. The table below is `warn`'s behavior: it reports what it found, then
boots anyway, except for the one row with no innocent reading. `require`
refuses anything it cannot positively verify, third-party images included.
`off` skips the check entirely.

What `warn` does with the answer:

| situation | behaviour |
| --- | --- |
| image under `ghcr.io/brig-sh/`, signature verifies | one line saying so, boots |
| image published by somebody else | warning, boots |
| cosign not installed | warning, boots |
| image under `ghcr.io/brig-sh/`, signature fails | stops and asks `[y/N]`. Refuses when there is no terminal |

The asymmetry is deliberate. Bring-your-own images are a supported way to use
Brig, so refusing to boot one makes the feature useless. "Unable to
check" is not the same as "failed". The one case with no innocent reading is
an image sitting under our registry whose signature does not verify. That
is the case that stops.

A typo in `BRIG_VERIFY` refuses the run, naming the three values, rather than
being read as either of them.

Point `BRIG_VERIFY_REGISTRY`, `BRIG_VERIFY_IDENTITY` and `BRIG_VERIFY_ISSUER`
at your own registry and workflow if you publish signed images yourself.

### The digest, not the tag

A tag is a name, and a name can resolve to different bytes in the registry and
in your local store. The provenance claim is about the bytes that run. Brig
resolves the reference to the digest the registry serves before the check,
verifies that digest, and boots it. The object cosign checked is the object
that runs, and the success line names the digest rather than the tag it came
from. A local store holding a different digest under the tag is treated as
the signature-failure row above: it stops. A yes boots the verified
digest rather than the copy on disk. A registry that cannot be reached stops
in the same way. One of our images was not checked, and a
network failure must not be what turns the default mode into "unchecked".
`BRIG_VERIFY=require` refuses outright. Every cosign call is bounded, so an
outage fails in seconds rather than hanging the boot.

All of this is for images under `ghcr.io/brig-sh/`. An image Brig did not
publish carries no signature of ours to check, so Brig makes no cosign call for
it at all. It boots by tag, as it always has, with one line saying whose
image it is. Resolving a digest for it costs a registry round trip on
every boot and buys a pin Brig cannot vouch for.

This holds wherever the runtime's store answers a digest: containerd on
Linux, and hull from 0.1.0-rc23 on macOS. Brig asks the hull it is driving
rather than assuming, because the one on PATH can be older than Brig. An
older hull cannot find a digest reference in its store, so a pinned boot
there re-pulls on every run. It fails outright under `BRIG_PULL=never`
with the bytes on disk. Brig therefore verifies and boots the tag on such a
hull, prints one line saying so, and the gap remains there until you upgrade.
Under the default `missing` pull policy, cosign checks the tag in the
registry, not necessarily the copy hull already holds. `BRIG_PULL=always`
is the workaround. Brig does not name a digest on a boot that did not pin
one.

Two things are still narrower on macOS than on Linux. An image pulled under an
older hull has no index digest on record, and a multi-arch tag resolves to
its index digest. The first pinned boot of such an image after upgrading
hull misses the cache and pulls once. Under `BRIG_PULL=never` it fails until
the image is pulled again. The second gap is that hull does not yet expose
the digest its store holds for a reference. The report that the local copy
differs from what the registry serves is Linux-only for now. The boot is
pinned either way.

`claude-desktop` points at a `ghcr.io/nofireai/` image, and `ubuntu` at
`docker.io/library/ubuntu`. Brig has no signing policy for either registry, so
both warn on every boot until those images move.

### The kernel, not only the image

Six of the eight shipped profiles boot a kernel and an initrd Brig downloads,
rather than one baked into the image. They are `claude-code`, `codex`,
`gemini`, `grok`, `opencode` and `ubuntu`. `cursor` and `claude-desktop` boot
their own image and skip everything below. `cursor` has no published image
today: `brig agent ls` marks it `(no published image)`, so a run of it fails
before any of this applies.

That bundle is checked too, under the same `BRIG_VERIFY` setting, but as a
second and separately-rooted check. Its trust root is its own: registry
prefix `ghcr.io/nofireai/`, signing identity the `build-assets.yml` workflow
in `NOFireAI/hull-assets`, the same issuer as the image check. A signature
that fails stops the boot outright, in every mode except `off`. There is no
`[y/N]` prompt the way there is for the image, because there is no reading of
a bad kernel signature worth asking about.

One thing the check does not buy: nothing is pinned from the result. The
check verifies a registry reference, not the bytes that boot. What actually
starts the guest is whatever kernel and initrd sit in `BRIG_BOOT_ASSETS`, or
failing that the runtime's own asset directory, or the platform default.
Only existence and being non-empty are checked there. Brig does not bind the
kernel on disk to the artifact it just verified. `BRIG_VERIFY_REGISTRY`,
`BRIG_VERIFY_IDENTITY` and `BRIG_VERIFY_ISSUER` repoint the image's trust
policy only: the kernel's identity is fixed.

## Brig's own binaries

Releases are signed with keyless cosign as well. There is no key to
distribute and none for us to lose. The certificate is short-lived, bound to
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

Telemetry must not cross the boundary this page describes. The runtime Brig
drives sends usage events to NOFire AI, on macOS only: nothing is sent on
Linux. `brig telemetry off` (or `DO_NOT_TRACK=1`) turns it off everywhere
it does run. See [docs/telemetry.md](telemetry.md) for how to check the
current state, and for the field-by-field list of what an event carries.
That list is
[hull's stated commitment](https://github.com/brig-sh/hull/blob/main/docs/telemetry.md),
not something this repository verifies: it excludes host paths, repository
names, command arguments and agent prompts. It also excludes secret names
and values, image references, network destinations and file metadata.

Four more hull behaviors matter for checking that commitment yourself.
`HULL_TELEMETRY_DEBUG=1` prints payloads to stderr instead of sending them,
so you can read what an event actually carries. The install identifier is a
random value hull stores in `~/.hull/telemetry.json`. Deleting the file
rotates that identifier. Crash reports queue in `~/.hull/crashes/`, and you
can read or delete one there before it is uploaded. That commitment also
drops IP addresses at ingestion and keeps raw events for a year.

If you find one of those excluded items in a payload, that is a bug and
[SECURITY.md](../SECURITY.md) is how to report it.

## Things brig does not claim

It does not sandbox the agent from the network by default, on any backend. A
sandbox nobody attached a policy to has unrestricted egress. That is what
every sandbox had before policies existed, and what `brig run <agent>` gets on
a fresh install. `--network offline` is the one posture with no route out at
all, on every backend.

Attach one and that changes, on `hvi`: the rules are enforced at the network
gateway Brig gives that sandbox. Every runtime Brig ships refuses to boot a
policy it cannot enforce, rather than booting it unconstrained (see
[docs/policies.md](policies.md)). That refusal is each adapter's own choice,
not a guarantee Brig imposes on every runtime it will ever drive. The
one exception is `--network offline`: a policy-carrying sandbox with no route
out satisfies every rule set, so it is not refused on any backend. Short of
that, on `vz`, on `qemu` and on Linux there is no policy to be had at all.
Outbound traffic there is whatever the runtime allows.

It does not say whether the guest can reach services bound on the host
itself. That covers a dev server, a local model, an MCP server, a metadata
endpoint. Brig adds nothing to narrow that. What the guest's network reaches
is the runtime's default, and whether that includes the host is unmeasured
on every backend. The one control that exists is an egress policy on `hvi`,
with `default: deny` and no `cidr` allow for the host's own ranges. There is
no equivalent on `vz`, `qemu` or Linux.

It does not promise that one sandbox cannot reach another under the default
`shared` network. What happens there depends on the backend. Brig asks the
runtime for its shared network, and what the runtime does with that request
is the runtime's own behaviour, not Brig's. The measurements are in
[docs/manual-tests/sandbox-reachability.md](manual-tests/sandbox-reachability.md).

| backend | can one sandbox reach another? |
| --- | --- |
| `hvi` on macOS | no. A packet capture in both guests shows why: an ARP broadcast from one guest does reach the other, and the other answers, but the gateway does not forward that unicast reply back. The first guest never learns the second's MAC address, so it never sends a packet, and the second guest sees no TCP at all |
| `vz` on macOS | no. vmnet does not carry traffic from one guest to another in the mode hull uses |
| Linux, measured with plain containers on the nerdctl bridge | **yes.** The CNI bridge is an ordinary layer 2 segment, and two sandboxes on it reach each other the way two containers do. The shipped default shim, `io.containerd.urunc.v2`, puts a microVM behind that same bridge, and nothing here has measured whether that changes the answer. Assume it does not |

So on Linux, two agents you gave *different* credentials sit on one broadcast
domain, each able to reach whatever the other is listening on. That is a real
hole in the narrow-blast-radius argument above: there the radius is narrow per
guest home and per token, not per sandbox. If it matters that two agents
cannot reach each other, run them on separate hosts, or on macOS.

On macOS the separation under the *shared* network is real, but it is a
property of the backend rather than something Brig asks for. Brig asks for a
shared network and gets guests that cannot address each other. No test in Brig
notices if a runtime change removes it. Treat it as a property that holds,
not as a guarantee Brig makes.

`--network isolated` is the guarantee. It gives the sandbox a network of its
own. On Linux that is its own CNI network. On `hvi` it is its own gateway
process on a `/30` of its own. No other sandbox is on it, no matter what the
backend does with a shared one. A sandbox carrying an egress policy is
isolated whether or not it asked to be. The rules live on that gateway and
cover everything behind it.

That posture is **refused** on `vz` and on `qemu`, where
Brig owns no network to give. A run asking for it there is stopped and told
which backend implements it. It is not booted onto the shared network under a
row claiming otherwise. The guarantee is about reachability, not resources. An
isolated sandbox is one process and one network more than a shared one, which
is what `brig rm --all` prunes.

It does not filter what the agent writes to your terminal. `brig` hands the
tty over with `syscall.Exec` and is gone before the agent produces a byte.
That buys correct `^C` handling and a truthful exit status. Every byte the
agent emits reaches your terminal emulator unexamined.
The one exception is `--json`: there `brig` stays alive as the agent's parent
so it can print one status line after the agent exits. The tty is still the
agent's. `brig` reads nothing the agent prints and writes nothing to it, and
the exit status is still the agent's own. What changes is only that `brig` is
present for the run rather than gone.
That is a real surface. OSC 52 writes to, and reads from, the system
clipboard. DCS sequences are forwarded verbatim by `tmux` and `screen` to the
*outer* terminal. A cursor-position query makes the terminal type its
reply onto your shell's standard input. An agent that has read a hostile
README can do any of those. `hull exec` and `hull logs` do filter, because
hull stays in the middle of that stream. `brig run` deliberately does not
stay. `brig logs` does: it reads a log back rather than driving a terminal, so
it filters control sequences by default. `--raw` turns that off and hands
you the bytes with the surface above intact. If this matters for your threat
model, run Brig inside a terminal you are willing to lose, or through
`hull exec`.

It does not protect the guest home from the agent. Everything in there is
writable by design, since that is the work.

It does not stop an agent from spending your money. The `deny` list keeps a
metered key from being forwarded by accident, which is a different and much
smaller promise.

## Trust assumptions

Everything above narrows what an agent can reach. None of it removes the need
to trust four things, and it is worth naming them in one place.

**The guest image, and whoever publishes it.** An image under
`ghcr.io/brig-sh/` is checked against a specific build workflow. An image Brig
did not publish boots on a warning under the default mode, and
`BRIG_VERIFY=off` turns the check off for either kind. Either way, the image
is code that runs with your credentials.

**The profile author.** A profile names the image, the volumes it hostmounts,
and any `files:` binding. A `files:` binding is the one channel the deny list
does not cover. A profile is only as careful as whoever wrote it.

**hull or nerdctl.** The kernel boundary, the shared-network separation on
macOS, and every device and namespace decision belong to the runtime, not to
Brig. Brig reports what it resolved. It neither hardens nor weakens what the
runtime does.

**Brig itself.** Brig runs as you, on the host, outside the sandbox. A
resolved credential sits in its process memory as plaintext for the run's
lifetime. Brig clears that memory on the way out. That is defence in depth,
not a control. A process already holding your credentials in memory is not
something this page claims to protect against.
