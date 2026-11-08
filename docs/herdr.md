# A herd of agents

The `herdr` profile runs Claude Code with a [herdr](https://herdr.dev) server
in each sandbox. One herdr client on your host adds each sandbox as a remote
machine, so you watch and drive many agents from one place, one agent per
sandbox.

herdr reaches a remote machine only over SSH, and a sandbox has no open port.
The `herdr-stock` image carries `sshd` and a wrapper, `herdr-sshd`. Your ssh
runs that wrapper through `brig exec` as its `ProxyCommand`, and the wrapper
runs `sshd` on the exec's input and output. Nothing listens in the guest.

The image is `ghcr.io/brig-sh/herdr-stock`, built from
[images/herdr](https://github.com/brig-sh/community-images/tree/main/images/herdr)
in community-images.

## Set it up once

Add this to `~/.ssh/config`, above any `Host *` block that sets `User`:

```sshconfig
Host herdr*+*
  User root
  ForwardAgent no
  StrictHostKeyChecking yes
  UserKnownHostsFile /dev/null
  KnownHostsCommand /bin/sh -c 'r=$(echo "$1" | sed "s/+/@/") && k=$(brig -q exec "$r" -- cat /etc/ssh/ssh_host_ed25519_key.pub 2>/dev/null) && [ -n "$k" ] && echo "$1 $k"' sh %n
  LogLevel ERROR
  ProxyCommand sh -c 'exec brig -q exec "${1%%+*}@${1#*+}" -- /usr/local/sbin/herdr-sshd 2>/dev/null' sh %n
```

A host is `<profile>+<sandbox>`: `herdr+api` is the sandbox `herdr@api`, and
`herdr-review+api` is `herdr-review@api`. Both commands split at the first `+`,
and a profile name cannot contain one, so the profile is always what is left
of it. The block matches any profile whose name starts with `herdr`. A
profile made with `brig agent new <name> --from herdr` needs no block of its
own if you name it `herdr-<something>`.

Until v0.5.1 the profile was `herd` and the wrapper `herd-sshd`. A block
written then matches the new names, but its `ProxyCommand` runs a wrapper
the `herdr-stock` image does not have: replace the block with the one above.
[migration.md](migration.md#profile-names) lists the rest.

- `brig exec`, not `brig sh`. `brig sh` always asks for a terminal in the
  guest, and a terminal in the middle of the SSH stream hangs it.
- The host key is read through `brig exec`, not from a known_hosts file. The
  image ships no host keys and the guest root does not survive `brig stop`, so
  a sandbox has new keys after every boot. herdr checks a machine with strict
  host key checking, which a known_hosts file could only pass until the next
  boot. An empty key fails the check rather than passing a malformed line.
  `KnownHostsCommand` cannot use `${...}`, which ssh expands itself, so it
  splits the host name with `sed`.
- No key. The image accepts root with an empty password. Whoever can run
  `brig exec` on your host is already root in the guest, and the guest has
  no open port, so a key would guard nothing that `brig exec` does not.
- `ForwardAgent no`. A forwarded agent would give the guest a signer for
  every key you hold.

For Claude Code to keep its login on the host, attach the same policy as for
`claude-code`. See [broker.md](broker.md#use-it):

```bash
brig policy attach claude-routed herdr
```

## Add a sandbox

```bash
brig run -d herdr@api ~/code/api
herdr machine add herdr+api --label api
```

`brig run -d` boots the sandbox and starts no agent: Claude Code runs only in
the herdr panes, one per `claude` you start there. `herdr machine add` prints
`Remote server is ready`. Open a pane on `api` in herdr and run
`cd /work/api && claude`: a pane starts in `/root`, not in the project.

Each SSH connection is a new `brig exec`, so it carries a new session token.
`herdr-sshd` writes what the exec received to `/run/herdr/env`, and
`/etc/bash.bashrc` reads it in an SSH session, because `sshd` starts every
session with an empty environment. A `brig sh` does not read it, and keeps
the token Brig gave it. A pane keeps the values it started with. A pane that
does not start `bash` gets none of them. See
[What is still exposed](security.md#what-is-still-exposed).

## Remove a sandbox

Remove the herdr machine first, then restart the herdr client, then remove the
sandbox:

```bash
herdr machine remove <id>
brig rm herdr@api
```

`herdr machine remove` takes the id that `herdr machine list` prints. A running
herdr client keeps a machine it has connected to until it restarts, and it
reconnects. The reconnect is a `brig exec`, which starts the sandbox again
after `brig rm`.
