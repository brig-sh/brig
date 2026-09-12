# Keeping secrets in your keyring

`brig secret` is Brig's own store, and it is the **only** store a run reads, on
every profile Brig ships. A profile of your own still carrying the deprecated
`hostCredential:` field is the exception: it reads the host item it names on
every run. A profile declares the names it wants under `secrets:`. What is
in this store under those names reaches the sandbox. It arrives as a file
where the agent reads one, or as an environment variable where it does not.

Most people never fill it by hand. One command carries the login already on
your Mac into it:

```bash
brig run claude-code               # log in inside the sandbox, or:
brig secret import claude-code     # carry your host login in, once
```

The first of those does not persist: an in-sandbox login is written to
`~/.claude/.credentials.json` on a memory-backed mount. That is what keeps that
credential off host disk, so it dies with the sandbox on `brig stop`. Importing
is what survives a stop.

If you keep credentials in 1Password, Vault or `pass`, you have two roads.
Pipe the value in once, with `op read ... | brig secret create <name>` or
`brig secret import <profile> <name> --from-command 'op read ...'`. Or keep
using the environment: an `env.<name>` binding still reads Brig's own
environment on every run. That is why
`<your secret manager's run-with-env command> -- brig run claude-code` remains
a working integration for the variables a profile binds that way.

