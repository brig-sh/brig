# Logging an agent in

An agent in a sandbox starts with no login. This page covers three separate
needs:

- The agent's login to its provider, such as Anthropic or Google.
- Git access to GitHub over HTTPS. See
  [Git access in the guest](#git-access-in-the-guest).
- Access to other services, such as a cloud API. See
  [A secret behind a route](secrets.md#a-secret-behind-a-route).

## Choose a path

| Your goal | Path | Profiles |
| --- | --- | --- |
| A first run with no setup | [1. Log in inside the guest](#1-log-in-inside-the-guest) | all |
| Start logged in with the login you already have on this host | [2. Carry your host login in, once](#2-carry-your-host-login-in-once) | `claude-code`, `claude-desktop`, `herdr` |
| Use an API key from your shell, a CI job or a secret manager | [3. Live environment bindings](#3-live-environment-bindings) | `cursor`, `gemini`, `grok`, `opencode` |
| Keep the login on the host, out of the guest | [4. Keep the login on the host](#4-keep-the-login-on-the-host) | `claude-code`, or any profile with a route |

`brig info <ref>` shows which credentials a run gets, by name, and boots
nothing. A secret is a value in Brig's own secret store: the macOS keychain,
or a Secret Service keyring on Linux. See [Secrets](secrets.md).

A built-in agent and its profile share one name. Commands that take a
profile, such as `brig secret import claude-code`, take no `@label`.

### 1. Log in inside the guest

```bash
brig run claude ~/code/demo
```

This path is the default. Claude Code asks for a login, and you complete it
inside the guest, as on a new machine.

The profile decides where the login is stored:

| Profiles | Where the login is stored | After `brig stop` |
| --- | --- | --- |
| `claude-code`, `herdr` | a memory-backed mount | lost. The next `brig run` asks again |
| `claude-desktop`, `codex`, `cursor`, `gemini`, `grok`, `opencode` | the guest home, on host disk | kept, until `brig rm` deletes a guest home Brig created |

For `claude-code`, the login never reaches host disk.

`codex` signs in with `codex login --device-auth` inside the guest.

`claude-code` declares two optional secrets, `claude-credentials` and
`gh-token`. A run without them prints a warning and continues:

```console
$ brig info claude
brig: claude-code runs without 2 secrets
  ○ claude-credentials  → brig secret import claude-code
                          ↳ run `claude` on the host once to log in
  ○ gh-token            → brig secret create gh-token
                          ↳ export GH_TOKEN before running brig, or store one: gh auth token | brig secret create gh-token
...
```

`○` marks a secret with no value, `→` the command that gives it one, and `↳`
a note. In a log or a pipe, every line starts with `brig:`.

Only a required secret stops a run before the sandbox exists. No built-in
profile declares one. On a Linux host with no keyring, Brig prints no
warning for an optional secret.

### 2. Carry your host login in, once

Log in to Claude Code on this host first. Then copy that login into Brig's
secret store:

```bash
brig secret import claude-code
```

`import` copies the Claude Code login from the macOS keychain, or from
`~/.claude/.credentials.json`, and stores it as
`claude-credentials`. `claude-desktop` and `herdr` use the same login. Brig
then gives the login to the guest on every `run` and `sh`, so the sandbox
starts logged in, also after a stop.

To check the result, run `brig info claude`. The `guest login:` line names
the stored secret and where it was imported from.

The other profiles declare no secrets. For them, `import` prints
`<profile> declares no secrets, so there is nothing to import` and exits 0.

[Import from your host](secrets.md#import-from-your-host) lists the flags
of `import`.

> [!WARNING]
> If a long `claude-code` session stops authenticating, log in on the host
> again, then run `brig secret import claude-code` again.
>
> Claude Code refreshes its login inside the guest, and Anthropic accepts
> each refresh token once. A refresh in the guest makes the stored copy
> invalid, and the next Brig command writes that invalid copy over the fresh
> one in the guest. This behavior can change with the agent or the provider.

### 3. Live environment bindings

```bash
export GEMINI_API_KEY=<key>
brig run gemini ~/code/demo
```

Brig reads the key from your environment on every run and stores no copy.

| Profile | Variable it forwards |
| --- | --- |
| `cursor` | `CURSOR_API_KEY` |
| `gemini` | `GEMINI_API_KEY` |
| `grok` | `XAI_API_KEY` |
| `opencode` | `OPENROUTER_API_KEY` |

Some profiles refuse to forward a provider key, because the key would move
the agent from your subscription to metered API billing:

| Profile | Never forwarded |
| --- | --- |
| `claude-code`, `claude-desktop`, `herdr` | `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` |
| `codex` | `OPENAI_API_KEY` |

To forward one anyway, change the profile's `deny` list. See
[`deny` is the billing guard](profiles.md#deny-is-the-billing-guard).

### 4. Keep the login on the host

A route of the credential broker keeps the credential on the host. The guest
gets a session token that works only through the broker, and `brig stop` or
`brig rm` revokes it. For Claude Code, import the login as in path 2, then
bind a policy with a `claude-code` route.
[The credential broker](broker.md#use-it) has the complete example.

Routes work on Linux, and on macOS with the `hvi` backend. A route on the `aws` preset is the exception to "the credential stays on the
host": it gives the guest temporary AWS keys. See
[Routes that vend](broker.md#routes-that-vend).

## Git access in the guest

```bash
BRIG_GIT_CONFIG=1 brig run claude ~/code/demo
```

`BRIG_GIT_CONFIG=1` turns on git over HTTPS in the guest. It is off by
default. A token in `GH_TOKEN` is for git only. It does not log the agent in
to its provider.

With the setting on, git in the guest uses `GH_TOKEN` for HTTPS. If no token
reaches the guest, git reports its own authentication error.

The profile decides where `GH_TOKEN` comes from:

| Profiles | Source of `GH_TOKEN` |
| --- | --- |
| `claude-code`, `claude-desktop`, `herdr` | your exported `GH_TOKEN` if you set one, then a stored `gh-token` secret |
| the other six | your exported `GH_TOKEN` only |

To store the token once:

```bash
gh auth token | brig secret create gh-token
```

To check the setting, run `brig info claude`. It prints
`guest git over HTTPS: on` or `off`, and its `CREDENTIALS` row names
`GH_TOKEN` when a value reaches the guest.

The agent can use the token for as long as the sandbox holds it, so give it
a token limited to the repositories the agent needs.

[Profiles](profiles.md) shows how to change any of this for an agent of
your own.
