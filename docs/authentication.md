# Logging an agent in

This page covers the three ways an agent gets its credentials, and how to
give the guest git access to GitHub over HTTPS.

Prerequisites: Brig installed ([install.md](install.md)) and a shipped agent,
such as `claude-code`.

This page uses **agent** for the CLI surface you run: `brig run claude`, a
session. It uses **profile** for the file that declares credentials:
`brig secret import claude-code`. A built-in agent and its profile share one
name. A profile has no session, so `brig secret import` takes no `@label`.

## Three ways to give an agent a credential

Which one fits depends on whether the login already exists on your host, and
whether you want it to survive `brig stop`.

### 1. Log in inside the guest

This is the default, and it needs no setup:

```bash
brig run claude ~/code/demo
```

`claude-code` declares two secrets, `claude-credentials` and `gh-token`.
Both are optional, so neither must exist before the sandbox boots.
`brig info claude` prints the warnings a `run` prints, without booting
anything:

```console
$ brig info claude
brig: claude-code runs without 2 secrets
  ○ claude-credentials  → brig secret import claude-code
                          ↳ run `claude` on the host once to log in
  ○ gh-token            → brig secret create gh-token
                          ↳ export GH_TOKEN before running brig, or store one: gh auth token | brig secret create gh-token
...
```

`○` marks a secret with no value. `→` is the command that gives it one, and
`↳` is a note about the row above it. In a log or a pipe, every line starts
with `brig:`, so a line you copy still says where it came from.

The warnings do not stop the run. Only a **required** secret stops a run
before the sandbox exists, and none of the eight built-in profiles declares
one.

On a Linux host with no keyring there is no secret store. Brig prints no
warning for an optional secret there, so the block above does not appear. A
required secret still stops the run, and the error names the store it could
not read.

Claude Code prompts for the login the sandbox does not have. Complete it
inside the guest, as you would on a new machine.

Where that login is stored depends on the profile. `claude-code` and
`claude-desktop` write it to a memory-backed mount. `brig stop claude` discards
that mount, so the next `brig run claude` prompts again. The other five agent
profiles keep the whole guest home on host disk: `codex`, `cursor`, `gemini`,
`grok`, `opencode`. A login written there survives a stop.

Choose this path for a first run with no setup.

### 2. Carry your host login in, once

Use this when you have already logged in to the agent's own app or CLI on
this host, and you want that login to survive a stop:

```bash
brig secret import claude-code
```

`claude-code` and `claude-desktop` fill `claude-credentials` from the first
of two places on your host: the macOS keychain's `Claude Code-credentials`
generic-password item, then the file `~/.claude/.credentials.json`. Neither
declares a source for `gh-token`. Store that one by hand
(`brig secret create gh-token`) or export `GH_TOKEN` on each run
([Git access in the guest](#git-access-in-the-guest)).

The other six shipped profiles declare no `secrets:` at all: `codex`,
`cursor`, `gemini`, `grok`, `opencode`, `ubuntu`. For those, the command prints
`<profile> declares no secrets, so there is nothing to import` and exits 0,
on any host, without opening the secret store.

**If a long `claude-code` session stops authenticating.** Brig re-delivers the
stored `claude-credentials` document on every command that reaches the
sandbox: every `run`, `sh` and `exec`.

Measured 2026-08-19 against a live account: Claude Code refreshes that
document in place, and Anthropic rotates the refresh token single-use. A
refresh inside the guest invalidates the host copy, so the next Brig command
re-delivers the dead stored document over the guest's fresh one. This can
change with either the agent or the provider. If a session starts failing to
authenticate, log in on the host again and re-run
`brig secret import claude-code`.

Choose this path when you already have a working login on this host and want
the sandbox to start authenticated.

### 3. Live environment bindings

Four profiles forward an API key from your own environment, read on every
run:

| profile | variable |
| --- | --- |
| `cursor` | `CURSOR_API_KEY` |
| `gemini` | `GEMINI_API_KEY` |
| `grok` | `XAI_API_KEY` |
| `opencode` | `OPENROUTER_API_KEY` |

Export it before you run:

```bash
export GEMINI_API_KEY=<key>
brig run gemini ~/code/demo
```

`claude-code` and `claude-desktop` do not forward `ANTHROPIC_API_KEY` or
`ANTHROPIC_AUTH_TOKEN` from your environment: forwarding either one moves the
sandbox off your subscription and onto metered billing. `codex` denies
`OPENAI_API_KEY` for the same reason. It signs in with
`codex login --device-auth` inside the guest, which is path 1 above, and it
declares no `secrets:`, so path 2 has nothing to fill.
[profiles.md](profiles.md#deny-is-the-billing-guard) names the override.

Choose this path when a key already lives in your shell, a CI job, or a
secret manager's run-with-env wrapper. Brig reads it fresh on every run
instead of storing a copy.

## Git access in the guest

A token in `GH_TOKEN` is for git over HTTPS inside the guest. It does not
log the agent in to its provider.

Guest git over HTTPS is off by default. `brig info` reports the setting:

```console
$ brig info claude
...
brig: guest git over HTTPS: off (BRIG_GIT_CONFIG=1 to enable)
```

Turn it on with `BRIG_GIT_CONFIG=1`:

```bash
BRIG_GIT_CONFIG=1 brig run claude ~/code/demo
```

With it on, Brig regenerates a credential helper and a managed gitconfig
inside the guest on every run. The helper reads `GH_TOKEN` at the moment git
asks for a credential. It does nothing when the variable is unset, so git
reports its own authentication error instead of failing on an empty
password.

`GH_TOKEN` reaches the guest differently by profile. On `claude-code` and
`claude-desktop` it is a chain: your exported `GH_TOKEN` wins if you set one,
and a stored `gh-token` secret is the fallback. On the other six profiles it
comes only from your exported `GH_TOKEN`. A stored `gh-token` secret does not
reach them.

[secrets.md](secrets.md) is the reference for the store behind path 2 and the
`gh-token` fallback. See [profiles.md](profiles.md) for how to change any of
this for an agent of your own.
