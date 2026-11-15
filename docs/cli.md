# Command line reference

For a walkthrough of a first run, see [Quickstart](quickstart.md).

v0.5.0 removed the retired spellings, with two exceptions.
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
| `brig report claude` | writes a diagnostic report to attach to an issue |
| `brig network publish claude 3000` | opens the agent's port 3000 on `localhost:3000` |

## Help

Every verb answers `--help`, `-h` and `brig help <verb>` with its own usage,
printed to stdout with a zero exit. The two spellings print the same text:

```bash
brig run --help
brig help run
```

`brig help` alone prints the command list and the flags the verbs share. A
word after the ref is still the agent's, so `brig run claude --help` passes
`--help` to the agent.

## Verbs

### `brig run`

```bash
brig run claude ~/code/demo
```

`brig run` starts the sandbox if it is not running, then runs the agent in
it.

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
brig run -d claude ~/code/demo
```

Run with no project mounted, even one that this session ran with before:

```bash
brig run --no-project claude
```

If Brig reads an argument as its own, put the argument after `--` to pass it
to the agent:

```bash
brig run claude ~/code/demo -- --version
```

#### Run a Brigfile target

```bash
brig run --target dev
brig run --target dev --session fix -- --continue
brig run --target ci -f ci/Brigfile.ci --approve sha256:<digest>
brig run
```

```
brig run --target <name> [--session <name>] [-f <path>] [flags] [-- args...]
```

- The project is the directory that holds the [Brigfile](brigfile.md). The
  line takes no ref and no project.
- The arguments after `--` replace the target's `args`. With no `--`, the
  agent gets the target's `args`.
- A word before `--` that is not one of Brig's flags is a usage error.
- With no ref and no `--target`, `brig run` runs the Brigfile's
  `defaultTarget`. A Brigfile with no `defaultTarget` lists its targets and
  exits `2`. With no Brigfile, `brig run` says that it needs a profile, as
  before.

The run boots only what you approved. Brig shows the plan of the run and
asks. A yes is stored, and the next run with the same configuration asks
nothing. With no terminal, or with `--json`, a run that no stored approval
covers boots nothing, exits `8`, and prints the digest of the approval. See
[The approval](brigfile.md#the-approval).

| Flag | What it does |
| --- | --- |
| `--target <name>` | The target to run |
| `--session <name>` | A session of the target, with its own sandbox and guest home |
| `-f <path>` | Read this Brigfile. Its directory is the project |
| `--approve <digest>` | Approve this run without a question. The digest must be the digest of this run |
| `--recreate` | Boot the session's sandbox again on the same guest home, when its configuration changed |

The target line also takes `--image`, `--mem`, `--cpus`, `--network`,
`--offline`, `--publish`, `--skills`, `-d` and `--json`. It refuses `--home`
and `--no-project`.

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
it. The command runs under a login shell, which gives it its environment.
The guest does not parse the words again, so a pipe, a `;` or a glob is not
shell syntax there.

To run a script, put `-c` in front of it, as with `sh -c`. The login shell
parses the script in the guest, so `~` and `$HOME` are the guest home:

```bash
brig sh claude -c 'ls /work | wc -l'
brig sh claude -c 'cat ~/.claude/settings.json'
```

Words after the script are its `$0`, `$1` and onward, as with `sh -c`.
The `sh -c` flag forms also work: `-ec` stops at the first failing command,
`-xc` traces and `-uc` refuses unset variables. These flags apply to the
script, not to the login profile that runs before it. Brig refuses `-c`
with no script, or with an empty script, before the sandbox starts.

Without `-c`, your own shell expands an unquoted `~`, so it names your home
on the host.

A single word with a space or a shell operator in it, such as
`brig sh claude 'ls | wc -l'`, is read as the name of a command. Brig prints
a hint that names `-c` before the command runs.

### `brig stop`

```bash
brig stop claude
```

Stops the sandbox and keeps its name and state on disk. Stopping a sandbox
that is not running is not an error.

```bash
brig stop --target dev [--session <name>] [-f <path>]
```

With `--target`, `stop` stops a session of a target of the repository's
[Brigfile](brigfile.md#stop-and-remove-a-session). It does not read the
Brigfile. It exits `3` when Brig has no record of the session and the runtime
has no sandbox for it.

### `brig rm`

```bash
brig rm claude
```

`brig rm` stops the sandbox and removes it.

- `rm` deletes a guest home that Brig created, and prints its path. Brig
  creates one when the run names no `--home` or `BRIG_WORKSPACE`.
- `rm` leaves a guest home that you named, and your project, and prints the
  path of each.

[Sessions](sessions.md) lists what survives each command.

These cases fail:

| Case | Exit | Result |
| --- | --- | --- |
| the ref has no sandbox | `3` | `rm` says so and forgets the session. The next run of that ref starts a new session. A guest home that Brig created stays until that run deletes it, and `rm` prints its path |
| Brig cannot update its record of sessions | `1` | `rm` keeps the session as it was |
| `rm` removes the sandbox and cannot delete its guest home | `1` | `rm` names the home. The next run of the session deletes what is left of the home before it boots |

```bash
brig rm --all
```

`brig rm --all` stops and removes every sandbox that Brig has. It lists
each one by ref, sandbox name and state, then asks for confirmation.

- If stdin is not a terminal, the command refuses, removes nothing and exits
  `1`.
- To confirm in advance, for example in a script, pass `-y` (or `--yes`).
- If there is nothing to remove, the command asks nothing and exits `0`.

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

```bash
brig rm --target dev [--session <name>] [-f <path>] [--dry-run]
```

With `--target`, `rm` removes a session of a target of the repository's
[Brigfile](brigfile.md#stop-and-remove-a-session), with its guest home and
its approval. It does not read the Brigfile. The cases in the table above
apply to it too.
`brig rm --all` removes the sandboxes of targets with the rest.

`rm` and `stop` each take a single ref.

### `brig ls`

```bash
brig ls
```

`brig ls` lists every sandbox that Brig knows. The columns are `REF`,
`SANDBOX` (the runtime's name for the sandbox), `STATE` and `WORKSPACE` (the
guest home).

```bash
brig ls -q
```

`-q` prints refs only, one per line, and skips a row with no derivable ref.
Every line it prints works as a ref for another verb.

The sessions of [Brigfile targets](brigfile.md#what-brig-ls-shows) follow
in a second table, with the columns `TARGET`, `SESSION`, `SANDBOX`, `STATE`
and `PROJECT`. The table is printed only when there is such a session. A
session of a target has no ref, so `-q` leaves it out.

In `brig --json ls`, a sandbox of a target has a `project` object and no
`ref`:

| Field | Meaning |
| --- | --- |
| `recorded` | `false` when Brig has no record of the session. The other fields are then omitted |
| `target` | the target |
| `session` | the session name, omitted for the default session |
| `root` | the project root |
| `brigfile` | the Brigfile the session was started from |

A sandbox of a profile has no `project` object.

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
sandbox has one: a new default `hvi` sandbox, or one with a policy. For any
other sandbox, the command exits `3`.

```bash
brig logs --gateway
brig logs --gateway claude
```

### `brig info`

```bash
brig info claude
```

`brig info` prints the execution envelope and boots nothing:
the sandbox name, the isolation, the guest home, the image, the verification
mode, the network, every published port and the credentials by name.
`brig --verbose run` prints the same envelope before it boots.

The isolation row names the backend and the guest root. The network row
gives the posture of the running sandbox, and that of the next boot when it
differs.

If Brig cannot resolve a required secret, `info` fails with exit `6`. For a
secret marked `required: false`, it prints a warning and exits `0`. Both
secrets of `claude-code` are optional, so `brig info claude` with neither
set exits `0`.

### `brig plan`

```bash
brig plan claude
brig plan claude ~/src/demo
brig plan claude@refactor --json
brig plan --target dev
```

`brig plan` prints the permissions of a run: the mounts, the network, the
policies, the limits, the image, the runtime and the credentials. It boots
nothing and opens no secret. The [field table](#plan-json-fields) describes
each item.

`plan` reads the run line as `brig run` does, so you can plan a first run
before you start it. If `brig run` refuses to mount a project directory, the
plan fails the same way.

With no policy bound, `POLICY` reads `(none)` and `noPolicy` is `true`.

#### Plan a Brigfile target

`brig plan --target <name> [--session <name>] [-f <path>]` plans a target of
the repository's [Brigfile](brigfile.md#plan-a-target). The project is the
directory that holds the Brigfile. `--session` plans a named
[session](brigfile.md#sessions-of-a-target) of the target. The command takes
no ref and no project, and it refuses `--home` and `--no-project`.

The plan of a target adds these rows to the plan of a profile:

- `TARGET`: the target and its Brigfile
- `PROFILE`: also the profile's alias and source
- `IMAGE`, `NETWORK` and `LIMITS`: where each value comes from, and whether
  the image is pinned by digest
- `POLICY`: one row for each policy, with where it comes from
- `BROKER`: one row for each route, with its upstream and the secrets in
  your store that it uses
- `ARGS`: the arguments the agent gets
- `NOTE`: for example, that a file of yours that overrides a built-in does
  not apply, or that the session's sandbox was started with a different
  configuration

When two policies of the target cannot apply together, the `BROKER` rows
list the routes of each policy, so you can see the conflict.

With no Brigfile, or no target of that name, the command exits `3`.

#### Refusals in the plan

When `brig run` refuses a run before it starts anything, the plan opens with
a `REFUSED` row that carries the run's message, as `refused.reason` under
`--json`. `brig plan` still exits `0`, so a script must read `refused`, not
the exit code.

The plan reports these refusals:

1. a remembered project reached through a symlink
2. a sandbox the runtime holds whose network Brig cannot determine
3. a hypervisor this host cannot boot, such as `hvi` on macOS 14
4. what the backend cannot honor: a policy or a published port it cannot
   enforce, an isolated network on `vz` or `qemu`, or a graphical profile on
   a backend with no display
5. a [route](policies.md#routes) whose secret is not in the store, or a store
   that does not open. The run stops with exit `6`

With no runtime on PATH, `runtime.available` is `false`.

A required secret that is missing from the store also stops the run, with
exit `6`. The plan marks that credential `unresolved` and `required`, and
does not set `refused`.

The plan does not report these refusals:

- A session name that another session already owns.
- A refusal at boot: an image that fails verification, or, on `hvi` under a
  policy, a network gateway that does not confirm it can enforce the policy.

#### Credentials in the plan

`plan` reads the secret store by name, as `brig secret ls` does, and reads
no value.

| State | When |
| --- | --- |
| `resolved` | the store lists the name |
| `unresolved` | the secret is missing from the store. The command still exits `0` |
| `unknown` | `plan` cannot list the store, and nothing later in the chain of the credential has a value |
| `withheld` | the run drops the credential at the denylist, or the credential is an unresolved secret-manager reference in the shell |

The run reads the value, and that read can fail for a credential that the
plan marks `resolved`:

- Another tool emptied the value. The run skips it and uses the next source
  in the chain. When the secret is the last source, the run prints a warning
  that names the secret.
- The stored item is damaged.
- You deny a keychain or keyring prompt.
- A Linux keyring collection is locked.

On the last three, the run stops with exit `6` for a required secret and
drops an optional one with a warning.

#### The plan digest

The last row is a digest of the plan: `sha256:` over the `data` object with
`digest` set to the empty string, encoded compactly:

- the fields in the order of the [field table](#plan-json-fields)
- no whitespace
- non-ASCII text as UTF-8
- `<`, `>` and `&` escaped as `\u003c`, `\u003e` and `\u0026`

The digest is the same for the same run and changes when any field changes.

To verify a digest, encode `data` that way and hash it. Python's
`json.dumps(data, separators=(",", ":"), ensure_ascii=False)` gives the same
bytes when no string in `data` holds `<`, `>`, `&`, U+2028 or U+2029.

The digest covers every field, the sentences included: `isolation`,
`network`, `refused.reason`, each credential's `reason`, and `runtime.bin`.
A release that rewords one of them changes the digest with no permission
changed.

#### Plan JSON fields

`brig plan --json` prints `kind: Plan`. Its `network`, `image.ref` and
`image.pull` are the values that `brig info --json` prints. Its
`argvExposed` has the same names as that of `brig info --json`, sorted. The
`data` fields, in order:

| Field | Type | Meaning |
| --- | --- | --- |
| `session` | string, omitted for an unnamed run | the session name, as typed |
| `profile` | string | the profile the ref resolved to |
| `sandbox` | string | the sandbox name |
| `target` | object, omitted for a plan of a profile | the Brigfile target, [below](#the-target-object) |
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
| `broker` | object, omitted for a run with no route | the `broker` of `brig info --json`, less what only a running broker knows: whether it runs, its pid and port, and the state of its refreshes. The text form prints it as the `BROKER` row, with `refused: see REFUSED` in place of that state |
| `credentials` | list | one entry per credential, below |
| `argvExposed` | list of strings, omitted when empty | the variables whose values `BRIG_ENV_ARGV=1` puts on the runtime's command line, by name, sorted |
| `limits` | object | `mem` in MB and `cpus` |
| `digest` | string | `sha256:` and 64 hex digits |

On the plan of a target, `broker` also has `detail`: one entry for each
route, sorted by name.

| Field | Meaning |
| --- | --- |
| `name` | the route name |
| `policy` | where the policy that declares the route comes from |
| `preset` | the preset, omitted when the route has none |
| `mode` | the route mode |
| `upstream` | where the broker sends requests, omitted for a `gcp` route |
| `services` | for a `gcp` route, the Google APIs it reaches |
| `secrets` | the names of the secrets in your store that the broker reads for the route |
| `vends` | `true` when the route gives the guest AWS keys |

#### The target object

| Field | Meaning |
| --- | --- |
| `name` | the target |
| `brigfile` | the Brigfile |
| `root` | the project root, which is also the project |
| `profile` | `alias` and `source`, such as `builtin:claude-code` |
| `origins` | where `image`, `mem`, `cpus` and `network` come from: `flag`, `env BRIG_<KEY>`, `target <name>`, or `profile <alias> (<source>)`. `networkAsked` is the posture before a policy changed it, omitted when no policy did |
| `imagePinned` | `true` when the image is named by digest |
| `policies` | each policy, with `name` and `from`: `brigfile <alias> (<source>)`, `builtin profile <name>` or `attachment to <name>` |
| `notes` | notes about the target, omitted when there are none |
| `args` | the arguments the agent gets |

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
| `state` | `resolved`, `unresolved`, `withheld` or `unknown`, as in [Credentials in the plan](#credentials-in-the-plan) |
| `reason` | why the credential is not `resolved`, omitted when it is |

### `brig network`

```bash
brig network publish claude 3000      # the agent's dev server, on localhost:3000
brig network publish claude 8080:80   # host 8080 carries guest 80
brig network ls claude                # what this sandbox publishes
brig network unpublish claude 8080    # close it again
brig network unpublish claude --all
```

`publish` opens a guest port on the host of a sandbox that is up, or
records it for the next boot of one that is not. `--publish` on `brig run`
does the same at boot. `ls` lists the ports and whether each is open now,
even when a required secret is missing. `brig info` prints the ports as its
`PORTS` row.

A port uses Docker's syntax:

| Written | Means |
| --- | --- |
| `3000` | host `127.0.0.1:3000` carries guest `3000` |
| `8080:80` | host `127.0.0.1:8080` carries guest `80` |
| `127.0.0.1:8080:80` | the same, with the address written out |
| `0.0.0.0:8080:80` | offered to the network this host is on |
| `5353:53/udp` | host `127.0.0.1:5353` carries guest `53` over UDP |

The host address defaults to `127.0.0.1`, so a published port is reachable
from this machine only. The execution envelope shows the address of each
port.

A publication belongs to the sandbox and outlives a run. `brig stop`
releases the host port and keeps the publication, so the next `brig run`
opens the same ports again. `brig network unpublish` removes one, and
`brig rm` removes all of them with the sandbox.

`unpublish` names a port by its host side: `brig network unpublish claude
8080` closes whatever host port `8080` carries. A value from the HOST column
of `brig network ls` works as printed, so
`brig network unpublish claude 0.0.0.0:8080` closes the port on that address
only.

On macOS, publishing needs the `hvi` backend. On `vz` and `qemu`, Brig
refuses a run that publishes a port. On Linux, ports are set only when the
sandbox is created: `brig run --publish` works, and `brig network publish`
on a running sandbox fails and tells you to remove the sandbox and run again
with `--publish`.

### `brig doctor`

```bash
brig doctor
```

`brig doctor` checks these items, one line each: the Brig build, the host,
the hypervisor, the runtime, the credential broker, the boot assets, the
overlay root, cosign, the profile directory, the secret store, brigd and the
image of an agent.

The `rootfs` line says whether this host can build the overlay root that a
`genericBoot` profile boots by default. When it cannot, the line names what
is missing, and a run keeps the runtime's own root. It reads `!!` only when
`BRIG_ROOTFS_MODE=overlay-block` asks for an overlay the host cannot build,
or when `BRIG_ROOTFS_MODE` is not a mode.

The first line is the build that `brig version` prints. If a running brigd
reports a different build, `doctor` marks it `!!` and tells you to restart
brigd.

```bash
brig doctor claude
```

With an agent, the `image` line checks that agent's image. Without one, the
line is `--` and says how to name an agent. When the agent's profile names a
`runtimeBin`, the runtime and boot lines report on that binary, not the one
on `PATH`. An unknown agent exits `3`.

A `bundle` line appears when `brig` runs from the bin directory of the runtime
bundle without its launcher. See
[Started without the launcher](troubleshooting.md#started-without-the-launcher).

On Linux with nerdctl or hull, or with no runtime yet, `doctor` adds what a
run needs from the host: `subids`, `userns` and `uidmap` for a user other than
root, and `devices` (`/dev/vhost-vsock`) and `cni` for every user. These
lines do not change the exit status.

- For a user other than root, `devices` and `virtual` read `!!` when the
  user reaches the device only through the `kvm` group. A rootless run
  drops that group. The device has to be this user's, carry an ACL entry
  for this user, or be open to everyone.
- Where AppArmor restricts user namespaces, as on Ubuntu 24.04 and later,
  `userns` looks for a loaded profile that allows a rootless run.
- For a `nerdctl` or `hull` from outside the runtime bundle, or when no
  runtime is found, `doctor` adds `tools`: the hypervisor a run boots on,
  and `oras` when the boot assets are missing. For such a `nerdctl`, the
  `runtime` line ends with `(not the runtime bundle; see install.sh)`.
  `doctor` cannot tell whether that runtime boots a sandbox.
- With hull, `cni` looks where hull does: `CNI_PATH`, then the runtime
  bundle's plugins, then `/opt/cni/bin`.

Doctor checks the image against the trust policy a run of that agent uses,
with the `BRIG_VERIFY_*` variables applied, per-agent spelling first. The
verify line names the variable its mode came from. When a variable replaces
the shipped policy, a `trust` line names the registry, identity and issuer
in use. With no agent named, only the global variables count.

```bash
brig doctor --json
```

`--json` prints the same checks in the [envelope](#--json-output), as
`kind: Doctor`. `data` is an array, and each entry has `name`, `state`
(`ok`, `!!` or `--`), `finding` and, when the check failed, `fix`.

Only two checks set the exit status: a missing or broken runtime exits `4`,
and a secret store that does not open exits `6`. Every other finding,
including one marked `!!`, prints its fix and exits `0`.

### `brig report`

```bash
brig report claude
```

`brig report` writes a diagnostic report for a session to one plain-text
file, then prints the file name. Run it from another shell when a session
hangs, slows down or stops, before you close the stuck terminal if you can.
The sandbox outlives the terminal, so the report also works after.

```
brig report [<ref>] [-o <file>] [--tail N]
```

| Flag | Meaning |
| --- | --- |
| `-o <file>` | where to write the report: a new file, or a directory to put it in. Default: `~/.brig/host/reports/brig-report-<ref>-<time>.txt` |
| `--tail N` | keep the last `N` lines of each log. Default: `2000` |

The report starts with a verdict: the known failures it found, the line
that shows each one, and the fix. For example, when the guest ran out of
memory, the verdict names the kill and tells you to stop the sandbox and run
it with a larger `--mem`. Then it holds:

- the Brig, runtime and host versions, and the memory and vCPUs of the VM
- the `brig doctor` lines, with the secret store left unchecked
- `brig ls` and the session record of the ref
- the VMM process and its thread stacks
- answers from the guest: memory pressure, `free`, `uptime`, the largest
  processes, `df` and `dmesg`
- the console, gateway and broker logs

Each step has a time limit, so a guest that does not answer cannot stop the
report. A step that runs out of time is written in the report as a finding.
`brig report` does not stop, start or repair anything.

The default directory is not mounted into any sandbox. The current
directory is usually the project, which the agent can read and write, so
`brig report` warns when `-o` points into the project or the guest home.
It never writes over a file that is already there.

The report masks credentials of known shapes and does not read the secret
store. Read the file before you post it: a value of a shape Brig does not
know is not masked. After it writes the file, `brig report` lists the lines
that hold a string that looks random and was not masked, so you can check
those first. Paths, user names and project names stay in the report.

With no ref, the report holds the host, `brig doctor` and `brig ls` only.
`brig report` exits `0` when it wrote the file, also when steps ran out of
time.

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

The version comes from the nearest tag:

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
stdout and installs nothing. See [Shell completion](completions.md) for
where each shell reads its script.

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

`rm` refuses while a sandbox of the agent exists, running or stopped, and
names the `brig rm <ref>` to run first for each one. It also refuses when
Brig cannot reach the agent's runtime, for example with an unknown
`BRIG_RUNTIME`.

`--force` (or `-f`) with `new` or `export` overwrites an existing
destination file. Without it, Brig refuses and names the file. `--json`
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
brig policy show --preset <preset> [--json]
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

### `brig target`

```bash
brig target ls
```

`brig target ls` lists the targets of the repository's
[Brigfile](brigfile.md). `*` marks the default target. Each row names the
target's profile alias, where that profile comes from, and its policies:
the target's own, then those its profile lists.

```
  TARGET  PROFILE  SOURCE                POLICIES
  ci      tool     file:.brig/tool.yaml  -
