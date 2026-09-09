# brigd

`brigd` keeps the session inventory and owns boot and teardown when several
callers want the same sandbox. It runs the same `internal/wrap` library the
CLI does, so it cannot grow a second opinion about how a sandbox is built.

It is deliberately small, and optional. The CLI has no client for it: every
`brig` command talks to the runtime directly, whether brigd is running or not.
`brig doctor` reports it with a neutral `--`, the mark it gives anything
absent that is not a problem, never `!!`:

```
--  brigd     not running (no socket at /Users/pmoust/.brig/brigd.sock)
```

Run it when something else wants one socket to ask brig through. That is a
second tool driving several sandboxes from one process, or a client that
wants boots on one sandbox serialized across several callers. Most people
never run it.

## What it does not do

It does not proxy exec. Handing your terminal to a process inside the guest
means passing file descriptors. `brig sh` already does that correctly, by
replacing itself with the runtime. The daemon owns lifecycle, the CLI owns the
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

One daemon serves one socket path. Starting a second on a path already being
served exits non-zero and names the process in the way, rather than taking
the path over. Two daemons sharing one socket split the inventory, each
holding half the sandboxes. The lock is a `brigd.sock.lock` file beside the
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
  "sandbox":"brig-claude-code-refactor","workspace":"/Users/me/brig/claude-code-refactor",
  "running":true}]}
```

Errors come back as `{"v":1,"ok":false,"code":...,"error":"..."}` rather than as
a closed connection.

### Protocol version

Every request and every response carries `v`, the protocol version. It is `1`
today.

A request with no `v` is read as version 1: there is no client older than this,
and the field has to start somewhere. A request carrying a `v` brigd does not
know is refused with `code` 2 and an error naming the versions it does speak.
It is not served a shape brigd cannot be sure it understands. The response
always carries `v`, so a client can tell what it is talking to from the
answer itself.

### Request id

A request can carry `id`, any string, and the response echoes it back
unchanged. A client pipelining several requests on one connection can match
each answer to its question. Absent in, absent out:

```json
{"v":1,"id":"boot-42","op":"ensure","agent":"claude-code"}
{"v":1,"id":"boot-42","ok":true,"code":0,"sessions":[...]}
```

### Exit code

Beside `error`, a response carries `code`: the stable exit status `brig`
returns to a script, the same set and the same causes. A script driving
brigd branches the way one driving brig does. `0` is success. On failure it
is one of `1` general, `2` usage (an unknown op, an unknown protocol
version), or `3` no such profile or sandbox. The rest are `4` a runtime that
is missing or broken, `5` a boot verification refused, and `6` a credential
that was not resolved. The full table is at
[docs/cli.md#exit-codes](cli.md#exit-codes).

```json
{"v":1,"op":"ensure","agent":"no-such-profile"}
{"v":1,"ok":false,"code":3,"error":"unknown agent \"no-such-profile\""}
```

### Who can connect

The socket is `0600` and the invoking user's alone. A mode is only a mode,
though. On each accepted connection brigd reads the peer's uid from the
kernel, which the peer cannot forge (`SO_PEERCRED` on Linux, `LOCAL_PEERCRED`
on darwin). It refuses a uid other than its own, with one line and a closed
connection:

```json
{"v":1,"ok":false,"code":1,"error":"connection from uid 1001 refused: this socket serves only its owner, uid 1000"}
```

The socket carries lifecycle control over sandboxes holding live credentials, so
this is a floor, not a nicety.

`running` is re-read from the runtime on every report, rather than taken from
the inventory. A sandbox stopped by something else is still reported as
stopped. When the runtime cannot be asked at all -- its binary is gone, a
permission error, containerd is down -- the session carries `runningError`
instead. `running` says nothing:

```json
{"agent":"claude-code","sandbox":"brig-claude-code","workspace":"/Users/me/brig/claude-code",
  "running":false,"runningError":"nerdctl ps: exit status 1: cannot connect to containerd"}
