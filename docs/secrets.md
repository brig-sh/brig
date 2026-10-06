# Keeping secrets in your keyring

`brig secret` stores credentials in your keyring. It is the **only** store
that a run reads. A profile declares the names it wants under `secrets:`.
The value stored under each name reaches the sandbox. It arrives as a file
where the agent reads one, and as an environment variable otherwise.

<p align="center">
  <img alt="How a secret reaches the sandbox, for the built-in claude-code profile. A run reads Brig's secret store and no other. gh-token arrives as the GH_TOKEN environment variable, and claude-credentials arrives as the file ~/.claude/.credentials.json, held in memory. Your login keychain, your SSH agent and other secret managers do not reach the sandbox." src="../assets/secret-delivery.svg" width="820">
</p>

One command copies the login on your host into the store:

```bash
brig run claude-code               # log in inside the sandbox, or:
brig secret import claude-code     # carry your host login in, once
```

A `claude-code` login made inside the sandbox is gone after `brig stop`. An
imported login survives a stop. [Logging an agent in](authentication.md) compares the ways
to log in.

If you keep credentials in 1Password, Vault or `pass`, you have two options:

- Pipe the value in once. Use `op read ... | brig secret create <name>` or
  `brig secret import <profile> <name> --from-command 'op read ...'`.
- Keep the value in the environment. An `env.<name>` binding reads the
  environment of Brig on every run. As a result,
  `<your secret manager's run-with-env command> -- brig run claude-code`
  works for the variables that a profile binds that way.

