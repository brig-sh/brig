# Manual test: stdin delivery and privileged-mount visibility

PR 4 (`internal/runtime`) adds `Runtime.Feed`, which carries a value into the
guest on stdin instead of argv, and `RunSpec.Tmpfs` / `ExecSpec.User`, which
let a container runtime create a tmpfs at boot and hull mount one with a
privileged exec. `go test ./internal/runtime/` only asserts on the argv a spec
builds; it cannot see a guest. This is the guest-side check the plan asks for,
run by hand on a macOS host with hull already installed.

CI cannot run this: `ci.yml` is Linux-only and GitHub Actions is disabled at
the repository level, so none of it is machine-verified.

## What was run

```
$ brig create claude-code
...
brig: starting sandbox brig-claude-code...
VMM started (PID 34585)
brig-claude-code

$ hull ps
ID                STATUS   EXIT  PID    IP         CREATED
brig-claude-code  running  -     34585  10.87.0.2  2026-08-17 18:34:19
```

The sandbox came up on the first attempt; no image-pull or boot problems, so
the "if it will not boot" fallback in the task instructions did not apply.

### Attempt 1 — the commands exactly as the plan step 4 gives them

```
$ hull exec -u root brig-claude-code -- sh -c \
    'mkdir -p /run/probe && mount -t tmpfs -o size=1m,mode=0700 tmpfs /run/probe'
(exit 0)

$ printf 'value-on-stdin' | hull exec brig-claude-code -- sh -c 'cat > /run/probe/f'
sh: 1: cannot create /run/probe/f: Permission denied
(exit 2)

$ hull exec brig-claude-code -- sh -c 'stat -f -c %T /run/probe; cat /run/probe/f; echo'
tmpfs
cat: /run/probe/f: Permission denied
```

**Finding: the plan's step-4 commands do not work verbatim.** `mode=0700` on
the mount makes the tmpfs root directory `rwx------`, owned by root (the user
that ran the privileged exec). The unprivileged guest user (`claude`, uid
501 -- confirmed with `hull exec brig-claude-code -- sh -c 'id'`) then has no
`x` on that directory and cannot create or read a file under it. This is an
ordinary Unix permission result, not a mount-namespace problem, but it means
the literal transcript in the plan would not have produced the two facts it
asks for. The unprivileged `stat` call *did* succeed and correctly reported
`tmpfs` -- `stat` on the directory itself only needs traversal of its parent
(`/run`), not of `/run/probe`, so that much of the plan's transcript does hold
and is evidence on its own for the visibility question below.

### Attempt 2 — same two facts, with permissions that let the unprivileged user through

```
$ hull exec -u root brig-claude-code -- sh -c \
    'rm -rf /run/probe2 && mkdir -p /run/probe2 &&
     mount -t tmpfs -o size=1m,mode=0755 tmpfs /run/probe2 &&
     chown claude:claude /run/probe2'
(exit 0)

$ printf 'value-on-stdin' | hull exec brig-claude-code -- sh -c 'cat > /run/probe2/f'
(exit 0)

$ hull exec brig-claude-code -- sh -c 'stat -f -c %T /run/probe2; cat /run/probe2/f; echo'
tmpfs
value-on-stdin
```

Both defaulted (no `-u`) execs above ran as the guest's own unprivileged user.

## The two facts the plan asks for

1. **The value round-trips on stdin.** `printf 'value-on-stdin' | hull exec ...`
   delivered the string into the guest with nothing on the command line, and
   a later exec read it back byte-for-byte. Confirms `Feed`'s approach: the
   value never has to appear in argv to reach a file inside the guest.
2. **A mount made by a privileged exec is visible to a later unprivileged
   one.** In both attempts, `stat -f -c %T` from an *unprivileged* exec
   reported `tmpfs` for a mount created moments earlier by a `-u root` exec.
   hull's execs share one mount namespace, which is exactly what PR 6's
   design rests on.

## What is still outstanding

Everything above ran ad hoc against `hull exec`, not against
`internal/runtime`'s `Feed`, `Tmpfs`, or `User` fields wired end-to-end --
that plumbing has no caller yet (PR 6 is the one that calls `Feed` and sets
`Tmpfs`/`User` for real). This document establishes the two facts the guest
itself needed to answer; it does not exercise this PR's Go code against a
guest, because nothing in the current tree invokes `Feed` outside of
`hull_test.go`'s argv-only unit test. That end-to-end exercise is for PR 6,
once it has code that actually calls `Feed`.

The sandbox was removed after this test (`brig rm claude-code`), so nothing
from this session is left running.
