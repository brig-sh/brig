# Keeping secrets in your keyring

`brig secret` is Brig's own store, and it is the **only** store a run reads.
A profile declares the names it wants under `secrets:`. What is in this
store under those names reaches the sandbox. It arrives as a file
where the agent reads one, or as an environment variable where it does not.

You rarely need to fill it by hand. One command copies the login already on
your host into it:

```bash
brig run claude-code               # log in inside the sandbox, or:
brig secret import claude-code     # carry your host login in, once
```

The first does not persist. An in-sandbox login is written to
`~/.claude/.credentials.json` on a memory-backed mount. That keeps the
credential off host disk, so it is gone after `brig stop`. An imported login
survives a stop.

If you keep credentials in 1Password, Vault or `pass`, you have two options.
Pipe the value in once, with `op read ... | brig secret create <name>` or
`brig secret import <profile> <name> --from-command 'op read ...'`. Or keep
using the environment: an `env.<name>` binding still reads Brig's own
environment on every run, so
`<your secret manager's run-with-env command> -- brig run claude-code` works
for the variables a profile binds that way.

[security.md](security.md#the-secret-store) says what the keychain does and
does not protect.

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
argument is a **profile**. It says so when you give it a secret name by
mistake:

```console
$ brig secret import claude-credentials
brig: "claude-credentials" is a secret, not a profile, and import takes the profile that declares it: brig secret import claude-code claude-credentials
```

`create` refuses a name that is taken, and `update` refuses one that is not
there, so a mistyped name produces an error. Each refusal names the command
you probably meant:

```console
$ printf %s "$TOKEN" | brig secret create gh-token
$ printf %s "$TOKEN" | brig secret create gh-token
brig: a secret named "gh-token" already exists. To replace it: brig secret update gh-token
$ printf %s "$TOKEN" | brig secret update gh-tokne
brig: no secret named "gh-tokne". To create it: brig secret create gh-tokne
```

A successful write prints nothing.

`delete` and `ls` still answer to two retired spellings, `rm` and `list`. Both
print a deprecation notice and are removed in v0.4.0.
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

`ls` reads keychain attributes only, never a value, so it raises no access
prompt however many secrets you have. `UPDATED` is the item's own modification
date. A backend that cannot supply one prints `-` in the text form and omits
`modified` in the JSON form.

`FROM` is provenance: where `brig secret import` read the value. `import`
records it in the keychain item's comment attribute, so listing decrypts
nothing. A dash in the text form, or an absent `provenance` field in the JSON
form, means Brig did not put the value there: you created it by hand. A
`--from-command` value reads as `command`. Brig does not record the command
line, because it can hold a quote, a pipe or a credential.

Provenance also carries the credential's expiry when the profile declares an
`expiryField:`. A run uses it to warn, before boot and without decrypting
anything, that a stored copy has expired:

```console
$ brig run claude-code ~/code/demo
brig: the imported credential claude-credentials (claude-code) expired 3h ago
  → renew it on the host, then:  brig secret import claude-code
```

A secret with no `sources:`, filled instead with `--from-command`, gets a
different second line. The profile-wide `import` cannot refill it:
`renew it, then store it again:  brig secret import <profile> <name> --from-command '<command>'`.

An empty store is not an error:

```console
$ brig secret ls
no secrets yet. To add one: brig secret create <name>
```

`delete` asks before it acts, because a deleted value cannot be recovered.
Nothing in Brig keeps a copy, and the keychain keeps no history. Every other
destructive Brig command acts on something that can be made again: a sandbox
reboots, a profile is re-exported.

```console
$ brig secret delete gh-token
brig: delete "gh-token"? The value cannot be recovered [y/N] y
deleted gh-token
```

With no terminal to ask on, it refuses:

```console
$ echo | brig secret delete gh-token
brig: deleting "gh-token" cannot be undone, and there is no terminal to ask on. Pass -y to answer in advance: brig secret delete gh-token -y
```

## A worked example

This example stores a GitHub token, uses it, rotates it and removes it.

Store it. The value goes in on stdin:

```console
$ printf %s 'ghp_16C7e42F292c6912E7710c838347Ae178B4a' | brig secret create gh-token
$ brig secret ls
NAME      UPDATED           FROM
gh-token  2026-08-15 21:09  -
```

Use it. `claude-code` declares `gh-token`, so storing a secret under that
name is all it takes:

```console
$ brig info claude-code
...
brig: forwarding to guest:
brig:   IS_SANDBOX
brig:   GH_TOKEN(secret)
brig: never forwarded for claude-code: ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN (they would move this sandbox onto metered billing)
```

`(secret)` says the value came from Brig's store rather than from your shell.
`brig info` only reports this. `brig run claude-code` forwards it.

That denylist covers environment forwarding only. It does not check a
`files:` binding. It does not stop an agent that reads a credential inside the
guest from sending it over the network. See
[security.md](security.md#what-file-delivery-buys-and-what-it-costs) for
why the guard is scoped that way.

An exported `GH_TOKEN` still wins, because the profile binds the name as a
chain (`refs: [env.GH_TOKEN, secrets.gh-token]`). So
`GH_TOKEN=$(gh auth token) brig run claude-code` still works. The stored
value is the fallback for a shell that exports nothing.

Rotate it. `update` refuses to create, so a typo cannot leave you with two
secrets and the old one still in use:

```console
$ printf %s 'ghp_9a1FfE0d5B7c4A2e8D3b6C1a0F5e9D8c7B6a' | brig secret update gh-token
```

Brig re-reads what it hands the guest on every exec, so the next command
picks up the new value without a restart.

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
profile's credentials and copies them into Brig's store. Every run
afterwards reads only the store:

```console
$ brig secret import claude-code
claude-code: importing 1 secret
  claude-credentials: stored from keychain:Claude Code-credentials, expires 2026-08-19 01:31
  gh-token: no source on your host, so it is one you supply: brig secret create gh-token
note: claude-desktop also declares claude-credentials, so this fills it there too
```

The profile declares where it looks: each secret carries a `sources:` list,
and the first source that exists wins. `brig agent ls` shows which names a
profile can import and which it cannot. [profiles.md](profiles.md) describes
how to declare them in a profile of your own.

| flag | what it does |
| --- | --- |
| `--dry-run` | preview the action: **read the sources** to check them, but write nothing |
| `-y` | replace a value Brig did not write, without asking |
| `--from-command '<sh>'` | take one named secret's value from a command's stdout instead of from its declared sources |

`[name...]` after the profile narrows it to the names you list.

Four rules apply:

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

- **An unchanged value is skipped**, so `UPDATED` keeps meaning "the value
  last changed" rather than "an import last ran".

### The exit status

`import` exits non-zero when a secret that **has** a source is not filled:
no source held a value, or reading one failed. A name with no source is
reported and does not fail the command, so
`brig secret import x && brig run x` works for a profile that mixes imported
and hand-created secrets.

On a machine that has never run the agent, there is nothing to import and the
command exits non-zero. That is why these docs lead with
`brig run claude-code`.

### Large credential documents fit

On macOS the value is sealed into a keychain item of its own, with the key
in another, so a credential document of several kilobytes stores the same
way a token does. That covers Claude Code's document once plugins add their
MCP OAuth state to it, and `codex`'s `~/.codex/auth.json` with its two JWTs.
See [Where a value lives](#where-a-value-lives) for the layout. The one
limit is the 64 KiB read cap below.

## Getting a value in

The value is never an argument, which keeps it out of `ps` and out of your
shell history. It comes from stdin, or from a file:

| how | what gets stored |
| --- | --- |
| stdin, the default (`--stdin` if you like it spelled out) | the bytes, less one trailing line ending |
| `-f FILE` | the file's bytes, verbatim |
| `-f -` | stdin, spelled out. Same stripping as above |

Either source is capped at 65536 bytes. `create` and `update` read no more
than that before the value ever reaches the store, and refuse a longer one:

```console
$ brig secret create x < /dev/zero
brig: the value on stdin is over 65536 bytes, which is larger than any secret brig can store. If that is a file or a stream rather than a credential, this is the wrong one
```

The cap exists to refuse a stream. No credential comes near it.

**stdin strips exactly one trailing line ending, and `-f` does not.**

`echo tok |` is the line people type, and `echo` adds a newline that was
never part of the secret:

```console
$ echo tok | brig secret create with-echo
$ brig secret read with-echo | xxd
00000000: 746f 6b                                  tok
```

Three bytes, not four. A stored newline ends up inside an `Authorization:`
header, and the failure looks like a bad token. CRLF counts as one line
ending for the same reason: a lone `\r` left behind fails the same way.

A file is stored as it is, because a PEM key's final newline belongs to it:

```console
$ printf 'tok\n' > tok.txt
$ brig secret create from-file -f tok.txt
$ brig secret read from-file | xxd
00000000: 746f 6b0a                                tok.
```

Four bytes. For exact bytes from stdin, use `printf %s`. There is no flag
for it:

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

A terminal is the exception. It gets a trailing newline, so your prompt does
not run into the token. It also gets a warning, because the value is now in
the scrollback of a window that outlives the command.

```console
$ brig secret read gh-token
ghp_16C7e42F292c6912E7710c838347Ae178B4a
brig: gh-token is now in this terminal's scrollback
  → to keep it out, pipe it:  brig secret read gh-token | ...
```

The warning is on stderr, so a pipe never sees it and stdout carries the value
and nothing else.

Take care with command substitution. The shell, not Brig, changes the value:
`$(...)` strips *all* trailing newlines. For a token, that is what you want.
For a value whose trailing newline matters (a PEM key stored with `-f`), it
is not:

```console
$ brig secret read from-file | wc -c
       4
$ printf %s "$(brig secret read from-file)" | wc -c
       3
```

When the bytes matter, redirect:

```bash
(umask 077; brig secret read deploy-key > ./deploy-key)
```

## Naming a secret

Every verb enforces the same narrow grammar:

- letters, digits, `-` and `_`
- it starts with a letter
- at most 128 characters

```console
$ printf %s v | brig secret create gh.token
brig: a secret name holds letters, digits, - and _, and "gh.token" holds "."
$ printf %s v | brig secret create 1password
brig: a secret name starts with a letter, and "1password" starts with "1"
```

The grammar is narrow because the name is used in three places. It is the
keychain account, where a space or a slash makes the item awkward to address
by hand. It is a word in Brig's own error messages. And a profile references
it as the tail of `ref: secrets.<name>`, which rules out the `.`, since a dot
makes that reference ambiguous. A leading digit reads as a number, and a
leading dash reads as a flag wherever the name is typed.

## Where a value lives

On macOS a secret is two items in your login keychain, both under the
service `sh.brig.secret`.

The item under the secret's name holds a random 32-byte key and nothing
else. Every such item is the same size. The item under `<name>.sealed`
holds the value, encrypted with AES-256-GCM under that key, with the
secret's name bound in so a sealed item moved under another name does not
open. `brig secret ls` never shows the sealed item: the dot puts its name
outside the grammar a secret can have, and the listing skips such names.

There are two items because Brig writes a keychain item through
`security -i`, which reads one command into a 4096-byte buffer and shortens
a longer line without saying so. With the value on that line, a secret over
about 3KB could not be stored, and a credential document with plugin state
or two JWTs is larger than that. The key item is still written that way. The
sealed item is written through `security`'s own arguments instead, which
have no such cap. A command line is readable by other processes on the
host, and what stands on this one is ciphertext and the secret's name. The
value is never on any command line, and the key reaches `security` only
down a pipe.

An `update` keeps the key and replaces the sealed item, which `security`
does whole, so the one step that changes the value is that replacement.
`delete` removes the key item first, then the sealed item; once the key is
gone the sealed item is unreadable, so a failure between the two leaves
nothing usable behind.

Sealing does not change what protects the value. Anything that can
read Brig's keychain items as you can read the key and open the sealed
item. Anything that cannot open the keychain gets ciphertext.
[security.md](security.md#the-secret-store) says what the keychain item's
ACL does and does not stop.

A secret stored by an older Brig holds its value in the keychain item
itself. `read` returns it as before, and the next `update` or re-import
moves it into the layout above. Nothing has to be migrated by hand. An
older Brig reading a key item refuses it as one it did not write, so it
never hands a guest the key bytes.

Brig also reads back what it just wrote and compares the bytes, which
checks that both items landed and agree. A `create` that does not
match is removed, since a caller told the write failed expects nothing to
be there. An `update` that does not match cannot be undone the same way,
since the previous value is already gone, so it only reports the mismatch.

Linux stores the value in the keyring item itself, because a Secret Service
keyring has no such ceiling. The section below covers it.

## Linux

Linux stores into a Secret Service keyring on your D-Bus session bus:
`gnome-keyring` and KWallet both speak that API. Brig keeps one item per secret
in your default collection, tagged as its own so it lists and touches nothing
else in the keyring. It opens the collection through your keyring UI when it
is locked.

When there is no D-Bus session bus, or no keyring answering on it, `brig
secret` says which is missing. It does not fall back to a file, because
that would be a downgrade nothing told you about:

```console
$ brig secret create gh-token
brig: no secret store on this platform: a D-Bus session bus is running but no Secret Service answers on it. Install a keyring (gnome-keyring or KWallet, both speak the Secret Service API) and log in to a session that starts it, or read the secret once from a command's output with `brig secret import <profile> --from-command '<sh>'`, which stores it like any other import
```

The check runs before Brig reads stdin, so you learn there is no store
before you go and fetch a token. `--from-command` is for a value that lives
in an external secret manager. The import runs the command once, takes its
stdout, and stores that the way it stores any other import. It still needs a
store to write into, so it does not replace a keyring.

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
| `the value on stdin is over 65536 bytes, which is larger than any secret brig can store. If that is a file or a stream rather than a credential, this is the wrong one` | `create` or `update` read more than 65536 bytes before ever reaching the store. `-f FILE` names the file in place of `stdin` |
| `"x" secret is damaged: the key is in the keychain, but its sealed value is missing. Store it again: …` | the `x.sealed` item was removed and the key item was not. See [Where a value lives](#where-a-value-lives) |
| `"x" secret is damaged: the sealed item does not open with the key stored for it, so one of them was changed outside brig. Store it again: …` | one of the two items was replaced by something other than Brig. Storing the value again replaces both |
| `"x" secret is damaged: the sealed item is not a brig sealed value, so something other than brig put it there. Store it again: …` | the `x.sealed` item holds something that is not Brig's format at all. Storing the value again replaces both |
| `deleting "x" cannot be undone, and there is no terminal to ask on. Pass -y to answer in advance: …` | a cron job or a unit file. `-y` is the answer given ahead |
| `a secret name holds letters, digits, - and _, ...` | see [Naming a secret](#naming-a-secret) |
| `no secret store on this platform: … no Secret Service answers on it …` | Linux with no keyring on the D-Bus session bus. Install `gnome-keyring` or KWallet and log in to a desktop session that starts it |
| `no secret store on this platform: … a keyring is running but has no default collection …` | Linux with a keyring but no default collection (a headless or freshly provisioned session that has not opened one). Open your keyring once from a desktop session, which creates it |
| `"x" is a secret, not a profile, and import takes the profile that declares it: …` | `import`'s first argument is a profile. The message names the one that declares the secret you typed |
| `nothing to import for "x": … held no value` | the profile's sources exist and none of them had anything. Usually: run the agent on the host once to log in |
| `"x" is already stored and brig did not put it there, so importing would replace a value you supplied` | you created it by hand. `-y` if replacing it is what you meant |
| `--from-command fills one secret, so it needs one name` | it supplies a value, and nothing in the command says which secret it is for |
| `the imported credential x (y) expired N ago`, followed by `renew it on the host, then:  brig secret import y` | a run found a stored, imported credential past its `expiryField:`. Log in on the host again and re-import |
