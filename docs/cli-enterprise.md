# Brig Enterprise command line reference

Brig has two editions, built from the same source. Brig Community is the
Apache 2.0 edition that [Command line reference](cli.md) documents. Brig
Enterprise adds the commands on this page, which sign you in to NOFire AI.
Its source is in [enterprise/](../enterprise/), under its own license.

`brig version` names the edition you have:

```console
$ brig version
brig v0.3.0 (91b0c7b, 2026-09-26, go1.26.0, darwin/arm64, enterprise)
```

All other verbs, flags, exit codes and environment variables are the same in
both editions.

## Verbs

### `brig login`

```bash
brig login [URL] [--browser | --device] [--ttl DURATION]
```

`brig login` signs you in to a NOFire AI deployment and stores the token in
your keychain, one token for each deployment. No other command needs it. To
log an agent in, see [Logging an agent in](authentication.md).

The deployment is the `URL` argument, else `BRIG_NOFIRE_URL`, else
`https://my.nofire.ai`. A deployment that comes from `BRIG_NOFIRE_URL`, and
is not the default, is confirmed with `Continue logging in to <URL>? [y/N]`
before Brig contacts it. Without a terminal, Brig refuses; pass the URL as
an argument instead.

These environment variables also change where the sign-in goes:

- `SSL_CERT_FILE` and `SSL_CERT_DIR` replace the certificates Brig trusts.
  Whoever sets them can add a CA and read the sign-in. When either is set,
  Brig prints
  `Trusting certificates from <value> (from SSL_CERT_FILE)` and asks the same
  question for every deployment, including the default. It asks once, even
  when `BRIG_NOFIRE_URL` also needs confirming. Without a terminal it refuses
  with
  `SSL_CERT_FILE is set, and brig login needs a terminal to confirm it. Unset SSL_CERT_FILE, or run brig login from a terminal`. Go reads
  these variables on Linux only, but Brig asks on macOS too.
- `HTTPS_PROXY` (or `https_proxy`), and `HTTP_PROXY` for an `http`
  deployment, send the requests through a proxy. Brig prints
  `Using the proxy <URL> (from HTTPS_PROXY)`, without the proxy's user name
  or password, and does not ask. TLS runs end to end through the proxy, so
  the proxy sees the host Brig connects to, not what it sends.
  `NO_PROXY` is honoured. `brig logout` prints the same notice.

| Flag | Meaning |
| --- | --- |
| `--browser` | signs in through a browser on this host |
| `--device` | prints a code to enter on another device |
| `--ttl DURATION` | asks for a token lifetime, in `h` or `d`, such as `12h` or `30d` |

With neither `--browser` nor `--device`, Brig opens a browser. On a headless
host, an SSH session or Linux with no display, it uses the device code
instead. The flags override that choice, and cannot be combined.

A deployment can grant a shorter lifetime than `--ttl` asks for. Brig prints
the lifetime it got and exits `0`.

The token is stored in the OS keychain under the service `sh.brig.login`.
Brig has no plaintext fallback: with no keychain, `brig login` fails before it
contacts the deployment. If Brig cannot store the token, or you press Ctrl-C
after the deployment issued it, Brig revokes the token and does not store
it. If that revocation fails, the token stays valid until it expires.

A second login to the same deployment replaces the stored token and revokes
the previous one. If that revocation fails, the login still succeeds, and
Brig prints a note to revoke the earlier token from the dashboard. See [The
NOFire AI login token](security.md#the-nofire-ai-login-token) for how the
token is protected.

### `brig logout`

```bash
brig logout [URL]
```

`brig logout` revokes the token on the deployment, then deletes it from the
keychain. It picks the deployment as `brig login` does, but it does not ask
about a deployment from `BRIG_NOFIRE_URL`: it sends a token only to the
deployment the token was stored for. If the deployment does not confirm the
revocation, Brig still deletes the token, exits `1`, and says to revoke it
from the dashboard.

`SSL_CERT_FILE` and `SSL_CERT_DIR` are confirmed as for `brig login`, with
`Continue logging out of <URL>? [y/N]`. Whoever sets them can answer the
revocation with their own CA and keep the token valid. If you answer no, or
there is no terminal, Brig sends nothing, keeps the token, and exits `1`.

## Environment variables

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_NOFIRE_URL` | `https://my.nofire.ai` | the NOFire AI deployment `brig login` and `brig logout` use when no URL argument is given. It affects only those two verbs, and a value other than the default is confirmed before `brig login` contacts it (global only) |
