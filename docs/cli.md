# Command line reference

Every brig verb and flag, the environment variables, the JSON output and the
exit codes, checked against the code that implements them.
[quickstart.md](quickstart.md) is the walkthrough for a first run. Read this
page when you already know what you want and need the exact syntax.

Every old spelling still works for one more release. This page teaches only
the current one. [migration.md](migration.md) has the full old-to-new table
and the deprecation window.

## Verbs

### `brig run`

```bash
brig run claude ~/code/demo
```

Starts the sandbox if it is not already running, then runs the agent inside
it.

```
brig run <ref> [project] [args...]
```

- `<ref>` names the agent, and, with `@<label>`, a session of its own. See
  [The ref](#the-ref) below.
- `[project]` is a host directory. brig mounts it read-write at
  `/work/<name>` and starts the agent there. See
  [The run line](#the-run-line-ref-project-and-the-agents-own-arguments).
- `[args...]` reach the agent untouched.

Name no project, and brig remounts whatever this session ran with last:

```bash
brig run claude
```

Start a second, independent session of the same agent, with its own sandbox
and its own guest home:

```bash
brig run claude@refactor ~/code/demo
```

Start the sandbox and exit, without attaching:

```bash
brig run claude ~/code/demo -d
```

Run with no project mounted at all, even one this session ran with before:

```bash
brig run claude --no-project
```

Pass an argument through to the agent rather than have brig read it. `--`
ends brig's own parsing:

```bash
brig run claude ~/code/demo -- --version
```

### `brig sh`

```bash
brig sh claude
```

Opens a login shell inside the sandbox, starting it first if it is not
running.

```
brig sh <ref> [command...]
```

`sh` takes no project argument. The first bare word after the ref is already
the guest command:

```bash
brig sh claude ls /work
```

### `brig stop`

```bash
brig stop claude
```

Stops the sandbox and keeps its name and its state on disk. Stopping a
sandbox that is not running is not an error, because that is already the
state `stop` asks for.

### `brig rm`

```bash
brig rm claude
```

Stops the sandbox and removes it. Your guest home and your project are host
directories brig only mounted, so neither is touched, and `rm` says so,
naming the workspace it left behind.

```bash
brig rm --all
```

Stops and removes every sandbox brig has. It prints the list first, one ref
and its state per line, and asks before removing them. Without a terminal to
ask on it refuses and removes nothing: pass `-y` (or `--yes`) to answer in
advance, which is what a script wants. With no sandbox to remove there is
nothing to ask about, and the command proceeds.

```bash
brig rm --all --dry-run
brig rm claude --dry-run
```

`--dry-run` prints the same list, or the one sandbox and the workspace it
would leave, and exits `0` without removing anything. A ref with no sandbox
is still a not-found (exit `3`) under `--dry-run`. `-y` is refused on
`brig rm <ref>`, which asks no question.

`rm` and `stop` each take exactly one ref. Neither takes a list of them.

### `brig ls`

```bash
brig ls
```

Lists every sandbox brig knows about, with its ref, state and workspace.

```bash
brig ls -q
```

Prints refs only, one per line, and skips a row with no derivable ref. Every
line `-q` prints is a word another verb accepts.

### `brig logs`

```bash
brig logs claude --follow
```

Streams the sandbox's log.

```
brig logs <ref> [--follow] [--tail N] [--raw]
brig logs --gateway [<ref>]
```

| Flag | Meaning |
| --- | --- |
| `--follow` | keep streaming as new lines arrive |
| `--tail N` | show the last `N` lines. Default: `-1`, meaning every line |
| `--raw` | skip brig's own formatting |
| `--gateway` | read the network gateway's log instead of the sandbox's own |

With no ref, `--gateway` reads the shared gateway that serves the default
network. With a ref, it reads that sandbox's own gateway log, which exists
only for a sandbox on `--network isolated` or one carrying a policy:

```bash
brig logs --gateway
brig logs --gateway claude
```

### `brig info`

```bash
brig info claude
```

Prints the execution envelope without booting anything: the sandbox name,
the isolation, the guest home, the image, the verification mode, the network
and the credentials by name.

`info` fails only when a required secret cannot be resolved. A declared
secret marked `required: false` prints a warning, and the command still
exits `0`.

### `brig doctor`

```bash
brig doctor
```

Checks, one line each: the host, the hypervisor, the runtime, the boot
assets, cosign, the profile directory, the secret store and brigd.

```bash
brig doctor claude
```

Fills in the image line, which is otherwise the one check doctor cannot make
without knowing which agent you mean.

```bash
brig doctor --json
```

Prints the same checks as a JSON array, each with `name`, `state`, `finding`
and, when the check failed, `fix`.

Only two checks gate the exit status: a missing or broken runtime, and a
secret store that will not open. Every other finding, including one marked
`!!`, prints its fix and leaves the exit status at `0`.

### `brig version`

```bash
brig version
```

Prints `brig v0.1.0-rc18` in this release. `--version` is the same command.

### `brig completion`

```bash
brig completion zsh > "${fpath[1]}/_brig"
```

Prints a completion script for `bash`, `zsh` or `fish` to stdout. Nothing is
installed for you. See [completions.md](completions.md) for where each shell
reads its script from, and exactly what completes where.

### `brig agent`

```bash
brig agent ls
```

Lists the agents you can run.

```
brig agent ls
brig agent show <agent>
brig agent new <name> --from <agent>
brig agent edit <name>
brig agent rm <name>
brig agent import <file>
brig agent export <agent> [name]
```

Copy a built-in agent under a name of your own, then edit the copy:

```bash
brig agent new mine --from claude
brig agent edit mine
```

A built-in agent has no file of its own until `new` gives it one. Print an
agent, to read it or pipe it:

```bash
brig agent show claude-code
```

`export` is the alternative to `new --from`: same result, the other
direction. Save a copy of an agent you did not create, under a name you
choose:

```bash
brig agent export claude-code mine
```

Add a file you wrote or received:

```bash
brig agent import mine.yaml
```

Delete a file-backed agent, after asking:

```bash
brig agent rm mine
```

`--json` with `show`, `new` or `export` prints the document as JSON instead
of YAML, with no envelope. See [`--json` output](#--json-output) below.
[profiles.md](profiles.md) is the reference for the file format.

### `brig policy`

```bash
brig policy ls
```

Lists every policy, and what binds it.

```
brig policy ls
brig policy create <name>
brig policy edit <name>
brig policy show <name>
brig policy rm <name>
brig policy attach <policy> <agent> [-n <label>]
brig policy detach <policy> <agent> [-n <label>]
brig policy check <agent> [-n <label>]
```

Write a starter policy, then open it:

```bash
brig policy create locked-down
```

Bind it to every run of an agent:

```bash
brig policy attach locked-down claude
```

Bind it to one session instead of every run:

```bash
brig policy attach locked-down claude -n refactor
```

Confirm what is bound to a run, and whether brig can enforce it:

```bash
brig policy check claude
```

[policies.md](policies.md) covers the document format and what each network
posture means.

### `brig secret`

```bash
brig secret create gh-token
```

Reads the value from stdin and stores it under that name in your keyring.

```
brig secret create <name> [-f FILE]
brig secret update <name> [-f FILE]
brig secret read <name>
brig secret delete <name>
brig secret ls
brig secret import <agent>
brig secret import <agent> <name>
```

Fill every secret an agent's profile declares, from your host, once:

```bash
brig secret import claude-code
```

Fill one of them:

```bash
brig secret import claude-code gh-token
```

The value is never a command-line argument, so it never appears in `ps` or
in your shell history. [secrets.md](secrets.md) covers the store, provenance
and the sources a profile can declare.

### `brig telemetry`

```bash
brig telemetry status
```

Reports whether usage data is sent, and what decided the answer.

```bash
brig telemetry off
```

Turns it off, durably, on this machine. See [telemetry.md](telemetry.md) for
what is counted, what is never collected, and how the answer is stored.

## The ref

A ref is `<agent>` or `<agent>@<label>`. An empty label is the agent's
default session, so `claude` and `claude@refactor` are two sessions of one
agent, never two agents. [sessions.md](sessions.md) explains what a session
keeps separate, and what survives which command.

The separator is exactly one `@`. `brig` refuses a ref instead of rewriting
it:

| Written | Refused because |
| --- | --- |
| `claude@@x` | more than one `@` |
| `@refactor` | no agent named before the `@` |
| `claude@` | a trailing `@` names no session. Drop it for the default session, or name one |
| `claude@Refactor` | the label is not already clean. Labels use lowercase letters, digits, dot, dash and underscore |

A label is never rewritten for you. The label reaches two places that must
agree on it, the sandbox name and the guest home directory, so a label that
needs cleaning up first is refused rather than silently changed.

## The run line: ref, project, and the agent's own arguments

`brig run <ref> [project] [args...]` reads three kinds of token from one
line, told apart by position and count, never by the filesystem:

1. The first bare word is the ref.
2. On `run` only, the second bare word is a project directory. brig mounts
   it read-write at `/work/<basename>` and starts the agent there.
3. The next bare word, or anything after `--`, is the agent's own argument.

The directory named as the project must exist. brig does not read a bare
word as a project only when a directory of that name happens to exist: an
argument's meaning does not depend on the filesystem, so a directory that is
not there is an error rather than a silent fallback to the agent's argv.

`--` ends brig's own parsing outright. No word after it is ever read as a
project, and a project already read before the marker stands:

```bash
brig run claude ~/code/demo -- --version
```

Brig's own flags keep being read after the ref and after the project,
because both are brig's own operands too:

```bash
brig run claude ~/code/demo --mem 4096 -d
```

Only `run` takes a project this way. `brig sh claude ~/code/demo` reads
`~/code/demo` as the start of the guest command, not as a directory to
mount.

## Flag placement

A brig line has two positions for brig's own flags: global, left of the
verb, and run-line, between the verb and wherever the agent's own arguments
begin.

| Position | Flags |
| --- | --- |
| Global | `--verbose`, `-q`/`--quiet`, `--json` |
| Run-line | `--image`, `--home`, `--mem`, `--cpus`, `--no-project`, `-d`/`--detach`, `--skills`, `--network`, `--offline`, and, as peers, `-q`/`--quiet` and `--json` |

The global position is a closed set. A token there that is none of the three
global flags is refused by name, never forwarded to an agent:

```
brig: unknown flag "--nope" before the command. brig takes a command first:
`brig run claude`, `brig ls`. If "--nope" is the agent's, it goes after the
profile
```

Exactly two flags are accepted in both positions, and they behave
differently:

| Flag | After the verb, on the run line |
| --- | --- |
| `-q`/`--quiet` | still works this release, and prints one deprecation notice moving it left. See [migration.md](migration.md) |
| `--json` | a permanent peer spelling. No notice, either position |

```bash
brig info claude --json
brig --json info claude
```

Both print the same report, and neither is deprecated.

An unrecognized flag before the ref is refused by name, because there is no
agent yet to hand it to:

```
brig: unknown flag "--help" before the profile name. brig's own flags come
before the profile and the agent's after it; put "--help" after the profile
to pass it through, or -- to end brig's flags
```

The same spelling after the ref reaches the agent untouched. brig names one
of its own flags found past that point, without taking it, so a working line
keeps working:

```bash
brig run claude -p hi --quiet
```

runs the agent with `-p hi --quiet` and warns that `--quiet` looked like
brig's own flag but stood where the agent's arguments already begin.

### Run-line flags

| Flag | Value | Default | Notes |
| --- | --- | --- | --- |
| `--image IMAGE` | image ref | the agent's own | guest image to boot |
| `--home PATH` | host directory | the agent's own guest home | mounted as the agent's home. Replaces `--workspace`, see [migration.md](migration.md) |
| `--mem MB` | number | the agent's own (`4096` for most shipped agents) | guest memory |
| `--cpus N` | number | the agent's own (`4` for most shipped agents) | guest vCPUs |
| `--no-project` | (none) | off | mount no project this run, even one this session ran with before. On any verb but `run`, refused by name as a usage error |
| `-d`, `--detach` | (none) | off | start the sandbox and exit, without attaching. Parses on every verb, but only `run` reads it. On `sh`, `stop`, `rm` and `info` it is silently inert |
| `--skills` | (none) | off | copy your own `~/.claude` skills and plugins into the guest home. The host copy is never written. Same as `BRIG_SKILLS=1` |
| `--network MODE` | `shared`, `isolated` or `offline` | `shared`, unless the agent's own profile sets `network:` (none of the shipped agents do) | the sandbox's network posture. See [policies.md](policies.md) |
| `--offline` | (none) | off | shorthand for `--network offline`: the agent runs, the workspace is there, nothing leaves |

Flag beats an environment setting beats the profile's own field, in that
order, for every value above with an `env` counterpart in
[Environment variables](#environment-variables) below.

`--mem` and `--cpus` take a positive whole number. Anything else, including
`0`, is refused by name rather than silently rounded or ignored.

## `--json` output

The set of verbs that accept `--json` is larger than `brig --help`'s own
summary line suggests, and it splits by where the flag stands as well as by
verb.

| Verb | Where `--json` is accepted | Shape |
| --- | --- | --- |
| `ls` | global, or local after `ls` | envelope |
| `info`, `env` | global, or local on the run line | envelope |
| `doctor` | global, or local after `doctor` | envelope |
| `run`, `sh` | global, or local on the run line | one compact line, see below |
| `agent ls` | global, or local after `ls` | envelope |
| `secret ls` | global, or local after `ls` | envelope |
| `agent show`, `agent export`, `agent new` | local only, after the subcommand | bare document, no envelope |
| `policy show` | local only, after the subcommand | bare document, no envelope |

"Global" means left of the verb: `brig --json ls`. "Local" means the flag
stands after the subcommand, on either side of that subcommand's own
operand: `brig agent show claude-code --json` and
`brig agent show --json claude-code` both work.

`agent` accepts `--json` in the global position only when its subverb is
`ls`. `policy` never does, on any subverb. So the global spelling refuses
both `agent show` and `policy show`, even though each has a local `--json`
that works:

```
brig --json agent show claude-code
brig: `brig agent` has no --json output. --json is for the read verbs: ls,
info, agent ls, secret ls, doctor (env takes it too, but env is deprecated;
prefer info), and for run and sh
```

The flag has to follow `agent show`, not precede `agent`. Every verb not
listed in the table above refuses `--json` in both positions, naming the
verbs that do accept it. `env` is the deprecated spelling of `info` and
takes the flag on the same terms. Prefer `info`.

**Envelope shape.** A list or report verb prints
`{"apiVersion": "brig.sh/v1alpha1", "kind": "...", "data": ...}`. Within one
`apiVersion`, a field is only ever added, never renamed or removed, so a
script written against it keeps parsing as brig grows. No field carries a
credential value, names only.

**Bare shape.** `agent show`, `agent export`, `agent new` and `policy show`
print the document itself, with no envelope, because each renders a file
meant to be saved and read back by brig:

```bash
brig agent show claude-code --json
```

```json
{
  "name": "claude-code",
  "desc": "Claude Code (Anthropic)",
  "binary": "claude",
  ...
}
```

**`run` and `sh` under `--json`.** The agent runs as a child of brig, and
after it exits brig prints one compact JSON line with the outcome, the last
line of stdout:

```json
{"apiVersion":"brig.sh/v1alpha1","kind":"Run","data":{"ref":"claude","sandbox":"brig-claude-code","stage":"agent","exit":0}}
```

`data.stage` is one of four values: `"brig"` for a refusal before the agent
ran, `"agent"` for an agent that ran, `"gui"` for a windowed agent, or
`"detached"` under `-d`. `data.exit` is the agent's own exit status when
`stage` is `"agent"`, and one of brig's own exit codes otherwise. See
[Exit codes](#exit-codes) for what those carve-outs mean for a script.

**Completion under-offers it.** Shell completion does not offer `--json` for
`ls`, `agent ls` or `secret ls`, even though all three accept it. This is a
gap in the completion tables, not in what the verbs accept. Type it by hand
on those three until it is fixed.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | success |
| `1` | a general failure |
| `2` | a usage error: an unknown flag, a stray argument, or a value in the wrong place, as reported by the verb's own parser |
| `3` | no such thing: an unknown agent, or a sandbox that is not there |
| `4` | no usable runtime: none installed, an unknown `BRIG_RUNTIME`, or `BRIG_RUNTIME_BIN` (or a profile's own `runtimeBin`) pointing at nothing. The refusal names the setting that caused it |
| `5` | a boot refused over image verification |
| `6` | a required secret was not resolved, or the secret store did not open |

This table is asserted end to end by `script/smoke.sh` and
`cmd/brig/exit_test.go`, and is the one [docs/stability.md](stability.md)
calls stable enough to script against.

**6 is for required secrets only.** A declared secret marked
`required: false` produces a warning, not a failure. `claude-code`'s two
secrets are both optional, so `brig info claude` with neither one set exits
`0`.

**Two carve-outs under `run --json` and `sh --json`.** The agent runs as a
child of brig, and its own exit status becomes brig's, read ahead of every
class above. An exit `3` from `brig --json run claude` can be the agent's
own `3`, not brig's "no such agent".

Branch on the `Run` object's `data.stage` field, not on the number alone.
`"agent"` means the code is the agent's. Anything else means it is one of
the classes in the table.

Separately, a brig refusal under `--json` is written to stdout as the `Run`
object, the same place as every success case, never to stderr.

**A handful of usage mistakes exit `1` instead of `2`.** An unknown
top-level command, a run-line verb given no ref, a missing subcommand on
`agent`, `policy`, `secret` or `telemetry`, and `secret import` or
`policy check` given no agent, currently return a plain error rather than
the usage type, and so exit `1`:

```
brig nosuchverb
brig: unknown command "nosuchverb" (try `brig help`)
```

```
brig run
brig: run needs a profile, for example `brig run claude`. `brig agent ls`
lists them
```

Both exit `1`, while the same class of mistake on `brig ls extra` or
`brig completion bogus` exits `2`. If a script needs to tell a usage mistake
apart from a general failure in this specific area, check for a nonzero
status rather than for exactly `2`.

## Environment variables

Most settings below are read through `BRIG_<KEY>` and also honor
`BRIG_<AGENT>_<KEY>`, which wins when both are set, so one shell can carry a
different value per agent. The agent name is upper-cased with dashes turned
to underscores, so `claude-code` reads `BRIG_CLAUDE_CODE_MEM` ahead of
`BRIG_MEM`.

A handful of settings have no per-agent form, because they are read once for
the whole invocation rather than resolved per run: the directories, the
runtime choice, the boot-asset and gateway paths, and `BRIG_ENV_ARGV`. Each
is marked "global only" below.

### Sandbox and profile locations

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_WORKSPACE` | `~/brig/<agent>` | host directory mounted as the guest home. A named session appends `-<slug>` |
| `BRIG_NAME` | `brig-<agent>` | the sandbox's own name. Must begin with `brig-`, or `brig ls` and `brig rm --all` cannot find it. A named session appends `-<slug>` |
| `BRIG_PROFILE_DIR` (global only) | `$XDG_CONFIG_HOME/brig` | where your own agent files live. `BRIG_TEMPLATE_DIR` still works for one release |
| `BRIG_POLICY_DIR` (global only) | `$XDG_CONFIG_HOME/brig/policies` | where policy files live |
| `BRIG_STATE_DIR` (global only) | `~/.brig` | where brig keeps what has to outlive one command, including the project each sandbox last ran with |

### Guest resources and network

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_IMAGE` | the agent's own | guest image to boot |
| `BRIG_PULL` | `missing` | `missing` pulls only when the image is not already on the host. `always` re-pulls every run. `never` refuses to boot an image that is not already there |
| `BRIG_MEM` | the agent's own | guest memory, MB |
| `BRIG_CPUS` | the agent's own | guest vCPUs |
| `BRIG_READY_TIMEOUT` | `30` | seconds to wait for the in-guest agent once the runtime reports the sandbox running. The two are not the same moment |
| `BRIG_NETWORK` | `shared` | `shared`, `isolated` or `offline`. An unrecognized value refuses the run. See [policies.md](policies.md) |
| `BRIG_SKILLS` | `0` | `1` copies your own `~/.claude` skills and plugins into the guest home. Same as `--skills` |
| `BRIG_FORWARD_ENV` | (unset) | a space-separated list of environment variable names to carry into the guest, read live on every run |
| `BRIG_TITLE` | the agent's own | window title for a graphical agent |

`BRIG_FORWARD_ENV` replaces only a profile's own `env:` bindings that
declare the singular `ref: env.<name>` form. It leaves a binding that names
its source through a `refs:` chain untouched, even one whose chain includes
an `env.` entry. A name the profile binds from somewhere else, `secrets.` or
a literal `value:`, is dropped from the override rather than layered onto
it, and brig warns about each one it drops. The profile's own binding wins,
because the profile is what you wrote.

### Credentials and Git

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_ALLOW_REFS` | `0` | `1` forwards a value that still looks like an unresolved `scheme://` secret reference |
| `BRIG_ALLOW_DENIED` | `0` | `1` forwards a variable on the agent's own billing denylist |
| `BRIG_ALLOW_EXPIRED` | `0` | `1` forwards a host credential even though it reports as expired. Scoped to the deprecated `hostCredential:` field |
| `BRIG_GIT_CONFIG` | `0` | `1` writes a credential helper and gitconfig into the guest, routing an SSH GitHub remote over HTTPS |
| `BRIG_GIT_HOSTS` | `github.com` | space-separated hosts the forwarded token applies to |
| `BRIG_GIT_USER` | resolved on the host | username paired with the forwarded token |
| `BRIG_GIT_IDENTITY` | `1` | `0` stops brig from forwarding the host commit identity resolved from the invoking directory |
| `BRIG_GIT_NAME`, `BRIG_GIT_EMAIL` | the host's `git config` | override that identity |
| `BRIG_TRUST_WORKSPACE` | `1` | pre-answers the agent's own "do you trust this folder" question for the directory a run starts in |
| `BRIG_ENV_ARGV` (global only) | (unset) | exactly `1` puts a forwarded value on the runtime's own command line, where `ps` can read it. Never applies to a value brig resolved itself, a secret or the host credential, which stay off the command line regardless |

[authentication.md](authentication.md) and [secrets.md](secrets.md) cover
what each of these does with the credential once it is in the guest.

### Image verification

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_VERIFY` | `warn` | `warn`, `require` or `off`. `strict` is an alias for `require`, and `none` and `0` both alias `off`. An unrecognized value refuses the run rather than falling back to `warn` |
| `BRIG_VERIFY_REGISTRY` | `ghcr.io/brig-sh/` | image prefix treated as brig's own, so a signature is expected |
| `BRIG_VERIFY_IDENTITY` | brig's own community-images build workflow | certificate identity regexp cosign must match |
| `BRIG_VERIFY_ISSUER` | GitHub Actions OIDC | certificate OIDC issuer |
| `BRIG_COSIGN_BIN` | `cosign` on `PATH` | path to the cosign binary |

`warn` reports an unverifiable image and boots anyway, and also stops to ask
about an image that claims to be brig's own and is not. `require` refuses to
boot anything it cannot positively verify, cosign missing included. See
[security.md](security.md) for what verification does and does not catch.

### Runtime and hypervisor

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_RUNTIME` (global only) | `hull` on macOS, `nerdctl` on Linux | which backend to drive |
| `BRIG_RUNTIME_BIN` (global only) | the first of `hull`, `nerdctl`, `docker` found on `PATH` | path to that binary |
| `BRIG_HYPERVISOR` | the agent's own `hypervisor:` field, else `vz` | macOS only: `vz`, `hvi` or `qemu`. Wins over the agent's own field when set |
| `BRIG_ROOTFS_TYPE` | the agent's own `rootfsType:` field | `block`, `virtiofs` or `9pfs`, how the guest root reaches the VM under `hull`. `nerdctl` ignores it. A profile's own `rootfsType:` outside that set is refused when the profile loads, but this variable is passed to `hull` unchecked |

`hvi` is the only backend that enforces an attached egress policy or
`--network isolated`, and it needs macOS 15 or newer. `vz` is the only
backend with a graphical console. On macOS 14, set `BRIG_HYPERVISOR=vz` to
run at all, since brig refuses an `hvi` run there rather than falling back
on its own. See [runtimes.md](runtimes.md).

### Boot assets and the network gateway

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_BOOT_ASSETS` (global only) | `~/.hull/assets` on macOS, `$XDG_DATA_HOME/brig/assets` on Linux | directory holding the host kernel and initrd a `genericBoot` agent needs |
| `BRIG_BOOT_ASSETS_REF` (global only) | `ghcr.io/nofireai/hull-assets:<os>-<arch>` | the bundle brig fetches when the boot assets are missing |
| `BRIG_GATEWAY_SOCK` (global only) | `<gateway dir>/gateway-<subnet>.sock` | control socket of the shared network gateway |
| `BRIG_GATEWAY_DIR` (global only) | the directory of `BRIG_GATEWAY_SOCK`, else `~/.brig` | where every gateway socket and log lives, shared and per-sandbox alike |

## See also

[migration.md](migration.md) has every retired verb, subverb, flag,
position and profile key, and what replaces each one.
[stability.md](stability.md) says which parts of this page you can write a
script against today.
