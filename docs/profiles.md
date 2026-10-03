# Writing an agent profile

**Agent** is the word on the command line: `brig agent ls`, `brig agent
edit`, and the `<ref>` every verb takes. **Profile** is the file behind an
agent: the format this page documents, the directory it lives in
(`$XDG_CONFIG_HOME/brig`), and the variable that points somewhere else
(`BRIG_PROFILE_DIR`). You run an agent, and you edit a profile.

Brig boots an agent from an OCI image, and any Linux CLI that runs in one
works. A profile saves you from spelling out the image, the guest home and the
credential variables on every invocation.

Start from the closest existing profile:

```bash
brig agent new mine --from claude-code   # writes ~/.config/brig/mine.yaml
brig agent edit mine                     # change the image, and what it forwards
brig run mine
```

The argument to `new` is a name, not a path. Brig writes the file into your
profile directory, the only place a profile file has any effect. A
destination with a `/` in it is refused.

It is also the name the profile itself carries. Brig keys on the `name:`
field inside the file, not on the file name, so `new` writes the name you
gave it into the file. `mine.yaml` says `name: mine`, and every command that
takes a profile takes `mine` from then on. Nothing else in the file changes,
so the image, the guest home and the comments still describe the profile you
copied. Change them with `brig agent edit`.

The file carries a header comment that explains every field. `brig agent
edit` opens a file-backed profile in `$VISUAL`, then `$EDITOR`, then `vi`. A
built-in has no file, so for one `edit` prints the commands that create a
copy and creates nothing itself.

