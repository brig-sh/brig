# Moving off the retired spellings

Brig v0.5.0 removed the retired spellings, with two exceptions. `brig exec`
stays until `brig sh` can pipe the output of a command, and its notice says
that instead of a release. The flat form of a
[policy document](#policy-documents) still parses, prints a notice, and is
removed in v0.6.0. `brig run` is never removed.
[Stability](stability.md#retired-spellings) has the rule.

Brig refuses a removed command or subcommand as a usage error, with exit code
2. The refusal names the replacement, and nothing runs:

```
brig: `brig profiles` was removed; use `brig agent ls`
```

A removed [flag](#flags) is refused the same way before the ref, and after
the ref it is the agent's own. A removed [profile key](#profile-keys) fails
the profile, and the error names the key that replaced it.

To find removed spellings in a script, run
[`script/check-retired-spellings.sh`](../script/check-retired-spellings.sh)
over it.

Three changes are not spellings:

- A word on the `brig run` line
  [changed meaning](#one-word-whose-meaning-changed), and Brig prints no
  notice.
- New sandboxes use a different [network default](#network-defaults).
- `brig sh` passes its words to the guest command as arguments, so a script
  typed as one quoted word does not run. See
  [A quoted script on `brig sh`](#a-quoted-script-on-brig-sh).

## Verbs

| Removed in | Spelling | Current |
| --- | --- | --- |
| 0.3.0 | `brig shell <ref>` | `brig sh <ref>` |
| 0.5.0 | `brig profiles`, `brig agents` | `brig agent ls` |
| 0.5.0 | `brig profile <verb>` | `brig agent <verb>` |
| 0.5.0 | `brig template <verb>` | `brig agent <verb>` |
| 0.5.0 | `brig import` | `brig agent import` |
| 0.5.0 | `brig export` | `brig agent export` |
| 0.5.0 | `brig policies` | `brig policy ls` |
| 0.5.0 | `brig create <ref>` | `brig run -d <ref>` |
| 0.5.0 | `brig env <ref>` | `brig info <ref>` |
| 0.5.0 | `brig reset` | `brig rm --all` |

`brig rm --all` asks before it removes anything. Without a terminal to answer
on, it refuses unless you pass `-y`. A script that ran `brig reset`
unattended needs `brig rm --all -y`.

### The one retired verb left

| Retired | Current |
| --- | --- |
| `brig exec <ref> -- <cmd>` | `brig sh <ref> <cmd>` |

`brig exec` still works, and prints a notice on stderr that names its
replacement:

```
brig: `brig exec` is now `brig sh`
  ↳ the old spelling stays until `brig sh` can pipe a command's output
```

When stderr is not a terminal, both lines start with `brig:`.

`brig exec` runs its command without a guest pty, and `brig sh` does not yet
([#335](https://github.com/brig-sh/brig/issues/335)). Until it does, use
`brig exec` to pipe the output of a command cleanly.

## Subverbs

| Removed in | Spelling | Current |
| --- | --- | --- |
| 0.5.0 | `brig agent list` | `brig agent ls` |
| 0.5.0 | `brig agent save` | `brig agent export` |
| 0.5.0 | `brig agent load` | `brig agent import` |
| 0.5.0 | `brig policy list` | `brig policy ls` |
| 0.5.0 | `brig secret list` | `brig secret ls` |
| 0.5.0 | `brig secret rm` | `brig secret delete` |

## Flags

| Removed in | Spelling | Current |
| --- | --- | --- |
| 0.5.0 | `-t IMAGE` | `--image IMAGE` |
| 0.5.0 | `-m MB` | `--mem MB` |
| 0.5.0 | `-n NAME`, `--name NAME` | `<agent>@<label>` |
| 0.5.0 | `-w PATH`, `--workspace PATH` | `--home PATH` |

These letters went back to the agents: `-n`, `-m` and `-w` are flags of
Claude Code, Codex, Gemini or opencode. After the ref, each is the agent's
own, as any flag that Brig does not own is. `brig run claude -w x` passes
`-w x` to Claude Code.

Before the ref there is no agent to own one, and Brig refuses it as a usage
error that names its replacement. The inline form (`-t=myimage:1`) is the
same flag:

```
brig: `-t` was removed; use `--image`
```

`--memory` is a current spelling of `--mem`. The help text does not show it,
and completion does not offer it.

## One flag whose position changed

`-q` and `--quiet` moved to the global position, left of the verb. v0.5.0
removed the run-line spelling:

```bash
brig -q run claude      # current
brig run -q claude      # refused
brig run claude -q      # -q goes to the agent
```

Before the ref, Brig refuses it and names where it went:

```
brig: `brig <verb> <ref> -q` was removed; use `brig -q <verb> <ref>`
```

After the ref it is the agent's, and Brig says that it did not read it:

```
brig: -q is one of brig's own flags, but here it is the agent's
  ↳ brig stopped reading the line at the argument before it
  → for brig to read it, put -q before the command
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

A session is part of the ref, which replaced `--name`:

```bash
brig run claude@refactor          # current
brig run --name refactor claude   # refused
```

`brig run claude` is the default session of the `claude-code` agent.
`claude@refactor` is a second session, with its own sandbox and its own
guest home. [Sessions](sessions.md) explains what each session keeps
separate.

`--name` is also Claude Code's own flag. Since v0.5.0, Brig does not read a
`--name` after the ref, so `brig run claude --name x` sends `--name x` to
Claude Code, as `brig run claude -- --name x` does.

## Profile keys

v0.5.0 removed `shell:`, `gui:`, `forward:` and `statePaths:`. Brig refuses a
profile that still has one, as it refuses any field that it does not know,
and the error names the key that replaced it.
`brig agent export claude-code` prints a current profile, and its header
comment documents every field.

| Removed in | Key | Current |
| --- | --- | --- |
| 0.3.0 | `hostCredential:` | declare the credential under `secrets:` and run `brig secret import <agent>` |
| 0.5.0 | `shell:`, `gui:` booleans | `kind:` |
| 0.5.0 | `forward:` | `env:`, with `ref: env.<name>` |
| 0.5.0 | `statePaths:` | `volumes:` |

`forward: [GH_TOKEN]` is this `env:` entry, which reads the variable from
Brig's own environment when it is set:

```yaml
env:
  - name: GH_TOKEN
    ref: env.GH_TOKEN
```

`statePaths:` named what persists and enforced nothing, so a profile that
drops it persists what it did before. `volumes:` is the list Brig acts on.

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
| the seven `hvi` profiles | `isolated`, named explicitly |
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

## Policy documents

A policy document uses the Kubernetes envelope: `kind: Policy`, `metadata:`
and `spec:`. The flat form, with `name:`, `desc:` and `egress:` at the top
level, still parses until v0.6.0. Brig converts it into the envelope as it
loads it.

`brig policy check` prints a notice for each bound policy that a file still
writes in the flat form:

```
brig: /home/you/.config/brig/policies/locked-down.yaml uses the flat policy form, which is now `kind: Policy` with `metadata:` and `spec:`
  ↳ the old spelling is removed in v0.6.0
  → to print it in the new form:  brig policy show locked-down
```

| Flat form | Envelope |
| --- | --- |
| no `kind:` | `kind: Policy` |
| `name:` | `metadata.name:` |
| `desc:` | `metadata.description:`, which is one line. Brig joins the lines of a longer `desc:` with spaces |
| `egress:` | `spec.egress:` |

`apiVersion:` does not change. For instance, this flat document:

```yaml
apiVersion: brig.sh/v1alpha1
name: locked-down
desc: only Anthropic's API
egress:
  default: deny
  allow:
    - host: api.anthropic.com
```

is this envelope:

```yaml
apiVersion: brig.sh/v1alpha1
kind: Policy
metadata:
  name: locked-down
  description: only Anthropic's API
spec:
  egress:
    default: deny
    allow:
      - host: api.anthropic.com
```

`brig policy show <name>` prints any policy in the envelope form. For a
policy in the flat form, its output is a valid policy file.

The flat form outlives the spellings v0.5.0 removed by one release, because
v0.5.0 is the first release that prints its notice.

## Settings

| Removed in | Setting | Current |
| --- | --- | --- |
| 0.2.0 | `BRIG_CREDENTIALS_CMD` | declare the credential under `secrets:`, then run `brig secret import <agent> <name> --from-command '<command>'` |
| 0.5.0 | `BRIG_TEMPLATE_DIR` | `BRIG_PROFILE_DIR` |

Brig refuses to start while `BRIG_TEMPLATE_DIR` is set and
`BRIG_PROFILE_DIR` is not, rather than read the built-in profiles in place of
the directory it named. Set `BRIG_PROFILE_DIR` to the same directory. Beside
`BRIG_PROFILE_DIR`, which always won, it is ignored. Brig refuses a run that
still sets `BRIG_CREDENTIALS_CMD`.

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
