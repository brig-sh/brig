# Logging an agent in

An agent can get its credentials in three ways. You can also give the guest
git access to GitHub over HTTPS.

Prerequisites: Brig installed ([Install](install.md)) and a shipped agent,
such as `claude-code`.

An **agent** is the CLI surface that you run, in a session:
`brig run claude`. A **profile** is the file that declares credentials:
`brig secret import claude-code`. A built-in agent and its profile share one
name. A profile has no session, so `brig secret import` takes no `@label`.

## Three login paths

| path | use it when |
| --- | --- |
| [1. Log in inside the guest](#1-log-in-inside-the-guest) | you want a first run with no setup |
| [2. Carry your host login in, once](#2-carry-your-host-login-in-once) | you have a login to the app or CLI of the agent on this host, and you want the sandbox to start authenticated |
| [3. Live environment bindings](#3-live-environment-bindings) | a key is in your shell, a CI job, or the run-with-env wrapper of a secret manager |

### 1. Log in inside the guest

```bash
brig run claude ~/code/demo
```

This path is the default, and it needs no setup. Claude Code prompts for the
login that the sandbox does not have. Complete the login inside the guest, as
you do on a new machine.

The profile decides where the login is stored:

| profiles | where the login is stored | after a stop |
| --- | --- | --- |
| `claude-code`, `claude-desktop` | a memory-backed mount | `brig stop claude` discards the mount, so the next `brig run claude` prompts again |
| `codex`, `cursor`, `gemini`, `grok`, `opencode` | the guest home, which these profiles keep on host disk | the login survives |

For `claude-code`, the login file in the guest is
`/brig/claude/.credentials.json`, and `CLAUDE_CONFIG_DIR` points the agent at
that directory. The memory-backed mount keeps the credential off host disk.

`claude-code` declares two secrets, `claude-credentials` and `gh-token`. Both
are optional, so the sandbox boots without them. `brig info claude` prints
the warnings that a `run` prints, and it boots nothing:

```console
$ brig info claude
brig: claude-code runs without 2 secrets
  ○ claude-credentials  → brig secret import claude-code
                          ↳ run `claude` on the host once to log in
  ○ gh-token            → brig secret create gh-token
                          ↳ export GH_TOKEN before running brig, or store one: gh auth token | brig secret create gh-token
...
```

| mark | meaning |
| --- | --- |
| `○` | a secret with no value |
| `→` | the command that gives the secret a value |
| `↳` | a note about the row before it |

In a log or a pipe, every line starts with `brig:`.

The warnings do not stop the run. Only a **required** secret stops a run
before the sandbox exists. None of the eight built-in profiles declares a
required secret.

A Linux host with no keyring has no secret store. On that host, Brig prints
no warning for an optional secret. A required secret still stops the run, and
the error names the store that Brig cannot read.

### 2. Carry your host login in, once

```bash
brig secret import claude-code
```

Use this path after you log in to the app or CLI of the agent on this host.
The imported login survives a stop.

| profiles | what `import` does |
| --- | --- |
| `claude-code`, `claude-desktop` | fills `claude-credentials` from the first of two places on your host: the `Claude Code-credentials` generic-password item in the macOS keychain, then the file `~/.claude/.credentials.json` |
| `codex`, `cursor`, `gemini`, `grok`, `opencode`, `ubuntu` | these profiles declare no `secrets:`. The command prints `<profile> declares no secrets, so there is nothing to import` and exits 0, on any host. It does not open the secret store |

`claude-code` and `claude-desktop` declare no source for `gh-token`. Store
that secret by hand (`brig secret create gh-token`) or export `GH_TOKEN` on
each run. See [Git access in the guest](#git-access-in-the-guest).

[Import from your host](secrets.md#import-from-your-host) gives the flags and
the rules of `import`.

> [!WARNING]
> If a long `claude-code` session stops authenticating, log in on the host
> again. Then run `brig secret import claude-code` again.

Brig delivers the stored `claude-credentials` document again on every command
that reaches the sandbox: every `run`, `sh` and `exec`. Claude Code refreshes
that document in place, and Anthropic rotates the refresh token single-use. A
refresh inside the guest invalidates the host copy. The next Brig command
then delivers the dead stored document over the fresh document in the guest.
This behavior can change with the agent or with the provider.

### 3. Live environment bindings

```bash
export GEMINI_API_KEY=<key>
brig run gemini ~/code/demo
```

Four profiles forward an API key from your environment. Brig reads the key on
every run and stores no copy. Export the key before you run.

| profile | variable |
| --- | --- |
| `cursor` | `CURSOR_API_KEY` |
| `gemini` | `GEMINI_API_KEY` |
| `grok` | `XAI_API_KEY` |
| `opencode` | `OPENROUTER_API_KEY` |

Three profiles deny a provider key:

| profile | not forwarded from your environment |
| --- | --- |
| `claude-code`, `claude-desktop` | `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` |
| `codex` | `OPENAI_API_KEY` |

A forwarded key moves the sandbox off your subscription and onto metered
billing. [Deny is the billing guard](profiles.md#deny-is-the-billing-guard)
names the override.

`codex` signs in with `codex login --device-auth` inside the guest, which is
path 1. It declares no `secrets:`, so path 2 has nothing to fill.

## Git access in the guest

```bash
BRIG_GIT_CONFIG=1 brig run claude ~/code/demo
```

`BRIG_GIT_CONFIG=1` turns on git over HTTPS in the guest. It is off by
default. `brig info` reports the setting:

```console
$ brig info claude
...
brig: guest git over HTTPS: off (BRIG_GIT_CONFIG=1 to enable)
```

A token in `GH_TOKEN` is for git over HTTPS inside the guest. It does not log
the agent in to its provider.

With the setting on, Brig regenerates a credential helper and a managed
gitconfig inside the guest on every run. The helper reads `GH_TOKEN` at the
moment git asks for a credential. If the variable is unset, the helper does
nothing. Git then reports its own authentication error, and does not fail on
an empty password.

The profile decides how `GH_TOKEN` reaches the guest:

| profiles | source of `GH_TOKEN` |
| --- | --- |
| `claude-code`, `claude-desktop` | a chain: your exported `GH_TOKEN` wins if you set one, and a stored `gh-token` secret is the fallback |
| the other six profiles | your exported `GH_TOKEN` only. A stored `gh-token` secret does not reach them |

[Keeping secrets in your keyring](secrets.md) is the reference for the store
behind path 2 and the `gh-token` fallback. See [Profiles](profiles.md) to
change any of this for an agent of your own.
