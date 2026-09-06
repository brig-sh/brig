# brigd

`brigd` keeps the session inventory and owns boot and teardown when several
callers want the same sandbox. It runs the same `internal/wrap` library the
CLI does, so it cannot grow a second opinion about how a sandbox is built.

It is deliberately small. The CLI works fine without it, and most people will
never run it.

## What it does not do

It does not proxy exec. Handing your terminal to a process inside the guest
means passing file descriptors, and `brig exec` already does that correctly by
replacing itself with the runtime. The daemon owns lifecycle, the CLI owns the
terminal.

## Running it

```bash
brigd                                  # ~/.brig/brigd.sock, or $XDG_RUNTIME_DIR
brigd --socket /tmp/brigd.sock
```

The socket is created 0600. It carries lifecycle control over sandboxes
holding live credentials, so it belongs to the invoking user alone.

A unix socket path has a length limit the kernel enforces -- 103 bytes on
macOS, 107 on Linux -- and a path over it is refused before the bind, with the
limit, the length given, and where the path came from. The kernel's own answer
to an over-long path is `bind: invalid argument`, which names none of those.

One daemon serves one socket path. Starting a second on a path that is already
being served exits non-zero and names the process in the way, rather than
taking the path over: two daemons on one socket would each keep an inventory
of half the sandboxes. The lock is a `brigd.sock.lock` file beside the socket,
held for as long as the daemon runs and released by the kernel if it dies.

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
know is refused with `code` 2 and an error naming the versions it does speak,
rather than served a shape it may not understand. The response always carries
`v`, so a client can tell what it is talking to from the answer itself.

### Request id

A request may carry `id`, any string, and the response echoes it back unchanged,
so a client pipelining several requests on one connection can match each answer
to its question. Absent in, absent out:

```json
{"v":1,"id":"boot-42","op":"ensure","agent":"claude-code"}
{"v":1,"id":"boot-42","ok":true,"code":0,"sessions":[...]}
```

### Exit code

Beside `error`, a response carries `code`: the stable exit status `brig` returns
to a script, the same set and the same causes, so a script driving brigd
branches the way one driving brig does. `0` on success, and on failure one of
`1` general, `2` usage (an unknown op, an unknown protocol version), `3` no such
profile or sandbox, `4` a runtime that is missing or broken, `5` a boot
verification refused, `6` a credential that could not be resolved. The full
table is in the README, beside the command reference.

```json
{"v":1,"op":"ensure","agent":"no-such-profile"}
{"v":1,"ok":false,"code":3,"error":"unknown agent \"no-such-profile\""}
```

### Who may connect

The socket is `0600` and the invoking user's alone. A mode is only a mode,
though, so on each accepted connection brigd reads the peer's uid from the
kernel -- `SO_PEERCRED` on Linux, `LOCAL_PEERCRED` on darwin, neither of them
anything the peer can forge -- and refuses a uid other than its own with one
line and a closed connection:

```json
{"v":1,"ok":false,"code":1,"error":"connection from uid 1001 refused: this socket serves only its owner, uid 1000"}
```

The socket carries lifecycle control over sandboxes holding live credentials, so
this is a floor, not a nicety.

`running` is re-read from the runtime on every report rather than taken from
the inventory, so a sandbox stopped by something else is still reported as
stopped. When the runtime could not be asked at all -- its binary is gone, a
permission error, containerd is down -- the session carries `runningError`
instead, and `running` says nothing:

```json
{"agent":"claude-code","sandbox":"brig-claude-code","workspace":"/Users/me/brig/claude-code",
  "running":false,"runningError":"nerdctl ps: exit status 1: cannot connect to containerd"}
```

Show that as "cannot tell", not as a stopped sandbox: the daemon booted this
one, so `running:false` on its own would claim it exited, and a runtime nobody
could reach never made that claim.

Anything the run says about itself comes back with it, as `warnings`, one line
each: a credential that was not forwarded and why, a secret about to expire, an
image that could not be verified. The CLI prints these on the terminal of the
person who typed the command, and the daemon has no such terminal, so they
travel to the client that asked rather than to whatever terminal brigd happens
to have been started from.

Only things to act on. What a run narrates about its own progress, such as the
line saying a boot has started, goes to brigd's stderr and not to the client, so
an `ensure` that went entirely well comes back with no `warnings` at all.

A connection that sends nothing for five minutes is closed. The deadline is
reset on every read, so it bounds the silence and not the work: an `ensure`
that spends a minute booting is not racing it, and neither is a request that
arrives in pieces.

A response has thirty seconds to be delivered. A response is a line of JSON and
fits the socket buffer, so a client that reads its answers never comes near
this; one that asks and then stops reading would hold a goroutine and a
descriptor open indefinitely once the buffer filled, and has its connection
closed instead.

A request line is at most 1 MiB, newline excluded. That is far more than an op,
a profile name and a session name need; the limit exists so a client that never
sends a newline cannot make the daemon buffer without bound. A longer one gets
an error saying so written to it, and the connection is then closed, because
what follows an over-length request in the stream cannot be told apart from the
tail of it.

Whether that error is read is another matter. It goes out the moment the limit
is reached, which is while the client is usually still writing, so a client that
does not read until its write returns sees the broken pipe rather than the
error. The error is there for one that reads as it writes; the closed connection
is what everybody else gets.

For instance, with `socat`:

```bash
echo '{"op":"ensure","agent":"claude"}' | socat - UNIX-CONNECT:$HOME/.brig/brigd.sock
```

## Behaviour worth knowing

`ensure` takes exactly the CLI's path: it prepares the workspace, resolves
credentials, verifies the image and checks the share, then boots only if
needed. Work on one sandbox is serialised, so two clients asking for the same
one at the same moment get one boot rather than two.

The daemon never asks a question. The image check stops to ask when an image
claiming to be ours fails verification, and there is nobody on the other end of
brigd's terminal to answer: the client is somewhere else and the sandbox's lock
would be held across the wait. So a request that would have prompted is refused
instead, with the reason and the setting that overrides it in `error`. Setting
`BRIG_VERIFY=off`, or fixing the image, is how such a request goes through.

The runtime is resolved per request, from the profile the request names, so a
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

Nothing. On start brigd loads the profile registry and begins listening; it does
not go to the runtime to rebuild which sandboxes it had started. The inventory
is a cache of what this process has done, not a record of what exists -- the
runtime is the source of truth, which is why `status` re-reads liveness from it
on every report and `stop` will act on a running sandbox the inventory never
heard of. So a fresh daemon has an empty inventory over a set of sandboxes that
are still up: `status` shows none until something asks for them again, and
`brig ls`, which reads the runtime directly, shows them throughout.

## Stability

The protocol is versioned so a client can tell what it is talking to. It is
**internal until brig 0.3**: within a version a field may be added, but none is
renamed or removed, so a client that ignores unknown fields keeps working; a
change that would break such a client is a new version (`v: 2`). That is the
same rule `brig --json` output follows.
