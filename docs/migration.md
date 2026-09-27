# Moving off the retired spellings

Brig renamed most of its commands while it was still a prerelease. Every old
spelling on this page still works today, except the ones under
[Removed](#removed). Each one prints a notice on stderr that names its
replacement and the release that removes it, in this form:

```
brig: `brig profiles` is now `brig agent ls`
  ↳ the old spelling is removed in v0.4.0
```

The old spellings are removed in v0.4.0. `brig exec` is the exception: it
stays until `brig sh` can pipe a command's output, and its notice says that
instead of a release. `brig run` is never removed.
[stability.md](stability.md#retired-spellings) has the rule.

If you have a script written against an older spelling, this page is the
whole list of what to change. Some changes go beyond spelling: a word on
the `brig run` line changed meaning, and it prints nothing. See
[One word whose meaning changed](#one-word-whose-meaning-changed). New
sandboxes also use a different [network default](#network-defaults).
`brig sh` now passes its words to the guest command as arguments, so a
script typed as one quoted word no longer runs. See
[A quoted script on `brig sh`](#a-quoted-script-on-brig-sh).

To find out whether a script still uses one, run
[`script/check-retired-spellings.sh`](../script/check-retired-spellings.sh)
over your own files, or watch stderr for the notice.

## Verbs

| Retired | Current |
| --- | --- |
| `brig profiles`, `brig agents` | `brig agent ls` |
| `brig profile <verb>` | `brig agent <verb>` |
| `brig template` | `brig agent` |
| `brig import` | `brig agent import` |
| `brig export` | `brig agent export` |
| `brig policies` | `brig policy ls` |
| `brig create <ref>` | `brig run -d <ref>` |
| `brig exec <ref> -- <cmd>` | `brig sh <ref> <cmd>` |
| `brig env <ref>` | `brig info <ref>` |
| `brig reset` | `brig rm --all` |

`brig rm --all` asks before it removes anything. A script has no terminal to
answer on, so a script that ran `brig reset` unattended needs
`brig rm --all -y`. The same applies to `brig reset` itself: without a
terminal it also refuses unless `-y` is passed.

`brig exec` runs its command without a guest pty, and `brig sh` does not
yet. Until [#335](https://github.com/brig-sh/brig/issues/335) lands, `brig
exec` is the way to pipe a command's output cleanly, so it has no removal
release yet.

There is deliberately no `brig template edit`. The retired group kept only the
verbs it already had, so asking for that one is an error rather than a
deprecation notice.

### Removed

These no longer run. Each is refused as a usage error, exit code 2, that
names its replacement:

```
brig: `brig shell` was removed; use `brig sh <ref> [command...]`
```

| Removed | Spelling | Current |
| --- | --- | --- |
| 0.3.0 | `brig shell <ref>` | `brig sh <ref>` |

## Subverbs

| Retired | Current |
| --- | --- |
| `brig agent list` | `brig agent ls` |
| `brig agent save` | `brig agent export` |
| `brig agent load` | `brig agent import` |
| `brig policy list` | `brig policy ls` |
| `brig secret list` | `brig secret ls` |
| `brig secret rm` | `brig secret delete` |

## Flags

| Retired | Current |
| --- | --- |
| `-t IMAGE` | `--image IMAGE` |
| `-m MB` | `--mem MB` |
| `-n NAME`, `--name NAME` | `<agent>@<label>` |
| `-w PATH`, `--workspace PATH` | `--home PATH` |

Each still writes the same value, and the inline form (`-t=myimage:1`) warns
too. A value that merely looks like a retired flag is left alone, so
`brig run claude --name -t` warns about `--name` and not about `-t`.

`--memory` is a current spelling of `--mem`. It is not retired, but the help
text does not teach it and completion does not offer it.

## One flag whose position changed

`-q` and `--quiet` moved to the global position, left of the verb:

```bash
brig -q run claude      # current
brig run claude -q      # still works, prints a notice
```

The notice names the move rather than a new spelling:

```
brig: `brig <verb> <ref> -q` is now `brig -q <verb> <ref>`
  ↳ the old spelling is removed in v0.4.0
```

`--json` is different. It is accepted on both sides of the verb permanently,
and prints no notice either way.

## One word whose meaning changed

Up to 0.1.0-rc17, the second bare word on a `brig run` line went to the
agent. From 0.1.0-rc18 it is the project directory Brig mounts, and the
agent starts in it:

```bash
brig run claude ~/code/demo   # mounts ~/code/demo at /work/demo
brig run claude -- src        # passes src to the agent
```

This one does not keep working the old way, and it prints no notice.
For a line written against rc17, such as `brig run claude src`:

- If `src` is a directory, Brig mounts it read-write at `/work/src` and
  starts the agent there. At the default verbosity nothing is printed
  about it. `brig info claude` shows the mounted project without running
  anything, and `brig --verbose run` prints it before the boot.
- If it is not, Brig refuses the run and says to put it after `--`.

`--` ends Brig's own parsing, so anything after it reaches the agent
untouched. That is the spelling that keeps the old meaning.

0.1.0-rc18 and 0.2.0 printed a notice about this on every run that named a
project, unless `-q` was given. It is gone.
`script/check-retired-spellings.sh` cannot find these lines either, because
the line is still valid and only its meaning changed. Look for `brig run`
lines with a second bare word after the agent.

## A quoted script on `brig sh`

`brig sh <ref> <command...>` used to join its trailing words with spaces and
hand the result to `bash -lc` as a script. That threw away every argument
boundary, so `brig sh ubuntu sh -c 'echo FIRST; echo SECOND'` printed a blank
line and `SECOND`. Each word now reaches the guest as one argument, the way
`brig exec <ref> -- <cmd>` passed them. Two differences are left between the
two. `sh` runs the command under a login shell and `exec` does not. `sh` also
always asks for a terminal in the guest, where `exec` asked for one only when
brig's own stdin was a terminal. On hull, output piped or redirected from `sh`
therefore comes through that terminal: lines end in CRLF, and stderr is mixed
into stdout. On Linux the runtime can refuse the terminal, and then the
command does not run at all. docker refuses it when brig's stdin is not a
terminal. nerdctl refuses it when none of brig's stdin, stdout and stderr is
one.

`brig run` on a `kind: shell` profile such as `ubuntu` runs its trailing words
the same way `sh` does, and changed with it.

A line that relied on the join, passing shell syntax as a single quoted word,
now fails in the guest instead of running it, and brig prints a hint naming
`-c` first. Put `-c` in front of the script, which runs it under the login
shell the way the join did:

```bash
brig sh claude 'ls /work | wc -l'      # no longer runs
brig sh claude -c 'ls /work | wc -l'   # runs it as a script
```

A variable assignment in front of the command is shell syntax too, even
unquoted. `brig sh claude FOO=bar npm test` now looks for a command named
`FOO=bar` and exits 127. Pass the variable through `env`, which keeps the
words as they are, or write the line as a script:

```bash
brig sh claude FOO=bar npm test          # no longer runs
brig sh claude env FOO=bar npm test      # runs npm test with FOO set
brig sh claude -c 'FOO=bar npm test'     # the same, as a script
```

The command still runs under a login shell, so its environment is unchanged,
and a shell builtin such as `ulimit` or a function the login profile defines,
such as `nvm`, still works as the first word. A first word that starts with
`-` is a command name, not an option, and exits 127 when no such command
exists. The exception is `-c`, alone or combined as in `-ec`, which runs the
next word as a script.

When the first word is a profile function, or the login profile sets a trap,
bash stays running as the command's parent. A `SIGTERM` sent to the session
then ends bash and leaves the command running until the sandbox stops.

The words are now positional parameters, so a login profile that runs a
top-level `shift` or `set --` rewrites them and so the command. This applies
to `/etc/profile` in the image. It also applies to a `.bash_profile` in your
guest home when the profile mounts that home at the guest user's `$HOME`.
Move either into a function, where bash scopes it to the call. See
[guest-image.md](guest-image.md).

## Session names

`--name` is the flag this replaced most visibly. A session is now part of the
ref:

```bash
brig run claude@refactor          # current
brig run claude --name refactor   # still works, prints a notice
```

`brig run claude` is the default session of the `claude-code` agent.
`claude@refactor` is a second session, with its own sandbox and its own
guest home. [docs/sessions.md](sessions.md) explains what each session
keeps separate.

`--name` is also Claude Code's own flag. Anything you type to the right of
the ref goes to the agent untouched. `brig run claude -- --name x` sends
`--name x` to Claude Code, and Brig never sees it.

## Profile keys

These keys still parse in a profile file until v0.4.0. They print no notice
at run time. `brig agent edit` on an old file is the quickest way to see the
current spelling, because the header comment documents every field.

| Retired key | Current |
| --- | --- |
| `hostCredential:` | **removed**: declare a secret and run `brig secret import <agent>` |
| `shell:`, `gui:` booleans | `kind:` |
| `forward:` | `env:`, with `ref: env.<name>` |
| `statePaths:` | `volumes:` |

Declaring a retired key beside its replacement is an error, not a warning.
`kind:` beside `shell:` or `gui:` is refused only when the two disagree.
`forward:` beside an `env:` entry of the same name, and `statePaths:` beside
`volumes:`, are refused whatever their values. Brig refuses the profile
rather than guessing which one you meant.

## Network defaults

New sandboxes on `hvi` and Linux now default to `isolated`, a network of
their own. They still reach the internet. This change separates sandbox
networks; it adds no egress policy and makes no new claim about access to
host services.

Existing sandboxes keep the posture recorded when they started. For an
older session with no posture record, Brig inspects its sandbox's runtime
configuration to recover `shared`, `isolated` or `offline` when possible.
An unrecorded Hull gateway named `sandbox-*.sock` recovers as isolated when
a readable, nonempty `.spec` remains beside its recorded socket path. Without
that evidence it stays unknown: `brig stop` removes the spec, so its absence
does not mean shared. Restore its posture record or choose `--network`
explicitly; see the [recovery limits](policies.md#network-postures), including
the ambiguity of stale specs left beside old shared overrides.
Upgrading does not restart it to apply the new profile default.
A sandbox the runtime confirms is absent gets the default for a new sandbox.

If the runtime cannot establish an existing sandbox's posture, Brig
refuses a flagless run instead of guessing. Choose its intended posture
explicitly to recreate it. To move a sandbox deliberately, run:

```bash
brig run claude --network isolated
```

That restarts the sandbox and disconnects any session using it. If one
sandbox needs to reach another, start both with `--network shared`, set
`BRIG_NETWORK=shared`, or put `network: shared` in their profiles.

The six `hvi` profiles explicitly use `isolated`; the graphical
`claude-desktop` profile uses `shared` for `vz`. The unpublished `cursor`
profile leaves its posture unset: it isolates on Linux and `hvi`, and takes
the shared fallback on `vz` or `qemu`. Any profile without a network choice
falls back to `shared` on `vz` or `qemu`, and `brig info` reports why. An explicit `isolated`
remains refused on `vz` and `qemu`.

If you override an `hvi` profile to `vz` or `qemu`, also override its
explicit isolated posture. For example, on macOS 14:

```bash
BRIG_HYPERVISOR=vz brig run claude --network shared
```

An exported profile is a copy: it keeps whatever `network:` it names.
Add that field if you want a custom profile to keep a particular posture
for new sessions. [Network postures](policies.md#network-postures) covers
the precedence, backend exceptions and resource costs.

## Settings

| Retired | Current |
| --- | --- |
| `BRIG_TEMPLATE_DIR` | `BRIG_PROFILE_DIR` |

`BRIG_TEMPLATE_DIR` still works until v0.4.0, and prints no notice.
`BRIG_PROFILE_DIR` wins when both are set.

## The words `agent` and `profile`

Both are current, and they name different things. The rename landed on the
command surface only:

- **agent** is the CLI noun. `brig agent ls`, `brig agent edit`, and the `<ref>`
  every verb takes.
- **profile** is the file format, the directory and the environment variable.
  A profile is a YAML file in `$XDG_CONFIG_HOME/brig`, and `BRIG_PROFILE_DIR`
  points somewhere else.

So you edit a profile file to change an agent. [docs/profiles.md](profiles.md)
is the reference for the file format.
