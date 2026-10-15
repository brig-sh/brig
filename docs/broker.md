# The credential broker

The broker serves the routes of [policies.md](policies.md#routes). A tool in
the guest sends its requests to the route with a session token. The broker
checks the token and the route's `request.allow`, adds the credential on the
host, and forwards the request to the route's upstream. The guest never holds
the credential.

This page describes `internal/broker`, the request handler. This release does
not run it yet, and a run bound to a policy with routes is still refused. The
process that hosts the handler, serves TLS for `brig.internal` and mints the
tokens comes in a later change.

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
5. Refuse a `<rest>` that the upstream could read as another path. This is
   a 400. See [Canonical paths](#canonical-paths).
6. Check `request.allow`. A miss is a 403 with the refusal below. No
   credential has been read yet.
7. Resolve the credential through the secret broker. A failure is a 502,
   and nothing is cached.
8. Edit the headers and forward the request. The response streams back as
   the upstream sends it, with the credential redacted.
9. Write the audit line.

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

The broker keeps a copy of the request body for the retry, up to 8 MiB per
request. All requests together hold at most 64 MiB, which `ReplayBudget` in
the broker's configuration changes. A body over either limit is not sent
again, and the first 401 goes back to the guest. So does a 401 that the
upstream sends before it has read the whole body.

## Errors

| Status | When | `check=` on the audit line |
| --- | --- | --- |
| 400 | The request asks for a protocol upgrade, or is a `CONNECT` | `upgrade` |
| 404 | The path is not `/<sandbox-id>/r/<route>/…`, or names a sandbox or route the broker does not serve | `path`, `sandbox`, `route` |
| 401 | No token, a malformed token, a bad signature, a token of another sandbox or route, an expired token, or a request that arrived on another sandbox's gateway | `token`, `signature`, `token.sandbox`, `token.route`, `expired`, `arrival` |
| 400 | `<rest>` is not in canonical form | `path` |
| 403 | `request.allow` does not cover the request | `request.allow.methods`, `request.allow.paths`, `request.allow.repos` |
| 502 | The credential cannot be resolved, or the field it names is missing, ambiguous or not a header value | `credential` |
| 502 | The upstream cannot be reached, or its certificate does not verify | `upstream` |
| 502 | The response body has a `Content-Encoding` the broker cannot read | `response.encoding` |

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

The process that hosts the broker bounds what a guest can hold open. It sets
a read-header timeout and an idle timeout on its server, and limits the
connections per sandbox. The upstream transport has no response-header
timeout, because a model can take minutes before the first byte of an event
stream.

## Audit line

The broker writes one line per request:

```
msg=broker workload=spiffe://brig.local/brig/profile/claude-code/session/refactor sandbox=brig-7f3a route=anthropic method=POST path=/v1/messages status=200 dur=1.8s
```

| Field | What it is |
| --- | --- |
| `workload` | The workload the credential was resolved for. Only a request with a valid token has one |
| `sandbox`, `route` | From the path |
| `method` | The request method |
| `path` | `<rest>`, without the query. Empty when the request is refused before its path is split (`check=upgrade`, or `check=path` with a 404) |
| `status` | The status sent to the guest |
| `dur` | The time from the request to the end of the response |
| `check` | The check that refused the request |
| `policy`, `rule`, `reason` | The refusal of a `request.allow` miss |
| `dropped.<header>` | The number of values that `inject.headers.allow` dropped |
| `renew` | After an upstream 401: `retried`, `failed`, `interval` when a renewal was forced for the secret less than a minute before, or `body` when the body was not kept for a retry |

The line never holds a credential, a token, a header value or a body.
