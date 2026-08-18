# Keeping secrets in your keyring

`brig secret` is a store of brig's own, for when you have no secret manager to
point `brig run` at. It keeps named values in your login keychain, and gives
you five verbs to put them there and get them back.

If you already have 1Password, Vault, `pass` or anything like them, this is
probably not the page you want. A profile can name a variable to read from
brig's own environment instead, so putting a value in that environment is the
whole integration, and it is a shorter road than storing a second copy here:

```bash
CLAUDE_CODE_OAUTH_TOKEN=$(op read op://vault/claude/token) brig run claude
```

See [Credentials](../README.md#credentials) in the README for that path. The
store exists for the case where there is no such command to write.

What the keychain does and does not protect is
[security.md](security.md#the-secret-store). This page is about using it.

## Stored is not forwarded until a profile asks for it

Worth knowing before you build anything on top of it. Storing a value under
the name `gh-token` does not on its own make `GH_TOKEN` appear in the guest --
the store holds names, and a profile decides which of them the workload needs:

```console
$ brig secret ls
NAME      UPDATED
gh-token  2026-08-15 21:09
$ brig env claude
...
brig: forwarding to guest:
brig:   CLAUDE_CODE_OAUTH_TOKEN(host)
```

Two lines in the profile connect them. `secrets:` is the requirement list and
`env:` is the binding:

```yaml
secrets:
  - gh-token
env:
  - name: GH_TOKEN
    ref: secrets.gh-token
```

```console
$ brig env claude
...
brig: forwarding to guest:
brig:   GH_TOKEN(secret)
brig:   CLAUDE_CODE_OAUTH_TOKEN(host)
```

The value never passes through your shell, and it is never put on the
runtime's command line -- `BRIG_ENV_ARGV` cannot reach a value brig resolved
itself. A declared secret the store does not have fails the run before any
sandbox is created, naming every one that is missing. See
[profiles.md](profiles.md#secrets-and-env-for-a-credential-brig-resolves-itself)
for the grammar and the errors.

Composing it in the shell still works, and is the answer for a one-off:

```bash
GH_TOKEN=$(brig secret read gh-token) brig run claude
```

It is the weaker of the two, though. A value that came in from the ambient
environment is the one `BRIG_ENV_ARGV=1` can put in argv, and it is the one
the unresolved-`scheme://` guard has to second-guess. Prefer the profile.

## The five verbs

| verb | what it does |
| --- | --- |
| `brig secret create <name>` | store a new secret. Refuses if the name is taken |
| `brig secret read <name>` | print the value |
| `brig secret update <name>` | replace an existing value. Refuses if the name is not there |
| `brig secret delete <name>` | remove it, after asking. `-y` answers in advance |
| `brig secret ls` | names and dates, never values |

`delete` also answers to `rm`, and `ls` to `list`. Neither `create` nor
`update` takes the value as an argument -- see
[Getting a value in](#getting-a-value-in).

`create` and `update` are each other's mirror, and neither will do the other's
job: `create` refuses a name that is taken, `update` refuses one that is not
there. That is what turns a mistyped name into a message rather than a
silently lost secret, and each refusal names the command you evidently meant:

```console
$ printf %s "$TOKEN" | brig secret create gh-token
$ printf %s "$TOKEN" | brig secret create gh-token
brig: a secret named "gh-token" already exists. To replace it: brig secret update gh-token
$ printf %s "$TOKEN" | brig secret update gh-tokne
brig: no secret named "gh-tokne". To create it: brig secret create gh-tokne
```

A successful write prints nothing. Silence is the report.

`ls` prints two columns and no values:

```console
$ brig secret ls
NAME        UPDATED
deploy-key  2026-08-15 21:09
gh-token    2026-08-15 21:09
```

It reads keychain attributes only, never a value, which is why it raises no
access prompt however many secrets you have. `UPDATED` is the item's own
modification date. A backend that cannot supply one renders as `-` rather than
as an invented date -- the keychain always supplies one, so you will not see
that today, but a future backend may not.

An empty store is an ordinary state, not an error, and says how to leave it:

```console
$ brig secret ls
no secrets yet. To add one: brig secret create <name>
```

`delete` is the one verb that stops to ask, because it is the one whose
mistake cannot be undone: nothing in brig keeps a copy, and the keychain keeps
no history of its own. Every other destructive brig command acts on something
that can be made again -- a sandbox reboots, a profile is re-exported.

```console
$ brig secret delete gh-token
brig: delete "gh-token"? The value cannot be recovered [y/N] y
deleted gh-token
```

With no terminal to ask on it refuses rather than assuming yes, which would
make the scripted case the one that cannot be stopped:

```console
$ echo | brig secret delete gh-token
brig: deleting "gh-token" cannot be undone, and there is no terminal to ask on. Pass -y to answer in advance: brig secret delete gh-token -y
```

## Getting a value in

The value is never an argument. That is what keeps it out of `ps` and out of
your shell history, and it is why there is no `brig secret create gh-token
ghp_...` to type. It comes from stdin, or from a file:

| how | what gets stored |
| --- | --- |
| stdin, the default (`--stdin` if you like it spelled out) | the bytes, less one trailing line ending |
| `-f FILE` | the file's bytes, verbatim |
| `-f -` | stdin, spelled out. Same stripping as above |

**stdin strips exactly one trailing line ending, and `-f` does not.** The
asymmetry is the one thing on this page most worth remembering, so here is
each half of it.

`echo tok |` is the line people actually type, and `echo` adds a newline that
was never part of the secret:

```console
$ echo tok | brig secret create with-echo
$ brig secret read with-echo | xxd
00000000: 746f 6b                                  tok
```

Three bytes, not four. Storing the newline would be the worse default by some
distance: a trailing newline inside an `Authorization:` header fails in a way
that reads like a bad token rather than a bad store, and you would go looking
at GitHub's settings page rather than here. CRLF counts as one line ending for
the same reason -- a lone `\r` left behind fails identically and is harder to
spot in a bug report.

A file is taken as it is, because a file's bytes are what it holds and a PEM
key's final newline belongs to it:

```console
$ printf 'tok\n' > tok.txt
$ brig secret create from-file -f tok.txt
$ brig secret read from-file | xxd
00000000: 746f 6b0a                                tok.
```

Four bytes. So when you want exact bytes from stdin, say so with `printf %s`
rather than reaching for a flag -- there is no flag, because this covers it:

```console
$ printf %s 'tok' | brig secret create exact
$ brig secret read exact | xxd
00000000: 746f 6b                                  tok
```

The two invocations you will write most often:

```bash
printf %s "$TOKEN" | brig secret create gh-token
brig secret create deploy-key -f ~/.ssh/id_ed25519
```

## Getting a value out

`read` writes the value to stdout and adds nothing to it, so a pipe gets the
exact bytes:

```console
$ brig secret read from-file | xxd
00000000: 746f 6b0a                                tok.
```

A terminal is the exception, and gets a newline -- otherwise your prompt lands
against the tail of a token. It also gets a warning on stderr, because the
reason `create` will not take a value from a terminal does not stop applying on
the way out: the value is now in the scrollback of a window that outlives the
command.

```console
$ brig secret read gh-token
ghp_16C7e42F292c6912E7710c838347Ae178B4a
brig: gh-token is now in this terminal's scrollback. Pipe it instead to keep it out: brig secret read gh-token | ...
```

The warning is on stderr, so a pipe never sees it and stdout carries the value
and nothing else.

Command substitution is the case worth being careful with, and the care is the
shell's rather than brig's: `$(...)` strips *all* trailing newlines. For a
token that is exactly what you want, and it is why the composed run at the top
of this page is correct. For a value whose trailing newline matters -- a PEM
key stored with `-f` -- it is not:

```console
$ brig secret read from-file | wc -c
       4
$ printf %s "$(brig secret read from-file)" | wc -c
       3
```

Redirect rather than substitute when the bytes matter:

```bash
(umask 077; brig secret read deploy-key > ./deploy-key)
```

## Naming a secret

The grammar is narrow, and it is enforced on every verb, so it is worth
knowing rather than discovering:

- letters, digits, `-` and `_`
- it starts with a letter
- at most 128 characters

```console
$ printf %s v | brig secret create gh.token
brig: a secret name holds letters, digits, - and _, and "gh.token" holds "."
$ printf %s v | brig secret create 1password
brig: a secret name starts with a letter, and "1password" starts with "1"
```

The reason it is this narrow is that the name is three things at once. It is
the keychain account, where a space or a slash would make the item awkward to
address by hand. It is a word in brig's own error messages. And once profiles
can reference stored secrets ([#7](https://github.com/brig-sh/brig/issues/7)),
it is the tail of `ref: secrets.<name>` -- which is what rules out the `.`
specifically, since a dot would make that reference ambiguous. A leading digit
reads as a number rather than a name, and a leading dash reads as a flag
wherever the name is typed.

## The size limit

A value is capped at around 3KB, and it is deliberately not a round number.

brig hands `security` the write command on a single line, and `security` reads
one command into a 4096-byte buffer. The budget is the whole command rather
than the value, so the name competes with the value it names: the name appears
twice on that line, as the keychain account and in the item's label, and
base64 spends four characters for every three bytes of value.

| name | longest value `create` takes | `update` |
| --- | --- | --- |
| `a` (1 character) | 3012 bytes | 3009 bytes |
| `gh-token` (8 characters) | 3003 bytes | 3000 bytes |
| `deploy-key` (10 characters) | 3000 bytes | 2997 bytes |
| a 43-character name | 2949 bytes | 2946 bytes |

`update` is three bytes tighter throughout because its command carries `-U`.

Every API key and every SSH key you are likely to have fits: an ed25519
private key is 387 bytes. A 4096-bit RSA private key does not, and is refused
with both numbers named rather than stored short:

```console
$ brig secret create deploy-key -f rsa4096.pem
brig: the value for "deploy-key" is 3272 bytes, and the keychain takes at most 3000
```

The check is there because the failure it replaces was invisible. The first
implementation of this write path fed the value to `security`'s interactive
password prompt, which truncates at 128 characters -- 96 bytes of value, once
base64 has had its way. An `ANTHROPIC_API_KEY` is about 108 bytes. The cut
landed on a multiple of four, so the shortened base64 still decoded cleanly,
`create` reported success, and nothing failed anywhere until a guest tried to
authenticate with a key missing its last dozen bytes.

So brig also reads back what it just wrote and compares. `security` answers a
line it cannot fit by shortening it and reporting success, which makes that
failure silent by construction; one extra decrypt on a write a person typed is
a cheap way to make "stored" mean it. A `create` that stored the wrong thing
is removed, since a caller told the write failed will reasonably expect
nothing to be there.

## Linux

There is no backend yet. `brig secret` says so rather than falling back to a
file, which would be a downgrade nothing told you about:

```console
$ brig secret create gh-token
brig: no secret store on this platform: brig secret needs the macOS keychain so far
```

Note that it says so *before* asking for a value. The check happens ahead of
the read from stdin, so you are not sent off to find a token only to be told
afterwards that there is nowhere to put it. A Linux backend is
[#8](https://github.com/brig-sh/brig/issues/8).

## Errors you are likely to meet

Every one of these is a message rather than a silent outcome, which is the
point of the table: the error is the second entry point into these docs.

| what brig says | what happened |
| --- | --- |
| `a secret named "x" already exists. To replace it: brig secret update x` | `create` will not overwrite |
| `no secret named "x". To create it: brig secret create x` | `update` will not create |
| ``no secret named "x". `brig secret ls` lists them`` | `read` or `delete` on a name that is not there |
| `create x was given an empty value, and brig skips empty variables when it forwards them, so it would never reach a sandbox` | the source was empty. brig refuses because a forwarded empty variable is skipped, so the secret could never do anything but confuse whoever went looking |
| `no value on stdin. Pipe one in, or pass -f <file>: …` (and prints two examples) | `create` at a prompt with nothing piped in. It refuses rather than waiting, because typing the value there would put it in your scrollback |
| ``-f was given an empty path. Leave it out to read stdin, or pass `-f -` to say so`` | `-f "$KEYFILE"` with the variable unset. Falling through to stdin would store whatever the script had on it, under your name, and report success |
| `--stdin and -f name two different sources; pass one` | both given, and guessing which you meant would silently store the wrong one |
| `the value for "x" is N bytes, and the keychain takes at most M` | over [the size limit](#the-size-limit) |
| `deleting "x" cannot be undone, and there is no terminal to ask on. Pass -y to answer in advance: …` | a cron job or a unit file. `-y` is the answer given ahead |
| `a secret name holds letters, digits, - and _, ...` | see [Naming a secret](#naming-a-secret) |
| `no secret store on this platform: brig secret needs the macOS keychain so far` | Linux, [#8](https://github.com/brig-sh/brig/issues/8) |

## A worked example

Storing a GitHub token, using it, rotating it and removing it.

Store it. `printf %s` rather than `echo`, so no newline is stored, and the
value comes down a pipe rather than sitting in your history:

```console
$ printf %s 'ghp_16C7e42F292c6912E7710c838347Ae178B4a' | brig secret create gh-token
$ brig secret ls
NAME      UPDATED
gh-token  2026-08-15 21:09
```

Use it. Point a profile at it, once:

```yaml
secrets:
  - gh-token
env:
  - name: GH_TOKEN
    ref: secrets.gh-token
```

```console
$ brig env claude
...
brig: forwarding to guest:
brig:   GH_TOKEN(secret)
brig:   CLAUDE_CODE_OAUTH_TOKEN(host)
brig: never forwarded for claude-code: ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN (they would move this sandbox onto metered billing)
```

`brig env` reports; `brig run` does it, with nothing to remember at the
prompt:

```bash
brig run claude
```

Rotate it. `update` refuses to create, so a typo here cannot quietly leave you
with two secrets and the old one still in use:

```console
$ printf %s 'ghp_9a1FfE0d5B7c4A2e8D3b6C1a0F5e9D8c7B6a' | brig secret update gh-token
```

Nothing else has to change. brig re-reads what it forwards on every exec, so
the next command picks up the new value from the store and the sandbox does
not need restarting.

Remove it:

```console
$ brig secret delete gh-token
brig: delete "gh-token"? The value cannot be recovered [y/N] y
deleted gh-token
$ brig secret ls
no secrets yet. To add one: brig secret create <name>
```
