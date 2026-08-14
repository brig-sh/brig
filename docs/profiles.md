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
| `forward` | no | Variables carried in from your environment when they are set and non-empty |
| `deny` | no | Variables never forwarded, whatever `forward` says. See below |
| `statePaths` | no | Paths under `guestHome` holding the agent's state. Reference only at the moment |
| `staleCredentialFiles` | no | Paths an older wrapper used to write a credential into. brig never does, so finding one is worth a warning rather than a deletion |
| `headless` | no | The agent supports a non-interactive run |
| `guiTitle` | no | Window title, for a `kind: gui` profile |
| `hypervisor` | no | macOS backend to boot on: `vz` (the default, and the only one with a graphical console), `hvi` or `qemu`. `BRIG_HYPERVISOR` wins over it. Ignored on Linux, where the shim decides |
| `runtimeBin` | no | The runtime binary to drive instead of the one on `PATH`, `~` expanded. Unlike every other field this is about your machine rather than the workload, so it does not travel usefully to anyone else -- it is how you pin a profile to a build you are working on without exporting a variable in every shell. `BRIG_RUNTIME_BIN` wins over it |
| `rootfsType` | no | How the guest root reaches the VM: `block`, `virtiofs` or `9pfs`. Left unset the runtime picks its own default, which is what a profile that only runs an agent wants. Set `block` when the sandbox installs packages and needs a real writable disk rather than a share sized to the image |
| `genericBoot` | no | The image was never built to be a guest -- a plain OCI image with no kernel and no urunc metadata. The runtime supplies the kernel and initrd and boots it unmodified, on macOS and Linux alike. See below |
| `onboarding` | no | A first-run state file to seed. See below |
| `hostCredential` | no | A credential to read from the host when the environment carries none |
| `reserved` | no | Marks a profile that owns the workspace a session name could otherwise slug onto. See below |

A misspelled field is refused rather than ignored. `forwards:` instead of
`forward:` would otherwise decode into nothing, forward no credentials, and
look exactly like a broken sandbox.

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
to use. `ANTHROPIC_API_KEY` beats `CLAUDE_CODE_OAUTH_TOKEN` in Claude Code's
own precedence, so forwarding it would move the sandbox off your subscription
and onto metered API billing without telling you.

Put anything with that property in `deny`. It is refused with an explanation,
and `BRIG_ALLOW_DENIED=1` is the deliberate override for someone who does want
metered billing.

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

## `hostCredential`, for reusing a login you already have

This is what lets a fresh sandbox work without anyone minting a token first:

```yaml
hostCredential:
  keychainService: Claude Code-credentials
  tokenField: accessToken
  expiryField: expiresAt
  targetVar: CLAUDE_CODE_OAUTH_TOKEN
  renewHint: run claude on the host once to renew it
```

brig reads the macOS keychain by default and searches the blob for those
fields at any depth, so a credential wrapped in an envelope needs no path.
`BRIG_CREDENTIALS_CMD` points the same machinery at any command that prints
equivalent JSON, for any other backend.

The value is read fresh on every invocation and forwarded as environment. It
is never written into the workspace.

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
