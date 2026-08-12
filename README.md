# brig

**Run a coding agent in a sandbox it cannot escape, with the credentials it
needs and none of the ones it does not.**

## TL;DR

```bash
brew tap brig-sh/brig
brew trust brig-sh/brig       # brew refuses untrusted third-party taps
brew install --cask brig

brig run claude               # boots a sandbox, starts Claude Code in it
```

Put the projects the agent should work on in `~/brig/claude-code`. That
directory is the agent's entire world; the rest of your machine is invisible
to it. The sandbox stays up between commands, so the second `brig run` is
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

On Linux there is no equivalent piece. brig drives `nerdctl` over containerd,
so the Linux path is `brig` and nothing else.

## What brig is

`brig` boots an agent CLI -- Claude Code, Codex, and others -- inside a microVM
on macOS or a container on Linux, mounts one directory as the agent's home,
and forwards your credentials into it per exec.

It is not a container runtime, and does not want to be one. It delegates every
mechanical operation -- boot this image, exec in it, stop it -- to `hull` on
macOS and `nerdctl` on Linux, and adds only the four things those tools have
no concept of:

- **Workspace as guest home.** One host directory is the agent's whole world
  and the unit of persistence: its logins, its settings and your projects live
  there and survive a restart.
- **Host-resolved credentials, forwarded per exec.** The guest cannot reach
  your keychain, your secret manager or your SSH agent -- that inaccessibility
  *is* the isolation boundary -- so brig resolves credentials on the host and
  passes them in. Re-read on every exec, so a rotated token needs no restart,
  never written into the workspace, and never placed in a command line where
  `ps` could read it.
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
| `brig run <agent> [args…]` | start the sandbox if needed, then run the agent. Arguments pass through untouched |
| `brig create <agent>` | start the sandbox without attaching; prints its name |
| `brig shell <agent> [cmd…]` | a login shell inside the sandbox, or one command in it |
| `brig exec <agent> -- cmd…` | run one command inside the sandbox |
| `brig stop <agent>` | stop the sandbox, keep it. Starting it again is a boot, not a fresh creation |
| `brig rm <agent>` | stop and remove the sandbox. The workspace is untouched |
| `brig ls` | every brig sandbox, running or merely holding its name, with its workspace |
| `brig reset` | stop and remove every brig sandbox. Workspaces are untouched |
| `brig env <agent>` | what would be forwarded, **by name only**, and whether the guest will be authenticated |
| `brig agents` | the templates, their images, and what each one refuses to forward |
| `brig template ls\|export\|import\|rm` | manage templates (`brig export`/`brig import` are the short forms). `export --json` for JSON instead of YAML |
| `brig version` | |

Flags go before the agent's own arguments; `--` ends brig's parsing outright,
so an agent flag spelled like one of brig's still reaches the agent.

| flag | what it does |
| --- | --- |
| `-n, --name NAME` | a session of its own: own workspace, own sandbox |
| `-t, --image IMAGE` | guest image to boot |
| `-w, --workspace PATH` | host directory to mount as the guest home |
| `-m, --memory MB` | guest memory |
| `--cpus N` | guest vCPUs |
| `-d, --detach` | with `run`: start the sandbox, print its name, exit |

Each flag overrides the corresponding `BRIG_*` setting, which overrides the
template.

```bash
brig run claude -p "summarise this repo"   # headless, arguments passed through
brig run claude -- --name not-a-session    # --name reaches claude, not brig
brig shell codex 'ls -la ~'                # one command, in a login shell
```

### Named sessions

```bash
brig run claude --name refactor
```

A session of its own: its own workspace (`~/brig/claude-code-refactor`), its
own sandbox, and the name you typed reaches the agent as its display name.

Paths use a short lowercase form of the name -- letters, digits, dot, dash and
underscore, ten characters -- so `my_project` and `my-project` stay separate,
while `Foo` and `foo` do not (macOS filesystems ignore case, and two names
sharing one directory but not one VM is a bug waiting to happen). If the slug
differs from what you typed, brig says which directory it used.
`brig env claude --name refactor` prints the one in use.

### If you know Docker Sandboxes (`sbx`)

