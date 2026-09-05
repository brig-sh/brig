# Manual test: `volumes:` and the credential in an ephemeral `.claude`

PR 6 covers the agent's config directory with a tmpfs so that nothing written
there can reach host disk, pins the paths a profile wants to keep across the
cover, and writes a `files:` binding into the tmpfs as an ordinary file with
the value on stdin. `go test ./internal/wrap/` drives all of that against a
recording double: it can assert the order of the mounts and that no value ever
reaches argv, but it cannot tell you what `stat -f -c %T` says in a real guest,
which is the only thing that makes the design fail-closed rather than intended.

CI cannot run this either: `ci.yml` is Linux-only and GitHub Actions is
disabled at the repository level, so nothing in this PR is machine-verified.

Run on a macOS host (arm64, macOS 26.4) with hull installed, 2026-08-18,
against `ghcr.io/brig-sh/claude-code-stock:latest` (Ubuntu, aarch64).

## The profile under test

The built-in `claude-code` spec does not declare `volumes:` yet -- that is
PR 8 -- so the run used a shadowing profile in a throwaway
`$XDG_CONFIG_HOME/brig/claude-code.yaml`: the built-in with `statePaths:`
removed and this appended.

```yaml
secrets:
  - name: volumes-probe-cred
    required: false

files:
  - ref: secrets.volumes-probe-cred
    path: .claude/.credentials.json
    mode: "0600"

volumes:
  - kind: hostmount
    path: .claude/sessions
  - kind: tmpfs               # deliberately BELOW a hostmount in the file
    path: .claude
  - kind: hostmount
    path: .claude/history.jsonl
    file: true
  - kind: hostmount
    path: .claude/projects
```

The secret is synthetic -- a document of the right shape with the string
`probe-not-a-real-token` in it. No real credential was imported, and the item
was deleted afterwards.

The workspace was seeded before the first boot so that "the kept paths
survived" is a claim about real bytes:

```
$ echo 'session-from-before' > $WS/.claude/sessions/old.jsonl
$ echo '{"before":1}'        > $WS/.claude/history.jsonl
```

## 1. The mounts, and their order

```
$ brig create claude-code
brig: image ghcr.io/brig-sh/claude-code-stock:latest: signature verified
brig: starting sandbox brig-claude-code...
VMM started (PID 55252)
brig-claude-code

$ brig exec claude-code -- sh -c \
    'grep -E "claude|persist" /proc/self/mountinfo | awk "{print \$5, \$(NF-2)}"'
/home/claude                                virtiofs
/run/brig/persist/.claude%2Fsessions        virtiofs
/run/brig/persist/.claude%2Fhistory.jsonl   virtiofs
/run/brig/persist/.claude%2Fprojects        virtiofs
/home/claude/.claude                        tmpfs
/home/claude/.claude/sessions               virtiofs
/home/claude/.claude/history.jsonl          virtiofs
/home/claude/.claude/projects               virtiofs
```

The pins are there and they are listed before the cover, which is the whole
trick: once tmpfs covers `.claude` the host copies underneath are unreachable,
so each hostmount is bound out to `/run/brig/persist/<escaped path>` first and
bound back in afterwards. The tmpfs is mounted second even though the profile
lists `.claude/sessions` above `.claude` -- mount order is derived from path
depth, never from the file.

## 2. `stat -f -c %T`: the fail-closed check

```
$ brig exec claude-code -- sh -c \
    'for p in ~/.claude ~/.claude/sessions ~/.claude/history.jsonl \
              ~/.claude/projects ~/.claude/.credentials.json; do
       printf "%-38s %s\n" "$p" "$(stat -f -c %T $p)"; done'
/home/claude/.claude                   tmpfs
/home/claude/.claude/sessions          fuseblk
/home/claude/.claude/history.jsonl     fuseblk
/home/claude/.claude/projects          fuseblk
/home/claude/.claude/.credentials.json tmpfs
```

`.claude` is tmpfs and the credential sits on it. Each kept path reads
`fuseblk` -- the host share -- which is the assertion that it really did
survive the cover rather than being a fresh empty directory inside the tmpfs.
brig makes both of these checks itself and fails the run on either, and the
same run checks that the guest has no swap:

```
$ brig exec claude-code -- cat /proc/swaps
Filename				Type		Size		Used		Priority
```

Header only. Nothing for a tmpfs page to be written out to.

## 3. The credential

```
$ brig exec claude-code -- stat -c "%n %F %U:%G %a %s" ~/.claude/.credentials.json
/home/claude/.claude/.credentials.json regular file claude:root 600 170
```

Owned by the guest user, mode 0600 from the binding, 170 bytes -- the stored
document, whole. Group stays `root` because the cover chowns the owner only;
mode 0600 makes the group irrelevant.

