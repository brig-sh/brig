# Logging an agent in

By the end of this page you can pick how an agent gets its credentials. It
can then reach GitHub over HTTPS from inside the guest.

Prerequisites: Brig installed ([install.md](install.md)) and a shipped agent,
such as `claude-code`. Nothing else.

This page uses **agent** for the CLI surface you run: `brig run claude`, a
session. It uses **profile** for the file that declares credentials:
`brig secret import claude-code`. A built-in agent and its profile share one
name, but they are not the same thing. A profile has no session, and
`brig secret import` takes no `@label`.

## Three ways to give an agent a credential

Pick based on whether the login already exists on your host, and whether you
want it to survive `brig stop`.

### 1. Log in inside the guest

The default. It needs no setup:

```bash
brig run claude ~/code/demo
```

`claude-code` declares two secrets, `claude-credentials` and `gh-token`.
Both are optional, so neither must exist before the sandbox boots.
`brig info claude` prints the same warnings a `run` prints, without booting
anything:

```console
$ brig info claude
brig: no value for the secret "gh-token", and claude-code will run without it.
brig: To supply one: brig secret create gh-token
brig: no value for the secret "claude-credentials", and claude-code will run without it.
brig: To carry it in from your host: brig secret import claude-code
brig: run `claude` on the host once to log in
```

That run exits 0. Only a **required** secret stops a run before the sandbox
exists. None of the eight built-in profiles declares one, so a missing
secret never stops a run.

Claude Code prompts for the login the sandbox does not have. Complete it
inside the guest, the same way a fresh machine's first login works.

Where that login lands next depends on the profile. `claude-code` and
`claude-desktop` write it to a memory-backed mount. `brig stop claude` takes
the whole sandbox with it, and the next `brig run claude` prompts again. The
other five agent profiles mount the guest home from host disk instead: `codex`,
`cursor`, `gemini`, `grok`, `opencode`. A login written there survives a stop.

Choose this path for a first run with no setup. On `claude-code` or
`claude-desktop`, you log in again after every `brig stop`. On the other five
profiles, the login persists on its own.

### 2. Carry your host login in, once

You already logged in to the agent's own app or CLI on this Mac. You want
that login to survive a stop, without repeating it inside the guest:

```bash
brig secret import claude-code
```

`claude-code` and `claude-desktop` fill `claude-credentials` from the first
of two places on your host: the macOS keychain's `Claude Code-credentials`
generic-password item, then the file `~/.claude/.credentials.json`. Neither
declares a source for `gh-token`. That one takes a value you supply by hand
(`brig secret create gh-token`) or export live, path 3 below.

The other six shipped profiles declare no `secrets:` at all: `codex`,
`cursor`, `gemini`, `grok`, `opencode`, `ubuntu`. There is nothing on your
host for `import` to read. On a host with a working secret store, the
command still succeeds: it prints `<profile>: importing 0 secrets` and exits
0. On a host with no keyring it fails instead, even though there is nothing
to import.

**If a long `claude-code` session stops authenticating.** Brig re-delivers the
stored `claude-credentials` document on every command that reaches the
sandbox. `run`, `sh` and `exec` all count, not only the first boot.

Measured 2026-08-19 against a live account: Claude Code refreshes that
document in place, and Anthropic rotates the refresh token single-use. A
refresh inside the guest invalidates the host copy, so the next Brig command
re-delivers the dead stored document over the guest's fresh one. This can
change with either the agent or the provider. If a session starts failing to
authenticate, log in on the host again and re-run
`brig secret import claude-code`.

Choose this path when you already have a working login on this Mac, and want
the sandbox to start already authenticated.

### 3. Live environment bindings

Some agents read a credential straight from your own environment, live, on
every run:

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

`claude-code` and `claude-desktop` refuse `ANTHROPIC_API_KEY` and
`ANTHROPIC_AUTH_TOKEN` from your environment on purpose: forwarding either
one moves the sandbox off your subscription and onto metered billing.
`codex` denies `OPENAI_API_KEY` for the same reason. It signs in with
`codex login --device-auth` inside the guest instead, which is path 1 above.
`codex` also declares no `secrets:`, so path 2 cannot fill one either.
[secrets.md](secrets.md) has the exact message Brig prints and the override.

Choose this path when a key already lives in your shell, a CI job, or a
secret manager's run-with-env wrapper. Brig reads it fresh on every run
instead of storing a copy.

## Git access in the guest

A token in `GH_TOKEN` is for git over HTTPS inside the guest, not for the
agent's own authentication to its provider.

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