[The secret store](security.md#the-secret-store) describes what the keychain
protects and what it does not protect.

## The six verbs

| verb | what it does |
| --- | --- |
| `brig secret create <name>` | store a new secret. Refuses if the name is taken |
| `brig secret read <name>` | print the value |
| `brig secret update <name>` | replace an existing value. Refuses if the name is not there |
| `brig secret delete <name>` | remove it, after asking. `-y` answers in advance |
| `brig secret ls` | names, dates and where each value came from. Never values |
| `brig secret import <profile>` | fill that profile's secrets from your host, once |

`create` and `update` read the value from stdin or from a file. See
[Getting a value in](#getting-a-value-in).

`import` takes a **profile** as its argument. If you give it a secret name,
the error names the profile that declares the secret:

```console
$ brig secret import claude-credentials
brig: "claude-credentials" is a secret, not a profile, and import takes the profile that declares it: brig secret import claude-code claude-credentials
```

A mistyped name produces an error. Each refusal names the command to use:

```console
$ printf %s "$TOKEN" | brig secret create gh-token
$ printf %s "$TOKEN" | brig secret create gh-token
brig: a secret named "gh-token" already exists. To replace it: brig secret update gh-token
$ printf %s "$TOKEN" | brig secret update gh-tokne
brig: no secret named "gh-tokne". To create it: brig secret create gh-tokne
```

A successful write prints nothing.

`delete` and `ls` also accept two retired spellings, `rm` and `list`. Both
print a deprecation notice, and v0.5.0 removes them. See
[the full mapping](migration.md#subverbs) of the retired spellings that Brig
accepts.

### List secrets

`ls` prints three columns and no values:

```console
$ brig secret ls
NAME                UPDATED           FROM
claude-credentials  2026-08-18 12:31  keychain:Claude Code-credentials
deploy-key          2026-08-15 21:09  -
gh-token            2026-08-15 21:09  -
```

`--json` prints the same three facts as data, one object per secret. It
omits a field that a secret does not have:

```console
$ brig secret ls --json
{
  "apiVersion": "brig.sh/v1alpha1",
  "kind": "SecretList",
  "data": []
}
```

`ls` reads keychain attributes only and never a value, so it raises no access
prompt. The number of secrets does not change this.

| column | meaning |
| --- | --- |
| `UPDATED` | the modification date of the item. If a backend cannot supply a date, the text form prints `-` and the JSON form omits `modified` |
| `FROM` | the provenance: the place that `brig secret import` read the value from |
| `FROM` is `-`, or the JSON form has no `provenance` field | Brig did not put the value there. You created it by hand |
| `FROM` is `command` | the value came from `--from-command`. Brig does not record the command line, because it can hold a quote, a pipe or a credential |

`import` records the provenance in the comment attribute of the keychain
item, so `ls` decrypts nothing.

An empty store is not an error:

```console
$ brig secret ls
no secrets yet. To add one: brig secret create <name>
```

### Expiry warnings

If the profile declares an `expiryField:`, the provenance also carries the
expiry of the credential. Before boot, a run uses the expiry to warn that a
stored copy is expired. The run decrypts nothing to do this:

```console
$ brig run claude-code ~/code/demo
brig: the imported credential claude-credentials (claude-code) expired 3h ago
  → renew it on the host, then:  brig secret import claude-code
```

A secret with no `sources:` that you filled with `--from-command` gets a
different second line, because the profile-wide `import` cannot refill it:
`renew it, then store it again:  brig secret import <profile> <name> --from-command '<command>'`.

### Delete a secret

`delete` asks before it removes a value, because you cannot recover a deleted
value. Brig keeps no copy, and the keychain keeps no history.

```console
$ brig secret delete gh-token
brig: delete "gh-token"? The value cannot be recovered [y/N] y
deleted gh-token
```

If there is no terminal to ask on, `delete` refuses:

```console
$ echo | brig secret delete gh-token
brig: deleting "gh-token" cannot be undone, and there is no terminal to ask on. Pass -y to answer in advance: brig secret delete gh-token -y
```

## A worked example

This example stores a GitHub token, uses it, rotates it and removes it.

**Store the token.** Send the value on stdin:

```console
$ printf %s 'ghp_16C7e42F292c6912E7710c838347Ae178B4a' | brig secret create gh-token
$ brig secret ls
NAME      UPDATED           FROM
gh-token  2026-08-15 21:09  -
```

**Use the token.** `claude-code` declares `gh-token`, so a secret stored
under that name needs no other setup:

```console
$ brig info claude-code
...
brig: forwarding to guest:
brig:   IS_SANDBOX
brig:   GH_TOKEN(secret)
brig: never forwarded for claude-code: ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN (they would move this sandbox onto metered billing)
```

`(secret)` shows that the value comes from the store and not from your shell.
`brig info` only reports this. `brig run claude-code` forwards the value.

The denylist in the last line covers environment forwarding only. It does not
check a `files:` binding. It does not stop an agent that reads a credential
inside the guest from sending it over the network. See
[What file delivery buys and what it costs](security.md#what-file-delivery-buys-and-what-it-costs)
for the reason.

An exported `GH_TOKEN` wins over the stored value, because the profile binds
the name as a chain (`refs: [env.GH_TOKEN, secrets.gh-token]`). As a result,
`GH_TOKEN=$(gh auth token) brig run claude-code` works. The stored value is
the fallback for a shell that exports nothing. See
[Git access in the guest](authentication.md#git-access-in-the-guest).

**Rotate the token.** `update` does not create a secret. A mistyped name
cannot leave you with two secrets and the old one in use:

```console
$ printf %s 'ghp_9a1FfE0d5B7c4A2e8D3b6C1a0F5e9D8c7B6a' | brig secret update gh-token
```

Brig reads the value again on every exec, so the next command gets the new
value without a restart.

**Remove the token.**

```console
$ brig secret delete gh-token
brig: delete "gh-token"? The value cannot be recovered [y/N] y
deleted gh-token
$ brig secret ls
no secrets yet. To add one: brig secret create <name>
```

## Import from your host

`brig secret import <profile>` copies the credentials of that profile from
your host into the store. Every later run reads only the store:

```console
$ brig secret import claude-code
claude-code: importing 1 secret
  claude-credentials: stored from keychain:Claude Code-credentials, expires 2026-08-19 01:31
  gh-token: no source on your host, so it is one you supply: brig secret create gh-token
note: claude-desktop also declares claude-credentials, so this fills it there too
```

The profile declares where `import` looks. Each secret carries a `sources:`
list, and the first source that exists wins. `brig agent ls` shows which
names a profile can import and which names it cannot. See
[Profiles](profiles.md) to declare sources in a profile of your own.

| flag | what it does |
| --- | --- |
| `--dry-run` | preview the action: **read the sources** to check them, but write nothing |
| `-y` | replace a value Brig did not write, without asking |
| `--from-command '<sh>'` | take one named secret's value from a command's stdout instead of from its declared sources |

To import only some secrets, add their names (`[name...]`) after the profile.

Four rules apply:

- **`import` reads the credential stores of your host when you run it, and
  never again.** A later run reads only the keychain item that Brig wrote.
  That item has the default ACL, so a run raises no approval dialog. Only
  `import` raises that dialog, one time.
- **The copy does not track its source.** If you renew the login on the host,
  the copy does not change. If you revoke the login on the host, the copy
  stays valid. To refresh the copy, import it again. `brig secret delete`
  removes the copy permanently.
- **`import` does not replace a value that Brig did not write.** A value with
  no provenance is one that you created by hand. `import` stops, because it
  cannot recover that value:

  ```console
  $ brig secret import claude-code
  brig: "claude-credentials" is already stored and brig did not put it there, so importing would replace a value you supplied. To replace it: brig secret import claude-code claude-credentials -y
  ```

- **`import` skips an unchanged value.** As a result, `UPDATED` shows when
  the value last changed, and not when an import last ran.

### Exit status

| case | result |
| --- | --- |
| a secret **has** a source and is not filled: no source held a value, or the read of a source failed | `import` exits non-zero |
| a secret has no source | `import` reports the name. This does not fail the command |
| the agent never ran on this machine, so there is nothing to import | `import` exits non-zero |

As a result, `brig secret import x && brig run x` works for a profile that
mixes imported secrets and secrets created by hand.

### Large credential documents

On macOS, a credential document of several kilobytes stores the same way as a
token. This includes the Claude Code document after plugins add their MCP
OAuth state to it. It also includes the `~/.codex/auth.json` of `codex`, with
its two JWTs. The only limit is the 65536-byte limit in
[Getting a value in](#getting-a-value-in). See
[Where a value lives](#where-a-value-lives) for the layout.

## Getting a value in

`create` and `update` take the value from stdin or from a file. The value is
never an argument, which keeps it out of `ps` and out of your shell history.

| how | what gets stored |
| --- | --- |
| stdin, the default (`--stdin` if you like it spelled out) | the bytes, less one trailing line ending |
| `-f FILE` | the file's bytes, verbatim |
| `-f -` | stdin, spelled out. Same stripping as above |

Each source has a limit of 65536 bytes. `create` and `update` read no more
than that, and they refuse a longer value before it reaches the store:

```console
$ brig secret create x < /dev/zero
brig: the value on stdin is over 65536 bytes, which is larger than any secret brig can store. If that is a file or a stream rather than a credential, this is the wrong one
```

The limit stops a stream. No credential is near that size.

**stdin strips one trailing line ending. `-f` strips nothing.**

`echo` adds a newline that is not part of the secret, and Brig strips it:

```console
$ echo tok | brig secret create with-echo
$ brig secret read with-echo | xxd
00000000: 746f 6b                                  tok
```

The stored value is three bytes. A stored newline goes into an
`Authorization:` header, and the failure looks like a bad token. CRLF counts
as one line ending, because a `\r` that stays fails the same way.

Brig stores a file unchanged, because the final newline of a PEM key is part
of the key:

```console
$ printf 'tok\n' > tok.txt
$ brig secret create from-file -f tok.txt
$ brig secret read from-file | xxd
00000000: 746f 6b0a                                tok.
```

The stored value is four bytes. To send unchanged bytes on stdin, use
`printf %s`. No flag does this:

```console
$ printf %s 'tok' | brig secret create exact
$ brig secret read exact | xxd
00000000: 746f 6b                                  tok
```

The two most common commands:

```bash
printf %s "$TOKEN" | brig secret create gh-token
brig secret create deploy-key -f ~/.ssh/id_ed25519
```

## Getting a value out

`read` writes the value to stdout and adds nothing, so a pipe gets the stored
bytes unchanged:

```console
$ brig secret read from-file | xxd
00000000: 746f 6b0a                                tok.
```

On a terminal, `read` adds a trailing newline, so that your prompt starts on
a new line. It also prints a warning, because the value stays in the
scrollback of a window that outlives the command:

```console
$ brig secret read gh-token
ghp_16C7e42F292c6912E7710c838347Ae178B4a
brig: gh-token is now in this terminal's scrollback
  → to keep it out, pipe it:  brig secret read gh-token | ...
```

The warning goes to stderr, so a pipe gets only the value on stdout.

Command substitution changes the value. The shell strips *all* trailing
newlines from `$(...)`. That is correct for a token. It is wrong for a value
that needs its trailing newline, such as a PEM key stored with `-f`:

```console
$ brig secret read from-file | wc -c
       4
$ printf %s "$(brig secret read from-file)" | wc -c
       3
```

If the bytes matter, redirect the output to a file:

```bash
(umask 077; brig secret read deploy-key > ./deploy-key)
```

## Naming a secret

Every verb applies the same rules to a name:

- letters, digits, `-` and `_`
- it starts with a letter
- at most 128 characters

```console
$ printf %s v | brig secret create gh.token
brig: a secret name holds letters, digits, - and _, and "gh.token" holds "."
$ printf %s v | brig secret create 1password
brig: a secret name starts with a letter, and "1password" starts with "1"
```

Brig uses the name in three places: as the keychain account, as a word in its
error messages, and as the tail of `ref: secrets.<name>` in a profile. A dot
makes that reference ambiguous. A leading digit reads as a number, and a
leading dash reads as a flag.

## Where a value lives

On macOS, a secret is two items in your login keychain. Both are under the
service `sh.brig.secret`.

| item | what it holds |
| --- | --- |
| `<name>` | a random 32-byte key and nothing else. Every key item is the same size |
| `<name>.sealed` | the value, encrypted with AES-256-GCM under that key. The name of the secret is bound in, so a sealed item moved under another name does not open |

`brig secret ls` never shows the sealed item. The dot puts its name outside
the name rules, and the listing skips such names.

Brig uses two items because of a limit in `security -i`. Brig writes a
keychain item through `security -i`, which reads one command into a 4096-byte
buffer. It shortens a longer line and gives no message. With the value on
that line, Brig cannot store a secret over about 3KB. A credential document
with plugin state or two JWTs is larger than that.

Brig writes the key item through `security -i`. It writes the sealed item
through the arguments of `security`, which have no such limit. Other
processes on the host can read a command line. That command line holds only
ciphertext and the name of the secret. The value is never on a command line,
and the key reaches `security` only through a pipe.

`update` keeps the key and replaces the sealed item in one step. `delete`
removes the key item first, and then the sealed item. Without the key, the
sealed item is unreadable, so a failure between the two steps leaves nothing
usable.

After a write, Brig reads the value back and compares the bytes. This makes
sure that both items exist and agree.

| write | if the bytes do not match |
| --- | --- |
| `create` | Brig removes the secret, because a caller that gets a failure expects nothing in the store |
| `update` | Brig only reports the mismatch. It cannot undo the write, because the previous value is already gone |

Sealing does not change what protects the value. A process that can read the
keychain items of Brig as you can read the key and open the sealed item. A
process that cannot open the keychain gets ciphertext.
[The secret store](security.md#the-secret-store) describes what the ACL of
the keychain item stops and what it does not stop.

Linux stores the value in the keyring item itself, because a Secret Service
keyring has no such limit. See [Linux](#linux).

<details><summary>A secret stored by an older Brig</summary>

A secret stored by an older Brig holds its value in the keychain item itself.
`read` returns that value. The next `update` or import moves it into the
two-item layout, so you do not migrate anything by hand. An older Brig that
reads a key item refuses it as an item it did not write. It never gives the
key bytes to a guest.

</details>

## Linux

On Linux, Brig stores secrets in a Secret Service keyring on your D-Bus
session bus. `gnome-keyring` and KWallet both speak that API. Brig keeps one
item per secret in your default collection. It tags each item as its own, so
it lists and changes nothing else in the keyring. If the collection is
locked, Brig opens it through your keyring UI.

If there is no D-Bus session bus, or no keyring answers on it, `brig secret`
says which one is missing. It does not fall back to a file:

```console
$ brig secret create gh-token
brig: no secret store on this platform: a D-Bus session bus is running but no Secret Service answers on it. Install a keyring (gnome-keyring or KWallet, both speak the Secret Service API) and log in to a session that starts it, or read the secret once from a command's output with `brig secret import <profile> --from-command '<sh>'`, which stores it like any other import
```

This check runs before Brig reads stdin, so you learn about the missing store
before you fetch a token.

`--from-command` is for a value in an external secret manager. The import
runs the command once and stores its stdout like any other import. It needs a
store to write into, so it does not replace a keyring.

## Common errors

| what Brig says | what happened |
| --- | --- |
| `a secret named "x" already exists. To replace it: brig secret update x` | `create` does not overwrite |
| `no secret named "x". To create it: brig secret create x` | `update` does not create |
| ``no secret named "x". `brig secret ls` lists them`` | `read` or `delete` on a name that is not there |
| `create x was given an empty value, and brig skips empty variables when it forwards them, so it would never reach a sandbox` | the source was empty. Brig skips an empty variable when it forwards, so it refuses to store one |
| `no value on stdin. Pipe one in, or pass -f <file>: …` (and prints two examples) | `create` at a prompt with nothing piped in. Brig does not wait for input, because a typed value stays in your scrollback |
| ``-f was given an empty path. Leave it out to read stdin, or pass `-f -` to say so`` | `-f "$KEYFILE"` with the variable unset. Brig does not fall through to stdin, because that stores the stdin of the script under your name and reports success |
| `--stdin and -f name two different sources; pass one` | both given. Brig does not guess, because a guess can store the wrong value silently |
| `the value on stdin is over 65536 bytes, which is larger than any secret brig can store. If that is a file or a stream rather than a credential, this is the wrong one` | `create` or `update` read more than 65536 bytes. `-f FILE` names the file in place of `stdin` |
| `"x" secret is damaged: the key is in the keychain, but its sealed value is missing. Store it again: …` | the `x.sealed` item was removed and the key item was not. See [Where a value lives](#where-a-value-lives) |
| `"x" secret is damaged: the sealed item does not open with the key stored for it, so one of them was changed outside brig. Store it again: …` | something other than Brig replaced one of the two items. Store the value again to replace both |
| `"x" secret is damaged: the sealed item is not a brig sealed value, so something other than brig put it there. Store it again: …` | the `x.sealed` item is not in the format of Brig. Store the value again to replace both |
| `deleting "x" cannot be undone, and there is no terminal to ask on. Pass -y to answer in advance: …` | a cron job or a unit file. `-y` gives the answer in advance |
| `a secret name holds letters, digits, - and _, ...` | see [Naming a secret](#naming-a-secret) |
| `no secret store on this platform: … no Secret Service answers on it …` | Linux with no keyring on the D-Bus session bus. Install `gnome-keyring` or KWallet and log in to a desktop session that starts it |
| `no secret store on this platform: … a keyring is running but has no default collection …` | Linux with a keyring but no default collection (a headless or freshly provisioned session that did not open one). Open your keyring once from a desktop session, which creates the collection |
| `"x" is a secret, not a profile, and import takes the profile that declares it: …` | the first argument of `import` is a profile. The message names the profile that declares the secret you typed |
| `nothing to import for "x": … held no value` | the sources of the profile exist and none of them held a value. Usually: run the agent on the host once to log in |
| `"x" is already stored and brig did not put it there, so importing would replace a value you supplied` | you created it by hand. Pass `-y` if you want to replace it |
| `--from-command fills one secret, so it needs one name` | `--from-command` supplies a value, and nothing in the command says which secret the value is for |
| `the imported credential x (y) expired N ago`, followed by `renew it on the host, then:  brig secret import y` | a run found a stored, imported credential past its `expiryField:`. Log in on the host again and import again |
