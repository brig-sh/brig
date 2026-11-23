# The credential broker

The broker serves the routes of [policies.md](policies.md#routes). A tool in
the guest sends its requests to the route with a session token. The broker
checks the token and the route's `request.allow`, adds the credential on the
host, and forwards the request to the route's upstream. The guest never holds
the credential. A route on the `aws` preset is the exception: it answers the
guest with temporary AWS keys. See [Routes that vend](#routes-that-vend).

`internal/broker` is the request handler, and `brig-broker` the process
that hosts it. hull on `hvi`, hull on Linux and nerdctl with
urunc reach a route at `brig.internal`, and a route that vends at
`169.254.170.2`. A pod on a Kubernetes node reaches a broker that the node's
shim started with `--listen` at `brig.internal`, over the network. See
[Running the broker on a node](#running-the-broker-on-a-node). A run bound to
a policy with routes on any other runtime is refused before boot. See
[Transports](#transports).

<p align="center">
  <img alt="A request from Claude Code through the claude-code route of the credential broker. The agent in the sandbox holds ANTHROPIC_BASE_URL, the route URL, and CLAUDE_CODE_OAUTH_TOKEN, a session token for the route. It sends its request to brig.internal on a microVM. brig-broker on the host checks the token and the route's request.allow, adds the credential from Brig's secret store, and sends the request to api.anthropic.com on a TLS connection of its own. The response comes back with the credential redacted. The credential stays on the host and never reaches the sandbox, and egress: default: deny refuses a direct connection from the sandbox." src="../assets/credential-broker.svg" width="820">
</p>

## Use it

This example keeps the Claude Code login on the host. The guest gets a
session token for one route, and never the login.

Import the login once. Brig stores it as `claude-credentials`
(see [Authentication](authentication.md)):

```bash
brig secret import claude-code
```

Write a policy with one route on the `claude-code` preset, in
`~/.config/brig/policies/claude-routed.yaml`:

```yaml
apiVersion: brig.sh/v1alpha1
kind: Policy
metadata:
  name: claude-routed
spec:
  egress:
    default: deny
  routes:
    - name: anthropic
      preset: claude-code
      credential:
        secretRef: { name: claude-credentials }
```

The sandbox can still reach the internet, so `default: deny` is what keeps
the guest to the broker. Add `allow` rules for anything else the agent
needs. See [Policy fields](policies.md#policy-fields).

Attach the policy and check it:

```bash
brig policy attach claude-routed claude-code
brig policy check claude-code
```

Routes work on Linux, and on macOS with the `hvi` backend, which
`claude-code` uses. `check` prints a row per run path, and exits non-zero
when this host cannot serve the route. Then run the agent as usual:

```bash
brig run claude-code ~/code/demo
```

`brig info claude-code` names the broker in its `BROKER` row:

```
BROKER  transport brig.internal, routes anthropic; brig-broker pid 4242 on 127.0.0.1:52546; kept on the host: claude-credentials; modes endpoint, host
```

If the secret store does not hold `claude-credentials`, or does not open,
Brig refuses the run before boot with exit `6`, and names the command that
stores the secret.

The guest holds the route URL and a session token, never the login:

```
ANTHROPIC_BASE_URL=https://brig.internal/brig-claude-code/r/anthropic
CLAUDE_CODE_OAUTH_TOKEN=<session token>
```

[What the guest gets](#what-the-guest-gets) lists everything else a route
delivers, such as the CA the guest trusts for `brig.internal`.

`brig stop claude-code` and `brig rm claude-code` revoke the sandbox's
tokens. A token that leaked out of the guest is refused from then on. The
broker exits when the last sandbox is removed.

To go back to a login in the guest, detach the policy. The next run
restarts the sandbox without the route:

```bash
brig policy detach claude-routed claude-code
```

## Request path

A request names its sandbox and its route:

```
https://brig.internal/<sandbox-id>/r/<route>/<rest>
```

`<sandbox-id>` is the runtime's instance name, the one `brig info` prints.
`<rest>` is the path the upstream receives, with the query kept. A route
that vends is reached at `http://169.254.170.2/<sandbox-id>/r/<route>`
instead. See [Transports](#transports).

The broker runs these steps in order. A request that fails one step gets the
error of that step, and the upstream is never dialed.

1. Refuse a protocol upgrade: an `Upgrade` header, `upgrade` in
   `Connection`, or the `CONNECT` method. This is a 400. An upgraded
   connection or a tunnel would be a raw pipe with the credential on it,
   and no later request on it would be checked.
2. Split the path, and check where the request arrived: on a divert
   socket, only the sandbox of that socket is served. An unknown sandbox or
   route, or another sandbox's path, is a 404. A request on the link-local
   address is served only for a route that vends. Any other route is a 404
   there, before its token is read. A [passthrough
   route](#passthrough-routes) is served only for a request that arrived on
   its sandbox's divert socket, or on the address listener from the IP the
   sandbox registered. On the loopback port it is a 404.
3. Read the session token from `Authorization: Bearer`, or from another
   carrier of the route that [Placeholder detection](#placeholder-detection)
   lists, such as the basic-auth password that git's credential helper
   sends. A passthrough route has no session token, and skips this step and
   the next.
4. Verify the token: the signature with the sandbox key, the sandbox claim
   against the path, the route claim against the route, and the expiry.
   Any failure is a 401.
5. Read the body, up to 8 MiB, and check the expiry again: the body can
   take longer to arrive than the token had left. An expired token is a
   401. Then refuse a session token anywhere but its carrier. This is a
   403. See [Placeholder detection](#placeholder-detection). A passthrough
   route has no carrier, so a session token of any route of the sandbox is
   refused wherever it is.
6. Refuse a `<rest>` that the upstream could read as another path. This is
   a 400. See [Canonical paths](#canonical-paths). A route that vends serves
   `/` alone, and any other `<rest>` is a 404.
7. Check `request.allow`. A miss is a 403 that names the rule. No
   credential has been read yet. On a route with `services`, the first
   segment of `<rest>` names the service. A service segment with any
   escape in it is a 400 with `check=path`, since the broker reads the
   service decoded and forwards the path as written. The broker refuses a
   method override and an unlisted service, then a batch request, then
   checks the service's `request.allow` against the path after the
   service, and sends the request to the service's host. See [The gcp
   preset](policies.md#the-gcp-preset).
8. On a route with `request.body`, refuse any call but `POST /v1/messages`,
   `GET /v1/models` and `GET /v1/models/{id}` with 403, and check the body of
   a `POST /v1/messages`. A compressed body is a 415. A body that is not JSON
   is a 400. A miss is a 403 in the Anthropic error shape. No credential has
   been read yet.
9. Resolve the credential through the secret broker. A failure is a 502,
   and nothing is cached. A route with no `credential`, such as a
   [certificate-only](policies.md#client-certificates) route or a passthrough
   route, skips this step.
   On a `gcp` route whose secret is a gcloud pointer, the broker mints the
   token. See [Minted tokens](#minted-tokens).
10. Edit the headers and forward the request. The response streams back as
    the upstream sends it, with the credential redacted. A route that vends
    answers with keys instead. See [Routes that vend](#routes-that-vend).
11. Write the audit line.

### Canonical paths

`request.allow` matches `<rest>` as the broker sees it. An upstream that
reads the same path another way can serve a path the list does not cover.
The broker refuses a `<rest>` with:

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
- an escaped `/`, `\` or `.`, also escaped twice, such as `%252e`. On a
  `gcp` route, an escaped `/` is taken in a Cloud Storage object name of
  the JSON API, such as `o/dir%2Ffile`. See
  [The gcp preset](policies.md#the-gcp-preset)
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
key name.

The value, the field or the whole secret, goes into a header. The broker
drops one trailing `\n` or `\r\n`. A value with any other control
character is refused with a 502.

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

A passthrough route has no `inject.headers.set` or `remove`, so the broker
keeps `Authorization` and every other header that carries the guest's own
credential. It removes the rest of the list above.

`inject.headers.allow` then keeps only the listed values of its header. A
header that holds a comma-separated list is filtered item by item, and the
values that remain are sent as one header. The audit line counts the values
dropped, without naming them.

Last, `inject.headers.set` sets each header with the credential in place of
`{value}`, or `inject.basicAuth` sets `Authorization: Basic`. `Host` is the
upstream's.

### Upstream

The broker dials the route's upstream over TLS, verified with the system
roots, except where a route's `inject.tls.ca` replaces them. It does not use a
proxy from the environment. A TLS failure is a 502, and there is no retry
without verification.

A route with `inject.tls` has a transport of its own for each sandbox. The
broker builds it from the transport that the routes without `inject.tls` use,
so the dial and timeout settings are the same. It takes over no TLS session
cache, key log or setting that skips verification, so no TLS session is shared
across sandboxes or certificates. Two sandboxes never share a connection, even
when they name the same route and the same upstream. On a route with
`inject.tls.ca`, the `ca` certificates replace the system roots. The
upstream's certificate must chain to one of them, and a certificate that
chains only to a system root is refused with a 502. A route with
`inject.tls.clientCertificate` and no `ca` still verifies the upstream with
the system roots.

The `clientCertificate` secret and the `ca` secret are resolved on each
request, the way a header credential is, and neither reaches the guest. The
broker caches each one by its bytes, so a changed secret is read again on the
next request. The client key never leaves the broker. The broker presents the
certificate and signs the handshake with the key.

The transport of a route is bound to the CA bundle and the certificate chain
it was built with, and it presents that certificate and no other. Rotating the
secret needs no restart of the sandbox or the broker. When the
`clientCertificate` secret or the `ca` secret changes, the next request builds
a new transport, and every new connection carries the new certificate and
trusts the new CA. This holds for an HTTP/2 upstream too, although HTTP/2
keeps one connection open for many requests. A request that was already
in flight finishes on the connection it started on, with the old certificate,
and that connection closes after it. A sandbox that is removed or replaced
closes its connections the same way.

A request on a route with `inject.tls` that fails a check before the dial does
not reach the upstream. An expired certificate is `tls.expired`. A certificate
that cannot be read, that is not yet valid, or whose key does not match is
`tls.certificate`, and a CA that cannot be read is `tls.ca`. A handshake that
fails is `tls.handshake`. That one happens after the dial: the upstream may
have seen a connection. Under TLS 1.3 an upstream that refuses the client
certificate says so on the first read, so the request may already have been
written on that connection. An HTTP/2 upstream reports the refusal as a
failure to establish the client connection. The upstream then closes the
connection, so a write before the broker reads the refusal fails with a
broken pipe or a reset. On a new connection the broker counts both as
`tls.handshake`. On a connection that served
requests before, the handshake finished long ago, and a reset there is
`upstream`.

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
The response has no `Content-Length`.

On a route with `request.body.toolUse`, a denied `tool_use` block in a 200
Messages answer is replaced by a refusal text, or dropped under `drop`. The
broker does this to a JSON answer as a whole, and to a stream event by event.
A rewritten JSON answer carries `Content-Length`, and a stream does not.

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

A 401 to a [minted token](#minted-tokens) goes back to the guest as it is,
with no new mint and no retry. A new mint would return the same token,
which `gcloud` caches until it is close to its expiry.

A retry sends the copy of the body that the broker read to scan for a
[session token](#placeholder-detection), up to 8 MiB. A body over 8 MiB is
not sent again, and the first 401 goes back to the guest. [Holding the
body](#holding-the-body) has the limits on what all requests hold at once.

## Placeholder detection

The session token is the guest's handle on its route. The tool in the guest
sends it in one place, the carrier: `Authorization: Bearer`, the basic-auth
password on a `basicAuth` route, `Authorization: token` on the
[`github-api` route](#the-github-api-preset), a header the route sets with `{value}`,
such as `x-api-key`, or the whole `Authorization` value on a route that
vends. Anywhere else, the agent is leaking its handle: in a prompt that
echoes the environment, a path, a query, a header of its own. So after the
token verifies, and before `request.allow` and the credential, the broker
scans the request for every live token of the sandbox. A hit is a 403, and
the upstream is never dialed:

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

Each spelling uses one case throughout.

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
so a sandbox with two routes and more than 512 execs in 12 hours drops its
oldest early. More routes reach the cap sooner. They are kept in memory only. A token of a broker that
restarted fails the signature check before the scan. A sandbox that is
removed takes its tokens and its count with it.

The tokens of other sandboxes are not looked for. The broker cannot tell
another sandbox's token in a prompt from data the agent was handed.
Refusing it would let one sandbox stop another's conversation by planting
its own token in a file the other reads.

Per live token, the broker keeps the hash of the last 32 bytes of each of
its ten spellings, and not the spellings. The scan hashes every 32-byte
window of the request once, and compares a spelling byte by byte only
where a hash matches. Those 32 bytes encode the token's MAC, so a match is
nearly always a real one, and the time is linear in the request, whatever
the number of tokens. 1024 tokens take about 540 KB.
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
  mixed case, such as `%6a%4B`, outside the path and query, JSON escapes
  other than the four above, quoted-printable, hex with separators, HTML entities, encoded twice, encrypted, or inside
  an archive in the body. Go puts a header name in canonical case, so a
  token in a name is found only in a spelling that keeps its case, such as
  lower case hex. The scan stops a leak by accident or by an injected
  prompt. An agent that means to hide its token can.
- A conversation whose history holds the token is refused on every later
  turn, since the history is sent again each time, until the token expires
  or the sandbox is removed. A new conversation works.

### Holding the body

What the broker holds of a body is taken from the replay budget, 64 MiB for
all requests at once, which `ReplayBudget` in the broker's configuration
changes:

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
| 404 | The path is not `/<sandbox-id>/r/<route>/…`, names a sandbox or route the broker does not serve, or names a sandbox other than the one whose divert socket the request arrived on, or arrived on the address listener from an IP that sandbox did not register, or from an IP no sandbox registered. On a passthrough route, a request on the loopback port. On a route that vends, any `<rest>` but `/`. On the link-local address, a route that does not vend | `path`, `arrival`, `sandbox`, `route`, `route.cleartext` |
| 401 | No token, a malformed token, a bad signature, a token of another sandbox or route, or an expired token | `token`, `signature`, `token.sandbox`, `token.route`, `expired` |
| 400 | The request body could not be read | `body` |
| 400 | On a route with `request.body`, a `POST /v1/messages` body that is not JSON, or is over 8 MiB. `check=body` is shared with the read failures above, and the `reason` attr, `json` or `size`, tells them apart | `body` |
| 408 | The part of the body the broker holds did not arrive within `BodyTimeout` | `body` |
| 503 | The replay budget, or the sandbox's share of it, could not hold the body in time | `budget` |
| 413 | A compressed body read whole decompresses to more than 8 MiB | `body` |
| 403 | A session token of the sandbox is in the request outside its carrier | `placeholder` |
| 415 | The request body has a `Content-Encoding` other than gzip, x-gzip or deflate, more than one, does not decompress, or has bytes after its compressed stream. A `POST /v1/messages` body on a route with `request.body` is refused with 415 for any `Content-Encoding` other than identity | `content-encoding` |
| 400 | `<rest>` is not in canonical form, or on a route with `services` its service segment holds an escape | `path` |
| 403 | `request.allow` does not cover the request. On a `gcp` route the rule is `spec.routes[<route>].services[<service>].request.allow.<field>` | `request.allow.methods`, `request.allow.paths`, `request.allow.repos` |
| 403 | A `request.body` rule misses on a `POST /v1/messages`: the model, a tool, `max_tokens`, or a `toolResultDeny` match. On a route with `request.body`, any call but `POST /v1/messages`, `GET /v1/models` and `GET /v1/models/{id}`, with `reason=path` | `body` |
| 403 | On a route with `services`, the path names no listed service | `request.services` |
| 403 | On a route with `services`, the request has a header `X-HTTP-Method-Override`, `X-HTTP-Method` or `X-Method-Override`, or a query parameter `$httpMethod` or `httpMethod` in any case. The rule is `spec.routes[<route>]` | `request.method-override` |
| 403 | On a route with `services`, the first path segment after the service is `batch`, in any case and with or without `;parameters`. A segment such as `batchGet` is not refused. The rule is `spec.routes[<route>].services[<service>]` | `request.batch` |
| 502 | The credential cannot be resolved, or the field it names is missing, ambiguous or not a header value. The body ends in "brig's secret store is not available on this host" when the store did not open. On a `gcp` route, the token could not be minted. See [Minted tokens](#minted-tokens) | `credential` |
| 502 | The secret is a gcloud pointer, on a route without `services`. Nothing is sent upstream | `credential.gcponly` |
| 502 | The secret is an aws pointer, on a route that does not vend. Nothing is sent upstream | `credential.vendonly` |
| 502 | The secret starts a JSON object and does not decode, such as a pointer cut short before its kind field. Nothing is sent upstream | `credential.malformed` |
| 502 | The secret is a whole JSON object, on a route without `credential.field`. Nothing is sent upstream | `credential.document` |
| 502 | The upstream cannot be reached, or on a route without `inject.tls` its certificate does not verify | `upstream` |
| 502 | A route that vends could not fetch keys. See [Routes that vend](#routes-that-vend) | `vend` |
| 502 | The upstream's certificate does not verify on a route with `inject.tls`, or the TLS handshake with it fails. The upstream refused the client certificate, over HTTP/1.1 or HTTP/2, on a route that presents one | `tls.handshake` |
| 502 | The `ca` secret cannot be resolved, or holds no CA certificate | `tls.ca` |
| 502 | The `clientCertificate` secret cannot be resolved, does not parse, is not yet valid, or its key does not match | `tls.certificate` |
| 502 | The `clientCertificate` has expired | `tls.expired` |
| 502 | The response body has a `Content-Encoding` the broker cannot read | `response.encoding` |
| 500 | The broker failed on a bug. `broker.log` has the error. When the response had already started, the connection is closed and the line keeps its status | `panic` |

A 401 body does not say which check failed. The audit line does.

On a `basicAuth` route, such as `github-token`, a 401 for the session token
also carries `WWW-Authenticate: Basic realm="brig"`. git offers credentials
only after a challenge, so without it a clone through the route fails. A route
that carries the token in a header gets no challenge.

A 502 body does not carry the error of the secret backend.

## `request.allow`

The broker enforces the four lists of `request.allow`. Every list that is
set must match.

| List | Matched against |
| --- | --- |
| `methods` | The request method |
| `paths` | `<rest>`, without the query. `*` does not match a `/` |
| `repos` | The `owner/name` that the first two segments of `<rest>` name, with a `.git` suffix removed. A path with fewer than two segments is refused |
| `graphql` | The GraphQL operation in the body of a request to `/api/graphql`, on the `github-api` preset alone. See [GraphQL requests](#graphql-requests) |

On a route with `services` each service has its own lists, and the route has
none. Apart from `graphql`, the broker does not read request bodies for
this, so a method override in a form-encoded body is not refused.

A miss returns 403 with this body, as do the refusals of an unlisted
service, a method override and a batch request on a route with `services`.
The same rule and reason go to the audit line:

```
brig broker refused this request
policy:  github-brig-sh
route:   github
rule:    spec.routes[github].request.allow.repos
reason:  repo "other-org/repo" is not in the allow list [brig-sh/*]
sandbox: brig-7f3a
```

## `request.body`

`request.body` limits a `POST /v1/messages` on the Anthropic presets. The
route serves that call, `GET /v1/models` and `GET /v1/models/{id}`, and
refuses every other call with 403 and `reason=path`. The method of
`POST /v1/messages` is read in any case, so `post` is checked too. The
broker reads the request body for the model, the tools and `max_tokens`. It
matches the `toolResultDeny` patterns against every string in the body. A
miss is a 403 in the Anthropic error shape, and the upstream is never
dialed. No credential has been read.

```
{"type":"error","error":{"type":"permission_error","message":"brig broker refused this request: model \"gpt-4o\" is not in request.body.models [claude-sonnet-4-5]"}}
```

The audit line carries the policy, the rule and the reason, with `check=body`:

```
msg=broker sandbox=brig-7f3a route=anthropic method=POST path=/v1/messages status=403 check=body policy=claude-body rule=spec.routes[anthropic].request.body.models reason=model=gpt-4o
```

The `reason` is `model=<name>`, `max_tokens=<n>`, `tool=<name>`, `text`, or
`path` for a call the route does not serve. A `text` reason never names the
text that matched. A body that is not JSON, or is over 8 MiB, is a 400 with
`check=body`, and the `reason` is `json` or `size`.

On a route with `request.body.tools`, the broker drops the `Anthropic-Beta`
values `inline-tools-2026-09-15` and `mid-conversation-tool-changes-2026-07-01`
before it forwards a request. They may carry tools inside `messages`, where
the `tools` rule does not read. The audit line counts them in
`dropped.anthropic-beta`.

## Passthrough routes

A route with `passthrough: true` holds no credential. The guest sends its
own, and the broker forwards it after the request rules. Use it when the
guest already holds the key, and the broker is there to apply `request.allow`
and `request.body`, not to keep the key from the guest.

A passthrough route takes no `credential`, and no `inject` but
`inject.headers.allow`. Its `guest.env` never holds `{session.token}`. On
the `anthropic-api-key` and `claude-code` presets, the guest gets the route
URL in `ANTHROPIC_BASE_URL` and keeps its own `ANTHROPIC_API_KEY` or
`CLAUDE_CODE_OAUTH_TOKEN`. The other presets need a credential of the
broker's, and refuse `passthrough`.

A passthrough route has no session token, so only where its request arrived
binds it to the sandbox. The broker serves it for a request on the
sandbox's divert socket, or on the address listener from the IP the sandbox
registered. A sandbox with neither is refused at registration, with a 400
that names the route. The loopback port names no sandbox, and any process on
the host can send a request there.

The rest of the request path applies as on any route: the arrival, the
canonical path, `request.allow`, `request.body`, the rewrite of denied tool
calls in the answer, and the audit line. The response is not redacted, since
the broker put no credential on the request. The preset's `Anthropic-Beta`
list still applies, since it is a request rule and not a credential. A beta
off the list, such as `mcp-client-2025-04-04`, is dropped before the request
goes upstream.

The request rules of a passthrough route hold only while egress refuses the
upstream to the guest. Otherwise the guest can send the same key to the
upstream directly. So Brig refuses a run, and `brig policy check` refuses the
policies bound to it, when the run's merged egress would let the guest reach
the upstream's host:

- No bound policy has `spec.egress`.
- The merged default is `allow`. A host `deny` under it is best effort,
  because a connection sent straight to an address asks for no name.
- An `allow` glob covers the host, and no literal `deny` names it. A `deny`
  glob is not enough: it applies only to the names the guest asks for, and
  another name the `allow` covers can resolve to an address of the host.
- Any `allow` cidr is bound. Brig cannot resolve the host before the run, so
  it cannot tell whether the cidr holds one of its addresses.

A policy whose own `egress.allow` covers the upstream of its passthrough
route is refused when it is read, with a message that names the host to
remove. A pod on a node broker is not a Brig run, and Brig does not check its
egress. Its network policy has to refuse the upstream, or the pod can send
the same key past the broker.

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
itself and asks `brig-broker`.

### Lifecycle

Brig starts `brig-broker` on the first run that has a route. It runs in a
session of its own, so it outlives the `brig` that started it and does not
take the terminal's signals. Every later run reuses it while its pid file
names a live process with the start time it recorded, since a pid alone can
be reused by an unrelated process. Brig holds `start.lock` while it checks
for a broker and starts one, so runs started at once start one broker.

`brig stop`, `brig rm` and `brig rm --all` remove each sandbox they stop
from the broker. Its tokens are refused from the next request on. They ask
the control socket whatever the pid file says, and warn
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

When brig's secret store does not open, Brig refuses a run with a route
before boot, with exit `6`. A store that stops opening after the boot
reaches the broker instead. Each route then answers 502, ending in "brig's
secret store is not available on this host", with `check=credential`, and
reaches no upstream. The broker tries the store again on each request, and
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

While the broker is down, any local process can bind its port, another
user's included on a host several people use. `brig doctor` then fails its
`broker` row and names the process that holds the port.

A broker started with `--listen` has no loopback port. It serves the routes
on the address it was given, and it neither reads nor writes `port`. A `port`
file left in the directory by an earlier start is left as it is.

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
| `port` | The loopback port, written by the broker at first start and bound at every start after. See [The port](#the-port). A broker started with `--listen` neither reads nor writes it |
| `start.lock` | Held by `brig` while it starts the broker |
| `audit.log`, `audit.log.1` | The [audit line](#audit-line) of every request and of every diverted connection. Rotated at 64 MiB, one previous file kept |
| `divert/<hash>.sock` | The divert socket of a sandbox that reaches `brig.internal`, 0600 in a 0700 directory. `<hash>` is the first 12 hex digits of the SHA-256 of the sandbox id. It is removed with the sandbox and when the broker stops |
| `broker.log`, `broker.log.1` | The broker's own output, rotated the same way. What the broker writes to stderr, a panic included, moves to the new file with each rotation |

### Control protocol

Brig talks to the broker over HTTP on `control.sock`, with JSON bodies. A
request with a field the protocol does not have is refused with 400, and so
is a sandbox id that is not 1 to 255 letters, digits, dots, dashes and
underscores. 255 bytes is the longest name of a directory.

| Request | Body | Answer |
| --- | --- | --- |
| `POST /sandboxes` | `{id, profile, routes: [{policy, route}], brigInternal, address, uid}` | `{divertSocket, ca, regions, vends}`. The broker expands each route again, so a credential lifecycle comes only from a preset. `regions` and `vends` name, for each route that vends, the region of its AWS profile and the identity it hands out. A check of such a route that fails is a 424. See [Routes that vend](#routes-that-vend). So is a mint that fails for a `gcp` route whose secret is a gcloud pointer. See [Minted tokens](#minted-tokens). The same registration again changes nothing, so the tokens of a running agent stay valid. Other routes under the same id are a new registration with a new key. `brigInternal` is the address the guest resolves `brig.internal` to. The answer then names the divert socket and the CA certificate. See [Transports](#transports). `address` is the sandbox's own IP, for a broker started with `--listen`. The answer then carries the CA certificate and no divert socket. `uid` is the UID of the pod with that address. A second sandbox with the same address is refused with 409, unless both registrations carry a `uid` and the two differ. Then the second one replaces the first. See [Running the broker on a node](#running-the-broker-on-a-node) |
| `DELETE /sandboxes/<id>` | | `{removed, remaining}`. The divert socket of the sandbox is closed and removed |
| `POST /tokens` | `{sandbox, session, route, ttlSeconds}` | `{token}`. At most 12 hours |
| `GET /status` | | `{pid, port, sandboxes, pending, failing, recovering, placeholder, vending}`. `port` is 0 for a broker started with `--listen`. Names, and in `placeholder` the number of [placeholder](#placeholder-detection) refusals per sandbox that has one. `pending` is a refreshed secret whose store writes have not all landed. A secret on a read-only store, see [Running the broker on a node](#running-the-broker-on-a-node), is never named, because no write is waiting. `failing` is one whose last refresh failed. `recovering` is one with a [renewal journal](secrets.md#the-renewal-journal) that a stopped broker left, that another broker holds, or that the broker could not read. `vending` is each `<sandbox>/<route>` of a route that vends whose last fetch or check failed, and of a `gcp` route whose last mint failed |
| `POST /stop` | `{ifIdle}` | `{stopping}`. With `ifIdle`, only when no sandbox is registered |

A broker on its way out answers 503 to a registration or a mint. Brig waits
for it to exit and starts another. It does the same when the broker it found
is gone by the time it registers. Brig waits 10 seconds for an answer. It
waits 30 seconds for a registration, and 70 seconds more for each route that vends:
the broker runs up to two `aws` calls of 30 seconds each for every one. A
token travels only on the control socket to Brig, and from Brig to the guest
in the environment of one exec.

### Server limits

The broker bounds what a guest can hold open. A request header has 10
seconds to arrive, an idle connection is closed after 90 seconds, and the
request line and headers together are at most 16 KiB. A larger request gets
431 before it reaches the handler, and writes no audit line. The broker
holds at most 256 connections at once. A loopback connection names no
sandbox until its request is read, so the bound is for the host and not per
sandbox. The upstream transport has no response-header timeout, because a
model can take minutes before the first byte of an event stream.

### Credentials

The broker reads the credential of a route from Brig's own store, the login
keychain on macOS, on every request. A secret with a lifecycle, such as
`claude-credentials` under the `claude-code` preset, has one cache in the
broker. The cache refreshes it and writes it back, as
[Renewal on the host](secrets.md#renewal-on-the-host) describes, so two
sandboxes that share it share one refresh.

A broker on a Kubernetes node reads the files of a mounted Secret instead.
See [Running the broker on a node](#running-the-broker-on-a-node).

The broker reads the keychain with `security`, from its own session. Measured
on macOS 26: a process in a new session, whose parent had exited, read a
keychain item of the login session with no prompt. A broker started from an
SSH session can only read an unlocked keychain. A read that fails is a 502.

A route that vends holds no credential in the store. Its secret points at an
AWS profile, and the broker runs the host's `aws` CLI. A `gcp` route's
secret can point at a gcloud configuration in place of a token, and the
broker runs the host's `gcloud` CLI. See [Routes that vend](#routes-that-vend)
and [Minted tokens](#minted-tokens).

## Running the broker on a node

On a Kubernetes node the broker has no keychain, no Secret Service and no
host `aws` CLI. `brig-broker` takes flags for that case. They are for a node
daemon that a shim of the platform starts and registers sandboxes with. Brig
does not pass them, and nothing here changes how `brig` starts the broker on
a workstation.

| Flag | What it does |
| --- | --- |
| `--listen <host:port>` | Serves the routes on this TCP address, with TLS, instead of on a loopback port |
| `--secret-dir <path>` | Reads every credential from the files of this directory, one file per secret, instead of from Brig's secret store |

The flags combine. A node broker is usually started with both.

### The address listener

`--listen` replaces the loopback listener. The broker serves the routes on the
address, presenting the `brig.internal` certificate its CA signs, the one the
divert sockets present. It neither reads nor writes `port`, and
`GET /status` reports `port` 0. The divert sockets keep working beside it: a
registration with `brigInternal` or a route mode besides `endpoint` still gets
one.

The broker closes a connection from an IP that no sandbox registered as soon
as it accepts it, before the TLS handshake. Every pod of the cluster can
reach the listener, and such a connection would otherwise hold one of the 256
connections the broker holds open. The TLS handshake runs when the server
first reads a connection, under the 10 seconds a request header has to
arrive. A client that never finishes it is closed then.

A pod does not need a divert socket. It resolves `brig.internal` to the
listener, trusts the CA from the answer to its registration, and sends its
requests to `https://brig.internal/<sandbox-id>/r/<route>`.

The registration says which pod. `address` is the pod's own IP, and `uid` is
the pod's UID. A Kubernetes shim knows both from the pod:

```json
{"id": "pod-7f3a", "profile": "inventory", "address": "10.244.1.17", "uid": "6f1c2a9e-4b7d-4e0a-9c2b-3d5e8f1a7b40", "routes": []}
```

The broker normalizes the IP, so an IPv4-mapped IPv6 spelling reads as the
IPv4 address. It refuses, with 400, an address that is not one IP, that is
unspecified, multicast, loopback or link-local, or that has a zone. A pod
never has a loopback or a link-local address. A connection from one comes
from the node's host network namespace, which every `hostNetwork` pod, the
kubelet and the node's daemons share, so the address would not name one pod.
It also refuses `address` with `brigInternal` or `modes`, which belong to a
divert socket, and `address` on a broker started without `--listen`.

The arrival rule is the one a divert socket applies, with the peer IP of the
connection in place of the socket:

| Request | Answer |
| --- | --- |
| The peer IP is the `address` of the sandbox the path names | Served, and the token is checked next |
| The peer IP is the `address` of another sandbox | 404, `check=arrival`, `arrived=<that sandbox>` |
| The peer IP is no sandbox's `address` | Closed at accept. A connection whose sandbox was removed while it stayed open gets 404, `check=arrival`, before any token is read |

So a pod that holds the token of another sandbox cannot use it, and cannot
tell another sandbox from one that does not exist. The address goes back when
its sandbox registers another one or is removed. The audit line carries the
sandbox the address bound the request to in `arrived`.

Two sandboxes may not register the same `address`. The CNI gives the IP of a
pod that is gone to a new pod, and the shim can miss the removal of the
first, after a crash or a lost watch event. `uid` tells the two pods apart:

| Second registration of an `address` | What the broker does |
| --- | --- |
| Both carry a `uid`, and they differ | Removes the first sandbox and registers the second. The first sandbox's tokens stop working, and `broker.log` names both pods |
| Both carry the same `uid` | Refuses the second with 409 |
| Either has no `uid` | Refuses the second with 409. The first stays bound to the address until it is removed |

The peer IP is what the network delivers, so the rule holds where the pod
network does not rewrite or share source addresses. These are its limits:

- A pod network that puts several pods behind one address, such as SNAT, or
  a pod with `hostNetwork`, cannot be told apart by it.
- A registration has no lifetime, and a pod IP is reused. A shim that does
  not send `uid` leaves a stale registration bound to the IP until it removes
  the sandbox. A token of that sandbox works from the pod that has the IP
  now, for as long as the token lives. The replacement above needs a shim
  that sends `uid`.
- A request carries no client identity besides its token. The TLS is
  server-only, and the source IP is the only binding to the sandbox.

### The secret directory

`--secret-dir` selects a read-only store of files. Each secret is one regular
file named after the secret, and its bytes are the value, a trailing newline
included. The shape is a Kubernetes Secret mounted as a volume: one file per
key. A route's `credential.secretRef.name` names the file.

```yaml
volumes:
  - name: brig-secrets
    secret:
      secretName: brig-credentials
      defaultMode: 0440
```

The broker checks the files and not the directory. The kubelet mounts a
Secret as a root-owned `0755` directory whose entries are symlinks through
`..data`, so no rule about the directory's owner or mode could pass. Each
entry is followed through its links, and these rules apply to what it
reaches:

| Entry | What the broker does |
| --- | --- |
| A regular file that is not world-readable (`mode & 0004 == 0`) | Serves it |
| A regular file that any user on the node can read | Refuses to start, and the error names the file. Set `defaultMode: 0440` on the volume |
| A name inside the secret-name grammar whose link dangles, or that cannot be read | Refuses to start, and the error names it |
| A name outside the grammar, such as `ca.crt` (letters, digits, `-` and `_`, starting with a letter; a dot does not fit) | Skips it, and `broker.log` names it once at start |
| A directory, or another entry that is not a regular file | Skips it, and `broker.log` names it once at start |
| A name that starts with a dot, such as `..data` | Ignores it. It is the kubelet's own |

There is no ownership rule. The node broker runs as root, or the Secret's
`fsGroup` makes the files readable to it. A directory that does not open is
fatal, and the broker does not start. A secret that has no file is a 502 on
the route that reads it, with `check=credential`.

The store is read-only. `Create`, `Update` and `Delete` return an error, and
nothing writes to the directory. A secret with a lifecycle is still renewed,
as [Renewal on the host](secrets.md#renewal-on-the-host) describes, with one
difference. The renewed document stays in memory, because there is nowhere to
write it. The broker logs once per secret that it cannot write the credential
back, and serves the value it has. `GET /status` does not name the secret in
`pending`, because no write is waiting. A restart starts from the file again,
and a changed file replaces the held value at the next recheck, within five
minutes. See [The renewal journal](secrets.md#the-renewal-journal).

### What a node broker refuses

A node has no host `aws` CLI and no AWS profile. A broker started with
`--listen` or `--secret-dir` refuses a registration that names a route that
vends, which is a route on the `aws` preset, with 400 at `POST /sandboxes`:

```
sandbox pod-7f3a: route aws vends AWS keys through the host's aws CLI and profile, and a broker started with --listen has neither. Use the platform's own credential vending for this workload
```

The error names the route and the flag. The broker runs no `aws`, keeps no
state for the sandbox, and does not register its other routes. The platform
gives a workload on a node its AWS keys by its own means, such as a role
bound to the pod.

A `gcp` route whose secret is a gcloud pointer is refused the same way, with
400. It runs the host's `gcloud`, and a node has no user login for it. On
GKE, `gcloud` there falls back to the node's own service account, an
identity nobody named for the sandbox:

```
sandbox pod-7f3a: route gcp mints Google tokens through the host's gcloud CLI and login, and a broker started with --secret-dir has neither. On a node, gcloud would mint as the node's own service account. Store a token the platform mints for this workload instead of a gcloud pointer
```

A `gcp` route whose secret is a token is served. A mounted Secret can change
while the broker runs, so a node broker also refuses a pointer at the
request: the answer is 502, `broker.log` carries the error, and `gcloud` does
not run.

The rest of the registration checks are the ones everywhere else. A route
mode the build does not serve, `splice` for one, is refused with 400 and
`route mode splice is not part of this build of brig`. A sandbox with
`address` is refused when it also names `brigInternal` or `modes`, and when
the broker has no `--listen`. `uid` is refused without `address`, or when it
is not letters, digits and dashes, at most 64 of them. A shared `address` is
a 409, unless both registrations carry different `uid` values.

## Routes that vend

A route on the `aws` preset answers the guest itself, and has no upstream.
AWS signs each request with the secret key, so the broker cannot add a
credential to a request on its way out. The guest's AWS SDK asks the route
for keys, as the AWS container credential provider, and calls AWS with them.
[The aws route](security.md#the-aws-route) states what that hands the guest.

The request takes the steps of [Request path](#request-path), with these
differences:

- The session token can be the whole `Authorization` value, with no scheme.
  The AWS SDK sends `AWS_CONTAINER_AUTHORIZATION_TOKEN` that way. Every other
  route answers a token sent that way with 401.
- The route serves its URL alone, with or without a trailing `/`. Any other
  path is a 404 with `check=path`, before `request.allow`.
- `request.allow` of the preset allows `GET` alone.
- The broker resolves the route's secret, a pointer to an AWS profile, and
  answers with keys. Nothing is forwarded.

The answer is the document the AWS SDKs read, with `Cache-Control:
no-store`:

```json
{"AccessKeyId":"ASIA…","SecretAccessKey":"…","Token":"…","Expiration":"2026-10-26T13:04:05Z"}
```

### Fetching keys

The broker runs the host's `aws` CLI, found on its `PATH`:

| Mode | Command |
| --- | --- |
| role | `aws sts assume-role --profile <p> --role-arn <arn> --role-session-name <sandbox-id> [--source-identity <id>] --duration-seconds <s> [--policy-arns arn=<policy>...] --output json` |
| self | `aws configure export-credentials --profile <p> --format process` |

The session name is the sandbox id, cut to 64 bytes, with each character
outside `A-Za-z0-9+=,.@_-` replaced by `-`. The keys come back on stdout. No
key is on a command line, and no error repeats stdout. `aws` gets the
broker's environment with every `AWS_` variable removed. Each call has 30
seconds. A credential with no `Expiration` is refused. `<s>` is the secret's
`--session-duration` in seconds, 1800 by default. `--policy-arns` is passed
only when the secret names session policies, one `arn=<policy>` for each.

The broker holds the keys in memory, per sandbox and route. A request gets
the keys held while they have at least 20 minutes left. Below that, the
broker fetches new ones. Requests that arrive during a fetch wait for it, so
a burst of requests is one call, and each gets the keys as `aws` returned
them. Self mode can return keys with less than 20 minutes left. The broker
serves those for one minute after the fetch while they are valid, so it runs
`aws` at most once a minute per route. A failed fetch is an error even when
older keys are held, since those are about to expire. After the secret is
imported again with another profile, mode, role, session length or list of
session policies, the next request fetches new keys. A sandbox removed from
the broker takes its keys with it.

A fetch that fails is a 502 with `check=vend`:

| Failure | Body |
| --- | --- |
| `aws` reports that the profile's SSO login has expired | names the fix, `aws sso login --profile <p>` |
| No `aws` on the broker's `PATH` | names the AWS CLI v2 install page |
| Anything else | `brig broker: the credentials of route <route> could not be fetched; see the broker log` |

The error of the last row goes to `broker.log`, with the sandbox and the
route, and never to the guest or to the audit log. It can name paths of the
host.

### Registration

A broker started with `--listen` or `--secret-dir` refuses a route that vends
at registration, before any of this. See
[What a node broker refuses](#what-a-node-broker-refuses).

A registration checks every route that vends before the broker answers,
also when it is the same registration again. The broker resolves the secret,
fetches keys when those it holds have less than 15 minutes left, and reads
the profile's region. So a new run that cannot get AWS credentials is
refused at its start, and not at the agent's first AWS call. An exec into a
registered sandbox whose held keys have 15 minutes or more left fetches no
keys, so a lapsed SSO login does not refuse it.

A check that fails is a 424 with `X-Brig-Refusal: credential`, and the body
`route <name>: <message>`. Brig refuses the run or the exec with exit `6`
and the broker's words. A 424 without the header is not read as a
credential to fix. A sandbox that
was not registered before leaves nothing behind. One that was keeps its
registration, and `GET /status` names the route in `vending` until a fetch
works.

The answer names, for each route that vends, the region and the identity.
Brig hands the region to the guest as `AWS_REGION` and
`AWS_DEFAULT_REGION`, and refuses the run when the answer names none. It
prints one line per route, which `-q` hides:

```
brig: route aws: AWS role arn:aws:iam::111122223333:role/brig-coding-agent, assumed from your profile dev
brig: route aws: AWS role arn:aws:iam::111122223333:role/brig-coding-agent with session policies arn:aws:iam::111122223333:policy/brig-read-ec2, assumed from your profile dev
brig: route aws: AWS as your own identity (profile dev); the sandbox can do what you can
```

## Minted tokens

A `gcp` route forwards each request and injects a Google access token, as
[The gcp preset](policies.md#the-gcp-preset) describes. Its secret is either
that token, stored as it is, or a pointer at a gcloud configuration on the
host, which `brig secret import` writes. See
[A gcloud login behind a route](secrets.md#a-gcloud-login-behind-a-route).
For a pointer, the broker mints the token itself. The guest never sees the
token or the pointer.

A pointer is minted on a route with `services` alone. On any other route the
broker answers 502 with `check=credential.gcponly` and sends nothing
upstream, since the pointer names your gcloud configuration and service
account. A broker started with `--listen` or `--secret-dir` mints no pointer.
See [What a node broker refuses](#what-a-node-broker-refuses).

### Minting a token

The broker runs the host's `gcloud` CLI, found on its `PATH`:

| Mode | Command |
| --- | --- |
| impersonate | `gcloud auth print-access-token --configuration <c> --impersonate-service-account <email> --format=json --no-log-http --verbosity=error` |
| self | `gcloud auth print-access-token <account> --configuration <c> --format=json --no-log-http --verbosity=error` |

The token comes back on stdout. No token is on a command line, and no error
repeats stdout. `gcloud` gets the broker's environment without the variables
that pick an identity or set HTTP logging or the log level, and with prompts
turned off. Each call has 30 seconds.

`gcloud` does not print when the token expires. The broker POSTs the token
in a form body to `https://oauth2.googleapis.com/tokeninfo`, from the host,
and reads `expires_in` and `email`. It does not follow a redirect, and the
call has 10 seconds. A token with no `expires_in` is refused. In
impersonate mode, a token whose `email` is missing or is another identity is
refused. gcloud mints an impersonated token with the `userinfo.email` scope,
so tokeninfo names the service account (checked with gcloud 591). In self
mode, `<account>` is the account the import recorded, and a token whose
`email` is missing or is another account is refused. A user login's token
carries the `userinfo.email` scope, so tokeninfo names its account.

The broker holds the tokens in memory, per sandbox and route, and writes none
to the store, to a file or to a log. A request gets the token held while it
has at least 2 minutes left. Below that, the broker mints a new one.
Requests that arrive during a mint wait for it, so a burst of requests is
one call. A request that gives up does not stop the mint, which fills the
cache for the next one. `gcloud` hands back its own cached token until that
token is close to its expiry, so a mint can return one with less than 2
minutes left. The broker serves it for one minute after the mint while it is
valid. A failed mint is an error even when an older token is held. After the
secret is imported again with another configuration, mode or service
account, the next request mints a new token. A sandbox removed from the
broker takes its tokens with it.

The token lives as long as `gcloud` makes it, about an hour. Brig does not
set its lifetime.

A mint that fails is a 502 with `check=credential`:

| Failure | Body |
| --- | --- |
| `gcloud` reports that the configuration's login must be done again | names the fix, `gcloud auth login --configuration <c>`, or in self mode `gcloud auth login <account> --configuration <c>` |
| `gcloud` reports that your account may not impersonate the service account | names the role it needs, `roles/iam.serviceAccountTokenCreator` |
| No `gcloud` on the broker's `PATH` | names the Google Cloud CLI install page |
| Anything else | `brig broker: the credential of route <route> could not be minted; see the broker log` |

The error of the last row goes to `broker.log`, with the sandbox and the
route, and never to the guest or to the audit log. `GET /status` names the
route in `vending` until a mint works.

An upstream 401 goes back to the guest. See
[Retry after a 401](#retry-after-a-401).

### Registration of a gcp route

A registration checks every `gcp` route whose secret is a pointer, after the
routes that vend, also when it is the same registration again. The broker
resolves the secret and mints a token when the one it holds has less than 2
minutes left. So a new run on a lapsed gcloud login is refused at its start,
and not at the agent's first Google call. A `gcp` route whose secret is a
static token runs no `gcloud`, and a secret that does not resolve is left to
the request, as on any route.

A check that fails is a 424 with `X-Brig-Refusal: credential` and the body
`route <name>: <message>`, as for a route that vends. Brig refuses the run
or the exec with exit `6`. A sandbox that was not registered before leaves
no tokens behind. One that was keeps its registration, and `GET /status`
names the route in `vending` until a mint works.

The message goes to `brig` on the host, not to the guest, so it can quote
`gcloud`'s stderr, clipped, where a request in the guest gets only the
fixed text of the table above.

## Route modes

A route's `mode` says how the tool in the guest reaches it. `endpoint`, the
default, points the tool at the route URL. The broker adds the credential
and sends the request upstream on a TLS connection of its own.

An `endpoint` route can never:

- leave the tool as it is. The tool is pointed at the route URL, and on
  hull and nerdctl it trusts the brig CA for `brig.internal`.
- serve a tool that pins the upstream's certificate, or that has no setting
  for its endpoint. A `host` route serves a tool of this kind that takes a
  host name, such as `gh`: see [The github-api preset](#the-github-api-preset).
- keep one session from the tool to the upstream. There are two: the
  tool's to the broker, and the broker's to the upstream.
- leave a request untouched. The broker reads and forwards every request.

`host` is an `endpoint` route that the guest reaches at its own name,
`https://<route>.brig.internal`, for a tool that takes a host name and no
path. The name of a `host` route is a DNS label: at most 63
characters, and it does not end with a dash. The broker takes the route from
the host label and the sandbox from the divert socket, and forwards the whole
request path. A `host` route is reached at its name only: the path form
`https://brig.internal/<sandbox-id>/r/<route>` is a `404` with
`check=route.mode`, and so is a host name that names an `endpoint` route. The
runtime answers the name with the `brig.internal` address, and the upstream
of a `host` route is not diverted. `host` runs where `endpoint` runs.

This build of Brig serves the `endpoint` and `host` modes alone. A policy
that names another mode still parses. `brig policy check` refuses the route, and a run
refuses it before boot with exit `7`:

```
brig: policy claude-relay (route anthropic) declares a route in mode relay: a policy applies to this sandbox, and hull on hvi cannot enforce the route mode relay: route mode relay is not part of this build of brig. Use mode: endpoint, or detach the policy. brig will not boot a sandbox under a policy nothing enforces
```

A route in mode `endpoint` or `host` is refused where its run path cannot reach the
broker:

| Where | What is refused |
| --- | --- |
| `brig policy check` | a route that the run would refuse, on the runtime and hypervisor the run would use. It prints a row per run path and exits non-zero |
| `brig run`, `brig sh` and `brig plan` | a route on hull on `vz` or `qemu`, on docker, on hull or nerdctl with no network, or on a hull whose gateway takes no `--divert`. A route that vends on a hull whose gateway cannot divert `169.254.170.2`. Nothing is started, and Brig exits `7`. See [Route mode answers](runtimes.md#route-mode-answers) |

`brig info` names the modes the run path serves in its `BROKER` row.

## Transports

`brig info` names the transport in its `BROKER` row.

| Transport | Runtime | Route URL |
| --- | --- | --- |
| `brig.internal` | hull on `hvi`, hull on Linux, nerdctl with urunc | `https://brig.internal/<sandbox-id>/r/<route>`, or `https://<route>.brig.internal` for a `host` route |
| link-local | hull on `hvi`, hull on Linux, nerdctl with urunc, for a route that vends | `http://169.254.170.2/<sandbox-id>/r/<route>` |

**brig.internal.** TLS to a name only Brig serves, with the brig CA trusted
in the guest. At first start the broker creates the CA, an ECDSA P-256 key in
`ca.key`. Its name constraints permit `brig.internal` and exclude every IP
address, so a guest that trusts it trusts no other site. The broker signs a
certificate for `brig.internal` and `*.brig.internal` at every start, and
that key stays in memory. A broker that runs for 23 days signs a new one.

The name resolves to the sandbox's own gateway, and its port 443 reaches the
broker through the sandbox's divert socket:

| Runtime | Name | Port 443 | Other ports of the address |
| --- | --- | --- | --- |
| hull on `hvi` and hull on Linux | a `--host` record of the gateway's address on `hull network-gateway` | a `--divert` of the address to the divert socket | reset. The gateway never dials its own address |
| nerdctl | a record the egress resolver answers with the bridge's address, whatever the policy | the sandbox's nftables table redirects it to the relay of the resolver. See [Routing brig.internal](policies.md#routing-briginternal) | refused by the table |

The broker learns the address when Brig registers the sandbox. A diverted
connection to it is served as `brig.internal` and never dialed. The broker
completes the TLS handshake with the `brig.internal` certificate, offers
HTTP/1.1, and hands the connection to the route handler of the loopback
port. Each request carries the sandbox whose socket it arrived on. The
arrival check refuses a request for another sandbox with `404` and
`check=arrival`, before the sandbox is looked up or a token is read, so a
guest cannot tell another sandbox from one that does not exist. A sandbox
registered for `brig.internal` is refused on the loopback port the same way.

Brig writes the CA certificate into the guest after the boot, and again on
every exec, through stdin. The broker hands it over at registration, and Brig
refuses anything but one certificate of a CA constrained to `brig.internal`.
The key never leaves the broker. The guest gets two files, root's and 0644,
and its own trust store is left as it is:

| File | What it holds | Variables |
| --- | --- | --- |
| `/brig/ca/brig.pem` | the CA certificate | `NODE_EXTRA_CA_CERTS`, which Node adds to its roots |
| `/brig/ca/bundle.pem` | the image's own roots, then the CA | `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `GIT_SSL_CAINFO`, which replace a client's roots |

The bundle keeps every other site trusted for a client that reads one file.
Brig takes the first of `/etc/ssl/certs/ca-certificates.crt`,
`/etc/pki/tls/certs/ca-bundle.crt` and `/etc/ssl/cert.pem` that the image
has. An image with none gets the CA alone.

`brig.internal` needs a network of the sandbox's own. hull starts a gateway
for the sandbox whatever the posture. On nerdctl, a run with a route always
takes the `isolated` posture, which gives the sandbox a network of its own.
hull on `vz` and `qemu` takes its network from vmnet, and docker manages its
own bridges, so neither has a gateway to answer the name on. A
run with a route there is refused before boot, with exit `7`.

**The address listener.** A pod has no divert socket. A broker started with
`--listen` serves the same routes on a TCP address, with TLS and the same
`brig.internal` certificate. The registration of a sandbox carries `address`,
the pod's own IP, and the answer carries the CA the pod trusts. The platform
resolves `brig.internal` to the listener. Each request carries the IP its
connection came from, and the arrival check binds it to the sandbox registered
with that IP, as a divert socket binds it to its own sandbox. See
[Running the broker on a node](#running-the-broker-on-a-node).

**link-local.** Plain HTTP to `169.254.170.2` on port 80, the address of
the AWS container credential provider. The AWS SDKs need it:

- botocore, which boto3 and the AWS CLI v2 use, verifies HTTPS to a
  container credential URL with its own CA list. It ignores
  `AWS_CA_BUNDLE`, so it cannot trust the brig CA for `brig.internal`.
- The SDKs take plain HTTP for that URL only to loopback, `localhost`,
  `169.254.170.2`, `169.254.170.23` and `fd00:ec2::23`. They check the host
  as it is written in the URL.

A run with a route that vends diverts that address and port to the
sandbox's divert socket, beside `brig.internal`. A run without one diverts
no link-local address. The broker serves a diverted connection to it in
plain HTTP, with the route handler of `brig.internal`, and never dials it.
Each request carries the sandbox whose socket it arrived on, and the
arrival check applies as on `brig.internal`. The broker closes a connection
to any other port of the address.

On this path the session token crosses the virtual link from the guest to
the host in clear text. The broker serves only a route that vends there,
and answers any other route with `404` and `check=route.cleartext`, before it
reads the token or the credential. So a token of a route that forwards
opens nothing on that path. [The aws route](security.md#the-aws-route)
states the bounds of the token that does cross.

| Runtime | Port 80 | Other ports, and the rest of `169.254.0.0/16` |
| --- | --- | --- |
| hull on `hvi` and hull on Linux | a `--divert` of the address to the divert socket | reset by the gateway |
| nerdctl | the sandbox's nftables table redirects it to the relay of the resolver. See [Routing brig.internal](policies.md#routing-briginternal) | refused by the table under a policy with a default |

No `allow` rule is needed for the address, and Brig adds none under
`default: deny`. On hull, the gateway must take a divert of a link-local
address, which needs hull
[v0.1.0](https://github.com/brig-sh/hull/releases/tag/v0.1.0) or later.
`install.sh` installs the newest hull release unless `HULL_VERSION` pins
another. An older gateway refuses the divert only when it starts, so Brig
asks hull whether its gateway takes the divert before it boots a run with a
route that vends. It refuses an older hull with exit `7` and the remedy
"Upgrade hull, or detach the policy", and a probe with any other answer as
`unknown`. See [Every command Brig
runs](runtimes.md#every-command-brig-runs).

**The divert socket.** The runtime hands a guest connection to the
`brig.internal` address, or to the link-local address, to the divert socket
of the sandbox, instead of dialing it. The diverter connects once per guest
connection, and writes one line first:

```
BRIG-DIVERT/1 <dst-ip> <dst-port> <src-ip> <src-port>\n
```

The addresses are in the form Go's `netip.Addr` prints, IPv6 without
brackets and without a zone. The ports are decimal, with no leading zero.
The line is at most 160 bytes with its newline. After it come the guest's
bytes, both ways, unchanged. The broker writes nothing back before it
serves. It refuses by closing the socket, and the diverter then resets the
guest's connection. Each side passes a half-close on.

The broker holds at most 64 diverted connections of one sandbox open at
once, and 512 of every sandbox together. A connection over either is closed.

## What the guest gets

Brig mints a token per route on every exec and hands the guest the route's
`guest.env`, with `{route.url}`, `{session.token}`, `{route.region}` and
`{route.host}` filled in. A token is never on a command line. A guest that reaches
`brig.internal` also gets the CA variables of [Transports](#transports). Per
preset:

| Preset | Guest environment |
| --- | --- |
| `claude-code` | `ANTHROPIC_BASE_URL=<route URL>`, `CLAUDE_CODE_OAUTH_TOKEN=<token>` |
| `anthropic-api-key` | `ANTHROPIC_BASE_URL=<route URL>`, `ANTHROPIC_API_KEY=<token>` |
| `github-token` | `BRIG_ROUTE_GITHUB_TOKEN=<token>`, and git configuration in `GIT_CONFIG_COUNT` |
| `github-api` | `GH_HOST=github-api.brig.internal`, `GH_ENTERPRISE_TOKEN=<token>` |
| `aws` | `AWS_CONTAINER_CREDENTIALS_FULL_URI=<route URL>`, `AWS_CONTAINER_AUTHORIZATION_TOKEN=<token>`, `AWS_REGION` and `AWS_DEFAULT_REGION=<the region of the AWS profile>`. The route URL is `http://169.254.170.2/<sandbox>/r/<route>` |
| `gcp` | `BRIG_GCP_URL=<route URL>`, `BRIG_GCP_TOKEN=<token>`, `CLOUDSDK_API_ENDPOINT_OVERRIDES_<SERVICE>=<route URL>/<service>/<path>` for each service, and, on the `brig.internal` transport, `CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE` |

### The github-api preset

`github-api` is a `host` route for `gh`. `gh` takes no API URL for
github.com, so `GH_HOST` names the route and `gh` treats it as a GitHub
Enterprise host, with the token in `GH_ENTERPRISE_TOKEN`. The broker sends
the request to `https://api.github.com` with `Authorization: Bearer` and the
credential. `gh` sends its token as `Authorization: token <token>`. On this
preset only, the broker reads that scheme, case-insensitively, as the session
token. On any other route the scheme is not read and the request is a `401`.
The preset is for `api.github.com`: its path mapping assumes that host, so an
`upstream` that points at a GitHub Enterprise server does not work through it.

The broker forwards only the paths `gh` sends to an Enterprise host:

| `gh` sends | Forwarded as |
| --- | --- |
| `/api/v3` | `/` |
| `/api/v3/<path>` | `/<path>` |
| `/api/graphql` | `/graphql` |

Any other path is a `404` with `check=github-api.path`, before the broker reads
the credential. `request.allow.paths` matches the path `gh` sent, such as
`/api/v3/repos/*` or `/api/graphql`. `request.allow.repos` is refused on this
preset. A path rule cannot tell one GraphQL request from another; to limit
them, use `request.allow.graphql` (see [GraphQL requests](#graphql-requests)).

Select a repository with `gh ... -R <route>.brig.internal/<owner>/<repo>`, such
as `github-api.brig.internal/<owner>/<repo>` for a route named `github-api`,
because the host `gh` knows is the name of the route. git clone and push keep
using the `github-token` route. `gh repo clone` and `gh pr checkout` are not
served by `github-api`, and neither is `uploads.github.com`.

#### GraphQL requests

Most `gh pr` and `gh issue` commands send GraphQL: one `POST /api/graphql`
that can read or change anything the token reaches. `request.allow.graphql`
limits the operation that request carries:

```yaml
request:
  allow:
    graphql:
      queries: [repository]
      mutations: [addComment]
      repos: [brig-sh/brig]
```

| List | Matched against |
| --- | --- |
| `queries` | Each top-level field of a query, through fragments, by field name and not alias. `*` allows any |
| `mutations` | Each mutation of a mutation, the same way. `*` allows any |
| `repos` | The `owner` and `name` of each `repository` field at the root of a query, without ASCII case, and the repository of each node ID a mutation names |

A list that is left out allows nothing, so a route without `mutations` is
read-only. `__typename` is always allowed, and so are `__type` and
`__schema` in a query: they read the schema, not a repository. The broker
refuses subscriptions. With `repos`, `queries` may list only `repository`
and `rateLimit`, because every other root field reaches repositories by a
node ID, a search, a URL or an owner, which `repos` cannot check.

**On a route with `request.allow.graphql`:**

- Only `graphql` governs `/api/graphql`. `methods` and `paths` do not apply
  to it.
- Every other path is the REST API. The broker allows only `GET` and `HEAD`
  there, narrowed by `methods` and `paths`. This stops `gh api -X PUT
  .../merge` from merging a pull request that GraphQL refuses to merge.
- The broker refuses a path that differs from `/api/graphql` only in case, a
  slash, or a segment after it, such as `/api/v3/GraphQL`.

**What the broker reads.** The broker checks the request before it reads
the credential, and refuses a request that it cannot read whole:

- a `POST` of `application/json`, not compressed, with no query string, and
  at most 8 MiB
- one JSON object with only `query`, `operationName` and `variables`, no key
  twice at any depth, nested at most 64 deep, in UTF-8
- a query in the GraphQL specification of October 2021, at most 64 deep and
  50,000 tokens, with no name of an operation, fragment, variable, argument
  or input field twice, no undefined fragment, and no fragment cycle

The broker picks the operation as GitHub does: the one `operationName`
names, or the only one. A repository name must be ASCII, from a string or a
variable, and not a block string.

**Renamed and moved repositories.** GitHub resolves the old name of a
renamed or transferred repository to its new one, so a name that matches
`repos` can reach a repository it never listed. With `repos`, the broker
asks GitHub, with the route's credential, whether each repository exists
under exactly that name (`followRenames: false`) before it forwards the
request. It keeps each confirmation for five minutes for each route.

**Mutations.** A mutation names its target by node ID, such as
`subjectId: "PR_kw..."`, and not by repository. With `repos`, the broker
reads every node ID in each mutation's arguments: each argument or input
field named `id`, `ids`, `inReplyTo`, `allowedActors` or `reviewers`, or
ending in `Id` or `Ids`, except `clientMutationId`, through input objects,
lists, variables and their defaults. It asks GitHub for the repository of each ID with one
`nodes(ids:)` query, with the route's credential, and forwards the mutation
only when every ID belongs to a listed repository. An exact `owner/name`
matches by repository ID, so a renamed repository matches only its current
name. A glob matches the current `owner/name`.

A pull request has two repositories: the base, and the head that
`updatePullRequestBranch` writes to. Both must be listed, so a mutation on
a pull request from a fork is refused unless `repos` also lists the fork.

The broker refuses:

- an ID that does not resolve, or that belongs to no repository, such as a
  user
- an ID field whose value is not a string or a list of strings
- a field that names its target by name or by URL, such as
  `repositoryNameWithOwner` or `subIssueUrl`, since only an ID can be looked
  up. This includes fields such as `homepageUrl` that name no target
- a `headRefName` written as `owner:branch`, which names a branch in a fork;
  use `headRepositoryId`
- a mutation that names no node ID, because nothing ties it to a repository
- more than 100 node IDs in one request

The broker keeps the repository of each ID for five minutes for each route,
so an issue transferred to another repository is seen within that time. The
audit line records each ID and its repository as
`resolved="I_kw...=owner/name"`, and the head of a pull request from another
repository as `PR_kw....head=owner/name`. The refusal sent to the guest does
not name the repository, so a guest cannot use it to learn where a node ID
is.

A lookup that fails, because GitHub cannot be reached or answers with an
error, is a `502` with `check=upstream`, as for a request that cannot reach
its upstream. The cause is on the audit line, not in the response.

A refusal is a `403` that names the rule:

```
brig broker refused this request
policy:  gh-pr-comment
route:   github-api
rule:    spec.routes[github-api].request.allow.graphql
reason:  mutation field "mergePullRequest" is not in the allow list [addComment]
sandbox: brig-7f3a
```

**Which `gh` commands work.** With the example policy and `gh` 2.103:

| Command | Result |
| --- | --- |
| `gh pr list`, `gh pr view`, `gh issue list`, `gh issue view` | Allowed |
| `gh pr comment`, `gh issue comment` | Allowed: `addComment` |
| `gh pr checks` | Refused: its query for the checks selects `node` |
| `gh pr status` | Refused: it asks for the current user with `viewer`, then selects `search` |
| `gh pr merge`, `gh pr create`, `gh issue close` and other changes | Refused, until their mutations are listed |
| `gh api` on a REST path | `GET` alone |

`gh pr checks` works without `repos`, with `node` in `queries`, and `gh pr
status` with `viewer` and `search`.

Because `GH_HOST` names the route, `gh` treats it as a GitHub Enterprise
host. It reads `GET /api/v3/meta`, and before `gh pr checks`, `gh pr
status`, `gh pr merge` and `gh issue view` it asks the schema which fields a
type has, with `__type`. The broker allows both, so the rule that refuses a
command is the one for the fields or mutation the command needs.

To find the fields and mutations a command needs, run it in the sandbox
with `GH_DEBUG=api`, which prints each query. On the host, `gh` talks to
github.com and skips the schema queries.

**What `graphql` does not limit.**

- `repos` limits where a query starts and what a mutation changes, not
  what either reads. A query that starts at a listed repository can follow
  an edge, such as an issue's `parent`, a pull request's `headRepository`,
  or a cross-reference, into another repository. `gh pr view` and `gh issue
  view` select such edges. A mutation's response can select the same edges.
- A closing keyword in a pull request or commit, such as `Fixes
  other/repo#5`, closes an issue in another repository when the pull
  request merges. The broker sees only the text.
- The REST API stays readable on every path that `paths` allows.

For each of these, the token's scope is the only limit. Use a fine-grained
token that covers only the repositories and permissions the agent needs.

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

A run with a `github-token` or `github-api` route also withholds `GH_TOKEN`
and `GITHUB_TOKEN`, whatever secret or variable the profile reads them from.
The route keeps the token on the host, and forwarding it as well would put
the real token in the guest beside the route's session token:

```
brig: not forwarding GH_TOKEN: this sandbox reaches GitHub through route github, which keeps the token on the host
```

The guest's git credential helper reads `GH_TOKEN`. With a `github-api`
route and no `github-token` route, git over HTTPS to github.com gets no
token, and the notice says that git needs a `github-token` route. A preset
route whose `upstream` is not the preset's own does not reach GitHub and
does not count.

A policy bound to one session with `brig policy attach -n` counts. To forward
the token anyway, set `BRIG_FORWARD_GITHUB_TOKEN=1`; Brig then says that the real
token is in the guest beside the route. Without a GitHub route, the shipped
profiles forward `GH_TOKEN` as before, and `brig info` says that a route
would keep it on the host.

A run with a route runs isolated. A run with a route and the `offline`
posture is refused.

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
| `check` | The check that refused the request. `body` is a `request.body` refusal or a body that is not JSON, and `tool_use` is a denied tool call that the broker rewrote in the answer |
| `arrived` | The sandbox whose divert socket the request arrived on, or, on the address listener, the sandbox registered with the IP of its connection. A request on the loopback port has none, and so does one from an IP no sandbox registered |
| `policy`, `rule`, `reason` | The refusal of a `request.allow` miss, and of a `request.body` refusal, which shares these three fields. A `request.body` refusal has `check=body`, and `reason` is `model=<name>`, `max_tokens=<n>`, `tool=<name>`, `text` or `path`. `check=tool_use` is written once per denied `tool_use` block that the broker rewrites, with `reason=tool=<name>` |
| `dropped.<header>` | The number of values that `inject.headers.allow` dropped. On a route with `request.body.tools`, `dropped.anthropic-beta` also counts the tool betas the broker drops |
| `cert` | The fingerprint of the client certificate that the connection of the request presented, `sha256:` and 16 hex digits. Only on a route with `inject.tls.clientCertificate` |
| `cert.notAfter` | The expiry of that certificate, in UTC. Written with `cert` |
| `renew` | After an upstream 401: `retried`, `failed`, `interval` when a renewal was forced for the secret less than a minute before, or `body` when the body was not kept for a retry |
| `where`, `encoding` | Where a [placeholder](#placeholder-detection) was found and how it was spelled. The `path` field of that line is `[redacted]`, and so is `method` when the token was in it |
| `placeholder` | `partial` when only the start of the body was scanned |
| `vended` | The access key id a route that vends answered with. Never the secret key or the session token |

The line never holds a credential, a token, a header value or a body.

The line never holds the client key.

A diverted connection writes one line of its own when it ends. On
`brig.internal` and `link-local`, the requests it carried have lines of their
own beside it:

```
msg=divert sandbox=brig-7f3a dst=198.18.1.1:443 transport=brig.internal dur=41s
```

| Field | What it is |
| --- | --- |
| `sandbox` | The sandbox of the divert socket |
| `dst` | The address and port the guest dialed |
| `transport` | `brig.internal` for a connection to the sandbox's `brig.internal` address, `link-local` for one to `169.254.170.2` |
| `check` | Why the connection was not served: `preamble`, `capacity`, `capacity.host` or `port`, on `brig.internal`, `brig.internal.port`, `tls` or `stopping`, and on `link-local`, `link-local.port` or `stopping` |

Anything on the host that reaches the broker's port can send it a request.
So the line of a request whose token has not verified carries only
`method`, `status`, `dur`, `check` and `arrived`, and nothing the sender
wrote into the path. Measured on a Mac, a flood of 200,000 such requests
with 8 KiB paths in 5 seconds wrote 21 MB. The broker rotates `audit.log`
to `audit.log.1` at 64 MiB and keeps that one previous file, so the two take
at most 128 MiB. A log
that cannot rotate keeps its file, says so once on stderr, which is
`broker.log`, and tries again at the next line.
