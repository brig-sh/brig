# Brig Enterprise command line reference

Brig has two editions, built from the same source. Brig Community is the
Apache 2.0 edition that [Command line reference](cli.md) documents. Brig
Enterprise adds the commands on this page, which sign you in to NOFire AI and
send the recordings your organization asks for. Its source is in
[enterprise/](../enterprise/), under its own license.

`brig version` names the edition you have:

```console
$ brig version
brig v0.3.0 (91b0c7b, 2026-09-26, go1.26.0, darwin/arm64, enterprise)
```

All other verbs, flags, exit codes and environment variables are the same in
both editions. One thing differs for a run: when you are signed in and your
organization turned recording on, Brig Enterprise records it. [Session
recording](#session-recording) says when.

## Verbs

### `brig login`

```bash
brig login [URL] [--browser | --device] [--ttl DURATION]
```

`brig login` signs you in to a NOFire AI deployment and stores the token in
your keychain, one token for each deployment. Brig Enterprise reads it for two
things: `brig logout`, and [session recording](#session-recording). To log an
agent in, see [Logging an agent in](authentication.md).

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
  these variables on Linux only, but Brig asks on macOS too. If you answer
  yes, Brig keeps the name and value of each variable with the token, as an
  absolute path, and the question names that path.
  [Session recording](#session-recording) uses that, and `brig logout`
  removes it.
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

### `brig recordings`

```bash
brig recordings flush
```

`brig recordings flush` sends the recordings that are still waiting on this
machine and closes them. A recording waits when the network did not take it
before its run ended. It prints how many it sent and closed, how many wait for
a login or for a live run, and how many it could not send yet:

```console
$ brig recordings flush
recordings: 1 sent and closed, 0 waiting for a login or a live run, 0 not sent yet
```

When it applies, the line also counts the recordings it removed without
sending them, because the deployment said not to record them, refused them, or
their files were gone: `1 removed without being sent`.

Before it sends a recording, Brig asks the deployment whether the recording
belongs to the login stored now, with an empty request that sends no output.
If the login that recorded it is gone, for example after you signed out and
signed in to another organization on the same deployment, the deployment says
it has no such recording. Brig then sends nothing, opens no recording, and
leaves the files. The command counts these as `waiting for the login that
recorded them`. A recording that stays unsent for a reason a later
sign-in does not fix, because the deployment has no such recording for your
login, its details file cannot be used, or the deployment names a position
outside the recording, is removed after 30 days, and the
command counts it as `removed unsent after 30 days`. A recording that only has
no usable login now, because you are signed out, the login expired, a
certificate setting was not accepted or the keychain cannot be opened, is kept
however old it is, and `brig login` then lets a flush send it.

If the deployment refuses your login, the recordings stay on this machine and
Brig exits `1`. Run `brig login` again, then `brig recordings flush`. The
command also finishes a run that continued in a second recording. Telemetry
does not count it.

If `SSL_CERT_FILE` or `SSL_CERT_DIR` is set and was not accepted when you signed
in, the command refuses, names the variable and says that `brig login`
accepts it, because it sends your token. See [the rule for a trust
store](#session-recording).

## Session recording

Brig Enterprise records the terminal output of a run when all of these are
true:

- You signed in with `brig login` to the deployment, and the token has not
  expired. The deployment is `BRIG_NOFIRE_URL`, else `https://my.nofire.ai`,
  as for `brig login` with no argument. A token you stored for another
  deployment does not count.
- The runtime is hull. With nerdctl nothing is recorded, and `BRIG_RUNTIME`
  can select it. Only `brig run` and `brig sh` are recorded, and not under
  `--json`, with `--detach`, or for a GUI agent. `brig exec` hands over no <!-- retired-ok -->
  terminal, so it is not recorded.
- Your stdin and stdout are the same terminal. A run whose input or output is
  a pipe or a file, or whose stdin and stdout are two terminals, is not
  recorded. Brig compares the terminal device, so a run started with
  `</dev/tty`, which is a different device that names the same terminal, is not
  recorded.
- Neither `SSL_CERT_FILE` nor `SSL_CERT_DIR` is set, or each one that is set
  has the same value that you accepted when `brig login` asked about it. Brig
  stores what you accepted with the login, and `brig logout` removes it. A run
  cannot ask, so a variable that was not accepted, or whose value changed,
  means the run is not recorded. Brig makes a relative value absolute against
  the working directory, and compares the cleaned absolute path. A relative
  path from another directory is therefore not what you accepted. A value with
  a `..` element is never accepted, and `brig login` refuses to ask about it:
  set an absolute path. `SSL_CERT_FILE` is one path, and only `SSL_CERT_DIR` is
  a list, split at `:`. Brig does not resolve symlinks and does not read the
  file.
- Your organization turned recording on at the deployment. Brig asks the
  deployment once, with a 3 second timeout.

No setting exists to turn recording on or off. A deployment you did not sign
in to, such as one `BRIG_NOFIRE_URL` names, `--json`, `brig exec` and <!-- retired-ok -->
`BRIG_RUNTIME=nerdctl` exist for other reasons, and each leaves a run
unrecorded.

If any of these fails, or the deployment is down, the run goes on as it always
does, with no message and no delay beyond the timeout. Two cases print one
line to stderr at the start of a terminal run on hull. A trust-store variable
that was not accepted: `brig: could not check whether this run is recorded:
SSL_CERT_FILE is set and was not accepted when you signed in. Run `brig login`
to accept it`, which names the variable and never its value. A keychain that
does not answer within 3 seconds, such as a locked one waiting on an unlock
prompt: `brig: could not read your login within 3 seconds, so this run is not
recorded`. A run never needs the deployment to start. Brig Community records
nothing. `brig recordings flush` waits up to 30 seconds for the keychain,
because you asked for it and an unlock prompt can need a password.

At the start of a recorded run, Brig prints this line to stderr:

```text
brig: this run is recorded for your organization
```

In a recorded run Brig stays the parent of the command and relays the
terminal, instead of handing the terminal over. The command and its exit
status are unchanged, but Ctrl-Z does not stop the command. If your stderr is
not the same terminal as your stdout, such as a file, a pipe or another
terminal, the command's stderr stays that file, pipe or terminal, and it is not
recorded.

A run whose stdin or stdout is not a terminal, or whose stdin and stdout are two
different terminals, such as
`echo 'fix it' | brig run claude -- -p` or `brig run claude -- -p q | jq .`, is
not recorded. It runs exactly as it does without recording, and Brig asks the
deployment nothing. So is a run on a runtime other than hull.

Nothing more is printed while the run goes on. When it ends, Brig prints one
line for each thing that happened:

- `brig: this run's recording was interrupted and continues in a second
  recording`, with the number of the recording that follows
- ``brig: the end of this run's recording is still on this machine; the next
  recorded run or `brig recordings flush` will send it``
- `brig: recording stopped before the run ended (<reason>); output after
  <time> is not recorded`, also when the deployment refused the recording
  while the run went on
- `brig: this run is not recorded after all: Brig could not set up the recorder`,
  when the recorder could not start the command. The run then goes on as an
  unrecorded run
- `brig: this run's recording stopped and the rest of it was not sent`, when
  the deployment refused the recording as the run ended
- `brig: the deployment refused your login, so the rest of this run's recording
  was not sent; sign in again with `brig login`, then run `brig recordings
  flush``, in place of the line about the next recorded run
- `brig: recording stopped part way through this run because it reached the
  limit of 512 MiB; the rest of its output is not recorded`, or, for a write
  error on the spool, `because Brig could not write the recording to disk`
- `brig: sent the rest of <n> earlier recordings`
- `brig: removed <n> earlier recordings without sending them`, when the
  deployment said not to record them, refused them, or their files were gone
- `brig: removed <n> earlier recordings that were more than 30 days old and
  could not be sent`

Brig writes the output to a file under `recordings` in the state directory
(`~/.brig`, or `BRIG_STATE_DIR`) as the run goes, and uploads it to the
deployment in batches. While a recording is open and
quiet, Brig sends an empty batch every 45 seconds so the deployment does not
close it. The deployment closes a recording that hears nothing for 2
minutes. A recorder that was cut off for longer, because the machine slept or
the network was down, continues in a new recording, with the terminal's size
at that time. A run can use more than one recording.

What the network did not take stays in that directory. The next recorded
run sends it in the background, and so does `brig recordings flush`. When a
run ends, Brig waits up to 5 seconds for the deployment before it leaves
the rest on disk. Ctrl-C, or a SIGTERM or SIGHUP, that arrives after the
command has ended, also while Brig is still reading the last of its output,
ends the wait at once, leaves the rest on disk, and Brig exits with the
command's own status. A signal that arrived while the command ran went to the
command and does not end the wait.
[Session recording](security.md#session-recording) lists
what is captured, what leaves the machine, and the limits.

## Environment variables

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIG_NOFIRE_URL` | `https://my.nofire.ai` | the NOFire AI deployment `brig login` and `brig logout` use when no URL argument is given, and the one a run asks about recording. A value other than the default is confirmed before `brig login` contacts it. A run sends your token only to a deployment you signed in to (global only) |
