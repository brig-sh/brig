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
| before the verb | the verbs, and the three global flags (`--verbose`, `-q`/`--quiet`, `--json`) |
| a flag, either side of the ref | the run-line flags that verb accepts -- brig reads those on both sides |
| a ref | every agent, and every session under one: `claude`, `claude@refactor` |
| `brig run <ref> …` | the project directory, for the first word only |
| once the agent's arguments have begun | nothing |

The global flags complete only before the verb. `--verbose` has no run-line
row at all. `-q`/`--quiet` has one, but it is the retiring spelling, and
completion never offers a spelling on its way out, so neither is offered
beside the ref.

Completion computes what a flag is legal for by its position on the line, not
by whether the verb does anything with it once parsed. `brig run claude --mem
4096` is a line brig reads, so `--mem` is offered after the ref as readily as
before it, on every verb that reads run-line flags at all. Completion does not
know, and does not claim, that a given verb acts on the value. The agent's
arguments begin at the first word or flag brig does not own (`--`, an
unrecognised flag, or a bare word past the project), and from there completion
offers nothing, because the vocabulary is another program's and brig has no
list of it.

`stop` and `rm` act on a sandbox that exists, so they offer the sessions there
are rather than every agent there is. `run`, `sh` and `info` take an
agent that has never run, so they offer all of them. `brig rm --all` names no
session, so a line carrying it offers no ref. It offers only the two flags
that go with it, `--dry-run` and `--yes`.

Under the noun commands -- `agent`, `policy`, `secret`, `telemetry` -- the
subcommands complete, and so do the names they take: agents for `agent show`,
policies for `policy attach`, the agents with a file of their own for `agent
edit` and `agent rm`. `--network` completes its three postures. `--home`
completes directories.

Two things are deliberately not offered:

- **Secret names.** Listing them opens your keyring -- on macOS that means
  `security dump-keychain`, and a Linux secret-service backend can raise an
  unlock prompt. A keystroke must not do either. `brig secret ls` lists them.
- **Every retired spelling.** Each one still works and prints one line saying
  what replaced it. Completion teaches only the spelling that stays. See
  [migration.md](migration.md) for the full old-to-new list.

## Session names can be stale

The labels come from brig's session index, which is a file rather than a
question put to the runtime: completion has to answer in milliseconds, and on
a host with no runtime installed it has to answer at all. A sandbox removed
outside brig -- with `nerdctl rm`, say -- stays on offer until the next
`brig ls` prunes it. The command that follows says it is gone.
