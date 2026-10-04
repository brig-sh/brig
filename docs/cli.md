# Command line reference

For a walkthrough of a first run, see [Quickstart](quickstart.md).

Most retired spellings still work until v0.4.0.
[Retired spellings](migration.md) has the old-to-new table and the
exceptions. [Stability](stability.md) lists what you can write a script
against.

## Everyday commands

| Command | What it does |
| --- | --- |
| `brig run claude ~/code/demo` | starts the sandbox and runs the agent against that project |
| `brig run claude` | reruns the agent, remounting the project this session used last |
| `brig run claude@refactor ~/code/demo` | starts a second, independent session of the same agent |
| `brig sh claude` | opens a login shell inside the sandbox |
| `brig ls` | lists every sandbox, with its ref, state and guest home |
| `brig info claude` | prints the execution envelope, without booting anything |
| `brig stop claude` | stops the sandbox and keeps its state on disk |
| `brig rm claude` | stops the sandbox and removes it |
| `brig network publish claude 3000` | opens the agent's port 3000 on `localhost:3000` |

## Verbs

### `brig run`

```bash
brig run claude ~/code/demo
```

`brig run` starts the sandbox if it is not running. Then it runs the agent
inside the sandbox.

```
brig run <ref> [project] [args...]
```

- `<ref>` names the agent and, with `@<label>`, a separate session. See
  [The ref](#the-ref).
- `[project]` is a host directory that Brig mounts for the agent. See
  [The run line](#the-run-line).
- `[args...]` reach the agent untouched.

If you name no project, Brig mounts the project that this session ran with
last:

```bash
brig run claude
```

Start a second session of the same agent. It has its own sandbox and its own
guest home:

```bash
brig run claude@refactor ~/code/demo
```

Start the sandbox and exit without attaching:

```bash
brig run claude ~/code/demo -d
```

Run with no project mounted, even one that this session ran with before:

```bash
brig run claude --no-project
```

If Brig reads an argument as its own, put the argument after `--` to pass it
to the agent:

```bash
brig run claude ~/code/demo -- --version
```

### `brig sh`

```bash
brig sh claude
```

`brig sh` opens a login shell inside the sandbox. If the sandbox is not
running, `brig sh` starts it first.

```
brig sh <ref> [command...]
brig sh <ref> -c '<script>' [args...]
```

`sh` takes no project argument. The first bare word after the ref starts the
guest command:

```bash
brig sh claude ls /work
```

Each word after the ref is one argument to the guest command, as you typed
it. The command runs under a login shell, which gives the command its
environment. The guest does not parse the words again, so a pipe, a `;` or a
glob is not shell syntax there.

To run a script, put `-c` in front of it, as with `sh -c`. The login shell
parses the script in the guest, so `~` and `$HOME` are the guest home:

```bash
brig sh claude -c 'ls /work | wc -l'
brig sh claude -c 'cat ~/.claude/settings.json'
```

Words after the script are its `$0`, `$1` and the parameters after them, as
with `sh -c`. The flags that go in front of `-c` with `sh -c` also work:
`-ec` stops at the first failing command, `-xc` traces and `-uc` refuses
unset variables. These flags apply to the script, and not to the login
profile that runs before it. Brig refuses `-c` with no script, or with an
empty script, before the sandbox starts.

Without `-c`, your own shell expands an unquoted `~` before Brig sees it, so
the `~` names your home on the host.

A single word with a space or a shell operator in it, such as
`brig sh claude 'ls | wc -l'`, is read as the name of a command. Brig prints
a hint that names `-c` before the command runs.

### `brig stop`

```bash
brig stop claude
```

Stops the sandbox and keeps its name and its state on disk. Stopping a
sandbox that is not running is not an error.

### `brig rm`

```bash
brig rm claude
```

`brig rm` stops the sandbox and removes it.

- `rm` deletes a guest home that Brig created, and prints its path. Brig
  creates one when the run names no `--home` or `BRIG_WORKSPACE`.
- `rm` leaves a guest home that you named, and your project. Brig only
  mounted these host directories. `rm` prints the path of each.

These cases fail:

| Case | Exit | Result |
| --- | --- | --- |
| the runtime reports no sandbox for the ref, for example after `hull rm` | `3` | `rm` says so and forgets the session that Brig still had for the ref. The next run of that ref starts a new session, on the network that a new session gets and not the network of the old sandbox. A guest home that Brig created stays until that run, which deletes it, and `rm` prints its path |
| Brig cannot rewrite its session index | `1` | `rm` keeps the session as it was |
| `rm` removes the sandbox and cannot delete its guest home | `1` | `rm` names the home. The sandbox stays removed. The next run of the session deletes what is left of the home before it boots |

```bash
brig rm --all
```

`brig rm --all` stops and removes every sandbox that Brig has. First it lists
each sandbox as its ref, sandbox name and state, one per line. Then it asks
for confirmation.

- If stdin is not a terminal, the command refuses, removes nothing and exits
  `1`.
- To confirm in advance, for example in a script, pass `-y` (or `--yes`).
- If there is nothing to remove, the command asks nothing and exits `0`.

`brig rm --all` also stops the shared network gateway that `hvi` sandboxes
use, when no sandbox is on it. It examines the whole host first. A sandbox of
another session, or a sandbox that still boots, keeps the gateway running.

```bash
brig rm --all --dry-run
brig rm claude --dry-run
```

`--dry-run` prints what the command removes and removes nothing. It exits
`0`, except for a ref with no sandbox.

| Command | `--dry-run` prints |
| --- | --- |
| `rm --all` | the same list that the prompt shows |
| `rm <ref>` | the one sandbox, and the guest home that `rm` deletes or leaves |
| `rm <ref>`, when the ref has no sandbox | the session that `rm` forgets. `--dry-run` forgets nothing and still exits `3` |

`brig rm <ref>` asks no question, so it refuses `-y`.

`rm` and `stop` each take a single ref.

### `brig ls`

```bash
brig ls
```

`brig ls` lists every sandbox that Brig knows, one row each. The columns are
`REF`, `SANDBOX` (the runtime's name for the sandbox), `STATE` and
`WORKSPACE` (the guest home).

```bash
brig ls -q
```

`-q` prints refs only, one per line. It skips a row with no derivable ref.
Another verb accepts every line that `-q` prints.

### `brig logs`

```bash
brig logs claude --follow
```

`brig logs` streams the log of the sandbox.

```
brig logs <ref> [--follow] [--tail N] [--raw]
brig logs --gateway [<ref>]
```

| Flag | Meaning |
| --- | --- |
| `--follow` | keep streaming as new lines arrive |
| `--tail N` | show the last `N` lines. Default: `-1`, meaning every line |
| `--raw` | keep terminal control sequences, which Brig strips by default |
| `--gateway` | read the network gateway's log instead of the sandbox's own |

With no ref, `--gateway` reads the log of the gateway that `shared` sandboxes
use. With a ref, it reads the gateway log of that sandbox. Only an isolated
sandbox has one. A new default `hvi` sandbox is isolated, and so is a sandbox
that carries a policy. For any other sandbox, the command exits `3`.

```bash
brig logs --gateway
brig logs --gateway claude
```

### `brig info`

```bash
brig info claude
```

`brig info` prints the execution envelope and boots nothing. The envelope
has the sandbox name, the isolation, the guest home, the image and the
verification mode. It also has the network, every published port and the
credentials by name. `brig --verbose run` prints the same envelope before it boots.

The network row gives the posture of the running sandbox. When the posture of
the next boot is different, the row gives that posture too.

If Brig cannot resolve a required secret, `info` fails with exit `6`. For a
declared secret marked `required: false`, `info` prints a warning and still
exits `0`. The two secrets of `claude-code` are both optional, so
`brig info claude` with neither one set exits `0`.

### `brig plan`

```bash
brig plan claude
brig plan claude ~/src/demo
brig plan claude@refactor --json
```

`brig plan` prints the permissions of a run: the mounts, the network, the
policies, the limits, the image, the runtime and the credentials. It boots
nothing and opens no secret. The [field table](#plan-json-fields) describes
each item.

`plan` reads the run line as `brig run` does: a project after the ref,
`--no-project` and the run-line flags that shape a run. As a result, you can
plan the first `brig run claude ~/src/demo` before you run it. If `brig run`
refuses to mount a project directory, the plan fails the same way.

With no policy bound, `POLICY` reads `(none)` and `noPolicy` is `true`.

#### Refusals in the plan

When `brig run` refuses a run before it starts anything, the plan opens with
a `REFUSED` row. The row carries the message of the run, and `--json` carries
the same message as `refused.reason`. The plan still prints and `brig plan`
still exits `0`, so a script must read `refused` and not the exit code.

The plan reports these refusals:

1. a remembered project reached through a symlink
2. a sandbox the runtime holds whose network Brig cannot determine
3. a hypervisor this host cannot boot, such as `hvi` on macOS 14
4. what the backend cannot honor: a policy or a published port it cannot
   enforce, an isolated network on `vz` or `qemu`, or a graphical profile on
   a backend with no display

With no runtime on PATH, Brig does not ask the backend, and
`runtime.available` is `false`.

A required secret that is missing from the store also stops the run, with
exit `6`. The plan marks that credential `unresolved` and `required`. It
does not set `refused`.

The plan does not report these refusals:

- A session name that another session already owns. The run makes this check
  after checks 1 and 2 and before checks 3 and 4. The check records the
  name, which a plan does not do. If a name collides and check 3 or 4 also
  refuses, the run reports the collision and the plan reports the other
  refusal.
- A refusal at boot. One is an image that fails verification. The other, on
  `hvi` under a policy, is a network gateway that does not confirm that it
  can enforce the policy. Brig asks the runtime about the gateway only when
  it starts one.

#### Credentials in the plan

`plan` reads the secret store by name, as `brig secret ls` does. It does not
read a value.

| State | When |
| --- | --- |
| `resolved` | the store lists the name |
| `unresolved` | the secret is missing from the store. The command still exits `0` |
| `unknown` | `plan` cannot list the store, and nothing later in the chain of the credential has a value |
| `withheld` | the run drops the credential at the denylist, or the credential is an unresolved secret-manager reference in the shell |

The run reads the value. That read can fail for a credential that the plan
marks `resolved`:

- Another tool emptied the value. Brig refuses to store an empty value, so
  only another tool leaves one. The run does not forward the empty value. It
  uses the next source in the chain. When the secret is the last source, the
  run prints a warning that names the secret.
- The item is damaged, such as a macOS keychain item whose sealed half is
  gone.
- You deny a keychain or keyring prompt.
- A Linux keyring collection is locked.

On the last three, the run stops with exit `6` for a required secret and
drops an optional one with a warning.

#### The plan digest

The last row is a digest of the plan. It is `sha256:` over the `data` object
with `digest` set to the empty string. The object is in the compact encoding
that Go's `encoding/json` writes:

- the fields in the order of the [field table](#plan-json-fields)
- no whitespace
- non-ASCII text as UTF-8
- `<`, `>` and `&` escaped as `\u003c`, `\u003e` and `\u0026`

Every list in the plan is sorted. As a result, the digest is the same for the
same run, and it changes when any field changes.

To verify a digest, encode `data` that way and hash it. Python's
`json.dumps(data, separators=(",", ":"), ensure_ascii=False)` gives the same
bytes when no string in `data` holds `<`, `>`, `&`, U+2028 or U+2029.

The digest covers every field, the sentences included: `isolation`,
`network`, `refused.reason`, each credential's `reason`, and `runtime.bin`.
A release that rewords one of them changes the digest with no permission
changed.

#### Plan JSON fields

`brig plan --json` prints `kind: Plan`. Its `network`, `image.ref` and
`image.pull` are the same values that `brig info --json` prints. Its
`argvExposed` has the same names as the one from `brig info --json`, sorted.
`brig info --json` keeps the order in which the run adds them. The `data`
fields, in order:

| Field | Type | Meaning |
| --- | --- | --- |
| `session` | string, omitted for an unnamed run | the session name, as typed |
| `profile` | string | the profile the ref resolved to |
| `sandbox` | string | the sandbox name |
| `refused` | object, omitted when nothing refuses the run | `reason`: the message `brig run` prints when it refuses this run |
| `runtime` | object | `kind`, `bin` and `available`, as in `brig info --json`, and `backend`: the hypervisor the profile or `BRIG_HYPERVISOR` names, omitted for the runtime's own default |
| `isolation` | string | what the sandbox stands on, as a sentence |
| `home` | mount | the guest home |
| `project` | mount, omitted when the run has none | the project |
| `projectRefused` | string, omitted when nothing was refused | why the project this session remembers is not mounted. `refused` carries the same reason |
| `skills` | list of mounts | the host directories `--skills` copies into the guest home |
| `image` | object | `ref` and `pull` |
| `verify` | object | `mode` (`off`, `warn` or `require`) and `policy` (`brig`, `replaced` or `off`), as in `brig info --json` |
| `network` | string | the posture as a sentence |
| `posture` | string | `shared`, `isolated` or `offline` for the next boot, or `unknown` when the run refuses a sandbox whose network Brig cannot determine |
| `ports` | list of strings | every published port |
| `policies` | list of strings | every bound policy |
| `noPolicy` | bool | `true` when no policy is bound |
| `egress` | object, omitted when no policy is bound | `default` (`allow` or `deny`), and the `allow` and `deny` lists. Each rule has a `host` or a `cidr` |
| `credentials` | list | one entry per credential, below |
| `argvExposed` | list of strings, omitted when empty | the variables whose values `BRIG_ENV_ARGV=1` puts on the runtime's command line, by name, sorted |
| `limits` | object | `mem` in MB and `cpus` |
| `digest` | string | `sha256:` and 64 hex digits |

A mount has `host`, `guest` and `mode`. `mode` is `read-write` for the home
and the project, and `copy` for a skills directory. Each credential has these
fields:

| Field | Values |
| --- | --- |
| `name` | the guest variable for `env`, the path under the guest home for `file`, and the secret itself for `none` |
| `delivery` | `env`, `file`, or `none` for a required secret that no binding delivers |
| `source` | `environment` or `secret`, omitted when nothing in the chain has a value |
| `secret` | the store name the binding reads, omitted when it reads none |
| `required` | whether the run stops without it |
| `state` | `resolved`, `unresolved`, `withheld` (the run drops it at a guard), or `unknown` (the store did not answer a listing) |
| `reason` | why the credential is not `resolved`, omitted when it is |

### `brig network`

```bash
brig network publish claude 3000      # the agent's dev server, on localhost:3000
brig network publish claude 8080:80   # host 8080 carries guest 80
brig network ls claude                # what this sandbox publishes
brig network unpublish claude 8080    # close it again
brig network unpublish claude --all
```

`publish` opens a guest port on the host of a sandbox that is already up, or
records it for the next boot of one that is not. `--publish` on `brig run`
asks for the same thing at boot. `ls` lists the ports and whether each is open
right now.

`brig info` prints the ports too, as the `PORTS` row of the execution
envelope. `brig network ls` resolves no credentials, so it answers even when
a required secret is missing.

A port uses Docker's syntax:

| Written | Means |
| --- | --- |
| `3000` | host `127.0.0.1:3000` carries guest `3000` |
| `8080:80` | host `127.0.0.1:8080` carries guest `80` |
| `127.0.0.1:8080:80` | the same, with the address written out |
| `0.0.0.0:8080:80` | offered to the network this host is on |
| `5353:53/udp` | host `127.0.0.1:5353` carries guest `53` over UDP |

The host address defaults to `127.0.0.1`, so a published port is reachable
from this machine only. Use `0.0.0.0` to offer it to the network this
machine is on. The execution envelope shows the address on the row for that
port.

A publication belongs to the sandbox and outlives a run. `brig stop`
releases the host port and keeps the publication, so the next `brig run`
opens the same ports again. `brig network unpublish` removes one, and
`brig rm` removes all of them with the sandbox.

`unpublish` names a port by its host side. `brig network unpublish claude
8080` closes whatever host port `8080` carries. A value from the HOST column
of `brig network ls` works as printed:
`brig network unpublish claude 0.0.0.0:8080` closes the port on that address
only.

On macOS, publishing needs the `hvi` backend, where Brig owns the network
gateway. On `vz` and `qemu`, Brig refuses a run that publishes a port. On
Linux the container runtime publishes, and only when it creates the sandbox:
`brig run --publish` works, and `brig network publish` on a running sandbox
fails and tells you to remove the sandbox and run it again with `--publish`.

### `brig doctor`

```bash
brig doctor
```

`brig doctor` examines these items, one line each: the Brig build, the host,
the hypervisor, the runtime, the boot assets, cosign, the profile directory,
the secret store and brigd.

The first line is the build that `brig version` prints. `doctor` asks a
running brigd for its build. If that build differs from the `brig` binary,
`doctor` marks it `!!` and gives a restart as the fix. A daemon that stays up
across an upgrade still serves the old code.

```bash
brig doctor claude
```

With an agent, `doctor` adds the image check. When the profile of the agent
names a `runtimeBin`, the runtime and boot lines report on that binary and
not on the one on `PATH`. An unknown agent exits `3`.

```bash
brig doctor --json
```

`--json` prints the same checks in the [envelope](#--json-output), as
`kind: Doctor`. `data` is an array, and each entry has `name`, `state`
(`ok`, `!!` or `--`), `finding` and, when the check failed, `fix`.

Only two checks set the exit status. A missing or broken runtime exits `4`,
and a secret store that does not open exits `6`. Every other finding,
including one marked `!!`, prints its fix and leaves the exit status at `0`.

### `brig version`

```bash
brig version
```

`brig version` prints the version and, in parentheses, the build it came
from: the short commit, the commit date, the Go version and the platform.
`--version` is the same command.

```
brig v0.3.0 (91b0c7b, 2026-09-26, go1.26.0, darwin/arm64)
```

Go derives the version from the nearest tag when it builds the binary:

| Build | Version printed |
| --- | --- |
| a release | its tag |
| a commit after a tag | a pseudo-version that names the commit, such as `v0.3.1-0.20261002125255-d872cb5622d0`, with `+dirty` appended when the tree had uncommitted changes |
| a build with no git history behind it, such as one from a source tarball | `dev` and no commit |

`--json` prints the same build in the envelope, with the commit in full.
`commit` and `commitTime` are absent when the build carried no git history.

```bash
brig version --json
```

```json
{
  "apiVersion": "brig.sh/v1alpha1",
  "kind": "Version",
  "data": {
    "version": "v0.3.0",
    "commit": "91b0c7b81fa38eeeb519e9108ae3d1506e0db744",
    "commitTime": "2026-09-26T08:47:10Z",
    "modified": false,
    "goVersion": "go1.26.0",
    "os": "darwin",
    "arch": "arm64"
  }
}
```

### `brig completion`

```bash
brig completion zsh > "${fpath[1]}/_brig"
```

`brig completion` prints a completion script for `bash`, `zsh` or `fish` to
stdout. Brig installs nothing. See [Shell completion](completions.md) for
where each shell reads its script from, and what completes where.

### `brig agent`

```bash
brig agent ls
```

`brig agent ls` lists the agents that you can run.

```
brig agent ls
brig agent show <agent> [--json]
brig agent new <name> --from <agent> [--json] [--force]
brig agent edit <name>
brig agent rm <name> [-y]
brig agent import <file>
brig agent export <agent> [name] [--json] [--force]
```

Copy a built-in agent under your own name. Then edit the copy:

```bash
brig agent new mine --from claude
brig agent edit mine
```

A built-in agent has no file until `new` gives it one.

Print an agent, to read it or to pipe it:

```bash
brig agent show claude-code
```

`export` with a destination name does the same as `new --from`, with the
arguments in the opposite order:

```bash
brig agent export claude-code mine
```

Add a file that you wrote or received. `-` reads the file from stdin:

```bash
brig agent import mine.yaml
```

Delete an agent that has its own file. `rm` asks first, and `-y` answers in
advance:

```bash
brig agent rm mine
```

`rm` refuses while a sandbox of the agent exists, running or stopped. For
each sandbox, it names the `brig rm <ref>` to run first. `rm` also refuses
when Brig cannot ask the runtime of the agent, for example with an unknown
`BRIG_RUNTIME`.

`--force` (or `-f`) with `new` or `export` overwrites a destination file
that already exists. Without it, Brig refuses and names the file. `--json`
with `show`, `new` or `export` prints the document as JSON instead of YAML,
with no envelope. See [`--json` output](#--json-output).
[Profiles](profiles.md) is the reference for the file format.

### `brig policy`

```bash
brig policy ls
```

`brig policy ls` lists every policy, and what binds it.

```
brig policy ls
brig policy create <name> [--force]
brig policy edit <name> [--force]
brig policy show <name> [--json]
brig policy rm <name> [--force]
brig policy attach <policy> <agent> [-n <label>]
brig policy detach <policy> <agent> [-n <label>]
brig policy check <agent> [-n <label>]
```

Write a starter policy and open it in your editor:

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

See what is bound to a run, and whether Brig can enforce it:

```bash
brig policy check claude
```

[Policies](policies.md) covers the document format and what each network
posture means.

### `brig secret`

```bash
brig secret create gh-token
```

`brig secret create` reads the value from stdin and stores it under that
name in your keyring.

```
brig secret create <name> [-f FILE]
brig secret update <name> [-f FILE]
brig secret read <name>
brig secret delete <name> [-y]
brig secret ls
brig secret import <agent>
brig secret import <agent> <name>
```

Fill every secret that the profile of an agent declares, from your host,
once:

```bash
brig secret import claude-code
```

Fill one of them:

```bash
brig secret import claude-code gh-token
```

The value is never a command-line argument, so it never appears in `ps` or
in your shell history. See [Secrets](secrets.md) for the store, provenance
and the sources that a profile can declare.

### `brig telemetry`

```bash
brig telemetry status
```

`brig telemetry status` reports whether Brig sends usage data, what decided
the answer, and the install identifier the events of this machine carry.

```bash
brig telemetry off
```

`brig telemetry off` turns usage data off on this machine and records the
answer. `brig telemetry on` reverses it. See [Telemetry](telemetry.md) for
what is counted, what is never collected, and how the answer is stored.

## The ref

A ref is `<agent>` or `<agent>@<label>`. With no label, the ref names the
default session of the agent, so `claude` and `claude@refactor` are two
sessions of one agent. See [Sessions](sessions.md) for what a session keeps
separate, and what survives which command.

The separator is one `@`. Brig refuses these refs:

| Written | Refused because |
| --- | --- |
| `claude@@x` | more than one `@` |
| `@refactor` | no agent named before the `@` |
| `claude@` | a trailing `@` names no session. Drop it for the default session, or name one |
| `claude@Refactor` | the label has a character that Brig must change. Labels use lowercase letters, digits, dot, dash and underscore |

The label becomes part of the sandbox name and of the guest home directory,
and the two must agree.

## The run line

Brig reads three kinds of token from a `brig run <ref> [project] [args...]`
line. It identifies each one by position:

1. The first bare word is the ref.
2. On `run` and `plan`, the second bare word is a project directory. Brig
   mounts it read-write at `/work/<basename>` and starts the agent there.
   `plan` shows that run and does not start it.
3. The next bare word, or anything after `--`, is the agent's own argument.

The second bare word is the project even when no directory of that name
exists. If the word does not name an existing directory, Brig refuses the
run and tells you to put the word after `--`.

`--` ends Brig's parsing. No word after it is read as a project, and a
project named before it still counts.

Brig still reads its own flags after the ref and after the project:

```bash
brig run claude ~/code/demo --mem 4096 -d
```

## Flag placement

A Brig line has two positions for Brig's own flags. Global flags stand left
of the verb. Run-line flags stand between the verb and the first argument of
the agent.

| Position | Flags |
| --- | --- |
| Global | `--verbose`, `-q`/`--quiet`, `--json` |
| Run-line | `--image`, `--home`, `--mem`, `--cpus`, `--no-project`, `-d`/`--detach`, `--skills`, `--network`, `--offline`, `--publish`, `-q`/`--quiet` and `--json` |

The global position takes only those three flags. Brig refuses any other
flag there by name:

```
brig: unknown flag "--nope" before the command. brig takes a command first:
`brig run claude`, `brig ls`. If "--nope" is the agent's, it goes after the
profile
```

Both positions accept two flags:

| Flag | After the verb, on the run line |
| --- | --- |
| `-q`/`--quiet` | still works until v0.4.0, and prints one deprecation notice moving it left. See [Retired spellings](migration.md) |
| `--json` | a permanent peer spelling. No notice, either position |

```bash
brig info claude --json
brig --json info claude
```

Both print the same report.

Brig refuses by name an unrecognized flag that stands before the ref:

```
brig: unknown flag "--help" before the profile name. brig's own flags come
before the profile and the agent's after it; put "--help" after the profile
to pass it through, or -- to end brig's flags
```

The same flag after the ref reaches the agent untouched. After the first
argument of the agent, Brig stops reading its own flags. If Brig finds one
there, it prints a warning and passes the flag to the agent:

```bash
brig run claude -p hi --quiet
```

This command runs the agent with `-p hi --quiet`. Brig warns that `--quiet`
is a Brig flag and that here it goes to the agent.

### Run-line flags

| Flag | Value | Default | Notes |
| --- | --- | --- | --- |
| `--image IMAGE` | image ref | the agent's own | guest image to boot |
| `--home PATH` | host directory | `~/.brig/homes/<sandbox>` | mounted as the guest home. Replaces `--workspace`, see [migration.md](migration.md). The environment variable is still `BRIG_WORKSPACE`. There is no `BRIG_HOME` |
| `--mem MB` | number | the agent's own (`4096` for most shipped agents) | guest memory |
| `--cpus N` | number | the agent's own (`4` for most shipped agents) | guest vCPUs |
| `--no-project` | (none) | off | mount no project this run, even one this session ran with before. On any verb but `run` and `plan`, refused by name as a usage error |
| `-d`, `--detach` | (none) | off | start the sandbox, print its name and exit, without attaching. Only `run` acts on it. `sh`, `stop`, `rm`, `info` and `plan` accept it and ignore it |
| `--skills` | (none) | off | copy your own `~/.claude` skills and plugins into the guest home. The host copy is never written. Same as `BRIG_SKILLS=1` |
| `--network MODE` | `shared`, `isolated` or `offline` | the posture an existing sandbox was started with, then the profile's `network:`, then `isolated` (`shared` on `vz` or `qemu` when nothing names a posture) | the sandbox's network posture. A sandbox keeps its posture, so a verb without the flag does not change it. If Brig cannot establish the posture of an existing sandbox, name one with this flag or `BRIG_NETWORK`. See [policies.md](policies.md) |
| `--offline` | (none) | off | shorthand for `--network offline`: the agent runs with its guest home, and nothing leaves the sandbox. Refused together with a different `--network` value |
| `--publish PORT` | `3000`, `8080:80`, `127.0.0.1:8080:80`, `5353:53/udp` | nothing published | open a guest port on the host. Repeatable. Binds to `127.0.0.1` unless the address says otherwise. There is no `-p`, which stays the agent's flag. See [`brig network`](#brig-network) |

Some values in the table also have an
[environment variable](#environment-variables). The flag wins over the
variable, and the variable wins over the profile's field.

`--mem` and `--cpus` take a positive whole number. Brig refuses anything
else, including `0`.

## `--json` output

The verbs that accept `--json` depend on the position of the flag.

| Verb | Where `--json` is accepted | Shape |
| --- | --- | --- |
| `ls` | global, or local after `ls` | envelope |
| `info` | global, or local on the run line | envelope |
| `plan` | global, or local on the run line | envelope, `kind: Plan` |
| `doctor` | global, or local after `doctor` | envelope |
| `version` | global, or local after `version` | envelope |
| `network ls`, `network publish`, `network unpublish` | global, or local after the ref | envelope, `kind: Ports` |
| `run`, `sh` | global, or local on the run line | one compact line, see below |
| `agent ls` | global, or local after `ls` | envelope |
| `secret ls` | global, or local after `ls` | envelope |
| `agent show`, `agent export`, `agent new` | local only, after the subcommand | bare document, no envelope |
| `policy show` | local only, after the subcommand | bare document, no envelope |

- "Global" means left of the verb: `brig --json ls`.
- "Local" means after the subcommand, on either side of the operand of that
  subcommand. `brig agent show claude-code --json` and
  `brig agent show --json claude-code` both work.

Every verb that is not in the table refuses `--json` in both positions.

`agent` accepts `--json` in the global position only when its subverb is
`ls`. `policy` does not accept it there on any subverb. As a result, the
global position refuses `agent show` and `policy show`:

```
brig --json agent show claude-code
brig: `brig agent` has no --json output. --json is for the read verbs: ls,
info, plan, agent ls, secret ls, doctor, version and the network verbs (env
takes it too, but env is deprecated; prefer info), and for run and sh
```

Put the flag after the subcommand, where each of the two accepts it.

**Envelope shape.** A list or report verb prints
`{"apiVersion": "brig.sh/v1alpha1", "kind": "...", "data": ...}`. Within one
`apiVersion`, fields are added but never renamed or removed, so a script
written against it keeps parsing. No field carries a credential value.

**Bare shape.** `agent show`, `agent export`, `agent new` and `policy show`
print the document itself, with no envelope. Each is a file that Brig can
read back:

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

**The network verbs under `--json`.** All three print what the sandbox
publishes after the change that the command made, as `kind: Ports`:

```json
{
  "apiVersion": "brig.sh/v1alpha1",
  "kind": "Ports",
  "data": {
    "sandbox": "brig-claude-code",
    "ports": [
      {"host": "127.0.0.1:8080", "guest": 80, "protocol": "tcp", "live": true}
    ]
  }
}
```

`live` says whether the gateway forwards that port now. A recorded port that
is not live belongs to a sandbox that is not running. Brig opens the port
again when the sandbox starts.

When Brig cannot ask the runtime, `live` is `null` and the `STATE` column of
`brig network ls` reads `unknown`. Brig cannot ask the Linux runtime, so a
port there always reports `unknown`.

**`run` and `sh` under `--json`.** The agent runs as a child of Brig. After
the agent exits, Brig prints one compact JSON line with the outcome. It is
the last line of stdout:

```json
{"apiVersion":"brig.sh/v1alpha1","kind":"Run","data":{"ref":"claude","sandbox":"brig-claude-code","stage":"agent","exit":0}}
```

| Field | Meaning |
| --- | --- |
| `data.stage` | `"brig"`: Brig refused before the agent ran, and `data.error` carries the reason. `"agent"`: the agent ran. `"gui"`: a windowed agent. `"detached"`: applies under `-d` |
| `data.exit` | the agent's own exit status when `stage` is `"agent"`, and one of Brig's own exit codes otherwise |
| `data.signal` | the signal, when a signal killed the agent |

When Brig refuses under `--json`, it prints the `Run` object on stdout, where
every success case goes, and the usual error line on stderr. See
[Exit codes](#exit-codes) for how a script must read the exit status.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | success |
| `1` | a general failure |
| `2` | a usage error: an unknown flag, a stray argument, or a value in the wrong place, as reported by the verb's own parser |
| `3` | no such thing: an unknown agent, or a sandbox that is not there |
| `4` | no usable runtime: none installed, an unknown `BRIG_RUNTIME`, or `BRIG_RUNTIME_BIN` (or a profile's own `runtimeBin`) pointing at nothing. The refusal names the setting that caused it |
| `5` | a boot refused over image verification |
| `6` | a required secret was not resolved, or the secret store did not open. A declared secret marked `required: false` produces a warning and no failure |
| `7` | the runtime and backend cannot enforce a property bound to the sandbox, or cannot confirm they do. The refusal names the property, the runtime and the backend. See [Exit 7 cases](#exit-7-cases) |

[Stability](stability.md) lists this table as stable enough to script
against.

**Under `run --json` and `sh --json`, the agent's exit status becomes
Brig's.** An exit `3` from `brig --json run claude` can be the agent's own
`3` or Brig's "no such agent". Branch on the `data.stage` field of the
[`Run` object](#--json-output). `"agent"` means that the code is the
agent's, and anything else means that it is one of the classes in the table.

### Exit 7 cases

The one such property today is an egress policy. `brig run`, `brig sh` and
the retired spellings that stand for them refuse a policy with `7`:

- on hull's `vz` or `qemu`
- on docker
- on `hvi` with a gateway probe that fails or finds no `--egress-default`
- on nerdctl with an nft probe that fails
- on a hull backend that Brig holds no answer for

The code is `7` whether a flag, `BRIG_NETWORK` or the profile's `network:`
chose the posture.

`brig network publish` refuses with `7` on `vz`, `qemu`, docker and a
backend that Brig holds no answer for. It runs no gateway probe. On `hvi` it
records the port, and a failed probe refuses the next run with `7`.

Two refusals exit `1`. With no policy bound, the refusal of an isolated
network on `vz` or `qemu` is a different refusal. A refusal that Brig reaches
first also keeps its own code: a graphical profile on a backend with no
window exits `1`, under a policy or not.

<details><summary>Brig v0.3.0 and earlier</summary>

Those releases exit `1` for all of the exit `7` cases but the failed probe.
On a failed probe, those releases started the gateway with the rules anyway.
The run booted with the rules on the gateway, or exited `1` when the gateway
did not come up.

</details>

### Usage mistakes that exit 1

These usage mistakes exit `1` and not `2`:

- an unknown top-level command
- a run-line verb given no ref
- a missing subcommand on `agent`, `policy` or `secret`
- `secret import` or `policy check` given no agent
- a `--mem` or `--cpus` value that is not a positive whole number

```
brig nosuchverb
brig: unknown command "nosuchverb" (try `brig help`)
```

```
brig run
brig: run needs a profile, for example `brig run claude`. `brig agent ls`
lists them
```

Both exit `1`. `brig ls extra` and `brig completion bogus` exit `2`. A
script that looks for a usage mistake must test for a nonzero status.

A bare `brig telemetry` is not a usage mistake. It runs `status`.

## Environment variables

Brig reads most settings from `BRIG_<KEY>` and also from
`BRIG_<AGENT>_<KEY>`. When both are set, the agent-specific form wins, so
one shell can carry a different value for each agent. The agent name is in
upper case, with each dash changed to an underscore. For example,
`claude-code` reads `BRIG_CLAUDE_CODE_MEM` ahead of `BRIG_MEM`.

Some settings have no per-agent form: the directories, the runtime choice,
the boot-asset and gateway paths, and `BRIG_ENV_ARGV`. The tables mark each
one "global only".

### Sandbox and profile locations

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_WORKSPACE` | `~/.brig/homes/<sandbox>` | host directory mounted as the guest home. A named session appends `-<slug>` to one you set. Brig deletes the default one on `brig rm`, and never deletes one you set. A sandbox that a release before 0.3.0 started on `~/brig/<agent>` keeps that home, and Brig does not delete it |
| `BRIG_NAME` | `brig-<agent>` | the sandbox's own name. Must begin with `brig-`, or `brig ls` and `brig rm --all` cannot find it. A named session appends `-<slug>` |
| `BRIG_PROFILE_DIR` (global only) | `$XDG_CONFIG_HOME/brig`, which is `~/.config/brig` when that is unset | where your own agent files live. `BRIG_TEMPLATE_DIR` still works until v0.4.0 |
| `BRIG_POLICY_DIR` (global only) | `$XDG_CONFIG_HOME/brig/policies` | where policy files live |
| `BRIG_STATE_DIR` (global only) | `~/.brig` | where Brig keeps what has to outlive one command, including the project each sandbox last ran with |

### Guest resources and network

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_IMAGE` | the agent's own | guest image to boot |
| `BRIG_PULL` | `missing` | `missing` pulls only when the image is not already on the host. `always` re-pulls every run. `never` refuses to boot an image that is not already there |
| `BRIG_MEM` | the agent's own | guest memory, MB. A value that is not a positive whole number is ignored |
| `BRIG_CPUS` | the agent's own | guest vCPUs. A value that is not a positive whole number is ignored |
| `BRIG_READY_TIMEOUT` | `30` | seconds to wait for the in-guest agent once the runtime reports the sandbox running |
| `BRIG_NETWORK` | the posture an existing sandbox was started with, then the profile's `network:`, then `isolated` (`shared` on `vz` or `qemu`) | `shared`, `isolated` or `offline`. Wins over the posture an existing sandbox was started with. The `shared` fallback applies only when nothing names a posture, and `brig info` reports it. An unrecognized value refuses the run. See [policies.md](policies.md) |
| `BRIG_SKILLS` | `0` | `1` copies your own `~/.claude` skills and plugins into the guest home. Same as `--skills` |
| `BRIG_FORWARD_ENV` | (unset) | a space-separated list of environment variable names to carry into the guest, read live on every run |
| `BRIG_TITLE` | the agent's own | window title for a graphical agent |

`BRIG_FORWARD_ENV` replaces only the profile's `env:` bindings that use the
singular `ref: env.<name>` form. A binding that names its source through a
`refs:` chain stays, even when the chain includes an `env.` entry.

If the list names a variable that the profile already binds from another
source, such as `secrets.` or a literal `value:`, the profile's binding wins.
Brig ignores that name and warns about it.

### Credentials and Git

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_ALLOW_REFS` | `0` | `1` forwards a value that still looks like an unresolved `scheme://` secret reference |
| `BRIG_ALLOW_DENIED` | `0` | `1` forwards a variable on the agent's own billing denylist |
| `BRIG_GIT_CONFIG` | `0` | `1` writes a credential helper and gitconfig into the guest, routing an SSH GitHub remote over HTTPS |
| `BRIG_GIT_HOSTS` | `github.com` | space-separated hosts the forwarded token applies to |
| `BRIG_GIT_USER` | resolved on the host | username paired with the forwarded token |
| `BRIG_GIT_IDENTITY` | `1` | `0` stops Brig from forwarding the host commit identity resolved from the invoking directory |
| `BRIG_GIT_NAME`, `BRIG_GIT_EMAIL` | the host's `git config` | override that identity |
| `BRIG_TRUST_WORKSPACE` | `1` | pre-answers the agent's own "do you trust this folder" question for the directory a run starts in |
| `BRIG_ENV_ARGV` (global only) | (unset) | `1`, and no other value, puts a forwarded value on the runtime's own command line, where `ps` can read it. It never applies to a value Brig resolved itself, such as a stored secret |

`BRIG_SKILLS`, `BRIG_GIT_CONFIG`, `BRIG_TRUST_WORKSPACE`, `BRIG_ALLOW_REFS`
and `BRIG_ALLOW_DENIED` accept `1`, `true`, `yes` or `on`, and `0`, `false`,
`no` or `off`. Any other value refuses the run.

[Authentication](authentication.md) and [Secrets](secrets.md) cover what
each of these does with the credential in the guest.

### Image verification

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_VERIFY` | `warn` | `warn`, `require` or `off`. `strict` is an alias for `require`, and `none` and `0` both alias `off`. An unrecognized value refuses the run |
| `BRIG_VERIFY_REGISTRY` | `ghcr.io/brig-sh/` | image prefix treated as Brig's own, so a signature is expected |
| `BRIG_VERIFY_IDENTITY` | Brig's own community-images build workflow | certificate identity regexp cosign must match |
| `BRIG_VERIFY_ISSUER` | GitHub Actions OIDC | certificate OIDC issuer |
| `BRIG_VERIFY_RUNTIME_IDENTITY` | the Linux runtime bundle's release workflow, on a tag | certificate identity regexp the runtime bundle's signed record must match. Set it for a bundle released from a fork, as `INSTALL_BRIG_SIG_IDENTITY` is set for its installer |
| `BRIG_VERIFY_RUNTIME_ISSUER` | GitHub Actions OIDC | certificate OIDC issuer for that record |
| `BRIG_COSIGN_BIN` | `cosign` on `PATH` | path to the cosign binary |

`warn` reports an image that it cannot verify and boots the image. It also
stops to ask about an image that claims to be Brig's own and is not.
`require` refuses to boot anything it cannot positively verify, including
when cosign is missing. See [Security](security.md) for what verification
does and does not catch.

### Runtime and hypervisor

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_RUNTIME` (global only) | `hull` on macOS, `nerdctl` on Linux | which runtime to drive |
| `BRIG_RUNTIME_BIN` (global only) | `hull` on the `hull` runtime, `nerdctl` then `docker` on the `nerdctl` runtime, each found on `PATH` | path to that binary. `BRIG_RUNTIME` picks the runtime first, and this only overrides its executable |
| `BRIG_HYPERVISOR` | the agent's own `hypervisor:` field, else `vz` | macOS only: `vz`, `hvi` or `qemu`. Wins over the agent's own field when set |
| `BRIG_ROOTFS_TYPE` | the agent's own `rootfsType:` field | `block`, `virtiofs` or `9pfs`, how the guest root reaches the microVM under `hull`. `nerdctl` ignores it. A profile's own `rootfsType:` outside that set is refused when the profile loads, but this variable is passed to `hull` unchecked |
| `BRIG_CONTAINERD_RUNTIME` (global only) | `io.containerd.urunc.v2` | Linux only, on the `nerdctl` runtime: the containerd shim that boots the sandbox as a microVM. Another microVM shim is accepted. A shim Brig knows shares the host kernel (`runc` or `crun`, by name or by path, or a shim name containerd resolves to the runc shim, such as `io.containerd.runc.v2`) is refused |

On macOS, `hvi` is the only backend that enforces an attached egress policy
or `--network isolated`. It needs macOS 15 or newer. Linux also supports the
isolated posture, and enforces egress policies with nerdctl. `vz` is the
only backend with a graphical console. See [Runtimes](runtimes.md).

<details><summary>macOS 14</summary>

For the built-in `hvi` profiles, set `BRIG_HYPERVISOR=vz` and
`BRIG_NETWORK=shared`. Brig refuses an `hvi` run on macOS 14, and `vz` cannot
satisfy the isolated posture of those profiles.

</details>

### Boot assets and the network gateway

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_BOOT_ASSETS` (global only) | on macOS, wherever `hull assets dir` says (`~/.hull/assets` if hull cannot answer). On Linux, `$XDG_DATA_HOME/brig/assets` (`~/.local/share/brig/assets` when that is unset) | directory holding the host kernel and initrd a `genericBoot` agent needs |
| `BRIG_BOOT_ASSETS_REF` (global only) | `ghcr.io/nofireai/hull-assets:<os>-<arch>` | the bundle Brig fetches when the boot assets are missing |
| `BRIG_GATEWAY_SOCK` (global only) | `<gateway dir>/gateway-<subnet>.sock` | control socket of the shared network gateway. Names matching `sandbox-*.sock` are reserved for isolated gateways and refused here |
| `BRIG_GATEWAY_DIR` (global only) | the directory of `BRIG_GATEWAY_SOCK`, else `~/.brig` | where gateway sockets, logs and network records live, shared and per-sandbox alike |