What the keychain does and does not protect is
[security.md](security.md#the-secret-store). This page is about using it.

## The six verbs

| verb | what it does |
| --- | --- |
| `brig secret create <name>` | store a new secret. Refuses if the name is taken |
| `brig secret read <name>` | print the value |
| `brig secret update <name>` | replace an existing value. Refuses if the name is not there |
| `brig secret delete <name>` | remove it, after asking. `-y` answers in advance |
| `brig secret ls` | names, dates and where each value came from. Never values |
| `brig secret import <profile>` | fill that profile's secrets from your host, once |

Neither `create` nor `update` takes the value as an argument. See
[Getting a value in](#getting-a-value-in). `import` is the one verb whose
argument is a **profile** rather than a secret name. It says so when you
give it a name by mistake:

```console
$ brig secret import claude-credentials
brig: "claude-credentials" is a secret, not a profile, and import takes the profile that declares it: brig secret import claude-code claude-credentials
```

`create` and `update` are each other's mirror. Neither will do the other's
job: `create` refuses a name that is taken, `update` refuses one that is not
there. That is what turns a mistyped name into a message rather than a
silently lost secret. Each refusal names the command you evidently meant:

```console
$ printf %s "$TOKEN" | brig secret create gh-token
$ printf %s "$TOKEN" | brig secret create gh-token
brig: a secret named "gh-token" already exists. To replace it: brig secret update gh-token
$ printf %s "$TOKEN" | brig secret update gh-tokne
brig: no secret named "gh-tokne". To create it: brig secret create gh-tokne
```

A successful write prints nothing. Silence is the report.

`delete` and `ls` still answer to two retired spellings, `rm` and `list`. Both
print a deprecation notice and are scheduled for removal in 0.3.
[migration.md](migration.md#subverbs) has the full mapping of retired
spellings Brig still accepts.

`ls` prints three columns and no values:

```console
$ brig secret ls
NAME                UPDATED           FROM
claude-credentials  2026-08-18 12:31  keychain:Claude Code-credentials
deploy-key          2026-08-15 21:09  -
gh-token            2026-08-15 21:09  -
```

`--json` prints the same three facts as data, one object per secret. It
leaves out any field a secret does not have, instead of printing a dash for it:

```console
$ brig secret ls --json
{
  "apiVersion": "brig.sh/v1alpha1",
  "kind": "SecretList",
  "data": []
}
```

It reads keychain attributes only, never a value, which is why it raises no
access prompt however many secrets you have. `UPDATED` is the item's own
modification date. A backend that cannot supply one renders as `-` in the text
form, or omits `modified` in the JSON form, rather than inventing a date. The
keychain always supplies one, so you will not see that today. A future
backend can omit it.

`FROM` is provenance: where `brig secret import` read the value. `import`
records it in the keychain item's comment attribute, which is why listing
costs no decrypt. A dash in the text form, or an absent `provenance` field in
the JSON form, means Brig did not put the value there. You created it by
hand. A `--from-command` value reads as `command (a command you gave)` rather
than the command line itself, because the line can hold a quote, a pipe or a
credential.

Provenance also carries the credential's expiry when the profile declared an
`expiryField:`. That is what lets a run warn, before boot and without
decrypting anything, that a stored copy has gone stale:

```console
$ brig run claude-code ~/code/demo
brig: the imported credential claude-credentials (claude-code) expired 3h ago.
brig: Renew it on the host, then: brig secret import claude-code
```

A secret with no `sources:`, filled instead with `--from-command`, gets a
different second line. The profile-wide `import` cannot refill it:
`Renew it, then store it again: brig secret import <profile> <name> --from-command '<command>'`.

An empty store is an ordinary state, not an error, and says how to leave it:

```console
$ brig secret ls
no secrets yet. To add one: brig secret create <name>
```

`delete` is the one verb that stops to ask, because it is the one whose
mistake cannot be undone. Nothing in Brig keeps a copy, and the keychain keeps
no history of its own. Every other destructive Brig command acts on something
that can be made again: a sandbox reboots, a profile is re-exported.

```console
$ brig secret delete gh-token
brig: delete "gh-token"? The value cannot be recovered [y/N] y
deleted gh-token
```

With no terminal to ask on, it refuses rather than assuming yes. Assuming yes
makes the scripted case the one that cannot be stopped:

```console
$ echo | brig secret delete gh-token
brig: deleting "gh-token" cannot be undone, and there is no terminal to ask on. Pass -y to answer in advance: brig secret delete gh-token -y
```

## A worked example

Storing a GitHub token, using it, rotating it and removing it. The commands
are real. The exact wording Brig prints is illustrative, since no test pins it
byte for byte.

Store it. `printf %s` rather than `echo`, so no newline is stored, and the
value comes down a pipe rather than sitting in your history:

```console
$ printf %s 'ghp_16C7e42F292c6912E7710c838347Ae178B4a' | brig secret create gh-token
$ brig secret ls
NAME      UPDATED           FROM
gh-token  2026-08-15 21:09  -
```

Use it. `claude-code` declares `gh-token`, so nothing else is needed: the
name in the store is the binding:

```console
$ brig info claude-code
...
brig: forwarding to guest:
brig:   GH_TOKEN(secret)
brig: never forwarded for claude-code: ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN (they would move this sandbox onto metered billing)
```

`(secret)` says the value came from Brig's store rather than from your shell.
`brig info` reports it. `brig run claude-code` does it.

That denylist covers environment forwarding only. It does not check a
`files:` binding. It does not stop an agent that reads a credential inside the
guest from sending it over the network. See
[security.md](security.md#what-file-delivery-buys-and-what-it-costs) for
why the guard is scoped that way.

An exported `GH_TOKEN` still wins, because the profile binds the name as a
chain (`refs: [env.GH_TOKEN, secrets.gh-token]`). So
`GH_TOKEN=$(gh auth token) brig run claude-code` keeps working exactly as it
did before the store existed. The stored value is the fallback for a shell
that exports nothing.

Rotate it. `update` refuses to create, so a typo here cannot quietly leave you
with two secrets and the old one still in use:

```console
$ printf %s 'ghp_9a1FfE0d5B7c4A2e8D3b6C1a0F5e9D8c7B6a' | brig secret update gh-token
```

Nothing else has to change. Brig re-reads what it hands the guest on every
exec. The next command picks up the new value, and the sandbox does not need
restarting.

Remove it:

```console
$ brig secret delete gh-token
brig: delete "gh-token"? The value cannot be recovered [y/N] y
deleted gh-token
$ brig secret ls
no secrets yet. To add one: brig secret create <name>
```

## `import`, to fill a profile from your host

`brig secret import <profile>` reads where your host already keeps that
profile's credentials and copies them into Brig's store, so that every run
afterwards reads only the store. No test pins the exact wording Brig prints,
so treat this as the shape rather than a literal transcript:

```console
$ brig secret import claude-code
claude-code: importing 1 secret
  claude-credentials: stored from keychain:Claude Code-credentials, expires 2026-08-19 01:31
  gh-token: no source on your host, so it is one you supply: brig secret create gh-token
note: claude-desktop also declares claude-credentials, so this fills it there too
```

Where it looks is data in the profile, not knowledge in Brig: each secret
carries a `sources:` list and the first that exists wins. `brig agent ls` shows
which names a profile can import and which it cannot, and
[profiles.md](profiles.md) is how to declare them in one of your own.

A profile can still declare a credential with the deprecated `hostCredential:`
field instead of `secrets:` and `sources:`. It keeps working, but reads the
named host keychain item on every run rather than once at import.
[migration.md](migration.md#profile-keys) has the replacement. This page
assumes `secrets:`.

| flag | what it does |
| --- | --- |
| `--dry-run` | preview the action: **read the sources** to check them, but write nothing |
| `-y` | replace a value Brig did not write, without asking |
| `--from-command '<sh>'` | take one named secret's value from a command's stdout instead of from its declared sources |

`[name...]` after the profile narrows it to the names you list.

Four rules worth knowing before you build anything on it:

- **It reads your host's own credential stores when you type it, and never
  again.** A run afterwards reads only Brig's own keychain item, which carries
  the default ACL, so it raises no approval dialog. That dialog belongs to
  `import` itself, once.
- **The copy does not track its source.** Renewing the login on the host does
  not update Brig's copy, and revoking it does not invalidate it. Re-import it
  to refresh the copy. `brig secret delete` removes it for good.
- **`import` does not replace a value Brig did not write.** No provenance means
  you created it by hand, and import stops rather than discarding something it
  cannot recover:

  ```console
  $ brig secret import claude-code
  brig: "claude-credentials" is already stored and brig did not put it there, so importing would replace a value you supplied. To replace it: brig secret import claude-code claude-credentials -y
  ```

- **An unchanged value is skipped rather than rewritten**, so `UPDATED` keeps
  meaning "the value last changed" rather than "an import last ran".

### The exit status

Non-zero when a secret that **has** an importer is not filled: the
source was there and gave nothing, or reading it failed. A name with no
importer at all is informational and does not fail the command.
`brig secret import x && brig run x` works for a profile that mixes imported
and hand-created secrets.

The consequence to plan around: on a machine that has never run the agent,
there is nothing to import and the command exits non-zero. That is why these
docs lead with `brig run claude-code`. Putting `import` first greets a
fresh machine with a red exit code for a state that is perfectly normal.

### The size ceiling is a real limit, and `codex` hits it

A stored value is capped at about 3KB (see [the size limit](#the-size-limit)).
That is enough for every API key, every OAuth credential document and every
ed25519 key. It is **not** enough for a credential document the size of
`codex`'s `~/.codex/auth.json`, which carries two JWTs. It does not fit, and
it cannot be delivered as a file today.

## Getting a value in

The value is never an argument. That is what keeps it out of `ps` and out of
your shell history. It is also why there is no `brig secret create gh-token
ghp_...` to type. It comes from stdin, or from a file:

| how | what gets stored |
| --- | --- |
| stdin, the default (`--stdin` if you like it spelled out) | the bytes, less one trailing line ending |
| `-f FILE` | the file's bytes, verbatim |
| `-f -` | stdin, spelled out. Same stripping as above |

Either source is capped at 4096 bytes. `create` and `update` read no more than
that before the value ever reaches the store, and refuse a longer one:

```console
$ brig secret create x < /dev/zero
brig: the value on stdin is over 4096 bytes, which is larger than any secret brig can store. If that is a file or a stream rather than a credential, this is the wrong one
```

That cap is where *reading* stops. What the store itself accepts is tighter,
about 3KB once the name and provenance are folded in: see
[the size limit](#the-size-limit).

**stdin strips exactly one trailing line ending, and `-f` does not**. The
asymmetry is the one thing on this page most worth remembering, so here is
each half of it.

`echo tok |` is the line people actually type, and `echo` adds a newline that
was never part of the secret:

```console
$ echo tok | brig secret create with-echo
$ brig secret read with-echo | xxd
00000000: 746f 6b                                  tok
```

Three bytes, not four. Storing the newline is the worse default by some
distance. A trailing newline inside an `Authorization:` header fails in a way
that reads like a bad token rather than a bad store. You go looking
at GitHub's settings page rather than here. CRLF counts as one line ending for
the same reason. A lone `\r` left behind fails identically, and it is harder
to spot in a bug report.

A file is taken as it is, because a file's bytes are what it holds and a PEM
key's final newline belongs to it:

```console
$ printf 'tok\n' > tok.txt
$ brig secret create from-file -f tok.txt
$ brig secret read from-file | xxd
00000000: 746f 6b0a                                tok.
```

Four bytes. So when you want exact bytes from stdin, say so with `printf %s`
rather than reaching for a flag. There is no flag, because this covers it:

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

A terminal is the exception. It gets a newline, so your prompt does not land
against the tail of a token. It also gets a warning on stderr. The reason
`create` will not take a value from a terminal does not stop applying on the
way out. The value is now in the scrollback of a window that outlives the
command.

```console
$ brig secret read gh-token
ghp_16C7e42F292c6912E7710c838347Ae178B4a
brig: gh-token is now in this terminal's scrollback. Pipe it instead to keep it out: brig secret read gh-token | ...
```

The warning is on stderr, so a pipe never sees it and stdout carries the value
and nothing else.

Command substitution is the case worth being careful with, and the care is the
shell's rather than Brig's: `$(...)` strips *all* trailing newlines. For a
token, that is exactly what you want, and it is why the composed run at the
top of this page is correct. For a value whose trailing newline matters (a PEM
key stored with `-f`), it is not:

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
the keychain account, where a space or a slash makes the item awkward to
address by hand. It is a word in Brig's own error messages. And a profile
references it as the tail of `ref: secrets.<name>`, which is what rules out
the `.` specifically, since a dot makes that reference ambiguous. A
leading digit reads as a number rather than a name, and a leading dash reads
as a flag wherever the name is typed.

## The size limit

A value is capped at around 3KB, and it is deliberately not a round number.

Brig hands `security` the write command on a single line, and `security` reads
one command into a 4096-byte buffer. The budget is the whole command rather
than the value, so the name competes with the value it names. The name appears
twice on that line, as the keychain account and in the item's label. And
base64 spends four characters for every three bytes of value.

| name | longest value `create` takes | `update` |
| --- | --- | --- |
| `a` (1 character) | 3012 bytes | 3006 bytes |
| `gh-token` (8 characters) | 3003 bytes | 2994 bytes |
| `deploy-key` (10 characters) | 3000 bytes | 2991 bytes |
| a 43-character name | 2949 bytes | 2943 bytes |

`update` is tighter because its command carries `-U` and an empty `-j` that
clears the previous provenance. Base64's four-for-three then rounds that gap
to six or nine bytes, depending on the name's length.

Every API key and every SSH key you are likely to have fits: an ed25519
private key is 387 bytes. A 4096-bit RSA private key does not, and is refused
with both numbers named rather than stored short:

```console
$ brig secret create deploy-key -f rsa4096.pem
brig: the value for "deploy-key" is 3272 bytes, and with its provenance the keychain takes at most 3000
```

Brig also reads back what it just wrote and compares the bytes. A `create`
that does not match is removed, since a caller told the write failed expects
nothing to be there. An `update` that does not match cannot be undone the same
way, since the previous value is already gone, so it only reports the
mismatch.

## Linux

Linux stores into a Secret Service keyring on your D-Bus session bus:
`gnome-keyring` and KWallet both speak that API. Brig keeps one item per secret
in your default collection, tagged as its own so it lists and touches nothing
else in the keyring. It opens the collection through your keyring UI when it
is locked.

When there is no D-Bus session bus, or no keyring answering on it, `brig
secret` says which is missing. It does not fall back to a file. That is
a downgrade nothing told you about:

```console
$ brig secret create gh-token
brig: no secret store on this platform: a D-Bus session bus is running but no Secret Service answers on it. Install a keyring (gnome-keyring or KWallet, both speak the Secret Service API) and log in to a session that starts it, or read the secret once from a command's output with `brig secret import <profile> --from-command '<sh>'`, which stores it like any other import
```

It says so *before* asking for a value. The check happens ahead of the read
from stdin. You are not sent off to find a token only to be told afterwards
that there is nowhere to put it. `--from-command` is the other way out for a
value that lives in an external secret manager. The import runs the command
once, takes its stdout, and stores that the way it stores any other import.
It still needs a store to write into. So on a host with no keyring, it is a
way to fill the store from somewhere other than a file. It is not a way
around having one.

## Errors you are likely to meet

| what Brig says | what happened |
| --- | --- |
| `a secret named "x" already exists. To replace it: brig secret update x` | `create` will not overwrite |
| `no secret named "x". To create it: brig secret create x` | `update` will not create |
| ``no secret named "x". `brig secret ls` lists them`` | `read` or `delete` on a name that is not there |
| `create x was given an empty value, and brig skips empty variables when it forwards them, so it would never reach a sandbox` | the source was empty. Brig refuses because a forwarded empty variable is skipped, so the secret does nothing but confuse whoever went looking |
| `no value on stdin. Pipe one in, or pass -f <file>: …` (and prints two examples) | `create` at a prompt with nothing piped in. It refuses rather than waiting, because typing the value there puts it in your scrollback |
| ``-f was given an empty path. Leave it out to read stdin, or pass `-f -` to say so`` | `-f "$KEYFILE"` with the variable unset. Falling through to stdin stores whatever the script had on it, under your name, and reports success |
| `--stdin and -f name two different sources; pass one` | both given, and guessing which you meant silently stores the wrong one |
| `the value on stdin is over 4096 bytes, which is larger than any secret brig can store. If that is a file or a stream rather than a credential, this is the wrong one` | `create` or `update` read more than 4096 bytes before ever reaching the store. `-f FILE` names the file in place of `stdin` |
| `the value for "x" is N bytes, and with its provenance the keychain takes at most M` | over [the size limit](#the-size-limit) |
| `deleting "x" cannot be undone, and there is no terminal to ask on. Pass -y to answer in advance: …` | a cron job or a unit file. `-y` is the answer given ahead |
| `a secret name holds letters, digits, - and _, ...` | see [Naming a secret](#naming-a-secret) |
| `no secret store on this platform: … no Secret Service answers on it …` | Linux with no keyring on the D-Bus session bus. Install `gnome-keyring` or KWallet and log in to a desktop session that starts it |
| `no secret store on this platform: … a keyring is running but has no default collection …` | Linux with a keyring but no default collection (a headless or freshly provisioned session that has not opened one). Open your keyring once from a desktop session, which creates it |
| `"x" is a secret, not a profile, and import takes the profile that declares it: …` | `import`'s first argument is a profile. The message names the one that declares the secret you typed |
| `nothing to import for "x": … held no value` | the profile's sources exist and none of them had anything. Usually: run the agent on the host once to log in |
| `"x" is already stored and brig did not put it there, so importing would replace a value you supplied` | you created it by hand. `-y` if replacing it is what you meant |
| `the value for "x" is N bytes and the store takes at most M, so nothing was written` | over [the size limit](#the-size-limit), checked before the write so a re-import cannot destroy a good value |
| `--from-command fills one secret, so it needs one name` | it supplies a value, and nothing in the command says which secret it is for |
| `the imported credential x (y) expired N ago.`, followed by `Renew it on the host, then: brig secret import y` | a run found a stored, imported credential past its `expiryField:`. Log in on the host again and re-import |
