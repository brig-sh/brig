# Writing an agent profile

Two words mean different things. The command is `brig agent`: `brig agent
ls`, `brig agent edit`, and the `<ref>` every verb takes. Profile names the
file format this page documents, the directory it lives in
(`$XDG_CONFIG_HOME/brig`), and the variable that points somewhere else
(`BRIG_PROFILE_DIR`). You run an agent, and you edit a profile.

Brig boots an agent from an OCI image. Any Linux CLI that runs in one already
works, so a profile is not a requirement. It saves you from spelling out the
image, the guest home and the credential variables on every invocation.

Start from the closest existing profile rather than from a blank file:

```bash
brig agent new mine --from claude-code   # writes ~/.config/brig/mine.yaml
brig agent edit mine                     # change the image, and what it forwards
brig run mine
```

The name you give `new` is a name, not a path. Brig puts the file in your
profile directory, the only place a profile file does anything, and writes
nowhere else. A destination with a `/` in it is refused, not honoured.

It is also the name the profile itself carries. Brig keys on the `name:`
field inside the file, not on the file name, so `new` writes the name you
gave it into the file. `mine.yaml` says `name: mine`, and every command that
takes a profile takes `mine` from that point on. Nothing else in the file
changes, so the image, the guest home and the comments still describe the
profile you copied. That is what the edit in the second line is for.

The exported file carries a header comment that explains every field, so you
can edit it without coming back here. `brig agent edit` opens a file-backed
profile directly in `$VISUAL`, then `$EDITOR`, then `vi`. It only works on a
profile backed by a file. A built-in has none, so it says so, prints the
commands to create one, and creates nothing itself.

