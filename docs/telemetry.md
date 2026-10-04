# Telemetry

Brig sends anonymous usage events and crash reports, and you can turn them
off.

## Turn it off

```bash
brig telemetry off
```

Brig records the answer on this machine. The answer holds whether or not
Brig is the caller: hull, the runtime Brig drives on macOS, reads the same
answer.

To turn telemetry off without a recorded answer, set `DO_NOT_TRACK=1` in
your environment:

```bash
DO_NOT_TRACK=1 brig run claude
```

`HULL_TELEMETRY_DISABLED=1` works the same way as `DO_NOT_TRACK=1`. Both
variables are compared to the exact string `1`, and both beat an answer
already recorded on disk.

## What is counted

Brig sends two kinds of event itself:

- **One event per command.** It names the command, the agent it ran, and
  whether it worked. A command that failed carries the class of the failure,
  never its message.
- **A crash report** when Brig panics.

On macOS, hull also reports what only it can see, for the sandboxes Brig
asks it to run:

- **A sandbox boot**, with the hypervisor backend.
- **How long the sandbox lived**, when it stops.
- **The memory use and CPU share of its VM**, while a session is attached to
  it: first a few seconds after the attach, then every 30 seconds.
- **A crash report** when hull panics.

| Host | What is sent |
| --- | --- |
| macOS | Brig's events, and hull's events for the sandboxes Brig runs |
| Linux | Brig's events. `nerdctl` and `docker` send nothing |

Every other call Brig makes to hull is not counted. A reachability probe, a
`ps` lookup and cleanup work send nothing. hull never sends a command event
of its own for a call Brig makes, so a command counts once.

Some invocations send no command event:

- `brig telemetry`, which is how you answer, and the shell completion
  script, which runs in every new shell
- a bare `brig`, a mistyped global flag, and a spelling Brig has removed
- a command stopped by Ctrl-C or a signal before it hands your terminal to
  the agent. A panic sends its crash report instead

A `brig run` or `brig sh` that hands your terminal to the agent counts as
`ok` once the handover starts, whatever the agent does after it.

### Before you answer

Nothing is sent until someone answers.

The first `brig run` or `brig sh` on a terminal asks the question, before
anything boots, when Brig runs in the foreground of that terminal:

```
brig collects anonymous usage events and crash reports to help us
improve it: command names, which agent and backend you run, OS and tool
versions, and stack traces. An agent of your own goes out as a salted
hash of its name. File paths, arguments and image names are never sent.
Docs: https://github.com/brig-sh/brig/blob/main/docs/telemetry.md
Enable telemetry? [Y/n]
```

A single enter or `y` answers yes, and `n` answers no. Ctrl-D is not an
answer, and the question stays open. Any other command, a script, a pipe, a
job in the background, CI, `--json` and `-q` never ask, and send nothing
until there is an answer.

A recorded yes stops counting as an answer when Brig or hull widens what it
collects. The next `brig run` or `brig sh` on a terminal asks again, and
nothing is sent until you answer.

## Check or change the state

```bash
brig telemetry status
```

| Command | What it does |
| --- | --- |
| `brig telemetry` | the same as `status`. It changes nothing |
| `brig telemetry status` | reports the state |
| `brig telemetry on` | records the answer on, then reports the state |
| `brig telemetry off` | records the answer off, then reports the state |

After `on` or `off`, Brig reports the state that resulted, which can differ
from the answer you recorded. If `DO_NOT_TRACK` is set in your shell,
`brig telemetry on` still reports off and names the variable.

The report is one of three states: on, off, and not answered yet. Under the
state, `↳` lines say what decided it, and `→` lines say what to type to change
it. When the machine has an install identifier, the report names it.

Telemetry is on:

```
telemetry: on
  ↳ each brig command counts once
  ↳ on macOS, a sandbox also reports its boot and how long it ran
  ↳ install id: 12345678-90ab-4cde-8f01-234567890abc
  → to turn it off:  brig telemetry off
```

Telemetry is off because a variable in your shell decided it:

```
telemetry: off
  ↳ DO_NOT_TRACK=1 in this environment turns it off
  ↳ it beats any answer recorded on this machine
  ↳ install id: 12345678-90ab-4cde-8f01-234567890abc
  → to let the recorded answer decide:  unset DO_NOT_TRACK
```

Telemetry is off because you recorded the answer with `brig telemetry off`:

