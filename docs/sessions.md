# Sessions, homes and projects

[quickstart.md](quickstart.md) is the walkthrough. This page is the
reference underneath it.

## The ref

Every ref is `<agent>` or `<agent>@<label>`. An empty label means the
agent's default session, so `claude` and `claude@refactor` are two
independent sessions of one agent. Each gets its own guest home and its own
sandbox.

`claude` and `desktop` are aliases, resolved to the built-in agents
`claude-code` and `claude-desktop`. Those are the only two aliases Brig
resolves. A real agent always wins over an alias. If you create a profile
of your own literally named `claude`, it takes the word outright. The
built-in `claude-code` is then reachable only by its full name.

A label always picks the guest home and the sandbox name, for every agent.
Reaching the agent itself as a display name is narrower. Brig passes the
label to the agent only when the resolved profile is literally
`claude-code`. It does that only on `brig run`, never on `brig sh`. No
other profile, not even `claude-desktop`, receives it this way.

## The guest home

The guest home is a host directory mounted as the agent's home:

```
~/brig/<resolved agent name>[-<label>]
```

The resolved name is the agent's own, not the alias you typed, so
`claude`'s guest home is `~/brig/claude-code`. `claude@refactor`'s is
`~/brig/claude-code-refactor`, a sibling of the default, not a directory
inside it. The guest home is host-backed. It is an ordinary directory on
your disk, and it survives every command this page covers, until you
remove it by hand.

`brig ls` and `brig info` both print this directory as `WORKSPACE`, and the
`BRIG_WORKSPACE` environment variable names it too. Both are the CLI's
labels for the guest home, the term this page uses everywhere else.

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

Run the same session against a different project than it last used, and
Brig recreates the sandbox. It warns you, stops the running sandbox,
removes it, and starts a new one with the new project mounted. A mount
cannot be attached to a live guest. The same recreate happens the first
time you name a project on a session that had none, and when
`--no-project` drops one it had. Because it is a fresh sandbox, the
recreate takes everything inside the old guest with it, including a
`claude-code` login that had landed on memory.

A remembered project that has since gone off disk does not refuse the run.
The run proceeds with no project, and names the missing directory in the
restart warning.

## What survives

Four things can hold state for a session. They are the guest home on host
disk, a memory-backed mount inside the guest, the sandbox itself, and what
Brig recorded about the session. Only two of the eight built-in agents,
`claude-code` and `claude-desktop`, declare a memory-backed mount at all.
For the other six, `codex`, `cursor`, `gemini`, `grok`, `opencode` and
`ubuntu`, the whole guest home is the host-backed share above. An in-guest
login for those agents lands on host disk and survives every stop.

| Event | Guest home | Memory-backed mount (`claude-code`, `claude-desktop`) | The sandbox | What Brig recorded |
| --- | --- | --- | --- | --- |
| The agent exits | kept | kept, the sandbox is still up | still running | unchanged |
| `brig stop` | kept | gone with the sandbox | stopped, still named in `brig ls` | kept |
| `brig rm` | kept | gone | removed | dropped |
| `brig rm --all` | kept, every session | gone | every sandbox removed | dropped, every session |
| A host reboot | kept, an ordinary host directory | gone, guest memory cannot survive a reboot | the runtime's own business, not established here | kept as host files |

`brig stop` keeps the sandbox's name, its row in `brig ls`, and what Brig
recorded about the session. `brig rm` drops the last of those too. Neither
touches the guest home.

A host reboot cannot take your work: the guest home is a host directory,
and what Brig recorded is host files. It does take everything inside the
sandbox, including a `claude-code` login that had landed on memory.
Whether the sandbox itself is still listed afterward is the runtime's own
business. If it has gone, the next `brig ls` forgets it, and the next run
boots a new one.

Brig keeps its own bookkeeping, the session index among it, under
`~/.brig`. That layout is not a stable interface: do not build a script
against its files directly.

## What a label can contain

A label is turned into a slug: lowercased, and every character outside
`[A-Za-z0-9._-]` replaced with a dash. Runs of dashes are collapsed, and
leading and trailing dots and dashes are trimmed. Nothing is shortened.

The `<agent>@<label>` form is strict. If a label is not already its own
slug, Brig refuses it and names the slug it maps to, instead of rewriting
it for you. It also refuses more than one `@`, a ref naming no agent, a
trailing `@` with no label, and a label with no usable characters. A
label that names another agent's own default session is refused too.

The retiring `--name` flag is the lenient spelling. It sanitizes the name
itself and warns which directory it actually used, so `--name "Refactor
Sprint"` becomes `refactor-sprint` with a warning, not a refusal. Only the
`@` form insists you already typed the slug.

## Printing the ref

`brig ls` prints the canonical agent name plus the label, not the alias
you typed: a session started as `claude@refactor` shows as
`claude-code@refactor`. `run --json`'s Run object instead prints the ref
exactly as typed, so `brig --json run claude` reports `"ref": "claude"`,
not `"claude-code"`. A script reading both commands sees two different
strings for the same session.