The verbs line up deliberately, so muscle memory carries over:

| `sbx` | `brig` | note |
| --- | --- | --- |
| `sbx run <agent> [path]` | `brig run <agent> [-w path]` | brig takes the workspace as a flag rather than a positional, so it never has to guess where agent arguments begin |
| `sbx create` | `brig create` | |
| `sbx exec` / `sbx ls` / `sbx stop` / `sbx rm` / `sbx reset` | same | |
| `sbx cp` | n/a | the workspace is a live host directory, so there is nothing to copy across |
| `sbx template ls\|save\|load\|rm` | `brig template ls\|export\|import\|rm` | brig's templates describe the *agent*, not just its image, and are YAML like sbx's kits |
| `sbx secret set` | `BRIG_FORWARD_ENV` + your own secret store | brig resolves nothing itself and stores nothing; whatever puts a value in its environment is enough |
| `sbx -t/--template` | `brig -t/--image` | |
| `sbx -m/--memory`, `--cpus`, `-d`, `--name` | same | |
| `sbx --publish`, `--deny-network` | n/a | not yet; brig does not manage guest networking |
| `sbx --clone` | n/a | not yet; the workspace is mounted directly |
| `sbx login` | n/a | nothing to sign in to |

The deeper differences are the interesting ones. sbx keeps secrets out of the
guest entirely by rewriting auth headers in a host-side proxy; brig forwards
the credential into the guest, because the agent has to be able to use it for
anything other than a proxied HTTP call -- `git push`, a vendor CLI's own
refresh, an MCP server. brig's answer to that exposure is a narrow blast
radius (one workspace, a fine-grained token) rather than a sentinel value.

## Credentials

brig resolves nothing itself: it reads the variables a template names from its
own environment, so any backend works.

```bash
CLAUDE_CODE_OAUTH_TOKEN=$(your-secret-tool read claude/token) brig run claude
<your secret manager's run-with-env command> -- brig run claude
```

With nothing set, brig falls back to the login already on this Mac, read from
the keychain on every invocation. `BRIG_CREDENTIALS_CMD` points that at any
command printing the same JSON, for any other backend.

Three rules apply to every forwarded variable:

- **Unset or empty is skipped**, so it cannot shadow a value baked into the
  image.
- **A `scheme://` value is refused** -- direnv and friends readily leave a
  secret-manager reference in the environment unresolved, and forwarded
  verbatim it yields "Invalid username or token" in the guest, which looks
  exactly like a broken sandbox. `BRIG_ALLOW_REFS=1` forwards it anyway.
- **A denylisted variable is refused**, with the reason. `BRIG_ALLOW_DENIED=1`
  if metered billing is genuinely what you want.

`brig env <agent>` reports what would be forwarded, by name, and whether the
guest will actually be authenticated -- an expired host token is exactly what
sends a sandbox back to its login screen.

Values reach the runtime through its environment, not its command line, so a
forwarded credential is not readable in `ps`. Inside the sandbox it is
readable by anything running alongside the agent, which is inherent: the
sandbox cannot use a credential it cannot see. Prefer a fine-grained
`GH_TOKEN` scoped to the repositories you want reachable.

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

`BRIG_VERIFY=require` refuses anything that cannot be positively verified,
third-party images included. `BRIG_VERIFY=off` skips the check.
[docs/security.md](docs/security.md) explains the reasoning, and what brig does
not protect you from.

Two limitations worth knowing: under the default `missing` pull policy cosign
verifies the tag in the registry, not necessarily the copy already in your
local store; and `claude-desktop` and `ubuntu` still point at
`ghcr.io/nofireai/` images, so they warn on every boot until those move.

## Documentation

The README is the overview. The details live in [docs/](docs/):

- [templates.md](docs/templates.md) -- writing an agent template, field by field
- [security.md](docs/security.md) -- what the boundary is, and what it is not
- [migration.md](docs/migration.md) -- coming from the urunc-* wrappers
- [brigd.md](docs/brigd.md) -- the session daemon and its protocol

## Custom agents and your own images

Any Linux CLI in an OCI image already runs under brig. A template just saves
you spelling out the image, the guest home and the credential variables every
time:

```bash
brig export claude-code > mine.yaml   # start from the closest one
$EDITOR mine.yaml                     # change name, image, forward/deny
brig import mine.yaml
brig run mine
```

Templates are **YAML or JSON** -- JSON is a subset of YAML, so one parser reads
both and nothing has to guess. Export writes YAML, because a template is a
file a person edits and YAML has comments; the exported file carries a header
explaining every field. `brig export --json` for anything consuming templates
programmatically.

An imported file is stored byte for byte as you wrote it, so your comments and
your ordering survive:

```yaml
# A brig agent template. Edit it, then: brig import <this file>
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

A custom template may take a built-in's name -- that is how you pin your own
image for an agent brig already knows about. `brig agents` marks those
`(custom)`. They live in `~/.config/brig/templates/`, one file each.

[docs/templates.md](docs/templates.md) walks through the fields with a worked
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

## Environment variables

Every setting is read in this order, first hit wins:

```
BRIG_<AGENT>_<KEY>   →   BRIG_<KEY>   →   URUNC_<LEGACY>_<KEY>
```

`<AGENT>` is the template name uppercased with dashes as underscores
(`BRIG_CLAUDE_CODE_WORKSPACE`), so one shell can carry different settings for
two agents. The legacy names are the ones the Homebrew wrappers used -- read,
never written, so an existing shell profile keeps working: `URUNC_CLAUDE_*`
for `claude-code`, `URUNC_CLAUDE_DESKTOP_*` for `claude-desktop`,
`URUNC_UBUNTU_*` for `ubuntu`.

Booleans are shell-style: anything except `0` is on.

### Where things live

| variable | default | what it does |
| --- | --- | --- |
| `BRIG_WORKSPACE` | `~/brig/<agent>` | Host directory mounted as the guest home. A named session appends `-<slug>`. An existing Homebrew-era `~/sandboxed-claude` is adopted instead, so a migration keeps its history |
| `BRIG_NAME` | `brig-<agent>` | Sandbox (VM or container) name. A named session appends `-<slug>` |
| `BRIG_TEMPLATE_DIR` | `~/.config/brig/templates` | Where custom templates are read from and written to |

### Image and guest

| variable | default | what it does |
| --- | --- | --- |
| `BRIG_IMAGE` | per template | Guest image to boot |
| `BRIG_PULL` | `missing` | `missing`, `always` or `never`. A cached tag is not re-resolved, so a republished moving tag stays invisible until you say `always` |
| `BRIG_MEM` | per template (4096) | Guest memory, MB |
| `BRIG_CPUS` | per template (4) | Guest vCPUs |
| `BRIG_READY_TIMEOUT` | `30` | Seconds to wait for the in-guest agent after the runtime reports the sandbox running. The two are not the same moment |
| `BRIG_TITLE` | per template | Window title, for a graphical agent |

#### If you know Docker Sandboxes (`sbx`)

The verbs line up deliberately, so muscle memory carries over:

| `sbx` | `brig` | note |
| --- | --- | --- |
| `sbx run <agent> [path]` | `brig run <agent> [-w path]` | brig takes the workspace as a flag rather than a positional, so it never has to guess where agent arguments begin |
| `sbx create` | `brig create` | |
| `sbx exec` / `sbx ls` / `sbx stop` / `sbx rm` / `sbx reset` | same | |
| `sbx cp` | n/a | the workspace is a live host directory, so there is nothing to copy across |
| `sbx template ls\|save\|load\|rm` | `brig template ls\|export\|import\|rm` | brig's templates describe the *agent*, not just its image, and are YAML like sbx's kits |
| `sbx secret set` | `BRIG_FORWARD_ENV` + your own secret store | brig resolves nothing itself and stores nothing; whatever puts a value in its environment is enough |
| `sbx -t/--template` | `brig -t/--image` | |
| `sbx -m/--memory`, `--cpus`, `-d`, `--name` | same | |
| `sbx --publish`, `--deny-network` | n/a | not yet; brig does not manage guest networking |
| `sbx --clone` | n/a | not yet; the workspace is mounted directly |
| `sbx login` | n/a | nothing to sign in to |

The deeper differences are the interesting ones. sbx keeps secrets out of the
guest entirely by rewriting auth headers in a host-side proxy; brig forwards
the credential into the guest, because the agent has to be able to use it for
anything other than a proxied HTTP call -- `git push`, a vendor CLI's own
refresh, an MCP server. brig's answer to that exposure is a narrow blast
radius (one workspace, a fine-grained token) rather than a sentinel value.

## Credentials

| variable | default | what it does |
| --- | --- | --- |
| `BRIG_FORWARD_ENV` | per template | Space-separated list of variables to forward. Replaces the template's list rather than adding to it |
| `BRIG_CREDENTIALS_CMD` | (keychain) | Command printing the host credential JSON on stdout. Any backend: `op read`, `vault kv get`, a script |
| `BRIG_ALLOW_REFS` | `0` | Forward a value that looks like an unresolved `scheme://` secret reference |
| `BRIG_ALLOW_DENIED` | `0` | Forward a variable on the template's billing denylist |
| `GIT_TERMINAL_PROMPT` | `0` | Forwarded as-is; set it to `1` on the host to let git prompt in the guest |

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
| `BRIG_RUNTIME_BIN` | first of `hull`, `urunc-macos` / `nerdctl`, `docker` on PATH | Path to that binary |
| `BRIG_HYPERVISOR` | `vz` | Hypervisor backend, macOS only |
| `BRIG_ENV_ARGV` | `0` | Put forwarded values back on the runtime's command line, where `ps` can read them. For a runtime build that does not accept a bare `--env KEY`. Opt-in, and it costs you the `ps` guarantee |
| `DO_NOT_TRACK`, `HULL_TELEMETRY_DISABLED` | | Passed through to the runtime untouched, and always win. `URUNC_TELEMETRY_DISABLED` still works on urunc-macos builds |

