# Shell completion

`brig completion bash|zsh|fish` prints a completion script to stdout. Nothing
is installed for you: where a script belongs differs per shell and per host,
and brig does not write to your startup files.

A Homebrew cask install already has completion -- the cask installs all three
scripts where each shell reads them from. The lines below are for anyone who
installed the binary another way.

## Installing it

```bash
# bash
brig completion bash > /usr/local/etc/bash_completion.d/brig
# or, without a completion directory:
echo 'eval "$(brig completion bash)"' >> ~/.bashrc
```

```zsh
# zsh -- the file must be called _brig, and it must be on your fpath
brig completion zsh > "${fpath[1]}/_brig"
# then start a new shell, or:
compinit
```

```fish
brig completion fish > ~/.config/fish/completions/brig.fish
```

The script asks the `brig` on your `PATH` what to offer, so it does not go
stale when brig gains a verb or a flag. Reinstalling the script is only
necessary if the script itself changes.

## What completes where

A brig line has three positions, and completion follows them.

| where the cursor is | what is offered |
| --- | --- |
| before the verb | the verbs, and the global flags (`--verbose`, `-q`) |
| after the verb, before the ref | the flags that verb honours, and then the ref |
| a ref | every agent, and every session under one: `claude`, `claude@refactor` |
| `brig run <ref> …` | the project directory, for the first word only |
| after that, on `run` and `sh` | nothing -- the arguments there are the agent's |

`stop` and `rm` act on a sandbox that exists, so they offer the sessions there
are rather than every agent there could be. `run`, `sh` and `info` take an
agent that has never run, so they offer all of them.

Under the noun commands -- `agent`, `policy`, `secret`, `telemetry` -- the
subcommands complete, and so do the names they take: agents for `agent show`,
policies for `policy attach`, the agents with a file of their own for `agent
edit` and `agent rm`. `--network` completes its three postures. `--home`
completes directories.

Two things are deliberately not offered:

- **Secret names.** Listing them opens your keyring -- on macOS that means
  `security dump-keychain`, and a Linux secret-service backend can raise an
  unlock prompt. A keystroke should do neither. `brig secret ls` lists them.
- **The retired spellings.** `brig exec`, `brig env`, `brig create`,
  `brig reset`, `--name`, `--workspace` and the rest still work and say what
  replaced them. Completion teaches the spelling that stays.

## Session names can be stale

The labels come from brig's session index, which is a file rather than a
question put to the runtime: completion has to answer in milliseconds, and on
a host with no runtime installed it has to answer at all. A sandbox removed
outside brig -- with `nerdctl rm`, say -- stays on offer until the next
`brig ls` prunes it. The command that follows says it is gone.
