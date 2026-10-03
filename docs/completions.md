# Shell completion

`brig completion bash|zsh|fish` prints a completion script to stdout. Brig
installs nothing: where a script belongs differs per shell and per host, and
Brig does not write to your startup files.

A Homebrew cask install already has completion: the cask installs all
three scripts where each shell reads them from. The lines below are for
anyone who installed the binary another way.

## Installing it

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

The script asks the `brig` on your `PATH` what to offer, so it does not go
stale when Brig gains a verb or a flag. Reinstall it only when the script
itself changes.

## What completes where

A `brig` line has three positions, and completion follows them.

| where the cursor is | what is offered |
| --- | --- |
| before the verb | the verbs, and the three global flags (`--verbose`, `-q`/`--quiet`, `--json`) |
| a flag, either side of the ref | the run-line flags that verb accepts (Brig reads those on both sides) |
| a ref | every agent, and every session under one: `claude`, `claude@refactor` |
| `brig run <ref> …` or `brig plan <ref> …` | the project directory, for the first word only |
| once the agent's arguments have begun | nothing |

`--verbose` and `-q`/`--quiet` complete only before the verb, and `-q` also
after `ls`, which has a `-q` of its own. `-q` after any other verb still
works, but that position is retiring, so completion does not offer it.
`--json` completes before the verb, and after every verb that accepts it
except `ls`, `agent ls` and `secret ls`.

Completion offers a flag wherever Brig reads it, whether or not the verb
uses the value. Brig reads `brig run claude --mem 4096`, so `--mem`
completes after the ref as well as before it, on every verb that reads
run-line flags. Past the first word or flag Brig does not own, completion
offers nothing, because the rest of the line belongs to the agent.

`stop` and `rm` act on a sandbox that exists, so they offer only the
sessions that exist. `run`, `sh`, `info` and `plan` accept an agent that has
never run, so they offer every agent. `brig rm --all` names no session, so a
line carrying it offers no ref. It offers only the flags that go with it,
`--dry-run` and `-y`/`--yes`.

Under the noun commands (`agent`, `policy`, `secret`, `telemetry`), the
subcommands complete, and so do the names they take. Agents complete for
`agent show`, policies for `policy attach`, and the agents with a file of
their own complete for `agent edit` and `agent rm`. `--network` completes
its three postures. `--home` completes directories.

Two things are never offered:

- **Secret names.** Listing them opens your keyring: on macOS that means
  `security dump-keychain`, and a Linux secret-service backend can raise
  an unlock prompt. Completion must not trigger either. `brig secret ls`
  lists them.
- **Retired spellings.** Each one still works and prints a notice naming
  its replacement. Completion offers only the current spelling. See
  [migration.md](migration.md) for the full old-to-new list.

## Session names can be stale

The labels come from Brig's session index, which is a file. Completion
does not ask the runtime, because it has to answer in milliseconds and has
to work on a host with no runtime installed. A sandbox removed outside Brig
(with `nerdctl rm`, for example) stays on offer until the next `brig ls`
prunes it or `brig rm <ref>` forgets it. A command given the stale name
reports that the sandbox is gone.