```

Show that as "cannot tell", not as a stopped sandbox. The daemon booted this
one, so a bare `running:false` reads as exited, a claim a runtime nobody can
reach never made.

Anything the run says about itself comes back with it, as `warnings`, one
line each. Examples: a credential that was not forwarded and why, a secret
about to expire, an image the check did not confirm. The CLI prints these on
the terminal of the person who typed the command. The daemon has no such
terminal, so they travel to the client that asked, not to brigd's own
terminal, wherever that is.

Only things to act on. What a run narrates about its own progress, such as
the line saying a boot has started, goes to brigd's stderr, not to the
client. An `ensure` that went entirely well comes back with no `warnings` at
all.

A connection that sends nothing for five minutes is closed. The deadline is
reset on every read, so it bounds the silence and not the work. An `ensure`
that spends a minute booting is not racing it, and neither is a request that
arrives in pieces.

A response has thirty seconds to be delivered. A response is a line of JSON and
fits the socket buffer, so a client that reads its answers never comes near
this. One that asks and then stops reading holds a goroutine and a
descriptor open indefinitely once the buffer fills, so its connection is
closed instead.

A request line is at most 1 MiB, newline excluded. That is far more than an
op, a profile name and a session name need. The limit exists so a client
that never sends a newline cannot make the daemon buffer without bound. A
longer one gets an error saying so written to it, and the connection is then
closed. What follows an over-length request in the stream cannot be told
apart from the tail of it.

Whether that error is read is another matter. It goes out the moment the
limit is reached, while the client is usually still writing. A client that
does not read until its write returns sees the broken pipe rather than the
error. The error is there for one that reads as it writes. The closed
connection is what everybody else gets.

For instance, with `socat`:

```bash
echo '{"op":"ensure","agent":"claude"}' | socat - UNIX-CONNECT:$HOME/.brig/brigd.sock
```

## Behaviour worth knowing

`ensure` takes exactly the CLI's path: it prepares the guest home, resolves
credentials, verifies the image and checks the share, then boots only if
needed. Work on one sandbox is serialised, so two clients asking for the same
one at the same moment get one boot rather than two.

The daemon never asks a question. The image check stops to ask when an image
claiming to be ours fails verification. There is nobody at brigd's terminal
to answer: the client is somewhere else, and the sandbox's lock stays held
across the wait. So a request that needs a prompt is refused
instead, with the reason and the setting that overrides it in `error`. Setting
`BRIG_VERIFY=off`, or fixing the image, is how such a request goes through.

The runtime is resolved per request, from the profile the request names. A
profile carrying `runtimeBin` drives the same binary through the daemon as it
does through the CLI. A profile naming a binary that is not there fails that
request and no other.

`status` re-reads liveness from the runtime instead of trusting the inventory.
A sandbox can be stopped by anything, including a `brig stop` that never went
through the daemon.

The inventory lives in memory. Restarting brigd forgets which sandboxes it
started, though the sandboxes themselves keep running and `brig ls` still
finds them.

## What a restart reconciles

Nothing. On start brigd loads the profile registry and begins listening. It
does not go to the runtime to rebuild which sandboxes it had started. The
inventory is a cache of what this process has done, not a record of what
exists. The runtime is the source of truth. That is why `status` re-reads
liveness from it on every report, and `stop` acts on a running sandbox the
inventory never heard of.

So a fresh daemon has an empty inventory over a set of sandboxes that are
still up. `status` shows none until something asks for them again. `brig
ls`, which reads the runtime directly, shows them throughout.

## Stability

The protocol is versioned so a client can tell what it is talking to. It is
**internal until brig 0.3**: within a version a field can be added, but none
is renamed or removed. A client that ignores unknown fields keeps working. A
change that breaks such a client becomes a new version (`v: 2`). That is the
same rule `brig --json` output follows.
