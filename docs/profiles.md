# Writing an agent profile

brig boots an agent from an OCI image. Any Linux CLI that runs in one already
works, so a profile is not a requirement -- it just saves you spelling out
the image, the guest home and the credential variables on every invocation.

The quickest way to write one is to start from the closest existing profile
rather than from a blank file:

```bash
brig profile export claude-code mine   # writes ~/.config/brig/mine.yaml
brig profile edit mine                 # change name: to mine, and the image
brig run mine
```

The second word is a *name*, not a path: brig puts the file in your profile
directory, which is the only place a profile file does anything. It writes
nowhere else -- a destination with a `/` in it is refused, not honoured.

Note that brig keys on the `name:` field inside the file, not on the file
name, so a freshly exported `mine.yaml` still says `name: claude-code` and is
still the claude-code profile until you change that line. Export says so when
the two disagree.

The exported file carries a header explaining every field, so you can edit it
without coming back here. Once you have a profile of your own on disk,
`brig profile edit` opens it directly in `$VISUAL`, then `$EDITOR`, then `vi`
-- there is no round trip through a separate file any more. It only works on
a profile backed by a file: a built-in has none, so it says so and prints the
commands that would make one, and creates nothing itself.

## Where profiles live

The eight profiles brig ships are embedded in the binary, so brig works with
no setup at all -- there is nothing to install before `brig run claude` boots
a sandbox.

