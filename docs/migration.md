# Moving off the retired spellings

Every retired Brig spelling still works today, except the ones marked
removed. Each retired command and flag prints a notice on stderr. The notice names the
replacement and the release that removes the old spelling:

```
brig: `brig profiles` is now `brig agent ls`
  ↳ the old spelling is removed in v0.5.0
```

When stderr is not a terminal, both lines start with `brig:`.

Brig removes the old spellings in v0.5.0, with one exception. `brig exec`
stays until `brig sh` can pipe the output of a command, and its notice says
that instead of a release. `brig run` is never removed.
[Stability](stability.md#retired-spellings) has the rule.

To find retired spellings in a script, run
[`script/check-retired-spellings.sh`](../script/check-retired-spellings.sh)
over it, or watch stderr for the notice.

Three changes are not spellings:

- A word on the `brig run` line
  [changed meaning](#one-word-whose-meaning-changed), and Brig prints no
  notice.
- New sandboxes use a different [network default](#network-defaults).
- `brig sh` passes its words to the guest command as arguments, so a script
  typed as one quoted word does not run. See
  [A quoted script on `brig sh`](#a-quoted-script-on-brig-sh).

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

`brig rm --all` and `brig reset` both ask before they remove anything.
Without a terminal to answer on, both refuse unless you pass `-y`. A script
that ran `brig reset` unattended needs `brig rm --all -y`.

`brig exec` runs its command without a guest pty, and `brig sh` does not yet
([#335](https://github.com/brig-sh/brig/issues/335)). Until it does, use
`brig exec` to pipe the output of a command cleanly.

`brig template edit` does not exist. The retired group has only the verbs
that it had before. That line prints the notice and then fails, and the
failure names `brig agent edit`.

### Removed

Brig refuses each removed spelling as a usage error, with exit code 2. The
refusal names the replacement:

```
brig: `brig shell` was removed; use `brig sh <ref> [command...]`
```

| Removed in | Spelling | Current |
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

Each retired flag still sets the same value. The inline form
(`-t=myimage:1`) also prints the notice. Brig leaves alone a value that looks
like a retired flag. For example, `brig run claude --name -t` warns about
`--name` and not about `-t`.

`--memory` is a current spelling of `--mem`. The help text does not show it,
and completion does not offer it.

## One flag whose position changed

`-q` and `--quiet` moved to the global position, left of the verb:

```bash
brig -q run claude      # current
brig run claude -q      # still works, prints a notice
```

The notice names the move:

```
brig: `brig <verb> <ref> -q` is now `brig -q <verb> <ref>`
  ↳ the old spelling is removed in v0.5.0
```

`--json` is accepted on both sides of the verb and prints no notice.

## One word whose meaning changed

The second bare word on a `brig run` line is the project directory that Brig
mounts, and the agent starts in it. Older prereleases of 0.1.0 passed that
word to the agent.

```bash
brig run claude ~/code/demo   # mounts ~/code/demo at /work/demo
brig run claude -- src        # passes src to the agent
```

To keep the old meaning, put the word after `--`. `--` ends Brig's parsing,
so anything after it reaches the agent untouched.

Brig prints no notice for a line that has the old meaning, such as
`brig run claude src`:

| `src` | What Brig does |
| --- | --- |
| is a directory | mounts it read-write at `/work/src` and starts the agent there. At the default verbosity, Brig prints nothing about the mount |
| is not a directory | refuses the run and says to put `src` after `--` |

`brig info claude` shows the mounted project and runs nothing.
`brig --verbose run` prints the project before the boot.

`script/check-retired-spellings.sh` cannot find these lines, because each
line is still valid. Look for `brig run` lines with a second bare word after
the agent. The later prereleases of 0.1.0, and Brig 0.2.0, printed a notice
about this change on every run that named a project. Current releases print
no notice.

## A quoted script on `brig sh`

`brig sh <ref> <command...>` passes each word to the guest as one argument,
as `brig exec <ref> -- <cmd>` does. Older releases joined the trailing words
with spaces and gave the result to `bash -lc` as a script. The join lost
every argument boundary, so `brig sh ubuntu sh -c 'echo FIRST; echo SECOND'`
printed a blank line and `SECOND`.

`brig run` on a `kind: shell` profile such as `ubuntu` runs its trailing
words as `sh` does, and changed with it.

A line that passed shell syntax as a single quoted word now fails in the
guest. Brig prints a hint that names `-c` first. Put `-c` in front of the script. `-c` runs the script under the login
shell, as the join did:

```bash
brig sh claude 'ls /work | wc -l'      # no longer runs
brig sh claude -c 'ls /work | wc -l'   # runs it as a script
```

A variable assignment in front of the command is also shell syntax, even
unquoted. `brig sh claude FOO=bar npm test` now looks for
a command named `FOO=bar` and exits 127. Pass the variable through `env`,
which keeps the words as they are, or write the line as a script:

```bash
brig sh claude FOO=bar npm test          # no longer runs
brig sh claude env FOO=bar npm test      # runs npm test with FOO set
brig sh claude -c 'FOO=bar npm test'     # the same, as a script
```

The command still runs under a login shell, so its environment is unchanged.
A shell builtin such as `ulimit` still works as the first word. So does a
function that the login profile defines, such as `nvm`.

A first word that starts with `-` is a command name, and the line exits 127
when no such command exists. The exception is `-c`, alone or combined as in
`-ec`. It runs the next word as a script.

Two differences from `brig exec` remain. `sh` runs the command under a login
shell, and `exec` does not. `sh` always asks for a terminal in the guest, and
`exec` asks for one only when Brig's own stdin is a terminal. As a result,
output piped or redirected from `sh` depends on the runtime:

| Runtime | Effect |
| --- | --- |
| hull | the output comes through that terminal: lines end in CRLF, and stderr is mixed into stdout |
| docker | refuses the terminal when Brig's stdin is not a terminal. The command then does not run |
| nerdctl | refuses the terminal when none of Brig's stdin, stdout and stderr is a terminal. The command then does not run |

<details><summary>Login profiles that change the command</summary>

Bash stays running as the parent of the command in two cases: the first word
is a profile function, or the login profile sets a trap. A `SIGTERM` sent to
the session then ends bash and leaves the command running until the sandbox
stops.

The words are positional parameters. A login profile that runs a top-level
`shift` or `set --` rewrites them, and so rewrites the command. This applies
to `/etc/profile` in the image. It also applies to a `.bash_profile` in your
guest home when the profile mounts that home at the guest user's `$HOME`.
Move the `shift` or `set --` into a function, where bash scopes it to the
call. See [Guest image](guest-image.md).

</details>

## Session names

A session is part of the ref, which replaces `--name`:

```bash
brig run claude@refactor          # current
brig run claude --name refactor   # still works, prints a notice
```

`brig run claude` is the default session of the `claude-code` agent.
`claude@refactor` is a second session, with its own sandbox and its own
guest home. [Sessions](sessions.md) explains what each session keeps
separate.

`--name` is also Claude Code's own flag. Until v0.5.0, Brig still reads a
`--name` that stands before the agent's own arguments. To pass it to the
agent, put it after `--`: `brig run claude -- --name x` sends `--name x` to
Claude Code.

## Profile keys

`shell:`, `gui:`, `forward:` and `statePaths:` still parse in a profile file
until v0.5.0, and print no notice at run time. Brig refuses a profile that
still has `hostCredential:`, as it refuses any field that it does not know.
`brig agent export claude-code` prints a current profile, and its header
comment documents every field.

| Retired key | Current |
| --- | --- |
| `hostCredential:` | removed in 0.3.0: declare the credential under `secrets:` and run `brig secret import <agent>` |
| `shell:`, `gui:` booleans | `kind:` |
| `forward:` | `env:`, with `ref: env.<name>` |
| `statePaths:` | `volumes:` |

A retired key beside its replacement is an error:

| Keys | Refused |
| --- | --- |
| `kind:` beside `shell:` or `gui:` | only when the two disagree |
| `forward:` beside an `env:` entry of the same name | whatever their values |
| `statePaths:` beside `volumes:` | whatever their values |

## Network defaults

A new sandbox on `hvi` or Linux defaults to `isolated`, which is its own
network. It still reaches the internet. This change separates sandbox
networks. It adds no egress policy and makes no new claim about access to
host services.

| Sandbox | Posture |
| --- | --- |
| existing | the posture recorded when it started. An upgrade does not restart it to apply the new profile default |
| one that the runtime confirms is absent | the default for a new sandbox |
| existing, when the runtime cannot establish its posture | Brig refuses a run that names no posture. Name the one you want with `--network` or `BRIG_NETWORK`, and Brig recreates the sandbox with it |

To move a sandbox to another posture, run:

```bash
brig run claude --network isolated
```

That command restarts the sandbox and disconnects any session that uses it.
If one sandbox must reach another, start both with `--network shared`, set
`BRIG_NETWORK=shared`, or put `network: shared` in their profiles.

The shipped profiles name these postures:

| Profile | Posture |
| --- | --- |
| the six `hvi` profiles | `isolated`, named explicitly |
| the graphical `claude-desktop` profile | `shared`, for `vz` |
| the unpublished `cursor` profile | unset. It isolates on Linux and `hvi`, and takes the shared fallback on `vz` or `qemu` |
| any profile without a network choice | falls back to `shared` on `vz` or `qemu`, and `brig info` reports why |

Brig still refuses an explicit `isolated` on `vz` and `qemu`. If you override
an `hvi` profile to `vz` or `qemu`, also override its explicit isolated
posture. For example, on macOS 14:

```bash
BRIG_HYPERVISOR=vz brig run claude --network shared
```

An exported profile is a copy, and it keeps the `network:` that it names. To
make a custom profile keep a posture for new sessions, add that field.
[Network postures](policies.md#network-postures) covers the precedence,
backend exceptions and resource costs.

<details><summary>Older sessions with no posture record</summary>

For an older session with no posture record, Brig inspects the runtime
configuration of its sandbox. It recovers `shared`, `isolated` or `offline`
when possible.

An unrecorded Hull gateway named `sandbox-*.sock` recovers as isolated when
a readable, nonempty `.spec` remains beside its recorded socket path. Without
that evidence, the posture stays unknown. `brig stop` removes the spec, so a
missing spec does not mean shared.

For an unknown posture, restore the posture record or name a posture with
`--network`. See the [recovery limits](policies.md#network-postures),
including the ambiguity of stale specs left beside old shared overrides.

</details>

## Settings

| Retired | Current |
| --- | --- |
| `BRIG_TEMPLATE_DIR` | `BRIG_PROFILE_DIR` |
| `BRIG_CREDENTIALS_CMD` | removed: declare the credential under `secrets:`, then run `brig secret import <agent> <name> --from-command '<command>'` |

`BRIG_TEMPLATE_DIR` still works until v0.5.0, and prints no notice.
`BRIG_PROFILE_DIR` wins when both are set. Brig refuses a run that still sets
`BRIG_CREDENTIALS_CMD`.

## The words `agent` and `profile`

Both words are current, and they name different things. Only the commands
have new names:

- **agent** is the CLI noun: `brig agent ls`, `brig agent edit`, and the
  `<ref>` that every verb takes.
- **profile** is the file format, the directory and the environment variable.
  A profile is a YAML file in `$XDG_CONFIG_HOME/brig`, or in the directory
  that `BRIG_PROFILE_DIR` names.

To change an agent, edit its profile file. [Profiles](profiles.md) is the
reference for the file format.
