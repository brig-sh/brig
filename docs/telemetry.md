# What Brig counts, and how to turn it off

Brig keeps no telemetry store and runs no collector of its own. The sandbox
runtime it drives is what actually counts anything and sends it. On macOS
the runtime is `hull`. On Linux the runtime is `nerdctl`, and nothing is
sent at all.

## Turn it off

```bash
brig telemetry off
```

Durable, on this machine, whether Brig is the one asking again or not.
`DO_NOT_TRACK=1` in your environment does the same thing without recording
anything, and beats any answer already on disk:

```bash
DO_NOT_TRACK=1 brig run claude
```

## What is counted

Three things, each once per Brig command:

- **A sandbox boot.**
- **A sandbox stop**, including the stop inside a restart, when Brig recreates
  a sandbox whose shares or policy went stale. `brig rm` is not counted.
- **The command that hands your terminal to the agent**, which is `run`
  without `--json`, `sh`, or the `--json` child path either one takes.

Nothing else Brig does on its own behalf is ever counted. A reachability
probe, a `ps` lookup, cleanup work, and the telemetry query itself all run
with counting suppressed. One command you type counts at most once, never
once per internal step it takes.

Brig suppresses counting for a sandbox boot until hull reports a recorded
answer. A boot with no terminal attached is never counted before anyone has
answered. The one exception is the command that hands your terminal to the
agent. There Brig lets hull's own prompt through instead of suppressing it,
so you are asked before anything is sent. Whether that prompt always
precedes the send is a fact about hull's own code, outside this checkout.

## `brig telemetry status`, `on`, `off`

```bash
brig telemetry status
```

`status` is the default when you give no verb, and a bare `brig telemetry`
changes nothing. `on` and `off` record an answer. `brig telemetry` then
reports the state that resulted, not what you asked for, because the two
can differ. Recording `on` while `DO_NOT_TRACK` is set in your shell still
reports off, and names the variable.

The report is one of four states, in Brig's own words. `off` prints one of
two wordings, depending on whether a variable in your shell decided it or
an answer recorded on disk did:

```
telemetry: on
A sandbox boot and the command that takes your terminal each count
once. `brig telemetry off` stops it.
```

```
telemetry: off
DO_NOT_TRACK=1 in this environment turns it off, and beats any
answer recorded on this machine.
```

That is the wording when a variable decided it. Recording the answer
yourself, with `brig telemetry off`, reports it instead as:

```
telemetry: off
The answer is recorded on this machine. `brig telemetry on` reverses it.
```

```
telemetry: not answered yet
Nothing goes out for a sandbox boot until you answer, and the first
command that hands your terminal to an agent asks the question.
`brig telemetry on` or `brig telemetry off` answers it now.
```

```
telemetry: cannot tell
The sandbox runtime did not report a state this version of brig
recognises. Boots are not counted while that is true.
```

The last of the four is deliberate rather than a guess. Brig reads one line
from the runtime. A phrase it does not recognize is read as unanswered in
effect. This happens whether the phrase comes from an older or a newer
runtime speaking a dialect this version has never seen. Nothing is counted,
and Brig reports `cannot tell` rather than assume either on or off.

A recorded answer stops counting as an answer once hull widens what it
collects. `status` then reports the same unanswered state a fresh install
gets, until you answer again.

Neither the report nor `brig telemetry --help` names the runtime underneath.
The point of the command is that opting out does not require knowing which
binary is doing the sending.

On a runtime that implements no telemetry reporting at all, which is
`nerdctl` on Linux today, the report is direct instead of an error:

```
telemetry: off
The sandbox runtime on this host sends nothing, so there is nothing
to turn off.
```

`brig telemetry` takes no `--json` form. Each report above is the whole
answer.

## Where an event goes, and what it carries

hull is what sends, so
[hull's own telemetry docs](https://github.com/brig-sh/hull/blob/main/docs/telemetry.md)
are the field-by-field reference. What follows is the shape Brig's earlier
README carried, moved to this page. It is not checked against hull's
source, which is a separate repository this checkout does not include.

An event's envelope carries the product name and version, the OS version
and CPU architecture. It also carries an install identifier generated on
your machine, a timestamp and a checksum. On top of that, one event per
operation carries:

- which runtime operation ran, and whether it succeeded, with a failure
  bucketed into a class such as `network` or `permission`, never its
  error text
- which hypervisor backend booted, and whether the boot worked
- how long the sandbox lived
- if Brig or the runtime panics, the panic type, with a stack trace whose
  paths are trimmed

What is never collected, as hull's own commitment rather than a description
of one build:

- guest home paths, or any host path
- repository names, branches or remotes
- command arguments, including the agent's own
- agent prompts, or anything the agent read or wrote
- secret names, secret values, or which credentials were forwarded
- image names and registry references
- network destinations the guest reached
- file metadata: names, sizes, timestamps, counts

`HULL_TELEMETRY_DISABLED=1` reaches hull the same way `DO_NOT_TRACK=1` does,
untouched, and both beat an answer already recorded on disk.

## See also

[docs/cli.md](cli.md) has the flags and exit codes for every verb, including
`brig telemetry`. [docs/security.md](security.md) covers the rest of the
boundary a sandbox keeps.
