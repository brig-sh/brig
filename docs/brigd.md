# brigd

`brigd` keeps the session inventory and owns boot and teardown when several
callers want the same sandbox. It runs the same `internal/wrap` library the
CLI does, so both build a sandbox the same way.

brigd is optional. Every `brig` command talks to the runtime directly,
whether brigd is running or not. Only `brig doctor` talks to brigd, to ask a
running one for its build. With no brigd, `brig doctor` prints `--`, the
mark for something absent that is not a problem:

```
  --  brigd     not running (no socket at /Users/me/.brig/brigd.sock)
```

The line reads `!!` when the socket's mode is not `0600`, or when the
running brigd is a different build from `brig`.

Run brigd when another program needs one socket to drive Brig through: a
tool that drives several sandboxes from one process, or a client that needs
boots of one sandbox serialized across several callers. Most people never
run it.

## What it does not do

It does not proxy exec. Handing your terminal to a process inside the guest
means passing file descriptors, and `brig sh` already does that by replacing
itself with the runtime. The daemon owns lifecycle, and the CLI owns the
terminal.

## Running it

```bash
brigd                                  # $XDG_RUNTIME_DIR/brigd.sock if set, else ~/.brig/brigd.sock
brigd --socket /tmp/brigd.sock
```

The socket is created 0600. It carries lifecycle control over sandboxes
holding live credentials, so it belongs to the invoking user alone.

A unix socket path has a length limit the kernel enforces: 103 bytes on
macOS, 107 on Linux. A path over it is refused before the bind, naming the
limit, the length, and where the path came from. The kernel's own answer to
an over-long path is `bind: invalid argument`, which names none of those.

One daemon serves one socket path. A second daemon started on a path that is
already served exits non-zero and names the process holding it. It does not
take the path over, because two daemons on one socket would each hold part
of the inventory. The lock is a `brigd.sock.lock` file beside the
socket, held for as long as the daemon runs and released by the kernel if it
dies.

## Protocol

Line-delimited JSON, one request per line, one response per line.

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

An error comes back as `{"v":1,"ok":false,"code":...,"error":"..."}`, not as
a closed connection.

`version` answers with the build the daemon came from, the same fields
`brig version --json` prints for the CLI:

```json
{"v":1,"ok":true,"code":0,"version":"v0.2.0",
  "commit":"131e3bc5615df5ff74e6b5af9a5bcf2ed42b1d57","commitTime":"2026-09-15T09:36:19Z",
  "modified":false}
```

`commit` and `commitTime` are absent when the build carried no git history.
`modified` is always present, and `true` when the tree had uncommitted changes.

### Protocol version

Every request and every response carries `v`, the protocol version. It is `1`
today.

A request with no `v` is read as version 1. A request carrying a `v` brigd
does not know is refused with `code` 2 and an error naming the versions it
does speak. The response always carries `v`, so a client can tell what it is
talking to from the answer.

### Request id

A request can carry `id`, any string, and the response echoes it back
unchanged. A client pipelining several requests on one connection can match
each answer to its question. Absent in, absent out:

```json
{"v":1,"id":"boot-42","op":"ensure","agent":"claude-code"}
{"v":1,"id":"boot-42","ok":true,"code":0,"sessions":[...]}
```

### Exit code

