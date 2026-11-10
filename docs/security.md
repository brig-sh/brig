# What Brig protects

Brig runs an agent in a microVM. On the host, the agent can reach its guest
home, the credentials you deliver to it, and any project you name on the run
line. No other host directory is mounted.

A run reads no host credential source. See [Credentials](#credentials).

Network reach has weaker limits. See
[Things brig does not claim](#things-brig-does-not-claim). That section is
also the line between a vulnerability and a known limitation.

[Claims and the tests behind them](claims.md) ties promises here to the tests
that defend them. To report a flaw in a boundary privately, see
[SECURITY.md](../SECURITY.md).

## What the agent can reach

The guest can reach:

- its guest home, read-write
- the project you name on the run line, read-write at `/work/<name>`
- the credentials you deliver to it
- the internet, on either `isolated` or `shared` (see
  [Network egress](#network-egress) and [Host services](#host-services))
- a port another sandbox listens on, when both use `shared`, on `hvi` and on
  Linux (see [Sandbox to sandbox](#sandbox-to-sandbox))
- any hostmount volume a profile declares (see [The boundary](#the-boundary))

The guest cannot reach:

- any other host directory
- your keychain
- your SSH agent
- your secret manager
- an environment variable a profile's `deny` list refuses. A `deny` list does
  not cover a `files:` binding (see
  [file delivery](#what-file-delivery-buys-and-what-it-costs)).

The guest gets only the credentials you deliver to it.

The host can reach the guest only on a port you publish. See
[Published ports](#published-ports).

The agent changes your real files in the guest home and in the project, not a
copy of them. It can send anything it reads there over the network. It can
use a delivered credential, as any holder can, for as long as it holds it.

### Network posture

| Case | Posture |
| --- | --- |
| New sandbox on `hvi` or Linux | `isolated` |
| The graphical `claude-desktop` profile | `shared`, set explicitly in the profile |
| The unpublished `cursor` profile, which leaves its posture unset | `isolated` on Linux and `hvi`. The backend fallback on `vz` or `qemu` |
| Custom profile without a network choice, on `vz` or `qemu` | `shared`. `brig info` names that fallback |
| Existing sandbox | Its recorded posture |
| Existing sandbox from an older session with no record | The actual posture, inspected from the runtime |

Brig refuses a flagless run if it cannot establish the posture of an existing
sandbox.

`isolated` separates sandbox networks. It does not restrict internet access
or settle whether host services are reachable. See
[Sandbox to sandbox](#sandbox-to-sandbox) for what it guarantees, and
[Network postures](policies.md#network-postures) for overrides and backend
limits.

## The boundary

The sandbox is a microVM on macOS and on Linux.

| Host | How the microVM boots |
| --- | --- |
| macOS | [hull](https://github.com/brig-sh/hull) boots it. Its `hvi` backend uses Hypervisor.framework, and seven of the nine shipped profiles ask for `hvi`. Its `vz` backend uses Virtualization.framework, and `claude-desktop` and `cursor` run on `vz`. `BRIG_HYPERVISOR` overrides the profile. See [Run paths compared](runtimes.md#run-paths-compared). `brew install --cask brig` installs hull |
| Linux | Brig drives `nerdctl` and hands the container to the urunc shim (`io.containerd.urunc.v2`) by default. The guest gets its own kernel |

On a Linux host that has another microVM shim, `BRIG_CONTAINERD_RUNTIME` can
name that shim. Brig refuses a shim that it knows shares the host kernel:
`runc`, `crun` or the runc shim binaries `containerd-shim-runc-v1` and `-v2`,
by name or by path. A container that shares the host kernel is not the
boundary Brig provides.

The `ISOLATION` row of the execution envelope names the boundary this run
resolved. `brig info` prints it and boots nothing. `brig --verbose run`
prints it before the boot:

```
ISOLATION    microVM (hull, hvi backend, rootfs overlay-block)
ISOLATION    microVM (hull, vz backend, rootfs block)
ISOLATION    microVM (nerdctl over containerd, io.containerd.urunc.v2, hypervisor urunc default)
ISOLATION    microVM (nerdctl over containerd, io.containerd.urunc.v2, hypervisor hvi, rootfs overlay-block)
ISOLATION    microVM (nerdctl over containerd, io.containerd.urunc.v2, hypervisor hvi, rootfs urunc default)
ISOLATION    container (nerdctl over containerd, BRIG_CONTAINERD_RUNTIME=runc: the guest shares the host kernel)
ISOLATION    unknown (nerdctl over containerd, BRIG_CONTAINERD_RUNTIME=io.containerd.kata.v2: brig cannot tell whether that shim boots a kernel of its own)
```

Brig can boot with a shim it does not recognise, but cannot tell from the
name whether it boots its own kernel, so the row says `unknown`. A shim
Brig recognises as sharing the host kernel is named by `brig info`, and the
run is refused. A shim that `BRIG_CONTAINERD_RUNTIME` chose is printed with
the variable. The row also names the guest root. See
[The guest root of a generic boot](runtimes.md#the-guest-root-of-a-generic-boot).

Inside Brig, the guest has your guest home mounted as its home, read-write.
Name a project on the run line and that project is a second host directory,
also mounted read-write, at `/work/<name>`.

Each hostmount volume in the profile is one more share. Every hostmount
volume in a shipped profile is inside the guest home, so it exposes nothing
more. A hostmount that your own profile declares outside a tmpfs cover is a
host path the guest can see.

Beyond those, the guest does not have your keychain, your SSH agent, your
secret manager, or any other directory on the host. The guest cannot fetch a
credential for itself, so you must deliver each credential explicitly. See
[Credentials](#credentials).

### Published ports

Nothing on the host can open a connection into the sandbox until you publish
a port. To publish a port, use `--publish` on a run. On macOS, `brig network
publish` also opens one on a sandbox that is up. On Linux a port can be
published only when the sandbox is created. `brig network unpublish` closes
the port.

**The port binds to loopback by default.** A published port binds to
`127.0.0.1` unless you write an address, so it is reachable from this
machine and not from the network this machine is on. To ask for the wider
binding, write the address, as in `0.0.0.0:8080:80`.

**The execution envelope names the port.** The `PORTS` row lists every port
the sandbox publishes. `brig info` prints it, and `brig --verbose run` prints
it before the boot:

```
NETWORK      isolated (a network of this sandbox's own)
PORTS        127.0.0.1:8080 -> 80
             0.0.0.0:443 -> 443 (reachable from the network this host is on)
```

A publication outlives the run that made it, so the row can list ports this
command line did not ask for. A row with an address other than loopback says
so.

**No egress rule applies to the port.** A policy decides where the guest can
open a connection *to*, not a connection opened *into* the guest. A sandbox
under a strict egress policy is still reachable on a port it publishes.

## Credentials

You choose how the agent gets a login. Log in inside the sandbox, or import
your host login once:

```bash
brig run claude-code               # log in inside the sandbox, or:
brig secret import claude-code     # carry the host login in, once
```

`brig secret import <profile>` puts your host login into Brig's own store.
Every run afterwards reads only [that store](#the-secret-store).

**A run reads no host credential source.** Nothing on the
`brig run`, `sh` or `exec` path reaches a keychain item Brig did not write.
Nothing on that path reaches a credential file outside the guest home, or a
host command that produces one.

Two host reads do happen on a run, and no setting turns either off:

1. Brig runs `git config --get` in the directory you invoked it from, for
   `user.name`, `user.email` and `github.user`. The answers give the commit
   identity that Brig forwards into the guest.
2. If `github.user` is empty, Brig reads the `user:` line from the stanza for
   your git host in gh's `hosts.yml`, under `$GH_CONFIG_DIR` or
   `~/.config/gh`. That line names the login that pairs with the forwarded
   token. The file usually holds gh's own OAuth token too. Brig takes only
   the login.

`BRIG_GIT_IDENTITY=0`, `BRIG_GIT_CONFIG=0` and `BRIG_GIT_USER` change what
Brig does with the answers. They do not stop the reads.

A credential reaches the guest by one of two channels, which the profile
picks for each secret. A third channel, `broker`, keeps the credential on
the host. A policy's [route](policies.md#routes) names the secret, and the
guest gets a session token for the route in its place. Routes work on `hvi`
and on Linux with nerdctl.

| Channel | What it does |
| --- | --- |
| `files:` | Writes the credential into the guest at the path the agent already reads |
| `env:` | Binds the credential as an environment variable, for a credential whose consumer offers no file interface |
| `broker` | Keeps the credential in `brig-broker`, which adds it to each request the guest sends through the route. The client certificate, private key and CA bundle of a route's `inject.tls` stay there too. The guest gets a session token and the route URL. A `files:` or `env:` binding of the same secret is dropped |

`brig info --json` names the channel of each credential as its `source`:
`environment`, `secret`, `file` or `broker`.

For an `env.<name>` binding, Brig reads the named variable from its own
environment, so whatever sets that environment can supply the value.

Brig reads values again on every exec, so the sandbox picks up a rotated
credential without a restart. Nothing is written into the guest home from the
host for this.

Brig applies these rules when it resolves a secret:

- Brig skips a variable that is unset or empty, so it cannot shadow a value
  baked into the image.
- A `scheme://` value read from the environment is refused as an unresolved
  secret-manager reference. A `value:` literal or a value from Brig's own
  secret store skips this check. `BRIG_ALLOW_REFS=1` forwards an ambient
  reference anyway.
- A variable on the profile's `deny` list is refused, with the reason.

`brig info <agent>` reports the guest's environment by name, and never a
value. It fails the same way a run does if a required secret cannot be
resolved. A variable from a secret is annotated, for example
`GH_TOKEN(secret)`. A credential delivered as a file is not in that list.

On a Linux host with no keyring there is no
[secret store](#the-secret-store):

| Profile secrets | Result |
| --- | --- |
| *Optional*, as in `claude-code` | The run boots and the agent asks for a login |
| **Required** | The run fails, because that host has no place to read the secret from |

### What file delivery buys, and what it costs

A credential delivered as a file:

- stays out of `/proc/<pid>/environ`
- is not inherited by processes the agent spawns
- can be rewritten under a running agent, so a rotated secret can reach a
  live session. No environment variable can do that.
- lands on a memory-backed mount and does not reach your disk (see
  [What reaches host disk](#what-reaches-host-disk))

It also has costs:

- **Brig stores and hands over a refresh token.** Without one, Claude Code
  fails to refresh and prompts for a login. So a compromised agent inside
  the sandbox can mint access tokens indefinitely, even after the host's own
  token expires. In exchange, a long session does not break
  every few hours.
- **Brig's copy is less protected than the item it came from.** The host's
  Claude item is ACL-scoped to the application that wrote it, so it raises a
  dialog the first time something else reads it. Brig's copy has the default
  ACL, like every secret in [the secret store](#the-secret-store). The only
  real mitigation is to keep the stored copy low-value, and a refresh token is
  not low-value. When you do not use the copy, delete it:
  `brig secret delete claude-credentials`.
- **The `deny` list covers only environment variables.** A `files:` binding
  bypasses the deny check. A profile can deliver a metered API key inside a
  `settings.json`, and nothing sees it. `deny` exists to catch an
  **accident**: an ambient variable from your shell swept into the guest. A
  file binding needs an explicit stored secret and an explicit binding the
  profile author wrote.
- **The stored copy does not rotate.** A credential renewed on the host does
  not update Brig's copy. A credential *revoked* on the host stays valid in
  Brig's store until you import it again or delete it. Brig warns before boot
  when the stored copy is expired, and names the command that refreshes it.
  Brig cannot see a revocation.

A `0600` file is still readable by anything that runs as the agent's uid
inside the sandbox. File delivery narrows the exposure. It does not make a
boundary. See [What is still exposed](#what-is-still-exposed).

### What reaches host disk

The credential file does not reach host disk while the agent runs with
`CLAUDE_CONFIG_DIR`. That variable points the agent at a `tmpfs` mount at
`/brig/claude`, which is not on the home share. Brig checks that the mount
is `tmpfs` with no swap before it writes anything. `brig stop` removes that
mount with the sandbox, so an in-sandbox login on this profile does not
outlive a stop. The limits are under
[What is still exposed](#what-is-still-exposed).

Eight paths are hostmounted from `~/.claude` in the guest home into
`/brig/claude`. They are on host disk, and they persist across boots:

| Paths | Contents |
| --- | --- |
| `settings.json`, `CLAUDE.md` | Your permission allowlist and your user-level memory, written by hand or by the agent on your instruction |
| `sessions`, `projects`, `history.jsonl` | The conversation |
| `plugins`, `skills` | Plugins and skills. `--skills` copies your own here, so that flag does nothing if a profile leaves either off |
| `.claude.json` | The agent's global state: onboarding, trust and per-project settings |

Anything else under `/brig/claude` is ephemeral, including anything a future
Claude Code version starts to write there. The source of this list is the
`volumes:` block of the `claude-code` profile.

### Not in argv

Forwarded values go into the runtime process's own environment, and only the
variable *name* appears on its command line. So other processes on the host
cannot read a forwarded credential in `ps`.

`HOME`, `PATH`, `TMPDIR` and any `XDG_` variable are the exception. Brig
passes these on the command line as `NAME=value` on every run, because the
runtime reads them for itself. None of these variables carries a
credential, and Brig refuses a stored secret bound to one of these names.

`BRIG_ENV_ARGV=1` puts forwarded values back on the command line, for a
runtime that cannot take a variable by name alone. A value read from the
environment then loses this guarantee. A value from Brig's own secret store
stays off the command line either way. The guest agent logs the argv of
every exec. On some backends that log is a file on host disk, and a value on
the command line stays in it after the exec ends.

### What is still exposed

Give the agent a narrow credential. Prefer a fine-grained `GH_TOKEN` scoped
to the repositories you want reachable, over a classic PAT that carries your
whole account.

Anything that runs alongside the agent inside the sandbox can read the
credential, because the sandbox cannot use a credential it cannot see. Brig
limits the blast radius. It does not use a sentinel value or a host-side
proxy (see
[A TLS-terminating host proxy](non-goals.md#a-tls-terminating-host-proxy)).
A [route](policies.md#routes) is the exception. There the sandbox sees a
session token, which works through its route until it expires or the
sandbox is stopped. On a run with a route, the guest also trusts the brig
CA, for `brig.internal` alone. The `aws` route is not such an exception: its
guest holds temporary AWS keys. See [The aws route](#the-aws-route).

A route with a client certificate gives the token the certificate's
identity. The guest never holds the key, but whoever holds the route's
session token can send requests that the upstream sees as the certificate
holder's. Only the route's `request.allow` limits them, so make it as narrow
as the certificate's purpose.

The `claude-code` credential stays off host disk only while the agent has
`CLAUDE_CONFIG_DIR`. Brig hands it to every process it starts in the sandbox.
A `claude` started without it, under `sudo -i`, `su -` or `env -i` for
example, writes `.credentials.json` to `~/.claude` in the guest home, which
is on the home share and so on host disk.

The `herdr` profile writes the forwarded environment into the guest. Each
SSH session into a herdr sandbox copies everything it received to
`/run/herdr/env` for herdr panes. That includes the route's session token
and an exported `GH_TOKEN`. The file is on `tmpfs`, mode `0600`, owned by
root, so it does not reach host disk. It holds the values of the latest
connection until the next one or a stop. Only a shell that sshd started
reads it, so `brig sh` keeps the token it was given. A pane that does not
start `bash` has no `CLAUDE_CONFIG_DIR`, and a `claude` started there writes
its credential to the home share as above. See [herdr.md](herdr.md).

On `hvi`, the guest root is itself a share of a directory on the host. If
something on the host renames or replaces the directory under
`/brig/claude` there while the sandbox runs, the guest drops the `tmpfs`. A
credential the agent writes before Brig's next command then lands on host
disk until `brig rm`. Brig's next command mounts the `tmpfs` again.

### The aws route

The `aws` route hands the guest real AWS keys. Every other route keeps its
credential on the host. AWS signs each request with the secret key, so the
broker cannot add a credential on the way out. The route instead answers the
guest's AWS SDK with temporary keys, and the guest calls AWS itself. See
[The aws preset](policies.md#the-aws-preset).

**What the guest holds.** An access key id, a secret access key and a
session token. They work anywhere AWS answers, outside the sandbox too,
until they expire. In role mode they last 30 minutes, or 25 minutes to an
hour with `brig secret import --session-duration`. In self mode they expire
when the credentials of your AWS profile do. The broker fetches new keys
before the ones it holds run out. It refuses credentials with no expiry,
such as the long-lived keys of an IAM user.

The broker keeps the keys in memory and writes none of them to the secret
store or to a file. The audit line names the access key id it handed out,
and never the secret key or the session token.

**What the keys can do.** The keys can do what the identity behind them can
do. In role mode that is the role's policy, so give the sandbox a role with
a narrow policy. In self mode it is your whole identity in that profile: the
sandbox can do what you can. Self mode is never a default: `brig secret
import` refuses an import that names neither mode, or both. Each run and
exec prints a line that names the role, or says that the sandbox has your
own identity.

**Credentials that win over the route.** An AWS SDK reads its environment
and `~/.aws` before the container credential provider. A run with a route
that vends is refused when the profile or `BRIG_FORWARD_ENV` also hands the
guest `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`,
`AWS_PROFILE`, `AWS_DEFAULT_PROFILE`, `AWS_CONFIG_FILE`,
`AWS_SHARED_CREDENTIALS_FILE`, `AWS_WEB_IDENTITY_TOKEN_FILE`,
`AWS_ROLE_ARN`, or an `AWS_CONTAINER_*` variable. It is also refused when
`~/.aws` would be in the guest: a guest home that already holds
`.aws/credentials` or `.aws/config` (for example `--home ~`), or a profile
volume, `files:` binding or host config seed at `.aws`, or a seed copied
from your own `~/.aws`. The CLI's own cache in `~/.aws/cli`, which the
`aws` CLI in the guest writes itself, is allowed. Brig does not check the
image: an `AWS_*` variable baked into it wins over the route.

**Self mode.** The keys are the role credentials that the `aws` CLI caches
from your SSO login in `~/.aws/cli/cache`, the same ones your host uses, so
CloudTrail cannot tell the sandbox's calls from yours. They last as long as
the permission set's session, up to 12 hours. Revoking them revokes your own
host session too.

**Revocation.** `brig stop` revokes the sandbox's session tokens, so it can
fetch no more keys. A secret you import again with another role or mode
takes effect at the next fetch: the broker does not serve keys it fetched
for the old secret, and fetches again. Keys already handed out stay valid
until they expire. To cut them off sooner, change the role's policy or
revoke its sessions in AWS.

**The SSO session.** The route keeps working after your SSO login lapses,
for as long as the credentials the `aws` CLI cached from it last, up to 12
hours. When `aws` can get no credentials, a new run is refused with exit
`6`, and a request from a running guest that needs new keys gets a 502. Both
name the fix: `aws sso login --profile <profile>`, run on the host. The
guest keeps the keys it holds until they expire. To cut the route off
sooner, run `aws sso logout` on the host, run `brig stop`, or revoke the
sessions in AWS.

**The session token.** The guest fetches keys with its session token for the
route. Brig mints one on every exec, and it lasts 12 hours. An agent process
older than that cannot fetch keys again. The next exec gets a new token.

**The link to the broker.** The guest fetches keys in plain HTTP from
`169.254.170.2`, and not over TLS at `brig.internal`, because the AWS SDKs
accept no other form there. So the session token of the route and the keys
the broker answers with cross the virtual link between the VM and the host
in clear text. The token is bound to one sandbox, and only the broker
accepts it. The request never leaves the virtual link for a network. The
broker serves only a route that vends on that path, and refuses any other
route before it reads the token. See [Transports](broker.md#transports).

**Named, not ambient.** You name the AWS profile when you run
`brig secret import`. The secret holds that pointer and no key. Brig does
not read `~/.aws` or the AWS SSO cache itself. `brig-broker` runs the host's
`aws` CLI, which does, on every fetch. The broker uses the first `aws` on
its own `PATH`. The broker runs `aws` with every `AWS_` variable removed
from its environment, so a variable in your shell, such as `AWS_PROFILE` or
`AWS_ACCESS_KEY_ID`, never decides which identity a sandbox gets. The import
applies the same rule. That also drops `AWS_CONFIG_FILE`, so keep your AWS
configuration in its default place. No package on the run path links the
code that runs `aws`. A run still reads no host credential source. The
broker, a separate process, reads one through `aws`.

### The gcp route

The `gcp` route forwards requests to several Google APIs with one session
token. See [The gcp preset](policies.md#the-gcp-preset).

**What the guest holds.** The guest holds only the session token of the
route. The Google access token stays on the host, and the broker injects it
on the way out. The session token works only through the broker, for this
sandbox. The broker checks each request against the `request.allow` of the
service it names.

**What the route can do.** The reach is the IAM of the identity behind the
access token, narrowed by each service's `request.allow`. With a gcloud
pointer imported with `--impersonate-service-account`, that identity is the
service account, so its IAM roles cap what the sandbox can do. With
`--identity self`, or with a static access token that you store, it is your
own identity: the sandbox can do what you can, and Google's audit logs cannot
tell its calls from yours. Self mode is never a default: `brig secret import`
refuses a `gcloud` import that names neither mode, or both. A static token
expires, usually in an hour, and is not replaced.

**Minted on the host.** For a gcloud pointer, `brig-broker` runs the host's
`gcloud` CLI to mint the access token. It sends the token to Google's
tokeninfo endpoint from the host to learn its expiry. It holds minted
tokens in memory, per sandbox and route, and writes none of them to the
secret store, to a file, to a log or to a command line.
A token lives as long as `gcloud` makes it, about an hour, and the broker
mints a new one when it has less than 2 minutes left. A minted token is sent
only on a route with `services`, to its Google APIs. A pointer on any other
route is refused with `check=credential.gcponly`. See
[Minted tokens](broker.md#minted-tokens).

**Named, not ambient.** You name the gcloud configuration and the service
account when you run `brig secret import`. A self-mode pointer also names
the account of the login, and a token for any other account is refused. The
secret holds that pointer and no token. Brig does not read
`~/.config/gcloud` itself. `brig-broker` runs the host's `gcloud`, which
does, on every mint. The broker uses the first `gcloud` on its own `PATH`.
It runs `gcloud` without `GOOGLE_APPLICATION_CREDENTIALS`,
`CLOUDSDK_CORE_ACCOUNT`, `CLOUDSDK_ACTIVE_CONFIG_NAME` and every
`CLOUDSDK_AUTH_*` variable, so a variable in its environment does not decide
which identity a sandbox gets. The import applies the same rule. Neither
keeps `CLOUDSDK_CONFIG`, so both read the logins under
`$HOME/.config/gcloud`, and the import refuses while `CLOUDSDK_CONFIG` is
set. No package on the run path links the code that runs `gcloud`.

**Revocation.** `brig stop` revokes the sandbox's session tokens, so its
requests get no more Google tokens. A secret you import again with another
configuration, mode or service account takes effect at the next request.
The guest never holds a Google token, so nothing it holds outlives the
session token. A token the broker has minted stays valid at Google until it
expires. To cut a service account off sooner, remove its roles or the
`serviceAccountTokenCreator` grant in IAM.

**The gcloud login.** The route works while the login of the configuration
works. When `gcloud` can mint no token, a new run is refused with exit `6`,
and a request from a running guest gets a 502. Both name the fix:
`gcloud auth login --configuration <c>`, run on the host, with the
pointer's account named in self mode. An upstream 401 to a minted token
goes back to the guest, with no new mint and no retry. The broker's
`gcloud` runs with the broker's `PATH` and `HOME`, and with no other
variable of your shell, such as `CLOUDSDK_PYTHON`. A `gcloud` that needs one
of them works at import and can fail in the broker.

**What `request.allow` does not narrow.**

- `request.allow` matches methods and paths, and `*` also matches a `:verb`
  suffix. A service that allows `POST` has to list paths that name their verb
  exactly, such as `/v2/entries:list`. `methods: [GET, POST]` with
  `/v1/projects/*` also allows `POST /v1/projects/x:setIamPolicy`.
- The logging read names its project in the JSON body, so only the IAM of the
  credential narrows which project's logs are read.
- The broker does not read request bodies, so a method override in a
  form-encoded body is not refused.
- The broker serves HTTP/1.1 REST APIs. gRPC clients are not supported.
- The broker takes an escaped slash in one place: a Cloud Storage object
  name in the JSON API, `o/dir%2Ffile`, which that API reads as one name.
  Another Google API may read `%2F` as a separator after `request.allow`
  matched it as part of a segment, so the broker refuses it there. A name
  with a `.`, `..` or empty piece, such as `a/../b`, gets a 400. A name
  that ends in `/`, such as a folder placeholder, is a name.
- `request.allow` matches the path only. A rule that limits objects by a
  name prefix does not limit an upload, which names the object in the query
  or the body, a compose, which names its sources in the body, or a
  listing, which returns every name in the bucket.

**Refused outright.** The broker refuses the method-override headers
`X-HTTP-Method-Override`, `X-HTTP-Method` and `X-Method-Override`, the query
parameters `$httpMethod` and `httpMethod`, and batch requests. Each one
carries a request that `request.allow` does not see.

**Not yet done.** Brig does not refuse before boot a guest variable that
picks another identity, such as `GOOGLE_APPLICATION_CREDENTIALS`, or a guest
home that holds `.config/gcloud`. The guest writes the session token to the
file that `CLOUDSDK_AUTH_ACCESS_TOKEN_FILE` names itself. A
`CLOUDSDK_API_ENDPOINT_OVERRIDES_<SERVICE>` for a service the route does not
list is refused on another route's `guest.env`, but not when it comes in
through the variables you forward. Brig's own overrides win for the services
it lists. An override that points elsewhere sends gcloud a session token that
no other host takes. A run does not print which Google identity the sandbox
acts as, as the aws route does. `brig secret import` prints it. Brig does not
set a minted token's lifetime.

## The secret store

`brig secret` is the **only** store a run reads, and the one place where Brig
stores a value. A profile names the secrets it wants under `secrets:`. `brig
secret import` puts a host login in.

Prefer the store to a value you compose into Brig's environment. A value from
the store stays off the command line the host logs, whatever anyone sets
later. An ambient value does not (see [Not in argv](#not-in-argv)).

[Keeping secrets in your keyring](secrets.md) covers how to use the store.
[Secrets and env](profiles.md#secrets-and-env-for-a-credential-brig-resolves-itself)
covers the profile side.

On macOS the backend is the login keychain. Every item is a generic password
under the service `sh.brig.secret`, with the secret's name as the account:

```bash
printf %s "$TOKEN" | brig secret create gh-token
brig secret create deploy-key -f ~/.ssh/id_ed25519
brig secret ls
```

**The value never appears in argv.** A secret is two keychain items. The
key item is written through a pipe, so its key is not on any command line.
The sealed item is written through command arguments, which hold the
ciphertext and the secret's name.

**Each item has the default ACL.** `security` created these items, so
`security` can read them back with no keychain dialog. **Anything that can
run `/usr/bin/security` as you can read them back too.** That is the same
boundary as your own shell, and it is weaker than a per-application ACL.
Brig neither widens nor narrows the default.

**The keychain holds the key in one item and the sealed value in another.**
[Where a value lives](secrets.md#where-a-value-lives) has the layout and the
reason for it. It changes nothing in the threat model. Any process that
runs as you can read Brig's items, so it reads the key and opens the sealed
item. A copy of the keychain file without the login password holds
two encrypted items it cannot open. Keychain Access shows a base64 key and
base64 ciphertext, not the secret.

**`brig secret ls` never decrypts.** It reads attributes only, so a listing
raises no access prompt and shows names and dates but never values. The
read is wider than Brig's own items. The listing has no service filter,
so `ls` reads the attributes of every item in your keychain
*search list*. On a stock Mac that is your login keychain **and the System
keychain**. `security list-keychains` shows yours. Nothing is decrypted and
nothing leaves the process. Names and dates of other applications and of
the system do pass through Brig before it discards them.

**Brig writes only under its own service.** Brig creates, changes and
removes items only under `sh.brig.secret`. The service name is a label, not
an authenticity check. Another process that runs as you can add an item
under that service, and Brig then reads, updates and deletes that item as
its own. Two checks limit this. `read` reports a value that is not in Brig's
encoding, and `ls` skips a name outside Brig's grammar. So an item Brig did
not write is reported or skipped, and is not presented as yours.

**`brig secret import` reads another application's item and never writes
it.** Claude Code's own item is one example. That read raises a keychain
dialog, because `security` did not create the item. The dialog appears once,
when you run the import. A run never performs that read.

On Linux the store is a Secret Service keyring on your session bus,
gnome-keyring or KWallet (see [Linux](secrets.md#linux)). A host with no
keyring has no store. `brig secret` says which half is missing. It does not
fall back to a file.

## Writing into the workspace

Brig refuses a symlink in the guest home that it is about to write through.
The run fails before anything is written, and the error names the link and
its target. Remove the link, or point the guest home somewhere else.

The guest home is mounted read-write, so the sandbox chooses its contents.
Brig also writes into it from the host on every invocation, for example the
onboarding seed, the guest git files and the skills that `--skills` copies
in. So the guest home is the one place where the sandbox can influence what
happens on the host.

Brig runs as you and outside the sandbox. A guest can plant a symlink where
Brig writes next, aimed at a host path the guest itself cannot reach, such
as your `~/.ssh` or your shell profile. The microVM boundary does not stop
that write, because the write is on the host side of the boundary.

So every host-side read and write Brig makes inside the guest home goes
through an `os.Root` opened on it. There is no window between the check and
the open in which the guest can swap the file.

| Symlink | On a write | On a read |
| --- | --- | --- |
| Escapes the guest home | Refused | Refused |
| Stays *inside* the guest home | Refused. Brig writes only regular files where a state file belongs | Followed. A `.gitconfig` symlinked to your own dotfiles, inside the guest home, keeps working |
| At the guest home, or on the way to it | Refused before anything is created (see [The path to the guest home](#the-path-to-the-guest-home)) | Refused |

A fifo, socket, device or directory where a regular file belongs is refused
too.

A refusal looks like this:

```console
$ brig run claude
brig: refusing to write /Users/alex/.brig/homes/brig-claude-code/.claude/.claude.json:
it is a symlink to "/Users/alex/.ssh/authorized_keys", and brig writes only
regular files inside the workspace. The workspace is mounted read-write as the
sandbox's home, so that link was put there from inside the sandbox, to have
brig -- which runs as you, on the host -- reach a file the sandbox cannot.
Nothing was written; inspect
/Users/alex/.brig/homes/brig-claude-code/.claude/.claude.json and remove it before
running brig again: a symlink leads out of a directory brig is checking
```

Brig does not create the links it writes through, so a link in the way is
yours or the sandbox's. Do not retry the run with the link in place.

`--home` pointed at a symlink is refused for the same reason, with the same
kind of message, and is fixed by naming the real directory.

### The path to the guest home

Brig also checks the path to the guest home, before it creates anything.
The guest writes as you, so it can swap an entry only inside a directory you
can write. Brig splits the path at the first such directory.

| Part of the path | What Brig does with a link |
| --- | --- |
| Above the split | Follows it. The guest cannot reach these entries, and links such as `/tmp` and `/var` on macOS come from the system or an administrator |
| From the split down | Refuses it |

The guest home is always in the lower part, so a link there is refused
wherever it is. A directory counts as one you can write if your user, any of
your groups or an ACL can write it, or if you own it.

A link above the split whose target passes through a directory you can write
is refused, like any other link the guest can reach. A root-owned `/data`
that points into your home is one example. Name the real directory instead.

<details>
<summary>When Brig runs as root</summary>

The guest's writes are root's too, so ownership tells Brig nothing. Brig
then trusts only the entries of `/`, which it never mounts. The system's own
links there, such as `/tmp` on macOS or `/home` on an ostree system, still
resolve. Below them, a link on the way to the guest home or the project is
refused.

</details>

### Deleting a guest home

`brig rm` deletes a guest home Brig created. It never deletes a guest home
you named with `--home` or `BRIG_WORKSPACE`, wherever that home is.

The tree that `brig rm` deletes is the guest's, so the delete has limits:

- It takes only a direct child of `~/.brig/homes`.
- Nothing it does can resolve outside that directory.
- It removes a symlink as a link and never descends into it, so the target
  of a symlink the guest left stays untouched.

A first run whose boot fails deletes the home it created the same way.

## Mounting a project

Name a project on the run line and Brig mounts it read-write at
`/work/<name>`. The sandbox can replace every component at or below it.

Brig refuses a project path that goes through a symlink. The run fails before
anything is mounted, and the error names the link and its target. Name the
real directory instead:

```console
$ brig run claude ~/lab/monorepo/frontend
brig: refusing to use /Users/alex/lab/monorepo/frontend as this run's
project: it is a symlink to "/Users/alex/escape-target", and the project is
mounted read-write into the sandbox, so brig will not hand a guest a
directory reached through a link. Name the real directory instead: a symlink
leads out of a directory brig is checking
```

For a link further up the path, the error names the component that is a link,
not the directory you typed:

```console
$ brig run claude ~/lab/monorepo/frontend/src
brig: refusing to use /Users/alex/lab/monorepo/frontend/src as this run's
project: /Users/alex/lab/monorepo/frontend on the way to it is a symlink to
"/Users/alex/escape-target", so the sandbox would be handed a directory other
than the one you named. Name the real directory instead: a symlink leads out
of a directory brig is checking
```

A link you made yourself is refused too. Brig cannot tell it from a planted
link.

The `PROJECT` row in the run envelope reports the directory the path resolved
to, not the path as typed. On macOS a project under `/tmp` prints as
`/private/tmp`, the directory the sandbox gets.

### Why a link is refused

The host resolves a share path, outside the microVM, so a planted link
exports the directory it points at.

The risk needs a second sandbox with write access to the path, such as the
sandbox of an earlier run, or one from another session that still runs with
an overlapping project. An agent there replaces a subdirectory with a link
to another place on the host. You later narrow a run to that subdirectory,
and the new sandbox gets the directory the agent chose.

So Brig checks the project path the same way as the path to the guest home
(see [The path to the guest home](#the-path-to-the-guest-home)). The boot
checks again and refuses a path that no longer names the directory it holds.

A hostmount volume source is checked the same way, when the run begins and
again on a restart. A source swapped for a link is refused and is not
exported.

### The handover gap

> [!WARNING]
> The path walk does not close the handover. The share is a string that the
> VMM resolves later, on its own time.

A sandbox from another session that still runs with an overlapping project
can swap a component in that window. One example is `~/monorepo` while this
run names `~/monorepo/frontend`. The guest home has the same gap, and so does
a hostmount volume source. To close the gap, the runtime must accept a
directory handle and not a path.

### What the host runs from the project

Brig mounts the whole project, `.git` included. Git on the host runs
programs that the repository names, so a file the agent writes into `.git`
runs on the host, as you, at your next git command in that directory:

- a hook in `.git/hooks`, for example `post-checkout` at `git checkout .`
- a program that `.git/config` names, for example `core.fsmonitor` at
  `git status` and `git diff`. `core.hooksPath`, `core.pager`, filter, diff
  and merge drivers, and aliases that start with `!` work the same way.

`core.fsmonitor` runs before you have read anything: at the first
`git diff`, or when an editor or a shell prompt runs `git status` in that
directory.

Reading the diff does not catch either one. `.git` is not part of the working
tree, so neither `git diff` nor `git status` shows a new hook or a changed
config. Git's `safe.directory` check does not apply, because you still own
the repository. Your global git config does not help, because the
repository's config overrides it.

Files in the working tree can run on the host later too: CI workflows,
editor task files and package scripts. Those show in the diff, but they run
when a tool runs them, not when you read them.

After a run, before any other git command in the project on the host, do
one of these:

- Read the repository's config and hooks with commands that run nothing:
  `git config --local --list` and `ls -la .git/hooks`.
- Continue from a fresh clone, and leave the directory the agent worked in.

Brig warns when these files change. `brig run`, `sh`, `exec`, `stop` and
`rm` record what the project's `.git/config`, `.git/config.worktree`,
`.git/commondir`, `.git/hooks` and `.git/info` hold. The next of them for
the same sandbox names each file added, changed or removed since:

```console
$ brig stop claude
brig: /Users/alex/code/demo/.git changed while brig-claude-code could write to it
  ↳ git on the host runs these files as you, at your next git command there
  ↳ added .git/hooks/post-checkout
  → read them with commands that run nothing:  git config --local --list; ls -la .git/hooks
                                               ↳ or continue from a fresh clone
```

When stderr is not a terminal, every line starts with `brig:`.

A `.git/commondir` is named on every command that finds one. git then reads
config, hooks and info from the directory it names, which Brig does not look
at.

When `.git/config` sets `core.hooksPath` to a directory inside the project,
Brig looks at that directory too. husky does this with `.husky/_`, and ignores
everything in it, so `git status` does not show a hook changed there either.

The warning is a look between two brig commands, not a guard:

- A foreground run hands the terminal to the agent, so Brig is not there
  when it ends. Nothing is said until the next brig command for that
  sandbox, and a git command you run before that runs whatever is there.
- It does not stop the write, and the guest can write again after the look.
- It names a change, not who made it. A hook manager or a `git push -u` on
  the host changes the same files.
- `-q` and `--json` hide it, and the next command that prints warnings
  names the change instead.
- Submodules keep their hooks and config under `.git/modules`, which is not
  looked at.
- A file that `.git/config` already includes, with `include.path` or
  `includeIf`, is not looked at unless it is one of the paths above. The
  guest can set `core.fsmonitor` or `core.hooksPath` there without
  `.git/config` changing.
- The first look at a sandbox has nothing to compare against: the first
  command for it, and the first after an upgrade from a Brig that did not
  look. Whatever the guest had planted by then is recorded with no warning.
- A hook that is a symlink is recorded by where it points, not by what that
  file holds. After the first warning, the guest can change the file it
  points to, an ignored one in the working tree for instance, and no second
  warning comes.
- The look hashes the first 8 MiB of a file and 64 MiB in all. A change
  past a file's first 8 MiB that keeps its size is not named. A file reached
  after the 64 MiB is recorded by its mode and size only, so a change that
  keeps both is not named. The look stops after 4096 entries and 32
  levels of directories, and what is past either is not looked at. A look
  that stopped says so in the warning.
- A `.git` that is a file, whose `gitdir:` names a directory inside the
  project (`git init --separate-git-dir`), is watched as the file only. The
  config and hooks in that directory are not looked at.
- A symlinked `.git/hooks`, or a symlinked `core.hooksPath` directory, is
  recorded by where it points. Edits to the directory it points to are not
  seen, whether or not it is inside the project.
- An entry under `.git/hooks` or `.git/info` that Brig cannot read, a
  directory the guest made mode 000 for instance, stops the look with an
  error. Until it is gone, that command names no change at all. A
  `core.hooksPath` that cannot be read does not do this: it is recorded as
  unreadable and the look goes on.
- Reading `core.hooksPath` needs git 2.18 or newer. With an older git, or
  any other git error, the directory is not watched, and nothing says so. A
  `core.hooksPath` set in `.git/config.worktree` is not read. An absolute
  one that reaches the project by another name than the one Brig holds,
  `/private/var` for `/var` on macOS, is not recognized as inside it and is
  not watched.

## Guest images

An image is code that runs with your credentials, so Brig checks where it
came from before it boots it. The check is cosign's keyless verification. It
asks whether that workflow, in that repo, built the image. You can run the
same check:

```bash
cosign verify \
  --certificate-identity-regexp \
    '^https://github\.com/brig-sh/community-images/\.github/workflows/build-images\.yml@refs/heads/main$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/brig-sh/claude-code-stock:root
```

The command needs no login. `:root` is a multi-arch index. The
per-architecture tags (`:arm64`, `:amd64`) and the immutable `:<arch>-<sha>`
pins are signed the same way and verify with the same command.

The identity is anchored on the repository and the workflow file. A
signature from anywhere else fails, including one from another workflow in
the same repository.

### Verification modes

`BRIG_VERIFY` sets the mode.

| Mode | Behaviour |
| --- | --- |
| `warn` (the default) | Reports what it found, then boots. It stops for an image under `ghcr.io/brig-sh/` that fails the check or cannot be checked, and for boot assets whose signature fails. The next table lists each case |
| `require` | Refuses anything it cannot positively verify, third-party images included |
| `off` | Skips the check |

A typo in `BRIG_VERIFY` refuses the run and names the three values.

What `warn` does with the answer:

| situation | behaviour |
| --- | --- |
| image under `ghcr.io/brig-sh/`, signature verifies | one line saying so, boots |
| image published by somebody else | warning, boots |
| cosign not installed | warning, boots |
| image under `ghcr.io/brig-sh/`, signature fails | stops and asks `[y/N]`. Refuses when there is no terminal |
| image under `ghcr.io/brig-sh/`, registry cannot be reached or cosign does not answer | stops and asks, as above. See [Digest pinning](#digest-pinning) |
| boot assets, signature fails | refuses, with no question |

Bring-your-own images are a supported way to use Brig, so `warn` boots them.
An image under our registry whose signature does not verify stops the boot,
under `warn` too. An image of ours that Brig cannot check, because the
registry or cosign does not answer, stops it the same way.

`claude-desktop` points at a `ghcr.io/nofireai/` image, and `ubuntu` at
`docker.io/library/ubuntu`. Brig has no signing policy for either registry, so
both warn on every boot until those images move.

If you publish signed images yourself, point `BRIG_VERIFY_REGISTRY`,
`BRIG_VERIFY_IDENTITY` and `BRIG_VERIFY_ISSUER` at your own registry and
workflow.

### Digest pinning

Brig boots the bytes that cosign checked. Before the check, Brig resolves
the tag to the digest the registry serves, verifies that digest and boots
it. The object cosign checked is the object that runs, and the success
line names the digest instead of the tag it came from.

| Case | Under `warn` | Under `require` |
| --- | --- | --- |
| The local store holds a different digest under the tag | Stops and asks, like the signature-failure row. A yes boots the verified digest, not the copy on disk | Refuses |
| The registry cannot be reached | Stops in the same way, because the image was not checked | Refuses |

Every cosign call is bounded, so an outage fails in seconds and does not hang
the boot.

Pinning is for images under `ghcr.io/brig-sh/`. An image Brig did not publish
carries no signature of ours, so Brig makes no cosign call for it. It boots
by tag, with one line that says whose image it is.

Pinning works on Linux, and on macOS with a current hull (see
[Every command Brig runs](runtimes.md#every-command-brig-runs)).

With an older hull, Brig verifies and boots the tag, and prints one line
that says so. The gap remains until you upgrade hull:

- Under the default `missing` pull policy, cosign checks the tag in the
  registry. That is not necessarily the copy hull already holds.
  `BRIG_PULL=always` is the workaround.
- Brig does not name a digest on a boot that did not pin one.

Two things are narrower on macOS than on Linux:

- After a hull upgrade, the first pinned boot of an image pulled under the
  older hull pulls it once more. Under `BRIG_PULL=never` it fails until the
  image is pulled again.
- The report that the local copy differs from what the registry serves is
  Linux-only for now. The boot is pinned on both platforms.

### The kernel, not only the image

Seven of the nine shipped profiles boot a kernel and an initrd that Brig
downloads: `claude-code`, `codex`, `gemini`, `grok`, `herdr`, `opencode` and
`ubuntu`. Brig checks that bundle too, under the same
[`BRIG_VERIFY` mode](#verification-modes), as a second check with its own
trust root.

`cursor` and `claude-desktop` boot the kernel in their own image, so nothing
in this section applies to them. `cursor` has no published image today.
`brig agent ls` marks it `(no published image)`, and a run of it fails before
any of these checks.

| Trust root of the bundle | Value |
| --- | --- |
| Registry prefix | `ghcr.io/nofireai/` |
| Signing identity | The `build-assets.yml` workflow in `NOFireAI/hull-assets` |
| Issuer | The same issuer as the image check |

The kernel's identity is fixed. `BRIG_VERIFY_REGISTRY`, `BRIG_VERIFY_IDENTITY`
and `BRIG_VERIFY_ISSUER` repoint the image's trust policy only.

A signature that fails stops the boot, in every mode except `off`, with no
`[y/N]` prompt.

The signature covers the bundle's manifest, which lists a sha256 for each
file. Before the boot, Brig reads the manifest by the verified digest and
compares the kernel and initrd it boots with that list.
`brig: image and boot assets verified` appears only after both files match.

Brig refuses a registry answer that is not that digest, an index, a
redirect loop, or a token realm or redirect over plain http.

On macOS, when Brig cannot read the manifest, it reads hull's
`provenance.json` in the asset directory instead. It accepts only a record
that names the verified digest. Anything that can rewrite the kernel there
can rewrite the record to match. So files that differ from the record still
count as a difference, and files that match it are "cannot check", never
verified. A registry answer Brig refused gets no fallback.

What a difference does depends on who chose the directory.

**`BRIG_BOOT_ASSETS` unset.** Brig chose the directory and fetches into it,
by the digest whose signature verified.

- Files of an older bundle, fetched before the tag moved, are replaced
  with the verified bundle.
- Any other file that differs refuses the run under `warn` and `require`, and
  nothing is fetched over it. The refusal names the file, its digest and the
  digest the bundle lists. To fetch the bundle again, delete the two files.
- hull's store is this case, wherever `HULL_BOOT_ASSETS` puts it.

**`BRIG_BOOT_ASSETS` set.** The directory is someone's build.

- `warn` states the difference and boots it, and nothing vouches for that
  kernel.
- `require` refuses.
- A [Linux runtime bundle](#the-linux-runtime-bundle) from before its signed
  record is this case. Under `require` its directory refuses until
  `install.sh` installs a newer bundle.
- A named copy of hull's directory with a changed file still fails under
  `warn`, because hull checks it too.

**Digests Brig cannot check.** `warn` states it and boots. `require` refuses.
The cases are:

- no manifest and no record of the verified bundle
- a registry answer Brig refused
- a record with no entry for one of the files
- files that match only hull's record

`BRIG_VERIFY=off` skips the signature and the digest checks, and one line
says so. A `BRIG_BOOT_ASSETS_REF` under `ghcr.io/nofireai/` is checked the
same way against its own digest. Any other reference has no signature of ours,
so there is no digest to bind.

#### The Linux runtime bundle

The Linux runtime bundle ships its own kernel and initrd, and its signed
release lists their checksums in a `SHA256SUMS`. On Linux, when the
directory `BRIG_BOOT_ASSETS` names holds a `SHA256SUMS`, Brig checks that
record instead of the boot bundle's signature:

1. The kernel and initrd must hash to what `SHA256SUMS` lists.
2. The release's `checksums.txt` must list that `SHA256SUMS`.
3. cosign checks the signature on `checksums.txt` against the release
   workflow of `NOFireAI/brig-standalone-linux`, on a tag, with the same
   issuer as the image check. `BRIG_VERIFY_RUNTIME_IDENTITY` and
   `BRIG_VERIFY_RUNTIME_ISSUER` point it at a fork's release workflow
   instead.

| Result | `warn` | `require` |
| --- | --- | --- |
| A file the record does not list, a record the release does not list, or a signature that does not verify | Refuses | Refuses |
| Files match a record Brig cannot check (no cosign, no signature files beside it, or no answer from Sigstore) | States it and boots | Refuses |

The bundle chose those files, so the first row refuses under both modes. To
put the files back, reinstall the bundle with `install.sh`.

The first two checks need no network, so a changed file refuses even where
the signature cannot be checked.

Only a nerdctl run reads the record. On macOS a `SHA256SUMS` in a named
directory is ignored.

#### After the check

The comparison happens before the runtime starts. The runtime opens the
files later, after the image pull, which can take minutes the first time.
Something that can write to the asset directory in that time
can still swap a file:

- the host's own user
- a sandbox, only when it mounts that directory. A user install keeps its
  asset directory under `$HOME`, and `brig run claude ~` shares `$HOME`
  read-write.

On macOS hull checks each file against its `provenance.json` again, so a
swap has to rewrite the record too. On Linux nothing checks the files
again after Brig does.

## Brig's own binaries

Releases are signed with keyless cosign. The certificate is short-lived,
bound to the release workflow's identity, and recorded in a public
transparency log.
To verify a release yourself:

```bash
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature checksums.txt.sig \
  --certificate-identity-regexp \
    '^https://github\.com/brig-sh/brig/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

shasum -a 256 -c checksums.txt --ignore-missing
```

The first command vouches for the checksum file, and the second ties every
archive to it. Each archive also ships an SPDX SBOM.

The macOS binaries are also signed with a Developer ID certificate and
notarized with Apple. Nothing strips the quarantine
attribute from the files Homebrew installed. Gatekeeper accepts them because
they are notarized.

```bash
spctl -a -vv -t install "$(which brig)"  # source=Notarized Developer ID
xattr -l "$(which brig)"                 # com.apple.quarantine, still there
```

A from-source build is neither signed nor notarized. You trust it because you
built it.

## Telemetry

`brig telemetry off` (or `DO_NOT_TRACK=1`) turns telemetry off everywhere it
runs. Brig sends anonymous usage events and crash reports to NOFire AI, on
macOS and Linux. On macOS, hull also reports the boot, lifetime and resource
use of the sandboxes Brig runs. [Telemetry](telemetry.md) shows how to check
the current state, and lists what an event carries field by field.

Telemetry must not cross the boundary that Brig protects. Brig's own events
carry the fields [Telemetry](telemetry.md) lists. The events hull sends
follow
[hull's stated commitment](https://github.com/brig-sh/hull/blob/main/docs/telemetry.md),
which this repository does not verify. Together they:

- exclude host paths, repository names, command arguments and agent prompts
- exclude secret names and values, image references, network destinations
  and file metadata
- send the name of an agent profile of your own only as a salted hash, which
  anyone who guesses the name can compute too
- drop IP addresses at ingestion and keep raw events for a year

To check that yourself:

- `HULL_TELEMETRY_DEBUG=1` prints payloads to stderr and does not send them,
  for Brig's events and hull's alike.
- The install identifier is a random value stored in
  `~/.hull/telemetry.json`. Delete the file to rotate the identifier.
- Crash reports queue in `~/.hull/crashes/`, and events not yet sent in
  `~/.hull/outbox/`. You can read or delete them there before they are
  uploaded.

If you find an excluded item in a payload, that is a bug. Report it as
[SECURITY.md](../SECURITY.md) describes.

## Things brig does not claim

### Network egress

To restrict egress, attach a policy on `hvi` or on Linux with nerdctl.
`--network offline` is the one posture with no route out at all, on every
backend.

Brig does not sandbox the agent from the network by default, on any backend.
A sandbox with no policy has unrestricted egress, and that is what
`brig run <agent>` gets on a fresh install.

| Backend | Where a policy is enforced |
| --- | --- |
| `hvi` | At the network gateway Brig gives that sandbox |
| Linux with nerdctl | In nftables on the sandbox's own bridge, with a resolver of Brig's that answers the guest's DNS |
| `vz`, `qemu`, docker | No policy is available. Outbound traffic is whatever the runtime allows |

Both enforcement points are outside the guest's kernel.

Every runtime Brig ships refuses to boot a policy it cannot enforce, and does
not boot that sandbox unconstrained (see
[Networking and egress policy](policies.md)). That refusal is each adapter's
own choice, not a guarantee Brig imposes on every runtime it will ever
drive. The one exception is `--network offline`: a sandbox with a policy and
no route out satisfies every rule set, so no backend refuses it.

On nerdctl the nftables table is the sandbox's only filter. It outlives
Brig's resolver:

- If the resolver dies, the table refuses every new connection.
- If something removes the table, the sandbox is unfiltered until the
  resolver installs the table again, within 5 seconds. If the resolver is
  gone too, the sandbox is unfiltered until the next boot.
- A table installed again has lost the addresses the guest looked up. It
  refuses them until the guest asks again.

### Host services

The one control is an egress policy on `hvi` or on nerdctl, with
`default: deny` and no `cidr` allow for the host's own ranges.

Brig does not say whether the guest can reach services bound on the host
itself, such as a dev server, a local model, an MCP server or a metadata
endpoint. With no policy bound, Brig adds nothing to narrow that. What the
guest's network reaches is the runtime's default. Whether that includes the
host is unmeasured on every backend.

On nerdctl a policy of either default also refuses the addresses of the
network namespace that holds the sandbox's bridge. DNS and ping to the bridge
address are the exception, and so is port 443 of the bridge address on a
run with a route.

| nerdctl | Whose namespace | Effect |
| --- | --- | --- |
| Rootful | The host's | The policy covers every address of the host |
| Rootless | rootlesskit's | The rules judge the host's own addresses like any other |

There is no equivalent on `vz`, `qemu` or docker.

A run with a route reaches one host service by design: the credential
broker, at `brig.internal` on port 443. A run with a route that vends also
reaches the broker at `169.254.170.2` on port 80. Brig adds no `allow`
rule for it. The rest of `169.254.0.0/16` is refused on `hvi`, and on
nerdctl under a policy with a default. A connection to `169.254.170.2`
reaches only the broker. See [Transports](broker.md#transports).

### Sandbox to sandbox

If two agents must not reach each other, run both with `--network isolated`
on `hvi` or on Linux. On `vz` and on `qemu`, where that posture is refused,
run them on separate hosts.

`isolated` is the guarantee. It is the default for new `hvi` and Linux
sandboxes unless another posture is named (see
[Network posture](#network-posture)). `--network isolated` also moves an
existing shared sandbox onto it.

`isolated` gives the sandbox a network of its own. No other sandbox is on
that network, whatever the backend does with a shared one. A sandbox that
carries an egress policy is isolated whether or not it asked to be.

`isolated` is **refused** on `vz` and on `qemu`, where Brig owns no network to
give. A run that asks for it there is stopped and told which backend
implements it. It is never booted onto the shared network under a row that
claims otherwise.

The guarantee is about reachability, not resources. An isolated sandbox
costs one more network on the host. `brig rm --all`
prunes them.

Brig does not promise that one sandbox cannot reach another under a `shared`
network. A sandbox gets `shared` in these cases:

- A flag, a setting or a profile chooses it.
- An older sandbox keeps it, if it was recorded or recovered from the
  runtime.
- `vz` and `qemu` use it when no posture is named.

Whether the shared network forwards traffic from one guest to another is the
runtime's behaviour, not Brig's. The measurements are in
[Manual test: can one sandbox reach another?](manual-tests/sandbox-reachability.md).

| backend | can one sandbox reach another? |
| --- | --- |
| `hvi` on macOS | **yes.** Measured on 2026-09-27 with brig v0.3.0 ([#364](https://github.com/brig-sh/brig/issues/364)): one sandbox fetched a file over HTTP that only the other served. With `--network isolated` on both, the same request timed out. An earlier hull release gave "no" on the shared network. What changed the answer is not known |
| `vz` on macOS | not measured on a current hull |
| `qemu` on macOS | not measured. It takes its network from vmnet, as `vz` does |
| Linux, nerdctl/urunc microVMs | **yes** on `shared`. On 2026-09-30, one microVM fetched the other's HTTP marker. On separate isolated networks, the same request timed out while connection controls passed. The [ARM run](manual-tests/sandbox-reachability.md#2026-09-30-linux-arm64-under-qemu) used documented console and runtime setup workarounds. The [amd64 run](manual-tests/sandbox-reachability.md#2026-10-02-linux-amd64-with-the-stock-runtime) on 2026-10-02 gave the same results with the shipped runtime, unmodified |

So on Linux and on `hvi`, two agents on `shared` that you gave *different*
credentials can each reach whatever the other is listening on. That is a real
hole in the [narrow blast radius](#what-is-still-exposed), which is narrow
per guest home and per token, not per sandbox.

The `hvi` answer changed between two measurements, and nothing in Brig
noticed at the time. Do not treat any answer about `shared` in the table as a
property of Brig.

### Terminal output

What the agent writes to your terminal can do more than draw. A few escape
sequences make the terminal answer, and the answer lands on standard input.
An agent that read a hostile README can try any of these:

- An OSC 52 query reads the system clipboard. An OSC 52 write sets it.
- `tmux` and `screen` forward DCS sequences verbatim to the *outer* terminal.
- A cursor-position query makes the terminal type its reply onto your shell's
  standard input.

Brig itself filters nothing. Whether the bytes are filtered depends on the
runtime that gets the tty.

| Command | Filters control sequences? |
| --- | --- |
| `brig run`, `brig sh` on macOS | Yes. hull filters what reaches your terminal |
| `brig run`, `brig sh` on Linux | No. Every byte the agent emits reaches your terminal emulator unexamined |
| `brig run` with `--json` | As without it. Brig reads nothing the agent prints and writes nothing to it |
| `hull exec`, `hull logs` | Yes. hull stays in the middle of that stream |
| `brig logs` | Yes, by default. It reads a log back and does not drive a terminal. `--raw` turns the filter off and hands you the bytes with that surface intact |

hull's filter keeps a terminal agent working. Color, cursor movement, mouse
tracking, bracketed paste and the alternate screen pass through. It drops
the sequences that ask the terminal for a reply or reach past the display:
OSC 52 queries, OSC 1337, palette queries, device and status reports,
window reports, and DCS, SOS, PM and APC strings. It filters only when the
output is a terminal. A pipe or a file gets the bytes as they are.

Two things still pass on macOS:

- **OSC 52 writes.** That is how vim, tmux and agents copy, and blocking it
  broke copy and paste. A guest can set your clipboard, and what it set
  reaches whatever you paste into next.
- **C1 controls spelled as UTF-8.** hull drops them as raw bytes, but not
  yet in their two-byte UTF-8 form
  ([brig-sh/hull#17](https://github.com/brig-sh/hull/issues/17)).

`HULL_TERMINAL_FILTER=off` turns hull's filter off and gives the guest your
terminal. Use it when the filter gets in the way, and do not leave it set.

On Linux, or with the filter off, run Brig inside a terminal you are willing
to lose if terminal output matters for your threat model.

### Files the host runs later

Brig does not stop the agent from writing a file that the host runs later.
The project is mounted read-write, `.git` included, so a hook or a program
named in `.git/config` runs on the host at your next git command there. Brig
names a change to those files at the next brig command for the sandbox, and
a git command run before that is not warned about. See
[What the host runs from the project](#what-the-host-runs-from-the-project).

### Diagnostic reports

`brig report` writes a file that you can attach to a public issue. It masks
credentials by their shape: Anthropic keys, GitHub tokens, Authorization,
Bearer and Basic values, JWTs, Brig's own session tokens, secret fields in
JSON, also when the JSON is escaped inside a string, `NAME=value` where the
name ends in TOKEN, KEY, SECRET or PASSWORD in any case, and AWS key IDs. It
does not read the secret store, so it cannot mask a value by what the value
is. A credential of another shape that a log or the guest printed stays in
the file. `brig report` lists the lines that hold a string that looks random
and was not masked, but it cannot tell a credential from other random text.
Read the report before you post it.

The file is 0600, in `~/.brig/host/reports` unless `-o` names another
place. No sandbox mounts that directory. `brig report` creates the file new
each time, so it refuses a file or a link that is already at that name.

### The guest home

Brig does not protect the guest home from the agent. Everything in there is
writable, because that is where the agent works.

### Spending

Brig does not stop an agent from spending your money. The `deny` list keeps a
metered key from being forwarded by accident, which is a much smaller
promise.

## Trust assumptions

Brig narrows what an agent can reach. You must still trust five things.

**The guest image, and whoever publishes it.** An image under
`ghcr.io/brig-sh/` is checked against a specific build workflow. An image Brig
did not publish boots on a warning under the default mode. `BRIG_VERIFY=off`
turns the check off for either kind (see
[Verification modes](#verification-modes)). In each case, the image is code
that runs with your credentials.

**The profile author.** A profile names the image, the volumes it hostmounts,
and any `files:` binding, the one channel the `deny` list does not cover. A
profile is only as careful as its author.

**hull or nerdctl.** The kernel boundary, whether the shared network
forwards traffic between guests, and every device and namespace decision
belong to the runtime, not to Brig. Brig reports what it resolved. It
neither hardens nor weakens what the runtime does.

**Brig itself.** Brig runs as you, on the host, outside the sandbox. A
resolved credential is in its process memory as plaintext for the run's
lifetime. Brig clears that memory on the way out, as defence in depth, not a
control. Brig does not claim to protect against a process that already
holds your credentials in memory.

**brig-broker.** The broker runs as you, on the host. It holds the private
key of a route's client certificate in its process memory, parsed, while a
registered route names the secret, and drops it when the last such route
goes. A process that can read the broker's memory can read the key.