Your own live as one file per profile in `$XDG_CONFIG_HOME/brig`, default
`~/.config/brig`, flat: `~/.config/brig/claude-code.yaml`. `BRIG_PROFILE_DIR`
overrides the location outright; the older `BRIG_TEMPLATE_DIR` is still
honoured for one release. This follows the
[XDG Base Directory Specification, version 0.8](https://specifications.freedesktop.org/basedir/latest/):
an empty `XDG_CONFIG_HOME` counts as unset, and a *relative* one is ignored as
invalid -- which matters here because brig runs from whatever project
directory you are in, so honouring a relative value would resolve profiles
against the current directory and quietly give you a different set per
project. The old `~/.config/brig/templates` default is not read. Nothing is
migrated for you -- these files name credential variables, and there is no
safe guess about which of them you still want -- but brig notices files left
there and says so on every invocation until you move them across with
`brig profile import`.

**The directory starts empty, and brig never writes there unless you ask
it to.** Nothing is pre-seeded on first run. `brig profile import` and
`brig profile export <name> <dest>` are the only commands that write to it,
and export refuses to overwrite an existing file unless you pass `--force`.

A profile may be backed by more than one file, because a file need not be
named after the profile inside it. Two files declaring one name is a mistake
with no winner worth having -- which survives depends on where the names
happen to sort -- so brig reports it and says which one won.
`brig profile rm` takes all of them.

A file may take a built-in's name. That is deliberate: it is how you pin your
own image for a profile brig already knows about, without inventing a second
name for it. `brig profiles` lists the merged set -- embedded and file-backed
together, one namespace -- and marks where each came from: unmarked is
embedded, `(file)` is a profile that exists only as a file, and
`(file, overrides built-in)` is a file shadowing an embedded one.

## Export and import

```bash
brig profile export claude-code                # prints to stdout
brig profile export claude-code mine           # ~/.config/brig/mine.yaml
brig profile export claude-code mine --force   # ...overwriting what is there
brig profile export claude-code > ./mine.yaml  # a copy somewhere of your own
brig profile export x | brig profile import -
```

With no destination, export prints to stdout, so it composes into a pipe.
With one, it writes the file -- comments and all, exactly as brig ships it,
not a re-marshalled struct with the explanations stripped. Import stores your
bytes exactly as written too, so whichever way the file reached you, your
comments and your ordering survive.

**The destination is a name, never a path.** brig writes the profile directory
and nowhere else, so a path -- or a typo that looks like one -- is refused
rather than honoured. Wanting a copy elsewhere already has a spelling: export
to stdout and redirect it, under the shell's rules rather than brig's.

Export writes YAML because a profile is a file a person edits, and YAML has
comments. Use `brig profile export --json` if something downstream consumes
profiles programmatically -- JSON is a subset of YAML, so import reads both
with the same parser and neither has to guess.

## The fields

| field | required | what it is |
| --- | --- | --- |
| `name` | yes | The profile name. It becomes the workspace directory and the sandbox name, so it is restricted to lowercase letters, digits, dot, dash and underscore |
| `image` | yes | The guest image to boot |
| `guestHome` | yes | Absolute path where the workspace is mounted. The agent's state lands here, which is what makes the workspace the unit of persistence |
| `kind` | no | `agent` (the default), `shell` or `gui`. An `agent` needs a `binary`; the other two have nothing to pass arguments to |
| `binary` | yes, for `kind: agent` | The agent CLI inside the guest |
| `mem`, `cpus` | yes | Guest size. Both must be greater than zero |
| `desc` | no | One line, shown by `brig profiles` |
| `secrets` | no | Names this profile wants out of brig's own secret store, checked before the sandbox is created. Each entry says whether a run without it should stop, and where `brig secret import` may find it. See below |
| `env` | no | The variables the guest sees, and where each one's value comes from: a literal, a stored secret, or brig's own environment. See below |
| `forward` | no | Deprecated spelling of `env` for the environment case. Still works; folded into `env` when the file is read. See below |
| `files` | no | Credential files the guest sees: which stored secret fills each one, and where under `guestHome` it is written. See below |
| `volumes` | no | What is mounted inside `guestHome`: a `tmpfs` that nothing written to can reach host disk, and the `hostmount` exceptions kept across boots. See below |
| `deny` | no | Variables never bound, whatever `env` or `forward` says. See below |
| `statePaths` | no | Deprecated: `volumes` says the same thing and is acted on. Still read, so an older file keeps parsing; declaring both is an error rather than a merge |
| `staleCredentialFiles` | no | Paths an older wrapper used to write a credential into. brig never does, so finding one is worth a warning rather than a deletion |
| `headless` | no | The agent supports a non-interactive run |
| `guiTitle` | no | Window title, for a `kind: gui` profile |
| `hypervisor` | no | macOS backend to boot on: `vz` (the default, and the only one with a graphical console), `hvi` or `qemu`. `BRIG_HYPERVISOR` wins over it. Ignored on Linux, where the shim decides |
| `runtimeBin` | no | The runtime binary to drive instead of the one on `PATH`, `~` expanded. Unlike every other field this is about your machine rather than the workload, so it does not travel usefully to anyone else -- it is how you pin a profile to a build you are working on without exporting a variable in every shell. `BRIG_RUNTIME_BIN` wins over it |
| `rootfsType` | no | How the guest root reaches the VM: `block`, `virtiofs` or `9pfs`. Left unset the runtime picks its own default, which is what a profile that only runs an agent wants. Set `block` when the sandbox installs packages and needs a real writable disk rather than a share sized to the image |
| `genericBoot` | no | The image was never built to be a guest -- a plain OCI image with no kernel and no urunc metadata. The runtime supplies the kernel and initrd and boots it unmodified, on macOS and Linux alike. See below |
| `hostConfigDir`, `projectPaths` | no | Where the user's own agent configuration lives on the host, and which subdirectories of it to seed into the workspace. Only when the run passes `--skills` or sets `BRIG_SKILLS=1`. See below |
| `onboarding` | no | A first-run state file to seed. See below |
| `hostCredential` | no | **Deprecated, removed next release.** A credential read from the host keychain on every run when the environment carries none. Replaced by `secrets` with `sources`, filled once by `brig secret import`. See below |
| `reserved` | no | Marks a profile that owns the workspace a session name could otherwise slug onto. See below |

A misspelled field is refused rather than ignored. `forwards:` instead of
`forward:` would otherwise decode into nothing, forward no credentials, and
look exactly like a broken sandbox.

## `secrets` and `env`, for a credential brig resolves itself

`secrets:` is what a profile requires out of brig's own secret store.
`env:` is where each variable the guest sees actually comes from -- a
literal, one of those secrets, or brig's own environment:

```yaml
name: alex
image: ghcr.io/brig-sh/claude-code:latest
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
separate is what lets brig check the whole list up front: one error names
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
    sources:                            # where `brig secret import` may look
      - from: keychain
        service: Claude Code-credentials
      - from: file
        path: ~/.claude/.credentials.json
        hint: run `claude` on the host once to log in
```

**`required:` decides whether the run stops.** Absent means required, which is
why a bare string keeps meaning what it always meant. `required: false` warns
and boots -- which is what the shipped `claude-code` does, because an agent
that can complete its own login inside the sandbox should not be prevented
from trying.

**`sources:` decides which command the error names.** They are tried in order
and the first that exists wins, which is what makes a profile portable without
a per-platform predicate: the same entry names the macOS keychain and the Linux
file, and brig never has to know which host maps to which. A secret with **no**
`sources:` is hand-created by definition, and its message names
`brig secret create` rather than `brig secret import`.

Three `from:` values exist, each taking exactly one locator -- a `keychain`
source carrying a `path:` is a parse error rather than something ignored, since
ignoring it reads as a working portable chain that resolves nothing:

| `from:` | locator | what it reads |
| --- | --- | --- |
| `keychain` | `service:` | a macOS keychain generic-password item. macOS only; a Linux backend is [#8](https://github.com/brig-sh/brig/issues/8) |
| `file` | `path:` | a host file, verbatim. A leading `~` is expanded when it is read, so a profile carries no one host's home directory |
| `env` | `var:` | a host environment variable, **copied once at import** |

That last row is the one to read twice. `from: env` and `ref: env.<name>` are
the same word with opposite temporal semantics: a `ref:` is read live on every
run, while a `from: env` source copies the value into the store when you type
`brig secret import` and never looks again. Prefer the `refs:` chain below for
anything that expires. The shipped `claude-code` uses no `from: env` source for
exactly this reason.

`field:` and `expiryField:` say how to read a secret's value and its expiry out
of whatever a source yields, and they sit on the **secret** rather than on a
source -- which is only possible because every source for one secret must yield
the same document shape. An absent `field:` stores the value verbatim, which is
what a file-shaped secret wants: the host keychain blob *is* the format the
agent's credentials file takes, so nothing is extracted and no field brig does
not understand is lost.

The boundary that follows from that is worth stating rather than leaving to be
discovered: **a credential whose two host locations differ in shape needs two
secrets** -- and a `files:` binding can name only one, with two bindings on one
`path:` refused. So an agent whose credential file differs per platform cannot
be expressed as a single portable profile today. Claude's two locations happen
to share a shape.

On macOS the keychain source answers first, so nothing in a supported
configuration today ever reaches `path: ~/.claude/.credentials.json`. That path
is the **documented** Linux location rather than an observed one, and a Linux
host has no store to import into yet ([#8](https://github.com/brig-sh/brig/issues/8)),
so it is data that costs three lines and makes the profile portable the day the
backend lands.

### What happens when a secret is missing

The run fails before any sandbox is created, and the error names the secret,
the sandbox it was needed for, and the command that creates it:

```
$ brig
brig: missing secret "gh_token" needed by the brig-alex sandbox -- create it first with: brig secret create gh_token
```

A profile missing more than one gets every name in the same error, not one
failed run per secret:

```
$ brig
brig: missing 2 secrets needed by the brig-alex sandbox -- create them first:
  brig secret create gh_token
  brig secret create npm_token
```

This is not a warning and not a skipped binding. A sandbox whose environment
was built without a credential it was told it needs is exactly what the
requirement list exists to prevent.

An **optional** secret is not that. It warns, names what it could not find and
the command that supplies it, and the run boots:

```
$ brig run claude-code
brig: no value for the secret "claude-credentials", and claude-code will run without it.
brig: To carry it in from your host: brig secret import claude-code
brig: run `claude` on the host once to log in
```

A secret with no `sources:` gets the other verb, because import cannot fill it:

```
brig: no value for the secret "gh-token", and claude-code will run without it.
brig: To supply one: brig secret create gh-token
```

There is no secret store on Linux yet. A **required** secret therefore fails
every run on the nerdctl backend, for the same reason and in the same way --
failing closed is correct, but it is worth knowing before your first run of
such a profile there rather than after. An **optional** one is silent there
instead of warning on every run: there is no store to create it in, so nothing
the user does on that host would change the outcome, and a warning about it
would be noise rather than information. That is why the shipped `claude-code`
still boots on Linux.

### The `ref` grammar, and `refs:` chains

`ref: secrets.<name>` reads the named secret out of brig's own store.
`ref: env.<name>` reads the named variable out of brig's own environment --
the same place `forward:` always read from. A namespace that is neither is a
parse error naming the two that exist.

`refs:` is a list of those, and the first that resolves wins. `ref:` **is** a
`refs:` of length one, so a binding carries one spelling or the other and never
both:

```yaml
env:
  - name: GH_TOKEN
    refs: [env.GH_TOKEN, secrets.gh-token]   # shell override first, store second
```

That order is not decoration. A name a profile binds is dropped from the
ambient forward, so a bare `ref: secrets.gh-token` would make
`GH_TOKEN=$(gh auth token) brig run claude-code` stop reaching the guest, and
`BRIG_FORWARD_ENV` could not restore it. The chain is what lets a profile add a
store fallback without taking the shell override away, and it is why the
shipped `claude-code` binds `GH_TOKEN` this way.

A chain also decides what a run has to read: if an earlier `env.` element
resolves, the later `secrets.` element is not needed, and a run the environment
already satisfies never opens the store at all -- so no keychain is touched for
a value the shell already supplied.

A `secrets.<name>` ref whose name is absent from `secrets:` is a parse error
too: the requirement list is what makes the up-front check complete, and a
ref that bypassed it would be a secret nothing ever checks for.

For a sandbox served by `brigd`, an `env.<name>` ref resolves against the
*daemon's* environment, not the shell that sent the request. That is how
forwarding has always worked through the daemon -- it is not new here -- but
a `ref:` is the first thing that puts the source in the file where you can
see it.

### `value:`, for a literal

`value:` sets the guest variable to a literal, for configuration that is not
a credential -- `EDITOR: vi` above, say. An entry has exactly one of
`value:` or `ref:`; both or neither is refused.

### The `forward:` migration

`forward: [GH_TOKEN]` still works, and still means exactly what it always
did: carry `GH_TOKEN` in from brig's own environment when it is set and
non-empty. It is translated into the equivalent `env:` binding when the file
is read, so this:

```yaml
forward:
  - GH_TOKEN
```

means exactly this:

```yaml
env:
  - name: GH_TOKEN
    ref: env.GH_TOKEN
```

The translation happens once, at parse time, so nothing downstream of it has
to know which spelling a given file used. One consequence worth knowing:
`brig profile export --json` emits the `env:` form even for a profile
written with `forward:`, because JSON export marshals the parsed profile,
and by then there is no `forward:` left in it. Plain YAML export is
unaffected -- it hands back the file exactly as written, comments and all,
because it never re-marshals it.

### `BRIG_FORWARD_ENV` replaces the env-sourced set, and only that set

`BRIG_FORWARD_ENV` still overrides which variables are carried in from
brig's own environment. Before bindings existed that was the entire
mechanism, so overriding it replaced everything a profile could forward. Now
a `ref: secrets.<name>` binding is the profile's own declaration of what the
workload needs, and it survives the override untouched.

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
`/proc/<pid>/environ`, is not inherited by the processes the agent spawns, and
can be rewritten under a running agent, so a rotated secret can reach a live
session. An environment variable can do none of those.

The env channel is **not** deprecated, and will not be: `GEMINI_API_KEY`,
`XAI_API_KEY`, `OPENROUTER_API_KEY` and `CURSOR_API_KEY` are env-only by their
agents' own design, and a file for them would be a file nothing reads. Binding
one secret through both channels is legal, and the exposure is the union of the
two -- so do it only when something genuinely reads both.

Four rules the parser enforces:

- **`ref:` only, never `refs:`.** A chain exists to give one environment
  *variable* a shell override and a store fallback, and a file has no shell to
  override it. `env.<name>` is refused as well: a file binding exists to put a
  *stored* credential where an agent reads one.
- **`mode:` is a quoted string.** YAML reads an unquoted `0600` as decimal
  600, which is `0o1130` -- a mode nobody meant and that nothing would report.
- **The target must sit inside a `tmpfs` volume**, and must not be carved back
  out by a `hostmount` under it. This is refused at parse time rather than at
  delivery, where it would be a live token already on your disk.
- **One binding per `path:`.** A file has one source.

`field:` on a file-bound secret is legal and almost always wrong: it writes a
bare token where the agent expects a document, the agent attempts a refresh,
fails, and prompts. Leave `field:` off unless the agent really does read a
one-line file.

An **unresolved** binding leaves nothing behind: no file, no mount, no empty
target. An empty file at a credential path is indistinguishable from a real
leak to whoever finds it, and it is a login prompt the agent cannot explain.

## `volumes`, for what reaches host disk

`volumes:` is what is mounted inside `guestHome`, one primitive per entry:

```yaml
volumes:
  - kind: tmpfs
    path: .claude               # memory-only: nothing written here reaches the host
  - kind: hostmount
    path: .claude/sessions      # ... except these, kept across boots
  - kind: hostmount
    path: .claude/projects
  - kind: hostmount
    path: .claude/history.jsonl
    file: true                  # the target is a file, not a directory
```

A `tmpfs` covers a directory so that nothing written under it can reach your
disk. That is what makes a `files:` binding safe: the safety is
**fail-closed by construction** rather than by inspection, because there is no
path from the credential to the host to check. brig still verifies it -- the
covered path must read as `tmpfs` and `/proc/swaps` must be empty, or the run
stops rather than handing over a credential.

A `hostmount` names an exception: one path bound back out to the same path in
the workspace, which is where that state already lives. Its source is implicit,
so `source:` is refused on it. A `hostmount` **not** nested under a `tmpfs` is
a parse error: the workspace is already `guestHome`, so such an entry mounts a
path onto itself and reads as protection that is not there.

Order in the file is taste. brig mounts parents before children by path depth,
because declaration order would be a trap -- a profile listing `.claude/sessions`
above `.claude` would otherwise mount the tmpfs over the hostmount it had just
made and silently lose the state it named.

`kind: volume` is reserved for a named volume several sandboxes share. It is
parsed and refused as not yet supported, so a profile written against it fails
with the reason it fails and lands unchanged when it is implemented.

### What this costs

**Anything a future agent version starts writing under a covered directory is
silently ephemeral until someone adds a `hostmount` for it.** That is real, and
it is the trade this design makes: the arrangement it replaced lost nothing by
default and leaked by default; this one leaks nothing by default and loses by
default. The second is the better failure, but it is a failure -- if the agent
you sandbox gains a new state directory, its state disappears at shutdown and
nothing says so.

The corollary for `--skills`: those host directories are copied into the
workspace, so a profile that covers the directory they land in needs a
`hostmount` for each, or the copy is hidden and the flag does nothing.

`volumes:` replaces `statePaths:`, which was reference-only documentation.
Declaring both is an error rather than a merge: two lists that can disagree
about what persists is worse than the duplication.

### What is not here yet

`brig secret push` -- rotating a file binding in a running sandbox -- is not
part of this. The mechanism is built (a re-run rewrites the file under a live
agent); the verb is not.

## `brig env`, to see what a run would send

`brig env <profile>` reports what the guest would be handed, by name --
never a value, on any path. A variable sourced from the secret store is
annotated `(secret)`; one from the deprecated `hostCredential:` is annotated
`(host)`; an ambient or literal one is reported bare:

```
$ brig env alex
brig: workspace /Users/alex/brig/alex (sandbox brig-alex)
brig: runtime hull (/opt/homebrew/bin/hull)
brig: image ghcr.io/brig-sh/claude-code:latest (pull missing)
brig: forwarding to guest:
brig:   GH_TOKEN(secret)
brig:   CI
brig:   EDITOR
brig: guest git over HTTPS: off (BRIG_GIT_CONFIG=1 to enable)
```

That is not incidental. The reporting name travels as its own parameter,
separate from the value, from the point a binding is resolved -- and the
report is built from that name list alone, so no path through this command
reads a value. A test fails the build if one ever reaches the output.

`brig env` resolves secrets the same way any other run does, so a profile
missing one from the store fails the same way `brig run` would. It is not a
preview that quietly skips what it cannot resolve.

`BRIG_ENV_ARGV=1` still puts an ordinary forwarded variable on the runtime's
own command line, for a runtime build that will not take a bare `--env KEY`.
It is deliberately inert for a value brig resolved on your behalf -- one bound
from the secret store, and the host credential too: the host durably logs
every exec's argv, and a debugging escape hatch is not worth turning into a
credential leak. So on a runtime build that needs the hatch, the credential
does not arrive at all rather than arriving in the log; that is the intended
trade.

## `reserved`, so a session name cannot land on the wrong workspace

`claude-desktop` owns the Desktop app's workspace, so `brig run claude --name
desktop` is refused rather than quietly landing a Claude Code session there.
That protection is `reserved: true` on the `claude-desktop` profile.

The trailing word is reserved too, not just the full name: `claude-desktop`
reserves `desktop` as well as `claude-desktop` itself. Watch for the
consequence -- it is easy to trip over by accident. A profile of your own
named `my-codex` with `reserved: true` reserves `codex` along with
`my-codex`, so `brig profile import` of anything else named `codex` is
refused as a collision, even though nothing on brig's side is called that.

## `deny` is the billing guard

Some provider variables outrank the credential you actually want the sandbox
to use. `ANTHROPIC_API_KEY` beats Claude Code's own subscription credential in
its precedence, so forwarding it would move the sandbox off your subscription
and onto metered API billing without telling you.

Put anything with that property in `deny`. It is refused with an explanation,
and `BRIG_ALLOW_DENIED=1` is the deliberate override for someone who does want
metered billing.

`deny` guards the **environment** channel, and only that one. A `files:`
binding is not checked against it, and no name check could be: a profile could
deliver a metered key inside a `settings.json` and nothing would see it. That
is deliberate rather than an oversight -- `deny` exists to catch *accident*, an
ambient variable swept in because it happened to be in your shell, and a file
binding takes an explicit stored secret plus an explicit binding you wrote.

The guard applies the same way when the value arrives by `ref:` instead of
straight from the environment -- see `secrets` and `env` above. The billing
consequence of a metered key does not change with where the value came from.

## `onboarding`, for an agent that stops on a first-run screen

Some agents ask something on first run that is not authentication, and that
the guest cannot answer -- picking a login method there opens a browser the
microVM does not have. Seeding a couple of non-secret flags into the agent's
own state file settles it:

```yaml
onboarding:
  file: .claude.json
  seed:
    hasCompletedOnboarding: true
    hasTrustDialogAccepted: true
  trustKey: [projects, hasTrustDialogAccepted]
```

`seed` is written only when `file` does not exist. An existing file belongs to
the agent, and brig will not overwrite it.

`trustKey` names the two JSON levels around a directory name, for an agent
that records trust per directory. brig sets it for the directory each run
starts in, resolved to the git repository root as the guest sees it. Nothing
is ever seeded that contains a credential.

## `hostCredential`, deprecated

**Removed in the next release.** No built-in profile carries it any more, and
brig warns when a profile file does.

```yaml
hostCredential:                                  # deprecated
  keychainService: Claude Code-credentials
  tokenField: accessToken
  expiryField: expiresAt
  targetVar: CLAUDE_CODE_OAUTH_TOKEN
  renewHint: run claude on the host once to renew it
```

It read another application's keychain item on **every run**, whenever the
environment carried no value for `targetVar`, and forwarded what it found as an
environment variable. That is the automatic host read this release retires: a
run should not reach a credential store you did not point it at.

Say the same thing with a secret and a source instead, and fill it once:

```yaml
secrets:
  - name: mytool-credentials
    required: false
    field: accessToken                  # drop this to store the whole document
    expiryField: expiresAt
    sources:
      - from: keychain
        service: My Tool-credentials
        hint: run mytool on the host once to log in
env:
  - name: MYTOOL_TOKEN
    ref: secrets.mytool-credentials
```

```bash
brig secret import mytool
```

The difference is when the host is read: at import, once, when you asked --
rather than on every boot. `BRIG_CREDENTIALS_CMD`, which pointed the old
machinery at any command printing equivalent JSON, is replaced by
`brig secret import <profile> --from-command '<command>'` and goes in the same
release.

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
  - MYTOOL_ADMIN_KEY   # would let the agent reconfigure the account
statePaths:
  - .config/mytool
headless: true
mem: 8192
cpus: 4
```

```bash
brig profile import mytool.yaml
MYTOOL_TOKEN=$(pass show mytool/token) brig run mytool
```

`forward:` and `statePaths:` above are the deprecated spellings, kept here
because this is what most existing profiles look like. Written today the same
profile says `env:` with a `ref:`, and `volumes:` in place of `statePaths:`.

brig cannot verify the signature of an image outside `ghcr.io/brig-sh`, so it
will say so on every boot. That is a warning, not a refusal -- see
[security.md](security.md).

## Building the image

The image is the part brig does not do for you. Guest images for the built-in
profiles live in
[brig-sh/community-images](https://github.com/brig-sh/community-images) with
open Dockerfiles, and building your own is documented in
[bring-your-own-image.md](https://github.com/brig-sh/community-images/blob/main/docs/bring-your-own-image.md).

Two things matter for a guest brig can drive. Install the CLI outside the home
directory, because the workspace gets mounted over it at boot. And bake no
credential into the image, since the agent authenticates at runtime and its
state belongs in the home directory brig persists.

## `genericBoot`, for an image that is not a guest image

A guest image normally carries its own kernel and the urunc metadata that says
how to boot it. `genericBoot: true` says this one does not: it is an ordinary
OCI image such as `ubuntu:latest`, and the runtime supplies the kernel and the
initrd instead.

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
mounts are restored before the guest switches root, so it runs as it would
anywhere else.

This works on both operating systems, and for the same reason: the kernel and
the initrd travel as two OCI annotations. hull takes them on its command line
on macOS, and on Linux urunc reads the same two from the container's OCI spec,
which nerdctl passes through. Nothing about the profile changes between them.

Three constraints come with it. The kernel and initrd are host files, and are
never taken from image metadata -- an image must not be able to nominate a
file on your machine. On macOS brig asks hull where they are, because they live
under hull's store and only hull knows where that is; on Linux it looks in
`~/.local/share/brig/assets`. `BRIG_BOOT_ASSETS` overrides both.
The kernel is named for the architecture it boots: `Image` on arm64,
`bzImage` on x86_64. And the guest agent that `brig exec` talks to comes from
that initrd rather than from the image, which is what makes an unmodified image
drivable at all.

You do not have to put them there yourself. If they are missing, brig fetches
them once, on the first `brig run` of a `genericBoot` profile.

Which tool does the fetching differs, because what is guaranteed to be on the
machine differs. On macOS it is hull: it downloads the same bundle for its own
`hull run`, so it already knows the reference, the directory and the registry
credentials, and brig would rather drive that than keep a second copy of all
three. On Linux hull does not exist -- it does not build there -- so brig uses
`oras`, the same shell-out-if-present shape it uses for `cosign` when checking
signatures. If oras is not installed it says so and tells you what to fetch.

Setting `BRIG_BOOT_ASSETS` turns the fetching off entirely: that variable points
at a build you are iterating on, and downloading a release bundle over your own
work would be the opposite of helpful. `BRIG_BOOT_ASSETS_REF` pins a specific
bundle instead of the current one for your platform.

The bundles come from
[hull-assets](https://github.com/NOFireAI/hull-assets), one per guest platform;
`oci/pull-bundle.sh` there does the same fetch by hand.

On Linux this needs `nerdctl` rather than `docker`. Docker does not carry OCI
annotations through to the runtime, so the sandbox would boot with no kernel;
brig refuses up front instead.

## Removing a profile

```bash
brig profile rm mytool
```

The argument is a profile name, not a file name: `rm` resolves it through the
merged set, so it takes aliases and it finds the file whatever that file is
called. If more than one file declares the name, it takes all of them --
removing only the one that loaded would promote the other and leave the
profile listed exactly as before.

Built-in profiles are compiled in, so there is nothing to remove -- import a
profile of the same name to shadow one instead.
