# Writing an agent template

brig boots an agent from an OCI image. Any Linux CLI that runs in one already
works, so a template is not a requirement -- it just saves you spelling out
the image, the guest home and the credential variables on every invocation.

The quickest way to write one is to start from the closest existing template
rather than from a blank file:

```bash
brig export claude-code > mine.yaml
$EDITOR mine.yaml
brig import mine.yaml
brig run mine
```

The exported file carries a header explaining every field, so you can edit it
without coming back here.

## Where templates live

One file per template in `~/.config/brig/templates`, overridable with
`BRIG_TEMPLATE_DIR`. YAML or JSON, read the same way: JSON is a subset of
YAML, so one parser handles both.

Export writes YAML because a template is a file a person edits, and YAML has
comments. Import stores your bytes exactly as you wrote them, so the comments
and the ordering survive. Use `brig export --json` if something downstream
consumes templates programmatically.

A template may take a built-in's name. That is deliberate: it is how you pin
your own image for an agent brig already knows about, without inventing a
second name for it. `brig agents` marks those `(custom)`.

## The fields

| field | required | what it is |
| --- | --- | --- |
| `name` | yes | The template name. It becomes the workspace directory and the sandbox name, so it is restricted to lowercase letters, digits, dot, dash and underscore |
| `image` | yes | The guest image to boot |
| `guestHome` | yes | Absolute path where the workspace is mounted. The agent's state lands here, which is what makes the workspace the unit of persistence |
| `binary` | yes* | The agent CLI inside the guest. Not needed when `shell` or `gui` is set |
| `mem`, `cpus` | yes | Guest size. Both must be greater than zero |
| `desc` | no | One line, shown by `brig agents` |
| `forward` | no | Variables carried in from your environment when they are set and non-empty |
| `deny` | no | Variables never forwarded, whatever `forward` says. See below |
| `statePaths` | no | Paths under `guestHome` holding the agent's state. Reference only at the moment |
| `staleCredentialFiles` | no | Paths an older wrapper used to write a credential into. brig never does, so finding one is worth a warning rather than a deletion |
| `headless` | no | The agent supports a non-interactive run |
| `shell` | no | The "agent" is the guest shell itself. `brig run <name> cmd...` runs one command in a login shell |
| `gui` | no | The agent opens a window. There is nothing to pass arguments to |
| `guiTitle` | no | Window title, for a `gui` template |
| `onboarding` | no | A first-run state file to seed. See below |
| `hostCredential` | no | A credential to read from the host when the environment carries none |

A misspelled field is refused rather than ignored. `forwards:` instead of
`forward:` would otherwise decode into nothing, forward no credentials, and
look exactly like a broken sandbox.

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
brig import mytool.yaml
MYTOOL_TOKEN=$(pass show mytool/token) brig run mytool
```

brig cannot verify the signature of an image outside `ghcr.io/brig-sh`, so it
will say so on every boot. That is a warning, not a refusal -- see
[security.md](security.md).

## Building the image

The image is the part brig does not do for you. Guest images for the built-in
templates live in
[brig-sh/community-images](https://github.com/brig-sh/community-images) with
open Dockerfiles, and building your own is documented in
[bring-your-own-image.md](https://github.com/brig-sh/community-images/blob/main/docs/bring-your-own-image.md).

Two things matter for a guest brig can drive. Install the CLI outside the home
directory, because the workspace gets mounted over it at boot. And bake no
credential into the image, since the agent authenticates at runtime and its
state belongs in the home directory brig persists.

## Removing a template

```bash
brig template rm mytool
```

Built-in templates are compiled in, so there is nothing to remove -- import a
template of the same name to shadow one instead.
