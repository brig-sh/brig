# Command line reference

Every Brig verb and flag, the environment variables, the JSON output and the
exit codes. For a walkthrough of a first run, see
[quickstart.md](quickstart.md).

This page teaches only the current spellings. Most retired spellings still
work until v0.4.0. [migration.md](migration.md) has the old-to-new table and
the exceptions.

## Everyday commands

The nine commands you type most.

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

Starts the sandbox if it is not already running, then runs the agent inside
it.

```
brig run <ref> [project] [args...]
```

- `<ref>` names the agent, and, with `@<label>`, a session of its own. See
  [The ref](#the-ref) below.
- `[project]` is a host directory. Brig mounts it read-write at
  `/work/<name>` and starts the agent there. See
  [The run line](#the-run-line-ref-project-and-the-agents-own-arguments).
- `[args...]` reach the agent untouched.

With no project named, Brig remounts the one this session ran with last:

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

To pass the agent an argument that Brig would otherwise read, put it after
`--`, which ends Brig's parsing:

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
brig sh <ref> -c '<script>' [args...]
```

`sh` takes no project argument. The first bare word after the ref starts the
guest command:

```bash
brig sh claude ls /work
```

Each word after the ref is one argument to the guest command, passed the way
you typed it, and the command runs under a login shell for its environment.
The words are not parsed again inside the guest, so a pipe, a `;` or a glob
is not shell syntax there. For a script, put `-c` in front of it, the way
`sh -c` takes one. The login shell parses it in the guest, so `~` is the
guest home and `$HOME` the guest's:

```bash
brig sh claude -c 'ls /work | wc -l'
brig sh claude -c 'cat ~/.claude/settings.json'
```

Words after the script are its `$0`, `$1` and on, as with `sh -c`. The flags
that go in front of `-c` with `sh -c` work here too: `-ec` stops at the first
failing command, `-xc` traces, `-uc` refuses unset variables. They cover the
script, not the login profile that runs before it. `-c` with no
script, or an empty one, is refused before the sandbox starts. An
unquoted `~` in the plain form is expanded by your own shell before brig sees
it, so it names your home on the host, not the guest's.

A single word with a space or a shell operator in it, such as
`brig sh claude 'ls | wc -l'`, is looked up as a command by that name. brig
prints a hint naming `-c` before it runs.

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

Stops the sandbox and removes it. When Brig created the guest home, because
the run named no `--home` or `BRIG_WORKSPACE`, `rm` deletes it too and prints
its path. A guest home you named and your project are host directories
Brig only mounted, so neither is touched, and `rm` prints the path of each
one it left behind.

If the runtime reports no sandbox for the ref, for example after `hull rm`,
`rm` exits `3`, says so, and forgets the session Brig still had for it. The
next run of that ref starts a new session, on the network a new session
gets rather than the one the old sandbox had. A guest home Brig created
stays until that run, which deletes it, and `rm` prints its path.
`--dry-run` says that `rm` would forget the session, and forgets nothing.
If Brig cannot rewrite its session index, `rm` exits `1` and keeps the
session as it was.

If the sandbox is removed but its guest home cannot be deleted, `rm` exits
`1` and names the home. The sandbox stays removed, and the next run of the
session deletes what is left of the home before it boots.

```bash
brig rm --all
```

Stops and removes every sandbox Brig has. Before removing anything, it lists
each sandbox as its ref, sandbox name and state, one per line, and asks for
confirmation. If stdin is not a terminal, the command refuses, removes
nothing, and exits `1`. Pass `-y` (or `--yes`) to confirm in advance, for
example in a script. If there is nothing to remove, the command asks nothing
and exits `0`.

It also stops the shared network gateway that `hvi` sandboxes use, once no
sandbox is on it. It checks the whole host first, so a sandbox of another
session, or one still booting, keeps the gateway running.

```bash
brig rm --all --dry-run
brig rm claude --dry-run
```

`--dry-run` prints what the command would remove and exits `0` without
removing anything. For `rm --all` this is the same list the prompt shows.
For `rm <ref>` it is the one sandbox, and the guest home it would delete or
leave. A ref with no sandbox still exits `3`. `brig rm <ref>` asks no
question, so it refuses `-y`.

`rm` and `stop` each take exactly one ref.

### `brig ls`

```bash
brig ls
```

Lists every sandbox Brig knows about, one row each: `REF`, `SANDBOX` (the
runtime's name for it), `STATE` and `WORKSPACE` (the guest home).

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
| `--raw` | keep terminal control sequences, which Brig strips by default |
| `--gateway` | read the network gateway's log instead of the sandbox's own |

With no ref, `--gateway` reads the log of the gateway that `shared`
sandboxes use. With a ref, it reads that sandbox's own gateway log. Only an
isolated sandbox has one, which includes a new default `hvi` sandbox and one
carrying a policy. For any other sandbox, the command exits `3`.

```bash
brig logs --gateway
brig logs --gateway claude
```

### `brig info`

```bash
brig info claude
```

Prints the execution envelope without booting anything. The envelope has the
sandbox name, the isolation, the guest home, the image, the verification
mode, the network, every published port and the credentials by name. The
network row gives the posture of the running sandbox, and the posture of its
next boot when the two differ. `brig --verbose run` prints the same envelope
before it boots.

A required secret that cannot be resolved fails `info` with exit `6`. A
declared secret marked `required: false` prints a warning, and the command
still exits `0`.

### `brig plan`

```bash
brig plan claude
brig plan claude ~/src/demo
brig plan claude@refactor --json
```

Prints the permissions of a run without booting anything and without
opening a secret. The plan has the guest home and the project with
their mode and guest path, the host directories `--skills` copies into the
guest home, the network posture, every published port, every bound policy,
the merged egress rules, the limits, the image, the verification mode, the
runtime and its backend, and every credential with how it reaches the guest
and where its value comes from.

`plan` reads the run line the way `brig run` does: a project after the ref,
`--no-project`, and the run-line flags that shape a run. So the first
`brig run claude ~/src/demo` can be planned before it happens. A project
directory that `brig run` would refuse to mount fails the plan the same way.

When `brig run` would refuse the run before it starts anything, the plan
opens with a `REFUSED` row that carries the run's own message, and `--json`
carries the same message as `refused.reason`. The plan sees these refusals:

- a remembered project reached through a symlink;
- a sandbox the runtime holds whose network brig cannot determine;
- a hypervisor this host cannot boot, such as `hvi` on macOS 14;
- what the backend cannot honor: a policy or a published port it cannot
  enforce, an isolated network on `vz` or `qemu`, or a graphical profile on
  a backend with no display.

The plan still prints, and `brig plan` still exits `0`, so a script reads
`refused` rather than the exit code. With no runtime on PATH the backend is
not asked, and `runtime.available` is `false`. A required secret missing from
the store also stops the run, with exit `6`. The plan marks that credential
`unresolved` and `required` rather than `refused`.

Some refusals are not in the plan, because the plan does not ask:

- A session name another session already owns. The run checks it after the
  first two checks above and before the last two, and the check writes the
  name down, which a plan must not do. When a name collides and one of the
  last two checks would refuse too, the run reports the collision and the
  plan reports the other refusal.
- Refusals at boot: an image that fails verification, and, on `hvi` under a
  policy, a network gateway that does not confirm it can enforce the policy.
  brig asks the runtime about the gateway only when it starts one.

It reads the secret store the way `brig secret ls` does, by name. A secret
missing from the store is marked `unresolved`, and the command still exits
`0`. A store it cannot list marks a credential `unknown` when nothing later in
its chain has a value. A credential the run drops at the denylist, or an
unresolved secret-manager reference in the shell, is marked `withheld`.

A listing reads names, not values, so a credential `resolved` from the store
means the store lists the name, and nothing more. The run reads the value,
and that read can still fail where the listing did not:

- A value another tool emptied. brig refuses to store an empty value, so
  only another tool leaves one. The run does not forward it. It moves on to
  the next source in the chain, or, when the secret is the last one, prints
  a warning that names it.
- A damaged item, such as a macOS keychain item whose sealed half is gone.
- A keychain or keyring prompt you deny.
- A Linux keyring collection that is locked.

On the last three, the run stops with exit `6` for a required secret and
drops an optional one with a warning.

With no policy bound, `POLICY` reads `(none)` and `noPolicy` is `true`.

The last row is a digest of the plan. It is `sha256:` over the `data` object
with `digest` set to the empty string, in the compact encoding Go's
`encoding/json` writes: the fields in the order of the table below, no
whitespace, non-ASCII text as UTF-8, and `<`, `>` and `&` escaped as
`\u003c`, `\u003e` and `\u0026`. Every list in it is sorted, so the digest is
the same for the same run and changes when any field does. To check one,
encode `data` that way and hash it. Python's
`json.dumps(data, separators=(",", ":"), ensure_ascii=False)` gives the same
bytes when no string in it holds `<`, `>`, `&`, U+2028 or U+2029.

The digest covers every field, the sentences included: `isolation`,
`network`, `refused.reason`, each credential's `reason`, and `runtime.bin`. A
release that rewords one of them moves the digest with no permission
changed.

`brig plan --json` prints `kind: Plan`. Its `network`, `image.ref` and
`image.pull` are the same values `brig info --json` prints. Its
`argvExposed` has the same names as info's, sorted, where info keeps the
order the run adds them in: the order of a profile's bindings is not a
permission, so it stays out of the digest. The `data` fields, in order:

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
| `posture` | string | `shared`, `isolated` or `offline` for the next boot, or `unknown` when the run refuses a sandbox whose network brig cannot determine |
| `ports` | list of strings | every published port |
| `policies` | list of strings | every bound policy |
| `noPolicy` | bool | `true` when no policy is bound |
| `egress` | object, omitted when no policy is bound | `default` (`allow` or `deny`), and the `allow` and `deny` lists. Each rule has a `host` or a `cidr` |
| `credentials` | list | one entry per credential, below |
| `argvExposed` | list of strings, omitted when empty | the variables whose values `BRIG_ENV_ARGV=1` puts on the runtime's command line, by name, sorted |
| `limits` | object | `mem` in MB and `cpus` |
| `digest` | string | `sha256:` and 64 hex digits |

A mount is `host`, `guest` and `mode`. `mode` is `read-write` for the home
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

Checks, one line each: the Brig build, the host, the hypervisor, the
runtime, the boot assets, cosign, the profile directory, the secret store and
brigd. The first line is the build `brig version` prints, so a pasted report
says which binary produced it. Doctor asks a running brigd which build it
is. One that differs from the `brig` binary is marked `!!` with a restart as
its fix, because a daemon left up across an upgrade still serves the old
code.

```bash
brig doctor claude
```

Adds the image check, which needs an agent. When the agent's profile names
a `runtimeBin`, the runtime and boot lines report on that binary instead of
the one on `PATH`. An unknown agent exits `3`.

```bash
brig doctor --json
```

Prints the same checks in the [envelope](#--json-output), as
`kind: Doctor`. `data` is an array, and each entry has `name`, `state`
(`ok`, `!!` or `--`), `finding` and, when the check failed, `fix`.

Only two checks set the exit status: a missing or broken runtime exits `4`,
and a secret store that will not open exits `6`. Every other finding,
including one marked `!!`, prints its fix and leaves the exit status at `0`.

### `brig version`

```bash
brig version
```

Prints the version and, in parentheses, the build it came from: the short
commit, the commit date, the Go version and the platform. `--version` is the
same command.

```
brig v0.3.0 (91b0c7b, 2026-09-26, go1.26.0, darwin/arm64)
```

The version is what Go derived from the nearest tag when the binary was
built. A release prints its tag. A build from a commit after a tag prints a
pseudo-version naming that commit, such as
`v0.3.1-0.20261002125255-d872cb5622d0`, with `+dirty` appended when the tree
had uncommitted changes. A build with no git history behind it, such as one
from a source tarball, prints `dev` and no commit.

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

Prints a completion script for `bash`, `zsh` or `fish` to stdout. Brig
installs nothing. See [completions.md](completions.md) for where each shell
reads its script from, and what completes where.

### `brig agent`

```bash
brig agent ls
```

Lists the agents you can run.

```
brig agent ls
brig agent show <agent> [--json]
brig agent new <name> --from <agent> [--json] [--force]
brig agent edit <name>
brig agent rm <name> [-y]
brig agent import <file>
brig agent export <agent> [name] [--json] [--force]
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

`export` with a destination name does what `new --from` does, with the
arguments the other way round:

```bash
brig agent export claude-code mine
```

Add a file you wrote or received. `-` reads it from stdin:

```bash
brig agent import mine.yaml
```

Delete an agent that has a file of its own. `rm` asks first, and `-y`
answers in advance:

```bash
brig agent rm mine
```

`rm` refuses while a sandbox of the agent exists, running or stopped, and
names the `brig rm <ref>` to run first for each. It also refuses when Brig
cannot ask the agent's runtime, such as an unknown `BRIG_RUNTIME`.

`--force` (or `-f`) with `new` or `export` overwrites a destination file that
already exists. Without it, Brig refuses and names the file. `--json` with
`show`, `new` or `export` prints the document as JSON instead
of YAML, with no envelope. See [`--json` output](#--json-output) below.
[profiles.md](profiles.md) is the reference for the file format.

### `brig policy`

```bash
brig policy ls
```

Lists every policy, and what binds it.

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

Confirm what is bound to a run, and whether Brig can enforce it:

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
brig secret delete <name> [-y]
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
in your shell history. See [secrets.md](secrets.md) for the store,
provenance and the sources a profile can declare.

### `brig telemetry`

```bash
brig telemetry status
```

Reports whether usage data is sent, and what decided the answer.

```bash
brig telemetry off
```

Turns it off on this machine and records the answer. `brig telemetry on`
reverses it. See [telemetry.md](telemetry.md) for what is counted, what is
never collected, and how the answer is stored.

## The ref

A ref is `<agent>` or `<agent>@<label>`. With no label, the ref names the
agent's default session, so `claude` and `claude@refactor` are two sessions
of one agent. See [sessions.md](sessions.md) for what a session keeps
separate, and what survives which command.

The separator is exactly one `@`. Brig refuses these refs:

| Written | Refused because |
| --- | --- |
| `claude@@x` | more than one `@` |
| `@refactor` | no agent named before the `@` |
| `claude@` | a trailing `@` names no session. Drop it for the default session, or name one |
| `claude@Refactor` | the label has a character Brig would have to change. Labels use lowercase letters, digits, dot, dash and underscore |

The label becomes part of the sandbox name and of the guest home directory,
and the two must agree. That is why Brig refuses a label it would have to
change.

## The run line: ref, project, and the agent's own arguments

`brig run <ref> [project] [args...]` reads three kinds of token from one
line and tells them apart by position:

1. The first bare word is the ref.
2. On `run` and `plan`, the second bare word is a project directory. Brig
   mounts it read-write at `/work/<basename>` and starts the agent there.
   `plan` shows the run that would.
3. The next bare word, or anything after `--`, is the agent's own argument.

The second bare word is the project whether or not a directory of that name
exists. If it does not exist, or is not a directory, Brig refuses the run
and tells you to put the word after `--`.

`--` ends Brig's parsing. No word after it is read as a project, and a
project named before it still counts:

```bash
brig run claude ~/code/demo -- --version
```

Brig still reads its own flags after the ref and after the project:

```bash
brig run claude ~/code/demo --mem 4096 -d
```

Only `run` and `plan` take a project. `brig sh claude ~/code/demo` reads
`~/code/demo` as the start of the guest command.

## Flag placement

A Brig line has two positions for Brig's own flags. Global flags stand left
of the verb. Run-line flags stand between the verb and wherever the agent's
own arguments begin.

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

Two flags are accepted in both positions:

| Flag | After the verb, on the run line |
| --- | --- |
| `-q`/`--quiet` | still works until v0.4.0, and prints one deprecation notice moving it left. See [migration.md](migration.md) |
| `--json` | a permanent peer spelling. No notice, either position |

```bash
brig info claude --json
brig --json info claude
```

Both print the same report.

Brig refuses an unrecognized flag before the ref by name, because there is
no agent yet to hand it to:

```
brig: unknown flag "--help" before the profile name. brig's own flags come
before the profile and the agent's after it; put "--help" after the profile
to pass it through, or -- to end brig's flags
```

The same flag after the ref reaches the agent untouched. Once the agent's
arguments have begun, Brig stops reading its own flags too. It warns about
one it finds there and passes it on:

```bash
brig run claude -p hi --quiet
```

This runs the agent with `-p hi --quiet` and warns that `--quiet` is one of
Brig's own flags but here is the agent's.

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

For every value above that also has an
[environment variable](#environment-variables), the flag wins over the
variable, and the variable wins over the profile's field.

`--mem` and `--cpus` take a positive whole number. Brig refuses anything
else, including `0`.

## `--json` output

Which verbs accept `--json` depends on where the flag stands.

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
info, plan, agent ls, secret ls, doctor, version and the network verbs (env
takes it too, but env is deprecated; prefer info), and for run and sh
```

Put the flag after `agent show` instead. Every verb not listed in the table
above refuses `--json` in both positions.

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
publishes, after whatever the command changed, as `kind: Ports`:

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

`live` says whether the gateway is forwarding that port now. A recorded port
that is not live belongs to a sandbox that is not running, and Brig opens it
again when the sandbox starts.

`live` is `null`, and the `STATE` column of `brig network ls` reads
`unknown`, when Brig cannot ask the runtime. Brig cannot ask the Linux
runtime, so a port there always reports `unknown`.

**`run` and `sh` under `--json`.** The agent runs as a child of Brig. After
it exits, Brig prints one compact JSON line with the outcome, the last line
of stdout:

```json
{"apiVersion":"brig.sh/v1alpha1","kind":"Run","data":{"ref":"claude","sandbox":"brig-claude-code","stage":"agent","exit":0}}
```

`data.stage` is one of four values. `"brig"` means Brig refused before the
agent ran, and `data.error` carries the reason. `"agent"` means the agent
ran. `"gui"` means a windowed agent. `"detached"` applies under `-d`.
`data.exit` is the agent's own exit status when `stage` is `"agent"`, and
one of Brig's own exit codes otherwise. `data.signal` names the signal when
the agent was killed by one. See [Exit codes](#exit-codes) for how a script
should read them.

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
| `7` | the runtime and backend cannot enforce a property bound to the sandbox, or cannot confirm they do. The one such property today is an egress policy: on hull's `vz` or `qemu`, on nerdctl or docker, on `hvi` with a gateway probe that fails or finds no `--egress-default`, or on a hull backend Brig holds no answer for. `brig run` and `brig sh` refuse it with `7`, and so do the retired spellings that stand for them. That holds whether a flag, `BRIG_NETWORK` or the profile's `network:` chose the posture. `brig network publish` refuses with `7` on `vz`, `qemu`, nerdctl, docker and a backend Brig holds no answer for. It runs no gateway probe, so on `hvi` it records the port, and a failed probe refuses the next run with `7`. The refusal names the property, the runtime and the backend. Brig v0.3.0 and earlier exit `1` for all of these but the failed probe. On a failed probe, those releases started the gateway with the rules anyway: the run booted with the rules on the gateway, or exited `1` when the gateway did not come up. With no policy bound, the refusal of an isolated network on `vz` or `qemu` is a different one, and it exits `1`. A refusal Brig reaches first keeps its own code: a graphical profile on a backend with no window exits `1`, under a policy or not |

`script/smoke.sh` and `cmd/brig/exit_test.go` assert this table, and
[stability.md](stability.md) lists it as stable enough to script against.

**6 is for required secrets only.** A declared secret marked
`required: false` produces a warning and no failure. `claude-code`'s two
secrets are both optional, so `brig info claude` with neither one set exits
`0`.

**Under `run --json` and `sh --json`, the agent's exit status becomes
Brig's.** An exit `3` from `brig --json run claude` can be the agent's own
`3` or Brig's "no such agent". Branch on the `Run` object's `data.stage`
field: `"agent"` means the code is the agent's, and anything else means it
is one of the classes in the table.

When Brig refuses under `--json`, it prints the `Run` object on stdout, where
every success case goes, and the usual error line on stderr.

**Some usage mistakes exit `1` instead of `2`:**

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

Both exit `1`, while `brig ls extra` and `brig completion bogus` exit `2`.
A script that looks for a usage mistake should test for a nonzero status.

A bare `brig telemetry` is not a usage mistake. It runs `status`.

## Environment variables

Most settings below are read through `BRIG_<KEY>` and also honor
`BRIG_<AGENT>_<KEY>`. The agent-specific form wins when both are set, so one
shell can carry a different value per agent. The agent name is upper-cased
with dashes turned to underscores, so `claude-code` reads
`BRIG_CLAUDE_CODE_MEM` ahead of `BRIG_MEM`.

Some settings have no per-agent form: the directories, the runtime choice,
the boot-asset and gateway paths, and `BRIG_ENV_ARGV`. Each is marked
"global only" below.

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
`refs:` chain stays, even when the chain includes an `env.` entry. If the
list names a variable the profile already binds from somewhere else, such as
`secrets.` or a literal `value:`, the profile's binding wins. Brig ignores
that name and warns about it.

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
| `BRIG_ENV_ARGV` (global only) | (unset) | exactly `1` puts a forwarded value on the runtime's own command line, where `ps` can read it. It never applies to a value Brig resolved itself, such as a stored secret |

`BRIG_SKILLS`, `BRIG_GIT_CONFIG`, `BRIG_TRUST_WORKSPACE`, `BRIG_ALLOW_REFS`
and `BRIG_ALLOW_DENIED` accept `1`, `true`, `yes` or `on`, and `0`, `false`,
`no` or `off`. Any other value refuses the run.

[authentication.md](authentication.md) and [secrets.md](secrets.md) cover
what each of these does with the credential once it is in the guest.

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

`warn` reports an unverifiable image and boots anyway, and also stops to ask
about an image that claims to be Brig's own and is not. `require` refuses to
boot anything it cannot positively verify, cosign missing included. See
[security.md](security.md) for what verification does and does not catch.

### Runtime and hypervisor

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_RUNTIME` (global only) | `hull` on macOS, `nerdctl` on Linux | which runtime to drive |
| `BRIG_RUNTIME_BIN` (global only) | `hull` on the `hull` runtime, `nerdctl` then `docker` on the `nerdctl` runtime, each found on `PATH` | path to that binary. `BRIG_RUNTIME` picks the runtime first, and this only overrides its executable |
| `BRIG_HYPERVISOR` | the agent's own `hypervisor:` field, else `vz` | macOS only: `vz`, `hvi` or `qemu`. Wins over the agent's own field when set |
| `BRIG_ROOTFS_TYPE` | the agent's own `rootfsType:` field | `block`, `virtiofs` or `9pfs`, how the guest root reaches the microVM under `hull`. `nerdctl` ignores it. A profile's own `rootfsType:` outside that set is refused when the profile loads, but this variable is passed to `hull` unchecked |
| `BRIG_CONTAINERD_RUNTIME` (global only) | `io.containerd.urunc.v2` | Linux only, on the `nerdctl` runtime: the containerd shim that boots the sandbox as a microVM. Another microVM shim is accepted; a shim Brig knows shares the host kernel (`runc` or `crun`, by name or by path, or a shim name containerd resolves to the runc shim, such as `io.containerd.runc.v2`) is refused |

On macOS, `hvi` is the only backend that enforces an attached egress policy
or `--network isolated`, and it needs macOS 15 or newer. Linux also supports
the isolated posture, but refuses egress policies. `vz` is the only backend
with a graphical console. On macOS 14, set `BRIG_HYPERVISOR=vz` and
`BRIG_NETWORK=shared` for the built-in `hvi` profiles: Brig refuses an
`hvi` run there, and `vz` cannot satisfy those profiles' isolated posture.
See [runtimes.md](runtimes.md).

### Boot assets and the network gateway

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_BOOT_ASSETS` (global only) | on macOS, wherever `hull assets dir` says (`~/.hull/assets` if hull cannot answer); on Linux, `$XDG_DATA_HOME/brig/assets` (`~/.local/share/brig/assets` when that is unset) | directory holding the host kernel and initrd a `genericBoot` agent needs |
| `BRIG_BOOT_ASSETS_REF` (global only) | `ghcr.io/nofireai/hull-assets:<os>-<arch>` | the bundle Brig fetches when the boot assets are missing |
| `BRIG_GATEWAY_SOCK` (global only) | `<gateway dir>/gateway-<subnet>.sock` | control socket of the shared network gateway. Names matching `sandbox-*.sock` are reserved for isolated gateways and refused here |
| `BRIG_GATEWAY_DIR` (global only) | the directory of `BRIG_GATEWAY_SOCK`, else `~/.brig` | where gateway sockets, logs and network records live, shared and per-sandbox alike |

## See also

[migration.md](migration.md) lists every retired verb, subverb, flag,
position and profile key, and what replaces each one.
[stability.md](stability.md) says which parts of this page you can write a
script against.
