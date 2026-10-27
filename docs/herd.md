# A herd of agents

The `herd` profile runs Claude Code with a [herdr](https://herdr.dev) server
in each sandbox. One herdr client on your host adds each sandbox as a remote
machine, so you watch and drive many agents from one place, one agent per
sandbox.

herdr reaches a remote machine only over SSH, and a sandbox has no open port.
The `herd` image carries `sshd` and a wrapper, `herd-sshd`. Your ssh runs that
wrapper through `brig exec` as its `ProxyCommand`, and the wrapper runs `sshd`
on the exec's input and output. Nothing listens in the guest.

The image is `ghcr.io/brig-sh/herd-stock`, built from
[images/herd](https://github.com/brig-sh/community-images/tree/main/images/herd)
in community-images.

## Set it up once

Add this to `~/.ssh/config`, above any `Host *` block that sets `User`:

```sshconfig
Host herd*+*
  User root
  ForwardAgent no
  StrictHostKeyChecking yes
  UserKnownHostsFile /dev/null
  KnownHostsCommand /bin/sh -c 'r=$(echo "$1" | sed "s/+/@/") && k=$(brig -q exec "$r" -- cat /etc/ssh/ssh_host_ed25519_key.pub 2>/dev/null) && echo "$1 $k"' sh %n
  LogLevel ERROR
  ProxyCommand sh -c 'exec brig -q exec "${1%%+*}@${1#*+}" -- /usr/local/sbin/herd-sshd 2>/dev/null' sh %n
```

A host is `<profile>+<sandbox>`: `herd+api` is the sandbox `herd@api`, and
`herd-review+api` is `herd-review@api`. Brig names cannot contain `+`, so the
split is never ambiguous. The block matches any profile whose name starts with
`herd`, so a profile made with `brig agent new <name> --from herd` needs no
block of its own if you name it `herd-<something>`.

- `brig exec`, not `brig sh`. `brig sh` always asks for a terminal in the
  guest, and a terminal in the middle of the SSH stream hangs it.
- The host key is read through `brig exec`, not from a known_hosts file. The
  image ships no host keys and the guest root does not survive `brig stop`, so
  a sandbox has new keys after every boot. herdr checks a machine with strict
  host key checking, which a known_hosts file could only pass until the next
  boot. `KnownHostsCommand` cannot use `${...}`, which ssh expands itself, so
  it splits the host name with `sed`.
- No key. The image accepts root with an empty password. Whoever can run
  `brig exec` on your host is already root in the guest, and the guest has
  no open port, so a key would guard nothing that `brig exec` does not.
- `ForwardAgent no`. A forwarded agent would give the guest a signer for
  every key you hold.

For Claude Code to keep its login on the host, attach the same policy as for
`claude-code`. See [broker.md](broker.md#use-it):

```bash
brig policy attach claude-routed herd
```

## Add a sandbox

```bash
brig run -d herd@api ~/code/api
herdr machine add herd+api --label api
```

`herdr machine add` prints `Remote server is ready`. Open a pane on `api` in
herdr and run `cd /work/api && claude`: a pane starts in `/root`, not in the
project.

Each SSH connection is a new `brig exec`, so it carries a new session token.
`herd-sshd` writes what the exec received to `/run/herd/env`, and
`/etc/bash.bashrc` reads it, because `sshd` starts every session with an
empty environment. A pane keeps the values it started with. A pane that does
not start `bash` gets none of them. See
[What is still exposed](security.md#what-is-still-exposed).

## Remove a sandbox

Remove the herdr machine first, then restart the herdr client, then remove the
sandbox:

```bash
herdr machine remove <id>
brig rm herd@api
```

`herdr machine remove` takes the id that `herdr machine list` prints. A running
herdr client keeps a machine it has connected to until it restarts, and it
reconnects. The reconnect is a `brig exec`, which starts the sandbox again
after `brig rm`.
