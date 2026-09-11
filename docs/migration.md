# Moving off the retired spellings

brig renamed most of its commands while it was still a prerelease. Every old
spelling on this page still works today. Each one prints one line on stderr
naming its replacement, in this form:

```
brig: `brig profiles` is now `brig agent ls`
```

The old spellings are scheduled for removal in 0.3. `brig run` is never
removed. If you have a script written against an older spelling, this page is
the whole list of what to change.

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
| `brig shell <ref>` | `brig sh <ref>` |
| `brig env <ref>` | `brig info <ref>` |
| `brig reset` | `brig rm --all` |

`brig rm --all` asks before it removes anything. A script has no terminal to
answer on, so a script that ran `brig reset` unattended needs
`brig rm --all -y`. The same applies to `brig reset` itself: without a
terminal it also refuses unless `-y` is passed.

There is deliberately no `brig template edit`. The retired group kept only the
verbs it already had, so asking for that one is an error rather than a
deprecation notice.

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
brig: brig <verb> <ref> -q is now brig -q <verb> <ref>
```

`--json` is different. It is accepted on both sides of the verb permanently,
and prints no notice either way.

## Session names

`--name` is the flag this replaced most visibly. A session is now part of the
ref:

```bash
brig run claude@refactor          # current
brig run claude --name refactor   # still works, prints a notice
```

`brig run claude` is the default session of the `claude-code` agent, and
`claude@refactor` is a second one with its own sandbox and its own guest home.
[docs/sessions.md](sessions.md) explains what each session keeps separate.

`--name` is also Claude Code's own flag. Anything you type to the right of the
ref goes to the agent untouched, so `brig run claude -- --name x` sends
`--name x` to Claude Code and brig never sees it.

## Profile keys

These keys still parse in a profile file for one more release. `brig agent
edit` on an old file is the quickest way to see the current spelling, because
the header comment documents every field.

| Retired key | Current |
| --- | --- |
| `hostCredential:` | declare a secret and run `brig secret import <agent>` |
| `shell:`, `gui:` booleans | `kind:` |
| `forward:` | `env:`, with `ref: env.<name>` |
| `statePaths:` | `volumes:` |

Declaring a retired key and its replacement with values that disagree is an
error, not a warning. brig refuses the profile rather than guessing which one
you meant.

`hostCredential:` warns only on a profile backed by a file of your own. No
built-in profile warns about itself.

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
