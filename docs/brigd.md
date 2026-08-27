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

One daemon serves one socket path. Starting a second on a path that is already
being served exits non-zero and names the process in the way, rather than
taking the path over: two daemons on one socket would each keep an inventory
of half the sandboxes. The lock is a `brigd.sock.lock` file beside the socket,
held for as long as the daemon runs and released by the kernel if it dies.

## Protocol

Line-delimited JSON, one request per line, one response per line.

```json
{"op":"ensure","agent":"claude-code","name":"refactor"}
{"op":"status"}
{"op":"stop","agent":"claude-code","name":"refactor"}
{"op":"version"}
```

`agent` is a profile name or alias. `name` is the optional session name and
follows the same rules as `brig --name`.

A response looks like:

```json
{"ok":true,"sessions":[{"agent":"claude-code","name":"refactor",
  "vm":"brig-claude-code-refactor","workspace":"/Users/me/brig/claude-code-refactor",
  "running":true}]}
```

Errors come back as `{"ok":false,"error":"..."}` rather than as a closed
connection.

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
