# Writing an agent profile

A profile is the file that tells Brig how to run one agent. It holds the
image, the guest home and the credential variables, so you do not type them on
every invocation. Brig boots an agent from an OCI image, and any Linux CLI
that runs in one works.

| word | what it names |
| --- | --- |
| **Agent** | The word on the command line: `brig agent ls`, `brig agent edit`, and the `<ref>` that every verb takes |
| **Profile** | The file behind an agent, the directory that it lives in (`$XDG_CONFIG_HOME/brig`), and the variable that points somewhere else (`BRIG_PROFILE_DIR`) |

You run an agent, and you edit a profile.

## Make one from a built-in

Copy the closest built-in profile, change one field, and check the result
before you run it. This example gives the `ubuntu` shell more memory:

```bash
brig agent new mine --from ubuntu   # writes ~/.config/brig/mine.yaml
brig agent edit mine                # change mem: 2048 to mem: 4096
brig plan mine                      # boots nothing
```

`brig plan` prints what a run of `mine` gets. Its `LIMITS` row shows the
change:

```
LIMITS       4096 MB, 2 vCPUs
```

Then run it with `brig run mine ~/code/demo`. To undo, run
`brig agent rm mine`, which deletes the file.

`new` takes a name, not a path, and writes the file into your
[profile directory](#where-profiles-live). It sets the `name:` field, which
is how Brig identifies the profile. Everything else, comments included,
still describes the profile you copied. A header comment in the file
explains every field.

`brig agent edit` opens a file-backed profile in `$VISUAL`, then `$EDITOR`,
then `vi`. A built-in profile has no file. For a built-in profile, `edit`
prints the commands that create a copy and creates nothing.

To run only the built-in agents, read up to
[Removing a profile](#removing-a-profile). The sections after it are for a
profile of your own.

## Where profiles live

`brig run claude` needs no profile setup, because the nine profiles that Brig
ships are embedded in the binary.

Your own profiles live as one file per profile, with no subdirectories:

| setting | profile directory |
| --- | --- |
| Default | `~/.config/brig`, for example `~/.config/brig/claude-code.yaml` |
| `XDG_CONFIG_HOME` | `$XDG_CONFIG_HOME/brig` |
| `BRIG_PROFILE_DIR` | The directory that it names. It overrides the location |

Brig follows the
[XDG Base Directory Specification, version 0.8](https://specifications.freedesktop.org/basedir/latest/).
It treats an empty or relative `XDG_CONFIG_HOME` as unset.

The directory starts empty. Only these commands write to it:

- `brig agent import`
- `brig agent export <agent> <name>`
- `brig agent new <name> --from <agent>`

`export` and `new` refuse to overwrite an existing file unless you pass
`--force`.

A file can take the name of a built-in profile, to pin your own image for it.
`brig agent ls` lists embedded and file-backed profiles together, and marks
the origin of each:

| mark | origin |
| --- | --- |
| No mark | Embedded |
| `(file)` | A profile that exists only as a file |
| `(file, overrides built-in)` | A file that shadows an embedded profile |

A file need not be named after the profile inside it, so two files can
declare the same name. That is a mistake. Brig reports the collision and
names the file that won, which depends on how the file names sort.
`brig agent rm` removes all of the files, after it asks.

<details>
<summary>Locations from older versions</summary>

- Brig refuses to start while `BRIG_TEMPLATE_DIR` is set and
  `BRIG_PROFILE_DIR` is not.
- Brig does not read `~/.config/brig/templates`. If files remain there, Brig
  says so on every invocation. Move them across with `brig agent import`.

</details>

## The nine built-in profiles

| profile | alias | kind | image |
| --- | --- | --- | --- |
| `claude-code` | `claude` | agent | `ghcr.io/brig-sh/claude-code-stock:root` |
| `claude-desktop` | `desktop` | gui | `ghcr.io/nofireai/urunc-claude-desktop:aarch64` |
| `codex` |  | agent | `ghcr.io/brig-sh/codex-stock:root` |
| `cursor` |  | agent, example profile | `ghcr.io/brig-sh/cursor:latest` |
| `gemini` |  | agent, example profile | `ghcr.io/brig-sh/gemini-stock:root` |
| `grok` |  | agent, example profile | `ghcr.io/brig-sh/grok-stock:root` |
| `herdr` | `herd` | agent | `ghcr.io/brig-sh/herdr-stock:latest` |
| `opencode` |  | agent, example profile | `ghcr.io/brig-sh/opencode-stock:root` |
| `ubuntu` |  | shell | `docker.io/library/ubuntu:latest` |

- "example profile" is the profile's own `desc:`, which `brig agent ls`
  prints.
- For `kind:`, see [`kind`](#kind).
- `herdr` is `claude-code` with sshd and a herdr server in the image. See
  [herdr.md](herdr.md).

`cursor` declares `unpublished: true`. `brig run` refuses it before it
reaches the registry, and `brig agent ls` appends `(no published image)`. To
run it, pass your own `--image`.

Signature verification depends on the registry
([security.md](security.md)):

| profiles | registry | signature |
| --- | --- | --- |
| `claude-code`, `codex`, `gemini`, `grok`, `herdr`, `opencode` | `ghcr.io/brig-sh`, Brig's own builds | Brig can verify a signature against this registry |
| `claude-desktop` | `ghcr.io/nofireai/` | Brig cannot verify the signature and warns on every boot. The image is single-architecture, `aarch64` only |
| `ubuntu` | Docker Hub | Brig cannot verify the signature and warns on every boot |

Each profile sets the network default for a new sandbox. See
[Network postures](policies.md#network-postures).

| case | network |
| --- | --- |
| The seven shipped `hvi` profiles | `network: isolated` |
| `claude-desktop` | `network: shared`, because its GUI requires `vz` |
| `cursor` | Unset. Linux and `hvi` isolate. `vz` and `qemu` fall back to `shared` |
| An existing session | Keeps its posture |

## Export and import

```bash
brig agent export claude-code                # prints to stdout
brig agent export claude-code mine           # ~/.config/brig/mine.yaml
brig agent export claude-code mine --force   # ...overwriting what is there
brig agent export claude-code > ./mine.yaml  # a copy somewhere of your own
brig agent export x | brig agent import -
```

- Export writes YAML, comments included. If a program reads the profile, use
  `brig agent show --json`.
- Import stores your bytes as written, so your comments and your ordering
  survive. It reads YAML and JSON.

Limits on the destination:

- **The destination is a name, never a path.** Brig writes only to the profile
  directory, and refuses a path. For a copy somewhere else, export to stdout
  and redirect it.
- **Brig refuses a name that is already in use.** A profile named `claude`
  wins the lookup over the alias of `claude-code`. After
  `brig agent new claude --from claude-code`, every `brig run claude` means
  the copy, and the built-in is reachable only as `claude-code`.
- **Brig refuses a name that a `reserved:` profile owns.**

Both refusals say what the collision is. Pick another name.

An exported built-in carries its `network:` choice. If you copy a profile
that names `isolated` and change its backend to `vz` or `qemu`, change the
network to `shared` too. Brig refuses `isolated` on those backends.

## `kind`

`kind` sets what `brig run` does when the guest is up. It takes three values:

| value | what `brig run` does | limits |
| --- | --- | --- |
| `kind: agent`, the default | Execs `binary:` and passes your trailing arguments to it | `binary:` is required |
| `kind: shell` | Opens a login shell. Trailing words run as one command instead, each word one argument to it, or as a script after `-c` | `binary:` is not required and not read |
| `kind: gui` | Only starts the sandbox, which boots with a graphical console. There is nothing to attach to. `guiTitle:` names the window | Brig refuses a trailing argument. Only the `vz` hypervisor backend shows a console, so Brig refuses to boot a `kind: gui` profile on `hvi` or `qemu` |

- `brig sh` always runs `bash -l`, or the command or script under
  `bash -lc`. It never runs `binary:`.
- Brig refuses a `kind: shell` or `kind: gui` profile that carries
  `policy:`.
- A profile that still has the old `shell:` or `gui:` boolean fails to
  parse, and the error names `kind:`. See
  [migration.md](migration.md#profile-keys).

## `brig info`

`brig info <profile>` reports what a run hands to the guest. It prints names
only and never a value.

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

- `(secret)` marks a variable from the secret store.
- The `...` stands for the block that comes first (`PROFILE`, `SANDBOX`,
  `WORKSPACE` and the rest).
- `brig info` resolves secrets as a run does. A profile that misses a
  required secret fails here as it does under `brig run`.

`BRIG_ENV_ARGV=1` puts ordinary forwarded variables on the runtime's command
line, for a runtime build that does not take a bare `--env KEY`. A value on
that command line can be logged to a file on host disk. Brig never does this
for a stored secret, so on such a build a stored secret does not arrive at
all. See [Forwarded values](runtimes.md#forwarded-values).

## `reserved`

`reserved: true` stops a session name from landing on the guest home that
another profile owns.

A session's guest home is named after the agent and the label, joined by a
dash, so the pair can spell another profile's name. `claude-desktop` sets
`reserved: true`, so Brig refuses a session whose agent and label join to
`claude-desktop`.

- `brig run claude@desktop` is allowed. `claude` is an alias of
  `claude-code`, so its guest home is `~/.brig/homes/brig-claude-code-desktop`.
- If you name a profile of your own `claude`, it wins over the alias, and
  Brig refuses `claude@desktop`.
- `brig agent import` refuses a profile named `desktop`.
- A profile of your own named `my-codex` with `reserved: true` reserves
  `codex` the same way. Brig then refuses to import a profile named `codex`,
  including one meant to override the built-in.

## `deny` is the billing guard

`deny` lists the variables that Brig does not forward. Brig says why when it
holds one back.

Use it for a provider variable that outranks the credential that you want the
sandbox to use. `ANTHROPIC_API_KEY` beats the subscription credential of
Claude Code. If Brig forwards it, the sandbox moves off your subscription and
onto metered API billing without a notice.

`BRIG_ALLOW_DENIED=1` is the override for a user who wants metered billing.

Limits:

- `deny` guards only the environment. Brig does not check a `files:` binding
  against it. A profile can deliver a metered key inside a `settings.json`,
  and nothing sees it.
- The guard applies the same way when the value arrives by `ref:`. See
  [`secrets` and `env`](#secrets-and-env-for-a-credential-brig-resolves-itself).

## Removing a profile

```bash
brig agent rm mytool
```

The argument is a profile name or an alias. `rm` finds the file under any
file name. If more than one file declares the name, `rm` removes all of them.

`rm` asks before it removes a file that you did not type:

| file | what `rm` does |
| --- | --- |
| `mytool.yaml`, the file that you named | Removes it without asking |
| A file reached through an alias | Names the file and waits for a `y` |
| A second file that declares the same profile | Names the file and waits for a `y` |
| A file renamed by hand | Names the file and waits for a `y` |

`-y` answers in advance, as it does for `brig secret delete`. With no terminal
to ask on, `rm` refuses:

```bash
brig agent rm claude -y   # the alias resolves to claude-code's file
```

`rm` refuses while a sandbox of the profile exists, running or stopped. It
names the `brig rm <ref>` for each sandbox. Once the file is gone,
`brig rm <ref>` cannot reach those sandboxes, and only `brig rm --all` or a
new profile of the same name removes them. `rm` does not refuse an override
of a built-in, because the name still resolves to the built-in.

To find those sandboxes, `rm` asks the profile's runtime:

- It uses the profile's `runtimeBin` only when Brig has run the profile, so
  `rm` never executes the `runtimeBin` of a profile that you imported and
  never ran.
- With no runtime installed, `rm` removes the file.
- If Brig cannot ask, `rm` keeps the file and names the cause. Examples are
  `BRIG_RUNTIME` set to an unknown runtime, or a `runtimeBin` that points at
  nothing. Fix that setting first.

Built-in profiles are compiled in, so there is nothing to remove. To shadow
one, import a profile of the same name.

## The fields

| field | required | what it is |
| --- | --- | --- |
| `name` | yes | The profile name. It becomes the guest home directory and the sandbox name. So it is restricted to lowercase letters, digits, dot, dash and underscore |
| `image` | yes | The guest image to boot |
| `guestHome` | yes | Absolute path where the guest home is mounted. The agent's state lands here, which makes the guest home the unit of persistence |
| `kind` | no | The sort of workload: `agent` (the default), `shell` or `gui`. See [`kind`](#kind) |
| `binary` | yes, for `kind: agent` | The agent CLI inside the guest |
| `mem`, `cpus` | yes | Guest size. Both must be greater than zero |
| `desc` | no | One line, shown by `brig agent ls` |
| `secrets` | no | Names this profile wants out of Brig's own secret store, checked before the sandbox is created. Each entry says whether a run without it stops, and where `brig secret import` can find it. See [`secrets` and `env`](#secrets-and-env-for-a-credential-brig-resolves-itself) |
| `env` | no | The variables the guest sees, and where the value of each comes from: a literal, a stored secret, or Brig's own environment. See [`secrets` and `env`](#secrets-and-env-for-a-credential-brig-resolves-itself) |
| `files` | no | Credential files the guest sees: which stored secret fills each one, and where under `guestHome` it is written. See [`files`](#files) |
| `volumes` | no | What is mounted inside `guestHome`, or at a fixed guest path with `at:`: a `tmpfs` whose contents never reach host disk, how big it can grow, and the `hostmount` exceptions kept across boots. See [`volumes`](#volumes) |
| `deny` | no | Variables Brig does not forward unless `BRIG_ALLOW_DENIED=1` is set. Brig refuses a profile that also binds one in `env`. See [`deny`](#deny-is-the-billing-guard) |
| `staleCredentialFiles` | no | Paths where an older wrapper wrote a credential. Brig warns when it finds one and does not delete it |
| `headless` | no | The agent supports a non-interactive run |
| `guiTitle` | no | Window title, for a `kind: gui` profile |
| `network` | no | The network posture of the sandbox: `shared` (one network for sandboxes using it), `isolated` (a network for this sandbox alone) or `offline` (no route out at all). A new sandbox defaults to `isolated`. When no posture is named, the `vz` and `qemu` backends fall back to `shared` and report it. Brig refuses an explicit `isolated` on `vz` and `qemu`. An existing sandbox keeps its posture. `BRIG_NETWORK`, `--network` and `--offline` win over both |
| `hypervisor` | no | macOS backend to boot on: `vz` (the default when the field is absent, and the only one with a graphical console), `hvi` or `qemu`. Seven of the nine shipped profiles say `hvi`. `BRIG_HYPERVISOR` wins over it. On Linux, for a `genericBoot` profile, an absent field or `vz` means `hvi`, and the runtime bundle does not ship `qemu`. See [`BRIG_HYPERVISOR`](cli.md#environment-variables) |
| `runtimeBin` | no | The runtime binary to drive instead of the one on `PATH`, `~` expanded. It describes your machine, so it is of no use to a person you share the profile with. `BRIG_RUNTIME_BIN` wins over it |
| `rootfsType` | no | How the guest root reaches the microVM: `block`, `virtiofs` or `9pfs`. Set `block` when the sandbox installs packages and needs a writable disk. Left unset or set to `virtiofs`, a `genericBoot` profile boots an overlay root where the host can build one. `BRIG_ROOTFS_MODE=virtiofs` turns the overlay off. See [the guest root of a generic boot](runtimes.md#the-guest-root-of-a-generic-boot) |
| `genericBoot` | no | The image is a plain OCI image with no kernel and no urunc metadata. Brig supplies the kernel and boots the image unmodified, on macOS and Linux alike. See [`genericBoot`](#genericboot) |
| `hostConfigDir`, `projectPaths` | no | Where the user's own agent configuration lives on the host, and which subdirectories of it to seed into the guest home. Brig seeds them only when the run passes `--skills` or sets `BRIG_SKILLS=1`. Both must be set for either to take effect. Only `claude-code` and `herdr` declare them, so `--skills` does nothing on the other seven |
| `onboarding` | no | A first-run state file to seed. See [`onboarding`](#onboarding) |
| `reserved` | no | Marks a profile that owns the guest home a session name can otherwise slug onto. See [`reserved`](#reserved) |
| `unpublished` | no | Brig ships the profile but no image for it. `brig run` says so and stops before the pull. Pass `--image` with one you built. `cursor` is the one that carries it |
| `policy` | no | Names of policies attached to this profile inline: every run carries all of them, unioned with whatever is attached separately by name |

Brig refuses a misspelled field, such as `volume:` in place of `volumes:`.
It also refuses a key that v0.5.0 removed, and the error names the key that
replaced it. See [migration.md](migration.md#profile-keys).

## `secrets` and `env`, for a credential Brig resolves itself

`secrets:` lists what a profile requires out of Brig's own secret store.
`env:` says where each variable that the guest sees comes from: a literal, one
of those secrets, or Brig's own environment.

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

**`secrets:` is the requirement list, and `env:` is the binding.** Brig checks
the whole list before it creates the sandbox, so one error names every missing
secret.

### The object form

A `secrets:` entry in the object form says whether the secret is required and
where `brig secret import` finds it:

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

A bare string is the short form of a required, hand-created secret:
`secrets: [gh_token]` means `{name: gh_token, required: true}`.

| key | what it does |
| --- | --- |
| `required:` | Decides whether the run stops. Absent means required. `required: false` warns and boots |
| `sources:` | Decides which command the error names. Brig tries the sources in order, and the first that exists wins |
| `field:` | Says how to read the value of the secret out of what a source yields |
| `expiryField:` | Says how to read the expiry of the secret out of what a source yields |

- The shipped `claude-code` uses `required: false`, because the agent can
  log in inside the sandbox.
- `sources:` makes a profile portable: the same entry names the macOS keychain
  and the Linux file.
- A secret with no `sources:` is hand-created. Its message names
  `brig secret create` instead of `brig secret import`.

Four `from:` values exist. `keychain`, `file` and `env` each take one
locator and no other, and `aws` takes none. For example, a `keychain` source
that carries a `path:` is a parse error.

| `from:` | locator | what it reads |
| --- | --- | --- |
| `keychain` | `service:` | a macOS keychain generic-password item. macOS only: the Linux store is a Secret Service keyring ([secrets.md](secrets.md#linux)), and no source reads from it |
| `file` | `path:` | a host file, verbatim. A leading `~` is expanded when it is read, so a profile carries no one host's home directory |
| `env` | `var:` | a host environment variable, **copied once at import** |
| `aws` | none | no file or keychain item. `import` checks the AWS profile you name with `--aws-profile` and stores a pointer to it, for a route on the `aws` preset. Each machine names its own AWS profile, so the profile you share carries none. See [An AWS profile behind a route](secrets.md#an-aws-profile-behind-a-route) |

`from: env` and `ref: env.<name>` share a word and behave differently:

- Brig reads a `ref:` on every run.
- A `from: env` source copies the value into the store when you run
  `brig secret import`, and Brig never reads the variable again.
- For a value that expires, prefer a [`refs:` chain](#ref-and-refs).

Limits of `field:` and `expiryField:`:

- They sit on the secret and not on a source, so every source for one secret
  must yield the same document shape.
- An absent `field:` stores the value verbatim, which suits a file-shaped
  secret.
- A credential whose two host locations differ in shape needs two secrets.
  A `files:` binding names one secret, and Brig refuses two bindings on one
  `path:`. So one portable profile cannot yet serve an agent whose
  credential file differs per platform. Claude's two locations share a
  shape.

Source order in the example:

| host | what import reads |
| --- | --- |
| macOS | The keychain source first. It reads `path: ~/.claude/.credentials.json` only when the keychain item does not exist |
| Linux with a keyring | `~/.claude/.credentials.json`, the documented Linux location. Import stores it into the Secret Service backend ([secrets.md](secrets.md#linux)) |

### A missing secret

If a required secret is missing, the run fails before Brig creates a sandbox:

```console
$ brig run mine
brig: missing secret "gh_token" needed by the brig-mine sandbox -- create it first with: brig secret create gh_token
```

If a profile misses more than one secret, the same error names each one:

```console
brig: missing 2 secrets needed by the brig-mine sandbox:
  gh_token: create it first with: brig secret create gh_token
  npm_token: create it first with: brig secret create npm_token
```

A missing optional secret is a warning, and the run boots:

```console
$ brig run claude-code
brig: claude-code runs without 1 secret
  ○ claude-credentials  → brig secret import claude-code
                          ↳ run `claude` on the host once to log in
```

A secret with no `sources:` gets the other verb:

```console
brig: claude-code runs without 1 secret
  ○ gh-token  → brig secret create gh-token
                ↳ export GH_TOKEN before running brig, or store one: gh auth token | brig secret create gh-token
```

<details>
<summary>A Linux host without a keyring</summary>

On Linux the store is a Secret Service keyring. A host with no session bus, or
with nothing that answers on it, has no store.

- A required secret fails every run on such a host.
- An optional secret is silent there, because there is no store to create it
  in. So the shipped `claude-code` still boots on such a host.

</details>

### `ref` and `refs`

A `ref:` names where one value comes from. A `refs:` chain is a list of those,
and the first that resolves wins:

```yaml
env:
  - name: GH_TOKEN
    refs: [env.GH_TOKEN, secrets.gh-token]   # shell override first, store second
```

| form | what it reads |
| --- | --- |
| `ref: secrets.<name>` | The named secret, out of Brig's own store |
| `ref: env.<name>` | The named variable, out of Brig's own environment |

Limits:

- Any namespace other than `secrets` or `env` is a parse error.
- A binding carries `ref:` or `refs:`, never both.
- A `secrets.<name>` ref whose name is absent from `secrets:` is a parse
  error.
- For a sandbox that `brigd` serves, an `env.<name>` ref resolves against the
  environment of the daemon, not the shell that sent the request.

The order in a chain matters:

- A name that a profile binds is no longer forwarded from your shell. With a
  bare `ref: secrets.gh-token`, `GH_TOKEN=$(gh auth token) brig run claude-code`
  stops reaching the guest, and `BRIG_FORWARD_ENV` cannot restore it.
- A chain keeps the shell override and adds a store fallback. The shipped
  `claude-code` binds `GH_TOKEN` this way.
- If an earlier `env.` element resolves, the run does not need the later
  `secrets.` element.

### `value`

`value:` sets the guest variable to a literal. Use it for configuration that
is not a credential, such as `EDITOR: vi` in the first example. An entry has
one of `value:`, `ref:` or `refs:`, and Brig refuses any other combination.

### `HOME`, `PATH`, `TMPDIR` and `XDG_*`

Brig passes these names on the runtime's command line, so a binding for one
takes `value:` or `ref: env.<name>`. Brig refuses one that resolves from the
secret store, because a stored secret never goes on the command line. See
[security.md](security.md#not-in-argv).

### `BRIG_PROJECT`

Brig sets `BRIG_PROJECT` itself, to the guest path of the run's project, and
leaves it unset when there is no project. It ignores a binding of that name,
including one from `BRIG_FORWARD_ENV`, and prints a warning. The CREDENTIALS
row does not name the binding.

### `BRIG_FORWARD_ENV`

`BRIG_FORWARD_ENV` overrides which variables Brig carries in from its own
environment. It replaces the profile's `ref: env.<name>` bindings, and only
those. A `ref: secrets.<name>` binding survives the override.

## `files`

A `files:` entry names a stored secret and where inside the guest Brig writes
it:

```yaml
files:
  - ref: secrets.claude-credentials
    path: .claude/.credentials.json    # relative to guestHome
    mode: "0600"                       # default "0600"; quoted, see below
```

**Use `files:` wherever the agent can read a credential from a file, and
`env:` where it cannot.** A file has three properties that an environment
variable lacks:

- It stays out of `/proc/<pid>/environ`.
- The processes that the agent spawns do not inherit it.
- Brig can rewrite it under a running agent, so a rotated secret can reach a
  live session.

`GEMINI_API_KEY`, `XAI_API_KEY`, `OPENROUTER_API_KEY` and `CURSOR_API_KEY`
are env-only, because their agents read no file for them. You can bind one
secret through both channels, and the exposure is then the union of the two.
Do it only when something reads both.

Brig refuses a `files:` entry that breaks one of four rules:

- **`ref:` only, never `refs:`, and never `env.<name>`.** A file binding
  takes a stored secret.
- **`mode:` is a quoted string.** YAML reads an unquoted `0644` as the number
  420.
- **The target must sit inside a `tmpfs` volume**, and a `hostmount` under
  that volume must not carve it back out. Brig refuses this at parse time,
  before any token can reach your disk.
- **One binding per `path:`.**

Other limits:

- `field:` on a file-bound secret is legal and almost always wrong. It writes
  a bare token where the agent expects a document. The agent then attempts a
  refresh, fails, and prompts. Leave `field:` off unless the agent reads a
  one-line file.
- An unresolved binding leaves nothing behind: no file, no mount, no empty
  target.
- There is no `brig secret push` yet to rotate a file binding in a running
  sandbox. A re-run rewrites the file under the live agent.

## `volumes`

`volumes:` lists what Brig mounts inside `guestHome`, one primitive per entry.
A `tmpfs` can instead be mounted at a fixed guest path with `at:`:

```yaml
volumes:
  - kind: tmpfs
    path: .claude               # memory-only: nothing written here reaches the host
    at: /brig/claude            # optional: mount it here, off the home share
    size: 512m                  # optional, default 64m
  - kind: hostmount
    path: .claude/sessions      # ... except these, kept across boots
  - kind: hostmount
    path: .claude/projects
  - kind: hostmount
    path: .claude/history.jsonl
    file: true                  # the target is a file, not a directory
```

Order in the file does not matter.

**`kind: tmpfs`** covers a directory, so nothing written under it can reach
your disk. That is what makes a `files:` binding safe.

- Before it hands over a credential, Brig checks that the path reads as
  `tmpfs` and that the guest has no swap. Otherwise the run stops.
- `size:` sets how big the `tmpfs` can grow. The default is `64m`. Brig
  refuses a `size:` that is not a number optionally followed by `k`, `m` or
  `g`.
- The guest meets that ceiling as `ENOSPC` from its own tools, and nothing on
  the host watches for it. An agent's home, where edit history and scratch
  files grow, needs more than a directory of configuration.

**`at:`** mounts a `tmpfs` at an absolute guest path instead of at
`guestHome/path`. The guest home is a share from the host. If the host
renames or replaces a directory on it, for example with `git clean -fdx`, the
guest can drop a mount there. The path then leads back to host disk, and the
next credential the agent writes lands there. So put memory-only state at a
path of Brig's own, such as `/brig/<name>`, and point the agent there with
`env:`.

- Keep writing `path:`, `hostmount` entries and `files:` bindings relative to
  `guestHome`. Brig maps them to the same relative path under `at:`.
- A host-side rename of the home cannot move a `hostmount` under a relocated
  `tmpfs` either.
- On the `hvi` backend, `/brig` sits on a per-sandbox root that only the
  host user writes.
- Only `kind: tmpfs` takes `at:`. Brig refuses, at parse time, an `at:` that
  is not absolute and clean, that is `/`, that is `guestHome` or overlaps it,
  that sits in a system directory (`/proc`, `/sys`, `/dev`, `/run`, `/tmp`,
  `/etc`, `/usr`, `/bin`, `/sbin`, `/lib*`, `/boot`, `/var`) or in `/work`,
  where a project is mounted, or that overlaps another `tmpfs`'s `at:`. It
  also refuses two `tmpfs` volumes whose `path:` values nest when either one
  sets `at:`.
- `claude-code` mounts its `.claude` `tmpfs` at `/brig/claude` and sets
  `CLAUDE_CONFIG_DIR` to it. `claude-desktop` still covers `.claude` inside
  the guest home.

**`kind: hostmount`** names an exception under a `tmpfs`: one path that
keeps its host copy from the guest home across boots.

- Brig refuses `source:`, `size:` and `at:` on it.
- A `hostmount` that is not nested under a `tmpfs` is a parse error.

**`kind: volume`** is reserved for a named volume that several sandboxes
share. Brig refuses it as not yet supported.

Shipped profiles:

| profiles | what they declare | where state lives |
| --- | --- | --- |
| `claude-code`, `claude-desktop`, `herdr` | `volumes:` | In-guest state can live in memory and stay off host disk |
| `codex`, `cursor`, `gemini`, `grok`, `opencode`, `ubuntu` | No `volumes:` | Everything under `guestHome` persists to host disk, with nothing memory-only |

### State lost at `brig stop`

If an agent writes state under a `tmpfs`-covered path with no matching
`hostmount`, that state does not survive `brig stop`. The gap differs by
profile:

| profile | hostmounts under the `.claude` tmpfs | effect |
| --- | --- | --- |
| `claude-code` | Include `.claude/skills`, `.claude/plugins` and `.claude/.claude.json` | A `--skills` copy there survives a stop, and so does the agent's global state |
| `claude-desktop` | Stop at `settings.json`, `CLAUDE.md`, `sessions`, `projects`, `plugins` and `history.jsonl` | `.claude/skills` is not among them, so anything the bundled desktop app writes there is lost at shutdown |

## `onboarding`

`onboarding:` seeds non-secret flags into the agent's own state file, so the
agent does not stop on a first-run screen that the guest cannot answer, such
as a login choice that opens a browser:

```yaml
onboarding:
  file: .claude/.claude.json
  seed:
    hasCompletedOnboarding: true
    hasTrustDialogAccepted: true
  trustKey: [projects, hasTrustDialogAccepted]
```

- Brig writes `seed` only when `file` does not exist. It never overwrites the
  agent's own file.
- `trustKey` names the two JSON levels around a directory name, for an agent
  that records trust per directory. Brig sets it for the directory that each
  run starts in, resolved to the git repository root.
- Brig never seeds anything that contains a credential.
- An older `claude-code` workspace has `.claude.json` at its root. Brig moves
  it into `.claude` once, before boot, and never over a file already there.

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
it warns on every boot, and the boot continues. See
[security.md](security.md).

## `genericBoot`

`genericBoot: true` boots an ordinary OCI image, such as `ubuntu:latest`, that
has no kernel and no urunc metadata. Brig supplies the kernel and the
initrd.

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

Brig never modifies the image, and it runs as it does anywhere else. The
profile is the same on macOS and Linux.

On Linux it needs `nerdctl` and the urunc that the runtime bundle builds.
Brig refuses docker up front. See
[runtimes.md](runtimes.md#what-brig-requires-of-each).

The kernel and the initrd:

- They are host files. Brig never takes them from the image, so an image
  cannot nominate a file on your machine.
- The kernel is `Image` on arm64 and `bzImage` on x86_64. The initrd is
  `container-initrd`.
- The image needs no guest agent of its own. `brig sh` talks to the one in
  the initrd.
- The Linux runtime bundle carries them, so a bundle install fetches
  nothing.
- If the files are missing, Brig fetches them once, on the first `brig run` of
  a `genericBoot` profile. On Linux they go in
  `$XDG_DATA_HOME/brig/assets` (default `~/.local/share/brig/assets`), and
  the fetch needs `oras`. Without it, Brig says so and tells you what to
  fetch.

| variable | effect |
| --- | --- |
| `BRIG_BOOT_ASSETS` | Names the directory that holds the files, on both systems. It also turns the fetch off, so Brig never downloads a release bundle over a build you are working on |
| `BRIG_BOOT_ASSETS_REF` | Pins a specific bundle instead of the current one for your platform |

The bundle is the OCI artifact `ghcr.io/nofireai/hull-assets`, and it pulls
anonymously. See
[What you are installing](runtimes.md#what-you-are-installing).
`oras pull ghcr.io/nofireai/hull-assets:<os>-<arch> --output <dir>` is the
same fetch by hand.

## Building the image

Brig does not build the image for you.

- [bring-your-own-image.md](https://github.com/brig-sh/community-images/blob/main/docs/bring-your-own-image.md)
  documents how to build one.
- The images of the built-in profiles are open source at
  [brig-sh/community-images](https://github.com/brig-sh/community-images).
- [guest-image.md](guest-image.md) lists what the image must carry: every
  binary that Brig runs inside the guest, and the account that it expects.
  It also names a script that checks an image you built.
