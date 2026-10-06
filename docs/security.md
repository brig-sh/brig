# What Brig protects

Brig runs an agent in a microVM. On the host, the agent can reach its guest
home, the credentials you deliver to it, and any project you name on the run
line. No other host directory is mounted.

A run reads no host credential source. See [Credentials](#credentials).

Network reach has weaker limits. See
[Things brig does not claim](#things-brig-does-not-claim). That section is
also the line between a vulnerability and a known limitation.

[Claims and the tests behind them](claims.md) ties promises here to the tests
that defend them. To report a flaw in a boundary privately, see
[SECURITY.md](../SECURITY.md).

## What the agent can reach

The guest can reach:

- its guest home, read-write
- the project you name on the run line, read-write at `/work/<name>`
- the credentials you deliver to it
- the internet, on either `isolated` or `shared` (see
  [Network egress](#network-egress) and [Host services](#host-services))
- a port another sandbox listens on, when both use `shared`, on `hvi` and on
  Linux (see [Sandbox to sandbox](#sandbox-to-sandbox))
- any hostmount volume a profile declares (see [The boundary](#the-boundary))

The guest cannot reach:

- any other host directory
- your keychain
- your SSH agent
- your secret manager
- an environment variable a profile's `deny` list refuses. A `deny` list does
  not cover a `files:` binding (see
  [file delivery](#what-file-delivery-buys-and-what-it-costs)).

The guest gets only the credentials you deliver to it.

The host can reach the guest only on a port you publish. See
[Published ports](#published-ports).

The agent changes your real files in the guest home and in the project, not a
copy of them. It can send anything it reads there over the network. The agent
can use a delivered credential for as long as it holds it. The agent can
misuse that credential the same way a person who holds it can.

### Network posture

A new sandbox on `hvi` or on Linux gets the `isolated` posture.

| Case | Posture |
| --- | --- |
| New sandbox on `hvi` or Linux | `isolated` |
| The graphical `claude-desktop` profile | `shared`, set explicitly in the profile |
| The unpublished `cursor` profile, which leaves its posture unset | `isolated` on Linux and `hvi`. The backend fallback on `vz` or `qemu` |
| Custom profile without a network choice, on `vz` or `qemu` | `shared`. `brig info` names that fallback |
| Existing sandbox | Its recorded posture |
| Existing sandbox from an older session with no record | The actual posture, inspected from the runtime |

Brig refuses a flagless run if it cannot establish the posture of an existing
sandbox.

`isolated` separates sandbox networks. It does not restrict internet access
or establish whether host services are reachable. See
[Sandbox to sandbox](#sandbox-to-sandbox) for what `isolated` guarantees, and
[Network postures](policies.md#network-postures) for overrides and the
backend limits.

## The boundary

The sandbox is a microVM on macOS and on Linux.

| Host | How the microVM boots |
| --- | --- |
| macOS | [hull](https://github.com/brig-sh/hull) boots it over Virtualization.framework. `brew install --cask brig` installs hull |
| Linux | Brig drives `nerdctl` and hands the container to the urunc shim (`io.containerd.urunc.v2`) by default. The guest gets its own kernel |

On a Linux host that has another microVM shim, `BRIG_CONTAINERD_RUNTIME` can
name that shim. Brig refuses a shim that it knows shares the host kernel:
`runc` or `crun`, by name or by path. A container that shares the host kernel
is not the boundary Brig provides.

The `ISOLATION` row of the execution envelope names the boundary that this
run resolved. `brig info` prints the row and boots nothing. `brig --verbose
run` prints it before the boot:

```
ISOLATION    microVM (hull, hvi backend)
ISOLATION    microVM (hull, vz backend)
ISOLATION    microVM (nerdctl over containerd, io.containerd.urunc.v2)
ISOLATION    container (nerdctl over containerd, runc: the guest shares the host kernel)
ISOLATION    unknown (nerdctl over containerd, io.containerd.kata.v2: brig cannot tell whether that shim boots a kernel of its own)
```

The row reports the binary in hand, the backend the run settled on, and the
shim the run will name. Brig can boot a sandbox with a shim that it does not
recognise. Brig cannot establish the isolation of that sandbox from a shim
name alone, so the row says that Brig cannot tell. For a shim that Brig
recognises as sharing the host kernel, `brig info` names it and the run is
refused.

Inside Brig, the guest has your guest home mounted as its home, read-write.
Name a project on the run line and that project is a second host directory,
also mounted read-write, at `/work/<name>`.

Each hostmount volume in the profile is one more share. Every hostmount
volume in a shipped profile is inside the guest home, so no shipped hostmount
exposes anything more today. That is a property of the shipped profiles. It
is not a guarantee about a profile that you write. A hostmount that your own
profile declares outside a tmpfs cover is a host path the guest can see.

Beyond those, the guest does not have your keychain, your SSH agent, your
secret manager, or any other directory on the host. The guest cannot fetch a
credential for itself, so you must deliver each credential explicitly. See
[Credentials](#credentials).

### Published ports

Nothing on the host can open a connection into the sandbox until you publish
a port. To publish a port, use `--publish` on a run, or `brig network publish`
on a sandbox that is up. `brig network unpublish` closes the port. A
published port is the only inbound opening in the boundary.

**The port binds to loopback by default.** A published port binds to
`127.0.0.1` unless you write an address. The port is then reachable from this
machine, and not from the network this machine is on. To ask for the wider
binding, write the address, as in `0.0.0.0:8080:80`. The wider binding is
never the default.

**The execution envelope names the port.** The `PORTS` row lists every port
the sandbox publishes. `brig info` prints the row, and `brig --verbose run`
prints it before the boot:

```
NETWORK      isolated (a network of this sandbox's own)
PORTS        127.0.0.1:8080 -> 80
             0.0.0.0:443 -> 443 (reachable from the network this host is on)
```

A publication outlives the run that made it, so the row is not limited to the
ports this command line asked for. A row with an address other than loopback
says so.

**No egress rule applies to the port.** A published port is ingress. A policy
decides where the guest can open a connection *to*. It does not apply to a
connection opened *into* the guest. A sandbox under a strict egress policy is
still reachable on a port it publishes.

## Credentials

You choose how the agent gets a login. Log in inside the sandbox, or import
your host login once:

```bash
brig run claude-code               # log in inside the sandbox, or:
brig secret import claude-code     # carry the host login in, once
```

`brig secret import <profile>` puts your host login into Brig's own store.
Every run afterwards reads only [that store](#the-secret-store).

**A run reads no host credential source.** Nothing on the
`brig run`, `sh` or `exec` path reaches a keychain item Brig did not write.
Nothing on that path reaches a credential file outside the guest home, or a
host command that produces one.

Two host reads do happen on a run, and no setting turns either off:

1. Brig runs `git config --get` in the directory you invoked it from, for
   `user.name`, `user.email` and `github.user`. The answers give the commit
   identity that Brig forwards into the guest.
2. If `github.user` is empty, Brig reads the `user:` line from the stanza for
   your git host in gh's `hosts.yml`, under `$GH_CONFIG_DIR` or
   `~/.config/gh`. That line names the login that pairs with the forwarded
   token. That file usually holds gh's own OAuth token too. Brig takes the
   login and nothing else.

`BRIG_GIT_IDENTITY=0`, `BRIG_GIT_CONFIG=0` and `BRIG_GIT_USER` change what
Brig does with the answers. They do not stop the reads.

A credential reaches the guest by one of two channels. The profile picks the
channel for each secret.

| Channel | What it does |
| --- | --- |
| `files:` | Writes the credential into the guest at the path the agent already reads |
| `env:` | Binds the credential as an environment variable, for a credential whose consumer offers no file interface |

For an `env.<name>` binding, or the deprecated `forward:` spelling of one,
Brig still reads the named variable from its own environment. Whatever
populates that environment remains a usable backend for those bindings.

Brig reads values again on every exec, so the sandbox picks up a rotated
credential without a restart. Nothing is written into the guest home from the
host for this.

Brig applies these rules when it resolves a secret:

- Brig skips a variable that is unset or empty, so it cannot shadow a value
  baked into the image.
- A `scheme://` value read from the environment is refused as an unresolved
  secret-manager reference. Tools such as direnv leave those references in the
  environment. Forwarded verbatim, a reference yields "Invalid username or
  token" in the guest, which looks like a broken sandbox. A `value:` literal
  or a value from Brig's own secret store skips this check. That value is not
  a reference a tool failed to resolve. `BRIG_ALLOW_REFS=1` forwards an
  ambient reference anyway.
- A variable on the profile's `deny` list is refused, with the reason.

`brig info <agent>` reports the guest's environment by name. It fails the
same way a run does if a required secret cannot be resolved. It never prints
a value. A variable that comes from a secret is annotated, for example
`GH_TOKEN(secret)`. A credential delivered as a file is not an environment
variable, so it does not appear in that list.

On a Linux host with no keyring there is no
[secret store](#the-secret-store):

| Profile secrets | Result |
| --- | --- |
| *Optional*, as in `claude-code` | The run boots and the agent asks for a login |
| **Required** | The run fails, because that host has no place to read the secret from |

### What file delivery buys, and what it costs

A credential delivered as a file:

- stays out of `/proc/<pid>/environ`
- is not inherited by processes the agent spawns
- can be rewritten under a running agent, so a rotated secret can reach a
  live session. No environment variable can do that.
- lands on a memory-backed mount and does not reach your disk (see
  [What reaches host disk](#what-reaches-host-disk))

Weigh these costs before you rely on file delivery.

- **Brig stores and hands over a refresh token.** Claude Code cannot use a
  `.credentials.json` without `refreshToken` and `refreshTokenExpiresAt`. A
  file with only an access token is *worse* than an environment variable,
  because the agent attempts a refresh, fails, and prompts. So a compromised
  agent inside the sandbox can mint access tokens indefinitely. It continues
  after the host's own token expires. In exchange, the guest refreshes for
  itself, so a long session does not break every few hours.
- **Brig's copy is less protected than the item it came from.** The host's
  Claude item is ACL-scoped to the application that wrote it, so it raises a
  dialog the first time something else reads it. Brig's copy has the default
  ACL, like every secret in [the secret store](#the-secret-store). The only
  real mitigation is to keep the stored copy low-value, and a refresh token is
  not low-value. When you do not use the copy, delete it:
  `brig secret delete claude-credentials`.
- **The `deny` list covers only environment variables.** A `files:` binding
  bypasses the deny check, and no name check can fix that. A profile can
  deliver a metered API key inside a `settings.json`, and nothing sees it.
  `deny` exists to catch an **accident**: an ambient variable from your shell
  that is swept into the guest. A file binding takes an explicit stored secret
  and an explicit binding that the profile author wrote. The two names on the
  `deny` list of `claude-code` are environment variables by the agent's own
  design. So the check still covers the channel the risk uses.
- **The stored copy does not rotate.** A credential renewed on the host does
  not update Brig's copy. A credential *revoked* on the host stays valid in
  Brig's store until you import it again or delete it. Brig warns before boot
  when the stored copy is expired, and names the command that refreshes it.
  Brig cannot see a revocation.

A `0600` file is still readable by anything that runs as the agent's uid
inside the sandbox. File delivery narrows the exposure. It does not make a
boundary. See [What is still exposed](#what-is-still-exposed).

### What reaches host disk

The credential file does not reach host disk while the agent runs with
`CLAUDE_CONFIG_DIR`. It lands on a `tmpfs` mount at `/brig/claude`, which is
not on the home share, and `CLAUDE_CONFIG_DIR` points the agent there. Brig
checks that the mount is `tmpfs` with no swap before it writes anything. So
`.credentials.json`, and the temp file the agent renames onto it, never touch
your disk. `brig stop` removes that mount with the sandbox, so an in-sandbox
login on this profile does not outlive a stop. The limits of this are under
[What is still exposed](#what-is-still-exposed).

Eight paths are hostmounted from `~/.claude` in the guest home into
`/brig/claude`. They are on host disk, and they persist across boots:

| Paths | Contents |
| --- | --- |
| `settings.json`, `CLAUDE.md` | Your permission allowlist and your user-level memory, written by hand or by the agent on your instruction |
| `sessions`, `projects`, `history.jsonl` | The conversation |
| `plugins`, `skills` | Plugins and skills. `--skills` copies your own here, so that flag does nothing if a profile leaves either off |
| `.claude.json` | The agent's global state: onboarding, trust and per-project settings |

Anything else under `/brig/claude` is ephemeral. That includes anything a future
Claude Code version starts to write there. The source of this list is the
`volumes:` block of the `claude-code` profile.

### Not in argv

Forwarded values go into the runtime process's own environment, and only the
variable *name* appears on its command line. So other processes on the host
cannot read a forwarded credential in `ps`.

`HOME`, `PATH`, `TMPDIR` and any `XDG_` variable are the exception. Brig
passes these on the command line as `NAME=value`, on every run. The runtime
reads these variables for itself, and a guest value there redirects the
runtime. hull keeps its store under `HOME` and finds `hvi` on `PATH`. A
rootless nerdctl reads its registry config under `HOME`. None of these
variables carries a credential, and Brig refuses a stored secret bound to one
of these names. The `BRIG_ENV_ARGV` warning does not list them.

`BRIG_ENV_ARGV=1` puts forwarded values back on the command line, for a
runtime build that does not accept a bare `--env KEY`. A value read from the
environment then loses this guarantee. A value Brig resolved from its own
secret store stays off the command line with or without `BRIG_ENV_ARGV`. The
host durably logs the argv of every exec, so a value on the command line
stays in that log.

### What is still exposed

Give the agent a narrow credential. Prefer a fine-grained `GH_TOKEN` scoped
to the repositories you want reachable, over a classic PAT that carries your
whole account.

Anything that runs alongside the agent inside the sandbox can read the
credential. The sandbox cannot use a credential it cannot see. Brig forwards
the credential and limits the blast radius. It does not use a sentinel value
or a host-side proxy (see
[A TLS-terminating host proxy](non-goals.md#a-tls-terminating-host-proxy)).

The `claude-code` credential stays off host disk only while the agent has
`CLAUDE_CONFIG_DIR`. Brig hands it to every process it starts in the sandbox.
A `claude` started without it, under `sudo -i`, `su -` or `env -i` for
example, writes `.credentials.json` to `~/.claude` in the guest home. That
directory is on the home share, so the file is on host disk.

On hull with the `hvi` backend, the guest root is itself a share of the
sandbox's directory in hull's store. If something on the host renames or
replaces the directory under `/brig/claude` there while the sandbox runs, the
guest drops the `tmpfs`. A credential the agent writes before Brig's next
command then lands in hull's store, and stays there until `brig rm`. Brig's
next command mounts the `tmpfs` again.

## The secret store

`brig secret` is the **only** store a run reads, and the one place where Brig
stores a value. A profile names the secrets it wants under `secrets:`. `brig
secret import` puts a host login in.

Prefer the store to a value you compose into Brig's environment. A value from
the store stays off the command line the host logs, whatever anyone sets
later. An ambient value does not (see [Not in argv](#not-in-argv)).

[Keeping secrets in your keyring](secrets.md) covers how to use the store.
[Secrets and env](profiles.md#secrets-and-env-for-a-credential-brig-resolves-itself)
covers the profile side.

On macOS the backend is the login keychain. Every item is a generic password
under the service `sh.brig.secret`, with the secret's name as the account:

```bash
printf %s "$TOKEN" | brig secret create gh-token
brig secret create deploy-key -f ~/.ssh/id_ed25519
brig secret ls
```

**The value never appears in argv.** A secret is two keychain items, written
by two `security` invocations. The whole `add-generic-password` command for
the key item, with the base64 key, goes to `security -i` through a pipe. So
that command line is `security -i` and nothing else. The sealed item is
written through the arguments of `security`. Those arguments hold the
ciphertext and the secret's name.

<details>
<summary>Check the key item's argv</summary>

`security -i` reads one command per line and then blocks for the next line.
So the write stays on the process table for as long as the pipe stays open.

1. Run `security -i` against a fifo.
2. Hold the fifo open with an idle writer.
3. Send the real `add-generic-password` line down the fifo.
4. Read `ps -Ao args`. The argv shows `security -i` and nothing more.
5. Run `security find-generic-password`. It shows that the value was stored.
6. Close the fifo and delete the probe item.

</details>

**Each item has the default ACL.** `security` created these items, so
`security` can read them back with no keychain dialog. **Anything that can
run `/usr/bin/security` as you can read them back too.** That is the same
boundary as your own shell, and it is weaker than a per-application ACL. Brig
does not ask for the broad `-A`, and it does not narrow the default.

**The keychain holds the key in one item and the sealed value in another.**
[Where a value lives](secrets.md#where-a-value-lives) has the layout and the
reason for it. The layout changes nothing in the threat model. A process that
can read Brig's items, which is any process that runs as you, reads the key
and opens the sealed item. A copy of the keychain file without the login
password holds two encrypted items it cannot open. Keychain Access shows a
base64 key and base64 ciphertext, not the secret.

**`brig secret ls` never decrypts.** It reads attributes only. So a listing
raises no access prompt, and it shows names and dates but never values. The
read is wider than Brig's own items:

- `security dump-keychain` takes no service filter. Its options are
  `[-adhir] [keychain...]` and nothing else.
- Brig names no keychain, so the dump covers the whole keychain *search
  list*. On a stock Mac that is your login keychain **and the System
  keychain**. `security list-keychains` shows yours.
- `ls` enumerates the attributes of every item in those keychains and
  discards the items that are not Brig's.
- Nothing is decrypted and nothing leaves the process. Names and dates that
  belong to other applications, and to the system, do pass through Brig
  before Brig discards them.

**Brig writes only under its own service.** Every command that creates,
changes or removes an item carries `-s sh.brig.secret`, so Brig cannot reach
outside that namespace. The service name is a label. It is not an
authenticity check. Another process that runs as you can add an item under
that service, and Brig then reads, updates and deletes that item as its own.
Two checks limit this. `read` reports a value that is not in Brig's encoding,
and `ls` skips a name outside Brig's grammar. So an item Brig did not write is
reported or skipped, and is not presented as yours.

**`brig secret import` reads another application's item and never writes
it.** Claude Code's own item is one example. That read raises a keychain
dialog, because `security` did not create the item. A run never performs that
read. The dialog appears once, when you run the import, and never on the boot
path.

On Linux the store is a Secret Service keyring on your session bus,
gnome-keyring or KWallet (see [Linux](secrets.md#linux)). A host with no
keyring has no store. `brig secret` says which half is missing. It does not
fall back to a file.

## Writing into the workspace

Brig refuses a symlink in the guest home that it is about to write through.
The run fails before anything is written, and the error names the link and
its target. Remove the link, or point the guest home somewhere else.

The guest home is mounted read-write, so the sandbox chooses its contents.
Brig also writes into it from the host on every invocation. Those writes are
the stale-share marker, the onboarding seed, the trust key, the guest git
files, and the skills that `--skills` copies in. So the guest home is the one
place where the sandbox can influence what happens on the host.

Brig runs as you and outside the sandbox. A guest can plant a symlink where
Brig writes next. The link aims Brig at a host path the guest itself cannot
reach, such as your `~/.ssh` or your shell profile. The microVM boundary does
not stop that write, because the write is on the host side of the boundary.

So every host-side read and write Brig makes inside the guest home goes
through an `os.Root` opened on it. `os.Root` resolves each path itself. It
refuses an absolute symlink, and it does not let a relative symlink climb
past the root. There is no window between the check and the open in which the
guest can swap the file.

| Symlink | On a write | On a read |
| --- | --- | --- |
| Escapes the guest home | Refused | Refused |
| Stays *inside* the guest home | Refused. Brig writes only regular files where a state file belongs | Followed. A `.gitconfig` symlinked to your own dotfiles, inside the guest home, keeps working |
| At the guest home, or on the way to it | Refused before anything is created (see [The path to the guest home](#the-path-to-the-guest-home)) | Refused |

In each case, Brig decides from what it opened, not from an earlier check. A
fifo, socket, device or directory where a regular file belongs is refused.
That closes the window in which the guest renames one file type over another.

A refusal looks like this:

```console
$ brig run claude
brig: refusing to write /Users/alex/.brig/homes/brig-claude-code/.claude/.claude.json:
it is a symlink to "/Users/alex/.ssh/authorized_keys", and brig writes only
regular files inside the workspace. The workspace is mounted read-write as the
sandbox's home, so that link was put there from inside the sandbox, to have
brig -- which runs as you, on the host -- reach a file the sandbox cannot.
Nothing was written; inspect
/Users/alex/.brig/homes/brig-claude-code/.claude/.claude.json and remove it before
running brig again: a symlink leads out of a directory brig is checking
```

Brig does not create the links it writes through. A link in the way is one
that you put there, or one that the sandbox planted. Do not retry the run
with the link in place.

`--home` pointed at a symlink is refused for the same reason, with the same
kind of message, and is fixed by naming the real directory.

### The path to the guest home

A root cannot see a symlink *at* the guest home or on the way to it. Every
path below that link still resolves inside the root. So Brig checks the path
separately, before it creates anything.

The guest writes as you, so it can swap an entry only inside a directory you
can write. Brig splits the path to the guest home at the first directory you
can write.

| Part of the path | How Brig opens it |
| --- | --- |
| Above the split | By name. The guest cannot reach these entries. Brig follows the links the system or an administrator put there (`/tmp` and `/var` on macOS are links) |
| From the split down | One component at a time, against the directory Brig already holds. Brig refuses every symlink, and checks after each step that it opened what it looked at |

The guest home is always in the lower part, so a link there is refused
wherever it is.

To decide whether you can write a directory, Brig asks the kernel through
`access(2)`. It also counts ownership, because you can always `chmod` a
directory you own. So a group membership past the first sixteen and an ACL
both count, on both platforms.

A link above the split whose target passes through a directory you can write
is refused, like any other link the guest can reach. A root-owned `/data`
that points into your home is one example. Name the real directory instead.

<details>
<summary>When Brig runs as root</summary>

Ownership tells Brig nothing, because the guest's writes are root's too. A
directory that a root sandbox had read-write looks like one of the machine's
own. Brig then trusts only the entries of `/`, which it never mounts. The
system's own links there, such as `/tmp` on macOS or `/home` on an ostree
system, still resolve. Brig walks every component below them link by link.
So a link on the way to the guest home or the project is refused.

</details>

### Deleting a guest home

`brig rm` deletes a guest home Brig created. It never deletes a guest home
you named with `--home` or `BRIG_WORKSPACE`, wherever that home is.

The tree that `brig rm` deletes is the guest's, so the delete has limits:

- The delete takes only a direct child of `~/.brig/homes`.
- The delete goes through an `os.Root` opened there, so nothing it does can
  resolve outside that directory.
- Inside that directory, the delete removes a symlink as a link and never
  descends into it. So the target of a symlink the guest left in its home
  stays untouched. `os.Root` alone does not give that, because it follows a
  link whose target stays inside the root.

A first run whose boot fails deletes the home it created the same way.

## Mounting a project

Name a project on the run line and Brig mounts it read-write at
`/work/<name>`. The project is the second host directory the sandbox can
change, and the sandbox can replace every component at or below it.

Brig refuses a project path that goes through a symlink. The run fails before
anything is mounted, and the error names the link and its target. Name the
real directory instead:

```console
$ brig run claude ~/lab/monorepo/frontend
brig: refusing to use /Users/alex/lab/monorepo/frontend as this run's
project: it is a symlink to "/Users/alex/escape-target", and the project is
mounted read-write into the sandbox, so brig will not hand a guest a
directory reached through a link. Name the real directory instead: a symlink
leads out of a directory brig is checking
```

For a link further up the path, the error names the component that is a link,
not the directory you typed:

```console
$ brig run claude ~/lab/monorepo/frontend/src
brig: refusing to use /Users/alex/lab/monorepo/frontend/src as this run's
project: /Users/alex/lab/monorepo/frontend on the way to it is a symlink to
"/Users/alex/escape-target", so the sandbox would be handed a directory other
than the one you named. Name the real directory instead: a symlink leads out
of a directory brig is checking
```

A link you made yourself is refused too. Brig cannot tell it from a planted
link: same owner, same directory, same bytes. So the rule is about links and
not about who made them.

The `PROJECT` row in the run envelope reports the directory the path resolved
to, not the path as typed. On macOS a project under `/tmp` prints as
`/private/tmp`. That is the directory the sandbox gets.

### Why a link is refused

A share is a path, and the runtime resolves it. Brig runs as you and outside
the sandbox, so a link planted in the project is followed on the host side of
the boundary. The microVM does not stop this. The VMM is asked to export a
directory, and it exports the directory the link points at.

The risk needs a second sandbox with write access to the path. A run's own
sandbox gains nothing from a swap, because it already has the project
read-write. Another sandbox does: the sandbox of an earlier run, or one from
another session that still runs with an overlapping project. The sequence is:

1. An agent replaces a subdirectory with a link to another place on the host.
2. The operator later narrows a run to that subdirectory. That is the
   ordinary way to point an agent at one part of a repository.
3. The path is the operator's own, and the directory it reaches is the
   agent's choice.

So Brig reaches the project the same way it reaches the guest home (see
[The path to the guest home](#the-path-to-the-guest-home)). The boot descends
again and refuses a path that no longer names the directory it holds.

The guest home refuses links for a different reason: Brig creates it, so a
link there has no legitimate author. One walk produces both refusals, and
only the wording differs.

A hostmount volume source is reached the same way, and gets the same second
check. Its path is inside the guest home, so the guest owns every component.
Brig checks that the source is symlink-safe when the run begins. A restart
reopens the boot after the guest held the guest home. So Brig checks the
source again through the held handle before it builds the share. A source
swapped for a link is refused and is not exported.

### The handover gap

> [!WARNING]
> The path walk does not close the handover. The share is a string that the
> VMM resolves later, on its own time.

A sandbox from another session that still runs with an overlapping project
can swap a component in that window. One example is `~/monorepo` while this
run names `~/monorepo/frontend`. The guest home has the same gap, and so does
a hostmount volume source. To close the gap, the runtime must accept a
directory handle and not a path.

## Guest images

Brig checks where an image came from before it boots the image, because an
image is code that runs with your credentials. The check is cosign's keyless
verification. It asks more than whether the image was signed: it asks whether
that workflow, in that repo, built it. You can run the same check:

```bash
cosign verify \
  --certificate-identity-regexp \
    '^https://github\.com/brig-sh/community-images/\.github/workflows/build-images\.yml@refs/heads/main$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/brig-sh/claude-code-stock:root
```

The command works anonymously. The images are public, and the signature is in
Sigstore's transparency log, not behind a registry login. `:root` is a
multi-arch index. The per-architecture tags (`:arm64`, `:amd64`) and the
immutable `:<arch>-<sha>` pins are signed the same way and verify with the
same command.

The identity is anchored on the repository and the workflow file. A
signature from anywhere else fails, including one from another workflow in
the same repository.

### Verification modes

`BRIG_VERIFY` sets the mode.

| Mode | Behaviour |
| --- | --- |
| `warn` (the default) | Reports what it found, then boots. The one exception is the last row of the next table |
| `require` | Refuses anything it cannot positively verify, third-party images included |
| `off` | Skips the check |

A typo in `BRIG_VERIFY` refuses the run and names the three values. Brig does
not read a typo as one of them.

What `warn` does with the answer:

| situation | behaviour |
| --- | --- |
| image under `ghcr.io/brig-sh/`, signature verifies | one line saying so, boots |
| image published by somebody else | warning, boots |
| cosign not installed | warning, boots |
| image under `ghcr.io/brig-sh/`, signature fails | stops and asks `[y/N]`. Refuses when there is no terminal |

Bring-your-own images are a supported way to use Brig, so `warn` boots them.
"Unable to check" is not the same as "failed". The one case with no innocent
reading is an image sitting under our registry whose signature does not
verify. That is the case that stops.

`claude-desktop` points at a `ghcr.io/nofireai/` image, and `ubuntu` at
`docker.io/library/ubuntu`. Brig has no signing policy for either registry, so
both warn on every boot until those images move.

If you publish signed images yourself, point `BRIG_VERIFY_REGISTRY`,
`BRIG_VERIFY_IDENTITY` and `BRIG_VERIFY_ISSUER` at your own registry and
workflow.

### Digest pinning

Brig boots the bytes that cosign checked. A tag is a name, and a name can
resolve to different bytes in the registry and in your local store. So before
the check, Brig resolves the reference to the digest the registry serves.
Brig verifies that digest and boots it. The object cosign checked is the
object that runs, and the success line names the digest instead of the tag it
came from.

| Case | Under `warn` | Under `require` |
| --- | --- | --- |
| The local store holds a different digest under the tag | Stops and asks, like the signature-failure row. A yes boots the verified digest, not the copy on disk | Refuses |
| The registry cannot be reached | Stops in the same way, because the image was not checked | Refuses |

Every cosign call is bounded, so an outage fails in seconds and does not hang
the boot.

Pinning is for images under `ghcr.io/brig-sh/`. An image Brig did not publish
carries no signature of ours, so Brig makes no cosign call for it. That image
boots by tag, with one line that says whose image it is.

Pinning works where the runtime's store answers a digest: containerd on Linux,
and a current hull on macOS (see
[Every command Brig runs](runtimes.md#every-command-brig-runs)). Brig asks the
hull it drives, because the hull on `PATH` can be older than Brig.

An older hull cannot find a digest reference in its store:

- A pinned boot on that hull pulls again on every run. Under
  `BRIG_PULL=never` it fails with the bytes on disk.
- So Brig verifies and boots the tag on that hull, and prints one line that
  says so. The gap remains until you upgrade hull.
- Under the default `missing` pull policy, cosign checks the tag in the
  registry. That is not necessarily the copy hull already holds.
  `BRIG_PULL=always` is the workaround.
- Brig does not name a digest on a boot that did not pin one.

Two things are narrower on macOS than on Linux:

- An image pulled under an older hull has no index digest on record, and a
  multi-arch tag resolves to its index digest. After a hull upgrade, the first
  pinned boot of that image misses the cache and pulls once. Under
  `BRIG_PULL=never` it fails until the image is pulled again.
- hull does not yet expose the digest its store holds for a reference. So the
  report that the local copy differs from what the registry serves is
  Linux-only for now. The boot is pinned on both platforms.

### The kernel, not only the image

Six of the eight shipped profiles boot a kernel and an initrd that Brig
downloads. They are `claude-code`, `codex`, `gemini`, `grok`, `opencode` and
`ubuntu`. Brig checks that bundle too, under the same
[`BRIG_VERIFY` mode](#verification-modes), as a second check with its own
trust root.

`cursor` and `claude-desktop` boot the kernel in their own image, so nothing
in this section applies to them. `cursor` has no published image today.
`brig agent ls` marks it `(no published image)`, and a run of it fails before
any of these checks.

| Trust root of the bundle | Value |
| --- | --- |
| Registry prefix | `ghcr.io/nofireai/` |
| Signing identity | The `build-assets.yml` workflow in `NOFireAI/hull-assets` |
| Issuer | The same issuer as the image check |

The kernel's identity is fixed. `BRIG_VERIFY_REGISTRY`, `BRIG_VERIFY_IDENTITY`
and `BRIG_VERIFY_ISSUER` repoint the image's trust policy only.

A signature that fails stops the boot, in every mode except `off`. There is
no `[y/N]` prompt as there is for the image.

The signature covers the bundle's manifest, and the manifest lists a sha256
for each file. Brig does these steps before the boot:

1. Brig keeps the digest whose signature verified.
2. Brig reads the manifest from the registry by that digest, and checks that
   its bytes hash to it.
3. Brig hashes the kernel and initrd it hands the runtime, and compares them
   with that list.

`brig: image and boot assets verified` appears only after both files match.

Brig refuses a registry that answers with any of these:

- bytes that are not that digest
- an index
- a redirect loop
- a token realm or redirect over plain http

On macOS, when Brig cannot read the manifest, it reads hull's
`provenance.json` in the asset directory instead. It accepts only a record
that names the verified digest. hull writes that record into the directory it
describes, so anything that can rewrite the kernel there can rewrite the
record to match. Files that differ from the record still count as a
difference. Files that match it are "cannot check", never verified. A registry
answer Brig refused gets no fallback.

What a difference does depends on who chose the directory.

**`BRIG_BOOT_ASSETS` unset.** Brig chose the directory and fetches into it,
by the digest whose signature verified.

- Files that match a `provenance.json` for another bundle are that bundle,
  fetched before the tag moved. Brig fetches the verified digest over them and
  compares again.
- Any other file that differs refuses the run under `warn` and `require`, and
  nothing is fetched over it. The refusal names the file, its digest and the
  digest the bundle lists. To fetch the bundle again, delete the two files.
- hull's store is such a directory wherever `HULL_BOOT_ASSETS` puts it,
  because hull fetches into it.

**`BRIG_BOOT_ASSETS` set.** The directory is someone's build.

- `warn` states the difference and boots it, and nothing vouches for that
  kernel.
- `require` refuses.
- A [Linux runtime bundle](#the-linux-runtime-bundle) from before its signed
  record is this case.
  Under `require` its directory refuses until `install.sh` installs a newer
  bundle.
- hull checks a directory against its own `provenance.json` too. So a named
  copy of hull's directory with a changed file fails at hull even under
  `warn`.

**Digests Brig cannot check.** `warn` states it and boots. `require` refuses.
The cases are:

- no manifest and no record of the verified bundle
- a registry answer Brig refused
- a record with no entry for one of the files
- files that match only hull's record

`BRIG_VERIFY=off` skips the signature and the digest checks, and one line
says so. A `BRIG_BOOT_ASSETS_REF` under `ghcr.io/nofireai/` is checked the
same way against its own digest. Any other reference has no signature of ours,
so there is no digest to bind.

#### The Linux runtime bundle

The Linux runtime bundle's kernel and initrd are its own, and the boot
bundle's manifest lists neither. The bundle's release lists them:

- The release publishes `share/guest/SHA256SUMS` as an asset.
- The signed `checksums.txt` of the release covers that asset.
- The bundle's installer keeps `checksums.txt`, `checksums.txt.sig` and
  `checksums.txt.pem` beside the files.

On Linux, when the directory `BRIG_BOOT_ASSETS` names holds a `SHA256SUMS`,
Brig checks that record instead of the boot bundle's signature, in this
order:

1. The kernel and initrd must hash to what `SHA256SUMS` lists.
2. `checksums.txt` must list the `SHA256SUMS` in the directory, byte for
   byte.
3. cosign checks the signature on `checksums.txt` against the release
   workflow of `NOFireAI/brig-standalone-linux`, on a tag, with the same
   issuer as the image check. `BRIG_VERIFY_RUNTIME_IDENTITY` and
   `BRIG_VERIFY_RUNTIME_ISSUER` point it at a fork's release workflow
   instead.

| Result | `warn` | `require` |
| --- | --- | --- |
| A file the record does not list, a record the release does not list, or a signature that does not verify | Refuses | Refuses |
| Files match a record Brig cannot check (no cosign, no signature files beside it, or no answer from Sigstore) | States it and boots | Refuses |

The bundle chose those files, which is why the first row refuses under both
modes. To put the files back, reinstall the bundle with `install.sh`.

The first two checks need no network, so a changed file refuses even where
the signature cannot be checked. cosign checks the signature online, as it
does for the image. That takes about as long as the boot bundle's check it
replaces.

Only a run through nerdctl reads the record. On hull a `SHA256SUMS` in a
named directory is ignored.

#### After the check

The comparison happens before the runtime starts. The runtime opens the files
later by path, after the workspace and the image pull. A first pull can take
minutes. Something that can write to the asset directory in that time can
still swap a file:

- the host's own user
- a sandbox, only when it mounts that directory. A user install keeps its
  asset directory under `$HOME`, and `brig run claude ~` shares `$HOME`
  read-write.

On macOS hull narrows the window. It stages a copy of each file and checks
the copy against its `provenance.json`, so a swap has to rewrite the record
too. On Linux nothing checks the files again after Brig does.

## Brig's own binaries

You can verify a release yourself. Releases are signed with keyless cosign,
so there is no key to distribute and none for us to lose. The certificate is
short-lived, bound to the release workflow's OIDC identity, and recorded in a
public transparency log.

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

The first command vouches for the checksum file, and the second ties every
archive to it. Each archive also ships an SPDX SBOM.

The macOS binaries are also signed with a Developer ID certificate and
notarized with Apple. cosign proves provenance and Gatekeeper checks
something else, so neither replaces the other. Nothing strips the quarantine
attribute. It is still on the files Homebrew installed, and Gatekeeper
accepts them because they are notarized.

```bash
spctl -a -vv -t install "$(which brig)"  # source=Notarized Developer ID
xattr -l "$(which brig)"                 # com.apple.quarantine, still there
```

A from-source build is neither signed nor notarized. You trust it because you
built it.

## Telemetry

`brig telemetry off` (or `DO_NOT_TRACK=1`) turns telemetry off everywhere it
runs. Brig sends anonymous usage events and crash reports to NOFire AI, on
macOS and Linux. On macOS, hull also reports the boot, lifetime and resource
use of the sandboxes Brig runs. [Telemetry](telemetry.md) shows how to check
the current state, and lists what an event carries field by field.

Telemetry must not cross the boundary that Brig protects. Brig's own events
carry the fields [Telemetry](telemetry.md) lists, and the events hull sends
follow
[hull's stated commitment](https://github.com/brig-sh/hull/blob/main/docs/telemetry.md),
which this repository does not verify. Together they:

- exclude host paths, repository names, command arguments and agent prompts
- exclude secret names and values, image references, network destinations
  and file metadata
- send the name of an agent profile of your own only as a salted hash, which
  anyone who guesses the name can compute too
- drop IP addresses at ingestion and keep raw events for a year

To check that yourself:

- `HULL_TELEMETRY_DEBUG=1` prints payloads to stderr and does not send them,
  for Brig's events and hull's alike. You can then read what an event
  carries.
- The install identifier is a random value stored in
  `~/.hull/telemetry.json`. Delete the file to rotate the identifier.
- Crash reports queue in `~/.hull/crashes/`, and events not yet sent in
  `~/.hull/outbox/`. You can read or delete them there before they are
  uploaded.

If you find an excluded item in a payload, that is a bug. Report it as
[SECURITY.md](../SECURITY.md) describes.

## Things brig does not claim

### Network egress

To restrict egress, attach a policy on `hvi` or on Linux with nerdctl.
`--network offline` is the one posture with no route out at all, on every
backend.

Brig does not sandbox the agent from the network by default, on any backend.
A sandbox nobody attached a policy to has unrestricted egress. That is what
`brig run <agent>` gets on a fresh install.

| Backend | Where a policy is enforced |
| --- | --- |
| `hvi` | At the network gateway Brig gives that sandbox |
| Linux with nerdctl | In nftables on the sandbox's own bridge, with a resolver of Brig's that answers the guest's DNS |
| `vz`, `qemu`, docker | No policy is available. Outbound traffic is whatever the runtime allows |

Both enforcement points are outside the guest's kernel.

Every runtime Brig ships refuses to boot a policy it cannot enforce. It does
not boot that sandbox unconstrained (see
[Networking and egress policy](policies.md)). That refusal is each adapter's
own choice. It is not a guarantee Brig imposes on every runtime it will ever
drive. The one exception is `--network offline`: a sandbox with a policy and
no route out satisfies every rule set, so no backend refuses it.

On nerdctl the nftables table is the sandbox's only filter, and it outlives
Brig's resolver:

- If the resolver dies, the table refuses every new connection.
- If something removes the table, the sandbox is unfiltered until the
  resolver installs the table again, within 5 seconds. If the resolver is
  gone too, the sandbox is unfiltered until the next boot.
- A table installed again has lost the addresses the guest looked up. It
  refuses them until the guest asks again.

### Host services

The one control is an egress policy on `hvi` or on nerdctl, with
`default: deny` and no `cidr` allow for the host's own ranges.

Brig does not say whether the guest can reach services bound on the host
itself. That covers a dev server, a local model, an MCP server, a metadata
endpoint. Brig adds nothing to narrow that. What the guest's network reaches
is the runtime's default. Whether that includes the host is unmeasured on
every backend.

On nerdctl a policy of either default also refuses the addresses of the
network namespace that holds the sandbox's bridge. DNS and ping to the bridge
address are the exception.

| nerdctl | Whose namespace | Effect |
| --- | --- | --- |
| Rootful | The host's | The policy covers every address of the host |
| Rootless | rootlesskit's | The rules judge the host's own addresses like any other |

There is no equivalent on `vz`, `qemu` or docker.

### Sandbox to sandbox

If two agents must not reach each other, run both with `--network isolated`
on `hvi` or on Linux. On `vz` and on `qemu`, where that posture is refused,
run them on separate hosts.

`isolated` is the guarantee. It is the default for new `hvi` and Linux
sandboxes unless another posture is named (see
[Network posture](#network-posture)). `--network isolated` also moves an
existing shared sandbox onto it.

`isolated` gives the sandbox its own network. On Linux that is its own CNI
network. On `hvi` it is its own gateway process on its own `/30`. No other
sandbox is on that network, whatever the backend does with a shared one. A
sandbox that carries an egress policy is isolated whether or not it asked to
be. The rules live on that gateway and cover everything behind it.

`isolated` is **refused** on `vz` and on `qemu`, where Brig owns no network to
give. A run that asks for it there is stopped and told which backend
implements it. Brig does not boot that run onto the shared network under a
row that claims otherwise.

The guarantee is about reachability, not resources. An isolated sandbox is
one process and one network more than a shared one. `brig rm --all` prunes
them.

Brig does not promise that one sandbox cannot reach another under a `shared`
network. A sandbox gets `shared` in these cases:

- A flag, a setting or a profile chooses it.
- An older sandbox keeps it, if it was recorded or recovered from the
  runtime.
- `vz` and `qemu` use it when no posture is named.

Brig asks the runtime for its shared network. On `hvi`, Brig also hands out
the addresses on it. Whether that network forwards traffic from one guest to
another is the runtime's behaviour, not Brig's. The measurements are in
[Manual test: can one sandbox reach another?](manual-tests/sandbox-reachability.md).

| backend | can one sandbox reach another? |
| --- | --- |
| `hvi` on macOS | **yes.** Measured on 2026-09-27 with brig v0.3.0 ([#364](https://github.com/brig-sh/brig/issues/364)): one sandbox fetched a file over HTTP that only the other served. With `--network isolated` on both, the same request timed out. An earlier hull release gave "no" on the shared network. What changed the answer is not known |
| `vz` on macOS | not measured on a current hull |
| `qemu` on macOS | not measured. It takes its network from vmnet, as `vz` does |
| Linux, nerdctl/urunc microVMs | **yes** on `shared`. On 2026-09-30, one microVM fetched the other's HTTP marker. On separate isolated networks, the same request timed out while connection controls passed. The [ARM run](manual-tests/sandbox-reachability.md#2026-09-30-linux-arm64-under-qemu) used documented console and runtime setup workarounds. The [amd64 run](manual-tests/sandbox-reachability.md#2026-10-02-linux-amd64-with-the-stock-runtime) on 2026-10-02 gave the same results with the shipped runtime, unmodified |

So on Linux and on `hvi`, two agents on `shared` that you gave *different*
credentials can each reach whatever the other is listening on. That is a real
hole in the [narrow blast radius](#what-is-still-exposed). The radius is
narrow per guest home and per token, not per sandbox.

The `hvi` answer changed between two measurements, and nothing in Brig
noticed at the time. Do not treat any answer about `shared` in the table as a
property of Brig.

### Terminal output

What the agent writes to your terminal can do more than draw. A few escape
sequences make the terminal answer, and the answer lands on standard input.
An agent that read a hostile README can try any of these:

- An OSC 52 query reads the system clipboard. An OSC 52 write sets it.
- `tmux` and `screen` forward DCS sequences verbatim to the *outer* terminal.
- A cursor-position query makes the terminal type its reply onto your shell's
  standard input.

Brig itself filters nothing. `brig` hands the tty over with `syscall.Exec`
and is gone before the agent produces a byte. That gives correct `^C`
handling and a truthful exit status. Whether the bytes are filtered depends
on the runtime that Brig hands the tty to.

| Command | Filters control sequences? |
| --- | --- |
| `brig run`, `brig sh` on macOS | Yes. Brig hands the tty to `hull exec -t`, and hull filters what reaches your terminal |
| `brig run`, `brig sh` on Linux | No. nerdctl and docker pass every byte the agent emits to your terminal emulator unexamined |
| `brig run` with `--json` | As without it. `brig` stays alive as the agent's parent, so that it can print one status line after the agent exits. It runs the same command, reads nothing the agent prints and writes nothing to it, and the exit status is still the agent's own |
| `hull exec`, `hull logs` | Yes. hull stays in the middle of that stream |
| `brig logs` | Yes, by default. It reads a log back and does not drive a terminal. `--raw` turns the filter off and hands you the bytes with that surface intact |

hull's filter keeps a terminal agent working. Color, cursor movement, mouse
tracking, bracketed paste and the alternate screen pass through. It drops
the sequences that ask the terminal for a reply or reach past the display:
OSC 52 queries, OSC 1337, palette queries, device and status reports,
window reports, and DCS, SOS, PM and APC strings. It filters only when the
output is a terminal; a pipe or a file gets the bytes as they are.

Two things still pass on macOS:

- **OSC 52 writes.** That is how vim, tmux and agents copy, and blocking it
  broke copy and paste. A guest can set your clipboard, and what it set
  reaches whatever you paste into next.
- **C1 controls spelled as UTF-8.** hull drops them as raw bytes, but not
  yet in their two-byte UTF-8 form
  ([brig-sh/hull#17](https://github.com/brig-sh/hull/issues/17)).

`HULL_TERMINAL_FILTER=off` turns hull's filter off and gives the guest your
terminal. It is there for when the filter gets in the way, and it is the
wrong thing to leave set.

On Linux, or with the filter off, run Brig inside a terminal you are willing
to lose if terminal output matters for your threat model.

### The guest home

Brig does not protect the guest home from the agent. Everything in there is
writable, because that is where the agent works.

### Spending

Brig does not stop an agent from spending your money. The `deny` list keeps a
metered key from being forwarded by accident. That is a different and much
smaller promise.

## Trust assumptions

Brig narrows what an agent can reach. You must still trust four things.

**The guest image, and whoever publishes it.** An image under
`ghcr.io/brig-sh/` is checked against a specific build workflow. An image Brig
did not publish boots on a warning under the default mode. `BRIG_VERIFY=off`
turns the check off for either kind (see
[Verification modes](#verification-modes)). In each case, the image is code
that runs with your credentials.

**The profile author.** A profile names the image, the volumes it hostmounts,
and any `files:` binding. A `files:` binding is the one channel the `deny`
list does not cover. A profile is only as careful as its author.

**hull or nerdctl.** The kernel boundary, whether the shared network
forwards traffic between guests, and every device and namespace decision
belong to the runtime, not to Brig. Brig reports what it resolved. It
neither hardens nor weakens what the runtime does.

**Brig itself.** Brig runs as you, on the host, outside the sandbox. A
resolved credential is in its process memory as plaintext for the run's
lifetime. Brig clears that memory on the way out. That is defence in depth,
not a control. Brig does not claim to protect against a process that already
holds your credentials in memory.
