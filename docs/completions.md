# Shell completion

`brig completion bash|zsh|fish` prints a completion script to stdout.

A Homebrew cask install already has completion. The cask installs all three
scripts where each shell reads them. With any other install, add the script
yourself. `brig completion` installs nothing and does not write to your
startup files, because the location of a script differs per shell and per
host.

## Install the script

```bash
# bash
brig completion bash > /usr/local/etc/bash_completion.d/brig
# or, without a completion directory:
echo 'eval "$(brig completion bash)"' >> ~/.bashrc
```

```zsh
# zsh: the file must be called _brig, and it must be on your fpath
brig completion zsh > "${fpath[1]}/_brig"
# then start a new shell, or:
compinit
```

```fish
brig completion fish > ~/.config/fish/completions/brig.fish
```

The script asks the `brig` on your `PATH` what to offer, so it stays current
when Brig gains a verb or a flag. Reinstall it only when the script itself
changes.

## What completes where

A `brig` line has three positions, and completion follows them.

| where the cursor is | what is offered |
| --- | --- |
| before the verb | the verbs, and the three global flags (`--verbose`, `-q`/`--quiet`, `--json`) |
| a flag, either side of the ref | the run-line flags that verb accepts (Brig reads those on both sides) |
| a ref | every agent, and every session under one: `claude`, `claude@refactor` |
| `brig run <ref> …` or `brig plan <ref> …` | the project directory, for the first word only |
| once the agent's arguments have begun | nothing |

### Global flags

| Flag | Where it completes |
| --- | --- |
| `--verbose` | Before the verb |
| `-q`/`--quiet` | Before the verb. `-q` also completes after `ls`, which has its own `-q`. |
| `--json` | Before the verb, and after every verb that accepts it except `ls`, `agent ls` and `secret ls` |

`-q` after any other verb still works. That position is retiring, so
completion does not offer it.

### Run-line flags

Completion offers a flag wherever Brig reads it, whether or not the verb
uses the value. For example, Brig reads `brig run claude --mem 4096`. As a
result, `--mem` completes after the ref and before it, on every verb that
reads run-line flags.

After the first word or flag that Brig does not own, completion offers
nothing. The rest of the line belongs to the agent.

### Refs

| Verb | Refs that completion offers | Reason |
| --- | --- | --- |
| `run`, `sh`, `info`, `plan` | Every agent | These verbs accept an agent that has never run |
| `stop`, `rm` | Only the sessions that exist | These verbs act on a sandbox that exists |
| `brig rm --all` | None. Completion offers only the flags that go with it, `--dry-run` and `-y`/`--yes`. | The line names no session |

### Noun commands

Under the noun commands (`agent`, `policy`, `secret`, `telemetry`), the
subcommands complete, and so do the names they take.

| After | Completion offers |
| --- | --- |
| `agent show` | Agents |
| `policy attach` | Policies |
| `agent edit`, `agent rm` | The agents that have their own file |
| `--network` | Its three postures |
| `--home` | Directories |

### Never offered

- **Secret names.** `brig secret ls` lists them. Completion does not, because
  a list of secret names opens your keyring. On macOS, that means
  `security dump-keychain`. On Linux, a secret-service backend can raise an
  unlock prompt.
- **Retired spellings.** Completion offers only the current spelling. Each
  retired spelling still works and prints a notice that names its
  replacement. See [migration.md](migration.md) for the full old-to-new list.

## Session names can be stale

The labels come from the session index of Brig, which is a file. Completion
does not ask the runtime, because it must answer in milliseconds and must
work on a host with no runtime installed.

A sandbox removed outside Brig (with `nerdctl rm`, for example) stays on
offer until the next `brig ls` prunes it or `brig rm <ref>` forgets it. A
command given the stale name reports that the sandbox is gone.
