# Moving off the retired spellings

Brig renamed most of its commands while it was still a prerelease. Every old
spelling on this page still works today, except the ones under
[Removed](#removed). Each one prints one line on stderr naming its
replacement, in this form:

```
brig: `brig profiles` is now `brig agent ls`
```

The old spellings are scheduled for removal in 0.3. `brig run` is never
removed. If you have a script written against an older spelling, this page is
the whole list of what to change. One entry is not a spelling at all: a word
on the `brig run` line changed meaning, and it prints nothing. See
[One word whose meaning changed](#one-word-whose-meaning-changed).

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
always gives the command a terminal in the guest, where `exec` gave it one
only when brig's own stdin was a terminal. Output piped or redirected from
`sh` therefore comes through that terminal: lines end in CRLF, and stderr is
mixed into stdout.

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
exists.

When the first word is a profile function, or the login profile sets an
`EXIT` trap, bash stays running as the command's parent. A `SIGTERM` sent to
the session then ends bash and leaves the command running until the sandbox
stops.

The words are now positional parameters, so a login profile that runs a
top-level `shift` or `set --` rewrites them and so the command. This applies
to `/etc/profile` in the image and to a `~/.bash_profile` in your own guest
home. Move either into a function, where bash scopes it to the call. See
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

These keys still parse in a profile file for one more release. `brig agent
edit` on an old file is the quickest way to see the current spelling, because
the header comment documents every field.

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
