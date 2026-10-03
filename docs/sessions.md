# Sessions, homes and projects

[quickstart.md](quickstart.md) is the walkthrough. This page is the
reference underneath it.

## The ref

Every ref is `<agent>` or `<agent>@<label>`. An empty label means the
agent's default session, so `claude` and `claude@refactor` are two
independent sessions of one agent. Each gets its own guest home and its own
sandbox.

`claude` and `desktop` are aliases for the built-in agents `claude-code`
and `claude-desktop`. Brig resolves no other alias. An agent wins over an
alias: if you create a profile of your own named `claude`, that name runs
your profile, and the built-in `claude-code` is reachable only by its full
name.

A label picks the guest home and the sandbox name, for every agent. Brig
also passes the label to the agent as its display name, but only when the
resolved profile is `claude-code`, and only on `brig run`. `brig sh` does
not pass it, and no other profile receives it, `claude-desktop` included.

## The guest home

The guest home is a host directory mounted as the agent's home. When the
run names none, Brig creates one under its own state directory:

```
~/.brig/homes/<sandbox name>
```

The sandbox name is `brig-<resolved agent name>[-<label>]`. The resolved
name is the agent's own, not the alias you typed, so the guest home of
`claude` is `~/.brig/homes/brig-claude-code`. The guest home of
`claude@refactor` is `~/.brig/homes/brig-claude-code-refactor`, beside the
default one.

A guest home Brig created belongs to the sandbox. It survives `brig stop`
and a host reboot, and `brig rm` deletes it. The next `brig run` of the
same session starts from an empty home. The first run of such a session
says so on stderr. A first run whose boot fails deletes the home it
created, once the runtime confirms that no sandbox of that name exists.

A home can still be left behind, by a sandbox that was removed outside
Brig or by a run that was killed before its sandbox booted. It is deleted
before the next run of that session boots, once the runtime confirms that
the sandbox no longer exists, stopped or running. Brig says so on stderr
when it does.

To keep the guest home, name it with `--home <dir>` or `BRIG_WORKSPACE`. A
guest home you named is yours: Brig never deletes it, and it survives every
command this page covers. A named session appends `-<label>` to it.

Releases before 0.3.0 created the default guest home in
`~/brig/<resolved agent name>[-<label>]`, and kept it on `brig rm`. A
session started by one of those releases keeps that home while its sandbox
exists. After you remove it, pass `--home ~/brig/<resolved agent name>` to go on
using it.

`brig ls` and `brig info` print this directory as `WORKSPACE`, and the
`BRIG_WORKSPACE` environment variable sets it. Both are the CLI's names for
the guest home.

## The project

Name a directory on the run line:

```bash
brig run claude ~/code/demo
```

Brig mounts `~/code/demo` read-write at `/work/demo`, and the agent starts
there. Name no project, and Brig remounts whatever this session last ran
with, read back from its session index. `--no-project` mounts none.
`--no-project` is refused alongside a project named on the same line, and
refused on every verb but `run`.

Run a session against a different project than it last used, and Brig
recreates the sandbox, because a mount cannot be attached to a live guest.
It warns you, stops the running sandbox, removes it, and starts a new one
with the new project mounted. The same happens the first time you name a
project on a session that had none, and when `--no-project` drops one it
had. The new sandbox keeps nothing from inside the old guest, so a
`claude-code` login held in memory is lost.

If a remembered project no longer exists on disk, the run continues with no
project and names the missing directory in the restart warning.

## What survives

Four things can hold state for a session. They are the guest home on host
disk, a memory-backed mount inside the guest, the sandbox itself, and what
Brig recorded about the session. Only two of the eight built-in agents,
`claude-code` and `claude-desktop`, declare a memory-backed mount at all.
For the other six, `codex`, `cursor`, `gemini`, `grok`, `opencode` and
`ubuntu`, the whole guest home is the host-backed share above. An in-guest
login for those agents lands on host disk and survives every stop. It goes
on `brig rm` when Brig created the guest home.

| Event | Guest home | Memory-backed mount (`claude-code`, `claude-desktop`) | The sandbox | What Brig recorded |
| --- | --- | --- | --- | --- |
| The agent exits | kept | kept, the sandbox is still up | still running | unchanged |
| `brig stop` | kept | gone with the sandbox | stopped, still named in `brig ls` | kept |
| `brig rm` | deleted if Brig created it, kept if you named it | gone | removed | dropped |
| `brig rm --all` | as `brig rm`, every session | gone | every sandbox removed | dropped, every session |
| The sandbox is removed outside Brig, then `brig rm` | kept; the next run of the ref deletes it if Brig created it | gone with the sandbox | already gone; `rm` exits `3` | dropped |
| A host reboot | kept, an ordinary host directory | gone, guest memory cannot survive a reboot | depends on the runtime | kept as host files |

`brig stop` keeps the sandbox's name, its row in `brig ls`, and what Brig
recorded about the session. `brig rm` drops the last of those too, and
deletes the guest home when Brig created it. Neither touches a project or a
guest home you named. When the runtime reports no sandbox for the ref, as
after a removal outside Brig, `brig rm` forgets the session and names the
guest home it leaves. The next run of that ref deletes a home Brig created.

A host reboot keeps your work: the guest home is a host directory, and what Brig
recorded is host files. It loses everything inside the sandbox, including a
`claude-code` login held in memory. Whether the sandbox itself is still listed
afterward depends on the runtime. If it has gone, the next `brig ls` or
`brig rm` of its ref forgets it, and the next run boots a new one.

Brig keeps its own records, the session index among them, under `~/.brig`.
That layout is not a stable interface. Do not script against its files.

## What a label can contain

A label is turned into a slug: lowercased, and every character outside
`[A-Za-z0-9._-]` replaced with a dash. Runs of dashes are collapsed, and
leading and trailing dots and dashes are trimmed. Nothing is shortened.

The `<agent>@<label>` form is strict. If a label is not already its own slug,
Brig refuses it and names the slug it maps to. It also refuses more than one
`@`, a ref naming no agent, a trailing `@` with no label, and a label with no
usable characters. A label that names another agent's own default session is
refused too.

The retiring `--name` flag is lenient. It sanitizes the name and warns
which directory it used, so `--name "Refactor Sprint"` becomes
`refactor-sprint` with a warning.

## Printing the ref

`brig ls` prints the agent's own name plus the label, not the alias you
typed: a session started as `claude@refactor` shows as
`claude-code@refactor`. The Run object of `run --json` prints the ref as
typed, so `brig --json run claude` reports `"ref": "claude"`. A script
reading both commands sees two different strings for the same session.
