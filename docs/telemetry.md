# What brig counts, and how to turn it off

brig keeps no telemetry store and runs no collector of its own. The sandbox
runtime it drives is what actually counts anything and sends it, so what
follows is true on macOS, where the runtime is `hull`, and not on Linux,
where the runtime is `nerdctl` and nothing is sent at all.

## Turn it off

```bash
brig telemetry off
```

Durable, on this machine, whether brig is the one asking again or not.
`DO_NOT_TRACK=1` in your environment does the same thing without recording
anything, and beats any answer already on disk:

```bash
DO_NOT_TRACK=1 brig run claude
```

## What is counted

Exactly two things, each once per brig command:

- **A sandbox boot.**
- **The command that hands your terminal to the agent**, which is `run`
  without `--json`, `sh`, or the `--json` child path either one takes.

Nothing else brig does on its own behalf is ever counted. A reachability
probe, a `ps` lookup, cleanup work, and the telemetry query itself are all
run with counting suppressed, so one command you typed counts at most once,
never once per internal step it happens to take.

## Until an answer is on file

hull ships with telemetry on by default and asks on first use. Left alone,
that sends an event for a fresh install's first boot before anyone has
answered anything, with the question then appearing later, on an unrelated
interactive step.

brig closes that gap for everything it does on your behalf. A sandbox boot
runs with no terminal attached, so there is nobody to ask, and brig
suppresses counting for it until hull reports a recorded answer.

The one exception is the command that hands your terminal to the agent.
There, brig lets the question through instead of suppressing it, so hull can
put it to you before anything is sent. Suppressing that invocation too
leaves the prompt never appearing, and the question stays unanswered
forever rather than answered once.

Whether hull's own prompt genuinely precedes every send on that one
invocation is a fact about hull's code, which lives in its own repository
and is not part of this checkout.

A recorded answer stops being read as an answer when hull widens what it
collects: `brig telemetry status` then reports the same unanswered state a
fresh install gets, and nothing from the wider list is sent until you answer
again.

## `brig telemetry status`, `on`, `off`

```bash
brig telemetry status
```

`status` is the default when you give no verb, and a bare `brig telemetry`
changes nothing. `on` and `off` record an answer, then `brig telemetry`
reports the state that resulted rather than echoing what you asked for,
because the two can differ: recording `on` while `DO_NOT_TRACK` is set in
your shell still reports off, with the variable named.

The report is one of four states, in brig's own words:

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

The last of the four is deliberate rather than a guess. brig reads one line
from the runtime, and a phrase it does not recognize, an older or a newer
runtime speaking a dialect this brig has never seen, is read as unanswered
in effect (nothing is counted) and reported as `cannot tell` rather than
being assumed to mean either on or off.

Neither the report nor `brig telemetry --help` names the runtime underneath.
The point of the command is that opting out does not require knowing which
binary is doing the sending.

On a backend that implements no telemetry reporting at all, which is
`nerdctl` on Linux today, the report is direct instead of an error:

```
telemetry: off
The sandbox runtime on this host sends nothing, so there is nothing
to turn off.
```

`brig telemetry` takes no `--json` form. Every report above is the whole
answer, in one of the four shapes.

## Where an event goes, and what it carries

hull is what sends, so
[hull's own telemetry docs](https://github.com/brig-sh/hull/blob/main/docs/telemetry.md)
are the field-by-field reference. What follows is the shape brig's earlier
README carried, moved to this page rather than checked against hull's
source, which is a separate repository this checkout does not include.

An event carries an envelope of product name and version, the OS version and
CPU architecture, an install identifier generated on your machine, a
timestamp and a checksum. On top of that: which runtime operation ran and
whether it succeeded, with a failure bucketed into a coarse class such as
`network` or `permission` rather than its error text, which hypervisor
backend booted and whether the boot worked, how long a sandbox lived, and,
if brig or the runtime panics, the panic type with a stack trace whose paths
are trimmed.

What is never collected, as hull's own commitment rather than a description
of one build:

- workspace paths, or any host path
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