```
telemetry: off
  ↳ the answer is recorded on this machine
  ↳ install id: 12345678-90ab-4cde-8f01-234567890abc
  → to turn it on:  brig telemetry on
```

Nobody answered yet, or the answer was to a shorter list than today's:

```
telemetry: not answered yet
  ↳ nothing goes out until you answer
  ↳ the first `brig run` or `brig sh` on a terminal asks the question
  → to turn it on:   brig telemetry on
  → to turn it off:  brig telemetry off
```

Every event carries the install identifier, so it finds the events this
machine sent, for example when you ask for them to be deleted. Asking for the
report creates no identifier.

The report and `brig telemetry --help` do not name the runtime, so you can
opt out without knowing which binary sends. `brig telemetry` takes no
`--json` form.

## Where the answer lives

The answer and the install identifier are in `~/.hull/telemetry.json`. You
can read or delete that file at any time. Deleting it rotates the install
identifier and forgets the answer.

Brig never waits on the network. When a command ends, or right before it
hands your terminal to the agent, Brig writes its event to `~/.hull/outbox/`
and starts `brig telemetry flush` in the background. That process uploads the
outbox and the crash reports in `~/.hull/crashes/`, then exits, within about
30 seconds. It has no terminal and prints nothing, and only one runs at a
time. An event it cannot send stays queued, and the next command's upload
tries again.

A build of Brig with no endpoint, such as one from `go install`, sends
nothing and queues nothing. `brig telemetry off` empties the queues.

These are hull's files, so one answer covers both tools. On Linux, where hull
does not run yet, Brig creates `~/.hull` for these files alone. Brig run
under sudo with your HOME leaves no files there.

## What an event carries

Every event carries:

| Field | Example | What it is |
| --- | --- | --- |
| `schema_version` | `3` | which version of this list the event follows |
| `event` | `command` | `command` or `crash` from Brig; `start`, `end`, `metrics` and `crash` from hull |
| `product` | `brig` | |
| `version` | `0.4.0` | Brig's version |
| `platform` | `macos` | `macos` or `linux` |
| `os` | `26.3.1` | the macOS version, or the distribution and its version from os-release, such as `ubuntu 24.04` |
| `arch` | `arm64` | the CPU architecture |
| `uname` | `Darwin 25.3.0 arm64` | the kernel's name, the version at the start of its release, and the machine; not the hostname, the kernel's build string, or the rest of the release |
| `install_id` | random UUID | generated on your machine, and not derived from it |
| `captured_at` | RFC 3339 time | when the event happened |
| `checksum` | hex SHA-256 | over a few of the fields above, so the collector can drop a forged event |

A command event also carries:

| Field | Example | What it is |
| --- | --- | --- |
| `command` | `run` | the command, such as `run`, `sh` or `ls`. A word that is not one of Brig's commands is sent as `unknown` |
| `outcome` | `ok` | `ok` or `error` |
| `error_class` | `credentials` | on a failure, one of `usage`, `not-found`, `runtime`, `verify`, `credentials`, `capability` and `other`: the classes of the [exit codes](cli.md), never the message |
| `agent` | `claude-code` | a profile that Brig ships, by name. Any other profile is sent as `custom` |
| `agent_hash` | `91805c9cfd386de4` | for a `custom` profile only: a salted hash of its name |
| `runtime` | `hull` | the runtime Brig drove: `hull`, `nerdctl` or `docker` |

`agent_hash` lets one profile of your own be counted across machines without
sending its name. The salt is in Brig's source, so anyone who guesses the name
can compute the same hash. A name that is easy to guess is as good as sent.

A crash report carries the command, the Go type of the panic, and a stack
trace whose paths are trimmed. It never carries the panic message, which can
hold a path.

The events hull sends for a sandbox carry Brig's version, and hull's own as
`runtime_version`.
[hull's telemetry docs](https://github.com/brig-sh/hull/blob/main/docs/telemetry.md)
are the field-by-field reference for those, and for the transport.

## What is never collected

- guest home paths, or any host path
- repository names, branches or remotes
- command arguments, including the agent's own
- agent prompts, or anything the agent read or wrote
- secret names, secret values, or which credentials were forwarded
- image names and registry references
- network destinations the guest reached
- file metadata: names, sizes, timestamps, counts

## See also

- [CLI reference](cli.md) has the flags and exit codes for every verb,
  including `brig telemetry`.
- [Security](security.md) covers the rest of the boundary a sandbox keeps.