Beside `error`, a response carries `code`. brigd uses the same codes and
the same causes as `brig` itself, listed at
[docs/cli.md#exit-codes](cli.md#exit-codes). A script driving brigd
branches the way one driving `brig` does.

```json
{"v":1,"op":"ensure","agent":"no-such-profile"}
{"v":1,"ok":false,"code":3,"error":"unknown agent \"no-such-profile\""}
```

### Who can connect

The socket is `0600` and the invoking user's alone. brigd also checks each
accepted connection: it reads the peer's uid from the kernel, which the peer
cannot forge (`SO_PEERCRED` on Linux, `LOCAL_PEERCRED` on macOS). It refuses
a uid other than its own, with one line and a closed connection:

```json
{"v":1,"ok":false,"code":1,"error":"connection from uid 1001 refused: this socket serves only its owner, uid 1000"}
```

`running` is read from the runtime on every report, not from the inventory,
so a sandbox stopped by something else is reported as stopped. Sometimes
the runtime cannot be asked at all: its binary is gone, a permission error
came back, or containerd is down. Then the session carries `runningError`,
and `running` means nothing:

```json
{"agent":"claude-code","sandbox":"brig-claude-code","workspace":"/Users/me/.brig/homes/brig-claude-code",
  "running":false,"runningError":"nerdctl ps: exit status 1: cannot connect to containerd"}
```

A client should show that as "cannot tell", not as a stopped sandbox.

What the run warns about comes back in the response as `warnings`, one line
each: a credential that was not forwarded and why, a secret about to
expire, an image that did not verify. The CLI prints these on your
terminal. The daemon has no such terminal, so they go to the client that
asked.

`warnings` holds only things to act on. Progress lines, such as the one
saying a boot has started, go to brigd's stderr. An `ensure` with nothing to
report comes back with no `warnings`.

A connection that sends nothing for five minutes is closed. The deadline is
reset on every read, so it limits silence, not work. An `ensure` that spends
a minute booting is unaffected, and so is a request that arrives in pieces.

A response has thirty seconds to be delivered. A response is a line of JSON
and fits the socket buffer, so a client that reads its answers never reaches
that limit. A client that asks and then stops reading would hold a goroutine
and a descriptor open once the buffer fills, so brigd closes its connection.

A request line is at most 1 MiB, newline excluded. The limit stops a client
that never sends a newline from making the daemon buffer without bound. A
longer request gets an error, and brigd then closes the connection, because
it cannot tell what follows from the rest of that request.

That error goes out the moment the limit is reached, while the client is
usually still writing. A client that reads as it writes sees the error. A
client that does not read until its write returns sees a broken pipe.

An example with `socat`:

```bash
echo '{"op":"ensure","agent":"claude"}' | socat - UNIX-CONNECT:$HOME/.brig/brigd.sock
```

## How brigd behaves

`ensure` takes exactly the CLI's path: it prepares the guest home, resolves
credentials, verifies the image and checks the share, then boots only if
needed. Work on one sandbox is serialised, so two clients asking for the same
one at the same moment get one boot rather than two.

The daemon never asks a question. The CLI stops to ask when an image under
Brig's own registry fails to verify. Nobody is at brigd's terminal to
answer, and the sandbox's lock would stay held across the wait. So brigd
refuses a request that needs a prompt, with the reason and the setting that
overrides it in `error`. Fix the image, or set `BRIG_VERIFY=off`, to let
such a request through.

The runtime is resolved per request, from the profile the request names. A
profile carrying `runtimeBin` drives the same binary through the daemon as it
does through the CLI. A profile naming a binary that is not there fails that
request and no other.

`status` reads liveness from the runtime on every report, because anything
can stop a sandbox, including a `brig stop` that never went through the
daemon.

## What a restart reconciles

Nothing. The inventory lives in memory and holds only what this process has
done. On start brigd loads the profile registry and begins listening. It
does not ask the runtime which sandboxes it had started. The runtime is the
source of truth, so `stop` acts on a running sandbox the inventory does not
hold.

A restarted daemon has an empty inventory while its sandboxes are still up.
`status` shows none until something asks for them again. `brig ls`, which
reads the runtime directly, shows them throughout.

## Stability

The protocol is versioned so a client can tell what it is talking to, and
[stability.md](stability.md#stable-enough-to-script-against) lists it as
stable: within a version a field can be added, but none is renamed or
removed. A client that ignores unknown fields keeps working. A change that
breaks such a client becomes a new version (`v: 2`), the same rule
[docs/cli.md#--json-output](cli.md#--json-output) sets for `--json` output.
