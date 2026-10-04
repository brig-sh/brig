# brigd

`brigd` is an optional daemon that gives other programs one socket to drive
Brig through. It keeps the session inventory, and it owns boot and teardown
when several callers want the same sandbox. It uses the same `internal/wrap`
library as the CLI, so both build a sandbox the same way.

## When to run it

Run brigd when another program needs one socket to drive Brig through:

- a tool that drives several sandboxes from one process
- a client that needs boots of one sandbox serialized across several callers

Most people never run it. Every `brig` command talks to the runtime
directly, whether brigd runs or not.

brigd does not proxy exec. It owns the lifecycle of a sandbox, and the CLI
owns the terminal. To hand your terminal to a process inside the guest, use
`brig sh`, which passes the file descriptors when it replaces itself with
the runtime.

## Run it

```bash
brigd                                  # $XDG_RUNTIME_DIR/brigd.sock if set, else ~/.brig/brigd.sock
brigd --socket /tmp/brigd.sock
```

brigd creates the socket with mode 0600. The socket carries lifecycle
control over sandboxes that hold live credentials, so it belongs to the
invoking user alone.

One daemon serves one socket path. A second daemon started on a served path
exits non-zero and names the process that holds the path. It does not take
the path over. The lock is a `brigd.sock.lock` file beside the socket. The
daemon holds the lock while it runs, and the kernel releases the lock if
the daemon dies.

The kernel limits the length of a unix socket path: 103 bytes on macOS and
107 bytes on Linux. brigd refuses a longer path before the bind. The refusal
names the limit, the length, and where the path came from. The kernel's own
error for a path that is too long is `bind: invalid argument`, which names
none of those.

### Check it with `brig doctor`

`brig doctor` is the only `brig` command that talks to brigd. It asks a
running brigd for its build.

| Mark | Meaning |
| --- | --- |
| `--` | brigd is not running. This mark means that something is absent and that it is not a problem |
| `!!` | the mode of the socket is not `0600`, or the running brigd is a different build from `brig` |

```
  --  brigd     not running (no socket at /Users/me/.brig/brigd.sock)
```

## Protocol

The protocol is line-delimited JSON: one request per line, one response per
line.

```json
{"v":1,"op":"ensure","agent":"claude-code","name":"refactor"}
{"v":1,"op":"status"}
{"v":1,"op":"stop","agent":"claude-code","name":"refactor"}
{"v":1,"op":"version"}
```

`agent` is a profile name or alias. `name` is the optional session name and
follows the same rules as `brig --name`.

A response looks like:

```json
{"v":1,"ok":true,"code":0,"sessions":[{"agent":"claude-code","name":"refactor",
  "sandbox":"brig-claude-code-refactor","workspace":"/Users/me/.brig/homes/brig-claude-code-refactor",
  "running":true}]}
```

brigd reports an error as `{"v":1,"ok":false,"code":...,"error":"..."}`, and
not as a closed connection.

`version` answers with the build the daemon came from. The fields are the
same ones that `brig version --json` prints for the CLI:

```json
{"v":1,"ok":true,"code":0,"version":"v0.2.0",
  "commit":"131e3bc5615df5ff74e6b5af9a5bcf2ed42b1d57","commitTime":"2026-09-15T09:36:19Z",
  "modified":false}
```

`commit` and `commitTime` are absent when the build carried no git history.
`modified` is always present, and it is `true` when the tree had uncommitted
changes.

An example with `socat`:

```bash
echo '{"op":"ensure","agent":"claude"}' | socat - UNIX-CONNECT:$HOME/.brig/brigd.sock
```

### Protocol version

Every request and every response carries `v`, the protocol version. It is `1`
today. The response always carries `v`, so a client can tell what it talks
to from the answer.

| Request | What brigd does |
| --- | --- |
| no `v` | reads the request as version 1 |
| a `v` that brigd does not know | refuses with `code` 2 and an error that names the versions it speaks |

### Request id

A request can carry `id`, which is any string. The response returns the same
`id` unchanged, so a client that pipelines several requests on one
connection can match each answer to its question. A request without `id`
gets a response without `id`.

```json
{"v":1,"id":"boot-42","op":"ensure","agent":"claude-code"}
{"v":1,"id":"boot-42","ok":true,"code":0,"sessions":[...]}
```