* dev     claude   builtin:claude-code   anthropic

* the default target
read from /home/you/code/demo/Brigfile
```

```
brig target ls [-f <path>] [--json]
```

`-f` (or `--file`) reads that Brigfile instead of looking for one.
`--json` prints the rows as `kind: TargetList`.

### `brig config`

```bash
brig config --target dev
```

`brig config` shows what the repository's [Brigfile](brigfile.md) declares
and where each part comes from. With no flag it shows the project: the
Brigfile, the project root, the default target, and every profile, policy
and target with its source.

With `--target <name>` it shows that one target resolved:

```
target    dev (default)
brigfile  /home/you/code/demo/Brigfile
profile   claude  builtin:claude-code
cpus      2                                       from target dev
mem       4096                                    from profile claude
image     ghcr.io/brig-sh/claude-code-stock:root  from profile claude
policies  anthropic  file:.brig/anthropic.yaml
args      "--continue"
```

`config` does not apply `BRIG_*` settings or run-line flags.
[`brig plan --target`](#brig-plan) does.

When the Brigfile uses `builtin:<name>` and your profile directory has a file
that overrides that built-in, `config` prints a note. The Brigfile does not
read your file.

```
brig config [--target <name>] [-f <path>] [--json]
```

`-f` (or `--file`) reads that Brigfile instead of looking for one. `--json`
prints `kind: Config`. With `--target` the payload also has a `target`
object, whose `cpus`, `mem` and `image` each carry a `value` and a `from`
(`target` or `profile`). A name that is not a target exits `3` and lists the
targets there are.

### `brig init`

```bash
brig init
```

`brig init` writes a starter [Brigfile](brigfile.md) at the top of the Git
worktree, or in the current directory outside Git. The starter has one
target, `dev`, on the `claude-code` profile Brig ships. Then it prints the
next commands to try.

```
brig init [-f <path>]
```

`-f` (or `--file`) writes the file at that path instead. `init` never
replaces a file or a link that is already at the path. It exits `1` and
names the path.

### `brig secret`

```bash
brig secret create gh-token
```

`brig secret create` reads the value from stdin and stores it under that
name in your keyring.

```
brig secret create <name> [-f FILE] [--type tls|ca]
brig secret update <name> [-f FILE] [--type tls|ca]
brig secret read <name>
brig secret delete <name> [-y]
brig secret ls
brig secret import <agent>
brig secret import <agent> <name>
```

`--type tls` checks the value as a client certificate with its key, and
`--type ca` as a bundle of CA certificates. See
[Getting a value in](secrets.md#getting-a-value-in).

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
and the sources a profile can declare.

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
what is counted and what is never collected.

## The ref

A ref is `<agent>` or `<agent>@<label>`. With no label, the ref names the
agent's default session, so `claude` and `claude@refactor` are two sessions
of one agent. See [Sessions](sessions.md) for what a session keeps separate
and what survives which command.

The separator is one `@`. Brig refuses these refs:

| Written | Refused because |
| --- | --- |
| `claude@@x` | more than one `@` |
| `@refactor` | no agent named before the `@` |
| `claude@` | a trailing `@` names no session. Drop it for the default session, or name one |
| `claude@Refactor` | the label has a character that Brig must change. Labels use lowercase letters, digits, dot, dash and underscore |

## The run line

Brig reads three kinds of token from a `brig run <ref> [project] [args...]`
line, by position:

1. The first bare word is the ref.
2. On `run` and `plan`, the second bare word is a project directory. Brig
   mounts it read-write at `/work/<basename>` and starts the agent there.
   Every process you start in that sandbox, `brig sh` included, gets
   `BRIG_PROJECT` set to that path. Without a project the variable is
   unset. `plan` shows that run and does not start it.
3. The next bare word, or anything after `--`, is the agent's own argument.

The second bare word is the project even when it names no existing
directory. Brig then refuses the run and tells you to put the word after
`--`.

`--` ends Brig's parsing. No word after it is read as a project, and a
project named before it still counts.

Brig also reads its own flags after the ref and after the project, up to the
first argument of the agent. These two lines do the same:

```bash
brig run --mem 4096 -d claude ~/code/demo
brig run claude ~/code/demo --mem 4096 -d
```

The examples in these pages put Brig's flags before the ref. Use that form:
there Brig refuses a flag it does not know, so a mistyped flag never reaches
the agent.

## Flag placement

A Brig line has two positions for Brig's own flags. Global flags stand left
of the verb. Run-line flags stand between the verb and the first argument of
the agent.

| Position | Flags |
| --- | --- |
| Global | `--verbose`, `-q`/`--quiet`, `--json` |
| Run-line | `--image`, `--home`, `--mem`, `--cpus`, `--no-project`, `-d`/`--detach`, `--skills`, `--network`, `--offline`, `--publish` and `--json` |

Brig refuses any other flag in the global position by name:

```
brig: unknown flag "--nope" before the command. brig takes a command first:
`brig run claude`, `brig ls`. If "--nope" is the agent's, it goes after the
profile
```

Both positions accept `--json`. `-q`/`--quiet` works only before the verb.
Between the verb and the ref it is refused, and after the ref it is the
agent's. See
[One flag whose position changed](migration.md#one-flag-whose-position-changed).

```bash
brig info claude --json
brig --json info claude
```

Both print the same report.

A help flag before the ref is a help request (see [Help](#help)). Brig
refuses by name any other unrecognized flag there:

```
brig: unknown flag "--nope" before the profile name. brig's own flags come
before the profile and the agent's after it; put "--nope" after the profile
to pass it through, or -- to end brig's flags
```

The same flag after the ref reaches the agent untouched. After the agent's
first argument, Brig stops reading its own flags. A Brig flag there gets a
warning and goes to the agent:

```bash
brig run claude -p hi --quiet
```

This runs the agent with `-p hi --quiet`, with a warning about `--quiet`.

On a run line with no ref, `--target`, `--session`, `-f`, `--approve` and
`--recreate` are Brig's too. After a ref they are the agent's, so
`brig run claude -f notes.md` hands `-f notes.md` to the agent. See
[Run a Brigfile target](#run-a-brigfile-target).

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
| `--network MODE` | `shared`, `isolated` or `offline` | the posture an existing sandbox was started with, then the profile's `network:`, then `isolated` (`shared` on `vz` or `qemu` when nothing names a posture) | the sandbox's network posture. A verb without the flag does not change the posture of a sandbox. If Brig cannot establish the posture of an existing sandbox, name one with this flag or `BRIG_NETWORK`. See [policies.md](policies.md) |
| `--offline` | (none) | off | shorthand for `--network offline`: the agent runs with its guest home, and nothing leaves the sandbox. Refused together with a different `--network` value |
| `--publish PORT` | `3000`, `8080:80`, `127.0.0.1:8080:80`, `5353:53/udp` | nothing published | open a guest port on the host. Repeatable. Binds to `127.0.0.1` unless the address says otherwise. There is no `-p`, which stays the agent's flag. See [`brig network`](#brig-network) |

A flag wins over its [environment variable](#environment-variables), and
the variable wins over the profile's field.

`--mem` and `--cpus` take a positive whole number. Brig refuses anything
else, including `0`.

## `--json` output

Which verbs accept `--json` depends on its position.

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
| `target ls` | global, or local after `ls` | envelope, `kind: TargetList` |
| `config` | global, or local after `config` | envelope, `kind: Config` |
| `agent show`, `agent export`, `agent new` | local only, after the subcommand | bare document, no envelope |
| `policy show` | local only, after the subcommand | bare document, no envelope |

- "Global" means left of the verb: `brig --json ls`.
- "Local" means after the subcommand, on either side of the operand of that
  subcommand. `brig agent show claude-code --json` and
  `brig agent show --json claude-code` both work.

Every verb that is not in the table refuses `--json` in both positions.

`agent show` and `policy show` are local only, so the global position
refuses them:

```
brig --json agent show claude-code
brig: `brig agent` has no --json output. --json is for the read verbs: ls,
info, plan, agent ls, secret ls, target ls, config, doctor, version and the
network verbs, and for run and sh
```

Put the flag after the subcommand instead.

**Envelope shape.** A list or report verb prints
`{"apiVersion": "brig.sh/v1alpha1", "kind": "...", "data": ...}`. Within one
`apiVersion`, fields are added but never renamed or removed, so a script
written against it keeps parsing. No field carries a credential value.

**Bare shape.** `agent show`, `agent export`, `agent new` and `policy show`
print the document itself, a file that Brig can read back:

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
publishes after the command's change, as `kind: Ports`:

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

`live` says whether the port is open now. A recorded port that is not live
belongs to a sandbox that is not running, and Brig opens it again when the
sandbox starts. When Brig cannot tell, `live` is `null` and the `STATE`
column of `brig network ls` reads `unknown`. On Linux a port always reports
`unknown`.

**`run` and `sh` under `--json`.** After the agent exits, Brig prints the
outcome as one compact JSON line, the last line of stdout:

```json
{"apiVersion":"brig.sh/v1alpha1","kind":"Run","data":{"ref":"claude","sandbox":"brig-claude-code","stage":"agent","exit":0}}
```

| Field | Meaning |
| --- | --- |
| `data.stage` | `"brig"`: Brig refused before the agent ran, and `data.error` carries the reason. `"agent"`: the agent ran. `"gui"`: a windowed agent. `"detached"`: applies under `-d` |
| `data.exit` | the agent's own exit status when `stage` is `"agent"`, and one of Brig's own exit codes otherwise |
| `data.signal` | the signal, when a signal killed the agent |

When Brig refuses under `--json`, it prints the `Run` object on stdout, as
on success, and the usual error line on stderr. See
[Exit codes](#exit-codes) for how a script must read the exit status.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | success |
| `1` | a general failure |
| `2` | a usage error: an unknown flag, a stray argument, or a value in the wrong place, as reported by the verb's own parser |
| `3` | no such thing: an unknown agent, a sandbox that is not there, no Brigfile, or a target the Brigfile does not have |
| `4` | no usable runtime: none installed, an unknown `BRIG_RUNTIME`, or `BRIG_RUNTIME_BIN` (or a profile's own `runtimeBin`) pointing at nothing. The refusal names the setting that caused it |
| `5` | a boot refused over image verification |
| `6` | a required secret was not resolved, or the secret store did not open. A declared secret marked `required: false` produces a warning and no failure |
| `7` | the runtime and backend cannot enforce a property bound to the sandbox, or cannot confirm they do. The refusal names the property, the runtime and the backend. See [Exit 7 cases](#exit-7-cases) |
| `8` | a run of a Brigfile target that no approval covers. Nothing booted. The refusal names the digest that approves the run. See [The approval](brigfile.md#the-approval) |

[Stability](stability.md) lists this table as stable to script against.

**Under `run --json` and `sh --json`, the agent's exit status becomes
Brig's.** An exit `3` from `brig --json run claude` can be the agent's own
`3` or Brig's "no such agent". Branch on `data.stage` of the
[`Run` object](#--json-output): `"agent"` means the code is the agent's, and
anything else means it is one of the classes in the table.

### Exit 7 cases

An egress policy is one such property. `brig run` and `brig sh`, and the
retired spelling of `brig sh`, refuse a policy with `7`:

- on `vz` or `qemu`
- on docker
- on `hvi`, when the runtime is too old to enforce it or Brig cannot check
- on Linux, when Brig cannot check that the host enforces it
- on a macOS backend that Brig does not know

The code is `7` whether a flag, `BRIG_NETWORK` or the profile's `network:`
chose the posture.

A [route](broker.md#route-modes) is refused with `7` too, where the run
cannot serve it: on `vz` or `qemu`, on docker, with no network, and on an
`hvi` runtime too old to serve routes. So is a route in a mode this build of
Brig does not serve.

`brig network publish` refuses with `7` on `vz`, `qemu`, docker and a macOS
backend that Brig does not know. On `hvi` it records the port, and the next
run refuses with `7` if Brig cannot check the gateway.

Two refusals exit `1`. With no policy bound, an isolated network on `vz`
or `qemu` is a different refusal. A refusal that Brig reaches first keeps
its own code: a graphical profile on a backend with no window exits `1`,
under a policy or not.

### Usage mistakes that exit 1

These exit `1`, not `2`:

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
`BRIG_<AGENT>_<KEY>`, and the agent-specific form wins. The agent name is in
upper case, with each dash changed to an underscore: `claude-code` reads
`BRIG_CLAUDE_CODE_MEM` ahead of `BRIG_MEM`.

Some settings have no per-agent form: the directories, the runtime choice,
the boot-asset and gateway paths, and `BRIG_ENV_ARGV`. The tables mark
them "global only".

### Sandbox and profile locations

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_WORKSPACE` | `~/.brig/homes/<sandbox>` | host directory mounted as the guest home. A named session appends `-<slug>` to one you set. Brig deletes the default one on `brig rm`, and never deletes one you set. A sandbox that a release before 0.3.0 started on `~/brig/<agent>` keeps that home, and Brig does not delete it |
| `BRIG_NAME` | `brig-<agent>` | the sandbox's own name. Must begin with `brig-`, or `brig ls` and `brig rm --all` cannot find it. A name that begins with `brig--` is refused: those names belong to [Brigfile](brigfile.md) targets. A named session appends `-<slug>` |
| `BRIG_PROFILE_DIR` (global only) | `$XDG_CONFIG_HOME/brig`, which is `~/.config/brig` when that is unset | where your own agent files live. Brig refuses to start while `BRIG_TEMPLATE_DIR`, removed in v0.5.0, is set and this is not |
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
| `BRIG_NETWORK` | the posture an existing sandbox was started with, then the profile's `network:`, then `isolated` (`shared` on `vz` or `qemu` when nothing names a posture, which `brig info` reports) | `shared`, `isolated` or `offline`. Wins over the posture an existing sandbox was started with. An unrecognized value refuses the run. See [policies.md](policies.md) |
| `BRIG_SKILLS` | `0` | `1` copies your own `~/.claude` skills and plugins into the guest home. Same as `--skills` |
| `BRIG_FORWARD_ENV` | (unset) | a space-separated list of environment variable names to carry into the guest, read live on every run |
| `BRIG_TITLE` | the agent's own | window title for a graphical agent |