The kept state is there too, read back from the host copies through the cover:

```
$ brig exec claude-code -- cat .claude/sessions/old.jsonl .claude/history.jsonl
session-from-before
{"before":1}
```

## 4. The host stays clean

```
$ find $WS/.claude
WS/.claude
WS/.claude/projects
WS/.claude/sessions
WS/.claude/history.jsonl
WS/.claude/sessions/old.jsonl
```

No `.credentials.json`, and no `.credentials.json.tmp.*`. And the value is not
in hull's durable argv log either -- it goes in on stdin through
`runtime.Feed`:

```
$ grep -rl "probe-not-a-real-token" ~/.hull/store/instances/brig-claude-code/
(nothing)
```

## 5. The atomic write that killed the original design now works

This is gate 1's failure reproduced against the new design. Claude Code writes
its credential with a temp file in the same directory and a `renameat` onto the
real path; against the bind mount D4 originally specified that `renameat`
returned `EBUSY`, and the temp file landed in the workspace. Simulated exactly:

```
$ brig exec claude-code -- sh -c '
    C=~/.claude/.credentials.json
    echo "inode before: $(stat -c %i $C)"
    T=$C.tmp.$$
    (umask 077; cat $C > $T)
    mv $T $C && echo "renameat: ok"
    echo "inode after:  $(stat -c %i $C)"
    stat -c "%F %U %a %s" $C'
inode before: 6
renameat: ok
inode after:  7
regular file claude 600 108
```

The rename succeeds -- it is a rename within a tmpfs onto a path that is not
itself a mountpoint -- and the inode changes, which is what an atomic write
looks like. The host workspace after it still holds no credential and no temp
file (section 4's `find` was re-run and is unchanged).

## 6. A second run neither stacks mounts nor empties the credential

`EnsureRunning` delivers on an already-running sandbox too, so this is the path
that runs every time after the first.

```
$ brig exec claude-code -- sh -c 'grep -cE "claude|persist" /proc/self/mountinfo'
8
$ brig create claude-code
brig-claude-code
$ brig exec claude-code -- sh -c '
    echo "mount lines: $(grep -cE "claude|persist" /proc/self/mountinfo)"
    stat -c "%n %U %a %s" ~/.claude/.credentials.json
    stat -f -c %T ~/.claude
    cat ~/.claude/sessions/old.jsonl'
mount lines: 8
/home/claude/.claude/.credentials.json claude 600 170
tmpfs
session-from-before
```

Same eight mounts, same credential, same kept state.

Rotation reaches a live session, which is the thing an environment variable
cannot do:

```
$ printf '%s' '{"claudeAiOauth":{"accessToken":"probe-rotated",...}}' \
    | brig secret update volumes-probe-cred -f -
$ brig create claude-code
$ brig exec claude-code -- sh -c \
    'wc -c < ~/.claude/.credentials.json; grep -o probe-rotated ~/.claude/.credentials.json'
108
probe-rotated
```

## 7. A planted symlink at a target fails the run

Every one of these paths is inside the workspace, which the sandbox has had
write access to. A root-run `mount --bind` that followed a planted link would
be an arbitrary-write primitive reaching the host bundle, so brig creates the
targets on the host through the workspace root and refuses rather than repairs.

```
$ brig rm claude-code
$ ln -s /etc $WS/.claude/sessions
$ brig create claude-code
brig: refusing to mount .../ws/.claude/sessions: it is a symlink to "/etc", and
brig writes only regular files inside the workspace. The workspace is mounted
read-write as the sandbox's home, so that link was put there from inside the
sandbox, to have brig -- which runs as you, on the host -- reach a file the
sandbox cannot. Nothing was written; inspect .../ws/.claude/sessions and remove
it before running brig again: a symlink in the workspace leads out of it
$ echo $?
1
```

The same with the link at `.claude` itself -- the tmpfs mount point -- gives the
same refusal and the same exit 1. Both fail **before** the sandbox is created,
so nothing is mounted and nothing is written.

## What this does not cover

- **nerdctl.** A container runtime has no privileged exec to mount with, so its
  tmpfs and hostmounts are handed to it in the create request instead
  (`createTimeVolumes`). There is no nerdctl on this host, so that path is
  asserted only by `TestCreateTimeVolumesAreForTheRuntimesThatCannotMount` and
  has never been run against a real container.
- **Gate 2, refresh-token rotation.** Still open. This test used a synthetic
  credential against no provider, so it says nothing about whether a second
  boot from the same stored refresh token authenticates. PR 8 is gated on it.
- **The cost of the design.** Anything a future Claude Code version starts
  writing under `.claude/` is ephemeral until someone adds a `hostmount` for
  it. That is the trade -- the old design lost nothing by default and leaked by
  default; this one leaks nothing by default and loses by default -- and it is
  not something a test can catch.