This page has two halves. If you only run the built-in agents, read up to
[Removing a profile](#removing-a-profile) and stop there. It covers where
profiles live, the eight Brig ships, exporting and importing them, `kind`,
`brig info`, `reserved` and `deny`. Everything after that is for writing a
profile of your own.

## Where profiles live

The eight profiles Brig ships are embedded in the binary. Brig works with no
setup: there is nothing to install before `brig run claude` boots a sandbox.

Your own live as one file per profile in `$XDG_CONFIG_HOME/brig`, default
`~/.config/brig`, flat: `~/.config/brig/claude-code.yaml`. `BRIG_PROFILE_DIR`
overrides the location outright. The older `BRIG_TEMPLATE_DIR` is still
honoured for one release.

This follows the
[XDG Base Directory Specification, version 0.8](https://specifications.freedesktop.org/basedir/latest/).
An empty `XDG_CONFIG_HOME` counts as unset, and a relative one is ignored as
invalid. That matters here, because Brig runs from whatever project
directory you are in. Honouring a relative value can resolve profiles
against the current directory and quietly give you a different set per
project.

The old `~/.config/brig/templates` default is not read, and nothing is
migrated for you. These files name credential variables, and there is no
safe guess about which ones you still want. Brig notices files left there
and says so on every invocation, until you move them across with
`brig agent import`.

**The directory starts empty, and Brig never writes there unless you ask
it to.** Nothing is pre-seeded on first run. `brig agent import`,
`brig agent export <agent> <name>` and `brig agent new <name> --from <agent>`
are the only commands that write to it. The last two refuse to overwrite
an existing file unless you pass `--force`.

A profile can be backed by more than one file, because a file need not be
named after the profile inside it. Two files declaring one name is a mistake
with no good winner: which one wins depends on where the names happen to
sort. Brig reports the collision and says which one won. `brig agent rm`
removes all of them, after asking, because a second file is by definition
not the file you named.

A file can take a built-in's name. That is deliberate: it is how you pin
your own image for a profile Brig already knows, without inventing a second
name for it. `brig agent ls` lists the merged set, embedded and file-backed
together in one namespace, and marks where each came from. Unmarked means
embedded, `(file)` means a profile that exists only as a file, and
`(file, overrides built-in)` means a file shadowing an embedded one.

## The eight built-in profiles

| profile | alias | kind | image |
| --- | --- | --- | --- |
| `claude-code` | `claude` | agent | `ghcr.io/brig-sh/claude-code-stock:latest` |
| `claude-desktop` | `desktop` | gui | `ghcr.io/nofireai/urunc-claude-desktop:aarch64` |
| `codex` |  | agent | `ghcr.io/brig-sh/codex-stock:latest` |
| `cursor` |  | agent, example profile | `ghcr.io/brig-sh/cursor:latest` |
| `gemini` |  | agent, example profile | `ghcr.io/brig-sh/gemini-stock:latest` |
| `grok` |  | agent, example profile | `ghcr.io/brig-sh/grok-stock:latest` |
| `opencode` |  | agent, example profile | `ghcr.io/brig-sh/opencode-stock:latest` |
| `ubuntu` |  | shell | `docker.io/library/ubuntu:latest` |

"example profile" is the profile's own `desc:`, and `brig agent ls` prints
it. `claude-code` and `codex` carry no such marker.

Seven of the eight name an image Brig expects you to be able to pull.
`cursor` is the exception: it declares `unpublished: true`, so `brig run`
refuses it before reaching the registry, and `brig agent ls` appends
`(no published image)`. Pass your own `--image` to run it anyway. Whether
the other seven pull today is a registry fact rather than a Brig fact: this
table states only what each profile declares.

Five of those seven, `claude-code`, `codex`, `gemini`, `grok` and `opencode`,
are Brig's own multi-architecture `:latest` builds under `ghcr.io/brig-sh`.
That is the registry Brig can verify a signature against. `claude-desktop` and
`ubuntu` point outside it, at `ghcr.io/nofireai/` and Docker Hub, so Brig
cannot check their signature and warns on every boot
([security.md](security.md)). `claude-desktop`'s image is also
single-architecture, `aarch64` only.

`claude-desktop` is the one shipped `kind: gui` profile, and `ubuntu` the
one shipped `kind: shell` profile. The other six are `kind: agent`, the
default. See [`kind`, and what each one does](#kind-and-what-each-one-does)
below.

## Export and import

```bash
brig agent export claude-code                # prints to stdout
brig agent export claude-code mine           # ~/.config/brig/mine.yaml
brig agent export claude-code mine --force   # ...overwriting what is there
brig agent export claude-code > ./mine.yaml  # a copy somewhere of your own
brig agent export x | brig agent import -
```

With no destination, export prints to stdout, so it composes into a pipe.
With one, it writes the file, comments and all, exactly as Brig ships it,
not a re-marshalled struct with the explanations stripped. Import stores
your bytes exactly as written too, so whichever way the file reached you,
your comments and your ordering survive.

**The destination is a name, never a path.** Brig writes to the profile
directory and nowhere else, so a path, or a typo that looks like one, is
refused rather than honoured. For a copy somewhere else, export to stdout
and redirect it, under the shell's own rules rather than Brig's.

A destination that is already spoken for is refused too, because the
destination is the name written into the file. `claude` is how Brig spells
`claude-code`, and a profile actually named `claude` wins the lookup over
that alias. `brig agent new claude --from claude-code` then makes every
`brig run claude` mean the copy, leaving the built-in reachable only under
its full name. A name a `reserved:` profile owns is refused for the
same reason. Both refusals say what the collision is, so pick another name.

Export writes YAML, because a profile is a file a person edits, and YAML
allows comments. Use `brig agent show --json` if something downstream reads
profiles programmatically. JSON is a subset of YAML, so import reads both
with the same parser and neither has to guess.

## `kind`, and what each one does

Three values, and each changes what `brig run` does once the guest is up.

- **`kind: agent`**, the default. `brig run` execs `binary:` and passes your
  trailing arguments to it. `binary:` is required.
- **`kind: shell`**. `brig run` opens a login shell, and trailing words run
  as one command instead. There is no CLI to pass arguments to: `brig sh`
  always execs `bash -l` (or `bash -lc` for a command), never `binary:`.
  So the field is not required and not read for this kind. `ubuntu` sets
  `binary: bash` anyway, which documents the shell without Brig acting on
  it.
- **`kind: gui`**. The sandbox boots with a graphical console and there is
  nothing to attach to. Starting the sandbox is the whole command, and
  `brig run` refuses a trailing argument rather than discarding it.
  `guiTitle:` names the window. Only the `vz` hypervisor backend shows a
  console, so Brig refuses to boot a `kind: gui` profile on `hvi` or `qemu`.

The older `shell:` and `gui:` booleans still parse and fold into `kind:`
when the file is read. See [migration.md](migration.md#profile-keys).

Two of the eight shipped profiles are not agents: `claude-desktop` is
`kind: gui`, and `ubuntu` is `kind: shell`. The `agent` command group's own
help text calls the group "the agents you can run" (`brig agent --help`).
That holds for six of the eight and not for those two. `brig agent ls`
still lists them, but running one opens a window or a shell rather than an
agent CLI.

A `kind: shell` or `kind: gui` profile cannot carry `policy:`. Neither has
an agent process to hook an egress rule into. Brig refuses the profile
at parse time rather than accept a rule nothing enforces.

## `brig info`, to see what a run sends

`brig info <profile>` reports what the guest is handed, by name only,
never a value, on any path. A variable sourced from the secret store is
annotated `(secret)`, and one from the deprecated `hostCredential:` is
annotated `(host)`. An ambient or literal one is reported bare. No test
pins the exact wording, so treat this as the shape rather than a literal
transcript:

```console
$ brig info mine
brig: workspace /Users/you/brig/mine (sandbox brig-mine)
brig: runtime hull (/opt/homebrew/bin/hull)
brig: image ghcr.io/brig-sh/claude-code-stock:latest (pull missing)
brig: forwarding to guest:
brig:   GH_TOKEN(secret)
brig:   CI
brig:   EDITOR
brig: guest git over HTTPS: off (BRIG_GIT_CONFIG=1 to enable)
```

That is not incidental. The reporting name travels as its own parameter,
separate from the value, from the point a binding is resolved. The report
is built from that name list alone, so no path through this command reads
a value. A test fails the build if one ever reaches the output.

`brig info` resolves secrets the same way any other run does, so a profile
missing one from the store fails the same way `brig run` does. It is not a
preview that quietly skips what it cannot resolve.

`BRIG_ENV_ARGV=1` still puts an ordinary forwarded variable on the runtime's
own command line, for a runtime build that will not take a bare
`--env KEY`. It is deliberately inert for a value Brig resolved on your
behalf, one bound from the secret store or the host credential. The host
durably logs every exec's argv, and a debugging escape hatch is not worth
turning into a credential leak. On a runtime build that needs the hatch, the
credential does not arrive at all, rather than arriving in the log. That is
the intended trade.

## `reserved`, so a session name cannot land on the wrong guest home

`claude-desktop` owns the Desktop app's guest home, so `brig run
claude@desktop` is refused rather than quietly landing a Claude Code session
there. That protection is `reserved: true` on the `claude-desktop` profile.

The trailing word is reserved too, not only the full name: `claude-desktop`
reserves `desktop` as well as `claude-desktop` itself. Watch for the
consequence, because it is easy to trip over by accident. A profile of your
own named `my-codex` with `reserved: true` reserves `codex` along with
`my-codex`. So `brig agent import` of anything else named `codex` is
refused as a collision, even though nothing on Brig's side is called that.

## `deny` is the billing guard

Some provider variables outrank the credential you actually want the
sandbox to use. `ANTHROPIC_API_KEY` beats Claude Code's own subscription
credential in its precedence. Forwarding it moves the sandbox off your
subscription and onto metered API billing without telling you.

Put anything with that property in `deny`. Brig refuses it with an
explanation, and `BRIG_ALLOW_DENIED=1` is the deliberate override for
someone who does want metered billing.

`deny` guards the environment channel, and only that one. A `files:`
binding is not checked against it, and no name check can be. A profile can
deliver a metered key inside a `settings.json`, and nothing sees it. That
is deliberate rather than an oversight. `deny` exists to catch accident, an
ambient variable swept in because it happened to be in your shell. A
file binding takes an explicit stored secret plus an explicit binding you
wrote.

The guard applies the same way when the value arrives by `ref:` instead of
straight from the environment: see
[`secrets` and `env`](#secrets-and-env-for-a-credential-brig-resolves-itself)
below. The billing consequence of a metered key does not change with where
the value came from.

## Removing a profile

```bash
brig agent rm mytool
```

The argument is a profile name, not a file name. `rm` resolves it through
the merged set, so it takes aliases and finds the file whatever that file
is called. If more than one file declares the name, it removes all of them.
Removing only the one that loaded promotes the other and leaves the
profile listed exactly as before.

That resolution is why `rm` asks. `mytool.yaml` for `brig agent rm mytool`
is the file you named, and it goes without a word. Anything else is a file
Brig found that you did not type. That includes an alias, a second file
declaring the same profile, or a file renamed by hand. Brig names the file
and waits for a `y`. `-y` answers in advance, the way `brig secret delete`
spells it, and with no terminal to ask on, `rm` refuses and says so rather
than assuming yes:

```bash
brig agent rm claude -y   # the alias resolves to claude-code's file
```

Built-in profiles are compiled in, so there is nothing to remove. Import a
profile of the same name to shadow one instead.

## The fields

| field | required | what it is |
| --- | --- | --- |
| `name` | yes | The profile name. It becomes the guest home directory and the sandbox name, so it is restricted to lowercase letters, digits, dot, dash and underscore |
| `image` | yes | The guest image to boot |
| `guestHome` | yes | Absolute path where the guest home is mounted. The agent's state lands here, which is what makes the guest home the unit of persistence |
| `kind` | no | What sort of workload this is: `agent` (the default), `shell` or `gui`. See [`kind`, and what each one does](#kind-and-what-each-one-does) above |
| `binary` | yes, for `kind: agent` | The agent CLI inside the guest |
| `mem`, `cpus` | yes | Guest size. Both must be greater than zero |
| `desc` | no | One line, shown by `brig agent ls` |
| `secrets` | no | Names this profile wants out of Brig's own secret store, checked before the sandbox is created. Each entry says whether a run without it stops, and where `brig secret import` can find it. See below |
| `env` | no | The variables the guest sees, and where each one's value comes from: a literal, a stored secret, or Brig's own environment. See below |
| `forward` | no | Deprecated, see [migration.md](migration.md#profile-keys). Still works, folded into `env` when the file is read |
| `files` | no | Credential files the guest sees: which stored secret fills each one, and where under `guestHome` it is written. See below |
| `volumes` | no | What is mounted inside `guestHome`: a `tmpfs` that nothing written to can reach host disk, how big it can grow, and the `hostmount` exceptions kept across boots. See below |
| `deny` | no | Variables never bound, whatever `env` or `forward` says. See above |
| `statePaths` | no | Deprecated, see [migration.md](migration.md#profile-keys). Still parses, but declaring it alongside `volumes:` is an error, not a merge |
| `staleCredentialFiles` | no | Paths an older wrapper used to write a credential into. Brig never does, so finding one is worth a warning rather than a deletion |
| `headless` | no | The agent supports a non-interactive run |
| `guiTitle` | no | Window title, for a `kind: gui` profile |
| `network` | no | The sandbox's network posture: `shared` (the default, one network for every sandbox on this host), `isolated` (a network of this sandbox's own) or `offline` (no route out at all). `BRIG_NETWORK`, `--network` and `--offline` win over it |
| `hypervisor` | no | macOS backend to boot on: `vz` (the default when the field is absent, and the only one with a graphical console), `hvi` or `qemu`. Six of the eight shipped profiles say `hvi`. `BRIG_HYPERVISOR` wins over it. Ignored on Linux, where the shim decides |
| `runtimeBin` | no | The runtime binary to drive instead of the one on `PATH`, `~` expanded. Unlike every other field this is about your machine rather than the workload, so it does not travel usefully to anyone else: it is how you pin a profile to a build you are working on without exporting a variable in every shell. `BRIG_RUNTIME_BIN` wins over it |
| `rootfsType` | no | How the guest root reaches the microVM: `block`, `virtiofs` or `9pfs`. Left unset, the runtime picks its own default, which is what a profile that only runs an agent wants. Set `block` when the sandbox installs packages and needs a real writable disk rather than a share sized to the image |
| `genericBoot` | no | The image was never built to be a guest, a plain OCI image with no kernel and no urunc metadata. The runtime supplies the kernel and initrd and boots it unmodified, on macOS and Linux alike. See below |
| `hostConfigDir`, `projectPaths` | no | Where the user's own agent configuration lives on the host, and which subdirectories of it to seed into the guest home, only when the run passes `--skills` or sets `BRIG_SKILLS=1`. Both fields are required together, and only `claude-code` declares them, so `--skills` does nothing on the other seven |
| `onboarding` | no | A first-run state file to seed. See below |
| `hostCredential` | no | **Deprecated, removed next release**, see [migration.md](migration.md#profile-keys). A credential read from the host keychain on every run when the environment carries none. Replaced by `secrets` with `sources`, filled once by `brig secret import`. See below |
| `reserved` | no | Marks a profile that owns the guest home a session name can otherwise slug onto. See above |
| `unpublished` | no | We ship the profile but not an image for it. `brig run` says so and stops, rather than letting the pull fail against the registry with a 404 that reads like an outage. Pass `--image` with one you built, and `brig agent ls` marks it. `cursor` is the one that carries it |
| `policy` | no | Names of policies attached to this profile inline: every run carries all of them, unioned with whatever is attached separately by name |

A misspelled field is refused rather than ignored. `forwards:` instead of
`forward:` otherwise decodes into nothing, forwards no credentials, and
looks exactly like a broken sandbox.

Two fields above are deprecated but still parse: `forward:` and
`statePaths:`. `forward:` folds into an equivalent `env:` binding.
`statePaths:`, superseded by `volumes:`, is refused rather than merged if
you declare both alongside each other. See
[migration.md](migration.md#profile-keys) for what each becomes.

One consequence of that folding is worth knowing. `brig agent show --json`
always prints the `env:` form, even for a profile written with `forward:`.
JSON export marshals the parsed profile, and by then `forward:` is gone
from it. Plain YAML export is not affected, since it hands back the file
exactly as written.

## `secrets` and `env`, for a credential brig resolves itself

`secrets:` is what a profile requires out of Brig's own secret store.
`env:` is where each variable the guest sees actually comes from: a
literal, one of those secrets, or Brig's own environment.

```yaml
name: mine
image: ghcr.io/brig-sh/claude-code-stock:latest
guestHome: /home/claude
binary: claude
mem: 4096
cpus: 4
secrets:
  - gh_token
env:
  - name: GH_TOKEN
    ref: secrets.gh_token
  - name: CI
    ref: env.CI
  - name: EDITOR
    value: vi
```

**`secrets:` is the requirement list, `env:` is the binding.** Keeping them
separate is what lets Brig check the whole list up front. One error names
every missing secret, rather than the run dying on the first `ref` it
happens to resolve.

### The object form: `required:` and `sources:`

A bare string is the short spelling of a required, hand-created secret:
`secrets: [gh_token]` means `{name: gh_token, required: true}`, so every
profile written before this schema keeps parsing unchanged. The object form
says two more things:

```yaml
secrets:
  - name: claude-credentials
    required: false                     # warn and boot, instead of refusing
    expiryField: expiresAt              # found at any depth; drives the stale warning
    sources:                            # where `brig secret import` looks
      - from: keychain
        service: Claude Code-credentials
      - from: file
        path: ~/.claude/.credentials.json
        hint: run `claude` on the host once to log in
```

**`required:` decides whether the run stops.** Absent means required, which
is why a bare string keeps meaning what it always meant. `required: false`
warns and boots, which is what the shipped `claude-code` does. An agent
that can complete its own login inside the sandbox is free to try.

**`sources:` decides which command the error names.** They are tried in
order, and the first that exists wins. That is what makes a profile
portable without a per-platform predicate. The same entry names the macOS
keychain and the Linux file, and Brig never has to know which host maps to
which. A secret with no `sources:` is hand-created by definition, and its
message names `brig secret create` rather than `brig secret import`.

Three `from:` values exist, each taking exactly one locator. A `keychain`
source carrying a `path:` is a parse error, not something silently
ignored. Ignoring it reads as a working portable chain that resolves
nothing:

| `from:` | locator | what it reads |
| --- | --- | --- |
| `keychain` | `service:` | a macOS keychain generic-password item. macOS only: the Linux store is a Secret Service keyring ([secrets.md](secrets.md#linux)), and no source reads from it |
| `file` | `path:` | a host file, verbatim. A leading `~` is expanded when it is read, so a profile carries no one host's home directory |
| `env` | `var:` | a host environment variable, **copied once at import** |

That last row is the one to read twice. `from: env` and `ref: env.<name>`
are the same word with opposite temporal semantics. A `ref:` is read live
on every run. A `from: env` source copies the value into the store
when you type `brig secret import`, and never looks again. Prefer the
`refs:` chain below for anything that expires. The shipped `claude-code`
uses no `from: env` source, for exactly this reason.

`field:` and `expiryField:` say how to read a secret's value and its expiry
out of whatever a source yields. They sit on the secret rather than on a
source. That is only possible because every source for one secret must
yield the same document shape. An absent `field:` stores the value
verbatim, which is what a file-shaped secret wants. The host keychain blob
is the format the agent's credentials file takes, so nothing is extracted
and no field Brig does not understand is lost.

The boundary that follows from that is worth stating rather than leaving
to be discovered. A credential whose two host locations differ in shape
needs two secrets. A `files:` binding can name only one, with two
bindings on one `path:` refused. So an agent whose credential file differs
per platform cannot be expressed as a single portable profile today.
Claude's two locations happen to share a shape.

On macOS the keychain source answers first, so nothing in a supported
configuration today ever reaches `path: ~/.claude/.credentials.json`. That
path is the documented Linux location rather than an observed one. On a
Linux host with a keyring, import reads it and stores into the Secret
Service backend ([secrets.md](secrets.md#linux)). It is data that costs
three lines and makes the profile portable.

### What happens when a secret is missing

The run fails before any sandbox is created. The error names the
secret, the sandbox it was needed for, and the command that creates it:

```console
$ brig run mine
brig: missing secret "gh_token" needed by the brig-mine sandbox -- create it first with: brig secret create gh_token
```

A profile missing more than one gets every name in the same error, not one
failed run per secret:

```console
brig: missing 2 secrets needed by the brig-mine sandbox:
  gh_token: create it first with: brig secret create gh_token
  npm_token: create it first with: brig secret create npm_token
```

This is not a warning and not a skipped binding. A sandbox whose
environment was built without a credential it was told it needs is exactly
what the requirement list exists to prevent.

An optional secret is not that. It warns, names what it did not find and
the command that supplies it, and the run boots:

```console
$ brig run claude-code
brig: no value for the secret "claude-credentials", and claude-code will run without it.
brig: To carry it in from your host: brig secret import claude-code
brig: run `claude` on the host once to log in
```

A secret with no `sources:` gets the other verb, because import cannot fill
it:

```console
brig: no value for the secret "gh-token", and claude-code will run without it.
brig: To supply one: brig secret create gh-token
```

On Linux the store is a Secret Service keyring, and a host without one, no
session bus or nothing answering on it, has no store. A required secret
therefore fails every run on such a host, for the same reason and in the
same way. Failing closed is correct, but it is worth knowing before your
first run of such a profile there rather than after. An optional one is
silent there instead of warning on every run: there is no store to create
it in. Nothing you do on that host, short of installing a keyring, changes
the outcome, so a warning about it is noise rather than information.
That is why the shipped `claude-code` still boots on Linux.

### The `ref` grammar, and `refs:` chains

`ref: secrets.<name>` reads the named secret out of Brig's own store.
`ref: env.<name>` reads the named variable out of Brig's own environment,
the same place `forward:` always read from. A namespace that is neither is
a parse error naming the two that exist.

`refs:` is a list of those, and the first that resolves wins. `ref:` is a
`refs:` of length one, so a binding carries one spelling or the other and
never both:

```yaml
env:
  - name: GH_TOKEN
    refs: [env.GH_TOKEN, secrets.gh-token]   # shell override first, store second
```

That order is not decoration. A name a profile binds is dropped from the
ambient forward. So a bare `ref: secrets.gh-token` makes
`GH_TOKEN=$(gh auth token) brig run claude-code` stop reaching the guest,
and `BRIG_FORWARD_ENV` cannot restore it. The chain is what lets a
profile add a store fallback without taking the shell override away. That
is why the shipped `claude-code` binds `GH_TOKEN` this way.

A chain also decides what a run has to read. If an earlier `env.` element
resolves, the later `secrets.` element is not needed. A run the
environment already satisfies never opens the store at all: no keychain is
touched for a value the shell already supplied.

A `secrets.<name>` ref whose name is absent from `secrets:` is a parse
error too. The requirement list is what makes the up-front check complete,
and a ref that bypasses it is a secret nothing ever checks for.

For a sandbox served by `brigd`, an `env.<name>` ref resolves against the
daemon's environment, not the shell that sent the request. That is how
forwarding has always worked through the daemon, not new here. A `ref:`
is the first thing that puts the source in the file where you can see it.

### `value:`, for a literal

`value:` sets the guest variable to a literal, for configuration that is
not a credential, `EDITOR: vi` above, say. An entry has exactly one of
`value:` or `ref:`. Both or neither is refused.

### `BRIG_FORWARD_ENV` replaces the env-sourced set, and only that set

`BRIG_FORWARD_ENV` still overrides which variables are carried in from
Brig's own environment. Before bindings existed that was the entire
mechanism, so overriding it replaced everything a profile forwarded.
Now a `ref: secrets.<name>` binding is the profile's own declaration of
what the workload needs, and it survives the override untouched.

## `files`, for a credential the agent reads from disk

A `files:` entry names a stored secret and where inside the guest it is
written:

```yaml
files:
  - ref: secrets.claude-credentials
    path: .claude/.credentials.json    # relative to guestHome
    mode: "0600"                       # default "0600"; quoted, see below
```

**The author's rule: use `files:` wherever the agent can read a credential
from one, and `env:` where it cannot.** A file stays out of
`/proc/<pid>/environ`, is not inherited by the processes the agent spawns,
and can be rewritten under a running agent. A rotated secret can reach a
live session that way. An environment variable can do none of those.

The env channel is not deprecated and stays supported. `GEMINI_API_KEY`,
`XAI_API_KEY`, `OPENROUTER_API_KEY` and `CURSOR_API_KEY` are env-only by
their agents' own design, and a file for them is a file nothing reads.
Binding one secret through both channels is legal, and the exposure is the
union of the two. Do it only when something genuinely reads both.

Four rules the parser enforces:

- **`ref:` only, never `refs:`.** A chain exists to give one environment
  variable a shell override and a store fallback, and a file has no shell
  to override it. `env.<name>` is refused as well: a file binding exists to
  put a stored credential where an agent reads one.
- **`mode:` is a quoted string.** YAML reads an unquoted `0600` as decimal
  600, which is `0o1130`, a mode nobody meant and that nothing reports.
- **The target must sit inside a `tmpfs` volume**, and must not be carved
  back out by a `hostmount` under it. This is refused at parse time rather
  than at delivery, where it is a live token already on your disk.
- **One binding per `path:`.** A file has one source.

`field:` on a file-bound secret is legal and almost always wrong. It
writes a bare token where the agent expects a document, so the agent
attempts a refresh, fails, and prompts. Leave `field:` off unless the
agent really does read a one-line file.

An unresolved binding leaves nothing behind: no file, no mount, no empty
target. An empty file at a credential path is indistinguishable from a
real leak to whoever finds it. It is also a login prompt the agent cannot
explain.

`brig secret push`, for rotating a file binding in a running sandbox, is
not implemented yet. The mechanism already is: a re-run rewrites the
file under a live agent.

## `volumes`, for what reaches host disk

`volumes:` is what is mounted inside `guestHome`, one primitive per entry:

```yaml
volumes:
  - kind: tmpfs
    path: .claude               # memory-only: nothing written here reaches the host
    size: 512m                  # optional, default 64m
  - kind: hostmount
    path: .claude/sessions      # ... except these, kept across boots
  - kind: hostmount
    path: .claude/projects
  - kind: hostmount
    path: .claude/history.jsonl
    file: true                  # the target is a file, not a directory
```

A `tmpfs` covers a directory so that nothing written under it can reach
your disk. That is what makes a `files:` binding safe. The safety is
fail-closed by construction rather than by inspection, because there is no
path from the credential to the host to check. Brig still verifies it.
The covered path must read as `tmpfs` and `/proc/swaps` must be empty, or
the run stops rather than handing over a credential.

A `hostmount` names an exception: one path bound back out to the same path
in the guest home, which is where that state already lives. Its source is
implicit, so `source:` is refused on it. A `hostmount` not nested under a
`tmpfs` is a parse error. The guest home is already `guestHome`, so such an
entry mounts a path onto itself and reads as protection that is not there.

Order in the file is taste. Brig mounts parents before children by path
depth, because declaration order is a trap otherwise. A profile listing
`.claude/sessions` above `.claude` mounts the tmpfs over the hostmount
already made, silently losing the state it named.

`size:` is how big a `tmpfs` can grow, `64m` when a volume does not say.
Only a `tmpfs` takes one: a `hostmount` is as large as the guest home it
comes from. It is a ceiling the guest meets as `ENOSPC` from its own tools,
with nothing on the host watching for it. Size it for what the mount
actually holds. A directory of configuration is not the same as an agent's
home, where edit history and per-job scratch accumulate. A value that is
not a number optionally followed by `k`, `m` or `g` is refused at parse
time. `mount(8)` answers an option it cannot parse by failing the
boot.

`kind: volume` is reserved for a named volume several sandboxes share. It
is parsed and refused as not yet supported. A profile written against it
fails with the reason it fails, and lands unchanged when it is
implemented.

`claude-code` and `claude-desktop` are the only two shipped profiles that
declare `volumes:`. They are the only two where in-guest state can live
in memory rather than on host disk. The other six persist everything under
`guestHome` to host disk, with nothing memory-only. Five, `codex`, `cursor`,
`gemini`, `grok` and `opencode`, declare only the deprecated `statePaths:`,
and `ubuntu` declares neither.

### What this costs

If an agent writes state under a `tmpfs`-covered path with no matching
`hostmount`, that state does not survive `brig stop`.

That gap is not the same on every profile. `claude-code`'s tmpfs carves
out `.claude/skills` and `.claude/plugins` as hostmounts, so a `--skills`
copy there survives a stop. `claude-desktop`'s tmpfs covers `.claude` too,
but its hostmounts stop at `settings.json`, `CLAUDE.md`, `sessions`,
`projects`, `plugins` and `history.jsonl`. `.claude/skills` is not among
them, so anything the bundled desktop app writes there is lost at
shutdown, unlike the same path under `claude-code`.

## `onboarding`, for an agent that stops on a first-run screen

Some agents ask something on first run that is not authentication, and
that the guest cannot answer. Picking a login method there opens a browser
the microVM does not have. Seeding a couple of non-secret flags into the
agent's own state file settles it:

```yaml
onboarding:
  file: .claude.json
  seed:
    hasCompletedOnboarding: true
    hasTrustDialogAccepted: true
  trustKey: [projects, hasTrustDialogAccepted]
```

`seed` is written only when `file` does not exist. An existing file belongs
to the agent, and Brig will not overwrite it.

`trustKey` names the two JSON levels around a directory name, for an agent
that records trust per directory. Brig sets it for the directory each run
starts in, resolved to the git repository root as the guest sees it.
Nothing is ever seeded that contains a credential.

## `hostCredential`, deprecated

**Removed in the next release.** It read another application's keychain
item on every run, whenever the environment carried no value for its
target variable. It then forwarded what it found as an environment
variable. `brig secret import` replaces it: it reads the host once, when
you ask, instead of on every boot. `BRIG_CREDENTIALS_CMD`, the older way
to point that host read at a command of your own, is gone too. Brig
refuses to start when it is set, and names `brig secret import <profile>
<name> --from-command '<command>'` as the replacement.

Each `hostCredential:` field has a `secrets:`/`env:` equivalent:

| `hostCredential:` field | replacement |
| --- | --- |
| `keychainService` | a `sources:` entry with `from: keychain` and `service:` |
| `tokenField` | `field:` on the secret |
| `expiryField` | `expiryField:` on the secret, unchanged |
| `targetVar` | the `name:` on the `env:` binding that references the secret |
| `renewHint` | `hint:` on the secret or its source |

See [migration.md](migration.md#profile-keys) for the rest of what changed.

## A worked example

Say we have a CLI called `mytool` in an image of our own:

```yaml
# The vendored CLI needs a bigger guest than the default.
name: mytool
desc: our internal agent
image: ghcr.io/example/mytool:arm64
guestHome: /home/mytool
binary: mytool
forward:
  - MYTOOL_TOKEN
  - GH_TOKEN
deny:
  - MYTOOL_ADMIN_KEY   # lets the agent reconfigure the account
statePaths:
  - .config/mytool
headless: true
mem: 8192
cpus: 4
```

```bash
brig agent import mytool.yaml
MYTOOL_TOKEN=$(pass show mytool/token) brig run mytool
```

`forward:` and `statePaths:` here are the deprecated spellings.

Brig cannot verify the signature of an image outside `ghcr.io/brig-sh`, so
it will say so on every boot. That is a warning, not a refusal. See
[security.md](security.md).

## `genericBoot`, for an image that is not a guest image

A guest image normally carries its own kernel and the urunc metadata that
says how to boot it. `genericBoot: true` says this one does not: it is an
ordinary OCI image such as `ubuntu:latest`, and the runtime supplies the
kernel and the initrd instead.

```yaml
name: plain-ubuntu
image: docker.io/library/ubuntu:latest
guestHome: /root/work
kind: shell
binary: bash
genericBoot: true
rootfsType: block        # room to apt install
mem: 4096
cpus: 2
```

The image itself is never modified. Its own argv, environment, hostname and
mounts are restored before the guest switches root, so it runs as it does
anywhere else.

This works on both operating systems, and for the same reason: the kernel
and the initrd travel as two OCI annotations. On macOS, hull takes them on
its command line. On Linux, urunc reads the same two from the container's
OCI spec, which nerdctl passes through. Nothing about the profile changes
between them.

Three constraints come with it. The kernel and initrd are host files, and
are never taken from image metadata. An image must not be able to nominate
a file on your machine. On macOS Brig asks hull where they are, because
they live under hull's store and only hull knows where that is. On Linux it
looks in `~/.local/share/brig/assets`. `BRIG_BOOT_ASSETS` overrides both.
The kernel is named for the architecture it boots: `Image` on arm64,
`bzImage` on x86_64. The guest agent that `brig sh` talks to comes from
that initrd rather than from the image. That is what makes an unmodified
image drivable at all.

You do not have to put them there yourself. If they are missing, Brig
fetches them once, on the first `brig run` of a `genericBoot` profile.

Which tool does the fetching differs, because what is guaranteed to be on
the machine differs. On macOS it is hull: it downloads the same bundle for
its own `hull run`. It already knows the reference, the directory and
the registry credentials, so Brig drives that instead of keeping a second
copy of all three. On Linux hull does not exist, since it does not
build there. So Brig uses `oras`, the same shell-out-if-present shape it
uses for `cosign` when checking signatures. If oras is not installed, it
says so and tells you what to fetch.

Setting `BRIG_BOOT_ASSETS` turns the fetching off entirely. That variable
points at a build you are iterating on, and downloading a release bundle
over your own work is the opposite of helpful. `BRIG_BOOT_ASSETS_REF`
pins a specific bundle instead of the current one for your platform.

The bundle is published as the OCI artifact `ghcr.io/nofireai/hull-assets`,
one tag per guest platform, and pulls anonymously. The repository that
builds it is not public. `oras pull ghcr.io/nofireai/hull-assets:<os>-<arch>
--output <dir>` is the same fetch by hand.

On Linux this needs `nerdctl` rather than `docker`. Docker does not carry
OCI annotations through to the runtime, and a sandbox booted through it has
no kernel. Brig refuses up front instead.

## Building the image

The image is the part Brig does not do for you. Building one is documented
in [bring-your-own-image.md](https://github.com/brig-sh/community-images/blob/main/docs/bring-your-own-image.md).
The built-in profiles' own images are open source at
[brig-sh/community-images](https://github.com/brig-sh/community-images).
[guest-image.md](guest-image.md) is the full contract: every binary Brig
execs inside the guest, and the account it expects. It also names a script
that checks an image you built against it.