If you only run the built-in agents, read up to
[Removing a profile](#removing-a-profile). That part covers where profiles
live, the eight Brig ships, exporting and importing them, `kind`,
`brig info`, `reserved` and `deny`. Everything after it is for writing a
profile of your own.

## Where profiles live

The eight profiles Brig ships are embedded in the binary, so `brig run
claude` needs no profile setup.

Your own live as one file per profile in `$XDG_CONFIG_HOME/brig`, default
`~/.config/brig`, with no subdirectories: `~/.config/brig/claude-code.yaml`.
`BRIG_PROFILE_DIR` overrides the location. The older `BRIG_TEMPLATE_DIR`
still works until v0.4.0.

This follows the
[XDG Base Directory Specification, version 0.8](https://specifications.freedesktop.org/basedir/latest/).
An empty `XDG_CONFIG_HOME` counts as unset, and a relative one is ignored as
invalid. Brig runs from whatever project directory you are in, so a relative
value would resolve profiles against the current directory and give you a
different set per project.

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
named after the profile inside it. Two files declaring one name is a
mistake, and which one wins depends on how the file names sort. Brig reports
the collision and says which one won. `brig agent rm` removes all of them,
after asking.

A file can take a built-in's name. That is how you pin your own image for a
profile Brig already knows, without inventing a second name for it.
`brig agent ls` lists the merged set, embedded and file-backed
together in one namespace, and marks where each came from. Unmarked means
embedded, `(file)` means a profile that exists only as a file, and
`(file, overrides built-in)` means a file shadowing an embedded one.

## The eight built-in profiles

| profile | alias | kind | image |
| --- | --- | --- | --- |
| `claude-code` | `claude` | agent | `ghcr.io/brig-sh/claude-code-stock:root` |
| `claude-desktop` | `desktop` | gui | `ghcr.io/nofireai/urunc-claude-desktop:aarch64` |
| `codex` |  | agent | `ghcr.io/brig-sh/codex-stock:root` |
| `cursor` |  | agent, example profile | `ghcr.io/brig-sh/cursor:latest` |
| `gemini` |  | agent, example profile | `ghcr.io/brig-sh/gemini-stock:root` |
| `grok` |  | agent, example profile | `ghcr.io/brig-sh/grok-stock:root` |
| `opencode` |  | agent, example profile | `ghcr.io/brig-sh/opencode-stock:root` |
| `ubuntu` |  | shell | `docker.io/library/ubuntu:latest` |

"example profile" is the profile's own `desc:`, and `brig agent ls` prints
it. `claude-code` and `codex` carry no such marker.

Seven of the eight name an image Brig expects you to be able to pull.
`cursor` is the exception: it declares `unpublished: true`, so `brig run`
refuses it before reaching the registry, and `brig agent ls` appends
`(no published image)`. Pass your own `--image` to run it anyway. The table
states what each profile declares. It does not say whether the registry
serves the image today.

Five of those seven, `claude-code`, `codex`, `gemini`, `grok` and `opencode`,
are Brig's own builds under `ghcr.io/brig-sh`, the registry Brig can verify a
signature against. `claude-desktop` and `ubuntu` point outside it, at
`ghcr.io/nofireai/` and Docker Hub, so Brig cannot verify their signature and
warns on every boot
([security.md](security.md)). `claude-desktop`'s image is also
single-architecture, `aarch64` only.

`claude-desktop` is the one shipped `kind: gui` profile, and `ubuntu` the
one shipped `kind: shell` profile. The other six are `kind: agent`, the
default. See [`kind`, and what each one does](#kind-and-what-each-one-does)
below.

The six shipped `hvi` profiles use `network: isolated`; `claude-desktop`
uses `network: shared` because its GUI requires `vz`. The unpublished
`cursor` profile leaves the choice unset: Linux and `hvi` isolate, while
`vz` and `qemu` fall back to `shared`.
These are defaults for new sandboxes. Existing sessions keep their
recorded posture, or the actual posture inspected from the runtime when
an older session has no record. See [Network postures](policies.md#network-postures).

## Export and import

```bash
brig agent export claude-code                # prints to stdout
brig agent export claude-code mine           # ~/.config/brig/mine.yaml
brig agent export claude-code mine --force   # ...overwriting what is there
brig agent export claude-code > ./mine.yaml  # a copy somewhere of your own
brig agent export x | brig agent import -
```

With no destination, export prints to stdout. With one, it writes the file
as Brig ships it, comments included. Import also stores your bytes exactly as
written, so your comments and your ordering survive.

An exported built-in also carries its explicit `network:` choice. If you
copy a profile that names `isolated` and change its backend to `vz` or
`qemu`, change the network to `shared` too: those backends cannot enforce
isolation, and an explicit request for it is refused.

**The destination is a name, never a path.** Brig writes to the profile
directory and nowhere else, so a path, or a typo that looks like one, is
refused. For a copy somewhere else, export to stdout and redirect it.

A destination that is already spoken for is refused too, because the
destination is the name written into the file. `claude` is how Brig spells
`claude-code`, and a profile actually named `claude` wins the lookup over
that alias. `brig agent new claude --from claude-code` then makes every
`brig run claude` mean the copy, leaving the built-in reachable only under
its full name. A name a `reserved:` profile owns is refused for the
same reason. Both refusals say what the collision is, so pick another name.

Export writes YAML, because a profile is a file a person edits, and YAML
allows comments. Use `brig agent show --json` if a program reads the
profile. JSON is a subset of YAML, so import reads both with the same parser.

## `kind`, and what each one does

`kind` takes three values, and each changes what `brig run` does once the
guest is up.

- **`kind: agent`**, the default. `brig run` execs `binary:` and passes your
  trailing arguments to it. `binary:` is required.
- **`kind: shell`**. `brig run` opens a login shell, and trailing words run
  as one command instead, each word one argument to it, or as a script after
  `-c`. There is no CLI to pass arguments to: `brig sh` always execs
  `bash -l`, or runs the command or script under `bash -lc`, never `binary:`.
  So the field is not required and not read for this kind. `ubuntu` sets
  `binary: bash` anyway, which documents the shell without Brig acting on
  it.
- **`kind: gui`**. The sandbox boots with a graphical console and there is
  nothing to attach to. `brig run` only starts the sandbox, and refuses a
  trailing argument.
  `guiTitle:` names the window. Only the `vz` hypervisor backend shows a
  console, so Brig refuses to boot a `kind: gui` profile on `hvi` or `qemu`.

The older `shell:` and `gui:` booleans still parse and fold into `kind:`
when the file is read. See [migration.md](migration.md#profile-keys).

Two of the eight shipped profiles are not agents: `claude-desktop` is
`kind: gui`, and `ubuntu` is `kind: shell`. `brig agent ls` lists them with
the other six, but running one opens a window or a shell.

A `kind: shell` or `kind: gui` profile cannot carry `policy:`. Neither has
an agent process to hook an egress rule into, so Brig refuses the profile at
parse time.

## `brig info`, to see what a run sends

`brig info <profile>` reports what the guest is handed, by name only,
never a value, on any path. A variable sourced from the secret store is
annotated `(secret)`. An ambient or literal one is reported bare. No test
pins the exact wording, so treat this as the shape of the output. The `...`
stands for the envelope block that comes first (`PROFILE`, `SANDBOX`,
`WORKSPACE` and the rest):

```console
$ brig info mine
...
brig: workspace /Users/you/.brig/homes/brig-mine (sandbox brig-mine)
brig: runtime hull (/opt/homebrew/bin/hull)
brig: image ghcr.io/brig-sh/claude-code-stock:root (pull missing)
brig: image verification: warn, against brig's own trust policy
brig: forwarding to guest:
brig:   GH_TOKEN(secret)
brig:   CI
brig:   EDITOR
brig: guest git over HTTPS: off (BRIG_GIT_CONFIG=1 to enable)
```

The reporting name travels as its own parameter, separate from the value,
from the point a binding is resolved. The report is built from that name
list alone, so no path through this command reads a value. A test fails the
build if one ever reaches the output.

`brig info` resolves secrets the same way a run does, so a profile missing a
required secret fails here as it does under `brig run`.

`BRIG_ENV_ARGV=1` still puts an ordinary forwarded variable on the runtime's
own command line, for a runtime build that will not take a bare
`--env KEY`. It has no effect on a value Brig resolved on your behalf, one
bound from the secret store, because the host durably logs every exec's
argv. On a runtime build that needs the hatch, such a credential does not
arrive at all, rather than arriving in the log.

## `reserved`, so a session name cannot land on the wrong guest home

A session's guest home is named after the agent and the label joined by a
dash, so the pair can spell another profile's name. `claude-desktop` owns the
Desktop app's guest home and sets `reserved: true`, so Brig refuses a session
whose agent and label join to `claude-desktop`.

`brig run claude@desktop` is not such a session. `claude` is an alias of
`claude-code`, so its guest home is `~/.brig/homes/brig-claude-code-desktop`,
which no profile owns. A profile of your own named `claude` changes that: it
wins the lookup over the alias, and `claude@desktop` is then refused.

The trailing word is reserved for profile names too: `brig agent import`
refuses a profile named `desktop`. A profile of your own named `my-codex`
with `reserved: true` reserves `codex` in the same way, so importing a
profile named `codex` is refused as a collision, including one meant to
override the built-in.

## `deny` is the billing guard

Some provider variables outrank the credential you actually want the
sandbox to use. `ANTHROPIC_API_KEY` beats Claude Code's own subscription
credential in its precedence. Forwarding it moves the sandbox off your
subscription and onto metered API billing without telling you.

Put anything with that property in `deny`. Brig does not forward a denied
variable and says why. `BRIG_ALLOW_DENIED=1` is the override for someone who
does want metered billing.

`deny` guards the environment channel, and only that one. A `files:`
binding is not checked against it, and no name check can be. A profile can
deliver a metered key inside a `settings.json`, and nothing sees it. That
is deliberate. `deny` exists to catch an accident: an ambient variable
forwarded because it happened to be in your shell. A file binding takes an
explicit stored secret plus an explicit binding you wrote.

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
is called. If more than one file declares the name, it removes all of them,
because removing only the one that loaded would promote the other and leave
the profile listed as before.

`brig agent rm mytool` removes `mytool.yaml` without asking, because that is
the file you named. Any other file is one Brig found and you did not type:
one reached through an alias, a second file declaring the same profile, or a
file renamed by hand. Brig names the file and waits for a `y`. `-y` answers
in advance, as it does for `brig secret delete`. With no terminal to ask on,
`rm` refuses:

```bash
brig agent rm claude -y   # the alias resolves to claude-code's file
```

`rm` refuses while a sandbox of the profile exists, running or stopped, and
names the `brig rm <ref>` for each. Once the file is gone, `brig rm <ref>`
cannot reach those sandboxes. The way out is then `brig rm --all`, or a new
profile of the same name. An override of a built-in is not refused, because
the name still resolves to the built-in.

To find those sandboxes, `rm` asks the profile's runtime what it holds. That
is the profile's `runtimeBin` only once Brig has recorded a session of the
profile, and the runtime on `PATH` before that, so `rm` never executes the
`runtimeBin` of a profile you imported and never ran. If Brig cannot ask,
`rm` keeps the file and names the cause: `BRIG_RUNTIME` set to a runtime Brig
does not know, for example, or a `runtimeBin` that points at nothing on a
profile Brig has run. Fix that setting first. With no runtime installed at
all, there are no sandboxes, and `rm` goes ahead.

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
| `volumes` | no | What is mounted inside `guestHome`: a `tmpfs` whose contents never reach host disk, how big it can grow, and the `hostmount` exceptions kept across boots. See below |
| `deny` | no | Variables Brig does not forward unless `BRIG_ALLOW_DENIED=1` is set. A profile that also binds one in `env` or `forward` is refused. See above |
| `statePaths` | no | Deprecated, see [migration.md](migration.md#profile-keys). Still parses, but declaring it alongside `volumes:` is an error, not a merge |
| `staleCredentialFiles` | no | Paths an older wrapper used to write a credential into. Brig never does. It warns when it finds one and does not delete it |
| `headless` | no | The agent supports a non-interactive run |
| `guiTitle` | no | Window title, for a `kind: gui` profile |
| `network` | no | The sandbox's network posture: `shared` (one network for sandboxes using it), `isolated` (a network of this sandbox's own) or `offline` (no route out at all). Defaults to `isolated` for a new sandbox, with a reported `shared` fallback on hull's `vz` and `qemu` backends when no posture is named. An explicit `isolated` is refused on `vz` and `qemu`. An existing sandbox keeps its recorded or runtime-inspected posture; `BRIG_NETWORK`, `--network` and `--offline` win over both |
| `hypervisor` | no | macOS backend to boot on: `vz` (the default when the field is absent, and the only one with a graphical console), `hvi` or `qemu`. Six of the eight shipped profiles say `hvi`. `BRIG_HYPERVISOR` wins over it. Ignored on Linux, where the shim decides |
| `runtimeBin` | no | The runtime binary to drive instead of the one on `PATH`, `~` expanded. It describes your machine, not the workload, so it is no use to anyone you share the profile with. Use it to pin a profile to a build you are working on without exporting a variable in every shell. `BRIG_RUNTIME_BIN` wins over it |
| `rootfsType` | no | How the guest root reaches the microVM: `block`, `virtiofs` or `9pfs`. Left unset, the runtime picks its own default, which suits a profile that only runs an agent. Set `block` when the sandbox installs packages and needs a writable disk instead of a share sized to the image |
| `genericBoot` | no | The image was never built to be a guest, a plain OCI image with no kernel and no urunc metadata. The runtime supplies the kernel and initrd and boots it unmodified, on macOS and Linux alike. See below |
| `hostConfigDir`, `projectPaths` | no | Where the user's own agent configuration lives on the host, and which subdirectories of it to seed into the guest home, only when the run passes `--skills` or sets `BRIG_SKILLS=1`. Both must be set for either to take effect, and only `claude-code` declares them, so `--skills` does nothing on the other seven |
| `onboarding` | no | A first-run state file to seed. See below |
| `reserved` | no | Marks a profile that owns the guest home a session name can otherwise slug onto. See above |
| `unpublished` | no | Brig ships the profile but no image for it. `brig run` says so and stops before the pull, which would otherwise fail with a registry 404. Pass `--image` with one you built. `brig agent ls` marks the profile, and `cursor` is the one that carries it |
| `policy` | no | Names of policies attached to this profile inline: every run carries all of them, unioned with whatever is attached separately by name |

A misspelled field is refused. If `forwards:` in place of `forward:` were
ignored, the profile would forward no credentials and look like a broken
sandbox.

Two fields above are deprecated but still parse: `forward:` and
`statePaths:`. `forward:` folds into an equivalent `env:` binding.
`statePaths:`, superseded by `volumes:`, is refused rather than merged if
you declare both alongside each other. See
[migration.md](migration.md#profile-keys) for what each becomes.

Because of that folding, `brig agent show --json` always prints the `env:`
form, even for a profile written with `forward:`. JSON export marshals the
parsed profile, and by then `forward:` is gone from it. Plain YAML export
hands back the file exactly as written.

## `secrets` and `env`, for a credential Brig resolves itself

`secrets:` is what a profile requires out of Brig's own secret store.
`env:` says where each variable the guest sees comes from: a literal, one of
those secrets, or Brig's own environment.

```yaml
name: mine
image: ghcr.io/brig-sh/claude-code-stock:root
guestHome: /root
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
separate lets Brig check the whole list up front, so one error names every
missing secret.

### The object form: `required:` and `sources:`

A bare string is the short spelling of a required, hand-created secret:
`secrets: [gh_token]` means `{name: gh_token, required: true}`, so every
profile written before this schema keeps parsing unchanged. The object form
says more:

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

**`required:` decides whether the run stops.** Absent means required.
`required: false` warns and boots. The shipped `claude-code` uses it,
because the agent can complete its own login inside the sandbox.

**`sources:` decides which command the error names.** Sources are tried in
order, and the first that exists wins. That makes a profile portable: the
same entry names the macOS keychain and the Linux file. A secret with no
`sources:` is hand-created, and its message names `brig secret create`
instead of `brig secret import`.

Three `from:` values exist, each taking exactly one locator. A `keychain`
source carrying a `path:` is a parse error. If it were ignored, the profile
would look portable and resolve nothing:

| `from:` | locator | what it reads |
| --- | --- | --- |
| `keychain` | `service:` | a macOS keychain generic-password item. macOS only: the Linux store is a Secret Service keyring ([secrets.md](secrets.md#linux)), and no source reads from it |
| `file` | `path:` | a host file, verbatim. A leading `~` is expanded when it is read, so a profile carries no one host's home directory |
| `env` | `var:` | a host environment variable, **copied once at import** |

`from: env` and `ref: env.<name>` share a word and behave differently. A
`ref:` is read on every run. A `from: env` source copies the value into the
store when you run `brig secret import`, and never reads it again. Prefer
the `refs:` chain below for anything that expires. The shipped `claude-code`
uses no `from: env` source for this reason.

`field:` and `expiryField:` say how to read a secret's value and its expiry
out of whatever a source yields. They sit on the secret, not on a source, so
every source for one secret must yield the same document shape. An absent
`field:` stores the value
verbatim, which is what a file-shaped secret wants. The host keychain blob
is the format the agent's credentials file takes, so nothing is extracted
and no field Brig does not understand is lost.

This has one limit. A credential whose two host locations differ in shape
needs two secrets. A `files:` binding can name only one, with two
bindings on one `path:` refused. So an agent whose credential file differs
per platform cannot be expressed as a single portable profile today.
Claude's two locations happen to share a shape.

On macOS the keychain source is tried first, and
`path: ~/.claude/.credentials.json` is read only when the keychain item does
not exist. That path is the documented Linux location. On a Linux host with
a keyring, import reads it and stores into the Secret Service backend
([secrets.md](secrets.md#linux)).

### What happens when a secret is missing

The run fails before any sandbox is created. The error names the
secret, the sandbox it was needed for, and the command that creates it:

```console
$ brig run mine
brig: missing secret "gh_token" needed by the brig-mine sandbox -- create it first with: brig secret create gh_token
```

A profile missing more than one gets every name in the same error:

```console
brig: missing 2 secrets needed by the brig-mine sandbox:
  gh_token: create it first with: brig secret create gh_token
  npm_token: create it first with: brig secret create npm_token
```

A missing required secret is never reduced to a warning or a skipped
binding.

A missing optional secret is a warning. It names what was not found and the
command that supplies it, and the run boots:

```console
$ brig run claude-code
brig: claude-code runs without 1 secret
  ○ claude-credentials  → brig secret import claude-code
                          ↳ run `claude` on the host once to log in
```

A secret with no `sources:` gets the other verb, because import cannot fill
it:

```console
brig: claude-code runs without 1 secret
  ○ gh-token  → brig secret create gh-token
                ↳ export GH_TOKEN before running brig, or store one: gh auth token | brig secret create gh-token
```

On Linux the store is a Secret Service keyring, and a host without one, no
session bus or nothing answering on it, has no store. A required secret
therefore fails every run on such a host. An optional one is silent there:
there is no store to create it in, so a warning on every run would give you
nothing to act on. That is why the shipped `claude-code` still boots on such
a host.

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

The order matters. A name a profile binds is dropped from the ambient
forward. So a bare `ref: secrets.gh-token` makes
`GH_TOKEN=$(gh auth token) brig run claude-code` stop reaching the guest,
and `BRIG_FORWARD_ENV` cannot restore it. A chain lets a profile add a store
fallback and keep the shell override, which is why the shipped `claude-code`
binds `GH_TOKEN` this way.

A chain also decides what a run has to read. If an earlier `env.` element
resolves, the later `secrets.` element is not needed, and a run the
environment already satisfies never opens the store.

A `secrets.<name>` ref whose name is absent from `secrets:` is a parse
error too. The requirement list is what makes the up-front check complete,
and a ref that bypasses it is a secret nothing ever checks for.

For a sandbox served by `brigd`, an `env.<name>` ref resolves against the
daemon's environment, not the shell that sent the request. Forwarding
through the daemon has always worked that way.

### `value:`, for a literal

`value:` sets the guest variable to a literal, for configuration that is
not a credential, such as `EDITOR: vi` above. An entry has exactly one of
`value:`, `ref:` or `refs:`. Any other combination is refused.

### `HOME`, `PATH`, `TMPDIR` and `XDG_*`

The runtime reads these names for itself, so Brig passes their guest values
on the runtime's command line as `NAME=value`. See
[security.md](security.md#not-in-argv). A binding for one of them takes
`value:` or `ref: env.<name>`. One that resolves from the secret store is
refused when the sandbox boots or execs, because a stored secret never goes
on the command line.

### `BRIG_FORWARD_ENV` replaces the env-sourced set, and only that set

`BRIG_FORWARD_ENV` overrides which variables are carried in from Brig's own
environment: it replaces the profile's `ref: env.<name>` bindings. A
`ref: secrets.<name>` binding is the profile's own declaration of what the
workload needs, and it survives the override.

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

The env channel stays supported. `GEMINI_API_KEY`, `XAI_API_KEY`,
`OPENROUTER_API_KEY` and `CURSOR_API_KEY` are env-only by their agents' own
design, so nothing would read a file for them. Binding one secret through
both channels is legal, and the exposure is the union of the two. Do it only
when something reads both.

The parser enforces four rules:

- **`ref:` only, never `refs:`.** A chain exists to give one environment
  variable a shell override and a store fallback, and a file has no shell
  to override it. `env.<name>` is refused as well: a file binding exists to
  put a stored credential where an agent reads one.
- **`mode:` is a quoted string.** YAML turns an unquoted `0600` into a number
  before Brig sees it, and the result is refused or, for some values, read
  as a different mode.
- **The target must sit inside a `tmpfs` volume**, and must not be carved
  back out by a `hostmount` under it. This is refused at parse time, before
  any token can reach your disk.
- **One binding per `path:`.** A file has one source.

`field:` on a file-bound secret is legal and almost always wrong. It
writes a bare token where the agent expects a document, so the agent
attempts a refresh, fails, and prompts. Leave `field:` off unless the
agent really does read a one-line file.

An unresolved binding leaves nothing behind: no file, no mount, no empty
target. An empty file at a credential path is indistinguishable from a
real leak to whoever finds it. It is also a login prompt the agent cannot
explain.

There is no `brig secret push` yet for rotating a file binding in a running
sandbox. A re-run does it: it rewrites the file under the live agent.

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
your disk. That is what makes a `files:` binding safe, and it is safe by
construction: there is no path from the credential to the host. Brig still
verifies it. The covered path must read as `tmpfs` and `/proc/swaps` must be
empty, or the run stops before it hands over a credential.

A `hostmount` names an exception: one path bound back out to the same path
in the guest home, which is where that state already lives. Its source is
implicit, so `source:` is refused on it. A `hostmount` not nested under a
`tmpfs` is a parse error. The guest home is already `guestHome`, so such an
entry mounts a path onto itself and reads as protection that is not there.

Order in the file does not matter. Brig mounts parents before children by
path depth. Mounting in declaration order would let a profile that lists
`.claude/sessions` above `.claude` mount the tmpfs over the hostmount and
lose the state it named.

`size:` is how big a `tmpfs` can grow, `64m` when a volume does not say.
Only a `tmpfs` takes one: a `hostmount` is as large as the guest home it
comes from. It is a ceiling the guest meets as `ENOSPC` from its own tools,
with nothing on the host watching for it. Size it for what the mount holds:
an agent's home, where edit history and per-job scratch accumulate, needs
more than a directory of configuration. A value that is not a number
optionally followed by `k`, `m` or `g` is refused at parse time, because
`mount(8)` fails the boot on an option it cannot parse.

`kind: volume` is reserved for a named volume several sandboxes share. It
is parsed and refused as not yet supported.

`claude-code` and `claude-desktop` are the only two shipped profiles that
declare `volumes:`. They are the only two where in-guest state can live
in memory rather than on host disk. The other six persist everything under
`guestHome` to host disk, with nothing memory-only. Five, `codex`, `cursor`,
`gemini`, `grok` and `opencode`, declare only the deprecated `statePaths:`,
and `ubuntu` declares neither.

### What this costs

If an agent writes state under a `tmpfs`-covered path with no matching
`hostmount`, that state does not survive `brig stop`.

The gap differs by profile. `claude-code`'s tmpfs carves
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

## A worked example

This profile runs a CLI called `mytool` from an image of your own:

```yaml
# The vendored CLI needs a large guest.
name: mytool
desc: our internal agent
image: ghcr.io/example/mytool:arm64
guestHome: /home/mytool
binary: mytool
env:
  - name: MYTOOL_TOKEN
    ref: env.MYTOOL_TOKEN
  - name: GH_TOKEN
    ref: env.GH_TOKEN
deny:
  - MYTOOL_ADMIN_KEY   # lets the agent reconfigure the account
headless: true
mem: 8192
cpus: 4
```

```bash
brig agent import mytool.yaml
MYTOOL_TOKEN=$(pass show mytool/token) brig run mytool
```

Brig cannot verify the signature of an image outside `ghcr.io/brig-sh`, so
it says so on every boot. That is a warning, not a refusal. See
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

This works on both operating systems, because the kernel and the initrd
travel as two OCI annotations. On macOS, hull takes them on
its command line. On Linux, urunc reads the same two from the container's
OCI spec, which nerdctl passes through. Nothing about the profile changes
between them. On Linux that takes the urunc the runtime bundle builds,
because no urunc release reads the pair
([runtimes.md](runtimes.md#what-brig-requires-of-each)).

The kernel and initrd are host files, and are never taken from image
metadata, so an image cannot nominate a file on your machine. On macOS Brig
asks hull where they are, because they live under hull's store. On Linux it
looks in `~/.local/share/brig/assets`. `BRIG_BOOT_ASSETS` overrides both.
The kernel is named for the architecture it boots: `Image` on arm64,
`bzImage` on x86_64. The guest agent that `brig sh` talks to comes from that
initrd, not from the image, which is what makes an unmodified image
drivable.

If the files are missing, Brig fetches them once, on the first `brig run` of
a `genericBoot` profile. On macOS hull does the fetching: it downloads the
same bundle for its own `hull run`, and already knows the reference, the
directory and the registry credentials. On Linux, where hull does not build,
Brig shells out to `oras`. If `oras` is not installed, Brig says so and tells
you what to fetch.

Setting `BRIG_BOOT_ASSETS` turns the fetching off, so Brig never downloads a
release bundle over a build you are working on. `BRIG_BOOT_ASSETS_REF` pins
a specific bundle instead of the current one for your platform.

The bundle is published as the OCI artifact `ghcr.io/nofireai/hull-assets`,
one tag per guest platform, and pulls anonymously. The repository that
builds it is not public. `oras pull ghcr.io/nofireai/hull-assets:<os>-<arch>
--output <dir>` is the same fetch by hand.

On Linux this needs `nerdctl`. Docker does not carry OCI annotations through
to the runtime, so a sandbox booted through it would have no kernel, and
Brig refuses up front.

## Building the image

Brig does not build the image for you. Building one is documented
in [bring-your-own-image.md](https://github.com/brig-sh/community-images/blob/main/docs/bring-your-own-image.md).
The built-in profiles' own images are open source at
[brig-sh/community-images](https://github.com/brig-sh/community-images).
[guest-image.md](guest-image.md) is the full contract: every binary Brig
execs inside the guest, and the account it expects. It also names a script
that checks an image you built against it.
