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

Start from the closest existing profile:

```bash
brig agent new mine --from claude-code   # writes ~/.config/brig/mine.yaml
brig agent edit mine                     # change the image, and what it forwards
brig run mine
```

- The argument to `new` is a name. Brig refuses a destination that contains a
  `/`.
- Brig writes the file into your profile directory. A profile file has an
  effect only in that directory.
- `new` writes the name into the `name:` field of the file. Brig identifies a
  profile by that field and ignores the file name.
- `mine.yaml` says `name: mine`, and every command that takes a profile then
  takes `mine`.
- Nothing else in the file changes. The image, the guest home and the comments
  still describe the profile that you copied. Change them with
  `brig agent edit`.
- The file has a header comment that explains every field.

`brig agent edit` opens a file-backed profile in `$VISUAL`, then `$EDITOR`,
then `vi`. A built-in profile has no file. For a built-in profile, `edit`
prints the commands that create a copy and creates nothing.

To run only the built-in agents, read up to
[Removing a profile](#removing-a-profile). The sections after it are for a
profile of your own.

## Where profiles live

`brig run claude` needs no profile setup, because the eight profiles that Brig
ships are embedded in the binary.

Your own profiles live as one file per profile, with no subdirectories:

| setting | profile directory |
| --- | --- |
| Default | `~/.config/brig`, for example `~/.config/brig/claude-code.yaml` |
| `XDG_CONFIG_HOME` | `$XDG_CONFIG_HOME/brig` |
| `BRIG_PROFILE_DIR` | The directory that it names. It overrides the location |

Brig follows the
[XDG Base Directory Specification, version 0.8](https://specifications.freedesktop.org/basedir/latest/).
An empty `XDG_CONFIG_HOME` counts as unset. Brig ignores a relative
`XDG_CONFIG_HOME` as invalid.

The directory starts empty, and Brig writes to it only when you ask. Only
these commands write to it:

- `brig agent import`
- `brig agent export <agent> <name>`
- `brig agent new <name> --from <agent>`

`export` and `new` refuse to overwrite an existing file unless you pass
`--force`.

A file can take the name of a built-in profile. Use this to pin your own image
for a profile that Brig already knows, without a second name. `brig agent ls`
lists embedded and file-backed profiles together in one namespace, and marks
the origin of each:

| mark | origin |
| --- | --- |
| No mark | Embedded |
| `(file)` | A profile that exists only as a file |
| `(file, overrides built-in)` | A file that shadows an embedded profile |

More than one file can declare the same profile name, because a file need not
be named after the profile inside it. Two files with one name is a mistake:

- The file that wins depends on how the file names sort.
- Brig reports the collision and says which file won.
- `brig agent rm` removes all of the files, after it asks.

<details>
<summary>Locations from older versions</summary>

- The older `BRIG_TEMPLATE_DIR` works until v0.5.0.
- Brig does not read the old `~/.config/brig/templates` default, and it
  migrates nothing. If files remain there, Brig says so on every invocation.
  Move them across with `brig agent import`.

</details>

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

- "example profile" is the profile's own `desc:`, and `brig agent ls` prints
  it. `claude-code` and `codex` have no such marker.
- `claude-desktop` is the one shipped `kind: gui` profile, and `ubuntu` is the
  one shipped `kind: shell` profile. The other six are `kind: agent`, the
  default. See [`kind`](#kind).
- The table states what each profile declares. It does not say whether the
  registry serves the image today.

Seven of the eight profiles name an image that Brig expects you can pull.
`cursor` declares `unpublished: true`:

- `brig run` refuses it before it reaches the registry.
- `brig agent ls` appends `(no published image)`.
- To run it, pass your own `--image`.

Signature verification depends on the registry
([security.md](security.md)):

| profiles | registry | signature |
| --- | --- | --- |
| `claude-code`, `codex`, `gemini`, `grok`, `opencode` | `ghcr.io/brig-sh`, Brig's own builds | Brig can verify a signature against this registry |
| `claude-desktop` | `ghcr.io/nofireai/` | Brig cannot verify the signature and warns on every boot. The image is single-architecture, `aarch64` only |
| `ubuntu` | Docker Hub | Brig cannot verify the signature and warns on every boot |

Each profile sets the network default for a new sandbox. See
[Network postures](policies.md#network-postures).

| case | network |
| --- | --- |
| The six shipped `hvi` profiles | `network: isolated` |
| `claude-desktop` | `network: shared`, because its GUI requires `vz` |
| `cursor` | Unset. Linux and `hvi` isolate. `vz` and `qemu` fall back to `shared` |
| An existing session | Keeps its recorded posture |
| An older session with no record | Keeps the actual posture, inspected from the runtime |

## Export and import

```bash
brig agent export claude-code                # prints to stdout
brig agent export claude-code mine           # ~/.config/brig/mine.yaml
brig agent export claude-code mine --force   # ...overwriting what is there
brig agent export claude-code > ./mine.yaml  # a copy somewhere of your own
brig agent export x | brig agent import -
```

- With no destination, export prints to stdout.
- With a destination, export writes the file as Brig ships it, comments
  included.
- Import stores your bytes as written, so your comments and your ordering
  survive.
- Export writes YAML, which allows comments. If a program reads the profile,
  use `brig agent show --json`.
- JSON is a subset of YAML, so import reads both with the same parser.

Limits on the destination:

- **The destination is a name, never a path.** Brig writes only to the profile
  directory. It refuses a path, or a typo that looks like one. For a copy
  somewhere else, export to stdout and redirect it.
- **Brig refuses a name that is already in use**, because the destination is
  the name written into the file. `claude` is the alias of `claude-code`, and
  a profile named `claude` wins the lookup over that alias. After
  `brig agent new claude --from claude-code`, every `brig run claude` means
  the copy. The built-in is then reachable only under its full name.
- **Brig refuses a name that a `reserved:` profile owns**, for the same
  reason.

Both refusals say what the collision is. Pick another name.

An exported built-in also carries its explicit `network:` choice. If you copy
a profile that names `isolated` and change its backend to `vz` or `qemu`,
change the network to `shared` too. Those backends cannot enforce isolation,
and Brig refuses an explicit request for it.

## `kind`

`kind` sets what `brig run` does when the guest is up. It takes three values:

| value | what `brig run` does | limits |
| --- | --- | --- |
| `kind: agent`, the default | Execs `binary:` and passes your trailing arguments to it | `binary:` is required |
| `kind: shell` | Opens a login shell. Trailing words run as one command instead, each word one argument to it, or as a script after `-c` | `binary:` is not required and not read |
| `kind: gui` | Only starts the sandbox, which boots with a graphical console. There is nothing to attach to. `guiTitle:` names the window | Brig refuses a trailing argument. Only the `vz` hypervisor backend shows a console, so Brig refuses to boot a `kind: gui` profile on `hvi` or `qemu` |

- `brig sh` always execs `bash -l`, or runs the command or script under
  `bash -lc`. It never runs `binary:`. `ubuntu` sets `binary: bash` anyway,
  which documents the shell, and Brig does not act on it.
- `brig agent ls` lists `claude-desktop` and `ubuntu` with the other six
  profiles, but a run of one opens a window or a shell.
- A `kind: shell` or `kind: gui` profile cannot carry `policy:`. Neither has
  an agent process to hook an egress rule into, so Brig refuses the profile at
  parse time.
- The older `shell:` and `gui:` booleans still parse, and Brig folds them into
  `kind:` when it reads the file. See
  [migration.md](migration.md#profile-keys).

## `brig info`

`brig info <profile>` reports what a run hands to the guest. It prints names
only and never a value, on any path.

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

- A variable sourced from the secret store has the annotation `(secret)`. An
  ambient or literal variable has none.
- The `...` stands for the envelope block that comes first (`PROFILE`,
  `SANDBOX`, `WORKSPACE` and the rest).
- The example gives the shape of the output. No test pins the wording.
- Brig builds the report from the list of names alone. A test fails the build
  if a value reaches the output.
- `brig info` resolves secrets the same way that a run does. A profile that
  misses a required secret fails here as it does under `brig run`.

`BRIG_ENV_ARGV=1` still puts an ordinary forwarded variable on the runtime's
own command line. It is for a runtime build that does not take a bare
`--env KEY`. It has no effect on a value bound from the secret store, because
the host durably logs the argv of every exec. On a runtime build that needs
`BRIG_ENV_ARGV=1`, such a credential does not arrive at all, so it never
reaches the log.

## `reserved`

`reserved: true` stops a session name from landing on the guest home that
another profile owns.

Brig names the guest home of a session after the agent and the label, joined
by a dash. So the pair can spell the name of another profile. `claude-desktop`
owns the guest home of the Desktop app and sets `reserved: true`. Brig then
refuses a session whose agent and label join to `claude-desktop`.

- `brig run claude@desktop` is not such a session. `claude` is an alias of
  `claude-code`, so its guest home is `~/.brig/homes/brig-claude-code-desktop`,
  which no profile owns.
- A profile of your own named `claude` wins the lookup over the alias. Brig
  then refuses `claude@desktop`.
- The trailing word is reserved for profile names too. `brig agent import`
  refuses a profile named `desktop`.
- A profile of your own named `my-codex` with `reserved: true` reserves
  `codex` in the same way. Brig then refuses the import of a profile named
  `codex` as a collision. That includes a profile meant to override the
  built-in.

## `deny` is the billing guard

`deny` lists the variables that Brig does not forward. Brig says why when it
holds one back.

Use it for a provider variable that outranks the credential that you want the
sandbox to use. `ANTHROPIC_API_KEY` beats the subscription credential of
Claude Code in its precedence. If Brig forwards it, the sandbox moves off your
subscription and onto metered API billing without a notice.

`BRIG_ALLOW_DENIED=1` is the override for a user who wants metered billing.

Limits:

- `deny` guards only the environment channel.
- Brig does not check a `files:` binding against it, and no name check can. A
  profile can deliver a metered key inside a `settings.json`, and nothing sees
  it.
- `deny` exists to catch an accident: an ambient variable that Brig forwards
  because it was in your shell. A file binding takes an explicit stored secret
  and an explicit binding that you wrote.
- The guard applies the same way when the value arrives by `ref:` and not
  straight from the environment. See
  [`secrets` and `env`](#secrets-and-env-for-a-credential-brig-resolves-itself).

## Removing a profile

```bash
brig agent rm mytool
```

The argument is a profile name. `rm` resolves it through the merged set, so it
takes aliases and finds the file under any file name. If more than one file
declares the name, `rm` removes all of them. Otherwise the other file loads,
and the profile stays listed.

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
names the `brig rm <ref>` for each sandbox.

- When the file is gone, `brig rm <ref>` cannot reach those sandboxes. The way
  out is then `brig rm --all`, or a new profile of the same name.
- `rm` does not refuse an override of a built-in, because the name still
  resolves to the built-in.

To find those sandboxes, `rm` asks the runtime of the profile what it holds:

| case | what `rm` does |
| --- | --- |
| Brig has a recorded session of the profile | Asks the profile's `runtimeBin` |
| Brig has no recorded session | Asks the runtime on `PATH`. So `rm` never executes the `runtimeBin` of a profile that you imported and never ran |
| No runtime is installed | Removes the file, because there are no sandboxes |
| Brig cannot ask | Keeps the file and names the cause. Fix that setting first |

Two examples of a cause: `BRIG_RUNTIME` set to a runtime that Brig does not
know, or a `runtimeBin` that points at nothing on a profile that Brig has run.

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
| `forward` | no | Deprecated. It still works, and Brig folds it into an equivalent `env` binding when it reads the file. See [migration.md](migration.md#profile-keys) |
| `files` | no | Credential files the guest sees: which stored secret fills each one, and where under `guestHome` it is written. See [`files`](#files) |
| `volumes` | no | What is mounted inside `guestHome`, or at a fixed guest path with `at:`: a `tmpfs` whose contents never reach host disk, how big it can grow, and the `hostmount` exceptions kept across boots. See [`volumes`](#volumes) |
| `deny` | no | Variables Brig does not forward unless `BRIG_ALLOW_DENIED=1` is set. Brig refuses a profile that also binds one in `env` or `forward`. See [`deny`](#deny-is-the-billing-guard) |
| `statePaths` | no | Deprecated, superseded by `volumes:`. It still parses. If you declare it alongside `volumes:`, Brig reports an error and does not merge them. See [migration.md](migration.md#profile-keys) |
| `staleCredentialFiles` | no | Paths an older wrapper used to write a credential into. Brig never does. It warns when it finds one and does not delete it |
| `headless` | no | The agent supports a non-interactive run |
| `guiTitle` | no | Window title, for a `kind: gui` profile |
| `network` | no | The network posture of the sandbox: `shared` (one network for sandboxes using it), `isolated` (a network for this sandbox alone) or `offline` (no route out at all). A new sandbox defaults to `isolated`. When no posture is named, hull's `vz` and `qemu` backends fall back to `shared` and report it. Brig refuses an explicit `isolated` on `vz` and `qemu`. An existing sandbox keeps its recorded or runtime-inspected posture. `BRIG_NETWORK`, `--network` and `--offline` win over both |
| `hypervisor` | no | macOS backend to boot on: `vz` (the default when the field is absent, and the only one with a graphical console), `hvi` or `qemu`. Six of the eight shipped profiles say `hvi`. `BRIG_HYPERVISOR` wins over it. Ignored on Linux, where the shim decides |
| `runtimeBin` | no | The runtime binary to drive instead of the one on `PATH`, `~` expanded. It describes your machine and not the workload, so it is of no use to a person you share the profile with. Use it to pin a profile to a build you are working on without exporting a variable in every shell. `BRIG_RUNTIME_BIN` wins over it |
| `rootfsType` | no | How the guest root reaches the microVM: `block`, `virtiofs` or `9pfs`. Left unset, the runtime picks its own default, which suits a profile that only runs an agent. Set `block` when the sandbox installs packages and needs a writable disk instead of a share sized to the image |
| `genericBoot` | no | The image was never built to be a guest: a plain OCI image with no kernel and no urunc metadata. The runtime supplies the kernel and initrd and boots it unmodified, on macOS and Linux alike. See [`genericBoot`](#genericboot) |
| `hostConfigDir`, `projectPaths` | no | Where the user's own agent configuration lives on the host, and which subdirectories of it to seed into the guest home. Brig seeds them only when the run passes `--skills` or sets `BRIG_SKILLS=1`. Both must be set for either to take effect. Only `claude-code` declares them, so `--skills` does nothing on the other seven |
| `onboarding` | no | A first-run state file to seed. See [`onboarding`](#onboarding) |
| `reserved` | no | Marks a profile that owns the guest home a session name can otherwise slug onto. See [`reserved`](#reserved) |
| `unpublished` | no | Brig ships the profile but no image for it. `brig run` says so and stops before the pull, which otherwise fails with a registry 404. Pass `--image` with one you built. `brig agent ls` marks the profile, and `cursor` is the one that carries it |
| `policy` | no | Names of policies attached to this profile inline: every run carries all of them, unioned with whatever is attached separately by name |

Brig refuses a misspelled field, such as `forwards:` in place of `forward:`.

`brig agent show --json` always prints the `env:` form, even for a profile
written with `forward:`. JSON export marshals the parsed profile, and the
parser folds `forward:` into `env:`. Plain YAML export gives back the file as
written.

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
  complete its own login inside the sandbox.
- `sources:` makes a profile portable: the same entry names the macOS keychain
  and the Linux file.
- A secret with no `sources:` is hand-created. Its message names
  `brig secret create` instead of `brig secret import`.

Three `from:` values exist, and each takes one locator and no other. For
example, a `keychain` source that carries a `path:` is a parse error.

| `from:` | locator | what it reads |
| --- | --- | --- |
| `keychain` | `service:` | a macOS keychain generic-password item. macOS only: the Linux store is a Secret Service keyring ([secrets.md](secrets.md#linux)), and no source reads from it |
| `file` | `path:` | a host file, verbatim. A leading `~` is expanded when it is read, so a profile carries no one host's home directory |
| `env` | `var:` | a host environment variable, **copied once at import** |

`from: env` and `ref: env.<name>` share a word and behave differently:

- Brig reads a `ref:` on every run.
- A `from: env` source copies the value into the store when you run
  `brig secret import`, and Brig never reads the variable again.
- For a value that expires, prefer a [`refs:` chain](#ref-and-refs). The
  shipped `claude-code` uses no `from: env` source for this reason.

Limits of `field:` and `expiryField:`:

- They sit on the secret and not on a source. So every source for one secret
  must yield the same document shape.
- An absent `field:` stores the value verbatim, which suits a file-shaped
  secret. The host keychain blob is the format that the agent's credentials
  file takes. So Brig extracts nothing, and no field that Brig does not
  understand is lost.
- A credential whose two host locations differ in shape needs two secrets. A
  `files:` binding can name only one secret, and Brig refuses two bindings on
  one `path:`. So a single portable profile cannot express an agent whose
  credential file differs per platform today. The two locations of Claude
  share a shape.

Source order in the example:

| host | what import reads |
| --- | --- |
| macOS | The keychain source first. It reads `path: ~/.claude/.credentials.json` only when the keychain item does not exist |
| Linux with a keyring | `~/.claude/.credentials.json`, the documented Linux location. Import stores it into the Secret Service backend ([secrets.md](secrets.md#linux)) |

### A missing secret

If a required secret is missing, the run fails before Brig creates a sandbox.
The error names the secret, the sandbox that needed it, and the command that
creates it:

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

Brig never reduces a missing required secret to a warning or a skipped
binding.

A missing optional secret is a warning, and the run boots. The warning names
what Brig did not find and the command that supplies it:

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
| `ref: env.<name>` | The named variable, out of Brig's own environment. `forward:` reads from the same place |

Limits:

- A namespace that is neither `secrets` nor `env` is a parse error, and the
  error names the two that exist.
- `ref:` is a `refs:` of length one. A binding carries one spelling or the
  other, and never both.
- A `secrets.<name>` ref whose name is absent from `secrets:` is a parse
  error.
- For a sandbox that `brigd` serves, an `env.<name>` ref resolves against the
  environment of the daemon. It does not read the shell that sent the request.

The order in a chain matters:

- Brig drops a name that a profile binds from the ambient forward. With a bare
  `ref: secrets.gh-token`, `GH_TOKEN=$(gh auth token) brig run claude-code`
  stops reaching the guest, and `BRIG_FORWARD_ENV` cannot restore it.
- A chain lets a profile add a store fallback and keep the shell override. The
  shipped `claude-code` binds `GH_TOKEN` this way.
- If an earlier `env.` element resolves, the run does not need the later
  `secrets.` element. A run that the environment already satisfies never opens
  the store.

### `value`

`value:` sets the guest variable to a literal. Use it for configuration that
is not a credential, such as `EDITOR: vi` in the first example. An entry has
one of `value:`, `ref:` or `refs:`, and Brig refuses any other combination.

### `HOME`, `PATH`, `TMPDIR` and `XDG_*`

A binding for one of these names takes `value:` or `ref: env.<name>`. The
runtime reads these names for itself, so Brig passes their guest values on the
runtime's command line as `NAME=value`. See
[security.md](security.md#not-in-argv).

Brig refuses a binding for one of them that resolves from the secret store. It
does so when the sandbox boots or execs, because a stored secret never goes on
the command line.

### `BRIG_FORWARD_ENV`

`BRIG_FORWARD_ENV` overrides which variables Brig carries in from its own
environment. It replaces the profile's `ref: env.<name>` bindings, and only
those. A `ref: secrets.<name>` binding is the profile's own declaration of
what the workload needs, and it survives the override.

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
variable does not have:

- It stays out of `/proc/<pid>/environ`.
- The processes that the agent spawns do not inherit it.
- Brig can rewrite it under a running agent, so a rotated secret can reach a
  live session.

The `env:` channel stays supported. `GEMINI_API_KEY`, `XAI_API_KEY`,
`OPENROUTER_API_KEY` and `CURSOR_API_KEY` are env-only by the design of their
agents, so nothing reads a file for them. You can bind one secret through both
channels, and the exposure is then the union of the two. Do it only when
something reads both.

The parser enforces four rules:

- **`ref:` only, never `refs:`.** A chain gives one environment variable a
  shell override and a store fallback, and a file has no shell to override it.
  Brig refuses `env.<name>` as well, because a file binding puts a stored
  credential where an agent reads one.
- **`mode:` is a quoted string.** YAML reads an unquoted `0644` as the number
  420, so Brig refuses a mode that is not quoted.
- **The target must sit inside a `tmpfs` volume**, and a `hostmount` under
  that volume must not carve it back out. Brig refuses this at parse time,
  before any token can reach your disk.
- **One binding per `path:`.** A file has one source.

Other limits:

- `field:` on a file-bound secret is legal and almost always wrong. It writes
  a bare token where the agent expects a document. The agent then attempts a
  refresh, fails, and prompts. Leave `field:` off unless the agent reads a
  one-line file.
- An unresolved binding leaves nothing behind: no file, no mount, no empty
  target.
- There is no `brig secret push` yet to rotate a file binding in a running
  sandbox. A re-run does it: it rewrites the file under the live agent.

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

Order in the file does not matter. Brig mounts parents before children, by
path depth.

**`kind: tmpfs`** covers a directory, so nothing written under it can reach
your disk. That makes a `files:` binding safe by construction: there is no
path from the credential to the host.

- Brig still verifies it. The covered path must read as `tmpfs`, and
  `/proc/swaps` must be empty. Otherwise the run stops before it hands over a
  credential.
- `size:` sets how big the `tmpfs` can grow. The default is `64m`.
- The guest meets that ceiling as `ENOSPC` from its own tools, and nothing on
  the host watches for it.
- Size it for what the mount holds. An agent's home, where edit history and
  per-job scratch accumulate, needs more than a directory of configuration.
- Brig refuses, at parse time, a `size:` that is not a number optionally
  followed by `k`, `m` or `g`.

**`at:`** mounts a `tmpfs` at an absolute guest path instead of at
`guestHome/path`. The guest home is a share from the host. A mount whose mount
point is a directory on that share can be dropped by the guest kernel when the
host renames or replaces that directory, for example with `git clean -fdx`.
The path then leads back to host disk, and the next credential the agent
writes lands there. Memory-only state belongs off the share, so put it at a
path of Brig's own, such as `/brig/<name>`, and point the agent there with
`env:`.

- `path:` stays the name everything else uses. A `hostmount` under it, a
  `files:` binding and the checks Brig runs are all written relative to
  `guestHome`, and Brig translates them to the same relative path under
  `at:`.
- A `hostmount` under a relocated `tmpfs` is bound from its path in the guest
  home to the same relative path under `at:`. Its mount point is on the
  `tmpfs`, not on a share, so a host-side rename of the home cannot move it
  either.
- On hull's `hvi` backend, `/brig` sits on hull's own per-instance root,
  which only hull and the host user write.
- Only `kind: tmpfs` takes `at:`. Brig refuses, at parse time, an `at:` that
  is not absolute and clean, that is `/`, that is `guestHome` or overlaps it,
  that sits in a system directory (`/proc`, `/sys`, `/dev`, `/run`, `/tmp`,
  `/etc`, `/usr`, `/bin`, `/sbin`, `/lib*`, `/boot`, `/var`) or in `/work`,
  where a project is mounted, or that overlaps another `tmpfs`'s `at:`. It
  also refuses two `tmpfs` volumes whose `path:` values nest when either one
  sets `at:`.
- `claude-code` mounts its `.claude` `tmpfs` at `/brig/claude` and sets
  `CLAUDE_CONFIG_DIR` to it. `claude-desktop` still covers `.claude` inside
  the guest home, because its bundled binary has not been checked for
  `CLAUDE_CONFIG_DIR`.

**`kind: hostmount`** names an exception: one path bound back out to the same
path in the guest home, where that state already lives.

- Its source is implicit, so Brig refuses `source:` on it.
- A `hostmount` that is not nested under a `tmpfs` is a parse error. The guest
  home is already `guestHome`, so such an entry mounts a path onto itself.
- It takes no `size:` and no `at:`. A `hostmount` is as large as the guest
  home that it comes from, and it goes wherever the `tmpfs` above it goes.

**`kind: volume`** is reserved for a named volume that several sandboxes
share. Brig parses it and refuses it as not yet supported.

Shipped profiles:

| profiles | what they declare | where state lives |
| --- | --- | --- |
| `claude-code`, `claude-desktop` | `volumes:` | In-guest state can live in memory and stay off host disk |
| `codex`, `cursor`, `gemini`, `grok`, `opencode` | Only the deprecated `statePaths:` | Everything under `guestHome` persists to host disk, with nothing memory-only |
| `ubuntu` | Neither | Everything under `guestHome` persists to host disk, with nothing memory-only |

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
agent does not stop on a first-run screen:

```yaml
onboarding:
  file: .claude/.claude.json
  seed:
    hasCompletedOnboarding: true
    hasTrustDialogAccepted: true
  trustKey: [projects, hasTrustDialogAccepted]
```

Some agents ask a question on first run that is not authentication and that
the guest cannot answer. For example, a choice of login method there opens a
browser, and the microVM has none.

- Brig writes `seed` only when `file` does not exist. An existing file belongs
  to the agent, and Brig does not overwrite it.
- `trustKey` names the two JSON levels around a directory name, for an agent
  that records trust per directory.
- Brig sets `trustKey` for the directory that each run starts in, resolved to
  the git repository root as the guest sees it.
- Brig never seeds anything that contains a credential.
- `claude-code` keeps this file at `.claude/.claude.json`, where
  `CLAUDE_CONFIG_DIR` has the agent read it. A workspace made before that
  still has `.claude.json` at its root. Brig moves it into `.claude` once,
  before boot, and never over a file already there.

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

Brig cannot verify the signature of an image outside `ghcr.io/brig-sh`, so it
says so on every boot. That is a warning, and the boot continues. See
[security.md](security.md).

## `genericBoot`

`genericBoot: true` boots an ordinary OCI image, such as `ubuntu:latest`, that
has no kernel and no urunc metadata. The runtime supplies the kernel and the
initrd. A guest image normally carries both the kernel and that metadata.

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

Brig never modifies the image. The image's own argv, environment, hostname and
mounts are restored before the guest switches root, so it runs as it does
anywhere else.

The profile is the same on both operating systems, because the kernel and the
initrd travel as two OCI annotations:

| | macOS | Linux |
| --- | --- | --- |
| Who reads the annotations | hull takes them on its command line | urunc reads them from the container's OCI spec, which nerdctl passes through |
| Where the kernel and initrd are | Under hull's store. Brig asks hull for the location | `~/.local/share/brig/assets` |
| Who fetches missing files | hull. It downloads the same bundle for its own `hull run`, and already knows the reference, the directory and the registry credentials | Brig shells out to `oras`, because hull does not build on Linux. If `oras` is not installed, Brig says so and tells you what to fetch |

Limits on Linux:

- It needs the urunc that the runtime bundle builds, because no urunc release
  reads the pair
  ([runtimes.md](runtimes.md#what-brig-requires-of-each)).
- It needs `nerdctl`. Docker does not carry OCI annotations through to the
  runtime, so a sandbox booted through it has no kernel. Brig refuses up
  front.

The kernel and the initrd:

- They are host files. Brig never takes them from image metadata, so an image
  cannot nominate a file on your machine.
- The kernel is named for the architecture that it boots: `Image` on arm64,
  `bzImage` on x86_64.
- The guest agent that `brig sh` talks to comes from that initrd and not from
  the image. That is how Brig can drive an unmodified image.
- If the files are missing, Brig fetches them once, on the first `brig run` of
  a `genericBoot` profile.

| variable | effect |
| --- | --- |
| `BRIG_BOOT_ASSETS` | Overrides both locations. It also turns the fetch off, so Brig never downloads a release bundle over a build you are working on |
| `BRIG_BOOT_ASSETS_REF` | Pins a specific bundle instead of the current one for your platform |

The bundle is published as the OCI artifact `ghcr.io/nofireai/hull-assets`,
one tag per guest platform, and pulls anonymously. The repository that builds
it is not public. `oras pull ghcr.io/nofireai/hull-assets:<os>-<arch>
--output <dir>` is the same fetch by hand.

## Building the image

Brig does not build the image for you.

- [bring-your-own-image.md](https://github.com/brig-sh/community-images/blob/main/docs/bring-your-own-image.md)
  documents how to build one.
- The images of the built-in profiles are open source at
  [brig-sh/community-images](https://github.com/brig-sh/community-images).
- [guest-image.md](guest-image.md) is the full contract: every binary that
  Brig execs inside the guest, and the account that it expects. It also names
  a script that checks an image you built against the contract.