`BRIG_FORWARD_ENV` replaces only the profile's `env:` bindings that use the
singular `ref: env.<name>` form. A `refs:` chain stays, even when it
includes an `env.` entry. If the list names a variable that the profile
binds from another source, such as `secrets.` or a literal `value:`, the
profile's binding wins and Brig warns about the name. See
[`BRIG_FORWARD_ENV`](profiles.md#brig_forward_env).

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

`warn` reports an image that has no signature Brig can check, and boots the
image. It stops to ask about an image under `ghcr.io/brig-sh/` that fails
the check or cannot be checked, and it refuses boot assets whose signature
fails.
`require` refuses to boot anything it cannot positively verify, including
when cosign is missing. See [Security](security.md) for what verification
does and does not catch.

### Runtime and hypervisor

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_RUNTIME` (global only) | `hull` on macOS, `nerdctl` on Linux | which runtime to drive |
| `BRIG_RUNTIME_BIN` (global only) | `hull` on the `hull` runtime, `nerdctl` then `docker` on the `nerdctl` runtime, each found on `PATH` | path to that binary. `BRIG_RUNTIME` picks the runtime first, and this only overrides its executable |
| `BRIG_HYPERVISOR` | the agent's own `hypervisor:` field, else `vz` on macOS and `hvi` on Linux | `vz`, `hvi` or `qemu` on macOS. On Linux, for a `genericBoot` profile: `hvi`, `cloud-hypervisor`, `qemu` or `firecracker`, with `vz` and an empty value meaning `hvi`. Wins over the agent's own field when set |
| `BRIG_ROOTFS_MODE` | unset: `overlay-block` where the host can build it, else the runtime's own root | The root of a `genericBoot` profile whose `rootfsType` is unset or `virtiofs`. `overlay-block` boots the image read-only with a sparse 16 GiB writable disk per sandbox. Unset, Brig falls back to the runtime's own root without a message. Set to `overlay-block`, a host that cannot build it refuses the run and says why. `virtiofs` keeps the runtime's own root. Any other value is refused, as is `overlay-block` on another profile or next to a `block` or `9pfs` rootfs type. The overlay needs `brew install e2fsprogs` on macOS (`vz` and `hvi`), and the overlayfs snapshotter and `mke2fs` on Linux (`hvi`, `cloud-hypervisor` and `qemu`). `brig doctor` says whether the overlay is available. See [The guest root of a generic boot](runtimes.md#the-guest-root-of-a-generic-boot) |
| `BRIG_ROOTFS_TYPE` | the agent's own `rootfsType:` field | `block`, `virtiofs` or `9pfs`, how the guest root reaches the microVM on macOS. `block` and `9pfs` keep the runtime's own root, and unset or `virtiofs` takes the overlay root where `BRIG_ROOTFS_MODE` allows it. Brig refuses a profile's `rootfsType:` outside that set, but does not check this variable |
| `BRIG_CONTAINERD_RUNTIME` (global only) | `io.containerd.urunc.v2` | Linux only, on the `nerdctl` runtime: the containerd shim that boots the sandbox as a microVM. Another microVM shim is accepted. A shim that Brig knows shares the host kernel, such as `runc`, `crun`, `containerd-shim-runc-v2` or `io.containerd.runc.v2`, is refused, by name or by path. The `ISOLATION` row names the variable when it is set |

On macOS, `hvi` is the only backend that enforces an attached egress policy
or `--network isolated`, and it needs macOS 15 or newer. Linux supports the
isolated posture too, and enforces egress policies with nerdctl. `vz` is the
only backend with a graphical console. See [Runtimes](runtimes.md).

<details><summary>macOS 14</summary>

For the built-in `hvi` profiles, set `BRIG_HYPERVISOR=vz` and
`BRIG_NETWORK=shared`. Brig refuses an `hvi` run on macOS 14, and `vz` cannot
satisfy the isolated posture of those profiles.

</details>

### Boot assets and the network gateway

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_BOOT_ASSETS` (global only) | on macOS, hull's assets directory, usually `~/.hull/assets`. On Linux, `$XDG_DATA_HOME/brig/assets` (`~/.local/share/brig/assets` when that is unset) | directory holding the host kernel and initrd a `genericBoot` agent needs |
| `BRIG_BOOT_ASSETS_REF` (global only) | `ghcr.io/nofireai/hull-assets:<os>-<arch>` | the bundle Brig fetches when the boot assets are missing |
| `BRIG_GATEWAY_SOCK` (global only) | `<gateway dir>/gateway-<subnet>.sock` | control socket of the shared network gateway. Names matching `sandbox-*.sock` are reserved for isolated gateways and refused here |
| `BRIG_GATEWAY_DIR` (global only) | the directory of `BRIG_GATEWAY_SOCK`, else `~/.brig` | where gateway sockets, logs and network records live, shared and per-sandbox alike |
| `BRIG_BROKER_BIN` (global only) | `brig-broker` beside `brig`, else on `PATH` | the credential broker a run with a [route](policies.md#routes) starts. See [broker.md](broker.md#hosting) |