## Coming from the Homebrew wrappers

| was | now |
| --- | --- |
| `urunc-claude` | `brig run claude` |
| `urunc-claude --name foo` | `brig run claude --name foo` |
| `urunc-claude --urunc-shell` | `brig shell claude` |
| `urunc-claude --urunc-stop` | `brig stop claude` |
| `urunc-claude --urunc-env` | `brig env claude` |
| `urunc-claude-desktop` | `brig run claude-desktop` |
| `urunc-ubuntu [cmd…]` | `brig run ubuntu [cmd…]` |

An existing `~/sandboxed-claude`, `~/sandboxed-claude-desktop` or
`~/urunc-ubuntu` is adopted as the workspace when it is there, so history,
logins and checkouts carry over. `URUNC_CLAUDE_*` settings are still read.

Stop the old sandbox first (`urunc-claude --urunc-stop`): brig names its own
VM, so both would otherwise be up with the same directory shared into each.
[docs/migration.md](docs/migration.md) has the full mapping.

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

Templates are data ([internal/agent](internal/agent/agent.go)): a binary, the
variables carrying its credentials, the ones denied for billing safety, the
state paths, and an image. Guest images live in
[brig-sh/community-images](https://github.com/brig-sh/community-images) with
open Dockerfiles, and a template name is the same string as its image name.

Claude Code and Codex are the proven core. Gemini, Grok and opencode are
example templates. `claude-desktop` is the GUI app in a windowed VM, and
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
    '^https://github.com/brig-sh/brig/.github/workflows/release.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

shasum -a 256 -c checksums.txt --ignore-missing
```

Each archive also ships an SPDX SBOM.

## Not yet ported

- **Workspace clone/overlay and explicit-apply.** The workspace is mounted
  directly, as the bash wrapper mounted it, so an agent works on your files
  rather than on a private copy it later applies.
- **Developer ID signing and notarisation** of the brig binaries. cosign
  proves provenance; macOS Gatekeeper wants something else, which is why the
  cask strips the quarantine attribute for now.
- **amd64 guest images.** The published images are arm64, which is what hull
  boots on Apple Silicon.

## License

Apache-2.0, see [LICENSE](LICENSE). That covers brig itself. The agent CLIs it
runs, and the guest images they come in, are each under their own terms.
