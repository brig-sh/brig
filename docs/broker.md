# The credential broker

The broker serves the routes of [policies.md](policies.md#routes). A tool in
the guest sends its requests to the route with a session token. The broker
checks the token and the route's `request.allow`, adds the credential on the
host, and forwards the request to the route's upstream. The guest never holds
the credential.

This page describes `internal/broker`, the request handler, and
`brig-broker`, the process that hosts it. In this release no runtime routes a
guest to the broker yet, and a run bound to a policy with routes on hull or
nerdctl is refused before boot. See [Transports](#transports).

## Request path

A request names its sandbox and its route:

```
https://brig.internal/<sandbox-id>/r/<route>/<rest>
```

`<sandbox-id>` is the runtime's instance name, the one `brig info` prints.
`<rest>` is the path the upstream receives, with the query kept.

The broker runs these steps in order. A request that fails one step gets the
error of that step, and the upstream is never dialed.

1. Refuse a protocol upgrade: an `Upgrade` header, `upgrade` in
   `Connection`, or the `CONNECT` method. This is a 400. An upgraded
   connection or a tunnel would be a raw pipe with the credential on it, and
   no later request on it would be checked.
2. Split the path. An unknown sandbox or route is a 404.
3. Read the session token. It comes from `Authorization: Bearer`. On a
   `basicAuth` route it can also be the basic-auth password, which is what
   git's credential helper sends. On a route that sets the credential in
   another header, such as `x-api-key`, it can also come from that header.
4. Verify the token: the signature with the sandbox key, the sandbox claim
   against the path, the route claim against the route, the expiry, and the
   address the request arrived from. Any failure is a 401.
5. Read the body, up to 8 MiB, and check the expiry again: the body can
   take longer to arrive than the token had left. An expired token is a
   401. Then refuse a session token anywhere but its carrier. This is a
   403. See [Placeholder detection](#placeholder-detection).
6. Refuse a `<rest>` that the upstream could read as another path. This is
   a 400. See [Canonical paths](#canonical-paths).
7. Check `request.allow`. A miss is a 403 with the refusal below. No
   credential has been read yet.
8. Resolve the credential through the secret broker. A failure is a 502,
   and nothing is cached.
9. Edit the headers and forward the request. The response streams back as
   the upstream sends it, with the credential redacted.
10. Write the audit line.

### Canonical paths

`request.allow` matches `<rest>` as the broker sees it. An upstream that reads
the same path another way could serve a path the list does not cover. So the
broker refuses a `<rest>` with:

- a `.` or `..` segment, also with a `;parameter` after it, such as `..;`,
  or with a parameter after a Unicode semicolon, such as `..；x`: the
  code points that NFKC folds to `;`, which are `；`, `﹔`, `︔` and
  the Greek question mark U+037E, which NFC folds to `;` too
- a segment of fullwidth or other Unicode dots, such as `．．` or `‥`: the
  code points that NFKC folds to dots, and the ideographic full stops
- a Unicode slash, backslash or percent sign, such as `／`: the code points
  that NFKC folds to `/`, `\` or `%`, and the look-alikes `∕`, `⁄`, `⧸`,
  `⧵`, `⧹` and `∖`. `．．／admin` is one segment to the broker, and
  `../admin` to an upstream that normalizes Unicode
- an empty segment, except a trailing one
- a backslash or a control character
- an escaped `/`, `\` or `.`, also escaped twice, such as `%252e`
- an escaped `%`, such as `%25%32%65`: it reads as `%2e` here, and as a dot
  to an upstream that decodes the path twice
- on a route with `request.allow.repos`, any character outside ASCII

### Credential field

`credential.field` selects a string in a document-shaped secret. The broker
looks for it in this order:

1. The dotted name as a path from the root, such as `claudeAiOauth`, then
   `accessToken`.
2. A key at the root that holds the whole name.
3. At any depth: a key that holds the whole name, or the path below a key
   that holds its first segment.

At any depth, two matches with different values are refused with a 502. A
credential document can carry the tokens of other services under the same
key name, and the broker does not pick one at random.

The value, the field or the whole secret, goes into a header. The broker
drops one trailing `\n` or `\r\n`, which a secret saved from a file usually
has. A value with any other control character is refused with a 502.

### Headers

Before the request goes upstream, the broker removes:

- every header whose name holds an underscore. Some servers read `_` as `-`,
  so `x_api_key` would otherwise pass every rule as another header
- the hop-by-hop headers, and the headers that `Connection` names
- `Forwarded`, `Via`, `X-Real-Ip` and every `X-Forwarded-` header
- `X-Http-Method-Override`, `X-Http-Method`, `X-Method-Override`,
  `X-Original-Url`, `X-Rewrite-Url` and `X-Original-Method`. A framework that
  honors one serves another method or path than `request.allow` checked
- every header in `inject.headers.remove` and `inject.headers.set`, with
  `Authorization` always among them
- every `X-Brig-` header and every `Proxy-` header
- any header that holds the session token
- `Accept-Encoding`. The transport then asks for a compression it undoes
  itself, so the response body the broker redacts is plain. On a request
  with `Range`, or a `HEAD`, the transport asks for none, and the broker
  sends `Accept-Encoding: identity`
- every request trailer. The rules here edit the headers, and a trailer
  would carry past them what they remove

The names are compared in lower case.

`inject.headers.allow` then keeps only the listed values of its header. A
header that holds a comma-separated list is filtered item by item, and the
values that remain are sent as one header. The audit line counts the values
dropped, without naming them.

Last, `inject.headers.set` sets each header with the credential in place of
`{value}`, or `inject.basicAuth` sets `Authorization: Basic`. `Host` is the
upstream's.

### Upstream

The broker dials the route's upstream over TLS, verified with the system
roots. It does not use a proxy from the environment. A TLS failure is a 502,
and there is no retry without verification.

The broker does not follow a redirect. A 3xx goes back to the guest as the
upstream sent it.

An event stream reaches the guest event by event. The broker flushes after
every write from the upstream.

### Response

An upstream that reflects request headers would hand the guest the
credential. Before a response reaches the guest, the broker:

- drops every informational (1xx) response, such as `103 Early Hints`. Its
  headers would reach the guest before the checks below
- drops every `Set-Cookie`
- drops every header and trailer whose value holds an injected string
- replaces each injected string in the body with `[redacted]`, as the body
  streams

The injected strings are the credential and, on a `basicAuth` route, the
base64 that `Authorization: Basic` carries. The broker matches these exact
bytes. A credential the upstream sends back encoded another way, such as
URL-escaped, is not found. So a body the broker cannot read as plain bytes,
one with a `Content-Encoding` other than `identity` after the transport
undid its own gzip, is refused with a 502.

A string can span two writes of the upstream. The broker holds back the end
of a write only while it could be the start of an injected string. An
injected string holds no line break, so the end of an event is never held.
The response has no `Content-Length`, because each replacement changes the
length.

### Retry after a 401

On a route whose credential has a lifecycle, such as `claude-code`, an
upstream 401 renews the credential once and sends the request once more. The
response to that retry goes back to the guest, whatever its status.

Each forced renewal rotates the refresh token, and one that fails at the
provider leaves the secret unusable for every sandbox that shares it. So the
broker forces at most one renewal per secret per minute. A 401 within that
minute is retried only when another request has renewed the secret since.
Otherwise it goes back to the guest. A credential close to its expiry is
renewed ahead of time when it is resolved, without a 401.

The broker reads the request body before the request goes upstream, up to
8 MiB per request, to scan it for a [session token](#placeholder-detection).
The same copy is what a retry sends. A body over 8 MiB is not sent again,
and the first 401 goes back to the guest. [Holding the
body](#holding-the-body) has the limits on what all requests hold at once.

## Placeholder detection

The session token is the guest's handle on its route. The tool in the guest
sends it in one place, the carrier: `Authorization: Bearer`, the basic-auth
password on a `basicAuth` route, or a header the route sets with `{value}`,
such as `x-api-key`. Anywhere else, the agent is leaking its handle: in a
prompt that echoes the environment, a path, a query, a header of its own. So
after the token verifies, and before `request.allow` and the credential, the
broker scans the request for every live token of the sandbox. A hit is a 403,
and the upstream is never dialed:

```
brig: refused: the session token was sent outside its carrier (route github, in body)
```

The route is the route of the token found. A token of route `github` in a
request to route `anthropic` is a hit too. Sent as the carrier of the other
route, it fails the route check with 401 before the scan. The audit line has
`check=placeholder`, `where=` and `encoding=`, and never the token or the
body. `GET /status` on the control socket counts the refusals per sandbox.

The scan covers:

- the method, the path as sent and as decoded, and the query as sent and as
  decoded. The query is decoded escape by escape, and an escape that is not
  two hex digits stays as it is
- `Host`, and every header name and value. In a carrier it scans what the
  value holds besides the token: the scheme, the basic-auth username, the
  text around `{value}`. A carrier that holds another token is refused. The
  same token in two carriers of its route, such as `Authorization` and
  `x-api-key`, passes
- the body, up to 8 MiB, and the trailers of a body read to its end. The
  trailers are not forwarded

It looks for each token in these spellings:

| Encoding | Spelling |
| --- | --- |
| `raw` | The token. A JSON string holds it as is |
| `percent` | Every byte percent-escaped, in upper or lower case hex |
| `json` | Every byte as `\u00XX`, in upper or lower case hex |
| `hex` | Upper or lower case |
| `base64` | With or without padding, alone or inside the encoding of a longer string, such as `Bearer <token>`, at any of the three alignments. A token's base64 holds no `+` or `/`, so it is the same in the standard and the URL alphabet |

Each spelling uses one case throughout. Outside the path and the query,
which the broker decodes, a token escaped in mixed case, such as `%6a%4B`,
is not found.

The scan drops spaces, tabs and line breaks first, and the escapes `\n`,
`\r` and `\t` that a JSON string writes them as, and reads `\/` as `/`, each
also escaped again, as `\\n` is in a JSON string inside a JSON string. So a
token split across lines is found, such as base64 wrapped at 76 columns,
also inside a JSON string. A gzip or deflate body is scanned as it
decompresses, and the encoding is named `gzip+raw` and so on. It is also
scanned as sent, since an upstream that ignores the coding reads it so.

The live tokens of a sandbox are every token minted for it, and any token
presented as its carrier, until each expires: 12 hours for a token Brig
mints. A sandbox keeps at most 1024. Brig mints one per route on every exec,
so only a sandbox with more than 512 execs in 12 hours on two routes drops
its oldest early. They are kept in memory only. A broker that restarts
registers each sandbox with a new key, so a token of the old broker fails
the signature check before the scan. A sandbox that is removed takes its
tokens and its count with it.

The tokens of other sandboxes are not looked for. A leak of a handle is a
leak by the sandbox that holds it. The broker cannot tell another sandbox's
token in a prompt from data the agent was handed, and refusing it would let
one sandbox stop another's conversation by planting its own token in a file
the other reads.

The broker keeps, per live token, the hash of the last 32 bytes of each of
its ten spellings, and not the spellings. The scan hashes every 32-byte
window of the request once. Where a hash matches, it compares that spelling,
computed byte by byte from the token. The last 32 bytes encode the token's
MAC, so a match is nearly always a real one, and the time is linear in the
request, whatever the number of tokens. 1024 tokens take about 540 KB.
Measured on a Mac with an M-series chip, for an 8 MiB body: 21 ms with 10
live tokens, and 35 ms for a body built of the ends of the spellings of 64
(`BenchmarkPlaceholderScanAdversarial`). A test holds the scan under 500 ms.

Limits:

- Above 8 MiB, the start of the body is scanned and the rest goes upstream
  unscanned. The audit line says `placeholder=partial`. No smaller body is
  scanned in part: one the budget cannot hold is refused.
- A request body with a `Content-Encoding` other than `gzip`, `x-gzip` or
  `deflate`, with more than one, that does not decompress, or with bytes
  after the end of its compressed stream, is refused with 415. So is a
  body above 8 MiB whose stream fails or ends before the cut at 8 MiB.
  `deflate` is read as zlib, or as raw deflate when it has no zlib header.
  A compressed body is scanned up to 8 MiB of its decompressed bytes, and
  goes upstream as the guest compressed it. One read whole that
  decompresses to more is refused with 413. git compresses the body of a
  large fetch negotiation with gzip.
- A spelling not in the table is not found: a token escaped in part or in
  mixed case outside the path and query, JSON escapes other than the four
  above, quoted-printable, hex with separators, HTML entities, encoded
  twice, encrypted, or inside an archive in the body. Go puts a header name
  in canonical case, so a token in a name is found only in a spelling that
  keeps its case, such as lower case hex. The scan stops a leak by accident or by an injected
  prompt. An agent that means to hide its token can.
- A conversation whose history holds the token is refused on every later
  turn, since the history is sent again each time, until the token expires
  or the sandbox is removed. A new conversation works.

### Holding the body

The broker reads the body before the request goes upstream. What it holds is
taken from the replay budget, 64 MiB for all requests at once, which
`ReplayBudget` in the broker's configuration changes:

- The buffer grows as the bytes arrive, from 32 KiB at a time, and not from
  the length the request declares. A body that declares 8 MiB and stalls
  holds 32 KiB.
- A sandbox holds at most its share: the budget over the registered
  sandboxes, and never less than 8 MiB and one byte. So one sandbox that
  stalls its bodies leaves the rest of the budget to the others.
- A body the budget cannot hold is never forwarded unscanned. It waits for
  bytes to come back, up to a tenth of `BodyTimeout`, and is then refused
  with 503 and `Retry-After: 1`. The audit line says `check=budget`.
- The part the broker holds has to arrive within `BodyTimeout`, 60 seconds
  by default. A body that does not is refused with 408. The rest of a body
  over 8 MiB streams with no deadline.

## Errors

| Status | When | `check=` on the audit line |
| --- | --- | --- |
| 400 | The request asks for a protocol upgrade, or is a `CONNECT` | `upgrade` |
| 404 | The path is not `/<sandbox-id>/r/<route>/…`, or names a sandbox or route the broker does not serve | `path`, `sandbox`, `route` |
| 401 | No token, a malformed token, a bad signature, a token of another sandbox or route, an expired token, or a request that arrived on another sandbox's gateway | `token`, `signature`, `token.sandbox`, `token.route`, `expired`, `arrival` |
| 400 | The request body could not be read | `body` |
| 408 | The part of the body the broker holds did not arrive within `BodyTimeout` | `body` |
| 503 | The replay budget, or the sandbox's share of it, could not hold the body in time | `budget` |
| 413 | A compressed body read whole decompresses to more than 8 MiB | `body` |
| 403 | A session token of the sandbox is in the request outside its carrier | `placeholder` |
| 415 | The request body has a `Content-Encoding` other than gzip or deflate, more than one, does not decompress, or has bytes after its compressed stream | `content-encoding` |
| 400 | `<rest>` is not in canonical form | `path` |
| 403 | `request.allow` does not cover the request | `request.allow.methods`, `request.allow.paths`, `request.allow.repos` |
| 502 | The credential cannot be resolved, or the field it names is missing, ambiguous or not a header value. The body ends in "brig's secret store is not available on this host" when the store did not open | `credential` |
| 502 | The upstream cannot be reached, or its certificate does not verify | `upstream` |
| 502 | The response body has a `Content-Encoding` the broker cannot read | `response.encoding` |
| 500 | The broker failed on a bug. `broker.log` has the error. When the response had already started, the connection is closed and the line keeps its status | `panic` |

A 401 body does not say which check failed. The audit line does.

A 502 body does not carry the error of the secret backend.

## `request.allow`

The broker enforces the three lists of `request.allow`. Every list that is
set must match.

| List | Matched against |
| --- | --- |
| `methods` | The request method |
| `paths` | `<rest>`, without the query. `*` does not match a `/` |
| `repos` | The `owner/name` that the first two segments of `<rest>` name, with a `.git` suffix removed. A path with fewer than two segments is refused |

The `models` and `tools` lists that the Anthropic presets will take read the
request body. The policy type does not carry them yet, so the broker does not
enforce them.

A miss returns 403 with this body. The same rule and reason go to the audit
line:

```
brig broker refused this request
policy:  github-brig-sh
route:   github
rule:    spec.routes[github].request.allow.repos
reason:  repo "other-org/repo" is not in the allow list [brig-sh/*]
sandbox: brig-7f3a
```

## Session tokens

A session token is two base64url strings, without padding, joined by a dot:

```
base64url(claims) "." base64url(HMAC-SHA256(key_sandbox, claims))
```

The broker decodes both parts strictly, so a token has one spelling. Nothing
may follow the claims object.

The claims are a JSON object:

| Claim | What it is |
| --- | --- |
| `sandbox` | The sandbox id |
| `session` | The named session, or empty for the default one |
| `route` | The route name |
| `exp` | The expiry, in Unix seconds |
| `typ` | `brig-route` |

`key_sandbox` is HKDF-SHA256 of the host key, with no salt and the info
`brig-sandbox:<sandbox-id>:<nonce>`. The nonce is 16 random bytes, drawn each
time a sandbox is added to the broker. Each sandbox has its own key, so a
token of one sandbox fails the signature check on the path of another.

The host key is at least 32 bytes and stays in the memory of the broker. The
guest holds no key and cannot mint a token. A token lasts 12 hours by default.

Removing a sandbox from the broker refuses its tokens from the next request
on, with 404. A sandbox added again under the same id has a new nonce and a
new key, so the tokens minted before the removal fail the signature check
with 401. Replacing a sandbox that is still registered does the same.

## Hosting

`brig-broker` hosts the broker, one per host. It is a binary of its own,
installed beside `brig`, because it refreshes credentials and no package on
the run path links the refresher (`internal/hostsrc/arch_test.go`). Neither
does the run path link `internal/broker`, so `brig` cannot mint a token
itself. It asks `brig-broker`.

### Lifecycle

Brig starts `brig-broker` on the first run that has a route. It runs in a
session of its own, so it outlives the `brig` that started it and does not
take the terminal's signals. Every later run reuses it while its pid file
names a live process with the start time it recorded. A pid alone can be
reused by an unrelated process. The start time is the number the kernel keeps
for the process, not the text `ps` prints, which follows the locale. Brig
holds `start.lock` while it checks for a broker and starts one, so runs
started at once start one broker.

`brig stop`, `brig rm` and `brig rm --all` remove each sandbox they stop
from the broker. Its tokens are refused from the next
request on. They ask the control socket whatever the pid file says, and warn
when the broker is there and does not answer. When no sandbox is left, Brig
asks the broker to stop, and it stops only if no run registered a sandbox
since. A run registers its sandbox after the checks before boot, image
verification included. A run that fails before its sandbox boots removes it
from the broker again, which stops the broker when no sandbox is left. A run
whose registration the broker refuses stops the broker it started for it.
A run that boots a guest, a restart included, removes the sandbox from the
broker first, so the tokens of the guest before it are refused even when
the routes are the same. An exec into a running sandbox whose policies no
longer declare a route removes it too.

`brig rm --all` then removes every sandbox the broker still has registered
and stops it. That includes a sandbox of a runtime other than the detected
one, and a run that was killed between its registration and its boot. A
sandbox that is still running gets 404 on its routes until its next exec
registers it again.

On a host where brig's secret store does not open, such as a Linux host with
no Secret Service on its session bus, Brig refuses a run with a route before
boot, with exit `6`. A store that stops opening after the boot reaches the
broker instead. Each route then answers 502, ending in "brig's secret store
is not available on this host", with `check=credential`, and reaches no
upstream. The broker tries the store again on each request, and
`broker.log` says why it did not open.

Brig does not restart a broker that exited. The `BROKER` row says it is not
running, and the next run starts a new one. That run registers the sandbox
again with a new key, so an agent still running with a token from the old
broker gets 401 until its next exec. A broker that is alive and does not
answer on its control socket is named in the row by its pid, with
`broker.log` to read. `brig info --json` gives it `running: false` with its
`pid`, and a broker that is not running no `pid`.

### The port

The broker picks a free loopback port at first start and writes it to
`port`. Every later start binds that port and no other. If another process
holds it, the broker does not start, and Brig refuses the run and every exec
of a routed sandbox:

```
brig: could not register the routes of brig-claude-code with the credential broker: another process holds the credential broker's port, 127.0.0.1:52546, so brig refuses the run. ...
```

While the broker is down, its port is free. Any local process can bind it,
another user's included on a host several people use. Brig refuses to start
a broker beside such a process, and `brig doctor` fails its `broker` row and
names the process that holds the port.

`BRIG_BROKER_BIN` names a `brig-broker` to run instead of the one beside
`brig`, then on `PATH`.

### Files

Everything is in `~/.brig/host/broker`, or `host/broker` under
`BRIG_STATE_DIR`. Brig and the broker refuse the directory when it is a
link, belongs to another user, or is open to the group or to others. Every
file is created `0600`.

| File | What it holds |
| --- | --- |
| `hostkey` | 32 random bytes, drawn at first start. Each sandbox key is derived from it. Only the broker reads it. It is never on a command line or in a log |
| `ca.pem`, `ca.key` | The brig CA of this host. See [Transports](#transports). A `ca.key` that is not the key of `ca.pem` is refused at start. Remove both, and the next start creates a new pair |
| `control.sock` | The control socket |
| `broker.pid` | The pid of the running broker and its start time |
| `broker.lock` | Held by the running broker. A second one exits |
| `port` | The loopback port, written by the broker at first start and bound at every start after. See [The port](#the-port) |
| `start.lock` | Held by `brig` while it starts the broker |
| `audit.log`, `audit.log.1` | The [audit line](#audit-line) of every request. Rotated at 64 MiB, one previous file kept |
| `broker.log`, `broker.log.1` | The broker's own output, rotated the same way. What the broker writes to stderr, a panic included, moves to the new file with each rotation |

### Control protocol

Brig talks to the broker over HTTP on `control.sock`, with JSON bodies. A
request with a field the protocol does not have is refused with 400, and so
is a sandbox id that is not 1 to 255 letters, digits, dots, dashes and
underscores. 255 bytes is the longest name of a directory.

| Request | Body | Answer |
| --- | --- | --- |
| `POST /sandboxes` | `{id, profile, routes: [{policy, route}]}` | 204. The broker expands each route again, so a credential lifecycle comes only from a preset. The same registration again changes nothing, so the tokens of a running agent stay valid. Other routes under the same id are a new registration with a new key |
| `DELETE /sandboxes/<id>` | | `{removed, remaining}` |
| `POST /tokens` | `{sandbox, session, route, ttlSeconds}` | `{token}`. At most 12 hours |
| `GET /status` | | `{pid, port, sandboxes, pending, failing, recovering, placeholder}`. Names, and in `placeholder` the number of [placeholder](#placeholder-detection) refusals per sandbox that has one. `pending` is a refreshed secret whose store writes have not all landed. `failing` is one whose last refresh failed. `recovering` is one with a [renewal journal](secrets.md#the-renewal-journal) that a stopped broker left, that another broker holds, or that the broker could not read |
| `POST /stop` | `{ifIdle}` | `{stopping}`. With `ifIdle`, only when no sandbox is registered |

A broker on its way out answers 503 to a registration or a mint. Brig waits
for it to exit and starts another. It does the same when the broker it found
is gone by the time it registers, because another `brig` removed the last
sandbox and stopped it. A token travels only on the control socket
to Brig, and from Brig to the guest in the environment of one exec. Brig mints
one per route on every exec.

### Server limits

The broker bounds what a guest can hold open. A request header has 10
seconds to arrive, an idle connection is closed after 90 seconds, and the
request line and headers together are at most 16 KiB. A larger request gets
431 before it reaches the handler, and writes no audit line. The broker holds at most 256 connections at
once. A loopback connection names no sandbox until its request is read, so
the bound is for the host and not per sandbox. The upstream transport has no
response-header timeout, because a model can take minutes before the first
byte of an event stream.

### Credentials

The broker reads the credential of a route from Brig's own store, the login
keychain on macOS, on every request. A secret with a lifecycle, such as
`claude-credentials` under the `claude-code` preset, has one cache in the
broker. The cache refreshes it and writes it back, as
[Renewal on the host](secrets.md#renewal-on-the-host) describes, so two
sandboxes that share it share one refresh.

The broker reads the keychain with `security`, from its own session. Measured
on macOS 26: a process in a new session, whose parent had exited, read a
keychain item of the login session with no prompt. A broker started from an
SSH session can only read an unlocked keychain. A read that fails is a 502.

## Transports

A guest reaches the broker at `brig.internal`. `brig info` names the
transport in the `BROKER` row.

| Transport | Runtime | Route URL |
| --- | --- | --- |
| `brig.internal` | hull, nerdctl | `https://brig.internal/<sandbox-id>/r/<route>` |

**brig.internal.** TLS to a name only Brig serves, with the brig CA trusted
in the guest. At first start the broker creates the CA, an ECDSA P-256 key in
`ca.key`. Its name constraints permit `brig.internal` and exclude every IP
address, so a guest that trusts it trusts no other site. The broker signs a
certificate for `brig.internal` at every start, and that key stays in
memory. Brig points the guest at the CA in `SSL_CERT_FILE`,
`NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE` and `GIT_SSL_CAINFO`, at
`/brig/ca/brig.pem`.

This release does not route `brig.internal` from a guest to the broker.
`hull network-gateway` serves a static DNS record and forwards a host port
into a guest, but it has no forward from a guest to the host. So a run with a
route on hull or nerdctl is refused before boot, with "brig.internal routing
is not available on this runtime yet". The CA and the guest environment
exist, and no listener uses them (#265).

## What the guest gets

Brig mints a token per route on every exec and hands the guest the route's
`guest.env`, with `{route.url}` and `{session.token}` filled in. A token is
never on a command line:

| Preset | Guest environment |
| --- | --- |
| `claude-code` | `ANTHROPIC_BASE_URL=<route URL>`, `CLAUDE_CODE_OAUTH_TOKEN=<token>` |
| `anthropic-api-key` | `ANTHROPIC_BASE_URL=<route URL>`, `ANTHROPIC_API_KEY=<token>` |
| `github-token` | `BRIG_ROUTE_GITHUB_TOKEN=<token>`, and git configuration in `GIT_CONFIG_COUNT` |

A route with `guest.git` points git at the route through the environment,
and writes nothing into the guest home:

```
url.<route URL>/.insteadOf = https://github.com/
url.<route URL>/.insteadOf = git@github.com:
credential.<route URL>/.helper =
credential.<route URL>/.helper = !f() { test "$1" = get || exit 0; printf '%s\n' 'username=x-access-token' "password=$BRIG_ROUTE_GITHUB_TOKEN"; }; f
```

The empty helper resets the list for the route URL, so a helper from your own
git configuration is not asked first. The username is quoted for the shell.
A `GIT_CONFIG_COUNT` the profile already delivers keeps its entries, and the
route's entries are numbered after them.

The secret a route injects stays on the host, whatever the profile binds:

- a `files:` binding of it is dropped
- an `env:` binding whose chain names it is dropped whole, so an ambient
  value cannot stand in for it
- Brig does not read it. The broker does

A run with a route runs isolated.

## Audit line

The broker writes one line per request:

```
msg=broker workload=spiffe://brig.local/brig/profile/claude-code/session/refactor sandbox=brig-7f3a route=anthropic method=POST path=/v1/messages status=200 dur=1.8s
```

| Field | What it is |
| --- | --- |
| `workload` | The workload the credential was resolved for. Only a request with a valid token has one |
| `sandbox`, `route` | From the path |
| `method` | The request method, cut to 16 bytes |
| `path` | `<rest>`, without the query, cut to 256 bytes |
| `status` | The status sent to the guest |
| `dur` | The time from the request to the end of the response |
| `check` | The check that refused the request |
| `policy`, `rule`, `reason` | The refusal of a `request.allow` miss |
| `dropped.<header>` | The number of values that `inject.headers.allow` dropped |
| `renew` | After an upstream 401: `retried`, `failed`, `interval` when a renewal was forced for the secret less than a minute before, or `body` when the body was not kept for a retry |
| `where`, `encoding` | Where a [placeholder](#placeholder-detection) was found and how it was spelled. The `path` field of that line is `[redacted]`, and so is `method` when the token was in it |
| `placeholder` | `partial` when only the start of the body was scanned |

The line never holds a credential, a token, a header value or a body.

Anything on the host that reaches the broker's port can send it a request.
So the line of a request whose token has not verified carries only
`method`, `status`, `dur` and `check`, and nothing the sender wrote into the
path. Measured on a Mac, a flood of 200,000 such requests with 8 KiB paths
in 5 seconds wrote 21 MB. The broker rotates `audit.log` to `audit.log.1` at 64
MiB and keeps that one previous file, so the two take at most 128 MiB. A log
that cannot rotate keeps its file, says so once on stderr, which is
`broker.log`, and tries again at the next line.
