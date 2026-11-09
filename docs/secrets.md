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
imported login survives a stop. To choose a login path, see
[Logging an agent in](authentication.md).

If you keep credentials in 1Password, Vault or `pass`, you have two options:

- Pipe the value in once. Use `op read ... | brig secret create <name>` or
  `brig secret import <profile> <name> --from-command 'op read ...'`.
- Keep the value in the environment. An `env.<name>` binding reads Brig's
  environment on every run, so
  `<your secret manager's run-with-env command> -- brig run claude-code`
  works for the variables that a profile binds that way.

[The secret store](security.md#the-secret-store) lists what the keychain
protects and what it does not.

## The six verbs

| verb | what it does |
| --- | --- |
| `brig secret create <name> [--type tls\|ca]` | store a new secret. Refuses if the name is taken. `--type` checks the value first |
| `brig secret read <name>` | print the value |
| `brig secret update <name> [--type tls\|ca]` | replace an existing value. Refuses if the name is not there. `--type` checks the value first |
| `brig secret delete <name>` | remove it, after asking. `-y` answers in advance |
| `brig secret ls` | names, dates and where each value came from. Never values |
| `brig secret import <profile>` | fill that profile's secrets from your host, once |

`create` and `update` read the value from stdin or from a file
([Getting a value in](#getting-a-value-in)).

`import` takes a **profile** as its argument. If you give it a secret name,
the error names the profile that declares the secret:

```console
$ brig secret import claude-credentials
brig: "claude-credentials" is a secret, not a profile, and import takes the profile that declares it: brig secret import claude-code claude-credentials
```

Each refusal names the command to use:

```console
$ printf %s "$TOKEN" | brig secret create gh-token
$ printf %s "$TOKEN" | brig secret create gh-token
brig: a secret named "gh-token" already exists. To replace it: brig secret update gh-token
$ printf %s "$TOKEN" | brig secret update gh-tokne
brig: no secret named "gh-tokne". To create it: brig secret create gh-tokne
```

A successful write prints nothing.

v0.5.0 removed `rm` and `list`, the retired spellings of `delete` and `ls`.
Brig refuses each as a usage error that names its replacement
([the full mapping](migration.md#subverbs)).

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

`ls` reads no value, so it raises no access prompt.

| column | meaning |
| --- | --- |
| `UPDATED` | when the value last changed. If the store cannot supply a date, the text form prints `-` and the JSON form omits `modified` |
| `FROM` | the provenance: the place that `brig secret import` read the value from |
| `FROM` is `-`, or the JSON form has no `provenance` field | Brig did not put the value there. You created it by hand |
| `FROM` is `command` | the value came from `--from-command`. Brig does not record the command line, because it can hold a credential |

An empty store is not an error:

```console
$ brig secret ls
no secrets yet. To add one: brig secret create <name>
```

### Expiry warnings

If the profile declares an `expiryField:`, the provenance also carries the
credential's expiry. Before boot, a run warns that a stored copy has
expired:

```console
$ brig run claude-code ~/code/demo
brig: the imported credential claude-credentials (claude-code) expired 3h ago
  → renew it on the host, then:  brig secret import claude-code
```

A secret with no `sources:` that you filled with `--from-command` gets a
different second line, because the profile-wide `import` cannot refill it:
`renew it, then store it again:  brig secret import <profile> <name> --from-command '<command>'`.

A certificate stored with `--type tls` gets a warning within three days of
its expiry, and once it has expired. Brig prints it once per `brig plan`,
and once when `brig run` or `brig sh` starts a sandbox or attaches to one.

The first line reads
`the client certificate <name> (route <route>) expires in <time>`, or
`expired <time> ago`. When several routes present the same secret, the line
names all of them: `(routes <route>, <route>)`. It names the command that
stores the renewed copy: `brig secret update <name> --type tls`.

A certificate stored without `--type` gets no warning. A plain
`brig secret update <name>` without `--type tls` clears the recorded
expiry, so the warning stops too. Store the renewed copy with `--type tls`.

### Delete a secret

`delete` asks first, because a deleted value cannot be recovered. Brig
keeps no copy, and the keychain keeps no history.

```console
$ brig secret delete gh-token
brig: delete "gh-token"? The value cannot be recovered [y/N] y
deleted gh-token
```

`delete` also removes the secret's
[renewal journal](#the-renewal-journal), which can hold a renewed copy of
the value. It removes a journal whose secret is already gone as well, and
says `deleted the renewal journal <name>.journal`.

If there is no terminal to ask on, `delete` refuses:

```console
$ echo | brig secret delete gh-token
brig: deleting "gh-token" cannot be undone, and there is no terminal to ask on. Pass -y to answer in advance: brig secret delete gh-token -y
```

## A worked example

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
brig:   CLAUDE_CONFIG_DIR
brig:   GH_TOKEN(secret)
brig: never forwarded for claude-code: ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN (they would move this sandbox onto metered billing)
```

`(secret)` marks a value from the store, not from your shell. `brig info`
only reports it. `brig run claude-code` forwards it.

The denylist in the last line covers environment forwarding only. It does
not check a `files:` binding, and it does not stop an agent that reads a
credential inside the guest from sending it over the network. See
[What file delivery buys and what it costs](security.md#what-file-delivery-buys-and-what-it-costs).

An exported `GH_TOKEN` wins over the stored value, so
`GH_TOKEN=$(gh auth token) brig run claude-code` works. The stored value is
the fallback for a shell that exports nothing. See
[Git access in the guest](authentication.md#git-access-in-the-guest).

**Rotate the token.** `update` does not create a secret, so a mistyped name
fails and does not leave the old value in use:

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

`brig secret import <profile>` copies that profile's credentials from your
host into the store:

```console
$ brig secret import claude-code
claude-code: importing 1 secret
  claude-credentials: stored from keychain:Claude Code-credentials, expires 2026-08-19 01:31
  gh-token: no source on your host, so it is one you supply: brig secret create gh-token
note: claude-desktop also declares claude-credentials, so this fills it there too
```

Each secret in the profile carries a `sources:` list, and the first source
that exists wins. `brig agent ls` shows which names a profile can import and
which it cannot. To declare sources in your own profile, see
[Profiles](profiles.md).

| flag | what it does |
| --- | --- |
| `--dry-run` | preview the action: **read the sources** to check them, but write nothing |
| `-y` | replace a value Brig did not write, without asking |
| `--from-command '<sh>'` | take one named secret's value from a command's stdout instead of from its declared sources |
| `--aws-profile <p>` | for a secret whose source is `aws`: the AWS profile on your host that the sandbox's keys come from. See [An AWS profile behind a route](#an-aws-profile-behind-a-route) |
| `--role-arn <arn>` | with `--aws-profile`: the sandbox assumes this role. The recommended answer |
| `--identity self` | with `--aws-profile` or `--gcloud-configuration`: the sandbox gets your own identity |
| `--source-identity <id>` | with `--role-arn`: the source identity of the role session, in place of the one Brig reads from the profile |
| `--session-duration <d>` | with `--role-arn`: the lifetime of the role session, `25m` to `1h`. Default `30m` |
| `--gcloud-configuration <c>` | for a secret whose source is `gcloud`: the gcloud configuration on your host that the sandbox's tokens come from. See [A gcloud login behind a route](#a-gcloud-login-behind-a-route) |
| `--impersonate-service-account <email>` | with `--gcloud-configuration`: the sandbox acts as this service account. The recommended answer |

To import only some secrets, add their names (`[name...]`) after the profile.

Four rules apply:

- **`import` reads the credential stores of your host when you run it, and
  never again.** A later run reads only Brig's own copy, so only `import`
  raises an approval dialog. The `aws` and `gcloud` sources are the
  exceptions. Each time `brig-broker` fetches keys or mints a token, it
  runs the host's `aws` or `gcloud` CLI, which reads your login.
- **The copy does not track its source.** If you renew the login on the host,
  the copy does not change. If you log out on the host, the copy stays
  valid. To refresh the copy, import it again. `brig secret delete`
  removes the copy permanently.
- **`import` does not replace a value that Brig did not write.** A value with
  no provenance is one you created by hand. `import` stops, because it
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

So `brig secret import x && brig run x` works for a profile that mixes
imported secrets and secrets created by hand.

### Large credential documents

A credential document of several kilobytes stores the same way as a token.
Examples are the Claude Code document after plugins add their MCP OAuth
state, and the `~/.codex/auth.json` of `codex`. The only limit is 65536
bytes ([Getting a value in](#getting-a-value-in)).

## Getting a value in

`create` and `update` take the value from stdin or from a file. The value is
never an argument, which keeps it out of `ps` and out of your shell history.

| how | what gets stored |
| --- | --- |
| stdin, the default (`--stdin` if you like it spelled out) | the bytes, less one trailing line ending |
| `-f FILE` | the file's bytes, verbatim |
| `-f -` | stdin, spelled out. Same stripping as above |
| `-f` more than once | the files appended in the order given. A `-` among them is stdin, stripped the same way and refused at a terminal the same way |

Each source has a limit of 65536 bytes. `create` and `update` read no more,
and refuse a longer value before it reaches the store:

```console
$ brig secret create x < /dev/zero
brig: the value on stdin is over 65536 bytes, which is larger than any secret brig can store. If that is a file or a stream rather than a credential, this is the wrong one
```

**stdin strips one trailing line ending. `-f` strips nothing.**

`echo` adds a newline that is not part of the secret, and Brig strips it:

```console
$ echo tok | brig secret create with-echo
$ brig secret read with-echo | xxd
00000000: 746f 6b                                  tok
```

The stored value is three bytes. A stored newline goes into an
`Authorization:` header, and the failure looks like a bad token. CRLF counts
as one line ending.

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

### Certificates and CA bundles

With `--type tls` or `--type ca`, Brig checks the value before it stores it,
and writes nothing if the check fails. The refusal
reads `<verb> <name>: the value is not a client certificate: <reason>` for
`tls`, and `<verb> <name>: the value is not a CA bundle: <reason>` for `ca`,
where `<verb>` is `create` or `update`. For example,
`create inventory-client: the value is not a client certificate: no private key block`.
The reason is in the right column:

| The value | The refusal says |
| --- | --- |
| no `CERTIFICATE` block | no usable CERTIFICATE block |
| a `CERTIFICATE` block that does not parse | for `tls`, no usable CERTIFICATE block: a certificate block does not parse; for `ca`, a CERTIFICATE block does not parse (block N) |
| no private key block | no private key block |
| more than one private key block | more than one private key block |
| a key of type `ENCRYPTED PRIVATE KEY`, or with `Proc-Type: 4,ENCRYPTED` | the private key is encrypted |
| a key block of another type that ends in `PRIVATE KEY`, such as `OPENSSH PRIVATE KEY` or `DSA PRIVATE KEY` | unsupported key block "OPENSSH PRIVATE KEY", with the type of the block in quotes |
| a `PRIVATE KEY`, `RSA PRIVATE KEY` or `EC PRIVATE KEY` block that does not parse | the private key block does not parse |
| a key that parses and is not RSA, ECDSA or Ed25519, such as RSA-PSS or X25519 | unsupported private key type |
| no certificate whose public key matches the key | the private key does not match the certificate |
| more than one different certificate that matches the key | more than one certificate matches the private key |
| a leaf past its `notAfter` | the certificate has expired on 2026-11-02T12:00:00Z, with the date of the `notAfter` |
| a leaf before its `notBefore` | the certificate is not yet valid: it is valid from 2026-11-02T12:00:00Z, with the date of the `notBefore` |
| a leaf with an extended key usage that holds neither `clientAuth` nor `anyExtendedKeyUsage`, such as a certificate for `serverAuth` alone. A leaf with no extended key usage is accepted | the certificate's extended key usage does not allow client authentication |

For `--type ca`, the first two rows apply too: a CA bundle needs at least
one `CERTIFICATE` block, and every block must parse. It also needs one
certificate that is a CA (basic constraints `CA:TRUE`), or the refusal says
no CERTIFICATE block is a CA. Only the CA certificates in the bundle become
roots.

A PKI hands out a client certificate in one of two layouts, and both work:

```bash
# one bundle: the certificate, its chain and the key in one file
brig secret create inventory-client --type tls -f bundle.pem

# separate files, appended one after the other, in any order
brig secret create inventory-client --type tls -f client.crt -f client.key -f ca.crt
cat client.crt intermediate.crt client.key | brig secret create inventory-client --type tls

brig secret create example-root-ca --type ca -f root-ca.pem
```

The files are appended in the order given, with a line break between two
when the first does not end in one, and the result is stored as given.

A `tls` value holds `CERTIFICATE` blocks and exactly one private key block
(`PRIVATE KEY`, `RSA PRIVATE KEY` or `EC PRIVATE KEY`), in any order. Any
other block whose type ends in `PRIVATE KEY` is refused by name, not read as
a missing key. Text outside the blocks, such as the `Bag Attributes` lines
that `openssl pkcs12` writes, is ignored. Brig reads it in four steps:

1. The leaf is the one certificate whose public key matches the private key.
   A certificate that appears twice, byte for byte, counts once. A
   `fullchain.pem` that repeats its leaf works. Brig refuses a value with no
   matching certificate, and a value with two different ones.
2. The chain is built from the leaf up. The next certificate is one that
   signed the one before and is a CA (basic constraints `CA:TRUE`), so a
   matching name is not enough. A certificate that does not continue the
   chain is not sent.
3. A self-signed root at the top of the chain is not sent either, because the
   upstream holds its own roots.
4. The broker sends the leaf and then the chain in the handshake.

So the order of the blocks never matters, and a bundle that also carries the
CA works as it is. On success `create` and `update` print to stderr the
certificate's subject, issuer and expiry, the length of the chain, and the
subject of each certificate that will not be sent, and never the key. They
print it after the value is stored, so a `create` that is refused because the
name exists prints nothing.

A `ca` value holds one or more `CERTIFICATE` blocks. `inject.tls.ca` can name
the same secret as `inject.tls.clientCertificate`, for a PKI whose root signs
both the client certificate and the certificate of the upstream. The roots are
then the certificates in that bundle that are CAs (basic constraints
`CA:TRUE`), and the broker does not read the private key in it for that.

`brig secret ls` does not show the type.

## Getting a value out

`read` writes the value to stdout and adds nothing, so a pipe gets the stored
bytes unchanged:

```console
$ brig secret read from-file | xxd
00000000: 746f 6b0a                                tok.
```

On a terminal, `read` adds a trailing newline, so your prompt starts on a
new line. It also warns on stderr, because the value stays in the
scrollback of a window that outlives the command:

```console
$ brig secret read gh-token
ghp_16C7e42F292c6912E7710c838347Ae178B4a
brig: gh-token is now in this terminal's scrollback
  → to keep it out, pipe it:  brig secret read gh-token | ...
```

Command substitution changes the value: the shell strips *all* trailing
newlines from `$(...)`. That is correct for a token and wrong for a value
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

## Where a value lives

On macOS, a secret lives in your login keychain, under the service
`sh.brig.secret`. Brig stores it as two items: `<name>` holds a random key,
and `<name>.sealed` holds the value, encrypted under that key. The renewal
journal is a third item, `<name>.journal`. `brig secret ls` lists neither
the `.sealed` nor the `.journal` item. The value and the key never appear on
a command line. The ciphertext and the secret's name do, and other processes
on the host can read them there.

The encryption does not change who can read the value. Each item has the
keychain's default ACL, so anything that can run `/usr/bin/security` as you
can read it with no dialog. A process that can read Brig's keychain items as
you can also decrypt the value.
[The secret store](security.md#the-secret-store) lists what the keychain
stops and what it does not.

After a write, Brig reads the value back and compares the bytes. If they
differ, `create` removes the secret. `update` only reports the mismatch,
because the previous value is already gone.

<details><summary>A secret stored by an older Brig</summary>

A secret stored by an older Brig keeps working. The next `update` or import
moves it into the two-item layout. An older Brig refuses a secret stored in
this layout, and never gives the key to a guest.

</details>

## A secret behind a route

A policy's [route](policies.md#routes) names a secret in this store. On a run
with that route, Brig neither reads nor delivers the secret.
`brig-broker` reads it from the store on each request through the route, and
adds it on the host. The guest gets a session token. A `files:` or `env:`
binding of the same secret in the profile is dropped for that run, and
`brig info` lists it as kept on the host.

A run whose route's secret is missing, or whose store does not open, is
refused with exit `6` before boot. The message names the command that puts
the secret there. A secret removed after the boot makes the route answer
502, and `~/.brig/host/broker/audit.log` says why. Create or import the
secret, and the next request finds it.

A `clientCertificate` or `ca` secret of [`inject.tls`](policies.md#client-certificates)
is read the same way, on each request. The guest never gets it.

A secret behind an `aws` route holds a pointer to an AWS profile and no
key. The broker reads it on each request and answers with keys it fetches
through `aws`. See [An AWS profile behind a route](#an-aws-profile-behind-a-route).
A route that proxies does not send such a pointer upstream: it answers 502
with `check=credential.vendonly`, since the pointer names your AWS profile
and role.

A secret behind a `gcp` route can hold a pointer to a gcloud configuration
in place of an access token. The broker reads it on each request and injects
a token it mints through `gcloud`. See
[A gcloud login behind a route](#a-gcloud-login-behind-a-route). A route
without `services` does not send such a pointer upstream: it answers 502 with
`check=credential.gcponly`, since the pointer names your gcloud configuration
and service account.

### An AWS profile behind a route

The [aws preset](policies.md#the-aws-preset) hands the guest temporary AWS
keys. Its secret names the AWS profile on your host that the keys come from.
Declare the secret in a profile with an `aws` source:

```yaml
secrets:
  - name: aws-creds
    sources:
      - from: aws
```

Then import it, and say how the sandbox gets AWS:

```bash
brig secret import <profile> aws-creds --aws-profile dev \
  --role-arn arn:aws:iam::111122223333:role/brig-coding-agent
```

| Mode | Flag | What the sandbox gets |
| --- | --- | --- |
| role | `--role-arn <arn>` | keys of a session of that role, assumed with your profile for 30 minutes at a time (`--session-duration`). The session is named after the sandbox, so CloudTrail shows which sandbox made a call |
| self | `--identity self` | keys of your own identity in that profile. The sandbox can do what you can |

`import` refuses a line that names neither mode, or both, so self mode is
never a default. An `aws` flag on a secret whose source is not `aws` is
refused too. On a profile with other secrets, name the `aws` secret on the
line. The flags answer for one secret, so a line that selects two `aws`
secrets is refused. Import each on its own line.

In role mode the session carries a source identity. Brig takes the last
path segment of the profile's caller ARN, which for an SSO login is your
user name. `--source-identity` sets another. It is 2 to 64 of the characters
`A-Za-z0-9+=,.@_-`.

A role session lasts 30 minutes, which bounds how long a leaked key works.
`--session-duration` sets `25m` to `1h`. The role cannot enforce the
length, because IAM has no condition key for it. Brig requests it.

Before it stores anything, `import` checks with `aws` that the profile
works, that it has a region, and in role mode that it can assume the role.
The guest gets the region as `AWS_REGION` and `AWS_DEFAULT_REGION`. A
profile with no region is refused with the command that sets one.
`--dry-run` runs the same checks and stores nothing. In self mode, `import`
prints the identity the sandbox will get. `brig secret ls` shows
`aws:<profile>` in the `FROM` column.

`aws` runs with every `AWS_` variable removed from its environment. A
profile that works only because your shell exports `AWS_PROFILE`,
`AWS_CONFIG_FILE` or a key fails at import, and not at the first run.

When the SSO login lapses and `aws` has no cached credentials left, a new
run is refused with exit `6`. The message names
`aws sso login --profile <profile>`.
[The aws route](security.md#the-aws-route) lists what the guest holds and
how to revoke it.

### A gcloud login behind a route

The [gcp preset](policies.md#the-gcp-preset) injects a Google access token
into each request. Its secret can name the gcloud configuration on your host
that the token comes from. Declare the secret in a profile with a `gcloud`
source:

```yaml
secrets:
  - name: gcp-creds
    sources:
      - from: gcloud
```

Then import it, and say how the sandbox gets GCP:

```bash
brig secret import <profile> gcp-creds --gcloud-configuration default \
  --impersonate-service-account brig-readonly@my-project.iam.gserviceaccount.com
```

| Mode | Flag | What the sandbox gets |
| --- | --- | --- |
| impersonate | `--impersonate-service-account <email>` | tokens of that service account, minted with your gcloud login. The service account's IAM roles limit what the sandbox can do. Your account needs `roles/iam.serviceAccountTokenCreator` on it |
| self | `--identity self` | tokens of your own gcloud login, the account the configuration names at import. The sandbox can do what you can |

As for `aws`, `import` refuses a line that names neither mode, or both, so
self mode is never a default. `--gcloud-configuration` is required. A
`gcloud` flag on a secret whose source is not `gcloud` is refused, and so is
a line that selects two `gcloud` secrets. A line with an `aws`-only flag and
a `gcloud` flag is refused. Import each secret on its own line.

The configuration name is a lowercase letter followed by up to 62 lowercase
letters, digits and `-`. The service account is a `*.gserviceaccount.com`
address.

In self mode, `import` records the account that the configuration names. A
configuration with no account is refused. If you later change the
configuration's account, with `gcloud config set account` or a
`gcloud auth login` as another user, mints fail until you import the secret
again. The sandbox does not get the other identity.

Before it stores anything, `import` mints one token, as the broker will. It
sends the token to Google's tokeninfo endpoint to check the identity it
belongs to, and refuses a token that names no identity or another one. A
lapsed login or a
missing `serviceAccountTokenCreator` grant is refused here, and not at the
guest's first Google call. `--dry-run` runs the same check and stores
nothing. `import` prints the identity the sandbox will act as:

```
gcp: the sandbox will act as brig-readonly@my-project.iam.gserviceaccount.com (impersonated by you@example.com)
gcp: the sandbox will get your own identity you@example.com
```

`brig secret ls` shows `gcloud:<configuration>` in the `FROM` column.

`gcloud` runs without `GOOGLE_APPLICATION_CREDENTIALS`, `CLOUDSDK_CONFIG`,
`CLOUDSDK_CORE_ACCOUNT`, `CLOUDSDK_ACTIVE_CONFIG_NAME`, any `CLOUDSDK_AUTH_*`
variable, and the variables that set HTTP logging or the log level. A
variable in your shell does not decide which identity the sandbox gets.
`gcloud` reads your logins from `$HOME/.config/gcloud`. `import` refuses
while `CLOUDSDK_CONFIG` is set. Unset it, or log in under
`$HOME/.config/gcloud` with `gcloud auth login --configuration <c>`.

When the login of the configuration has lapsed, a new run is refused with
exit `6`. The message names `gcloud auth login --configuration <c>`, or in
self mode `gcloud auth login <account> --configuration <c>`.
[The gcp route](security.md#the-gcp-route) lists what the guest holds and
the limits of this version.

A static token still works as the secret of a `gcp` route, for a quick try
without a `gcloud` source. See [The gcp preset](policies.md#the-gcp-preset).

## Renewal on the host

`brig-broker` renews a secret this way for a run whose
[route](policies.md#routes) serves it, on macOS with the `hvi` backend and
on Linux with nerdctl and urunc ([Transports](broker.md#transports)). Every other run delivers
`claude-credentials` as a file, as
[What file delivery buys](security.md#what-file-delivery-buys-and-what-it-costs)
describes.

A credential type can carry a lifecycle that renews the stored value on the
host, so the guest never holds what it renews with. The first one is
`anthropic-oauth`, for the `claude-credentials` document that
`brig secret import claude-code` stores.

| | `anthropic-oauth` |
| --- | --- |
| What refreshes | The OAuth pair in `claudeAiOauth`: `accessToken`, `refreshToken` and `expiresAt` |
| When | 10 minutes before `expiresAt`, or halfway through the life of a pair Brig refreshed when that life is shorter than 20 minutes, and once when the provider refuses the access token |
| From where | The host, to the provider's token endpoint over HTTPS, with system roots and full verification. Never through a broker route, and never after a redirect. The host's proxy settings (`HTTPS_PROXY`, `NO_PROXY`) apply |
| What is written back | The same document with the three fields replaced. Every other field is kept. The provenance keeps its `FROM`, and its expiry moves to the new `expiresAt` |
| What is refused | An answer that is not a 200 with an access token and a refresh token. Brig keeps the old document and writes nothing. For another status it reports the status code. For a 200 it cannot use, it reports a fixed error and nothing from the body |

The provider makes each refresh token single-use, so one owner on the host
refreshes each secret, one refresh at a time. Two sandboxes that share the
secret wait on the same refresh.

Once a refresh request has reached the provider, Brig does not send that
refresh token again, whatever fails after it. Brig serves the old document
until its own expiry and reports the refresh as failing.

The new document is written back to the store before its access token is
used. Until the write lands, the stored secret holds a document the provider
already refuses, and the new document is in
[the renewal journal](#the-renewal-journal).

Brig reads the store again at least every 5 minutes. A deleted secret then
stops being served. A secret you import again replaces the document in
memory, and a pending write-back never overwrites it. After a
failed refresh, log in on the host and run `brig secret import claude-code`.

The import copies the host's own refresh token. Brig and Claude Code on the
host then hold the same pair, and whichever side refreshes first invalidates
the copy the other holds. Brig's write-back keeps its own store current. It
does not reach the host's Claude Code. Do not run Claude Code on the host
against the same account during a brokered run.

### The renewal journal

A crash of `brig-broker`, or a host that loses power, between the
provider's answer and the write-back would leave the store with a spent
token. So the broker journals each refresh as `<name>.journal`, a store
item next to the secret and never a file.

1. The broker creates the journal in phase `renewing`. It records when the
   refresh started and a hash of the document it started from. If the journal cannot
   be written, the refresh is not sent, Brig reports it as failing, and the
   broker tries again 30 seconds later at the earliest.
2. The broker calls the provider.
3. The journal moves to phase `renewed`, and now holds the new document.
4. The secret takes the new document.
5. The broker deletes the journal.

A write that fails is retried, and the broker serves the new document from
memory meanwhile. No other refresh of the secret starts until the journal is
gone. A refused access token in that window is not renewed again.

When the broker next serves the secret, it reads the journal first. It also
does this when a sandbox registers a route for the secret, without waiting
for a request.

| state found | what the broker does | what you see |
| --- | --- | --- |
| `renewed`, and the secret is still the document the refresh started from | Writes the journal's document to the secret, and deletes the journal. It does not call the provider | `brig doctor` reports `refresh recovering` until the write lands. The run goes on with the renewed token |
| `renewed`, and the secret already holds the journal's document | Deletes the journal | Nothing |
| `renewing`, and the secret is still the document the refresh started from | Keeps the secret, deletes the journal, and renews once at the next request, whatever the expiry. The refresh may have reached the provider, and the provider may have rotated the stored token | `refresh recovering` until the provider answers. If the provider refuses the token as spent, the secret is reported as failing and is not renewed again. Log in on the host and import again |
| Any journal, and the secret changed since the refresh started | Deletes the journal. A secret you imported again is newer than the journal | Nothing |
| A journal of another broker that still runs, younger than 10 minutes | Leaves it alone, does not renew, and serves the stored document until it expires. It reads the journal again every 30 seconds | `refresh recovering` |
| A journal of another broker, older than 10 minutes or dated in the future | The broker treats it as left by a broker that stopped | As above for its phase |
| A journal of this broker that it did not know of | Deletes it, and renews 30 seconds later | `refresh failing` until that renewal |
| A journal Brig cannot read: damaged in the store, not one Brig wrote, or a `renewed` document that is not a credential of the secret's kind | Keeps the secret, deletes the journal, and renews once, as for `renewing` | As for `renewing` |
| The store does not answer | Serves the stored document, and reads the journal again 30 seconds later at the earliest | `refresh recovering` |

The journal records the process id of its broker and the start time of that
process. A broker counts as running when a process of yours has that id and
that start time. `brig secret delete` removes the journal with the secret.
`brig secret read` and `delete` refuse a journal's own name, because a
journal can hold the only valid copy of a renewed credential.

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

Brig checks for the store before it reads stdin, so you learn of a missing
store before you fetch a token. `--from-command` also needs a keyring to
store into.

## Common errors

| what Brig says | what happened |
| --- | --- |
| `a secret named "x" already exists. To replace it: brig secret update x` | `create` does not overwrite |
| `no secret named "x". To create it: brig secret create x` | `update` does not create |
| ``no secret named "x". `brig secret ls` lists them`` | `read` or `delete` on a name that is not there |
| `create x was given an empty value, and brig skips empty variables when it forwards them, so it would never reach a sandbox` | the source was empty |
| `no value on stdin. Pipe one in, or pass -f <file>: …` (and prints two examples) | `create` at a prompt with nothing piped in. Brig does not wait for input, because a typed value stays in your scrollback |
| ``-f was given an empty path. Leave it out to read stdin, or pass `-f -` to say so`` | `-f "$KEYFILE"` with the variable unset. Brig does not fall through to stdin, because that stores the stdin of the script under your name and reports success |
| `--stdin and -f name two different sources; pass one` | both given. Brig does not guess which one you meant |
| `the value on stdin is over 65536 bytes, which is larger than any secret brig can store. If that is a file or a stream rather than a credential, this is the wrong one` | `create` or `update` read more than 65536 bytes. `-f FILE` names the file in place of `stdin` |
| `"x" secret is damaged: the key is in the keychain, but its sealed value is missing. Store it again: …` | the `x.sealed` item was removed and the key item was not. See [Where a value lives](#where-a-value-lives) |
| `"x" secret is damaged: the sealed item does not open with the key stored for it, so one of them was changed outside brig. Store it again: …` | something other than Brig replaced one of the two items. Store the value again to replace both |
| `"x" secret is damaged: the sealed item is not a brig sealed value, so something other than brig put it there. Store it again: …` | the `x.sealed` item is not in Brig's format. Store the value again to replace both |
| `deleting "x" cannot be undone, and there is no terminal to ask on. Pass -y to answer in advance: …` | a cron job or a unit file. Pass `-y` |
| `a secret name holds letters, digits, - and _, ...` | see [Naming a secret](#naming-a-secret) |
| `no secret store on this platform: … no Secret Service answers on it …` | Linux with no keyring on the D-Bus session bus. Install `gnome-keyring` or KWallet and log in to a desktop session that starts it |
| `no secret store on this platform: … a keyring is running but has no default collection …` | Linux with a keyring but no default collection (a headless or freshly provisioned session that did not open one). Open your keyring once from a desktop session, which creates the collection |
| `"x" is a secret, not a profile, and import takes the profile that declares it: …` | the first argument of `import` is a profile. The message names the one that declares the secret |
| `nothing to import for "x": … held no value` | the sources of the profile exist and none of them held a value. Usually: run the agent on the host once to log in |
| `"x" is already stored and brig did not put it there, so importing would replace a value you supplied` | you created it by hand. Pass `-y` if you want to replace it |
| `--from-command fills one secret, so it needs one name` | `--from-command` supplies a value, and nothing in the command says which secret the value is for |
| `say how the sandbox gets AWS: --role-arn <arn> (recommended) or --identity self` | an import of an `aws` source that names neither mode |
| `the AWS SSO session for profile x has expired; …` | the SSO login of the profile lapsed. Run the command on the host, then run again |
| `the aws command was not found; install the AWS CLI v2: …` | the `aws` CLI is not on `PATH`. For the broker, that is the `PATH` of the `brig` that started it |
| `say how the sandbox gets GCP: --impersonate-service-account <email> (recommended) or --identity self` | an import of a `gcloud` source that names neither mode |
| `--impersonate-service-account and --identity self are two answers; pass one` | an import of a `gcloud` source that names both modes |
| `--gcloud-configuration is required: …` | an import of a `gcloud` source with a mode and no configuration |
| `the gcloud configuration x has no account; run gcloud auth login --configuration x` | a self-mode import of a configuration that is not logged in |
| `gcloud minted a token for x, not the service account y` | an impersonate-mode mint returned a token that tokeninfo says belongs to another identity, or to none |
| `the gcloud configuration x minted a token for y, not z, the account it was imported with; …` | the configuration's account changed after a self-mode import. Import the secret again to use the new account |
| `the aws and gcloud flags fill different secrets; import each on its own line` | one import line carries an `aws`-only flag and a `gcloud` flag |
| `the gcloud login of configuration x has expired; …` | the gcloud login lapsed. Run the command on the host, then run again |
| `your gcloud account cannot impersonate x; it needs roles/iam.serviceAccountTokenCreator on that service account` | your account has no grant to mint tokens for the service account. Grant the role in IAM |
| `the gcloud command was not found; install the Google Cloud CLI: …` | the `gcloud` CLI is not on `PATH`. For the broker, that is the `PATH` of the `brig` that started it |
| `the imported credential x (y) expired N ago`, followed by `renew it on the host, then:  brig secret import y` | a run found a stored, imported credential past its `expiryField:`. Log in on the host again and import again |
