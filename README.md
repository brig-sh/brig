<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/brig-lockup-on-dark.svg">
    <img alt="brig" src="assets/brig-lockup-on-light.svg" width="300">
  </picture>
</p>

**Run a coding agent in a sandbox where one directory is its whole world and
the rest of the host is out of reach from inside it.** It boots with no
credential at all, which is what keeps trying it cheap: log in inside the
sandbox and that login lives in memory, so `brig stop` takes it with the VM and
the next run asks again.

## TL;DR

```bash
brew tap brig-sh/brig
brew trust brig-sh/brig       # brew refuses untrusted third-party taps
brew install --cask brig

brig run claude               # boots a sandbox, starts Claude Code in it
```

Put the projects the agent should work on in `~/brig/claude-code`. That
directory is the agent's entire world; no other directory on your machine is
mounted into it, and it cannot reach your keychain, SSH agent or secret
manager. The sandbox stays up between commands, so the second `brig run` is
immediate.

<details>
<summary>Building from source, or on Linux</summary>

```bash
git clone https://github.com/brig-sh/brig && cd brig
make build                    # produces ./brig and ./brigd
```

macOS needs `hull` on PATH (see [macOS](#macos) above); Linux needs
`nerdctl`. brig finds either. `cosign` is optional but recommended -- without
it, guest images cannot be verified and brig says so on every boot.

Two tools are needed only by a profile marked `genericBoot`, which boots an
image that was never built to be a guest and therefore needs a kernel and an
initrd brig has to fetch. On macOS that is hull, which is already there and
downloads them for its own use. On Linux it is `oras`, because hull does not
build there -- without it brig says so and tells you what to fetch. Neither is
needed for an image that carries its own kernel.
</details>

## macOS

On macOS the sandbox is a microVM, booted by
[hull](https://github.com/brig-sh/hull) over Virtualization.framework. The
Homebrew cask depends on it, so `brew install --cask brig` brings it along.

Building both from source works too:

```bash
git clone https://github.com/brig-sh/hull && cd hull && make build
```

hull ships a `vz-runner` helper that needs the
`com.apple.security.virtualization` entitlement, so a from-source build has to
be signed with a Developer ID certificate to boot a VM. The released build is
signed, notarized and stapled, which is the path to prefer.
[docs/runtimes.md](docs/runtimes.md#building-hull-from-source-on-macos) states
the signing requirement exactly, including the separate entitlement that the
`hvi` backend the shipped profiles ask for carries.

On Linux brig drives `nerdctl` over containerd, and hands the container to
the [urunc](https://github.com/urunc-dev/urunc) shim
(`io.containerd.urunc.v2`), so the sandbox is a microVM there too. Point
`BRIG_CONTAINERD_RUNTIME` at `runc` if you want a plain container instead.

## What brig is

`brig` boots an agent CLI -- Claude Code, Codex, and others -- inside a microVM
on macOS and on Linux alike, mounts one directory as the agent's home, and
forwards your credentials into it per exec.

<p align="center">
  <img alt="brig sandbox architecture: brig run &lt;agent&gt; resolves the session on the host, hull over Hypervisor.framework on macOS and urunc over KVM on Linux boot it, and both give the guest the same contract" src="assets/architecture.svg" width="900">
</p>

It is not a container runtime, and does not want to be one. It delegates every
mechanical operation -- boot this image, exec in it, stop it -- to `hull` on
macOS and `nerdctl` on Linux, and adds only the four things those tools have
no concept of:

- **Workspace as guest home.** One host directory is the agent's whole world
  and the unit of persistence: its logins, its settings and your projects live
  there and survive a restart.
- **Host-resolved credentials, handed in explicitly.** The guest cannot reach
  your keychain, your secret manager or your SSH agent -- that inaccessibility
  *is* the isolation boundary -- so brig resolves credentials on the host and
  passes them in. Only from its own store, and only what a profile names: you
  say once that a host login may enter a sandbox (`brig secret import`), and a
  run on a shipped profile never reads another application's keychain. A
  profile of your own still carrying the deprecated `hostCredential:` is the
  exception, and reads the host item it names on every run. Never written into
  the workspace, and never placed in a command line where `ps` could read it.
- **A billing denylist.** `ANTHROPIC_API_KEY` outranks the OAuth token in
  Claude Code's own precedence, so forwarding it would move a sandbox from
  your subscription onto metered API billing without saying so. It is refused
  by default. Codex gets the same treatment for `OPENAI_API_KEY`.
- **Guest image verification.** Images are checked against the workflow that
  built them before they boot.

Both operating systems run the same library. There is no macOS behaviour and a
separate Linux re-implementation of it.

## Commands

| command | what it does |
| --- | --- |
| `brig run <ref> [args…]` | start the sandbox if needed, then run the agent. Arguments pass through untouched. `-d` starts it and exits, printing its name |
| `brig sh <ref> [cmd…]` | a login shell inside the sandbox, or one command in it |
| `brig stop <ref>` | stop the sandbox, keep it. Starting it again is a boot, not a fresh creation |
| `brig rm <ref>` | stop and remove the sandbox. The workspace is untouched |
| `brig rm --all` | stop and remove every brig sandbox. Workspaces are untouched |
| `brig ls [-q]` | every brig sandbox, running or merely holding its name, with its ref and workspace. `-q` prints the refs alone, for a script |
| `brig info <ref>` | the boundary a run would trust -- sandbox, workspace, image, credentials **by name only** -- and whether the guest will be authenticated |
| `brig agent ls` | the agents, their images, and what each one refuses to forward |
| `brig agent show <agent>` | print one agent's spec. `--json` for JSON instead of YAML |
| `brig agent new <name> --from <agent>` | copy an agent under a new name, ready to edit |
| `brig agent edit\|rm\|import\|export` | manage agents |
| `brig policy ls\|create\|edit\|show\|rm` | manage policies. `show --json` for JSON instead of YAML |
| `brig secret create\|read\|update\|delete\|ls` | keep secrets in your keyring. macOS only for now |
| `brig secret import <profile>` | fill that profile's secrets from your host, once. macOS only for now |
| `brig telemetry status\|on\|off` | report what is counted, or turn the counting on or off. See [Telemetry](#telemetry) |
| `brig version` | |

A `<ref>` is the session: `claude` is that agent's default session, and
`claude@refactor` is a session of its own. `brig ls` prints the ref of every
sandbox, and every verb above takes one -- see [Named sessions](#named-sessions).

The older spellings all still work and each prints a one-line note naming the
new one, so nothing scripted breaks in this release; they no longer appear
above. `brig profiles` and the whole `brig profile` group are now `brig agent`,
`brig policies` is `brig policy ls`, and top-level `brig import` and
`brig export` are `brig agent import` and `brig agent export`. So are the
undocumented aliases that were never in this table -- `list`, `save`, `load`,
`secret rm`. `brig template …` and `brig agents` remain from an earlier rename,
and there is deliberately no `brig template edit`.

The lifecycle verbs collapsed in the same way. `brig exec` and `brig shell` are
both `brig sh`, which runs a command when you give it one and opens a login
shell when you do not -- one verb, and which of the two you get is said by the
line rather than by the word you reached for. `brig create` is `brig run -d`,
`brig reset` is `brig rm --all`, and `brig env` is `brig info`. `-n`/`--name` is
the label: `brig run claude@refactor` is what `brig run claude --name refactor`
was. That one is mapped and warned about rather than quietly reinterpreted,
because `--name` is also Claude Code's own flag.

`brig secret` keeps `create|read|update|delete`, and that is deliberate rather
than an oversight. The tidier `set`/`get` pair would make one typo turn a read
into a write, silently replacing a stored credential with no way back to the
original value. The inconsistency is the cheaper of the two.

A brig line has three places a token can stand, and they are not the same kind
of place. Left of the verb are brig's global flags, a closed set -- an unknown
flag there is a usage error naming the token, rather than an operand quietly
swallowed. Between the verb and the agent are the run-line flags below. Right of
the agent the vocabulary is the agent's, and `--` ends brig's parsing outright,
so an agent flag spelled like one of brig's still reaches it.

brig does still read its own flags after the agent, because `brig run claude -q`
is a line that works. But once an unknown token has ended brig's reading, a brig
flag further along belongs to the agent -- and brig now says so, without taking
the token, so a line that works today keeps working.

| flag | what it does |
| --- | --- |
| `-n, --name NAME` | a session of its own: own workspace, own sandbox. Also written `<agent>@<label>` |
| `--image IMAGE` | guest image to boot (`-t` still works, with a note) |
| `-w, --workspace PATH` | host directory to mount as the guest home |
| `--mem MB` | guest memory (`--memory` also works; `-m` works with a note) |
| `--cpus N` | guest vCPUs |
| `-d, --detach` | with `run`: start the sandbox, print its name, exit |
| `--skills` | seed your own `~/.claude` skills and plugins into the workspace |
| `--network MODE` | `shared`, `isolated` or `offline` (or `BRIG_NETWORK`) |
| `--offline` | shorthand for `--network offline`: no route out of the sandbox |

Each flag overrides the corresponding `BRIG_*` setting, which overrides the
profile.

### Exit codes

brig returns a small, stable set of exit codes so a script can tell what went
wrong without reading the message. Anything a command prints on success stays on
`stdout`; every failure below is reported on `stderr`.

| code | meaning |
| --- | --- |
| `0` | success |
| `1` | general failure -- something ran and did not finish |
| `2` | usage error -- an unknown flag, a stray argument, a value in the wrong place |
| `3` | no such profile or sandbox -- the name resolves to nothing |
| `4` | runtime unavailable -- none is installed, or the one named is broken |
| `5` | image verification refused the boot -- see [guest image verification](#guest-image-verification) |
| `6` | a credential the run needed could not be resolved |

`1` and `2` are unchanged from earlier releases. The rest name failures brig
already produced under `1`, so a caller that only checked "zero or not" keeps
working; one that wants to branch on the reason now can.

```bash
brig run claude -p "summarise this repo"   # headless, arguments passed through
brig run claude -- --name not-a-session    # --name reaches claude, not brig
brig sh codex 'ls -la ~'                   # one command, in a login shell
brig sh codex                              # a login shell, no command
```

### Named sessions

```bash
brig run claude@refactor
brig run claude --name refactor    # the same session, the older spelling
```

A session of its own: its own workspace (`~/brig/claude-code-refactor`), its
own sandbox, and the name you typed reaches the agent as its display name.

Paths use a lowercase form of the name -- letters, digits, dot, dash and
underscore -- so `my_project` and `my-project` stay separate, while `Foo` and
`foo` do not (macOS filesystems ignore case, and two names sharing one directory
but not one VM is a bug waiting to happen). Nothing is shortened, so the name you
pick is the directory you get. If the slug differs from what you typed, brig says
which directory it used. `brig info claude@refactor` prints the one in use.

The `@` form is the stricter of the two: a label brig would have to rewrite --
uppercase, a space, a slash -- is refused rather than quietly rewritten, and the
message names what it would have become. That is what makes the label safe to use
as an address: nothing is silently mapped onto a directory you did not ask for.
`--name` keeps its older, lenient behaviour.

**Every verb takes the ref.** A named session is addressed as
`<agent>@<label>`, and the older agent-plus-`--name` spelling still works
everywhere it did:

```bash
brig sh   claude@refactor git status
brig stop claude@refactor
brig rm   claude@refactor      # removes the sandbox, keeps the workspace
```

**`brig ls` prints the ref**, in the first column, and that is the string every
verb takes:

```
REF                   SANDBOX                    STATE     WORKSPACE
claude-code           brig-claude-code           running   /Users/you/brig/claude-code
claude-code@refactor  brig-claude-code-refactor  stopped   /Users/you/brig/claude-code-refactor
```

`brig ls -q` prints the refs alone, one to a line, so a script can read the
listing and hand what it finds straight back to a verb:

```bash
brig ls -q | while read -r ref; do brig stop "$ref"; done
```

The `SANDBOX` column is the sandbox's own name, which is not a ref: `brig rm
brig-claude-code-refactor` fails with "unknown profile", and it is there to be
recognised in the runtime's own output rather than to be typed at brig. A
sandbox brig has no session recorded for and cannot read a ref out of the name
of -- one renamed with `BRIG_NAME`, say -- shows a `-` in the `REF` column and is
left out of `brig ls -q` entirely, because every line of that output is meant to
be a word a verb takes. `brig rm --all` is what clears one of those.

To drop the workspace too, remove the directory `brig info` prints. And
`brig rm --all` stops and removes every brig sandbox at once, named ones
included, leaving all workspaces alone.

**`-w` is remembered.** A session created with `--workspace` keeps that
directory for every later verb, so `brig sh claude@refactor ls` finds it
without repeating the flag. brig records it in `~/.brig/sessions.json`, filed
under the session's ref, and reads it back when neither `--workspace` nor
`BRIG_WORKSPACE` names one; `brig rm` drops the entry with the sandbox, and
`brig ls` drops any whose sandbox has gone. That file is also where `brig ls`
reads the ref it prints.

That file is an index and not a record of the truth: the runtime stays the
authority on what is actually mounted, and an unreadable index costs one restart
rather than failing the command. Because it is filed under the ref, a session is
found under either spelling of its agent -- `claude` and `claude-code` are one
agent through an alias, and both reach the one entry. Pass either of them a directory that is not the one the sandbox is
mounting and it restarts, as it always has -- a share cannot be moved on a
running guest, and you asked for a different directory.

### If you know Docker Sandboxes (`sbx`)

The verbs line up deliberately, so muscle memory carries over:

| `sbx` | `brig` | note |
| --- | --- | --- |
| `sbx run <agent> [path]` | `brig run <agent> [-w path]` | brig takes the workspace as a flag rather than a positional, so it never has to guess where agent arguments begin |
| `sbx create` | `brig run -d` | starts the sandbox and exits, printing its name |
| `sbx ls` / `sbx stop` / `sbx rm` | same | |
| `sbx exec` | `brig sh` | one verb for a command and for a login shell |
| `sbx reset` | `brig rm --all` | the same removal as `brig rm`, over every sandbox |
| `sbx cp` | n/a | the workspace is a live host directory, so there is nothing to copy across |
| `sbx template ls\|save\|load\|rm` | `brig agent ls\|export\|import\|edit\|rm` | brig's profiles describe the *agent*, not just its image, and are YAML like sbx's kits |
| `sbx secret set` | `brig secret create` + a profile's `secrets:` | brig has a store of its own and binds from it. Or point a profile at your existing secret manager: whatever puts a value in brig's environment is enough |
| `sbx -t/--template` | `brig -t/--image` | |
| `sbx -m/--memory`, `--cpus`, `-d` | same | |
| `sbx --name <name>` | `brig <agent>@<name>` | brig's `--name` still works and says what replaces it |
| `sbx --publish`, `--deny-network` | n/a | not yet; brig does not manage guest networking, and sandboxes share one network -- see [docs/security.md](docs/security.md#things-brig-does-not-claim) |
| `sbx --clone` | n/a | not yet; the workspace is mounted directly |
| `sbx login` | n/a | nothing to sign in to |

The deeper differences are the interesting ones. sbx keeps secrets out of the
guest entirely by rewriting auth headers in a host-side proxy; brig forwards
the credential into the guest, because the agent has to be able to use it for
anything other than a proxied HTTP call -- `git push`, a vendor CLI's own
refresh, an MCP server. brig's answer to that exposure is a narrow blast
radius (one workspace, a fine-grained token) rather than a sentinel value.

## Credentials

**A sandbox with no credential still boots.** `brig run claude-code` starts the
agent and the agent asks you to log in, in there, exactly as it would on a
machine you had just set up. Everything below is optional.

```bash
brig run claude-code               # log in inside the sandbox, or:
brig secret import claude-code     # carry the login already on this Mac in, once
```

That in-sandbox login lasts as long as the sandbox does. It is written to
`~/.claude/.credentials.json`, which sits on a memory-backed mount and never
reaches host disk, so `brig stop` takes it with the rest of the VM and the next
`brig run` asks again. Other paths under `~/.claude` do reach host disk by
design; [docs/security.md](docs/security.md#what-reaches-host-disk) names them
and says why. Importing is what makes a login outlive a stop: brig keeps that
copy in your keyring and writes it back in on every boot. If you log in inside
the sandbox on a host that has no login of its own, expect to repeat it.

`import` reads your host login **once, when you type it**, and copies it into
brig's own store. Every run after that reads only that store. On a shipped
profile brig never opens another application's keychain item on the run path,
so a run raises no approval dialog and performs no host read you did not ask
for. A profile of your own still on the deprecated `hostCredential:` is the
exception: it reads the host item it names on every run.

The copy is a copy: renewing or revoking the login on the host does not change
it. Run `brig secret import claude-code` again to refresh it, or
`brig secret delete claude-credentials` to be rid of it. brig warns before boot
when the stored copy has expired.

Claude Code's credential arrives as a **file**, at the path the agent already
reads -- `~/.claude/.credentials.json` in the guest -- on memory-backed storage
that never reaches your disk. The agent refreshes it in there on its own, so a
long session does not break every few hours. `docs/security.md` states what
that costs, because it is not free: the document brig stores and hands over
contains a **refresh token**.

Credentials that no agent reads from a file still travel as environment
variables -- `GH_TOKEN` for git over HTTPS, and the API keys that are env-only
by their own tools' design. Three rules apply to every one of them:

- **Unset or empty is skipped**, so it cannot shadow a value baked into the
  image.
- **A `scheme://` value read from the environment is refused** -- direnv and
  friends readily leave a secret-manager reference in the environment
  unresolved, and forwarded verbatim it yields "Invalid username or token" in
  the guest, which looks exactly like a broken sandbox. A stored secret or a
  `value:` literal skips this check, having been put there on purpose.
  `BRIG_ALLOW_REFS=1` forwards an ambient one anyway.
- **A denylisted variable is refused**, with the reason. `BRIG_ALLOW_DENIED=1`
  if metered billing is genuinely what you want.

`brig info <agent>` reports what would be forwarded, by name, and whether the
guest will actually be authenticated -- an expired stored credential is exactly
what sends a sandbox back to its login screen.

Values reach the runtime through its environment, not its command line, so a
forwarded credential is not readable in `ps`. Inside the sandbox it is
readable by anything running alongside the agent, which is inherent: the
sandbox cannot use a credential it cannot see. Prefer a fine-grained
`GH_TOKEN` scoped to the repositories you want reachable.

### The secret store

`brig secret` is a store of brig's own, and it is the only store a run reads.
`brig secret import` fills it from your host; these verbs are how you fill it
by hand, from any backend you like. On macOS it keeps each secret in your login
keychain; there is no Linux backend yet, and brig says so rather than falling
back to a file.

```bash
printf %s "$TOKEN" | brig secret create gh-token   # the value comes from stdin
brig secret create deploy-key -f ~/.ssh/id_ed25519 # ... or from a file, verbatim
brig secret read gh-token
brig secret ls                                     # names and dates, never values
brig secret delete gh-token                        # asks first; -y answers ahead
```

The value is never an argument, so it stays out of `ps` and out of your shell
history. `create` refuses to overwrite and `update` refuses to create, so a
typo in a name is a message rather than a silently lost secret.

A profile declares the names it wants out of this store, so a stored secret
reaches a sandbox on its own -- as a file where the agent reads one, and as an
environment variable where it does not. `brig agent ls` prints each agent's
list, and which of those names `brig secret import` can fill for you:

```
claude-code     Claude Code (Anthropic)
                secrets: claude-credentials gh-token
                  from your host: brig secret import claude-code (claude-credentials)
                  by hand: brig secret create <name> (gh-token)
```

A secret that is missing does not stop `claude-code` from booting: both of its
names are optional, and the run says what it could not find and carries on.
[docs/profiles.md](docs/profiles.md) is how to declare them in a profile of
your own.

```console
$ brig info claude
PROFILE      claude-code
SANDBOX      brig-claude-code (hull)
ISOLATION    microVM (hull, vz backend)
WORKSPACE    /Users/you/brig/claude-code (read-write)
IMAGE        ghcr.io/brig-sh/claude-code-stock:latest (pull missing)
CREDENTIALS  GH_TOKEN
brig: forwarding to guest:
brig:   GH_TOKEN(secret)
```

The block at the top is the execution envelope: the boundary the run would
trust, printed before the sandbox boots. `brig run` prints the same block before
it starts one, so what you preview is what you get. Names only, never values.
`--quiet` drops it.

A secret a profile declares but the store does not have fails the run before
any sandbox is created, naming every one that is missing and the command that
creates it. This is the path to prefer over composing a value into brig's
environment: a value brig resolved itself is exempt from `BRIG_ENV_ARGV` and
stays off the runtime's command line whatever that is set to.

[docs/secrets.md](docs/secrets.md) is the usage guide -- the verbs, the
trailing-newline rule, the name grammar and the size limit. What the keychain
does and does not protect is
[docs/security.md](docs/security.md#the-secret-store).

## Guest image verification

Before booting an image, brig asks cosign not "was this signed?" but "was this
built by that workflow, in that repo?" -- the identity is anchored on the
repository *and* the workflow file, so a signature from anywhere else fails.

| situation | what happens |
| --- | --- |
| image under `ghcr.io/brig-sh/`, signature verifies | one line saying so, boots |
| image published by someone else | **warning**, boots. Bring-your-own images are a supported way to use brig |
| `cosign` not installed | **warning**, boots. "Could not check" is not "failed" |
| image under `ghcr.io/brig-sh/`, signature does **not** verify | **stops and asks** `[y/N]`. With no terminal it refuses |

Most profiles boot a kernel and an initrd brig downloads rather than their own
image, and the initrd carries the in-guest agent. That bundle is checked the
same way, against its own signing identity, under the same setting: a signature
that does not verify stops the boot, a bundle brig does not publish warns, and
`BRIG_VERIFY=require` refuses anything unchecked. The kernel is the more
privileged of the two, so verifying the image alone was never the whole
guarantee.

`BRIG_VERIFY=require` refuses anything that cannot be positively verified,
third-party images included. `BRIG_VERIFY=off` skips the check.
[docs/security.md](docs/security.md) explains the reasoning, and what brig does
not protect you from.

For an image under `ghcr.io/brig-sh/`, brig resolves the tag to a digest,
verifies that digest, and boots it, so the image it checked is the image that
runs and the line saying so names the digest. If the registry cannot be reached
the boot stops and asks, as it does for a bad signature. Images brig did not
publish boot by tag with no registry round trip, as before. Pinning needs a
runtime whose store answers a digest: containerd on Linux, and hull from
0.1.0-rc23 on macOS. An older hull cannot, so brig verifies and boots the
tag there and says so; under the default `missing` pull policy that check is of
the tag in the registry, not necessarily the copy already in your store, and
`BRIG_PULL=always` is the workaround until you upgrade. `claude-desktop` and
`ubuntu` still point at `ghcr.io/nofireai/` images, so they warn on every boot
until those move.

### Image tags

The five published agents default to `:latest`, a multi-arch index covering
`linux/arm64` and `linux/amd64`. One reference is therefore right on an Apple
Silicon Mac and on an x86 Linux host -- the runtime resolves the index and
pulls the matching manifest.

| tag | what it is |
| --- | --- |
| `:latest` | multi-arch index, arm64 + amd64. What the profiles use |
| `:arm64`, `:amd64` | one architecture |
| `:<arch>-<sha>` | immutable pin: that architecture, that build, permanently |

All of them are signed the same way and verify with the same command.

`:latest` moves when a new image is published, and under the default `missing`
pull policy a cached tag is never re-resolved -- so a republished image stays
invisible until you set `BRIG_PULL=always` or clear the store. When you want a
guest that cannot move under you, pin the build:

```bash
brig run claude --image ghcr.io/brig-sh/claude-code:arm64-98f4739
```

`cursor` is built by community-images but deliberately not published, pending
a terms check. `brig run cursor` says so rather than failing on a registry
404; build the image yourself and pass `--image`.

## Telemetry

brig counts usage, and this section says so plainly because nothing else about
the tool would lead you to expect it. There is no account and no server of
brig's own, but the runtime brig drives on macOS reports a handful of events to
a collector run by NOFire AI, tagged with brig's name because brig is the
product you installed. On Linux, where the backend is `nerdctl`, nothing is
sent at all.

Turning it off is one line, and it is durable rather than per-shell:

```bash
brig telemetry off      # or: DO_NOT_TRACK=1 in your environment
brig telemetry status   # what the current answer is, and what decided it
```

It is on by default once you answer yes, and the question is asked on the
first command that hands your terminal to an agent. Until it has been
answered, brig sends nothing: a sandbox boot happens with no terminal
attached, so nobody could have been asked, and brig suppresses counting for it
rather than letting a default stand in for consent.

An event carries the product name and version, the OS version and CPU
architecture, an install identifier generated on your machine (delete
`~/.hull/telemetry.json` to rotate it), a timestamp and a checksum. On top of
that envelope: which runtime operation ran and whether it succeeded, with
failures bucketed into coarse classes such as `network` or `permission` and
never the error text; which hypervisor backend booted and whether the boot
worked; how long a sandbox lived; sampled memory and CPU of the VM process
while a command is attached; and, if brig or the runtime panics, the panic
type with a stack trace whose paths are trimmed. Crash reports queue in
`~/.hull/crashes/`, where you can read or delete them before they go anywhere.

What is never collected, as a commitment rather than a description of the
current build:

- workspace paths, or any host path
- repository names, branches or remotes
- command arguments, including the agent's own
- agent prompts, or anything the agent read or wrote
- secret names, secret values, or which credentials were forwarded
- image names and registry references
- network destinations the guest reached
- file metadata: names, sizes, timestamps, counts

Your IP address is not stored: it is discarded at ingestion. Raw events are
kept for a year. If a future version widens what it collects, the question is
asked again with the new list, and nothing from the wider set is sent until
you answer it.

The full field-by-field description is
[hull's telemetry documentation](https://github.com/brig-sh/hull/blob/main/docs/telemetry.md),
since hull is what does the sending. `HULL_TELEMETRY_DISABLED=1` and
`DO_NOT_TRACK=1` both reach it untouched and both win over anything recorded on
disk.

## Documentation

The README is the overview. The details live in [docs/](docs/):

- [quickstart.md](docs/quickstart.md) -- from install to a running agent, one page
- [troubleshooting.md](docs/troubleshooting.md) -- when a run fails, organised by what you saw
- [profiles.md](docs/profiles.md) -- writing an agent profile, field by field
- [policies.md](docs/policies.md) -- writing an egress policy, verb by verb, with a worked example
- [secrets.md](docs/secrets.md) -- `brig secret`, verb by verb, with a worked example
- [security.md](docs/security.md) -- what the boundary is, and what it is not
- [non-goals.md](docs/non-goals.md) -- what brig will not do, with the reason
  and what would reopen each one
- [brigd.md](docs/brigd.md) -- the session daemon and its protocol
- [runtimes.md](docs/runtimes.md) -- hull, nerdctl and urunc: what each one is, its licence, and every command brig runs against it
- [support.md](docs/support.md) -- which computers brig runs on

Found a security problem? [SECURITY.md](SECURITY.md) is how to report it
privately, rather than in a public issue.

## Custom agents and your own images

Any Linux CLI in an OCI image runs under brig, as long as the image also
carries the utilities brig invokes to set the sandbox up and deliver the
credential. [docs/guest-image.md](docs/guest-image.md) is the list, with the
file that runs each one, and `script/check-guest-image.sh <image> [profile]`
checks an image against it by booting it as that profile's sandbox, default
`claude-code`. A stock distribution image passes as it ships; a
`FROM scratch` image holding only your static binary fails every line. A
profile just saves you spelling out the image, the guest home and the
credential variables every time:

```bash
brig agent new mine --from claude-code   # start from the closest one
brig agent edit mine                     # change the image, forward/deny
brig run mine
```

The name is a name, not a path: brig writes `~/.config/brig/mine.yaml`, which
is where a profile file has to be for brig to read it back. It is the agent's
name as well as the file's -- `new` writes `name: mine` into the file, so `mine`
is what every later command takes, `brig agent rm mine` included. Everything
else in there still describes claude-code, which is what the edit on the second
line is for. `new` writes that directory and nowhere else, so a path is refused
rather than honoured -- redirect stdout (`brig agent show claude-code >
mine.yaml`) if you want a copy of your own. It also refuses to overwrite an existing file
unless you pass `--force`, since an export is generated bytes and the file it
would replace is not.

Profiles are **YAML or JSON** -- JSON is a subset of YAML, so one parser reads
both and nothing has to guess. Export writes YAML, because a profile is a
file a person edits and YAML has comments; the exported file carries a header
explaining every field. `brig agent show --json` for anything consuming
profiles programmatically.

An imported file is stored byte for byte as you wrote it, so your comments and
your ordering survive:

```yaml
# A brig profile. Edit it, then: brig agent import <this file>
# ...
name: mine
image: docker.io/me/mine:latest
guestHome: /home/mine
binary: mine
forward: [GH_TOKEN]      # inline lists work too
mem: 8192                # this CLI is a memory hog
cpus: 2
```

A misspelled field is refused rather than ignored -- `forwards:` would
otherwise decode into nothing and forward no credentials, which looks exactly
like a broken sandbox.

A custom profile may take a built-in's name -- that is how you pin your own
image for an agent brig already knows about. `brig agent ls` marks those
`(file, overrides built-in)`. Your own live in `$XDG_CONFIG_HOME/brig`
(default `~/.config/brig`), one file each; the directory starts empty and
brig never writes there unless you ask it to.

[docs/profiles.md](docs/profiles.md) walks through the fields with a worked
example. Building an image for one is documented in
[community-images/docs/bring-your-own-image.md](https://github.com/brig-sh/community-images/blob/main/docs/bring-your-own-image.md).

## Git in the guest

```bash
BRIG_GIT_CONFIG=1 brig run claude
```

Off by default, because turning it on writes two files into your workspace: a
credential helper that reads `GH_TOKEN` from the guest environment, and a
gitconfig that rewrites SSH GitHub remotes to HTTPS (the guest has no SSH
agent, so SSH remotes cannot work there). Neither file holds a secret. Set
your login once with `git config --global github.user <login>`.

Your commit identity is forwarded as environment, resolved from the directory
you invoked brig in, so per-directory `includeIf` rules carry into the guest.
Guest commits are unsigned: signing needs a host-side agent the guest cannot
reach.

`GIT_TERMINAL_PROMPT=0` is forwarded unconditionally -- inside an agent session
there is nobody to answer a credential prompt, so git would simply hang.

## Your own skills in the guest

```bash
brig run claude --skills          # or BRIG_SKILLS=1
```

Off by default. With it on, brig seeds the directories the profile lists in
`hostConfigDir` and `projectPaths` -- `~/.claude/skills` and
`~/.claude/plugins` for Claude Code -- into the workspace, mirroring the host
layout, so the agent finds them at `~/.claude/skills` in the guest because the
workspace *is* the guest home.

**They are copied, not mounted read-only.** Read-only is what you would expect
here, and it looks like the careful choice, but it is the wrong one: agents
write inside these directories -- installing a plugin, populating a cache --
and a read-only mount turns that into an I/O error the agent cannot handle.
So the guest gets its own writable copy and your directory on the host is
never written to, which is what read-only was for. What follows from that is
worth knowing:

- The copy is entry by entry, and only what is missing. A plugin the guest
  installed is not clobbered on the next start, and a skill you add on the
  host later still arrives.
- The guest's copy wins. Once an entry exists in the workspace, brig leaves it
  alone -- so editing a skill on the host does not update the one in the
  sandbox. Delete it from the workspace to have it seeded again.
- A path you do not have is skipped rather than refused, so skills without
  plugins is not a case you have to care about.
- The sandbox can change its copy, because it is a file in the workspace like
  any other. It cannot change yours.

## Environment variables

Every setting is read in this order, first hit wins:

```
BRIG_<AGENT>_<KEY>   →   BRIG_<KEY>
```

`<AGENT>` is the profile name uppercased with dashes as underscores
(`BRIG_CLAUDE_CODE_WORKSPACE`), so one shell can carry different settings for
two agents.

Booleans are shell-style: anything except `0` is on.

### Where things live

| variable | default | what it does |
| --- | --- | --- |
| `BRIG_WORKSPACE` | `~/brig/<agent>` | Host directory mounted as the guest home. A named session appends `-<slug>` |
| `BRIG_NAME` | `brig-<agent>` | Sandbox (VM or container) name; must begin with `brig-`. A named session appends `-<slug>` |
| `BRIG_PROFILE_DIR` | `~/.config/brig` | Where custom profiles are read from and written to (`BRIG_TEMPLATE_DIR` still works) |
| `BRIG_STATE_DIR` | `~/.brig` | Where brig keeps what has to outlive one command, including the workspace each sandbox was started with. Bookkeeping only: an unusable file there costs a restart, never a failed command |

### Image and guest

| variable | default | what it does |
| --- | --- | --- |
| `BRIG_IMAGE` | per profile | Guest image to boot |
| `BRIG_PULL` | `missing` | `missing`, `always` or `never`. A cached tag is not re-resolved, so a republished moving tag stays invisible until you say `always` |
| `BRIG_MEM` | per profile (4096) | Guest memory, MB |
| `BRIG_CPUS` | per profile (4) | Guest vCPUs |
| `BRIG_READY_TIMEOUT` | `30` | Seconds to wait for the in-guest agent after the runtime reports the sandbox running. The two are not the same moment |
| `BRIG_TITLE` | per profile | Window title, for a graphical agent |
| `BRIG_NETWORK` | per profile (`shared`) | `shared`, `isolated` or `offline`. `isolated` gives the sandbox a network of its own, which on Linux is what keeps two sandboxes from reaching each other; the macOS backends already keep them apart. `offline` boots the sandbox with no route out: the agent runs, the workspace is there, nothing leaves. Same as `--network`; `--offline` is shorthand for `--network offline`. An unrecognised value refuses the run |
| `BRIG_SKILLS` | `0` | Seed the profile's `hostConfigDir`/`projectPaths` into the workspace -- for Claude Code, `~/.claude/skills` and `~/.claude/plugins`. Same as `--skills`; see [Your own skills in the guest](#your-own-skills-in-the-guest) |

### Credentials

| variable | default | what it does |
| --- | --- | --- |
| `BRIG_FORWARD_ENV` | per profile | Space-separated list of variables to forward. Replaces the profile's list rather than adding to it |
| `BRIG_ALLOW_REFS` | `0` | Forward a value that looks like an unresolved `scheme://` secret reference |
| `BRIG_ALLOW_DENIED` | `0` | Forward a variable on the profile's billing denylist |
| `BRIG_ALLOW_EXPIRED` | `0` | Forward the host credential even though it reports as expired. brig withholds one by default, because a dead token turns into a confusing failure inside the guest rather than a clear one on the host. Set this if your clock is the thing that is wrong. Scoped to the deprecated `hostCredential:` path, and goes with it: an imported credential that has expired warns and is still delivered, so there is nothing to override |
| `GIT_TERMINAL_PROMPT` | `0` | Forwarded as-is; set it to `1` on the host to let git prompt in the guest |

Removed: `BRIG_CREDENTIALS_CMD` ran a command of yours on every boot to read the host credential. brig now refuses to start when it is set, and names the replacement, which reads your command once instead: `brig secret import <profile> <name> --from-command '<command>'`.

### Guest git

| variable | default | what it does |
| --- | --- | --- |
| `BRIG_GIT_CONFIG` | `0` | Write the credential helper and gitconfig into the workspace, routing SSH GitHub remotes over HTTPS |
| `BRIG_GIT_HOSTS` | `github.com` | Space-separated hosts the token applies to |
| `BRIG_GIT_USER` | `github.user`, then gh's record, then `x-access-token` | Username paired with the forwarded token |
| `BRIG_GIT_IDENTITY` | `1` | Forward the host commit identity, resolved from the invoking directory |
| `BRIG_GIT_NAME`, `BRIG_GIT_EMAIL` | host git config | Override that identity |

### Trust and verification

| variable | default | what it does |
| --- | --- | --- |
| `BRIG_TRUST_WORKSPACE` | `1` | Pre-answer the agent's "do you trust the files in this folder?" for the directory each run starts in. The guest sees only the workspace, so mounting it already answered that question |
| `BRIG_VERIFY` | `warn` | `warn`, `require` or `off` -- see the table above |
| `BRIG_VERIFY_REGISTRY` | `ghcr.io/brig-sh/` | Image prefix treated as "ours", so a check is expected |
| `BRIG_VERIFY_IDENTITY` | community-images workflow | Certificate identity regexp cosign must match |
| `BRIG_VERIFY_ISSUER` | GitHub Actions OIDC | Certificate OIDC issuer |
| `BRIG_COSIGN_BIN` | `cosign` | Path to cosign |

### Runtime

| variable | default | what it does |
| --- | --- | --- |
| `BRIG_RUNTIME` | `hull` on macOS, `nerdctl` elsewhere | Which backend to drive |
| `BRIG_RUNTIME_BIN` | first of `hull` / `nerdctl`, `docker` on PATH | Path to that binary |
| `BRIG_HYPERVISOR` | `vz` | Hypervisor backend, macOS only: `vz`, `hvi` or `qemu`. Only `vz` has a graphical console, so a GUI profile is refused on the others |
| `BRIG_GATEWAY_SOCK` | `~/.brig/gateway-<subnet>.sock` | Control socket of the user-mode network gateway. `hvi` has no egress without one, so brig starts a shared gateway there and joins every sandbox to it. The default name carries the network it serves, so a gateway left from a different subnet is never reused for guests that are not on it |
| `BRIG_BOOT_ASSETS` | whatever `hull assets dir` reports on macOS, `$XDG_DATA_HOME/brig/assets` (default `~/.local/share/brig/assets`) on Linux | Directory holding the host kernel and `container-initrd` used to boot a profile marked `genericBoot`. The kernel is named `Image` on arm64 and `bzImage` on x86_64. Set it and brig uses what is there, unchanged; leave it unset and brig downloads the pair on first use (hull on macOS, `oras` on Linux) |
| `BRIG_BOOT_ASSETS_REF` | `ghcr.io/nofireai/hull-assets:<os>-<arch>` | The bundle brig fetches when the boot assets are missing. Override to pin a version or use a mirror |
| `BRIG_ROOTFS_TYPE` | profile's `rootfsType` | How the guest root reaches the VM: `block`, `virtiofs` or `9pfs` |
| `BRIG_ENV_ARGV` | `0` | Put forwarded values back on the runtime's command line, where `ps` can read them. For a runtime build that does not accept a bare `--env KEY`. Opt-in, and it costs you the `ps` guarantee. Inert for a value brig resolved on your behalf -- a secret from its store, or the host credential -- which stays off the command line regardless |
| `DO_NOT_TRACK`, `HULL_TELEMETRY_DISABLED` | | Passed through to the runtime untouched, and always win |


## brigd

`brigd` keeps the session inventory and is the single owner of boot and
teardown when several callers want the same sandbox, over the same library the
CLI uses. Line-delimited JSON on a unix socket:

```json
{"op":"ensure","agent":"claude-code","name":"refactor"}
{"op":"status"}
{"op":"stop","agent":"claude-code","name":"refactor"}
```

It does not proxy exec. Handing your terminal to a process inside the guest
means passing file descriptors, and `brig exec` already does that correctly by
replacing itself with the runtime. The daemon owns lifecycle, the CLI owns the
terminal. See [docs/brigd.md](docs/brigd.md).

## Agents

Profiles are data ([internal/profile](internal/profile/profile.go)): a binary,
the variables carrying its credentials, the ones denied for billing safety,
the state paths, and an image. The eight built-in specs live in
[internal/profile/specs](internal/profile/specs/) and are embedded in the
binary, so brig works with no setup at all. Guest images live in
[brig-sh/community-images](https://github.com/brig-sh/community-images) with
open Dockerfiles, and a profile name is the same string as its image name.

Claude Code and Codex are the proven core. Gemini, Grok and opencode are
example profiles. `claude-desktop` is the GUI app in a windowed VM, and
`ubuntu` is a plain root shell for when you need to inspect guest networking
or raise a raw socket.

## Verifying a brig download

Releases are signed with keyless cosign -- no key to distribute, none for us to
lose:

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

The macOS binaries are also signed with a Developer ID certificate and
notarized with Apple, so Gatekeeper accepts them without any quarantine
fiddling. Each archive ships an SPDX SBOM.

## Not yet ported

- **Workspace clone/overlay and explicit-apply.** The workspace is mounted
  directly, so an agent works on your files rather than on a private copy it
  later applies.

## License

Apache-2.0, see [LICENSE](LICENSE). That covers brig itself. The agent CLIs it
runs, and the guest images they come in, are each under their own terms.

---

<p align="center">
  <a href="https://nofire.ai">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="assets/nofire-logo-on-dark.svg">
      <img alt="NOFire AI" src="assets/nofire-logo.svg" width="150">
    </picture>
  </a>
</p>

<p align="center">Powered by <a href="https://nofire.ai">NOFire AI</a></p>
