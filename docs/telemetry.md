# Telemetry

Brig lets its sandbox runtime count three operations, and you can turn the
counting off.

## Turn it off

```bash
brig telemetry off
```

Brig records the answer on this machine. The answer holds whether or not
Brig is the caller.

To turn telemetry off without a recorded answer, set `DO_NOT_TRACK=1` in
your environment:

```bash
DO_NOT_TRACK=1 brig run claude
```

`HULL_TELEMETRY_DISABLED=1` reaches hull the same way as `DO_NOT_TRACK=1`,
untouched. Both variables beat an answer already recorded on disk.

## What is counted

Brig keeps no telemetry store and runs no collector. The sandbox runtime
does the counting and the sending.

| Host | Runtime | What is sent |
| --- | --- | --- |
| macOS | `hull` | the events for the three operations below |
| Linux | `nerdctl` | nothing |

Brig lets the runtime count three operations:

- **A sandbox boot.**
- **A sandbox stop**, including the stop inside a restart, when Brig recreates
  a sandbox whose shares or policy went stale. `brig rm` stops each sandbox
  before it removes it, so each of those stops is counted. The removal is not
  counted.
- **The command that hands your terminal to the agent**, which is `run`
  without `--json`, `sh`, or the `--json` child path either one takes.

Each operation is counted on its own, so one command can count more than
once:

- A `brig run` that boots a sandbox counts the boot and the handover.
- `brig rm --all` counts the stop of each sandbox it removes.

Brig suppresses counting on every other call it makes. A reachability probe,
a `ps` lookup, cleanup work and the telemetry query are never counted.

### Before you answer

Until hull reports that telemetry is on, Brig suppresses counting for a
sandbox boot and a sandbox stop. Neither has a terminal attached, so neither
is counted before you answer.

The command that hands your terminal to the agent is the one exception, when
the standard input of Brig is a terminal. There Brig lets the prompt from
hull through, so you are asked before anything is sent. Whether that prompt
always comes before the send depends on hull's code, which is outside the
Brig repository.

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

The report is one of four states: on, off, not answered yet, and cannot
tell.

Telemetry is on:

```
telemetry: on
A sandbox boot and the command that takes your terminal each count
once. `brig telemetry off` stops it.
```

Telemetry is off because a variable in your shell decided it:

```
telemetry: off
DO_NOT_TRACK=1 in this environment turns it off, and beats any
answer recorded on this machine.
```

Telemetry is off because you recorded the answer with `brig telemetry off`:

```
telemetry: off
The answer is recorded on this machine. `brig telemetry on` reverses it.
```

Nobody answered yet:

```
telemetry: not answered yet
Nothing goes out for a sandbox boot until you answer, and the first
command that hands your terminal to an agent asks the question.
`brig telemetry on` or `brig telemetry off` answers it now.
```

A recorded answer stops counting as an answer once hull widens what it
collects. `status` then reports this unanswered state until you answer
again.

Brig cannot tell:

```
telemetry: cannot tell
The sandbox runtime did not report a state this version of brig
recognises. Boots are not counted while that is true.
```

Brig reads one line from the runtime. If Brig does not recognize the phrase,
it treats the question as unanswered, reports `cannot tell` and does not
count boots. An older
runtime or a newer one can cause this.

The runtime implements no telemetry reporting, which is `nerdctl` on Linux
today:

```
telemetry: off
The sandbox runtime on this host sends nothing, so there is nothing
to turn off.
```

The report and `brig telemetry --help` do not name the runtime, so you can
opt out without knowing which binary sends. `brig telemetry` takes no
`--json` form.

## What an event carries

The events come from hull, and
[hull's own telemetry docs](https://github.com/brig-sh/hull/blob/main/docs/telemetry.md)
are the field-by-field reference. The summary below, and the list in
[What is never collected](#what-is-never-collected), are not checked against
hull's source.

The envelope of an event carries:

- the product name and version
- the OS version and CPU architecture
- an install identifier generated on your machine
- a timestamp and a checksum

The events that hull sends for an operation also carry:

- which runtime operation ran, and whether it succeeded, with a failure
  bucketed into a class such as `network` or `permission`, never its
  error text
- which hypervisor backend booted, and whether the boot worked
- how long the sandbox lived
- while a session is attached to the sandbox, the memory use and CPU share
  of its VMM process, first a few seconds after the attach and then every
  30 seconds
- if Brig or the runtime panics, the panic type, with a stack trace whose
  paths are trimmed

## What is never collected

The list below is a commitment from hull, and it is not limited to one
build:

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