### Exit code

Beside `error`, a response carries `code`. brigd uses the same codes and the
same causes as `brig`, so a script can branch on them the same way. See
[Exit codes](cli.md#exit-codes).

```json
{"v":1,"op":"ensure","agent":"no-such-profile"}
{"v":1,"ok":false,"code":3,"error":"unknown agent \"no-such-profile\""}
```

### Who can connect

Only the user who started brigd can connect. The socket is `0600`, and brigd
also checks each accepted connection. It reads the uid of the peer from the
kernel (`SO_PEERCRED` on Linux, `LOCAL_PEERCRED` on macOS), which the peer
cannot forge. It refuses a uid other than its own with one line, and then it
closes the connection:

```json
{"v":1,"ok":false,"code":1,"error":"connection from uid 1001 refused: this socket serves only its owner, uid 1000"}
```

### Running state

brigd reads `running` from the runtime on every report, because anything can
stop a sandbox, including a `brig stop` that did not go through the daemon.
A sandbox that something else stopped is reported as stopped.

If brigd cannot ask the runtime, the session carries `runningError`, and
`running` means nothing. This occurs when the binary of the runtime is gone,
when a permission error came back, or when containerd is down.

```json
{"agent":"claude-code","sandbox":"brig-claude-code","workspace":"/Users/me/.brig/homes/brig-claude-code",
  "running":false,"runningError":"nerdctl ps: exit status 1: cannot connect to containerd"}
```

In a client, show this session as "cannot tell". Do not show it as a stopped
sandbox.

### Warnings

The response carries what the run warns about as `warnings`, one line each.
The CLI prints the same lines on your terminal. Examples:

- a credential that was not forwarded, and why
- a secret about to expire
- an image that did not verify

`warnings` holds only things to act on. Progress lines, such as the line
that says a boot started, go to the stderr of brigd. An `ensure` with
nothing to report comes back with no `warnings`.

### Limits

| Limit | Value | What brigd does at the limit |
| --- | --- | --- |
| Silence on a connection | five minutes | closes the connection |
| Delivery of a response | thirty seconds | closes the connection |
| Length of a request line | 1 MiB, newline excluded | sends an error, then closes the connection |

The silence deadline resets on every read, so it limits silence and does not
limit work. An `ensure` that boots for a minute is unaffected. A request
that arrives in pieces is also unaffected.

A response is one line of JSON and fits the socket buffer. A client that
reads its answers never reaches the delivery limit. The limit applies to a
client that asks and then stops reading.

The length limit applies to a client that never sends a newline. brigd sends
the error the moment the limit is reached, while the client is usually still
writing. A client that reads as it writes sees the error. A client that does
not read until its write returns sees a broken pipe.

## How brigd behaves

`ensure` takes the same path as the CLI. It prepares the guest home, resolves
credentials, verifies the image and checks the share. Then it boots the
sandbox only if a boot is necessary. brigd serializes work on one sandbox,
so two clients that ask for the same sandbox at the same moment get one
boot.

brigd resolves the runtime for each request, from the profile that the
request names. A profile that carries `runtimeBin` drives the same binary
through the daemon as through the CLI. A profile that names a missing binary
fails that request and no other.

brigd never asks a question. The CLI stops to ask when an image under Brig's
own registry fails to verify. brigd refuses a request that needs that
prompt, and `error` carries the reason and the setting that overrides it. To
let such a request through, fix the image or set `BRIG_VERIFY=off`.

## After a restart

brigd reconciles nothing on a restart. The inventory lives in memory and
holds only what this process did. On start, brigd loads the profile registry
and begins to listen. It does not ask the runtime which sandboxes it started
before.

| After a restart | Result |
| --- | --- |
| `status` | shows no sandbox until something asks for it again, although the sandboxes are still up |
| `stop` | acts on a running sandbox that the inventory does not hold, because the runtime is the source of truth |
| `brig ls` | shows the sandboxes throughout, because it reads the runtime directly |

## Stability

[Stability](stability.md#stable-enough-to-script-against) lists the protocol
as stable. Within a version, a field can be added, but no field is renamed
or removed. A client that ignores unknown fields continues to work. A change
that breaks such a client becomes a new version (`v: 2`). The
[`--json` output](cli.md#--json-output) follows the same rule.
